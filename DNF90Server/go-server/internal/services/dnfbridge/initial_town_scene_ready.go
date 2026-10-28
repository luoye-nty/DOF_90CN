package dnfbridge

func markReturnSelectTownReentry(session *gameSession, previousCharacterID uint16) {
	if session == nil {
		return
	}
	session.townMu.Lock()
	session.returnSelectTownReentryPending = previousCharacterID != 0
	session.accountSharedInventoryRebindPending = previousCharacterID != 0
	session.returnSelectPreviousCharacterID = previousCharacterID
	session.returnSelectRetainedActorRemoved = false
	session.townMu.Unlock()
}

func clearReturnSelectTownReentry(session *gameSession) {
	if session == nil {
		return
	}
	session.townMu.Lock()
	session.returnSelectTownReentryPending = false
	session.accountSharedInventoryRebindPending = false
	session.returnSelectPreviousCharacterID = 0
	session.returnSelectRetainedActorRemoved = false
	session.townMu.Unlock()
}

func returnSelectTownReentryPending(session *gameSession) bool {
	if session == nil {
		return false
	}
	session.townMu.Lock()
	pending := session.returnSelectTownReentryPending
	session.townMu.Unlock()
	return pending
}

func sameCharacterReturnSelect(session *gameSession, characterID uint16) bool {
	if session == nil || characterID == 0 {
		return false
	}
	session.townMu.Lock()
	same := session.returnSelectTownReentryPending &&
		session.returnSelectPreviousCharacterID == characterID
	session.townMu.Unlock()
	return same
}

// currentSceneTransitionReadyForState identifies the first boundary at which
// the selected actor and its scene transition both exist.  A town route still
// needs the current player/object finalization chain after this point.
//
// Two shapes count as ready, and the second one is the one that keeps every
// 公会 action alive: the initial (login) town route committed its transition, or
// an ordinary op24/op36 town-area move retired that route while the actor stayed
// bound to the same scene. The move path itself is what creates the second
// shape (town_move.go retires the route and then records
// townSceneReadyCharacterID), so without this branch the predicate can never be
// true again for the rest of the session, and every guarded caller fails with
// its module's generic error:
//
//   - 公会金库 放入/取出/移动 → error 196, which the CN client renders as
//     「公会仓库尚未完成加载。」(dstr 21684) — live 2026-10-04 19:44, right after
//     the 安徒恩 teleport + two op36 area moves;
//   - 守护珠镶嵌、公会战商店/任务、公会管理 use this same predicate.
//
// This matches upstream (DNF_NEW internal/services/dnfbridge/
// initial_town_scene_ready.go); this fork had dropped the townReady branch.
func (s *Service) currentSceneTransitionReadyForState(session *gameSession) (bool, string) {
	if session == nil {
		return false, "session_missing"
	}
	if session.postStartMapPlayerStateSent {
		return true, "dungeon_post_op29_player_state"
	}
	session.townMu.Lock()
	ready := session.initialTownRouteCharacterID != 0 &&
		session.initialTownRouteCharacterID == session.selectedCharacterID &&
		session.initialTownRouteStage >= currentInitialTownRouteTransitionSent
	townReady := session.selectedCharacterID != 0 &&
		session.townSceneReadyCharacterID == session.selectedCharacterID &&
		session.initialTownRouteCharacterID == 0
	session.townMu.Unlock()
	if ready {
		return true, "initial_town_actor_bound_and_transition_sent"
	}
	// Ordinary op24 town moves retire the initial route but keep a bound actor.
	if townReady {
		session.dungeon.mu.Lock()
		inDungeon := session.dungeon.runtime != nil
		session.dungeon.mu.Unlock()
		if !inDungeon {
			return true, "committed_town_scene_after_initial_route"
		}
	}
	return false, "actor_binding_or_scene_transition_incomplete"
}

// currentSceneTransitionReadyForStateNonBlocking is the peer-safe read of
// currentSceneTransitionReadyForState: it never waits for another session's town
// lock. A caller that already holds one session's town lock must not block on a
// peer's, because the peer can be running the mirror image of that same call at
// the same moment. Live incident 2026-09-22 20:06: two characters of one guild
// logged in one second apart, each session held its own townMu through
// sendCurrentInitialTownRoute -> publishCurrentTownPresence and then waited for
// the other's inside the guild presence refresh, and both sessions wedged
// forever.
//
// busy reports that the session's town state could not be read right now. It is
// never true for an idle session, so a call that does not race another lock
// holder keeps the blocking form's answer.
func (s *Service) currentSceneTransitionReadyForStateNonBlocking(session *gameSession) (ready bool, source string, busy bool) {
	if session == nil {
		return false, "session_missing", false
	}
	if session.postStartMapPlayerStateSent {
		return true, "dungeon_post_op29_player_state", false
	}
	if !session.townMu.TryLock() {
		return false, "session_town_state_busy", true
	}
	ready = session.initialTownRouteCharacterID != 0 &&
		session.initialTownRouteCharacterID == session.selectedCharacterID &&
		session.initialTownRouteStage >= currentInitialTownRouteTransitionSent
	townReady := session.selectedCharacterID != 0 &&
		session.townSceneReadyCharacterID == session.selectedCharacterID &&
		session.initialTownRouteCharacterID == 0
	session.townMu.Unlock()
	if ready {
		return true, "initial_town_actor_bound_and_transition_sent", false
	}
	// Same "ordinary town move retired the route but kept a bound actor" shape
	// the blocking form accepts -- a peer session that just moved areas must not
	// read as unready here.
	if townReady {
		session.dungeon.mu.Lock()
		inDungeon := session.dungeon.runtime != nil
		session.dungeon.mu.Unlock()
		if !inDungeon {
			return true, "committed_town_scene_after_initial_route", false
		}
	}
	return false, "actor_binding_or_scene_transition_incomplete", false
}

// currentSceneActorReadyForState is the stronger finish-loading predicate.
// Dungeon entry reaches it through the post-op29 state chain. Completed town
// login prepares the full mode1/mode3/object-finalizer/display/placement chain
// before op24 and commits the transition only after that chain succeeds.
func (s *Service) currentSceneActorReadyForState(session *gameSession) (bool, string) {
	if session == nil {
		return false, "session_missing"
	}
	if session.postStartMapPlayerStateSent {
		return true, "dungeon_post_op29_player_state"
	}
	session.townMu.Lock()
	ready := session.initialTownRouteCharacterID != 0 &&
		session.initialTownRouteCharacterID == session.selectedCharacterID &&
		session.initialTownRouteStage >= currentInitialTownRoutePlayerStateSent
	session.townMu.Unlock()
	if ready {
		session.townPostTransition.mu.Lock()
		postTransitionStage := session.townPostTransition.stage
		postTransitionCharacterID := session.townPostTransition.characterID
		session.townPostTransition.mu.Unlock()
		if postTransitionCharacterID == session.selectedCharacterID &&
			postTransitionStage >= currentTownPostTransitionPending &&
			postTransitionStage < currentTownPostTransitionComplete {
			return false, "post_op24_actor_hud_generation_incomplete"
		}
		return true, "initial_town_player_state_finalized"
	}
	return false, "player_state_or_scene_finalizer_incomplete"
}
