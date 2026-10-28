package dnfbridge

import (
	"encoding/binary"
	"sync"
	"sync/atomic"
	"time"

	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfproto "longheng.io/server/internal/modules/dnf/protocol"
)

// 卡住诊断（2026-09-30）——**只打日志，不改任何行为**。
//
// 线上最频繁的"卡住"是同一类形状：服务端的某条分支判定不过就 `return nil`，**一个包都不发**；
// 而 DNF 客户端没有请求超时，于是"服务端沉默"就等于"客户端永久卡住"（同类结论见
// 过图卡死_同坐标换图被静默丢弃_根因与修复_20260922.md）。玩家侧的表现有两种：
//
//	· op39 怪物死亡（玩家口中的"怪物卡白"）：dungeon_combat_runtime.go 里既有
//	  "记一条 game-dungeon-monster-death-blocked 再 return nil"的分支，也有什么都不记的；
//	· op123 通关结算（game-dungeon-character-statistic-blocked /
//	  completed_settlement_entry_owner_not_ready）：17 小时 926 次、单角色峰值 152 次。
//
// 本文件**不做**判定、不发包、不改状态机，只回答四个问题：
//
//  1. 同一个目标被客户端重试了多少次、持续多久？（客户端 20~60ms 重发一次；
//     线上实测 object_key=69014 重发了 104 次）
//  2. 服务端给出的原因是什么？（沿用既有 blocked 事件里的 reason，不新增词表）
//  3. 这条会话最后发出去的 upper 包是什么、多久之前？（"客户端在等一个永远不来的包"）
//  4. 后来恢复了没有？
//
// 与既有日志的关系：**既有事件一字不改**（每条照旧打、字段与顺序都不动），本文件只在其上
// 追加两类事件：
//
//	<kind>-stuck    同一 (kind,key) 连续被拒达到阈值（默认 5 次）时打一条，
//	                之后每 dungeonStallRepeatInterval 补一条更新过的计数；
//	<kind>-resumed  该目标后来真的回了包（op38 发出且目标匹配）时打一条，并清零计数。
//
// 阈值与间隔都是常量：排障时直接看 retry_count/elapsed_ms 判断"这是不是真卡住"。
const (
	// dungeonStallRetryThreshold 同一目标连续被拒这么多次 → 判定卡住并升级事件。
	dungeonStallRetryThreshold = 5
	// dungeonStallRepeatInterval 判定卡住之后，隔多久补一条更新过的 stuck 事件。
	// 客户端重发风暴可以持续几分钟，没有这个限频会被刷屏。
	dungeonStallRepeatInterval = 10 * time.Second
	// dungeonStallMaxKeys 单会话同时跟踪的目标上限（超出后淘汰最久未更新的一个）。
	dungeonStallMaxKeys = 64
)

// 诊断分类。名字直接进事件名（<kind>-stuck / <kind>-resumed），保持可 grep。
const (
	dungeonStallKindMonsterDeath = "game-dungeon-monster-death"
	dungeonStallKindSettlement   = "game-dungeon-settlement-entry"
)

// dungeonStallKey 是聚合维度：同一类请求 + 同一个目标。
type dungeonStallKey struct {
	kind string
	key  uint32
}

// dungeonStallEntry 是一个目标的连续被拒计量。
type dungeonStallEntry struct {
	// streak 是连续被拒次数；收到该目标的 op38 应答后清零。
	streak int
	// attempts 是本会话累计次数（不清零），用来区分"偶发"与"习惯性"。
	attempts int
	// firstSeen 是本轮 streak 的起点。
	firstSeen time.Time
	// lastSeen 是最近一次被拒的时间。
	lastSeen time.Time
	// lastEmit 是最近一次打 stuck 事件的时间（限频用）。
	lastEmit time.Time
	// lastReason 是最近一次被拒的原因（来自既有 blocked 事件的 reason 字段）。
	lastReason string
	// escalated 表示本轮已经升级过（决定恢复时要不要补 -resumed）。
	escalated bool
}

// dungeonStallTracker 是**单会话**的卡住诊断器。它挂在 gameSession 上，按连接隔离，
// 因此不需要与别的会话竞争；对外只有原子量与一把小锁。
type dungeonStallTracker struct {
	mu    sync.Mutex
	keys  map[dungeonStallKey]*dungeonStallEntry
	order []dungeonStallKey

	// 最后一次发出的 upper 包（由出站单一漏斗写入，纯诊断）。
	lastSendOp atomic.Uint32
	lastSendAt atomic.Int64
}

func newDungeonStallTracker() *dungeonStallTracker {
	return &dungeonStallTracker{keys: make(map[dungeonStallKey]*dungeonStallEntry)}
}

// stallTracker 返回本会话的诊断器；裸会话（测试里大量存在）没有它，此时所有诊断自动关闭。
func (session *gameSession) stallTracker() *dungeonStallTracker {
	if session == nil {
		return nil
	}
	return session.stall
}

// noteBlocked 记一次"服务端本轮不会回包"，返回本次是否要打升级事件。
//
// 调用方负责打**既有**的 blocked 事件（格式不变）；本方法只在达到阈值时限频地
// 追加一条 <kind>-stuck。
func (t *dungeonStallTracker) noteBlocked(kind string, key uint32, reason string, now time.Time) (escalate bool, entry dungeonStallEntry) {
	if t == nil {
		return false, dungeonStallEntry{}
	}
	t.mu.Lock()
	current := t.entryLocked(dungeonStallKey{kind: kind, key: key}, now)
	current.streak++
	current.attempts++
	current.lastSeen = now
	if reason != "" {
		current.lastReason = reason
	}
	if current.streak >= dungeonStallRetryThreshold &&
		(current.lastEmit.IsZero() || now.Sub(current.lastEmit) >= dungeonStallRepeatInterval) {
		escalate = true
		current.escalated = true
		current.lastEmit = now
	}
	snapshot := *current
	t.mu.Unlock()
	return escalate, snapshot
}

// noteDeathResponse 是 op38（怪物死亡通知）发出时的回声：目标匹配则结束本轮计量。
// 返回 true 表示"本轮曾经升级过、现在恢复了"，调用方据此补一条 resumed 事件。
func (t *dungeonStallTracker) noteDeathResponse(targetKey uint16, now time.Time) (resumed bool, entry dungeonStallEntry) {
	if t == nil {
		return false, dungeonStallEntry{}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	// 目标 key 是 u16：只可能匹配到能寻址的目标（这正是 >0xFFFF 的目标永远收不到
	// op38 的那条已知边界，见 dungeon_death_response.go 的 key range 判定）。
	entryKey := dungeonStallKey{kind: dungeonStallKindMonsterDeath, key: uint32(targetKey)}
	current := t.keys[entryKey]
	if current == nil || current.streak == 0 {
		return false, dungeonStallEntry{}
	}
	snapshot := *current
	resumed = current.escalated
	current.streak = 0
	current.escalated = false
	current.lastEmit = time.Time{}
	return resumed, snapshot
}

// entryLocked 取（必要时新建）一个目标的计量；超出上限时淘汰最久未更新的。
func (t *dungeonStallTracker) entryLocked(key dungeonStallKey, now time.Time) *dungeonStallEntry {
	if current, ok := t.keys[key]; ok {
		return current
	}
	if len(t.keys) >= dungeonStallMaxKeys {
		t.evictOldestLocked()
	}
	current := &dungeonStallEntry{firstSeen: now}
	t.keys[key] = current
	t.order = append(t.order, key)
	return current
}

func (t *dungeonStallTracker) evictOldestLocked() {
	oldestIndex := -1
	var oldestKey dungeonStallKey
	var oldestSeen time.Time
	for index, key := range t.order {
		entry, ok := t.keys[key]
		if !ok {
			continue
		}
		if oldestIndex < 0 || entry.lastSeen.Before(oldestSeen) {
			oldestIndex = index
			oldestKey = key
			oldestSeen = entry.lastSeen
		}
	}
	if oldestIndex < 0 {
		return
	}
	delete(t.keys, oldestKey)
	t.order = append(t.order[:oldestIndex], t.order[oldestIndex+1:]...)
}

// logDungeonStallBlocked 打一条"服务端本轮不会回包"的事件。
//
// fields 与调用方原先传给 logGameEvent 的**完全一致（含 "reason"）**，因此既有日志
// 逐字节不变；这里额外做两件事：把 reason 记进聚合器、达到阈值时追加 stuck 事件。
//
// ⚠️ 调用点大多**正持有 session.dungeon.mu**（例如 op39 处理函数在锁内记录被拒原因），
// 所以本函数以及它调用的所有东西都不得再取那把锁——上下文读取走 TryLock，见
// dungeonStallContextFields。
func (s *Service) logDungeonStallBlocked(session *gameSession, kind, event string, key uint32, fields ...any) {
	if s == nil {
		return
	}
	tracker := session.stallTracker()
	if tracker == nil {
		s.logGameEvent(session, event, fields...)
		return
	}
	now := time.Now()
	escalate, entry := tracker.noteBlocked(kind, key, dungeonStallReasonFromFields(fields), now)
	// 先打既有事件，再打升级事件：日志顺序与时间序一致。
	s.logGameEvent(session, event, fields...)
	if !escalate {
		return
	}
	s.logGameEvent(session, kind+"-stuck", s.dungeonStallStuckFields(session, tracker, key, entry, fields)...)
}

// dungeonStallStuckFields 组装升级事件字段：既有的 reason 照带，另加次数、时长、
// 最后一次出站包与副本上下文，目的是一行就能判断"谁卡在哪、卡了多久、服务端最后说了什么"。
//
// callerFields 是调用方本来就要打的那些字段（dungeon_id / maze_index / related_actor_object_key …）：
// 它们是在锁内取好的现成值，直接复用，既丰富又不冒重入死锁的风险。
func (s *Service) dungeonStallStuckFields(session *gameSession, tracker *dungeonStallTracker, key uint32, entry dungeonStallEntry, callerFields []any) []any {
	now := time.Now()
	fields := []any{
		"object_key", key,
		"reason", entry.lastReason,
		"retry_count", entry.streak,
		"attempts_total", entry.attempts,
		"elapsed_ms", now.Sub(entry.firstSeen).Milliseconds(),
	}
	fields = append(fields, dungeonStallSendFields(tracker, now)...)
	fields = append(fields, dungeonStallContextFields(session)...)
	seen := map[string]bool{"object_key": true, "reason": true}
	for index := 0; index+1 < len(callerFields); index += 2 {
		name, ok := callerFields[index].(string)
		if !ok || seen[name] {
			continue
		}
		seen[name] = true
		fields = append(fields, callerFields[index], callerFields[index+1])
	}
	return fields
}

// dungeonStallSendFields 报告"这条会话最后发出去的是什么包、多久之前"。
// 客户端卡住时，这一对字段最直接：它在等的东西没有来。
func dungeonStallSendFields(tracker *dungeonStallTracker, now time.Time) []any {
	if tracker == nil {
		return nil
	}
	op := tracker.lastSendOp.Load()
	at := tracker.lastSendAt.Load()
	if at == 0 {
		return []any{"last_send_op", 0, "last_send_ago_ms", -1}
	}
	return []any{
		"last_send_op", op,
		"last_send_ago_ms", now.Sub(time.Unix(0, at)).Milliseconds(),
	}
}

// dungeonStallContextFields 尽力读取当前副本上下文（只读快照）。
//
// ⚠️ **绝不能阻塞**：多数调用点（op39 的被拒分支、结算的被拒分支）正持有
// session.dungeon.mu，直接 Lock 会自我死锁（Go 的 Mutex 不可重入）。
// 这里用 TryLock：拿得到就补上 dungeon_id/run_status/room，拿不到就只回 char_id，
// 由调用方现场字段（dungeon_id / maze_index …）兜底——只影响日志丰富度，不影响正确性。
func dungeonStallContextFields(session *gameSession) []any {
	if session == nil {
		return nil
	}
	fields := []any{"char_id", session.selectedCharacterID}
	if !session.dungeon.mu.TryLock() {
		return fields
	}
	defer session.dungeon.mu.Unlock()
	runtime := session.dungeon.runtime
	if runtime == nil {
		return append(fields, "dungeon_state", "no_runtime")
	}
	fields = append(fields, "dungeon_id", runtime.Dungeon.ID, "maze_index", runtime.MazeIndex)
	if runtime.Session != nil {
		fields = append(fields, "run_status", string(runtime.Session.Snapshot().Run.Status))
		if scene, ok := runtime.Session.Scene(); ok {
			fields = append(fields, "room", scene.Coordinate.String(), "map_id", scene.Map.Map.ID)
		}
	}
	return fields
}

// dungeonStallReasonFromFields 从既有事件的字段列表里取 "reason" 的值。
// 字段按 key/value 交替排列；取第一个字符串 key == "reason" 的值。
func dungeonStallReasonFromFields(fields []any) string {
	for index := 0; index+1 < len(fields); index += 2 {
		key, ok := fields[index].(string)
		if !ok || key != "reason" {
			continue
		}
		if value, ok := fields[index+1].(string); ok {
			return value
		}
	}
	return ""
}

// upperSendDiagnostics 从语义 upper 包（13 字节头 + body）取出 opcode；
// 当它是 op38（怪物死亡通知）时再取出目标 key（body 前 2 字节）。
// ok=false 表示这块字节短到连包头都不够，直接忽略。
func upperSendDiagnostics(inner []byte) (msgID uint16, target uint16, ok bool) {
	if len(inner) < dnfproto.ChannelHeaderSize {
		return 0, 0, false
	}
	msgID = binary.LittleEndian.Uint16(inner[1:3])
	if msgID != uint16(dnfenum.CmdPacketNotifyDieMonster) {
		return msgID, 0, true
	}
	if len(inner) >= dnfproto.ChannelHeaderSize+2 {
		target = binary.LittleEndian.Uint16(inner[dnfproto.ChannelHeaderSize : dnfproto.ChannelHeaderSize+2])
	}
	return msgID, target, true
}

// noteUpperSendDiagnostics 是出站单一漏斗上的诊断钩子：记住最后一个 upper 包，
// 并在死亡应答（op38）发出时结束卡住计量。
//
// **纯诊断**：只写原子量与日志，不参与任何发包决策；调用点在 writeGameUpperPacketLocked
// 里对语义包（inner）取样，因此 dproto 信封不会把 opcode 盖掉。
func (s *Service) noteUpperSendDiagnostics(session *gameSession, inner []byte) {
	if s == nil || session == nil {
		return
	}
	tracker := session.stallTracker()
	if tracker == nil {
		return
	}
	msgID, target, ok := upperSendDiagnostics(inner)
	if !ok {
		return
	}
	now := time.Now()
	tracker.lastSendOp.Store(uint32(msgID))
	tracker.lastSendAt.Store(now.UnixNano())
	if msgID != uint16(dnfenum.CmdPacketNotifyDieMonster) {
		return
	}
	resumed, entry := tracker.noteDeathResponse(target, now)
	if !resumed {
		return
	}
	fields := []any{
		"object_key", uint32(target),
		"reason", entry.lastReason,
		"retry_count", entry.streak,
		"attempts_total", entry.attempts,
		"stuck_ms", now.Sub(entry.firstSeen).Milliseconds(),
		"resolved_by", "op38",
	}
	s.logGameEvent(session, dungeonStallKindMonsterDeath+"-resumed", fields...)
}
