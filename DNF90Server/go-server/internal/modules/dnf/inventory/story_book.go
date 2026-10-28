package inventory

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

func (o *Owner) useStoryBook(
	ctx context.Context,
	cmd Command,
	resolution alignedcmd.StackableUseResolution,
	planner alignedcmd.StoryBookPlanner,
) (UseStackableResult, error) {
	if o == nil || o.settlements == nil || planner == nil || resolution.StoryBook == nil {
		return UseStackableResult{}, ErrStoryBookContract
	}
	book := *resolution.StoryBook
	normalizedAction := strings.ToLower(strings.TrimSpace(strings.ReplaceAll(book.ActionType, "`", "")))
	normalizedType := strings.ToLower(strings.TrimSpace(strings.ReplaceAll(resolution.StackableType, "`", "")))
	if cmd.SourceListType != listTypeMain || resolution.ItemID != int64(cmd.ItemTemplateID) ||
		strings.TrimSpace(resolution.PVFPath) == "" || normalizedType != "[waste]" ||
		normalizedAction != "[story book]" || book.QuestID <= 0 || book.QuestID > int64(^uint16(0)) {
		return UseStackableResult{}, fmt.Errorf("%w: item=%d resolution=%+v", ErrStoryBookContract, cmd.ItemTemplateID, resolution)
	}
	characterID := strconv.FormatUint(uint64(cmd.SelectedCharacterID), 10)
	acceptedAt := time.Now().UTC()
	var result UseStackableResult
	err := o.settlements.WithinCharacterSettlement(ctx, characterID, func(tx dnfrepo.Group) error {
		if tx.Character == nil || tx.Inventory == nil || tx.Quest == nil {
			return ErrStoryBookContract
		}
		character, found, err := tx.Character.Load(ctx, characterID)
		if err != nil {
			return err
		}
		if !found || character.CharacterID != characterID ||
			(strings.TrimSpace(cmd.AccountID) != "" && strings.TrimSpace(character.AccountID) != strings.TrimSpace(cmd.AccountID)) {
			return ErrCharacterRequired
		}
		if (book.MinimumLevel > 0 && int64(character.Level) < book.MinimumLevel) ||
			(book.MaximumLevel > 0 && int64(character.Level) > book.MaximumLevel) {
			return ErrStoryBookContract
		}

		inventory, found, err := tx.Inventory.Load(ctx, characterID)
		if err != nil {
			return err
		}
		if !found {
			return ErrInventoryNotFound
		}
		inventory = dnfrepo.CloneInventory(inventory)
		key := slotKey(cmd.SourceListType, cmd.SourceSlotIndex)
		stack, found := inventory.Slots[key]
		if !found || stack.Count <= 0 {
			return fmt.Errorf("%w: list=%d slot=%d", ErrSlotNotFound, cmd.SourceListType, cmd.SourceSlotIndex)
		}
		if stack.ItemID != resolution.ItemID {
			return fmt.Errorf("%w: list=%d slot=%d want=%d got=%d", ErrItemMismatch, cmd.SourceListType, cmd.SourceSlotIndex, resolution.ItemID, stack.ItemID)
		}
		durableKind := strings.ToLower(strings.TrimSpace(stack.Extra["item_kind"]))
		durablePath := strings.TrimSpace(stack.Extra["pvf_path"])
		durableType := strings.ToLower(strings.TrimSpace(strings.ReplaceAll(stack.Extra["stackable_type"], "`", "")))
		if durablePath == "" || !strings.EqualFold(durablePath, strings.TrimSpace(resolution.PVFPath)) ||
			(durableKind != "" && durableKind != "stackable") ||
			(durableType != "" && durableType != normalizedType) {
			return fmt.Errorf("%w: item=%d durable_kind=%q durable_path=%q durable_type=%q", ErrStoryBookContract, stack.ItemID, durableKind, durablePath, durableType)
		}

		questRecord, found, err := tx.Quest.Load(ctx, characterID)
		if err != nil {
			return err
		}
		if !found {
			questRecord = dnfrepo.QuestRecord{CharacterID: characterID}
		} else {
			questRecord = dnfrepo.CloneQuest(questRecord)
		}
		plan, err := planner(character, questRecord, book, acceptedAt)
		if err != nil {
			return err
		}
		if plan.QuestID != book.QuestID || plan.Quest.CharacterID != characterID || len(plan.QuestFields) == 0 {
			return ErrStoryBookContract
		}

		remaining := stack.Count - 1
		if remaining == 0 {
			delete(inventory.Slots, key)
		} else {
			stack = cloneStack(stack)
			stack.Count = remaining
			inventory.Slots[key] = stack
		}
		inventory.UpdatedAt = acceptedAt
		if err := dnfrepo.SaveInventoryFields(ctx, tx.Inventory, inventory, dnfrepo.InventoryFieldSlots); err != nil {
			return err
		}
		if err := dnfrepo.SaveQuestFields(ctx, tx.Quest, plan.Quest, plan.QuestFields...); err != nil {
			return err
		}
		result = UseStackableResult{
			CharacterID:          characterID,
			AccountID:            cmd.AccountID,
			ListType:             cmd.SourceListType,
			SlotIndex:            cmd.SourceSlotIndex,
			InstanceValue:        cmd.SourceInstanceValue,
			ItemID:               stack.ItemID,
			RemainingCount:       remaining,
			Changed:              true,
			PVFPath:              durablePath,
			StackableType:        resolution.StackableType,
			StoryBookApplied:     true,
			StoryBookQuestID:     plan.QuestID,
			StoryBookInitTrigger: plan.InitTrigger,
		}
		return nil
	})
	if err != nil {
		return UseStackableResult{}, err
	}
	return result, nil
}
