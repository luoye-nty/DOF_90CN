package memory

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"

	"longheng.io/server/internal/modules/dnf/repository"
)

type memoryAccountCharacterTradeUnitOfWork struct {
	mu               sync.Mutex
	sharedMu         *sync.Mutex
	accountInventory repository.AccountInventoryRepository
	character        repository.CharacterRepository
	inventory        repository.InventoryRepository
	epicBook         repository.EpicBookRepository
	pets             repository.PetRepository
}

func (u *memoryAccountCharacterTradeUnitOfWork) WithinAccountCharacterTrade(
	ctx context.Context,
	accountID string,
	firstCharacterID string,
	secondCharacterID string,
	apply func(repository.AccountInventoryRepository, repository.CharacterRepository, repository.InventoryRepository, repository.EpicBookRepository, repository.PetRepository) error,
) error {
	accountID = strings.TrimSpace(accountID)
	firstCharacterID = strings.TrimSpace(firstCharacterID)
	secondCharacterID = strings.TrimSpace(secondCharacterID)
	if accountID == "" || firstCharacterID == "" || secondCharacterID == "" || firstCharacterID == secondCharacterID || apply == nil ||
		u == nil || u.accountInventory == nil || u.character == nil || u.inventory == nil || u.epicBook == nil || u.pets == nil {
		return repository.ErrAccountCharacterTradeTransactionUnavailable
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	ids := []string{firstCharacterID, secondCharacterID}
	sort.Strings(ids)
	lock := &u.mu
	if u.sharedMu != nil {
		lock = u.sharedMu
	}
	lock.Lock()
	defer lock.Unlock()

	account, accountExists, err := u.accountInventory.Load(ctx, accountID)
	if err != nil {
		return err
	}
	account = repository.CloneAccountInventory(account)
	account.AccountID = accountID
	if account.Slots == nil {
		account.Slots = make(map[string]repository.ItemStack)
	}
	epicBook, epicBookExists, err := u.epicBook.Load(ctx, accountID)
	if err != nil {
		return err
	}
	characterRecords := make(map[string]repository.CharacterRecord, 2)
	characterExists := make(map[string]bool, 2)
	inventoryRecords := make(map[string]repository.InventoryRecord, 2)
	inventoryExists := make(map[string]bool, 2)
	petRecords := make(map[string]repository.PetRecord, 2)
	petExists := make(map[string]bool, 2)
	for _, characterID := range ids {
		character, found, loadErr := u.character.Load(ctx, characterID)
		if loadErr != nil {
			return loadErr
		}
		characterRecords[characterID] = repository.CloneCharacter(character)
		characterExists[characterID] = found
		inventory, found, loadErr := u.inventory.Load(ctx, characterID)
		if loadErr != nil {
			return loadErr
		}
		inventoryRecords[characterID] = repository.CloneInventory(inventory)
		inventoryExists[characterID] = found
		pet, found, loadErr := u.pets.Load(ctx, characterID)
		if loadErr != nil {
			return loadErr
		}
		petRecords[characterID] = repository.ClonePet(pet)
		petExists[characterID] = found
	}
	allowed := map[string]struct{}{ids[0]: {}, ids[1]: {}}
	accountTx := &memoryAccountInventoryTransaction{accountID: accountID, record: account, exists: accountExists}
	characterTx := &memoryCharacterTradeCharacterTransaction{allowed: allowed, records: characterRecords, exists: characterExists, dirty: make(map[string]bool, 2)}
	inventoryTx := &memoryCharacterTradeInventoryTransaction{allowed: allowed, records: inventoryRecords, exists: inventoryExists, dirty: make(map[string]bool, 2)}
	epicBookTx := &memoryEpicBookTransaction{accountID: accountID, record: repository.CloneEpicBook(epicBook), exists: epicBookExists}
	petTx := &memoryCharacterTradePetTransaction{allowed: allowed, records: petRecords, exists: petExists, dirty: make(map[string]bool, 2)}
	if err := apply(accountTx, characterTx, inventoryTx, epicBookTx, petTx); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	type committedRecord struct {
		kind string
		id   string
	}
	committed := make([]committedRecord, 0, 5)
	rollback := func(cause error) error {
		errs := []error{cause}
		for index := len(committed) - 1; index >= 0; index-- {
			row := committed[index]
			switch row.kind {
			case "account":
				errs = append(errs, restoreAccountInventoryRecord(u.accountInventory, accountID, account, accountExists))
			case "character":
				errs = append(errs, restoreCharacterRecord(u.character, row.id, characterRecords[row.id], characterExists[row.id], true))
			case "inventory":
				errs = append(errs, restoreInventoryRecord(u.inventory, row.id, inventoryRecords[row.id], inventoryExists[row.id]))
			case "epic_book":
				errs = append(errs, restoreEpicBookRecord(u.epicBook, accountID, epicBook, epicBookExists))
			case "pet":
				errs = append(errs, restorePetRecordIfDirty(u.pets, row.id, petRecords[row.id], petExists[row.id], true))
			}
		}
		return errors.Join(errs...)
	}
	if accountTx.dirty {
		if err := u.accountInventory.Save(ctx, accountTx.record); err != nil {
			return err
		}
		committed = append(committed, committedRecord{kind: "account", id: accountID})
	}
	if epicBookTx.dirty {
		if err := u.epicBook.Save(ctx, epicBookTx.record); err != nil {
			return rollback(err)
		}
		committed = append(committed, committedRecord{kind: "epic_book", id: accountID})
	}
	for _, characterID := range ids {
		if characterTx.dirty[characterID] {
			if err := u.character.Save(ctx, characterTx.records[characterID]); err != nil {
				return rollback(err)
			}
			committed = append(committed, committedRecord{kind: "character", id: characterID})
		}
	}
	for _, characterID := range ids {
		if inventoryTx.dirty[characterID] {
			if err := u.inventory.Save(ctx, inventoryTx.records[characterID]); err != nil {
				return rollback(err)
			}
			committed = append(committed, committedRecord{kind: "inventory", id: characterID})
		}
	}
	for _, characterID := range ids {
		if petTx.dirty[characterID] {
			if err := u.pets.Save(ctx, petTx.records[characterID]); err != nil {
				return rollback(err)
			}
			committed = append(committed, committedRecord{kind: "pet", id: characterID})
		}
	}
	return nil
}

type memoryCharacterTradePetTransaction struct {
	allowed map[string]struct{}
	records map[string]repository.PetRecord
	exists  map[string]bool
	dirty   map[string]bool
}

func (tx *memoryCharacterTradePetTransaction) Load(ctx context.Context, characterID string) (repository.PetRecord, bool, error) {
	characterID = strings.TrimSpace(characterID)
	if _, ok := tx.allowed[characterID]; !ok {
		return repository.PetRecord{}, false, repository.ErrAccountCharacterTradeTransactionUnavailable
	}
	return repository.ClonePet(tx.records[characterID]), tx.exists[characterID], nil
}

func (tx *memoryCharacterTradePetTransaction) Save(ctx context.Context, record repository.PetRecord) error {
	characterID := strings.TrimSpace(record.CharacterID)
	if _, ok := tx.allowed[characterID]; !ok {
		return repository.ErrAccountCharacterTradeTransactionUnavailable
	}
	tx.records[characterID] = repository.ClonePet(record)
	tx.exists[characterID] = true
	tx.dirty[characterID] = true
	return nil
}
