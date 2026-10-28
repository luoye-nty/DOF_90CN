package dnfbridge

import (
	"testing"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

// 副职业的两条发放路径（配方产出 / 提取产物）必须和其余获取途径一样，把六个晶块
// (354..359) 与六个灵魂(360..365)送进账号共享仓。它们过去直接调
// grantCurrentCeraShopProduct，产物会留在角色背包 —— 与 2026-09-21 NPC 商店金币购买
// 落错槽是同一类漏点。
//
// 这条路径没有既有的 op 级夹具（PlanExtraction 的配置只在 expertjob 包内可构造），
// 所以把两处发放抽成下面的小函数直接测；抽函数同时让 op 闭包变短。

func expertJobGrantDefinition(itemID uint32, stackLimit int64) dungeonDropItemDefinition {
	return dungeonDropItemDefinition{
		ItemID:        itemID,
		Kind:          dungeonDropItemStackable,
		PVFPath:       "stackable/material/cubepiece_clear.stk",
		StackableType: "[material]",
		StackLimit:    stackLimit,
		SlotStart:     121,
		SlotEnd:       176,
	}
}

// 提取产物是无色小晶块 → 必须落进账号晶体仓 0:358，背包里一个都不许有。
func TestGrantCurrentExpertJobExtractionRewardRoutesCubeIntoAccountCell(t *testing.T) {
	const (
		clearCube = uint32(3037)
		count     = uint32(20)
	)
	inventory := dnfrepo.InventoryRecord{CharacterID: "19", Slots: map[string]dnfrepo.ItemStack{}}
	account := dnfrepo.AccountInventoryRecord{AccountID: "account-1", Slots: map[string]dnfrepo.ItemStack{
		"0:358": {ItemID: int64(clearCube), Count: 43890},
	}}

	rows, accountChanged, err := grantCurrentExpertJobExtractionReward(&inventory, &account, expertJobGrantDefinition(clearCube, 0), count, 24)
	if err != nil {
		t.Fatalf("grantCurrentExpertJobExtractionReward: %v", err)
	}
	if !accountChanged {
		t.Fatal("accountChanged=false, want true (the account cell absorbed the reward)")
	}
	if len(rows) != 1 || rows[0].Slot != 358 || rows[0].ItemID != clearCube || rows[0].Count != count {
		t.Fatalf("rows=%+v want one row at the account crystal cell 358 with count %d", rows, count)
	}
	if stack := account.Slots["0:358"]; stack.ItemID != int64(clearCube) || stack.Count != 43890+int64(count) {
		t.Fatalf("account crystal cell=%+v want 3037x43910", stack)
	}
	if len(inventory.Slots) != 0 {
		t.Fatalf("character inventory must stay empty: %+v", inventory.Slots)
	}
}

// 固定格装不下时按槽拆分：账号格一条、溢出的背包各行各一条，每条报**该格增量**，
// 与已修好的分解机提取（expert_job_store.go）同一口径。
//
// 注意余量本身也可能跨行：PVF [stack limit] 5 时 6 个单位会落成 5 + 1 两行
// （grantCurrentCeraShopProduct 的行为），所以断言的是**总量与逐槽增量**，
// 而不是"一行装 6 个"。
func TestGrantCurrentExpertJobExtractionRewardSplitsWhenFixedCellIsFull(t *testing.T) {
	const clearCube = uint32(3037)
	inventory := dnfrepo.InventoryRecord{CharacterID: "19", Slots: map[string]dnfrepo.ItemStack{}}
	account := dnfrepo.AccountInventoryRecord{AccountID: "account-1", Slots: map[string]dnfrepo.ItemStack{
		"0:358": {ItemID: int64(clearCube), Count: 4},
	}}

	rows, accountChanged, err := grantCurrentExpertJobExtractionReward(&inventory, &account, expertJobGrantDefinition(clearCube, 5), 7, 24)
	if err != nil {
		t.Fatalf("grantCurrentExpertJobExtractionReward: %v", err)
	}
	if !accountChanged {
		t.Fatal("accountChanged=false, want true")
	}
	if stack := account.Slots["0:358"]; stack.Count != 5 {
		t.Fatalf("account cell=%+v want 3037x5 (capped by the PVF stack limit)", stack)
	}

	// 账号格那条：落在 358，数量 1。
	if len(rows) == 0 || rows[0].Slot != 358 || rows[0].Count != 1 {
		t.Fatalf("rows[0]=%+v want the account cell 358 with the 1 unit it absorbed", rows[0])
	}
	// 背包各行：逐条报该槽增量，合计必须等于余量 6，一个都不能少。
	var bagTotal uint32
	bagSlots := make(map[int16]uint32)
	for _, row := range rows[1:] {
		if row.Slot == 358 {
			t.Fatalf("the account cell must not be repeated: %+v", rows)
		}
		if _, duplicated := bagSlots[row.Slot]; duplicated {
			t.Fatalf("slot %d reported twice: %+v", row.Slot, rows)
		}
		bagSlots[row.Slot] = row.Count
		bagTotal += row.Count
		// 每条的 count 必须与该槽的实际落库增量一致。
		if got := inventory.Slots[currentCeraShopInventorySlotKey(dnfrepo.MainInventoryListType, row.Slot)]; got.Count != int64(row.Count) {
			t.Fatalf("slot %d: row count=%d but the row holds %d: %+v", row.Slot, row.Count, got.Count, rows)
		}
	}
	if bagTotal != 6 {
		t.Fatalf("backpack received %d units across %d rows, want 6: %+v", bagTotal, len(bagSlots), rows)
	}
	// 总量守恒：账号格 5 + 背包 6 = 11。
	if total := account.Slots["0:358"].Count + int64(bagTotal); total != 11 {
		t.Fatalf("units landed=%d want 11 (5 in the cell + 6 in the bag, nothing lost)", total)
	}
}

// 行为保持：非账号共享道具（元素结晶 3166）走原来的角色背包路径，账号记录一点不动。
func TestGrantCurrentExpertJobExtractionRewardLeavesOrdinaryItemsOnTheOldPath(t *testing.T) {
	const elementCrystal = uint32(3166)
	inventory := dnfrepo.InventoryRecord{CharacterID: "19", Slots: map[string]dnfrepo.ItemStack{}}
	account := dnfrepo.AccountInventoryRecord{AccountID: "account-1", Slots: map[string]dnfrepo.ItemStack{}}

	rows, accountChanged, err := grantCurrentExpertJobExtractionReward(&inventory, &account, expertJobGrantDefinition(elementCrystal, 999), 3, 24)
	if err != nil {
		t.Fatalf("grantCurrentExpertJobExtractionReward: %v", err)
	}
	if accountChanged {
		t.Fatal("accountChanged=true for a non-account-shared item")
	}
	if len(account.Slots) != 0 {
		t.Fatalf("account record must not be touched: %+v", account.Slots)
	}
	if len(rows) != 1 || rows[0].Slot != 121 || rows[0].ItemID != elementCrystal || rows[0].Count != 3 {
		t.Fatalf("rows=%+v want one row in the ordinary backpack page", rows)
	}
	if got := inventory.Slots["0:121"]; got.ItemID != int64(elementCrystal) || got.Count != 3 {
		t.Fatalf("backpack row=%+v want 3166x3", got)
	}
}

// 配方产出同样要过账号共享落点（灵魂为例）。
func TestGrantCurrentExpertJobCompoundRewardRoutesSoulIntoAccountCell(t *testing.T) {
	const commonSoul = uint32(10100115)
	inventory := dnfrepo.InventoryRecord{CharacterID: "19", Slots: map[string]dnfrepo.ItemStack{}}
	account := dnfrepo.AccountInventoryRecord{AccountID: "account-1", Slots: map[string]dnfrepo.ItemStack{}}

	accountChanged, err := grantCurrentExpertJobCompoundReward(&inventory, &account, expertJobGrantDefinition(commonSoul, 0), 2, 24)
	if err != nil {
		t.Fatalf("grantCurrentExpertJobCompoundReward: %v", err)
	}
	if !accountChanged {
		t.Fatal("accountChanged=false, want true")
	}
	if stack := account.Slots["0:360"]; stack.ItemID != int64(commonSoul) || stack.Count != 2 {
		t.Fatalf("account soul cell=%+v want 10100115x2", stack)
	}
	if len(inventory.Slots) != 0 {
		t.Fatalf("character inventory must stay empty: %+v", inventory.Slots)
	}
}

// 非账号共享道具在配方产出这条路上同样走原路径。
func TestGrantCurrentExpertJobCompoundRewardLeavesOrdinaryItemsOnTheOldPath(t *testing.T) {
	const elementCrystal = uint32(3166)
	inventory := dnfrepo.InventoryRecord{CharacterID: "19", Slots: map[string]dnfrepo.ItemStack{}}
	account := dnfrepo.AccountInventoryRecord{AccountID: "account-1", Slots: map[string]dnfrepo.ItemStack{}}

	accountChanged, err := grantCurrentExpertJobCompoundReward(&inventory, &account, expertJobGrantDefinition(elementCrystal, 999), 4, 24)
	if err != nil {
		t.Fatalf("grantCurrentExpertJobCompoundReward: %v", err)
	}
	if accountChanged || len(account.Slots) != 0 {
		t.Fatalf("non-member must not touch the account: changed=%t slots=%+v", accountChanged, account.Slots)
	}
	if got := inventory.Slots["0:121"]; got.ItemID != int64(elementCrystal) || got.Count != 4 {
		t.Fatalf("backpack row=%+v want 3166x4", got)
	}
}

// 非法入参 fail-closed，不 panic、不落库。
func TestGrantCurrentExpertJobExtractionRewardRejectsEmptyInputs(t *testing.T) {
	account := dnfrepo.AccountInventoryRecord{AccountID: "account-1", Slots: map[string]dnfrepo.ItemStack{}}
	if _, _, err := grantCurrentExpertJobExtractionReward(nil, &account, expertJobGrantDefinition(3037, 0), 1, 24); err == nil {
		t.Fatal("nil inventory must be rejected")
	}
	inventory := dnfrepo.InventoryRecord{CharacterID: "19", Slots: map[string]dnfrepo.ItemStack{}}
	if _, _, err := grantCurrentExpertJobExtractionReward(&inventory, &account, expertJobGrantDefinition(3037, 0), 0, 24); err == nil {
		t.Fatal("zero count must be rejected")
	}
	if len(account.Slots) != 0 || len(inventory.Slots) != 0 {
		t.Fatalf("rejected calls must not mutate anything: account=%+v inventory=%+v", account.Slots, inventory.Slots)
	}
}
