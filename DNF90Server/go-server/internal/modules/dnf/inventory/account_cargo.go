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

// moveAccountCargo moves real list-12 rows between the selected character and
// the account owner.  AccountInventory is deliberately the sole durable owner
// of list 12; a character Warehouse entry with a 12:* key is old residue and
// is never read or written here.
func (o *Owner) moveAccountCargo(ctx context.Context, characterID string, cmd Command) (MoveResult, error) {
	accountID := strings.TrimSpace(cmd.AccountID)
	if accountID == "" {
		return MoveResult{}, ErrAccountRequired
	}
	if o == nil || o.repo == nil || o.accountRepo == nil || o.accountItems == nil || o.accounts == nil {
		return MoveResult{}, ErrAccountSharedOwnerUnavailable
	}
	account, found, err := o.accounts.Load(ctx, accountID)
	if err != nil {
		return MoveResult{}, err
	}
	if !found {
		return MoveResult{}, ErrAccountRequired
	}
	capacity, created := accountCargoCapacity(account)
	if !created {
		return MoveResult{}, ErrAccountCargoNotCreated
	}
	// 运营口径 2026-09-18：落点在**未解锁**栏位不再是拒绝理由，而是顺延到已解锁区
	// （先叠加同类、再从第一格起找空格），真的满了才回"物品栏空间不足"。这里只挡住
	// 真正非法的负槽位；装不装得下交给事务里的 moveAccountCargoMaps 判定（那里才拿得到
	// 已加载的金库行）。源端同样放宽：历史数据可能把行留在未解锁格里，不能让那件道具
	// 卡死搬不出来。
	if err := checkAccountCargoNegativeSlot(cmd.SourceListType, cmd.SourceSlotIndex); err != nil {
		return MoveResult{}, err
	}
	if err := checkAccountCargoNegativeSlot(cmd.DestinationListType, cmd.DestinationSlotIndex); err != nil {
		return MoveResult{}, err
	}

	var result MoveResult
	err = o.accountItems.WithinAccountCharacterItems(ctx, accountID, characterID, func(accounts dnfrepo.AccountInventoryRepository, characters dnfrepo.InventoryRepository) error {
		character, exists, err := characters.Load(ctx, characterID)
		if err != nil {
			return err
		}
		if !exists {
			return ErrInventoryNotFound
		}
		cargo, exists, err := accounts.Load(ctx, accountID)
		if err != nil {
			return err
		}
		if !exists {
			cargo = dnfrepo.AccountInventoryRecord{AccountID: accountID}
		}
		character = dnfrepo.CloneInventory(character)
		cargo = dnfrepo.CloneAccountInventory(cargo)
		character.CharacterID = characterID
		cargo.AccountID = accountID
		if character.Slots == nil {
			character.Slots = make(map[string]dnfrepo.ItemStack)
		}
		if character.Warehouse == nil {
			character.Warehouse = make(map[string]dnfrepo.ItemStack)
		}
		if cargo.Slots == nil {
			cargo.Slots = make(map[string]dnfrepo.ItemStack)
		}

		srcItems, srcField, sourceCargo, err := accountCargoItemMap(&character, &cargo, cmd.SourceListType)
		if err != nil {
			return err
		}
		dstItems, dstField, destinationCargo, err := accountCargoItemMap(&character, &cargo, cmd.DestinationListType)
		if err != nil {
			return err
		}
		result, err = moveAccountCargoMaps(ctx, o, characterID, cmd, srcItems, dstItems, capacity)
		if err != nil || !result.Changed {
			return err
		}
		result.SortLockSignals = appendSortLockSignals(nil, srcItems, cmd.SourceListType, cmd.SourceSlotIndex)
		result.SortLockSignals = appendSortLockSignals(result.SortLockSignals, dstItems, cmd.DestinationListType, cmd.DestinationSlotIndex)
		if moveTouchesMainQuickSlot(cmd) {
			if err := o.validateMainQuickSlotCharmExclusivity(character); err != nil {
				return err
			}
		}
		return saveAccountCargoMove(ctx, accounts, characters, character, cargo, srcField, dstField, sourceCargo, destinationCargo, result)
	})
	if err != nil {
		return MoveResult{}, err
	}
	return result, nil
}

// sortAccountCargo 整理账号金库（list 12）。
// AccountInventory 是 list 12 唯一的 durable owner，容量来自账号 metadata 的
// account_cargo_level，因此段范围只能在加载账号之后确定；角色 Warehouse 里的
// 12:* 残留同样不读不写。整理后的 op13 快照由 bridge 的账号列表构造器重建。
func (o *Owner) sortAccountCargo(ctx context.Context, characterID string, cmd Command) (SortResult, error) {
	accountID := strings.TrimSpace(cmd.AccountID)
	if accountID == "" {
		return SortResult{}, ErrAccountRequired
	}
	if o == nil || o.accountRepo == nil || o.accountItems == nil || o.accounts == nil {
		return SortResult{}, ErrAccountSharedOwnerUnavailable
	}
	account, found, err := o.accounts.Load(ctx, accountID)
	if err != nil {
		return SortResult{}, err
	}
	if !found {
		return SortResult{}, ErrAccountRequired
	}
	capacity, created := accountCargoCapacity(account)
	if !created {
		return SortResult{}, ErrAccountCargoNotCreated
	}

	start, end, segmentOK := accountCargoSortSegment(cmd.Category, capacity)
	result := SortResult{
		CharacterID: characterID,
		ListType:    listTypeAccountCargo,
		Category:    cmd.Category,
		StartSlot:   start,
		EndSlot:     end,
		Mode:        "noop",
	}
	if !segmentOK {
		return result, nil
	}

	err = o.accountItems.WithinAccountCharacterItems(ctx, accountID, characterID, func(accounts dnfrepo.AccountInventoryRepository, _ dnfrepo.InventoryRepository) error {
		cargo, exists, err := accounts.Load(ctx, accountID)
		if err != nil {
			return err
		}
		if !exists {
			cargo = dnfrepo.AccountInventoryRecord{AccountID: accountID}
		}
		cargo = dnfrepo.CloneAccountInventory(cargo)
		cargo.AccountID = accountID
		if cargo.Slots == nil {
			cargo.Slots = make(map[string]dnfrepo.ItemStack)
		}
		moved, changed, err := sortSegmentItems(cargo.Slots, listTypeAccountCargo, start, end)
		if err != nil {
			return err
		}
		result.MovedCount = moved
		if !changed {
			return nil
		}
		cargo.UpdatedAt = time.Now()
		if err := accounts.Save(ctx, cargo); err != nil {
			return err
		}
		result.Mode = "sort"
		result.Changed = true
		return nil
	})
	if err != nil {
		return SortResult{}, err
	}
	return result, nil
}

// accountCargoSortSegment 给出账号金库整理段。当前 EXE 的金库整理按钮发送
// list=12 category=11（与个人仓库共用的仓库网格分类），段覆盖当前容量内的全部槽位。
func accountCargoSortSegment(category byte, capacity int16) (int16, int16, bool) {
	if category != 11 || capacity <= 0 {
		return 0, 0, false
	}
	return 0, capacity - 1, true
}

func accountCargoCapacity(account dnfrepo.AccountRecord) (int16, bool) {
	metadata := account.Metadata
	level, err := strconv.ParseInt(strings.TrimSpace(metadata["account_cargo_level"]), 10, 64)
	if err != nil || level <= 0 || level > int64(^uint16(0)>>1) {
		return 0, false
	}
	created := strings.EqualFold(strings.TrimSpace(metadata["account_cargo_created"]), "true") || level > 0
	return int16(level), created
}

// accountCargoAllowedAttachTypes 是唯一允许存进账号金库（list 12）的 PVF [attach type]
// 白名单：free（自由交易）、account（帐号绑定）、sealing（封装）。其余取值
// —— trade / trade delete / trade limit / sealing trade / 空值 —— 一律算不可交易，
// 拒绝入库。运营口径 2026-09-21（用户拍板），与 86JP 参考实现
// `InventoryInsertService.IsAccountCargoAttachTypeAllowed` 一致；品级 [rarity] 不参与判断。
var accountCargoAllowedAttachTypes = map[string]struct{}{
	"free":    {},
	"account": {},
	"sealing": {},
}

// accountCargoItemAllowed 报告一件道具能不能写进账号金库。它只看当前运行时 PVF 的
// [attach type]，并且 fail-close：没有解析器、模板解析不出来、或解析结果对不上这次
// 物品行，都按"不可交易"处理 —— 宁可不让进，也绝不猜，与
// dnfbridge.currentDungeonDiscardTradeable 的 fail-close 口径一致。
func (o *Owner) accountCargoItemAllowed(stack dnfrepo.ItemStack) bool {
	if o == nil || o.stackMerge == nil || stack.ItemID <= 0 {
		return false
	}
	resolution, err := o.stackMerge(stack.ItemID)
	if err != nil || resolution.ItemID != stack.ItemID {
		return false
	}
	_, allowed := accountCargoAllowedAttachTypes[alignedcmd.NormalizeAttachType(resolution.AttachType)]
	return allowed
}

// validateAccountCargoEntry 判定这一次移动会不会把某件道具写进账号金库（list 12），
// 会的话那件道具必须过白名单，否则整笔拒绝（两边都不落库）。
//
// 只看"物品实际落进哪个容器"，方向按客户端声明的端点推：
//   - 目标端是金库、来源不是：正向存入，进金库的是 source 格那件；
//   - 来源端是金库、目标不是，但 source 格是空的：客户端把已有物品写在 dst 端的反向
//     请求（与 2026-09-18 金库内反向上报同形），进金库的是 destination 格那件；
//   - 其余（金库 → 背包/个人仓库的正向取出、金库内部拖动）：没有东西从外面进来。
//
// 取出方向刻意不判：存量里已经躺在金库的非法行必须还能拿出来，否则那件道具会永远
// 卡在 list 12 里搬不动。金库内部（两端都是 list 12）同样不判，整理与挪格照旧。
func (o *Owner) validateAccountCargoEntry(
	cmd Command,
	source dnfrepo.ItemStack,
	sourceOK bool,
	destination dnfrepo.ItemStack,
	destinationOK bool,
) error {
	sourceCargo := cmd.SourceListType == listTypeAccountCargo
	destinationCargo := cmd.DestinationListType == listTypeAccountCargo
	if sourceCargo == destinationCargo {
		return nil
	}
	var entering dnfrepo.ItemStack
	enteringOK := false
	if destinationCargo {
		if sourceOK {
			entering, enteringOK = source, true
		}
	} else if !sourceOK {
		entering, enteringOK = destination, destinationOK
	}
	if !enteringOK || entering.ItemID <= 0 || entering.Count <= 0 {
		return nil
	}
	if o.accountCargoItemAllowed(entering) {
		return nil
	}
	return fmt.Errorf(
		"%w: item=%d count=%d from list=%d slot=%d to list=%d slot=%d",
		ErrAccountCargoItemNotAllowed,
		entering.ItemID,
		entering.Count,
		cmd.SourceListType,
		cmd.SourceSlotIndex,
		cmd.DestinationListType,
		cmd.DestinationSlotIndex,
	)
}

// checkAccountCargoNegativeSlot 只挡非法的负槽位。未解锁栏位（>=capacity）不是拒绝理由：
// 见 moveAccountCargo 顶部的运营口径说明。
func checkAccountCargoNegativeSlot(listType byte, slot int16) error {
	if listType != listTypeAccountCargo || slot >= 0 {
		return nil
	}
	return fmt.Errorf("%w: slot=%d", ErrAccountCargoSlotOutOfRange, slot)
}

// accountCargoSlotUnlocked 判断账号金库栏位是否已解锁（capacity 就是已解锁格数）。
func accountCargoSlotUnlocked(slot int16, capacity int16) bool {
	return slot >= 0 && slot < capacity
}

// accountCargoUnlockedEmptySlot 返回账号金库里第一个已解锁的空格（从第一格开始找）。
// excluded 用来跳过本次请求自己的端点，避免把"目标格"当成可用空格。
func accountCargoUnlockedEmptySlot(
	items map[string]dnfrepo.ItemStack,
	capacity int16,
	excluded ...int16,
) (int16, bool) {
	skip := make(map[int16]struct{}, len(excluded))
	for _, slot := range excluded {
		skip[slot] = struct{}{}
	}
	for slot := int16(0); slot < capacity; slot++ {
		if _, ignored := skip[slot]; ignored {
			continue
		}
		if _, occupied := items[slotKey(listTypeAccountCargo, slot)]; occupied {
			continue
		}
		return slot, true
	}
	return 0, false
}

func accountCargoItemMap(
	character *dnfrepo.InventoryRecord,
	cargo *dnfrepo.AccountInventoryRecord,
	listType byte,
) (map[string]dnfrepo.ItemStack, dnfrepo.InventoryField, bool, error) {
	switch listType {
	case listTypeAccountCargo:
		return cargo.Slots, "", true, nil
	case listTypeMain, listTypePersonalCargo:
		items, field := itemMapForList(character, listType)
		return items, field, false, nil
	default:
		return nil, "", false, fmt.Errorf("%w: account cargo move list=%d", ErrUnsupportedList, listType)
	}
}

func moveAccountCargoMaps(ctx context.Context, o *Owner, characterID string, cmd Command, srcItems, dstItems map[string]dnfrepo.ItemStack, capacity int16) (MoveResult, error) {
	srcKey := slotKey(cmd.SourceListType, cmd.SourceSlotIndex)
	dstKey := slotKey(cmd.DestinationListType, cmd.DestinationSlotIndex)
	result := MoveResult{
		CharacterID:          characterID,
		SourceListType:       cmd.SourceListType,
		SourceSlotIndex:      cmd.SourceSlotIndex,
		DestinationListType:  cmd.DestinationListType,
		DestinationSlotIndex: cmd.DestinationSlotIndex,
		MoveCount:            int64(cmd.MoveCount),
		Mode:                 "noop",
	}
	if cmd.SourceListType == cmd.DestinationListType && srcKey == dstKey {
		return result, nil
	}
	source, sourceOK := srcItems[srcKey]
	destination, destinationOK := dstItems[dstKey]
	if sourceOK {
		source = o.canonicalStackForMove(source)
	}
	if destinationOK {
		destination = o.canonicalStackForMove(destination)
	}
	if !sourceOK && !destinationOK {
		return MoveResult{}, fmt.Errorf("%w: list=%d slot=%d", ErrSlotNotFound, cmd.SourceListType, cmd.SourceSlotIndex)
	}
	if sourceOK && source.Count <= 0 {
		return MoveResult{}, fmt.Errorf("%w: list=%d slot=%d count=%d", ErrSlotNotFound, cmd.SourceListType, cmd.SourceSlotIndex, source.Count)
	}
	// 落库前的第一道闸：这一次移动会不会把一件"不可交易"的道具写进账号金库。
	// 互换/叠加/直放/反向四条分支都在下面，所以判定必须在这里做一次，不能只加在互换前。
	if err := o.validateAccountCargoEntry(cmd, source, sourceOK, destination, destinationOK); err != nil {
		return MoveResult{}, err
	}
	if !sourceOK {
		if destination.Count <= 0 {
			return MoveResult{}, fmt.Errorf("%w: list=%d slot=%d count=%d", ErrSlotNotFound, cmd.DestinationListType, cmd.DestinationSlotIndex, destination.Count)
		}
		moveCount := o.normalizeMoveCount(destination, int64(cmd.MoveCount))
		if moveCount <= 0 {
			return MoveResult{}, fmt.Errorf("%w: list=%d slot=%d count=%d", ErrSlotNotFound, cmd.DestinationListType, cmd.DestinationSlotIndex, destination.Count)
		}
		// 反向：物品从声明的目标格搬进源容器，携带上限按源容器算。
		if err := o.validateCarryLimitOnMove(
			cmd.DestinationListType, cmd.SourceListType, cmd.SourceSlotIndex, srcItems, destination, moveCount,
		); err != nil {
			return MoveResult{}, err
		}
		excludedSlots := []int16{cmd.SourceSlotIndex}
		if cmd.SourceListType == cmd.DestinationListType {
			excludedSlots = append(excludedSlots, cmd.DestinationSlotIndex)
		}
		// 反向落点（声明的 source 端）在未解锁栏位：必须顺延。
		// 现场 2026-09-18：金库内部把已有物品拖到未解锁格，客户端把落点写在 source 端
		// （服务端看到 src 格是空的、dst 格才是物品），旧代码按 reverse_move 直接放了进去。
		skipStackSearch := false
		if cmd.SourceListType == listTypeAccountCargo &&
			!accountCargoSlotUnlocked(cmd.SourceSlotIndex, capacity) {
			// 金库内操作（同容器）：落到未解锁栏位直接回原位，不顺延。
			if cmd.SourceListType == cmd.DestinationListType {
				return MoveResult{}, fmt.Errorf(
					"%w: 金库内拖动落在未解锁栏位 %d，直接回原位",
					ErrMoveDestinationFull,
					cmd.SourceSlotIndex,
				)
			}
			excludeStackSlots := []int16{cmd.SourceSlotIndex}
			if cmd.SourceListType == cmd.DestinationListType {
				excludeStackSlots = append(excludeStackSlots, cmd.DestinationSlotIndex)
			}
			targetSlot, target, stackable := findAccountCargoCompatibleStackSlot(
				o,
				srcItems,
				listTypeAccountCargo,
				destination,
				moveCount,
				capacity,
				excludeStackSlots...,
			)
			if stackable {
				target = mergeIntoStack(target, cmd.SourceListType, targetSlot, destination, moveCount)
				if destination.Count <= moveCount {
					delete(dstItems, dstKey)
				} else {
					destination.Count -= moveCount
					updateStackRawAmount(&destination)
					dstItems[dstKey] = destination
				}
				srcItems[slotKey(cmd.SourceListType, targetSlot)] = target
				result.MoveCount, result.Mode, result.Changed = moveCount, "auto_stack_reverse", true
				result.SourceSlotIndex = targetSlot
				result.RedirectedReservedMainSlot = true
				result = withAccountCargoCharacterRefresh(result, cmd.SourceListType, srcItems, cmd.DestinationListType, dstItems)
				return result, nil
			}
			slot, ok := accountCargoUnlockedEmptySlot(srcItems, capacity, cmd.SourceSlotIndex, cmd.DestinationSlotIndex)
			if !ok {
				return MoveResult{}, fmt.Errorf(
					"%w: 账号金库已解锁的 %d 格全满，落点 %d 未解锁",
					ErrMoveDestinationFull,
					capacity,
					cmd.SourceSlotIndex,
				)
			}
			cmd.SourceSlotIndex = slot
			srcKey = slotKey(cmd.SourceListType, slot)
			result.SourceSlotIndex = slot
			result.RedirectedReservedMainSlot = true
			skipStackSearch = true
		}
		if !skipStackSearch && !isMainItemQuickSlot(cmd.SourceListType, cmd.SourceSlotIndex) {
			if targetSlot, target, ok := findAccountCargoCompatibleStackSlot(
				o,
				srcItems,
				cmd.SourceListType,
				destination,
				moveCount,
				capacity,
				excludedSlots...,
			); ok {
				target = mergeIntoStack(target, cmd.SourceListType, targetSlot, destination, moveCount)
				if destination.Count <= moveCount {
					delete(dstItems, dstKey)
				} else {
					destination.Count -= moveCount
					updateStackRawAmount(&destination)
					dstItems[dstKey] = destination
				}
				srcItems[slotKey(cmd.SourceListType, targetSlot)] = target
				result.MoveCount, result.Mode, result.Changed = moveCount, "auto_stack_reverse", true
				result = withAccountCargoCharacterRefresh(result, cmd.SourceListType, srcItems, cmd.DestinationListType, dstItems)
				return result, nil
			}
		}
		if o.canSplitStack(destination, moveCount) {
			moved := cloneStack(destination)
			moved.Count = moveCount
			updateStackRawAmount(&moved)
			destination.Count -= moveCount
			updateStackRawAmount(&destination)
			dstItems[dstKey] = destination
			srcItems[srcKey] = preparedMoveStack(moved, cmd.DestinationListType, cmd.DestinationSlotIndex, cmd.SourceListType, cmd.SourceSlotIndex)
			result.MoveCount, result.Mode, result.Changed = moveCount, "reverse_split", true
			return result, nil
		}
		delete(dstItems, dstKey)
		srcItems[srcKey] = preparedMoveStack(destination, cmd.DestinationListType, cmd.DestinationSlotIndex, cmd.SourceListType, cmd.SourceSlotIndex)
		result.MoveCount, result.Mode, result.Changed = destination.Count, "reverse_move", true
		return result, nil
	}
	moveCount := o.normalizeMoveCount(source, int64(cmd.MoveCount))
	if moveCount <= 0 {
		return MoveResult{}, fmt.Errorf("%w: list=%d slot=%d count=%d", ErrSlotNotFound, cmd.SourceListType, cmd.SourceSlotIndex, source.Count)
	}
	// 携带上限（PVF [stack limit]）按容器总量算，账号金库同样适用。
	if err := o.validateCarryLimitOnMove(
		cmd.SourceListType, cmd.DestinationListType, cmd.DestinationSlotIndex, dstItems, source, moveCount,
	); err != nil {
		return MoveResult{}, err
	}
	// 落点在未解锁栏位：运营口径 2026-09-18 —— 顺延到已解锁区：
	// ① 已解锁区里同类可叠行（含上限校验，由 findAccountCargoCompatibleStackSlot 判定）；
	// ② 否则第一个已解锁空格（从第一格开始找）；③ 都没有（真的满了）才回"物品栏空间不足"。
	// 真实落点写进 result，ACK 必须回显它，客户端据此重画。
	if cmd.DestinationListType == listTypeAccountCargo &&
		!accountCargoSlotUnlocked(cmd.DestinationSlotIndex, capacity) {
		// 金库内操作（同容器）：落到未解锁栏位**不适用**顺延，直接回原位（拒绝）。
		// 顺延只服务于跨容器搬入（背包/个人仓库 ↔ 金库）。
		if cmd.SourceListType == cmd.DestinationListType {
			return MoveResult{}, fmt.Errorf(
				"%w: 金库内拖动落在未解锁栏位 %d，直接回原位",
				ErrMoveDestinationFull,
				cmd.DestinationSlotIndex,
			)
		}
		// 同容器移动时源格自己就是"同 ID 的行"，必须一起排除，否则会把这一堆叠到自己身上。
		excludedStackSlots := []int16{cmd.DestinationSlotIndex}
		if cmd.SourceListType == cmd.DestinationListType {
			excludedStackSlots = append(excludedStackSlots, cmd.SourceSlotIndex)
		}
		targetSlot, target, stackable := findAccountCargoCompatibleStackSlot(
			o,
			dstItems,
			listTypeAccountCargo,
			source,
			moveCount,
			capacity,
			excludedStackSlots...,
		)
		if !stackable {
			slot, ok := accountCargoUnlockedEmptySlot(dstItems, capacity, cmd.DestinationSlotIndex, cmd.SourceSlotIndex)
			if !ok {
				return MoveResult{}, fmt.Errorf(
					"%w: 账号金库已解锁的 %d 格全满，落点 %d 未解锁",
					ErrMoveDestinationFull,
					capacity,
					cmd.DestinationSlotIndex,
				)
			}
			targetSlot = slot
		}
		cmd.DestinationSlotIndex = targetSlot
		dstKey = slotKey(cmd.DestinationListType, targetSlot)
		if stackable {
			destination, destinationOK = target, true
		} else {
			destination, destinationOK = dnfrepo.ItemStack{}, false
		}
		result.DestinationSlotIndex = targetSlot
		result.RedirectedReservedMainSlot = true
	}
	// 反方向（账号金库 → 个人仓库）也要同一套顺延：个人仓库的解锁范围来自角色容器状态。
	if cmd.DestinationListType == listTypePersonalCargo {
		excludeSlot := int16(-1)
		if cmd.SourceListType == cmd.DestinationListType {
			excludeSlot = cmd.SourceSlotIndex
		}
		resolution := o.resolveLockedPersonalCargoDestination(
			ctx,
			cmd.DestinationListType,
			cmd.DestinationSlotIndex,
			dstItems,
			source,
			moveCount,
			excludeSlot,
			characterID,
		)
		if resolution.Locked && !resolution.Redirected {
			return MoveResult{}, fmt.Errorf(
				"%w: 个人仓库已解锁格全满，落点 %d 未解锁",
				ErrMoveDestinationFull,
				cmd.DestinationSlotIndex,
			)
		}
		if resolution.Redirected {
			cmd.DestinationSlotIndex = resolution.Slot
			dstKey = slotKey(cmd.DestinationListType, resolution.Slot)
			destination, destinationOK = dstItems[dstKey]
			if destinationOK {
				destination = o.canonicalStackForMove(destination)
			}
			result.DestinationSlotIndex = resolution.Slot
			result.RedirectedReservedMainSlot = true
		}
	}
	if !destinationOK {
		excludedSlots := []int16{cmd.DestinationSlotIndex}
		if cmd.SourceListType == cmd.DestinationListType {
			excludedSlots = append(excludedSlots, cmd.SourceSlotIndex)
		}
		if !isMainItemQuickSlot(cmd.DestinationListType, cmd.DestinationSlotIndex) {
			if targetSlot, target, ok := findAccountCargoCompatibleStackSlot(
				o,
				dstItems,
				cmd.DestinationListType,
				source,
				moveCount,
				capacity,
				excludedSlots...,
			); ok {
				target = mergeIntoStack(target, cmd.DestinationListType, targetSlot, source, moveCount)
				if source.Count <= moveCount {
					delete(srcItems, srcKey)
				} else {
					source.Count -= moveCount
					updateStackRawAmount(&source)
					srcItems[srcKey] = source
				}
				dstItems[slotKey(cmd.DestinationListType, targetSlot)] = target
				result.MoveCount, result.Mode, result.Changed = moveCount, "auto_stack", true
				// 真实落点写进 ACK：客户端拖过去的那一格其实没东西，物品并进了另一处
				// 同类堆，回显声明格会让客户端把这一堆画在空格上。
				// 这里不再补角色容器的整 list 快照 —— list-0 的保留槽 0（金币钱包行）
				// 会被一起下发，现场就是"获得了 N 金币"+ 物品行错位；list 12 由 bridge
				// 在 ACK 之后按 AccountInventory 重建，角色侧按 ACK 的真实端点重画即可。
				result.DestinationSlotIndex = targetSlot
				result.RedirectedReservedMainSlot = true
				return result, nil
			}
		}
		if o.canSplitStack(source, moveCount) {
			moved := cloneStack(source)
			moved.Count = moveCount
			updateStackRawAmount(&moved)
			source.Count -= moveCount
			updateStackRawAmount(&source)
			srcItems[srcKey] = source
			dstItems[dstKey] = preparedMoveStack(moved, cmd.SourceListType, cmd.SourceSlotIndex, cmd.DestinationListType, cmd.DestinationSlotIndex)
			result.MoveCount, result.Mode, result.Changed = moveCount, "split", true
			return result, nil
		}
		delete(srcItems, srcKey)
		dstItems[dstKey] = preparedMoveStack(source, cmd.SourceListType, cmd.SourceSlotIndex, cmd.DestinationListType, cmd.DestinationSlotIndex)
		result.MoveCount, result.Mode, result.Changed = source.Count, "move", true
		return result, nil
	}
	if o.canStack(source, destination, moveCount) {
		destination = mergeIntoStack(destination, cmd.DestinationListType, cmd.DestinationSlotIndex, source, moveCount)
		if source.Count <= moveCount {
			delete(srcItems, srcKey)
		} else {
			source.Count -= moveCount
			updateStackRawAmount(&source)
			srcItems[srcKey] = source
		}
		dstItems[dstKey] = destination
		result.MoveCount, result.Mode, result.Changed = moveCount, "stack", true
		return result, nil
	}
	// 目标格被别的物品占着，而且放不下这一堆：跨容器（金库↔背包/仓库）绝不互换。
	// op19 的 ACK 只能报一个目标槽，被换出去的那件客户端永远不知道去了哪，会一直
	// "消失"到小退（账号金库只有一格时放第二件就是这个现象）。改成先另寻落点，
	// 找不到就明确回"空间不足"。
	//
	// 主快捷栏（0:3..8）与账号共享晶体/灵魂槽（0:354..365）刻意**不参与顺延**：这两类
	// 落点不替玩家决定"改放哪儿"，所以它们不走进下面的重定向，而是落到最底下的
	// 跨容器不互换判定 —— 直接取消这次操作。
	if cmd.SourceListType != cmd.DestinationListType &&
		!isMainItemQuickSlot(cmd.DestinationListType, cmd.DestinationSlotIndex) &&
		!dnfrepo.IsAccountSharedInventorySlot(cmd.DestinationListType, cmd.DestinationSlotIndex) {
		if redirect, ok := o.accountCargoOccupiedDestinationRedirect(ctx, cmd, dstItems, destination, source, moveCount, capacity, characterID); ok {
			targetKey := slotKey(cmd.DestinationListType, redirect.Slot)
			result.MoveCount = moveCount
			if redirect.Mode == "stack" {
				target := mergeIntoStack(redirect.Stack, cmd.DestinationListType, redirect.Slot, source, moveCount)
				if source.Count <= moveCount {
					delete(srcItems, srcKey)
				} else {
					source.Count -= moveCount
					updateStackRawAmount(&source)
					srcItems[srcKey] = source
				}
				dstItems[targetKey] = target
				result.Mode = "stack"
			} else if o.canSplitStack(source, moveCount) {
				moved := cloneStack(source)
				moved.Count = moveCount
				updateStackRawAmount(&moved)
				source.Count -= moveCount
				updateStackRawAmount(&source)
				srcItems[srcKey] = source
				dstItems[targetKey] = preparedMoveStack(moved, cmd.SourceListType, cmd.SourceSlotIndex, cmd.DestinationListType, redirect.Slot)
				result.Mode = "split"
			} else {
				delete(srcItems, srcKey)
				dstItems[targetKey] = preparedMoveStack(source, cmd.SourceListType, cmd.SourceSlotIndex, cmd.DestinationListType, redirect.Slot)
				result.Mode = "move"
			}
			result.Changed = true
			result.DestinationSlotIndex = redirect.Slot
			result.RedirectedReservedMainSlot = true
			return result, nil
		}
		return MoveResult{}, fmt.Errorf("%w: list=%d slot=%d item=%d count=%d", ErrMoveDestinationFull, cmd.DestinationListType, cmd.DestinationSlotIndex, source.ItemID, moveCount)
	}
	// 走到这里说明：目标格被别的物品占着、放不下这一堆，而且目标容器里既没有同类可叠格
	// 也没有能落进去的空格。运营口径 2026-09-21：**跨容器一律不互换**。互换会把目标格
	// 那件搬进对方容器（账号金库这边就是"任务道具 / SS 装备跟着被拖进金库"），而 op19 的
	// ACK 只带一个目标槽，客户端不知道被换出去的东西去了哪。背包格 / 个人仓库格在
	// 上面已经按"先同类可叠、再第一个空格"顺延过了；快捷栏（3..8）与账号共享晶体/灵魂槽
	// 刻意不参与顺延，所以这里就是它们的终点：直接取消这次操作，两边数据都不动。
	if cmd.SourceListType != cmd.DestinationListType {
		return MoveResult{}, fmt.Errorf(
			"%w: source_list=%d source_slot=%d destination_list=%d destination_slot=%d item=%d count=%d",
			ErrCrossContainerSwapNotAllowed,
			cmd.SourceListType,
			cmd.SourceSlotIndex,
			cmd.DestinationListType,
			cmd.DestinationSlotIndex,
			source.ItemID,
			source.Count,
		)
	}
	// 金库内部（list 12 ↔ list 12）互换保持原语义：两端都在金库，没有任何东西跨容器搬动。
	srcItems[srcKey] = preparedMoveStack(destination, cmd.DestinationListType, cmd.DestinationSlotIndex, cmd.SourceListType, cmd.SourceSlotIndex)
	dstItems[dstKey] = preparedMoveStack(source, cmd.SourceListType, cmd.SourceSlotIndex, cmd.DestinationListType, cmd.DestinationSlotIndex)
	result.MoveCount, result.Mode, result.Changed = source.Count, "swap", true
	return result, nil
}

// accountCargoOccupiedDestinationRedirect 为"目标格被占"的金库移动另找落点，顺序与
// 角色容器一致：声明格同类可叠格 → 容器里另一处同类可叠格 → 容器里根本没有这件道具
// 的行时才另开一格。找不到就由调用方回空间不足。
func (o *Owner) accountCargoOccupiedDestinationRedirect(
	ctx context.Context,
	cmd Command,
	destinationItems map[string]dnfrepo.ItemStack,
	destination dnfrepo.ItemStack,
	moving dnfrepo.ItemStack,
	moveCount int64,
	capacity int16,
	characterID string,
) (crossContainerDestinationRedirect, bool) {
	if o.canStack(moving, destination, moveCount) {
		return crossContainerDestinationRedirect{Slot: cmd.DestinationSlotIndex, Stack: destination, Mode: "stack"}, true
	}
	if slot, stack, ok := findAccountCargoCompatibleStackSlot(o, destinationItems, cmd.DestinationListType, moving, moveCount, capacity, cmd.DestinationSlotIndex); ok {
		return crossContainerDestinationRedirect{Slot: slot, Stack: stack, Mode: "stack"}, true
	}
	if slot, ok := o.accountCargoEmptyDestinationSlot(ctx, cmd, destinationItems, moving, capacity, characterID); ok {
		return crossContainerDestinationRedirect{Slot: slot, Mode: "move"}, true
	}
	return crossContainerDestinationRedirect{}, false
}

// accountCargoEmptyDestinationSlot 给出目标容器里第一个空格：金库用容量（0..capacity-1），
// 角色容器用已解锁范围（读不到就 fail-close，绝不猜格子）。
func (o *Owner) accountCargoEmptyDestinationSlot(
	ctx context.Context,
	cmd Command,
	destinationItems map[string]dnfrepo.ItemStack,
	moving dnfrepo.ItemStack,
	capacity int16,
	characterID string,
) (int16, bool) {
	if cmd.DestinationListType == listTypeAccountCargo {
		if capacity <= 0 {
			return 0, false
		}
		return emptySlotInSlotRange(destinationItems, listTypeAccountCargo, 0, capacity-1, cmd.DestinationSlotIndex)
	}
	start, end, ok := o.moveDestinationUnlockedSlotRange(ctx, cmd.DestinationListType, moving, characterID)
	if !ok {
		return 0, false
	}
	return emptySlotInSlotRange(destinationItems, cmd.DestinationListType, start, end, cmd.DestinationSlotIndex)
}

func findAccountCargoCompatibleStackSlot(
	o *Owner,
	items map[string]dnfrepo.ItemStack,
	listType byte,
	moving dnfrepo.ItemStack,
	moveCount int64,
	accountCargoCapacity int16,
	excludedSlots ...int16,
) (int16, dnfrepo.ItemStack, bool) {
	if listType != listTypeAccountCargo {
		return o.findCompatibleStackSlot(items, listType, moving, moveCount, excludedSlots...)
	}
	excluded := make(map[int16]struct{}, len(excludedSlots))
	for _, slot := range excludedSlots {
		excluded[slot] = struct{}{}
	}
	for slot := int16(0); slot < accountCargoCapacity; slot++ {
		if _, skip := excluded[slot]; skip {
			continue
		}
		candidate, ok := items[slotKey(listTypeAccountCargo, slot)]
		candidate = o.canonicalStackForMove(candidate)
		if ok && o.canStack(moving, candidate, moveCount) {
			return slot, candidate, true
		}
	}
	return 0, dnfrepo.ItemStack{}, false
}

// withAccountCargoCharacterRefresh freezes only character-owned containers.
// List 12 is rebuilt by dnfbridge from AccountInventory after the op19 ACK.
// Auto-stack can choose a different slot than the empty endpoint named by the
// request, so every other touched container needs an authoritative snapshot.
func withAccountCargoCharacterRefresh(
	result MoveResult,
	sourceListType byte,
	sourceItems map[string]dnfrepo.ItemStack,
	destinationListType byte,
	destinationItems map[string]dnfrepo.ItemStack,
) MoveResult {
	add := func(listType byte, items map[string]dnfrepo.ItemStack) {
		if listType == listTypeAccountCargo {
			return
		}
		if result.Refresh == nil {
			result.Refresh = make(map[byte]map[string]dnfrepo.ItemStack, 2)
		}
		if _, exists := result.Refresh[listType]; exists {
			return
		}
		result.Refresh[listType] = cloneItemListType(items, listType)
		result.RefreshListTypes = append(result.RefreshListTypes, listType)
	}
	add(sourceListType, sourceItems)
	add(destinationListType, destinationItems)
	return result
}

func saveAccountCargoMove(
	ctx context.Context,
	accounts dnfrepo.AccountInventoryRepository,
	characters dnfrepo.InventoryRepository,
	character dnfrepo.InventoryRecord,
	cargo dnfrepo.AccountInventoryRecord,
	sourceField dnfrepo.InventoryField,
	destinationField dnfrepo.InventoryField,
	sourceCargo bool,
	destinationCargo bool,
	result MoveResult,
) error {
	now := time.Now()
	characterFields := make([]dnfrepo.InventoryField, 0, 2)
	if !sourceCargo {
		characterFields = append(characterFields, sourceField)
	}
	if !destinationCargo && (len(characterFields) == 0 || characterFields[0] != destinationField) {
		characterFields = append(characterFields, destinationField)
	}
	if len(characterFields) > 0 {
		character.UpdatedAt = now
		if err := dnfrepo.SaveInventoryFields(ctx, characters, character, characterFields...); err != nil {
			return err
		}
	}
	if sourceCargo || destinationCargo {
		cargo.UpdatedAt = now
		if err := accounts.Save(ctx, cargo); err != nil {
			return err
		}
	}
	return nil
}
