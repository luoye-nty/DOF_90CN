package vending

import (
	"context"
	"errors"
	"testing"
	"time"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

func TestOwnerUseDebitsExactClientMaterialSlotAndGrantsRewardAtomically(t *testing.T) {
	ctx := context.Background()
	repositories := vendingOwnerTestRepositories(t, ctx, map[string]dnfrepo.ItemStack{
		"0:140": {ItemID: 2749933, Count: 9},
		"0:142": {ItemID: 2749933, Count: 2, RawEntry: make([]byte, 61)},
	})
	owner, err := NewOwner(repositories)
	if err != nil {
		t.Fatal(err)
	}
	updatedAt := time.Unix(1700000000, 0).UTC()
	result, err := owner.Use(ctx, Command{
		AccountID:      "dnf:1",
		CharacterID:    "77",
		CompletionKey:  "vending/session-1/request-1",
		MaterialSlot:   142,
		MaterialItemID: 2749933,
		MaterialCount:  1,
		RewardItemID:   2749957,
		RewardCount:    2,
		UpdatedAt:      updatedAt,
		Project: func(candidate *dnfrepo.InventoryRecord, itemID, count uint32) ([]int16, error) {
			candidate.Slots["0:150"] = dnfrepo.ItemStack{ItemID: int64(itemID), Count: int64(count)}
			return []int16{150}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.MaterialSlot != 142 || result.MaterialItemID != 2749933 || result.MaterialCount != 1 ||
		result.MaterialRemaining != 1 || result.MaterialRemoved || result.RewardItemID != 2749957 ||
		result.RewardCount != 2 || len(result.TouchedSlots) != 1 || result.TouchedSlots[0] != 150 ||
		len(result.RewardSnapshots) != 1 || result.RewardSnapshots[0].Slot != 150 ||
		result.RewardSnapshots[0].Stack.ItemID != 2749957 || result.RewardSnapshots[0].Stack.Count != 2 || result.Replayed {
		t.Fatalf("result=%+v", result)
	}
	result.RewardSnapshots[0].Stack.Count = 99
	inventory := loadVendingInventory(t, ctx, repositories)
	if inventory.Slots["0:140"].Count != 9 || inventory.Slots["0:142"].Count != 1 ||
		inventory.Slots["0:150"].ItemID != 2749957 || inventory.Slots["0:150"].Count != 2 {
		t.Fatalf("inventory=%+v", inventory.Slots)
	}
	if got := inventory.Slots["0:142"].Extra["amount_or_count"]; got != "1" {
		t.Fatalf("material count projection=%q", got)
	}
	character, found, err := repositories.Character.Load(ctx, "77")
	if err != nil || !found || character.Stats[receiptPrefix+"version"] != receiptVersion {
		t.Fatalf("character found=%t err=%v stats=%+v", found, err, character.Stats)
	}
}

func TestOwnerUseRemovesDepletedMaterialSlot(t *testing.T) {
	ctx := context.Background()
	repositories := vendingOwnerTestRepositories(t, ctx, map[string]dnfrepo.ItemStack{
		"0:140": {ItemID: 7454, Count: 1},
	})
	owner, _ := NewOwner(repositories)
	result, err := owner.Use(ctx, vendingCommand(func(candidate *dnfrepo.InventoryRecord, itemID, count uint32) ([]int16, error) {
		candidate.Slots["0:151"] = dnfrepo.ItemStack{ItemID: int64(itemID), Count: int64(count)}
		return []int16{151}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	if !result.MaterialRemoved || result.MaterialRemaining != 0 {
		t.Fatalf("result=%+v", result)
	}
	if inventory := loadVendingInventory(t, ctx, repositories); inventory.Slots["0:140"].ItemID != 0 {
		t.Fatalf("depleted material remains: %+v", inventory.Slots)
	}
}

func TestOwnerUseRollsBackMissingMaterialCapacityAndProjectionErrors(t *testing.T) {
	wantProjectionErr := errors.New("raw reward projection failed")
	tests := []struct {
		name       string
		material   dnfrepo.ItemStack
		project    Projector
		want       error
		wantCalled bool
	}{
		{
			name:     "missing material",
			material: dnfrepo.ItemStack{ItemID: 7454, Count: 0},
			project: func(*dnfrepo.InventoryRecord, uint32, uint32) ([]int16, error) {
				t.Fatal("projector called without material")
				return nil, nil
			},
			want: ErrMaterialMissing,
		},
		{
			name:     "capacity",
			material: dnfrepo.ItemStack{ItemID: 7454, Count: 2},
			project: func(candidate *dnfrepo.InventoryRecord, itemID, count uint32) ([]int16, error) {
				candidate.Slots["0:151"] = dnfrepo.ItemStack{ItemID: int64(itemID), Count: int64(count)}
				return nil, ErrInventoryFull
			},
			want: ErrInventoryFull,
		},
		{
			name:     "projection",
			material: dnfrepo.ItemStack{ItemID: 7454, Count: 2},
			project: func(candidate *dnfrepo.InventoryRecord, itemID, count uint32) ([]int16, error) {
				candidate.Slots["0:151"] = dnfrepo.ItemStack{ItemID: int64(itemID), Count: int64(count)}
				return nil, wantProjectionErr
			},
			want: wantProjectionErr,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			repositories := vendingOwnerTestRepositories(t, ctx, map[string]dnfrepo.ItemStack{"0:140": test.material})
			owner, _ := NewOwner(repositories)
			_, err := owner.Use(ctx, vendingCommand(test.project))
			if !errors.Is(err, test.want) {
				t.Fatalf("error=%v want=%v", err, test.want)
			}
			inventory := loadVendingInventory(t, ctx, repositories)
			if inventory.Slots["0:140"].ItemID != test.material.ItemID ||
				inventory.Slots["0:140"].Count != test.material.Count {
				t.Fatalf("material changed after failure: before=%+v after=%+v", test.material, inventory.Slots["0:140"])
			}
			if _, found := inventory.Slots["0:151"]; found {
				t.Fatalf("reward leaked after failure: %+v", inventory.Slots)
			}
			character, _, err := repositories.Character.Load(ctx, "77")
			if err != nil || character.Stats[receiptPrefix+"version"] != 0 {
				t.Fatalf("receipt leaked after failure: stats=%+v err=%v", character.Stats, err)
			}
		})
	}
}

func TestOwnerUseRejectsWrongAccountAndWrongExplicitSlot(t *testing.T) {
	ctx := context.Background()
	repositories := vendingOwnerTestRepositories(t, ctx, map[string]dnfrepo.ItemStack{
		"0:140": {ItemID: 7454, Count: 2},
		"0:142": {ItemID: 7454, Count: 2},
	})
	owner, _ := NewOwner(repositories)
	command := vendingCommand(func(candidate *dnfrepo.InventoryRecord, itemID, count uint32) ([]int16, error) {
		candidate.Slots["0:151"] = dnfrepo.ItemStack{ItemID: int64(itemID), Count: int64(count)}
		return []int16{151}, nil
	})
	command.AccountID = "dnf:2"
	if _, err := owner.Use(ctx, command); !errors.Is(err, ErrAccountMismatch) {
		t.Fatalf("account error=%v", err)
	}
	command.AccountID = "dnf:1"
	command.MaterialSlot = 141
	if _, err := owner.Use(ctx, command); !errors.Is(err, ErrMaterialMissing) {
		t.Fatalf("slot error=%v", err)
	}
	inventory := loadVendingInventory(t, ctx, repositories)
	if inventory.Slots["0:140"].Count != 2 || inventory.Slots["0:142"].Count != 2 {
		t.Fatalf("wrong slot fallback consumed another stack: %+v", inventory.Slots)
	}
}

func TestOwnerUseReceiptReplaysCommitThenWrapperErrorExactlyOnce(t *testing.T) {
	ctx := context.Background()
	repositories := vendingOwnerTestRepositories(t, ctx, map[string]dnfrepo.ItemStack{
		"0:140": {ItemID: 7454, Count: 2},
	})
	wantErr := errors.New("transaction committed but response was lost")
	indeterminate := &commitThenErrorCharacterAssets{
		base:     repositories.CharacterAssets,
		failOnce: true,
		err:      wantErr,
	}
	repositories.CharacterAssets = indeterminate
	owner, err := NewOwner(repositories)
	if err != nil {
		t.Fatal(err)
	}
	projectCalls := 0
	command := vendingCommand(func(candidate *dnfrepo.InventoryRecord, itemID, count uint32) ([]int16, error) {
		projectCalls++
		candidate.Slots["0:151"] = dnfrepo.ItemStack{ItemID: int64(itemID), Count: int64(count)}
		return []int16{151}, nil
	})
	command.CompletionKey = "vending/session-77/request-0001"
	if _, err := owner.Use(ctx, command); !errors.Is(err, wantErr) {
		t.Fatalf("first error=%v", err)
	}
	inventory := loadVendingInventory(t, ctx, repositories)
	if inventory.Slots["0:140"].Count != 1 || inventory.Slots["0:151"].Count != 1 {
		t.Fatalf("inner commit did not persist exactly once: %+v", inventory.Slots)
	}

	replayed, err := owner.Use(ctx, command)
	if err != nil {
		t.Fatal(err)
	}
	if !replayed.Replayed || replayed.MaterialSlot != 140 || replayed.MaterialRemaining != 1 ||
		replayed.MaterialRemoved || replayed.RewardItemID != 31 || replayed.RewardCount != 1 ||
		len(replayed.TouchedSlots) != 1 || replayed.TouchedSlots[0] != 151 ||
		len(replayed.RewardSnapshots) != 1 || replayed.RewardSnapshots[0].Slot != 151 ||
		replayed.RewardSnapshots[0].Stack.ItemID != 31 || replayed.RewardSnapshots[0].Stack.Count != 1 {
		t.Fatalf("replayed=%+v", replayed)
	}
	inventory = loadVendingInventory(t, ctx, repositories)
	if inventory.Slots["0:140"].Count != 1 || inventory.Slots["0:151"].Count != 1 ||
		projectCalls != 1 || indeterminate.calls != 2 {
		t.Fatalf("replay duplicated mutation: slots=%+v projectCalls=%d txCalls=%d", inventory.Slots, projectCalls, indeterminate.calls)
	}
}

func TestOwnerUseSameReceiptKeyRejectsChangedRewardAuthority(t *testing.T) {
	ctx := context.Background()
	repositories := vendingOwnerTestRepositories(t, ctx, map[string]dnfrepo.ItemStack{
		"0:140": {ItemID: 7454, Count: 2},
	})
	owner, _ := NewOwner(repositories)
	command := vendingCommand(func(candidate *dnfrepo.InventoryRecord, itemID, count uint32) ([]int16, error) {
		candidate.Slots["0:151"] = dnfrepo.ItemStack{ItemID: int64(itemID), Count: int64(count)}
		return []int16{151}, nil
	})
	command.CompletionKey = "stable-key"
	if _, err := owner.Use(ctx, command); err != nil {
		t.Fatal(err)
	}
	command.RewardItemID = 44
	if _, err := owner.Use(ctx, command); !errors.Is(err, ErrReceiptInvalid) {
		t.Fatalf("changed authority replay error=%v", err)
	}
	inventory := loadVendingInventory(t, ctx, repositories)
	if inventory.Slots["0:140"].Count != 1 || inventory.Slots["0:151"].ItemID != 31 {
		t.Fatalf("changed replay mutated inventory: %+v", inventory.Slots)
	}
}

func vendingCommand(project Projector) Command {
	return Command{
		AccountID:      "dnf:1",
		CharacterID:    "77",
		CompletionKey:  "vending/test-request",
		MaterialSlot:   140,
		MaterialItemID: 7454,
		MaterialCount:  1,
		RewardItemID:   31,
		RewardCount:    1,
		Project:        project,
	}
}

func vendingOwnerTestRepositories(
	t *testing.T,
	ctx context.Context,
	slots map[string]dnfrepo.ItemStack,
) dnfrepo.Group {
	t.Helper()
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "77",
		AccountID:   "dnf:1",
		Stats:       map[string]int64{"gold": 100},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "77",
		Slots:       slots,
	}); err != nil {
		t.Fatal(err)
	}
	return repositories
}

func loadVendingInventory(t *testing.T, ctx context.Context, repositories dnfrepo.Group) dnfrepo.InventoryRecord {
	t.Helper()
	inventory, found, err := repositories.Inventory.Load(ctx, "77")
	if err != nil || !found {
		t.Fatalf("load inventory found=%t err=%v", found, err)
	}
	return inventory
}

type commitThenErrorCharacterAssets struct {
	base     dnfrepo.CharacterAssetUnitOfWork
	failOnce bool
	err      error
	calls    int
}

func (unit *commitThenErrorCharacterAssets) WithinCharacterAssets(
	ctx context.Context,
	characterID string,
	apply func(dnfrepo.CharacterRepository, dnfrepo.InventoryRepository, dnfrepo.EquipmentRepository) error,
) error {
	unit.calls++
	err := unit.base.WithinCharacterAssets(ctx, characterID, apply)
	if err == nil && unit.failOnce {
		unit.failOnce = false
		return unit.err
	}
	return err
}

var _ dnfrepo.CharacterAssetUnitOfWork = (*commitThenErrorCharacterAssets)(nil)
