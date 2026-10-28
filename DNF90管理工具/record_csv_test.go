package main

// CSV 导出的账号列口径。
//
// 导出的表是运营拿去核对、转发、做周报的东西：里面绝不能出现不可逆的
// dnf:<sha256(用户名)> 主键。这里锁两条：
//   - 翻译到 → 写真实账号名；
//   - 翻译不到 → 原样写主键（不能变成空白，否则那行记录等于丢了账号信息）。

import (
	"encoding/csv"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRecCSVExportsRealAccountName(t *testing.T) {
	path := filepath.Join(t.TempDir(), "records.csv")
	rows := []recRow{
		{
			record:      map[string]any{"account_id": "dnf:abc", "character_name": "角色A", "cera_spent": 100},
			accountName: "1000000001",
		},
		{
			record:      map[string]any{"account_id": "wpf-legacy", "character_name": "角色B"},
			accountName: "", // 翻译不到
		},
	}
	if err := recWriteCSV(path, rows); err != nil {
		t.Fatalf("写 CSV 失败: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读回 CSV 失败: %v", err)
	}
	// 去掉 BOM 后用标准 csv 解析，逐单元格断言（避免"子串恰好出现"的假通过）。
	text := strings.TrimPrefix(string(raw), "\xEF\xBB\xBF")
	parsed, err := csv.NewReader(strings.NewReader(text)).ReadAll()
	if err != nil {
		t.Fatalf("解析 CSV 失败: %v", err)
	}
	if len(parsed) != 3 {
		t.Fatalf("CSV 行数 = %d，期望 3（表头 + 2 行）", len(parsed))
	}
	idx := -1
	for i, name := range parsed[0] {
		if name == "account_id" {
			idx = i
		}
	}
	if idx < 0 {
		t.Fatalf("表头里没有 account_id 列：%v", parsed[0])
	}
	if got := parsed[1][idx]; got != "1000000001" {
		t.Fatalf("账号列 = %q，期望真实账号名 1000000001", got)
	}
	if got := parsed[2][idx]; got != "wpf-legacy" {
		t.Fatalf("翻译不到的账号列 = %q，期望原样保留主键", got)
	}
	for _, line := range parsed {
		for _, cell := range line {
			if strings.HasPrefix(cell, "dnf:") {
				t.Fatalf("导出的 CSV 里不该出现账号主键，实际有 %q", cell)
			}
		}
	}
}
