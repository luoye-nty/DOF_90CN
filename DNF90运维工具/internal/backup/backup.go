// Package backup 实现 DNF90 服务端 MySQL 数据库备份/恢复。
// 格式与登录器 DbDumpService 对齐：zip 内含 meta.json + dump.sql；
// dump.sql 每表一段，段首为 "-- TABLE: <name>"，段内 DROP + CREATE + INSERT。
package backup

import (
	"archive/zip"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	_ "github.com/go-sql-driver/mysql"

	"dnf90ops/internal/config"
)

const (
	MetaEntryName = "meta.json"
	SqlEntryName  = "dump.sql"
	Version       = "1.0"
	TableMarker   = "-- TABLE: "
)

// TableMeta 单表元信息（与登录器 DbDumpTableMeta 对齐）。
type TableMeta struct {
	Name      string `json:"name"`
	SourceDDL string `json:"source_ddl"`
	RowCount  int64  `json:"row_count"`
}

// Meta zip 内 meta.json 结构。
type Meta struct {
	SourceType string      `json:"source_type"`
	Version    string      `json:"version"`
	ExportedAt string      `json:"exported_at"`
	Tables     []TableMeta `json:"tables"`
}

// Result 导出结果摘要。
type Result struct {
	TableCount int
	RowCount   int64
	ZipPath    string
}

// Entry 备份列表条目。
type Entry struct {
	Name     string    `json:"name"`
	Size     int64     `json:"size"`
	ModTime  time.Time `json:"mod_time"`
	File     string    `json:"file"`
	Meta     *Meta     `json:"meta,omitempty"`
	MetaRead bool      `json:"-"`
}

// NextRunAt 计算下一次自动备份时刻：以基准时刻为 T0，按 interval 小时步进，
// 返回第一个严格晚于 now 的 T0 + k*interval（无视跨天；进程重启相位不丢）。
func NextRunAt(baseAt time.Time, intervalHours int, now time.Time) time.Time {
	if intervalHours <= 0 {
		intervalHours = 1
	}
	step := time.Duration(intervalHours) * time.Hour
	next := baseAt.Add(step)
	for !next.After(now) {
		next = next.Add(step)
	}
	return next
}

// DefaultBackupDir 返回备份目录（调用方 exe 同目录 backups\）。
func DefaultBackupDir(exeDir string) string {
	return filepath.Join(exeDir, "backups")
}

// dsn 构造连接串；multi 用于恢复时执行含多条语句的段落。
func dsn(cfg config.MySQLConfig, multi bool) string {
	extra := "charset=utf8mb4&parseTime=true&loc=Local&timeout=5s&readTimeout=60s&writeTimeout=300s"
	if multi {
		extra += "&multiStatements=true"
	}
	return fmt.Sprintf("%s:%s@tcp(%s:%d)/%s?%s",
		cfg.User, cfg.Password, cfg.Host, cfg.Port, cfg.DBName, extra)
}

func quoteIdent(s string) string {
	return "`" + strings.ReplaceAll(s, "`", "``") + "`"
}

// escapeSQL 转义 MySQL 字符串字面量：\ 与 ' 双写。
func escapeSQL(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `'`, `''`)
	return s
}

// sqlLiteral 将扫描值格式化为 SQL 字面量。
func sqlLiteral(v any) string {
	switch t := v.(type) {
	case nil:
		return "NULL"
	case int64:
		return strconv.FormatInt(t, 10)
	case int:
		return strconv.Itoa(t)
	case float64:
		return strconv.FormatFloat(t, 'g', -1, 64)
	case bool:
		if t {
			return "1"
		}
		return "0"
	case []byte:
		return fmt.Sprintf("X'%x'", t)
	case string:
		return "'" + escapeSQL(t) + "'"
	case time.Time:
		return "'" + t.Format("2006-01-02 15:04:05.999999") + "'"
	default:
		// 其余类型转字符串处理，避免 panic 打断整库备份。
		return "'" + escapeSQL(fmt.Sprintf("%v", t)) + "'"
	}
}

// tableSchema 读单表原始 CREATE TABLE（SHOW CREATE TABLE 最保真）。
// SHOW CREATE TABLE 返回两列（Table, Create Table），必须两个都 Scan 再取第二列。
func tableSchema(db *sql.DB, name string) (string, error) {
	var table, ddl string
	err := db.QueryRow("SHOW CREATE TABLE "+quoteIdent(name)).Scan(&table, &ddl)
	if err != nil {
		return "", err
	}
	return ddl, nil
}

// 导出时每条 INSERT 的规模上限（2026-09-27 起分批写）：
// 原来整表只写一条 INSERT，大表会让 ① meta.json（旧版把整段塞进 source_ddl）与 dump.sql
// 体积翻倍，② 还原时单条语句超过驱动的 64 MiB 客户端上限直接拒发。
const (
	dumpRowsPerInsert = 500     // 每条 INSERT 最多行数
	dumpInsertBytes   = 1 << 20 // 每条 INSERT 的文本上限（约 1 MB）
)

// dumpTable 导出单表段落（DROP + CREATE + 分批 INSERT）。
// 返回：段落 SQL、建表语句（写进 meta.json 的 source_ddl，与登录器备份格式一致）、行数。
func dumpTable(db *sql.DB, name string) (string, string, int64, error) {
	ddl, err := tableSchema(db, name)
	if err != nil {
		return "", "", 0, fmt.Errorf("读取 %s 表结构失败: %w", name, err)
	}
	rows, err := db.Query("SELECT * FROM " + quoteIdent(name))
	if err != nil {
		return "", "", 0, fmt.Errorf("读取 %s 数据失败: %w", name, err)
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return "", "", 0, err
	}
	// JSON 列的值必须按字符串字面量导出：X'hex' 是 binary 字符集，MySQL JSON 列不接受（3144）。
	jsonCol := make([]bool, len(cols))
	if colTypes, err := rows.ColumnTypes(); err == nil {
		for i, ct := range colTypes {
			if strings.EqualFold(ct.DatabaseTypeName(), "JSON") {
				jsonCol[i] = true
			}
		}
	}
	colList := make([]string, len(cols))
	for i, c := range cols {
		colList[i] = quoteIdent(c)
	}
	head := "INSERT INTO " + quoteIdent(name) + " (" + strings.Join(colList, ",") + ") VALUES "
	values := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range values {
		ptrs[i] = &values[i]
	}
	var n int64
	var batch strings.Builder // 当前这条 INSERT（含前缀、不含结尾分号）
	var stmts []string        // 已写完的 INSERT 语句
	rowsInBatch := 0
	flush := func() {
		if rowsInBatch == 0 {
			return
		}
		stmts = append(stmts, strings.TrimSuffix(batch.String(), ",")+";")
		batch.Reset()
		rowsInBatch = 0
	}
	for rows.Next() {
		if err := rows.Scan(ptrs...); err != nil {
			return "", "", 0, fmt.Errorf("扫描 %s 行失败: %w", name, err)
		}
		var row strings.Builder
		row.WriteString("(")
		for i, v := range values {
			if i > 0 {
				row.WriteString(",")
			}
			if jsonCol[i] {
				if bv, ok := v.([]byte); ok {
					v = string(bv)
				}
			}
			row.WriteString(sqlLiteral(v))
		}
		row.WriteString(")")
		if rowsInBatch == 0 {
			batch.WriteString(head)
		} else {
			batch.WriteString(",")
		}
		batch.WriteString(row.String())
		rowsInBatch++
		n++
		if rowsInBatch >= dumpRowsPerInsert || batch.Len() >= dumpInsertBytes {
			flush()
		}
	}
	if err := rows.Err(); err != nil {
		return "", "", 0, fmt.Errorf("遍历 %s 失败: %w", name, err)
	}
	flush()
	if n == 0 {
		// 无数据也生成空 INSERT 占位无意义，直接不写 VALUES 段。
		return ddl, ddl, 0, nil
	}
	section := "DROP TABLE IF EXISTS " + quoteIdent(name) + ";\n" + ddl + ";\n" + strings.Join(stmts, "\n")
	return section, ddl, n, nil
}

// DumpMySQL 全库导出 zip（meta.json + dump.sql）。
func DumpMySQL(cfg config.MySQLConfig, dir string, logf func(string)) (Result, error) {
	var res Result
	db, err := sql.Open("mysql", dsn(cfg, false))
	if err != nil {
		return res, err
	}
	defer db.Close()
	if err := db.Ping(); err != nil {
		return res, fmt.Errorf("连接 MySQL(%s:%d/%s) 失败: %w", cfg.Host, cfg.Port, cfg.DBName, err)
	}
	rows, err := db.Query("SELECT table_name FROM information_schema.tables WHERE table_schema = DATABASE() AND table_type = 'BASE TABLE' ORDER BY table_name")
	if err != nil {
		return res, fmt.Errorf("枚举数据表失败: %w", err)
	}
	var tables []string
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			rows.Close()
			return res, err
		}
		tables = append(tables, t)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return res, err
	}
	if len(tables) == 0 {
		return res, errors.New("目标库中未发现数据表")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return res, err
	}
	meta := Meta{
		SourceType: "mysql",
		Version:    Version,
		ExportedAt: time.Now().Format(time.RFC3339),
	}
	var sqlBuf strings.Builder
	sqlBuf.WriteString("-- DNF90 MySQL backup, exported at " + meta.ExportedAt + "\n")
	for _, t := range tables {
		if logf != nil {
			logf("导出表 " + t + " ...")
		}
		section, ddl, n, err := dumpTable(db, t)
		if err != nil {
			return res, err
		}
		// source_ddl 只放建表语句（与登录器 DbDumpService 的格式一致）：旧版这里塞的是
		// 整张表的段落（含全部数据），导致 meta.json 和 dump.sql 一样大 —— 这次线上那个
		// 备份包就是 167 MB + 167 MB，还原时还要把两份都读进内存。
		meta.Tables = append(meta.Tables, TableMeta{Name: t, SourceDDL: ddl, RowCount: n})
		sqlBuf.WriteString("\n" + TableMarker + t + "\n")
		sqlBuf.WriteString(section)
		sqlBuf.WriteString("\n")
		res.RowCount += n
	}
	res.TableCount = len(tables)
	zipPath := filepath.Join(dir, "dnf90db_"+time.Now().Format("20060102_150405")+".zip")
	// 先写临时文件再改名，避免半成品。
	tmp := zipPath + ".tmp"
	if err := writeZip(tmp, meta, sqlBuf.String()); err != nil {
		return res, err
	}
	if err := os.Rename(tmp, zipPath); err != nil {
		os.Remove(tmp)
		return res, err
	}
	res.ZipPath = zipPath
	if logf != nil {
		logf(fmt.Sprintf("备份完成：%d 张表 %d 行 -> %s", res.TableCount, res.RowCount, zipPath))
	}
	return res, nil
}

func writeZip(zipPath string, meta Meta, sqlContent string) error {
	f, err := os.Create(zipPath)
	if err != nil {
		return err
	}
	defer f.Close()
	zw := zip.NewWriter(f)
	// dump.sql 写入时用 CRLF 与登录器/Windows 环境对齐。
	mw, err := zw.Create(SqlEntryName)
	if err != nil {
		return err
	}
	if _, err := io.WriteString(mw, sqlContent); err != nil {
		return err
	}
	mw2, err := zw.Create(MetaEntryName)
	if err != nil {
		return err
	}
	metaJSON, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return err
	}
	if _, err := mw2.Write(metaJSON); err != nil {
		return err
	}
	return zw.Close()
}

// PruneBackups 备份目录只保留最近 keep 个 zip，删掉更旧的；返回被删掉的文件名（新→旧顺序里的尾部）。
//
// 为什么需要它：备份文件不像日志那样有轮转，早先只增不减，定时备份会把磁盘写满。
// keep<=0 时用 DefaultKeep 兜底；删除失败不阻断备份，只把失败的文件名留在返回的错误里。
func PruneBackups(dir string, keep int) ([]string, error) {
	if keep <= 0 {
		keep = DefaultKeep
	}
	entries, err := ListBackups(dir)
	if err != nil {
		return nil, err
	}
	if len(entries) <= keep {
		return nil, nil
	}
	removed := make([]string, 0, len(entries)-keep)
	var firstErr error
	for _, e := range entries[keep:] {
		if err := os.Remove(e.File); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("删除旧备份 %s 失败: %w", e.Name, err)
			}
			continue
		}
		removed = append(removed, e.Name)
	}
	return removed, firstErr
}

// DefaultKeep 未配置保留份数时的默认值（与 config.AutoBackupKeepDefault 一致；
// 这里保留一份是为了 backup 包不反向依赖 config）。
const DefaultKeep = 20

// ListBackups 列出备份目录 zip，按修改时间倒序。
func ListBackups(dir string) ([]Entry, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return []Entry{}, nil
		}
		return nil, err
	}
	var out []Entry
	for _, e := range entries {
		if e.IsDir() || !strings.EqualFold(filepath.Ext(e.Name()), ".zip") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, Entry{
			Name:    e.Name(),
			Size:    info.Size(),
			ModTime: info.ModTime(),
			File:    filepath.Join(dir, e.Name()),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ModTime.After(out[j].ModTime) })
	return out, nil
}

// ReadMeta 读取 zip 内 meta.json（不存在或损坏时返回 nil，不阻断恢复）。
func ReadMeta(zipPath string) (*Meta, error) {
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	for _, f := range zr.File {
		if f.Name != MetaEntryName {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return nil, err
		}
		defer rc.Close()
		return decodeMetaLight(rc)
	}
	return nil, errors.New("备份包缺少 meta.json")
}

// metaLight 只保留需要的字段：**不含 source_ddl**。
// 为什么要这样：旧版工具把整张表的 SQL 段落写进了 source_ddl，meta.json 会有几百 MB；
// 直接 Decode 成 Meta 会把这份内容整段留在内存里（还要再复制一份）。
type metaLight struct {
	SourceType string `json:"source_type"`
	Version    string `json:"version"`
	ExportedAt string `json:"exported_at"`
	Tables     []struct {
		Name     string `json:"name"`
		RowCount int64  `json:"row_count"`
	} `json:"tables"`
}

// decodeMetaLight 流式解码 meta.json，只取 source_type/version/exported_at 与每表名/行数；
// source_ddl 被解码器跳过，不驻留内存。旧备份（source_ddl 里塞着整表 SQL）同样适用。
func decodeMetaLight(r io.Reader) (*Meta, error) {
	var l metaLight
	if err := json.NewDecoder(r).Decode(&l); err != nil {
		return nil, err
	}
	m := &Meta{SourceType: l.SourceType, Version: l.Version, ExportedAt: l.ExportedAt}
	for _, t := range l.Tables {
		m.Tables = append(m.Tables, TableMeta{Name: t.Name, RowCount: t.RowCount})
	}
	return m, nil
}

// ReadMetaEntry 便捷：Entry 附上 Meta（供列表展示表数/导出时间）。
func (e *Entry) ReadMetaEntry() {
	m, err := ReadMeta(e.File)
	if err == nil {
		e.Meta = m
	}
	e.MetaRead = true
}

// RestoreMySQL 用备份 zip 覆盖还原目标库。
// 逐表执行 dump.sql 段落（DROP + CREATE + INSERT），单表失败立即中断返回错误。
//
// 2026-09-27 起：段落先拆成**单条语句**再逐条发送，超大的多值 INSERT 再按行边界分批。
// 原来是把整段当一个包发（db.Exec(sec.SQL)），大表会超过 go-sql-driver 的客户端上限
// （默认 64 MiB）被直接拒发 —— 报错原文 "packet for query is too large. Try adjusting
// the `Config.MaxAllowedPacket`"，那是驱动报的、不是 MySQL 报的，调服务端也没用；
// 而且失败发生在包发出之前，表已被 DROP，库会停在半还原状态。
func RestoreMySQL(cfg config.MySQLConfig, zipPath string, logf func(string)) (Result, error) {
	var res Result
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return res, err
	}
	defer zr.Close()
	var sqlContent []byte
	var meta *Meta
	for _, f := range zr.File {
		switch f.Name {
		case SqlEntryName:
			rc, err := f.Open()
			if err != nil {
				return res, err
			}
			data, err := io.ReadAll(rc)
			rc.Close()
			if err != nil {
				return res, err
			}
			sqlContent = data
		case MetaEntryName:
			// 旧备份的 meta.json 可能与 dump.sql 一样大（旧版把整表 SQL 塞进了 source_ddl），
			// 这里流式解码且只取需要的字段，不把整份 JSON 读进内存。
			rc, err := f.Open()
			if err != nil {
				continue
			}
			if m, derr := decodeMetaLight(rc); derr == nil {
				meta = m
			}
			rc.Close()
		}
	}
	if len(sqlContent) == 0 {
		return res, errors.New("备份包缺少 dump.sql")
	}
	if meta != nil && meta.SourceType != "mysql" {
		return res, fmt.Errorf("备份源类型为 %s，当前仅支持 mysql 还原", meta.SourceType)
	}
	db, err := sql.Open("mysql", dsn(cfg, true))
	if err != nil {
		return res, err
	}
	defer db.Close()
	if err := db.Ping(); err != nil {
		return res, fmt.Errorf("连接 MySQL(%s:%d/%s) 失败: %w", cfg.Host, cfg.Port, cfg.DBName, err)
	}
	limit := stmtLimitFor(serverMaxAllowedPacket(db))
	if logf != nil {
		logf(fmt.Sprintf("单条语句上限 %s（按服务端 max_allowed_packet 推导）", fmtBytes(limit)))
	}
	sections := splitSections(string(sqlContent))
	if len(sections) == 0 {
		return res, errors.New("备份包 dump.sql 无有效表段落")
	}
	for _, sec := range sections {
		plan, perr := planStatements(sec.SQL, limit)
		if perr != nil {
			return res, fmt.Errorf("还原表 %s 失败: %w", sec.Name, perr)
		}
		if logf != nil {
			logf(fmt.Sprintf("还原表 %s ...（%d 条语句）", sec.Name, len(plan)))
		}
		// 覆盖还原：先删旧表（兼容不含 DROP 的旧备份，避免同名表触发 1050）。
		if _, err := db.Exec("DROP TABLE IF EXISTS " + quoteIdent(sec.Name)); err != nil {
			return res, fmt.Errorf("还原前清理表 %s 失败: %w", sec.Name, err)
		}
		for _, stmt := range plan {
			if _, err := db.Exec(stmt); err != nil {
				return res, fmt.Errorf("还原表 %s 失败: %w", sec.Name, explainExecErr(err, len(stmt), limit))
			}
		}
		res.TableCount++
	}
	res.ZipPath = zipPath
	if logf != nil {
		logf(fmt.Sprintf("还原完成：共 %d 张表", res.TableCount))
	}
	return res, nil
}

type section struct {
	Name string
	SQL  string
}

// splitSections 按 "-- TABLE: <name>" 标记切分 dump.sql（按段落边界而非行，值内换行不受影响）。
//
// 2026-09-27 修正：原来只在 "\n-- TABLE: " 处切，于是**文件开头的第一个标记会被漏掉** ——
// 登录器导出的 dump.sql 没有文件头，第一段正好从 0 字节开始（DbDumpService 直接以
// "-- TABLE: xxx" 开头），导致用运维工具还原登录器备份时第一张表被整段跳过。
// 现在按"行首标记"识别：位置 0、\n 之后、\r\n 之后都算。
func splitSections(content string) []section {
	var starts []int
	for i := 0; i+len(TableMarker) <= len(content); {
		idx := strings.Index(content[i:], TableMarker)
		if idx < 0 {
			break
		}
		pos := i + idx
		if pos == 0 || content[pos-1] == '\n' || content[pos-1] == '\r' {
			starts = append(starts, pos)
		}
		i = pos + len(TableMarker)
	}
	var out []section
	for k, pos := range starts {
		rest := content[pos+len(TableMarker):]
		lineEnd := strings.IndexByte(rest, '\n')
		var name string
		var bodyStart int
		if lineEnd < 0 {
			name = strings.TrimSpace(rest)
			bodyStart = len(content)
		} else {
			name = strings.TrimSpace(rest[:lineEnd])
			bodyStart = pos + len(TableMarker) + lineEnd + 1
		}
		end := len(content)
		if k+1 < len(starts) {
			end = starts[k+1]
		}
		body := strings.TrimSpace(content[bodyStart:end])
		if name != "" && body != "" {
			out = append(out, section{Name: name, SQL: body})
		}
	}
	return out
}

// HashZip 计算备份文件 SHA-256 用于完整性展示/校验（预留，暂不导出使用）。
// 后续需要时再引入 crypto/sha256。
