package main

// 批量任务（account_batch.go）的契约测试。
//
// 这里锁两件在编译期看不出来的事：
//  1. **请求体字段名**：管理工具发出去的 JSON 键必须与服务器端 manage 包的
//     struct tag 完全一致（改错了服务端只会回"参数非法"，而 UI 上看不出是键名问题）；
//  2. **范围常量取值**：范围字符串是跨仓库契约，两边都得是 all-characters / online-characters
//     这类固定字面量，不能各自改名。
//
// 另外顺手保护一下"控件指针可能为 nil"的路径：批量骨架里到处传 *walk.PushButton /
// *walk.Label，任何一个为 nil 都必须是安全跳过，而不是在后台协程里 panic。

import (
	"encoding/json"
	"testing"
)

func TestMailboxClearBatchRequestWireShape(t *testing.T) {
	raw, err := json.Marshal(MailboxClearBatchRequest{
		Scope:   MailboxScopeAllCharacters,
		Confirm: true,
		DryRun:  true,
	})
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("反序列化失败: %v", err)
	}
	if got["scope"] != "all-characters" {
		t.Fatalf("scope = %v，期望 all-characters", got["scope"])
	}
	if got["confirm"] != true {
		t.Fatalf("confirm = %v，期望 true（服务端据此拒绝误触发）", got["confirm"])
	}
	if got["dry_run"] != true {
		t.Fatalf("dry_run = %v，期望 true（预演）", got["dry_run"])
	}
	// max 留空时不该出现在请求体里（服务端用默认值）。
	if _, exists := got["max"]; exists {
		t.Fatalf("max=0 时不应出现在请求体里，实际 %v", got["max"])
	}

	// 全服档必须显式把上限开满：服务端默认只处理 5000 个角色，
	// 不填就会被截断，运营会在"没清完"的情况下以为清完了。
	all, err := json.Marshal(MailboxClearBatchRequest{
		Scope:   MailboxScopeAllCharacters,
		Confirm: true,
		Max:     mailboxClearBatchMaxAll,
	})
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	var allGot map[string]any
	if err := json.Unmarshal(all, &allGot); err != nil {
		t.Fatalf("反序列化失败: %v", err)
	}
	if allGot["max"] != float64(mailboxClearBatchMaxAll) {
		t.Fatalf("全服档 max = %v，期望 %d", allGot["max"], mailboxClearBatchMaxAll)
	}
}

func TestWalletMailBatchRequestWireShape(t *testing.T) {
	raw, err := json.Marshal(WalletMailBatchRequest{
		Scope:    WalletMailScopeServerFirst,
		Currency: "gold",
		Amount:   100000,
	})
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("反序列化失败: %v", err)
	}
	if got["scope"] != "server-first-character" {
		t.Fatalf("scope = %v，期望 server-first-character", got["scope"])
	}
	if got["currency"] != "gold" {
		t.Fatalf("currency = %v，期望 gold", got["currency"])
	}
	if got["amount"] != float64(100000) {
		t.Fatalf("amount = %v，期望 100000", got["amount"])
	}
}

func TestBatchScopeConstants(t *testing.T) {
	// 与服务器端 manage 包常量一字不差；改这里必须同时改那边（否则服务端回 400）。
	// 注意：两套范围的取值有重叠（都是 all-accounts / online-characters），
	// 所以这里用切片而不是 map —— map 会因为重复的 value 直接编译不过。
	cases := []struct {
		name string
		got  string
		want string
	}{
		{"邮箱批量 all-characters", MailboxScopeAllCharacters, "all-characters"},
		{"邮箱批量 online-characters", MailboxScopeOnlineCharacters, "online-characters"},
		{"邮箱批量 all-accounts", MailboxScopeAllAccounts, "all-accounts"},
		{"资产批量 online-characters", WalletMailScopeOnlineChars, "online-characters"},
		{"资产批量 all-accounts", WalletMailScopeAllAccounts, "all-accounts"},
		{"资产批量 server-first-character", WalletMailScopeServerFirst, "server-first-character"},
	}
	for _, tc := range cases {
		if tc.got != tc.want {
			t.Fatalf("%s = %q，期望 %q", tc.name, tc.got, tc.want)
		}
	}
}

func TestBatchJobHelpersTolerateNilWidgets(t *testing.T) {
	a := &App{}
	job := batchJob{prefix: "测试"}
	// 全部控件为 nil（未建窗 / 单测环境）：三个入口都必须安全返回，不能 panic。
	a.batchSetBusy(job, true)
	a.batchSetBusy(job, false)
	a.batchApply(job, &BatchTask{Status: "running", Phase: "sending", Total: 3, Done: 1})
	a.batchClearRunning(job)

	// taskID 指向的字段在批量开始/结束时被写入与清空。
	id := "t-1"
	job.taskID = &id
	a.batchClearRunning(job)
	if id != "" {
		t.Fatalf("batchClearRunning 后 taskID 应清空，实际 %q", id)
	}
}

func TestAssetTargetLabelsCoverAllBranches(t *testing.T) {
	// onAcctAssetSendBatch 按 CurrentIndex 分三支：0=单个角色、1=在线角色、2=全服首个角色。
	// 下拉项数量与分支数必须一致（多一项会走进错误的分支，少一项有目标选不到）。
	if len(acctAssetTargetLabels) != 3 {
		t.Fatalf("资产邮件发放目标有 %d 项，期望 3（单个 / 在线 / 全服首个）", len(acctAssetTargetLabels))
	}
}
