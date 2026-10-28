package main

// 定时公告（E）的规则测试：配置映射、校验、以及三种方式在通用调度引擎下的判定。
//
// 这块写错的后果很具体：
//   - 一次性公告到点后重复发送 → 玩家看到同一条公告刷屏；
//   - 每天/每周的周期键算错 → 重启运维工具后又发一遍；
//   - 校验放过坏配置 → 运维工具每次 tick 都判定失败，公告永远不发出。

import (
	"strings"
	"testing"
	"time"
)

func announceTestItem(mode, clock, date string, weekday int) ScheduledAnnouncement {
	return ScheduledAnnouncement{
		ID:      "ann-1",
		Content: "今晚 8 点全服双倍经验",
		Color:   announceDefaultColor,
		Enabled: true,
		Mode:    mode,
		Time:    clock,
		Date:    date,
		Weekday: weekday,
	}
}

func TestAnnounceSpecOf(t *testing.T) {
	cases := []struct {
		mode string
		want string
	}{
		{"daily", scheduleModeDaily},
		{"DAILY", scheduleModeDaily},   // 大小写容错
		{" daily ", scheduleModeDaily}, // 前后空格容错
		{"once", scheduleModeOnce},
		{"weekly", scheduleModeWeekly},
		{"", scheduleModeDaily},        // 空=每天
		{"monthly", scheduleModeDaily}, // 不支持的方式回落到每天（管理工具不提供）
	}
	for _, tc := range cases {
		item := announceTestItem(tc.mode, "20:00", "2026-10-01", 3)
		if got := announceSpecOf(item).Mode; got != tc.want {
			t.Fatalf("announceSpecOf(mode=%q).Mode = %q，期望 %q", tc.mode, got, tc.want)
		}
	}
}

func TestValidateAnnounceItem(t *testing.T) {
	cases := []struct {
		name    string
		item    ScheduledAnnouncement
		wantErr string
	}{
		{"每天-正常", announceTestItem("daily", "20:00", "", 0), ""},
		{"一次性-正常", announceTestItem("once", "09:30", "2026-10-01", 0), ""},
		{"每周-正常", announceTestItem("weekly", "20:00", "", 7), ""},
		{"内容为空", announceTestItem("daily", "20:00", "", 0), "内容不能为空"},
		{"内容超长", announceTestItem("daily", "20:00", "", 0), "超过"},
		{"时间非法", announceTestItem("daily", "25:00", "", 0), "HH:MM"},
		{"时间缺冒号", announceTestItem("daily", "2000", "", 0), "HH:MM"},
		{"一次性缺日期", announceTestItem("once", "20:00", "", 0), "YYYY-MM-DD"},
		{"一次性日期非法", announceTestItem("once", "20:00", "2026-13-40", 0), "YYYY-MM-DD"},
		{"每周缺星期", announceTestItem("weekly", "20:00", "", 0), "星期"},
		{"每周星期越界", announceTestItem("weekly", "20:00", "", 8), "星期"},
	}
	for _, tc := range cases {
		item := tc.item
		switch tc.name {
		case "内容为空":
			item.Content = "   "
		case "内容超长":
			item.Content = strings.Repeat("公", 101)
		}
		err := validateAnnounceItem(item)
		if tc.wantErr == "" {
			if err != nil {
				t.Fatalf("%s：期望通过，实际 %v", tc.name, err)
			}
			continue
		}
		if err == nil {
			t.Fatalf("%s：期望报错（含 %q），实际通过", tc.name, tc.wantErr)
		}
		if !strings.Contains(err.Error(), tc.wantErr) {
			t.Fatalf("%s：错误信息 %q 不含 %q", tc.name, err.Error(), tc.wantErr)
		}
	}
}

// TestAnnounceOnceFiresOnlyOnce 一次性公告：到点在窗口内发一次，之后永不重复。
func TestAnnounceOnceFiresOnlyOnce(t *testing.T) {
	item := announceTestItem("once", "20:00", "2026-10-01", 0)
	spec := announceSpecOf(item)

	// 到点后 30 秒（tick 周期内）→ 该发。
	due := time.Date(2026, 10, 1, 20, 0, 0, 0, time.Local)
	dec := scheduleDecide(spec, due.Add(30*time.Second), "", announceScheduleTolerance)
	if !dec.Run {
		t.Fatalf("到点 30 秒应执行，实际 %+v", dec)
	}
	if dec.Period != "2026-10-01" {
		t.Fatalf("一次性周期键应为日期本身，实际 %q", dec.Period)
	}

	// 发完把周期标记为已处理 → 之后的 tick 不再发，也不再刷日志。
	for _, after := range []time.Duration{time.Minute, time.Hour, 24 * time.Hour, 30 * 24 * time.Hour} {
		dec = scheduleDecide(spec, due.Add(after), dec.Period, announceScheduleTolerance)
		if dec.Run {
			t.Fatalf("一次性公告在 %s 后不应再发", after)
		}
	}

	// 工具没开导致错过窗口 → 标记但不补发。
	dec = scheduleDecide(spec, due.Add(2*time.Hour), "", announceScheduleTolerance)
	if dec.Run || !dec.Mark {
		t.Fatalf("错过窗口应只标记不补发，实际 %+v", dec)
	}
}

// TestAnnounceDailyFiresOncePerDay 每天公告：同一天只发一次，跨天重新可发。
func TestAnnounceDailyFiresOncePerDay(t *testing.T) {
	item := announceTestItem("daily", "20:00", "", 0)
	spec := announceSpecOf(item)
	day1 := time.Date(2026, 9, 24, 19, 59, 0, 0, time.Local)

	// 还没到点 → 不发、不标记。
	if dec := scheduleDecide(spec, day1, "", announceScheduleTolerance); dec.Run || dec.Mark {
		t.Fatalf("未到点不应执行/标记，实际 %+v", dec)
	}
	// 到点 → 发。
	dec := scheduleDecide(spec, day1.Add(time.Minute), "", announceScheduleTolerance)
	if !dec.Run {
		t.Fatalf("到点应执行，实际 %+v", dec)
	}
	if dec.Period != "2026-09-24" {
		t.Fatalf("每天周期键应为 YYYY-MM-DD，实际 %q", dec.Period)
	}
	// 同一天再扫多次（含重启后）→ 都不再发。
	for i := 0; i < 5; i++ {
		if again := scheduleDecide(spec, day1.Add(time.Duration(i+2)*time.Minute), dec.Period, announceScheduleTolerance); again.Run {
			t.Fatalf("同一天第 %d 次扫描不应再发", i+2)
		}
	}
	// 第二天到点 → 重新可发（周期键变了）。
	next := time.Date(2026, 9, 25, 20, 0, 10, 0, time.Local)
	if d := scheduleDecide(spec, next, dec.Period, announceScheduleTolerance); !d.Run {
		t.Fatalf("第二天到点应执行，实际 %+v", d)
	}
}

// TestAnnounceWeeklyFiresOnWeekday 每周公告：只在指定星期几发，一周一次。
func TestAnnounceWeeklyFiresOnWeekday(t *testing.T) {
	// 2026-09-24 是周四；配置"每周四 20:00"（weekday=4）。
	item := announceTestItem("weekly", "20:00", "", 4)
	spec := announceSpecOf(item)

	thu := time.Date(2026, 9, 24, 20, 0, 5, 0, time.Local) // 周四到点
	if thu.Weekday() != time.Thursday {
		t.Fatalf("测试基准日期算错了：%s 是 %s", thu.Format("2006-01-02"), thu.Weekday())
	}
	dec := scheduleDecide(spec, thu, "", announceScheduleTolerance)
	if !dec.Run {
		t.Fatalf("周四到点应执行，实际 %+v", dec)
	}
	if dec.Period == "" || !strings.Contains(dec.Period, "-W") {
		t.Fatalf("每周周期键应为 ISO 周，实际 %q", dec.Period)
	}

	// 周五、周六都不到点（下一个周四还没来）→ 不发。
	fri := thu.AddDate(0, 0, 1)
	if d := scheduleDecide(spec, fri, dec.Period, announceScheduleTolerance); d.Run {
		t.Fatalf("周五不应执行，实际 %+v", d)
	}
	// 下周四 → 新周期，可发。
	nextThu := thu.AddDate(0, 0, 7)
	if d := scheduleDecide(spec, nextThu, dec.Period, announceScheduleTolerance); !d.Run {
		t.Fatalf("下周四应执行（周期键已变），实际 %+v", d)
	}
	// 本周内重复扫描 → 不再发。
	if d := scheduleDecide(spec, thu.Add(3*time.Minute), dec.Period, announceScheduleTolerance); d.Run {
		t.Fatalf("同一周内不应重复执行，实际 %+v", d)
	}
}

// TestAnnounceNextRunDisplay 界面上「下次发送」的取值规则。
func TestAnnounceNextRunDisplay(t *testing.T) {
	// 每天 20:00，现在 19:00 → 下一次是今天 20:00。
	daily := announceSpecOf(announceTestItem("daily", "20:00", "", 0))
	now := time.Date(2026, 9, 24, 19, 0, 0, 0, time.Local)
	next, ok := scheduleNextAfter(daily, now)
	if !ok || next.Hour() != 20 || next.Day() != 24 {
		t.Fatalf("每天公告的下一次应为今天 20:00，实际 %v ok=%v", next, ok)
	}
	// 已过点（工具关着）→ 下一次是明天 20:00。
	next, ok = scheduleNextAfter(daily, time.Date(2026, 9, 24, 21, 0, 0, 0, time.Local))
	if !ok || next.Day() != 25 {
		t.Fatalf("已过点后下一次应为明天 20:00，实际 %v ok=%v", next, ok)
	}
	// 一次性已过点 → 没有下一次（界面显示「不再发送」）。
	once := announceSpecOf(announceTestItem("once", "20:00", "2026-09-01", 0))
	if _, ok := scheduleNextAfter(once, now); ok {
		t.Fatal("一次性公告已过点不应有下一次")
	}
	// 一次性未到点 → 有下一次。
	onceFuture := announceSpecOf(announceTestItem("once", "20:00", "2026-10-01", 0))
	if next, ok := scheduleNextAfter(onceFuture, now); !ok || next.Day() != 1 || next.Month() != time.October {
		t.Fatalf("未到点的一次性公告应有下一次（2026-10-01），实际 %v ok=%v", next, ok)
	}
}
