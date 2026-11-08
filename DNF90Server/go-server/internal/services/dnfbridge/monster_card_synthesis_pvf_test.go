package dnfbridge

import (
	"os"
	"path/filepath"
	"testing"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

func synthesisTestConfig() currentMonsterCardSynthesisConfig {
	return currentMonsterCardSynthesisConfig{
		MixProbability: map[int]int64{
			0: 40000,
			1: 10000,
			2: 3000,
			3: 0,
		},
		BinderRates: map[int]int64{
			0: 100,
			1: 200,
			2: 300,
		},
		Results: map[int][]currentMonsterCardSynthesisWeightedItem{
			0: {{ItemID: 3619, Weight: 3450}, {ItemID: 3620, Weight: 6550}},
			1: {{ItemID: 3610, Weight: 1000}},
			2: {{ItemID: 3604, Weight: 1000}},
			3: {{ItemID: 3600, Weight: 1000}},
		},
	}
}

func TestCurrentMonsterCardSynthesisChanceUsesBinderRate(t *testing.T) {
	config := synthesisTestConfig()
	tests := []struct {
		binder, first, second int
		want                  int64
	}{
		{0, 0, 0, 40000},
		{1, 0, 0, 80000},
		{2, 0, 0, 100000},
		{0, 1, 1, 10000},
		{1, 1, 1, 20000},
		{2, 2, 2, 9000},
		{0, 0, 1, 40000},
	}
	for _, test := range tests {
		got, err := currentMonsterCardSynthesisChance(config, test.binder, test.first, test.second)
		if err != nil {
			t.Fatal(err)
		}
		if got != test.want {
			t.Fatalf("binder=%d rarity=%d+%d chance=%d want=%d", test.binder, test.first, test.second, got, test.want)
		}
	}
}

func TestCurrentMonsterCardSynthesisResultRarityMatchesPVFRules(t *testing.T) {
	tests := []struct {
		first, second int
		upgrade       bool
		want          int
	}{
		{0, 0, true, 1},
		{2, 2, false, 2},
		{3, 3, true, 3},
		{0, 2, true, 2},
		{0, 2, false, 1},
		{0, 1, false, 0},
	}
	for _, test := range tests {
		if got := currentMonsterCardSynthesisResultRarity(test.first, test.second, test.upgrade); got != test.want {
			t.Fatalf("rarity=%d+%d upgrade=%t got=%d want=%d", test.first, test.second, test.upgrade, got, test.want)
		}
	}
}

func TestCurrentMonsterCardSynthesisSelectsWeightedOutput(t *testing.T) {
	config := synthesisTestConfig()
	got, err := currentMonsterCardSynthesisSelectOutput(config, 0, func(int) int { return 3449 })
	if err != nil {
		t.Fatal(err)
	}
	if got != 3619 {
		t.Fatalf("first weighted output=%d want=3619", got)
	}
	got, err = currentMonsterCardSynthesisSelectOutput(config, 0, func(int) int { return 3450 })
	if err != nil {
		t.Fatal(err)
	}
	if got != 3620 {
		t.Fatalf("second weighted output=%d want=3620", got)
	}
}

func TestRealScriptPVFMonsterCardSynthesisPlanner(t *testing.T) {
	pvfPath := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if pvfPath == "" {
		pvfPath = filepath.Clean(filepath.Join(
			"..", "..", "..", "..", "runtime", "data", "dnf", "Script.pvf",
		))
	}
	archive, err := platformpvf.Open(pvfPath)
	if err != nil {
		t.Skipf("real Script.pvf unavailable: %v", err)
	}
	catalog, err := newPVFDungeonDropCatalog(archive)
	if err != nil {
		t.Fatal(err)
	}
	config, err := loadCurrentMonsterCardSynthesisConfig(archive)
	if err != nil {
		t.Fatal(err)
	}
	binder, err := catalog.ResolveItem(2600479)
	if err != nil {
		t.Fatal(err)
	}
	if !binder.MonsterCardBindOK || binder.MonsterCardBind != 0 {
		t.Fatalf("binder policy=%+v", binder)
	}
	plan, err := planCurrentMonsterCardSynthesis(
		catalog,
		archive,
		config,
		0,
		dnfrepo.ItemStack{ItemID: 2600479, Count: 1},
		[]dnfrepo.ItemStack{
			{ItemID: 3619, Count: 1},
			{ItemID: 3620, Count: 1},
		},
		func(int) int { return 0 },
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.CardRarities) != 2 ||
		plan.CardRarities[0] != 0 || plan.CardRarities[1] != 0 ||
		plan.ResultRarity != 1 || plan.Chance != 40000 ||
		!plan.UpgradeRoll || plan.Output.ItemID == 0 ||
		plan.Output.PVFPath == "" || plan.GoldCost != 10000 {
		t.Fatalf("real-PVF synthesis plan=%+v", plan)
	}

	// Current 90CN white binder type 2 plus the custom 10159187 monster card.
	// This card is absent from the legacy [card list] but carries its own
	// [rarity]=2, and the enchanter bind table must include type 2.
	binder, err = catalog.ResolveItem(2682575)
	if err != nil {
		t.Fatal(err)
	}
	plan, err = planCurrentMonsterCardSynthesis(
		catalog,
		archive,
		config,
		2,
		dnfrepo.ItemStack{ItemID: 2682575, Count: 1},
		[]dnfrepo.ItemStack{
			{ItemID: 10159187, Count: 1},
			{ItemID: 10159187, Count: 1},
		},
		func(int) int { return 0 },
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.CardRarities) != 2 ||
		plan.CardRarities[0] != 2 || plan.CardRarities[1] != 2 ||
		plan.Chance != 9000 || plan.GoldCost != 70000 ||
		plan.Output.ItemID == 0 || plan.Output.PVFPath == "" {
		t.Fatalf("real-PVF white-binder custom-card plan=%+v", plan)
	}
}
