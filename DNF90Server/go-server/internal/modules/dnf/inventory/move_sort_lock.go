// 本文件是 86JP InventoryMoveService 里整理锁定随移动迁移的规则移植。
// 客户端的 padlock 按槽位画，服务端的标记挂在物品行上，两者只有同时处理
// "搬走时该不该保留" 和 "搬完后往哪个槽位补发锁定包" 才不会互相错位。
package inventory

import (
	dnfitemlock "longheng.io/server/internal/modules/dnf/itemlock"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

// SortLockSignal 描述移动后需要向客户端补发的整理锁定槽位。
type SortLockSignal struct {
	ListType  byte
	SlotIndex int16
}

// preparedMoveStack 对齐 86JP PrepareForDestination：被搬到新槽位的物品在不满足
// 保留条件时丢掉整理锁定标记。只用于真正换位的分支（move/split/swap/reverse），
// 合并到已有堆叠时保留的是目标行自己的标记，和 86JP 一致。
func preparedMoveStack(
	stack dnfrepo.ItemStack,
	sourceListType byte,
	sourceSlotIndex int16,
	destinationListType byte,
	destinationSlotIndex int16,
) dnfrepo.ItemStack {
	if shouldPreserveSortLock(stack, sourceListType, sourceSlotIndex, destinationListType, destinationSlotIndex) {
		return stack
	}
	return dnfitemlock.ClearSortLockState(cloneStack(stack))
}

// shouldPreserveSortLock 对齐 86JP ShouldPreserveSortLock，但**跨容器一条按运营口径
// 2026-09-27 改过**：整理锁挂在物品上，玩家把锁好的东西在背包 / 个人仓库 / 账号金库
// 之间搬动时锁必须跟着走。86JP 原本在 sourceListType != destinationListType 时清锁，
// 现场表现就是"仓库里锁好的东西放进背包，锁没了"（用户拍板要求保留）。
func shouldPreserveSortLock(
	stack dnfrepo.ItemStack,
	sourceListType byte,
	sourceSlotIndex int16,
	destinationListType byte,
	destinationSlotIndex int16,
) bool {
	if !dnfitemlock.IsSortLocked(stack) {
		return true
	}
	// 装备栏端点：装备栏不在整理范围内，锁先留在物品上，脱下来还能用。
	if isEquipmentEndpointList(sourceListType) || isEquipmentEndpointList(destinationListType) {
		return true
	}
	// 主快捷栏（0:3..8）端点一律清：它不属于任何"格子段"，整理也不会碰它，
	// 锁在那里没有意义，留着只会在快捷栏上画一个不该有的锁。
	if isMainItemQuickSlot(sourceListType, sourceSlotIndex) ||
		isMainItemQuickSlot(destinationListType, destinationSlotIndex) {
		return false
	}
	// 跨容器：锁跟着物品走，任何容器之间搬动都保留。
	if sourceListType != destinationListType {
		return true
	}
	// 同容器：个人仓库 / 账号金库没有种类段，直接保留。
	if sourceListType == listTypePersonalCargo || sourceListType == listTypeAccountCargo {
		return true
	}
	sourceKind, sourceOK := sortLockSlotKind(sourceListType, sourceSlotIndex)
	destinationKind, destinationOK := sortLockSlotKind(destinationListType, destinationSlotIndex)
	return sourceOK && destinationOK && sourceKind == destinationKind
}

// mergeIntoStack 把 moveCount 件 incoming 并进 destination 堆叠，并让整理锁跟着走：
// 来源行被锁定时目标堆叠也要锁上。
//
// 否则"把锁好的那一件拖进同类堆"就是另一条悄悄洗掉锁的路径：合并分支保留的是
// **目标行**自己的标记，来源行的锁会随着来源行被删除而消失，玩家看到的现象和
// 跨容器搬动丢锁一模一样。合并/自动叠加全仓共 11 处，统一走这里，保证规则只有一份。
//
// 落点与 shouldPreserveSortLock 用同一套排除：主快捷栏（0:3..8）与装备栏端点不挂锁，
// 所以"把锁好的药水拖进快捷栏里已有的那一堆"不会在快捷栏上留下一个锁。
func mergeIntoStack(
	destination dnfrepo.ItemStack,
	destinationListType byte,
	destinationSlotIndex int16,
	incoming dnfrepo.ItemStack,
	moveCount int64,
) dnfrepo.ItemStack {
	destination.Count += moveCount
	updateStackRawAmount(&destination)
	if !dnfitemlock.IsSortLocked(incoming) {
		return destination
	}
	if isMainItemQuickSlot(destinationListType, destinationSlotIndex) || isEquipmentEndpointList(destinationListType) {
		return destination
	}
	return dnfitemlock.SetSortLockState(destination)
}

// appendSortLockSignals 对齐 86JP SendMoveSortLockSignals：移动后仍然锁定的端点槽位
// 需要补发 class1/op713，否则 padlock 会留在物品已经离开的旧槽位上。
func appendSortLockSignals(
	signals []SortLockSignal,
	items map[string]dnfrepo.ItemStack,
	listType byte,
	slotIndex int16,
) []SortLockSignal {
	if !shouldSendSortLockSignal(listType, slotIndex) {
		return signals
	}
	stack, ok := items[slotKey(listType, slotIndex)]
	if !ok || stack.Count <= 0 || !dnfitemlock.IsSortLocked(stack) {
		return signals
	}
	for _, existing := range signals {
		if existing.ListType == listType && existing.SlotIndex == slotIndex {
			return signals
		}
	}
	return append(signals, SortLockSignal{ListType: listType, SlotIndex: slotIndex})
}

// shouldSendSortLockSignal 对齐 86JP ShouldSendSortLockSignal。
func shouldSendSortLockSignal(listType byte, slotIndex int16) bool {
	if isEquipmentEndpointList(listType) {
		return false
	}
	if listType == listTypeMain && isMainItemQuickSlot(listType, slotIndex) {
		return false
	}
	return true
}

func isEquipmentEndpointList(listType byte) bool {
	return listType == listTypeEquipment || listType == listTypeActorWornAlt
}

// moveSortLockSignals 收集移动后两个端点里仍被整理锁定钉住的槽位。
// 客户端的 padlock 按槽位画且不会跟着 op19 走，所以搬完必须按 86JP
// SendMoveSortLockSignals 的做法补发锁定包。
func moveSortLockSignals(record *dnfrepo.InventoryRecord, result MoveResult) []SortLockSignal {
	if record == nil {
		return nil
	}
	signals := make([]SortLockSignal, 0, 2)
	sourceItems, _ := itemMapForList(record, result.SourceListType)
	signals = appendSortLockSignals(signals, sourceItems, result.SourceListType, result.SourceSlotIndex)
	destinationItems, _ := itemMapForList(record, result.DestinationListType)
	signals = appendSortLockSignals(signals, destinationItems, result.DestinationListType, result.DestinationSlotIndex)
	if len(signals) == 0 {
		return nil
	}
	return signals
}

// sortLockSlotKind 对齐 86JP ItemSlotBoundService.TryGetItemKindBySlot：
// 槽位段决定物品种类，个人仓库/账号金库没有种类段（那两条路径在上面已经返回）。
func sortLockSlotKind(listType byte, slotIndex int16) (string, bool) {
	switch listType {
	case listTypeMain:
		return mainSlotKind(slotIndex)
	case listTypeAvatar:
		if slotIndex >= 0 && slotIndex <= 209 {
			return "avatar", true
		}
		return "", false
	case listTypePet:
		switch {
		case slotIndex >= 0 && slotIndex <= 139:
			return "creature", true
		case slotIndex >= 140 && slotIndex <= 188:
			return "creature_equipment", true
		case slotIndex >= 189 && slotIndex <= 239:
			return "creature_consumable", true
		}
		return "", false
	default:
		return "", false
	}
}

func mainSlotKind(slotIndex int16) (string, bool) {
	if isMainItemQuickSlot(listTypeMain, slotIndex) {
		return "unknown", true
	}
	switch {
	case slotIndex >= 9 && slotIndex <= 64:
		return "equipment", true
	case slotIndex >= 65 && slotIndex <= 120:
		return "consumable", true
	case slotIndex >= 121 && slotIndex <= 176:
		return "material", true
	case slotIndex >= 177 && slotIndex <= 232:
		return "quest", true
	case slotIndex >= 233 && slotIndex <= 288:
		return "expert_job_material", true
	case slotIndex >= 289 && slotIndex <= 351:
		return "avatar_emblem", true
	}
	// 按 86JP MainExpandStageFull 的满扩展边界判定分类段；未开放的尾段客户端
	// 根本放不进物品，固定用满扩展不会放宽任何真实判定。
	return "", false
}
