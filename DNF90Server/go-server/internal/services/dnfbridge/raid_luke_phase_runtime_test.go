package dnfbridge

import (
	"encoding/binary"
	"fmt"
	"testing"
	"time"

	"longheng.io/server/internal/modules/dnf/raid"
)

// The two fixtures below mirror etc/raid/luke/luke.etc exactly; the real values
// are pinned against the shipped PVF by
// raid.TestRealScriptPVFLukePhaseMechanismTables.
func lukeTeamPhaseOneState() *runtimeRaidState {
	objectives := map[int]runtimeRaidDungeonObjective{}
	for _, gate := range []struct {
		mapID int
		state raid.AntonDungeonState
	}{
		{lukeBirthSanctumMapID, raid.AntonDungeonOpen},
		{lukeExtinctionSanctumMapID, raid.AntonDungeonHide},
		{lukeSpreadSanctumMapID, raid.AntonDungeonHide},
	} {
		objectives[gate.mapID] = runtimeRaidDungeonObjective{
			Phase: runtimeRaidDungeonPhaseOne, MapID: gate.mapID, State: gate.state,
			MaxHP: 3, RemainingHP: 3, PartyMax: 1,
		}
	}
	state := &runtimeRaidState{
		RaidKey:  0x00200001,
		RaidMode: runtimeRaidOwnerLuke,
		Lifecycle: runtimeRaidLifecycleState{
			Status:     runtimeRaidLifecyclePhaseActive,
			Phase:      runtimeRaidDungeonPhaseOne,
			Objectives: objectives,
		},
	}
	state.ensureRuntimeRaidLifecycleMapsLocked()
	initRuntimeRaidLukePhaseOneLocked(state)
	return state
}

func lukeTeamPhaseTwoState() *runtimeRaidState {
	objectives := map[int]runtimeRaidDungeonObjective{}
	// The init states are the ones etc/raid/luke/luke.etc actually declares: the
	// four energy/altar rooms start `close`, everything else `hide`.
	for mapID, initState := range map[int]raid.AntonDungeonState{
		lukeLightEntranceMapID: raid.AntonDungeonHide,
		lukeDarkEntranceMapID:  raid.AntonDungeonHide,
		lukeLightEnergyMapID:   raid.AntonDungeonClosed,
		lukeDarkEnergyMapID:    raid.AntonDungeonClosed,
		lukeLightAltarMapID:    raid.AntonDungeonClosed,
		lukeDarkAltarMapID:     raid.AntonDungeonClosed,
		lukeEnergyControlMapID: raid.AntonDungeonHide,
		lukeLightTempleMapID:   raid.AntonDungeonHide,
		lukeDarkTempleMapID:    raid.AntonDungeonHide,
		lukeLightThroneMapID:   raid.AntonDungeonHide,
		lukeDarkThroneMapID:    raid.AntonDungeonHide,
		lukeEnergySaveMapID:    raid.AntonDungeonHide,
	} {
		objectives[mapID] = runtimeRaidDungeonObjective{
			Phase: runtimeRaidDungeonPhaseTwo, MapID: mapID, State: initState,
			MaxHP: 1, RemainingHP: 1, PartyMax: 1,
		}
	}
	state := &runtimeRaidState{
		RaidKey:  0x00200001,
		RaidMode: runtimeRaidOwnerLuke,
		Started:  true,
		Lifecycle: runtimeRaidLifecycleState{
			Status:     runtimeRaidLifecyclePhaseActive,
			Phase:      runtimeRaidDungeonPhaseTwo,
			Objectives: objectives,
		},
	}
	state.ensureRuntimeRaidLifecycleMapsLocked()
	return state
}

func lukeGateState(t *testing.T, state *runtimeRaidState, mapID int) raid.AntonDungeonState {
	t.Helper()
	objective, ok := state.Lifecycle.Objectives[mapID]
	if !ok {
		t.Fatalf("gate %d missing", mapID)
	}
	return objective.State
}

// lukeClear drives one committed clear all the way through the shared objective
// advance, which is what the dungeon lifecycle does on a real clear.
func lukeClear(t *testing.T, state *runtimeRaidState, mapID int, now time.Time) []runtimeRaidMechanismTimer {
	t.Helper()
	if err := advanceRuntimeRaidObjectiveLocked(state, state.Lifecycle.Phase, mapID); err != nil {
		t.Fatalf("advance gate %d: %v", mapID, err)
	}
	if state.Lifecycle.Phase != runtimeRaidDungeonPhaseTwo {
		return nil
	}
	return applyRuntimeRaidLukePhaseTwoClearLocked(state, mapID, now)
}

func TestLukePhaseOneOpensBranchSanctumsOnEntryAndRecordsFirstBranch(t *testing.T) {
	state := lukeTeamPhaseOneState()
	if got := lukeGateState(t, state, lukeExtinctionSanctumMapID); got != raid.AntonDungeonHide {
		t.Fatalf("extinction sanctum starts %q, want hide", got)
	}

	// [ON ENTER DUNGEON] 5054 with [2PHASE DUNGEON TYPE] still 0.
	if !runtimeRaidLukeEnterDungeonLocked(state, lukeBirthSanctumMapID) {
		t.Fatal("entering the birth sanctum opened nothing")
	}
	for _, mapID := range []int{lukeExtinctionSanctumMapID, lukeSpreadSanctumMapID} {
		if got := lukeGateState(t, state, mapID); got != raid.AntonDungeonOpen {
			t.Fatalf("gate %d = %q after entry, want open", mapID, got)
		}
	}

	// Three clears finish a sanctum; 扩散 falling first sets the route marker to 2.
	now := time.Unix(1700000000, 0).UTC()
	for i := 0; i < 3; i++ {
		lukeClear(t, state, lukeSpreadSanctumMapID, now)
	}
	if got := runtimeRaidLukeSymbolLocked(state, lukeSymbolPhaseTwoDungeonType); got != lukePhaseTwoDungeonTypeSpread {
		t.Fatalf("2PHASE DUNGEON TYPE=%d want=%d", got, lukePhaseTwoDungeonTypeSpread)
	}
	// The marker is decided once: 灭亡 falling later must not overwrite it.
	for i := 0; i < 3; i++ {
		lukeClear(t, state, lukeExtinctionSanctumMapID, now)
	}
	if got := runtimeRaidLukeSymbolLocked(state, lukeSymbolPhaseTwoDungeonType); got != lukePhaseTwoDungeonTypeSpread {
		t.Fatalf("2PHASE DUNGEON TYPE overwritten to %d", got)
	}

	if runtimeRaidPhaseObjectivesComplete(*state, runtimeRaidDungeonPhaseOne) {
		t.Fatal("phase one complete with the birth sanctum still standing")
	}
	for i := 0; i < 3; i++ {
		lukeClear(t, state, lukeBirthSanctumMapID, now)
	}
	if !runtimeRaidPhaseObjectivesComplete(*state, runtimeRaidDungeonPhaseOne) {
		t.Fatal("phase one did not complete with all three sanctums down")
	}
	if got := runtimeRaidLukeSymbolLocked(state, lukeSymbolPhaseOneSuccess); got != 1 {
		t.Fatalf("[1PHASE SUCCESS]=%d want=1", got)
	}
}

func TestLukePhaseOneGiveUpResetsThatSanctumObjectCounter(t *testing.T) {
	state := lukeTeamPhaseOneState()
	setRuntimeRaidLukeSymbolLocked(state, lukeSymbolBirthSanctumObject, 4)
	setRuntimeRaidLukeSymbolLocked(state, lukeSymbolSpreadSanctumObject, 2)

	runtimeRaidLukeGiveUpDungeonLocked(state, lukeBirthSanctumMapID)
	if got := runtimeRaidLukeSymbolLocked(state, lukeSymbolBirthSanctumObject); got != 0 {
		t.Fatalf("birth sanctum objects=%d want=0", got)
	}
	// Only the abandoned sanctum resets.
	if got := runtimeRaidLukeSymbolLocked(state, lukeSymbolSpreadSanctumObject); got != 2 {
		t.Fatalf("spread sanctum objects=%d want=2", got)
	}
}

func TestLukePhaseTwoWalksBothChainsToTheTwoThrones(t *testing.T) {
	state := lukeTeamPhaseTwoState()
	now := time.Unix(1700000000, 0).UTC()

	armed := initRuntimeRaidLukePhaseTwoLocked(state, now)
	if len(armed) != 1 || armed[0].MapID != lukeBirthSanctumMapID {
		t.Fatalf("phase two opening timer=%+v", armed)
	}
	for _, mapID := range []int{lukeLightEntranceMapID, lukeDarkEntranceMapID} {
		if got := lukeGateState(t, state, mapID); got != raid.AntonDungeonOpen {
			t.Fatalf("entrance %d=%q want open", mapID, got)
		}
	}

	// One entrance is not enough to open the altars.
	lukeClear(t, state, lukeLightEntranceMapID, now)
	if got := lukeGateState(t, state, lukeLightEnergyMapID); got == raid.AntonDungeonOpen {
		t.Fatalf("light energy opened on one entrance: %q", got)
	}
	lukeClear(t, state, lukeDarkEntranceMapID, now)
	for _, mapID := range []int{lukeLightEnergyMapID, lukeDarkEnergyMapID, lukeEnergyControlMapID} {
		if got := lukeGateState(t, state, mapID); got != raid.AntonDungeonOpen {
			t.Fatalf("gate %d=%q after both entrances, want open", mapID, got)
		}
	}
	if got := runtimeRaidLukeSymbolLocked(state, lukeSymbolEnergyControlEffect); got != 1 {
		t.Fatalf("[ENERGY CONTROL EFFECT]=%d want=1", got)
	}

	// Each energy room opens its own altar, and both altars open both temples.
	lukeClear(t, state, lukeLightEnergyMapID, now)
	if got := lukeGateState(t, state, lukeLightAltarMapID); got != raid.AntonDungeonOpen {
		t.Fatalf("light altar=%q want open", got)
	}
	lukeClear(t, state, lukeDarkEnergyMapID, now)
	lukeClear(t, state, lukeLightAltarMapID, now)
	if got := lukeGateState(t, state, lukeLightTempleMapID); got != raid.AntonDungeonHide {
		t.Fatalf("light temple opened on one altar: %q", got)
	}
	lukeClear(t, state, lukeDarkAltarMapID, now)
	for _, mapID := range []int{lukeLightTempleMapID, lukeDarkTempleMapID} {
		if got := lukeGateState(t, state, mapID); got != raid.AntonDungeonOpen {
			t.Fatalf("temple %d=%q want open", mapID, got)
		}
	}
	// Both altars down also shuts the energy control room for good.
	if got := lukeGateState(t, state, lukeEnergyControlMapID); got != raid.AntonDungeonHide {
		t.Fatalf("energy control=%q want hide", got)
	}
	if got := runtimeRaidLukeSymbolLocked(state, lukeSymbolEnergyControlEffect); got != 0 {
		t.Fatalf("[ENERGY CONTROL EFFECT]=%d want=0", got)
	}

	// Both temples open 机械王座:明 only; 暗 follows the light throne's fall.
	lukeClear(t, state, lukeLightTempleMapID, now)
	lukeClear(t, state, lukeDarkTempleMapID, now)
	if got := lukeGateState(t, state, lukeLightThroneMapID); got != raid.AntonDungeonOpen {
		t.Fatalf("light throne=%q want open", got)
	}
	if got := lukeGateState(t, state, lukeDarkThroneMapID); got != raid.AntonDungeonHide {
		t.Fatalf("dark throne opened early: %q", got)
	}

	lukeClear(t, state, lukeLightThroneMapID, now)
	if got := lukeGateState(t, state, lukeDarkThroneMapID); got != raid.AntonDungeonOpen {
		t.Fatalf("dark throne=%q want open", got)
	}
	if got := state.Lifecycle.MovieSymbols[lukeSymbolKillAllCharacter]; got != 1 {
		t.Fatalf("[KILL ALL CHARACTER]=%d want=1 between the two throne stages", got)
	}
	if runtimeRaidPhaseObjectivesComplete(*state, runtimeRaidDungeonPhaseTwo) {
		t.Fatal("phase two complete before the dark throne fell")
	}

	lukeClear(t, state, lukeDarkThroneMapID, now)
	if !runtimeRaidPhaseObjectivesComplete(*state, runtimeRaidDungeonPhaseTwo) {
		t.Fatal("phase two did not complete on the dark throne")
	}
}

// TestLukePhaseTwoInitOpensEntrancesBeforeLifecyclePhaseIsAssigned reproduces
// the real call ordering inside activateRuntimeRaidPhase, which builds the new
// phase's objectives and runs the phase-two init BEFORE assigning
// Lifecycle.Phase. The gate setter used to compare an objective's phase against
// Lifecycle.Phase, so during activation every opening was silently dropped:
// live 2026-09-23T05:38:55Z op585 went out with 5311 and 5313 still at their
// PVF `hide` (state 2) and the whole phase-two route was unreachable, while the
// symbols -- which have no such guard -- went out correctly.
func TestLukePhaseTwoInitOpensEntrancesBeforeLifecyclePhaseIsAssigned(t *testing.T) {
	state := lukeTeamPhaseTwoState()
	// The objectives belong to phase two; the lifecycle still says phase one.
	state.Lifecycle.Phase = runtimeRaidDungeonPhaseOne

	initRuntimeRaidLukePhaseTwoLocked(state, time.Unix(1700000000, 0).UTC())

	for _, mapID := range []int{lukeLightEntranceMapID, lukeDarkEntranceMapID} {
		if got := lukeGateState(t, state, mapID); got != raid.AntonDungeonOpen {
			t.Fatalf("entrance %d=%q want open during activation", mapID, got)
		}
	}
	// Everything else keeps the state the PVF gave it.
	if got := lukeGateState(t, state, lukeLightEnergyMapID); got == raid.AntonDungeonOpen {
		t.Fatal("the light altar opened before both entrances were cleared")
	}
}

// TestLukePhaseTwoRoutesAreMutuallyExclusive covers the reported "两条线都能进".
// Nothing read [LOCKOUT TYPE], so any member could walk into both routes.
func TestLukePhaseTwoRoutesAreMutuallyExclusive(t *testing.T) {
	state := lukeTeamPhaseTwoState()
	initRuntimeRaidLukePhaseTwoLocked(state, time.Unix(1700000000, 0).UTC())
	const light, dark = uint16(1001), uint16(1002)

	// Before anyone commits, both routes are open to both members.
	for _, mapID := range []int{lukeLightEntranceMapID, lukeDarkEntranceMapID} {
		for _, charID := range []uint16{light, dark} {
			if !runtimeRaidLukeRouteEntryAllowedLocked(state, charID, mapID) {
				t.Fatalf("char %d refused gate %d before binding", charID, mapID)
			}
		}
	}

	if route, bound := bindRuntimeRaidLukeRouteLocked(state, light, lukeLightEntranceMapID); !bound || route != lukeRouteLight {
		t.Fatalf("light bind=(%d,%t)", route, bound)
	}
	if route, bound := bindRuntimeRaidLukeRouteLocked(state, dark, lukeDarkEntranceMapID); !bound || route != lukeRouteDark {
		t.Fatalf("dark bind=(%d,%t)", route, bound)
	}

	// Each member keeps its own whole route and is refused every gate of the other.
	lightGates := []int{lukeLightEnergyMapID, lukeLightAltarMapID, lukeLightTempleMapID, lukeLightThroneMapID}
	darkGates := []int{lukeDarkEnergyMapID, lukeDarkAltarMapID, lukeDarkTempleMapID, lukeDarkThroneMapID}
	for _, mapID := range lightGates {
		if !runtimeRaidLukeRouteEntryAllowedLocked(state, light, mapID) {
			t.Fatalf("light member refused its own gate %d", mapID)
		}
		if runtimeRaidLukeRouteEntryAllowedLocked(state, dark, mapID) {
			t.Fatalf("dark member entered light gate %d", mapID)
		}
	}
	for _, mapID := range darkGates {
		if !runtimeRaidLukeRouteEntryAllowedLocked(state, dark, mapID) {
			t.Fatalf("dark member refused its own gate %d", mapID)
		}
		if runtimeRaidLukeRouteEntryAllowedLocked(state, light, mapID) {
			t.Fatalf("light member entered dark gate %d", mapID)
		}
	}

	// The two shared rooms bind nobody and block nobody.
	for _, mapID := range []int{lukeEnergyControlMapID, lukeEnergySaveMapID} {
		for _, charID := range []uint16{light, dark} {
			if !runtimeRaidLukeRouteEntryAllowedLocked(state, charID, mapID) {
				t.Fatalf("char %d refused shared room %d", charID, mapID)
			}
		}
		if _, bound := bindRuntimeRaidLukeRouteLocked(state, 1003, mapID); bound {
			t.Fatalf("shared room %d bound a route", mapID)
		}
	}

	// A single raid is never route-locked.
	state.TailFlag = 1
	if !runtimeRaidLukeRouteEntryAllowedLocked(state, dark, lukeLightThroneMapID) {
		t.Fatal("a single raid was route-locked")
	}
}

func TestLukeEnergyControlRecoveryFollowsGeneratorTier(t *testing.T) {
	// [1PHASE OBJ DESTROY] 0 0 5 / 1 6 10 / 2 11 15 picks 133, and luke.etc
	// arms 45 / 90 / 120 s from it; the window is the same on every cycle.
	for _, test := range []struct {
		name      string
		destroyed [3]uint32
		tier      uint32
		want      time.Duration
	}{
		{name: "no generators", destroyed: [3]uint32{0, 0, 0}, tier: 0, want: 45 * time.Second},
		{name: "six generators", destroyed: [3]uint32{5, 1, 0}, tier: 1, want: 90 * time.Second},
		{name: "all fifteen", destroyed: [3]uint32{5, 5, 5}, tier: 2, want: 120 * time.Second},
	} {
		t.Run(test.name, func(t *testing.T) {
			lukeEnergyControlCycles(t, test.destroyed, test.tier, test.want)
		})
	}
}

func lukeEnergyControlCycles(t *testing.T, destroyed [3]uint32, tier uint32, want time.Duration) {
	t.Helper()
	state := lukeTeamPhaseTwoState()
	state.LukeConfig = lukeGeneratorTestConfig()
	setRuntimeRaidLukeSymbolLocked(state, lukeSymbolBirthSanctumObject, destroyed[0])
	setRuntimeRaidLukeSymbolLocked(state, lukeSymbolExtinctionSanctumObject, destroyed[1])
	setRuntimeRaidLukeSymbolLocked(state, lukeSymbolSpreadSanctumObject, destroyed[2])
	now := time.Unix(1700000000, 0).UTC()
	initRuntimeRaidLukePhaseTwoLocked(state, now)
	if got := runtimeRaidLukeSymbolLocked(state, lukeSymbolEnergyControlOpen); got != tier {
		t.Fatalf("[ENERGY CONTROL OPEN]=%d want tier %d", got, tier)
	}
	lukeClear(t, state, lukeLightEntranceMapID, now)
	lukeClear(t, state, lukeDarkEntranceMapID, now)

	for index := 0; index < 4; index++ {
		armed := lukeClear(t, state, lukeEnergyControlMapID, now)
		if len(armed) != 1 || armed[0].Kind != runtimeRaidMechanismRecovery || armed[0].MapID != lukeEnergyControlMapID {
			t.Fatalf("cycle %d armed=%+v", index, armed)
		}
		if got := armed[0].Deadline.Sub(now); got != want {
			t.Fatalf("cycle %d recovery=%s want=%s", index, got, want)
		}
		if got := lukeGateState(t, state, lukeEnergyControlMapID); got != raid.AntonDungeonClosed {
			t.Fatalf("cycle %d energy control=%q want close", index, got)
		}
		if got := runtimeRaidLukeSymbolLocked(state, lukeSymbolEnergyControlEffect); got != 0 {
			t.Fatalf("cycle %d [ENERGY CONTROL EFFECT]=%d want=0 while closed", index, got)
		}

		next, outcome := fireRuntimeRaidLukeMechanismLocked(state, armed[0], now.Add(want))
		if outcome != "energy_control_reopened" || len(next) != 0 {
			t.Fatalf("cycle %d reopen outcome=%q next=%+v", index, outcome, next)
		}
		if got := lukeGateState(t, state, lukeEnergyControlMapID); got != raid.AntonDungeonOpen {
			t.Fatalf("cycle %d energy control=%q want open", index, got)
		}
		if got := runtimeRaidLukeSymbolLocked(state, lukeSymbolEnergyControlEffect); got != 1 {
			t.Fatalf("cycle %d [ENERGY CONTROL EFFECT]=%d want=1 while open", index, got)
		}
	}
	if got := runtimeRaidLukeSymbolLocked(state, lukeSymbolEnergyControlOpen); got != tier {
		t.Fatalf("cycles moved [ENERGY CONTROL OPEN] to %d; it is the tier %d, not a cycle counter", got, tier)
	}
}

func TestLukeEnergySaveWipesTheRosterWhenItsClockRunsOut(t *testing.T) {
	state := lukeTeamPhaseTwoState()
	now := time.Unix(1700000000, 0).UTC()
	setRuntimeRaidLukeDungeonStateLocked(state, lukeLightThroneMapID, raid.AntonDungeonOpen)

	// Reaching the throne's boss room opens the energy save room and starts its clock.
	opened, armed := runtimeRaidLukeEnterMapLocked(state, lukeLightThroneMapID, lukeLightThroneBossRoomMapID, now)
	if !opened {
		t.Fatal("entering the throne boss room did not open the energy save room")
	}
	if len(armed) != 1 || armed[0].Deadline.Sub(now) != lukeEnergySaveWindow {
		t.Fatalf("energy save window=%+v want %s", armed, lukeEnergySaveWindow)
	}
	if got := runtimeRaidLukeSymbolLocked(state, lukeSymbolEnergySaveEffect); got != lukeEnergySaveEffectActive {
		t.Fatalf("[ENERGY SAVE EFFECT]=%d want=%d", got, lukeEnergySaveEffectActive)
	}

	next, outcome := fireRuntimeRaidLukeMechanismLocked(state, armed[0], now.Add(lukeEnergySaveWindow))
	if outcome != "energy_save_expired" || len(next) != 0 {
		t.Fatalf("expiry outcome=%q next=%+v", outcome, next)
	}
	if got := state.Lifecycle.MovieSymbols[lukeSymbolKillAllCharacter]; got != 1 {
		t.Fatalf("[KILL ALL CHARACTER]=%d want=1", got)
	}
	if got := runtimeRaidLukeSymbolLocked(state, lukeSymbolEnergySaveEffect); got != lukeEnergySaveEffectFailed {
		t.Fatalf("[ENERGY SAVE EFFECT]=%d want=%d", got, lukeEnergySaveEffectFailed)
	}
	if got := lukeGateState(t, state, lukeEnergySaveMapID); got != raid.AntonDungeonHide {
		t.Fatalf("energy save room=%q want hide", got)
	}
}

func TestLukeEnergySaveClockIsNotRestartedWhileItIsAlreadyRunning(t *testing.T) {
	state := lukeTeamPhaseTwoState()
	now := time.Unix(1700000000, 0).UTC()
	setRuntimeRaidLukeDungeonStateLocked(state, lukeLightThroneMapID, raid.AntonDungeonOpen)
	setRuntimeRaidLukeDungeonStateLocked(state, lukeDarkThroneMapID, raid.AntonDungeonOpen)
	opened, armed := runtimeRaidLukeEnterMapLocked(state, lukeLightThroneMapID, lukeLightThroneBossRoomMapID, now)
	if !opened || len(armed) != 1 {
		t.Fatalf("boss room opened=%t window=%+v want one timer", opened, armed)
	}
	deadline := armed[0].Deadline

	// The rest of the squad reaching the boss room, the other throne's squad
	// reaching its own boss room, and squads walking in and out of 能源储藏室 must
	// all leave the clock alone.
	later := now.Add(90 * time.Second)
	for _, step := range []struct {
		dungeon int
		room    int64
	}{
		{lukeLightThroneMapID, lukeLightThroneBossRoomMapID},
		{lukeDarkThroneMapID, lukeDarkThroneBossRoomMapID},
		{lukeEnergySaveMapID, lukeLightThroneBossRoomMapID},
	} {
		if again, rearmed := runtimeRaidLukeEnterMapLocked(state, step.dungeon, step.room, later); again || len(rearmed) != 0 {
			t.Fatalf("entering %d/%d reopened=%t rearmed=%+v", step.dungeon, step.room, again, rearmed)
		}
	}
	if runtimeRaidLukeEnterDungeonLocked(state, lukeEnergySaveMapID) {
		t.Fatal("loading into 能源储藏室 reopened it")
	}
	if again := armRuntimeRaidLukeEnergySaveWindowLocked(state, later); len(again) != 0 {
		t.Fatalf("re-arm returned %+v want none", again)
	}
	current, ok := state.Lifecycle.Mechanisms[runtimeRaidMechanismKey{
		Kind: runtimeRaidMechanismActive, MapID: lukeEnergySaveMapID,
	}]
	if !ok || !current.Deadline.Equal(deadline) {
		t.Fatalf("wipe clock deadline=%v ok=%t want the original %v", current.Deadline, ok, deadline)
	}
}

// luke.etc opens 能源储藏室 on [ON ENTER MAP] 55845 / 55846, the throne's second
// (boss) room -- not on entering the throne. A squad standing in the start room
// must not have started anybody's 150-second clock.
func TestLukeEnergySaveOpensOnTheThroneBossRoomNotTheThroneGate(t *testing.T) {
	state := lukeTeamPhaseTwoState()
	now := time.Unix(1700000000, 0).UTC()
	setRuntimeRaidLukeDungeonStateLocked(state, lukeLightThroneMapID, raid.AntonDungeonOpen)

	if runtimeRaidLukeEnterDungeonLocked(state, lukeLightThroneMapID) {
		t.Fatal("loading into the throne changed the graph")
	}
	const lightThroneStartRoomMapID int64 = 55849
	if opened, armed := runtimeRaidLukeEnterMapLocked(state, lukeLightThroneMapID, lightThroneStartRoomMapID, now); opened || len(armed) != 0 {
		t.Fatalf("throne start room opened=%t armed=%+v", opened, armed)
	}
	// The boss room of a throne only counts inside a throne gate.
	if opened, _ := runtimeRaidLukeEnterMapLocked(state, lukeLightTempleMapID, lukeLightThroneBossRoomMapID, now); opened {
		t.Fatal("a non-throne gate opened the energy save room")
	}
	if got := lukeGateState(t, state, lukeEnergySaveMapID); got != raid.AntonDungeonHide {
		t.Fatalf("energy save room=%q before the boss room, want hide", got)
	}
	if got := runtimeRaidLukeSymbolLocked(state, lukeSymbolEnergySaveEffect); got != 0 {
		t.Fatalf("[ENERGY SAVE EFFECT]=%d before the boss room, want 0", got)
	}

	bossAt := now.Add(40 * time.Second)
	opened, armed := runtimeRaidLukeEnterMapLocked(state, lukeLightThroneMapID, lukeLightThroneBossRoomMapID, bossAt)
	if !opened || len(armed) != 1 || !armed[0].Deadline.Equal(bossAt.Add(lukeEnergySaveWindow)) {
		t.Fatalf("boss room opened=%t armed=%+v want the clock from %v", opened, armed, bossAt)
	}
	if got := lukeGateState(t, state, lukeEnergySaveMapID); got != raid.AntonDungeonOpen {
		t.Fatalf("energy save room=%q after the boss room, want open", got)
	}
}

// The storage room is a repeatable objective: after a clear it closes, reopens
// 120 s later with a fresh 150 s clock, and must be enterable and clearable
// again -- every time. This walks the committed-clear path twice with the same
// gate conditions validateAntonRaidDungeonEntry checks.
func TestLukeEnergySaveCanBeClearedAgainAfterItRespawns(t *testing.T) {
	state := lukeTeamPhaseTwoState()
	now := time.Unix(1700000000, 0).UTC()
	setRuntimeRaidLukeDungeonStateLocked(state, lukeLightThroneMapID, raid.AntonDungeonOpen)
	if opened, _ := runtimeRaidLukeEnterMapLocked(state, lukeLightThroneMapID, lukeLightThroneBossRoomMapID, now); !opened {
		t.Fatal("boss room did not open the energy save room")
	}
	enterable := func(label string) {
		t.Helper()
		objective := state.Lifecycle.Objectives[lukeEnergySaveMapID]
		if objective.State != raid.AntonDungeonOpen || objective.RemainingHP <= 0 {
			t.Fatalf("%s: energy save state=%q remaining=%d, not enterable", label, objective.State, objective.RemainingHP)
		}
		if !runtimeRaidLukeRouteEntryAllowedLocked(state, 42, lukeEnergySaveMapID) {
			t.Fatalf("%s: route lock refuses the energy save room", label)
		}
	}
	clock := now
	for cycle := 1; cycle <= 3; cycle++ {
		enterable(fmt.Sprintf("cycle %d before clear", cycle))
		clock = clock.Add(60 * time.Second)
		if err := advanceRuntimeRaidObjectiveLocked(state, runtimeRaidDungeonPhaseTwo, lukeEnergySaveMapID); err != nil {
			t.Fatalf("cycle %d advance: %v", cycle, err)
		}
		armed := applyRuntimeRaidLukePhaseTwoClearLocked(state, lukeEnergySaveMapID, clock)
		if len(armed) != 1 || armed[0].Kind != runtimeRaidMechanismRecovery {
			t.Fatalf("cycle %d clear armed=%+v want the 120 s recovery", cycle, armed)
		}
		if _, running := state.Lifecycle.Mechanisms[runtimeRaidMechanismKey{Kind: runtimeRaidMechanismActive, MapID: lukeEnergySaveMapID}]; running {
			t.Fatalf("cycle %d: the wipe clock survived the clear", cycle)
		}
		clock = armed[0].Deadline
		next, outcome := fireRuntimeRaidLukeMechanismLocked(state, armed[0], clock)
		if outcome != "energy_save_reopened" || len(next) != 1 || next[0].Kind != runtimeRaidMechanismActive {
			t.Fatalf("cycle %d reopen outcome=%q next=%+v", cycle, outcome, next)
		}
		if got := runtimeRaidLukeSymbolLocked(state, lukeSymbolEnergySaveEffect); got != lukeEnergySaveEffectActive {
			t.Fatalf("cycle %d [ENERGY SAVE EFFECT]=%d after reopen", cycle, got)
		}
	}
	enterable("after the third respawn")
}

func TestLukeThroneGiveUpDisarmsTheEnergySaveClock(t *testing.T) {
	state := lukeTeamPhaseTwoState()
	now := time.Unix(1700000000, 0).UTC()
	setRuntimeRaidLukeDungeonStateLocked(state, lukeLightThroneMapID, raid.AntonDungeonOpen)
	if opened, _ := runtimeRaidLukeEnterMapLocked(state, lukeLightThroneMapID, lukeLightThroneBossRoomMapID, now); !opened {
		t.Fatal("entering the throne boss room did not open the energy save room")
	}

	runtimeRaidLukeGiveUpDungeonLocked(state, lukeLightThroneMapID)

	// [ON CHANGE DUNGEON STATE] 5066 `hide` resets timers 1 and 2: an abandoned
	// throne must not be wiped by a clock that outlived it.
	for _, kind := range []byte{runtimeRaidMechanismActive, runtimeRaidMechanismRecovery} {
		key := runtimeRaidMechanismKey{Kind: kind, MapID: lukeEnergySaveMapID}
		if _, armed := state.Lifecycle.Mechanisms[key]; armed {
			t.Fatalf("timer kind=%d survived the throne give-up", kind)
		}
	}
	if got := lukeGateState(t, state, lukeEnergySaveMapID); got != raid.AntonDungeonHide {
		t.Fatalf("energy save room=%q want hide", got)
	}
}

func TestLukeEnergySaveClearedRoomReopensAfterTwoMinutes(t *testing.T) {
	state := lukeTeamPhaseTwoState()
	now := time.Unix(1700000000, 0).UTC()
	setRuntimeRaidLukeDungeonStateLocked(state, lukeEnergySaveMapID, raid.AntonDungeonOpen)
	armRuntimeRaidLukeEnergySaveWindowLocked(state, now)

	armed := applyRuntimeRaidLukePhaseTwoClearLocked(state, lukeEnergySaveMapID, now)
	if len(armed) != 1 || armed[0].Kind != runtimeRaidMechanismRecovery ||
		armed[0].Deadline.Sub(now) != lukeEnergySaveRecovery {
		t.Fatalf("energy save recovery=%+v want %s", armed, lukeEnergySaveRecovery)
	}
	if got := runtimeRaidLukeSymbolLocked(state, lukeSymbolEnergySaveEffect); got != 0 {
		t.Fatalf("[ENERGY SAVE EFFECT]=%d want=0 after the clear", got)
	}
	// The wipe clock must be gone, or a cleared room would still kill the roster.
	if _, armedStill := state.Lifecycle.Mechanisms[runtimeRaidMechanismKey{
		Kind: runtimeRaidMechanismActive, MapID: lukeEnergySaveMapID,
	}]; armedStill {
		t.Fatal("the 150s wipe clock survived the clear")
	}

	next, outcome := fireRuntimeRaidLukeMechanismLocked(state, armed[0], now.Add(lukeEnergySaveRecovery))
	if outcome != "energy_save_reopened" {
		t.Fatalf("reopen outcome=%q", outcome)
	}
	if len(next) != 1 || next[0].Kind != runtimeRaidMechanismActive ||
		next[0].Deadline.Sub(now.Add(lukeEnergySaveRecovery)) != lukeEnergySaveWindow {
		t.Fatalf("reopened room armed=%+v, want a fresh %s window", next, lukeEnergySaveWindow)
	}
}

func TestLukeGiveUpThroneClosesTheEnergySaveRoom(t *testing.T) {
	state := lukeTeamPhaseTwoState()
	now := time.Unix(1700000000, 0).UTC()
	setRuntimeRaidLukeDungeonStateLocked(state, lukeEnergySaveMapID, raid.AntonDungeonOpen)
	armRuntimeRaidLukeEnergySaveWindowLocked(state, now)

	runtimeRaidLukeGiveUpDungeonLocked(state, lukeLightThroneMapID)
	if got := lukeGateState(t, state, lukeEnergySaveMapID); got != raid.AntonDungeonHide {
		t.Fatalf("energy save room=%q want hide", got)
	}
	if got := runtimeRaidLukeSymbolLocked(state, lukeSymbolEnergySaveEffect); got != 0 {
		t.Fatalf("[ENERGY SAVE EFFECT]=%d want=0", got)
	}
}

// TestLukeTeamSnapshotCarriesEveryGateRow is the regression for the empty
// 战况表 and the empty dungeon selection page. Live 2026-09-23T01:59:34Z, raid
// 1835009: op585 went out as 00 00 00000000 -- zero dungeon rows -- because the
// snapshot builder skipped any gate whose map id had no "<id> DGN HP" symbol,
// and no Luke team gate had one. op583 carried only the four phase-one symbols
// this runtime sets itself (107, 117, 118, 119).
func TestLukeTeamSnapshotCarriesEveryGateRow(t *testing.T) {
	for _, phase := range []struct {
		name  string
		state *runtimeRaidState
		gates map[int]uint32
	}{
		{
			name:  "phase one",
			state: lukeTeamPhaseOneState(),
			gates: map[int]uint32{5054: 67, 5055: 68, 5056: 69},
		},
		{
			name:  "phase two",
			state: lukeTeamPhaseTwoState(),
			gates: map[int]uint32{
				5311: 82, 5313: 84, 5057: 70, 5058: 71, 5059: 73, 5060: 74,
				5063: 85, 5064: 86, 5065: 87, 5061: 75, 5062: 76, 5066: 88,
			},
		},
	} {
		t.Run(phase.name, func(t *testing.T) {
			// 本树：viewer 版是独立函数（原 1 参版保持共享视图，签名不变），
			// 且多返回一个 dungeonRows。
			symbolBody, dungeonBody, _, err := runtimeRaidPhaseSnapshotBodiesForViewer(*phase.state, 0)
			if err != nil {
				t.Fatalf("snapshot bodies: %v", err)
			}
			// op585 is [reserved][count] then five bytes per row.
			if len(dungeonBody) < 2 || int(dungeonBody[1]) != len(phase.gates) {
				t.Fatalf("dungeon rows=%d want=%d body=% X", dungeonBody[1], len(phase.gates), dungeonBody)
			}
			seen := map[uint32]bool{}
			for index := 0; index < len(phase.gates); index++ {
				offset := 2 + index*5
				seen[binary.LittleEndian.Uint32(dungeonBody[offset:offset+4])] = true
			}
			for mapID := range phase.gates {
				if !seen[uint32(mapID)] {
					t.Fatalf("gate %d missing from op585: % X", mapID, dungeonBody)
				}
			}

			// Every gate also owns a DGN HP symbol from etc/raid/luke/luke.etc.symbol.
			for mapID, wantKey := range phase.gates {
				key, ok := runtimeRaidObjectiveSymbolKey(mapID)
				if !ok || key != wantKey {
					t.Fatalf("gate %d symbol=(%d,%t) want=%d", mapID, key, ok, wantKey)
				}
			}
			if len(symbolBody) == 0 || int(symbolBody[0]) < len(phase.gates) {
				t.Fatalf("symbol rows=%v want at least %d", symbolBody, len(phase.gates))
			}
		})
	}
}

// A gate the symbol table does not know must still reach the selection page.
func TestRaidSnapshotKeepsGateRowWithoutASymbolKey(t *testing.T) {
	state := lukeTeamPhaseOneState()
	state.Lifecycle.Objectives[9999] = runtimeRaidDungeonObjective{
		Phase: runtimeRaidDungeonPhaseOne, MapID: 9999, State: raid.AntonDungeonOpen,
		MaxHP: 1, RemainingHP: 1, PartyMax: 1,
	}
	if _, ok := runtimeRaidObjectiveSymbolKey(9999); ok {
		t.Fatal("9999 unexpectedly has a symbol key")
	}
	_, dungeonBody, _, err := runtimeRaidPhaseSnapshotBodiesForViewer(*state, 0)
	if err != nil {
		t.Fatalf("snapshot bodies: %v", err)
	}
	if int(dungeonBody[1]) != 4 {
		t.Fatalf("dungeon rows=%d want=4 (three sanctums plus the symbol-less gate)", dungeonBody[1])
	}
}

// TestLukeTeamGatesAreRaidEntriesNotOrdinaryDungeons is the regression for
// "选择进入副本 无法进入". runtimeRaidDungeonPhaseForMap is what marks a
// SelectDungeon request as a raid entry, and it knew no Luke team map, so
// entering 5054 from the Jelva waiting area was judged as an ordinary dungeon:
//
//	game-dungeon-select-entry-policy  anton_raid=false raid_started=false
//	game-dungeon-select-blocked       reason=ordinary_dungeon_town_return_origin_unavailable
//	                                  origin 1/32/4 does not match runtime 1/38/1
//
// 32/4 is the Luke raid waiting room ([WAITING ROOM] 32 4); 38/1 is the ordinary
// town the gate's worldmap entry belongs to.
func TestLukeTeamGatesAreRaidEntriesNotOrdinaryDungeons(t *testing.T) {
	phaseOne := []int{5054, 5055, 5056}
	phaseTwo := []int{5311, 5313, 5057, 5058, 5059, 5060, 5063, 5064, 5065, 5061, 5062, 5066}

	for _, mapID := range phaseOne {
		phase, ok := runtimeRaidDungeonPhaseForMap(mapID)
		if !ok || phase != runtimeRaidDungeonPhaseOne {
			t.Fatalf("gate %d phase=(%d,%t), want phase one", mapID, phase, ok)
		}
	}
	for _, mapID := range phaseTwo {
		phase, ok := runtimeRaidDungeonPhaseForMap(mapID)
		if !ok || phase != runtimeRaidDungeonPhaseTwo {
			t.Fatalf("gate %d phase=(%d,%t), want phase two", mapID, phase, ok)
		}
	}
	// This predicate is also what isAntonRaidEntryDungeonID answers, which is
	// what skips the ordinary level/fatigue policy and what routes the
	// town-return origin to the raid waiting room instead of an ordinary town.
	for _, mapID := range append(append([]int{}, phaseOne...), phaseTwo...) {
		if !isAntonRaidEntryDungeonID(int64(mapID)) {
			t.Fatalf("gate %d is not treated as a raid entry", mapID)
		}
	}

	// The Anton ranges and the Luke single route keep their own answers.
	for mapID, want := range map[int]byte{
		210: runtimeRaidDungeonPhaseOne, 216: runtimeRaidDungeonPhaseOne,
		218: runtimeRaidDungeonPhaseTwo, 224: runtimeRaidDungeonPhaseTwo,
		5314: runtimeRaidDungeonPhaseOne, 5323: runtimeRaidDungeonPhaseOne,
	} {
		phase, ok := runtimeRaidDungeonPhaseForMap(mapID)
		if !ok || phase != want {
			t.Fatalf("gate %d phase=(%d,%t) want=%d", mapID, phase, ok, want)
		}
	}
	// An ordinary dungeon must stay ordinary.
	if _, ok := runtimeRaidDungeonPhaseForMap(5100); ok {
		t.Fatal("5100 was promoted to a raid gate")
	}
}

// TestLukeRouteLockedGatesDrawClosedForThatMemberOnly pins the per-viewer
// snapshot: a member bound to one route sees the other route's open gates as
// closed (wire 1), which is the locked cell the board already draws for a gate it
// cannot enter. An unbound member keeps seeing them open, and the objective's own
// state is never changed.
func TestLukeRouteLockedGatesDrawClosedForThatMemberOnly(t *testing.T) {
	state := lukeTeamPhaseTwoState()
	initRuntimeRaidLukePhaseTwoLocked(state, time.Unix(1700000000, 0).UTC())
	const dark, unbound = uint16(1001), uint16(1002)
	bindRuntimeRaidLukeRouteLocked(state, dark, lukeDarkEntranceMapID)

	gateState := func(body []byte, mapID uint32) (uint32, bool) {
		rows := int(body[1])
		for index := 0; index < rows; index++ {
			offset := 2 + index*5
			if binary.LittleEndian.Uint32(body[offset:offset+4]) == mapID {
				return uint32(body[offset+4]), true
			}
		}
		return 0, false
	}

	// The dark-bound member sees the light entrance closed...
	_, darkBody, _, err := runtimeRaidPhaseSnapshotBodiesForViewer(*state, dark)
	if err != nil {
		t.Fatalf("dark viewer snapshot: %v", err)
	}
	got, ok := gateState(darkBody, lukeLightEntranceMapID)
	if !ok || got != runtimeRaidObjectiveWireState(raid.AntonDungeonClosed) {
		t.Fatalf("light entrance for the dark member = (%d,%t), want closed", got, ok)
	}
	// ...and its own route still open.
	if got, ok := gateState(darkBody, lukeDarkEntranceMapID); !ok || got != runtimeRaidObjectiveWireState(raid.AntonDungeonOpen) {
		t.Fatalf("dark entrance for the dark member = (%d,%t), want open", got, ok)
	}

	// An unbound member sees both routes open.
	_, freeBody, _, err := runtimeRaidPhaseSnapshotBodiesForViewer(*state, unbound)
	if err != nil {
		t.Fatalf("unbound viewer snapshot: %v", err)
	}
	for _, mapID := range []uint32{lukeLightEntranceMapID, lukeDarkEntranceMapID} {
		if got, ok := gateState(freeBody, mapID); !ok || got != runtimeRaidObjectiveWireState(raid.AntonDungeonOpen) {
			t.Fatalf("gate %d for an unbound member = (%d,%t), want open", mapID, got, ok)
		}
	}

	// The authoritative objective is untouched; only the view changed.
	if got := lukeGateState(t, state, lukeLightEntranceMapID); got != raid.AntonDungeonOpen {
		t.Fatalf("light entrance objective = %q, want open", got)
	}
}

// TestGatePartyMaxAdmitsExactlyTheDeclaredSquadCount covers "同一圣所同时只容一支
// 队伍". [PARTY MAX] was parsed and stored but never enforced, so a second squad
// walked into a gate another was already fighting: live 2026-09-23T08:27Z both
// members opened their own run of 5313 with separate lifecycle tokens.
func TestGatePartyMaxAdmitsExactlyTheDeclaredSquadCount(t *testing.T) {
	const gate = lukeDarkEntranceMapID
	seatOf := func(group uint8) runtimeRaidDungeonSeat {
		return runtimeRaidDungeonSeat{Group: group}
	}
	newState := func(partyMax int, occupants ...uint8) *runtimeRaidState {
		state := &runtimeRaidState{
			RaidMode: runtimeRaidOwnerLuke,
			Lifecycle: runtimeRaidLifecycleState{
				Phase: runtimeRaidDungeonPhaseTwo,
				Objectives: map[int]runtimeRaidDungeonObjective{
					gate: {
						Phase: runtimeRaidDungeonPhaseTwo, MapID: gate,
						State: raid.AntonDungeonOpen, MaxHP: 1, RemainingHP: 1,
						PartyMax: partyMax,
					},
				},
				Dungeons: map[runtimeRaidDungeonRunKey]runtimeRaidDungeonProgress{},
			},
		}
		for _, group := range occupants {
			state.Lifecycle.Dungeons[runtimeRaidDungeonRunKey{MapID: gate, Seat: seatOf(group)}] =
				runtimeRaidDungeonProgress{
					Phase: runtimeRaidDungeonPhaseTwo, MapID: gate, Seat: seatOf(group),
					Status: runtimeRaidDungeonProgressLoaded,
				}
		}
		return state
	}

	// Luke: one squad already inside, a different squad is refused.
	if _, _, full := runtimeRaidGatePartyMaxReachedLocked(newState(1, 1), seatOf(2), gate); !full {
		t.Fatal("a second squad entered a PARTY MAX 1 gate")
	}
	// The squad already inside is not blocked by its own run.
	if _, _, full := runtimeRaidGatePartyMaxReachedLocked(newState(1, 1), seatOf(1), gate); full {
		t.Fatal("the occupying squad was refused its own gate")
	}
	// An empty gate admits the first squad.
	if _, _, full := runtimeRaidGatePartyMaxReachedLocked(newState(1), seatOf(1), gate); full {
		t.Fatal("the first squad was refused an empty gate")
	}
	// A completed run does not hold the gate.
	completed := newState(1, 1)
	for key, progress := range completed.Lifecycle.Dungeons {
		progress.Status = runtimeRaidDungeonProgressCompleted
		completed.Lifecycle.Dungeons[key] = progress
	}
	if _, _, full := runtimeRaidGatePartyMaxReachedLocked(completed, seatOf(2), gate); full {
		t.Fatal("a completed run still held the gate")
	}

	// Anton declares 5 on its gates and must still admit several squads; the
	// blanket cross-squad refusal this replaces was removed for exactly that.
	if _, _, full := runtimeRaidGatePartyMaxReachedLocked(newState(5, 1, 2, 3), seatOf(4), gate); full {
		t.Fatal("a fourth squad was refused a PARTY MAX 5 gate")
	}
	if _, _, full := runtimeRaidGatePartyMaxReachedLocked(newState(5, 1, 2, 3, 4, 5), seatOf(6), gate); !full {
		t.Fatal("a sixth squad entered a PARTY MAX 5 gate")
	}
}

func TestLukeSymbolRowsRideTheSnapshotSortedAndOwnerScoped(t *testing.T) {
	state := lukeTeamPhaseTwoState()
	initRuntimeRaidLukePhaseTwoLocked(state, time.Unix(1700000000, 0).UTC())

	rows := runtimeRaidLukeSymbolRows(*state)
	if len(rows) == 0 {
		t.Fatal("no Luke symbol rows")
	}
	for index := 1; index < len(rows); index++ {
		if rows[index-1].Key >= rows[index].Key {
			t.Fatalf("rows not sorted by key: %+v", rows)
		}
	}
	seen := map[uint32]bool{}
	for _, row := range rows {
		seen[row.Key] = true
	}
	for _, key := range []uint32{lukeSymbolPhaseTwoInit, lukeSymbolAltarEntranceClear, lukeSymbolEnergyControlEffect} {
		if !seen[key] {
			t.Fatalf("symbol %d missing from the snapshot rows", key)
		}
	}

	// An Anton raid must never receive Luke's symbol namespace.
	state.RaidMode = runtimeRaidOwnerAnton
	if rows := runtimeRaidLukeSymbolRows(*state); len(rows) != 0 {
		t.Fatalf("anton snapshot carried Luke rows: %+v", rows)
	}
}

// The raid-skip-to-luke-throne test aid walks the real clear hooks, so it must
// leave exactly the state a played phase two leaves in front of the thrones.
func TestLukeThroneFastForwardLeavesTheFinalStageOpen(t *testing.T) {
	state := lukeTeamPhaseTwoState()
	state.FrozenMembers = []runtimeRaidMemberState{{CharID: 1, GroupIndex: 1}, {CharID: 42, GroupIndex: 2}}
	now := time.Unix(1700000000, 0).UTC()
	initRuntimeRaidLukePhaseTwoLocked(state, now)
	fastForwardRuntimeRaidLukeToThroneLocked(state, now)

	for mapID, want := range map[int]raid.AntonDungeonState{
		lukeLightThroneMapID:   raid.AntonDungeonOpen,
		lukeDarkThroneMapID:    raid.AntonDungeonHide,
		lukeEnergySaveMapID:    raid.AntonDungeonHide,
		lukeEnergyControlMapID: raid.AntonDungeonHide,
	} {
		if got := lukeGateState(t, state, mapID); got != want {
			t.Fatalf("gate %d=%q want %q", mapID, got, want)
		}
	}
	for _, mapID := range lukeThroneFastForwardGates {
		if got := state.Lifecycle.Objectives[mapID].RemainingHP; got != 0 {
			t.Fatalf("gate %d remaining=%d want cleared", mapID, got)
		}
	}
	if got := runtimeRaidLukeSymbolLocked(state, lukeSymbolEnergyControlEffect); got != 0 {
		t.Fatalf("[ENERGY CONTROL EFFECT]=%d after the altars, want 0", got)
	}
	if got := runtimeRaidLukeSymbolLocked(state, lukeSymbolTempleClear); got != lukeTempleRequired {
		t.Fatalf("[TEMPLE CLEAR]=%d want %d", got, lukeTempleRequired)
	}
}
