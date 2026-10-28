package main

// 时间下拉的纯逻辑测试（不建窗口）。
//
// 这里钉住的是"下拉索引 ↔ HH:MM"的换算：这块写错的后果是**静默发错时间**
// （比如把 20:00 发成 00:20），服务端照单全收，运营要到点没发才发现有鬼。

import (
	"testing"
)

func TestClockLabels(t *testing.T) {
	if len(timeHourLabels) != 24 {
		t.Fatalf("小时选项数 = %d，期望 24", len(timeHourLabels))
	}
	if len(timeMinuteLabels) != 60 {
		t.Fatalf("分钟选项数 = %d，期望 60", len(timeMinuteLabels))
	}
	if timeHourLabels[0] != "00" || timeHourLabels[23] != "23" {
		t.Fatalf("小时选项首尾 = %q..%q，期望 00..23", timeHourLabels[0], timeHourLabels[23])
	}
	if timeMinuteLabels[0] != "00" || timeMinuteLabels[59] != "59" {
		t.Fatalf("分钟选项首尾 = %q..%q，期望 00..59", timeMinuteLabels[0], timeMinuteLabels[59])
	}
}

func TestClockTextAndIndexRoundTrip(t *testing.T) {
	for _, tc := range []struct {
		hour, minute int
		want         string
	}{
		{0, 0, "00:00"},
		{8, 5, "08:05"},
		{20, 0, "20:00"},
		{23, 59, "23:59"},
	} {
		got := clockText(tc.hour, tc.minute)
		if got != tc.want {
			t.Fatalf("clockText(%d,%d) = %q，期望 %q", tc.hour, tc.minute, got, tc.want)
		}
		hourIdx, minuteIdx, ok := clockIndexFromText(got)
		if !ok || hourIdx != tc.hour || minuteIdx != tc.minute {
			t.Fatalf("clockIndexFromText(%q) = (%d,%d,%v)，期望 (%d,%d,true)",
				got, hourIdx, minuteIdx, ok, tc.hour, tc.minute)
		}
	}
}

func TestClockTextClampsIndex(t *testing.T) {
	// 越界索引（控件未绑定时 CurrentIndex 返回 -1）不能产生非法时间。
	if got := clockText(-1, -1); got != "00:00" {
		t.Fatalf("clockText(-1,-1) = %q，期望 00:00", got)
	}
	if got := clockText(99, 99); got != "00:00" {
		t.Fatalf("clockText(99,99) = %q，期望 00:00", got)
	}
}

func TestClockIndexFromTextTolerance(t *testing.T) {
	cases := []struct {
		in     string
		hour   int
		minute int
		ok     bool
	}{
		{"20:00", 20, 0, true},
		{" 9:05 ", 9, 5, true},
		{"8:5", 8, 5, true},    // 不补零也认
		{"20：00", 20, 0, true}, // 中文冒号
		{"24:00", 0, 0, false}, // 小时越界
		{"20:60", 0, 0, false}, // 分钟越界
		{"2000", 0, 0, false},  // 没有冒号
		{"", 0, 0, false},
		{"abc", 0, 0, false},
	}
	for _, tc := range cases {
		hour, minute, ok := clockIndexFromText(tc.in)
		if ok != tc.ok {
			t.Fatalf("clockIndexFromText(%q) ok = %v，期望 %v", tc.in, ok, tc.ok)
		}
		if ok && (hour != tc.hour || minute != tc.minute) {
			t.Fatalf("clockIndexFromText(%q) = (%d,%d)，期望 (%d,%d)", tc.in, hour, minute, tc.hour, tc.minute)
		}
	}
}
