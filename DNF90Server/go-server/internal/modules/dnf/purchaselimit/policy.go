// 本文件定义 PVF `[purchase limit]` / `[daily purchase limit]` 的限购策略、
// 服务窗口与作用域/周期枚举。
//
// PVF 现场（全库 Script.pvf 扫描：420 个文件、41 种组合）：
//
//	[purchase limit]
//	  `account` `weekly` 10        ← 新格式：作用域 周期 次数
//
// 旧格式是另一套字段，只有作用域+次数，周期固定为“每天”：
//
//	[daily purchase limit]
//	  `character` 1
//
// 作用域 2 种（account/character）× 周期 4 种（accumulate/weekly/daily/
// agitwarseason）。客户端 UI 上的“账号永久/账号每周/角色永久/角色每周”4 行是
// 按 (作用域, 是否周期) 分组的展示，不是 PVF 的取值。
package purchaselimit

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

var (
	ErrPolicyInvalid = errors.New("dnf purchase limit policy is invalid")
	ErrLimitReached  = errors.New("dnf purchase limit reached")
)

// Scope 是限购计数的持久化归属者。account 维度的计数落在账号记录上，character
// 维度落在角色记录上；两者可以同时存在于同一件道具（PVF 只写一条，所以实际
// 只会命中一条）。
type Scope byte

const (
	ScopeCharacter Scope = iota + 1
	ScopeAccount
)

func (s Scope) String() string {
	switch s {
	case ScopeCharacter:
		return "character"
	case ScopeAccount:
		return "account"
	default:
		return "unknown"
	}
}

// Period 是一条计数使用的服务时间窗口。
type Period byte

const (
	// PeriodAccumulate 是 PVF 的 `accumulate`：永不清零（永久限购）。
	PeriodAccumulate Period = iota + 1
	// PeriodDaily 是 PVF 的 `daily` 与旧字段 `[daily purchase limit]`。
	PeriodDaily
	// PeriodWeekly 是 PVF 的 `weekly`。
	PeriodWeekly
	// PeriodSeason 是 PVF 的 `agitwarseason`（公会争夺战赛季）。
	PeriodSeason
)

func (p Period) String() string {
	switch p {
	case PeriodAccumulate:
		return "accumulate"
	case PeriodDaily:
		return "daily"
	case PeriodWeekly:
		return "weekly"
	case PeriodSeason:
		return "season"
	default:
		return "unknown"
	}
}

// Periodic 报告该周期是否会按窗口重置。
func (p Period) Periodic() bool {
	return p == PeriodDaily || p == PeriodWeekly || p == PeriodSeason
}

// Policy 是一件道具的限购策略。
type Policy struct {
	ItemID uint32
	Scope  Scope
	Period Period
	Limit  uint32
	// Section 记录来源字段，便于日志区分新字段与旧字段。
	Section string
	// KeyTag 是计数键里的命名空间。道具维度（PVF `[purchase limit]`）留空；商城
	// 商品维度必须带前缀，因为商城是按**商品号**限购的，而商品号与道具号共用同
	// 一个数值空间（例如商品 104412 与某件道具 104412 会撞同一条计数）。
	KeyTag string
}

func (p Policy) Valid() error {
	if p.ItemID == 0 || p.Limit == 0 ||
		(p.Scope != ScopeCharacter && p.Scope != ScopeAccount) ||
		(p.Period != PeriodAccumulate && p.Period != PeriodDaily && p.Period != PeriodWeekly && p.Period != PeriodSeason) {
		return fmt.Errorf("%w: item=%d scope=%d period=%d limit=%d", ErrPolicyInvalid, p.ItemID, p.Scope, p.Period, p.Limit)
	}
	return nil
}

// ParseScope 解析 PVF 的作用域文本（反引号内的内容）。
//
// 真实 PVF 里除标准写法外还有作者笔误，全库扫描（2026-09-14）实测到的：
//   - `charac`      截断，出现在 40+ 个文件（如 100060533.equ、chn_490003157.stk）
//   - `chraracter`  错拼，出现在 chn_490002849.stk
//
// 这两种只可能是 character（另一取值 account 无法由它们拼出），因此按 character
// 接受。其它取值（例如 `trade`，见 chn_thief_chnagejob_10006348.stk）语义不明，
// 一律判为“不认识的字段”→ 不限购，绝不猜。
func ParseScope(raw string) (Scope, bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "account":
		return ScopeAccount, true
	case "character", "charac", "chraracter":
		return ScopeCharacter, true
	default:
		return 0, false
	}
}

// ParsePeriod 解析 PVF 的周期文本。
//
// 必须同时接受 Period.String() 的输出（"season"）与 PVF 的原始写法
// （"agitwarseason"）：计数键是用 Period.String() 拼的，ParseCountKey 再把它读回来。
// 只认 PVF 写法会让赛季计数键读不回来（op715 下发时被判成"没有这条计数"）。
func ParsePeriod(raw string) (Period, bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "accumulate":
		return PeriodAccumulate, true
	case "daily":
		return PeriodDaily, true
	case "weekly":
		return PeriodWeekly, true
	case "agitwarseason", "season":
		return PeriodSeason, true
	default:
		return 0, false
	}
}

// serviceCalendar 与 dungeonadmission 使用同一个北京服务时区：DNF 的服务日从
// 06:00 开始，周重置同样落在 06:00。
var serviceCalendar = time.FixedZone("Asia/Shanghai", 8*60*60)

// DefaultWeeklyResetWeekday 是商店限购的周重置日。DNF 的周重置统一在周四
// 06:00（与 raid 周重置一致）。PVF 不声明商店限购的重置日，所以这里是本服
// 择定的常量，改这里即可整体调整。
const DefaultWeeklyResetWeekday = time.Thursday

// Window 是一条计数当前所属的服务时间区间。Accumulate/Season 这类不按窗口
// 重置的策略返回零值窗口。
type Window struct {
	Start time.Time
	End   time.Time
}

// Window 计算 now 落在的服务窗口。零值 now 取当前时间。
func (p Policy) Window(now time.Time) Window {
	if !p.Period.Periodic() {
		return Window{}
	}
	if now.IsZero() {
		now = time.Now()
	}
	local := now.In(serviceCalendar)
	serviceDate := local.Add(-6 * time.Hour)
	start := time.Date(serviceDate.Year(), serviceDate.Month(), serviceDate.Day(), 6, 0, 0, 0, serviceCalendar)
	switch p.Period {
	case PeriodDaily:
		return Window{Start: start, End: start.AddDate(0, 0, 1)}
	case PeriodWeekly:
		days := (int(serviceDate.Weekday()) - int(DefaultWeeklyResetWeekday) + 7) % 7
		start = start.AddDate(0, 0, -days)
		return Window{Start: start, End: start.AddDate(0, 0, 7)}
	case PeriodSeason:
		// 本服没有区域占领战赛季日历，赛季窗口按“永不清零”处理（见
		// PeriodSeason 的说明）；这里返回零值窗口，计数不会因为换周被清掉。
		return Window{}
	default:
		return Window{}
	}
}
