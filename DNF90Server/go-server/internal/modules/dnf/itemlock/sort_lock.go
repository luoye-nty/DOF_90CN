package itemlock

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

const (
	sortLockStateKey    = "item_sort_lock_state"
	sortLockStateActive = "1"
)

// SortLockPosition is one client-visible sorting lock at its current item slot.
type SortLockPosition struct {
	ListType  byte
	SlotIndex int16
}

// DecodeSortLockRequest keeps the exact op713/op714 request separate from the
// response's leading success byte.
func DecodeSortLockRequest(body []byte) (Request, error) {
	if len(body) != 3 {
		return Request{}, fmt.Errorf("sort-lock body length: got %d want 3", len(body))
	}
	return DecodeRequest(body)
}

// BuildSortLockStateBody builds success + list + slot. The opcode selects lock
// or unlock; both successful operations use this current-client layout.
func BuildSortLockStateBody(listType byte, slotIndex int16) []byte {
	return append([]byte{1}, buildLockAck(listType, slotIndex)...)
}

// ApplySortLock persists the sorting-only state without touching the ordinary
// equipment protection lock. Repeating either operation remains successful and
// idempotent because the EXE uses the ACK to converge its blue icon.
func (o *Owner) ApplySortLock(ctx context.Context, cmd Command, locked bool) (Result, error) {
	if o == nil || o.repo == nil {
		return Result{}, ErrOwnerUnavailable
	}
	if cmd.SelectedCharacterID == 0 {
		return Result{}, ErrCharacterRequired
	}
	// 86JP IsSupportedLockListType 把 PersonalCargo / AccountCargo 都算进整理锁定
	// （装备锁 IsSupportedEquipmentLockListType 只含 PersonalCargo，不含 AccountCargo）。
	// 这里只放行本 owner 能正确落库的容器：list 0/1 在角色 Slots，list 2（个人金库）
	// 在角色 Warehouse，list 12（账号金库）在 AccountInventory。Equipment / Pet /
	// GuildMedal 继续 fail-close：它们的行不在这个 owner 持有的仓储里，放行只会变成
	// "改错表"或"永远找不到行"，两者都会让客户端拿不到 ACK。
	if cmd.ListType != listTypeMain && cmd.ListType != listTypeAvatar &&
		cmd.ListType != listTypePersonalCargo && cmd.ListType != listTypeAccountCargo {
		return Result{}, fmt.Errorf("%w: %d", ErrUnsupportedList, cmd.ListType)
	}

	characterID := strconv.FormatUint(uint64(cmd.SelectedCharacterID), 10)
	if cmd.ListType == listTypeAccountCargo {
		// 账号金库的行不在角色 inventory 记录里，只能由账号 owner 翻转。
		return o.applyAccountCargoSortLock(ctx, characterID, cmd, locked)
	}
	record, found, err := o.repo.Load(ctx, characterID)
	if err != nil {
		return Result{}, err
	}
	if !found {
		return Result{}, ErrInventoryNotFound
	}
	record = dnfrepo.CloneInventory(record)
	// 个人金库（list 2）的行在 Warehouse 而不是 Slots：取错表会一律判成空槽，
	// 客户端于是拿不到 ACK（现场 2026-09-27 11:37 的 op713 list=2 slot=118）。
	// 行所在的表与落库字段必须成对取自 itemMapForList，和装备锁 Apply 同一口径。
	items, field := itemMapForList(&record, cmd.ListType)
	key := slotKey(cmd.ListType, cmd.SlotIndex)
	stack, found := items[key]
	if !found || stack.ItemID <= 0 || stack.Count <= 0 {
		return Result{}, fmt.Errorf("%w: list=%d slot=%d", ErrSlotNotFound, cmd.ListType, cmd.SlotIndex)
	}

	wasLocked := IsSortLocked(stack)
	result := Result{
		CharacterID: characterID,
		ListType:    cmd.ListType,
		SlotIndex:   cmd.SlotIndex,
		Changed:     wasLocked != locked,
	}
	if locked {
		result.State = sortLockStateActive
	}
	if !result.Changed {
		return result, nil
	}
	if locked {
		stack = setSortLockState(stack)
	} else {
		stack = clearSortLockState(stack)
	}
	items[key] = stack
	record.UpdatedAt = time.Now()
	if err := dnfrepo.SaveInventoryFields(ctx, o.repo, record, field); err != nil {
		return Result{}, err
	}
	return result, nil
}

// IsSortLocked reports only the independent sorting lock state. Equipment
// protection locks deliberately use different Extra keys.
func IsSortLocked(stack dnfrepo.ItemStack) bool {
	switch strings.ToLower(strings.TrimSpace(currentSortLockState(stack))) {
	case sortLockStateActive, "locked", "active":
		return true
	default:
		return false
	}
}

func currentSortLockState(stack dnfrepo.ItemStack) string {
	if stack.Extra == nil {
		return ""
	}
	return strings.TrimSpace(stack.Extra[sortLockStateKey])
}

func setSortLockState(stack dnfrepo.ItemStack) dnfrepo.ItemStack {
	if stack.Extra == nil {
		stack.Extra = make(map[string]string, 1)
	}
	stack.Extra[sortLockStateKey] = sortLockStateActive
	return stack
}

// SetSortLockState 给物品行打上整理锁。供移动 / 合并路径把来源行已有的锁带到新落点：
// 整理锁挂在物品上，搬动与合并都不该让它消失（运营口径 2026-09-27）。
func SetSortLockState(stack dnfrepo.ItemStack) dnfrepo.ItemStack {
	return setSortLockState(stack)
}

// ClearSortLockState 清掉整理锁定标记，供移动物品时的 86JP PrepareForDestination 规则使用。
func ClearSortLockState(stack dnfrepo.ItemStack) dnfrepo.ItemStack {
	return clearSortLockState(stack)
}

func clearSortLockState(stack dnfrepo.ItemStack) dnfrepo.ItemStack {
	if stack.Extra == nil {
		return stack
	}
	delete(stack.Extra, sortLockStateKey)
	if len(stack.Extra) == 0 {
		stack.Extra = nil
	}
	return stack
}

// SortLockPositions returns deterministic positions for native op713 replay
// after op13 container snapshots have rebuilt the client's item objects.
//
// 每个容器只认自己该有的 list：角色 Slots 是 list 0/1，Warehouse 是 list 2
// （个人金库）。Warehouse 里历史遗留的 12:* 残留**不能**重发 —— 账号金库唯一的
// durable owner 是 AccountInventory，它的位置由 AccountCargoSortLockPositions 提供。
// 漏掉个人金库的后果是：锁虽然落了库，但任何一次 op13 重建（过图 / 开包 / 整理 /
// 重上）都会让客户端的蓝锁消失，而服务端 op13/op14 物品行并不携带排序锁 flag，
// 客户端只能靠这里的重发得知锁状态。
func SortLockPositions(record dnfrepo.InventoryRecord) []SortLockPosition {
	positions := make([]SortLockPosition, 0)
	positions = appendSortLockPositions(positions, record.Slots, listTypeMain, listTypeAvatar)
	positions = appendSortLockPositions(positions, record.Warehouse, listTypePersonalCargo)
	sort.Slice(positions, func(left, right int) bool {
		if positions[left].ListType != positions[right].ListType {
			return positions[left].ListType < positions[right].ListType
		}
		return positions[left].SlotIndex < positions[right].SlotIndex
	})
	return positions
}

// appendSortLockPositions 只接受调用方点名的 list：同一个 key 可能因为历史迁移留在
// 别的表里（例如 Warehouse 的 12:* 残留），按表限定 list 才不会把它当成真的锁。
func appendSortLockPositions(
	positions []SortLockPosition,
	items map[string]dnfrepo.ItemStack,
	allowedListTypes ...byte,
) []SortLockPosition {
	for key, stack := range items {
		listType, slotIndex, ok := parseLockSnapshotSlotKey(key)
		if !ok || stack.ItemID <= 0 || stack.Count <= 0 || !IsSortLocked(stack) {
			continue
		}
		if !containsListType(allowedListTypes, listType) {
			continue
		}
		positions = append(positions, SortLockPosition{ListType: listType, SlotIndex: slotIndex})
	}
	return positions
}

func containsListType(allowedListTypes []byte, listType byte) bool {
	for _, allowed := range allowedListTypes {
		if allowed == listType {
			return true
		}
	}
	return false
}
