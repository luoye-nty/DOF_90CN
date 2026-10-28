package main

// 泡点到账提示的文案与事件类型契约测试。
//
// 两件事都属于"写错了线上才会发现、而且很难查"的类型：
//  1. 文案格式由需求方逐字指定，改跑一个字玩家就会看出来；
//  2. cera_granted 这个**事件类型字符串**在两侧各写了一遍 —— 写入方在 pointsink.go，
//     清理方在 internal/store/gm_events_prune.go。两边一旦漂移，清理就彻底失效
//     （条件匹配不上 → 一条都不删 → 表继续按"每账号 8640 行/天"涨），
//     而失效是**静默**的：没人会注意到清理没生效，直到认证服务被拖垮。

import (
	"testing"
	"time"

	"dnf90ops/internal/store"
)

// durMillis 把毫秒数转成 Duration（表驱动用例里写 10000 比写 10*time.Second 更直观）。
func durMillis(ms int64) time.Duration { return time.Duration(ms) * time.Millisecond }

// TestPsGrantMessage 文案逐字锁定（需求方 2026-09-28 指定）。
//
// 2026-10-05 起文案里的币种名随配置变（泡点可以发代币券了）。
// **点券那三条必须逐字不变** —— 那是需求方锁死的原文案，动一个字玩家就会看出来。
func TestPsGrantMessage(t *testing.T) {
	cases := []struct {
		name                      string
		intervalSeconds, perRound int64
		thisRound, sessionTotal   int64
		currency                  string
		want                      string
	}{
		{
			name: "常规一轮", intervalSeconds: 10, perRound: 100, thisRound: 100, sessionTotal: 500,
			currency: "cera",
			want:     "每10秒获得100点券，当前获得100点券，本次登录共计获得500点券。",
		},
		{
			name: "首轮（累计=本轮）", intervalSeconds: 10, perRound: 100, thisRound: 100, sessionTotal: 100,
			currency: "cera",
			want:     "每10秒获得100点券，当前获得100点券，本次登录共计获得100点券。",
		},
		{
			name: "每日限额削减了本轮：当前获得 < 每轮点数，且与累计不同",
			intervalSeconds: 300, perRound: 100, thisRound: 30, sessionTotal: 1230,
			currency: "cera",
			want:     "每300秒获得100点券，当前获得30点券，本次登录共计获得1230点券。",
		},
		{
			name: "空币种 = 点券（老配置不走迁移，行为必须与改动前一致）",
			intervalSeconds: 10, perRound: 100, thisRound: 100, sessionTotal: 100,
			currency: "",
			want:     "每10秒获得100点券，当前获得100点券，本次登录共计获得100点券。",
		},
		{
			name: "代币券：只有币种名变，句式与数字位完全不变",
			intervalSeconds: 10, perRound: 100, thisRound: 100, sessionTotal: 500,
			currency: "token_cera",
			want:     "每10秒获得100代币券，当前获得100代币券，本次登录共计获得500代币券。",
		},
		{
			name: "欢乐代币券",
			intervalSeconds: 60, perRound: 50, thisRound: 50, sessionTotal: 150,
			currency: "happy_token_cera",
			want:     "每60秒获得50欢乐代币券，当前获得50欢乐代币券，本次登录共计获得150欢乐代币券。",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := psGrantMessage(tc.intervalSeconds, tc.perRound, tc.thisRound, tc.sessionTotal, tc.currency)
			if got != tc.want {
				t.Fatalf("文案 =\n  %q\nwant\n  %q", got, tc.want)
			}
		})
	}
}

// TestPsIntervalSeconds 间隔换算：异常值兜到 1，避免出现"每0秒获得"这种明显不对的说法。
func TestPsIntervalSeconds(t *testing.T) {
	cases := []struct {
		in   int64 // 毫秒
		want int64
	}{
		{10_000, 10},
		{300_000, 300},
		{500, 1},   // 亚秒 → 兜到 1
		{0, 1},     // 未配置 → 兜到 1
		{-5000, 1}, // 负值 → 兜到 1
	}
	for _, tc := range cases {
		if got := psIntervalSeconds(durMillis(tc.in)); got != tc.want {
			t.Fatalf("psIntervalSeconds(%dms) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

// TestCeraGrantedTypeMatchesPruner 写入方与清理方的事件类型必须完全一致。
// 漂移的后果是清理静默失效（见文件头注释）。
func TestCeraGrantedTypeMatchesPruner(t *testing.T) {
	if psEventCeraGranted != store.GMEventTypeCeraGranted {
		t.Fatalf("写入方用 %q，清理方用 %q —— 对不上时清理会一条都删不掉，"+
			"事件表会按每账号 8640 行/天无限增长，最终拖垮认证服务",
			psEventCeraGranted, store.GMEventTypeCeraGranted)
	}
}
