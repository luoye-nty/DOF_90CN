package dnfbridge

import (
	"errors"
	"testing"
	"time"

	"longheng.io/server/internal/modules/dnf/raid"
)

// A run whose owner is gone must not keep squatting on its gate.
//
// Live evidence 2026-09-16: the very first entry (09:33:19, map 210 loaded) ended
// in a client crash with no abandoned or completed event, and from then on every
// select of that gate was answered with "runtime raid group already occupies a
// dungeon: group=1 map=210". A run lives inside the owner's session, so it can
// never finish once that session is gone.
func TestRuntimeRaidDungeonRunIsReleasedWhenItsOwnerLeaves(t *testing.T) {
	service, raidKey := newRuntimeRaidLifecycleService(time.Now().UTC())
	if _, err := service.beginRuntimeRaidDungeon(1001, 212, 11); err != nil {
		t.Fatalf("owner entry: %v", err)
	}
	state := service.raids[raidKey]
	if len(state.Lifecycle.GroupOccupancy) != 1 || len(state.Lifecycle.Dungeons) != 1 {
		t.Fatalf("run was not registered: occupancy=%+v dungeons=%+v", state.Lifecycle.GroupOccupancy, state.Lifecycle.Dungeons)
	}

	if _, removed, _ := service.removeRuntimeRaidMember(1001); !removed {
		t.Fatal("the owner's leave was refused")
	}

	state = service.raids[raidKey]
	if len(state.Lifecycle.Dungeons) != 0 || len(state.Lifecycle.GroupOccupancy) != 0 {
		t.Fatalf("the dead run kept its gate: dungeons=%+v occupancy=%+v", state.Lifecycle.Dungeons, state.Lifecycle.GroupOccupancy)
	}
	// The surviving member of the other squad can open the gate again.
	if _, err := service.beginRuntimeRaidDungeon(1002, 212, 21); err != nil {
		t.Fatalf("the gate stayed blocked after its owner left: %v", err)
	}
}

// Live incident 2026-09-16 18:03: the leader and a squadmate stood in the waiting
// area, the leader picked 210, and the server answered
// `game-dungeon-select-blocked reason=dungeon_runtime_commit_failed
// error="runtime raid group already occupies a dungeon: group=1 map=210"` on every
// retry while the squadmate's own prepared run was created and abandoned in the
// same millisecond. Both members were in group 1, each of their dungeon runtimes
// carries its own lifecycle token, and beginRuntimeRaidDungeon only let the
// binding's own character reuse it -- so the second member of every squad was
// turned away, and a party could never enter a raid gate at all. The one earlier
// successful entry (09:33) was a solo squad.
func TestRuntimeRaidSquadmateJoinsTheGateItsSquadAlreadyOccupies(t *testing.T) {
	service, raidKey := newRuntimeRaidLifecycleService(time.Now().UTC())
	// A raid squad is what shares a gate, so both members sit in group 1 and a
	// third, unrelated member sits in group 2.
	squadmate := runtimeRaidMemberState{CharID: 1003, Name: "other-squad", GroupIndex: 2, SlotOrder: 0}
	state := service.raids[raidKey]
	state.Members[1].GroupIndex = 1
	state.FrozenMembers[1].GroupIndex = 1
	state.Members = append(state.Members, squadmate)
	state.FrozenMembers = append(state.FrozenMembers, squadmate)

	// The first member of the squad opens the run.
	owner, err := service.beginRuntimeRaidDungeon(1001, 212, 11)
	if err != nil || owner.RaidKey != raidKey {
		t.Fatalf("squad owner binding=%+v err=%v", owner, err)
	}

	// The squadmate, whose own runtime carries its own lifecycle token, adopts the
	// same run instead of being refused.
	joined, err := service.beginRuntimeRaidDungeon(1002, 212, 12)
	if err != nil {
		t.Fatalf("squadmate was refused the gate its squad occupies: %v", err)
	}
	if joined != owner {
		t.Fatalf("squadmate binding=%+v want the owner's %+v", joined, owner)
	}
	if len(service.raids[raidKey].Lifecycle.Dungeons) != 1 {
		t.Fatalf("the squad opened %d runs for one gate", len(service.raids[raidKey].Lifecycle.Dungeons))
	}

	// The owner re-beginning with a fresh token is a racing duplicate of its own
	// request, not an adoption.
	if _, err := service.beginRuntimeRaidDungeon(1001, 212, 13); err == nil {
		t.Fatal("the owner re-opened its own gate with a new token")
	}

	// A different squad may fight the same gate at the same time: 86JP keys its
	// participation by (dungeon, party) and only refuses a duplicate userId
	// (RaidManager.cs:731-792), so two squads are two runs, not a conflict. The
	// former "one gate, one squad" refusal here was the reason the second squad's
	// every select was answered with errRuntimeRaidDungeonOccupied.
	second, err := service.beginRuntimeRaidDungeon(1003, 212, 14)
	if err != nil {
		t.Fatalf("a second squad was refused the gate: %v", err)
	}
	if second.LifecycleToken != 14 || second.MapID != 212 {
		t.Fatalf("second squad binding=%+v", second)
	}
	state = service.raids[raidKey]
	if len(state.Lifecycle.Dungeons) != 2 || len(state.Lifecycle.GroupOccupancy) != 2 {
		t.Fatalf("two squads share one gate with %d runs / %d occupancies: dungeons=%+v occupancy=%+v",
			len(state.Lifecycle.Dungeons), len(state.Lifecycle.GroupOccupancy),
			state.Lifecycle.Dungeons, state.Lifecycle.GroupOccupancy)
	}
	if _, exists := state.Lifecycle.Dungeons[runtimeRaidDungeonRunKey{MapID: 212, Seat: runtimeRaidDungeonSeat{Group: 1}}]; !exists {
		t.Fatalf("the first squad's run was replaced: %+v", state.Lifecycle.Dungeons)
	}
	if _, exists := state.Lifecycle.Dungeons[runtimeRaidDungeonRunKey{MapID: 212, Seat: runtimeRaidDungeonSeat{Group: 2}}]; !exists {
		t.Fatalf("the second squad's run was not registered: %+v", state.Lifecycle.Dungeons)
	}
}

// One squad still fights one gate: the same group cannot hold two maps at once.
// That rule is carried by GroupOccupancy and must survive the multi-run change --
// it is the counterpart of the removed cross-squad refusal, and the two errors
// must never be confused (errRuntimeRaidGroupOccupied vs
// errRuntimeRaidDungeonOccupied).
func TestRuntimeRaidSameGroupCannotHoldTwoGates(t *testing.T) {
	service, raidKey := newRuntimeRaidLifecycleService(time.Now().UTC())
	if _, err := service.beginRuntimeRaidDungeon(1001, 212, 11); err != nil {
		t.Fatalf("first gate: %v", err)
	}
	// 1002 is in group 2 in this fixture, so it needs its own run first: group 1
	// must stay the one that is blocked.
	if _, err := service.beginRuntimeRaidDungeon(1002, 212, 21); err != nil {
		t.Fatalf("second squad gate: %v", err)
	}
	if _, err := service.beginRuntimeRaidDungeon(1001, 213, 12); !errors.Is(err, errRuntimeRaidGroupOccupied) {
		t.Fatalf("same group second map error=%v want=%v", err, errRuntimeRaidGroupOccupied)
	}
	if len(service.raids[raidKey].Lifecycle.Dungeons) != 2 {
		t.Fatalf("a same-group refusal changed the run table: %+v", service.raids[raidKey].Lifecycle.Dungeons)
	}
}

// A member with no squad is its own seat. Two unassigned members may therefore be
// inside two different gates at the same time, and an unassigned member standing in a
// gate does not hold squad 1's gate against the squad that actually owns it. Folding
// them onto squad 1 (runtimeRaidNormalizedGroup) refused the second entry with
// errRuntimeRaidGroupOccupied -- live 2026-09-17, the same session in which an
// unassigned member's 战况表 row turned out to be drawn as squad 1's.
func TestRuntimeRaidUnassignedMembersHoldTheirOwnSeats(t *testing.T) {
	service, raidKey := newRuntimeRaidLifecycleService(time.Now().UTC())
	state := service.raids[raidKey]
	state.Members = []runtimeRaidMemberState{
		{CharID: 1001, Name: "standby-a", GroupIndex: 0},
		{CharID: 1002, Name: "standby-b", GroupIndex: 0},
		{CharID: 1004, Name: "standby-c", GroupIndex: 0},
		{CharID: 1003, Name: "squad-one", GroupIndex: 1, SlotOrder: 0},
	}
	state.FrozenMembers = append([]runtimeRaidMemberState(nil), state.Members...)

	first, err := service.beginRuntimeRaidDungeon(1001, 212, 11)
	if err != nil {
		t.Fatalf("first unassigned entry: %v", err)
	}
	second, err := service.beginRuntimeRaidDungeon(1002, 213, 21)
	if err != nil {
		t.Fatalf("a second unassigned member was refused a different gate: %v", err)
	}
	if first.LifecycleToken == second.LifecycleToken {
		t.Fatal("both unassigned members share one run token")
	}
	// Same gate, two unassigned members: each fights its own instance of it.
	third, err := service.beginRuntimeRaidDungeon(1004, 212, 31)
	if err != nil {
		t.Fatalf("two unassigned members could not hold the same gate: %v", err)
	}
	if third.LifecycleToken == first.LifecycleToken {
		t.Fatal("two unassigned members share one run of the same gate")
	}
	// Squad 1 was never blocked: an unassigned run is not its gate.
	if _, err := service.beginRuntimeRaidDungeon(1003, 216, 41); err != nil {
		t.Fatalf("squad 1 was blocked by an unassigned member's run: %v", err)
	}
	if len(state.Lifecycle.Dungeons) != 4 {
		t.Fatalf("runs=%+v want one per seat", state.Lifecycle.Dungeons)
	}
	if occupied, ok := state.Lifecycle.GroupOccupancy[runtimeRaidDungeonSeat{CharacterID: 1001}]; !ok || occupied.MapID != 212 {
		t.Fatalf("the unassigned member's seat occupancy=%+v ok=%t", occupied, ok)
	}
	if _, ok := state.Lifecycle.GroupOccupancy[runtimeRaidDungeonSeat{Group: 1}]; !ok {
		t.Fatalf("squad 1 lost its own occupancy: %+v", state.Lifecycle.GroupOccupancy)
	}
}

// Two squads, one gate: each squad opens its own run and each clear advances the
// map's shared objective exactly once, registering only the clearing squad's
// members. 86JP keeps one RaidDungeonParticipation per (dungeon, party) and one
// scalar clear count per (raid, dungeon), while the member counters live on the
// raid aggregate (RaidManager.cs:919-938).
func TestRuntimeRaidTwoSquadsFightOneGateAndCountSeparately(t *testing.T) {
	service, raidKey := newRuntimeRaidLifecycleService(time.Now().UTC())
	state := service.raids[raidKey]
	state.Members = []runtimeRaidMemberState{
		{CharID: 1001, Name: "first-a", GroupIndex: 1, SlotOrder: 0},
		{CharID: 1002, Name: "first-b", GroupIndex: 1, SlotOrder: 1},
		{CharID: 1003, Name: "second-a", GroupIndex: 2, SlotOrder: 0},
		{CharID: 1004, Name: "second-b", GroupIndex: 2, SlotOrder: 1},
	}
	state.FrozenMembers = append([]runtimeRaidMemberState(nil), state.Members...)
	state.Lifecycle.Objectives = map[int]runtimeRaidDungeonObjective{
		210: {
			Phase: runtimeRaidDungeonPhaseOne, MapID: 210,
			State: raid.AntonDungeonOpen, MaxHP: 4, RemainingHP: 4, PartyMax: 5,
		},
	}

	first, err := service.beginRuntimeRaidDungeon(1001, 210, 11)
	if err != nil {
		t.Fatalf("first squad entry: %v", err)
	}
	second, err := service.beginRuntimeRaidDungeon(1003, 210, 21)
	if err != nil {
		t.Fatalf("second squad entry: %v", err)
	}
	if first.LifecycleToken == second.LifecycleToken {
		t.Fatalf("both squads share one run token %d", first.LifecycleToken)
	}
	for _, binding := range []runtimeRaidDungeonBinding{first, second} {
		charID := uint16(1001)
		if binding.LifecycleToken == 21 {
			charID = 1003
		}
		if err := service.applyRuntimeRaidDungeonLifecycleEvent(runtimeRaidDungeonEventForTest(binding, charID, runtimeRaidDungeonLoaded)); err != nil {
			t.Fatalf("loaded token=%d: %v", binding.LifecycleToken, err)
		}
	}

	if err := service.applyRuntimeRaidDungeonLifecycleEvent(runtimeRaidDungeonEventForTest(second, 1003, runtimeRaidDungeonCompleted)); err != nil {
		t.Fatalf("second squad clear: %v", err)
	}
	after := service.raids[raidKey]
	objective := after.Lifecycle.Objectives[210]
	if objective.RemainingHP != 3 || objective.ClearCount != 1 {
		t.Fatalf("objective after the second squad's clear=%+v", objective)
	}
	if counts := after.Lifecycle.PhaseClearCounts; counts[1003] != 1 || counts[1004] != 1 || counts[1001] != 0 || counts[1002] != 0 {
		t.Fatalf("member clear counts=%+v want only squad 2 registered", counts)
	}
	// The first squad's run is untouched by the other squad's clear.
	if _, exists := after.Lifecycle.Dungeons[runtimeRaidDungeonRunKey{MapID: 210, Seat: runtimeRaidDungeonSeat{Group: 1}}]; !exists {
		t.Fatalf("the first squad's run died with the second squad's clear: %+v", after.Lifecycle.Dungeons)
	}

	if err := service.applyRuntimeRaidDungeonLifecycleEvent(runtimeRaidDungeonEventForTest(first, 1001, runtimeRaidDungeonCompleted)); err != nil {
		t.Fatalf("first squad clear: %v", err)
	}
	after = service.raids[raidKey]
	objective = after.Lifecycle.Objectives[210]
	if objective.RemainingHP != 2 || objective.ClearCount != 2 {
		t.Fatalf("objective after both clears=%+v", objective)
	}
	counts := after.Lifecycle.PhaseClearCounts
	if counts[1001] != 1 || counts[1002] != 1 || counts[1003] != 1 || counts[1004] != 1 {
		t.Fatalf("member clear counts=%+v want one clear each and no double counting", counts)
	}
	if len(after.Lifecycle.Dungeons) != 0 || len(after.Lifecycle.GroupOccupancy) != 0 {
		t.Fatalf("cleared runs were not released: dungeons=%+v occupancy=%+v",
			after.Lifecycle.Dungeons, after.Lifecycle.GroupOccupancy)
	}
}

// Without an objective table a completed run keeps its record, so the "already
// cleared" refusal has to be visible to every squad and not only to the one that
// holds the run key.
func TestRuntimeRaidCompletedGateStaysRefusedForAnotherSquad(t *testing.T) {
	service, raidKey := newRuntimeRaidLifecycleService(time.Now().UTC())
	first, err := service.beginRuntimeRaidDungeon(1001, 210, 11)
	if err != nil {
		t.Fatalf("first squad entry: %v", err)
	}
	if err := service.applyRuntimeRaidDungeonLifecycleEvent(runtimeRaidDungeonEventForTest(first, 1001, runtimeRaidDungeonLoaded)); err != nil {
		t.Fatalf("loaded: %v", err)
	}
	if err := service.applyRuntimeRaidDungeonLifecycleEvent(runtimeRaidDungeonEventForTest(first, 1001, runtimeRaidDungeonCompleted)); err != nil {
		t.Fatalf("completed: %v", err)
	}
	if len(service.raids[raidKey].Lifecycle.Dungeons) != 1 {
		t.Fatalf("the completed record did not survive without objectives: %+v", service.raids[raidKey].Lifecycle.Dungeons)
	}
	if _, err := service.beginRuntimeRaidDungeon(1002, 210, 21); !errors.Is(err, errRuntimeRaidDungeonCompleted) {
		t.Fatalf("a cleared gate let another squad in: %v", err)
	}
}

// Releasing a departed owner drops only that owner's run and its own squad's
// occupancy; the other squad keeps fighting.
func TestRuntimeRaidOwnerLeavingReleasesOnlyItsOwnSquad(t *testing.T) {
	service, raidKey := newRuntimeRaidLifecycleService(time.Now().UTC())
	if _, err := service.beginRuntimeRaidDungeon(1001, 210, 11); err != nil {
		t.Fatalf("first squad entry: %v", err)
	}
	second, err := service.beginRuntimeRaidDungeon(1002, 210, 21)
	if err != nil {
		t.Fatalf("second squad entry: %v", err)
	}
	if err := service.applyRuntimeRaidDungeonLifecycleEvent(runtimeRaidDungeonEventForTest(second, 1002, runtimeRaidDungeonLoaded)); err != nil {
		t.Fatalf("second squad loaded: %v", err)
	}
	if _, removed, _ := service.removeRuntimeRaidMember(1001); !removed {
		t.Fatal("the owner's leave was refused")
	}
	after := service.raids[raidKey]
	if _, exists := after.Lifecycle.Dungeons[runtimeRaidDungeonRunKey{MapID: 210, Seat: runtimeRaidDungeonSeat{Group: 1}}]; exists {
		t.Fatalf("the dead run survived its owner: %+v", after.Lifecycle.Dungeons)
	}
	if _, exists := after.Lifecycle.Dungeons[runtimeRaidDungeonRunKey{MapID: 210, Seat: runtimeRaidDungeonSeat{Group: 2}}]; !exists {
		t.Fatalf("the other squad's run was released with the owner: %+v", after.Lifecycle.Dungeons)
	}
	if _, occupied := after.Lifecycle.GroupOccupancy[runtimeRaidDungeonSeat{Group: 2}]; !occupied {
		t.Fatalf("the other squad lost its gate occupancy: %+v", after.Lifecycle.GroupOccupancy)
	}
	// The survivor can still finish its own run. With no objective table the
	// completed record is retained, and only this squad's occupancy is released.
	if err := service.applyRuntimeRaidDungeonLifecycleEvent(runtimeRaidDungeonEventForTest(second, 1002, runtimeRaidDungeonCompleted)); err != nil {
		t.Fatalf("surviving squad clear: %v", err)
	}
	final := service.raids[raidKey]
	progress, exists := final.Lifecycle.Dungeons[runtimeRaidDungeonRunKey{MapID: 210, Seat: runtimeRaidDungeonSeat{Group: 2}}]
	if !exists || progress.Status != runtimeRaidDungeonProgressCompleted {
		t.Fatalf("the surviving squad's clear was not recorded: %+v", progress)
	}
	if len(final.Lifecycle.GroupOccupancy) != 0 {
		t.Fatalf("a completed run kept its gate occupied: %+v", final.Lifecycle.GroupOccupancy)
	}
}
