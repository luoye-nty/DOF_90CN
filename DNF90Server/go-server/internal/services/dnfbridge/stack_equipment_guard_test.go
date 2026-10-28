package dnfbridge

import (
	"testing"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

// 现场 2026-09-27：魔盒发奖缺陷留下「一行多件装备」（线上 90 行 357 件，
// 如 28309 单格 49 件、109010047 单格 8 件）。打孔/镶嵌、装备跨界这类
// 「一份材料/一颗石头作用一件」的入口必须自己挡住，否则一次代价作用到整行。
func TestCurrentStackIsSingleEquipment(t *testing.T) {
	cases := []struct {
		name  string
		stack dnfrepo.ItemStack
		want  bool
	}{
		{"装备一件", dnfrepo.ItemStack{ItemID: 109010047, Count: 1, Extra: map[string]string{"item_kind": "equipment"}}, true},
		{"装备八件", dnfrepo.ItemStack{ItemID: 109010047, Count: 8, Extra: map[string]string{"item_kind": "equipment"}}, false},
		{"装备四十九件", dnfrepo.ItemStack{ItemID: 28309, Count: 49, Extra: map[string]string{"item_kind": "equipment"}}, false},
		{"材料照旧", dnfrepo.ItemStack{ItemID: 3037, Count: 9999, Extra: map[string]string{"item_kind": "stackable"}}, true},
		{"无 kind 放行", dnfrepo.ItemStack{ItemID: 500, Count: 3}, true},
	}
	for _, testCase := range cases {
		if got := currentStackIsSingleEquipment(testCase.stack); got != testCase.want {
			t.Fatalf("%s: got=%t want=%t", testCase.name, got, testCase.want)
		}
	}
}

func TestCurrentCargoRuleAllowsRejectsStackedEquipmentRow(t *testing.T) {
	rule := currentCargoTransportRule{}
	definition := dungeonDropItemDefinition{ItemID: 109010047, Kind: dungeonDropItemEquipment}
	single := dnfrepo.ItemStack{ItemID: 109010047, Count: 1, Extra: map[string]string{"item_kind": "equipment"}}
	if !currentCargoRuleAllows(rule, definition, single) {
		t.Fatalf("单件装备必须允许跨界: %+v", single)
	}
	stacked := dnfrepo.ItemStack{ItemID: 109010047, Count: 8, Extra: map[string]string{"item_kind": "equipment"}}
	if currentCargoRuleAllows(rule, definition, stacked) {
		t.Fatalf("堆叠装备行不允许跨界（一颗跨界石/一次运费搬走 N 件）: %+v", stacked)
	}
}

func TestCurrentFindMainInventorySocketTargetRejectsStackedEquipmentRow(t *testing.T) {
	const itemID = int64(109010047)
	inventory := dnfrepo.InventoryRecord{
		CharacterID: "77",
		Slots: map[string]dnfrepo.ItemStack{
			currentSocketInventoryKey(currentSocketListMain, 37): {
				ItemID: itemID,
				Count:  8, // ← 魔盒并出来的历史堆叠行
				Extra:  map[string]string{"item_kind": "equipment"},
			},
		},
	}
	if _, _, _, err := currentFindMainInventorySocketTarget(inventory, 37, itemID); err == nil {
		t.Fatalf("堆叠装备行不允许打孔/镶嵌")
	}
	inventory.Slots[currentSocketInventoryKey(currentSocketListMain, 37)] = dnfrepo.ItemStack{
		ItemID: itemID,
		Count:  1,
		Extra:  map[string]string{"item_kind": "equipment"},
	}
	key, slot, stack, err := currentFindMainInventorySocketTarget(inventory, 37, itemID)
	if err != nil {
		t.Fatalf("单件装备必须能打孔: %v", err)
	}
	if key != currentSocketInventoryKey(currentSocketListMain, 37) || slot != 37 || stack.Count != 1 {
		t.Fatalf("落点不对: key=%s slot=%d stack=%+v", key, slot, stack)
	}
}
