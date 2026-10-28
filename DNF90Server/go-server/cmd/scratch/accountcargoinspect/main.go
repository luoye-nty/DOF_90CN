// accountcargoinspect 只读：按角色号查账号金库等级/账号元数据，并列出三张扩容券
// 在背包里的实际槽位。
//
// 现场 2026-10-03：物品栏券(op430)、金库券(op408) 客户端都会发包，帐号金库券
// (op306) **一条都没有**。要判断"客户端是不是压根没认这件道具"，先确认这件券在不在
// 背包里、账号金库建没建。
//
// 只读。跑法：
//
//	go run ./cmd/scratch/accountcargoinspect -character 11
package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	_ "github.com/go-sql-driver/mysql"
)

func main() {
	characterID := flag.String("character", "11", "character id")
	items := flag.String("items", "490701690,490701691,490701692", "comma separated item ids to locate")
	dsn := flag.String("dsn", "", "mysql dsn (default: deployed runtime logic.toml)")
	flag.Parse()

	resolvedDSN := *dsn
	if resolvedDSN == "" {
		var err error
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
	characterTable := locateTable(ctx, db, "characters")
	metadataTable := locateTable(ctx, db, "account_metadata")
	itemsTable := locateTable(ctx, db, "inventory_items")
	if characterTable == "" || itemsTable == "" {
		fmt.Fprintln(os.Stderr, "FAILED: could not locate character/inventory tables")
		os.Exit(1)
	}

	accountID := ""
	row := db.QueryRowContext(ctx, "SELECT account_id FROM "+characterTable+" WHERE character_id = ?", *characterID)
	if err := row.Scan(&accountID); err != nil {
		fmt.Fprintf(os.Stderr, "character %s lookup failed: %v\n", *characterID, err)
	}
	fmt.Printf("character=%s account=%q\n\n", *characterID, accountID)

	if metadataTable != "" && accountID != "" {
		fmt.Printf("== %s（account_cargo_level 在这里）==\n", metadataTable)
		rows, err := db.QueryContext(ctx, "SELECT entry_key, entry_value FROM "+metadataTable+" WHERE account_id = ? ORDER BY entry_key", accountID)
		if err != nil {
			fmt.Fprintln(os.Stderr, "  query failed:", err)
		} else {
			for rows.Next() {
				var key, value string
				if err := rows.Scan(&key, &value); err == nil {
					fmt.Printf("  %-28s = %s\n", key, value)
				}
			}
			rows.Close()
		}
	} else {
		fmt.Printf("== account_metadata 表没找到，或账号为空 ==\n")
	}

	fmt.Printf("\n== 三张扩容券在背包/金库里的位置 ==\n")
	wanted := map[string]bool{}
	for _, raw := range strings.Split(*items, ",") {
		if trimmed := strings.TrimSpace(raw); trimmed != "" {
			wanted[trimmed] = true
		}
	}
	rows, err := db.QueryContext(ctx,
		"SELECT entry_key, item_id, item_count FROM "+itemsTable+" WHERE character_id = ? ORDER BY entry_key", *characterID)
	if err != nil {
		fmt.Fprintln(os.Stderr, "  query failed:", err)
		os.Exit(1)
	}
	defer rows.Close()
	found := make([]string, 0, 8)
	for rows.Next() {
		var entryKey string
		var itemID, count int64
		if err := rows.Scan(&entryKey, &itemID, &count); err != nil {
			continue
		}
		if wanted[fmt.Sprintf("%d", itemID)] {
			found = append(found, fmt.Sprintf("  entry_key=%-10s item=%d count=%d", entryKey, itemID, count))
		}
	}
	sort.Strings(found)
	if len(found) == 0 {
		fmt.Printf("  背包里没有这三张券（可能已用光/在别的角色上）\n")
	}
	for _, line := range found {
		fmt.Println(line)
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

// locateTable 找 <schema>.dnf_<suffix> 或 <schema>.<suffix>。返回带反引号的全名。
func locateTable(ctx context.Context, db *sql.DB, suffix string) string {
	rows, err := db.QueryContext(ctx,
		"SELECT table_schema, table_name FROM information_schema.tables WHERE table_name IN (?, ?) ORDER BY LENGTH(table_name)",
		"dnf_"+suffix, suffix)
	if err != nil {
		return ""
	}
	defer rows.Close()
	for rows.Next() {
		var schema, table string
		if err := rows.Scan(&schema, &table); err != nil {
			return ""
		}
		return "`" + schema + "`.`" + table + "`"
	}
	return ""
}
