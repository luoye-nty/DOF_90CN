//go:build paid

// 群发邮件「按角色 ID 顺序发放」的纯逻辑测试。
//
// 为什么值得钉住：
//   - 顺序发放的意义就是"顺序固定、可复现"，排序写错（比如按字符串比 "100" < "19"）
//     会让日志里的顺序与在线列表对不上，排查时又得多绕一圈；
//   - 非数字 ID 必须仍然保留在队列里（宁可排最后，也不能丢目标）。

package manage

import "testing"

func TestSortBatchTargetsByCharacterID(t *testing.T) {
	targets := []batchMailTarget{
		{CharacterID: "19", CharacterName: "c19"},
		{CharacterID: "3", CharacterName: "c3"},
		{CharacterID: "100", CharacterName: "c100"},
		{CharacterID: "", CharacterName: "空ID"},
		{CharacterID: "abc", CharacterName: "非数字"},
		{CharacterID: " 7 ", CharacterName: "带空格"},
	}
	sortBatchTargetsByCharacterID(targets)

	// 数字 ID 按数值升序在前（3 < 7 < 19 < 100 —— 不是字符串序），
	// 非数字放最后（空串在 "abc" 前），一个都不能丢。
	want := []string{"3", " 7 ", "19", "100", "", "abc"}
	if len(targets) != len(want) {
		t.Fatalf("目标数 = %d，期望 %d（不能丢目标）", len(targets), len(want))
	}
	for i, expected := range want {
		if targets[i].CharacterID != expected {
			got := make([]string, 0, len(targets))
			for _, item := range targets {
				got = append(got, item.CharacterID)
			}
			t.Fatalf("第 %d 个 = %q，期望 %q（实际顺序 %v）", i, targets[i].CharacterID, expected, got)
		}
	}
}

func TestSortBatchTargetsStableForSameID(t *testing.T) {
	// 同 ID（理论上不该有）时保持原有顺序，避免同一账号多角色时顺序乱跳。
	targets := []batchMailTarget{
		{CharacterID: "5", Username: "先"},
		{CharacterID: "5", Username: "后"},
	}
	sortBatchTargetsByCharacterID(targets)
	if targets[0].Username != "先" || targets[1].Username != "后" {
		t.Fatalf("同 ID 应保持稳定顺序，实际 %q / %q", targets[0].Username, targets[1].Username)
	}
}
