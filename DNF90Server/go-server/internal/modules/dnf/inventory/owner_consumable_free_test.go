package inventory

import (
	"context"
	"testing"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

// 修炼道场 (5000) lets you use consumables without spending them. The dungeon
// side asks for that by setting alignedcmd.Request.ConsumableFree, which the
// command carries into the owner; the stack must stay untouched and the ack must
// report the unchanged count so the client's cell keeps its number.
func TestUseStackableConsumableFreeKeepsTheStackAndReportsUnchangedCount(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "77", AccountID: "acc", Job: "0", Level: 30,
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "77",
		Slots: map[string]dnfrepo.ItemStack{
			"0:10": {
				ItemID: 1234, Count: 5,
				Extra: map[string]string{
					"pvf_path":       "stackable/waste/hp_potion.stk",
					"stackable_type": "[waste]",
				},
			},
		},
	}); err != nil {
		t.Fatal(err)
	}
	owner, err := NewOwner(repositories)
	if err != nil {
		t.Fatal(err)
	}
	request := alignedcmd.Request{
		AccountID:           "acc",
		SelectedCharacterID: 77,
		InDungeon:           true,
		DungeonID:           5000,
		ConsumableFree:      true,
	}
	command := NewUseStackableCommand(request, UseStackableRequest{
		SlotIndex: 10, ListType: listTypeMain, InstanceValue: 0x11223344, ItemCode: 1234,
	})
	if !command.ConsumableFree {
		t.Fatalf("command dropped the free flag: %+v", command)
	}
	resolver := func(itemID int64) (alignedcmd.StackableUseResolution, error) {
		return alignedcmd.StackableUseResolution{
			ItemID: itemID, PVFPath: "stackable/waste/hp_potion.stk", StackableType: "[waste]",
		}, nil
	}
	result, err := owner.UseStackableWithResolvers(ctx, command, nil, nil, resolver)
	if err != nil {
		t.Fatalf("free use rejected: %v", err)
	}
	if result.RemainingCount != 5 {
		t.Fatalf("remaining count = %d want the untouched 5", result.RemainingCount)
	}
	loaded, _, err := repositories.Inventory.Load(ctx, "77")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Slots["0:10"].Count != 5 {
		t.Fatalf("training-room consumable was consumed: %+v", loaded.Slots["0:10"])
	}

	// The ordinary path must still consume exactly one.
	command.ConsumableFree = false
	result, err = owner.UseStackableWithResolvers(ctx, command, nil, nil, resolver)
	if err != nil {
		t.Fatalf("ordinary use rejected: %v", err)
	}
	if result.RemainingCount != 4 {
		t.Fatalf("ordinary remaining count = %d want 4", result.RemainingCount)
	}
	loaded, _, err = repositories.Inventory.Load(ctx, "77")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Slots["0:10"].Count != 4 {
		t.Fatalf("ordinary use did not consume: %+v", loaded.Slots["0:10"])
	}
}
