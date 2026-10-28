package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
	_ "modernc.org/sqlite"

	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

const (
	usePeriodOffset    = 0x0B
	legacyExpireOffset = 0x2B
	itemExpireOffset   = 0x38
)

var (
	safeIdentifier = regexp.MustCompile(`^[A-Za-z0-9_]+$`)
	timeSections   = map[string]struct{}{
		"expiration date":               {},
		"expiration info":               {},
		"usable datetime":               {},
		"usable period":                 {},
		"usable period after unsealing": {},
	}
	expirationExtraKeys = []string{"expire_time", "expire_unix", "usable_period_days", "expiration_source"}
)

type instanceConfig struct {
	Database struct {
		Mode     string `json:"mode"`
		Path     string `json:"path"`
		Host     string `json:"host"`
		Port     int    `json:"port"`
		Name     string `json:"name"`
		User     string `json:"user"`
		Password string `json:"password"`
	} `json:"database"`
}

type itemKinds struct {
	Stackable bool
	Equipment bool
}

type containerReport struct {
	Container        string `json:"container"`
	RowsScanned      int    `json:"rows_scanned"`
	TargetRows       int    `json:"target_rows"`
	RowsChanged      int    `json:"rows_changed"`
	RuntimePreserved int    `json:"runtime_expiration_preserved"`
}

type report struct {
	Mode                 string            `json:"mode"`
	Database             string            `json:"database"`
	PVFPath              string            `json:"pvf_path"`
	PVFSHA256            string            `json:"pvf_sha256"`
	PVFItemFilesTimed    int               `json:"pvf_item_files_timed"`
	PVFItemIDsTimed      int               `json:"pvf_item_ids_timed"`
	Containers           []containerReport `json:"containers"`
	RowsChanged          int               `json:"rows_changed"`
	RuntimePreserved     int               `json:"runtime_expiration_preserved"`
	Apply                bool              `json:"apply"`
	Committed            bool              `json:"committed"`
	ExpirationKeysPurged []string          `json:"expiration_keys_purged"`
}

type mysqlTableSpec struct {
	itemSuffix  string
	extraSuffix string
	keyColumns  []string
}

type mysqlItemRow struct {
	keys     []string
	itemID   int64
	expireAt sql.NullTime
	raw      []byte
}

type patchResult struct {
	changed   bool
	preserved bool
}

func main() {
	instancePath := flag.String("instance", `D:/DNF/runtime/config/instance.json`, "runtime instance configuration")
	pvfPath := flag.String("pvf", `D:/DNF/runtime/data/dnf/Script.pvf`, "original PVF containing item time-limit definitions")
	apply := flag.Bool("apply", false, "commit the migration; default is a read-only audit")
	flag.Parse()

	result, err := run(*instancePath, *pvfPath, *apply)
	if err != nil {
		fmt.Fprintln(os.Stderr, "removeitemexpirydb:", err)
		os.Exit(1)
	}
	encoded, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		fmt.Fprintln(os.Stderr, "removeitemexpirydb: encode report:", err)
		os.Exit(1)
	}
	fmt.Println(string(encoded))
}

func run(instancePath, pvfPath string, apply bool) (report, error) {
	var result report
	instancePath, err := filepath.Abs(filepath.Clean(strings.TrimSpace(instancePath)))
	if err != nil {
		return result, fmt.Errorf("resolve instance path: %w", err)
	}
	pvfPath, err = filepath.Abs(filepath.Clean(strings.TrimSpace(pvfPath)))
	if err != nil {
		return result, fmt.Errorf("resolve PVF path: %w", err)
	}
	configurationBytes, err := os.ReadFile(instancePath)
	if err != nil {
		return result, fmt.Errorf("read instance config: %w", err)
	}
	var configuration instanceConfig
	if err := json.Unmarshal(configurationBytes, &configuration); err != nil {
		return result, fmt.Errorf("decode instance config: %w", err)
	}
	targets, timedFiles, pvfHash, err := timedItemIDs(pvfPath)
	if err != nil {
		return result, err
	}
	result = report{
		Mode:                 strings.ToLower(strings.TrimSpace(configuration.Database.Mode)),
		PVFPath:              pvfPath,
		PVFSHA256:            pvfHash,
		PVFItemFilesTimed:    timedFiles,
		PVFItemIDsTimed:      len(targets),
		Apply:                apply,
		ExpirationKeysPurged: append([]string(nil), expirationExtraKeys...),
	}
	if result.Mode == "" {
		result.Mode = "portable"
	}
	switch result.Mode {
	case "portable", "mysql":
		result.Database = configuration.Database.Name
		result.Containers, result.Committed, err = migrateMySQL(configuration, targets, apply)
	case "sqlite":
		sqlitePath := strings.TrimSpace(configuration.Database.Path)
		if sqlitePath == "" {
			sqlitePath = "data/dnf90.db"
		}
		if !filepath.IsAbs(sqlitePath) {
			runtimeRoot := filepath.Dir(filepath.Dir(instancePath))
			sqlitePath = filepath.Join(runtimeRoot, filepath.FromSlash(sqlitePath))
		}
		sqlitePath = filepath.Clean(sqlitePath)
		result.Database = sqlitePath
		result.Containers, result.Committed, err = migrateSQLite(sqlitePath, targets, apply)
	default:
		err = fmt.Errorf("unsupported database mode %q", result.Mode)
	}
	if err != nil {
		return result, err
	}
	for _, container := range result.Containers {
		result.RowsChanged += container.RowsChanged
		result.RuntimePreserved += container.RuntimePreserved
	}
	return result, nil
}

func timedItemIDs(pvfPath string) (map[int64]itemKinds, int, string, error) {
	hash, err := hashFile(pvfPath)
	if err != nil {
		return nil, 0, "", err
	}
	archive, err := platformpvf.Open(pvfPath)
	if err != nil {
		return nil, 0, "", fmt.Errorf("open source PVF: %w", err)
	}
	timedPaths := make(map[string]struct{})
	for _, file := range archive.Files() {
		archivePath := normalizeArchivePath(file.ArchivePath)
		if file.DataType != 1 || (!strings.HasPrefix(archivePath, "stackable/") && !strings.HasPrefix(archivePath, "equipment/")) {
			continue
		}
		extension := strings.ToLower(filepath.Ext(archivePath))
		if extension != ".stk" && extension != ".equ" {
			continue
		}
		raw, readErr := archive.FileRaw(file.Index)
		if readErr != nil {
			return nil, 0, "", fmt.Errorf("read item definition %s: %w", file.ArchivePath, readErr)
		}
		if hasItemTimeSection(raw, archive.ResolveString) {
			timedPaths[archivePath] = struct{}{}
		}
	}
	targets := make(map[int64]itemKinds)
	for _, list := range []struct {
		kind string
		path string
		root string
	}{
		{kind: "stackable", path: "stackable/stackable.lst", root: "stackable"},
		{kind: "equipment", path: "equipment/equipment.lst", root: "equipment"},
	} {
		text, readErr := archive.ReadText(list.path)
		if readErr != nil {
			return nil, 0, "", fmt.Errorf("read %s: %w", list.path, readErr)
		}
		document, parseErr := dnfpvf.Parse(list.path, text)
		if parseErr != nil {
			return nil, 0, "", fmt.Errorf("parse %s: %w", list.path, parseErr)
		}
		for _, entry := range dnfpvf.ParseList(document) {
			actual := resolveListedPath(archive, list.root, entry.Path)
			if _, timed := timedPaths[normalizeArchivePath(actual)]; !timed {
				continue
			}
			kind := targets[entry.ID]
			if list.kind == "stackable" {
				kind.Stackable = true
			} else {
				kind.Equipment = true
			}
			targets[entry.ID] = kind
		}
	}
	return targets, len(timedPaths), hash, nil
}

func hasItemTimeSection(raw []byte, resolve func(int) string) bool {
	if len(raw)%5 != 0 {
		return false
	}
	for offset := 0; offset < len(raw); offset += 5 {
		if raw[offset] != 3 {
			continue
		}
		value := int(int32(binary.LittleEndian.Uint32(raw[offset+1 : offset+5])))
		if _, found := timeSections[normalizeSection(resolve(value))]; found {
			return true
		}
	}
	return false
}

func normalizeSection(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if len(value) >= 2 && value[0] == '[' && value[len(value)-1] == ']' {
		value = strings.TrimSpace(value[1 : len(value)-1])
	}
	return value
}

func normalizeArchivePath(value string) string {
	return strings.ToLower(strings.TrimPrefix(filepath.ToSlash(filepath.Clean(strings.TrimSpace(value))), "./"))
}

func resolveListedPath(archive *platformpvf.Archive, root, listed string) string {
	listed = strings.Trim(strings.TrimSpace(listed), "`\"'")
	for _, candidate := range []string{listed, filepath.ToSlash(filepath.Join(root, filepath.FromSlash(listed)))} {
		if file, found := archive.FindFile(candidate); found {
			return file.ArchivePath
		}
	}
	return ""
}

func migrateMySQL(configuration instanceConfig, targets map[int64]itemKinds, apply bool) ([]containerReport, bool, error) {
	if !safeIdentifier.MatchString(configuration.Database.Name) {
		return nil, false, fmt.Errorf("unsafe database name %q", configuration.Database.Name)
	}
	mysqlConfig := mysql.NewConfig()
	mysqlConfig.User = configuration.Database.User
	mysqlConfig.Passwd = configuration.Database.Password
	mysqlConfig.Net = "tcp"
	mysqlConfig.Addr = net.JoinHostPort(configuration.Database.Host, strconv.Itoa(configuration.Database.Port))
	mysqlConfig.DBName = configuration.Database.Name
	mysqlConfig.ParseTime = true
	mysqlConfig.Loc = time.UTC
	database, err := sql.Open("mysql", mysqlConfig.FormatDSN())
	if err != nil {
		return nil, false, fmt.Errorf("open MySQL: %w", err)
	}
	defer database.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := database.PingContext(ctx); err != nil {
		return nil, false, fmt.Errorf("ping MySQL: %w", err)
	}
	tables, err := mysqlTables(ctx, database, configuration.Database.Name)
	if err != nil {
		return nil, false, err
	}
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, fmt.Errorf("begin MySQL item expiration migration: %w", err)
	}
	defer tx.Rollback()
	specs := []mysqlTableSpec{
		{itemSuffix: "account_inventory_items", extraSuffix: "account_inventory_item_extra", keyColumns: []string{"account_id", "entry_key"}},
		{itemSuffix: "inventory_items", extraSuffix: "inventory_item_extra", keyColumns: []string{"character_id", "collection_name", "entry_key"}},
		{itemSuffix: "equipment_entries", extraSuffix: "equipment_entry_extra", keyColumns: []string{"character_id", "entry_key"}},
		{itemSuffix: "pet_artifacts", extraSuffix: "pet_artifact_extra", keyColumns: []string{"character_id", "entry_key"}},
		{itemSuffix: "mail_attachments", extraSuffix: "mail_attachment_extra", keyColumns: []string{"character_id", "mail_id", "attachment_index"}},
	}
	reports := make([]containerReport, 0, len(specs))
	for _, spec := range specs {
		itemTable := tables[spec.itemSuffix]
		extraTable := tables[spec.extraSuffix]
		if itemTable == "" || extraTable == "" {
			continue
		}
		rowReport, migrateErr := migrateMySQLTable(ctx, tx, itemTable, extraTable, spec.keyColumns, targets, apply)
		if migrateErr != nil {
			return nil, false, migrateErr
		}
		reports = append(reports, rowReport)
	}
	if !apply {
		return reports, false, nil
	}
	if err := tx.Commit(); err != nil {
		return nil, false, fmt.Errorf("commit MySQL item expiration migration: %w", err)
	}
	return reports, true, nil
}

func mysqlTables(ctx context.Context, database *sql.DB, databaseName string) (map[string]string, error) {
	wanted := map[string]struct{}{
		"account_inventory_items": {}, "account_inventory_item_extra": {},
		"inventory_items": {}, "inventory_item_extra": {},
		"equipment_entries": {}, "equipment_entry_extra": {},
		"pet_artifacts": {}, "pet_artifact_extra": {},
		"mail_attachments": {}, "mail_attachment_extra": {},
	}
	rows, err := database.QueryContext(ctx, `SELECT table_name FROM information_schema.tables WHERE table_schema = ?`, databaseName)
	if err != nil {
		return nil, fmt.Errorf("list MySQL tables: %w", err)
	}
	defer rows.Close()
	result := make(map[string]string)
	for rows.Next() {
		var table string
		if err := rows.Scan(&table); err != nil {
			return nil, err
		}
		if !safeIdentifier.MatchString(table) {
			return nil, fmt.Errorf("unsafe table name %q", table)
		}
		for suffix := range wanted {
			if table == suffix || strings.HasSuffix(table, "_"+suffix) {
				result[suffix] = table
			}
		}
	}
	return result, rows.Err()
}

func migrateMySQLTable(ctx context.Context, tx *sql.Tx, itemTable, extraTable string, keyColumns []string, targets map[int64]itemKinds, apply bool) (containerReport, error) {
	result := containerReport{Container: itemTable}
	quotedKeys := make([]string, len(keyColumns))
	for index, column := range keyColumns {
		if !safeIdentifier.MatchString(column) {
			return result, fmt.Errorf("unsafe key column %q", column)
		}
		quotedKeys[index] = "`" + column + "`"
	}
	query := "SELECT " + strings.Join(quotedKeys, ", ") + ", item_id, expire_at, raw_entry FROM `" + itemTable + "`"
	rows, err := tx.QueryContext(ctx, query)
	if err != nil {
		return result, fmt.Errorf("scan %s: %w", itemTable, err)
	}
	var candidates []mysqlItemRow
	for rows.Next() {
		result.RowsScanned++
		row := mysqlItemRow{keys: make([]string, len(keyColumns))}
		destinations := make([]any, 0, len(keyColumns)+3)
		for index := range row.keys {
			destinations = append(destinations, &row.keys[index])
		}
		destinations = append(destinations, &row.itemID, &row.expireAt, &row.raw)
		if err := rows.Scan(destinations...); err != nil {
			rows.Close()
			return result, fmt.Errorf("read %s row: %w", itemTable, err)
		}
		if _, target := targets[row.itemID]; target {
			result.TargetRows++
			row.raw = append([]byte(nil), row.raw...)
			candidates = append(candidates, row)
		}
	}
	if err := rows.Close(); err != nil {
		return result, err
	}
	if err := rows.Err(); err != nil {
		return result, err
	}
	where := make([]string, len(keyColumns))
	for index, column := range quotedKeys {
		where[index] = column + " = ?"
	}
	for _, row := range candidates {
		extra, err := mysqlExtras(ctx, tx, extraTable, where, row.keys)
		if err != nil {
			return result, err
		}
		patchedRaw, patch := makePermanent(row.raw, row.expireAt, extra, targets[row.itemID])
		if patch.preserved {
			result.RuntimePreserved++
			continue
		}
		if !patch.changed {
			continue
		}
		result.RowsChanged++
		if !apply {
			continue
		}
		args := []any{patchedRaw}
		for _, key := range row.keys {
			args = append(args, key)
		}
		update := "UPDATE `" + itemTable + "` SET expire_at = NULL, raw_entry = ? WHERE " + strings.Join(where, " AND ")
		if _, err := tx.ExecContext(ctx, update, args...); err != nil {
			return result, fmt.Errorf("update %s expiration: %w", itemTable, err)
		}
		deleteArgs := make([]any, 0, len(row.keys)+len(expirationExtraKeys))
		for _, key := range row.keys {
			deleteArgs = append(deleteArgs, key)
		}
		placeholders := make([]string, len(expirationExtraKeys))
		for index, key := range expirationExtraKeys {
			placeholders[index] = "?"
			deleteArgs = append(deleteArgs, key)
		}
		deleteQuery := "DELETE FROM `" + extraTable + "` WHERE " + strings.Join(where, " AND ") + " AND LOWER(extra_key) IN (" + strings.Join(placeholders, ",") + ")"
		if _, err := tx.ExecContext(ctx, deleteQuery, deleteArgs...); err != nil {
			return result, fmt.Errorf("delete %s expiration metadata: %w", extraTable, err)
		}
	}
	return result, nil
}

func mysqlExtras(ctx context.Context, tx *sql.Tx, table string, where []string, keys []string) (map[string]string, error) {
	args := make([]any, len(keys))
	for index, key := range keys {
		args[index] = key
	}
	rows, err := tx.QueryContext(ctx, "SELECT extra_key, extra_value FROM `"+table+"` WHERE "+strings.Join(where, " AND "), args...)
	if err != nil {
		return nil, fmt.Errorf("read %s metadata: %w", table, err)
	}
	defer rows.Close()
	result := make(map[string]string)
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			return nil, err
		}
		result[strings.ToLower(strings.TrimSpace(key))] = value
	}
	return result, rows.Err()
}

func makePermanent(raw []byte, expireAt sql.NullTime, extra map[string]string, kinds itemKinds) ([]byte, patchResult) {
	if preserveRuntimeExpiration(extra) {
		return raw, patchResult{preserved: true}
	}
	changed := expireAt.Valid
	oldUnix := expirationUnixCandidates(raw, expireAt, extra)
	patched := append([]byte(nil), raw...)
	hadRawExpiry := false
	if len(patched) >= itemExpireOffset+4 {
		hadRawExpiry = binary.LittleEndian.Uint32(patched[itemExpireOffset:itemExpireOffset+4]) != 0
		if hadRawExpiry {
			binary.LittleEndian.PutUint32(patched[itemExpireOffset:itemExpireOffset+4], 0)
			changed = true
		}
	}
	if len(patched) >= legacyExpireOffset+4 {
		legacy := binary.LittleEndian.Uint32(patched[legacyExpireOffset : legacyExpireOffset+4])
		if legacy != 0 && oldUnix[legacy] {
			binary.LittleEndian.PutUint32(patched[legacyExpireOffset:legacyExpireOffset+4], 0)
			changed = true
		}
	}
	stackable := strings.EqualFold(strings.TrimSpace(extra["item_kind"]), "stackable") || (kinds.Stackable && !kinds.Equipment)
	hadMetadata := false
	for _, key := range expirationExtraKeys {
		if _, found := extra[key]; found {
			hadMetadata = true
			changed = true
		}
	}
	if stackable && len(patched) >= usePeriodOffset+2 && (expireAt.Valid || hadRawExpiry || hadMetadata) && binary.LittleEndian.Uint16(patched[usePeriodOffset:usePeriodOffset+2]) != 0 {
		binary.LittleEndian.PutUint16(patched[usePeriodOffset:usePeriodOffset+2], 0)
		changed = true
	}
	return patched, patchResult{changed: changed}
}

func preserveRuntimeExpiration(extra map[string]string) bool {
	if strings.EqualFold(strings.TrimSpace(extra["source"]), "pvf_rental") {
		return true
	}
	return strings.EqualFold(strings.TrimSpace(extra["expiration_source"]), "runtime_pvf_cera_shop_duration_grant")
}

func expirationUnixCandidates(raw []byte, expireAt sql.NullTime, extra map[string]string) map[uint32]bool {
	result := make(map[uint32]bool)
	if expireAt.Valid && expireAt.Time.Unix() > 0 && expireAt.Time.Unix() <= int64(^uint32(0)) {
		result[uint32(expireAt.Time.Unix())] = true
	}
	for _, key := range []string{"expire_time", "expire_unix"} {
		value, err := strconv.ParseUint(strings.TrimSpace(extra[key]), 10, 32)
		if err == nil && value != 0 {
			result[uint32(value)] = true
		}
	}
	if len(raw) >= itemExpireOffset+4 {
		if value := binary.LittleEndian.Uint32(raw[itemExpireOffset : itemExpireOffset+4]); value != 0 {
			result[value] = true
		}
	}
	return result
}

func migrateSQLite(path string, targets map[int64]itemKinds, apply bool) ([]containerReport, bool, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, false, fmt.Errorf("inspect SQLite database: %w", err)
	}
	database, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, false, fmt.Errorf("open SQLite: %w", err)
	}
	defer database.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, fmt.Errorf("begin SQLite item expiration migration: %w", err)
	}
	defer tx.Rollback()
	kinds := []string{"account_inventory", "inventory", "equipment", "pet", "mailbox"}
	reports := make([]containerReport, 0, len(kinds))
	for _, recordKind := range kinds {
		rowReport, migrateErr := migrateSQLiteKind(ctx, tx, recordKind, targets, apply)
		if migrateErr != nil {
			return nil, false, migrateErr
		}
		reports = append(reports, rowReport)
	}
	if !apply {
		return reports, false, nil
	}
	if err := tx.Commit(); err != nil {
		return nil, false, fmt.Errorf("commit SQLite item expiration migration: %w", err)
	}
	return reports, true, nil
}

type sqliteRecord struct {
	key      string
	payload  []byte
	checksum string
}

func migrateSQLiteKind(ctx context.Context, tx *sql.Tx, recordKind string, targets map[int64]itemKinds, apply bool) (containerReport, error) {
	result := containerReport{Container: "dnf_repository_records:" + recordKind}
	rows, err := tx.QueryContext(ctx, `SELECT record_key, payload, checksum FROM dnf_repository_records WHERE record_kind = ? ORDER BY record_key`, recordKind)
	if err != nil {
		return result, fmt.Errorf("scan SQLite %s records: %w", recordKind, err)
	}
	var records []sqliteRecord
	for rows.Next() {
		var record sqliteRecord
		if err := rows.Scan(&record.key, &record.payload, &record.checksum); err != nil {
			rows.Close()
			return result, err
		}
		result.RowsScanned++
		sum := sha256.Sum256(record.payload)
		if !strings.EqualFold(hex.EncodeToString(sum[:]), record.checksum) {
			rows.Close()
			return result, fmt.Errorf("SQLite checksum mismatch for %s %q", recordKind, record.key)
		}
		record.payload = append([]byte(nil), record.payload...)
		records = append(records, record)
	}
	if err := rows.Close(); err != nil {
		return result, err
	}
	for _, record := range records {
		patched, targetRows, changedRows, preservedRows, err := patchSQLitePayload(recordKind, record.payload, targets)
		if err != nil {
			return result, fmt.Errorf("patch SQLite %s %q: %w", recordKind, record.key, err)
		}
		result.TargetRows += targetRows
		result.RowsChanged += changedRows
		result.RuntimePreserved += preservedRows
		if !apply || changedRows == 0 {
			continue
		}
		sum := sha256.Sum256(patched)
		if _, err := tx.ExecContext(ctx, `UPDATE dnf_repository_records SET payload = ?, checksum = ?, updated_at = ? WHERE record_kind = ? AND record_key = ?`,
			patched, hex.EncodeToString(sum[:]), time.Now().UTC().Format(time.RFC3339Nano), recordKind, record.key); err != nil {
			return result, fmt.Errorf("update SQLite %s %q: %w", recordKind, record.key, err)
		}
	}
	return result, nil
}

func patchSQLitePayload(recordKind string, payload []byte, targets map[int64]itemKinds) ([]byte, int, int, int, error) {
	targetRows, changedRows, preservedRows := 0, 0, 0
	encode := func(value any) error {
		encoded, err := json.Marshal(value)
		if err == nil {
			payload = encoded
		}
		return err
	}
	patchStack := func(stack *dnfrepo.ItemStack) {
		kinds, target := targets[stack.ItemID]
		if !target {
			return
		}
		targetRows++
		patchedRaw, result := makePermanent(stack.RawEntry, nullTime(stack.ExpireAt), stack.Extra, kinds)
		if result.preserved {
			preservedRows++
			return
		}
		if !result.changed {
			return
		}
		stack.ExpireAt = time.Time{}
		stack.RawEntry = patchedRaw
		removeExpirationExtra(stack.Extra)
		changedRows++
	}
	switch recordKind {
	case "account_inventory":
		var record dnfrepo.AccountInventoryRecord
		if err := json.Unmarshal(payload, &record); err != nil {
			return nil, 0, 0, 0, err
		}
		for key, stack := range record.Slots {
			patchStack(&stack)
			record.Slots[key] = stack
		}
		if err := encode(record); err != nil {
			return nil, 0, 0, 0, err
		}
	case "inventory":
		var record dnfrepo.InventoryRecord
		if err := json.Unmarshal(payload, &record); err != nil {
			return nil, 0, 0, 0, err
		}
		for key, stack := range record.Slots {
			patchStack(&stack)
			record.Slots[key] = stack
		}
		for key, stack := range record.Warehouse {
			patchStack(&stack)
			record.Warehouse[key] = stack
		}
		if err := encode(record); err != nil {
			return nil, 0, 0, 0, err
		}
	case "equipment":
		var record dnfrepo.EquipmentRecord
		if err := json.Unmarshal(payload, &record); err != nil {
			return nil, 0, 0, 0, err
		}
		for key, entry := range record.Entries {
			kinds, target := targets[entry.ItemID]
			if !target {
				continue
			}
			targetRows++
			patchedRaw, result := makePermanent(entry.RawEntry, nullTime(entry.ExpireAt), entry.Extra, kinds)
			if result.preserved {
				preservedRows++
				continue
			}
			if result.changed {
				entry.ExpireAt = time.Time{}
				entry.RawEntry = patchedRaw
				removeExpirationExtra(entry.Extra)
				record.Entries[key] = entry
				changedRows++
			}
		}
		if err := encode(record); err != nil {
			return nil, 0, 0, 0, err
		}
	case "pet":
		var record dnfrepo.PetRecord
		if err := json.Unmarshal(payload, &record); err != nil {
			return nil, 0, 0, 0, err
		}
		for key, stack := range record.Artifacts {
			patchStack(&stack)
			record.Artifacts[key] = stack
		}
		if err := encode(record); err != nil {
			return nil, 0, 0, 0, err
		}
	case "mailbox":
		var record dnfrepo.MailboxRecord
		if err := json.Unmarshal(payload, &record); err != nil {
			return nil, 0, 0, 0, err
		}
		for mailKey, mail := range record.Mails {
			for index := range mail.Attachments {
				attachment := &mail.Attachments[index]
				kinds, target := targets[attachment.ItemID]
				if !target {
					continue
				}
				targetRows++
				patchedRaw, result := makePermanent(attachment.RawEntry, nullTime(attachment.ExpireAt), attachment.Extra, kinds)
				if result.preserved {
					preservedRows++
					continue
				}
				if result.changed {
					attachment.ExpireAt = time.Time{}
					attachment.RawEntry = patchedRaw
					removeExpirationExtra(attachment.Extra)
					changedRows++
				}
			}
			record.Mails[mailKey] = mail
		}
		if err := encode(record); err != nil {
			return nil, 0, 0, 0, err
		}
	default:
		return nil, 0, 0, 0, fmt.Errorf("unsupported record kind %q", recordKind)
	}
	return payload, targetRows, changedRows, preservedRows, nil
}

func nullTime(value time.Time) sql.NullTime {
	return sql.NullTime{Time: value, Valid: !value.IsZero()}
}

func removeExpirationExtra(extra map[string]string) {
	for _, key := range expirationExtraKeys {
		delete(extra, key)
	}
}

func hashFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read %s for hashing: %w", path, err)
	}
	sum := sha256.Sum256(data)
	return strings.ToUpper(hex.EncodeToString(sum[:])), nil
}
