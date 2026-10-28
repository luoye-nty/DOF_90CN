package dnfbridge

import (
	"errors"
	"fmt"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
)

// A member who walks home alone out of a CLEARED dungeon leaves the party.
//
// Operator rule (2026-09-22): 「只要是自己单独回城了 视作直接退出队伍」.
//
// Live evidence for the old behaviour (server packet log 2026-09-22 09:22:08
// +08:00, party 2 = leader char 1 + follower char 22, dungeon 3):
//
//	char 1 sent op42 from the settlement page and the return ran
//	`current_exe_op31_completed_tutorial_or_settlement_timed_op42` with
//	interrupted_run=false. The completed route never touches the party
//	(dungeon_return_town.go only detaches on activeReturn), it only asks
//	rebuildRuntimePartyTownCoPresenceAfterReturn to reassemble the party, and
//	that defers while the peer is still inside
//	(`game-party-town-return-copresence-deferred member_id=22`). Nothing at all
//	was written to char 22's connection in that window, so char 22 kept
//	rendering char 1's dungeon actor, the party stayed intact and party chat
//	still reached both (`scope=party recipients=2` at 09:22:12). Char 22 never
//	returned, so the deferred rebuild never resolved: the state was stuck.
//
// The rule is applied to the COMPLETED route only, and only while a teammate is
// still inside the SAME run. The interrupted/abandoned route already detaches
// (party_forced_exit_leave.go) and a whole-party exit is marked with
// runtime.townReturnKeepParty. Scope is identical to the operator's choice
// 「只有队友还在同一个副本里才算」: a party member who is elsewhere (in town, or
// running a different dungeon) never triggers it, so "one member runs a dungeon
// alone while the party waits in town" keeps the party.
//
// There is deliberately no grace window and no room-type exemption: an ordinary
// dungeon and a raid gate are treated the same, per the same request.
func (s *Service) completedDungeonReturnLeavesParty(
	session *gameSession,
	runtime *runtimeDungeonState,
	partyState alignedcmd.PartyState,
) (bool, uint16, string) {
	if s == nil || session == nil || runtime == nil || session.selectedCharacterID == 0 {
		return false, 0, ""
	}
	if partyState.PartyID <= 0 {
		partyState = runtimePartyStateSnapshot(session)
	}
	members := runtimePartyMembers(partyState)
	if partyState.PartyID <= 0 || len(members) < 2 {
		return false, 0, ""
	}
	for _, member := range members {
		if member.UserID == 0 || member.UserID == session.selectedCharacterID {
			continue
		}
		inside, reason := s.runtimePartyPeerInsideSameDungeonRun(session, runtime, member.UserID)
		if inside {
			return true, member.UserID, reason
		}
	}
	return false, 0, ""
}

// runtimePartyPeerInsideSameDungeonRun reports whether one teammate is still
// inside the very run this session is leaving.
//
// "Same run" is proved by the shared party-dungeon roster first -- the admitted
// party dungeon binds every member's runtime to one immutable owner, and a
// member who already returned had that pointer released
// (releaseDungeonRuntimePayload), so the pointer is exactly "still inside this
// run, has not come home". A raid gate owns no party-dungeon owner, so the raid
// binding is the identity there: same raid key and same gate map.
func (s *Service) runtimePartyPeerInsideSameDungeonRun(
	session *gameSession,
	runtime *runtimeDungeonState,
	peerID uint16,
) (bool, string) {
	if s == nil || session == nil || runtime == nil || peerID == 0 ||
		peerID == session.selectedCharacterID {
		return false, ""
	}
	peer, online := s.onlineGameSession(peerID)
	if !online || peer == nil {
		return false, ""
	}
	peer.dungeon.mu.Lock()
	defer peer.dungeon.mu.Unlock()
	return runtimePartyPeerRuntimeInsideSameRun(runtime, peer.dungeon.runtime)
}

// runtimePartyPeerRuntimeInsideSameRun is the lock-free half of the test above;
// the caller must hold the peer's dungeon lock while it reads peerRuntime.
func runtimePartyPeerRuntimeInsideSameRun(
	runtime *runtimeDungeonState,
	peerRuntime *runtimeDungeonState,
) (bool, string) {
	if runtime == nil || peerRuntime == nil {
		return false, ""
	}
	if owner := runtime.partyDungeonOwner; owner != nil && peerRuntime.partyDungeonOwner == owner {
		return true, "shared_party_dungeon_roster"
	}
	if key := runtime.raidDungeon.RaidKey; key != 0 &&
		peerRuntime.raidDungeon.RaidKey == key &&
		peerRuntime.Dungeon.ID == runtime.Dungeon.ID {
		return true, "same_raid_gate_run"
	}
	return false, ""
}

// runtimePartyDungeonLeaveSurvivor is one member that has to lose the leaving
// actor from its dungeon scene, together with the stable slot its client
// addresses.
type runtimePartyDungeonLeaveSurvivor struct {
	userID     uint16
	generation uint64
	slot       byte
}

// projectRuntimePartyDungeonLeaveToSurvivors publishes the scene half of a
// party leave to the members that are still inside the run: op26 re-points the
// dungeon host when the leader left, and scene op6 retires the leaving actor.
//
// Scene op6 is the only packet that removes a peer model from the current
// client -- online_player_manager.broadcastTownPlayerAreaChange documents that
// the client reads 0x0006 as both a town-actor destroy and a party-member
// removal -- so a survivor that is never sent one keeps rendering the ghost,
// which is the reported symptom. The UDP relay restart is NOT part of this: the
// caller owns the membership projection and the relay.
func (s *Service) projectRuntimePartyDungeonLeaveToSurvivors(
	leavingCharacterID uint16,
	survivors []runtimePartyDungeonLeaveSurvivor,
	peerStillInsideSameRun func(peerRuntime *runtimeDungeonState) bool,
	leaderChanged bool,
	newLeaderSlot byte,
	op6Source string,
) error {
	if s == nil || leavingCharacterID == 0 || len(survivors) == 0 {
		return nil
	}
	leaveBody := buildTownUserLeaveBody(leavingCharacterID)
	var projectionErr error
	for _, survivor := range survivors {
		peerIdentity, current := s.onlineGameSessionCharacterSnapshot(survivor.userID)
		if !current || peerIdentity.session == nil || peerIdentity.generation != survivor.generation {
			continue
		}
		peer := peerIdentity.session
		peer.dungeon.mu.Lock()
		peerRuntime := peer.dungeon.runtime
		stillInside := peerRuntime != nil && peerStillInsideSameRun(peerRuntime)
		peer.dungeon.mu.Unlock()
		if !stillInside {
			continue
		}
		// op9 transfers the party-list leader, while op26 owns the live dungeon
		// controller/host slot. Leaving op26 behind keeps the survivor's combat
		// stream addressed to an actor that has already returned to town; the
		// visible symptom is valid skills resolving as one damage or no
		// authoritative hit at all.
		if leaderChanged && newLeaderSlot != 0xff {
			if err := s.sendGameUpperRawClass(peer, 26, []byte{newLeaderSlot}, 0); err != nil {
				projectionErr = errors.Join(projectionErr, fmt.Errorf(
					"transfer party dungeon host to slot %d for survivor %d: %w",
					newLeaderSlot,
					survivor.userID,
					err,
				))
			}
		}
		if err := s.sendCurrentSceneFixedClass0Packet(
			peer,
			currentTownUserLeaveMsgID,
			leaveBody,
			op6Source,
		); err != nil {
			projectionErr = errors.Join(projectionErr, fmt.Errorf(
				"retire returned party actor %d for survivor %d: %w",
				leavingCharacterID,
				survivor.userID,
				err,
			))
		}
	}
	return projectionErr
}

// leaveRuntimePartyOnSoloDungeonReturn is the no-owner half of the same rule: a
// run that owns no shared party-dungeon roster (a raid gate) still has to drop
// the returning member from the party and retire its actor for the teammates
// that are still inside the same gate.
func (s *Service) leaveRuntimePartyOnSoloDungeonReturn(
	session *gameSession,
	runtime *runtimeDungeonState,
	previousState alignedcmd.PartyState,
	peerID uint16,
	peerReason string,
	source string,
) error {
	if s == nil || session == nil || runtime == nil {
		return nil
	}
	sourceID := selectedCharacterID(session)
	if sourceID == 0 {
		return nil
	}
	if previousState.PartyID <= 0 {
		previousState = runtimePartyStateSnapshot(session)
	}
	members := runtimePartyMembers(previousState)
	if previousState.PartyID <= 0 || len(members) < 2 {
		return nil
	}
	identity, bound := s.boundGameSessionCharacterSnapshot(session)
	if !bound {
		return nil
	}

	nextState, result, removed := s.leaveManagedRuntimeParty(identity)
	if !removed {
		// The manager no longer holds this member, but its client cache still
		// shows the room. Clearing the projection and publishing the empty
		// window is idempotent and restores solo dungeon permission, exactly as
		// the native op13 leave handler does for the same state.
		s.clearRuntimePartyProjection(identity.character, identity.generation)
		s.logGameEvent(session, "game-party-solo-dungeon-return-leave-skipped",
			"char_id", sourceID,
			"party_id", previousState.PartyID,
			"peer_id", peerID,
			"peer_state", peerReason,
			"source", source,
			"reason", result.Reason)
		return s.sendRuntimePartyMemberRemoved(session, uint16(previousState.PartyID), alignedcmd.PartyState{})
	}
	s.logGameEvent(session, "game-party-solo-dungeon-return-left",
		"char_id", sourceID,
		"party_id", previousState.PartyID,
		"member_count", len(members),
		"remaining_party_id", nextState.PartyID,
		"peer_id", peerID,
		"peer_state", peerReason,
		"role", runtimePartyRole(sourceID, previousState),
		"source", source,
		"reason", "completed_dungeon_solo_town_return_leaves_the_party_inside_the_same_run")
	if err := s.sendRuntimePartyMemberRemoved(session, uint16(previousState.PartyID), nextState); err != nil {
		return err
	}
	if nextState.PartyID > 0 {
		s.syncRuntimePartyUDPRelay(nextState)
	} else {
		s.closeRuntimePartyUDPRelay(previousState.PartyID)
	}

	// Survivors come from the roster taken BEFORE the leave: with the
	// single-member disband rule the post-leave state can be empty while a
	// teammate is still standing inside the same run, and that teammate is
	// exactly the one that has to lose the returning actor from its scene. The
	// predicate below decides who is really still inside.
	survivors := s.runtimePartyDungeonLeaveSurvivorsFromState(previousState, sourceID)
	newLeaderID := nextState.UserID
	if newLeaderID == 0 && len(survivors) == 1 {
		// The party dissolved, so the member left inside is the whole run now.
		newLeaderID = survivors[0].userID
	}
	leaderChanged := previousState.UserID == sourceID && len(survivors) != 0
	newLeaderSlot := byte(0xff)
	if leaderChanged {
		newLeaderSlot = runtimePartySurvivorSlot(survivors, newLeaderID)
	}
	// Membership first, scene second: the survivor is told the party shrank (or
	// dissolved) before its scene loses the returning actor, exactly the order
	// the shared-roster detach uses.
	var projectionErr error
	if err := s.broadcastRuntimePartyMemberRemoved(session, previousState, nextState, sourceID, session); err != nil {
		projectionErr = errors.Join(projectionErr, err)
	}
	projectionErr = errors.Join(projectionErr, s.projectRuntimePartyDungeonLeaveToSurvivors(
		sourceID,
		survivors,
		func(peerRuntime *runtimeDungeonState) bool {
			inside, _ := runtimePartyPeerRuntimeInsideSameRun(runtime, peerRuntime)
			return inside
		},
		leaderChanged,
		newLeaderSlot,
		"party_member_solo_dungeon_return",
	))
	return projectionErr
}

// runtimePartyDungeonLeaveSurvivorsFromState lists the members that stay behind,
// excluding the leaver, keeping the stable slots their clients address and the
// live session generation the projection is bound to.
func (s *Service) runtimePartyDungeonLeaveSurvivorsFromState(
	state alignedcmd.PartyState,
	leavingID uint16,
) []runtimePartyDungeonLeaveSurvivor {
	members := runtimePartyMembers(state)
	survivors := make([]runtimePartyDungeonLeaveSurvivor, 0, len(members))
	for _, member := range members {
		if member.UserID == 0 || member.UserID == leavingID {
			continue
		}
		identity, online := s.onlineGameSessionCharacterSnapshot(member.UserID)
		if !online || identity.session == nil || identity.generation == 0 {
			continue
		}
		survivors = append(survivors, runtimePartyDungeonLeaveSurvivor{
			userID:     member.UserID,
			generation: identity.generation,
			slot:       member.Slot,
		})
	}
	return survivors
}

func runtimePartySurvivorSlot(survivors []runtimePartyDungeonLeaveSurvivor, userID uint16) byte {
	for _, survivor := range survivors {
		if survivor.userID == userID {
			return survivor.slot
		}
	}
	return 0xff
}

func runtimePartyRole(characterID uint16, state alignedcmd.PartyState) string {
	if state.PartyID > 0 && state.UserID == characterID {
		return "leader"
	}
	return "member"
}
