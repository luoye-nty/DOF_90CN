package main

// 账号名解析器（account_names.go）的契约测试。
//
// 这几条口径一旦破了，**所有**显示账号的页面都会受影响（把不可逆的
// dnf:<sha256(用户名)> 主键摆到运营面前），所以在这里锁住：
//   - 命中缓存 → 显示真实账号名；
//   - 没命中 → 原样回显主键（绝不空白）；
//   - 空白用户名不算翻译成功；
//   - 未登录 / 请求失败 → 不 panic、不报错，退回主键（老服务端没有该路由时会走这条）。
//
// 注意：真正发请求的部分要连 HTTPS 服务端，这里只测不依赖网络的那几条。

import (
	"context"
	"errors"
	"testing"
)

func TestAccountNameResolverTextAndFallback(t *testing.T) {
	r := newAccountNameResolver()

	// ① 没有任何缓存：原样回显主键（不能空白、不能变成 "—"）。
	if got := r.text("dnf:abc"); got != "dnf:abc" {
		t.Fatalf("未翻译的账号应回显主键，实际 %q", got)
	}
	// ② 空主键 → 空串（调用方用 orDash 之类自己兜底，不该显示 "（未匹配）"）。
	if got := r.text("   "); got != "" {
		t.Fatalf("空账号应返回空串，实际 %q", got)
	}
	// ③ 前后空格要吃掉（表格里拿到的主键偶尔带空格）。
	if got := r.text("  dnf:abc  "); got != "dnf:abc" {
		t.Fatalf("主键应 trim 后回显，实际 %q", got)
	}

	r.remember(map[string]string{"dnf:abc": " 1000000001 ", "dnf:blank": "   "})
	if got := r.text("dnf:abc"); got != "1000000001" {
		t.Fatalf("命中缓存应显示真实账号名（并 trim），实际 %q", got)
	}
	if got := r.text("dnf:blank"); got != "dnf:blank" {
		t.Fatalf("空白用户名不算翻译成功，应回显主键，实际 %q", got)
	}
}

func TestAccountNameResolverResolveWithoutClient(t *testing.T) {
	r := newAccountNameResolver()
	r.remember(map[string]string{"dnf:abc": "1000000001"})

	// 未登录（客户端为 nil）：不发请求，但已经翻译好的名字不能丢。
	names := r.resolve(context.Background(), nil, []string{"dnf:abc", "dnf:new", ""})
	if names["dnf:abc"] != "1000000001" {
		t.Fatalf("未登录时也应返回缓存里已有的名字，实际 %v", names)
	}
	if _, exists := names["dnf:new"]; exists {
		t.Fatal("未登录时不该凭空产生名字")
	}
}

func TestAccountNameResolverFailureIsSilent(t *testing.T) {
	r := newAccountNameResolver()
	// 失败只记一次日志、绝不 panic：老版本运维工具没有 /manage/v1/account/names
	// 路由（回 404），界面必须照常可用，只是退回显示主键。
	r.noteFailure(errors.New("not-found (HTTP 404)"))
	r.noteFailure(errors.New("not-found (HTTP 404)"))
	if got := r.text("dnf:abc"); got != "dnf:abc" {
		t.Fatalf("失败后应回退显示主键，实际 %q", got)
	}
	first := r.failed
	r.mu.Lock()
	r.failed = false
	r.mu.Unlock()
	if !first {
		t.Fatal("noteFailure 应置位 failed（用于只记一次日志）")
	}
}

func TestAccountNameListDedupesAndTrims(t *testing.T) {
	got := acctNameList("dnf:a", "  dnf:b  ", "dnf:a", "", "   ")
	want := []string{"dnf:a", "dnf:b"}
	if len(got) != len(want) {
		t.Fatalf("acctNameList = %v，期望 %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("acctNameList = %v，期望 %v（保持原顺序）", got, want)
		}
	}
}
