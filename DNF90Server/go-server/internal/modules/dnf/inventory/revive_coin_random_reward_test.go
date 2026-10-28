package inventory

import (
	"testing"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrevivecoin "longheng.io/server/internal/modules/dnf/revivecoin"
)

// A random-reward consumable that rolls a revive coin must fold it into the
// fixed slot-1 wallet, the same as the sibling useStackableWithResolvers path
// already does.
func TestGrantRandomRewardItemRoutesReviveCoinsToTheWallet(t *testing.T) {
	for _, itemID := range []int64{1, 42} {
		items := map[string]dnfrepo.ItemStack{
			dnfrevivecoin.WalletKey(): {ItemID: 1, Count: 7},
		}
		slots, err := grantRandomRewardItem(items, alignedcmd.MagicBoxRewardItem{
			ItemID:         itemID,
			Kind:           "stackable",
			TargetListType: listTypeMain,
			SlotStart:      121,
			SlotEnd:        176,
		}, 1)
		if err != nil || len(slots) != 1 || slots[0] != dnfrevivecoin.WalletSlot {
			t.Fatalf("item %d: slots=%v err=%v", itemID, slots, err)
		}
		wallet := items[dnfrevivecoin.WalletKey()]
		if wallet.ItemID != dnfrevivecoin.WalletItemID || wallet.Count != 8 {
			t.Fatalf("item %d: wallet=%+v want item 1 x8", itemID, wallet)
		}
		if len(items) != 1 {
			t.Fatalf("item %d: a loose bag row was left behind: %+v", itemID, items)
		}
	}
}
