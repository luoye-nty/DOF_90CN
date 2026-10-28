package dungeon

import (
	"testing"
	"time"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrevivecoin "longheng.io/server/internal/modules/dnf/revivecoin"
)

// A revive coin picked up in a dungeon must land in the fixed slot-1 wallet,
// never in an ordinary bag row. Item 42 (cash/coin_general.stk) counts as one
// wallet coin each, so it folds into the same row as item 1.
func TestPlacePickupItemRoutesReviveCoinsToTheWallet(t *testing.T) {
	for _, itemID := range []uint32{1, 42} {
		record := dnfrepo.InventoryRecord{CharacterID: "7", Slots: map[string]dnfrepo.ItemStack{
			dnfrevivecoin.WalletKey(): {ItemID: 1, Count: 100},
		}}
		slot, stack, err := PlacePickupItem(&record, PickupItemPlacement{
			Definition: PickupItemDefinition{
				ItemID: itemID, Kind: PickupItemStackable, StackLimit: 0,
				SlotStart: 121, SlotEnd: 176,
			},
			Amount: 10,
			BuildNew: func(int16) (dnfrepo.ItemStack, error) {
				t.Fatal("a revive coin must not build an ordinary bag row")
				return dnfrepo.ItemStack{}, nil
			},
		})
		if err != nil {
			t.Fatalf("item %d: %v", itemID, err)
		}
		if slot != uint16(dnfrevivecoin.WalletSlot) || stack.ItemID != dnfrevivecoin.WalletItemID || stack.Count != 110 {
			t.Fatalf("item %d: slot=%d stack=%+v want slot 1 item 1 x110", itemID, slot, stack)
		}
		if len(record.Slots) != 1 {
			t.Fatalf("item %d: a loose bag row was left behind: %+v", itemID, record.Slots)
		}
	}
}

// Ordinary pickups keep the PVF slot-range placement.
func TestPlacePickupItemLeavesOrdinaryItemsInTheBag(t *testing.T) {
	record := dnfrepo.InventoryRecord{CharacterID: "7", Slots: map[string]dnfrepo.ItemStack{}}
	slot, _, err := PlacePickupItem(&record, PickupItemPlacement{
		Definition: PickupItemDefinition{
			ItemID: 3166, Kind: PickupItemStackable, SlotStart: 121, SlotEnd: 176,
		},
		Amount: 3,
		BuildNew: func(slot int16) (dnfrepo.ItemStack, error) {
			return dnfrepo.ItemStack{ItemID: 3166, Count: 3}, nil
		},
	})
	if err != nil || slot != 121 {
		t.Fatalf("slot=%d err=%v want 121", slot, err)
	}
}

// The dungeon clear card flip is its own grant path and has to follow the
// same rule.
func TestAddCardItemRoutesReviveCoinsToTheWallet(t *testing.T) {
	record := dnfrepo.InventoryRecord{CharacterID: "7", Slots: map[string]dnfrepo.ItemStack{}}
	slot, err := addCardItem(&record, 256, CardItemReward{
		Stack:     dnfrepo.ItemStack{ItemID: 42, Count: 3},
		Stackable: true,
		SlotStart: 121,
		SlotEnd:   176,
	}, func(stack dnfrepo.ItemStack, _ time.Time) (dnfrepo.ItemStack, error) {
		return stack, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if slot != dnfrevivecoin.WalletSlot {
		t.Fatalf("slot=%d want %d", slot, dnfrevivecoin.WalletSlot)
	}
	wallet := record.Slots[dnfrevivecoin.WalletKey()]
	if wallet.ItemID != dnfrevivecoin.WalletItemID || wallet.Count != 3 {
		t.Fatalf("wallet=%+v want item 1 x3", wallet)
	}
	if len(record.Slots) != 1 {
		t.Fatalf("a loose bag row was left behind: %+v", record.Slots)
	}
}
