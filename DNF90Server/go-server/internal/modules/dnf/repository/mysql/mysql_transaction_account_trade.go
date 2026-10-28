package mysql

import (
	"context"
	"errors"
	"strings"

	"longheng.io/server/internal/modules/dnf/repository"
)

func (u *mysqlAccountCharacterTradeUnitOfWork) WithinAccountCharacterTrade(
	ctx context.Context,
	accountID string,
	firstCharacterID string,
	secondCharacterID string,
	apply func(repository.AccountInventoryRepository, repository.CharacterRepository, repository.InventoryRepository, repository.EpicBookRepository, repository.PetRepository) error,
) error {
	accountID = strings.TrimSpace(accountID)
	firstCharacterID = strings.TrimSpace(firstCharacterID)
	secondCharacterID = strings.TrimSpace(secondCharacterID)
	if accountID == "" || firstCharacterID == "" || secondCharacterID == "" || firstCharacterID == secondCharacterID || apply == nil {
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
	allowed := map[string]struct{}{firstCharacterID: {}, secondCharacterID: {}}
	accounts := &accountInventoryScopedRepository{accountID: accountID, base: txGroup.AccountInventory}
	characters := &characterTradeScopedCharacterRepository{allowed: allowed, base: txGroup.Character}
	inventories := &characterTradeScopedInventoryRepository{allowed: allowed, base: txGroup.Inventory}
	epicBooks := &epicBookScopedRepository{accountID: accountID, base: txGroup.EpicBook}
	pets := &characterTradeScopedPetRepository{allowed: allowed, base: txGroup.Pet}
	if err := apply(accounts, characters, inventories, epicBooks, pets); err != nil {
		return errors.Join(err, tx.Rollback())
	}
	if err := ctx.Err(); err != nil {
		return errors.Join(err, tx.Rollback())
	}
	return tx.Commit()
}

type characterTradeScopedPetRepository struct {
	allowed map[string]struct{}
	base    repository.PetRepository
}

func (r *characterTradeScopedPetRepository) Load(ctx context.Context, characterID string) (repository.PetRecord, bool, error) {
	if _, ok := r.allowed[strings.TrimSpace(characterID)]; !ok {
		return repository.PetRecord{}, false, repository.ErrAccountCharacterTradeTransactionUnavailable
	}
	return r.base.Load(ctx, characterID)
}

func (r *characterTradeScopedPetRepository) Save(ctx context.Context, record repository.PetRecord) error {
	if _, ok := r.allowed[strings.TrimSpace(record.CharacterID)]; !ok {
		return repository.ErrAccountCharacterTradeTransactionUnavailable
	}
	return r.base.Save(ctx, record)
}

func (r *characterTradeScopedPetRepository) SaveFields(ctx context.Context, record repository.PetRecord, fields ...repository.PetField) error {
	if _, ok := r.allowed[strings.TrimSpace(record.CharacterID)]; !ok {
		return repository.ErrAccountCharacterTradeTransactionUnavailable
	}
	return repository.SavePetFields(ctx, r.base, record, fields...)
}
