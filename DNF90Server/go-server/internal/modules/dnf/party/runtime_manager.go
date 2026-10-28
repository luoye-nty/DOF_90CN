package party

import (
	"sort"
	"sync"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
)

// RuntimePartyManager owns the process-local lifecycle of online parties.
// It deliberately contains no connection or packet code: dnfbridge validates
// the live session generation at its boundary, then supplies that immutable
// identity here. This keeps one authoritative membership graph instead of a
// divergent party copy on every game session.
type RuntimePartyManager struct {
	mu sync.Mutex

	nextID       uint16
	parties      map[uint16]*runtimeParty
	byUser       map[uint16]uint16
	peerRequests map[uint16]runtimePartyPeerRequest
}

type RuntimePartyMember struct {
	UserID            uint16
	SessionGeneration uint64
	State             alignedcmd.PartyMemberState
	Slot              byte
}

type RuntimePartySnapshot struct {
	ID       uint16
	Leader   uint16
	Settings alignedcmd.PartyState
	Members  []RuntimePartyMember
}

type RuntimePartyResult struct {
	OK          bool
	Reason      string
	Party       RuntimePartySnapshot
	Previous    *RuntimePartySnapshot
	PriorLeave  *RuntimePartyResult
	TargetUser  uint16
	Disbanded   bool
	LeaderMoved bool
}

type runtimeParty struct {
	id       uint16
	leader   uint16
	settings alignedcmd.PartyState
	members  [currentRuntimePartyMaxMembers]*RuntimePartyMember
}

// RuntimePartyPeerRequestContext is what the bridge learns from the pending op10
// peer request it just consumed. The direction is part of it because the two
// ordinary party modes travel in the same packet shape both ways: the packet
// alone cannot say whether the sender invited the target or applied to join the
// target's party (see runtimePartyPeerRequest.application).
type RuntimePartyPeerRequestContext struct {
	// PartyID is the requester's own party at request time, zero when the
	// requester had none.
	PartyID uint16
	// JoinApplication is true when the requester asked to JOIN the decider's
	// party. The decider owns the party that survives, so an acceptance moves
	// the REQUESTER; when it is false the request was an invitation and the
	// acceptance moves the DECIDER (the answering client) instead.
	JoinApplication bool
}

// runtimePartyPeerRequest is one pending op10 peer request, keyed by the
// character that received the prompt and owes the op11 answer.
type runtimePartyPeerRequest struct {
	decider             uint16
	deciderGeneration   uint64
	requester           uint16
	requesterGeneration uint64
	partyID             uint16
	mode                byte
	// application records that the requester asked to join the decider's party.
	// The current client sends op10 mode 0 for both menu entries -- 邀请组队 for
	// a partyless target and 加入 for a target who already has a party -- so the
	// bridge decides the direction from the target's party state at request time
	// and stores it here. Re-deriving it when the answer arrives would instead
	// read whatever the party graph looks like a second later.
	application bool
}

const currentRuntimePartyMaxMembers = 4

// currentRuntimePartyMinMembers is the smallest membership this server keeps as
// a party. Operator rule (2026-09-22): 「一个人不成队」-- whenever a leave or a
// kick would leave exactly one member behind, the party dissolves and that last
// member ends up partyless as well.
//
// The retained one-member aggregate used to be deliberate (see
// dnfbridge.publishRuntimePartyMembershipSnapshot: the native kind-2 transition
// owns the multiplayer-to-solo confirmation). Its visible cost was that the
// survivor sat in a party window the client never draws and could not leave --
// the open item in the 2026-09-20 party-peer-selection ghost-party fix, section
// 6: the bridge therefore publishes the native kind-2 one-member row first and
// clears the party object right after (see broadcastRuntimePartyMemberRemoved),
// so the client still gets that transition before the window disappears.
const currentRuntimePartyMinMembers = 2

func NewRuntimePartyManager() *RuntimePartyManager {
	return &RuntimePartyManager{
		// Current NoPack reserves party model key 1 during its own bootstrap.
		nextID:       2,
		parties:      make(map[uint16]*runtimeParty),
		byUser:       make(map[uint16]uint16),
		peerRequests: make(map[uint16]runtimePartyPeerRequest),
	}
}

func (m *RuntimePartyManager) SnapshotByUser(userID uint16, generation uint64) (RuntimePartySnapshot, bool) {
	if m == nil || userID == 0 {
		return RuntimePartySnapshot{}, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	party, member := m.partyMemberLocked(userID)
	if party == nil || member == nil || (generation != 0 && member.SessionGeneration != generation) {
		return RuntimePartySnapshot{}, false
	}
	return snapshotRuntimeParty(party), true
}

func (m *RuntimePartyManager) SnapshotByID(partyID uint16) (RuntimePartySnapshot, bool) {
	if m == nil || partyID == 0 {
		return RuntimePartySnapshot{}, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	party := m.parties[partyID]
	if party == nil {
		return RuntimePartySnapshot{}, false
	}
	return snapshotRuntimeParty(party), true
}

func (m *RuntimePartyManager) Snapshots() []RuntimePartySnapshot {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	ids := make([]int, 0, len(m.parties))
	for partyID := range m.parties {
		ids = append(ids, int(partyID))
	}
	sort.Ints(ids)
	result := make([]RuntimePartySnapshot, 0, len(ids))
	for _, partyID := range ids {
		result = append(result, snapshotRuntimeParty(m.parties[uint16(partyID)]))
	}
	return result
}

// Create starts a fresh party for leader. If that character already belongs
// to another party, it is detached atomically and the old-party result is
// returned through PriorLeave so the bridge can notify survivors.
func (m *RuntimePartyManager) Create(leader RuntimePartyMember, settings alignedcmd.PartyState) RuntimePartyResult {
	if m == nil || !validRuntimePartyMember(leader) {
		return runtimePartyFailure("invalid_leader")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	partyID := m.nextPartyIDLocked()
	if partyID == 0 {
		return runtimePartyFailure("party_id_exhausted")
	}
	prior := m.leaveLocked(leader.UserID, leader.SessionGeneration, true)
	party := &runtimeParty{id: partyID, leader: leader.UserID, settings: normalizeRuntimePartySettings(settings)}
	party.settings.PartyID = int(party.id)
	party.settings.UserID = leader.UserID
	party.settings.IsLeader = true
	if !party.add(leader) {
		return runtimePartyFailure("add_leader_failed")
	}
	m.parties[party.id] = party
	m.byUser[leader.UserID] = party.id
	result := runtimePartySuccess(party, leader.UserID)
	if prior.OK {
		result.PriorLeave = &prior
	}
	return result
}

// EnsureLeader creates a lobby only when this exact live leader has no party.
// It is used by bridge-side migration/first-contact paths, where two packets
// may observe the same partyless target concurrently. Unlike Create, it never
// detaches an existing party merely because a duplicate bootstrap arrived.
func (m *RuntimePartyManager) EnsureLeader(leader RuntimePartyMember, settings alignedcmd.PartyState) RuntimePartyResult {
	if m == nil || !validRuntimePartyMember(leader) {
		return runtimePartyFailure("invalid_leader")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if existing, member := m.partyMemberLocked(leader.UserID); existing != nil && member != nil {
		if member.SessionGeneration != leader.SessionGeneration {
			return runtimePartyFailure("stale_member_session")
		}
		if existing.leader != leader.UserID {
			return runtimePartyFailure("already_party_member")
		}
		return runtimePartySuccess(existing, leader.UserID)
	}
	party := &runtimeParty{id: m.nextPartyIDLocked(), leader: leader.UserID, settings: normalizeRuntimePartySettings(settings)}
	if party.id == 0 {
		return runtimePartyFailure("party_id_exhausted")
	}
	party.settings.PartyID = int(party.id)
	party.settings.UserID = leader.UserID
	party.settings.IsLeader = true
	if !party.add(leader) {
		return runtimePartyFailure("add_leader_failed")
	}
	m.parties[party.id] = party
	m.byUser[leader.UserID] = party.id
	return runtimePartySuccess(party, leader.UserID)
}

// Join joins member to the current party generation. A previous membership is
// always resolved within the same lock, so no character can exist in two
// parties even while join, leave and disconnect packets race.
func (m *RuntimePartyManager) Join(partyID uint16, member RuntimePartyMember) RuntimePartyResult {
	if m == nil || partyID == 0 || !validRuntimePartyMember(member) {
		return runtimePartyFailure("invalid_join")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	party := m.parties[partyID]
	if party == nil {
		return runtimePartyFailure("party_not_found")
	}
	if existing := party.member(member.UserID); existing != nil {
		if existing.SessionGeneration == member.SessionGeneration {
			return runtimePartyFailure("already_member")
		}
		return runtimePartyFailure("stale_member_session")
	}
	if party.full() {
		return runtimePartyFailure("party_full")
	}
	prior := m.leaveLocked(member.UserID, member.SessionGeneration, true)
	if !party.add(member) {
		return runtimePartyFailure("add_member_failed")
	}
	m.byUser[member.UserID] = party.id
	result := runtimePartySuccess(party, member.UserID)
	if prior.OK {
		result.PriorLeave = &prior
	}
	return result
}

func (m *RuntimePartyManager) Leave(userID uint16, generation uint64) RuntimePartyResult {
	if m == nil || userID == 0 || generation == 0 {
		return runtimePartyFailure("invalid_leave")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.leaveLocked(userID, generation, false)
}

func (m *RuntimePartyManager) leaveLocked(userID uint16, generation uint64, allowAbsent bool) RuntimePartyResult {
	party, member := m.partyMemberLocked(userID)
	if party == nil || member == nil {
		if allowAbsent {
			return RuntimePartyResult{}
		}
		return runtimePartyFailure("not_in_party")
	}
	if generation != 0 && member.SessionGeneration != generation {
		return runtimePartyFailure("stale_session")
	}
	previous := snapshotRuntimeParty(party)
	previousCopy := previous
	wasLeader := party.leader == userID
	party.remove(userID)
	delete(m.byUser, userID)
	m.clearPeerRequestsForLocked(userID, generation)
	if party.count() == 0 {
		delete(m.parties, party.id)
		return RuntimePartyResult{OK: true, Previous: &previousCopy, TargetUser: userID, Disbanded: true}
	}
	if result, disbanded := m.disbandSingleMemberPartyLocked(party, previousCopy, userID); disbanded {
		return result
	}
	if !wasLeader {
		result := runtimePartySuccess(party, userID)
		result.Previous = &previousCopy
		return result
	}

	// Same-version NoPack retains the native party model and fixed slots when
	// the leader leaves. Authority passes to the lowest occupied slot and the
	// complete kind-2 membership row carries that new leader slot.
	members := party.membersBySlot()
	newLeader := members[0]
	party.leader = newLeader.UserID
	party.settings.UserID = newLeader.UserID
	party.settings.IsLeader = true
	result := runtimePartySuccess(party, userID)
	result.Previous = &previousCopy
	result.LeaderMoved = true
	return result
}

func (m *RuntimePartyManager) Kick(byUser uint16, byGeneration uint64, targetUser uint16, targetGeneration uint64) RuntimePartyResult {
	if m == nil || byUser == 0 || targetUser == 0 || byUser == targetUser || byGeneration == 0 || targetGeneration == 0 {
		return runtimePartyFailure("invalid_kick")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	party, leader := m.partyMemberLocked(byUser)
	if party == nil || leader == nil {
		return runtimePartyFailure("not_in_party")
	}
	if leader.SessionGeneration != byGeneration {
		return runtimePartyFailure("stale_session")
	}
	if party.leader != byUser {
		return runtimePartyFailure("not_leader")
	}
	target := party.member(targetUser)
	if target == nil {
		return runtimePartyFailure("target_not_member")
	}
	if target.SessionGeneration != targetGeneration {
		return runtimePartyFailure("stale_target_session")
	}
	previous := snapshotRuntimeParty(party)
	party.remove(targetUser)
	delete(m.byUser, targetUser)
	m.clearPeerRequestsForLocked(targetUser, targetGeneration)
	if result, disbanded := m.disbandSingleMemberPartyLocked(party, previous, targetUser); disbanded {
		return result
	}
	result := runtimePartySuccess(party, targetUser)
	result.Previous = &previous
	return result
}

// disbandSingleMemberPartyLocked drops a party that a leave or a kick just
// reduced below the minimum membership, and detaches the last member with it.
// Both sides end up partyless, and Previous still carries the full pre-mutation
// roster so the bridge can publish the reduction to whoever was left behind.
// The caller must already hold m.mu and must have removed targetUser from
// m.byUser.
func (m *RuntimePartyManager) disbandSingleMemberPartyLocked(
	party *runtimeParty,
	previous RuntimePartySnapshot,
	targetUser uint16,
) (RuntimePartyResult, bool) {
	if m == nil || party == nil || party.count() >= currentRuntimePartyMinMembers {
		return RuntimePartyResult{}, false
	}
	for _, member := range party.membersBySlot() {
		party.remove(member.UserID)
		delete(m.byUser, member.UserID)
	}
	delete(m.parties, party.id)
	return RuntimePartyResult{
		OK:         true,
		Previous:   &previous,
		TargetUser: targetUser,
		Disbanded:  true,
	}, true
}

// TransferLeader changes only the authoritative leader of the existing party.
// Current NoPack sends the target's stable slot in op121 and expects the same
// party model and slot table back with a different trailing leader-slot byte.
// Rebuilding the party here would restart P2P and destroy the fixed-slot
// identity used by the HUD and minimap.
func (m *RuntimePartyManager) TransferLeader(byUser uint16, byGeneration uint64, newLeader uint16, newLeaderGeneration uint64) RuntimePartyResult {
	if m == nil || byUser == 0 || newLeader == 0 || byGeneration == 0 || newLeaderGeneration == 0 {
		return runtimePartyFailure("invalid_leader_transfer")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	party, leader := m.partyMemberLocked(byUser)
	if party == nil || leader == nil || leader.SessionGeneration != byGeneration {
		return runtimePartyFailure("stale_session")
	}
	if party.leader != byUser {
		return runtimePartyFailure("not_leader")
	}
	next := party.member(newLeader)
	if next == nil || next.SessionGeneration != newLeaderGeneration {
		return runtimePartyFailure("stale_target_session")
	}
	previous := snapshotRuntimeParty(party)
	party.leader = newLeader
	party.settings.UserID = newLeader
	party.settings.IsLeader = true
	result := runtimePartySuccess(party, newLeader)
	result.Previous = &previous
	result.LeaderMoved = true
	return result
}

// RecordInvite registers an ordinary party INVITATION: the requester asked the
// decider (the op10 target, which is the client that shows the prompt and
// answers with op11) to join the requester's own party.
func (m *RuntimePartyManager) RecordInvite(invitee uint16, inviteeGeneration uint64, inviter uint16, inviterGeneration uint64, partyID uint16, mode byte) bool {
	return m.recordPeerRequest(invitee, inviteeGeneration, inviter, inviterGeneration, partyID, mode, false)
}

// RecordJoinApplication registers an ordinary party JOIN APPLICATION: the
// requester asked to join a party the decider already owns, so the decider is
// the one who decides and the requester is the one that moves on acceptance.
//
// Named the other way round from RecordInvite on purpose -- owner/applicant
// instead of invitee/inviter -- because the two differ only in which side ends
// up changing party, and a call site that quietly swapped them would reproduce
// the 2026-09-26 defect where a leader was pulled out of his own party.
func (m *RuntimePartyManager) RecordJoinApplication(owner uint16, ownerGeneration uint64, applicant uint16, applicantGeneration uint64, partyID uint16, mode byte) bool {
	return m.recordPeerRequest(owner, ownerGeneration, applicant, applicantGeneration, partyID, mode, true)
}

func (m *RuntimePartyManager) recordPeerRequest(decider uint16, deciderGeneration uint64, requester uint16, requesterGeneration uint64, partyID uint16, mode byte, application bool) bool {
	if m == nil || decider == 0 || requester == 0 || decider == requester || deciderGeneration == 0 || requesterGeneration == 0 {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.peerRequests[decider] = runtimePartyPeerRequest{
		decider:             decider,
		deciderGeneration:   deciderGeneration,
		requester:           requester,
		requesterGeneration: requesterGeneration,
		partyID:             partyID,
		mode:                mode,
		application:         application,
	}
	return true
}

// ConsumePeerRequest validates and consumes the pending request that the given
// op11 answer belongs to. Both generations are checked, so an answer from a
// retired session -- or one aimed at a requester that has since reconnected --
// never moves anybody.
func (m *RuntimePartyManager) ConsumePeerRequest(decider uint16, deciderGeneration uint64, requester uint16, requesterGeneration uint64, mode byte) (RuntimePartyPeerRequestContext, bool) {
	if m == nil {
		return RuntimePartyPeerRequestContext{}, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	request, ok := m.peerRequests[decider]
	if !ok || request.deciderGeneration != deciderGeneration || request.requester != requester || request.requesterGeneration != requesterGeneration || request.mode != mode {
		return RuntimePartyPeerRequestContext{}, false
	}
	delete(m.peerRequests, decider)
	return RuntimePartyPeerRequestContext{PartyID: request.partyID, JoinApplication: request.application}, true
}

func (m *RuntimePartyManager) UpdateSettings(userID uint16, generation uint64, settings alignedcmd.PartyState) RuntimePartyResult {
	if m == nil || userID == 0 || generation == 0 {
		return runtimePartyFailure("invalid_settings")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	party, member := m.partyMemberLocked(userID)
	if party == nil || member == nil {
		return runtimePartyFailure("not_in_party")
	}
	if member.SessionGeneration != generation {
		return runtimePartyFailure("stale_session")
	}
	if party.leader != userID {
		return runtimePartyFailure("not_leader")
	}
	settings = normalizeRuntimePartySettings(settings)
	settings.PartyID = int(party.id)
	settings.UserID = party.leader
	settings.IsLeader = true
	party.settings = settings
	return runtimePartySuccess(party, userID)
}

func (m *RuntimePartyManager) OnDisconnected(userID uint16, generation uint64) RuntimePartyResult {
	return m.Leave(userID, generation)
}

// RebindSession moves an existing member to a replacement game-session
// generation without changing its party, slot or leader. Channel reconnects
// temporarily have two sockets for one character; accepting packets from the
// retired generation after this point would let it remove the replacement
// member, so this is deliberately generation-conditional.
func (m *RuntimePartyManager) RebindSession(userID uint16, priorGeneration uint64, replacement RuntimePartyMember) RuntimePartyResult {
	if m == nil || userID == 0 || priorGeneration == 0 || !validRuntimePartyMember(replacement) || replacement.UserID != userID {
		return runtimePartyFailure("invalid_session_rebind")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	party, member := m.partyMemberLocked(userID)
	if party == nil || member == nil {
		m.clearPeerRequestsForLocked(userID, priorGeneration)
		return runtimePartyFailure("not_in_party")
	}
	if member.SessionGeneration != priorGeneration {
		return runtimePartyFailure("stale_session")
	}
	replacement.Slot = member.Slot
	replacement.State = normalizeRuntimePartyMember(replacement.State, userID)
	*member = replacement
	m.clearPeerRequestsForLocked(userID, priorGeneration)
	return runtimePartySuccess(party, userID)
}

// Reposition moves any member between adjacent persistent party slots. The
// leader authority stays attached to the member identity and the authoritative
// leader slot is projected separately in op9.
func (m *RuntimePartyManager) Reposition(byUser uint16, byGeneration uint64, fromSlot uint8, toSlot uint8) RuntimePartyResult {
	if m == nil || byUser == 0 || byGeneration == 0 || fromSlot >= currentRuntimePartyMaxMembers || toSlot >= currentRuntimePartyMaxMembers || fromSlot == toSlot {
		return runtimePartyFailure("invalid_reposition")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	party, leader := m.partyMemberLocked(byUser)
	if party == nil || leader == nil || leader.SessionGeneration != byGeneration {
		return runtimePartyFailure("stale_session")
	}
	if party.leader != byUser {
		return runtimePartyFailure("not_leader")
	}
	if party.members[fromSlot] == nil {
		return runtimePartyFailure("invalid_source_slot")
	}
	party.members[fromSlot], party.members[toSlot] = party.members[toSlot], party.members[fromSlot]
	if party.members[fromSlot] != nil {
		party.members[fromSlot].Slot = fromSlot
	}
	if party.members[toSlot] != nil {
		party.members[toSlot].Slot = toSlot
	}
	return runtimePartySuccess(party, byUser)
}

func (m *RuntimePartyManager) nextPartyIDLocked() uint16 {
	for attempts := 0; attempts < int(^uint16(0))-1; attempts++ {
		id := m.nextID
		m.nextID++
		if m.nextID == 0 || m.nextID == ^uint16(0) {
			m.nextID = 2
		}
		if id != 0 && id != ^uint16(0) && m.parties[id] == nil {
			return id
		}
	}
	return 0
}

func (m *RuntimePartyManager) partyMemberLocked(userID uint16) (*runtimeParty, *RuntimePartyMember) {
	partyID := m.byUser[userID]
	party := m.parties[partyID]
	if party == nil {
		return nil, nil
	}
	return party, party.member(userID)
}

func (m *RuntimePartyManager) clearPeerRequestsForLocked(userID uint16, generation uint64) {
	for decider, request := range m.peerRequests {
		if (decider == userID && (generation == 0 || request.deciderGeneration == generation)) ||
			(request.requester == userID && (generation == 0 || request.requesterGeneration == generation)) {
			delete(m.peerRequests, decider)
		}
	}
}

func (p *runtimeParty) count() int {
	count := 0
	for _, member := range p.members {
		if member != nil {
			count++
		}
	}
	return count
}

func (p *runtimeParty) full() bool {
	limit := p.settings.MaxMembers
	if limit == 0 || limit > currentRuntimePartyMaxMembers {
		limit = currentRuntimePartyMaxMembers
	}
	return p.count() >= int(limit)
}

func (p *runtimeParty) member(userID uint16) *RuntimePartyMember {
	for _, member := range p.members {
		if member != nil && member.UserID == userID {
			return member
		}
	}
	return nil
}

func (p *runtimeParty) add(member RuntimePartyMember) bool {
	if p == nil || !validRuntimePartyMember(member) || p.full() || p.member(member.UserID) != nil {
		return false
	}
	for slot := range p.members {
		if p.members[slot] != nil {
			continue
		}
		member.Slot = byte(slot)
		member.State = normalizeRuntimePartyMember(member.State, member.UserID)
		p.members[slot] = &member
		return true
	}
	return false
}

func (p *runtimeParty) remove(userID uint16) bool {
	for slot, member := range p.members {
		if member != nil && member.UserID == userID {
			p.members[slot] = nil
			return true
		}
	}
	return false
}

func (p *runtimeParty) membersBySlot() []RuntimePartyMember {
	result := make([]RuntimePartyMember, 0, currentRuntimePartyMaxMembers)
	for _, member := range p.members {
		if member != nil {
			result = append(result, cloneRuntimePartyMember(*member))
		}
	}
	return result
}

func snapshotRuntimeParty(p *runtimeParty) RuntimePartySnapshot {
	if p == nil {
		return RuntimePartySnapshot{}
	}
	return RuntimePartySnapshot{ID: p.id, Leader: p.leader, Settings: cloneRuntimePartySettings(p.settings), Members: p.membersBySlot()}
}

func (s RuntimePartySnapshot) StateFor(userID uint16) alignedcmd.PartyState {
	if s.ID == 0 || !s.contains(userID) {
		return alignedcmd.PartyState{}
	}
	state := cloneRuntimePartySettings(s.Settings)
	state.PartyID = int(s.ID)
	state.UserID = s.Leader
	state.IsLeader = s.Leader == userID
	state.Members = make([]alignedcmd.PartyMemberState, 0, len(s.Members))
	for _, member := range s.Members {
		projected := normalizeRuntimePartyMember(member.State, member.UserID)
		projected.Slot = member.Slot
		state.Members = append(state.Members, projected)
	}
	return state
}

func (s RuntimePartySnapshot) contains(userID uint16) bool {
	for _, member := range s.Members {
		if member.UserID == userID {
			return true
		}
	}
	return false
}

func runtimePartySuccess(p *runtimeParty, target uint16) RuntimePartyResult {
	return RuntimePartyResult{OK: true, Party: snapshotRuntimeParty(p), TargetUser: target}
}

func runtimePartyFailure(reason string) RuntimePartyResult { return RuntimePartyResult{Reason: reason} }

func validRuntimePartyMember(member RuntimePartyMember) bool {
	return member.UserID != 0 && member.UserID != ^uint16(0) && member.SessionGeneration != 0
}

func normalizeRuntimePartySettings(state alignedcmd.PartyState) alignedcmd.PartyState {
	state = cloneRuntimePartySettings(state)
	if state.MaxMembers == 0 || state.MaxMembers > currentRuntimePartyMaxMembers {
		state.MaxMembers = currentRuntimePartyMaxMembers
	}
	state.Members = nil
	return state
}

func cloneRuntimePartySettings(state alignedcmd.PartyState) alignedcmd.PartyState {
	state.NameBytes = append([]byte(nil), state.NameBytes...)
	state.Members = append([]alignedcmd.PartyMemberState(nil), state.Members...)
	return state
}

func normalizeRuntimePartyMember(member alignedcmd.PartyMemberState, userID uint16) alignedcmd.PartyMemberState {
	member.UserID = userID
	if member.UserState == 0 || member.UserState == 1 {
		member.UserState = 100
	}
	if member.HPPercent == 0 {
		member.HPPercent = 100
	}
	if member.MPPercent == 0 {
		member.MPPercent = 100
	}
	return member
}

func cloneRuntimePartyMember(member RuntimePartyMember) RuntimePartyMember {
	member.State = normalizeRuntimePartyMember(member.State, member.UserID)
	return member
}
