// 本文件提供"一个容器内同一道具 id 的总量不得超过 PVF [stack limit]"这条规则的共享助手。
// 规则口径（项目 owner 定稿）：PVF [stack limit] N 既是单行上限，也是该容器能容纳的该道具
// 总量上限。因此一个容器里同一道具 id 只能存在一行：容器里已经有该道具的行时，入包必须
// 并进那一行，不能再开第二行。[stack limit] 1 表示该容器只能有一件；没有声明
// [stack limit] 的道具（装备等 StackLimit <= 0）不受限。
//
// 容器 = 线协议 list 类型对应的持久化 map：list 0 角色背包（InventoryRecord.Slots）、
// list 2 个人仓库（InventoryRecord.Warehouse）、list 12 账号金库
// （AccountInventoryRecord.Slots），以及 list 1/7/38 和账号共享晶石/灵魂行
// （键形如 "0:354".."0:365"）等同类 map。行键统一为 "<listType>:<slot>"。
//
// 本文件只做统计和计算，不修改任何容器内容，也不做任何 IO。
package repository

import (
	"math"
	"strconv"
	"strings"
)

// ContainerUncappedSpace 是"PVF 没有声明 [stack limit]"时本包返回的剩余空间哨兵。
// 与 runtime PVF 的既有口径一致（见 alignedcmd.StackMergeResolution）：省略
// [stack limit] 的可叠加道具按 MaxInt32 处理，不做总量限制。
const ContainerUncappedSpace int64 = math.MaxInt32

// InventorySlotKey 拼出 <listType>:<slot> 形式的行键。
func InventorySlotKey(listType byte, slot int16) string {
	return strconv.Itoa(int(listType)) + ":" + strconv.FormatInt(int64(slot), 10)
}

// ParseInventorySlotKey 解析 <listType>:<slot> 形式的行键。ok=false 表示键不是
// 该形式（listType 超出 0..255 或 slot 不是整数）。
func ParseInventorySlotKey(key string) (byte, int16, bool) {
	listRaw, slotRaw, ok := strings.Cut(key, ":")
	if !ok {
		return 0, 0, false
	}
	listValue, err := strconv.ParseInt(strings.TrimSpace(listRaw), 10, 16)
	if err != nil || listValue < 0 || listValue > 255 {
		return 0, 0, false
	}
	slotValue, err := strconv.ParseInt(strings.TrimSpace(slotRaw), 10, 16)
	if err != nil {
		return 0, 0, false
	}
	return byte(listValue), int16(slotValue), true
}

// ContainerItemTotal 统计某个容器里同一道具 id 的总量。
// 口径是"这道具在这个容器里一共有多少件"：整理锁、绑定、期限不同的行都算在一起，
// 只要行键的 listType 相同且 Count > 0。
func ContainerItemTotal(items map[string]ItemStack, listType byte, itemID int64) int64 {
	if itemID <= 0 || len(items) == 0 {
		return 0
	}
	total := int64(0)
	for key, stack := range items {
		keyListType, _, ok := ParseInventorySlotKey(key)
		if !ok || keyListType != listType || stack.ItemID != itemID || stack.Count <= 0 {
			continue
		}
		total += stack.Count
	}
	return total
}

// ContainerItemSpace 返回该容器还能再接收多少件该道具：
//
//	stackLimit > 0  -> max(0, stackLimit - ContainerItemTotal(...))
//	stackLimit <= 0 -> ContainerUncappedSpace（PVF 未声明上限，不受限）
//
// 调用方必须用返回的额度封顶本次入包量，并且只能把额度用在这一道具 id 已有的那一行
// （容器里没有该道具的行时才允许占用空格）。
func ContainerItemSpace(items map[string]ItemStack, listType byte, itemID int64, stackLimit int64) int64 {
	if itemID <= 0 {
		return 0
	}
	if stackLimit <= 0 {
		return ContainerUncappedSpace
	}
	space := stackLimit - ContainerItemTotal(items, listType, itemID)
	if space < 0 {
		return 0
	}
	return space
}

// ContainerItemRow 返回该容器里该道具 id 现存的槽位最小的那一行。
// ok=false 表示容器里没有这道具的行（此时并且只有此时才允许占用空格）。
// 多个同 id 行（历史数据）时返回槽位最小的那一行，保证结果确定。
func ContainerItemRow(items map[string]ItemStack, listType byte, itemID int64) (string, ItemStack, bool) {
	if itemID <= 0 || len(items) == 0 {
		return "", ItemStack{}, false
	}
	bestKey := ""
	bestSlot := int16(0)
	var bestStack ItemStack
	for key, stack := range items {
		keyListType, slot, ok := ParseInventorySlotKey(key)
		if !ok || keyListType != listType || stack.ItemID != itemID || stack.Count <= 0 {
			continue
		}
		if bestKey != "" && slot >= bestSlot {
			continue
		}
		bestKey, bestSlot, bestStack = key, slot, stack
	}
	if bestKey == "" {
		return "", ItemStack{}, false
	}
	return bestKey, bestStack, true
}

// CanOpenContainerItemRow 报告该容器是否还能为 itemID 新开一行（占用空格）：
//
//	stackLimit <= 0 -> true（PVF 未声明上限，沿用旧行为，不受本规则约束）
//	stackLimit >  0 -> 仅当容器里还没有该道具的行，并且总量还没到上限
//
// 入包路径的"占用空格"分支必须先过这一关：容器里已经有该道具的行时只能并进那一行，
// 并不下就走该路径既有的溢出/拒绝处理，绝不能开第二行。
func CanOpenContainerItemRow(items map[string]ItemStack, listType byte, itemID int64, stackLimit int64) bool {
	if stackLimit <= 0 || itemID <= 0 {
		return true
	}
	if ContainerItemSpace(items, listType, itemID, stackLimit) <= 0 {
		return false
	}
	_, _, exists := ContainerItemRow(items, listType, itemID)
	return !exists
}
