package main

// CDK 奖励（物品 + 点券 + 黑钻）表单逻辑测试：纯函数，不建窗口、不连库。
//
// 重点锁住四件事：
//  1. 点券行能生成合法的 reward_json（{"cera":N}）与 content_raw 奖励段（cera:N）；
//  2. 黑钻行能生成合法的 reward_json（{"black_diamond_days":N}）与奖励段（black_diamond:N，按天算）；
//  3. 物品行行为与改造前完全一致（item_id/count，逗号连接），三种奖励可以混在同一张 CDK；
//  4. 点券行/黑钻行**不会**被当成邮件附件发出去（老代码会发 {item_id:0,count:0}）。

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
	var rewards []cdkRewardJSON
	if err := json.Unmarshal([]byte(rewardJSON), &rewards); err != nil || len(rewards) != 1 {
		t.Fatalf("rewards=%+v err=%v", rewards, err)
	}
	if rewards[0].ItemID != 0 || rewards[0].Count != 0 || rewards[0].Cera != 5000 {
		t.Fatalf("点券行字段 = %+v", rewards[0])
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
	// 点券是可以多行累加的（兑换时相加）。
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
		{"数量为空", []cdkRewardRow{{Kind: cdkRewardKindItem, ItemID: "1"}}},
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
	var rewards []cdkRewardJSON
	if err := json.Unmarshal([]byte(rewardJSON), &rewards); err != nil || len(rewards) != 1 {
		t.Fatalf("rewards=%+v err=%v", rewards, err)
	}
	if rewards[0].BlackDiamondDays != 30 || rewards[0].ItemID != 0 || rewards[0].Cera != 0 {
		t.Fatalf("黑钻行字段 = %+v", rewards[0])
	}
}

// TestBuildRewardTextsAllThreeKinds：一张 CDK 同时带物品 + 点券 + 黑钻（用户要求的组合）。
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
	if got := cdkRewardCeraTotal(rewardJSON); got != 5000 {
		t.Fatalf("点券合计 = %d, want 5000", got)
	}
	if got := cdkRewardBlackDiamondDays(rewardJSON); got != 45 {
		t.Fatalf("黑钻合计 = %d, want 45（多行按天累加）", got)
	}
	if got := len(parseCdkRewardJSON(rewardJSON)); got != 1 {
		t.Fatalf("附件数 = %d, want 1（点券/黑钻行不进附件）", got)
	}
	// 编辑器内的天数合计自查（添加黑钻行时用来提前拦超上限）。
	if got := cdkRowsBlackDiamondDays(rows); got != 45 {
		t.Fatalf("编辑器黑钻合计 = %d, want 45", got)
	}
}

// TestParseCdkRewardJSONSkipsCera 是"点券/黑钻不能被当成邮件附件"的防线：
// 补发邮件走的就是这个函数，若把点券行/黑钻行变成 {item_id:0,count:0} 的附件，
// 服务端会拒绝或者发出一封空邮件。
func TestParseCdkRewardJSONSkipsCera(t *testing.T) {
	rewardJSON := `[{"item_id":1001,"count":2,"bind":false},{"cera":5000},{"black_diamond_days":30},{"item_id":1002,"count":1,"bind":true}]`
	attachments := parseCdkRewardJSON(rewardJSON)
	if len(attachments) != 2 {
		t.Fatalf("附件数 = %d, want 2（点券行与黑钻行必须被跳过）", len(attachments))
	}
	for _, att := range attachments {
		if att.ItemID == 0 || att.Count == 0 {
			t.Fatalf("出现了空附件: %+v", att)
		}
	}
	if got := cdkRewardCeraTotal(rewardJSON); got != 5000 {
		t.Fatalf("点券合计 = %d, want 5000", got)
	}
	if got := cdkRewardBlackDiamondDays(rewardJSON); got != 30 {
		t.Fatalf("黑钻合计 = %d, want 30", got)
	}
	// 老数据（无 cera/black_diamond_days 字段）不受影响。
	legacy := `[{"item_id":1001,"count":2,"bind":false}]`
	if len(parseCdkRewardJSON(legacy)) != 1 || cdkRewardCeraTotal(legacy) != 0 || cdkRewardBlackDiamondDays(legacy) != 0 {
		t.Fatal("老格式奖励解析结果不对")
	}
	// 空/非法输入不 panic。
	for _, raw := range []string{"", "null", "not-json", "{}"} {
		if got := parseCdkRewardJSON(raw); len(got) != 0 {
			t.Fatalf("parseCdkRewardJSON(%q) = %+v, want 空", raw, got)
		}
		if got := cdkRewardCeraTotal(raw); got != 0 {
			t.Fatalf("cdkRewardCeraTotal(%q) = %d, want 0", raw, got)
		}
		if got := cdkRewardBlackDiamondDays(raw); got != 0 {
			t.Fatalf("cdkRewardBlackDiamondDays(%q) = %d, want 0", raw, got)
		}
	}
}

// TestRedeemRetryTargetsRow 锁住"补发失败邮件"的射程：
// 全失败可以补物品；只失败点券/黑钻的那部分**不能**自动补（重发就是重复发放）。
func TestRedeemRetryTargetsRow(t *testing.T) {
	cases := []struct {
		status    string
		mailError string
		want      bool
	}{
		{"failed", "物品邮件: dial tcp: refused; 点券: dial tcp: refused", true},
		{"failed", "", true},
		{"partial", "物品邮件: dial tcp: refused", true},
		{"partial", "物品邮件: refused; 点券: refused", true},
		{"partial", "点券: 服务端拒绝入账", false},
		{"partial", "黑钻: 服务端拒绝激活", false},
		{"partial", "点券: refused; 黑钻: refused", false},
		{"partial", "", true},
		{"sent", "", false},
		{"pending", "", false},
		{"", "", false},
		{"FAILED", "", true},
	}
	for _, testCase := range cases {
		if got := redeemRetryTargetsRow(testCase.status, testCase.mailError); got != testCase.want {
			t.Fatalf("redeemRetryTargetsRow(%q,%q) = %v, want %v", testCase.status, testCase.mailError, got, testCase.want)
		}
	}
}

// TestCdkRewardModelShowsKind：奖励列表要能区分物品行、点券行与黑钻行。
func TestCdkRewardModelShowsKind(t *testing.T) {
	cdkWidgets.rewardRows = []cdkRewardRow{
		{Kind: cdkRewardKindCera, Count: "5000"},
		{Kind: cdkRewardKindItem, ItemID: "1001", Count: "2"},
		{Kind: cdkRewardKindBlackDiamond, Count: "30"},
	}
	defer func() { cdkWidgets.rewardRows = nil }()
	model := &cdkRewardModel{}
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
	if got := model.Value(1, 1); got != "物品" {
		t.Fatalf("第 2 行类型 = %v, want 物品", got)
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
	// 越界取值不能 panic（表格刷新时行数会短暂不一致）。
	if got := model.Value(5, 0); got != nil {
		t.Fatalf("越界行 = %v, want nil", got)
	}
	if got := model.Value(0, 99); got != "" {
		t.Fatalf("越界列 = %v, want 空", got)
	}
}
