package packageitem

import (
	"testing"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrevivecoin "longheng.io/server/internal/modules/dnf/revivecoin"
)

// A 赛丽亚盒子 that rolls revive coins must fold them into the fixed slot-1
// wallet. Left as an ordinary consumable row the player sees two counters --
// the old wallet total and the new stack -- and right-clicking the stack
// falls through to the experience/SP branch ("缺少可增加的经验值/SP"), until
// the next character select consolidates it.
func TestMagicBoxReviveCoinRewardGoesToTheWallet(t *testing.T) {
	for _, itemID := range []int64{1, 42} {
		items := map[string]dnfrepo.ItemStack{
			dnfrevivecoin.WalletKey(): {ItemID: 1, Count: 100},
		}
		remaining, stacks, err := grantMagicBoxReward(items, alignedcmd.MagicBoxRewardItem{
			ItemID:         itemID,
			Kind:           "stackable",
			TargetListType: listTypeMain,
			SlotStart:      121,
			SlotEnd:        176,
			PVFPath:        "stackable/cash/coin_general.stk",
		}, 10)
		if err != nil || remaining != 0 || len(stacks) != 0 {
			t.Fatalf("item %d: remaining=%d stacks=%d err=%v", itemID, remaining, len(stacks), err)
		}
		wallet := items[dnfrevivecoin.WalletKey()]
		if wallet.ItemID != dnfrevivecoin.WalletItemID || wallet.Count != 110 {
			t.Fatalf("item %d: wallet=%+v want item 1 x110", itemID, wallet)
		}
		if len(items) != 1 {
			t.Fatalf("item %d: a loose consumable row was left behind: %+v", itemID, items)
		}
		// The result popup still has to name a slot for the reward row.
		if slot := magicBoxRewardSlot(items, itemID); slot != dnfrevivecoin.WalletSlot {
			t.Fatalf("item %d: reward slot=%d want %d", itemID, slot, dnfrevivecoin.WalletSlot)
		}
	}
}

// An ordinary box reward keeps the PVF slot-range placement.
func TestMagicBoxOrdinaryRewardStillUsesTheBag(t *testing.T) {
	items := map[string]dnfrepo.ItemStack{}
	remaining, _, err := grantMagicBoxReward(items, alignedcmd.MagicBoxRewardItem{
		ItemID:         3166,
		Kind:           "stackable",
		TargetListType: listTypeMain,
		SlotStart:      121,
		SlotEnd:        176,
	}, 5)
	if err != nil || remaining != 0 {
		t.Fatalf("remaining=%d err=%v", remaining, err)
	}
	if stack := items["0:121"]; stack.ItemID != 3166 || stack.Count != 5 {
		t.Fatalf("bag row=%+v want 3166 x5", stack)
	}
}
