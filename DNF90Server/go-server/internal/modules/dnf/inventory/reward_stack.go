package inventory

import (
	"fmt"
	"math"
	"strconv"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	repo "longheng.io/server/internal/modules/dnf/repository"
)

// PlaceStackableReward stages a PVF-resolved reward without committing it. A
// full inventory leaves the input untouched, including partially filled stacks.
// The caller owns the transaction and its durable reward receipt.
func PlaceStackableReward(record repo.InventoryRecord, item alignedcmd.MagicBoxRewardItem, count int64) (repo.InventoryRecord, error) {
	if count <= 0 || count > math.MaxInt32 || item.StackLimit < 0 || item.StackLimit > math.MaxInt32 || item.SlotEnd > 32766 {
		return repo.InventoryRecord{}, fmt.Errorf("invalid stackable reward contract")
	}
	// An omitted PVF stack limit means unlimited in the existing grant owner;
	// retain that behavior with an explicit bound for the native count field.
	if item.StackLimit == 0 {
		item.StackLimit = math.MaxInt32
	}
	staged := repo.CloneInventory(record)
	if staged.Slots == nil {
		staged.Slots = map[string]repo.ItemStack{}
	}
	if _, err := placeStackableRewardRows(staged.Slots, item, count); err != nil {
		return repo.InventoryRecord{}, err
	}
	return staged, nil
}

// placeStackableRewardRows 按**行上限**放置可叠加奖励：先把范围内已有的同类行补满
// （每行上限 = PVF [stack limit]），再用范围内的空格开新行，最后一行允许不满。
//
// 刻意不复用 owner.go 的 grantRandomRewardItem：本树那个版本把 [stack limit] 同时当作
// "该道具在**整个容器**里的总量上限"（ContainerItemSpace），那是既有的、影响所有随机奖励
// 路径的运营决策，不属于本次决斗场移植范围。决斗场总战绩奖励走参考实现的按行语义，
// 因此这里自带一份放置逻辑，两者互不影响。
func placeStackableRewardRows(items map[string]repo.ItemStack, reward alignedcmd.MagicBoxRewardItem, count int64) ([]int16, error) {
	if count <= 0 {
		return nil, nil
	}
	if reward.ItemID <= 0 || reward.Kind != "stackable" || reward.TargetListType != listTypeMain || reward.SlotStart < 0 || reward.SlotEnd < reward.SlotStart {
		return nil, fmt.Errorf("%w: item=%d kind=%q target_list=%d slots=%d..%d", ErrRandomRewardContractRequired, reward.ItemID, reward.Kind, reward.TargetListType, reward.SlotStart, reward.SlotEnd)
	}
	remaining := count
	changed := make([]int16, 0, 2)
	markChanged := func(slot int16) {
		for _, previous := range changed {
			if previous == slot {
				return
			}
		}
		changed = append(changed, slot)
	}
	// 第一遍：补进范围内已有的同类行。
	for slot := reward.SlotStart; slot <= reward.SlotEnd && remaining > 0; slot++ {
		key := slotKey(listTypeMain, slot)
		stack, found := items[key]
		if !found || stack.ItemID != reward.ItemID || stack.Count <= 0 || reward.StackLimit == 1 || !randomRewardExpirationMatches(stack, reward.ExpireAt) {
			continue
		}
		capacity := remaining
		if reward.StackLimit > 0 {
			capacity = reward.StackLimit - stack.Count
		}
		if capacity <= 0 {
			continue
		}
		add := remaining
		if add > capacity {
			add = capacity
		}
		stack = cloneStack(stack)
		stack.Count += add
		updateStackRawAmount(&stack)
		items[key] = stack
		remaining -= add
		markChanged(slot)
	}
	// 第二遍：占用范围内的空格开新行，每行最多 StackLimit。
	for slot := reward.SlotStart; slot <= reward.SlotEnd && remaining > 0; slot++ {
		key := slotKey(listTypeMain, slot)
		if _, occupied := items[key]; occupied {
			continue
		}
		insert := remaining
		if reward.StackLimit > 0 && insert > reward.StackLimit {
			insert = reward.StackLimit
		}
		stack := repo.ItemStack{
			ItemID:   reward.ItemID,
			Count:    insert,
			ExpireAt: reward.ExpireAt,
			Extra: map[string]string{
				"item_kind": "stackable",
				"pvf_path":  reward.PVFPath,
			},
		}
		if !reward.ExpireAt.IsZero() && reward.ExpireAt.Unix() > 0 {
			expire := strconv.FormatInt(reward.ExpireAt.Unix(), 10)
			stack.Extra["expire_time"] = expire
			stack.Extra["expire_unix"] = expire
		}
		if reward.UsablePeriodDays > 0 {
			stack.Extra["usable_period_days"] = strconv.FormatInt(reward.UsablePeriodDays, 10)
			stack.Extra["expiration_source"] = "runtime_pvf_usable_period_grant"
		}
		updateStackRawAmount(&stack)
		items[key] = stack
		remaining -= insert
		markChanged(slot)
	}
	if remaining > 0 {
		return nil, fmt.Errorf("%w: item=%d remaining=%d", ErrRandomRewardInventoryFull, reward.ItemID, remaining)
	}
	return changed, nil
}
