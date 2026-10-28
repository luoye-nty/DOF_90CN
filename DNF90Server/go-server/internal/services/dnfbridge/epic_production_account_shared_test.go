package dnfbridge

import (
	"context"
	"testing"
	"time"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

// 史诗制作（制作机）的**产出**同样要过账号共享落点：无色/灵魂类产物必须先落进
// 354..365，而不是留在角色背包（统一入口见 account_shared_grant.go）。
//
// 本用例是 TestCommitCurrentEpicProductionCompoundConsumesSharedSoulGrantsCatalystAndPersistsCarry
// 的镜像：同一套夹具，只把产出物从催化剂(10163130)换成无色小晶块(3037)，
// 断言产出落进账号晶体仓、背包里一个都没有；材料（账号灵魂仓的传说灵魂）照旧被扣。
func TestEpicProductionCompoundRoutesCubeOutputIntoAccountCell(t *testing.T) {
	const clearCubeItemID = uint32(3037)
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "2", AccountID: "account-1", Level: 55, Stats: map[string]int64{},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.AccountInventory.Save(ctx, dnfrepo.AccountInventoryRecord{
		AccountID: "account-1",
		Slots: map[string]dnfrepo.ItemStack{
			dnfrepo.AccountSharedInventorySlotKey(365): {ItemID: 10158124, Count: 9999},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{CharacterID: "2", Slots: map[string]dnfrepo.ItemStack{}}); err != nil {
		t.Fatal(err)
	}
	service := Service{options: options{accountID: "stale-local-profile"}, repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true }}
	session := &gameSession{accountID: "account-1", selectedCharacterID: 2}
	request := currentEpicProductionCompoundRequest{
		CatalystItemID: clearCubeItemID,
		Materials:      []currentEpicProductionCompoundMaterial{{ItemID: 10158124, Slot: 365, Count: 200}},
	}
	// 配方必须与催化剂同一个 ID（commit 开头就校验 plan.Recipe.ItemID == request.CatalystItemID），
	// 所以复制一份测试配方、只把 ID 换成无色小晶块。
	recipe := currentEpicProductionTestCatalog().catalysts[10163130]
	recipe.ItemID = clearCubeItemID
	plan := currentEpicProductionCompoundPlan{
		Recipe: recipe,
		OutputDefinition: dungeonDropItemDefinition{
			ItemID: clearCubeItemID, Kind: dungeonDropItemStackable, StackableType: "[material]", SlotStart: 97, SlotEnd: 120,
		},
		Contribution: 100000,
	}
	result, err := service.commitCurrentEpicProductionCompound(ctx, session, plan, request, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if result.CatalystItemID != clearCubeItemID || result.OutputCount != 1 {
		t.Fatalf("result=%+v want catalyst=%d output=1", result, clearCubeItemID)
	}

	account, _, err := repositories.AccountInventory.Load(ctx, "account-1")
	if err != nil {
		t.Fatal(err)
	}
	// 产出落进账号晶体仓。
	if stack := account.Slots[dnfrepo.AccountSharedInventorySlotKey(358)]; stack.ItemID != int64(clearCubeItemID) || stack.Count != 1 {
		t.Fatalf("account crystal cell=%+v want 3037x1", stack)
	}
	// 材料照旧从账号灵魂仓扣。
	if got := account.Slots[dnfrepo.AccountSharedInventorySlotKey(365)].Count; got != 9799 {
		t.Fatalf("epic soul count=%d want=9799", got)
	}
	// 核心断言：角色背包里一个无色都不许有。
	inventory, _, err := repositories.Inventory.Load(ctx, "2")
	if err != nil {
		t.Fatal(err)
	}
	for key, stack := range inventory.Slots {
		if stack.ItemID == int64(clearCubeItemID) {
			t.Fatalf("the produced cube must not sit in the backpack: %s=%+v", key, stack)
		}
	}
}

// 行为保持：产出是普通道具（催化剂）时照旧落角色背包、账号仓不被这次产出改动。
func TestEpicProductionCompoundLeavesOrdinaryOutputInTheBag(t *testing.T) {
	const catalystItemID = uint32(10163130)
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "2", AccountID: "account-1", Level: 55, Stats: map[string]int64{},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.AccountInventory.Save(ctx, dnfrepo.AccountInventoryRecord{
		AccountID: "account-1",
		Slots: map[string]dnfrepo.ItemStack{
			dnfrepo.AccountSharedInventorySlotKey(365): {ItemID: 10158124, Count: 9999},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{CharacterID: "2", Slots: map[string]dnfrepo.ItemStack{}}); err != nil {
		t.Fatal(err)
	}
	service := Service{options: options{accountID: "stale-local-profile"}, repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true }}
	session := &gameSession{accountID: "account-1", selectedCharacterID: 2}
	request := currentEpicProductionCompoundRequest{
		CatalystItemID: catalystItemID,
		Materials:      []currentEpicProductionCompoundMaterial{{ItemID: 10158124, Slot: 365, Count: 200}},
	}
	plan := currentEpicProductionCompoundPlan{
		Recipe: currentEpicProductionTestCatalog().catalysts[catalystItemID],
		OutputDefinition: dungeonDropItemDefinition{
			ItemID: catalystItemID, Kind: dungeonDropItemStackable, StackableType: "[material]", SlotStart: 97, SlotEnd: 120,
		},
		Contribution: 100000,
	}
	if _, err := service.commitCurrentEpicProductionCompound(ctx, session, plan, request, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	inventory, _, err := repositories.Inventory.Load(ctx, "2")
	if err != nil {
		t.Fatal(err)
	}
	if got := inventory.Slots["0:97"]; got.ItemID != int64(catalystItemID) || got.Count != 1 {
		t.Fatalf("ordinary output must stay in the backpack: %+v", got)
	}
	account, _, err := repositories.AccountInventory.Load(ctx, "account-1")
	if err != nil {
		t.Fatal(err)
	}
	// 账号仓只有灵魂仓那一行（材料被扣过），没有新增的产出行。
	if len(account.Slots) != 1 {
		t.Fatalf("no account row may be added for an ordinary output: %+v", account.Slots)
	}
}
