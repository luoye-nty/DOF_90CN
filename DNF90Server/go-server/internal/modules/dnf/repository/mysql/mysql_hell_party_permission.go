package mysql

import (
	"context"
	"database/sql"
	"longheng.io/server/internal/modules/dnf/repository"
	"strings"
	"time"
)

const mysqlHellPartyPermissionTable = "hell_party_permissions"

type mysqlHellPartyPermissionStore struct{ mysqlStoreBase }

func (s *mysqlHellPartyPermissionStore) Load(ctx context.Context, characterID string) (repository.HellPartyPermissionRecord, bool, error) {
	characterID = strings.TrimSpace(characterID)
	if characterID == "" {
		return repository.HellPartyPermissionRecord{}, false, nil
	}
	table, err := s.router.readTable(mysqlHellPartyPermissionTable, characterID)
	if err != nil {
		return repository.HellPartyPermissionRecord{}, false, err
	}
	rows, err := s.router.db.QueryContext(ctx, "SELECT area_id, quest_id, updated_at FROM "+table+" WHERE character_id = ? ORDER BY area_id", characterID)
	if err != nil {
		return repository.HellPartyPermissionRecord{}, false, err
	}
	defer rows.Close()
	record := repository.HellPartyPermissionRecord{CharacterID: characterID}
	for rows.Next() {
		var areaID, questID uint64
		var updated sql.NullTime
		if err := rows.Scan(&areaID, &questID, &updated); err != nil {
			return repository.HellPartyPermissionRecord{}, false, err
		}
		if areaID == 0 || questID == 0 {
			continue
		}
		entry := repository.HellPartyPermissionEntry{AreaID: int64(areaID), QuestID: int64(questID), UpdatedAt: scanTime(updated)}
		record.Entries = append(record.Entries, entry)
		if entry.UpdatedAt.After(record.UpdatedAt) {
			record.UpdatedAt = entry.UpdatedAt
		}
	}
	if err := rows.Err(); err != nil {
		return repository.HellPartyPermissionRecord{}, false, err
	}
	return repository.CloneHellPartyPermission(record), len(record.Entries) != 0, nil
}
func (s *mysqlHellPartyPermissionStore) Upsert(ctx context.Context, characterID string, areaID, questID int64) (repository.HellPartyPermissionEntry, bool, error) {
	characterID = strings.TrimSpace(characterID)
	if characterID == "" || areaID <= 0 || questID <= 0 {
		return repository.HellPartyPermissionEntry{}, false, nil
	}
	table, err := s.router.writeTable(mysqlHellPartyPermissionTable, characterID)
	if err != nil {
		return repository.HellPartyPermissionEntry{}, false, err
	}
	now := timeOrNow(time.Time{}, s.router.now)
	result, err := s.router.db.ExecContext(ctx, "INSERT IGNORE INTO "+table+" (character_id, area_id, quest_id, updated_at) VALUES (?, ?, ?, ?)", characterID, uint64(areaID), uint64(questID), sqlTime(now))
	if err != nil {
		return repository.HellPartyPermissionEntry{}, false, err
	}
	affected, _ := result.RowsAffected()
	return repository.HellPartyPermissionEntry{AreaID: areaID, QuestID: questID, UpdatedAt: now}, affected > 0, nil
}

func (s *mysqlHellPartyPermissionStore) Delete(ctx context.Context, characterID string, areaID int64) error {
	characterID = strings.TrimSpace(characterID)
	if characterID == "" || areaID <= 0 {
		return nil
	}
	table, err := s.router.writeTable(mysqlHellPartyPermissionTable, characterID)
	if err != nil {
		return err
	}
	_, err = s.router.db.ExecContext(ctx, "DELETE FROM "+table+" WHERE character_id = ? AND area_id = ?", characterID, uint64(areaID))
	return err
}
