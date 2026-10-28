package mysql

import (
	"context"
	"errors"
	"strings"

	"longheng.io/server/internal/modules/dnf/repository"
)

type mysqlBoosterAssetUnitOfWork struct {
	router mysqlRouter
}

func (u *mysqlBoosterAssetUnitOfWork) WithinBoosterAssets(
	ctx context.Context,
	accountID string,
	characterID string,
	apply func(repository.AccountRepository, repository.AccountInventoryRepository, repository.CharacterRepository, repository.InventoryRepository, repository.EquipmentRepository, repository.EpicBookRepository) error,
) error {
	accountID = strings.TrimSpace(accountID)
	characterID = strings.TrimSpace(characterID)
	if u == nil || accountID == "" || characterID == "" || apply == nil {
		return repository.ErrBoosterAssetTransactionUnavailable
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	beginner, ok := u.router.db.(sqlTxBeginner)
	if !ok {
		return repository.ErrBoosterAssetTransactionUnavailable
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
	accountRecords := &rentalScopedAccountRepository{accountID: accountID, base: txGroup.Account}
	accounts := &accountInventoryScopedRepository{accountID: accountID, base: txGroup.AccountInventory}
	characters := &rentalScopedCharacterRepository{characterID: characterID, base: txGroup.Character}
	inventories := &rentalScopedInventoryRepository{characterID: characterID, base: txGroup.Inventory}
	equipment := &rentalScopedEquipmentRepository{characterID: characterID, base: txGroup.Equipment}
	epicBooks := &epicBookScopedRepository{accountID: accountID, base: txGroup.EpicBook}
	if err := apply(accountRecords, accounts, characters, inventories, equipment, epicBooks); err != nil {
		return errors.Join(err, tx.Rollback())
	}
	if err := ctx.Err(); err != nil {
		return errors.Join(err, tx.Rollback())
	}
	return tx.Commit()
}
