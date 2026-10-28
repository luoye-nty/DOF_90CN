package mysql

import (
	"context"
	"errors"
	"strings"
	"time"

	"longheng.io/server/internal/modules/dnf/repository"
)

type mysqlEpicBookCraftUnitOfWork struct {
	router mysqlRouter
}

func (u *mysqlEpicBookCraftUnitOfWork) WithinEpicBookCraft(
	ctx context.Context,
	accountID string,
	characterID string,
	apply func(repository.EpicBookRepository, repository.AccountInventoryRepository, repository.InventoryRepository, repository.EquipmentRepository) error,
) error {
	accountID = strings.TrimSpace(accountID)
	characterID = strings.TrimSpace(characterID)
	if u == nil || accountID == "" || characterID == "" || apply == nil {
		return repository.ErrEpicBookCraftTransactionUnavailable
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	beginner, ok := u.router.db.(sqlTxBeginner)
	if !ok {
		return repository.ErrEpicBookCraftTransactionUnavailable
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
	epicBooks := &epicBookScopedRepository{accountID: accountID, base: txGroup.EpicBook}
	accountInventories := &accountInventoryScopedRepository{accountID: accountID, base: txGroup.AccountInventory}
	inventories := &rentalScopedInventoryRepository{characterID: characterID, base: txGroup.Inventory}
	equipment := &rentalScopedEquipmentRepository{characterID: characterID, base: txGroup.Equipment}
	if err := apply(epicBooks, accountInventories, inventories, equipment); err != nil {
		return errors.Join(err, tx.Rollback())
	}
	if err := ctx.Err(); err != nil {
		return errors.Join(err, tx.Rollback())
	}
	return tx.Commit()
}

type epicBookScopedRepository struct {
	accountID string
	base      repository.EpicBookRepository
}

func (r *epicBookScopedRepository) Load(ctx context.Context, accountID string) (repository.EpicBookRecord, bool, error) {
	if err := repository.TransactionKeyError(ctx, r.accountID, accountID); err != nil {
		return repository.EpicBookRecord{}, false, err
	}
	return r.base.Load(ctx, accountID)
}

func (r *epicBookScopedRepository) Save(ctx context.Context, record repository.EpicBookRecord) error {
	if err := repository.TransactionKeyError(ctx, r.accountID, record.AccountID); err != nil {
		return err
	}
	return r.base.Save(ctx, record)
}

// AddBalance 在事务内给单个碎片余额加量。先做与 Load/Save 一致的账号范围校验，
// 再交给 repository.AddEpicBookBalance 分流：底层是 MySQL 存储时走单行 upsert，
// 否则退回整本账重写。分流逻辑集中在一处，避免这里和调用方各判一次。
//
// 注意 r.base 是**未包装**的存储（newMySQLGroupFromRouter 直接放入
// *mysqlEpicBookStore）。转发必须走 r.base 而不是 r 自身，否则会自我递归。
func (r *epicBookScopedRepository) AddBalance(
	ctx context.Context,
	accountID string,
	equipmentID uint32,
	amount uint32,
	now time.Time,
) (uint32, error) {
	if err := repository.TransactionKeyError(ctx, r.accountID, accountID); err != nil {
		return 0, err
	}
	return repository.AddEpicBookBalance(ctx, r.base, accountID, equipmentID, amount, now)
}
