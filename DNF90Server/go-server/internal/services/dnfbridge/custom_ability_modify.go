package dnfbridge

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"time"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	dnfenum "longheng.io/server/internal/modules/dnf/dnfenum"
	dnfproto "longheng.io/server/internal/modules/dnf/protocol"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

const (
	currentCustomAbilityModifyRequestSize        = 4
	currentCustomAbilityModifyErrorGeneric  byte = 1
	currentCustomAbilityModifyErrorMaterial byte = 8
)

var (
	errCurrentCustomAbilityModifyInvalid  = errors.New("current custom ability modify request is invalid")
	errCurrentCustomAbilityModifyMaterial = errors.New("current custom ability modify material is insufficient")
)

type currentCustomAbilityModifyRequest struct {
	targetSlot     int16
	optionPosition byte
	newOptionID    byte
}

type currentCustomAbilityModifyResult struct {
	itemID             uint32
	optionPosition     byte
	oldOptionID        byte
	newOptionID        byte
	grade              byte
	material           currentCustomAbilityMaterialCost
	materialRemaining  int64
	idempotent         bool
	groupConverter     bool
	converterItemID    int64
	converterRemaining int64
	refreshes          []alignedcmd.ItemSlotRefresh
}

func (catalog *currentCustomAbilityCatalog) customAbilityGroupForOption(optionID byte) ([]byte, bool) {
	if catalog == nil {
		return nil, false
	}
	for _, group := range catalog.groups {
		for _, candidate := range group {
			if candidate == optionID {
				return group, true
			}
		}
	}
	return nil, false
}

func commitCurrentCustomAbilityGroupConvert(
	ctx context.Context,
	repositories dnfrepo.Group,
	characterID string,
	request currentCustomAbilityModifyRequest,
	expectedItemID uint32,
	source alignedcmd.CustomAbilityGroupConverterProjection,
	catalog *currentCustomAbilityCatalog,
	drawIndex currentCustomAbilityDrawIndex,
) (currentCustomAbilityModifyResult, error) {
	if repositories.CharacterItems == nil || catalog == nil || drawIndex == nil ||
		request.optionPosition > 1 || source.CharacterID == 0 ||
		strconv.Itoa(int(source.CharacterID)) != characterID ||
		source.SourceListType != dnfrepo.MainInventoryListType || source.SourceSlotIndex < 0 ||
		source.SourceItemID <= 0 {
		return currentCustomAbilityModifyResult{}, errCurrentCustomAbilityModifyInvalid
	}
	group, found := catalog.customAbilityGroupForOption(request.newOptionID)
	if !found || len(group) == 0 {
		return currentCustomAbilityModifyResult{}, errCurrentCustomAbilityModifyInvalid
	}
	drawnIndex, err := drawIndex(len(group))
	if err != nil {
		return currentCustomAbilityModifyResult{}, fmt.Errorf("draw custom ability group option: %w", err)
	}
	if drawnIndex < 0 || drawnIndex >= len(group) {
		return currentCustomAbilityModifyResult{}, errCurrentCustomAbilityModifyInvalid
	}
	drawnOptionID := group[drawnIndex]

	var result currentCustomAbilityModifyResult
	err = dnfrepo.WithinCharacterItemsScoped(ctx, repositories.CharacterItems, characterID, func(inventoryRepo dnfrepo.InventoryRepository, _ dnfrepo.EquipmentRepository) error {
		inventory, found, err := inventoryRepo.Load(ctx, characterID)
		if err != nil {
			return err
		}
		if !found {
			return errCurrentCustomAbilityModifyInvalid
		}
		inventory = dnfrepo.CloneInventory(inventory)
		targetKey := currentCeraShopInventorySlotKey(dnfrepo.MainInventoryListType, request.targetSlot)
		target, found := inventory.Slots[targetKey]
		if !found || target.ItemID != int64(expectedItemID) || target.Count != 1 {
			return errCurrentCustomAbilityModifyInvalid
		}
		targetEntry := currentItemListEntryFromStack(dnfrepo.MainInventoryListType, request.targetSlot, target)
		state, valid := currentCustomAbilityStateFromRaw(targetEntry.data[:], catalog)
		if !valid {
			return errCurrentCustomAbilityModifyInvalid
		}

		sourceKey := currentCeraShopInventorySlotKey(source.SourceListType, source.SourceSlotIndex)
		device, found := inventory.Slots[sourceKey]
		if !found || device.ItemID != source.SourceItemID || device.Count <= 0 || sourceKey == targetKey {
			return errCurrentCustomAbilityModifyMaterial
		}
		oldOptionID := state.option0
		if request.optionPosition == 1 {
			oldOptionID = state.option1
		}
		if request.optionPosition == 0 {
			state.option0 = drawnOptionID
		} else {
			state.option1 = drawnOptionID
		}
		target.RawEntry, target.Extra = applyCurrentCustomAbilityState(targetEntry.data[:], target.Extra, state)
		inventory.Slots[targetKey] = target

		device.Count--
		remaining := device.Count
		if remaining == 0 {
			delete(inventory.Slots, sourceKey)
		} else {
			entry := currentItemListEntryFromStack(source.SourceListType, source.SourceSlotIndex, device)
			device.RawEntry = append([]byte(nil), entry.data[:]...)
			inventory.Slots[sourceKey] = device
		}
		inventory.UpdatedAt = time.Now().UTC()
		if err := dnfrepo.SaveInventoryFields(ctx, inventoryRepo, inventory, dnfrepo.InventoryFieldSlots); err != nil {
			return err
		}
		refreshes := []alignedcmd.ItemSlotRefresh{
			{ListType: dnfrepo.MainInventoryListType, SlotIndex: request.targetSlot},
			{ListType: source.SourceListType, SlotIndex: source.SourceSlotIndex},
		}
		sort.Slice(refreshes, func(i, j int) bool {
			if refreshes[i].ListType != refreshes[j].ListType {
				return refreshes[i].ListType < refreshes[j].ListType
			}
			return refreshes[i].SlotIndex < refreshes[j].SlotIndex
		})
		result = currentCustomAbilityModifyResult{
			itemID:             expectedItemID,
			optionPosition:     request.optionPosition,
			oldOptionID:        oldOptionID,
			newOptionID:        drawnOptionID,
			grade:              state.grade,
			groupConverter:     true,
			converterItemID:    source.SourceItemID,
			converterRemaining: remaining,
			refreshes:          refreshes,
		}
		return nil
	})
	return result, err
}

func decodeCurrentCustomAbilityModifyRequest(body []byte) (currentCustomAbilityModifyRequest, error) {
	if len(body) != currentCustomAbilityModifyRequestSize {
		return currentCustomAbilityModifyRequest{}, errCurrentCustomAbilityModifyInvalid
	}
	slot := binary.LittleEndian.Uint16(body[0:2])
	request := currentCustomAbilityModifyRequest{
		targetSlot:     int16(slot),
		optionPosition: body[2],
		newOptionID:    body[3],
	}
	if slot > math.MaxInt16 || request.optionPosition > 1 || request.newOptionID == 0 {
		return currentCustomAbilityModifyRequest{}, errCurrentCustomAbilityModifyInvalid
	}
	return request, nil
}

func buildCurrentCustomAbilityModifyFailureBody(err error) []byte {
	code := currentCustomAbilityModifyErrorGeneric
	if errors.Is(err, errCurrentCustomAbilityModifyMaterial) {
		code = currentCustomAbilityModifyErrorMaterial
	}
	return []byte{0, code}
}

func buildCurrentCustomAbilityModifySuccessBody() []byte {
	return []byte{1}
}

func commitCurrentCustomAbilityModify(
	ctx context.Context,
	repositories dnfrepo.Group,
	characterID string,
	request currentCustomAbilityModifyRequest,
	expectedItemID uint32,
	catalog *currentCustomAbilityCatalog,
) (currentCustomAbilityModifyResult, error) {
	if repositories.CharacterItems == nil || catalog == nil || request.optionPosition > 1 {
		return currentCustomAbilityModifyResult{}, errCurrentCustomAbilityModifyInvalid
	}
	if _, valid := catalog.optionIDs[request.newOptionID]; !valid {
		return currentCustomAbilityModifyResult{}, errCurrentCustomAbilityModifyInvalid
	}

	var result currentCustomAbilityModifyResult
	err := repositories.CharacterItems.WithinCharacterItems(ctx, characterID, func(inventoryRepo dnfrepo.InventoryRepository, _ dnfrepo.EquipmentRepository) error {
		inventory, found, err := inventoryRepo.Load(ctx, characterID)
		if err != nil {
			return err
		}
		if !found {
			return errCurrentCustomAbilityModifyInvalid
		}
		inventory = dnfrepo.CloneInventory(inventory)
		targetKey := currentCeraShopInventorySlotKey(dnfrepo.MainInventoryListType, request.targetSlot)
		target, found := inventory.Slots[targetKey]
		if !found || target.ItemID != int64(expectedItemID) || target.Count != 1 {
			return errCurrentCustomAbilityModifyInvalid
		}
		targetEntry := currentItemListEntryFromStack(dnfrepo.MainInventoryListType, request.targetSlot, target)
		state, valid := currentCustomAbilityStateFromRaw(targetEntry.data[:], catalog)
		if !valid {
			return errCurrentCustomAbilityModifyInvalid
		}
		gradeMaterials, found := catalog.modifyMaterial[request.newOptionID]
		if !found {
			return errCurrentCustomAbilityModifyInvalid
		}
		material, found := gradeMaterials[state.grade]
		if !found || material.itemID == 0 || material.count <= 0 {
			return errCurrentCustomAbilityModifyInvalid
		}

		oldOptionID := state.option0
		if request.optionPosition == 1 {
			oldOptionID = state.option1
		}
		if oldOptionID == request.newOptionID {
			result = currentCustomAbilityModifyResult{
				itemID:         expectedItemID,
				optionPosition: request.optionPosition,
				oldOptionID:    oldOptionID,
				newOptionID:    request.newOptionID,
				grade:          state.grade,
				material:       material,
				idempotent:     true,
				refreshes: []alignedcmd.ItemSlotRefresh{{
					ListType:  dnfrepo.MainInventoryListType,
					SlotIndex: request.targetSlot,
				}},
			}
			return nil
		}

		materialSlots := currentCompoundSlotsForItem(inventory.Slots, int64(material.itemID))
		available := int64(0)
		for _, slot := range materialSlots {
			available += inventory.Slots[currentCeraShopInventorySlotKey(dnfrepo.MainInventoryListType, slot)].Count
		}
		if available < material.count {
			return errCurrentCustomAbilityModifyMaterial
		}

		refreshes := []alignedcmd.ItemSlotRefresh{{ListType: dnfrepo.MainInventoryListType, SlotIndex: request.targetSlot}}
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

		if request.optionPosition == 0 {
			state.option0 = request.newOptionID
		} else {
			state.option1 = request.newOptionID
		}
		target.RawEntry, target.Extra = applyCurrentCustomAbilityState(targetEntry.data[:], target.Extra, state)
		inventory.Slots[targetKey] = target
		inventory.UpdatedAt = time.Now().UTC()
		if err := dnfrepo.SaveInventoryFields(ctx, inventoryRepo, inventory, dnfrepo.InventoryFieldSlots); err != nil {
			return err
		}
		sort.Slice(refreshes, func(i, j int) bool {
			if refreshes[i].ListType != refreshes[j].ListType {
				return refreshes[i].ListType < refreshes[j].ListType
			}
			return refreshes[i].SlotIndex < refreshes[j].SlotIndex
		})
		result = currentCustomAbilityModifyResult{
			itemID:            expectedItemID,
			optionPosition:    request.optionPosition,
			oldOptionID:       oldOptionID,
			newOptionID:       request.newOptionID,
			grade:             state.grade,
			material:          material,
			materialRemaining: available - material.count,
			refreshes:         refreshes,
		}
		return nil
	})
	return result, err
}

func (s *Service) handleCurrentCustomAbilityModify(session *gameSession, body []byte) error {
	request, err := decodeCurrentCustomAbilityModifyRequest(body)
	if err != nil || session == nil || session.selectedCharacterID == 0 {
		if err == nil {
			err = errCurrentCustomAbilityModifyInvalid
		}
		s.logGameEvent(session, "game-custom-ability-modify-rejected", "reason", err, "body_len", len(body))
		return s.sendGameUpperRawClass(session, uint16(dnfenum.CmdPacketCustomAbilityEquipOption), buildCurrentCustomAbilityModifyFailureBody(err), dnfproto.DefaultChannelClassification)
	}
	converterSource, converterMode, converterBusy := session.customAbilityConverter.begin(
		session.selectedCharacterID,
		s.gameplayNow(),
	)
	if converterBusy {
		err = errCurrentCustomAbilityModifyInvalid
		s.logGameEvent(session, "game-custom-ability-group-converter-rejected",
			"reason", "op1340_continuation_already_in_flight",
			"body_len", len(body))
		return s.sendGameUpperRawClass(session, uint16(dnfenum.CmdPacketCustomAbilityEquipOption), buildCurrentCustomAbilityModifyFailureBody(err), dnfproto.DefaultChannelClassification)
	}
	converterCommitted := false
	if converterMode {
		defer func() {
			session.customAbilityConverter.finish(converterCommitted)
		}()
	}

	characterID := strconv.Itoa(int(session.selectedCharacterID))
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.Inventory == nil || repositories.CharacterItems == nil {
		err = fmt.Errorf("custom ability modify repositories unavailable")
		s.logGameEvent(session, "game-custom-ability-modify-rejected", "reason", err, "body_len", len(body))
		return s.sendGameUpperRawClass(session, uint16(dnfenum.CmdPacketCustomAbilityEquipOption), buildCurrentCustomAbilityModifyFailureBody(err), dnfproto.DefaultChannelClassification)
	}
	ctx, cancel := context.WithTimeout(context.Background(), currentRepositorySnapshotTimeout)
	defer cancel()
	inventory, found, err := repositories.Inventory.Load(ctx, characterID)
	var target dnfrepo.ItemStack
	targetFound := false
	if err == nil {
		target, targetFound = inventory.Slots[currentCeraShopInventorySlotKey(dnfrepo.MainInventoryListType, request.targetSlot)]
	}
	if err == nil && (!found || !targetFound || target.ItemID <= 0 || target.ItemID > int64(math.MaxUint32) || target.Count != 1) {
		err = errCurrentCustomAbilityModifyInvalid
	} else if err == nil {
		profile, profileErr := s.resolveCurrentCustomAbilityItemProfile(uint32(target.ItemID))
		if profileErr != nil {
			err = profileErr
		} else if !profile.applicable || profile.optionCount != 2 {
			err = errCurrentCustomAbilityModifyInvalid
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

	var result currentCustomAbilityModifyResult
	if err == nil {
		if converterMode {
			result, err = commitCurrentCustomAbilityGroupConvert(
				ctx,
				repositories,
				characterID,
				request,
				uint32(target.ItemID),
				converterSource,
				catalog,
				currentCustomAbilitySecureDrawIndex,
			)
		} else {
			result, err = commitCurrentCustomAbilityModify(ctx, repositories, characterID, request, uint32(target.ItemID), catalog)
		}
	}
	if err != nil {
		s.logGameEvent(session, "game-custom-ability-modify-rejected",
			"slot", request.targetSlot,
			"option_position", request.optionPosition,
			"new_option_id", request.newOptionID,
			"item_id", target.ItemID,
			"reason", err,
			"body_len", len(body))
		return s.sendGameUpperRawClass(session, uint16(dnfenum.CmdPacketCustomAbilityEquipOption), buildCurrentCustomAbilityModifyFailureBody(err), dnfproto.DefaultChannelClassification)
	}
	converterCommitted = converterMode

	s.logGameEvent(session, "game-custom-ability-modify-committed",
		"slot", request.targetSlot,
		"item_id", result.itemID,
		"option_position", result.optionPosition,
		"option_before", result.oldOptionID,
		"option_after", result.newOptionID,
		"grade", result.grade,
		"material_item_id", result.material.itemID,
		"material_cost", result.material.count,
		"material_remaining", result.materialRemaining,
		"group_converter", result.groupConverter,
		"converter_item_id", result.converterItemID,
		"converter_remaining", result.converterRemaining,
		"idempotent", result.idempotent,
		"refresh_rows", len(result.refreshes),
		"sequence", "op14_list0_then_class1_op1340_success")
	if err := s.sendSelectedIncrementalItemSlotRefreshes(session, "custom_ability_modify_commit", result.refreshes); err != nil {
		return err
	}
	return s.sendGameUpperRawClass(session, uint16(dnfenum.CmdPacketCustomAbilityEquipOption), buildCurrentCustomAbilityModifySuccessBody(), dnfproto.DefaultChannelClassification)
}
