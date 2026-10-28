package skill

import (
	"context"
	"errors"
	"testing"

	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
)

func TestLoadBuildsJobScopedSkillTable(t *testing.T) {
	index := testIndex(t)
	table, err := Load(context.Background(), index, Options{})
	if err != nil {
		t.Fatalf("load skill: %v", err)
	}
	if snapshot := table.Snapshot(); snapshot.Jobs != 2 || snapshot.Skills != 2 {
		t.Fatalf("unexpected snapshot: %+v", snapshot)
	}

	swordman, ok := table.Find(0, 46)
	if !ok {
		t.Fatal("expected swordman skill")
	}
	if swordman.Name != "upper slash" || swordman.Job != 0 || swordman.Kind != "[active]" || !swordman.Active {
		t.Fatalf("unexpected swordman skill: %+v", swordman)
	}
	if swordman.RequiredLevel != 1 || swordman.RequiredLevelRange != 5 || swordman.MaximumLevel != 20 || swordman.SkillClass != 1 {
		t.Fatalf("unexpected level metadata: %+v", swordman)
	}
	if swordman.FeatureSkillIndex != 217 {
		t.Fatalf("unexpected feature skill index: %+v", swordman)
	}
	if !swordman.FixedLevelSkill || swordman.FixedLevelBase != 1 || swordman.FixedLevelInterval != 5 || swordman.FixedLevelIncrement != 2 || swordman.FixedLevelForCharacter(11) != 5 {
		t.Fatalf("unexpected fixed-level metadata: %+v", swordman)
	}
	if len(swordman.GrowTypes) != 3 || len(swordman.GrowTypeMaximumLevels) != 3 || swordman.GrowTypeMaximumLevels[2] != 20 || len(swordman.SecondGrowTypes) != 1 || swordman.SecondGrowTypes[0] != 2 || len(swordman.SecondGrowTypeMaximumLevels) != 6 || swordman.SecondGrowTypeMaximumLevels[5] != 20 || len(swordman.PurchaseCost) != 1 || swordman.PurchaseCost[0] != 20 {
		t.Fatalf("unexpected rule arrays: %+v", swordman)
	}
	if len(swordman.Prerequisites) != 1 || swordman.Prerequisites[0] != (Prerequisite{SkillID: 20, Level: 1}) {
		t.Fatalf("unexpected prerequisites: %+v", swordman.Prerequisites)
	}
	if !swordman.SupportsGrowType(0) || !swordman.SupportsGrowType(1) || !swordman.SupportsGrowType(2) || swordman.SupportsGrowType(9) || !swordman.IsTPSkill() || swordman.LevelCost(10) != 2 {
		t.Fatalf("unexpected domain rules: %+v", swordman)
	}
	if swordman.SupportsCharacterGrowth(0x12) || !swordman.SupportsCharacterGrowth(0x22) {
		t.Fatalf("second growtype fitness was not applied to packed grow_type: %+v", swordman)
	}
	if got := swordman.MaximumLearnableLevel(11, 0x22); got != 3 {
		t.Fatalf("maximum learnable level=%d want 3: %+v", got, swordman)
	}
	if swordman.CoolTime != 2000 || swordman.MPCost != 12 || swordman.Scalars["power"] != 120 {
		t.Fatalf("unexpected scalar fields: %+v", swordman)
	}

	fighter, ok := table.Find(1, 46)
	if !ok || fighter.Name != "muse upper" {
		t.Fatalf("same skill ID must remain job-scoped: %+v ok=%t", fighter, ok)
	}
	if _, ok := table.Find(2, 46); ok {
		t.Fatal("skill leaked into an unrelated job")
	}

	swordman.GrowTypes[0] = 99
	swordman.SecondGrowTypeMaximumLevels[5] = 99
	swordman.Prerequisites[0].Level = 99
	swordman.PurchaseCost[0] = 99
	swordman.Scalars["power"] = 1
	again, _ := table.FindPath(0, "./SKILL/Swordman/UpperSlash.skl")
	if again.GrowTypes[0] != 0 || again.SecondGrowTypeMaximumLevels[5] != 20 || again.Prerequisites[0].Level != 1 || again.PurchaseCost[0] != 20 || again.Scalars["power"] != 120 {
		t.Fatalf("table returned mutable fields: %+v", again)
	}
}

func TestSkillExplicitGrowTypeOwnershipOverridesClientPreviewMaximum(t *testing.T) {
	definition := Skill{
		GrowTypes:             []int{4},
		GrowTypeMaximumLevels: []int{50, 0, 0, 0, 50, 0},
	}
	if definition.SupportsGrowType(0) || !definition.SupportsGrowType(4) {
		t.Fatalf("explicit class ownership was not authoritative: %+v", definition)
	}
}

func TestSkillExclusiveClassMaximumRepairsContradictoryFitnessMarker(t *testing.T) {
	definition := Skill{
		GrowTypes:             []int{1},
		GrowTypeMaximumLevels: []int{0, 0, 1, 0, 0, 0},
	}
	if definition.SupportsGrowType(1) || !definition.SupportsGrowType(2) || !definition.SupportsCharacterGrowth(0x22) {
		t.Fatalf("exclusive class maximum did not repair contradictory fitness marker: %+v", definition)
	}
}

func TestSkillAmbiguousClassMaximumDoesNotOverrideFitnessMarker(t *testing.T) {
	definition := Skill{
		GrowTypes:             []int{1},
		GrowTypeMaximumLevels: []int{0, 0, 1, 1, 0, 0},
	}
	if !definition.SupportsGrowType(1) || definition.SupportsGrowType(2) || definition.SupportsGrowType(3) {
		t.Fatalf("ambiguous maximum vector overrode explicit fitness marker: %+v", definition)
	}
}

func TestExternalProfessionBranchZeroUsesPositivePVFMaximum(t *testing.T) {
	tests := []struct {
		name       string
		definition Skill
		want       bool
	}{
		{
			name: "creator quick standing",
			definition: Skill{
				Job:                   10,
				GrowTypes:             []int{1},
				GrowTypeMaximumLevels: []int{1, 1, 1, 1, 1, 1},
			},
			want: true,
		},
		{
			name: "dark knight tp",
			definition: Skill{
				Job:                   9,
				GrowTypes:             []int{1, 2, 3, 4},
				GrowTypeMaximumLevels: []int{5, 5, 5, 5, 5, 5},
			},
			want: true,
		},
		{
			name: "ordinary profession preview remains rejected",
			definition: Skill{
				Job:                   7,
				GrowTypes:             []int{1},
				GrowTypeMaximumLevels: []int{1, 1, 0, 0, 0, 0},
			},
			want: false,
		},
		{
			name: "external profession without branch zero maximum remains rejected",
			definition: Skill{
				Job:                   9,
				GrowTypes:             []int{1},
				GrowTypeMaximumLevels: []int{0, 1, 0, 0, 0, 0},
			},
			want: false,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := test.definition.SupportsCharacterGrowth(0); got != test.want {
				t.Fatalf("SupportsCharacterGrowth(0)=%t want %t: %+v", got, test.want, test.definition)
			}
		})
	}
}

func TestSkillMaximumLearnableLevel(t *testing.T) {
	tests := []struct {
		name           string
		definition     Skill
		characterLevel int
		growType       int
		want           int
	}{
		{
			name: "weapon combo level 90",
			definition: Skill{
				RequiredLevel:         15,
				RequiredLevelRange:    15,
				MaximumLevel:          15,
				GrowTypes:             []int{1},
				GrowTypeMaximumLevels: []int{0, 15, 0, 0, 0, 0},
			},
			characterLevel: 90,
			growType:       0x21,
			want:           6,
		},
		{
			name: "awakening interval and second-grow maximum",
			definition: Skill{
				RequiredLevel:               48,
				RequiredLevelRange:          3,
				MaximumLevel:                40,
				GrowTypeMaximumLevels:       []int{0, 0, 0, 0, 0, 0},
				SecondGrowTypes:             []int{1},
				SecondGrowTypeMaximumLevels: []int{0, 0, 0, 0, 0, 0, 30, 30, 0, 0, 0, 0},
			},
			characterLevel: 90,
			growType:       0x13,
			want:           15,
		},
		{
			name: "awakening stage zero is unsupported",
			definition: Skill{
				RequiredLevel:               48,
				RequiredLevelRange:          3,
				MaximumLevel:                40,
				SecondGrowTypes:             []int{1},
				SecondGrowTypeMaximumLevels: []int{0, 0, 0, 0, 0, 0, 30, 30, 0, 0, 0, 0},
			},
			characterLevel: 90,
			growType:       0x03,
			want:           0,
		},
		{
			name: "missing level range leaves static limits authoritative",
			definition: Skill{
				RequiredLevel:         15,
				MaximumLevel:          40,
				GrowTypes:             []int{1},
				GrowTypeMaximumLevels: []int{0, 30, 0, 0, 0, 0},
			},
			characterLevel: 15,
			growType:       0x01,
			want:           30,
		},
		{
			name: "below required level",
			definition: Skill{
				RequiredLevel:      15,
				RequiredLevelRange: 15,
				MaximumLevel:       15,
			},
			characterLevel: 14,
			growType:       0,
			want:           0,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := test.definition.MaximumLearnableLevel(test.characterLevel, test.growType); got != test.want {
				t.Fatalf("MaximumLearnableLevel(%d, %#x)=%d want %d: %+v", test.characterLevel, test.growType, got, test.want, test.definition)
			}
		})
	}
}

func TestSkillMaximumLearnableLevelForTree(t *testing.T) {
	// A tree row is learnable regardless of PVF profession fitness, but the
	// document's caps still bound it. When the character's own branch carries no
	// positive `[growtype maximum level]` row the largest declared cap applies.
	tests := []struct {
		name           string
		definition     Skill
		characterLevel int
		growType       int
		want           int
	}{
		{
			name: "own branch row is zero falls back to the declared class cap",
			// Ez-8自爆者 shape: [skill fitness growtype] 3 with
			// [growtype maximum level] 50 0 0 50 0 0 on a base Gunner.
			definition: Skill{
				RequiredLevel:         15,
				MaximumLevel:          60,
				GrowTypes:             []int{3},
				GrowTypeMaximumLevels: []int{50, 0, 0, 50, 0, 0},
			},
			characterLevel: 50,
			growType:       0x00,
			want:           50,
		},
		{
			name: "declared branch keeps its own cap",
			definition: Skill{
				RequiredLevel:         15,
				MaximumLevel:          60,
				GrowTypes:             []int{3},
				GrowTypeMaximumLevels: []int{50, 0, 0, 50, 0, 0},
			},
			characterLevel: 50,
			growType:       0x03,
			want:           50,
		},
		{
			name: "no own-branch row at all still yields the declared cap",
			definition: Skill{
				RequiredLevel:         1,
				MaximumLevel:          20,
				GrowTypeMaximumLevels: []int{0, 20},
			},
			characterLevel: 10,
			growType:       0x00,
			want:           20,
		},
		{
			name: "global maximum still wins over the fallback",
			definition: Skill{
				RequiredLevel:         1,
				MaximumLevel:          5,
				GrowTypeMaximumLevels: []int{0, 20},
			},
			characterLevel: 10,
			growType:       0x00,
			want:           5,
		},
		{
			name: "below the declared awakening stage keeps the remaining limits",
			definition: Skill{
				RequiredLevel:               48,
				RequiredLevelRange:          3,
				MaximumLevel:                40,
				SecondGrowTypes:             []int{1},
				SecondGrowTypeMaximumLevels: []int{0, 0, 0, 0, 0, 0, 30, 30, 0, 0, 0, 0},
			},
			characterLevel: 90,
			growType:       0x03,
			want:           15,
		},
		{
			name: "fitting awakening stage matches the fitness-aware maximum",
			definition: Skill{
				RequiredLevel:               48,
				RequiredLevelRange:          3,
				MaximumLevel:                40,
				SecondGrowTypes:             []int{1},
				SecondGrowTypeMaximumLevels: []int{0, 0, 0, 0, 0, 0, 30, 30, 0, 0, 0, 0},
			},
			characterLevel: 90,
			growType:       0x13,
			want:           15,
		},
		{
			name: "below required level stays unlearnable",
			definition: Skill{
				RequiredLevel:         15,
				MaximumLevel:          60,
				GrowTypeMaximumLevels: []int{50, 50, 50, 50, 50, 50},
			},
			characterLevel: 14,
			growType:       0x00,
			want:           0,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := test.definition.MaximumLearnableLevelForTree(test.characterLevel, test.growType); got != test.want {
				t.Fatalf("MaximumLearnableLevelForTree(%d, %#x)=%d want %d: %+v", test.characterLevel, test.growType, got, test.want, test.definition)
			}
		})
	}
}

// TestSkillSplitsClassBranchFromAwakeningDimension pins the two PVF fitness
// dimensions apart. Only the class branch is relaxed for a purchase (the client
// tree renders every row of the JOB); the awakening stage keeps blocking,
// because the first awakening is granted by its own transition and the second
// one requires its quest.
func TestSkillSplitsClassBranchFromAwakeningDimension(t *testing.T) {
	branchLocked := Skill{
		// Ez-8自爆者 shape: another branch only, no awakening rule.
		GrowTypes:             []int{3},
		GrowTypeMaximumLevels: []int{50, 0, 0, 50, 0, 0},
	}
	awakeningLocked := Skill{
		SecondGrowTypes:             []int{1},
		SecondGrowTypeMaximumLevels: []int{0, 0, 0, 0, 0, 30, 0, 0, 0, 0, 0, 0},
	}
	bothLocked := Skill{
		GrowTypes:                   []int{3},
		GrowTypeMaximumLevels:       []int{50, 0, 0, 50, 0, 0},
		SecondGrowTypes:             []int{1},
		SecondGrowTypeMaximumLevels: []int{0, 0, 0, 0, 0, 30, 0, 0, 0, 0, 0, 0},
	}
	tests := []struct {
		name              string
		definition        Skill
		growType          int
		wantBranch        bool
		wantAwakening     bool
		wantCharacterGrow bool
	}{
		{name: "branch locked, base class", definition: branchLocked, growType: 0x00, wantBranch: false, wantAwakening: true, wantCharacterGrow: false},
		{name: "branch locked, own branch", definition: branchLocked, growType: 0x03, wantBranch: true, wantAwakening: true, wantCharacterGrow: true},
		{name: "awakening locked, unawakened", definition: awakeningLocked, growType: 0x00, wantBranch: true, wantAwakening: false, wantCharacterGrow: false},
		{name: "awakening locked, awakened", definition: awakeningLocked, growType: 0x10, wantBranch: true, wantAwakening: true, wantCharacterGrow: true},
		{name: "both locked, base class", definition: bothLocked, growType: 0x00, wantBranch: false, wantAwakening: false, wantCharacterGrow: false},
		{name: "both locked, own branch unawakened", definition: bothLocked, growType: 0x03, wantBranch: true, wantAwakening: false, wantCharacterGrow: false},
		{name: "both locked, own branch awakened", definition: bothLocked, growType: 0x13, wantBranch: true, wantAwakening: true, wantCharacterGrow: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := test.definition.SupportsGrowType(test.growType & 0x0f); got != test.wantBranch {
				t.Fatalf("SupportsGrowType = %t, want %t", got, test.wantBranch)
			}
			if got := test.definition.SupportsCharacterAwakening(test.growType); got != test.wantAwakening {
				t.Fatalf("SupportsCharacterAwakening = %t, want %t", got, test.wantAwakening)
			}
			if got := test.definition.SupportsCharacterGrowth(test.growType); got != test.wantCharacterGrow {
				t.Fatalf("SupportsCharacterGrowth = %t, want %t", got, test.wantCharacterGrow)
			}
		})
	}
}

func TestLoadRequiresIndex(t *testing.T) {
	_, err := Load(context.Background(), nil, Options{})
	if !errors.Is(err, ErrIndexRequired) {
		t.Fatalf("expected ErrIndexRequired, got %v", err)
	}
}

func TestParseTreatsUntypedExecutableSkillAsActive(t *testing.T) {
	doc, err := dnfpvf.Parse("skill/ATMage/SpiralPress.skl", `
[name]
`+"`"+`wind dragon strike`+"`"+`
[executable states]
`+"`"+`[stand]`+"`"+` `+"`"+`[dash]`+"`"+`
[/executable states]
[consume MP]
580 4500
[/consume MP]
`)
	if err != nil {
		t.Fatal(err)
	}
	definition, err := parseSkill(8, 138, doc.Path, doc)
	if err != nil {
		t.Fatal(err)
	}
	if !definition.Active {
		t.Fatalf("untyped executable skill must be quick-slot eligible: %+v", definition)
	}

	passiveDoc, err := dnfpvf.Parse("skill/ATMage/WindMaster.skl", `
[type]
`+"`"+`[passive]`+"`"+`
[executable states]
`+"`"+`[stand]`+"`"+`
[/executable states]
`)
	if err != nil {
		t.Fatal(err)
	}
	passive, err := parseSkill(8, 75, passiveDoc.Path, passiveDoc)
	if err != nil {
		t.Fatal(err)
	}
	if passive.Active {
		t.Fatalf("explicit passive type must remain non-active: %+v", passive)
	}
}

func TestParseTreatsEmptyFixedLevelSectionAsAutomatic(t *testing.T) {
	doc, err := dnfpvf.Parse("skill/Mage/Bellatrix.skl", `
[name]
`+"`"+`Teana Transformation`+"`"+`
[required level]
50
[required level range]
5
[maximum level]
40
[fixed level skill]
[interval level]
5
[add level per interval]
1
[/fixed level skill]
`)
	if err != nil {
		t.Fatal(err)
	}
	definition, err := parseSkill(3, 83, doc.Path, doc)
	if err != nil {
		t.Fatal(err)
	}
	if !definition.FixedLevelSkill || definition.FixedLevelBase != 1 ||
		definition.FixedLevelInterval != 5 || definition.FixedLevelIncrement != 1 ||
		definition.RequiredLevelRange != 5 || definition.FixedLevelForCharacter(90) != 9 {
		t.Fatalf("unexpected container-style fixed-level metadata: %+v", definition)
	}
}

func TestLoadReportsEmptyList(t *testing.T) {
	index, err := dnfpvf.Build(context.Background(), textSource{
		"skill/skilllist.lst": "0 `SwordmanSkill.lst`\n",
	}, dnfpvf.BuildOptions{Paths: []string{"skill/skilllist.lst"}})
	if err != nil {
		t.Fatalf("build index: %v", err)
	}
	_, err = Load(context.Background(), index, Options{})
	if !errors.Is(err, ErrListEmpty) {
		t.Fatalf("expected ErrListEmpty, got %v", err)
	}
}

func testIndex(t *testing.T) *dnfpvf.Index {
	t.Helper()
	index, err := dnfpvf.Build(context.Background(), textSource{
		"skill/skilllist.lst":     "0 `SwordmanSkill.lst`\n1 `FighterSkill.lst`\n",
		"skill/SwordmanSkill.lst": "46 `Swordman/UpperSlash.skl`\n",
		"skill/FighterSkill.lst":  "46 `Fighter/MuseUpper.skl`\n",
		"skill/Swordman/UpperSlash.skl": `
[name]
` + "`upper slash`" + `
[type]
` + "`[active]`" + `
[required level]
1
[required level range]
5
[maximum level]
20
[fixed level skill]
1
[interval level]
5
[add level per interval]
2
[skill class]
1
[skill fitness growtype]
0 1 2
[/skill fitness growtype]
[growtype maximum level]
0 0 20
[/growtype maximum level]
[second growtype maximum level]
0 0 0 0 0 20
[/second growtype maximum level]
[skill fitness second growtype]
2
[/skill fitness second growtype]
[pre required skill]
20 1
[/pre required skill]
[purchase cost]
20
[/purchase cost]
[special purchase cost]
2
[/special purchase cost]
[feature skill type]
1
[feature skill index]
217
[cool time]
2000
[consume MP]
12
[power]
120
`,
		"skill/Fighter/MuseUpper.skl": `
[name]
` + "`muse upper`" + `
[type]
` + "`[passive]`" + `
`,
	}, dnfpvf.BuildOptions{Lists: []string{DefaultList}})
	if err != nil {
		t.Fatalf("build index: %v", err)
	}
	return index
}

type textSource map[string]string

func (s textSource) ReadText(relativePath string) (string, error) {
	for key, value := range s {
		if pathKey(key) == pathKey(relativePath) {
			return value, nil
		}
	}
	return "", dnfpvf.ErrDocNotFound
}
