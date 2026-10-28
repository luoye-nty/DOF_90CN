package inventory

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

func (o *Owner) convertChronicleEquipment(
	ctx context.Context,
	cmd Command,
	policy alignedcmd.ChronicleGrowthResolution,
) (ChronicleGrowthResult, error) {
	conversion := policy.Conversion
	if conversion == nil || policy.TicketItemID != cmd.ChronicleTicketID ||
		conversion.TicketType < 0 || conversion.OutputItemID <= 0 ||
		conversion.OutputQualitySeed == 0 ||
		len(conversion.Inputs) == 0 {
		return ChronicleGrowthResult{}, ErrChronicleRestricted
	}
	characterID := strconv.FormatUint(uint64(cmd.SelectedCharacterID), 10)
	accountID := strings.TrimSpace(cmd.AccountID)
	usesAccountMaterials := false
	for _, input := range conversion.Inputs {
		if _, accountOwned := upgradeAccountSharedMaterialSlot(input.ItemID); accountOwned {
			usesAccountMaterials = true
			break
		}
	}
	if usesAccountMaterials && (accountID == "" || o.accountRepo == nil || o.accountItems == nil) {
		return ChronicleGrowthResult{}, ErrAccountSharedOwnerUnavailable
	}
	var result ChronicleGrowthResult
	apply := func(txOwner *Owner) error {
		record, ok, err := txOwner.repo.Load(ctx, characterID)
		if err != nil {
			return err
		}
		if !ok {
			return ErrInventoryNotFound
		}
		record = dnfrepo.CloneInventory(record)
		if record.Slots == nil {
			return ErrInventoryNotFound
		}
		var account dnfrepo.AccountInventoryRecord
		if usesAccountMaterials {
			loaded, found, loadErr := txOwner.accountRepo.Load(ctx, accountID)
			if loadErr != nil {
				return loadErr
			}
			if !found || loaded.Slots == nil {
				return ErrChronicleMaterial
			}
			account = dnfrepo.CloneAccountInventory(loaded)
			account.AccountID = accountID
		}
		ticketKey := slotKey(listTypeMain, cmd.ChronicleTicketSlot)
		targetKey := slotKey(listTypeMain, cmd.ChronicleTargetSlot)
		ticket, ticketOK := record.Slots[ticketKey]
		target, targetOK := record.Slots[targetKey]
		autoMaterials := cmd.ChronicleMaterialSlot == -1 && cmd.ChronicleMaterialID == 0xffff
		var material dnfrepo.ItemStack
		materialOK := false
		if !autoMaterials {
			key := slotKey(listTypeMain, cmd.ChronicleMaterialSlot)
			if dnfrepo.IsAccountSharedInventorySlot(listTypeMain, cmd.ChronicleMaterialSlot) {
				material, materialOK = account.Slots[key]
			} else {
				material, materialOK = record.Slots[key]
			}
		}
		if !ticketOK || ticket.ItemID != cmd.ChronicleTicketID || ticket.Count <= 0 ||
			(!ticket.ExpireAt.IsZero() && !ticket.ExpireAt.After(time.Now())) ||
			!targetOK || target.ItemID != cmd.ChronicleTargetID || target.Count != 1 ||
			isEquipmentLocked(target) ||
			(!autoMaterials && (!materialOK || material.ItemID != cmd.ChronicleMaterialID || material.Count <= 0)) {
			return ErrChronicleMaterial
		}

		requirements := make(map[int64]int64, len(conversion.Inputs))
		materialRequested := false
		for _, input := range conversion.Inputs {
			if input.ItemID <= 0 || input.Count <= 0 || requirements[input.ItemID] > math.MaxInt64-input.Count {
				return ErrChronicleRestricted
			}
			requirements[input.ItemID] += input.Count
			if input.ItemID == cmd.ChronicleMaterialID {
				materialRequested = true
			}
		}
		if !autoMaterials && !materialRequested {
			return ErrChronicleMaterial
		}

		consumed := make(map[int16]ChronicleGrowthConsumption)
		accountDirty := false
		consumeSlot := func(accountOwned bool, slot int16, count int64) error {
			if count <= 0 || slot == cmd.ChronicleTargetSlot {
				return ErrChronicleRestricted
			}
			key := slotKey(listTypeMain, slot)
			items := record.Slots
			if accountOwned {
				items = account.Slots
			}
			stack, found := items[key]
			if !found || stack.Count < count {
				return ErrChronicleMaterial
			}
			stack = cloneStack(stack)
			stack.Count -= count
			if stack.Count == 0 {
				delete(items, key)
			} else {
				updateStackRawAmount(&stack)
				items[key] = stack
			}
			if accountOwned {
				accountDirty = true
			}
			row := consumed[slot]
			row.ListType = listTypeMain
			row.Slot = slot
			row.ItemID = stack.ItemID
			row.Count += count
			row.Remain = stack.Count
			consumed[slot] = row
			return nil
		}

		// The exact ticket row is part of the client request and must be consumed
		// first. Recipe materials may span ordinary character stacks plus the
		// current client's account-owned fixed crystal/soul slots.
		if err := consumeSlot(false, cmd.ChronicleTicketSlot, 1); err != nil {
			return err
		}
		inputIDs := make([]int64, 0, len(requirements))
		for itemID := range requirements {
			inputIDs = append(inputIDs, itemID)
		}
		sort.Slice(inputIDs, func(i, j int) bool { return inputIDs[i] < inputIDs[j] })
		for _, itemID := range inputIDs {
			remaining := requirements[itemID]
			if sharedSlot, accountOwned := upgradeAccountSharedMaterialSlot(itemID); accountOwned {
				if stack, found := account.Slots[dnfrepo.AccountSharedInventorySlotKey(sharedSlot)]; found && stack.ItemID == itemID && stack.Count > 0 {
					take := remaining
					if stack.Count < take {
						take = stack.Count
					}
					if err := consumeSlot(true, sharedSlot, take); err != nil {
						return err
					}
					remaining -= take
				}
			}
			slots := currentChronicleMaterialSlots(record.Slots, itemID, cmd.ChronicleTargetSlot, cmd.ChronicleMaterialSlot)
			for _, slot := range slots {
				if remaining == 0 {
					break
				}
				stack := record.Slots[slotKey(listTypeMain, slot)]
				take := remaining
				if stack.Count < take {
					take = stack.Count
				}
				if err := consumeSlot(false, slot, take); err != nil {
					return err
				}
				remaining -= take
			}
			if remaining != 0 {
				return ErrChronicleMaterial
			}
		}

		output := buildChronicleConversionOutput(cmd.ChronicleTargetSlot, target, *conversion)
		if output.ItemID <= 0 || len(output.RawEntry) != chronicleEquipmentRawSize {
			return fmt.Errorf("%w: invalid converted output", ErrChronicleRestricted)
		}
		record.Slots[targetKey] = output
		now := time.Now()
		record.UpdatedAt = now
		if err := dnfrepo.SaveInventoryFields(ctx, txOwner.repo, record, dnfrepo.InventoryFieldSlots); err != nil {
			return err
		}
		if accountDirty {
			account.UpdatedAt = now
			if err := txOwner.accountRepo.Save(ctx, account); err != nil {
				return err
			}
		}

		rows := make([]ChronicleGrowthConsumption, 0, len(consumed))
		if row, found := consumed[cmd.ChronicleTicketSlot]; found {
			rows = append(rows, row)
			delete(consumed, cmd.ChronicleTicketSlot)
		}
		remainingSlots := make([]int, 0, len(consumed))
		for slot := range consumed {
			remainingSlots = append(remainingSlots, int(slot))
		}
		sort.Ints(remainingSlots)
		for _, rawSlot := range remainingSlots {
			rows = append(rows, consumed[int16(rawSlot)])
		}
		result = ChronicleGrowthResult{
			TicketSlot: cmd.ChronicleTicketSlot, TargetSlot: cmd.ChronicleTargetSlot, MaterialSlot: cmd.ChronicleMaterialSlot,
			OutputItemID: conversion.OutputItemID, Conversion: true, Succeeded: true, Consumptions: rows,
		}
		return nil
	}
	var err error
	if usesAccountMaterials && !o.inAccountItemTx {
		err = o.accountItems.WithinAccountCharacterItems(ctx, accountID, characterID, func(accounts dnfrepo.AccountInventoryRepository, characters dnfrepo.InventoryRepository) error {
			txOwner := *o
			txOwner.accountRepo = accounts
			txOwner.repo = characters
			txOwner.inItemTransaction = true
			txOwner.inAccountItemTx = true
			return apply(&txOwner)
		})
	} else if o.inItemTransaction || o.inAccountItemTx {
		err = apply(o)
	} else {
		err = o.withinInventoryTransaction(ctx, characterID, apply)
	}
	return result, err
}

func currentChronicleMaterialSlots(slots map[string]dnfrepo.ItemStack, itemID int64, excludedTarget, requested int16) []int16 {
	result := make([]int16, 0)
	for key, stack := range slots {
		listType, slot, ok := parseSlotKey(key)
		if !ok || listType != listTypeMain || dnfrepo.IsAccountSharedInventorySlot(listType, slot) ||
			slot == excludedTarget || stack.ItemID != itemID || stack.Count <= 0 {
			continue
		}
		result = append(result, slot)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i] == requested {
			return true
		}
		if result[j] == requested {
			return false
		}
		return result[i] < result[j]
	})
	return result
}

func buildChronicleConversionOutput(slot int16, source dnfrepo.ItemStack, conversion alignedcmd.ChronicleEquipmentConversionResolution) dnfrepo.ItemStack {
	quality := strconv.FormatUint(uint64(conversion.OutputQualitySeed), 10)
	durability := strconv.FormatUint(uint64(conversion.OutputDurability), 10)
	seal := firstExtraInt(source.Extra, boolInt64(source.Bind), "seal_flag", "seal")
	output := dnfrepo.ItemStack{
		ItemID: conversion.OutputItemID,
		Count:  1,
		Bind:   conversion.BindOutput,
		Extra: map[string]string{
			"item_kind":         "equipment",
			"pvf_path":          conversion.OutputPVFPath,
			"equipment_type":    conversion.OutputEquipmentType,
			"rarity":            strconv.FormatInt(conversion.OutputRarity, 10),
			"grade":             strconv.FormatInt(conversion.OutputGrade, 10),
			"minimum_level":     strconv.FormatInt(conversion.OutputMinimumLevel, 10),
			"durability":        durability,
			"max_durability":    durability,
			"seal_flag":         strconv.FormatInt(seal, 10),
			"quality_seed":      quality,
			"amount_or_count":   quality,
			"trade_restriction": strconv.FormatBool(conversion.BindOutput),
			"source":            "equipment_emancipate_op271",
		},
	}
	output.RawEntry = currentRawEntryForStack(slot, output)
	if conversion.PreserveAttributes {
		setUpgradeLevel(&output, upgradeLevelOf(source))
		amplifyType, amplifyValue := upgradeAmplifyState(source)
		if amplifyValue > math.MaxUint16 {
			amplifyValue = math.MaxUint16
		}
		setAmplifyState(&output, amplifyType, uint16(amplifyValue))
		level := chronicleForgingLevel(source)
		output.Extra["forging_level"] = strconv.Itoa(level)
		output.Extra["byte_57"] = strconv.Itoa(level)
		output.RawEntry[currentForgingRawOffset] = byte(level)
		cardID := firstExtraInt(source.Extra, 0, "enchant_card_id", "value_a")
		if cardID == 0 && len(source.RawEntry) >= 0x13 {
			cardID = int64(uint32(source.RawEntry[0x0E]) | uint32(source.RawEntry[0x0F])<<8 | uint32(source.RawEntry[0x10])<<16 | uint32(source.RawEntry[0x11])<<24)
		}
		setEnchantFields(&output, cardID, enchantUpgradeCountOf(source))
	} else if conversion.UpgradeMaximum >= 0 || conversion.AmplifyMaximum >= 0 || conversion.ForgingMaximum >= 0 {
		if conversion.UpgradeMaximum >= 0 {
			setUpgradeLevel(&output, byte(minInt(int(upgradeLevelOf(source)), conversion.UpgradeMaximum)))
		}
		if conversion.AmplifyMaximum >= 0 {
			amplifyType, amplifyValue := upgradeAmplifyState(source)
			if amplifyValue > int64(conversion.AmplifyMaximum) {
				amplifyValue = int64(conversion.AmplifyMaximum)
			}
			if amplifyValue > math.MaxUint16 {
				amplifyValue = math.MaxUint16
			}
			setAmplifyState(&output, amplifyType, uint16(amplifyValue))
		}
		if conversion.ForgingMaximum >= 0 {
			level := minInt(chronicleForgingLevel(source), conversion.ForgingMaximum)
			output.Extra["forging_level"] = strconv.Itoa(level)
			output.Extra["byte_57"] = strconv.Itoa(level)
			output.RawEntry[currentForgingRawOffset] = byte(level)
		}
		cardID := firstExtraInt(source.Extra, 0, "enchant_card_id", "value_a")
		if cardID == 0 && len(source.RawEntry) >= 0x13 {
			cardID = int64(uint32(source.RawEntry[0x0E]) | uint32(source.RawEntry[0x0F])<<8 | uint32(source.RawEntry[0x10])<<16 | uint32(source.RawEntry[0x11])<<24)
		}
		setEnchantFields(&output, cardID, enchantUpgradeCountOf(source))
	}
	return output
}

func minInt(left, right int) int {
	if left < right {
		return left
	}
	return right
}
