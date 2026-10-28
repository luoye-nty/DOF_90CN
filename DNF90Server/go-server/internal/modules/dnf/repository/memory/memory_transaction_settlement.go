package memory

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"

	"longheng.io/server/internal/modules/dnf/repository"
)

// memoryCharacterSettlementUnitOfWork buffers all settlement aggregates
// before committing them under the shared aggregate transaction mutex.
type memoryCharacterSettlementUnitOfWork struct {
	mu                   sync.Mutex
	sharedMu             *sync.Mutex
	account              repository.AccountRepository
	accountInventory     repository.AccountInventoryRepository
	character            repository.CharacterRepository
	quests               repository.QuestRepository
	inventory            repository.InventoryRepository
	equipment            repository.EquipmentRepository
	skills               repository.SkillRepository
	settings             repository.SettingsRepository
	hellPartyPermissions repository.HellPartyPermissionRepository
}

func (u *memoryCharacterSettlementUnitOfWork) WithinCharacterSettlement(
	ctx context.Context,
	characterID string,
	apply func(repository.Group) error,
) (err error) {
	characterID = strings.TrimSpace(characterID)
	if characterID == "" || apply == nil || u == nil || u.character == nil ||
		u.quests == nil || u.inventory == nil || u.equipment == nil || u.skills == nil {
		return repository.ErrCharacterSettlementTransactionUnavailable
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
	var hellPartyPermissionsTx *memorySettlementHellPartyPermissionTransaction
	var permissionsCommitted bool
	defer func() {
		if err == nil || !permissionsCommitted || hellPartyPermissionsTx == nil {
			return
		}
		if rollbackErr := hellPartyPermissionsTx.rollback(ctx); rollbackErr != nil {
			err = errors.Join(err, rollbackErr)
		}
	}()

	character, characterExists, err := u.character.Load(ctx, characterID)
	if err != nil {
		return err
	}
	accountID := strings.TrimSpace(character.AccountID)
	var account repository.AccountRecord
	var accountExists bool
	if accountID != "" && u.account != nil {
		account, accountExists, err = u.account.Load(ctx, accountID)
		if err != nil {
			return err
		}
	}
	var accountInventory repository.AccountInventoryRecord
	var accountInventoryExists bool
	if accountID != "" && u.accountInventory != nil {
		accountInventory, accountInventoryExists, err = u.accountInventory.Load(ctx, accountID)
		if err != nil {
			return err
		}
	}
	questRecord, questExists, err := u.quests.Load(ctx, characterID)
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
	skills, skillExists, err := u.skills.Load(ctx, characterID)
	if err != nil {
		return err
	}
	if u.hellPartyPermissions != nil {
		hellPartyPermissionsTx = &memorySettlementHellPartyPermissionTransaction{base: u.hellPartyPermissions, characterID: characterID}
	}
	settingsTx := &memorySettlementSettingsTransaction{
		base:           u.settings,
		records:        make(map[string]repository.SettingsRecord),
		exists:         make(map[string]bool),
		original:       make(map[string]repository.SettingsRecord),
		originalExists: make(map[string]bool),
		dirty:          make(map[string]bool),
	}

	characterTx := &memoryCharacterTransaction{
		characterID: characterID,
		record:      repository.CloneCharacter(character),
		exists:      characterExists,
	}
	questTx := &memoryQuestTransaction{
		characterID: characterID,
		record:      repository.CloneQuest(questRecord),
		exists:      questExists,
	}
	inventoryTx := &memoryInventoryTransaction{
		characterID: characterID,
		record:      repository.CloneInventory(inventory),
		exists:      inventoryExists,
	}
	equipmentTx := &memoryEquipmentTransaction{
		characterID: characterID,
		record:      repository.CloneEquipment(equipment),
		exists:      equipmentExists,
	}
	skillTx := &memorySkillTransaction{
		characterID: characterID,
		record:      repository.CloneSkill(skills),
		exists:      skillExists,
	}
	accountInventoryTx := &memoryAccountInventoryTransaction{
		accountID: accountID,
		record:    repository.CloneAccountInventory(accountInventory),
		exists:    accountInventoryExists,
	}
	var accountTx *memoryAccountTransaction
	if u.account != nil {
		accountTx = &memoryAccountTransaction{
			accountID: accountID,
			record:    repository.CloneAccount(account),
			exists:    accountExists,
		}
	}
	txGroup := repository.Group{
		Account:          accountTx,
		AccountInventory: accountInventoryTx,
		Character:        characterTx,
		Quest:            questTx,
		Inventory:        inventoryTx,
		Equipment:        equipmentTx,
		Skill:            skillTx,
		Settings:         settingsTx,
	}
	if hellPartyPermissionsTx != nil {
		txGroup.HellPartyPermission = hellPartyPermissionsTx
	}
	if err := apply(txGroup); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	if characterTx.dirty {
		if err := u.character.Save(ctx, characterTx.record); err != nil {
			return errors.Join(
				err,
				restoreCharacterRecord(u.character, characterID, character, characterExists, true),
			)
		}
	}
	if questTx.dirty {
		if err := u.quests.Save(ctx, questTx.record); err != nil {
			return errors.Join(
				err,
				restoreQuestRecordIfDirty(u.quests, characterID, questRecord, questExists, true),
				restoreCharacterRecord(u.character, characterID, character, characterExists, characterTx.dirty),
			)
		}
	}
	if skillTx.dirty {
		if err := u.skills.Save(ctx, skillTx.record); err != nil {
			return errors.Join(
				err,
				restoreSkillRecord(u.skills, characterID, skills, skillExists, true),
				restoreQuestRecordIfDirty(u.quests, characterID, questRecord, questExists, questTx.dirty),
				restoreCharacterRecord(u.character, characterID, character, characterExists, characterTx.dirty),
			)
		}
	}
	if inventoryTx.dirty {
		if err := u.inventory.Save(ctx, inventoryTx.record); err != nil {
			return errors.Join(
				err,
				restoreInventoryRecordIfDirty(u.inventory, characterID, inventory, inventoryExists, true),
				restoreSkillRecord(u.skills, characterID, skills, skillExists, skillTx.dirty),
				restoreQuestRecordIfDirty(u.quests, characterID, questRecord, questExists, questTx.dirty),
				restoreCharacterRecord(u.character, characterID, character, characterExists, characterTx.dirty),
			)
		}
	}
	if equipmentTx.dirty {
		if err := u.equipment.Save(ctx, equipmentTx.record); err != nil {
			return errors.Join(
				err,
				restoreEquipmentRecordIfDirty(u.equipment, characterID, equipment, equipmentExists, true),
				restoreInventoryRecordIfDirty(u.inventory, characterID, inventory, inventoryExists, inventoryTx.dirty),
				restoreSkillRecord(u.skills, characterID, skills, skillExists, skillTx.dirty),
				restoreQuestRecordIfDirty(u.quests, characterID, questRecord, questExists, questTx.dirty),
				restoreCharacterRecord(u.character, characterID, character, characterExists, characterTx.dirty),
			)
		}
	}
	if settingsTx.dirtyCount() > 0 {
		if err := settingsTx.commit(ctx); err != nil {
			return errors.Join(
				err,
				restoreEquipmentRecordIfDirty(u.equipment, characterID, equipment, equipmentExists, equipmentTx.dirty),
				restoreInventoryRecordIfDirty(u.inventory, characterID, inventory, inventoryExists, inventoryTx.dirty),
				restoreSkillRecord(u.skills, characterID, skills, skillExists, skillTx.dirty),
				restoreQuestRecordIfDirty(u.quests, characterID, questRecord, questExists, questTx.dirty),
				restoreCharacterRecord(u.character, characterID, character, characterExists, characterTx.dirty),
			)
		}
	}
	if accountInventoryTx.dirty {
		if u.accountInventory == nil {
			return errors.Join(
				repository.ErrCharacterSettlementTransactionUnavailable,
				settingsTx.rollback(),
				restoreEquipmentRecordIfDirty(u.equipment, characterID, equipment, equipmentExists, equipmentTx.dirty),
				restoreInventoryRecordIfDirty(u.inventory, characterID, inventory, inventoryExists, inventoryTx.dirty),
				restoreSkillRecord(u.skills, characterID, skills, skillExists, skillTx.dirty),
				restoreQuestRecordIfDirty(u.quests, characterID, questRecord, questExists, questTx.dirty),
				restoreCharacterRecord(u.character, characterID, character, characterExists, characterTx.dirty),
			)
		}
		if err := u.accountInventory.Save(ctx, accountInventoryTx.record); err != nil {
			return errors.Join(
				err,
				restoreAccountInventoryRecord(u.accountInventory, accountID, accountInventory, accountInventoryExists),
				settingsTx.rollback(),
				restoreEquipmentRecordIfDirty(u.equipment, characterID, equipment, equipmentExists, equipmentTx.dirty),
				restoreInventoryRecordIfDirty(u.inventory, characterID, inventory, inventoryExists, inventoryTx.dirty),
				restoreSkillRecord(u.skills, characterID, skills, skillExists, skillTx.dirty),
				restoreQuestRecordIfDirty(u.quests, characterID, questRecord, questExists, questTx.dirty),
				restoreCharacterRecord(u.character, characterID, character, characterExists, characterTx.dirty),
			)
		}
	}
	if accountTx != nil && accountTx.dirty {
		if err := u.account.Save(ctx, accountTx.record); err != nil {
			return errors.Join(
				err,
				restoreAccountRecord(u.account, accountID, account, accountExists, true),
				restoreAccountInventoryRecordIfDirty(u.accountInventory, accountID, accountInventory, accountInventoryExists, accountInventoryTx.dirty),
				settingsTx.rollback(),
				restoreEquipmentRecordIfDirty(u.equipment, characterID, equipment, equipmentExists, equipmentTx.dirty),
				restoreInventoryRecordIfDirty(u.inventory, characterID, inventory, inventoryExists, inventoryTx.dirty),
				restoreSkillRecord(u.skills, characterID, skills, skillExists, skillTx.dirty),
				restoreQuestRecordIfDirty(u.quests, characterID, questRecord, questExists, questTx.dirty),
				restoreCharacterRecord(u.character, characterID, character, characterExists, characterTx.dirty),
			)
		}
	}
	if hellPartyPermissionsTx != nil {
		if err := hellPartyPermissionsTx.commit(ctx); err != nil {
			return err
		}
		permissionsCommitted = true
	}
	return nil
}

type memorySettlementHellPartyPermissionTransaction struct {
	base        repository.HellPartyPermissionRepository
	characterID string
	pending     []repository.HellPartyPermissionEntry
}

func (tx *memorySettlementHellPartyPermissionTransaction) Load(ctx context.Context, characterID string) (repository.HellPartyPermissionRecord, bool, error) {
	if tx == nil || tx.base == nil {
		return repository.HellPartyPermissionRecord{}, false, repository.ErrCharacterSettlementTransactionUnavailable
	}
	if err := repository.TransactionKeyError(ctx, tx.characterID, characterID); err != nil {
		return repository.HellPartyPermissionRecord{}, false, err
	}
	record, found, err := tx.base.Load(ctx, characterID)
	if err != nil {
		return repository.HellPartyPermissionRecord{}, false, err
	}
	for _, pending := range tx.pending {
		seen := false
		for _, entry := range record.Entries {
			if entry.AreaID == pending.AreaID {
				seen = true
				break
			}
		}
		if !seen {
			record.Entries = append(record.Entries, pending)
			found = true
		}
	}
	return repository.CloneHellPartyPermission(record), found, nil
}

func (tx *memorySettlementHellPartyPermissionTransaction) Upsert(ctx context.Context, characterID string, areaID, questID int64) (repository.HellPartyPermissionEntry, bool, error) {
	if err := repository.TransactionKeyError(ctx, tx.characterID, characterID); err != nil {
		return repository.HellPartyPermissionEntry{}, false, err
	}
	if areaID <= 0 || questID <= 0 {
		return repository.HellPartyPermissionEntry{}, false, nil
	}
	record, _, err := tx.Load(ctx, characterID)
	if err != nil {
		return repository.HellPartyPermissionEntry{}, false, err
	}
	for _, entry := range record.Entries {
		if entry.AreaID == areaID {
			return entry, false, nil
		}
	}
	entry := repository.HellPartyPermissionEntry{AreaID: areaID, QuestID: questID}
	tx.pending = append(tx.pending, entry)
	return entry, true, nil
}

func (tx *memorySettlementHellPartyPermissionTransaction) commit(ctx context.Context) error {
	if tx == nil || tx.base == nil {
		return repository.ErrCharacterSettlementTransactionUnavailable
	}
	inserted := make([]repository.HellPartyPermissionEntry, 0, len(tx.pending))
	for _, entry := range tx.pending {
		if _, changed, err := tx.base.Upsert(ctx, tx.characterID, entry.AreaID, entry.QuestID); err != nil {
			for _, prior := range inserted {
				_ = tx.delete(ctx, prior.AreaID)
			}
			return err
		} else if changed {
			inserted = append(inserted, entry)
		}
	}
	return nil
}

func (tx *memorySettlementHellPartyPermissionTransaction) rollback(ctx context.Context) error {
	if tx == nil || tx.base == nil {
		return nil
	}
	var joined error
	for _, entry := range tx.pending {
		if err := tx.delete(ctx, entry.AreaID); err != nil {
			joined = errors.Join(joined, err)
		}
	}
	return joined
}

func (tx *memorySettlementHellPartyPermissionTransaction) delete(ctx context.Context, areaID int64) error {
	restorer, ok := tx.base.(repository.HellPartyPermissionRestorer)
	if !ok {
		return repository.ErrCharacterSettlementTransactionUnavailable
	}
	return restorer.Delete(ctx, tx.characterID, areaID)
}

type memorySettlementSettingsTransaction struct {
	base           repository.SettingsRepository
	records        map[string]repository.SettingsRecord
	exists         map[string]bool
	original       map[string]repository.SettingsRecord
	originalExists map[string]bool
	dirty          map[string]bool
	committed      []string
}

func (tx *memorySettlementSettingsTransaction) Load(ctx context.Context, scope string) (repository.SettingsRecord, bool, error) {
	if tx == nil || tx.base == nil {
		return repository.SettingsRecord{}, false, repository.ErrCharacterSettlementTransactionUnavailable
	}
	if record, cached := tx.records[scope]; cached || tx.exists[scope] {
		return repository.CloneSettings(record), tx.exists[scope], nil
	}
	record, found, err := tx.base.Load(ctx, scope)
	if err != nil {
		return repository.SettingsRecord{}, false, err
	}
	tx.records[scope] = repository.CloneSettings(record)
	tx.exists[scope] = found
	tx.original[scope] = repository.CloneSettings(record)
	tx.originalExists[scope] = found
	return repository.CloneSettings(record), found, nil
}

func (tx *memorySettlementSettingsTransaction) Save(ctx context.Context, record repository.SettingsRecord) error {
	if tx == nil || tx.base == nil {
		return repository.ErrCharacterSettlementTransactionUnavailable
	}
	scope := repository.SettingsKey(record)
	if scope == "" {
		return repository.ErrCharacterSettlementTransactionUnavailable
	}
	if _, captured := tx.original[scope]; !captured {
		original, found, err := tx.base.Load(ctx, scope)
		if err != nil {
			return err
		}
		tx.original[scope] = repository.CloneSettings(original)
		tx.originalExists[scope] = found
	}
	tx.records[scope] = repository.CloneSettings(record)
	tx.exists[scope] = true
	tx.dirty[scope] = true
	return nil
}

func (tx *memorySettlementSettingsTransaction) dirtyCount() int {
	if tx == nil {
		return 0
	}
	return len(tx.dirty)
}

func (tx *memorySettlementSettingsTransaction) commit(ctx context.Context) error {
	scopes := make([]string, 0, len(tx.dirty))
	for scope := range tx.dirty {
		scopes = append(scopes, scope)
	}
	sort.Strings(scopes)
	for _, scope := range scopes {
		// A store is allowed to mutate and then report an error. Register the
		// scope before Save so the pre-image is restored in either outcome.
		tx.committed = append(tx.committed, scope)
		if err := tx.base.Save(ctx, tx.records[scope]); err != nil {
			return errors.Join(err, tx.rollback())
		}
	}
	return nil
}

func (tx *memorySettlementSettingsTransaction) rollback() error {
	if tx == nil || tx.base == nil {
		return nil
	}
	var rollbackErr error
	for index := len(tx.committed) - 1; index >= 0; index-- {
		scope := tx.committed[index]
		rollbackErr = errors.Join(rollbackErr, restoreSettingsRecord(
			tx.base,
			scope,
			tx.original[scope],
			tx.originalExists[scope],
			true,
		))
	}
	tx.committed = nil
	return rollbackErr
}

type memoryQuestTransaction struct {
	characterID string
	record      repository.QuestRecord
	exists      bool
	dirty       bool
}

func (tx *memoryQuestTransaction) Load(ctx context.Context, characterID string) (repository.QuestRecord, bool, error) {
	if err := repository.TransactionKeyError(ctx, tx.characterID, characterID); err != nil {
		return repository.QuestRecord{}, false, err
	}
	return repository.CloneQuest(tx.record), tx.exists, nil
}

func (tx *memoryQuestTransaction) Save(ctx context.Context, record repository.QuestRecord) error {
	if err := repository.TransactionKeyError(ctx, tx.characterID, record.CharacterID); err != nil {
		return err
	}
	tx.record = repository.CloneQuest(record)
	tx.exists = true
	tx.dirty = true
	return nil
}

func restoreQuestRecordIfDirty(
	repo repository.QuestRepository,
	characterID string,
	record repository.QuestRecord,
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
		return errors.New("quest rollback cannot remove newly created record")
	}
	return deleter.Delete(context.Background(), characterID)
}
