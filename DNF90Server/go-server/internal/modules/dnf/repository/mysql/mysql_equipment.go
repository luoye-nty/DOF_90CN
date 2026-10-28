// 本文件实现 DNF 穿戴装备仓储的 MySQL 字段化读写。
package mysql

import (
	"context"
	"database/sql"
	"longheng.io/server/internal/modules/dnf/repository"
)

const mysqlEquipmentTable = "equipments"

type mysqlEquipmentStore struct {
	mysqlStoreBase
}

// Load 按角色 ID 从 MySQL 读取穿戴装备记录。
func (s *mysqlEquipmentStore) Load(ctx context.Context, characterID string) (repository.EquipmentRecord, bool, error) {
	table, err := s.router.readTable(mysqlEquipmentTable, characterID)
	if err != nil {
		return repository.EquipmentRecord{}, false, err
	}
	entriesTable, err := s.router.readTable(mysqlEquipmentEntriesTable, characterID)
	if err != nil {
		return repository.EquipmentRecord{}, false, err
	}
	extraTable, err := s.router.readTable(mysqlEquipmentExtraTable, characterID)
	if err != nil {
		return repository.EquipmentRecord{}, false, err
	}
	query := s.router.selectQuery("SELECT character_id, updated_at FROM " + table + " WHERE character_id = ?")
	var record repository.EquipmentRecord
	var updatedAt sql.NullTime
	err = s.router.db.QueryRowContext(ctx, query, characterID).Scan(
		&record.CharacterID,
		&updatedAt,
	)
	if err != nil {
		ok, scanErr := scanErr(err)
		return repository.EquipmentRecord{}, ok, scanErr
	}
	record.Entries, err = loadEquipmentEntries(ctx, s.router, entriesTable, extraTable, characterID)
	if err != nil {
		return repository.EquipmentRecord{}, false, err
	}
	record.UpdatedAt = scanTime(updatedAt)
	return repository.CloneEquipment(record), true, nil
}

// Save 保存完整穿戴装备记录。
func (s *mysqlEquipmentStore) Save(ctx context.Context, record repository.EquipmentRecord) error {
	return s.SaveFields(ctx, record, repository.AllEquipmentFields()...)
}

// SaveFields 保存穿戴装备 dirty 字段。
func (s *mysqlEquipmentStore) SaveFields(ctx context.Context, record repository.EquipmentRecord, fields ...repository.EquipmentField) error {
	characterID, err := requireRecordKey(repository.EquipmentKey, record, "equipment")
	if err != nil {
		return err
	}
	fields = repository.EquipmentFields.Normalize(fields)
	if len(fields) == 0 {
		return nil
	}
	table, err := s.router.writeTable(mysqlEquipmentTable, characterID)
	if err != nil {
		return err
	}
	entriesTable, err := s.router.writeTable(mysqlEquipmentEntriesTable, characterID)
	if err != nil {
		return err
	}
	extraTable, err := s.router.writeTable(mysqlEquipmentExtraTable, characterID)
	if err != nil {
		return err
	}
	columns := []string{"character_id", "updated_at"}
	args := []any{characterID, timeOrNow(record.UpdatedAt, s.router.now)}
	updates := []string{updateValue("updated_at")}
	saveEntries := false
	for _, field := range fields {
		if field == repository.EquipmentFieldEntries {
			saveEntries = true
		}
	}
	return withMySQLWriteExecutor(ctx, s.router.db, func(database SQLDB) error {
		if _, execErr := database.ExecContext(ctx, buildUpsert(table, columns, updates), args...); execErr != nil {
			return execErr
		}
		if !saveEntries {
			return nil
		}
		return replaceEquipmentEntries(ctx, database, entriesTable, extraTable, characterID, record.Entries)
	})
}

func loadEquipmentEntries(
	ctx context.Context,
	router mysqlRouter,
	entriesTable, extraTable, characterID string,
) (map[string]repository.EquipmentEntry, error) {
	query := router.selectQuery("SELECT entry_key, slot_index, item_id, bind_flag, expire_at, raw_entry FROM " + entriesTable + " WHERE character_id = ? ORDER BY entry_key")
	rows, err := router.db.QueryContext(ctx, query, characterID)
	if err != nil {
		return nil, err
	}
	var entries map[string]repository.EquipmentEntry
	for rows.Next() {
		var key string
		var entry repository.EquipmentEntry
		var expireAt sql.NullTime
		var rawEntry []byte
		if err := rows.Scan(&key, &entry.SlotIndex, &entry.ItemID, &entry.Bind, &expireAt, &rawEntry); err != nil {
			rows.Close()
			return nil, err
		}
		entry.ExpireAt = scanTime(expireAt)
		entry.RawEntry = append([]byte(nil), rawEntry...)
		if entries == nil {
			entries = make(map[string]repository.EquipmentEntry)
		}
		entries[key] = entry
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	extras, err := router.db.QueryContext(ctx, router.selectQuery("SELECT entry_key, extra_key, extra_value FROM "+extraTable+" WHERE character_id = ? ORDER BY entry_key, extra_key"), characterID)
	if err != nil {
		return nil, err
	}
	defer extras.Close()
	for extras.Next() {
		var entryKey, extraKey, extraValue string
		if err := extras.Scan(&entryKey, &extraKey, &extraValue); err != nil {
			return nil, err
		}
		entry, ok := entries[entryKey]
		if !ok {
			continue
		}
		if entry.Extra == nil {
			entry.Extra = make(map[string]string)
		}
		entry.Extra[extraKey] = extraValue
		entries[entryKey] = entry
	}
	if err := extras.Err(); err != nil {
		return nil, err
	}
	return entries, nil
}

// loadEquipmentEntriesForUpdate 在写者的事务里读出并锁住已存的条目，
// 返回的形态与 loadEquipmentEntries 相同。
//
// 移植上游 c0ebdce。FOR UPDATE 直接写在查询里（不走 router.selectQuery，
// 那只在 lockReads 时才加锁）—— 否则读写分离下会读到旧行、误判差异。
func loadEquipmentEntriesForUpdate(
	ctx context.Context,
	database SQLDB,
	entriesTable, extraTable, characterID string,
) (map[string]repository.EquipmentEntry, error) {
	rows, err := database.QueryContext(ctx, "SELECT entry_key, slot_index, item_id, bind_flag, expire_at, raw_entry FROM "+entriesTable+" WHERE character_id = ? ORDER BY entry_key FOR UPDATE", characterID)
	if err != nil {
		return nil, err
	}
	entries := make(map[string]repository.EquipmentEntry)
	for rows.Next() {
		var key string
		var entry repository.EquipmentEntry
		var expireAt sql.NullTime
		var rawEntry []byte
		if err := rows.Scan(&key, &entry.SlotIndex, &entry.ItemID, &entry.Bind, &expireAt, &rawEntry); err != nil {
			rows.Close()
			return nil, err
		}
		entry.ExpireAt = scanTime(expireAt)
		entry.RawEntry = append([]byte(nil), rawEntry...)
		entries[key] = entry
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	extras, err := database.QueryContext(ctx, "SELECT entry_key, extra_key, extra_value FROM "+extraTable+" WHERE character_id = ? ORDER BY entry_key, extra_key FOR UPDATE", characterID)
	if err != nil {
		return nil, err
	}
	defer extras.Close()
	for extras.Next() {
		var entryKey, extraKey, extraValue string
		if err := extras.Scan(&entryKey, &extraKey, &extraValue); err != nil {
			return nil, err
		}
		entry, ok := entries[entryKey]
		if !ok {
			continue
		}
		if entry.Extra == nil {
			entry.Extra = make(map[string]string)
		}
		entry.Extra[extraKey] = extraValue
		entries[entryKey] = entry
	}
	return entries, extras.Err()
}

func replaceEquipmentEntries(
	ctx context.Context,
	database SQLDB,
	entriesTable, extraTable, characterID string,
	entries map[string]repository.EquipmentEntry,
) error {
	// 与锁住的持久行做差异，只碰真正变化的条目；理由同 replaceItemStackCollection
	// （上游 c0ebdce）。
	current, err := loadEquipmentEntriesForUpdate(ctx, database, entriesTable, extraTable, characterID)
	if err != nil {
		return err
	}
	writeKeys := make([]string, 0)
	for _, key := range sortedStringKeys(entries) {
		if old, ok := current[key]; !ok || !equipmentEntryStoredEqual(old, entries[key]) {
			writeKeys = append(writeKeys, key)
		}
	}
	deleteKeys := append([]string(nil), writeKeys...)
	for _, key := range sortedStringKeys(current) {
		if _, ok := entries[key]; !ok {
			deleteKeys = append(deleteKeys, key)
		}
	}
	owner := []any{characterID}
	if err := deleteMySQLEntryKeys(ctx, database, extraTable, "character_id = ?", owner, deleteKeys); err != nil {
		return err
	}
	if err := deleteMySQLEntryKeys(ctx, database, entriesTable, "character_id = ?", owner, deleteKeys); err != nil {
		return err
	}
	// 改前是「每个条目一条 ExecContext + 每个附加键再一条」，按 128 行一批改写后
	// 语句数与条目数无关。与背包 / 任务 / roster 路径用的同一个 buildMySQLBulkInsert。
	//
	// 两张表的写入顺序由「条目与其附加键交错」变为「先全部条目、再全部附加键」：
	// 它们分属不同的表且键在两处都唯一，因此结果集完全一致；整段仍在该替换事务内，
	// 中途失败由事务回滚，不产生半写状态。
	entryColumns := []string{
		"character_id", "entry_key", "slot_index", "item_id", "bind_flag", "expire_at", "raw_entry",
	}
	extraColumns := []string{"character_id", "entry_key", "extra_key", "extra_value"}
	// 只写差异出来的条目（上游 c0ebdce）。writeKeys 由 sortedStringKeys(entries) 过滤而来，
	// 所以顺序与原来的全量写一致。
	entryKeys := writeKeys
	for start := 0; start < len(entryKeys); start += mysqlReplaceBatchSize {
		end := min(start+mysqlReplaceBatchSize, len(entryKeys))
		chunk := entryKeys[start:end]
		args := make([]any, 0, len(chunk)*len(entryColumns))
		for _, entryKey := range chunk {
			entry := entries[entryKey]
			args = append(args,
				characterID, entryKey, entry.SlotIndex, entry.ItemID, entry.Bind,
				sqlTime(entry.ExpireAt), entry.RawEntry)
		}
		if _, err := database.ExecContext(
			ctx,
			buildMySQLBulkInsert(entriesTable, entryColumns, len(chunk)),
			args...,
		); err != nil {
			return err
		}
	}
	type equipmentExtraRow struct {
		entryKey string
		extraKey string
		value    any
	}
	extras := make([]equipmentExtraRow, 0, len(entryKeys))
	for _, entryKey := range entryKeys {
		entry := entries[entryKey]
		for _, extraKey := range sortedStringKeys(entry.Extra) {
			extras = append(extras, equipmentExtraRow{
				entryKey: entryKey,
				extraKey: extraKey,
				value:    entry.Extra[extraKey],
			})
		}
	}
	for start := 0; start < len(extras); start += mysqlReplaceBatchSize {
		end := min(start+mysqlReplaceBatchSize, len(extras))
		chunk := extras[start:end]
		args := make([]any, 0, len(chunk)*len(extraColumns))
		for _, row := range chunk {
			args = append(args, characterID, row.entryKey, row.extraKey, row.value)
		}
		if _, err := database.ExecContext(
			ctx,
			buildMySQLBulkInsert(extraTable, extraColumns, len(chunk)),
			args...,
		); err != nil {
			return err
		}
	}
	return nil
}
