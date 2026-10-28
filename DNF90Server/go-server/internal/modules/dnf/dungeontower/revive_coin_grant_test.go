package dungeontower

import (
	"testing"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrevivecoin "longheng.io/server/internal/modules/dnf/revivecoin"
)

// Tower settlement rewards are their own grant path; revive coins from them
// follow the same rule as every other channel.
func TestAddRewardItemRoutesReviveCoinsToTheWallet(t *testing.T) {
	for _, itemID := range []int64{1, 42} {
		record := &dnfrepo.InventoryRecord{CharacterID: "7", Slots: map[string]dnfrepo.ItemStack{
			dnfrevivecoin.WalletKey(): {ItemID: 1, Count: 1},
		}}
		slot, err := addRewardItem(record, 256, RewardItem{
			Stack:     dnfrepo.ItemStack{ItemID: itemID, Count: 2},
			Stackable: true,
			SlotStart: 121,
			SlotEnd:   176,
		})
		if err != nil {
			t.Fatalf("item %d: %v", itemID, err)
		}
		if slot != dnfrevivecoin.WalletSlot {
			t.Fatalf("item %d: slot=%d want %d", itemID, slot, dnfrevivecoin.WalletSlot)
		}
		wallet := record.Slots[dnfrevivecoin.WalletKey()]
		if wallet.ItemID != dnfrevivecoin.WalletItemID || wallet.Count != 3 {
			t.Fatalf("item %d: wallet=%+v want item 1 x3", itemID, wallet)
		}
		if len(record.Slots) != 1 {
			t.Fatalf("item %d: a loose bag row was left behind: %+v", itemID, record.Slots)
		}
	}
}
