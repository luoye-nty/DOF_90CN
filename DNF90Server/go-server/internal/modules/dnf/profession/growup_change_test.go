package profession

import (
	"context"
	"fmt"
	"testing"
	"time"

	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
	dnfskill "longheng.io/server/internal/modules/dnf/skill"
)

func TestResolveGrowupChangeGoldCost(t *testing.T) {
	cases := []struct {
		name        string
		changeCount int
		wantCost    int64
	}{
		{name: "first-free", changeCount: 0, wantCost: 0},
		{name: "second-100113", changeCount: 1, wantCost: 100113},
		{name: "third-133484", changeCount: 2, wantCost: 133484},
		{name: "penultimate-333713", changeCount: 8, wantCost: 333713},
		{name: "cap-index-9", changeCount: 9, wantCost: 1000000},
		{name: "cap-beyond-table", changeCount: 15, wantCost: 1000000},
		{name: "negative-clamps-zero", changeCount: -3, wantCost: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ResolveGrowupChangeGoldCost(tc.changeCount); got != tc.wantCost {
				t.Fatalf("ResolveGrowupChangeGoldCost(%d) = %d want %d", tc.changeCount, got, tc.wantCost)
			}
		})
	}
}

func TestApplyGrowupChangeSuccessAndDeduction(t *testing.T) {
	ctx := context.Background()
	profiles, catalog := growupChangeProfessionResources(t)
	repos := growupChangeSeed(t, ctx, func(r *dnfrepo.CharacterRecord) {
		r.Level = 20
		r.Stats = map[string]int64{"gold": 200000, "grow_type": 1}
	})

	result, err := ApplyGrowupChange(ctx, repos, growupChangeInput(2))
	if err != nil {
		t.Fatal(err)
	}
	if !result.Success || result.ResultCode != GrowupChangeAckResultCodeSuccess {
		t.Fatalf("result=%+v", result)
	}
	if result.NewGrowType != 2 || result.PreviousChangeCount != 0 || result.NewChangeCount != 1 {
		t.Fatalf("transition=%+v", result)
	}
	if result.GoldCost != 0 || result.UpdatedGold != 200000 {
		t.Fatalf("first change should be free: result=%+v", result)
	}
	persisted, ok, err := repos.Character.Load(ctx, "19")
	if err != nil || !ok || persisted.Stats["grow_type"] != 2 || persisted.Stats["growup_change_count"] != 1 {
		t.Fatalf("persisted grow state ok=%t err=%v stats=%+v", ok, err, persisted.Stats)
	}
	if profiles == nil || catalog == nil {
		t.Fatal("profession resources unexpectedly nil")
	}

	// 第二次转职收费 100113。
	result, err = ApplyGrowupChange(ctx, repos, growupChangeInput(1))
	if err != nil {
		t.Fatal(err)
	}
	if !result.Success || result.NewChangeCount != 2 || result.GoldCost != 100113 || result.UpdatedGold != 200000-100113 {
		t.Fatalf("second change result=%+v", result)
	}
}

func TestApplyGrowupChangeRejectsByState(t *testing.T) {
	ctx := context.Background()
	profiles, catalog := growupChangeProfessionResources(t)
	if profiles == nil || catalog == nil {
		t.Fatal("profession resources unexpectedly nil")
	}
	cases := []struct {
		name    string
		mutate  func(r *dnfrepo.CharacterRecord)
		target  byte
		wantAck int
	}{
		{name: "level-below-min", mutate: func(r *dnfrepo.CharacterRecord) { r.Level = 14 }, target: 2, wantAck: GrowupChangeAckResultCodeInvalidState},
		{name: "level-above-max", mutate: func(r *dnfrepo.CharacterRecord) { r.Level = 50 }, target: 2, wantAck: GrowupChangeAckResultCodeInvalidState},
		{name: "not-transferred", mutate: func(r *dnfrepo.CharacterRecord) { r.Stats = map[string]int64{"gold": 200000, "grow_type": 0} }, target: 2, wantAck: GrowupChangeAckResultCodeInvalidState},
		{name: "already-awakened", mutate: func(r *dnfrepo.CharacterRecord) { r.Stats = map[string]int64{"gold": 200000, "grow_type": 0x11} }, target: 2, wantAck: GrowupChangeAckResultCodeInvalidState},
		{name: "target-unchanged", mutate: func(r *dnfrepo.CharacterRecord) { r.Stats = map[string]int64{"gold": 200000, "grow_type": 1} }, target: 1, wantAck: GrowupChangeAckResultCodeInvalidState},
		{name: "target-unsupported", mutate: func(r *dnfrepo.CharacterRecord) { r.Stats = map[string]int64{"gold": 200000, "grow_type": 1} }, target: 5, wantAck: GrowupChangeAckResultCodeInvalidState},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repos := growupChangeSeed(t, ctx, tc.mutate)
			result, err := ApplyGrowupChange(ctx, repos, growupChangeInput(tc.target))
			if err != nil {
				t.Fatal(err)
			}
			if result.Success || result.ResultCode != tc.wantAck {
				t.Fatalf("result=%+v", result)
			}
			persisted, ok, err := repos.Character.Load(ctx, "19")
			if err != nil || !ok {
				t.Fatalf("load persisted ok=%t err=%v", ok, err)
			}
			if persisted.Stats["growup_change_count"] != 0 {
				t.Fatalf("rejected change advanced change count stats=%+v", persisted.Stats)
			}
		})
	}
}

func TestApplyGrowupChangeRejectsInsufficientGold(t *testing.T) {
	ctx := context.Background()
	repos := growupChangeSeed(t, ctx, func(r *dnfrepo.CharacterRecord) {
		r.Level = 20
		r.Stats = map[string]int64{"gold": 50000, "grow_type": 1, "growup_change_count": 1}
	})
	result, err := ApplyGrowupChange(ctx, repos, growupChangeInput(2))
	if err != nil {
		t.Fatal(err)
	}
	if result.Success || result.ResultCode != GrowupChangeAckResultCodeInsufficientGold {
		t.Fatalf("result=%+v", result)
	}
	if result.GoldCost != 100113 {
		t.Fatalf("insufficient-gold cost=%+v", result)
	}
}

func growupChangeSeed(t *testing.T, ctx context.Context, mutate func(r *dnfrepo.CharacterRecord)) dnfrepo.Group {
	t.Helper()
	repos := dnfrepomemory.NewMemoryGroup()
	character := dnfrepo.CharacterRecord{
		CharacterID: "19", AccountID: "acc", Job: "2", Level: 20,
		Stats: map[string]int64{"gold": 200000, "grow_type": 1},
	}
	if mutate != nil {
		mutate(&character)
	}
	if err := repos.Character.Save(ctx, character); err != nil {
		t.Fatal(err)
	}
	if err := repos.Skill.Save(ctx, dnfrepo.SkillRecord{CharacterID: "19", Points: dnfrepo.SkillPointState{SyncedLevel: character.Level}}); err != nil {
		t.Fatal(err)
	}
	return repos
}

func growupChangeInput(target byte) GrowupChangeInput {
	return GrowupChangeInput{
		AccountID:      "acc",
		CharacterID:    "19",
		TargetGrowType: target,
		Profiles:       growupChangeProfilesCache,
		SkillCatalog:   growupChangeCatalogCache,
		CommittedAt:    time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC),
	}
}

// growupChangeProfilesCache / growupChangeCatalogCache 由
// growupChangeProfessionResources 惰性初始化一次，供多次 ApplyGrowupChange 复用。
var (
	growupChangeProfilesCache *Profiles
	growupChangeCatalogCache  *dnfskill.Table
)

type growupChangeSource map[string]string

func (s growupChangeSource) ReadText(path string) (string, error) {
	text, ok := s[path]
	if !ok {
		return "", fmt.Errorf("missing %s", path)
	}
	return text, nil
}

func growupChangeProfessionResources(t *testing.T) (*Profiles, *dnfskill.Table) {
	t.Helper()
	if growupChangeProfilesCache != nil && growupChangeCatalogCache != nil {
		return growupChangeProfilesCache, growupChangeCatalogCache
	}
	source := growupChangeSource{
		DefaultCharacterList: "2 `Gunner/Gunner.chr`\n",
		"character/Gunner/Gunner.chr": `[initial value]
[skill]
1 1
[/skill]
[growtype 1]
[skill]
2 1
197 1
[/skill]
[growtype 2]
[skill]
3 1
[/skill]
[growtype 3]
[skill]
4 1
[/skill]
[awakening 1]
[awakening skill]
5 1
[/awakening skill]
`,
		dnfskill.DefaultList:       "2 `GunnerSkill.lst`\n",
		"skill/GunnerSkill.lst":    "1 `Gunner/a.skl` 2 `Gunner/b.skl` 3 `Gunner/c.skl` 4 `Gunner/d.skl` 5 `Gunner/e.skl` 197 `Gunner/mastery.skl`\n",
		"skill/Gunner/a.skl":       "[name]\n`a`\n[type]\n`[active]`\n",
		"skill/Gunner/b.skl":       "[name]\n`b`\n[type]\n`[active]`\n",
		"skill/Gunner/c.skl":       "[name]\n`c`\n[type]\n`[active]`\n",
		"skill/Gunner/d.skl":       "[name]\n`d`\n[type]\n`[active]`\n",
		"skill/Gunner/e.skl":       "[name]\n`e`\n[type]\n`[passive]`\n",
		"skill/Gunner/mastery.skl": "[name]\n`mastery`\n[type]\n`[passive]`\n",
	}
	index, err := dnfpvf.Build(context.Background(), source, dnfpvf.BuildOptions{Lists: []string{dnfskill.DefaultList}})
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := dnfskill.Load(context.Background(), index, dnfskill.Options{})
	if err != nil {
		t.Fatal(err)
	}
	profiles, err := LoadProfiles(context.Background(), source, catalog)
	if err != nil {
		t.Fatal(err)
	}
	growupChangeProfilesCache = profiles
	growupChangeCatalogCache = catalog
	return profiles, catalog
}
