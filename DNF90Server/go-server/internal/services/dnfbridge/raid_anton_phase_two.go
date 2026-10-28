package dnfbridge

import (
	"math/rand"
	"sort"
	"time"

	"longheng.io/server/internal/modules/dnf/dungeoncmd"
	"longheng.io/server/internal/modules/dnf/raid"
)

// Anton phase two is a siege on the black volcano's shield. Energy
// interception (218) and the hatcheries (221-224) report shield damage from
// inside their runs through the native op662 symbol request; the shield
// (symbol 110) starts full and the volcano (219) admits nobody until it reads
// zero. A broken shield recharges every second, faster while a hatchery is
// infected, and a fully recharged shield closes the volcano again. Clearing the
// volcano closes everything but the heart (220), which needs five clears.
//
// Required clears and initial gate states come from etc/raid/anton.etc. The
// shield size, transmission values, charge rates and hatchery timings follow
// the read-only 86JP C# server (RaidHandler.AntonDefinitions.cs,
// RaidHandler.cs TryAuthorizeAntonBarrierRequest, Progression.cs
// ChangeBlackVolcanoBarrierAsync / hatchery timers, AntonRaidRewardProvider.cs
// fallbacks) and are a hypothesis until the current EXE confirms them.
const (
	antonEnergyInterceptionMapID = 218
	antonBlackVolcanoMapID       = 219
	antonHeartMapID              = 220
	antonFirstHatcheryMapID      = 221
	antonLastHatcheryMapID       = 224

	antonBarrierMaximum                 uint32 = 10000
	antonEnergyLargeTransmission        uint32 = 3000
	antonEnergySmallTransmission        uint32 = 100
	antonEnergySmallTransmissionCount   uint32 = 5
	antonHatcheryReduction              uint32 = 1000
	antonInfectedHatcheryExtraReduction uint32 = 500
	antonBarrierChargePerSecond         uint32 = 17
	antonBarrierInfectedChargePerSecond uint32 = 34
	antonHatcheryOpenCount                     = 3
	antonHatcheryOpenDelay                     = 180 * time.Second
	antonHatcheryRecovery                      = 240 * time.Second
	antonBarrierRecoveryTick                   = time.Second

	// Wire symbol ids read by the native Anton phase-two script.
	antonSymbolBarrier        uint32 = 110
	antonSymbolVolcanoOpen    uint32 = 111
	antonSymbolPhaseTwoInit   uint32 = 106
	antonSymbolInfectionFlag  uint32 = 7
	antonSymbolInfectionMapID uint32 = 8

	// One-shot cutscene markers (etc/raid/anton.etc.symbol). The client owns the
	// video files; the server only pulses the marker on the 0x0247 channel.
	antonSymbolMovieHatcheryOpen uint32 = 128
	antonSymbolBarrierBreak      uint32 = 126
	antonSymbolBarrierRecover    uint32 = 127

	// antonSymbolPhaseOneFail is 86JP's AntonPhaseOneFailSymbolId = 104
	// (RaidHandler.AntonDefinitions.cs:55): the attack window of the first phase
	// ran out, which plays the failure presentation. It is the one symbol the
	// timeout sends and it has no phase-two counterpart.
	antonSymbolPhaseOneFail uint32 = 104

	// Hatchery presentation symbols (etc/raid/anton.etc.symbol): 3..6 are
	// [221..224 HATCHERY FAIL] and 9..12 are [221..224 HATCHERY NAMED CREATE],
	// one per entry of the client's [HATCHERY INFO] list (4 hatcheries,
	// 221 222 223 224). The client's own etc/raid/anton.etc script drives them;
	// the server only supplies the first window of the two timers that script
	// waits on.
	antonHatcheryFailSymbolBase  uint32 = 3
	antonHatcheryNamedSymbolBase uint32 = 9

	// The two windows 86JP's StartHatcheryEffectTimersAsync arms per hatchery:
	// timer type 1 with 120s then 45s, timer type 3 with 120s then 50s.
	antonHatcheryEffectWindow = 120 * time.Second
	antonHatcheryFailRepeat   = 45 * time.Second
	antonHatcheryNamedRepeat  = 50 * time.Second

	// op662 operation codes: 0 set, 1 add, 2 subtract.
	runtimeRaidSymbolOperationSet      byte = 0
	runtimeRaidSymbolOperationAdd      byte = 1
	runtimeRaidSymbolOperationSubtract byte = 2

	runtimeRaidMechanismHatcheryOpen    byte = 3
	runtimeRaidMechanismBarrierRecovery byte = 4
)

type runtimeRaidBarrierRequestKey struct {
	MapID          int
	LifecycleToken uint64
	Operand        uint32
}

func isAntonHatcheryMapID(mapID int) bool {
	return mapID >= antonFirstHatcheryMapID && mapID <= antonLastHatcheryMapID
}

// antonHatcherySymbolPlan maps a hatchery dungeon id onto the two presentation
// symbols the client's own etc/raid/anton.etc drives for it.
func antonHatcherySymbolPlan(mapID int) (fail uint32, named uint32, ok bool) {
	if !isAntonHatcheryMapID(mapID) {
		return 0, 0, false
	}
	index := uint32(mapID - antonFirstHatcheryMapID)
	return antonHatcheryFailSymbolBase + index, antonHatcheryNamedSymbolBase + index, true
}

// armRuntimeRaidHatcheryEffectTimersLocked arms the two client-side hatchery
// effect windows for one hatchery: wire timer types 1 and 3, each 120 seconds,
// keyed by the hatchery dungeon id.
//
// The client never starts these itself. etc/raid/anton.etc gates
//
//	[CHECK DUNGEON STATE] 221 `open` + [CHECK TIMER END] 1 221
//	  -> [SET] [221 HATCHERY FAIL] 1 + [SET TIMER] 1 221 45
//	[CHECK DUNGEON STATE] 221 `open` + [CHECK TIMER END] 3 221
//	  -> [SET] [221 HATCHERY NAMED CREATE] 1 + [SET TIMER] 3 221 50
//
// so the script only self-perpetuates once the first window has been armed, and
// that first window is the server's. Live traffic shows the server never armed
// it: across packet_log.txt every 0x0254 send carries type 0/1/2/3/4/5, but type
// 1 only ever names 212/214/216 (phase one) plus raid keys, and type 3 only ever
// names 219 (the volcano open timer). Types 1 and 3 were never sent for
// 221..224, so [SET] [221..224 HATCHERY NAMED CREATE] never ran on any client.
// 86JP arms the same pair in StartHatcheryEffectTimersAsync
// (RaidHandler.Progression.cs:588-600) and clears it in ClearHatcheryAsync
// (:393-397).
//
// The wire types reuse runtimeRaidMechanismActive (1) and
// runtimeRaidMechanismHatcheryOpen (3) because the wire type IS the mechanism
// kind; the hatchery map id keeps them apart from the phase-one windows and from
// the volcano open timer on 219. Caller holds raidMu.
func (state *runtimeRaidState) armRuntimeRaidHatcheryEffectTimersLocked(mapID int, now time.Time) []runtimeRaidMechanismTimer {
	if state == nil {
		return nil
	}
	if _, _, ok := antonHatcherySymbolPlan(mapID); !ok {
		return nil
	}
	return []runtimeRaidMechanismTimer{
		state.armRuntimeRaidMechanismLocked(runtimeRaidMechanismActive, mapID, now, antonHatcheryEffectWindow, 0),
		state.armRuntimeRaidMechanismLocked(runtimeRaidMechanismHatcheryOpen, mapID, now, antonHatcheryEffectWindow, 0),
	}
}

// cancelRuntimeRaidHatcheryEffectTimersLocked drops both effect windows of one
// hatchery: a closed hatchery must not keep pulsing [HATCHERY FAIL] or
// [HATCHERY NAMED CREATE] on either side. Caller holds raidMu.
func (state *runtimeRaidState) cancelRuntimeRaidHatcheryEffectTimersLocked(mapID int) {
	if state == nil {
		return
	}
	state.cancelRuntimeRaidMechanismLocked(runtimeRaidMechanismActive, mapID)
	state.cancelRuntimeRaidMechanismLocked(runtimeRaidMechanismHatcheryOpen, mapID)
}

// fireRuntimeRaidHatcheryEffectTimerLocked pulses one hatchery presentation
// symbol and re-arms its window with 86JP's repeat cadence. The pulse rides
// Lifecycle.MovieSymbols because that map is the one the mechanism callback
// diffs and broadcasts, so this edge goes out exactly once. The id keeps its
// current value in the phase snapshot too (runtimeRaidPhaseTwoSymbolRows seeds
// the pair so the client has the object at all), but that snapshot is only sent
// at boundaries -- phase activation, rejoin and a 战况表 reopen -- never on a
// timer, so it cannot race the client's own [SET] ... 1 every few seconds the
// way a periodic replay would. Caller holds raidMu.
func (state *runtimeRaidState) fireRuntimeRaidHatcheryEffectTimerLocked(mapID int, kind byte, now time.Time) ([]runtimeRaidMechanismTimer, string) {
	fail, named, ok := antonHatcherySymbolPlan(mapID)
	if !ok {
		return nil, ""
	}
	objective, exists := state.Lifecycle.Objectives[mapID]
	if !exists || objective.Phase != runtimeRaidDungeonPhaseTwo || objective.State != raid.AntonDungeonOpen {
		// etc/raid/anton.etc guards the same window with [CHECK DUNGEON STATE]
		// `<open>`; a closed hatchery has no presentation to run.
		return nil, ""
	}
	symbol, window, reason := named, antonHatcheryNamedRepeat, "hatchery_named_created"
	if kind == runtimeRaidMechanismActive {
		symbol, window, reason = fail, antonHatcheryFailRepeat, "hatchery_failed"
	}
	state.setRuntimeRaidMovieSymbolLocked(symbol, 1)
	return []runtimeRaidMechanismTimer{
		state.armRuntimeRaidMechanismLocked(kind, mapID, now, window, 0),
	}, reason
}

// runtimeRaidAntonEntryAllowedByBarrier is the phase-two dungeon-entry
// authority: the volcano admits nobody while the shield holds. Every other gate
// is decided by its objective state alone.
func runtimeRaidAntonEntryAllowedByBarrier(state runtimeRaidState, mapID int) bool {
	if state.TailFlag == 1 || mapID != antonBlackVolcanoMapID {
		return true
	}
	return state.Lifecycle.VolcanoOpen
}

// initRuntimeRaidPhaseTwoLocked resets the siege state when phase two is
// activated and arms the hatchery opening. Caller holds raidMu.
func initRuntimeRaidPhaseTwoLocked(state *runtimeRaidState, now time.Time) {
	state.ensureRuntimeRaidLifecycleMapsLocked()
	state.Lifecycle.Barrier = antonBarrierMaximum
	state.Lifecycle.VolcanoOpen = false
	state.Lifecycle.InfectionMapID = 0
	state.Lifecycle.BarrierRequests = make(map[runtimeRaidBarrierRequestKey]uint32)
	state.armRuntimeRaidMechanismLocked(runtimeRaidMechanismHatcheryOpen, antonBlackVolcanoMapID, now, antonHatcheryOpenDelay, 0)
}

// antonBarrierRequestLimit returns how many times one run may report a given
// shield reduction. Zero means the request is not a valid shield report.
func antonBarrierRequestLimit(mapID int, infectionMapID int, symbolID uint32, operand uint32, operation byte) uint32 {
	if symbolID != antonSymbolBarrier || operation != runtimeRaidSymbolOperationSubtract {
		return 0
	}
	if mapID == antonEnergyInterceptionMapID {
		switch operand {
		case antonEnergyLargeTransmission:
			return 1
		case antonEnergySmallTransmission:
			return antonEnergySmallTransmissionCount
		default:
			return 0
		}
	}
	if !isAntonHatcheryMapID(mapID) {
		return 0
	}
	if operand == antonHatcheryReduction {
		return 1
	}
	if mapID == infectionMapID && operand == antonInfectedHatcheryExtraReduction {
		return 1
	}
	return 0
}

func applyRuntimeRaidSymbolOperation(current uint32, operand uint32, operation byte) (uint32, bool) {
	switch operation {
	case runtimeRaidSymbolOperationSet:
		return operand, true
	case runtimeRaidSymbolOperationAdd:
		if operand > ^uint32(0)-current {
			return ^uint32(0), true
		}
		return current + operand, true
	case runtimeRaidSymbolOperationSubtract:
		if operand >= current {
			return 0, true
		}
		return current - operand, true
	default:
		return current, false
	}
}

// runtimeRaidBarrierChange is the outcome of one shield mutation.
type runtimeRaidBarrierChange struct {
	Previous uint32
	Next     uint32
	Broke    bool
	Restored bool
}

// runtimeRaidHatcheryTransferAllowedLocked reports whether this session is
// warping out of an active Anton energy-interception run (218) into one of the
// hatcheries the raid has already opened (221..224).
//
// The client sends the ordinary C2S op15/op16 selection for that warp -- the
// corner-room wall_open warp objects carry `[int data] 0 150 <221|222|223|224>`
// and live traffic shows the request arriving every time -- but the two
// dungeon-runtime guards rejected it with "dnf dungeon session is already
// active: dungeon=218 maze=0", which left the hidden area unreachable: raid_map_id
// 221..224 never appears anywhere in the packet log. Nothing on the client side is
// missing; only the server-side in-run transfer was. The caller retires the
// previous run instead of abandoning it. Caller holds session.dungeon.mu.
func (s *Service) runtimeRaidHatcheryTransferAllowedLocked(
	session *gameSession,
	previous *runtimeDungeonState,
	target int64,
) bool {
	if s == nil || session == nil || previous == nil {
		return false
	}
	if previous.Dungeon.ID != antonEnergyInterceptionMapID || !isAntonHatcheryMapID(int(target)) {
		return false
	}
	characterID := session.selectedCharacterID
	if characterID == 0 {
		return false
	}
	s.raidMu.Lock()
	defer s.raidMu.Unlock()
	state := s.runtimeRaidByMemberLocked(characterID)
	if state == nil || !state.Started || state.TailFlag == 1 {
		return false
	}
	if state.Lifecycle.Phase != runtimeRaidDungeonPhaseTwo || state.Lifecycle.Status != runtimeRaidLifecyclePhaseActive {
		return false
	}
	objective, ok := state.Lifecycle.Objectives[int(target)]
	return ok && objective.State == raid.AntonDungeonOpen
}

// runtimeRaidHatcheryWarpAdmission owns the op15 half of the 218 -> 221..224 warp.
//
// The wall_open warp object sends the ordinary op15/op16 pair, and
// runtimeRaidSelectorAdmission owns op15 for the whole raid channel. A member
// standing inside the energy-interception gate is by definition not in the private
// waiting instance, so the portal was refused and -- because that refusal answers
// nothing -- the client never received the op15 ACK, the fatigue row or the op27
// selector page its own scene finalizer is paired with. The warp is therefore
// admitted on the session's committed run: the member must still be inside 218 and
// at least one hatchery the phase opened must still be enterable. The op16 that
// follows keeps every other gate (objective state, group occupancy, run
// lifecycle), so this only restores the answer the client waits for.
//
// Caller must not hold raidMu: the state it passes is a snapshot and this helper
// takes session.dungeon.mu, which is the outer lock everywhere else.
func (s *Service) runtimeRaidHatcheryWarpAdmission(session *gameSession, state runtimeRaidState) bool {
	if s == nil || session == nil || session.selectedCharacterID == 0 {
		return false
	}
	if state.TailFlag == 1 ||
		state.Lifecycle.Phase != runtimeRaidDungeonPhaseTwo ||
		state.Lifecycle.Status != runtimeRaidLifecyclePhaseActive {
		return false
	}
	session.dungeon.mu.Lock()
	runtime := session.dungeon.runtime
	inEnergyInterception := runtime != nil &&
		dungeonRuntimeOwnsCharacter(runtime, session.selectedCharacterID) &&
		int(runtime.Dungeon.ID) == antonEnergyInterceptionMapID
	session.dungeon.mu.Unlock()
	if !inEnergyInterception {
		return false
	}
	for mapID := antonFirstHatcheryMapID; mapID <= antonLastHatcheryMapID; mapID++ {
		objective, ok := state.Lifecycle.Objectives[mapID]
		if ok && objective.Phase == runtimeRaidDungeonPhaseTwo && objective.State == raid.AntonDungeonOpen {
			return true
		}
	}
	return false
}

// runtimeRaidHatcheryWarpApplies reports whether one select is the in-run warp out
// of the energy-interception gate into an open hatchery, i.e. whether the squad's
// single gate has to move with the member instead of blocking it.
//
// Every input comes from the raid state alone: the target map is a hatchery the
// phase still offers, the squad's occupancy is the energy-interception gate of the
// same phase, and the warping member is the one that owns that gate's run and has
// not completed it. A squadmate that never opened 218 can therefore not use this
// path to move the squad's occupancy. Caller holds raidMu.
func runtimeRaidHatcheryWarpApplies(state *runtimeRaidState, seat runtimeRaidDungeonSeat, characterID uint16, phase byte, mapID int) bool {
	if state == nil || state.TailFlag == 1 ||
		phase != runtimeRaidDungeonPhaseTwo ||
		!isAntonHatcheryMapID(mapID) {
		return false
	}
	objective, ok := state.Lifecycle.Objectives[mapID]
	if !ok || objective.Phase != phase || objective.State != raid.AntonDungeonOpen {
		return false
	}
	occupied, ok := state.Lifecycle.GroupOccupancy[seat]
	if !ok || occupied.Phase != phase || occupied.MapID != antonEnergyInterceptionMapID {
		return false
	}
	progress, ok := state.Lifecycle.Dungeons[runtimeRaidDungeonRunKey{
		MapID: antonEnergyInterceptionMapID,
		Seat:  seat,
	}]
	if !ok || progress.Status == runtimeRaidDungeonProgressCompleted {
		return false
	}
	// The run belongs to the whole squad, not just to whoever opened it: every
	// member of a squad walks into the same gate and adopts the opener's binding,
	// so the second member's warp has to move that same run. Requiring the opener
	// here is what let only the FIRST member through and refused the rest with
	// "runtime raid group already occupies a dungeon: group=N map=218" -- live
	// operator report 2026-09-19: "孵化中心只能进去一个人 其他的人强制返回城镇了".
	// A member of another squad (same seat label, different character) still cannot
	// move this run.
	if progress.CharacterID != characterID {
		owner, found := runtimeRaidFrozenMember(*state, progress.CharacterID)
		if !found || runtimeRaidSeatFor(owner) != seat {
			return false
		}
	}
	return true
}

// runtimeRaidPhaseTwoGateAfterVolcano reports whether one phase-two gate is one the
// falling volcano closes. 86JP's ClearBlackVolcanoAsync invalidates the runs of
// 218/219 and 221..224 (:1732-1734); 220, the heart, is the objective it opens.
func runtimeRaidPhaseTwoGateAfterVolcano(mapID int) bool {
	return mapID == antonEnergyInterceptionMapID ||
		mapID == antonBlackVolcanoMapID ||
		isAntonHatcheryMapID(mapID)
}

// changeRuntimeRaidBarrierLocked applies one shield operation and flips the
// volcano authority on the two edges. It returns the timers to schedule when
// the shield just broke (recharge starts). Caller holds raidMu.
func changeRuntimeRaidBarrierLocked(state *runtimeRaidState, operand uint32, operation byte, now time.Time) (runtimeRaidBarrierChange, []runtimeRaidMechanismTimer, bool) {
	if state.TailFlag == 1 || state.Lifecycle.Phase != runtimeRaidDungeonPhaseTwo || state.Lifecycle.Status != runtimeRaidLifecyclePhaseActive {
		return runtimeRaidBarrierChange{}, nil, false
	}
	previous := state.Lifecycle.Barrier
	next, ok := applyRuntimeRaidSymbolOperation(previous, operand, operation)
	if !ok {
		return runtimeRaidBarrierChange{}, nil, false
	}
	if next > antonBarrierMaximum {
		next = antonBarrierMaximum
	}
	if next == previous {
		return runtimeRaidBarrierChange{Previous: previous, Next: next}, nil, true
	}
	change := runtimeRaidBarrierChange{Previous: previous, Next: next}
	state.Lifecycle.Barrier = next
	var armed []runtimeRaidMechanismTimer
	switch {
	case next == 0 && previous != 0:
		change.Broke = true
		state.Lifecycle.VolcanoOpen = true
		// The client's whole barrier reaction hangs off symbol changes
		// (etc/raid/anton.etc: [ON CHANGE SYMBOL][BLACK VOLCANO BARRIER] 0 ->
		// [SET] [MOVIE BARRIER BREAK] 1). 86JP ships the edge as one atomic set
		// {recover:0, barrier:0, volcano:1, break:1}; broadcastRuntimeRaidBarrier
		// below mirrors that, and the markers are persisted so a later snapshot
		// replays the same state.
		state.setRuntimeRaidMovieSymbolLocked(antonSymbolBarrierBreak, 1)
		state.setRuntimeRaidMovieSymbolLocked(antonSymbolBarrierRecover, 0)
		armed = append(armed, state.armRuntimeRaidMechanismLocked(runtimeRaidMechanismBarrierRecovery, antonBlackVolcanoMapID, now, antonBarrierRecoveryTick, 0))
	case next == antonBarrierMaximum && previous != antonBarrierMaximum:
		change.Restored = true
		state.Lifecycle.VolcanoOpen = false
		state.setRuntimeRaidMovieSymbolLocked(antonSymbolBarrierRecover, 1)
		state.setRuntimeRaidMovieSymbolLocked(antonSymbolBarrierBreak, 0)
		state.cancelRuntimeRaidMechanismLocked(runtimeRaidMechanismBarrierRecovery, antonBlackVolcanoMapID)
	}
	state.Lifecycle.Revision++
	return change, armed, true
}

// runtimeRaidBarrierChargeLocked is one recharge tick while the volcano is
// open. Caller holds raidMu.
func runtimeRaidBarrierChargeLocked(state *runtimeRaidState, now time.Time) (runtimeRaidBarrierChange, []runtimeRaidMechanismTimer, bool) {
	if !state.Lifecycle.VolcanoOpen {
		return runtimeRaidBarrierChange{}, nil, false
	}
	rate := antonBarrierChargePerSecond
	if state.Lifecycle.InfectionMapID != 0 && !state.Lifecycle.InfectionSuppressed {
		rate = antonBarrierInfectedChargePerSecond
	}
	change, _, ok := changeRuntimeRaidBarrierLocked(state, rate, runtimeRaidSymbolOperationAdd, now)
	if !ok {
		return change, nil, false
	}
	var armed []runtimeRaidMechanismTimer
	if !change.Restored {
		armed = append(armed, state.armRuntimeRaidMechanismLocked(runtimeRaidMechanismBarrierRecovery, antonBlackVolcanoMapID, now, antonBarrierRecoveryTick, 0))
	}
	return change, armed, true
}

// runtimeRaidInfectionMapIDForWire is the one value this client resolves the
// 感染孵化场 (infected hatchery) from: the trailing dword of 0x0249.
//
// The current EXE's 0x0249 handler sub_1D39B50 reads that dword AFTER the
// (dungeonID,state) rows and calls sub_17B87B0(manager, value), which is the ONLY
// writer of manager+0x150 in the whole binary; it then re-derives symbol 8
// ([INFECTION DUNGEON INDEX]) from it and symbol 7 ([IS EXIST INFECTION DUNGEON])
// from "value != 0". The 战况表 renderers read that same field back
// (sub_1CF26F0 is `mov eax,[ecx+0x150]; ret`, consumed inside the selector/board
// init sub_F176A0 and inside the board iterators sub_17C29F0 / sub_17C2B80, which
// compare it against each dungeon id).
//
// Two consequences this helper exists for:
//
//  1. A zero tail is not "no information": the client stores 0 and CLEARS symbol 8
//     that the 0x0247 table just set, because 0x0249 always follows 0x0247 in the
//     snapshot sequence. Leaving the tail unset is therefore what kept the board
//     from ever colouring the infected hatchery.
//  2. Suppressed means the infected hatchery was just cleared. 86JP writes symbol 7
//     back to 0 on that edge (ClearHatcheryAsync, Progression.cs:1746-1764) and this
//     client derives symbol 7 from the tail, so 0 is the only value that leaves the
//     two projections agreeing. It is deliberately NOT the remembered map id.
//
// Shared by the symbol rows and the state table so the two can never disagree.
func runtimeRaidInfectionMapIDForWire(state runtimeRaidState) uint32 {
	if state.Lifecycle.InfectionMapID == 0 || state.Lifecycle.InfectionSuppressed {
		return 0
	}
	return uint32(state.Lifecycle.InfectionMapID)
}

// runtimeRaidPhaseTwoSymbolRows are the non-objective symbol rows the phase-two
// script reads: init flag, shield, volcano authority, infection, and the two
// presentation symbols of every hatchery.
//
// The hatchery pair ([221..224 HATCHERY FAIL] = 3..6 and
// [221..224 HATCHERY NAMED CREATE] = 9..12, ids from etc/raid/anton.etc.symbol)
// is driven by the timer windows this service arms, and the client's raid UI
// resolves both ids for every hatchery row it draws. They used to be left out of
// the table entirely, so the first time the client's own script (or a status
// board rebuild) touched one of them it walked an object that had never been
// created. Live 2026-09-17, that single walk is what both remaining crashes land
// in:
//
//	entry into a hatchery      0x025BD2FF / 0x0263A2ED   eip=0 (null call)
//	卡牌退出回城 rebuilding 战况表  0x01F95C63 / 0x01F95611   access_address=0xB98
//
// Both appeared only after the hatchery effect timers were introduced (before
// that, entering a hatchery was clean and only the return-to-town walk existed),
// which is consistent with "the timer is what makes the client evaluate the
// hatchery branch at all".
//
// 86JP seeds exactly these eight ids with 0 in AntonSecondPhaseInitialSymbols
// (ServerS4A21-master, RaidHandler.AntonDefinitions.cs:116-138). The value sent
// here is this service's current one for the id, so a fresh phase starts at 0
// and a later boundary snapshot repeats what the server already believes instead
// of forcing 0.
func runtimeRaidPhaseTwoSymbolRows(state runtimeRaidState) []raid.RaidSymbolRow {
	if state.Lifecycle.Phase != runtimeRaidDungeonPhaseTwo || !runtimeRaidUsesAntonTeamGraph(state) {
		// 下面每个键都是 etc/raid/anton.etc.symbol 里的安徒恩符号 id。卢克有它自己的
		// 符号表，这些 id 在里面含义完全不同，所以卢克快照不能带它们。
		//
		// 上游同形（原来是 TailFlag == 1）：安徒恩与单人模式的求值结果完全相同，
		// 只有队伍卢克从「照发安徒恩符号」变成「不发」。
		return nil
	}
	volcano := uint32(0)
	if state.Lifecycle.VolcanoOpen {
		volcano = 1
	}
	infectionMapID := runtimeRaidInfectionMapIDForWire(state)
	infection := uint32(0)
	if infectionMapID != 0 {
		infection = 1
	}
	rows := []raid.RaidSymbolRow{
		{Key: antonSymbolInfectionFlag, Value: infection},
		{Key: antonSymbolInfectionMapID, Value: infectionMapID},
		{Key: antonSymbolPhaseTwoInit, Value: 1},
		{Key: antonSymbolBarrier, Value: state.Lifecycle.Barrier},
		{Key: antonSymbolVolcanoOpen, Value: volcano},
	}
	for mapID := antonFirstHatcheryMapID; mapID <= antonLastHatcheryMapID; mapID++ {
		fail, named, ok := antonHatcherySymbolPlan(mapID)
		if !ok {
			continue
		}
		rows = append(rows,
			raid.RaidSymbolRow{Key: fail, Value: state.Lifecycle.MovieSymbols[fail]},
			raid.RaidSymbolRow{Key: named, Value: state.Lifecycle.MovieSymbols[named]})
	}
	return rows
}

// openRuntimeRaidHatcheriesLocked opens antonHatcheryOpenCount of the four
// hatcheries, leaving one out, and infects one of the open ones. Caller holds
// raidMu; pick returns a value in [0, n).
func openRuntimeRaidHatcheriesLocked(state *runtimeRaidState, pick func(n int) int) ([]int, int) {
	hatcheries := make([]int, 0, antonLastHatcheryMapID-antonFirstHatcheryMapID+1)
	for mapID := antonFirstHatcheryMapID; mapID <= antonLastHatcheryMapID; mapID++ {
		if objective, ok := state.Lifecycle.Objectives[mapID]; ok && objective.Phase == runtimeRaidDungeonPhaseTwo {
			hatcheries = append(hatcheries, mapID)
		}
	}
	if len(hatcheries) == 0 {
		return nil, 0
	}
	omitted := pick(len(hatcheries))
	opened := make([]int, 0, antonHatcheryOpenCount)
	for index, mapID := range hatcheries {
		if index == omitted || len(opened) >= antonHatcheryOpenCount {
			continue
		}
		opened = append(opened, mapID)
	}
	if len(opened) == 0 {
		return nil, 0
	}
	infection := opened[pick(len(opened))]
	for _, mapID := range opened {
		objective := state.Lifecycle.Objectives[mapID]
		objective.State = raid.AntonDungeonOpen
		objective.RemainingHP = objective.MaxHP
		objective.ClearCount = 0
		state.Lifecycle.Objectives[mapID] = objective
	}
	state.Lifecycle.InfectionMapID = infection
	state.Lifecycle.InfectionSuppressed = false
	// etc/raid/anton.etc fires the hatchery cutscene with the same trigger that
	// opens them: [OPEN RANDOM HATCHERY] + [SET] [MOVIE HATCHERY OPEN] 1 (symbol
	// 128). It rides the snapshot the caller publishes right after.
	state.setRuntimeRaidMovieSymbolLocked(antonSymbolMovieHatcheryOpen, 1)
	state.Lifecycle.Revision++
	return opened, infection
}

// applyRuntimeRaidAntonPhaseTwoClearLocked reacts to one committed phase-two
// clear. Energy interception is repeatable, hatcheries recover after a delay,
// the volcano ends the siege. Caller holds raidMu.
func applyRuntimeRaidAntonPhaseTwoClearLocked(state *runtimeRaidState, mapID int, now time.Time) []runtimeRaidMechanismTimer {
	if state == nil || state.TailFlag == 1 || state.Lifecycle.Phase != runtimeRaidDungeonPhaseTwo ||
		state.Lifecycle.Status != runtimeRaidLifecyclePhaseActive {
		return nil
	}
	if runtimeRaidPhaseObjectivesComplete(*state, runtimeRaidDungeonPhaseTwo) {
		state.clearRuntimeRaidMechanismsLocked()
		return nil
	}
	objective, ok := state.Lifecycle.Objectives[mapID]
	if !ok || objective.RemainingHP != 0 {
		return nil
	}
	switch {
	case mapID == antonEnergyInterceptionMapID:
		objective.State = raid.AntonDungeonOpen
		objective.RemainingHP = objective.MaxHP
		objective.ClearCount = 0
		state.Lifecycle.Objectives[mapID] = objective
		state.Lifecycle.Revision++
		return nil
	case mapID == antonBlackVolcanoMapID:
		// advanceRuntimeRaidObjectiveLocked already hid 218/219 and the
		// hatcheries and opened the heart. End the siege bookkeeping.
		state.Lifecycle.VolcanoOpen = false
		state.Lifecycle.InfectionMapID = 0
		state.Lifecycle.InfectionSuppressed = false
		state.Lifecycle.Barrier = 0
		// 86JP's ClearBlackVolcanoAsync drives every hatchery's presentation pair
		// back to 0 as well (:1726-1730); the entries stay in MovieSymbols, so
		// without this the next snapshot re-pulses a hatchery the volcano just
		// closed and the client replays its hatchery animation.
		for hatchery := antonFirstHatcheryMapID; hatchery <= antonLastHatcheryMapID; hatchery++ {
			if fail, named, ok := antonHatcherySymbolPlan(hatchery); ok {
				state.setRuntimeRaidMovieSymbolLocked(fail, 0)
				state.setRuntimeRaidMovieSymbolLocked(named, 0)
			}
		}
		state.clearRuntimeRaidMechanismsLocked()
		state.Lifecycle.Revision++
		return nil
	case isAntonHatcheryMapID(mapID):
		// 86JP's ClearHatcheryAsync cancels timers 1 and 3 for the dungeon and
		// drives both presentation symbols back to 0 before starting the
		// recovery timer; that edge reaches the client through the snapshot the
		// caller publishes. Clearing the infected hatchery also clears the
		// infection marker itself (symbol 7 -> 0, Progression.cs:1750-1764), which
		// drops the shield recharge back to its uninfected rate until that
		// hatchery recovers.
		state.cancelRuntimeRaidHatcheryEffectTimersLocked(mapID)
		if mapID == state.Lifecycle.InfectionMapID {
			state.Lifecycle.InfectionSuppressed = true
		}
		if fail, named, ok := antonHatcherySymbolPlan(mapID); ok {
			state.setRuntimeRaidMovieSymbolLocked(fail, 0)
			state.setRuntimeRaidMovieSymbolLocked(named, 0)
		}
		return []runtimeRaidMechanismTimer{
			state.armRuntimeRaidMechanismLocked(runtimeRaidMechanismRecovery, mapID, now, antonHatcheryRecovery, 0),
		}
	default:
		return nil
	}
}

// currentRaidInRunTransferEntry reports whether one op16 moves the requesting raid
// member between its own runs of the ACTIVE phase, instead of forming a new party
// entry into the raid.
//
// It is the gate for skipping the party fan-out preflight (and with it the town
// co-presence test, which cannot hold while the party is inside the raid): the
// member must be a frozen member of a started raid, its phase must still be the
// one this map belongs to, and the session must already hold a committed run of
// that same raid whose binding names the very dungeon the member is standing in.
// A member standing in town holds no such run, so an ordinary party entry into a
// raid gate keeps every gate it has today.
func (s *Service) currentRaidInRunTransferEntry(
	session *gameSession,
	request dungeoncmd.SelectDungeonRequest,
) bool {
	if s == nil || session == nil || session.selectedCharacterID == 0 || request.DungeonID == 0 {
		return false
	}
	target := int(request.DungeonID)
	phase, raidMap := runtimeRaidDungeonPhaseForMap(target)
	if !raidMap {
		return false
	}
	state, ok := s.runtimeRaidForMember(session.selectedCharacterID)
	if !ok || !state.Started || state.TailFlag == 1 ||
		state.Lifecycle.Status != runtimeRaidLifecyclePhaseActive ||
		state.Lifecycle.Phase != phase {
		return false
	}
	session.dungeon.mu.Lock()
	defer session.dungeon.mu.Unlock()
	runtime := session.dungeon.runtime
	if runtime == nil || !dungeonRuntimeOwnsCharacter(runtime, session.selectedCharacterID) {
		return false
	}
	return runtime.raidDungeon.RaidKey == state.RaidKey && runtime.raidDungeon.RaidKey != 0
}

// runtimeRaidCurrentRaidDungeonRun reads the session's live raid dungeon
// binding: which raid map it is inside and the shared run token of its group.
//
// The binding's token is the SQUAD's run token, never the adopting member's own
// (see the adoption branch of beginRuntimeRaidDungeon), so it must not be compared
// with the session's local token here. That comparison silently dropped every
// shield report from every member but the run's opener: live 2026-09-19
// 14:18-14:19Z, raid 1507338, 26 game-raid-barrier-request-rejected entries with
// in_run=false while both members of squad 1 stood inside 218, and the volcano's
// shield only fell on the runs the opener itself reported. What the report needs to
// prove is that this member is inside the gate its binding names; the binding's
// token stays the shared per-run transmission budget.
func runtimeRaidCurrentRaidDungeonRun(session *gameSession) (runtimeRaidDungeonBinding, bool) {
	if session == nil {
		return runtimeRaidDungeonBinding{}, false
	}
	session.dungeon.mu.Lock()
	defer session.dungeon.mu.Unlock()
	runtime := session.dungeon.runtime
	if runtime == nil || runtime.raidDungeon.RaidKey == 0 || runtime.raidDungeon.LifecycleToken == 0 ||
		!dungeonRuntimeOwnsCharacter(runtime, session.selectedCharacterID) {
		return runtimeRaidDungeonBinding{}, false
	}
	if runtime.raidDungeon.MapID == 0 || runtime.raidDungeon.MapID != int(runtime.Dungeon.ID) {
		return runtimeRaidDungeonBinding{}, false
	}
	return runtime.raidDungeon, true
}

// applyRuntimeRaidBarrierRequest is the team-raid owner of op662. The request
// must come from inside a live energy-interception or hatchery run of the
// active phase two and stay within that run's transmission budget.
func (s *Service) applyRuntimeRaidBarrierRequest(session *gameSession, req raid.SetSymbolRequest) (bool, error) {
	characterID := selectedCharacterID(session)
	if characterID == 0 {
		return false, nil
	}
	run, inRun := runtimeRaidCurrentRaidDungeonRun(session)
	if !inRun || run.Phase != runtimeRaidDungeonPhaseTwo ||
		(run.MapID != antonEnergyInterceptionMapID && !isAntonHatcheryMapID(run.MapID)) {
		s.logGameEvent(session, "game-raid-barrier-request-rejected",
			"symbol", req.SourceValue, "operand", req.TargetValue, "operation", req.Symbol,
			"in_run", inRun, "run_map_id", run.MapID, "reason", "not_inside_energy_or_hatchery_run")
		return true, nil
	}
	now := s.gameplayNow()
	s.raidMu.Lock()
	state := s.runtimeRaidByMemberLocked(characterID)
	if state == nil || state.RaidKey != run.RaidKey || state.TailFlag == 1 || !state.Started ||
		state.Lifecycle.Phase != runtimeRaidDungeonPhaseTwo || state.Lifecycle.Status != runtimeRaidLifecyclePhaseActive {
		s.raidMu.Unlock()
		s.logGameEvent(session, "game-raid-barrier-request-rejected", "reason", "raid_not_in_active_phase_two")
		return true, nil
	}
	state.ensureRuntimeRaidLifecycleMapsLocked()
	limit := antonBarrierRequestLimit(run.MapID, state.Lifecycle.InfectionMapID, req.SourceValue, req.TargetValue, req.Symbol)
	if limit == 0 {
		s.raidMu.Unlock()
		s.logGameEvent(session, "game-raid-barrier-request-rejected",
			"symbol", req.SourceValue, "operand", req.TargetValue, "operation", req.Symbol,
			"run_map_id", run.MapID, "reason", "not_a_valid_shield_report")
		return true, nil
	}
	key := runtimeRaidBarrierRequestKey{MapID: run.MapID, LifecycleToken: run.LifecycleToken, Operand: req.TargetValue}
	if state.Lifecycle.BarrierRequests[key] >= limit {
		s.raidMu.Unlock()
		s.logGameEvent(session, "game-raid-barrier-request-rejected",
			"operand", req.TargetValue, "run_map_id", run.MapID, "limit", limit, "reason", "run_budget_exhausted")
		return true, nil
	}
	state.Lifecycle.BarrierRequests[key]++
	change, armed, ok := changeRuntimeRaidBarrierLocked(state, req.TargetValue, req.Symbol, now)
	snapshot := cloneRuntimeRaidState(*state)
	s.raidMu.Unlock()
	if !ok {
		return true, nil
	}
	if err := s.scheduleRuntimeRaidMechanismTimers(snapshot.RaidKey, armed); err != nil {
		s.logPacketEvent("game-raid-barrier-recovery-timer-failed", "raid_key", snapshot.RaidKey, "error", err)
	}
	s.logGameEvent(session, "game-raid-barrier-changed",
		"raid_key", snapshot.RaidKey,
		"run_map_id", run.MapID,
		"operand", req.TargetValue,
		"previous", change.Previous,
		"next", change.Next,
		"broke", change.Broke,
		"source", "op662")
	return true, s.broadcastRuntimeRaidBarrier(snapshot, change)
}

// broadcastRuntimeRaidMovieSymbolRows pulses cutscene markers on the 0x0247 symbol
// channel: the current client's handler (sub_1D39AE0) feeds SetRaidSymbol, which is
// what makes it play Video/AntonCutScene and the barrier animations.
func (s *Service) broadcastRuntimeRaidMovieSymbolRows(state runtimeRaidState, rows []raid.RaidSymbolRow) error {
	if len(rows) == 0 {
		return nil
	}
	body, err := raid.BuildRaidSymbolBody(rows)
	if err != nil {
		return err
	}
	return s.broadcastRuntimeRaidWireSequence(state, runtimeRaidWireNotification{msgID: raid.RaidSymbolMsgID, body: body})
}

// broadcastRuntimeRaidBarrier publishes the shield rows to every online member.
// On the two edges the volcano authority row travels with it, plus the cutscene
// markers the PVF reaction is keyed on (126 break / 127 recover), matching 86JP's
// atomic edge set.
func (s *Service) broadcastRuntimeRaidBarrier(state runtimeRaidState, change runtimeRaidBarrierChange) error {
	rows := []raid.RaidSymbolRow{{Key: antonSymbolBarrier, Value: state.Lifecycle.Barrier}}
	if change.Broke || change.Restored {
		volcano := uint32(0)
		if state.Lifecycle.VolcanoOpen {
			volcano = 1
		}
		rows = append(rows, raid.RaidSymbolRow{Key: antonSymbolVolcanoOpen, Value: volcano})
		for _, marker := range []uint32{antonSymbolBarrierBreak, antonSymbolBarrierRecover} {
			value, ok := state.Lifecycle.MovieSymbols[marker]
			if !ok {
				continue
			}
			rows = append(rows, raid.RaidSymbolRow{Key: marker, Value: value})
		}
	}
	body, err := raid.BuildRaidSymbolBody(rows)
	if err != nil {
		return err
	}
	return s.broadcastRuntimeRaidWireSequence(state, runtimeRaidWireNotification{msgID: raid.RaidSymbolMsgID, body: body})
}

// fireRuntimeRaidPhaseTwoMechanismLocked dispatches a phase-two timer whose
// generation has already been validated. Caller holds raidMu.
func (s *Service) fireRuntimeRaidPhaseTwoMechanismLocked(state *runtimeRaidState, armed runtimeRaidMechanismTimer, due time.Time) ([]runtimeRaidMechanismTimer, string, *runtimeRaidBarrierChange) {
	switch armed.Kind {
	case runtimeRaidMechanismHatcheryOpen:
		if isAntonHatcheryMapID(armed.MapID) {
			// Wire timer type 3 on a hatchery key is the client's
			// [HATCHERY NAMED CREATE] window, not the volcano open timer on 219.
			armedNext, outcome := state.fireRuntimeRaidHatcheryEffectTimerLocked(armed.MapID, armed.Kind, due)
			return armedNext, outcome, nil
		}
		opened, _ := openRuntimeRaidHatcheriesLocked(state, s.runtimeRaidRandomIndex)
		if len(opened) == 0 {
			return nil, "", nil
		}
		// 86JP starts the hatchery effect timers in the same step that opens the
		// hatcheries (RunHatcheryOpenTimerAsync -> StartHatcheryEffectTimersAsync);
		// they reach the client in the snapshot this callback publishes.
		armedNext := make([]runtimeRaidMechanismTimer, 0, 2*len(opened))
		for _, mapID := range opened {
			armedNext = append(armedNext, state.armRuntimeRaidHatcheryEffectTimersLocked(mapID, due)...)
		}
		return armedNext, "hatcheries_opened", nil
	case runtimeRaidMechanismActive:
		if isAntonHatcheryMapID(armed.MapID) {
			// Wire timer type 1 on a hatchery key is the client's
			// [HATCHERY FAIL] window. Phase one's type-1 active windows never
			// name a hatchery, and phase two has no other type-1 owner.
			armedNext, outcome := state.fireRuntimeRaidHatcheryEffectTimerLocked(armed.MapID, armed.Kind, due)
			return armedNext, outcome, nil
		}
		return nil, "", nil
	case runtimeRaidMechanismRecovery:
		if !isAntonHatcheryMapID(armed.MapID) {
			return nil, "", nil
		}
		objective, ok := state.Lifecycle.Objectives[armed.MapID]
		if !ok {
			return nil, "", nil
		}
		objective.State = raid.AntonDungeonOpen
		objective.RemainingHP = objective.MaxHP
		objective.ClearCount = 0
		state.Lifecycle.Objectives[armed.MapID] = objective
		// The infected hatchery carries its infection back when it recovers
		// (86JP TryRecoverHatcheryAsync writes symbol 7 back to 1, :2335-2340).
		if armed.MapID == state.Lifecycle.InfectionMapID {
			state.Lifecycle.InfectionSuppressed = false
		}
		state.Lifecycle.Revision++
		// 86JP re-arms the pair after the recovery timer reopens the hatchery
		// (RunHatcheryRecoveryTimerAsync -> StartHatcheryEffectTimersAsync).
		return state.armRuntimeRaidHatcheryEffectTimersLocked(armed.MapID, due), "hatchery_recovered", nil
	case runtimeRaidMechanismBarrierRecovery:
		change, armedNext, ok := runtimeRaidBarrierChargeLocked(state, due)
		if !ok {
			return nil, "", nil
		}
		return armedNext, "barrier_charged", &change
	default:
		return nil, "", nil
	}
}

func (s *Service) runtimeRaidRandomIndex(n int) int {
	if n <= 1 {
		return 0
	}
	if s != nil && s.raidRewardRandom != nil {
		return int(s.raidRewardRandom() % uint64(n))
	}
	return rand.Intn(n)
}

func sortedRuntimeRaidMechanismTimers(state runtimeRaidState) []runtimeRaidMechanismTimer {
	timers := make([]runtimeRaidMechanismTimer, 0, len(state.Lifecycle.Mechanisms))
	for _, timer := range state.Lifecycle.Mechanisms {
		timers = append(timers, timer)
	}
	sort.Slice(timers, func(i, j int) bool {
		if timers[i].Kind != timers[j].Kind {
			return timers[i].Kind < timers[j].Kind
		}
		return timers[i].MapID < timers[j].MapID
	})
	return timers
}
