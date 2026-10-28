package dnfbridge

import (
	"fmt"
	"math"
	"strconv"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

// currentGuildMedalListPlacement reports the list-38 page a PVF item natively
// belongs to: [flag] guild medals live on the medal page and [flag gem]
// guardian gems on the gem page. Every other item returns ok=false and keeps
// its ordinary list-0 placement. This reuses the mailbox/GM category rule so
// an NPC-shop (guild exchange) purchase lands where the client looks for it:
// a medal placed in the list-0 equipment page cannot be worn and a gem placed
// in the material page cannot be embedded until the character is reselected
// and the bootstrap re-sorts the rows.
func currentGuildMedalListPlacement(definition dungeonDropItemDefinition) (int16, int16, bool) {
	listType, start, end, err := currentMailboxPVFPlacement(definition)
	if err != nil || listType != currentGuildMedalInventoryListType {
		return 0, 0, false
	}
	return start, end, true
}

// grantCurrentNPCShopProduct places a purchased item into its native list.
// Guild medals and guardian gems go to list 38; everything else keeps the
// existing list-0 Cera-shop grant. The returned list type tells the caller
// which op14 list the changed rows belong to.
// mainSlotCount 是 CharacterContainerState.MainSlotCount（0/8/16/24，即已购买的
// 扩展行数）。它必须一路透传到 grantCurrentCeraShopProduct，否则
// limitCurrentMainInventoryDefinitionSlots 不会被调用，落点会越过已解锁范围落进
// 未解锁的扩展行——客户端不渲染那些格子，物品看起来就"消失"了。
// 取不到容器状态时传 0：unlockedEnd 会算成 slotStart+31，正好是"默认 4 行"，
// 是安全且保守的取值（与赛丽亚商店 placementMainSlotCount 的初值一致）。
func grantCurrentNPCShopProduct(
	inventory *dnfrepo.InventoryRecord,
	definition dungeonDropItemDefinition,
	count uint32,
	mainSlotCount uint16,
) (byte, []int16, error) {
	if start, end, ok := currentGuildMedalListPlacement(definition); ok {
		slots, err := grantCurrentGuildMedalListProduct(inventory, definition, count, start, end)
		if err != nil {
			return 0, nil, err
		}
		return currentGuildMedalInventoryListType, slots, nil
	}
	slots, err := grantCurrentCeraShopProduct(inventory, definition, count, mainSlotCount)
	if err != nil {
		return 0, nil, err
	}
	converted := make([]int16, 0, len(slots))
	for _, slot := range slots {
		converted = append(converted, int16(slot))
	}
	return dnfrepo.MainInventoryListType, converted, nil
}

// grantCurrentGuildMedalListProduct places count units of a [flag] or
// [flag gem] item into the given list-38 page. Equipment occupies one slot per
// unit; stackables first top up compatible existing stacks (same item, not
// bound, same expiration) up to the PVF stack limit and then spill the
// remainder into free slots.
func grantCurrentGuildMedalListProduct(
	inventory *dnfrepo.InventoryRecord,
	definition dungeonDropItemDefinition,
	count uint32,
	slotStart int16,
	slotEnd int16,
) ([]int16, error) {
	if inventory == nil || count == 0 || definition.ItemID == 0 || slotEnd < slotStart ||
		slotStart < currentGuildMedalPageSlotStart || slotEnd > currentGuardianGemPageSlotEnd {
		return nil, errCurrentCeraShopProductUnavailable
	}
	if inventory.Slots == nil {
		inventory.Slots = make(map[string]dnfrepo.ItemStack)
	}
	remaining := count
	changed := make([]int16, 0, 1)
	touched := make(map[int16]struct{})
	appendChanged := func(slot int16) {
		if _, duplicate := touched[slot]; duplicate {
			return
		}
		touched[slot] = struct{}{}
		changed = append(changed, slot)
	}
	// PVF [stack limit] 是这个容器（list 38 勋章/守护宝石栏）里该道具的总量上限：
	// 额度按容器里同 id 总量算，容器里已经有该道具的行时不能再开第二行。
	newRowStackLimit := int64(0)
	space := int64(math.MaxInt32)
	if definition.Kind == dungeonDropItemStackable && definition.StackLimit > 0 {
		newRowStackLimit = definition.StackLimit
		space = dnfrepo.ContainerItemSpace(
			inventory.Slots,
			currentGuildMedalInventoryListType,
			int64(definition.ItemID),
			definition.StackLimit,
		)
	}
	if definition.Kind == dungeonDropItemStackable {
		for slot := slotStart; slot <= slotEnd && remaining > 0; slot++ {
			key := currentCeraShopInventorySlotKey(currentGuildMedalInventoryListType, slot)
			stack, exists := inventory.Slots[key]
			if !exists || stack.ItemID != int64(definition.ItemID) || stack.Bind || stack.Count <= 0 ||
				!currentItemStackExpirationMatches(stack, definition.ExpirationDate) {
				continue
			}
			available := int64(math.MaxInt32) - stack.Count
			if definition.StackLimit > 0 {
				if stack.Count >= definition.StackLimit {
					continue
				}
				available = definition.StackLimit - stack.Count
			}
			if available > space {
				available = space
			}
			if available <= 0 {
				continue
			}
			added := int64(remaining)
			if added > available {
				added = available
			}
			stack.Count += added
			if !definition.ExpirationDate.IsZero() {
				stack, _ = applyCurrentPVFItemExpiration(stack, definition)
			}
			entry := currentItemListEntryFromStack(currentGuildMedalInventoryListType, slot, stack)
			stack.RawEntry = append([]byte(nil), entry.data[:]...)
			inventory.Slots[key] = stack
			remaining -= uint32(added)
			space -= added
			appendChanged(slot)
		}
	}
	for remaining > 0 {
		chunk := remaining
		switch definition.Kind {
		case dungeonDropItemEquipment:
			chunk = 1
		case dungeonDropItemStackable:
			if definition.StackLimit > 0 && int64(chunk) > definition.StackLimit {
				chunk = uint32(definition.StackLimit)
			}
			if int64(chunk) > space {
				chunk = uint32(space)
			}
		default:
			return nil, errCurrentCeraShopProductUnavailable
		}
		if chunk == 0 {
			return nil, fmt.Errorf("%w: guild medal list item=%d range=%d..%d", errDungeonPickupInventoryFull, definition.ItemID, slotStart, slotEnd)
		}
		if !dnfrepo.CanOpenContainerItemRow(
			inventory.Slots,
			currentGuildMedalInventoryListType,
			int64(definition.ItemID),
			newRowStackLimit,
		) {
			return nil, fmt.Errorf("%w: guild medal list item=%d range=%d..%d", errDungeonPickupInventoryFull, definition.ItemID, slotStart, slotEnd)
		}
		slot := int16(-1)
		for candidate := slotStart; candidate <= slotEnd; candidate++ {
			key := currentCeraShopInventorySlotKey(currentGuildMedalInventoryListType, candidate)
			if stack, occupied := inventory.Slots[key]; occupied && stack.ItemID > 0 && stack.Count > 0 {
				continue
			}
			slot = candidate
			break
		}
		if slot < 0 {
			return nil, fmt.Errorf("%w: guild medal list item=%d range=%d..%d", errDungeonPickupInventoryFull, definition.ItemID, slotStart, slotEnd)
		}
		// The generic constructor seeds its fixed row from a list-0 page slot
		// and rejects slot 0; the row is re-projected onto the list-38 slot below.
		seedSlot := definition.SlotStart
		if seedSlot <= 0 {
			seedSlot = 1
		}
		stack, err := buildCurrentDungeonPickupStack(seedSlot, definition, chunk)
		if err != nil {
			return nil, err
		}
		stack.Extra["source"] = "npc_shop_guild_medal_list"
		stack.Extra["list_type"] = strconv.Itoa(int(currentGuildMedalInventoryListType))
		entry := currentItemListEntryFromStack(currentGuildMedalInventoryListType, slot, stack)
		stack.RawEntry = append([]byte(nil), entry.data[:]...)
		inventory.Slots[currentCeraShopInventorySlotKey(currentGuildMedalInventoryListType, slot)] = stack
		remaining -= chunk
		space -= int64(chunk)
		appendChanged(slot)
	}
	return changed, nil
}
