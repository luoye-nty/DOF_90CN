package inventory

import (
	"context"
	"fmt"
	"math"
	"math/rand"
	"sort"
	"strconv"
	"time"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

const (
	monsterCardUpgradeErrorInventoryFull byte = 0x04
	monsterCardUpgradeErrorGold          byte = 0x0a
	monsterCardUpgradeErrorInvalid       byte = 0x13
	monsterCardUpgradeProbabilityBase         = 100000
)

// UpgradeMonsterCard applies one current NoPack op603 attempt inside the
// character wallet+inventory transaction. The target and material identities
// come only from the repository; the request contributes locations and count.
func (o *Owner) UpgradeMonsterCard(ctx context.Context, cmd Command, resolver alignedcmd.MonsterCardUpgradeResolver, policy alignedcmd.MonsterCardUpgradePolicy) (MonsterCardUpgradeResult, error) {
	base := MonsterCardUpgradeResult{
		ErrorCode:    monsterCardUpgradeErrorInvalid,
		TargetSlot:   cmd.TargetSlotIndex,
		MaterialSlot: cmd.MaterialSlotIndex,
		ResultSlot:   cmd.TargetSlotIndex,
	}
	if o == nil || o.repo == nil || o.characters == nil {
		return MonsterCardUpgradeResult{}, ErrOwnerUnavailable
	}
	if cmd.SelectedCharacterID == 0 {
		return MonsterCardUpgradeResult{}, ErrCharacterRequired
	}
	if resolver == nil {
		return MonsterCardUpgradeResult{}, ErrMonsterCardUpgradeResolverRequired
	}
	if cmd.SourceListType != listTypeMain || cmd.Count != 1 || cmd.TargetSlotIndex < 0 || cmd.MaterialSlotIndex < 0 {
		return base, nil
	}

	characterID := strconv.FormatUint(uint64(cmd.SelectedCharacterID), 10)
	base.CharacterID = characterID
	if !o.inItemTransaction {
		var result MonsterCardUpgradeResult
		err := o.withinCharacterAssetTransaction(ctx, characterID, func(txOwner *Owner) error {
			var applyErr error
			result, applyErr = txOwner.UpgradeMonsterCard(ctx, cmd, resolver, policy)
			return applyErr
		})
		if err != nil {
			return MonsterCardUpgradeResult{}, err
		}
		if result.CharacterID == "" {
			result = base
		}
		return result, nil
	}

	record, found, err := o.repo.Load(ctx, characterID)
	if err != nil {
		return MonsterCardUpgradeResult{}, err
	}
	if !found {
		return MonsterCardUpgradeResult{}, ErrInventoryNotFound
	}
	record = dnfrepo.CloneInventory(record)
	if record.Slots == nil {
		return base, nil
	}
	targetKey := slotKey(listTypeMain, cmd.TargetSlotIndex)
	materialKey := slotKey(listTypeMain, cmd.MaterialSlotIndex)
	target, targetFound := record.Slots[targetKey]
	material, materialFound := record.Slots[materialKey]
	if !targetFound || !materialFound || target.Count <= 0 || material.Count <= 0 ||
		(cmd.TargetSlotIndex == cmd.MaterialSlotIndex && target.Count < 2) {
		return base, nil
	}

	targetPolicy, err := resolver(target.ItemID)
	if err != nil {
		return MonsterCardUpgradeResult{}, fmt.Errorf("resolve target monster card %d: %w", target.ItemID, err)
	}
	materialPolicy, err := resolver(material.ItemID)
	if err != nil {
		return MonsterCardUpgradeResult{}, fmt.Errorf("resolve material monster card %d: %w", material.ItemID, err)
	}
	if targetPolicy.ItemID != target.ItemID || targetPolicy.PVFPath == "" ||
		materialPolicy.ItemID != material.ItemID || materialPolicy.PVFPath == "" ||
		targetPolicy.Rarity < 0 || targetPolicy.Rarity > 6 ||
		materialPolicy.Rarity < 0 || materialPolicy.Rarity > 6 ||
		targetPolicy.SlotStart < 0 || targetPolicy.SlotEnd < targetPolicy.SlotStart {
		return base, nil
	}
	currentUpgrade := monsterCardUpgradeCountOf(target)
	if targetPolicy.MaximumUpgrade == 0 || currentUpgrade >= targetPolicy.MaximumUpgrade {
		return base, nil
	}

	character, found, err := o.characters.Load(ctx, characterID)
	if err != nil {
		return MonsterCardUpgradeResult{}, err
	}
	if !found {
		return MonsterCardUpgradeResult{}, fmt.Errorf("%w: character=%s", ErrWalletTxnRequired, characterID)
	}
	character = dnfrepo.CloneCharacter(character)
	if character.Stats == nil {
		return MonsterCardUpgradeResult{}, fmt.Errorf("%w: character=%s has no stats", ErrWalletTxnRequired, characterID)
	}
	gold := character.Stats["gold"]
	if targetPolicy.GoldCost < 0 || gold < targetPolicy.GoldCost {
		base.ErrorCode = monsterCardUpgradeErrorGold
		return base, nil
	}

	// Prove the successful branch can be placed before rolling. Otherwise a
	// full inventory could be used to reroll indefinitely without payment.
	preflight := cloneItemMap(record.Slots)
	if _, _, applyErr := applyMonsterCardUpgradeAttempt(preflight, cmd.TargetSlotIndex, cmd.MaterialSlotIndex, targetPolicy, true); applyErr != nil {
		base.ErrorCode = monsterCardUpgradeErrorInventoryFull
		return base, nil
	}

	chance := monsterCardUpgradeChanceFor(policy, targetPolicy, materialPolicy, target.ItemID == material.ItemID)
	random := o.cardUpgradeRandom
	if random == nil {
		random = rand.Intn
	}
	upgradeSucceeded := chance >= monsterCardUpgradeProbabilityBase ||
		(chance > 0 && random(monsterCardUpgradeProbabilityBase) < chance)
	resultSlot, changedSlots, err := applyMonsterCardUpgradeAttempt(
		record.Slots,
		cmd.TargetSlotIndex,
		cmd.MaterialSlotIndex,
		targetPolicy,
		upgradeSucceeded,
	)
	if err != nil {
		return MonsterCardUpgradeResult{}, err
	}

	character.Stats["gold"] = gold - targetPolicy.GoldCost
	// The same [monstercard exp] row that sets the odds also says what one
	// attempt feeds back into the enchanter job, win or lose.
	experienceGain := monsterCardUpgradeExperienceGain(policy, random)
	if experienceGain > 0 {
		character.Stats[expertJobExperienceStat] += int64(experienceGain)
	}
	now := time.Now()
	character.UpdatedAt = now
	record.UpdatedAt = now
	if err := dnfrepo.SaveCharacterFields(ctx, o.characters, character, dnfrepo.CharacterFieldStats); err != nil {
		return MonsterCardUpgradeResult{}, err
	}
	if err := dnfrepo.SaveInventoryFields(ctx, o.repo, record, dnfrepo.InventoryFieldSlots); err != nil {
		return MonsterCardUpgradeResult{}, err
	}
	return MonsterCardUpgradeResult{
		CharacterID:      characterID,
		Success:          true,
		TargetSlot:       cmd.TargetSlotIndex,
		MaterialSlot:     cmd.MaterialSlotIndex,
		ResultSlot:       resultSlot,
		TargetItemID:     target.ItemID,
		UpgradeSucceeded: upgradeSucceeded,
		UpgradeCount:     currentUpgrade + boolByte(upgradeSucceeded),
		Chance:           chance,
		GoldCost:         targetPolicy.GoldCost,
		UpdatedGold:      character.Stats["gold"],
		ChangedSlots:     changedSlots,
		Changed:          true,
	}, nil
}

// expertJobExperienceStat is the character column the expert-job level is
// derived from; dnfbridge reads the same key when it projects the job panel.
const expertJobExperienceStat = "expert_job_exp"

// monsterCardUpgradeChanceFor prefers the enchanter's own [monstercard exp]
// row. That table is the game's authority: it is indexed by the character's
// enchanter level and then by the target card's rarity, so a level-1 enchanter
// reads 100/70/60/50/0/0 while level 11 reads 100 across the board. Until it
// was wired in, every same-card upgrade succeeded outright regardless of job
// level, which is why a fresh enchanter never failed.
//
// The old rarity-gap estimate stays as the fallback for when the enchanter
// config cannot be loaded, so a PVF problem degrades instead of failing closed
// on a player mid-upgrade.
func monsterCardUpgradeChanceFor(policy alignedcmd.MonsterCardUpgradePolicy, target, material alignedcmd.MonsterCardUpgradeResolution, sameItem bool) int {
	if !policy.Known {
		return monsterCardUpgradeChance(target, material, sameItem)
	}
	rarity := target.Rarity
	if rarity < 0 {
		rarity = 0
	}
	if rarity >= len(policy.SuccessRates) {
		rarity = len(policy.SuccessRates) - 1
	}
	rate := policy.SuccessRates[rarity]
	if rate <= 0 {
		return 0
	}
	if rate >= 100 {
		return monsterCardUpgradeProbabilityBase
	}
	return rate * (monsterCardUpgradeProbabilityBase / 100)
}

// monsterCardUpgradeExperienceGain rolls the row's [min,max] inclusive.
func monsterCardUpgradeExperienceGain(policy alignedcmd.MonsterCardUpgradePolicy, random func(int) int) int {
	if !policy.Known || policy.MaximumExperienceGain <= 0 {
		return 0
	}
	low, high := policy.MinimumExperienceGain, policy.MaximumExperienceGain
	if low < 0 {
		low = 0
	}
	if high < low {
		high = low
	}
	if high == low {
		return low
	}
	return low + random(high-low+1)
}

func monsterCardUpgradeChance(target, material alignedcmd.MonsterCardUpgradeResolution, sameItem bool) int {
	if sameItem || material.Rarity > target.Rarity {
		return monsterCardUpgradeProbabilityBase
	}
	if target.Rarity < 0 || target.Rarity > 6 || material.Rarity < 0 || material.Rarity > 6 || target.CalculateConst <= 0 {
		return 0
	}
	// The retained table owns four probability tiers. Current 90CN cards use
	// rarity 4..6 as higher presentation grades, so probability calculation
	// saturates at tier 3 while identity/ordering above remains exact.
	targetTier := min(target.Rarity, 3)
	materialTier := min(material.Rarity, 3)
	chance := int64((80 - targetTier*10) * 1000)
	for gap := targetTier - materialTier; gap > 0; gap-- {
		chance /= 2
	}
	chance = chance * int64(target.CalculateConst) / 10
	if chance > monsterCardUpgradeProbabilityBase {
		chance = monsterCardUpgradeProbabilityBase
	}
	if chance < 0 {
		return 0
	}
	return int(chance)
}

func applyMonsterCardUpgradeAttempt(items map[string]dnfrepo.ItemStack, targetSlot, materialSlot int16, policy alignedcmd.MonsterCardUpgradeResolution, success bool) (int16, []int16, error) {
	targetKey := slotKey(listTypeMain, targetSlot)
	materialKey := slotKey(listTypeMain, materialSlot)
	target, targetFound := items[targetKey]
	material, materialFound := items[materialKey]
	if !targetFound || !materialFound || target.Count <= 0 || material.Count <= 0 {
		return targetSlot, nil, ErrSlotNotFound
	}
	targetSnapshot := cloneStack(target)
	changed := map[int16]struct{}{materialSlot: {}}
	if material.Count == 1 {
		delete(items, materialKey)
	} else {
		material = cloneStack(material)
		material.Count--
		updateStackRawAmount(&material)
		items[materialKey] = material
	}
	if !success {
		return targetSlot, sortedMonsterCardSlots(changed), nil
	}

	upgraded := cloneStack(targetSnapshot)
	upgraded.Count = 1
	setMonsterCardUpgradeCount(&upgraded, monsterCardUpgradeCountOf(targetSnapshot)+1)
	updateStackRawAmount(&upgraded)

	// Spend the target card. When the target and the material share one stack
	// -- which is the normal case, since a pile of identical cards upgrades
	// against itself -- the material step above may already have emptied the
	// slot.
	if remaining, remains := items[targetKey]; remains {
		if remaining.Count <= 1 {
			delete(items, targetKey)
		} else {
			remaining = cloneStack(remaining)
			remaining.Count--
			updateStackRawAmount(&remaining)
			items[targetKey] = remaining
		}
	}
	changed[targetSlot] = struct{}{}

	// Merge before placing. This used to drop the result straight back into the
	// target slot whenever that slot had gone empty, which is exactly what the
	// last upgrade of a pile does: the new card landed in its own cell beside
	// the stack it belonged to, and only a full bag refresh merged them.
	// Merging cannot pick the target's own leftovers up by mistake, because
	// those still carry the old upgrade count.
	resultSlot, err := placeUpgradedMonsterCard(items, upgraded, policy, targetSlot)
	if err != nil {
		return targetSlot, nil, err
	}
	changed[resultSlot] = struct{}{}
	return resultSlot, sortedMonsterCardSlots(changed), nil
}

// placeUpgradedMonsterCard merges the result into an existing identical stack
// when there is one, and otherwise takes a free cell. preferred is the slot the
// upgrade came from: the merge pass always skips it, and it is then used first
// among the empty cells so a card with no stack to join stays where the player
// was working instead of jumping to the front of the tab.
//
// Skipping the source slot in the merge pass is load-bearing, not defensive.
// It was briefly removed on the theory that the upgrade-count comparison below
// already separated the result from its own leftovers; it does not, and the
// whole remaining pile inherited the upgraded card's state -- eleven cards all
// reading +1 after a single successful upgrade.
func placeUpgradedMonsterCard(items map[string]dnfrepo.ItemStack, upgraded dnfrepo.ItemStack, policy alignedcmd.MonsterCardUpgradeResolution, preferred int16) (int16, error) {
	upgradeCount := monsterCardUpgradeCountOf(upgraded)
	limit := policy.StackLimit
	if limit == 0 {
		limit = math.MaxInt32
	}
	// 升级结果行同样是"入包"：容器总量不得越过 PVF [stack limit]。这里升级本身
	// 消耗一张同 id 卡（调用方已经扣减），所以正常情况下容器总量不变，这条只拦
	// 历史超限数据。行的身份还包括卡片升级级数（不同级数必须是不同的行），因此
	// 这里不做"同 id 只能一行"的合并，见报告说明。
	if dnfrepo.ContainerItemSpace(items, listTypeMain, upgraded.ItemID, policy.StackLimit) <= 0 {
		return -1, fmt.Errorf(
			"monster card upgrade would exceed the container stack limit: item=%d",
			upgraded.ItemID,
		)
	}
	for slot := policy.SlotStart; slot <= policy.SlotEnd; slot++ {
		if slot == preferred {
			continue
		}
		key := slotKey(listTypeMain, slot)
		stack, found := items[key]
		if !found || stack.ItemID != upgraded.ItemID || stack.Count <= 0 || stack.Count >= limit ||
			stack.Bind != upgraded.Bind || !stack.ExpireAt.Equal(upgraded.ExpireAt) ||
			monsterCardUpgradeCountOf(stack) != upgradeCount {
			continue
		}
		stack = cloneStack(stack)
		stack.Count++
		updateStackRawAmount(&stack)
		items[key] = stack
		return slot, nil
	}
	if preferred >= policy.SlotStart && preferred <= policy.SlotEnd {
		key := slotKey(listTypeMain, preferred)
		if _, occupied := items[key]; !occupied {
			items[key] = upgraded
			return preferred, nil
		}
	}
	for slot := policy.SlotStart; slot <= policy.SlotEnd; slot++ {
		key := slotKey(listTypeMain, slot)
		if _, occupied := items[key]; occupied {
			continue
		}
		items[key] = upgraded
		return slot, nil
	}
	return -1, fmt.Errorf("monster card upgrade result has no free slot in %d..%d", policy.SlotStart, policy.SlotEnd)
}

func monsterCardUpgradeCountOf(stack dnfrepo.ItemStack) byte {
	value := firstExtraInt(stack.Extra, -1, "enchant_upgrade_count", "byte_12", "value_12")
	if value >= 0 && value <= int64(^byte(0)) {
		return byte(value)
	}
	if len(stack.RawEntry) > 0x12 {
		return stack.RawEntry[0x12]
	}
	return 0
}

func setMonsterCardUpgradeCount(stack *dnfrepo.ItemStack, value byte) {
	if stack == nil {
		return
	}
	if stack.Extra == nil {
		stack.Extra = make(map[string]string, 2)
	} else {
		stack.Extra = cloneStringMap(stack.Extra)
	}
	stack.Extra["enchant_upgrade_count"] = strconv.Itoa(int(value))
	stack.Extra["byte_12"] = strconv.Itoa(int(value))
	if len(stack.RawEntry) == currentItemListEntrySize {
		stack.RawEntry = append([]byte(nil), stack.RawEntry...)
		stack.RawEntry[0x12] = value
	}
}

func cloneStringMap(values map[string]string) map[string]string {
	out := make(map[string]string, len(values)+2)
	for key, value := range values {
		out[key] = value
	}
	return out
}

func sortedMonsterCardSlots(values map[int16]struct{}) []int16 {
	out := make([]int16, 0, len(values))
	for slot := range values {
		out = append(out, slot)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func boolByte(value bool) byte {
	if value {
		return 1
	}
	return 0
}
