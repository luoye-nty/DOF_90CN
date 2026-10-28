package avatarcompound

import (
	"context"
	"errors"
	"testing"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

func TestOwnerCommitsCompoundInventoryAtomically(t *testing.T) {
	ctx := context.Background()
	repositories := compoundOwnerRepositories(t, "dnf:1")
	owner, err := NewOwner(repositories)
	if err != nil {
		t.Fatal(err)
	}
	err = owner.Compound(ctx, Command{AccountID: "dnf:1", CharacterID: "77", Project: func(assets *Assets) error {
		delete(assets.Inventory.Slots, "1:4")
		assets.Inventory.Slots["1:4"] = dnfrepo.ItemStack{ItemID: 9002, Count: 1}
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	inventory, found, err := repositories.Inventory.Load(ctx, "77")
	if err != nil || !found || inventory.Slots["1:4"].ItemID != 9002 {
		t.Fatalf("inventory found=%t err=%v slots=%+v", found, err, inventory.Slots)
	}
}

func TestOwnerRollsBackCompoundFailure(t *testing.T) {
	ctx := context.Background()
	repositories := compoundOwnerRepositories(t, "dnf:1")
	owner, err := NewOwner(repositories)
	if err != nil {
		t.Fatal(err)
	}
	wantErr := errors.New("projection failed")
	err = owner.Compound(ctx, Command{AccountID: "dnf:1", CharacterID: "77", Project: func(assets *Assets) error {
		delete(assets.Inventory.Slots, "1:4")
		return wantErr
	}})
	if !errors.Is(err, wantErr) {
		t.Fatalf("error=%v want=%v", err, wantErr)
	}
	inventory, found, err := repositories.Inventory.Load(ctx, "77")
	if err != nil || !found || inventory.Slots["1:4"].ItemID != 9001 {
		t.Fatalf("rollback found=%t err=%v slots=%+v", found, err, inventory.Slots)
	}
}

func TestOwnerRejectsCrossAccountCompound(t *testing.T) {
	owner, err := NewOwner(compoundOwnerRepositories(t, "dnf:2"))
	if err != nil {
		t.Fatal(err)
	}
	err = owner.Compound(context.Background(), Command{AccountID: "dnf:1", CharacterID: "77", Project: func(*Assets) error { return nil }})
	if !errors.Is(err, ErrAccountMismatch) {
		t.Fatalf("error=%v want=%v", err, ErrAccountMismatch)
	}
}

func compoundOwnerRepositories(t *testing.T, accountID string) dnfrepo.Group {
	t.Helper()
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{CharacterID: "77", AccountID: accountID, Job: "0"}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{CharacterID: "77", Slots: map[string]dnfrepo.ItemStack{
		"1:4": {ItemID: 9001, Count: 1},
	}}); err != nil {
		t.Fatal(err)
	}
	return repositories
}
