package dnfbridge

import (
	"context"
	"encoding/binary"
	"testing"

	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

// A guild medal bought through the material-exchange shop (guild NPC) must be
// persisted on the list-38 medal page and announced under list byte 38.
// Before the fix it landed in the list-0 equipment page: the client showed it
// in the medal tab but could not equip it until a reselect re-sorted the rows.
func TestCurrentNPCShopBuyGuildMedalExchangeLandsInGuildMedalList(t *testing.T) {
	shop, items := mustCurrentNPCShopTestCatalogs(t)
	repositories := mustCurrentNPCShopRepositories(t, 1000, map[string]dnfrepo.ItemStack{"0:121": {ItemID: 900, Count: 10}})
	connection := &bufferConn{}
	service := currentNPCShopTestService(repositories, shop, items)
	session := &gameSession{conn: connection, connID: "npc-shop-guild-medal", selectedCharacterID: 19}
	if err := service.handleGameCommand(session, byte(dnfenum.GameCmdCommand), uint16(dnfenum.CmdPacketBuyItem), currentNPCShopBuyTestBody(702, 0, 28, 45)); err != nil {
		t.Fatal(err)
	}

	ack, rest := splitGameServerUpperPacket(t, connection.write.Bytes())
	if ack.Header.MsgID != uint16(dnfenum.CmdPacketBuyItem) || ack.Body[0] != 1 {
		t.Fatalf("medal ACK=%x", ack.Body)
	}
	ackItem := ack.Body[17 : 17+currentItemListEntryWireSize]
	if int16(binary.LittleEndian.Uint16(ackItem[0:2])) != currentGuildMedalPageSlotStart || binary.LittleEndian.Uint32(ackItem[2:6]) != 702 {
		t.Fatalf("medal ACK item row=%x", ackItem)
	}
	mainUpdate, rest := splitGameServerUpperPacket(t, rest)
	if mainUpdate.Header.MsgID != uint16(dnfenum.CmdPacketWalkoutPartyMember) || mainUpdate.Body[0] != dnfrepo.MainInventoryListType || binary.LittleEndian.Uint16(mainUpdate.Body[1:3]) != 2 {
		t.Fatalf("list-0 update header=%+v body=%x", mainUpdate.Header, mainUpdate.Body[:3])
	}
	for offset := 3; offset < len(mainUpdate.Body); offset += currentItemListEntryWireSize {
		row := mainUpdate.Body[offset : offset+currentItemListEntryWireSize]
		if binary.LittleEndian.Uint32(row[2:6]) == 702 {
			t.Fatalf("medal row leaked into the list-0 update: %x", row)
		}
	}
	medalUpdate, trailing := splitGameServerUpperPacket(t, rest)
	if len(trailing) != 0 || medalUpdate.Header.MsgID != uint16(dnfenum.CmdPacketWalkoutPartyMember) || medalUpdate.Body[0] != currentGuildMedalInventoryListType || binary.LittleEndian.Uint16(medalUpdate.Body[1:3]) != 1 {
		t.Fatalf("list-38 update header=%+v body=%x trailing=%d", medalUpdate.Header, medalUpdate.Body[:3], len(trailing))
	}
	row := medalUpdate.Body[3:]
	if int16(binary.LittleEndian.Uint16(row[0:2])) != currentGuildMedalPageSlotStart || binary.LittleEndian.Uint32(row[2:6]) != 702 || binary.LittleEndian.Uint32(row[6:10]) != 1 {
		t.Fatalf("list-38 medal row=%x", row)
	}

	inventory, _, _ := repositories.Inventory.Load(context.Background(), "19")
	stack, found := inventory.Slots[currentCeraShopInventorySlotKey(currentGuildMedalInventoryListType, currentGuildMedalPageSlotStart)]
	if !found || stack.ItemID != 702 || stack.Count != 1 || stack.Extra["last_grant_source"] != "npc_shop" {
		t.Fatalf("medal stack=%+v found=%t", stack, found)
	}
	for key, stack := range inventory.Slots {
		if stack.ItemID == 702 && key != "38:0" {
			t.Fatalf("medal persisted at %s", key)
		}
	}
	if material := inventory.Slots["0:121"]; material.ItemID != 900 || material.Count != 8 {
		t.Fatalf("material stack=%+v want 900x8", material)
	}
}

// Guardian gems bought for gold merge into the existing list-38 gem-page stack
// and never touch the list-0 material page.
func TestCurrentNPCShopBuyGuardianGemMergesIntoGemPage(t *testing.T) {
	shop, items := mustCurrentNPCShopTestCatalogs(t)
	existingKey := currentCeraShopInventorySlotKey(currentGuildMedalInventoryListType, currentGuardianGemPageSlotStart)
	repositories := mustCurrentNPCShopRepositories(t, 1000, map[string]dnfrepo.ItemStack{existingKey: {ItemID: 606, Count: 2}})
	connection := &bufferConn{}
	service := currentNPCShopTestService(repositories, shop, items)
	session := &gameSession{conn: connection, connID: "npc-shop-guardian-gem", selectedCharacterID: 19}
	if err := service.handleGameCommand(session, byte(dnfenum.GameCmdCommand), uint16(dnfenum.CmdPacketBuyItem), currentNPCShopBuyTestBody(606, 3, 44, 88)); err != nil {
		t.Fatal(err)
	}

	ack, rest := splitGameServerUpperPacket(t, connection.write.Bytes())
	if ack.Header.MsgID != uint16(dnfenum.CmdPacketBuyItem) || ack.Body[0] != 1 || binary.LittleEndian.Uint32(ack.Body[1:5]) != 700 {
		t.Fatalf("gem ACK=%x", ack.Body)
	}
	mainUpdate, rest := splitGameServerUpperPacket(t, rest)
	if mainUpdate.Body[0] != dnfrepo.MainInventoryListType || binary.LittleEndian.Uint16(mainUpdate.Body[1:3]) != 1 {
		t.Fatalf("list-0 update should carry only the wallet row: %x", mainUpdate.Body[:3])
	}
	wallet := mainUpdate.Body[3:]
	if binary.LittleEndian.Uint16(wallet[0:2]) != 0 || binary.LittleEndian.Uint32(wallet[6:10]) != 700 {
		t.Fatalf("wallet row=%x", wallet)
	}
	gemUpdate, trailing := splitGameServerUpperPacket(t, rest)
	if len(trailing) != 0 || gemUpdate.Body[0] != currentGuildMedalInventoryListType || binary.LittleEndian.Uint16(gemUpdate.Body[1:3]) != 1 {
		t.Fatalf("list-38 update=%x trailing=%d", gemUpdate.Body[:3], len(trailing))
	}
	row := gemUpdate.Body[3:]
	if int16(binary.LittleEndian.Uint16(row[0:2])) != currentGuardianGemPageSlotStart || binary.LittleEndian.Uint32(row[2:6]) != 606 || binary.LittleEndian.Uint32(row[6:10]) != 5 {
		t.Fatalf("list-38 gem row=%x", row)
	}

	inventory, _, _ := repositories.Inventory.Load(context.Background(), "19")
	if stack := inventory.Slots[existingKey]; stack.ItemID != 606 || stack.Count != 5 {
		t.Fatalf("gem stack=%+v want 606x5", stack)
	}
	for key, stack := range inventory.Slots {
		if stack.ItemID == 606 && key != existingKey {
			t.Fatalf("gem persisted at %s", key)
		}
	}
}

// Characters that bought medals/gems before the fix still hold them in list 0.
// The list-0 snapshot bootstrap moves them onto their list-38 pages once.
func TestMigrateLegacyGuildMedalItemsFromMainInventory(t *testing.T) {
	shop, items := mustCurrentNPCShopTestCatalogs(t)
	repositories := mustCurrentNPCShopRepositories(t, 1000, map[string]dnfrepo.ItemStack{
		"0:10":  {ItemID: 702, Count: 1, Extra: map[string]string{"last_grant_source": "npc_shop"}},
		"0:121": {ItemID: 606, Count: 4},
		"0:122": {ItemID: 900, Count: 7},
		"38:0":  {ItemID: 702, Count: 1},
	})
	service := currentNPCShopTestService(repositories, shop, items)
	ctx := context.Background()
	migrated, err := service.migrateLegacyGuildMedalItemsFromMainInventory(ctx, repositories, "19")
	if err != nil {
		t.Fatal(err)
	}
	if migrated != 2 {
		t.Fatalf("migrated=%d want 2", migrated)
	}
	inventory, _, _ := repositories.Inventory.Load(ctx, "19")
	if stack := inventory.Slots["38:1"]; stack.ItemID != 702 || stack.Count != 1 || stack.Extra["last_grant_source"] != "npc_shop" {
		t.Fatalf("migrated medal=%+v", stack)
	}
	if stack := inventory.Slots["38:49"]; stack.ItemID != 606 || stack.Count != 4 {
		t.Fatalf("migrated gem=%+v", stack)
	}
	if stack := inventory.Slots["0:122"]; stack.ItemID != 900 || stack.Count != 7 {
		t.Fatalf("ordinary material moved: %+v", stack)
	}
	for _, key := range []string{"0:10", "0:121"} {
		if _, still := inventory.Slots[key]; still {
			t.Fatalf("%s still present after migration", key)
		}
	}
	again, err := service.migrateLegacyGuildMedalItemsFromMainInventory(ctx, repositories, "19")
	if err != nil || again != 0 {
		t.Fatalf("second migration=%d err=%v", again, err)
	}
}
