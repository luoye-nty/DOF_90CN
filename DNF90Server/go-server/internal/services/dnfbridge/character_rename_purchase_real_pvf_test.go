package dnfbridge

import (
	"context"
	"os"
	"testing"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

// TestRealPVFRenameCardPurchaseNeverReachesTheBag is the regression lock for the
// live 2026-10-02 incident, end to end against the operator's actual Script.pvf.
//
// Commodity 100902 sells item 2682290 (stackable/cash/chn_character_rename.stk),
// whose document declares no [action type]. characterRenameCardKind used to
// require that declaration even for this path, so the product was classified as
// an ordinary stackable: the 8800 Cera was charged, the card was delivered to the
// bag, and the character never received the pending-rename entitlement. The
// client does not send op509 for a bag item without the action type, so the
// purchase bought nothing at all.
//
// Set DNFBRIDGE_REAL_PVF_SMOKE to the runtime PVF, e.g.
//
//	DNFBRIDGE_REAL_PVF_SMOKE=runtime\data\dnf\Script.pvf
func TestRealPVFRenameCardPurchaseNeverReachesTheBag(t *testing.T) {
	pvfPath := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE to verify the rename product checkout against the runtime PVF")
	}
	archive, err := platformpvf.LoadArchive(platformpvf.Options{Path: pvfPath})
	if err != nil {
		t.Fatalf("load real Script.pvf: %v", err)
	}
	catalog, err := newPVFCeraShopCatalog(archive)
	if err != nil {
		t.Fatalf("build cera shop catalog: %v", err)
	}

	// The product must resolve to the item the incident was about; if the runtime
	// PVF is ever renumbered this test says so instead of testing nothing.
	product, found := catalog.Product(currentCeraShopRenameProductCommodityID)
	if !found {
		t.Fatalf("commodity %d is not in the runtime Cera-shop [item] section", currentCeraShopRenameProductCommodityID)
	}
	if product.ItemID != uint32(currentCharacterRenameShopCardItemID) {
		t.Fatalf("commodity %d sells item %d, want %d",
			currentCeraShopRenameProductCommodityID, product.ItemID, currentCharacterRenameShopCardItemID)
	}
	definition, resolveErr := catalog.items.ResolveItem(product.ItemID)
	if resolveErr != nil {
		t.Fatalf("resolve item %d: %v", product.ItemID, resolveErr)
	}
	autoUse, recognized := catalog.characterRenameCardKind(definition)
	if !recognized || !autoUse {
		t.Fatalf("item %d (%s) recognized=%t autoUse=%t, want both true",
			product.ItemID, definition.PVFPath, recognized, autoUse)
	}

	repositories := dnfrepomemory.NewMemoryGroup()
	ctx := context.Background()
	seedCeraShopAccountCera(t, ctx, repositories, "account-1", 20000)
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "5",
		AccountID:   "account-1",
		Level:       90,
		Stats:       map[string]int64{currentCharacterRenameEntitlementStat: 0},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{CharacterID: "5", Slots: map[string]dnfrepo.ItemStack{}}); err != nil {
		t.Fatal(err)
	}

	connection := &bufferConn{}
	service := newCeraShopPremiumTestService(repositories, catalog)
	session := &gameSession{conn: connection, connID: "real-pvf-rename-purchase", selectedCharacterID: 5}
	if err := service.handleCurrentCeraShopPurchase(session, currentCeraShopTestRequestBody(currentCeraShopRenameProductCommodityID)); err != nil {
		t.Fatalf("purchase: %v", err)
	}

	inventory, foundInventory, err := repositories.Inventory.Load(ctx, "5")
	if err != nil || !foundInventory {
		t.Fatalf("load inventory found=%t err=%v", foundInventory, err)
	}
	if len(inventory.Slots) != 0 {
		t.Fatalf("the rename card reached the bag: %+v", inventory.Slots)
	}
	character, foundCharacter, err := repositories.Character.Load(ctx, "5")
	if err != nil || !foundCharacter {
		t.Fatalf("load character found=%t err=%v", foundCharacter, err)
	}
	if got := character.Stats[currentCharacterRenameEntitlementStat]; got != 1 {
		t.Fatalf("entitlement=%d want 1 (stats=%v)", got, character.Stats)
	}
}
