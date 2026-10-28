package dnfbridge

import (
	"fmt"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	"longheng.io/server/internal/modules/dnf/party"
)

// runtimePartyManagerForService is intentionally lazy because many protocol
// tests construct Service directly. Production initialization still creates it
// with Service, so all live party mutations share one central manager.
func (s *Service) runtimePartyManagerForService() *party.RuntimePartyManager {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.runtimePartyManager == nil {
		s.runtimePartyManager = party.NewRuntimePartyManager()
	}
	return s.runtimePartyManager
}

func (s *Service) runtimePartyMemberForIdentity(identity boundGameSessionCharacter) (party.RuntimePartyMember, bool) {
	if identity.session == nil || identity.character == 0 || identity.generation == 0 || !s.boundGameSessionCharacterCurrent(identity) {
		return party.RuntimePartyMember{}, false
	}
	state := runtimePartyStateSnapshot(identity.session)
	member := runtimePartyMember(identity.session, state)
	return party.RuntimePartyMember{
		UserID:            identity.character,
		SessionGeneration: identity.generation,
		State:             member,
	}, true
}

func (s *Service) publishRuntimePartySnapshot(snapshot party.RuntimePartySnapshot) alignedcmd.PartyState {
	if s == nil || snapshot.ID == 0 {
		return alignedcmd.PartyState{}
	}
	memberIDs := make([]uint16, 0, len(snapshot.Members))
	for _, member := range snapshot.Members {
		memberIDs = append(memberIDs, member.UserID)
		identity, ok := s.onlineGameSessionCharacterSnapshot(member.UserID)
		if !ok || identity.generation != member.SessionGeneration {
			continue
		}
		storeRuntimePartyState(identity.session, snapshot.StateFor(member.UserID))
	}
	// Attack-party squads follow a real ordinary party, including while the raid is
	// running: see runtimeRaidSquadsFollowOrdinaryParty. The party leader travels with
	// it so the party keeps its own squad number ("队长则保持队伍号不变").
	//
	// The leader is mirrored into the squad's roster row right after, and
	// unconditionally: that mirror is the answer the client's 队长标志 and its
	// leader-gated controls read (runtimeRaidMemberLeadsSquad), and a handover has to
	// survive the mid-run case the sync itself deliberately skips. Order matters --
	// after, so a party whose members are being put into a squad for the first time
	// records the leader against the squad the sync just gave them.
	s.syncRuntimeRaidSquadsWithOrdinaryPartyLedBy(memberIDs, snapshot.Leader)
	s.recordRuntimeRaidSquadPartyLeader(memberIDs, snapshot.Leader)
	return snapshot.StateFor(snapshot.Leader)
}

// publishRuntimePartyMembershipSnapshot retains the same aggregate even when
// one member remains. The native kind-2 transition owns the multiplayer-to-
// solo confirmation and keeps dungeon authority coherent; silently clearing
// the projection here strands the remaining actor on a stale party model.
func (s *Service) publishRuntimePartyMembershipSnapshot(snapshot party.RuntimePartySnapshot) alignedcmd.PartyState {
	return s.publishRuntimePartySnapshot(snapshot)
}

func (s *Service) clearRuntimePartyProjection(userID uint16, generation uint64) {
	identity, ok := s.onlineGameSessionCharacterSnapshot(userID)
	if !ok || identity.generation != generation {
		return
	}
	storeRuntimePartyState(identity.session, alignedcmd.PartyState{})
}

func (s *Service) createOrJoinManagedRuntimeParty(joiner, anchor *gameSession) (alignedcmd.PartyState, party.RuntimePartyResult, bool) {
	joinerIdentity, joinerOK := s.boundGameSessionCharacterSnapshot(joiner)
	anchorIdentity, anchorOK := s.boundGameSessionCharacterSnapshot(anchor)
	if !joinerOK || !anchorOK || joinerIdentity.character == anchorIdentity.character {
		return alignedcmd.PartyState{}, party.RuntimePartyResult{Reason: "invalid_session"}, false
	}
	manager := s.runtimePartyManagerForService()
	joinerMember, joinerMemberOK := s.runtimePartyMemberForIdentity(joinerIdentity)
	anchorMember, anchorMemberOK := s.runtimePartyMemberForIdentity(anchorIdentity)
	if manager == nil || !joinerMemberOK || !anchorMemberOK {
		return alignedcmd.PartyState{}, party.RuntimePartyResult{Reason: "stale_session"}, false
	}
	// SET_PARTY_INFO (op12) is a local client-window operation. It can create
	// a one-member projection on the invitee before that player accepts an
	// incoming invite, but it must never steal leadership from the inviter.
	// Resolve/import the packet target first: accepting or directly entering a
	// target always joins the target's party, creating that target's lobby when
	// it does not yet own one. A joiner's old central party is then detached by
	// RuntimePartyManager.Join as one atomic transition.
	anchorParty, anchorFound := s.bootstrapManagedRuntimePartyForSession(anchor)
	var result party.RuntimePartyResult
	if anchorFound {
		// Import a pre-central joiner's legacy projection only after the target
		// is known to own a party. This retains a real old-party survivor
		// transition during deployment, while a partyless target still wins over
		// an invitee's transient op12 singleton.
		if _, joinerManaged := manager.SnapshotByUser(joinerIdentity.character, joinerIdentity.generation); !joinerManaged &&
			runtimePartyStateSnapshot(joiner).PartyID > 0 {
			if imported, importedOK := s.bootstrapManagedRuntimePartyForSession(joiner); importedOK && imported.ID == anchorParty.ID {
				return s.publishRuntimePartySnapshot(anchorParty), party.RuntimePartyResult{OK: true, Party: anchorParty, TargetUser: joinerIdentity.character}, true
			}
		}
		result = manager.Join(anchorParty.ID, joinerMember)
	} else {
		// EnsureLeader is idempotent for concurrent invite/entry packets. It
		// never detaches a just-created target lobby as Create would.
		created := manager.EnsureLeader(anchorMember, runtimePartyStateSnapshot(anchor))
		if !created.OK {
			return alignedcmd.PartyState{}, created, false
		}
		result = manager.Join(created.Party.ID, joinerMember)
		if result.PriorLeave == nil && created.PriorLeave != nil {
			result.PriorLeave = created.PriorLeave
		}
	}
	if !result.OK {
		return alignedcmd.PartyState{}, result, false
	}
	if err := s.reconcileManagedPriorPartyLeave(result.PriorLeave, joinerIdentity.character); err != nil {
		result.Reason = "prior_party_projection_failed: " + err.Error()
		return alignedcmd.PartyState{}, result, false
	}
	state := s.publishRuntimePartySnapshot(result.Party)
	if state.PartyID <= 0 {
		return alignedcmd.PartyState{}, party.RuntimePartyResult{Reason: "party_projection_failed"}, false
	}
	return state, result, true
}

// reconcileManagedPriorPartyLeave projects the old half of a cross-party
// join. Membership has already moved under RuntimePartyManager's lock; this
// function only emits the corresponding client cache, relay and roster
// transition for remaining old-party members. Without it, a successful join
// could leave those survivors looking at a stale team until their next map
// change.
//
// A disbanded prior party is NOT automatically "nobody left": since the
// single-member disband rule (2026-09-22) the last member is detached as well,
// and that member still holds the old party table in its client cache. The
// disband projection is therefore published to whoever the party left behind
// instead of returning early.
func (s *Service) reconcileManagedPriorPartyLeave(prior *party.RuntimePartyResult, movedUser uint16) error {
	if prior == nil || !prior.OK || prior.Previous == nil || prior.Previous.ID == 0 {
		return nil
	}
	previous := *prior.Previous
	if prior.Disbanded || prior.Party.ID == 0 {
		s.closeRuntimePartyUDPRelay(int(previous.ID))
		return s.publishRuntimePartyDisbandToSurvivors(
			nil,
			previous.StateFor(previous.Leader),
			movedUser,
			nil,
			"managed_prior_party_disband",
		)
	}
	nextState := s.publishRuntimePartyMembershipSnapshot(prior.Party)
	if nextState.PartyID <= 0 {
		s.closeRuntimePartyUDPRelay(int(previous.ID))
	} else {
		s.syncRuntimePartyUDPRelay(nextState)
	}
	return s.broadcastRuntimePartyMemberRemoved(nil, previous.StateFor(previous.Leader), nextState, movedUser, nil)
}

// bootstrapManagedRuntimePartyForSession imports the old per-session
// projection exactly once when an already-open client window predates the
// central manager (including channel reconnect tests). From that point on the
// projection is only a client cache; every membership mutation goes through
// RuntimePartyManager. Production joins normally never take this path because
// op12 now creates the central party before its first roster packet is sent.
func (s *Service) bootstrapManagedRuntimePartyForSession(session *gameSession) (party.RuntimePartySnapshot, bool) {
	identity, bound := s.boundGameSessionCharacterSnapshot(session)
	if !bound {
		return party.RuntimePartySnapshot{}, false
	}
	manager := s.runtimePartyManagerForService()
	if manager == nil {
		return party.RuntimePartySnapshot{}, false
	}
	if snapshot, found := manager.SnapshotByUser(identity.character, identity.generation); found {
		return snapshot, true
	}
	state := runtimePartyStateSnapshot(session)
	if state.PartyID <= 0 {
		return party.RuntimePartySnapshot{}, false
	}
	leaderID := state.UserID
	if leaderID == 0 {
		leaderID = identity.character
	}
	leaderIdentity, leaderOnline := s.onlineGameSessionCharacterSnapshot(leaderID)
	if !leaderOnline {
		leaderIdentity = identity
		leaderID = identity.character
	}
	leaderMember, leaderOK := s.runtimePartyMemberForIdentity(leaderIdentity)
	if !leaderOK {
		return party.RuntimePartySnapshot{}, false
	}
	state.UserID = leaderID
	state.IsLeader = true
	created := manager.EnsureLeader(leaderMember, state)
	if !created.OK {
		return party.RuntimePartySnapshot{}, false
	}
	snapshot := created.Party
	for _, cachedMember := range runtimePartyMembers(state) {
		if cachedMember.UserID == 0 || cachedMember.UserID == leaderID {
			continue
		}
		memberIdentity, online := s.onlineGameSessionCharacterSnapshot(cachedMember.UserID)
		if !online {
			continue
		}
		member, memberOK := s.runtimePartyMemberForIdentity(memberIdentity)
		if !memberOK {
			continue
		}
		member.State = cachedMember
		joined := manager.Join(snapshot.ID, member)
		if joined.OK {
			snapshot = joined.Party
		}
	}
	s.publishRuntimePartySnapshot(snapshot)
	return snapshot, true
}

func (s *Service) leaveManagedRuntimeParty(identity boundGameSessionCharacter) (alignedcmd.PartyState, party.RuntimePartyResult, bool) {
	if !s.boundGameSessionCharacterCurrent(identity) {
		return alignedcmd.PartyState{}, party.RuntimePartyResult{Reason: "stale_session"}, false
	}
	manager := s.runtimePartyManagerForService()
	if manager == nil {
		return alignedcmd.PartyState{}, party.RuntimePartyResult{Reason: "party_manager_unavailable"}, false
	}
	if _, found := manager.SnapshotByUser(identity.character, identity.generation); !found {
		s.bootstrapManagedRuntimePartyForSession(identity.session)
	}
	result := manager.Leave(identity.character, identity.generation)
	if !result.OK {
		return alignedcmd.PartyState{}, result, false
	}
	s.clearRuntimePartyProjection(identity.character, identity.generation)
	if result.Disbanded || result.Party.ID == 0 {
		return alignedcmd.PartyState{}, result, true
	}
	return s.publishRuntimePartyMembershipSnapshot(result.Party), result, true
}

func (s *Service) ensureManagedRuntimePartyForSession(session *gameSession) (alignedcmd.PartyState, error) {
	identity, ok := s.boundGameSessionCharacterSnapshot(session)
	if !ok {
		return alignedcmd.PartyState{}, nil
	}
	manager := s.runtimePartyManagerForService()
	if manager == nil {
		return alignedcmd.PartyState{}, fmt.Errorf("runtime party manager unavailable")
	}
	state := runtimePartyStateSnapshot(session)
	if state.PartyID <= 0 {
		return state, nil
	}
	if _, memberOK := s.runtimePartyMemberForIdentity(identity); !memberOK {
		return alignedcmd.PartyState{}, nil
	}
	if snapshot, exists := manager.SnapshotByUser(identity.character, identity.generation); exists {
		result := manager.UpdateSettings(identity.character, identity.generation, state)
		if result.OK {
			return s.publishRuntimePartySnapshot(result.Party), nil
		}
		s.publishRuntimePartySnapshot(snapshot)
		return snapshot.StateFor(identity.character), nil
	}
	snapshot, created := s.bootstrapManagedRuntimePartyForSession(session)
	if !created {
		return alignedcmd.PartyState{}, fmt.Errorf("create runtime party")
	}
	return s.publishRuntimePartySnapshot(snapshot), nil
}

func (s *Service) hasManagedRuntimeParty(session *gameSession) bool {
	identity, bound := s.boundGameSessionCharacterSnapshot(session)
	if !bound {
		return false
	}
	if snapshot, found := s.runtimePartyManagerForService().SnapshotByUser(identity.character, identity.generation); found {
		return snapshot.ID != 0
	}
	if runtimePartyStateSnapshot(session).PartyID <= 0 {
		return false
	}
	_, found := s.bootstrapManagedRuntimePartyForSession(session)
	return found
}

// replaceManagedRuntimeParty expresses a higher-level grouping (currently a
// raid attack-party assignment) through the same central party graph. It
// intentionally does not reuse the caller's synthetic PartyID: the manager
// allocates a fresh generation just like ordinary create/leader transfer, then
// callers project the returned snapshot to the current EXE.
func (s *Service) replaceManagedRuntimeParty(state alignedcmd.PartyState) (alignedcmd.PartyState, bool) {
	members := runtimePartyMembers(state)
	if len(members) == 0 {
		return alignedcmd.PartyState{}, false
	}
	leaderID := state.UserID
	if leaderID == 0 {
		leaderID = members[0].UserID
	}
	leaderIdentity, online := s.onlineGameSessionCharacterSnapshot(leaderID)
	if !online {
		return alignedcmd.PartyState{}, false
	}
	leader, leaderOK := s.runtimePartyMemberForIdentity(leaderIdentity)
	if !leaderOK {
		return alignedcmd.PartyState{}, false
	}
	for _, cached := range members {
		if cached.UserID == leaderID {
			leader.State = cached
			break
		}
	}
	manager := s.runtimePartyManagerForService()
	if manager == nil {
		return alignedcmd.PartyState{}, false
	}
	created := manager.Create(leader, state)
	if !created.OK {
		return alignedcmd.PartyState{}, false
	}
	// Create detaches the leader from whatever party it was in. That prior party
	// still needs its own projection published -- with the single-member disband
	// rule it may have been dissolved outright, leaving one member that no
	// longer appears in the manager and would otherwise never be told. The raid
	// squad projection (materializeRuntimeRaidSquadParty) rebuilds squad parties
	// through this function, so a missing reconcile strands the member it just
	// moved to standby.
	if err := s.reconcileManagedPriorPartyLeave(created.PriorLeave, leaderID); err != nil {
		return alignedcmd.PartyState{}, false
	}
	snapshot := created.Party
	for _, cached := range members {
		if cached.UserID == leaderID {
			continue
		}
		identity, online := s.onlineGameSessionCharacterSnapshot(cached.UserID)
		if !online {
			return alignedcmd.PartyState{}, false
		}
		member, memberOK := s.runtimePartyMemberForIdentity(identity)
		if !memberOK {
			return alignedcmd.PartyState{}, false
		}
		member.State = cached
		joined := manager.Join(snapshot.ID, member)
		if !joined.OK {
			return alignedcmd.PartyState{}, false
		}
		snapshot = joined.Party
	}
	return s.publishRuntimePartySnapshot(snapshot), true
}

func (s *Service) publishAllManagedRuntimeParties() {
	manager := s.runtimePartyManagerForService()
	if manager == nil {
		return
	}
	for _, snapshot := range manager.Snapshots() {
		s.publishRuntimePartySnapshot(snapshot)
	}
}

func (s *Service) sendManagedRuntimePartySnapshots(state alignedcmd.PartyState) error {
	_, err := s.publishManagedRuntimePartySnapshots(state)
	return err
}

// publishManagedRuntimePartySnapshots reports whether the two-phase formation
// frame actually reached every member, as well as any transport error.
//
// The distinction matters because a deferral is not an error the caller can act
// on, but it is also not success: the party is then half-formed on the wire and
// only the bounded retry in party_publish_retry.go can complete it.
func (s *Service) publishManagedRuntimePartySnapshots(state alignedcmd.PartyState) (bool, error) {
	if state.PartyID <= 0 || state.PartyID >= int(^uint16(0)) {
		return false, nil
	}
	manager := s.runtimePartyManagerForService()
	snapshot, found := manager.SnapshotByID(uint16(state.PartyID))
	if !found {
		return false, fmt.Errorf("runtime party %d is no longer current", state.PartyID)
	}
	type snapshotTarget struct {
		session *gameSession
		state   alignedcmd.PartyState
	}
	targets := make([]snapshotTarget, 0, len(snapshot.Members))
	for _, member := range snapshot.Members {
		identity, online := s.onlineGameSessionCharacterSnapshot(member.UserID)
		// A deferral here leaves the party half-formed on the wire: the acceptor
		// already holds its class1 ACK, but nobody receives op11/op9/op153.
		// scheduleManagedRuntimePartySnapshotRetry is what retries it; without that
		// the party stayed 连接中 for its whole life. It must also be visible
		// without the packet log.
		if !online || identity.generation != member.SessionGeneration {
			s.logGameEvent(nil, "game-managed-runtime-party-snapshot-deferred",
				"party_id", state.PartyID,
				"member_id", member.UserID,
				"reason", "party_member_session_not_current_before_two_phase_publish")
			s.logWarn("dnfbridge party snapshot deferred",
				"party_id", state.PartyID,
				"member_id", member.UserID,
				"reason", "party_member_session_not_current_before_two_phase_publish")
			s.scheduleManagedRuntimePartySnapshotRetry(state, member.UserID,
				"party_member_session_not_current_before_two_phase_publish")
			return false, nil
		}
		memberState := snapshot.StateFor(member.UserID)
		if len(runtimePartyMembers(memberState)) > 1 && !s.runtimePartyPublishablePeerEndpoints(identity.session, memberState) {
			s.logGameEvent(identity.session, "game-managed-runtime-party-snapshot-deferred",
				"party_id", state.PartyID,
				"member_id", member.UserID,
				"reason", "native_peer_endpoint_table_incomplete_before_two_phase_publish")
			s.logWarn("dnfbridge party snapshot deferred",
				"party_id", state.PartyID,
				"member_id", member.UserID,
				"member_udp_port", currentPartyPeerPort(identity.session),
				"reason", "native_peer_endpoint_table_incomplete_before_two_phase_publish")
			s.recordRaidEvent(identity.session, "party_publish_deferred", map[string]any{
				"party_id":        state.PartyID,
				"member_id":       member.UserID,
				"member_udp_port": currentPartyPeerPort(identity.session),
				"member_count":    len(runtimePartyMembers(memberState)),
				"result":          "deferred",
				"reason":          "native_peer_endpoint_table_incomplete_before_two_phase_publish",
				"retry":           "bounded_publish_retry_armed",
			})
			s.scheduleManagedRuntimePartySnapshotRetry(state, member.UserID,
				"native_peer_endpoint_table_incomplete_before_two_phase_publish")
			return false, nil
		}
		targets = append(targets, snapshotTarget{session: identity.session, state: memberState})
	}

	// Formation is a two-phase publish across the whole party, not merely within
	// each TCP connection. Install op11 for every member first. Only after that
	// global barrier may any member receive op9 -> op153. Current NoPack starts
	// its native peer controller as op9 binds the scene party actor; the former
	// per-member op11 -> op9 -> op153 loop let the leader transmit its first UDP
	// handshake before the follower had consumed op11, producing a deterministic
	// one-way link (remote riding missing and party hits falling back to 1 HP).
	for _, target := range targets {
		if err := s.sendRuntimePartyPeerEndpointInfoLocal(
			target.session,
			target.state,
			"managed_runtime_party_snapshot_phase1",
		); err != nil {
			return false, err
		}
	}
	for _, target := range targets {
		if err := s.sendRuntimePartyRosterLocal(
			target.session,
			target.state,
			"managed_runtime_party_snapshot_phase2",
		); err != nil {
			return false, err
		}
	}
	for _, target := range targets {
		if err := s.broadcastCurrentPartyActorProjection(
			target.session,
			target.state,
			"managed_runtime_party_snapshot",
		); err != nil {
			return false, err
		}
	}
	// The peer projection above re-sends this party model as a kind-0 op9 to every
	// same-area peer -- the party's own members included -- and a frame that ends on a
	// bare op9 leaves the client's party HUD health bars empty (op153 is the only
	// carrier of the per-member HP/MP rows). broadcastCurrentPartyActorProjection now
	// closes each same-party observer with its own op153 rows, so no extra pass is
	// needed here; the ordering keeps every member's own frame last.
	//
	// The formation frame is now on the wire for this membership: retire any pending
	// retry for the party BEFORE returning, so a scheduled attempt can never replay
	// op11 over a link that is already up (op11 is a formation handshake; replaying it
	// makes the client tear the peer link down and re-enter 连接中).
	s.settleManagedRuntimePartySnapshotRetry(state.PartyID)
	return true, nil
}

// sendManagedRuntimePartyAggregateRefresh updates an already-connected party
// without replaying op11. Leadership and recruitment changes keep the same
// party model and peer routes; every member receives the complete op9
// aggregate followed by op153, matching the same-version native owner.
func (s *Service) sendManagedRuntimePartyAggregateRefresh(state alignedcmd.PartyState, source string) error {
	if state.PartyID <= 0 || state.PartyID >= int(^uint16(0)) {
		return nil
	}
	manager := s.runtimePartyManagerForService()
	snapshot, found := manager.SnapshotByID(uint16(state.PartyID))
	if !found {
		return fmt.Errorf("runtime party %d is no longer current", state.PartyID)
	}
	for _, member := range snapshot.Members {
		identity, online := s.onlineGameSessionCharacterSnapshot(member.UserID)
		if !online || identity.generation != member.SessionGeneration {
			continue
		}
		if err := s.sendRuntimePartyRosterLocal(identity.session, snapshot.StateFor(member.UserID), source); err != nil {
			return err
		}
	}
	return nil
}
