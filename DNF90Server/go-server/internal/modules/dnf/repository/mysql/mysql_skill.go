// 本文件实现 DNF 技能仓储的 MySQL 字段化读写。
package mysql

import (
	"context"
	"database/sql"
	"longheng.io/server/internal/modules/dnf/repository"
	"sort"
	"time"
)

const mysqlSkillTable = "skills"

type mysqlSkillStore struct {
	mysqlStoreBase
}

// Load 按角色 ID 从 MySQL 读取 DNF 技能记录。
func (s *mysqlSkillStore) Load(ctx context.Context, characterID string) (repository.SkillRecord, bool, error) {
	table, err := s.router.readTable(mysqlSkillTable, characterID)
	if err != nil {
		return repository.SkillRecord{}, false, err
	}
	statesTable, err := s.router.readTable(mysqlSkillStatesTable, characterID)
	if err != nil {
		return repository.SkillRecord{}, false, err
	}
	treePointsTable, err := s.router.readTable(mysqlSkillTreePointsTable, characterID)
	if err != nil {
		return repository.SkillRecord{}, false, err
	}
	treeStatesTable, err := s.router.readTable(mysqlSkillTreeStatesTable, characterID)
	if err != nil {
		return repository.SkillRecord{}, false, err
	}
	layoutsTable, err := s.router.readTable(mysqlSkillLayoutsTable, characterID)
	if err != nil {
		return repository.SkillRecord{}, false, err
	}
	commandsTable, err := s.router.readTable(mysqlSkillCommandsTable, characterID)
	if err != nil {
		return repository.SkillRecord{}, false, err
	}
	cooldownsTable, err := s.router.readTable(mysqlSkillCooldownsTable, characterID)
	if err != nil {
		return repository.SkillRecord{}, false, err
	}
	query := s.router.selectQuery("SELECT character_id, total_sp, remaining_sp, total_tp, remaining_tp, synced_level, updated_at FROM " + table + " WHERE character_id = ?")
	var record repository.SkillRecord
	var updatedAt sql.NullTime
	err = s.router.db.QueryRowContext(ctx, query, characterID).Scan(
		&record.CharacterID,
		&record.Points.TotalSP,
		&record.Points.RemainingSP,
		&record.Points.TotalTP,
		&record.Points.RemainingTP,
		&record.Points.SyncedLevel,
		&updatedAt,
	)
	if err != nil {
		ok, scanErr := scanErr(err)
		return repository.SkillRecord{}, ok, scanErr
	}
	record.Skills, err = loadSkillStates(ctx, s.router, statesTable, characterID)
	if err != nil {
		return repository.SkillRecord{}, false, err
	}
	record.Trees, err = loadSkillTrees(ctx, s.router, treePointsTable, treeStatesTable, characterID)
	if err != nil {
		return repository.SkillRecord{}, false, err
	}
	record.Layouts, err = loadSkillLayouts(ctx, s.router, layoutsTable, characterID)
	if err != nil {
		return repository.SkillRecord{}, false, err
	}
	record.Commands, err = loadSkillCommands(ctx, s.router, commandsTable, characterID)
	if err != nil {
		return repository.SkillRecord{}, false, err
	}
	record.Cooldowns, err = loadSkillCooldowns(ctx, s.router, cooldownsTable, characterID)
	if err != nil {
		return repository.SkillRecord{}, false, err
	}
	record.UpdatedAt = scanTime(updatedAt)
	return repository.CloneSkill(record), true, nil
}

// Save 保存完整 DNF 技能记录到 MySQL。
func (s *mysqlSkillStore) Save(ctx context.Context, record repository.SkillRecord) error {
	return s.SaveFields(ctx, record, repository.AllSkillFields()...)
}

// SaveFields 保存 DNF 技能指定字段到 MySQL。
// 技能等级和冷却分开写，避免技能释放路径覆盖配置同步后的等级状态。
func (s *mysqlSkillStore) SaveFields(ctx context.Context, record repository.SkillRecord, fields ...repository.SkillField) error {
	characterID, err := requireRecordKey(repository.SkillKey, record, "skill")
	if err != nil {
		return err
	}
	fields = repository.SkillFields.Normalize(fields)
	if len(fields) == 0 {
		return nil
	}
	table, err := s.router.writeTable(mysqlSkillTable, characterID)
	if err != nil {
		return err
	}
	statesTable, err := s.router.writeTable(mysqlSkillStatesTable, characterID)
	if err != nil {
		return err
	}
	treePointsTable, err := s.router.writeTable(mysqlSkillTreePointsTable, characterID)
	if err != nil {
		return err
	}
	treeStatesTable, err := s.router.writeTable(mysqlSkillTreeStatesTable, characterID)
	if err != nil {
		return err
	}
	layoutsTable, err := s.router.writeTable(mysqlSkillLayoutsTable, characterID)
	if err != nil {
		return err
	}
	commandsTable, err := s.router.writeTable(mysqlSkillCommandsTable, characterID)
	if err != nil {
		return err
	}
	cooldownsTable, err := s.router.writeTable(mysqlSkillCooldownsTable, characterID)
	if err != nil {
		return err
	}
	columns := []string{"character_id", "updated_at"}
	args := []any{characterID, timeOrNow(record.UpdatedAt, s.router.now)}
	updates := []string{updateValue("updated_at")}
	for _, field := range fields {
		if field == repository.SkillFieldPoints {
			columns = append(columns, "total_sp", "remaining_sp", "total_tp", "remaining_tp", "synced_level")
			args = append(
				args,
				record.Points.TotalSP,
				record.Points.RemainingSP,
				record.Points.TotalTP,
				record.Points.RemainingTP,
				record.Points.SyncedLevel,
			)
			updates = append(
				updates,
				updateValue("total_sp"),
				updateValue("remaining_sp"),
				updateValue("total_tp"),
				updateValue("remaining_tp"),
				updateValue("synced_level"),
			)
		}
	}
	return withMySQLWriteExecutor(ctx, s.router.db, func(database SQLDB) error {
		if _, execErr := database.ExecContext(ctx, buildUpsert(table, columns, updates), args...); execErr != nil {
			return execErr
		}
		for _, field := range fields {
			switch field {
			case repository.SkillFieldSkills:
				if err := replaceSkillStates(ctx, database, statesTable, characterID, record.Skills); err != nil {
					return err
				}
			case repository.SkillFieldLayouts:
				if err := replaceSkillLayouts(ctx, database, layoutsTable, characterID, record.Layouts); err != nil {
					return err
				}
			case repository.SkillFieldTrees:
				if err := replaceSkillTrees(ctx, database, treePointsTable, treeStatesTable, characterID, record.Trees); err != nil {
					return err
				}
			case repository.SkillFieldCommands:
				if err := replaceSkillCommands(ctx, database, commandsTable, characterID, record.Commands); err != nil {
					return err
				}
			case repository.SkillFieldCooldowns:
				if err := replaceSkillCooldowns(ctx, database, cooldownsTable, characterID, record.Cooldowns); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

func loadSkillTrees(ctx context.Context, router mysqlRouter, pointsTable, statesTable, characterID string) (map[int]repository.SkillTreeState, error) {
	rows, err := router.db.QueryContext(
		ctx,
		router.selectQuery("SELECT tree_id, total_sp, remaining_sp, total_tp, remaining_tp, synced_level FROM "+pointsTable+" WHERE character_id = ? ORDER BY tree_id"),
		characterID,
	)
	if err != nil {
		return nil, err
	}
	var trees map[int]repository.SkillTreeState
	for rows.Next() {
		var tree int
		var state repository.SkillTreeState
		if err := rows.Scan(&tree, &state.Points.TotalSP, &state.Points.RemainingSP, &state.Points.TotalTP, &state.Points.RemainingTP, &state.Points.SyncedLevel); err != nil {
			rows.Close()
			return nil, err
		}
		if tree <= 0 {
			continue
		}
		if trees == nil {
			trees = make(map[int]repository.SkillTreeState)
		}
		trees[tree] = state
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}

	rows, err = router.db.QueryContext(
		ctx,
		router.selectQuery("SELECT tree_id, skill_id, skill_level, enabled FROM "+statesTable+" WHERE character_id = ? ORDER BY tree_id, skill_id"),
		characterID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var tree int
		var skillID int64
		var skill repository.SkillState
		if err := rows.Scan(&tree, &skillID, &skill.Level, &skill.Enabled); err != nil {
			return nil, err
		}
		if tree <= 0 {
			continue
		}
		if trees == nil {
			trees = make(map[int]repository.SkillTreeState)
		}
		state := trees[tree]
		if state.Skills == nil {
			state.Skills = make(map[int64]repository.SkillState)
		}
		state.Skills[skillID] = skill
		trees[tree] = state
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return trees, nil
}

func loadSkillStates(ctx context.Context, router mysqlRouter, table, characterID string) (map[int64]repository.SkillState, error) {
	rows, err := router.db.QueryContext(
		ctx,
		router.selectQuery("SELECT skill_id, skill_level, enabled FROM "+table+" WHERE character_id = ? ORDER BY skill_id"),
		characterID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var values map[int64]repository.SkillState
	for rows.Next() {
		var skillID int64
		var state repository.SkillState
		if err := rows.Scan(&skillID, &state.Level, &state.Enabled); err != nil {
			return nil, err
		}
		if values == nil {
			values = make(map[int64]repository.SkillState)
		}
		values[skillID] = state
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return values, nil
}

func loadSkillLayouts(ctx context.Context, router mysqlRouter, table, characterID string) (map[int]repository.SkillLayout, error) {
	rows, err := router.db.QueryContext(
		ctx,
		router.selectQuery("SELECT tree_id, slot_index, skill_id FROM "+table+" WHERE character_id = ? ORDER BY tree_id, slot_index"),
		characterID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var layouts map[int]repository.SkillLayout
	for rows.Next() {
		var treeID, slot int
		var skillID uint16
		if err := rows.Scan(&treeID, &slot, &skillID); err != nil {
			return nil, err
		}
		if layouts == nil {
			layouts = make(map[int]repository.SkillLayout)
		}
		layout := layouts[treeID]
		if layout == nil {
			layout = make(repository.SkillLayout)
		}
		layout[slot] = skillID
		layouts[treeID] = layout
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return layouts, nil
}

func loadSkillCommands(ctx context.Context, router mysqlRouter, table, characterID string) (map[int64][]byte, error) {
	rows, err := router.db.QueryContext(
		ctx,
		router.selectQuery("SELECT skill_id, command_bytes FROM "+table+" WHERE character_id = ? ORDER BY skill_id"),
		characterID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var commands map[int64][]byte
	for rows.Next() {
		var skillID int64
		var command []byte
		if err := rows.Scan(&skillID, &command); err != nil {
			return nil, err
		}
		if len(command) == 0 {
			continue
		}
		if commands == nil {
			commands = make(map[int64][]byte)
		}
		commands[skillID] = append([]byte(nil), command...)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return commands, nil
}

func loadSkillCooldowns(ctx context.Context, router mysqlRouter, table, characterID string) (map[int64]time.Time, error) {
	rows, err := router.db.QueryContext(
		ctx,
		router.selectQuery("SELECT skill_id, expires_at FROM "+table+" WHERE character_id = ? ORDER BY skill_id"),
		characterID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var cooldowns map[int64]time.Time
	for rows.Next() {
		var skillID int64
		var expiresAt time.Time
		if err := rows.Scan(&skillID, &expiresAt); err != nil {
			return nil, err
		}
		if cooldowns == nil {
			cooldowns = make(map[int64]time.Time)
		}
		cooldowns[skillID] = expiresAt.UTC()
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return cooldowns, nil
}

func replaceSkillStates(ctx context.Context, database SQLDB, table, characterID string, values map[int64]repository.SkillState) error {
	if _, err := database.ExecContext(ctx, "DELETE FROM "+table+" WHERE character_id = ?", characterID); err != nil {
		return err
	}
	columns := []string{"character_id", "skill_id", "skill_level", "enabled"}
	ids := sortedInt64Keys(values)
	for start := 0; start < len(ids); start += mysqlReplaceBatchSize {
		end := min(start+mysqlReplaceBatchSize, len(ids))
		chunk := ids[start:end]
		args := make([]any, 0, len(chunk)*len(columns))
		for _, skillID := range chunk {
			state := values[skillID]
			args = append(args, characterID, skillID, state.Level, state.Enabled)
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

func replaceSkillTrees(ctx context.Context, database SQLDB, pointsTable, statesTable, characterID string, trees map[int]repository.SkillTreeState) error {
	if _, err := database.ExecContext(ctx, "DELETE FROM "+statesTable+" WHERE character_id = ?", characterID); err != nil {
		return err
	}
	if _, err := database.ExecContext(ctx, "DELETE FROM "+pointsTable+" WHERE character_id = ?", characterID); err != nil {
		return err
	}
	treeIDs := make([]int, 0, len(trees))
	for tree := range trees {
		if tree > 0 {
			treeIDs = append(treeIDs, tree)
		}
	}
	sort.Ints(treeIDs)
	pointColumns := []string{
		"character_id", "tree_id", "total_sp", "remaining_sp", "total_tp", "remaining_tp", "synced_level",
	}
	stateColumns := []string{"character_id", "tree_id", "skill_id", "skill_level", "enabled"}
	pointArgs := make([]any, 0, len(treeIDs)*len(pointColumns))
	for _, tree := range treeIDs {
		state := trees[tree]
		pointArgs = append(pointArgs, characterID, tree,
			state.Points.TotalSP, state.Points.RemainingSP,
			state.Points.TotalTP, state.Points.RemainingTP, state.Points.SyncedLevel)
	}
	for start := 0; start < len(treeIDs); start += mysqlReplaceBatchSize {
		end := min(start+mysqlReplaceBatchSize, len(treeIDs))
		if _, err := database.ExecContext(
			ctx,
			buildMySQLBulkInsert(pointsTable, pointColumns, end-start),
			pointArgs[start*len(pointColumns):end*len(pointColumns)]...,
		); err != nil {
			return err
		}
	}
	// 每个树的技能状态：行数最多的就是这里（角色技能可达上百条）。
	// 改前是「每棵树一条 points + 每个技能一条 states」。两张表不同，因此把
	// 「按树交错」改为「先全部 points、再全部 states」结果集完全一致。
	stateArgs := make([]any, 0, len(treeIDs)*8*len(stateColumns))
	stateRows := 0
	for _, tree := range treeIDs {
		state := trees[tree]
		for _, skillID := range sortedInt64Keys(state.Skills) {
			skill := state.Skills[skillID]
			stateArgs = append(stateArgs, characterID, tree, skillID, skill.Level, skill.Enabled)
			stateRows++
		}
	}
	for start := 0; start < stateRows; start += mysqlReplaceBatchSize {
		end := min(start+mysqlReplaceBatchSize, stateRows)
		if _, err := database.ExecContext(
			ctx,
			buildMySQLBulkInsert(statesTable, stateColumns, end-start),
			stateArgs[start*len(stateColumns):end*len(stateColumns)]...,
		); err != nil {
			return err
		}
	}
	return nil
}

func replaceSkillLayouts(ctx context.Context, database SQLDB, table, characterID string, layouts map[int]repository.SkillLayout) error {
	if _, err := database.ExecContext(ctx, "DELETE FROM "+table+" WHERE character_id = ?", characterID); err != nil {
		return err
	}
	columns := []string{"character_id", "tree_id", "slot_index", "skill_id"}
	trees := make([]int, 0, len(layouts))
	for treeID := range layouts {
		trees = append(trees, treeID)
	}
	sort.Ints(trees)
	args := make([]any, 0, len(trees)*8*len(columns))
	rows := 0
	for _, treeID := range trees {
		layout := layouts[treeID]
		slots := make([]int, 0, len(layout))
		for slot := range layout {
			slots = append(slots, slot)
		}
		sort.Ints(slots)
		for _, slot := range slots {
			args = append(args, characterID, treeID, slot, layout[slot])
			rows++
		}
	}
	for start := 0; start < rows; start += mysqlReplaceBatchSize {
		end := min(start+mysqlReplaceBatchSize, rows)
		if _, err := database.ExecContext(
			ctx,
			buildMySQLBulkInsert(table, columns, end-start),
			args[start*len(columns):end*len(columns)]...,
		); err != nil {
			return err
		}
	}
	return nil
}

func replaceSkillCommands(ctx context.Context, database SQLDB, table, characterID string, commands map[int64][]byte) error {
	if _, err := database.ExecContext(ctx, "DELETE FROM "+table+" WHERE character_id = ?", characterID); err != nil {
		return err
	}
	columns := []string{"character_id", "skill_id", "command_bytes"}
	args := make([]any, 0, len(commands)*len(columns))
	rows := 0
	for _, skillID := range sortedInt64Keys(commands) {
		command := commands[skillID]
		if len(command) == 0 {
			continue
		}
		// 保留原有的防御性复制：多行 INSERT 会把参数一直持有到 ExecContext 返回。
		args = append(args, characterID, skillID, append([]byte(nil), command...))
		rows++
	}
	for start := 0; start < rows; start += mysqlReplaceBatchSize {
		end := min(start+mysqlReplaceBatchSize, rows)
		if _, err := database.ExecContext(
			ctx,
			buildMySQLBulkInsert(table, columns, end-start),
			args[start*len(columns):end*len(columns)]...,
		); err != nil {
			return err
		}
	}
	return nil
}

func replaceSkillCooldowns(ctx context.Context, database SQLDB, table, characterID string, values map[int64]time.Time) error {
	if _, err := database.ExecContext(ctx, "DELETE FROM "+table+" WHERE character_id = ?", characterID); err != nil {
		return err
	}
	columns := []string{"character_id", "skill_id", "expires_at"}
	args := make([]any, 0, len(values)*len(columns))
	for _, skillID := range sortedInt64Keys(values) {
		args = append(args, characterID, skillID, values[skillID].UTC())
	}
	for start := 0; start < len(values); start += mysqlReplaceBatchSize {
		end := min(start+mysqlReplaceBatchSize, len(values))
		if _, err := database.ExecContext(
			ctx,
			buildMySQLBulkInsert(table, columns, end-start),
			args[start*len(columns):end*len(columns)]...,
		); err != nil {
			return err
		}
	}
	return nil
}

func sortedInt64Keys[V any](values map[int64]V) []int64 {
	keys := make([]int64, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(left, right int) bool { return keys[left] < keys[right] })
	return keys
}
