package dnfbridge

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

func (s *Service) commitCurrentAvatarOptionChange(
	ctx context.Context,
	session *gameSession,
	catalog *pvfDungeonDropCatalog,
	request currentAvatarOptionChangeRequest,
) (currentAvatarOptionChangeResult, error) {
	if s == nil || session == nil || session.selectedCharacterID == 0 || catalog == nil || catalog.source == nil {
		return currentAvatarOptionChangeResult{}, errCurrentAvatarOptionChangeUnavailable
	}
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.CharacterAssets == nil {
		return currentAvatarOptionChangeResult{}, errCurrentAvatarOptionChangeUnavailable
	}
	accountID := strings.TrimSpace(s.accountIDForSession(session))
	if accountID == "" {
		return currentAvatarOptionChangeResult{}, errCurrentAvatarOptionChangeUnavailable
	}
	characterID := strconv.FormatUint(uint64(session.selectedCharacterID), 10)
	var result currentAvatarOptionChangeResult
	err := repositories.CharacterAssets.WithinCharacterAssets(ctx, characterID, func(
		characters dnfrepo.CharacterRepository,
		inventories dnfrepo.InventoryRepository,
		_ dnfrepo.EquipmentRepository,
	) error {
		if characters == nil || inventories == nil {
			return errCurrentAvatarOptionChangeUnavailable
		}
		character, found, err := characters.Load(ctx, characterID)
		if err != nil || !found || strings.TrimSpace(character.CharacterID) != characterID || strings.TrimSpace(character.AccountID) != accountID {
			return errors.Join(errCurrentAvatarOptionChangeUnavailable, err)
		}
		inventory, found, err := inventories.Load(ctx, characterID)
		if err != nil || !found || strings.TrimSpace(inventory.CharacterID) != characterID || inventory.Slots == nil {
			return errors.Join(errCurrentAvatarOptionChangeUnavailable, err)
		}
		inventory = dnfrepo.CloneInventory(inventory)

		sourceKey := currentCeraShopInventorySlotKey(dnfrepo.MainInventoryListType, request.SourceSlot)
		source, found := inventory.Slots[sourceKey]
		if !found || source.ItemID != int64(request.SourceItemID) || source.Count <= 0 || currentNPCShopItemLocked(source) {
			return errCurrentAvatarOptionChangeToolInvalid
		}
		toolDefinition, err := catalog.ResolveItem(request.SourceItemID)
		if err != nil || toolDefinition.Kind != dungeonDropItemStackable {
			return errors.Join(errCurrentAvatarOptionChangeToolInvalid, err)
		}
		toolDocument, err := parseDungeonCardPVFDocument(catalog.source, toolDefinition.PVFPath)
		if err != nil {
			return errors.Join(errCurrentAvatarOptionChangeToolInvalid, err)
		}

		targetKey := currentCeraShopInventorySlotKey(currentAvatarInventoryListType, request.TargetSlot)
		target, found := inventory.Slots[targetKey]
		if !found || target.ItemID != int64(request.TargetItemID) || target.Count != 1 || currentNPCShopItemLocked(target) {
			return errCurrentAvatarOptionChangeTargetInvalid
		}
		targetDefinition, _, err := currentAvatarOptionChangeAvatarDefinition(catalog, request.TargetItemID)
		if err != nil {
			return err
		}
		if !currentAvatarOptionChangeToolAllowsGrade(toolDocument, targetDefinition.Grade) {
			return fmt.Errorf("%w: source=%d target=%d grade=%d", errCurrentAvatarOptionChangeToolInvalid, request.SourceItemID, request.TargetItemID, targetDefinition.Grade)
		}

		source.Count--
		result.SourceRemaining = source.Count
		if source.Count == 0 {
			delete(inventory.Slots, sourceKey)
		} else {
			entry := currentItemListEntryFromStack(dnfrepo.MainInventoryListType, request.SourceSlot, source)
			source.RawEntry = append([]byte(nil), entry.data[:]...)
			inventory.Slots[sourceKey] = source
		}

		target.Extra = currentAvatarOptionChangeExtra(target.Extra, targetDefinition)
		target.Extra["avatar_ability_no"] = strconv.FormatUint(uint64(request.AbilityNo), 10)
		target.Extra["durability"] = strconv.FormatUint(uint64(request.AbilityNo), 10)
		targetEntry := currentItemListEntryFromStack(currentAvatarInventoryListType, request.TargetSlot, target)
		target.RawEntry = append([]byte(nil), targetEntry.data[:]...)
		inventory.Slots[targetKey] = target

		inventory.UpdatedAt = time.Now().UTC()
		return dnfrepo.SaveInventoryFields(ctx, inventories, inventory, dnfrepo.InventoryFieldSlots)
	})
	if err != nil {
		return currentAvatarOptionChangeResult{}, err
	}
	return result, nil
}

func currentAvatarOptionChangeAvatarDefinition(catalog *pvfDungeonDropCatalog, itemID uint32) (dungeonDropItemDefinition, *dnfpvf.Document, error) {
	definition, err := catalog.ResolveItem(itemID)
	if err != nil || definition.Kind != dungeonDropItemEquipment ||
		(!definition.ExpirationDate.IsZero() && !time.Now().UTC().Before(definition.ExpirationDate)) {
		return dungeonDropItemDefinition{}, nil, errors.Join(errCurrentAvatarOptionChangeTargetInvalid, err)
	}
	document, err := parseDungeonCardPVFDocument(catalog.source, definition.PVFPath)
	if err != nil || !currentDisjointDocumentIsAvatar(document) {
		return dungeonDropItemDefinition{}, nil, errors.Join(errCurrentAvatarOptionChangeTargetInvalid, err)
	}
	return definition, document, nil
}

func currentAvatarOptionChangeToolAllowsGrade(document *dnfpvf.Document, grade int64) bool {
	if document == nil || grade < 0 {
		return false
	}
	values, found := document.Section("avatar select ability change")
	if !found {
		return false
	}
	// Each PVF row is two selector tokens followed by grade and the item's
	// opaque mode flag.  The tool governs grade only; the target itself is
	// independently constrained to a PVF avatar above.
	integers := make([]int64, 0, len(values)/2)
	for _, value := range values {
		if value.Kind == dnfpvf.TokenInt {
			integers = append(integers, value.Int)
		}
	}
	for offset := 0; offset+1 < len(integers); offset += 2 {
		if integers[offset] == grade {
			return true
		}
	}
	return false
}

func currentAvatarOptionChangeExtra(extra map[string]string, definition dungeonDropItemDefinition) map[string]string {
	cloned := make(map[string]string, len(extra)+4)
	for key, value := range extra {
		cloned[key] = value
	}
	cloned["item_kind"] = "avatar"
	if strings.TrimSpace(cloned["pvf_path"]) == "" {
		cloned["pvf_path"] = definition.PVFPath
	}
	if strings.TrimSpace(cloned["equipment_type"]) == "" {
		cloned["equipment_type"] = definition.EquipmentType
	}
	return cloned
}
