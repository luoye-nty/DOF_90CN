package dnfbridge

import (
	"fmt"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
)

// rebuildRuntimePartyTownCoPresenceAfterReturn restores the scene-owned town
// actors only after every same-area party member has restored its server-owned
// town presence.  The typed op24 return packet intentionally contains just
// the receiving character, so its old peer actors have been detached.  The
// result-page op72 route does not emit the older op132/op15 scene finalizer on
// the current EXE; completed-return presence is therefore the authoritative
// barrier for the native mode0/mode1/op9/op23 reconstruction.
func (s *Service) rebuildRuntimePartyTownCoPresenceAfterReturn(session *gameSession, source string) error {
	if s == nil || session == nil || session.selectedCharacterID == 0 || s.onlinePlayers == nil {
		return nil
	}
	state := runtimePartyStateSnapshot(session)
	members := runtimePartyMembers(state)
	if state.PartyID <= 0 || len(members) == 0 {
		return nil
	}

	sessions := make([]*gameSession, 0, len(members))
	for _, member := range members {
		memberSession, online := s.onlineGameSession(member.UserID)
		if !online || memberSession == nil || !currentPartyTownReturnSceneReady(memberSession) {
			// The gate used to be len(members) < 2, which made a one-member
			// party return with no roster at all. The dungeon transition tears
			// the party frame down on this client too, and 0x0099 is the only
			// HP-percent source for it, so the single-member case has to go
			// through this function as well (its peer loop is naturally empty).
			// Doing so turned the old silent return into an observable one.
			s.logGameEvent(session, "game-party-town-return-copresence-deferred",
				"source", source,
				"party_id", state.PartyID,
				"member_count", len(members),
				"member_id", member.UserID,
				"online", online,
				"reason", "member_town_presence_not_restored_yet")
			return nil
		}
		memberState := runtimePartyStateSnapshot(memberSession)
		if memberState.PartyID != state.PartyID || memberState.UserID != state.UserID ||
			len(runtimePartyMembers(memberState)) != len(members) {
			return nil
		}
		sessions = append(sessions, memberSession)
	}

	for _, receiver := range sessions {
		for _, member := range members {
			if member.UserID == receiver.selectedCharacterID ||
				!s.onlinePlayers.PeerInSameArea(receiver.selectedCharacterID, member.UserID) {
				continue
			}
			actor, found := s.onlinePlayers.PlayerForCharacter(member.UserID)
			if !found || actor.Session == nil {
				return fmt.Errorf("party return peer %d has no town actor", member.UserID)
			}
			if err := s.sendTownRemoteActorState(receiver, actor, source+"_party_peer_actor"); err != nil {
				return err
			}
			areaBody := buildCurrentTownUserAreaNotificationBody(
				currentSceneActorObjectKey(actor.CharacterID),
				actor.TownID,
				actor.AreaID,
				actor.PositionX,
				actor.PositionY,
				actor.Direction,
				actor.AreaState,
			)
			if err := s.sendCurrentSceneFixedClass0Packet(
				receiver,
				currentTownUserAreaNotificationMsgID,
				areaBody,
				source+"_party_peer_area",
			); err != nil {
				return err
			}
		}
		if err := s.sendRuntimePartyRosterLocal(receiver, stateForRuntimePartyReceiver(state, receiver.selectedCharacterID), source+"_party_roster"); err != nil {
			return err
		}
	}
	// The party half above is the SQUAD. A raid is several squads, and the members of
	// the other squads are not in this party at all, so nothing above installs them
	// into this client -- and after the final phase the raid's own mate-actor fan-out
	// no longer runs, because the raid is Finished.
	//
	// Live 2026-09-29 00:55:40 (raid 1507329, own build), the return after the heart
	// clear with the card window taken INSIDE the dungeon: the ONLY actor projections
	// in that window were the six ordered pairs inside party 2 (chars 1/9/35,
	// source=..._party_peer_actor). Party 3 (chars 36/37/38/39) appeared in none of
	// them, in either direction -- so the two squads came back to town invisible to
	// each other, which is the operator's "回城直接看不到2队的人了 2队也看不到1队".
	//
	// The record-only form is deliberate (mode0 + the op9 frame, no mode1 binding):
	// it is what the admission path uses for exactly this job -- letting the client
	// resolve a peer's portrait and level from its own user table -- and it is the
	// cheap half (measured 679 B / 1373 B per peer versus a 14-slot equipment
	// projection each, see syncRuntimeRaidMateActorRecords).
	s.syncRuntimeRaidMateActorRecords(session, source+"_raid_mate_actors")
	s.logGameEvent(session, "game-party-town-return-copresence-rebuilt",
		"source", source,
		"party_id", state.PartyID,
		"member_count", len(members),
		"reason", "all_same_party_members_restored_completed_town_presence_then_mode0_mode1_op9_op23_and_realtime_roster")
	return nil
}

// consumeBackToVillageEnterSelectFinalizerAfterTownPosition clears the
// back-to-village selector-return marker once a town-side op35 proves the
// client rebuilt its town actor and resumed movement. It has two effects. The
// next player op15 is then the real dungeon-gate intent rather than the return
// finalizer. And - the reason this matters in a party - it is the only path
// that clears the marker for a follower, who never walks to the gate and so
// never sends op15; currentPartyTownReturnSceneReady gates on this field, so
// without an op35 consumer a follower's marker stayed set forever, the party
// co-presence rebuild never fired, and the leader's own first gate op15 failed
// to finalize the return - the reported "walk to the entrance twice". Solo only
// the first effect applies; the party rebuild is a no-op below a two-member
// party. It is safe to run on every confirmed town op35: the marker is only
// ever set by a dungeon return, so an ordinary movement op35 is a no-op.
func (s *Service) consumeBackToVillageEnterSelectFinalizerAfterTownPosition(session *gameSession, source string) error {
	if session == nil || !session.backToVillageEnterSelectPending {
		return nil
	}
	session.backToVillageEnterSelectPending = false
	s.logGameEvent(session, "game-back-to-village-enter-select-finalizer-consumed-by-town-position",
		"source", source,
		"char_id", session.selectedCharacterID,
		"reason", "op35_proves_town_actor_resumed_before_next_player_dungeon_gate_op15")
	return s.rebuildRuntimePartyTownCoPresenceAfterReturn(session, source)
}

func currentPartyTownReturnSceneReady(session *gameSession) bool {
	if session == nil || session.selectedCharacterID == 0 || session.backToVillageEnterSelectPending {
		return false
	}
	session.dungeon.mu.Lock()
	pendingReturn := session.confirmedDungeonReturnStatePending
	hasDungeonRuntime := session.dungeon.runtime != nil
	session.dungeon.mu.Unlock()
	if pendingReturn || hasDungeonRuntime {
		return false
	}
	session.townMu.Lock()
	ready := session.townSceneReadyCharacterID == session.selectedCharacterID &&
		session.townPositionSnapshot.CharacterID == session.selectedCharacterID &&
		session.townPositionSnapshot.PositionValid
	session.townMu.Unlock()
	return ready
}

func stateForRuntimePartyReceiver(state alignedcmd.PartyState, receiverID uint16) alignedcmd.PartyState {
	state = cloneRuntimePartyState(state)
	state.IsLeader = state.UserID == receiverID
	return state
}
