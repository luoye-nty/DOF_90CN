package equip

import (
	"context"
	"encoding/binary"
	"testing"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

func TestDecreaseDurabilityUpdatesRawAndExtraForExactSlot(t *testing.T) {
	repositories := dnfrepomemory.NewRuntime().Group()
	raw := make([]byte, 12)
	binary.LittleEndian.PutUint16(raw[10:12], 45)
	if err := repositories.Equipment.Save(context.Background(), dnfrepo.EquipmentRecord{
		CharacterID: "77",
		Entries: map[string]dnfrepo.EquipmentEntry{
			"12": {SlotIndex: 12, ItemID: 9001, RawEntry: raw, Extra: map[string]string{"durability": "45"}},
			"14": {SlotIndex: 14, ItemID: 9002, RawEntry: append([]byte(nil), raw...), Extra: map[string]string{"durability": "45"}},
		},
	}); err != nil {
		t.Fatal(err)
	}
	owner, err := NewOwner(repositories)
	if err != nil {
		t.Fatal(err)
	}
	result, err := owner.DecreaseDurability(context.Background(), DecreaseDurabilityCommand{
		SelectedCharacterID: 77,
		SlotIndex:           12,
		Amount:              1,
		Resolve: func(itemID int64) (alignedcmd.RepairCostEvidence, error) {
			return alignedcmd.RepairCostEvidence{MaxDurability: 45}, nil
		},
	})
	if err != nil || !result.Changed || result.OldDurability != 45 || result.NewDurability != 44 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	loaded, found, err := repositories.Equipment.Load(context.Background(), "77")
	if err != nil || !found {
		t.Fatalf("load found=%t err=%v", found, err)
	}
	if got := binary.LittleEndian.Uint16(loaded.Entries["12"].RawEntry[10:12]); got != 44 || loaded.Entries["12"].Extra["durability"] != "44" {
		t.Fatalf("slot12 raw=%d extra=%q", got, loaded.Entries["12"].Extra["durability"])
	}
	if got := binary.LittleEndian.Uint16(loaded.Entries["14"].RawEntry[10:12]); got != 45 {
		t.Fatalf("slot14 durability=%d want untouched 45", got)
	}
}

func TestDecreaseDurabilityClampsAtZeroAndSkipsNonRepairable(t *testing.T) {
	repositories := dnfrepomemory.NewRuntime().Group()
	raw := make([]byte, 12)
	binary.LittleEndian.PutUint16(raw[10:12], 1)
	if err := repositories.Equipment.Save(context.Background(), dnfrepo.EquipmentRecord{
		CharacterID: "77",
		Entries: map[string]dnfrepo.EquipmentEntry{
			"12": {SlotIndex: 12, ItemID: 9001, RawEntry: raw},
			"14": {SlotIndex: 14, ItemID: 9002, RawEntry: append([]byte(nil), raw...)},
		},
	}); err != nil {
		t.Fatal(err)
	}
	owner, err := NewOwner(repositories)
	if err != nil {
		t.Fatal(err)
	}
	result, err := owner.DecreaseDurability(context.Background(), DecreaseDurabilityCommand{
		SelectedCharacterID: 77,
		SlotIndex:           12,
		Amount:              9,
		Resolve: func(itemID int64) (alignedcmd.RepairCostEvidence, error) {
			return alignedcmd.RepairCostEvidence{MaxDurability: 45}, nil
		},
	})
	if err != nil || !result.Changed || result.NewDurability != 0 {
		t.Fatalf("clamp result=%+v err=%v", result, err)
	}
	result, err = owner.DecreaseDurability(context.Background(), DecreaseDurabilityCommand{
		SelectedCharacterID: 77,
		SlotIndex:           14,
		Resolve: func(itemID int64) (alignedcmd.RepairCostEvidence, error) {
			return alignedcmd.RepairCostEvidence{}, nil
		},
	})
	if err != nil || result.Changed {
		t.Fatalf("nonrepairable result=%+v err=%v", result, err)
	}
}

func TestDecreaseDurabilityUsesCurrentRaw77Offset(t *testing.T) {
	repositories := dnfrepomemory.NewRuntime().Group()
	raw := make([]byte, currentEquipmentRawEntryWireSize)
	binary.LittleEndian.PutUint32(raw[2:6], 9001)
	binary.LittleEndian.PutUint16(raw[0x0B:0x0D], 45)
	if err := repositories.Equipment.Save(context.Background(), dnfrepo.EquipmentRecord{
		CharacterID: "77",
		Entries: map[string]dnfrepo.EquipmentEntry{
			"12": {SlotIndex: 12, ItemID: 9001, RawEntry: raw, Extra: map[string]string{"durability": "45", "max_durability": "45"}},
		},
	}); err != nil {
		t.Fatal(err)
	}
	owner, err := NewOwner(repositories)
	if err != nil {
		t.Fatal(err)
	}
	result, err := owner.DecreaseDurability(context.Background(), DecreaseDurabilityCommand{
		SelectedCharacterID: 77,
		SlotIndex:           12,
		Resolve: func(itemID int64) (alignedcmd.RepairCostEvidence, error) {
			return alignedcmd.RepairCostEvidence{MaxDurability: 45}, nil
		},
	})
	if err != nil || !result.Changed || result.NewDurability != 44 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	loaded, _, err := repositories.Equipment.Load(context.Background(), "77")
	if err != nil {
		t.Fatal(err)
	}
	if got := binary.LittleEndian.Uint16(loaded.Entries["12"].RawEntry[0x0B:0x0D]); got != 44 {
		t.Fatalf("raw77 durability=%d want 44", got)
	}
}

func TestRepairUsesCurrentRaw77DurabilityOffset(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewRuntime().Group()
	raw := make([]byte, currentEquipmentRawEntryWireSize)
	binary.LittleEndian.PutUint32(raw[2:6], 9001)
	raw[0x0A] = 0x2F
	binary.LittleEndian.PutUint16(raw[0x0B:0x0D], 12)
	saveTestCharacter(t, ctx, repositories, dnfrepo.CharacterRecord{CharacterID: "77", Stats: map[string]int64{"gold": 1000}})
	saveTestEquipment(t, ctx, repositories, dnfrepo.EquipmentRecord{
		CharacterID: "77",
		Entries: map[string]dnfrepo.EquipmentEntry{
			"12": {SlotIndex: 12, ItemID: 9001, RawEntry: raw, Extra: map[string]string{"durability": "12"}},
		},
	})
	owner, err := NewOwner(repositories)
	if err != nil {
		t.Fatal(err)
	}
	result, err := owner.Repair(ctx, RepairCommand{SelectedCharacterID: 77, SlotIndex: 12}, repairTestResolver(45, 0, 20))
	if err != nil || !result.Changed || result.OldDurability != 12 || result.NewDurability != 45 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	loaded := loadTestEquipment(t, ctx, repositories, "77").Entries["12"]
	if loaded.RawEntry[0x0A] != 0x2F || binary.LittleEndian.Uint16(loaded.RawEntry[0x0B:0x0D]) != 45 {
		t.Fatalf("current raw after repair=%x", loaded.RawEntry[:0x0D])
	}
}

func TestRepairAllUsesCurrentRaw77DurabilityOffset(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewRuntime().Group()
	raw := make([]byte, currentEquipmentRawEntryWireSize)
	binary.LittleEndian.PutUint32(raw[2:6], 9001)
	raw[0x0A] = 0x1F
	binary.LittleEndian.PutUint16(raw[0x0B:0x0D], 12)
	saveTestCharacter(t, ctx, repositories, dnfrepo.CharacterRecord{CharacterID: "77", Stats: map[string]int64{"gold": 1000}})
	saveTestEquipment(t, ctx, repositories, dnfrepo.EquipmentRecord{
		CharacterID: "77",
		Entries: map[string]dnfrepo.EquipmentEntry{
			"12": {SlotIndex: 12, ItemID: 9001, RawEntry: raw, Extra: map[string]string{"durability": "12"}},
		},
	})
	saveTestInventory(t, ctx, repositories, dnfrepo.InventoryRecord{CharacterID: "77", Slots: map[string]dnfrepo.ItemStack{}})
	owner, err := NewOwner(repositories)
	if err != nil {
		t.Fatal(err)
	}
	result, err := owner.RepairAll(ctx, RepairCommand{SelectedCharacterID: 77, SlotIndex: -1}, repairAllTestResolver(map[int64]string{9001: "[weapon]"}))
	if err != nil || !result.Changed || result.RepairedCount != 1 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	loaded := loadTestEquipment(t, ctx, repositories, "77").Entries["12"]
	if loaded.RawEntry[0x0A] != 0x1F || binary.LittleEndian.Uint16(loaded.RawEntry[0x0B:0x0D]) != 20 {
		t.Fatalf("current raw after repair-all=%x", loaded.RawEntry[:0x0D])
	}
}
