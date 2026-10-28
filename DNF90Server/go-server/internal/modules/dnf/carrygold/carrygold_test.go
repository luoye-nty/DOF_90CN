package carrygold

import (
	"context"
	"errors"
	"testing"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

// realLevelLimitRows is the shape of the runtime table
// `etc/(r)goldlimitbylevel.etc` carries, trimmed to the rows the rules care
// about: the low-level ramp, the level-60 plateau, and the level-90 cap.
var realLevelLimitRows = []int64{
	0, 0,
	1, 100_000_000,
	30, 100_000_000,
	60, 400_000_000,
	99, 400_000_000,
}

func installTestLevelLimits(t *testing.T, values []int64) {
	t.Helper()
	limits, err := parseLevelLimits(values)
	if err != nil {
		t.Fatal(err)
	}
	previous := Installed()
	Install(limits)
	t.Cleanup(func() { Install(previous) })
}

func TestStageClampsToTheClientBound(t *testing.T) {
	if got := Stage(nil); got != 0 {
		t.Fatalf("nil stats stage=%d", got)
	}
	if got := Stage(map[string]int64{}); got != 0 {
		t.Fatalf("missing stat stage=%d", got)
	}
	if got := Stage(map[string]int64{StageStat: -3}); got != 0 {
		t.Fatalf("negative stat stage=%d", got)
	}
	if got := Stage(map[string]int64{StageStat: 99}); got != MaxStage {
		t.Fatalf("clamped stage=%d want %d", got, MaxStage)
	}
	for stage := byte(0); stage <= MaxStage; stage++ {
		if got := DisplayedLimit(stage); got != DisplayedBase+int(stage) {
			t.Fatalf("stage=%d displayed=%d", stage, got)
		}
		if got := StageBonus(stage); got != int64(stage)*Unit {
			t.Fatalf("stage=%d bonus=%d", stage, got)
		}
	}
}

func TestLevelLimitsFollowThePVFTable(t *testing.T) {
	limits, err := parseLevelLimits(realLevelLimitRows)
	if err != nil {
		t.Fatal(err)
	}
	// A level between two rows keeps the lower row's value, and a level above
	// the last row keeps the last row's value.
	for level, want := range map[int]int64{
		0: 0, 1: 100_000_000, 29: 100_000_000, 30: 100_000_000,
		59: 100_000_000, 60: 400_000_000, 90: 400_000_000, 200: 400_000_000,
	} {
		if got := limits.Limit(level); got != want {
			t.Fatalf("level=%d limit=%d want %d", level, got, want)
		}
	}
	if limits.Len() != len(realLevelLimitRows)/2 {
		t.Fatalf("rows=%d", limits.Len())
	}
	if _, err := parseLevelLimits([]int64{1}); !errors.Is(err, ErrLevelLimitTable) {
		t.Fatalf("odd table err=%v", err)
	}
	if _, err := LoadLevelLimits(context.Background(), nil); !errors.Is(err, ErrSourceRequired) {
		t.Fatalf("nil source err=%v", err)
	}
}

func TestLimitForCharacterAddsTheBoughtStagesToThePVFBase(t *testing.T) {
	installTestLevelLimits(t, realLevelLimitRows)
	capped := dnfrepo.CharacterRecord{Level: 90, Stats: map[string]int64{}}
	if got := LimitForCharacter(capped); got != 400_000_000 {
		t.Fatalf("level 90 stage 0 limit=%d", got)
	}
	capped.Stats[StageStat] = int64(MaxStage)
	// Four bought stages read as 8亿 in game, which is what the client's own
	// stage+4 string promises at the level cap.
	if got := LimitForCharacter(capped); got != 800_000_000 {
		t.Fatalf("level 90 stage 4 limit=%d", got)
	}
	// A low-level character gets the PVF base for its level, not the 4亿 the
	// dialog shows, plus the same per-stage 亿.
	low := dnfrepo.CharacterRecord{Level: 30, Stats: map[string]int64{StageStat: 1}}
	if got := LimitForCharacter(low); got != 200_000_000 {
		t.Fatalf("level 30 stage 1 limit=%d", got)
	}
	// Level 0 has no PVF allowance at all, which must read as uncapped rather
	// than as a zero cap that would swallow every gold gain.
	if got := LimitForCharacter(dnfrepo.CharacterRecord{Level: 0}); got != 0 {
		t.Fatalf("level 0 limit=%d", got)
	}
}

func TestNoInstalledTableLeavesEveryFaucetUncapped(t *testing.T) {
	previous := Installed()
	Install(nil)
	t.Cleanup(func() { Install(previous) })
	character := dnfrepo.CharacterRecord{Level: 90, Stats: map[string]int64{"gold": 900_000_000}}
	if got := LimitForCharacter(character); got != 0 {
		t.Fatalf("uninstalled limit=%d", got)
	}
	after, discarded := Credit(character, 1_000)
	if after != 900_001_000 || discarded != 0 {
		t.Fatalf("uninstalled credit after=%d discarded=%d", after, discarded)
	}
}

func TestCreditStopsAtTheCapAndNeverLowersAnExistingBalance(t *testing.T) {
	installTestLevelLimits(t, realLevelLimitRows)
	character := dnfrepo.CharacterRecord{Level: 90, Stats: map[string]int64{"gold": 399_000_000}}
	after, discarded := Credit(character, 5_000_000)
	if after != 400_000_000 || discarded != 4_000_000 {
		t.Fatalf("credit at cap after=%d discarded=%d", after, discarded)
	}

	// One purchased stage buys exactly one 亿 more room.
	character.Stats[StageStat] = 1
	after, discarded = Credit(character, 5_000_000)
	if after != 404_000_000 || discarded != 0 {
		t.Fatalf("credit after upgrade after=%d discarded=%d", after, discarded)
	}

	// Release-side gold was never capped, so an existing balance above the cap
	// must be left alone rather than clamped down and destroyed.
	over := dnfrepo.CharacterRecord{Level: 90, Stats: map[string]int64{"gold": 900_000_000}}
	after, discarded = Credit(over, 1_000)
	if after != 900_000_000 || discarded != 1_000 {
		t.Fatalf("credit above cap after=%d discarded=%d", after, discarded)
	}
	if got := Room(over); got != 0 {
		t.Fatalf("room above cap=%d", got)
	}
	if got := Room(dnfrepo.CharacterRecord{Level: 90, Stats: map[string]int64{"gold": 100_000_000}}); got != 300_000_000 {
		t.Fatalf("room=%d", got)
	}
}

func newUpgradeOwner(t *testing.T, gold int64, stage int64) (*Owner, dnfrepo.Group) {
	t.Helper()
	repositories := dnfrepomemory.NewMemoryGroup()
	stats := map[string]int64{"gold": gold}
	if stage > 0 {
		stats[StageStat] = stage
	}
	if err := repositories.Character.Save(context.Background(), dnfrepo.CharacterRecord{
		CharacterID: "17", AccountID: "account-1", Level: 90, Stats: stats,
	}); err != nil {
		t.Fatal(err)
	}
	owner, err := NewOwner(repositories)
	if err != nil {
		t.Fatal(err)
	}
	return owner, repositories
}

func TestOwnerUpgradeChargesAndPersistsTheNextStage(t *testing.T) {
	installTestLevelLimits(t, realLevelLimitRows)
	owner, repositories := newUpgradeOwner(t, 250_000_000, 0)
	result, err := owner.Upgrade(context.Background(), Command{
		AccountID: "account-1", SelectedCharacterID: 17, RequestedStage: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Upgraded || result.Stage != 1 || result.PreviousStage != 0 ||
		result.GoldAfter != 250_000_000-StageCost || result.Limit != 500_000_000 {
		t.Fatalf("upgrade result=%+v", result)
	}
	stored, _, _ := repositories.Character.Load(context.Background(), "17")
	if stored.Stats[StageStat] != 1 || stored.Stats["gold"] != 250_000_000-StageCost {
		t.Fatalf("stored stats=%+v", stored.Stats)
	}
	// The purchased stage has to move the limit gold gains are measured
	// against, otherwise the upgrade is a number nobody reads.
	if got := LimitForCharacter(stored); got != 500_000_000 {
		t.Fatalf("stored limit=%d", got)
	}
}

func TestOwnerUpgradeRefusalsLeaveTheCharacterUntouched(t *testing.T) {
	installTestLevelLimits(t, realLevelLimitRows)
	for name, testCase := range map[string]struct {
		gold      int64
		stage     int64
		requested byte
		wantErr   error
	}{
		"out of sequence": {gold: 900_000_000, stage: 1, requested: 3, wantErr: ErrStageOutOfSequence},
		"exhausted":       {gold: 900_000_000, stage: int64(MaxStage), requested: MaxStage + 1, wantErr: ErrStageExhausted},
		"short on gold":   {gold: StageCost - 1, stage: 0, requested: 1, wantErr: ErrGoldInsufficient},
	} {
		t.Run(name, func(t *testing.T) {
			owner, repositories := newUpgradeOwner(t, testCase.gold, testCase.stage)
			result, err := owner.Upgrade(context.Background(), Command{
				AccountID: "account-1", SelectedCharacterID: 17, RequestedStage: testCase.requested,
			})
			if !errors.Is(err, testCase.wantErr) {
				t.Fatalf("err=%v want %v", err, testCase.wantErr)
			}
			// The refusal still has to carry the durable stage so the client can
			// be told what it really owns.
			if result.Upgraded || result.Stage != StageFromValue(testCase.stage) {
				t.Fatalf("refused result=%+v", result)
			}
			stored, _, _ := repositories.Character.Load(context.Background(), "17")
			if stored.Stats["gold"] != testCase.gold || stored.Stats[StageStat] != testCase.stage {
				t.Fatalf("refused upgrade mutated stats=%+v", stored.Stats)
			}
		})
	}
}

func TestOwnerRejectsAForeignAccountAndAMissingCharacter(t *testing.T) {
	owner, _ := newUpgradeOwner(t, 250_000_000, 0)
	if _, err := owner.Upgrade(context.Background(), Command{
		AccountID: "account-2", SelectedCharacterID: 17, RequestedStage: 1,
	}); !errors.Is(err, ErrAccountMismatch) {
		t.Fatalf("account mismatch err=%v", err)
	}
	if _, err := owner.Upgrade(context.Background(), Command{
		AccountID: "account-1", SelectedCharacterID: 99, RequestedStage: 1,
	}); !errors.Is(err, ErrCharacterNotFound) {
		t.Fatalf("missing character err=%v", err)
	}
	if _, err := owner.Load(context.Background(), 0); !errors.Is(err, ErrCharacterRequired) {
		t.Fatalf("missing selection err=%v", err)
	}
}
