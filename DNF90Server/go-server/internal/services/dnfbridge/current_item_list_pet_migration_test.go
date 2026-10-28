package dnfbridge

import (
	"context"
	"testing"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

func TestMigrateLegacyPetItemsFromMainInventory(t *testing.T) {
	catalog := mustCurrentCeraShopTestCatalog(t)
	catalog.items.itemCache[7001] = dungeonDropItemDefinition{
		ItemID:        7001,
		Kind:          dungeonDropItemEquipment,
		PVFPath:       "equipment/creature/artifact_red/test_artifact.equ",
		EquipmentType: "[artifact red]",
	}
	catalog.items.itemCache[7002] = dungeonDropItemDefinition{
		ItemID:        7002,
		Kind:          dungeonDropItemStackable,
		PVFPath:       "stackable/cash/creature/rename_card.stk",
		StackableType: "[creature]",
		StackLimit:    1,
	}

	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{CharacterID: "26", AccountID: "dnf:1"}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "26",
		Slots: map[string]dnfrepo.ItemStack{
			"0:4": {ItemID: 7001, Count: 1, Extra: map[string]string{"item_kind": "equipment"}},
			"0:5": {ItemID: 7002, Count: 1, Extra: map[string]string{"item_kind": "stackable", "stackable_type": "[creature]"}},
			"0:6": {ItemID: 37, Count: 1, Extra: map[string]string{"item_kind": "stackable", "stackable_type": "[waste]"}},
		},
	}); err != nil {
		t.Fatal(err)
	}
	service := &Service{
		ceraShopCatalog: catalog,
		repositoryProvider: func() (dnfrepo.Group, bool) {
			return repositories, true
		},
	}

	migrated, err := service.migrateLegacyPetItemsFromMainInventory(ctx, repositories, "26")
	if err != nil || migrated != 2 {
		t.Fatalf("migrate legacy pet items migrated=%d err=%v", migrated, err)
	}
	inventory, found, err := repositories.Inventory.Load(ctx, "26")
	if err != nil || !found {
		t.Fatalf("load migrated inventory found=%t err=%v", found, err)
	}
	if _, found := inventory.Slots["0:4"]; found {
		t.Fatal("legacy pet artifact remained in main inventory")
	}
	if _, found := inventory.Slots["0:5"]; found {
		t.Fatal("legacy pet rename card remained in main inventory")
	}
	if _, found := inventory.Slots["0:6"]; !found {
		t.Fatal("ordinary stackable was moved out of main inventory")
	}
	if artifact := inventory.Slots["7:140"]; artifact.ItemID != 7001 || artifact.Count != 1 {
		t.Fatalf("migrated artifact=%+v", artifact)
	}
	if card := inventory.Slots["7:189"]; card.ItemID != 7002 || card.Count != 1 {
		t.Fatalf("migrated rename card=%+v", card)
	}
}
