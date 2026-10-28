package mysql

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"longheng.io/server/internal/modules/dnf/repository"
	"slices"
	"strings"
)

const mysqlPetTable = "pets"

type mysqlPetStore struct {
	mysqlStoreBase
}

// Load reads one character creature record from MySQL.
func (s *mysqlPetStore) Load(ctx context.Context, characterID string) (repository.PetRecord, bool, error) {
	table, err := s.router.readTable(mysqlPetTable, characterID)
	if err != nil {
		return repository.PetRecord{}, false, err
	}
	entriesTable, err := s.router.readTable(mysqlPetEntriesTable, characterID)
	if err != nil {
		return repository.PetRecord{}, false, err
	}
	extraTable, err := s.router.readTable(mysqlPetExtraTable, characterID)
	if err != nil {
		return repository.PetRecord{}, false, err
	}
	tokensTable, err := s.router.readTable(mysqlPetTokensTable, characterID)
	if err != nil {
		return repository.PetRecord{}, false, err
	}
	artifactsTable, err := s.router.readTable(mysqlPetArtifactsTable, characterID)
	if err != nil {
		return repository.PetRecord{}, false, err
	}
	artifactExtraTable, err := s.router.readTable(mysqlPetArtifactExtraTable, characterID)
	if err != nil {
		return repository.PetRecord{}, false, err
	}
	query := s.router.selectQuery("SELECT character_id, equipped_key, town_display, updated_at FROM " + table + " WHERE character_id = ?")
	var record repository.PetRecord
	var updatedAt sql.NullTime
	err = s.router.db.QueryRowContext(ctx, query, characterID).Scan(
		&record.CharacterID,
		&record.EquippedKey,
		&record.TownDisplay,
		&updatedAt,
	)
	if err != nil {
		ok, scanErr := scanErr(err)
		return repository.PetRecord{}, ok, scanErr
	}
	record.Entries, err = loadPetEntries(ctx, s.router, entriesTable, extraTable, tokensTable, characterID)
	if err != nil {
		return repository.PetRecord{}, false, err
	}
	record.Artifacts, err = loadItemStackCollection(
		ctx, s.router.db, artifactsTable, artifactExtraTable, "character_id", characterID, "", false,
	)
	if err != nil {
		return repository.PetRecord{}, false, err
	}
	record.UpdatedAt = scanTime(updatedAt)
	return repository.ClonePet(record), true, nil
}

// Save writes the complete creature record.
func (s *mysqlPetStore) Save(ctx context.Context, record repository.PetRecord) error {
	return s.SaveFields(ctx, record, repository.AllPetFields()...)
}

// SaveFields writes selected creature fields.
func (s *mysqlPetStore) SaveFields(ctx context.Context, record repository.PetRecord, fields ...repository.PetField) error {
	characterID, err := requireRecordKey(repository.PetKey, record, "pet")
	if err != nil {
		return err
	}
	fields = repository.PetFields.Normalize(fields)
	if len(fields) == 0 {
		return nil
	}
	table, err := s.router.writeTable(mysqlPetTable, characterID)
	if err != nil {
		return err
	}
	entriesTable, err := s.router.writeTable(mysqlPetEntriesTable, characterID)
	if err != nil {
		return err
	}
	extraTable, err := s.router.writeTable(mysqlPetExtraTable, characterID)
	if err != nil {
		return err
	}
	tokensTable, err := s.router.writeTable(mysqlPetTokensTable, characterID)
	if err != nil {
		return err
	}
	artifactsTable, err := s.router.writeTable(mysqlPetArtifactsTable, characterID)
	if err != nil {
		return err
	}
	artifactExtraTable, err := s.router.writeTable(mysqlPetArtifactExtraTable, characterID)
	if err != nil {
		return err
	}
	columns := []string{"character_id", "updated_at"}
	args := []any{characterID, timeOrNow(record.UpdatedAt, s.router.now)}
	updates := []string{updateValue("updated_at")}
	saveEntries := false
	for _, field := range fields {
		switch field {
		case repository.PetFieldEntries:
			saveEntries = true
		case repository.PetFieldEquipped:
			columns = append(columns, "equipped_key")
			args = append(args, record.EquippedKey)
			updates = append(updates, updateValue("equipped_key"))
		case repository.PetFieldDisplay:
			columns = append(columns, "town_display")
			args = append(args, record.TownDisplay)
			updates = append(updates, updateValue("town_display"))
		}
	}
	return withMySQLWriteExecutor(ctx, s.router.db, func(database SQLDB) error {
		if _, execErr := database.ExecContext(ctx, buildUpsert(table, columns, updates), args...); execErr != nil {
			return execErr
		}
		if !saveEntries {
			return nil
		}
		if err := replacePetEntries(ctx, database, entriesTable, extraTable, tokensTable, characterID, record.Entries); err != nil {
			return err
		}
		return replaceItemStackCollection(
			ctx,
			database,
			artifactsTable,
			artifactExtraTable,
			"character_id",
			characterID,
			"",
			false,
			record.Artifacts,
		)
	})
}

func loadPetEntries(
	ctx context.Context,
	router mysqlRouter,
	entriesTable, extraTable, tokensTable, characterID string,
) (map[string]repository.PetEntry, error) {
	return loadPetEntryRows(ctx, router.db, router.selectQuery, entriesTable, extraTable, tokensTable, characterID)
}

// loadPetEntryRows 经 database 读出宠物行；selectQuery 决定加锁后缀
// （读路径用 router 的，差异写用 " FOR UPDATE"）。
//
// 移植上游 0897707：本函数由原来的 loadPetEntries 改名并参数化而来，
// 函数体逐行未动，只把 router.selectQuery / router.db 换成传入的两个参数。
func loadPetEntryRows(
	ctx context.Context,
	database SQLDB,
	selectQuery func(string) string,
	entriesTable, extraTable, tokensTable, characterID string,
) (map[string]repository.PetEntry, error) {
	query := selectQuery("SELECT pet_key, creature_key, item_id, source_list_type, source_slot_index, pet_name, name_raw, satiety, satiety_micros, mode_flag, mode1_field_0a, mode1_field_0b, pet_level, pet_exp, tail_flag, raw_entry FROM " + entriesTable + " WHERE character_id = ? ORDER BY pet_key")
	rows, err := database.QueryContext(ctx, query, characterID)
	if err != nil {
		return nil, err
	}
	var entries map[string]repository.PetEntry
	for rows.Next() {
		var key string
		var entry repository.PetEntry
		var nameRaw, rawEntry []byte
		if err := rows.Scan(
			&key,
			&entry.CreatureKey,
			&entry.ItemID,
			&entry.SourceListType,
			&entry.SourceSlotIndex,
			&entry.Name,
			&nameRaw,
			&entry.Satiety,
			&entry.SatietyMicros,
			&entry.ModeFlag,
			&entry.Mode1Field0A,
			&entry.Mode1Field0B,
			&entry.Level,
			&entry.Exp,
			&entry.TailFlag,
			&rawEntry,
		); err != nil {
			rows.Close()
			return nil, err
		}
		entry.PetKey = key
		entry.NameRaw = append([]byte(nil), nameRaw...)
		entry.RawEntry = append([]byte(nil), rawEntry...)
		if entries == nil {
			entries = make(map[string]repository.PetEntry)
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

	extras, err := database.QueryContext(
		ctx,
		selectQuery("SELECT entry_key, extra_key, extra_value FROM "+extraTable+" WHERE character_id = ? ORDER BY entry_key, extra_key"),
		characterID,
	)
	if err != nil {
		return nil, err
	}
	for extras.Next() {
		var petKey, key, value string
		if err := extras.Scan(&petKey, &key, &value); err != nil {
			extras.Close()
			return nil, err
		}
		entry, ok := entries[petKey]
		if !ok {
			continue
		}
		if entry.Extra == nil {
			entry.Extra = make(map[string]string)
		}
		entry.Extra[key] = value
		entries[petKey] = entry
	}
	if err := extras.Err(); err != nil {
		extras.Close()
		return nil, err
	}
	if err := extras.Close(); err != nil {
		return nil, err
	}

	tokens, err := database.QueryContext(
		ctx,
		selectQuery("SELECT pet_key, token, applied FROM "+tokensTable+" WHERE character_id = ? ORDER BY pet_key, token_order"),
		characterID,
	)
	if err != nil {
		return nil, err
	}
	defer tokens.Close()
	for tokens.Next() {
		var petKey, token string
		var applied bool
		if err := tokens.Scan(&petKey, &token, &applied); err != nil {
			return nil, err
		}
		entry, ok := entries[petKey]
		if !ok {
			continue
		}
		if entry.AppliedClearTokens == nil {
			entry.AppliedClearTokens = make(map[string]bool)
		}
		entry.AppliedClearTokens[token] = applied
		entry.AppliedClearTokenOrder = append(entry.AppliedClearTokenOrder, token)
		entries[petKey] = entry
	}
	if err := tokens.Err(); err != nil {
		return nil, err
	}
	return entries, nil
}

func replacePetEntries(
	ctx context.Context,
	database SQLDB,
	entriesTable, extraTable, tokensTable, characterID string,
	entries map[string]repository.PetEntry,
) error {
	// 与锁住的持久行做差异，**按宠物、按部分**（主行 / 附加键 / 清除令牌）。
	// 上游 2026-09-24 的抓包：每次宠物保存 —— 包括每次移动前的那次成长结算 ——
	// 都会把全部宠物连同它们 119 个清除令牌（147 行）重写一遍，只为改一个饱食度。
	//
	// 移植上游 0897707。本函数原来是本树的「逐表全量删 + 批量插」形态；
	// 差异写的等价比它更强：没事的部分一条语句都不发。
	current, err := loadPetEntryRows(ctx, database, func(query string) string { return query + " FOR UPDATE" },
		entriesTable, extraTable, tokensTable, characterID)
	if err != nil {
		return err
	}
	var rowKeys, extraKeys, tokenKeys []string
	for _, petKey := range sortedStringKeys(entries) {
		entry := entries[petKey]
		old, found := current[petKey]
		if !found {
			// 新键在每一部分都要重写，这同时会清掉「有附加键/令牌却没有主行」的残留
			// （loader 会把那种行跳过）。
			rowKeys, extraKeys, tokenKeys = append(rowKeys, petKey), append(extraKeys, petKey), append(tokenKeys, petKey)
			continue
		}
		if !petEntryRowStoredEqual(old, entry) {
			rowKeys = append(rowKeys, petKey)
		}
		if !stringMapsEqual(old.Extra, entry.Extra) {
			extraKeys = append(extraKeys, petKey)
		}
		if !slices.Equal(petClearTokenRows(old), petClearTokenRows(entry)) {
			tokenKeys = append(tokenKeys, petKey)
		}
	}
	for _, petKey := range sortedStringKeys(current) {
		if _, kept := entries[petKey]; !kept {
			rowKeys, extraKeys, tokenKeys = append(rowKeys, petKey), append(extraKeys, petKey), append(tokenKeys, petKey)
		}
	}
	owner := []any{characterID}
	if err := deleteMySQLKeys(ctx, database, tokensTable, "character_id = ?", owner, "pet_key", tokenKeys); err != nil {
		return err
	}
	if err := deleteMySQLKeys(ctx, database, extraTable, "character_id = ?", owner, "entry_key", extraKeys); err != nil {
		return err
	}
	if err := deleteMySQLKeys(ctx, database, entriesTable, "character_id = ?", owner, "pet_key", rowKeys); err != nil {
		return err
	}
	entryRows := newMySQLBulkInserter(ctx, database, entriesTable,
		"character_id", "pet_key", "creature_key", "item_id", "source_list_type", "source_slot_index",
		"pet_name", "name_raw", "satiety", "satiety_micros", "mode_flag", "mode1_field_0a",
		"mode1_field_0b", "pet_level", "pet_exp", "tail_flag", "raw_entry")
	extraRows := newMySQLBulkInserter(ctx, database, extraTable, "character_id", "entry_key", "extra_key", "extra_value")
	tokenRows := newMySQLBulkInserter(ctx, database, tokensTable, "character_id", "pet_key", "token_order", "token", "applied")
	writeRow := keySet(rowKeys)
	writeExtra := keySet(extraKeys)
	writeTokens := keySet(tokenKeys)
	for _, petKey := range sortedStringKeys(entries) {
		entry := entries[petKey]
		if _, ok := writeRow[petKey]; ok {
			if err := entryRows.add(
				characterID,
				petKey,
				entry.CreatureKey,
				entry.ItemID,
				entry.SourceListType,
				entry.SourceSlotIndex,
				entry.Name,
				entry.NameRaw,
				entry.Satiety,
				entry.SatietyMicros,
				entry.ModeFlag,
				entry.Mode1Field0A,
				entry.Mode1Field0B,
				entry.Level,
				entry.Exp,
				entry.TailFlag,
				entry.RawEntry,
			); err != nil {
				return err
			}
		}
		if _, ok := writeExtra[petKey]; ok {
			for _, key := range sortedStringKeys(entry.Extra) {
				if err := extraRows.add(characterID, petKey, key, entry.Extra[key]); err != nil {
					return err
				}
			}
		}
		if _, ok := writeTokens[petKey]; ok {
			for index, row := range petClearTokenRows(entry) {
				if err := tokenRows.add(characterID, petKey, index, row.token, row.applied); err != nil {
					return err
				}
			}
		}
	}
	for _, rows := range []*mysqlBulkInserter{entryRows, extraRows, tokenRows} {
		if err := rows.flush(); err != nil {
			return err
		}
	}
	return nil
}

type petClearTokenRow struct {
	token   string
	applied bool
}

// petClearTokenRows 是**落库时的令牌顺序**：先按 AppliedClearTokenOrder 过滤掉未知令牌
// 与重复项，剩下的再按 key 排序补上。读出来的行原样过一遍，所以新旧可以直接比较。
func petClearTokenRows(entry repository.PetEntry) []petClearTokenRow {
	rows := make([]petClearTokenRow, 0, len(entry.AppliedClearTokens))
	seen := make(map[string]struct{}, len(entry.AppliedClearTokens))
	for _, token := range entry.AppliedClearTokenOrder {
		if _, ok := entry.AppliedClearTokens[token]; !ok {
			continue
		}
		if _, duplicate := seen[token]; duplicate {
			continue
		}
		seen[token] = struct{}{}
		rows = append(rows, petClearTokenRow{token: token, applied: entry.AppliedClearTokens[token]})
	}
	for _, token := range sortedStringKeys(entry.AppliedClearTokens) {
		if _, ok := seen[token]; ok {
			continue
		}
		rows = append(rows, petClearTokenRow{token: token, applied: entry.AppliedClearTokens[token]})
	}
	return rows
}

// petEntryRowStoredEqual 比较 pet_entries 的列；NameRaw/RawEntry 与 loader 一样
// 把 nil 与空视为相同。
func petEntryRowStoredEqual(a, b repository.PetEntry) bool {
	return a.CreatureKey == b.CreatureKey && a.ItemID == b.ItemID &&
		a.SourceListType == b.SourceListType && a.SourceSlotIndex == b.SourceSlotIndex &&
		a.Name == b.Name && bytes.Equal(a.NameRaw, b.NameRaw) &&
		a.Satiety == b.Satiety && a.SatietyMicros == b.SatietyMicros &&
		a.ModeFlag == b.ModeFlag && a.Mode1Field0A == b.Mode1Field0A && a.Mode1Field0B == b.Mode1Field0B &&
		a.Level == b.Level && a.Exp == b.Exp && a.TailFlag == b.TailFlag &&
		bytes.Equal(a.RawEntry, b.RawEntry)
}

func keySet(keys []string) map[string]struct{} {
	out := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		out[key] = struct{}{}
	}
	return out
}

const petEntriesEnvelopeVersion = 1

// petEntriesEnvelope extends the existing entries_json column without a
// schema migration. Legacy rows are a plain map of creature key to PetEntry;
// rows with equipped artifacts use this versioned envelope instead.
type petEntriesEnvelope struct {
	Version   int                             `json:"version"`
	Creatures map[string]repository.PetEntry  `json:"creatures,omitempty"`
	Artifacts map[string]repository.ItemStack `json:"artifacts,omitempty"`
}

func petEntriesJSONArg(record repository.PetRecord) (any, error) {
	if len(record.Artifacts) == 0 {
		return jsonArg(record.Entries)
	}
	return jsonArg(petEntriesEnvelope{
		Version:   petEntriesEnvelopeVersion,
		Creatures: record.Entries,
		Artifacts: record.Artifacts,
	})
}

func scanPetEntries(value sql.NullString, record *repository.PetRecord) error {
	if record == nil {
		return fmt.Errorf("pet record is nil")
	}
	if !value.Valid || strings.TrimSpace(value.String) == "" {
		return nil
	}

	var shape map[string]json.RawMessage
	if err := json.Unmarshal([]byte(value.String), &shape); err != nil {
		return err
	}
	_, hasVersion := shape["version"]
	_, hasCreatures := shape["creatures"]
	_, hasArtifacts := shape["artifacts"]
	if !hasVersion && !hasCreatures && !hasArtifacts {
		return json.Unmarshal([]byte(value.String), &record.Entries)
	}

	var envelope petEntriesEnvelope
	if err := json.Unmarshal([]byte(value.String), &envelope); err != nil {
		return err
	}
	if envelope.Version != petEntriesEnvelopeVersion {
		return fmt.Errorf("unsupported pet entries envelope version %d", envelope.Version)
	}
	record.Entries = envelope.Creatures
	record.Artifacts = envelope.Artifacts
	return nil
}
