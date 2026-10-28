package dnfbridge

import (
	"context"
	"encoding/binary"
	"fmt"

	"longheng.io/server/internal/modules/dnf/dnfenum"
)

func (s *Service) sendEnterSelectDungeonState(
	session *gameSession,
	source string,
	transport bool,
	requestDriven bool,
) error {
	backToVillagePending := false
	if !transport && requestDriven && session != nil {
		session.townMu.Lock()
		session.townTransportEnterSelectPending = false
		session.townMu.Unlock()
	}
	if !transport && requestDriven && session != nil && session.backToVillageEnterSelectPending {
		// The current client reuses this first op15 both as the typed-op24 scene
		// finalizer and as the player's selector intent. Keep the marker until the
		// pending return has committed so a failed repository/player-state suffix
		// can be retried without requiring a second click.
		backToVillagePending = true
	}
	if requestDriven && isCurrentImplementedRaidChannel(session) {
		state, allowed, reason := s.runtimeRaidSelectorAdmission(session)
		if !allowed {
			s.logGameEvent(session, "game-raid-enter-select-dungeon-blocked",
				"source", source,
				"char_id", selectedCharacterID(session),
				"raid_key", state.RaidKey,
				"raid_started", state.Started,
				"entry_cost_paid", state.EntryCostPaid,
				"lifecycle_status", byte(state.Lifecycle.Status),
				"lifecycle_phase", state.Lifecycle.Phase,
				"reason", reason,
				"response", "none")
			return nil
		}
	}
	if !transport && requestDriven && session != nil && session.selectedCharacterID != 0 {
		partyState := runtimePartyStateSnapshot(session)
		if partyState.PartyID > 0 && len(runtimePartyMembers(partyState)) > 1 &&
			partyState.UserID != session.selectedCharacterID {
			// A non-leader still needs the return transaction committed and its
			// live-dungeon membership removed. It does not own the shared selector,
			// so consume the same first op15 without opening a second page.
			if backToVillagePending {
				if err := s.commitPendingDungeonReturnBeforeTownEnterSelect(session, source+"_party_member_return"); err != nil {
					return err
				}
				session.backToVillageEnterSelectPending = false
				session.backToVillageSelectorDuplicateGuard = true
				if err := s.rebuildRuntimePartyTownCoPresenceAfterReturn(session, source+"_party_member_return"); err != nil {
					return err
				}
			}
			s.logGameEvent(session, "game-party-enter-select-non-leader-consumed",
				"source", source,
				"party_id", partyState.PartyID,
				"leader_char_id", partyState.UserID,
				"char_id", session.selectedCharacterID,
				"reason", "same_version_party_selector_is_leader_owned")
			return nil
		}
	}
	if !transport && requestDriven {
		if err := s.commitPendingDungeonReturnBeforeTownEnterSelect(session, source); err != nil {
			return err
		}
		if backToVillagePending {
			session.backToVillageEnterSelectPending = false
			session.backToVillageSelectorDuplicateGuard = true
			if err := s.rebuildRuntimePartyTownCoPresenceAfterReturn(
				session,
				source+"_after_back_to_village_return",
			); err != nil {
				return err
			}
			s.logGameEvent(session, "game-upper-enter-select-dungeon-after-back-to-village-accepted",
				"source", source,
				"char_id", session.selectedCharacterID,
				"reason", "current_exe_first_op15_is_scene_finalizer_and_selector_intent",
				"response", "selector_state")
		}
		retired, err := s.retireCompletedDungeonForTownSelect(session)
		if err != nil {
			s.logGameEvent(session, "game-upper-enter-select-dungeon-completed-runtime-retire-blocked",
				"source", source,
				"char_id", session.selectedCharacterID,
				"reason", "verified_town_origin_unavailable",
				"error", err)
		} else if retired {
			s.logGameEvent(session, "game-upper-enter-select-dungeon-completed-runtime-retired",
				"source", source,
				"char_id", session.selectedCharacterID,
				"reason", "settlement_page_select_other_dungeon_uses_current_op15_op27_route")
		}
	}
	if requestDriven && session.enterSelectDungeonAckSent {
		s.logGameEvent(session, "game-upper-enter-select-dungeon-duplicate-ack-replay",
			"source", source,
			"request_driven", true,
			"packet_count", 2,
			"reason", "current_client_retries_op15_after_post_start_map_state")
	}
	if !requestDriven && session.enterSelectDungeonSent {
		s.logGameEvent(session, "game-upper-enter-select-dungeon-duplicate-skipped", "source", source, "request_driven", false)
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	charID, _, character, hasCharacter := s.selectedCharacterForEnter(ctx, session)

	contextReady := false
	contextReadyReason := "legacy_or_proactive_path"
	if !transport && requestDriven {
		contextReady, contextReadyReason = s.currentTownEnterSelectReady(session)
	}
	packetCount := 2
	if contextReady {
		packetCount += 3
	}
	s.logPacketEvent("game-enter-select-dungeon-state-send",
		"conn_id", session.connID,
		"channel_id", session.channel.ID,
		"source", source,
		"char_id", charID,
		"scene_object_key", currentSceneActorObjectKey(charID),
		"transport", transport,
		"request_driven", requestDriven,
		"enter_msg_id", uint16(dnfenum.CmdPacketEnterSelectDungeon),
		"fatigue_msg_id", currentFatigueMsgID,
		"packet_count", packetCount,
		"town_context_ready", contextReady,
		"town_context_ready_reason", contextReadyReason,
		"reason", "current_exe_op15_ack_op36_fatigue_optional_missing_skill_manager_op19_op5_permissions_and_guarded_op27_enter_select_page")

	if transport {
		enterBody := buildEnterSelectDungeonBody(charID)
		if err := s.sendGame(session, byte(dnfenum.GameCmdCommand), uint16(dnfenum.CmdPacketEnterSelectDungeon), enterBody); err != nil {
			return err
		}
	} else if err := s.sendGameUpperSuccess(session, uint16(dnfenum.CmdPacketEnterSelectDungeon), buildEnterSelectDungeonAckBody()); err != nil {
		return err
	}
	fatigueBody := buildCurrentFatigueBody(character, hasCharacter)
	s.logGameEvent(session, "game-enter-select-dungeon-fatigue-send",
		"char_id", charID,
		"msg_id", currentFatigueMsgID,
		"body_len", len(fatigueBody),
		"fatigue_used", binary.LittleEndian.Uint16(fatigueBody[0:2]),
		"fatigue_limit", binary.LittleEndian.Uint16(fatigueBody[2:4]),
		"actor_aux", binary.LittleEndian.Uint16(fatigueBody[4:6]),
		"fatigue_battery_counter", binary.LittleEndian.Uint16(fatigueBody[6:8]),
		"actor_extra", binary.LittleEndian.Uint16(fatigueBody[8:10]),
		"body_source", "current_exe_sub_1D7ABE0_fatigue_five_u16")
	if transport {
		if err := s.sendGame(session, byte(dnfenum.GameCmdCommand), currentFatigueMsgID, fatigueBody); err != nil {
			return err
		}
	} else if err := s.sendGameUpperRawClass(session, currentFatigueMsgID, fatigueBody, 0); err != nil {
		return err
	}
	session.enterSelectDungeonSent = true
	if requestDriven {
		session.enterSelectDungeonAckSent = true
		session.enterSelectDungeonPassiveParty = false
	}
	if !transport && requestDriven {
		if contextReady {
			// The current client evaluates its local skill-initialization guard
			// before it sends op16. A town transition or another actor-manager
			// rebuild can leave that guard stale even though op29 committed the
			// learned levels and the repository is complete. Reinstall the full
			// repository/PVF-backed op19 while the finalized town actor is still the
			// current owner.
			if !hasCharacter {
				return fmt.Errorf("dungeon selector skill initialization requires selected character %d", charID)
			}
			if err := s.sendCurrentSceneSkillInfo(
				session,
				ctx,
				character,
				source+"_after_op15_ack_fatigue_before_op27",
			); err != nil {
				return fmt.Errorf("initialize dungeon selector skills: %w", err)
			}
			// A fallback op19 is character-specific and can rebuild native managers
			// used by the selector. Publish the complete PVF ordinary-dungeon authority
			// after that decision, as the final state before op27 binds the page. A valid
			// character with zero active quests must still receive every ordinary
			// dungeon row at state zero or higher; task state never gates this table.
			if err := s.sendCurrentDungeonPermissionSnapshot(ctx, session, source+"_after_op15_ack_fatigue_and_skill_manager"); err != nil {
				return err
			}
		}
		if err := s.sendCurrentTownEnterSelectContext(session, source+"_after_op15_ack_fatigue"); err != nil {
			return err
		}
		if contextReady {
			if err := s.synchronizeRuntimePartyMembersEnterSelect(session, source+"_party_members"); err != nil {
				return err
			}
			// Operator product decision: a character sitting on the
			// dungeon-selection page must no longer be visible in town. This runs
			// after the party fan-out on purpose - every path that resolves the
			// opener's followers or their town rows should still see the initiator
			// in the roster while it does so.
			//
			// Deliberately NOT followed by the party co-presence projection here:
			// town_enter_select_test.go pins this fan-out's shape, and it forbids the
			// passive follower an op9 record (the actor projection embeds one). The
			// route that needs the projection is the one this page is reached from
			// after a completed run - see retireCompletedDungeonForTownSelect.
			s.leaveTownPresenceForDungeonSelector(session, source+"_after_selector_page_committed")
		}
	}
	if err := s.sendRuntimeRaidSelectorSnapshot(session, source+"_after_current_selector_context"); err != nil {
		return err
	}
	return nil
}

// runtimeRaidSelectorAdmission owns the Anton waiting-map dungeon portal.
// Type-23 is a cross-channel event boundary, so it must never fall back to the
// ordinary selector merely because the client sent the common op15 command.
func (s *Service) runtimeRaidSelectorAdmission(session *gameSession) (runtimeRaidState, bool, string) {
	spec, raidChannel := currentRaidTownSpecForSession(session)
	if s == nil || session == nil || !raidChannel {
		return runtimeRaidState{}, true, "ordinary_channel"
	}
	state, ok := s.runtimeRaidForMember(selectedCharacterID(session))
	if !ok {
		return runtimeRaidState{}, false, "raid_membership_missing"
	}
	if !state.Started || !state.EntryCostPaid {
		return state, false, "raid_not_started"
	}
	if state.Lifecycle.Status != runtimeRaidLifecyclePhaseActive {
		return state, false, "raid_phase_not_active"
	}
	if _, frozen := runtimeRaidFrozenMember(state, selectedCharacterID(session)); !frozen {
		return state, false, "raid_member_not_in_frozen_roster"
	}
	if s.onlinePlayers == nil {
		return state, false, "raid_waiting_instance_unavailable"
	}
	presence, present := s.onlinePlayers.PlayerForCharacter(selectedCharacterID(session))
	if !present || presence.Session != session ||
		presence.TownID != byte(spec.townID) ||
		presence.AreaID != spec.waitingArea ||
		presence.InstanceKey != state.RaidKey {
		// Dungeon entry deliberately removes the actor from town presence.  The
		// settlement-page "select another dungeon" action is nevertheless a
		// valid return to the Raid selector after the completed objective and
		// the op72 exit ACK are both committed.  Requiring the waiting-instance
		// presence here deadlocks exactly that route: the actor cannot regain a
		// town presence until after the completed dungeon runtime is retired.
		// Keep this exception narrow so an arbitrary op15 from an active run (or
		// an unjoined/unstarted Raid) still cannot cross the private portal.
		if completedRuntimeRaidSelectorReturnReady(session, state.RaidKey) {
			return state, true, "completed_raid_objective_exit"
		}
		// The corner-room wall_open warp out of an active energy-interception run
		// (218) into a hatchery the raid has opened sends the same op15. Refusing
		// it answered nothing at all (`response=none`), so the client was left
		// without the selector page its op15 is the finalizer for: live
		// 2026-09-17 07:25:12Z, dungeon_id=222, followed by
		// game-dungeon-move-blocked reason=dungeon_runtime_missing and
		// game-dungeon-giveup-game-blocked reason=active_dungeon_runtime_owner_missing
		// -- a client that could neither move nor leave.
		if s.runtimeRaidHatcheryWarpAdmission(session, state) {
			return state, true, "hatchery_transfer_warp"
		}
		return state, false, "raid_member_not_in_private_waiting_instance"
	}
	return state, true, "raid_phase_active"
}

func completedRuntimeRaidSelectorReturnReady(session *gameSession, raidKey uint32) bool {
	if session == nil || raidKey == 0 || session.selectedCharacterID == 0 {
		return false
	}
	session.dungeon.mu.Lock()
	defer session.dungeon.mu.Unlock()
	runtime := session.dungeon.runtime
	return runtime != nil && runtime.raidDungeon.RaidKey == raidKey &&
		runtime.settlementCardExitAckSent &&
		runtime.settlementPhase >= currentDungeonSettlementPhaseEnding &&
		completedRuntimeRaidDungeonExitReady(runtime, session.selectedCharacterID)
}

// synchronizeRuntimePartyMembersEnterSelect promotes the leader's op15 to one
// party scene transition. Members may be in different public areas on the same
// channel; stable party membership, not town visibility, owns the fan-out.
func (s *Service) synchronizeRuntimePartyMembersEnterSelect(initiator *gameSession, source string) error {
	if initiator == nil || initiator.selectedCharacterID == 0 {
		return nil
	}
	state := runtimePartyStateSnapshot(initiator)
	if state.PartyID <= 0 || state.UserID != initiator.selectedCharacterID {
		return nil
	}
	// 并发扇出：initiator 自己已被跳过，每个 follower 都在【它自己的】event loop
	// 上执行且互不依赖。改前是在循环里给每人现开一个 createWriteTimeout 再同步等，
	// 把发起者的 event loop 按成员数串行累加地卡住。
	members := runtimePartyMembers(state)
	targets := make([]sessionFanoutTarget, 0, len(members))
	for _, member := range members {
		if member.UserID == 0 || member.UserID == initiator.selectedCharacterID {
			continue
		}
		follower, ok := s.onlineGameSession(member.UserID)
		if !ok || follower == initiator {
			continue
		}
		followerState := runtimePartyStateSnapshot(follower)
		if followerState.PartyID != state.PartyID || followerState.UserID != state.UserID {
			continue
		}
		targets = append(targets, sessionFanoutTarget{
			label:       fmt.Sprintf("%d", member.UserID),
			characterID: member.UserID,
			session:     follower,
		})
	}
	return s.fanOutToGameSessions(context.Background(), targets, "runtime-party-enter-select",
		func(target sessionFanoutTarget) error {
			_, prepareErr := s.prepareRuntimePartyFollowerEnterSelect(
				target.session,
				state.UserID,
				state.PartyID,
				source,
			)
			return prepareErr
		})
}

// prepareRuntimePartyFollowerEnterSelect uses the same-version passive-member
// selector sequence: own fatigue, leader-derived dungeon authority and op27.
// A follower did not submit op15, so it must not receive a fabricated op15 ACK
// or rebuild the established party table with op9.
func (s *Service) prepareRuntimePartyFollowerEnterSelect(
	follower *gameSession,
	leaderCharacterID uint16,
	partyID int,
	source string,
) (bool, error) {
	if follower == nil || follower.selectedCharacterID == 0 {
		return false, nil
	}
	if follower.enterSelectDungeonContextSent && follower.enterSelectDungeonSent {
		return true, nil
	}
	ready, reason := s.currentTownEnterSelectReady(follower)
	if !ready {
		s.logGameEvent(follower, "game-party-enter-select-follower-deferred",
			"source", source,
			"leader_char_id", leaderCharacterID,
			"party_id", partyID,
			"reason", reason)
		return false, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	charID, _, character, hasCharacter := s.selectedCharacterForEnter(ctx, follower)
	if charID == 0 {
		charID = follower.selectedCharacterID
	}
	partyState := runtimePartyStateSnapshot(follower)
	if partyState.PartyID != partyID || partyState.UserID != leaderCharacterID {
		return false, nil
	}
	fatigueBody := buildCurrentFatigueBody(character, hasCharacter)
	if err := s.sendGameUpperRawClass(follower, currentFatigueMsgID, fatigueBody, 0); err != nil {
		return false, err
	}
	if err := s.sendCurrentDungeonPermissionSnapshot(ctx, follower, source+"_after_fatigue"); err != nil {
		return false, err
	}
	if !follower.enterSelectDungeonContextSent {
		if err := s.sendCurrentTownEnterSelectContext(follower, source+"_after_fatigue"); err != nil {
			return false, err
		}
	}
	if !follower.enterSelectDungeonContextSent {
		return false, nil
	}
	follower.enterSelectDungeonSent = true
	follower.enterSelectDungeonAckSent = false
	follower.enterSelectDungeonPassiveParty = true
	if err := s.sendRuntimeRaidSelectorSnapshot(follower, source+"_after_passive_party_selector_context"); err != nil {
		return false, err
	}
	s.logGameEvent(follower, "game-party-enter-select-follower-synchronized",
		"source", source,
		"leader_char_id", leaderCharacterID,
		"char_id", charID,
		"party_id", partyID,
		"sequence", "leader_selector_then_follower_op36_own_fatigue_op5_leader_authority_op27_without_op15_ack")
	return true, nil
}
