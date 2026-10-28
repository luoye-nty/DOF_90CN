package dungeon

import (
	"context"
	"testing"
	"time"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

// 翻牌（副本通关卡牌）是玩家最常撞上的晶块/灵魂来源，必须和其余获取途径一样，把六个
// 晶块(354..359)与六个灵魂(360..365)送进账号共享仓（权威表见
// repository/account_shared_materials.go）。这条路过去直接 addCardItem 写角色背包
// —— 与 2026-09-21 NPC 商店金币购买落错槽是同一类漏点。
//
// 既有夹具 seedDungeonOwnerCharacter 种的角色记录**没有 AccountID**，所以这里自己种
// 一个带 AccountID 的角色，外加一条账号仓库记录。
func seedCardRewardAccountCharacter(t *testing.T, ctx context.Context, repos dnfrepo.Group, gold int64) {
	t.Helper()
	if err := repos.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "19",
		AccountID:   "account-1",
		Stats:       map[string]int64{"gold": gold},
	}); err != nil {
		t.Fatalf("seed character: %v", err)
	}
}

func seedCardRewardAccountInventory(t *testing.T, ctx context.Context, repos dnfrepo.Group, slots map[string]dnfrepo.ItemStack) {
	t.Helper()
	if err := repos.AccountInventory.Save(ctx, dnfrepo.AccountInventoryRecord{
		AccountID: "account-1",
		Slots:     slots,
	}); err != nil {
		t.Fatalf("seed account inventory: %v", err)
	}
}

func cardRewardBagCount(t *testing.T, ctx context.Context, repos dnfrepo.Group, itemID int64) int64 {
	t.Helper()
	inventory, found, err := repos.Inventory.Load(ctx, "19")
	if err != nil || !found {
		t.Fatalf("load inventory found=%t err=%v", found, err)
	}
	var total int64
	for _, stack := range inventory.Slots {
		if stack.ItemID == itemID {
			total += stack.Count
		}
	}
	return total
}

func grantCardRewardForTest(t *testing.T, ctx context.Context, repos dnfrepo.Group, reward CardItemReward) CardRewardResult {
	t.Helper()
	owner, err := NewOwner(repos)
	if err != nil {
		t.Fatalf("NewOwner: %v", err)
	}
	result, err := owner.GrantCardReward(ctx, CardRewardCommand{
		CharacterID: "19",
		MainSlots:   24,
		Bundle:      CardRewardBundle{Items: []CardItemReward{reward}},
		UpdatedAt:   time.Unix(1000, 0).UTC(),
	})
	if err != nil {
		t.Fatalf("GrantCardReward: %v", err)
	}
	return result
}

// 通关奖励是无色小晶块 → 必须并进账号晶体仓 0:358，背包里一个都不许有。
func TestGrantCardRewardRoutesClearCubeIntoAccountCell(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	seedCardRewardAccountCharacter(t, ctx, repos, 100)
	// 现场形态：账号晶体仓里已经堆着 43890 个无色。
	seedCardRewardAccountInventory(t, ctx, repos, map[string]dnfrepo.ItemStack{
		"0:358": {ItemID: 3037, Count: 43890},
	})
	if err := repos.Inventory.Save(ctx, dnfrepo.InventoryRecord{CharacterID: "19", Slots: map[string]dnfrepo.ItemStack{}}); err != nil {
		t.Fatalf("seed inventory: %v", err)
	}

	result := grantCardRewardForTest(t, ctx, repos, CardItemReward{
		Stack:     dnfrepo.ItemStack{ItemID: 3037, Count: 20, RawEntry: make([]byte, 0x77)},
		Stackable: true,
	})

	if len(result.ItemSlots) != 1 || result.ItemSlots[0] != 358 {
		t.Fatalf("ItemSlots=%v want the account crystal cell 358", result.ItemSlots)
	}
	account, found, err := repos.AccountInventory.Load(ctx, "account-1")
	if err != nil || !found {
		t.Fatalf("load account inventory found=%t err=%v", found, err)
	}
	if stack := account.Slots["0:358"]; stack.ItemID != 3037 || stack.Count != 43910 {
		t.Fatalf("account crystal cell=%+v want 3037x43910", stack)
	}
	if got := cardRewardBagCount(t, ctx, repos, 3037); got != 0 {
		t.Fatalf("the backpack must stay empty of the cube, got %d", got)
	}
}

// 通关奖励是灵魂 → 落账号灵魂仓 0:360。
func TestGrantCardRewardRoutesSoulIntoAccountCell(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	seedCardRewardAccountCharacter(t, ctx, repos, 0)
	seedCardRewardAccountInventory(t, ctx, repos, map[string]dnfrepo.ItemStack{})
	if err := repos.Inventory.Save(ctx, dnfrepo.InventoryRecord{CharacterID: "19", Slots: map[string]dnfrepo.ItemStack{}}); err != nil {
		t.Fatalf("seed inventory: %v", err)
	}

	result := grantCardRewardForTest(t, ctx, repos, CardItemReward{
		Stack:     dnfrepo.ItemStack{ItemID: 10100115, Count: 2, RawEntry: make([]byte, 0x77)},
		Stackable: true,
	})

	if len(result.ItemSlots) != 1 || result.ItemSlots[0] != 360 {
		t.Fatalf("ItemSlots=%v want the account soul cell 360", result.ItemSlots)
	}
	account, _, err := repos.AccountInventory.Load(ctx, "account-1")
	if err != nil {
		t.Fatalf("load account inventory: %v", err)
	}
	if stack := account.Slots["0:360"]; stack.ItemID != 10100115 || stack.Count != 2 {
		t.Fatalf("account soul cell=%+v want 10100115x2", stack)
	}
	if got := cardRewardBagCount(t, ctx, repos, 10100115); got != 0 {
		t.Fatalf("the backpack must stay empty of the soul, got %d", got)
	}
}

// 行为保持：普通通关奖励（7001）照旧落角色背包，账号记录一点不动。
func TestGrantCardRewardLeavesOrdinaryItemsInTheBag(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	seedCardRewardAccountCharacter(t, ctx, repos, 0)
	seedCardRewardAccountInventory(t, ctx, repos, map[string]dnfrepo.ItemStack{})
	if err := repos.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "19",
		Slots:       map[string]dnfrepo.ItemStack{"0:0": {ItemID: 7001, Count: 2}},
	}); err != nil {
		t.Fatalf("seed inventory: %v", err)
	}

	result := grantCardRewardForTest(t, ctx, repos, CardItemReward{
		Stack:     dnfrepo.ItemStack{ItemID: 7001, Count: 3},
		Stackable: true,
	})

	if len(result.ItemSlots) != 1 || result.ItemSlots[0] != 0 {
		t.Fatalf("ItemSlots=%v want the existing backpack row 0", result.ItemSlots)
	}
	if got := cardRewardBagCount(t, ctx, repos, 7001); got != 5 {
		t.Fatalf("backpack count=%d want 5 (2 + 3)", got)
	}
	account, _, err := repos.AccountInventory.Load(ctx, "account-1")
	if err != nil {
		t.Fatalf("load account inventory: %v", err)
	}
	if len(account.Slots) != 0 {
		t.Fatalf("ordinary rewards must not touch the account warehouse: %+v", account.Slots)
	}
}

// 固定格被**别的**道具占着时绝不驱赶：整件退回角色背包，占用者的行原样不动。
func TestGrantCardRewardFallsBackToBagWhenTheFixedCellIsTaken(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	seedCardRewardAccountCharacter(t, ctx, repos, 0)
	seedCardRewardAccountInventory(t, ctx, repos, map[string]dnfrepo.ItemStack{
		"0:358": {ItemID: 9999, Count: 4},
	})
	if err := repos.Inventory.Save(ctx, dnfrepo.InventoryRecord{CharacterID: "19", Slots: map[string]dnfrepo.ItemStack{}}); err != nil {
		t.Fatalf("seed inventory: %v", err)
	}

	result := grantCardRewardForTest(t, ctx, repos, CardItemReward{
		Stack:     dnfrepo.ItemStack{ItemID: 3037, Count: 7, RawEntry: make([]byte, 0x77)},
		Stackable: true,
	})

	if len(result.ItemSlots) != 1 || result.ItemSlots[0] == 358 {
		t.Fatalf("ItemSlots=%v want a backpack slot (the fixed cell is taken)", result.ItemSlots)
	}
	if got := cardRewardBagCount(t, ctx, repos, 3037); got != 7 {
		t.Fatalf("the whole reward must fall back to the bag, got %d want 7", got)
	}
	account, _, err := repos.AccountInventory.Load(ctx, "account-1")
	if err != nil {
		t.Fatalf("load account inventory: %v", err)
	}
	if got := account.Slots["0:358"]; got.ItemID != 9999 || got.Count != 4 {
		t.Fatalf("foreign occupant was disturbed: %+v", got)
	}
	if len(account.Slots) != 1 {
		t.Fatalf("no extra account rows may be left behind: %+v", account.Slots)
	}
}

// 角色记录里没有 AccountID（或没有账号仓库仓库）时，整条退回角色背包那条既有路径，
// 绝不能拿空 AccountID 去写账号记录。既有夹具正是这种角色，所以这条门必须存在。
func TestGrantCardRewardSkipsRoutingWhenTheCharacterHasNoAccount(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	// 故意用不带 AccountID 的角色（既有夹具的形态）。
	seedDungeonOwnerCharacter(t, ctx, repos, 0)
	if err := repos.Inventory.Save(ctx, dnfrepo.InventoryRecord{CharacterID: "19", Slots: map[string]dnfrepo.ItemStack{}}); err != nil {
		t.Fatalf("seed inventory: %v", err)
	}

	result := grantCardRewardForTest(t, ctx, repos, CardItemReward{
		Stack:     dnfrepo.ItemStack{ItemID: 3037, Count: 5, RawEntry: make([]byte, 0x77)},
		Stackable: true,
	})

	if len(result.ItemSlots) != 1 || result.ItemSlots[0] == 358 {
		t.Fatalf("ItemSlots=%v want a backpack slot when the character has no account id", result.ItemSlots)
	}
	if got := cardRewardBagCount(t, ctx, repos, 3037); got != 5 {
		t.Fatalf("the reward must land in the bag, got %d want 5", got)
	}
	if account, found, err := repos.AccountInventory.Load(ctx, "account-1"); err != nil {
		t.Fatalf("load account inventory: %v", err)
	} else if found && len(account.Slots) != 0 {
		t.Fatalf("no account row may be written without an account id: %+v", account.Slots)
	}
}
