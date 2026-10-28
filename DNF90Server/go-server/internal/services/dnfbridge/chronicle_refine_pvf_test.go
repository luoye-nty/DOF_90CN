package dnfbridge

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"os"
	"strings"
	"testing"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

func TestResolveCurrentChronicleRefineMetadataUsesImportedCheckAndSkill(t *testing.T) {
	source := bridgePVFSource{
		"monster/monster.lst":                     "",
		"stackable/stackable.lst":                 "3037 `cube.stk`\n3311 `fragment.stk`\n10090760 `10090001/10090760.stk`\n",
		"equipment/equipment.lst":                 "116010135 `character/gunblader/weapon/sblade/116010135.equ`\n",
		"stackable/cube.stk":                      "[stackable type] `[material]`\n[stack limit] 999\n",
		"stackable/fragment.stk":                  "[stackable type] `[material]`\n[stack limit] 999\n",
		"equipment/pricetable.tbl":                "[rate]\n1000 150 30\n",
		"etc/disjoint.etc":                        "[cube index]\n`[no element]` 3037\n[/cube index]\n[cube creation const]\n150 1 1 1 1 1 1\n[/cube creation const]\n[additional result]\n0 0 0 0 0 0\n[/additional result]\n[additional result const]\n0 0 0 0 0 0\n[/additional result const]\n[additional result expand]\n0 0 0 0 0 0\n[/additional result expand]\n[additional result expand const]\n0 0 0 0 0 0\n[/additional result expand const]\n[avatar disjoint info]\n[fighter]\n2559000 2559003 2559006 2559009 2559006 2559001 2559004 2559007 2559010 2559007 2559002 2559005 2559008 2559011 2559014\n[/fighter]\n[/avatar disjoint info]\n",
		"etc/emblemcompound.etc":                  "[emblem compound info]\n1 2 3037\n[/emblem compound info]\n",
		"etc/avatardisjoint/emblemlistinfo_0.etc": "[result info]\n1 3037 100 1 0\n[/result info]\n",
		"etc/avatardisjoint/emblemlistinfo_1.etc": "[result info]\n1 3037 100 1 0\n[/result info]\n",
		"etc/avatardisjoint/emblemlistinfo_2.etc": "[min max value]\n50 69\n[result info]\n1 3037 100 1 0\n[/result info]\n",
		"stackable/10090001/10090760.stk": `
[stackable type]
	` + "`[etc]`" + `
[import script]
	` + "`DimensionForce_G.stk`" + `
[type]
	2
[probability]
	100 100
[need material]
	3311 1
`,
		"stackable/DimensionForce_G.stk": `
[3choro enchant]
[check]
	15 2 ` + "`weapon`" + `
[skill]
	11 ` + "`[gun blader]`" + ` 123 456
[/skill]
[/check]
[check]
	5 2 ` + "`weapon`" + `
[skill]
	0 ` + "`[at gunner]`" + ` 123 456
[/skill]
[/check]
[/3choro enchant]
`,
		"equipment/character/gunblader/weapon/sblade/116010135.equ": `
[rarity]
	5
[durability]
	45
[minimum level]
	70
[value]
	300
[equipment type]
	` + "`[weapon]`" + `
[character item check]
	15 2 ` + "`weapon`" + ` 5 2 ` + "`weapon`" + `
[/character item check]
`,
	}
	catalog, err := newPVFDungeonDropCatalog(source)
	if err != nil {
		t.Fatal(err)
	}
	resolution, err := resolveCurrentChronicleRefineMetadata(catalog, source, 10090760, 116010135, 15, 2, 11)
	if err != nil {
		t.Fatal(err)
	}
	if !resolution.Matched || resolution.AuraType != 2 || resolution.CanonicalAuraOptionID != 1256 ||
		resolution.Probabilities != [2]byte{100, 100} || resolution.TargetEquipmentTypeCode != 12 ||
		resolution.TargetDurability != 45 || resolution.FailureFragmentItemID != 3311 ||
		resolution.FailureFragmentItem.ItemID != 3311 || resolution.FailureCubeItem.ItemID != 3037 ||
		resolution.FailureCubeCount != 2 {
		t.Fatalf("resolution = %+v", resolution)
	}
	if strings.ToLower(resolution.ImportedScriptPVFPath) != "stackable/dimensionforce_g.stk" {
		t.Fatalf("imported path = %q", resolution.ImportedScriptPVFPath)
	}

	rejected, err := resolveCurrentChronicleRefineMetadata(catalog, source, 10090760, 116010135, 15, 2, 10)
	if err != nil || rejected.Matched {
		t.Fatalf("unsupported option resolution = %+v err=%v", rejected, err)
	}
	rejected, err = resolveCurrentChronicleRefineMetadata(catalog, source, 10090760, 116010135, 15, 1, 11)
	if err != nil || rejected.Matched {
		t.Fatalf("wrong grow resolution = %+v err=%v", rejected, err)
	}

	femaleGunner, err := resolveCurrentChronicleRefineMetadata(catalog, source, 10090760, 116010135, 5, 2, 0)
	if err != nil || !femaleGunner.Matched {
		t.Fatalf("shared female-gunner target resolution = %+v err=%v", femaleGunner, err)
	}
}

func TestRealPVFChronicleRefineObservedGunnerWeapons(t *testing.T) {
	pvfPath := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE to verify current chronicle aura data")
	}
	archive, err := platformpvf.Open(pvfPath)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := newPVFDungeonDropCatalog(archive)
	if err != nil {
		t.Fatal(err)
	}
	for _, materialID := range []int64{1256, 10090760} {
		resolution, resolveErr := resolveCurrentChronicleRefineMetadata(catalog, archive, materialID, 116010135, 15, 2, 11)
		if resolveErr != nil {
			t.Fatalf("material %d: %v", materialID, resolveErr)
		}
		if !resolution.Matched || resolution.AuraType != 2 || resolution.CanonicalAuraOptionID != 1256 ||
			resolution.TargetEquipmentTypeCode != 12 || resolution.FailureFragmentItemID != 3311 ||
			resolution.FailureFragmentItem.ItemID != 3311 || resolution.FailureCubeItem.ItemID != 3037 ||
			resolution.FailureCubeCount <= 0 {
			t.Fatalf("material %d resolution = %+v", materialID, resolution)
		}
		t.Logf("material=%d material_path=%s import=%s probabilities=%v fragment=%d cube=(%d,%d) target=%s",
			materialID, resolution.MaterialPVFPath, resolution.ImportedScriptPVFPath,
			resolution.Probabilities, resolution.FailureFragmentItemID,
			resolution.FailureCubeItem.ItemID, resolution.FailureCubeCount, resolution.TargetPVFPath)
	}

	femaleTargets := []struct {
		itemID int64
		code   byte
		path   string
	}{
		{itemID: 625000, code: 12, path: "equipment/character/gunner/weapon/revolver/rev_3choro1.equ"},
		{itemID: 115000, code: 14, path: "equipment/character/common/jacket/leather/coat_lt_3choro1.equ"},
		{itemID: 445072, code: 22, path: "equipment/character/common/support/support_3choro73.equ"},
	}
	for _, target := range femaleTargets {
		for _, materialID := range []int64{1254, 1255, 1256} {
			resolution, resolveErr := resolveCurrentChronicleRefineMetadata(catalog, archive, materialID, target.itemID, 5, 1, 0)
			if resolveErr != nil {
				t.Fatalf("female gunner target %d material %d: %v", target.itemID, materialID, resolveErr)
			}
			if !resolution.Matched || resolution.AuraType != byte(materialID-currentChronicleCanonicalAuraBase) ||
				resolution.CanonicalAuraOptionID != int32(materialID) || resolution.TargetEquipmentTypeCode != target.code ||
				resolution.TargetPVFPath != target.path {
				t.Fatalf("female gunner target %d material %d resolution = %+v", target.itemID, materialID, resolution)
			}
		}
	}
}

func TestCurrentChronicleEquipmentTypeUsesClientDefinitionTypes(t *testing.T) {
	tests := map[string]byte{
		"weapon":      12,
		"coat":        14,
		"shoulder":    15,
		"pants":       16,
		"shoes":       17,
		"waist":       18,
		"amulet":      19,
		"wrist":       20,
		"ring":        21,
		"support":     22,
		"magic stone": 23,
		"earring":     25,
	}
	for raw, want := range tests {
		t.Run(raw, func(t *testing.T) {
			name, got, ok := currentChronicleEquipmentType(raw)
			if !ok || name != raw || got != want {
				t.Fatalf("currentChronicleEquipmentType(%q) = (%q,%d,%t), want (%q,%d,true)", raw, name, got, ok, raw, want)
			}
		})
	}
	for _, unsupported := range []string{"title", "support weapon", "unknown"} {
		if _, _, ok := currentChronicleEquipmentType(unsupported); ok {
			t.Fatalf("unsupported equipment type %q accepted", unsupported)
		}
	}
}

func TestChronicleOptionVectorSurvivesCurrentItemProjections(t *testing.T) {
	raw := make([]byte, currentItemListEntryWireSize)
	raw[0x27] = 1
	binary.LittleEndian.PutUint32(raw[0x28:0x2C], 1256)
	raw[0x30] = 15
	raw[0x32] = 2
	raw[0x34] = 12
	raw[0x36] = 11
	vector := append([]byte(nil), raw[0x27:0x38]...)

	stack := dnfrepo.ItemStack{
		ItemID:   116010135,
		Count:    1,
		RawEntry: append([]byte(nil), raw...),
		Extra: map[string]string{
			"item_kind":      "equipment",
			"middle_data_1a": hex.EncodeToString(vector),
		},
	}
	bag := currentItemListEntryFromStack(dnfrepo.MainInventoryListType, 29, stack)
	if got := bag.data[0x27:0x38]; !bytes.Equal(got, vector) {
		t.Fatalf("list0 current-item projection vector=%x want=%x", got, vector)
	}

	equipped := dnfrepo.EquipmentEntry{
		SlotIndex: 11,
		ItemID:    stack.ItemID,
		RawEntry:  append([]byte(nil), stack.RawEntry...),
		Extra:     stack.Extra,
	}
	login, ok := currentItemListEntryFromEquipment(equipped)
	if !ok {
		t.Fatal("list3 current-item projection rejected Chronicle weapon")
	}
	if got := login.data[0x27:0x38]; !bytes.Equal(got, vector) {
		t.Fatalf("list3 current-item projection vector=%x want=%x", got, vector)
	}
	update := currentEquipmentUpdateEntryFromEquipment(equipped)
	if got := update.data[0x1A:0x2B]; !bytes.Equal(got, vector) {
		t.Fatalf("equipment-update projection vector=%x want=%x", got, vector)
	}
}
