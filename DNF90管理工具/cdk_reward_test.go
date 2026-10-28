package main

// CDK 奖励（物品 + 点券 + 黑钻）表单逻辑测试：纯函数，不建窗口、不连服务器。
//
// 管理工具与运维工具共用同一套 content_raw / reward_json 约定，
// 这里锁住点券行能生成 {"cera":N}、黑钻行能生成 {"black_diamond_days":N}（按天算），
// 且物品行行为不变、三种奖励可以混在同一张 CDK。

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestBuildRewardTextsItemsOnly(t *testing.T) {
	rows := []cdkRewardRow{
		{Kind: cdkRewardKindItem, ItemID: "1001", Count: "2"},
		{ItemID: "2002", Count: "1"}, // Kind 为空按物品处理（兼容老数据）
	}
	text, rewardJSON, err := buildRewardTexts(rows)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if text != "1001:2,2002:1" {
		t.Fatalf("奖励段 = %q", text)
	}
	var rewards []cdkRewardJSON
	if err := json.Unmarshal([]byte(rewardJSON), &rewards); err != nil {
		t.Fatalf("reward_json 非法: %v (%s)", err, rewardJSON)
	}
	if len(rewards) != 2 || rewards[0].ItemID != 1001 || rewards[0].Count != 2 || rewards[1].ItemID != 2002 {
		t.Fatalf("rewards = %+v", rewards)
	}
	if rewards[0].Cera != 0 {
		t.Fatalf("物品行不应带 cera: %+v", rewards[0])
	}
}

func TestBuildRewardTextsCeraOnly(t *testing.T) {
	rows := []cdkRewardRow{{Kind: cdkRewardKindCera, Count: "5000"}}
	text, rewardJSON, err := buildRewardTexts(rows)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if text != "cera:5000" {
		t.Fatalf("奖励段 = %q, want cera:5000", text)
	}
	if !strings.Contains(rewardJSON, `"cera":5000`) {
		t.Fatalf("reward_json = %s, 期望含 cera", rewardJSON)
	}
}

func TestBuildRewardTextsMixed(t *testing.T) {
	rows := []cdkRewardRow{
		{Kind: cdkRewardKindCera, Count: "3000"},
		{Kind: cdkRewardKindItem, ItemID: "777", Count: "5"},
		{Kind: cdkRewardKindCera, Count: "2000"},
	}
	text, rewardJSON, err := buildRewardTexts(rows)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if text != "cera:3000,777:5,cera:2000" {
		t.Fatalf("奖励段 = %q", text)
	}
	var rewards []cdkRewardJSON
	if err := json.Unmarshal([]byte(rewardJSON), &rewards); err != nil || len(rewards) != 3 {
		t.Fatalf("rewards=%+v err=%v", rewards, err)
	}
	var total int64
	for _, reward := range rewards {
		total += reward.Cera
	}
	if total != 5000 {
		t.Fatalf("点券合计 = %d, want 5000", total)
	}
}

func TestBuildRewardTextsRejectsBadInput(t *testing.T) {
	cases := []struct {
		name string
		rows []cdkRewardRow
	}{
		{"空列表", nil},
		{"物品缺 item_id", []cdkRewardRow{{Kind: cdkRewardKindItem, Count: "1"}}},
		{"物品 item_id 为 0", []cdkRewardRow{{Kind: cdkRewardKindItem, ItemID: "0", Count: "1"}}},
		{"物品数量为 0", []cdkRewardRow{{Kind: cdkRewardKindItem, ItemID: "1", Count: "0"}}},
		{"点券为 0", []cdkRewardRow{{Kind: cdkRewardKindCera, Count: "0"}}},
		{"点券为负", []cdkRewardRow{{Kind: cdkRewardKindCera, Count: "-5"}}},
		{"点券超上限", []cdkRewardRow{{Kind: cdkRewardKindCera, Count: "2147483648"}}},
		{"点券非数字", []cdkRewardRow{{Kind: cdkRewardKindCera, Count: "abc"}}},
		{"黑钻为 0", []cdkRewardRow{{Kind: cdkRewardKindBlackDiamond, Count: "0"}}},
		{"黑钻为负", []cdkRewardRow{{Kind: cdkRewardKindBlackDiamond, Count: "-30"}}},
		{"黑钻超上限", []cdkRewardRow{{Kind: cdkRewardKindBlackDiamond, Count: "3651"}}},
		{"黑钻非数字", []cdkRewardRow{{Kind: cdkRewardKindBlackDiamond, Count: "一个月"}}},
		{"多行黑钻合计超上限", []cdkRewardRow{
			{Kind: cdkRewardKindBlackDiamond, Count: "3000"},
			{Kind: cdkRewardKindBlackDiamond, Count: "1000"},
		}},
	}
	for _, testCase := range cases {
		if _, _, err := buildRewardTexts(testCase.rows); err == nil {
			t.Fatalf("%s：应当报错但通过了", testCase.name)
		}
	}
}

// TestBuildRewardTextsBlackDiamondOnly：黑钻行按"天"进 reward_json 与 content_raw。
func TestBuildRewardTextsBlackDiamondOnly(t *testing.T) {
	rows := []cdkRewardRow{{Kind: cdkRewardKindBlackDiamond, Count: "30"}}
	text, rewardJSON, err := buildRewardTexts(rows)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if text != "black_diamond:30" {
		t.Fatalf("奖励段 = %q, want black_diamond:30", text)
	}
	if !strings.Contains(rewardJSON, `"black_diamond_days":30`) {
		t.Fatalf("reward_json = %s, 期望含 black_diamond_days", rewardJSON)
	}
	var rewards []cdkRewardJSON
	if err := json.Unmarshal([]byte(rewardJSON), &rewards); err != nil || len(rewards) != 1 {
		t.Fatalf("rewards=%+v err=%v", rewards, err)
	}
	if rewards[0].BlackDiamondDays != 30 || rewards[0].ItemID != 0 || rewards[0].Cera != 0 {
		t.Fatalf("黑钻行字段 = %+v", rewards[0])
	}
}

// TestBuildRewardTextsAllThreeKinds：一张 CDK 同时带物品 + 点券 + 黑钻（多行黑钻按天累加）。
func TestBuildRewardTextsAllThreeKinds(t *testing.T) {
	rows := []cdkRewardRow{
		{Kind: cdkRewardKindItem, ItemID: "1001", Count: "2"},
		{Kind: cdkRewardKindCera, Count: "5000"},
		{Kind: cdkRewardKindBlackDiamond, Count: "30"},
		{Kind: cdkRewardKindBlackDiamond, Count: "15"},
	}
	text, rewardJSON, err := buildRewardTexts(rows)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if text != "1001:2,cera:5000,black_diamond:30,black_diamond:15" {
		t.Fatalf("奖励段 = %q", text)
	}
	var rewards []cdkRewardJSON
	if err := json.Unmarshal([]byte(rewardJSON), &rewards); err != nil || len(rewards) != 4 {
		t.Fatalf("rewards=%+v err=%v", rewards, err)
	}
	var bdTotal int64
	for _, reward := range rewards {
		bdTotal += reward.BlackDiamondDays
	}
	if bdTotal != 45 {
		t.Fatalf("黑钻合计 = %d, want 45", bdTotal)
	}
	// 编辑器内的天数合计自查（添加黑钻行时用来提前拦超上限）。
	if got := cdkRowsBlackDiamondDays(rows); got != 45 {
		t.Fatalf("编辑器黑钻合计 = %d, want 45", got)
	}
}

func TestCdkRewardModelShowsKind(t *testing.T) {
	model := &cdkRewardModel{rows: []cdkRewardRow{
		{Kind: cdkRewardKindCera, Count: "5000"},
		{Kind: cdkRewardKindItem, ItemID: "1001", Count: "2"},
		{Kind: cdkRewardKindBlackDiamond, Count: "30"},
	}}
	if model.RowCount() != 3 {
		t.Fatalf("行数 = %d, want 3", model.RowCount())
	}
	if got := model.Value(0, 1); got != "点券" {
		t.Fatalf("第 1 行类型 = %v, want 点券", got)
	}
	if got := model.Value(0, 2); got != "—" {
		t.Fatalf("点券行的 item_id 列 = %v, want —", got)
	}
	if got := model.Value(0, 3); got != "5000" {
		t.Fatalf("点券行的数量列 = %v, want 5000", got)
	}
	if got := model.Value(1, 2); got != "1001" {
		t.Fatalf("物品行的 item_id 列 = %v, want 1001", got)
	}
	if got := model.Value(2, 1); got != "黑钻" {
		t.Fatalf("第 3 行类型 = %v, want 黑钻", got)
	}
	if got := model.Value(2, 2); got != "—" {
		t.Fatalf("黑钻行的 item_id 列 = %v, want —", got)
	}
	if got := model.Value(2, 3); got != "30 天" {
		t.Fatalf("黑钻行的数量列 = %v, want 30 天（按天算）", got)
	}
	if got := model.Value(9, 0); got != nil {
		t.Fatalf("越界行 = %v, want nil", got)
	}
}
