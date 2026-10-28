package inventory

import (
	"context"
	"fmt"
	"strconv"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
)

// The current client sends PVF [gm quest clear] and related quest-completion
// actions through class-1/op515 with this action index. The request names only
// the durable source slot; the server resolves and revalidates the item inside
// the existing inventory/quest/character settlement transaction.
const questCompletionTicketActionIndex uint32 = 167

func (o *Owner) UseQuestCompletionTicketAction(
	ctx context.Context,
	cmd Command,
	resolver alignedcmd.StackableUseResolver,
	planner alignedcmd.QuestCompletionTicketPlanner,
	inDungeon bool,
) (UseStackableResult, error) {
	if o == nil || o.repo == nil {
		return UseStackableResult{}, ErrOwnerUnavailable
	}
	if cmd.SelectedCharacterID == 0 {
		return UseStackableResult{}, ErrCharacterRequired
	}
	if cmd.SourceListType != listTypeMain || cmd.SourceSlotIndex < 0 || cmd.ActionIndex != questCompletionTicketActionIndex {
		return UseStackableResult{}, ErrQuestCompletionTicketContract
	}
	if resolver == nil || planner == nil {
		return UseStackableResult{}, ErrQuestCompletionTicketContract
	}
	characterID := strconv.FormatUint(uint64(cmd.SelectedCharacterID), 10)
	inventory, found, err := o.repo.Load(ctx, characterID)
	if err != nil {
		return UseStackableResult{}, err
	}
	if !found {
		return UseStackableResult{}, ErrInventoryNotFound
	}
	stack, found := inventory.Slots[slotKey(cmd.SourceListType, cmd.SourceSlotIndex)]
	if !found || stack.Count <= 0 || stack.ItemID <= 0 {
		return UseStackableResult{}, fmt.Errorf("%w: list=%d slot=%d", ErrSlotNotFound, cmd.SourceListType, cmd.SourceSlotIndex)
	}
	if stack.ItemID > int64(^uint32(0)>>1) {
		return UseStackableResult{}, fmt.Errorf("%w: item=%d exceeds current-client signed item id", ErrQuestCompletionTicketContract, stack.ItemID)
	}
	resolution, err := resolver(stack.ItemID)
	if err != nil {
		return UseStackableResult{}, fmt.Errorf("%w: item=%d: %v", ErrQuestCompletionTicketResolveFailed, stack.ItemID, err)
	}
	if resolution.ItemID != stack.ItemID || resolution.QuestCompletionTicket == nil {
		return UseStackableResult{}, fmt.Errorf("%w: item=%d resolution=%+v", ErrQuestCompletionTicketContract, stack.ItemID, resolution)
	}
	if resolution.QuestCompletionTicket.VillageOnly && inDungeon {
		return UseStackableResult{}, ErrQuestCompletionTicketContract
	}
	cmd.ItemTemplateID = int32(stack.ItemID)
	return o.useQuestCompletionTicket(ctx, cmd, resolution, planner)
}
