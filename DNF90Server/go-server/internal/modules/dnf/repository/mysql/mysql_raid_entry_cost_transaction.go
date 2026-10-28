package mysql

import (
	"context"
	"errors"
	"sort"
	"strings"

	"longheng.io/server/internal/modules/dnf/repository"
)

type mysqlRaidEntryCostUnitOfWork struct {
	router mysqlRouter
}

func (u *mysqlRaidEntryCostUnitOfWork) WithinRaidEntryCost(
	ctx context.Context,
	characterIDs []string,
	apply func(repository.InventoryRepository) error,
) error {
	ids, err := normalizeMySQLRaidEntryCostCharacterIDs(characterIDs)
	if err != nil || apply == nil {
		return repository.ErrRaidEntryCostTransactionUnavailable
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	beginner, ok := u.router.db.(sqlTxBeginner)
	if !ok {
		return repository.ErrRaidEntryCostTransactionUnavailable
	}
	tx, err := beginner.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	txRouter := u.router
	txRouter.db = sqlTransactionDB{tx: tx}
	txRouter.readDBs = append([]string(nil), u.router.writeDBs...)
	txRouter.writeDBs = append([]string(nil), u.router.writeDBs...)
	txRouter.lockReads = true
	txGroup := newMySQLGroupFromRouter(txRouter, false)
	allowed := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		allowed[id] = struct{}{}
	}
	inventories := &mysqlRaidEntryCostInventory{allowed: allowed, base: txGroup.Inventory}
	if err := apply(inventories); err != nil {
		return errors.Join(err, tx.Rollback())
	}
	if err := ctx.Err(); err != nil {
		return errors.Join(err, tx.Rollback())
	}
	return tx.Commit()
}

type mysqlRaidEntryCostInventory struct {
	allowed map[string]struct{}
	base    repository.InventoryRepository
}

func (r *mysqlRaidEntryCostInventory) Load(ctx context.Context, characterID string) (repository.InventoryRecord, bool, error) {
	characterID = strings.TrimSpace(characterID)
	if _, ok := r.allowed[characterID]; !ok {
		return repository.InventoryRecord{}, false, repository.ErrRaidEntryCostTransactionUnavailable
	}
	return r.base.Load(ctx, characterID)
}

func (r *mysqlRaidEntryCostInventory) Save(ctx context.Context, record repository.InventoryRecord) error {
	characterID := strings.TrimSpace(record.CharacterID)
	if _, ok := r.allowed[characterID]; !ok {
		return repository.ErrRaidEntryCostTransactionUnavailable
	}
	return r.base.Save(ctx, record)
}

func (r *mysqlRaidEntryCostInventory) SaveFields(ctx context.Context, record repository.InventoryRecord, fields ...repository.InventoryField) error {
	characterID := strings.TrimSpace(record.CharacterID)
	if _, ok := r.allowed[characterID]; !ok {
		return repository.ErrRaidEntryCostTransactionUnavailable
	}
	return repository.SaveInventoryFields(ctx, r.base, record, fields...)
}

func normalizeMySQLRaidEntryCostCharacterIDs(source []string) ([]string, error) {
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
