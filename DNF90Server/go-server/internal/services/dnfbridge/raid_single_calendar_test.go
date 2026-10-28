package dnfbridge

import (
	"testing"
	"time"

	"longheng.io/server/internal/modules/dnf/raid"
)

func runtimeRaidBridgeTestSingleAntonConfig() *raid.AntonConfig {
	config := runtimeRaidBridgeTestAntonConfig()
	config.SingleMode = &raid.AntonSingleModeConfig{
		Name:             "single",
		TimeLimitSeconds: 2100,
		Phase: raid.AntonPhase{Dungeons: []raid.AntonDungeonInfo{
			{Index: 5314, InitState: raid.AntonDungeonOpen, HP: 1},
			{Index: 5315, InitState: raid.AntonDungeonHide, HP: 1},
			{Index: 5316, InitState: raid.AntonDungeonHide, HP: 1},
			{Index: 5317, InitState: raid.AntonDungeonHide, HP: 1},
			{Index: 5318, InitState: raid.AntonDungeonHide, HP: 1},
			{Index: 5321, InitState: raid.AntonDungeonHide, HP: 1},
			{Index: 5322, InitState: raid.AntonDungeonHide, HP: 1},
			{Index: 5319, InitState: raid.AntonDungeonHide, HP: 1},
			{Index: 5320, InitState: raid.AntonDungeonHide, HP: 1},
		}},
	}
	return config
}

func runtimeRaidBridgeTestLukeConfig() *raid.LukeConfig {
	config := runtimeRaidBridgeTestAntonConfig()
	config.EntryCost = raid.AntonEntryCost{ItemID: 10159649, Count: 1}
	config.SingleMode = &raid.AntonSingleModeConfig{
		Name:             "luke single",
		TimeLimitSeconds: 1800,
		Phase: raid.AntonPhase{Dungeons: []raid.AntonDungeonInfo{
			{Index: 5323, InitState: raid.AntonDungeonOpen, HP: 1},
			{Index: 5324, InitState: raid.AntonDungeonClosed, HP: 1},
			{Index: 5325, InitState: raid.AntonDungeonHide, HP: 1},
			{Index: 5326, InitState: raid.AntonDungeonHide, HP: 1},
			{Index: 5327, InitState: raid.AntonDungeonHide, HP: 1},
			{Index: 5328, InitState: raid.AntonDungeonHide, HP: 1},
			{Index: 5329, InitState: raid.AntonDungeonOpen, HP: 1},
			{Index: 5330, InitState: raid.AntonDungeonClosed, HP: 1},
			{Index: 5331, InitState: raid.AntonDungeonHide, HP: 1},
			{Index: 5332, InitState: raid.AntonDungeonHide, HP: 1},
			{Index: 5333, InitState: raid.AntonDungeonHide, HP: 1},
			{Index: 5334, InitState: raid.AntonDungeonHide, HP: 1},
		}},
	}
	return &raid.LukeConfig{
		Raid:                   config,
		DailyAccountEnterLimit: 10,
		LightRoute:             []uint32{5323, 5324, 5325, 5326, 5327, 5328},
		DarkRoute:              []uint32{5329, 5330, 5331, 5332, 5333, 5334},
	}
}

func TestRuntimeRaidSingleTypeUsesChinaServiceCalendar(t *testing.T) {
	t.Parallel()
	wants := []byte{0, 0, 3, 3, 3, 6, 6}
	for day, want := range wants {
		at := time.Date(2026, 8, 16+day, 12, 0, 0, 0, runtimeRaidChinaCalendar)
		if got := runtimeRaidSingleTypeForTime(at); got != want {
			t.Fatalf("weekday=%s route=%d want=%d", at.Weekday(), got, want)
		}
	}
	// Monday 16:30 UTC is already Tuesday in China. This catches accidental
	// use of the process/UTC calendar around the service-day boundary.
	at := time.Date(2026, 8, 17, 16, 30, 0, 0, time.UTC)
	if got := runtimeRaidSingleTypeForTime(at); got != 3 {
		t.Fatalf("China Tuesday boundary route=%d want=3", got)
	}
}

func TestRuntimeRaidLukeSingleTypeUsesChinaServiceCalendar(t *testing.T) {
	t.Parallel()
	wants := []byte{
		runtimeRaidLukeDarkRoute,
		runtimeRaidLukeLightRoute,
		runtimeRaidLukeLightRoute,
		runtimeRaidLukeLightRoute,
		runtimeRaidLukeDarkRoute,
		runtimeRaidLukeDarkRoute,
		runtimeRaidLukeDarkRoute,
	}
	for day, want := range wants {
		at := time.Date(2026, 8, 16+day, 12, 0, 0, 0, runtimeRaidChinaCalendar)
		if got := runtimeRaidLukeSingleTypeForTime(at); got != want {
			t.Fatalf("weekday=%s route=%d want=%d", at.Weekday(), got, want)
		}
	}
}

func TestAttachRuntimeRaidLukeOwnerConfigUsesLukeTicketBeforeStart(t *testing.T) {
	t.Parallel()
	luke := runtimeRaidBridgeTestLukeConfig()
	const raidKey = uint32(0x00200013)
	service := &Service{raids: map[uint32]*runtimeRaidState{
		raidKey: {
			RaidKey:  raidKey,
			LeaderID: 19,
			RaidMode: runtimeRaidOwnerLuke,
			TailFlag: 1,
			Members:  []runtimeRaidMemberState{{CharID: 19}},
		},
	}}
	state, ok := service.attachRuntimeRaidOwnerConfig(raidKey, runtimeRaidOwnerConfig{
		Mode: runtimeRaidOwnerLuke,
		Raid: luke.Raid,
		Luke: luke,
	})
	if !ok || state.LukeConfig != luke || state.AntonConfig != nil {
		t.Fatalf("attach ok=%t state=%+v", ok, state)
	}
	itemID, count := runtimeRaidEntryCostForState(state)
	if itemID != 10159649 || count != 1 {
		t.Fatalf("Luke pre-start entry cost=(%d,%d), want=(10159649,1)", itemID, count)
	}
}

func TestCommitRuntimeSingleRaidStartFreezesCalendarRoute(t *testing.T) {
	t.Parallel()
	const (
		characterID = uint16(19)
		raidKey     = uint32(0x00170013)
	)
	member := runtimeRaidMemberState{CharID: characterID, GroupIndex: 1}
	state := &runtimeRaidState{
		RaidKey:        raidKey,
		LeaderID:       characterID,
		TailFlag:       1,
		SingleRaidType: 0,
		Members:        []runtimeRaidMemberState{member},
		FrozenMembers:  []runtimeRaidMemberState{member},
		StartPending:   true,
		StartAttempt:   7,
	}
	service := &Service{raids: map[uint32]*runtimeRaidState{raidKey: state}}
	reservation := runtimeRaidStartReservation{
		State:   cloneRuntimeRaidState(*state),
		Attempt: state.StartAttempt,
	}
	chinaTuesday := time.Date(2026, 8, 17, 16, 30, 0, 0, time.UTC)
	committed, ok := service.commitRuntimeRaidStart(
		reservation,
		nil,
		runtimeRaidBridgeTestSingleAntonConfig(),
		chinaTuesday,
	)
	if !ok || committed.SingleRaidType != 3 {
		t.Fatalf("commit ok=%t route=%d want China Tuesday route 3", ok, committed.SingleRaidType)
	}

	started := service.raids[raidKey]
	if err := advanceRuntimeRaidObjectiveLocked(started, runtimeRaidDungeonPhaseOne, 5314); err != nil {
		t.Fatalf("complete black mist: %v", err)
	}
	if got := started.Lifecycle.Objectives[5315].State; got != raid.AntonDungeonOpen {
		t.Fatalf("China Tuesday second map state=%v want tremor open", got)
	}
	if got := started.Lifecycle.Objectives[5317].State; got == raid.AntonDungeonOpen {
		t.Fatalf("China Tuesday incorrectly opened towering route state=%v", got)
	}
}
