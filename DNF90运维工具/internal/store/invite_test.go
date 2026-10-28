package store

// 注册码（邀请码）纯函数测试。
//
// 2026-09-24 新增「注册码」功能：管理工具批量生成短码（形如 ABCD-EFGH），
// 玩家在登录器注册界面手输。这里钉住三件事：
//  1. 码只从去易混字符集里取（玩家不会把 O 抄成 0、把 I 抄成 1）；
//  2. 归一化能把"漏横线/小写/多空格"都收敛成同一个值（否则玩家明明抄对了却注册失败）；
//  3. 归一化不会把老格式长码（DNF90-xxxx-xxxx）改坏 —— 历史未使用的码必须继续可用。

import (
	"strings"
	"testing"
)

func TestGenerateInviteCodeFormat(t *testing.T) {
	seen := make(map[string]struct{}, 64)
	for i := 0; i < 64; i++ {
		code, err := GenerateInviteCode()
		if err != nil {
			t.Fatalf("GenerateInviteCode 出错: %v", err)
		}
		if len(code) != inviteCodeLength+1 {
			t.Fatalf("码 %q 长度 = %d，期望 %d（8 位 + 1 个横线）", code, len(code), inviteCodeLength+1)
		}
		if code[4] != '-' {
			t.Fatalf("码 %q 第 5 位应当是横线", code)
		}
		for _, r := range strings.ReplaceAll(code, "-", "") {
			if !strings.ContainsRune(inviteCharset, r) {
				t.Fatalf("码 %q 含字符集外的字符 %q（不能出现 0/O/1/I 这类易混字符）", code, r)
			}
		}
		if _, dup := seen[code]; dup {
			t.Fatalf("64 次生成出现重复码 %q（32^8 空间下几乎不可能，说明随机源有问题）", code)
		}
		seen[code] = struct{}{}
	}
}

// TestNormalizeInviteCode 归一化必须让"同一枚码的不同写法"收敛到同一个值。
func TestNormalizeInviteCode(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"标准写法", "ABCD-EFGH", "ABCDEFGH"},
		{"小写 + 无横线（玩家手打最常见）", "abcdefgh", "ABCDEFGH"},
		{"小写 + 带横线", "abcd-efgh", "ABCDEFGH"},
		{"中间有空格（从聊天软件复制）", "ABCD EFGH", "ABCDEFGH"},
		{"前后空白", "  ABCD-EFGH  ", "ABCDEFGH"},
		{"下划线当分隔（手误）", "ABCD_EFGH", "ABCDEFGH"},
		{"老格式长码：只去分隔、不改内容", "DNF90-ab12cd34ef56-7890abcd", "DNF90AB12CD34EF567890ABCD"},
		{"空", "   ", ""},
	}
	for _, tc := range cases {
		if got := NormalizeInviteCode(tc.raw); got != tc.want {
			t.Fatalf("%s: NormalizeInviteCode(%q) = %q，期望 %q", tc.name, tc.raw, got, tc.want)
		}
	}
}

// TestInviteCodeRoundTripThroughNormalize：生成的码无论玩家怎么写，归一化后都一致。
func TestInviteCodeRoundTripThroughNormalize(t *testing.T) {
	code, err := GenerateInviteCode()
	if err != nil {
		t.Fatalf("GenerateInviteCode: %v", err)
	}
	want := NormalizeInviteCode(code)
	if len(want) != inviteCodeLength {
		t.Fatalf("归一化后长度 = %d，期望 %d", len(want), inviteCodeLength)
	}
	if got := NormalizeInviteCode(strings.ToLower(code)); got != want {
		t.Fatalf("小写写法归一化后 = %q，期望 %q", got, want)
	}
	if got := NormalizeInviteCode(strings.ReplaceAll(code, "-", " ")); got != want {
		t.Fatalf("空格分隔写法归一化后 = %q，期望 %q", got, want)
	}
}
