package sqlite

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	"longheng.io/server/internal/modules/dnf/repository"
)

func TestCharacterSettlementAtomicallyPersistsQuestAndInventory(t *testing.T) {
	ctx := context.Background()
	component, database := startTestComponent(t, filepath.Join(t.TempDir(), "dnf90.db"))
	defer database.Close()
	defer component.Stop(ctx)
	group, _ := component.Group()
	if err := group.Character.Save(ctx, repository.CharacterRecord{CharacterID: "1001", AccountID: "account-1"}); err != nil {
		t.Fatal(err)
	}
	if err := group.Inventory.Save(ctx, repository.InventoryRecord{CharacterID: "1001", Slots: map[string]repository.ItemStack{}}); err != nil {
		t.Fatal(err)
	}
	if err := group.CharacterSettlement.WithinCharacterSettlement(ctx, "1001", func(tx repository.Group) error {
		inventory, found, err := tx.Inventory.Load(ctx, "1001")
		if err != nil || !found {
			return fmt.Errorf("load inventory found=%t: %w", found, err)
		}
		if inventory.Slots == nil {
			inventory.Slots = make(map[string]repository.ItemStack)
		}
		inventory.Slots["0:177"] = repository.ItemStack{ItemID: 1001, Count: 1}
		if err := repository.SaveInventoryFields(ctx, tx.Inventory, inventory, repository.InventoryFieldSlots); err != nil {
			return err
		}
		return repository.SaveQuestFields(ctx, tx.Quest, repository.QuestRecord{
			CharacterID: "1001",
			States:      map[int64]repository.QuestState{1680: {Status: "active", ProgressValue: 1}},
		}, repository.QuestFieldStates)
	}); err != nil {
		t.Fatal(err)
	}
	persisted, err := loadState(ctx, database)
	if err != nil {
		t.Fatal(err)
	}
	if len(persisted.Inventories) != 1 || persisted.Inventories[0].Slots["0:177"].ItemID != 1001 ||
		len(persisted.Quests) != 1 || persisted.Quests[0].States[1680].Status != "active" {
		t.Fatalf("SQLite atomic quest/inventory state inventories=%+v quests=%+v", persisted.Inventories, persisted.Quests)
	}

	forced := errors.New("forced quest accept rollback")
	err = group.CharacterSettlement.WithinCharacterSettlement(ctx, "1001", func(tx repository.Group) error {
		inventory, _, err := tx.Inventory.Load(ctx, "1001")
		if err != nil {
			return err
		}
		stack := inventory.Slots["0:177"]
		stack.Count = 2
		inventory.Slots["0:177"] = stack
		if err := repository.SaveInventoryFields(ctx, tx.Inventory, inventory, repository.InventoryFieldSlots); err != nil {
			return err
		}
		return forced
	})
	if !errors.Is(err, forced) {
		t.Fatalf("SQLite rollback error=%v", err)
	}
	inventory, found, err := group.Inventory.Load(ctx, "1001")
	if err != nil || !found || inventory.Slots["0:177"].Count != 1 {
		t.Fatalf("SQLite rollback inventory=%+v found=%t err=%v", inventory, found, err)
	}
}

func TestCharacterItemsScopedPersistsOnlyOwnedRecords(t *testing.T) {
	ctx := context.Background()
	component, database := startTestComponent(t, filepath.Join(t.TempDir(), "dnf90.db"))
	defer database.Close()
	defer component.Stop(ctx)
	group, _ := component.Group()

	if err := group.Inventory.Save(ctx, repository.InventoryRecord{
		CharacterID: "1001",
		Slots:       map[string]repository.ItemStack{"0:10": {ItemID: 8474, Count: 2}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := group.Quest.Save(ctx, repository.QuestRecord{
		CharacterID: "unrelated",
		States:      map[int64]repository.QuestState{7: {Status: "active"}},
	}); err != nil {
		t.Fatal(err)
	}

	scopedItems, ok := group.CharacterItems.(repository.CharacterItemScopedUnitOfWork)
	if !ok {
		t.Fatal("SQLite character items do not expose the scoped consumable transaction")
	}
	if err := scopedItems.WithinCharacterItemsScoped(ctx, "1001", func(inventories repository.InventoryRepository, _ repository.EquipmentRepository) error {
		record, found, err := inventories.Load(ctx, "1001")
		if err != nil || !found {
			return fmt.Errorf("load inventory found=%t: %w", found, err)
		}
		stack := record.Slots["0:10"]
		stack.Count--
		record.Slots["0:10"] = stack
		return inventories.Save(ctx, record)
	}); err != nil {
		t.Fatal(err)
	}

	state, err := loadState(ctx, database)
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Inventories) != 1 || state.Inventories[0].Slots["0:10"].Count != 1 {
		t.Fatalf("scoped inventory was not durable: %+v", state.Inventories)
	}
	if len(state.Quests) != 0 {
		t.Fatalf("scoped consumable flushed unrelated quest state: %+v", state.Quests)
	}
	if !component.Snapshot().Dirty {
		t.Fatal("scoped consumable cleared the unrelated pending-write signal")
	}
}

func TestCharacterItemsScopedPersistenceFailureRestoresOnlyItsRuntimeRecords(t *testing.T) {
	ctx := context.Background()
	component, database := startTestComponent(t, filepath.Join(t.TempDir(), "dnf90.db"))
	defer database.Close()
	defer component.Stop(ctx)
	group, _ := component.Group()

	if err := group.Inventory.Save(ctx, repository.InventoryRecord{
		CharacterID: "1001",
		Slots:       map[string]repository.ItemStack{"0:10": {ItemID: 8474, Count: 2}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := group.CharacterItems.WithinCharacterItems(ctx, "1001", func(repository.InventoryRepository, repository.EquipmentRepository) error {
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := database.ExecContext(ctx, `
CREATE TRIGGER reject_scoped_inventory_update
BEFORE UPDATE ON dnf_repository_records
WHEN NEW.record_kind = 'inventory' AND NEW.record_key = '1001'
BEGIN
    SELECT RAISE(ABORT, 'forced scoped inventory failure');
END`); err != nil {
		t.Fatal(err)
	}

	scopedItems := group.CharacterItems.(repository.CharacterItemScopedUnitOfWork)
	err := scopedItems.WithinCharacterItemsScoped(ctx, "1001", func(inventories repository.InventoryRepository, _ repository.EquipmentRepository) error {
		record, found, err := inventories.Load(ctx, "1001")
		if err != nil || !found {
			return fmt.Errorf("load inventory found=%t: %w", found, err)
		}
		stack := record.Slots["0:10"]
		stack.Count = 1
		record.Slots["0:10"] = stack
		return inventories.Save(ctx, record)
	})
	if err == nil {
		t.Fatal("scoped persistence failure was accepted")
	}

	runtimeRecord, found, loadErr := group.Inventory.Load(ctx, "1001")
	if loadErr != nil || !found || runtimeRecord.Slots["0:10"].Count != 2 {
		t.Fatalf("runtime rollback found=%t err=%v record=%+v", found, loadErr, runtimeRecord)
	}
	durable, loadErr := loadState(ctx, database)
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if len(durable.Inventories) != 1 || durable.Inventories[0].Slots["0:10"].Count != 2 {
		t.Fatalf("durable inventory changed after failed scope: %+v", durable.Inventories)
	}
}

func TestRentalAssetsScopedPersistsOnlyOwnedRecords(t *testing.T) {
	ctx := context.Background()
	component, database := startTestComponent(t, filepath.Join(t.TempDir(), "dnf90.db"))
	defer database.Close()
	defer component.Stop(ctx)
	group, _ := component.Group()

	if err := group.Account.Save(ctx, repository.AccountRecord{
		AccountID: "dnf:1",
		Metadata:  map[string]string{"rental_lucky_stars": "10"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := group.Character.Save(ctx, repository.CharacterRecord{
		CharacterID: "1001",
		AccountID:   "dnf:1",
		Stats:       map[string]int64{"gold": 100},
	}); err != nil {
		t.Fatal(err)
	}
	if err := group.Inventory.Save(ctx, repository.InventoryRecord{
		CharacterID: "1001",
		Slots:       map[string]repository.ItemStack{"0:0": {ItemID: 700, Count: 1}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := group.Equipment.Save(ctx, repository.EquipmentRecord{
		CharacterID: "1001",
		Entries:     map[string]repository.EquipmentEntry{},
	}); err != nil {
		t.Fatal(err)
	}
	if err := group.Quest.Save(ctx, repository.QuestRecord{
		CharacterID: "unrelated",
		States:      map[int64]repository.QuestState{7: {Status: "active"}},
	}); err != nil {
		t.Fatal(err)
	}

	if _, ok := group.RentalAssets.(repository.RentalAssetScopedUnitOfWork); !ok {
		t.Fatal("SQLite rental assets do not expose the record-scoped transaction")
	}
	if err := repository.WithinRentalAssetsScoped(ctx, group.RentalAssets, "dnf:1", "1001", func(
		accounts repository.AccountRepository,
		characters repository.CharacterRepository,
		inventories repository.InventoryRepository,
		equipment repository.EquipmentRepository,
	) error {
		account, found, err := accounts.Load(ctx, "dnf:1")
		if err != nil || !found {
			return fmt.Errorf("load account found=%t: %w", found, err)
		}
		character, found, err := characters.Load(ctx, "1001")
		if err != nil || !found {
			return fmt.Errorf("load character found=%t: %w", found, err)
		}
		inventory, found, err := inventories.Load(ctx, "1001")
		if err != nil || !found {
			return fmt.Errorf("load inventory found=%t: %w", found, err)
		}
		worn, found, err := equipment.Load(ctx, "1001")
		if err != nil || !found {
			return fmt.Errorf("load equipment found=%t: %w", found, err)
		}
		if worn.Entries == nil {
			worn.Entries = make(map[string]repository.EquipmentEntry)
		}
		account.Metadata["rental_lucky_stars"] = "6"
		character.Stats["gold"] = 80
		inventory.Slots["0:1"] = repository.ItemStack{ItemID: 9001, Count: 1}
		worn.Entries["11"] = repository.EquipmentEntry{SlotIndex: 11, ItemID: 9001}
		if err := accounts.Save(ctx, account); err != nil {
			return err
		}
		if err := repository.SaveCharacterFields(ctx, characters, character, repository.CharacterFieldStats); err != nil {
			return err
		}
		if err := repository.SaveInventoryFields(ctx, inventories, inventory, repository.InventoryFieldSlots); err != nil {
			return err
		}
		return repository.SaveEquipmentFields(ctx, equipment, worn, repository.EquipmentFieldEntries)
	}); err != nil {
		t.Fatal(err)
	}

	durable, err := loadState(ctx, database)
	if err != nil {
		t.Fatal(err)
	}
	if len(durable.Accounts) != 1 || durable.Accounts[0].Metadata["rental_lucky_stars"] != "6" ||
		len(durable.Characters) != 1 || durable.Characters[0].Stats["gold"] != 80 ||
		len(durable.Inventories) != 1 || durable.Inventories[0].Slots["0:1"].ItemID != 9001 ||
		len(durable.Equipment) != 1 || durable.Equipment[0].Entries["11"].ItemID != 9001 {
		t.Fatalf("scoped rental state was not durable: %+v", durable)
	}
	if len(durable.Quests) != 0 {
		t.Fatalf("scoped rental transaction flushed unrelated quest state: %+v", durable.Quests)
	}
	if !component.Snapshot().Dirty {
		t.Fatal("scoped rental transaction cleared the unrelated pending-write signal")
	}
}

func TestRentalAssetsScopedPersistenceFailureRollsBackFourRecords(t *testing.T) {
	ctx := context.Background()
	component, database := startTestComponent(t, filepath.Join(t.TempDir(), "dnf90.db"))
	defer database.Close()
	defer component.Stop(ctx)
	group, _ := component.Group()

	if err := group.Account.Save(ctx, repository.AccountRecord{
		AccountID: "dnf:1",
		Metadata:  map[string]string{"rental_lucky_stars": "10"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := group.Character.Save(ctx, repository.CharacterRecord{
		CharacterID: "1001",
		AccountID:   "dnf:1",
		Stats:       map[string]int64{"gold": 100},
	}); err != nil {
		t.Fatal(err)
	}
	if err := group.Inventory.Save(ctx, repository.InventoryRecord{
		CharacterID: "1001",
		Slots:       map[string]repository.ItemStack{"0:0": {ItemID: 700, Count: 1}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := group.Equipment.Save(ctx, repository.EquipmentRecord{
		CharacterID: "1001",
		Entries:     map[string]repository.EquipmentEntry{},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repository.WithinRentalAssetsScoped(ctx, group.RentalAssets, "dnf:1", "1001", func(
		repository.AccountRepository,
		repository.CharacterRepository,
		repository.InventoryRepository,
		repository.EquipmentRepository,
	) error {
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := database.ExecContext(ctx, `
CREATE TRIGGER reject_scoped_rental_inventory_update
BEFORE UPDATE ON dnf_repository_records
WHEN NEW.record_kind = 'inventory' AND NEW.record_key = '1001'
BEGIN
    SELECT RAISE(ABORT, 'forced scoped rental failure');
END`); err != nil {
		t.Fatal(err)
	}

	err := repository.WithinRentalAssetsScoped(ctx, group.RentalAssets, "dnf:1", "1001", func(
		accounts repository.AccountRepository,
		characters repository.CharacterRepository,
		inventories repository.InventoryRepository,
		equipment repository.EquipmentRepository,
	) error {
		account, _, err := accounts.Load(ctx, "dnf:1")
		if err != nil {
			return err
		}
		character, _, err := characters.Load(ctx, "1001")
		if err != nil {
			return err
		}
		inventory, _, err := inventories.Load(ctx, "1001")
		if err != nil {
			return err
		}
		worn, _, err := equipment.Load(ctx, "1001")
		if err != nil {
			return err
		}
		if worn.Entries == nil {
			worn.Entries = make(map[string]repository.EquipmentEntry)
		}
		account.Metadata["rental_lucky_stars"] = "0"
		character.Stats["gold"] = 0
		inventory.Slots["0:1"] = repository.ItemStack{ItemID: 9001, Count: 1}
		worn.Entries["11"] = repository.EquipmentEntry{SlotIndex: 11, ItemID: 9001}
		if err := accounts.Save(ctx, account); err != nil {
			return err
		}
		if err := characters.Save(ctx, character); err != nil {
			return err
		}
		if err := inventories.Save(ctx, inventory); err != nil {
			return err
		}
		return equipment.Save(ctx, worn)
	})
	if err == nil {
		t.Fatal("scoped rental persistence failure was accepted")
	}

	account, accountFound, accountErr := group.Account.Load(ctx, "dnf:1")
	character, characterFound, characterErr := group.Character.Load(ctx, "1001")
	inventory, inventoryFound, inventoryErr := group.Inventory.Load(ctx, "1001")
	worn, equipmentFound, equipmentErr := group.Equipment.Load(ctx, "1001")
	if accountErr != nil || characterErr != nil || inventoryErr != nil || equipmentErr != nil ||
		!accountFound || !characterFound || !inventoryFound || !equipmentFound ||
		account.Metadata["rental_lucky_stars"] != "10" || character.Stats["gold"] != 100 ||
		len(inventory.Slots) != 1 || inventory.Slots["0:0"].ItemID != 700 || len(worn.Entries) != 0 {
		t.Fatalf("runtime rollback account=%+v character=%+v inventory=%+v equipment=%+v errors=%v/%v/%v/%v",
			account, character, inventory, worn, accountErr, characterErr, inventoryErr, equipmentErr)
	}
	durable, loadErr := loadState(ctx, database)
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if len(durable.Accounts) != 1 || durable.Accounts[0].Metadata["rental_lucky_stars"] != "10" ||
		len(durable.Characters) != 1 || durable.Characters[0].Stats["gold"] != 100 ||
		len(durable.Inventories) != 1 || len(durable.Inventories[0].Slots) != 1 ||
		durable.Inventories[0].Slots["0:0"].ItemID != 700 || len(durable.Equipment) != 1 ||
		len(durable.Equipment[0].Entries) != 0 {
		t.Fatalf("durable rental records changed after failed scope: %+v", durable)
	}
}
