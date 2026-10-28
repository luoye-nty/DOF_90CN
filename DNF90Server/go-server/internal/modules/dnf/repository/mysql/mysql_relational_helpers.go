package mysql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"longheng.io/server/internal/modules/dnf/repository"
	"sort"
	"strings"
)

func withMySQLWriteExecutor(ctx context.Context, database SQLDB, apply func(SQLDB) error) error {
	if apply == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	beginner, ok := database.(sqlTxBeginner)
	if !ok {
		return apply(database)
	}
	tx, err := beginner.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	txDB := sqlTransactionDB{tx: tx}
	if err := apply(txDB); err != nil {
		return errors.Join(err, tx.Rollback())
	}
	if err := ctx.Err(); err != nil {
		return errors.Join(err, tx.Rollback())
	}
	return tx.Commit()
}

func sortedStringKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func loadStringMap(
	ctx context.Context,
	database SQLDB,
	query string,
	ownerID string,
) (map[string]string, error) {
	rows, err := database.QueryContext(ctx, query, ownerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var values map[string]string
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			return nil, err
		}
		if values == nil {
			values = make(map[string]string)
		}
		values[key] = value
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return values, nil
}

// mysqlReplaceBatchSize bounds one multi-row VALUES statement so a full
// collection write stays well under max_allowed_packet even with large
// raw_entry blobs (0x77+ bytes per inventory row). It matches the bound
// replaceQuestStates already uses.
const mysqlReplaceBatchSize = 128

// buildMySQLBulkInsert renders one multi-row INSERT for `rows` tuples over
// `columns`; callers append the arguments row by row in the same column order.
// A full inventory save (DELETE then re-INSERT ~93 slot rows plus their extras)
// used to issue one ExecContext per row, which is where op26 disjoint spent
// most of its 340-420ms.
func buildMySQLBulkInsert(table string, columns []string, rows int) string {
	quoted := make([]string, 0, len(columns))
	for _, column := range columns {
		quoted = append(quoted, quoteSQLIdentifier(column))
	}
	placeholders := "(" + strings.TrimSuffix(strings.Repeat("?, ", len(columns)), ", ") + ")"
	values := make([]string, rows)
	for index := range values {
		values[index] = placeholders
	}
	return "INSERT INTO " + table + " (" + strings.Join(quoted, ", ") + ") VALUES " + strings.Join(values, ", ")
}

// mysqlBulkInserter 收集一张表的行，按 mysqlReplaceBatchSize 分块经
// buildMySQLBulkInsert 写出。「先删后插」的集合 writer 原来是每行一次
// ExecContext：上游 2026-09-24 的 general log 抓包显示，一次杀怪会逐行重插
// 48 行 character_stats，每次宠物保存会逐行写 100 行 pet_clear_tokens。
//
// 移植上游 89b6ea4 / 0897707（上游把它放在 mysql_relational_helpers.go）。
type mysqlBulkInserter struct {
	ctx      context.Context
	database SQLDB
	table    string
	columns  []string
	args     []any
	rows     int
}

func newMySQLBulkInserter(ctx context.Context, database SQLDB, table string, columns ...string) *mysqlBulkInserter {
	return &mysqlBulkInserter{ctx: ctx, database: database, table: table, columns: columns}
}

// add 排队一行；values 必须按构造函数的列顺序。
func (b *mysqlBulkInserter) add(values ...any) error {
	if len(values) != len(b.columns) {
		return fmt.Errorf("%w: bulk insert %s got %d values for %d columns", ErrMySQLConfigInvalid, b.table, len(values), len(b.columns))
	}
	b.args = append(b.args, values...)
	b.rows++
	if b.rows >= mysqlReplaceBatchSize {
		return b.flush()
	}
	return nil
}

// flush 写出已排队的行；调用者必须在最后一次 add 之后调用它一次。
func (b *mysqlBulkInserter) flush() error {
	if b.rows == 0 {
		return nil
	}
	if _, err := b.database.ExecContext(b.ctx, buildMySQLBulkInsert(b.table, b.columns, b.rows), b.args...); err != nil {
		return err
	}
	b.args = b.args[:0]
	b.rows = 0
	return nil
}

func replaceStringMap(
	ctx context.Context,
	database SQLDB,
	table, ownerColumn, ownerID string,
	values map[string]string,
) error {
	// 与锁住的持久行做差异（账号元数据、设置、数据包模板元数据）。上游 2026-09-24 的
	// general log 抓包显示：每个在线账号每 30 秒把全部 37 行 account_metadata 原样重写。
	//
	// 移植上游 a4d1fec。FOR UPDATE 直接写在事务内查询里（不走 mysqlRouter.selectQuery，
	// 那只在 lockReads 时才加锁）—— 否则读写分离下会读到旧行、误判差异。
	where := quoteSQLIdentifier(ownerColumn) + " = ?"
	current, err := loadStringMap(ctx, database,
		"SELECT entry_key, entry_value FROM "+table+" WHERE "+where+" FOR UPDATE", ownerID)
	if err != nil {
		return err
	}
	keys := make([]string, 0)
	for _, key := range sortedStringKeys(values) {
		if old, ok := current[key]; !ok || old != values[key] {
			keys = append(keys, key)
		}
	}
	deleteKeys := append([]string(nil), keys...)
	for _, key := range sortedStringKeys(current) {
		if _, ok := values[key]; !ok {
			deleteKeys = append(deleteKeys, key)
		}
	}
	if err := deleteMySQLEntryKeys(ctx, database, table, where, []any{ownerID}, deleteKeys); err != nil {
		return err
	}
	columns := []string{ownerColumn, "entry_key", "entry_value"}
	for start := 0; start < len(keys); start += mysqlReplaceBatchSize {
		end := min(start+mysqlReplaceBatchSize, len(keys))
		chunk := keys[start:end]
		args := make([]any, 0, len(chunk)*len(columns))
		for _, key := range chunk {
			args = append(args, ownerID, key, values[key])
		}
		if _, err := database.ExecContext(ctx, buildMySQLBulkInsert(table, columns, len(chunk)), args...); err != nil {
			return err
		}
	}
	return nil
}

func loadItemStackCollection(
	ctx context.Context,
	database SQLDB,
	itemTable, extraTable, ownerColumn, ownerID, collection string,
	withCollection bool,
) (map[string]repository.ItemStack, error) {
	return loadItemStackCollectionRows(ctx, database, itemTable, extraTable, ownerColumn, ownerID, collection, withCollection, false)
}

// loadItemStackCollectionRows 是带可选 FOR UPDATE 的 loadItemStackCollection，
// 供差异写锁住它要比对的那些行。
//
// 移植上游 c0ebdce。
func loadItemStackCollectionRows(
	ctx context.Context,
	database SQLDB,
	itemTable, extraTable, ownerColumn, ownerID, collection string,
	withCollection bool,
	forUpdate bool,
) (map[string]repository.ItemStack, error) {
	lock := ""
	if forUpdate {
		lock = " FOR UPDATE"
	}
	where := quoteSQLIdentifier(ownerColumn) + " = ?"
	args := []any{ownerID}
	if withCollection {
		where += " AND collection_name = ?"
		args = append(args, collection)
	}
	query := "SELECT entry_key, item_id, item_count, bind_flag, expire_at, raw_entry FROM " + itemTable + " WHERE " + where + " ORDER BY entry_key" + lock
	rows, err := database.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	var values map[string]repository.ItemStack
	for rows.Next() {
		var key string
		var stack repository.ItemStack
		var expireAt sql.NullTime
		var rawEntry []byte
		if err := rows.Scan(&key, &stack.ItemID, &stack.Count, &stack.Bind, &expireAt, &rawEntry); err != nil {
			rows.Close()
			return nil, err
		}
		stack.ExpireAt = scanTime(expireAt)
		stack.RawEntry = append([]byte(nil), rawEntry...)
		if values == nil {
			values = make(map[string]repository.ItemStack)
		}
		values[key] = stack
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}

	extraWhere := quoteSQLIdentifier(ownerColumn) + " = ?"
	extraArgs := []any{ownerID}
	if withCollection {
		extraWhere += " AND collection_name = ?"
		extraArgs = append(extraArgs, collection)
	}
	extraQuery := "SELECT entry_key, extra_key, extra_value FROM " + extraTable + " WHERE " + extraWhere + " ORDER BY entry_key, extra_key" + lock
	extras, err := database.QueryContext(ctx, extraQuery, extraArgs...)
	if err != nil {
		return nil, err
	}
	defer extras.Close()
	for extras.Next() {
		var entryKey, extraKey, extraValue string
		if err := extras.Scan(&entryKey, &extraKey, &extraValue); err != nil {
			return nil, err
		}
		stack, ok := values[entryKey]
		if !ok {
			continue
		}
		if stack.Extra == nil {
			stack.Extra = make(map[string]string)
		}
		stack.Extra[extraKey] = extraValue
		values[entryKey] = stack
	}
	if err := extras.Err(); err != nil {
		return nil, err
	}
	return values, nil
}

func replaceItemStackCollection(
	ctx context.Context,
	database SQLDB,
	itemTable, extraTable, ownerColumn, ownerID, collection string,
	withCollection bool,
	values map[string]repository.ItemStack,
) error {
	where := quoteSQLIdentifier(ownerColumn) + " = ?"
	args := []any{ownerID}
	if withCollection {
		where += " AND collection_name = ?"
		args = append(args, collection)
	}
	// 与锁住的持久行做差异，只碰真正变化的东西。上游 2026-09-24 的抓包显示：
	// 一次宠物换装会把整个背包与全部装备重写一遍（约 1700 行先删后插），只为挪三个字段。
	//
	// 移植上游 c0ebdce。
	current, err := loadItemStackCollectionRows(ctx, database, itemTable, extraTable, ownerColumn, ownerID, collection, withCollection, true)
	if err != nil {
		return err
	}
	writeKeys := make([]string, 0)
	for _, key := range sortedStringKeys(values) {
		if old, ok := current[key]; !ok || !itemStackStoredEqual(old, values[key]) {
			writeKeys = append(writeKeys, key)
		}
	}
	// 每个要写的键也先删：这同时会清掉「有附加行却没有物品行」的残留 —— loader 会跳过
	// 那种行，但如果不删，它会被挂到同一键上后来放的新物品上。
	deleteKeys := append([]string(nil), writeKeys...)
	for _, key := range sortedStringKeys(current) {
		if _, ok := values[key]; !ok {
			deleteKeys = append(deleteKeys, key)
		}
	}
	if err := deleteMySQLEntryKeys(ctx, database, extraTable, where, args, deleteKeys); err != nil {
		return err
	}
	if err := deleteMySQLEntryKeys(ctx, database, itemTable, where, args, deleteKeys); err != nil {
		return err
	}
	if len(writeKeys) == 0 {
		return nil
	}
	values = subsetItemStacks(values, writeKeys)

	var itemColumns, extraColumns []string
	if withCollection {
		itemColumns = []string{ownerColumn, "collection_name", "entry_key", "item_id", "item_count", "bind_flag", "expire_at", "raw_entry"}
		extraColumns = []string{ownerColumn, "collection_name", "entry_key", "extra_key", "extra_value"}
	} else {
		itemColumns = []string{ownerColumn, "entry_key", "item_id", "item_count", "bind_flag", "expire_at", "raw_entry"}
		extraColumns = []string{ownerColumn, "entry_key", "extra_key", "extra_value"}
	}

	// Rows are batched into bounded multi-row INSERTs; entry_key order and the
	// items-before-extras error semantics of the former per-row loop are kept.
	type extraRow struct {
		entryKey string
		key      string
		value    string
	}
	itemArgs := make([]any, 0, mysqlReplaceBatchSize*len(itemColumns))
	itemRowCount := 0
	extraRows := make([]extraRow, 0, mysqlReplaceBatchSize)
	flushItems := func() error {
		if itemRowCount == 0 {
			return nil
		}
		if _, err := database.ExecContext(ctx, buildMySQLBulkInsert(itemTable, itemColumns, itemRowCount), itemArgs...); err != nil {
			return err
		}
		itemArgs = itemArgs[:0]
		itemRowCount = 0
		return nil
	}
	flushExtras := func() error {
		if len(extraRows) == 0 {
			return nil
		}
		args := make([]any, 0, len(extraRows)*len(extraColumns))
		for _, row := range extraRows {
			if withCollection {
				args = append(args, ownerID, collection, row.entryKey, row.key, row.value)
			} else {
				args = append(args, ownerID, row.entryKey, row.key, row.value)
			}
		}
		if _, err := database.ExecContext(ctx, buildMySQLBulkInsert(extraTable, extraColumns, len(extraRows)), args...); err != nil {
			return err
		}
		extraRows = extraRows[:0]
		return nil
	}

	for _, entryKey := range sortedStringKeys(values) {
		stack := values[entryKey]
		if withCollection {
			itemArgs = append(itemArgs, ownerID, collection, entryKey, stack.ItemID, stack.Count, stack.Bind, sqlTime(stack.ExpireAt), stack.RawEntry)
		} else {
			itemArgs = append(itemArgs, ownerID, entryKey, stack.ItemID, stack.Count, stack.Bind, sqlTime(stack.ExpireAt), stack.RawEntry)
		}
		itemRowCount++
		if itemRowCount == mysqlReplaceBatchSize {
			if err := flushItems(); err != nil {
				return err
			}
		}
		for _, extraKey := range sortedStringKeys(stack.Extra) {
			extraRows = append(extraRows, extraRow{entryKey: entryKey, key: extraKey, value: stack.Extra[extraKey]})
			if len(extraRows) == mysqlReplaceBatchSize {
				if err := flushExtras(); err != nil {
					return err
				}
			}
		}
	}
	if err := flushItems(); err != nil {
		return err
	}
	return flushExtras()
}
