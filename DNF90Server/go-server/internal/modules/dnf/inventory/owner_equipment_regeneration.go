package inventory

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

const equipmentRegenerationErrorDefault byte = 17

type EquipmentRegenerationChangedRow struct {
	SlotIndex int16
	Stack     dnfrepo.ItemStack
}

type EquipmentRegenerationResult struct {
	Success         bool
	ErrorCode       byte
	SourceSlotIndex int16
	SourceItemID    int64
	ResultItemID    int64
	ResultStack     dnfrepo.ItemStack
	ChangedRows     []EquipmentRegenerationChangedRow
	CandidateCount  int
	Changed         bool
}

func (o *Owner) RegenerateEquipment(ctx context.Context, accountID string, characterID uint16, request EquipmentRegenerationRequest, resolver alignedcmd.EquipmentRegenerationResolver) (EquipmentRegenerationResult, error) {
	base := EquipmentRegenerationResult{ErrorCode: equipmentRegenerationErrorDefault, SourceSlotIndex: request.SourceSlotIndex}
	if o == nil || o.repo == nil || o.accountRepo == nil {
		return base, ErrOwnerUnavailable
	}
	if characterID == 0 {
		return base, ErrCharacterRequired
	}
	accountID = strings.TrimSpace(accountID)
	if accountID == "" {
		return base, ErrAccountRequired
	}
	if resolver == nil {
		return base, fmt.Errorf("equipment regeneration runtime-PVF resolver required")
	}
	if request.SourceSlotIndex < 9 || request.Mode > 1 {
		return base, nil
	}

	characterKey := strconv.FormatUint(uint64(characterID), 10)
	if !o.inItemTransaction {
		var result EquipmentRegenerationResult
		err := o.withinAccountCharacterAssetTransaction(ctx, accountID, characterKey, func(txOwner *Owner) error {
			var innerErr error
			result, innerErr = txOwner.RegenerateEquipment(ctx, accountID, characterID, request, resolver)
			return innerErr
		})
		if err != nil {
			return base, err
		}
		return result, nil
	}

	inventoryRecord, found, err := o.repo.Load(ctx, characterKey)
	if err != nil {
		return base, err
	}
	if !found {
		return base, ErrInventoryNotFound
	}
	inventoryRecord = dnfrepo.CloneInventory(inventoryRecord)
	if inventoryRecord.Slots == nil {
		inventoryRecord.Slots = make(map[string]dnfrepo.ItemStack)
	}
	sourceKey := slotKey(listTypeMain, request.SourceSlotIndex)
	source, found := inventoryRecord.Slots[sourceKey]
	if !found || source.ItemID <= 0 || source.Count != 1 || isEquipmentLocked(source) || !isUpgradeableEquipmentStack(source) {
		return base, nil
	}
	base.SourceItemID = source.ItemID

	resolution, err := resolver(source.ItemID, request.Mode, request.Part)
	if err != nil {
		return base, err
	}
	if !resolution.Eligible || resolution.SourceItemID != source.ItemID || resolution.ResultItemID <= 0 ||
		strings.TrimSpace(resolution.SourcePVFPath) == "" || strings.TrimSpace(resolution.ResultPVFPath) == "" ||
		strings.TrimSpace(resolution.ResultEquipmentType) == "" || resolution.QualitySeed == 0 || len(resolution.Materials) == 0 {
		return base, nil
	}

	accountRecord, _, err := o.accountRepo.Load(ctx, accountID)
	if err != nil {
		return base, err
	}
	accountRecord = dnfrepo.CloneAccountInventory(accountRecord)
	accountRecord.AccountID = accountID
	if accountRecord.Slots == nil {
		accountRecord.Slots = make(map[string]dnfrepo.ItemStack)
	}

	changed := make(map[int16]dnfrepo.ItemStack)
	for _, requirement := range resolution.Materials {
		if requirement.ItemID <= 0 || requirement.Count <= 0 {
			return base, nil
		}
		remaining := requirement.Count
		if requirement.AccountShared {
			slot, ok := upgradeAccountSharedMaterialSlot(requirement.ItemID)
			if !ok {
				return base, nil
			}
			key := slotKey(listTypeMain, slot)
			stack, ok := accountRecord.Slots[key]
			if !ok || stack.ItemID != requirement.ItemID || stack.Count < remaining {
				return base, nil
			}
			stack = cloneStack(stack)
			stack.Count -= remaining
			if stack.Count == 0 {
				delete(accountRecord.Slots, key)
				stack = dnfrepo.ItemStack{}
			} else {
				updateStackRawAmount(&stack)
				accountRecord.Slots[key] = stack
			}
			changed[slot] = stack
			continue
		}

		slots := make([]int, 0)
		for key, stack := range inventoryRecord.Slots {
			listType, slot, ok := parseSlotKey(key)
			if ok && listType == listTypeMain && slot != request.SourceSlotIndex && stack.ItemID == requirement.ItemID && stack.Count > 0 {
				slots = append(slots, int(slot))
			}
		}
		sort.Ints(slots)
		for _, rawSlot := range slots {
			if remaining == 0 {
				break
			}
			slot := int16(rawSlot)
			key := slotKey(listTypeMain, slot)
			stack := cloneStack(inventoryRecord.Slots[key])
			take := remaining
			if stack.Count < take {
				take = stack.Count
			}
			stack.Count -= take
			remaining -= take
			if stack.Count == 0 {
				delete(inventoryRecord.Slots, key)
				stack = dnfrepo.ItemStack{}
			} else {
				updateStackRawAmount(&stack)
				inventoryRecord.Slots[key] = stack
			}
			changed[slot] = stack
		}
		if remaining != 0 {
			return base, nil
		}
	}

	sealFlag := firstExtraInt(source.Extra, boolInt64(source.Bind), "seal_flag", "seal", "bind_flag", "bind")
	quality := strconv.FormatUint(uint64(resolution.QualitySeed), 10)
	durability := strconv.FormatUint(uint64(resolution.ResultDurability), 10)
	resultStack := dnfrepo.ItemStack{
		ItemID: resolution.ResultItemID,
		Count:  1,
		Bind:   source.Bind,
		Extra: map[string]string{
			"item_kind":       "equipment",
			"pvf_path":        resolution.ResultPVFPath,
			"equipment_type":  resolution.ResultEquipmentType,
			"rarity":          strconv.FormatInt(resolution.ResultRarity, 10),
			"minimum_level":   strconv.FormatInt(resolution.ResultMinimumLevel, 10),
			"durability":      durability,
			"max_durability":  durability,
			"seal_flag":       strconv.FormatInt(sealFlag, 10),
			"quality_seed":    quality,
			"amount_or_count": quality,
			"source":          "equipment_regeneration_op407",
		},
	}
	resultStack.RawEntry = currentRawEntryForStack(request.SourceSlotIndex, resultStack)
	inventoryRecord.Slots[sourceKey] = resultStack
	changed[request.SourceSlotIndex] = resultStack

	now := time.Now()
	inventoryRecord.UpdatedAt = now
	accountRecord.UpdatedAt = now
	if err := o.accountRepo.Save(ctx, accountRecord); err != nil {
		return base, err
	}
	if err := dnfrepo.SaveInventoryFields(ctx, o.repo, inventoryRecord, dnfrepo.InventoryFieldSlots); err != nil {
		return base, err
	}

	rows := make([]EquipmentRegenerationChangedRow, 0, len(changed))
	for slot, stack := range changed {
		rows = append(rows, EquipmentRegenerationChangedRow{SlotIndex: slot, Stack: cloneStack(stack)})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].SlotIndex < rows[j].SlotIndex })
	return EquipmentRegenerationResult{
		Success:         true,
		SourceSlotIndex: request.SourceSlotIndex,
		SourceItemID:    source.ItemID,
		ResultItemID:    resolution.ResultItemID,
		ResultStack:     cloneStack(resultStack),
		ChangedRows:     rows,
		CandidateCount:  resolution.CandidateCount,
		Changed:         true,
	}, nil
}
