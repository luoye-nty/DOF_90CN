// equipinspect 只读：打印某角色背包里的装备行（raw_entry 关键字节 + extra），
// 用来定位"封装显示"这类 raweEntry/extra 不一致的问题。
//
// 现场 2026-10-03（服主报）：圣物转换书做的圣物，用品级箱调整后显示成"封装"，
// 小退又恢复正常。需要看库里那行的 raw_entry[0x0D]（封装位）与 extra（seal_flag/bind）
// 到底哪个是真、哪个是脏。
//
// 只读。跑法：
//
//	go run ./cmd/scratch/equipinspect -character 10 -rarity 3
//	go run ./cmd/scratch/equipinspect -character 10 -slot 42
package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	_ "github.com/go-sql-driver/mysql"
)

func main() {
	characterID := flag.String("character", "", "character id (required)")
	rarity := flag.Int64("rarity", -1, "only rows whose extra rarity matches")
	slot := flag.Int("slot", -1, "only this slot")
	limit := flag.Int("limit", 40, "max rows to print")
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
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "FAILED: mysql unreachable:", err)
		os.Exit(1)
	}
	schema, table, err := locateTable(ctx, db, "\\_inventory\\_items", "account")
	if err != nil {
		fmt.Fprintln(os.Stderr, "FAILED:", err)
		os.Exit(1)
	}
	_, extraTable, err := locateTable(ctx, db, "\\_inventory\\_item\\_extra", "account")
	if err != nil {
		fmt.Fprintln(os.Stderr, "FAILED:", err)
		os.Exit(1)
	}
	fmt.Printf("schema=%s item表=%s extra表=%s character=%s\n\n", schema, table, extraTable, *characterID)

	rows, err := db.QueryContext(ctx,
		"SELECT entry_key, item_id, item_count, bind_flag, raw_entry FROM `"+schema+"`.`"+table+
			"` WHERE character_id = ? ORDER BY LENGTH(entry_key), entry_key", *characterID)
	if err != nil {
		fmt.Fprintln(os.Stderr, "FAILED: query:", err)
		os.Exit(1)
	}
	defer rows.Close()
	type row struct {
		key      string
		itemID   int64
		count    int64
		bindFlag int64
		raw      []byte
	}
	all := make([]row, 0, 256)
	for rows.Next() {
		var item, count, bind sql.NullInt64
		var key string
		var raw []byte
		if err := rows.Scan(&key, &item, &count, &bind, &raw); err != nil {
			fmt.Fprintln(os.Stderr, "FAILED: scan:", err)
			os.Exit(1)
		}
		all = append(all, row{key: key, itemID: item.Int64, count: count.Int64, bindFlag: bind.Int64, raw: raw})
	}
	if err := rows.Err(); err != nil {
		fmt.Fprintln(os.Stderr, "FAILED: rows:", err)
		os.Exit(1)
	}

	extras := map[string]map[string]string{}
	extraRows, err := db.QueryContext(ctx,
		"SELECT entry_key, extra_key, extra_value FROM `"+schema+"`.`"+extraTable+"` WHERE character_id = ?", *characterID)
	if err != nil {
		fmt.Fprintln(os.Stderr, "FAILED: extra query:", err)
		os.Exit(1)
	}
	defer extraRows.Close()
	for extraRows.Next() {
		var key, name, value string
		if err := extraRows.Scan(&key, &name, &value); err != nil {
			fmt.Fprintln(os.Stderr, "FAILED: extra scan:", err)
			os.Exit(1)
		}
		if extras[key] == nil {
			extras[key] = map[string]string{}
		}
		extras[key][name] = value
	}

	printed := 0
	for _, item := range all {
		extra := extras[item.key]
		if *slot >= 0 {
			_, slotPart, _ := strings.Cut(item.key, ":")
			if parsed, convErr := strconv.Atoi(slotPart); convErr != nil || parsed != *slot {
				continue
			}
		}
		if *rarity >= 0 {
			if extra["rarity"] != strconv.FormatInt(*rarity, 10) {
				continue
			}
		}
		if printed >= *limit {
			fmt.Printf("... 还有更多（用 -limit 提高）\n")
			break
		}
		printed++
		rawSeal := "n/a"
		if len(item.raw) > 0x0D {
			rawSeal = fmt.Sprintf("%d", item.raw[0x0D])
		}
		fmt.Printf("slot %-6s item=%-11d count=%-3d bind_flag=%d raw[0x0D](封装位)=%s rawlen=%d\n",
			item.key, item.itemID, item.count, item.bindFlag, rawSeal, len(item.raw))
		keys := make([]string, 0, len(extra))
		for name := range extra {
			keys = append(keys, name)
		}
		sort.Strings(keys)
		interesting := []string{}
		for _, name := range keys {
			switch name {
			case "raw_entry_hex", "current_raw_entry_77", "raw_entry_77":
				rawHex := extra[name]
				sealByte := "n/a"
				if len(rawHex) > 0x0D*2+1 {
					sealByte = rawHex[0x0D*2 : 0x0D*2+2]
				}
				interesting = append(interesting, fmt.Sprintf("%s[0x0D]=%s(len=%d)", name, sealByte, len(rawHex)/2))
			case "seal_flag", "seal", "bind", "bind_flag", "rarity", "item_kind", "quality_seed",
				"source", "trade_restriction", "pvf_path", "grade", "reseal_count", "last_grade_adjust",
				"grade_adjust_type", "equipment_update_seal_flag", "update_seal_flag":
				interesting = append(interesting, fmt.Sprintf("%s=%s", name, extra[name]))
			}
		}
		if len(interesting) > 0 {
			fmt.Printf("        %s\n", strings.Join(interesting, "  "))
		}
		other := []string{}
		for _, name := range keys {
			switch name {
			case "raw_entry_hex", "current_raw_entry_77", "raw_entry_77":
				rawHex := extra[name]
				sealByte := "n/a"
				if len(rawHex) > 0x0D*2+1 {
					sealByte = rawHex[0x0D*2 : 0x0D*2+2]
				}
				interesting = append(interesting, fmt.Sprintf("%s[0x0D]=%s(len=%d)", name, sealByte, len(rawHex)/2))
			case "seal_flag", "seal", "bind", "bind_flag", "rarity", "item_kind", "quality_seed",
				"source", "trade_restriction", "pvf_path", "grade", "reseal_count", "last_grade_adjust",
				"grade_adjust_type", "equipment_update_seal_flag", "update_seal_flag":
				continue
			}
			other = append(other, name)
		}
		if len(other) > 0 {
			fmt.Printf("        其它 extra 键: %s\n", strings.Join(other, ", "))
		}
	}
	if printed == 0 {
		fmt.Printf("没有匹配的行（-rarity=%d -slot=%d）\n", *rarity, *slot)
	}
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

func locateTable(ctx context.Context, db *sql.DB, pattern, exclude string) (string, string, error) {
	rows, err := db.QueryContext(ctx,
		"SELECT table_schema, table_name FROM information_schema.tables WHERE table_name LIKE ? ORDER BY table_schema",
		"%"+pattern)
	if err != nil {
		return "", "", err
	}
	defer rows.Close()
	for rows.Next() {
		var schema, table string
		if err := rows.Scan(&schema, &table); err != nil {
			return "", "", err
		}
		if exclude != "" && strings.Contains(table, exclude) {
			continue
		}
		return schema, table, nil
	}
	return "", "", fmt.Errorf("no table matching %s", pattern)
}
