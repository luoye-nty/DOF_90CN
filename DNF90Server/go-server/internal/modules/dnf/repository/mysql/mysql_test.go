package mysql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"longheng.io/server/internal/modules/dnf/dungeonpermission"
	"longheng.io/server/internal/modules/dnf/repository"
	"slices"
	"strings"
	"testing"
	"time"
)

type fakeSQLCall struct {
	Query string
	Args  []any
}

func TestMySQLGroupExposesAtomicCharacterSettlement(t *testing.T) {
	database, err := sql.Open("mysql", "root:password@tcp(127.0.0.1:1)/dnf")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	group, err := NewMySQLGroupFromDB(database, MySQLGroupOptions{DatabasePlan: repository.DatabasePlan{
		Feature: repository.FeatureRepository, ShardID: "9999", GroupID: "test",
		ReadDatabases: []string{"dnf"}, WriteDatabases: []string{"dnf"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if group.CharacterSettlement == nil {
		t.Fatal("MySQL repository group does not expose the atomic quest/inventory settlement")
	}
}

func TestMySQLDungeonPermissionLoadsAccountOwnerKeyWithoutCharacterCoercion(t *testing.T) {
	accountKey, err := dungeonpermission.AccountOwnerKey("account-shared")
	if err != nil {
		t.Fatal(err)
	}
	sqlDB := &fakeSQLDB{
		rowsets: []fakeSQLRows{{
			rows: []fakeSQLRow{{
				values: []any{
					uint64(700), uint8(4), 0,
					time.Date(2026, 8, 21, 23, 0, 0, 0, time.UTC),
				},
			}},
		}},
	}
	repositories := newTestMySQLGroup(t, sqlDB)
	record, found, err := repositories.DungeonPermission.Load(context.Background(), accountKey)
	if err != nil || !found || record.CharacterID != accountKey || len(record.Entries) != 1 ||
		record.Entries[0].DungeonID != 700 || record.Entries[0].ClearState != 4 {
		t.Fatalf("account permission=%+v found=%t err=%v", record, found, err)
	}
	call := requireExecContaining(t, sqlDB, "dnf_dungeon_permissions")
	if len(call.Args) != 1 || call.Args[0] != accountKey {
		t.Fatalf("account owner query args=%#v want=%q", call.Args, accountKey)
	}
}

type fakeSQLDB struct {
	execs   []fakeSQLCall
	rows    []fakeSQLRow
	rowsets []fakeSQLRows
	pings   int
	execErr error
	pingErr error
}

func (f *fakeSQLDB) ExecContext(_ context.Context, query string, args ...any) (sql.Result, error) {
	f.execs = append(f.execs, fakeSQLCall{
		Query: query,
		Args:  append([]any(nil), args...),
	})
	if f.execErr != nil {
		return nil, f.execErr
	}
	return nil, nil
}

func (f *fakeSQLDB) QueryContext(_ context.Context, query string, args ...any) (SQLRows, error) {
	f.execs = append(f.execs, fakeSQLCall{
		Query: query,
		Args:  append([]any(nil), args...),
	})
	if len(f.rowsets) == 0 {
		return &fakeSQLRows{}, nil
	}
	rows := f.rowsets[0]
	f.rowsets = f.rowsets[1:]
	return &rows, nil
}

func (f *fakeSQLDB) QueryRowContext(_ context.Context, query string, args ...any) SQLRow {
	f.execs = append(f.execs, fakeSQLCall{
		Query: query,
		Args:  append([]any(nil), args...),
	})
	if len(f.rows) == 0 {
		return fakeSQLRow{err: sql.ErrNoRows}
	}
	row := f.rows[0]
	f.rows = f.rows[1:]
	return row
}

func (f *fakeSQLDB) PingContext(context.Context) error {
	f.pings++
	if f.pingErr != nil {
		return f.pingErr
	}
	return nil
}

type fakeSQLRow struct {
	values []any
	err    error
}

type fakeSQLRows struct {
	rows []fakeSQLRow
	idx  int
	err  error
}

func (r *fakeSQLRows) Close() error {
	return nil
}

func (r *fakeSQLRows) Next() bool {
	if r == nil || r.idx >= len(r.rows) {
		return false
	}
	r.idx++
	return true
}

func (r *fakeSQLRows) Scan(dest ...any) error {
	if r == nil || r.idx == 0 || r.idx > len(r.rows) {
		return errors.New("fake sql rows scan before next")
	}
	return r.rows[r.idx-1].Scan(dest...)
}

func (r *fakeSQLRows) Err() error {
	if r == nil {
		return nil
	}
	return r.err
}

func (r fakeSQLRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	if len(dest) != len(r.values) {
		return errors.New("fake sql row destination count mismatch")
	}
	for idx, value := range r.values {
		if err := assignFakeSQLValue(dest[idx], value); err != nil {
			return err
		}
	}
	return nil
}

func assignFakeSQLValue(dest any, value any) error {
	switch out := dest.(type) {
	case *string:
		if value == nil {
			*out = ""
			return nil
		}
		*out = value.(string)
	case *int:
		if value == nil {
			*out = 0
			return nil
		}
		*out = value.(int)
	case *int16:
		if value == nil {
			*out = 0
			return nil
		}
		switch v := value.(type) {
		case int16:
			*out = v
		case int:
			*out = int16(v)
		case int64:
			*out = int16(v)
		default:
			return errors.New("unsupported fake sql int16 value")
		}
	case *int64:
		if value == nil {
			*out = 0
			return nil
		}
		switch v := value.(type) {
		case int:
			*out = int64(v)
		case int64:
			*out = v
		default:
			return errors.New("unsupported fake sql int64 value")
		}
	case *bool:
		if value == nil {
			*out = false
			return nil
		}
		*out = value.(bool)
	case *byte:
		if value == nil {
			*out = 0
			return nil
		}
		switch v := value.(type) {
		case byte:
			*out = v
		case int:
			*out = byte(v)
		default:
			return errors.New("unsupported fake sql byte value")
		}
	case *uint16:
		if value == nil {
			*out = 0
			return nil
		}
		switch v := value.(type) {
		case uint16:
			*out = v
		case int:
			*out = uint16(v)
		default:
			return errors.New("unsupported fake sql uint16 value")
		}
	case *uint32:
		if value == nil {
			*out = 0
			return nil
		}
		switch v := value.(type) {
		case uint32:
			*out = v
		case int:
			*out = uint32(v)
		default:
			return errors.New("unsupported fake sql uint32 value")
		}
	case *uint64:
		if value == nil {
			*out = 0
			return nil
		}
		switch v := value.(type) {
		case uint64:
			*out = v
		case int64:
			*out = uint64(v)
		case int:
			*out = uint64(v)
		default:
			return errors.New("unsupported fake sql uint64 value")
		}
	case *[]byte:
		if value == nil {
			*out = nil
			return nil
		}
		*out = append([]byte(nil), value.([]byte)...)
	case *sql.NullString:
		if value == nil {
			*out = sql.NullString{}
			return nil
		}
		switch v := value.(type) {
		case string:
			*out = sql.NullString{String: v, Valid: true}
		case []byte:
			*out = sql.NullString{String: string(v), Valid: true}
		default:
			return errors.New("unsupported fake sql null string value")
		}
	case *sql.NullTime:
		if value == nil {
			*out = sql.NullTime{}
			return nil
		}
		*out = sql.NullTime{Time: value.(time.Time), Valid: true}
	case *sql.NullInt64:
		if value == nil {
			*out = sql.NullInt64{}
			return nil
		}
		switch v := value.(type) {
		case int:
			*out = sql.NullInt64{Int64: int64(v), Valid: true}
		case int64:
			*out = sql.NullInt64{Int64: v, Valid: true}
		default:
			return errors.New("unsupported fake sql null int64 value")
		}
	case *time.Time:
		if value == nil {
			*out = time.Time{}
			return nil
		}
		*out = value.(time.Time)
	default:
		return errors.New("unsupported fake sql destination")
	}
	return nil
}

func TestNewMySQLGroupRejectsMissingDB(t *testing.T) {
	_, err := NewMySQLGroup(nil, MySQLGroupOptions{
		DatabasePlan: repository.DatabasePlan{WriteDatabases: []string{"dnf_s1_w1"}},
	})
	if !errors.Is(err, ErrMySQLDBRequired) {
		t.Fatalf("NewMySQLGroup() error = %v, want ErrMySQLDBRequired", err)
	}
}

func TestMySQLGroupCheckPingsStores(t *testing.T) {
	sqlDB := &fakeSQLDB{}
	repos := newTestMySQLGroup(t, sqlDB)
	if err := repos.Check(context.Background()); err != nil {
		t.Fatalf("Check() error = %v", err)
	}
	if sqlDB.pings != 15 {
		t.Fatalf("pings = %d, want 15", sqlDB.pings)
	}
}

func TestMySQLCharacterCreateWritesSQLColumnsOnly(t *testing.T) {
	sqlDB := &fakeSQLDB{}
	repos := newTestMySQLGroup(t, sqlDB)
	record := repository.CharacterRecord{
		CharacterID: "char-1",
		AccountID:   "acc-1",
		Slot:        2,
		Name:        "fighter",
		Job:         "15",
		Level:       1,
	}
	if err := repository.CreateCharacter(context.Background(), repos.Character, record); err != nil {
		t.Fatalf("CreateCharacter() error = %v", err)
	}
	call := requireExecContaining(t, sqlDB, "`dnf_s1_w1`.`dnf_characters`")
	assertContains(t, call.Query, "`dnf_s1_w1`.`dnf_characters`")
	assertContains(t, call.Query, "`character_id`")
	assertContains(t, call.Query, "`grow_type`")
	assertContains(t, call.Query, "`user_state`")
	assertContains(t, call.Query, "`tutorial_completed`")
	assertContains(t, call.Query, "`tutorial_reward_progress_38`")
	assertContains(t, call.Query, "`delete_flag`")
	assertContains(t, call.Query, "`pc_room_id`")
	assertContains(t, call.Query, "`stat_block_marker`")
	assertContains(t, call.Query, "`roster_card_flag`")
	assertContains(t, call.Query, "`create_option_byte_63`")
	assertNotContains(t, call.Query, "`stats_json`")
	assertNotContains(t, call.Query, "`location_json`")
	assertNotContains(t, call.Query, "`roster_json`")
	statIndex := requireMySQLCharacterStatIndex(t, "tutorial_completed")
	if got := call.Args[8+statIndex]; got != int64(0) {
		t.Fatalf("created tutorial_completed arg = %#v, want 0", got)
	}
	rewardStatIndex := requireMySQLCharacterStatIndex(t, "tutorial_reward_progress_38")
	if got := call.Args[8+rewardStatIndex]; got != int64(0) {
		t.Fatalf("created tutorial_reward_progress_38 arg = %#v, want 0", got)
	}
}

func TestMySQLCharacterFindIDByNameIgnoresSoftDeletedRows(t *testing.T) {
	sqlDB := &fakeSQLDB{}
	repos := newTestMySQLGroup(t, sqlDB)

	_, ok, err := repos.Character.FindIDByName(context.Background(), "gone")
	if err != nil {
		t.Fatalf("FindIDByName() error = %v", err)
	}
	if ok {
		t.Fatalf("FindIDByName() found a row from empty fake DB")
	}
	call := requireExecContaining(t, sqlDB, "`dnf_s1_r1`.`dnf_characters`")
	assertContains(t, call.Query, "WHERE name = ? AND delete_flag = 0")
}

func TestMySQLCharacterSaveFieldsOnlyUpdatesDirtyColumns(t *testing.T) {
	sqlDB := &fakeSQLDB{}
	repos := newTestMySQLGroup(t, sqlDB)
	record := repository.CharacterRecord{
		CharacterID: "char-1",
		AccountID:   "acc-1",
		Name:        "fighter",
		Stats:       map[string]int64{"str": 99},
	}
	if err := repository.SaveCharacterFields(context.Background(), repos.Character, record, repository.CharacterFieldStats); err != nil {
		t.Fatalf("SaveCharacterFields() error = %v", err)
	}
	call := requireExecContaining(t, sqlDB, "`dnf_s1_w1`.`dnf_characters`")
	assertContains(t, call.Query, "`dnf_s1_w1`.`dnf_characters`")
	assertContains(t, call.Query, "`grow_type` = VALUES(`grow_type`)")
	assertContains(t, call.Query, "`roster_display_flags` = VALUES(`roster_display_flags`)")
	assertNotContains(t, call.Query, "`location_json` = VALUES(`location_json`)")
	assertNotContains(t, call.Query, "`roster_json` = VALUES(`roster_json`)")
	assertNotContains(t, call.Query, "`stats_json`")
	assertNotContains(t, call.Query, "`name` = VALUES(`name`)")
}

func TestMySQLCharacterTutorialFlagsSaveAndLoadThroughMirrorColumns(t *testing.T) {
	ctx := context.Background()
	statIndex := requireMySQLCharacterStatIndex(t, "tutorial_completed")
	rewardStatIndex := requireMySQLCharacterStatIndex(t, "tutorial_reward_progress_38")

	saveDB := &fakeSQLDB{}
	saveRepos := newTestMySQLGroup(t, saveDB)
	record := repository.CharacterRecord{
		CharacterID: "tutorial-char-1",
		AccountID:   "acc-1",
		Name:        "gunner",
		Job:         "2",
		Level:       1,
		Stats: map[string]int64{
			"tutorial_completed":          1,
			"tutorial_reward_progress_38": 1,
		},
	}
	if err := repository.SaveCharacterFields(ctx, saveRepos.Character, record, repository.CharacterFieldStats); err != nil {
		t.Fatalf("SaveCharacterFields() error = %v", err)
	}
	saveCall := requireExecContaining(t, saveDB, "`dnf_s1_w1`.`dnf_characters`")
	assertContains(t, saveCall.Query, "`tutorial_completed` = VALUES(`tutorial_completed`)")
	assertContains(t, saveCall.Query, "`tutorial_reward_progress_38` = VALUES(`tutorial_reward_progress_38`)")
	if got := saveCall.Args[8+statIndex]; got != int64(1) {
		t.Fatalf("saved tutorial_completed arg = %#v, want 1", got)
	}
	if got := saveCall.Args[8+rewardStatIndex]; got != int64(1) {
		t.Fatalf("saved tutorial_reward_progress_38 arg = %#v, want 1", got)
	}

	rowValues := []any{"tutorial-char-1", "acc-1", 0, "gunner", "2", 1}
	for idx := range mysqlCharacterStatColumns {
		value := int64(0)
		if idx == statIndex || idx == rewardStatIndex {
			value = 1
		}
		rowValues = append(rowValues, value)
	}
	rowValues = append(rowValues, nil, nil)
	loadDB := &fakeSQLDB{rows: []fakeSQLRow{{values: rowValues}}}
	loadRepos := newTestMySQLGroup(t, loadDB)
	loaded, ok, err := loadRepos.Character.Load(ctx, "tutorial-char-1")
	if err != nil || !ok {
		t.Fatalf("Load() ok=%v error=%v", ok, err)
	}
	if got := loaded.Stats["tutorial_completed"]; got != 1 {
		t.Fatalf("loaded tutorial_completed = %d, want 1", got)
	}
	if got := loaded.Stats["tutorial_reward_progress_38"]; got != 1 {
		t.Fatalf("loaded tutorial_reward_progress_38 = %d, want 1", got)
	}
	loadCall := requireExecContaining(t, loadDB, "`dnf_s1_r1`.`dnf_characters`")
	assertContains(t, loadCall.Query, "`tutorial_completed`")
	assertContains(t, loadCall.Query, "`tutorial_reward_progress_38`")
}

func TestMySQLCharacterCrystalSelectionSaveAndLoadThroughSignedMirrorColumn(t *testing.T) {
	ctx := context.Background()
	statIndex := requireMySQLCharacterStatIndex(t, "premium_crystal_selection")

	for _, test := range []struct {
		name      string
		selection int64
	}{
		{name: "none", selection: -1},
		{name: "black", selection: 0},
	} {
		selection := test.selection
		t.Run(test.name, func(t *testing.T) {
			saveDB := &fakeSQLDB{}
			saveRepos := newTestMySQLGroup(t, saveDB)
			record := repository.CharacterRecord{
				CharacterID: "crystal-char-1",
				AccountID:   "acc-1",
				Name:        "slayer",
				Job:         "0",
				Level:       1,
				Stats: map[string]int64{
					"premium_crystal_selection": selection,
				},
			}
			if err := repository.SaveCharacterFields(ctx, saveRepos.Character, record, repository.CharacterFieldStats); err != nil {
				t.Fatalf("SaveCharacterFields() error = %v", err)
			}
			saveCall := requireExecContaining(t, saveDB, "`dnf_s1_w1`.`dnf_characters`")
			assertContains(t, saveCall.Query, "`premium_crystal_selection` = VALUES(`premium_crystal_selection`)")
			if got := saveCall.Args[8+statIndex]; got != selection {
				t.Fatalf("saved premium_crystal_selection arg = %#v, want %d", got, selection)
			}

			rowValues := []any{"crystal-char-1", "acc-1", 0, "slayer", "0", 1}
			for idx, column := range mysqlCharacterStatColumns {
				value := column.fallback
				if idx == statIndex {
					value = selection
				}
				rowValues = append(rowValues, value)
			}
			rowValues = append(rowValues, nil, nil)
			loadDB := &fakeSQLDB{rows: []fakeSQLRow{{values: rowValues}}}
			loadRepos := newTestMySQLGroup(t, loadDB)
			loaded, ok, err := loadRepos.Character.Load(ctx, "crystal-char-1")
			if err != nil || !ok {
				t.Fatalf("Load() ok=%v error=%v", ok, err)
			}
			if got := loaded.Stats["premium_crystal_selection"]; got != selection {
				t.Fatalf("loaded premium_crystal_selection = %d, want %d", got, selection)
			}
		})
	}
}

func TestMySQLCharacterSaveFieldsUsesRelationalRosterTables(t *testing.T) {
	sqlDB := &fakeSQLDB{}
	repos := newTestMySQLGroup(t, sqlDB)
	record := repository.CharacterRecord{
		CharacterID: "char-1",
		AccountID:   "acc-1",
		Name:        "fighter",
		Roster: repository.CharacterRoster{
			Entry: repository.CharacterRosterEntry{
				PackedJobGrow: 0x24,
				ByteC:         56,
				ObjectID:      17,
				Flags:         []int64{1, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 3, 0, 0, 4},
			},
		},
	}
	if err := repository.SaveCharacterFields(context.Background(), repos.Character, record, repository.CharacterFieldRoster); err != nil {
		t.Fatalf("SaveCharacterFields() error = %v", err)
	}
	call := requireExecContaining(t, sqlDB, "`dnf_s1_w1`.`dnf_characters`")
	assertNotContains(t, call.Query, "`roster_json`")
	assertNotContains(t, call.Query, "`stats_json` = VALUES(`stats_json`)")
	assertNotContains(t, call.Query, "`location_json` = VALUES(`location_json`)")
	assertNotContains(t, call.Query, "`name` = VALUES(`name`)")
	requireExecContaining(t, sqlDB, "`dnf_s1_w1`.`dnf_character_rosters`")
	requireExecContaining(t, sqlDB, "`dnf_s1_w1`.`dnf_character_roster_lists`")
}

func TestMergeCharacterMirrorStatsUsesOnlyMySQLColumns(t *testing.T) {
	record := repository.CharacterRecord{
		Stats: map[string]int64{
			"grow_type":   7,
			"user_state":  9,
			"pvp_grade":   8,
			"custom_only": 123,
		},
	}
	values := make([]sql.NullInt64, len(mysqlCharacterStatColumns))
	for idx, column := range mysqlCharacterStatColumns {
		switch column.key {
		case "grow_type":
			values[idx] = sql.NullInt64{Int64: 0, Valid: true}
		case "user_state":
			values[idx] = sql.NullInt64{Int64: 0, Valid: true}
		case "pvp_grade":
			values[idx] = sql.NullInt64{Int64: 0, Valid: true}
		}
	}

	mergeCharacterMirrorStats(&record, values)

	if record.Stats["grow_type"] != 0 || record.Stats["user_state"] != 0 || record.Stats["pvp_grade"] != 0 {
		t.Fatalf("MySQL mirror columns did not override stale stats_json: %+v", record.Stats)
	}
	if _, ok := record.Stats["custom_only"]; ok {
		t.Fatalf("custom stats_json field leaked into MySQL-only stats: %+v", record.Stats)
	}
}

func TestMySQLInventorySaveFieldsOnlyWritesWarehouse(t *testing.T) {
	sqlDB := &fakeSQLDB{}
	repos := newTestMySQLGroup(t, sqlDB)
	record := repository.InventoryRecord{
		CharacterID: "char-1",
		Warehouse:   map[string]repository.ItemStack{"safe-1": {ItemID: 1001, Count: 2}},
	}
	if err := repository.SaveInventoryFields(context.Background(), repos.Inventory, record, repository.InventoryFieldWarehouse); err != nil {
		t.Fatalf("SaveInventoryFields() error = %v", err)
	}
	requireExecContaining(t, sqlDB, "`dnf_s1_w1`.`dnf_inventories`")
	insert := requireExecContaining(t, sqlDB, "`dnf_s1_w1`.`dnf_inventory_items`")
	assertContains(t, insert.Query, "collection_name")
	assertNotContains(t, strings.Join(execQueries(sqlDB), "\n"), "slots_json")
}

// 一个普通背包保存必须把全部槽位行合并成单条多行 VALUES INSERT；逐行
// ExecContext 是 op26 分解 ~350ms 延迟的主要放大器（93 行循环 vs 单语句）。
func TestMySQLInventorySaveSlotsUsesSingleMultiRowInsert(t *testing.T) {
	ctx := context.Background()
	sqlDB := &fakeSQLDB{}
	repos := newTestMySQLGroup(t, sqlDB)
	slots := make(map[string]repository.ItemStack, 93)
	for index := 0; index < 93; index++ {
		slots[fmt.Sprintf("slot-%02d", index)] = repository.ItemStack{
			ItemID:   int64(1000 + index),
			Count:    1,
			RawEntry: make([]byte, 0x77),
			Extra:    map[string]string{"source": "test"},
		}
	}
	record := repository.InventoryRecord{CharacterID: "char-bulk-slots", Slots: slots}
	if err := repository.SaveInventoryFields(ctx, repos.Inventory, record, repository.InventoryFieldSlots); err != nil {
		t.Fatalf("SaveInventoryFields() error = %v", err)
	}
	itemInserts := 0
	extraInserts := 0
	for _, call := range sqlDB.execs {
		switch {
		case strings.Contains(call.Query, "INSERT INTO `dnf_s1_w1`.`dnf_inventory_items`"):
			itemInserts++
			assertContains(t, call.Query, "), (")
			rows := len(call.Args) / 8
			if rows == 0 || len(call.Args)%8 != 0 || rows > mysqlReplaceBatchSize {
				t.Fatalf("item insert args=%d rows=%d query=%s", len(call.Args), rows, call.Query)
			}
		case strings.Contains(call.Query, "INSERT INTO `dnf_s1_w1`.`dnf_inventory_item_extra`"):
			extraInserts++
			assertContains(t, call.Query, "), (")
			rows := len(call.Args) / 5
			if rows == 0 || len(call.Args)%5 != 0 || rows > mysqlReplaceBatchSize {
				t.Fatalf("extra insert args=%d rows=%d query=%s", len(call.Args), rows, call.Query)
			}
		}
	}
	if itemInserts != 1 || extraInserts != 1 {
		t.Fatalf("slot writes must be one multi-row INSERT per table; items=%d extra=%d queries=%v",
			itemInserts, extraInserts, execQueries(sqlDB))
	}
}

// 超过批大小（128 行）时按块拆分，且保持 entry_key 排序顺序。
func TestMySQLInventorySaveSlotsChunksBeyondBatchSize(t *testing.T) {
	ctx := context.Background()
	sqlDB := &fakeSQLDB{}
	repos := newTestMySQLGroup(t, sqlDB)
	const total = mysqlReplaceBatchSize + 10
	slots := make(map[string]repository.ItemStack, total)
	for index := 0; index < total; index++ {
		slots[fmt.Sprintf("slot-%04d", index)] = repository.ItemStack{ItemID: int64(2000 + index), Count: 1}
	}
	record := repository.InventoryRecord{CharacterID: "char-chunked-slots", Slots: slots}
	if err := repository.SaveInventoryFields(ctx, repos.Inventory, record, repository.InventoryFieldSlots); err != nil {
		t.Fatalf("SaveInventoryFields() error = %v", err)
	}
	var chunkRows []int
	for _, call := range sqlDB.execs {
		if !strings.Contains(call.Query, "INSERT INTO `dnf_s1_w1`.`dnf_inventory_items`") {
			continue
		}
		chunkRows = append(chunkRows, len(call.Args)/8)
	}
	if len(chunkRows) != 2 || chunkRows[0] != mysqlReplaceBatchSize || chunkRows[1] != 10 {
		t.Fatalf("expected chunks [%d 10], got %v", mysqlReplaceBatchSize, chunkRows)
	}
}

func TestMySQLEquipmentSaveFieldsWritesEntries(t *testing.T) {
	sqlDB := &fakeSQLDB{}
	repos := newTestMySQLGroup(t, sqlDB)
	record := repository.EquipmentRecord{
		CharacterID: "char-1",
		Entries: map[string]repository.EquipmentEntry{
			"11": {SlotIndex: 11, ItemID: 1001, RawEntry: []byte{1, 2, 3}},
		},
	}
	if err := repository.SaveEquipmentFields(context.Background(), repos.Equipment, record, repository.EquipmentFieldEntries); err != nil {
		t.Fatalf("SaveEquipmentFields() error = %v", err)
	}
	requireExecContaining(t, sqlDB, "`dnf_s1_w1`.`dnf_equipments`")
	requireExecContaining(t, sqlDB, "`dnf_s1_w1`.`dnf_equipment_entries`")
	assertNotContains(t, strings.Join(execQueries(sqlDB), "\n"), "entries_json")
}

func TestMySQLSkillSaveFieldsWritesLevelsAndPointsTogether(t *testing.T) {
	sqlDB := &fakeSQLDB{}
	repos := newTestMySQLGroup(t, sqlDB)
	record := repository.SkillRecord{
		CharacterID: "char-1",
		Skills:      map[int64]repository.SkillState{46: {Level: 2, Enabled: true}},
		Points:      repository.SkillPointState{TotalSP: 100, RemainingSP: 60},
		Layouts:     map[int]repository.SkillLayout{0: {0: 46}},
	}
	if err := repository.SaveSkillFields(context.Background(), repos.Skill, record, repository.SkillFieldSkills, repository.SkillFieldPoints, repository.SkillFieldLayouts); err != nil {
		t.Fatalf("SaveSkillFields() error = %v", err)
	}
	root := requireExecContaining(t, sqlDB, "`dnf_s1_w1`.`dnf_skills`")
	assertContains(t, root.Query, "`total_sp` = VALUES(`total_sp`)")
	requireExecContaining(t, sqlDB, "`dnf_s1_w1`.`dnf_skill_states`")
	requireExecContaining(t, sqlDB, "`dnf_s1_w1`.`dnf_skill_layouts`")
	assertNotContains(t, strings.Join(execQueries(sqlDB), "\n"), "_json")
}

func TestMySQLSkillSaveFieldsWritesIndependentAlternateTree(t *testing.T) {
	sqlDB := &fakeSQLDB{}
	repos := newTestMySQLGroup(t, sqlDB)
	record := repository.SkillRecord{
		CharacterID: "char-1",
		Trees: map[int]repository.SkillTreeState{
			1: {
				Skills: map[int64]repository.SkillState{47: {Level: 3, Enabled: true}},
				Points: repository.SkillPointState{TotalSP: 200, RemainingSP: 80, TotalTP: 20, RemainingTP: 7},
			},
		},
	}
	if err := repository.SaveSkillFields(context.Background(), repos.Skill, record, repository.SkillFieldTrees); err != nil {
		t.Fatalf("SaveSkillFields() error = %v", err)
	}
	// 列序仍被断言：下面的 Args 断言按下标取值，列序是它们的前提。
	// 形状改为批量插入后与 TestMySQLInventorySaveSlotsUsesSingleMultiRowInsert
	// 固化的规范一致：标识符由 buildMySQLBulkInsert 加反引号。
	points := requireExecContaining(t, sqlDB, "`dnf_s1_w1`.`dnf_skill_tree_points` (`character_id`, `tree_id`")
	if len(points.Args) < 7 || points.Args[1] != 1 || points.Args[2] != 200 || points.Args[3] != 80 || points.Args[4] != 20 || points.Args[5] != 7 {
		t.Fatalf("alternate-tree point args = %#v", points.Args)
	}
	states := requireExecContaining(t, sqlDB, "`dnf_s1_w1`.`dnf_skill_tree_states` (`character_id`, `tree_id`")
	if len(states.Args) < 5 || states.Args[1] != 1 || states.Args[2] != int64(47) || states.Args[3] != 3 || states.Args[4] != true {
		t.Fatalf("alternate-tree skill args = %#v", states.Args)
	}
}

func TestMySQLSkillSaveFieldsWritesCommandBytes(t *testing.T) {
	sqlDB := &fakeSQLDB{}
	repos := newTestMySQLGroup(t, sqlDB)
	record := repository.SkillRecord{
		CharacterID: "char-1",
		Commands:    map[int64][]byte{46: {0, 4, 0xff}},
	}
	if err := repository.SaveSkillFields(context.Background(), repos.Skill, record, repository.SkillFieldCommands); err != nil {
		t.Fatalf("SaveSkillFields() error = %v", err)
	}
	queries := strings.Join(execQueries(sqlDB), "\n")
	if !strings.Contains(queries, "INSERT INTO `dnf_s1_w1`.`dnf_skill_commands` (`character_id`, `skill_id`, `command_bytes`)") {
		t.Fatalf("command write queries=%q", queries)
	}
}

func TestMySQLPetSaveFieldsWritesEntriesAndDisplay(t *testing.T) {
	sqlDB := &fakeSQLDB{}
	repos := newTestMySQLGroup(t, sqlDB)
	record := repository.PetRecord{
		CharacterID: "char-1",
		Entries: map[string]repository.PetEntry{
			"pet-1": {PetKey: "pet-1", ItemID: 1001},
		},
		EquippedKey: "pet-1",
		TownDisplay: true,
	}
	if err := repository.SavePetFields(context.Background(), repos.Pet, record, repository.PetFieldEntries, repository.PetFieldEquipped, repository.PetFieldDisplay); err != nil {
		t.Fatalf("SavePetFields() error = %v", err)
	}
	call := requireExecContaining(t, sqlDB, "`dnf_s1_w1`.`dnf_pets`")
	assertContains(t, call.Query, "`equipped_key` = VALUES(`equipped_key`)")
	assertContains(t, call.Query, "`town_display` = VALUES(`town_display`)")
	requireExecContaining(t, sqlDB, "`dnf_s1_w1`.`dnf_pet_entries`")
	assertNotContains(t, strings.Join(execQueries(sqlDB), "\n"), "entries_json")
}

func TestMySQLMailboxSaveFieldsWritesMails(t *testing.T) {
	sqlDB := &fakeSQLDB{}
	repos := newTestMySQLGroup(t, sqlDB)
	record := repository.MailboxRecord{
		CharacterID: "char-1",
		Mails: map[string]repository.MailRecord{
			"mail-1": {MailID: "mail-1", Title: "hello", Gold: 100},
		},
	}
	if err := repository.SaveMailboxFields(context.Background(), repos.Mailbox, record, repository.MailboxFieldMails); err != nil {
		t.Fatalf("SaveMailboxFields() error = %v", err)
	}
	requireExecContaining(t, sqlDB, "`dnf_s1_w1`.`dnf_mailboxes`")
	requireExecContaining(t, sqlDB, "`dnf_s1_w1`.`dnf_mails`")
	assertNotContains(t, strings.Join(execQueries(sqlDB), "\n"), "mails_json")
}

func TestMySQLMailboxLoadReadsMails(t *testing.T) {
	sqlDB := &fakeSQLDB{
		rows: []fakeSQLRow{{
			values: []any{
				"char-1",
				time.Date(2026, 7, 20, 2, 0, 0, 0, time.UTC),
			},
		}},
		rowsets: []fakeSQLRows{{rows: []fakeSQLRow{{values: []any{
			"mail-1", "", "", "", "", "hello", "", int64(100), false, false, false, nil, nil,
		}}}}},
	}
	repos := newTestMySQLGroup(t, sqlDB)
	record, ok, err := repos.Mailbox.Load(context.Background(), "char-1")
	if err != nil || !ok {
		t.Fatalf("Load() ok=%v error=%v", ok, err)
	}
	if record.Mails["mail-1"].Gold != 100 || record.Mails["mail-1"].Title != "hello" {
		t.Fatalf("mailbox = %+v", record)
	}
	call := requireExecContaining(t, sqlDB, "`dnf_s1_r1`.`dnf_mailboxes`")
	assertContains(t, call.Query, "`dnf_s1_r1`.`dnf_mailboxes`")
}

func TestMySQLAccountLoadUsesReadDatabase(t *testing.T) {
	sqlDB := &fakeSQLDB{
		rows: []fakeSQLRow{{
			values: []any{
				"acc-1",
				"open",
				uint64(123456789),
				"adventure-one",
				time.Date(2026, 6, 29, 1, 0, 0, 0, time.UTC),
				time.Date(2026, 6, 29, 2, 0, 0, 0, time.UTC),
			},
		}},
		rowsets: []fakeSQLRows{{rows: []fakeSQLRow{{values: []any{"source", "unit"}}}}},
	}
	repos := newTestMySQLGroup(t, sqlDB)
	record, ok, err := repos.Account.Load(context.Background(), "acc-1")
	if err != nil || !ok {
		t.Fatalf("Load() ok=%v err=%v", ok, err)
	}
	if record.Metadata["source"] != "unit" || record.State != "open" || record.HonorExp != 123456789 || record.RepresentAccountName != "adventure-one" {
		t.Fatalf("unexpected account: %+v", record)
	}
	call := requireExecContaining(t, sqlDB, "`dnf_s1_r1`.`dnf_accounts`")
	assertContains(t, call.Query, "`dnf_s1_r1`.`dnf_accounts`")
	assertNotContains(t, call.Query, "FOR UPDATE")
}

func TestReplaceQuestStatesUsesBoundedMultiRowInsertBatches(t *testing.T) {
	sqlDB := &fakeSQLDB{}
	states := make(map[int64]repository.QuestState, 129)
	for questID := int64(1); questID <= 129; questID++ {
		states[questID] = repository.QuestState{
			Status:        "active",
			TriggerType:   1,
			ProgressValue: questID,
			Extra:         map[string]string{"completion_key": "batch-test"},
		}
	}
	if err := replaceQuestStates(context.Background(), sqlDB, "quest_states", "quest_extra", "char-1", "states", states); err != nil {
		t.Fatal(err)
	}
	if len(sqlDB.execs) != 6 {
		t.Fatalf("exec count=%d want 6 (2 deletes + 2 state batches + 2 extra batches)", len(sqlDB.execs))
	}
	stateInserts, extraInserts := 0, 0
	for _, call := range sqlDB.execs {
		switch {
		case strings.Contains(call.Query, "INSERT INTO quest_states"):
			stateInserts++
			if len(call.Args) > 128*9 {
				t.Fatalf("state batch args=%d exceeds bounded batch", len(call.Args))
			}
		case strings.Contains(call.Query, "INSERT INTO quest_extra"):
			extraInserts++
			if len(call.Args) > 128*5 {
				t.Fatalf("extra batch args=%d exceeds bounded batch", len(call.Args))
			}
		}
	}
	if stateInserts != 2 || extraInserts != 2 {
		t.Fatalf("state inserts=%d extra inserts=%d", stateInserts, extraInserts)
	}
}

// replaceStringMap 的差异写：**值没变的行一个字节都不写**。
//
// 这条是数据库那批移植里风险最高的分支：默认的假 driver 读回来是"没有行"，等于只覆盖了
// "全是新增"；只有喂进一份与输入相同的现有行集，才能证明"相等 → 不删也不插"。
// （上游对这条的证明放在 DNF_MYSQL_LIVE_DSN 的真库往返用例里，见 live_bulk_roundtrip_test.go；
// 本树运行时 MySQL 当时没起，所以补一条不依赖真库的等价断言。）
func TestReplaceStringMapWritesOnlyChangedKeys(t *testing.T) {
	ctx := context.Background()
	database := &fakeSQLDB{rowsets: []fakeSQLRows{{rows: []fakeSQLRow{
		{values: []any{"keep", "same"}},
		{values: []any{"edit", "old"}},
		{values: []any{"drop", "gone"}},
	}}}}
	err := replaceStringMap(ctx, database, "account_metadata", "character_id", "77", map[string]string{
		"keep":  "same",  // 未变：一个语句里都不该出现
		"edit":  "new",   // 改值：删掉再插回
		"fresh": "added", // 新增：只插
		// "drop" 从输入里消失：只删
	})
	if err != nil {
		t.Fatalf("replaceStringMap() error = %v", err)
	}
	deletes, inserts := 0, 0
	insertedRows := make([]string, 0, 2)
	for _, call := range database.execs {
		switch {
		case strings.HasPrefix(call.Query, "DELETE FROM account_metadata"):
			deletes++
			assertContains(t, call.Query, "IN (")
			// 只应删「要重写的」与「已消失的」：edit / fresh / drop —— 不含 keep。
			if containsArg(call.Args, "keep") {
				t.Fatalf("未变化的键被删除: args=%v", call.Args)
			}
			for _, key := range []string{"edit", "fresh", "drop"} {
				if !containsArg(call.Args, key) {
					t.Fatalf("键 %q 应被删除: args=%v", key, call.Args)
				}
			}
		case strings.HasPrefix(call.Query, "INSERT INTO account_metadata"):
			inserts++
			assertContains(t, call.Query, "), (")
			if len(call.Args)%3 != 0 {
				t.Fatalf("insert args=%v", call.Args)
			}
			for index := 0; index < len(call.Args); index += 3 {
				insertedRows = append(insertedRows, call.Args[index+1].(string))
				if containsArg([]any{call.Args[index+1]}, "keep") {
					t.Fatalf("未变化的键被重写: args=%v", call.Args)
				}
			}
		}
	}
	if deletes != 1 || inserts != 1 {
		t.Fatalf("差异写应恰好一次 DELETE + 一次 INSERT，实际 deletes=%d inserts=%d queries=%v",
			deletes, inserts, execQueries(database))
	}
	if !slices.Equal(insertedRows, []string{"edit", "fresh"}) {
		t.Fatalf("只应重写变化/新增的键，实际插入 %v", insertedRows)
	}
}

// 宠物保存的差异写：**没变的主行、附加键、清除令牌一行都不写**。
//
// 这是 0897707 要证明的核心 —— 上游抓包显示"每次宠物保存都会重写全部宠物连同它们
// 119 个清除令牌，只为改一个饱食度"。这里喂进一只完全没变的宠物（带 3 个清除令牌）、
// 一只只改了饱食度的宠物、一只新增的、一只被删掉的，逐一断言各部分的写法。
func TestReplacePetEntriesWritesOnlyChangedPetsAndParts(t *testing.T) {
	ctx := context.Background()
	entryRow := func(key string, satiety byte, extra ...any) fakeSQLRow {
		values := []any{
			key, uint32(5001), int64(900001), byte(0), int16(9),
			"", []byte(nil), satiety, int64(0), byte(0), byte(0), byte(0),
			int64(1), int64(0), byte(0), []byte(nil),
		}
		return fakeSQLRow{values: append(values, extra...)}
	}
	database := &fakeSQLDB{rowsets: []fakeSQLRows{
		// 现有 pet_entries：unchanged（饱食度 50）与 shrunk（饱食度 50，输入改成 40）
		{rows: []fakeSQLRow{entryRow("unchanged", 50), entryRow("shrunk", 50), entryRow("dropped", 7)}},
		// 现有 pet_extra：unchanged 有一条
		{rows: []fakeSQLRow{{values: []any{"unchanged", "k", "v"}}}},
		// 现有 pet_clear_tokens：unchanged 三个令牌（顺序 t1,t2,t3）
		{rows: []fakeSQLRow{
			{values: []any{"unchanged", "t1", true}},
			{values: []any{"unchanged", "t2", false}},
			{values: []any{"unchanged", "t3", true}},
		}},
	}}
	unchanged := repository.PetEntry{
		PetKey: "unchanged", CreatureKey: 5001, ItemID: 900001, SourceSlotIndex: 9,
		Satiety: 50, Level: 1,
		Extra:                  map[string]string{"k": "v"},
		AppliedClearTokens:     map[string]bool{"t1": true, "t2": false, "t3": true},
		AppliedClearTokenOrder: []string{"t1", "t2", "t3"},
	}
	// shrunk 的持久状态只有主行（无附加键、无令牌），输入只改了饱食度 50→40。
	shrunk := repository.PetEntry{
		PetKey: "shrunk", CreatureKey: 5001, ItemID: 900001, SourceSlotIndex: 9,
		Satiety: 40, Level: 1,
	}
	added := unchanged
	added.PetKey = "added"

	err := replacePetEntries(ctx, database, "pet_entries", "pet_extra", "pet_clear_tokens", "77",
		map[string]repository.PetEntry{"unchanged": unchanged, "shrunk": shrunk, "added": added})
	if err != nil {
		t.Fatalf("replacePetEntries() error = %v", err)
	}

	for _, call := range database.execs {
		isWrite := strings.HasPrefix(call.Query, "DELETE FROM") || strings.HasPrefix(call.Query, "INSERT INTO")
		if !isWrite {
			continue // 三次 FOR UPDATE 读
		}
		if containsArg(call.Args, "unchanged") {
			t.Fatalf("完全没变的宠物被写了: query=%s args=%v", call.Query, call.Args)
		}
	}
	// shrunk 只改了饱食度 ⇒ 只有主行重写，令牌一行不动。
	// added 是新键 ⇒ 三部分都写。dropped 从输入里消失 ⇒ 三部分都删。
	var entryDeletes, tokenDeletes, entryInserts, tokenInserts int
	for _, call := range database.execs {
		switch {
		case strings.HasPrefix(call.Query, "DELETE FROM pet_entries"):
			entryDeletes++
			for _, key := range []string{"shrunk", "added", "dropped"} {
				if !containsArg(call.Args, key) {
					t.Fatalf("主行应删除 %q: args=%v", key, call.Args)
				}
			}
		case strings.HasPrefix(call.Query, "DELETE FROM pet_clear_tokens"):
			tokenDeletes++
			if containsArg(call.Args, "shrunk") {
				t.Fatalf("只改了饱食度的宠物不应重写清除令牌: args=%v", call.Args)
			}
			if !containsArg(call.Args, "dropped") || !containsArg(call.Args, "added") {
				t.Fatalf("被删与新增的宠物都该重写清除令牌: args=%v", call.Args)
			}
		case strings.HasPrefix(call.Query, "INSERT INTO pet_entries"):
			entryInserts++
			for index := 1; index < len(call.Args); index += 17 {
				key, _ := call.Args[index].(string)
				if key == "unchanged" || key == "dropped" {
					t.Fatalf("只应重插变化/新增的主行，出现 %q", key)
				}
			}
		case strings.HasPrefix(call.Query, "INSERT INTO pet_clear_tokens"):
			tokenInserts++
			if containsArg(call.Args, "unchanged") || containsArg(call.Args, "shrunk") {
				t.Fatalf("未变化的令牌不应重插: args=%v", call.Args)
			}
		}
	}
	if entryDeletes != 1 || entryInserts != 1 || tokenDeletes != 1 || tokenInserts != 1 {
		t.Fatalf("语句条数 entryDel=%d entryIns=%d tokenDel=%d tokenIns=%d queries=%v",
			entryDeletes, entryInserts, tokenDeletes, tokenInserts, execQueries(database))
	}
}

// 背包与装备的差异写：**没变的行一条语句都不发**（c0ebdce）。
//
// 上游的证明在 DNF_MYSQL_LIVE_DSN 真库用例里；这里用能返回行集的假 driver 做等价断言 ——
// 默认的假 driver 读回来是"没有行"，只覆盖"全是新增"，恰好漏掉最该防的那条分支。
func TestDiffCollectionWritersTouchOnlyChangedRows(t *testing.T) {
	ctx := context.Background()
	stack := func(itemID, count int64) repository.ItemStack {
		return repository.ItemStack{ItemID: itemID, Count: count}
	}
	database := &fakeSQLDB{rowsets: []fakeSQLRows{
		// 现有 inventory_items：keep（会被原样写回）与 edit（数量变了）、drop（输入里没了）
		{rows: []fakeSQLRow{
			{values: []any{"keep", int64(100), int64(1), false, nil, []byte(nil)}},
			{values: []any{"edit", int64(200), int64(1), false, nil, []byte(nil)}},
			{values: []any{"drop", int64(300), int64(1), false, nil, []byte(nil)}},
		}},
		// 现有 inventory_item_extra：空
		{rows: nil},
	}}
	err := replaceItemStackCollection(ctx, database, "inventory_items", "inventory_item_extra",
		"character_id", "77", "", false, map[string]repository.ItemStack{
			"keep":  stack(100, 1), // 未变
			"edit":  stack(200, 5), // 数量变了
			"fresh": stack(400, 1), // 新增
			// "drop" 消失
		})
	if err != nil {
		t.Fatalf("replaceItemStackCollection() error = %v", err)
	}
	for _, call := range database.execs {
		if !strings.HasPrefix(call.Query, "DELETE FROM") && !strings.HasPrefix(call.Query, "INSERT INTO") {
			continue
		}
		if containsArg(call.Args, "keep") {
			t.Fatalf("没变的背包行被写了: query=%s args=%v", call.Query, call.Args)
		}
	}

	// 装备同理。
	equipmentDB := &fakeSQLDB{rowsets: []fakeSQLRows{
		{rows: []fakeSQLRow{
			{values: []any{"7", int16(7), int64(900), false, nil, []byte(nil)}},
			{values: []any{"13", int16(13), int64(901), false, nil, []byte(nil)}},
		}},
		{rows: nil},
	}}
	unchangedEntry := repository.EquipmentEntry{SlotIndex: 7, ItemID: 900}
	changedEntry := repository.EquipmentEntry{SlotIndex: 13, ItemID: 999}
	if err := replaceEquipmentEntries(ctx, equipmentDB, "equipment_entries", "equipment_extra", "77",
		map[string]repository.EquipmentEntry{"7": unchangedEntry, "13": changedEntry}); err != nil {
		t.Fatalf("replaceEquipmentEntries() error = %v", err)
	}
	sawWrite := false
	for _, call := range equipmentDB.execs {
		if strings.HasPrefix(call.Query, "DELETE FROM") || strings.HasPrefix(call.Query, "INSERT INTO") {
			sawWrite = true
			if containsArg(call.Args, "7") {
				t.Fatalf("没变的装备条目被写了: query=%s args=%v", call.Query, call.Args)
			}
			if !containsArg(call.Args, "13") {
				t.Fatalf("变化的装备条目应被写: query=%s args=%v", call.Query, call.Args)
			}
		}
	}
	if !sawWrite {
		t.Fatal("变化的装备条目一条语句都没发")
	}
}

func containsArg(args []any, want string) bool {
	for _, arg := range args {
		if value, ok := arg.(string); ok && value == want {
			return true
		}
	}
	return false
}

func TestMySQLAccountLoadLocksCurrentRowInsideTransaction(t *testing.T) {
	sqlDB := &fakeSQLDB{rows: []fakeSQLRow{{values: []any{
		"acc-1",
		"open",
		uint64(123),
		nil,
		nil,
		nil,
	}}}}
	store := &mysqlAccountStore{mysqlStoreBase: mysqlStoreBase{router: mysqlRouter{
		db:          sqlDB,
		readDBs:     []string{"dnf_s1_w1"},
		writeDBs:    []string{"dnf_s1_w1"},
		tablePrefix: "dnf",
		lockReads:   true,
	}}}
	if _, ok, err := store.Load(context.Background(), "acc-1"); err != nil || !ok {
		t.Fatalf("Load() ok=%v err=%v", ok, err)
	}
	call := requireExecContaining(t, sqlDB, "`dnf_s1_w1`.`dnf_accounts`")
	assertContains(t, call.Query, "FOR UPDATE")
}

func TestMySQLAccountSaveWritesHonorExp(t *testing.T) {
	sqlDB := &fakeSQLDB{}
	repos := newTestMySQLGroup(t, sqlDB)
	if err := repos.Account.Save(context.Background(), repository.AccountRecord{
		AccountID:            "acc-1",
		State:                "open",
		HonorExp:             17699999999,
		RepresentAccountName: "adventure-one",
	}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	call := requireExecContaining(t, sqlDB, "`dnf_s1_w1`.`dnf_accounts`")
	assertContains(t, call.Query, "`honor_exp`")
	assertContains(t, call.Query, "`represent_account_name`")
	if len(call.Args) < 3 || call.Args[2] != uint64(17699999999) {
		t.Fatalf("honor_exp args = %#v", call.Args)
	}
	if len(call.Args) < 4 || call.Args[3] != "adventure-one" {
		t.Fatalf("represent_account_name args = %#v", call.Args)
	}
}

func TestMySQLAccountFindIDByRepresentNameUsesReadDatabase(t *testing.T) {
	sqlDB := &fakeSQLDB{rows: []fakeSQLRow{{values: []any{"acc-1"}}}}
	repos := newTestMySQLGroup(t, sqlDB)
	finder := repos.Account.(repository.RepresentAccountNameFinder)
	accountID, found, err := finder.FindAccountIDByRepresentName(context.Background(), "adventure-one")
	if err != nil || !found || accountID != "acc-1" {
		t.Fatalf("FindAccountIDByRepresentName() account=%q found=%v err=%v", accountID, found, err)
	}
	call := requireOneExec(t, sqlDB)
	assertContains(t, call.Query, "`dnf_s1_r1`.`dnf_accounts`")
	assertContains(t, call.Query, "represent_account_name = ?")
}

func TestMySQLAccountFindIDByLoginUsernameUsesLoginMapping(t *testing.T) {
	database := &fakeSQLDB{rows: []fakeSQLRow{{values: []any{"dnf:1"}}}}
	router, err := newMySQLRouter(database, MySQLGroupOptions{
		TablePrefix: "dnf",
		DatabasePlan: repository.DatabasePlan{
			ReadDatabases:  []string{"dnf_local"},
			WriteDatabases: []string{"dnf_local"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	accountID, found, err := (&mysqlAccountStore{mysqlStoreBase{router: router}}).FindAccountIDByLoginUsername(context.Background(), " ACCOUNT-ONE ")
	if err != nil || !found || accountID != "dnf:1" {
		t.Fatalf("FindAccountIDByLoginUsername() account=%q found=%t error=%v", accountID, found, err)
	}
	if len(database.execs) != 1 || !strings.Contains(database.execs[0].Query, "dnf_login_accounts") ||
		len(database.execs[0].Args) != 1 || database.execs[0].Args[0] != "account-one" {
		t.Fatalf("login lookup query = %+v", database.execs)
	}
}

func TestMySQLCharacterLoadNoRows(t *testing.T) {
	sqlDB := &fakeSQLDB{}
	repos := newTestMySQLGroup(t, sqlDB)
	_, ok, err := repos.Character.Load(context.Background(), "missing-char")
	if err != nil || ok {
		t.Fatalf("Load() ok=%v err=%v, want no row", ok, err)
	}
}

func TestMySQLPacketSaveClonesBodyArg(t *testing.T) {
	sqlDB := &fakeSQLDB{}
	repos := newTestMySQLGroup(t, sqlDB)
	body := []byte{1, 2, 3}
	if err := repos.PacketTemplate.Save(context.Background(), repository.PacketTemplateRecord{
		TemplateID: "tpl-1",
		Name:       "login",
		Body:       body,
	}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	body[0] = 9
	call := requireExecContaining(t, sqlDB, "`dnf_s1_w1`.`dnf_packet_templates`")
	got := call.Args[2].([]byte)
	if got[0] != 1 {
		t.Fatalf("body arg should be cloned, got %v", got)
	}
}

func newTestMySQLGroup(t *testing.T, sqlDB *fakeSQLDB) repository.Group {
	t.Helper()
	repos, err := NewMySQLGroup(sqlDB, MySQLGroupOptions{
		DatabasePlan: repository.DatabasePlan{
			WriteDatabases: []string{"dnf_s1_w1"},
			ReadDatabases:  []string{"dnf_s1_r1"},
		},
		Now: func() time.Time {
			return time.Date(2026, 6, 29, 3, 0, 0, 0, time.UTC)
		},
	})
	if err != nil {
		t.Fatalf("NewMySQLGroup() error = %v", err)
	}
	return repos
}

func requireOneExec(t *testing.T, sqlDB *fakeSQLDB) fakeSQLCall {
	t.Helper()
	if len(sqlDB.execs) != 1 {
		t.Fatalf("exec count = %d, want 1", len(sqlDB.execs))
	}
	return sqlDB.execs[0]
}

func requireExecContaining(t *testing.T, sqlDB *fakeSQLDB, needle string) fakeSQLCall {
	t.Helper()
	for _, call := range sqlDB.execs {
		if strings.Contains(call.Query, needle) {
			return call
		}
	}
	t.Fatalf("no SQL call contains %q; calls=%v", needle, execQueries(sqlDB))
	return fakeSQLCall{}
}

func execQueries(sqlDB *fakeSQLDB) []string {
	queries := make([]string, 0, len(sqlDB.execs))
	for _, call := range sqlDB.execs {
		queries = append(queries, call.Query)
	}
	return queries
}

func assertContains(t *testing.T, value, want string) {
	t.Helper()
	if !strings.Contains(value, want) {
		t.Fatalf("value missing %q:\n%s", want, value)
	}
}

func assertNotContains(t *testing.T, value, want string) {
	t.Helper()
	if strings.Contains(value, want) {
		t.Fatalf("value unexpectedly contains %q:\n%s", want, value)
	}
}

func requireMySQLCharacterStatIndex(t *testing.T, key string) int {
	t.Helper()
	for idx, column := range mysqlCharacterStatColumns {
		if column.key == key {
			return idx
		}
	}
	t.Fatalf("mysql character stat column %q not found", key)
	return -1
}
