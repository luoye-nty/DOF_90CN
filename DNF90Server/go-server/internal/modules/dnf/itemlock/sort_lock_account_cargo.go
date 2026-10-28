// 本文件实现账号金库（list 12）的整理锁定。
// 86JP IsSupportedLockListType 把 AccountCargo 算在整理锁定内（装备锁不含它），
// 而 list 12 的行只存在 AccountInventory，因此必须由账号 owner 翻转标记。
package itemlock

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

func (o *Owner) applyAccountCargoSortLock(ctx context.Context, characterID string, cmd Command, locked bool) (Result, error) {
	accountID := strings.TrimSpace(cmd.AccountID)
	if accountID == "" {
		return Result{}, ErrAccountRequired
	}
	if o == nil || o.accountRepo == nil || o.accountItems == nil {
		return Result{}, ErrAccountOwnerRequired
	}

	var result Result
	err := o.accountItems.WithinAccountCharacterItems(ctx, accountID, characterID, func(accounts dnfrepo.AccountInventoryRepository, _ dnfrepo.InventoryRepository) error {
		cargo, found, err := accounts.Load(ctx, accountID)
		if err != nil {
			return err
		}
		if !found {
			return fmt.Errorf("%w: list=%d slot=%d", ErrSlotNotFound, cmd.ListType, cmd.SlotIndex)
		}
		cargo = dnfrepo.CloneAccountInventory(cargo)
		cargo.AccountID = accountID
		if cargo.Slots == nil {
			cargo.Slots = make(map[string]dnfrepo.ItemStack)
		}
		key := slotKey(cmd.ListType, cmd.SlotIndex)
		stack, ok := cargo.Slots[key]
		if !ok || stack.ItemID <= 0 || stack.Count <= 0 {
			return fmt.Errorf("%w: list=%d slot=%d", ErrSlotNotFound, cmd.ListType, cmd.SlotIndex)
		}

		wasLocked := IsSortLocked(stack)
		result = Result{
			CharacterID: characterID,
			ListType:    cmd.ListType,
			SlotIndex:   cmd.SlotIndex,
			Changed:     wasLocked != locked,
		}
		if locked {
			result.State = sortLockStateActive
		}
		if !result.Changed {
			return nil
		}
		if locked {
			stack = setSortLockState(stack)
		} else {
			stack = clearSortLockState(stack)
		}
		cargo.Slots[key] = stack
		cargo.UpdatedAt = time.Now()
		return accounts.Save(ctx, cargo)
	})
	if err != nil {
		return Result{}, err
	}
	return result, nil
}

// AccountCargoSortLockPositions 给出账号金库里仍然锁定的槽位，供 op13 重建 list 12 之后
// 重发 op713。账号金库的表格和角色容器一样会被重建，padlock 也必须跟着重发。
func AccountCargoSortLockPositions(record dnfrepo.AccountInventoryRecord) []SortLockPosition {
	positions := make([]SortLockPosition, 0)
	for key, stack := range record.Slots {
		listType, slotIndex, ok := parseLockSnapshotSlotKey(key)
		if !ok || listType != listTypeAccountCargo || stack.ItemID <= 0 || stack.Count <= 0 || !IsSortLocked(stack) {
			continue
		}
		positions = append(positions, SortLockPosition{ListType: listType, SlotIndex: slotIndex})
	}
	sort.Slice(positions, func(left, right int) bool {
		return positions[left].SlotIndex < positions[right].SlotIndex
	})
	return positions
}
