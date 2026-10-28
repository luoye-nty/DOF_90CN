package memory

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"longheng.io/server/internal/modules/dnf/repository"
)

func TestMemoryEpicBookCraftRollsBackCallbackMutation(t *testing.T) {
	repositories := NewMemoryGroup()
	ctx := context.Background()
	wantBook := repository.EpicBookRecord{AccountID: "account-1", Balances: map[uint32]uint32{101020256: 1000}}
	wantAccountInventory := repository.AccountInventoryRecord{AccountID: "account-1", Slots: map[string]repository.ItemStack{"0:364": {ItemID: 10099775, Count: 44}}}
	wantInventory := repository.InventoryRecord{CharacterID: "19", Slots: map[string]repository.ItemStack{"0:65": {ItemID: 700, Count: 1}}}
	wantEquipment := repository.EquipmentRecord{CharacterID: "19", Entries: map[string]repository.EquipmentEntry{"weapon": {ItemID: 1001}}}
	if err := repositories.EpicBook.Save(ctx, wantBook); err != nil {
		t.Fatal(err)
	}
	if err := repositories.AccountInventory.Save(ctx, wantAccountInventory); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Inventory.Save(ctx, wantInventory); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Equipment.Save(ctx, wantEquipment); err != nil {
		t.Fatal(err)
	}
	wantErr := errors.New("abort epic book craft")
	err := repositories.EpicBookCraft.WithinEpicBookCraft(ctx, "account-1", "19", func(
		epicBooks repository.EpicBookRepository,
		accountInventories repository.AccountInventoryRepository,
		inventories repository.InventoryRepository,
		equipment repository.EquipmentRepository,
	) error {
		book, _, _ := epicBooks.Load(ctx, "account-1")
		book.Balances[101020256] = 0
		if saveErr := epicBooks.Save(ctx, book); saveErr != nil {
			return saveErr
		}
		accountInventory, _, _ := accountInventories.Load(ctx, "account-1")
		delete(accountInventory.Slots, "0:364")
		if saveErr := accountInventories.Save(ctx, accountInventory); saveErr != nil {
			return saveErr
		}
		inventory, _, _ := inventories.Load(ctx, "19")
		delete(inventory.Slots, "0:65")
		if saveErr := inventories.Save(ctx, inventory); saveErr != nil {
			return saveErr
		}
		worn, _, _ := equipment.Load(ctx, "19")
		delete(worn.Entries, "weapon")
		if saveErr := equipment.Save(ctx, worn); saveErr != nil {
			return saveErr
		}
		return wantErr
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("WithinEpicBookCraft() error=%v", err)
	}
	gotBook, _, _ := repositories.EpicBook.Load(ctx, "account-1")
	gotAccountInventory, _, _ := repositories.AccountInventory.Load(ctx, "account-1")
	gotInventory, _, _ := repositories.Inventory.Load(ctx, "19")
	gotEquipment, _, _ := repositories.Equipment.Load(ctx, "19")
	if !reflect.DeepEqual(gotBook, wantBook) || !reflect.DeepEqual(gotAccountInventory, wantAccountInventory) || !reflect.DeepEqual(gotInventory, wantInventory) || !reflect.DeepEqual(gotEquipment, wantEquipment) {
		t.Fatalf("mutation escaped rollback book=%+v account_inventory=%+v inventory=%+v equipment=%+v", gotBook, gotAccountInventory, gotInventory, gotEquipment)
	}
}
