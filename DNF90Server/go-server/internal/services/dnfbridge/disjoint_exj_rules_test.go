package dnfbridge

import (
	"testing"

	dnfexpertjob "longheng.io/server/internal/modules/dnf/expertjob"
	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

// fakeDisjointerRules carries the live disjointer.exj rows that ordinary
// disassembly borrows: machine-grade-0 rarity-5 row `0 5 0 3037 0.54 5 5 9`,
// table 5 (混沌魔石 ≤66 / 虚空魔石碎片 67+), its big win (混沌魔石 / 虚空魔石)
// and the otherverse table 6 (浓缩的异界精髓 30% / 矛盾的结晶体 70%, divisor 100).
type fakeDisjointerRules struct{}

func (fakeDisjointerRules) ResultRule(machineGradeColumn, rarity, state int) (dnfexpertjob.DisjointerResultRule, bool) {
	if machineGradeColumn == 0 && rarity == 5 && state == 0 {
		return dnfexpertjob.DisjointerResultRule{ItemID: 3037, Multiplier: 0.54, AdditionalTable: 5, BigWinTable: 5, BigWinChancePercent: 9}, true
	}
	return dnfexpertjob.DisjointerResultRule{}, false
}

func (fakeDisjointerRules) ChronicleSetResult() (int64, int64, bool) {
	return 3311, 20, true
}

func selectFakeDisjointerRule(rows []dnfexpertjob.DisjointerSelectionRule, level int, rng dnfexpertjob.RandomFunc) (dnfexpertjob.DisjointerSelectionRule, bool) {
	eligible := make([]dnfexpertjob.DisjointerSelectionRule, 0, len(rows))
	total := 0
	for _, row := range rows {
		if level >= row.MinimumGrade && level <= row.MaximumGrade {
			eligible = append(eligible, row)
			total += row.Weight
		}
	}
	if total == 0 {
		return dnfexpertjob.DisjointerSelectionRule{}, false
	}
	roll := rng(total)
	for _, row := range eligible {
		if roll < row.Weight {
			return row, true
		}
		roll -= row.Weight
	}
	return eligible[len(eligible)-1], true
}

func (fakeDisjointerRules) SelectAdditional(table, level int, rng dnfexpertjob.RandomFunc) (dnfexpertjob.DisjointerSelectionRule, bool) {
	switch table {
	case 5:
		return selectFakeDisjointerRule([]dnfexpertjob.DisjointerSelectionRule{
			{MinimumGrade: 0, MaximumGrade: 66, ItemID: 3228, Weight: 10000, CountDivisor: 64},
			{MinimumGrade: 67, MaximumGrade: 99, ItemID: 3300, Weight: 10000, CountDivisor: 14},
		}, level, rng)
	case 6:
		return selectFakeDisjointerRule([]dnfexpertjob.DisjointerSelectionRule{
			{MinimumGrade: 0, MaximumGrade: 99, ItemID: 3245, Weight: 3000, CountDivisor: 100},
			{MinimumGrade: 0, MaximumGrade: 99, ItemID: 3242, Weight: 7000, CountDivisor: 100},
		}, level, rng)
	}
	return dnfexpertjob.DisjointerSelectionRule{}, false
}

func (fakeDisjointerRules) SelectBigWin(table, level int, rng dnfexpertjob.RandomFunc) (dnfexpertjob.DisjointerSelectionRule, bool) {
	if table != 5 {
		return dnfexpertjob.DisjointerSelectionRule{}, false
	}
	return selectFakeDisjointerRule([]dnfexpertjob.DisjointerSelectionRule{
		{MinimumGrade: 0, MaximumGrade: 66, ItemID: 3228, Weight: 10000, CountDivisor: 12},
		{MinimumGrade: 67, MaximumGrade: 99, ItemID: 3299, Weight: 10000, CountDivisor: 18},
	}, level, rng)
}

// chronicleDisjointConfig mirrors the live etc/disjoint.etc rows that matter
// for rarity 5: cube multiplier 0.70, additional 3229 (混沌魔石碎片) at 6%+15%,
// soul expansion disabled.
func chronicleDisjointConfig() currentDisjointPVFConfig {
	return currentDisjointPVFConfig{
		CubeItemID: 3037, CubeBase: 150, EquipmentSellRate: 1000,
		CubeMultipliers: []float64{1.12, 1.35, 0.07, 1.48, 1.80, 0.70, 1.53},
		Additional:      [][]uint32{{}, {3033}, {3166}, {3167}, {}, {3229}, {3332}},
		AdditionalConsts: []currentDisjointAdditionalConst{
			{}, {0.30, 8.70, 8, 5}, {50, 50, 8, 10}, {0.19, 5.65, 8, 20}, {}, {1.30, 5.50, 6, 15}, {0.19, 5.65, 8, 20},
		},
		Expands: make([]currentDisjointExpand, 7),
	}
}

func rewardCount(rewards []currentDisjointReward, itemID uint32) (uint32, bool) {
	for _, reward := range rewards {
		if reward.ItemID == itemID {
			return reward.Count, true
		}
	}
	return 0, false
}

func TestCurrentEquipmentDisjointChronicleFollowsDisjointerVoidStoneTables(t *testing.T) {
	document, err := dnfpvf.Parse("equipment/chronicle70.equ", "[rarity]\n5\n[minimum level]\n70\n[value]\n30000\n")
	if err != nil {
		t.Fatal(err)
	}
	config := chronicleDisjointConfig()
	sawFragments, sawStones := false, false
	for run := 0; run < 600; run++ {
		rewards, err := currentEquipmentDisjointRewardsForSource(config, fakeDisjointerRules{}, 105000, document, dnfrepo.ItemStack{})
		if err != nil {
			t.Fatal(err)
		}
		if cubes, ok := rewardCount(rewards, 3037); !ok || cubes == 0 {
			t.Fatalf("run %d rewards=%+v lost the cube baseline", run, rewards)
		}
		if _, legacy := rewardCount(rewards, 3229); legacy {
			t.Fatalf("run %d rewards=%+v still rolled the etc 混沌魔石碎片 row", run, rewards)
		}
		if _, refund := rewardCount(rewards, 3311); refund {
			t.Fatalf("run %d rewards=%+v granted the destruction-only 异次元碎片 refund on disassembly", run, rewards)
		}
		fragments, hasFragments := rewardCount(rewards, 3300)
		stones, hasStones := rewardCount(rewards, 3299)
		switch {
		case hasFragments && !hasStones:
			// 70 / 14 = 5 exactly, no fractional roll.
			if fragments != 5 {
				t.Fatalf("run %d 虚空魔石碎片=%d want 5", run, fragments)
			}
			sawFragments = true
		case hasStones && !hasFragments:
			// Big win: 70 / 18 = 3.89 -> 3 or 4.
			if stones != 3 && stones != 4 {
				t.Fatalf("run %d 虚空魔石=%d want 3 or 4", run, stones)
			}
			sawStones = true
		default:
			t.Fatalf("run %d rewards=%+v want exactly one of 虚空魔石碎片 / 虚空魔石", run, rewards)
		}
	}
	if !sawFragments || !sawStones {
		t.Fatalf("600 runs never produced both outcomes: fragments=%t stones=%t", sawFragments, sawStones)
	}
}

func TestCurrentEquipmentDisjointChronicleBelowLevel67RollsChaosStone(t *testing.T) {
	document, err := dnfpvf.Parse("equipment/chronicle60.equ", "[rarity]\n5\n[minimum level]\n60\n[value]\n20000\n")
	if err != nil {
		t.Fatal(err)
	}
	config := chronicleDisjointConfig()
	for run := 0; run < 200; run++ {
		rewards, err := currentEquipmentDisjointRewardsForSource(config, fakeDisjointerRules{}, 105000, document, dnfrepo.ItemStack{})
		if err != nil {
			t.Fatal(err)
		}
		for _, itemID := range []uint32{3229, 3300, 3299} {
			if _, present := rewardCount(rewards, itemID); present {
				t.Fatalf("run %d rewards=%+v carries %d for a level-60 chronicle piece", run, rewards, itemID)
			}
		}
		// 60 / 64 (or 60 / 12 on a big win): zero or one stone, or five.
		if stones, ok := rewardCount(rewards, 3228); ok && stones != 1 && stones != 5 {
			t.Fatalf("run %d 混沌魔石=%d", run, stones)
		}
	}
}

func TestCurrentEquipmentDisjointWithoutDisjointerRulesKeepsEtcRow(t *testing.T) {
	document, err := dnfpvf.Parse("equipment/chronicle70.equ", "[rarity]\n5\n[minimum level]\n70\n[value]\n30000\n")
	if err != nil {
		t.Fatal(err)
	}
	config := chronicleDisjointConfig()
	sawLegacy := false
	for run := 0; run < 400; run++ {
		rewards, err := currentEquipmentDisjointRewardsForSource(config, nil, 105000, document, dnfrepo.ItemStack{})
		if err != nil {
			t.Fatal(err)
		}
		for _, itemID := range []uint32{3300, 3299, 3228, 3311} {
			if _, present := rewardCount(rewards, itemID); present {
				t.Fatalf("run %d rewards=%+v used exj rows without a disjointer config", run, rewards)
			}
		}
		if _, legacy := rewardCount(rewards, 3229); legacy {
			sawLegacy = true
		}
	}
	if !sawLegacy {
		t.Fatal("400 runs never rolled the 21% etc 混沌魔石碎片 row")
	}
}

func TestCurrentEquipmentDisjointOtherverseAuraAddsEssenceRoll(t *testing.T) {
	document, err := dnfpvf.Parse("equipment/rare85.equ", "[rarity]\n2\n[minimum level]\n85\n[value]\n30000\n")
	if err != nil {
		t.Fatal(err)
	}
	config := chronicleDisjointConfig()
	plain := dnfrepo.ItemStack{ItemID: 5001, Count: 1}
	for run := 0; run < 200; run++ {
		rewards, err := currentEquipmentDisjointRewardsForSource(config, fakeDisjointerRules{}, 5001, document, plain)
		if err != nil {
			t.Fatal(err)
		}
		for _, itemID := range []uint32{3245, 3242, 3311} {
			if _, present := rewardCount(rewards, itemID); present {
				t.Fatalf("run %d rewards=%+v rolled otherverse or chronicle-set rows for a plain rare item", run, rewards)
			}
		}
	}
	sawEssence, sawCrystal, sawNothing := false, false, false
	for _, amplified := range []dnfrepo.ItemStack{
		{ItemID: 5001, Count: 1, Extra: map[string]string{"amplify_type": "1"}},
		{ItemID: 5001, Count: 1, RawEntry: append(make([]byte, 0x13), 0x83)},
	} {
		for run := 0; run < 400; run++ {
			rewards, err := currentEquipmentDisjointRewardsForSource(config, fakeDisjointerRules{}, 5001, document, amplified)
			if err != nil {
				t.Fatal(err)
			}
			essence, hasEssence := rewardCount(rewards, 3245)
			crystal, hasCrystal := rewardCount(rewards, 3242)
			if hasEssence && hasCrystal {
				t.Fatalf("run %d rewards=%+v rolled both otherverse rows", run, rewards)
			}
			// 85 / 100 = 0.85: one item on 85% of the rolls, nothing otherwise.
			switch {
			case hasEssence:
				if essence != 1 {
					t.Fatalf("run %d 浓缩的异界精髓=%d want 1", run, essence)
				}
				sawEssence = true
			case hasCrystal:
				if crystal != 1 {
					t.Fatalf("run %d 矛盾的结晶体=%d want 1", run, crystal)
				}
				sawCrystal = true
			default:
				sawNothing = true
			}
		}
	}
	if !sawEssence || !sawCrystal || !sawNothing {
		t.Fatalf("otherverse outcomes essence=%t crystal=%t nothing=%t, want all three", sawEssence, sawCrystal, sawNothing)
	}
}

func TestCurrentUpgradeDestroyExtraRewardsRefundChronicleSetPieces(t *testing.T) {
	chronicle := dungeonDropItemDefinition{ItemID: 105000, Kind: dungeonDropItemEquipment, Rarity: 5, MinimumLevel: 70}
	if got := currentUpgradeDestroyExtraRewards(chronicle, fakeDisjointerRules{}); len(got) != 1 || got[0].ItemID != 3311 || got[0].Count != 20 {
		t.Fatalf("chronicle destroy extra rewards=%+v want 3311x20", got)
	}
	if got := currentUpgradeDestroyExtraRewards(chronicle, nil); got != nil {
		t.Fatalf("without disjointer rules extra rewards=%+v", got)
	}
	for _, rarity := range []int64{0, 1, 2, 3, 4, 6} {
		other := chronicle
		other.Rarity = rarity
		if got := currentUpgradeDestroyExtraRewards(other, fakeDisjointerRules{}); got != nil {
			t.Fatalf("rarity %d destroy extra rewards=%+v want none", rarity, got)
		}
	}
}

func TestCurrentUpgradeDestroyBonusCountFollowsUpgradeEtcDisjointGates(t *testing.T) {
	table := &currentUpgradeTable{
		DisjointBonusItemID:     3243,
		DisjointEquipLevelConst: 40,
		DisjointLevelConst:      10,
		DisjointWeightByRarity: map[string]float64{
			"rare": 1, "unique": 1.20, "epic": 1.50, "chronicle": 1, "legendary": 1.30,
		},
		DisjointCorrectionGradeByRarity: map[string]float64{
			"common": 1, "uncommon": 1, "rare": 2, "unique": 4, "epic": 5, "chronicle": 3, "legendary": 4,
		},
	}
	tests := []struct {
		name   string
		level  int
		rarity int
		grade  int
		want   int
	}{
		{"epic +12 level 85", 12, 4, 85, 6},
		{"epic +13 level 85", 13, 4, 85, 13},
		{"rare +12 level 85", 12, 2, 85, 4},
		{"legendary +11 level 85", 11, 6, 85, 1},
		{"at the level base grants nothing", 10, 4, 85, 0},
		{"common has no weight", 12, 0, 85, 0},
		{"grade below the equip level const after correction", 12, 4, 44, 0},
		{"grade exactly at the equip level const after correction", 12, 4, 45, 6},
	}
	for _, test := range tests {
		if got := currentUpgradeDestroyBonusCount(table, test.level, test.rarity, test.grade); got != test.want {
			t.Fatalf("%s: count=%d want %d", test.name, got, test.want)
		}
	}
}
