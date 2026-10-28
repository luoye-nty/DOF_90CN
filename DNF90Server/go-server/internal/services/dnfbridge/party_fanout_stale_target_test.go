package dnfbridge

import (
	"fmt"
	"testing"

	"longheng.io/server/internal/modules/dnf/dnfenum"
)

// staleWriteConn stands in for a socket that a previous session generation
// already closed. Every write fails exactly the way a closed TCPConn fails:
//
//	set tcp 10.1.0.16:10023: use of closed network connection
//
// which is the error recorded live at 2026-09-25 06:02:14Z for port 10023 (the
// channel-23 socket of the 05:24 raid generation, closed at 05:37:50).
type staleWriteConn struct {
	bufferConn
}

func (c *staleWriteConn) Write(_ []byte) (int, error) {
	return 0, fmt.Errorf("set tcp 10.1.0.16:10023: use of closed network connection")
}

// TestWalkoutPartyMemberSurvivesADeadMemberSocket pins the live incident.
//
// A leader who kicks a member whose own socket is already gone must keep his own
// connection. The party fan-out used to return the MEMBER's write error to the
// KICKER's handler; game_session_events.go treats such an error as fatal and
// calls session.conn.Close(), so the kicker was disconnected with
// 「网络连接中断」 (2026-09-25 06:02:14Z, leader 1111 / game-002957; char 9 at
// 06:02:33 and char 36 at 06:04:26 were lost the same way).
//
// This test deliberately uses only long-standing symbols so the same body can be
// compiled against the pre-fix tree to prove it is a real regression test.
func TestWalkoutPartyMemberSurvivesADeadMemberSocket(t *testing.T) {
	service, leader, sessions := newForcedExitLeaveTestParty(t, 3)
	victim := sessions[1]

	state := runtimePartyStateSnapshot(leader)
	victimSlot := byte(0)
	found := false
	for _, member := range state.Members {
		if member.UserID == victim.selectedCharacterID {
			victimSlot = member.Slot
			found = true
		}
	}
	if !found {
		t.Fatalf("victim %d is not in the party roster: %+v", victim.selectedCharacterID, state.Members)
	}

	// The kicked member's socket is already closed before the kick is processed.
	victim.conn = &staleWriteConn{}
	leader.conn.(*bufferConn).write.Reset()

	err := service.handleOnlineWalkoutPartyMember(
		leader,
		uint16(dnfenum.CmdPacketWalkoutPartyMember),
		[]byte{victimSlot},
	)
	if err != nil {
		t.Fatalf("a dead member socket became the kicker's own failure: %v", err)
	}
	if leader.conn.(*bufferConn).write.Len() == 0 {
		t.Fatal("the kicker never received its op14 acknowledgement")
	}
}
