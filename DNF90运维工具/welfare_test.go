package main

// 在线福利调度规则的纯函数测试。
//
// 这块是"到点自动发"的心脏，规则写错的后果都很具体：
//   - 窗口判定写错 → 要么每次 tick 都重发（玩家收到一堆重复邮件），
//     要么永远不发（运营以为功能坏了）；
//   - 周期键写错 → 当天发过之后重启运维工具又发一遍；
//   - 错过判定写错 → 维护完一开机突然补发一堆（需求明确要求**不补发**）。

import (
	"strings"
	"testing"
	"time"
)

func welfareTestSlot(timeText string, rewards ...WelfareReward) WelfareSlot {
	return WelfareSlot{Enabled: true, Time: timeText, Rewards: rewards, Notice: "在线奖励已发放"}
}

func welfareItemReward() WelfareReward {
	return WelfareReward{Kind: welfareKindItem, ItemID: 1001, Amount: 5}
}

func TestWelfareParseClock(t *testing.T) {
	cases := []struct {
		in     string
		h, m   int
		wantOK bool
	}{
		{"20:00", 20, 0, true},
		{" 09:05 ", 9, 5, true},
		{"0:00", 0, 0, true},
		{"23:59", 23, 59, true},
		{"24:00", 0, 0, false},
		{"20:60", 0, 0, false},
		{"20", 0, 0, false},
		{"20:0x", 0, 0, false},
		{"abc", 0, 0, false},
		{"", 0, 0, false},
	}
	for _, tc := range cases {
		h, m, ok := welfareParseClock(tc.in)
		if ok != tc.wantOK {
			t.Fatalf("welfareParseClock(%q) ok = %v，期望 %v", tc.in, ok, tc.wantOK)
		}
		if ok && (h != tc.h || m != tc.m) {
			t.Fatalf("welfareParseClock(%q) = %d:%d，期望 %d:%d", tc.in, h, m, tc.h, tc.m)
		}
	}
}

func TestWelfarePeriodKey(t *testing.T) {
	// 同一天（不同时刻）→ 同一个 daily 键；相邻两天 → 不同键。
	d1 := time.Date(2026, 9, 24, 0, 5, 0, 0, time.Local)
	d2 := time.Date(2026, 9, 24, 23, 55, 0, 0, time.Local)
	d3 := time.Date(2026, 9, 25, 0, 5, 0, 0, time.Local)
	if welfarePeriodKey(welfareDaily, d1) != welfarePeriodKey(welfareDaily, d2) {
		t.Fatal("同一天的 daily 周期键应相同")
	}
	if welfarePeriodKey(welfareDaily, d1) == welfarePeriodKey(welfareDaily, d3) {
		t.Fatal("相邻两天的 daily 周期键应不同")
	}
	if got := welfarePeriodKey(welfareDaily, d1); got != "2026-09-24" {
		t.Fatalf("daily 周期键 = %q，期望 2026-09-24", got)
	}
	if got := welfarePeriodKey(welfareMonthly, d1); got != "2026-09" {
		t.Fatalf("monthly 周期键 = %q，期望 2026-09", got)
	}
	// 周键：同 ISO 周内任一天相同，且格式为 YYYY-Www。
	weekKey := welfarePeriodKey(welfareWeekly, d1)
	if !strings.Contains(weekKey, "-W") {
		t.Fatalf("weekly 周期键格式异常: %q", weekKey)
	}
	// d1(周四) 所在周的周一与周日必须同键。
	monday := d1.AddDate(0, 0, -int((d1.Weekday()+6)%7))
	sunday := monday.AddDate(0, 0, 6)
	if welfarePeriodKey(welfareWeekly, monday) != welfarePeriodKey(welfareWeekly, sunday) {
		t.Fatalf("同一 ISO 周的周一(%s)与周日(%s)周期键应相同",
			monday.Format("2006-01-02"), sunday.Format("2006-01-02"))
	}
}

func TestWelfareDecideDaily(t *testing.T) {
	slot := welfareTestSlot("20:00", welfareItemReward())
	day := func(h, m int) time.Time { return time.Date(2026, 9, 24, h, m, 0, 0, time.Local) }

	cases := []struct {
		name     string
		now      time.Time
		lastRun  string
		wantRun  bool
		wantMark bool
	}{
		{"19:30 还没到点", day(19, 30), "", false, false},
		{"20:00 准点发", day(20, 0), "", true, false},
		{"20:03 在容忍窗口内（补一次 tick 的抖动）", day(20, 3), "", true, false},
		{"20:06 超过容忍窗口 → 不补发但要标记", day(20, 6), "", false, true},
		{"当天已处理过 → 不再发", day(20, 5), "2026-09-24", false, true},
		{"昨天处理过、今天到点 → 照发", day(20, 1), "2026-09-23", true, false},
	}
	for _, tc := range cases {
		dec := welfareDecide(welfareDaily, slot, tc.now, tc.lastRun, welfareTolerance)
		if dec.Run != tc.wantRun || dec.Mark != tc.wantMark {
			t.Fatalf("%s: Run=%v Mark=%v（期望 %v/%v）原因=%q",
				tc.name, dec.Run, dec.Mark, tc.wantRun, tc.wantMark, dec.Reason)
		}
		if dec.Period != "2026-09-24" {
			t.Fatalf("%s: 周期键 = %q", tc.name, dec.Period)
		}
	}
}

func TestWelfareDecideDisabledOrInvalid(t *testing.T) {
	now := time.Date(2026, 9, 24, 20, 0, 0, 0, time.Local)

	// 未启用
	slot := welfareTestSlot("20:00", welfareItemReward())
	slot.Enabled = false
	if dec := welfareDecide(welfareDaily, slot, now, "", welfareTolerance); dec.Run || dec.Mark {
		t.Fatalf("未启用不应发也不应标记: %+v", dec)
	}
	// 没有奖励
	empty := WelfareSlot{Enabled: true, Time: "20:00"}
	if dec := welfareDecide(welfareDaily, empty, now, "", welfareTolerance); dec.Run || dec.Mark {
		t.Fatalf("没有奖励不应发: %+v", dec)
	}
	// 时间非法
	bad := welfareTestSlot("25:99", welfareItemReward())
	if dec := welfareDecide(welfareDaily, bad, now, "", welfareTolerance); dec.Run || dec.Mark {
		t.Fatalf("时间非法不应发: %+v", dec)
	}
}

func TestWelfareDecideWeekly(t *testing.T) {
	// 2026-09-24 是周四（Go: Weekday=4）；配置"每周三 20:00"。
	thursday := time.Date(2026, 9, 24, 20, 0, 0, 0, time.Local)
	slot := welfareTestSlot("20:00", welfareItemReward())
	slot.Weekday = 3
	if thursday.Weekday() != time.Thursday {
		t.Fatalf("测试基准日不是周四：%s", thursday.Weekday())
	}
	// 周四 20:00 看"本周三 20:00"：已过去一天多 → 超过容忍窗口 → 不补发。
	dec := welfareDecide(welfareWeekly, slot, thursday, "", welfareTolerance)
	if dec.Run || !dec.Mark {
		t.Fatalf("错过本周三应当不补发且标记: %+v", dec)
	}
	if !strings.Contains(dec.Reason, "不补发") {
		t.Fatalf("判定原因应说明不补发：%q", dec.Reason)
	}

	// 周三 20:00 准点：应发。
	wednesday := time.Date(2026, 9, 23, 20, 0, 0, 0, time.Local)
	if wednesday.Weekday() != time.Wednesday {
		t.Fatalf("基准日不是周三：%s", wednesday.Weekday())
	}
	if dec := welfareDecide(welfareWeekly, slot, wednesday, "", welfareTolerance); !dec.Run {
		t.Fatalf("周三准点应当发: %+v", dec)
	}
	// 周日 12:00：还没到"下周三"→ 不发也不标记。
	sunday := time.Date(2026, 9, 27, 12, 0, 0, 0, time.Local)
	if dec := welfareDecide(welfareWeekly, slot, sunday, "", welfareTolerance); dec.Run || dec.Mark {
		t.Fatalf("本周三已过、下周三未到：既不该发也不该标记: %+v", dec)
	}
}

func TestWelfareDecideMonthly(t *testing.T) {
	slot := welfareTestSlot("10:00", welfareItemReward())
	slot.Day = 10

	// 10 号 10:00 准点发
	if dec := welfareDecide(welfareMonthly, slot, time.Date(2026, 9, 10, 10, 0, 0, 0, time.Local), "", welfareTolerance); !dec.Run {
		t.Fatalf("10 号准点应当发: %+v", dec)
	}
	// 3 号：还没到 → 不发不标记
	if dec := welfareDecide(welfareMonthly, slot, time.Date(2026, 9, 3, 10, 0, 0, 0, time.Local), "", welfareTolerance); dec.Run || dec.Mark {
		t.Fatalf("3 号不该发: %+v", dec)
	}
	// 20 号：早过了 → 不补发但标记
	dec := welfareDecide(welfareMonthly, slot, time.Date(2026, 9, 20, 10, 0, 0, 0, time.Local), "", welfareTolerance)
	if dec.Run || !dec.Mark {
		t.Fatalf("20 号应标记为已错过: %+v", dec)
	}
	// 日期越界（31 号）→ 配置非法，不发不标记
	bad := welfareTestSlot("10:00", welfareItemReward())
	bad.Day = 31
	if dec := welfareDecide(welfareMonthly, bad, time.Date(2026, 9, 30, 10, 0, 0, 0, time.Local), "", welfareTolerance); dec.Run || dec.Mark {
		t.Fatalf("日期越界不应发: %+v", dec)
	}
}

func TestWelfareValidateReward(t *testing.T) {
	ok := []WelfareReward{
		{Kind: welfareKindItem, ItemID: 1001, Amount: 5},
		{Kind: welfareKindItem, ItemID: 1001}, // 数量留空按 1
		{Kind: welfareKindCera, Amount: 5000},
		{Kind: welfareKindBlackDiamond, Amount: 30},
	}
	for _, r := range ok {
		got, err := welfareValidateReward(r)
		if err != nil {
			t.Fatalf("应当通过: %+v err=%v", r, err)
		}
		if got.Kind == welfareKindItem && got.Amount == 0 {
			t.Fatalf("道具数量应默认为 1: %+v", got)
		}
	}
	bad := []WelfareReward{
		{Kind: welfareKindItem},                        // 缺 item_id
		{Kind: welfareKindCera},                        // 点券数量 0
		{Kind: welfareKindCera, Amount: 100_000_001},   // 超上限
		{Kind: welfareKindBlackDiamond, Amount: 3651},  // 黑钻超上限
		{Kind: "gold", Amount: 100},                    // 未知类型
		{Kind: welfareKindItem, ItemID: 1, ReinforceLevel: -1}, // 负数
	}
	for _, r := range bad {
		if _, err := welfareValidateReward(r); err == nil {
			t.Fatalf("应当被拒绝: %+v", r)
		}
	}
}
