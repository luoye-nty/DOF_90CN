// 本文件拥有限购计数的读写与消费。计数按“归属者 × 周期 × 道具”一条，落点和
// 本服其它限次系统一致：
//
//   - character 维度 → 角色记录的 Stats（int64 map）
//   - account   维度 → 账号记录的 Metadata（string map）
//
// 每条计数配一个窗口起点：窗口变了就等于 0，不需要定时清零任务（与
// dungeonadmission/epic_production 的做法一致）。
package purchaselimit

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	countKeyPrefix  = "purchase_limit_count_"
	windowKeyPrefix = "purchase_limit_window_"
)

// CountKey 返回一条计数在 Stats/Metadata 里的键。作用域由容器本身决定，因此
// 键里只带命名空间、道具与周期。
func CountKey(p Policy) string {
	return countKeyPrefix + p.KeyTag + strconv.FormatUint(uint64(p.ItemID), 10) + "_" + p.Period.String()
}

// WindowKey 返回该计数窗口起点的键。
func WindowKey(p Policy) string {
	return windowKeyPrefix + p.KeyTag + strconv.FormatUint(uint64(p.ItemID), 10) + "_" + p.Period.String()
}

// LimitError 在超出限购时返回，并保留当时的快照供日志使用。
type LimitError struct {
	Policy    Policy
	State     State
	Requested uint32
}

func (e *LimitError) Error() string {
	if e == nil {
		return ErrLimitReached.Error()
	}
	return fmt.Sprintf("%s: item=%d scope=%s period=%s used=%d requested=%d limit=%d",
		ErrLimitReached, e.Policy.ItemID, e.Policy.Scope, e.Policy.Period, e.State.Used, e.Requested, e.Policy.Limit)
}

func (e *LimitError) Unwrap() error { return ErrLimitReached }

// State 是一条计数当前的权威投影。
type State struct {
	Used        uint32
	WindowStart int64
	Limit       uint32
	Remaining   uint32
}

func stateFromUsed(p Policy, used uint32) State {
	state := State{Used: used, Limit: p.Limit}
	if used < p.Limit {
		state.Remaining = p.Limit - used
	}
	return state
}

func windowStartUnix(window Window) int64 {
	if window.Start.IsZero() {
		return 0
	}
	return window.Start.Unix()
}

// ReadCharacter 读取角色维度的计数。
func ReadCharacter(stats map[string]int64, p Policy, now time.Time) State {
	if err := p.Valid(); err != nil || p.Scope != ScopeCharacter {
		return stateFromUsed(p, 0)
	}
	used, _ := stats[CountKey(p)]
	if used < 0 {
		used = 0
	}
	stored := int64(0)
	if window := p.Window(now); !window.Start.IsZero() {
		stored, _ = stats[WindowKey(p)]
		if stored != windowStartUnix(window) {
			used = 0
		}
	}
	if used > int64(p.Limit) {
		used = int64(p.Limit)
	}
	return stateFromUsed(p, uint32(used))
}

// ReadAccount 读取账号维度的计数。
func ReadAccount(metadata map[string]string, p Policy, now time.Time) State {
	if err := p.Valid(); err != nil || p.Scope != ScopeAccount {
		return stateFromUsed(p, 0)
	}
	used := int64(0)
	if raw, ok := metadata[CountKey(p)]; ok {
		if parsed, err := strconv.ParseInt(raw, 10, 64); err == nil && parsed > 0 {
			used = parsed
		}
	}
	if window := p.Window(now); !window.Start.IsZero() {
		stored := int64(0)
		if raw, ok := metadata[WindowKey(p)]; ok {
			if parsed, err := strconv.ParseInt(raw, 10, 64); err == nil {
				stored = parsed
			}
		}
		if stored != windowStartUnix(window) {
			used = 0
		}
	}
	if used > int64(p.Limit) {
		used = int64(p.Limit)
	}
	return stateFromUsed(p, uint32(used))
}

// Read 按策略作用域选择容器读取。
func Read(characterStats map[string]int64, accountMetadata map[string]string, p Policy, now time.Time) State {
	if p.Scope == ScopeAccount {
		return ReadAccount(accountMetadata, p, now)
	}
	return ReadCharacter(characterStats, p, now)
}

// ConsumeCharacter 在角色容器里消费 count 次；超出限购返回 *LimitError。
func ConsumeCharacter(stats map[string]int64, p Policy, count uint32, now time.Time) error {
	if err := p.Valid(); err != nil {
		return err
	}
	if p.Scope != ScopeCharacter {
		return fmt.Errorf("%w: consume character with scope=%s", ErrPolicyInvalid, p.Scope)
	}
	if count == 0 {
		count = 1
	}
	state := ReadCharacter(stats, p, now)
	if uint64(state.Used)+uint64(count) > uint64(p.Limit) {
		return &LimitError{Policy: p, State: state, Requested: count}
	}
	if stats == nil {
		return fmt.Errorf("%w: character stats map is nil", ErrPolicyInvalid)
	}
	stats[CountKey(p)] = int64(state.Used) + int64(count)
	if window := p.Window(now); !window.Start.IsZero() {
		stats[WindowKey(p)] = windowStartUnix(window)
	}
	return nil
}

// ConsumeAccount 在账号容器里消费 count 次；超出限购返回 *LimitError。
func ConsumeAccount(metadata map[string]string, p Policy, count uint32, now time.Time) error {
	if err := p.Valid(); err != nil {
		return err
	}
	if p.Scope != ScopeAccount {
		return fmt.Errorf("%w: consume account with scope=%s", ErrPolicyInvalid, p.Scope)
	}
	if count == 0 {
		count = 1
	}
	state := ReadAccount(metadata, p, now)
	if uint64(state.Used)+uint64(count) > uint64(p.Limit) {
		return &LimitError{Policy: p, State: state, Requested: count}
	}
	if metadata == nil {
		return fmt.Errorf("%w: account metadata map is nil", ErrPolicyInvalid)
	}
	metadata[CountKey(p)] = strconv.FormatInt(int64(state.Used)+int64(count), 10)
	if window := p.Window(now); !window.Start.IsZero() {
		metadata[WindowKey(p)] = strconv.FormatInt(windowStartUnix(window), 10)
	}
	return nil
}

// Consume 按策略作用域选择容器消费。
func Consume(characterStats map[string]int64, accountMetadata map[string]string, p Policy, count uint32, now time.Time) error {
	if p.Scope == ScopeAccount {
		return ConsumeAccount(accountMetadata, p, count, now)
	}
	return ConsumeCharacter(characterStats, p, count, now)
}

// ParseCountKey 解析一条计数键，返回道具与周期。
//
// 带 KeyTag 命名空间的键在这里解析失败（道具段不是纯数字）：这是**刻意**的——
// UsedCounts 只服务 NPC 商店的 op715“4 张表”，商城商品维度的计数不能混进那 4 张
// 表里，否则那个窗口会把商品号当道具号显示。
func ParseCountKey(key string) (uint32, Period, bool) {
	rest, ok := strings.CutPrefix(strings.TrimSpace(key), countKeyPrefix)
	if !ok {
		return 0, 0, false
	}
	index := strings.LastIndex(rest, "_")
	if index <= 0 || index+1 >= len(rest) {
		return 0, 0, false
	}
	itemID, err := strconv.ParseUint(rest[:index], 10, 32)
	if err != nil {
		return 0, 0, false
	}
	period, ok := ParsePeriod(rest[index+1:])
	if !ok {
		return 0, 0, false
	}
	return uint32(itemID), period, true
}

// Entry 是容器里的一条限购计数投影。
type Entry struct {
	ItemID uint32
	Scope  Scope
	Period Period
	Used   uint32
}

// UsedCounts 枚举容器里所有限购计数，并按窗口判定过期（过期的按 0 处理，跳过）。
// 用于 op715 回包：把权威的已购次数下发给客户端。
func UsedCounts(characterStats map[string]int64, accountMetadata map[string]string, now time.Time) []Entry {
	entries := make([]Entry, 0, len(characterStats)+len(accountMetadata))
	for key, value := range characterStats {
		itemID, period, ok := ParseCountKey(key)
		if !ok || value <= 0 {
			continue
		}
		policy := Policy{ItemID: itemID, Scope: ScopeCharacter, Period: period, Limit: ^uint32(0)}
		if used := ReadCharacter(characterStats, policy, now).Used; used > 0 {
			entries = append(entries, Entry{ItemID: itemID, Scope: ScopeCharacter, Period: period, Used: used})
		}
	}
	for key, raw := range accountMetadata {
		itemID, period, ok := ParseCountKey(key)
		if !ok {
			continue
		}
		value, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
		if err != nil || value <= 0 {
			continue
		}
		policy := Policy{ItemID: itemID, Scope: ScopeAccount, Period: period, Limit: ^uint32(0)}
		if used := ReadAccount(accountMetadata, policy, now).Used; used > 0 {
			entries = append(entries, Entry{ItemID: itemID, Scope: ScopeAccount, Period: period, Used: used})
		}
	}
	sort.Slice(entries, func(left, right int) bool {
		if entries[left].Scope != entries[right].Scope {
			return entries[left].Scope < entries[right].Scope
		}
		if entries[left].Period != entries[right].Period {
			return entries[left].Period < entries[right].Period
		}
		return entries[left].ItemID < entries[right].ItemID
	})
	return entries
}
