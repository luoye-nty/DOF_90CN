// 放弃任务的道具回收：只回收"属于这个任务"的道具。
//
// 参考端（_86ref/Server/DfoServer/Game/Quests/QuestGiveupItemRecoveryPolicy.cs
// + QuestGiveupApplicationService.cs）的口径：
//
//  1. 候选集 = 本任务的 `[depend give item]` 事件道具 ∪ 本任务的需求道具
//     （`[seeking]` / `[seek n meet npc]` 的 `[int data]`）；
//  2. 只有"是该任务的事件道具"**或**"PVF 家族是 `[quest]`（任务专属）"才真的删；
//     **普通材料（`[material]` 等）一律不动** —— 那些是玩家自己的东西，
//     任务只是数背包里的数量，交付时才扣（见 finish_owner.go 的
//     consumeFinishRequiredItems）；
//  3. 还要给"其它仍在进行中的任务"留够需求量（retainCount），只删超出的部分。
//
// 现场依据（2026-09-22 角色 1111 日常任务 8557「进攻准备」）：
// `[type] [seeking]`、`[int data] 10094602 10`、无 `[depend give item]`，
// 而 10094602（未精炼的念气结晶）是 `[material]` + `[trade]` 的可交易普通材料
// ⇒ 它的回收名单为空：放弃应当成功，且一件都不删。
package quest

import (
	"sort"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

// GiveUpReclaimEntry 是一件"放弃该任务后应当回收"的道具及其保留量。
type GiveUpReclaimEntry struct {
	ItemID int64
	// RetainCount 是其它仍在进行中的任务对该道具的需求量：只删超出这一数量的部分。
	RetainCount int64
}

// GiveUpReclaimedItem 记录一次真实发生的扣减，供日志与后续背包刷新使用。
type GiveUpReclaimedItem struct {
	SlotKey   string
	SlotIndex uint16
	ItemID    int64
	Delta     int64
	PostCount int64
	RawEntry  []byte
}

// PlanGiveUpReclaim 计算放弃 abandonedQuestID 时应回收的道具。
// isQuestItem 判定某道具是否属于 PVF `[quest]` 家族（任务专属、落在任务栏
// 177..232 且不可交易）。nil 视为"没有任何任务专属道具"：放弃仍然成功，只是
// 不回收任何东西 —— 这与参考端在物品元数据不可用时的降级一致。
func PlanGiveUpReclaim(
	catalog *Catalog,
	record dnfrepo.QuestRecord,
	abandonedQuestID int64,
	isQuestItem func(int64) bool,
) []GiveUpReclaimEntry {
	if catalog == nil || abandonedQuestID <= 0 {
		return nil
	}
	definition, known := catalog.Find(abandonedQuestID)
	if !known {
		return nil
	}

	activationCounts := giveUpItemRuleCounts(itemPairRulesFromInts(definition.DependGiveItemData))
	candidates := make(map[int64]struct{}, len(activationCounts)+2)
	for itemID, count := range activationCounts {
		if count > 0 {
			candidates[itemID] = struct{}{}
		}
	}
	for _, rule := range finishDefinitionSeekingRules(definition) {
		if rule.ItemID > 0 && rule.Count > 0 {
			candidates[rule.ItemID] = struct{}{}
		}
	}
	if len(candidates) == 0 {
		return nil
	}

	plan := make([]GiveUpReclaimEntry, 0, len(candidates))
	for itemID := range candidates {
		if _, isActivationItem := activationCounts[itemID]; !isActivationItem &&
			(isQuestItem == nil || !isQuestItem(itemID)) {
			// 普通材料：不是这个任务发的、也不是任务专属 ⇒ 不回收。
			continue
		}
		plan = append(plan, GiveUpReclaimEntry{
			ItemID:      itemID,
			RetainCount: retainedGiveUpItemCount(catalog, record, abandonedQuestID, itemID),
		})
	}
	sort.Slice(plan, func(left, right int) bool { return plan[left].ItemID < plan[right].ItemID })
	return plan
}

// retainedGiveUpItemCount 取"其它进行中任务"对该道具的最大需求量。
func retainedGiveUpItemCount(
	catalog *Catalog,
	record dnfrepo.QuestRecord,
	abandonedQuestID int64,
	itemID int64,
) int64 {
	if catalog == nil || itemID <= 0 {
		return 0
	}
	retain := int64(0)
	for _, questID := range activeGiveUpQuestIDs(record, abandonedQuestID) {
		definition, known := catalog.Find(questID)
		if !known {
			continue
		}
		eventCount := giveUpItemRuleCounts(itemPairRulesFromInts(definition.DependGiveItemData))[itemID]
		seekingCount := giveUpItemRuleCounts(finishDefinitionSeekingRules(definition))[itemID]
		need := eventCount
		if seekingCount > need {
			need = seekingCount
		}
		if need > retain {
			retain = need
		}
	}
	return retain
}

// activeGiveUpQuestIDs 返回除被放弃任务之外的进行中任务 id（States 与 Progress
// 的并集；同一任务在两处出现时只算一次）。
func activeGiveUpQuestIDs(record dnfrepo.QuestRecord, abandonedQuestID int64) []int64 {
	seen := make(map[int64]struct{}, len(record.States)+len(record.Progress))
	ids := make([]int64, 0, len(record.States)+len(record.Progress))
	collect := func(states map[int64]dnfrepo.QuestState) {
		for questID, state := range states {
			if questID <= 0 || questID == abandonedQuestID {
				continue
			}
			if !isActiveQuestStatus(state.Status) {
				continue
			}
			if _, exists := seen[questID]; exists {
				continue
			}
			seen[questID] = struct{}{}
			ids = append(ids, questID)
		}
	}
	collect(record.States)
	collect(record.Progress)
	sort.Slice(ids, func(left, right int) bool { return ids[left] < ids[right] })
	return ids
}

func giveUpItemRuleCounts(rules []RewardItemRule) map[int64]int64 {
	if len(rules) == 0 {
		return nil
	}
	counts := make(map[int64]int64, len(rules))
	for _, rule := range rules {
		if rule.ItemID <= 0 || rule.Count <= 0 {
			continue
		}
		counts[rule.ItemID] += rule.Count
	}
	return counts
}

// applyGiveUpReclaim 按计划扣减角色背包主容器（list 0 非账号共享格）里的道具。
// 只处理计数为正的普通行：计数为 0 的原始行（数量只存在于 0x77 原始行里）不动，
// 避免误删来源不明的行。扣到 0 的行删除，否则改写计数并同步 0x77 原始行。
func applyGiveUpReclaim(
	record *dnfrepo.InventoryRecord,
	plan []GiveUpReclaimEntry,
) []GiveUpReclaimedItem {
	if record == nil || len(plan) == 0 {
		return nil
	}
	type reclaimSlot struct {
		key   string
		index uint16
		stack dnfrepo.ItemStack
	}
	byItem := make(map[int64][]reclaimSlot, len(plan))
	for key, stack := range record.Slots {
		index, ok := parseFinishMainInventorySlotKey(key)
		if !ok || finishSlotIsAccountShared(index) {
			continue
		}
		if stack.ItemID <= 0 || stack.Count <= 0 {
			continue
		}
		byItem[stack.ItemID] = append(byItem[stack.ItemID], reclaimSlot{key: key, index: index, stack: stack})
	}
	for itemID := range byItem {
		slots := byItem[itemID]
		sort.Slice(slots, func(left, right int) bool { return slots[left].index < slots[right].index })
		byItem[itemID] = slots
	}

	reclaimed := make([]GiveUpReclaimedItem, 0, len(plan))
	for _, entry := range plan {
		if entry.ItemID <= 0 {
			continue
		}
		slots := byItem[entry.ItemID]
		if len(slots) == 0 {
			continue
		}
		held := int64(0)
		for _, candidate := range slots {
			held += candidate.stack.Count
		}
		remaining := held - entry.RetainCount
		if remaining <= 0 {
			continue
		}
		for _, candidate := range slots {
			if remaining <= 0 {
				break
			}
			take := candidate.stack.Count
			if take > remaining {
				take = remaining
			}
			post := candidate.stack.Count - take
			receipt := GiveUpReclaimedItem{
				SlotKey:   candidate.key,
				SlotIndex: candidate.index,
				ItemID:    entry.ItemID,
				Delta:     take,
				PostCount: post,
			}
			if post <= 0 {
				delete(record.Slots, candidate.key)
			} else {
				stack := candidate.stack
				stack.Count = post
				stack = finishStackWithUpdatedCount(stack, candidate.index, entry.ItemID, post)
				record.Slots[candidate.key] = stack
				receipt.RawEntry = append([]byte(nil), stack.RawEntry...)
			}
			reclaimed = append(reclaimed, receipt)
			remaining -= take
		}
	}
	if len(reclaimed) == 0 {
		return nil
	}
	return reclaimed
}
