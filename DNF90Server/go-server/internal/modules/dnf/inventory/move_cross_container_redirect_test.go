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
	dnfitemlock "longheng.io/server/internal/modules/dnf/itemlock"
	dnfproto "longheng.io/server/internal/modules/dnf/protocol"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

// TestHandlerBagToCargoRedirectsOccupiedDestinationIntoUnlockedEmptySlot 锁住
// "背包→仓库拖到客户端认为空、服务端却放着别的物品的格子"这条回归：物品必须落到目标
// 容器里另一个已解锁的空格，op19 ACK 必须报真实落点，并且不能附带整 list 刷新
// （整 list 刷新会把 list-0 的金币钱包行带歪）。
func TestHandlerBagToCargoRedirectsOccupiedDestinationIntoUnlockedEmptySlot(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	saveTestInventory(t, ctx, repos, dnfrepo.InventoryRecord{
		CharacterID: "77",
		Slots: map[string]dnfrepo.ItemStack{
			slotKey(listTypeMain, 65): pvfMoveStack(3330, 5, 0),
		},
		Warehouse: map[string]dnfrepo.ItemStack{
			// 声明格 2:3 里是"别的物品"，所以不能就地合并，只能另找一个空格。
			slotKey(listTypePersonalCargo, 3): pvfMoveStack(3331, 20, 0),
		},
	})
	saveMoveContainerState(t, ctx, repos, "77", 24, 8)

	got, err := NewHandler().Handle(ctx, alignedcmd.Request{
		Opcode: uint16(dnfenum.CmdPacketMoveItemspace),
		// 客户端声明目标格 2:3 是空的：DestinationInstance 与 DestinationStack 都是 0。
		Body:                currentMoveItemspaceBody(listTypeMain, 65, 0, 5, listTypePersonalCargo, 3, 0, 0, -1),
		AccountID:           "dnf:1",
		SelectedCharacterID: 77,
		Repositories:        repos,
	})
	if err != nil {
		t.Fatalf("Handle error = %v", err)
	}
	if !got.Handled || !got.ResponseAllowed || len(got.UpperResponses) != 1 {
		t.Fatalf("result = %+v", got)
	}
	ack := got.UpperResponses[0]
	if ack.MsgID != uint16(dnfenum.CmdPacketMoveItemspace) || ack.Classification != 1 {
		t.Fatalf("ACK header = %+v", ack)
	}
	// 真实落点是 2:0（cargo 已解锁的最低空格），不是客户端声明的 2:3。
	wantACK := []byte{1, listTypeMain, 65, 0, 5, 0, 0, 0, listTypePersonalCargo, 0, 0}
	if !reflect.DeepEqual(ack.Body, wantACK) {
		t.Fatalf("ACK body = % X, want % X", ack.Body, wantACK)
	}
	if len(got.ItemSlotRefreshes) != 0 {
		t.Fatalf("item slot refreshes = %+v, want none", got.ItemSlotRefreshes)
	}
	if !reflect.DeepEqual(got.PostActions, []alignedcmd.PostAction{alignedcmd.PostActionRefreshSelectedMainWallets}) {
		t.Fatalf("post actions = %+v", got.PostActions)
	}

	loaded := loadTestInventory(t, ctx, repos, "77")
	if _, exists := loaded.Slots[slotKey(listTypeMain, 65)]; exists {
		t.Fatalf("source still exists: %+v", loaded.Slots)
	}
	if got := loaded.Warehouse[slotKey(listTypePersonalCargo, 3)].ItemID; got != 3331 {
		t.Fatalf("declared destination changed: %+v", loaded.Warehouse)
	}
	redirected := loaded.Warehouse[slotKey(listTypePersonalCargo, 0)]
	if redirected.ItemID != 3330 || redirected.Count != 5 {
		t.Fatalf("redirected stack = %+v, want item 3330 count 5 in cargo slot 0", redirected)
	}
}

// TestOwnerCrossContainerOccupiedDestinationRedirectFallbacks 覆盖跨容器落点修正的
// 其余分支：没有空格时退到同类可叠格、客户端明确声明目标格有物品时保持就地合并、
// 以及拿不到容器状态时 fail-close 不改落点。
func TestOwnerCrossContainerOccupiedDestinationRedirectFallbacks(t *testing.T) {
	tests := []struct {
		name                    string
		destinationStack        int32
		withContainerState      bool
		fillCargo               bool
		declaredItemID          int64
		wantErr                 error
		wantMode                string
		wantDestinationSlot     int16
		wantRedirected          bool
		wantDeclaredSlotCount   int64
		wantOtherStackSlotCount int64
		wantEmptySlotCount      int64
	}{
		{
			// 声明格自己就是同类可叠格 -> 就地合并，绝不新开第二行。
			name:                  "same item at the declared cell merges there",
			withContainerState:    true,
			wantMode:              "stack",
			wantDestinationSlot:   3,
			wantRedirected:        true,
			wantDeclaredSlotCount: 25,
		},
		{
			// 声明格是别的物品，容器里另有一行同类 -> 并进那一行。
			name:                    "another compatible stack in the container takes it",
			withContainerState:      true,
			fillCargo:               true,
			declaredItemID:          3331,
			wantMode:                "stack",
			wantDestinationSlot:     5,
			wantRedirected:          true,
			wantDeclaredSlotCount:   20,
			wantOtherStackSlotCount: 35,
			wantEmptySlotCount:      1,
		},
		{
			// 客户端明确声明目标格里有那件物品（DestinationStack 非 0）-> 走原路径就地合并。
			name:                    "client-declared occupied destination keeps merging in place",
			destinationStack:        20,
			withContainerState:      true,
			fillCargo:               true,
			wantMode:                "stack",
			wantDestinationSlot:     3,
			wantDeclaredSlotCount:   25,
			wantOtherStackSlotCount: 30,
			wantEmptySlotCount:      1,
		},
		{
			// 声明格是别的物品、容器里也没有同类行、又读不到容器状态 -> 只能明确拒绝。
			name:           "different item without container state is rejected",
			declaredItemID: 3331,
			wantErr:        ErrMoveDestinationFull,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			repos := dnfrepomemory.NewMemoryGroup()
			declaredItemID := test.declaredItemID
			if declaredItemID == 0 {
				declaredItemID = 3330
			}
			record := dnfrepo.InventoryRecord{
				CharacterID: "77",
				Slots: map[string]dnfrepo.ItemStack{
					slotKey(listTypeMain, 65): pvfMoveStack(3330, 5, 0),
				},
				Warehouse: map[string]dnfrepo.ItemStack{
					slotKey(listTypePersonalCargo, 3): pvfMoveStack(declaredItemID, 20, 0),
				},
			}
			if test.fillCargo {
				// 8 个已解锁仓库格全满，逼出"同类可叠格"回退分支。
				for _, slot := range []int16{0, 1, 2, 4, 6, 7} {
					record.Warehouse[slotKey(listTypePersonalCargo, slot)] = pvfMoveStack(3331, 1, 10)
				}
				record.Warehouse[slotKey(listTypePersonalCargo, 3)] = pvfMoveStack(declaredItemID, 20, 0)
				record.Warehouse[slotKey(listTypePersonalCargo, 5)] = pvfMoveStack(3330, 30, 0)
			}
			saveTestInventory(t, ctx, repos, record)
			if test.withContainerState {
				saveMoveContainerState(t, ctx, repos, "77", 24, 8)
			}
			owner, err := NewOwner(repos)
			if err != nil {
				t.Fatalf("NewOwner error = %v", err)
			}

			result, err := owner.Move(ctx, NewMoveCommand(alignedcmd.Request{
				AccountID:           "dnf:1",
				SelectedCharacterID: 77,
			}, MoveItemspaceRequest{
				SourceListType:           listTypeMain,
				SourceSlotIndex:          65,
				MoveCount:                5,
				DestinationListType:      listTypePersonalCargo,
				DestinationSlotIndex:     3,
				DestinationInstanceValue: 0,
				DestinationStack:         test.destinationStack,
			}))
			if test.wantErr != nil {
				if !errors.Is(err, test.wantErr) {
					t.Fatalf("Move error = %v, want %v", err, test.wantErr)
				}
				loaded := loadTestInventory(t, ctx, repos, "77")
				if source := loaded.Slots[slotKey(listTypeMain, 65)]; source.ItemID != 3330 || source.Count != 5 {
					t.Fatalf("source changed after rejection: %+v", source)
				}
				return
			}
			if err != nil {
				t.Fatalf("Move error = %v", err)
			}
			if !result.Changed || result.MoveCount != 5 || result.Mode != test.wantMode ||
				result.DestinationSlotIndex != test.wantDestinationSlot ||
				result.RedirectedReservedMainSlot != test.wantRedirected {
				t.Fatalf("result = %+v, want mode=%s slot=%d redirected=%t",
					result, test.wantMode, test.wantDestinationSlot, test.wantRedirected)
			}
			// 修正后的落点已经写进 ACK，不能再叠加整 list 刷新。
			if len(result.RefreshListTypes) != 0 || len(result.Refresh) != 0 {
				t.Fatalf("refresh = %v lists=%v, want none", result.Refresh, result.RefreshListTypes)
			}

			loaded := loadTestInventory(t, ctx, repos, "77")
			if _, exists := loaded.Slots[slotKey(listTypeMain, 65)]; exists {
				t.Fatalf("source still exists: %+v", loaded.Slots)
			}
			if got := loaded.Warehouse[slotKey(listTypePersonalCargo, 3)].Count; got != test.wantDeclaredSlotCount {
				t.Fatalf("declared destination count = %d, want %d", got, test.wantDeclaredSlotCount)
			}
			if got := loaded.Warehouse[slotKey(listTypePersonalCargo, 5)].Count; got != test.wantOtherStackSlotCount {
				t.Fatalf("other compatible stack count = %d, want %d", got, test.wantOtherStackSlotCount)
			}
			if got := loaded.Warehouse[slotKey(listTypePersonalCargo, 0)].Count; got != test.wantEmptySlotCount {
				t.Fatalf("cargo slot 0 count = %d, want %d", got, test.wantEmptySlotCount)
			}
		})
	}
}

// TestHandlerCrossContainerFullDestinationFailsWithSpaceError 锁住"目标容器满了"这条
// 规则：跨容器移动时目标格被别的物品占着、容器里又没有空格/同类可叠格，就必须回一个
// 明确的失败包（客户端弹"物品栏空间不足"），不能偷偷互换 —— 互换时被换出去的那件
// 客户端不知道去了哪，会"消失"到小退；也不能不回包（现场就是"没提示也放不进去"）。
func TestHandlerCrossContainerFullDestinationFailsWithSpaceError(t *testing.T) {
	tests := []struct {
		name             string
		destinationSlot  int16
		destinationStack int32
	}{
		{name: "client declared the cell empty", destinationSlot: 3},
		{name: "client declared the occupant", destinationSlot: 3, destinationStack: 1},
		{name: "client targets a visible different item", destinationSlot: 5, destinationStack: 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			repos := dnfrepomemory.NewMemoryGroup()
			record := dnfrepo.InventoryRecord{
				CharacterID: "77",
				Slots: map[string]dnfrepo.ItemStack{
					slotKey(listTypeMain, 65): pvfMoveStack(3330, 5, 0),
				},
				Warehouse: make(map[string]dnfrepo.ItemStack),
			}
			// 8 个已解锁仓库格全满，其中没有一件与源物品同类。
			for slot := int16(0); slot < 8; slot++ {
				record.Warehouse[slotKey(listTypePersonalCargo, slot)] = pvfMoveStack(3331, 1, 10)
			}
			saveTestInventory(t, ctx, repos, record)
			saveMoveContainerState(t, ctx, repos, "77", 24, 8)

			got, err := NewHandler().Handle(ctx, alignedcmd.Request{
				Opcode: uint16(dnfenum.CmdPacketMoveItemspace),
				Body: currentMoveItemspaceBody(
					listTypeMain, 65, 0, 5,
					listTypePersonalCargo, test.destinationSlot, 0, test.destinationStack, -1,
				),
				AccountID:           "dnf:1",
				SelectedCharacterID: 77,
				Repositories:        repos,
			})
			if err != nil {
				t.Fatalf("Handle error = %v", err)
			}
			if !got.Handled || !got.ResponseAllowed || len(got.UpperResponses) != 1 {
				t.Fatalf("result = %+v", got)
			}
			ack := got.UpperResponses[0]
			wantACK := []byte{0, moveItemspaceDestinationFullErrorCode, listTypeMain, listTypePersonalCargo}
			if ack.MsgID != uint16(dnfenum.CmdPacketMoveItemspace) || ack.Classification != 1 ||
				!reflect.DeepEqual(ack.Body, wantACK) {
				t.Fatalf("ACK = %+v body=% X, want % X", ack, ack.Body, wantACK)
			}

			loaded := loadTestInventory(t, ctx, repos, "77")
			if source := loaded.Slots[slotKey(listTypeMain, 65)]; source.ItemID != 3330 || source.Count != 5 {
				t.Fatalf("source changed after rejection: %+v", source)
			}
			for slot := int16(0); slot < 8; slot++ {
				stack := loaded.Warehouse[slotKey(listTypePersonalCargo, slot)]
				if stack.ItemID != 3331 || stack.Count != 1 {
					t.Fatalf("cargo slot %d changed after rejection: %+v", slot, stack)
				}
			}
			for key := range loaded.Slots {
				if key != slotKey(listTypeMain, 65) {
					t.Fatalf("unexpected bag row after rejection: %s", key)
				}
			}
		})
	}
}

// TestHandlerAccountCargoSingleSlotRejectsSecondDepositWithoutLosingTheFirst 复现用户报告的
// "账号金库只有一个格子，放第二件时原来那件消失、小退才回到背包"：容量一格且已被占，
// 再来一件不同物品时必须回失败包、两边都不动，绝不能互换。
func TestHandlerAccountCargoSingleSlotRejectsSecondDepositWithoutLosingTheFirst(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	if err := repos.Account.Save(ctx, dnfrepo.AccountRecord{
		AccountID: "dnf:tiny-cargo",
		Metadata:  map[string]string{"account_cargo_created": "true", "account_cargo_level": "1"},
	}); err != nil {
		t.Fatal(err)
	}
	saveTestInventory(t, ctx, repos, dnfrepo.InventoryRecord{
		CharacterID: "77",
		Slots: map[string]dnfrepo.ItemStack{
			slotKey(listTypeMain, 65): pvfMoveStack(3330, 1, 0),
		},
	})
	if err := repos.AccountInventory.Save(ctx, dnfrepo.AccountInventoryRecord{
		AccountID: "dnf:tiny-cargo",
		Slots: map[string]dnfrepo.ItemStack{
			slotKey(listTypeAccountCargo, 0): pvfMoveStack(3331, 1, 0),
		},
	}); err != nil {
		t.Fatal(err)
	}

	got, err := NewHandler().Handle(ctx, alignedcmd.Request{
		Opcode:              uint16(dnfenum.CmdPacketMoveItemspace),
		Body:                currentMoveItemspaceBody(listTypeMain, 65, 0, 1, listTypeAccountCargo, 0, 0x24, 0, -1),
		AccountID:           "dnf:tiny-cargo",
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
	wantACK := []byte{0, moveItemspaceDestinationFullErrorCode, listTypeMain, listTypeAccountCargo}
	if ack := got.UpperResponses[0]; !reflect.DeepEqual(ack.Body, wantACK) {
		t.Fatalf("ACK body=% X, want % X", ack.Body, wantACK)
	}

	loaded := loadTestInventory(t, ctx, repos, "77")
	if source := loaded.Slots[slotKey(listTypeMain, 65)]; source.ItemID != 3330 || source.Count != 1 {
		t.Fatalf("bag source changed after rejection: %+v", source)
	}
	cargo, found, err := repos.AccountInventory.Load(ctx, "dnf:tiny-cargo")
	if err != nil || !found {
		t.Fatalf("load cargo found=%t err=%v", found, err)
	}
	if original := cargo.Slots[slotKey(listTypeAccountCargo, 0)]; original.ItemID != 3331 || original.Count != 1 {
		t.Fatalf("account cargo row changed after rejection: %+v", original)
	}
}

// TestHandlerCrossContainerAutoStackReportsRealLandingSlotWithoutListRefresh 复现用户 05:40:28
// 那次抓包：背包拖进仓库空格、仓库里已有同类堆，物品实际并进那一堆。ACK 必须报真实
// 落点，并且不能再下发整 list（整 list 会把 list-0 的金币钱包行带下去，现场就是
// "获得了 N 金币" + 物品行错位）。
func TestHandlerCrossContainerAutoStackReportsRealLandingSlotWithoutListRefresh(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	saveTestInventory(t, ctx, repos, dnfrepo.InventoryRecord{
		CharacterID: "77",
		Slots: map[string]dnfrepo.ItemStack{
			slotKey(listTypeMain, 65): pvfMoveStack(3330, 5, 0),
		},
		Warehouse: map[string]dnfrepo.ItemStack{
			slotKey(listTypePersonalCargo, 4): pvfMoveStack(3330, 30, 0),
		},
	})
	saveMoveContainerState(t, ctx, repos, "77", 24, 8)

	got, err := NewHandler().Handle(ctx, alignedcmd.Request{
		Opcode: uint16(dnfenum.CmdPacketMoveItemspace),
		// 客户端拖到 2:7（服务端是空格），仓库里 2:4 已有同一件物品。
		Body:                currentMoveItemspaceBody(listTypeMain, 65, 0, 5, listTypePersonalCargo, 7, 0, 0, -1),
		AccountID:           "dnf:1",
		SelectedCharacterID: 77,
		Repositories:        repos,
	})
	if err != nil {
		t.Fatalf("Handle error = %v", err)
	}
	if !got.Handled || !got.ResponseAllowed || len(got.UpperResponses) != 1 {
		t.Fatalf("result = %+v, want a single ACK and no list refresh", got)
	}
	wantACK := []byte{1, listTypeMain, 65, 0, 5, 0, 0, 0, listTypePersonalCargo, 4, 0}
	if ack := got.UpperResponses[0]; !reflect.DeepEqual(ack.Body, wantACK) {
		t.Fatalf("ACK body=% X, want % X", ack.Body, wantACK)
	}
	wantRefreshes := []alignedcmd.ItemSlotRefresh{
		{ListType: listTypeMain, SlotIndex: 65},
		{ListType: listTypePersonalCargo, SlotIndex: 4},
	}
	if !reflect.DeepEqual(got.ItemSlotRefreshes, wantRefreshes) {
		t.Fatalf("item slot refreshes = %+v, want %+v", got.ItemSlotRefreshes, wantRefreshes)
	}
	if !reflect.DeepEqual(got.PostActions, []alignedcmd.PostAction{alignedcmd.PostActionRefreshSelectedMainWallets}) {
		t.Fatalf("post actions = %+v", got.PostActions)
	}

	loaded := loadTestInventory(t, ctx, repos, "77")
	if _, exists := loaded.Slots[slotKey(listTypeMain, 65)]; exists {
		t.Fatalf("source still exists: %+v", loaded.Slots)
	}
	if _, exists := loaded.Warehouse[slotKey(listTypePersonalCargo, 7)]; exists {
		t.Fatalf("declared empty cell was populated: %+v", loaded.Warehouse)
	}
	if merged := loaded.Warehouse[slotKey(listTypePersonalCargo, 4)]; merged.Count != 35 {
		t.Fatalf("merged stack = %+v, want count 35", merged)
	}
}

// TestHandlerCarryLimitIsPerContainerTotal 锁住 [stack limit] 的容器总量口径：
// 同一个容器里该道具总量不得超过 [stack limit]，上限 1 就代表容器里只能有一件
// （同 id 只有一行），不能靠"另找空格"绕过去；PVF 没写 [stack limit] 的不限。
func TestHandlerCarryLimitIsPerContainerTotal(t *testing.T) {
	const limitOneTicket int64 = 10008067
	tests := []struct {
		name              string
		itemID            int64
		stackLimit        int64
		moveCount         int32
		warehouseSlot     int16
		warehouseCount    int64
		warehouseLimit    int64
		wantRejected      bool
		wantDestination   int16
		wantWarehouseLeft int64
	}{
		{
			name: "limit 1 rejects a second copy even into an empty cell",
			// 背包 1 件、仓库 1 件：再存一件超上限 -> 直接拒绝，空格也不行。
			itemID: limitOneTicket, stackLimit: 1, moveCount: 1,
			warehouseSlot: 3, warehouseCount: 1, warehouseLimit: 1,
			wantRejected: true, wantWarehouseLeft: 1,
		},
		{
			name: "limit 1 accepts the first copy of that container",
			// 仓库里没有这件道具 -> 正常落进空格。
			itemID: limitOneTicket, stackLimit: 1, moveCount: 1,
			warehouseSlot: 3, warehouseCount: 0, warehouseLimit: 1,
			wantDestination: 4, wantWarehouseLeft: 1,
		},
		{
			name: "limit 1000 still has room for the remaining amount",
			// 仓库已有 700，再存 300 -> 正好到 1000，允许并进那一行。
			itemID: 3330, stackLimit: 1000, moveCount: 300,
			warehouseSlot: 3, warehouseCount: 700, warehouseLimit: 1000,
			wantDestination: 3, wantWarehouseLeft: 1000,
		},
		{
			name:   "limit 1000 rejects the move that would exceed it",
			itemID: 3330, stackLimit: 1000, moveCount: 301,
			warehouseSlot: 3, warehouseCount: 700, warehouseLimit: 1000,
			wantRejected: true, wantWarehouseLeft: 700,
		},
		{
			name: "no [stack limit] is unrestricted",
			// PVF 没写 [stack limit]（装备一类）-> 不参与这条规则。
			itemID: 3330, stackLimit: 0, moveCount: 5,
			warehouseSlot: 3, warehouseCount: 5, warehouseLimit: 0,
			wantDestination: 3, wantWarehouseLeft: 10,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			repos := dnfrepomemory.NewMemoryGroup()
			record := dnfrepo.InventoryRecord{
				CharacterID: "77",
				Slots: map[string]dnfrepo.ItemStack{
					slotKey(listTypeMain, 65): pvfMoveStack(test.itemID, int64(test.moveCount), test.stackLimit),
				},
				Warehouse: make(map[string]dnfrepo.ItemStack),
			}
			if test.warehouseCount > 0 {
				record.Warehouse[slotKey(listTypePersonalCargo, test.warehouseSlot)] =
					pvfMoveStack(test.itemID, test.warehouseCount, test.warehouseLimit)
			}
			saveTestInventory(t, ctx, repos, record)
			saveMoveContainerState(t, ctx, repos, "77", 24, 8)

			got, err := NewHandler().Handle(ctx, alignedcmd.Request{
				Opcode: uint16(dnfenum.CmdPacketMoveItemspace),
				Body: currentMoveItemspaceBody(
					listTypeMain, 65, 0, test.moveCount,
					listTypePersonalCargo, 4, 0, 0, -1,
				),
				AccountID:           "dnf:1",
				SelectedCharacterID: 77,
				Repositories:        repos,
			})
			if err != nil {
				t.Fatalf("Handle error = %v", err)
			}
			if !got.Handled || !got.ResponseAllowed || len(got.UpperResponses) != 1 {
				t.Fatalf("result = %+v", got)
			}
			ack := got.UpperResponses[0]
			if test.wantRejected {
				wantACK := []byte{0, moveItemspaceDestinationFullErrorCode, listTypeMain, listTypePersonalCargo}
				if !reflect.DeepEqual(ack.Body, wantACK) {
					t.Fatalf("ACK body=% X, want % X", ack.Body, wantACK)
				}
				loaded := loadTestInventory(t, ctx, repos, "77")
				if source := loaded.Slots[slotKey(listTypeMain, 65)]; source.Count != int64(test.moveCount) {
					t.Fatalf("source changed after rejection: %+v", source)
				}
				if _, exists := loaded.Warehouse[slotKey(listTypePersonalCargo, 4)]; exists {
					t.Fatalf("empty cell was populated despite rejection: %+v", loaded.Warehouse)
				}
				if got := loaded.Warehouse[slotKey(listTypePersonalCargo, test.warehouseSlot)].Count; got != test.warehouseCount {
					t.Fatalf("existing row changed after rejection: %d, want %d", got, test.warehouseCount)
				}
				return
			}
			if ack.Body[0] != 1 {
				t.Fatalf("ACK body=% X, want success", ack.Body)
			}
			if got := int16(binary.LittleEndian.Uint16(ack.Body[9:11])); got != test.wantDestination {
				t.Fatalf("ACK destination slot=%d, want %d (body=% X)", got, test.wantDestination, ack.Body)
			}
			loaded := loadTestInventory(t, ctx, repos, "77")
			if _, exists := loaded.Slots[slotKey(listTypeMain, 65)]; exists {
				t.Fatalf("source remained: %+v", loaded.Slots)
			}
			if got := loaded.Warehouse[slotKey(listTypePersonalCargo, test.wantDestination)].Count; got != test.wantWarehouseLeft {
				t.Fatalf("destination count=%d, want %d", got, test.wantWarehouseLeft)
			}
		})
	}
}

// TestHandlerPersonalCargoSameContainerMoveToLockedSlotSnapsBack 仓库内把一堆拖到未解锁格：
// 按运营口径 2026-09-18 **不适用**"顺延到第一个空格"，直接回原位（失败帧），
// 源格绝不能被当成叠加目标（否则数量会多算一份）。
func TestHandlerPersonalCargoSameContainerMoveToLockedSlotSnapsBack(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	saveTestInventory(t, ctx, repos, dnfrepo.InventoryRecord{
		CharacterID: "77",
		Warehouse: map[string]dnfrepo.ItemStack{
			slotKey(listTypePersonalCargo, 3): pvfMoveStack(3330, 10, 1000),
		},
		Slots: map[string]dnfrepo.ItemStack{},
	})
	saveMoveContainerState(t, ctx, repos, "77", 24, 8)

	got, err := NewHandler().Handle(ctx, alignedcmd.Request{
		Opcode:              uint16(dnfenum.CmdPacketMoveItemspace),
		Body:                currentMoveItemspaceBody(listTypePersonalCargo, 3, 0, 10, listTypePersonalCargo, 40, 0, 0, -1),
		AccountID:           "dnf:1",
		SelectedCharacterID: 77,
		Repositories:        repos,
	})
	if err != nil {
		t.Fatalf("Handle error = %v", err)
	}
	if !got.Handled || !got.ResponseAllowed || len(got.UpperResponses) != 1 {
		t.Fatalf("必须回失败帧让物品弹回: %+v", got)
	}
	wantFailure := []byte{0, moveItemspaceDestinationFullErrorCode, listTypePersonalCargo, listTypePersonalCargo}
	if body := got.UpperResponses[0].Body; !reflect.DeepEqual(body, wantFailure) {
		t.Fatalf("failure ack=% X, want % X", body, wantFailure)
	}
	loaded := loadTestInventory(t, ctx, repos, "77")
	if _, exists := loaded.Warehouse[slotKey(listTypePersonalCargo, 40)]; exists {
		t.Fatalf("未解锁格不能被写入: %+v", loaded.Warehouse)
	}
	total := int64(0)
	for _, stack := range loaded.Warehouse {
		if stack.ItemID == 3330 {
			total += stack.Count
		}
	}
	if total != 10 {
		t.Fatalf("物品应停在原格、总量保持 10，实际 %d: %+v", total, loaded.Warehouse)
	}
	if stayed := loaded.Warehouse[slotKey(listTypePersonalCargo, 3)]; stayed.Count != 10 {
		t.Fatalf("物品应停在原格 2:3: %+v", loaded.Warehouse)
	}
}

// TestHandlerPersonalCargoInternalReverseMoveToLockedSlotSnapsBack 和账号金库同一形态：
// 仓库内部把已有物品拖到未解锁格时，客户端把落点写在 source 端（reverse_move），
// 服务端必须回失败帧让物品弹回原位置，不能放进未解锁格、也不顺延。
func TestHandlerPersonalCargoInternalReverseMoveToLockedSlotSnapsBack(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	saveTestInventory(t, ctx, repos, dnfrepo.InventoryRecord{
		CharacterID: "77",
		Slots:       map[string]dnfrepo.ItemStack{},
		Warehouse: map[string]dnfrepo.ItemStack{
			slotKey(listTypePersonalCargo, 3): pvfMoveStack(3330, 10, 1000),
		},
	})
	saveMoveContainerState(t, ctx, repos, "77", 24, 8)

	got, err := NewHandler().Handle(ctx, alignedcmd.Request{
		Opcode:              uint16(dnfenum.CmdPacketMoveItemspace),
		Body:                currentMoveItemspaceBody(listTypePersonalCargo, 40, 0, 0, listTypePersonalCargo, 3, 0x1D34E451, 0, -1),
		AccountID:           "dnf:1",
		SelectedCharacterID: 77,
		Repositories:        repos,
	})
	if err != nil {
		t.Fatalf("Handle error = %v", err)
	}
	if !got.Handled || !got.ResponseAllowed || len(got.UpperResponses) != 1 {
		t.Fatalf("必须回失败帧让物品弹回: %+v", got)
	}
	wantFailure := []byte{0, moveItemspaceDestinationFullErrorCode, listTypePersonalCargo, listTypePersonalCargo}
	if body := got.UpperResponses[0].Body; !reflect.DeepEqual(body, wantFailure) {
		t.Fatalf("failure ack=% X, want % X", body, wantFailure)
	}
	loaded := loadTestInventory(t, ctx, repos, "77")
	if _, exists := loaded.Warehouse[slotKey(listTypePersonalCargo, 40)]; exists {
		t.Fatalf("未解锁格不能被写入: %+v", loaded.Warehouse)
	}
	if stayed := loaded.Warehouse[slotKey(listTypePersonalCargo, 3)]; stayed.ItemID != 3330 || stayed.Count != 10 {
		t.Fatalf("物品应停在原格 2:3: %+v", loaded.Warehouse)
	}
	if len(loaded.Warehouse) != 1 {
		t.Fatalf("不应产生新行: %+v", loaded.Warehouse)
	}
}

func saveMoveContainerState(
	t *testing.T,
	ctx context.Context,
	repos dnfrepo.Group,
	characterID string,
	mainSlotCount uint16,
	personalCargoSlotCount uint16,
) {
	t.Helper()
	if err := repos.Settings.Save(ctx, dnfrepo.SettingsRecord{
		Scope: dnfrepo.CharacterContainerStateScope(characterID),
		Values: map[string]string{
			"source":                      "cross_container_redirect_test",
			"main_list_param16":           strconv.Itoa(int(mainSlotCount)),
			"avatar_list_param16":         "0",
			"personal_cargo_list_param16": strconv.Itoa(int(personalCargoSlotCount)),
			"account_cargo_selection_key": "0",
			"account_cargo_value32":       "0",
		},
		UpdatedAt: time.Unix(1_900_000_000, 0).UTC(),
	}); err != nil {
		t.Fatalf("Save container state error = %v", err)
	}
}

// TestHandlerAccountCargoLockedSlotMoveAnswersWithFailureFrame 复现现场 2026-09-18：把道具拖进
// 账号金库**尚未解锁**的栏位（dst 12:28，容量 8）而金库已解锁格全满时，必须回 op19 失败帧而
// 不是静默——旧代码在这里走 ownerBlockedResult（不回帧），客户端 sub_1D2DE80 的 pending 拖拽
// 对象永远不释放，金库、装备、消耗品窗口全部点不动，客户端之后一个包都不再发。
// （未满时的顺延落点见 account_cargo_test.go 的 locked-slot 用例。）
func TestHandlerAccountCargoLockedSlotMoveAnswersWithFailureFrame(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	if err := repos.Account.Save(ctx, dnfrepo.AccountRecord{
		AccountID: "dnf:locked-slot",
		Metadata:  map[string]string{"account_cargo_created": "true", "account_cargo_level": "8"},
	}); err != nil {
		t.Fatal(err)
	}
	saveTestInventory(t, ctx, repos, dnfrepo.InventoryRecord{
		CharacterID: "77",
		Slots: map[string]dnfrepo.ItemStack{
			slotKey(listTypeMain, 69): pvfMoveStack(3330, 1, 0),
		},
	})
	// 已解锁的 0..7 全被别的道具占满：顺延也找不到落点，只能拒绝。
	full := make(map[string]dnfrepo.ItemStack, 8)
	for slot := int16(0); slot < 8; slot++ {
		full[slotKey(listTypeAccountCargo, slot)] = pvfMoveStack(int64(4000+slot), 1, 0)
	}
	if err := repos.AccountInventory.Save(ctx, dnfrepo.AccountInventoryRecord{
		AccountID: "dnf:locked-slot",
		Slots:     full,
	}); err != nil {
		t.Fatal(err)
	}
	saveMoveContainerState(t, ctx, repos, "77", 24, 8)

	got, err := NewHandler().Handle(ctx, alignedcmd.Request{
		Opcode: uint16(dnfenum.CmdPacketMoveItemspace),
		// 客户端允许拖到未解锁的第 28 格（解锁 8 格），服务端必须给出明确结果。
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
		t.Fatalf("必须回一帧失败包，否则客户端拖拽 pending 状态不释放: %+v", got)
	}
	ack := got.UpperResponses[0]
	wantACK := []byte{0, moveItemspaceDestinationFullErrorCode, listTypeMain, listTypeAccountCargo}
	if ack.MsgID != uint16(dnfenum.CmdPacketMoveItemspace) ||
		ack.Classification != dnfproto.DefaultChannelClassification ||
		!reflect.DeepEqual(ack.Body, wantACK) {
		t.Fatalf("ACK = %+v body=% X, want % X", ack, ack.Body, wantACK)
	}

	loaded := loadTestInventory(t, ctx, repos, "77")
	if source := loaded.Slots[slotKey(listTypeMain, 69)]; source.ItemID != 3330 || source.Count != 1 {
		t.Fatalf("源行在拒绝后被动过: %+v", source)
	}
	cargo, found, err := repos.AccountInventory.Load(ctx, "dnf:locked-slot")
	if err != nil {
		t.Fatal(err)
	}
	if found {
		if _, exists := cargo.Slots[slotKey(listTypeAccountCargo, 28)]; exists {
			t.Fatalf("未解锁格不能被写入: %+v", cargo.Slots)
		}
	}
}

// parseMoveSuccessAckEndpoints 按当前 EXE 的 op19 成功 ACK 布局读端点：
// result(1) + 源 list(1) + 源槽(i16) + 移动量(i32) + 目标 list(1) + 目标槽(i16)。
func parseMoveSuccessAckEndpoints(t *testing.T, body []byte) (byte, int16, byte, int16) {
	t.Helper()
	if len(body) != 11 || body[0] != 1 {
		t.Fatalf("不是 op19 成功 ACK: % X", body)
	}
	return body[1],
		int16(binary.LittleEndian.Uint16(body[2:4])),
		body[8],
		int16(binary.LittleEndian.Uint16(body[9:11]))
}

// TestHandlerPersonalCargoLockedSlotMoveLandsInFirstUnlockedSlot 运营口径 2026-09-18：
// 个人仓库解锁 8 格（0..7）。把道具拖到未解锁的 2:40 时不能照单落库（旧行为＝直接写进看不见
// 的格子），必须顺延到已解锁区第一个空格：第 0 格被别的道具占着，就落到第 1 格，并且 ACK
// 要回显真实落点。
func TestHandlerPersonalCargoLockedSlotMoveLandsInFirstUnlockedSlot(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	saveTestInventory(t, ctx, repos, dnfrepo.InventoryRecord{
		CharacterID: "77",
		Slots: map[string]dnfrepo.ItemStack{
			slotKey(listTypeMain, 65): pvfMoveStack(3330, 1, 0),
		},
		Warehouse: map[string]dnfrepo.ItemStack{
			slotKey(listTypePersonalCargo, 0): pvfMoveStack(3331, 1, 0),
		},
	})
	saveMoveContainerState(t, ctx, repos, "77", 24, 8)

	got, err := NewHandler().Handle(ctx, alignedcmd.Request{
		Opcode:              uint16(dnfenum.CmdPacketMoveItemspace),
		Body:                currentMoveItemspaceBody(listTypeMain, 65, 0, 1, listTypePersonalCargo, 40, 0, 0, -1),
		AccountID:           "dnf:1",
		SelectedCharacterID: 77,
		Repositories:        repos,
	})
	if err != nil {
		t.Fatalf("Handle error = %v", err)
	}
	if !got.Handled || !got.ResponseAllowed || len(got.UpperResponses) != 1 {
		t.Fatalf("result = %+v", got)
	}
	srcList, srcSlot, dstList, dstSlot := parseMoveSuccessAckEndpoints(t, got.UpperResponses[0].Body)
	if srcList != listTypeMain || srcSlot != 65 || dstList != listTypePersonalCargo || dstSlot != 1 {
		t.Fatalf("ACK 端点 = (%d,%d)->(%d,%d), 期望 (0,65)->(2,1)", srcList, srcSlot, dstList, dstSlot)
	}
	loaded := loadTestInventory(t, ctx, repos, "77")
	if _, exists := loaded.Warehouse[slotKey(listTypePersonalCargo, 40)]; exists {
		t.Fatalf("未解锁格不能被写入: %+v", loaded.Warehouse)
	}
	if landed := loaded.Warehouse[slotKey(listTypePersonalCargo, 1)]; landed.ItemID != 3330 || landed.Count != 1 {
		t.Fatalf("落点应为 2:1 的 3330x1: %+v", landed)
	}
	if _, exists := loaded.Slots[slotKey(listTypeMain, 65)]; exists {
		t.Fatalf("源格应被搬空: %+v", loaded.Slots)
	}
}

// TestHandlerPersonalCargoLockedSlotMoveStacksOntoExistingRow 仓库里已有同 ID 行 → 必须叠加
// 进那一行（不新开第二行，也绝不落到未解锁格）。
func TestHandlerPersonalCargoLockedSlotMoveStacksOntoExistingRow(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	saveTestInventory(t, ctx, repos, dnfrepo.InventoryRecord{
		CharacterID: "77",
		Slots: map[string]dnfrepo.ItemStack{
			slotKey(listTypeMain, 65): pvfMoveStack(3330, 5, 1000),
		},
		Warehouse: map[string]dnfrepo.ItemStack{
			slotKey(listTypePersonalCargo, 2): pvfMoveStack(3330, 3, 1000),
		},
	})
	saveMoveContainerState(t, ctx, repos, "77", 24, 8)

	got, err := NewHandler().Handle(ctx, alignedcmd.Request{
		Opcode:              uint16(dnfenum.CmdPacketMoveItemspace),
		Body:                currentMoveItemspaceBody(listTypeMain, 65, 0, 5, listTypePersonalCargo, 45, 0, 0, -1),
		AccountID:           "dnf:1",
		SelectedCharacterID: 77,
		Repositories:        repos,
	})
	if err != nil {
		t.Fatalf("Handle error = %v", err)
	}
	if !got.Handled || !got.ResponseAllowed || len(got.UpperResponses) != 1 {
		t.Fatalf("result = %+v", got)
	}
	_, _, _, dstSlot := parseMoveSuccessAckEndpoints(t, got.UpperResponses[0].Body)
	if dstSlot != 2 {
		t.Fatalf("ACK 落点 = %d, 期望叠加进 2:2", dstSlot)
	}
	loaded := loadTestInventory(t, ctx, repos, "77")
	if _, exists := loaded.Warehouse[slotKey(listTypePersonalCargo, 45)]; exists {
		t.Fatalf("未解锁格不能被写入: %+v", loaded.Warehouse)
	}
	if stacked := loaded.Warehouse[slotKey(listTypePersonalCargo, 2)]; stacked.Count != 8 {
		t.Fatalf("2:2 应叠加到 8: %+v", stacked)
	}
	if _, exists := loaded.Slots[slotKey(listTypeMain, 65)]; exists {
		t.Fatalf("源格应被搬空: %+v", loaded.Slots)
	}
}

// TestHandlerPersonalCargoLockedSlotMoveRejectsWhenUnlockedAreaIsFull 已解锁区真的满了
// （0..7 全被别的道具占着）→ 回 op19 失败帧，未解锁格绝不能落库。
func TestHandlerPersonalCargoLockedSlotMoveRejectsWhenUnlockedAreaIsFull(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	full := make(map[string]dnfrepo.ItemStack, 8)
	for slot := int16(0); slot < 8; slot++ {
		full[slotKey(listTypePersonalCargo, slot)] = pvfMoveStack(int64(5000+slot), 1, 0)
	}
	saveTestInventory(t, ctx, repos, dnfrepo.InventoryRecord{
		CharacterID: "77",
		Slots: map[string]dnfrepo.ItemStack{
			slotKey(listTypeMain, 65): pvfMoveStack(3330, 1, 0),
		},
		Warehouse: full,
	})
	saveMoveContainerState(t, ctx, repos, "77", 24, 8)

	got, err := NewHandler().Handle(ctx, alignedcmd.Request{
		Opcode:              uint16(dnfenum.CmdPacketMoveItemspace),
		Body:                currentMoveItemspaceBody(listTypeMain, 65, 0, 1, listTypePersonalCargo, 40, 0, 0, -1),
		AccountID:           "dnf:1",
		SelectedCharacterID: 77,
		Repositories:        repos,
	})
	if err != nil {
		t.Fatalf("Handle error = %v", err)
	}
	if !got.Handled || !got.ResponseAllowed || len(got.UpperResponses) != 1 {
		t.Fatalf("满了必须回失败帧: %+v", got)
	}
	wantACK := []byte{0, moveItemspaceDestinationFullErrorCode, listTypeMain, listTypePersonalCargo}
	if body := got.UpperResponses[0].Body; !reflect.DeepEqual(body, wantACK) {
		t.Fatalf("ACK body=% X, want % X", body, wantACK)
	}
	loaded := loadTestInventory(t, ctx, repos, "77")
	if _, exists := loaded.Warehouse[slotKey(listTypePersonalCargo, 40)]; exists {
		t.Fatalf("未解锁格不能被写入: %+v", loaded.Warehouse)
	}
	if source := loaded.Slots[slotKey(listTypeMain, 65)]; source.Count != 1 {
		t.Fatalf("拒绝后源行不应变化: %+v", source)
	}
}

// TestHandlerCrossContainerOccupiedDestinationWithClientRowRedirectsToFreeSlot 复刻现场
// 2026-09-27 的两个方向：玩家把东西丢在**客户端看得见的**目标行上（DestinationInstance /
// DestinationStack 非 0），那一行装的是别的东西、叠不上，而目标容器还有空格。
//
// 旧实现：crossContainerOccupiedDestinationRedirect 整段被
// crossContainerClientSawEmptyDestination 挡住 → 回"空间不足"（错误码 4）。于是
//   - 背包里的材料拖到金库装着消耗品的格子 → 报错（(0,124) → (2,150)）；
//   - 金库里锁好的频道喇叭拖回背包的占用格 → 报错（(2,143) → (0,69)/(0,92)/(0,111)），
//     只有丢进同类堆才给动。
//
// 跨容器不能互换，所以"叠不上"之后必须落到目标容器里的空格上。
func TestHandlerCrossContainerOccupiedDestinationWithClientRowRedirectsToFreeSlot(t *testing.T) {
	t.Run("bag to personal cargo", func(t *testing.T) {
		ctx := context.Background()
		repos := dnfrepomemory.NewMemoryGroup()
		saveTestInventory(t, ctx, repos, dnfrepo.InventoryRecord{
			CharacterID: "77",
			Slots: map[string]dnfrepo.ItemStack{
				slotKey(listTypeMain, 65): pvfMoveStack(3330, 5, 0),
			},
			Warehouse: map[string]dnfrepo.ItemStack{
				// 客户端看得见的这一行是别的物品，叠不上。
				slotKey(listTypePersonalCargo, 3): pvfMoveStack(3331, 20, 0),
			},
		})
		saveMoveContainerState(t, ctx, repos, "77", 24, 8)

		got, err := NewHandler().Handle(ctx, alignedcmd.Request{
			Opcode: uint16(dnfenum.CmdPacketMoveItemspace),
			// dstInst 非 0：客户端把它看见的那一行带在包体里。
			Body:                currentMoveItemspaceBody(listTypeMain, 65, 0, 5, listTypePersonalCargo, 3, 0x24, 20, -1),
			AccountID:           "dnf:1",
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
		if dstList != listTypePersonalCargo || dstSlot == 3 {
			t.Fatalf("ACK 落点 = (%d,%d), 期望金库里的空格而不是声明格 2:3", dstList, dstSlot)
		}
		loaded := loadTestInventory(t, ctx, repos, "77")
		if _, exists := loaded.Slots[slotKey(listTypeMain, 65)]; exists {
			t.Fatalf("源行应被搬空: %+v", loaded.Slots)
		}
		if target := loaded.Warehouse[slotKey(listTypePersonalCargo, 3)]; target.ItemID != 3331 || target.Count != 20 {
			t.Fatalf("声明格那件不许被动: %+v", target)
		}
		if landed := loaded.Warehouse[slotKey(listTypePersonalCargo, dstSlot)]; landed.ItemID != 3330 || landed.Count != 5 {
			t.Fatalf("材料应落在金库 %d: %+v", dstSlot, landed)
		}
	})

	t.Run("personal cargo to bag", func(t *testing.T) {
		ctx := context.Background()
		repos := dnfrepomemory.NewMemoryGroup()
		locked := pvfMoveStack(3330, 5, 0)
		locked.Extra["item_sort_lock_state"] = "1"
		saveTestInventory(t, ctx, repos, dnfrepo.InventoryRecord{
			CharacterID: "77",
			Slots: map[string]dnfrepo.ItemStack{
				slotKey(listTypeMain, 20): pvfMoveStack(3331, 20, 0),
			},
			Warehouse: map[string]dnfrepo.ItemStack{
				slotKey(listTypePersonalCargo, 3): locked,
			},
		})
		saveMoveContainerState(t, ctx, repos, "77", 24, 8)

		got, err := NewHandler().Handle(ctx, alignedcmd.Request{
			Opcode: uint16(dnfenum.CmdPacketMoveItemspace),
			// 往背包的占用格上丢（客户端带着它看见的那一行）。
			Body:                currentMoveItemspaceBody(listTypePersonalCargo, 3, 0, 5, listTypeMain, 20, 0x24, 20, -1),
			AccountID:           "dnf:1",
			SelectedCharacterID: 77,
			Repositories:        repos,
			StackMergeResolver: accountCargoAttachResolverWithBagPage(map[int64]string{
				3330: "`[free]`",
				3331: "`[free]`",
			}),
		})
		if err != nil {
			t.Fatalf("Handle error = %v", err)
		}
		if !got.Handled || !got.ResponseAllowed || len(got.UpperResponses) == 0 {
			t.Fatalf("result = %+v", got)
		}
		ack := got.UpperResponses[0]
		if ack.MsgID != uint16(dnfenum.CmdPacketMoveItemspace) || ack.Classification != 1 {
			t.Fatalf("ACK header = %+v", ack)
		}
		_, _, dstList, dstSlot := parseMoveSuccessAckEndpoints(t, ack.Body)
		if dstList != listTypeMain || dstSlot == 20 || dstSlot < 9 || dstSlot > 64 {
			t.Fatalf("ACK 落点 = (%d,%d), 期望背包页里的空格", dstList, dstSlot)
		}
		// 这件是锁着的：落点改了之后必须按新落点补发一条 class1/op713，否则客户端
		// 的 padlock 会留在金库的旧格上。
		if len(got.UpperResponses) != 2 {
			t.Fatalf("responses = %+v, 期望 op19 ACK + 一条 op713 补发", got.UpperResponses)
		}
		signal := got.UpperResponses[1]
		if signal.MsgID != uint16(dnfenum.CmdPacketRequestItemSortLock) ||
			!reflect.DeepEqual(signal.Body, []byte{1, listTypeMain, byte(dstSlot), 0}) {
			t.Fatalf("整理锁补发 = %+v, 期望落点 (%d,%d)", signal, listTypeMain, dstSlot)
		}
		loaded := loadTestInventory(t, ctx, repos, "77")
		if _, exists := loaded.Warehouse[slotKey(listTypePersonalCargo, 3)]; exists {
			t.Fatalf("金库源行应被搬空: %+v", loaded.Warehouse)
		}
		if target := loaded.Slots[slotKey(listTypeMain, 20)]; target.ItemID != 3331 || target.Count != 20 {
			t.Fatalf("背包目标那件不许被换走: %+v", target)
		}
		landed := loaded.Slots[slotKey(listTypeMain, dstSlot)]
		if landed.ItemID != 3330 || landed.Count != 5 {
			t.Fatalf("喇叭应落在背包 %d: %+v", dstSlot, landed)
		}
		// 整理锁跟着物品走，改落点也不能把它丢掉。
		if !dnfitemlock.IsSortLocked(landed) {
			t.Fatalf("改落点后整理锁丢了: %+v", landed.Extra)
		}
		if len(got.ItemSlotRefreshes) != 0 {
			t.Fatalf("item slot refreshes = %+v, want none", got.ItemSlotRefreshes)
		}
	})
}

// TestHandlerEmblemDepositIntoWarehouseIsRejected 锁住运营口径 2026-10-03：
// **徽章不允许存进仓库**（个人仓库 list 2 / 账号金库 list 12），服务端直接拒绝。
//
// 理由：徽章页 289..351 是它唯一该在的地方（9 行 × 7 格 = 63）。允许往仓库里挪，就等于
// 给"徽章栏满了"开了一条后门 —— 页面上空出格子，而分解产出与邮件领取都只按徽章页找落点，
// 页面状态与真实持有量从此对不上。
//
// 取出**不拦**（历史已经躺在仓库里的那些要能拖出来，拖出来会按 PVF 页落回徽章页）。
// 普通道具照旧能存，这条用例同时钉住"别把仓库闸门一起关掉"。
func TestHandlerEmblemDepositIntoWarehouseIsRejected(t *testing.T) {
	const emblemID int64 = 2520037
	emblemResolver := func(itemID int64) (alignedcmd.StackMergeResolution, error) {
		if itemID == emblemID {
			return alignedcmd.StackMergeResolution{
				ItemID: itemID, Kind: "stackable", PVFPath: "stackable/emblem/green/glitteremblem_greenphysicalcri3.stk",
				StackableType: "[avatar emblem]", StackLimit: 999, SlotStart: 289, SlotEnd: 351,
			}, nil
		}
		return alignedcmd.StackMergeResolution{
			ItemID: itemID, Kind: "stackable", PVFPath: "stackable/material/test.stk",
			StackableType: "[material]", StackLimit: 1000, SlotStart: 121, SlotEnd: 176,
		}, nil
	}

	tests := []struct {
		name          string
		itemID        int64
		destination   byte
		destSlot      int16
		wantStored    bool
		wantErrorCode byte
	}{
		{
			name: "徽章存个人仓库必须被拒", itemID: emblemID,
			destination: listTypePersonalCargo, destSlot: 5,
			wantStored: false, wantErrorCode: moveItemspaceRejectedErrorCode,
		},
		{
			name: "徽章存账号金库必须被拒", itemID: emblemID,
			destination: listTypeAccountCargo, destSlot: 1,
			wantStored: false, wantErrorCode: moveItemspaceRejectedErrorCode,
		},
		{
			name: "普通材料照旧能存个人仓库", itemID: 3330,
			destination: listTypePersonalCargo, destSlot: 5,
			wantStored: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			const accountID = "dnf:emblem-warehouse-ban"
			ctx := context.Background()
			repos := dnfrepomemory.NewMemoryGroup()
			saveLockedSlotAccountCargoFixture(t, ctx, repos, accountID, map[string]dnfrepo.ItemStack{})
			saveBagItem(t, ctx, repos, "77", 70, pvfMoveStack(test.itemID, 3, 999))
			saveMoveContainerState(t, ctx, repos, "77", 24, 8)

			got, err := NewHandler().Handle(ctx, alignedcmd.Request{
				Opcode: uint16(dnfenum.CmdPacketMoveItemspace),
				Body: currentMoveItemspaceBody(
					listTypeMain, 70, 0, 3,
					test.destination, test.destSlot, 0, 0, -1,
				),
				AccountID:           accountID,
				SelectedCharacterID: 77,
				Repositories:        repos,
				StackMergeResolver:  emblemResolver,
			})
			if err != nil {
				t.Fatalf("Handle error = %v", err)
			}
			if !got.Handled || !got.ResponseAllowed || len(got.UpperResponses) != 1 {
				t.Fatalf("result = %+v", got)
			}
			body := got.UpperResponses[0].Body
			loaded := loadTestInventory(t, ctx, repos, "77")
			source := loaded.Slots[slotKey(listTypeMain, 70)]
			if test.wantStored {
				if len(body) == 0 || body[0] != 1 {
					t.Fatalf("普通道具必须能存进仓库, body=% X", body)
				}
				if _, exists := loaded.Slots[slotKey(listTypeMain, 70)]; exists {
					t.Fatalf("存仓后背包源行应被搬空: %+v", loaded.Slots)
				}
				if landed := loaded.Warehouse[slotKey(listTypePersonalCargo, test.destSlot)]; landed.ItemID != test.itemID {
					t.Fatalf("落点 = %+v, want item %d", landed, test.itemID)
				}
				return
			}
			// 拒绝：必须回失败帧，而且要回"不允许"码（17）而不是"物品栏空间不足"（4）——
			// 仓库明明空着，回 4 会让玩家以为是格子问题。
			if len(body) < 2 || body[0] != 0 {
				t.Fatalf("徽章存仓库必须回失败帧, body=% X", body)
			}
			if body[1] != test.wantErrorCode {
				t.Fatalf("失败码 = %d, want %d（不允许，而不是空间不足）, body=% X", body[1], test.wantErrorCode, body)
			}
			if source.ItemID != test.itemID || source.Count != 3 {
				t.Fatalf("拒绝后源行不许被改动: %+v", source)
			}
			if len(loaded.Warehouse) != 0 {
				t.Fatalf("个人仓库不许被写入: %+v", loaded.Warehouse)
			}
			cargo, _, err := repos.AccountInventory.Load(ctx, accountID)
			if err != nil {
				t.Fatalf("load account cargo: %v", err)
			}
			if len(cargo.Slots) != 0 {
				t.Fatalf("账号金库不许被写入: %+v", cargo.Slots)
			}
		})
	}

	// 取出不拦：仓库里的徽章必须能拖回背包，并且按 PVF 页落到徽章页。
	t.Run("仓库里的徽章取出不拦且落回徽章页", func(t *testing.T) {
		const accountID = "dnf:emblem-warehouse-withdraw"
		ctx := context.Background()
		repos := dnfrepomemory.NewMemoryGroup()
		saveLockedSlotAccountCargoFixture(t, ctx, repos, accountID, map[string]dnfrepo.ItemStack{})
		saveTestInventory(t, ctx, repos, dnfrepo.InventoryRecord{
			CharacterID: "77",
			Slots:       map[string]dnfrepo.ItemStack{},
			Warehouse: map[string]dnfrepo.ItemStack{
				slotKey(listTypePersonalCargo, 5): pvfMoveStack(emblemID, 3, 999),
			},
		})
		saveMoveContainerState(t, ctx, repos, "77", 24, 8)

		// 客户端照旧声明一个错页落点（消耗品页 116），修复后必须被拉回徽章页。
		got, err := NewHandler().Handle(ctx, alignedcmd.Request{
			Opcode:              uint16(dnfenum.CmdPacketMoveItemspace),
			Body:                currentMoveItemspaceBody(listTypePersonalCargo, 5, 0, 3, listTypeMain, 116, 0, 0, -1),
			AccountID:           accountID,
			SelectedCharacterID: 77,
			Repositories:        repos,
			StackMergeResolver:  emblemResolver,
		})
		if err != nil {
			t.Fatalf("Handle error = %v", err)
		}
		if !got.Handled || len(got.UpperResponses) != 1 {
			t.Fatalf("取回必须成功: %+v", got)
		}
		if body := got.UpperResponses[0].Body; len(body) == 0 || body[0] != 1 {
			t.Fatalf("取出不许被拦, body=% X", body)
		}
		loaded := loadTestInventory(t, ctx, repos, "77")
		if landed := loaded.Slots[slotKey(listTypeMain, 289)]; landed.ItemID != emblemID || landed.Count != 3 {
			t.Fatalf("取出的徽章应落在徽章页 289: %+v", landed)
		}
		if _, exists := loaded.Warehouse[slotKey(listTypePersonalCargo, 5)]; exists {
			t.Fatalf("仓库源行应被搬空: %+v", loaded.Warehouse)
		}
	})
}

// TestHandlerAccountCargoUntradeableDepositWithFreeSlotStaysRejected 是"别把账号金库的
// 资格闸门修坏了"的守卫：空格回退只负责落点，**不是**入库资格的旁路。不可交易
// （[attach type] trade delete）的道具即使在金库里有空格可落，也必须整笔拒绝，
// 两边数据都不动。账号金库只收 free / account / sealing。
func TestHandlerAccountCargoUntradeableDepositWithFreeSlotStaysRejected(t *testing.T) {
	const accountID = "dnf:untradeable-free-slot"
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	// 金库容量 8 格，只占 12:0 —— 12:1..12:7 全是空格，落点回退本来能成功。
	saveLockedSlotAccountCargoFixture(t, ctx, repos, accountID, map[string]dnfrepo.ItemStack{
		slotKey(listTypeAccountCargo, 0): pvfMoveStack(3331, 1, 0),
	})
	saveBagItem(t, ctx, repos, "77", 70, pvfMoveStack(3330, 1, 0))

	got, err := NewHandler().Handle(ctx, alignedcmd.Request{
		Opcode:              uint16(dnfenum.CmdPacketMoveItemspace),
		Body:                currentMoveItemspaceBody(listTypeMain, 70, 0, 1, listTypeAccountCargo, 0, 0x24, 1, -1),
		AccountID:           accountID,
		SelectedCharacterID: 77,
		Repositories:        repos,
		StackMergeResolver: accountCargoAttachResolverWithBagPage(map[int64]string{
			3330: "`[trade delete]`",
			3331: "`[free]`",
		}),
	})
	if err != nil {
		t.Fatalf("Handle error = %v", err)
	}
	if !got.Handled || len(got.UpperResponses) != 1 {
		t.Fatalf("必须回一个失败帧: %+v", got)
	}
	if body := got.UpperResponses[0].Body; len(body) == 0 || body[0] != 0 {
		t.Fatalf("不可交易道具入账号金库必须回失败帧, body=% X", body)
	}
	loaded := loadTestInventory(t, ctx, repos, "77")
	if source := loaded.Slots[slotKey(listTypeMain, 70)]; source.ItemID != 3330 || source.Count != 1 {
		t.Fatalf("拒绝后源行不应变化: %+v", source)
	}
	cargo, _, err := repos.AccountInventory.Load(ctx, accountID)
	if err != nil {
		t.Fatalf("load account cargo: %v", err)
	}
	if len(cargo.Slots) != 1 {
		t.Fatalf("账号金库不许被写入新行: %+v", cargo.Slots)
	}
	if row := cargo.Slots[slotKey(listTypeAccountCargo, 0)]; row.ItemID != 3331 {
		t.Fatalf("账号金库原有行被改动: %+v", row)
	}
}
