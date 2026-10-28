package dnfbridge

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

const gmRobotMaximumStockItems = 14

type GMRobotStockItem struct {
	ItemID uint32 `json:"item_id"`
	Count  uint32 `json:"count"`
}

type GMRobotStockRequest struct {
	AccountID   string             `json:"account_id"`
	CharacterID uint16             `json:"character_id"`
	Items       []GMRobotStockItem `json:"items"`
}

type GMRobotStockResult struct {
	CharacterID uint16             `json:"character_id"`
	Items       []GMRobotStockItem `json:"items"`
	Slots       []int16            `json:"slots"`
}

func (s *Service) GMStockRobotCharacter(ctx context.Context, request GMRobotStockRequest) (GMRobotStockResult, error) {
	if s == nil {
		return GMRobotStockResult{}, ErrGMRepositoryUnavailable
	}
	request.AccountID = strings.TrimSpace(request.AccountID)
	if request.AccountID == "" || request.CharacterID == 0 || len(request.Items) == 0 || len(request.Items) > gmRobotMaximumStockItems {
		return GMRobotStockResult{}, fmt.Errorf("%w: invalid robot stock request", ErrGMRequestInvalid)
	}
	seen := make(map[uint32]struct{}, len(request.Items))
	catalog, err := s.currentPVFItemCatalog()
	if err != nil {
		return GMRobotStockResult{}, err
	}
	definitions := make([]dungeonDropItemDefinition, len(request.Items))
	for index, item := range request.Items {
		if item.ItemID == 0 || item.Count == 0 {
			return GMRobotStockResult{}, fmt.Errorf("%w: invalid robot stock row %d", ErrGMRequestInvalid, index)
		}
		if _, duplicated := seen[item.ItemID]; duplicated {
			return GMRobotStockResult{}, fmt.Errorf("%w: duplicate robot stock item %d", ErrGMRequestInvalid, item.ItemID)
		}
		seen[item.ItemID] = struct{}{}
		definition, resolveErr := catalog.ResolveItem(item.ItemID)
		if resolveErr != nil {
			return GMRobotStockResult{}, fmt.Errorf("%w: resolve stock item %d: %v", ErrGMRequestInvalid, item.ItemID, resolveErr)
		}
		if definition.Kind == dungeonDropItemEquipment && item.Count != 1 {
			return GMRobotStockResult{}, fmt.Errorf("%w: equipment stock item %d requires count 1", ErrGMRequestInvalid, item.ItemID)
		}
		definitions[index] = definition
	}
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.CharacterAssets == nil {
		return GMRobotStockResult{}, ErrGMRepositoryUnavailable
	}
	if ctx == nil {
		ctx = context.Background()
	}
	characterID := strconv.Itoa(int(request.CharacterID))
	result := GMRobotStockResult{
		CharacterID: request.CharacterID,
		Items:       append([]GMRobotStockItem(nil), request.Items...),
		Slots:       make([]int16, 0, len(request.Items)),
	}
	err = repositories.CharacterAssets.WithinCharacterAssets(ctx, characterID, func(
		characters dnfrepo.CharacterRepository,
		inventories dnfrepo.InventoryRepository,
		_ dnfrepo.EquipmentRepository,
	) error {
		character, found, loadErr := characters.Load(ctx, characterID)
		if loadErr != nil {
			return loadErr
		}
		if !found || strings.TrimSpace(character.AccountID) != request.AccountID {
			return fmt.Errorf("%w: robot character/account mismatch", ErrGMRequestInvalid)
		}
		inventory, found, loadErr := inventories.Load(ctx, characterID)
		if loadErr != nil {
			return loadErr
		}
		if !found {
			return fmt.Errorf("%w: robot inventory is unavailable", ErrGMRepositoryUnavailable)
		}
		for index, item := range request.Items {
			slot, placeErr := addCurrentDungeonPickupToInventory(&inventory, definitions[index], item.Count)
			if placeErr != nil {
				return fmt.Errorf("place robot stock item %d: %w", item.ItemID, placeErr)
			}
			key := fmt.Sprintf("%d:%d", dnfrepo.MainInventoryListType, slot)
			stack, found := inventory.Slots[key]
			if !found {
				return fmt.Errorf("%w: robot stock item %d placement disappeared", ErrGMRequestInvalid, item.ItemID)
			}
			_, _, eligible := currentPrivateStoreRegistrationPolicyForDefinition(definitions[index], stack)
			if !eligible {
				return fmt.Errorf("%w: item %d is not private-store eligible", ErrGMRequestInvalid, item.ItemID)
			}
			if stack.Extra == nil {
				stack.Extra = make(map[string]string)
			}
			stack.Extra["source"] = "robot_gm_stock"
			currentRefreshStackRawEntry(&stack, dnfrepo.MainInventoryListType, int16(slot))
			inventory.Slots[key] = stack
			result.Slots = append(result.Slots, int16(slot))
		}
		return dnfrepo.SaveInventoryFields(ctx, inventories, inventory, dnfrepo.InventoryFieldSlots)
	})
	if err != nil {
		return GMRobotStockResult{}, err
	}
	return result, nil
}
