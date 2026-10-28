package dnfbridge

import (
	"context"
	"testing"

	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfitemlock "longheng.io/server/internal/modules/dnf/itemlock"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

// 两把锁都只存在于客户端：黄色装备锁在 op251 锁列表里，小的蓝色整理锁在 op713
// 位置回放里（物品行并不携带它们）。打开礼包会重发 op14 物品行，客户端重建格子
// 时两套标记一起消失 —— 活体 2026-09-15 07:36-07:40：开礼包后装备锁与装扮锁定
// 的小蓝锁都消失，整理背包或重新选角才恢复（那两条路都会重放）。
// 这条测试同时钉住"没有任何锁定时一个多余的包都不发"。
func TestPackageOpenReplaysBothLockKindsOnlyWhenSomethingIsLocked(t *testing.T) {
	for name, test := range map[string]struct {
		rows          map[string]dnfrepo.ItemStack
		wantEquipLock bool
		wantSortLock  bool
	}{
		"装备锁一行": {
			rows: map[string]dnfrepo.ItemStack{
				"0:20": {ItemID: 700, Count: 1, Extra: map[string]string{"equipment_lock_state": "1"}},
			},
			wantEquipLock: true,
		},
		"整理锁一行": {
			rows: map[string]dnfrepo.ItemStack{
				"0:21": {ItemID: 701, Count: 1, Extra: map[string]string{"item_sort_lock_state": "1"}},
			},
			wantSortLock: true,
		},
		"两把锁都有": {
			rows: map[string]dnfrepo.ItemStack{
				"0:20": {ItemID: 700, Count: 1, Extra: map[string]string{"equipment_lock_state": "1"}},
				"0:21": {ItemID: 701, Count: 1, Extra: map[string]string{"item_sort_lock_state": "1"}},
			},
			wantEquipLock: true,
			wantSortLock:  true,
		},
		"没有锁": {rows: nil},
	} {
		t.Run(name, func(t *testing.T) {
			catalog := mustCurrentBoosterTestCatalog(t)
			repositories := dnfrepomemory.NewMemoryGroup()
			ctx := context.Background()
			if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
				CharacterID: "19", AccountID: "account-1",
			}); err != nil {
				t.Fatal(err)
			}
			slots := map[string]dnfrepo.ItemStack{"0:75": {ItemID: 500, Count: 2}}
			for key, stack := range test.rows {
				slots[key] = stack
			}
			if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{
				CharacterID: "19", Slots: slots,
			}); err != nil {
				t.Fatal(err)
			}
			connection := &bufferConn{}
			service := currentBoosterTestService(catalog, repositories)
			service.gameplayTimers = newFakeCurrentDungeonDeathTimerQueue()
			session := &gameSession{conn: connection, connID: "booster-lock-replay-test", selectedCharacterID: 19}
			if err := service.handleGameCommand(
				session,
				byte(dnfenum.GameCmdCommand),
				uint16(dnfenum.CmdPacketUseBoosterItem),
				[]byte{75, 0},
			); err != nil {
				t.Fatal(err)
			}
			ack, rest := splitGameServerUpperPacket(t, connection.write.Bytes())
			if ack.Header.MsgID != uint16(dnfenum.CmdPacketUseBoosterItem) {
				t.Fatalf("ack header=%+v", ack.Header)
			}
			rest = assertCurrentBoosterSourceRefresh(t, rest, 75, 500, 1)
			_, rest = splitCurrentSceneItemListPacket(t, rest)

			// 逐包收尾：允许 op251 与 op713 各一次，其他一律不允许。
			var sawEquipLock, sawSortLock bool
			for len(rest) > 0 {
				packet, trailing := splitGameServerUpperPacket(t, rest)
				rest = trailing
				switch packet.Header.MsgID {
				case dnfitemlock.LockListMessageID:
					if sawEquipLock {
						t.Fatalf("重复的锁列表包 header=%+v", packet.Header)
					}
					sawEquipLock = true
					if packet.Header.Classification != 0 || len(packet.Body) < 2 ||
						packet.Body[0] == 0 && packet.Body[1] == 0 {
						t.Fatalf("锁列表包应为非空: header=%+v body=%x", packet.Header, packet.Body)
					}
				case uint16(dnfenum.CmdPacketRequestItemSortLock):
					if sawSortLock {
						t.Fatalf("重复的整理锁包 header=%+v", packet.Header)
					}
					sawSortLock = true
				default:
					t.Fatalf("尾部出现意外包 header=%+v", packet.Header)
				}
			}
			if sawEquipLock != test.wantEquipLock || sawSortLock != test.wantSortLock {
				t.Fatalf("锁回放 装备锁=%t 整理锁=%t want 装备锁=%t 整理锁=%t",
					sawEquipLock, sawSortLock, test.wantEquipLock, test.wantSortLock)
			}
		})
	}
}
