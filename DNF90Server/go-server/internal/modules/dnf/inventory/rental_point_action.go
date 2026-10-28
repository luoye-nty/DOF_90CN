package inventory

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	dnfrental "longheng.io/server/internal/modules/dnf/rental"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

// The current client routes a [waste] item whose [action type] is
// `[rent point]` through class-1/op515 with this action index, exactly like
// the fatigue potion uses 58. Live evidence: using 50幸运星礼盒 (490001384,
// `[action type] `[rent point]` 50`) sends op515 body
// {slot=3, list=0, action=105}; the server used to answer failure 17 because
// no case claimed the index.
const rentalPointActionIndex uint32 = 105

// UseRentalPointAction consumes the source slot and charges the account's
// rental wallet in one account/character/inventory transaction. The request
// carries no item id, so the item is resolved from the durable slot and
// revalidated inside the transaction.
func (o *Owner) UseRentalPointAction(
	ctx context.Context,
	cmd Command,
	resolver alignedcmd.StackableUseResolver,
	now time.Time,
) (UseStackableResult, error) {
	if o == nil || o.rentalAssets == nil || resolver == nil {
		return UseStackableResult{}, ErrRentalPointContract
	}
	if cmd.SelectedCharacterID == 0 || cmd.SourceListType != listTypeMain ||
		cmd.SourceSlotIndex < 0 || cmd.ActionIndex != rentalPointActionIndex ||
		strings.TrimSpace(cmd.AccountID) == "" {
		return UseStackableResult{}, ErrRentalPointContract
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	characterID := strconv.FormatUint(uint64(cmd.SelectedCharacterID), 10)
	accountID := strings.TrimSpace(cmd.AccountID)
	var result UseStackableResult
	err := dnfrepo.WithinRentalAssetsScoped(ctx, o.rentalAssets, accountID, characterID, func(
		accounts dnfrepo.AccountRepository,
		characters dnfrepo.CharacterRepository,
		inventoryRepo dnfrepo.InventoryRepository,
		_ dnfrepo.EquipmentRepository,
	) error {
		character, found, err := characters.Load(ctx, characterID)
		if err != nil {
			return err
		}
		if !found || strings.TrimSpace(character.CharacterID) != characterID ||
			strings.TrimSpace(character.AccountID) != accountID {
			return ErrCharacterRequired
		}
		loadedAccount, found, err := accounts.Load(ctx, accountID)
		if err != nil {
			return err
		}
		if !found || strings.TrimSpace(loadedAccount.AccountID) != accountID {
			return ErrCharacterRequired
		}
		account := dnfrepo.CloneAccount(loadedAccount)
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
			return fmt.Errorf("%w: item=%d: %v", ErrRentalPointContract, stack.ItemID, err)
		}
		grant := resolution.RentalPoint
		if resolution.ItemID != stack.ItemID || grant == nil || grant.Grant == 0 || grant.Limit == 0 ||
			!strings.EqualFold(strings.TrimSpace(resolution.StackableType), "[waste]") {
			return fmt.Errorf("%w: item=%d resolution=%+v", ErrRentalPointContract, stack.ItemID, resolution)
		}
		before, err := dnfrental.Points(account)
		if err != nil {
			return fmt.Errorf("%w: item=%d: %v", ErrRentalPointContract, stack.ItemID, err)
		}
		// The client's own explain text states the cap ("持有上限为400个,
		// 超过后无法继续获得"), so a full wallet must refuse the use instead of
		// eating the box for nothing.
		if before >= grant.Limit {
			return fmt.Errorf("%w: item=%d wallet %d already at limit %d", ErrRentalPointLimit, stack.ItemID, before, grant.Limit)
		}
		applied := grant.Grant
		if available := grant.Limit - before; applied > available {
			applied = available
		}
		after := before + applied
		dnfrental.SetPoints(&account, after)
		account.UpdatedAt = now
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
		if err := accounts.Save(ctx, account); err != nil {
			return err
		}
		result = UseStackableResult{
			CharacterID:        characterID,
			AccountID:          accountID,
			ListType:           cmd.SourceListType,
			SlotIndex:          cmd.SourceSlotIndex,
			ItemID:             stack.ItemID,
			RemainingCount:     remainingCount,
			Changed:            true,
			PVFPath:            strings.TrimSpace(resolution.PVFPath),
			StackableType:      resolution.StackableType,
			RentalPointApplied: true,
			RentalPointBefore:  before,
			RentalPointGranted: applied,
			RentalPointAfter:   after,
			RentalPointLimit:   grant.Limit,
			RentalPointAtLimit: after >= grant.Limit,
		}
		return nil
	})
	if err != nil {
		return UseStackableResult{}, err
	}
	return result, nil
}
