package dnfbridge

import (
	"context"
	"slices"
	"testing"

	"longheng.io/server/internal/modules/dnf/channelcatalog"
	"longheng.io/server/internal/modules/dnf/dnfenum"
)

// 「攻坚战无论怎么中途放弃 都不应该虚弱」（操作员 2026-10-02 定案）——
// 把"这一次未通关回城算不算攻坚战退出"单独钉住。
//
// 真机 2026-10-02 00:00:36（raid 12058625，分片 20261002-001）：超时空的**失败收尾**
// 给 char 22 挂上了虚弱 9，而对照的走门那次（00:04:24）是豁免的：
//
//	game-dungeon-forced-return-weakness-applied char_id=22
//	  source=fiendwar_failed_waiting_area_abandoned_run_town_return
//	  exemption=none weakness_level=9 client_condition=19
//	game-dungeon-forced-return-weakness-raid-exempt char_id=1
//	  source=current_exe_bodyless_op42_active_run_giveup_abandoned_run_town_return
//	  rule=raid_gate_exit_never_weakens
//
// 差别在于**服务器自己把绑定清空了**：returnRuntimeFiendWarDungeons 的 ended 分支
// 先 `runtime.raidDungeon = runtimeRaidDungeonBinding{}`，于是"看绑定"的那条豁免失效。
func TestForcedReturnWeaknessExemptsEveryRaidExit(t *testing.T) {
	service := &Service{}
	// 超时空频道（类型 42）；普通频道用来做反例。
	fiendWar := &gameSession{selectedCharacterID: 22,
		channel: channelcatalog.Channel{Type: dnfenum.FiendWarRaidChannelType}}
	ordinary := &gameSession{selectedCharacterID: 22,
		channel: channelcatalog.Channel{Type: 11}}

	// 1) 走门那一种：run 上还带着攻坚绑定 → 豁免。
	raidRun := &runtimeDungeonState{raidDungeon: runtimeRaidDungeonBinding{RaidKey: 12058625}}
	rule, exempt := service.currentForcedReturnWeaknessRaidExempt(fiendWar, raidRun)
	if !exempt || rule != "raid_gate_exit_never_weakens" {
		t.Fatalf("带着攻坚绑定的退出被判成要虚弱：rule=%q exempt=%t", rule, exempt)
	}

	// 2) **绑定已被服务器自己清空**的超时空失败收尾 → 也必须豁免。
	//    这就是真机 00:00:36 的那一条。
	clearedBinding := &runtimeDungeonState{}
	rule, exempt = service.currentForcedReturnWeaknessRaidExempt(fiendWar, clearedBinding)
	if !exempt || rule != "raid_channel_exit_never_weakens" {
		t.Fatalf("超时空失败收尾（绑定已清空）被判成要虚弱：rule=%q exempt=%t\n"+
			"（真机 2026-10-02 00:00:36 就是这条弱点：weakness_level=9 exemption=none）", rule, exempt)
	}

	// 3) 普通频道、没有攻坚绑定 → 照旧虚弱（这条放松不许漏到普通副本）。
	if rule, exempt := service.currentForcedReturnWeaknessRaidExempt(ordinary, clearedBinding); exempt {
		t.Fatalf("普通副本中途退出被豁免了：rule=%q", rule)
	}

	// 4) 空值安全。
	if _, exempt := service.currentForcedReturnWeaknessRaidExempt(nil, nil); exempt {
		t.Fatal("nil session 被放行了")
	}
	if _, exempt := service.currentForcedReturnWeaknessRaidExempt(ordinary, nil); exempt {
		t.Fatal("nil runtime + 普通频道被放行了")
	}
}

// 上一条豁免只许落在超时空：**安徒恩(23)/卢克(32) 的频道位一个都不许被覆盖**。
//
// 超时空那条 `raid_channel_exit_never_weakens` 是"看频道类型"的（不是看绑定），所以
// 它天生比第一条宽 —— 这条用例就是它的护栏：把 `isCurrentFiendWarRaidChannel` 的
// 判据放宽成"任何攻坚频道"、或者把 42 写成 23/32，这里必须立刻变红。
//
// 两条规则的分工（都逐字保留）：
//
//	raid_gate_exit_never_weakens  —— 看 run 上的攻坚绑定，三个团本**一视同仁**：
//	                                 站在门里退出来本来就不该虚弱，这是既有行为。
//	raid_channel_exit_never_weakens —— 只看超时空频道位；绑定已被服务器清空时兜底。
func TestForcedReturnWeaknessExemptionNeverCoversAntonOrLukeChannels(t *testing.T) {
	service := &Service{}
	anton := &gameSession{selectedCharacterID: 1,
		channel: channelcatalog.Channel{Type: dnfenum.RaidChannelType}}
	luke := &gameSession{selectedCharacterID: 1,
		channel: channelcatalog.Channel{Type: dnfenum.HiddenRaidType}}
	if dnfenum.RaidChannelType == dnfenum.FiendWarRaidChannelType ||
		dnfenum.HiddenRaidType == dnfenum.FiendWarRaidChannelType {
		t.Fatalf("频道位常量撞车：安东 %d / 卢克 %d / 超时空 %d",
			dnfenum.RaidChannelType, dnfenum.HiddenRaidType, dnfenum.FiendWarRaidChannelType)
	}

	// 没有攻坚绑定的中途退出（= 普通副本那一类）：安东/卢克必须**照旧虚弱**。
	// 超时空在这一格是豁免的（上一条用例第 2 段），差别只能来自频道类型。
	clearedBinding := &runtimeDungeonState{}
	for name, session := range map[string]*gameSession{"安徒恩": anton, "卢克": luke} {
		rule, exempt := service.currentForcedReturnWeaknessRaidExempt(session, clearedBinding)
		if exempt {
			t.Fatalf("%s 频道（type %d）的中途退出被超时空那条规则豁免了：rule=%q —— "+
				"`raid_channel_exit_never_weakens` 只能认超时空的 %d",
				name, session.channel.Type, rule, dnfenum.FiendWarRaidChannelType)
		}
	}

	// 走门（run 上还带攻坚绑定）在三个团本上都是一样的：既有行为，逐字保留。
	raidRun := &runtimeDungeonState{raidDungeon: runtimeRaidDungeonBinding{RaidKey: 12058625}}
	for name, session := range map[string]*gameSession{"安徒恩": anton, "卢克": luke} {
		rule, exempt := service.currentForcedReturnWeaknessRaidExempt(session, raidRun)
		if !exempt || rule != "raid_gate_exit_never_weakens" {
			t.Fatalf("%s 带着攻坚绑定的退出被判成要虚弱：rule=%q exempt=%t", name, rule, exempt)
		}
	}
}

// 「中途放弃不得虚弱」的**端到端**护栏：真走一遍 C2S op42（handleDungeonGiveupGame），
// 按频道位对比同一条路上会不会挂上虚弱。
//
// 超时空的豁免是 `raid_channel_exit_never_weakens`（只看频道类型 = 42），所以必须证明
// 它**没有**顺带把安徒恩(23)/卢克(32) 也豁免掉 —— 那才是"改了超时空别动其他团本"。
// 两条对照都要求 client_condition 那一步照旧出现（回城包序里的 op33weakness），
// 而超时空那一条必须一个字节都不发。
func TestOp42GiveUpNeverWeakensOnTheFiendWarChannel(t *testing.T) {
	for _, fixture := range []struct {
		name         string
		channelType  uint8
		wantWeakness bool
	}{
		{"超时空频道 type 42", dnfenum.FiendWarRaidChannelType, false},
		{"安徒恩频道 type 23（对照）", dnfenum.RaidChannelType, true},
		{"卢克频道 type 32（对照）", dnfenum.HiddenRaidType, true},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			service, session, runtime := newBackToVillageRuntime(t)
			if runtime == nil {
				t.Fatal("夹具没有造出一次在跑的副本")
			}
			session.channel = channelcatalog.Channel{ID: 184, Type: fixture.channelType}

			if err := service.handleDungeonGiveupGame(session, nil); err != nil {
				t.Fatalf("op42 放弃: %v", err)
			}
			signatures := townPostTransitionPacketSignatures(t, session.conn.(*bufferConn).write.Bytes())
			shipped := slices.Contains(signatures, "op33weakness")
			if shipped != fixture.wantWeakness {
				t.Fatalf("op42 之后的回城包序 = %v（op33weakness=%t, want %t）",
					signatures, shipped, fixture.wantWeakness)
			}

			repositories, ok := service.repositoryGroup()
			if !ok || repositories.Character == nil {
				t.Fatal("角色仓库不可用")
			}
			character, found, err := repositories.Character.Load(context.Background(), "99")
			if err != nil || !found {
				t.Fatalf("读角色 found=%t err=%v", found, err)
			}
			level := character.Stats[currentWeaknessStatKey]
			if fixture.wantWeakness && level == 0 {
				t.Fatalf("对照频道（type %d）没有挂上虚弱：stamina=%d —— 豁免漏到别的团本了",
					fixture.channelType, level)
			}
			if !fixture.wantWeakness && level != 0 {
				t.Fatalf("超时空中途放弃被挂上了虚弱 stamina=%d —— "+
					"操作员裁定「攻坚战无论怎么中途放弃 都不应该虚弱」", level)
			}
		})
	}
}
