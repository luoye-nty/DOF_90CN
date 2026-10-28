//go:build paid

package manage

import (
	"context"
	"net/http"
	"testing"
)

// TestPointsinkApplyDailyLimit 锁住泡点配置端点的"每日限额"契约：
// 上限校验在服务端做，且必须把 daily_limit 原样透传给执行端 Bridge。
//
// 这条契约是跨端的：管理工具"保存全局配置"会整组提交（enabled/interval/points/daily_limit），
// 少传或传错都会被这里挡住或静默写成 0（不限），运营配好的限额就丢了。
func TestPointsinkApplyDailyLimit(t *testing.T) {
	var gotEnabled bool
	var gotInterval, gotPoints, gotLimit int
	var gotCurrency string
	bridge := &Bridge{
		PointsinkApply: func(ctx context.Context, enabled bool, intervalSeconds, points, dailyLimit int, currency string) (PointsinkStatus, error) {
			gotEnabled, gotInterval, gotPoints, gotLimit = enabled, intervalSeconds, points, dailyLimit
			gotCurrency = currency
			return PointsinkStatus{Enabled: enabled, IntervalSeconds: intervalSeconds, Points: points, DailyLimit: dailyLimit}, nil
		},
	}
	s := newTestServer(t, bridge)

	// ① 正常：1000 点券/天 透传到 Bridge，并在响应里回显
	code, raw := doMux(t, s, http.MethodPost, "/manage/v1/pointsink",
		`{"enabled":true,"interval_seconds":60,"points":100,"daily_limit":1000}`)
	if code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200（响应 %s）", code, raw)
	}
	if !gotEnabled || gotInterval != 60 || gotPoints != 100 || gotLimit != 1000 {
		t.Fatalf("桥接收到 enabled=%v interval=%d points=%d limit=%d", gotEnabled, gotInterval, gotPoints, gotLimit)
	}
	ok, errMsg, data := decodeEnvelope(t, raw)
	if !ok {
		t.Fatalf("应 ok=true，实际 error=%q", errMsg)
	}
	if !contains(string(data), `"daily_limit":1000`) {
		t.Fatalf("响应里应回显 daily_limit：%s", string(data))
	}
	// 该用例的 body 不带 currency（老管理端形状）：必须原样传空串给归一化层，
	// 由它落成点券 —— 而不是在这里被拒或擅自改写。
	if gotCurrency != "" {
		t.Fatalf("body 不带 currency 时，桥应收到空串，实际 %q", gotCurrency)
	}

	// ② 缺字段 = 0（不限）：老客户端提交不限额是合法语义，不能报错
	code, raw = doMux(t, s, http.MethodPost, "/manage/v1/pointsink",
		`{"enabled":true,"interval_seconds":60,"points":100}`)
	if code != http.StatusOK {
		t.Fatalf("缺 daily_limit 时状态码 = %d，期望 200（响应 %s）", code, raw)
	}
	if gotLimit != 0 {
		t.Fatalf("缺字段应落成 0（不限），实际 %d", gotLimit)
	}

	// ③ 越界：负数与超过上限都必须 400，且不调用桥接
	for _, body := range []string{
		`{"enabled":true,"interval_seconds":60,"points":100,"daily_limit":-1}`,
		`{"enabled":true,"interval_seconds":60,"points":100,"daily_limit":100000001}`,
	} {
		before := gotLimit
		code, raw = doMux(t, s, http.MethodPost, "/manage/v1/pointsink", body)
		if code != http.StatusBadRequest {
			t.Fatalf("越界 daily_limit 应 400，实际 %d（body=%s 响应 %s）", code, body, raw)
		}
		if gotLimit != before {
			t.Fatalf("被拒的请求不应调用桥接（body=%s）", body)
		}
	}
}

// TestPointsinkApplyCurrency 锁住泡点「发放币种」的跨端契约（2026-10-05 新增）：
//  1. 带 currency → 原样透传给 Bridge；
//  2. **缺 currency（老管理端）→ 必须 200**，否则老管理端连泡点配置都改不了；
//  3. 非法 currency → 400，且不调用桥接。
//
// 第 2 条是这次改动最容易踩的坑：如果这里把空值当非法拒掉，
// 运营用旧版管理工具点"保存泡点"会直接失败。
func TestPointsinkApplyCurrency(t *testing.T) {
	var gotCurrency string
	var calls int
	bridge := &Bridge{
		PointsinkApply: func(ctx context.Context, enabled bool, intervalSeconds, points, dailyLimit int, currency string) (PointsinkStatus, error) {
			gotCurrency = currency
			calls++
			return PointsinkStatus{
				Enabled: enabled, IntervalSeconds: intervalSeconds, Points: points,
				DailyLimit: dailyLimit, Currency: currency, CurrencyLabel: "代币券",
			}, nil
		},
	}
	s := newTestServer(t, bridge)

	// ① 带币种：透传 + 回显
	code, raw := doMux(t, s, http.MethodPost, "/manage/v1/pointsink",
		`{"enabled":true,"interval_seconds":60,"points":100,"daily_limit":0,"currency":"token_cera"}`)
	if code != http.StatusOK {
		t.Fatalf("带币种应 200，实际 %d（%s）", code, raw)
	}
	if gotCurrency != "token_cera" {
		t.Fatalf("币种未透传，桥收到 %q", gotCurrency)
	}
	if _, _, data := decodeEnvelope(t, raw); !contains(string(data), `"currency":"token_cera"`) {
		t.Fatalf("响应里应回显 currency：%s", string(data))
	}

	// ② 老管理端：完全不带 currency 字段
	code, raw = doMux(t, s, http.MethodPost, "/manage/v1/pointsink",
		`{"enabled":true,"interval_seconds":60,"points":100}`)
	if code != http.StatusOK {
		t.Fatalf("缺 currency 必须放行（兼容老管理端），实际 %d（%s）", code, raw)
	}
	if gotCurrency != "" {
		t.Fatalf("缺字段应原样传空串给归一化层（由它落成点券），实际 %q", gotCurrency)
	}

	// ③ 非法币种：gold 不是泡点可发币种（金币是角色级、不是账号钱包，限额台账也对不上）
	before := calls
	code, raw = doMux(t, s, http.MethodPost, "/manage/v1/pointsink",
		`{"enabled":true,"interval_seconds":60,"points":100,"currency":"gold"}`)
	if code != http.StatusBadRequest {
		t.Fatalf("非法币种应 400，实际 %d（%s）", code, raw)
	}
	if calls != before {
		t.Fatal("被拒的请求不应调用桥接")
	}
}

// contains 小工具：避免为一个子串判断引入 strings 依赖冲突。
func contains(haystack, needle string) bool {
	if len(needle) == 0 || len(haystack) < len(needle) {
		return false
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
