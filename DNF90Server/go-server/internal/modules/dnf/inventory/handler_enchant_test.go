package inventory

import (
	"context"
	"encoding/binary"
	"strconv"
	"testing"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

func handleEnchantRequest(t *testing.T, repos dnfrepo.Group, resolver alignedcmd.EnchantBeadResolver, body []byte) alignedcmd.Result {
	t.Helper()
	got, err := NewHandler().Handle(context.Background(), alignedcmd.Request{
		Opcode:              uint16(dnfenum.CmdPacketEnchantByBead),
		Body:                body,
		AccountID:           "acc",
		SelectedCharacterID: 77,
		Repositories:        repos,
		EnchantBeadResolver: resolver,
	})
	if err != nil {
		t.Fatalf("Handle error = %v", err)
	}
	return got
}

func enchantRequestBody(beadList byte, beadSlot int16, targetList byte, targetSlot int16) []byte {
	return []byte{
		beadList,
		byte(beadSlot), byte(beadSlot >> 8),
		targetList,
		byte(targetSlot), byte(targetSlot >> 8),
	}
}

func TestHandlerEnchantByBeadSendsRefreshThenAck(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	saveEnchantFixture(t, ctx, repos, 2, 0, 0)

	got := handleEnchantRequest(t, repos, coatEnchantResolver(9001, nil), enchantRequestBody(listTypeMain, 12, listTypeMain, 30))
	if !got.Handled || !got.ResponseAllowed {
		t.Fatalf("result = %+v", got)
	}
	if len(got.UpperResponses) != 2 {
		t.Fatalf("responses = %d, want refresh then ack", len(got.UpperResponses))
	}
	refresh := got.UpperResponses[0]
	if refresh.MsgID != msgItemListRefresh || refresh.Classification != 0 {
		t.Fatalf("first response = msg 0x%04X class %d, want class0 0x000D refresh", refresh.MsgID, refresh.Classification)
	}
	ack := got.UpperResponses[1]
	if ack.MsgID != uint16(dnfenum.CmdPacketEnchantByBead) {
		t.Fatalf("ack msg = 0x%04X, want 0x0110", ack.MsgID)
	}
	wantAck := []byte{1, listTypeMain, 30, 0}
	if string(ack.Body) != string(wantAck) {
		t.Fatalf("ack body = %x, want %x", ack.Body, wantAck)
	}
	if len(got.PostActions) != 0 {
		t.Fatalf("post actions = %v, want no duplicate refresh after ACK", got.PostActions)
	}
}

func TestHandlerEnchantPetCreatureSendsTargetAndBeadUpdatesBeforeAck(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	savePetEnchantFixture(t, ctx, repos, 1)

	got := handleEnchantRequest(t, repos, creatureEnchantResolver(10008663, 400990168), enchantRequestBody(listTypeMain, 12, listTypePet, 24))
	if !got.Handled || !got.ResponseAllowed || len(got.UpperResponses) != 3 {
		t.Fatalf("result = %+v", got)
	}
	petUpdate := got.UpperResponses[0]
	if petUpdate.Classification != 0 || petUpdate.MsgID != msgItemListUpdate || len(petUpdate.Body) != 3+currentItemListEntrySize || petUpdate.Body[0] != listTypePet {
		t.Fatalf("pet update = class%d msg0x%04X body=%x", petUpdate.Classification, petUpdate.MsgID, petUpdate.Body)
	}
	petRow := petUpdate.Body[3:]
	if gotCard := uint32(petRow[0x0E]) | uint32(petRow[0x0F])<<8 | uint32(petRow[0x10])<<16 | uint32(petRow[0x11])<<24; gotCard != 10008663 {
		t.Fatalf("pet update card=%d want=10008663", gotCard)
	}
	beadUpdate := got.UpperResponses[1]
	if beadUpdate.Classification != 0 || beadUpdate.MsgID != msgItemListUpdate || len(beadUpdate.Body) != 3+currentItemListEntrySize || beadUpdate.Body[0] != listTypeMain {
		t.Fatalf("bead update = class%d msg0x%04X body=%x", beadUpdate.Classification, beadUpdate.MsgID, beadUpdate.Body)
	}
	beadRow := beadUpdate.Body[3:]
	if beadRow[0x02] != 0xFF || beadRow[0x03] != 0xFF || beadRow[0x04] != 0xFF || beadRow[0x05] != 0xFF {
		t.Fatalf("consumed bead row did not carry empty item marker: %x", beadRow[:10])
	}
	ack := got.UpperResponses[2]
	if ack.MsgID != uint16(dnfenum.CmdPacketEnchantByBead) || string(ack.Body) != string([]byte{1, listTypePet, 24, 0}) {
		t.Fatalf("ack = msg0x%04X body=%x", ack.MsgID, ack.Body)
	}
}

func TestHandlerEnchantEquippedPetCreatureSendsBeadUpdateAckAndEquipmentRefresh(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	const petItemID int64 = 400990167
	const petSerial uint32 = 41
	beadRaw := make([]byte, currentItemListEntrySize)
	if err := repos.Inventory.Save(ctx, dnfrepo.InventoryRecord{CharacterID: "77", Slots: map[string]dnfrepo.ItemStack{
		slotKey(listTypeMain, 12): {ItemID: 490007163, Count: 1, RawEntry: beadRaw, Extra: map[string]string{"item_kind": "stackable"}},
	}}); err != nil {
		t.Fatal(err)
	}
	raw := make([]byte, 43)
	raw[0] = 26
	binary.LittleEndian.PutUint32(raw[1:5], uint32(petItemID))
	binary.LittleEndian.PutUint32(raw[24:28], petSerial)
	if err := repos.Equipment.Save(ctx, dnfrepo.EquipmentRecord{CharacterID: "77", Entries: map[string]dnfrepo.EquipmentEntry{"26": {SlotIndex: 26, ItemID: petItemID, RawEntry: raw, Extra: map[string]string{"item_kind": "equipment", "equipment_type": "creature"}}}}); err != nil {
		t.Fatal(err)
	}
	if err := repos.Pet.Save(ctx, dnfrepo.PetRecord{CharacterID: "77", EquippedKey: "41", Entries: map[string]dnfrepo.PetEntry{"41": {PetKey: "41", CreatureKey: petSerial, ItemID: petItemID, SourceListType: listTypeEquipment, SourceSlotIndex: 26}}}); err != nil {
		t.Fatal(err)
	}
	got := handleEnchantRequest(t, repos, creatureEnchantResolver(10008713, petItemID), enchantRequestBody(listTypeMain, 12, listTypeEquipment, 26))
	if len(got.UpperResponses) != 2 || len(got.PostActions) != 1 || got.PostActions[0] != alignedcmd.PostActionRefreshSelectedEquippedCreatureItem {
		t.Fatalf("result = %+v", got)
	}
	if got.UpperResponses[0].MsgID != msgItemListUpdate || got.UpperResponses[0].Body[0] != listTypeMain {
		t.Fatalf("bead update = %+v", got.UpperResponses[0])
	}
	if got.UpperResponses[1].MsgID != uint16(dnfenum.CmdPacketEnchantByBead) || string(got.UpperResponses[1].Body) != string([]byte{1, listTypeEquipment, 26, 0}) {
		t.Fatalf("ack = %+v", got.UpperResponses[1])
	}
}

func TestHandlerEnchantByBeadErrorAckOnly(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	saveEnchantFixture(t, ctx, repos, 2, 0, 0)

	got := handleEnchantRequest(t, repos, coatEnchantResolver(0, nil), enchantRequestBody(listTypeMain, 12, listTypeMain, 30))
	if !got.Handled || !got.ResponseAllowed {
		t.Fatalf("result = %+v", got)
	}
	if len(got.UpperResponses) != 1 {
		t.Fatalf("responses = %d, want single error ack", len(got.UpperResponses))
	}
	ack := got.UpperResponses[0]
	if ack.MsgID != uint16(dnfenum.CmdPacketEnchantByBead) {
		t.Fatalf("ack msg = 0x%04X, want 0x0110", ack.MsgID)
	}
	if string(ack.Body) != string([]byte{0, enchantErrorInvalidBead}) {
		t.Fatalf("ack body = %x, want {0,0x11}", ack.Body)
	}
}

func TestHandlerEnchantByBeadParseFailureAcksInvalidBead(t *testing.T) {
	repos := dnfrepomemory.NewMemoryGroup()
	got := handleEnchantRequest(t, repos, coatEnchantResolver(9001, nil), []byte{1, 2})
	if !got.Handled || !got.ResponseAllowed || len(got.UpperResponses) != 1 {
		t.Fatalf("result = %+v", got)
	}
	if string(got.UpperResponses[0].Body) != string([]byte{0, enchantErrorInvalidBead}) {
		t.Fatalf("ack body = %x, want {0,0x11}", got.UpperResponses[0].Body)
	}
}

func TestHandlerEnchantByBeadNilResolverFailsClosed(t *testing.T) {
	repos := dnfrepomemory.NewMemoryGroup()
	got := handleEnchantRequest(t, repos, nil, enchantRequestBody(listTypeMain, 12, listTypeMain, 30))
	if !got.Handled {
		t.Fatalf("result = %+v", got)
	}
	if got.ResponseAllowed || len(got.UpperResponses) != 0 {
		t.Fatalf("nil resolver must fail closed without any response: %+v", got)
	}
}

// currentEquipmentQualityRow builds one complete current 0x77 equipment row:
// the PVF item id at +2 and the durable quality seed at +6, which is the word
// the client reads the displayed grade (鍝佺骇) from.
func currentEquipmentQualityRow(slot int16, itemID int64, seed uint32) []byte {
	raw := make([]byte, currentItemListEntrySize)
	binary.LittleEndian.PutUint16(raw[0:2], uint16(slot))
	binary.LittleEndian.PutUint32(raw[2:6], uint32(itemID))
	binary.LittleEndian.PutUint32(raw[6:10], seed)
	return raw
}

// currentEquipmentQualityStack models a durable equipment row. carryExtraSeed
// selects the two shapes that exist in the database: a row created by the drop
// and reward paths (quality_seed mirrored into Extra) and an imported row whose
// grade lives only in its stored 0x77 row.
func currentEquipmentQualityStack(slot int16, itemID int64, seed uint32, carryExtraSeed bool) dnfrepo.ItemStack {
	extra := map[string]string{
		"item_kind":      "equipment",
		"pvf_path":       "equipment/character/common/coat/test_coat.equ",
		"equipment_type": "[coat]",
	}
	if carryExtraSeed {
		extra["quality_seed"] = strconv.FormatUint(uint64(seed), 10)
	}
	return dnfrepo.ItemStack{
		ItemID:   itemID,
		Count:    1,
		RawEntry: currentEquipmentQualityRow(slot, itemID, seed),
		Extra:    extra,
	}
}

// mainListRefreshRowForSlot returns the single 0x77 row a class0 0x000D refresh
// body carries for one slot.
func mainListRefreshRowForSlot(t *testing.T, body []byte, slot int16) []byte {
	t.Helper()
	if len(body) < 5 || body[0] != listTypeMain {
		t.Fatalf("refresh body = % X", body)
	}
	count := int(binary.LittleEndian.Uint16(body[3:5]))
	if len(body) != 5+count*currentItemListEntrySize {
		t.Fatalf("refresh body len=%d rows=%d", len(body), count)
	}
	for index := 0; index < count; index++ {
		row := body[5+index*currentItemListEntrySize : 5+(index+1)*currentItemListEntrySize]
		if int16(binary.LittleEndian.Uint16(row[0:2])) == slot {
			return row
		}
	}
	t.Fatalf("slot %d missing from a %d-row refresh", slot, count)
	return nil
}

// TestHandlerEnchantMainRefreshKeepsEquipmentQualitySeed is the regression guard
// for the live report "enchanting changes the grade of the equipment in the
// bag, and only re-selecting the character restores it".
//
// A bag enchant answers with a full 0x000D main-list refresh, which re-encodes
// every row from the durable inventory. raw+0x06 of an equipment row is its
// quality seed, so encoding the stack count there (equipment always stores
// Count 1) repainted the whole bag as the lowest grade until the client
// re-read the list on the next select-character. The bead row must still carry
// its remaining stack count, which is what that same word means for stackables.
func TestHandlerEnchantMainRefreshKeepsEquipmentQualitySeed(t *testing.T) {
	const (
		beadSlot    int16  = 12
		targetSlot  int16  = 30
		siblingSlot int16  = 31
		targetSeed  uint32 = 998506240
		siblingSeed uint32 = 345678901
		beadCount   int64  = 5
	)
	for _, test := range []struct {
		name           string
		carryExtraSeed bool
	}{
		{name: "grade mirrored into Extra", carryExtraSeed: true},
		{name: "grade only in the stored row", carryExtraSeed: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			repos := dnfrepomemory.NewMemoryGroup()
			beadRaw := make([]byte, currentItemListEntrySize)
			saveTestInventory(t, ctx, repos, dnfrepo.InventoryRecord{
				CharacterID: "77",
				Slots: map[string]dnfrepo.ItemStack{
					slotKey(listTypeMain, beadSlot): {
						ItemID:   8801,
						Count:    beadCount,
						RawEntry: beadRaw,
						Extra: map[string]string{
							"item_kind":      "stackable",
							"stackable_type": "[etc]",
							"pvf_path":       "stackable/bead/fire_bead.stk",
						},
					},
					slotKey(listTypeMain, targetSlot):  currentEquipmentQualityStack(targetSlot, 700, targetSeed, test.carryExtraSeed),
					slotKey(listTypeMain, siblingSlot): currentEquipmentQualityStack(siblingSlot, 701, siblingSeed, test.carryExtraSeed),
				},
			})

			got := handleEnchantRequest(t, repos, coatEnchantResolver(9001, nil),
				enchantRequestBody(listTypeMain, beadSlot, listTypeMain, targetSlot))
			if len(got.UpperResponses) != 2 || got.UpperResponses[0].MsgID != msgItemListRefresh {
				t.Fatalf("result = %+v", got)
			}
			body := got.UpperResponses[0].Body

			target := mainListRefreshRowForSlot(t, body, targetSlot)
			if got := binary.LittleEndian.Uint32(target[6:10]); got != targetSeed {
				t.Fatalf("enchanted row grade seed=%d want=%d row=% X", got, targetSeed, target[:0x10])
			}
			// The enchant rewrites one slot's state; every other equipment row
			// in the same refresh must keep the grade the character loaded with.
			sibling := mainListRefreshRowForSlot(t, body, siblingSlot)
			if got := binary.LittleEndian.Uint32(sibling[6:10]); got != siblingSeed {
				t.Fatalf("sibling row grade seed=%d want=%d row=% X", got, siblingSeed, sibling[:0x10])
			}
			// A stackable row still carries its remaining count in that word.
			bead := mainListRefreshRowForSlot(t, body, beadSlot)
			if got := binary.LittleEndian.Uint32(bead[6:10]); got != uint32(beadCount-1) {
				t.Fatalf("bead row count=%d want=%d row=% X", got, beadCount-1, bead[:0x10])
			}
		})
	}
}
