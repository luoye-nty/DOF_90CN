package memory

import (
	"context"
	"errors"
	"strings"
	"sync"

	"longheng.io/server/internal/modules/dnf/repository"
)

type memoryEpicBookCraftUnitOfWork struct {
	mu               sync.Mutex
	sharedMu         *sync.Mutex
	epicBook         repository.EpicBookRepository
	accountInventory repository.AccountInventoryRepository
	inventory        repository.InventoryRepository
	equipment        repository.EquipmentRepository
}

func (u *memoryEpicBookCraftUnitOfWork) WithinEpicBookCraft(
	ctx context.Context,
	accountID string,
	characterID string,
	apply func(repository.EpicBookRepository, repository.AccountInventoryRepository, repository.InventoryRepository, repository.EquipmentRepository) error,
) error {
	accountID = strings.TrimSpace(accountID)
	characterID = strings.TrimSpace(characterID)
	if u == nil || accountID == "" || characterID == "" || apply == nil || u.epicBook == nil || u.accountInventory == nil || u.inventory == nil || u.equipment == nil {
		return repository.ErrEpicBookCraftTransactionUnavailable
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

	epicBook, epicBookExists, err := u.epicBook.Load(ctx, accountID)
	if err != nil {
		return err
	}
	accountInventory, accountInventoryExists, err := u.accountInventory.Load(ctx, accountID)
	if err != nil {
		return err
	}
	inventory, inventoryExists, err := u.inventory.Load(ctx, characterID)
	if err != nil {
		return err
	}
	equipment, equipmentExists, err := u.equipment.Load(ctx, characterID)
	if err != nil {
		return err
	}
	epicBookTx := &memoryEpicBookTransaction{accountID: accountID, record: repository.CloneEpicBook(epicBook), exists: epicBookExists}
	accountInventoryTx := &memoryAccountInventoryTransaction{accountID: accountID, record: repository.CloneAccountInventory(accountInventory), exists: accountInventoryExists}
	inventoryTx := &memoryInventoryTransaction{characterID: characterID, record: repository.CloneInventory(inventory), exists: inventoryExists}
	equipmentTx := &memoryEquipmentTransaction{characterID: characterID, record: repository.CloneEquipment(equipment), exists: equipmentExists}
	if err := apply(epicBookTx, accountInventoryTx, inventoryTx, equipmentTx); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	if epicBookTx.dirty {
		if err := u.epicBook.Save(ctx, epicBookTx.record); err != nil {
			return errors.Join(err, restoreEpicBookRecord(u.epicBook, accountID, epicBook, epicBookExists))
		}
	}
	if accountInventoryTx.dirty {
		if err := u.accountInventory.Save(ctx, accountInventoryTx.record); err != nil {
			return errors.Join(err,
				restoreAccountInventoryRecord(u.accountInventory, accountID, accountInventory, accountInventoryExists),
				restoreEpicBookRecordIfDirty(u.epicBook, accountID, epicBook, epicBookExists, epicBookTx.dirty),
			)
		}
	}
	if inventoryTx.dirty {
		if err := u.inventory.Save(ctx, inventoryTx.record); err != nil {
			return errors.Join(err,
				restoreInventoryRecordIfDirty(u.inventory, characterID, inventory, inventoryExists, true),
				restoreAccountInventoryRecordIfDirty(u.accountInventory, accountID, accountInventory, accountInventoryExists, accountInventoryTx.dirty),
				restoreEpicBookRecordIfDirty(u.epicBook, accountID, epicBook, epicBookExists, epicBookTx.dirty),
			)
		}
	}
	if equipmentTx.dirty {
		if err := u.equipment.Save(ctx, equipmentTx.record); err != nil {
			return errors.Join(err,
				restoreEquipmentRecordIfDirty(u.equipment, characterID, equipment, equipmentExists, true),
				restoreInventoryRecordIfDirty(u.inventory, characterID, inventory, inventoryExists, inventoryTx.dirty),
				restoreAccountInventoryRecordIfDirty(u.accountInventory, accountID, accountInventory, accountInventoryExists, accountInventoryTx.dirty),
				restoreEpicBookRecordIfDirty(u.epicBook, accountID, epicBook, epicBookExists, epicBookTx.dirty),
			)
		}
	}
	return nil
}

type memoryEpicBookTransaction struct {
	accountID string
	record    repository.EpicBookRecord
	exists    bool
	dirty     bool
}

func (tx *memoryEpicBookTransaction) Load(ctx context.Context, accountID string) (repository.EpicBookRecord, bool, error) {
	if err := repository.TransactionKeyError(ctx, tx.accountID, accountID); err != nil {
		return repository.EpicBookRecord{}, false, err
	}
	return repository.CloneEpicBook(tx.record), tx.exists, nil
}

func (tx *memoryEpicBookTransaction) Save(ctx context.Context, record repository.EpicBookRecord) error {
	if err := repository.TransactionKeyError(ctx, tx.accountID, record.AccountID); err != nil {
		return err
	}
	tx.record = repository.CloneEpicBook(record)
	tx.exists = true
	tx.dirty = true
	return nil
}

func restoreEpicBookRecord(repo repository.EpicBookRepository, accountID string, record repository.EpicBookRecord, existed bool) error {
	if repo == nil {
		return nil
	}
	if existed {
		return repo.Save(context.Background(), record)
	}
	deleter, ok := repo.(interface {
		Delete(context.Context, string) error
	})
	if !ok {
		return errors.New("epic book rollback cannot remove newly created record")
	}
	return deleter.Delete(context.Background(), accountID)
}

func restoreEpicBookRecordIfDirty(repo repository.EpicBookRepository, accountID string, record repository.EpicBookRecord, existed, dirty bool) error {
	if !dirty {
		return nil
	}
	return restoreEpicBookRecord(repo, accountID, record, existed)
}
