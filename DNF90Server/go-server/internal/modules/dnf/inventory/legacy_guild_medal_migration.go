package inventory

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

var ErrLegacyGuildMedalMigrationUnavailable = errors.New("legacy guild medal inventory migration is unavailable")

// LegacyGuildMedalInventoryCandidate is a bridge-classified list-0 row that
// belongs on a list-38 page: a [flag] guild medal (medal page 0..48) or a
// [flag gem] guardian gem (gem page 49..97). The owner revalidates the durable
// item id and target page inside one character transaction before moving it.
type LegacyGuildMedalInventoryCandidate struct {
	SourceKey  string
	ItemID     int64
	TargetFrom int16
	TargetTo   int16
}

type LegacyGuildMedalInventoryMigrationResult struct {
	Migrated int
}

// LegacyGuildMedalMigrationOwner owns the one-time repair of guild medals and
// guardian gems that older NPC-shop builds granted into the main inventory.
type LegacyGuildMedalMigrationOwner struct {
	assets dnfrepo.CharacterAssetUnitOfWork
}

func NewLegacyGuildMedalMigrationOwner(assets dnfrepo.CharacterAssetUnitOfWork) (*LegacyGuildMedalMigrationOwner, error) {
	if assets == nil {
		return nil, ErrLegacyGuildMedalMigrationUnavailable
	}
	return &LegacyGuildMedalMigrationOwner{assets: assets}, nil
}

func (o *LegacyGuildMedalMigrationOwner) Migrate(
	ctx context.Context,
	characterID string,
	candidates []LegacyGuildMedalInventoryCandidate,
) (LegacyGuildMedalInventoryMigrationResult, error) {
	if o == nil || o.assets == nil {
		return LegacyGuildMedalInventoryMigrationResult{}, ErrLegacyGuildMedalMigrationUnavailable
	}
	characterID = strings.TrimSpace(characterID)
	if characterID == "" || len(candidates) == 0 {
		return LegacyGuildMedalInventoryMigrationResult{}, nil
	}
	for _, candidate := range candidates {
		if !strings.HasPrefix(candidate.SourceKey, "0:") || candidate.ItemID <= 0 ||
			!validLegacyGuildMedalTargetRange(candidate.TargetFrom, candidate.TargetTo) {
			return LegacyGuildMedalInventoryMigrationResult{}, fmt.Errorf("%w: invalid candidate %+v", ErrLegacyGuildMedalMigrationUnavailable, candidate)
		}
	}

	result := LegacyGuildMedalInventoryMigrationResult{}
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
			targetSlot, found := firstFreeLegacyGuildMedalSlot(inventory.Slots, candidate.TargetFrom, candidate.TargetTo)
			if !found {
				return fmt.Errorf("%w: guild medal list slots %d..%d full", ErrLegacyGuildMedalMigrationUnavailable, candidate.TargetFrom, candidate.TargetTo)
			}
			if len(stack.RawEntry) >= 2 {
				stack.RawEntry = append([]byte(nil), stack.RawEntry...)
				binary.LittleEndian.PutUint16(stack.RawEntry[:2], uint16(targetSlot))
			}
			inventory.Slots[slotKey(listTypeGuildMedal, targetSlot)] = stack
			delete(inventory.Slots, candidate.SourceKey)
			result.Migrated++
		}
		if result.Migrated == 0 {
			return nil
		}
		return dnfrepo.SaveInventoryFields(ctx, inventoryRepo, inventory, dnfrepo.InventoryFieldSlots)
	})
	if err != nil {
		return LegacyGuildMedalInventoryMigrationResult{}, err
	}
	return result, nil
}

func validLegacyGuildMedalTargetRange(from, to int16) bool {
	return (from == 0 && to == 48) || (from == 49 && to == 97)
}

func firstFreeLegacyGuildMedalSlot(slots map[string]dnfrepo.ItemStack, from, to int16) (int16, bool) {
	for slot := from; slot <= to; slot++ {
		stack, occupied := slots[slotKey(listTypeGuildMedal, slot)]
		if !occupied || stack.ItemID <= 0 || stack.Count <= 0 {
			return slot, true
		}
	}
	return 0, false
}
