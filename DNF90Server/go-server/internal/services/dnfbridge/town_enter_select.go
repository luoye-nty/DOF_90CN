package dnfbridge

import (
	"context"

	"longheng.io/server/internal/modules/dnf/worldmap"
)

func (s *Service) markCurrentTownSceneReady(session *gameSession) {
	if session == nil || session.selectedCharacterID == 0 {
		return
	}
	session.townMu.Lock()
	session.townSceneReadyCharacterID = session.selectedCharacterID
	session.townMu.Unlock()
}

func (s *Service) currentTownEnterSelectReady(session *gameSession) (bool, string) {
	if session == nil || session.selectedCharacterID == 0 {
		return false, "selected_character_missing"
	}
	session.townMu.Lock()
	readyCharacterID := session.townSceneReadyCharacterID
	positionSnapshot := session.townPositionSnapshot
	session.townMu.Unlock()
	if readyCharacterID == 0 || readyCharacterID != session.selectedCharacterID {
		return false, "town_scene_player_state_not_finalized"
	}
	if positionSnapshot.CharacterID != session.selectedCharacterID {
		return false, "town_scene_location_owner_missing"
	}
	if !positionSnapshot.PositionValid {
		return false, "town_scene_position_not_reported"
	}
	session.dungeon.mu.Lock()
	hasDungeonRuntime := session.dungeon.runtime != nil
	session.dungeon.mu.Unlock()
	if hasDungeonRuntime {
		return false, "active_dungeon_runtime"
	}
	return true, "current_town_scene_player_state_finalized"
}

// commitPendingDungeonReturnBeforeTownEnterSelect repairs the current EXE
// route where the client returns to town from an active dungeon and immediately
// clicks the dungeon gate again. That op15 can be the first town-side request
// after the typed return op24, so the wrapper commits the pending runtime and
// restores the town position owner needed by sendCurrentTownEnterSelectContext.
func (s *Service) commitPendingDungeonReturnBeforeTownEnterSelect(session *gameSession, source string) error {
	if session == nil || session.selectedCharacterID == 0 {
		return nil
	}
	characterID := session.selectedCharacterID

	session.dungeon.mu.Lock()
	runtime := session.dungeon.runtime
	shouldCommit := runtime != nil && runtime.Session != nil &&
		runtime.townReturnPending && runtime.townReturnOp24Sent
	transition := currentDungeonTownTransition{}
	origin := currentTownPositionSnapshot{}
	if shouldCommit {
		transition = cloneCurrentDungeonTownTransition(runtime.townReturnTransition)
		origin = runtime.townReturnOrigin
	}
	session.dungeon.mu.Unlock()
	if !shouldCommit {
		return s.ensureCurrentConfirmedDungeonReturnPlayerState(
			session,
			source+"_without_retained_runtime",
		)
	}
	if err := s.commitPendingDungeonReturnForSceneRequest(session, "current_exe_op15_enter_select_after_pending_town_return"); err != nil {
		return err
	}

	session.dungeon.mu.Lock()
	stillHasRuntime := session.dungeon.runtime != nil
	session.dungeon.mu.Unlock()
	if stillHasRuntime || session.selectedCharacterID != characterID {
		s.logGameEvent(session, "game-upper-enter-select-dungeon-pending-return-committed",
			"source", source,
			"char_id", characterID,
			"runtime_retained", stillHasRuntime,
			"selected_character", session.selectedCharacterID,
			"reason", "pending_return_committed_but_town_context_changed")
		return nil
	}

	snapshot := currentTownPositionSnapshot{
		CharacterID:   characterID,
		TownID:        transition.TownID,
		AreaID:        transition.AreaID,
		PositionX:     uint16(transition.PositionX),
		PositionY:     uint16(transition.PositionY),
		MovementCode:  transition.Direction,
		PositionValid: true,
	}
	positionSource := transition.PositionSource
	if origin.CharacterID == characterID &&
		origin.TownID == transition.TownID &&
		origin.AreaID == transition.AreaID &&
		origin.PositionValid {
		snapshot = origin
		positionSource = "current_exe_op35_runtime_origin_snapshot"
	}

	session.townMu.Lock()
	clearCurrentTownSelectorOriginLocked(session)
	session.townSceneReadyCharacterID = characterID
	session.townPositionSnapshot = snapshot
	session.townMu.Unlock()

	s.logGameEvent(session, "game-upper-enter-select-dungeon-pending-return-committed",
		"source", source,
		"char_id", characterID,
		"town_id", snapshot.TownID,
		"area_id", snapshot.AreaID,
		"position_x", snapshot.PositionX,
		"position_y", snapshot.PositionY,
		"movement_code", snapshot.MovementCode,
		"position_source", positionSource,
		"reason", "op15_can_be_first_town_side_request_after_back_to_village")
	return nil
}

// retireCompletedDungeonForTownSelect maps the settlement page's
// "select another dungeon" action onto the current op15/op27 selector owner.
// 86JP establishes that this action retires the completed run before entering
// the selector; the location itself is reconstructed from the current run's
// frozen op35 origin or the current Go/PVF town-transition builder.
func (s *Service) retireCompletedDungeonForTownSelect(session *gameSession) (bool, error) {
	retired, _, err := s.retireCompletedRunIntoSelector(session)
	return retired, err
}

// retireCompletedRunPartyIntoSelctor retires every party member's own completed run of
// this dungeon into the selector page.
//
// A party retries together, but its clients do not arrive together: the leader's own
// 再次挑战 is processed while the member's is still in flight, so the member is still
// standing inside its own completed run and its town scene is not finalized. The party
// entry preflight then refuses the leader's re-entry one gate later than the missing
// anchor did before -- "party member 22 selector is not ready:
// town_scene_player_state_not_finalized", live 2026-09-20 16:52:46Z -- and the leader is
// handed the town selector: the operator's "组队再次挑战直接返回城镇".
//
// This is the member's own 再次挑战 / 选择其他地下城 performed for it, with the same
// preconditions (retireCompletedRunIntoSelector refuses anything that is not a
// settlement-exit-ready completed run), so a member whose client already did it is
// simply skipped.
func (s *Service) retireCompletedRunPartyIntoSelctor(leader *gameSession, dungeonID int64, source string) int {
	if s == nil || leader == nil || leader.selectedCharacterID == 0 || dungeonID == 0 {
		return 0
	}
	retired := 0
	for _, member := range runtimePartyMembers(runtimePartyStateSnapshot(leader)) {
		if member.UserID == 0 || member.UserID == leader.selectedCharacterID {
			continue
		}
		follower, ok := s.onlineGameSession(member.UserID)
		if !ok || follower == nil {
			continue
		}
		follower.dungeon.mu.Lock()
		runtime := follower.dungeon.runtime
		inside := runtime != nil && runtime.Session != nil &&
			dungeonRuntimeOwnsCharacter(runtime, member.UserID) &&
			runtime.Session.Snapshot().Run.Status == worldmap.DungeonRunCompleted &&
			runtime.Session.Snapshot().Run.DungeonID == dungeonID
		follower.dungeon.mu.Unlock()
		if !inside {
			continue
		}
		did, _, err := s.retireCompletedRunIntoSelector(follower)
		if err != nil {
			s.logGameEvent(leader, "game-dungeon-party-retry-member-retire-blocked",
				"source", source,
				"leader_char_id", leader.selectedCharacterID,
				"char_id", member.UserID,
				"dungeon_id", dungeonID,
				"error", err)
			continue
		}
		if !did {
			continue
		}
		retired++
		s.logGameEvent(follower, "game-dungeon-party-retry-member-retired",
			"source", source,
			"leader_char_id", leader.selectedCharacterID,
			"char_id", member.UserID,
			"dungeon_id", dungeonID,
			"reason", "the_leader_retried_before_this_member_client_did")
	}
	return retired
}

// retireCompletedRunIntoSelector is the single-character action: retire this session's
// settlement-exit-ready completed run, leave the selector anchor behind, and report the
// dungeon that was retired. Callers that act for a whole party use
// retireCompletedRunPartyIntoSelctor around it.
func (s *Service) retireCompletedRunIntoSelector(session *gameSession) (bool, int64, error) {
	if session == nil || session.selectedCharacterID == 0 {
		return false, 0, nil
	}
	characterID := session.selectedCharacterID
	session.dungeon.mu.Lock()
	runtime := session.dungeon.runtime
	if runtime == nil || runtime.Session == nil ||
		!s.completedDungeonSettlementExitReady(runtime, characterID) {
		session.dungeon.mu.Unlock()
		return false, 0, nil
	}
	dungeonID := runtime.Dungeon.ID
	origin := runtime.townReturnOrigin
	session.dungeon.mu.Unlock()

	originSource := "frozen_pre_dungeon_op35_origin"
	if origin.CharacterID != characterID || !origin.PositionValid {
		ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
		transition, err := s.prepareCurrentDungeonTownTransitionForSession(ctx, session, characterID)
		cancel()
		if err != nil {
			return false, 0, err
		}
		origin = currentTownPositionSnapshot{
			CharacterID:   characterID,
			TownID:        transition.TownID,
			AreaID:        transition.AreaID,
			PositionX:     uint16(transition.PositionX),
			PositionY:     uint16(transition.PositionY),
			MovementCode:  transition.Direction,
			PositionValid: true,
		}
		originSource = transition.PositionSource
	}

	session.dungeon.mu.Lock()
	if session.dungeon.runtime != runtime ||
		!s.completedDungeonSettlementExitReady(runtime, characterID) {
		session.dungeon.mu.Unlock()
		return false, 0, nil
	}
	s.retireCurrentDungeonRuntimeLocked(session, runtime, "completed_run_retires_into_other_dungeon_selector")
	session.dungeon.mu.Unlock()
	if err := s.switchCurrentPetGrowthClock(session, currentPetGrowthClockTown, s.gameplayNow(), "completed_run_other_dungeon_selector"); err != nil {
		s.logGameEvent(session, "game-pet-growth-clock-start-deferred",
			"char_id", characterID,
			"mode", currentPetGrowthClockTown.String(),
			"source", "completed_run_other_dungeon_selector",
			"error", err)
	}

	resetDungeonEntrySceneGates(session)
	clearCurrentDungeonSelectContext(session)
	session.townMu.Lock()
	// Keep the pre-entry origin after the retirement. The client's next request on
	// this screen may be 返回城镇 rather than a selector op15, and by then this run
	// is gone: without the stash that return fell back to the persisted login town
	// (the Seria room), so the party came back invisible to each other and the next
	// dungeon entry was refused with 队员不在附近.
	session.completedRunTownReturnOrigin = origin
	// Re-bind the selector origin to the frozen pre-entry spot instead of clearing
	// it. The client may return to town straight from this selector page rather
	// than opening a new one, and that return resolves through the selector origin:
	// clearing it left the return with no origin at all, so it fell back to the
	// persisted login town (the Seria room). Measured 2026-09-19: a party that had
	// just cleared a run came back invisible to each other there, and the next
	// entry was refused with 队员不在附近 -- while the same return without a
	// completed run (origin still bound) landed correctly. This keeps both routes
	// on the same spot the direct 返回城镇 uses.
	session.townSelectorOriginSnapshot = origin
	session.townSelectorOriginBound = true
	session.townSceneReadyCharacterID = characterID
	session.townPositionSnapshot = origin
	session.townMu.Unlock()
	// Leave the selector-page presence anchor behind. This route retires a
	// COMPLETED run into the very page the op15 route opens, but by now the
	// character has usually been off the town roster since it entered the
	// dungeon: entry removed the registration and consumed the selector memo, so
	// neither the live roster nor a retired record answers "where does this
	// character stand". The party entry preflight reads exactly that resolution,
	// so the leader's next op16 was refused for every member with the retryable
	// reason-3 rejection - the reply this client answers with silence, which the
	// player reports as 点不动 (measured 2026-09-19 13:19:23Z, dungeon 3, party
	// of two, 16 identical requests).
	s.bindCompletedRunSelectorPresence(session, origin, "completed_run_retires_into_other_dungeon_selector")
	// This route retires a completed run straight into the page the op15 route opens,
	// so it needs the same party-scoped actor projection that route writes: the anchor
	// above only tells the SERVER where this character stands, while the client's own
	// party-entry proximity check reads scene actors, and this character is
	// deliberately absent from the town roster. Without it the next party entry can be
	// refused client-side with 队友不在附近 even though the server preflight passes.
	s.rebuildRuntimePartySelectorPageCoPresence(session, "completed_run_retires_into_other_dungeon_selector")
	s.logGameEvent(session, "game-dungeon-completed-other-selector-committed",
		"char_id", characterID,
		"dungeon_id", runtime.Dungeon.ID,
		"town_id", origin.TownID,
		"area_id", origin.AreaID,
		"position_x", origin.PositionX,
		"position_y", origin.PositionY,
		"position_source", originSource,
		"runtime_detached", true,
		"next_owner", "current_exe_op15_ack_fatigue_op27_selector_context")
	return true, dungeonID, nil
}

func (s *Service) sendCurrentTownEnterSelectContext(session *gameSession, source string) error {
	ready, reason := s.currentTownEnterSelectReady(session)
	if !ready {
		s.logGameEvent(session, "game-upper-town-enter-select-context-deferred",
			"source", source,
			"char_id", session.selectedCharacterID,
			"msg_id", currentDungeonContextMsgID,
			"reason", reason)
		return nil
	}
	if err := s.sendCurrentDungeonContextOp27(session, source); err != nil {
		return err
	}
	origin, originBound := bindCurrentTownSelectorOrigin(session)
	s.logGameEvent(session, "game-upper-town-enter-select-origin-bound",
		"source", source,
		"char_id", session.selectedCharacterID,
		"town_id", origin.TownID,
		"area_id", origin.AreaID,
		"position_x", origin.PositionX,
		"position_y", origin.PositionY,
		"position_valid", origin.PositionValid,
		"origin_bound", originBound,
		"reason", "freeze_current_town_origin_after_successful_op27_write")
	session.enterSelectDungeonContextSent = true
	return nil
}
