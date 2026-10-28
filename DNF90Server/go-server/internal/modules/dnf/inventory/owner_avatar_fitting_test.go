package inventory

import (
	"context"
	"testing"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

func TestAvatarFittingRoomTicketOpensWithoutPrematureConsumption(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{CharacterID: "77", AccountID: "acc", Job: "0", Level: 30}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{CharacterID: "77", Slots: map[string]dnfrepo.ItemStack{
		"0:10": {
			ItemID: 490702492, Count: 8,
			Extra: map[string]string{"pvf_path": "stackable/etc/avatar_fitting_room_ticket.stk", "stackable_type": "[etc]"},
		},
	}}); err != nil {
		t.Fatal(err)
	}
	owner, err := NewOwner(repositories)
	if err != nil {
		t.Fatal(err)
	}
	command := NewUseStackableCommand(alignedcmd.Request{AccountID: "acc", SelectedCharacterID: 77}, UseStackableRequest{
		SlotIndex: 10, ListType: listTypeMain, InstanceValue: 0x11223344, ItemCode: 490702492,
	})
	resolver := func(itemID int64) (alignedcmd.StackableUseResolution, error) {
		return alignedcmd.StackableUseResolution{
			ItemID: itemID, PVFPath: "stackable/etc/avatar_fitting_room_ticket.stk", StackableType: "[etc]",
			AvatarFittingRoomTicket: &alignedcmd.AvatarFittingRoomTicketResolution{MinimumLevel: 30, VillageAllowed: true},
		}, nil
	}
	result, err := owner.UseStackableWithResolvers(ctx, command, nil, nil, resolver)
	if err != nil {
		t.Fatal(err)
	}
	if !result.AvatarFittingRoomOpened || result.Changed || result.RemainingCount != 8 {
		t.Fatalf("result=%+v", result)
	}
	loaded, _, _ := repositories.Inventory.Load(ctx, "77")
	if loaded.Slots["0:10"].Count != 8 {
		t.Fatalf("ticket was consumed by op44: %+v", loaded.Slots["0:10"])
	}
	wire := ownerAppliedUseStackableResult(44, command, result)
	if len(wire.UpperResponses) != 1 || len(wire.ItemSlotRefreshes) != 0 {
		t.Fatalf("wire=%+v", wire)
	}
}
