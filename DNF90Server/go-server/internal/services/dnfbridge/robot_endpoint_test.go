package dnfbridge

import (
	"context"
	"testing"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

func TestRobotPrivateStoreRowsUseCurrentPVFQuantityAndEquipmentInstanceRules(t *testing.T) {
	// 交易白名单是 {free, sealing}（见 currentDungeonDiscardTradeable），所以
	// 夹具里「可交易」用 `[free]` 表达；7002 仍然靠 Bind=true 单独覆盖「绑定不可上架」。
	catalog, err := newPVFDungeonDropCatalog(dungeonDropCatalogTestSource{
		"monster/monster.lst":          "",
		"stackable/stackable.lst":      "7001 `material/trade.stk`\n7002 `material/bound.stk`\n",
		"equipment/equipment.lst":      "8001 `weapon/trade.equ`\n",
		"stackable/material/trade.stk": "[name] `Trade Material`\n[stackable type] `[material]`\n[attach type] `[free]`\n",
		"stackable/material/bound.stk": "[name] `Bound Material`\n[stackable type] `[material]`\n[attach type] `[free]`\n",
		"equipment/weapon/trade.equ":   "[name] `Trade Sword`\n[rarity] 0\n[attach type] `[free]`\n[equipment type] `[weapon]`\n",
	})
	if err != nil {
		t.Fatal(err)
	}
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Inventory.Save(context.Background(), dnfrepo.InventoryRecord{
		CharacterID: "7",
		Slots: map[string]dnfrepo.ItemStack{
			"0:12": {ItemID: 7001, Count: 9},
			"0:13": {ItemID: 7002, Count: 5, Bind: true},
			"0:14": {ItemID: 8001, Count: 1, Extra: map[string]string{"item_kind": "equipment", "quality_seed": "669297134"}},
			"2:1":  {ItemID: 7001, Count: 3},
		},
	}); err != nil {
		t.Fatal(err)
	}
	service := &Service{
		pvfItemCatalog:     catalog,
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
	}
	rows, err := service.RobotPrivateStoreRows(context.Background(), 7, 14, 1234)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].DisplayIndex != 0 || rows[0].Slot != 12 || rows[0].WireValue != 9 || rows[0].Price != 1234 ||
		rows[1].DisplayIndex != 1 || rows[1].Slot != 14 || rows[1].WireValue != 669297134 {
		t.Fatalf("rows = %+v", rows)
	}
}
