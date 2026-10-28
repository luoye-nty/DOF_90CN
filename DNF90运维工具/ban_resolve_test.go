package main

// 封禁目标解析的纯函数测试。
// 这里锁的是"管理工具只填用户名、封禁值留空"这条曾经必失败的主流程：
// 原实现拿空字符串去查库 → user-not-found，运营看到的就是"封号坏了"。

import "testing"

func TestChooseAccountLookup(t *testing.T) {
	const accountID = "dnf:e3319b58ec68c92d20b223abdb895f90f4bb05ccc7a3ce3bf34c68c7b50ebb3e"

	cases := []struct {
		name        string
		value       string
		username    string
		wantMode    accountLookupMode
		wantOperand string
	}{
		{"只填用户名（管理工具主流程，曾经必然失败）", "", "1000000001", accountLookupByUsername, "1000000001"},
		{"用户名前后空白", "  ", " 1000000001 ", accountLookupByUsername, "1000000001"},
		{"只填封禁值=用户名（运维工具 GUI 用法）", "1000000001", "", accountLookupByUsername, "1000000001"},
		{"只填封禁值=精确账号ID", accountID, "", accountLookupByID, accountID},
		{"两者都填时用户名优先", accountID, "1000000001", accountLookupByUsername, "1000000001"},
		{"两者都空", "", "", accountLookupEmpty, ""},
		{"账号ID前后空白", " " + accountID + " ", "", accountLookupByID, accountID},
		{"老库 md5 账号ID 也认", "dnf:992d0c4d27b0493a4a1a4eaa337d266e", "", accountLookupByID, "dnf:992d0c4d27b0493a4a1a4eaa337d266e"},
	}

	for _, testCase := range cases {
		mode, operand := chooseAccountLookup(testCase.value, testCase.username)
		if mode != testCase.wantMode || operand != testCase.wantOperand {
			t.Fatalf("%s：chooseAccountLookup(%q, %q) = (%d, %q)，期望 (%d, %q)",
				testCase.name, testCase.value, testCase.username, mode, operand, testCase.wantMode, testCase.wantOperand)
		}
	}
}

// TestBanTargetLabel：错误提示必须说清"到底拿什么去查了"，否则运营只会看到 user-not-found 干瞪眼。
func TestBanTargetLabel(t *testing.T) {
	if got := banTargetLabel("", "1000000001"); got != "用户名 1000000001" {
		t.Fatalf("banTargetLabel = %q", got)
	}
	if got := banTargetLabel("dnf:992d0c4d27b0493a4a1a4eaa337d266e", ""); got != "账号ID dnf:992d0c4d27b0493a4a1a4eaa337d266e" {
		t.Fatalf("banTargetLabel = %q", got)
	}
	if got := banTargetLabel("", ""); got != "（未填写）" {
		t.Fatalf("banTargetLabel = %q", got)
	}
}
