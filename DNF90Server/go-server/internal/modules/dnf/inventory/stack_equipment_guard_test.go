package inventory

import (
	"context"
	"encoding/binary"
	"testing"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

// 现场 2026-09-27（线上 203.0.113.11）：魔盒发奖缺陷把 N 件同名装备并进一格
// （dnf_inventory_items.item_count = 2..49，线上实测 90 行 357 件，extra 是
// item_kind=equipment + quality_seed=999999998 的魔盒指纹）。客户端装备行的
// 0x77 里 0x06 是品质种子而不是数量，玩家界面上看不见 N；于是所有「一份材料
// 作用一件」的入口都必须自己挡住这种行，否则一次费用会作用到整行 N 件上。
func TestEquipmentStackCountIsSingle(t *testing.T) {
	cases := []struct {
		name  string
		stack dnfrepo.ItemStack
		want  bool
	}{
		{"装备一件", dnfrepo.ItemStack{ItemID: 109010047, Count: 1, Extra: map[string]string{"item_kind": "equipment"}}, true},
		{"装备堆叠", dnfrepo.ItemStack{ItemID: 109010047, Count: 8, Extra: map[string]string{"item_kind": "equipment"}}, false},
		{"装备堆叠(kind 键)", dnfrepo.ItemStack{ItemID: 28309, Count: 49, Extra: map[string]string{"kind": "equipment"}}, false},
		{"装备堆叠(大小写)", dnfrepo.ItemStack{ItemID: 28309, Count: 2, Extra: map[string]string{"item_kind": "Equipment"}}, false},
		{"可叠加道具照旧放行", dnfrepo.ItemStack{ItemID: 3037, Count: 9999, Extra: map[string]string{"item_kind": "stackable"}}, true},
		{"没有 kind 的行放行", dnfrepo.ItemStack{ItemID: 500, Count: 5, Extra: map[string]string{}}, true},
		{"nil extra 放行", dnfrepo.ItemStack{ItemID: 500, Count: 5}, true},
	}
	for _, testCase := range cases {
		if got := equipmentStackCountIsSingle(testCase.stack); got != testCase.want {
			t.Fatalf("%s: equipmentStackCountIsSingle=%t want %t", testCase.name, got, testCase.want)
		}
	}
}

func TestIsUpgradeableEquipmentStackRequiresSingleEquipment(t *testing.T) {
	single := dnfrepo.ItemStack{ItemID: 700, Count: 1, Extra: map[string]string{"item_kind": "equipment"}}
	if !isUpgradeableEquipmentStack(single) {
		t.Fatalf("单件装备必须可强化: %+v", single)
	}
	stacked := dnfrepo.ItemStack{ItemID: 700, Count: 5, Extra: map[string]string{"item_kind": "equipment"}}
	if isUpgradeableEquipmentStack(stacked) {
		t.Fatalf("堆叠装备行绝不能进强化: %+v", stacked)
	}
}

func TestOwnerUpgradeRejectsStackedEquipmentRow(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	raw := make([]byte, 0x77)
	binary.LittleEndian.PutUint16(raw[0x00:0x02], 9)
	binary.LittleEndian.PutUint32(raw[0x02:0x06], 700)
	binary.LittleEndian.PutUint32(raw[0x06:0x0A], 999999998)
	raw[0x0A] = 2
	binary.LittleEndian.PutUint16(raw[0x0B:0x0D], 45)
	saveTestCharacter(t, ctx, repos, dnfrepo.CharacterRecord{CharacterID: "77", AccountID: "acc", Stats: map[string]int64{"gold": 12345}})
	saveTestInventory(t, ctx, repos, dnfrepo.InventoryRecord{
		CharacterID: "77",
		Slots: map[string]dnfrepo.ItemStack{
			slotKey(listTypeMain, 9): {
				ItemID:   700,
				Count:    5, // ← 魔盒并出来的历史堆叠行
				RawEntry: raw,
				Extra: map[string]string{
					"item_kind":      "equipment",
					"quality_seed":   "999999998",
					"durability":     "45",
					"max_durability": "45",
				},
			},
			slotKey(listTypeMain, 121): {
				ItemID: 5000,
				Count:  3,
				Extra: map[string]string{
					"item_kind":   "stackable",
					"pvf_path":    "stackable/material/upgrade.mat",
					"stack_limit": "999",
				},
			},
		},
	})
	owner, err := NewOwner(repos)
	if err != nil {
		t.Fatal(err)
	}
	result, err := owner.Upgrade(ctx, NewUpgradeCommand(alignedcmd.Request{
		AccountID:           "acc",
		SelectedCharacterID: 77,
	}, UpgradeItemRequest{
		Mode:                    "reinforce",
		TargetSlotIndex:         9,
		TargetItemTemplateID:    700,
		MaterialSlotIndex:       121,
		OptionalTicketSlotIndex: -1,
	}))
	if err != nil {
		t.Fatalf("Upgrade error = %v", err)
	}
	if result.Success || result.Changed {
		t.Fatalf("堆叠装备行不允许被强化: %+v", result)
	}
	if result.ErrorCode != upgradeErrorInvalidTarget {
		t.Fatalf("error code = %d, want %d", result.ErrorCode, upgradeErrorInvalidTarget)
	}
	loaded := loadTestInventory(t, ctx, repos, "77")
	if got := loaded.Slots[slotKey(listTypeMain, 9)].Extra["reinforce"]; got != "" {
		t.Fatalf("被拒的强化不许写等级: reinforce=%q", got)
	}
	if got := loaded.Slots[slotKey(listTypeMain, 121)].Count; got != 3 {
		t.Fatalf("被拒的强化不许扣材料: material=%d want 3", got)
	}
}
