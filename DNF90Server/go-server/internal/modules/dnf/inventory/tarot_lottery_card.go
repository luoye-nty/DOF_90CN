package inventory

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

// 塔罗牌类道具（PVF `[stackable type]` = `[monster tarot card]`）的使用。
//
// 为什么它**不能**照抄 useRandomRewardItem：那张卡要同时改**金币**和**背包**。
// useRandomRewardItem 走 withinInventoryTransaction（只给 inventory/equipment 两个
// 仓库），金币根本拿不到；必须在 withinCharacterAssetTransaction 里做，它给的是
// (characters, inventory, equipment) 三个仓库，同一笔事务提交 —— 这正是
// 「要么全成要么全不成」的边界：任何一步 return error，整笔回滚，金币、道具、
// 背包三者一个都没变。
//
// 金币从哪减：PVF `[lottery use cost]`（**从文档读，不硬编码**；服主 2026-09-29 确认
// 这个字段扣的是金币，塔罗牌这次是 100000）。写法和修理路径一致
// （owner.go:1854 `character.Stats["gold"] = gold - cost` +
// dnfrepo.SaveCharacterFields(..., CharacterFieldStats)）。
var (
	// ErrTarotLotteryContractRequired 契约不完整（源道具不匹配 / 无路径 / 类型不对 /
	// 没有金币花费 / 既没抽中也没落空）。
	ErrTarotLotteryContractRequired = errors.New("tarot lottery card contract is required")
	// ErrTarotLotteryInsufficientGold 金币不足：整次使用中止，什么都不改。
	ErrTarotLotteryInsufficientGold = errors.New("tarot lottery card gold is insufficient")
	// ErrTarotLotteryMissed 掷到了奖池的空隙。**这是我们与官服可能不同的一处保守
	// 选择**：官方"照收钱不给东西"或"整次中止"两种做法都说得通，这里选整次中止
	// （不扣钱、不扣道具），因为宁可这次不生效，也不能让玩家花了钱拿到空气。
	ErrTarotLotteryMissed = errors.New("tarot lottery card roll landed in the table gap")
)

// UseTarotLotteryCard opens one tarot card: charge its gold, consume the card and
// grant the drawn item, all inside one character-asset transaction.
func (o *Owner) UseTarotLotteryCard(
	ctx context.Context,
	cmd Command,
	resolution alignedcmd.TarotLotteryResolution,
) (UseStackableResult, error) {
	if o == nil {
		return UseStackableResult{}, ErrOwnerUnavailable
	}
	if cmd.SelectedCharacterID == 0 {
		return UseStackableResult{}, ErrCharacterRequired
	}
	characterID := strconv.FormatUint(uint64(cmd.SelectedCharacterID), 10)
	if !o.inItemTransaction {
		// 金币 + 背包必须同一笔：withinCharacterAssetTransaction 会把 characters /
		// inventory / equipment 三个仓库一起换进 txOwner，并在 apply 返回 error 时
		// 整笔回滚。
		var result UseStackableResult
		err := o.withinCharacterAssetTransaction(ctx, characterID, func(txOwner *Owner) error {
			var err error
			result, err = txOwner.useTarotLotteryCard(ctx, cmd, resolution)
			return err
		})
		return result, err
	}
	return o.useTarotLotteryCard(ctx, cmd, resolution)
}

func (o *Owner) useTarotLotteryCard(
	ctx context.Context,
	cmd Command,
	resolution alignedcmd.TarotLotteryResolution,
) (UseStackableResult, error) {
	// --- 契约校验：任何一项不成立都直接中止（fail closed，什么都不改） ---
	if resolution.SourceItemID != int64(cmd.ItemTemplateID) ||
		strings.TrimSpace(resolution.SourcePVFPath) == "" ||
		!strings.EqualFold(strings.Trim(resolution.StackableType, "` []"), "monster tarot card") ||
		resolution.GoldCost <= 0 {
		return UseStackableResult{}, fmt.Errorf("%w: item=%d", ErrTarotLotteryContractRequired, cmd.ItemTemplateID)
	}
	if resolution.Drawn == nil || resolution.Drawn.Reward.ItemID <= 0 || resolution.Drawn.Count <= 0 {
		if resolution.Missed {
			return UseStackableResult{}, fmt.Errorf("%w: item=%d roll=%d total=%d",
				ErrTarotLotteryMissed, cmd.ItemTemplateID, resolution.Roll, resolution.TotalWeight)
		}
		return UseStackableResult{}, fmt.Errorf("%w: item=%d has nothing drawn", ErrTarotLotteryContractRequired, cmd.ItemTemplateID)
	}

	// --- 金币：先读再判，不足就在动任何东西之前中止 ---
	characterID := strconv.FormatUint(uint64(cmd.SelectedCharacterID), 10)
	character, found, err := o.characters.Load(ctx, characterID)
	if err != nil {
		return UseStackableResult{}, err
	}
	if !found {
		return UseStackableResult{}, ErrInventoryNotFound
	}
	if character.Stats["gold"] < resolution.GoldCost {
		return UseStackableResult{}, fmt.Errorf("%w: have=%d need=%d",
			ErrTarotLotteryInsufficientGold, character.Stats["gold"], resolution.GoldCost)
	}

	// --- 背包：源槽校验 + 消耗 1 个 ---
	record, ok, err := o.repo.Load(ctx, characterID)
	if err != nil {
		return UseStackableResult{}, err
	}
	if !ok {
		return UseStackableResult{}, ErrInventoryNotFound
	}
	record = dnfrepo.CloneInventory(record)
	key := slotKey(cmd.SourceListType, cmd.SourceSlotIndex)
	source, ok := record.Slots[key]
	if !ok || source.Count <= 0 {
		return UseStackableResult{}, fmt.Errorf("%w: list=%d slot=%d", ErrSlotNotFound, cmd.SourceListType, cmd.SourceSlotIndex)
	}
	if source.ItemID != int64(cmd.ItemTemplateID) {
		return UseStackableResult{}, fmt.Errorf("%w: list=%d slot=%d want=%d got=%d",
			ErrItemMismatch, cmd.SourceListType, cmd.SourceSlotIndex, cmd.ItemTemplateID, source.ItemID)
	}
	// 真档没有 [usable count]/[stack limit]，op44 这条框架路径默认**消耗 1 个**。
	remaining := source.Count - 1
	if remaining == 0 {
		delete(record.Slots, key)
	} else {
		source = cloneStack(source)
		source.Count = remaining
		updateStackRawAmount(&source)
		record.Slots[key] = source
	}

	// --- 发放中奖物品（复用既有发奖；数量来自 [int data] 的 count 列） ---
	slots, err := grantRandomRewardItem(record.Slots, resolution.Drawn.Reward, resolution.Drawn.Count)
	if err != nil {
		return UseStackableResult{}, err
	}

	result := UseStackableResult{
		CharacterID:    characterID,
		AccountID:      cmd.AccountID,
		ListType:       cmd.SourceListType,
		SlotIndex:      cmd.SourceSlotIndex,
		InstanceValue:  cmd.SourceInstanceValue,
		ItemID:         int64(cmd.ItemTemplateID),
		RemainingCount: remaining,
		Changed:        true,
		PVFPath:        resolution.SourcePVFPath,
		StackableType:  resolution.StackableType,

		RandomRewardItemID: resolution.Drawn.Reward.ItemID,
		RandomRewardSlots:  slots,
	}

	// --- 一次提交：金币与道具同笔。任一失败都回滚，金币不会被扣。 ---
	character.Stats["gold"] -= resolution.GoldCost
	character.UpdatedAt = time.Now()
	if err := dnfrepo.SaveCharacterFields(ctx, o.characters, character, dnfrepo.CharacterFieldStats); err != nil {
		return UseStackableResult{}, err
	}
	record.UpdatedAt = time.Now()
	if err := dnfrepo.SaveInventoryFields(ctx, o.repo, record, dnfrepo.InventoryFieldSlots); err != nil {
		return UseStackableResult{}, err
	}
	return result, nil
}
