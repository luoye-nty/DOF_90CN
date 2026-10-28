package dnfbridge

import (
	"errors"
	"strings"
	"testing"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

// 这组回归锁定"一个容器里同一道具 id 的总量不得超过 PVF [stack limit]"在
// 赛丽亚商店/NPC 商店（含 list 38 勋章栏）入包路径上的行为。grantCurrentCeraShopProduct
// 是这些路径共用的入包助手。

func stackLimitGrantDefinition(itemID uint32, stackLimit int64, slotStart, slotEnd int16) dungeonDropItemDefinition {
	return dungeonDropItemDefinition{
		ItemID:        itemID,
		Kind:          dungeonDropItemStackable,
		PVFPath:       "stackable/material/grant_limit.stk",
		StackableType: "[material]",
		StackLimit:    stackLimit,
		SlotStart:     slotStart,
		SlotEnd:       slotEnd,
	}
}

// 容器里已经有这道具的行、还有额度：并进那一行，不另开一行。
func TestGrantCurrentCeraShopProductMergesIntoExistingRowUnderStackLimit(t *testing.T) {
	inventory := dnfrepo.InventoryRecord{Slots: map[string]dnfrepo.ItemStack{
		"0:121": {ItemID: 3227, Count: 5},
	}}
	slots, err := grantCurrentCeraShopProduct(&inventory, stackLimitGrantDefinition(3227, 1000, 121, 176), 3, 24)
	if err != nil {
		t.Fatalf("grantCurrentCeraShopProduct: %v", err)
	}
	if len(slots) != 1 || slots[0] != 121 {
		t.Fatalf("slots=%v, want the existing row 121", slots)
	}
	if len(inventory.Slots) != 1 || inventory.Slots["0:121"].Count != 8 {
		t.Fatalf("inventory=%+v", inventory.Slots)
	}
}

// [stack limit] 1：容器里已经有一件时整包拒绝（既有错误 errDungeonPickupInventoryFull
// 让调用方走邮件等溢出），不许写第二行。
func TestGrantCurrentCeraShopProductRefusesSecondRowForStackLimitOne(t *testing.T) {
	inventory := dnfrepo.InventoryRecord{Slots: map[string]dnfrepo.ItemStack{
		"0:121": {ItemID: 3227, Count: 1},
	}}
	if _, err := grantCurrentCeraShopProduct(&inventory, stackLimitGrantDefinition(3227, 1, 121, 176), 1, 24); !errors.Is(err, errDungeonPickupInventoryFull) {
		t.Fatalf("error=%v, want errDungeonPickupInventoryFull", err)
	}
	if len(inventory.Slots) != 1 || inventory.Slots["0:121"].Count != 1 {
		t.Fatalf("refused grant mutated the container: %+v", inventory.Slots)
	}
}

// 额度按容器总量算：历史拆行合计已经到上限时再入包必须拒绝；还有余量时并进
// 现有行并把容器刚好填到上限。
func TestGrantCurrentCeraShopProductCapsByContainerTotal(t *testing.T) {
	full := dnfrepo.InventoryRecord{Slots: map[string]dnfrepo.ItemStack{
		"0:121": {ItemID: 3227, Count: 4},
		"0:122": {ItemID: 3227, Count: 1},
	}}
	if _, err := grantCurrentCeraShopProduct(&full, stackLimitGrantDefinition(3227, 5, 121, 176), 1, 24); !errors.Is(err, errDungeonPickupInventoryFull) {
		t.Fatalf("error=%v, want errDungeonPickupInventoryFull", err)
	}
	if len(full.Slots) != 2 || full.Slots["0:121"].Count != 4 || full.Slots["0:122"].Count != 1 {
		t.Fatalf("refused grant mutated the container: %+v", full.Slots)
	}

	room := dnfrepo.InventoryRecord{Slots: map[string]dnfrepo.ItemStack{
		"0:121": {ItemID: 3227, Count: 3},
		"0:122": {ItemID: 3227, Count: 1},
	}}
	slots, err := grantCurrentCeraShopProduct(&room, stackLimitGrantDefinition(3227, 5, 121, 176), 1, 24)
	if err != nil {
		t.Fatalf("grantCurrentCeraShopProduct: %v", err)
	}
	if len(slots) != 1 || slots[0] != 121 {
		t.Fatalf("slots=%v, want the lowest existing row", slots)
	}
	if len(room.Slots) != 2 || room.Slots["0:121"].Count != 4 || room.Slots["0:122"].Count != 1 {
		t.Fatalf("inventory=%+v", room.Slots)
	}
}

// 没写 [stack limit] 的道具不受这条约束：容器里那一行不在落点范围内时，
// 仍然可以在范围内开新行（有上限时同一场景必须拒绝）。
func TestGrantCurrentCeraShopProductKeepsUncappedItemsUnrestricted(t *testing.T) {
	uncapped := dnfrepo.InventoryRecord{Slots: map[string]dnfrepo.ItemStack{
		"0:9": {ItemID: 3227, Count: 5},
	}}
	slots, err := grantCurrentCeraShopProduct(&uncapped, stackLimitGrantDefinition(3227, 0, 121, 176), 3, 24)
	if err != nil {
		t.Fatalf("uncapped grantCurrentCeraShopProduct: %v", err)
	}
	if len(slots) != 1 || slots[0] != 121 {
		t.Fatalf("uncapped slots=%v, want a fresh row at 121", slots)
	}
	if stack := uncapped.Slots["0:121"]; stack.ItemID != 3227 || stack.Count != 3 {
		t.Fatalf("uncapped fresh row=%+v", stack)
	}
	if stack := uncapped.Slots["0:9"]; stack.Count != 5 {
		t.Fatalf("uncapped existing row was mutated: %+v", stack)
	}

	// 到容器上限（这里 limit=5，容器里已有 5 件）：既放不进那一行，也不能另开一行，
	// 整笔拒绝并回滚，由调用方走溢出（邮件）。
	capped := dnfrepo.InventoryRecord{Slots: map[string]dnfrepo.ItemStack{
		"0:9": {ItemID: 3227, Count: 5},
	}}
	if _, err := grantCurrentCeraShopProduct(&capped, stackLimitGrantDefinition(3227, 5, 121, 176), 3, 24); !errors.Is(err, errDungeonPickupInventoryFull) {
		t.Fatalf("capped error=%v, want errDungeonPickupInventoryFull", err)
	}
	if len(capped.Slots) != 1 {
		t.Fatalf("capped grant opened a second row: %+v", capped.Slots)
	}
	if stack := capped.Slots["0:9"]; stack.Count != 5 {
		t.Fatalf("capped grant was not rolled back: %+v", stack)
	}
}

// NPC 商店的 list 38 勋章/守护宝石栏是独立容器：同一容器里已经有该道具的行时
// 不再开第二行。
func TestGrantCurrentGuildMedalListProductRefusesSecondRowForStackLimitOne(t *testing.T) {
	definition := dungeonDropItemDefinition{
		ItemID:        702,
		Kind:          dungeonDropItemStackable,
		PVFPath:       "stackable/flag/guild_medal.stk",
		StackableType: "[flag]",
		StackLimit:    1,
		SlotStart:     1,
		SlotEnd:       1,
	}
	key := currentCeraShopInventorySlotKey(currentGuildMedalInventoryListType, currentGuildMedalPageSlotStart)
	inventory := dnfrepo.InventoryRecord{Slots: map[string]dnfrepo.ItemStack{
		key: {ItemID: 702, Count: 1},
	}}
	_, err := grantCurrentGuildMedalListProduct(
		&inventory,
		definition,
		1,
		currentGuildMedalPageSlotStart,
		currentGuildMedalPageSlotStart,
	)
	if !errors.Is(err, errDungeonPickupInventoryFull) {
		t.Fatalf("error=%v, want errDungeonPickupInventoryFull", err)
	}
	if len(inventory.Slots) != 1 || inventory.Slots[key].Count != 1 {
		t.Fatalf("refused medal grant mutated the container: %+v", inventory.Slots)
	}
}

// 守护宝石（同一 list 38 容器）还有额度时并进现有行。
func TestGrantCurrentGuildMedalListProductMergesIntoExistingRow(t *testing.T) {
	definition := dungeonDropItemDefinition{
		ItemID:        606,
		Kind:          dungeonDropItemStackable,
		PVFPath:       "stackable/flag/guardian_gem.stk",
		StackableType: "[flag gem]",
		StackLimit:    100,
		SlotStart:     1,
		SlotEnd:       1,
	}
	key := currentCeraShopInventorySlotKey(currentGuildMedalInventoryListType, currentGuardianGemPageSlotStart)
	inventory := dnfrepo.InventoryRecord{Slots: map[string]dnfrepo.ItemStack{
		key: {ItemID: 606, Count: 2},
	}}
	slots, err := grantCurrentGuildMedalListProduct(
		&inventory,
		definition,
		3,
		currentGuardianGemPageSlotStart,
		currentGuardianGemPageSlotStart,
	)
	if err != nil {
		t.Fatalf("grantCurrentGuildMedalListProduct: %v", err)
	}
	if len(slots) != 1 || int16(slots[0]) != currentGuardianGemPageSlotStart {
		t.Fatalf("slots=%v", slots)
	}
	if len(inventory.Slots) != 1 || inventory.Slots[key].Count != 5 {
		t.Fatalf("inventory=%+v", inventory.Slots)
	}
	for other := range inventory.Slots {
		if !strings.HasPrefix(other, "38:") {
			t.Fatalf("guardian gem leaked into %s", other)
		}
	}
}
