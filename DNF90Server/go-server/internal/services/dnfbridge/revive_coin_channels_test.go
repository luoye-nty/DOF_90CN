package dnfbridge

import (
	"testing"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrevivecoin "longheng.io/server/internal/modules/dnf/revivecoin"
)

// Quest monster drops, altar drops and guild contents all place items through
// currentQuestDropGrantStack. Revive coins from any of them belong in the
// fixed slot-1 wallet, not in an ordinary bag row.
func TestCurrentQuestDropGrantRoutesReviveCoinsToTheWallet(t *testing.T) {
	for _, itemID := range []uint32{1, 42} {
		inventory := &dnfrepo.InventoryRecord{CharacterID: "7", Slots: map[string]dnfrepo.ItemStack{
			dnfrevivecoin.WalletKey(): {ItemID: 1, Count: 4},
		}}
		slot, err := currentQuestDropGrantStack(inventory, dungeonDropItemDefinition{
			ItemID: itemID, Kind: dungeonDropItemStackable, SlotStart: 177, SlotEnd: 232,
		}, 6)
		if err != nil {
			t.Fatalf("item %d: %v", itemID, err)
		}
		if slot != dnfrevivecoin.WalletSlot {
			t.Fatalf("item %d: slot=%d want %d", itemID, slot, dnfrevivecoin.WalletSlot)
		}
		wallet := inventory.Slots[dnfrevivecoin.WalletKey()]
		if wallet.ItemID != dnfrevivecoin.WalletItemID || wallet.Count != 10 {
			t.Fatalf("item %d: wallet=%+v want item 1 x10", itemID, wallet)
		}
		if len(inventory.Slots) != 1 {
			t.Fatalf("item %d: a loose bag row was left behind: %+v", itemID, inventory.Slots)
		}
	}
}

// Cera / NPC shop products already route through grantCurrentCeraShopProduct;
// pin that both revive-coin ids come back as the wallet slot so the NPC shop
// buy path's row verification keeps matching.
func TestGrantCurrentCeraShopProductReviveCoinReturnsTheWalletSlot(t *testing.T) {
	for _, itemID := range []uint32{1, 42} {
		inventory := &dnfrepo.InventoryRecord{CharacterID: "7", Slots: map[string]dnfrepo.ItemStack{}}
		// The 4th argument is this tree's addition to the helper
		// (CharacterContainerState.MainSlotCount, see npc_shop_guild_medal_grant.go);
		// upstream's test passes three. 24 is the unlocked-row count the other
		// grant tests here use, and the wallet branch ignores placement anyway.
		slots, err := grantCurrentCeraShopProduct(inventory, dungeonDropItemDefinition{
			ItemID: itemID, Kind: dungeonDropItemStackable, SlotStart: 121, SlotEnd: 176,
		}, 2, 24)
		if err != nil || len(slots) != 1 || slots[0] != uint16(dnfrevivecoin.WalletSlot) {
			t.Fatalf("item %d: slots=%v err=%v", itemID, slots, err)
		}
		wallet := inventory.Slots[dnfrevivecoin.WalletKey()]
		if wallet.ItemID != dnfrevivecoin.WalletItemID || wallet.Count != 2 {
			t.Fatalf("item %d: wallet=%+v want item 1 x2", itemID, wallet)
		}
	}
}
