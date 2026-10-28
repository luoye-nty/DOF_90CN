package dnfbridge

import (
	"encoding/binary"
	"testing"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

func TestCurrentQuestDropOmittedPVFLimitStacksPastLegacy999Threshold(t *testing.T) {
	raw := make([]byte, currentItemListEntryWireSize)
	binary.LittleEndian.PutUint32(raw[0x06:0x0a], 1200)
	inventory := dnfrepo.InventoryRecord{Slots: map[string]dnfrepo.ItemStack{
		"0:177": {
			ItemID: 3244, Count: 1200, RawEntry: raw,
			Extra: map[string]string{"source": "old_quest_drop", "pvf_path": "stackable/old.stk"},
		},
	}}
	definition := dungeonDropItemDefinition{
		ItemID: 3244, Kind: dungeonDropItemStackable,
		PVFPath: "stackable/material/crystal_pure.stk", StackableType: "[material]",
		StackLimit: 0, SlotStart: 177, SlotEnd: 232,
	}
	slot, err := currentQuestDropGrantStack(&inventory, definition, 5)
	if err != nil {
		t.Fatal(err)
	}
	if slot != 177 || len(inventory.Slots) != 1 {
		t.Fatalf("slot=%d inventory=%+v", slot, inventory.Slots)
	}
	stack := inventory.Slots["0:177"]
	if stack.Count != 1205 || binary.LittleEndian.Uint32(stack.RawEntry[0x06:0x0a]) != 1205 {
		t.Fatalf("stack=%+v raw=%x", stack, stack.RawEntry[:10])
	}
	if stack.Extra["item_kind"] != "stackable" || stack.Extra["pvf_path"] != definition.PVFPath ||
		stack.Extra["stackable_type"] != "[material]" {
		t.Fatalf("normalized metadata=%+v", stack.Extra)
	}
}
