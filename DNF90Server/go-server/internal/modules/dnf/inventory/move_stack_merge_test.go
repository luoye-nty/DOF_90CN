package inventory

import (
	"context"
	"encoding/binary"
	"errors"
	"reflect"
	"strconv"
	"testing"
	"time"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

func TestOwnerMoveAutoStacksIntoLowestTargetThatFitsPVFLimit(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	saveTestInventory(t, ctx, repos, dnfrepo.InventoryRecord{
		CharacterID: "77",
		Slots: map[string]dnfrepo.ItemStack{
			slotKey(listTypeMain, 65): pvfMoveStack(3227, 3, 10),
			slotKey(listTypeMain, 70): pvfMoveStack(3227, 8, 10),
			slotKey(listTypeMain, 72): pvfMoveStack(3227, 4, 10),
			slotKey(listTypeMain, 75): pvfMoveStack(3227, 1, 10),
		},
	})
	owner, err := NewOwner(repos)
	if err != nil {
		t.Fatalf("NewOwner error = %v", err)
	}

	result, err := owner.Move(ctx, NewMoveCommand(alignedcmd.Request{
		AccountID:           "dnf:1",
		SelectedCharacterID: 77,
	}, MoveItemspaceRequest{
		SourceListType:       listTypeMain,
		SourceSlotIndex:      65,
		MoveCount:            3,
		DestinationListType:  listTypeMain,
		DestinationSlotIndex: 80,
	}))
	if err != nil {
		t.Fatalf("Move error = %v", err)
	}
	if result.Mode != "auto_stack" || result.MoveCount != 3 || !result.Changed {
		t.Fatalf("result = %+v", result)
	}
	if !reflect.DeepEqual(result.RefreshListTypes, []byte{listTypeMain}) {
		t.Fatalf("refresh list types = %v", result.RefreshListTypes)
	}

	loaded := loadTestInventory(t, ctx, repos, "77")
	if _, exists := loaded.Slots[slotKey(listTypeMain, 65)]; exists {
		t.Fatalf("source still exists: %+v", loaded.Slots)
	}
	if _, exists := loaded.Slots[slotKey(listTypeMain, 80)]; exists {
		t.Fatalf("literal empty destination was populated instead of merging: %+v", loaded.Slots)
	}
	if got := loaded.Slots[slotKey(listTypeMain, 70)].Count; got != 8 {
		t.Fatalf("full lower slot changed = %d", got)
	}
	if got := loaded.Slots[slotKey(listTypeMain, 72)].Count; got != 7 {
		t.Fatalf("lowest compatible slot count = %d, want 7", got)
	}
	if got := loaded.Slots[slotKey(listTypeMain, 75)].Count; got != 1 {
		t.Fatalf("later compatible slot changed = %d", got)
	}
}

func TestOwnerMoveAndSortKeepUpgradedMonsterCardSeparateFromBaseStack(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	base := testMonsterCardStack(testMonsterCardTarget, 4, 0)
	upgraded := testMonsterCardStack(testMonsterCardTarget, 1, 2)
	saveMonsterCardFixture(t, repos, 10_000, map[string]dnfrepo.ItemStack{
		slotKey(listTypeMain, 235): base,
		slotKey(listTypeMain, 236): upgraded,
	})
	owner, err := NewOwner(repos)
	if err != nil {
		t.Fatal(err)
	}
	owner.SetStackMergeResolver(func(itemID int64) (alignedcmd.StackMergeResolution, error) {
		return alignedcmd.StackMergeResolution{
			ItemID: itemID, Kind: "stackable", PVFPath: "stackable/monstercard/test.stk",
			StackableType: "[material expert job]", StackLimit: 1000,
		}, nil
	})

	move, err := owner.Move(ctx, NewMoveCommand(alignedcmd.Request{
		AccountID: "acc", SelectedCharacterID: 77,
	}, MoveItemspaceRequest{
		SourceListType: listTypeMain, SourceSlotIndex: 236, MoveCount: 1,
		DestinationListType: listTypeMain, DestinationSlotIndex: 240,
	}))
	if err != nil {
		t.Fatal(err)
	}
	if move.Mode != "move" || !move.Changed {
		t.Fatalf("move=%+v, upgraded card must use the requested empty slot instead of auto-stacking into the base card", move)
	}
	afterMove := loadTestInventory(t, ctx, repos, "77")
	if got := afterMove.Slots[slotKey(listTypeMain, 235)]; got.Count != 4 || monsterCardUpgradeCountOf(got) != 0 {
		t.Fatalf("base after move=%+v", got)
	}
	if got := afterMove.Slots[slotKey(listTypeMain, 240)]; got.Count != 1 || monsterCardUpgradeCountOf(got) != 2 || got.RawEntry[0x12] != 2 {
		t.Fatalf("upgraded after move=%+v raw=%x", got, got.RawEntry)
	}

	sorted, err := owner.Sort(ctx, NewSortCommand(alignedcmd.Request{
		AccountID: "acc", SelectedCharacterID: 77,
	}, SortItemRequest{ListType: listTypeMain, Category: 10}))
	if err != nil {
		t.Fatal(err)
	}
	if !sorted.Changed {
		t.Fatalf("sort=%+v", sorted)
	}
	afterSort := loadTestInventory(t, ctx, repos, "77")
	base = afterSort.Slots[slotKey(listTypeMain, 233)]
	upgraded = afterSort.Slots[slotKey(listTypeMain, 234)]
	if base.Count != 4 || monsterCardUpgradeCountOf(base) != 0 || upgraded.Count != 1 || monsterCardUpgradeCountOf(upgraded) != 2 || upgraded.RawEntry[0x12] != 2 {
		t.Fatalf("sorted base=%+v upgraded=%+v upgraded_raw=%x", base, upgraded, upgraded.RawEntry)
	}
}

func TestOwnerMoveAutoStackIntoPersonalCargoSynchronizesDurableCountFields(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	source := pvfMoveStack(3330, 5850, 0)
	source.RawEntry = make([]byte, currentItemListEntrySize)
	binary.LittleEndian.PutUint32(source.RawEntry[0x06:0x0A], 5850)
	source.Extra["amount"] = "5850"
	source.Extra["amount_or_count"] = "5850"
	target := pvfMoveStack(3330, 20470, 0)
	target.RawEntry = make([]byte, currentItemListEntrySize)
	binary.LittleEndian.PutUint32(target.RawEntry[0x06:0x0A], 20470)
	target.Extra["amount"] = "17970"
	target.Extra["amount_or_count"] = "17970"
	saveTestInventory(t, ctx, repos, dnfrepo.InventoryRecord{
		CharacterID: "77",
		Slots: map[string]dnfrepo.ItemStack{
			slotKey(listTypeMain, 7): source,
		},
		Warehouse: map[string]dnfrepo.ItemStack{
			slotKey(listTypePersonalCargo, 0): target,
		},
	})
	owner, err := NewOwner(repos)
	if err != nil {
		t.Fatalf("NewOwner error = %v", err)
	}

	result, err := owner.Move(ctx, NewMoveCommand(alignedcmd.Request{
		AccountID:           "dnf:1",
		SelectedCharacterID: 77,
	}, MoveItemspaceRequest{
		SourceListType:       listTypeMain,
		SourceSlotIndex:      7,
		MoveCount:            5850,
		DestinationListType:  listTypePersonalCargo,
		DestinationSlotIndex: 43,
	}))
	if err != nil {
		t.Fatalf("Move error = %v", err)
	}
	if result.Mode != "stack" || result.MoveCount != 5850 || !result.Changed {
		t.Fatalf("result = %+v", result)
	}
	// 跨容器 auto_stack 必须把真实落点（仓库 2:0）写进 ACK，并且不能再整 list 刷新：
	// 整 list 会把 list-0 的保留槽 0（金币钱包行）一起下发，现场就是"获得了 N 金币"。
	if result.DestinationSlotIndex != 0 || !result.RedirectedReservedMainSlot {
		t.Fatalf("ack destination=%d redirected=%t, want the real merged slot 0",
			result.DestinationSlotIndex, result.RedirectedReservedMainSlot)
	}
	if len(result.RefreshListTypes) != 0 || len(result.Refresh) != 0 {
		t.Fatalf("refresh=%v lists=%v, want none", result.Refresh, result.RefreshListTypes)
	}

	loaded := loadTestInventory(t, ctx, repos, "77")
	if _, exists := loaded.Slots[slotKey(listTypeMain, 7)]; exists {
		t.Fatalf("source still exists: %+v", loaded.Slots)
	}
	if _, exists := loaded.Warehouse[slotKey(listTypePersonalCargo, 43)]; exists {
		t.Fatalf("literal empty destination was populated: %+v", loaded.Warehouse)
	}
	merged := loaded.Warehouse[slotKey(listTypePersonalCargo, 0)]
	if merged.Count != 26320 {
		t.Fatalf("merged count = %d, want 26320", merged.Count)
	}
	if got := binary.LittleEndian.Uint32(merged.RawEntry[0x06:0x0A]); got != 26320 {
		t.Fatalf("merged raw count = %d, want 26320", got)
	}
	if merged.Extra["amount"] != "26320" || merged.Extra["amount_or_count"] != "26320" {
		t.Fatalf("merged count aliases = %+v", merged.Extra)
	}
}

func TestOwnerMoveUsesActivePVFToStackLegacyUnlimitedMaterialIntoPersonalCargo(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	source := dnfrepo.ItemStack{ItemID: 3244, Count: 17, Extra: map[string]string{"source": "old_pickup"}}
	target := dnfrepo.ItemStack{ItemID: 3244, Count: 23, Extra: map[string]string{
		"item_kind": "stackable", "pvf_path": "stackable/obsolete.stk", "source": "old_mail",
	}}
	source.RawEntry = make([]byte, currentItemListEntrySize)
	target.RawEntry = make([]byte, currentItemListEntrySize)
	binary.LittleEndian.PutUint32(source.RawEntry[0x06:0x0a], 17)
	binary.LittleEndian.PutUint32(target.RawEntry[0x06:0x0a], 23)
	saveTestInventory(t, ctx, repos, dnfrepo.InventoryRecord{
		CharacterID: "77",
		Slots:       map[string]dnfrepo.ItemStack{slotKey(listTypeMain, 65): source},
		Warehouse:   map[string]dnfrepo.ItemStack{slotKey(listTypePersonalCargo, 4): target},
	})
	owner, err := NewOwner(repos)
	if err != nil {
		t.Fatal(err)
	}
	owner.SetStackMergeResolver(func(itemID int64) (alignedcmd.StackMergeResolution, error) {
		return alignedcmd.StackMergeResolution{
			ItemID: itemID, Kind: "stackable", PVFPath: "stackable/material/crystal_pure.stk",
			StackableType: "[material]", StackLimit: 0,
		}, nil
	})

	result, err := owner.Move(ctx, NewMoveCommand(alignedcmd.Request{
		AccountID: "dnf:1", SelectedCharacterID: 77,
	}, MoveItemspaceRequest{
		SourceListType: listTypeMain, SourceSlotIndex: 65, MoveCount: 17,
		DestinationListType: listTypePersonalCargo, DestinationSlotIndex: 40,
	}))
	if err != nil {
		t.Fatalf("Move error = %v", err)
	}
	if result.Mode != "stack" || result.MoveCount != 17 || !result.Changed {
		t.Fatalf("result = %+v", result)
	}
	if result.DestinationSlotIndex != 4 || !result.RedirectedReservedMainSlot {
		t.Fatalf("ack destination=%d redirected=%t, want the real merged slot 4",
			result.DestinationSlotIndex, result.RedirectedReservedMainSlot)
	}
	if len(result.RefreshListTypes) != 0 || len(result.Refresh) != 0 {
		t.Fatalf("refresh=%v lists=%v, want none", result.Refresh, result.RefreshListTypes)
	}
	loaded := loadTestInventory(t, ctx, repos, "77")
	if _, exists := loaded.Warehouse[slotKey(listTypePersonalCargo, 40)]; exists {
		t.Fatalf("literal destination was populated: %+v", loaded.Warehouse)
	}
	merged := loaded.Warehouse[slotKey(listTypePersonalCargo, 4)]
	if merged.Count != 40 || binary.LittleEndian.Uint32(merged.RawEntry[0x06:0x0a]) != 40 {
		t.Fatalf("merged stack = %+v raw=%x", merged, merged.RawEntry[:10])
	}
	if merged.Extra["item_kind"] != "stackable" || merged.Extra["pvf_path"] != "stackable/material/crystal_pure.stk" ||
		merged.Extra["stackable_type"] != "[material]" || merged.Extra["stack_limit"] != "" {
		t.Fatalf("canonical PVF metadata = %+v", merged.Extra)
	}
}

func TestOwnerMoveAutoStackIsSymmetricForReverseOccupiedEndpoint(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	saveTestInventory(t, ctx, repos, dnfrepo.InventoryRecord{
		CharacterID: "77",
		Slots: map[string]dnfrepo.ItemStack{
			slotKey(listTypeMain, 65): pvfMoveStack(3227, 2, 10),
		},
		Warehouse: map[string]dnfrepo.ItemStack{
			slotKey(listTypePersonalCargo, 9): pvfMoveStack(3227, 3, 10),
		},
	})
	owner, err := NewOwner(repos)
	if err != nil {
		t.Fatalf("NewOwner error = %v", err)
	}

	result, err := owner.Move(ctx, NewMoveCommand(alignedcmd.Request{
		AccountID:           "dnf:1",
		SelectedCharacterID: 77,
	}, MoveItemspaceRequest{
		SourceListType:       listTypeMain,
		SourceSlotIndex:      80,
		MoveCount:            3,
		DestinationListType:  listTypePersonalCargo,
		DestinationSlotIndex: 9,
	}))
	if err != nil {
		t.Fatalf("Move error = %v", err)
	}
	if result.Mode != "auto_stack_reverse" || result.MoveCount != 3 || !result.Changed {
		t.Fatalf("result = %+v", result)
	}
	if !reflect.DeepEqual(result.RefreshListTypes, []byte{listTypeMain, listTypePersonalCargo}) {
		t.Fatalf("refresh list types = %v", result.RefreshListTypes)
	}

	loaded := loadTestInventory(t, ctx, repos, "77")
	if got := loaded.Slots[slotKey(listTypeMain, 65)].Count; got != 5 {
		t.Fatalf("main target count = %d, want 5", got)
	}
	if _, exists := loaded.Warehouse[slotKey(listTypePersonalCargo, 9)]; exists {
		t.Fatalf("reverse source still exists: %+v", loaded.Warehouse)
	}
	if _, exists := loaded.Slots[slotKey(listTypeMain, 80)]; exists {
		t.Fatalf("literal empty endpoint was populated: %+v", loaded.Slots)
	}
}

func TestOwnerMoveToExplicitEmptyQuickSlotDoesNotAutoStackElsewhere(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	saveTestInventory(t, ctx, repos, dnfrepo.InventoryRecord{
		CharacterID: "77",
		Slots: map[string]dnfrepo.ItemStack{
			slotKey(listTypeMain, 65): pvfMoveStack(3227, 3, 10),
			slotKey(listTypeMain, 66): pvfMoveStack(3227, 1, 10),
		},
	})
	owner, err := NewOwner(repos)
	if err != nil {
		t.Fatalf("NewOwner error = %v", err)
	}

	result, err := owner.Move(ctx, NewMoveCommand(alignedcmd.Request{
		AccountID:           "dnf:1",
		SelectedCharacterID: 77,
	}, MoveItemspaceRequest{
		SourceListType:       listTypeMain,
		SourceSlotIndex:      65,
		MoveCount:            3,
		DestinationListType:  listTypeMain,
		DestinationSlotIndex: 3,
	}))
	if err != nil {
		t.Fatalf("Move error = %v", err)
	}
	if result.Mode != "move" || result.MoveCount != 3 || !result.Changed {
		t.Fatalf("result = %+v", result)
	}

	loaded := loadTestInventory(t, ctx, repos, "77")
	if _, exists := loaded.Slots[slotKey(listTypeMain, 65)]; exists {
		t.Fatalf("source still exists: %+v", loaded.Slots)
	}
	if got := loaded.Slots[slotKey(listTypeMain, 3)]; got.ItemID != 3227 || got.Count != 3 {
		t.Fatalf("explicit quick slot stack = %+v", got)
	}
	if got := loaded.Slots[slotKey(listTypeMain, 66)].Count; got != 1 {
		t.Fatalf("ordinary compatible stack changed = %d", got)
	}
}

func TestOwnerReverseMoveToExplicitEmptyQuickSlotDoesNotAutoStackElsewhere(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	saveTestInventory(t, ctx, repos, dnfrepo.InventoryRecord{
		CharacterID: "77",
		Slots: map[string]dnfrepo.ItemStack{
			slotKey(listTypeMain, 65): pvfMoveStack(3227, 3, 10),
			slotKey(listTypeMain, 66): pvfMoveStack(3227, 1, 10),
		},
	})
	owner, err := NewOwner(repos)
	if err != nil {
		t.Fatalf("NewOwner error = %v", err)
	}

	result, err := owner.Move(ctx, NewMoveCommand(alignedcmd.Request{
		AccountID:           "dnf:1",
		SelectedCharacterID: 77,
	}, MoveItemspaceRequest{
		SourceListType:       listTypeMain,
		SourceSlotIndex:      3,
		MoveCount:            3,
		DestinationListType:  listTypeMain,
		DestinationSlotIndex: 65,
	}))
	if err != nil {
		t.Fatalf("Move error = %v", err)
	}
	if result.Mode != "reverse_move" || result.MoveCount != 3 || !result.Changed {
		t.Fatalf("result = %+v", result)
	}

	loaded := loadTestInventory(t, ctx, repos, "77")
	if _, exists := loaded.Slots[slotKey(listTypeMain, 65)]; exists {
		t.Fatalf("occupied endpoint still exists: %+v", loaded.Slots)
	}
	if got := loaded.Slots[slotKey(listTypeMain, 3)]; got.ItemID != 3227 || got.Count != 3 {
		t.Fatalf("explicit quick slot stack = %+v", got)
	}
	if got := loaded.Slots[slotKey(listTypeMain, 66)].Count; got != 1 {
		t.Fatalf("ordinary compatible stack changed = %d", got)
	}
}

func TestOwnerMoveAutoStackDoesNotCrossAccountSharedOwnership(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	saveTestInventory(t, ctx, repos, dnfrepo.InventoryRecord{
		CharacterID: "77",
		Slots: map[string]dnfrepo.ItemStack{
			slotKey(listTypeMain, 65):  pvfMoveStack(3227, 2, 10),
			slotKey(listTypeMain, 354): pvfMoveStack(3227, 1, 10),
		},
	})
	owner, err := NewOwner(repos)
	if err != nil {
		t.Fatalf("NewOwner error = %v", err)
	}

	result, err := owner.Move(ctx, NewMoveCommand(alignedcmd.Request{
		AccountID:           "dnf:1",
		SelectedCharacterID: 77,
	}, MoveItemspaceRequest{
		SourceListType:       listTypeMain,
		SourceSlotIndex:      65,
		MoveCount:            2,
		DestinationListType:  listTypeMain,
		DestinationSlotIndex: 80,
	}))
	if err != nil {
		t.Fatalf("Move error = %v", err)
	}
	if result.Mode != "move" || len(result.RefreshListTypes) != 0 {
		t.Fatalf("result = %+v", result)
	}
	loaded := loadTestInventory(t, ctx, repos, "77")
	if got := loaded.Slots[slotKey(listTypeMain, 354)].Count; got != 1 {
		t.Fatalf("account-shared candidate changed = %d", got)
	}
	if got := loaded.Slots[slotKey(listTypeMain, 80)].Count; got != 2 {
		t.Fatalf("literal destination count = %d, want 2", got)
	}
}

func TestCanStackRequiresMatchingPVFIdentityOwnershipExpiryAndCapacity(t *testing.T) {
	baseSource := pvfMoveStack(3227, 2, 10)
	baseSource.Extra["pvf_path"] = "STACKABLE\\Material\\test_3227.stk"
	baseDestination := pvfMoveStack(3227, 8, 10)

	tests := []struct {
		name        string
		mutate      func(*dnfrepo.ItemStack, *dnfrepo.ItemStack)
		moveCount   int64
		wantAllowed bool
	}{
		{name: "exact limit", moveCount: 2, wantAllowed: true},
		{name: "over limit", moveCount: 3, mutate: func(source, _ *dnfrepo.ItemStack) { source.Count = 3 }, wantAllowed: false},
		{name: "missing item kind", moveCount: 2, mutate: func(source, _ *dnfrepo.ItemStack) { delete(source.Extra, "item_kind") }},
		{name: "missing PVF path", moveCount: 2, mutate: func(source, _ *dnfrepo.ItemStack) { delete(source.Extra, "pvf_path") }},
		{name: "different PVF path", moveCount: 2, mutate: func(_, destination *dnfrepo.ItemStack) {
			destination.Extra["pvf_path"] = "stackable/material/other.stk"
		}},
		{name: "different stack limit", moveCount: 2, mutate: func(_, destination *dnfrepo.ItemStack) { destination.Extra["stack_limit"] = "11" }},
		{name: "different bind", moveCount: 2, mutate: func(_, destination *dnfrepo.ItemStack) { destination.Bind = true }},
		{name: "different expiry", moveCount: 2, mutate: func(_, destination *dnfrepo.ItemStack) { destination.ExpireAt = time.Unix(1234, 0) }},
		{name: "source count exceeds PVF limit", moveCount: 2, mutate: func(source, _ *dnfrepo.ItemStack) { source.Count = 11 }},
		{name: "PVF omitted stack limit is uncapped", moveCount: 2, mutate: func(source, destination *dnfrepo.ItemStack) {
			delete(source.Extra, "stack_limit")
			delete(destination.Extra, "stack_limit")
		}, wantAllowed: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			source := cloneStack(baseSource)
			destination := cloneStack(baseDestination)
			if test.mutate != nil {
				test.mutate(&source, &destination)
			}
			if got := canStack(source, destination, test.moveCount); got != test.wantAllowed {
				t.Fatalf("canStack = %t, want %t; source=%+v destination=%+v", got, test.wantAllowed, source, destination)
			}
		})
	}
}

func TestOwnerManualMoveStacksPVFUnlimitedRows(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	source := pvfMoveStack(2660671, 200, 10)
	destination := pvfMoveStack(2660671, 200, 10)
	delete(source.Extra, "stack_limit")
	delete(destination.Extra, "stack_limit")
	saveTestInventory(t, ctx, repos, dnfrepo.InventoryRecord{
		CharacterID: "77",
		Slots: map[string]dnfrepo.ItemStack{
			slotKey(listTypeMain, 100): source,
			slotKey(listTypeMain, 109): destination,
		},
	})
	owner, err := NewOwner(repos)
	if err != nil {
		t.Fatalf("NewOwner error = %v", err)
	}

	result, err := owner.Move(ctx, NewMoveCommand(alignedcmd.Request{
		AccountID:           "dnf:1",
		SelectedCharacterID: 77,
	}, MoveItemspaceRequest{
		SourceListType:       listTypeMain,
		SourceSlotIndex:      100,
		MoveCount:            0,
		DestinationListType:  listTypeMain,
		DestinationSlotIndex: 109,
	}))
	if err != nil {
		t.Fatalf("Move error = %v", err)
	}
	if result.Mode != "stack" || result.MoveCount != 200 || !result.Changed {
		t.Fatalf("result = %+v", result)
	}
	if !reflect.DeepEqual(result.RefreshListTypes, []byte{listTypeMain}) {
		t.Fatalf("refresh list types = %v", result.RefreshListTypes)
	}
	loaded := loadTestInventory(t, ctx, repos, "77")
	if _, found := loaded.Slots[slotKey(listTypeMain, 100)]; found {
		t.Fatalf("source row still exists: %+v", loaded.Slots)
	}
	if got := loaded.Slots[slotKey(listTypeMain, 109)].Count; got != 400 {
		t.Fatalf("destination count = %d, want 400", got)
	}
}

func TestHandlerManualMoveStacksCapturedPVFUnlimitedRows(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	source := pvfMoveStack(2660671, 200, 10)
	destination := pvfMoveStack(2660671, 200, 10)
	delete(source.Extra, "stack_limit")
	delete(destination.Extra, "stack_limit")
	saveTestInventory(t, ctx, repos, dnfrepo.InventoryRecord{
		CharacterID: "19",
		Slots: map[string]dnfrepo.ItemStack{
			slotKey(listTypeMain, 100): source,
			slotKey(listTypeMain, 109): destination,
		},
	})

	got, err := NewHandler().Handle(ctx, alignedcmd.Request{
		Opcode:              uint16(dnfenum.CmdPacketMoveItemspace),
		Body:                currentMoveItemspaceBody(listTypeMain, 100, 0x0028993F, 0, listTypeMain, 109, 0x0028993F, 0, -1),
		AccountID:           "dnf:1",
		SelectedCharacterID: 19,
		Repositories:        repos,
	})
	if err != nil {
		t.Fatalf("Handle error = %v", err)
	}
	if !got.Handled || !got.ResponseAllowed || len(got.UpperResponses) != 2 {
		t.Fatalf("result = %+v", got)
	}
	wantACK := []byte{1, listTypeMain, 100, 0, 200, 0, 0, 0, listTypeMain, 109, 0}
	if ack := got.UpperResponses[0]; ack.MsgID != uint16(dnfenum.CmdPacketMoveItemspace) ||
		ack.Classification != 1 || !reflect.DeepEqual(ack.Body, wantACK) {
		t.Fatalf("ACK = %+v body=% X, want % X", ack, ack.Body, wantACK)
	}
	refresh := got.UpperResponses[1]
	if refresh.MsgID != msgItemListRefresh || refresh.Classification != 0 || refresh.Body[0] != listTypeMain {
		t.Fatalf("refresh = %+v", refresh)
	}
	if count := binary.LittleEndian.Uint16(refresh.Body[3:5]); count != 1 {
		t.Fatalf("refresh entry count = %d, want 1", count)
	}
	row := refresh.Body[5 : 5+currentItemListEntrySize]
	if slot := int16(binary.LittleEndian.Uint16(row[0:2])); slot != 109 ||
		binary.LittleEndian.Uint32(row[2:6]) != 2660671 ||
		binary.LittleEndian.Uint32(row[6:10]) != 400 {
		t.Fatalf("refresh row = % X", row[:16])
	}
}

func TestHandlerAutoStackSendsAckThenAuthoritativeMainItemList(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	saveTestInventory(t, ctx, repos, dnfrepo.InventoryRecord{
		CharacterID: "77",
		Slots: map[string]dnfrepo.ItemStack{
			slotKey(listTypeMain, 65): pvfMoveStack(3227, 3, 10),
			slotKey(listTypeMain, 70): pvfMoveStack(3227, 4, 10),
		},
	})
	if err := repos.AccountInventory.Save(ctx, dnfrepo.AccountInventoryRecord{
		AccountID: "dnf:1",
		Slots: map[string]dnfrepo.ItemStack{
			slotKey(listTypeMain, 354): {ItemID: 3033, Count: 9},
		},
	}); err != nil {
		t.Fatalf("Save account inventory error = %v", err)
	}

	got, err := NewHandler().Handle(ctx, alignedcmd.Request{
		Opcode:              uint16(dnfenum.CmdPacketMoveItemspace),
		Body:                currentMoveItemspaceBody(listTypeMain, 65, 0, 0, listTypeMain, 80, 0, 0, -1),
		AccountID:           "dnf:1",
		SelectedCharacterID: 77,
		Repositories:        repos,
	})
	if err != nil {
		t.Fatalf("Handle error = %v", err)
	}
	if !got.Handled || !got.ResponseAllowed || len(got.UpperResponses) != 2 {
		t.Fatalf("result = %+v", got)
	}
	ack := got.UpperResponses[0]
	if ack.MsgID != uint16(dnfenum.CmdPacketMoveItemspace) || ack.Classification != 1 {
		t.Fatalf("ACK header = %+v", ack)
	}
	wantACK := []byte{1, listTypeMain, 65, 0, 3, 0, 0, 0, listTypeMain, 80, 0}
	if !reflect.DeepEqual(ack.Body, wantACK) {
		t.Fatalf("ACK body = % X, want % X", ack.Body, wantACK)
	}

	refresh := got.UpperResponses[1]
	if refresh.MsgID != msgItemListRefresh || refresh.Classification != 0 || !refresh.AllowCodec {
		t.Fatalf("refresh header = %+v", refresh)
	}
	if len(refresh.Body) != 5+2*currentItemListEntrySize {
		t.Fatalf("refresh length = %d, want %d", len(refresh.Body), 5+2*currentItemListEntrySize)
	}
	if refresh.Body[0] != listTypeMain || binary.LittleEndian.Uint16(refresh.Body[3:5]) != 2 {
		t.Fatalf("refresh header body = % X", refresh.Body[:5])
	}
	first := refresh.Body[5 : 5+currentItemListEntrySize]
	second := refresh.Body[5+currentItemListEntrySize:]
	if slot := int16(binary.LittleEndian.Uint16(first[0:2])); slot != 70 ||
		binary.LittleEndian.Uint32(first[2:6]) != 3227 || binary.LittleEndian.Uint32(first[6:10]) != 7 {
		t.Fatalf("first row = % X", first[:16])
	}
	if slot := int16(binary.LittleEndian.Uint16(second[0:2])); slot != 354 ||
		binary.LittleEndian.Uint32(second[2:6]) != 3033 || binary.LittleEndian.Uint32(second[6:10]) != 9 {
		t.Fatalf("account row = % X", second[:16])
	}
}

func TestOwnerMoveAutoStackRollsBackWhenAuthoritativeListCannotBeBuilt(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	saveTestInventory(t, ctx, repos, dnfrepo.InventoryRecord{
		CharacterID: "77",
		Slots: map[string]dnfrepo.ItemStack{
			slotKey(listTypeMain, 65): pvfMoveStack(3227, 3, 10),
			slotKey(listTypeMain, 70): pvfMoveStack(3227, 4, 10),
		},
	})
	wantErr := errors.New("account snapshot unavailable")
	repos.AccountInventory = failingMoveAccountInventoryRepository{err: wantErr}
	owner, err := NewOwner(repos)
	if err != nil {
		t.Fatalf("NewOwner error = %v", err)
	}

	_, err = owner.Move(ctx, NewMoveCommand(alignedcmd.Request{
		AccountID:           "dnf:1",
		SelectedCharacterID: 77,
	}, MoveItemspaceRequest{
		SourceListType:       listTypeMain,
		SourceSlotIndex:      65,
		MoveCount:            3,
		DestinationListType:  listTypeMain,
		DestinationSlotIndex: 80,
	}))
	if !errors.Is(err, wantErr) {
		t.Fatalf("Move error = %v, want %v", err, wantErr)
	}
	loaded := loadTestInventory(t, ctx, repos, "77")
	if got := loaded.Slots[slotKey(listTypeMain, 65)].Count; got != 3 {
		t.Fatalf("source changed despite snapshot failure = %d", got)
	}
	if got := loaded.Slots[slotKey(listTypeMain, 70)].Count; got != 4 {
		t.Fatalf("target changed despite snapshot failure = %d", got)
	}
	if _, exists := loaded.Slots[slotKey(listTypeMain, 80)]; exists {
		t.Fatalf("literal destination appeared despite rollback: %+v", loaded.Slots)
	}
}

func TestHandlerAccountSharedExplicitStackSendsAckThenSingleMergedList0Refresh(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	saveTestInventory(t, ctx, repos, dnfrepo.InventoryRecord{
		CharacterID: "77",
		Slots: map[string]dnfrepo.ItemStack{
			slotKey(listTypeMain, 9): {ItemID: 700, Count: 1},
		},
	})
	if err := repos.AccountInventory.Save(ctx, dnfrepo.AccountInventoryRecord{
		AccountID: "dnf:1",
		Slots: map[string]dnfrepo.ItemStack{
			slotKey(listTypeMain, 354): pvfMoveStack(3227, 3, 10),
			slotKey(listTypeMain, 355): pvfMoveStack(3227, 4, 10),
		},
	}); err != nil {
		t.Fatalf("Save account inventory error = %v", err)
	}

	got, err := NewHandler().Handle(ctx, alignedcmd.Request{
		Opcode:              uint16(dnfenum.CmdPacketMoveItemspace),
		Body:                currentMoveItemspaceBody(listTypeMain, 354, 0, 3, listTypeMain, 355, 0, 0, -1),
		AccountID:           "dnf:1",
		SelectedCharacterID: 77,
		Repositories:        repos,
	})
	if err != nil {
		t.Fatalf("Handle error = %v", err)
	}
	if !got.Handled || !got.ResponseAllowed || len(got.UpperResponses) != 2 {
		t.Fatalf("result = %+v", got)
	}
	wantACK := []byte{1, listTypeMain, 0x62, 0x01, 3, 0, 0, 0, listTypeMain, 0x63, 0x01}
	if ack := got.UpperResponses[0]; ack.MsgID != uint16(dnfenum.CmdPacketMoveItemspace) || ack.Classification != 1 || !reflect.DeepEqual(ack.Body, wantACK) {
		t.Fatalf("ACK = %+v body=% X, want % X", ack, ack.Body, wantACK)
	}

	refresh := got.UpperResponses[1]
	if refresh.MsgID != msgItemListRefresh || refresh.Classification != 0 || refresh.Body[0] != listTypeMain {
		t.Fatalf("refresh = %+v", refresh)
	}
	if count := binary.LittleEndian.Uint16(refresh.Body[3:5]); count != 2 {
		t.Fatalf("refresh count = %d body=% X", count, refresh.Body[:5])
	}
	if len(refresh.Body) != 5+2*currentItemListEntrySize {
		t.Fatalf("refresh length = %d", len(refresh.Body))
	}
	ordinary := refresh.Body[5 : 5+currentItemListEntrySize]
	shared := refresh.Body[5+currentItemListEntrySize:]
	if slot := int16(binary.LittleEndian.Uint16(ordinary[0:2])); slot != 9 ||
		binary.LittleEndian.Uint32(ordinary[2:6]) != 700 || binary.LittleEndian.Uint32(ordinary[6:10]) != 1 {
		t.Fatalf("ordinary row = % X", ordinary[:16])
	}
	if slot := int16(binary.LittleEndian.Uint16(shared[0:2])); slot != 355 ||
		binary.LittleEndian.Uint32(shared[2:6]) != 3227 || binary.LittleEndian.Uint32(shared[6:10]) != 7 {
		t.Fatalf("shared row = % X", shared[:16])
	}
}

func TestOwnerAccountSharedExplicitCrossListStackFreezesSourceThenDestinationSnapshots(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	saveTestInventory(t, ctx, repos, dnfrepo.InventoryRecord{
		CharacterID: "77",
		Slots: map[string]dnfrepo.ItemStack{
			slotKey(listTypeMain, 9):   {ItemID: 700, Count: 1},
			slotKey(listTypeMain, 354): {ItemID: 9999, Count: 99},
		},
		Warehouse: map[string]dnfrepo.ItemStack{
			slotKey(listTypePersonalCargo, 8): pvfMoveStack(3227, 2, 10),
		},
	})
	if err := repos.AccountInventory.Save(ctx, dnfrepo.AccountInventoryRecord{
		AccountID: "dnf:1",
		Slots: map[string]dnfrepo.ItemStack{
			slotKey(listTypeMain, 354): pvfMoveStack(3227, 3, 10),
			slotKey(listTypeMain, 355): {ItemID: 3033, Count: 6},
		},
	}); err != nil {
		t.Fatalf("Save account inventory error = %v", err)
	}
	owner, err := NewOwner(repos)
	if err != nil {
		t.Fatalf("NewOwner error = %v", err)
	}

	result, err := owner.Move(ctx, NewMoveCommand(alignedcmd.Request{
		AccountID:           "dnf:1",
		SelectedCharacterID: 77,
	}, MoveItemspaceRequest{
		SourceListType:       listTypePersonalCargo,
		SourceSlotIndex:      8,
		MoveCount:            2,
		DestinationListType:  listTypeMain,
		DestinationSlotIndex: 354,
	}))
	if err != nil {
		t.Fatalf("Move error = %v", err)
	}
	if result.Mode != "stack" || !result.Changed || result.MoveCount != 2 {
		t.Fatalf("result = %+v", result)
	}
	if !reflect.DeepEqual(result.RefreshListTypes, []byte{listTypePersonalCargo, listTypeMain}) {
		t.Fatalf("refresh order = %v", result.RefreshListTypes)
	}
	if len(result.Refresh[listTypePersonalCargo]) != 0 {
		t.Fatalf("source snapshot = %+v, want empty post-merge list2", result.Refresh[listTypePersonalCargo])
	}
	main := result.Refresh[listTypeMain]
	if len(main) != 3 {
		t.Fatalf("main snapshot = %+v", main)
	}
	if got := main[slotKey(listTypeMain, 9)]; got.ItemID != 700 || got.Count != 1 {
		t.Fatalf("ordinary slot = %+v", got)
	}
	if got := main[slotKey(listTypeMain, 354)]; got.ItemID != 3227 || got.Count != 5 {
		t.Fatalf("overlaid shared destination = %+v", got)
	}
	if got := main[slotKey(listTypeMain, 355)]; got.ItemID != 3033 || got.Count != 6 {
		t.Fatalf("other shared slot = %+v", got)
	}
	for key := range main {
		if listType, _, ok := parseSlotKey(key); !ok || listType != listTypeMain {
			t.Fatalf("non-list0 key in main snapshot: %q", key)
		}
	}
}

func TestHandlerSameListPartialSplitSendsAckThenFinalMainRefresh(t *testing.T) {
	tests := []struct {
		name            string
		sourceSlot      int16
		destinationSlot int16
		shared          bool
		wantCounts      map[int16]uint32
	}{
		{
			name:            "ordinary split",
			sourceSlot:      65,
			destinationSlot: 80,
			wantCounts:      map[int16]uint32{10: 1, 65: 3, 80: 2},
		},
		{
			name:            "ordinary reverse split",
			sourceSlot:      80,
			destinationSlot: 65,
			wantCounts:      map[int16]uint32{10: 1, 65: 3, 80: 2},
		},
		{
			name:            "account shared split",
			sourceSlot:      354,
			destinationSlot: 9,
			shared:          true,
			wantCounts:      map[int16]uint32{9: 2, 10: 1, 354: 3},
		},
		{
			name:            "account shared reverse split",
			sourceSlot:      9,
			destinationSlot: 354,
			shared:          true,
			wantCounts:      map[int16]uint32{9: 2, 10: 1, 354: 3},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			repos := dnfrepomemory.NewMemoryGroup()
			characterSlots := map[string]dnfrepo.ItemStack{
				slotKey(listTypeMain, 10): {ItemID: 700, Count: 1},
			}
			accountSlots := make(map[string]dnfrepo.ItemStack)
			occupiedSlot := test.sourceSlot
			if test.name == "ordinary reverse split" || test.name == "account shared reverse split" {
				occupiedSlot = test.destinationSlot
			}
			if test.shared {
				accountSlots[slotKey(listTypeMain, occupiedSlot)] = pvfMoveStack(3227, 5, 10)
			} else {
				characterSlots[slotKey(listTypeMain, occupiedSlot)] = pvfMoveStack(3227, 5, 10)
			}
			saveTestInventory(t, ctx, repos, dnfrepo.InventoryRecord{CharacterID: "77", Slots: characterSlots})
			if err := repos.AccountInventory.Save(ctx, dnfrepo.AccountInventoryRecord{AccountID: "dnf:1", Slots: accountSlots}); err != nil {
				t.Fatalf("Save account inventory error = %v", err)
			}

			got, err := NewHandler().Handle(ctx, alignedcmd.Request{
				Opcode:              uint16(dnfenum.CmdPacketMoveItemspace),
				Body:                currentMoveItemspaceBody(listTypeMain, test.sourceSlot, 0, 2, listTypeMain, test.destinationSlot, 0, 0, -1),
				AccountID:           "dnf:1",
				SelectedCharacterID: 77,
				Repositories:        repos,
			})
			if err != nil {
				t.Fatalf("Handle error = %v", err)
			}
			if !got.Handled || !got.ResponseAllowed || len(got.UpperResponses) != 2 {
				t.Fatalf("result = %+v", got)
			}
			ack := got.UpperResponses[0]
			if ack.MsgID != uint16(dnfenum.CmdPacketMoveItemspace) || ack.Classification != 1 ||
				int16(binary.LittleEndian.Uint16(ack.Body[2:4])) != test.sourceSlot ||
				binary.LittleEndian.Uint32(ack.Body[4:8]) != 2 ||
				int16(binary.LittleEndian.Uint16(ack.Body[9:11])) != test.destinationSlot {
				t.Fatalf("ACK = %+v body=% X", ack, ack.Body)
			}
			refresh := got.UpperResponses[1]
			if refresh.MsgID != msgItemListRefresh || refresh.Classification != 0 || refresh.Body[0] != listTypeMain {
				t.Fatalf("refresh = %+v", refresh)
			}
			count := int(binary.LittleEndian.Uint16(refresh.Body[3:5]))
			if count != len(test.wantCounts) || len(refresh.Body) != 5+count*currentItemListEntrySize {
				t.Fatalf("refresh count=%d len=%d body=% X", count, len(refresh.Body), refresh.Body[:5])
			}
			gotCounts := make(map[int16]uint32, count)
			for index := range count {
				offset := 5 + index*currentItemListEntrySize
				row := refresh.Body[offset : offset+currentItemListEntrySize]
				gotCounts[int16(binary.LittleEndian.Uint16(row[0:2]))] = binary.LittleEndian.Uint32(row[6:10])
			}
			if !reflect.DeepEqual(gotCounts, test.wantCounts) {
				t.Fatalf("refresh counts = %v, want %v", gotCounts, test.wantCounts)
			}
		})
	}
}

func TestOwnerCrossListPartialSplitDoesNotRequestRefresh(t *testing.T) {
	tests := []struct {
		name       string
		shared     bool
		sourceSlot int16
	}{
		{name: "ordinary", sourceSlot: 65},
		{name: "account shared", shared: true, sourceSlot: 354},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			repos := dnfrepomemory.NewMemoryGroup()
			characterSlots := make(map[string]dnfrepo.ItemStack)
			accountSlots := make(map[string]dnfrepo.ItemStack)
			if test.shared {
				accountSlots[slotKey(listTypeMain, test.sourceSlot)] = pvfMoveStack(3227, 5, 10)
			} else {
				characterSlots[slotKey(listTypeMain, test.sourceSlot)] = pvfMoveStack(3227, 5, 10)
			}
			saveTestInventory(t, ctx, repos, dnfrepo.InventoryRecord{CharacterID: "77", Slots: characterSlots})
			if err := repos.AccountInventory.Save(ctx, dnfrepo.AccountInventoryRecord{AccountID: "dnf:1", Slots: accountSlots}); err != nil {
				t.Fatalf("Save account inventory error = %v", err)
			}
			owner, err := NewOwner(repos)
			if err != nil {
				t.Fatalf("NewOwner error = %v", err)
			}
			result, err := owner.Move(ctx, NewMoveCommand(alignedcmd.Request{
				AccountID:           "dnf:1",
				SelectedCharacterID: 77,
			}, MoveItemspaceRequest{
				SourceListType:       listTypeMain,
				SourceSlotIndex:      test.sourceSlot,
				MoveCount:            2,
				DestinationListType:  listTypePersonalCargo,
				DestinationSlotIndex: 8,
			}))
			if err != nil {
				t.Fatalf("Move error = %v", err)
			}
			if result.Mode != "split" || len(result.RefreshListTypes) != 0 || len(result.Refresh) != 0 {
				t.Fatalf("result = %+v", result)
			}
		})
	}
}

func TestHandlerAccountSharedExplicitStackTransactionFailureSendsNoSuccessAndRollsBack(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	saveTestInventory(t, ctx, repos, dnfrepo.InventoryRecord{
		CharacterID: "77",
		Warehouse: map[string]dnfrepo.ItemStack{
			slotKey(listTypePersonalCargo, 8): pvfMoveStack(3227, 2, 10),
		},
	})
	if err := repos.AccountInventory.Save(ctx, dnfrepo.AccountInventoryRecord{
		AccountID: "dnf:1",
		Slots: map[string]dnfrepo.ItemStack{
			slotKey(listTypeMain, 354): pvfMoveStack(3227, 3, 10),
		},
	}); err != nil {
		t.Fatalf("Save account inventory error = %v", err)
	}
	wantErr := errors.New("forced account stack rollback")
	repos.AccountItems = rollbackAccountCharacterItemsAfterApply{base: repos.AccountItems, err: wantErr}

	got, err := NewHandler().Handle(ctx, alignedcmd.Request{
		Opcode:              uint16(dnfenum.CmdPacketMoveItemspace),
		Body:                currentMoveItemspaceBody(listTypePersonalCargo, 8, 0, 2, listTypeMain, 354, 0, 0, -1),
		AccountID:           "dnf:1",
		SelectedCharacterID: 77,
		Repositories:        repos,
	})
	if err != nil {
		t.Fatalf("Handle error = %v", err)
	}
	// 2026-09-18：事务回滚同样必须回 op19 失败帧——客户端在收到 class1/op19 之前一直
	// 保持 pending 拖拽对象，静默等于把物品窗口锁死（见 ownerRejectedMoveRequestResult）。
	if !got.Handled || !got.ResponseAllowed || len(got.UpperResponses) != 1 {
		t.Fatalf("failure result = %+v", got)
	}
	wantFailure := []byte{0, moveItemspaceRejectedErrorCode, listTypePersonalCargo, listTypeMain}
	if body := got.UpperResponses[0].Body; string(body) != string(wantFailure) {
		t.Fatalf("failure ack=% X, want % X", body, wantFailure)
	}
	character := loadTestInventory(t, ctx, repos, "77")
	if got := character.Warehouse[slotKey(listTypePersonalCargo, 8)].Count; got != 2 {
		t.Fatalf("source count after rollback = %d, want 2", got)
	}
	account, found, loadErr := repos.AccountInventory.Load(ctx, "dnf:1")
	if loadErr != nil || !found {
		t.Fatalf("Load account inventory found=%t err=%v", found, loadErr)
	}
	if got := account.Slots[slotKey(listTypeMain, 354)].Count; got != 3 {
		t.Fatalf("destination count after rollback = %d, want 3", got)
	}
}

func TestHandlerMainAndPersonalCargoExplicitStackUsesTwoSlotIncrementalRefresh(t *testing.T) {
	tests := []struct {
		name            string
		sourceList      byte
		sourceSlot      int16
		destinationList byte
		destinationSlot int16
	}{
		{
			name:            "backpack into personal cargo",
			sourceList:      listTypeMain,
			sourceSlot:      65,
			destinationList: listTypePersonalCargo,
			destinationSlot: 8,
		},
		{
			name:            "personal cargo into backpack",
			sourceList:      listTypePersonalCargo,
			sourceSlot:      8,
			destinationList: listTypeMain,
			destinationSlot: 65,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			repos := dnfrepomemory.NewMemoryGroup()
			record := dnfrepo.InventoryRecord{
				CharacterID: "77",
				Slots:       make(map[string]dnfrepo.ItemStack),
				Warehouse:   make(map[string]dnfrepo.ItemStack),
			}
			source := pvfMoveStack(3227, 2, 10)
			destination := pvfMoveStack(3227, 3, 10)
			if test.sourceList == listTypePersonalCargo {
				record.Warehouse[slotKey(test.sourceList, test.sourceSlot)] = source
			} else {
				record.Slots[slotKey(test.sourceList, test.sourceSlot)] = source
			}
			if test.destinationList == listTypePersonalCargo {
				record.Warehouse[slotKey(test.destinationList, test.destinationSlot)] = destination
			} else {
				record.Slots[slotKey(test.destinationList, test.destinationSlot)] = destination
			}
			saveTestInventory(t, ctx, repos, record)

			got, err := NewHandler().Handle(ctx, alignedcmd.Request{
				Opcode: uint16(dnfenum.CmdPacketMoveItemspace),
				Body: currentMoveItemspaceBody(
					test.sourceList,
					test.sourceSlot,
					0,
					2,
					test.destinationList,
					test.destinationSlot,
					0,
					3,
					-1,
				),
				AccountID:           "dnf:1",
				SelectedCharacterID: 77,
				Repositories:        repos,
			})
			if err != nil {
				t.Fatalf("Handle error = %v", err)
			}
			wantRefreshes := []alignedcmd.ItemSlotRefresh{
				{ListType: test.sourceList, SlotIndex: test.sourceSlot},
				{ListType: test.destinationList, SlotIndex: test.destinationSlot},
			}
			if !got.Handled || !got.ResponseAllowed ||
				len(got.UpperResponses) != 1 ||
				!reflect.DeepEqual(got.ItemSlotRefreshes, wantRefreshes) ||
				!reflect.DeepEqual(got.PostActions, []alignedcmd.PostAction{alignedcmd.PostActionRefreshSelectedMainWallets}) {
				t.Fatalf("result = %+v, want refreshes=%+v", got, wantRefreshes)
			}
			ack := got.UpperResponses[0]
			if ack.MsgID != uint16(dnfenum.CmdPacketMoveItemspace) ||
				ack.Classification != 1 ||
				ack.Body[1] != test.sourceList ||
				int16(binary.LittleEndian.Uint16(ack.Body[2:4])) != test.sourceSlot ||
				binary.LittleEndian.Uint32(ack.Body[4:8]) != 2 ||
				ack.Body[8] != test.destinationList ||
				int16(binary.LittleEndian.Uint16(ack.Body[9:11])) != test.destinationSlot {
				t.Fatalf("ACK = %+v body=% X", ack, ack.Body)
			}

			loaded := loadTestInventory(t, ctx, repos, "77")
			sourceKey := slotKey(test.sourceList, test.sourceSlot)
			if _, exists := loaded.Slots[sourceKey]; exists {
				t.Fatalf("source remained in slots: %+v", loaded.Slots)
			}
			if _, exists := loaded.Warehouse[sourceKey]; exists {
				t.Fatalf("source remained in warehouse: %+v", loaded.Warehouse)
			}
			destinationKey := slotKey(test.destinationList, test.destinationSlot)
			merged, exists := loaded.Slots[destinationKey]
			if test.destinationList == listTypePersonalCargo {
				merged, exists = loaded.Warehouse[destinationKey]
			}
			if !exists || merged.ItemID != 3227 || merged.Count != 5 {
				t.Fatalf("merged destination = %+v exists=%t", merged, exists)
			}
		})
	}
}

func TestHandlerPersonalCargoWithdrawalRedirectsGoldWalletSlotIntoPVFPage(t *testing.T) {
	tests := []struct {
		name            string
		sourceList      byte
		sourceSlot      int16
		destinationList byte
		destinationSlot int16
		ackSlotOffset   int
	}{
		{
			name:            "cargo is source",
			sourceList:      listTypePersonalCargo,
			sourceSlot:      8,
			destinationList: listTypeMain,
			destinationSlot: 0,
			ackSlotOffset:   9,
		},
		{
			name:            "empty backpack endpoint is source",
			sourceList:      listTypeMain,
			sourceSlot:      0,
			destinationList: listTypePersonalCargo,
			destinationSlot: 8,
			ackSlotOffset:   2,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			repos := dnfrepomemory.NewMemoryGroup()
			saveTestInventory(t, ctx, repos, dnfrepo.InventoryRecord{
				CharacterID: "77",
				Slots: map[string]dnfrepo.ItemStack{
					slotKey(listTypeMain, 177): pvfMoveStack(999001, 1, 1),
				},
				Warehouse: map[string]dnfrepo.ItemStack{
					slotKey(listTypePersonalCargo, 8): pvfMoveStack(7273, 1, 100),
				},
			})
			resolver := func(itemID int64) (alignedcmd.StackMergeResolution, error) {
				return alignedcmd.StackMergeResolution{
					ItemID: itemID, Kind: "stackable", PVFPath: "stackable/ticket/portable_upgrade.stk",
					StackableType: "[legacy]", StackLimit: 100, SlotStart: 177, SlotEnd: 179,
				}, nil
			}
			got, err := NewHandler().Handle(ctx, alignedcmd.Request{
				Opcode: uint16(dnfenum.CmdPacketMoveItemspace),
				Body: currentMoveItemspaceBody(
					test.sourceList, test.sourceSlot, 0, 1,
					test.destinationList, test.destinationSlot, 0, 0, -1,
				),
				AccountID:           "dnf:1",
				SelectedCharacterID: 77,
				Repositories:        repos,
				StackMergeResolver:  resolver,
			})
			if err != nil {
				t.Fatalf("Handle error = %v", err)
			}
			if !got.Handled || !got.ResponseAllowed || len(got.UpperResponses) != 1 ||
				!reflect.DeepEqual(got.PostActions, []alignedcmd.PostAction{alignedcmd.PostActionRefreshSelectedMainWallets}) {
				t.Fatalf("result = %+v", got)
			}
			ack := got.UpperResponses[0].Body
			if gotSlot := int16(binary.LittleEndian.Uint16(ack[test.ackSlotOffset : test.ackSlotOffset+2])); gotSlot != 178 {
				t.Fatalf("redirected ACK slot = %d, want 178 body=% X", gotSlot, ack)
			}
			loaded := loadTestInventory(t, ctx, repos, "77")
			if _, exists := loaded.Slots[slotKey(listTypeMain, 0)]; exists {
				t.Fatalf("ordinary item persisted in gold-wallet slot: %+v", loaded.Slots)
			}
			if stack := loaded.Slots[slotKey(listTypeMain, 178)]; stack.ItemID != 7273 || stack.Count != 1 {
				t.Fatalf("redirected stack = %+v, want ticket in 178", stack)
			}
			if _, exists := loaded.Warehouse[slotKey(listTypePersonalCargo, 8)]; exists {
				t.Fatalf("cargo source survived withdrawal: %+v", loaded.Warehouse)
			}
		})
	}
}

// emblemMoveStack 造一行真实形状的徽章：pvf_path 与 stackable_type 都按现场（stackable/
// compound_result.stk、[avatar emblem]），这样 o.canStack 能靠 [stack limit] 判出可叠。
func emblemMoveStack(itemID, count int64) dnfrepo.ItemStack {
	return dnfrepo.ItemStack{
		ItemID: itemID,
		Count:  count,
		Extra: map[string]string{
			"item_kind":      "stackable",
			"pvf_path":       "stackable/compound_result.stk",
			"stackable_type": "[avatar emblem]",
			"stack_limit":    "999",
		},
	}
}

// emblemPageMoveResolver 复刻真实 PVF 对徽章的解析：徽章页是 289..351。
func emblemPageMoveResolver() alignedcmd.StackMergeResolver {
	return func(itemID int64) (alignedcmd.StackMergeResolution, error) {
		return alignedcmd.StackMergeResolution{
			ItemID: itemID, Kind: "stackable", PVFPath: "stackable/compound_result.stk",
			StackableType: "[avatar emblem]", StackLimit: 999,
			SlotStart: 289, SlotEnd: 344,
		}, nil
	}
}

// TestPersonalCargoWithdrawalKeepsEmblemOnItsPVFPage 复刻现场 2026-10-03 05:41（服主报）：
// 徽章存进个人仓库再取出时，客户端声明的落点是它自己挑的第一个空格 —— 消耗品页 0:116，
// 而徽章页是 289..351。旧行为照单落库，于是徽章出现在消耗品栏里（那一页客户端根本不
// 渲染徽章）。修法同晶体/灵魂与金币钱包行：op19 的落点由道具的 PVF 页决定。
//
// 现场两条日志（解密后）：
//
//	05:41:28  move src=(0,289 华丽的双重徽章 ×3) dst=(2,166)  存进仓库
//	05:41:31  move src=(2,166 ×3)              dst=(0,116)  ← 取出，落进消耗品栏
func TestPersonalCargoWithdrawalKeepsEmblemOnItsPVFPage(t *testing.T) {
	tests := []struct {
		name        string
		bag         map[string]dnfrepo.ItemStack
		wantSlot    int16
		wantCount   int64
		wantMerged  bool
		sourceCount int64
	}{
		{
			name:        "徽章页空着时落到页首空格",
			bag:         map[string]dnfrepo.ItemStack{},
			wantSlot:    289,
			wantCount:   3,
			sourceCount: 3,
		},
		{
			name: "徽章页已有同类行时并进去，不另开第二行",
			bag: map[string]dnfrepo.ItemStack{
				slotKey(listTypeMain, 300): emblemMoveStack(9001, 5),
			},
			wantSlot:    300,
			wantCount:   8,
			wantMerged:  true,
			sourceCount: 3,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			repos := dnfrepomemory.NewMemoryGroup()
			slots := map[string]dnfrepo.ItemStack{}
			for key, stack := range test.bag {
				slots[key] = stack
			}
			saveTestInventory(t, ctx, repos, dnfrepo.InventoryRecord{
				CharacterID: "77",
				Slots:       slots,
				Warehouse: map[string]dnfrepo.ItemStack{
					slotKey(listTypePersonalCargo, 166): emblemMoveStack(9001, 3),
				},
			})
			saveMoveContainerState(t, ctx, repos, "77", 24, 8)

			got, err := NewHandler().Handle(ctx, alignedcmd.Request{
				Opcode: uint16(dnfenum.CmdPacketMoveItemspace),
				// 客户端的错误声明：目标 0:116（消耗品页），正如现场日志。
				Body:                currentMoveItemspaceBody(listTypePersonalCargo, 166, 0, 3, listTypeMain, 116, 0, 0, -1),
				AccountID:           "dnf:1",
				SelectedCharacterID: 77,
				Repositories:        repos,
				StackMergeResolver:  emblemPageMoveResolver(),
			})
			if err != nil {
				t.Fatalf("Handle error = %v", err)
			}
			if !got.Handled || !got.ResponseAllowed || len(got.UpperResponses) != 1 {
				t.Fatalf("result = %+v", got)
			}
			// op19 ACK：目标槽必须回执真实落点，而不是客户端声明的 116。
			ack := got.UpperResponses[0].Body
			if gotSlot := int16(binary.LittleEndian.Uint16(ack[9:11])); gotSlot != test.wantSlot {
				t.Fatalf("redirected ACK slot = %d, want %d body=% X", gotSlot, test.wantSlot, ack)
			}
			loaded := loadTestInventory(t, ctx, repos, "77")
			if _, exists := loaded.Slots[slotKey(listTypeMain, 116)]; exists {
				t.Fatalf("徽章被写进了消耗品页 0:116（原 bug）：%+v", loaded.Slots)
			}
			if _, exists := loaded.Warehouse[slotKey(listTypePersonalCargo, 166)]; exists {
				t.Fatalf("仓库源行没有被搬空：%+v", loaded.Warehouse)
			}
			landed := loaded.Slots[slotKey(listTypeMain, test.wantSlot)]
			if landed.ItemID != 9001 || landed.Count != test.wantCount {
				t.Fatalf("落点 = %+v，want item 9001 count %d in slot %d", landed, test.wantCount, test.wantSlot)
			}
			if test.wantMerged {
				if len(loaded.Slots) != 1 {
					t.Fatalf("不该另开第二行：%+v", loaded.Slots)
				}
			} else if len(loaded.Slots) != 1 {
				t.Fatalf("徽章页应只多一行：%+v", loaded.Slots)
			}
		})
	}
}

// TestBagMoveSnapsMisplacedEmblemBackToItsPVFPage 覆盖历史脏行：已经被写在消耗品页上的
// 徽章（例如本次修复之前存进仓库再取出留下的那些），玩家在背包里把它拖到任意一格，
// 落点都会被拉回徽章页 289..351 —— 否则那些行只能靠洗数据清掉。
func TestBagMoveSnapsMisplacedEmblemBackToItsPVFPage(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	saveTestInventory(t, ctx, repos, dnfrepo.InventoryRecord{
		CharacterID: "77",
		Slots: map[string]dnfrepo.ItemStack{
			slotKey(listTypeMain, 113): emblemMoveStack(9001, 3),
		},
	})
	saveMoveContainerState(t, ctx, repos, "77", 24, 8)

	got, err := NewHandler().Handle(ctx, alignedcmd.Request{
		Opcode: uint16(dnfenum.CmdPacketMoveItemspace),
		// 玩家把这一摞（本来就在错页上）拖到消耗品页的另一格。
		Body:                currentMoveItemspaceBody(listTypeMain, 113, 0, 3, listTypeMain, 150, 0, 0, -1),
		AccountID:           "dnf:1",
		SelectedCharacterID: 77,
		Repositories:        repos,
		StackMergeResolver:  emblemPageMoveResolver(),
	})
	if err != nil {
		t.Fatalf("Handle error = %v", err)
	}
	if !got.Handled || !got.ResponseAllowed || len(got.UpperResponses) != 1 {
		t.Fatalf("result = %+v", got)
	}
	ack := got.UpperResponses[0].Body
	if gotSlot := int16(binary.LittleEndian.Uint16(ack[9:11])); gotSlot != 289 {
		t.Fatalf("redirected ACK slot = %d, want 289 body=% X", gotSlot, ack)
	}
	loaded := loadTestInventory(t, ctx, repos, "77")
	if _, exists := loaded.Slots[slotKey(listTypeMain, 113)]; exists {
		t.Fatalf("错页上的源行没有被搬空：%+v", loaded.Slots)
	}
	if _, exists := loaded.Slots[slotKey(listTypeMain, 150)]; exists {
		t.Fatalf("徽章又被写进了消耗品页 0:150：%+v", loaded.Slots)
	}
	if landed := loaded.Slots[slotKey(listTypeMain, 289)]; landed.ItemID != 9001 || landed.Count != 3 {
		t.Fatalf("落点 = %+v，want 9001 ×3 @289", landed)
	}
}

func TestOwnerPersonalCargoWithdrawalToGoldWalletSlotFailsAtomicallyWhenPVFPageFull(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	saveTestInventory(t, ctx, repos, dnfrepo.InventoryRecord{
		CharacterID: "77",
		Slots: map[string]dnfrepo.ItemStack{
			slotKey(listTypeMain, 177): pvfMoveStack(900177, 1, 1),
			slotKey(listTypeMain, 178): pvfMoveStack(900178, 1, 1),
		},
		Warehouse: map[string]dnfrepo.ItemStack{
			slotKey(listTypePersonalCargo, 8): pvfMoveStack(7273, 1, 100),
		},
	})
	owner, err := NewOwner(repos)
	if err != nil {
		t.Fatal(err)
	}
	owner.SetStackMergeResolver(func(itemID int64) (alignedcmd.StackMergeResolution, error) {
		return alignedcmd.StackMergeResolution{ItemID: itemID, Kind: "stackable", StackLimit: 100, SlotStart: 177, SlotEnd: 178}, nil
	})
	_, err = owner.Move(ctx, NewMoveCommand(alignedcmd.Request{AccountID: "dnf:1", SelectedCharacterID: 77}, MoveItemspaceRequest{
		SourceListType: listTypePersonalCargo, SourceSlotIndex: 8, MoveCount: 1,
		DestinationListType: listTypeMain, DestinationSlotIndex: 0,
	}))
	if !errors.Is(err, ErrReservedSlotRelocationFull) {
		t.Fatalf("Move error = %v, want ErrReservedSlotRelocationFull", err)
	}
	loaded := loadTestInventory(t, ctx, repos, "77")
	if stack := loaded.Warehouse[slotKey(listTypePersonalCargo, 8)]; stack.ItemID != 7273 || stack.Count != 1 {
		t.Fatalf("cargo changed after rejected move: %+v", loaded)
	}
	if _, exists := loaded.Slots[slotKey(listTypeMain, 0)]; exists {
		t.Fatalf("gold-wallet slot changed after rejected move: %+v", loaded.Slots)
	}
}

func pvfMoveStack(itemID, count, limit int64) dnfrepo.ItemStack {
	return dnfrepo.ItemStack{
		ItemID: itemID,
		Count:  count,
		Extra: map[string]string{
			"item_kind":      "stackable",
			"pvf_path":       "stackable/material/test_3227.stk",
			"stackable_type": "[material]",
			"stack_limit":    strconv.FormatInt(limit, 10),
		},
	}
}

type failingMoveAccountInventoryRepository struct {
	err error
}

func (r failingMoveAccountInventoryRepository) Load(context.Context, string) (dnfrepo.AccountInventoryRecord, bool, error) {
	return dnfrepo.AccountInventoryRecord{}, false, r.err
}

func (r failingMoveAccountInventoryRepository) Save(context.Context, dnfrepo.AccountInventoryRecord) error {
	return r.err
}

type rollbackAccountCharacterItemsAfterApply struct {
	base dnfrepo.AccountCharacterItemUnitOfWork
	err  error
}

func (u rollbackAccountCharacterItemsAfterApply) WithinAccountCharacterItems(
	ctx context.Context,
	accountID string,
	characterID string,
	apply func(dnfrepo.AccountInventoryRepository, dnfrepo.InventoryRepository) error,
) error {
	return u.base.WithinAccountCharacterItems(ctx, accountID, characterID, func(accounts dnfrepo.AccountInventoryRepository, characters dnfrepo.InventoryRepository) error {
		if err := apply(accounts, characters); err != nil {
			return err
		}
		return u.err
	})
}
