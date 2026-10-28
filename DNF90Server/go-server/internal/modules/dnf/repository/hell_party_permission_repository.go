package repository

import (
	"context"
	"strings"
	"time"
)

type HellPartyPermissionEntry struct {
	AreaID    int64     `json:"area_id"`
	QuestID   int64     `json:"quest_id"`
	UpdatedAt time.Time `json:"updated_at,omitempty"`
}

type HellPartyPermissionRecord struct {
	CharacterID string                     `json:"character_id"`
	Entries     []HellPartyPermissionEntry `json:"entries,omitempty"`
	UpdatedAt   time.Time                  `json:"updated_at,omitempty"`
}

type HellPartyPermissionRepository interface {
	Load(context.Context, string) (HellPartyPermissionRecord, bool, error)
	Upsert(context.Context, string, int64, int64) (HellPartyPermissionEntry, bool, error)
}

// HellPartyPermissionRestorer is implemented by stores that can remove a
// permission inserted by a failed in-memory aggregate commit. SQL callers use
// the surrounding transaction for rollback and do not need this hook.
type HellPartyPermissionRestorer interface {
	Delete(context.Context, string, int64) error
}

func CloneHellPartyPermission(record HellPartyPermissionRecord) HellPartyPermissionRecord {
	record.CharacterID = strings.TrimSpace(record.CharacterID)
	record.Entries = append([]HellPartyPermissionEntry(nil), record.Entries...)
	return record
}
