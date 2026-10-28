// containersinspect 只读：打印某角色的容器状态（扩容档位）与账号金库等级。
//
// 现场 2026-10-03：物品栏/金库/帐号金库三张扩展券"使用后没效果"。要判断是不是
// "客户端认为已达上限所以根本不发包"，必须先看服务端侧的档位值。
//
// 只读。跑法：
//
//	go run ./cmd/scratch/containersinspect -character 11
package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	_ "github.com/go-sql-driver/mysql"
)

func main() {
	characterID := flag.String("character", "", "character id (required)")
	dsn := flag.String("dsn", "", "mysql dsn (default: deployed runtime logic.toml)")
	flag.Parse()
	if *characterID == "" {
		fmt.Fprintln(os.Stderr, "FAILED: -character is required")
		os.Exit(2)
	}
	resolvedDSN := *dsn
	var err error
	if resolvedDSN == "" {
		resolvedDSN, err = dsnFromRuntimeConfig()
		if err != nil {
			fmt.Fprintln(os.Stderr, "FAILED:", err)
			os.Exit(2)
		}
	}
	db, err := sql.Open("mysql", resolvedDSN)
	if err != nil {
		fmt.Fprintln(os.Stderr, "FAILED:", err)
		os.Exit(1)
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "FAILED: mysql unreachable:", err)
		os.Exit(1)
	}
	schema, err := locateSchema(ctx, db)
	if err != nil {
		fmt.Fprintln(os.Stderr, "FAILED:", err)
		os.Exit(1)
	}

	scope := "character:" + *characterID + ":container_state"
	fmt.Printf("schema=%s scope=%s\n\n", schema, scope)
	dumpScope(ctx, db, schema, scope)
	fmt.Printf("\n账号行（account_cargo_level 在这里）：\n")
	dumpScope(ctx, db, schema, "account:"+*characterID)
}

func dumpScope(ctx context.Context, db *sql.DB, schema, scope string) {
	table, err := firstExistingTable(ctx, db, schema, "setting_values")
	if err != nil || table == "" {
		fmt.Fprintln(os.Stderr, "  settings 表不存在:", err)
		return
	}
	columns, err := tableColumns(ctx, db, schema, table)
	if err != nil {
		fmt.Fprintln(os.Stderr, "  列读取失败:", err)
		return
	}
	keyCol := ""
	for _, candidate := range []string{"scope", "record_key", "settings_key"} {
		for _, column := range columns {
			if column == candidate {
				keyCol = candidate
				break
			}
		}
		if keyCol != "" {
			break
		}
	}
	entryKeyCol, entryValueCol := "", ""
	for _, column := range columns {
		switch column {
		case "entry_key", "value_key", "settings_key", "key_name", "name":
			entryKeyCol = column
		case "entry_value", "value_json", "value", "entry_data":
			entryValueCol = column
		}
	}
	if keyCol == "" || entryKeyCol == "" || entryValueCol == "" {
		fmt.Printf("  列不匹配：cols=%v\n", columns)
		return
	}
	rows, err := db.QueryContext(ctx,
		"SELECT `"+entryKeyCol+"`, `"+entryValueCol+"` FROM `"+schema+"`.`"+table+"` WHERE `"+keyCol+"` = ? ORDER BY `"+entryKeyCol+"`",
		scope)
	if err != nil {
		fmt.Fprintln(os.Stderr, "  查询失败:", err)
		return
	}
	defer rows.Close()
	found := false
	for rows.Next() {
		found = true
		var name, value string
		if err := rows.Scan(&name, &value); err != nil {
			fmt.Fprintln(os.Stderr, "  扫描失败:", err)
			return
		}
		fmt.Printf("  %-34s = %s\n", name, trim(value, 200))
	}
	if !found {
		fmt.Printf("  没有匹配 %q 的行\n", scope)
	}
}

func trim(value string, max int) string {
	value = strings.ReplaceAll(value, "\n", " ")
	if len(value) <= max {
		return value
	}
	return value[:max] + "..."
}

func dsnFromRuntimeConfig() (string, error) {
	for _, path := range []string{
		`runtime\configs\dnf\logic.toml`,
		`..\runtime\configs\dnf\logic.toml`,
	} {
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(raw), "\n") {
			trimmed := strings.TrimSpace(line)
			if !strings.HasPrefix(trimmed, "mysql_dsn") {
				continue
			}
			_, value, found := strings.Cut(trimmed, "=")
			if !found {
				continue
			}
			return strings.Trim(strings.TrimSpace(value), `"`), nil
		}
	}
	return "", fmt.Errorf("mysql_dsn not found")
}

func locateSchema(ctx context.Context, db *sql.DB) (string, error) {
	rows, err := db.QueryContext(ctx,
		"SELECT table_schema FROM information_schema.tables WHERE table_name LIKE '%\\_inventory\\_items' ORDER BY table_schema")
	if err != nil {
		return "", err
	}
	defer rows.Close()
	for rows.Next() {
		var schema string
		if err := rows.Scan(&schema); err != nil {
			return "", err
		}
		return schema, nil
	}
	return "", fmt.Errorf("no inventory schema found")
}

func firstExistingTable(ctx context.Context, db *sql.DB, schema, suffix string) (string, error) {
	rows, err := db.QueryContext(ctx,
		"SELECT table_name FROM information_schema.tables WHERE table_schema = ? AND table_name LIKE ? ORDER BY LENGTH(table_name)",
		schema, "%_"+suffix)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return "", err
		}
		return name, nil
	}
	return "", rows.Err()
}

func tableColumns(ctx context.Context, db *sql.DB, schema, table string) ([]string, error) {
	rows, err := db.QueryContext(ctx,
		"SELECT column_name FROM information_schema.columns WHERE table_schema = ? AND table_name = ? ORDER BY ordinal_position",
		schema, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var columns []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		columns = append(columns, name)
	}
	return columns, rows.Err()
}
