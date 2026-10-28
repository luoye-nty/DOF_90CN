package dnfbridge

import (
	"context"
	"encoding/binary"
	"reflect"
	"testing"
	"time"

	dnfepicbook "longheng.io/server/internal/modules/dnf/epicbook"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

func TestBuildCurrentEpicBookInfoBodyUsesAbsoluteSortedRows(t *testing.T) {
	body, err := buildCurrentEpicBookInfoBody(1, []currentEpicBookBalanceRow{
		{FragmentID: 10300529, Balance: 4},
		{FragmentID: 10300528, Balance: 21},
		{FragmentID: 10300529, Balance: 7},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(body) != 21 || body[0] != 1 || binary.LittleEndian.Uint32(body[1:5]) != 2 {
		t.Fatalf("body header=%x", body)
	}
	if binary.LittleEndian.Uint32(body[5:9]) != 10300528 || binary.LittleEndian.Uint32(body[9:13]) != 21 ||
		binary.LittleEndian.Uint32(body[13:17]) != 10300529 || binary.LittleEndian.Uint32(body[17:21]) != 7 {
		t.Fatalf("body rows=%x", body)
	}
}

func TestDecodeCurrentEpicBookMakeItemRequest(t *testing.T) {
	body := make([]byte, 4)
	binary.LittleEndian.PutUint32(body, 101020256)
	if got, err := decodeCurrentEpicBookMakeItemRequest(body); err != nil || got != 101020256 {
		t.Fatalf("decode=%d error=%v", got, err)
	}
	for _, invalid := range [][]byte{nil, make([]byte, 3), make([]byte, 5), make([]byte, 4)} {
		if _, err := decodeCurrentEpicBookMakeItemRequest(invalid); err == nil {
			t.Fatalf("invalid request accepted: %x", invalid)
		}
	}
}

func TestCommitCurrentEpicBookMakeItemIsAtomic(t *testing.T) {
	service, repositories, session := currentEpicBookTestService(t)
	result, err := service.commitCurrentEpicBookMakeItem(context.Background(), session, 101020256, time.Unix(1_700_000_000, 0).UTC())
	if err != nil {
		t.Fatal(err)
	}
	if result.FragmentID != 10300528 || result.FragmentBalance != 0 || len(result.ItemUpdates) != 3 {
		t.Fatalf("result=%+v", result)
	}
	book, found, err := repositories.EpicBook.Load(context.Background(), "account-1")
	if err != nil || !found || book.Balances[101020256] != 0 {
		t.Fatalf("book=%+v found=%t error=%v", book, found, err)
	}
	accountInventory, found, err := repositories.AccountInventory.Load(context.Background(), "account-1")
	if err != nil || !found {
		t.Fatalf("account inventory found=%t error=%v", found, err)
	}
	if accountInventory.Slots["0:364"].Count != 6 || accountInventory.Slots["0:365"].Count != 2 {
		t.Fatalf("materials after=%+v", accountInventory.Slots)
	}
	inventory, found, err := repositories.Inventory.Load(context.Background(), "19")
	if err != nil || !found {
		t.Fatalf("inventory found=%t error=%v", found, err)
	}
	if inventory.Slots["0:65"].Count != 500 || inventory.Slots["0:66"].Count != 500 {
		t.Fatalf("character-owned material decoys changed=%+v", inventory.Slots)
	}
	output, ok := inventory.Slots["0:9"]
	if !ok || output.ItemID != 101020256 || output.Count != 1 || len(output.RawEntry) != currentItemListEntrySizeForType(dnfrepo.MainInventoryListType) {
		t.Fatalf("output=%+v ok=%t", output, ok)
	}
}

func TestCommitCurrentEpicBookMakeItemRollsBackRejectedCrafts(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(t *testing.T, repositories dnfrepo.Group)
	}{
		{
			name: "fragment shortage",
			mutate: func(t *testing.T, repositories dnfrepo.Group) {
				t.Helper()
				if err := repositories.EpicBook.Save(context.Background(), dnfrepo.EpicBookRecord{AccountID: "account-1", Balances: map[uint32]uint32{101020256: 999}}); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "material shortage",
			mutate: func(t *testing.T, repositories dnfrepo.Group) {
				t.Helper()
				inventory, _, err := repositories.AccountInventory.Load(context.Background(), "account-1")
				if err != nil {
					t.Fatal(err)
				}
				stack := inventory.Slots["0:364"]
				stack.Count = 43
				inventory.Slots["0:364"] = stack
				if err := repositories.AccountInventory.Save(context.Background(), inventory); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "equipment inventory full",
			mutate: func(t *testing.T, repositories dnfrepo.Group) {
				t.Helper()
				inventory, _, err := repositories.Inventory.Load(context.Background(), "19")
				if err != nil {
					t.Fatal(err)
				}
				if inventory.Slots == nil {
					inventory.Slots = make(map[string]dnfrepo.ItemStack)
				}
				for slot := 9; slot <= 64; slot++ {
					inventory.Slots[currentDungeonPickupMainSlotKey(int16(slot))] = dnfrepo.ItemStack{ItemID: 90000000 + int64(slot), Count: 1}
				}
				if err := repositories.Inventory.Save(context.Background(), inventory); err != nil {
					t.Fatal(err)
				}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			service, repositories, session := currentEpicBookTestService(t)
			test.mutate(t, repositories)
			wantBook, _, _ := repositories.EpicBook.Load(context.Background(), "account-1")
			wantAccountInventory, _, _ := repositories.AccountInventory.Load(context.Background(), "account-1")
			wantInventory, _, _ := repositories.Inventory.Load(context.Background(), "19")
			if _, err := service.commitCurrentEpicBookMakeItem(context.Background(), session, 101020256, time.Unix(1_700_000_000, 0).UTC()); err == nil {
				t.Fatal("commit succeeded, want rejection")
			}
			gotBook, _, _ := repositories.EpicBook.Load(context.Background(), "account-1")
			gotAccountInventory, _, _ := repositories.AccountInventory.Load(context.Background(), "account-1")
			gotInventory, _, _ := repositories.Inventory.Load(context.Background(), "19")
			if !reflect.DeepEqual(gotBook, wantBook) || !reflect.DeepEqual(gotAccountInventory, wantAccountInventory) || !reflect.DeepEqual(gotInventory, wantInventory) {
				t.Fatalf("rejected craft changed state book=%+v account_inventory=%+v inventory=%+v", gotBook, gotAccountInventory, gotInventory)
			}
		})
	}
}

func TestHandleCurrentEpicBookMakeItemSendsAckThenInventoryAndAbsoluteBalance(t *testing.T) {
	service, _, session := currentEpicBookTestService(t)
	connection := &bufferConn{}
	session.conn = connection
	body := make([]byte, 4)
	binary.LittleEndian.PutUint32(body, 101020256)
	if err := service.handleCurrentEpicBookMakeItem(session, body); err != nil {
		t.Fatal(err)
	}
	ack, rest := splitCurrentGameServerUpperPacketAuto(t, connection.write.Bytes())
	if ack.Header.Classification != 1 || ack.Header.MsgID != 788 || len(ack.Body) != 4 || ack.Body[3] != 1 {
		t.Fatalf("ack=%+v body=%x", ack.Header, ack.Body)
	}
	update, rest := splitCurrentGameServerUpperPacketAuto(t, rest)
	if update.Header.Classification != 0 || update.Header.MsgID != 14 || len(update.Body) != 3+3+3*currentItemListEntryWireSize {
		t.Fatalf("update=%+v body=%x", update.Header, update.Body)
	}
	payload := update.Body[3:]
	if payload[0] != dnfrepo.MainInventoryListType || binary.LittleEndian.Uint16(payload[1:3]) != 3 {
		t.Fatalf("update payload header=%x", payload[:3])
	}
	for index, want := range []struct {
		slot       uint16
		itemID     uint32
		count      uint32
		checkCount bool
	}{
		{slot: 364, itemID: 10099775, count: 6, checkCount: true},
		{slot: 365, itemID: 10158124, count: 2, checkCount: true},
		{slot: 9, itemID: 101020256},
	} {
		row := payload[3+index*currentItemListEntryWireSize:]
		if binary.LittleEndian.Uint16(row[0:2]) != want.slot ||
			binary.LittleEndian.Uint32(row[2:6]) != want.itemID ||
			(want.checkCount && binary.LittleEndian.Uint32(row[6:10]) != want.count) {
			t.Fatalf("update row=%d got=%x want=%+v", index, row[:10], want)
		}
	}
	syncPacket, rest := splitCurrentGameServerUpperPacketAuto(t, rest)
	if syncPacket.Header.Classification != 0 || syncPacket.Header.MsgID != currentEpicBookInfoWireOpcode ||
		len(syncPacket.Body) != 16 || syncPacket.Body[3] != 1 ||
		binary.LittleEndian.Uint32(syncPacket.Body[8:12]) != 10300528 || binary.LittleEndian.Uint32(syncPacket.Body[12:16]) != 0 {
		t.Fatalf("sync=%+v body=%x", syncPacket.Header, syncPacket.Body)
	}
	if len(rest) != 0 {
		t.Fatalf("trailing packets=%x", rest)
	}
}

func TestSendCurrentEpicBookSnapshotUsesModeZero(t *testing.T) {
	service, repositories, session := currentEpicBookTestService(t)
	if err := repositories.EpicBook.Save(context.Background(), dnfrepo.EpicBookRecord{
		AccountID: "account-1",
		Balances:  map[uint32]uint32{101020256: 33},
	}); err != nil {
		t.Fatal(err)
	}
	connection := &bufferConn{}
	session.conn = connection
	if err := service.sendCurrentEpicBookSnapshot(session, "test"); err != nil {
		t.Fatal(err)
	}
	packet, rest := splitCurrentGameServerUpperPacketAuto(t, connection.write.Bytes())
	if packet.Header.Classification != 0 || packet.Header.MsgID != currentEpicBookInfoWireOpcode ||
		len(packet.Body) != 16 || packet.Body[3] != 0 || binary.LittleEndian.Uint32(packet.Body[4:8]) != 1 ||
		binary.LittleEndian.Uint32(packet.Body[8:12]) != 10300528 || binary.LittleEndian.Uint32(packet.Body[12:16]) != 33 {
		t.Fatalf("packet=%+v body=%x", packet.Header, packet.Body)
	}
	if len(rest) != 0 || session.selectedEpicBookBootstrapCharacterID != 19 {
		t.Fatalf("rest=%x owner=%d", rest, session.selectedEpicBookBootstrapCharacterID)
	}
}

func TestSendCurrentEpicBookSnapshotClearsEmptyAccount(t *testing.T) {
	service, repositories, session := currentEpicBookTestService(t)
	if err := repositories.EpicBook.Save(context.Background(), dnfrepo.EpicBookRecord{AccountID: "account-1"}); err != nil {
		t.Fatal(err)
	}
	connection := &bufferConn{}
	session.conn = connection
	if err := service.sendCurrentEpicBookSnapshot(session, "empty-test"); err != nil {
		t.Fatal(err)
	}
	packet, rest := splitCurrentGameServerUpperPacketAuto(t, connection.write.Bytes())
	if packet.Header.Classification != 0 || packet.Header.MsgID != currentEpicBookInfoWireOpcode ||
		len(packet.Body) != 8 || packet.Body[3] != 0 || binary.LittleEndian.Uint32(packet.Body[4:8]) != 0 {
		t.Fatalf("packet=%+v body=%x", packet.Header, packet.Body)
	}
	if len(rest) != 0 {
		t.Fatalf("trailing packets=%x", rest)
	}
}

func TestBuildCurrentEpicBookPickupResultBodyUsesVirtualAccountDestination(t *testing.T) {
	body, err := buildCurrentEpicBookPickupResultBody(0x210, 0x191, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(body) != 19 || binary.LittleEndian.Uint32(body[0:4]) != 0x210 || binary.LittleEndian.Uint16(body[4:6]) != 0x191 ||
		binary.LittleEndian.Uint16(body[14:16]) != 0 || binary.LittleEndian.Uint16(body[16:18]) != 0 || body[18] != 0 {
		t.Fatalf("body=%x", body)
	}
}

func currentEpicBookTestService(t *testing.T) (*Service, dnfrepo.Group, *gameSession) {
	t.Helper()
	catalog, err := dnfepicbook.Parse(`[equipment piece drop info]
[piece list]
101020256 10300528
[/piece list]
[/equipment piece drop info]
[equipment recipe info]
[info]
[output]
101020256
[recipe]
10300528 1000 10099775 44 10158124 6
[/recipe]
[/info]
[/equipment recipe info]`)
	if err != nil {
		t.Fatal(err)
	}
	source := dungeonDropCatalogTestSource{
		"monster/monster.lst":    "",
		dungeonDropStackableList: "10099775 `material/a.stk`\n10158124 `material/b.stk`",
		dungeonDropEquipmentList: "101020256 `character/test/epic.equ`",
		"stackable/material/a.stk": `[stackable type]
` + "`[material]`" + ` 0
[stack limit]
1000`,
		"stackable/material/b.stk": `[stackable type]
` + "`[material]`" + ` 0
[stack limit]
1000`,
		"equipment/character/test/epic.equ": `[rarity]
4
[minimum level]
85
[creation rate]
100
[durability]
45
[equipment type]
` + "`[weapon]`" + ` 0`,
	}
	itemCatalog, err := newPVFDungeonDropCatalog(source)
	if err != nil {
		t.Fatal(err)
	}
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Character.Save(context.Background(), dnfrepo.CharacterRecord{CharacterID: "19", AccountID: "account-1"}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.AccountInventory.Save(context.Background(), dnfrepo.AccountInventoryRecord{
		AccountID: "account-1",
		Slots: map[string]dnfrepo.ItemStack{
			"0:364": {ItemID: 10099775, Count: 50},
			"0:365": {ItemID: 10158124, Count: 8},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Inventory.Save(context.Background(), dnfrepo.InventoryRecord{
		CharacterID: "19",
		Slots: map[string]dnfrepo.ItemStack{
			"0:65": {ItemID: 10099775, Count: 500},
			"0:66": {ItemID: 10158124, Count: 500},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.EpicBook.Save(context.Background(), dnfrepo.EpicBookRecord{AccountID: "account-1", Balances: map[uint32]uint32{101020256: 1000}}); err != nil {
		t.Fatal(err)
	}
	service := &Service{
		options:            options{accountID: "account-1", gameUpperHeader: gameUpperHeaderServer16, gameUpperBodyCodec: gameUpperBodyCodecPlain},
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
		epicBookCatalog:    catalog,
		pvfItemCatalog:     itemCatalog,
	}
	return service, repositories, &gameSession{accountID: "account-1", selectedCharacterID: 19}
}
