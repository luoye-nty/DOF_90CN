package guild

import "testing"

// The layout file nests [dungeon] blocks with their own subsections, and the
// section names span several whitespace-separated fields, so the scanner is the
// risky part. This fixture is a trimmed copy of the real
// etc/guild/guilddungeon/0_2Way6Dungeon.etc.
const dungeonCatalogFixture = `
[weekly reward]

[final dungeon]
8508
[/weekly reward]

[dungeon]

[dungeon index]
8500
[gauge limit]
10000
[gauge on clear]
150
[conquest gauge ratio]
50
[color]
` + "`black`" + `
[type]
` + "`normal`" + `
[reward]

[gauge ratio]
99
[item]
10154439 3
[contribute]
20
[/gauge ratio]

[/reward]

[/dungeon]

[dungeon]

[dungeon index]
8504
[gauge limit]
10000
[gauge on clear]
75
[conquest gauge ratio]
50
[color]
` + "`red`" + `
[type]
` + "`normal`" + `
[required dungeon conquest]
8500
[/required dungeon conquest]

[/dungeon]

[dungeon]

[dungeon index]
8503
[gauge limit]
10000
[gauge on clear]
75
[conquest gauge ratio]
50
[color]
` + "`blue`" + `
[type]
` + "`normal`" + `

[/dungeon]

[dungeon]

[dungeon index]
8505
[gauge limit]
10000
[gauge on clear]
50
[conquest gauge ratio]
50
[color]
` + "`red`" + `
[type]
` + "`hard`" + `

[/dungeon]

[dungeon]

[dungeon index]
8508
[gauge limit]
10000
[gauge on clear]
30
[conquest gauge ratio]
100
[color]
` + "`red`" + `
[type]
` + "`final`" + `
[required dungeon conquest]
8503 8505
[/required dungeon conquest]

[/dungeon]
`

func TestParseDungeonCatalogReadsStagesAndPrerequisites(t *testing.T) {
	catalog, err := parseDungeonCatalog(dungeonCatalogFixture)
	if err != nil {
		t.Fatal(err)
	}
	if catalog.FinalDungeon != 8508 {
		t.Fatalf("final dungeon=%d want 8508", catalog.FinalDungeon)
	}
	if len(catalog.Stages) != 5 {
		t.Fatalf("stages=%d want 5", len(catalog.Stages))
	}

	first, ok := catalog.Stage(8500)
	if !ok {
		t.Fatal("8500 missing")
	}
	if first.GaugeLimit != 10000 || first.GaugeOnClear != 150 || first.ConquestGaugeRatio != 50 {
		t.Fatalf("8500=%+v", first)
	}
	if first.Color != "black" || first.Type != "normal" {
		t.Fatalf("8500 color=%q type=%q", first.Color, first.Type)
	}
	// The reward block's own numbers (99, 10154439, 3, 20) must not leak into
	// the stage: they sit inside [reward] and belong to a different scope.
	if len(first.RequiredConquest) != 0 {
		t.Fatalf("8500 must have no prerequisite, got %v", first.RequiredConquest)
	}
	// 50% of 10000.
	if got := first.ConquestGauge(); got != 5000 {
		t.Fatalf("8500 conquest gauge=%d want 5000", got)
	}

	second, _ := catalog.Stage(8504)
	if len(second.RequiredConquest) != 1 || second.RequiredConquest[0] != 8500 {
		t.Fatalf("8504 prerequisites=%v want [8500]", second.RequiredConquest)
	}
	final, _ := catalog.Stage(8508)
	if len(final.RequiredConquest) != 2 || final.RequiredConquest[0] != 8503 || final.RequiredConquest[1] != 8505 {
		t.Fatalf("8508 prerequisites=%v want [8503 8505]", final.RequiredConquest)
	}
	if got := final.ConquestGauge(); got != 10000 {
		t.Fatalf("8508 conquest gauge=%d want 10000 (100%%)", got)
	}
}

// The unlock rule is what the client's own gate mirrors: a stage opens only
// once every prerequisite has reached its conquest threshold.
func TestDungeonCatalogUnlockFollowsConquest(t *testing.T) {
	catalog, err := parseDungeonCatalog(dungeonCatalogFixture)
	if err != nil {
		t.Fatal(err)
	}
	gauges := map[uint32]uint32{}

	// No prerequisites: always open, which is why an empty guild still sees 伤城.
	if !catalog.Unlocked(gauges, 8500) {
		t.Fatal("8500 must be open with no gauges")
	}
	if catalog.Unlocked(gauges, 8504) {
		t.Fatal("8504 must be shut before 8500 is conquered")
	}
	// One clear is not a conquest: 150 of the 5000 needed.
	gauges[8500] = 150
	if catalog.Unlocked(gauges, 8504) {
		t.Fatal("8504 opened after a single clear of 8500")
	}
	gauges[8500] = 4999
	if catalog.Unlocked(gauges, 8504) {
		t.Fatal("8504 opened one point short of the threshold")
	}
	gauges[8500] = 5000
	if !catalog.Unlocked(gauges, 8504) {
		t.Fatal("8504 still shut at exactly the threshold")
	}

	// The final stage needs both prerequisites, and at 100%.
	gauges[8503] = 10000
	if catalog.Unlocked(gauges, 8508) {
		t.Fatal("8508 opened with only one prerequisite conquered")
	}
	// 8505 conquers at 50% of 10000, so one point short must still block.
	gauges[8505] = 4999
	if catalog.Unlocked(gauges, 8508) {
		t.Fatal("8508 opened while 8505 was below its own threshold")
	}
	gauges[8505] = 5000
	if !catalog.Unlocked(gauges, 8508) {
		t.Fatal("8508 shut with both prerequisites conquered")
	}
}

func TestAddDungeonGaugeClampsToTheStageLimit(t *testing.T) {
	registry := NewRegistry()
	registry.Guilds[1] = &Guild{ID: 1}
	// The pair is (before, after): the caller needs both to tell which reward
	// thresholds this clear actually crossed.
	if previous, gauge, err := registry.AddDungeonGauge(1, 8500, 150, 10000); err != nil || previous != 0 || gauge != 150 {
		t.Fatalf("first clear %d->%d err=%v want 0->150", previous, gauge, err)
	}
	if previous, gauge, err := registry.AddDungeonGauge(1, 8500, 150, 10000); err != nil || previous != 150 || gauge != 300 {
		t.Fatalf("second clear %d->%d err=%v want 150->300", previous, gauge, err)
	}
	if _, gauge, err := registry.AddDungeonGauge(1, 8500, 100_000, 10000); err != nil || gauge != 10000 {
		t.Fatalf("clamped gauge=%d err=%v want 10000", gauge, err)
	}
	if _, _, err := registry.AddDungeonGauge(99, 8500, 1, 10000); err == nil {
		t.Fatal("an unknown guild must not accumulate a gauge")
	}
}

// Rewards are paid on crossing a [gauge ratio] line, once. A repeat clear past
// the line must not pay again, and a clear that jumps several lines at once
// pays each of them.
func TestRewardsCrossedPaysEachThresholdOnce(t *testing.T) {
	stage := DungeonStage{
		GaugeLimit:   10000,
		GaugeOnClear: 150,
		Rewards: []DungeonReward{
			{GaugeRatio: 99, ItemID: 10154439, ItemCount: 3, Contribute: 20},
			{GaugeRatio: 100, ItemID: 10154439, ItemCount: 3, Contribute: 20},
		},
	}
	if got := stage.RewardsCrossed(0, 150); len(got) != 0 {
		t.Fatalf("an early clear paid %d rewards", len(got))
	}
	// 99% of 10000 is 9900.
	if got := stage.RewardsCrossed(9800, 9900); len(got) != 1 || got[0].GaugeRatio != 99 {
		t.Fatalf("crossing 99%% paid %+v", got)
	}
	// Already past it: nothing more until 100%.
	if got := stage.RewardsCrossed(9900, 9950); len(got) != 0 {
		t.Fatalf("staying above 99%% paid again: %+v", got)
	}
	if got := stage.RewardsCrossed(9950, 10000); len(got) != 1 || got[0].GaugeRatio != 100 {
		t.Fatalf("crossing 100%% paid %+v", got)
	}
	// One clear that jumps both lines pays both.
	if got := stage.RewardsCrossed(0, 10000); len(got) != 2 {
		t.Fatalf("a full jump paid %d rewards, want 2", len(got))
	}
}

// The claim marker is guild-wide and survives within the week, so a second
// member cannot collect the same threshold.
func TestClaimDungeonRewardIsOncePerGuildPerThreshold(t *testing.T) {
	registry := NewRegistry()
	registry.Guilds[1] = &Guild{ID: 1}
	claimed, err := registry.ClaimDungeonReward(1, 8500, 99)
	if err != nil || !claimed {
		t.Fatalf("first claim=%t err=%v", claimed, err)
	}
	claimed, err = registry.ClaimDungeonReward(1, 8500, 99)
	if err != nil || claimed {
		t.Fatalf("second claim=%t err=%v, must not pay twice", claimed, err)
	}
	// A different threshold, and a different stage, are separate claims.
	if claimed, _ := registry.ClaimDungeonReward(1, 8500, 100); !claimed {
		t.Fatal("the 100%% threshold was blocked by the 99%% claim")
	}
	if claimed, _ := registry.ClaimDungeonReward(1, 8504, 99); !claimed {
		t.Fatal("another stage was blocked by the first stage's claim")
	}
}

// A new week wipes gauges and claims so the cycle restarts; the same week is a
// no-op, which is what keeps a clear from resetting the bar it just raised.
func TestResetDungeonWeekClearsProgressOnlyOnANewWeek(t *testing.T) {
	registry := NewRegistry()
	registry.Guilds[1] = &Guild{ID: 1}
	if _, _, err := registry.AddDungeonGauge(1, 8500, 9000, 10000); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.ClaimDungeonReward(1, 8500, 99); err != nil {
		t.Fatal(err)
	}

	// First settle of a fresh guild stamps the week without wiping anything.
	if reset, err := registry.ResetDungeonWeek(1, 100); err != nil || !reset {
		t.Fatalf("first settle reset=%t err=%v", reset, err)
	}
	if len(registry.DungeonGauges(1)) != 0 {
		t.Fatal("the first settle should have cleared the pre-existing progress")
	}

	if _, _, err := registry.AddDungeonGauge(1, 8500, 600, 10000); err != nil {
		t.Fatal(err)
	}
	// Same week: progress must survive.
	if reset, err := registry.ResetDungeonWeek(1, 100); err != nil || reset {
		t.Fatalf("same-week reset=%t err=%v", reset, err)
	}
	if registry.DungeonGauges(1)[8500] != 600 {
		t.Fatalf("same-week settle wiped the gauge: %v", registry.DungeonGauges(1))
	}
	// Next week: wiped, claims too.
	if reset, err := registry.ResetDungeonWeek(1, 101); err != nil || !reset {
		t.Fatalf("new-week reset=%t err=%v", reset, err)
	}
	if len(registry.DungeonGauges(1)) != 0 {
		t.Fatalf("new week kept gauges: %v", registry.DungeonGauges(1))
	}
	if claimed, _ := registry.ClaimDungeonReward(1, 8500, 99); !claimed {
		t.Fatal("new week kept last week's reward claim")
	}
}
