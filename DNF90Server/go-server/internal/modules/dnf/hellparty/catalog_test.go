package hellparty

import (
	"errors"
	"testing"
)

const catalogFixture = `[difficulty]
4 ` + "`A`" + ` 8 5 100 100 0 ` + "`B`" + ` 6 4 0 0 0
[/difficulty]
[hellparty monster group]
[group index]
125
[group]
` + "`A`" + ` 1050 0
[/group]
[group index]
155
[group]
` + "`A`" + ` 20438 1 36404 1 ` + "`B`" + ` 30410 1
[/group]
[/hellparty monster group]`

func TestParseCatalog(t *testing.T) {
	catalog, err := Parse(catalogFixture)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	rule, ok := catalog.Difficulty("a")
	if !ok || rule.RewardRollCount != 8 || rule.Values != [5]int64{8, 5, 100, 100, 0} {
		t.Fatalf("Difficulty(A) = %+v, %v", rule, ok)
	}
	group, ok := catalog.Group(155)
	if !ok || len(group.Actors["A"]) != 2 || group.Actors["A"][0] != (ActorDefinition{Code: 20438, EntityType: 1}) {
		t.Fatalf("Group(155) = %+v, %v", group, ok)
	}
	group.Actors["A"][0].Code = 1
	again, _ := catalog.Group(155)
	if again.Actors["A"][0].Code != 20438 {
		t.Fatal("Group() returned mutable catalog storage")
	}
}

func TestParseCatalogRejectsMalformedActorPair(t *testing.T) {
	_, err := Parse(`[difficulty] 1 ` + "`A`" + ` 1 2 3 4 5 [/difficulty]
[group index] 1 [group] ` + "`A`" + ` 100 [/group]`)
	if !errors.Is(err, ErrCatalogInvalid) {
		t.Fatalf("Parse() error = %v, want ErrCatalogInvalid", err)
	}
}

func TestBuildPlanSelectsOneStableGroupPerOrder(t *testing.T) {
	catalog, err := Parse(catalogFixture)
	if err != nil {
		t.Fatal(err)
	}
	options := PlanOptions{
		GroupKey: "A",
		Seed:     0x12345678,
		Entries: []MapEntry{
			{GroupID: 125, Rate: 25, Order: 1},
			{GroupID: 155, Rate: 75, Order: 1},
			{GroupID: 155, Rate: 100, Order: 2},
		},
	}
	first, err := BuildPlan(catalog, options)
	if err != nil {
		t.Fatalf("BuildPlan() error = %v", err)
	}
	second, err := BuildPlan(catalog, options)
	if err != nil {
		t.Fatalf("BuildPlan() second error = %v", err)
	}
	if len(first.Waves) != 2 || first.Waves[0].Order != 1 || first.Waves[1].Order != 2 {
		t.Fatalf("waves = %+v", first.Waves)
	}
	if first.Waves[0].GroupID != second.Waves[0].GroupID {
		t.Fatalf("selection is not stable: %d != %d", first.Waves[0].GroupID, second.Waves[0].GroupID)
	}
}

func TestRuntimeDeathIsIdempotentAndReleasesPerGroup(t *testing.T) {
	catalog, err := Parse(catalogFixture)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := BuildPlan(catalog, PlanOptions{
		GroupKey: "A",
		Seed:     1,
		Entries:  []MapEntry{{GroupID: 155, Rate: 100, Order: 1}},
	})
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := NewRuntime(plan)
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.Start(); err != nil {
		t.Fatal(err)
	}
	first := plan.Waves[0].Actors[0].Identity
	second := plan.Waves[0].Actors[1].Identity
	result, err := runtime.RecordDeath(first)
	if err != nil || result.GroupCompleted || result.RewardRollCount != 0 {
		t.Fatalf("first death = %+v, %v", result, err)
	}
	duplicate, err := runtime.RecordDeath(first)
	if err != nil || !duplicate.Duplicate {
		t.Fatalf("duplicate death = %+v, %v", duplicate, err)
	}
	result, err = runtime.RecordDeath(second)
	if err != nil || !result.GroupCompleted || !result.AllComplete || result.RewardRollCount != 8 {
		t.Fatalf("final death = %+v, %v", result, err)
	}
}

func TestRuntimeReleasesEachOrderIndependently(t *testing.T) {
	catalog, err := Parse(catalogFixture)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := BuildPlan(catalog, PlanOptions{
		GroupKey: "A",
		Seed:     2,
		Entries: []MapEntry{
			{GroupID: 155, Rate: 100, Order: 1},
			{GroupID: 155, Rate: 100, Order: 2},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := NewRuntime(plan)
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.Start(); err != nil {
		t.Fatal(err)
	}
	for waveIndex, wave := range plan.Waves {
		for actorIndex, actor := range wave.Actors {
			result, recordErr := runtime.RecordDeath(actor.Identity)
			if recordErr != nil {
				t.Fatalf("wave=%d actor=%d: %v", waveIndex, actorIndex, recordErr)
			}
			lastActor := actorIndex == len(wave.Actors)-1
			if result.GroupCompleted != lastActor {
				t.Fatalf("wave=%d actor=%d result=%+v", waveIndex, actorIndex, result)
			}
			if lastActor && result.RewardRollCount != 8 {
				t.Fatalf("wave=%d reward rolls=%d, want 8", waveIndex, result.RewardRollCount)
			}
			lastWave := waveIndex == len(plan.Waves)-1
			if result.AllComplete != (lastWave && lastActor) {
				t.Fatalf("wave=%d actor=%d all_complete=%t", waveIndex, actorIndex, result.AllComplete)
			}
		}
	}
}

func TestRuntimeSuppressesRewardWhenHellMonsterDies(t *testing.T) {
	catalog, err := Parse(catalogFixture)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := BuildPlan(catalog, PlanOptions{
		GroupKey: "A",
		Seed:     3,
		Entries:  []MapEntry{{GroupID: 125, Rate: 100, Order: 1}},
		SuppressGroupReward: func(code uint32, entityType uint8) bool {
			return code == 1050 && entityType == 0
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := NewRuntime(plan)
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.Start(); err != nil {
		t.Fatal(err)
	}
	result, err := runtime.RecordDeath(plan.Waves[0].Actors[0].Identity)
	if err != nil {
		t.Fatal(err)
	}
	if !result.GroupCompleted || !result.AllComplete || !result.RewardSuppressed || result.RewardRollCount != 0 {
		t.Fatalf("result=%+v", result)
	}
}
