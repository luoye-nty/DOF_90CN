package dungeon

import (
	"testing"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

// list 38 的公会勋章页合法地从 slot 0 开始，而主背包的 slot 0 是金币伪行。
// PlacePickupItem 原先无条件按 SlotStart <= 0 判非法，于是勋章改入 list 38 之后
// 每一次拾取都被挡成 "dungeon pickup item is invalid" —— 玩家表现为勋章捡不起来。
func TestPlacePickupItemAcceptsSlotZeroOnNonMainList(t *testing.T) {
	record := &dnfrepo.InventoryRecord{
		CharacterID: "1",
		Slots:       map[string]dnfrepo.ItemStack{},
	}
	placement := PickupItemPlacement{
		ListType: 38,
		Definition: PickupItemDefinition{
			ItemID:    100380022,
			Kind:      PickupItemEquipment,
			SlotStart: 0,
			SlotEnd:   48,
		},
		Amount: 1,
		BuildNew: func(slot int16) (dnfrepo.ItemStack, error) {
			return dnfrepo.ItemStack{ItemID: 100380022, Count: 1}, nil
		},
	}
	slot, stack, err := PlacePickupItem(record, placement)
	if err != nil {
		t.Fatalf("PlacePickupItem() error = %v, want a placed guild medal", err)
	}
	if slot != 0 {
		t.Fatalf("slot = %d, want the first medal slot 0", slot)
	}
	if stack.ItemID != 100380022 {
		t.Fatalf("stack = %+v", stack)
	}
	if _, ok := record.Slots["38:0"]; !ok {
		t.Fatalf("medal was not written to the list-38 key; slots = %v", record.Slots)
	}
}

// 主背包必须保持原样：slot 0 是金币伪行，页码从 0 开始的定义仍然非法。
func TestPlacePickupItemStillRejectsSlotZeroOnMainList(t *testing.T) {
	record := &dnfrepo.InventoryRecord{
		CharacterID: "1",
		Slots:       map[string]dnfrepo.ItemStack{},
	}
	placement := PickupItemPlacement{
		Definition: PickupItemDefinition{
			ItemID:    1001,
			Kind:      PickupItemEquipment,
			SlotStart: 0,
			SlotEnd:   64,
		},
		Amount: 1,
		BuildNew: func(slot int16) (dnfrepo.ItemStack, error) {
			return dnfrepo.ItemStack{ItemID: 1001, Count: 1}, nil
		},
	}
	if _, _, err := PlacePickupItem(record, placement); err != ErrPickupItemInvalid {
		t.Fatalf("PlacePickupItem() error = %v, want ErrPickupItemInvalid", err)
	}
}
