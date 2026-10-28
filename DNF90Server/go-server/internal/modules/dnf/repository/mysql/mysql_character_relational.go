package mysql

import (
	"context"
	"database/sql"
	"errors"
	"longheng.io/server/internal/modules/dnf/repository"
)

func loadCharacterSupplement(
	ctx context.Context,
	router mysqlRouter,
	characterID string,
	record *repository.CharacterRecord,
) error {
	statsTable, err := router.readTable(mysqlCharacterStatsTable, characterID)
	if err != nil {
		return err
	}
	locationTable, err := router.readTable(mysqlCharacterLocationTable, characterID)
	if err != nil {
		return err
	}
	rosterTable, err := router.readTable(mysqlCharacterRosterTable, characterID)
	if err != nil {
		return err
	}
	equipTable, err := router.readTable(mysqlCharacterRosterEquipTable, characterID)
	if err != nil {
		return err
	}
	listTable, err := router.readTable(mysqlCharacterRosterListTable, characterID)
	if err != nil {
		return err
	}

	rows, err := router.db.QueryContext(
		ctx,
		router.selectQuery("SELECT stat_key, stat_value FROM "+statsTable+" WHERE character_id = ? ORDER BY stat_key"),
		characterID,
	)
	if err != nil {
		return err
	}
	for rows.Next() {
		var key string
		var value int64
		if err := rows.Scan(&key, &value); err != nil {
			rows.Close()
			return err
		}
		if record.Stats == nil {
			record.Stats = make(map[string]int64)
		}
		record.Stats[key] = value
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}

	err = router.db.QueryRowContext(
		ctx,
		router.selectQuery("SELECT channel_id, town_id, dungeon_id, room_id FROM "+locationTable+" WHERE character_id = ?"),
		characterID,
	).Scan(
		&record.Location.ChannelID,
		&record.Location.TownID,
		&record.Location.DungeonID,
		&record.Location.RoomID,
	)
	if err != nil && !errorsIsNoRows(err) {
		return err
	}

	if err := loadCharacterRoster(ctx, router, rosterTable, equipTable, listTable, characterID, &record.Roster); err != nil {
		return err
	}
	return nil
}

func errorsIsNoRows(err error) bool {
	return errors.Is(err, sql.ErrNoRows)
}

func saveCharacterSupplement(
	ctx context.Context,
	database SQLDB,
	router mysqlRouter,
	characterID string,
	record repository.CharacterRecord,
	fields map[repository.CharacterField]struct{},
) error {
	if _, ok := fields[repository.CharacterFieldStats]; ok {
		statsTable, err := router.writeTable(mysqlCharacterStatsTable, characterID)
		if err != nil {
			return err
		}
		if err := replaceCharacterExtraStats(ctx, database, statsTable, characterID, record.Stats); err != nil {
			return err
		}
	}
	if _, ok := fields[repository.CharacterFieldLocation]; ok {
		locationTable, err := router.writeTable(mysqlCharacterLocationTable, characterID)
		if err != nil {
			return err
		}
		columns := []string{"character_id", "channel_id", "town_id", "dungeon_id", "room_id"}
		updates := []string{
			updateValue("channel_id"),
			updateValue("town_id"),
			updateValue("dungeon_id"),
			updateValue("room_id"),
		}
		if _, err := database.ExecContext(
			ctx,
			buildUpsert(locationTable, columns, updates),
			characterID,
			record.Location.ChannelID,
			record.Location.TownID,
			record.Location.DungeonID,
			record.Location.RoomID,
		); err != nil {
			return err
		}
	}
	if _, ok := fields[repository.CharacterFieldRoster]; ok {
		rosterTable, err := router.writeTable(mysqlCharacterRosterTable, characterID)
		if err != nil {
			return err
		}
		equipTable, err := router.writeTable(mysqlCharacterRosterEquipTable, characterID)
		if err != nil {
			return err
		}
		listTable, err := router.writeTable(mysqlCharacterRosterListTable, characterID)
		if err != nil {
			return err
		}
		if err := replaceCharacterRoster(ctx, database, rosterTable, equipTable, listTable, characterID, record.Roster); err != nil {
			return err
		}
	}
	return nil
}

func replaceCharacterExtraStats(
	ctx context.Context,
	database SQLDB,
	table, characterID string,
	stats map[string]int64,
) error {
	if _, err := database.ExecContext(ctx, "DELETE FROM "+table+" WHERE character_id = ?", characterID); err != nil {
		return err
	}
	known := make(map[string]struct{}, len(mysqlCharacterStatColumns))
	for _, stat := range mysqlCharacterStatColumns {
		known[stat.key] = struct{}{}
	}
	// 先过滤出真正要写入的键（顺序仍按 sortedStringKeys），再按 128 行一批写入，
	// 与背包 / 任务 / roster 路径一致。改前是每个键一条 ExecContext —— 而这个函数
	// 几乎每个存档事务都会走到（包括"每杀一只怪"那个结算事务内部）。
	insertKeys := make([]string, 0, len(stats))
	for _, key := range sortedStringKeys(stats) {
		if _, ok := known[key]; ok {
			continue
		}
		insertKeys = append(insertKeys, key)
	}
	columns := []string{"character_id", "stat_key", "stat_value"}
	for start := 0; start < len(insertKeys); start += mysqlReplaceBatchSize {
		end := min(start+mysqlReplaceBatchSize, len(insertKeys))
		chunk := insertKeys[start:end]
		args := make([]any, 0, len(chunk)*len(columns))
		for _, key := range chunk {
			args = append(args, characterID, key, stats[key])
		}
		if _, err := database.ExecContext(
			ctx,
			buildMySQLBulkInsert(table, columns, len(chunk)),
			args...,
		); err != nil {
			return err
		}
	}
	return nil
}

func characterRosterColumns() []string {
	return []string{
		"character_id",
		"header_unk_a",
		"header_unk_b",
		"header_total_or_slot_limit",
		"header_used_or_remain",
		"header_selected_or_page",
		"header_roster_state",
		"header_page_count",
		"header_roster_flag",
		"header_roster_value_a",
		"header_roster_value_b",
		"entry_byte_a",
		"entry_packed_job_grow",
		"entry_byte_c",
		"entry_field_2cc",
		"entry_state0",
		"entry_time_a",
		"entry_time_b",
		"entry_value0",
		"entry_value1",
		"entry_value2",
		"entry_reserved_a",
		"entry_reserved_b",
		"entry_value3",
		"entry_object_id",
		"entry_flag0_eq1",
		"entry_special_status_flag",
		"entry_value5",
		"entry_display_flags",
		"entry_reserved_c",
		"entry_reserved_d",
		"entry_value6",
		"entry_flag1_nonzero",
		"entry_bool_a_eq1",
		"entry_bool_b_eq1",
		"entry_bool_c_eq1",
		"entry_flag2_nonzero",
		"entry_flag3_nonzero",
		"entry_flag4_nonzero",
		"entry_flag5_nonzero",
		"entry_value7",
		"entry_flag6_eq1",
	}
}

func characterRosterArgs(characterID string, roster repository.CharacterRoster) []any {
	header := roster.Header
	entry := roster.Entry
	return []any{
		characterID,
		header.UnkA,
		header.UnkB,
		header.TotalOrSlotLimit,
		header.UsedOrRemain,
		header.SelectedOrPage,
		header.RosterState,
		header.PageCount,
		header.RosterFlag,
		header.RosterValueA,
		header.RosterValueB,
		entry.ByteA,
		entry.PackedJobGrow,
		entry.ByteC,
		entry.Field2CC,
		entry.State0,
		entry.TimeA,
		entry.TimeB,
		entry.Value0,
		entry.Value1,
		entry.Value2,
		entry.ReservedA,
		entry.ReservedB,
		entry.Value3,
		entry.ObjectID,
		entry.Flag0Eq1,
		entry.SpecialStatusFlag,
		entry.Value5,
		entry.DisplayFlags,
		entry.ReservedC,
		entry.ReservedD,
		entry.Value6,
		entry.Flag1Nonzero,
		entry.BoolAEq1,
		entry.BoolBEq1,
		entry.BoolCEq1,
		entry.Flag2Nonzero,
		entry.Flag3Nonzero,
		entry.Flag4Nonzero,
		entry.Flag5Nonzero,
		entry.Value7,
		entry.Flag6Eq1,
	}
}

func characterRosterScanDest(roster *repository.CharacterRoster) []any {
	header := &roster.Header
	entry := &roster.Entry
	return []any{
		&header.UnkA,
		&header.UnkB,
		&header.TotalOrSlotLimit,
		&header.UsedOrRemain,
		&header.SelectedOrPage,
		&header.RosterState,
		&header.PageCount,
		&header.RosterFlag,
		&header.RosterValueA,
		&header.RosterValueB,
		&entry.ByteA,
		&entry.PackedJobGrow,
		&entry.ByteC,
		&entry.Field2CC,
		&entry.State0,
		&entry.TimeA,
		&entry.TimeB,
		&entry.Value0,
		&entry.Value1,
		&entry.Value2,
		&entry.ReservedA,
		&entry.ReservedB,
		&entry.Value3,
		&entry.ObjectID,
		&entry.Flag0Eq1,
		&entry.SpecialStatusFlag,
		&entry.Value5,
		&entry.DisplayFlags,
		&entry.ReservedC,
		&entry.ReservedD,
		&entry.Value6,
		&entry.Flag1Nonzero,
		&entry.BoolAEq1,
		&entry.BoolBEq1,
		&entry.BoolCEq1,
		&entry.Flag2Nonzero,
		&entry.Flag3Nonzero,
		&entry.Flag4Nonzero,
		&entry.Flag5Nonzero,
		&entry.Value7,
		&entry.Flag6Eq1,
	}
}

func loadCharacterRoster(
	ctx context.Context,
	router mysqlRouter,
	rosterTable, equipTable, listTable, characterID string,
	roster *repository.CharacterRoster,
) error {
	columns := characterRosterColumns()[1:]
	query := router.selectQuery("SELECT " + quotedColumns(columns) + " FROM " + rosterTable + " WHERE character_id = ?")
	if err := router.db.QueryRowContext(ctx, query, characterID).Scan(characterRosterScanDest(roster)...); err != nil {
		if errorsIsNoRows(err) {
			return nil
		}
		return err
	}
	equipmentRows, err := router.db.QueryContext(
		ctx,
		router.selectQuery("SELECT slot, item_id_or_icon, raw_entry, packed_flags, optional_id_or_expire, aux_value, aux_flag FROM "+equipTable+" WHERE character_id = ? ORDER BY ordinal"),
		characterID,
	)
	if err != nil {
		return err
	}
	for equipmentRows.Next() {
		var row repository.CharacterRosterEquipSummary
		var rawEntry []byte
		if err := equipmentRows.Scan(
			&row.Slot,
			&row.ItemIDOrIcon,
			&rawEntry,
			&row.PackedFlags,
			&row.OptionalIDOrExpire,
			&row.AuxValue,
			&row.AuxFlag,
		); err != nil {
			equipmentRows.Close()
			return err
		}
		row.RawEntry = append([]byte(nil), rawEntry...)
		roster.Entry.EquipSummary = append(roster.Entry.EquipSummary, row)
	}
	if err := equipmentRows.Err(); err != nil {
		equipmentRows.Close()
		return err
	}
	if err := equipmentRows.Close(); err != nil {
		return err
	}
	listRows, err := router.db.QueryContext(
		ctx,
		router.selectQuery("SELECT list_name, int_value FROM "+listTable+" WHERE character_id = ? ORDER BY list_name, ordinal"),
		characterID,
	)
	if err != nil {
		return err
	}
	defer listRows.Close()
	for listRows.Next() {
		var listName string
		var value int64
		if err := listRows.Scan(&listName, &value); err != nil {
			return err
		}
		switch listName {
		case "flags":
			roster.Entry.Flags = append(roster.Entry.Flags, value)
		case "linked_ids":
			roster.Entry.LinkedIDBlock = append(roster.Entry.LinkedIDBlock, value)
		}
	}
	if err := listRows.Err(); err != nil {
		return err
	}
	return nil
}

func replaceCharacterRoster(
	ctx context.Context,
	database SQLDB,
	rosterTable, equipTable, listTable, characterID string,
	roster repository.CharacterRoster,
) error {
	if _, err := database.ExecContext(ctx, "DELETE FROM "+equipTable+" WHERE character_id = ?", characterID); err != nil {
		return err
	}
	if _, err := database.ExecContext(ctx, "DELETE FROM "+listTable+" WHERE character_id = ?", characterID); err != nil {
		return err
	}
	if isZeroCharacterRoster(roster) {
		_, err := database.ExecContext(ctx, "DELETE FROM "+rosterTable+" WHERE character_id = ?", characterID)
		return err
	}
	columns := characterRosterColumns()
	updates := make([]string, 0, len(columns)-1)
	for _, column := range columns[1:] {
		updates = append(updates, updateValue(column))
	}
	if _, err := database.ExecContext(ctx, buildUpsert(rosterTable, columns, updates), characterRosterArgs(characterID, roster)...); err != nil {
		return err
	}
	// 下面三段原本是「每行一条 ExecContext」。一次 Character.Save() 会因此产生
	// 约 46 条语句（roster upsert + 每条装备摘要 1 条 + 每条 linked_id 1 条 +
	// 每个 flag 1 条），而 Save 几乎出现在每个结算事务里。同一个包里的背包
	// （replaceStringMap / buildMySQLBulkInsert）与任务路径早就按 128 行一批
	// 批量化了，只有 roster 漏了 —— 见 mysql_relational_helpers.go:83-99 的注释。
	//
	// 改为分块多行 INSERT 后，同样的内容只需 3 条语句（装备 / linked_ids / flags
	// 各一条）。顺序、序号与参数完全不变：ordinal 仍然是该行在整个切片里的下标。
	equipmentColumns := []string{
		"character_id", "ordinal", "slot", "item_id_or_icon", "raw_entry",
		"packed_flags", "optional_id_or_expire", "aux_value", "aux_flag",
	}
	for start := 0; start < len(roster.Entry.EquipSummary); start += mysqlReplaceBatchSize {
		end := min(start+mysqlReplaceBatchSize, len(roster.Entry.EquipSummary))
		chunk := roster.Entry.EquipSummary[start:end]
		args := make([]any, 0, len(chunk)*len(equipmentColumns))
		for index, row := range chunk {
			args = append(args,
				characterID,
				start+index,
				row.Slot,
				row.ItemIDOrIcon,
				row.RawEntry,
				row.PackedFlags,
				row.OptionalIDOrExpire,
				row.AuxValue,
				row.AuxFlag,
			)
		}
		if _, err := database.ExecContext(
			ctx,
			buildMySQLBulkInsert(equipTable, equipmentColumns, len(chunk)),
			args...,
		); err != nil {
			return err
		}
	}
	listColumns := []string{"character_id", "list_name", "ordinal", "int_value"}
	for start := 0; start < len(roster.Entry.LinkedIDBlock); start += mysqlReplaceBatchSize {
		end := min(start+mysqlReplaceBatchSize, len(roster.Entry.LinkedIDBlock))
		chunk := roster.Entry.LinkedIDBlock[start:end]
		args := make([]any, 0, len(chunk)*len(listColumns))
		for index, value := range chunk {
			args = append(args, characterID, "linked_ids", start+index, value)
		}
		if _, err := database.ExecContext(
			ctx,
			buildMySQLBulkInsert(listTable, listColumns, len(chunk)),
			args...,
		); err != nil {
			return err
		}
	}
	for start := 0; start < len(roster.Entry.Flags); start += mysqlReplaceBatchSize {
		end := min(start+mysqlReplaceBatchSize, len(roster.Entry.Flags))
		chunk := roster.Entry.Flags[start:end]
		args := make([]any, 0, len(chunk)*len(listColumns))
		for index, value := range chunk {
			args = append(args, characterID, "flags", start+index, value)
		}
		if _, err := database.ExecContext(
			ctx,
			buildMySQLBulkInsert(listTable, listColumns, len(chunk)),
			args...,
		); err != nil {
			return err
		}
	}
	return nil
}
