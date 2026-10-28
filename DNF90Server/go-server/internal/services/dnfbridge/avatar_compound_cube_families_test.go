package dnfbridge

import (
	"context"
	"os"
	"testing"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

// TestCurrentAvatarCompoundAcceptsEveryCubeFamily pins the acceptance rule for
// the consumed 装扮合成器. Only the three-tier tail used to be accepted, which
// rejected every 90-level and event cube with op99 {0,0x16} — the client shows
// that as "this cube cannot be used" (live 2026-10-04: 升级的远古装扮合成器
// 10301984).
func TestCurrentAvatarCompoundAcceptsEveryCubeFamily(t *testing.T) {
	source := newAvatarCompoundPVFTestSource(0, 0)
	config, err := loadCurrentAvatarCompoundConfig(source, 0)
	if err != nil {
		t.Fatal(err)
	}
	// The fixture's job file carries one [common bind cube] block whose
	// [material] is 999.
	if !config.BlockMaterials[999] {
		t.Fatalf("block material 999 was not collected: %+v", config.BlockMaterials)
	}
	if config.UpperBonus != 200 || config.MasterBonus != 400 {
		t.Fatalf("bonuses=(%d,%d)", config.UpperBonus, config.MasterBonus)
	}

	catalog := &pvfDungeonDropCatalog{
		source: source,
		itemCache: map[uint32]dungeonDropItemDefinition{
			999:      {ItemID: 999, Kind: dungeonDropItemStackable, Name: "升级的远古装扮合成器"},
			10007495: {ItemID: 10007495, Kind: dungeonDropItemStackable, Name: "钻石装扮合成器"},
			2683630:  {ItemID: 2683630, Kind: dungeonDropItemStackable, Name: "追忆之装扮合成器"},
			10001874: {ItemID: 10001874, Kind: dungeonDropItemStackable, Name: "升级的远古装扮合成器"},
			10007246: {ItemID: 10007246, Kind: dungeonDropItemStackable, Name: "梦想装扮合成器 (天光云影套)"},
			10007247: {ItemID: 10007247, Kind: dungeonDropItemStackable, Name: "梦幻装扮合成器 (天光云影套)"},
			2681674:  {ItemID: 2681674, Kind: dungeonDropItemStackable, Name: "装扮合成器礼包"},
		},
	}
	for _, test := range []struct {
		name     string
		itemID   uint32
		want     int64
		accepted bool
	}{
		{name: "canonical ordinary 21", itemID: 21, want: 0, accepted: true},
		{name: "canonical golden 899", itemID: 899, want: 200, accepted: true},
		{name: "canonical diamond 490702265", itemID: 490702265, want: 400, accepted: true},
		{name: "bind cube block material", itemID: 999, want: 0, accepted: true},
		{name: "ancient by name", itemID: 10001874, want: 0, accepted: true},
		{name: "diamond event re-issue", itemID: 10007495, want: 400, accepted: true},
		{name: "memory re-issue", itemID: 2683630, want: 200, accepted: true},
		{name: "absolute dream cube stays out", itemID: 10007246, accepted: false},
		{name: "absolute fantasy cube stays out", itemID: 10007247, accepted: false},
		{name: "gift package stays out", itemID: 2681674, accepted: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			bonus, accepted := currentAvatarCompoundCubeBonus(config, catalog, test.itemID)
			if accepted != test.accepted {
				t.Fatalf("item=%d accepted=%t want=%t", test.itemID, accepted, test.accepted)
			}
			if accepted && bonus != test.want {
				t.Fatalf("item=%d bonus=%d want=%d", test.itemID, bonus, test.want)
			}
		})
	}
}

// TestCurrentAvatarCompoundCommitsWithBindCubeBlockMaterial runs the whole
// commit path with a [common bind cube] block's own material (the 远古 family
// shape) instead of the canonical 21: acceptance, roll, three consumes and one
// output.
func TestCurrentAvatarCompoundCommitsWithBindCubeBlockMaterial(t *testing.T) {
	ctx := context.Background()
	source := newAvatarCompoundPVFTestSource(0, 0)
	source["equipment/source1.equ"] = "[equipment type] `[coat avatar]`\n[grade] 1\n"
	source["equipment/source2.equ"] = "[equipment type] `[coat avatar]`\n[grade] 2\n"
	source["equipment/output.equ"] = "[equipment type] `[coat avatar]`\n[grade] 2\n"
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{CharacterID: "19", AccountID: "dnf:1", Job: "0"}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{CharacterID: "19", Slots: map[string]dnfrepo.ItemStack{
		"0:117": {ItemID: 999, Count: 1},
		"1:21":  {ItemID: 3001, Count: 1},
		"1:22":  {ItemID: 3002, Count: 1},
	}}); err != nil {
		t.Fatal(err)
	}
	service := &Service{options: options{accountPrefix: defaultAccountPrefix}, repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true }}
	session := &gameSession{accountID: "dnf:1", selectedCharacterID: 19}
	catalog := &pvfDungeonDropCatalog{
		source: source,
		itemCache: map[uint32]dungeonDropItemDefinition{
			999:  {ItemID: 999, Kind: dungeonDropItemStackable, PVFPath: "stackable/ancient.stk", Name: "升级的远古装扮合成器"},
			3001: {ItemID: 3001, Kind: dungeonDropItemEquipment, PVFPath: "equipment/source1.equ", EquipmentType: "[coat avatar]", Grade: 1},
			3002: {ItemID: 3002, Kind: dungeonDropItemEquipment, PVFPath: "equipment/source2.equ", EquipmentType: "[coat avatar]", Grade: 2},
			2004: {ItemID: 2004, Kind: dungeonDropItemEquipment, PVFPath: "equipment/output.equ", EquipmentType: "[coat avatar]", Grade: 2},
		},
	}
	request := currentAvatarCompoundRequest{
		ConsumeSlot: 117, Slot1: 21, ExpectedItemID1: 3001, Slot2: 22, ExpectedItemID2: 3002,
		RequestedItemID: 2004, AbilityNo: 2, MaterialClass: 13,
	}
	result, err := service.commitCurrentAvatarCompound(ctx, session, catalog, request)
	if err != nil {
		t.Fatalf("ancient cube was refused: %v", err)
	}
	if len(result.NewItemIDs) != 1 || result.NewItemIDs[0] != 2004 || len(result.NewSlots) != 1 || result.NewSlots[0] != 21 {
		t.Fatalf("result=%+v", result)
	}
	inventory, _, _ := repositories.Inventory.Load(ctx, "19")
	if _, found := inventory.Slots["0:117"]; found {
		t.Fatalf("cube was not consumed: %+v", inventory.Slots["0:117"])
	}
	if output := inventory.Slots["1:21"]; output.ItemID != 2004 || output.Count != 1 {
		t.Fatalf("output=%+v", output)
	}
}

// TestRealPVFAvatarCompoundCollectsBindCubeBlocks reads the deployed Script.pvf
// and proves the 90-level cube the player actually owns is collectable from the
// job file's [common bind cube] block.
func TestRealPVFAvatarCompoundCollectsBindCubeBlocks(t *testing.T) {
	pvfPath := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if pvfPath == "" {
		pvfPath = realLukeRewardPVFPath()
	}
	archive, err := platformpvf.Open(pvfPath)
	if err != nil {
		t.Skipf("runtime Script.pvf unavailable: %v", err)
	}
	// job 0 = 鬼剑士 (the reporting character is 剑神).
	config, err := loadCurrentAvatarCompoundConfig(archive, 0)
	if err != nil {
		t.Fatalf("load compound-avatar config: %v", err)
	}
	if len(config.BlockMaterials) == 0 {
		t.Fatalf("no [common bind cube] materials collected from %s", config.PVFPath)
	}
	if !config.BlockMaterials[10301984] {
		t.Fatalf("升级的远古装扮合成器 10301984 missing from %+v", config.BlockMaterials)
	}
	t.Logf("job=0 blocks=%v canonical=(%d,%d,%d)", config.BlockMaterials,
		config.NormalMaterialID, config.UpperMaterialID, config.MasterMaterialID)
}
