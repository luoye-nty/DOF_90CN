package dnfbridge

import (
	"errors"
	"math"
	"sort"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

// currentGrantDisjointRewards places one resolved reward set per its fixed
// warehouse identity: the six cube fragment types stack into their fixed
// crystal warehouse slots (354..359) and the six soul types into their fixed
// soul warehouse slots (360..365); every other reward lands in the character
// backpack. Warehouse overflow falls back to the backpack so no reward is
// ever lost. It returns the ACK rows (slot/item/count/granted), whether the
// account warehouse record changed, and an error.
//
// overflow, when non-nil, collects the rows the main bag had no room for
// instead of failing the disassembly; the caller posts them as mail.
func currentGrantDisjointRewards(
	inventory *dnfrepo.InventoryRecord,
	account *dnfrepo.AccountInventoryRecord,
	catalog *pvfDungeonDropCatalog,
	rewards []currentDisjointReward,
	overflow *[]currentDisjointReward,
	mainSlotCount uint16,
) ([]currentDisjointRewardSlot, bool, error) {
	if inventory == nil || account == nil || catalog == nil || len(rewards) == 0 {
		return nil, false, errCurrentDisjointRewardInvalid
	}
	previousCounts := make(map[int16]int64)
	for key, stack := range inventory.Slots {
		listType, slot, ok := parseSceneInventorySlotKey(key)
		if ok && listType == dnfrepo.MainInventoryListType && stack.Count > 0 {
			previousCounts[slot] = stack.Count
		}
	}
	for key, stack := range account.Slots {
		listType, slot, ok := parseSceneInventorySlotKey(key)
		if ok && listType == dnfrepo.MainInventoryListType && stack.Count > 0 {
			previousCounts[slot] = stack.Count
		}
	}
	changed := make(map[int16]uint32)
	accountDirty := false
	for _, reward := range rewards {
		if reward.ItemID == 0 || reward.Count == 0 {
			return nil, false, errCurrentDisjointRewardInvalid
		}
		definition, err := catalog.ResolveItem(reward.ItemID)
		if err != nil || definition.Kind != dungeonDropItemStackable {
			return nil, false, errCurrentDisjointRewardInvalid
		}
		remaining := int64(reward.Count)
		// 统一入口：有固定格就先进账号共享仓，装不下的余量再交给下面的角色侧
		// 溢出（含溢出邮件）。落点判定不再在这里手写一行。
		shared := grantCurrentAccountSharedFirst(account, definition, reward.Count, "dungeon_pvf_disjoint")
		if shared.Changed {
			accountDirty = true
			changed[shared.Slot] = reward.ItemID
		}
		remaining = int64(shared.Remainder)
		if remaining > 0 {
			slots, err := grantCurrentCeraShopProduct(inventory, definition, uint32(remaining), mainSlotCount)
			if err != nil {
				// A full bag used to abort the whole disassembly, so the
				// source survived but the player got nothing and had to clear
				// space first. Hand the row to the caller's overflow sink
				// instead; it posts the rewards as mail inside this same
				// transaction, so the source is never consumed by a commit
				// whose mail did not land.
				if overflow != nil && errors.Is(err, errDungeonPickupInventoryFull) {
					*overflow = append(*overflow, currentDisjointReward{
						ItemID: reward.ItemID,
						Count:  uint32(remaining),
					})
					continue
				}
				return nil, false, err
			}
			for _, slotValue := range slots {
				slot := int16(slotValue)
				stack, found := inventory.Slots[currentCeraShopInventorySlotKey(0, slot)]
				if !found || stack.ItemID != int64(reward.ItemID) || stack.Count <= 0 {
					return nil, false, errCurrentDisjointRewardInvalid
				}
				entry := currentItemListEntryFromStack(0, slot, stack)
				stack.RawEntry = append([]byte(nil), entry.data[:]...)
				inventory.Slots[currentCeraShopInventorySlotKey(0, slot)] = stack
				changed[slot] = reward.ItemID
			}
		}
	}
	out := make([]currentDisjointRewardSlot, 0, len(changed))
	for slot, itemID := range changed {
		stack, found := inventory.Slots[currentCeraShopInventorySlotKey(0, slot)]
		if !found {
			stack, found = account.Slots[currentCeraShopInventorySlotKey(0, slot)]
		}
		if !found || stack.Count <= 0 || stack.Count > math.MaxUint32 {
			return nil, false, errCurrentDisjointRewardInvalid
		}
		added := stack.Count - previousCounts[slot]
		if added <= 0 || added > math.MaxUint32 {
			return nil, false, errCurrentDisjointRewardInvalid
		}
		// Count 与 Granted 是两个不同口径，任何一条弹窗体路径都必须用 Granted：
		// Count 是整堆终值（给 0x14 增量行），Granted 是本次新增量（给弹窗"获得 N 个"）。
		// 两者只在背包里本来没有该道具时数值相同 —— 这正是 op202 早先发错字段却测不出来的原因。
		out = append(out, currentDisjointRewardSlot{Slot: slot, ItemID: itemID, Count: uint32(stack.Count), Granted: uint32(added)})
	}
	sort.Slice(out, func(left, right int) bool { return out[left].Slot < out[right].Slot })
	return out, accountDirty, nil
}

// currentDisjointWarehouseFixedSlots 是**权威表**（dnfrepo.AccountSharedWarehouseFixedSlots）
// 的 uint32 视图：六个晶块占晶体仓库 354..359、六个灵魂占灵魂仓库 360..365，
// 一个道具 ID 一个固定格；元素结晶(3166/3167)与其它所有可叠加道具没有固定格，
// 留在角色背包。
//
// 这里过去是一份手写的 12 条硬编码表，与 inventory/owner.go 的
// upgradeAccountSharedMaterialSlot、equipment_regeneration_pvf.go 的
// equipmentRegenerationAccountSharedItem 是三份彼此独立的副本。现改为从权威表**派生**，
// 结构上不可能再各自漂移；保留 map 形态只是为了让既有调用点零改动。
// 权威表本身、以及"获取必须进 354..365 / 允许手动存进个人仓库与账号金库 /
// 取回仍回 354..365"的完整运营口径，见 repository/account_shared_materials.go。
var currentDisjointWarehouseFixedSlots = func() map[uint32]int16 {
	canonical := dnfrepo.AccountSharedWarehouseFixedSlots()
	out := make(map[uint32]int16, len(canonical))
	for itemID, slot := range canonical {
		out[uint32(itemID)] = slot
	}
	return out
}()

// currentAccountWarehouseFixedSlot 是权威表的只读查询入口，供 op19 落点规则使用：
// 晶体/灵魂类道具（六个晶块 + 六个灵魂）在自己账号里只有一个固定仓库格，
// 落在角色背包里的那一份要改回这个格子。
func currentAccountWarehouseFixedSlot(itemID int64) (int16, bool) {
	return dnfrepo.AccountSharedWarehouseSlotForItem(itemID)
}
