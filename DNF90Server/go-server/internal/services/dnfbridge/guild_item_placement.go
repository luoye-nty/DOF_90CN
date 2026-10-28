package dnfbridge

import (
	"context"
	"encoding/binary"
	"math"
	"sort"
	"strconv"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

// Move old list-0 rows intact; occupied list-38 cells and unknown PVF items stay untouched.
func relocateCurrentGuildItems(inventory *dnfrepo.InventoryRecord, catalog *pvfDungeonDropCatalog) int {
	keys := make([]string, 0)
	for key, stack := range inventory.Slots {
		list, slot, valid := parseSceneInventorySlotKey(key)
		if valid && list == dnfrepo.MainInventoryListType && slot >= 9 && slot <= 120 && stack.ItemID > 0 && stack.ItemID <= math.MaxUint32 && stack.Count > 0 {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	moved := 0
	for _, key := range keys {
		stack := inventory.Slots[key]
		definition, err := catalog.ResolveItem(uint32(stack.ItemID))
		if err != nil || definition.InventoryListType != currentGuildMedalInventoryListType {
			continue
		}
		for slot := definition.SlotStart; slot <= definition.SlotEnd; slot++ {
			target := currentCeraShopInventorySlotKey(definition.InventoryListType, slot)
			if _, occupied := inventory.Slots[target]; occupied {
				continue
			}
			if len(stack.RawEntry) == currentItemListEntryWireSize {
				stack.RawEntry = append([]byte(nil), stack.RawEntry...)
				binary.LittleEndian.PutUint16(stack.RawEntry, uint16(slot))
			} else {
				entry := currentItemListEntryFromStack(definition.InventoryListType, slot, stack)
				stack.RawEntry = append([]byte(nil), entry.data[:]...)
			}
			inventory.Slots[target] = stack
			delete(inventory.Slots, key)
			moved++
			break
		}
	}
	return moved
}

func (s *Service) migrateCurrentGuildItemPlacement(ctx context.Context, session *gameSession) error {
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.CharacterItems == nil {
		return nil
	}
	catalog, err := s.currentPVFItemCatalog()
	if err != nil {
		return err
	}
	characterID := strconv.Itoa(int(session.selectedCharacterID))
	moved := 0
	err = dnfrepo.WithinCharacterItemsScoped(ctx, repositories.CharacterItems, characterID, func(inventories dnfrepo.InventoryRepository, _ dnfrepo.EquipmentRepository) error {
		inventory, found, err := inventories.Load(ctx, characterID)
		if err != nil || !found {
			return err
		}
		inventory = dnfrepo.CloneInventory(inventory)
		moved = relocateCurrentGuildItems(&inventory, catalog)
		if moved == 0 {
			return nil
		}
		inventory.UpdatedAt = s.gameplayNow()
		return dnfrepo.SaveInventoryFields(ctx, inventories, inventory, dnfrepo.InventoryFieldSlots)
	})
	if err == nil && moved != 0 {
		s.logGameEvent(session, "game-guild-item-placement-migrated", "moved_stacks", moved)
	}
	return err
}
