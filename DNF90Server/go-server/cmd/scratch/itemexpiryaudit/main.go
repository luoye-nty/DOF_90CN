package main

import (
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
)

const (
	itemWireSize        = 0x77
	itemExpireOffset    = 0x38
	legacyExpireOffset  = 0x2B
	defaultTargetItemID = int64(10004024)
)

var safeIdentifier = regexp.MustCompile(`^[A-Za-z0-9_]+$`)

type instanceConfig struct {
	Database struct {
		Host     string `json:"host"`
		Port     int    `json:"port"`
		Name     string `json:"name"`
		User     string `json:"user"`
		Password string `json:"password"`
	} `json:"database"`
}

type tableAudit struct {
	Table                 string `json:"table"`
	Rows                  int64  `json:"rows"`
	RowsWithExpireAt      int64  `json:"rows_with_expire_at"`
	TargetRows            int64  `json:"target_rows"`
	TargetRowsWithExpire  int64  `json:"target_rows_with_expire_at"`
	TargetRowsWithRawTime int64  `json:"target_rows_with_raw_expire"`
}

type auditReport struct {
	Database     string       `json:"database"`
	TargetItemID int64        `json:"target_item_id"`
	Tables       []tableAudit `json:"tables"`
}

func main() {
	instancePath := flag.String("instance", `D:/DNF/runtime/config/instance.json`, "runtime instance config")
	targetItemID := flag.Int64("item", defaultTargetItemID, "item ID to inspect without mutation")
	flag.Parse()
	result, err := audit(*instancePath, *targetItemID)
	if err != nil {
		fmt.Fprintln(os.Stderr, "itemexpiryaudit:", err)
		os.Exit(1)
	}
	encoded, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		fmt.Fprintln(os.Stderr, "itemexpiryaudit: encode report:", err)
		os.Exit(1)
	}
	fmt.Println(string(encoded))
}

func audit(instancePath string, targetItemID int64) (auditReport, error) {
	var result auditReport
	data, err := os.ReadFile(instancePath)
	if err != nil {
		return result, fmt.Errorf("read instance config: %w", err)
	}
	var instance instanceConfig
	if err := json.Unmarshal(data, &instance); err != nil {
		return result, fmt.Errorf("decode instance config: %w", err)
	}
	if !safeIdentifier.MatchString(instance.Database.Name) {
		return result, fmt.Errorf("unsafe database name %q", instance.Database.Name)
	}
	configuration := mysql.NewConfig()
	configuration.User = instance.Database.User
	configuration.Passwd = instance.Database.Password
	configuration.Net = "tcp"
	configuration.Addr = net.JoinHostPort(instance.Database.Host, strconv.Itoa(instance.Database.Port))
	configuration.DBName = instance.Database.Name
	configuration.ParseTime = true
	configuration.Loc = time.UTC
	database, err := sql.Open("mysql", configuration.FormatDSN())
	if err != nil {
		return result, fmt.Errorf("open database: %w", err)
	}
	defer database.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := database.PingContext(ctx); err != nil {
		return result, fmt.Errorf("ping database: %w", err)
	}

	result.Database = instance.Database.Name
	result.TargetItemID = targetItemID
	tables, err := expirationTables(ctx, database, instance.Database.Name)
	if err != nil {
		return result, err
	}
	for _, table := range tables {
		row, err := auditTable(ctx, database, table, targetItemID)
		if err != nil {
			return result, err
		}
		result.Tables = append(result.Tables, row)
	}
	return result, nil
}

func expirationTables(ctx context.Context, database *sql.DB, databaseName string) ([]string, error) {
	rows, err := database.QueryContext(ctx, `SELECT table_name FROM information_schema.tables WHERE table_schema = ? ORDER BY table_name`, databaseName)
	if err != nil {
		return nil, fmt.Errorf("list database tables: %w", err)
	}
	defer rows.Close()
	wantedSuffixes := []string{"account_inventory_items", "inventory_items", "equipment_entries", "mail_attachments"}
	var tables []string
	for rows.Next() {
		var table string
		if err := rows.Scan(&table); err != nil {
			return nil, err
		}
		if !safeIdentifier.MatchString(table) {
			return nil, fmt.Errorf("unsafe table name %q", table)
		}
		for _, suffix := range wantedSuffixes {
			if table == suffix || strings.HasSuffix(table, "_"+suffix) {
				tables = append(tables, table)
				break
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return tables, nil
}

func auditTable(ctx context.Context, database *sql.DB, table string, targetItemID int64) (tableAudit, error) {
	result := tableAudit{Table: table}
	query := "SELECT COUNT(*), COALESCE(SUM(expire_at IS NOT NULL), 0), COALESCE(SUM(item_id = ?), 0), COALESCE(SUM(item_id = ? AND expire_at IS NOT NULL), 0) FROM `" + table + "`"
	if err := database.QueryRowContext(ctx, query, targetItemID, targetItemID).Scan(
		&result.Rows,
		&result.RowsWithExpireAt,
		&result.TargetRows,
		&result.TargetRowsWithExpire,
	); err != nil {
		return result, fmt.Errorf("audit table %s: %w", table, err)
	}
	rows, err := database.QueryContext(ctx, "SELECT raw_entry FROM `"+table+"` WHERE item_id = ?", targetItemID)
	if err != nil {
		return result, fmt.Errorf("audit target raw rows in %s: %w", table, err)
	}
	defer rows.Close()
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return result, err
		}
		if len(raw) == itemWireSize &&
			(binary.LittleEndian.Uint32(raw[itemExpireOffset:itemExpireOffset+4]) != 0 ||
				binary.LittleEndian.Uint32(raw[legacyExpireOffset:legacyExpireOffset+4]) != 0) {
			result.TargetRowsWithRawTime++
		}
	}
	if err := rows.Err(); err != nil {
		return result, err
	}
	return result, nil
}
