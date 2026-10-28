package dnfbridge

import (
	"fmt"
	"sort"
	"time"

	"longheng.io/server/internal/modules/dnf/raid"
)

// Anton phase one is a mechanism cycle, not a flat list of seven gates. The
// black-fog source (210) has to fall four times; that opens the two quakes
// (212/214) and the two legs (213/215), arms the regenerated fog (211) and the
// naval cannon (216) on their own timers, and starts the pressure clocks. Every
// mechanism dungeon that is left open past its active window fails the whole
// cycle: gates return to their PVF initial state, clear counts drop to zero,
// and the roster attacks again inside the same 40-minute phase clock.
//
// Required clears and initial gate states come from etc/raid/anton.etc, which
// the runtime already parses (HP / init state per [DUNGEON INFO]). The window
// lengths below are not in that file. They follow the read-only 86JP C# server
// (Network/Handlers/RaidHandler.AntonDefinitions.cs, .Progression.cs and
// .Runtime.cs) and are a hypothesis until the current EXE confirms them.
const (
	antonBlackFogSourceMapID = 210
	antonBlackFogMapID       = 211
	antonQuakeAMapID         = 212
	antonLegAMapID           = 213
	antonQuakeBMapID         = 214
	antonLegBMapID           = 215
	antonNavalCannonMapID    = 216

	antonBlackFogRecovery = 300 * time.Second
	antonBlackFogActive   = 480 * time.Second
	antonQuakeRecovery    = 150 * time.Second
	antonQuakeActive      = 300 * time.Second
	antonNavalOpenDelay   = 20 * time.Second
	antonNavalRecovery    = 150 * time.Second
	antonNavalActive      = 360 * time.Second

	// Timer kinds are the 86JP RAID_SET_TIMER key0 values. They ride in the
	// current EXE's 0x0254 layout as TimerType, with the dungeon id in Value0.
	runtimeRaidMechanismActive    byte = 1
	runtimeRaidMechanismRecovery  byte = 2
	runtimeRaidMechanismNavalOpen byte = 5

	// One-shot phase-one cutscene markers (etc/raid/anton.etc.symbol): the
	// black-fog source being spent sets [DGN SMOKE OPEN] and [MOVIE SMOKE OUT].
	antonSymbolSmokeOpen     uint32 = 120
	antonSymbolMovieSmokeOut uint32 = 122
)

type runtimeRaidMechanismKey struct {
	Kind  byte
	MapID int
}

type runtimeRaidMechanismTimer struct {
	Kind       byte
	MapID      int
	Generation uint64
	Cycle      uint64
	StartedAt  time.Time
	Deadline   time.Time
	// ActiveAfter is the active window that follows a recovery / open timer
	// once its dungeon reopens.
	ActiveAfter time.Duration
}

func (timer runtimeRaidMechanismTimer) key() runtimeRaidMechanismKey {
	return runtimeRaidMechanismKey{Kind: timer.Kind, MapID: timer.MapID}
}

// antonPhaseOneOpensWithBlackFogSource lists the gates that open the moment
// the black-fog source is spent.
func antonPhaseOneOpensWithBlackFogSource(mapID int) bool {
	switch mapID {
	case antonQuakeAMapID, antonLegAMapID, antonQuakeBMapID, antonLegBMapID:
		return true
	default:
		return false
	}
}

// runtimeRaidPhaseOneGateClosedByFogRegeneration reports whether one phase-one gate is
// one the regenerated black fog closes.
//
// Operator's rule, 2026-09-20: "当黑雾重生倒计时完毕 战况表和选择副本界面就没有擎天
// 震颤 舰炮了 相当于副本就进不去了 … 所有还在擎天 震颤 舰炮的小队 全部强制退出副本
// （还是以组队的状态）". The quakes (212/214), the legs (213/215) and the naval cannon
// (216) are exactly the gates the spent black-fog source opens
// (antonPhaseOneOpensWithBlackFogSource), so they are the ones its regeneration closes.
func runtimeRaidPhaseOneGateClosedByFogRegeneration(mapID int) bool {
	switch mapID {
	case antonQuakeAMapID, antonQuakeBMapID, antonLegAMapID, antonLegBMapID, antonNavalCannonMapID:
		return true
	default:
		return false
	}
}

// runtimeRaidPhaseOneGateClosedByCycleReset reports whether one phase-one gate is one the
// mechanism-failure cycle reset closes.
//
// A reset restores every phase-one gate to its PVF initial state (raid_lifecycle.go
// runtimeRaidResetAntonPhaseOneLocked), so the quakes, the legs, the naval cannon AND the
// regenerated fog all close while the black-fog source (210) reopens. Anyone still inside a
// closed gate is standing in a dungeon the raid no longer owns and must come home.
//
// This is the edge the operator actually hits: the fog's own respawn countdown
// (antonBlackFogRecovery) and the quakes' active windows (antonQuakeActive) are both 300s
// and are armed in the same instant, so the quake timer normally expires FIRST and resets
// the cycle before the fog-recovery task can run. Live 2026-09-20T13:50:09Z, raid 1507329:
//
//	13:45:09 anton-mechanisms-armed cleared_map_id=210 timers=[1/212@13:50:09 1/214@13:50:09 2/211@13:50:09 5/…]
//	13:50:09 anton-mechanism-fired kind=1 map_id=212 outcome=phase_one_reset phase_cycle=1 timers=[]
//	13:50:56…13:51:03 the four members still inside 213/215 clicked 回城 themselves (op72)
//
// i.e. the countdown ended and nobody was sent home -- the operator's "黑雾副本重生倒计时
// 结束后 在副本里的队伍不强制回城".
func runtimeRaidPhaseOneGateClosedByCycleReset(mapID int) bool {
	if mapID == antonBlackFogSourceMapID {
		// 黑雾之源 reopens with the reset; its occupants keep a valid gate.
		return false
	}
	return mapID == antonBlackFogMapID || runtimeRaidPhaseOneGateClosedByFogRegeneration(mapID)
}

// antonPhaseOneRecoveryPlan returns how long a cleared mechanism dungeon stays
// down and how long it then stays open before failing the cycle.
func antonPhaseOneRecoveryPlan(mapID int) (recovery, active time.Duration, ok bool) {
	switch mapID {
	case antonBlackFogMapID:
		return antonBlackFogRecovery, antonBlackFogActive, true
	case antonQuakeAMapID, antonQuakeBMapID:
		return antonQuakeRecovery, antonQuakeActive, true
	case antonNavalCannonMapID:
		return antonNavalRecovery, antonNavalActive, true
	default:
		return 0, 0, false
	}
}

func (state *runtimeRaidState) armRuntimeRaidMechanismLocked(kind byte, mapID int, now time.Time, window, activeAfter time.Duration) runtimeRaidMechanismTimer {
	state.ensureRuntimeRaidLifecycleMapsLocked()
	state.Lifecycle.MechanismGeneration++
	timer := runtimeRaidMechanismTimer{
		Kind:        kind,
		MapID:       mapID,
		Generation:  state.Lifecycle.MechanismGeneration,
		Cycle:       state.Lifecycle.PhaseCycle,
		StartedAt:   now,
		Deadline:    now.Add(window),
		ActiveAfter: activeAfter,
	}
	state.Lifecycle.Mechanisms[timer.key()] = timer
	return timer
}

func (state *runtimeRaidState) cancelRuntimeRaidMechanismLocked(kind byte, mapID int) {
	delete(state.Lifecycle.Mechanisms, runtimeRaidMechanismKey{Kind: kind, MapID: mapID})
}

// clearRuntimeRaidMechanismsLocked drops every armed timer and advances the
// generation so callbacks already dequeued cannot act.
func (state *runtimeRaidState) clearRuntimeRaidMechanismsLocked() {
	state.Lifecycle.Mechanisms = make(map[runtimeRaidMechanismKey]runtimeRaidMechanismTimer)
	state.Lifecycle.MechanismGeneration++
}

// applyRuntimeRaidAntonPhaseOneClearLocked reacts to one committed phase-one
// clear after advanceRuntimeRaidObjectiveLocked has updated the objective. It
// returns the timers that now need scheduling. Caller holds raidMu.
func applyRuntimeRaidAntonPhaseOneClearLocked(state *runtimeRaidState, mapID int, now time.Time) []runtimeRaidMechanismTimer {
	if state == nil || state.TailFlag == 1 || state.Lifecycle.Phase != runtimeRaidDungeonPhaseOne ||
		state.Lifecycle.Status != runtimeRaidLifecyclePhaseActive {
		return nil
	}
	if runtimeRaidPhaseObjectivesComplete(*state, runtimeRaidDungeonPhaseOne) {
		// Both legs are down: no mechanism may fail a phase that is already won.
		state.clearRuntimeRaidMechanismsLocked()
		return nil
	}
	objective, ok := state.Lifecycle.Objectives[mapID]
	if !ok || objective.RemainingHP != 0 {
		return nil
	}
	var armed []runtimeRaidMechanismTimer
	switch mapID {
	case antonBlackFogSourceMapID:
		if len(state.Lifecycle.Mechanisms) != 0 {
			return nil
		}
		// The black-fog source is spent (four clears, HP 0). etc/raid/anton.etc
		// spells the client-side reaction itself: [TRIGGER][IF][DGN SMOKE OPEN] 0 /
		// [CHECK DUNGEON STATE] 210 `clear` -> [SET] [DGN SMOKE OPEN] 1 and
		// [SET] [MOVIE SMOKE OUT] 1, i.e. symbols 120 and 122. Without them the
		// squadmate's status board never plays the cutscene (live 2026-09-16: no
		// 121..128 key was ever broadcast in the whole log).
		state.setRuntimeRaidMovieSymbolLocked(antonSymbolSmokeOpen, 1)
		state.setRuntimeRaidMovieSymbolLocked(antonSymbolMovieSmokeOut, 1)
		armed = append(armed,
			state.armRuntimeRaidMechanismLocked(runtimeRaidMechanismActive, antonQuakeAMapID, now, antonQuakeActive, 0),
			state.armRuntimeRaidMechanismLocked(runtimeRaidMechanismActive, antonQuakeBMapID, now, antonQuakeActive, 0),
			state.armRuntimeRaidMechanismLocked(runtimeRaidMechanismRecovery, antonBlackFogMapID, now, antonBlackFogRecovery, antonBlackFogActive),
			state.armRuntimeRaidMechanismLocked(runtimeRaidMechanismNavalOpen, antonNavalCannonMapID, now, antonNavalOpenDelay, antonNavalActive),
		)
	case antonBlackFogMapID, antonQuakeAMapID, antonQuakeBMapID, antonNavalCannonMapID:
		recovery, active, _ := antonPhaseOneRecoveryPlan(mapID)
		state.cancelRuntimeRaidMechanismLocked(runtimeRaidMechanismActive, mapID)
		armed = append(armed,
			state.armRuntimeRaidMechanismLocked(runtimeRaidMechanismRecovery, mapID, now, recovery, active))
	}
	return armed
}

// runtimeRaidReopenAntonDungeonLocked brings a recovered mechanism dungeon
// back: open, full HP, clear count zero. Caller holds raidMu.
func runtimeRaidReopenAntonDungeonLocked(state *runtimeRaidState, mapID int) bool {
	objective, ok := state.Lifecycle.Objectives[mapID]
	if !ok || objective.Phase != runtimeRaidDungeonPhaseOne {
		return false
	}
	objective.State = raid.AntonDungeonOpen
	objective.RemainingHP = objective.MaxHP
	objective.ClearCount = 0
	state.Lifecycle.Objectives[mapID] = objective
	state.Lifecycle.Revision++
	return true
}

// runtimeRaidResetAntonPhaseOneLocked is the mechanism failure: every phase-one
// gate returns to its PVF initial state with zero clears, all timers die, and
// the cycle counter moves on. Runs still inside a dungeon are deliberately left
// alone; their late clears are ignored by advanceRuntimeRaidObjectiveLocked and
// they settle normally. Caller holds raidMu.
func runtimeRaidResetAntonPhaseOneLocked(state *runtimeRaidState) bool {
	config := runtimeRaidBaseConfig(*state)
	phaseConfig, ok := runtimeRaidAntonPhase(config, runtimeRaidDungeonPhaseOne)
	if !ok {
		return false
	}
	for _, dungeon := range phaseConfig.Dungeons {
		mapID := int(dungeon.Index)
		objective, exists := state.Lifecycle.Objectives[mapID]
		if !exists {
			continue
		}
		objective.State = dungeon.InitState
		objective.MaxHP = dungeon.HP
		objective.RemainingHP = dungeon.HP
		objective.ClearCount = 0
		state.Lifecycle.Objectives[mapID] = objective
	}
	state.clearRuntimeRaidMechanismsLocked()
	state.Lifecycle.PhaseCycle++
	state.Lifecycle.Revision++
	return true
}

func runtimeRaidMechanismTimerName(raidKey uint32, timer runtimeRaidMechanismTimer) string {
	return fmt.Sprintf("raid-mech:%d:%d:%d:%d", raidKey, timer.Kind, timer.MapID, timer.Generation)
}

// afterRuntimeRaidObjectiveAdvance runs once a completed dungeon has been
// committed to the raid objective and before the progress snapshot is
// published, so the snapshot already carries the new gates and timers.
func (s *Service) afterRuntimeRaidObjectiveAdvance(raidKey uint32, phase byte, mapID int) error {
	if s == nil || (phase != runtimeRaidDungeonPhaseOne && phase != runtimeRaidDungeonPhaseTwo) {
		return nil
	}
	now := s.gameplayNow()
	s.raidMu.Lock()
	state := s.raids[raidKey]
	if state == nil {
		s.raidMu.Unlock()
		return nil
	}
	// 卢克分支：只在「队伍卢克」时为真。安徒恩状态与单人模式都不会进这里，
	// 所以下面安徒恩那条路径逐字未动（上游那条 state.TailFlag == 1 的提前返回
	// 本树不加：我们的 apply 助手本来就拒绝单人模式，加了反而改变调用形态）。
	if state.TailFlag != 1 && runtimeRaidTeamGraphOwner(*state) == runtimeRaidOwnerLuke {
		// Luke's gates and mechanism timers are its own; Anton's map ids match
		// nothing in them.
		// The caller publishes the progress snapshot right after this returns, so
		// the gate states this arm changed reach the client there. The one-shot
		// markers do not ride that snapshot on purpose, so diff and pulse them
		// here exactly like the Anton arm below does.
		var lukeArmed []runtimeRaidMechanismTimer
		lukeSymbolsBefore := cloneRuntimeRaidMovieSymbols(state)
		if phase == runtimeRaidDungeonPhaseTwo {
			lukeArmed = applyRuntimeRaidLukePhaseTwoClearLocked(state, mapID, now)
		}
		lukePulses := runtimeRaidMovieSymbolChanges(lukeSymbolsBefore, state.Lifecycle.MovieSymbols)
		lukeSnapshot := cloneRuntimeRaidState(*state)
		s.raidMu.Unlock()
		if len(lukePulses) != 0 {
			if err := s.broadcastRuntimeRaidMovieSymbolRows(lukeSnapshot, lukePulses); err != nil {
				s.logPacketEvent("game-raid-luke-pulse-symbol-failed",
					"raid_key", raidKey, "cleared_map_id", mapID, "error", err)
			}
		}
		if len(lukeArmed) == 0 {
			return nil
		}
		if err := s.scheduleRuntimeRaidMechanismTimers(raidKey, lukeArmed); err != nil {
			return err
		}
		s.logPacketEvent("game-raid-luke-mechanisms-armed",
			"raid_key", raidKey, "cleared_map_id", mapID,
			"timers", describeRuntimeRaidMechanismTimers(lukeArmed))
		return nil
	}
	var armed []runtimeRaidMechanismTimer
	symbolsBefore := cloneRuntimeRaidMovieSymbols(state)
	if phase == runtimeRaidDungeonPhaseOne {
		armed = applyRuntimeRaidAntonPhaseOneClearLocked(state, mapID, now)
	} else {
		armed = applyRuntimeRaidAntonPhaseTwoClearLocked(state, mapID, now)
	}
	// A gate whose own clear quota is met is DONE, and 86JP ends the dungeon's
	// participation record there (RaidDungeonParticipation.Dispose), which is where
	// its UsedCoinCount/GrantedCoinCount live. So a finished gate hands back a full
	// revive-coin allowance if it is ever reopened -- the regenerated black fog (211)
	// and the respawning legs are exactly that case, and this server re-initializes
	// their objectives with a fresh RemainingHP when they reopen
	// (applyRuntimeRaidAntonPhaseOneClearLocked).
	//
	// `RemainingHP == 0` is our "cleared": it is the value
	// advanceRuntimeRaidObjectiveLocked turns into wire state 3, and it is reached
	// only by the clear that exhausts the quota, so a multi-clear gate such as the
	// black-fog source keeps its spend until its last clear. The reset runs here,
	// inside the lock, so the progress snapshot published right afterwards already
	// carries the restored row (see scheduleRuntimeRaidMonsterStateBroadcast for the
	// spend side of the same ledger).
	reviveCoinsReset := 0
	if objective, ok := state.Lifecycle.Objectives[mapID]; ok &&
		objective.Phase == phase && objective.RemainingHP == 0 {
		reviveCoinsReset = s.clearRuntimeRaidReviveCoinLedgerForGateLocked(raidKey, phase, mapID)
	}
	movieRows := runtimeRaidMovieSymbolChanges(symbolsBefore, state.Lifecycle.MovieSymbols)
	snapshot := cloneRuntimeRaidState(*state)
	s.raidMu.Unlock()
	if reviveCoinsReset != 0 {
		s.logPacketEvent("game-raid-revive-coin-ledger-reset",
			"raid_key", raidKey,
			"raid_phase", phase,
			"raid_map_id", mapID,
			"cleared_entries", reviveCoinsReset,
			"reason", "the_gate_reached_its_clear_quota_so_its_allowance_starts_over")
	}
	if len(movieRows) != 0 {
		// The cutscene edge has to reach the client even when no mechanism timer was
		// armed this clear (the black-fog source is spent exactly once).
		if err := s.broadcastRuntimeRaidMovieSymbolRows(snapshot, movieRows); err != nil {
			s.logPacketEvent("game-raid-anton-movie-symbol-failed",
				"raid_key", raidKey, "cleared_map_id", mapID, "error", err)
		}
	}
	if phase == runtimeRaidDungeonPhaseTwo && mapID == antonBlackVolcanoMapID {
		// The black volcano is down, so the siege is over and every other gate of
		// this phase is gone with it. 86JP's ClearBlackVolcanoAsync invalidates the
		// runs of 218/219/221..224 and hands them to
		// ReturnRaidDungeonGroupsClosedByObjectiveAsync (:1732-1734); a party still
		// inside one of them can no longer do anything the raid will accept, and this
		// server's own phase movie refuses to open while any gate is occupied. Live
		// 2026-09-19, raid 1507338: "一阶段 有队伍在图里 就不结束" was the phase-one
		// half of exactly this.
		//
		// The sweep is scheduled, never run here: this function is called from the
		// completing member's packet goroutine while it holds its own
		// session.dungeon.mu, and the sweep takes every targeted teammate's session
		// lock. 219 itself is one of the gates the volcano closes, so the teammate
		// standing next to the completer is a target -- that is the live
		// 2026-09-20 08:32:02Z deadlock, reproduced by
		// TestRuntimeRaidVolcanoClearNeverWaitsOnAMateSessionLock.
		s.scheduleRuntimeRaidForcedGroupReturn(
			raidKey,
			phase,
			"raid_black_volcano_cleared",
			"game-raid-volcano-cleared-groups-returned",
			func(closed int) []any {
				return []any{
					"raid_key", raidKey,
					"cleared_map_id", mapID,
					"returned_sessions", closed,
					"reason", "the_volcano_fell_and_closed_every_remaining_gate_of_the_phase",
				}
			},
			runtimeRaidPhaseTwoGateAfterVolcano,
		)
	}
	if len(armed) == 0 {
		return nil
	}
	if err := s.scheduleRuntimeRaidMechanismTimers(raidKey, armed); err != nil {
		return err
	}
	s.logPacketEvent("game-raid-anton-mechanisms-armed",
		"raid_key", raidKey, "cleared_map_id", mapID, "timers", describeRuntimeRaidMechanismTimers(armed))
	return nil
}

func (s *Service) scheduleRuntimeRaidMechanismTimers(raidKey uint32, armed []runtimeRaidMechanismTimer) error {
	if len(armed) == 0 {
		return nil
	}
	queue := s.ensureGameplayTimeQueue()
	if queue == nil {
		return errGameplayTimeQueueUnavailable
	}
	var firstErr error
	for _, timer := range armed {
		delay := timer.Deadline.Sub(queue.Now())
		if delay < 0 {
			delay = 0
		}
		armedTimer := timer
		err := queue.ScheduleAfter(runtimeRaidMechanismTimerName(raidKey, armedTimer), delay, func(due time.Time) {
			s.fireRuntimeRaidMechanismTimer(raidKey, armedTimer, due)
		})
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// fireRuntimeRaidMechanismTimer is the one-shot callback. Only the exact
// generation inside the current cycle of an active phase one may act; a clear,
// a reset, a phase change or a disband all leave the callback stale.
func (s *Service) fireRuntimeRaidMechanismTimer(raidKey uint32, armed runtimeRaidMechanismTimer, due time.Time) {
	due = runtimeRaidNormalizeTime(due)
	s.raidMu.Lock()
	state := s.raids[raidKey]
	if state == nil {
		s.raidMu.Unlock()
		return
	}
	current, ok := state.Lifecycle.Mechanisms[armed.key()]
	if !ok || current.Generation != armed.Generation || current.Cycle != state.Lifecycle.PhaseCycle ||
		state.Lifecycle.Status != runtimeRaidLifecyclePhaseActive ||
		(state.Lifecycle.Phase != runtimeRaidDungeonPhaseOne && state.Lifecycle.Phase != runtimeRaidDungeonPhaseTwo) {
		s.raidMu.Unlock()
		return
	}
	delete(state.Lifecycle.Mechanisms, armed.key())
	var armedNext []runtimeRaidMechanismTimer
	var barrierChange *runtimeRaidBarrierChange
	outcome := ""
	fogRegenerated := false
	cycleReset := false
	symbolsBefore := cloneRuntimeRaidMovieSymbols(state)
	if state.Lifecycle.Phase == runtimeRaidDungeonPhaseTwo {
		armedNext, outcome, barrierChange = s.fireRuntimeRaidPhaseTwoMechanismLocked(state, armed, due)
	} else {
		switch armed.Kind {
		case runtimeRaidMechanismActive:
			if runtimeRaidResetAntonPhaseOneLocked(state) {
				outcome = "phase_one_reset"
				cycleReset = true
			}
		case runtimeRaidMechanismRecovery, runtimeRaidMechanismNavalOpen:
			if runtimeRaidReopenAntonDungeonLocked(state, armed.MapID) {
				armedNext = append(armedNext,
					state.armRuntimeRaidMechanismLocked(runtimeRaidMechanismActive, armed.MapID, due, armed.ActiveAfter, 0))
				outcome = "dungeon_reopened"
				fogRegenerated = armed.MapID == antonBlackFogMapID
			}
		}
	}
	movieRows := runtimeRaidMovieSymbolChanges(symbolsBefore, state.Lifecycle.MovieSymbols)
	if barrierChange != nil && (barrierChange.Broke || barrierChange.Restored) {
		// broadcastRuntimeRaidBarrier already carries 126/127 together with the
		// shield and volcano rows, exactly like the 86 reference's atomic edge set.
		movieRows = filterRuntimeRaidMovieSymbolRows(movieRows, antonSymbolBarrierBreak, antonSymbolBarrierRecover)
	}
	snapshot := cloneRuntimeRaidState(*state)
	s.raidMu.Unlock()
	if fogRegenerated || cycleReset {
		// The gates that just closed must not keep a squad inside them: the regenerated
		// fog closes the quakes/legs/cannon, and a mechanism-failure reset closes those
		// plus the fog itself (its own countdown and the quakes' active windows share the
		// same 300s deadline, so in practice the reset is the edge the operator sees).
		// Scheduled, never run here: the sweep takes every targeted session's lock.
		sweep := runtimeRaidPhaseOneGateClosedByFogRegeneration
		logKind := "game-raid-fog-regenerated-groups-returned"
		reason := "the_regenerated_black_fog_closed_the_quake_leg_and_cannon_gates"
		source := "raid_phase_one_fog_regenerated"
		if cycleReset {
			sweep = runtimeRaidPhaseOneGateClosedByCycleReset
			logKind = "game-raid-phase-one-reset-groups-returned"
			reason = "the_mechanism_cycle_reset_closed_every_mechanism_gate_of_phase_one"
			source = "raid_phase_one_cycle_reset"
		}
		s.scheduleRuntimeRaidForcedGroupReturn(
			raidKey,
			runtimeRaidDungeonPhaseOne,
			source,
			logKind,
			func(returned int) []any {
				return []any{
					"raid_key", raidKey,
					"returned_sessions", returned,
					"reason", reason,
				}
			},
			sweep,
		)
	}
	if outcome == "" {
		return
	}
	if len(movieRows) != 0 {
		if err := s.broadcastRuntimeRaidMovieSymbolRows(snapshot, movieRows); err != nil {
			s.logPacketEvent("game-raid-anton-movie-symbol-failed",
				"raid_key", raidKey, "map_id", armed.MapID, "error", err)
		}
	}
	if err := s.scheduleRuntimeRaidMechanismTimers(raidKey, armedNext); err != nil {
		s.logPacketEvent("game-raid-anton-mechanism-timer-failed",
			"raid_key", raidKey, "map_id", armed.MapID, "error", err)
	}
	if barrierChange != nil {
		// A recharge tick is a one-row symbol update, not a full map replay.
		if err := s.broadcastRuntimeRaidBarrier(snapshot, *barrierChange); err != nil {
			s.logPacketEvent("game-raid-anton-barrier-broadcast-failed",
				"raid_key", raidKey, "error", err)
		}
		if barrierChange.Restored {
			// The shield is back to full, so the volcano is sealed again: 86JP detaches
			// every participation of 219 and sends those groups home
			// (RaidManager.cs:1664-1673, Progression.cs:1993-1998). Without it a party
			// that walked into the volcano while the shield was broken can simply wait
			// out the recharge and kill the heart, skipping the whole siege.
			if returned := s.forceReturnRuntimeRaidGroupsMatching(
				raidKey, runtimeRaidDungeonPhaseTwo, "raid_barrier_restored",
				func(mapID int) bool { return mapID == antonBlackVolcanoMapID },
			); returned > 0 {
				s.logPacketEvent("game-raid-barrier-restored-groups-returned",
					"raid_key", raidKey,
					"map_id", antonBlackVolcanoMapID,
					"returned_sessions", returned,
					"reason", "the_shield_recovered_and_sealed_the_volcano_again")
			}
		}
		if !barrierChange.Restored {
			return
		}
	}
	if err := s.broadcastRuntimeRaidPhaseSnapshot(snapshot, false, due); err != nil {
		s.logPacketEvent("game-raid-anton-mechanism-snapshot-failed",
			"raid_key", raidKey, "map_id", armed.MapID, "error", err)
	}
	s.logPacketEvent("game-raid-anton-mechanism-fired",
		"raid_key", raidKey,
		"kind", armed.Kind,
		"map_id", armed.MapID,
		"outcome", outcome,
		"phase_cycle", snapshot.Lifecycle.PhaseCycle,
		"timers", describeRuntimeRaidMechanismTimers(armedNext))
}

// runtimeRaidMechanismTimerNotifications projects the armed timers in the
// current EXE 0x0254 layout so both the live snapshot and a reconnect show the
// same countdowns.
func runtimeRaidMechanismTimerNotifications(state runtimeRaidState, now time.Time) []runtimeRaidWireNotification {
	if len(state.Lifecycle.Mechanisms) == 0 {
		return nil
	}
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
	notifications := make([]runtimeRaidWireNotification, 0, len(timers))
	for _, timer := range timers {
		duration := timer.Deadline.Sub(timer.StartedAt)
		if duration < 0 {
			duration = 0
		}
		notifications = append(notifications, runtimeRaidWireNotification{
			msgID: raid.RaidTimerMsgID,
			body: raid.BuildRaidTimerBody(raid.RaidTimer{
				TimerType: timer.Kind,
				Value0:    uint32(timer.MapID),
				Value1:    uint32(timer.Deadline.Unix()),
				Value2:    runtimeRaidDurationSeconds(duration),
			}),
		})
	}
	return notifications
}

func describeRuntimeRaidMechanismTimers(timers []runtimeRaidMechanismTimer) string {
	parts := make([]string, 0, len(timers))
	for _, timer := range timers {
		parts = append(parts, fmt.Sprintf("%d/%d@%s", timer.Kind, timer.MapID, timer.Deadline.UTC().Format(time.RFC3339)))
	}
	return fmt.Sprintf("%v", parts)
}
