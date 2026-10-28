package dnfbridge

import (
	"encoding/binary"
	"testing"

	"longheng.io/server/internal/modules/dnf/channelcatalog"
	"longheng.io/server/internal/modules/dnf/dnfenum"
	"longheng.io/server/internal/modules/dnf/raid"
)

// 退出攻坚队必须**永远有回应**，包括"名册只剩一个人 + 客户端发来的 key 读不懂"这种情形。
//
// 真机 2026-10-01 20:50（操作员）：名册只剩 9999 一个人时，他连按三次退出攻坚队，
// 服务端什么都没做。报文是 op665 body=0003 —— 既不是他自己（22）也不是攻坚队 key 的
// 低 16 位（1）。旧代码只看"是队长且 key 不是我"，于是把它当成"队长踢一个不存在的
// 成员"，拒绝且不留日志 —— 玩家看到的就是"点了没反应"。
//
// 现在超时空多一条回落：key 认不出名册里的人时按"自己退出"处理（安东/卢克不动）。
func TestFiendWarLeaveAcceptsAnUnreadableKeyWhenOneMemberIsLeft(t *testing.T) {
	service := &Service{
		options: options{gameUpperBodyCodec: gameUpperBodyCodecPlain},
		raids:   make(map[uint32]*runtimeRaidState),
	}
	connection := &bufferConn{}
	session := &gameSession{conn: connection}
	session.channel = channelcatalog.Channel{ID: 184, Type: 42} // 超时空攻坚频道
	service.bindGameSessionCharacter(session, 22)

	battle, err := raid.NewFiendWarBattle(&raid.FiendWarBattleConfig{
		Raid:  &raid.AntonConfig{PhaseMax: 1, MemberMax: 8},
		Rules: []raid.FiendWarRule{{}},
		Areas: map[int]raid.FiendWarArea{2: {Index: 2, MemberMax: 8, Next: []int{6}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	service.raids[12058625] = &runtimeRaidState{
		RaidKey:       12058625,
		LeaderID:      22,
		RaidMode:      runtimeRaidOwnerFiendWar,
		Started:       true,
		Members:       []runtimeRaidMemberState{{CharID: 22, GroupIndex: 2}},
		FrozenMembers: []runtimeRaidMemberState{{CharID: 22, GroupIndex: 2}},
		Lifecycle: runtimeRaidLifecycleState{
			Status:   runtimeRaidLifecyclePhaseActive,
			Phase:    1,
			FiendWar: battle,
		},
	}

	body := make([]byte, 2)
	binary.LittleEndian.PutUint16(body, 3) // 真机那一脚

	// 回包失败不算这一脚没生效：名册效果才是这条用例要钉的东西（状态太少时
	// sendRuntimeSingleRaidEnded 可能因为缺少战场快照而报错，那是另一条线）。
	_ = service.handleOnlineLeaveRaid(session, uint16(dnfenum.CmdPacketLeaveRaid), body)

	if _, exists := service.raids[12058625]; exists {
		t.Fatal("名册只剩一个人时，退出攻坚队没有生效：raid 还在")
	}
	if connection.write.Len() == 0 {
		t.Fatal("退出攻坚队没有回包：客户端会一直等")
	}
}
