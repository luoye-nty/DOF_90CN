package dnfbridge

import (
	"testing"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

func TestMailboxItemResolutionAllowsListedPVFItemWithoutPrice(t *testing.T) {
	got, err := mailboxItemResolutionFromPVF(dungeonDropItemDefinition{
		ItemID:        3166,
		Kind:          dungeonDropItemStackable,
		PVFPath:       "stackable/reproduced-item.stk",
		StackableType: "[material]",
		Durability:    0,
		PriceFound:    false,
		SlotStart:     121,
		SlotEnd:       176,
		StackLimit:    1000,
	}, true)
	if err != nil {
		t.Fatalf("mailboxItemResolutionFromPVF error = %v", err)
	}
	if got.Price != 0 || got.PVFPath != "stackable/reproduced-item.stk" || got.Kind != "stackable" ||
		got.StackableType != "[material]" ||
		got.SlotStart != 121 || got.SlotEnd != 176 || got.StackLimit != 1000 || got.PreferQuickSlots {
		t.Fatalf("mailbox item resolution = %+v, want listed zero-value stackable", got)
	}
}

func TestMailboxItemResolutionCarriesEquipmentDurability(t *testing.T) {
	got, err := mailboxItemResolutionFromPVF(dungeonDropItemDefinition{
		ItemID:        5001,
		Kind:          dungeonDropItemEquipment,
		PVFPath:       "equipment/test.equ",
		EquipmentType: "[weapon]",
		Durability:    57,
		SlotStart:     9,
		SlotEnd:       64,
	}, false)
	if err != nil {
		t.Fatalf("mailboxItemResolutionFromPVF error = %v", err)
	}
	if got.Kind != "equipment" || got.EquipmentType != "[weapon]" || got.Durability != 57 || got.StackLimit != 1 {
		t.Fatalf("mailbox equipment resolution = %+v", got)
	}
}

func TestMailboxItemResolutionAllowsQuickSlotsOnlyForPVFWaste(t *testing.T) {
	got, err := mailboxItemResolutionFromPVF(dungeonDropItemDefinition{
		ItemID:        500,
		Kind:          dungeonDropItemStackable,
		StackableType: "[waste]",
		SlotStart:     65,
		SlotEnd:       120,
		StackLimit:    1000,
	}, false)
	if err != nil {
		t.Fatalf("mailboxItemResolutionFromPVF error = %v", err)
	}
	if !got.PreferQuickSlots {
		t.Fatalf("waste mailbox resolution = %+v, want quick-slot preference", got)
	}
}

func TestMailboxItemResolutionRejectsNegativePrice(t *testing.T) {
	_, err := mailboxItemResolutionFromPVF(dungeonDropItemDefinition{
		ItemID:     3166,
		Kind:       dungeonDropItemStackable,
		Price:      -1,
		PriceFound: true,
	}, true)
	if err == nil {
		t.Fatal("negative mailbox PVF price should be rejected")
	}
}

func TestMailboxItemResolutionRoutesEveryNativeInventoryFamily(t *testing.T) {
	tests := []struct {
		name       string
		definition dungeonDropItemDefinition
		listType   byte
		start      int16
		end        int16
	}{
		{
			name: "ordinary equipment",
			definition: dungeonDropItemDefinition{ItemID: 5001, Kind: dungeonDropItemEquipment,
				EquipmentType: "[weapon]", SlotStart: 9, SlotEnd: 64},
			listType: 0, start: 9, end: 64,
		},
		{
			name: "avatar",
			definition: dungeonDropItemDefinition{ItemID: 5002, Kind: dungeonDropItemEquipment,
				EquipmentType: "[skin avatar]", SlotStart: 9, SlotEnd: 64},
			listType: 1, start: 0, end: 500,
		},
		{
			name: "creature",
			definition: dungeonDropItemDefinition{ItemID: 5003, Kind: dungeonDropItemEquipment,
				EquipmentType: "[creature]", SlotStart: 9, SlotEnd: 64},
			listType: 7, start: 0, end: 139,
		},
		{
			name: "creature artifact",
			definition: dungeonDropItemDefinition{ItemID: 5004, Kind: dungeonDropItemEquipment,
				EquipmentType: "[artifact red]", SlotStart: 9, SlotEnd: 64},
			listType: 7, start: 140, end: 188,
		},
		{
			name: "creature feed",
			definition: dungeonDropItemDefinition{ItemID: 5005, Kind: dungeonDropItemStackable,
				StackableType: "[feed]", SlotStart: 65, SlotEnd: 120},
			listType: 7, start: 189, end: 238,
		},
		{
			name: "guild medal",
			definition: dungeonDropItemDefinition{ItemID: 5006, Kind: dungeonDropItemEquipment,
				EquipmentType: "[flag]", SlotStart: 9, SlotEnd: 64},
			listType: 38, start: 0, end: 48,
		},
		{
			name: "guardian gem",
			definition: dungeonDropItemDefinition{ItemID: 5007, Kind: dungeonDropItemStackable,
				StackableType: "[flag gem]", SlotStart: 65, SlotEnd: 120},
			listType: 38, start: 49, end: 97,
		},
		{
			name: "ordinary material subtype four",
			definition: dungeonDropItemDefinition{ItemID: 5008, Kind: dungeonDropItemStackable,
				StackableType: "[material] 4", SlotStart: 345, SlotEnd: 359},
			listType: 0, start: 345, end: 353,
		},
		{
			name: "fixed crystal",
			definition: dungeonDropItemDefinition{ItemID: 3033, Kind: dungeonDropItemStackable,
				StackableType: "[material] 4", SlotStart: 345, SlotEnd: 359},
			listType: 0, start: dnfrepo.CrystalWarehouseFirstSlot, end: dnfrepo.CrystalWarehouseFirstSlot,
		},
		{
			name: "fixed soul",
			definition: dungeonDropItemDefinition{ItemID: 10158124, Kind: dungeonDropItemStackable,
				StackableType: "[material]", SlotStart: 121, SlotEnd: 176},
			listType: 0, start: dnfrepo.SoulWarehouseLastSlot, end: dnfrepo.SoulWarehouseLastSlot,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := mailboxItemResolutionFromPVF(test.definition, false)
			if err != nil {
				t.Fatal(err)
			}
			if got.ListType != test.listType || got.SlotStart != test.start || got.SlotEnd != test.end {
				t.Fatalf("placement=(%d,%d..%d), want=(%d,%d..%d): %+v",
					got.ListType, got.SlotStart, got.SlotEnd, test.listType, test.start, test.end, got)
			}
		})
	}
}
