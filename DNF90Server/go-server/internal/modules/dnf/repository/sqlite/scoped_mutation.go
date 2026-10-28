package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"longheng.io/server/internal/modules/dnf/repository"
	"longheng.io/server/internal/modules/dnf/repository/memory"
)

// scopedRecordRef identifies one aggregate record that a synchronous unit of
// work is allowed to mutate. Keeping this list explicit prevents a small item
// transaction from cloning, checksumming and writing the complete runtime
// snapshot while it owns SQLite's mutation gate.
type scopedRecordRef struct {
	kind string
	key  string
}

func (c *Component) mutateCharacterItemsScopedSync(
	ctx context.Context,
	characterID string,
	apply func(repository.InventoryRepository, repository.EquipmentRepository) error,
) error {
	characterID = strings.TrimSpace(characterID)
	if characterID == "" || apply == nil {
		return repository.ErrCharacterItemTransactionUnavailable
	}
	return c.mutateRecordScopeSync(ctx, []scopedRecordRef{
		{kind: kindInventory, key: characterID},
		{kind: kindEquipment, key: characterID},
	}, func(group repository.Group) error {
		return group.CharacterItems.WithinCharacterItems(ctx, characterID, apply)
	})
}

func (c *Component) mutateCharacterPetsScopedSync(
	ctx context.Context,
	characterID string,
	apply func(repository.InventoryRepository, repository.EquipmentRepository, repository.PetRepository) error,
) error {
	characterID = strings.TrimSpace(characterID)
	if characterID == "" || apply == nil {
		return repository.ErrCharacterPetTransactionUnavailable
	}
	return c.mutateRecordScopeSync(ctx, []scopedRecordRef{
		{kind: kindInventory, key: characterID},
		{kind: kindEquipment, key: characterID},
		{kind: kindPet, key: characterID},
	}, func(group repository.Group) error {
		return group.CharacterPets.WithinCharacterPets(ctx, characterID, apply)
	})
}

func (c *Component) mutateRentalAssetsScopedSync(
	ctx context.Context,
	accountID string,
	characterID string,
	apply func(repository.AccountRepository, repository.CharacterRepository, repository.InventoryRepository, repository.EquipmentRepository) error,
) error {
	accountID = strings.TrimSpace(accountID)
	characterID = strings.TrimSpace(characterID)
	if accountID == "" || characterID == "" || apply == nil {
		return repository.ErrRentalAssetTransactionUnavailable
	}
	return c.mutateRecordScopeSync(ctx, []scopedRecordRef{
		{kind: kindAccount, key: accountID},
		{kind: kindCharacter, key: characterID},
		{kind: kindInventory, key: characterID},
		{kind: kindEquipment, key: characterID},
	}, func(group repository.Group) error {
		return group.RentalAssets.WithinRentalAssets(ctx, accountID, characterID, apply)
	})
}

func (c *Component) mutateRecordScopeSync(
	ctx context.Context,
	refs []scopedRecordRef,
	apply func(repository.Group) error,
) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	refs, err := normalizeScopedRecordRefs(refs)
	if err != nil {
		return err
	}
	if apply == nil {
		return fmt.Errorf("sqlite scoped mutation callback is nil")
	}
	if err := c.acquireMutation(ctx); err != nil {
		return err
	}
	defer c.releaseMutation()

	c.mu.RLock()
	if !c.started || c.runtime == nil {
		c.mu.RUnlock()
		return ErrDatabaseRequired
	}
	group := c.runtime.Group()
	persistedBefore, err := selectScopedState(c.persisted, refs)
	dirtyBefore := c.dirty
	c.mu.RUnlock()
	if err != nil {
		return err
	}

	runtimeBefore, err := loadScopedRuntimeState(ctx, group, refs)
	if err != nil {
		return err
	}
	if err := apply(group); err != nil {
		return err
	}
	runtimeAfter, err := loadScopedRuntimeState(ctx, group, refs)
	if err != nil {
		return errors.Join(err, restoreScopedRuntimeState(group, refs, runtimeBefore))
	}
	if err := persistScopedStateTransition(ctx, c.db, persistedBefore, runtimeAfter); err != nil {
		return errors.Join(err, restoreScopedRuntimeState(group, refs, runtimeBefore))
	}

	c.mu.Lock()
	replaceScopedState(&c.persisted, refs, runtimeAfter)
	// An earlier asynchronous mutation may still own an unrelated dirty
	// record. Never clear that signal merely because this scope is durable.
	c.dirty = dirtyBefore
	c.mu.Unlock()
	return nil
}

func normalizeScopedRecordRefs(refs []scopedRecordRef) ([]scopedRecordRef, error) {
	out := make([]scopedRecordRef, 0, len(refs))
	seen := make(map[string]struct{}, len(refs))
	for _, ref := range refs {
		ref.kind = strings.TrimSpace(ref.kind)
		ref.key = strings.TrimSpace(ref.key)
		if ref.key == "" {
			return nil, fmt.Errorf("sqlite scoped mutation record key is empty")
		}
		switch ref.kind {
		case kindAccount, kindCharacter, kindInventory, kindEquipment, kindPet:
		default:
			return nil, fmt.Errorf("unsupported sqlite scoped mutation kind %q", ref.kind)
		}
		identity := ref.kind + "\x00" + ref.key
		if _, exists := seen[identity]; exists {
			continue
		}
		seen[identity] = struct{}{}
		out = append(out, ref)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("sqlite scoped mutation records are empty")
	}
	return out, nil
}

func loadScopedRuntimeState(ctx context.Context, group repository.Group, refs []scopedRecordRef) (memory.State, error) {
	state := memory.State{Version: memory.StateVersion}
	for _, ref := range refs {
		switch ref.kind {
		case kindAccount:
			record, found, err := group.Account.Load(ctx, ref.key)
			if err != nil {
				return memory.State{}, err
			}
			if found {
				state.Accounts = append(state.Accounts, repository.CloneAccount(record))
			}
		case kindCharacter:
			record, found, err := group.Character.Load(ctx, ref.key)
			if err != nil {
				return memory.State{}, err
			}
			if found {
				state.Characters = append(state.Characters, repository.CloneCharacter(record))
			}
		case kindInventory:
			record, found, err := group.Inventory.Load(ctx, ref.key)
			if err != nil {
				return memory.State{}, err
			}
			if found {
				state.Inventories = append(state.Inventories, repository.CloneInventory(record))
			}
		case kindEquipment:
			record, found, err := group.Equipment.Load(ctx, ref.key)
			if err != nil {
				return memory.State{}, err
			}
			if found {
				state.Equipment = append(state.Equipment, repository.CloneEquipment(record))
			}
		case kindPet:
			record, found, err := group.Pet.Load(ctx, ref.key)
			if err != nil {
				return memory.State{}, err
			}
			if found {
				state.Pets = append(state.Pets, repository.ClonePet(record))
			}
		}
	}
	return state, nil
}

func selectScopedState(state memory.State, refs []scopedRecordRef) (memory.State, error) {
	// The persisted snapshot is immutable while the mutation gate is held. A
	// tiny memory runtime gives us the same clone and key-validation behavior
	// as live repositories without serializing unrelated records.
	temporary, err := memory.NewRuntimeFromState(context.Background(), stateForScopedRefs(state, refs))
	if err != nil {
		return memory.State{}, err
	}
	return loadScopedRuntimeState(context.Background(), temporary.Group(), refs)
}

func stateForScopedRefs(state memory.State, refs []scopedRecordRef) memory.State {
	out := memory.State{Version: memory.StateVersion}
	for _, ref := range refs {
		switch ref.kind {
		case kindAccount:
			for _, record := range state.Accounts {
				if record.AccountID == ref.key {
					out.Accounts = append(out.Accounts, repository.CloneAccount(record))
					break
				}
			}
		case kindCharacter:
			for _, record := range state.Characters {
				if record.CharacterID == ref.key {
					out.Characters = append(out.Characters, repository.CloneCharacter(record))
					break
				}
			}
		case kindInventory:
			for _, record := range state.Inventories {
				if record.CharacterID == ref.key {
					out.Inventories = append(out.Inventories, repository.CloneInventory(record))
					break
				}
			}
		case kindEquipment:
			for _, record := range state.Equipment {
				if record.CharacterID == ref.key {
					out.Equipment = append(out.Equipment, repository.CloneEquipment(record))
					break
				}
			}
		case kindPet:
			for _, record := range state.Pets {
				if record.CharacterID == ref.key {
					out.Pets = append(out.Pets, repository.ClonePet(record))
					break
				}
			}
		}
	}
	return out
}

func persistScopedStateTransition(ctx context.Context, database *sql.DB, before, after memory.State) error {
	oldRecords, err := stateRecords(before)
	if err != nil {
		return err
	}
	newRecords, err := stateRecords(after)
	if err != nil {
		return err
	}
	return persistRecordTransition(ctx, database, oldRecords, newRecords)
}

func replaceScopedState(target *memory.State, refs []scopedRecordRef, replacement memory.State) {
	if target == nil {
		return
	}
	for _, ref := range refs {
		switch ref.kind {
		case kindAccount:
			target.Accounts = removeAccountStateRecord(target.Accounts, ref.key)
		case kindCharacter:
			target.Characters = removeCharacterStateRecord(target.Characters, ref.key)
		case kindInventory:
			target.Inventories = removeInventoryStateRecord(target.Inventories, ref.key)
		case kindEquipment:
			target.Equipment = removeEquipmentStateRecord(target.Equipment, ref.key)
		case kindPet:
			target.Pets = removePetStateRecord(target.Pets, ref.key)
		}
	}
	target.Accounts = append(target.Accounts, replacement.Accounts...)
	target.Characters = append(target.Characters, replacement.Characters...)
	target.Inventories = append(target.Inventories, replacement.Inventories...)
	target.Equipment = append(target.Equipment, replacement.Equipment...)
	target.Pets = append(target.Pets, replacement.Pets...)
}

func restoreScopedRuntimeState(group repository.Group, refs []scopedRecordRef, state memory.State) error {
	var restoreErrors []error
	for _, ref := range refs {
		switch ref.kind {
		case kindAccount:
			record, found := findAccountStateRecord(state.Accounts, ref.key)
			restoreErrors = appendRestoreError(restoreErrors, restoreStoreRecord(group.Account, ref.key, record, found))
		case kindCharacter:
			record, found := findCharacterStateRecord(state.Characters, ref.key)
			restoreErrors = appendRestoreError(restoreErrors, restoreStoreRecord(group.Character, ref.key, record, found))
		case kindInventory:
			record, found := findInventoryStateRecord(state.Inventories, ref.key)
			restoreErrors = appendRestoreError(restoreErrors, restoreStoreRecord(group.Inventory, ref.key, record, found))
		case kindEquipment:
			record, found := findEquipmentStateRecord(state.Equipment, ref.key)
			restoreErrors = appendRestoreError(restoreErrors, restoreStoreRecord(group.Equipment, ref.key, record, found))
		case kindPet:
			record, found := findPetStateRecord(state.Pets, ref.key)
			restoreErrors = appendRestoreError(restoreErrors, restoreStoreRecord(group.Pet, ref.key, record, found))
		}
	}
	return errors.Join(restoreErrors...)
}

type deletableStore[T any] interface {
	Save(context.Context, T) error
	Delete(context.Context, string) error
}

func restoreStoreRecord[T any](store interface {
	Save(context.Context, T) error
}, key string, record T, found bool) error {
	if found {
		return store.Save(context.Background(), record)
	}
	deletable, ok := store.(deletableStore[T])
	if !ok {
		return fmt.Errorf("sqlite scoped rollback cannot delete %q", key)
	}
	return deletable.Delete(context.Background(), key)
}

func appendRestoreError(errs []error, err error) []error {
	if err != nil {
		return append(errs, err)
	}
	return errs
}

func findAccountStateRecord(records []repository.AccountRecord, key string) (repository.AccountRecord, bool) {
	for _, record := range records {
		if record.AccountID == key {
			return repository.CloneAccount(record), true
		}
	}
	return repository.AccountRecord{}, false
}

func findCharacterStateRecord(records []repository.CharacterRecord, key string) (repository.CharacterRecord, bool) {
	for _, record := range records {
		if record.CharacterID == key {
			return repository.CloneCharacter(record), true
		}
	}
	return repository.CharacterRecord{}, false
}

func findInventoryStateRecord(records []repository.InventoryRecord, key string) (repository.InventoryRecord, bool) {
	for _, record := range records {
		if record.CharacterID == key {
			return repository.CloneInventory(record), true
		}
	}
	return repository.InventoryRecord{}, false
}

func findEquipmentStateRecord(records []repository.EquipmentRecord, key string) (repository.EquipmentRecord, bool) {
	for _, record := range records {
		if record.CharacterID == key {
			return repository.CloneEquipment(record), true
		}
	}
	return repository.EquipmentRecord{}, false
}

func findPetStateRecord(records []repository.PetRecord, key string) (repository.PetRecord, bool) {
	for _, record := range records {
		if record.CharacterID == key {
			return repository.ClonePet(record), true
		}
	}
	return repository.PetRecord{}, false
}

func removeAccountStateRecord(records []repository.AccountRecord, key string) []repository.AccountRecord {
	out := records[:0]
	for _, record := range records {
		if record.AccountID != key {
			out = append(out, record)
		}
	}
	return out
}

func removeCharacterStateRecord(records []repository.CharacterRecord, key string) []repository.CharacterRecord {
	out := records[:0]
	for _, record := range records {
		if record.CharacterID != key {
			out = append(out, record)
		}
	}
	return out
}

func removeInventoryStateRecord(records []repository.InventoryRecord, key string) []repository.InventoryRecord {
	out := records[:0]
	for _, record := range records {
		if record.CharacterID != key {
			out = append(out, record)
		}
	}
	return out
}

func removeEquipmentStateRecord(records []repository.EquipmentRecord, key string) []repository.EquipmentRecord {
	out := records[:0]
	for _, record := range records {
		if record.CharacterID != key {
			out = append(out, record)
		}
	}
	return out
}

func removePetStateRecord(records []repository.PetRecord, key string) []repository.PetRecord {
	out := records[:0]
	for _, record := range records {
		if record.CharacterID != key {
			out = append(out, record)
		}
	}
	return out
}
