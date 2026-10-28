package mysql

import (
	"context"
	"errors"
	"strings"

	"longheng.io/server/internal/modules/dnf/repository"
	"longheng.io/server/internal/platform/db"
)

type mysqlRaidRewardUnitOfWork struct {
	router mysqlRouter
}

type mysqlRaidRewardAccountRepository struct {
	allowed map[string]struct{}
	base    repository.AccountRepository
}

func (r *mysqlRaidRewardAccountRepository) Load(ctx context.Context, accountID string) (repository.AccountRecord, bool, error) {
	accountID = strings.TrimSpace(accountID)
	if _, ok := r.allowed[accountID]; !ok {
		return repository.AccountRecord{}, false, db.ErrRecordKeyRequired
	}
	return r.base.Load(ctx, accountID)
}

func (r *mysqlRaidRewardAccountRepository) Save(ctx context.Context, account repository.AccountRecord) error {
	accountID := strings.TrimSpace(account.AccountID)
	if _, ok := r.allowed[accountID]; !ok {
		return db.ErrRecordKeyRequired
	}
	return r.base.Save(ctx, account)
}

func (u *mysqlRaidRewardUnitOfWork) WithinRaidRewards(
	ctx context.Context,
	characterIDs []string,
	receiptScope string,
	apply func(repository.InventoryRepository, repository.SettingsRepository) error,
) error {
	ids, err := normalizeMySQLRaidEntryCostCharacterIDs(characterIDs)
	receiptScope = strings.TrimSpace(receiptScope)
	if err != nil || receiptScope == "" || apply == nil {
		return repository.ErrRaidRewardTransactionUnavailable
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	beginner, ok := u.router.db.(sqlTxBeginner)
	if !ok {
		return repository.ErrRaidRewardTransactionUnavailable
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
	receipts := &ceraShopScopedSettingsRepository{scope: receiptScope, base: txGroup.Settings}
	if err := apply(inventories, receipts); err != nil {
		return errors.Join(err, tx.Rollback())
	}
	if err := ctx.Err(); err != nil {
		return errors.Join(err, tx.Rollback())
	}
	return tx.Commit()
}

func (u *mysqlRaidRewardUnitOfWork) WithinRaidRewardAssets(
	ctx context.Context,
	characterIDs []string,
	accountIDs []string,
	receiptScope string,
	apply func(repository.AccountRepository, repository.InventoryRepository, repository.SettingsRepository) error,
) error {
	ids, err := normalizeMySQLRaidEntryCostCharacterIDs(characterIDs)
	receiptScope = strings.TrimSpace(receiptScope)
	if err != nil || receiptScope == "" || len(accountIDs) == 0 || apply == nil {
		return repository.ErrRaidRewardTransactionUnavailable
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	beginner, ok := u.router.db.(sqlTxBeginner)
	if !ok {
		return repository.ErrRaidRewardTransactionUnavailable
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
	if txGroup.Account == nil {
		return errors.Join(repository.ErrRaidRewardTransactionUnavailable, tx.Rollback())
	}
	allowedCharacters := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		allowedCharacters[id] = struct{}{}
	}
	allowedAccounts := make(map[string]struct{}, len(accountIDs))
	for _, id := range accountIDs {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		allowedAccounts[id] = struct{}{}
	}
	if len(allowedAccounts) == 0 {
		return errors.Join(repository.ErrRaidRewardTransactionUnavailable, tx.Rollback())
	}
	accounts := &mysqlRaidRewardAccountRepository{allowed: allowedAccounts, base: txGroup.Account}
	inventories := &mysqlRaidEntryCostInventory{allowed: allowedCharacters, base: txGroup.Inventory}
	receipts := &ceraShopScopedSettingsRepository{scope: receiptScope, base: txGroup.Settings}
	if err := apply(accounts, inventories, receipts); err != nil {
		return errors.Join(err, tx.Rollback())
	}
	if err := ctx.Err(); err != nil {
		return errors.Join(err, tx.Rollback())
	}
	return tx.Commit()
}
