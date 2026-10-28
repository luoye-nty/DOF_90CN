package inventory

import (
	"context"
	"encoding/binary"
	"errors"
	"reflect"
	"testing"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

func TestMoveAccountCargoPersistsRowsUnderAccountOwner(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	if err := repos.Account.Save(ctx, dnfrepo.AccountRecord{
		AccountID: "dnf:account-cargo",
		Metadata: map[string]string{
			"account_cargo_created": "true",
			"account_cargo_level":   "8",
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repos.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "77",
		Slots: map[string]dnfrepo.ItemStack{
			"0:21": {ItemID: 100021, Count: 3, Extra: map[string]string{"stack_limit": "100"}},
		},
	}); err != nil {
		t.Fatal(err)
	}
	owner, err := NewOwner(repos)
	if err != nil {
		t.Fatal(err)
	}
	owner.SetStackMergeResolver(accountCargoTestResolver(map[int64]int64{100021: 100}))
	result, err := owner.Move(ctx, NewMoveCommand(alignedcmd.Request{
		AccountID:           "dnf:account-cargo",
		SelectedCharacterID: 77,
	}, MoveItemspaceRequest{
		SourceListType:       listTypeMain,
		SourceSlotIndex:      21,
		MoveCount:            3,
		DestinationListType:  listTypeAccountCargo,
		DestinationSlotIndex: 2,
	}))
	if err != nil {
		t.Fatalf("move to account cargo: %v", err)
	}
	if !result.Changed || result.Mode != "move" || result.MoveCount != 3 {
		t.Fatalf("move result=%+v", result)
	}
	character, found, err := repos.Inventory.Load(ctx, "77")
	if err != nil || !found {
		t.Fatalf("load character found=%t err=%v", found, err)
	}
	if _, exists := character.Slots["0:21"]; exists {
		t.Fatalf("source still belongs to character: %+v", character.Slots)
	}
	cargo, found, err := repos.AccountInventory.Load(ctx, "dnf:account-cargo")
	if err != nil || !found {
		t.Fatalf("load account cargo found=%t err=%v", found, err)
	}
	if got := cargo.Slots["12:2"]; got.ItemID != 100021 || got.Count != 3 {
		t.Fatalf("account cargo row=%+v", got)
	}

	// A different selected character must see the same account-owned row and
	// can take it back without creating a per-character list-12 duplicate.
	if err := repos.Inventory.Save(ctx, dnfrepo.InventoryRecord{CharacterID: "78", Slots: map[string]dnfrepo.ItemStack{}}); err != nil {
		t.Fatal(err)
	}
	result, err = owner.Move(ctx, NewMoveCommand(alignedcmd.Request{
		AccountID:           "dnf:account-cargo",
		SelectedCharacterID: 78,
	}, MoveItemspaceRequest{
		SourceListType:       listTypeAccountCargo,
		SourceSlotIndex:      2,
		MoveCount:            3,
		DestinationListType:  listTypeMain,
		DestinationSlotIndex: 22,
	}))
	if err != nil {
		t.Fatalf("withdraw from account cargo: %v", err)
	}
	if !result.Changed || result.Mode != "move" {
		t.Fatalf("withdraw result=%+v", result)
	}
	second, found, err := repos.Inventory.Load(ctx, "78")
	if err != nil || !found {
		t.Fatalf("load second character found=%t err=%v", found, err)
	}
	if got := second.Slots["0:22"]; got.ItemID != 100021 || got.Count != 3 {
		t.Fatalf("withdrawn row=%+v", got)
	}
	cargo, found, err = repos.AccountInventory.Load(ctx, "dnf:account-cargo")
	if err != nil || !found {
		t.Fatalf("reload account cargo found=%t err=%v", found, err)
	}
	if _, exists := cargo.Slots["12:2"]; exists {
		t.Fatalf("account cargo row remained after withdraw: %+v", cargo.Slots)
	}
}

func TestHandlerAccountCargoRejectsSecondPVFCharmInQuickSlotWithoutPartialSave(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	const accountID = "dnf:account-cargo-charm"
	if err := repos.Account.Save(ctx, dnfrepo.AccountRecord{
		AccountID: accountID,
		Metadata: map[string]string{
			"account_cargo_created": "true",
			"account_cargo_level":   "8",
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repos.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "77",
		Slots: map[string]dnfrepo.ItemStack{
			slotKey(listTypeMain, 4): {ItemID: 400360003, Count: 1},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repos.AccountInventory.Save(ctx, dnfrepo.AccountInventoryRecord{
		AccountID: accountID,
		Slots: map[string]dnfrepo.ItemStack{
			slotKey(listTypeAccountCargo, 6): {ItemID: 400360005, Count: 1},
		},
	}); err != nil {
		t.Fatal(err)
	}
	resolver := func(itemID int64) (alignedcmd.StackMergeResolution, error) {
		return alignedcmd.StackMergeResolution{ItemID: itemID, Kind: "equipment", EquipmentType: "[charm]"}, nil
	}

	got, err := NewHandler().Handle(ctx, alignedcmd.Request{
		Opcode:              uint16(dnfenum.CmdPacketMoveItemspace),
		Body:                currentMoveItemspaceBody(listTypeAccountCargo, 6, 0, 1, listTypeMain, 3, 0, 0, -1),
		AccountID:           accountID,
		SelectedCharacterID: 77,
		Repositories:        repos,
		StackMergeResolver:  resolver,
	})
	if err != nil {
		t.Fatalf("Handle error = %v", err)
	}
	wantBody := []byte{0, moveItemspaceRejectedErrorCode, listTypeAccountCargo, listTypeMain}
	if !got.Handled || !got.ResponseAllowed || len(got.UpperResponses) != 1 ||
		!reflect.DeepEqual(got.UpperResponses[0].Body, wantBody) {
		t.Fatalf("result = %+v, want body % X", got, wantBody)
	}
	character, _, _ := repos.Inventory.Load(ctx, "77")
	if _, found := character.Slots[slotKey(listTypeMain, 3)]; found ||
		character.Slots[slotKey(listTypeMain, 4)].ItemID != 400360003 {
		t.Fatalf("character partially saved: %+v", character.Slots)
	}
	cargo, _, _ := repos.AccountInventory.Load(ctx, accountID)
	if got := cargo.Slots[slotKey(listTypeAccountCargo, 6)].ItemID; got != 400360005 {
		t.Fatalf("account cargo source changed to %d", got)
	}
}

func TestMoveAccountCargoUsesPVFResolverToMergeLegacyMaterial(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	if err := repos.Account.Save(ctx, dnfrepo.AccountRecord{AccountID: "dnf:legacy-cargo", Metadata: map[string]string{
		"account_cargo_created": "true", "account_cargo_level": "8",
	}}); err != nil {
		t.Fatal(err)
	}
	if err := repos.Inventory.Save(ctx, dnfrepo.InventoryRecord{CharacterID: "77", Slots: map[string]dnfrepo.ItemStack{
		"0:65": {ItemID: 3244, Count: 5, Extra: map[string]string{"source": "pickup"}},
	}}); err != nil {
		t.Fatal(err)
	}
	if err := repos.AccountInventory.Save(ctx, dnfrepo.AccountInventoryRecord{AccountID: "dnf:legacy-cargo", Slots: map[string]dnfrepo.ItemStack{
		"12:2": {ItemID: 3244, Count: 7, Extra: map[string]string{"source": "mail"}},
	}}); err != nil {
		t.Fatal(err)
	}
	owner, err := NewOwner(repos)
	if err != nil {
		t.Fatal(err)
	}
	owner.SetStackMergeResolver(func(itemID int64) (alignedcmd.StackMergeResolution, error) {
		return alignedcmd.StackMergeResolution{
			ItemID:     itemID,
			Kind:       "stackable",
			PVFPath:    "stackable/material/crystal_pure.stk",
			AttachType: "`[free]`",
			// 这条用例考的是"按 PVF 解析器合并进已有堆"，与 attach type 无关；
			// 解析器必须同时声明它可入金库，否则会在入库闸门就被挡掉。
			StackableType: "[material]",
		}, nil
	})
	result, err := owner.Move(ctx, NewMoveCommand(alignedcmd.Request{AccountID: "dnf:legacy-cargo", SelectedCharacterID: 77}, MoveItemspaceRequest{
		SourceListType: listTypeMain, SourceSlotIndex: 65, MoveCount: 5,
		DestinationListType: listTypeAccountCargo, DestinationSlotIndex: 6,
	}))
	if err != nil {
		t.Fatalf("Move error = %v", err)
	}
	if result.Mode != "auto_stack" || result.MoveCount != 5 || !result.Changed {
		t.Fatalf("result = %+v", result)
	}
	cargo, found, err := repos.AccountInventory.Load(ctx, "dnf:legacy-cargo")
	if err != nil || !found {
		t.Fatalf("load cargo found=%t err=%v", found, err)
	}
	if _, exists := cargo.Slots["12:6"]; exists {
		t.Fatalf("literal destination was populated: %+v", cargo.Slots)
	}
	if stack := cargo.Slots["12:2"]; stack.Count != 12 || stack.Extra["pvf_path"] != "stackable/material/crystal_pure.stk" {
		t.Fatalf("merged cargo stack = %+v", stack)
	}
}

// TestMoveAccountCargoRedirectsSlotOutsidePersistedCapacityIntoUnlockedSlot 运营口径
// 2026-09-18：落点在未解锁栏位（这里是只解锁 1 格的金库、客户端却拖到第 1 格）不再报错，
// 而是顺延到已解锁区第一个空格（第 0 格）。旧行为是把这次拖拽整个拒掉。
func TestMoveAccountCargoRedirectsSlotOutsidePersistedCapacityIntoUnlockedSlot(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	if err := repos.Account.Save(ctx, dnfrepo.AccountRecord{
		AccountID: "dnf:tiny-cargo",
		Metadata:  map[string]string{"account_cargo_created": "true", "account_cargo_level": "1"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repos.Inventory.Save(ctx, dnfrepo.InventoryRecord{CharacterID: "77", Slots: map[string]dnfrepo.ItemStack{"0:1": {ItemID: 1, Count: 1}}}); err != nil {
		t.Fatal(err)
	}
	owner, err := NewOwner(repos)
	if err != nil {
		t.Fatal(err)
	}
	owner.SetStackMergeResolver(accountCargoTestFreeResolver())
	result, err := owner.Move(ctx, NewMoveCommand(alignedcmd.Request{
		AccountID:           "dnf:tiny-cargo",
		SelectedCharacterID: 77,
	}, MoveItemspaceRequest{
		SourceListType:       listTypeMain,
		SourceSlotIndex:      1,
		MoveCount:            1,
		DestinationListType:  listTypeAccountCargo,
		DestinationSlotIndex: 1,
	}))
	if err != nil {
		t.Fatalf("Move error = %v", err)
	}
	if !result.Changed || result.DestinationSlotIndex != 0 || !result.RedirectedReservedMainSlot {
		t.Fatalf("result = %+v, 期望顺延到 12:0", result)
	}
	cargo, found, err := repos.AccountInventory.Load(ctx, "dnf:tiny-cargo")
	if err != nil || !found {
		t.Fatalf("load cargo found=%t err=%v", found, err)
	}
	if _, exists := cargo.Slots["12:1"]; exists {
		t.Fatalf("未解锁格不能被写入: %+v", cargo.Slots)
	}
	if stack := cargo.Slots["12:0"]; stack.ItemID != 1 || stack.Count != 1 {
		t.Fatalf("落点应为 12:0: %+v", cargo.Slots)
	}
}

func TestMoveAccountCargoAutoStacksBackpackDepositIntoLowestPVFCompatibleSlot(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	if err := repos.Account.Save(ctx, dnfrepo.AccountRecord{
		AccountID: "dnf:auto-deposit",
		Metadata: map[string]string{
			"account_cargo_created": "true",
			"account_cargo_level":   "8",
		},
	}); err != nil {
		t.Fatal(err)
	}
	source := pvfMoveStack(3227, 2, 10)
	source.RawEntry = make([]byte, currentItemListEntrySize)
	binary.LittleEndian.PutUint32(source.RawEntry[0x06:0x0A], 2)
	target := pvfMoveStack(3227, 3, 10)
	target.RawEntry = make([]byte, currentItemListEntrySize)
	binary.LittleEndian.PutUint32(target.RawEntry[0x06:0x0A], 3)
	if err := repos.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "77",
		Slots: map[string]dnfrepo.ItemStack{
			"0:65": source,
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repos.AccountInventory.Save(ctx, dnfrepo.AccountInventoryRecord{
		AccountID: "dnf:auto-deposit",
		Slots: map[string]dnfrepo.ItemStack{
			// 同一道具在一个容器里只有一行：3227 只放在 12:0。
			"12:0": target,
			"12:2": pvfMoveStack(3331, 3, 10),
			// A stale row beyond the persisted capacity is never an auto target.
			"12:9": pvfMoveStack(3227, 1, 10),
		},
	}); err != nil {
		t.Fatal(err)
	}
	owner, err := NewOwner(repos)
	if err != nil {
		t.Fatal(err)
	}
	owner.SetStackMergeResolver(accountCargoTestResolver(map[int64]int64{3227: 10}))

	result, err := owner.Move(ctx, NewMoveCommand(alignedcmd.Request{
		AccountID:           "dnf:auto-deposit",
		SelectedCharacterID: 77,
	}, MoveItemspaceRequest{
		SourceListType:       listTypeMain,
		SourceSlotIndex:      65,
		MoveCount:            2,
		DestinationListType:  listTypeAccountCargo,
		DestinationSlotIndex: 4,
	}))
	if err != nil {
		t.Fatalf("auto-stack deposit: %v", err)
	}
	if result.Mode != "auto_stack" || result.MoveCount != 2 || !result.Changed {
		t.Fatalf("result=%+v", result)
	}
	// 跨容器 auto_stack 的真实落点写在 ACK 里，不能再补角色容器的整 list 快照：
	// list-0 的保留槽 0（金币钱包行）会被一起下发，现场就是"获得了 N 金币"。
	if len(result.RefreshListTypes) != 0 || len(result.Refresh) != 0 {
		t.Fatalf("refresh=%v lists=%v, want none", result.Refresh, result.RefreshListTypes)
	}
	if result.DestinationSlotIndex != 0 || !result.RedirectedReservedMainSlot {
		t.Fatalf("ack destination=%d redirected=%t, want the real merged slot 0",
			result.DestinationSlotIndex, result.RedirectedReservedMainSlot)
	}
	character, found, err := repos.Inventory.Load(ctx, "77")
	if err != nil || !found {
		t.Fatalf("load character found=%t err=%v", found, err)
	}
	if _, exists := character.Slots["0:65"]; exists {
		t.Fatalf("source remained after deposit: %+v", character.Slots)
	}
	cargo, found, err := repos.AccountInventory.Load(ctx, "dnf:auto-deposit")
	if err != nil || !found {
		t.Fatalf("load cargo found=%t err=%v", found, err)
	}
	if _, exists := cargo.Slots["12:4"]; exists {
		t.Fatalf("literal empty destination was populated: %+v", cargo.Slots)
	}
	merged := cargo.Slots["12:0"]
	if merged.Count != 5 || binary.LittleEndian.Uint32(merged.RawEntry[0x06:0x0A]) != 5 {
		t.Fatalf("merged cargo stack=%+v raw=%x", merged, merged.RawEntry)
	}
	if cargo.Slots["12:9"].Count != 1 {
		t.Fatalf("out-of-capacity stale row changed: %+v", cargo.Slots["12:9"])
	}
}

func TestMoveAccountCargoAutoStacksWithdrawalIntoExistingBackpackStack(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	if err := repos.Account.Save(ctx, dnfrepo.AccountRecord{
		AccountID: "dnf:auto-withdraw",
		Metadata: map[string]string{
			"account_cargo_created": "true",
			"account_cargo_level":   "8",
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repos.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "77",
		Slots: map[string]dnfrepo.ItemStack{
			"0:66": pvfMoveStack(3227, 3, 10),
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repos.AccountInventory.Save(ctx, dnfrepo.AccountInventoryRecord{
		AccountID: "dnf:auto-withdraw",
		Slots: map[string]dnfrepo.ItemStack{
			"12:6": pvfMoveStack(3227, 2, 10),
		},
	}); err != nil {
		t.Fatal(err)
	}
	owner, err := NewOwner(repos)
	if err != nil {
		t.Fatal(err)
	}

	result, err := owner.Move(ctx, NewMoveCommand(alignedcmd.Request{
		AccountID:           "dnf:auto-withdraw",
		SelectedCharacterID: 77,
	}, MoveItemspaceRequest{
		SourceListType:       listTypeAccountCargo,
		SourceSlotIndex:      6,
		MoveCount:            2,
		DestinationListType:  listTypeMain,
		DestinationSlotIndex: 70,
	}))
	if err != nil {
		t.Fatalf("auto-stack withdrawal: %v", err)
	}
	if result.Mode != "auto_stack" || result.MoveCount != 2 || !result.Changed {
		t.Fatalf("result=%+v", result)
	}
	// 同上：真实落点（背包里那一堆）写进 ACK，不再补整 list 快照。
	if len(result.RefreshListTypes) != 0 || len(result.Refresh) != 0 {
		t.Fatalf("refresh=%v lists=%v, want none", result.Refresh, result.RefreshListTypes)
	}
	if result.DestinationSlotIndex != 66 || !result.RedirectedReservedMainSlot {
		t.Fatalf("ack destination=%d redirected=%t, want the real merged slot 66",
			result.DestinationSlotIndex, result.RedirectedReservedMainSlot)
	}
	character, found, err := repos.Inventory.Load(ctx, "77")
	if err != nil || !found {
		t.Fatalf("load character found=%t err=%v", found, err)
	}
	if got := character.Slots["0:66"].Count; got != 5 {
		t.Fatalf("backpack merged count=%d want=5", got)
	}
	if _, exists := character.Slots["0:70"]; exists {
		t.Fatalf("literal empty destination was populated: %+v", character.Slots)
	}
	cargo, found, err := repos.AccountInventory.Load(ctx, "dnf:auto-withdraw")
	if err != nil || !found {
		t.Fatalf("load cargo found=%t err=%v", found, err)
	}
	if _, exists := cargo.Slots["12:6"]; exists {
		t.Fatalf("cargo source remained: %+v", cargo.Slots)
	}
}

func TestHandlerAccountCargoAutoStackReturnsAuthoritativeCharacterAndCargoRefreshPlan(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	if err := repos.Account.Save(ctx, dnfrepo.AccountRecord{
		AccountID: "dnf:auto-handler",
		Metadata: map[string]string{
			"account_cargo_created": "true",
			"account_cargo_level":   "8",
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repos.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "77",
		Slots: map[string]dnfrepo.ItemStack{
			"0:66": pvfMoveStack(3227, 3, 10),
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repos.AccountInventory.Save(ctx, dnfrepo.AccountInventoryRecord{
		AccountID: "dnf:auto-handler",
		Slots: map[string]dnfrepo.ItemStack{
			"12:6": pvfMoveStack(3227, 2, 10),
		},
	}); err != nil {
		t.Fatal(err)
	}

	got, err := NewHandler().Handle(ctx, alignedcmd.Request{
		Opcode: uint16(dnfenum.CmdPacketMoveItemspace),
		Body: currentMoveItemspaceBody(
			listTypeAccountCargo,
			6,
			0,
			2,
			listTypeMain,
			70,
			0,
			0,
			-1,
		),
		AccountID:           "dnf:auto-handler",
		SelectedCharacterID: 77,
		Repositories:        repos,
	})
	if err != nil {
		t.Fatalf("Handle error=%v", err)
	}
	if !got.Handled || !got.ResponseAllowed || len(got.UpperResponses) != 1 ||
		len(got.ItemSlotRefreshes) != 0 ||
		!reflect.DeepEqual(got.PostActions, []alignedcmd.PostAction{alignedcmd.PostActionRefreshSelectedAccountCargo}) {
		t.Fatalf("result=%+v", got)
	}
	ack := got.UpperResponses[0]
	if ack.MsgID != uint16(dnfenum.CmdPacketMoveItemspace) || ack.Classification != 1 ||
		binary.LittleEndian.Uint32(ack.Body[4:8]) != 2 {
		t.Fatalf("ACK=%+v body=%x", ack, ack.Body)
	}
	// 真实落点是背包已有的那一堆 0:66，不是客户端拖过去的空格 0:70；ACK 必须报 66，
	// 并且不再补角色容器的整 list 快照（那会把 list-0 的金币钱包行一起下发）。
	wantACK := []byte{1, listTypeAccountCargo, 6, 0, 2, 0, 0, 0, listTypeMain, 66, 0}
	if !reflect.DeepEqual(ack.Body, wantACK) {
		t.Fatalf("ACK body=% X, want % X", ack.Body, wantACK)
	}
}

func TestHandlerAccountCargoExplicitStackRefreshesCharacterEndpointAndCargo(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	if err := repos.Account.Save(ctx, dnfrepo.AccountRecord{
		AccountID: "dnf:explicit-stack",
		Metadata: map[string]string{
			"account_cargo_created": "true",
			"account_cargo_level":   "8",
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repos.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "77",
		Slots: map[string]dnfrepo.ItemStack{
			"0:66": pvfMoveStack(3227, 3, 10),
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repos.AccountInventory.Save(ctx, dnfrepo.AccountInventoryRecord{
		AccountID: "dnf:explicit-stack",
		Slots: map[string]dnfrepo.ItemStack{
			"12:6": pvfMoveStack(3227, 2, 10),
		},
	}); err != nil {
		t.Fatal(err)
	}

	got, err := NewHandler().Handle(ctx, alignedcmd.Request{
		Opcode: uint16(dnfenum.CmdPacketMoveItemspace),
		Body: currentMoveItemspaceBody(
			listTypeAccountCargo,
			6,
			0,
			2,
			listTypeMain,
			66,
			0,
			3,
			-1,
		),
		AccountID:           "dnf:explicit-stack",
		SelectedCharacterID: 77,
		Repositories:        repos,
	})
	if err != nil {
		t.Fatalf("Handle error=%v", err)
	}
	if !got.Handled || !got.ResponseAllowed || len(got.UpperResponses) != 1 ||
		!reflect.DeepEqual(got.ItemSlotRefreshes, []alignedcmd.ItemSlotRefresh{{ListType: listTypeMain, SlotIndex: 66}}) ||
		!reflect.DeepEqual(got.PostActions, []alignedcmd.PostAction{alignedcmd.PostActionRefreshSelectedAccountCargo}) {
		t.Fatalf("result=%+v", got)
	}
}

func TestHandlerAccountCargoAutoStackTransactionFailureRollsBackBothOwners(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	if err := repos.Account.Save(ctx, dnfrepo.AccountRecord{
		AccountID: "dnf:auto-rollback",
		Metadata: map[string]string{
			"account_cargo_created": "true",
			"account_cargo_level":   "8",
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repos.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "77",
		Slots: map[string]dnfrepo.ItemStack{
			"0:65": pvfMoveStack(3227, 2, 10),
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repos.AccountInventory.Save(ctx, dnfrepo.AccountInventoryRecord{
		AccountID: "dnf:auto-rollback",
		Slots: map[string]dnfrepo.ItemStack{
			"12:2": pvfMoveStack(3227, 3, 10),
		},
	}); err != nil {
		t.Fatal(err)
	}
	wantErr := errors.New("forced account cargo auto-stack rollback")
	repos.AccountItems = rollbackAccountCharacterItemsAfterApply{base: repos.AccountItems, err: wantErr}

	got, err := NewHandler().Handle(ctx, alignedcmd.Request{
		Opcode: uint16(dnfenum.CmdPacketMoveItemspace),
		Body: currentMoveItemspaceBody(
			listTypeMain,
			65,
			0,
			2,
			listTypeAccountCargo,
			4,
			0,
			0,
			-1,
		),
		AccountID:           "dnf:auto-rollback",
		SelectedCharacterID: 77,
		Repositories:        repos,
		StackMergeResolver:  accountCargoTestResolver(map[int64]int64{3227: 10}),
	})
	if err != nil {
		t.Fatalf("Handle error=%v", err)
	}
	// 2026-09-18：已解析成 move 的请求被 owner 拒绝时必须回 op19 失败帧。静默会让客户端
	// sub_1D2DE80 的 pending 拖拽对象永远不释放——现场就是金库/装备/消耗品窗口全部点不动。
	if !got.Handled || !got.ResponseAllowed || len(got.UpperResponses) != 1 {
		t.Fatalf("failure result=%+v", got)
	}
	wantFailure := []byte{0, moveItemspaceRejectedErrorCode, listTypeMain, listTypeAccountCargo}
	if body := got.UpperResponses[0].Body; string(body) != string(wantFailure) {
		t.Fatalf("failure ack=% X, want % X", body, wantFailure)
	}
	character, _, _ := repos.Inventory.Load(ctx, "77")
	if got := character.Slots["0:65"].Count; got != 2 {
		t.Fatalf("source after rollback=%d want=2", got)
	}
	cargo, _, _ := repos.AccountInventory.Load(ctx, "dnf:auto-rollback")
	if got := cargo.Slots["12:2"].Count; got != 3 {
		t.Fatalf("cargo target after rollback=%d want=3", got)
	}
}

// TestHandlerAccountCargoToPersonalCargoLockedSlotMoveLandsInFirstUnlockedSlot 反方向
// （账号金库 → 个人仓库）同样按运营口径顺延：拖到未解锁的 2:40 时落到第一个已解锁空格。
func TestHandlerAccountCargoToPersonalCargoLockedSlotMoveLandsInFirstUnlockedSlot(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	saveLockedSlotAccountCargoFixture(t, ctx, repos, "dnf:locked-slot", map[string]dnfrepo.ItemStack{
		slotKey(listTypeAccountCargo, 0): pvfMoveStack(3330, 1, 0),
	})
	saveTestInventory(t, ctx, repos, dnfrepo.InventoryRecord{
		CharacterID: "77",
		Warehouse:   map[string]dnfrepo.ItemStack{},
	})
	saveMoveContainerState(t, ctx, repos, "77", 24, 8)

	got, err := NewHandler().Handle(ctx, alignedcmd.Request{
		Opcode:              uint16(dnfenum.CmdPacketMoveItemspace),
		Body:                currentMoveItemspaceBody(listTypeAccountCargo, 0, 0, 1, listTypePersonalCargo, 40, 0, 0, -1),
		AccountID:           "dnf:locked-slot",
		SelectedCharacterID: 77,
		Repositories:        repos,
	})
	if err != nil {
		t.Fatalf("Handle error = %v", err)
	}
	if !got.Handled || !got.ResponseAllowed || len(got.UpperResponses) != 1 {
		t.Fatalf("result = %+v", got)
	}
	_, _, dstList, dstSlot := parseMoveSuccessAckEndpoints(t, got.UpperResponses[0].Body)
	if dstList != listTypePersonalCargo || dstSlot != 0 {
		t.Fatalf("ACK 目标 = (%d,%d), 期望 (2,0)", dstList, dstSlot)
	}
	loaded := loadTestInventory(t, ctx, repos, "77")
	if _, exists := loaded.Warehouse[slotKey(listTypePersonalCargo, 40)]; exists {
		t.Fatalf("未解锁格不能被写入: %+v", loaded.Warehouse)
	}
	if landed := loaded.Warehouse[slotKey(listTypePersonalCargo, 0)]; landed.ItemID != 3330 || landed.Count != 1 {
		t.Fatalf("落点应为 2:0 的 3330x1: %+v", landed)
	}
	cargo, _, _ := repos.AccountInventory.Load(ctx, "dnf:locked-slot")
	if _, exists := cargo.Slots[slotKey(listTypeAccountCargo, 0)]; exists {
		t.Fatalf("金库源行应被搬空: %+v", cargo.Slots)
	}
}

// TestHandlerAccountCargoSameContainerMoveToLockedSlotSnapsBack 金库内把一堆拖到未解锁栏位：
// 按运营口径 2026-09-18 **不适用**"顺延到第一个空格"，直接回原位（回 op19 失败帧）。
func TestHandlerAccountCargoSameContainerMoveToLockedSlotSnapsBack(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	saveLockedSlotAccountCargoFixture(t, ctx, repos, "dnf:locked-slot", map[string]dnfrepo.ItemStack{
		slotKey(listTypeAccountCargo, 2): pvfMoveStack(3330, 10, 1000),
	})
	saveTestInventory(t, ctx, repos, dnfrepo.InventoryRecord{CharacterID: "77"})

	move := NewMoveCommand(alignedcmd.Request{
		AccountID:           "dnf:locked-slot",
		SelectedCharacterID: 77,
	}, MoveItemspaceRequest{
		SourceListType:       listTypeAccountCargo,
		SourceSlotIndex:      2,
		MoveCount:            10,
		DestinationListType:  listTypeAccountCargo,
		DestinationSlotIndex: 28,
	})
	owner, err := NewOwner(repos)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := owner.Move(ctx, move); !errors.Is(err, ErrMoveDestinationFull) {
		t.Fatalf("Move error = %v, 期望 ErrMoveDestinationFull（回原位）", err)
	}
	cargo, _, _ := repos.AccountInventory.Load(ctx, "dnf:locked-slot")
	if _, exists := cargo.Slots[slotKey(listTypeAccountCargo, 28)]; exists {
		t.Fatalf("未解锁格不能被写入: %+v", cargo.Slots)
	}
	if stayed := cargo.Slots[slotKey(listTypeAccountCargo, 2)]; stayed.Count != 10 {
		t.Fatalf("物品应停在原格 12:2: %+v", cargo.Slots)
	}
}

// TestHandlerAccountCargoSameContainerMoveToLockedSlotAnswersWithFailureFrame 同一个现场走
// handler：客户端必须收到失败帧，物品才会弹回原位置（静默会让拖拽 pending 卡住）。
func TestHandlerAccountCargoSameContainerMoveToLockedSlotAnswersWithFailureFrame(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	saveLockedSlotAccountCargoFixture(t, ctx, repos, "dnf:locked-slot", map[string]dnfrepo.ItemStack{
		slotKey(listTypeAccountCargo, 2): pvfMoveStack(3330, 10, 1000),
	})
	saveTestInventory(t, ctx, repos, dnfrepo.InventoryRecord{CharacterID: "77"})

	got, err := NewHandler().Handle(ctx, alignedcmd.Request{
		Opcode:              uint16(dnfenum.CmdPacketMoveItemspace),
		Body:                currentMoveItemspaceBody(listTypeAccountCargo, 2, 0, 10, listTypeAccountCargo, 28, 0, 0, -1),
		AccountID:           "dnf:locked-slot",
		SelectedCharacterID: 77,
		Repositories:        repos,
	})
	if err != nil {
		t.Fatalf("Handle error = %v", err)
	}
	if !got.Handled || !got.ResponseAllowed || len(got.UpperResponses) != 1 {
		t.Fatalf("必须回失败帧让物品弹回: %+v", got)
	}
	wantFailure := []byte{0, moveItemspaceDestinationFullErrorCode, listTypeAccountCargo, listTypeAccountCargo}
	if body := got.UpperResponses[0].Body; !reflect.DeepEqual(body, wantFailure) {
		t.Fatalf("failure ack=% X, want % X", body, wantFailure)
	}
	cargo, _, _ := repos.AccountInventory.Load(ctx, "dnf:locked-slot")
	if len(cargo.Slots) != 1 || cargo.Slots[slotKey(listTypeAccountCargo, 2)].Count != 10 {
		t.Fatalf("数据不应变化: %+v", cargo.Slots)
	}
}

// TestHandlerAccountCargoInternalReverseMoveToLockedSlotSnapsBack 复现现场 2026-09-18
// （金库内部）：客户端把**已有物品**拖到未解锁栏位时，把落点写在 source 端——服务端看到的是
// src=(12,27) 空格 + dst=(12,3) 物品，走 reverse_move 分支。旧代码直接把物品放进 12:27。
// 按运营口径：金库内拖动落未解锁栏位 → 直接回原位（失败帧），不顺延。
func TestHandlerAccountCargoInternalReverseMoveToLockedSlotSnapsBack(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	saveLockedSlotAccountCargoFixture(t, ctx, repos, "dnf:locked-slot", map[string]dnfrepo.ItemStack{
		slotKey(listTypeAccountCargo, 3): pvfMoveStack(3330, 4000, 0),
	})
	saveTestInventory(t, ctx, repos, dnfrepo.InventoryRecord{CharacterID: "77"})

	got, err := NewHandler().Handle(ctx, alignedcmd.Request{
		Opcode: uint16(dnfenum.CmdPacketMoveItemspace),
		// 与线上抓包同形：src 是拖到的未解锁格（服务端那里是空的），dst 是物品所在格。
		Body:                currentMoveItemspaceBody(listTypeAccountCargo, 27, 0, 0, listTypeAccountCargo, 3, 0x1D34E451, 0, -1),
		AccountID:           "dnf:locked-slot",
		SelectedCharacterID: 77,
		Repositories:        repos,
	})
	if err != nil {
		t.Fatalf("Handle error = %v", err)
	}
	if !got.Handled || !got.ResponseAllowed || len(got.UpperResponses) != 1 {
		t.Fatalf("必须回失败帧让物品弹回: %+v", got)
	}
	wantFailure := []byte{0, moveItemspaceDestinationFullErrorCode, listTypeAccountCargo, listTypeAccountCargo}
	if body := got.UpperResponses[0].Body; !reflect.DeepEqual(body, wantFailure) {
		t.Fatalf("failure ack=% X, want % X", body, wantFailure)
	}
	cargo, _, _ := repos.AccountInventory.Load(ctx, "dnf:locked-slot")
	if _, exists := cargo.Slots[slotKey(listTypeAccountCargo, 27)]; exists {
		t.Fatalf("未解锁格不能被写入: %+v", cargo.Slots)
	}
	if stayed := cargo.Slots[slotKey(listTypeAccountCargo, 3)]; stayed.ItemID != 3330 || stayed.Count != 4000 {
		t.Fatalf("物品应停在原格 12:3: %+v", cargo.Slots)
	}
	if len(cargo.Slots) != 1 {
		t.Fatalf("不应产生新行: %+v", cargo.Slots)
	}
}

// TestHandlerAccountCargoInternalReverseMoveToLockedSlotStacksOntoExistingRow 反向 + 已解锁区
// 已有同 ID 行：同样按"金库内拖动"口径直接回原位，不叠加、不新开行。
func TestHandlerAccountCargoInternalReverseMoveToLockedSlotStacksOntoExistingRow(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	saveLockedSlotAccountCargoFixture(t, ctx, repos, "dnf:locked-slot", map[string]dnfrepo.ItemStack{
		slotKey(listTypeAccountCargo, 5): pvfMoveStack(3330, 3, 1000),
		slotKey(listTypeAccountCargo, 3): pvfMoveStack(3330, 5, 1000),
	})
	saveTestInventory(t, ctx, repos, dnfrepo.InventoryRecord{CharacterID: "77"})

	got, err := NewHandler().Handle(ctx, alignedcmd.Request{
		Opcode:              uint16(dnfenum.CmdPacketMoveItemspace),
		Body:                currentMoveItemspaceBody(listTypeAccountCargo, 27, 0, 0, listTypeAccountCargo, 3, 0x1D34E451, 0, -1),
		AccountID:           "dnf:locked-slot",
		SelectedCharacterID: 77,
		Repositories:        repos,
	})
	if err != nil {
		t.Fatalf("Handle error = %v", err)
	}
	wantFailure := []byte{0, moveItemspaceDestinationFullErrorCode, listTypeAccountCargo, listTypeAccountCargo}
	if !got.Handled || !got.ResponseAllowed || len(got.UpperResponses) != 1 ||
		!reflect.DeepEqual(got.UpperResponses[0].Body, wantFailure) {
		t.Fatalf("必须回失败帧: %+v", got)
	}
	cargo, _, _ := repos.AccountInventory.Load(ctx, "dnf:locked-slot")
	if _, exists := cargo.Slots[slotKey(listTypeAccountCargo, 27)]; exists {
		t.Fatalf("未解锁格不能被写入: %+v", cargo.Slots)
	}
	if stacked := cargo.Slots[slotKey(listTypeAccountCargo, 5)]; stacked.Count != 3 {
		t.Fatalf("12:5 不应变化: %+v", cargo.Slots)
	}
	if stayed := cargo.Slots[slotKey(listTypeAccountCargo, 3)]; stayed.Count != 5 {
		t.Fatalf("物品应停在原格 12:3: %+v", cargo.Slots)
	}
}

// saveLockedSlotAccountCargoFixture 建一个"账号金库已创建、解锁 8 格"的夹具。
func saveLockedSlotAccountCargoFixture(
	t *testing.T,
	ctx context.Context,
	repos dnfrepo.Group,
	accountID string,
	cargoSlots map[string]dnfrepo.ItemStack,
) {
	t.Helper()
	if err := repos.Account.Save(ctx, dnfrepo.AccountRecord{
		AccountID: accountID,
		Metadata:  map[string]string{"account_cargo_created": "true", "account_cargo_level": "8"},
	}); err != nil {
		t.Fatal(err)
	}
	if cargoSlots != nil {
		if err := repos.AccountInventory.Save(ctx, dnfrepo.AccountInventoryRecord{
			AccountID: accountID,
			Slots:     cargoSlots,
		}); err != nil {
			t.Fatal(err)
		}
	}
}

// accountCargoTestResolver 是"落点 / 容量 / 叠加 / 满仓"这些用例共用的运行时 PVF 桩。
// 它们考的是搬到哪一格、叠不叠、满不满，**不是**物品资格，所以这里统一声明成可入金库的
// [attach type]（free）——与 0.2026-09-21 起的入库白名单（free / account / sealing）一致。
// limits 按 item id 给出 [stack limit]，与夹具 pvfMoveStack 写进 Extra 的取值同义；不在表里
// 的 item 回"不是可叠加类别"（装备一类的形状）。资格判定本身由
// account_cargo_eligibility_test.go 的用例单独覆盖，不要拿这个桩去断言放行/拒绝。
func accountCargoTestResolver(limits map[int64]int64) alignedcmd.StackMergeResolver {
	return func(itemID int64) (alignedcmd.StackMergeResolution, error) {
		resolution := alignedcmd.StackMergeResolution{ItemID: itemID, AttachType: "`[free]`"}
		if limit := limits[itemID]; limit > 0 {
			resolution.Kind = "stackable"
			resolution.PVFPath = "stackable/material/test_3227.stk"
			resolution.StackableType = "[material]"
			resolution.StackLimit = limit
		}
		return resolution, nil
	}
}

// accountCargoTestFreeResolver 给只需要"这件道具能进金库"、不关心堆叠契约的用例用。
func accountCargoTestFreeResolver() alignedcmd.StackMergeResolver {
	return accountCargoTestResolver(nil)
}

// TestHandlerAccountCargoLockedSlotMoveLandsInFirstUnlockedSlot 运营口径 2026-09-18：
// 把道具拖进账号金库**未解锁**的 12:28 时不再拒绝，而是顺延到已解锁区第一个空格
// （第 0 格被别的道具占着 → 落到第 1 格），ACK 必须回显真实落点。
func TestHandlerAccountCargoLockedSlotMoveLandsInFirstUnlockedSlot(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	saveLockedSlotAccountCargoFixture(t, ctx, repos, "dnf:locked-slot", map[string]dnfrepo.ItemStack{
		slotKey(listTypeAccountCargo, 0): pvfMoveStack(3331, 1, 0),
	})
	saveTestInventory(t, ctx, repos, dnfrepo.InventoryRecord{
		CharacterID: "77",
		Slots: map[string]dnfrepo.ItemStack{
			slotKey(listTypeMain, 69): pvfMoveStack(3330, 1, 0),
		},
	})

	got, err := NewHandler().Handle(ctx, alignedcmd.Request{
		Opcode:              uint16(dnfenum.CmdPacketMoveItemspace),
		Body:                currentMoveItemspaceBody(listTypeMain, 69, 0, 1, listTypeAccountCargo, 28, 0, 0, -1),
		AccountID:           "dnf:locked-slot",
		SelectedCharacterID: 77,
		Repositories:        repos,
		StackMergeResolver:  accountCargoTestFreeResolver(),
	})
	if err != nil {
		t.Fatalf("Handle error = %v", err)
	}
	if !got.Handled || !got.ResponseAllowed || len(got.UpperResponses) != 1 {
		t.Fatalf("result = %+v", got)
	}
	srcList, srcSlot, dstList, dstSlot := parseMoveSuccessAckEndpoints(t, got.UpperResponses[0].Body)
	if srcList != listTypeMain || srcSlot != 69 || dstList != listTypeAccountCargo || dstSlot != 1 {
		t.Fatalf("ACK 端点 = (%d,%d)->(%d,%d), 期望 (0,69)->(12,1)", srcList, srcSlot, dstList, dstSlot)
	}
	cargo, found, err := repos.AccountInventory.Load(ctx, "dnf:locked-slot")
	if err != nil || !found {
		t.Fatalf("load cargo found=%t err=%v", found, err)
	}
	if _, exists := cargo.Slots[slotKey(listTypeAccountCargo, 28)]; exists {
		t.Fatalf("未解锁格不能被写入: %+v", cargo.Slots)
	}
	if landed := cargo.Slots[slotKey(listTypeAccountCargo, 1)]; landed.ItemID != 3330 || landed.Count != 1 {
		t.Fatalf("落点应为 12:1 的 3330x1: %+v", landed)
	}
	loaded := loadTestInventory(t, ctx, repos, "77")
	if _, exists := loaded.Slots[slotKey(listTypeMain, 69)]; exists {
		t.Fatalf("源格应被搬空: %+v", loaded.Slots)
	}
}

// TestHandlerAccountCargoLockedSlotMoveStacksOntoExistingRow 金库里已有同 ID 行 → 叠加进那一行，
// 不新开第二行、也不落到未解锁格。
func TestHandlerAccountCargoLockedSlotMoveStacksOntoExistingRow(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	saveLockedSlotAccountCargoFixture(t, ctx, repos, "dnf:locked-slot", map[string]dnfrepo.ItemStack{
		slotKey(listTypeAccountCargo, 3): pvfMoveStack(3330, 3, 1000),
	})
	saveTestInventory(t, ctx, repos, dnfrepo.InventoryRecord{
		CharacterID: "77",
		Slots: map[string]dnfrepo.ItemStack{
			slotKey(listTypeMain, 69): pvfMoveStack(3330, 5, 1000),
		},
	})

	got, err := NewHandler().Handle(ctx, alignedcmd.Request{
		Opcode:              uint16(dnfenum.CmdPacketMoveItemspace),
		Body:                currentMoveItemspaceBody(listTypeMain, 69, 0, 5, listTypeAccountCargo, 28, 0, 0, -1),
		AccountID:           "dnf:locked-slot",
		SelectedCharacterID: 77,
		Repositories:        repos,
		StackMergeResolver:  accountCargoTestResolver(map[int64]int64{3330: 1000}),
	})
	if err != nil {
		t.Fatalf("Handle error = %v", err)
	}
	if !got.Handled || !got.ResponseAllowed || len(got.UpperResponses) != 1 {
		t.Fatalf("result = %+v", got)
	}
	_, _, _, dstSlot := parseMoveSuccessAckEndpoints(t, got.UpperResponses[0].Body)
	if dstSlot != 3 {
		t.Fatalf("ACK 落点 = %d, 期望叠加进 12:3", dstSlot)
	}
	cargo, _, _ := repos.AccountInventory.Load(ctx, "dnf:locked-slot")
	if stacked := cargo.Slots[slotKey(listTypeAccountCargo, 3)]; stacked.Count != 8 {
		t.Fatalf("12:3 应叠加到 8: %+v", stacked)
	}
	if len(cargo.Slots) != 1 {
		t.Fatalf("同 ID 只能有一行: %+v", cargo.Slots)
	}
}

// TestHandlerAccountCargoLockedSlotMoveRejectsWhenCarryLimitReached 金库里同 ID 已经堆到
// PVF [stack limit] 上限 → 必须回拒绝提示（不是把新的一堆塞进未解锁格）。
func TestHandlerAccountCargoLockedSlotMoveRejectsWhenCarryLimitReached(t *testing.T) {
	const limitOneTicket int64 = 10008067
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	saveLockedSlotAccountCargoFixture(t, ctx, repos, "dnf:locked-slot", map[string]dnfrepo.ItemStack{
		slotKey(listTypeAccountCargo, 3): pvfMoveStack(limitOneTicket, 1, 1),
	})
	saveTestInventory(t, ctx, repos, dnfrepo.InventoryRecord{
		CharacterID: "77",
		Slots: map[string]dnfrepo.ItemStack{
			slotKey(listTypeMain, 69): pvfMoveStack(limitOneTicket, 1, 1),
		},
	})

	got, err := NewHandler().Handle(ctx, alignedcmd.Request{
		Opcode:              uint16(dnfenum.CmdPacketMoveItemspace),
		Body:                currentMoveItemspaceBody(listTypeMain, 69, 0, 1, listTypeAccountCargo, 28, 0, 0, -1),
		AccountID:           "dnf:locked-slot",
		SelectedCharacterID: 77,
		Repositories:        repos,
		StackMergeResolver:  accountCargoTestResolver(map[int64]int64{limitOneTicket: 1}),
	})
	if err != nil {
		t.Fatalf("Handle error = %v", err)
	}
	if !got.Handled || !got.ResponseAllowed || len(got.UpperResponses) != 1 {
		t.Fatalf("超上限必须回失败帧: %+v", got)
	}
	wantFailure := []byte{0, moveItemspaceDestinationFullErrorCode, listTypeMain, listTypeAccountCargo}
	if body := got.UpperResponses[0].Body; !reflect.DeepEqual(body, wantFailure) {
		t.Fatalf("failure ack=% X, want % X", body, wantFailure)
	}
	cargo, _, _ := repos.AccountInventory.Load(ctx, "dnf:locked-slot")
	if len(cargo.Slots) != 1 || cargo.Slots[slotKey(listTypeAccountCargo, 3)].Count != 1 {
		t.Fatalf("拒绝后金库不应变化: %+v", cargo.Slots)
	}
	if _, exists := cargo.Slots[slotKey(listTypeAccountCargo, 28)]; exists {
		t.Fatalf("未解锁格不能被写入: %+v", cargo.Slots)
	}
	loaded := loadTestInventory(t, ctx, repos, "77")
	if source := loaded.Slots[slotKey(listTypeMain, 69)]; source.Count != 1 {
		t.Fatalf("拒绝后源行不应变化: %+v", source)
	}
}
