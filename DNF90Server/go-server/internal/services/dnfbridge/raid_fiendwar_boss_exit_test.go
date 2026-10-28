package dnfbridge

import (
	"testing"

	"longheng.io/server/internal/modules/dnf/channelcatalog"
	"longheng.io/server/internal/modules/dnf/raid"
)

// 攻坚打完（罗什已死）之后，副本里的"返回城镇"必须放行 —— 不管这一场的结算是服务端发的，
// 还是客户端自己放的（dungeon_boss_die_settlement.go 的 native 路线一个结算包都不发）。
//
// 真机 2026-10-01 20:35：9999 那一场走 native，服务端没发过任何结算，
// settlementClearRewardSent 永远 false，于是他连按十几次"返回城镇"全部被
// game-dungeon-card-exit-blocked reason=settlement_result_not_ready 挡掉（死按钮），
// 而同一次通关里礼白666 被送上 area 30 坐标 (0,0) 卡在副本里。
//
// 超时空的"阶段完成"由战场自己判：PVF 的 [CLEAR PHASE] 规则把 battle.Complete 立起来
// （见 runtimeRaidPhaseObjectivesComplete 的超时空分支），所以 fixture 直接切这个位。
func TestFiendWarCompletedBossDungeonAcceptsTheExit(t *testing.T) {
	service := &Service{raids: make(map[uint32]*runtimeRaidState)}
	session := &gameSession{}
	session.channel = channelcatalog.Channel{ID: 184, Type: 42} // 超时空攻坚频道
	service.bindGameSessionCharacter(session, 22)

	battle, err := raid.NewFiendWarBattle(&raid.FiendWarBattleConfig{
		Raid:  &raid.AntonConfig{PhaseMax: 1, MemberMax: 8},
		Rules: []raid.FiendWarRule{{}},
		Areas: map[int]raid.FiendWarArea{30: {Index: 30, Dungeon: 4978, Kind: "top boss"}},
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
			Phase:    runtimeRaidDungeonPhaseOne,
			FiendWar: battle,
		},
	}
	runtime := &runtimeDungeonState{
		raidDungeon: runtimeRaidDungeonBinding{RaidKey: 12058625, Phase: runtimeRaidDungeonPhaseOne},
	}

	// 罗什还没死：这一场没打完，不放行。
	if service.fiendWarCompletedDungeonExitReady(session, runtime) {
		t.Fatal("攻坚还没打完就放行了退出")
	}
	// 罗什已死（战场把 Complete 立起来）：必须放行，否则玩家就卡死在里面。
	service.raids[12058625].Lifecycle.FiendWar.Complete = true
	if !service.fiendWarCompletedDungeonExitReady(session, runtime) {
		t.Fatal("攻坚已经打完，退出仍然被挡（真机死按钮）")
	}
	// 不是这一场的人（没有攻坚成员身份）不享受这条放行。
	stranger := &gameSession{}
	stranger.channel = channelcatalog.Channel{ID: 184, Type: 42}
	service.bindGameSessionCharacter(stranger, 99)
	if service.fiendWarCompletedDungeonExitReady(stranger, runtime) {
		t.Fatal("非攻坚成员被放行")
	}
}
