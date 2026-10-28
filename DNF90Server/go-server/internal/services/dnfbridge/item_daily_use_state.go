package dnfbridge

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	dnffatigue "longheng.io/server/internal/modules/dnf/fatigue"
)

// currentItemDailyUseStateMsgID is the client's 每日可用次数 state packet.
//
// Reverse engineering 2026-09-15 (DNF.exe):
//
//	client registration : RVA 0x0197F3F2  push 0x21D; push handler; call register
//	handler             : RVA 0x0197B2B0  (per-item byte applier)
//	handler effect      : its only call into the item is sub_01F20590, the unique
//	                      writer of [item+0x46C] / [item+0x470]
//	tooltip             : "每天可用次数 : %d/%d" (string 33077) is formatted from
//	                      sub_01F17F70(item) = [item+0x470] - [item+0x46C] and
//	                      [definition+0x3F4]
//
// The op14 item row never writes those fields, and op541 does not appear anywhere
// in this server's packet log: the counter was frozen at the full value because
// the packet was never sent.
//
// Class: the registration table that carries op541 (registrar RVA 0x01E62980,
// 994 entries) does NOT contain op44/op515, which this server has always sent
// successfully as class 1, while op541 itself answers nothing when sent as class
// 1 (live 2026-09-14 23:50: the client kept rendering 3/3 with class 1). So op541
// belongs to the other registry: class 0.
const currentItemDailyUseStateMsgID uint16 = 541

// currentItemDailyUseStateClass is the wire classification of the state packet.
const currentItemDailyUseStateClass byte = 0

// buildCurrentItemDailyUseStateBody encodes the per-item counters.
//
// Layout (from the handler's own reads): a leading count byte, then one entry per
// item of a 4-byte item id followed by the used-count byte. The client looks each
// id up in the receiving character's containers and applies the byte to that
// item.
func buildCurrentItemDailyUseStateBody(states []alignedcmd.DailyUseState) ([]byte, error) {
	if len(states) == 0 {
		return nil, nil
	}
	if len(states) > 0xFF {
		return nil, fmt.Errorf("daily use state carries %d items, maximum is 255", len(states))
	}
	body := make([]byte, 0, 1+5*len(states))
	body = append(body, byte(len(states)))
	for _, state := range states {
		if state.ItemID == 0 {
			return nil, fmt.Errorf("daily use state item id is zero")
		}
		body = append(body,
			byte(state.ItemID), byte(state.ItemID>>8), byte(state.ItemID>>16), byte(state.ItemID>>24),
			state.Used)
	}
	return body, nil
}

// dailyUseStatesFromCounters turns the durable "used today" counters into the
// client's state packet. The byte is the used count itself, so the durable stat
// can be projected as-is -- zero included, which resets the tooltip to the full
// value (live 2026-09-15: sending 2 with one potion already spent rendered 1/3,
// i.e. 上限 - 字节).
func dailyUseStatesFromCounters(stats map[string]int64) []alignedcmd.DailyUseState {
	if len(stats) == 0 {
		return nil
	}
	used := make(map[uint32]int64, 4)
	collect := func(prefix string) {
		for key, value := range stats {
			if !strings.HasPrefix(key, prefix) {
				continue
			}
			itemID, err := strconv.ParseUint(strings.TrimPrefix(key, prefix), 10, 32)
			if err != nil || itemID == 0 {
				continue
			}
			if value < 0 {
				value = 0
			}
			if value > 0xFF {
				value = 0xFF
			}
			// A zero counter is still emitted on purpose: it is what clears a
			// client that stayed online across the 06:00 reset. Coexisting
			// prefixes keep the larger value instead of emitting twice.
			if current, seen := used[uint32(itemID)]; !seen || value > current {
				used[uint32(itemID)] = value
			}
		}
	}
	// The generic key is the owner for every [total usable count] item; the
	// legacy fatigue-potion spelling stays readable so a counter written before
	// the migration is not lost within the same service day. An item present
	// under both keys keeps the larger value, so it cannot be counted twice.
	collect(dnffatigue.ItemUsageStatPrefix)
	collect(dnffatigue.PotionUsageStatPrefix)
	if len(used) == 0 {
		return nil
	}
	states := make([]alignedcmd.DailyUseState, 0, len(used))
	for itemID, value := range used {
		states = append(states, alignedcmd.DailyUseState{ItemID: itemID, Used: byte(value)})
	}
	sort.Slice(states, func(i, j int) bool { return states[i].ItemID < states[j].ItemID })
	return states
}

// sendCurrentCharacterDailyUseStates replays those counters once the character's
// containers are visible. The client's 每日可用次数 state is memory-only, so
// without this replay every login would show a partly used item as untouched
// until its next use.
func (s *Service) sendCurrentCharacterDailyUseStates(session *gameSession, source string) error {
	if s == nil || session == nil || session.selectedCharacterID == 0 {
		return nil
	}
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.Character == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	character, found, err := repositories.Character.Load(ctx, strconv.Itoa(int(session.selectedCharacterID)))
	if err != nil || !found {
		return err
	}
	states := dailyUseStatesFromCounters(character.Stats)
	body, err := buildCurrentItemDailyUseStateBody(states)
	if err != nil {
		s.logGameEvent(session, "game-item-daily-use-state-blocked",
			"source", source,
			"states", len(states),
			"error", err)
		return nil
	}
	if len(body) == 0 {
		return nil
	}
	if err := s.sendCurrentItemDailyUseStatePacket(session, body); err != nil {
		return err
	}
	s.logGameEvent(session, "game-item-daily-use-state-replayed",
		"source", source,
		"char_id", session.selectedCharacterID,
		"entries", len(states),
		"body_len", len(body),
		"msg_id", currentItemDailyUseStateMsgID,
		"classification", currentItemDailyUseStateClass)
	return nil
}

// sendCurrentItemDailyUseStatePacket emits the state packet.
//
// Class 0. Live 2026-09-15 (23:5x) settled it: the same body sent as class 1 was
// ignored outright (the tooltip kept rendering 3/3), while class 0 was applied
// immediately (one potion already spent, server sent 2, tooltip showed 1/3).
func (s *Service) sendCurrentItemDailyUseStatePacket(session *gameSession, body []byte) error {
	return s.sendGameUpperRawClass(session, currentItemDailyUseStateMsgID, body, currentItemDailyUseStateClass)
}

// sendAlignedDailyUseStates projects the counters a command changed to the client.
// It runs after the command's own ACK so the item state is already visible when
// the tooltip redraws.
func (s *Service) sendAlignedDailyUseStates(session *gameSession, result alignedcmd.Result) {
	if s == nil || session == nil || len(result.DailyUseStates) == 0 {
		return
	}
	body, err := buildCurrentItemDailyUseStateBody(result.DailyUseStates)
	if err != nil {
		s.logGameEvent(session, "game-item-daily-use-state-blocked",
			"operation", result.Operation,
			"states", len(result.DailyUseStates),
			"error", err)
		return
	}
	if len(body) == 0 {
		return
	}
	if err := s.sendCurrentItemDailyUseStatePacket(session, body); err != nil {
		s.logGameEvent(session, "game-item-daily-use-state-blocked",
			"operation", result.Operation,
			"states", len(result.DailyUseStates),
			"error", err)
		return
	}
	for _, state := range result.DailyUseStates {
		s.logGameEvent(session, "game-item-daily-use-state-sent",
			"operation", result.Operation,
			"item_id", state.ItemID,
			"remaining", state.Used,
			"msg_id", currentItemDailyUseStateMsgID,
			"classification", currentItemDailyUseStateClass,
			"body_len", len(body))
	}
}

// sendCurrentItemDailyUseState 把一次"非 aligned 通道"的消耗之后「每天可用次数」
// 的进度告诉客户端。op160 开箱与 op518 礼包都走这里。
//
// 它们不像 op44/op515 那样经过 aligned 通道，所以必须自己发这一包：客户端的已用
// 计数**只**由 op541 写入，不发的话道具的悬浮说明永远停在满值，本地也不会拦住第二次
// 点击 —— 那正是"礼盒的每天可用次数形同不存在"的成因。包序与 aligned 通道一致：
// 业务结果包在前、op541 在后（客户端此时已经拿到了这件道具的对象）。
//
// 发送失败只记日志：消费已经提交，一个投影包不该让到手的奖励回退。
//
// 注意 op27 抽奖罐**不**调用这里：那条路径有一条被测试钉死的"结果包之后不得再有包"
// 约束（真机上插入背包包会让客户端崩溃），其计数改由登录回放刷新。详见 lottery_item.go。
func (s *Service) sendCurrentItemDailyUseState(
	session *gameSession,
	itemID uint32,
	used int64,
	limit int64,
	source string,
) {
	if s == nil || session == nil || limit <= 0 || itemID == 0 {
		return
	}
	if used < 0 {
		used = 0
	}
	if used > 0xFF {
		used = 0xFF
	}
	body, err := buildCurrentItemDailyUseStateBody([]alignedcmd.DailyUseState{{
		ItemID: itemID,
		Used:   byte(used),
	}})
	if err != nil {
		s.logGameEvent(session, "game-item-daily-use-state-blocked", "source", source, "error", err)
		return
	}
	if err := s.sendCurrentItemDailyUseStatePacket(session, body); err != nil {
		s.logGameEvent(session, "game-item-daily-use-state-blocked", "source", source, "error", err)
		return
	}
	s.logGameEvent(session, "game-item-daily-use-state-sent",
		"source", source,
		"item_id", itemID,
		"used", used,
		"limit", limit,
		"msg_id", currentItemDailyUseStateMsgID,
		"classification", currentItemDailyUseStateClass)
}
