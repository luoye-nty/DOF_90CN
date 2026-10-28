package dnfbridge

import "longheng.io/server/internal/modules/dnf/dnfenum"

const (
	currentDungeonPlayResultClientRankPointOffset = 10
	currentDungeonCharacterStateMsgID             = uint16(dnfenum.CmdPacketFinishLoading)
)

// sendCurrentDungeonSettlementResultsLocked sends only a previously frozen,
// committed plan. It never calculates or fills a reward while handling op46.
// The caller owns session.dungeon.mu.
func (s *Service) sendCurrentDungeonSettlementResultsLocked(
	session *gameSession,
	runtime *runtimeDungeonState,
	source string,
) error {
	if session == nil || runtime == nil || session.dungeon.runtime != runtime ||
		!runtime.settlementPlayResultReceived ||
		runtime.settlementPhase < currentDungeonSettlementPhaseClearEnabled ||
		runtime.settlementPhase >= currentDungeonSettlementPhaseEnding {
		return nil
	}
	plan := runtime.settlementResultPlan
	if plan == nil {
		s.logGameEvent(session, "game-dungeon-settlement-results-blocked",
			"char_id", session.selectedCharacterID,
			"dungeon_id", runtime.Dungeon.ID,
			"source", source,
			"reason", "committed_dungeon_reward_snapshot_missing")
		return nil
	}
	if plan.CharacterID != session.selectedCharacterID ||
		!dungeonRuntimeOwnsCharacter(runtime, plan.CharacterID) ||
		plan.CompletionKey == "" || plan.Source == "" ||
		(runtime.clearMapCompletionKey != "" && plan.CompletionKey != runtime.clearMapCompletionKey) {
		s.logGameEvent(session, "game-dungeon-settlement-results-blocked",
			"char_id", session.selectedCharacterID,
			"plan_char_id", plan.CharacterID,
			"dungeon_id", runtime.Dungeon.ID,
			"plan_completion_key", plan.CompletionKey,
			"runtime_completion_key", runtime.clearMapCompletionKey,
			"source", source,
			"reason", "committed_settlement_plan_owner_mismatch")
		return nil
	}
	if len(runtime.settlementPlayResultBody) <= currentDungeonPlayResultClientRankPointOffset ||
		runtime.settlementPlayResultBody[currentDungeonPlayResultClientRankPointOffset] != plan.ClientRankPoint {
		s.logGameEvent(session, "game-dungeon-settlement-results-blocked",
			"char_id", session.selectedCharacterID,
			"dungeon_id", runtime.Dungeon.ID,
			"request_body_len", len(runtime.settlementPlayResultBody),
			"planned_client_rank_point", plan.ClientRankPoint,
			"source", source,
			"reason", "op46_client_rank_point_conflicts_with_committed_plan")
		return nil
	}
	if len(plan.PlayResultBody) == 0 || len(plan.CharacterBody) != currentFinishLoadingCharacterStateBodySize ||
		len(plan.ClearRewardBody) < currentDungeonClearRewardMinimumBodySize ||
		!currentDungeonTowerPlanValid(runtime, plan) {
		s.logGameEvent(session, "game-dungeon-settlement-results-blocked",
			"char_id", session.selectedCharacterID,
			"dungeon_id", runtime.Dungeon.ID,
			"op34_body_len", len(plan.PlayResultBody),
			"op37_body_len", len(plan.CharacterBody),
			"op35_body_len", len(plan.ClearRewardBody),
			"source", source,
			"reason", "frozen_settlement_packet_plan_shape_invalid")
		return nil
	}

	if !runtime.settlementClearRewardSent && plan.TowerRewardMsgID == 0 {
		ready, err := s.prepareRuntimePartyDungeonCardRewardsLocked(session, runtime)
		if err != nil || !ready {
			return err
		}
	}
	if !runtime.settlementResultNoticeSent {
		if err := s.sendGameUpperRawClass(session, currentDungeonPlayResultNoticeMsgID, plan.PlayResultBody, 0); err != nil {
			return err
		}
		runtime.settlementResultNoticeSent = true
		s.logGameEvent(session, "game-dungeon-settlement-result-op34-sent",
			"char_id", plan.CharacterID,
			"dungeon_id", runtime.Dungeon.ID,
			"completion_key", plan.CompletionKey,
			"body_len", len(plan.PlayResultBody),
			"body_source", "current_exe_sub_1D3BAE0_committed_runtime_result")
	}
	if !runtime.settlementCharacterStateSent {
		if err := s.sendGameUpperRawClass(session, currentDungeonCharacterStateMsgID, plan.CharacterBody, 0); err != nil {
			return err
		}
		runtime.settlementCharacterStateSent = true
		s.logGameEvent(session, "game-dungeon-settlement-character-op37-sent",
			"char_id", plan.CharacterID,
			"dungeon_id", runtime.Dungeon.ID,
			"completion_key", plan.CompletionKey,
			"body_len", len(plan.CharacterBody),
			"body_source", "current_exe_sub_1D78240_post_commit_character_snapshot")
	}
	if !runtime.settlementClearRewardSent {
		if err := s.sendGameUpperRawClass(session, currentDungeonClearRewardMsgID, plan.ClearRewardBody, 0); err != nil {
			return err
		}
		runtime.settlementClearRewardSent = true
		s.logGameEvent(session, "game-dungeon-settlement-clear-reward-op35-sent",
			"char_id", plan.CharacterID,
			"dungeon_id", runtime.Dungeon.ID,
			"completion_key", plan.CompletionKey,
			"reward_source", plan.Source,
			"body_len", len(plan.ClearRewardBody),
			"body_source", "current_exe_sub_1D4D380_committed_reward_snapshot")
		// 同公会成员组队通关地下城，可以获得1点公会贡献 (help text 76513).
		// Paused on the user's request (2026-09-12): only 签到 pays 公会贡献 for now.
		// Re-enable by uncommenting; the settlement is per character, so each guild
		// member of the party would be paid by its own clear and never twice for one
		// run.
		// s.grantCurrentGuildPartyMileage(session, "current_dungeon_clear")
	}
	// 公会贡献度 coins delivered by this clear. Sent once, before the wedding
	// capsule and tower rows, which is where upstream places it. The guard matters
	// because the result send can run more than once for one settlement.
	if !runtime.guildContributionSent {
		if err := s.sendCurrentGuildContributionUpdates(session, runtime.guildContributionUpdates); err != nil {
			return err
		}
		runtime.guildContributionSent = true
	}
	if len(plan.WeddingCapsuleBody) != 0 && !runtime.settlementWeddingCapsuleSent {
		// 1016 号：已婚角色通关推荐副本后回婚姻成长胶囊进度。只发一次；
		// 发送失败不置位，下一次结算会重试（进度取自已提交的角色记录，
		// 重发不会重复计入）。
		if err := s.sendGameUpperRawClass(session, 1016, plan.WeddingCapsuleBody, 0); err != nil {
			return err
		}
		runtime.settlementWeddingCapsuleSent = true
	}
	if plan.TowerRewardMsgID != 0 && !runtime.settlementTowerRewardSent {
		// The two daily pools are consumed by this clear; republish both native
		// counters before the window so the tooltip agrees with the ledger.
		if !runtime.settlementTowerCountsSent {
			ctx, cancel := s.currentGuildContext()
			err := s.sendCurrentAdventureTowerCounts(ctx, session, "tower_clear_committed")
			cancel()
			if err != nil {
				return err
			}
			runtime.settlementTowerCountsSent = true
		}
		if !runtime.settlementTowerInventorySent {
			if len(runtime.towerRewardUpdates) != 0 {
				if err := s.sendGameUpperRawClass(session, 14, buildCurrentItemUpdateBody(0, runtime.towerRewardUpdates), 0); err != nil {
					return err
				}
			}
			runtime.settlementTowerInventorySent = true
		}
		if err := s.sendGameUpperRawClass(session, plan.TowerRewardMsgID, plan.TowerRewardBody, 0); err != nil {
			return err
		}
		runtime.settlementTowerRewardSent = true
		s.logGameEvent(session, "game-dungeon-tower-clear-reward-sent",
			"char_id", plan.CharacterID, "dungeon_id", runtime.Dungeon.ID,
			"completion_key", plan.CompletionKey, "msg_id", plan.TowerRewardMsgID,
			"body_len", len(plan.TowerRewardBody),
			"body_source", "native_ten_reward_slots_with_committed_tower_rewards_and_empty_sentinels")
	}
	if len(plan.DungeonPermissionBody) != 0 && !runtime.settlementDungeonPermissionSent {
		if err := s.sendGameUpperRawClass(session, currentDungeonResourceStateMsgID, plan.DungeonPermissionBody, 0); err != nil {
			return err
		}
		runtime.settlementDungeonPermissionSent = true
		s.logGameEvent(session, "game-dungeon-settlement-permission-op5-sent",
			"char_id", plan.CharacterID,
			"dungeon_id", runtime.Dungeon.ID,
			"completion_key", plan.CompletionKey,
			"body_len", len(plan.DungeonPermissionBody),
			"body_source", "current_exe_sub_1D37AC0_u16_count_u32_key_u8_state",
			"state_source", "persisted_dungeon_clear_state_after_settlement")
	}
	runtime.advanceSettlementPhase(currentDungeonSettlementPhaseResultShown)
	// Publish the lucky-star result committed with this settlement.
	s.sendSuitableDungeonLuckyStarState(session, runtime)
	return nil
}
