package main

// 本轮三个界面改动的契约测试（都是纯函数/纯模型逻辑，不建窗、不联网）：
//
//  1. 在线福利的 item_id 从 NumberEdit 换成 LineEdit（为了能复制粘贴），
//     控件级数字限制随之消失 —— 解析函数是唯一防线，必须"只认数字、绝不尽力解析"；
//  2. 副本记录新增「通关时间」列，取值只来自 elapsed_seconds；
//  3. 在线角色表勾选某一行 = 选中该角色作为操作依据（服主实测：勾了人却发给了上一次查询选中的角色）。

import "testing"

func TestWelfareParseItemID(t *testing.T) {
	cases := []struct {
		in      string
		want    uint32
		wantErr bool
	}{
		{"", 0, false},          // 点券/黑钻奖励本来就不填
		{"   ", 0, false},       // 只有空格 = 没填
		{"1001", 1001, false},   // 正常
		{" 1001 ", 1001, false}, // 首尾空格
		{"１２３", 123, false},      // 全角数字（中文输入法下粘贴很常见）→ 转半角
		{"99999999", 99999999, false},
		{"100000000", 100000000, false}, // 9 位必须放行（本服 50.9% 的道具 ID 是 9 位）
		{"490701973", 490701973, false}, // 现场真实道具 ID
		{"4294967295", 4294967295, false}, // uint32 上限
		{"4294967296", 0, true},           // 超 uint32 才报错
		{"1001a", 0, true},     // 混入字母必须报错，不能解析成 1001
		{"10.5", 0, true},      // 小数点
		{"-1", 0, true},        // 负号
		{"1 001", 0, true},     // 中间有空格
		{"０x1", 0, true},       // 全角+字母
	}
	for _, tc := range cases {
		got, err := welfareParseItemID(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Fatalf("welfareParseItemID(%q) 应报错，实际返回 %d", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Fatalf("welfareParseItemID(%q) 意外报错: %v", tc.in, err)
		}
		if got != tc.want {
			t.Fatalf("welfareParseItemID(%q) = %d，期望 %d", tc.in, got, tc.want)
		}
	}
}

func TestRecClearTimeText(t *testing.T) {
	// 副本记录：秒数 → "X 分 Y 秒（N 秒）"
	if got := recClearTimeText(map[string]any{"elapsed_seconds": float64(192)}); got != "3 分 12 秒（192 秒）" {
		t.Fatalf("数字型 elapsed_seconds = %q", got)
	}
	if got := recClearTimeText(map[string]any{"elapsed_seconds": "45"}); got != "45 秒" {
		t.Fatalf("字符串型 elapsed_seconds = %q", got)
	}
	// 其它类别没有这个字段 → 空串（列里显示空白，不编一个"—"出来）
	if got := recClearTimeText(map[string]any{"message": "你好"}); got != "" {
		t.Fatalf("非副本记录的通关时间应为空，实际 %q", got)
	}
	// 解析不了的取值原样带回（不吞掉服务端给的原始信息）
	if got := recClearTimeText(map[string]any{"elapsed_seconds": "abc"}); got != "abc 秒" {
		t.Fatalf("无法解析时应原样显示，实际 %q", got)
	}
}

func TestRecClearTimeColumnIsInTable(t *testing.T) {
	// 表格列顺序：时间|角色|角色ID|账号|频道|概要|通关时间|结果（结果必须在最后一列）。
	rows := []recRow{{
		ts: "2026-09-30 13:00:00", character: "角色A", charID: "58",
		accountID: "dnf:abc", accountName: "1000000001", channel: "121",
		summary: "一次副本", clearTime: "3 分 12 秒（192 秒）", result: "通关",
		record: map[string]any{},
	}}
	m := &recTableModel{rows: rows}
	if got := m.Value(0, 6); got != "3 分 12 秒（192 秒）" {
		t.Fatalf("第 6 列（通关时间）= %v", got)
	}
	if got := m.Value(0, 7); got != "通关" {
		t.Fatalf("第 7 列（结果）= %v", got)
	}
	if got := m.Value(0, 8); got != nil {
		t.Fatalf("第 8 列应越界返回 nil，实际 %v", got)
	}
}

func TestOnlineModelCheckSelectsRow(t *testing.T) {
	var picked []int
	m := &acctOnlineModel{
		items: []onlineCharacterOut{{Name: "角色A"}, {Name: "角色B"}},
		onCheck: func(index int) {
			picked = append(picked, index)
		},
	}
	m.reset(m.items)

	// 勾选 = 选中（服主口径："应该选中那个发哪个"）
	if err := m.SetChecked(1, true); err != nil {
		t.Fatalf("SetChecked 报错: %v", err)
	}
	if len(picked) != 1 || picked[0] != 1 {
		t.Fatalf("勾选第 1 行应回调一次 index=1，实际 %v", picked)
	}
	// 取消勾选不改变当前操作依据
	if err := m.SetChecked(1, false); err != nil {
		t.Fatalf("SetChecked 报错: %v", err)
	}
	if len(picked) != 1 {
		t.Fatalf("取消勾选不该回调，实际 %v", picked)
	}
	// 越界索引安全返回，不回调
	if err := m.SetChecked(99, true); err != nil {
		t.Fatalf("越界 SetChecked 应安全返回，实际 %v", err)
	}
	if len(picked) != 1 {
		t.Fatalf("越界索引不该回调，实际 %v", picked)
	}
}
