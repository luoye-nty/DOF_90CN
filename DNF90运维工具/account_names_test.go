// 账号显示口径（account_names.go）的契约测试。
//
// 运维工具本机版与管理工具那份是**两套实现**（一个查库、一个走 HTTP），但显示口径必须一致：
//   - 命中缓存 → 显示真实账号名；
//   - 没命中 → 原样回显主键（绝不空白）；
//   - 空白用户名不算翻译成功；
//   - 认证库没起/查库失败 → 不报错、不 panic，退回主键。
//
// 这里只测不依赖数据库的那一半（缓存与"待翻译集合"的计算）。

package main

import (
	"errors"
	"testing"
)

func TestOpsAccountNamesTextAndRemember(t *testing.T) {
	r := &opsAccountNames{names: map[string]string{}}

	if got := r.text("dnf:abc"); got != "dnf:abc" {
		t.Fatalf("未翻译的账号应回显主键，实际 %q", got)
	}
	if got := r.text("  "); got != "" {
		t.Fatalf("空账号应返回空串，实际 %q", got)
	}
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

func TestOpsAccountNamesMissingDedupesAndKeepsOrder(t *testing.T) {
	r := &opsAccountNames{names: map[string]string{"dnf:known": "1000000001"}}
	got := r.missing([]string{"dnf:a", "  dnf:b  ", "dnf:a", "", "dnf:known", "   "})
	want := []string{"dnf:a", "dnf:b"}
	if len(got) != len(want) {
		t.Fatalf("missing = %v，期望 %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("missing = %v，期望 %v（去重、trim、保持原顺序、跳过已翻译的）", got, want)
		}
	}
}

func TestOpsAccountNamesStoreClearsFailureFlag(t *testing.T) {
	r := &opsAccountNames{names: map[string]string{}}
	r.noteFailure(errors.New("认证服务未运行"))
	r.mu.Lock()
	failed := r.failed
	r.mu.Unlock()
	if !failed {
		t.Fatal("noteFailure 应置位 failed（用于只记一次日志）")
	}
	r.store(map[string]string{"dnf:abc": "1000000001"})
	r.mu.Lock()
	failed = r.failed
	r.mu.Unlock()
	if failed {
		t.Fatal("成功翻译后应复位 failed，否则下次失败就不再记日志了")
	}
	if got := r.text("dnf:abc"); got != "1000000001" {
		t.Fatalf("store 后应能取到账号名，实际 %q", got)
	}
}

func TestAcctNameListDedupesAndTrims(t *testing.T) {
	got := acctNameList("dnf:a", " dnf:b ", "dnf:a", "", "   ")
	if len(got) != 2 || got[0] != "dnf:a" || got[1] != "dnf:b" {
		t.Fatalf("acctNameList = %v，期望 [dnf:a dnf:b]", got)
	}
}
