package dnfbridge

// raid_log.go —— 团本（攻坚战）专用日志。
//
// 为什么单独开一类
//
//	攻坚相关事件以前全部混在 packet_log.txt 里（那台机器一天 6~10 GB），排查时要在
//	几十万行里翻几十条，且真正有用的字段（谁在等谁、名单几个人、就绪几个、
//	驱动者是谁、某个成员为什么没就绪）当时根本没记。本文件把团本事件单独落一个
//	文件（records/raid-YYYYMMDD-NNN.jsonl），字段尽量全，并且不阻塞玩法。
//
// 与 packet_log 的关系
//
//	同一事件仍然照常进 packet_log（保留原样，便于和客户端 trace 对齐），
//	但高频、低信息量的那些由 suppressPacketLogEvent 过滤掉，降低 packet_log 噪声。
//
// 三条纪律（沿用 activity_record.go 的既有约定）
//
//  1. 旁路：记录器未启用（records == nil）时，所有函数立刻返回，代价是一次指针判空。
//  2. 不阻塞：写入是 channel 入队 + 单协程落盘；这里只做 map 组装。
//  3. 锁安全：**本文件里的函数绝不主动获取 session.dungeon.mu**。
//     攻坚记录大量发生在已经持有 dungeon.mu 的路径上（例如 release 与
//     怪物死亡投影），在那里再取一次锁就是死锁风险。需要 runtime 字段时，
//     由调用方（已经持有 runtime）通过 raidStateFields 显式传进来。
//
// 字段命名约定
//
//	raid_key / raid_phase / lifecycle_status / lifecycle_revision / roster_size /
//	frozen_size / online_members / squad / slot / is_leader / leader_id /
//	party_id / conn_id / character_id / character_name / channel_id /
//	event / event_name / reason / result / wall_ms / error

import (
	"fmt"
	"time"
)

// recordRaidKind 是团本记录的文件类别（records/raid-*.jsonl）。
const recordRaidKind = recordKindRaid

// raidEventName 把事件名翻成中文，便于直接看文件排查。
func raidEventName(event string) string {
	switch event {
	// 生命周期
	case "raid_created":
		return "创建攻坚队"
	case "raid_disbanded":
		return "攻坚队解散"
	case "raid_started":
		return "开始攻坚"
	case "raid_start_rejected":
		return "开始攻坚被拒"
	case "raid_phase_advanced":
		return "阶段推进"
	case "raid_phase_timeout":
		return "阶段攻击超时"
	case "raid_phase_force_return":
		return "阶段收尾强制回城"
	case "raid_cleared":
		return "攻坚通关"
	// 副本门（gate）生命周期
	case "gate_selected":
		return "选中副本门"
	case "gate_loaded":
		return "副本门加载完成"
	case "gate_completed":
		return "副本门通关"
	case "gate_abandoned":
		return "副本门放弃/被踢出"
	case "gate_boundary_fanout":
		return "门边界演员扇出"
	// 读条 / 进图
	case "loading_ready":
		return "成员读条完成登记"
	case "loading_released":
		return "读条放行完成"
	case "loading_release_driver":
		return "读条放行驱动者"
	case "loading_release_skipped":
		return "读条放行跳过"
	case "loading_barrier_timeout":
		return "读条屏障超时"
	case "entry_blocked":
		return "进图被拒"
	case "entry_prepared":
		return "进图已准备"
	case "forced_exit":
		return "被强制移出副本"
	case "forced_return":
		return "被强制回城"
	// 队伍 / 端点 / 中继
	case "party_formed":
		return "组队完成"
	case "party_publish_deferred":
		return "队伍发布被推迟"
	case "party_peer_endpoint":
		return "队友端点登记"
	case "party_peer_endpoint_missing":
		return "队友端点缺失"
	case "party_member_removed":
		return "成员被移出队伍"
	case "party_kick_result":
		return "踢人结果"
	case "relay_leg_missing":
		return "中继腿缺失"
	case "relay_leg_unresolved":
		return "中继腿重试仍失败"
	case "relay_throttled":
		return "中继被限流"
	case "town_return_copresence":
		return "回城共现重建"
	case "scene_tail_deferred":
		return "场景尾巴包被推迟"
	// 奖励
	case "session_cost_summary":
		return "本场服务端成本汇总"
	case "movie_confirm_fallback":
		return "过场确认超时兜底开窗"
	case "reward_cards_opened":
		return "翻牌窗口打开"
	case "reward_cards_open_skipped":
		return "翻牌窗口被跳过"
	case "reward_auto_flipped":
		return "自动翻牌"
	case "reward_manual_flip":
		return "手动翻牌"
	case "reward_delivered":
		return "奖励已发放"
	case "reward_incomplete":
		return "奖励不完整"
	default:
		return event
	}
}

// raidLifecycleStatusName 攻坚生命周期状态中文名。
func raidLifecycleStatusName(status runtimeRaidLifecycleStatus) string {
	switch status {
	case runtimeRaidLifecycleCreated:
		return "已创建"
	case runtimeRaidLifecyclePreparing:
		return "准备中"
	case runtimeRaidLifecyclePhaseActive:
		return "阶段进行中"
	case runtimeRaidLifecycleAwaitingMovie:
		return "等待过场"
	case runtimeRaidLifecycleAwaitingRewards:
		return "等待翻牌"
	case runtimeRaidLifecycleStandby:
		return "待命"
	case runtimeRaidLifecycleFinished:
		return "已完成"
	case runtimeRaidLifecycleAbandoned:
		return "已放弃"
	case runtimeRaidLifecycleFailed:
		return "失败(攻击超时)"
	default:
		return fmt.Sprintf("状态%d", int(status))
	}
}

// raidStateFields 从一份**调用方已经持有**的攻坚状态里取出排查所需字段。
//
// 调用方必须自己保证读 state 的并发安全（本函数不加锁，也不复制整份状态）。
// 缺字段就留空——记录是旁路，不为凑字段去多查一次库或抢一次锁。
func raidStateFields(state *runtimeRaidState, characterID uint16) map[string]any {
	fields := map[string]any{}
	if state == nil {
		return fields
	}
	fields["raid_key"] = state.RaidKey
	fields["raid_phase"] = int(state.Lifecycle.Phase)
	fields["lifecycle_status"] = int(state.Lifecycle.Status)
	fields["lifecycle_status_name"] = raidLifecycleStatusName(state.Lifecycle.Status)
	fields["lifecycle_revision"] = state.Lifecycle.Revision
	fields["raid_started"] = state.Started
	fields["entry_cost_paid"] = state.EntryCostPaid
	fields["start_attempt"] = state.StartAttempt
	fields["leader_id"] = state.LeaderID
	fields["roster_size"] = len(state.Members)
	fields["frozen_size"] = len(state.FrozenMembers)
	if len(state.NameBytes) != 0 {
		fields["raid_name"] = string(state.NameBytes)
	}
	if characterID != 0 {
		fields["is_leader"] = characterID == state.LeaderID
		squad, slot, found := 0, 0, false
		for _, member := range state.Members {
			if member.CharID != characterID {
				continue
			}
			squad = int(member.GroupIndex)
			slot = int(member.SlotOrder)
			found = true
			break
		}
		if found {
			fields["squad"] = squad
			fields["slot"] = slot
			if state.SquadPartyLeaders != nil {
				if partyLeader := state.SquadPartyLeaders[byte(squad)]; partyLeader != 0 {
					fields["squad_party_leader"] = partyLeader
				}
			}
		}
	}
	return fields
}

// raidOnlineMembers 数一遍当前在线的攻坚成员。
//
// 会取 s.mu（onlineGameSession 内部），因此**只在本来就安全的调用点使用**：
// 创建/解散/开始/阶段推进/奖励这类低频事件。高频路径（门边界扇出、读条放行）
// 不要调用它，避免在持有 dungeon.mu 的调用栈里引入新的锁序。
func (s *Service) raidOnlineMembers(state *runtimeRaidState) (online int, total int) {
	if s == nil || state == nil {
		return 0, 0
	}
	for _, member := range state.FrozenMembers {
		total++
		if session, ok := s.onlineGameSession(member.CharID); ok && session != nil && session.conn != nil {
			online++
		}
	}
	if total == 0 {
		for _, member := range state.Members {
			total++
			if session, ok := s.onlineGameSession(member.CharID); ok && session != nil && session.conn != nil {
				online++
			}
		}
	}
	return online, total
}

// recordRaidEvent 记一条团本事件（轻量上下文：只读会话上已有的普通字段）。
func (s *Service) recordRaidEvent(session *gameSession, event string, fields map[string]any) {
	if s == nil || s.records.Load() == nil {
		return
	}
	merged := s.activitySessionFields(session)
	if session != nil {
		merged["conn_id"] = session.connID
		merged["character_generation"] = session.characterGeneration
		merged["session_online"] = session.conn != nil
	}
	merged["event"] = event
	merged["event_name"] = raidEventName(event)
	for key, value := range fields {
		merged[key] = value
	}
	s.recordActivity(recordRaidKind, merged)
}

// recordRaidStateEvent 记一条团本事件，并带上调用方持有的攻坚状态与在线人数。
// 只用于低频事件（见 raidOnlineMembers 的说明）。
func (s *Service) recordRaidStateEvent(session *gameSession, event string, state *runtimeRaidState, fields map[string]any) {
	if s == nil || s.records.Load() == nil {
		return
	}
	merged := fields
	if merged == nil {
		merged = map[string]any{}
	}
	charID := uint16(0)
	if session != nil {
		charID = session.selectedCharacterID
	}
	for key, value := range raidStateFields(state, charID) {
		merged[key] = value
	}
	if online, total := s.raidOnlineMembers(state); total > 0 {
		merged["online_members"] = online
		merged["roster_members"] = total
	}
	s.recordRaidEvent(session, event, merged)
}

// recordRaidStateEventLight 与 recordRaidStateEvent 相同，但**不**去数在线人数。
//
// 热路径/可能持有 session.dungeon.mu 的调用栈必须用这个版本：数在线人数要取 s.mu，
// 在已持有 dungeon.mu 的栈里再取 s.mu 会引入新的锁序。
func (s *Service) recordRaidStateEventLight(session *gameSession, event string, state *runtimeRaidState, fields map[string]any) {
	if s == nil || s.records.Load() == nil {
		return
	}
	merged := fields
	if merged == nil {
		merged = map[string]any{}
	}
	charID := uint16(0)
	if session != nil {
		charID = session.selectedCharacterID
	}
	for key, value := range raidStateFields(state, charID) {
		merged[key] = value
	}
	s.recordRaidEvent(session, event, merged)
}

// raidMillis 把时长写成毫秒（float，保留一位小数），便于直接排序/筛选。
func raidMillis(d time.Duration) float64 {
	return float64(d.Microseconds()) / 1000
}

// raidWallField 组装"这次操作花了多久"。
func raidWallField(d time.Duration) map[string]any {
	return map[string]any{"wall_ms": raidMillis(d)}
}

// errorText 把 error 写成可读字符串（nil 记空串，便于 JSON 里直接看）。
func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// raidMemberIDs 把成员列表压成 [1 12 22] 这样的数组，便于看名单。
func raidMemberIDsFromState(members []runtimeRaidMemberState) []uint16 {
	out := make([]uint16, 0, len(members))
	for _, member := range members {
		out = append(out, member.CharID)
	}
	return out
}

// raidBitsCount 数一个 8 位槽位位图里有几个人（队伍最多 4 人，8 位足够）。
func raidBitsCount(mask uint8) int {
	count := 0
	for bit := uint8(1); bit != 0; bit <<= 1 {
		if mask&bit != 0 {
			count++
		}
	}
	return count
}

// raidMissingSlots 返回"期望到齐但还没登记"的槽位，用于直接点名谁卡住了。
func raidMissingSlots(ready, expected uint8) []int {
	missing := make([]int, 0, 8)
	for slot := 0; slot < 8; slot++ {
		bit := uint8(1) << slot
		if expected&bit != 0 && ready&bit == 0 {
			missing = append(missing, slot)
		}
	}
	return missing
}
