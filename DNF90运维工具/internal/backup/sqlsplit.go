// 本文件：把 dump 段落切成「可安全发送」的单条语句。
//
// 背景（2026-09-27 线上故障）：
//
//	还原时原本把整张表的段落（DROP + CREATE + 整表 INSERT）当成**一个包**发出去
//	（db.Exec(sec.SQL)，DSN 开了 multiStatements）。go-sql-driver 在发送前有客户端上限
//	maxAllowedPacket（默认 64 MiB），超了就直接拒发，报错原文：
//	  packet for query is too large. Try adjusting the `Config.MaxAllowedPacket`
//	注意这是**驱动报的**，不是 MySQL 报的 —— 所以把服务端 max_allowed_packet 调大也没用。
//	大表（几十万行、LONGTEXT 列）必然踩中；而失败发生在"包还没发出去"之前，此时表已被
//	DROP，库会停在半还原状态（该表被删且没建回来）。
//
// 对策：
//  1. 还原一律按**语句**发送，绝不整段一个包；
//  2. 单条语句仍然过大时（旧备份是"整表一条 INSERT"），按行边界切成小批；
//  3. 单行本身就超过上限时无法切分 —— 明确报错并给出处理办法，而不是抛驱动原文。
package backup

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"

	mysqldrv "github.com/go-sql-driver/mysql"
)

// 单条语句发送上限（按语句文本字节数）：
// 取服务端 max_allowed_packet 的一半，再夹在 [min, max] 之间；拿不到服务端值时用默认值。
const (
	stmtLimitDefault = 4 << 20  // 4 MiB
	stmtLimitMax     = 8 << 20  // 8 MiB
	stmtLimitMin     = 64 << 10 // 64 KiB
)

// stmtLimitFor 由服务端 max_allowed_packet 推导单条语句上限。
func stmtLimitFor(serverMax int64) int {
	if serverMax <= 0 {
		return stmtLimitDefault
	}
	limit := int(serverMax / 2)
	if limit > stmtLimitMax {
		limit = stmtLimitMax
	}
	if limit < stmtLimitMin {
		limit = stmtLimitMin
	}
	return limit
}

// serverMaxAllowedPacket 读服务端 max_allowed_packet；失败返回 0（调用方用默认值）。
func serverMaxAllowedPacket(db *sql.DB) int64 {
	var v int64
	if err := db.QueryRow("SELECT @@max_allowed_packet").Scan(&v); err != nil {
		return 0
	}
	return v
}

// fmtBytes 人类可读字节数（日志用）。
func fmtBytes(n int) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}

// splitStatements 把一段 dump SQL 按顶层分号切成单条语句。
//
// 需要正确跳过的结构：'...' / "..." 字符串（含 \ 转义与 ” 双写）、`...` 反引号标识符、
// -- 与 # 行注释、/* */ 块注释 —— 这些结构内部的分号不是语句分隔符。
// （多字节 UTF-8 的每个字节都 ≥ 0x80，与所有分隔符的 ASCII 字节不同，逐字节扫描安全。）
func splitStatements(sql string) []string {
	var out []string
	start := 0
	i := 0
	n := len(sql)
	for i < n {
		switch c := sql[i]; c {
		case '\'', '"', '`':
			i = skipQuoted(sql, i)
		case '-':
			if i+1 < n && sql[i+1] == '-' && (i+2 >= n || isSpace(sql[i+2])) {
				i = skipLineComment(sql, i+2)
			} else {
				i++
			}
		case '#':
			i = skipLineComment(sql, i+1)
		case '/':
			if i+1 < n && sql[i+1] == '*' {
				i = skipBlockComment(sql, i+2)
			} else {
				i++
			}
		case ';':
			if stmt := strings.TrimSpace(sql[start:i]); stmt != "" {
				out = append(out, stmt)
			}
			i++
			start = i
		default:
			i++
		}
	}
	if tail := strings.TrimSpace(sql[start:]); tail != "" {
		out = append(out, tail)
	}
	return out
}

func isSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\r' || c == '\n'
}

// skipQuoted 从引号处跳到其配对引号之后；返回下一个待扫描位置（未闭合时返回 len）。
func skipQuoted(sql string, i int) int {
	q := sql[i]
	i++
	n := len(sql)
	for i < n {
		c := sql[i]
		if c == '\\' { // \' 与 \\ 转义
			i += 2
			continue
		}
		if c == q {
			if i+1 < n && sql[i+1] == q { // '' 双写
				i += 2
				continue
			}
			return i + 1
		}
		i++
	}
	return n
}

// skipLineComment 跳过 -- / # 行注释，返回到换行之后。
func skipLineComment(sql string, i int) int {
	if idx := strings.IndexByte(sql[i:], '\n'); idx >= 0 {
		return i + idx + 1
	}
	return len(sql)
}

// skipBlockComment 跳过 /* */ 块注释，返回其后位置（未闭合时返回 len）。
func skipBlockComment(sql string, i int) int {
	if idx := strings.Index(sql[i:], "*/"); idx >= 0 {
		return i + idx + 2
	}
	return len(sql)
}

// indexTopLevelKeyword 在顶层（不在引号/注释内）查找关键字，返回其起始下标，找不到返回 -1。
func indexTopLevelKeyword(sql string, kw string) int {
	i := 0
	n := len(sql)
	for i < n {
		switch c := sql[i]; c {
		case '\'', '"', '`':
			i = skipQuoted(sql, i)
			continue
		case '-':
			if i+1 < n && sql[i+1] == '-' && (i+2 >= n || isSpace(sql[i+2])) {
				i = skipLineComment(sql, i+2)
				continue
			}
		case '#':
			i = skipLineComment(sql, i+1)
			continue
		case '/':
			if i+1 < n && sql[i+1] == '*' {
				i = skipBlockComment(sql, i+2)
				continue
			}
		}
		if len(kw) <= n-i && strings.EqualFold(sql[i:i+len(kw)], kw) {
			return i
		}
		i++
	}
	return -1
}

// splitOversizedInsert 把一条超大 INSERT ... VALUES (...),(...) 按行边界切成多批（每批 ≤ limit）。
// 返回 nil 表示无法安全切分（单行本身就超限、不是 VALUES 列表形式、或用了 ROW(...) 构造器），
// 由调用方给出明确错误提示。
func splitOversizedInsert(stmt string, limit int) []string {
	if limit <= 0 {
		return nil
	}
	valuesAt := indexTopLevelKeyword(stmt, "VALUES")
	if valuesAt < 0 {
		return nil
	}
	// 行列表起点：VALUES 之后第一个 '('。
	listStart := -1
	for i := valuesAt + len("VALUES"); i < len(stmt); i++ {
		if stmt[i] == '(' {
			listStart = i
			break
		}
		if !isSpace(stmt[i]) {
			break
		}
	}
	if listStart < 0 {
		return nil
	}
	// MySQL 8.0 的 VALUES ROW(...),ROW(...) 形式不切（拼回去容易出错）。
	if before := strings.TrimSpace(stmt[:listStart]); strings.HasSuffix(strings.ToUpper(before), "ROW") {
		return nil
	}

	// 收集每行 "(...)" 文本。
	var rows []string
	depth := 0
	rowStart := -1
	i := listStart
	for i < len(stmt) {
		switch c := stmt[i]; c {
		case '\'', '"', '`':
			i = skipQuoted(stmt, i)
			continue
		case '-':
			if i+1 < len(stmt) && stmt[i+1] == '-' {
				i = skipLineComment(stmt, i+2)
				continue
			}
		case '#':
			i = skipLineComment(stmt, i+1)
			continue
		case '/':
			if i+1 < len(stmt) && stmt[i+1] == '*' {
				i = skipBlockComment(stmt, i+2)
				continue
			}
		case '(':
			if depth == 0 {
				rowStart = i
			}
			depth++
			i++
			continue
		case ')':
			depth--
			if depth < 0 {
				return nil
			}
			if depth == 0 && rowStart >= 0 {
				rows = append(rows, stmt[rowStart:i+1])
				rowStart = -1
			}
			i++
			continue
		}
		i++
	}
	if depth != 0 || len(rows) == 0 {
		return nil
	}

	prefix := stmt[:listStart] // "INSERT INTO `t` (cols) VALUES "
	fixed := len(prefix) + 1   // 结尾分号
	var out []string
	var cur strings.Builder
	flush := func() {
		if cur.Len() == 0 {
			return
		}
		out = append(out, prefix+strings.TrimSuffix(cur.String(), ",")+";")
		cur.Reset()
	}
	for _, row := range rows {
		if fixed+len(row) > limit { // 单行就超过上限：切不动
			return nil
		}
		if cur.Len() > 0 && fixed+cur.Len()+1+len(row) > limit {
			flush()
		}
		cur.WriteString(row)
		cur.WriteString(",")
	}
	flush()
	return out
}

// planStatements 把一段表段落展开成可安全发送的语句列表：
// 先按语句切分，逐条检查大小，超限的多值 INSERT 再按行切成小批。
func planStatements(section string, limit int) ([]string, error) {
	stmts := splitStatements(section)
	if len(stmts) == 0 {
		return nil, errors.New("段落内没有可执行语句")
	}
	out := make([]string, 0, len(stmts))
	for _, s := range stmts {
		if len(s) <= limit {
			out = append(out, s)
			continue
		}
		batches := splitOversizedInsert(s, limit)
		if batches == nil {
			return nil, fmt.Errorf(
				"有一条 %s 的语句超过单条发送上限 %s，且无法按行拆分（多半是单行数据本身就过大）。"+
					"处理：把服务端 max_allowed_packet 调大（例如 512M）后重试",
				fmtBytes(len(s)), fmtBytes(limit))
		}
		out = append(out, batches...)
	}
	return out, nil
}

// explainExecErr 把驱动/服务端的"包太大"类错误翻译成人话，其余原样返回。
func explainExecErr(err error, stmtBytes, limit int) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	switch {
	case errors.Is(err, mysqldrv.ErrPktTooLarge), strings.Contains(msg, "packet for query is too large"):
		return fmt.Errorf("单条语句 %s 超过发送上限 %s（%v）。处理：把服务端 max_allowed_packet 调大（例如 512M）后重试",
			fmtBytes(stmtBytes), fmtBytes(limit), err)
	case strings.Contains(msg, "max_allowed_packet"):
		return fmt.Errorf("服务端拒绝了超过 max_allowed_packet 的包（单条语句 %s）：%w。处理：调大服务端 max_allowed_packet（例如 512M）后重试",
			fmtBytes(stmtBytes), err)
	}
	return err
}
