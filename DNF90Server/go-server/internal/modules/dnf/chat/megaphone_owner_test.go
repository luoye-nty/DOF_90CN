package chat

import (
	"context"
	"errors"
	"testing"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	"longheng.io/server/internal/modules/dnf/repository/memory"
)

func TestConsumeServerMegaphoneUsesExactMainInventorySlot(t *testing.T) {
	repositories := memory.NewMemoryGroup()
	ctx := context.Background()
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{CharacterID: "77", AccountID: "account"}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "77",
		Slots: map[string]dnfrepo.ItemStack{
			"0:7": {ItemID: int64(ServerMegaphoneItemID), Count: 2},
			"0:8": {ItemID: int64(ServerMegaphoneItemID), Count: 3},
		},
	}); err != nil {
		t.Fatal(err)
	}
	owner, err := NewOwner(repositories)
	if err != nil {
		t.Fatal(err)
	}
	result, err := owner.ConsumeServerMegaphone(ctx, Command{AccountID: "account", CharacterID: "77", Slot: 7})
	if err != nil {
		t.Fatal(err)
	}
	if result.Slot != 7 || result.Remaining != 1 || result.Removed {
		t.Fatalf("result=%+v", result)
	}
	stored, found, err := repositories.Inventory.Load(ctx, "77")
	if err != nil || !found {
		t.Fatalf("load inventory found=%t err=%v", found, err)
	}
	if got := stored.Slots["0:7"].Count; got != 1 {
		t.Fatalf("slot7 count=%d want=1", got)
	}
	if got := stored.Slots["0:8"].Count; got != 3 {
		t.Fatalf("slot8 count=%d want=3", got)
	}
}

func TestConsumeServerMegaphoneRejectsWrongOwnerAndMissingExactSlot(t *testing.T) {
	repositories := memory.NewMemoryGroup()
	ctx := context.Background()
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{CharacterID: "77", AccountID: "account"}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "77",
		Slots:       map[string]dnfrepo.ItemStack{"0:8": {ItemID: int64(ServerMegaphoneItemID), Count: 1}},
	}); err != nil {
		t.Fatal(err)
	}
	owner, _ := NewOwner(repositories)
	if _, err := owner.ConsumeServerMegaphone(ctx, Command{AccountID: "other", CharacterID: "77", Slot: 8}); !errors.Is(err, ErrAccountMismatch) {
		t.Fatalf("wrong-account error=%v", err)
	}
	if _, err := owner.ConsumeServerMegaphone(ctx, Command{AccountID: "account", CharacterID: "77", Slot: 7}); !errors.Is(err, ErrItemMissing) {
		t.Fatalf("wrong-slot error=%v", err)
	}
	stored, _, _ := repositories.Inventory.Load(ctx, "77")
	if got := stored.Slots["0:8"].Count; got != 1 {
		t.Fatalf("slot8 changed after rejection: %d", got)
	}
}
