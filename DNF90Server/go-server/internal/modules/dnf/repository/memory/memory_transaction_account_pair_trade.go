package memory

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"

	"longheng.io/server/internal/modules/dnf/repository"
)

type memoryAccountPairCharacterTradeUnitOfWork struct {
	mu               sync.Mutex
	sharedMu         *sync.Mutex
	accountInventory repository.AccountInventoryRepository
	character        repository.CharacterRepository
	inventory        repository.InventoryRepository
}

// WithinAccountPairCharacterTrade snapshots both warehouses and both
// characters, hands scoped repositories to apply, and restores every committed
// record when a later save fails. Passing the same account twice is valid: the
// two characters then share one warehouse and see one snapshot of it.
func (u *memoryAccountPairCharacterTradeUnitOfWork) WithinAccountPairCharacterTrade(
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
		firstCharacterID == secondCharacterID || apply == nil ||
		u == nil || u.accountInventory == nil || u.character == nil || u.inventory == nil {
		return repository.ErrAccountCharacterTradeTransactionUnavailable
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	accountIDs := []string{firstAccountID}
	if secondAccountID != firstAccountID {
		accountIDs = append(accountIDs, secondAccountID)
	}
	sort.Strings(accountIDs)
	characterIDs := []string{firstCharacterID, secondCharacterID}
	sort.Strings(characterIDs)

	lock := &u.mu
	if u.sharedMu != nil {
		lock = u.sharedMu
	}
	lock.Lock()
	defer lock.Unlock()

	accountRecords := make(map[string]repository.AccountInventoryRecord, len(accountIDs))
	accountExists := make(map[string]bool, len(accountIDs))
	for _, accountID := range accountIDs {
		record, found, err := u.accountInventory.Load(ctx, accountID)
		if err != nil {
			return err
		}
		record = repository.CloneAccountInventory(record)
		record.AccountID = accountID
		if record.Slots == nil {
			record.Slots = make(map[string]repository.ItemStack)
		}
		accountRecords[accountID] = record
		accountExists[accountID] = found
	}
	characterRecords := make(map[string]repository.CharacterRecord, 2)
	characterExists := make(map[string]bool, 2)
	inventoryRecords := make(map[string]repository.InventoryRecord, 2)
	inventoryExists := make(map[string]bool, 2)
	for _, characterID := range characterIDs {
		character, found, err := u.character.Load(ctx, characterID)
		if err != nil {
			return err
		}
		characterRecords[characterID] = repository.CloneCharacter(character)
		characterExists[characterID] = found
		inventory, found, err := u.inventory.Load(ctx, characterID)
		if err != nil {
			return err
		}
		inventoryRecords[characterID] = repository.CloneInventory(inventory)
		inventoryExists[characterID] = found
	}

	allowedCharacters := map[string]struct{}{characterIDs[0]: {}, characterIDs[1]: {}}
	allowedAccounts := make(map[string]struct{}, len(accountIDs))
	for _, accountID := range accountIDs {
		allowedAccounts[accountID] = struct{}{}
	}
	accountTx := &memoryAccountInventoryPairTransaction{
		allowed: allowedAccounts,
		records: accountRecords,
		exists:  accountExists,
		dirty:   make(map[string]bool, len(accountIDs)),
	}
	characterTx := &memoryCharacterTradeCharacterTransaction{allowed: allowedCharacters, records: characterRecords, exists: characterExists, dirty: make(map[string]bool, 2)}
	inventoryTx := &memoryCharacterTradeInventoryTransaction{allowed: allowedCharacters, records: inventoryRecords, exists: inventoryExists, dirty: make(map[string]bool, 2)}
	if err := apply(accountTx, characterTx, inventoryTx); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	type committedRecord struct {
		kind string
		id   string
	}
	committed := make([]committedRecord, 0, 4)
	rollback := func(cause error) error {
		errs := []error{cause}
		for index := len(committed) - 1; index >= 0; index-- {
			row := committed[index]
			switch row.kind {
			case "account":
				errs = append(errs, restoreAccountInventoryRecord(u.accountInventory, row.id, accountRecords[row.id], accountExists[row.id]))
			case "character":
				errs = append(errs, restoreCharacterRecord(u.character, row.id, characterRecords[row.id], characterExists[row.id], true))
			case "inventory":
				errs = append(errs, restoreInventoryRecord(u.inventory, row.id, inventoryRecords[row.id], inventoryExists[row.id]))
			}
		}
		return errors.Join(errs...)
	}
	for _, accountID := range accountIDs {
		if !accountTx.dirty[accountID] {
			continue
		}
		if err := u.accountInventory.Save(ctx, accountTx.records[accountID]); err != nil {
			return rollback(err)
		}
		committed = append(committed, committedRecord{kind: "account", id: accountID})
	}
	for _, characterID := range characterIDs {
		if !characterTx.dirty[characterID] {
			continue
		}
		if err := u.character.Save(ctx, characterTx.records[characterID]); err != nil {
			return rollback(err)
		}
		committed = append(committed, committedRecord{kind: "character", id: characterID})
	}
	for _, characterID := range characterIDs {
		if !inventoryTx.dirty[characterID] {
			continue
		}
		if err := u.inventory.Save(ctx, inventoryTx.records[characterID]); err != nil {
			return rollback(err)
		}
		committed = append(committed, committedRecord{kind: "inventory", id: characterID})
	}
	return nil
}

type memoryAccountInventoryPairTransaction struct {
	allowed map[string]struct{}
	records map[string]repository.AccountInventoryRecord
	exists  map[string]bool
	dirty   map[string]bool
}

func (tx *memoryAccountInventoryPairTransaction) Load(ctx context.Context, accountID string) (repository.AccountInventoryRecord, bool, error) {
	accountID = strings.TrimSpace(accountID)
	if _, ok := tx.allowed[accountID]; !ok {
		return repository.AccountInventoryRecord{}, false, repository.ErrAccountCharacterTradeTransactionUnavailable
	}
	if err := ctx.Err(); err != nil {
		return repository.AccountInventoryRecord{}, false, err
	}
	return repository.CloneAccountInventory(tx.records[accountID]), tx.exists[accountID], nil
}

func (tx *memoryAccountInventoryPairTransaction) Save(ctx context.Context, record repository.AccountInventoryRecord) error {
	accountID := strings.TrimSpace(record.AccountID)
	if _, ok := tx.allowed[accountID]; !ok {
		return repository.ErrAccountCharacterTradeTransactionUnavailable
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	stored := repository.CloneAccountInventory(record)
	stored.AccountID = accountID
	tx.records[accountID] = stored
	tx.exists[accountID] = true
	tx.dirty[accountID] = true
	return nil
}
