package inventory

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"time"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

// Current NoPack error codes the 90 client's op455 result handler knows how to
// paint. 23 and 19 are the codes the reference DFO implementation returns for
// "the selected slot holds no seal option" and "this seal grade cannot be
// modified"; 17 is the family's generic "invalid material" code.
const (
	randomOptionResetErrorInvalidMaterial byte = 17
	randomOptionResetErrorGradeFixed      byte = 19
	randomOptionResetErrorNoSealOption    byte = 23
)

// currentRandomOptionResetBoxItemIDs are the current NoPack stackables that
// drive the seal-grade adjustment. 15 is 装备品级调整箱, whose PVF description
// states that placing an already unsealed equipment also adjusts the grade of
// its seal attributes between C and S.
var currentRandomOptionResetBoxItemIDs = map[int64]struct{}{
	15: {},
}

// currentRandomOptionResetMaxPercent mirrors the runtime MAX_OPTION_VALUE: the
// per-option value bytes are percentages in 0..100, so a rolled window that
// would exceed 100 is clamped.
const currentRandomOptionResetMaxPercent int64 = 100

// RandomOptionResetResult reports one op455 seal-grade adjustment. Both stacks
// are echoed back so the client repaints the equipment row and the consumed
// adjustment box row.
type RandomOptionResetResult struct {
	CharacterID       string
	Success           bool
	Failure           RandomOptionFailure
	ErrorCode         byte
	TargetSlotIndex   int16
	MaterialSlotIndex int16
	TargetItemID      int64
	MaterialItemID    int64
	OptionIndex       byte
	OptionType        byte
	OldPercent        byte
	NewPercent        byte
	Grade             int64
	MaterialConsumed  bool
	UpdatedTarget     dnfrepo.ItemStack
	UpdatedMaterial   dnfrepo.ItemStack
	MaterialSlotEmpty bool
}

// ResetRandomOption implements current NoPack class1/op455: the player fed one
// 装备品级调整箱 and the client's attribute dropdown picked which seal option to
// re-grade. The current PVF only stores a 0..100 percentage per seal option, so
// the grade is re-rolled as "weighted grade window from
// [postfix grade modification] -> uniform percentage inside that window", which
// is exactly what the reference implementation writes back through
// set_first_value/set_second_value.
func (o *Owner) ResetRandomOption(ctx context.Context, cmd Command, resolver alignedcmd.RandomOptionResolver) (RandomOptionResetResult, error) {
	base := RandomOptionResetResult{
		TargetSlotIndex:   cmd.TargetSlotIndex,
		MaterialSlotIndex: cmd.MaterialSlotIndex,
		OptionIndex:       cmd.RandomOptionIndex,
	}
	if o == nil || o.repo == nil || o.characters == nil {
		return RandomOptionResetResult{}, ErrOwnerUnavailable
	}
	if cmd.SelectedCharacterID == 0 {
		return RandomOptionResetResult{}, ErrCharacterRequired
	}
	if resolver == nil {
		return RandomOptionResetResult{}, ErrRandomOptionResolverRequired
	}
	if cmd.TargetSlotIndex < 0 || cmd.MaterialSlotIndex < 0 {
		base.Failure = RandomOptionFailureMaterialMissing
		base.ErrorCode = randomOptionResetErrorInvalidMaterial
		return base, nil
	}

	characterID := strconv.FormatUint(uint64(cmd.SelectedCharacterID), 10)
	base.CharacterID = characterID
	if !o.inItemTransaction {
		var result RandomOptionResetResult
		err := o.withinCharacterAssetTransaction(ctx, characterID, func(txOwner *Owner) error {
			var innerErr error
			result, innerErr = txOwner.ResetRandomOption(ctx, cmd, resolver)
			return innerErr
		})
		if err != nil {
			return RandomOptionResetResult{}, err
		}
		if result.CharacterID == "" {
			result = base
		}
		return result, nil
	}

	record, found, err := o.repo.Load(ctx, characterID)
	if err != nil {
		return RandomOptionResetResult{}, err
	}
	if !found {
		return RandomOptionResetResult{}, ErrInventoryNotFound
	}
	record = dnfrepo.CloneInventory(record)
	record.CharacterID = characterID
	if record.Slots == nil {
		record.Slots = make(map[string]dnfrepo.ItemStack)
	}

	result := base
	targetKey := slotKey(listTypeMain, cmd.TargetSlotIndex)
	target, found := record.Slots[targetKey]
	if !found || target.Count <= 0 || target.ItemID <= 0 || isEquipmentLocked(target) {
		result.Failure = RandomOptionFailureNoSealOption
		result.ErrorCode = randomOptionResetErrorNoSealOption
		return result, nil
	}
	result.TargetItemID = target.ItemID

	materialKey := slotKey(listTypeMain, cmd.MaterialSlotIndex)
	if cmd.MaterialSlotIndex == cmd.TargetSlotIndex {
		result.Failure = RandomOptionFailureMaterialMissing
		result.ErrorCode = randomOptionResetErrorInvalidMaterial
		return result, nil
	}
	material, found := record.Slots[materialKey]
	if !found || material.Count <= 0 || material.ItemID <= 0 {
		result.Failure = RandomOptionFailureMaterialMissing
		result.ErrorCode = randomOptionResetErrorInvalidMaterial
		return result, nil
	}
	result.MaterialItemID = material.ItemID
	if _, ok := currentRandomOptionResetBoxItemIDs[material.ItemID]; !ok {
		result.Failure = RandomOptionFailureMaterialMissing
		result.ErrorCode = randomOptionResetErrorInvalidMaterial
		return result, nil
	}
	if isEquipmentLocked(material) {
		result.Failure = RandomOptionFailureMaterialLocked
		result.ErrorCode = randomOptionResetErrorInvalidMaterial
		return result, nil
	}

	resolution, err := resolver(target.ItemID)
	if err != nil {
		return RandomOptionResetResult{}, err
	}
	if !resolution.Eligible || len(resolution.ResetGrades) == 0 {
		result.Failure = RandomOptionFailureGradesUnavailable
		result.ErrorCode = randomOptionResetErrorGradeFixed
		return result, nil
	}

	raw := currentRawEntryForStack(cmd.TargetSlotIndex, target)
	if len(raw) != currentItemListEntrySize {
		return RandomOptionResetResult{}, fmt.Errorf("random-option reset target row length %d, want %d", len(raw), currentItemListEntrySize)
	}
	current, valid := randomOptionsFromRaw(raw)
	if !valid || len(current) == 0 || int(cmd.RandomOptionIndex) >= len(current) {
		result.Failure = RandomOptionFailureNoSealOption
		result.ErrorCode = randomOptionResetErrorNoSealOption
		return result, nil
	}
	selected := current[cmd.RandomOptionIndex]
	result.OptionType = selected.Type
	result.OldPercent = selected.Value1
	if _, blocked := resolution.GradeUnmodifiable[selected.Type]; blocked {
		result.Failure = RandomOptionFailureGradeFixed
		result.ErrorCode = randomOptionResetErrorGradeFixed
		return result, nil
	}
	if _, blocked := resolution.GradeGradeless[selected.Type]; blocked {
		result.Failure = RandomOptionFailureGradeFixed
		result.ErrorCode = randomOptionResetErrorGradeFixed
		return result, nil
	}

	newPercent, grade, err := rollCurrentRandomOptionGradePercent(resolution.ResetGrades)
	if err != nil {
		return RandomOptionResetResult{}, err
	}

	next := append([]RandomOptionValue(nil), current...)
	next[cmd.RandomOptionIndex] = RandomOptionValue{Type: selected.Type, Value1: newPercent, Value2: newPercent}

	target = cloneStack(target)
	setRandomOptions(&target, cmd.TargetSlotIndex, next)
	record.Slots[targetKey] = target

	material = cloneStack(material)
	material.Count--
	if material.Count <= 0 {
		delete(record.Slots, materialKey)
		result.MaterialSlotEmpty = true
		result.UpdatedMaterial = dnfrepo.ItemStack{}
	} else {
		updateStackRawAmount(&material)
		record.Slots[materialKey] = material
		result.UpdatedMaterial = cloneStack(material)
	}
	record.UpdatedAt = time.Now()
	if err := dnfrepo.SaveInventoryFields(ctx, o.repo, record, dnfrepo.InventoryFieldSlots); err != nil {
		return RandomOptionResetResult{}, err
	}

	result.Success = true
	result.MaterialConsumed = true
	result.NewPercent = newPercent
	result.Grade = grade
	result.UpdatedTarget = cloneStack(target)
	return result, nil
}

// rollCurrentRandomOptionGradePercent picks one seal grade by its PVF weight and
// then a uniform percentage inside that grade's window. The current PVF grades
// are C/B/A/S with weights 330/520/100/50 out of 1000.
func rollCurrentRandomOptionGradePercent(bands []alignedcmd.RandomOptionGradeBand) (byte, int64, error) {
	total := int64(0)
	for _, band := range bands {
		if band.Weight <= 0 || band.MaxPct < band.MinPct {
			continue
		}
		if band.Weight > math.MaxInt64-total {
			return 0, 0, fmt.Errorf("random-option grade weights overflow")
		}
		total += band.Weight
	}
	if total <= 0 {
		return 0, 0, fmt.Errorf("random-option grade windows unavailable")
	}
	roll, err := secureRandomOptionInt(total)
	if err != nil {
		return 0, 0, err
	}
	var selected alignedcmd.RandomOptionGradeBand
	found := false
	for _, band := range bands {
		if band.Weight <= 0 || band.MaxPct < band.MinPct {
			continue
		}
		if roll < band.Weight {
			selected = band
			found = true
			break
		}
		roll -= band.Weight
	}
	if !found {
		return 0, 0, fmt.Errorf("random-option grade selection exhausted")
	}
	span := selected.MaxPct - selected.MinPct
	offset := int64(0)
	if span > 0 {
		offset, err = secureRandomOptionInt(span + 1)
		if err != nil {
			return 0, 0, err
		}
	}
	percent := selected.MinPct + offset
	if percent > currentRandomOptionResetMaxPercent {
		percent = currentRandomOptionResetMaxPercent
	}
	if percent < 0 {
		percent = 0
	}
	return byte(percent), selected.Grade, nil
}
