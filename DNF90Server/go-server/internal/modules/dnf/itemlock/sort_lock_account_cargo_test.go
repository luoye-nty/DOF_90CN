package itemlock

import (
	"context"
	"errors"
	"testing"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

func saveAccountCargoRow(t *testing.T, ctx context.Context, repos dnfrepo.Group, accountID string, slot int16, stack dnfrepo.ItemStack) {
	t.Helper()
	if err := repos.AccountInventory.Save(ctx, dnfrepo.AccountInventoryRecord{
		AccountID: accountID,
		Slots:     map[string]dnfrepo.ItemStack{slotKey(listTypeAccountCargo, slot): stack},
	}); err != nil {
		t.Fatalf("save account cargo: %v", err)
	}
}

// 86JP IsSupportedLockListType 把 AccountCargo 算进整理锁定；实况日志里
// list=12 的 op713 曾被 "itemlock list type is not supported: 12" 直接拒掉。
func TestApplySortLockTogglesAccountCargoRow(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	if err := repos.Inventory.Save(ctx, dnfrepo.InventoryRecord{CharacterID: "77"}); err != nil {
		t.Fatal(err)
	}
	saveAccountCargoRow(t, ctx, repos, "dnf:acc", 5, dnfrepo.ItemStack{ItemID: 300, Count: 1})

	owner, err := NewOwner(repos)
	if err != nil {
		t.Fatalf("NewOwner error = %v", err)
	}
	cmd := Command{
		Operation:           "request_item_sort_lock",
		AccountID:           "dnf:acc",
		SelectedCharacterID: 77,
		ListType:            listTypeAccountCargo,
		SlotIndex:           5,
	}
	result, err := owner.ApplySortLock(ctx, cmd, true)
	if err != nil {
		t.Fatalf("ApplySortLock error = %v", err)
	}
	if result.State != sortLockStateActive || !result.Changed || result.ListType != listTypeAccountCargo || result.SlotIndex != 5 {
		t.Fatalf("result = %+v", result)
	}
	cargo, found, err := repos.AccountInventory.Load(ctx, "dnf:acc")
	if err != nil || !found {
		t.Fatalf("load account cargo found=%t err=%v", found, err)
	}
	if !IsSortLocked(cargo.Slots[slotKey(listTypeAccountCargo, 5)]) {
		t.Fatalf("account cargo row is not locked: %+v", cargo.Slots)
	}

	result, err = owner.ApplySortLock(ctx, cmd, false)
	if err != nil {
		t.Fatalf("ApplySortLock unlock error = %v", err)
	}
	if result.State != "" || !result.Changed {
		t.Fatalf("unlock result = %+v", result)
	}
	cargo, _, err = repos.AccountInventory.Load(ctx, "dnf:acc")
	if err != nil {
		t.Fatal(err)
	}
	if IsSortLocked(cargo.Slots[slotKey(listTypeAccountCargo, 5)]) {
		t.Fatalf("account cargo row stayed locked: %+v", cargo.Slots)
	}
}

func TestApplySortLockAccountCargoRejectsEmptySlot(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	if err := repos.Inventory.Save(ctx, dnfrepo.InventoryRecord{CharacterID: "77"}); err != nil {
		t.Fatal(err)
	}
	saveAccountCargoRow(t, ctx, repos, "dnf:acc", 5, dnfrepo.ItemStack{ItemID: 300, Count: 1})

	owner, err := NewOwner(repos)
	if err != nil {
		t.Fatalf("NewOwner error = %v", err)
	}
	_, err = owner.ApplySortLock(ctx, Command{
		Operation:           "request_item_sort_lock",
		AccountID:           "dnf:acc",
		SelectedCharacterID: 77,
		ListType:            listTypeAccountCargo,
		SlotIndex:           6,
	}, true)
	if !errors.Is(err, ErrSlotNotFound) {
		t.Fatalf("ApplySortLock error = %v, want ErrSlotNotFound", err)
	}
}

// 装备锁按 86JP IsSupportedEquipmentLockListType 不覆盖账号金库，保持 fail-close。
func TestApplyEquipmentLockRejectsAccountCargo(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	if err := repos.Inventory.Save(ctx, dnfrepo.InventoryRecord{CharacterID: "77"}); err != nil {
		t.Fatal(err)
	}
	owner, err := NewOwner(repos)
	if err != nil {
		t.Fatalf("NewOwner error = %v", err)
	}
	_, err = owner.Apply(ctx, Command{
		Operation:           "request_item_lock",
		AccountID:           "dnf:acc",
		SelectedCharacterID: 77,
		ListType:            listTypeAccountCargo,
		SlotIndex:           5,
	})
	if !errors.Is(err, ErrUnsupportedList) {
		t.Fatalf("Apply error = %v, want ErrUnsupportedList", err)
	}
}

// 账号金库的锁定行必须进入重发位置表，否则容器重建后 padlock 不会回来。
func TestAccountCargoSortLockPositions(t *testing.T) {
	record := dnfrepo.AccountInventoryRecord{
		AccountID: "dnf:acc",
		Slots: map[string]dnfrepo.ItemStack{
			slotKey(listTypeAccountCargo, 5): {ItemID: 300, Count: 1, Extra: map[string]string{sortLockStateKey: "1"}},
			slotKey(listTypeAccountCargo, 2): {ItemID: 301, Count: 1},
			// 数量为零的残留行不重发。
			slotKey(listTypeAccountCargo, 7): {ItemID: 302, Count: 0, Extra: map[string]string{sortLockStateKey: "1"}},
		},
	}
	positions := AccountCargoSortLockPositions(record)
	if len(positions) != 1 || positions[0] != (SortLockPosition{ListType: listTypeAccountCargo, SlotIndex: 5}) {
		t.Fatalf("positions = %+v", positions)
	}
}

// handler 侧：list 12 的 op713 现在必须回 class1 锁定包而不是被拒。
func TestHandlerSortLockAccountCargoAnswersLockPacket(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	if err := repos.Inventory.Save(ctx, dnfrepo.InventoryRecord{CharacterID: "77"}); err != nil {
		t.Fatal(err)
	}
	saveAccountCargoRow(t, ctx, repos, "dnf:acc", 5, dnfrepo.ItemStack{ItemID: 300, Count: 1})

	got, err := NewHandler().Handle(ctx, alignedcmd.Request{
		Opcode:              uint16(dnfenum.CmdPacketRequestItemSortLock),
		AccountID:           "dnf:acc",
		SelectedCharacterID: 77,
		Repositories:        repos,
		Body:                []byte{listTypeAccountCargo, 5, 0},
	})
	if err != nil {
		t.Fatalf("Handle error = %v", err)
	}
	if !got.Handled || !got.ResponseAllowed {
		t.Fatalf("result = %+v", got)
	}
	if len(got.UpperResponses) != 1 || got.UpperResponses[0].MsgID != uint16(dnfenum.CmdPacketRequestItemSortLock) {
		t.Fatalf("responses = %+v", got.UpperResponses)
	}
	want := BuildSortLockStateBody(listTypeAccountCargo, 5)
	if string(got.UpperResponses[0].Body) != string(want) {
		t.Fatalf("body = % X, want % X", got.UpperResponses[0].Body, want)
	}
}
