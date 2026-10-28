package dnfbridge

import (
	"context"
	"encoding/binary"
	"fmt"
	"strconv"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

func currentGuardianGemFindTarget(
	inventory dnfrepo.InventoryRecord,
	equipment dnfrepo.EquipmentRecord,
	equipmentFound bool,
	request currentGuardianGemUseRequest,
) (currentGuardianGemTargetLocation, error) {
	_ = inventory
	candidates := make([]currentGuardianGemTargetLocation, 0, 1)
	if equipmentFound {
		for key, item := range equipment.Entries {
			if item.ItemID != int64(request.TargetMedalItemID) || item.ItemID <= 0 {
				continue
			}
			actorSlot, ok := currentEXEActorEquipmentSlot(item)
			if !ok || actorSlot != currentGuildMedalActorSlot {
				continue
			}
			candidates = append(candidates, currentGuardianGemTargetLocation{
				Container: currentGuardianGemTargetEquipped,
				ListType:  currentSocketListEquipment,
				Slot:      item.SlotIndex,
				Key:       key,
			})
		}
	}
	switch len(candidates) {
	case 0:
		return currentGuardianGemTargetLocation{}, fmt.Errorf("%w: actor_slot=%d item=%d", errCurrentGuardianGemTargetMissing, currentGuildMedalActorSlot, request.TargetMedalItemID)
	case 1:
		return candidates[0], nil
	default:
		return currentGuardianGemTargetLocation{}, fmt.Errorf("%w: actor_slot=%d item=%d matches=%d", errCurrentGuardianGemTargetAmbiguous, currentGuildMedalActorSlot, request.TargetMedalItemID, len(candidates))
	}
}

func currentGuardianGemWriteTarget(
	inventory *dnfrepo.InventoryRecord,
	equipment *dnfrepo.EquipmentRecord,
	target currentGuardianGemTargetLocation,
	request currentGuardianGemUseRequest,
) error {
	if inventory == nil || equipment == nil {
		return errCurrentGuardianGemInventoryMissing
	}
	switch target.Container {
	case currentGuardianGemTargetInventory:
		item, ok := inventory.Slots[target.Key]
		if !ok {
			return errCurrentGuardianGemTargetMissing
		}
		if err := currentGuardianGemWriteRawSocket(item.RawEntry, request.SocketIndex, request.GuardianGemItemID); err != nil {
			return err
		}
		currentGuardianGemSyncRawSocketExtra(&item.Extra, item.RawEntry)
		currentRefreshStackRawEntry(&item, target.ListType, target.Slot)
		inventory.Slots[target.Key] = item
	case currentGuardianGemTargetWarehouse:
		item, ok := inventory.Warehouse[target.Key]
		if !ok {
			return errCurrentGuardianGemTargetMissing
		}
		if err := currentGuardianGemWriteRawSocket(item.RawEntry, request.SocketIndex, request.GuardianGemItemID); err != nil {
			return err
		}
		currentGuardianGemSyncRawSocketExtra(&item.Extra, item.RawEntry)
		currentRefreshStackRawEntry(&item, target.ListType, target.Slot)
		inventory.Warehouse[target.Key] = item
	case currentGuardianGemTargetEquipped:
		item, ok := equipment.Entries[target.Key]
		if !ok {
			return errCurrentGuardianGemTargetMissing
		}
		if err := currentGuardianGemWriteRawSocket(item.RawEntry, request.SocketIndex, request.GuardianGemItemID); err != nil {
			return err
		}
		currentGuardianGemSyncRawSocketExtra(&item.Extra, item.RawEntry)
		if row, ok := currentItemListEntryFromEquipment(item); ok {
			item.RawEntry = append([]byte(nil), row.data[:]...)
		}
		equipment.Entries[target.Key] = item
	default:
		return errCurrentGuardianGemTargetMissing
	}
	return nil
}

func currentGuardianGemWriteRawSocket(raw []byte, socketIndex byte, gemItemID uint32) error {
	if len(raw) != currentItemListEntryWireSize {
		return fmt.Errorf("%w: raw_len=%d", errCurrentGuardianGemTargetRawMissing, len(raw))
	}
	value, err := currentGuardianGemSocketValue(gemItemID)
	if err != nil {
		return err
	}
	offset := currentGuardianGemRawSocketOffset + int(socketIndex)*currentGuardianGemRawSocketWidth
	if offset < 0 || offset+currentGuardianGemRawSocketWidth > len(raw) {
		return fmt.Errorf("%w: socket=%d offset=%d raw_len=%d", errCurrentGuardianGemTargetRawMissing, socketIndex, offset, len(raw))
	}
	// The current client confirms that an occupied guardian-gem slot cannot
	// return its old gem. Reusing the same op829 request therefore replaces the
	// stored word; the caller atomically consumes only the newly selected gem.
	binary.LittleEndian.PutUint16(raw[offset:offset+currentGuardianGemRawSocketWidth], value)
	return nil
}

// currentGuardianGemTargetRawEntry re-reads the target medal raw row using the
// same container rules as currentGuardianGemWriteTarget, so the duplicate
// check inspects exactly the bytes the write would modify.
func currentGuardianGemTargetRawEntry(
	inventory *dnfrepo.InventoryRecord,
	equipment *dnfrepo.EquipmentRecord,
	target currentGuardianGemTargetLocation,
) ([]byte, error) {
	if inventory == nil || equipment == nil {
		return nil, errCurrentGuardianGemInventoryMissing
	}
	switch target.Container {
	case currentGuardianGemTargetInventory:
		item, ok := inventory.Slots[target.Key]
		if !ok {
			return nil, errCurrentGuardianGemTargetMissing
		}
		return item.RawEntry, nil
	case currentGuardianGemTargetWarehouse:
		item, ok := inventory.Warehouse[target.Key]
		if !ok {
			return nil, errCurrentGuardianGemTargetMissing
		}
		return item.RawEntry, nil
	case currentGuardianGemTargetEquipped:
		item, ok := equipment.Entries[target.Key]
		if !ok {
			return nil, errCurrentGuardianGemTargetMissing
		}
		return item.RawEntry, nil
	default:
		return nil, errCurrentGuardianGemTargetMissing
	}
}

// currentGuardianGemRejectDuplicateAttribute enforces the guardian-gem rule
// that one medal cannot carry two gems of the same PVF enchant family. The
// client refuses that in its own socket table, but the C2S request carries
// nothing but a raw socket index, so the rule has to be enforced here before
// the socket word is written.
//
// A stored socket word that no longer resolves to a [flag gem] is skipped
// rather than treated as a conflict: the server cannot name its family, and
// refusing would strand a medal whose word predates this rule.
func currentGuardianGemRejectDuplicateAttribute(
	inventory *dnfrepo.InventoryRecord,
	equipment *dnfrepo.EquipmentRecord,
	target currentGuardianGemTargetLocation,
	request currentGuardianGemUseRequest,
	catalog *pvfDungeonDropCatalog,
	gem currentGuardianGemDefinition,
) error {
	if gem.EnchantFamily == "" {
		return nil
	}
	raw, err := currentGuardianGemTargetRawEntry(inventory, equipment, target)
	if err != nil {
		return err
	}
	if len(raw) != currentItemListEntryWireSize {
		return fmt.Errorf("%w: raw_len=%d", errCurrentGuardianGemTargetRawMissing, len(raw))
	}
	for index := 0; index < currentGuardianGemSocketCount; index++ {
		if byte(index) == request.SocketIndex {
			continue
		}
		offset := currentGuardianGemRawSocketOffset + index*currentGuardianGemRawSocketWidth
		if offset+currentGuardianGemRawSocketWidth > len(raw) {
			continue
		}
		value := binary.LittleEndian.Uint16(raw[offset : offset+currentGuardianGemRawSocketWidth])
		if value == 0 {
			continue
		}
		occupiedItemID := currentGuardianGemItemIDBase + uint32(value)
		occupied, resolveErr := resolveCurrentGuardianGem(catalog, occupiedItemID)
		if resolveErr != nil {
			continue
		}
		if occupied.EnchantFamily == gem.EnchantFamily {
			return fmt.Errorf(
				"%w: socket=%d family=%q occupied_socket=%d occupied_item=%d",
				errCurrentGuardianGemAttributeDuplicate,
				request.SocketIndex,
				gem.EnchantFamily,
				index,
				occupiedItemID,
			)
		}
	}
	return nil
}

func currentGuardianGemSyncRawSocketExtra(extra *map[string]string, raw []byte) {
	const rawSocketStateBytes = 9 // four u16 values plus the adjacent preserved byte.
	if len(raw) < currentGuardianGemRawSocketOffset+rawSocketStateBytes {
		return
	}
	currentEnsureExtra(extra)
	currentSetHexExtra(*extra, "raw_data_65", raw[currentGuardianGemRawSocketOffset:currentGuardianGemRawSocketOffset+rawSocketStateBytes])
}

func currentGuardianGemFindMedalBagSource(items map[string]dnfrepo.ItemStack, sourceSlot uint16, gemItemID uint32) (string, int16, error) {
	if sourceSlot > 32767 || !currentGuardianGemPageContains(int16(sourceSlot)) {
		return "", 0, fmt.Errorf("%w: slot=%d", errCurrentGuardianGemSourceSlotRange, sourceSlot)
	}
	slot := int16(sourceSlot)
	key := currentSocketInventoryKey(currentGuardianGemInventoryListType, slot)
	item, found := items[key]
	if !found || item.ItemID != int64(gemItemID) || item.Count <= 0 {
		return "", 0, fmt.Errorf("%w: key=%s item=%d", errCurrentGuardianGemSourceMissing, key, gemItemID)
	}
	return key, slot, nil
}

func (s *Service) currentGuardianGemUseSnapshot(
	session *gameSession,
	request currentGuardianGemUseRequest,
) (currentGuardianGemUseSnapshot, error) {
	if session == nil || session.selectedCharacterID == 0 {
		return currentGuardianGemUseSnapshot{}, errCurrentGuardianGemCharacterMissing
	}
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.Inventory == nil || repositories.Equipment == nil {
		return currentGuardianGemUseSnapshot{}, errCurrentGuardianGemRepositoryMissing
	}
	characterID := strconv.Itoa(int(session.selectedCharacterID))
	inventory, inventoryFound, err := repositories.Inventory.Load(context.Background(), characterID)
	if err != nil {
		return currentGuardianGemUseSnapshot{}, fmt.Errorf("load guardian gem inventory: %w", err)
	}
	equipment, equipmentFound, err := repositories.Equipment.Load(context.Background(), characterID)
	if err != nil {
		return currentGuardianGemUseSnapshot{}, fmt.Errorf("load guardian gem equipment: %w", err)
	}
	var snapshot currentGuardianGemUseSnapshot
	if inventoryFound {
		currentGuardianGemInspectInventoryMap(&snapshot, inventory.Slots, request, false)
		currentGuardianGemInspectInventoryMap(&snapshot, inventory.Warehouse, request, true)
	}
	if equipmentFound {
		for _, item := range equipment.Entries {
			actorSlot, ok := currentEXEActorEquipmentSlot(item)
			if ok && actorSlot == currentGuildMedalActorSlot && item.ItemID == int64(request.TargetMedalItemID) {
				snapshot.TargetEquippedMatches++
			}
		}
	}
	return snapshot, nil
}

func currentGuardianGemInspectInventoryMap(
	snapshot *currentGuardianGemUseSnapshot,
	items map[string]dnfrepo.ItemStack,
	request currentGuardianGemUseRequest,
	warehouse bool,
) {
	if snapshot == nil {
		return
	}
	for key, item := range items {
		listType, slot, ok := parseSceneInventorySlotKey(key)
		if !ok {
			continue
		}
		if !warehouse && listType == currentGuardianGemInventoryListType &&
			slot == int16(request.GuardianGemSourceSlot) &&
			item.ItemID == int64(request.GuardianGemItemID) && item.Count > 0 {
			snapshot.GemMainStackCount += item.Count
		}
	}
}
