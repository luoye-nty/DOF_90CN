package memory

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"

	"longheng.io/server/internal/modules/dnf/repository"
)

type memoryRaidEntryCostUnitOfWork struct {
	mu        sync.Mutex
	sharedMu  *sync.Mutex
	inventory repository.InventoryRepository
}

func (u *memoryRaidEntryCostUnitOfWork) WithinRaidEntryCost(
	ctx context.Context,
	characterIDs []string,
	apply func(repository.InventoryRepository) error,
) error {
	ids, err := normalizeRaidEntryCostCharacterIDs(characterIDs)
	if err != nil || u == nil || u.inventory == nil || apply == nil {
		return repository.ErrRaidEntryCostTransactionUnavailable
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	lock := &u.mu
	if u.sharedMu != nil {
		lock = u.sharedMu
	}
	lock.Lock()
	defer lock.Unlock()

	tx := &memoryRaidEntryCostInventory{
		allowed: make(map[string]struct{}, len(ids)),
		records: make(map[string]repository.InventoryRecord, len(ids)),
		exists:  make(map[string]bool, len(ids)),
		dirty:   make(map[string]bool, len(ids)),
	}
	preimages := make(map[string]repository.InventoryRecord, len(ids))
	for _, id := range ids {
		record, found, loadErr := u.inventory.Load(ctx, id)
		if loadErr != nil {
			return loadErr
		}
		tx.allowed[id] = struct{}{}
		tx.records[id] = repository.CloneInventory(record)
		tx.exists[id] = found
		preimages[id] = repository.CloneInventory(record)
	}
	if err := apply(tx); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	committed := make([]string, 0, len(ids))
	for _, id := range ids {
		if !tx.dirty[id] {
			continue
		}
		if err := u.inventory.Save(ctx, repository.CloneInventory(tx.records[id])); err != nil {
			rollback := []error{err}
			for index := len(committed) - 1; index >= 0; index-- {
				rollback = append(rollback, u.inventory.Save(context.Background(), preimages[committed[index]]))
			}
			return errors.Join(rollback...)
		}
		committed = append(committed, id)
	}
	return nil
}

type memoryRaidEntryCostInventory struct {
	allowed map[string]struct{}
	records map[string]repository.InventoryRecord
	exists  map[string]bool
	dirty   map[string]bool
}

func (tx *memoryRaidEntryCostInventory) Load(ctx context.Context, characterID string) (repository.InventoryRecord, bool, error) {
	characterID = strings.TrimSpace(characterID)
	if err := ctx.Err(); err != nil {
		return repository.InventoryRecord{}, false, err
	}
	if _, ok := tx.allowed[characterID]; !ok {
		return repository.InventoryRecord{}, false, repository.ErrRaidEntryCostTransactionUnavailable
	}
	return repository.CloneInventory(tx.records[characterID]), tx.exists[characterID], nil
}

func (tx *memoryRaidEntryCostInventory) Save(ctx context.Context, record repository.InventoryRecord) error {
	characterID := strings.TrimSpace(record.CharacterID)
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, ok := tx.allowed[characterID]; !ok {
		return repository.ErrRaidEntryCostTransactionUnavailable
	}
	tx.records[characterID] = repository.CloneInventory(record)
	tx.exists[characterID] = true
	tx.dirty[characterID] = true
	return nil
}

func normalizeRaidEntryCostCharacterIDs(source []string) ([]string, error) {
	seen := make(map[string]struct{}, len(source))
	ids := make([]string, 0, len(source))
	for _, raw := range source {
		id := strings.TrimSpace(raw)
		if id == "" {
			return nil, repository.ErrRaidEntryCostTransactionUnavailable
		}
		if _, duplicate := seen[id]; duplicate {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return nil, repository.ErrRaidEntryCostTransactionUnavailable
	}
	sort.Strings(ids)
	return ids, nil
}
