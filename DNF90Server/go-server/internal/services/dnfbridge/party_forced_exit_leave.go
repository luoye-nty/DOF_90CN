package dnfbridge

import (
	"longheng.io/server/internal/modules/dnf/alignedcmd"
)

// leaveRuntimePartyOnAbandonedDungeonExit removes the character from its party
// when it abandons a dungeon run, per the operator's rule: a member who force
// quits while the party still has other members leaves the party; the last
// remaining member stays. A one-member party is left untouched because leaving
// it is indistinguishable from staying and would only churn the client window.
//
// Only an abandoned run reaches this. A completed clear returns to town through
// its own settlement route and must keep the party together for the next run.
//
// This is the non-party-dungeon half of the rule. A run that was admitted as a
// shared party dungeon owns a runtimePartyDungeonOwner, and
// detachRuntimePartyDungeonMemberForTownReturn already removes the returning
// member from both the central party and that shared roster under the owner's
// world lock. A member that entered alone while its town party stayed behind
// has no owner at all, so nothing removed it and the abandoned run left the
// quitter still rostered - which is what this closes. The two are mutually
// exclusive at the single abandon boundary in sendCurrentActiveDungeonReturnToTown.
func (s *Service) leaveRuntimePartyOnAbandonedDungeonExit(
	session *gameSession,
	previousState alignedcmd.PartyState,
	source string,
) error {
	if s == nil || session == nil {
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
		s.logGameEvent(session, "game-party-forced-dungeon-exit-leave-skipped",
			"char_id", sourceID,
			"party_id", previousState.PartyID,
			"source", source,
			"reason", result.Reason)
		return s.sendRuntimePartyMemberRemoved(session, uint16(previousState.PartyID), alignedcmd.PartyState{})
	}
	s.logGameEvent(session, "game-party-forced-dungeon-exit-left",
		"char_id", sourceID,
		"party_id", previousState.PartyID,
		"member_count", len(members),
		"remaining_party_id", nextState.PartyID,
		"source", source,
		"reason", "abandoned_dungeon_run_leaves_a_party_that_still_has_other_members")
	if err := s.sendRuntimePartyMemberRemoved(session, uint16(previousState.PartyID), nextState); err != nil {
		return err
	}
	if nextState.PartyID > 0 {
		s.syncRuntimePartyUDPRelay(nextState)
	} else {
		s.closeRuntimePartyUDPRelay(previousState.PartyID)
	}
	return s.broadcastRuntimePartyMemberRemoved(session, previousState, nextState, sourceID, session)
}
