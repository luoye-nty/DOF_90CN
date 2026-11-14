package dnfbridge

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"longheng.io/server/internal/modules/dnf/premium"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrobotdomain "longheng.io/server/internal/modules/dnf/robot"
	"longheng.io/server/internal/services/dnfrobot"
)

type configuredPrivateStoreCandidate struct {
	ListType  uint8
	Slot      int16
	Stack     dnfrepo.ItemStack
	WireValue uint32
}

func (s *Service) PrepareRobotPrivateStoreRows(
	ctx context.Context,
	accountID string,
	characterID uint16,
	items []dnfrobotdomain.PrivateStoreItem,
) ([]dnfrobot.PrivateStoreRow, error) {
	if s == nil || characterID == 0 || len(items) == 0 || len(items) > currentPrivateStoreMaxRows {
		return nil, errors.New("configured robot private store items are invalid")
	}
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.Inventory == nil {
		return nil, errors.New("dnf robot inventory repository is unavailable")
	}
	catalog, err := s.currentPVFItemCatalog()
	if err != nil {
		return nil, err
	}
	definitions := make(map[uint32]dungeonDropItemDefinition, len(items))
	seen := make(map[uint32]struct{}, len(items))
	for _, item := range items {
		if item.ItemID == 0 || item.Count == 0 || item.Price == 0 {
			return nil, fmt.Errorf("invalid configured private store item %d", item.ItemID)
		}
		if _, duplicate := seen[item.ItemID]; duplicate {
			return nil, fmt.Errorf("duplicate configured private store item %d", item.ItemID)
		}
		seen[item.ItemID] = struct{}{}
		definition, resolveErr := catalog.ResolveItem(item.ItemID)
		if resolveErr != nil {
			return nil, fmt.Errorf("resolve configured private store item %d: %w", item.ItemID, resolveErr)
		}
		if definition.Kind == dungeonDropItemEquipment {
			if item.Count != 1 {
				return nil, fmt.Errorf("equipment private store item %d requires count 1", item.ItemID)
			}
		} else if definition.StackLimit > 0 && int64(item.Count) > definition.StackLimit {
			return nil, fmt.Errorf("item %d count %d exceeds stack limit %d", item.ItemID, item.Count, definition.StackLimit)
		}
		definitions[item.ItemID] = definition
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := s.ensureRobotPrivateStoreLicense(ctx, repositories, accountID, characterID); err != nil {
		return nil, err
	}

	inventory, err := loadRobotConfiguredPrivateStoreInventory(ctx, repositories, characterID)
	if err != nil {
		return nil, err
	}
	for attempt := 0; attempt < 2; attempt++ {
		missing := make([]GMRobotStockItem, 0, len(items))
		for _, item := range items {
			best, found := bestRobotConfiguredPrivateStoreCandidate(inventory, catalog, definitions[item.ItemID], item.ItemID)
			target := int64(item.Count)
			if definitions[item.ItemID].Kind == dungeonDropItemEquipment {
				target = 1
			}
			current := int64(0)
			if found {
				current = best.Stack.Count
			}
			if current < target {
				missing = append(missing, GMRobotStockItem{ItemID: item.ItemID, Count: uint32(target - current)})
			}
		}
		if len(missing) == 0 {
			break
		}
		if _, err := s.GMStockRobotCharacter(ctx, GMRobotStockRequest{
			AccountID: accountID, CharacterID: characterID, Items: missing,
		}); err != nil {
			return nil, fmt.Errorf("stock configured robot private store: %w", err)
		}
		inventory, err = loadRobotConfiguredPrivateStoreInventory(ctx, repositories, characterID)
		if err != nil {
			return nil, err
		}
	}

	rows := make([]dnfrobot.PrivateStoreRow, 0, len(items))
	for index, item := range items {
		candidate, found := bestRobotConfiguredPrivateStoreCandidate(inventory, catalog, definitions[item.ItemID], item.ItemID)
		if !found || candidate.Stack.Count < int64(item.Count) {
			return nil, fmt.Errorf("configured item %d has insufficient private-store stock", item.ItemID)
		}
		wireValue := item.Count
		if definitions[item.ItemID].Kind == dungeonDropItemEquipment {
			wireValue = candidate.WireValue
		}
		if count, instance, _, _, valid := s.resolveCurrentPrivateStoreRowStack(candidate.ListType, candidate.Stack, wireValue); !valid || count == 0 || instance != 0 && instance != wireValue {
			return nil, fmt.Errorf("configured item %d cannot be registered in a private store", item.ItemID)
		}
		rows = append(rows, dnfrobot.PrivateStoreRow{
			DisplayIndex: uint16(index),
			Price:        item.Price,
			ListType:     candidate.ListType,
			Slot:         candidate.Slot,
			WireValue:    wireValue,
		})
	}
	return rows, nil
}

func loadRobotConfiguredPrivateStoreInventory(ctx context.Context, repositories dnfrepo.Group, characterID uint16) (dnfrepo.InventoryRecord, error) {
	inventory, found, err := repositories.Inventory.Load(ctx, strconv.Itoa(int(characterID)))
	if err != nil {
		return dnfrepo.InventoryRecord{}, err
	}
	if !found {
		return dnfrepo.InventoryRecord{}, errors.New("dnf robot inventory was not found")
	}
	return inventory, nil
}

func bestRobotConfiguredPrivateStoreCandidate(
	inventory dnfrepo.InventoryRecord,
	catalog *pvfDungeonDropCatalog,
	definition dungeonDropItemDefinition,
	itemID uint32,
) (configuredPrivateStoreCandidate, bool) {
	candidates := make([]configuredPrivateStoreCandidate, 0, 2)
	for key, stack := range inventory.Slots {
		if stack.ItemID != int64(itemID) || stack.Count <= 0 || currentDungeonDiscardStackLocked(stack) || currentDungeonDiscardStackExpired(stack, time.Now()) {
			continue
		}
		parts := strings.Split(key, ":")
		if len(parts) != 2 {
			continue
		}
		listValue, listErr := strconv.ParseUint(parts[0], 10, 8)
		slotValue, slotErr := strconv.ParseInt(parts[1], 10, 16)
		if listErr != nil || slotErr != nil || (listValue != 0 && listValue != 1) || slotValue < 0 {
			continue
		}
		if _, _, eligible := currentPrivateStoreRegistrationPolicyForDefinition(definition, stack); !eligible {
			continue
		}
		wireValue := uint32(0)
		if definition.Kind == dungeonDropItemEquipment {
			wireValue = currentPrivateStoreEquipmentInstance(byte(listValue), stack)
		} else if stack.Count <= int64(^uint32(0)) {
			wireValue = uint32(stack.Count)
		}
		if wireValue == 0 {
			continue
		}
		candidates = append(candidates, configuredPrivateStoreCandidate{
			ListType: uint8(listValue), Slot: int16(slotValue), Stack: stack, WireValue: wireValue,
		})
	}
	if len(candidates) == 0 {
		return configuredPrivateStoreCandidate{}, false
	}
	sort.Slice(candidates, func(left, right int) bool {
		if candidates[left].Stack.Count != candidates[right].Stack.Count {
			return candidates[left].Stack.Count > candidates[right].Stack.Count
		}
		if candidates[left].ListType != candidates[right].ListType {
			return candidates[left].ListType < candidates[right].ListType
		}
		return candidates[left].Slot < candidates[right].Slot
	})
	return candidates[0], true
}

func (s *Service) ensureRobotPrivateStoreLicense(ctx context.Context, repositories dnfrepo.Group, accountID string, characterID uint16) error {
	if repositories.Account == nil {
		return ErrGMRepositoryUnavailable
	}
	now := time.Now().UTC()
	account, found, err := repositories.Account.Load(ctx, accountID)
	if err != nil {
		return err
	}
	if !found || strings.TrimSpace(account.AccountID) != strings.TrimSpace(accountID) {
		return ErrGMRequestInvalid
	}
	if premium.Active(account, premium.TypeAuction, now) && premium.Active(account, premium.TypePrivateStore, now) {
		return nil
	}
	account = dnfrepo.CloneAccount(account)
	premium.Upsert(&account, premium.TypeAuction, automaticRobotStoreContractSeconds, 1, now)
	if repositories.CharacterSettlement == nil {
		for _, premiumType := range premium.ActivationTypes(premium.TypeAuction) {
			key := premium.MetadataKey(premiumType)
			if err := dnfrepo.SaveAccountMetadataEntry(ctx, repositories.Account, account, key, account.Metadata[key], now); err != nil {
				return err
			}
		}
		return nil
	}
	return repositories.CharacterSettlement.WithinCharacterSettlement(ctx, strconv.Itoa(int(characterID)), func(tx dnfrepo.Group) error {
		current, found, loadErr := tx.Account.Load(ctx, accountID)
		if loadErr != nil {
			return loadErr
		}
		if !found {
			return ErrGMRepositoryUnavailable
		}
		current = dnfrepo.CloneAccount(current)
		premium.Upsert(&current, premium.TypeAuction, automaticRobotStoreContractSeconds, 1, now)
		for _, premiumType := range premium.ActivationTypes(premium.TypeAuction) {
			key := premium.MetadataKey(premiumType)
			if saveErr := dnfrepo.SaveAccountMetadataEntry(ctx, tx.Account, current, key, current.Metadata[key], now); saveErr != nil {
				return saveErr
			}
		}
		return nil
	})
}
