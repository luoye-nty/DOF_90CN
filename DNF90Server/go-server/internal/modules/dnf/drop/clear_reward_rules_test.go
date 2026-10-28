package drop

import (
	"errors"
	"os"
	"testing"

	platformpvf "longheng.io/server/internal/platform/pvf"
)

func TestLoadClearRewardRulesPreservesPVFRowsAndResolvesCurrentFormula(t *testing.T) {
	rules, err := LoadClearRewardRules(clearRewardRuleTestSource{ClearRewardRulePath: validClearRewardRuleText()})
	if err != nil {
		t.Fatal(err)
	}
	if snapshot := rules.Snapshot(); snapshot != (ClearRewardRuleSnapshot{
		MapRateRows: 5, DifficultyBonusRows: 5, RarityThresholdRows: 7,
		PartyBonusRows: 4, GoldDifficultyRows: 5, GoldCardCostRows: 2, ItemReferenceRows: 2,
	}) {
		t.Fatalf("snapshot=%+v", snapshot)
	}
	plan := rules.PlanSource(60, 1, 2, 5)
	if !plan.ItemRateFound || !plan.DifficultyFound || !plan.PartyBonusFound ||
		!plan.GoldDifficultyFound || !plan.GoldCardCostFound || !plan.ItemReferenceFound ||
		plan.ItemRate != 6000 || plan.DifficultyBonus != 600 || plan.PartyBonus != 1.2 ||
		plan.GoldDifficultyRate != 1.38 || plan.GoldCardCost != 7180 ||
		plan.ItemReference != (ClearRewardItemReference{LookupKey: 60, ValueA: 7, ValueB: 3}) {
		t.Fatalf("plan=%+v", plan)
	}
	missing := rules.PlanSource(59, 5, 5, 6)
	if missing.ItemRateFound || missing.DifficultyFound || missing.PartyBonusFound ||
		missing.GoldDifficultyFound || missing.GoldCardCostFound || missing.ItemReferenceFound {
		t.Fatalf("missing lookup fabricated rows: %+v", missing)
	}
	raw := rules.RawTables()
	if raw.ItemRateByMapCount[5] != 6000 || raw.DifficultyDropBonus != ([5]int64{0, 600, 1000, 1400, 1200}) ||
		raw.RarityDecision != ([7]int64{500000, 799900, 999900, 1000000, 1000001, 1000002, 1000003}) ||
		raw.PartyMemberDropBonus != ([4]float64{1, 1.2, 1.4, 1.6}) ||
		raw.GoldDifficultyDropBonus != ([5]float64{1.02, 1.38, 1.6, 1.9, 2}) || raw.GoldCardCostByLevel[60] != 7180 {
		t.Fatalf("raw=%+v", raw)
	}
	raw.ItemRateByMapCount[5] = 1
	raw.GoldCardCostByLevel[60] = 1
	again := rules.RawTables()
	if again.ItemRateByMapCount[5] != 6000 || again.GoldCardCostByLevel[60] != 7180 {
		t.Fatalf("rules leaked mutable tables: %+v", again)
	}
	resolved, err := rules.Resolve(60, 1, 2, 9)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.EquipmentRate != 10_000 || resolved.GradeDown != 7 || resolved.GradeUp != 3 ||
		resolved.RarityThresholds != ([7]uint32{500000, 799900, 999900, 1000000, 1000001, 1000002, 1000003}) ||
		resolved.RarityScale != 1000003 || resolved.PaidCardCost != 7180 ||
		resolved.GoldDifficultyRate != 1.38 || resolved.MapCount != 5 {
		t.Fatalf("resolved=%+v", resolved)
	}
	if _, err := rules.Resolve(59, 0, 1, 1); !errors.Is(err, ErrClearRewardRuleLookupMissing) {
		t.Fatalf("missing resolved lookup error=%v", err)
	}
}

func TestLoadClearRewardRulesRejectsMalformedPVFShapes(t *testing.T) {
	if _, err := LoadClearRewardRules(nil); !errors.Is(err, ErrClearRewardRuleSourceRequired) {
		t.Fatalf("nil source error=%v", err)
	}
	tests := []struct {
		name   string
		mutate func(string) string
	}{
		{
			name: "map counts must be complete and ordered",
			mutate: func(text string) string {
				return replaceOnce(t, text, "1 2000 2 3000", "1 2000 3 3000")
			},
		},
		{
			name: "difficulty rows must remain five",
			mutate: func(text string) string {
				return replaceOnce(t, text, "0 600 1000 1400 1200", "0 600 1000 1400")
			},
		},
		{
			name: "rarity thresholds cannot decrease",
			mutate: func(text string) string {
				return replaceOnce(t, text, "500000 799900 999900", "500000 400000 999900")
			},
		},
		{
			name: "party bonus must remain positive",
			mutate: func(text string) string {
				return replaceOnce(t, text, "1.00 1.20 1.40 1.60", "1.00 0 1.40 1.60")
			},
		},
		{
			name: "gold card level keys must increase",
			mutate: func(text string) string {
				return replaceOnce(t, text, "1 100 60 7180", "60 7180 1 100")
			},
		},
		{
			name: "item references require nonnegative values",
			mutate: func(text string) string {
				return replaceOnce(t, text, "60 7 3", "60 -1 3")
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := LoadClearRewardRules(clearRewardRuleTestSource{ClearRewardRulePath: test.mutate(validClearRewardRuleText())})
			if !errors.Is(err, ErrClearRewardRuleSectionShape) {
				t.Fatalf("error=%v", err)
			}
		})
	}
}

func TestRealScriptPVFClearRewardRulesPreserveCurrentSourceTables(t *testing.T) {
	pvfPath := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if pvfPath == "" {
		pvfPath = os.Getenv("DNF_WORLDMAP_REAL_PVF_SMOKE")
	}
	if pvfPath == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE to run real clear-reward PVF smoke")
	}
	archive, err := platformpvf.Open(pvfPath)
	if err != nil {
		t.Fatal(err)
	}
	rules, err := LoadClearRewardRules(archive)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := rules.Snapshot()
	if snapshot.MapRateRows != 5 || snapshot.DifficultyBonusRows != 5 || snapshot.RarityThresholdRows != 7 ||
		snapshot.PartyBonusRows != 4 || snapshot.GoldDifficultyRows != 5 || snapshot.GoldCardCostRows == 0 || snapshot.ItemReferenceRows == 0 {
		t.Fatalf("real snapshot=%+v", snapshot)
	}
}

func validClearRewardRuleText() string {
	return `[reward item rate per map max count]
1 2000 2 3000 3 4000 4 5000 5 6000
[dungeon difficulty drop bonusrate]
0 600 1000 1400 1200
[basis of rarity dicision]
500000 799900 999900 1000000 1000001 1000002 1000003
[party member drop bonusrate]
1.00 1.20 1.40 1.60
[dungeon difficulty gold drop bonusrate]
1.02 1.38 1.60 1.90 2.00
[gold card cost table]
1 100 60 7180
[item drop ref table]
1 0 3 60 7 3
`
}

type clearRewardRuleTestSource map[string]string

func (source clearRewardRuleTestSource) ReadText(path string) (string, error) {
	if text, found := source[path]; found {
		return text, nil
	}
	return "", errors.New("missing test PVF path: " + path)
}
