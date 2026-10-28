package profession

import (
	"context"
	"errors"
	"testing"
	"time"

	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfskill "longheng.io/server/internal/modules/dnf/skill"
)

type profileSource map[string]string

func (s profileSource) ReadText(relativePath string) (string, error) {
	value, ok := s[relativePath]
	if !ok {
		return "", errors.New("missing " + relativePath)
	}
	return value, nil
}

func TestProfilesApplyClassChangeAndCumulativeAwakening(t *testing.T) {
	source := profileSource{
		DefaultCharacterList: "2 `Gunner/Gunner.chr`\n",
		"character/Gunner/Gunner.chr": `[initial value]
[skill]
1 1
[/skill]
[growtype 2]
[skill]
2 1
[/skill]
[awakening 1]
[awakening skill]
3 1
[/awakening skill]
[awakening 2]
[awakening skill]
4 1
[/awakening skill]
[growtype 3]
[skill]
10 1
[/skill]
`,
		dnfskill.DefaultList:         "2 `GunnerSkill.lst`\n",
		"skill/GunnerSkill.lst":      "1 `Gunner/a.skl` 2 `Gunner/b.skl` 3 `Gunner/c.skl` 4 `Gunner/d.skl` 5 `Gunner/system.skl` 6 `Gunner/expert.skl` 7 `Gunner/paid.skl` 8 `Gunner/future.skl` 9 `Gunner/awakening-only.skl` 10 `Gunner/alternate.skl` 11 `Gunner/optional-zero-cost.skl`\n",
		"skill/Gunner/a.skl":         "[name]\n`a`\n[skill type]\n`[active]`\n",
		"skill/Gunner/b.skl":         "[name]\n`b`\n[skill type]\n`[passive]`\n",
		"skill/Gunner/c.skl":         "[name]\n`c`\n[skill type]\n`[passive]`\n",
		"skill/Gunner/d.skl":         "[name]\n`d`\n[skill type]\n`[passive]`\n",
		"skill/Gunner/alternate.skl": "[name]\n`alternate`\n[skill type]\n`[active]`\n",
		"skill/Gunner/system.skl": `[name]
` + "`system`" + `
[required level]
15
[maximum level]
1
[skill class]
2
[growtype maximum level]
0 1 0
[/growtype maximum level]
[purchase cost]
0
[/purchase cost]
`,
		"skill/Gunner/expert.skl": `[name]
` + "`expert`" + `
[required level]
15
[maximum level]
1
[skill class]
4
[growtype maximum level]
0 1 0
[/growtype maximum level]
[purchase cost]
0
[/purchase cost]
`,
		"skill/Gunner/paid.skl": `[name]
` + "`paid`" + `
[required level]
15
[maximum level]
1
[skill class]
2
[growtype maximum level]
0 1 0
[/growtype maximum level]
[purchase cost]
20
[/purchase cost]
`,
		"skill/Gunner/future.skl": `[name]
` + "`future`" + `
[required level]
60
[maximum level]
1
[skill class]
2
[growtype maximum level]
0 1 0
[/growtype maximum level]
[purchase cost]
0
[/purchase cost]
`,
		"skill/Gunner/awakening-only.skl": `[name]
` + "`awakening-only`" + `
[required level]
15
[maximum level]
1
[skill class]
2
[growtype maximum level]
0 1 0
[/growtype maximum level]
[skill fitness second growtype]
1
[/skill fitness second growtype]
[purchase cost]
0
[/purchase cost]
`,
		"skill/Gunner/optional-zero-cost.skl": `[name]
` + "`optional zero-cost toggle`" + `
[required level]
30
[maximum level]
1
[skill class]
3
[growtype maximum level]
0 1 0
[/growtype maximum level]
[skill fitness growtype]
1
[/skill fitness growtype]
[pre required skill]
1 1
[/pre required skill]
[purchase cost]
0
[/purchase cost]
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
	profiles, err := LoadProfiles(context.Background(), source, catalog)
	if err != nil {
		t.Fatal(err)
	}
	points := dnfrepo.SkillPointState{TotalSP: 100, RemainingSP: 70, TotalTP: 10, RemainingTP: 5, SyncedLevel: 50}
	record := dnfrepo.SkillRecord{CharacterID: "9", Skills: map[int64]dnfrepo.SkillState{99: {Level: 2, Enabled: true}}}
	changed, err := profiles.ApplySkillTransition(catalog, 2, 50, Transition{Kind: KindClassChange, FirstGrowType: 1}, record, points)
	if err != nil {
		t.Fatal(err)
	}
	if len(changed.Skills) != 3 || changed.Skills[1].Level != 1 || changed.Skills[2].Level != 1 || changed.Skills[5].Level != 1 ||
		changed.Skills[6].Level != 0 || changed.Skills[7].Level != 0 || changed.Skills[8].Level != 0 || changed.Skills[9].Level != 0 ||
		changed.Skills[11].Level != 0 ||
		changed.Points.RemainingSP != 100 || changed.Points.RemainingTP != 10 {
		t.Fatalf("changed = %+v", changed)
	}
	alternateChanged, found := dnfrepo.SkillTree(changed, 1)
	if !found || len(alternateChanged.Skills) != 3 || alternateChanged.Skills[5].Level != 1 || alternateChanged.Points != changed.Points {
		t.Fatalf("alternate class-change tree = %+v found=%t", alternateChanged, found)
	}
	awakened, err := profiles.ApplySkillTransition(catalog, 2, 50, Transition{Kind: KindAwakening, FirstGrowType: 1, AwakeningStage: 2}, changed, changed.Points)
	if err != nil {
		t.Fatal(err)
	}
	if len(awakened.Skills) != 5 || awakened.Skills[3].Level != 1 || awakened.Skills[4].Level != 1 || awakened.Skills[5].Level != 1 {
		t.Fatalf("awakened skills = %+v", awakened.Skills)
	}
	alternateAwakened, found := dnfrepo.SkillTree(awakened, 1)
	if !found || len(alternateAwakened.Skills) != 5 || alternateAwakened.Skills[3].Level != 1 || alternateAwakened.Skills[4].Level != 1 || alternateAwakened.Skills[5].Level != 1 {
		t.Fatalf("alternate awakened tree = %+v found=%t", alternateAwakened, found)
	}
	freeAtLevel, ok := profiles.FreeGrantsAtLevel(catalog, 2, 0x21, 50)
	if !ok || len(freeAtLevel) != 5 || !containsGrant(freeAtLevel, 1) || !containsGrant(freeAtLevel, 2) || !containsGrant(freeAtLevel, 3) || !containsGrant(freeAtLevel, 4) || !containsGrant(freeAtLevel, 5) {
		t.Fatalf("second-awakening free baseline = %v, found=%t", freeAtLevel, ok)
	}
	purchased := record
	purchased.Skills = map[int64]dnfrepo.SkillState{99: {Level: 2, Enabled: true}}
	purchased.Points = points
	preserved, err := profiles.ApplySkillTransition(catalog, 2, 50, Transition{Kind: KindAwakening, FirstGrowType: 1, AwakeningStage: 1}, purchased, points)
	if err != nil {
		t.Fatal(err)
	}
	if preserved.Points != points || preserved.Skills[99].Level != 2 || preserved.Skills[3].Level != 1 {
		t.Fatalf("awakening changed learned skills or point ledger = %+v", preserved)
	}
	preservedAlternate, found := dnfrepo.SkillTree(preserved, 1)
	if !found || preservedAlternate.Points != points || preservedAlternate.Skills[99].Level != 2 || preservedAlternate.Skills[3].Level != 1 || preservedAlternate.Skills[5].Level != 1 {
		t.Fatalf("awakening alternate tree = %+v found=%t", preservedAlternate, found)
	}
	rebaseSource := preserved
	rebaseSource.Commands = map[int64][]byte{3: {1, 2, 3}}
	rebaseSource.Cooldowns = map[int64]time.Time{3: time.Unix(123, 0).UTC()}
	rebased, err := profiles.RebuildSkillRecordForGrowType(catalog, 2, 50, 0x02, rebaseSource, points)
	if err != nil {
		t.Fatal(err)
	}
	if len(rebased.Skills) != 2 || rebased.Skills[1].Level != 1 || rebased.Skills[10].Level != 1 ||
		rebased.Skills[2].Level != 0 || rebased.Skills[3].Level != 0 || len(rebased.Commands) != 0 || len(rebased.Cooldowns) != 0 ||
		rebased.Points.RemainingSP != rebased.Points.TotalSP || rebased.Points.RemainingTP != rebased.Points.TotalTP {
		t.Fatalf("administrative profession rebase = %+v", rebased)
	}
	rebasedAlternate, found := dnfrepo.SkillTree(rebased, 1)
	if !found || len(rebasedAlternate.Skills) != 2 || rebasedAlternate.Skills[10].Level != 1 || rebasedAlternate.Points != rebased.Points {
		t.Fatalf("administrative alternate tree = %+v found=%t", rebasedAlternate, found)
	}
	if _, err := profiles.RebuildSkillRecordForGrowType(catalog, 2, 50, 0x04, rebaseSource, points); !errors.Is(err, ErrTransitionInvalid) {
		t.Fatalf("unsupported administrative branch error = %v, want %v", err, ErrTransitionInvalid)
	}
	if _, err := profiles.RebuildSkillRecordForGrowType(catalog, 2, 50, 0x13, rebaseSource, points); !errors.Is(err, ErrTransitionInvalid) {
		t.Fatalf("unsupported administrative awakening error = %v, want %v", err, ErrTransitionInvalid)
	}
}

func TestProfilesAddsMappedZeroCostAwakeningHelperOnlyAtItsStageAndBranch(t *testing.T) {
	source := profileSource{
		DefaultCharacterList: "3 `Mage/Mage.chr`\n",
		"character/Mage/Mage.chr": `[initial value]
[skill]
1 1
[/skill]
[growtype 3]
[awakening 1]
[awakening skill]
[/awakening skill]
[growtype 4]
[awakening 1]
[awakening skill]
[/awakening skill]
`,
		dnfskill.DefaultList:  "3 `MageSkill.lst`\n",
		"skill/MageSkill.lst": "1 `Mage/base.skl` 301 `Mage/bellatrixconversion.skl`\n",
		"skill/Mage/base.skl": "[name]\n`base`\n[skill type]\n`[active]`\n",
		"skill/Mage/bellatrixconversion.skl": `[name]
` + "`bellatrix conversion`" + `
[required level]
50
[maximum level]
1
[skill class]
2
[growtype maximum level]
0 0 0 0 0 0
[/growtype maximum level]
[skill fitness second growtype]
1
[/skill fitness second growtype]
[purchase cost]
0
[/purchase cost]
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
	profiles, err := LoadProfiles(context.Background(), source, catalog)
	if err != nil {
		t.Fatal(err)
	}
	beforeAwakening, _ := profiles.FreeGrantsAtLevel(catalog, 3, 0x03, 50)
	wrongBranch, _ := profiles.FreeGrantsAtLevel(catalog, 3, 0x12, 50)
	firstAwakening, _ := profiles.FreeGrantsAtLevel(catalog, 3, 0x13, 50)
	if containsGrant(beforeAwakening, 301) || containsGrant(wrongBranch, 301) || grantLevel(firstAwakening, 301) != 1 {
		t.Fatalf("mapped awakening helper before=%v wrong_branch=%v first=%v", beforeAwakening, wrongBranch, firstAwakening)
	}
}

func TestFreeGrantsAtLevelRetainsMissingAwakeningDefinitionsForAudit(t *testing.T) {
	source := profileSource{
		DefaultCharacterList: "3 `Mage/Mage.chr`\n",
		"character/Mage/Mage.chr": "[initial value]\n[skill]\n1 1\n[/skill]\n" +
			"[growtype 4]\n[awakening 1]\n[awakening skill]\n777 1\n[/awakening skill]\n",
		dnfskill.DefaultList:  "3 `MageSkill.lst`\n",
		"skill/MageSkill.lst": "1 `Mage/base.skl`\n",
		"skill/Mage/base.skl": "[name]\n`base`\n[type]\n`[active]`\n",
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
	raw, rawOK := profiles.FreeGrants(3, 3, 1)
	protected, protectedOK := profiles.FreeGrantsAtLevel(catalog, 3, 0x13, 90)
	if !rawOK || !protectedOK || !containsGrant(raw, 777) || !containsGrant(protected, 777) {
		t.Fatalf("missing awakening definition raw=%v found=%t protected=%v found=%t", raw, rawOK, protected, protectedOK)
	}
	if snapshot := profiles.Snapshot(); snapshot.MissingSkills != 1 {
		t.Fatalf("profession snapshot=%+v want one observable missing skill", snapshot)
	}
}

func TestProfilesDisplayNameUsesPVFGrowAndAwakeningNames(t *testing.T) {
	profile, err := parseJobProfile(`[growtype name]
` + "`鬼剑士` `驭剑士`" + `
[initial value]
[skill]
1 1
[/skill]
[growtype 1]
[awakening name]
` + "`黑暗武士` `时空主宰`" + `
[growtype 2]
[skill]
2 1
[/skill]
[awakening name]
` + "`剑宗` `剑皇`" + `
`)
	if err != nil {
		t.Fatal(err)
	}
	profiles := &Profiles{jobs: map[byte]jobProfile{11: profile}}
	for _, test := range []struct {
		grow byte
		want string
	}{
		{grow: 0x00, want: "鬼剑士"},
		{grow: 0x01, want: "驭剑士"},
		{grow: 0x11, want: "剑宗"},
		{grow: 0x21, want: "剑皇"},
	} {
		got, ok := profiles.DisplayName(11, test.grow)
		if !ok || got != test.want {
			t.Fatalf("grow=0x%02x display=%q found=%t want=%q", test.grow, got, ok, test.want)
		}
	}
}

func TestProfilesApplyPVFFixedLevelSkillAtCharacterLevel(t *testing.T) {
	source := profileSource{
		DefaultCharacterList:          "2 `Gunner/Gunner.chr`\n",
		"character/Gunner/Gunner.chr": "[initial value]\n[skill]\n1 1\n[/skill]\n[growtype 2]\n[skill]\n2 1\n[/skill]\n",
		dnfskill.DefaultList:          "2 `GunnerSkill.lst`\n",
		"skill/GunnerSkill.lst":       "1 `Gunner/a.skl` 2 `Gunner/b.skl`\n",
		"skill/Gunner/a.skl":          "[name]\n`a`\n[required level]\n1\n[maximum level]\n10\n[fixed level skill]\n1\n[interval level]\n10\n[add level per interval]\n1\n",
		"skill/Gunner/b.skl":          "[name]\n`b`\n[type]\n`[passive]`\n",
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
	changed, err := profiles.ApplySkillTransition(
		catalog, 2, 21,
		Transition{Kind: KindClassChange, FirstGrowType: 1},
		dnfrepo.SkillRecord{CharacterID: "9"},
		dnfrepo.SkillPointState{TotalSP: 100, RemainingSP: 70},
	)
	if err != nil {
		t.Fatal(err)
	}
	if got := changed.Skills[1].Level; got != 3 {
		t.Fatalf("fixed-level initial skill=%d want=3", got)
	}
	freeAtLevel, ok := profiles.FreeGrantsAtLevel(catalog, 2, 0x01, 21)
	if !ok || grantLevel(freeAtLevel, 1) != 3 {
		t.Fatalf("fixed-level free baseline=%v found=%t want skill1 level3", freeAtLevel, ok)
	}
}

func grantLevel(grants []Grant, skillID uint16) int {
	for _, grant := range grants {
		if grant.SkillID == skillID {
			return grant.Level
		}
	}
	return 0
}

func TestParseJobProfileRecordsEmptyBranchZeroAwakeningCapabilities(t *testing.T) {
	profile, err := parseJobProfile(`[initial value]
[skill]
1 1
[/skill]
[growtype 1]
[awakening 1]
[awakening skill]
[/awakening skill]
[awakening 2]
[awakening skill]
[/awakening skill]
`)
	if err != nil {
		t.Fatal(err)
	}
	profiles := &Profiles{jobs: map[byte]jobProfile{9: profile}}
	firstRequest, _ := ParseReward(2, "[awakening type]", []int64{1})
	first, err := profiles.PlanTransition(9, 0x00, firstRequest)
	if err != nil || first.NewGrowType != 0x10 {
		t.Fatalf("empty branch-zero awakening1 capability = %+v, %v", first, err)
	}
	secondRequest, _ := ParseReward(3, "[awakening type]", []int64{2})
	second, err := profiles.PlanTransition(9, first.NewGrowType, secondRequest)
	if err != nil || second.NewGrowType != 0x20 {
		t.Fatalf("empty branch-zero awakening2 capability = %+v, %v", second, err)
	}
}
