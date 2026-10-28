package dnfbridge

import (
	"context"

	"longheng.io/server/internal/modules/dnf/dnfenum"
	"longheng.io/server/internal/modules/dnf/worldmap"
)

// 超时空：**队长按下撤退（无包体 op42）时，当前这支临时队伍一起回城。**
//
// 操作员 2026-10-02 的定案：
//
//	「然后是战况表撤退 在组成临时队伍后 队长点击战况表的撤退 当前临时队伍所有人
//	  都应该直接回城」
//
// 真机证据（raid 12058625，分片 20261002-006，两两进罗什那次）：
//
//	10:29:10 game-000141 (char 1 = 礼白666，party_id=22 的队长) 发 op42
//	         ENUM_CMDPACKET_GIVEUP_GAME
//	         → game-dungeon-full-town-route-send char_id=1 dungeon_id=4978
//	           source=current_exe_bodyless_op42_active_run_giveup_abandoned_run_town_return
//	         → 只有队长一个人回了城；同队的 char 21 留在那一格里
//	10:29:33 game-000148 (char 21，队员) 连点 op72{1,2}（战况表那个按钮）
//	         → game-dungeon-card-exit-blocked request_value_a=1 request_value_b=2
//	           reason=settlement_result_not_ready   —— 这一窗口里被拒了 119 次
//	         （队员点它不是队长的活，见 leadRuntimePartyHomeFromActiveRun 的守卫）
//	         → 23 秒后他自己又按了一次 op42 才结束
//
// 全日志（2026-10-01 + 2026-10-02）里客户端从来没有发过 661 RAID_DO_BEHAVIOR /
// 667 SET_RAID_WAITING / 670 MODIFY_RAID_INFO 中的任何一个，所以客户端那个按钮
// 就是 op42；而 op72{1,2} 是**队员**手里的同一个战况表按钮。
//
// 现成的整队通道早就有：`sendCurrentActiveDungeonReturnToTownKeepingParty`
// （战况表队长带队回城 / 攻坚阶段收尾都用它，回城后编制保留）。op72 那条路
// (`leadRuntimePartyHomeFromActiveRun`) 已经会遍历全队 —— 缺的只是
// **队长这一脚 op42 也走整队**。
//
// 作用域：只加超时空。安东/卢克一个字不改（它们的小队编制与进图模型不同，
// 而且它们从没报过这个症状）。

// runtimeFiendWarPartyRetreatPlan 是"这一次 op42 要不要把同队的人一起带回家"的
// 快照。必须在**队长自己那一趟回城之前**抓下来：那一趟会把 runtime 上的攻坚绑定
// 清掉，之后就判不出他当时属于哪支临时队伍了。
type runtimeFiendWarPartyRetreatPlan struct {
	armed   bool
	raidKey uint32
	partyID uint16
	leader  uint16
	members []uint16
}

// runtimeFiendWarPartyRetreatPlanFor 判断这一脚是不是"队长在临时队伍里按下撤退"。
// 调用者持 session.dungeon.mu（op42 处理体里就是那个位置）。
func (s *Service) runtimeFiendWarPartyRetreatPlanFor(
	session *gameSession,
	runtime *runtimeDungeonState,
) runtimeFiendWarPartyRetreatPlan {
	if s == nil || session == nil || runtime == nil {
		return runtimeFiendWarPartyRetreatPlan{}
	}
	if !isCurrentFiendWarRaidChannel(session) {
		return runtimeFiendWarPartyRetreatPlan{}
	}
	owner := runtime.partyDungeonOwner
	if owner == nil || runtime.raidDungeon.RaidKey == 0 {
		return runtimeFiendWarPartyRetreatPlan{}
	}
	owner.mu.Lock()
	leader, partyID, retired := owner.leaderID, owner.partyID, owner.retired
	members := make([]uint16, 0, len(owner.members))
	for _, member := range owner.members {
		members = append(members, member.userID)
	}
	owner.mu.Unlock()
	// 只有队长的这一按拉全队；队员自己的退出仍旧只退他自己。
	if retired || partyID == 0 || leader == 0 || leader != session.selectedCharacterID {
		return runtimeFiendWarPartyRetreatPlan{}
	}
	if len(members) == 0 {
		return runtimeFiendWarPartyRetreatPlan{}
	}
	return runtimeFiendWarPartyRetreatPlan{
		armed:   true,
		raidKey: runtime.raidDungeon.RaidKey,
		partyID: partyID,
		leader:  leader,
		members: members,
	}
}

// runRuntimeFiendWarPartyRetreat 把同队其余成员各自送回城。
//
// 队长的自己那一趟由调用方（op42 处理体）走原有通道发，这里**只处理别人**：
// 不重复、也不改动队长那一脚已经发出去的报文序列。
//
// 只带**还在这一场攻坚里、而且还在自己的副本里**的人：已经站在城里的成员不动，
// 别人的副本不碰。占着别人 dungeon 锁的人跳过而不是等待 —— 两个会话互相等会死锁
// （同 leadRuntimePartyHomeFromActiveRun 里那条纪律）。
func (s *Service) runRuntimeFiendWarPartyRetreat(plan runtimeFiendWarPartyRetreatPlan, source string) {
	if s == nil || !plan.armed {
		return
	}
	returned := 0
	for _, charID := range plan.members {
		if charID == 0 || charID == plan.leader {
			continue
		}
		target, online := s.onlineGameSession(charID)
		if !online || target == nil {
			continue
		}
		if !target.dungeon.mu.TryLock() {
			s.logGameEvent(nil, "game-fiendwar-party-retreat-skipped-busy-session",
				"raid_key", plan.raidKey,
				"party_id", plan.partyID,
				"leader_char_id", plan.leader,
				"char_id", charID,
				"source", source)
			continue
		}
		targetRuntime := target.dungeon.runtime
		inside, active := false, false
		if targetRuntime != nil &&
			targetRuntime.raidDungeon.RaidKey == plan.raidKey &&
			dungeonRuntimeOwnsCharacter(targetRuntime, charID) {
			inside = true
			active = targetRuntime.Session.Snapshot().Run.Status == worldmap.DungeonRunActive
		}
		if inside && active {
			// 这一场对他也结束了：标记放弃 + 保留编制（和战况表带队回城同一条路）。
			targetRuntime.raidDungeon.abandonedNotified = true
			targetRuntime.townReturnKeepsSquadLabel = true
		}
		target.dungeon.mu.Unlock()
		if !inside {
			continue
		}
		if !active {
			// 已经通关的成员走他自己的结算/回城链，别抢在他结算前面把他拖回城。
			s.logGameEvent(target, "game-fiendwar-party-retreat-member-skipped",
				"raid_key", plan.raidKey,
				"party_id", plan.partyID,
				"leader_char_id", plan.leader,
				"char_id", charID,
				"dungeon_id", targetRuntime.Dungeon.ID,
				"run_status", targetRuntime.Session.Snapshot().Run.Status,
				"reason", "member_run_is_not_active",
				"source", source)
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
		transition, err := s.prepareCurrentDungeonTownTransitionForSession(ctx, target, charID)
		cancel()
		if err != nil {
			s.logGameEvent(target, "game-fiendwar-party-retreat-member-blocked",
				"raid_key", plan.raidKey,
				"party_id", plan.partyID,
				"leader_char_id", plan.leader,
				"char_id", charID,
				"dungeon_id", targetRuntime.Dungeon.ID,
				"reason", "town_transition_unavailable_for_party_retreat",
				"error", err,
				"source", source)
			continue
		}
		if err := s.sendCurrentActiveDungeonReturnToTownKeepingParty(
			target,
			targetRuntime,
			transition,
			uint16(dnfenum.CmdPacketGiveupGame),
			source,
		); err != nil {
			s.logGameEvent(target, "game-fiendwar-party-retreat-member-blocked",
				"raid_key", plan.raidKey,
				"party_id", plan.partyID,
				"leader_char_id", plan.leader,
				"char_id", charID,
				"dungeon_id", targetRuntime.Dungeon.ID,
				"reason", "party_retreat_town_return_failed",
				"error", err,
				"source", source)
			continue
		}
		returned++
		s.logGameEvent(target, "game-fiendwar-party-retreat-member-returned",
			"raid_key", plan.raidKey,
			"party_id", plan.partyID,
			"leader_char_id", plan.leader,
			"char_id", charID,
			"dungeon_id", targetRuntime.Dungeon.ID,
			"source", source,
			"reason", "the_leader_retreated_so_the_temporary_party_goes_home_together")
	}
	if returned > 0 {
		s.logGameEvent(nil, "game-fiendwar-party-retreat-committed",
			"raid_key", plan.raidKey,
			"party_id", plan.partyID,
			"leader_char_id", plan.leader,
			"returned_members", returned,
			"source", source)
	}
}
