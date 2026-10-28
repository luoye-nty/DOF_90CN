package dnfbridge

import (
	"errors"
	"fmt"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
)

// publishRuntimePartyDisbandToSurvivors tells every member a dissolved party
// left behind that the party is gone.
//
// Operator rule (2026-09-22): 「一个人不成队」-- whenever a leave, a kick, a
// disconnect or a solo dungeon return drops a party to a single member, that
// member is detached too (RuntimePartyManager.disbandSingleMemberPartyLocked)
// and this function is what makes the client agree.
//
// Two frames, in this order, on purpose:
//
//  1. the native kind-2 one-member row. publishRuntimePartyMembershipSnapshot
//     documents why the aggregate used to be retained: "The native kind-2
//     transition owns the multiplayer-to-solo confirmation ... silently
//     clearing the projection here strands the remaining actor on a stale party
//     model". A disband therefore still hands the client that transition.
//  2. the party object is cleared - op9 kind-3 record removal with
//     party_marker=0xffff plus an empty 0x0099 - exactly the projection the
//     native leave handler publishes for the leaver's own frame
//     (handleOnlineLeaveParty), and the same "no party" shape the town
//     peer-selection path proved live on 2026-09-20.
//
// The survivor also leaves the attack squad's 编制, mirroring the leaver's
// unassign in handleOnlineLeaveParty: the squad label is derived from the
// ordinary party, so a party that no longer exists must not keep claiming one.
// unassignRuntimeRaidSquadOnPartyLeave skips anyone inside a dungeon run, so a
// squadmate still fighting a gate is untouched.
func (s *Service) publishRuntimePartyDisbandToSurvivors(
	origin *gameSession,
	previousState alignedcmd.PartyState,
	removedID uint16,
	skip *gameSession,
	source string,
) error {
	if s == nil || previousState.PartyID <= 0 {
		return nil
	}
	previousPartyID := uint16(previousState.PartyID)
	var publishErr error
	for _, member := range runtimePartyMembers(previousState) {
		if member.UserID == 0 || member.UserID == removedID {
			continue
		}
		session, online := s.onlineGameSession(member.UserID)
		if !online || session == nil || session == skip {
			continue
		}
		if solo, ok := runtimePartySoloStateForMember(previousState, member); ok {
			storeRuntimePartyState(session, solo)
			// A survivor whose socket is already gone is that survivor's own
			// problem: it must not become the origin's failure. See
			// swallowPeerFanoutWrite for the live incident behind this boundary.
			if err := s.swallowPeerFanoutWrite(
				origin,
				session,
				"runtime_party_disband_one_member_row",
				0,
				s.sendRuntimePartyMembershipReduction(session, solo),
			); err != nil {
				publishErr = errors.Join(publishErr, fmt.Errorf(
					"publish one-member party row to %d: %w", member.UserID, err))
			}
		}
		storeRuntimePartyState(session, alignedcmd.PartyState{})
		if err := s.swallowPeerFanoutWrite(
			origin,
			session,
			"runtime_party_disband_projection_clear",
			0,
			s.sendRuntimePartyMemberRemoved(
				session,
				previousPartyID,
				alignedcmd.PartyState{},
			),
		); err != nil {
			publishErr = errors.Join(publishErr, fmt.Errorf(
				"clear dissolved party projection for %d: %w", member.UserID, err))
		}
		s.logGameEvent(session, "game-party-disbanded-by-single-member-left",
			"char_id", member.UserID,
			"party_id", previousPartyID,
			"removed_id", removedID,
			"source", source,
			"reason", "a_party_that_drops_to_one_member_is_not_a_party")
	}
	return publishErr
}

// runtimePartySoloStateForMember narrows a party state to the one member the
// client has to keep through the native multiplayer-to-solo transition.
func runtimePartySoloStateForMember(
	state alignedcmd.PartyState,
	member alignedcmd.PartyMemberState,
) (alignedcmd.PartyState, bool) {
	if state.PartyID <= 0 || member.UserID == 0 {
		return alignedcmd.PartyState{}, false
	}
	solo := cloneRuntimePartyState(state)
	solo.Members = []alignedcmd.PartyMemberState{member}
	solo.UserID = member.UserID
	solo.IsLeader = true
	solo.Members[0].UserState = member.UserState
	return solo, true
}
