package dnfbridge

import (
	"strings"
	"testing"

	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

// 公会地下城奖励礼盒（10156320）的 [booster info] 整张表都是 100380021..100380050
// 的公会勋章（[flag] 装备）和 90001..90122 的守护珠（[flag gem] 堆叠物），全部属于
// list 38。礼盒发放原先走 grantCurrentCeraShopProduct，那条路只会往 list 0 塞，
// 于是每一次开箱都被判非法，客户端把失败码 4 显示成「物品栏空间不足」。
//
// 这里钉住：奖励发放和副本拾取、NPC 公会兑换用的是同一条归位规则。
func TestGrantCurrentBoosterRewardSendsGuildMedalsToList38(t *testing.T) {
	for _, row := range []struct {
		name          string
		itemID        uint32
		kind          dungeonDropItemKind
		equipmentType string
		stackableType string
		wantPage      [2]int16
	}{
		{
			name:          "公会勋章",
			itemID:        100380028,
			kind:          dungeonDropItemEquipment,
			equipmentType: "[flag]",
			wantPage:      [2]int16{currentGuildMedalPageSlotStart, currentGuildMedalPageSlotEnd},
		},
		{
			name:          "守护珠",
			itemID:        90001,
			kind:          dungeonDropItemStackable,
			stackableType: "[flag gem]",
			wantPage:      [2]int16{currentGuardianGemPageSlotStart, currentGuardianGemPageSlotEnd},
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			definition := dungeonDropItemDefinition{
				ItemID:        row.itemID,
				Kind:          row.kind,
				EquipmentType: row.equipmentType,
				StackableType: row.stackableType,
				StackLimit:    100,
				// 这两项故意写成 list 0 的 PVF 页，证明归位靠的是
				// [flag]/[flag gem] 而不是定义里的槽位范围。
				SlotStart: 9,
				SlotEnd:   64,
			}
			inventory := &dnfrepo.InventoryRecord{
				CharacterID: "1",
				Slots:       map[string]dnfrepo.ItemStack{},
			}
			// 【合并适配 2026-09-18】本地这一侧的 grantCurrentBoosterReward 多一个
			// mainSlotCount（主背包可用槽位/开行上限）参数，与 booster_item_test.go
			// 里其它用例保持一致传 24。勋章的 list 38 落点由 definition 的
			// [flag]/[flag gem] 决定，与这个上限无关。
			keys, err := grantCurrentBoosterReward(inventory, dnfpvf.Source(nil), currentBoosterReward{
				Definition: definition,
				Count:      1,
				Placement:  currentBoosterRewardMain,
			}, 24)
			if err != nil {
				t.Fatalf("grantCurrentBoosterReward() error = %v, want the reward placed in list 38", err)
			}
			if len(keys) != 1 {
				t.Fatalf("keys = %v, want exactly one", keys)
			}
			listType, slot, parsed := parseSceneInventorySlotKey(keys[0])
			if !parsed {
				t.Fatalf("key %q is not a scene inventory slot key", keys[0])
			}
			if listType != currentGuildMedalInventoryListType {
				t.Fatalf("list = %d, want %d; key=%q", listType, currentGuildMedalInventoryListType, keys[0])
			}
			if slot < row.wantPage[0] || slot > row.wantPage[1] {
				t.Fatalf("slot = %d, want it inside the %d..%d page", slot, row.wantPage[0], row.wantPage[1])
			}
			if !strings.HasPrefix(keys[0], "38:") {
				t.Fatalf("key = %q, want the list-38 key so the caller's changedLists follows", keys[0])
			}
			if stack := inventory.Slots[keys[0]]; stack.ItemID != int64(row.itemID) {
				t.Fatalf("slot %q holds %+v", keys[0], stack)
			}
		})
	}
}

// 普通奖励必须继续落在主背包，别被这条路由顺手改掉。
func TestGrantCurrentBoosterRewardKeepsOrdinaryRewardsInTheMainList(t *testing.T) {
	inventory := &dnfrepo.InventoryRecord{
		CharacterID: "1",
		Slots:       map[string]dnfrepo.ItemStack{},
	}
	// 同上：本地签名多一个 mainSlotCount（普通消耗品仍走主背包放置逻辑，这里传 24）。
	keys, err := grantCurrentBoosterReward(inventory, dnfpvf.Source(nil), currentBoosterReward{
		Definition: dungeonDropItemDefinition{
			ItemID:        3011,
			Kind:          dungeonDropItemStackable,
			StackableType: "[material]",
			StackLimit:    100,
			SlotStart:     9,
			SlotEnd:       64,
		},
		Count:     1,
		Placement: currentBoosterRewardMain,
	}, 24)
	if err != nil {
		t.Fatalf("grantCurrentBoosterReward() error = %v", err)
	}
	if len(keys) != 1 {
		t.Fatalf("keys = %v, want exactly one", keys)
	}
	if listType, _, _ := parseSceneInventorySlotKey(keys[0]); listType != dnfrepo.MainInventoryListType {
		t.Fatalf("list = %d, want the main inventory %d", listType, dnfrepo.MainInventoryListType)
	}
}
