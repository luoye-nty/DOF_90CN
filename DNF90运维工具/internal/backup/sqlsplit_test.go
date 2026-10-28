package backup

import (
	"archive/zip"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStmtLimitFor(t *testing.T) {
	cases := []struct {
		serverMax int64
		want      int
	}{
		{0, stmtLimitDefault},     // 拿不到服务端值
		{-1, stmtLimitDefault},    // 异常值
		{128 << 10, stmtLimitMin}, // 服务端很小：夹到下限
		{4 << 20, 2 << 20},        // 一半
		{64 << 20, stmtLimitMax},  // 一半超过封顶：取封顶
		{512 << 20, stmtLimitMax}, // 同上
		{200 << 10, 100 << 10},    // 正常区间
	}
	for _, c := range cases {
		if got := stmtLimitFor(c.serverMax); got != c.want {
			t.Errorf("stmtLimitFor(%d) = %d, want %d", c.serverMax, got, c.want)
		}
	}
}

func TestSplitStatements(t *testing.T) {
	cases := []struct {
		name string
		sql  string
		want int
	}{
		{"两条普通语句", "CREATE TABLE `a` (id INT);\nINSERT INTO `a` VALUES (1);", 2},
		{"字符串里的分号", "INSERT INTO t VALUES ('a;b');", 1},
		{"转义单引号", `INSERT INTO t VALUES ('it\'s;x');`, 1},
		{"双写单引号", "INSERT INTO t VALUES ('it''s;x');", 1},
		{"双引号字符串", `INSERT INTO t VALUES ("a;b");`, 1},
		{"反引号标识符里的分号", "CREATE TABLE `a;b` (i INT);", 1},
		{"行注释里的分号", "-- TABLE: x;\nCREATE TABLE t (i INT);", 1},
		{"井号注释里的分号", "# hi; there\nSELECT 1;", 1},
		{"块注释里的分号", "CREATE /* ; */ TABLE t (i INT);", 1},
		{"十六进制字面量", "INSERT INTO t VALUES (X'41423B');", 1},
		{"结尾无分号", "SELECT 1", 1},
		{"空白段落", "   \n\t ", 0},
		{"注释行结尾的分号不算语句", "-- DNF90 MySQL backup;\n\n-- TABLE: t\nSELECT 1;", 1},
	}
	for _, c := range cases {
		if got := splitStatements(c.sql); len(got) != c.want {
			t.Errorf("%s: got %d 条 %q, want %d", c.name, len(got), got, c.want)
		}
	}
}

// buildBigInsert 造一条多值 INSERT。
func buildBigInsert(rows int, value string) string {
	var b strings.Builder
	b.WriteString("INSERT INTO `big` (`id`,`name`) VALUES ")
	for i := 0; i < rows; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString("(")
		b.WriteString(strings.TrimSpace(strings.ReplaceAll(string(rune('0'+i%10)), " ", "")))
		b.WriteString(",'")
		b.WriteString(value)
		b.WriteString("')")
	}
	b.WriteString(";")
	return b.String()
}

func TestSplitOversizedInsert(t *testing.T) {
	const rows = 200
	const limit = 512
	stmt := buildBigInsert(rows, "abcdefghij")

	batches := splitOversizedInsert(stmt, limit)
	if batches == nil {
		t.Fatalf("应当能切分，却返回 nil")
	}
	if len(batches) < 2 {
		t.Fatalf("应当切成多批，实际 %d 批", len(batches))
	}
	total := 0
	for i, b := range batches {
		if len(b) > limit {
			t.Errorf("第 %d 批 %d 字节，超过上限 %d", i, len(b), limit)
		}
		if !strings.HasPrefix(b, "INSERT INTO `big` (`id`,`name`) VALUES (") {
			t.Errorf("第 %d 批前缀不对: %q", i, b[:60])
		}
		if !strings.HasSuffix(b, ";") {
			t.Errorf("第 %d 批没有以分号结尾", i)
		}
		total += strings.Count(b, "'),(") + 1 // 行数
	}
	if total != rows {
		t.Errorf("切分后总行数 = %d, want %d", total, rows)
	}
	// 值里含 "),(" 也不能被误切
	stmt2 := buildBigInsert(30, "a),(")
	b2 := splitOversizedInsert(stmt2, limit)
	if b2 == nil {
		t.Fatalf("值里含 ),( 时也应当能切分")
	}
	sum := 0
	for _, b := range b2 {
		if len(b) > limit {
			t.Errorf("含 ),( 的批次超限: %d > %d", len(b), limit)
		}
		sum += strings.Count(b, "'),(") + 1
	}
	if sum != 30 {
		t.Errorf("含 ),( 时切分后行数 = %d, want 30", sum)
	}
}

func TestSplitOversizedInsertSingleRowTooBig(t *testing.T) {
	huge := strings.Repeat("x", 5000)
	stmt := "INSERT INTO `big` (`a`) VALUES ('" + huge + "');"
	if got := splitOversizedInsert(stmt, 512); got != nil {
		t.Fatalf("单行超限应当返回 nil，实际 %d 批", len(got))
	}
}

func TestSplitOversizedInsertNonInsert(t *testing.T) {
	ddl := "CREATE TABLE `a` (" + strings.Repeat("`c` INT,", 200) + "`z` INT)"
	if got := splitOversizedInsert(ddl, 64); got != nil {
		t.Fatalf("非 INSERT 语句应当返回 nil")
	}
}

func TestPlanStatements(t *testing.T) {
	const limit = 600
	section := "DROP TABLE IF EXISTS `big`;\n" +
		"CREATE TABLE `big` (`id` INT, `name` TEXT);\n" +
		buildBigInsert(150, "abcdefghij")
	plan, err := planStatements(section, limit)
	if err != nil {
		t.Fatalf("planStatements 报错: %v", err)
	}
	if len(plan) < 3 {
		t.Fatalf("应当拆成多条（DROP + CREATE + 多批 INSERT），实际 %d 条", len(plan))
	}
	if !strings.HasPrefix(plan[0], "DROP TABLE") {
		t.Errorf("第一条应当是 DROP，实际 %q", plan[0][:40])
	}
	if !strings.HasPrefix(plan[1], "CREATE TABLE") {
		t.Errorf("第二条应当是 CREATE，实际 %q", plan[1][:40])
	}
	for i, s := range plan {
		if len(s) > limit {
			t.Errorf("第 %d 条 %d 字节超过上限 %d", i, len(s), limit)
		}
	}
}

func TestPlanStatementsErrorWhenUnsplitable(t *testing.T) {
	section := "INSERT INTO `big` (`a`) VALUES ('" + strings.Repeat("x", 5000) + "');"
	_, err := planStatements(section, 512)
	if err == nil {
		t.Fatal("单行超限应当返回错误")
	}
	if !strings.Contains(err.Error(), "max_allowed_packet") {
		t.Errorf("错误提示应当告诉用户调 max_allowed_packet，实际: %v", err)
	}
}

// TestDecodeMetaLightOldFormat 验证：旧备份（source_ddl 里塞着整表 SQL）也能正确读出，
// 且不会把那份内容留在内存里（这里只断言字段正确、SourceDDL 为空）。
func TestDecodeMetaLightOldFormat(t *testing.T) {
	dir := t.TempDir()
	zipPath := filepath.Join(dir, "old.zip")
	f, err := os.Create(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	mw, err := zw.Create(MetaEntryName)
	if err != nil {
		t.Fatal(err)
	}
	// 模拟旧版：source_ddl 里是整表段落（这里用 2 MB 代表）
	bigSection := strings.Repeat("INSERT INTO `t` VALUES (1);", 80000)
	meta := map[string]any{
		"source_type": "mysql",
		"version":     "1.0",
		"exported_at": "2026-09-27T09:54:00+08:00",
		"tables": []map[string]any{
			{"name": "dnf_quest_state_extra", "row_count": 33953, "source_ddl": bigSection},
			{"name": "dnf_characters", "row_count": 44, "source_ddl": "CREATE TABLE `dnf_characters` (`id` INT)"},
		},
	}
	raw, err := json.Marshal(meta)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mw.Write(raw); err != nil {
		t.Fatal(err)
	}
	sw, err := zw.Create(SqlEntryName)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sw.Write([]byte("-- TABLE: t\nINSERT INTO `t` VALUES (1);\n")); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	m, err := ReadMeta(zipPath)
	if err != nil {
		t.Fatalf("ReadMeta 失败: %v", err)
	}
	if m.SourceType != "mysql" || m.Version != "1.0" {
		t.Errorf("meta 头部读错: %+v", m)
	}
	if len(m.Tables) != 2 || m.Tables[0].Name != "dnf_quest_state_extra" || m.Tables[0].RowCount != 33953 {
		t.Errorf("表信息读错: %+v", m.Tables)
	}
	for _, tb := range m.Tables {
		if tb.SourceDDL != "" {
			t.Errorf("旧备份的 source_ddl 不应驻留内存，实际长度 %d", len(tb.SourceDDL))
		}
	}
}

// TestSplitStatementsThenPlanRoundTrip 用"导出→还原规划"完整走一遍：
// 老格式（整表一条 INSERT）+ 新格式（分批 INSERT）都必须能被规划成不超过上限的语句。
func TestSplitStatementsThenPlanRoundTrip(t *testing.T) {
	const limit = 1024
	oldFormat := "-- TABLE: t\nDROP TABLE IF EXISTS `t`;\nCREATE TABLE `t` (`a` INT);\n" +
		buildBigInsert(300, "0123456789") + "\n"
	newFormat := "-- TABLE: t\nDROP TABLE IF EXISTS `t`;\nCREATE TABLE `t` (`a` INT);\n" +
		buildBigInsert(100, "0123456789") + "\n" + buildBigInsert(100, "0123456789") + "\n"
	for name, content := range map[string]string{"老格式": oldFormat, "新格式": newFormat} {
		secs := splitSections(content)
		if len(secs) != 1 {
			t.Fatalf("%s: 段落数 = %d, want 1", name, len(secs))
		}
		plan, err := planStatements(secs[0].SQL, limit)
		if err != nil {
			t.Fatalf("%s: planStatements 失败: %v", name, err)
		}
		rows := 0
		for _, s := range plan {
			if len(s) > limit {
				t.Errorf("%s: 语句超限 %d > %d", name, len(s), limit)
			}
			rows += strings.Count(s, "'),(") + strings.Count(s, "),(") + 1
			if strings.HasPrefix(s, "INSERT") {
				// 只统计 INSERT 行数（DROP/CREATE 不含 ),( ）
			}
		}
		if rows < 300 {
			t.Errorf("%s: 规划后行数疑似丢失，统计到 %d", name, rows)
		}
	}
}
