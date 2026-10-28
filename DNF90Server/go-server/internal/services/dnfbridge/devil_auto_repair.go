package dnfbridge

import (
	"context"
	"fmt"
	"strconv"

	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfequip "longheng.io/server/internal/modules/dnf/equip"
	"longheng.io/server/internal/modules/dnf/premium"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

// 魔王契约「自动修理」的服务端触发点。
//
// 触发语义（用户实测的官方口径）：
//   - 只在身上任意一件装备的耐久**被消耗到 0** 的那一刻触发：本来就挂着一件 0 耐久的
//     装备不会触发，因为那不是一次 >0 → 0 的转变；
//   - 触发后把全身装备（含快捷栏 3..8）修满，不收金币；
//   - 每个角色每天 6 次，当天用完该角色就不再享受，而且**不会退化成收费修理**
//     （玩家并没有点过修理）。
//
// 为什么之前完全不生效：客户端唯一的耐久消耗入口是 legacy cmd=1/type=48
// (handleDungeonDecreaseDurability)。那条路径原先只减 1 点、回一行 op14 就结束了，
// 从不查契约。2026-09-13 的线上证据：
//
//	EVENT kind=game-dungeon-durability-decreased slot=12 item_id=101040517 before=1 after=0
//
// 之后就再没有任何修理动作 —— 契约、次数、免费修理三条逻辑其实都已存在
// (premium.DevilSlotAutoRepair / TryConsumeDaily 上限 6 / Owner.RepairAll(AutoRepair))，
// 缺的只是这个触发点。

const currentDevilAutoRepairTriggerSource = "dungeon_op48_consumed_to_zero"

// applyCurrentDevilAutoRepairOnZeroDurability 在"耐久被消耗到 0"之后尝试发动契约自动
// 修理。它是 best-effort：失败只记日志，绝不破坏 op48 原有的耐久上报链路。
func (s *Service) applyCurrentDevilAutoRepairOnZeroDurability(
	session *gameSession,
	catalog *currentRepairCostCatalog,
	decrease dnfequip.DecreaseDurabilityResult,
) {
	if s == nil || session == nil || session.selectedCharacterID == 0 || catalog == nil {
		return
	}
	// 只认消耗性归零：本就为 0 的装备（Changed=false）与没有真的掉耐久的报告都不触发。
	if !decrease.Changed || decrease.OldDurability == 0 || decrease.NewDurability != 0 {
		return
	}
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.Equipment == nil || repositories.Character == nil || repositories.Account == nil {
		s.logGameEvent(session, "game-devil-auto-repair-blocked",
			"slot", decrease.SlotIndex,
			"item_id", decrease.ItemID,
			"reason", "repositories_unavailable")
		return
	}
	owner, err := dnfequip.NewOwner(repositories)
	if err != nil {
		s.logGameEvent(session, "game-devil-auto-repair-blocked",
			"slot", decrease.SlotIndex,
			"item_id", decrease.ItemID,
			"reason", err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()

	repair, err := owner.RepairAll(ctx, dnfequip.RepairCommand{
		SelectedCharacterID: session.selectedCharacterID,
		AccountID:           s.accountIDForSession(session),
		SlotIndex:           -1,
		AutoRepair:          true,
		ContractAutoRepair:  true,
	}, catalog.resolve)
	if err != nil {
		s.logGameEvent(session, "game-devil-auto-repair-failed",
			"slot", decrease.SlotIndex,
			"item_id", decrease.ItemID,
			"reason", err)
		return
	}
	if repair.ContractSkipped || !repair.FreeRepair {
		// 契约不在有效期，或该角色当天 6 次已用完：什么都不做，也绝不收费。
		s.logGameEvent(session, "game-devil-auto-repair-skipped",
			"slot", decrease.SlotIndex,
			"item_id", decrease.ItemID,
			"daily_limit", premium.DailyLimit(premium.DevilSlotAutoRepair),
			"reason", "contract_inactive_or_daily_limit_reached")
		return
	}
	if repair.RepairedCount == 0 {
		s.logGameEvent(session, "game-devil-auto-repair-noop",
			"slot", decrease.SlotIndex,
			"item_id", decrease.ItemID,
			"reason", "no_repairable_equipment_target")
		return
	}

	equippedCount, bagCount := 0, 0
	for _, repaired := range repair.RepairedSlots {
		if repaired.Equipped {
			equippedCount++
			continue
		}
		bagCount++
	}
	// 只发变更行：这个客户端在副本里收到整张 list 3 会崩，所以沿用 op48 的 delta 载体。
	published, publishErr := s.sendCurrentDevilAutoRepairRows(ctx, session, repair.RepairedSlots)
	usedAfter := int64(0)
	if character, found, loadErr := repositories.Character.Load(ctx, strconv.Itoa(int(session.selectedCharacterID))); loadErr == nil && found {
		usedAfter = premium.DailyUsage(character, premium.DevilSlotAutoRepair, s.gameplayNow())
	}
	s.logGameEvent(session, "game-devil-auto-repair-applied",
		"slot", decrease.SlotIndex,
		"item_id", decrease.ItemID,
		"durability_before", decrease.OldDurability,
		"durability_after", decrease.NewDurability,
		"repaired_count", repair.RepairedCount,
		"equipped_count", equippedCount,
		"bag_count", bagCount,
		"published_rows", published,
		"daily_used_after", usedAfter,
		"daily_limit", premium.DailyLimit(premium.DevilSlotAutoRepair),
		"cost", repair.Cost,
		"gold_after", repair.UpdatedGold,
		"free_repair", repair.FreeRepair,
		"source", currentDevilAutoRepairTriggerSource)
	if publishErr != nil {
		s.logGameEvent(session, "game-devil-auto-repair-publish-failed",
			"slot", decrease.SlotIndex,
			"item_id", decrease.ItemID,
			"reason", publishErr)
		return
	}
	// 让客户端的魔王契约面板显示用掉一次（与其它契约服务同一口径）。
	if stateErr := s.sendCurrentPremiumServiceState(session, "devil_auto_repair_after_zero_durability", currentPremiumServiceTypeForSession(session)); stateErr != nil {
		s.logGameEvent(session, "game-devil-auto-repair-state-refresh-failed",
			"slot", decrease.SlotIndex,
			"reason", stateErr)
	}
}

// sendCurrentDevilAutoRepairRows 把修好的行按容器分组发 op14 delta，返回发出的行数。
// 装备行走 list 3（currentSocketListEquipment，和 op48 同一个载体），
// 快捷栏行走 list 0。两种 list 的 delta 载体都是 msg 14。
func (s *Service) sendCurrentDevilAutoRepairRows(
	ctx context.Context,
	session *gameSession,
	slots []dnfequip.RepairSlot,
) (int, error) {
	if s == nil || session == nil || session.selectedCharacterID == 0 || len(slots) == 0 {
		return 0, nil
	}
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.Equipment == nil {
		return 0, fmt.Errorf("devil auto repair requires equipment repository")
	}
	characterID := strconv.Itoa(int(session.selectedCharacterID))

	equippedSlots := make([]int16, 0, len(slots))
	bagSlots := make([]int16, 0, len(slots))
	for _, repaired := range slots {
		if repaired.Equipped {
			equippedSlots = append(equippedSlots, repaired.SlotIndex)
			continue
		}
		bagSlots = append(bagSlots, repaired.SlotIndex)
	}

	published := 0
	if len(equippedSlots) > 0 {
		equipment, found, err := repositories.Equipment.Load(ctx, characterID)
		if err != nil {
			return published, fmt.Errorf("load equipment for auto repair publish: %w", err)
		}
		rows := make([]currentItemListEntry, 0, len(equippedSlots))
		if found {
			for _, slot := range equippedSlots {
				entry, entryFound := equipment.Entries[strconv.FormatInt(int64(slot), 10)]
				if !entryFound || entry.SlotIndex != slot {
					continue
				}
				projected, projectOK := currentItemListEntryFromEquipment(entry)
				if !projectOK {
					continue
				}
				// op48 上报的是当前客户端的 actor 槽号；历史行可能带旧编号，
				// 这里保留同一个身份，避免客户端把行插到别的格子上。
				projected.setUint16(0, uint16(slot))
				rows = append(rows, projected)
			}
		}
		if len(rows) > 0 {
			body := buildCurrentItemUpdateBody(currentSocketListEquipment, rows)
			if err := s.sendGameUpperRawClass(
				session,
				uint16(dnfenum.CmdPacketWalkoutPartyMember),
				body,
				0,
			); err != nil {
				return published, err
			}
			published += len(rows)
		}
	}

	if len(bagSlots) > 0 && repositories.Inventory != nil {
		inventory, found, err := repositories.Inventory.Load(ctx, characterID)
		if err != nil {
			return published, fmt.Errorf("load inventory for auto repair publish: %w", err)
		}
		rows := make([]currentItemListEntry, 0, len(bagSlots))
		if found {
			for _, slot := range bagSlots {
				stack, stackFound := inventory.Slots[currentDungeonPickupMainSlotKey(slot)]
				if !stackFound || stack.ItemID <= 0 {
					continue
				}
				rows = append(rows, currentItemListEntryFromStack(dnfrepo.MainInventoryListType, slot, stack))
			}
		}
		if len(rows) > 0 {
			body := buildCurrentItemUpdateBody(dnfrepo.MainInventoryListType, rows)
			if err := s.sendGameUpperRawClass(
				session,
				uint16(dnfenum.CmdPacketWalkoutPartyMember),
				body,
				0,
			); err != nil {
				return published, err
			}
			published += len(rows)
		}
	}
	return published, nil
}
