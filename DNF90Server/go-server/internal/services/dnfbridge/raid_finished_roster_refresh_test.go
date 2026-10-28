package dnfbridge

import (
	"bytes"
	"encoding/binary"
	"testing"

	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfproto "longheng.io/server/internal/modules/dnf/protocol"
	"longheng.io/server/internal/modules/dnf/raid"
)

// Operator's report, 2026-09-20: "攻坚结束后 这个成员列表不实时刷新 1队的两个人已经退出
// 攻坚队了 还在攻坚队内的人看成员列表 显示这俩人还在攻坚队". Live raid 1507329:
//
//	13:21:29 game-raid-completed-team-leave-acknowledged char_id=1  member_count=4
//	13:21:36 game-raid-completed-team-leave-acknowledged char_id=21 member_count=4
//
// Both leavers were answered, and no roster packet followed: the live raid row was already
// gone (finalizeRuntimeRaid deletes it), so the remaining members kept drawing all four
// rows. The reduced roster is now rebuilt from the frozen completion record and pushed.
func TestCompletedTeamRaidLeaveRefreshesTheRemainingRoster(t *testing.T) {
	service := &Service{
		options:             options{gameUpperBodyCodec: gameUpperBodyCodecPlain},
		raids:               make(map[uint32]*runtimeRaidState),
		raidRewardCompleted: make(map[runtimeRaidRewardCompletionKey]runtimeRaidSettlementRewardCompletion),
	}
	teamRaidKey := uint32(1507329)
	leaver := &gameSession{conn: &bufferConn{}}
	stayer := &gameSession{conn: &bufferConn{}}
	service.bindGameSessionCharacter(leaver, 13)
	service.bindGameSessionCharacter(stayer, 1)
	completed := runtimeRaidState{
		RaidKey:  teamRaidKey,
		LeaderID: 1,
		TailFlag: 0,
		FrozenMembers: []runtimeRaidMemberState{
			{CharID: 1, Name: "leader", GroupIndex: 1, SlotOrder: 0, UserState: 1, HPPercent: 100, MPPercent: 100},
			{CharID: 13, Name: "member", GroupIndex: 1, SlotOrder: 1, UserState: 1, HPPercent: 100, MPPercent: 100},
		},
		Lifecycle: runtimeRaidLifecycleState{
			Phase:  runtimeRaidDungeonPhaseTwo,
			Status: runtimeRaidLifecycleFinished,
		},
	}
	key := runtimeRaidRewardCompletionKey{RaidKey: teamRaidKey, Phase: runtimeRaidDungeonPhaseTwo, Token: 11}
	service.raidRewardCompleted[key] = runtimeRaidSettlementRewardCompletion{Raid: completed}
	stayer.conn.(*bufferConn).write.Reset()

	body := make([]byte, 2)
	binary.LittleEndian.PutUint16(body, 13)
	if err := service.handleOnlineLeaveRaid(leaver, uint16(dnfenum.CmdPacketLeaveRaid), body); err != nil {
		t.Fatalf("completed team leave: %v", err)
	}

	packets := runtimeRaidPeerTake(t, stayer)
	refresh, ok := runtimeRaidPeerFind(packets, raid.RaidMemberRefreshMsgID)
	if !ok {
		t.Fatal("the remaining member received no roster refresh after a teammate left the released raid")
	}
	// The pushed list must be the frozen roster minus the leaver.
	reduced := cloneRuntimeRaidState(completed)
	reduced.Members = []runtimeRaidMemberState{completed.FrozenMembers[0]}
	reduced.FrozenMembers = []runtimeRaidMemberState{completed.FrozenMembers[0]}
	want, err := raid.BuildMemberRefreshMode3Body(runtimeRaidMemberRefresh(reduced))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(refresh.Body, want) {
		t.Fatalf("refreshed roster body=% X, want the one-member list % X", refresh.Body, want)
	}
	fullBody, err := raid.BuildMemberRefreshMode3Body(runtimeRaidMemberRefresh(completed))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(fullBody, want) {
		t.Fatal("the fixture's reduced list is not actually smaller than the frozen roster")
	}

	// The leaver itself is answered as before: success ACK + its own object removed.
	leaverPackets := runtimeRaidPeerTake(t, leaver)
	ack, ok := runtimeRaidPeerFind(leaverPackets, uint16(dnfenum.CmdPacketLeaveRaid))
	if !ok || !bytes.Equal(ack.Body, raid.BuildLeaveRaidResultBody(false)) {
		t.Fatalf("leaver ack msg=%d body=% X, want the success body", ack.Header.MsgID, ack.Body)
	}
	if removal, ok := runtimeRaidPeerFind(leaverPackets, raid.RaidObjectMsgID); !ok ||
		!bytes.Equal(removal.Body, raid.BuildRaidObjectRemoveBody(teamRaidKey)) {
		t.Fatalf("leaver received msg=%d body=% X, want the raid object removal", removal.Header.MsgID, removal.Body)
	}
	var _ dnfproto.ChannelPacket = refresh
}
