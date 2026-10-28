package dnfbridge

import (
	"testing"
)

func newEnchantBeadTestCatalog(t *testing.T) (*pvfDungeonDropCatalog, dungeonDropCatalogTestSource) {
	t.Helper()
	source := dungeonDropCatalogTestSource{
		"monster/monster.lst":             "",
		"stackable/stackable.lst":         "8801 `bead/fire_bead.stk`\n8802 `bead/plain_bead.stk`\n9001 `card/fire_card.stk`\n",
		"equipment/equipment.lst":         "700 `weapon/test_coat.equ`\n701 `weapon/test_sword.equ`\n",
		"stackable/bead/fire_bead.stk":    "[name] `Fire Bead`\n[stackable type] `[material]`\n[monster card id] 9001\n[bead limited usable item] 700 701\n",
		"stackable/bead/plain_bead.stk":   "[name] `Plain Bead`\n[stackable type] `[material]`\n",
		"stackable/card/fire_card.stk":    "[name] `Fire Card`\n[string data] `icon.img` `[coat]` `[weapon]`\n[enchant table]\n[enchant index]\n0\n[value] 10\n[/enchant index]\n[enchant index]\n1\n[value] 11\n[/enchant index]\n[enchant index]\n2\n[value] 12\n[/enchant index]\n[/enchant table]\n",
		"equipment/weapon/test_coat.equ":  "[name] `Test Coat`\n[equipment type] `[coat]`\n[durability] 57\n",
		"equipment/weapon/test_sword.equ": "[name] `Test Sword`\n[equipment type] `[weapon]`\n[durability] 57\n",
	}
	catalog, err := newPVFDungeonDropCatalog(source)
	if err != nil {
		t.Fatal(err)
	}
	return catalog, source
}

func TestResolveCurrentEnchantBeadMetadata(t *testing.T) {
	catalog, source := newEnchantBeadTestCatalog(t)
	resolution, err := resolveCurrentEnchantBeadMetadata(catalog, source, 8801, 700)
	if err != nil {
		t.Fatalf("resolve error = %v", err)
	}
	if resolution.CardItemID != 9001 {
		t.Fatalf("card id = %d, want 9001", resolution.CardItemID)
	}
	if len(resolution.TargetWhitelist) != 2 || resolution.TargetWhitelist[0] != 700 || resolution.TargetWhitelist[1] != 701 {
		t.Fatalf("whitelist = %v", resolution.TargetWhitelist)
	}
	if len(resolution.AllowedEquipmentTypes) != 2 || resolution.AllowedEquipmentTypes[0] != "[coat]" || resolution.AllowedEquipmentTypes[1] != "[weapon]" {
		t.Fatalf("allowed types = %v, want icon entry skipped", resolution.AllowedEquipmentTypes)
	}
	if len(resolution.UpgradeCounts) != 3 || resolution.UpgradeCounts[0] != 0 || resolution.UpgradeCounts[1] != 1 || resolution.UpgradeCounts[2] != 2 {
		t.Fatalf("upgrade counts = %v", resolution.UpgradeCounts)
	}
	if resolution.TargetKind != "equipment" || resolution.TargetEquipmentType != "[coat]" {
		t.Fatalf("target = %+v", resolution)
	}
	if resolution.CardPVFPath != "stackable/card/fire_card.stk" {
		t.Fatalf("card path = %q", resolution.CardPVFPath)
	}
}

func TestChooseCurrentEnchantCardIDPreservesRepeatedPVFWeights(t *testing.T) {
	values := []int64{9001, 9001, 9002}
	seen := make(map[int64]bool)
	for index := 0; index < 128; index++ {
		cardID, found, err := chooseCurrentEnchantCardID(values)
		if err != nil {
			t.Fatal(err)
		}
		if !found || (cardID != 9001 && cardID != 9002) {
			t.Fatalf("card id = %d found=%t", cardID, found)
		}
		seen[cardID] = true
	}
	if len(seen) != 2 {
		t.Fatalf("random candidates = %v, want both weighted cards", seen)
	}
}

func TestResolveCurrentEnchantBeadMetadataInvalidBeads(t *testing.T) {
	catalog, source := newEnchantBeadTestCatalog(t)
	tests := []struct {
		name string
		bead int64
	}{
		{name: "bead without card", bead: 8802},
		{name: "unknown bead", bead: 9999},
		{name: "equipment as bead", bead: 700},
		{name: "zero bead", bead: 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resolution, err := resolveCurrentEnchantBeadMetadata(catalog, source, tc.bead, 700)
			if err != nil {
				t.Fatalf("resolve error = %v", err)
			}
			if resolution.CardItemID != 0 {
				t.Fatalf("card id = %d, want 0 for invalid bead", resolution.CardItemID)
			}
		})
	}
}

func TestResolveCurrentEnchantBeadMetadataUnresolvedTarget(t *testing.T) {
	catalog, source := newEnchantBeadTestCatalog(t)
	resolution, err := resolveCurrentEnchantBeadMetadata(catalog, source, 8801, 9999)
	if err != nil {
		t.Fatalf("resolve error = %v", err)
	}
	if resolution.CardItemID != 9001 {
		t.Fatalf("card id = %d, want 9001", resolution.CardItemID)
	}
	if resolution.TargetKind != "" {
		t.Fatalf("target kind = %q, want empty for unresolved target", resolution.TargetKind)
	}
}

func TestResolveCurrentEnchantBeadMetadataRandomTitleEnchant(t *testing.T) {
	source := dungeonDropCatalogTestSource{
		"monster/monster.lst":        "",
		"stackable/stackable.lst":    "8803 `bead/random.stk`\n",
		"equipment/equipment.lst":    "702 `title/test.equ`\n",
		"stackable/bead/random.stk":  "[stackable type] `[auto enchant]`\n[random title enchant] 10001\n[string data] `[title name]`\n[bead limited usable item] 702\n",
		"equipment/title/test.equ":   "[equipment type] `[title name]`\n",
		"etc/randomtitleenchant.etc": "[index]\n10001 1 1 100 `[swordman]` 4 1\n[/index]",
	}
	catalog, err := newPVFDungeonDropCatalog(source)
	if err != nil {
		t.Fatal(err)
	}
	resolution, err := resolveCurrentEnchantBeadMetadata(catalog, source, 8803, 702)
	if err != nil {
		t.Fatal(err)
	}
	if resolution.CardItemID != 0 || resolution.RandomTitleEnchantConfigID != 10001 || resolution.RandomTitleEnchantRowID != 1 {
		t.Fatalf("resolution = %+v", resolution)
	}
	if resolution.TargetEquipmentType != "[title name]" || resolution.TargetKind != "equipment" {
		t.Fatalf("target = %+v", resolution)
	}
}
