package dnfbridge

import (
	"encoding/binary"
	"testing"

	"longheng.io/server/internal/modules/dnf/dnfenum"
	"longheng.io/server/internal/modules/dnf/raid"
)

func runtimeRaidManagerWorkBody(operation uint32, member uint16, argument uint32) []byte {
	body := make([]byte, 12)
	binary.LittleEndian.PutUint32(body[0:4], operation)
	binary.LittleEndian.PutUint32(body[4:8], uint32(member))
	binary.LittleEndian.PutUint32(body[8:12], argument)
	return body
}

func TestRuntimeRaidManagerWorkTransfersLeadershipToLiveMember(t *testing.T) {
	fixture := newRuntimeRaidDisconnectFixture(t, 1001, 1002, 1003)
	for _, session := range fixture.sessions {
		session.conn.(*bufferConn).write.Reset()
	}

	if err := fixture.service.handleOnlineRaidManagerWork(fixture.sessions[1001], uint16(dnfenum.CmdPacketRaidManagerWork), runtimeRaidManagerWorkBody(runtimeRaidManagerWorkTransferLeader, 1002, 0)); err != nil {
		t.Fatalf("leader transfer: %v", err)
	}
	state := fixture.state(t)
	if state.LeaderID != 1002 {
		t.Fatalf("leader=%d want 1002", state.LeaderID)
	}
	for id, session := range fixture.sessions {
		packets := runtimeRaidPeerTake(t, session)
		object, ok := runtimeRaidPeerFind(packets, raid.RaidObjectMsgID)
		if !ok {
			t.Fatalf("member %d received no mode=0 object after the transfer", id)
		}
		wantObject, err := raid.BuildRaidObjectMode0Body(runtimeRaidObjectMode0(state))
		if err != nil {
			t.Fatal(err)
		}
		if string(object.Body) != string(wantObject) {
			t.Fatalf("member %d object body=%x want=%x", id, object.Body, wantObject)
		}
		if _, ok := runtimeRaidPeerFind(packets, raid.RaidMemberRefreshMsgID); !ok {
			t.Fatalf("member %d received no mode=3 roster after the transfer", id)
		}
	}
	// A subgroup move from the old leader is now refused: authority moved.
	if _, ok := fixture.service.moveRuntimeRaidMember(1001, 1003, 2); ok {
		t.Fatal("old leader could still edit subgroups after transferring leadership")
	}
}

func TestRuntimeRaidManagerWorkRejectsBadTransfers(t *testing.T) {
	fixture := newRuntimeRaidDisconnectFixture(t, 1001, 1002, 1003)
	fixture.disconnect(t, 1003)

	// Non-leader cannot transfer.
	if _, ok := fixture.service.transferRuntimeRaidLeader(1002, 1001); ok {
		t.Fatal("non-leader transferred leadership")
	}
	// Target inside its disconnect grace window cannot lead.
	if _, ok := fixture.service.transferRuntimeRaidLeader(1001, 1003); ok {
		t.Fatal("leadership went to a disconnected member")
	}
	// Self and outsiders are rejected.
	if _, ok := fixture.service.transferRuntimeRaidLeader(1001, 1001); ok {
		t.Fatal("self transfer accepted")
	}
	if _, ok := fixture.service.transferRuntimeRaidLeader(1001, 4242); ok {
		t.Fatal("transfer to a non-member accepted")
	}
	// A non-zero argument on the transfer operation is refused by the handler.
	fixture.sessions[1002].conn.(*bufferConn).write.Reset()
	if err := fixture.service.handleOnlineRaidManagerWork(fixture.sessions[1001], uint16(dnfenum.CmdPacketRaidManagerWork), runtimeRaidManagerWorkBody(runtimeRaidManagerWorkTransferLeader, 1002, 3)); err != nil {
		t.Fatalf("transfer with argument: %v", err)
	}
	if fixture.state(t).LeaderID != 1001 {
		t.Fatal("transfer with a non-zero argument changed the leader")
	}
	// After the ready countdown begins, leadership is frozen.
	fixture.service.raidMu.Lock()
	fixture.service.raids[fixture.raidKey].StartPending = true
	fixture.service.raidMu.Unlock()
	if _, ok := fixture.service.transferRuntimeRaidLeader(1001, 1002); ok {
		t.Fatal("leadership transferred during the ready countdown")
	}
}

func TestRuntimeRaidManagerWorkOperationZeroStillMovesGroups(t *testing.T) {
	fixture := newRuntimeRaidDisconnectFixture(t, 1001, 1002)
	if err := fixture.service.handleOnlineRaidManagerWork(fixture.sessions[1001], uint16(dnfenum.CmdPacketRaidManagerWork), runtimeRaidManagerWorkBody(runtimeRaidManagerWorkAssignGroup, 1002, 3)); err != nil {
		t.Fatalf("group move: %v", err)
	}
	state := fixture.state(t)
	index := runtimeRaidMemberIndex(state, 1002)
	if index < 0 || state.Members[index].GroupIndex != 3 || state.LeaderID != 1001 {
		t.Fatalf("operation 0 result: member=%+v leader=%d", state.Members[index], state.LeaderID)
	}
	// An unknown operation changes nothing.
	if err := fixture.service.handleOnlineRaidManagerWork(fixture.sessions[1001], uint16(dnfenum.CmdPacketRaidManagerWork), runtimeRaidManagerWorkBody(7, 1002, 1)); err != nil {
		t.Fatalf("unknown operation: %v", err)
	}
	if after := fixture.state(t); after.Members[runtimeRaidMemberIndex(after, 1002)].GroupIndex != 3 || after.LeaderID != 1001 {
		t.Fatal("unknown operation mutated the raid")
	}
}

// TestRuntimeRaidRegroupRunsUntilThePresentationFreezesIt pins the regroup gate
// the leader actually experiences:
//
//   - a running attack stays editable, including while a squad is inside a
//     dungeon (the cross-area case: the leader in the waiting area, the member
//     inside a dungeon run). Every one of those requests used to be dropped
//     silently, which reads in game as "团长和队员不在同一区域就改不了编制";
//   - the ready countdown and the movie/card presentation still freeze the
//     roster, because those are the windows that decide and pay out squads;
//   - a regroup that happens after the start has to reach FrozenMembers, which
//     phase two binds groups from.
func TestRuntimeRaidRegroupRunsUntilThePresentationFreezesIt(t *testing.T) {
	fixture := newRuntimeRaidDisconnectFixture(t, 1001, 1002, 1003)
	fixture.service.raidMu.Lock()
	state := fixture.service.raids[fixture.raidKey]
	state.Started = true
	state.EntryCostPaid = true
	state.FrozenMembers = append([]runtimeRaidMemberState(nil), state.Members...)
	state.Lifecycle = newRuntimeRaidLifecycleState(fixture.queue.Now())
	state.Lifecycle.Status = runtimeRaidLifecyclePhaseActive
	state.Lifecycle.Phase = runtimeRaidDungeonPhaseOne
	// Squads 1 and 3 are still inside a dungeon run while the leader rearranges
	// the roster from the waiting area.
	state.Lifecycle.GroupOccupancy[runtimeRaidDungeonSeat{Group: 1}] = runtimeRaidDungeonBinding{RaidKey: fixture.raidKey, Phase: 1, MapID: 213}
	fixture.service.raidMu.Unlock()

	moved, ok := fixture.service.moveRuntimeRaidMember(1001, 1003, 2)
	if !ok {
		t.Fatal("regroup rejected during an active phase with a squad inside a dungeon")
	}
	frozen, found := runtimeRaidFrozenMember(moved, 1003)
	if !found || frozen.GroupIndex != 2 {
		t.Fatalf("frozen roster did not follow the active-phase regroup: %+v", frozen)
	}
	live := moved.Members[runtimeRaidMemberIndex(moved, 1003)]
	if live.GroupIndex != 2 || live.SlotOrder != frozen.SlotOrder {
		t.Fatalf("live=%+v frozen=%+v diverged after the regroup", live, frozen)
	}
	assertRuntimeRaidGridUnique(t, moved)

	// Standby keeps working the same way.
	fixture.service.raidMu.Lock()
	state.Lifecycle.Status = runtimeRaidLifecycleStandby
	fixture.service.raidMu.Unlock()
	if _, ok := fixture.service.moveRuntimeRaidMember(1001, 1003, 3); !ok {
		t.Fatal("regroup rejected during phase-one standby")
	}

	// The ready countdown freezes the roster: the preparation snapshot the
	// countdown validated must be the roster that starts.
	fixture.service.raidMu.Lock()
	state.StartPending = true
	fixture.service.raidMu.Unlock()
	if _, ok := fixture.service.moveRuntimeRaidMember(1001, 1002, 3); ok {
		t.Fatal("regroup accepted during the ready countdown")
	}

	// So does the movie/card presentation, which resolves the per-squad rewards.
	fixture.service.raidMu.Lock()
	state.StartPending = false
	state.Lifecycle.Status = runtimeRaidLifecycleAwaitingRewards
	fixture.service.raidMu.Unlock()
	if _, ok := fixture.service.moveRuntimeRaidMember(1001, 1002, 3); ok {
		t.Fatal("regroup accepted during the reward presentation")
	}
}
