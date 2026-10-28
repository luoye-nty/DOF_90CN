package dnfbridge

import (
	"path/filepath"
	"testing"

	platformpvf "longheng.io/server/internal/platform/pvf"
)

func newMonsterCardUpgradeTestCatalog(t *testing.T) (*pvfDungeonDropCatalog, dungeonDropCatalogTestSource) {
	t.Helper()
	source := dungeonDropCatalogTestSource{
		"monster/monster.lst":                  "",
		"equipment/equipment.lst":              "700 `weapon/test.equ`\n",
		"stackable/stackable.lst":              "3653 `monstercard/target.stk`\n3654 `monstercard/material.stk`\n3655 `material/not_card.stk`\n3656 `monstercard/no_upgrade.stk`\n",
		"equipment/weapon/test.equ":            "[equipment type] `[weapon]`\n",
		"stackable/monstercard/target.stk":     "[stackable type] `[material expert job]`\n[stack limit] 1000\n[item category] `monster card`\n[rarity] 3\n[enchant table]\n[enchant index]\n0\n[value] 10\n[/enchant index]\n[enchant index]\n1\n[value] 11\n[/enchant index]\n[enchant index]\n2\n[value] 12\n[/enchant index]\n[enchant index]\n3\n[value] 13\n[/enchant index]\n[/enchant table]\n",
		"stackable/monstercard/material.stk":   "[stackable type] `[material expert job]`\n[stack limit] 1000\n[item category] `monster card`\n[rarity] 1\n[enchant index] 0\n",
		"stackable/material/not_card.stk":      "[stackable type] `[material expert job]`\n[stack limit] 1000\n[item category] `material`\n[rarity] 2\n[enchant index] 0\n",
		"stackable/monstercard/no_upgrade.stk": "[stackable type] `[material expert job]`\n[stack limit] 1000\n[item category] `monster card`\n[rarity] 2\n",
	}
	catalog, err := newPVFDungeonDropCatalog(source)
	if err != nil {
		t.Fatal(err)
	}
	return catalog, source
}

func TestResolveCurrentMonsterCardUpgradeMetadata(t *testing.T) {
	catalog, source := newMonsterCardUpgradeTestCatalog(t)
	got, err := resolveCurrentMonsterCardUpgradeMetadata(catalog, source, 3653)
	if err != nil {
		t.Fatal(err)
	}
	if got.ItemID != 3653 || got.PVFPath != "stackable/monstercard/target.stk" || got.Rarity != 3 || got.MaximumUpgrade != 3 ||
		got.StackLimit != 1000 || got.SlotStart != 233 || got.SlotEnd != 288 || got.GoldCost != 5000 || got.CalculateConst != 10 {
		t.Fatalf("resolution = %+v", got)
	}

	material, err := resolveCurrentMonsterCardUpgradeMetadata(catalog, source, 3654)
	if err != nil {
		t.Fatal(err)
	}
	if material.PVFPath == "" || material.Rarity != 1 || material.MaximumUpgrade != 0 {
		t.Fatalf("zero-stage material card = %+v", material)
	}
}

func TestResolveCurrentMonsterCardUpgradeMetadataRejectsNonCards(t *testing.T) {
	catalog, source := newMonsterCardUpgradeTestCatalog(t)
	for _, itemID := range []int64{0, 700, 3655, 3656, 9999} {
		got, err := resolveCurrentMonsterCardUpgradeMetadata(catalog, source, itemID)
		if err != nil {
			t.Fatalf("item %d resolve error = %v", itemID, err)
		}
		if got.PVFPath != "" {
			t.Fatalf("item %d unexpectedly eligible: %+v", itemID, got)
		}
	}
}

func TestRealScriptPVFLiveMonsterCard3689ResolvesUpgradePolicy(t *testing.T) {
	pvfPath := filepath.Clean(filepath.Join("..", "..", "..", "..", "runtime", "data", "dnf", "Script.pvf"))
	archive, err := platformpvf.Open(pvfPath)
	if err != nil {
		t.Skipf("runtime Script.pvf unavailable: %v", err)
	}
	catalog, err := newPVFDungeonDropCatalog(archive)
	if err != nil {
		t.Fatal(err)
	}
	got, err := resolveCurrentMonsterCardUpgradeMetadata(catalog, archive, 3689)
	if err != nil {
		t.Fatal(err)
	}
	if got.ItemID != 3689 || got.PVFPath != "stackable/monsterCard/mcard_golgo.stk" || got.Rarity != 3 || got.MaximumUpgrade != 2 ||
		got.SlotStart != 233 || got.SlotEnd != 288 || got.GoldCost != 5000 || got.CalculateConst != 10 {
		t.Fatalf("live card 3689 policy = %+v", got)
	}
}

func TestRealScriptPVFMakerLukeLightCardResolvesUpgradePolicy(t *testing.T) {
	pvfPath := filepath.Clean(filepath.Join("..", "..", "..", "..", "runtime", "data", "dnf", "Script.pvf"))
	archive, err := platformpvf.Open(pvfPath)
	if err != nil {
		t.Skipf("runtime Script.pvf unavailable: %v", err)
	}
	catalog, err := newPVFDungeonDropCatalog(archive)
	if err != nil {
		t.Fatal(err)
	}
	got, err := resolveCurrentMonsterCardUpgradeMetadata(catalog, archive, 10159184)
	if err != nil {
		t.Fatal(err)
	}
	if got.ItemID != 10159184 || got.PVFPath != "stackable/10159001/10159184.stk" || got.Rarity != 6 || got.MaximumUpgrade != 2 || got.GoldCost != 5000 {
		t.Fatalf("Maker Luke (Light) policy = %+v", got)
	}
}
