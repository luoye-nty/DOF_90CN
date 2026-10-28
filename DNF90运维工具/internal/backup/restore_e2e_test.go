package backup

import (
	"archive/zip"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"dnf90ops/internal/config"
)

// 这是对 2026-09-27 线上故障的端到端回归：
//
//	还原一张"单条 INSERT 超过驱动 64 MiB 客户端上限"的大表，旧实现会直接拒发
//	（packet for query is too large）并把表留在被 DROP 掉的状态；新实现按语句/按行分批，
//	必须能还原成功。
//
// 默认跳过（需要一台可写的 MySQL）：设置以下环境变量后运行即可：
//
//	DNF_OPS_E2E=1
//	DNF_OPS_E2E_PASSWORD=<root 密码>
//	可选：DNF_OPS_E2E_HOST(127.0.0.1) DNF_OPS_E2E_PORT(13306) DNF_OPS_E2E_USER(root)
//
//	go test ./internal/backup/ -run TestRestoreOversizedTableE2E -v -timeout 20m
func TestRestoreOversizedTableE2E(t *testing.T) {
	if os.Getenv("DNF_OPS_E2E") == "" {
		t.Skip("未设置 DNF_OPS_E2E=1，跳过需要 MySQL 的端到端用例")
	}
	password := os.Getenv("DNF_OPS_E2E_PASSWORD")
	if password == "" {
		t.Skip("未设置 DNF_OPS_E2E_PASSWORD，跳过")
	}
	host := envOr("DNF_OPS_E2E_HOST", "127.0.0.1")
	port := envOr("DNF_OPS_E2E_PORT", "13306")
	user := envOr("DNF_OPS_E2E_USER", "root")
	const dbName = "dnf_backup_e2e"

	rootDSN := fmt.Sprintf("%s:%s@tcp(%s:%s)/?charset=utf8mb4&parseTime=true", user, password, host, port)
	root, err := sql.Open("mysql", rootDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := root.Ping(); err != nil {
		t.Fatalf("连接 MySQL 失败: %v", err)
	}
	if _, err := root.Exec("CREATE DATABASE IF NOT EXISTS " + dbName + " DEFAULT CHARSET utf8mb4"); err != nil {
		t.Fatalf("建测试库失败: %v", err)
	}
	defer func() { _, _ = root.Exec("DROP DATABASE IF EXISTS " + dbName) }()

	cfg := config.MySQLConfig{Host: host, Port: atoi(port), User: user, Password: password, DBName: dbName}
	dsnTarget := fmt.Sprintf("%s:%s@tcp(%s:%s)/%s?charset=utf8mb4&parseTime=true&multiStatements=true",
		user, password, host, port, dbName)
	db, err := sql.Open("mysql", dsnTarget)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// 造一张大表：300 行 × 200 KB ≈ 60 MB 原始数据；导出成 X'hex' 后约 120 MB，
	// 单条 INSERT 必然超过驱动默认的 64 MiB 上限（复现线上现场）。
	const rowCount = 300
	const payloadBytes = 200000
	if _, err := db.Exec("DROP TABLE IF EXISTS `big`"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("CREATE TABLE `big` (`id` INT NOT NULL, `payload` LONGTEXT NOT NULL, PRIMARY KEY (`id`)) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4"); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= rowCount; i++ {
		if _, err := db.Exec("INSERT INTO `big` (`id`,`payload`) VALUES (?, REPEAT('a', ?))", i, payloadBytes); err != nil {
			t.Fatalf("插入测试数据失败: %v", err)
		}
	}

	dir := t.TempDir()
	logf := func(s string) { t.Log(s) }
	res, err := DumpMySQL(cfg, dir, logf)
	if err != nil {
		t.Fatalf("DumpMySQL 失败: %v", err)
	}
	if res.RowCount != rowCount {
		t.Fatalf("导出行数 = %d, want %d", res.RowCount, rowCount)
	}

	// 1) 新备份：meta.json 必须很小（旧版会把整表 SQL 塞进 source_ddl，和 dump.sql 一样大）
	metaSize, sqlSize := entrySizes(t, res.ZipPath)
	if metaSize > 256<<10 {
		t.Errorf("meta.json = %d 字节，仍然过大（应当只存建表语句）", metaSize)
	}
	t.Logf("新备份：dump.sql=%s meta.json=%s", fmtSize(sqlSize), fmtSize(metaSize))
	if sqlSize < 64<<20 {
		t.Fatalf("dump.sql 只有 %s，没造出「超过 64 MiB 单语句」的现场，用例无效", fmtSize(sqlSize))
	}

	// 2) 新备份里的表段落应当是**多条** INSERT（导出侧分批）
	newSQL := readZipEntry(t, res.ZipPath, SqlEntryName)
	secs := splitSections(string(newSQL))
	if len(secs) != 1 {
		t.Fatalf("段落数 = %d, want 1", len(secs))
	}
	newPlan, err := planStatements(secs[0].SQL, stmtLimitFor(64<<20))
	if err != nil {
		t.Fatalf("规划新备份失败: %v", err)
	}
	insertStmts := 0
	for _, s := range newPlan {
		if strings.HasPrefix(strings.ToUpper(strings.TrimSpace(s)), "INSERT INTO") {
			insertStmts++
		}
	}
	if insertStmts < 2 {
		t.Errorf("新备份的表段落里 INSERT 语句只有 %d 条，导出侧没分批", insertStmts)
	}
	t.Logf("新备份：段落被规划成 %d 条语句（其中 INSERT %d 条），最大 %s",
		len(newPlan), insertStmts, fmtBytes(maxLen(newPlan)))

	// 3) 复现旧行为：整段一个包 → 必须被驱动拒发（证明现场有效）
	if _, err := db.Exec(secs[0].SQL); err == nil {
		t.Fatalf("整段 Exec 竟然成功了 —— 现场没造出来，用例无效（段落只有 %s）", fmtBytes(len(secs[0].SQL)))
	} else {
		t.Logf("复现旧失败：整段 Exec -> %v", err)
	}

	// 4) 老格式备份（整表一条 INSERT + meta.json 里塞整段 SQL）也必须能还原
	oldZip := filepath.Join(dir, "old_format.zip")
	writeOldFormatZip(t, oldZip, secs[0], rowCount)
	oldRes, err := RestoreMySQL(cfg, oldZip, logf)
	if err != nil {
		t.Fatalf("老格式备份还原失败（这正是线上那个包）：%v", err)
	}
	assertBigTable(t, db, rowCount, payloadBytes, oldRes)

	// 5) 新格式备份还原
	if _, err := db.Exec("DROP TABLE IF EXISTS `big`"); err != nil {
		t.Fatal(err)
	}
	newRes, err := RestoreMySQL(cfg, res.ZipPath, logf)
	if err != nil {
		t.Fatalf("新格式备份还原失败: %v", err)
	}
	assertBigTable(t, db, rowCount, payloadBytes, newRes)
}

func assertBigTable(t *testing.T, db *sql.DB, rowCount, payloadBytes int, res Result) {
	t.Helper()
	if res.TableCount == 0 {
		t.Errorf("还原表数 = 0")
	}
	var n int
	if err := db.QueryRow("SELECT COUNT(*) FROM `big`").Scan(&n); err != nil {
		t.Fatalf("还原后查行数失败: %v", err)
	}
	if n != rowCount {
		t.Errorf("还原后行数 = %d, want %d", n, rowCount)
	}
	var bad int
	if err := db.QueryRow("SELECT COUNT(*) FROM `big` WHERE CHAR_LENGTH(`payload`) <> ?", payloadBytes).Scan(&bad); err != nil {
		t.Fatal(err)
	}
	if bad != 0 {
		t.Errorf("有 %d 行的内容长度不对", bad)
	}
}

// writeOldFormatZip 造一份"旧版工具"格式的备份包：
// dump.sql 每张表只有**一条** INSERT，meta.json 的 source_ddl 里塞着整段 SQL。
func writeOldFormatZip(t *testing.T, zipPath string, sec section, rowCount int) {
	t.Helper()
	stmts := splitStatements(sec.SQL)
	var dropCreate []string
	var insertHead string
	var rowParts []string
	for _, s := range stmts {
		if strings.HasPrefix(strings.ToUpper(strings.TrimSpace(s)), "INSERT INTO") {
			v := indexTopLevelKeyword(s, "VALUES")
			if v < 0 {
				t.Fatalf("测试数据异常：INSERT 里找不到 VALUES")
			}
			if insertHead == "" {
				insertHead = s[:v+len("VALUES")]
			}
			rowParts = append(rowParts, strings.TrimSuffix(strings.TrimSpace(s[v+len("VALUES"):]), ";"))
			continue
		}
		dropCreate = append(dropCreate, s+";")
	}
	single := insertHead + " " + strings.Join(rowParts, ",") + ";"
	oldSection := strings.Join(dropCreate, "\n") + "\n" + single
	oldDump := "\n" + TableMarker + sec.Name + "\n" + oldSection + "\n"

	f, err := os.Create(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	sw, err := zw.Create(SqlEntryName)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sw.Write([]byte(oldDump)); err != nil {
		t.Fatal(err)
	}
	mw, err := zw.Create(MetaEntryName)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(map[string]any{
		"source_type": "mysql",
		"version":     Version,
		"exported_at": "2026-09-27T09:54:00+08:00",
		"tables": []map[string]any{
			{"name": sec.Name, "row_count": rowCount, "source_ddl": oldSection}, // 旧版：整段 SQL
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mw.Write(raw); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func entrySizes(t *testing.T, zipPath string) (metaSize, sqlSize int64) {
	t.Helper()
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	for _, f := range zr.File {
		switch f.Name {
		case MetaEntryName:
			metaSize = int64(f.UncompressedSize64)
		case SqlEntryName:
			sqlSize = int64(f.UncompressedSize64)
		}
	}
	return metaSize, sqlSize
}

func readZipEntry(t *testing.T, zipPath, name string) []byte {
	t.Helper()
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	for _, f := range zr.File {
		if f.Name != name {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		defer rc.Close()
		data, err := io.ReadAll(rc)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	t.Fatalf("zip 内没有 %s", name)
	return nil
}

func maxLen(list []string) int {
	m := 0
	for _, s := range list {
		if len(s) > m {
			m = len(s)
		}
	}
	return m
}

func fmtSize(n int64) string { return fmtBytes(int(n)) }

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func atoi(s string) int {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return n
		}
		n = n*10 + int(c-'0')
	}
	return n
}
