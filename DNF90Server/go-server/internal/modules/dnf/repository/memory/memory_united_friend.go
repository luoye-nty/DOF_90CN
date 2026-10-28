package memory

import (
	"context"
	"sort"

	"longheng.io/server/internal/modules/dnf/repository"
	"longheng.io/server/internal/platform/db"
)

type memoryUnitedFriendStore struct {
	*db.MemoryStore[repository.UnitedFriendRecord]
}

func newMemoryUnitedFriendStore() *memoryUnitedFriendStore {
	return &memoryUnitedFriendStore{MemoryStore: db.NewMemoryStore(repository.UnitedFriendKey, repository.CloneUnitedFriend)}
}

func (s *memoryUnitedFriendStore) Save(ctx context.Context, record repository.UnitedFriendRecord) error {
	return s.MemoryStore.Save(ctx, repository.NormalizeUnitedFriend(record))
}

func (s *memoryUnitedFriendStore) ListObservers(ctx context.Context, friendName string) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var out []string
	for _, record := range s.Snapshot(nil) {
		for _, name := range record.FriendNames {
			if name == friendName {
				out = append(out, record.OwnerName)
				break
			}
		}
	}
	sort.Strings(out)
	return out, nil
}
