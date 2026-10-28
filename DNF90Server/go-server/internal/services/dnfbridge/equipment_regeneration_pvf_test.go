package dnfbridge

import (
	"os"
	"testing"

	platformpvf "longheng.io/server/internal/platform/pvf"
)

func TestEquipmentRegenerationPVFResolvesDirectedArmorAndMixedMaterialOwners(t *testing.T) {
	source := dungeonDropCatalogTestSource{
		currentEquipmentRegenerationPath: `[choose part]
2 ` + "`防具` `cl shoes` `lt shoulder`" + `
[/choose part]
[compound need material for specific]
6 2 10099775 2 3167 100
[/compound need material for specific]
[compound specific apply cost by level]
6 80 3.00 6 90 4.00
[/compound specific apply cost by level]
`,
		currentEquipmentRegenerationOverallPath: `[regen level limit]
6 85 100000 0
[/regen level limit]
`,
		"equipment/source.equ": `[rarity] 6
[minimum level] 85
[creation rate] 0
[item group name] ` + "`cl shoes`" + `
[attach type] ` + "`[sealing]`" + `
[equipment type] ` + "`[shoes]`" + `
[durability] 18
`,
		"equipment/result.equ": `[rarity] 6
[minimum level] 85
[creation rate] 0
[item group name] ` + "`cl shoes`" + `
[attach type] ` + "`[sealing]`" + `
[equipment type] ` + "`[shoes]`" + `
[durability] 22
`,
		// A different group inside the same [choose part] is a legitimate
		// candidate: part 2 (防具) spans all 25 armour groups, so a 布甲鞋 may
		// regenerate into a 轻甲护肩.
		"equipment/othergroup.equ": `[rarity] 6
[minimum level] 85
[creation rate] 0
[item group name] ` + "`lt shoulder`" + `
[attach type] ` + "`[sealing]`" + `
[equipment type] ` + "`[shoulder]`" + `
[durability] 20
`,
		// A group that belongs to another part must never be drawn.
		"equipment/otherpart.equ": `[rarity] 6
[minimum level] 85
[creation rate] 0
[item group name] ` + "`amulet`" + `
[attach type] ` + "`[sealing]`" + `
[equipment type] ` + "`[amulet]`" + `
[durability] 20
`,
	}
	items := &pvfDungeonDropCatalog{
		source: source,
		itemRefs: map[uint32]dungeonDropItemReference{
			1001: {kind: dungeonDropItemEquipment, path: "source.equ"},
			1002: {kind: dungeonDropItemEquipment, path: "result.equ"},
			1003: {kind: dungeonDropItemEquipment, path: "othergroup.equ"},
			1004: {kind: dungeonDropItemEquipment, path: "otherpart.equ"},
		},
	}
	catalog, err := loadCurrentEquipmentRegenerationCatalog(items)
	if err != nil {
		t.Fatal(err)
	}
	resolution, err := catalog.resolve(1001, 0, 2)
	if err != nil {
		t.Fatal(err)
	}
	// The pool is every group the part lists -- 1001, 1002 and 1003 -- and
	// never the amulet from part 3. Restricting it to the source's own group
	// left one candidate and handed the player back the item they fed in.
	if !resolution.Eligible || resolution.SourceItemID != 1001 || resolution.CandidateCount != 3 {
		t.Fatalf("resolution=%+v", resolution)
	}
	switch resolution.ResultItemID {
	case 1001, 1002, 1003:
	default:
		t.Fatalf("result outside the part pool: %+v", resolution)
	}
	if len(resolution.Materials) != 2 || resolution.Materials[0].ItemID != 10099775 || resolution.Materials[0].Count != 6 || !resolution.Materials[0].AccountShared ||
		resolution.Materials[1].ItemID != 3167 || resolution.Materials[1].Count != 300 || resolution.Materials[1].AccountShared {
		t.Fatalf("materials=%+v", resolution.Materials)
	}
	if rejected, err := catalog.resolve(1001, 0, 3); err != nil || rejected.Eligible {
		t.Fatalf("wrong-part resolution=%+v err=%v", rejected, err)
	}
}

func TestEquipmentRegenerationRealPVFReportedEightLeggedSteed(t *testing.T) {
	pvfPath := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE to verify op407 against the active PVF")
	}
	archive, err := platformpvf.Open(pvfPath)
	if err != nil {
		t.Fatal(err)
	}
	items, err := newPVFDungeonDropCatalog(archive)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := loadCurrentEquipmentRegenerationCatalog(items)
	if err != nil {
		t.Fatal(err)
	}
	resolution, err := catalog.resolve(100250415, 0, 2)
	if err != nil {
		t.Fatal(err)
	}
	if !resolution.Eligible || resolution.SourceItemID != 100250415 || resolution.ResultItemID <= 0 || resolution.ResultEquipmentType == "" || resolution.CandidateCount == 0 {
		t.Fatalf("resolution=%+v", resolution)
	}
	if len(resolution.Materials) != 2 || resolution.Materials[0].ItemID != 10099775 || resolution.Materials[0].Count != 6 || !resolution.Materials[0].AccountShared ||
		resolution.Materials[1].ItemID != 3167 || resolution.Materials[1].Count != 300 || resolution.Materials[1].AccountShared {
		t.Fatalf("materials=%+v", resolution.Materials)
	}

	// 灭战者 (rarity 6, [minimum level] 90, [item group name] `club`) is the
	// live report: the active PVF holds exactly one [sealing] piece per group
	// in that band, so filtering the pool down to the source's own group left
	// one candidate and the player always got their own weapon back. Part 1
	// (武器) lists 37 families, so the pool has to be far larger than one.
	weapon, err := catalog.resolve(101020665, 0, 1)
	if err != nil {
		t.Fatal(err)
	}
	if !weapon.Eligible || weapon.CandidateCount < 10 {
		t.Fatalf("weapon pool collapsed: %+v", weapon)
	}
}
