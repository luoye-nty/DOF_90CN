package dnfbridge

import (
	"context"
	"encoding/binary"
	"math"
	"strconv"
	"testing"
	"time"

	"longheng.io/server/internal/modules/dnf/premium"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

// The current client replays the dword it was shown at +0x06 of the item row,
// so the private-store expectation has to be whatever the list projection puts
// there. A legacy 43-byte row carries a different field at that offset: the old
// RawEntry[6:10] read answered 0 for it while the client returned the projected
// count, and every stall containing such an item failed op87 with error 4.
func TestPrivateStoreEquipmentInstanceMatchesProjectedRow(t *testing.T) {
	seeded := dnfrepo.ItemStack{
		ItemID: 5001,
		Count:  1,
		Extra:  map[string]string{"item_kind": "equipment"},
		RawEntry: func() []byte {
			raw := make([]byte, currentItemListEntryWireSize)
			binary.LittleEndian.PutUint32(raw[2:6], 5001)
			binary.LittleEndian.PutUint32(raw[6:10], 0x0A48BC23)
			return raw
		}(),
	}
	// The live 2026-09-11 row: equipment imported with the 43-byte legacy
	// projection and no quality seed at all.
	legacy := dnfrepo.ItemStack{
		ItemID:   101010912,
		Count:    1,
		Extra:    map[string]string{"item_kind": "equipment"},
		RawEntry: []byte{0x0b, 0xe0, 0x4d, 0x05, 0x06, 0x01, 0x00, 0x00, 0x00, 0x00, 0x2c, 0x00, 0x00},
	}
	bare := dnfrepo.ItemStack{ItemID: 101010912, Count: 1, Extra: map[string]string{"item_kind": "equipment"}}

	for name, stack := range map[string]dnfrepo.ItemStack{"seeded": seeded, "legacy": legacy, "bare": bare} {
		entry := currentItemListEntryFromStack(dnfrepo.MainInventoryListType, 26, stack)
		projected := binary.LittleEndian.Uint32(entry.data[0x06:0x0A])
		if got := currentPrivateStoreEquipmentInstance(dnfrepo.MainInventoryListType, stack); got != projected {
			t.Fatalf("%s: private-store instance %d disagrees with projected row dword %d", name, got, projected)
		}
		if projected == 0 {
			t.Fatalf("%s: projected instance dword is 0; the row would be unsellable", name)
		}
	}
	if projectedLegacy := currentPrivateStoreEquipmentInstance(dnfrepo.MainInventoryListType, legacy); projectedLegacy != 1 {
		t.Fatalf("legacy row instance = %d, want the projected count 1", projectedLegacy)
	}
	if seededInstance := currentPrivateStoreEquipmentInstance(dnfrepo.MainInventoryListType, seeded); seededInstance != 0x0A48BC23 {
		t.Fatalf("seeded row instance = %#x, want the quality seed 0x0A48BC23", seededInstance)
	}
}

// A crystal listed straight out of the account warehouse: the client names that
// container list 36 while keeping the merged list-0 slot, and the row has to
// resolve against the account record rather than the character inventory.
func TestPrivateStoreCompleteAcceptsAccountWarehouseRow(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	const accountID = "account-crystal-store"
	if err := repositories.Account.Save(ctx, dnfrepo.AccountRecord{
		AccountID: accountID,
		Metadata: map[string]string{
			premium.MetadataKey(currentPrivateStorePremiumType): strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10),
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.AccountInventory.Save(ctx, dnfrepo.AccountInventoryRecord{
		AccountID: accountID,
		Slots: map[string]dnfrepo.ItemStack{
			"0:358": {ItemID: 3037, Count: 1066, Extra: map[string]string{"item_kind": "stackable"}},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{CharacterID: "10", Slots: map[string]dnfrepo.ItemStack{}}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{CharacterID: "10", AccountID: accountID, Name: "Seller"}); err != nil {
		t.Fatal(err)
	}
	connection := &bufferConn{}
	session := &gameSession{conn: connection, selectedCharacterID: 10, accountID: accountID}
	service := &Service{
		options:                     options{gameUpperHeader: gameUpperHeaderChannel13, gameUpperBodyCodec: gameUpperBodyCodecPlain},
		repositoryProvider:          func() (dnfrepo.Group, bool) { return repositories, true },
		privateStoreVisitors:        make(map[*gameSession]uint16),
		privateStoreItemEligibility: func(itemID uint32) bool { return itemID == 3037 },
		onlinePlayers:               newOnlinePlayerManager(),
		privateStores: map[uint16]*currentPrivateStore{
			10: {OwnerID: 10, OwnerAccountID: accountID, OwnerSession: session},
		},
	}

	// Exact live row shape: grid cell 2, price 100, list 36, slot 358, 200 units.
	var complete packetWriter
	complete.writeUint32(3)
	complete.writeBytes([]byte("123"))
	complete.writeByte(1)
	complete.writeUint16(2)
	complete.writeUint32(100)
	complete.writeByte(currentPrivateStoreAccountSharedListType)
	complete.writeUint16(358)
	complete.writeUint32(200)
	complete.writeUint16(math.MaxUint16)
	if err := service.completeCurrentPrivateStore(session, complete.bytes()); err != nil {
		t.Fatal(err)
	}
	if connection.write.Len() == 0 {
		t.Fatal("op87 completion produced no response at all")
	}
	stored := service.privateStores[10]
	if stored == nil || !stored.Active || len(stored.Rows) != 1 {
		t.Fatalf("store = %+v", stored)
	}
	row := stored.Rows[0]
	if row.ListType != dnfrepo.MainInventoryListType || row.Slot != 358 || row.ItemID != 3037 || row.Count != 200 || row.Price != 100 {
		t.Fatalf("frozen row = %+v", row)
	}
}

// Both private-store purchase notices carry the quantity that was just sold,
// because the client subtracts that dword from the cell it already tracks
// (store window vtable+0x7C). Sending the remainder there made the seller's own
// stall drift to 1 after the first sale and then close itself on the next one.
func TestPrivateStorePurchaseNoticesCarrySoldQuantity(t *testing.T) {
	const displayIndex, soldQuantity = uint16(11), uint32(192)

	count := buildCurrentPrivateStoreCountNotification(displayIndex, soldQuantity)
	if len(count) != 6 {
		t.Fatalf("count notification length = %d, want 6", len(count))
	}
	if binary.LittleEndian.Uint16(count[0:2]) != displayIndex ||
		binary.LittleEndian.Uint32(count[2:6]) != soldQuantity {
		t.Fatalf("count notification = %x", count)
	}

	notice := buildCurrentPrivateStorePurchaseNotification(3037, displayIndex, soldQuantity, "奶爸", 100, soldQuantity, 971834073)
	// u8 mode, u32 seller gold, u32 item, u16 cell, u32 sold, dstr name, ...
	if binary.LittleEndian.Uint16(notice[9:11]) != displayIndex ||
		binary.LittleEndian.Uint32(notice[11:15]) != soldQuantity {
		t.Fatalf("seller notification = %x", notice)
	}
}
