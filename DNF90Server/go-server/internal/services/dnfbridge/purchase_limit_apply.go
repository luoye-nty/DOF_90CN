// 本文件把 PVF 限购接进购买事务（NPC 商店与商场共用）。
//
// 计数归属：
//   - `character` → 角色记录 Stats
//   - `account`   → 账号记录 Metadata
//
// 两者都在购买事务内读写，超限时返回错误让整笔回滚（金币/点券、材料、道具、
// 计数一起失败），提交时计数与发货同时生效。
package dnfbridge

import (
	"errors"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"longheng.io/server/internal/modules/dnf/purchaselimit"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

var (
	// errCurrentPurchaseLimitReached：PVF `[purchase limit]` 上限已满。
	errCurrentPurchaseLimitReached = errors.New("dnf current purchase limit reached")
	// errCurrentPurchaseLimitUnavailable：限购状态容器缺失（角色记录或账号记录
	// 没拿到），此时不能安全地记账。
	errCurrentPurchaseLimitUnavailable = errors.New("dnf current purchase limit state is unavailable")
)

// currentPurchaseLimitErrorCode 是超限时回给客户端的失败码。
//
// 逆向实证（2026-09-14）：客户端文案「超过购买限制」的字符串 id 是 44465（0xADB1）。
// 全 EXE 只有 3 处 `push 44465`，其中一处（rva 0x0192842F）位于 op21 购买回包处理器
// `sub_1D27590`（入口 0x01927590，就是本服日志里 ack_body=current_exe_sub_1D27590
// 指的那个函数）的失败分支，形如 `cmp bl, 0x85 / push 0 / push 44465`。即客户端按
// 失败包第二个字节查文案表：0x85 → 超过购买限制。同表其它取值实测：
// 0x0A/0x24/0xC8 → 所持金额不足(747)、0xCB → 物品保管箱已满(72839)、
// 0xD9 → 现在不是出售时间(44466)、0x15 → 当前状态无法购买活动商品(46057)、
// 0x16 → 不符合购买条件(70743)、0x01 → 被禁闭在赛丽亚旅馆(70352)。
const currentPurchaseLimitErrorCode = 0x85

// currentPurchaseLimitPolicy 取出道具的限购策略；无字段或字段畸形时返回 false
// （畸形字段在解析期已记录，购买链路上按“不限购”处理以免锁死一件商品）。
func currentPurchaseLimitPolicy(definition dungeonDropItemDefinition) (purchaselimit.Policy, bool) {
	if !definition.PurchaseLimitFound {
		return purchaselimit.Policy{}, false
	}
	if err := definition.PurchaseLimit.Valid(); err != nil {
		return purchaselimit.Policy{}, false
	}
	return definition.PurchaseLimit, true
}

// currentPurchaseLimitClockOffset 是限购窗口的“时间旅行”偏移，只用于验收“到期刷新”：
// 环境变量 DNF90_PURCHASE_LIMIT_CLOCK_OFFSET_HOURS 非 0 时，限购窗口判定用的时间整体平移
// 该小时数（正数=往未来）。例：+200 小时会跨过周四 06:00 的周重置边界，于是本周窗口
// 变成下周窗口、存量计数被判过期而归零——和真正到期时的代码路径完全一致。
//
// 只影响限购窗口判定，不影响疲劳、掉落、任务等其它时间逻辑；设置后每次 op715 请求的
// 日志都会带上 clock_offset_hours 字段，避免忘了关掉还看不出来。
var (
	currentPurchaseLimitClockOffsetOnce  sync.Once
	currentPurchaseLimitClockOffsetValue time.Duration
)

func currentPurchaseLimitClockOffset() time.Duration {
	currentPurchaseLimitClockOffsetOnce.Do(func() {
		raw := strings.TrimSpace(os.Getenv(currentPurchaseLimitClockOffsetEnv))
		if raw == "" {
			return
		}
		hours, err := strconv.ParseFloat(raw, 64)
		if err != nil || hours == 0 {
			return
		}
		currentPurchaseLimitClockOffsetValue = time.Duration(hours * float64(time.Hour))
	})
	return currentPurchaseLimitClockOffsetValue
}

const currentPurchaseLimitClockOffsetEnv = "DNF90_PURCHASE_LIMIT_CLOCK_OFFSET_HOURS"

// currentPurchaseLimitTime 返回限购窗口判定使用的时间；测试用
// Service.purchaseLimitClock 注入，运维可用上面那个环境变量做时间旅行。
func (s *Service) currentPurchaseLimitTime() time.Time {
	now := time.Now()
	if s != nil && s.purchaseLimitClock != nil {
		now = s.purchaseLimitClock()
	}
	return now.Add(currentPurchaseLimitClockOffset())
}

// readCurrentPurchaseLimit 返回道具的限购策略与当前已购次数。
func readCurrentPurchaseLimit(
	characterStats map[string]int64,
	accountMetadata map[string]string,
	definition dungeonDropItemDefinition,
	now time.Time,
) (purchaselimit.Policy, purchaselimit.State, bool) {
	policy, ok := currentPurchaseLimitPolicy(definition)
	if !ok {
		return purchaselimit.Policy{}, purchaselimit.State{}, false
	}
	return policy, purchaselimit.Read(characterStats, accountMetadata, policy, now), true
}

// consumeCurrentPurchaseLimit 在购买事务里消费 count 次限购，返回被消费的策略
// 与消费前快照（供日志）。无策略时返回零值策略与 nil 错误。
func consumeCurrentPurchaseLimit(
	character *dnfrepo.CharacterRecord,
	account *dnfrepo.AccountRecord,
	definition dungeonDropItemDefinition,
	count uint32,
	now time.Time,
) (purchaselimit.Policy, purchaselimit.State, error) {
	policy, ok := currentPurchaseLimitPolicy(definition)
	if !ok {
		return purchaselimit.Policy{}, purchaselimit.State{}, nil
	}
	return consumeCurrentPurchaseLimitPolicy(character, account, policy, count, now)
}

// consumeCurrentPurchaseLimitPolicy 按**显式策略**消费，容器与回滚语义和
// consumeCurrentPurchaseLimit 完全一致。用于策略不是来自道具 PVF 的场景：
// 商城的 `[buy restrict item]`（按商品号）与道具说明文案里的账号限购。
func consumeCurrentPurchaseLimitPolicy(
	character *dnfrepo.CharacterRecord,
	account *dnfrepo.AccountRecord,
	policy purchaselimit.Policy,
	count uint32,
	now time.Time,
) (purchaselimit.Policy, purchaselimit.State, error) {
	if err := policy.Valid(); err != nil {
		// 策略本身不合法（次数为 0 等）：按“不限购”处理，绝不因为一条畸形数据
		// 锁死一件商品。
		return purchaselimit.Policy{}, purchaselimit.State{}, nil
	}
	if character == nil {
		return policy, purchaselimit.State{}, errCurrentPurchaseLimitUnavailable
	}
	var accountMetadata map[string]string
	if policy.Scope == purchaselimit.ScopeAccount {
		if account == nil {
			return policy, purchaselimit.State{}, errCurrentPurchaseLimitUnavailable
		}
		if account.Metadata == nil {
			account.Metadata = make(map[string]string)
		}
		accountMetadata = account.Metadata
	} else if character.Stats == nil {
		character.Stats = make(map[string]int64)
	}
	state := purchaselimit.Read(character.Stats, accountMetadata, policy, now)
	if err := purchaselimit.Consume(character.Stats, accountMetadata, policy, count, now); err != nil {
		return policy, state, err
	}
	return policy, state, nil
}
