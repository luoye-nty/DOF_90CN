package equip

import (
	"context"
	"testing"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

func TestOwnerMovePersistsInheritsAndClearsCloneAvatarAppearance(t *testing.T) {
	const (
		ordinaryAvatar int64 = 415500058
		cloneAvatarA   int64 = 415500017
		cloneAvatarB   int64 = 415500027
		wearSlot       int16 = 3
	)
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	saveTestInventory(t, ctx, repos, dnfrepo.InventoryRecord{
		CharacterID: "77",
		Slots: map[string]dnfrepo.ItemStack{
			"1:19": {ItemID: cloneAvatarA, Count: 0, RawEntry: []byte{0xA1}},
			"1:20": {ItemID: cloneAvatarB, Count: 0, RawEntry: []byte{0xB2}},
		},
	})
	saveTestEquipment(t, ctx, repos, dnfrepo.EquipmentRecord{
		CharacterID: "77",
		Entries: map[string]dnfrepo.EquipmentEntry{
			"3": {SlotIndex: wearSlot, ItemID: ordinaryAvatar, RawEntry: []byte{0xC3}},
		},
	})

	owner, err := NewOwnerWithPlacementValidator(repos, &recordingPlacementValidator{})
	if err != nil {
		t.Fatal(err)
	}
	owner.SetAvatarClassifier(func(itemID uint32) (bool, bool) {
		switch int64(itemID) {
		case ordinaryAvatar:
			return true, false
		case cloneAvatarA, cloneAvatarB:
			return true, true
		default:
			return false, false
		}
	})

	equip := func(slot int16) MoveResult {
		t.Helper()
		result, moveErr := owner.Move(ctx, MoveCommand{
			SelectedCharacterID:  77,
			SourceListType:       wireListAvatar,
			SourceSlotIndex:      slot,
			MoveCount:            1,
			DestinationListType:  wireListEquipment,
			DestinationSlotIndex: wearSlot,
		})
		if moveErr != nil {
			t.Fatalf("equip avatar slot %d: %v", slot, moveErr)
		}
		return result
	}

	if result := equip(19); result.Mode != "equip_swap" || result.ItemID != cloneAvatarA || result.SwappedItemID != ordinaryAvatar {
		t.Fatalf("first clone result=%+v", result)
	}
	worn := loadTestEquipment(t, ctx, repos, "77").Entries["3"]
	if worn.ItemID != cloneAvatarA || worn.Extra[dnfrepo.CloneAvatarAppearanceExtraKey] != "415500058" {
		t.Fatalf("first clone worn=%+v", worn)
	}
	if returned := loadTestInventory(t, ctx, repos, "77").Slots["1:19"]; returned.ItemID != ordinaryAvatar || returned.Extra[dnfrepo.CloneAvatarAppearanceExtraKey] != "" {
		t.Fatalf("returned ordinary avatar=%+v", returned)
	}

	if result := equip(20); result.Mode != "equip_swap" || result.ItemID != cloneAvatarB || result.SwappedItemID != cloneAvatarA {
		t.Fatalf("second clone result=%+v", result)
	}
	worn = loadTestEquipment(t, ctx, repos, "77").Entries["3"]
	if worn.ItemID != cloneAvatarB || worn.Extra[dnfrepo.CloneAvatarAppearanceExtraKey] != "415500058" {
		t.Fatalf("inherited clone worn=%+v", worn)
	}
	if returned := loadTestInventory(t, ctx, repos, "77").Slots["1:20"]; returned.ItemID != cloneAvatarA || returned.Extra[dnfrepo.CloneAvatarAppearanceExtraKey] != "" {
		t.Fatalf("returned first clone=%+v", returned)
	}

	result, err := owner.Move(ctx, MoveCommand{
		SelectedCharacterID:      77,
		SourceListType:           wireListAvatar,
		SourceSlotIndex:          21,
		SourceInstanceValue:      0,
		MoveCount:                1,
		DestinationListType:      wireListEquipment,
		DestinationSlotIndex:     wearSlot,
		DestinationInstanceValue: int32(cloneAvatarB),
	})
	if err != nil || result.Mode != "unequip" {
		t.Fatalf("unequip clone result=%+v err=%v", result, err)
	}
	returned := loadTestInventory(t, ctx, repos, "77").Slots["1:21"]
	if returned.ItemID != cloneAvatarB || returned.Extra[dnfrepo.CloneAvatarAppearanceExtraKey] != "" {
		t.Fatalf("unequipped clone retained appearance=%+v", returned)
	}
}
