package memory

import (
	"context"
	"encoding/json"
	"longheng.io/server/internal/modules/dnf/repository"
	"sort"
	"strings"
	"sync"
	"time"

	"longheng.io/server/internal/platform/db"
)

func (s *memoryAccountStore) FindAccountIDByPVPTotalTeamName(ctx context.Context, name string) (string, bool, error) {
	if err := ctxErr(ctx); err != nil {
		return "", false, err
	}
	for _, record := range s.Snapshot(nil) {
		var team struct {
			Name string `json:"name"`
		}
		if json.Unmarshal([]byte(record.Metadata["pvp_total_match_team_v1"]), &team) == nil && strings.EqualFold(team.Name, name) {
			return record.AccountID, true, nil
		}
	}
	return "", false, nil
}

func (s *memoryAccountStore) ListAccounts(ctx context.Context, query string, limit int) ([]repository.AccountRecord, error) {
	if err := ctxErr(ctx); err != nil {
		return nil, err
	}
	query = strings.ToLower(strings.TrimSpace(query))
	if limit <= 0 {
		limit = 50
	}
	records := s.Snapshot(func(left, right repository.AccountRecord) bool {
		if !left.UpdatedAt.Equal(right.UpdatedAt) {
			return left.UpdatedAt.After(right.UpdatedAt)
		}
		return left.AccountID < right.AccountID
	})
	out := make([]repository.AccountRecord, 0, min(limit, len(records)))
	for _, record := range records {
		if query != "" && !strings.Contains(strings.ToLower(record.AccountID), query) &&
			!strings.Contains(strings.ToLower(record.RepresentAccountName), query) {
			continue
		}
		out = append(out, repository.CloneAccount(record))
		if len(out) >= limit {
			break
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].UpdatedAt.Equal(out[j].UpdatedAt) {
			return out[i].UpdatedAt.After(out[j].UpdatedAt)
		}
		return out[i].AccountID < out[j].AccountID
	})
	return out, nil
}

type memoryAccountStore struct {
	*db.MemoryStore[repository.AccountRecord]
	nameMu sync.Mutex
}

func newMemoryAccountStore() *memoryAccountStore {
	return &memoryAccountStore{
		MemoryStore: db.NewMemoryStore(repository.AccountKey, repository.CloneAccount),
	}
}

func (s *memoryAccountStore) Save(ctx context.Context, record repository.AccountRecord) error {
	if err := ctxErr(ctx); err != nil {
		return err
	}
	s.nameMu.Lock()
	defer s.nameMu.Unlock()
	name := strings.TrimSpace(record.RepresentAccountName)
	if name != "" {
		for _, existing := range s.Snapshot(nil) {
			if existing.AccountID != record.AccountID && strings.EqualFold(strings.TrimSpace(existing.RepresentAccountName), name) {
				return repository.ErrRepresentAccountNameExists
			}
		}
	}
	record.RepresentAccountName = name
	return s.MemoryStore.Save(ctx, record)
}

func (s *memoryAccountStore) SaveMetadataEntry(
	ctx context.Context,
	accountID string,
	key string,
	value string,
	updatedAt time.Time,
) error {
	if err := ctxErr(ctx); err != nil {
		return err
	}
	accountID = strings.TrimSpace(accountID)
	key = strings.TrimSpace(key)
	if accountID == "" {
		return db.ErrRecordKeyRequired
	}
	if key == "" {
		return repository.ErrAccountMetadataKeyRequired
	}
	s.nameMu.Lock()
	defer s.nameMu.Unlock()
	record, found, err := s.MemoryStore.Load(ctx, accountID)
	if err != nil {
		return err
	}
	if !found {
		return repository.ErrAccountMetadataUnavailable
	}
	record = repository.CloneAccount(record)
	if record.Metadata == nil {
		record.Metadata = make(map[string]string)
	}
	record.Metadata[key] = value
	record.UpdatedAt = updatedAt.UTC()
	return s.MemoryStore.Save(ctx, record)
}

func (s *memoryAccountStore) FindAccountIDByRepresentName(ctx context.Context, name string) (string, bool, error) {
	if err := ctxErr(ctx); err != nil {
		return "", false, err
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return "", false, nil
	}
	for _, record := range s.Snapshot(nil) {
		if strings.EqualFold(strings.TrimSpace(record.RepresentAccountName), name) {
			return record.AccountID, true, nil
		}
	}
	return "", false, nil
}

func (s *memoryAccountStore) FindAccountIDByLoginUsername(ctx context.Context, username string) (string, bool, error) {
	if err := ctxErr(ctx); err != nil {
		return "", false, err
	}
	username = strings.ToLower(strings.TrimSpace(username))
	if username == "" {
		return "", false, nil
	}
	for _, record := range s.Snapshot(nil) {
		if strings.EqualFold(strings.TrimSpace(record.Metadata["login_username"]), username) {
			return record.AccountID, true, nil
		}
	}
	return "", false, nil
}
