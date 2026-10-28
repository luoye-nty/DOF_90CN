package dnfbridge

import (
	"testing"

	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
)

func TestParseCurrentUpgradeTableKeepsFractionalCostWeights(t *testing.T) {
	const text = "[cost]\n100\n[/cost]\n[cost weights by rarity]\n0.5 1.5 3 5 8\n[/cost weights by rarity]\n[type]\n1.2 0 0.7\n[/type]\n"
	document, err := dnfpvf.Parse("etc/upgrade.etc", text)
	if err != nil {
		t.Fatal(err)
	}
	table := parseCurrentUpgradeTable(document, text)
	if len(table.CostWeightsByRarity) != 5 || table.CostWeightsByRarity[0] != 0.5 || table.CostWeightsByRarity[4] != 8 {
		t.Fatalf("rarity weights=%v", table.CostWeightsByRarity)
	}
	if len(table.TypeWeights) != 3 || table.TypeWeights[0] != 1.2 || table.TypeWeights[1] != 0 || table.TypeWeights[2] != 0.7 {
		t.Fatalf("type weights=%v", table.TypeWeights)
	}
}

func TestCalculateCurrentUpgradeGoldCostUsesGradeRarityTypeAndTargetLevel(t *testing.T) {
	costs := make([]int, 91)
	costs[85] = 53_715
	base := currentUpgradeTable{
		Costs:               costs,
		CostWeightsByRarity: []float64{0.5, 1.5, 3, 5, 8, 4, 6},
		TypeWeights:         []float64{1.2, 0, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1},
		// The coefficients are [cost weight by upgrade level], indexed by the
		// *current* level: 0.50 for +0, 1.00 for +1. These are the two levels
		// where the live 强化 table and the retired [table type] weight * 25
		// curve agree, so the expectations below did not move when the
		// coefficient source was corrected.
		CostWeightByUpgradeLevel: []float64{0.5, 1.0},
		Rows: []currentUpgradeTableRow{
			{TargetLevel: 1},
			{TargetLevel: 2},
		},
	}

	// The [cost] row is indexed by [grade], not [minimum level]: a definition
	// carrying only MinimumLevel must not resolve.
	weapon := dungeonDropItemDefinition{ItemID: 1, Kind: dungeonDropItemEquipment, Grade: 85, Rarity: 4, EquipmentType: "[weapon]"}
	// 53715 * 8 * 1.2 * 0.02 * 25 = 257832
	got, err := calculateCurrentUpgradeGoldCost(&base, weapon, 1)
	if err != nil || got != 257_832 {
		t.Fatalf("weapon gold cost=%d err=%v want=257832", got, err)
	}
	// Doubling the level coefficient doubles the price.
	got, err = calculateCurrentUpgradeGoldCost(&base, weapon, 2)
	if err != nil || got != 515_664 {
		t.Fatalf("weapon +2 gold cost=%d err=%v want=515664", got, err)
	}
	coat := weapon
	coat.EquipmentType = "[coat]"
	// 53715 * 8 * 1 * 0.02 * 25 = 214860
	got, err = calculateCurrentUpgradeGoldCost(&base, coat, 1)
	if err != nil || got != 214_860 {
		t.Fatalf("coat gold cost=%d err=%v want=214860", got, err)
	}
	// A target level the table does not cover must fail closed rather than
	// silently price the attempt at zero.
	if _, err := calculateCurrentUpgradeGoldCost(&base, weapon, 3); err == nil {
		t.Fatal("target level beyond the table was accepted")
	}
	onlyMinimumLevel := dungeonDropItemDefinition{ItemID: 1, Kind: dungeonDropItemEquipment, MinimumLevel: 85, Rarity: 4, EquipmentType: "[weapon]"}
	if _, err := calculateCurrentUpgradeGoldCost(&base, onlyMinimumLevel, 1); err == nil {
		t.Fatal("grade 0 was accepted; the cost row must come from [grade]")
	}
}

func TestCalculateCurrentAmplifyGoldCostUsesIndependentPVFWeights(t *testing.T) {
	costs := make([]int, 91)
	costs[85] = 60_000
	// No CostWeightByUpgradeLevel: etc/amplifyupgrade.etc carries no
	// [cost weight by upgrade level] section, so the client's 增幅器 prices at
	// the bare base cost. The live reading on 2026-09-25 was 220500 for a
	// +10->+11 attempt, which is exactly 44100 * 2.50 * 2.00 with no level
	// factor; the retired [table type] weight * 25 term charged 992250 there.
	table := currentUpgradeTable{
		Costs:               costs,
		CostWeightsByRarity: []float64{0.25, 0.75, 1.5, 2.5, 4, 2, 3},
		TypeWeights:         []float64{2, 0, 0.7, 0.7, 0.7, 0.7, 0.7, 0.7, 0.7, 0.7, 0.7, 0.7, 0.7},
		Rows:                []currentUpgradeTableRow{{TargetLevel: 1}},
	}
	definition := dungeonDropItemDefinition{ItemID: 1, Kind: dungeonDropItemEquipment, Grade: 85, Rarity: 4, EquipmentType: "[earring]"}
	// 60000 * 4 * 0.7 = 168000
	got, err := calculateCurrentUpgradeGoldCost(&table, definition, 1)
	if err != nil || got != 168_000 {
		t.Fatalf("amplify gold cost=%d err=%v want=168000", got, err)
	}
	// An 增幅 has no level coefficient, so +10->+11 costs the same base price
	// as +0->+1. That flat curve is the client's, not a dropped coefficient:
	// TestRealPVFAmplifyGoldCostMatchesTheClientPopup pins it against the live
	// PVF at the level the 2026-09-25 popup was read.
	table.Rows = make([]currentUpgradeTableRow, 11)
	for i := range table.Rows {
		table.Rows[i] = currentUpgradeTableRow{TargetLevel: i + 1}
	}
	got, err = calculateCurrentUpgradeGoldCost(&table, definition, 11)
	if err != nil || got != 168_000 {
		t.Fatalf("amplify +10->+11 gold cost=%d err=%v want=168000", got, err)
	}
}

// TestCalculateCurrentUpgradeGoldCostUsesTheClientsPerLevelWeight pins the
// divergence that the retired [table type] weight * 25 curve got wrong: at
// current +2 the client's [cost weight by upgrade level] is 0.50 while that
// curve produced 1.00, so the live 强化 charged twice the popup.
func TestCalculateCurrentUpgradeGoldCostUsesTheClientsPerLevelWeight(t *testing.T) {
	costs := make([]int, 91)
	costs[85] = 53_715
	table := currentUpgradeTable{
		Costs:               costs,
		CostWeightsByRarity: []float64{0.5, 1.5, 3, 5, 8, 4, 6},
		TypeWeights:         []float64{1.2, 0, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1},
		// The live upgrade.etc run: 0..3 -> 0.50, 10 -> 1.50, 14 -> 8.50.
		CostWeightByUpgradeLevel: []float64{0.5, 0.5, 0.5, 0.5, 1, 1.1, 1.2, 1.3, 1.4, 1.5, 1.5, 2.5, 4, 6, 8.5},
	}
	for i := 1; i <= len(table.CostWeightByUpgradeLevel); i++ {
		table.Rows = append(table.Rows, currentUpgradeTableRow{TargetLevel: i})
	}
	weapon := dungeonDropItemDefinition{ItemID: 1, Kind: dungeonDropItemEquipment, Grade: 85, Rarity: 4, EquipmentType: "[weapon]"}
	// base = 53715 * 8 * 1.2 = 515664
	for _, testCase := range []struct {
		name        string
		targetLevel int
		want        int64
	}{
		{name: "+0->+1 uses 0.50", targetLevel: 1, want: 257_832},
		{name: "+2->+3 still uses 0.50", targetLevel: 3, want: 257_832},
		{name: "+9->+10 uses 1.50", targetLevel: 10, want: 773_496},
		{name: "+14->+15 uses 8.50", targetLevel: 15, want: 4_383_144},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			got, err := calculateCurrentUpgradeGoldCost(&table, weapon, testCase.targetLevel)
			if err != nil || got != testCase.want {
				t.Fatalf("gold cost=%d err=%v want=%d", got, err, testCase.want)
			}
		})
	}
	// Past the end of the client's table the last published coefficient is held
	// rather than dropping to 1, which would make +19 cheaper than +18.
	table.Rows = append(table.Rows, currentUpgradeTableRow{TargetLevel: 16})
	got, err := calculateCurrentUpgradeGoldCost(&table, weapon, 16)
	if err != nil || got != 4_383_144 {
		t.Fatalf("gold cost past the last level=%d err=%v want=4383144", got, err)
	}
}

// TestParseCurrentUpgradeCostWeightByUpgradeLevel pins the shape check: only a
// clean level-0..n-1 run of positive weights is accepted, so a shifted or
// zeroed section can never silently reprice every level.
func TestParseCurrentUpgradeCostWeightByUpgradeLevel(t *testing.T) {
	live := []float64{
		0, 0.50, 0.50, 1, 0.50, 0.50, 2, 0.50, 0.50, 3, 0.50, 0.50,
		4, 1, 1, 5, 1.10, 1.10, 6, 1.20, 1.20, 7, 1.30, 1.30,
		8, 1.40, 1.40, 9, 1.50, 1.50, 10, 1.50, 1.50, 11, 2.50, 2.50,
		12, 4, 4, 13, 6, 6, 14, 8.50, 8.50, 15, 11.50, 11.50,
		16, 15, 15, 17, 16, 16, 18, 16, 16,
	}
	got := parseCurrentUpgradeCostWeightByUpgradeLevel(live)
	if len(got) != 19 || got[0] != 0.50 || got[10] != 1.50 || got[11] != 2.50 || got[14] != 8.50 || got[18] != 16 {
		t.Fatalf("live section folded to %v", got)
	}
	for _, testCase := range []struct {
		name   string
		values []float64
	}{
		{name: "absent", values: nil},
		{name: "not triples", values: []float64{0, 0.5, 1, 0.5}},
		{name: "level gap", values: []float64{0, 0.5, 0.5, 2, 0.5, 0.5}},
		{name: "does not start at zero", values: []float64{1, 0.5, 0.5, 2, 0.5, 0.5}},
		{name: "zero weight", values: []float64{0, 0.5, 0.5, 1, 0, 0}},
		{name: "negative weight", values: []float64{0, 0.5, 0.5, 1, -1, -1}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if got := parseCurrentUpgradeCostWeightByUpgradeLevel(testCase.values); got != nil {
				t.Fatalf("folded %v to %v, want nil", testCase.values, got)
			}
		})
	}
}

func TestCostWeightForCurrentLevel(t *testing.T) {
	absent := &currentUpgradeTable{}
	if got := absent.costWeightForCurrentLevel(10); got != 1 {
		t.Fatalf("section-less table coefficient=%v want 1", got)
	}
	table := &currentUpgradeTable{CostWeightByUpgradeLevel: []float64{0.5, 1.0, 2.5}}
	for _, testCase := range []struct {
		level int
		want  float64
	}{
		{level: -1, want: 0.5},
		{level: 0, want: 0.5},
		{level: 1, want: 1.0},
		{level: 2, want: 2.5},
		{level: 3, want: 2.5},
		{level: 40, want: 2.5},
	} {
		if got := table.costWeightForCurrentLevel(testCase.level); got != testCase.want {
			t.Fatalf("current level %d coefficient=%v want %v", testCase.level, got, testCase.want)
		}
	}
}

func TestCurrentUpgradePenaltyForAttemptUsesSharedReinforcementAndAmplificationBoundary(t *testing.T) {
	table := &currentUpgradeTable{DestroyLevelByRarity: map[string]int{"common": 7}}

	for _, tc := range []struct {
		name         string
		mode         string
		currentLevel int
		row          currentUpgradeTableRow
		wantType     int
		wantValue    int
	}{
		{name: "below downgrade boundary keeps PVF", mode: "reinforce", currentLevel: 6, row: currentUpgradeTableRow{PenaltyType: 0, PenaltyValue: 4}, wantType: 0, wantValue: 4},
		{name: "plus seven failure drops to six", mode: "reinforce", currentLevel: 7, row: currentUpgradeTableRow{PenaltyType: 3}, wantType: 1},
		{name: "plus eight failure drops to seven", mode: "reinforce", currentLevel: 8, row: currentUpgradeTableRow{PenaltyType: 3}, wantType: 1},
		{name: "plus nine failure drops to eight", mode: "reinforce", currentLevel: 9, row: currentUpgradeTableRow{PenaltyType: 3}, wantType: 1},
		{name: "plus ten failure destroys", mode: "reinforce", currentLevel: 10, row: currentUpgradeTableRow{PenaltyType: 2, PenaltyValue: 3}, wantType: 3},
		{name: "above plus ten failure destroys", mode: "reinforce", currentLevel: 11, row: currentUpgradeTableRow{PenaltyType: 2, PenaltyValue: 3}, wantType: 3},
		{name: "plus seven amplification failure drops to six", mode: "amplify", currentLevel: 7, row: currentUpgradeTableRow{PenaltyType: 2, PenaltyValue: 7}, wantType: 1},
		{name: "plus eight amplification failure drops to seven", mode: "amplify", currentLevel: 8, row: currentUpgradeTableRow{PenaltyType: 2, PenaltyValue: 8}, wantType: 1},
		{name: "plus nine amplification failure drops to eight", mode: "amplify", currentLevel: 9, row: currentUpgradeTableRow{PenaltyType: 2, PenaltyValue: 9}, wantType: 1},
		{name: "plus ten amplification failure destroys", mode: "amplify", currentLevel: 10, row: currentUpgradeTableRow{PenaltyType: 3}, wantType: 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gotType, gotValue := currentUpgradePenaltyForAttempt(tc.mode, tc.currentLevel, tc.row, table)
			if gotType != tc.wantType || gotValue != tc.wantValue {
				t.Fatalf("penalty=(%d,%d), want (%d,%d)", gotType, gotValue, tc.wantType, tc.wantValue)
			}
		})
	}
}
