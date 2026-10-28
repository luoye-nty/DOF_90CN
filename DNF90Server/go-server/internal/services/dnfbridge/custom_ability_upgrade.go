package dnfbridge

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"time"

	"longheng.io/server/internal/modules/dnf/adventuregroup"
	"longheng.io/server/internal/modules/dnf/alignedcmd"
	dnfenum "longheng.io/server/internal/modules/dnf/dnfenum"
	dnfproto "longheng.io/server/internal/modules/dnf/protocol"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

const currentCustomAbilityUpgradeRequestSize = 2

var (
	errCurrentCustomAbilityUpgradeInvalid  = errors.New("current custom ability upgrade request is invalid")
	errCurrentCustomAbilityUpgradeMaterial = errors.New("current custom ability upgrade material is insufficient")
	errCurrentCustomAbilityUpgradeMaximum  = errors.New("current custom ability grade is already maximum")
)

type currentCustomAbilityUpgradeResult struct {
	itemID            uint32
	oldGrade          byte
	newGrade          byte
	material          currentCustomAbilityMaterialCost
	materialRemaining int64
	refreshes         []alignedcmd.ItemSlotRefresh
}

func buildCurrentCustomAbilityUpgradeSuccessBody(state adventuregroup.RuntimeState, now time.Time) []byte {
	var body packetWriter
	body.writeByte(1)
	body.writeBytes(buildCurrentAdventureExpeditionStateBody(state, now))
	return body.bytes()
}

func decodeCurrentCustomAbilityUpgradeRequest(body []byte) (int16, error) {
	if len(body) != currentCustomAbilityUpgradeRequestSize {
		return 0, errCurrentCustomAbilityUpgradeInvalid
	}
	slot := int16(binary.LittleEndian.Uint16(body))
	if slot < 0 {
		return 0, errCurrentCustomAbilityUpgradeInvalid
	}
	return slot, nil
}

func commitCurrentCustomAbilityUpgrade(
	ctx context.Context,
	repositories dnfrepo.Group,
	characterID string,
	targetSlot int16,
	expectedItemID uint32,
	catalog *currentCustomAbilityCatalog,
) (currentCustomAbilityUpgradeResult, error) {
	if repositories.CharacterItems == nil || catalog == nil || catalog.upgradeMax == 0 {
		return currentCustomAbilityUpgradeResult{}, errCurrentCustomAbilityUpgradeInvalid
	}
	var result currentCustomAbilityUpgradeResult
	err := repositories.CharacterItems.WithinCharacterItems(ctx, characterID, func(inventoryRepo dnfrepo.InventoryRepository, _ dnfrepo.EquipmentRepository) error {
		inventory, found, err := inventoryRepo.Load(ctx, characterID)
		if err != nil {
			return err
		}
		if !found {
			return errCurrentCustomAbilityUpgradeInvalid
		}
		inventory = dnfrepo.CloneInventory(inventory)
		targetKey := currentCeraShopInventorySlotKey(dnfrepo.MainInventoryListType, targetSlot)
		target, found := inventory.Slots[targetKey]
		if !found || target.ItemID != int64(expectedItemID) || target.Count != 1 {
			return errCurrentCustomAbilityUpgradeInvalid
		}
		targetEntry := currentItemListEntryFromStack(dnfrepo.MainInventoryListType, targetSlot, target)
		state, valid := currentCustomAbilityStateFromRaw(targetEntry.data[:], catalog)
		if !valid {
			return errCurrentCustomAbilityUpgradeInvalid
		}
		if state.grade >= catalog.upgradeMax-1 {
			return errCurrentCustomAbilityUpgradeMaximum
		}
		newGrade := state.grade + 1
		material, found := catalog.upgradeMaterial[newGrade]
		if !found || material.itemID == 0 || material.count <= 0 {
			return errCurrentCustomAbilityUpgradeInvalid
		}

		materialSlots := currentCompoundSlotsForItem(inventory.Slots, int64(material.itemID))
		available := int64(0)
		for _, slot := range materialSlots {
			available += inventory.Slots[currentCeraShopInventorySlotKey(dnfrepo.MainInventoryListType, slot)].Count
		}
		if available < material.count {
			return errCurrentCustomAbilityUpgradeMaterial
		}

		refreshes := []alignedcmd.ItemSlotRefresh{{ListType: dnfrepo.MainInventoryListType, SlotIndex: targetSlot}}
		left := material.count
		for _, slot := range materialSlots {
			if left == 0 {
				break
			}
			key := currentCeraShopInventorySlotKey(dnfrepo.MainInventoryListType, slot)
			stack := inventory.Slots[key]
			take := stack.Count
			if take > left {
				take = left
			}
			stack.Count -= take
			left -= take
			if stack.Count == 0 {
				delete(inventory.Slots, key)
			} else {
				entry := currentItemListEntryFromStack(dnfrepo.MainInventoryListType, slot, stack)
				stack.RawEntry = append([]byte(nil), entry.data[:]...)
				inventory.Slots[key] = stack
			}
			refreshes = append(refreshes, alignedcmd.ItemSlotRefresh{ListType: dnfrepo.MainInventoryListType, SlotIndex: slot})
		}

		state.grade = newGrade
		target.RawEntry, target.Extra = applyCurrentCustomAbilityState(targetEntry.data[:], target.Extra, state)
		inventory.Slots[targetKey] = target
		inventory.UpdatedAt = time.Now().UTC()
		if err := dnfrepo.SaveInventoryFields(ctx, inventoryRepo, inventory, dnfrepo.InventoryFieldSlots); err != nil {
			return err
		}
		sort.Slice(refreshes, func(i, j int) bool { return refreshes[i].SlotIndex < refreshes[j].SlotIndex })
		result = currentCustomAbilityUpgradeResult{
			itemID:            expectedItemID,
			oldGrade:          state.grade - 1,
			newGrade:          state.grade,
			material:          material,
			materialRemaining: available - material.count,
			refreshes:         refreshes,
		}
		return nil
	})
	return result, err
}

func (s *Service) handleCurrentCustomAbilityUpgrade(session *gameSession, body []byte) error {
	slot, err := decodeCurrentCustomAbilityUpgradeRequest(body)
	if err != nil || session == nil || session.selectedCharacterID == 0 {
		if err == nil {
			err = errCurrentCustomAbilityUpgradeInvalid
		}
		s.logGameEvent(session, "game-custom-ability-upgrade-rejected", "reason", err, "body_len", len(body))
		return s.sendGameUpperRawClass(session, uint16(dnfenum.CmdPacketCustomAbilityUpgrade), []byte{0}, dnfproto.DefaultChannelClassification)
	}

	characterID := strconv.Itoa(int(session.selectedCharacterID))
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.Inventory == nil || repositories.CharacterItems == nil {
		return fmt.Errorf("custom ability upgrade repositories unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), currentRepositorySnapshotTimeout)
	defer cancel()
	inventory, found, err := repositories.Inventory.Load(ctx, characterID)
	if err != nil {
		return err
	}
	target, targetFound := inventory.Slots[currentCeraShopInventorySlotKey(dnfrepo.MainInventoryListType, slot)]
	if !found || !targetFound || target.ItemID <= 0 || target.ItemID > int64(^uint32(0)) || target.Count != 1 {
		err = errCurrentCustomAbilityUpgradeInvalid
	} else {
		profile, profileErr := s.resolveCurrentCustomAbilityItemProfile(uint32(target.ItemID))
		if profileErr != nil {
			err = profileErr
		} else if !profile.applicable || profile.optionCount != 2 {
			err = errCurrentCustomAbilityUpgradeInvalid
		}
	}

	var catalog *currentCustomAbilityCatalog
	if err == nil {
		s.initialEquipmentMu.Lock()
		archive, archiveErr := s.initialEquipmentArchiveLocked()
		s.initialEquipmentMu.Unlock()
		if archiveErr != nil {
			err = archiveErr
		} else {
			catalog, err = parseCurrentCustomAbilityCatalog(archive)
		}
	}

	var result currentCustomAbilityUpgradeResult
	var successBody []byte
	if err == nil {
		s.adventureGroupRuntimeMu.Lock()
		adventureState, adventureErr := s.currentAdventureSelectedState(ctx, session)
		s.adventureGroupRuntimeMu.Unlock()
		if adventureErr != nil {
			err = adventureErr
		} else {
			successBody = buildCurrentCustomAbilityUpgradeSuccessBody(adventureState.Runtime, s.gameplayNow())
		}
	}
	if err == nil {
		result, err = commitCurrentCustomAbilityUpgrade(ctx, repositories, characterID, slot, uint32(target.ItemID), catalog)
	}
	if err != nil {
		s.logGameEvent(session, "game-custom-ability-upgrade-rejected",
			"slot", slot,
			"item_id", target.ItemID,
			"reason", err,
			"body_len", len(body))
		return s.sendGameUpperRawClass(session, uint16(dnfenum.CmdPacketCustomAbilityUpgrade), []byte{0}, dnfproto.DefaultChannelClassification)
	}

	s.logGameEvent(session, "game-custom-ability-upgrade-committed",
		"slot", slot,
		"item_id", result.itemID,
		"grade_before", result.oldGrade,
		"grade_after", result.newGrade,
		"material_item_id", result.material.itemID,
		"material_cost", result.material.count,
		"material_remaining", result.materialRemaining,
		"refresh_rows", len(result.refreshes),
		"sequence", "op14_list0_then_class1_op1342_success")
	if err := s.sendSelectedIncrementalItemSlotRefreshes(session, "custom_ability_upgrade_commit", result.refreshes); err != nil {
		return err
	}
	return s.sendGameUpperRawClass(session, uint16(dnfenum.CmdPacketCustomAbilityUpgrade), successBody, dnfproto.DefaultChannelClassification)
}
