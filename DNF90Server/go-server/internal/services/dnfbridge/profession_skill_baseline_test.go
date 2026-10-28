package dnfbridge

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	dnfprofession "longheng.io/server/internal/modules/dnf/profession"
	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
	dnfskill "longheng.io/server/internal/modules/dnf/skill"
	"longheng.io/server/internal/modules/dnf/skillcmd"
)

func TestProtectedProfessionSkillLevelsKeepFreeAwakeningAndExcludePurchasedPassives(t *testing.T) {
	profiles, catalog := cumulativeProfessionSkillResources(t)
	tests := []struct {
		name     string
		growType byte
		want     map[uint16]int
	}{
		{name: "base", growType: 0x00, want: map[uint16]int{10: 7}},
		{name: "class change", growType: 0x03, want: map[uint16]int{10: 7, 20: 1}},
		{name: "first awakening", growType: 0x13, want: map[uint16]int{10: 7, 20: 1, 83: 1}},
		{name: "second awakening", growType: 0x23, want: map[uint16]int{10: 7, 20: 1, 83: 1}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			levels, ok := protectedProfessionSkillLevels(profiles, catalog, 3, test.growType, 90)
			if !ok || !reflect.DeepEqual(levels, test.want) {
				t.Fatalf("protected levels=%v found=%t want=%v", levels, ok, test.want)
			}
		})
	}
}

func TestProtectedProfessionSkillLevelsExcludeClearFlapperOptionalToggle(t *testing.T) {
	profiles, catalog := cumulativeProfessionSkillResources(t)
	levels, ok := protectedProfessionSkillLevels(profiles, catalog, 3, 0x04, 90)
	if !ok {
		t.Fatal("witch protected profession levels unavailable")
	}
	if _, protected := levels[167]; protected {
		t.Fatalf("optional clear-flapper toggle entered permanent skill floor: %v", levels)
	}
	definition, found := catalog.Find(3, 167)
	if !found || !strings.EqualFold(definition.Path, "skill/mage/clearflapper.skl") || len(definition.Prerequisites) != 1 ||
		definition.Prerequisites[0].SkillID != 95 || definition.Prerequisites[0].Level != 1 || definition.LevelCost(0) != 0 {
		t.Fatalf("clear-flapper evidence definition=%+v found=%t", definition, found)
	}
}

func TestSkillResetRestoresCumulativeProfessionFloorInBothTrees(t *testing.T) {
	ctx := context.Background()
	profiles, catalog := cumulativeProfessionSkillResources(t)
	repositories := dnfrepomemory.NewMemoryGroup()
	character := dnfrepo.CharacterRecord{
		CharacterID: "77",
		AccountID:   "dnf:test",
		Job:         "3",
		Level:       90,
		Stats:       map[string]int64{"grow_type": 0x23},
	}
	if err := repositories.Character.Save(ctx, character); err != nil {
		t.Fatal(err)
	}
	points := dnfrepo.SkillPointState{TotalSP: 1000, RemainingSP: 100, TotalTP: 20, RemainingTP: 2, SyncedLevel: 90}
	record := dnfrepo.SkillRecord{
		CharacterID: character.CharacterID,
		Skills: map[int64]dnfrepo.SkillState{
			10:  {Level: 9, Enabled: true},
			83:  {Level: 12, Enabled: true},
			84:  {Level: 16, Enabled: true},
			99:  {Level: 5, Enabled: true},
			126: {Level: 6, Enabled: true},
		},
		Points: points,
		Layouts: map[int]dnfrepo.SkillLayout{
			0: {0: 99},
			1: {0: 99},
		},
	}
	dnfrepo.SetSkillTree(&record, 1, dnfrepo.SkillTreeState{
		Skills: map[int64]dnfrepo.SkillState{
			10:  {Level: 9, Enabled: true},
			83:  {Level: 12, Enabled: true},
			84:  {Level: 16, Enabled: true},
			99:  {Level: 5, Enabled: true},
			126: {Level: 6, Enabled: true},
		},
		Points: points,
	})
	if err := repositories.Skill.Save(ctx, record); err != nil {
		t.Fatal(err)
	}
	service := &Service{
		professionProfiles: profiles,
		skillCatalog:       catalog,
		initialSPTable:     map[int]int{90: 1000},
		initialTPTable:     map[int]int{90: 20},
	}
	resolvedCatalog, levels, baseline := service.skillOwnerRules(ctx, repositories, 77)
	want := map[uint16]int{10: 7, 20: 1, 83: 1}
	if resolvedCatalog != catalog || !reflect.DeepEqual(levels, want) || baseline == nil || baseline.RemainingSP != 1000 || baseline.RemainingTP != 20 {
		t.Fatalf("reset rules catalog=%p levels=%v baseline=%+v", resolvedCatalog, levels, baseline)
	}
	owner, err := skillcmd.NewOwner(repositories, skillcmd.OwnerOptions{
		Catalog:       resolvedCatalog,
		InitialLevels: levels,
		PointBaseline: baseline,
	})
	if err != nil {
		t.Fatal(err)
	}
	for tree := byte(0); tree < currentSkillInfoTreeCount; tree++ {
		if _, err := owner.ApplyReset(ctx, skillcmd.NewResetCommand(
			alignedcmd.Request{AccountID: "dnf:test", SelectedCharacterID: 77},
			skillcmd.SkillInitRequest{SkillTree: tree, Mode: 0},
		)); err != nil {
			t.Fatalf("reset tree %d: %v", tree, err)
		}
	}
	saved, found, err := repositories.Skill.Load(ctx, character.CharacterID)
	if err != nil || !found {
		t.Fatalf("load reset record found=%t err=%v", found, err)
	}
	for tree := 0; tree < currentSkillInfoTreeCount; tree++ {
		state, ok := dnfrepo.SkillTree(saved, tree)
		if !ok || len(state.Skills) != len(want) {
			t.Fatalf("tree %d state=%+v found=%t", tree, state, ok)
		}
		for skillID, level := range want {
			if got := state.Skills[int64(skillID)]; got.Level != level || !got.Enabled {
				t.Fatalf("tree %d skill %d=%+v want level=%d enabled", tree, skillID, got, level)
			}
		}
		for _, purchasedSkillID := range []int64{84, 99, 126} {
			if _, stale := state.Skills[purchasedSkillID]; stale {
				t.Fatalf("tree %d retained purchased skill %d: %+v", tree, purchasedSkillID, state.Skills)
			}
		}
	}
}

// TestSkillOwnerRulesBaselineIsTheOrdinaryPVFPointTarget pins the contract that
// makes +SP/+TP books work at all, and guards the regression that folding the
// book bonus into this baseline caused.
//
// inventory.useTPBook receives this value as alignedcmd.Request.SkillPointBaseline
// and adds the persisted bonus_sp/bonus_tp itself (skillPointBookTarget does
// `baseline.TotalSP += bonusSP`), so a baseline that ALREADY carried the bonus
// would count the accumulated bonus twice on every book use and hand out
// quadratic free SP (N books of +d grant d*N*(N-1)/2 extra).
//
// The skill plan mutations that do need the bonus - op29 buy_skill and op491
// skill reset - add it themselves in skillcmd.skillPointTargetForCharacter.
func TestSkillOwnerRulesBaselineIsTheOrdinaryPVFPointTarget(t *testing.T) {
	ctx := context.Background()
	profiles, catalog := cumulativeProfessionSkillResources(t)
	repositories := dnfrepomemory.NewMemoryGroup()
	character := dnfrepo.CharacterRecord{
		CharacterID: "77",
		AccountID:   "dnf:test",
		Job:         "3",
		Level:       90,
		Stats:       map[string]int64{"grow_type": 0x23, "bonus_sp": 180, "bonus_tp": 4},
	}
	if err := repositories.Character.Save(ctx, character); err != nil {
		t.Fatal(err)
	}
	service := &Service{
		professionProfiles: profiles,
		skillCatalog:       catalog,
		initialSPTable:     map[int]int{90: 1000},
		initialTPTable:     map[int]int{90: 20},
	}
	resolvedCatalog, _, baseline := service.skillOwnerRules(ctx, repositories, 77)
	if resolvedCatalog != catalog || baseline == nil {
		t.Fatalf("skill owner rules catalog=%p baseline=%+v", resolvedCatalog, baseline)
	}
	if baseline.TotalSP != 1000 || baseline.RemainingSP != 1000 ||
		baseline.TotalTP != 20 || baseline.RemainingTP != 20 || baseline.SyncedLevel != 90 {
		t.Fatalf("baseline = %+v, want the ordinary PVF SP/TP tables 1000/20 with NO book bonus; "+
			"a bonus-inclusive baseline double-counts bonus_sp=180/bonus_tp=4 on every book use", baseline)
	}
	// The bonus belongs to the login/GM/book target instead. Assert the two
	// differ by exactly the persisted bonus so the split stays visible.
	target, err := service.currentPVFSkillPointTarget(ctx, character)
	if err != nil {
		t.Fatal(err)
	}
	if target.TotalSP != 1180 || target.TotalTP != 24 {
		t.Fatalf("shared PVF+bonus target = %+v, want 1180/24", target)
	}
	if target.TotalSP-baseline.TotalSP != int(character.Stats["bonus_sp"]) ||
		target.TotalTP-baseline.TotalTP != int(character.Stats["bonus_tp"]) {
		t.Fatalf("baseline %+v and target %+v must differ by exactly bonus_sp=%d/bonus_tp=%d",
			*baseline, target, character.Stats["bonus_sp"], character.Stats["bonus_tp"])
	}
}

func cumulativeProfessionSkillResources(t *testing.T) (*dnfprofession.Profiles, *dnfskill.Table) {
	t.Helper()
	source := finishBridgePVFSource{
		dnfprofession.DefaultCharacterList: "3 `Mage/Mage.chr`\n",
		"character/Mage/Mage.chr": "[initial value]\n[skill]\n10 7\n[/skill]\n" +
			"[growtype 4]\n[skill]\n20 1\n[/skill]\n" +
			"[awakening 1]\n[awakening skill]\n83 1\n84 1\n[/awakening skill]\n" +
			"[awakening 2]\n[awakening skill]\n[/awakening skill]\n",
		dnfskill.DefaultList:          "3 `MageSkill.lst`\n",
		"skill/MageSkill.lst":         "10 `Mage/base.skl` 20 `Mage/class.skl` 83 `Mage/bellatrix.skl` 84 `Mage/combomastery.skl` 95 `Mage/flapper.skl` 99 `Mage/purchased.skl` 126 `Mage/multirole.skl` 167 `Mage/clearflapper.skl`\n",
		"skill/Mage/base.skl":         "[name]\n`base`\n[type]\n`[passive]`\n",
		"skill/Mage/class.skl":        "[name]\n`class`\n[type]\n`[active]`\n",
		"skill/Mage/bellatrix.skl":    "[name]\n`变身贝亚娜`\n[type]\n`[active]`\n[purchase cost]\n0\n[/purchase cost]\n",
		"skill/Mage/combomastery.skl": "[name]\n`斗神意志`\n[type]\n`[passive]`\n[purchase cost]\n45\n[/purchase cost]\n",
		"skill/Mage/flapper.skl":      "[name]\n`flapper`\n[type]\n`[active]`\n",
		"skill/Mage/purchased.skl":    "[name]\n`purchased`\n[type]\n`[active]`\n",
		"skill/Mage/multirole.skl":    "[name]\n`战灵潜能`\n[type]\n`[passive]`\n[purchase cost]\n80\n[/purchase cost]\n",
		"skill/Mage/clearflapper.skl": `[name]
` + "`苍蝇拍 : 魔法禁锢`" + `
[purchase cost]
0
[/purchase cost]
[pre required skill]
95 1
[/pre required skill]
[required level]
30
[skill class]
3
[maximum level]
1
[growtype maximum level]
0 0 0 0 1 0
[/growtype maximum level]
[skill fitness growtype]
4
[/skill fitness growtype]
`,
	}
	index, err := dnfpvf.Build(context.Background(), source, dnfpvf.BuildOptions{Lists: []string{dnfskill.DefaultList}})
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := dnfskill.Load(context.Background(), index, dnfskill.Options{})
	if err != nil {
		t.Fatal(err)
	}
	profiles, err := dnfprofession.LoadProfiles(context.Background(), source, catalog)
	if err != nil {
		t.Fatal(err)
	}
	return profiles, catalog
}
