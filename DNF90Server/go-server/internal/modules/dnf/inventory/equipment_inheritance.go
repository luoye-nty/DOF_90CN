package inventory

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

const (
	currentEquipmentInheritanceCouponItemID int64 = 202611001
	equipmentInheritanceSourceSlot          int16 = 9
	equipmentInheritanceVectorOffset              = 0x3C
	equipmentInheritanceEmblemBytes               = 9
	equipmentInheritanceLegacyOffset              = 0x2F
	equipmentInheritanceTailBytes                 = 37
)

const (
	equipmentInheritanceMissingSourceText = "背包装备栏第一格没有装备"
	equipmentInheritanceMissingTargetText = "对应穿戴部位没有装备"
	equipmentInheritanceForbiddenText     = "该装备不允许参与继承"
	equipmentInheritanceRarityLevelText   = "仅60级且神器以上稀有度装备可以参与继承"
	equipmentInheritanceLevelRangeText    = "装备继承区间为前后5级"
	equipmentInheritanceNoBuildText       = "装备没有可以继承的打造属性"
	equipmentInheritanceMaterialText      = "继承消耗材料不足"
)

var equipmentInheritanceTargetSlots = map[string]int16{
	"[weapon]":      12,
	"[title name]":  13,
	"[coat]":        14,
	"[shoulder]":    15,
	"[pants]":       16,
	"[shoes]":       17,
	"[waist]":       18,
	"[amulet]":      19,
	"[wrist]":       20,
	"[ring]":        21,
	"[support]":     22,
	"[magic stone]": 23,
	"[earring]":     25,
}

var equipmentInheritanceEmblemKeys = []string{
	"equipment_emblem_data",
	"equipment_socket_data",
	"current_equipment_emblem_data",
	"current_equipment_socket_data",
}

var equipmentInheritanceTailKeys = []string{
	"tail_data_2f",
	"tailData2F",
	"tail2f",
	"raw_data_2f",
}

type equipmentInheritanceBuild struct {
	reinforce      byte
	amplifyType    byte
	amplifyValue   uint16
	forging        byte
	enchantCard    int64
	enchantUpgrade byte
	emblems        [equipmentInheritanceEmblemBytes]byte
	socketType     byte
}

type equipmentInheritanceMaterialAllocation struct {
	slot  int16
	count int64
}

type equipmentInheritanceMaterialPlan struct {
	itemID      int64
	count       int64
	allocations []equipmentInheritanceMaterialAllocation
}

// useEquipmentInheritance moves the complete supported crafted-build snapshot
// from main-inventory slot 9 to the matching worn equipment slot. The coupon
// itself is not consumed; configured materials and gold are committed in the
// same character-assets transaction as both equipment rows.
func (o *Owner) useEquipmentInheritance(
	ctx context.Context,
	cmd Command,
	resolution alignedcmd.StackableUseResolution,
) (UseStackableResult, error) {
	inheritance := resolution.EquipmentInheritance
	if o == nil || o.repo == nil || o.equipment == nil || o.characters == nil ||
		o.assets == nil || inheritance == nil || inheritance.ResolveEquipment == nil ||
		cmd.SourceListType != listTypeMain || cmd.SourceSlotIndex < 0 ||
		cmd.SourceInstanceValue == 0 || cmd.ItemTemplateID <= 0 ||
		resolution.ItemID != int64(cmd.ItemTemplateID) ||
		resolution.PVFPath == "" ||
		!equipmentInheritanceCouponTypeAllowed(resolution.StackableType) {
		return UseStackableResult{}, ErrEquipmentInheritanceContract
	}
	characterID := strconv.FormatUint(uint64(cmd.SelectedCharacterID), 10)
	if characterID == "0" {
		return UseStackableResult{}, ErrCharacterRequired
	}

	var result UseStackableResult
	err := o.withinCharacterAssetTransaction(ctx, characterID, func(tx *Owner) error {
		now := time.Now().UTC()
		if tx.now != nil {
			now = tx.now().UTC()
		}
		inventory, found, err := tx.repo.Load(ctx, characterID)
		if err != nil {
			return err
		}
		if !found {
			return ErrInventoryNotFound
		}
		equipmentRecord, found, err := tx.equipment.Load(ctx, characterID)
		if err != nil {
			return err
		}
		if !found {
			return ErrEquipmentInheritanceContract
		}
		character, found, err := tx.characters.Load(ctx, characterID)
		if err != nil {
			return err
		}
		if !found || strings.TrimSpace(character.CharacterID) != characterID {
			return ErrCharacterRequired
		}
		inventory = dnfrepo.CloneInventory(inventory)
		equipmentRecord = dnfrepo.CloneEquipment(equipmentRecord)
		character = dnfrepo.CloneCharacter(character)

		couponKey := slotKey(cmd.SourceListType, cmd.SourceSlotIndex)
		coupon, found := inventory.Slots[couponKey]
		if !found || coupon.Count <= 0 || coupon.ItemID != resolution.ItemID {
			return fmt.Errorf("%w: coupon list=%d slot=%d", ErrEquipmentInheritanceContract, cmd.SourceListType, cmd.SourceSlotIndex)
		}
		result = equipmentInheritanceBaseResult(ctx, cmd, coupon, resolution)
		result.EquipmentInheritanceGoldBefore = character.Stats["gold"]

		sourceKey := slotKey(listTypeMain, equipmentInheritanceSourceSlot)
		source, found := inventory.Slots[sourceKey]
		if !found || source.ItemID <= 0 {
			result.EquipmentInheritanceRejected = true
			result.EquipmentInheritanceMessage = equipmentInheritanceMissingSourceText
			return nil
		}
		if source.Count != 1 {
			result.EquipmentInheritanceRejected = true
			result.EquipmentInheritanceMessage = equipmentInheritanceForbiddenText
			return nil
		}

		sourceMeta, err := inheritance.ResolveEquipment(source.ItemID)
		if err != nil {
			return fmt.Errorf("%w: resolve source item=%d: %v", ErrEquipmentInheritanceContract, source.ItemID, err)
		}
		sourceType := normalizeEquipmentInheritanceType(sourceMeta.EquipmentType)
		targetSlot, allowed := equipmentInheritanceTargetSlots[sourceType]
		if !allowed {
			result.EquipmentInheritanceRejected = true
			result.EquipmentInheritanceMessage = equipmentInheritanceForbiddenText
			return nil
		}
		result.EquipmentInheritanceTargetSlot = targetSlot

		targetKey := strconv.Itoa(int(targetSlot))
		targetEntry, found := equipmentRecord.Entries[targetKey]
		if !found || targetEntry.ItemID <= 0 {
			result.EquipmentInheritanceRejected = true
			result.EquipmentInheritanceMessage = equipmentInheritanceMissingTargetText
			return nil
		}
		target := forgingStackFromEquipmentEntry(targetEntry)
		if target.ItemID <= 0 || !equipmentStackCountIsSingle(target) {
			result.EquipmentInheritanceRejected = true
			result.EquipmentInheritanceMessage = equipmentInheritanceForbiddenText
			return nil
		}
		targetMeta, err := inheritance.ResolveEquipment(target.ItemID)
		if err != nil {
			return fmt.Errorf("%w: resolve target item=%d: %v", ErrEquipmentInheritanceContract, target.ItemID, err)
		}
		targetType := normalizeEquipmentInheritanceType(targetMeta.EquipmentType)
		if targetType != sourceType ||
			equipmentInheritanceExcluded(inheritance, source.ItemID, target.ItemID) ||
			sourceMeta.PeriodEquipment || targetMeta.PeriodEquipment ||
			isEquipmentLocked(source) || isEquipmentLocked(target) ||
			equipmentInheritanceExpired(source, now) || equipmentInheritanceExpired(target, now) ||
			equipmentInheritanceRental(source) || equipmentInheritanceRental(target) {
			result.EquipmentInheritanceRejected = true
			result.EquipmentInheritanceMessage = equipmentInheritanceForbiddenText
			return nil
		}
		if sourceMeta.Rarity < 3 || targetMeta.Rarity < 3 ||
			sourceMeta.MinimumLevel < 60 || targetMeta.MinimumLevel < 60 {
			result.EquipmentInheritanceRejected = true
			result.EquipmentInheritanceMessage = equipmentInheritanceRarityLevelText
			return nil
		}
		if absInt64(sourceMeta.MinimumLevel-targetMeta.MinimumLevel) > 5 {
			result.EquipmentInheritanceRejected = true
			result.EquipmentInheritanceMessage = equipmentInheritanceLevelRangeText
			return nil
		}
		if len(source.RawEntry) != currentItemListEntrySize || len(target.RawEntry) != currentItemListEntrySize {
			return fmt.Errorf("%w: source_raw=%d target_raw=%d", ErrEquipmentInheritanceContract, len(source.RawEntry), len(target.RawEntry))
		}

		build, err := equipmentInheritanceBuildOf(source, targetSlot)
		if err != nil {
			return err
		}
		if !build.hasAny() {
			result.EquipmentInheritanceRejected = true
			result.EquipmentInheritanceMessage = equipmentInheritanceNoBuildText
			return nil
		}
		materialPlans, goldCost, ok := equipmentInheritanceMaterialPlans(inventory, character, inheritance.Materials, now)
		if !ok {
			result.EquipmentInheritanceRejected = true
			result.EquipmentInheritanceMessage = equipmentInheritanceMaterialText
			return nil
		}

		source = cloneStack(source)
		applyEquipmentInheritanceBuild(&source, targetSlot, equipmentInheritanceBuild{})
		applyEquipmentInheritanceBuild(&target, targetSlot, build)
		inventory.Slots[sourceKey] = source

		changedSlots := map[int16]struct{}{equipmentInheritanceSourceSlot: {}}
		for _, plan := range materialPlans {
			for _, allocation := range plan.allocations {
				key := slotKey(listTypeMain, allocation.slot)
				stack := inventory.Slots[key]
				stack = cloneStack(stack)
				stack.Count -= allocation.count
				if stack.Count <= 0 {
					delete(inventory.Slots, key)
				} else {
					updateStackRawAmount(&stack)
					inventory.Slots[key] = stack
				}
				changedSlots[allocation.slot] = struct{}{}
			}
		}
		if goldCost > 0 {
			character.Stats["gold"] -= goldCost
		}

		targetEntry.RawEntry = append([]byte(nil), target.RawEntry...)
		targetEntry.Extra = cloneStack(target).Extra
		equipmentRecord.Entries[targetKey] = targetEntry
		equipmentRecord.UpdatedAt = now
		inventory.UpdatedAt = now

		if err := dnfrepo.SaveInventoryFields(ctx, tx.repo, inventory, dnfrepo.InventoryFieldSlots); err != nil {
			return err
		}
		if err := dnfrepo.SaveEquipmentFields(ctx, tx.equipment, equipmentRecord, dnfrepo.EquipmentFieldEntries); err != nil {
			return err
		}
		if goldCost > 0 {
			character.UpdatedAt = now
			if err := dnfrepo.SaveCharacterFields(ctx, tx.characters, character, dnfrepo.CharacterFieldStats); err != nil {
				return err
			}
		}

		slotList := make([]int16, 0, len(changedSlots))
		for slot := range changedSlots {
			slotList = append(slotList, slot)
		}
		sort.Slice(slotList, func(i, j int) bool { return slotList[i] < slotList[j] })
		result.EquipmentInheritanceApplied = true
		result.EquipmentInheritanceChangedSlots = slotList
		result.EquipmentInheritanceGoldAfter = character.Stats["gold"]
		result.EquipmentInheritanceGoldChanged = goldCost > 0
		result.EquipmentInheritanceSourceSlot = equipmentInheritanceSourceSlot
		return nil
	})
	if err != nil {
		return UseStackableResult{}, err
	}
	return result, nil
}

// Current-client direct-use effect items are [only effect]. Keep [etc] and
// [waste] accepted so an already-imported legacy row can still be used while
// the active PVF is being rolled forward.
func equipmentInheritanceCouponTypeAllowed(stackableType string) bool {
	stackableType = strings.ToLower(strings.TrimSpace(stackableType))
	return strings.HasPrefix(stackableType, "[only effect]") ||
		strings.HasPrefix(stackableType, "[etc]") ||
		strings.HasPrefix(stackableType, "[waste]")
}

// TryUseEquipmentInheritanceSlot recognises the coupon from the durable source
// slot before an opcode-specific consumable handler runs. It is used by op515
// actions and the class1/type251 [only effect] route. A non-coupon slot returns
// matched=false so every existing item keeps its original behaviour.
func (o *Owner) TryUseEquipmentInheritanceSlot(
	ctx context.Context,
	cmd Command,
	resolver alignedcmd.StackableUseResolver,
) (UseStackableResult, bool, error) {
	if o == nil || o.repo == nil || resolver == nil ||
		cmd.SourceListType != listTypeMain || cmd.SourceSlotIndex < 0 ||
		cmd.SelectedCharacterID == 0 {
		return UseStackableResult{}, false, nil
	}
	characterID := strconv.FormatUint(uint64(cmd.SelectedCharacterID), 10)
	inventory, found, err := o.repo.Load(ctx, characterID)
	if err != nil {
		return UseStackableResult{}, true, err
	}
	if !found {
		return UseStackableResult{}, false, nil
	}
	coupon, found := inventory.Slots[slotKey(cmd.SourceListType, cmd.SourceSlotIndex)]
	if !found || coupon.Count <= 0 || coupon.ItemID != currentEquipmentInheritanceCouponItemID {
		return UseStackableResult{}, false, nil
	}
	if cmd.ItemTemplateID != 0 && int64(cmd.ItemTemplateID) != coupon.ItemID {
		return UseStackableResult{}, true, ErrEquipmentInheritanceContract
	}
	resolution, err := resolver(coupon.ItemID)
	if err != nil {
		return UseStackableResult{}, true, err
	}
	if resolution.ItemID != coupon.ItemID || resolution.EquipmentInheritance == nil {
		return UseStackableResult{}, true, ErrEquipmentInheritanceContract
	}
	cmd.ItemTemplateID = int32(coupon.ItemID)
	cmd.SourceInstanceValue = equipmentInheritanceCouponInstanceValue(coupon)
	result, err := o.useEquipmentInheritance(ctx, cmd, resolution)
	return result, true, err
}

func equipmentInheritanceCouponInstanceValue(coupon dnfrepo.ItemStack) int32 {
	if len(coupon.RawEntry) == currentItemListEntrySize {
		if instance := binary.LittleEndian.Uint32(coupon.RawEntry[0x0E:0x12]); instance != 0 {
			return int32(instance)
		}
	}
	return clampInt32(commonItemListStackValueA(coupon))
}

func equipmentInheritanceBaseResult(
	_ context.Context,
	cmd Command,
	coupon dnfrepo.ItemStack,
	resolution alignedcmd.StackableUseResolution,
) UseStackableResult {
	return UseStackableResult{
		CharacterID:                     strconv.FormatUint(uint64(cmd.SelectedCharacterID), 10),
		AccountID:                       cmd.AccountID,
		ListType:                        cmd.SourceListType,
		SlotIndex:                       cmd.SourceSlotIndex,
		InstanceValue:                   cmd.SourceInstanceValue,
		ItemID:                          coupon.ItemID,
		RemainingCount:                  coupon.Count,
		PVFPath:                         resolution.PVFPath,
		StackableType:                   resolution.StackableType,
		EquipmentInheritanceSourceSlot:  equipmentInheritanceSourceSlot,
		EquipmentInheritanceTargetSlot:  0,
		EquipmentInheritanceGoldBefore:  0,
		EquipmentInheritanceGoldAfter:   0,
		EquipmentInheritanceGoldChanged: false,
	}
}

func normalizeEquipmentInheritanceType(value string) string {
	value = strings.ToLower(strings.TrimSpace(strings.ReplaceAll(value, "`", "")))
	value = strings.ReplaceAll(value, "_", " ")
	for strings.Contains(value, "  ") {
		value = strings.ReplaceAll(value, "  ", " ")
	}
	return value
}

func equipmentInheritanceExcluded(
	resolution *alignedcmd.EquipmentInheritanceResolution,
	sourceID, targetID int64,
) bool {
	for _, excluded := range resolution.ExcludedEquipmentIDs {
		if excluded == sourceID || excluded == targetID {
			return true
		}
	}
	return false
}

func equipmentInheritanceExpired(stack dnfrepo.ItemStack, now time.Time) bool {
	return !stack.ExpireAt.IsZero() && !stack.ExpireAt.After(now)
}

func equipmentInheritanceRental(stack dnfrepo.ItemStack) bool {
	return strings.EqualFold(strings.TrimSpace(stack.Extra["source"]), "pvf_rental")
}

func absInt64(value int64) int64 {
	if value < 0 {
		return -value
	}
	return value
}

func equipmentInheritanceBuildOf(
	source dnfrepo.ItemStack,
	targetSlot int16,
) (equipmentInheritanceBuild, error) {
	amplifyType, amplifyValue := upgradeAmplifyState(source)
	if amplifyValue < 0 || amplifyValue > math.MaxUint16 {
		return equipmentInheritanceBuild{}, fmt.Errorf("%w: amplify value=%d", ErrEquipmentInheritanceContract, amplifyValue)
	}
	cardID, enchantUpgrade := equipmentInheritanceEnchantState(source)
	return equipmentInheritanceBuild{
		reinforce:      upgradeLevelOf(source),
		amplifyType:    amplifyType,
		amplifyValue:   uint16(amplifyValue),
		forging:        forgingLevelOf(source),
		enchantCard:    cardID,
		enchantUpgrade: enchantUpgrade,
		emblems:        equipmentInheritanceEmblemData(source),
		socketType:     equipmentInheritanceSocketType(source, targetSlot),
	}, nil
}

func (build equipmentInheritanceBuild) hasAny() bool {
	return build.reinforce > 0 ||
		build.amplifyType != 0 ||
		build.amplifyValue != 0 ||
		build.forging > 0 ||
		build.enchantCard > 0 ||
		build.enchantUpgrade > 0 ||
		build.emblems[0] > 0
}

func applyEquipmentInheritanceBuild(
	stack *dnfrepo.ItemStack,
	targetSlot int16,
	build equipmentInheritanceBuild,
) {
	if stack == nil {
		return
	}
	setUpgradeLevel(stack, build.reinforce)
	setAmplifyState(stack, build.amplifyType, build.amplifyValue)
	setForgingLevel(stack, build.forging)
	setEnchantFields(stack, build.enchantCard, build.enchantUpgrade)
	socketType := build.socketType
	if build.emblems[0] == 0 {
		socketType = 0
	} else if socketType == 0 {
		socketType = equipmentInheritanceSocketTypeForSlot(targetSlot)
	}
	equipmentInheritanceApplyEmblems(stack, build.emblems, socketType)
}

func equipmentInheritanceEnchantState(stack dnfrepo.ItemStack) (int64, byte) {
	cardID := firstExtraInt(stack.Extra, 0, "enchant_card_id", "value_a")
	if cardID == 0 && len(stack.RawEntry) >= 0x12 {
		cardID = int64(uint32(stack.RawEntry[0x0E]) |
			uint32(stack.RawEntry[0x0F])<<8 |
			uint32(stack.RawEntry[0x10])<<16 |
			uint32(stack.RawEntry[0x11])<<24)
	}
	return cardID, enchantUpgradeCountOf(stack)
}

func equipmentInheritanceSocketType(stack dnfrepo.ItemStack, targetSlot int16) byte {
	for _, key := range []string{"equipment_socket_type", "equipment_jewel_socket_type", "jewel_socket_type"} {
		value, err := strconv.ParseInt(strings.TrimSpace(stack.Extra[key]), 10, 16)
		if err == nil && value > 0 && value <= math.MaxUint8 {
			return byte(value)
		}
	}
	if len(stack.RawEntry) >= equipmentInheritanceVectorOffset+1 &&
		stack.RawEntry[equipmentInheritanceVectorOffset] > 0 {
		return equipmentInheritanceSocketTypeForSlot(targetSlot)
	}
	return 0
}

func equipmentInheritanceSocketTypeForSlot(slot int16) byte {
	switch slot {
	case 14, 16:
		return 0x04
	case 15, 19:
		return 0x02
	case 18, 21:
		return 0x01
	case 17, 20:
		return 0x08
	default:
		return 0x10
	}
}

func equipmentInheritanceEmblemData(stack dnfrepo.ItemStack) [equipmentInheritanceEmblemBytes]byte {
	var best [equipmentInheritanceEmblemBytes]byte
	bestScore := -1
	consider := func(data [equipmentInheritanceEmblemBytes]byte) {
		score := equipmentInheritanceEmblemScore(data)
		if score > bestScore {
			best, bestScore = data, score
		}
	}
	for _, key := range equipmentInheritanceEmblemKeys {
		if data, ok := equipmentInheritanceDecodeEmblem(stack.Extra[key]); ok {
			consider(data)
		}
	}
	if data, ok := equipmentInheritanceCurrentRawEmblems(stack.RawEntry); ok {
		consider(data)
	}
	if tail, ok := equipmentInheritanceTailData(stack.Extra, stack.RawEntry); ok {
		var data [equipmentInheritanceEmblemBytes]byte
		copy(data[:], tail[:len(data)])
		consider(data)
	}
	if data, ok := equipmentInheritanceLegacyRawEmblems(stack.RawEntry); ok {
		consider(data)
	}
	return best
}

func equipmentInheritanceDecodeEmblem(value string) ([equipmentInheritanceEmblemBytes]byte, bool) {
	var data [equipmentInheritanceEmblemBytes]byte
	value = strings.TrimSpace(value)
	if value == "" {
		return data, false
	}
	value = strings.NewReplacer(" ", "", "\t", "", "\r", "", "\n", "", "0x", "", "0X", "").Replace(value)
	decoded, err := hex.DecodeString(value)
	if err != nil || len(decoded) < len(data) {
		return data, false
	}
	copy(data[:], decoded[:len(data)])
	return data, equipmentInheritanceEmblemScore(data) >= 0
}

func equipmentInheritanceCurrentRawEmblems(raw []byte) ([equipmentInheritanceEmblemBytes]byte, bool) {
	var data [equipmentInheritanceEmblemBytes]byte
	if len(raw) < equipmentInheritanceVectorOffset+equipmentInheritanceEmblemBytes {
		return data, false
	}
	count := int(raw[equipmentInheritanceVectorOffset])
	if count > 2 {
		return data, false
	}
	data[0] = byte(count)
	if count > 0 {
		copy(data[1:5], raw[equipmentInheritanceVectorOffset+1:equipmentInheritanceVectorOffset+5])
	}
	if count > 1 {
		copy(data[5:9], raw[equipmentInheritanceVectorOffset+5:equipmentInheritanceVectorOffset+9])
	}
	return data, true
}

func equipmentInheritanceLegacyRawEmblems(raw []byte) ([equipmentInheritanceEmblemBytes]byte, bool) {
	var data [equipmentInheritanceEmblemBytes]byte
	if len(raw) < equipmentInheritanceVectorOffset+1 ||
		raw[equipmentInheritanceVectorOffset] > 0 ||
		len(raw) < equipmentInheritanceLegacyOffset+equipmentInheritanceEmblemBytes {
		return data, false
	}
	copy(data[:], raw[equipmentInheritanceLegacyOffset:equipmentInheritanceLegacyOffset+equipmentInheritanceEmblemBytes])
	return data, equipmentInheritanceEmblemScore(data) >= 0
}

func equipmentInheritanceEmblemScore(data [equipmentInheritanceEmblemBytes]byte) int {
	if data[0] > 2 {
		return -1
	}
	score := 0
	if data[0] > 0 {
		score += 10 + int(data[0])
	}
	if data[0] > 0 && binary.LittleEndian.Uint32(data[1:5]) != 0 {
		score += 10
	}
	if data[0] > 1 && binary.LittleEndian.Uint32(data[5:9]) != 0 {
		score += 10
	}
	return score
}

func equipmentInheritanceTailData(extra map[string]string, raw []byte) ([]byte, bool) {
	for _, key := range equipmentInheritanceTailKeys {
		value := strings.TrimSpace(extra[key])
		if value == "" {
			continue
		}
		value = strings.NewReplacer(" ", "", "\t", "", "\r", "", "\n", "", "0x", "", "0X", "").Replace(value)
		decoded, err := hex.DecodeString(value)
		if err == nil && len(decoded) >= equipmentInheritanceTailBytes {
			return append([]byte(nil), decoded[:equipmentInheritanceTailBytes]...), true
		}
	}
	if len(raw) >= equipmentInheritanceLegacyOffset+equipmentInheritanceTailBytes {
		return append([]byte(nil), raw[equipmentInheritanceLegacyOffset:equipmentInheritanceLegacyOffset+equipmentInheritanceTailBytes]...), true
	}
	return nil, false
}

func equipmentInheritanceApplyEmblems(
	stack *dnfrepo.ItemStack,
	data [equipmentInheritanceEmblemBytes]byte,
	socketType byte,
) {
	if stack == nil {
		return
	}
	if stack.Extra == nil {
		stack.Extra = make(map[string]string)
	}
	for _, key := range equipmentInheritanceEmblemKeys {
		stack.Extra[key] = hex.EncodeToString(data[:])
	}
	stack.Extra["equipment_socket_type"] = strconv.Itoa(int(socketType))
	stack.Extra["equipment_jewel_socket_type"] = strconv.Itoa(int(socketType))
	if socketType == 0 {
		delete(stack.Extra, "equipment_socket_type")
		delete(stack.Extra, "equipment_jewel_socket_type")
		delete(stack.Extra, "jewel_socket_type")
	}
	tail, _ := equipmentInheritanceTailData(stack.Extra, stack.RawEntry)
	if len(tail) == 0 {
		tail = make([]byte, equipmentInheritanceTailBytes)
	}
	for index := 0; index < equipmentInheritanceVectorOffset-equipmentInheritanceLegacyOffset+equipmentInheritanceEmblemBytes; index++ {
		tail[index] = 0
	}
	copy(tail[equipmentInheritanceVectorOffset-equipmentInheritanceLegacyOffset:], data[:])
	for _, key := range equipmentInheritanceTailKeys {
		stack.Extra[key] = hex.EncodeToString(tail)
	}
	if len(stack.RawEntry) == currentItemListEntrySize {
		stack.RawEntry = append([]byte(nil), stack.RawEntry...)
		copy(stack.RawEntry[equipmentInheritanceVectorOffset:equipmentInheritanceVectorOffset+equipmentInheritanceEmblemBytes], data[:])
		copy(stack.RawEntry[equipmentInheritanceLegacyOffset:equipmentInheritanceVectorOffset], make([]byte, equipmentInheritanceVectorOffset-equipmentInheritanceLegacyOffset))
	}
}

func equipmentInheritanceMaterialPlans(
	inventory dnfrepo.InventoryRecord,
	character dnfrepo.CharacterRecord,
	materials []alignedcmd.EquipmentInheritanceMaterial,
	now time.Time,
) ([]equipmentInheritanceMaterialPlan, int64, bool) {
	requirements := make(map[int64]int64, len(materials))
	for _, material := range materials {
		if material.ItemID < 0 || material.Count <= 0 || requirements[material.ItemID] > math.MaxInt64-material.Count {
			return nil, 0, false
		}
		requirements[material.ItemID] += material.Count
	}
	goldCost := requirements[0]
	if goldCost < 0 || goldCost > character.Stats["gold"] {
		return nil, 0, false
	}
	ids := make([]int64, 0, len(requirements))
	for itemID := range requirements {
		if itemID > 0 {
			ids = append(ids, itemID)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	plans := make([]equipmentInheritanceMaterialPlan, 0, len(ids))
	for _, itemID := range ids {
		need := requirements[itemID]
		slots := make([]int16, 0)
		for key, stack := range inventory.Slots {
			listType, slot, ok := parseSlotKey(key)
			if !ok || listType != listTypeMain || slot == equipmentInheritanceSourceSlot ||
				dnfrepo.IsAccountSharedInventorySlot(listType, slot) ||
				stack.ItemID != itemID || stack.Count <= 0 ||
				isEquipmentLocked(stack) || equipmentInheritanceExpired(stack, now) {
				continue
			}
			slots = append(slots, slot)
		}
		sort.Slice(slots, func(i, j int) bool { return slots[i] < slots[j] })
		plan := equipmentInheritanceMaterialPlan{itemID: itemID, count: need}
		var available int64
		for _, slot := range slots {
			stack := inventory.Slots[slotKey(listTypeMain, slot)]
			if available > math.MaxInt64-stack.Count {
				available = math.MaxInt64
				break
			}
			available += stack.Count
		}
		if available < need {
			return nil, 0, false
		}
		remaining := need
		for _, slot := range slots {
			if remaining == 0 {
				break
			}
			stack := inventory.Slots[slotKey(listTypeMain, slot)]
			take := stack.Count
			if take > remaining {
				take = remaining
			}
			plan.allocations = append(plan.allocations, equipmentInheritanceMaterialAllocation{slot: slot, count: take})
			remaining -= take
		}
		plans = append(plans, plan)
	}
	return plans, goldCost, true
}
