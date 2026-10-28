package memory

import (
	"context"
	"errors"
	"longheng.io/server/internal/modules/dnf/repository"
	"strings"
	"sync"
)

type memoryMailboxAssetUnitOfWork struct {
	mu               sync.Mutex
	sharedMu         *sync.Mutex
	account          repository.AccountRepository
	accountInventory repository.AccountInventoryRepository
	character        repository.CharacterRepository
	inventory        repository.InventoryRepository
	mailbox          repository.MailboxRepository
	epicBook         repository.EpicBookRepository
}

func (u *memoryMailboxAssetUnitOfWork) WithinMailboxAssets(
	ctx context.Context,
	assetCharacterID string,
	mailboxCharacterID string,
	apply func(repository.AccountRepository, repository.AccountInventoryRepository, repository.CharacterRepository, repository.InventoryRepository, repository.MailboxRepository) error,
) error {
	if apply == nil {
		return repository.ErrMailboxAssetTransactionUnavailable
	}
	return u.withinMailboxAssets(ctx, assetCharacterID, mailboxCharacterID, false, func(
		accounts repository.AccountRepository,
		accountInventories repository.AccountInventoryRepository,
		characters repository.CharacterRepository,
		inventories repository.InventoryRepository,
		mailboxes repository.MailboxRepository,
		_ repository.EpicBookRepository,
	) error {
		return apply(accounts, accountInventories, characters, inventories, mailboxes)
	})
}

func (u *memoryMailboxAssetUnitOfWork) WithinMailboxClaimAssets(
	ctx context.Context,
	assetCharacterID string,
	mailboxCharacterID string,
	apply func(repository.AccountRepository, repository.AccountInventoryRepository, repository.CharacterRepository, repository.InventoryRepository, repository.MailboxRepository, repository.EpicBookRepository) error,
) error {
	return u.withinMailboxAssets(ctx, assetCharacterID, mailboxCharacterID, true, apply)
}

func (u *memoryMailboxAssetUnitOfWork) withinMailboxAssets(
	ctx context.Context,
	assetCharacterID string,
	mailboxCharacterID string,
	includeEpicBook bool,
	apply func(repository.AccountRepository, repository.AccountInventoryRepository, repository.CharacterRepository, repository.InventoryRepository, repository.MailboxRepository, repository.EpicBookRepository) error,
) error {
	assetCharacterID = strings.TrimSpace(assetCharacterID)
	mailboxCharacterID = strings.TrimSpace(mailboxCharacterID)
	if assetCharacterID == "" || mailboxCharacterID == "" || apply == nil || u == nil ||
		u.account == nil || u.accountInventory == nil || u.character == nil || u.inventory == nil || u.mailbox == nil ||
		(includeEpicBook && u.epicBook == nil) {
		return repository.ErrMailboxAssetTransactionUnavailable
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

	character, characterExists, err := u.character.Load(ctx, assetCharacterID)
	if err != nil {
		return err
	}
	accountID := strings.TrimSpace(character.AccountID)
	var account repository.AccountRecord
	var accountExists bool
	if accountID != "" {
		account, accountExists, err = u.account.Load(ctx, accountID)
		if err != nil {
			return err
		}
	}
	var accountInventory repository.AccountInventoryRecord
	var accountInventoryExists bool
	if accountID != "" {
		accountInventory, accountInventoryExists, err = u.accountInventory.Load(ctx, accountID)
		if err != nil {
			return err
		}
	}
	var epicBook repository.EpicBookRecord
	var epicBookExists bool
	if includeEpicBook && accountID != "" {
		epicBook, epicBookExists, err = u.epicBook.Load(ctx, accountID)
		if err != nil {
			return err
		}
	}
	inventory, inventoryExists, err := u.inventory.Load(ctx, assetCharacterID)
	if err != nil {
		return err
	}
	mailbox, mailboxExists, err := u.mailbox.Load(ctx, mailboxCharacterID)
	if err != nil {
		return err
	}

	accountTx := &memoryAccountTransaction{
		accountID: accountID,
		record:    repository.CloneAccount(account),
		exists:    accountExists,
	}
	accountInventoryTx := &memoryAccountInventoryTransaction{
		accountID: accountID,
		record:    repository.CloneAccountInventory(accountInventory),
		exists:    accountInventoryExists,
	}
	characterTx := &memoryCharacterTransaction{
		characterID: assetCharacterID,
		record:      repository.CloneCharacter(character),
		exists:      characterExists,
	}
	inventoryTx := &memoryInventoryTransaction{
		characterID: assetCharacterID,
		record:      repository.CloneInventory(inventory),
		exists:      inventoryExists,
	}
	mailboxTx := &memoryMailboxTransaction{
		characterID: mailboxCharacterID,
		record:      repository.CloneMailbox(mailbox),
		exists:      mailboxExists,
	}
	var epicBookTx *memoryEpicBookTransaction
	if includeEpicBook {
		epicBookTx = &memoryEpicBookTransaction{
			accountID: accountID,
			record:    repository.CloneEpicBook(epicBook),
			exists:    epicBookExists,
		}
	}
	if err := apply(accountTx, accountInventoryTx, characterTx, inventoryTx, mailboxTx, epicBookTx); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	if accountTx.dirty {
		if err := u.account.Save(ctx, accountTx.record); err != nil {
			return errors.Join(err, restoreAccountRecord(u.account, accountID, account, accountExists, true))
		}
	}
	if accountInventoryTx.dirty {
		if err := u.accountInventory.Save(ctx, accountInventoryTx.record); err != nil {
			return errors.Join(
				err,
				restoreAccountInventoryRecord(u.accountInventory, accountID, accountInventory, accountInventoryExists),
				restoreAccountRecord(u.account, accountID, account, accountExists, accountTx.dirty),
			)
		}
	}
	if characterTx.dirty {
		if err := u.character.Save(ctx, characterTx.record); err != nil {
			return errors.Join(err, restoreCharacterRecord(
				u.character,
				assetCharacterID,
				character,
				characterExists,
				true,
			),
				restoreAccountInventoryRecordIfDirty(u.accountInventory, accountID, accountInventory, accountInventoryExists, accountInventoryTx.dirty),
				restoreAccountRecord(u.account, accountID, account, accountExists, accountTx.dirty),
			)
		}
	}
	if inventoryTx.dirty {
		if err := u.inventory.Save(ctx, inventoryTx.record); err != nil {
			return errors.Join(
				err,
				restoreInventoryRecordIfDirty(u.inventory, assetCharacterID, inventory, inventoryExists, true),
				restoreCharacterRecord(u.character, assetCharacterID, character, characterExists, characterTx.dirty),
				restoreAccountInventoryRecordIfDirty(u.accountInventory, accountID, accountInventory, accountInventoryExists, accountInventoryTx.dirty),
				restoreAccountRecord(u.account, accountID, account, accountExists, accountTx.dirty),
			)
		}
	}
	if epicBookTx != nil && epicBookTx.dirty {
		if err := u.epicBook.Save(ctx, epicBookTx.record); err != nil {
			return errors.Join(
				err,
				restoreEpicBookRecord(u.epicBook, accountID, epicBook, epicBookExists),
				restoreInventoryRecordIfDirty(u.inventory, assetCharacterID, inventory, inventoryExists, inventoryTx.dirty),
				restoreCharacterRecord(u.character, assetCharacterID, character, characterExists, characterTx.dirty),
				restoreAccountInventoryRecordIfDirty(u.accountInventory, accountID, accountInventory, accountInventoryExists, accountInventoryTx.dirty),
				restoreAccountRecord(u.account, accountID, account, accountExists, accountTx.dirty),
			)
		}
	}
	if mailboxTx.dirty {
		if err := u.mailbox.Save(ctx, mailboxTx.record); err != nil {
			return errors.Join(
				err,
				restoreMailboxRecordIfDirty(u.mailbox, mailboxCharacterID, mailbox, mailboxExists, true),
				restoreEpicBookRecordIfDirty(u.epicBook, accountID, epicBook, epicBookExists, epicBookTx != nil && epicBookTx.dirty),
				restoreInventoryRecordIfDirty(u.inventory, assetCharacterID, inventory, inventoryExists, inventoryTx.dirty),
				restoreCharacterRecord(u.character, assetCharacterID, character, characterExists, characterTx.dirty),
				restoreAccountInventoryRecordIfDirty(u.accountInventory, accountID, accountInventory, accountInventoryExists, accountInventoryTx.dirty),
				restoreAccountRecord(u.account, accountID, account, accountExists, accountTx.dirty),
			)
		}
	}
	return nil
}

type memoryMailboxTransaction struct {
	characterID string
	record      repository.MailboxRecord
	exists      bool
	dirty       bool
}

func (tx *memoryMailboxTransaction) Load(ctx context.Context, characterID string) (repository.MailboxRecord, bool, error) {
	if err := repository.TransactionKeyError(ctx, tx.characterID, characterID); err != nil {
		return repository.MailboxRecord{}, false, err
	}
	return repository.CloneMailbox(tx.record), tx.exists, nil
}

func (tx *memoryMailboxTransaction) Save(ctx context.Context, record repository.MailboxRecord) error {
	if err := repository.TransactionKeyError(ctx, tx.characterID, record.CharacterID); err != nil {
		return err
	}
	tx.record = repository.CloneMailbox(record)
	tx.exists = true
	tx.dirty = true
	return nil
}

func (tx *memoryMailboxTransaction) SaveFields(ctx context.Context, record repository.MailboxRecord, fields ...repository.MailboxField) error {
	if err := repository.TransactionKeyError(ctx, tx.characterID, record.CharacterID); err != nil {
		return err
	}
	if len(fields) == 0 {
		return nil
	}
	tx.record = repository.CloneMailbox(record)
	tx.exists = true
	tx.dirty = true
	return nil
}

func restoreMailboxRecordIfDirty(
	repo repository.MailboxRepository,
	characterID string,
	record repository.MailboxRecord,
	existed bool,
	dirty bool,
) error {
	if !dirty {
		return nil
	}
	if existed {
		return repo.Save(context.Background(), record)
	}
	deleter, ok := repo.(interface {
		Delete(context.Context, string) error
	})
	if !ok {
		return errors.New("mailbox rollback cannot remove newly created record")
	}
	return deleter.Delete(context.Background(), characterID)
}
