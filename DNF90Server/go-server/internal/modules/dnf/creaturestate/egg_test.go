package creaturestate

import (
	"context"
	"testing"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

// 线上 Script.pvf 里 2681466 = `equipment/creature/egg_SDCharacterCreature_ft.equ`
// （宠物蛋 (迷你气功师)，带 [output index] 2681467）；
// 400990167 / 400990168 是直接发放的真宠物（无 [output index]）。
const (
	testPetEggItemID  int64  = 2681466
	testRealPetItemID int64  = 400990167
	testPetEggSerial  uint32 = 2
)

// TestReconcileHealsPetEggBinding 覆盖现场：蛋被当成"直接发放的宠物"，拿到了
// serial 并建出了宠物记录，于是丢弃侧按"已绑定宠物"拒绝（「宠物蛋无法丢弃」）。
// 对账必须自愈：清掉行上的绑定 extra、删掉误建的宠物记录。
func TestReconcileHealsPetEggBinding(t *testing.T) {
	ctx := context.Background()
	const characterID = "77"

	SetPetEggProbe(func(itemID int64) bool { return itemID == testPetEggItemID })
	defer SetPetEggProbe(nil)

	repos := dnfrepomemory.NewMemoryGroup()
	if err := repos.Inventory.Save(ctx, dnfrepo.InventoryRecord{CharacterID: characterID, Slots: map[string]dnfrepo.ItemStack{
		"7:0": {ItemID: testPetEggItemID, Count: 1, Extra: map[string]string{
			"creature_serial_or_handle": "2",
			"creature_key":              "2",
			"item_kind":                 "equipment",
			"equipment_type":            "[creature]",
			"pvf_path":                  "equipment/creature/egg_SDCharacterCreature_ft.equ",
		}},
		"7:1": {ItemID: testRealPetItemID, Count: 1, Extra: map[string]string{
			"creature_serial_or_handle": "1",
			"item_kind":                 "equipment",
			"equipment_type":            "[creature]",
			"pvf_path":                  "equipment/creature/chn_2018_SpringFestival/chn_400990167.equ",
		}},
	}}); err != nil {
		t.Fatal(err)
	}
	if err := repos.Equipment.Save(ctx, dnfrepo.EquipmentRecord{CharacterID: characterID, Entries: map[string]dnfrepo.EquipmentEntry{}}); err != nil {
		t.Fatal(err)
	}
	if err := repos.Pet.Save(ctx, dnfrepo.PetRecord{CharacterID: characterID, Entries: map[string]dnfrepo.PetEntry{
		"2": {PetKey: "2", CreatureKey: 2, ItemID: testPetEggItemID, SourceListType: 7, SourceSlotIndex: 0},
		"1": {PetKey: "1", CreatureKey: 1, ItemID: testRealPetItemID, SourceListType: 7, SourceSlotIndex: 1},
	}}); err != nil {
		t.Fatal(err)
	}

	if _, err := ReconcileInventory(ctx, characterID, repos.Inventory, repos.Equipment, repos.Pet); err != nil {
		t.Fatalf("ReconcileInventory: %v", err)
	}

	inventory, found, err := repos.Inventory.Load(ctx, characterID)
	if err != nil || !found {
		t.Fatalf("load inventory found=%t err=%v", found, err)
	}
	egg := inventory.Slots["7:0"]
	if !stackLacksPetBinding(egg) {
		t.Fatalf("蛋行仍带宠物绑定 extra: %+v", egg.Extra)
	}

	petRecord, found, err := repos.Pet.Load(ctx, characterID)
	if err != nil || !found {
		t.Fatalf("load pets found=%t err=%v", found, err)
	}
	if _, still := petRecord.Entries["2"]; still {
		t.Fatalf("蛋的宠物记录没有被清理: %+v", petRecord.Entries)
	}
	// 真宠物必须不受影响。
	if _, ok := petRecord.Entries["1"]; !ok {
		t.Fatalf("真宠物的记录被误删: %+v", petRecord.Entries)
	}
	if got := inventory.Slots["7:1"].Extra["creature_serial_or_handle"]; got != "1" {
		t.Fatalf("真宠物的 serial 被误改: %q", got)
	}
}

// TestReconcileKeepsEggBehaviourWithoutProbe 钉住回退路径：判据未装配（隔离测试、
// 未接线的调用方）时必须完全等于改动前的行为。
func TestReconcileKeepsEggBehaviourWithoutProbe(t *testing.T) {
	ctx := context.Background()
	const characterID = "78"

	SetPetEggProbe(nil)
	defer SetPetEggProbe(nil)

	repos := dnfrepomemory.NewMemoryGroup()
	if err := repos.Inventory.Save(ctx, dnfrepo.InventoryRecord{CharacterID: characterID, Slots: map[string]dnfrepo.ItemStack{
		"7:0": {ItemID: testPetEggItemID, Count: 1, Extra: map[string]string{
			"item_kind":      "equipment",
			"equipment_type": "[creature]",
			"pvf_path":       "equipment/creature/egg_SDCharacterCreature_ft.equ",
		}},
	}}); err != nil {
		t.Fatal(err)
	}
	if err := repos.Equipment.Save(ctx, dnfrepo.EquipmentRecord{CharacterID: characterID, Entries: map[string]dnfrepo.EquipmentEntry{}}); err != nil {
		t.Fatal(err)
	}
	if err := repos.Pet.Save(ctx, dnfrepo.PetRecord{CharacterID: characterID, Entries: map[string]dnfrepo.PetEntry{}}); err != nil {
		t.Fatal(err)
	}

	if _, err := ReconcileInventory(ctx, characterID, repos.Inventory, repos.Equipment, repos.Pet); err != nil {
		t.Fatalf("ReconcileInventory: %v", err)
	}
	inventory, _, _ := repos.Inventory.Load(ctx, characterID)
	if got := inventory.Slots["7:0"].Extra["creature_serial_or_handle"]; got == "" {
		t.Fatal("未装配判据时不应改变行为（旧行为会给蛋分配 serial）")
	}
}

func stackLacksPetBinding(stack dnfrepo.ItemStack) bool {
	if _, ok := stackSerial(stack.Extra); ok {
		return false
	}
	for _, key := range []string{"creature_serial_or_handle", "creature_key", "creature_serial", "pet_serial"} {
		if stack.Extra[key] != "" {
			return false
		}
	}
	return true
}
