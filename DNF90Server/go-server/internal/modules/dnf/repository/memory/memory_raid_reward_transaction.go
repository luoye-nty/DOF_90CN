package memory

import (
	"context"
	"errors"
	"strings"
	"sync"

	"longheng.io/server/internal/modules/dnf/repository"
	"longheng.io/server/internal/platform/db"
)

type memoryRaidRewardUnitOfWork struct {
	mu        sync.Mutex
	sharedMu  *sync.Mutex
	account   repository.AccountRepository
	inventory repository.InventoryRepository
	settings  repository.SettingsRepository
}

type memoryRaidRewardAccountRepository struct {
	allowed map[string]struct{}
	records map[string]repository.AccountRecord
	exists  map[string]bool
	dirty   map[string]bool
}

func (r *memoryRaidRewardAccountRepository) Load(ctx context.Context, accountID string) (repository.AccountRecord, bool, error) {
	accountID = strings.TrimSpace(accountID)
	if _, ok := r.allowed[accountID]; !ok {
		return repository.AccountRecord{}, false, db.ErrRecordKeyRequired
	}
	return repository.CloneAccount(r.records[accountID]), r.exists[accountID], ctx.Err()
}

func (r *memoryRaidRewardAccountRepository) Save(ctx context.Context, account repository.AccountRecord) error {
	accountID := strings.TrimSpace(account.AccountID)
	if _, ok := r.allowed[accountID]; !ok {
		return db.ErrRecordKeyRequired
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	r.records[accountID] = repository.CloneAccount(account)
	r.exists[accountID] = true
	r.dirty[accountID] = true
	return nil
}

func (u *memoryRaidRewardUnitOfWork) WithinRaidRewards(
	ctx context.Context,
	characterIDs []string,
	receiptScope string,
	apply func(repository.InventoryRepository, repository.SettingsRepository) error,
) error {
	ids, err := normalizeRaidEntryCostCharacterIDs(characterIDs)
	receiptScope = strings.TrimSpace(receiptScope)
	if err != nil || receiptScope == "" || u == nil || u.inventory == nil || u.settings == nil || apply == nil {
		return repository.ErrRaidRewardTransactionUnavailable
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

	inventories := &memoryRaidEntryCostInventory{
		allowed: make(map[string]struct{}, len(ids)),
		records: make(map[string]repository.InventoryRecord, len(ids)),
		exists:  make(map[string]bool, len(ids)),
		dirty:   make(map[string]bool, len(ids)),
	}
	preimages := make(map[string]repository.InventoryRecord, len(ids))
	for _, id := range ids {
		record, found, loadErr := u.inventory.Load(ctx, id)
		if loadErr != nil {
			return loadErr
		}
		inventories.allowed[id] = struct{}{}
		inventories.records[id] = repository.CloneInventory(record)
		inventories.exists[id] = found
		preimages[id] = repository.CloneInventory(record)
	}
	setting, settingFound, err := u.settings.Load(ctx, receiptScope)
	if err != nil {
		return err
	}
	receipt := &memorySettingsTransaction{
		scope: receiptScope, record: repository.CloneSettings(setting), exists: settingFound,
	}
	if err := apply(inventories, receipt); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	committed := make([]string, 0, len(ids))
	rollbackInventories := func() error {
		var rollback []error
		for index := len(committed) - 1; index >= 0; index-- {
			rollback = append(rollback, u.inventory.Save(context.Background(), preimages[committed[index]]))
		}
		return errors.Join(rollback...)
	}
	for _, id := range ids {
		if !inventories.dirty[id] {
			continue
		}
		if err := u.inventory.Save(ctx, repository.CloneInventory(inventories.records[id])); err != nil {
			return errors.Join(err, rollbackInventories())
		}
		committed = append(committed, id)
	}
	if receipt.dirty {
		if err := u.settings.Save(ctx, repository.CloneSettings(receipt.record)); err != nil {
			return errors.Join(err, rollbackInventories())
		}
	}
	return nil
}

func (u *memoryRaidRewardUnitOfWork) WithinRaidRewardAssets(
	ctx context.Context,
	characterIDs []string,
	accountIDs []string,
	receiptScope string,
	apply func(repository.AccountRepository, repository.InventoryRepository, repository.SettingsRepository) error,
) error {
	ids, err := normalizeRaidEntryCostCharacterIDs(characterIDs)
	receiptScope = strings.TrimSpace(receiptScope)
	if err != nil || receiptScope == "" || len(accountIDs) == 0 || u == nil ||
		u.account == nil || u.inventory == nil || u.settings == nil || apply == nil {
		return repository.ErrRaidRewardTransactionUnavailable
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	normalizedAccounts := make([]string, 0, len(accountIDs))
	seenAccounts := make(map[string]struct{}, len(accountIDs))
	for _, accountID := range accountIDs {
		accountID = strings.TrimSpace(accountID)
		if accountID == "" {
			continue
		}
		if _, duplicate := seenAccounts[accountID]; duplicate {
			continue
		}
		seenAccounts[accountID] = struct{}{}
		normalizedAccounts = append(normalizedAccounts, accountID)
	}
	if len(normalizedAccounts) == 0 {
		return repository.ErrRaidRewardTransactionUnavailable
	}
	lock := &u.mu
	if u.sharedMu != nil {
		lock = u.sharedMu
	}
	lock.Lock()
	defer lock.Unlock()

	inventories := &memoryRaidEntryCostInventory{
		allowed: make(map[string]struct{}, len(ids)),
		records: make(map[string]repository.InventoryRecord, len(ids)),
		exists:  make(map[string]bool, len(ids)),
		dirty:   make(map[string]bool, len(ids)),
	}
	inventoryPreimages := make(map[string]repository.InventoryRecord, len(ids))
	for _, id := range ids {
		record, found, loadErr := u.inventory.Load(ctx, id)
		if loadErr != nil {
			return loadErr
		}
		inventories.allowed[id] = struct{}{}
		inventories.records[id] = repository.CloneInventory(record)
		inventories.exists[id] = found
		inventoryPreimages[id] = repository.CloneInventory(record)
	}
	accounts := &memoryRaidRewardAccountRepository{
		allowed: make(map[string]struct{}, len(normalizedAccounts)),
		records: make(map[string]repository.AccountRecord, len(normalizedAccounts)),
		exists:  make(map[string]bool, len(normalizedAccounts)),
		dirty:   make(map[string]bool, len(normalizedAccounts)),
	}
	accountPreimages := make(map[string]repository.AccountRecord, len(normalizedAccounts))
	for _, accountID := range normalizedAccounts {
		record, found, loadErr := u.account.Load(ctx, accountID)
		if loadErr != nil {
			return loadErr
		}
		accounts.allowed[accountID] = struct{}{}
		accounts.records[accountID] = repository.CloneAccount(record)
		accounts.exists[accountID] = found
		accountPreimages[accountID] = repository.CloneAccount(record)
	}
	setting, settingFound, err := u.settings.Load(ctx, receiptScope)
	if err != nil {
		return err
	}
	receipt := &memorySettingsTransaction{
		scope: receiptScope, record: repository.CloneSettings(setting), exists: settingFound,
	}
	if err := apply(accounts, inventories, receipt); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	committedAccounts := make([]string, 0, len(normalizedAccounts))
	rollbackAccounts := func() error {
		var rollbackErr []error
		for index := len(committedAccounts) - 1; index >= 0; index-- {
			accountID := committedAccounts[index]
			rollbackErr = append(rollbackErr, u.account.Save(context.Background(), repository.CloneAccount(accountPreimages[accountID])))
		}
		return errors.Join(rollbackErr...)
	}
	for _, accountID := range normalizedAccounts {
		if !accounts.dirty[accountID] {
			continue
		}
		if err := u.account.Save(ctx, repository.CloneAccount(accounts.records[accountID])); err != nil {
			return errors.Join(err, rollbackAccounts())
		}
		committedAccounts = append(committedAccounts, accountID)
	}

	committedInventories := make([]string, 0, len(ids))
	rollbackInventories := func() error {
		var rollbackErr []error
		for index := len(committedInventories) - 1; index >= 0; index-- {
			rollbackErr = append(rollbackErr, u.inventory.Save(context.Background(), inventoryPreimages[committedInventories[index]]))
		}
		rollbackErr = append(rollbackErr, rollbackAccounts())
		return errors.Join(rollbackErr...)
	}
	for _, id := range ids {
		if !inventories.dirty[id] {
			continue
		}
		if err := u.inventory.Save(ctx, repository.CloneInventory(inventories.records[id])); err != nil {
			return errors.Join(err, rollbackInventories())
		}
		committedInventories = append(committedInventories, id)
	}
	if receipt.dirty {
		if err := u.settings.Save(ctx, repository.CloneSettings(receipt.record)); err != nil {
			return errors.Join(err, rollbackInventories())
		}
	}
	return nil
}
