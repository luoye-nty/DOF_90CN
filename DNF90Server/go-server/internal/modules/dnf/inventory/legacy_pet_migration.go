package inventory

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

var ErrLegacyPetMigrationUnavailable = errors.New("legacy pet inventory migration is unavailable")

// LegacyPetInventoryCandidate is a bridge-classified list-0 row. The owner
// revalidates the durable item id and target segment inside one character
// transaction before moving it.
type LegacyPetInventoryCandidate struct {
	SourceKey  string
	ItemID     int64
	TargetFrom int16
	TargetTo   int16
}

type LegacyPetInventoryMigrationResult struct {
	Migrated int
}

// LegacyPetMigrationOwner owns the one-time repair of pet artifacts and pet
// consumables that older builds placed in the main inventory.
type LegacyPetMigrationOwner struct {
	assets dnfrepo.CharacterAssetUnitOfWork
}

func NewLegacyPetMigrationOwner(assets dnfrepo.CharacterAssetUnitOfWork) (*LegacyPetMigrationOwner, error) {
	if assets == nil {
		return nil, ErrLegacyPetMigrationUnavailable
	}
	return &LegacyPetMigrationOwner{assets: assets}, nil
}

func (o *LegacyPetMigrationOwner) Migrate(
	ctx context.Context,
	characterID string,
	candidates []LegacyPetInventoryCandidate,
) (LegacyPetInventoryMigrationResult, error) {
	if o == nil || o.assets == nil {
		return LegacyPetInventoryMigrationResult{}, ErrLegacyPetMigrationUnavailable
	}
	characterID = strings.TrimSpace(characterID)
	if characterID == "" || len(candidates) == 0 {
		return LegacyPetInventoryMigrationResult{}, nil
	}
	for _, candidate := range candidates {
		if !strings.HasPrefix(candidate.SourceKey, "0:") || candidate.ItemID <= 0 ||
			!validLegacyPetTargetRange(candidate.TargetFrom, candidate.TargetTo) {
			return LegacyPetInventoryMigrationResult{}, fmt.Errorf("%w: invalid candidate %+v", ErrLegacyPetMigrationUnavailable, candidate)
		}
	}

	result := LegacyPetInventoryMigrationResult{}
	err := dnfrepo.WithinCharacterAssetsScoped(ctx, o.assets, characterID, func(
		_ dnfrepo.CharacterRepository,
		inventoryRepo dnfrepo.InventoryRepository,
		_ dnfrepo.EquipmentRepository,
	) error {
		inventory, found, err := inventoryRepo.Load(ctx, characterID)
		if err != nil || !found {
			return err
		}
		inventory = dnfrepo.CloneInventory(inventory)
		if inventory.Slots == nil {
			return nil
		}
		for _, candidate := range candidates {
			stack, found := inventory.Slots[candidate.SourceKey]
			if !found || stack.ItemID != candidate.ItemID || stack.Count <= 0 {
				continue
			}
			targetSlot, found := firstFreeLegacyPetSlot(inventory.Slots, candidate.TargetFrom, candidate.TargetTo)
			if !found {
				return fmt.Errorf("%w: pet slots %d..%d full", ErrLegacyPetMigrationUnavailable, candidate.TargetFrom, candidate.TargetTo)
			}
			if len(stack.RawEntry) >= 2 {
				stack.RawEntry = append([]byte(nil), stack.RawEntry...)
				binary.LittleEndian.PutUint16(stack.RawEntry[:2], uint16(targetSlot))
			}
			inventory.Slots[slotKey(listTypePet, targetSlot)] = stack
			delete(inventory.Slots, candidate.SourceKey)
			result.Migrated++
		}
		if result.Migrated == 0 {
			return nil
		}
		return dnfrepo.SaveInventoryFields(ctx, inventoryRepo, inventory, dnfrepo.InventoryFieldSlots)
	})
	if err != nil {
		return LegacyPetInventoryMigrationResult{}, err
	}
	return result, nil
}

func validLegacyPetTargetRange(from, to int16) bool {
	return (from == 140 && to == 188) || (from == 189 && to == 238)
}

func firstFreeLegacyPetSlot(slots map[string]dnfrepo.ItemStack, from, to int16) (int16, bool) {
	for slot := from; slot <= to; slot++ {
		stack, occupied := slots[slotKey(listTypePet, slot)]
		if !occupied || stack.ItemID <= 0 || stack.Count <= 0 {
			return slot, true
		}
	}
	return 0, false
}
