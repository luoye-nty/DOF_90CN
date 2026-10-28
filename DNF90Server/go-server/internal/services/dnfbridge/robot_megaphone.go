package dnfbridge

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	dnfchat "longheng.io/server/internal/modules/dnf/chat"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	"longheng.io/server/internal/services/dnfrobot"
)

const automaticRobotMegaphoneStock = uint32(20)

// PrepareRobotServerMegaphone returns an authoritative main-inventory slot for
// a real item 36. It replenishes only automatic robot characters; the normal
// mode-11 chat owner still consumes one item before the global broadcast.
func (s *Service) PrepareRobotServerMegaphone(ctx context.Context, accountID string, characterID uint16) (int16, error) {
	accountID = strings.TrimSpace(accountID)
	if s == nil || accountID == "" || characterID == 0 {
		return -1, fmt.Errorf("%w: invalid automatic robot megaphone request", ErrGMRequestInvalid)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	catalog, err := s.currentPVFItemCatalog()
	if err != nil {
		return -1, err
	}
	definition, err := catalog.ResolveItem(dnfchat.ServerMegaphoneItemID)
	if err != nil || definition.Kind != dungeonDropItemStackable ||
		cleanDungeonDropPath(definition.PVFPath) != currentServerMegaphonePVFPath ||
		normalizeDungeonDropStackableType(definition.StackableType) != "[waste]" {
		return -1, fmt.Errorf("%w: automatic robot megaphone PVF identity mismatch", ErrGMRequestInvalid)
	}
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.CharacterAssets == nil {
		return -1, ErrGMRepositoryUnavailable
	}
	characterKey := strconv.Itoa(int(characterID))
	resultSlot := int16(-1)
	err = repositories.CharacterAssets.WithinCharacterAssets(ctx, characterKey, func(
		characters dnfrepo.CharacterRepository,
		inventories dnfrepo.InventoryRepository,
		_ dnfrepo.EquipmentRepository,
	) error {
		character, found, loadErr := characters.Load(ctx, characterKey)
		if loadErr != nil {
			return loadErr
		}
		if !found || strings.TrimSpace(character.AccountID) != accountID || !strings.HasPrefix(accountID, automaticRobotAccountPrefix+"-") {
			return fmt.Errorf("%w: automatic robot megaphone identity mismatch", ErrGMRequestInvalid)
		}
		inventory, found, loadErr := inventories.Load(ctx, characterKey)
		if loadErr != nil {
			return loadErr
		}
		if !found || inventory.Slots == nil {
			return ErrGMRepositoryUnavailable
		}
		for key, stack := range inventory.Slots {
			listType, slot, parsed := parseSceneInventorySlotKey(key)
			if !parsed || listType != dnfrepo.MainInventoryListType || slot < 0 ||
				dnfrepo.IsAccountSharedInventorySlot(listType, slot) || stack.ItemID != int64(dnfchat.ServerMegaphoneItemID) || stack.Count < 1 {
				continue
			}
			if resultSlot < 0 || slot < resultSlot {
				resultSlot = slot
			}
		}
		if resultSlot >= 0 {
			return nil
		}
		amount := automaticRobotMegaphoneStock
		if definition.StackLimit > 0 && uint64(amount) > uint64(definition.StackLimit) {
			amount = uint32(definition.StackLimit)
		}
		if amount == 0 {
			amount = 1
		}
		slot, placeErr := addCurrentDungeonPickupToInventory(&inventory, definition, amount)
		if placeErr != nil {
			return placeErr
		}
		key := currentCeraShopInventorySlotKey(dnfrepo.MainInventoryListType, int16(slot))
		stack, found := inventory.Slots[key]
		if !found || stack.ItemID != int64(dnfchat.ServerMegaphoneItemID) || stack.Count < 1 {
			return fmt.Errorf("%w: automatic robot megaphone placement disappeared", ErrGMRequestInvalid)
		}
		if stack.Extra == nil {
			stack.Extra = make(map[string]string)
		}
		stack.Extra["source"] = "automatic_robot_megaphone"
		currentRefreshStackRawEntry(&stack, dnfrepo.MainInventoryListType, int16(slot))
		inventory.Slots[key] = stack
		resultSlot = int16(slot)
		return dnfrepo.SaveInventoryFields(ctx, inventories, inventory, dnfrepo.InventoryFieldSlots)
	})
	if err != nil {
		return -1, err
	}
	if resultSlot < 0 {
		return -1, fmt.Errorf("%w: automatic robot megaphone slot missing", ErrGMRequestInvalid)
	}
	return resultSlot, nil
}

var _ dnfrobot.MegaphonePreparer = (*Service)(nil)
