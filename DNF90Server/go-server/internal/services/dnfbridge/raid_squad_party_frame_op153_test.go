package dnfbridge

import (
	"testing"

	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfproto "longheng.io/server/internal/modules/dnf/protocol"
)

// A party frame's HP/MP rows travel only in op153
// (party.BuildSingleMemberRealtimeInfo). A frame whose last party packet is a bare
// kind-0 op9 therefore leaves the client's party HUD health bars empty, and the
// operator's live report (2026-09-20) is that the leader then cannot lead the party
// into a dungeon until a leadership change re-publishes op9+op153:
//
//	"攻坚队分队后自动组建的队伍中，队友血量和蓝量显示为空，此时队长无法带领队伍
//	 进入副本，必须更换队长刷新队伍成员的血蓝状态后才能正常进入副本。"
//
// The auto-formed squad party published exactly such a frame: its phase-three
// same-area peer projection ends every member's own connection on an op9.

// partyFrameTail walks one connection's packets and reports the message id of the
// last party-frame packet, plus how many op9 records were left without any later
// op153 on the same connection.
func partyFrameTail(packets []dnfproto.ChannelPacket) (tail uint16, bareOp9 int) {
	seenRealtimeAfter := false
	for index := len(packets) - 1; index >= 0; index-- {
		switch packets[index].Header.MsgID {
		case uint16(dnfenum.CmdPacketRecoverStamina):
			if !seenRealtimeAfter {
				bareOp9++
			}
			if tail == 0 {
				tail = packets[index].Header.MsgID
			}
		case 0x0099:
			seenRealtimeAfter = true
			if tail == 0 {
				tail = packets[index].Header.MsgID
			}
		}
	}
	return tail, bareOp9
}

func TestRuntimeRaidSquadPartyFrameEndsWithOp153(t *testing.T) {
	fixture := newRuntimeRaidSquadProjectionFixture(t, []runtimeRaidMemberState{
		runtimeRaidSquadProjectionMember(1001, 1, 0),
		runtimeRaidSquadProjectionMember(1002, 1, 1),
	}, true)
	// The squad party's own members stand in one town area, so the projection's
	// phase-three peer broadcast really reaches them -- that is the packet order
	// the live client saw.
	for _, id := range []uint16{1001, 1002} {
		fixture.service.onlinePlayers.EnterArea(&onlinePlayerInfo{
			CharacterID: id,
			ChannelID:   200,
			TownID:      19,
			AreaID:      2,
			Session:     fixture.sessions[id],
		})
	}

	state := fixture.state(t)
	if err := fixture.service.applyRuntimeRaidParties(state); err != nil {
		t.Fatalf("first projection: %v", err)
	}

	for _, id := range []uint16{1001, 1002} {
		packets := runtimeRaidPeerTake(t, fixture.sessions[id])
		realtime, ok := runtimeRaidPeerFind(packets, 0x0099)
		if !ok {
			t.Fatalf("char %d received no op153 realtime rows in its own party frame", id)
		}
		if rows := realtime.Body[0]; rows != 2 {
			t.Fatalf("char %d received %d op153 rows, want one per party member (2)", id, rows)
		}
		tail, bare := partyFrameTail(packets)
		if tail != 0x0099 {
			t.Fatalf("char %d's party frame ends on msg_id=%d, want the op153 realtime rows: "+
				"the client renders teammates' HP/MP only from op153", id, tail)
		}
		if bare != 0 {
			t.Fatalf("char %d holds %d op9 party records with no op153 behind them", id, bare)
		}
	}
}
