package dnfbridge

import (
	"context"
	"errors"
	"testing"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
	appkit "longheng.io/server/internal/platform/app"
)

func TestGMEquipmentUnequipAndDeleteAreOfflineAndAtomic(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	must := func(err error) {
		if err != nil {
			t.Fatal(err)
		}
	}
	must(repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "11", AccountID: "dnf:equipment", Name: "Equipment", Job: "0", Level: 90,
	}))
	must(repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "11",
		Slots: map[string]dnfrepo.ItemStack{
			"0:9": {ItemID: 99, Count: 1},
		},
		Warehouse: map[string]dnfrepo.ItemStack{},
	}))
	must(repositories.AccountInventory.Save(ctx, dnfrepo.AccountInventoryRecord{AccountID: "dnf:equipment", Slots: map[string]dnfrepo.ItemStack{}}))
	must(repositories.Equipment.Save(ctx, dnfrepo.EquipmentRecord{
		CharacterID: "11",
		Entries: map[string]dnfrepo.EquipmentEntry{
			"12": {SlotIndex: 12, ItemID: 700, RawEntry: []byte{0x12, 0x34}, Extra: map[string]string{"reinforce": "12"}},
			"13": {SlotIndex: 13, ItemID: 701, RawEntry: []byte{0x56, 0x78}},
		},
	}))
	service := NewWithOptions(&appkit.Env{}, ServiceOptions{RepositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true }})

	snapshot, err := service.GMUnequipEquipment(ctx, "11", "12")
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Items) != 3 {
		t.Fatalf("snapshot items=%+v", snapshot.Items)
	}
	inventory, found, err := repositories.Inventory.Load(ctx, "11")
	if err != nil || !found {
		t.Fatalf("load inventory found=%v err=%v", found, err)
	}
	unequipped := inventory.Slots["0:10"]
	if unequipped.ItemID != 700 || unequipped.Count != 1 || unequipped.Extra["reinforce"] != "12" {
		t.Fatalf("unequipped stack=%+v", unequipped)
	}
	equipment, found, err := repositories.Equipment.Load(ctx, "11")
	if err != nil || !found {
		t.Fatalf("load equipment found=%v err=%v", found, err)
	}
	if _, exists := equipment.Entries["12"]; exists {
		t.Fatalf("slot 12 remained equipped: %+v", equipment.Entries)
	}

	snapshot, err = service.GMDeleteEquipment(ctx, "11", "3:13")
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range snapshot.Items {
		if item.Container == "equipment" && item.Slot == 13 {
			t.Fatalf("deleted equipment remained in snapshot: %+v", item)
		}
	}
	equipment, _, _ = repositories.Equipment.Load(ctx, "11")
	if _, exists := equipment.Entries["13"]; exists {
		t.Fatalf("slot 13 remained equipped: %+v", equipment.Entries)
	}

	service.gameSessions[11] = &gameSession{selectedCharacterID: 11}
	if _, err := service.GMUnequipEquipment(ctx, "11", "12"); !errors.Is(err, ErrGMCharacterOnline) {
		t.Fatalf("online unequip error=%v", err)
	}
}
