package creaturestate

import (
	"context"
	"encoding/binary"
	"testing"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

func TestReconcileInventoryRestoresPetEnchantAliasesFromTypedState(t *testing.T) {
	ctx := context.Background()
	const characterID = "77"
	const itemID int64 = 400990167
	const serial uint32 = 41

	t.Run("worn equipment", func(t *testing.T) {
		repos := dnfrepomemory.NewMemoryGroup()
		raw := make([]byte, 43)
		binary.LittleEndian.PutUint32(raw[5:9], serial)
		binary.LittleEndian.PutUint32(raw[24:28], serial)
		if err := repos.Inventory.Save(ctx, dnfrepo.InventoryRecord{CharacterID: characterID, Slots: map[string]dnfrepo.ItemStack{}}); err != nil {
			t.Fatal(err)
		}
		if err := repos.Equipment.Save(ctx, dnfrepo.EquipmentRecord{CharacterID: characterID, Entries: map[string]dnfrepo.EquipmentEntry{
			"26": {SlotIndex: 26, ItemID: itemID, RawEntry: raw, Extra: map[string]string{"pet_enchant_card_item_id": "0", "enchant_card_id": "0"}},
		}}); err != nil {
			t.Fatal(err)
		}
		if err := repos.Pet.Save(ctx, dnfrepo.PetRecord{CharacterID: characterID, EquippedKey: "41", Entries: map[string]dnfrepo.PetEntry{
			"41": {PetKey: "41", CreatureKey: serial, ItemID: itemID, SourceListType: 3, SourceSlotIndex: 26, Extra: map[string]string{"pet_enchant_card_item_id": "10008729", "pet_enchant_upgrade_count": "2"}},
		}}); err != nil {
			t.Fatal(err)
		}

		changed, err := ReconcileInventory(ctx, characterID, repos.Inventory, repos.Equipment, repos.Pet)
		if err != nil || !changed {
			t.Fatalf("ReconcileInventory changed=%t err=%v", changed, err)
		}
		equipment, found, err := repos.Equipment.Load(ctx, characterID)
		if err != nil || !found {
			t.Fatalf("load equipment found=%t err=%v", found, err)
		}
		entry := equipment.Entries["26"]
		if got := binary.LittleEndian.Uint32(entry.RawEntry[0x10:0x14]); got != 10008729 || entry.RawEntry[0x14] != 2 {
			t.Fatalf("equipment enchant raw=%d/%d", got, entry.RawEntry[0x14])
		}
		if entry.Extra["pet_enchant_card_item_id"] != "10008729" {
			t.Fatalf("equipment enchant extra=%+v", entry.Extra)
		}
	})

	t.Run("pet inventory", func(t *testing.T) {
		repos := dnfrepomemory.NewMemoryGroup()
		raw := make([]byte, 119)
		if err := repos.Inventory.Save(ctx, dnfrepo.InventoryRecord{CharacterID: characterID, Slots: map[string]dnfrepo.ItemStack{
			"7:48": {ItemID: itemID, Count: 1, RawEntry: raw, Extra: map[string]string{"creature_serial_or_handle": "41", "pet_enchant_card_item_id": "0", "enchant_card_id": "0"}},
		}}); err != nil {
			t.Fatal(err)
		}
		if err := repos.Equipment.Save(ctx, dnfrepo.EquipmentRecord{CharacterID: characterID, Entries: map[string]dnfrepo.EquipmentEntry{}}); err != nil {
			t.Fatal(err)
		}
		if err := repos.Pet.Save(ctx, dnfrepo.PetRecord{CharacterID: characterID, Entries: map[string]dnfrepo.PetEntry{
			"41": {PetKey: "41", CreatureKey: serial, ItemID: itemID, SourceListType: 7, SourceSlotIndex: 48, Extra: map[string]string{"pet_enchant_card_item_id": "10008729", "pet_enchant_upgrade_count": "2"}},
		}}); err != nil {
			t.Fatal(err)
		}

		changed, err := ReconcileInventory(ctx, characterID, repos.Inventory, repos.Equipment, repos.Pet)
		if err != nil || !changed {
			t.Fatalf("ReconcileInventory changed=%t err=%v", changed, err)
		}
		inventory, found, err := repos.Inventory.Load(ctx, characterID)
		if err != nil || !found {
			t.Fatalf("load inventory found=%t err=%v", found, err)
		}
		stack := inventory.Slots["7:48"]
		if stack.Extra["pet_enchant_card_item_id"] != "10008729" || binary.LittleEndian.Uint32(stack.RawEntry[0x0E:0x12]) != 10008729 || stack.RawEntry[0x12] != 2 {
			t.Fatalf("inventory enchant stack=%+v raw=%x", stack.Extra, stack.RawEntry)
		}
	})
}
