package main

// 记录查询"汉化 + 账号名 + 通关用时"的纯逻辑测试（不建窗口、不发请求）。
//
// 为什么值得钉住：
//   - 账号翻译来自运维工具批量返回的 map，翻译不到时必须**退回显示原主键**，
//     否则运营会看到一行空白，以为记录坏了；
//   - 通关用时是排查外挂的关键指标，秒→分秒的换算写错会直接误导判断。

import (
	"strings"
	"testing"
)

func TestRecFieldLabel(t *testing.T) {
	if got := recFieldLabel("elapsed_seconds"); got != "通关用时" {
		t.Fatalf("elapsed_seconds 标签 = %q，期望 通关用时", got)
	}
	if got := recFieldLabel("cera_spent"); got != "实扣点券" {
		t.Fatalf("cera_spent 标签 = %q，期望 实扣点券", got)
	}
	// 未收录字段保留原名并标注，便于把缺的字段报回来补字典。
	if got := recFieldLabel("some_new_field"); !strings.Contains(got, "some_new_field") {
		t.Fatalf("未收录字段应保留原名，实际 %q", got)
	}
}

func TestRecDurationText(t *testing.T) {
	cases := []struct {
		seconds float64
		want    string
	}{
		{0, "0 秒"},
		{45, "45 秒"},
		{59, "59 秒"},
		{60, "1 分（60 秒）"},
		{192, "3 分 12 秒（192 秒）"},
		{-5, "0 秒"},
	}
	for _, tc := range cases {
		if got := recDurationText(tc.seconds); got != tc.want {
			t.Fatalf("recDurationText(%v) = %q，期望 %q", tc.seconds, got, tc.want)
		}
	}
}

func TestRecFieldValueTextAccount(t *testing.T) {
	names := map[string]string{"dnf:abc": "1000000001"}

	// 账号列**只显示真实账号名**：主键（dnf:<sha256>）不可逆、运营看不懂，
	// 以前那种 "名字（原主键 dnf:…）" 的写法仍然把哈希摆在界面上，已按需求去掉。
	got := recFieldValueText("account_id", "dnf:abc", names)
	if got != "1000000001" {
		t.Fatalf("账号应只显示真实账号名，实际 %q", got)
	}

	// 翻译不到：必须原样显示主键 + 标注，不能变成空白，也不能假装它没有主键。
	got = recFieldValueText("account_id", "wpf-unknown", names)
	if !strings.Contains(got, "wpf-unknown") || !strings.Contains(got, "未匹配") {
		t.Fatalf("翻译不到时应显示原主键+标注，实际 %q", got)
	}
}

func TestRecFieldValueTextBasics(t *testing.T) {
	if got := recFieldValueText("elapsed_seconds", float64(192), nil); got != "3 分 12 秒（192 秒）" {
		t.Fatalf("elapsed_seconds(数字) = %q", got)
	}
	if got := recFieldValueText("elapsed_seconds", "192", nil); got != "3 分 12 秒（192 秒）" {
		t.Fatalf("elapsed_seconds(字符串) = %q", got)
	}
	if got := recFieldValueText("cleared", true, nil); got != "是" {
		t.Fatalf("cleared=true 应显示 是，实际 %q", got)
	}
	if got := recFieldValueText("cleared", false, nil); got != "否" {
		t.Fatalf("cleared=false 应显示 否，实际 %q", got)
	}
	if got := recFieldValueText("result", "ok", nil); got != "成功" {
		t.Fatalf("result=ok 应显示 成功，实际 %q", got)
	}
	if got := recFieldValueText("result", "rejected", nil); got != "被拒绝" {
		t.Fatalf("result=rejected 应显示 被拒绝，实际 %q", got)
	}
	// 普通字符串原样返回（不做多余加工）。
	if got := recFieldValueText("dungeon_name", "王的遗迹", nil); got != "王的遗迹" {
		t.Fatalf("普通字段应原样返回，实际 %q", got)
	}
}

func TestBuildRecRowsUsesAccountNames(t *testing.T) {
	page := &RecordPage{
		Records: []map[string]any{
			{"kind": "dungeon", "account_id": "dnf:abc", "character_name": "2222", "elapsed_seconds": float64(192)},
			{"kind": "dungeon", "account_id": "wpf-x"},
		},
		AccountNames: map[string]string{"dnf:abc": "1000000001"},
	}
	rows := buildRecRows(page, "dungeon")
	if len(rows) != 2 {
		t.Fatalf("行数 = %d，期望 2", len(rows))
	}
	if rows[0].accountName != "1000000001" {
		t.Fatalf("第 1 行账号名 = %q，期望 1000000001", rows[0].accountName)
	}
	if rows[1].accountName != "" {
		t.Fatalf("翻译不到的行应留空（由表格退回主键），实际 %q", rows[1].accountName)
	}
	// 表格取值：有账号名用账号名，没有则退回主键。
	model := &recTableModel{rows: rows}
	if got := model.Value(0, 3); got != "1000000001" {
		t.Fatalf("表格第 1 行账号列 = %v，期望 1000000001", got)
	}
	if got := model.Value(1, 3); got != "wpf-x" {
		t.Fatalf("表格第 2 行账号列应退回主键，实际 %v", got)
	}
}

func TestRecDetailTextLocalizesFields(t *testing.T) {
	entry := map[string]any{
		"account_id":      "dnf:abc",
		"elapsed_seconds": float64(192),
		"cleared":         true,
		"some_new_field":  "x",
	}
	text := recDetailText(entry, map[string]string{"dnf:abc": "1000000001"})
	for _, want := range []string{"账号 = 1000000001", "通关用时 = 3 分 12 秒", "是否通关 = 是", "some_new_field（未汉化） = x"} {
		if !strings.Contains(text, want) {
			t.Fatalf("详情区缺少 %q\n实际内容:\n%s", want, text)
		}
	}
}
