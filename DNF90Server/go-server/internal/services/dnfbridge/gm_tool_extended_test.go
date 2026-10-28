package dnfbridge

import (
	"context"
	"errors"
	"testing"

	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
	dnfquest "longheng.io/server/internal/modules/dnf/quest"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
	appkit "longheng.io/server/internal/platform/app"
)

func TestGMExtendedAccountSkillAndInventoryFlow(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	must := func(err error) {
		if err != nil {
			t.Fatal(err)
		}
	}
	must(repositories.Account.Save(ctx, dnfrepo.AccountRecord{
		AccountID: "dnf:extended", HonorExp: 123,
		Metadata: map[string]string{"account_cera": "50", "account_cargo_gold": "70", "account_cargo_level": "8", "account_cargo_created": "true"},
	}))
	must(repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "11", AccountID: "dnf:extended", Name: "Extended", Job: "0", Level: 20,
		Stats: map[string]int64{"gold": 100, "bonus_sp": 10, "bonus_tp": 2},
	}))
	must(repositories.Skill.Save(ctx, dnfrepo.SkillRecord{CharacterID: "11", Points: dnfrepo.SkillPointState{
		TotalSP: 100, RemainingSP: 75, TotalTP: 10, RemainingTP: 7, SyncedLevel: 20,
	}}))
	must(repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{CharacterID: "11",
		Slots:     map[string]dnfrepo.ItemStack{"0:4": {ItemID: 1001, Count: 9}},
		Warehouse: map[string]dnfrepo.ItemStack{"2:3": {ItemID: 2001, Count: 1}},
	}))
	must(repositories.AccountInventory.Save(ctx, dnfrepo.AccountInventoryRecord{AccountID: "dnf:extended",
		Slots: map[string]dnfrepo.ItemStack{"12:0": {ItemID: 3001, Count: 2}},
	}))
	service := NewWithOptions(&appkit.Env{}, ServiceOptions{RepositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true }})
	service.premiumCatalog = &currentPremiumCatalog{crystalCubeIDs: [6]int64{3033, 3034, 3035, 3036, 3037, 3262}}
	itemCatalog, err := newPVFDungeonDropCatalog(dungeonDropCatalogTestSource{
		"monster/monster.lst":      "",
		"equipment/equipment.lst":  "",
		"stackable/stackable.lst":  "3033 `cube/black.stk`\n3034 `cube/white.stk`\n3035 `cube/red.stk`\n3036 `cube/blue.stk`\n3037 `cube/clear.stk`\n3262 `cube/gold.stk`\n",
		"stackable/cube/black.stk": "[name] `黑色小晶体`\n[stackable type] `[material]`\n[stack limit] 4294967295\n",
		"stackable/cube/white.stk": "[name] `白色小晶体`\n[stackable type] `[material]`\n[stack limit] 4294967295\n",
		"stackable/cube/red.stk":   "[name] `红色小晶体`\n[stackable type] `[material]`\n[stack limit] 4294967295\n",
		"stackable/cube/blue.stk":  "[name] `蓝色小晶体`\n[stackable type] `[material]`\n[stack limit] 4294967295\n",
		"stackable/cube/clear.stk": "[name] `无色小晶体`\n[stackable type] `[material]`\n[stack limit] 4294967295\n",
		"stackable/cube/gold.stk":  "[name] `金色小晶体`\n[stackable type] `[material]`\n[stack limit] 4294967295\n",
	})
	if err != nil {
		t.Fatal(err)
	}
	service.pvfItemCatalog = itemCatalog

	account, err := service.GMAccount(ctx, "dnf:extended")
	if err != nil || account.Cera != 50 || account.CargoGold != 70 || account.CargoLevel != 8 || !account.CargoCreated {
		t.Fatalf("GMAccount = %+v, %v", account, err)
	}
	account, err = service.GMSetAccountValue(ctx, "dnf:extended", "cera", 999)
	if err != nil || account.Cera != 999 {
		t.Fatalf("GMSetAccountValue = %+v, %v", account, err)
	}

	detail, err := service.GMSetCharacterSkillBonus(ctx, "11", 30, 5)
	if err != nil || detail.BonusSP != 30 || detail.BonusTP != 5 || detail.Skill.TotalSP != 120 || detail.Skill.RemainingSP != 95 || detail.Skill.TotalTP != 13 || detail.Skill.RemainingTP != 10 {
		t.Fatalf("GMSetCharacterSkillBonus = %+v, %v", detail, err)
	}

	snapshot, err := service.GMInventory(ctx, "11")
	if err != nil || len(snapshot.Items) != 3 {
		t.Fatalf("GMInventory = %+v, %v", snapshot, err)
	}
	snapshot, err = service.GMDeleteInventoryItem(ctx, "11", "inventory", "0:4", 4)
	if err != nil {
		t.Fatal(err)
	}
	var remaining int64
	for _, item := range snapshot.Items {
		if item.Container == "inventory" && item.Key == "0:4" {
			remaining = item.Count
		}
	}
	if remaining != 5 {
		t.Fatalf("remaining inventory count = %d, want 5", remaining)
	}
	cubes, err := service.GMSetAccountCube(ctx, "11", 4, 123456)
	if err != nil || len(cubes.Cubes) != 6 || cubes.Cubes[4].ItemID != 3037 || cubes.Cubes[4].Count != 123456 {
		t.Fatalf("GMSetAccountCube = %+v, %v", cubes, err)
	}
	accountInventory, found, err := repositories.AccountInventory.Load(ctx, "dnf:extended")
	if err != nil || !found {
		t.Fatalf("load repaired account cubes: found=%v err=%v", found, err)
	}
	repaired := accountInventory.Slots[dnfrepo.AccountSharedInventorySlotKey(dnfrepo.CrystalWarehouseFirstSlot+4)]
	if repaired.ItemID != 3037 || repaired.Count != 123456 || len(repaired.RawEntry) != currentItemListEntryWireSize {
		t.Fatalf("repaired cube stack = %+v", repaired)
	}
	if _, err := service.GMClearInventory(ctx, "11", "account"); err != nil {
		t.Fatal(err)
	}
	accountInventory, found, err = repositories.AccountInventory.Load(ctx, "dnf:extended")
	if err != nil || !found || len(accountInventory.Slots) != 0 {
		t.Fatalf("account inventory = %+v found=%v err=%v", accountInventory, found, err)
	}

	service.gameSessions[11] = &gameSession{selectedCharacterID: 11}
	if _, err := service.GMSetAccountValue(ctx, "dnf:extended", "cera", 1); !errors.Is(err, ErrGMCharacterOnline) {
		t.Fatalf("online account write error = %v", err)
	}
	// 2026-09-14 行为变更：在线角色现在允许 GM 删除库存。
	// 旧实现一律返回 ErrGMCharacterOnline，等于"删个东西先把人踢下线"；
	// 现在仓储改动照常提交，客户端那份显示靠提交后的 op14 增量刷新补。
	//
	// 这里挂的是没有 conn 的假会话，所以刷新会以
	// "dnf game connection is unavailable" 失败并被记为 deferred —— 但**不能**
	// 因此让删除本身失败，这条断言就是在钉这个边界。
	if _, err := service.GMDeleteInventoryItem(ctx, "11", "warehouse", "2:3", 0); err != nil {
		t.Fatalf("online warehouse delete should succeed, got %v", err)
	}
	inventory, found, err := repositories.Inventory.Load(ctx, "11")
	if err != nil || !found {
		t.Fatalf("load inventory after online delete: found=%v err=%v", found, err)
	}
	if _, still := inventory.Warehouse["2:3"]; still {
		t.Fatalf("online warehouse delete did not commit: %+v", inventory.Warehouse)
	}
	if _, err := service.GMClearInventory(ctx, "11", "inventory"); err != nil {
		t.Fatalf("online inventory clear should succeed, got %v", err)
	}
	inventory, found, err = repositories.Inventory.Load(ctx, "11")
	if err != nil || !found {
		t.Fatalf("load inventory after online clear: found=%v err=%v", found, err)
	}
	if len(inventory.Slots) != 0 {
		t.Fatalf("online inventory clear did not commit: %+v", inventory.Slots)
	}
}

func TestGMBatchQuestCompletionIsAtomicAndRewardFree(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{CharacterID: "12", AccountID: "dnf:q", Job: "0", Level: 20}); err != nil {
		t.Fatal(err)
	}
	source := gmQuestPVFSource{
		dnfquest.DefaultList: "10 `a.qst`\n11 `b.qst`\n",
		"n_quest/a.qst":      "[name]\n`A`\n[grade]\n`[normal]`\n[type]\n`[clear map]`\n[level]\n1 99\n[job]\n`[all]`\n",
		"n_quest/b.qst":      "[name]\n`B`\n[grade]\n`[normal]`\n[type]\n`[clear map]`\n[level]\n1 99\n[job]\n`[all]`\n[pre required quest]\n10\n",
	}
	index, err := dnfpvf.Build(ctx, source, dnfpvf.BuildOptions{Lists: []string{dnfquest.DefaultList}})
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := dnfquest.Load(ctx, index)
	if err != nil {
		t.Fatal(err)
	}
	service := NewWithOptions(&appkit.Env{}, ServiceOptions{RepositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true }})
	service.questCatalog = catalog
	overview, err := service.GMCompleteQuestBatch(ctx, "12", []int64{10, 11})
	if err != nil || len(overview.Completed) != 2 {
		t.Fatalf("batch overview = %+v, %v", overview, err)
	}
	record, found, err := repositories.Quest.Load(ctx, "12")
	if err != nil || !found || record.States[10].Extra["gm_rewards_granted"] != "false" || record.States[11].Status != "completed" {
		t.Fatalf("quest record = %+v found=%v err=%v", record, found, err)
	}
}
