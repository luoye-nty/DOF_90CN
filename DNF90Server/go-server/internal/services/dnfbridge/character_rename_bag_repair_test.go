package dnfbridge

import (
	"context"
	"strconv"
	"testing"
	"time"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

// newRenameRepairTestService builds the minimum service the repair needs: only
// the repository provider is exercised, no PVF catalog and no packet logger.
func newRenameRepairTestService(repositories dnfrepo.Group) *Service {
	return &Service{
		options:            options{accountID: "account-1"},
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
	}
}

func seedRenameRepairCharacter(
	t *testing.T,
	ctx context.Context,
	repositories dnfrepo.Group,
	characterID string,
	stats map[string]int64,
	slots map[string]dnfrepo.ItemStack,
) {
	t.Helper()
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: characterID,
		AccountID:   "account-1",
		Level:       90,
		Stats:       stats,
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{CharacterID: characterID, Slots: slots}); err != nil {
		t.Fatal(err)
	}
}

func loadRenameRepairInventory(t *testing.T, ctx context.Context, repositories dnfrepo.Group, characterID string) dnfrepo.InventoryRecord {
	t.Helper()
	inventory, found, err := repositories.Inventory.Load(ctx, characterID)
	if err != nil || !found {
		t.Fatalf("load inventory %s found=%t err=%v", characterID, found, err)
	}
	return inventory
}

func loadRenameRepairCharacter(t *testing.T, ctx context.Context, repositories dnfrepo.Group, characterID string) dnfrepo.CharacterRecord {
	t.Helper()
	character, found, err := repositories.Character.Load(ctx, characterID)
	if err != nil || !found {
		t.Fatalf("load character %s found=%t err=%v", characterID, found, err)
	}
	return character
}

// TestCharacterRenameShopCardRepairDeletesTheBagCardAndArmsTheRename is the live
// incident, reproduced: character 3333 bought commodity 100902 (item 2682290),
// which landed in bag slot 0:87 because the product was not recognized as a
// rename card. The repair must delete that row and arm the pending rename in one
// step, and must not disturb any other row.
func TestCharacterRenameShopCardRepairDeletesTheBagCardAndArmsTheRename(t *testing.T) {
	repositories := dnfrepomemory.NewMemoryGroup()
	ctx := context.Background()
	cardKey := currentCeraShopInventorySlotKey(dnfrepo.MainInventoryListType, 87)
	otherKey := currentCeraShopInventorySlotKey(dnfrepo.MainInventoryListType, 5)
	seedRenameRepairCharacter(t, ctx, repositories, "5",
		map[string]int64{"fatigue_battery_charge": 834},
		map[string]dnfrepo.ItemStack{
			cardKey:  {ItemID: currentCharacterRenameShopCardItemID, Count: 1},
			otherKey: {ItemID: 2660671, Count: 400},
		},
	)

	service := newRenameRepairTestService(repositories)
	if !service.repairCurrentCharacterRenameShopCardsForAccount(ctx, "account-1", []dnfrepo.CharacterRecord{{CharacterID: "5"}}) {
		t.Fatal("repair reported no change for a bag holding the shop rename card")
	}

	inventory := loadRenameRepairInventory(t, ctx, repositories, "5")
	if _, stillThere := inventory.Slots[cardKey]; stillThere {
		t.Fatalf("shop rename card row %s survived the repair: %+v", cardKey, inventory.Slots[cardKey])
	}
	if other := inventory.Slots[otherKey]; other.ItemID != 2660671 || other.Count != 400 {
		t.Fatalf("repair touched an unrelated row: %+v", other)
	}
	character := loadRenameRepairCharacter(t, ctx, repositories, "5")
	if got := character.Stats[currentCharacterRenameEntitlementStat]; got != 1 {
		t.Fatalf("entitlement=%d want 1 (stats=%v)", got, character.Stats)
	}
	if got := character.Stats["fatigue_battery_charge"]; got != 834 {
		t.Fatalf("repair dropped an unrelated stat: %v", character.Stats)
	}
}

// TestCharacterRenameShopCardRepairIsIdempotentAndRemovesAStrayCardWhenArmed
// locks two properties: a repaired character is not reported as changed on the
// next login (so the roster is not re-read forever), and a stray shop card is
// still deleted for a character that is already armed without the entitlement
// being double counted.
func TestCharacterRenameShopCardRepairIsIdempotentAndRemovesAStrayCardWhenArmed(t *testing.T) {
	repositories := dnfrepomemory.NewMemoryGroup()
	ctx := context.Background()
	cardKey := currentCeraShopInventorySlotKey(dnfrepo.MainInventoryListType, 87)
	seedRenameRepairCharacter(t, ctx, repositories, "5", map[string]int64{},
		map[string]dnfrepo.ItemStack{cardKey: {ItemID: currentCharacterRenameShopCardItemID, Count: 2}},
	)

	service := newRenameRepairTestService(repositories)
	characters := []dnfrepo.CharacterRecord{{CharacterID: "5"}}
	if !service.repairCurrentCharacterRenameShopCardsForAccount(ctx, "account-1", characters) {
		t.Fatal("first repair reported no change")
	}
	// Only lvl 90 / no cooldown, so the whole stack goes and the entitlement is
	// armed exactly once even though two cards were present.
	if _, stillThere := loadRenameRepairInventory(t, ctx, repositories, "5").Slots[cardKey]; stillThere {
		t.Fatal("a second card in the same stack survived")
	}
	if got := loadRenameRepairCharacter(t, ctx, repositories, "5").Stats[currentCharacterRenameEntitlementStat]; got != 1 {
		t.Fatalf("entitlement=%d want exactly 1", got)
	}
	if service.repairCurrentCharacterRenameShopCardsForAccount(ctx, "account-1", characters) {
		t.Fatal("second repair reported a change; a clean roster must not be re-read")
	}

	// Already armed, stray card arrives later: the card goes, the entitlement
	// stays 1 (never incremented).
	seedRenameRepairCharacter(t, ctx, repositories, "6",
		map[string]int64{currentCharacterRenameEntitlementStat: 1},
		map[string]dnfrepo.ItemStack{cardKey: {ItemID: currentCharacterRenameShopCardItemID, Count: 1}},
	)
	if !service.repairCurrentCharacterRenameShopCardsForAccount(ctx, "account-1", []dnfrepo.CharacterRecord{{CharacterID: "6"}}) {
		t.Fatal("repair skipped a stray card on an armed character")
	}
	if _, stillThere := loadRenameRepairInventory(t, ctx, repositories, "6").Slots[cardKey]; stillThere {
		t.Fatal("stray card survived on an armed character")
	}
	if got := loadRenameRepairCharacter(t, ctx, repositories, "6").Stats[currentCharacterRenameEntitlementStat]; got != 1 {
		t.Fatalf("entitlement=%d want 1 (must not stack)", got)
	}
}

// TestCharacterRenameShopCardRepairLeavesCharactersInsideTheCooldownAlone is the
// operator's rule: inside the 90-day window nothing at all happens - the card is
// not deleted and no entitlement is granted, because arming a character that is
// forbidden to rename would lock the account out of the game.
func TestCharacterRenameShopCardRepairLeavesCharactersInsideTheCooldownAlone(t *testing.T) {
	repositories := dnfrepomemory.NewMemoryGroup()
	ctx := context.Background()
	cardKey := currentCeraShopInventorySlotKey(dnfrepo.MainInventoryListType, 87)
	now := time.Now().UTC()
	lastRename := now.Add(-30 * 24 * time.Hour).Unix()
	seedRenameRepairCharacter(t, ctx, repositories, "1",
		map[string]int64{currentCharacterRenameLastUnixStat: lastRename},
		map[string]dnfrepo.ItemStack{cardKey: {ItemID: currentCharacterRenameShopCardItemID, Count: 1}},
	)

	service := newRenameRepairTestService(repositories)
	if service.repairCurrentCharacterRenameShopCardsForAccount(ctx, "account-1", []dnfrepo.CharacterRecord{{CharacterID: "1"}}) {
		t.Fatal("repair changed state for a character inside the cooldown")
	}
	if _, stillThere := loadRenameRepairInventory(t, ctx, repositories, "1").Slots[cardKey]; !stillThere {
		t.Fatal("repair deleted the card of a character inside the cooldown")
	}
	character := loadRenameRepairCharacter(t, ctx, repositories, "1")
	if got := character.Stats[currentCharacterRenameEntitlementStat]; got != 0 {
		t.Fatalf("repair armed a character inside the cooldown: entitlement=%d", got)
	}
	if got := character.Stats[currentCharacterRenameLastUnixStat]; got != lastRename {
		t.Fatalf("repair moved the rename clock: %d want %d", got, lastRename)
	}

	// Past the window the very same character must be repaired on the next login.
	expired := now.Add(-(currentCharacterRenameCooldown + 24*time.Hour)).Unix()
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "1", AccountID: "account-1", Level: 90,
		Stats: map[string]int64{
			currentCharacterRenameLastUnixStat:    expired,
			currentCharacterRenameEntitlementStat: 0,
		},
	}); err != nil {
		t.Fatal(err)
	}
	if !service.repairCurrentCharacterRenameShopCardsForAccount(ctx, "account-1", []dnfrepo.CharacterRecord{{CharacterID: "1"}}) {
		t.Fatal("repair did not run once the cooldown had passed")
	}
	if _, stillThere := loadRenameRepairInventory(t, ctx, repositories, "1").Slots[cardKey]; stillThere {
		t.Fatal("card survived after the cooldown passed")
	}
	if got := loadRenameRepairCharacter(t, ctx, repositories, "1").Stats[currentCharacterRenameEntitlementStat]; got != 1 {
		t.Fatalf("entitlement=%d want 1 after the cooldown passed", got)
	}
}

// TestCharacterRenameShopCardRepairIgnoresEverythingThatIsNotTheShopCard keeps
// the repair surgical: the bag rename card the client can actually use
// (2660698) and the account-bound event cards must not be touched, and the
// personal cargo / warehouse copies are out of scope.
func TestCharacterRenameShopCardRepairIgnoresEverythingThatIsNotTheShopCard(t *testing.T) {
	repositories := dnfrepomemory.NewMemoryGroup()
	ctx := context.Background()
	bagCardKey := currentCeraShopInventorySlotKey(dnfrepo.MainInventoryListType, 117)
	cargoCardKey := currentCeraShopInventorySlotKey(currentExeInventoryPersonalCargoListType, 3)
	seedRenameRepairCharacter(t, ctx, repositories, "1", map[string]int64{},
		map[string]dnfrepo.ItemStack{
			// 490000735 is the account-bound bag card the operator holds 100 of.
			bagCardKey:   {ItemID: 490000735, Count: 100},
			cargoCardKey: {ItemID: currentCharacterRenameShopCardItemID, Count: 1},
		},
	)

	service := newRenameRepairTestService(repositories)
	if service.repairCurrentCharacterRenameShopCardsForAccount(ctx, "account-1", []dnfrepo.CharacterRecord{{CharacterID: "1"}}) {
		t.Fatal("repair ran for a bag that holds no shop rename card")
	}
	inventory := loadRenameRepairInventory(t, ctx, repositories, "1")
	if stack := inventory.Slots[bagCardKey]; stack.ItemID != 490000735 || stack.Count != 100 {
		t.Fatalf("repair touched the bag rename card: %+v", stack)
	}
	if stack := inventory.Slots[cargoCardKey]; stack.ItemID != currentCharacterRenameShopCardItemID {
		t.Fatalf("repair touched the personal cargo copy: %+v", stack)
	}
	if got := loadRenameRepairCharacter(t, ctx, repositories, "1").Stats[currentCharacterRenameEntitlementStat]; got != 0 {
		t.Fatalf("repair armed a character holding no shop card: entitlement=%d", got)
	}
}

// TestCharacterRenameShopCardRepairTargetAndScopeAreLocked pins the two things
// this repair keys on: the item id it hunts for (so a PVF change that renumbers
// the shop product cannot leave it hunting a ghost) and the container scope (main
// bag only, via the slot-key prefix).
func TestCharacterRenameShopCardRepairTargetAndScopeAreLocked(t *testing.T) {
	if currentCharacterRenameShopCardItemID != 2682290 {
		t.Fatalf("shop rename card item id changed to %d; update the repair and this lock together",
			currentCharacterRenameShopCardItemID)
	}
	if !currentCharacterRenameAutoUsePVFPath("stackable/cash/chn_character_rename.stk") {
		t.Fatal("the repair's item id no longer belongs to the auto-use rename product path")
	}
	// The scope test is what keeps a personal-cargo copy out of the repair.
	mainBag := currentCeraShopInventorySlotKey(dnfrepo.MainInventoryListType, 87)
	cargo := currentCeraShopInventorySlotKey(currentExeInventoryPersonalCargoListType, 3)
	inventory := dnfrepo.InventoryRecord{CharacterID: "1", Slots: map[string]dnfrepo.ItemStack{
		mainBag: {ItemID: currentCharacterRenameShopCardItemID, Count: 1},
		cargo:   {ItemID: currentCharacterRenameShopCardItemID, Count: 1},
	}}
	keys, count := currentCharacterRenameShopCardSlots(inventory)
	if count != 1 || len(keys) != 1 || keys[0] != mainBag {
		t.Fatalf("scope keys=%v count=%d want only %s", keys, count, mainBag)
	}
	if got := strconv.Itoa(int(dnfrepo.MainInventoryListType)); got != "0" {
		t.Fatalf("main inventory list type changed to %s; the scope prefix is stale", got)
	}
}
