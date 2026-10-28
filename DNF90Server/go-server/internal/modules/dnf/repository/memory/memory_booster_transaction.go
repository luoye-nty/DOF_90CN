package memory

import (
	"context"
	"errors"
	"strings"
	"sync"

	"longheng.io/server/internal/modules/dnf/repository"
)

// memoryBoosterAssetUnitOfWork mirrors the SQL booster transaction boundary:
// shared inventory, character, ordinary items, equipment and Epic Book are
// committed or restored as one aggregate.
type memoryBoosterAssetUnitOfWork struct {
	mu               sync.Mutex
	sharedMu         *sync.Mutex
	account          repository.AccountRepository
	accountInventory repository.AccountInventoryRepository
	character        repository.CharacterRepository
	inventory        repository.InventoryRepository
	equipment        repository.EquipmentRepository
	epicBook         repository.EpicBookRepository
}

func (u *memoryBoosterAssetUnitOfWork) WithinBoosterAssets(
	ctx context.Context,
	accountID string,
	characterID string,
	apply func(repository.AccountRepository, repository.AccountInventoryRepository, repository.CharacterRepository, repository.InventoryRepository, repository.EquipmentRepository, repository.EpicBookRepository) error,
) error {
	accountID = strings.TrimSpace(accountID)
	characterID = strings.TrimSpace(characterID)
	if u == nil || accountID == "" || characterID == "" || apply == nil ||
		u.account == nil || u.accountInventory == nil || u.character == nil || u.inventory == nil || u.equipment == nil || u.epicBook == nil {
		return repository.ErrBoosterAssetTransactionUnavailable
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

	accountState, accountStateExists, err := u.account.Load(ctx, accountID)
	if err != nil {
		return err
	}
	accountRecord, accountExists, err := u.accountInventory.Load(ctx, accountID)
	if err != nil {
		return err
	}
	characterRecord, characterExists, err := u.character.Load(ctx, characterID)
	if err != nil {
		return err
	}
	inventoryRecord, inventoryExists, err := u.inventory.Load(ctx, characterID)
	if err != nil {
		return err
	}
	equipmentRecord, equipmentExists, err := u.equipment.Load(ctx, characterID)
	if err != nil {
		return err
	}
	epicBookRecord, epicBookExists, err := u.epicBook.Load(ctx, accountID)
	if err != nil {
		return err
	}

	accountStateTx := &memoryAccountTransaction{accountID: accountID, record: repository.CloneAccount(accountState), exists: accountStateExists}
	accountTx := &memoryAccountInventoryTransaction{accountID: accountID, record: repository.CloneAccountInventory(accountRecord), exists: accountExists}
	characterTx := &memoryCharacterTransaction{characterID: characterID, record: repository.CloneCharacter(characterRecord), exists: characterExists}
	inventoryTx := &memoryInventoryTransaction{characterID: characterID, record: repository.CloneInventory(inventoryRecord), exists: inventoryExists}
	equipmentTx := &memoryEquipmentTransaction{characterID: characterID, record: repository.CloneEquipment(equipmentRecord), exists: equipmentExists}
	epicBookTx := &memoryEpicBookTransaction{accountID: accountID, record: repository.CloneEpicBook(epicBookRecord), exists: epicBookExists}
	if err := apply(accountStateTx, accountTx, characterTx, inventoryTx, equipmentTx, epicBookTx); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	if accountStateTx.dirty {
		if err := u.account.Save(ctx, accountStateTx.record); err != nil {
			return errors.Join(err, restoreAccountRecord(u.account, accountID, accountState, accountStateExists, true))
		}
	}
	if accountTx.dirty {
		if err := u.accountInventory.Save(ctx, accountTx.record); err != nil {
			return errors.Join(err,
				restoreAccountInventoryRecord(u.accountInventory, accountID, accountRecord, accountExists),
				restoreAccountRecord(u.account, accountID, accountState, accountStateExists, accountStateTx.dirty),
			)
		}
	}
	if characterTx.dirty {
		if err := u.character.Save(ctx, characterTx.record); err != nil {
			return errors.Join(err,
				restoreCharacterRecord(u.character, characterID, characterRecord, characterExists, true),
				restoreAccountInventoryRecordIfDirty(u.accountInventory, accountID, accountRecord, accountExists, accountTx.dirty),
				restoreAccountRecord(u.account, accountID, accountState, accountStateExists, accountStateTx.dirty),
			)
		}
	}
	if inventoryTx.dirty {
		if err := u.inventory.Save(ctx, inventoryTx.record); err != nil {
			return errors.Join(err,
				restoreInventoryRecordIfDirty(u.inventory, characterID, inventoryRecord, inventoryExists, true),
				restoreCharacterRecord(u.character, characterID, characterRecord, characterExists, characterTx.dirty),
				restoreAccountInventoryRecordIfDirty(u.accountInventory, accountID, accountRecord, accountExists, accountTx.dirty),
				restoreAccountRecord(u.account, accountID, accountState, accountStateExists, accountStateTx.dirty),
			)
		}
	}
	if equipmentTx.dirty {
		if err := u.equipment.Save(ctx, equipmentTx.record); err != nil {
			return errors.Join(err,
				restoreEquipmentRecordIfDirty(u.equipment, characterID, equipmentRecord, equipmentExists, true),
				restoreInventoryRecordIfDirty(u.inventory, characterID, inventoryRecord, inventoryExists, inventoryTx.dirty),
				restoreCharacterRecord(u.character, characterID, characterRecord, characterExists, characterTx.dirty),
				restoreAccountInventoryRecordIfDirty(u.accountInventory, accountID, accountRecord, accountExists, accountTx.dirty),
				restoreAccountRecord(u.account, accountID, accountState, accountStateExists, accountStateTx.dirty),
			)
		}
	}
	if epicBookTx.dirty {
		if err := u.epicBook.Save(ctx, epicBookTx.record); err != nil {
			return errors.Join(err,
				restoreEpicBookRecord(u.epicBook, accountID, epicBookRecord, epicBookExists),
				restoreEquipmentRecordIfDirty(u.equipment, characterID, equipmentRecord, equipmentExists, equipmentTx.dirty),
				restoreInventoryRecordIfDirty(u.inventory, characterID, inventoryRecord, inventoryExists, inventoryTx.dirty),
				restoreCharacterRecord(u.character, characterID, characterRecord, characterExists, characterTx.dirty),
				restoreAccountInventoryRecordIfDirty(u.accountInventory, accountID, accountRecord, accountExists, accountTx.dirty),
				restoreAccountRecord(u.account, accountID, accountState, accountStateExists, accountStateTx.dirty),
			)
		}
	}
	return nil
}
