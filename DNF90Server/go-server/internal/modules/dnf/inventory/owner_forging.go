package inventory

import (
	"context"
	"encoding/binary"
	"fmt"
	"math/rand"
	"strconv"
	"strings"
	"time"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

const (
	currentForgingSuccessBase = 10000
	// Current NoPack maps raw+0x57 to the encoded equipment-state field at
	// +0x78, which AppendWeaponForgingTooltipLines uses as its level value.
	currentForgingRawOffset = 0x57
)

// ForgingResult is the durable outcome of one current-client op438 attempt.
// Success means the request was accepted and material was consumed; a chance
// failure keeps the old level while still returning Success=true.
type ForgingResult struct {
	CharacterID            string
	Success                bool
	UpgradeSucceeded       bool
	TargetListType         byte
	TargetSlotIndex        int16
	TargetItemTemplateID   int32
	MaterialSlotIndex      int16
	MaterialItemID         int64
	MaterialCost           int64
	MaterialRemainingCount int64
	OldLevel               byte
	NewLevel               byte
	ReinforceLevel         byte
	IndependentAttack      int64
	SuccessWeight          int
	TargetUpdatedStack     dnfrepo.ItemStack
	MaterialUpdatedStack   dnfrepo.ItemStack
	Changed                bool
}

// Forge validates and commits a PVF-backed weapon-forging attempt. Target and
// material changes share one character-item transaction for both list 0 and 3.
func (o *Owner) Forge(ctx context.Context, cmd Command, resolver alignedcmd.ForgingResolver) (ForgingResult, error) {
	base := ForgingResult{
		TargetListType:       cmd.TargetListType,
		TargetSlotIndex:      cmd.TargetSlotIndex,
		TargetItemTemplateID: cmd.TargetItemTemplateID,
		MaterialSlotIndex:    cmd.MaterialSlotIndex,
	}
	if o == nil || o.repo == nil || o.equipment == nil {
		return base, ErrOwnerUnavailable
	}
	if cmd.SelectedCharacterID == 0 {
		return base, ErrCharacterRequired
	}
	if resolver == nil {
		return base, fmt.Errorf("forging runtime-PVF resolver required")
	}
	if cmd.TargetListType != listTypeMain && cmd.TargetListType != listTypeEquipment {
		return base, nil
	}
	if cmd.TargetSlotIndex < 0 || cmd.MaterialSlotIndex < 0 || dnfrepo.IsAccountSharedInventorySlot(listTypeMain, cmd.MaterialSlotIndex) {
		return base, nil
	}
	if cmd.TargetListType == listTypeMain && cmd.TargetSlotIndex == cmd.MaterialSlotIndex {
		return base, nil
	}

	characterID := strconv.FormatUint(uint64(cmd.SelectedCharacterID), 10)
	base.CharacterID = characterID
	if !o.inItemTransaction {
		var result ForgingResult
		err := o.withinInventoryTransaction(ctx, characterID, func(txOwner *Owner) error {
			var innerErr error
			result, innerErr = txOwner.Forge(ctx, cmd, resolver)
			return innerErr
		})
		if err != nil {
			return base, err
		}
		if result.CharacterID == "" {
			result = base
		}
		return result, nil
	}

	inventoryRecord, found, err := o.repo.Load(ctx, characterID)
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

	var target dnfrepo.ItemStack
	var equipmentRecord dnfrepo.EquipmentRecord
	var equipmentEntry dnfrepo.EquipmentEntry
	var targetKey string
	switch cmd.TargetListType {
	case listTypeMain:
		targetKey = slotKey(listTypeMain, cmd.TargetSlotIndex)
		stack, ok := inventoryRecord.Slots[targetKey]
		if !ok || stack.ItemID != int64(cmd.TargetItemTemplateID) || stack.Count <= 0 {
			return base, nil
		}
		target = cloneStack(stack)
	case listTypeEquipment:
		loaded, ok, loadErr := o.equipment.Load(ctx, characterID)
		if loadErr != nil {
			return base, loadErr
		}
		if !ok {
			return base, nil
		}
		equipmentRecord = dnfrepo.CloneEquipment(loaded)
		targetKey = strconv.FormatInt(int64(cmd.TargetSlotIndex), 10)
		entry, ok := equipmentRecord.Entries[targetKey]
		if !ok || entry.ItemID != int64(cmd.TargetItemTemplateID) {
			return base, nil
		}
		equipmentEntry = entry
		target = forgingStackFromEquipmentEntry(entry)
	}

	if isEquipmentLocked(target) {
		return base, nil
	}
	if !equipmentStackCountIsSingle(target) {
		// 一份锻造材料只能作用一件装备：历史堆叠行（装备 Count>1）一律拒绝。
		return base, nil
	}
	oldLevel := forgingLevelOf(target)
	base.OldLevel = oldLevel
	base.NewLevel = oldLevel
	base.ReinforceLevel = upgradeLevelOf(target)
	materialKey := slotKey(listTypeMain, cmd.MaterialSlotIndex)
	material, ok := inventoryRecord.Slots[materialKey]
	if !ok || material.ItemID <= 0 || material.Count <= 0 ||
		(!material.ExpireAt.IsZero() && !material.ExpireAt.After(time.Now())) {
		return base, nil
	}
	// The request-scoped forging resolver proves the material template against
	// the active PVF below. Do not require the durable inventory row to already
	// carry item_kind/pvf_path metadata: older, mailed and package-granted
	// one-time forging devices can be valid stackables while retaining a legacy
	// minimal row. Requiring pvfStackContract here rejected those tools before
	// their authoritative [portable genuine damage upgrade upgrede] document
	// was ever consulted.
	resolution, err := resolver(material.ItemID, int64(cmd.TargetItemTemplateID), oldLevel)
	if err != nil {
		return base, err
	}
	switch {
	case resolution.TicketRandom:
		// [enchant random] + [action type] `[random upgrade separate]` family
		// (e.g. +6锻造券): the er_* target gates and the weighted candidate
		// roll are shared with the op50 ticket scene, applied here to the
		// forging level instead of the reinforcement level.
		if !validForgingRandomTicketResolution(resolution) {
			return base, nil
		}
		if !enchantRandomRestrictionAllows(resolution.RandomTicket, resolution.TargetEquipmentType, resolution.TargetRarity, target) {
			return base, nil
		}
		entry, ok := o.selectEnchantRandomEntry(resolution.RandomTicket.Entries)
		if !ok || entry.TargetLevel <= 0 || entry.TargetLevel > int64(resolution.MaxLevel) {
			return base, nil
		}
		resolution.TargetLevel = byte(entry.TargetLevel)
		resolution.SuccessWeight = forgingWeightFromEnchantRandom(entry.Weight)
		resolution.IndependentAttack = resolution.RandomIndependentAttacks[entry.TargetLevel]
	case strings.TrimSpace(resolution.TicketMode) == "":
		if !validForgingResolution(resolution, oldLevel) || !forgingDurabilityOK(target, resolution.TargetDurability) {
			return base, nil
		}
	default:
		if !validForgingTicketResolution(resolution, oldLevel) {
			return base, nil
		}
	}
	if material.ItemID != resolution.MaterialItemID || material.Count < resolution.MaterialCount {
		return base, nil
	}

	upgradeSucceeded := rand.Intn(currentForgingSuccessBase) < resolution.SuccessWeight
	newLevel := oldLevel
	if upgradeSucceeded {
		newLevel = resolution.TargetLevel
		setForgingLevel(&target, newLevel)
		switch cmd.TargetListType {
		case listTypeMain:
			inventoryRecord.Slots[targetKey] = target
		case listTypeEquipment:
			equipmentEntry.RawEntry = append([]byte(nil), target.RawEntry...)
			equipmentEntry.Extra = cloneStringMapForForging(target.Extra)
			equipmentRecord.Entries[targetKey] = equipmentEntry
			equipmentRecord.UpdatedAt = time.Now()
			if err := dnfrepo.SaveEquipmentFields(ctx, o.equipment, equipmentRecord, dnfrepo.EquipmentFieldEntries); err != nil {
				return base, err
			}
		}
	}

	material = cloneStack(material)
	material.Count -= resolution.MaterialCount
	if material.Count <= 0 {
		material = dnfrepo.ItemStack{}
		delete(inventoryRecord.Slots, materialKey)
	} else {
		updateStackRawAmount(&material)
		inventoryRecord.Slots[materialKey] = material
	}
	inventoryRecord.UpdatedAt = time.Now()
	if err := dnfrepo.SaveInventoryFields(ctx, o.repo, inventoryRecord, dnfrepo.InventoryFieldSlots); err != nil {
		return base, err
	}

	if cmd.TargetListType == listTypeEquipment {
		if upgradeSucceeded {
			target = forgingStackFromEquipmentEntry(equipmentRecord.Entries[targetKey])
		} else {
			target = forgingStackFromEquipmentEntry(equipmentEntry)
		}
	}
	base.Success = true
	base.UpgradeSucceeded = upgradeSucceeded
	base.MaterialItemID = resolution.MaterialItemID
	base.MaterialCost = resolution.MaterialCount
	base.MaterialRemainingCount = material.Count
	base.NewLevel = newLevel
	if upgradeSucceeded {
		base.IndependentAttack = resolution.IndependentAttack
	}
	base.SuccessWeight = resolution.SuccessWeight
	base.TargetUpdatedStack = target
	base.MaterialUpdatedStack = material
	base.Changed = true
	return base, nil
}

func validForgingResolution(resolution alignedcmd.ForgingResolution, oldLevel byte) bool {
	if !strings.EqualFold(strings.TrimSpace(resolution.TargetKind), "equipment") {
		return false
	}
	equipmentType := strings.ToLower(strings.TrimSpace(strings.ReplaceAll(resolution.TargetEquipmentType, "`", "")))
	if equipmentType != "[weapon]" && equipmentType != "weapon" {
		return false
	}
	if resolution.MaxLevel == 0 || oldLevel >= resolution.MaxLevel || resolution.TargetLevel != oldLevel+1 || resolution.TargetLevel > resolution.MaxLevel {
		return false
	}
	return resolution.IndependentAttack >= 0 && resolution.SuccessWeight >= 0 && resolution.SuccessWeight <= currentForgingSuccessBase && resolution.MaterialItemID > 0 && resolution.MaterialCount > 0
}

func validForgingTicketResolution(resolution alignedcmd.ForgingResolution, oldLevel byte) bool {
	if !strings.EqualFold(strings.TrimSpace(resolution.TargetKind), "equipment") {
		return false
	}
	equipmentType := strings.ToLower(strings.TrimSpace(strings.ReplaceAll(resolution.TargetEquipmentType, "`", "")))
	if equipmentType != "[weapon]" && equipmentType != "weapon" {
		return false
	}
	mode := strings.ToLower(strings.TrimSpace(resolution.TicketMode))
	if mode != "fixed" && mode != "additional" {
		return false
	}
	if resolution.MaxLevel == 0 || resolution.TargetLevel == 0 || resolution.TargetLevel > resolution.MaxLevel {
		return false
	}
	if mode == "additional" && resolution.TargetLevel <= oldLevel {
		return false
	}
	return resolution.IndependentAttack >= 0 &&
		resolution.SuccessWeight >= 0 && resolution.SuccessWeight <= currentForgingSuccessBase &&
		resolution.MaterialItemID > 0 && resolution.MaterialCount == 1
}

// validForgingRandomTicketResolution accepts the [enchant random] forging
// coupon family before the candidate roll. The concrete level is not known
// yet, so only the target class, the table bound and the one-coupon material
// contract can be checked here.
func validForgingRandomTicketResolution(resolution alignedcmd.ForgingResolution) bool {
	if !strings.EqualFold(strings.TrimSpace(resolution.TargetKind), "equipment") {
		return false
	}
	equipmentType := strings.ToLower(strings.TrimSpace(strings.ReplaceAll(resolution.TargetEquipmentType, "`", "")))
	if equipmentType != "[weapon]" && equipmentType != "weapon" {
		return false
	}
	return resolution.MaxLevel > 0 && len(resolution.RandomTicket.Entries) > 0 &&
		resolution.MaterialItemID > 0 && resolution.MaterialCount == 1
}

// forgingWeightFromEnchantRandom rescales an er_enchant weight (out of 100000)
// to the native op438 probability base of 10000.
func forgingWeightFromEnchantRandom(weight int64) int {
	scaled := weight / (enchantRandomWeightBase / currentForgingSuccessBase)
	if scaled < 0 {
		return 0
	}
	if scaled > currentForgingSuccessBase {
		return currentForgingSuccessBase
	}
	return int(scaled)
}

func forgingDurabilityOK(stack dnfrepo.ItemStack, expected uint16) bool {
	if value := firstExtraInt(stack.Extra, -1, "durability"); value >= 0 {
		return value == int64(expected)
	}
	if len(stack.RawEntry) == currentItemListEntrySize {
		return binary.LittleEndian.Uint16(stack.RawEntry[0x0B:0x0D]) == expected
	}
	return expected == 0
}

func forgingLevelOf(stack dnfrepo.ItemStack) byte {
	if raw := strings.TrimSpace(stack.Extra["forging_level"]); raw != "" {
		if value, err := strconv.ParseUint(raw, 0, 8); err == nil {
			return byte(value)
		}
	}
	if len(stack.RawEntry) == currentItemListEntrySize {
		return stack.RawEntry[currentForgingRawOffset]
	}
	return 0
}

func setForgingLevel(stack *dnfrepo.ItemStack, level byte) {
	if stack == nil {
		return
	}
	stack.RawEntry = append([]byte(nil), stack.RawEntry...)
	stack.Extra = cloneStringMapForForging(stack.Extra)
	stack.Extra["forging_level"] = strconv.Itoa(int(level))
	if len(stack.RawEntry) == currentItemListEntrySize {
		stack.RawEntry[currentForgingRawOffset] = level
		// buildCommonItemListUpdateBody owns these opaque vectors through
		// Extra. Snapshot the current raw bytes so an op14 forging refresh
		// cannot clear adjacent current-client state.
		stack.Extra["raw_data_65"] = fmt.Sprintf("%x", stack.RawEntry[0x65:0x6E])
		if _, ok := stack.Extra["tail_data_72"]; !ok {
			stack.Extra["tail_data_72"] = fmt.Sprintf("%x", stack.RawEntry[0x72:0x77])
		}
	}
}

func forgingStackFromEquipmentEntry(entry dnfrepo.EquipmentEntry) dnfrepo.ItemStack {
	raw := append([]byte(nil), entry.RawEntry...)
	extra := cloneStringMapForForging(entry.Extra)
	if len(raw) == currentItemListEntrySize {
		setForgingProjectionFallback(extra, "amount_or_count", uint64(binary.LittleEndian.Uint32(raw[0x06:0x0A])))
		setForgingProjectionFallback(extra, "ext_data0", uint64(raw[0x0A]))
		setForgingProjectionFallback(extra, "durability", uint64(binary.LittleEndian.Uint16(raw[0x0B:0x0D])))
		setForgingProjectionFallback(extra, "seal_flag", uint64(raw[0x0D]))
		setForgingProjectionFallback(extra, "value_a", uint64(binary.LittleEndian.Uint32(raw[0x0E:0x12])))
		setForgingProjectionFallback(extra, "byte_12", uint64(raw[0x12]))
		setForgingProjectionFallback(extra, "byte_13", uint64(raw[0x13]))
		setForgingProjectionFallback(extra, "marker_16", uint64(binary.LittleEndian.Uint16(raw[0x14:0x16])))
		setForgingProjectionFallback(extra, "byte_57", uint64(raw[0x57]))
		setForgingProjectionFallback(extra, "byte_58", uint64(raw[0x58]))
		setForgingProjectionFallback(extra, "byte_59", uint64(raw[0x59]))
		if _, ok := extra["raw_data_65"]; !ok {
			extra["raw_data_65"] = fmt.Sprintf("%x", raw[0x65:0x6E])
		}
		if _, ok := extra["tail_data_72"]; !ok {
			extra["tail_data_72"] = fmt.Sprintf("%x", raw[0x72:0x77])
		}
	}
	return dnfrepo.ItemStack{
		ItemID:   entry.ItemID,
		Count:    1,
		Bind:     entry.Bind,
		ExpireAt: entry.ExpireAt,
		RawEntry: raw,
		Extra:    extra,
	}
}

func setForgingProjectionFallback(extra map[string]string, key string, value uint64) {
	if _, ok := extra[key]; !ok {
		extra[key] = strconv.FormatUint(value, 10)
	}
}

func cloneStringMapForForging(source map[string]string) map[string]string {
	out := make(map[string]string, len(source)+1)
	for key, value := range source {
		out[key] = value
	}
	return out
}
