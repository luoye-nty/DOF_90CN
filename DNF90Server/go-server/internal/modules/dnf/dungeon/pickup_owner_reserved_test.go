package dungeon

import (
	"testing"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

func reservedTestPlacement(slotStart int16, slotEnd int16) PickupItemPlacement {
	return PickupItemPlacement{
		Definition: PickupItemDefinition{
			ItemID:     7001,
			Kind:       PickupItemStackable,
			StackLimit: 999,
			SlotStart:  slotStart,
			SlotEnd:    slotEnd,
		},
		Amount: 1,
		BuildNew: func(int16) (dnfrepo.ItemStack, error) {
			return dnfrepo.ItemStack{ItemID: 7001, Count: 1}, nil
		},
	}
}

// 弃置物躺在地上期间，它被丢下的那一格必须留给它。
//
// 装备不能并进已占用的行，所以一旦新拾取物按"最小空格优先"占掉那一格，那件弃置物
// 就永久捡不起来 —— 2026-09-27 线上就是这样卡死的（新拾取占了 43 格，地面对象 504
// 再也拾取不了）。归还原格位这条规则不变，这里只验证"已许诺"的格位会被跳过。
func TestPlacePickupItemSkipsSlotsReservedForDiscardOrigins(t *testing.T) {
	record := dnfrepo.InventoryRecord{Slots: map[string]dnfrepo.ItemStack{}}
	placement := reservedTestPlacement(5, 9)
	placement.ReservedSlots = map[int16]struct{}{5: {}, 6: {}}

	slot, _, err := PlacePickupItem(&record, placement)
	if err != nil {
		t.Fatalf("PlacePickupItem: %v", err)
	}
	if slot != 7 {
		t.Fatalf("slot=%d, want 7 (5 and 6 are promised to pending discards)", slot)
	}
	if _, placed := record.Slots["0:7"]; !placed {
		t.Fatalf("inventory=%+v", record.Slots)
	}
}

// 保留集只对主背包生效。行键按列表分开（"<list>:<slot>"），而每一个 discard origin
// 都是 list 0，所以别的列表不该被主背包的保留影响。
func TestPlacePickupItemIgnoresReservationsOnOtherLists(t *testing.T) {
	record := dnfrepo.InventoryRecord{Slots: map[string]dnfrepo.ItemStack{}}
	placement := reservedTestPlacement(5, 9)
	placement.ListType = 38
	placement.ReservedSlots = map[int16]struct{}{5: {}}

	slot, _, err := PlacePickupItem(&record, placement)
	if err != nil {
		t.Fatalf("PlacePickupItem: %v", err)
	}
	if slot != 5 {
		t.Fatalf("slot=%d, want 5: reservations are main-bag only", slot)
	}
}

// nil 保留集（除副本拾取以外的所有调用方）必须与改动前完全一致：最小空格优先。
func TestPlacePickupItemWithoutReservationsKeepsLowestFreeSlot(t *testing.T) {
	record := dnfrepo.InventoryRecord{Slots: map[string]dnfrepo.ItemStack{}}
	slot, _, err := PlacePickupItem(&record, reservedTestPlacement(5, 9))
	if err != nil {
		t.Fatalf("PlacePickupItem: %v", err)
	}
	if slot != 5 {
		t.Fatalf("slot=%d, want 5", slot)
	}
}

// 保留一个格位不等于把它永久锁死：那个格位一旦被真正占用（或不再保留），
// 分配就回到原来的最小空格语义。
func TestPlacePickupItemFallsThroughWhenRangeIsOnlyReservedSlots(t *testing.T) {
	record := dnfrepo.InventoryRecord{Slots: map[string]dnfrepo.ItemStack{}}
	placement := reservedTestPlacement(5, 6)
	placement.ReservedSlots = map[int16]struct{}{5: {}, 6: {}}

	if _, _, err := PlacePickupItem(&record, placement); err == nil {
		t.Fatal("want inventory-full error when every slot in range is reserved")
	}
	if len(record.Slots) != 0 {
		t.Fatalf("inventory=%+v", record.Slots)
	}
}
