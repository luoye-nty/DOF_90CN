package dnfbridge

import (
	"os"
	"testing"

	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

// The 强化器 popup is the contract: whatever it quotes is what the player
// expects to pay. These are the live readings taken on 2026-09-11 against the
// running client, and they pinned two defects at once -- the [cost] row was
// indexed by [minimum level] instead of [grade], and the per-target-level
// coefficient was dropped by the row parser entirely, so every level of a
// piece cost the same.
func TestCurrentUpgradeGoldCostMatchesTheClientPopup(t *testing.T) {
	pvfPath := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE to check op50 gold against the active PVF")
	}
	archive, err := platformpvf.Open(pvfPath)
	if err != nil {
		t.Fatal(err)
	}
	text, err := archive.ReadText(currentUpgradePVFPath)
	if err != nil {
		t.Fatal(err)
	}
	document, err := dnfpvf.Parse(currentUpgradePVFPath, text)
	if err != nil {
		t.Fatal(err)
	}
	table := parseCurrentUpgradeTable(document, text)

	for _, testCase := range []struct {
		name        string
		grade       int64
		rarity      int64
		targetLevel int
		want        int64
	}{
		// 密制锋利的印度古剑 101030286: grade 17, rarity 2, weapon. Levels 1 and
		// 2 share GoldWeight 0.02, which is why two consecutive upgrades both
		// quoted 2124 and made the cost look level-independent.
		{name: "grade 17 rarity 2 to +1", grade: 17, rarity: 2, targetLevel: 1, want: 2124},
		{name: "grade 17 rarity 2 to +2", grade: 17, rarity: 2, targetLevel: 2, want: 2124},
		// 戮蛊的断空巨剑 101010047: grade 90, rarity 6, weapon.
		{name: "grade 90 rarity 6 to +1", grade: 90, rarity: 6, targetLevel: 1, want: 199422},
		{name: "grade 90 rarity 6 to +15", grade: 90, rarity: 6, targetLevel: 15, want: 3390174},
		// 远古遗愿 28309: grade 69, rarity 3, weapon. Live 2026-09-25, character
		// 1: the log charged 84975 for +0->+1 and again for +1->+2, and the
		// player's 强化器 quoted 84975 both times. Both land on coefficient
		// 0.50, which is also what the retired [table type] weight * 25 curve
		// produced -- which is why the +2->+3 row below is the one that was
		// silently double-priced (169950 charged against an 84975 popup).
		{name: "grade 69 rarity 3 to +1", grade: 69, rarity: 3, targetLevel: 1, want: 84975},
		{name: "grade 69 rarity 3 to +2", grade: 69, rarity: 3, targetLevel: 2, want: 84975},
		{name: "grade 69 rarity 3 to +3", grade: 69, rarity: 3, targetLevel: 3, want: 84975},
		{name: "grade 69 rarity 3 to +11", grade: 69, rarity: 3, targetLevel: 11, want: 254925},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			definition := dungeonDropItemDefinition{
				ItemID:        1,
				Kind:          dungeonDropItemEquipment,
				Grade:         testCase.grade,
				Rarity:        testCase.rarity,
				EquipmentType: "[weapon]",
			}
			got, err := calculateCurrentUpgradeGoldCost(table, definition, testCase.targetLevel)
			if err != nil {
				t.Fatal(err)
			}
			if got != testCase.want {
				t.Fatalf("gold=%d want %d (client popup)", got, testCase.want)
			}
		})
	}

	// The cost has to grow with the target level; a flat curve is the old bug.
	low, err := calculateCurrentUpgradeGoldCost(table, dungeonDropItemDefinition{
		ItemID: 1, Kind: dungeonDropItemEquipment, Grade: 90, Rarity: 6, EquipmentType: "[weapon]",
	}, 3)
	if err != nil {
		t.Fatal(err)
	}
	high, err := calculateCurrentUpgradeGoldCost(table, dungeonDropItemDefinition{
		ItemID: 1, Kind: dungeonDropItemEquipment, Grade: 90, Rarity: 6, EquipmentType: "[weapon]",
	}, 12)
	if err != nil {
		t.Fatal(err)
	}
	if high <= low {
		t.Fatalf("gold did not grow with target level: +3=%d +12=%d", low, high)
	}
}

// TestRealPVFAmplifyGoldCostMatchesTheClientPopup is the 增幅 half of the same
// contract, and the reading that exposed the coefficient bug.
//
// Live 2026-09-25, character 1, 远古遗愿 (item 28309, grade 69, rarity 3,
// [weapon]) at +10: the 增幅器 confirmation dialog read
//
//	消耗材料   220500 金币
//	           [矛盾的结晶体] 11个
//
// and the server charged 992250 -- exactly 4.5x. 4.5 was the retired
// [table type] weight (0.18) * 25; etc/amplifyupgrade.etc has no
// [cost weight by upgrade level] section, so the client applies no level factor
// at all and 220500 = [cost][69] * [cost weights by rarity][3] * [type][weapon]
// = 44100 * 2.50 * 2.00. The material count on the same row was 11 on both
// sides, which is what proves only the gold factor was wrong.
func TestRealPVFAmplifyGoldCostMatchesTheClientPopup(t *testing.T) {
	pvfPath := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE to check the 增幅器 popup price")
	}
	archive, err := platformpvf.Open(pvfPath)
	if err != nil {
		t.Fatal(err)
	}
	text, err := archive.ReadText(currentAmplifyUpgradePVFPath)
	if err != nil {
		t.Fatal(err)
	}
	document, err := dnfpvf.Parse(currentAmplifyUpgradePVFPath, text)
	if err != nil {
		t.Fatal(err)
	}
	table := parseCurrentUpgradeTable(document, text)

	// The whole path: resolve the real item, then price the attempt.
	service := &Service{options: options{pvfPath: pvfPath}}
	got, err := service.currentUpgradeGoldCost(table, 28309, 11)
	if err != nil {
		t.Fatal(err)
	}
	if got != 220_500 {
		t.Fatalf("amplify +10->+11 gold=%d want 220500 (client popup)", got)
	}

	// The same row, everything except gold, must be untouched by this change.
	row, ok := table.RowForLevel(11)
	if !ok {
		t.Fatal("amplify table is missing row 11")
	}
	if row.MaterialItemID != 3242 || row.MaterialCount != 11 {
		t.Fatalf("amplify row 11 material=%d x%d, want 矛盾的结晶体 x11", row.MaterialItemID, row.MaterialCount)
	}
	if row.FailureWeight != 60000 || row.PenaltyType != 3 {
		t.Fatalf("amplify row 11 failure=%d penalty=%d, want 60000 and 3 (destroy)", row.FailureWeight, row.PenaltyType)
	}
	if row.SuccessWeight() != 40000 {
		t.Fatalf("amplify row 11 success weight=%d want 40000", row.SuccessWeight())
	}
	if len(table.CostWeightByUpgradeLevel) != 0 {
		t.Fatalf("amplifyupgrade.etc grew a [cost weight by upgrade level] section: %v", table.CostWeightByUpgradeLevel)
	}
}

// TestRealPVFUpgradeCostWeightByUpgradeLevelMatchesTheClient pins the section the
// client actually prices with, straight off the live PVF. These 19 values are
// what the retired [table type] weight * 25 curve only matched at 0, 1 and 14.
func TestRealPVFUpgradeCostWeightByUpgradeLevelMatchesTheClient(t *testing.T) {
	pvfPath := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE to inspect runtime upgrade.etc")
	}
	archive, err := platformpvf.Open(pvfPath)
	if err != nil {
		t.Fatal(err)
	}
	text, err := archive.ReadText(currentUpgradePVFPath)
	if err != nil {
		t.Fatal(err)
	}
	document, err := dnfpvf.Parse(currentUpgradePVFPath, text)
	if err != nil {
		t.Fatal(err)
	}
	table := parseCurrentUpgradeTable(document, text)
	want := []float64{
		0.50, 0.50, 0.50, 0.50, 1, 1.10, 1.20, 1.30, 1.40, 1.50,
		1.50, 2.50, 4, 6, 8.50, 11.50, 15, 16, 16,
	}
	if len(table.CostWeightByUpgradeLevel) != len(want) {
		t.Fatalf("coefficients=%v, want %d entries", table.CostWeightByUpgradeLevel, len(want))
	}
	for level, expected := range want {
		if got := table.costWeightForCurrentLevel(level); got != expected {
			t.Fatalf("current level %d coefficient=%v want %v", level, got, expected)
		}
	}
	// +15->+16 is the first level where the retired curve under-charged (9.5 vs
	// the client's 11.5); +2->+3 is where it double-charged.
	if got := table.costWeightForCurrentLevel(15); got != 11.50 {
		t.Fatalf("+15->+16 coefficient=%v want 11.50", got)
	}
	if got := table.costWeightForCurrentLevel(2); got != 0.50 {
		t.Fatalf("+2->+3 coefficient=%v want 0.50", got)
	}
}
