package raid

import (
	"path/filepath"
	"reflect"
	"testing"

	platformpvf "longheng.io/server/internal/platform/pvf"
)

func TestRealScriptPVFLukeSingleRoutes(t *testing.T) {
	pvfPath := filepath.Clean(filepath.Join("..", "..", "..", "..", "..", "runtime", "data", "dnf", "Script.pvf"))
	archive, err := platformpvf.Open(pvfPath)
	if err != nil {
		t.Skipf("runtime Script.pvf unavailable: %v", err)
	}
	config, err := LoadLukeConfig(archive)
	if err != nil {
		t.Fatalf("LoadLukeConfig error = %v", err)
	}
	if config.Raid == nil || config.Raid.SingleMode == nil {
		t.Fatal("Luke single config missing")
	}
	if config.DailyAccountEnterLimit != 10 || config.Raid.StartDelaySeconds != 3 ||
		config.Raid.EntryCost.ItemID != 10159649 || config.Raid.EntryCost.Count != 1 {
		t.Fatalf("Luke global config = %+v", config)
	}
	if got := config.LightRoute; len(got) != 6 || got[0] != 5323 || got[5] != 5328 {
		t.Fatalf("light route = %v", got)
	}
	if got := config.DarkRoute; len(got) != 6 || got[0] != 5329 || got[5] != 5334 {
		t.Fatalf("dark route = %v", got)
	}
}

// TestRealScriptPVFLukePhaseMechanismTables pins the phase-one object tables and
// the per-gate route classifiers the two-phase runtime owner needs. Every value
// here is read from etc/raid/luke/luke.etc.
func TestRealScriptPVFLukePhaseMechanismTables(t *testing.T) {
	pvfPath := filepath.Clean(filepath.Join("..", "..", "..", "..", "..", "runtime", "data", "dnf", "Script.pvf"))
	archive, err := platformpvf.Open(pvfPath)
	if err != nil {
		t.Skipf("runtime Script.pvf unavailable: %v", err)
	}
	config, err := LoadLukeConfig(archive)
	if err != nil {
		t.Fatalf("LoadLukeConfig error = %v", err)
	}
	if config.Raid == nil || len(config.Raid.Phases) != 2 {
		t.Fatalf("Luke ordinary phases = %+v", config.Raid)
	}
	if config.Raid.WeeklyEnterCount != [2]int{20, 2} {
		t.Fatalf("weekly enter count = %v want [20 2]", config.Raid.WeeklyEnterCount)
	}
	if config.SanctumTimerCount != 5 {
		t.Fatalf("sanctum timer count = %d want 5", config.SanctumTimerCount)
	}
	wantSanctums := []LukeSanctumObjectCount{
		{Section: "birth sanctum", Count: 5, DungeonID: 5054},
		{Section: "extinction sanctum", Count: 5, DungeonID: 5055},
		{Section: "spread sanctum", Count: 5, DungeonID: 5056},
	}
	if !reflect.DeepEqual(config.SanctumObjectCounts, wantSanctums) {
		t.Fatalf("sanctum object counts = %+v want %+v", config.SanctumObjectCounts, wantSanctums)
	}
	wantDestroy := []LukePhaseOneObjectDestroy{
		{Group: 0, Minimum: 0, Maximum: 5},
		{Group: 1, Minimum: 6, Maximum: 10},
		{Group: 2, Minimum: 11, Maximum: 15},
	}
	if !reflect.DeepEqual(config.PhaseOneObjectDestroy, wantDestroy) {
		t.Fatalf("1phase obj destroy = %+v want %+v", config.PhaseOneObjectDestroy, wantDestroy)
	}
	wantCards := []int{0, 0, 0, 3, 3, 3, 6, 6, 6, 9, 9, 9, 12, 12, 12}
	if !reflect.DeepEqual(config.CardSetMapInfo, wantCards) {
		t.Fatalf("card set map info = %v want %v", config.CardSetMapInfo, wantCards)
	}

	// Phase one declares no revive-coin cap and no minimum clear count; every
	// gate is a one-party sanctum carrying its own destructible object group.
	phaseOne := config.Raid.Phases[0]
	if phaseOne.CoinLimit != 0 || phaseOne.MinimalDungeonClearCount != 0 {
		t.Fatalf("phase one coin_limit=%d minimum_clear=%d", phaseOne.CoinLimit, phaseOne.MinimalDungeonClearCount)
	}
	wantObjects := map[uint32]string{
		5054: "birth sanctum object",
		5055: "extinction sanctum object",
		5056: "spread sanctum object",
	}
	if len(phaseOne.Dungeons) != len(wantObjects) {
		t.Fatalf("phase one dungeons = %+v", phaseOne.Dungeons)
	}
	for _, dungeon := range phaseOne.Dungeons {
		want, ok := wantObjects[dungeon.Index]
		if !ok {
			t.Fatalf("unexpected phase one dungeon=%d", dungeon.Index)
		}
		if dungeon.PartyMax != 1 || dungeon.HP != 3 || dungeon.LockoutType != [2]int{0, 0} {
			t.Fatalf("phase one dungeon=%d party_max=%d hp=%d lockout=%v",
				dungeon.Index, dungeon.PartyMax, dungeon.HP, dungeon.LockoutType)
		}
		if !reflect.DeepEqual(dungeon.DestroyObjects, []string{want}) {
			t.Fatalf("phase one dungeon=%d destroy objects=%v want=[%q]", dungeon.Index, dungeon.DestroyObjects, want)
		}
	}

	// Phase two caps revive coins and tags every gate with its route: 4 on the
	// light chain, 5 on the dark one, 0 on the two shared rooms (energy control
	// 5063 and energy save 5066).
	phaseTwo := config.Raid.Phases[1]
	if phaseTwo.CoinLimit != 20 || phaseTwo.MinimalDungeonClearCount != 2 {
		t.Fatalf("phase two coin_limit=%d minimum_clear=%d", phaseTwo.CoinLimit, phaseTwo.MinimalDungeonClearCount)
	}
	wantLockout := map[uint32]int{
		5311: 4, 5057: 4, 5059: 4, 5064: 4, 5061: 4,
		5313: 5, 5058: 5, 5060: 5, 5065: 5, 5062: 5,
		5063: 0, 5066: 0,
	}
	if len(phaseTwo.Dungeons) != len(wantLockout) {
		t.Fatalf("phase two dungeons = %d want %d", len(phaseTwo.Dungeons), len(wantLockout))
	}
	for _, dungeon := range phaseTwo.Dungeons {
		want, ok := wantLockout[dungeon.Index]
		if !ok {
			t.Fatalf("unexpected phase two dungeon=%d", dungeon.Index)
		}
		if dungeon.LockoutType != [2]int{want, 0} {
			t.Fatalf("phase two dungeon=%d lockout=%v want [%d 0]", dungeon.Index, dungeon.LockoutType, want)
		}
		if len(dungeon.DestroyObjects) != 0 {
			t.Fatalf("phase two dungeon=%d unexpected destroy objects=%v", dungeon.Index, dungeon.DestroyObjects)
		}
	}

	// The rank table is shared by both phases and the single-mode phase.
	wantRank := []AntonRankCondition{
		{MinimumDeaths: 0, MaximumDeaths: 1, Rank: 0},
		{MinimumDeaths: 2, MaximumDeaths: 5, Rank: 1},
		{MinimumDeaths: 6, MaximumDeaths: 9, Rank: 2},
		{MinimumDeaths: 10, MaximumDeaths: 14, Rank: 3},
	}
	for index, phase := range []AntonPhase{phaseOne, phaseTwo, config.Raid.SingleMode.Phase} {
		if !reflect.DeepEqual(phase.RankConditions, wantRank) {
			t.Fatalf("phase %d rank conditions = %+v want %+v", index, phase.RankConditions, wantRank)
		}
	}

	// All three [MINIMAL DUNGEON CLEAR COUNT] values the reward gate can read
	// (dnfbridge runtimeRaidPhaseMinimalClearRequirement) are pinned here, because
	// they are three different answers and one of them is easy to get wrong:
	//
	//   - phase one 0 and phase two 2 are the official reward condition
	//     「1阶段：无条件 / 2阶段：通关2个以上的地下城」;
	//   - the single-mode block omits the section entirely, so the shared parser
	//     defaults it to 1 (anton_pvf.go:503) instead of inheriting team phase
	//     one's 0. If single mode ever started reading that 0, the raid would pay
	//     its whole roster without anyone clearing a gate.
	if got := config.Raid.SingleMode.Phase.MinimalDungeonClearCount; got != 1 {
		t.Fatalf("single-mode minimum clear count = %d want 1", got)
	}
}
