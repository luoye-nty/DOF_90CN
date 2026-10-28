package main

// 原生 GM 响应信封的两套格式契约（2026-10-05）。
//
// ── 为什么要有这组测试 ────────────────────────────────────────────────────
// 服务端有**两个接口族、两套信封**：
//
//	/local/dnf-gm/*       → {"ok":true,"data":{...}}         （gm_admin.go）
//	/dnf/gm/characters/*  → {"success":true,"data":{...}}    （gm_tool_admin.go writeGMData）
//
// 改造前 `acctEnvelope.OK` 是 `bool`。接上 career（走 /dnf/gm/*）之后，
// `{"success":true,...}` 里没有 `ok` 字段 → `OK` 解析成 false →
// **HTTP 200 的成功响应被当成失败**，最终报成
// 「DNF90Server 原生 GM 接口: 200 OK (HTTP 502)」——
// 一句话里既有 200 又有 502，运营完全无法判断到底成没成。
//
// 这组测试钉住：两套信封都认、字段缺席与显式 false 必须区分、未知信封按 HTTP 状态兜底。

import (
	"encoding/json"
	"testing"
)

// TestAcctEnvelopeRecognizesBothFamilies 直接喂两族的真实报文。
func TestAcctEnvelopeRecognizesBothFamilies(t *testing.T) {
	cases := []struct {
		name   string
		body   string
		status int
		want   bool
		// hasData 该报文里是否**应该**能解析出 data（{} 这种本来就为空，不参与断言）。
		hasData bool
	}{
		// /local/dnf-gm/* 家族
		{"local 族成功", `{"ok":true,"data":{"job":0}}`, 200, true, true},
		{"local 族显式失败", `{"ok":false,"error":"dnf gm request is invalid"}`, 400, false, false},
		// /dnf/gm/* 家族 —— 这就是线上踩到的那一种
		{"gm 族成功（career 返回的形状）", `{"success":true,"data":{"job":8,"first":0,"awakening":0}}`, 200, true, true},
		{"gm 族显式失败", `{"success":false,"message":"character is online"}`, 409, false, false},
		// 未知信封（将来出现第三种）→ 按 HTTP 状态兜底，**绝不能把 2xx 当失败**
		{"未知信封 + 200 → 按成功", `{"result":"ok","data":{}}`, 200, true, true},
		{"未知信封 + 500 → 按失败", `{"result":"boom"}`, 500, false, false},
		{"空对象 + 200 → 按成功", `{}`, 200, true, false},
		// 两套字段同时出现时，ok 优先（本工具自己的族）
		{"同时有 ok/success：ok 优先", `{"ok":false,"success":true}`, 200, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var env acctEnvelope
			if err := json.Unmarshal([]byte(tc.body), &env); err != nil {
				t.Fatalf("解析 %s 失败：%v", tc.body, err)
			}
			if got := acctEnvelopeOK(env, tc.status); got != tc.want {
				t.Fatalf("acctEnvelopeOK(%s, %d) = %v，期望 %v", tc.body, tc.status, got, tc.want)
			}
			// data 必须从两族里都能取到（字段名恰好都叫 data）。
			if tc.hasData && len(env.Data) == 0 {
				t.Fatalf("%s 的 data 未被解析出来", tc.body)
			}
		})
	}
}

// TestAcctEnvelopeDistinguishesAbsentFromFalse 三态指针语义：
// "字段缺席"（另一套信封）必须与"显式 false"（真失败）区分开 ——
// 这正是 bug 的根因，如果哪天有人把 *bool 改回 bool，这条会失败。
func TestAcctEnvelopeDistinguishesAbsentFromFalse(t *testing.T) {
	var absent acctEnvelope
	if err := json.Unmarshal([]byte(`{"data":{}}`), &absent); err != nil {
		t.Fatal(err)
	}
	if absent.OK != nil {
		t.Fatal("ok 字段缺席时 OK 必须是 nil（不能是 false），否则未知信封会被当成失败")
	}
	if absent.Success != nil {
		t.Fatal("success 字段缺席时 Success 必须是 nil")
	}

	var explicitFalse acctEnvelope
	if err := json.Unmarshal([]byte(`{"ok":false}`), &explicitFalse); err != nil {
		t.Fatal(err)
	}
	if explicitFalse.OK == nil || *explicitFalse.OK {
		t.Fatal("显式 ok=false 必须解析成非 nil 的 false")
	}
	if acctEnvelopeOK(explicitFalse, 200) {
		t.Fatal("显式 ok=false 即便 HTTP 200 也必须判失败")
	}
}

// TestAcctEnvelopeBoolText 日志文案：nil 要读作「缺席」，否则排查时分不清
// "服务端没这个字段" 和 "服务端说 false"。
func TestAcctEnvelopeBoolText(t *testing.T) {
	yes, no := true, false
	cases := []struct {
		in   *bool
		want string
	}{
		{nil, "缺席"},
		{&yes, "true"},
		{&no, "false"},
	}
	for _, tc := range cases {
		if got := acctEnvelopeBoolText(tc.in); got != tc.want {
			t.Fatalf("acctEnvelopeBoolText(%v) = %q，期望 %q", tc.in, got, tc.want)
		}
	}
}
