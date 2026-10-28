package dnfbridge

import (
	"context"
	"fmt"
	"math"
	"sort"
	"time"

	"longheng.io/server/internal/modules/dnf/dnfenum"
	"longheng.io/server/internal/modules/dnf/raid"
)

const (
	runtimeRaidPrepareState    byte   = 1
	runtimeRaidPhaseStartState byte   = 2
	runtimeRaidTopTimerKey     uint32 = 0
	// runtimeRaidFailedResultWindow is how long a failed raid keeps its terminal
	// object. 86JP: "Preserve the terminal result long enough for the client result
	// UI and reconnect snapshot, then release the roster exactly once"
	// (RemoveFailedRaidAfterResultDelayAsync awaits Task.Delay(10000)).
	runtimeRaidFailedResultWindow = 10 * time.Second
)

func (s *Service) publishRuntimeRaidReady(state runtimeRaidState) error {
	if state.Lifecycle.Status != runtimeRaidLifecyclePreparing || state.Lifecycle.ReadyDeadline.IsZero() {
		return errRuntimeRaidLifecycleState
	}
	// 超时空：待命阶段额外推一帧 1453{1,0}。原生端在开战前用这一帧把战况表从
	// "准备中"切到"已就绪"，缺了它表会一直停在准备态。
	if state.RaidMode == runtimeRaidOwnerFiendWar {
		if err := s.broadcastRuntimeRaidWireSequence(state, runtimeRaidWireNotification{msgID: 1453, body: []byte{1, 0}}); err != nil {
			return err
		}
	}
	duration := state.Lifecycle.ReadyDeadline.Sub(state.Lifecycle.StartedAt)
	if duration < 0 {
		duration = 0
	}
	ready := raid.RaidTimer{
		TimerType: 0,
		Value0:    runtimeRaidTopTimerKey,
		Value1:    uint32(state.Lifecycle.ReadyDeadline.Unix()),
		Value2:    runtimeRaidDurationSeconds(duration),
	}
	if err := s.broadcastRuntimeRaidWireSequence(state,
		// Current EXE sub_1D50040 records the preparation lifecycle first;
		// op596/op597 then stage the deadline and remaining seconds. The
		// bodyless op594 handler sub_1D67770 is the final materialization edge:
		// it calls sub_1C5DE30 and constructs the native visible 3,2,1 panel
		// from the already-populated timer state.
		runtimeRaidWireNotification{
			msgID: raid.RaidStateMsgID,
			body: raid.BuildRaidStateBody(raid.RaidState{
				State:    runtimeRaidPrepareState,
				Argument: int8(state.Lifecycle.Phase - 1),
			}),
		},
		runtimeRaidWireNotification{msgID: raid.RaidTimerMsgID, body: raid.BuildRaidTimerBody(ready)},
		runtimeRaidWireNotification{
			msgID: raid.RaidRemainTimeMsgID,
			body: raid.BuildRaidRemainTimeBody(raid.RaidRemainTime{
				TimerType: 0,
				Seconds:   ready.Value2,
			}),
		},
		runtimeRaidWireNotification{msgID: raid.RaidPrepareStartMsgID},
	); err != nil {
		return err
	}
	return s.scheduleRuntimeRaidReady(state)
}

func (s *Service) scheduleRuntimeRaidReady(state runtimeRaidState) error {
	queue := s.ensureGameplayTimeQueue()
	if queue == nil {
		return errGameplayTimeQueueUnavailable
	}
	delay := state.Lifecycle.ReadyDeadline.Sub(queue.Now())
	if delay < 0 {
		delay = 0
	}
	name := fmt.Sprintf("raid-ready:%d:%d", state.RaidKey, state.StartAttempt)
	return queue.ScheduleAfter(name, delay, func(due time.Time) {
		// 走 activateRuntimeRaidReadyPhase：没有调试标记文件时它与原来那一行
		// （直接 activateRuntimeRaidPhase(..., phaseOne, due, false)）逐字同义，
		// 只是多了一层 runtime/state/raid-skip-* 标记检查。见 raid_debug_phase_skip.go。
		if err := s.activateRuntimeRaidReadyPhase(state.RaidKey, due); err != nil {
			s.logPacketEvent("game-raid-ready-activation-failed", "raid_key", state.RaidKey, "error", err)
		}
	})
}

func (s *Service) activateRuntimeRaidPhase(raidKey uint32, phase byte, now time.Time, allowEarly bool) (runtimeRaidState, error) {
	now = runtimeRaidNormalizeTime(now)
	s.raidMu.Lock()
	state := s.raids[raidKey]
	if state == nil {
		s.raidMu.Unlock()
		return runtimeRaidState{}, errRuntimeRaidNotFound
	}
	config := runtimeRaidBaseConfig(*state)
	if config == nil {
		s.raidMu.Unlock()
		return runtimeRaidState{}, errRuntimeRaidLifecycleState
	}
	if state.Lifecycle.Status == runtimeRaidLifecyclePhaseActive && state.Lifecycle.Phase == phase {
		active := cloneRuntimeRaidState(*state)
		s.raidMu.Unlock()
		return s.publishRuntimeRaidPhaseActivation(active, now)
	}
	if state.TailFlag == 1 && phase != runtimeRaidDungeonPhaseOne {
		s.raidMu.Unlock()
		return runtimeRaidState{}, errRuntimeRaidPhaseMismatch
	}
	var phaseTwoArmed []runtimeRaidMechanismTimer
	switch phase {
	case runtimeRaidDungeonPhaseOne:
		if state.Lifecycle.Status != runtimeRaidLifecyclePreparing || state.Lifecycle.Phase != phase ||
			(!allowEarly && now.Before(state.Lifecycle.ReadyDeadline)) {
			s.raidMu.Unlock()
			return runtimeRaidState{}, errRuntimeRaidLifecycleState
		}
		// 卢克一阶段：把一阶段的门状态与符号初始化成 luke.etc 的首态。
		// 安徒恩状态（owner != Luke）与单人模式（TailFlag == 1）都不进这里。
		if state.TailFlag != 1 && runtimeRaidTeamGraphOwner(*state) == runtimeRaidOwnerLuke {
			initRuntimeRaidLukePhaseOneLocked(state)
		}
	case runtimeRaidDungeonPhaseTwo:
		if state.Lifecycle.Status != runtimeRaidLifecycleStandby || state.Lifecycle.Phase != runtimeRaidDungeonPhaseOne ||
			(!allowEarly && now.Before(state.Lifecycle.StandbyDeadline)) {
			s.raidMu.Unlock()
			return runtimeRaidState{}, errRuntimeRaidLifecycleState
		}
		if err := initializeRuntimeRaidPhaseObjectives(&state.Lifecycle, config, phase); err != nil {
			s.raidMu.Unlock()
			return runtimeRaidState{}, err
		}
		switch {
		case runtimeRaidUsesAntonTeamGraph(*state):
			// Anton's siege state only. Seeding it for a Luke raid filled in a
			// full black-volcano shield and armed a hatchery-open timer on
			// Anton's map 219, none of which exists in Luke's phase two.
			// 安徒恩语义与改动前逐字一致：原来的条件是 state.TailFlag != 1，
			// 而 runtimeRaidUsesAntonTeamGraph = TailFlag != 1 && owner == Anton，
			// 对安徒恩与单人模式求值结果完全相同。
			initRuntimeRaidPhaseTwoLocked(state, now)
		case state.TailFlag != 1 && runtimeRaidTeamGraphOwner(*state) == runtimeRaidOwnerLuke:
			phaseTwoArmed = initRuntimeRaidLukePhaseTwoLocked(state, now)
		}
	default:
		s.raidMu.Unlock()
		return runtimeRaidState{}, errRuntimeRaidPhaseMismatch
	}
	_, timeoutSeconds, ok := runtimeRaidPhaseConfig(*state, phase)
	if !ok || timeoutSeconds <= 0 {
		s.raidMu.Unlock()
		return runtimeRaidState{}, errRuntimeRaidPhaseMismatch
	}
	state.Lifecycle.Status = runtimeRaidLifecyclePhaseActive
	// 超时空：阶段激活是战场的第一次求值 —— 空事件会跑一遍 PVF 规则对，把
	// fiendwar init 立起来、把 SET TIMER 那批计时器全部武装(敌军游走、雷达、
	// 电池、商人、区域恢复)。syncRuntimeFiendWarObjectives 立刻把战场的区域状态
	// 翻译成战况表的行。安东/卢克没有战场，这段不执行。
	if state.Lifecycle.FiendWar != nil {
		if err := state.Lifecycle.FiendWar.Apply(raid.FiendWarEvent{}, now); err != nil {
			s.raidMu.Unlock()
			return runtimeRaidState{}, err
		}
		syncRuntimeFiendWarObjectives(state)
	}
	state.Lifecycle.Phase = phase
	// 新阶段的复活币计数归零（只有声明了 [PHASE COIN LIMIT] 的阶段用得上：
	// 卢克二阶段 20 次，全队共享，重新进图不重置）。见 raid_phase_coin.go。
	state.Lifecycle.PhaseCoinsUsed = 0
	state.Lifecycle.ReadyDeadline = time.Time{}
	state.Lifecycle.StandbyStartedAt = time.Time{}
	state.Lifecycle.StandbyDeadline = time.Time{}
	state.Lifecycle.PhaseStartedAt = now
	state.Lifecycle.PhaseDeadline = now.Add(time.Duration(timeoutSeconds) * time.Second)
	state.Lifecycle.MovieOpenedAt = time.Time{}
	clear(state.Lifecycle.MovieConfirmed)
	// A new phase starts with every member's clear registration at zero (86JP
	// RaidManager clears all personal counters when the next phase begins), and
	// with every gate's one-clear-per-run ledger gone: the next phase's runs are
	// new party clears even on a map id both phases carry.
	clear(state.Lifecycle.PhaseClearCounts)
	clear(state.Lifecycle.RunClearCredits)
	state.Lifecycle.RewardToken = 0
	state.Lifecycle.RewardOpenedAt = time.Time{}
	clear(state.Lifecycle.RewardSelections)
	// A new phase re-decides who is skipped: presence is read again from the
	// sockets that exist now, not from the previous phase's settlement.
	state.Lifecycle.RewardSkippedMembers = make(map[uint16]bool)
	state.Lifecycle.RewardRosterFrozen = false
	state.Lifecycle.RewardDelivery = nil
	state.Lifecycle.RewardDeliveryCommitted = false
	state.Lifecycle.RewardDeliveryCommittedAt = time.Time{}
	state.Lifecycle.Revision++
	active := cloneRuntimeRaidState(*state)
	s.raidMu.Unlock()

	// 卢克二阶段激活时上弦的机制计时器（能源控制室 45/90/120 秒、储藏室 150 秒等）。
	// 安徒恩这条 phaseTwoArmed 恒为 nil，与改动前完全不做事一致。
	if err := s.scheduleRuntimeRaidMechanismTimers(raidKey, phaseTwoArmed); err != nil {
		s.logPacketEvent("game-raid-luke-phase-two-timer-failed", "raid_key", raidKey, "error", err)
	}

	return s.publishRuntimeRaidPhaseActivation(active, now)
}

func (s *Service) publishRuntimeRaidPhaseActivation(state runtimeRaidState, now time.Time) (runtimeRaidState, error) {
	// The current client creates the attack-party panel from the mode=0 raid
	// object.  That object is sent when the party is created, but the client
	// tears down the selection view while the ready timer is running.  Re-send
	// the complete object and its buff state at phase activation so the native
	// panel is materialized before state=2 and the map snapshot arrive.
	if err := s.broadcastRuntimeRaidCreate(state); err != nil {
		return runtimeRaidState{}, err
	}
	// Do NOT push the 0x024F mode3 attack-party roster from here. It looked like
	// the fix for "战况表按钮不出现" (the clear-registration path sends mode0+mode3,
	// and entering the dungeon-select view once made the panel appear), but the
	// client's mode3 branch builds a fresh 0x98-byte roster object and inserts it
	// into the raid manager, so every extra send left another stale row behind:
	// live 2026-09-17T07:5xZ the board showed two identical squad rows for one
	// occupied squad and never cleared the previous one ("进一次出现一次，之前的不清空").
	// The panel entry needs a non-duplicating trigger instead.
	if err := s.broadcastRuntimeRaidPhaseSnapshot(state, true, now); err != nil {
		return runtimeRaidState{}, err
	}
	if state.TailFlag != 1 {
		// A new phase is exactly where the two squads stop moving together: one
		// may already be inside its gate while the other is still in the waiting
		// area. Re-asserting the squad parties here keeps the party HUD, the
		// 战况�?grid and the standby room resolving the same roster. A single
		// raid owns no squad, so its trigger points stay where they were.
		if err := s.applyRuntimeRaidParties(state); err != nil {
			s.logGameEvent(nil, "game-raid-squad-party-projection-failed",
				"raid_key", state.RaidKey,
				"stage", "phase_activation",
				"error", err)
		}
		// Activation is the other moment the two squads are guaranteed to be in
		// different scenes, so every pair of members needs the other's actor record
		// before any raid window is drawn.
		s.syncRuntimeRaidMateActorsAll(state, "raid_phase_activation_raid_mate_actors")
		// This sweep is the measured source of the phase-start freeze (live
		// 2026-09-29: all eight loops ~1.0s with mode1, ~0.37s without), so the
		// per-raid cost summary is emitted right here, covering the phase that just
		// ended -- see raid_cost_summary.go. It is a synchronous one-line record and
		// deliberately schedules no timer (the scheduling below is pinned by tests).
		s.publishRuntimeRaidCostSummary(state.RaidKey, "phase_activation_previous_phase_window")
	}
	if err := s.scheduleRuntimeRaidPhaseExpiry(state); err != nil {
		return runtimeRaidState{}, err
	}
	// Mechanism timers armed during activation (phase two arms the hatchery
	// opening) are queued here, outside the raid lock.
	if err := s.scheduleRuntimeRaidMechanismTimers(state.RaidKey, sortedRuntimeRaidMechanismTimers(state)); err != nil {
		return runtimeRaidState{}, err
	}
	// 超时空的战场 tick：每秒一次，让敌军在地图上移动、计时器到点、商人出现。
	// 它是自续期的(每次 tick 结束时重新排下一次)，所以在攻坚进行期间一直跑。
	if state.RaidMode == runtimeRaidOwnerFiendWar {
		if err := s.scheduleRuntimeFiendWarTick(state); err != nil {
			return runtimeRaidState{}, err
		}
	}
	return state, nil
}

func (s *Service) broadcastRuntimeRaidPhaseSnapshot(state runtimeRaidState, announce bool, now time.Time) error {
	// 超时空的战况表是它自己的一族包 (1452..1460)，与安徒恩/卢克的 0x0247/0x0249
	// 完全不同构，所以整条快照路径在入口处分流。
	if state.RaidMode == runtimeRaidOwnerFiendWar {
		return s.broadcastRuntimeFiendWarBoard(state, announce, now)
	}
	symbolBody, dungeonBody, dungeonRows, err := runtimeRaidPhaseSnapshotBodies(state)
	if err != nil {
		return err
	}
	seconds := runtimeRaidDurationSeconds(runtimeRaidPhaseSnapshotRemaining(state, now))
	phaseDuration := state.Lifecycle.PhaseDeadline.Sub(state.Lifecycle.PhaseStartedAt)
	if phaseDuration < 0 {
		phaseDuration = 0
	}
	// The trailer of the 0x0249 this snapshot is about to send, in the one form the
	// client resolves the infection from (see runtimeRaidInfectionMapIDForWire).
	infection := runtimeRaidInfectionMapIDForWire(state)
	notifications := make([]runtimeRaidWireNotification, 0, 7)
	// The 0x0249 dungeon-state table goes out in full exactly once per phase, at its
	// activation. Every row carries its gate's wire state and a CLEARED gate keeps
	// reading 3 for the rest of the phase, so re-sending the table re-announces
	// finished dungeons to the client -- which answers each cleared row with the
	// 「已通关[%s]地下城」 toast (string 646). Measured on the live server 2026-09-19
	// 14:23-14:46Z, raid 1507338: 618 sends of 0x0249 in 23 minutes, re-announcing
	// 第一孵化中心 (221) 104 times, 第二孵化中心 (222) 52 times and 第四孵化中心 (224)
	// 196 times -- the operator's "每个阶段只要是通关过的就会一直弹". After activation only
	// the rows that actually moved travel, each as a one-row 0x024A change, which is
	// what 86JP does (Progression.cs:681-685 for the table; Runtime.cs:369-403 for
	// every later change). Symbol rows travel with the same snapshot and are
	// unchanged by this: a client that opens the selector, rejoins or re-activates
	// the phase still receives the complete table from sendRuntimeRaidSelectorSnapshot
	// / the rejoin admission, and the change rows are absolute, not deltas.
	notifications = append(notifications,
		runtimeRaidWireNotification{msgID: raid.RaidSymbolMsgID, body: symbolBody},
	)
	tableNotification := runtimeRaidWireNotification{msgID: raid.RaidDungeonStateMsgID, body: dungeonBody}
	if announce {
		s.recordRuntimeRaidDungeonStateRows(state.RaidKey, state.Lifecycle.Phase, dungeonRows, infection)
		notifications = append(notifications, tableNotification)
	} else if changed, fullTable, infectionChanged := s.claimRuntimeRaidDungeonStateRows(state.RaidKey, state.Lifecycle.Phase, dungeonRows, infection); fullTable {
		notifications = append(notifications, tableNotification)
	} else {
		for _, row := range changed {
			notifications = append(notifications, runtimeRaidWireNotification{
				msgID: raid.RaidDungeonChangeMsgID,
				body: raid.BuildRaidDungeonChangeBody(raid.RaidDungeonChange{
					DungeonID: row.DungeonID,
					State:     byte(row.State),
					Argument:  byte(row.State),
				}),
			})
		}
		if infectionChanged {
			// The infection moved without the whole table going out, so ship the
			// trailer on its own: a zero-row 0x0249 is legal for this client
			// (sub_1D39B50 loops `count` times and always reads the dword after the
			// rows), it inserts and overwrites no gate, and it therefore cannot
			// re-announce a cleared gate the way a whole table would. It follows the
			// 0x0247 above in the same sequence, which matters for symbol 7: the
			// trailer only ever SETS it (cmp [esi+0x150],0 / je), so the symbol table
			// is what clears it on the suppression edge.
			tailOnly, err := raid.BuildRaidDungeonStateBody(raid.RaidDungeonStateSnapshot{TrailingValue: infection})
			if err != nil {
				return err
			}
			notifications = append(notifications, runtimeRaidWireNotification{
				msgID: raid.RaidDungeonStateMsgID,
				body:  tailOnly,
			})
		}
	}
	if announce {
		// Current EXE sub_F176A0 initializes the Anton selector while processing
		// op587 state=2, and sub_F163C0 reads every dungeon state during that
		// initialization. op585's observer does not request a full redraw, so the
		// symbols and rows must already exist before state=2 materializes the view.
		notifications = append(notifications, runtimeRaidWireNotification{
			msgID: raid.RaidStateMsgID,
			body: raid.BuildRaidStateBody(raid.RaidState{
				State:    runtimeRaidPhaseStartState,
				Argument: int8(state.Lifecycle.Phase - 1),
			}),
		})
		singleBootstrap, err := runtimeRaidSinglePhaseBootstrapNotifications(state)
		if err != nil {
			return err
		}
		notifications = append(notifications, singleBootstrap...)
	}
	notifications = append(notifications,
		runtimeRaidWireNotification{
			msgID: raid.RaidTimerMsgID,
			body: raid.BuildRaidTimerBody(raid.RaidTimer{
				TimerType: 0,
				Value0:    runtimeRaidTopTimerKey,
				Value1:    uint32(state.Lifecycle.PhaseDeadline.Unix()),
				Value2:    runtimeRaidDurationSeconds(phaseDuration),
			}),
		},
		runtimeRaidWireNotification{
			msgID: raid.RaidRemainTimeMsgID,
			body:  raid.BuildRaidRemainTimeBody(raid.RaidRemainTime{TimerType: 0, Seconds: seconds}),
		},
	)
	notifications = append(notifications, runtimeRaidMechanismTimerNotifications(state, now)...)
	// op775：小队已用复活币数。有复活币总额的小队，客户端会把它当"已用"读，所以它
	// 跟着每次快照走 —— 激活时清零，重连与后进队的人靠它追上（raid_phase_coin.go）。
	//
	// ⚠️ 上游对「所有非单人模式」都发这一条；本树只对卢克发：我们这条线自己有一套
	// 安徒恩复活币账本（clearRuntimeRaidReviveCoinLedgerForGateLocked 等），
	// 给安徒恩快照再加一条 op775 会改动那条已经验收过的路径。卢克二阶段才声明
	// [PHASE COIN LIMIT]，所以只有它需要这一格。
	if state.TailFlag != 1 && runtimeRaidTeamGraphOwner(state) == runtimeRaidOwnerLuke {
		notifications = append(notifications, runtimeRaidSquadCoinUsedNotification(state))
	}
	// 卢克二阶段一旦有成员被路线锁绑住，0x0249 表就得按成员重建：被绑到另一条路线的
	// 成员看那条路线的门应当画成 close（见 runtimeRaidViewerObjectiveWireState）。
	// 安徒恩与单人模式不进这里，走下面原路。
	if state.TailFlag != 1 && runtimeRaidTeamGraphOwner(state) == runtimeRaidOwnerLuke &&
		len(state.Lifecycle.LukeRouteLocks) != 0 {
		return s.broadcastRuntimeRaidWireSequencePerViewer(state, notifications,
			func(viewer uint16) (runtimeRaidWireNotification, bool) {
				viewerDungeonBody, buildErr := runtimeRaidPhaseSnapshotViewerDungeonBody(state, viewer)
				if buildErr != nil {
					s.logPacketEvent("game-raid-luke-viewer-snapshot-failed",
						"raid_key", state.RaidKey, "viewer", viewer, "error", buildErr)
					return runtimeRaidWireNotification{}, false
				}
				return runtimeRaidWireNotification{msgID: raid.RaidDungeonStateMsgID, body: viewerDungeonBody}, true
			})
	}
	return s.broadcastRuntimeRaidWireSequence(state, notifications...)
}

// claimRuntimeRaidDungeonStateRows records the (gate -> wire state) table this raid
// is about to put on the wire and reports which rows actually moved since the last
// broadcast of the same phase. Callers send the whole table only when fullTable is
// true; otherwise they send the returned rows as one-row changes, so a snapshot
// that changes one gate no longer re-announces every gate the phase already
// finished. It returns fullTable=true when the caller has no row breakdown (the
// single-mode table) or when this is the first broadcast of the phase.
//
// The infected-hatchery trailer is claimed with the rows even though it is not a
// row: 0x024A carries no trailer, so an infection edge that moved no gate (a
// suppression or a recovery whose row was about to move anyway) would otherwise
// never reach the client. When it moved, the caller ships it as a zero-row 0x0249
// -- the whole table would re-announce every cleared gate. infectionChanged is
// always false when fullTable is true, because that table already carries the
// trailer.
func (s *Service) claimRuntimeRaidDungeonStateRows(
	raidKey uint32,
	phase byte,
	rows []raid.RaidDungeonStateRow,
	infection uint32,
) ([]raid.RaidDungeonStateRow, bool, bool) {
	if s == nil || raidKey == 0 || len(rows) == 0 {
		return nil, true, false
	}
	s.raidMu.Lock()
	defer s.raidMu.Unlock()
	state := s.raids[raidKey]
	if state == nil {
		return nil, true, false
	}
	previous := state.Lifecycle.DungeonStates
	samePhase := state.Lifecycle.DungeonStatePhase == phase && len(previous) > 0
	changed := make([]raid.RaidDungeonStateRow, 0, len(rows))
	next := make(map[uint32]uint32, len(rows))
	for _, row := range rows {
		next[row.DungeonID] = row.State
		if !samePhase || previous[row.DungeonID] != row.State {
			changed = append(changed, row)
		}
	}
	infectionChanged := state.Lifecycle.DungeonStateInfection != infection
	state.Lifecycle.DungeonStatePhase = phase
	state.Lifecycle.DungeonStates = next
	state.Lifecycle.DungeonStateInfection = infection
	if !samePhase {
		// The first table of a phase is always complete, however it was reached:
		// a caller that only sent the changed rows here would leave every gate the
		// client had not seen yet at its stale value.
		return nil, true, false
	}
	return changed, false, infectionChanged
}

// recordRuntimeRaidDungeonStateRows stores the table a full-table broadcast has just
// delivered, so the next snapshot can send only the rows that move. Callers that
// broadcast to every member use this; a per-session replay
// (sendRuntimeRaidSelectorSnapshot) deliberately does not, because the other members
// did not receive it.
func (s *Service) recordRuntimeRaidDungeonStateRows(raidKey uint32, phase byte, rows []raid.RaidDungeonStateRow, infection uint32) {
	if s == nil || raidKey == 0 || len(rows) == 0 {
		return
	}
	next := make(map[uint32]uint32, len(rows))
	for _, row := range rows {
		next[row.DungeonID] = row.State
	}
	s.raidMu.Lock()
	defer s.raidMu.Unlock()
	if state := s.raids[raidKey]; state != nil {
		state.Lifecycle.DungeonStatePhase = phase
		state.Lifecycle.DungeonStates = next
		state.Lifecycle.DungeonStateInfection = infection
	}
}

// runtimeRaidSinglePhaseBootstrapNotifications refreshes the current-client
// single-mode start node after the complete 0x0249 phase snapshot and state=2
// activation have been delivered. Participation op584 is deliberately absent:
// its op=1 row means that the member has entered the dungeon and is owned by the
// first real FINISH_LOADING notification, never phase activation.
func runtimeRaidSinglePhaseBootstrapNotifications(state runtimeRaidState) ([]runtimeRaidWireNotification, error) {
	if state.TailFlag != 1 || state.Lifecycle.Phase != runtimeRaidDungeonPhaseOne {
		return nil, nil
	}

	openObjectives := make([]runtimeRaidDungeonObjective, 0, 1)
	for _, objective := range state.Lifecycle.Objectives {
		if objective.Phase == state.Lifecycle.Phase && objective.State == raid.AntonDungeonOpen {
			openObjectives = append(openObjectives, objective)
		}
	}
	if len(openObjectives) != 1 {
		return nil, errRuntimeRaidPhaseMismatch
	}
	sort.Slice(openObjectives, func(i, j int) bool { return openObjectives[i].MapID < openObjectives[j].MapID })

	objective := openObjectives[0]
	clientDungeonID, ok := runtimeRaidSingleSelectorMapIDForState(state, objective.MapID)
	if !ok {
		return nil, errRuntimeRaidPhaseMismatch
	}
	stateValue := runtimeRaidObjectiveWireState(objective.State)
	return []runtimeRaidWireNotification{
		{
			msgID: raid.RaidDungeonChangeMsgID,
			body: raid.BuildRaidDungeonChangeBody(raid.RaidDungeonChange{
				DungeonID: uint32(clientDungeonID),
				State:     byte(stateValue),
				Argument:  byte(stateValue),
			}),
		},
	}, nil
}

func runtimeRaidPhaseSnapshotRemaining(state runtimeRaidState, now time.Time) time.Duration {
	startedAt := state.Lifecycle.PhaseStartedAt
	deadline := state.Lifecycle.PhaseDeadline
	if startedAt.IsZero() || deadline.IsZero() || !deadline.After(startedAt) {
		return 0
	}
	duration := deadline.Sub(startedAt)
	elapsed := runtimeRaidPhaseElapsed(state, now)
	if elapsed >= duration {
		return 0
	}
	return duration - elapsed
}

// runtimeRaidPhaseSnapshotBodies builds the symbol table and the 0x0249
// dungeon-state table of one phase snapshot, plus the row breakdown of that table
// when the caller can diff it. The single-mode table (TailFlag 1) carries a native
// alias row per gate and therefore reports no breakdown: its caller always sends the
// whole table.
func runtimeRaidPhaseSnapshotBodies(state runtimeRaidState) ([]byte, []byte, []raid.RaidDungeonStateRow, error) {
	// viewer=0 表示「共享视图」：不做任何按成员改画，行为与改动前逐字一致。
	return runtimeRaidPhaseSnapshotBodiesForViewer(state, 0)
}

// runtimeRaidPhaseSnapshotBodiesForViewer 是 runtimeRaidPhaseSnapshotBodies 的
// 「按某一名成员所见」版本。
//
// 只有 0x0249/0x024A 每行的状态会变：卢克二阶段的路线锁要按成员可见 —— 被绑到另一条
// 路线的成员，看那条路线的门应当画成 close，否则界面把它画成 open、玩家点了却被拒，
// 屏幕上什么解释都没有。权威 objective 的状态不变，真正拒绝入场仍然是
// validateAntonRaidDungeonEntry。viewer=0 或非队伍卢克时逐字走原路径。
func runtimeRaidPhaseSnapshotBodiesForViewer(state runtimeRaidState, viewer uint16) ([]byte, []byte, []raid.RaidDungeonStateRow, error) {
	if state.TailFlag == 1 {
		symbolBody, dungeonBody, err := runtimeRaidSingleSelectorSnapshotBodies(state)
		return symbolBody, dungeonBody, nil, err
	}
	objectives := make([]runtimeRaidDungeonObjective, 0, len(state.Lifecycle.Objectives))
	for _, objective := range state.Lifecycle.Objectives {
		if objective.Phase == state.Lifecycle.Phase {
			objectives = append(objectives, objective)
		}
	}
	sort.Slice(objectives, func(i, j int) bool { return objectives[i].MapID < objectives[j].MapID })
	symbols := make([]raid.RaidSymbolRow, 0, len(objectives))
	dungeons := make([]raid.RaidDungeonStateRow, 0, len(objectives))
	for _, objective := range objectives {
		// 副本行先出，再查符号：选图页与战况表是按副本行构建的，所以一个门不管
		// 符号表认不认识它的通关计数键，都必须出现在那里。上游把两者绑在一起，
		// 结果把每个卢克队伍攻坚都清空了 —— 它所有关卡都没有符号项，循环跳过它们，
		// op585 发出 0 行。安徒恩每个门都有符号项，所以行为不变。
		dungeons = append(dungeons, raid.RaidDungeonStateRow{
			DungeonID: uint32(objective.MapID),
			State:     runtimeRaidViewerObjectiveWireState(state, viewer, objective),
		})
		key, ok := runtimeRaidObjectiveSymbolKey(objective.MapID)
		if !ok {
			// A gate with no symbol row must be skipped, never fatal: this error
			// travels back through publishRuntimeRaidPhaseActivation after
			// activateRuntimeRaidPhase has already committed Status=PhaseActive and
			// the new PhaseDeadline, so returning it leaves the raid in phase two
			// with no snapshot and no expiry timer armed -- a permanent stall. The
			// symbol table (etc/raid/anton.etc.symbol) defines 50..56 for 210..216,
			// 57..63 for 218..224 and 64..84 for the single-raid selectors; 217 has
			// no entry at all and is legitimately absent from both sides.
			continue
		}
		symbols = append(symbols, raid.RaidSymbolRow{Key: key, Value: uint32(objective.ClearCount)})
	}
	symbols = append(symbols, runtimeRaidPhaseTwoSymbolRows(state)...)
	// 卢克团队图的持久符号（祭坛/神殿计数、两个能源房效果、二阶段路线标记）。
	// 它们不是一次性脉冲，所以随每次快照重发，重连的成员看到同样的进度；
	// 安徒恩的符号表在 luke.etc.symbol 里什么也匹配不到，返回空。
	symbols = append(symbols, runtimeRaidLukeSymbolRows(state)...)
	symbolBody, err := raid.BuildRaidSymbolBody(symbols)
	if err != nil {
		return nil, nil, nil, err
	}
	// The trailing dword of 0x0249 is the infected-hatchery map id the client's
	// 战况表 colours from, not padding: the current EXE reads it and stores it at
	// manager+0x150, and re-derives symbols 8 and 7 from it (see
	// runtimeRaidInfectionMapIDForWire). It was left at its zero value here, so
	// every board this server ever drew had no 感染孵化场 marker while the client
	// also had symbol 8 silently cleared one packet later.
	//
	// Deliberately only the VALUE changes: the table is still sent on exactly the
	// same occasions as before (announce, or the 695 战况表 refresh / rejoin /
	// phase activation). Forcing an extra full table on the infection edge would
	// re-announce every already-cleared gate, which this client answers with the
	// 「已通关[%s]地下城」 toast (see the broadcast policy above).
	dungeonBody, err := raid.BuildRaidDungeonStateBody(raid.RaidDungeonStateSnapshot{
		Rows:          dungeons,
		TrailingValue: runtimeRaidInfectionMapIDForWire(state),
	})
	if err != nil {
		return nil, nil, nil, err
	}
	return symbolBody, dungeonBody, dungeons, nil
}

// runtimeRaidMovieSymbolChanges lists the cutscene markers whose value moved, so a
// caller can pulse exactly those keys after a state change. They are deliberately
// NOT part of the periodic snapshot: the client's 0x0247 handler feeds
// SetRaidSymbol, whose value change drives the video, so replaying a marker with
// every snapshot (each clear, each timer, every reconnect) would restart the clip.
// The ids live in etc/raid/anton.etc.symbol and the client resolves the
// Video/AntonCutScene file itself -- the server never names a file.
func runtimeRaidMovieSymbolChanges(before, after map[uint32]uint32) []raid.RaidSymbolRow {
	if len(after) == 0 {
		return nil
	}
	keys := make([]uint32, 0, len(after))
	for key, value := range after {
		if previous, ok := before[key]; ok && previous == value {
			continue
		}
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	rows := make([]raid.RaidSymbolRow, 0, len(keys))
	for _, key := range keys {
		rows = append(rows, raid.RaidSymbolRow{Key: key, Value: after[key]})
	}
	return rows
}

// filterRuntimeRaidMovieSymbolRows drops the given keys from a marker pulse, for
// edges another packet already carries.
func filterRuntimeRaidMovieSymbolRows(rows []raid.RaidSymbolRow, excluded ...uint32) []raid.RaidSymbolRow {
	if len(rows) == 0 || len(excluded) == 0 {
		return rows
	}
	drop := make(map[uint32]struct{}, len(excluded))
	for _, key := range excluded {
		drop[key] = struct{}{}
	}
	out := make([]raid.RaidSymbolRow, 0, len(rows))
	for _, row := range rows {
		if _, skip := drop[row.Key]; skip {
			continue
		}
		out = append(out, row)
	}
	return out
}

// cloneRuntimeRaidMovieSymbols snapshots the markers so a caller can diff them.
func cloneRuntimeRaidMovieSymbols(state *runtimeRaidState) map[uint32]uint32 {
	if state == nil || len(state.Lifecycle.MovieSymbols) == 0 {
		return nil
	}
	out := make(map[uint32]uint32, len(state.Lifecycle.MovieSymbols))
	for key, value := range state.Lifecycle.MovieSymbols {
		out[key] = value
	}
	return out
}

// setRuntimeRaidMovieSymbolLocked pulses one cutscene marker. Caller holds raidMu
// and must broadcast runtimeRaidMovieSymbolChanges afterwards, otherwise the client
// never learns about the edge.
func (state *runtimeRaidState) setRuntimeRaidMovieSymbolLocked(key, value uint32) {
	if state == nil {
		return
	}
	state.ensureRuntimeRaidLifecycleMapsLocked()
	// An absent key reads as 0, but an explicit 0 (the reset half of an edge set,
	// e.g. 127=0 alongside the break) still has to be recorded and announced --
	// only a repeat of the same stored value is a no-op.
	if previous, ok := state.Lifecycle.MovieSymbols[key]; ok && previous == value {
		return
	}
	state.Lifecycle.MovieSymbols[key] = value
	state.Lifecycle.Revision++
}

func runtimeRaidSingleSelectorSnapshotBodies(state runtimeRaidState) ([]byte, []byte, error) {
	if state.Lifecycle.Phase != runtimeRaidDungeonPhaseOne {
		return nil, nil, errRuntimeRaidPhaseMismatch
	}
	if state.RaidMode == runtimeRaidOwnerLuke {
		return runtimeRaidLukeSingleSelectorSnapshotBodies(state)
	}
	selectorIDs := []int{210, 211, 212, 213, 214, 215, 216, 218, 219, 220, 221, 222, 223, 224}
	wireStates := make(map[int]uint32, len(selectorIDs))
	clearCounts := make(map[int]uint32, len(selectorIDs))
	nativeObjectives := make([]runtimeRaidDungeonObjective, 0, len(state.Lifecycle.Objectives))
	for _, selectorID := range selectorIDs {
		wireStates[selectorID] = runtimeRaidObjectiveWireState(raid.AntonDungeonHide)
	}
	for _, objective := range state.Lifecycle.Objectives {
		if objective.Phase != runtimeRaidDungeonPhaseOne {
			continue
		}
		nativeObjectives = append(nativeObjectives, objective)
		selectorID, ok := runtimeRaidSingleSelectorMapID(state.SingleRaidType, objective.MapID)
		if !ok {
			continue
		}
		wireStates[selectorID] = runtimeRaidObjectiveWireState(objective.State)
		clearCounts[selectorID] = uint32(objective.ClearCount)
	}
	symbols := make([]raid.RaidSymbolRow, 0, len(selectorIDs))
	dungeons := make([]raid.RaidDungeonStateRow, 0, len(selectorIDs))
	for _, selectorID := range selectorIDs {
		key, ok := runtimeRaidObjectiveSymbolKey(selectorID)
		if !ok {
			return nil, nil, errRuntimeRaidPhaseMismatch
		}
		symbols = append(symbols, raid.RaidSymbolRow{Key: key, Value: clearCounts[selectorID]})
		dungeons = append(dungeons, raid.RaidDungeonStateRow{
			DungeonID: uint32(selectorID),
			State:     wireStates[selectorID],
		})
	}
	// This EXE has two simultaneous identities for a single-Anton dungeon.
	// The selector and Insert status UI render only the legacy 210..224 rows,
	// while the dedicated 5314..5322 identity from op28 is used by the Raid
	// objective lookup that builds op695 monster-HP rows.  Keep both identities
	// in op585: omitting the native alias makes that lookup return an empty
	// vector and the client retires every room actor immediately; omitting the
	// legacy row leaves the selector blank.
	sort.Slice(nativeObjectives, func(i, j int) bool { return nativeObjectives[i].MapID < nativeObjectives[j].MapID })
	for _, objective := range nativeObjectives {
		dungeons = append(dungeons, raid.RaidDungeonStateRow{
			DungeonID: uint32(objective.MapID),
			State:     runtimeRaidObjectiveWireState(objective.State),
		})
	}
	symbolBody, err := raid.BuildRaidSymbolBody(symbols)
	if err != nil {
		return nil, nil, err
	}
	dungeonBody, err := raid.BuildRaidDungeonStateBody(raid.RaidDungeonStateSnapshot{Rows: dungeons})
	if err != nil {
		return nil, nil, err
	}
	return symbolBody, dungeonBody, nil
}

func runtimeRaidLukeSingleSelectorSnapshotBodies(state runtimeRaidState) ([]byte, []byte, error) {
	selectorIDs := make([]int, 0, len(state.Lifecycle.Objectives))
	for mapID, objective := range state.Lifecycle.Objectives {
		if objective.Phase == runtimeRaidDungeonPhaseOne {
			selectorIDs = append(selectorIDs, mapID)
		}
	}
	sort.Ints(selectorIDs)
	symbols := make([]raid.RaidSymbolRow, 0, len(selectorIDs))
	dungeons := make([]raid.RaidDungeonStateRow, 0, len(selectorIDs))
	for _, selectorID := range selectorIDs {
		objective := state.Lifecycle.Objectives[selectorID]
		key, ok := runtimeRaidObjectiveSymbolKey(selectorID)
		if !ok {
			return nil, nil, errRuntimeRaidPhaseMismatch
		}
		symbols = append(symbols, raid.RaidSymbolRow{Key: key, Value: uint32(objective.ClearCount)})
		dungeons = append(dungeons, raid.RaidDungeonStateRow{
			DungeonID: uint32(selectorID),
			State:     runtimeRaidObjectiveWireState(objective.State),
		})
	}
	symbolBody, err := raid.BuildRaidSymbolBody(symbols)
	if err != nil {
		return nil, nil, err
	}
	dungeonBody, err := raid.BuildRaidDungeonStateBody(raid.RaidDungeonStateSnapshot{Rows: dungeons})
	if err != nil {
		return nil, nil, err
	}
	return symbolBody, dungeonBody, nil
}

// runtimeRaidLukeObjectiveSymbolKeys 是 etc/raid/luke/luke.etc.symbol 的
// "<id> DGN HP" 表里**队伍攻坚**那些门，逐字抄录而不是算出来的：这些号不连续
// （72、77..81、83 空着），而且队伍门也不是按 map id 排序的，任何算术形式都是猜。
//
// 上游这张表还包含 5323..5334 → 89..100（单人卢克的 12 关）。本树**有意省略那一段**：
// 我们这条线的单人卢克已经上线在用，走下面原有的 `73 + (mapID-5323)` 算术分支；
// 上游把这一段重编成 89..100，动它会改到已经跑通的单人卢克显示，所以这里只补
// 队伍攻坚缺失的门（df0b093：卢克队伍门以前在这张表里一个都没有，快照构建器会把
// 没有符号的门整行跳过，op585 就发出 0 行，战况表与选图页什么都画不出来）。
var runtimeRaidLukeObjectiveSymbolKeys = map[int]uint32{
	// 一阶段三个圣所。
	5054: 67, 5055: 68, 5056: 69,
	// 二阶段：两个能源房、两个祭坛、两个王座。
	5057: 70, 5058: 71, 5059: 73, 5060: 74, 5061: 75, 5062: 76,
	// 二阶段：两个祭坛入口与两个共用房间。
	5311: 82, 5313: 84, 5063: 85, 5064: 86, 5065: 87, 5066: 88,
}

func runtimeRaidObjectiveSymbolKey(mapID int) (uint32, bool) {
	if key, ok := runtimeRaidLukeObjectiveSymbolKeys[mapID]; ok {
		return key, true
	}
	switch {
	case mapID >= 5314 && mapID <= 5322:
		return uint32(64 + mapID - 5314), true
	case mapID >= 5323 && mapID <= 5334:
		return uint32(73 + mapID - 5323), true
	case mapID >= 210 && mapID <= 216:
		return uint32(50 + mapID - 210), true
	case mapID >= 218 && mapID <= 224:
		return uint32(57 + mapID - 218), true
	default:
		return 0, false
	}
}

// runtimeRaidObjectiveWireState projects one gate state onto the 0x0249/0x024A
// state byte.
//
// The current EXE hands the byte straight to sub_17C13C0(manager, dungeonId,
// state) and every reader branches on -1 / 0 / 1 / 2 / 3 (getter sub_17C1420 has
// 43 consumers: 0x00F142C9 tests 0/1/3 explicitly and defaults 2, while
// 0x00F16987, 0x015CA923 and 0x015CBCCF compare against -1, 2, 1 and 3 in turn).
// etc/raid/anton.etc only ever spells "open" and "hide" -- verified against the
// real Script.pvf -- and the 86JP reference server sends 0 for those and 2 for a
// gate it has closed again, 3 for one whose clears are done. So:
//
//	open -> 0, hide -> 2, cleared -> 3
//
// "close" is a PVF word this raid never uses (the Luke catalogs do); it keeps its
// old value because nothing pins what a closed-but-listed gate should look like.
func runtimeRaidObjectiveWireState(state raid.AntonDungeonState) uint32 {
	switch state {
	case raid.AntonDungeonOpen:
		return 0
	case raid.AntonDungeonCleared:
		return 3
	case raid.AntonDungeonClosed:
		return 1
	case raid.AntonDungeonHide:
		return 2
	default:
		return 2
	}
}

// runtimeRaidViewerObjectiveWireState 把一行的状态投影成**某一名成员**看到的字节。
//
// 卢克二阶段有两条互斥路线：被绑到一条的成员，看另一条路线的门应当画成 close，
// 否则界面把它画成 open、玩家点了却被拒，屏幕上什么解释都没有。
//
// 只有呈现会变：权威 objective 保留自己的状态，真正拒绝入场的是
// validateAntonRaidDungeonEntry。viewer=0 或非队伍卢克时逐字返回原值。
//
// 移植自上游 raid_phase_runtime.go（bfb2098 / ede0bf5）。
func runtimeRaidViewerObjectiveWireState(
	state runtimeRaidState,
	viewer uint16,
	objective runtimeRaidDungeonObjective,
) uint32 {
	wire := runtimeRaidObjectiveWireState(objective.State)
	if viewer == 0 || objective.State != raid.AntonDungeonOpen ||
		state.TailFlag == 1 || runtimeRaidTeamGraphOwner(state) != runtimeRaidOwnerLuke {
		return wire
	}
	if runtimeRaidLukeRouteEntryAllowedLocked(&state, viewer, objective.MapID) {
		return wire
	}
	return runtimeRaidObjectiveWireState(raid.AntonDungeonClosed)
}

// runtimeRaidPhaseSnapshotViewerDungeonBody 只重建某一名成员的 0x024A 行，
// 共享的符号/计时器通知仍然只编码一次。
func runtimeRaidPhaseSnapshotViewerDungeonBody(state runtimeRaidState, viewer uint16) ([]byte, error) {
	_, dungeonBody, _, err := runtimeRaidPhaseSnapshotBodiesForViewer(state, viewer)
	if err != nil {
		return nil, err
	}
	return dungeonBody, nil
}

func (s *Service) publishRuntimeRaidObjectiveProgress(raidKey uint32, phase byte) error {
	// A member whose client froze inside a gate never reports abandoned or
	// completed, so its record and its squad's occupancy would survive every later
	// completion and keep openRuntimeRaidPhaseMovie closed forever. Reconcile the
	// open gates against the live sessions before this progress is judged.
	if released := s.releaseRuntimeRaidStaleGateOccupancy(raidKey); released > 0 {
		s.logPacketEvent("game-raid-objective-progress-stale-gates-released",
			"raid_key", raidKey, "raid_phase", phase, "released", released)
	}
	s.raidMu.Lock()
	state := s.raids[raidKey]
	if state == nil {
		s.raidMu.Unlock()
		return errRuntimeRaidNotFound
	}
	if state.Lifecycle.Phase != phase || len(state.Lifecycle.Objectives) == 0 {
		s.raidMu.Unlock()
		return nil
	}
	progress := cloneRuntimeRaidState(*state)
	complete := runtimeRaidPhaseObjectivesComplete(progress, phase)
	s.raidMu.Unlock()

	now := time.Now().UTC()
	if err := s.broadcastRuntimeRaidPhaseSnapshot(progress, false, now); err != nil {
		return err
	}
	if !complete {
		return nil
	}
	// The tail is handed to the gameplay timer queue instead of running on the
	// caller's stack.
	//
	// The caller here is the completing member's packet goroutine, and it holds
	// that member's own session.dungeon.mu (completeCurrentDungeonBossDieCheckLocked
	// is the entry point). The tail force-returns every teammate still inside a gate
	// of the phase, and that return takes the teammate's session lock
	// (sendCurrentFullDungeonReturnToTown, dungeon_return_town.go:1000). With two
	// members both inside their own packet path -- the normal case for a party
	// clearing 擎天 A/B or 火山 -- each goroutine holds its own lock and then waits
	// for the other's: a permanent deadlock in which the settlement, the movie and
	// every later packet never happen. Live 2026-09-20 08:32:02Z, raid 1507329: op115
	// went out, the participation rows and the volcano cutscene marker went out, and
	// then that connection was silent for 71.2 s until the client's socket died --
	// no event=completed, no phase-clear registration, no settlement entry.
	// Reproduced deterministically (before this change) by
	// TestRuntimeRaidPhaseCompletionNeverWaitsOnAMateSessionLock.
	//
	// What the old comment here called "safe" was only the TryLock *probe*: the probe
	// skips a busy session, but between the probe and the actual return the teammate's
	// own goroutine can take its lock, and then the return blocks on it. 86JP has the
	// same ordering discipline this restores: the phase-break presentation is awaited
	// outside every manager and session critical section (RaidHandler.Progression.cs
	// :998-1015), and the displaced groups are returned per session, each in its own
	// try/catch, never from the clearing member's critical section.
	//
	// Ordering is unchanged -- the forced returns still run before the settlement and
	// the movie, because finishRuntimeRaidPhase does them in that order -- it simply
	// no longer runs inside the completing member's session lock.
	s.scheduleRuntimeRaidPhaseCompletion(raidKey, phase)
	return nil
}

// finishRuntimeRaidPhase is the completed phase's tail: force the parties still
// inside the phase's gates back to town, announce the clear, and publish the
// settlement movie.
func (s *Service) finishRuntimeRaidPhase(raidKey uint32, phase byte) {
	if s == nil || raidKey == 0 {
		return
	}
	// The gates first: the movie refuses to open while any gate is still occupied
	// (openRuntimeRaidPhaseMovie requires len(GroupOccupancy) == 0), and the forced
	// return is what releases them.
	if returned := s.forceReturnRuntimeRaidPhaseGroups(raidKey, phase, "raid_phase_objective_completed"); returned > 0 {
		s.logPacketEvent("game-raid-phase-groups-force-returned",
			"raid_key", raidKey,
			"raid_phase", phase,
			"returned_sessions", returned,
			"reason", "phase_objectives_complete_closes_every_gate_of_the_phase")
	}
	s.raidMu.Lock()
	state := s.raids[raidKey]
	if state == nil || state.Lifecycle.Phase != phase ||
		state.Lifecycle.Status != runtimeRaidLifecyclePhaseActive {
		// The phase already settled (a second member's completion scheduled the same
		// tail) or the raid is gone: the settlement below must not run twice.
		s.raidMu.Unlock()
		return
	}
	settling := cloneRuntimeRaidState(*state)
	s.raidMu.Unlock()

	// 门还没全关上时**不能**往下走：openRuntimeRaidPhaseMovie 要求 GroupOccupancy 为空，
	// 于是电影打不开 → 结算不发 → 客户端既没有结算页也没有翻牌，通报也一起丢。
	//
	// 真机 2026-10-01 20:32:37/38（两人几乎同时打死罗什）：两台会话各自占着自己的
	// session.dungeon 锁去登记通关，forceReturnRuntimeRaidPhaseGroups 逐个 TryLock
	// 时**两个都被跳过**（game-raid-phase-force-return-skipped-busy-session ×2），
	// 于是这里没有重试、整场卡住三分钟，直到玩家自己按"返回城镇"。
	// 现在改为有界重试：等他们真的回城了再把结算放出去。
	if len(settling.Lifecycle.GroupOccupancy) != 0 {
		if s.scheduleRuntimeRaidPhaseCompletionRetry(raidKey, phase) {
			s.logPacketEvent("game-raid-phase-completion-awaits-free-gates",
				"raid_key", raidKey,
				"raid_phase", phase,
				"occupied_gates", len(settling.Lifecycle.GroupOccupancy),
				"reason", "phase_tail_retries_until_every_gate_is_released")
		}
		return
	}
	s.clearRuntimeRaidPhaseCompletionRetries(raidKey)

	// The raid is over the moment its last phase completes: announce it on the
	// channel while the settlement movie is still to come.
	s.announceRuntimeRaidClearIfNeeded(settling, phase)
	wirePhase, err := runtimeRaidSettlementWirePhase(phase)
	if err != nil {
		s.logPacketEvent("game-raid-phase-completion-wire-phase-failed",
			"raid_key", raidKey, "raid_phase", phase, "error", err)
		return
	}
	if err := s.publishRuntimeRaidPhaseSettlement(raidKey, phase, raid.RaidSettlementResult{
		ResultType:   0,
		PhaseIndex:   wirePhase,
		ClearSeconds: runtimeRaidDurationSeconds(runtimeRaidPhaseElapsed(settling, time.Now().UTC())),
	}, time.Now().UTC()); err != nil {
		s.logPacketEvent("game-raid-phase-completion-settlement-failed",
			"raid_key", raidKey, "raid_phase", phase, "error", err)
	}
}

// runtimeRaidPhaseCompletionRetryDelay / runtimeRaidPhaseCompletionRetryLimit bound the
// "wait for every gate to be released" retry of the completed phase's tail.
//
// Two seconds is longer than a packet path takes to release its session lock and shorter
// than the client's own patience; 15 attempts is 30 seconds, which is plenty for a forced
// return to land and small enough that a genuinely stuck gate cannot spin forever.
const (
	runtimeRaidPhaseCompletionRetryDelay = 2 * time.Second
	runtimeRaidPhaseCompletionRetryLimit = 15
)

// scheduleRuntimeRaidPhaseCompletionRetry re-runs the completed phase's tail while some
// gate is still occupied, so the settlement movie is not lost when every forced return
// hit a busy session lock. Bounded: runtimeRaidPhaseCompletionRetryLimit attempts.
//
// Returns false when the budget is spent (the tail then simply does not settle, exactly
// as before this change, and the log says so).
func (s *Service) scheduleRuntimeRaidPhaseCompletionRetry(raidKey uint32, phase byte) bool {
	if s == nil || raidKey == 0 {
		return false
	}
	s.raidPhaseCompletionMu.Lock()
	if s.raidPhaseCompletionRetries == nil {
		s.raidPhaseCompletionRetries = make(map[uint32]int)
	}
	attempts := s.raidPhaseCompletionRetries[raidKey]
	if attempts >= runtimeRaidPhaseCompletionRetryLimit {
		s.raidPhaseCompletionMu.Unlock()
		s.logPacketEvent("game-raid-phase-completion-retry-exhausted",
			"raid_key", raidKey, "raid_phase", phase, "attempts", attempts,
			"reason", "gates_stayed_occupied_until_the_retry_budget_ran_out")
		return false
	}
	s.raidPhaseCompletionRetries[raidKey] = attempts + 1
	s.raidPhaseCompletionMu.Unlock()

	retry := func(time.Time) { s.finishRuntimeRaidPhase(raidKey, phase) }
	queue := s.ensureGameplayTimeQueue()
	if queue == nil {
		go retry(time.Time{})
		return true
	}
	name := fmt.Sprintf("raid-phase-complete-retry:%d:%d:%d", raidKey, phase, attempts)
	if err := queue.ScheduleAfter(name, runtimeRaidPhaseCompletionRetryDelay, retry); err != nil {
		s.logPacketEvent("game-raid-phase-completion-retry-schedule-failed",
			"raid_key", raidKey, "raid_phase", phase, "error", err)
		go retry(time.Time{})
	}
	return true
}

// clearRuntimeRaidPhaseCompletionRetries drops the retry budget once the tail settled.
func (s *Service) clearRuntimeRaidPhaseCompletionRetries(raidKey uint32) {
	if s == nil || raidKey == 0 {
		return
	}
	s.raidPhaseCompletionMu.Lock()
	delete(s.raidPhaseCompletionRetries, raidKey)
	s.raidPhaseCompletionMu.Unlock()
}

// scheduleRuntimeRaidPhaseCompletion is kept for the callers that must not run the
// tail on their own stack (a raid phase that expired from a timer callback, and the
// objective-completion path, whose caller is a packet goroutine holding its own
// session lock). It is the one entry point for the tail from any such caller.
func (s *Service) scheduleRuntimeRaidPhaseCompletion(raidKey uint32, phase byte) {
	if s == nil || raidKey == 0 {
		return
	}
	queue := s.ensureGameplayTimeQueue()
	if queue == nil {
		go s.finishRuntimeRaidPhase(raidKey, phase)
		return
	}
	name := fmt.Sprintf("raid-phase-complete:%d:%d", raidKey, phase)
	if err := queue.ScheduleAfter(name, 0, func(time.Time) {
		s.finishRuntimeRaidPhase(raidKey, phase)
	}); err != nil {
		s.logPacketEvent("game-raid-phase-completion-schedule-failed",
			"raid_key", raidKey, "raid_phase", phase, "error", err)
		go s.finishRuntimeRaidPhase(raidKey, phase)
	}
}

// scheduleRuntimeRaidForcedGroupReturn hands one forced-return sweep to the gameplay
// timer queue.
//
// The sweep itself is synchronous and correct; what must never happen is running it
// while the caller holds a session lock. Its callers are packet paths (the
// completing member's own goroutine, which holds that member's session.dungeon.mu),
// and the sweep takes each targeted teammate's session lock, so two members each
// inside their own packet path would wait for each other forever. The reference
// server has the same rule from the other side: its phase-break presentation and its
// displaced-group returns are awaited outside every manager lock
// (RaidHandler.Progression.cs:992-1015, and "锁内绝不 await" in
// DungeonCombatHandler.cs:2164).
//
// logKind, when non-empty, is emitted with report(returned) after the sweep.
// It falls back to a bare goroutine when the queue is unavailable or refuses the
// task, because losing the sweep would leave the gate occupied and stall the phase.
func (s *Service) scheduleRuntimeRaidForcedGroupReturn(
	raidKey uint32,
	phase byte,
	source string,
	logKind string,
	report func(returned int) []any,
	match func(mapID int) bool,
) {
	if s == nil || raidKey == 0 {
		return
	}
	run := func(time.Time) {
		returned := s.forceReturnRuntimeRaidGroupsMatching(raidKey, phase, source, match)
		if returned > 0 && logKind != "" && report != nil {
			s.logPacketEvent(logKind, report(returned)...)
		}
	}
	queue := s.ensureGameplayTimeQueue()
	if queue == nil {
		go run(time.Time{})
		return
	}
	name := fmt.Sprintf("raid-phase-return-groups:%d:%d:%s", raidKey, phase, source)
	if err := queue.ScheduleAfter(name, 0, run); err != nil {
		s.logPacketEvent("game-raid-phase-group-return-schedule-failed",
			"raid_key", raidKey,
			"raid_phase", phase,
			"source", source,
			"error", err)
		go run(time.Time{})
	}
}

// forceReturnRuntimeRaidPhaseGroups sends every member still inside a gate of the
// completed phase back to town, through the same abandon route the client's own
// 返回城镇 uses, and reports how many sessions were returned.
//
// It is the current-server shape of 86JP's
// ReturnRaidDungeonGroupsClosedByObjectiveAsync: the gate is closed by the
// objective, so the run cannot be finished and the member must not be left in a
// dungeon the raid no longer owns. Retiring the run also publishes the raid
// abandon (sendCurrentActiveDungeonReturnToTown -> notifyRuntimeRaidDungeonAbandonedLocked),
// which releases the squad's gate occupancy and lets the phase movie open.
func (s *Service) forceReturnRuntimeRaidPhaseGroups(raidKey uint32, phase byte, source string) int {
	return s.forceReturnRuntimeRaidGroupsMatching(raidKey, phase, source, nil)
}

// forceReturnRuntimeRaidGroupsMatching is the same forced return narrowed to the
// gates the caller names: the volcano's shield coming back closes only the black
// volcano (219), while the volcano falling closes 218/219 and every hatchery
// (86JP TryInvalidateDungeonRuns([218,219,221..224])). A nil match means every gate
// of the phase.
func (s *Service) forceReturnRuntimeRaidGroupsMatching(
	raidKey uint32,
	phase byte,
	source string,
	match func(mapID int) bool,
) int {
	if s == nil || raidKey == 0 {
		return 0
	}
	s.raidMu.Lock()
	state := s.raids[raidKey]
	if state == nil || state.Lifecycle.Phase != phase {
		s.raidMu.Unlock()
		return 0
	}
	seen := make(map[uint16]bool, len(state.FrozenMembers))
	candidates := make([]uint16, 0, len(state.FrozenMembers))
	for _, member := range state.FrozenMembers {
		if member.CharID == 0 || seen[member.CharID] {
			continue
		}
		seen[member.CharID] = true
		candidates = append(candidates, member.CharID)
	}
	s.raidMu.Unlock()

	type phaseRunTarget struct {
		session *gameSession
		runtime *runtimeDungeonState
	}
	targets := make([]phaseRunTarget, 0, len(candidates))
	for _, characterID := range candidates {
		session, ok := s.onlineGameSession(characterID)
		if !ok || session == nil {
			continue
		}
		// TryLock, never Lock. This runs from the completing member's own packet path,
		// which already holds that member's session.dungeon.mu, and Go mutexes are not
		// reentrant: a plain Lock on that session parked its goroutine forever, and the
		// phase never settled (live 2026-09-19 16:37:45Z, raid 1507329: closed gate
		// table and snapshot out, then nothing at all -- no settlement, no error, "一阶段
		// 打完了 不结束"). A session whose lock is busy is skipped, never waited on: the
		// member that is holding it is the one that just cleared the gate, whose client
		// is on its own settlement page, and every teammate still inside a closed gate
		// is returned below.
		if !session.dungeon.mu.TryLock() {
			s.logPacketEvent("game-raid-phase-force-return-skipped-busy-session",
				"raid_key", raidKey,
				"raid_phase", phase,
				"char_id", characterID,
				"reason", "session_lock_held_by_the_member_that_completed_the_phase")
			continue
		}
		runtime := session.dungeon.runtime
		insidePhase := runtime != nil &&
			runtime.raidDungeon.RaidKey == raidKey &&
			runtime.raidDungeon.Phase == phase &&
			dungeonRuntimeOwnsCharacter(runtime, characterID)
		if insidePhase && match != nil {
			insidePhase = match(runtime.raidDungeon.MapID)
		}
		session.dungeon.mu.Unlock()
		if insidePhase {
			targets = append(targets, phaseRunTarget{session: session, runtime: runtime})
		}
	}

	returned := 0
	for _, target := range targets {
		ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
		transition, err := s.prepareCurrentDungeonTownTransitionForSession(ctx, target.session, target.session.selectedCharacterID)
		cancel()
		if err != nil {
			s.logGameEvent(target.session, "game-raid-phase-force-return-blocked",
				"raid_key", raidKey,
				"raid_phase", phase,
				"char_id", target.session.selectedCharacterID,
				"dungeon_id", target.runtime.Dungeon.ID,
				"reason", "town_transition_unavailable_for_completed_phase",
				"error", err)
			continue
		}
		if err := s.sendCurrentActiveDungeonReturnToTownKeepingParty(
			target.session,
			target.runtime,
			transition,
			uint16(dnfenum.CmdPacketGiveupGame),
			source+"_force_town_return",
		); err != nil {
			s.logGameEvent(target.session, "game-raid-phase-force-return-blocked",
				"raid_key", raidKey,
				"raid_phase", phase,
				"char_id", target.session.selectedCharacterID,
				"dungeon_id", target.runtime.Dungeon.ID,
				"reason", "force_town_return_failed",
				"error", err)
			continue
		}
		returned++
		s.logGameEvent(target.session, "game-raid-phase-group-returned",
			"raid_key", raidKey,
			"raid_phase", phase,
			"char_id", target.session.selectedCharacterID,
			"dungeon_id", target.runtime.Dungeon.ID,
			"source", source,
			"reason", "the_phase_completed_and_closed_every_gate_of_this_phase")
	}
	return returned
}

func (s *Service) scheduleRuntimeRaidPhaseExpiry(state runtimeRaidState) error {
	queue := s.ensureGameplayTimeQueue()
	if queue == nil {
		return errGameplayTimeQueueUnavailable
	}
	delay := state.Lifecycle.PhaseDeadline.Sub(queue.Now())
	if delay < 0 {
		delay = 0
	}
	name := fmt.Sprintf("raid-phase:%d:%d:%d", state.RaidKey, state.Lifecycle.Phase, state.StartAttempt)
	return queue.ScheduleAfter(name, delay, func(due time.Time) {
		failed, err := s.expireRuntimeRaidPhase(state.RaidKey, state.Lifecycle.Phase, due)
		if err != nil {
			s.logPacketEvent("game-raid-phase-expiry-ignored", "raid_key", state.RaidKey, "raid_phase", state.Lifecycle.Phase, "error", err)
			return
		}
		// 超时空的过期与安徒恩/卢克走的不是同一条收尾：它要把整张地图上的成员全部
		// 送回**待机区** (98/2，不是频道门口 98/0)、退掉重试券、发 587{6,1}+601 这一
		// 对失败演出，并在保持 5 秒后放回 587{0} 让攻坚队还能再开一次。
		// 关键区别：**不删攻坚队行、不发 op665** —— op665 就是客户端那句
		// 「已经强制退出攻坚队。」，也是服主报的"攻坚队被解散"。见 publishFailedFiendWar。
		if failed.RaidMode == runtimeRaidOwnerFiendWar {
			if err := s.grantRuntimeFiendWarRetry(failed, due); err != nil {
				s.logPacketEvent("game-fiendwar-retry-ticket-grant-failed", "raid_key", failed.RaidKey, "error", err)
				s.scheduleRuntimeFiendWarRetry(failed, due)
			}
			// expireRuntimeRaidPhase 把行留在表里 (Status=Failed) 并在这里交给
			// 超时空自己的收尾；它既不删行也不排 scheduleRuntimeRaidFailedRemoval
			// —— 那条十秒清除器会把刚保住的攻坚队又删掉。
			if err := s.publishFailedFiendWar(failed); err != nil {
				s.logPacketEvent("game-fiendwar-expiry-publish-failed", "raid_key", failed.RaidKey, "error", err)
			}
			// 攻坚失败返还入场次数（+ 入场材料，超时空那一份由上面的重试券链路独占，
			// 见 runtimeRaidEntryMaterialRefundOwnedByRetry）。放在失败演出之后：返还的
			// 收尾是重推 op1303/op1206，那两包不能挤进横幅序列里。
			s.refundFailedRuntimeRaidEntry(failed, due)
			return
		}
		// 86JP publishes the failure, then hands the terminal object to a ten-second
		// remover (RaidHandler.Runtime.cs:639-644, :730-762). The raid stays in the
		// raid table in between so a reconnecting client still reads the result.
		if err := s.publishFailedRuntimeRaid(failed); err != nil {
			s.logPacketEvent("game-raid-phase-timeout-publish-failed", "raid_key", failed.RaidKey, "raid_phase", failed.Lifecycle.Phase, "tail_flag", failed.TailFlag, "error", err)
		}
		// 攻坚失败返还：安徒恩/卢克的唯一失败形态就是这条倒计时到点，所以这里是它们
		// 全部的次数返还点。中途放弃（abandonRuntimeRaid）不走这里，也就不返还。
		s.refundFailedRuntimeRaidEntry(failed, due)
		s.scheduleRuntimeRaidFailedRemoval(failed.RaidKey, failed.Lifecycle.Phase)
	})
}

// scheduleRuntimeRaidFailedRemoval arms 86JP's delayed teardown of a failed raid
// (RemoveFailedRaidAfterResultDelayAsync, RaidHandler.Runtime.cs:730-762). It is
// armed before the failure presentation is written, so a failed raid can never
// outlive its window even if a socket write fails.
func (s *Service) scheduleRuntimeRaidFailedRemoval(raidKey uint32, phase byte) {
	if s == nil || raidKey == 0 {
		return
	}
	queue := s.ensureGameplayTimeQueue()
	if queue == nil {
		// Without a timer queue the failed raid would stay in the table forever, so
		// release it inline rather than leaving every later admission blocked.
		s.removeRuntimeRaidFailedPhase(raidKey, phase)
		return
	}
	name := fmt.Sprintf("raid-fail-remove:%d:%d", raidKey, phase)
	if err := queue.ScheduleAfter(name, runtimeRaidFailedResultWindow, func(time.Time) {
		s.removeRuntimeRaidFailedPhase(raidKey, phase)
	}); err != nil {
		s.logPacketEvent("game-raid-failed-raid-removal-schedule-failed",
			"raid_key", raidKey, "raid_phase", phase, "error", err)
		s.removeRuntimeRaidFailedPhase(raidKey, phase)
	}
}

func (s *Service) enterRuntimeRaidStandby(raidKey uint32, phase byte, now time.Time) (runtimeRaidState, raid.RaidTimer, error) {
	now = runtimeRaidNormalizeTime(now)
	s.raidMu.Lock()
	state := s.raids[raidKey]
	if state == nil {
		s.raidMu.Unlock()
		return runtimeRaidState{}, raid.RaidTimer{}, errRuntimeRaidNotFound
	}
	if state.Lifecycle.Status == runtimeRaidLifecycleStandby && state.Lifecycle.Phase == phase {
		standby := cloneRuntimeRaidState(*state)
		s.raidMu.Unlock()
		return standby, runtimeRaidStandbyTimer(standby), nil
	}
	if phase != runtimeRaidDungeonPhaseOne || state.Lifecycle.Phase != phase ||
		state.Lifecycle.Status != runtimeRaidLifecycleAwaitingRewards || !state.Lifecycle.RewardDeliveryCommitted {
		s.raidMu.Unlock()
		return runtimeRaidState{}, raid.RaidTimer{}, errRuntimeRaidRewardNotReady
	}
	state.Lifecycle.Status = runtimeRaidLifecycleStandby
	state.Lifecycle.StandbyStartedAt = now
	config := runtimeRaidBaseConfig(*state)
	if config == nil {
		s.raidMu.Unlock()
		return runtimeRaidState{}, raid.RaidTimer{}, errRuntimeRaidLifecycleState
	}
	state.Lifecycle.StandbyDeadline = now.Add(time.Duration(config.PhaseBreakSeconds) * time.Second)
	state.Lifecycle.Revision++
	previousRevision := state.Lifecycle.Revision - 1
	standby := cloneRuntimeRaidState(*state)
	s.raidMu.Unlock()

	if err := s.scheduleRuntimeRaidStandby(standby); err != nil {
		s.raidMu.Lock()
		if current := s.raids[raidKey]; current != nil &&
			current.Lifecycle.Status == runtimeRaidLifecycleStandby &&
			current.Lifecycle.Phase == phase &&
			current.Lifecycle.Revision == standby.Lifecycle.Revision {
			current.Lifecycle.Status = runtimeRaidLifecycleAwaitingRewards
			current.Lifecycle.StandbyStartedAt = time.Time{}
			current.Lifecycle.StandbyDeadline = time.Time{}
			current.Lifecycle.Revision = previousRevision
		}
		s.raidMu.Unlock()
		return runtimeRaidState{}, raid.RaidTimer{}, err
	}
	return standby, runtimeRaidStandbyTimer(standby), nil
}

func runtimeRaidStandbyTimer(state runtimeRaidState) raid.RaidTimer {
	return raid.RaidTimer{
		TimerType: 1,
		Value0:    state.RaidKey,
		Value1:    uint32(state.Lifecycle.StandbyDeadline.Unix()),
		Value2:    runtimeRaidDurationSeconds(state.Lifecycle.StandbyDeadline.Sub(state.Lifecycle.StandbyStartedAt)),
	}
}

func (s *Service) scheduleRuntimeRaidStandby(state runtimeRaidState) error {
	queue := s.ensureGameplayTimeQueue()
	if queue == nil {
		return errGameplayTimeQueueUnavailable
	}
	delay := state.Lifecycle.StandbyDeadline.Sub(queue.Now())
	if delay < 0 {
		delay = 0
	}
	name := fmt.Sprintf("raid-standby:%d:%d", state.RaidKey, state.StartAttempt)
	return queue.ScheduleAfter(name, delay, func(due time.Time) {
		if _, err := s.activateRuntimeRaidPhase(state.RaidKey, runtimeRaidDungeonPhaseTwo, due, false); err != nil {
			s.logPacketEvent("game-raid-standby-activation-failed", "raid_key", state.RaidKey, "error", err)
		}
	})
}

func runtimeRaidDurationSeconds(duration time.Duration) uint32 {
	if duration <= 0 {
		return 0
	}
	seconds := uint64(duration / time.Second)
	if duration%time.Second != 0 {
		seconds++
	}
	if seconds > math.MaxUint32 {
		return math.MaxUint32
	}
	return uint32(seconds)
}
