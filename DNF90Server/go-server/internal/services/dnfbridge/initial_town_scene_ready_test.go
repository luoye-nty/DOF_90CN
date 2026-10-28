package dnfbridge

import "testing"

// TestCurrentSceneTransitionReadyAfterOrdinaryTownMove pins the upstream branch
// this fork had dropped. The op24/op36 town-area move path retires the initial
// town route (initialTownRouteCharacterID = 0, stage = idle) and then records
// townSceneReadyCharacterID for the same character -- it never re-arms the
// route. Without the "retired route, bound actor" branch the predicate is false
// for the rest of the session, and every 公会 caller that guards on it fails
// with the module's generic error byte.
//
// Live 2026-10-04 19:44: the player teleported to 安徒恩战舰, moved areas twice,
// then every 公会金库 deposit was answered op247 {0, 196} and the client showed
// dstr 21684 「公会仓库尚未完成加载。」— the cargo itself was fine (op261 had
// just answered capacity 56 / zero rows).
func TestCurrentSceneTransitionReadyAfterOrdinaryTownMove(t *testing.T) {
	service, session, _, _ := newLegacyTownSceneReadySkillTest(t)

	// Login shape: the initial town route is live and committed.
	if ready, source := service.currentSceneTransitionReadyForState(session); !ready {
		t.Fatalf("committed initial town route read as unready: %s", source)
	}

	// Ordinary town-area move: retire the route, keep the actor bound.
	session.townMu.Lock()
	session.initialTownRouteCharacterID = 0
	session.initialTownRouteStage = currentInitialTownRouteIdle
	session.townSceneReadyCharacterID = session.selectedCharacterID
	session.townMu.Unlock()

	ready, source := service.currentSceneTransitionReadyForState(session)
	if !ready || source != "committed_town_scene_after_initial_route" {
		t.Fatalf("town move left the scene unready: ready=%t source=%s", ready, source)
	}
	if ready, source, busy := service.currentSceneTransitionReadyForStateNonBlocking(session); !ready || busy ||
		source != "committed_town_scene_after_initial_route" {
		t.Fatalf("non-blocking read disagrees: ready=%t busy=%t source=%s", ready, busy, source)
	}

	// The same session states must not read as ready in a dungeon: the dungeon
	// path owns its own boundary (postStartMapPlayerStateSent), and a retired
	// town route is not evidence that a dungeon scene exists.
	session.dungeon.runtime = &runtimeDungeonState{}
	ready, source = service.currentSceneTransitionReadyForState(session)
	if ready || source != "actor_binding_or_scene_transition_incomplete" {
		t.Fatalf("dungeon with a retired town route read as ready: ready=%t source=%s", ready, source)
	}
	if ready, source, busy := service.currentSceneTransitionReadyForStateNonBlocking(session); ready || busy ||
		source != "actor_binding_or_scene_transition_incomplete" {
		t.Fatalf("non-blocking dungeon read disagrees: ready=%t busy=%t source=%s", ready, busy, source)
	}
	session.dungeon.runtime = nil

	// A different character's bound scene must not authorize this session, and a
	// session with no selected character (character select) stays unready.
	session.townMu.Lock()
	session.townSceneReadyCharacterID = session.selectedCharacterID + 1
	session.townMu.Unlock()
	if ready, source := service.currentSceneTransitionReadyForState(session); ready {
		t.Fatalf("another character's town scene authorized the session: %s", source)
	}
	session.townMu.Lock()
	session.townSceneReadyCharacterID = session.selectedCharacterID
	session.selectedCharacterID = 0
	session.townMu.Unlock()
	if ready, source := service.currentSceneTransitionReadyForState(session); ready {
		t.Fatalf("character select read as ready: %s", source)
	}
}
