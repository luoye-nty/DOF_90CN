package dnfbridge

import (
	"context"
	"strconv"
	"time"

	"longheng.io/server/internal/modules/dnf/premium"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

// PVF `[daily delete item] 1` —— 每日重置时删除的物品。
//
// # 为什么不能复用 expire_at
//
// 本服**有意屏蔽了物品期限**：cmd/scratch/pvfremoveitemexpiry 从运行时 PVF 摘掉
// `[expiration date]` / `[usable datetime]` / `[usable period]` /
// `[usable period after unsealing]`，cmd/scratch/removeitemexpirydb 清掉库里的
// 期限字段。所以 expire_at 这条通道是被主动关掉的，往它上面盖戳会在下一次跑那两个
// 工具时被一起清掉。
//
// `[daily delete item]` **不在那两个工具的剥离名单里**，在运行时 PVF 里仍然有效 ——
// 客户端把它渲染成"期限 1 天"，而服务端此前完全没解析过这个字段（全仓库 grep
// "daily delete" 零命中），于是这类物品永远不会消失。玩家实测：公会祝福礼盒
// （490001382）与魔界抗疲劳秘药（490001383）显示 1 天期限却一直留在背包里。
//
// 所以这里用**独立的状态位**，不碰 expire_at：把物品被发放（或首次被看见）的服务日
// 记在 stack.Extra 里，跨过服务日边界就删。这样期限屏蔽照旧、每日删除照常生效，
// 两者互不影响。
//
// # 边界
//
// 服务日边界是北京 06:00，复用 premium.ServiceDayID（它把 unix 秒 +2h 再按 24h 分桶，
// 因为 06:00 UTC+8 就是前一civil日的 22:00 UTC）。这和库里现有期限值全都落在
// `22:00:00` 是同一个边界。
//
// # 存量
//
// 已经在背包里、没有戳的旧物品**不当场没收**（运营决定 2026-09-16）：扫描时补盖成
// 当天，于是它们在下一个 06:00 消失。
const (
	// currentDailyDeleteItemStateKey 存放该物品实例被发放/首次看见的服务日 ID。
	currentDailyDeleteItemStateKey = "daily_delete_day"
)

// currentDailyDeleteServiceDay 是当前服务日 ID。
func (s *Service) currentDailyDeleteServiceDay(now time.Time) int64 {
	if now.IsZero() {
		now = s.gameplayNow()
	}
	return premium.ServiceDayID(now.UTC())
}

// stampCurrentDailyDeleteItem 在发放时给每日删除物品打上当前服务日。
// 非每日删除物品原样返回，调用方无需先判断。
func stampCurrentDailyDeleteItem(
	stack dnfrepo.ItemStack,
	definition dungeonDropItemDefinition,
	serviceDay int64,
) (dnfrepo.ItemStack, bool) {
	if !definition.DailyDeleteItem {
		return stack, false
	}
	value := strconv.FormatInt(serviceDay, 10)
	if stack.Extra != nil && stack.Extra[currentDailyDeleteItemStateKey] == value {
		return stack, false
	}
	if stack.Extra == nil {
		stack.Extra = make(map[string]string, 1)
	}
	stack.Extra[currentDailyDeleteItemStateKey] = value
	return stack, true
}

// currentDailyDeleteItemDue 判断一个已打戳的物品是不是该删了。
// 没有戳的返回 (false, false)：调用方应当补盖，而不是当场删除。
func currentDailyDeleteItemDue(stack dnfrepo.ItemStack, serviceDay int64) (due bool, stamped bool) {
	raw, ok := stack.Extra[currentDailyDeleteItemStateKey]
	if !ok {
		return false, false
	}
	stored, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		// 戳坏了就当没有：补盖一次，下一个边界再删。总比凭一个读不懂的值没收东西好。
		return false, false
	}
	return serviceDay > stored, true
}

// sweepSelectedDailyDeleteItems 在选人/背包 bootstrap 时跑一次：跨过服务日边界的
// 每日删除物品就地删掉，没有戳的补盖成当天。
//
// 只解析**背包里实际存在的** item id 的定义（几十个），不扫全 PVF —— 公会合成那次
// 全量 ResolveItem 让第一次操作卡了 8.66 秒，这里不重蹈覆辙。
func (s *Service) sweepSelectedDailyDeleteItems(session *gameSession, source string) {
	if s == nil || session == nil || session.selectedCharacterID == 0 {
		return
	}
	items, err := s.currentPVFItemCatalog()
	if err != nil {
		return
	}
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.CharacterAssets == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	characterID := strconv.Itoa(int(session.selectedCharacterID))
	serviceDay := s.currentDailyDeleteServiceDay(s.gameplayNow())
	now := s.gameplayNow().UTC()

	var removed, stamped []string
	err = repositories.CharacterAssets.WithinCharacterAssets(
		ctx,
		characterID,
		func(
			_ dnfrepo.CharacterRepository,
			inventories dnfrepo.InventoryRepository,
			_ dnfrepo.EquipmentRepository,
		) error {
			record, found, loadErr := inventories.Load(ctx, characterID)
			if loadErr != nil || !found {
				return loadErr
			}
			record = dnfrepo.CloneInventory(record)
			if len(record.Slots) == 0 {
				return nil
			}
			// item id -> 是否每日删除，按背包里出现过的 id 缓存，避免同一个 id
			// 在多个格子里重复解析。
			daily := make(map[int64]bool, 16)
			changed := false
			for key, stack := range record.Slots {
				if stack.ItemID <= 0 || stack.ItemID > int64(^uint32(0)) {
					continue
				}
				isDaily, seen := daily[stack.ItemID]
				if !seen {
					definition, defErr := items.ResolveItem(uint32(stack.ItemID))
					isDaily = defErr == nil && definition.DailyDeleteItem
					daily[stack.ItemID] = isDaily
				}
				if !isDaily {
					continue
				}
				due, hasStamp := currentDailyDeleteItemDue(stack, serviceDay)
				switch {
				case due:
					delete(record.Slots, key)
					removed = append(removed, key)
					changed = true
				case !hasStamp:
					// 存量补盖：不当场没收，下一个服务日边界才消失。
					updated, dirty := stampCurrentDailyDeleteItem(stack, dungeonDropItemDefinition{DailyDeleteItem: true}, serviceDay)
					if dirty {
						record.Slots[key] = updated
						stamped = append(stamped, key)
						changed = true
					}
				}
			}
			if !changed {
				return nil
			}
			record.UpdatedAt = now
			return dnfrepo.SaveInventoryFields(ctx, inventories, record, dnfrepo.InventoryFieldSlots)
		},
	)
	if err != nil {
		s.logGameEvent(session, "game-daily-delete-item-sweep-failed",
			"source", source, "char_id", session.selectedCharacterID, "reason", err)
		return
	}
	if len(removed) == 0 && len(stamped) == 0 {
		return
	}
	s.logGameEvent(session, "game-daily-delete-item-sweep-applied",
		"source", source,
		"char_id", session.selectedCharacterID,
		"service_day", serviceDay,
		"removed", len(removed),
		"removed_keys", removed,
		"stamped", len(stamped),
		"stamped_keys", stamped)
}
