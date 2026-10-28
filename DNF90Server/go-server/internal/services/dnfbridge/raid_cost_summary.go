package dnfbridge

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// raid_cost_summary.go —— 每场攻坚的"服务端成本汇总"。
//
// 为什么需要它
//
//	操作者的实测条件：**超过 8 人时收不到玩家侧的日志**（8 开是他本机，其他人是外面的玩家）。
//	所以"这场到底卡没卡、掉没掉"必须只用服务端证据回答。服务端本来就有这些测量：
//
//	  · 每个 loop 的慢事件（SlowEventStats：次数 / 最坏时长 / 是哪一类事件）
//	  · 写不进客户端（game-write-stalled-on-client）
//	  · 演员扇出分片耗时（game-raid-mate-actor-install-chunk）
//	  · 中继腿端口耗尽 / 缺失
//	  · 断线原因（read-close 是 RST/FIN、有没有半帧）
//
//	但它们散落在成千上万行里。本文件把一场攻坚的这些数字聚合成**一行**团本记录
//	（event=session_cost_summary），于是 >8 人的实测只要把服务端 logs\ 给出来，或者
//	操作者自己看最后一行，就能判定：卡是服务端造成的（某 loop 慢事件）还是客户端
//	自己（写阻塞、RST、闪退）。
//
// 纪律
//
//	旁路：只累加计数，不做 I/O、不加锁跨 gameplay 锁；emit 时只读会话上已有的字段。
//	不阻塞：emit 由时间队列回调在**发起者之外的上下文**触发，单条记录。
//	不抢锁：聚合表用独立 mutex（raidCostMu），绝不与 s.mu / dungeon.mu / raidMu 交叉持有。

const (
	// raidCostSummaryTick 是进行中的攻坚每隔多久打一次汇总。够短，能在一场长阶段里
	// 至少留一条；够长，不会把 packet log 变成流水账。
	raidCostSummaryTick = 10 * time.Minute
	// raidCostSummaryWorstLimit 是汇总里列出的"最慢 N 个成员"，避免 40 人时一行过长。
	raidCostSummaryWorstLimit = 8
)

// runtimeRaidCostCounters 是一场攻坚期间累加的计数器。
type runtimeRaidCostCounters struct {
	memberIDs map[uint16]bool

	installChunks     int64
	installChunkWorst float64
	installs          int64
	installsWithMode1 int64

	writeStalls     int64
	writeStallWorst float64

	relayPortExhausted int64
	relayLegMissing    int64

	disconnects    int64
	lastDisconnect string
}

type runtimeRaidCostRegistry struct {
	mu       sync.Mutex
	counters map[uint32]*runtimeRaidCostCounters
}

func (r *runtimeRaidCostRegistry) counterLocked(raidKey uint32) *runtimeRaidCostCounters {
	if r.counters == nil {
		r.counters = make(map[uint32]*runtimeRaidCostCounters)
	}
	counter := r.counters[raidKey]
	if counter == nil {
		counter = &runtimeRaidCostCounters{memberIDs: make(map[uint16]bool)}
		r.counters[raidKey] = counter
	}
	return counter
}

// noteRaidCost records one observation for a raid. It is safe to call from any
// goroutine and from paths that hold gameplay locks: it takes only its own mutex.
func (s *Service) noteRaidCost(raidKey uint32, apply func(*runtimeRaidCostCounters)) {
	if s == nil || raidKey == 0 || apply == nil {
		return
	}
	s.raidCost.mu.Lock()
	defer s.raidCost.mu.Unlock()
	apply(s.raidCost.counterLocked(raidKey))
}

// raidCostMember adds one member to the raid's summary roster, so the emitted row
// can name whose loop was worst.
func (s *Service) raidCostMember(raidKey uint32, characterID uint16) {
	if characterID == 0 {
		return
	}
	s.noteRaidCost(raidKey, func(counter *runtimeRaidCostCounters) {
		counter.memberIDs[characterID] = true
	})
}

func (s *Service) noteRaidCostInstallChunk(raidKey uint32, installs int, elapsed time.Duration) {
	s.noteRaidCost(raidKey, func(counter *runtimeRaidCostCounters) {
		counter.installChunks++
		counter.installs += int64(installs)
		if ms := raidMillis(elapsed); ms > counter.installChunkWorst {
			counter.installChunkWorst = ms
		}
	})
}

func (s *Service) noteRaidCostWriteStall(raidKey uint32, elapsed time.Duration) {
	s.noteRaidCost(raidKey, func(counter *runtimeRaidCostCounters) {
		counter.writeStalls++
		if ms := raidMillis(elapsed); ms > counter.writeStallWorst {
			counter.writeStallWorst = ms
		}
	})
}

func (s *Service) noteRaidCostRelayPortExhausted(raidKey uint32) {
	s.noteRaidCost(raidKey, func(counter *runtimeRaidCostCounters) { counter.relayPortExhausted++ })
}

func (s *Service) noteRaidCostRelayLegMissing(raidKey uint32) {
	s.noteRaidCost(raidKey, func(counter *runtimeRaidCostCounters) { counter.relayLegMissing++ })
}

func (s *Service) noteRaidCostDisconnect(raidKey uint32, reason string) {
	s.noteRaidCost(raidKey, func(counter *runtimeRaidCostCounters) {
		counter.disconnects++
		counter.lastDisconnect = reason
	})
}

// armRuntimeRaidCostSummary schedules the periodic summary for a running raid.
// The timer name carries the raid key, so re-arming replaces the pending tick
// instead of stacking one per phase activation.
//
// Deliberately NOT a timer. The activation path's scheduling is pinned exactly by
// the phase-retry tests, and a diagnostic must not perturb what it measures: the
// summary is emitted synchronously at each phase activation (covering the phase
// that just ended) and at a terminal transition, which is also when a reader wants
// it. The cost of one emission is a handful of atomics plus one log line.
func (s *Service) armRuntimeRaidCostSummary(state runtimeRaidState) {
}

// publishRuntimeRaidCostSummary writes one row that answers "did this raid cost
// anyone a freeze" without needing a single client-side log.
func (s *Service) publishRuntimeRaidCostSummary(raidKey uint32, reason string) {
	if s == nil || raidKey == 0 {
		return
	}
	state, known := s.runtimeRaidForMemberKey(raidKey)

	s.raidCost.mu.Lock()
	counter := s.raidCost.counters[raidKey]
	snapshot := runtimeRaidCostCounters{}
	roster := make([]uint16, 0, 8)
	if counter != nil {
		snapshot = *counter
		for memberID := range counter.memberIDs {
			roster = append(roster, memberID)
		}
	}
	s.raidCost.mu.Unlock()
	if len(roster) == 0 {
		// Nothing was ever recorded for this raid key (or it was already emitted and
		// cleared). An all-zero row with member_count=0 is pure noise -- live
		// 2026-09-29 03:25 produced exactly one when the disband ran after every
		// session was gone -- so the ledger entry is dropped instead.
		s.raidCost.mu.Lock()
		if live := s.raidCost.counters[raidKey]; live != nil && len(live.memberIDs) == 0 {
			delete(s.raidCost.counters, raidKey)
		}
		s.raidCost.mu.Unlock()
		return
	}
	sort.Slice(roster, func(i, j int) bool { return roster[i] < roster[j] })

	type memberSlow struct {
		memberID  uint16
		count     int64
		worstMS   float64
		worstType string
		online    bool
	}
	slowest := make([]memberSlow, 0, len(roster))
	totalSlow := int64(0)
	worstOverall := 0.0
	worstOverallType := ""
	worstMember := uint16(0)
	onlineMembers := 0
	for _, memberID := range roster {
		session, online := s.onlineGameSession(memberID)
		if !online || session == nil || session.events == nil || session.events.loop == nil {
			slowest = append(slowest, memberSlow{memberID: memberID})
			continue
		}
		onlineMembers++
		count, worst, worstType := session.events.loop.SlowEventStats()
		totalSlow += count
		ms := float64(worst.Microseconds()) / 1000
		if ms > worstOverall {
			worstOverall = ms
			worstOverallType = worstType
			worstMember = memberID
		}
		slowest = append(slowest, memberSlow{
			memberID: memberID, count: count, worstMS: ms, worstType: worstType, online: true,
		})
	}
	sort.Slice(slowest, func(i, j int) bool { return slowest[i].worstMS > slowest[j].worstMS })
	if len(slowest) > raidCostSummaryWorstLimit {
		slowest = slowest[:raidCostSummaryWorstLimit]
	}
	parts := make([]string, 0, len(slowest))
	for _, entry := range slowest {
		parts = append(parts, fmt.Sprintf("%d:%.0fms/%d/%s", entry.memberID, entry.worstMS, entry.count, entry.worstType))
	}

	relayPorts, relayCapacity, relayLegs := 0, 0, 0
	if relay := s.currentPartyUDPRelay(); relay != nil && relay.Enabled() {
		relayPorts, relayCapacity, relayLegs = relay.Usage()
	}

	fields := map[string]any{
		"reason":                reason,
		"member_count":          len(roster),
		"online_members":        onlineMembers,
		"slow_event_total":      totalSlow,
		"slow_event_worst_ms":   worstOverall,
		"slow_event_worst_type": worstOverallType,
		"slow_event_worst_member": func() int {
			if worstMember == 0 {
				return 0
			}
			return int(worstMember)
		}(),
		"slow_events_per_member": strings.Join(parts, " "),
		"install_chunks":         snapshot.installChunks,
		"installs":               snapshot.installs,
		"installs_with_mode1":    snapshot.installsWithMode1,
		"install_chunk_worst_ms": snapshot.installChunkWorst,
		"write_stalls":           snapshot.writeStalls,
		"write_stall_worst_ms":   snapshot.writeStallWorst,
		"relay_legs":             relayLegs,
		"relay_ports_used":       relayPorts,
		"relay_port_capacity":    relayCapacity,
		"relay_port_exhausted":   snapshot.relayPortExhausted,
		"relay_leg_missing":      snapshot.relayLegMissing,
		"disconnects":            snapshot.disconnects,
		"last_disconnect_reason": snapshot.lastDisconnect,
	}
	if known {
		fields["raid_phase"] = int(state.Lifecycle.Phase)
	}
	s.logPacketEvent("game-raid-cost-summary",
		"raid_key", raidKey,
		"member_count", len(roster),
		"online_members", onlineMembers,
		"slow_event_total", totalSlow,
		"slow_event_worst_ms", worstOverall,
		"slow_event_worst_type", worstOverallType,
		"slow_events_per_member", strings.Join(parts, " "),
		"install_chunks", snapshot.installChunks,
		"installs", snapshot.installs,
		"installs_with_mode1", snapshot.installsWithMode1,
		"install_chunk_worst_ms", snapshot.installChunkWorst,
		"write_stalls", snapshot.writeStalls,
		"write_stall_worst_ms", snapshot.writeStallWorst,
		"relay_legs", relayLegs,
		"relay_ports_used", relayPorts,
		"relay_port_capacity", relayCapacity,
		"relay_port_exhausted", snapshot.relayPortExhausted,
		"relay_leg_missing", snapshot.relayLegMissing,
		"disconnects", snapshot.disconnects,
		"last_disconnect_reason", snapshot.lastDisconnect,
		"source", "server_side_only_no_client_logs_needed")
	if known {
		s.recordRaidStateEventLight(nil, "session_cost_summary", &state, fields)
		return
	}
	s.recordRaidEvent(nil, "session_cost_summary", fields)
}

// runtimeRaidForMemberKey resolves a raid by key (any member works). It exists so
// the summary can be emitted from a timer callback without knowing a member.
func (s *Service) runtimeRaidForMemberKey(raidKey uint32) (runtimeRaidState, bool) {
	if s == nil || raidKey == 0 {
		return runtimeRaidState{}, false
	}
	s.raidMu.Lock()
	defer s.raidMu.Unlock()
	state := s.raids[raidKey]
	if state == nil {
		return runtimeRaidState{}, false
	}
	return cloneRuntimeRaidState(*state), true
}
