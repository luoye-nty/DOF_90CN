package dnfbridge

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	dnfequip "longheng.io/server/internal/modules/dnf/equip"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

const (
	gmEquipmentListMain       byte = 0
	gmEquipmentListAvatar     byte = 1
	gmEquipmentListWorn       byte = 3
	gmEquipmentListPet        byte = 7
	gmEquipmentListGuildMedal byte = 38
)

// GMUnequipEquipment atomically moves one offline character's worn item back
// to the correct inventory page through the same equipment owner as gameplay.
func (s *Service) GMUnequipEquipment(ctx context.Context, characterID, key string) (GMInventorySnapshot, error) {
	characterID, numericID, repositories, slot, err := s.gmEquipmentMutationTarget(ctx, characterID, key)
	if err != nil {
		return GMInventorySnapshot{}, err
	}
	destinationList, destinationSlot, err := gmEquipmentUnequipDestination(slot)
	if err != nil {
		return GMInventorySnapshot{}, err
	}
	validator := dnfequip.PlacementValidatorFunc(func(context.Context, dnfequip.Placement) error {
		return fmt.Errorf("%w: unequip unexpectedly attempted an equipment placement", ErrGMInvalidRequest)
	})
	owner, err := dnfequip.NewOwnerWithPlacementValidator(repositories, validator)
	if err != nil {
		return GMInventorySnapshot{}, err
	}
	if _, err = owner.Move(ctx, dnfequip.MoveCommand{
		SelectedCharacterID:  numericID,
		SourceListType:       gmEquipmentListWorn,
		SourceSlotIndex:      slot,
		MoveCount:            1,
		DestinationListType:  destinationList,
		DestinationSlotIndex: destinationSlot,
	}); err != nil {
		return GMInventorySnapshot{}, err
	}
	return s.GMInventory(ctx, characterID)
}

// GMDeleteEquipment permanently destroys one ordinary worn item. Creature and
// artifact slots must first be unequipped so their pet aggregate stays valid.
func (s *Service) GMDeleteEquipment(ctx context.Context, characterID, key string) (GMInventorySnapshot, error) {
	characterID, _, repositories, slot, err := s.gmEquipmentMutationTarget(ctx, characterID, key)
	if err != nil {
		return GMInventorySnapshot{}, err
	}
	if slot >= 26 && slot <= 29 {
		return GMInventorySnapshot{}, fmt.Errorf("%w: creature equipment must be unequipped before deletion", ErrGMInvalidRequest)
	}
	if repositories.CharacterItems == nil {
		return GMInventorySnapshot{}, ErrGMUnavailable
	}
	entryKey := strconv.FormatInt(int64(slot), 10)
	err = repositories.CharacterItems.WithinCharacterItems(ctx, characterID, func(_ dnfrepo.InventoryRepository, equipment dnfrepo.EquipmentRepository) error {
		record, found, loadErr := equipment.Load(ctx, characterID)
		if loadErr != nil {
			return loadErr
		}
		if !found {
			return fmt.Errorf("%w: equipment record is missing", ErrGMInvalidRequest)
		}
		record = dnfrepo.CloneEquipment(record)
		entry, found := record.Entries[entryKey]
		if !found {
			return fmt.Errorf("%w: equipped slot %d is empty", ErrGMInvalidRequest, slot)
		}
		if entry.SlotIndex != slot {
			return fmt.Errorf("%w: equipment key %s contains slot %d", ErrGMInvalidRequest, entryKey, entry.SlotIndex)
		}
		delete(record.Entries, entryKey)
		record.UpdatedAt = time.Now().UTC()
		return dnfrepo.SaveEquipmentFields(ctx, equipment, record, dnfrepo.EquipmentFieldEntries)
	})
	if err != nil {
		return GMInventorySnapshot{}, err
	}
	return s.GMInventory(ctx, characterID)
}

func (s *Service) gmEquipmentMutationTarget(
	ctx context.Context,
	characterID string,
	key string,
) (string, uint16, dnfrepo.Group, int16, error) {
	characterID, numericID, err := gmCharacterID(characterID)
	if err != nil {
		return "", 0, dnfrepo.Group{}, 0, err
	}
	if _, online := s.onlineGameSession(numericID); online {
		return "", 0, dnfrepo.Group{}, 0, ErrGMCharacterOnline
	}
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.Character == nil || repositories.Inventory == nil || repositories.Equipment == nil {
		return "", 0, dnfrepo.Group{}, 0, ErrGMUnavailable
	}
	if _, found, loadErr := repositories.Character.Load(ctx, characterID); loadErr != nil {
		return "", 0, dnfrepo.Group{}, 0, loadErr
	} else if !found {
		return "", 0, dnfrepo.Group{}, 0, ErrGMCharacterNotFound
	}
	slot, err := gmEquipmentSlot(key)
	if err != nil {
		return "", 0, dnfrepo.Group{}, 0, err
	}
	record, found, err := repositories.Equipment.Load(ctx, characterID)
	if err != nil {
		return "", 0, dnfrepo.Group{}, 0, err
	}
	entryKey := strconv.FormatInt(int64(slot), 10)
	entry, exists := record.Entries[entryKey]
	if !found || !exists {
		return "", 0, dnfrepo.Group{}, 0, fmt.Errorf("%w: equipped slot %d is empty", ErrGMInvalidRequest, slot)
	}
	if entry.SlotIndex != slot {
		return "", 0, dnfrepo.Group{}, 0, fmt.Errorf("%w: equipment key %s contains slot %d", ErrGMInvalidRequest, entryKey, entry.SlotIndex)
	}
	return characterID, numericID, repositories, slot, nil
}

func gmEquipmentSlot(key string) (int16, error) {
	key = strings.TrimSpace(key)
	if strings.HasPrefix(key, "3:") {
		key = strings.TrimPrefix(key, "3:")
	}
	value, err := strconv.ParseInt(key, 10, 16)
	if err != nil || value < 0 || value > 32 {
		return 0, fmt.Errorf("%w: invalid equipment key %q", ErrGMInvalidRequest, key)
	}
	return int16(value), nil
}

func gmEquipmentUnequipDestination(slot int16) (byte, int16, error) {
	switch {
	case slot >= 0 && slot <= 11:
		return gmEquipmentListAvatar, 0, nil
	case slot >= 12 && slot <= 25:
		return gmEquipmentListMain, 9, nil
	case slot == 26:
		return gmEquipmentListPet, 0, nil
	case slot >= 27 && slot <= 29:
		return gmEquipmentListPet, 140, nil
	case slot == 30:
		return gmEquipmentListMain, 9, nil
	case slot == 32:
		return gmEquipmentListGuildMedal, 0, nil
	default:
		return 0, 0, fmt.Errorf("%w: equipped slot %d has no proven inventory destination", ErrGMInvalidRequest, slot)
	}
}
