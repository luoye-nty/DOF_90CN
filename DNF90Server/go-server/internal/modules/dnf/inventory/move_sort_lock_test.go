package inventory

import (
	"context"
	"testing"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfitemlock "longheng.io/server/internal/modules/dnf/itemlock"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

func moveSortLockTestOwner(t *testing.T, ctx context.Context, slots map[string]dnfrepo.ItemStack) (*Owner, dnfrepo.Group) {
	t.Helper()
	repos := dnfrepomemory.NewMemoryGroup()
	saveTestInventory(t, ctx, repos, dnfrepo.InventoryRecord{
		CharacterID: "77",
		Slots:       slots,
		Warehouse:   map[string]dnfrepo.ItemStack{},
	})
	owner, err := NewOwner(repos)
	if err != nil {
		t.Fatalf("NewOwner error = %v", err)
	}
	return owner, repos
}

func moveSortLockRequest(src byte, srcSlot int16, dst byte, dstSlot int16) Command {
	return NewMoveCommand(alignedcmd.Request{AccountID: "acc", SelectedCharacterID: 77}, MoveItemspaceRequest{
		SourceListType:       src,
		SourceSlotIndex:      srcSlot,
		MoveCount:            1,
		DestinationListType:  dst,
		DestinationSlotIndex: dstSlot,
	})
}

// 86JP ShouldPreserveSortLock：同容器同种类段内搬动保留整理锁，并按
// SendMoveSortLockSignals 补发新槽位的 op713，否则 padlock 会留在旧槽位上。
func TestMoveKeepsSortLockInsideTheSameKindSegmentAndSignalsTheNewSlot(t *testing.T) {
	ctx := context.Background()
	owner, repos := moveSortLockTestOwner(t, ctx, map[string]dnfrepo.ItemStack{
		slotKey(listTypeMain, 65): {ItemID: 500, Count: 1, Extra: map[string]string{"item_kind": "stackable", "item_sort_lock_state": "1"}},
	})
	result, err := owner.Move(ctx, moveSortLockRequest(listTypeMain, 65, listTypeMain, 70))
	if err != nil {
		t.Fatalf("Move error = %v", err)
	}
	if !result.Changed || result.Mode != "move" {
		t.Fatalf("result = %+v", result)
	}
	if len(result.SortLockSignals) != 1 || result.SortLockSignals[0] != (SortLockSignal{ListType: listTypeMain, SlotIndex: 70}) {
		t.Fatalf("sort lock signals = %+v, want the destination slot", result.SortLockSignals)
	}
	loaded := loadTestInventory(t, ctx, repos, "77")
	if !dnfitemlock.IsSortLocked(loaded.Slots[slotKey(listTypeMain, 70)]) {
		t.Fatalf("moved stack lost its sort lock: %+v", loaded.Slots)
	}
	// handler：ACK 之后紧跟一条 class1/op713 锁定包。
	got := ownerAppliedMoveResult(ctx, repos.Settings, 19, moveSortLockRequest(listTypeMain, 65, listTypeMain, 70), result)
	if len(got.UpperResponses) < 2 {
		t.Fatalf("responses = %+v, want ack then sort-lock signal", got.UpperResponses)
	}
	last := got.UpperResponses[len(got.UpperResponses)-1]
	if last.MsgID != uint16(dnfenum.CmdPacketRequestItemSortLock) || string(last.Body) != string(dnfitemlock.BuildSortLockStateBody(listTypeMain, 70)) {
		t.Fatalf("signal = %+v", last)
	}
}

// 跨容器搬动**保留**整理锁，并按 SendMoveSortLockSignals 给新落点补发 op713。
//
// 历史（重要）：这个用例原来断言的是"跨容器清锁"，起因是现场看到"主背包 46 格的锁
// 跟着物品进了个人仓库，第一次 Ctrl+左键在仓库里变成了解锁"。那不是锁不该跟过去，
// 而是**客户端不知道目标格已经上锁**造成的双向状态错位 —— 当时的服务端没有给新落点
// 补发 op713，客户端认为自己没锁、服务端认为自己锁了，于是第一次点击成了"解锁"。
// 现在 appendSortLockSignals 会对仍锁定的端点补发，客户端能收敛，清锁这个权宜之计
// 就可以撤掉。运营口径 2026-09-27：锁挂在物品上，怎么换容器都不该消失。
func TestMoveKeepsSortLockAcrossContainersAndSignalsTheNewSlot(t *testing.T) {
	ctx := context.Background()
	owner, repos := moveSortLockTestOwner(t, ctx, map[string]dnfrepo.ItemStack{
		slotKey(listTypeMain, 65): {ItemID: 500, Count: 1, Extra: map[string]string{"item_kind": "stackable", "item_sort_lock_state": "1"}},
	})
	result, err := owner.Move(ctx, moveSortLockRequest(listTypeMain, 65, listTypePersonalCargo, 3))
	if err != nil {
		t.Fatalf("Move error = %v", err)
	}
	if len(result.SortLockSignals) != 1 || result.SortLockSignals[0] != (SortLockSignal{ListType: listTypePersonalCargo, SlotIndex: 3}) {
		t.Fatalf("sort lock signals = %+v, want the destination slot", result.SortLockSignals)
	}
	loaded := loadTestInventory(t, ctx, repos, "77")
	if !dnfitemlock.IsSortLocked(loaded.Warehouse[slotKey(listTypePersonalCargo, 3)]) {
		t.Fatalf("lock was dropped crossing into the vault: %+v", loaded.Warehouse)
	}

	// 反向（个人仓库 → 背包）同样保留，并给背包的新槽位补发。
	back, err := owner.Move(ctx, moveSortLockRequest(listTypePersonalCargo, 3, listTypeMain, 70))
	if err != nil {
		t.Fatalf("reverse Move error = %v", err)
	}
	if len(back.SortLockSignals) != 1 || back.SortLockSignals[0] != (SortLockSignal{ListType: listTypeMain, SlotIndex: 70}) {
		t.Fatalf("reverse sort lock signals = %+v, want the bag slot", back.SortLockSignals)
	}
	if !dnfitemlock.IsSortLocked(loadTestInventory(t, ctx, repos, "77").Slots[slotKey(listTypeMain, 70)]) {
		t.Fatal("lock was dropped moving back into the bag")
	}
}

// shouldPreserveSortLock 的规则表：跨容器一律保留；同容器只有同种类段保留；
// 主快捷栏（0:3..8）端点仍然清锁（它不属于任何格子段，整理也不会碰它）。
func TestShouldPreserveSortLockRules(t *testing.T) {
	locked := dnfrepo.ItemStack{
		ItemID: 500,
		Count:  1,
		Extra:  map[string]string{"item_kind": "stackable", "item_sort_lock_state": "1"},
	}
	for _, testCase := range []struct {
		name     string
		srcList  byte
		srcSlot  int16
		dstList  byte
		dstSlot  int16
		wantKeep bool
	}{
		{name: "背包→个人仓库", srcList: listTypeMain, srcSlot: 65, dstList: listTypePersonalCargo, dstSlot: 7, wantKeep: true},
		{name: "个人仓库→背包", srcList: listTypePersonalCargo, srcSlot: 7, dstList: listTypeMain, dstSlot: 65, wantKeep: true},
		{name: "背包→账号金库", srcList: listTypeMain, srcSlot: 65, dstList: listTypeAccountCargo, dstSlot: 3, wantKeep: true},
		{name: "账号金库→个人仓库", srcList: listTypeAccountCargo, srcSlot: 3, dstList: listTypePersonalCargo, dstSlot: 7, wantKeep: true},
		{name: "同容器同种类段", srcList: listTypeMain, srcSlot: 65, dstList: listTypeMain, dstSlot: 70, wantKeep: true},
		{name: "同容器换种类段", srcList: listTypeMain, srcSlot: 65, dstList: listTypeMain, dstSlot: 121, wantKeep: false},
		{name: "同容器进主快捷栏", srcList: listTypeMain, srcSlot: 65, dstList: listTypeMain, dstSlot: 3, wantKeep: false},
		{name: "跨容器进主快捷栏", srcList: listTypePersonalCargo, srcSlot: 7, dstList: listTypeMain, dstSlot: 3, wantKeep: false},
		{name: "仓库内挪格", srcList: listTypePersonalCargo, srcSlot: 7, dstList: listTypePersonalCargo, dstSlot: 40, wantKeep: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			got := shouldPreserveSortLock(locked, testCase.srcList, testCase.srcSlot, testCase.dstList, testCase.dstSlot)
			if got != testCase.wantKeep {
				t.Fatalf("shouldPreserveSortLock = %t, want %t", got, testCase.wantKeep)
			}
		})
	}
}

// sortLockedMoveStack 造一个带 PVF 堆叠契约、并且已经上了整理锁的可叠道具。
func sortLockedMoveStack(itemID, count, limit int64) dnfrepo.ItemStack {
	stack := pvfMoveStack(itemID, count, limit)
	stack.Extra["item_sort_lock_state"] = "1"
	return stack
}

// 合并进已上锁的同类堆：目标本来就锁着，结果保持锁定，不因为来源行被删而丢锁。
func TestMoveMergesIntoALockedStackKeepsTheLock(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	saveTestInventory(t, ctx, repos, dnfrepo.InventoryRecord{
		CharacterID: "77",
		Slots: map[string]dnfrepo.ItemStack{
			slotKey(listTypeMain, 65): sortLockedMoveStack(3227, 1, 10),
			slotKey(listTypeMain, 70): sortLockedMoveStack(3227, 5, 10),
		},
		Warehouse: map[string]dnfrepo.ItemStack{},
	})
	owner, err := NewOwner(repos)
	if err != nil {
		t.Fatalf("NewOwner error = %v", err)
	}
	result, err := owner.Move(ctx, moveSortLockRequest(listTypeMain, 65, listTypeMain, 70))
	if err != nil {
		t.Fatalf("Move error = %v", err)
	}
	if result.Mode != "stack" {
		t.Fatalf("mode = %q, want stack", result.Mode)
	}
	merged := loadTestInventory(t, ctx, repos, "77").Slots[slotKey(listTypeMain, 70)]
	if merged.Count != 6 {
		t.Fatalf("merged count = %d, want 6", merged.Count)
	}
	if !dnfitemlock.IsSortLocked(merged) {
		t.Fatalf("merged stack lost the lock: %+v", merged.Extra)
	}
	// 合并后目标格仍锁定，必须补发它自己的 op713。
	if len(result.SortLockSignals) != 1 || result.SortLockSignals[0] != (SortLockSignal{ListType: listTypeMain, SlotIndex: 70}) {
		t.Fatalf("sort lock signals = %+v, want the merged slot", result.SortLockSignals)
	}
}

// 合并进**未上锁**的同类堆：来源行锁着，锁必须跟着走进目标堆叠。
// 否则"把锁好的那一件拖进同类堆"就是另一条悄悄洗掉锁的路径。
func TestMoveMergesLockedStackIntoAnUnlockedStackCarriesTheLock(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	saveTestInventory(t, ctx, repos, dnfrepo.InventoryRecord{
		CharacterID: "77",
		Slots: map[string]dnfrepo.ItemStack{
			slotKey(listTypeMain, 65): sortLockedMoveStack(3227, 1, 10),
		},
		Warehouse: map[string]dnfrepo.ItemStack{
			// 仓库里已经有一组同类、未上锁的堆；落点声明在 40（空格）会被顺延到这里。
			slotKey(listTypePersonalCargo, 7): pvfMoveStack(3227, 5, 10),
		},
	})
	owner, err := NewOwner(repos)
	if err != nil {
		t.Fatalf("NewOwner error = %v", err)
	}
	result, err := owner.Move(ctx, moveSortLockRequest(listTypeMain, 65, listTypePersonalCargo, 40))
	if err != nil {
		t.Fatalf("Move error = %v", err)
	}
	if result.DestinationSlotIndex != 7 {
		t.Fatalf("destination = %d, want the existing stack at 7", result.DestinationSlotIndex)
	}
	merged := loadTestInventory(t, ctx, repos, "77").Warehouse[slotKey(listTypePersonalCargo, 7)]
	if merged.Count != 6 {
		t.Fatalf("merged count = %d, want 6", merged.Count)
	}
	if !dnfitemlock.IsSortLocked(merged) {
		t.Fatalf("lock was washed out by the merge: %+v", merged.Extra)
	}
	if len(result.SortLockSignals) != 1 || result.SortLockSignals[0] != (SortLockSignal{ListType: listTypePersonalCargo, SlotIndex: 7}) {
		t.Fatalf("sort lock signals = %+v, want the merged vault slot", result.SortLockSignals)
	}
}

// 合并落点在主快捷栏（0:3..8）时不挂锁 —— 与 shouldPreserveSortLock 的排除保持一致，
// 否则快捷栏上会留下一个锁，而快捷栏本来就不参与整理、也不该显示整理锁。
func TestMergeIntoMainQuickSlotDoesNotCarryTheLock(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	saveTestInventory(t, ctx, repos, dnfrepo.InventoryRecord{
		CharacterID: "77",
		Slots: map[string]dnfrepo.ItemStack{
			slotKey(listTypeMain, 65): sortLockedMoveStack(3227, 1, 10),
			slotKey(listTypeMain, 3):  pvfMoveStack(3227, 5, 10),
		},
		Warehouse: map[string]dnfrepo.ItemStack{},
	})
	owner, err := NewOwner(repos)
	if err != nil {
		t.Fatalf("NewOwner error = %v", err)
	}
	result, err := owner.Move(ctx, moveSortLockRequest(listTypeMain, 65, listTypeMain, 3))
	if err != nil {
		t.Fatalf("Move error = %v", err)
	}
	if result.Mode != "stack" {
		t.Fatalf("mode = %q, want stack", result.Mode)
	}
	merged := loadTestInventory(t, ctx, repos, "77").Slots[slotKey(listTypeMain, 3)]
	if merged.Count != 6 {
		t.Fatalf("merged count = %d, want 6", merged.Count)
	}
	if dnfitemlock.IsSortLocked(merged) {
		t.Fatalf("quick slot must not carry a sort lock: %+v", merged.Extra)
	}
	if len(result.SortLockSignals) != 0 {
		t.Fatalf("quick slot must not be signalled: %+v", result.SortLockSignals)
	}
}

// 同容器但换了种类段（消耗品 → 材料）同样清锁。
func TestMoveClearsSortLockWhenTheKindSegmentChanges(t *testing.T) {
	ctx := context.Background()
	owner, repos := moveSortLockTestOwner(t, ctx, map[string]dnfrepo.ItemStack{
		slotKey(listTypeMain, 65): {ItemID: 500, Count: 1, Extra: map[string]string{"item_kind": "stackable", "item_sort_lock_state": "1"}},
	})
	result, err := owner.Move(ctx, moveSortLockRequest(listTypeMain, 65, listTypeMain, 121))
	if err != nil {
		t.Fatalf("Move error = %v", err)
	}
	if len(result.SortLockSignals) != 0 {
		t.Fatalf("segment change must not signal a lock: %+v", result.SortLockSignals)
	}
	loaded := loadTestInventory(t, ctx, repos, "77")
	if dnfitemlock.IsSortLocked(loaded.Slots[slotKey(listTypeMain, 121)]) {
		t.Fatalf("lock survived the segment change: %+v", loaded.Slots)
	}
}
