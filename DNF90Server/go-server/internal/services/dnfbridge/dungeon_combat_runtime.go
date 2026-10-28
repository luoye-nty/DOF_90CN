package dnfbridge

import (
	"context"
	"fmt"

	"longheng.io/server/internal/modules/dnf/dnfenum"
	"longheng.io/server/internal/modules/dnf/dungeoncmd"
	dnfhellparty "longheng.io/server/internal/modules/dnf/hellparty"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	"longheng.io/server/internal/modules/dnf/worldmap"
)

// Friendly PVF APCs can follow the player into later rooms while retaining
// the object key announced by their original room.
func dungeonRuntimeContainsDeathOwnerObjectKey(runtime *runtimeDungeonState, objectKey uint32) bool {
	if runtime == nil || runtime.Room == nil || objectKey == 0 {
		return false
	}
	if runtime.Room.ContainsAnnouncedActorObjectKey(objectKey) {
		return true
	}
	for _, visit := range runtime.Rooms {
		if visit == nil || visit.Room == nil || visit.Room == runtime.Room {
			continue
		}
		if visit.Room.ContainsAnnouncedNonHostileAICharacterObjectKey(objectKey) {
			return true
		}
	}
	return false
}

// isHostileAnnouncedKiller reports whether the killer object key names a
// hostile actor of the current room (an ordinary monster or an extended actor
// carrying a hostile reference). The current EXE reports summons, pets and
// creatures with dynamic keys the server never announced; those are not
// hostile and must not be mistaken for a forged killer.
func isHostileAnnouncedKiller(room *runtimeDungeonRoom, objectKey uint32) bool {
	if room == nil || objectKey == 0 {
		return false
	}
	room.mu.RLock()
	defer room.mu.RUnlock()
	if _, ok := room.byObjectKey[objectKey]; ok {
		return true
	}
	if index, ok := room.extendedByObjectKey[objectKey]; ok && index >= 0 && index < len(room.extendedActors) {
		if room.extendedActors[index].HostileReference != nil {
			return true
		}
	}
	return false
}

func (s *Service) handleDungeonMonsterDeath(session *gameSession, body []byte) error {
	return s.executeRuntimePartyDungeonMonsterDeath(session, append([]byte(nil), body...))
}

func (s *Service) handleDungeonMonsterDeathLocal(session *gameSession, body []byte) error {
	request, err := dungeoncmd.DecodeDieMonsterRequest(body)
	if err != nil {
		s.logDungeonStallBlocked(session, dungeonStallKindMonsterDeath,
			"game-dungeon-monster-death-blocked", 0,
			"body_len", len(body),
			"reason", "current_exe_op39_request_malformed",
			"error", err)
		return nil
	}
	if len(request.OpaqueTail) != 0 {
		s.logDungeonStallBlocked(session, dungeonStallKindMonsterDeath,
			"game-dungeon-monster-death-blocked", request.RuntimeObjectKey,
			"object_key", request.RuntimeObjectKey,
			"body_len", len(body),
			"tail_len", len(request.OpaqueTail),
			"reason", "current_exe_op39_request_tail_unproven")
		return nil
	}
	if session == nil {
		return nil
	}
	session.dungeon.mu.Lock()
	defer session.dungeon.mu.Unlock()
	runtime := session.dungeon.runtime
	if runtime == nil || runtime.Session == nil || runtime.Room == nil {
		s.logDungeonStallBlocked(session, dungeonStallKindMonsterDeath,
			"game-dungeon-monster-death-blocked", request.RuntimeObjectKey,
			"object_key", request.RuntimeObjectKey,
			"reason", "dungeon_runtime_missing")
		return nil
	}
	scene, hasScene := runtime.Session.Scene()
	// The current EXE's altar controller uses the variable op39 writer for
	// dynamic actors, but its combat-row count and owner vary with the native
	// controller path. The decoded boundary, empty opaque tail (checked above),
	// and an exact current-room altar sequence are the fail-closed authority.
	if hasScene && request.Layout == dungeoncmd.DieMonsterRequestLayoutVariableCombat {
		if sequence, altarOwned := currentDungeonAltarDynamicDeathSequenceLocked(
			runtime,
			scene,
			request.RuntimeObjectKey,
		); altarOwned {
			return s.handleCurrentDungeonAltarDeathsLocked(
				session,
				runtime,
				scene,
				[]uint16{sequence},
				"client_op39_dynamic_death",
				true,
			)
		}
	}
	if runtime.HellParty != nil && runtime.HellParty.Runtime != nil && hasScene &&
		scene.Coordinate == runtime.HellParty.Room.Coordinate && scene.Map.Map.ID == runtime.HellParty.Room.HellMap.ID {
		actor, known := runtime.HellParty.Actors[request.RuntimeObjectKey]
		if known {
			if cached := runtime.HellParty.DeathResponses[request.RuntimeObjectKey]; len(cached) != 0 {
				return s.sendGameUpperRawClass(session, uint16(dnfenum.CmdPacketNotifyDieMonster), append([]byte(nil), cached...), 0)
			}
			playerObjectKey := currentSceneActorObjectKey(session.selectedCharacterID)
			if request.OwnerObjectKey != playerObjectKey && request.OwnerObjectKey != ^uint16(0) &&
				!dungeonRuntimeContainsDeathOwnerObjectKey(runtime, uint32(request.OwnerObjectKey)) &&
				!runtimePartyDungeonContainsActor(runtime, request.OwnerObjectKey) {
				return nil
			}
			if !runtime.Room.ContainsAnnouncedActorObjectKey(request.RuntimeObjectKey) {
				return nil
			}
			if runtime.HellParty.Runtime.Phase() == dnfhellparty.PhaseWaitingStart {
				if err := runtime.HellParty.Runtime.Start(); err != nil {
					return err
				}
			}
			result, err := runtime.HellParty.Runtime.RecordDeath(actor.Identity)
			if err != nil {
				return nil
			}
			target, matched, err := runtime.Room.RetireHellPartyActor(request.RuntimeObjectKey)
			if err != nil || !matched {
				return err
			}
			var drops []currentDungeonDeathDropWire
			var selected []currentDungeonHellPartySelectedDrop
			if result.GroupCompleted && !result.RewardSuppressed && result.RewardRollCount > 0 {
				drops, selected, err = s.planCurrentDungeonHellPartyDrops(runtime, actor, playerObjectKey, result.RewardRollCount)
				if err != nil {
					s.logGameEvent(session, "game-dungeon-hell-party-drop-plan-failed",
						"dungeon_id", runtime.Dungeon.ID,
						"room", scene.Coordinate.String(),
						"map_id", scene.Map.Map.ID,
						"object_key", request.RuntimeObjectKey,
						"actor_code", actor.Code,
						"actor_level", actor.Level,
						"reward_roll_count", result.RewardRollCount,
						"error", err)
					drops, selected = nil, nil
				}
			}
			response, err := buildCurrentDungeonDeathNotificationBodyWithDrops(target.ObjectKey, target.ResponseKind, drops)
			if err != nil {
				return err
			}
			runtime.HellParty.DeathResponses[request.RuntimeObjectKey] = append([]byte(nil), response...)
			if err := runtime.cacheCurrentDungeonRoom(); err != nil {
				return err
			}
			if err := s.sendGameUpperRawClass(session, uint16(dnfenum.CmdPacketNotifyDieMonster), response, 0); err != nil {
				return err
			}
			s.logGameEvent(session, "game-dungeon-hell-party-actor-death-accepted",
				"dungeon_id", runtime.Dungeon.ID,
				"room", scene.Coordinate.String(),
				"map_id", scene.Map.Map.ID,
				"object_key", request.RuntimeObjectKey,
				"actor_code", actor.Code,
				"group_completed", result.GroupCompleted,
				"all_complete", result.AllComplete,
				"reward_roll_count", result.RewardRollCount,
				"reward_suppressed", result.RewardSuppressed,
				"drop_count", len(drops),
				"selected_drop_count", len(selected))
			return nil
		}
	}
	if runtime.tournament != nil && hasScene {
		if request.RuntimeObjectKey == 0 || request.RuntimeObjectKey > uint32(^uint16(0)) {
			s.logGameEvent(session, "game-tournament-actor-death-blocked",
				"dungeon_id", runtime.Dungeon.ID,
				"object_key", request.RuntimeObjectKey,
				"reason", "tournament_actor_object_key_out_of_range")
			return nil
		}
		actor, actorKnown := runtime.tournament.ActorForSequence(uint16(request.RuntimeObjectKey))
		if actorKnown {
			playerObjectKey := currentSceneActorObjectKey(session.selectedCharacterID)
			if request.OwnerObjectKey != playerObjectKey {
				s.logGameEvent(session, "game-tournament-actor-death-blocked",
					"dungeon_id", runtime.Dungeon.ID,
					"maze_index", runtime.MazeIndex,
					"room", scene.Coordinate.String(),
					"map_id", scene.Map.Map.ID,
					"object_key", request.RuntimeObjectKey,
					"related_actor_object_key", request.OwnerObjectKey,
					"player_object_key", playerObjectKey,
					"reason", "tournament_owner_must_be_current_player")
				return nil
			}
			if !runtime.Room.ContainsAnnouncedActorObjectKey(request.RuntimeObjectKey) {
				s.logGameEvent(session, "game-tournament-actor-death-blocked",
					"dungeon_id", runtime.Dungeon.ID,
					"object_key", request.RuntimeObjectKey,
					"reason", "tournament_actor_not_announced")
				return nil
			}
			eventID := fmt.Sprintf("tournament:%d:%d:%d:%d:%d:%d:%d",
				session.selectedCharacterID, runtime.Dungeon.ID, runtime.MazeIndex,
				scene.Coordinate.X, scene.Coordinate.Y, runtime.Seed, request.RuntimeObjectKey)
			transition := runtime.tournament.ApplyActorDeath(eventID, uint16(request.RuntimeObjectKey))
			if transition.Disposition == worldmap.TournamentDeathDuplicate {
				s.logGameEvent(session, "game-tournament-actor-death-duplicate",
					"dungeon_id", runtime.Dungeon.ID, "object_key", request.RuntimeObjectKey, "event_id", eventID)
				return nil
			}
			if transition.Disposition == worldmap.TournamentDeathRejected {
				s.logGameEvent(session, "game-tournament-actor-death-blocked",
					"dungeon_id", runtime.Dungeon.ID, "object_key", request.RuntimeObjectKey,
					"current_round", transition.CurrentRound, "reason", "tournament_runtime_rejected_death", "event_id", eventID)
				return nil
			}
			responseKind := currentTournamentDeathResponseKind(actor)
			target, retireErr := runtime.Room.RetireTournamentActor(request.RuntimeObjectKey, responseKind)
			if retireErr != nil {
				s.logGameEvent(session, "game-tournament-actor-death-blocked",
					"dungeon_id", runtime.Dungeon.ID, "object_key", request.RuntimeObjectKey,
					"reason", "tournament_room_actor_retirement_failed", "error", retireErr, "event_id", eventID)
				return nil
			}
			responseBody, buildErr := buildCurrentDungeonDeathNotificationBody(target.ObjectKey, target.ResponseKind)
			if buildErr != nil {
				return buildErr
			}
			if sendErr := s.sendGameUpperRawClass(session, uint16(dnfenum.CmdPacketNotifyDieMonster), responseBody, 0); sendErr != nil {
				return sendErr
			}
			if cacheErr := runtime.cacheCurrentDungeonRoom(); cacheErr != nil {
				return fmt.Errorf("cache tournament actor death: %w", cacheErr)
			}
			if transition.Disposition == worldmap.TournamentCompleted {
				if rewardErr := s.sendCurrentTournamentClearRewardLocked(session, runtime); rewardErr != nil {
					return fmt.Errorf("send tournament clear reward: %w", rewardErr)
				}
			}
			return nil
		}
	}
	if hasScene {
		handled, handleErr := s.handleCurrentFruzStationNarkeObstacleDeathLocked(
			session,
			runtime,
			scene,
			request,
		)
		if handleErr != nil {
			return handleErr
		}
		if handled {
			return nil
		}
	}
	ownerIsSentinel := request.OwnerObjectKey == ^uint16(0)
	var scriptedTutorialMonster runtimeDungeonMonster
	var scriptedTutorialEvidence dungeonTutorialScriptEvidence
	scriptedTutorialDeath := false
	ownerSentinelOrdinaryTarget := false
	ownerSentinelHostileAITarget := false
	ownerSentinelBlockingClearTarget := false
	ownerSentinelBlockingClearSource := ""
	ownerSentinelConsumedLayerBaseBossTarget := false
	ownerSentinelConsumedLayerBaseBossSource := ""
	if ownerIsSentinel {
		if request.Layout != dungeoncmd.DieMonsterRequestLayoutVariableCombat {
			s.logDungeonStallBlocked(session, dungeonStallKindMonsterDeath,
				"game-dungeon-monster-death-blocked", request.RuntimeObjectKey,
				"dungeon_id", runtime.Dungeon.ID,
				"maze_index", runtime.MazeIndex,
				"object_key", request.RuntimeObjectKey,
				"related_actor_object_key", request.OwnerObjectKey,
				"request_layout", request.Layout,
				"combat_entry_count", request.CombatEntryCount,
				"reason", "owner_sentinel_request_layout_unproven")
			return nil
		}
		if hasScene {
			scriptedTutorialMonster, scriptedTutorialEvidence, scriptedTutorialDeath = s.isPVFTutorialScriptedMonsterDeath(
				runtime,
				scene,
				request.RuntimeObjectKey,
			)
			if !scriptedTutorialDeath {
				_, ownerSentinelOrdinaryTarget = currentDungeonOwnerSentinelAnnouncedOrdinaryMonster(
					runtime,
					scene,
					request.RuntimeObjectKey,
				)
				_, ownerSentinelBlockingClearSource, ownerSentinelBlockingClearTarget =
					currentDungeonOwnerSentinelBlockingClearTarget(runtime, scene, request.RuntimeObjectKey)
				if !ownerSentinelBlockingClearTarget {
					_, ownerSentinelConsumedLayerBaseBossSource, ownerSentinelConsumedLayerBaseBossTarget =
						currentDungeonOwnerSentinelConsumedLayerBaseBossTarget(runtime, scene, request.RuntimeObjectKey)
					if !ownerSentinelConsumedLayerBaseBossTarget && !ownerSentinelOrdinaryTarget {
						ownerSentinelHostileAITarget = currentDungeonOwnerSentinelAnnouncedHostileAI(
							runtime,
							scene,
							request.RuntimeObjectKey,
						)
					}
				}
			}
		}
		combatShapeProven := request.CombatEntryCount == 0 ||
			(request.CombatEntryCount == 1 && (ownerSentinelOrdinaryTarget || ownerSentinelHostileAITarget))
		if !combatShapeProven {
			s.logDungeonStallBlocked(session, dungeonStallKindMonsterDeath,
				"game-dungeon-monster-death-blocked", request.RuntimeObjectKey,
				"dungeon_id", runtime.Dungeon.ID,
				"maze_index", runtime.MazeIndex,
				"object_key", request.RuntimeObjectKey,
				"related_actor_object_key", request.OwnerObjectKey,
				"request_layout", request.Layout,
				"combat_entry_count", request.CombatEntryCount,
				"owner_sentinel_ordinary_target", ownerSentinelOrdinaryTarget,
				"owner_sentinel_hostile_ai_target", ownerSentinelHostileAITarget,
				"reason", "owner_sentinel_combat_shape_unproven")
			return nil
		}
	}
	if ownerIsSentinel && !scriptedTutorialDeath && !ownerSentinelBlockingClearTarget &&
		!ownerSentinelConsumedLayerBaseBossTarget && !ownerSentinelOrdinaryTarget && !ownerSentinelHostileAITarget {
		target, retirementErr := runtime.Room.RetireNonHostileAIActor(request.RuntimeObjectKey)
		if retirementErr != nil {
			if hasScene {
				handled, clearErr := s.handleCurrentDungeonDestroyObjectClearConditionDeathLocked(
					session,
					runtime,
					scene,
					request.RuntimeObjectKey,
				)
				if clearErr != nil {
					return clearErr
				}
				if handled {
					s.cancelCurrentDungeonReturnAfterDungeonEvidenceLocked(
						session,
						runtime,
						"accepted_destroy_object_clear_condition_op39_after_pending_op24",
					)
					return nil
				}
				handled, rewardErr := s.handleCurrentDungeonQuestPassiveObjectRewardDeathLocked(
					session,
					runtime,
					scene,
					request.RuntimeObjectKey,
				)
				if rewardErr != nil {
					s.logGameEvent(session, "game-dungeon-quest-passive-object-reward-deferred",
						"dungeon_id", runtime.Dungeon.ID,
						"maze_index", runtime.MazeIndex,
						"object_key", request.RuntimeObjectKey,
						"related_actor_object_key", request.OwnerObjectKey,
						"reason", "active_pvf_enemy_reward_item_type3_failed",
						"error", rewardErr)
					return nil
				}
				if handled {
					s.cancelCurrentDungeonReturnAfterDungeonEvidenceLocked(
						session,
						runtime,
						"accepted_quest_passive_object_reward_op39_after_pending_op24",
					)
					return nil
				}
				handled, passiveErr := s.handleCurrentDungeonQuestPassiveObjectDeathLocked(
					session,
					runtime,
					scene,
					request.RuntimeObjectKey,
				)
				if passiveErr != nil {
					s.logGameEvent(session, "game-dungeon-quest-passive-object-death-deferred",
						"dungeon_id", runtime.Dungeon.ID,
						"maze_index", runtime.MazeIndex,
						"object_key", request.RuntimeObjectKey,
						"related_actor_object_key", request.OwnerObjectKey,
						"reason", "unique_active_pvf_passive_target_owner_failed",
						"error", passiveErr)
					return nil
				}
				if handled {
					s.cancelCurrentDungeonReturnAfterDungeonEvidenceLocked(
						session,
						runtime,
						"accepted_quest_passive_object_op39_after_pending_op24",
					)
					return nil
				}
			}
			s.logDungeonStallBlocked(session, dungeonStallKindMonsterDeath,
				"game-dungeon-monster-death-blocked", request.RuntimeObjectKey,
				"dungeon_id", runtime.Dungeon.ID,
				"maze_index", runtime.MazeIndex,
				"object_key", request.RuntimeObjectKey,
				"related_actor_object_key", request.OwnerObjectKey,
				"request_layout", request.Layout,
				"reason", "non_hostile_ai_retirement_rejected",
				"error", retirementErr)
			return nil
		}
		responseBody, err := buildCurrentDungeonDeathNotificationBody(
			target.ObjectKey,
			target.ResponseKind,
		)
		if err != nil {
			return err
		}
		if err := s.sendGameUpperRawClass(session, uint16(dnfenum.CmdPacketNotifyDieMonster), responseBody, 0); err != nil {
			return err
		}
		scene, _ = runtime.Session.Scene()
		s.logGameEvent(session, "game-dungeon-non-hostile-ai-retired",
			"dungeon_id", runtime.Dungeon.ID,
			"maze_index", runtime.MazeIndex,
			"room", scene.Coordinate.String(),
			"map_id", scene.Map.Map.ID,
			"object_key", request.RuntimeObjectKey,
			"related_actor_object_key", request.OwnerObjectKey,
			"request_layout", request.Layout,
			"combat_entry_count", request.CombatEntryCount,
			"response_kind", target.ResponseKind,
			"response_opcode", uint16(dnfenum.CmdPacketNotifyDieMonster),
			"response_body_len", len(responseBody),
			"room_cleared", scene.Cleared,
			"boss_room", scene.Boss,
			"reward_assets", "unchanged",
			"response", "current_exe_op38_non_hostile_ai_retirement_ack")
		s.cancelCurrentDungeonReturnAfterDungeonEvidenceLocked(
			session,
			runtime,
			"accepted_non_hostile_ai_op39_after_pending_op24",
		)
		return nil
	}
	if ownerSentinelConsumedLayerBaseBossTarget {
		target, _, reportErr := runtime.Room.CommitActorDeathReport(request.RuntimeObjectKey, runtime.Session)
		if reportErr != nil {
			s.logDungeonStallBlocked(session, dungeonStallKindMonsterDeath,
				"game-dungeon-monster-death-blocked", request.RuntimeObjectKey,
				"dungeon_id", runtime.Dungeon.ID,
				"maze_index", runtime.MazeIndex,
				"object_key", request.RuntimeObjectKey,
				"reason", "consumed_layer_base_boss_retirement_rejected",
				"error", reportErr)
			return nil
		}
		responseBody, buildErr := buildCurrentDungeonDeathNotificationBody(target.ObjectKey, target.ResponseKind)
		if buildErr != nil {
			return buildErr
		}
		if err := s.sendGameUpperRawClass(session, uint16(dnfenum.CmdPacketNotifyDieMonster), responseBody, 0); err != nil {
			return err
		}
		if err := runtime.cacheCurrentDungeonRoom(); err != nil {
			return fmt.Errorf("cache consumed layered base room after dummy boss retirement: %w", err)
		}
		scene, _ = runtime.Session.Scene()
		s.logGameEvent(session, "game-dungeon-consumed-layer-base-boss-retired",
			"dungeon_id", runtime.Dungeon.ID,
			"maze_index", runtime.MazeIndex,
			"room", scene.Coordinate.String(),
			"map_id", scene.Map.Map.ID,
			"object_key", request.RuntimeObjectKey,
			"related_actor_object_key", request.OwnerObjectKey,
			"request_layout", request.Layout,
			"combat_entry_count", request.CombatEntryCount,
			"completion_source", ownerSentinelConsumedLayerBaseBossSource,
			"room_cleared", scene.Cleared,
			"reward_assets", "unchanged",
			"response_opcode", uint16(dnfenum.CmdPacketNotifyDieMonster),
			"response_body_len", len(responseBody),
			"next_stage", "ordinary_final_room_settlement")
		if !scene.Cleared {
			return nil
		}
		if err := s.completeCurrentDungeonOrdinaryFinalRoomAfterDeathLocked(
			session,
			runtime,
			scene,
			request.RuntimeObjectKey,
		); err != nil {
			return err
		}
		s.cancelCurrentDungeonReturnAfterDungeonEvidenceLocked(
			session,
			runtime,
			"accepted_consumed_layer_base_boss_op39_after_pending_op24",
		)
		return nil
	}
	playerObjectKey := currentSceneActorObjectKey(session.selectedCharacterID)
	// Male Priest tutorial 7113 proves a second native non-hostile APC death
	// shape: variable op39, one combat row, and owner=current player. The older
	// tutorial captures used owner=0xffff and are handled above. Keep this
	// compatibility path limited to the currently owned PVF tutorial scene and
	// to the exact player-owned request shape; RetireNonHostileAIActor then
	// proves that the target is an announced/retired, non-blocking character AI
	// with no hostile reference. It only retires the visual APC and never grants
	// EXP/drops or changes hostile room-clear state.
	if hasScene &&
		isPVFTutorialDungeonScene(runtime, scene) &&
		request.Layout == dungeoncmd.DieMonsterRequestLayoutVariableCombat &&
		request.CombatEntryCount == 1 &&
		request.OwnerObjectKey == playerObjectKey {
		target, retirementErr := runtime.Room.RetireNonHostileAIActor(request.RuntimeObjectKey)
		if retirementErr == nil {
			responseBody, buildErr := buildCurrentDungeonDeathNotificationBody(
				target.ObjectKey,
				target.ResponseKind,
			)
			if buildErr != nil {
				return buildErr
			}
			if sendErr := s.sendGameUpperRawClass(
				session,
				uint16(dnfenum.CmdPacketNotifyDieMonster),
				responseBody,
				0,
			); sendErr != nil {
				return sendErr
			}
			scene, _ = runtime.Session.Scene()
			s.logGameEvent(session, "game-dungeon-non-hostile-ai-retired",
				"dungeon_id", runtime.Dungeon.ID,
				"maze_index", runtime.MazeIndex,
				"room", scene.Coordinate.String(),
				"map_id", scene.Map.Map.ID,
				"object_key", request.RuntimeObjectKey,
				"related_actor_object_key", request.OwnerObjectKey,
				"request_layout", request.Layout,
				"combat_entry_count", request.CombatEntryCount,
				"response_kind", target.ResponseKind,
				"response_opcode", uint16(dnfenum.CmdPacketNotifyDieMonster),
				"response_body_len", len(responseBody),
				"room_cleared", scene.Cleared,
				"boss_room", scene.Boss,
				"reward_assets", "unchanged",
				"response", "current_exe_op38_player_owned_tutorial_ai_retirement_ack")
			s.cancelCurrentDungeonReturnAfterDungeonEvidenceLocked(
				session,
				runtime,
				"accepted_player_owned_tutorial_ai_op39_after_pending_op24",
			)
			return nil
		}
	}
	if !scriptedTutorialDeath && !ownerSentinelBlockingClearTarget && !ownerSentinelOrdinaryTarget &&
		!ownerSentinelHostileAITarget &&
		request.OwnerObjectKey != playerObjectKey {
		if isHostileAnnouncedKiller(runtime.Room, uint32(request.OwnerObjectKey)) {
			// A hostile room actor can never be the killer of another monster.
			s.logDungeonStallBlocked(session, dungeonStallKindMonsterDeath,
				"game-dungeon-monster-death-blocked", request.RuntimeObjectKey,
				"object_key", request.RuntimeObjectKey,
				"related_actor_object_key", request.OwnerObjectKey,
				"player_object_key", playerObjectKey,
				"request_layout", request.Layout,
				"reason", "room_related_actor_object_key_mismatch",
				"error", errDungeonMonsterOwnerMismatch)
			return nil
		}
		if !dungeonRuntimeContainsDeathOwnerObjectKey(runtime, uint32(request.OwnerObjectKey)) &&
			!runtimePartyDungeonContainsActor(runtime, request.OwnerObjectKey) {
			// Friendly APCs that followed from an earlier room and party members
			// are known killers and pass silently above. Everything else follows
			// the 90US alignment: the victim is the authority (CommitActorDeathReport);
			// the killer key is display-only. Summons, pets, creatures and quest APC
			// dynamic keys (even far keys) are reported here by the current EXE and
			// were never announced by the server. usdof_v54 opcode0027 die_monster
			// (seq-gated) and the 86JP DungeonCombatHandler / DungeonKillApplicationService
			// never validate the killer, so any non-hostile killer is allowed.
			s.logGameEvent(session, "game-dungeon-monster-death-killer-allowed-non-hostile",
				"object_key", request.RuntimeObjectKey,
				"related_actor_object_key", request.OwnerObjectKey,
				"player_object_key", playerObjectKey,
				"request_layout", request.Layout,
				"reason", "killer_not_player_nor_friendly_apc_allowed")
		}
	}
	if target, matchedSpecialMonster, retirementErr := runtime.Room.RetireSpecialMonsterActor(request.RuntimeObjectKey); matchedSpecialMonster {
		if retirementErr != nil {
			s.logDungeonStallBlocked(session, dungeonStallKindMonsterDeath,
				"game-dungeon-monster-death-blocked", request.RuntimeObjectKey,
				"dungeon_id", runtime.Dungeon.ID,
				"maze_index", runtime.MazeIndex,
				"object_key", request.RuntimeObjectKey,
				"related_actor_object_key", request.OwnerObjectKey,
				"request_layout", request.Layout,
				"combat_entry_count", request.CombatEntryCount,
				"reason", "special_monster_retirement_rejected",
				"error", retirementErr)
			return nil
		}
		if err := s.completeCurrentSuspiciousVillageElevatorLocked(session, runtime, target.ObjectKey); err != nil {
			return err
		}
		responseBody, err := buildCurrentDungeonDeathNotificationBody(
			target.ObjectKey,
			target.ResponseKind,
		)
		if err != nil {
			return err
		}
		if err := s.sendGameUpperRawClass(session, uint16(dnfenum.CmdPacketNotifyDieMonster), responseBody, 0); err != nil {
			return err
		}
		if err := runtime.cacheCurrentDungeonRoom(); err != nil {
			return fmt.Errorf("cache dungeon room after special monster retirement: %w", err)
		}
		scene, _ = runtime.Session.Scene()
		s.logGameEvent(session, "game-dungeon-special-monster-retired",
			"dungeon_id", runtime.Dungeon.ID,
			"maze_index", runtime.MazeIndex,
			"room", scene.Coordinate.String(),
			"map_id", scene.Map.Map.ID,
			"object_key", request.RuntimeObjectKey,
			"related_actor_object_key", request.OwnerObjectKey,
			"request_layout", request.Layout,
			"combat_entry_count", request.CombatEntryCount,
			"response_kind", target.ResponseKind,
			"response_opcode", uint16(dnfenum.CmdPacketNotifyDieMonster),
			"response_body_len", len(responseBody),
			"room_cleared", scene.Cleared,
			"boss_room", scene.Boss,
			"reward_assets", "unchanged",
			"response", "current_exe_op38_special_monster_retirement_ack")
		s.cancelCurrentDungeonReturnAfterDungeonEvidenceLocked(
			session,
			runtime,
			"accepted_special_monster_op39_after_pending_op24",
		)
		return nil
	}
	dropMonster, ordinaryDropCandidate := runtime.Room.AnnouncedMonster(request.RuntimeObjectKey)
	questAICharacterCode, questAICharacterDropCandidate := runtime.Room.AnnouncedHostileAICharacterCode(
		request.RuntimeObjectKey,
	)
	bossActorDeath, bossActorType, bossActorSource := currentDungeonRuntimeObjectBossActor(
		runtime,
		request.RuntimeObjectKey,
		runtimeDungeonMonsterAnnounced,
	)
	clearConditionActorDeath := false
	clearConditionSource := ""
	if ordinaryDropCandidate {
		clearConditionSource = currentDungeonClearConditionSource(runtime, dropMonster)
		clearConditionActorDeath = clearConditionSource != ""
		if clearConditionActorDeath && !bossActorDeath {
			bossActorDeath = true
			bossActorType = 3
			bossActorSource = clearConditionSource
		}
	}
	target, cleared, reportErr := runtime.Room.CommitActorDeathReport(request.RuntimeObjectKey, runtime.Session)
	if reportErr != nil {
		s.logDungeonStallBlocked(session, dungeonStallKindMonsterDeath,
			"game-dungeon-monster-death-blocked", request.RuntimeObjectKey,
			"dungeon_id", runtime.Dungeon.ID,
			"maze_index", runtime.MazeIndex,
			"object_key", request.RuntimeObjectKey,
			"reason", "runtime_actor_owner_rejected",
			"error", reportErr)
		return nil
	}
	scene, _ = runtime.Session.Scene()
	storyAIBossGate, storyAIBossGateActive := currentDungeonStoryAIBossDeathGate(runtime, scene)
	pvfMultiBossGate, pvfMultiBossGateActive := currentDungeonPVFMultiBossDeathGate(runtime, scene)
	pvfMultiBossPending := pvfMultiBossGateActive && !pvfMultiBossGate.Ready
	rewardMonster := dropMonster
	rewardMonsterCandidate := ordinaryDropCandidate
	if !rewardMonsterCandidate {
		if monster, ok := runtime.Room.MonsterByReference(
			target.Reference,
			runtimeDungeonMonsterAnnounced,
			runtimeDungeonMonsterDefeated,
		); ok {
			rewardMonster = monster
			rewardMonsterCandidate = true
		}
	}
	if rewardMonsterCandidate && !clearConditionActorDeath {
		clearConditionSource = currentDungeonClearConditionSource(runtime, rewardMonster)
		clearConditionActorDeath = clearConditionSource != ""
		if clearConditionActorDeath && !bossActorDeath {
			bossActorDeath = true
			bossActorType = 3
			bossActorSource = clearConditionSource
		}
	}
	// The clear-card gold formula is driven by the run's weighted kill count,
	// so tally the death on the same gate that already decided it earns
	// rewards. Practice runs stay isolated from every reward source.
	if rewardMonsterCandidate && !currentDungeonRuntimeIsPractice(runtime) {
		runtime.recordCurrentDungeonClearKill(
			currentDungeonClearKillActorType(rewardMonster, bossActorDeath, bossActorType),
		)
	}
	var deathDrops []currentDungeonDeathDropWire
	dropSource := "none"
	if currentDungeonRuntimeIsPractice(runtime) {
		dropSource = "practice_zero_growth_isolation"
	} else if rewardMonsterCandidate {
		deathDrops, err = s.planCurrentDungeonMonsterDrops(runtime, rewardMonster, playerObjectKey)
		if err != nil {
			reason := "runtime_pvf_csharp_formula_drop_plan_failed_zero_drop_fallback"
			dropSource = "zero_drop_fallback"
			s.logGameEvent(session, "game-dungeon-monster-drop-deferred",
				"dungeon_id", runtime.Dungeon.ID,
				"maze_index", runtime.MazeIndex,
				"room", scene.Coordinate.String(),
				"map_id", scene.Map.Map.ID,
				"monster_id", rewardMonster.Spawn.MonsterID,
				"monster_object_key", request.RuntimeObjectKey,
				"logical_monster_object_key", rewardMonster.ObjectKey,
				"map_random_drop_count", rewardMonster.Spawn.RandomDropCount,
				"map_offset7_specified_pool_count", rewardMonster.Spawn.FixedDropCount,
				"reason", reason,
				"error", err)
			deathDrops = nil
		} else if len(deathDrops) != 0 {
			dropSource = "runtime_pvf_csharp_drop_formula_standard_item_branch"
		} else {
			dropSource = "runtime_pvf_csharp_drop_formula_no_standard_item"
		}
		if err == nil && currentDungeonStandaloneRiftRuntime(runtime) {
			riftDrops, selectedRiftDrops, riftDropErr := s.planCurrentDungeonStandaloneRiftBossDrops(
				runtime,
				rewardMonster,
				playerObjectKey,
			)
			if riftDropErr != nil {
				s.logGameEvent(session, "game-dungeon-standalone-rift-boss-drop-deferred",
					"dungeon_id", runtime.Dungeon.ID,
					"maze_index", runtime.MazeIndex,
					"room", scene.Coordinate.String(),
					"map_id", scene.Map.Map.ID,
					"monster_id", rewardMonster.Spawn.MonsterID,
					"monster_rank", rewardMonster.Spawn.Rank,
					"reason", "pvf_hell_drop_plan_failed_standard_drops_retained",
					"error", riftDropErr)
			} else if len(riftDrops) != 0 {
				deathDrops = append(deathDrops, riftDrops...)
				dropSource = "runtime_pvf_standard_plus_standalone_rift_boss"
				s.logGameEvent(session, "game-dungeon-standalone-rift-boss-drop-planned",
					"dungeon_id", runtime.Dungeon.ID,
					"maze_index", runtime.MazeIndex,
					"room", scene.Coordinate.String(),
					"map_id", scene.Map.Map.ID,
					"monster_id", rewardMonster.Spawn.MonsterID,
					"monster_rank", rewardMonster.Spawn.Rank,
					"drop_count", len(riftDrops),
					"selected_drops", selectedRiftDrops,
					"source", "pvf_inferno_or_particular_hell_plus_itemdropinfo_monster_hell")
			}
		}
	}
	var monsterExperience currentDungeonMonsterExperienceCommitResult
	var monsterExperienceBody []byte
	// monsterExperienceOp37Total is the value actually written into op37's
	// +0x01 field. At the level cap it collapses onto the durable EXP (see
	// currentMonsterKillOp37TotalExperience) so the client difference is zero
	// and it stops announcing a level-cap kill award.
	var monsterExperienceOp37Total uint32
	forcedBossClearCount := 0
	forcedBossVisualDeathCount := 0
	var stagedStoryForcedCombatEnd *currentDungeonForcedCombatEnd
	nextStoryCoordinate, nextStoryStageIndex, pendingStoryAdvance := currentDungeonPendingStoryAdvance(runtime, scene)
	if pendingStoryAdvance && bossActorDeath && !cleared && !storyAIBossGateActive && !pvfMultiBossPending &&
		!isPVFTutorialDungeonScene(runtime, scene) {
		prepared, prepareErr := s.prepareCurrentDungeonForcedCombatEndLocked(runtime, request.RuntimeObjectKey)
		if prepareErr != nil {
			s.logGameEvent(session, "game-dungeon-story-stage-force-clear-deferred",
				"dungeon_id", runtime.Dungeon.ID,
				"maze_index", runtime.MazeIndex,
				"room", scene.Coordinate.String(),
				"map_id", scene.Map.Map.ID,
				"story_stage_index", runtime.StoryStageIndex,
				"next_story_stage_index", nextStoryStageIndex,
				"next_story_room", nextStoryCoordinate.String(),
				"boss_object_key", request.RuntimeObjectKey,
				"reason", "story_stage_boss_forced_clear_prepare_failed",
				"error", prepareErr)
		} else {
			stagedStoryForcedCombatEnd = &prepared
			forcedBossClearCount = len(prepared.Targets)
			cleared = cleared || prepared.Cleared
			if prepared.SceneOK {
				scene = prepared.Scene
			}
		}
	}
	var optionalStoryRoom []worldmap.RoomCoordinate
	if pendingStoryAdvance && cleared && (!storyAIBossGateActive || storyAIBossGate.Ready) {
		optionalStoryRoom = append(optionalStoryRoom, nextStoryCoordinate)
	}
	responseBody, err := buildCurrentDungeonDeathNotificationBodyWithDrops(
		target.ObjectKey,
		target.ResponseKind,
		deathDrops,
		optionalStoryRoom...,
	)
	if err != nil {
		return err
	}
	if err := s.sendGameUpperRawClass(session, uint16(dnfenum.CmdPacketNotifyDieMonster), responseBody, 0); err != nil {
		return err
	}
	if err := s.publishCurrentDungeonBossEntranceGateAfterDeathLocked(session, runtime); err != nil {
		return err
	}
	// A death can satisfy a passive object's live-monster check (能源储藏室's
	// cylinders count their room's monster down to zero and open the boss door).
	// （上游 b402882 的物件脚本子系统）
	s.runCurrentDungeonActionObjectsLocked(session, runtime, scene, runtimeDungeonActionObjectInRoom, "monster_death")
	// The current client keeps a defeated actor in its white-hit state until
	// op38 arrives. Send that authoritative visual retirement before the
	// durable experience transaction so SQLite contention can never delay the
	// visible death or its ground-drop projection.
	if rewardMonsterCandidate && !currentDungeonRuntimeIsPractice(runtime) &&
		!s.currentDungeonRuntimeIsRaidGate(runtime) {
		// A raid gate grants no experience at all: live 2026-09-16 20:28Z showed
		// per-kill 怪物经验 rows inside gate 215 plus a 20.3M clear-exp package.
		// The gate's reward is the raid phase settlement, not character growth.
		experienceContext, cancelExperience := context.WithTimeout(context.Background(), createWriteTimeout)
		monsterExperience, err = s.awardCurrentDungeonMonsterExperience(
			experienceContext,
			session,
			runtime,
			rewardMonster,
		)
		cancelExperience()
		if err != nil {
			s.logGameEvent(session, "game-dungeon-monster-experience-deferred",
				"dungeon_id", runtime.Dungeon.ID,
				"maze_index", runtime.MazeIndex,
				"room", scene.Coordinate.String(),
				"map_id", scene.Map.Map.ID,
				"monster_id", rewardMonster.Spawn.MonsterID,
				"monster_object_key", request.RuntimeObjectKey,
				"logical_monster_object_key", rewardMonster.ObjectKey,
				"reason", "runtime_pvf_csharp_domain_formula_or_atomic_progression_unavailable",
				"error", err)
		} else {
			recordCurrentDungeonMonsterExperience(runtime, rewardMonster, monsterExperience)
			if monsterExperience.Award.Gain > 0 {
				runtime.Character = dnfrepo.CloneCharacter(monsterExperience.Character)
				// 满级(90)角色：op37 的累计经验压回持久化值，客户端差值为 0
				// 就不再弹"获得X(基础…)"。判定与理由见
				// currentMonsterKillOp37TotalExperience。
				monsterExperienceOp37Total = currentMonsterKillOp37TotalExperience(monsterExperience)
				monsterExperienceBody = buildCurrentFinishLoadingCharacterStateBodyWithPresentation(
					monsterExperience.Character,
					monsterExperience.Skill.Points,
					&currentFinishLoadingExperiencePresentation{
						GrowthContractBonus: monsterExperience.GrowthContractBonus,
						FatigueBatteryBonus: monsterExperience.FatigueBatteryBonus,
						WelcomeBonus:        monsterExperience.WelcomeBonus,
						TotalExperience:     &monsterExperienceOp37Total,
					},
				)
				if len(monsterExperienceBody) != currentFinishLoadingCharacterStateBodySize {
					s.logGameEvent(session, "game-dungeon-monster-experience-deferred",
						"dungeon_id", runtime.Dungeon.ID,
						"monster_id", dropMonster.Spawn.MonsterID,
						"monster_object_key", request.RuntimeObjectKey,
						"reason", "current_exe_op37_body_shape_invalid_after_atomic_commit",
						"body_len", len(monsterExperienceBody))
					monsterExperienceBody = nil
				}
			}
		}
	}
	if stagedStoryForcedCombatEnd != nil {
		forcedVisual, forcedCleared, flushErr := s.flushCurrentDungeonForcedCombatEndLocked(
			session,
			runtime,
			request.RuntimeObjectKey,
			"op39_story_stage_boss_actor_death_after_source_coordinate_response",
			*stagedStoryForcedCombatEnd,
		)
		if flushErr != nil {
			return flushErr
		}
		forcedBossVisualDeathCount = forcedVisual
		cleared = cleared || forcedCleared
	}
	if len(monsterExperienceBody) != 0 {
		if err := s.sendGameUpperRawClass(session, currentDungeonCharacterStateMsgID, monsterExperienceBody, 0); err != nil {
			return err
		}
		if monsterExperience.Character.Level != monsterExperience.PreviousLevel {
			if err := s.sendCurrentDungeonLevelUpSkillRuntimeRefresh(
				session,
				monsterExperience.Character,
				monsterExperience.Skill,
				"current_exe_op38_op37_committed_level_change_skill_runtime_before_quest_refresh",
			); err != nil {
				return err
			}
			if err := s.sendCurrentAcceptableQuestListForSession(
				session,
				"current_exe_op38_op37_op19_after_atomic_monster_level_change",
			); err != nil {
				return err
			}
			s.notifyCurrentUnitedFriendPresence(
				monsterExperience.Character.Name,
				currentFriendPresence{ChannelID: uint16(session.channel.ID), Busy: true},
				"dungeon_level_up_friend_absolute_refresh",
			)
		}
		s.logGameEvent(session, "game-dungeon-monster-experience-awarded",
			"dungeon_id", runtime.Dungeon.ID,
			"maze_index", runtime.MazeIndex,
			"room", scene.Coordinate.String(),
			"map_id", scene.Map.Map.ID,
			"monster_id", monsterExperience.Award.MonsterID,
			"monster_object_key", request.RuntimeObjectKey,
			"logical_monster_object_key", rewardMonster.ObjectKey,
			"monster_level", monsterExperience.Award.MonsterLevel,
			"monster_type", monsterExperience.Award.MonsterType,
			"named_monster", monsterExperience.Award.NamedMonster,
			"monster_table_exp", monsterExperience.Award.MonsterTableEXP,
			"dungeon_weight", monsterExperience.Award.DungeonWeight,
			"difficulty_rate", monsterExperience.Award.DifficultyRate,
			"monster_type_rate", monsterExperience.Award.MonsterTypeRate,
			"level_penalty", monsterExperience.Award.LevelPenalty,
			"pre_penalty_gain", monsterExperience.Award.PrePenaltyGain,
			"experience_gain", monsterExperience.Award.Gain,
			"growth_contract_bonus", monsterExperience.GrowthContractBonus,
			"fatigue_battery_bonus", monsterExperience.FatigueBatteryBonus,
			"equipment_bonus", monsterExperience.EquipmentBonus,
			"attendance_marble_bonus", monsterExperience.AttendanceMarbleBonus,
			"committed_experience_gain", uint64(monsterExperience.Award.Gain)+uint64(monsterExperience.GrowthContractBonus)+uint64(monsterExperience.EquipmentBonus)+uint64(monsterExperience.AttendanceMarbleBonus),
			"honor_expert_gain", monsterExperience.HonorExpertGain,
			"honor_expert_level", numericCharacterStatValue(monsterExperience.Character, currentHonorExpertLevelStatKey),
			"honor_expert_progress", numericCharacterStatValue(monsterExperience.Character, currentHonorExpertProgressExperienceStatKey),
			// 同一笔经验现在也镜像进账号冒险团：这两个字段是现场核对
			// "打怪到底有没有在攒结晶"的唯一凭据。credit = 进骁勇结晶的
			// 经验（任意等级），maximum = 其中的满级溢出部分（进胶囊槽/名誉）。
			"adventure_group_credit", monsterExperience.AdventureGroupCredit,
			"adventure_group_maximum", monsterExperience.AdventureGroupMaximum,
			"adventure_group_crystals", monsterExperience.AdventureGroupCrystals,
			"sp_gain", monsterExperience.SPGain,
			"tp_gain", monsterExperience.TPGain,
			"remaining_sp", monsterExperience.Skill.Points.RemainingSP,
			"remaining_tp", monsterExperience.Skill.Points.RemainingTP,
			"new_level", monsterExperience.Character.Level,
			"new_total_exp", statU32(monsterExperience.Character, "exp", 0),
			"presented_total_exp", monsterExperience.PresentedTotalExperience,
			// op37_total_exp 是真正写进包里的 +0x01；满级时它等于持久化经验，
			// 客户端差值为 0，因此不会弹"获得X(基础…)"提示。
			"op37_total_exp", monsterExperienceOp37Total,
			"exp_notice_suppressed", monsterExperienceOp37Total != monsterExperience.PresentedTotalExperience,
			"op38_then_op37", true,
			"op37_body_len", len(monsterExperienceBody),
			"formula_source", "runtime_pvf_monsterexp_table_formula",
			"packet_source", "current_exe_sub_1D78240_full_character_state")
	}
	if rewardMonsterCandidate && !currentDungeonRuntimeIsPractice(runtime) {
		// Ordinary hunt objectives are owned by the current client's native
		// op33 SET_TRIGGER. Applying the same death here races that write and
		// double-decrements packed progress. Passive/room-object objectives,
		// which have no client owner, retain the explicit server path.
		s.checkCurrentDungeonQuestMonsterDrops(session, runtime, rewardMonster.Spawn.MonsterID)
	} else if questAICharacterDropCandidate && !currentDungeonRuntimeIsPractice(runtime) {
		// enemy_type=2 is the PVF domain for hostile AI/APC actors. Their op39
		// carries only the room object key, so resolve and freeze the AI code
		// before CommitActorDeathReport changes the actor state, then grant the
		// matching quest item after the authoritative op38 death notification.
		s.checkCurrentDungeonQuestEnemyDrops(
			session,
			runtime,
			questAICharacterCode,
			currentDungeonQuestEnemyTypeAICharacter,
			false,
		)
	}
	if bossActorDeath && !storyAIBossGateActive && !pvfMultiBossPending && !isPVFTutorialDungeonScene(runtime, scene) &&
		stagedStoryForcedCombatEnd == nil {
		forcedDefeated, forcedVisual, forcedCleared, forceErr := s.forceCurrentDungeonRemainingHostilesForCombatEndLocked(
			session,
			runtime,
			request.RuntimeObjectKey,
			"op39_boss_actor_death",
		)
		if forceErr != nil {
			s.logGameEvent(session, "game-dungeon-boss-forced-clear-deferred",
				"dungeon_id", runtime.Dungeon.ID,
				"maze_index", runtime.MazeIndex,
				"room", scene.Coordinate.String(),
				"map_id", scene.Map.Map.ID,
				"boss_object_key", request.RuntimeObjectKey,
				"monster_id", rewardMonster.Spawn.MonsterID,
				"boss_rank", rewardMonster.Spawn.Rank,
				"boss_actor_type", bossActorType,
				"boss_actor_source", bossActorSource,
				"forced_defeated_count", forcedDefeated,
				"reason", "boss_death_forced_clear_commit_failed",
				"error", forceErr)
		} else if forcedDefeated > 0 || forcedCleared {
			forcedBossClearCount = forcedDefeated
			forcedBossVisualDeathCount = forcedVisual
			cleared = cleared || forcedCleared
			scene, _ = runtime.Session.Scene()
			s.logGameEvent(session, "game-dungeon-boss-forced-room-clear",
				"dungeon_id", runtime.Dungeon.ID,
				"maze_index", runtime.MazeIndex,
				"room", scene.Coordinate.String(),
				"map_id", scene.Map.Map.ID,
				"boss_object_key", request.RuntimeObjectKey,
				"monster_id", rewardMonster.Spawn.MonsterID,
				"boss_rank", rewardMonster.Spawn.Rank,
				"boss_actor_type", bossActorType,
				"boss_actor_source", bossActorSource,
				"forced_defeated_count", forcedDefeated,
				"forced_visual_death_count", forcedBossVisualDeathCount,
				"defeated_actor_count", len(scene.DefeatedObjects),
				"tracked_hostile_count", len(scene.ExpectedHostiles),
				"blocking_hostile_count", len(scene.BlockingHostiles),
				"room_cleared", scene.Cleared,
				"flow_source", "boss_death_combat_end_policy")
		}
	}
	if err := runtime.cacheCurrentDungeonRoom(); err != nil {
		return fmt.Errorf("cache dungeon room after actor death: %w", err)
	}
	scene, _ = runtime.Session.Scene()
	s.logGameEvent(session, "game-dungeon-monster-death-accepted",
		"dungeon_id", runtime.Dungeon.ID,
		"maze_index", runtime.MazeIndex,
		"room", scene.Coordinate.String(),
		"map_id", scene.Map.Map.ID,
		"object_key", request.RuntimeObjectKey,
		"related_actor_object_key", request.OwnerObjectKey,
		"request_layout", request.Layout,
		"combat_entry_count", request.CombatEntryCount,
		"actor_kind", target.Reference.Kind,
		"scripted_tutorial_death", scriptedTutorialDeath,
		"scripted_tutorial_monster_id", scriptedTutorialMonster.Spawn.MonsterID,
		"scripted_tutorial_rank", scriptedTutorialMonster.Spawn.Rank,
		"scripted_tutorial_monster_index", scriptedTutorialEvidence.MonsterIndex,
		"scripted_tutorial_cinematic_id", scriptedTutorialEvidence.CinematicID,
		"scripted_tutorial_cinematic_path", scriptedTutorialEvidence.CinematicPath,
		"owner_sentinel_blocking_clear_target", ownerSentinelBlockingClearTarget,
		"owner_sentinel_blocking_clear_source", ownerSentinelBlockingClearSource,
		"owner_sentinel_ordinary_target", ownerSentinelOrdinaryTarget,
		"owner_sentinel_hostile_ai_target", ownerSentinelHostileAITarget,
		"response_kind", target.ResponseKind,
		"response_opcode", uint16(dnfenum.CmdPacketNotifyDieMonster),
		"response_body_len", len(responseBody),
		"story_stage_index", runtime.StoryStageIndex,
		"next_story_stage_index", nextStoryStageIndex,
		"story_stage_advance", len(optionalStoryRoom) == 1,
		"story_stage_next_room", nextStoryCoordinate.String(),
		"defeated_actor_count", len(scene.DefeatedObjects),
		"tracked_hostile_count", len(scene.ExpectedHostiles),
		"blocking_hostile_count", len(scene.BlockingHostiles),
		"room_cleared", cleared,
		"boss_actor_death", bossActorDeath,
		"boss_actor_type", bossActorType,
		"boss_actor_source", bossActorSource,
		"clear_condition_actor_death", clearConditionActorDeath,
		"clear_condition_source", clearConditionSource,
		"logical_monster_resolved", rewardMonsterCandidate,
		"logical_monster_id", rewardMonster.Spawn.MonsterID,
		"logical_monster_object_key", rewardMonster.ObjectKey,
		"boss_forced_defeated_count", forcedBossClearCount,
		"boss_forced_visual_death_count", forcedBossVisualDeathCount,
		"story_ai_boss_dual_death_gate", storyAIBossGateActive,
		"story_ai_boss_dual_death_ready", storyAIBossGate.Ready,
		"story_dummy_boss_object_keys", storyAIBossGate.DummyBossObjectKeys,
		"story_ai_boss_object_keys", storyAIBossGate.AIBossObjectKeys,
		"pvf_multi_boss_gate", pvfMultiBossGateActive,
		"pvf_multi_boss_ready", pvfMultiBossGate.Ready,
		"pvf_multi_boss_object_keys", pvfMultiBossGate.BossObjectKeys,
		"boss_room", scene.Boss,
		"drop_count", len(deathDrops),
		"drop_source", dropSource,
		"monster_experience_gain", monsterExperience.Award.Gain,
		"monster_experience_state_sent", len(monsterExperienceBody) != 0,
		"response", "current_exe_op38_actor_death_notification")
	if runtime.bossDieCheckPending &&
		uint32(runtime.bossDieCheckPendingRequest.TargetObjectKey) == request.RuntimeObjectKey {
		pendingRequest := runtime.bossDieCheckPendingRequest
		s.logGameEvent(session, "game-dungeon-boss-die-check-resume-after-death",
			"dungeon_id", runtime.Dungeon.ID,
			"maze_index", runtime.MazeIndex,
			"room", scene.Coordinate.String(),
			"map_id", scene.Map.Map.ID,
			"related_actor_object_key", pendingRequest.RelatedActorObjectKey,
			"target_object_key", pendingRequest.TargetObjectKey,
			"reason", "same_target_authoritative_op39_committed")
		return s.handleDungeonBossDieCheckLocked(session, runtime, pendingRequest, true)
	}
	// A boss actor dying inside a room that is already Cleared is the cinematic
	// dummy-boss case (quest 3599 / dungeon 96 map 76114): every spawn is on
	// team 0, so the room never blocked and the death itself is the only
	// settlement signal the client will send.
	if cleared || (storyAIBossGateActive && storyAIBossGate.Ready && scene.Cleared) || (bossActorDeath && scene.Cleared) {
		if visit, visitErr := runtime.currentDungeonRoomVisit(scene); visitErr == nil {
			s.awardCurrentPetRoomClearLocked(
				session,
				runtime,
				scene,
				int(visit.FatigueConsumedAmount),
				"authoritative_monster_room_clear",
			)
		} else {
			s.logGameEvent(session, "game-pet-room-clear-growth-deferred",
				"char_id", session.selectedCharacterID,
				"dungeon_id", runtime.Dungeon.ID,
				"room", scene.Coordinate.String(),
				"source", "authoritative_monster_room_clear",
				"reason", "room_visit_unavailable",
				"error", visitErr)
		}
		if err := s.completeCurrentDungeonOrdinaryFinalRoomAfterDeathLocked(session, runtime, scene, request.RuntimeObjectKey); err != nil {
			return err
		}
		if err := s.completePVFTutorialBosslessFinalRoomAfterDeathLocked(session, runtime, scene); err != nil {
			return err
		}
	}
	s.cancelCurrentDungeonReturnAfterDungeonEvidenceLocked(
		session,
		runtime,
		"accepted_hostile_op39_after_pending_op24",
	)
	return nil
}

func (runtime *runtimeDungeonState) accumulateCurrentDungeonSettlementMonsterExperience(
	monsterType byte,
	gain uint32,
) {
	if runtime == nil || gain == 0 {
		return
	}
	runtime.settlementMonsterExperienceTotal = saturatingCurrentDungeonUint32Add(
		runtime.settlementMonsterExperienceTotal,
		gain,
	)
	switch monsterType {
	case 1:
		runtime.settlementChampionExperience = saturatingCurrentDungeonUint32Add(
			runtime.settlementChampionExperience,
			gain,
		)
	case 2:
		runtime.settlementSuperChampionExperience = saturatingCurrentDungeonUint32Add(
			runtime.settlementSuperChampionExperience,
			gain,
		)
	case 3:
		runtime.settlementBossExperience = saturatingCurrentDungeonUint32Add(
			runtime.settlementBossExperience,
			gain,
		)
	}
}

func saturatingCurrentDungeonUint32Add(current uint32, delta uint32) uint32 {
	if ^uint32(0)-current < delta {
		return ^uint32(0)
	}
	return current + delta
}
