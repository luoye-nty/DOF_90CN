package dnfbridge

import (
	"time"

	"longheng.io/server/internal/modules/dnf/dnfenum"
	"longheng.io/server/internal/modules/dnf/party"
	"longheng.io/server/internal/modules/dnf/raid"
)

// Attack-party admission rides on the generic class-1/op10 peer request with
// mode 10. The same wire mode is directional: a raid leader clicking an
// outsider invites that outsider, an outsider clicking a raid member applies to
// that raid's current leader. Both directions end with the responder's op11,
// and only an accepted response admits the applicant through the same roster
// rule as the native op668 path.
//
// Hypothesis source (never authority for the wire layout): the read-only 86JP
// C# server, RaidHandler.PeerAdmission.cs. The op7 prompt body itself comes
// from party.BuildRequestPeerNotice, which follows the current EXE reader.
const (
	runtimeRaidPeerRequestMode      byte = 10
	runtimeRaidPeerAdmissionTimeout      = 30 * time.Second
)

type runtimeRaidPeerAdmissionKey struct {
	Responder uint16
	Requester uint16
}

type runtimeRaidPeerAdmission struct {
	RaidKey       uint32
	IsApplication bool
	Requester     boundGameSessionCharacter
	Responder     boundGameSessionCharacter
	Applicant     boundGameSessionCharacter
	Leader        uint16
	PeerValue     uint32
	ExpiresAt     time.Time
}

// resolveRuntimeRaidPeerAdmission decides who is admitting whom. Only a leader
// may invite; anyone outside may apply; two members of different raids, or two
// outsiders, have no admission relationship.
func resolveRuntimeRaidPeerAdmission(requesterInRaid, targetInRaid, requesterIsLeader bool) (isApplication bool, ok bool) {
	switch {
	case requesterInRaid && !targetInRaid:
		return false, requesterIsLeader
	case !requesterInRaid && targetInRaid:
		return true, true
	default:
		return false, false
	}
}

func runtimeRaidOpenForPeerAdmission(state runtimeRaidState) bool {
	return state.RaidKey != 0 &&
		state.TailFlag != 1 &&
		!state.Started &&
		!state.StartPending &&
		len(state.Members) < raid.MaxAttackPartyMembers
}

// handleRuntimeRaidPeerRequest owns mode-10 op10 requests inside a team
// channel. It reports whether the request was consumed so the ordinary party
// path never treats a one-member raid as a four-member party.
func (s *Service) handleRuntimeRaidPeerRequest(session *gameSession, parsed party.RequestPeerRequest) (bool, error) {
	if s == nil || session == nil || parsed.Mode != runtimeRaidPeerRequestMode || runtimeRaidModeForSession(session) == 0 {
		return false, nil
	}
	return s.recordRuntimeRaidPeerAdmission(session, parsed)
}

// recordRuntimeRaidPeerAdmission is the mode-10 admission itself, without the
// team-channel gate. Split out of handleRuntimeRaidPeerRequest so the op820
// browse-window application can reuse it: that request names the raid by key
// instead of by a clicked actor, and the applicant does not have to be inside a
// team channel for the leader to be told about it -- which is exactly the
// same-channel/different-area case where clicking the leader's actor is
// impossible.
func (s *Service) recordRuntimeRaidPeerAdmission(session *gameSession, parsed party.RequestPeerRequest) (bool, error) {
	if s == nil || session == nil || parsed.Mode != runtimeRaidPeerRequestMode {
		return false, nil
	}
	requester, requesterBound := s.boundGameSessionCharacterSnapshot(session)
	target, targetBound := s.onlineGameSessionCharacterSnapshot(parsed.TargetID)
	if !requesterBound || !targetBound || target.session == nil || requester.character == target.character {
		s.logGameEvent(session, "game-raid-peer-request-rejected",
			"target_id", parsed.TargetID, "reason", "requester_or_target_not_bound")
		return true, nil
	}
	if currentExpertJobSessionInDungeon(session) || currentExpertJobSessionInDungeon(target.session) {
		s.logGameEvent(session, "game-raid-peer-request-rejected",
			"target_id", parsed.TargetID, "reason", "requester_or_target_in_dungeon")
		return true, nil
	}

	requesterRaid, requesterInRaid := s.runtimeRaidForMember(requester.character)
	targetRaid, targetInRaid := s.runtimeRaidForMember(target.character)
	if requesterInRaid && targetInRaid && requesterRaid.RaidKey == targetRaid.RaidKey {
		// Clicking a teammate is a status refresh, not an admission.
		s.logGameEvent(session, "game-raid-peer-request-same-raid-refresh",
			"raid_key", requesterRaid.RaidKey, "target_id", parsed.TargetID)
		return true, normalizeRuntimeRaidRejoinError(s.sendRuntimeRaidRejoinRecovery(requester, requesterRaid, s.runtimeRaidRejoinNow()))
	}
	requesterIsLeader := requesterInRaid && requesterRaid.LeaderID == requester.character
	isApplication, ok := resolveRuntimeRaidPeerAdmission(requesterInRaid, targetInRaid, requesterIsLeader)
	if !ok {
		s.logGameEvent(session, "game-raid-peer-request-rejected",
			"target_id", parsed.TargetID,
			"requester_in_raid", requesterInRaid,
			"target_in_raid", targetInRaid,
			"requester_is_leader", requesterIsLeader,
			"reason", "direction_not_admissible")
		return true, nil
	}
	state := requesterRaid
	if isApplication {
		state = targetRaid
	}
	if !runtimeRaidOpenForPeerAdmission(state) {
		s.logGameEvent(session, "game-raid-peer-request-rejected",
			"raid_key", state.RaidKey,
			"target_id", parsed.TargetID,
			"started", state.Started,
			"start_pending", state.StartPending,
			"member_count", len(state.Members),
			"reason", "raid_not_open")
		return true, nil
	}

	responder := target
	applicant := target
	if isApplication {
		applicant = requester
		leader, leaderOnline := s.onlineGameSessionCharacterSnapshot(state.LeaderID)
		if !leaderOnline || leader.session == nil {
			s.logGameEvent(session, "game-raid-peer-request-rejected",
				"raid_key", state.RaidKey, "leader_id", state.LeaderID, "reason", "leader_offline")
			return true, nil
		}
		responder = leader
	}

	now := s.gameplayNow()
	pending := runtimeRaidPeerAdmission{
		RaidKey:       state.RaidKey,
		IsApplication: isApplication,
		Requester:     requester,
		Responder:     responder,
		Applicant:     applicant,
		Leader:        state.LeaderID,
		PeerValue:     parsed.Value0,
		ExpiresAt:     now.Add(runtimeRaidPeerAdmissionTimeout),
	}
	key := runtimeRaidPeerAdmissionKey{Responder: responder.character, Requester: requester.character}
	s.storeRuntimeRaidPeerAdmission(key, pending, now)

	noticeBody := party.BuildRequestPeerNotice(requester.character, parsed, s.currentPeerNoticeStats(requester.session, parsed))
	// Install the applicant's actor record before the prompt. The client resolves op7's
	// source through its own user table and drops the whole notification when the
	// key is missing (sub_1D82CF0 reads u16 source, calls sub_2699A30, and jumps
	// straight to its epilogue when that returns null). Measured: with the
	// applicant standing in a Seria room -- a character-private scene the server
	// deliberately keeps out of the public presence map -- the leader received the
	// packet and showed nothing at all, and the same request worked as soon as the
	// applicant walked out into the public town.
	//
	// Record form (mode0 + op9), not the scene-derived binding form: mode0 is the
	// half that fills that user table -- name, level, portrait -- while mode1 is the
	// half that binds equipment and skill pages and is documented as the RECEIVER's
	// own stat block (see sendTownRemoteActorRecord / sendTownRemoteActorState). A
	// prompt needs the applicant resolvable, not his equipment, and building the
	// binding form here was also the single most expensive step of the whole request
	// (see the note on the removed reverse install below).
	s.sendRuntimeRaidPeerActorRecord(state, responder.session, requester.character, "raid_peer_request_source_actor")
	// The reverse install -- the LEADER's own record into the applicant -- has been
	// removed on purpose. It had no measurement behind it (the leader's prompt is the
	// only direction with a live symptom), it was redundant (an application is made
	// through the raid directory, whose browse had already installed that row's actor
	// into the applicant: measured 2026-09-26 13:56:42.030, 1.3 s before this request),
	// and on admission syncRuntimeRaidMateActors installs both directions anyway.
	// Cost of sending it: the leader's own record (mode0 472 B + mode1 1373 B with 15
	// equipment rows) plus ~87 ms of equipment/PVF building that sat IN FRONT of the
	// leader's prompt -- the operator's "申请后只有团长会卡一下".
	if err := s.sendGameUpperRawClassCodec(responder.session, 0x0007, noticeBody, 0, true); err != nil {
		s.removeRuntimeRaidPeerAdmission(key)
		return true, err
	}
	s.logGameEvent(session, "game-raid-peer-request-recorded",
		"raid_key", state.RaidKey,
		"requester_id", requester.character,
		"target_id", parsed.TargetID,
		"responder_id", responder.character,
		"applicant_id", applicant.character,
		"application", isApplication,
		"expires_in_seconds", int(runtimeRaidPeerAdmissionTimeout/time.Second))
	return true, nil
}

// handleRuntimeRaidPeerResponse owns mode-10 op11 responses. parsed.TargetID is
// the original requester; the responding session is the party that was
// prompted. A declined or expired prompt only drops the pending record.
func (s *Service) handleRuntimeRaidPeerResponse(session *gameSession, parsed party.ResponsePeerRequest, accepted bool) (bool, error) {
	if s == nil || session == nil || parsed.Mode != runtimeRaidPeerRequestMode {
		return false, nil
	}
	responder, responderBound := s.boundGameSessionCharacterSnapshot(session)
	if !responderBound {
		return true, nil
	}
	key := runtimeRaidPeerAdmissionKey{Responder: responder.character, Requester: parsed.TargetID}
	now := s.gameplayNow()
	pending, found := s.takeRuntimeRaidPeerAdmission(key, now)
	if !found {
		s.logGameEvent(session, "game-raid-peer-response-ignored",
			"requester_id", parsed.TargetID, "reason", "no_pending_or_expired")
		return true, nil
	}
	if err := s.sendGameUpperSuccess(session, uint16(dnfenum.CmdPacketResponsePeer), party.BuildResponsePeerAckPayload(parsed.TargetID, parsed.Mode)); err != nil {
		return true, err
	}
	if !accepted {
		s.logGameEvent(session, "game-raid-peer-response-declined",
			"raid_key", pending.RaidKey, "requester_id", parsed.TargetID, "application", pending.IsApplication)
		return true, nil
	}
	// Both parties must still be the exact sessions that were prompted; a
	// character reselection or reconnect in between voids the prompt.
	if !s.boundGameSessionCharacterCurrent(pending.Responder) || !s.boundGameSessionCharacterCurrent(pending.Applicant) ||
		pending.Responder.character != responder.character || pending.Responder.generation != responder.generation {
		s.logGameEvent(session, "game-raid-peer-response-ignored",
			"raid_key", pending.RaidKey, "reason", "session_identity_changed")
		return true, nil
	}
	if currentExpertJobSessionInDungeon(pending.Applicant.session) {
		s.logGameEvent(session, "game-raid-peer-response-ignored",
			"raid_key", pending.RaidKey, "reason", "applicant_in_dungeon")
		return true, nil
	}
	approvingLeader := pending.Responder
	if !pending.IsApplication {
		approvingLeader = pending.Requester
	}
	state, added, ok := s.admitRuntimeRaidPeer(pending.RaidKey, approvingLeader.character, pending.Applicant.character)
	if !ok {
		s.logGameEvent(session, "game-raid-peer-response-join-rejected",
			"raid_key", pending.RaidKey,
			"applicant_id", pending.Applicant.character,
			"leader_id", approvingLeader.character,
			"reason", "raid_changed_or_roster_rule")
		return true, nil
	}
	if !added {
		s.logGameEvent(session, "game-raid-peer-response-already-member",
			"raid_key", pending.RaidKey, "applicant_id", pending.Applicant.character)
		return true, nil
	}
	// Bind the applicant exactly like a fresh 664: the object must exist before
	// the create-format success ACK selects the raid key locally, and the mode=3
	// roster afterwards materializes every row for every member.
	if err := s.sendRuntimeRaidCreateBinding(pending.Applicant.session, uint16(dnfenum.CmdPacketCreateRaid), state); err != nil {
		return true, err
	}
	// Membership just changed, so refresh every member material column too.
	// The 入场材料 column is rendered from the 0x0256 snapshot and no roster push
	// refreshes it, which is why the leader -- the first to open the window, whose
	// snapshot was captured before anyone joined -- kept seeing every later member
	// column empty while the members who joined later saw a nearly complete one.
	// Kept out of broadcastRuntimeRaidRefresh on purpose: that function also runs in
	// the start flow, where the ack-before-cost-sync ordering is load bearing.
	if err := s.broadcastRuntimeRaidEntryCost(state); err != nil {
		return true, err
	}
	if err := s.broadcastRuntimeRaidBuffRegistration(state, 0); err != nil {
		return true, err
	}
	// The applicant joins while standing wherever it was -- often a different area
	// of the raid channel, or still inside its own Seria room. The roster row that
	// just appeared renders its portrait and level from the client's own user
	// table, which only actor creation fills, so both sides need each other's
	// record now rather than whenever they next happen to share an area.
	//
	// Two operator decisions (2026-09-27) shape this call:
	//
	//   - Record-only installs. The roster row resolves from the mode0 record alone;
	//     the mode1 half carries the peer's equipment and skill pages, which nothing
	//     on the accepted-member path reads. Before the change the acceptance paid
	//     679 B + 1373 B of bindings plus two 14-slot equipment projections for them.
	//   - Exactly one call. syncRuntimeRaidMateActorsWith installs BOTH directions of
	//     every pair, so the second call this path used to make for the approving
	//     leader repeated every pair it touched -- live 2026-09-26 17:18:06Z, raid
	//     1507329: identical sizes twice per direction (472/1373 to the applicant at
	//     .404 and .529, 459/679 to the leader at .434 and .463), 2983 B of that
	//     6674 B acceptance.
	s.syncRuntimeRaidMateActorRecords(pending.Applicant.session, "raid_admitted_member_actor")
	s.logGameEvent(session, "game-raid-peer-admitted",
		"raid_key", state.RaidKey,
		"applicant_id", pending.Applicant.character,
		"leader_id", state.LeaderID,
		"application", pending.IsApplication,
		"member_count", len(state.Members))
	return true, nil
}

// admitRuntimeRaidPeer re-validates the approving leader under the raid lock
// and then applies the shared op668 roster rule.
func (s *Service) admitRuntimeRaidPeer(raidKey uint32, leaderID uint16, applicantID uint16) (runtimeRaidState, bool, bool) {
	if raidKey == 0 || leaderID == 0 || applicantID == 0 {
		return runtimeRaidState{}, false, false
	}
	s.raidMu.Lock()
	defer s.raidMu.Unlock()
	state := s.raids[raidKey]
	if state == nil || state.LeaderID != leaderID || !runtimeRaidOpenForPeerAdmission(*state) {
		return runtimeRaidState{}, false, false
	}
	return s.rejoinRuntimeRaidAdmissionLocked(raidKey, applicantID)
}

func (s *Service) storeRuntimeRaidPeerAdmission(key runtimeRaidPeerAdmissionKey, pending runtimeRaidPeerAdmission, now time.Time) {
	s.raidMu.Lock()
	defer s.raidMu.Unlock()
	if s.raidPeerAdmissions == nil {
		s.raidPeerAdmissions = make(map[runtimeRaidPeerAdmissionKey]runtimeRaidPeerAdmission)
	}
	for existing, entry := range s.raidPeerAdmissions {
		// One outstanding prompt per requester, and never keep expired rows.
		if existing.Requester == key.Requester || !now.Before(entry.ExpiresAt) {
			delete(s.raidPeerAdmissions, existing)
		}
	}
	s.raidPeerAdmissions[key] = pending
}

func (s *Service) takeRuntimeRaidPeerAdmission(key runtimeRaidPeerAdmissionKey, now time.Time) (runtimeRaidPeerAdmission, bool) {
	s.raidMu.Lock()
	defer s.raidMu.Unlock()
	pending, found := s.raidPeerAdmissions[key]
	if !found {
		return runtimeRaidPeerAdmission{}, false
	}
	delete(s.raidPeerAdmissions, key)
	if !now.Before(pending.ExpiresAt) {
		return runtimeRaidPeerAdmission{}, false
	}
	return pending, true
}

func (s *Service) removeRuntimeRaidPeerAdmission(key runtimeRaidPeerAdmissionKey) {
	s.raidMu.Lock()
	delete(s.raidPeerAdmissions, key)
	s.raidMu.Unlock()
}
