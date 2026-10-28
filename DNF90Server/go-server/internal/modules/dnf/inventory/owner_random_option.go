package inventory

import (
	"context"
	cryptorand "crypto/rand"
	"errors"
	"fmt"
	"math"
	"math/big"
	"strconv"
	"strings"
	"time"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

var ErrRandomOptionResolverRequired = errors.New("random-option mutation requires a runtime-PVF resolver")

const (
	randomOptionCountOffset        = 0x47
	randomOptionTypeOffset         = 0x48
	randomOptionValue1Offset       = 0x4B
	randomOptionValue2Offset       = 0x4E
	randomOptionStateOffset        = 0x51
	randomOptionChangedIndexOffset = 0x52
	randomOptionCandidateOffset    = 0x53
	randomOptionMaximumCount       = 3
)

type RandomOptionValue struct {
	Type   byte
	Value1 byte
	Value2 byte
}

type RandomOptionFailure string

const (
	RandomOptionFailureNone             RandomOptionFailure = ""
	RandomOptionFailureInsufficientGold RandomOptionFailure = "insufficient_gold"
	// op455 (seal-grade adjustment) outcomes; each maps to the current
	// NoPack error code the client's op455 result handler can render.
	RandomOptionFailureNoSealOption      RandomOptionFailure = "no_seal_option"
	RandomOptionFailureGradeFixed        RandomOptionFailure = "grade_fixed"
	RandomOptionFailureGradesUnavailable RandomOptionFailure = "grades_unavailable"
	RandomOptionFailureMaterialMissing   RandomOptionFailure = "material_missing"
	RandomOptionFailureMaterialLocked    RandomOptionFailure = "material_locked"
)

type RandomOptionMutationResult struct {
	CharacterID     string
	Success         bool
	Failure         RandomOptionFailure
	Mode            string
	TargetSlotIndex int16
	TargetItemID    int64
	OptionIndex     byte
	Options         []RandomOptionValue
	GoldCost        int64
	UpdatedGold     int64
	UpdatedStack    dnfrepo.ItemStack
	Changed         bool
	// PercentileFallback is true when the runtime PVF shipped no
	// [different weight] window for this rarity and the value roll had to fall
	// back to the whole 0..100 domain. It is surfaced in the bridge log so a
	// server that keeps unsealing into odd grades is diagnosable.
	PercentileFallback bool
}

// 【封印属性数值 = 品级百分位，不是绝对数值】
//
// 90 客户端 X 服同源的 df_game_r(1) 里，random_option 命名空间把这件事写死了：
//
//   - 常量 MAX_OPTION_VALUE = 100（random_option::MAX_OPTION_VALUE，1 字节）。
//   - RandomOption 结构：+0..+8 三个槽各 {type, value1, value2}；+0xA..+0xC 是本次
//     "变换候选"，+0xD 是被变换槽号。RandomOption::set_first_value / set_second_value
//     写的两个字节是一对【同值】的百分位（reset_option 里还会 `if (p > 100) p = 100`）。
//   - 展示值由客户端自己按 PVF 区间算：_get_option_value(min,max) 做的是
//     min + 量化((max-min) * 百分位)，即两个字节是"在这条属性区间里的位置"。
//
// 所以服务端写词条值时必须掷百分位；把 PVF 的 [level] 区间端点 (min,max) 直接塞进
// 这两个字节是错的 —— 那样等于把 min 当成了"min% 的位置"，品级会常年落在最低档
// （72 号属性 10 级是 (5,39)，写进去就是 5% → C 级）。
//
// 窗口来自 etc/randomoption/randomizedoptionoverall2.etc 的 [different weight]：
// 行格式 "稀有度 槽位 下限% 上限%"，当前 PVF 是
//
//	2 1 0 25   2 2 10 30   2 3 30 55      (稀有：第 1 条 0~25%，第 2 条 10~30%，第 3 条 30~55%)
//	3 1 10 35  3 2 20 40   3 3 40 65      (史诗：10~35% / 20~40% / 40~65%)
//
// 与 [postfix grade modification] 的品级区间处在同一个 0~100 百分位空间（稀有第 3
// 条上限 55 对 S 级上限 56），也就是说解封掷完之后，客户端自己就能算出 C/B/A/S。
//
// 若 PVF 没有这张表（自定义 PVF 删段），退化成在整个 0..100 里均匀掷，并把
// RandomOptionMutationResult.PercentileFallback 置位，日志里能看到。
func sealPercentileForSlot(windows []alignedcmd.RandomOptionPercentWindow, slot int64) (byte, error) {
	minPct, maxPct := int64(0), currentRandomOptionResetMaxPercent
	for _, window := range windows {
		if window.Slot != slot || window.MaxPct < window.MinPct {
			continue
		}
		minPct, maxPct = window.MinPct, window.MaxPct
		break
	}
	if maxPct > currentRandomOptionResetMaxPercent {
		maxPct = currentRandomOptionResetMaxPercent
	}
	if minPct < 0 {
		minPct = 0
	}
	if maxPct < minPct {
		maxPct = minPct
	}
	span := maxPct - minPct
	offset := int64(0)
	if span > 0 {
		var err error
		offset, err = secureRandomOptionInt(span + 1)
		if err != nil {
			return 0, err
		}
	}
	return byte(minPct + offset), nil
}

func (o *Owner) UnsealRandomOption(ctx context.Context, cmd Command, resolver alignedcmd.RandomOptionResolver) (RandomOptionMutationResult, error) {
	return o.mutateRandomOption(ctx, cmd, resolver)
}

func (o *Owner) ChangeRandomOption(ctx context.Context, cmd Command, resolver alignedcmd.RandomOptionResolver) (RandomOptionMutationResult, error) {
	return o.mutateRandomOption(ctx, cmd, resolver)
}

func (o *Owner) mutateRandomOption(ctx context.Context, cmd Command, resolver alignedcmd.RandomOptionResolver) (RandomOptionMutationResult, error) {
	base := RandomOptionMutationResult{
		Mode:            cmd.Operation,
		TargetSlotIndex: cmd.TargetSlotIndex,
		OptionIndex:     cmd.RandomOptionIndex,
	}
	if o == nil || o.repo == nil || o.characters == nil {
		return RandomOptionMutationResult{}, ErrOwnerUnavailable
	}
	if cmd.SelectedCharacterID == 0 {
		return RandomOptionMutationResult{}, ErrCharacterRequired
	}
	if resolver == nil {
		return RandomOptionMutationResult{}, ErrRandomOptionResolverRequired
	}
	if cmd.Operation != "unseal_random_option" && cmd.Operation != "change_random_option" {
		return RandomOptionMutationResult{}, fmt.Errorf("unsupported random-option operation %q", cmd.Operation)
	}
	if cmd.TargetSlotIndex < 0 {
		return base, nil
	}

	characterID := strconv.FormatUint(uint64(cmd.SelectedCharacterID), 10)
	base.CharacterID = characterID
	if !o.inItemTransaction {
		var result RandomOptionMutationResult
		err := o.withinCharacterAssetTransaction(ctx, characterID, func(txOwner *Owner) error {
			var err error
			result, err = txOwner.mutateRandomOption(ctx, cmd, resolver)
			return err
		})
		if err != nil {
			return RandomOptionMutationResult{}, err
		}
		if result.CharacterID == "" {
			result = base
		}
		return result, nil
	}

	record, found, err := o.repo.Load(ctx, characterID)
	if err != nil {
		return RandomOptionMutationResult{}, err
	}
	if !found {
		return RandomOptionMutationResult{}, ErrInventoryNotFound
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
		return result, nil
	}
	result.TargetItemID = target.ItemID

	resolution, err := resolver(target.ItemID)
	if err != nil {
		return RandomOptionMutationResult{}, err
	}
	eligible := resolution.Eligible &&
		strings.EqualFold(strings.TrimSpace(resolution.TargetKind), "equipment") &&
		strings.TrimSpace(resolution.TargetPVFPath) != "" &&
		resolution.TargetMinimumLevel >= 0 &&
		(resolution.TargetRarity == 2 || resolution.TargetRarity == 3)
	if cmd.Operation == "unseal_random_option" && !eligible {
		// 客户端右键「解封」走的就是这条 op401 —— 它是通用的撕膜命令，只有稀有 / 神器才
		// 顺带开随机属性。称号、传说、史诗这类没有随机属性的贴膜物品以前在这里直接被拒，
		// 客户端本地已经把膜撕了，服务端却一直存着 seal_flag=1：重登、进称号簿都又变回
		// "封装"。没有随机属性可开就只撕膜，不收金币。
		// （移植上游 fc3d6d2，2026-09-22）
		if !stackSealFilmSet(target) {
			return result, nil
		}
		target = cloneStack(target)
		clearStackSealFilm(&target)
		record.Slots[targetKey] = target
		record.UpdatedAt = time.Now()
		if err := dnfrepo.SaveInventoryFields(ctx, o.repo, record, dnfrepo.InventoryFieldSlots); err != nil {
			return RandomOptionMutationResult{}, err
		}
		result.Success = true
		result.Changed = true
		result.Mode = "unseal_seal_film"
		result.UpdatedStack = cloneStack(target)
		return result, nil
	}
	if !eligible {
		return result, nil
	}

	raw := currentRawEntryForStack(cmd.TargetSlotIndex, target)
	if len(raw) != currentItemListEntrySize {
		return RandomOptionMutationResult{}, fmt.Errorf("random-option target row length %d, want %d", len(raw), currentItemListEntrySize)
	}
	current, valid := randomOptionsFromRaw(raw)
	var next []RandomOptionValue
	var goldCost int64
	switch cmd.Operation {
	case "unseal_random_option":
		if valid && len(current) > 0 {
			return result, nil
		}
		quantity, err := rollRandomOptionQuantity(resolution.QuantityWeights)
		if err != nil {
			return RandomOptionMutationResult{}, err
		}
		if quantity == 0 || int(quantity) > len(resolution.InitialGroups) || quantity > randomOptionMaximumCount {
			return result, nil
		}
		next, err = rollRandomOptionSet(resolution.InitialGroups[:quantity], nil)
		if err != nil {
			return RandomOptionMutationResult{}, err
		}
		// 解封：每个词条的数值要按"槽位窗口"掷品级百分位（见文件末尾的
		// sealPercentileForSlot 注释），槽位从 1 开始。
		for index := range next {
			percent, percentErr := sealPercentileForSlot(resolution.InitialSlotWindows, int64(index)+1)
			if percentErr != nil {
				return RandomOptionMutationResult{}, percentErr
			}
			next[index].Value1 = percent
			next[index].Value2 = percent
		}
		result.PercentileFallback = len(resolution.InitialSlotWindows) == 0
		goldCost = resolution.BreakSealGoldCost
	case "change_random_option":
		if !valid || len(current) == 0 || int(cmd.RandomOptionIndex) >= len(current) || int(cmd.RandomOptionIndex) >= len(resolution.ModifiedGroups) {
			return result, nil
		}
		used := make(map[byte]struct{}, len(current)-1)
		for index, option := range current {
			if index != int(cmd.RandomOptionIndex) {
				used[option.Type] = struct{}{}
			}
		}
		candidate, err := rollRandomOptionCandidate(resolution.ModifiedGroups[cmd.RandomOptionIndex], used)
		if err != nil {
			return RandomOptionMutationResult{}, err
		}
		next = append([]RandomOptionValue(nil), current...)
		next[cmd.RandomOptionIndex] = candidate
		// 变更是"换类型 + 重掷数值"：当前 PVF 没有 [changed postfix]，所以沿用
		// [different weight] 的同槽位窗口（见文件末尾 sealPercentileForSlot）。
		changeWindows := resolution.ChangeSlotWindows
		if len(changeWindows) == 0 {
			changeWindows = resolution.InitialSlotWindows
		}
		percent, err := sealPercentileForSlot(changeWindows, int64(cmd.RandomOptionIndex)+1)
		if err != nil {
			return RandomOptionMutationResult{}, err
		}
		next[cmd.RandomOptionIndex].Value1 = percent
		next[cmd.RandomOptionIndex].Value2 = percent
		result.PercentileFallback = len(changeWindows) == 0
		goldCost = resolution.ModificationGoldCost
	}
	if goldCost < 0 {
		return RandomOptionMutationResult{}, fmt.Errorf("random-option negative PVF gold cost %d", goldCost)
	}

	character, found, err := o.characters.Load(ctx, characterID)
	if err != nil {
		return RandomOptionMutationResult{}, err
	}
	if !found {
		return RandomOptionMutationResult{}, fmt.Errorf("%w: character=%s", ErrWalletTxnRequired, characterID)
	}
	if character.Stats == nil {
		character.Stats = make(map[string]int64, 1)
	}
	result.UpdatedGold = character.Stats["gold"]
	result.GoldCost = goldCost
	if result.UpdatedGold < goldCost {
		result.Failure = RandomOptionFailureInsufficientGold
		return result, nil
	}

	target = cloneStack(target)
	setRandomOptions(&target, cmd.TargetSlotIndex, next)
	record.Slots[targetKey] = target
	character = dnfrepo.CloneCharacter(character)
	if character.Stats == nil {
		character.Stats = make(map[string]int64, 1)
	}
	character.Stats["gold"] -= goldCost
	character.UpdatedAt = time.Now()
	if err := dnfrepo.SaveCharacterFields(ctx, o.characters, character, dnfrepo.CharacterFieldStats); err != nil {
		return RandomOptionMutationResult{}, err
	}
	record.UpdatedAt = time.Now()
	if err := dnfrepo.SaveInventoryFields(ctx, o.repo, record, dnfrepo.InventoryFieldSlots); err != nil {
		return RandomOptionMutationResult{}, err
	}

	result.Success = true
	result.Options = append([]RandomOptionValue(nil), next...)
	result.UpdatedGold = character.Stats["gold"]
	result.UpdatedStack = cloneStack(target)
	result.Changed = true
	return result, nil
}

// stackSealFilmSet 判断物品当前是否贴着膜（封装）：持久 seal 别名或 0x77 行 +0x0D 非 0。
// 只看 seal 语义的键，不把 bind（绑定）当封装。
//
// 移植上游 fc3d6d2（2026-09-22）。
func stackSealFilmSet(stack dnfrepo.ItemStack) bool {
	if firstExtraInt(stack.Extra, 0, "seal_flag", "seal") != 0 {
		return true
	}
	return len(stack.RawEntry) > 0x0D && stack.RawEntry[0x0D] != 0
}

// clearStackSealFilm 撕膜：与 equip.removeSealOnFirstEquipmentUse 同一组键与同一个原始行字节。
//
// 我们 equip 侧（equip/owner.go:1215）用的是同一组键（seal_flag / seal /
// equipment_update_seal_flag / update_seal_flag）与同一个 RawEntry[0x0D]，所以两处的口径一致。
func clearStackSealFilm(stack *dnfrepo.ItemStack) {
	if stack == nil {
		return
	}
	if len(stack.RawEntry) > 0x0D {
		stack.RawEntry = append([]byte(nil), stack.RawEntry...)
		stack.RawEntry[0x0D] = 0
	}
	if stack.Extra == nil {
		stack.Extra = make(map[string]string, 4)
	}
	for _, key := range []string{"seal_flag", "seal", "equipment_update_seal_flag", "update_seal_flag"} {
		delete(stack.Extra, key)
	}
	if hexRaw := strings.TrimSpace(stack.Extra["raw_entry_hex"]); len(hexRaw) >= 0x1C {
		// 0x0D 字节在十六进制串里是第 26、27 个字符；有些路径只读这份副本。
		stack.Extra["raw_entry_hex"] = hexRaw[:0x1A] + "00" + hexRaw[0x1C:]
	}
	stack.Extra["seal_removed_by_unseal"] = "1"
}

func randomOptionsFromRaw(raw []byte) ([]RandomOptionValue, bool) {
	if len(raw) != currentItemListEntrySize {
		return nil, false
	}
	count := int(raw[randomOptionCountOffset])
	if count < 0 || count > randomOptionMaximumCount {
		return nil, false
	}
	options := make([]RandomOptionValue, 0, count)
	seen := make(map[byte]struct{}, count)
	for index := 0; index < count; index++ {
		option := RandomOptionValue{
			Type:   raw[randomOptionTypeOffset+index],
			Value1: raw[randomOptionValue1Offset+index],
			Value2: raw[randomOptionValue2Offset+index],
		}
		if option.Type == 0 {
			return nil, false
		}
		if _, duplicate := seen[option.Type]; duplicate {
			return nil, false
		}
		seen[option.Type] = struct{}{}
		options = append(options, option)
	}
	return options, true
}

func setRandomOptions(stack *dnfrepo.ItemStack, slot int16, options []RandomOptionValue) {
	if stack == nil || len(options) == 0 || len(options) > randomOptionMaximumCount {
		return
	}
	raw := currentRawEntryForStack(slot, *stack)
	for index := randomOptionCountOffset; index <= randomOptionCandidateOffset+3; index++ {
		raw[index] = 0
	}
	raw[randomOptionCountOffset] = byte(len(options))
	for index, option := range options {
		raw[randomOptionTypeOffset+index] = option.Type
		raw[randomOptionValue1Offset+index] = option.Value1
		raw[randomOptionValue2Offset+index] = option.Value2
	}
	raw[randomOptionStateOffset] = 0
	raw[randomOptionChangedIndexOffset] = math.MaxUint8
	stack.RawEntry = raw
	if stack.Extra == nil {
		stack.Extra = make(map[string]string, 16)
	}
	stack.Extra["random_option_count"] = strconv.Itoa(len(options))
	stack.Extra["random_option_state"] = "0"
	stack.Extra["random_option_changed_index"] = "255"
	for index := 0; index < randomOptionMaximumCount; index++ {
		prefix := fmt.Sprintf("random_option_%d_", index+1)
		if index < len(options) {
			stack.Extra[prefix+"type"] = strconv.Itoa(int(options[index].Type))
			stack.Extra[prefix+"value1"] = strconv.Itoa(int(options[index].Value1))
			stack.Extra[prefix+"value2"] = strconv.Itoa(int(options[index].Value2))
		} else {
			stack.Extra[prefix+"type"] = "0"
			stack.Extra[prefix+"value1"] = "0"
			stack.Extra[prefix+"value2"] = "0"
		}
	}
}

func rollRandomOptionQuantity(entries []alignedcmd.RandomOptionWeightedQuantity) (byte, error) {
	total := int64(0)
	for _, entry := range entries {
		if entry.Quantity == 0 || entry.Quantity > randomOptionMaximumCount || entry.Weight <= 0 {
			continue
		}
		if entry.Weight > math.MaxInt64-total {
			return 0, fmt.Errorf("random-option quantity weights overflow")
		}
		total += entry.Weight
	}
	roll, err := secureRandomOptionInt(total)
	if err != nil {
		return 0, err
	}
	for _, entry := range entries {
		if entry.Quantity == 0 || entry.Quantity > randomOptionMaximumCount || entry.Weight <= 0 {
			continue
		}
		if roll < entry.Weight {
			return entry.Quantity, nil
		}
		roll -= entry.Weight
	}
	return 0, fmt.Errorf("random-option quantity selection exhausted")
}

func rollRandomOptionSet(groups [][]alignedcmd.RandomOptionCandidate, used map[byte]struct{}) ([]RandomOptionValue, error) {
	if used == nil {
		used = make(map[byte]struct{}, len(groups))
	}
	options := make([]RandomOptionValue, 0, len(groups))
	for _, group := range groups {
		option, err := rollRandomOptionCandidate(group, used)
		if err != nil {
			return nil, err
		}
		used[option.Type] = struct{}{}
		options = append(options, option)
	}
	return options, nil
}

func rollRandomOptionCandidate(entries []alignedcmd.RandomOptionCandidate, excluded map[byte]struct{}) (RandomOptionValue, error) {
	total := int64(0)
	for _, entry := range entries {
		if entry.Type == 0 || entry.Weight <= 0 {
			continue
		}
		if _, skip := excluded[entry.Type]; skip {
			continue
		}
		if entry.Weight > math.MaxInt64-total {
			return RandomOptionValue{}, fmt.Errorf("random-option candidate weights overflow")
		}
		total += entry.Weight
	}
	roll, err := secureRandomOptionInt(total)
	if err != nil {
		return RandomOptionValue{}, err
	}
	for _, entry := range entries {
		if entry.Type == 0 || entry.Weight <= 0 {
			continue
		}
		if _, skip := excluded[entry.Type]; skip {
			continue
		}
		if roll < entry.Weight {
			return RandomOptionValue{Type: entry.Type, Value1: entry.Value1, Value2: entry.Value2}, nil
		}
		roll -= entry.Weight
	}
	return RandomOptionValue{}, fmt.Errorf("random-option candidate selection exhausted")
}

func secureRandomOptionInt(upper int64) (int64, error) {
	if upper <= 0 {
		return 0, fmt.Errorf("random-option random upper bound %d invalid", upper)
	}
	value, err := cryptorand.Int(cryptorand.Reader, big.NewInt(upper))
	if err != nil {
		return 0, fmt.Errorf("random-option random: %w", err)
	}
	return value.Int64(), nil
}
