package inventory

import (
	"context"
	"testing"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

func TestCustomAbilityGroupConverterOpensWithoutPrematureConsumption(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{CharacterID: "77", AccountID: "acc", Job: "0", Level: 90}); err != nil {
		t.Fatal(err)
	}
	const pvfPath = "stackable/10163001/10163994.stk"
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{CharacterID: "77", Slots: map[string]dnfrepo.ItemStack{
		"0:10": {
			ItemID: 10163994,
			Count:  3,
			Extra:  map[string]string{"pvf_path": pvfPath, "stackable_type": "[etc]"},
		},
	}}); err != nil {
		t.Fatal(err)
	}
	owner, err := NewOwner(repositories)
	if err != nil {
		t.Fatal(err)
	}
	command := NewUseStackableCommand(alignedcmd.Request{AccountID: "acc", SelectedCharacterID: 77}, UseStackableRequest{
		SlotIndex: 10, ListType: listTypeMain, InstanceValue: 0x11223344, ItemCode: 10163994,
	})
	resolver := func(itemID int64) (alignedcmd.StackableUseResolution, error) {
		return alignedcmd.StackableUseResolution{
			ItemID: itemID, PVFPath: pvfPath, StackableType: "[etc]",
			CustomAbilityGroupConverter: &alignedcmd.CustomAbilityGroupConverterResolution{MinimumLevel: 1},
		}, nil
	}
	result, err := owner.UseStackableWithResolvers(ctx, command, nil, nil, resolver)
	if err != nil {
		t.Fatal(err)
	}
	if !result.CustomAbilityGroupConverterOpened || result.Changed || result.RemainingCount != 3 {
		t.Fatalf("result=%+v", result)
	}
	loaded, _, err := repositories.Inventory.Load(ctx, "77")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Slots["0:10"].Count != 3 {
		t.Fatalf("device was consumed by op44: %+v", loaded.Slots["0:10"])
	}
	wire := ownerAppliedUseStackableResult(44, command, result)
	if len(wire.UpperResponses) != 1 || len(wire.ItemSlotRefreshes) != 0 || wire.CustomAbilityGroupConverter == nil {
		t.Fatalf("wire=%+v", wire)
	}
	if wire.CustomAbilityGroupConverter.CharacterID != 77 || wire.CustomAbilityGroupConverter.SourceSlotIndex != 10 ||
		wire.CustomAbilityGroupConverter.SourceItemID != 10163994 {
		t.Fatalf("projection=%+v", wire.CustomAbilityGroupConverter)
	}
}
