package mysql

import (
	"context"
	"database/sql"
	"fmt"
	"longheng.io/server/internal/modules/dnf/repository"
	"longheng.io/server/internal/platform/db"
	"sort"
	"strings"
	"time"
)

const mysqlQuestTable = "quests"

type mysqlQuestStore struct {
	mysqlStoreBase
}

func (s *mysqlQuestStore) Load(ctx context.Context, characterID string) (repository.QuestRecord, bool, error) {
	table, err := s.router.readTable(mysqlQuestTable, characterID)
	if err != nil {
		return repository.QuestRecord{}, false, err
	}
	statesTable, err := s.router.readTable(mysqlQuestStatesTable, characterID)
	if err != nil {
		return repository.QuestRecord{}, false, err
	}
	extraTable, err := s.router.readTable(mysqlQuestExtraTable, characterID)
	if err != nil {
		return repository.QuestRecord{}, false, err
	}
	query := s.router.selectQuery("SELECT character_id, updated_at FROM " + table + " WHERE character_id = ?")
	var record repository.QuestRecord
	var updatedAt sql.NullTime
	err = s.router.db.QueryRowContext(ctx, query, characterID).Scan(
		&record.CharacterID,
		&updatedAt,
	)
	if err != nil {
		ok, scanErr := scanErr(err)
		return repository.QuestRecord{}, ok, scanErr
	}
	record.States, err = loadQuestStates(ctx, s.router, statesTable, extraTable, characterID, "states")
	if err != nil {
		return repository.QuestRecord{}, false, err
	}
	record.Progress, err = loadQuestStates(ctx, s.router, statesTable, extraTable, characterID, "progress")
	if err != nil {
		return repository.QuestRecord{}, false, err
	}
	record.UpdatedAt = scanTime(updatedAt)
	return repository.CloneQuest(record), true, nil
}

func (s *mysqlQuestStore) Save(ctx context.Context, record repository.QuestRecord) error {
	return s.SaveFields(ctx, record, repository.AllQuestFields()...)
}

func (s *mysqlQuestStore) SaveFields(ctx context.Context, record repository.QuestRecord, fields ...repository.QuestField) error {
	characterID, err := requireRecordKey(repository.QuestKey, record, "quest")
	if err != nil {
		return err
	}
	fields = repository.QuestFields.Normalize(fields)
	if len(fields) == 0 {
		return nil
	}
	table, err := s.router.writeTable(mysqlQuestTable, characterID)
	if err != nil {
		return err
	}
	statesTable, err := s.router.writeTable(mysqlQuestStatesTable, characterID)
	if err != nil {
		return err
	}
	extraTable, err := s.router.writeTable(mysqlQuestExtraTable, characterID)
	if err != nil {
		return err
	}
	columns := []string{"character_id", "updated_at"}
	args := []any{characterID, timeOrNow(record.UpdatedAt, s.router.now)}
	updates := []string{updateValue("updated_at")}
	return withMySQLWriteExecutor(ctx, s.router.db, func(database SQLDB) error {
		if _, execErr := database.ExecContext(ctx, buildUpsert(table, columns, updates), args...); execErr != nil {
			return execErr
		}
		for _, field := range fields {
			switch field {
			case repository.QuestFieldStates:
				if err := replaceQuestStates(ctx, database, statesTable, extraTable, characterID, "states", record.States); err != nil {
					return err
				}
			case repository.QuestFieldProgress:
				if err := replaceQuestStates(ctx, database, statesTable, extraTable, characterID, "progress", record.Progress); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

// SaveQuestStateEntry updates one objective row and its extension fields.
// SET_TRIGGER is frequent; replacing every historical objective on each
// packet makes latency grow with the lifetime quest count.
func (s *mysqlQuestStore) SaveQuestStateEntry(
	ctx context.Context,
	characterID string,
	field repository.QuestField,
	questID int64,
	state repository.QuestState,
	updatedAt time.Time,
) error {
	characterID = strings.TrimSpace(characterID)
	if characterID == "" {
		return db.ErrRecordKeyRequired
	}
	var stateGroup string
	switch field {
	case repository.QuestFieldStates:
		stateGroup = "states"
	case repository.QuestFieldProgress:
		stateGroup = "progress"
	default:
		return fmt.Errorf("unsupported quest state field %q", field)
	}
	table, err := s.router.writeTable(mysqlQuestTable, characterID)
	if err != nil {
		return err
	}
	statesTable, err := s.router.writeTable(mysqlQuestStatesTable, characterID)
	if err != nil {
		return err
	}
	extraTable, err := s.router.writeTable(mysqlQuestExtraTable, characterID)
	if err != nil {
		return err
	}
	stateColumns := []string{
		"character_id", "state_group", "quest_id", "status", "trigger_type",
		"progress_value", "reward_select_index", "multiplier", "state_updated_at",
	}
	stateUpdates := make([]string, 0, len(stateColumns)-3)
	for _, column := range stateColumns[3:] {
		stateUpdates = append(stateUpdates, updateValue(column))
	}
	return withMySQLWriteExecutor(ctx, s.router.db, func(database SQLDB) error {
		if _, execErr := database.ExecContext(
			ctx,
			buildUpsert(table, []string{"character_id", "updated_at"}, []string{updateValue("updated_at")}),
			characterID,
			timeOrNow(updatedAt, s.router.now),
		); execErr != nil {
			return execErr
		}
		if _, execErr := database.ExecContext(
			ctx,
			buildUpsert(statesTable, stateColumns, stateUpdates),
			characterID, stateGroup, questID, state.Status, state.TriggerType,
			state.ProgressValue, state.RewardSelectIndex, state.Multiplier,
			sqlTime(state.UpdatedAt),
		); execErr != nil {
			return execErr
		}
		if _, execErr := database.ExecContext(
			ctx,
			"DELETE FROM "+extraTable+" WHERE character_id = ? AND state_group = ? AND quest_id = ?",
			characterID, stateGroup, questID,
		); execErr != nil {
			return execErr
		}
		extraColumns := []string{"character_id", "state_group", "quest_id", "extra_key", "extra_value"}
		extraKeys := sortedStringKeys(state.Extra)
		for start := 0; start < len(extraKeys); start += mysqlReplaceBatchSize {
			end := min(start+mysqlReplaceBatchSize, len(extraKeys))
			chunk := extraKeys[start:end]
			args := make([]any, 0, len(chunk)*len(extraColumns))
			for _, key := range chunk {
				args = append(args, characterID, stateGroup, questID, key, state.Extra[key])
			}
			if _, execErr := database.ExecContext(
				ctx,
				buildMySQLBulkInsert(extraTable, extraColumns, len(chunk)),
				args...,
			); execErr != nil {
				return execErr
			}
		}
		return nil
	})
}

func loadQuestStates(
	ctx context.Context,
	router mysqlRouter,
	statesTable, extraTable, characterID, stateGroup string,
) (map[int64]repository.QuestState, error) {
	query := router.selectQuery("SELECT quest_id, status, trigger_type, progress_value, reward_select_index, multiplier, state_updated_at FROM " + statesTable + " WHERE character_id = ? AND state_group = ? ORDER BY quest_id")
	rows, err := router.db.QueryContext(ctx, query, characterID, stateGroup)
	if err != nil {
		return nil, err
	}
	var states map[int64]repository.QuestState
	for rows.Next() {
		var questID int64
		var state repository.QuestState
		var updatedAt sql.NullTime
		if err := rows.Scan(
			&questID,
			&state.Status,
			&state.TriggerType,
			&state.ProgressValue,
			&state.RewardSelectIndex,
			&state.Multiplier,
			&updatedAt,
		); err != nil {
			rows.Close()
			return nil, err
		}
		state.UpdatedAt = scanTime(updatedAt)
		if states == nil {
			states = make(map[int64]repository.QuestState)
		}
		states[questID] = state
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	extras, err := router.db.QueryContext(
		ctx,
		router.selectQuery("SELECT quest_id, extra_key, extra_value FROM "+extraTable+" WHERE character_id = ? AND state_group = ? ORDER BY quest_id, extra_key"),
		characterID,
		stateGroup,
	)
	if err != nil {
		return nil, err
	}
	defer extras.Close()
	for extras.Next() {
		var questID int64
		var key, value string
		if err := extras.Scan(&questID, &key, &value); err != nil {
			return nil, err
		}
		state, ok := states[questID]
		if !ok {
			continue
		}
		if state.Extra == nil {
			state.Extra = make(map[string]string)
		}
		state.Extra[key] = value
		states[questID] = state
	}
	if err := extras.Err(); err != nil {
		return nil, err
	}
	return states, nil
}

func replaceQuestStates(
	ctx context.Context,
	database SQLDB,
	statesTable, extraTable, characterID, stateGroup string,
	states map[int64]repository.QuestState,
) error {
	if _, err := database.ExecContext(ctx, "DELETE FROM "+extraTable+" WHERE character_id = ? AND state_group = ?", characterID, stateGroup); err != nil {
		return err
	}
	if _, err := database.ExecContext(ctx, "DELETE FROM "+statesTable+" WHERE character_id = ? AND state_group = ?", characterID, stateGroup); err != nil {
		return err
	}
	stateQuery := "INSERT INTO " + statesTable + " (character_id, state_group, quest_id, status, trigger_type, progress_value, reward_select_index, multiplier, state_updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)"
	extraQuery := "INSERT INTO " + extraTable + " (character_id, state_group, quest_id, extra_key, extra_value) VALUES (?, ?, ?, ?, ?)"
	questIDs := make([]int64, 0, len(states))
	for questID := range states {
		questIDs = append(questIDs, questID)
	}
	sort.Slice(questIDs, func(left, right int) bool { return questIDs[left] < questIDs[right] })
	const batchSize = 128
	for start := 0; start < len(questIDs); start += batchSize {
		end := start + batchSize
		if end > len(questIDs) {
			end = len(questIDs)
		}
		stateArgs := make([]any, 0, (end-start)*9)
		stateValues := make([]string, 0, end-start)
		extraArgs := make([]any, 0, (end-start)*5)
		extraValues := make([]string, 0, end-start)
		for _, questID := range questIDs[start:end] {
			state := states[questID]
			stateValues = append(stateValues, "(?, ?, ?, ?, ?, ?, ?, ?, ?)")
			stateArgs = append(stateArgs, characterID, stateGroup, questID, state.Status,
				state.TriggerType, state.ProgressValue, state.RewardSelectIndex,
				state.Multiplier, sqlTime(state.UpdatedAt))
			for _, key := range sortedStringKeys(state.Extra) {
				extraValues = append(extraValues, "(?, ?, ?, ?, ?)")
				extraArgs = append(extraArgs, characterID, stateGroup, questID, key, state.Extra[key])
			}
		}
		if len(stateValues) > 0 {
			if _, err := database.ExecContext(ctx, stateQueryWithValues(stateQuery, stateValues), stateArgs...); err != nil {
				return err
			}
		}
		if len(extraValues) > 0 {
			if _, err := database.ExecContext(ctx, extraQueryWithValues(extraQuery, extraValues), extraArgs...); err != nil {
				return err
			}
		}
	}
	return nil
}

func stateQueryWithValues(query string, values []string) string {
	return strings.Replace(query, "VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)", "VALUES "+strings.Join(values, ", "), 1)
}

func extraQueryWithValues(query string, values []string) string {
	return strings.Replace(query, "VALUES (?, ?, ?, ?, ?)", "VALUES "+strings.Join(values, ", "), 1)
}
