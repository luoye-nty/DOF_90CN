package memory

import (
	"context"
	"longheng.io/server/internal/modules/dnf/repository"
	"sort"
	"strings"
	"sync"
	"time"
)

type memoryHellPartyPermissionStore struct {
	mu      sync.RWMutex
	records map[string]repository.HellPartyPermissionRecord
	now     func() time.Time
}

func (s *memoryHellPartyPermissionStore) snapshot() []repository.HellPartyPermissionRecord {
	s.mu.RLock()
	records := make([]repository.HellPartyPermissionRecord, 0, len(s.records))
	for _, record := range s.records {
		records = append(records, repository.CloneHellPartyPermission(record))
	}
	s.mu.RUnlock()
	sort.Slice(records, func(i, j int) bool { return records[i].CharacterID < records[j].CharacterID })
	return records
}

func (s *memoryHellPartyPermissionStore) replace(records []repository.HellPartyPermissionRecord) {
	s.mu.Lock()
	s.records = make(map[string]repository.HellPartyPermissionRecord, len(records))
	for _, record := range records {
		if key := strings.TrimSpace(record.CharacterID); key != "" {
			s.records[key] = repository.CloneHellPartyPermission(record)
		}
	}
	s.mu.Unlock()
}

func newMemoryHellPartyPermissionStore() *memoryHellPartyPermissionStore {
	return &memoryHellPartyPermissionStore{records: make(map[string]repository.HellPartyPermissionRecord)}
}
func (s *memoryHellPartyPermissionStore) Check(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	return ctx.Err()
}
func (s *memoryHellPartyPermissionStore) Load(ctx context.Context, characterID string) (repository.HellPartyPermissionRecord, bool, error) {
	if err := ctxErr(ctx); err != nil {
		return repository.HellPartyPermissionRecord{}, false, err
	}
	characterID = strings.TrimSpace(characterID)
	if characterID == "" {
		return repository.HellPartyPermissionRecord{}, false, nil
	}
	s.mu.RLock()
	record, ok := s.records[characterID]
	s.mu.RUnlock()
	if !ok {
		return repository.HellPartyPermissionRecord{}, false, nil
	}
	return repository.CloneHellPartyPermission(record), true, nil
}
func (s *memoryHellPartyPermissionStore) Upsert(ctx context.Context, characterID string, areaID, questID int64) (repository.HellPartyPermissionEntry, bool, error) {
	if err := ctxErr(ctx); err != nil {
		return repository.HellPartyPermissionEntry{}, false, err
	}
	characterID = strings.TrimSpace(characterID)
	if characterID == "" || areaID <= 0 || questID <= 0 {
		return repository.HellPartyPermissionEntry{}, false, nil
	}
	now := timeOrNow(time.Time{}, s.now)
	s.mu.Lock()
	defer s.mu.Unlock()
	record := repository.CloneHellPartyPermission(s.records[characterID])
	record.CharacterID = characterID
	for i := range record.Entries {
		if record.Entries[i].AreaID == areaID {
			return record.Entries[i], false, nil
		}
	}
	entry := repository.HellPartyPermissionEntry{AreaID: areaID, QuestID: questID, UpdatedAt: now}
	record.Entries = append(record.Entries, entry)
	record.UpdatedAt = now
	s.records[characterID] = repository.CloneHellPartyPermission(record)
	return entry, true, nil
}

func (s *memoryHellPartyPermissionStore) Delete(ctx context.Context, characterID string, areaID int64) error {
	if err := ctxErr(ctx); err != nil {
		return err
	}
	characterID = strings.TrimSpace(characterID)
	if characterID == "" || areaID <= 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	record, ok := s.records[characterID]
	if !ok {
		return nil
	}
	filtered := record.Entries[:0]
	for _, entry := range record.Entries {
		if entry.AreaID != areaID {
			filtered = append(filtered, entry)
		}
	}
	if len(filtered) == len(record.Entries) {
		return nil
	}
	record.Entries = append([]repository.HellPartyPermissionEntry(nil), filtered...)
	if len(record.Entries) == 0 {
		delete(s.records, characterID)
	} else {
		s.records[characterID] = repository.CloneHellPartyPermission(record)
	}
	return nil
}
