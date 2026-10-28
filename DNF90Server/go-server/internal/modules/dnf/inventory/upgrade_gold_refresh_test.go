package inventory

import "testing"

// The gold an upgrade spends left the database but never reached the client:
// the op50 refresh only carried the mutated item slots, and the wallet is
// main-list slot 0, which is not one of them. The player saw the old balance
// until the next full list refresh, which reads as "强化没有扣金币".
func TestUpgradeItemUpdateEntriesCarryTheWalletWhenGoldWasSpent(t *testing.T) {
	result := UpgradeResult{
		Mode:                    "reinforce",
		TargetSlotIndex:         27,
		MaterialSlotIndex:       358,
		OptionalTicketSlotIndex: -1,
		MaterialUpdated:         true,
		GoldCost:                368604,
		UpdatedGold:             969273855,
	}
	entries := upgradeItemUpdateEntries(result, false)
	var wallet *commonItemListEntry
	for index := range entries {
		if entries[index].slot == 0 {
			wallet = &entries[index]
		}
	}
	if wallet == nil {
		t.Fatalf("no wallet row in %+v", entries)
	}
	if wallet.stack.ItemID != 0 || wallet.stack.Count != 969273855 {
		t.Fatalf("wallet row=%+v want item 0 balance 969273855", wallet.stack)
	}
	// Rows stay sorted, so the wallet leads.
	if entries[0].slot != 0 {
		t.Fatalf("entries are not slot-ordered: %+v", entries)
	}

	// A free attempt (portable tool, or a table row with no gold cost) must
	// not push a wallet row.
	free := result
	free.GoldCost = 0
	for _, entry := range upgradeItemUpdateEntries(free, false) {
		if entry.slot == 0 {
			t.Fatalf("free upgrade pushed a wallet row: %+v", entry)
		}
	}
}
