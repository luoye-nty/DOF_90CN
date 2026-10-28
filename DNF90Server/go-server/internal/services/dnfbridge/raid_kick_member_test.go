package dnfbridge

import (
	"encoding/binary"
	"testing"

	"longheng.io/server/internal/modules/dnf/dnfenum"
	"longheng.io/server/internal/modules/dnf/raid"
)

// op665 is one opcode for two gestures: a leaver names itself, and a leader
// kicking somebody names that member. Reading it as "always the sender" made the
// leader remove itself and hand the raid to the member it meant to kick --
// measured live as the leader's own connection sending body u16=2 while member 2
// was kicked.
func TestRuntimeRaidLeaderKickRemovesTheMemberNotTheLeader(t *testing.T) {
	fixture := newRuntimeRaidDisconnectFixture(t, 1001, 1002)
	leader := fixture.sessions[1001]
	leader.conn.(*bufferConn).write.Reset()

	body := make([]byte, 2)
	binary.LittleEndian.PutUint16(body, 1002)
	if err := fixture.service.handleOnlineLeaveRaid(leader, uint16(dnfenum.CmdPacketLeaveRaid), body); err != nil {
		t.Fatalf("leader kick: %v", err)
	}

	state := fixture.state(t)
	if runtimeRaidMemberIndex(state, 1001) < 0 {
		t.Fatal("the leader removed itself instead of the member")
	}
	if state.LeaderID != 1001 {
		t.Fatalf("leader=%d want 1001 to keep the raid", state.LeaderID)
	}
	if runtimeRaidMemberIndex(state, 1002) >= 0 {
		t.Fatalf("the kicked member is still on the roster: %+v", state.Members)
	}

	// The op665 result is receiver-relative ("you are out"), so the leader must NOT
	// get one: sub_1D04A00 would tear down the leader's own raid projection.
	leaderPackets := runtimeRaidPeerTake(t, leader)
	if ack, ok := runtimeRaidPeerFind(leaderPackets, uint16(dnfenum.CmdPacketLeaveRaid)); ok {
		t.Fatalf("the leader received an op665 result body=%x and would drop its own raid", ack.Body)
	}
	// The leader does learn about it from the roster refresh.
	if _, ok := runtimeRaidPeerFind(leaderPackets, raid.RaidMemberRefreshMsgID); !ok {
		t.Fatal("the leader received no roster refresh after the kick")
	}

	// The kicked member is the one told it was removed, with the flag that pops
	// string 46791 "已经强制退出攻坚队。" and runs the forced-leave cleanup.
	kicked := runtimeRaidPeerTake(t, fixture.sessions[1002])
	leave, ok := runtimeRaidPeerFind(kicked, uint16(dnfenum.CmdPacketLeaveRaid))
	if !ok {
		t.Fatal("the kicked member received no op665 result")
	}
	if len(leave.Body) != 2 || leave.Body[0] != 1 || leave.Body[1] != 1 {
		t.Fatalf("kicked op665 body=%x want 01 01 (success + forced flag)", leave.Body)
	}
	if _, ok := runtimeRaidPeerFind(kicked, raid.RaidObjectMsgID); !ok {
		t.Fatal("the kicked member received no raid object removal")
	}
}

// A self-leave must keep working through the unchanged path.
func TestRuntimeRaidSelfLeaveStillRemovesTheSender(t *testing.T) {
	fixture := newRuntimeRaidDisconnectFixture(t, 1001, 1002)
	member := fixture.sessions[1002]
	member.conn.(*bufferConn).write.Reset()

	body := make([]byte, 2)
	binary.LittleEndian.PutUint16(body, 1002)
	if err := fixture.service.handleOnlineLeaveRaid(member, uint16(dnfenum.CmdPacketLeaveRaid), body); err != nil {
		t.Fatalf("member leave: %v", err)
	}

	state := fixture.state(t)
	if runtimeRaidMemberIndex(state, 1002) >= 0 {
		t.Fatal("the leaver is still on the roster")
	}
	if runtimeRaidMemberIndex(state, 1001) < 0 || state.LeaderID != 1001 {
		t.Fatalf("the leader was disturbed by a member leaving: %+v", state.Members)
	}
}

// Leaving on your own is never a forced removal, even when it empties the raid.
// The op665 flag byte is what makes the client print 46791 "已经强制退出攻坚队。", so
// tying it to the disband result made an ordinary leave show that message.
func TestRuntimeRaidSelfLeaveThatDisbandsKeepsTheForcedFlagClear(t *testing.T) {
	fixture := newRuntimeRaidDisconnectFixture(t, 1001)
	leader := fixture.sessions[1001]
	leader.conn.(*bufferConn).write.Reset()

	body := make([]byte, 2)
	binary.LittleEndian.PutUint16(body, 1001)
	if err := fixture.service.handleOnlineLeaveRaid(leader, uint16(dnfenum.CmdPacketLeaveRaid), body); err != nil {
		t.Fatalf("leader leave: %v", err)
	}

	leave, ok := runtimeRaidPeerFind(runtimeRaidPeerTake(t, leader), uint16(dnfenum.CmdPacketLeaveRaid))
	if !ok {
		t.Fatal("the leaver received no op665 result")
	}
	if len(leave.Body) != 2 || leave.Body[0] != 1 || leave.Body[1] != 0 {
		t.Fatalf("leave body=%x want 01 00 (success, forced flag clear)", leave.Body)
	}
	// This departure really did disband the raid; the flag must not follow it.
	fixture.service.raidMu.Lock()
	_, alive := fixture.service.raids[fixture.raidKey]
	fixture.service.raidMu.Unlock()
	if alive {
		t.Fatal("fixture leave did not disband the raid")
	}
}

// A key that names nobody on the roster must not be read as a kick.
func TestRuntimeRaidLeaderKickWithAnUnknownKeyIsIgnored(t *testing.T) {
	fixture := newRuntimeRaidDisconnectFixture(t, 1001, 1002)
	leader := fixture.sessions[1001]
	leader.conn.(*bufferConn).write.Reset()

	body := make([]byte, 2)
	binary.LittleEndian.PutUint16(body, 4242)
	if err := fixture.service.handleOnlineLeaveRaid(leader, uint16(dnfenum.CmdPacketLeaveRaid), body); err != nil {
		t.Fatalf("leader kick with an unknown key: %v", err)
	}

	state := fixture.state(t)
	if runtimeRaidMemberIndex(state, 1001) < 0 || runtimeRaidMemberIndex(state, 1002) < 0 {
		t.Fatalf("an unknown key changed the roster: %+v", state.Members)
	}
}
