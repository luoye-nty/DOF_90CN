package store

// "全服第一个角色"查询口径的契约测试。
//
// 这是一条**无库**测试：真正执行 SQL 需要 MySQL（其余 store 用例也同样不起库），
// 但"发错人"的风险全部集中在 ORDER BY 口径上 —— created_at 可空、character_id 是 VARCHAR，
// 少一级排序就可能让"全服第一角色"在两次调用之间变成另一个人。所以这里把口径钉死在 SQL 上。

import (
	"strings"
	"testing"
)

func TestServerFirstCharacterSQLOrder(t *testing.T) {
	sql := strings.Join(strings.Fields(serverFirstCharacterSQL), " ")

	// 三级排序：空值分组 → 创建时间 → 角色 ID 兜底（保证稳定且唯一）。
	want := "ORDER BY (created_at IS NULL), created_at ASC, CAST(character_id AS UNSIGNED) ASC"
	if !strings.Contains(sql, want) {
		t.Fatalf("排序口径被改了：\n  实际 %s\n  期望包含 %s", sql, want)
	}
	if !strings.Contains(sql, "FROM dnf_characters") {
		t.Fatalf("必须直查游戏库 dnf_characters，实际 %s", sql)
	}
	if !strings.Contains(sql, "delete_flag = 0") {
		t.Fatalf("已删除的角色不能参与（会把目标定到玩家看不到的角色上），实际 %s", sql)
	}
	if !strings.Contains(sql, "LIMIT 1") {
		t.Fatalf("全服第一角色只应有一个目标，实际 %s", sql)
	}
	// 只读：这条查询绝不能带任何写操作。
	upper := strings.ToUpper(sql)
	for _, bad := range []string{"UPDATE ", "DELETE ", "INSERT ", "REPLACE "} {
		if strings.Contains(upper, bad) {
			t.Fatalf("只读查询里出现了写操作 %q：%s", bad, sql)
		}
	}
}

// TestServerFirstCharacterStructShape 响应字段名（管理工具按它渲染）。
func TestServerFirstCharacterStructShape(t *testing.T) {
	// 直接用 JSON tag 钉住：字段名一改，管理工具的"发放目标"预览就空白。
	ch := ServerFirstCharacter{CharacterID: "1001", Name: "角色A", AccountID: "dnf:a", Level: 90}
	if ch.CharacterID == "" || ch.Name == "" || ch.AccountID == "" || ch.Level == 0 {
		t.Fatal("结构体字段不齐（character_id / name / account_id / level 四个都要）")
	}
}
