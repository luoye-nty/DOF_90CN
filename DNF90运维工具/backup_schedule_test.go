package main

// 自动备份调度测试（纯函数，不建窗口、不连库）。
//
// 这里锁住的是一个真实事故：老实现写成
//
//	next := backup.NextRunAt(base, interval, now)  // 定义：第一个严格晚于 now 的时刻
//	if now.Before(next) { return }                 // ← 永远为真
//
// 于是定时备份**从来没触发过**（用户反馈"不会按时自动备份"）。新实现把
// "求到点时刻"和"该不该跑"拆成 dueAutoBackupSlot / decideAutoBackup 两个纯函数，
// 用例直接从这两处下手。

import (
	"testing"
	"time"

	"dnf90ops/internal/backup"
	"dnf90ops/internal/config"
)

// TestNextRunAtIsAlwaysInTheFuture 记录老实现为什么会失效：
// NextRunAt 永远返回未来时刻，用 `now.Before(next)` 当"到点判断"必然永远成立。
func TestNextRunAtIsAlwaysInTheFuture(t *testing.T) {
	base := time.Date(2026, 9, 14, 10, 0, 0, 0, time.Local)
	for _, offset := range []time.Duration{0, time.Minute, 59 * time.Minute, time.Hour, 25 * time.Hour} {
		now := base.Add(offset)
		next := backup.NextRunAt(base, 1, now)
		if !next.After(now) {
			t.Fatalf("NextRunAt(offset=%s) = %s，不再晚于 now", offset, next)
		}
	}
}

func TestDueAutoBackupSlot(t *testing.T) {
	base := time.Date(2026, 9, 14, 10, 0, 0, 0, time.Local)
	ab := config.AutoBackupConfig{Enabled: true, IntervalHours: 1, BaseAt: base.Format(time.RFC3339)}

	cases := []struct {
		name    string
		now     time.Time
		wantDue bool
		wantAt  time.Time
	}{
		{"还没到第一个点", base.Add(59 * time.Minute), false, time.Time{}},
		{"正好到第一个点", base.Add(time.Hour), true, base.Add(time.Hour)},
		{"差一纳秒", base.Add(time.Hour - time.Nanosecond), false, time.Time{}},
		{"过了两个点", base.Add(2*time.Hour + 30*time.Minute), true, base.Add(2 * time.Hour)},
		{"停机很久后仍然按相位补一次", base.Add(50*time.Hour + time.Minute), true, base.Add(50 * time.Hour)},
		{"基准在未来（时钟回拨/换机）不触发", base.Add(-time.Minute), false, time.Time{}},
	}
	for _, tc := range cases {
		slot, due := dueAutoBackupSlot(ab, tc.now)
		if due != tc.wantDue {
			t.Fatalf("%s: due=%v, want %v", tc.name, due, tc.wantDue)
		}
		if due && !slot.Equal(tc.wantAt) {
			t.Fatalf("%s: slot=%s, want %s", tc.name, slot, tc.wantAt)
		}
	}

	// 基准缺失/非法、间隔非法都不应触发（分别由 InitBase 与 idle 兜底）。
	bad := config.AutoBackupConfig{Enabled: true, IntervalHours: 1, BaseAt: "not-a-time"}
	if _, due := dueAutoBackupSlot(bad, time.Now()); due {
		t.Fatal("非法 BaseAt 不应触发")
	}
	if _, due := dueAutoBackupSlot(config.AutoBackupConfig{Enabled: true, IntervalHours: 0, BaseAt: base.Format(time.RFC3339)}, base.Add(time.Hour)); due {
		t.Fatal("间隔为 0 不应触发")
	}
}

func TestDecideAutoBackup(t *testing.T) {
	base := time.Date(2026, 9, 14, 10, 0, 0, 0, time.Local)
	baseStr := base.Format(time.RFC3339)

	// 未启用 → 什么都不做。
	if action, _ := decideAutoBackup(config.AutoBackupConfig{BaseAt: baseStr, IntervalHours: 1}, base.Add(time.Hour), time.Time{}); action != autoBackupIdle {
		t.Fatalf("未启用却要动作: %v", action)
	}
	// 首次启用（无基准）→ 写基准。
	if action, _ := decideAutoBackup(config.AutoBackupConfig{Enabled: true, IntervalHours: 1}, base, time.Time{}); action != autoBackupInitBase {
		t.Fatalf("无基准时应 InitBase, got %v", action)
	}
	// 到点 → 执行。
	action, slot := decideAutoBackup(config.AutoBackupConfig{Enabled: true, IntervalHours: 1, BaseAt: baseStr}, base.Add(time.Hour+3*time.Second), time.Time{})
	if action != autoBackupRun {
		t.Fatalf("到点应执行, got %v", action)
	}
	if !slot.Equal(base.Add(time.Hour)) {
		t.Fatalf("到点时刻 = %s, want %s", slot, base.Add(time.Hour))
	}
	// 没到点 → idle。
	if action, _ := decideAutoBackup(config.AutoBackupConfig{Enabled: true, IntervalHours: 6, BaseAt: baseStr}, base.Add(time.Hour), time.Time{}); action != autoBackupIdle {
		t.Fatalf("没到点却要执行: %v", action)
	}
}

// TestDecideAutoBackupRetryThrottle 到点没跑成时必须**保留**待执行状态，
// 但不能每 20 秒重试一次（会刷日志、反复连库）。
func TestDecideAutoBackupRetryThrottle(t *testing.T) {
	base := time.Date(2026, 9, 14, 10, 0, 0, 0, time.Local)
	ab := config.AutoBackupConfig{Enabled: true, IntervalHours: 1, BaseAt: base.Format(time.RFC3339)}
	now := base.Add(time.Hour + 10*time.Minute)

	if action, _ := decideAutoBackup(ab, now, now.Add(-time.Minute)); action != autoBackupIdle {
		t.Fatalf("距上次尝试 1 分钟就重试了: %v", action)
	}
	if action, _ := decideAutoBackup(ab, now, now.Add(-autoBackupRetry-time.Second)); action != autoBackupRun {
		t.Fatalf("超过重试间隔应重试, got %v", action)
	}
	// 关键回归：重试时到点时刻必须仍是原来那个 slot（不因重试而往后漂移）。
	_, slot := decideAutoBackup(ab, now, now.Add(-autoBackupRetry-time.Second))
	if !slot.Equal(base.Add(time.Hour)) {
		t.Fatalf("重试的到点时刻 = %s, want %s", slot, base.Add(time.Hour))
	}
}
