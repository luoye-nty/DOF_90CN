package dnfbridge

import (
	"bytes"
	"context"
	"encoding/binary"

	"longheng.io/server/internal/modules/dnf/dnfenum"
	"longheng.io/server/internal/modules/dnf/worldmap"
)

const currentDungeonCharacterStatisticBodySize = 24

// handleDungeonCharacterStatistic records the six-u32 telemetry packet emitted
// by the current EXE's completed tutorial/settlement manager. The values never
// become authoritative rewards or assets. A single-floor tower has already
// committed its reward inventory and next floor while building op34/op35; this
// client-owned barrier only publishes that committed dedicated result.
func (s *Service) handleDungeonCharacterStatistic(session *gameSession, body []byte) error {
	if session == nil {
		return nil
	}
	if len(body) != currentDungeonCharacterStatisticBodySize {
		s.logDungeonStallBlocked(session, dungeonStallKindSettlement,
			"game-dungeon-character-statistic-blocked", uint32(session.selectedCharacterID),
			"body_len", len(body),
			"expected_body_len", currentDungeonCharacterStatisticBodySize,
			"reason", "current_exe_op123_six_u32_boundary_mismatch")
		return nil
	}

	session.dungeon.mu.Lock()
	runtime := session.dungeon.runtime
	if !completedDungeonSettlementEntryReady(runtime, session.selectedCharacterID) {
		session.dungeon.mu.Unlock()
		s.logDungeonStallBlocked(session, dungeonStallKindSettlement,
			"game-dungeon-character-statistic-blocked", uint32(session.selectedCharacterID),
			"char_id", session.selectedCharacterID,
			"body_len", len(body),
			"reason", "completed_settlement_entry_owner_not_ready")
		return nil
	}
	if runtime.settlementStatisticReceived {
		if bytes.Equal(runtime.settlementStatisticBody, body) {
			s.logGameEvent(session, "game-dungeon-character-statistic-replay",
				"char_id", session.selectedCharacterID,
				"dungeon_id", runtime.Dungeon.ID,
				"body_len", len(body),
				"reason", "exact_replay_is_idempotent")
		} else {
			session.dungeon.mu.Unlock()
			s.logDungeonStallBlocked(session, dungeonStallKindSettlement,
				"game-dungeon-character-statistic-blocked", uint32(session.selectedCharacterID),
				"char_id", session.selectedCharacterID,
				"dungeon_id", runtime.Dungeon.ID,
				"body_len", len(body),
				"reason", "conflicting_replay_after_first_capture")
			return nil
		}
	} else {
		runtime.settlementStatisticReceived = true
		runtime.settlementStatisticBody = append([]byte(nil), body...)
		s.logGameEvent(session, "game-dungeon-character-statistic-captured",
			"char_id", session.selectedCharacterID,
			"dungeon_id", runtime.Dungeon.ID,
			"body_len", len(body),
			"value0", binary.LittleEndian.Uint32(body[0:4]),
			"value1", binary.LittleEndian.Uint32(body[4:8]),
			"value2", binary.LittleEndian.Uint32(body[8:12]),
			"value3", binary.LittleEndian.Uint32(body[12:16]),
			"value4", binary.LittleEndian.Uint32(body[16:20]),
			"value5", binary.LittleEndian.Uint32(body[20:24]),
			"authoritative_for_rewards", false,
			"source", "current_cn_six_u32_settlement_writer")
	}

	// A tower never runs the ordinary card settlement: its clear window is the
	// native op348/op1263 notification published by the frozen settlement plan.
	if currentDungeonTowerRewardMsgID(runtime) == 0 {
		session.dungeon.mu.Unlock()
		return nil
	}
	session.dungeon.mu.Unlock()
	return nil
}

// currentDungeonSingleFloorTowerSettlement reports whether this run is a tower
// clear and which family it belongs to. The family name is kept for logging;
// the native message ID (op348/op1263) is what actually drives the window.
func (s *Service) currentDungeonSingleFloorTowerSettlement(runtime *runtimeDungeonState) (string, bool) {
	kind := currentDungeonTowerKind(runtime)
	if kind == "" {
		return "", false
	}
	return "tower_of_" + string(kind), true
}

// handleDungeonGiveupGame closes both current EXE op42 meanings without
// conflating them: an active run remains a give-up request, while a completed
// run is accepted only after the server's settlement-entry op31, current EXE
// op46 play-result request, and post-settlement op123 statistic barrier. The
// client owns the wait before it emits op42; the server must not replace that
// lifecycle with an unrelated timer. An active-run typed op24 remains pending
// until positive town-side scene evidence commits it. A completed tutorial's
// first op42 instead uses the already accepted full completed-town route and
// detaches only after that route writes its typed op24.
func (s *Service) handleDungeonGiveupGame(session *gameSession, body []byte) error {
	if session == nil {
		return nil
	}
	if len(body) != 0 {
		s.logGameEvent(session, "game-dungeon-giveup-game-blocked",
			"body_len", len(body),
			"reason", "current_exe_op42_body_must_be_empty")
		return nil
	}
	if session.selectedCharacterID == 0 {
		s.logGameEvent(session, "game-dungeon-giveup-game-blocked",
			"body_len", len(body),
			"reason", "selected_character_missing")
		return nil
	}
	if _, err := s.persistCurrentActiveTutorialExit(
		session,
		session.selectedCharacterID,
		"confirmed_mid_tutorial_bodyless_op42_active_run_giveup",
	); err != nil {
		s.logGameEvent(session, "game-dungeon-giveup-game-blocked",
			"char_id", session.selectedCharacterID,
			"reason", "confirmed_tutorial_abandonment_persistence_failed",
			"error", err)
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	transition, err := s.prepareCurrentDungeonTownTransitionForSession(ctx, session, session.selectedCharacterID)
	if err != nil {
		s.logGameEvent(session, "game-dungeon-giveup-game-blocked",
			"char_id", session.selectedCharacterID,
			"reason", "persisted_town_transition_unavailable",
			"error", err)
		return nil
	}

	session.dungeon.mu.Lock()
	runtime := session.dungeon.runtime
	if runtime == nil || runtime.Session == nil || !dungeonRuntimeOwnsCharacter(runtime, session.selectedCharacterID) {
		session.dungeon.mu.Unlock()
		s.logGameEvent(session, "game-dungeon-giveup-game-blocked",
			"char_id", session.selectedCharacterID,
			"reason", "active_dungeon_runtime_owner_missing")
		return nil
	}
	snapshot := runtime.Session.Snapshot()
	// A cleared TEAM raid gate has no card flip at all: its reward is the phase
	// settlement the raid owner publishes once the phase completes, not a
	// per-dungeon card row. The ordinary completed-run chain therefore never runs
	// for one, and every barrier that waits for it -- the settlement entry, the
	// play-result capture, the statistic, the card layout/selection/reward commit --
	// would wait forever. Live incident 2026-09-16 18:43: both members of a team
	// raid cleared gate 210, each sent op42 twice, and every request was answered
	// with "completed_settlement_entry_owner_not_ready" or
	// "exit-deferred-before-card-reward"; both clients sat on the clear screen with
	// neither 再次挑战 nor 返回城镇 working.
	cardlessRaid := s.completedCurrentTeamRaidDungeonExitReady(runtime, session.selectedCharacterID)
	if snapshot.Run.Status == worldmap.DungeonRunCompleted &&
		!cardlessRaid &&
		!completedDungeonSettlementResultReady(runtime, session.selectedCharacterID) {
		session.dungeon.mu.Unlock()
		s.logGameEvent(session, "game-dungeon-giveup-game-blocked",
			"char_id", session.selectedCharacterID,
			"dungeon_id", runtime.Dungeon.ID,
			"run_status", snapshot.Run.Status,
			"settlement_entry_sent", runtime.settlementEntrySent,
			"settlement_play_result_received", runtime.settlementPlayResultReceived,
			"settlement_statistic_received", runtime.settlementStatisticReceived,
			"reason", "completed_run_missing_play_result_or_statistic_barrier")
		return nil
	}
	if snapshot.Run.Status != worldmap.DungeonRunActive && snapshot.Run.Status != worldmap.DungeonRunCompleted {
		session.dungeon.mu.Unlock()
		s.logGameEvent(session, "game-dungeon-giveup-game-blocked",
			"char_id", session.selectedCharacterID,
			"dungeon_id", runtime.Dungeon.ID,
			"run_status", snapshot.Run.Status,
			"reason", "dungeon_run_not_active_or_completed")
		return nil
	}
	if runtime.townReturnPending && snapshot.Run.Status != worldmap.DungeonRunCompleted {
		session.dungeon.mu.Unlock()
		s.logGameEvent(session, "game-dungeon-giveup-game-replay",
			"char_id", session.selectedCharacterID,
			"dungeon_id", runtime.Dungeon.ID,
			"run_status", snapshot.Run.Status,
			"op24_sent", runtime.townReturnOp24Sent,
			"reason", "town_transition_already_pending")
		return nil
	}

	source := "current_exe_bodyless_op42_active_run_giveup"
	if snapshot.Run.Status == worldmap.DungeonRunCompleted {
		source = "current_exe_op31_completed_tutorial_or_settlement_timed_op42"
		completedTutorial := isPVFTutorialDungeon(runtime)
		if completedTutorial {
			source = "current_exe_completed_pvf_tutorial_first_op42_return_to_town"
		}
		if currentDungeonRuntimeIsPractice(runtime) {
			runtime.advanceSettlementPhase(currentDungeonSettlementPhaseEnding)
			session.dungeon.mu.Unlock()
			return s.sendCurrentCompletedDungeonReturnToTown(
				session,
				runtime,
				transition,
				uint16(dnfenum.CmdPacketGiveupGame),
				"current_exe_op255_practice_direct_return_to_town",
				// op42 is its own handler and holds no world lock.
				false,
			)
		}
		if cardlessRaid {
			// A raid gate is done the moment its objective is committed; there is no
			// card row to wait for, so the exit goes straight back to the raid town.
			runtime.advanceSettlementPhase(currentDungeonSettlementPhaseEnding)
			session.dungeon.mu.Unlock()
			return s.sendCurrentCompletedDungeonReturnToTown(
				session,
				runtime,
				transition,
				uint16(dnfenum.CmdPacketGiveupGame),
				"current_exe_team_raid_completed_direct_return_to_town",
				false,
			)
		}
		cardlessTower := completedCurrentSingleFloorTowerSettlementExitReady(runtime, session.selectedCharacterID)
		if !completedTutorial && !cardlessTower && !cardlessRaid && runtime.settlementCardLayoutSent &&
			!runtime.settlementCardSideSelectionSent[dungeonCardSideFree] {
			if err := s.selectCurrentDungeonCardLocked(
				session,
				runtime,
				dungeonCardSideFree,
				runtimePartyDungeonCardMemberIndex(runtime),
				"current_exe_completed_op42_auto_flip_free_row_before_exit",
			); err != nil {
				s.logGameEvent(session, "game-dungeon-giveup-game-completed-auto-flip-blocked",
					"char_id", session.selectedCharacterID,
					"dungeon_id", runtime.Dungeon.ID,
					"request_msg_id", uint16(dnfenum.CmdPacketGiveupGame),
					"error", err)
			}
		}
		if !completedTutorial && !cardlessTower && !cardlessRaid && runtime.settlementCardSelectionSent && !runtime.settlementCardRewardCommitted {
			if err := s.commitCurrentDungeonCardRewardLocked(session, runtime, "current_exe_completed_op42_reward_retry_before_exit"); err != nil {
				s.logGameEvent(session, "game-dungeon-giveup-game-completed-exit-reward-retry-blocked",
					"char_id", session.selectedCharacterID,
					"dungeon_id", runtime.Dungeon.ID,
					"request_msg_id", uint16(dnfenum.CmdPacketGiveupGame),
					"error", err)
			}
		}
		if !completedTutorial && !cardlessTower && !cardlessRaid && (!runtime.settlementCardLayoutSent ||
			!runtime.settlementCardSelectionSent || !runtime.settlementCardRewardCommitted ||
			runtime.settlementPhase < currentDungeonSettlementPhaseRewardCommitted) {
			s.logGameEvent(session, "game-dungeon-giveup-game-completed-exit-deferred-before-card-reward",
				"char_id", session.selectedCharacterID,
				"dungeon_id", runtime.Dungeon.ID,
				"request_msg_id", uint16(dnfenum.CmdPacketGiveupGame),
				"layout_sent", runtime.settlementCardLayoutSent,
				"selection_sent", runtime.settlementCardSelectionSent,
				"reward_committed", runtime.settlementCardRewardCommitted,
				"settlement_phase", runtime.settlementPhase.String(),
				"reason", "current_exe_card_request_chain_not_finished")
			session.dungeon.mu.Unlock()
			return nil
		}
		runtime.advanceSettlementPhase(currentDungeonSettlementPhaseEnding)
		session.dungeon.mu.Unlock()
		return s.sendCurrentCompletedDungeonReturnToTown(
			session,
			runtime,
			transition,
			uint16(dnfenum.CmdPacketGiveupGame),
			source,
			false,
		)
	}
	// 超时空：**队长**按下撤退（无包体 op42）时，这支临时队伍一起回城。
	// 判据必须在队长自己那一趟回城**之前**抓下来 —— 那一趟会把 runtime 上的攻坚绑定
	// 清掉（真机 2026-10-02 10:29:10：只有队长一个人回了城，同队的 char 21 留在格里，
	// 23 秒后连点 op72 被 settlement_result_not_ready 拒了 119 次才自己按了 op42）。
	// 只影响超时空；安东/卢克这里拿到的永远是零值 plan。
	retreatPlan := s.runtimeFiendWarPartyRetreatPlanFor(session, runtime)
	session.dungeon.mu.Unlock()
	if err := s.sendCurrentActiveDungeonReturnToTown(
		session,
		runtime,
		transition,
		uint16(dnfenum.CmdPacketGiveupGame),
		source,
	); err != nil {
		return err
	}
	s.runRuntimeFiendWarPartyRetreat(retreatPlan, source)
	return nil
}

func (s *Service) completedDungeonSettlementExitReady(runtime *runtimeDungeonState, characterID uint16) bool {
	if completedCurrentDungeonAltarSettlementExitReady(runtime, characterID) {
		return true
	}
	if completedRuntimeRaidDungeonExitReady(runtime, characterID) {
		return true
	}
	if s.currentRaidGateCardlessExitReady(runtime, characterID) {
		return true
	}
	if !completedDungeonSettlementResultReady(runtime, characterID) {
		return false
	}
	if completedCurrentSingleFloorTowerSettlementExitReady(runtime, characterID) {
		return true
	}
	if isPVFTutorialDungeon(runtime) {
		return true
	}
	if runtime.tournament != nil {
		return runtime.tournament.RewardSelectionComplete()
	}
	if currentDungeonRuntimeIsPractice(runtime) {
		return true
	}
	return runtime.settlementCardLayoutSent && runtime.settlementCardSelectionSent &&
		runtime.settlementCardRewardCommitted &&
		runtime.settlementPhase >= currentDungeonSettlementPhaseRewardCommitted
}

// completedCurrentSingleFloorTowerSettlementExitReady reports whether a tower
// clear may return to town: its own native window (op348/op1263) must have been
// published, and the ordinary result statistics are not its ACK.
func completedCurrentSingleFloorTowerSettlementExitReady(runtime *runtimeDungeonState, characterID uint16) bool {
	return completedCurrentDungeonTowerSettlementExitReady(runtime, characterID)
}

// completedRuntimeRaidDungeonExitReady separates a completed Raid objective
// from the ordinary dungeon card owner. Anton advances and rewards the whole
// raid through its own lifecycle; each intermediate dungeon must therefore be
// allowed to return to the private waiting selector after op31/op46/op123/op35
// without inventing a per-map card selection.  The final single-Raid native
// movie/card path never sends that ordinary settlement sequence; state=6 owns
// a separate completion flag and only needs the authoritative completed run.
func completedRuntimeRaidDungeonExitReady(runtime *runtimeDungeonState, characterID uint16) bool {
	if runtime == nil || !runtime.raidDungeon.completedNotified ||
		runtime.raidDungeon.abandonedNotified || runtime.raidDungeon.RaidKey == 0 {
		return false
	}
	if runtime.raidNativeSettlementFinished {
		return runtime.Session != nil && dungeonRuntimeOwnsCharacter(runtime, characterID) &&
			runtime.Session.Snapshot().Run.Status == worldmap.DungeonRunCompleted
	}
	return completedDungeonSettlementResultReady(runtime, characterID)
}

func completedTournamentSettlementExitReady(runtime *runtimeDungeonState, characterID uint16) bool {
	return runtime != nil && runtime.tournament != nil &&
		runtime.tournament.RewardSelectionComplete() &&
		completedDungeonSettlementResultReady(runtime, characterID)
}

// currentDungeonRuntimeIsRaidGate reports a run inside a raid gate.
//
// A raid gate drops nothing at all: its reward is the phase settlement the raid
// owner publishes, not loot, so both the weighted monster pool (which carries the
// gold entries and the regional materials) and the quest-item table stay switched
// off for it.
func (s *Service) currentDungeonRuntimeIsRaidGate(runtime *runtimeDungeonState) bool {
	return runtime != nil && runtime.raidDungeon.RaidKey != 0
}

// completedCurrentTeamRaidDungeonExitReady reports a cleared TEAM raid gate,
// which has no card flip and therefore no ordinary settlement chain to wait for.
//
// A single raid keeps the native settlement -- the raid owner drives the result
// movie and the card rows (runtimeRaidCompletedDungeonOwnsNativeSettlement) -- so
// only the team case is exempt.
func (s *Service) completedCurrentTeamRaidDungeonExitReady(runtime *runtimeDungeonState, characterID uint16) bool {
	if s == nil || runtime == nil || characterID == 0 || runtime.raidDungeon.RaidKey == 0 {
		return false
	}
	return !s.runtimeRaidCompletedDungeonOwnsNativeSettlement(characterID, runtime.raidDungeon)
}

// currentRaidGateCardlessExitReady reports a cleared raid gate whose settlement
// result screen is already up.
//
// A raid gate never owns an ordinary per-map card row: its reward is the phase
// settlement the raid owner publishes. The opener's session proves that through
// its completed raid binding, but a squadmate that adopted the gate's binding
// carries no local completed flag of its own, and a failed completion publish is
// deliberately kept non-fatal instead of disconnecting the player. Both cases
// used to leave the client on a card screen that can never be filled -- live
// report 2026-09-16 11:14Z, the leader's 返回城镇 was answered with
// op72_cannot_cross_unfinished_card_or_reward_phase while the raid was cleared.
// Every other exit owner still requires its own terminal state; this one only
// fires for a completed raid gate with a fully shown result.
func (s *Service) currentRaidGateCardlessExitReady(runtime *runtimeDungeonState, characterID uint16) bool {
	return s.currentDungeonRuntimeIsRaidGate(runtime) &&
		completedDungeonSettlementResultReady(runtime, characterID)
}

func completedDungeonSettlementResultReady(runtime *runtimeDungeonState, characterID uint16) bool {
	if !completedDungeonSettlementEntryReady(runtime, characterID) ||
		!runtime.settlementPlayResultReceived || !runtime.settlementClearRewardSent ||
		runtime.settlementPhase < currentDungeonSettlementPhaseResultShown {
		return false
	}
	if currentDungeonTowerRewardMsgID(runtime) != 0 {
		// Tower notifications set their own native clear flag and show the
		// dedicated window. The ordinary result statistics are not its ACK.
		return runtime.settlementTowerRewardSent
	}
	return currentDungeonRuntimeIsPractice(runtime) || runtime.settlementStatisticReceived
}

func completedDungeonSettlementEntryReady(runtime *runtimeDungeonState, characterID uint16) bool {
	if runtime == nil || runtime.Session == nil || !dungeonRuntimeOwnsCharacter(runtime, characterID) ||
		!runtime.settlementEntrySent {
		return false
	}
	return runtime.Session.Snapshot().Run.Status == worldmap.DungeonRunCompleted
}
