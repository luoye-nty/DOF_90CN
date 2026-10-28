package inventory

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	dnffatigue "longheng.io/server/internal/modules/dnf/fatigue"
	dnfitemdailylimit "longheng.io/server/internal/modules/dnf/itemdailylimit"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

// The current client sends fatigue potions through class-1/op515 rather than
// the ordinary class-1/op44 consumable request.
const fatiguePotionActionIndex uint32 = 58

// ErrFatiguePotionDailyLimit marks the "今天已经用满了" rejection so callers can
// answer with a notice the player can read instead of the bare failure code the
// client renders as silence.
var ErrFatiguePotionDailyLimit = errors.New("fatigue potion daily use limit reached")

// FatiguePotionDailyLimitError carries the numbers the player-facing notice
// needs (今天用了几次 / 每天几次).
type FatiguePotionDailyLimitError struct {
	ItemID int64
	Used   int64
	Limit  int64
}

func (e FatiguePotionDailyLimitError) Error() string {
	return fmt.Sprintf("fatigue potion item=%d daily use limit reached: used=%d limit=%d", e.ItemID, e.Used, e.Limit)
}

func (e FatiguePotionDailyLimitError) Is(target error) bool {
	return target == ErrFatiguePotionDailyLimit
}

// UseFatiguePotionAction consumes the source slot and restores fatigue in one
// character-assets transaction. The request carries no item id, so the item
// is resolved from the durable slot and revalidated inside the transaction.
func (o *Owner) UseFatiguePotionAction(
	ctx context.Context,
	cmd Command,
	resolver alignedcmd.StackableUseResolver,
	now time.Time,
	inDungeon bool,
) (UseStackableResult, error) {
	if o == nil || o.assets == nil || resolver == nil {
		return UseStackableResult{}, ErrFatiguePotionContract
	}
	if cmd.SelectedCharacterID == 0 || cmd.SourceListType != listTypeMain ||
		cmd.SourceSlotIndex < 0 || cmd.ActionIndex != fatiguePotionActionIndex || inDungeon {
		return UseStackableResult{}, ErrFatiguePotionContract
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	characterID := strconv.FormatUint(uint64(cmd.SelectedCharacterID), 10)
	var result UseStackableResult
	err := o.assets.WithinCharacterAssets(ctx, characterID, func(characters dnfrepo.CharacterRepository, inventoryRepo dnfrepo.InventoryRepository, _ dnfrepo.EquipmentRepository) error {
		character, found, err := characters.Load(ctx, characterID)
		if err != nil {
			return err
		}
		if !found || strings.TrimSpace(character.CharacterID) != characterID ||
			(strings.TrimSpace(cmd.AccountID) != "" && strings.TrimSpace(character.AccountID) != strings.TrimSpace(cmd.AccountID)) {
			return ErrCharacterRequired
		}
		inventory, found, err := inventoryRepo.Load(ctx, characterID)
		if err != nil {
			return err
		}
		if !found {
			return ErrInventoryNotFound
		}
		inventory = dnfrepo.CloneInventory(inventory)
		if inventory.Slots == nil {
			inventory.Slots = make(map[string]dnfrepo.ItemStack)
		}
		key := slotKey(cmd.SourceListType, cmd.SourceSlotIndex)
		stack, found := inventory.Slots[key]
		if !found || stack.Count <= 0 || stack.ItemID <= 0 {
			return fmt.Errorf("%w: list=%d slot=%d", ErrSlotNotFound, cmd.SourceListType, cmd.SourceSlotIndex)
		}
		resolution, err := resolver(stack.ItemID)
		if err != nil {
			return fmt.Errorf("%w: item=%d: %v", ErrFatiguePotionContract, stack.ItemID, err)
		}
		if resolution.ItemID != stack.ItemID || resolution.FatigueRecovery <= 0 ||
			!strings.EqualFold(strings.TrimSpace(resolution.StackableType), "[expert town potion]") {
			return fmt.Errorf("%w: item=%d resolution=%+v", ErrFatiguePotionContract, stack.ItemID, resolution)
		}
		durablePVFPath := strings.TrimSpace(stack.Extra["pvf_path"])
		if durablePVFPath == "" || !strings.EqualFold(durablePVFPath, strings.TrimSpace(resolution.PVFPath)) {
			return fmt.Errorf("%w: item=%d durable_path=%q resolved_path=%q", ErrFatiguePotionContract, stack.ItemID, durablePVFPath, resolution.PVFPath)
		}
		character = dnfrepo.CloneCharacter(character)
		if character.Stats == nil {
			character.Stats = make(map[string]int64, 6)
		}
		dnffatigue.ReconcileDailyStats(character.Stats, now, character.Level)
		remaining, ok := fatigueRemainingStat(character.Stats)
		if !ok {
			return fmt.Errorf("%w: fatigue remaining value is unavailable", ErrFatiguePotionContract)
		}
		limit := character.Stats["fatigue_limit"]
		if limit <= 0 {
			limit = remaining
			if limit <= 0 {
				limit = 156
			}
			character.Stats["fatigue_limit"] = limit
		}
		maximum := resolution.FatigueMaximumRemaining
		if maximum <= 0 {
			maximum = limit - resolution.FatigueRecovery
		}
		if maximum > 0 && remaining >= maximum {
			return fmt.Errorf("%w: item=%d fatigue=%d must be below %d", ErrFatiguePotionContract, stack.ItemID, remaining, maximum)
		}
		// 上限是道具自己的 PVF [total usable count]，计数走所有消费路径共用的那一份
		// 实现（itemdailylimit）。疲劳药故意**不**走 op515 分派层的统一预留：这次
		// 使用和疲劳恢复必须同生共死，所以扣次数留在这个事务里（handler.go 里
		// action 58 的让路注释是另一半）。用满时返回可识别的拒绝，且被拒的那一次
		// 不改计数、不消耗道具。
		used, consumeErr := dnfitemdailylimit.Consume(
			character.Stats, stack.ItemID, resolution.DailyUseLimit, now, character.Level)
		if consumeErr != nil {
			var reached dnfitemdailylimit.LimitReachedError
			if errors.As(consumeErr, &reached) {
				return fmt.Errorf("%w: %w", ErrFatiguePotionContract, FatiguePotionDailyLimitError{
					ItemID: reached.ItemID,
					Used:   reached.Used,
					Limit:  reached.Limit,
				})
			}
			return fmt.Errorf("%w: %v", ErrFatiguePotionContract, consumeErr)
		}
		after := remaining + resolution.FatigueRecovery
		if after > limit {
			after = limit
		}
		if after < remaining {
			after = remaining
		}
		character.Stats["fatigue"] = after
		character.Stats["fatigue_display_update"] = limit - after
		if character.Stats["fatigue_display_update"] < 0 {
			character.Stats["fatigue_display_update"] = 0
		}
		// 计数已由上面的 Consume 写在同一个事务、同一张 stats 上。
		character.UpdatedAt = now
		remainingCount := stack.Count - 1
		if remainingCount == 0 {
			delete(inventory.Slots, key)
		} else {
			stack = cloneStack(stack)
			stack.Count = remainingCount
			inventory.Slots[key] = stack
		}
		inventory.UpdatedAt = now
		if err := dnfrepo.SaveInventoryFields(ctx, inventoryRepo, inventory, dnfrepo.InventoryFieldSlots); err != nil {
			return err
		}
		if err := dnfrepo.SaveCharacterFields(ctx, characters, character, dnfrepo.CharacterFieldStats); err != nil {
			return err
		}
		result = UseStackableResult{
			CharacterID: characterID, AccountID: cmd.AccountID, ListType: cmd.SourceListType,
			SlotIndex: cmd.SourceSlotIndex, ItemID: stack.ItemID, RemainingCount: remainingCount,
			Changed: true, PVFPath: durablePVFPath, StackableType: resolution.StackableType,
			FatiguePotionApplied: true, FatigueBefore: remaining, FatigueAfter: after,
			FatigueRecovery: resolution.FatigueRecovery,
			DailyUseItemID:  stack.ItemID,
			DailyUseUsed:    used,
			DailyUseLimit:   resolution.DailyUseLimit,
		}
		return nil
	})
	if err != nil {
		return UseStackableResult{}, err
	}
	return result, nil
}

func fatigueRemainingStat(stats map[string]int64) (int64, bool) {
	for _, key := range []string{"fatigue", "fp", "疲劳"} {
		if value, ok := stats[key]; ok {
			return value, true
		}
	}
	return 0, false
}
