package inventory

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

const (
	chronicleOptionVectorOffset         = 0x27
	chronicleOptionVectorSize           = 17
	chronicleOptionCapacity             = 2
	chronicleFailureFragmentCount int64 = 5

	chronicleRefineErrorInvalidMaterial byte = 0x04
	chronicleRefineErrorInvalidTarget   byte = 0x13
)

var errChronicleFailureRewardUnavailable = errors.New("chronicle refine failure reward is unavailable")

type chronicleOption struct {
	CanonicalAuraOptionID int32
	CharacterJob          byte
	FirstGrowType         byte
	EquipmentType         byte
	OptionNo              byte
}

// ChronicleRefineReward is one destruction reward row consumed by the
// current op370 result reader. UpdatedStack is the final durable slot state
// used by the following op14 refresh.
type ChronicleRefineReward struct {
	SlotIndex    int16
	ItemID       int64
	Count        int64
	UpdatedStack dnfrepo.ItemStack
}

// ChronicleRefineResult is the durable result of one current-client op370
// attempt. A failed chance roll consumes the aura, destroys the target, and
// grants only five dimensional fragments plus the PVF-backed no-element cube
// baseline. Normal disassembly additional rewards are deliberately excluded.
type ChronicleRefineResult struct {
	CharacterID            string
	Success                bool
	ErrorCode              byte
	MaterialSlotIndex      int16
	MaterialRemainingCount int64
	TargetListType         byte
	TargetSlotIndex        int16
	TargetUpdatedStack     dnfrepo.ItemStack
	MaterialUpdatedStack   dnfrepo.ItemStack
	AuraType               byte
	CanonicalAuraOptionID  int32
	OptionCount            byte
	Probability            byte
	ProbabilityRoll        byte
	RefineSucceeded        bool
	TargetDestroyed        bool
	FailureRewards         []ChronicleRefineReward
	Changed                bool
}

// RefineChronicle applies one PVF-authorized aura record and consumes one
// material in the same character-asset transaction.
func (o *Owner) RefineChronicle(ctx context.Context, cmd Command, resolver alignedcmd.ChronicleRefineResolver) (ChronicleRefineResult, error) {
	base := ChronicleRefineResult{
		ErrorCode:         chronicleRefineErrorInvalidMaterial,
		MaterialSlotIndex: cmd.MaterialSlotIndex,
		TargetListType:    cmd.TargetListType,
		TargetSlotIndex:   cmd.TargetSlotIndex,
	}
	if o == nil || o.repo == nil || o.equipment == nil || o.characters == nil {
		return base, ErrOwnerUnavailable
	}
	if cmd.SelectedCharacterID == 0 {
		return base, ErrCharacterRequired
	}
	if resolver == nil {
		return base, fmt.Errorf("chronicle refine runtime-PVF resolver required")
	}
	if cmd.MaterialSlotIndex < 0 || cmd.TargetSlotIndex < 0 ||
		(cmd.TargetListType != listTypeMain && cmd.TargetListType != listTypeEquipment) ||
		(cmd.TargetListType == listTypeMain && cmd.MaterialSlotIndex == cmd.TargetSlotIndex) {
		return base, nil
	}

	characterID := strconv.FormatUint(uint64(cmd.SelectedCharacterID), 10)
	base.CharacterID = characterID
	if !o.inItemTransaction {
		accountID := strings.TrimSpace(cmd.AccountID)
		if accountID == "" {
			return base, ErrAccountRequired
		}
		var result ChronicleRefineResult
		err := o.withinAccountCharacterAssetTransaction(ctx, accountID, characterID, func(txOwner *Owner) error {
			var innerErr error
			result, innerErr = txOwner.RefineChronicle(ctx, cmd, resolver)
			return innerErr
		})
		if err != nil {
			return base, err
		}
		return result, nil
	}

	character, found, err := o.characters.Load(ctx, characterID)
	if err != nil {
		return base, err
	}
	if !found {
		return base, ErrCharacterRequired
	}
	jobValue, err := strconv.ParseUint(strings.TrimSpace(character.Job), 10, 8)
	if err != nil {
		return base, fmt.Errorf("chronicle refine character job %q invalid", character.Job)
	}
	firstGrowType := byte(character.Stats["grow_type"]) & 0x0f

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

	materialKey := slotKey(listTypeMain, cmd.MaterialSlotIndex)
	material, found := inventoryRecord.Slots[materialKey]
	if !found || material.ItemID != int64(cmd.MaterialItemTemplateID) || material.Count <= 0 || !isStackable(material) {
		return base, nil
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
			base.ErrorCode = chronicleRefineErrorInvalidTarget
			return base, nil
		}
		target = cloneStack(stack)
	case listTypeEquipment:
		loaded, ok, loadErr := o.equipment.Load(ctx, characterID)
		if loadErr != nil {
			return base, loadErr
		}
		if !ok {
			base.ErrorCode = chronicleRefineErrorInvalidTarget
			return base, nil
		}
		equipmentRecord = dnfrepo.CloneEquipment(loaded)
		targetKey = strconv.FormatInt(int64(cmd.TargetSlotIndex), 10)
		entry, ok := equipmentRecord.Entries[targetKey]
		if !ok || entry.ItemID != int64(cmd.TargetItemTemplateID) {
			base.ErrorCode = chronicleRefineErrorInvalidTarget
			return base, nil
		}
		equipmentEntry = entry
		target = forgingStackFromEquipmentEntry(entry)
	}
	if isEquipmentLocked(target) || len(target.RawEntry) != currentItemListEntrySize {
		base.ErrorCode = chronicleRefineErrorInvalidTarget
		return base, nil
	}

	resolution, err := resolver(
		int64(cmd.MaterialItemTemplateID),
		int64(cmd.TargetItemTemplateID),
		byte(jobValue),
		firstGrowType,
		cmd.ChronicleOptionNo,
	)
	if err != nil {
		return base, err
	}
	if !validChronicleRefineResolution(resolution) || !forgingDurabilityOK(target, resolution.TargetDurability) {
		base.ErrorCode = chronicleRefineErrorInvalidTarget
		return base, nil
	}

	options, err := parseChronicleOptions(target.RawEntry)
	if err != nil || len(options) >= chronicleOptionCapacity {
		base.ErrorCode = chronicleRefineErrorInvalidTarget
		return base, nil
	}
	for _, option := range options {
		if option.CanonicalAuraOptionID == resolution.CanonicalAuraOptionID && option.OptionNo == cmd.ChronicleOptionNo {
			base.ErrorCode = chronicleRefineErrorInvalidTarget
			return base, nil
		}
	}
	probability := resolution.Probabilities[len(options)]
	base.Probability = probability
	roll := byte(0)
	refineSucceeded := probability >= 100
	if !refineSucceeded {
		roll = byte(o.chronicleRandomIntn(100))
		refineSucceeded = roll < probability
	}
	base.ProbabilityRoll = roll
	base.RefineSucceeded = refineSucceeded

	material = cloneStack(material)
	material.Count--
	if material.Count == 0 {
		delete(inventoryRecord.Slots, materialKey)
	} else {
		updateStackRawAmount(&material)
		inventoryRecord.Slots[materialKey] = material
	}

	if refineSucceeded {
		options = append(options, chronicleOption{
			CanonicalAuraOptionID: resolution.CanonicalAuraOptionID,
			CharacterJob:          byte(jobValue),
			FirstGrowType:         firstGrowType,
			EquipmentType:         resolution.TargetEquipmentTypeCode,
			OptionNo:              cmd.ChronicleOptionNo,
		})
		setChronicleOptions(&target, options)
		switch cmd.TargetListType {
		case listTypeMain:
			inventoryRecord.Slots[targetKey] = target
		case listTypeEquipment:
			equipmentEntry.RawEntry = append([]byte(nil), target.RawEntry...)
			equipmentEntry.Extra = cloneStringMapForForging(target.Extra)
			equipmentRecord.Entries[targetKey] = equipmentEntry
		}
	} else {
		if !validChronicleFailureResolution(resolution) {
			return base, errChronicleFailureRewardUnavailable
		}
		switch cmd.TargetListType {
		case listTypeMain:
			delete(inventoryRecord.Slots, targetKey)
		case listTypeEquipment:
			delete(equipmentRecord.Entries, targetKey)
		}
		failureRewards, accountRecord, rewardErr := o.grantChronicleFailureRewards(
			ctx,
			strings.TrimSpace(cmd.AccountID),
			&inventoryRecord,
			resolution,
		)
		if rewardErr != nil {
			return base, rewardErr
		}
		base.FailureRewards = failureRewards
		base.TargetDestroyed = true
		accountRecord.UpdatedAt = time.Now()
		if err := o.accountRepo.Save(ctx, accountRecord); err != nil {
			return base, err
		}
	}

	if cmd.TargetListType == listTypeEquipment {
		equipmentRecord.UpdatedAt = time.Now()
		if err := dnfrepo.SaveEquipmentFields(ctx, o.equipment, equipmentRecord, dnfrepo.EquipmentFieldEntries); err != nil {
			return base, err
		}
	}
	inventoryRecord.UpdatedAt = time.Now()
	if err := dnfrepo.SaveInventoryFields(ctx, o.repo, inventoryRecord, dnfrepo.InventoryFieldSlots); err != nil {
		return base, err
	}

	base.Success = true
	base.MaterialRemainingCount = material.Count
	base.TargetUpdatedStack = inventoryRecord.Slots[targetKey]
	if cmd.TargetListType == listTypeEquipment && refineSucceeded {
		base.TargetUpdatedStack = target
	}
	base.MaterialUpdatedStack = inventoryRecord.Slots[materialKey]
	base.AuraType = resolution.AuraType
	base.CanonicalAuraOptionID = resolution.CanonicalAuraOptionID
	base.OptionCount = byte(len(options))
	base.Changed = true
	return base, nil
}

func validChronicleRefineResolution(resolution alignedcmd.ChronicleRefineResolution) bool {
	return resolution.Matched && resolution.CanonicalAuraOptionID == int32(1254)+int32(resolution.AuraType) &&
		resolution.AuraType <= 2 && strings.EqualFold(strings.TrimSpace(resolution.TargetKind), "equipment") &&
		validChronicleEquipmentDefinitionType(resolution.TargetEquipmentTypeCode) &&
		resolution.Probabilities[0] <= 100 && resolution.Probabilities[1] <= 100
}

func validChronicleEquipmentDefinitionType(code byte) bool {
	switch code {
	case 12, 14, 15, 16, 17, 18, 19, 20, 21, 22, 23, 25:
		return true
	default:
		return false
	}
}

func validChronicleFailureResolution(resolution alignedcmd.ChronicleRefineResolution) bool {
	cubeSlot, cubeAccountOwned := upgradeAccountSharedMaterialSlot(resolution.FailureCubeItem.ItemID)
	return resolution.FailureCubeCount > 0 && resolution.FailureCubeItem.ItemID > 0 &&
		strings.EqualFold(strings.TrimSpace(resolution.FailureCubeItem.Kind), "stackable") &&
		resolution.FailureCubeItem.TargetListType == listTypeMain && cubeAccountOwned &&
		cubeSlot >= dnfrepo.CrystalWarehouseFirstSlot && cubeSlot <= dnfrepo.CrystalWarehouseLastSlot &&
		resolution.FailureFragmentItemID == 3311 &&
		resolution.FailureFragmentItem.ItemID == resolution.FailureFragmentItemID &&
		strings.EqualFold(strings.TrimSpace(resolution.FailureFragmentItem.Kind), "stackable") &&
		resolution.FailureFragmentItem.TargetListType == listTypeMain &&
		resolution.FailureFragmentItem.SlotStart >= 0 &&
		resolution.FailureFragmentItem.SlotEnd >= resolution.FailureFragmentItem.SlotStart
}

func (o *Owner) chronicleRandomIntn(limit int) int {
	if limit <= 1 || o == nil || o.chronicleRandom == nil {
		return 0
	}
	value := o.chronicleRandom(limit)
	if value < 0 {
		return 0
	}
	if value >= limit {
		return value % limit
	}
	return value
}

func (o *Owner) grantChronicleFailureRewards(
	ctx context.Context,
	accountID string,
	inventory *dnfrepo.InventoryRecord,
	resolution alignedcmd.ChronicleRefineResolution,
) ([]ChronicleRefineReward, dnfrepo.AccountInventoryRecord, error) {
	if o == nil || o.accountRepo == nil || strings.TrimSpace(accountID) == "" || inventory == nil ||
		!validChronicleFailureResolution(resolution) {
		return nil, dnfrepo.AccountInventoryRecord{}, errChronicleFailureRewardUnavailable
	}
	account, _, err := o.accountRepo.Load(ctx, accountID)
	if err != nil {
		return nil, dnfrepo.AccountInventoryRecord{}, err
	}
	account = dnfrepo.CloneAccountInventory(account)
	account.AccountID = strings.TrimSpace(accountID)
	if account.Slots == nil {
		account.Slots = make(map[string]dnfrepo.ItemStack)
	}
	if inventory.Slots == nil {
		inventory.Slots = make(map[string]dnfrepo.ItemStack)
	}

	rewards, err := grantChronicleBackpackReward(inventory.Slots, resolution.FailureFragmentItem, chronicleFailureFragmentCount)
	if err != nil {
		return nil, dnfrepo.AccountInventoryRecord{}, err
	}
	cubeReward, err := grantChronicleAccountReward(&account, resolution.FailureCubeItem, resolution.FailureCubeCount)
	if err != nil {
		return nil, dnfrepo.AccountInventoryRecord{}, err
	}
	rewards = append(rewards, cubeReward)
	return rewards, account, nil
}

func grantChronicleBackpackReward(items map[string]dnfrepo.ItemStack, reward alignedcmd.MagicBoxRewardItem, count int64) ([]ChronicleRefineReward, error) {
	before := make(map[int16]int64)
	for slot := reward.SlotStart; slot <= reward.SlotEnd; slot++ {
		if stack, found := items[slotKey(listTypeMain, slot)]; found && stack.ItemID == reward.ItemID && stack.Count > 0 {
			before[slot] = stack.Count
		}
	}
	slots, err := grantRandomRewardItem(items, reward, count)
	if err != nil {
		return nil, err
	}
	result := make([]ChronicleRefineReward, 0, len(slots))
	for _, slot := range slots {
		stack, found := items[slotKey(listTypeMain, slot)]
		granted := stack.Count - before[slot]
		if !found || stack.ItemID != reward.ItemID || granted <= 0 {
			return nil, errChronicleFailureRewardUnavailable
		}
		result = append(result, ChronicleRefineReward{
			SlotIndex:    slot,
			ItemID:       reward.ItemID,
			Count:        granted,
			UpdatedStack: cloneStack(stack),
		})
	}
	return result, nil
}

func grantChronicleAccountReward(account *dnfrepo.AccountInventoryRecord, reward alignedcmd.MagicBoxRewardItem, count int64) (ChronicleRefineReward, error) {
	if account == nil || count <= 0 || reward.ItemID <= 0 || !strings.EqualFold(strings.TrimSpace(reward.Kind), "stackable") {
		return ChronicleRefineReward{}, errChronicleFailureRewardUnavailable
	}
	slot, ok := upgradeAccountSharedMaterialSlot(reward.ItemID)
	if !ok || slot < dnfrepo.CrystalWarehouseFirstSlot || slot > dnfrepo.CrystalWarehouseLastSlot {
		return ChronicleRefineReward{}, errChronicleFailureRewardUnavailable
	}
	key := dnfrepo.AccountSharedInventorySlotKey(slot)
	stack, found := account.Slots[key]
	if found && stack.ItemID > 0 && stack.ItemID != reward.ItemID {
		return ChronicleRefineReward{}, errChronicleFailureRewardUnavailable
	}
	// 账号共享晶石/灵魂行是账号金库这个容器的一部分：封顶按"这容器里该道具总量"
	// 算，不能只看目标格那一行；容器里已经有该道具的行时也不能再开第二行。
	space := dnfrepo.ContainerItemSpace(account.Slots, dnfrepo.MainInventoryListType, reward.ItemID, reward.StackLimit)
	if reward.StackLimit > 0 && count > space {
		return ChronicleRefineReward{}, ErrRandomRewardInventoryFull
	}
	if rowKey, _, exists := dnfrepo.ContainerItemRow(account.Slots, dnfrepo.MainInventoryListType, reward.ItemID); exists && rowKey != key {
		return ChronicleRefineReward{}, ErrRandomRewardInventoryFull
	}
	if !found || stack.ItemID <= 0 {
		stack = dnfrepo.ItemStack{
			ItemID: reward.ItemID,
			Bind:   reward.Seal,
			Extra: map[string]string{
				"item_kind": "stackable",
				"pvf_path":  reward.PVFPath,
				"source":    "chronicle_refine_failure",
			},
		}
		if reward.StackLimit > 0 {
			stack.Extra["stack_limit"] = strconv.FormatInt(reward.StackLimit, 10)
		}
	}
	stack = cloneStack(stack)
	stack.Count += count
	updateStackRawAmount(&stack)
	account.Slots[key] = stack
	return ChronicleRefineReward{
		SlotIndex:    slot,
		ItemID:       reward.ItemID,
		Count:        count,
		UpdatedStack: cloneStack(stack),
	}, nil
}

func parseChronicleOptions(raw []byte) ([]chronicleOption, error) {
	if len(raw) != currentItemListEntrySize {
		return nil, fmt.Errorf("chronicle target raw length=%d want=%d", len(raw), currentItemListEntrySize)
	}
	count := int(raw[chronicleOptionVectorOffset])
	if count > chronicleOptionCapacity {
		return nil, fmt.Errorf("chronicle option count=%d", count)
	}
	options := make([]chronicleOption, 0, count)
	for index := 0; index < count; index++ {
		optionID := int32(binary.LittleEndian.Uint32(raw[0x28+index*4 : 0x2c+index*4]))
		if optionID < 1254 || optionID > 1256 {
			return nil, fmt.Errorf("chronicle canonical option id=%d unsupported", optionID)
		}
		options = append(options, chronicleOption{
			CanonicalAuraOptionID: optionID,
			CharacterJob:          raw[0x30+index],
			FirstGrowType:         raw[0x32+index],
			EquipmentType:         raw[0x34+index],
			OptionNo:              raw[0x36+index],
		})
	}
	return options, nil
}

func setChronicleOptions(stack *dnfrepo.ItemStack, options []chronicleOption) {
	if stack == nil || len(stack.RawEntry) != currentItemListEntrySize || len(options) > chronicleOptionCapacity {
		return
	}
	stack.RawEntry = append([]byte(nil), stack.RawEntry...)
	stack.Extra = cloneStringMapForForging(stack.Extra)
	vector := stack.RawEntry[chronicleOptionVectorOffset : chronicleOptionVectorOffset+chronicleOptionVectorSize]
	clear(vector)
	vector[0] = byte(len(options))
	for index, option := range options {
		binary.LittleEndian.PutUint32(vector[1+index*4:5+index*4], uint32(option.CanonicalAuraOptionID))
		vector[9+index] = option.CharacterJob
		vector[11+index] = option.FirstGrowType
		vector[13+index] = option.EquipmentType
		vector[15+index] = option.OptionNo
	}
	stack.Extra["middle_data_1a"] = fmt.Sprintf("%x", vector)
}
