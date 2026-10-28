package dnfbridge

import (
	"context"
	"strings"
	"time"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

// Lazy repair for the Cera shop rename card that ended up in a bag.
//
// Background (live incident 2026-10-02/03, character 3333): commodity 100902
// sold item 2682290 - stackable/cash/chn_character_rename.stk, whose document
// declares no [action type] - so characterRenameCardKind did not recognize it
// and the purchase was classified as an ordinary stackable. The card went into
// the bag, the character never received the pending-rename entitlement, and the
// client will not send op509 for a bag item that does not declare the action
// type. The purchase bought nothing.
//
// characterRenameCardKind now recognizes the shop product by its path, so new
// purchases are consumed at checkout and never reach a bag. This file only
// cleans up the copies that are already sitting in bags.
//
// Rules, confirmed with the operator:
//   - a character inside the 90-day rename cooldown is left completely alone
//     (arming it would produce a character that is forced to rename but is not
//     allowed to, which locks the account out of the game);
//   - once the cooldown has passed and the card is still in the bag, the card is
//     deleted and the pending-rename entitlement is armed.
const currentCharacterRenameShopCardItemID int64 = 2682290

// currentCeraShopRenameProductCommodityID is the Cera-shop commodity that sells
// that item (etc/cerashop.etc: `100902 2682290 1 0 0 8800`). It is only used by
// diagnostics and tests - the runtime recognizes the product by the item it
// resolves to, never by the commodity number.
const currentCeraShopRenameProductCommodityID uint32 = 100902

// currentCharacterRenameShopCardStatus is the per-character outcome, used for
// logging and asserted by tests. A character with nothing to repair reports
// currentCharacterRenameShopCardNone.
type currentCharacterRenameShopCardStatus int

const (
	currentCharacterRenameShopCardNone currentCharacterRenameShopCardStatus = iota
	currentCharacterRenameShopCardRepaired
	currentCharacterRenameShopCardCooldown
)

// repairCurrentCharacterRenameShopCardsForAccount is the lazy entry point: every
// login the character selector loads the account's roster through
// listCharactersWithLimit, and any character still holding the shop card is
// repaired there, before the roster rows are built - so the next character
// select already projects the pending-rename marker.
//
// It reports whether any durable state changed, so the caller only pays for a
// re-read of the roster when there was actually something to repair.
func (s *Service) repairCurrentCharacterRenameShopCardsForAccount(ctx context.Context, accountID string, characters []dnfrepo.CharacterRecord) bool {
	if s == nil || len(characters) == 0 {
		return false
	}
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.Inventory == nil || repositories.Character == nil {
		return false
	}
	now := s.gameplayNow().UTC()
	repaired := false
	for _, character := range characters {
		characterID := strings.TrimSpace(character.CharacterID)
		if characterID == "" {
			continue
		}
		status, cardCount, cooldownUntil, err := s.repairCurrentCharacterRenameShopCards(ctx, repositories, characterID, now)
		if err != nil {
			s.logPacketEvent("game-character-rename-card-bag-repair-failed",
				"character_id", characterID, "error", err)
			continue
		}
		switch status {
		case currentCharacterRenameShopCardRepaired:
			repaired = true
			s.logPacketEvent("game-character-rename-card-bag-repaired",
				"character_id", characterID,
				"cards", cardCount,
				"entitlement", 1,
				"sequence", "bag_card_deleted_then_rename_armed")
		case currentCharacterRenameShopCardCooldown:
			// Report only: the operator asked for no action at all inside the
			// window. The card stays where it is and is retried on a later login
			// once the window has passed.
			s.logPacketEvent("game-character-rename-card-bag-repair-deferred",
				"character_id", characterID,
				"cards", cardCount,
				"cooldown_until_unix", cooldownUntil,
				"reason", "rename_cooldown_active")
		}
	}
	return repaired
}

// repairCurrentCharacterRenameShopCards deletes the shop rename card from one
// character's main inventory and arms the pending-rename entitlement in a single
// transaction, so a failure can never take the card without granting the
// entitlement. It is idempotent: the entitlement is written as exactly 1, never
// incremented, so repeated logins cannot stack entitlements.
//
// The inventory repository is deliberately the plain one rather than the
// Cera-shop transaction scope: this repair owns no Cera purchase, and the plain
// inventory scope is what every other mutation of a bag row already uses.
func (s *Service) repairCurrentCharacterRenameShopCards(
	ctx context.Context,
	repositories dnfrepo.Group,
	characterID string,
	now time.Time,
) (currentCharacterRenameShopCardStatus, int, int64, error) {
	if repositories.Inventory == nil || repositories.Character == nil || strings.TrimSpace(characterID) == "" {
		return currentCharacterRenameShopCardNone, 0, 0, errCurrentCeraShopOwnerUnavailable
	}
	inventory, found, err := repositories.Inventory.Load(ctx, characterID)
	if err != nil {
		return currentCharacterRenameShopCardNone, 0, 0, err
	}
	if !found || strings.TrimSpace(inventory.CharacterID) != characterID {
		return currentCharacterRenameShopCardNone, 0, 0, nil
	}

	slots, cardCount := currentCharacterRenameShopCardSlots(inventory)
	if cardCount == 0 {
		return currentCharacterRenameShopCardNone, 0, 0, nil
	}

	character, found, err := repositories.Character.Load(ctx, characterID)
	if err != nil {
		return currentCharacterRenameShopCardNone, cardCount, 0, err
	}
	if !found || strings.TrimSpace(character.CharacterID) != characterID {
		return currentCharacterRenameShopCardNone, cardCount, 0, nil
	}

	cooldownUntil := int64(0)
	if character.Stats != nil {
		if lastRename := character.Stats[currentCharacterRenameLastUnixStat]; lastRename > 0 {
			cooldownUntil = lastRename + int64(currentCharacterRenameCooldown/time.Second)
			if now.Unix() < cooldownUntil {
				return currentCharacterRenameShopCardCooldown, cardCount, cooldownUntil, nil
			}
		}
	}

	// Nothing to do if the character is already armed, but the stray card must
	// still go: otherwise the player keeps a bag item the client will not use.
	grantEntitlement := character.Stats == nil || character.Stats[currentCharacterRenameEntitlementStat] == 0
	for _, key := range slots {
		delete(inventory.Slots, key)
	}
	if grantEntitlement {
		if character.Stats == nil {
			character.Stats = make(map[string]int64, 2)
		}
		character.Stats[currentCharacterRenameEntitlementStat] = 1
		character.UpdatedAt = now
	}
	inventory.UpdatedAt = now

	if grantEntitlement {
		if err := dnfrepo.SaveCharacterFields(ctx, repositories.Character, character, dnfrepo.CharacterFieldStats); err != nil {
			return currentCharacterRenameShopCardNone, cardCount, cooldownUntil, err
		}
	}
	if err := dnfrepo.SaveInventoryFields(ctx, repositories.Inventory, inventory, dnfrepo.InventoryFieldSlots); err != nil {
		return currentCharacterRenameShopCardNone, cardCount, cooldownUntil, err
	}
	return currentCharacterRenameShopCardRepaired, cardCount, cooldownUntil, nil
}

// currentCharacterRenameShopCardSlots returns the main-inventory keys holding the
// shop rename card and how many cards they add up to. Only the main bag is
// considered: the shop delivers there, and every other container (personal
// cargo, account cargo, mail attachments) holds items the player moved or was
// sent deliberately.
func currentCharacterRenameShopCardSlots(inventory dnfrepo.InventoryRecord) ([]string, int) {
	if len(inventory.Slots) == 0 {
		return nil, 0
	}
	// Slot keys are "<listType>:<slot>" (see currentCeraShopInventorySlotKey).
	mainBagPrefix := currentCeraShopInventorySlotKey(dnfrepo.MainInventoryListType, 0)
	mainBagPrefix = mainBagPrefix[:strings.IndexByte(mainBagPrefix, ':')+1]
	var keys []string
	count := 0
	for key, stack := range inventory.Slots {
		if stack.ItemID != currentCharacterRenameShopCardItemID || stack.Count <= 0 {
			continue
		}
		if !strings.HasPrefix(key, mainBagPrefix) {
			continue
		}
		keys = append(keys, key)
		count += int(stack.Count)
	}
	return keys, count
}
