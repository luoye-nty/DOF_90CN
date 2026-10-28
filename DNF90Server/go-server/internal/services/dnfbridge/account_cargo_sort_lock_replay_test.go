package dnfbridge

import (
	"context"
	"testing"

	dnfitemlock "longheng.io/server/internal/modules/dnf/itemlock"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

// The list-12 rebuild destroys the vault's item rows and their client-side
// padlocks. The replay projection therefore has to include the account cargo's
// locked slots next to the character containers' ones.
func TestSortLockReplayPositionsIncludeAccountCargo(t *testing.T) {
	repositories := testRepositoryGroup()
	ctx := context.Background()
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "19",
		Slots: map[string]dnfrepo.ItemStack{
			"0:64": {ItemID: 600, Count: 1, Extra: map[string]string{"item_sort_lock_state": "1"}},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.AccountInventory.Save(ctx, dnfrepo.AccountInventoryRecord{
		AccountID: "account-1",
		Slots: map[string]dnfrepo.ItemStack{
			"12:5": {ItemID: 300, Count: 1, Extra: map[string]string{"item_sort_lock_state": "1"}},
			"12:2": {ItemID: 301, Count: 1},
		},
	}); err != nil {
		t.Fatal(err)
	}
	service := &Service{
		options:            options{accountID: "account-1"},
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
	}
	session := &gameSession{selectedCharacterID: 19}
	positions, ok := service.buildCurrentSortLockPositionsForSession(ctx, session)
	if !ok {
		t.Fatal("sort lock positions unavailable")
	}
	want := []dnfitemlock.SortLockPosition{{ListType: 0, SlotIndex: 64}, {ListType: 12, SlotIndex: 5}}
	if len(positions) != len(want) {
		t.Fatalf("positions = %+v want %+v", positions, want)
	}
	for index := range want {
		if positions[index] != want[index] {
			t.Fatalf("positions[%d] = %+v want %+v", index, positions[index], want[index])
		}
	}
}
