package mysql

import (
	"context"
	"errors"
	"strings"

	"longheng.io/server/internal/modules/dnf/repository"
)

type mysqlAccountPairCharacterTradeUnitOfWork struct {
	router mysqlRouter
}

// WithinAccountPairCharacterTrade locks both accounts' warehouses together with
// both characters. A crystal sold across accounts debits one warehouse and
// credits the other, so neither half may commit on its own.
func (u *mysqlAccountPairCharacterTradeUnitOfWork) WithinAccountPairCharacterTrade(
	ctx context.Context,
	firstAccountID string,
	secondAccountID string,
	firstCharacterID string,
	secondCharacterID string,
	apply func(repository.AccountInventoryRepository, repository.CharacterRepository, repository.InventoryRepository) error,
) error {
	firstAccountID = strings.TrimSpace(firstAccountID)
	secondAccountID = strings.TrimSpace(secondAccountID)
	firstCharacterID = strings.TrimSpace(firstCharacterID)
	secondCharacterID = strings.TrimSpace(secondCharacterID)
	if firstAccountID == "" || secondAccountID == "" || firstCharacterID == "" || secondCharacterID == "" ||
		firstCharacterID == secondCharacterID || apply == nil || u == nil {
		return repository.ErrAccountCharacterTradeTransactionUnavailable
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	beginner, ok := u.router.db.(sqlTxBeginner)
	if !ok {
		return repository.ErrAccountCharacterTradeTransactionUnavailable
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
	allowedCharacters := map[string]struct{}{firstCharacterID: {}, secondCharacterID: {}}
	allowedAccounts := map[string]struct{}{firstAccountID: {}, secondAccountID: {}}
	accounts := &accountInventoryPairScopedRepository{allowed: allowedAccounts, base: txGroup.AccountInventory}
	characters := &characterTradeScopedCharacterRepository{allowed: allowedCharacters, base: txGroup.Character}
	inventories := &characterTradeScopedInventoryRepository{allowed: allowedCharacters, base: txGroup.Inventory}
	if err := apply(accounts, characters, inventories); err != nil {
		return errors.Join(err, tx.Rollback())
	}
	if err := ctx.Err(); err != nil {
		return errors.Join(err, tx.Rollback())
	}
	return tx.Commit()
}

type accountInventoryPairScopedRepository struct {
	allowed map[string]struct{}
	base    repository.AccountInventoryRepository
}

func (r *accountInventoryPairScopedRepository) Load(ctx context.Context, accountID string) (repository.AccountInventoryRecord, bool, error) {
	if _, ok := r.allowed[strings.TrimSpace(accountID)]; !ok {
		return repository.AccountInventoryRecord{}, false, repository.ErrAccountCharacterTradeTransactionUnavailable
	}
	return r.base.Load(ctx, accountID)
}

func (r *accountInventoryPairScopedRepository) Save(ctx context.Context, record repository.AccountInventoryRecord) error {
	if _, ok := r.allowed[strings.TrimSpace(record.AccountID)]; !ok {
		return repository.ErrAccountCharacterTradeTransactionUnavailable
	}
	return r.base.Save(ctx, record)
}
