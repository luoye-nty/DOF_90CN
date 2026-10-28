package dnfbridge

import (
	"testing"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	"longheng.io/server/internal/modules/dnf/channelcatalog"
)

func TestManagedPartyCrossJoinRefreshesPriorPartySurvivor(t *testing.T) {
	service := &Service{options: options{gameUpperBodyCodec: gameUpperBodyCodecPlain}}
	channel := channelcatalog.Channel{ID: 38, Type: 1, Name: "ch.38", Port: 10038}
	partyA := alignedcmd.PartyState{
		PartyID: 77, UserID: 1001, IsLeader: true, MaxMembers: 4,
		Members: []alignedcmd.PartyMemberState{
			{UserID: 1001, UserState: 1, HPPercent: 100, MPPercent: 100},
			{UserID: 1002, UserState: 1, HPPercent: 90, MPPercent: 80},
		},
	}
	partyB := alignedcmd.PartyState{
		PartyID: 88, UserID: 1003, IsLeader: true, MaxMembers: 4,
		Members: []alignedcmd.PartyMemberState{{UserID: 1003, UserState: 1, HPPercent: 100, MPPercent: 100}},
	}
	leaderA := &gameSession{conn: &bufferConn{}, channel: channel, selectedCharacterID: 1001, party: partySessionState{state: partyA}}
	joiner := &gameSession{conn: &bufferConn{}, channel: channel, selectedCharacterID: 1002, party: partySessionState{state: partyA}}
	leaderB := &gameSession{conn: &bufferConn{}, channel: channel, selectedCharacterID: 1003, party: partySessionState{state: partyB}}
	service.bindGameSessionCharacter(leaderA, 1001)
	service.bindGameSessionCharacter(joiner, 1002)
	service.bindGameSessionCharacter(leaderB, 1003)

	state, result, joined := service.createOrJoinManagedRuntimeParty(joiner, leaderB)
	if !joined || !result.OK || result.PriorLeave == nil || !result.PriorLeave.OK {
		t.Fatalf("cross-party join=%t result=%+v", joined, result)
	}
	if state.UserID != 1003 || len(runtimePartyMembers(state)) != 2 {
		t.Fatalf("target party projection=%+v", state)
	}
	// The joiner was the only other member of the prior party, so leaving it
	// dissolved that party (single-member disband rule) and the member left
	// behind is partyless with its client cache cleared.
	if old := runtimePartyStateSnapshot(leaderA); old.PartyID != 0 || len(runtimePartyMembers(old)) != 0 {
		t.Fatalf("prior party survivor aggregate = %+v", old)
	}
	if next := runtimePartyStateSnapshot(joiner); next.UserID != 1003 || len(runtimePartyMembers(next)) != 2 {
		t.Fatalf("joiner projection=%+v", next)
	}
	if snapshot, found := service.runtimePartyManagerForService().SnapshotByUser(1001, leaderA.characterGeneration); found {
		t.Fatalf("prior party survivor stayed in the manager=%+v", snapshot)
	}
}

func TestManagedPartyInviteTargetKeepsLeadershipOverAcceptorTransientSetPartyInfo(t *testing.T) {
	service := &Service{options: options{gameUpperBodyCodec: gameUpperBodyCodecPlain}}
	channel := channelcatalog.Channel{ID: 38, Type: 1, Name: "ch.38", Port: 10038}
	inviter := &gameSession{conn: &bufferConn{}, channel: channel, selectedCharacterID: 1001}
	acceptor := &gameSession{
		conn:                &bufferConn{},
		channel:             channel,
		selectedCharacterID: 1002,
		party: partySessionState{state: alignedcmd.PartyState{
			PartyID:    1002,
			UserID:     1002,
			IsLeader:   true,
			MaxMembers: 4,
			Members: []alignedcmd.PartyMemberState{{
				UserID: 1002, UserState: 1, HPPercent: 100, MPPercent: 100,
			}},
		}},
	}
	service.bindGameSessionCharacter(inviter, 1001)
	service.bindGameSessionCharacter(acceptor, 1002)

	// op12 can establish this local one-member cache before the accept button.
	// It is deliberately imported here to model that order; accepting the
	// inviter must still rebuild under character 1001.
	if _, err := service.ensureManagedRuntimePartyForSession(acceptor); err != nil {
		t.Fatalf("bootstrap acceptor transient party: %v", err)
	}
	state, result, joined := service.createOrJoinManagedRuntimeParty(acceptor, inviter)
	if !joined || !result.OK {
		t.Fatalf("accept invite joined=%t result=%+v", joined, result)
	}
	if state.UserID != 1001 || !state.IsLeader || len(runtimePartyMembers(state)) != 2 {
		t.Fatalf("invite target did not retain leadership: %+v", state)
	}
	if members := runtimePartyMembers(state); members[0].UserID != 1001 || members[1].UserID != 1002 {
		t.Fatalf("party order=%+v, want inviter then acceptor", members)
	}
	inviterIdentity, ok := service.boundGameSessionCharacterSnapshot(inviter)
	if !ok {
		t.Fatal("inviter identity was not bound")
	}
	snapshot, found := service.runtimePartyManagerForService().SnapshotByUser(inviterIdentity.character, inviterIdentity.generation)
	if !found || snapshot.Leader != 1001 || len(snapshot.Members) != 2 {
		t.Fatalf("authoritative party snapshot=%+v found=%t", snapshot, found)
	}
}

// TestManagedPartyReplacementSessionLeavesTheParty pins the operator rule
// (2026-09-27): 「被顶掉以后 客户端的连接就断开了 此时也按退队处理」,「还有更换频道 也算
// 退队」. A second socket for a character the server still holds -- another client
// taking it over, or the same client on a new channel -- used to inherit the party
// (RuntimePartyManager.RebindSession). It now performs a LEAVE under the OLD
// session's generation, so the survivors are told their roster shrank and the
// replacement comes up partyless.
//
// The leave has to happen inside bindGameSessionCharacter: the retired socket's
// own teardown cannot do it, because by then s.gameSessions[char] is the
// replacement, so boundGameSessionCharacterCurrent answers false for the old
// identity and detachRuntimePartySession returns without touching the roster.
// Dropping the hand-over without performing the leave would leave a member nobody
// can remove -- the 2026-09-20 ghost-party class, which this test now guards.
func TestManagedPartyReplacementSessionLeavesTheParty(t *testing.T) {
	service := &Service{options: options{gameUpperBodyCodec: gameUpperBodyCodecPlain}}
	channel := channelcatalog.Channel{ID: 38, Type: 1, Name: "ch.38", Port: 10038}
	leader := &gameSession{conn: &bufferConn{}, channel: channel, selectedCharacterID: 1001}
	member := &gameSession{conn: &bufferConn{}, channel: channel, selectedCharacterID: 1002}
	other := &gameSession{conn: &bufferConn{}, channel: channel, selectedCharacterID: 1003}
	service.bindGameSessionCharacter(leader, 1001)
	service.bindGameSessionCharacter(member, 1002)
	service.bindGameSessionCharacter(other, 1003)
	for _, joiner := range []*gameSession{member, other} {
		if _, result, joined := service.createOrJoinManagedRuntimeParty(joiner, leader); !joined || !result.OK {
			t.Fatalf("initial join=%t result=%+v", joined, result)
		}
	}
	state := runtimePartyStateSnapshot(leader)
	if len(runtimePartyMembers(state)) != 3 {
		t.Fatalf("party has %d members before the takeover, want 3", len(runtimePartyMembers(state)))
	}
	oldGeneration := member.characterGeneration
	leader.conn.(*bufferConn).write.Reset()

	replacement := &gameSession{conn: &bufferConn{}, channel: channel, selectedCharacterID: 1002}
	service.bindGameSessionCharacter(replacement, 1002)

	if replacement.characterGeneration == oldGeneration {
		t.Fatalf("replacement generation=%d old=%d", replacement.characterGeneration, oldGeneration)
	}
	if _, stale := service.runtimePartyManagerForService().SnapshotByUser(1002, oldGeneration); stale {
		t.Fatal("retired session generation remained in central party")
	}
	if _, current := service.runtimePartyManagerForService().SnapshotByUser(1002, replacement.characterGeneration); current {
		t.Fatal("the replacement session inherited the party instead of leaving it")
	}
	if projected := runtimePartyStateSnapshot(replacement); projected.PartyID != 0 {
		t.Fatalf("replacement projection=%+v, want no party", projected)
	}
	if leader.conn.(*bufferConn).write.Len() == 0 {
		t.Fatal("the survivors were never told the replacement left the party")
	}
	// Two survivors remain, so the party survives without the departed member.
	survivorState := runtimePartyStateSnapshot(leader)
	members := runtimePartyMembers(survivorState)
	if survivorState.PartyID != state.PartyID || len(members) != 2 {
		t.Fatalf("survivor party=%+v, want party %d with 2 members", survivorState, state.PartyID)
	}
	if containsRuntimePartyMember(members, 1002) {
		t.Fatal("the departed character is still on the survivors' roster")
	}
}
