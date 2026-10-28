package dnfbridge

import (
	"fmt"
	"sort"
	"time"

	"longheng.io/server/internal/modules/dnf/raid"
)

// Luke's team raid is two phases of gate graph, transcribed from the [TRIGGER]
// / [BEHAVIOR] pairs of etc/raid/luke/luke.etc. Every symbol id below is from
// etc/raid/luke/luke.etc.symbol, which is a different namespace from Anton's:
// the same number means a different thing in each file, so nothing here may be
// shared with raid_anton_phase_*.go.
//
// Phase one, 进入源生宫殿: three sanctums, three clears each. Only 诞生 (5054)
// starts open; entering it opens the other two. Whichever of 灭亡 (5055) /
// 扩散 (5056) clears first decides [2PHASE DUNGEON TYPE]. All three clear ->
// [1PHASE SUCCESS].
//
// Phase two, 进入卓越圣殿: the two entrances open together, then the light chain
// 5057 -> 5059 -> 5064 and the dark chain 5058 -> 5060 -> 5065 run in parallel,
// gated by the [ALTAR ENTERANCE CLEAR] / [ALTAR CLEAR] / [TEMPLE CLEAR] counters.
// Both temples down opens 机械王座:明 (5061); clearing it opens 机械王座:暗
// (5062), and clearing that ends the phase. The energy control room (5063) and
// the energy save room (5066) are the two shared rooms that run on timers.
const (
	lukeBirthSanctumMapID      = 5054
	lukeExtinctionSanctumMapID = 5055
	lukeSpreadSanctumMapID     = 5056

	lukeLightEntranceMapID = 5311
	lukeDarkEntranceMapID  = 5313
	lukeLightEnergyMapID   = 5057
	lukeDarkEnergyMapID    = 5058
	lukeLightAltarMapID    = 5059
	lukeDarkAltarMapID     = 5060
	lukeEnergyControlMapID = 5063
	lukeLightTempleMapID   = 5064
	lukeDarkTempleMapID    = 5065
	lukeLightThroneMapID   = 5061
	lukeDarkThroneMapID    = 5062
	lukeEnergySaveMapID    = 5066

	// Boss rooms inside the two thrones (map ids, not dungeon ids).
	lukeLightThroneBossRoomMapID int64 = 55845
	lukeDarkThroneBossRoomMapID  int64 = 55846

	lukeSymbolKillAllCharacter        uint32 = 102
	lukeSymbolKickOutCharacter        uint32 = 103
	lukeSymbolPhaseOneFail            uint32 = 104
	lukeSymbolPhaseOneSuccess         uint32 = 105
	lukeSymbolPhaseTwoInit            uint32 = 106
	lukeSymbolPhaseOneInit            uint32 = 107
	lukeSymbolPhaseTwoDungeonType     uint32 = 108
	lukeSymbolBirthSanctumObject      uint32 = 117
	lukeSymbolExtinctionSanctumObject uint32 = 118
	lukeSymbolSpreadSanctumObject     uint32 = 119
	lukeSymbolTempleClear             uint32 = 122
	lukeSymbolAltarClear              uint32 = 124
	lukeSymbolEnergyControlOpen       uint32 = 133
	lukeSymbolEnergyControlEffect     uint32 = 138
	lukeSymbolEnergySaveEffect        uint32 = 139
	lukeSymbolAltarEntranceClear      uint32 = 140

	// [2PHASE DUNGEON TYPE] records which branch sanctum fell first. The PVF
	// sets it but never reads it back in phase two, so it is carried as raid
	// state the client can read rather than used as a gate here.
	lukePhaseTwoDungeonTypeExtinction uint32 = 1
	lukePhaseTwoDungeonTypeSpread     uint32 = 2

	// [ENERGY SAVE EFFECT] values: 1 while the room is open and its clock runs,
	// 2 once the clock ran out and the roster was wiped, 0 otherwise.
	lukeEnergySaveEffectActive uint32 = 1
	lukeEnergySaveEffectFailed uint32 = 2

	lukePhaseTwoOpeningWindow = 10 * time.Second
	lukeEnergySaveWindow      = 150 * time.Second
	lukeEnergySaveRecovery    = 120 * time.Second

	lukeAltarEntranceRequired = 2
	lukeAltarRequired         = 2
	lukeTempleRequired        = 2
)

// lukeEnergyControlRecovery is how long the energy control room stays closed
// after each clear. etc/raid/luke/luke.etc branches on [ENERGY CONTROL OPEN]
// being 0, 1 or 2 and arms 45, 90 or 120 seconds. The PVF only ever reads that
// symbol; its value is the [1PHASE OBJ DESTROY] tier of the generators the
// roster destroyed in phase one (0-5 / 6-10 / 11-15), so a raid that wrecked
// more generators gets a longer break between debuff cycles. It is not a cycle
// counter -- an earlier version counted clears into it.
var lukeEnergyControlRecovery = [...]time.Duration{45 * time.Second, 90 * time.Second, 120 * time.Second}

// lukeLightChain and lukeDarkChain are the two route chains of phase two in
// clear order. Each link opens the next.
var (
	lukeLightChain = [...]int{lukeLightEnergyMapID, lukeLightAltarMapID, lukeLightTempleMapID}
	lukeDarkChain  = [...]int{lukeDarkEnergyMapID, lukeDarkAltarMapID, lukeDarkTempleMapID}
)

// runtimeRaidLukeTeamDungeonPhases is which phase owns each Luke team gate.
//
// runtimeRaidDungeonPhaseForMap consults it, and that predicate is what marks a
// SelectDungeon request as a raid entry at all. Without these ids a Luke gate
// looked like an ordinary dungeon to every caller downstream: the ordinary
// level/fatigue policy ran instead of the raid roster check, and the
// town-return origin was resolved from the gate's ordinary worldmap town
// instead of the raid waiting room, so entering 5054 from the Jelva waiting
// area (32/4) was refused against its ordinary town (38/1).
var runtimeRaidLukeTeamDungeonPhases = map[int]byte{
	lukeBirthSanctumMapID:      runtimeRaidDungeonPhaseOne,
	lukeExtinctionSanctumMapID: runtimeRaidDungeonPhaseOne,
	lukeSpreadSanctumMapID:     runtimeRaidDungeonPhaseOne,

	lukeLightEntranceMapID: runtimeRaidDungeonPhaseTwo,
	lukeDarkEntranceMapID:  runtimeRaidDungeonPhaseTwo,
	lukeLightEnergyMapID:   runtimeRaidDungeonPhaseTwo,
	lukeDarkEnergyMapID:    runtimeRaidDungeonPhaseTwo,
	lukeLightAltarMapID:    runtimeRaidDungeonPhaseTwo,
	lukeDarkAltarMapID:     runtimeRaidDungeonPhaseTwo,
	lukeEnergyControlMapID: runtimeRaidDungeonPhaseTwo,
	lukeLightTempleMapID:   runtimeRaidDungeonPhaseTwo,
	lukeDarkTempleMapID:    runtimeRaidDungeonPhaseTwo,
	lukeLightThroneMapID:   runtimeRaidDungeonPhaseTwo,
	lukeDarkThroneMapID:    runtimeRaidDungeonPhaseTwo,
	lukeEnergySaveMapID:    runtimeRaidDungeonPhaseTwo,
}

// Route tags of the phase-two gates, mirroring their `[LOCKOUT TYPE]` first
// value in etc/raid/luke/luke.etc. The exact values are pinned against the
// shipped PVF by raid.TestRealScriptPVFLukePhaseMechanismTables.
const (
	lukeRouteNone  byte = 0
	lukeRouteLight byte = 4
	lukeRouteDark  byte = 5
)

var runtimeRaidLukeGateRoutes = map[int]byte{
	lukeLightEntranceMapID: lukeRouteLight,
	lukeLightEnergyMapID:   lukeRouteLight,
	lukeLightAltarMapID:    lukeRouteLight,
	lukeLightTempleMapID:   lukeRouteLight,
	lukeLightThroneMapID:   lukeRouteLight,

	lukeDarkEntranceMapID: lukeRouteDark,
	lukeDarkEnergyMapID:   lukeRouteDark,
	lukeDarkAltarMapID:    lukeRouteDark,
	lukeDarkTempleMapID:   lukeRouteDark,
	lukeDarkThroneMapID:   lukeRouteDark,

	// The energy control and energy save rooms are shared: both sides run them,
	// and running one must not commit anybody to a route.
	lukeEnergyControlMapID: lukeRouteNone,
	lukeEnergySaveMapID:    lukeRouteNone,
}

func runtimeRaidLukeGateRoute(mapID int) byte {
	return runtimeRaidLukeGateRoutes[mapID]
}

// runtimeRaidLukeRouteEntryAllowedLocked reports whether this member may enter
// this phase-two gate.
//
// Live 2026-09-23: every member could walk into both routes, because nothing
// read `[LOCKOUT TYPE]` at all. The first route gate a member enters binds it to
// that route for the rest of the phase; the shared rooms bind nothing and block
// nobody.
//
// The lock is per CHARACTER, not per squad. Three independent sources say so,
// and the granularity is the whole point of the rule -- a member bound to one
// route is locked out of the other, not merely forbidden to switch squads:
//
//   - the official Korean update notes for the 3.16 revision describe the
//     mechanic as 「입장 시 해당 던전에 귀속되어 다른 던전에 입장할 수 없게
//     됩니다」 and name the subject as 「1파티의 구성원들」 -- the squad's
//     *members*, not the squad and not the raid;
//   - the community wiki entry for the same system states it is applied
//     「파티 단위 아닌, 파티원 개개별로」 (per member, not per squad), and that
//     swapping squad compositions afterwards does not clear it -- a symptom only
//     a per-character record can produce;
//   - the client's own notification packet for this state is named
//     `RAID_MEMBER_LOCKOUT_INFO` (opcode 1262), i.e. keyed on a member.
//
// Recorded in 卢克_官方流程核对_20260929.md §2.4 (LK-2/LK-4). Do not tighten this
// to the squad.
func runtimeRaidLukeRouteEntryAllowedLocked(state *runtimeRaidState, characterID uint16, mapID int) bool {
	if state == nil || state.TailFlag == 1 || characterID == 0 {
		return true
	}
	route := runtimeRaidLukeGateRoute(mapID)
	if route == lukeRouteNone {
		return true
	}
	locked, exists := state.Lifecycle.LukeRouteLocks[characterID]
	return !exists || locked == route
}

// bindRuntimeRaidLukeRouteLocked commits a member to the route of the gate it
// just entered. Caller holds raidMu.
func bindRuntimeRaidLukeRouteLocked(state *runtimeRaidState, characterID uint16, mapID int) (byte, bool) {
	if state == nil || characterID == 0 {
		return lukeRouteNone, false
	}
	route := runtimeRaidLukeGateRoute(mapID)
	if route == lukeRouteNone {
		return lukeRouteNone, false
	}
	state.ensureRuntimeRaidLifecycleMapsLocked()
	if _, exists := state.Lifecycle.LukeRouteLocks[characterID]; exists {
		return route, false
	}
	state.Lifecycle.LukeRouteLocks[characterID] = route
	state.Lifecycle.Revision++
	return route, true
}

// The one-shot markers. etc/raid/luke/luke.etc pulses these inside a [BEHAVIOR]
// and the script acts on the edge -- [1PHASE FAIL] is even set straight back to
// 0 in the same behavior. They are NOT durable phase state, so they live in
// MovieSymbols (delta-pulsed, deliberately absent from the periodic snapshot)
// exactly like Anton's cutscene markers.
//
// Putting them in LukeSymbols instead meant every periodic snapshot re-asserted
// [KILL ALL CHARACTER]=1 for the rest of the phase: entering the throne room
// killed the player again on the next snapshot, and a revive was undone by the
// one after it, which read as "复活币一个都用不了".
func setRuntimeRaidLukePulseLocked(state *runtimeRaidState, key uint32, value uint32) {
	if state == nil {
		return
	}
	state.ensureRuntimeRaidLifecycleMapsLocked()
	state.setRuntimeRaidMovieSymbolLocked(key, value)
}

func runtimeRaidLukeSymbolLocked(state *runtimeRaidState, key uint32) uint32 {
	if state == nil || state.Lifecycle.LukeSymbols == nil {
		return 0
	}
	return state.Lifecycle.LukeSymbols[key]
}

// setRuntimeRaidLukeSymbolLocked writes one symbol and reports whether it moved.
// Caller holds raidMu.
func setRuntimeRaidLukeSymbolLocked(state *runtimeRaidState, key uint32, value uint32) bool {
	if state == nil {
		return false
	}
	state.ensureRuntimeRaidLifecycleMapsLocked()
	if previous, ok := state.Lifecycle.LukeSymbols[key]; ok && previous == value {
		return false
	}
	state.Lifecycle.LukeSymbols[key] = value
	state.Lifecycle.Revision++
	return true
}

func addRuntimeRaidLukeSymbolLocked(state *runtimeRaidState, key uint32, delta uint32) uint32 {
	next := runtimeRaidLukeSymbolLocked(state, key) + delta
	setRuntimeRaidLukeSymbolLocked(state, key, next)
	return next
}

// setRuntimeRaidLukeDungeonStateLocked is the [SET DUNGEON STATE] behavior. A
// gate that has already been spent stays spent: the PVF drives the same gate to
// `hide` right after its clear, and re-opening it later would hand the roster a
// second run at an objective it already finished.
func setRuntimeRaidLukeDungeonStateLocked(state *runtimeRaidState, mapID int, want raid.AntonDungeonState) bool {
	if state == nil {
		return false
	}
	// Do NOT gate this on state.Lifecycle.Phase. activateRuntimeRaidPhase builds
	// the new phase's objectives and runs this phase's init BEFORE it assigns
	// Lifecycle.Phase, so during phase-two activation the objectives already read
	// phase 2 while Lifecycle.Phase is still 1. Comparing the two made every
	// phase-two opening a no-op: the two altar entrances stayed at their PVF
	// `hide` and the whole route was unreachable, while the symbols -- which have
	// no such guard -- went out correctly and made it look like init had run.
	//
	// The comparison was redundant anyway: initializeRuntimeRaidPhaseObjectives
	// rebuilds Objectives from scratch for one phase, so every entry in the map
	// belongs to the phase being activated.
	objective, ok := state.Lifecycle.Objectives[mapID]
	if !ok {
		return false
	}
	if want == raid.AntonDungeonOpen && objective.RemainingHP <= 0 {
		return false
	}
	if objective.State == want {
		return false
	}
	objective.State = want
	state.Lifecycle.Objectives[mapID] = objective
	state.Lifecycle.Revision++
	return true
}

// initRuntimeRaidLukePhaseOneLocked is the [1PHASE INIT] trigger: the three
// sanctum object counters start at zero. Caller holds raidMu.
func initRuntimeRaidLukePhaseOneLocked(state *runtimeRaidState) {
	if state == nil {
		return
	}
	state.ensureRuntimeRaidLifecycleMapsLocked()
	// A fresh phase one draws every sanctum's maze group anew.
	state.Lifecycle.LukeSanctumMazeGroups = nil
	setRuntimeRaidLukeSymbolLocked(state, lukeSymbolPhaseOneInit, 1)
	setRuntimeRaidLukeSymbolLocked(state, lukeSymbolBirthSanctumObject, 0)
	setRuntimeRaidLukeSymbolLocked(state, lukeSymbolExtinctionSanctumObject, 0)
	setRuntimeRaidLukeSymbolLocked(state, lukeSymbolSpreadSanctumObject, 0)
}

// initRuntimeRaidLukePhaseTwoLocked is the [2PHASE INIT] trigger: both altar
// entrances open and the ten-second opening timer is armed on 5054. That timer
// is never checked by any trigger and is only reset when the phase ends, so it
// is a display countdown the client draws; nothing fires when it elapses.
// Caller holds raidMu.
func initRuntimeRaidLukePhaseTwoLocked(state *runtimeRaidState, now time.Time) []runtimeRaidMechanismTimer {
	if state == nil {
		return nil
	}
	state.ensureRuntimeRaidLifecycleMapsLocked()
	// A fresh phase two re-opens both routes to everybody.
	state.Lifecycle.LukeRouteLocks = make(map[uint16]byte)
	setRuntimeRaidLukeSymbolLocked(state, lukeSymbolPhaseTwoInit, 1)
	setRuntimeRaidLukeSymbolLocked(state, lukeSymbolAltarEntranceClear, 0)
	setRuntimeRaidLukeSymbolLocked(state, lukeSymbolAltarClear, 0)
	setRuntimeRaidLukeSymbolLocked(state, lukeSymbolTempleClear, 0)
	// [ENERGY CONTROL OPEN] is the phase-one generator tier, fixed for the
	// whole of phase two (raid_luke_generator.go).
	setRuntimeRaidLukeSymbolLocked(state, lukeSymbolEnergyControlOpen, runtimeRaidLukeEnergyControlTierLocked(state))
	setRuntimeRaidLukeSymbolLocked(state, lukeSymbolEnergyControlEffect, 0)
	setRuntimeRaidLukeSymbolLocked(state, lukeSymbolEnergySaveEffect, 0)
	setRuntimeRaidLukeDungeonStateLocked(state, lukeLightEntranceMapID, raid.AntonDungeonOpen)
	setRuntimeRaidLukeDungeonStateLocked(state, lukeDarkEntranceMapID, raid.AntonDungeonOpen)
	return []runtimeRaidMechanismTimer{
		state.armRuntimeRaidMechanismLocked(runtimeRaidMechanismActive, lukeBirthSanctumMapID, now, lukePhaseTwoOpeningWindow, 0),
	}
}

// runtimeRaidLukeEnterDungeonLocked is the [ON ENTER DUNGEON] side of the graph,
// driven by a squad binding itself into a gate: [ON ENTER DUNGEON] 5054 with
// [2PHASE DUNGEON TYPE] still 0 opens the other two sanctums. Entering a throne
// gate does nothing by itself -- the energy save room opens on the throne's
// boss room (runtimeRaidLukeEnterMapLocked). Caller holds raidMu.
func runtimeRaidLukeEnterDungeonLocked(state *runtimeRaidState, mapID int) bool {
	if state == nil || state.Lifecycle.Phase != runtimeRaidDungeonPhaseOne || mapID != lukeBirthSanctumMapID {
		return false
	}
	if runtimeRaidLukeSymbolLocked(state, lukeSymbolPhaseTwoDungeonType) != 0 {
		return false
	}
	opened := setRuntimeRaidLukeDungeonStateLocked(state, lukeExtinctionSanctumMapID, raid.AntonDungeonOpen)
	return setRuntimeRaidLukeDungeonStateLocked(state, lukeSpreadSanctumMapID, raid.AntonDungeonOpen) || opened
}

// runtimeRaidLukeIsThroneBossRoom reports the two maps luke.etc opens the energy
// save room on: [ON ENTER MAP] 55845 / 55846 with [CHECK DUNGEON STATE] 5066
// `hide`. They are the second (boss) room of Luke_Light_1.dgn / Luke_Dark_1.dgn;
// each throne has only its start room (55849 / 55850) in front of it.
func runtimeRaidLukeIsThroneBossRoom(roomMapID int64) bool {
	return roomMapID == lukeLightThroneBossRoomMapID || roomMapID == lukeDarkThroneBossRoomMapID
}

// runtimeRaidLukeEnterMapLocked is the [ON ENTER MAP] side: the first squad to
// reach a throne's boss room opens the energy save room, and that open edge
// ([ON CHANGE DUNGEON STATE] 5066 `open`) sets [ENERGY SAVE EFFECT] 1 and arms
// the 150-second wipe clock. It reports whether the room opened. Later arrivals,
// and a room that is closed for its recovery or already open, change nothing.
// Caller holds raidMu.
func runtimeRaidLukeEnterMapLocked(
	state *runtimeRaidState,
	dungeonMapID int,
	roomMapID int64,
	now time.Time,
) (bool, []runtimeRaidMechanismTimer) {
	if state == nil || state.TailFlag == 1 || !state.Started ||
		runtimeRaidTeamGraphOwner(*state) != runtimeRaidOwnerLuke ||
		state.Lifecycle.Status != runtimeRaidLifecyclePhaseActive ||
		state.Lifecycle.Phase != runtimeRaidDungeonPhaseTwo ||
		!runtimeRaidLukeIsThroneMapID(dungeonMapID) || !runtimeRaidLukeIsThroneBossRoom(roomMapID) {
		return false, nil
	}
	objective, ok := state.Lifecycle.Objectives[lukeEnergySaveMapID]
	if !ok || objective.State != raid.AntonDungeonHide {
		return false, nil
	}
	if !setRuntimeRaidLukeDungeonStateLocked(state, lukeEnergySaveMapID, raid.AntonDungeonOpen) {
		return false, nil
	}
	return true, armRuntimeRaidLukeEnergySaveWindowLocked(state, now)
}

// runtimeRaidLukeGiveUpDungeonLocked is the [ON GIVEUP DUNGEON] side.
// Abandoning a sanctum resets that sanctum's object counter; abandoning a
// throne closes the energy save room again. Caller holds raidMu.
func runtimeRaidLukeGiveUpDungeonLocked(state *runtimeRaidState, mapID int) bool {
	if state == nil {
		return false
	}
	switch mapID {
	case lukeBirthSanctumMapID:
		return setRuntimeRaidLukeSymbolLocked(state, lukeSymbolBirthSanctumObject, 0)
	case lukeExtinctionSanctumMapID:
		return setRuntimeRaidLukeSymbolLocked(state, lukeSymbolExtinctionSanctumObject, 0)
	case lukeSpreadSanctumMapID:
		return setRuntimeRaidLukeSymbolLocked(state, lukeSymbolSpreadSanctumObject, 0)
	case lukeLightThroneMapID, lukeDarkThroneMapID:
		// [KICK OUT CHARACTER] belongs to the abandoning member's own exit, which
		// the ordinary abandon path already performs; only the room state is this
		// graph's business.
		//
		// [ON CHANGE DUNGEON STATE] 5066 `hide` is [RESET TIMER] 1 5066 and
		// [RESET TIMER] 2 5066, so hiding the room has to take both of its timers
		// down with it. Leaving the wipe clock armed made it fire on a throne
		// nobody was holding any more.
		state.cancelRuntimeRaidMechanismLocked(runtimeRaidMechanismActive, lukeEnergySaveMapID)
		state.cancelRuntimeRaidMechanismLocked(runtimeRaidMechanismRecovery, lukeEnergySaveMapID)
		changed := setRuntimeRaidLukeDungeonStateLocked(state, lukeEnergySaveMapID, raid.AntonDungeonHide)
		return setRuntimeRaidLukeSymbolLocked(state, lukeSymbolEnergySaveEffect, 0) || changed
	default:
		return false
	}
}

// applyRuntimeRaidLukePhaseOneClearLocked reacts to one committed phase-one
// clear. Caller holds raidMu.
func applyRuntimeRaidLukePhaseOneClearLocked(state *runtimeRaidState, mapID int) {
	if state == nil {
		return
	}
	objective, ok := state.Lifecycle.Objectives[mapID]
	if !ok || objective.RemainingHP != 0 {
		return
	}
	// The first branch sanctum to fall decides the phase-two route marker.
	if runtimeRaidLukeSymbolLocked(state, lukeSymbolPhaseTwoDungeonType) == 0 {
		switch mapID {
		case lukeExtinctionSanctumMapID:
			setRuntimeRaidLukeSymbolLocked(state, lukeSymbolPhaseTwoDungeonType, lukePhaseTwoDungeonTypeExtinction)
		case lukeSpreadSanctumMapID:
			setRuntimeRaidLukeSymbolLocked(state, lukeSymbolPhaseTwoDungeonType, lukePhaseTwoDungeonTypeSpread)
		}
	}
	if !runtimeRaidLukeSanctumsClearedLocked(state) {
		return
	}
	setRuntimeRaidLukeSymbolLocked(state, lukeSymbolPhaseOneSuccess, 1)
	for _, sanctum := range []int{lukeBirthSanctumMapID, lukeExtinctionSanctumMapID, lukeSpreadSanctumMapID} {
		setRuntimeRaidLukeDungeonStateLocked(state, sanctum, raid.AntonDungeonHide)
	}
}

func runtimeRaidLukeSanctumsClearedLocked(state *runtimeRaidState) bool {
	for _, sanctum := range []int{lukeBirthSanctumMapID, lukeExtinctionSanctumMapID, lukeSpreadSanctumMapID} {
		objective, ok := state.Lifecycle.Objectives[sanctum]
		if !ok || objective.Phase != runtimeRaidDungeonPhaseOne || objective.RemainingHP != 0 {
			return false
		}
	}
	return true
}

// applyRuntimeRaidLukePhaseTwoClearLocked reacts to one committed phase-two
// clear and returns the timers it armed. Caller holds raidMu.
func applyRuntimeRaidLukePhaseTwoClearLocked(state *runtimeRaidState, mapID int, now time.Time) []runtimeRaidMechanismTimer {
	if state == nil {
		return nil
	}
	objective, ok := state.Lifecycle.Objectives[mapID]
	if !ok {
		return nil
	}
	// The energy control room is a cycle, not a one-shot gate: it is cleared
	// over and over and its own clear count never runs out.
	if mapID == lukeEnergyControlMapID {
		return runtimeRaidLukeCloseEnergyControlLocked(state, now)
	}
	if mapID == lukeEnergySaveMapID {
		return runtimeRaidLukeCloseEnergySaveLocked(state, now)
	}
	if objective.RemainingHP != 0 {
		return nil
	}
	switch mapID {
	case lukeLightEntranceMapID, lukeDarkEntranceMapID:
		setRuntimeRaidLukeDungeonStateLocked(state, mapID, raid.AntonDungeonHide)
		if addRuntimeRaidLukeSymbolLocked(state, lukeSymbolAltarEntranceClear, 1) < lukeAltarEntranceRequired {
			return nil
		}
		setRuntimeRaidLukeDungeonStateLocked(state, lukeLightEnergyMapID, raid.AntonDungeonOpen)
		setRuntimeRaidLukeDungeonStateLocked(state, lukeDarkEnergyMapID, raid.AntonDungeonOpen)
		setRuntimeRaidLukeDungeonStateLocked(state, lukeEnergyControlMapID, raid.AntonDungeonOpen)
		setRuntimeRaidLukeSymbolLocked(state, lukeSymbolEnergyControlEffect, 1)
		return nil
	case lukeLightEnergyMapID:
		setRuntimeRaidLukeDungeonStateLocked(state, mapID, raid.AntonDungeonHide)
		setRuntimeRaidLukeDungeonStateLocked(state, lukeLightAltarMapID, raid.AntonDungeonOpen)
		return nil
	case lukeDarkEnergyMapID:
		setRuntimeRaidLukeDungeonStateLocked(state, mapID, raid.AntonDungeonHide)
		setRuntimeRaidLukeDungeonStateLocked(state, lukeDarkAltarMapID, raid.AntonDungeonOpen)
		return nil
	case lukeLightAltarMapID, lukeDarkAltarMapID:
		setRuntimeRaidLukeDungeonStateLocked(state, mapID, raid.AntonDungeonHide)
		if addRuntimeRaidLukeSymbolLocked(state, lukeSymbolAltarClear, 1) < lukeAltarRequired {
			return nil
		}
		// Both altars down: the temples open and the energy control room shuts
		// for good, so its debuff and its opening timer both stop.
		setRuntimeRaidLukeDungeonStateLocked(state, lukeLightTempleMapID, raid.AntonDungeonOpen)
		setRuntimeRaidLukeDungeonStateLocked(state, lukeDarkTempleMapID, raid.AntonDungeonOpen)
		setRuntimeRaidLukeDungeonStateLocked(state, lukeEnergyControlMapID, raid.AntonDungeonHide)
		setRuntimeRaidLukeSymbolLocked(state, lukeSymbolEnergyControlEffect, 0)
		state.cancelRuntimeRaidMechanismLocked(runtimeRaidMechanismActive, lukeEnergyControlMapID)
		state.cancelRuntimeRaidMechanismLocked(runtimeRaidMechanismRecovery, lukeEnergyControlMapID)
		return nil
	case lukeLightTempleMapID, lukeDarkTempleMapID:
		setRuntimeRaidLukeDungeonStateLocked(state, mapID, raid.AntonDungeonHide)
		if addRuntimeRaidLukeSymbolLocked(state, lukeSymbolTempleClear, 1) < lukeTempleRequired {
			return nil
		}
		setRuntimeRaidLukeDungeonStateLocked(state, lukeLightThroneMapID, raid.AntonDungeonOpen)
		return nil
	case lukeLightThroneMapID:
		// 机械王座:明 falls, 机械王座:暗 opens. The roster is wiped between the two
		// stages ([KILL ALL CHARACTER]), which is the native forced regroup.
		setRuntimeRaidLukeDungeonStateLocked(state, lukeLightThroneMapID, raid.AntonDungeonHide)
		runtimeRaidLukeCloseEnergySaveRoomLocked(state)
		setRuntimeRaidLukeDungeonStateLocked(state, lukeDarkThroneMapID, raid.AntonDungeonOpen)
		setRuntimeRaidLukePulseLocked(state, lukeSymbolKillAllCharacter, 1)
		return nil
	case lukeDarkThroneMapID:
		// 机械王座:暗 falls and the phase is over; runtimeRaidPhaseObjectivesComplete
		// reads the same fact.
		setRuntimeRaidLukeDungeonStateLocked(state, lukeDarkThroneMapID, raid.AntonDungeonHide)
		runtimeRaidLukeCloseEnergySaveRoomLocked(state)
		setRuntimeRaidLukePulseLocked(state, lukeSymbolKillAllCharacter, 1)
		state.cancelRuntimeRaidMechanismLocked(runtimeRaidMechanismActive, lukeBirthSanctumMapID)
		return nil
	default:
		return nil
	}
}

// runtimeRaidLukeCloseEnergyControlLocked shuts the energy control room for its
// recovery window. luke.etc picks the window from [ENERGY CONTROL OPEN] (133)
// -- `5063 clear` with 133 = 0 / 1 / 2 arms 45 / 90 / 120 s -- and never writes
// 133 itself: it is the phase-one generator tier set once at phase-two init,
// so every cycle of one raid waits the same time.
func runtimeRaidLukeCloseEnergyControlLocked(state *runtimeRaidState, now time.Time) []runtimeRaidMechanismTimer {
	tier := runtimeRaidLukeSymbolLocked(state, lukeSymbolEnergyControlOpen)
	if tier >= uint32(len(lukeEnergyControlRecovery)) {
		tier = uint32(len(lukeEnergyControlRecovery)) - 1
	}
	recovery := lukeEnergyControlRecovery[tier]
	// The room is a repeatable objective: restore its clear budget so the next
	// cycle can be run again.
	if objective, ok := state.Lifecycle.Objectives[lukeEnergyControlMapID]; ok {
		objective.RemainingHP = objective.MaxHP
		objective.State = raid.AntonDungeonClosed
		state.Lifecycle.Objectives[lukeEnergyControlMapID] = objective
		state.Lifecycle.Revision++
	}
	setRuntimeRaidLukeSymbolLocked(state, lukeSymbolEnergyControlEffect, 0)
	return []runtimeRaidMechanismTimer{
		state.armRuntimeRaidMechanismLocked(runtimeRaidMechanismRecovery, lukeEnergyControlMapID, now, recovery, 0),
	}
}

// runtimeRaidLukeCloseEnergySaveLocked handles a cleared energy save room: the
// wipe clock is disarmed, the debuff drops and the room reopens after two
// minutes.
func runtimeRaidLukeCloseEnergySaveLocked(state *runtimeRaidState, now time.Time) []runtimeRaidMechanismTimer {
	state.cancelRuntimeRaidMechanismLocked(runtimeRaidMechanismActive, lukeEnergySaveMapID)
	if objective, ok := state.Lifecycle.Objectives[lukeEnergySaveMapID]; ok {
		objective.RemainingHP = objective.MaxHP
		objective.State = raid.AntonDungeonClosed
		state.Lifecycle.Objectives[lukeEnergySaveMapID] = objective
		state.Lifecycle.Revision++
	}
	setRuntimeRaidLukeSymbolLocked(state, lukeSymbolEnergySaveEffect, 0)
	return []runtimeRaidMechanismTimer{
		state.armRuntimeRaidMechanismLocked(runtimeRaidMechanismRecovery, lukeEnergySaveMapID, now, lukeEnergySaveRecovery, 0),
	}
}

// runtimeRaidLukeCloseEnergySaveRoomLocked hides the room and drops both of its
// timers, which is what the PVF does whenever a throne stage ends.
func runtimeRaidLukeCloseEnergySaveRoomLocked(state *runtimeRaidState) {
	setRuntimeRaidLukeDungeonStateLocked(state, lukeEnergySaveMapID, raid.AntonDungeonHide)
	setRuntimeRaidLukeSymbolLocked(state, lukeSymbolEnergySaveEffect, 0)
	state.cancelRuntimeRaidMechanismLocked(runtimeRaidMechanismActive, lukeEnergySaveMapID)
	state.cancelRuntimeRaidMechanismLocked(runtimeRaidMechanismRecovery, lukeEnergySaveMapID)
}

// armRuntimeRaidLukeEnergySaveWindowLocked starts the 150-second wipe clock the
// moment the room opens.
//
// The PVF arms it from one edge only: [ON CHANGE DUNGEON STATE] 5066 with
// [CHECK DUNGEON STATE] 5066 `open`. That edge cannot repeat while the room is
// already open, so an armed clock is never restarted -- squads walking in and
// out of 能源储藏室 must not buy the throne squad another 150 seconds. The guard
// enforces that here instead of trusting every caller to have proven the
// transition. Caller holds raidMu.
func armRuntimeRaidLukeEnergySaveWindowLocked(state *runtimeRaidState, now time.Time) []runtimeRaidMechanismTimer {
	key := runtimeRaidMechanismKey{Kind: runtimeRaidMechanismActive, MapID: lukeEnergySaveMapID}
	if _, armed := state.Lifecycle.Mechanisms[key]; armed {
		return nil
	}
	setRuntimeRaidLukeSymbolLocked(state, lukeSymbolEnergySaveEffect, lukeEnergySaveEffectActive)
	return []runtimeRaidMechanismTimer{
		state.armRuntimeRaidMechanismLocked(runtimeRaidMechanismActive, lukeEnergySaveMapID, now, lukeEnergySaveWindow, 0),
	}
}

// observeRuntimeRaidLukeEnterMapLocked feeds a committed room transition to
// runtimeRaidLukeEnterMapLocked. It is called for every room move, so it bails
// out before touching raidMu unless the run is a raid gate. Caller holds
// session.dungeon.mu (dungeon.mu -> raidMu is the allowed order); the snapshot is
// published from the gameplay queue because it locks every gate owner's dungeon
// mutex, this session's included.
func (s *Service) observeRuntimeRaidLukeEnterMapLocked(session *gameSession, runtime *runtimeDungeonState, roomMapID int64) {
	if s == nil || session == nil || runtime == nil || !runtimeRaidLukeIsThroneBossRoom(roomMapID) {
		return
	}
	binding := runtime.raidDungeon
	if binding.RaidKey == 0 || binding.LifecycleToken == 0 || binding.LifecycleToken != runtime.lifecycleToken ||
		!runtimeRaidLukeIsThroneMapID(binding.MapID) {
		return
	}
	now := s.gameplayNow()
	s.raidMu.Lock()
	state := s.runtimeRaidByMemberLocked(session.selectedCharacterID)
	var (
		opened bool
		armed  []runtimeRaidMechanismTimer
		phase  byte
	)
	if state != nil && state.RaidKey == binding.RaidKey {
		opened, armed = runtimeRaidLukeEnterMapLocked(state, binding.MapID, roomMapID, now)
		phase = state.Lifecycle.Phase
	}
	s.raidMu.Unlock()
	if !opened {
		return
	}
	s.logGameEvent(session, "game-raid-luke-energy-save-opened",
		"char_id", session.selectedCharacterID,
		"raid_key", binding.RaidKey,
		"throne", binding.MapID,
		"room_map_id", roomMapID,
		"wipe_clock_armed", len(armed) != 0)
	if err := s.scheduleRuntimeRaidMechanismTimers(binding.RaidKey, armed); err != nil {
		s.logPacketEvent("game-raid-luke-energy-save-timer-failed",
			"raid_key", binding.RaidKey, "map_id", binding.MapID, "error", err)
	}
	s.scheduleRuntimeRaidLukeProgressPublish(binding.RaidKey, phase, fmt.Sprintf("energy-save-open:%d", binding.MapID))
}

func runtimeRaidLukeIsThroneMapID(mapID int) bool {
	return mapID == lukeLightThroneMapID || mapID == lukeDarkThroneMapID
}

// scheduleRuntimeRaidLukeEntryProgressPublish publishes the phase snapshot for an
// entry edge once the caller's locks are gone.
//
// It must not run inline: its caller holds the entering session's dungeon mutex,
// and publishRuntimeRaidObjectiveProgress locks the dungeon mutex of every gate
// owner -- including that same session on an entry edge. Handing it to the
// gameplay queue moves it onto that queue's goroutine, which owns none of those
// locks. A zero delay keeps it on the next tick rather than a timer.
func (s *Service) scheduleRuntimeRaidLukeEntryProgressPublish(
	event runtimeRaidDungeonLifecycleEvent,
	kind runtimeRaidDungeonLifecycleKind,
) {
	if s == nil {
		return
	}
	queue := s.ensureGameplayTimeQueue()
	if queue == nil {
		return
	}
	name := fmt.Sprintf("raid-luke-entry-progress:%d:%d:%d:%s",
		event.RaidKey, event.CharacterID, event.MapID, kind.String())
	if err := queue.ScheduleAfter(name, 0, func(time.Time) {
		if err := s.publishRuntimeRaidObjectiveProgress(event.RaidKey, event.Phase); err != nil {
			s.logPacketEvent("game-raid-luke-entry-progress-publish-failed",
				"raid_key", event.RaidKey, "raid_map_id", event.MapID,
				"event", kind.String(), "error", err)
		}
	}); err != nil {
		s.logPacketEvent("game-raid-luke-entry-progress-schedule-failed",
			"raid_key", event.RaidKey, "raid_map_id", event.MapID,
			"event", kind.String(), "error", err)
	}
}

// fireRuntimeRaidLukeMechanismLocked runs one elapsed Luke timer. Caller holds
// raidMu and has already removed the timer from the table.
func fireRuntimeRaidLukeMechanismLocked(state *runtimeRaidState, armed runtimeRaidMechanismTimer, due time.Time) ([]runtimeRaidMechanismTimer, string) {
	if state == nil {
		return nil, ""
	}
	switch {
	case armed.Kind == runtimeRaidMechanismActive && armed.MapID == lukeBirthSanctumMapID:
		// The phase-two opening countdown is display only; nothing fires.
		return nil, ""
	case armed.Kind == runtimeRaidMechanismRecovery && armed.MapID == lukeEnergyControlMapID:
		objective, ok := state.Lifecycle.Objectives[lukeEnergyControlMapID]
		if !ok || objective.State != raid.AntonDungeonClosed {
			return nil, ""
		}
		setRuntimeRaidLukeDungeonStateLocked(state, lukeEnergyControlMapID, raid.AntonDungeonOpen)
		setRuntimeRaidLukeSymbolLocked(state, lukeSymbolEnergyControlEffect, 1)
		return nil, "energy_control_reopened"
	case armed.Kind == runtimeRaidMechanismActive && armed.MapID == lukeEnergySaveMapID:
		// The clock ran out: the roster is wiped and the room closes.
		setRuntimeRaidLukePulseLocked(state, lukeSymbolKillAllCharacter, 1)
		setRuntimeRaidLukeSymbolLocked(state, lukeSymbolEnergySaveEffect, lukeEnergySaveEffectFailed)
		setRuntimeRaidLukeDungeonStateLocked(state, lukeEnergySaveMapID, raid.AntonDungeonHide)
		state.cancelRuntimeRaidMechanismLocked(runtimeRaidMechanismRecovery, lukeEnergySaveMapID)
		return nil, "energy_save_expired"
	case armed.Kind == runtimeRaidMechanismRecovery && armed.MapID == lukeEnergySaveMapID:
		if !setRuntimeRaidLukeDungeonStateLocked(state, lukeEnergySaveMapID, raid.AntonDungeonOpen) {
			return nil, ""
		}
		return armRuntimeRaidLukeEnergySaveWindowLocked(state, due), "energy_save_reopened"
	default:
		return nil, ""
	}
}

// runtimeRaidLukePhaseObjectivesComplete is Luke's [CLEAR PHASE] condition:
// phase one ends when all three sanctums read clear, phase two when 机械王座:暗
// falls.
func runtimeRaidLukePhaseObjectivesComplete(state runtimeRaidState, phase byte) bool {
	remaining := func(mapID int) int {
		objective, ok := state.Lifecycle.Objectives[mapID]
		if !ok || objective.Phase != phase {
			return -1
		}
		return objective.RemainingHP
	}
	switch phase {
	case runtimeRaidDungeonPhaseOne:
		return remaining(lukeBirthSanctumMapID) == 0 &&
			remaining(lukeExtinctionSanctumMapID) == 0 &&
			remaining(lukeSpreadSanctumMapID) == 0
	case runtimeRaidDungeonPhaseTwo:
		return remaining(lukeDarkThroneMapID) == 0
	default:
		return false
	}
}

// runtimeRaidLukeSymbolRows are the Luke graph's symbol rows for the periodic
// phase snapshot. They are durable raid state, not one-shot cutscene pulses, so
// a reconnecting member has to receive them again.
func runtimeRaidLukeSymbolRows(state runtimeRaidState) []raid.RaidSymbolRow {
	if state.TailFlag == 1 || runtimeRaidTeamGraphOwner(state) != runtimeRaidOwnerLuke ||
		len(state.Lifecycle.LukeSymbols) == 0 {
		return nil
	}
	keys := make([]uint32, 0, len(state.Lifecycle.LukeSymbols))
	for key := range state.Lifecycle.LukeSymbols {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	rows := make([]raid.RaidSymbolRow, 0, len(keys))
	for _, key := range keys {
		rows = append(rows, raid.RaidSymbolRow{Key: key, Value: state.Lifecycle.LukeSymbols[key]})
	}
	return rows
}
