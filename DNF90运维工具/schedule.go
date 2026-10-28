package main

// 定时任务通用引擎：把"到点没到点、要不要标记已处理"这套判定抽出来，
// 定时公告（E）与在线福利（D）共用一份实现 —— 两处各写一套时间逻辑，
// 迟早会出现"福利不补发、公告却补发"这种不一致。
//
// 支持的周期形态：
//
//	once    指定日期 + 时间，发一次（Date=YYYY-MM-DD）
//	daily   每天 HH:MM
//	weekly  每周某天 HH:MM（Weekday 1=周一 … 7=周日）
//	monthly 每月某号 HH:MM（Day 1..28）
//
// "不补发"是共同的约定：超过容忍窗口（工具当时没在运行）就只标记、不事后补发。

import (
	"fmt"
	"strings"
	"time"
)

// 定时任务周期形态。
const (
	scheduleModeOnce    = "once"
	scheduleModeDaily   = "daily"
	scheduleModeWeekly  = "weekly"
	scheduleModeMonthly = "monthly"
)

// scheduleSpec 定时任务的通用描述。
type scheduleSpec struct {
	Mode    string
	Time    string // "HH:MM"
	Date    string // once：YYYY-MM-DD
	Weekday int    // weekly：1..7
	Day     int    // monthly：1..28
}

// scheduleDecision 一次判定结果（纯数据，便于单测）。
type scheduleDecision struct {
	Run    bool      // 现在就该执行
	Mark   bool      // 需要把该周期标记为已处理（错过窗口 / 已执行过）
	DueAt  time.Time // 本周期内的计划时刻
	Period string    // 周期键
	Reason string    // 判定说明（写日志用）
}

// parseClock 解析 "HH:MM"；非法返回 ok=false。
func parseClock(raw string) (hour, minute int, ok bool) {
	parts := strings.Split(strings.TrimSpace(raw), ":")
	if len(parts) != 2 {
		return 0, 0, false
	}
	h, err1 := parseIntStrict(parts[0])
	m, err2 := parseIntStrict(parts[1])
	if err1 != nil || err2 != nil || h < 0 || h > 23 || m < 0 || m > 59 {
		return 0, 0, false
	}
	return h, m, true
}

// parseIntStrict 只接受纯数字（不接受 "+1"、" 1 " 以外的花样，避免 08:0x 之类被解析成功）。
func parseIntStrict(raw string) (int, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, fmt.Errorf("empty")
	}
	n := 0
	for _, r := range raw {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("not a number: %q", raw)
		}
		n = n*10 + int(r-'0')
		if n > 100000 {
			return 0, fmt.Errorf("too large")
		}
	}
	return n, nil
}

// schedulePeriodKey 给定周期形态与时间，返回所属周期键。
// 周用 ISO 周（周一为一周之始），月用 YYYY-MM，一次性用日期本身。
func schedulePeriodKey(spec scheduleSpec, now time.Time) string {
	switch spec.Mode {
	case scheduleModeOnce:
		return strings.TrimSpace(spec.Date)
	case scheduleModeWeekly:
		year, week := now.ISOWeek()
		return fmt.Sprintf("%d-W%02d", year, week)
	case scheduleModeMonthly:
		return now.Format("2006-01")
	default:
		return now.Format("2006-01-02")
	}
}

// scheduleDueAt 计算该任务在 now 所属周期内的计划时刻。
// 第二项为 false 表示这一周期没有计划时刻（配置非法 / 本月没有这一天）。
func scheduleDueAt(spec scheduleSpec, now time.Time) (time.Time, bool) {
	hour, minute, ok := parseClock(spec.Time)
	if !ok {
		return time.Time{}, false
	}
	switch spec.Mode {
	case scheduleModeOnce:
		raw := strings.TrimSpace(spec.Date)
		// 时间按本地时区解析；配置里写的就是运营看到的日期。
		day, err := time.ParseInLocation("2006-01-02", raw, now.Location())
		if err != nil {
			return time.Time{}, false
		}
		return time.Date(day.Year(), day.Month(), day.Day(), hour, minute, 0, 0, now.Location()), true
	case scheduleModeWeekly:
		weekday := spec.Weekday
		if weekday < 1 || weekday > 7 {
			return time.Time{}, false
		}
		// Go 的 Weekday：Sunday=0；这里配置 1=周一…7=周日。
		target := time.Weekday(weekday % 7)
		delta := int(target - now.Weekday())
		day := now.AddDate(0, 0, delta)
		return time.Date(day.Year(), day.Month(), day.Day(), hour, minute, 0, 0, now.Location()), true
	case scheduleModeMonthly:
		if spec.Day < 1 || spec.Day > 28 {
			return time.Time{}, false
		}
		return time.Date(now.Year(), now.Month(), spec.Day, hour, minute, 0, 0, now.Location()), true
	default:
		return time.Date(now.Year(), now.Month(), now.Day(), hour, minute, 0, 0, now.Location()), true
	}
}

// scheduleDecide 判断此刻该不该执行（纯函数）。
//
// 规则（顺序即优先级）：
//  1. 周期描述非法（时间/日期/星期几/号数）→ 不执行、不标记（等用户改配置）；
//  2. 该周期已处理过（lastRun == 本周期键）→ 不执行（标记，便于调用方跳过日志）；
//  3. 计划时刻还没到 → 不执行、不标记；
//  4. 已过计划时刻但在容忍窗口内 → 执行；
//  5. 超过容忍窗口 → 不执行，但**标记为已处理**（约定：错过不补发），原因写清楚。
func scheduleDecide(spec scheduleSpec, now time.Time, lastRun string, tolerance time.Duration) scheduleDecision {
	dec := scheduleDecision{Period: schedulePeriodKey(spec, now)}
	dueAt, ok := scheduleDueAt(spec, now)
	if !ok {
		dec.Reason = "时间/日期配置非法"
		return dec
	}
	dec.DueAt = dueAt

	if period := strings.TrimSpace(dec.Period); period != "" && strings.TrimSpace(lastRun) == period {
		dec.Mark = true
		dec.Reason = "本周期已处理过"
		return dec
	}
	if now.Before(dueAt) {
		dec.Reason = "还没到时间（" + dueAt.Format("01-02 15:04") + "）"
		return dec
	}
	if now.Sub(dueAt) <= tolerance {
		dec.Run = true
		dec.Reason = "到点执行"
		return dec
	}
	dec.Mark = true
	dec.Reason = fmt.Sprintf("已错过（计划 %s，晚 %s 超过容忍窗口 %s），按约定不补发",
		dueAt.Format("01-02 15:04"), now.Sub(dueAt).Truncate(time.Minute), tolerance)
	return dec
}

// scheduleNextAfter 下一个计划时刻（界面上"下次：…"用）。
// 一次性任务若已过期，返回零值（没有下一次）。
func scheduleNextAfter(spec scheduleSpec, now time.Time) (time.Time, bool) {
	due, ok := scheduleDueAt(spec, now)
	if !ok {
		return time.Time{}, false
	}
	if due.After(now) {
		return due, true
	}
	switch spec.Mode {
	case scheduleModeOnce:
		return time.Time{}, false
	case scheduleModeWeekly:
		return due.AddDate(0, 0, 7), true
	case scheduleModeMonthly:
		return due.AddDate(0, 1, 0), true
	default:
		return due.AddDate(0, 0, 1), true
	}
}
