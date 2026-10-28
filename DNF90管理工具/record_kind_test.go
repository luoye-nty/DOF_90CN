package main

// 记录类别清单的契约测试。
//
// 类别是**硬编码在四处**的：go-server 的 recordKinds / 运维工具的白名单 /
// 本工具的 recKindValues / api_client.go 的 RecordKind* 常量。历史上"团本记录"
// 就是服务端已经写了一整类、运维工具白名单没跟，导致界面上根本选不到 ——
// 这类错误编译期看不出来，只能靠断言锁住。
//
// 这里锁三件事：
//  1. 下拉的标签与取值两个数组一一对应（错位会让"对话记录"选中别的类别）；
//  2. 每个类别 recSummary 都有 case（漏加时"概要"列会整列空白）；
//  3. 服务端已知的六个类别都在下拉里。

import (
	"strings"
	"testing"
)

func TestRecKindDropdownArraysStayInSync(t *testing.T) {
	if len(recKindLabels) != len(recKindValues) {
		t.Fatalf("类别下拉标签 %d 项、取值 %d 项，必须一一对应", len(recKindLabels), len(recKindValues))
	}
	seen := make(map[string]struct{}, len(recKindValues))
	for i, kind := range recKindValues {
		if strings.TrimSpace(kind) == "" {
			t.Fatalf("第 %d 项类别取值为空", i)
		}
		if strings.TrimSpace(recKindLabels[i]) == "" {
			t.Fatalf("第 %d 项类别标签为空", i)
		}
		if _, dup := seen[kind]; dup {
			t.Fatalf("类别取值 %q 重复（下拉会出现两个同样的类别）", kind)
		}
		seen[kind] = struct{}{}
	}
}

func TestEveryRecordKindHasSummaryCase(t *testing.T) {
	// 每个类别喂一个"只有这一类才有的字段"，断言它出现在概要里。
	// 为什么不能用 event_name 代替：recSummary 的 default 分支返回的就是 event_name，
	// 漏加 case 时输出同样非空 —— 只有"类别特有字段"才能区分"有 case"和"掉进 default"。
	cases := []struct {
		kind string
		mark string // 该类别特有的字段值，必须出现在概要里
		entry map[string]any
	}{
		{RecordKindCera, "实扣 456 点券", map[string]any{"cera_spent": "456"}},
		{RecordKindChat, "你好", map[string]any{"message": "你好"}},
		{RecordKindDungeon, "王的遗迹", map[string]any{"dungeon_name": "王的遗迹"}},
		{RecordKindRaid, "在线 3/8", map[string]any{"online_members": "3", "roster_members": "8"}},
		{RecordKindGoldConsign, "100000 金币", map[string]any{"gold_amount": "100000"}},
		{RecordKindAuction, "炉岩炭", map[string]any{"item_name": "炉岩炭"}},
		{RecordKindScene, "wait_for_scene_ready", map[string]any{"defer_reason": "wait_for_scene_ready"}},
	}
	for _, tc := range cases {
		entry := map[string]any{"event_name": "测试事件"}
		for k, v := range tc.entry {
			entry[k] = v
		}
		got := recSummary(tc.kind, entry)
		if !strings.Contains(got, tc.mark) {
			t.Fatalf("类别 %s 的概要 = %q，应包含 %q（recSummary 里可能漏了 case）", tc.kind, got, tc.mark)
		}
	}
}

func TestServerRecordKindsAreAllSelectable(t *testing.T) {
	// 与服务端 go-server/internal/services/dnfbridge/activity_record.go 的 recordKinds 对齐。
	want := []string{
		RecordKindCera, RecordKindChat, RecordKindDungeon,
		RecordKindRaid, RecordKindGoldConsign, RecordKindAuction, RecordKindScene,
	}
	for _, kind := range want {
		found := false
		for _, v := range recKindValues {
			if v == kind {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("服务端类别 %q 不在下拉取值里：这类记录运营永远选不到", kind)
		}
	}
}
