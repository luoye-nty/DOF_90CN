package store

// 账号主键 ↔ 用户名解析的测试。
//
// 这里能纯逻辑钉住两件事（都不需要真库）：
//  1. 空输入原样返回空，别去查库；
//  2. 已经是账号主键形态的输入**绝不查库** —— 用 &Store{}（db 为 nil）验证：
//     真去查库会 panic，所以这条用例就是"不做多余查询"的守门人。

import (
	"context"
	"strings"
	"testing"
)

func TestResolveAccountInputEmpty(t *testing.T) {
	s := &Store{}
	for _, in := range []string{"", "   ", "\t"} {
		got, err := s.ResolveAccountInput(context.Background(), in)
		if err != nil {
			t.Fatalf("ResolveAccountInput(%q) 出错: %v", in, err)
		}
		if got != "" {
			t.Fatalf("ResolveAccountInput(%q) = %q，期望空串", in, got)
		}
	}
}

func TestResolveAccountInputKeepsAccountKeyWithoutQuery(t *testing.T) {
	s := &Store{}                                 // db == nil：任何一次查询都会 panic，正好当"不许查库"的断言
	key := "dnf:" + strings.Repeat("95db080a", 8) // 64 位 hex，登录器账号主键形态
	got, err := s.ResolveAccountInput(context.Background(), "  "+key+"  ")
	if err != nil {
		t.Fatalf("ResolveAccountInput 出错: %v", err)
	}
	if got != key {
		t.Fatalf("ResolveAccountInput = %q，期望原样返回 %q", got, key)
	}
}

func TestLooksLikeAccountIDShape(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"dnf:" + strings.Repeat("a", 64), true},
		{"dnf:" + strings.Repeat("0", 32), true},
		{"dnf:1", false},                          // 服务端兜底主键（不是 32/64 位 hex），要走库查一次
		{"1000000001", false},                     // 纯数字用户名
		{"wpf-aa53", false},                       // 外部工具造的主键（要进库查一次）
		{"dnf:zz", false},                         // 尾巴不是 hex
		{"dnf:" + strings.Repeat("a", 31), false}, // 长度不对
		{"", false},
	}
	for _, tc := range cases {
		if got := LooksLikeAccountID(tc.in); got != tc.want {
			t.Fatalf("LooksLikeAccountID(%q) = %v，期望 %v", tc.in, got, tc.want)
		}
	}
}
