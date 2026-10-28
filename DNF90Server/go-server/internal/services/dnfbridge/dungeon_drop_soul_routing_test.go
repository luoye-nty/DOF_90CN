package dnfbridge

import (
	"context"
	"encoding/binary"
	"testing"

	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

// 六个灵魂与六个晶块一样住在账号共享仓库：晶块占晶体仓 354..359、灵魂占灵魂仓
// 360..365（权威表见 repository/account_shared_materials.go）。
//
// 副本掉落的落点判定过去用 CrystalWarehouseLastSlot(359) 收尾，只认晶块，把六个灵魂
// 整族挡在门外 —— 灵魂掉出来会走角色背包那条路（2026-09-21 与 NPC 商店金币购买
// 一并修掉；底层 GrantAccountSharedPickupItem 本来就按全区间 354..365 校验）。
//
// 本文件是 dungeon_drop_pickup_test.go 里
// TestCurrentDungeonPickupRoutesCubeFragmentsToSharedCrystalSlots 的**镜像**：
// 同一个夹具、同一套断言，只把映射换成灵魂。
func TestCurrentDungeonPickupRoutesSoulsToSharedWarehouseSlots(t *testing.T) {
	for _, test := range []struct {
		name   string
		itemID uint32
		slot   int16
	}{
		{"soul_common", 10100115, 360},
		{"soul_advanced", 10100116, 361},
		{"soul_rare", 10099773, 362},
		{"soul_unique", 10099774, 363},
		{"soul_legendary", 10099775, 364},
		{"soul_epic", 10158124, 365},
	} {
		t.Run(test.name, func(t *testing.T) {
			service, runtime, repositories := prepareCurrentDungeonDropTest(t, 1, test.itemID)
			connection := &bufferConn{}
			session := &gameSession{
				conn:                connection,
				connID:              "dungeon-soul-pickup-test",
				selectedCharacterID: 99,
				dungeon:             dungeonSessionState{runtime: runtime},
			}
			dropObjectKey := seedCurrentDungeonPickupDrop(t, service, runtime, test.itemID, currentSceneActorObjectKey(99))

			if err := service.handleCurrentDungeonPickup(session, currentDungeonPickupTestBody(dropObjectKey)); err != nil {
				t.Fatal(err)
			}
			pickup, rest := splitGameServerUpperPacket(t, connection.write.Bytes())
			update, trailing := splitGameServerUpperPacket(t, rest)
			if pickup.Header.MsgID != currentDungeonPickupResultOpcode || len(trailing) != 0 ||
				update.Header.MsgID != uint16(dnfenum.CmdPacketWalkoutPartyMember) || update.Body[0] != dnfrepo.MainInventoryListType ||
				binary.LittleEndian.Uint16(update.Body[1:3]) != 1 || binary.LittleEndian.Uint16(update.Body[3:5]) != uint16(test.slot) ||
				binary.LittleEndian.Uint32(update.Body[5:9]) != test.itemID || binary.LittleEndian.Uint32(update.Body[9:13]) != 1 {
				t.Fatalf("soul pickup=%+v update=%x trailing=%x (want the shared soul cell %d)", pickup.Header, update.Body, trailing, test.slot)
			}
			account, found, err := repositories.AccountInventory.Load(context.Background(), "account-1")
			if err != nil || !found {
				t.Fatalf("account inventory found=%t err=%v", found, err)
			}
			if stack := account.Slots[dnfrepo.AccountSharedInventorySlotKey(test.slot)]; stack.ItemID != int64(test.itemID) || stack.Count != 1 {
				t.Fatalf("shared soul stack=%+v", stack)
			}
			// 核心断言：角色背包必须一个格子都没被占用。
			inventory, found, err := repositories.Inventory.Load(context.Background(), "99")
			if err != nil || !found || len(inventory.Slots) != 0 {
				t.Fatalf("character inventory found=%t slots=%+v err=%v", found, inventory.Slots, err)
			}
		})
	}
}

// TestCurrentDungeonPickupAccountSharedSlotCoversCubesAndSouls 把谓词本身钉死：
// 权威表里 12 个道具（六晶块 + 六灵魂）都必须被认下，非成员一律 fail-closed。
// 这条用例存在的意义是防止"只判晶体"的半截守卫再长回来。
func TestCurrentDungeonPickupAccountSharedSlotCoversCubesAndSouls(t *testing.T) {
	canonical := dnfrepo.AccountSharedWarehouseFixedSlots()
	if len(canonical) != 12 {
		t.Fatalf("canonical table size = %d, want 12", len(canonical))
	}
	for itemID, wantSlot := range canonical {
		slot, ok := currentDungeonPickupAccountSharedSlot(uint32(itemID))
		if !ok || slot != wantSlot {
			t.Fatalf("item %d => %d,%t; want %d,true", itemID, slot, ok, wantSlot)
		}
	}
	for _, itemID := range []uint32{0, 1, 42, 3038, 3166, 3167, 3261, 3263, 10100114, 10158125} {
		if slot, ok := currentDungeonPickupAccountSharedSlot(itemID); ok {
			t.Fatalf("item %d => slot %d, want not-a-member", itemID, slot)
		}
	}
}
