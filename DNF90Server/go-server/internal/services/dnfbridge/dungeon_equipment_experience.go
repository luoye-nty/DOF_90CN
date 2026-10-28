package dnfbridge

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"
)

const (
	currentDungeonQuickSlotFirst = 3
	currentDungeonQuickSlotLast  = 8
)

func (s *Service) currentDungeonEquipmentExpAdvantagePercent(
	ctx context.Context,
	characterID string,
) (int64, error) {
	characterID = strings.TrimSpace(characterID)
	if s == nil || characterID == "" {
		return 0, nil
	}
	repositories, ok := s.repositoryGroup()
	if !ok {
		return 0, nil
	}
	now := time.Now().UTC()
	var total int64
	addItem := func(itemID int64, expireAt time.Time, extra map[string]string) bool {
		if itemID <= 0 || (!expireAt.IsZero() && !expireAt.After(now)) {
			return false
		}
		percent, resolved := s.equipmentPVFExpAdvantage(ctx, itemID, extra)
		if !resolved || percent <= 0 {
			return false
		}
		if total > math.MaxInt64-percent {
			total = math.MaxInt64
			return true
		}
		total += percent
		return total == math.MaxInt64
	}

	if repositories.Equipment != nil {
		equipment, found, err := repositories.Equipment.Load(ctx, characterID)
		if err != nil {
			return 0, err
		}
		if found {
			for _, entry := range equipment.Entries {
				if addItem(entry.ItemID, entry.ExpireAt, entry.Extra) {
					return total, nil
				}
			}
		}
	}

	if repositories.Inventory != nil {
		inventory, found, err := repositories.Inventory.Load(ctx, characterID)
		if err != nil {
			return 0, err
		}
		if found {
			for slot := currentDungeonQuickSlotFirst; slot <= currentDungeonQuickSlotLast; slot++ {
				stack := inventory.Slots[fmt.Sprintf("0:%d", slot)]
				// Quick-slot equipment contributes once per occupied slot. Count is
				// inventory quantity, not an experience multiplier.
				if addItem(stack.ItemID, stack.ExpireAt, stack.Extra) {
					return total, nil
				}
			}
		}
	}
	return total, nil
}
