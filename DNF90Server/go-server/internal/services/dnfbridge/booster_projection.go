package dnfbridge

import (
	"fmt"
	"math"
	"sort"
	"strconv"

	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

// grantCurrentBoosterRewardByOwner routes the fixed crystal/soul identities
// into the account-shared list-0 warehouse before falling back to the normal
// reward placement. The account and character writes are committed by the
// same BoosterAssets transaction as source consumption and Epic Book credit.
func grantCurrentBoosterRewardByOwner(
	inventory *dnfrepo.InventoryRecord,
	account *dnfrepo.AccountInventoryRecord,
	source dnfpvf.Source,
	reward currentBoosterReward,
	mainSlotCount uint16,
) ([]string, bool, error) {
	if inventory == nil || account == nil || reward.Definition.ItemID == 0 || reward.Count == 0 {
		return nil, false, errCurrentBoosterPVFInvalid
	}
	remaining := int64(reward.Count)
	accountChanged := false
	// 统一入口（account_shared_grant.go）。礼盒自己还要在账号行上补 last_grant_source
	// 溯源标记 —— 那一步对"合并进已有行"也生效，所以保留在入口之后。
	shared := grantCurrentAccountSharedFirst(account, reward.Definition, reward.Count, "booster_item")
	if shared.Changed {
		accountChanged = true
		key := dnfrepo.AccountSharedInventorySlotKey(shared.Slot)
		stack := account.Slots[key]
		if stack.Extra == nil {
			stack.Extra = make(map[string]string, 6)
		}
		stack.Extra["source"] = "booster_item"
		stack.Extra["last_grant_source"] = "booster_item"
		account.Slots[key] = stack
	}
	remaining = int64(shared.Remainder)
	if remaining == 0 {
		return nil, accountChanged, nil
	}
	if remaining > math.MaxUint32 {
		return nil, false, errCurrentBoosterPVFInvalid
	}
	overflow := reward
	overflow.Count = uint32(remaining)
	keys, err := grantCurrentBoosterReward(inventory, source, overflow, mainSlotCount)
	return keys, accountChanged, err
}
func grantCurrentBoosterReward(
	inventory *dnfrepo.InventoryRecord,
	source dnfpvf.Source,
	reward currentBoosterReward,
	mainSlotCount uint16,
) ([]string, error) {
	if inventory == nil || reward.Definition.ItemID == 0 || reward.Count == 0 {
		return nil, errCurrentBoosterPVFInvalid
	}
	keys := make([]string, 0, 1)
	switch reward.Placement {
	case currentBoosterRewardAvatar:
		for index := uint32(0); index < reward.Count; index++ {
			slot, err := grantCurrentCeraShopAvatar(
				inventory,
				source,
				reward.Definition,
				currentCeraShopProduct{ItemID: reward.Definition.ItemID, Count: 1, Section: "avatar"},
				reward.Option,
			)
			if err != nil {
				return nil, err
			}
			keys = append(keys, currentCeraShopInventorySlotKey(1, slot))
		}
	case currentBoosterRewardPetBody:
		for index := uint32(0); index < reward.Count; index++ {
			slot, err := grantCurrentCeraShopPet(inventory, reward.Definition)
			if err != nil {
				return nil, err
			}
			keys = append(keys, currentCeraShopInventorySlotKey(currentPetInventoryListType, slot))
		}
	case currentBoosterRewardPetArtifact:
		for index := uint32(0); index < reward.Count; index++ {
			slot, err := grantCurrentBoosterPetArtifact(inventory, reward)
			if err != nil {
				return nil, err
			}
			keys = append(keys, currentCeraShopInventorySlotKey(currentPetInventoryListType, slot))
		}
	case currentBoosterRewardPetConsumable:
		slots, err := grantCurrentCeraShopPetConsumable(inventory, reward.Definition, reward.Count)
		if err != nil {
			return nil, err
		}
		for _, slot := range slots {
			keys = append(keys, currentCeraShopInventorySlotKey(currentPetInventoryListType, slot))
		}
	default:
		// 走 grantCurrentNPCShopProduct 而不是 grantCurrentCeraShopProduct：
		// 前者会按 PVF 的 [flag]/[flag gem] 把公会勋章和守护珠路由到 list 38，
		// 后者只会往 list 0 塞。公会地下城奖励礼盒（10156320）的 [booster info]
		// 整张表都是 100380021..100380050 的勋章和 9000x..9012x 的守护珠，全部
		// 属于 list 38 —— 用 list 0 的放置逻辑必然失败，客户端把失败码 4 显示成
		// 「物品栏空间不足」。
		//
		// 调用方的 changedLists 是从 key 里反解出来的 listType，所以这里只要把
		// 正确的 list 写进 key，后续 op14 刷新就自动跟着走。
		//
		// 【合并说明 2026-09-18，来自服务端仓库 87f7e8c】我们这一侧给授予链补了
		// mainSlotCount（可用槽位/开行上限）参数，这里必须继续往下传：该函数在非
		// 勋章分支仍会转调 grantCurrentCeraShopProduct(..., mainSlotCount)，所以
		// 两边改动都生效 —— 勋章/守护珠进 list 38，普通奖励仍受槽位上限约束。
		listType, slots, err := grantCurrentNPCShopProduct(inventory, reward.Definition, reward.Count, mainSlotCount)
		if err != nil {
			return nil, err
		}
		for _, slot := range slots {
			keys = append(keys, currentCeraShopInventorySlotKey(listType, slot))
		}
	}
	for _, key := range keys {
		stack := inventory.Slots[key]
		if stack.Extra == nil {
			stack.Extra = make(map[string]string, 4)
		}
		if reward.Seal {
			stack.Extra["seal_flag"] = "1"
		}
		// [booster equipment upgrade] 指定的强化等级：只作用于**主背包里的装备**。
		// 四个键必须一起写，客户端读的是 RawEntry[0x0A]（本树其它强化路径同样口径，
		// 见 gm_admin.go），所以写完 Extra 还要重建 RawEntry，否则客户端看到的仍是 +0。
		if reward.UpgradeLevel > 0 && reward.Definition.Kind == dungeonDropItemEquipment &&
			reward.Placement == currentBoosterRewardMain {
			level := strconv.Itoa(int(reward.UpgradeLevel))
			stack.Extra["ext_data0"] = level
			stack.Extra["packed_flag_byte"] = level
			stack.Extra["reinforce"] = level
			stack.Extra["upgrade_level"] = level
			listType, slot, ok := parseSceneInventorySlotKey(key)
			if !ok {
				return nil, errCurrentBoosterPVFInvalid
			}
			entry := currentItemListEntryFromStack(listType, slot, stack)
			stack.RawEntry = append([]byte(nil), entry.data[:]...)
		}
		inventory.Slots[key] = stack
	}
	return keys, nil
}

func grantCurrentBoosterPetArtifact(
	inventory *dnfrepo.InventoryRecord,
	reward currentBoosterReward,
) (int16, error) {
	if inventory == nil || reward.Definition.Kind != dungeonDropItemEquipment || reward.Placement != currentBoosterRewardPetArtifact {
		return 0, errCurrentBoosterPVFInvalid
	}
	if inventory.Slots == nil {
		inventory.Slots = make(map[string]dnfrepo.ItemStack)
	}
	for slot := currentBoosterPetArtifactStart; slot <= currentBoosterPetArtifactEnd; slot++ {
		key := currentCeraShopInventorySlotKey(currentPetInventoryListType, slot)
		if _, occupied := inventory.Slots[key]; occupied {
			continue
		}
		extra := map[string]string{
			"source":          "booster_item",
			"item_kind":       "equipment",
			"pvf_path":        reward.Definition.PVFPath,
			"equipment_type":  reward.Definition.EquipmentType,
			"amount_or_count": "0",
		}
		if reward.Seal {
			extra["seal_flag"] = "1"
		}
		stack := dnfrepo.ItemStack{ItemID: int64(reward.Definition.ItemID), Count: 1, Extra: extra}
		if !reward.Definition.ExpirationDate.IsZero() {
			stack, _ = applyCurrentPVFItemExpiration(stack, reward.Definition)
		}
		entry := currentItemListEntryFromStack(currentPetInventoryListType, slot, stack)
		stack.RawEntry = append([]byte(nil), entry.data[:]...)
		inventory.Slots[key] = stack
		return slot, nil
	}
	return 0, fmt.Errorf("%w: pet artifact item=%d range=%d..%d", errDungeonPickupInventoryFull, reward.Definition.ItemID, currentBoosterPetArtifactStart, currentBoosterPetArtifactEnd)
}

func findCurrentBoosterMaterial(
	items map[string]dnfrepo.ItemStack,
	itemID int64,
	count int64,
	sourceKey string,
) string {
	keys := make([]string, 0, len(items))
	for key, stack := range items {
		listType, _, ok := parseSceneInventorySlotKey(key)
		if !ok || listType != dnfrepo.MainInventoryListType || key == sourceKey || stack.ItemID != itemID || stack.Count < count {
			continue
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)
	if len(keys) == 0 {
		return ""
	}
	return keys[0]
}
