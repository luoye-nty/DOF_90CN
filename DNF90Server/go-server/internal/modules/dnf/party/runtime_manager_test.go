package party

import (
	"testing"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
)

// A leave from a party that keeps at least two members still moves leadership
// to the lowest occupied slot and preserves every stable slot. The two-member
// case is covered by TestRuntimePartyManagerDisbandsWhenLeaveLeavesOneMember:
// operator rule 2026-09-22 「一个人不成队」.
func TestRuntimePartyManagerLeaderLeaveMovesAuthorityAndKeepsStableSlots(t *testing.T) {
	manager := NewRuntimePartyManager()
	leader := testRuntimePartyMember(101, 1)
	created := manager.Create(leader, alignedcmd.PartyState{MaxMembers: 4})
	if !created.OK || created.Party.ID == 0 {
		t.Fatalf("create result = %+v", created)
	}
	for _, userID := range []uint16{202, 303} {
		if joined := manager.Join(created.Party.ID, testRuntimePartyMember(userID, 1)); !joined.OK {
			t.Fatalf("join %d result = %+v", userID, joined)
		}
	}
	left := manager.Leave(101, 1)
	if !left.OK || !left.LeaderMoved {
		t.Fatalf("leader leave result = %+v", left)
	}
	if left.Party.ID != created.Party.ID {
		t.Fatalf("party generation old=%d new=%d", created.Party.ID, left.Party.ID)
	}
	if left.Party.Leader != 202 || len(left.Party.Members) != 2 {
		t.Fatalf("surviving party = %+v", left.Party)
	}
	if got := runtimePartySlot(left.Party, 202); got != 1 {
		t.Fatalf("new leader slot = %d, want preserved slot 1", got)
	}
	if got := runtimePartySlot(left.Party, 303); got != 2 {
		t.Fatalf("member 303 slot = %d, want preserved slot 2", got)
	}
	if current, found := manager.SnapshotByID(created.Party.ID); !found || current.Leader != 202 {
		t.Fatalf("party by id = %+v found=%v", current, found)
	}
	if current, found := manager.SnapshotByUser(202, 1); !found || current.ID != left.Party.ID {
		t.Fatalf("survivor lookup = %+v found=%v", current, found)
	}
}

// Whatever drops a party to a single member dissolves it: the last member is
// detached too, both sides end up partyless, and Previous still carries the
// full pre-mutation roster so the bridge can tell that member the party is gone.
func TestRuntimePartyManagerDisbandsWhenLeaveLeavesOneMember(t *testing.T) {
	for _, test := range []struct {
		name        string
		leavingID   uint16
		survivorID  uint16
		wantLeader  uint16
		wantMoved   bool
		description string
	}{
		{
			name: "leader leaves a two-member party", leavingID: 101, survivorID: 202,
			wantLeader: 101, wantMoved: false,
			description: "leader leave used to keep a one-member aggregate with moved authority",
		},
		{
			name: "follower leaves a two-member party", leavingID: 202, survivorID: 101,
			wantLeader: 101, wantMoved: false,
			description: "the surviving leader is detached as well",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			manager := NewRuntimePartyManager()
			created := manager.Create(testRuntimePartyMember(101, 1), alignedcmd.PartyState{MaxMembers: 4})
			if joined := manager.Join(created.Party.ID, testRuntimePartyMember(202, 1)); !joined.OK {
				t.Fatalf("join result = %+v", joined)
			}
			left := manager.Leave(test.leavingID, 1)
			if !left.OK || !left.Disbanded {
				t.Fatalf("leave result = %+v (%s)", left, test.description)
			}
			if left.Party.ID != 0 || len(left.Party.Members) != 0 {
				t.Fatalf("dissolved result still carries a party = %+v", left.Party)
			}
			if left.Previous == nil || left.Previous.Leader != test.wantLeader ||
				len(left.Previous.Members) != 2 {
				t.Fatalf("previous roster = %+v, want both members", left.Previous)
			}
			if left.LeaderMoved != test.wantMoved {
				t.Fatalf("leader moved = %t, want %t", left.LeaderMoved, test.wantMoved)
			}
			if _, found := manager.SnapshotByID(created.Party.ID); found {
				t.Fatal("dissolved party is still addressable by id")
			}
			for _, userID := range []uint16{101, 202} {
				if snapshot, found := manager.SnapshotByUser(userID, 1); found {
					t.Fatalf("member %d still in party after dissolve: %+v", userID, snapshot)
				}
			}
		})
	}
}

// Kicking the only other member leaves the kicker alone, which is the same
// single-member state, so the party dissolves and the kicker is detached.
func TestRuntimePartyManagerKickDisbandsTheTwoMemberParty(t *testing.T) {
	manager := NewRuntimePartyManager()
	created := manager.Create(testRuntimePartyMember(101, 1), alignedcmd.PartyState{MaxMembers: 4})
	if joined := manager.Join(created.Party.ID, testRuntimePartyMember(202, 1)); !joined.OK {
		t.Fatalf("join result = %+v", joined)
	}
	kicked := manager.Kick(101, 1, 202, 1)
	if !kicked.OK || !kicked.Disbanded || kicked.TargetUser != 202 {
		t.Fatalf("kick result = %+v", kicked)
	}
	if kicked.Previous == nil || len(kicked.Previous.Members) != 2 {
		t.Fatalf("previous roster = %+v, want both members", kicked.Previous)
	}
	for _, userID := range []uint16{101, 202} {
		if snapshot, found := manager.SnapshotByUser(userID, 1); found {
			t.Fatalf("member %d still in party after kick dissolve: %+v", userID, snapshot)
		}
	}
}

func TestRuntimePartyManagerMaintainsStableSlotsUntilLeaderRebuild(t *testing.T) {
	manager := NewRuntimePartyManager()
	created := manager.Create(testRuntimePartyMember(101, 1), alignedcmd.PartyState{MaxMembers: 4})
	for _, userID := range []uint16{202, 303, 404} {
		if result := manager.Join(created.Party.ID, testRuntimePartyMember(userID, 1)); !result.OK {
			t.Fatalf("join %d = %+v", userID, result)
		}
	}
	if result := manager.Leave(202, 1); !result.OK || result.LeaderMoved {
		t.Fatalf("nonleader leave = %+v", result)
	}
	snapshot, found := manager.SnapshotByUser(303, 1)
	if !found {
		t.Fatal("remaining party not found")
	}
	if got := runtimePartySlot(snapshot, 303); got != 2 {
		t.Fatalf("member 303 slot = %d, want preserved slot 2", got)
	}
	if got := runtimePartySlot(snapshot, 404); got != 3 {
		t.Fatalf("member 404 slot = %d, want preserved slot 3", got)
	}
	if result := manager.Join(snapshot.ID, testRuntimePartyMember(505, 1)); !result.OK {
		t.Fatalf("join replacement = %+v", result)
	}
	snapshot, _ = manager.SnapshotByUser(505, 1)
	if got := runtimePartySlot(snapshot, 505); got != 1 {
		t.Fatalf("replacement slot = %d, want released slot 1", got)
	}
}

func TestRuntimePartyManagerBindsInvitesAndMutationsToSessionGeneration(t *testing.T) {
	manager := NewRuntimePartyManager()
	created := manager.Create(testRuntimePartyMember(101, 9), alignedcmd.PartyState{MaxMembers: 4})
	if !manager.RecordInvite(202, 7, 101, 9, created.Party.ID, 0) {
		t.Fatal("record invite failed")
	}
	if _, ok := manager.ConsumePeerRequest(202, 6, 101, 9, 0); ok {
		t.Fatal("retired invitee session consumed pending invite")
	}
	request, ok := manager.ConsumePeerRequest(202, 7, 101, 9, 0)
	if !ok || request.PartyID != created.Party.ID || request.JoinApplication {
		t.Fatalf("consume invite party=%d application=%v ok=%v", request.PartyID, request.JoinApplication, ok)
	}
	if result := manager.Join(created.Party.ID, testRuntimePartyMember(202, 7)); !result.OK {
		t.Fatalf("join result = %+v", result)
	}
	if result := manager.Kick(101, 8, 202, 7); result.Reason != "stale_session" {
		t.Fatalf("stale leader kick = %+v", result)
	}
	if result := manager.Kick(101, 9, 202, 6); result.Reason != "stale_target_session" {
		t.Fatalf("stale target kick = %+v", result)
	}
}

func TestRuntimePartyManagerMovesJoinerOutOfPriorPartyAndReportsPriorLeave(t *testing.T) {
	manager := NewRuntimePartyManager()
	first := manager.Create(testRuntimePartyMember(101, 1), alignedcmd.PartyState{MaxMembers: 4})
	for _, userID := range []uint16{202, 404} {
		if result := manager.Join(first.Party.ID, testRuntimePartyMember(userID, 1)); !result.OK {
			t.Fatalf("join first party %d = %+v", userID, result)
		}
	}
	second := manager.Create(testRuntimePartyMember(303, 1), alignedcmd.PartyState{MaxMembers: 4})
	result := manager.Join(second.Party.ID, testRuntimePartyMember(202, 1))
	if !result.OK || result.PriorLeave == nil || !result.PriorLeave.OK {
		t.Fatalf("cross-party join = %+v", result)
	}
	if result.PriorLeave.Disbanded {
		t.Fatalf("two survivors were dissolved: %+v", result.PriorLeave)
	}
	if old, found := manager.SnapshotByUser(101, 1); !found || len(old.Members) != 2 {
		t.Fatalf("old party = %+v found=%v", old, found)
	}
	if next, found := manager.SnapshotByUser(202, 1); !found || next.ID != second.Party.ID {
		t.Fatalf("new party = %+v found=%v", next, found)
	}
}

// A joiner who was the only other member of its old party dissolves that party
// on the way out, and the bridge is told through PriorLeave so it can clear the
// member that was left behind.
func TestRuntimePartyManagerReportsPriorLeaveDisbandOnCrossPartyJoin(t *testing.T) {
	manager := NewRuntimePartyManager()
	first := manager.Create(testRuntimePartyMember(101, 1), alignedcmd.PartyState{MaxMembers: 4})
	if result := manager.Join(first.Party.ID, testRuntimePartyMember(202, 1)); !result.OK {
		t.Fatalf("join first party = %+v", result)
	}
	second := manager.Create(testRuntimePartyMember(303, 1), alignedcmd.PartyState{MaxMembers: 4})
	result := manager.Join(second.Party.ID, testRuntimePartyMember(202, 1))
	if !result.OK || result.PriorLeave == nil || !result.PriorLeave.Disbanded {
		t.Fatalf("cross-party join = %+v", result)
	}
	if snapshot, found := manager.SnapshotByUser(101, 1); found {
		t.Fatalf("leftover member kept a party after the prior party dissolved: %+v", snapshot)
	}
}

func TestRuntimePartyManagerRejectsFifthMemberAndKeepsFourStableSlots(t *testing.T) {
	manager := NewRuntimePartyManager()
	created := manager.Create(testRuntimePartyMember(101, 1), alignedcmd.PartyState{MaxMembers: 4})
	for _, userID := range []uint16{202, 303, 404} {
		if joined := manager.Join(created.Party.ID, testRuntimePartyMember(userID, 1)); !joined.OK {
			t.Fatalf("join %d = %+v", userID, joined)
		}
	}
	if fifth := manager.Join(created.Party.ID, testRuntimePartyMember(505, 1)); fifth.Reason != "party_full" {
		t.Fatalf("fifth join = %+v", fifth)
	}
	snapshot, found := manager.SnapshotByID(created.Party.ID)
	if !found || len(snapshot.Members) != 4 {
		t.Fatalf("party after fifth join = %+v found=%v", snapshot, found)
	}
	for slot, userID := range []uint16{101, 202, 303, 404} {
		if got := runtimePartySlot(snapshot, userID); got != byte(slot) {
			t.Fatalf("member %d slot=%d want=%d", userID, got, slot)
		}
	}
}

// 现场 2026-09-26 13:23:17：C 点 A 的菜单只出「加入」，客户端发的却是与「邀请
// 组队」完全相同的 op10 mode 0。桥接层当时把它记成邀请，A 一同意就被拔出自己
// 的队（party 4 解散、队友 B 被丢下）。方向属于请求本身，必须在这里存下来。
func TestRuntimePartyManagerKeepsPeerRequestDirection(t *testing.T) {
	manager := NewRuntimePartyManager()
	owner := manager.Create(testRuntimePartyMember(101, 9), alignedcmd.PartyState{MaxMembers: 4})
	if result := manager.Join(owner.Party.ID, testRuntimePartyMember(102, 9)); !result.OK {
		t.Fatalf("owner party join = %+v", result)
	}

	// C(103) 申请加入 A(101) 的队：申请人是 103，决定人是 101。
	if !manager.RecordJoinApplication(101, 9, 103, 9, 0, 0) {
		t.Fatal("record join application failed")
	}
	if _, ok := manager.ConsumePeerRequest(101, 8, 103, 9, 0); ok {
		t.Fatal("retired decider session consumed the application")
	}
	application, ok := manager.ConsumePeerRequest(101, 9, 103, 9, 0)
	if !ok || !application.JoinApplication {
		t.Fatalf("consume application = %+v ok=%v, want JoinApplication", application, ok)
	}
	if application.PartyID != 0 {
		t.Fatalf("applicant party = %d, want 0 (it had none)", application.PartyID)
	}
	if _, again := manager.ConsumePeerRequest(101, 9, 103, 9, 0); again {
		t.Fatal("the application was consumed twice")
	}

	// 旧方向原样保留：邀请人 101 邀请 202，同意之后移动的是 202。
	if !manager.RecordInvite(202, 9, 101, 9, owner.Party.ID, 0) {
		t.Fatal("record invite failed")
	}
	invite, ok := manager.ConsumePeerRequest(202, 9, 101, 9, 0)
	if !ok || invite.JoinApplication {
		t.Fatalf("consume invite = %+v ok=%v, want a plain invitation", invite, ok)
	}
	if invite.PartyID != owner.Party.ID {
		t.Fatalf("invite party = %d, want the inviter's party %d", invite.PartyID, owner.Party.ID)
	}
}

func TestRuntimePartyManagerRebindInvalidatesPriorGenerationInvites(t *testing.T) {
	manager := NewRuntimePartyManager()
	created := manager.Create(testRuntimePartyMember(101, 4), alignedcmd.PartyState{MaxMembers: 4})
	if !manager.RecordInvite(202, 7, 101, 4, created.Party.ID, 13) {
		t.Fatal("record invite")
	}
	rebound := manager.RebindSession(101, 4, testRuntimePartyMember(101, 5))
	if !rebound.OK || rebound.Party.ID != created.Party.ID {
		t.Fatalf("rebind = %+v", rebound)
	}
	if _, accepted := manager.ConsumePeerRequest(202, 7, 101, 4, 13); accepted {
		t.Fatal("invite from retired session was accepted")
	}
	if stale := manager.Leave(101, 4); stale.Reason != "stale_session" {
		t.Fatalf("stale leave = %+v", stale)
	}
	if current, found := manager.SnapshotByUser(101, 5); !found || current.ID != created.Party.ID {
		t.Fatalf("rebound snapshot = %+v found=%v", current, found)
	}
}

func TestRuntimePartyManagerRepositionMovesLeaderWithItsIdentity(t *testing.T) {
	manager := NewRuntimePartyManager()
	created := manager.Create(testRuntimePartyMember(101, 1), alignedcmd.PartyState{MaxMembers: 4})
	for _, userID := range []uint16{202, 303} {
		if joined := manager.Join(created.Party.ID, testRuntimePartyMember(userID, 1)); !joined.OK {
			t.Fatalf("join %d = %+v", userID, joined)
		}
	}
	if moved := manager.Reposition(101, 1, 1, 2); !moved.OK {
		t.Fatalf("reposition = %+v", moved)
	}
	snapshot, _ := manager.SnapshotByID(created.Party.ID)
	if runtimePartySlot(snapshot, 101) != 0 || runtimePartySlot(snapshot, 202) != 2 || runtimePartySlot(snapshot, 303) != 1 {
		t.Fatalf("reposition snapshot = %+v", snapshot)
	}
	if moved := manager.Reposition(101, 1, 0, 1); !moved.OK ||
		runtimePartySlot(moved.Party, 101) != 1 || runtimePartySlot(moved.Party, 303) != 0 || moved.Party.Leader != 101 {
		t.Fatalf("leader reposition = %+v", moved)
	}
}

func TestRuntimePartyManagerTransferLeaderKeepsPartyAndFixedSlots(t *testing.T) {
	manager := NewRuntimePartyManager()
	created := manager.Create(testRuntimePartyMember(101, 1), alignedcmd.PartyState{MaxMembers: 4})
	joined := manager.Join(created.Party.ID, testRuntimePartyMember(202, 1))
	if !joined.OK {
		t.Fatalf("join = %+v", joined)
	}
	changed := manager.TransferLeader(101, 1, 202, 1)
	if !changed.OK || !changed.LeaderMoved ||
		changed.Party.ID != created.Party.ID || changed.Party.Leader != 202 ||
		runtimePartySlot(changed.Party, 101) != 0 || runtimePartySlot(changed.Party, 202) != 1 {
		t.Fatalf("transfer = %+v", changed)
	}
	oldLeaderState := changed.Party.StateFor(101)
	newLeaderState := changed.Party.StateFor(202)
	if oldLeaderState.IsLeader || !newLeaderState.IsLeader ||
		oldLeaderState.UserID != 202 || newLeaderState.UserID != 202 ||
		oldLeaderState.Members[0].Slot != 0 || oldLeaderState.Members[1].Slot != 1 {
		t.Fatalf("old=%+v new=%+v", oldLeaderState, newLeaderState)
	}
}

func TestRuntimePartyManagerEnsureLeaderIsIdempotent(t *testing.T) {
	manager := NewRuntimePartyManager()
	first := manager.EnsureLeader(testRuntimePartyMember(101, 1), alignedcmd.PartyState{MaxMembers: 4, SelectionID: 7})
	if !first.OK {
		t.Fatalf("first ensure = %+v", first)
	}
	second := manager.EnsureLeader(testRuntimePartyMember(101, 1), alignedcmd.PartyState{MaxMembers: 4, SelectionID: 99})
	if !second.OK || second.Party.ID != first.Party.ID || second.Party.Settings.SelectionID != 7 {
		t.Fatalf("duplicate ensure = %+v first=%+v", second, first)
	}
}

func testRuntimePartyMember(userID uint16, generation uint64) RuntimePartyMember {
	return RuntimePartyMember{UserID: userID, SessionGeneration: generation, State: alignedcmd.PartyMemberState{UserID: userID, UserState: 1, HPPercent: 100, MPPercent: 100}}
}

func runtimePartySlot(snapshot RuntimePartySnapshot, userID uint16) byte {
	for _, member := range snapshot.Members {
		if member.UserID == userID {
			return member.Slot
		}
	}
	return 0xff
}
