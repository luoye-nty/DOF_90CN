package quest

import (
	"context"
	"errors"
	"testing"

	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

// buildGiveUpCatalog 造两个任务：3145（被放弃的收集类任务）+ 3146（另一个进行中任务）。
func buildGiveUpCatalog(t *testing.T, abandonedExtra, otherExtra string) *Catalog {
	t.Helper()
	source := catalogTestSource{
		DefaultList:          "3145 `giveup.qst`\n3146 `other.qst`\n",
		"n_quest/giveup.qst": questCatalogTestDefinition("[daily]", 1, 99, "[all]", abandonedExtra),
		"n_quest/other.qst":  questCatalogTestDefinition("[daily]", 1, 99, "[all]", otherExtra),
	}
	return buildCatalogFromSource(t, source)
}

func buildCatalogFromSource(t *testing.T, source catalogTestSource) *Catalog {
	t.Helper()
	index, err := dnfpvf.Build(context.Background(), source, dnfpvf.BuildOptions{Lists: []string{DefaultList}})
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := Load(context.Background(), index)
	if err != nil {
		t.Fatal(err)
	}
	return catalog
}

// 普通材料永远不会进回收名单：这正是 2026-09-22 角色 1111 / 任务 8557 的形状
// （`[seeking]` + `[int data] 10094602 10`，而 10094602 是 `[material]` 可交易材料）。
func TestPlanGiveUpReclaimSkipsOrdinaryMaterials(t *testing.T) {
	catalog := buildGiveUpCatalog(t, "[type]\n`[seeking]`\n[int data]\n10094602 10\n", "")
	record := dnfrepo.QuestRecord{CharacterID: "18", States: map[int64]dnfrepo.QuestState{
		3145: {Status: "active"},
	}}
	if plan := PlanGiveUpReclaim(catalog, record, 3145, nil); len(plan) != 0 {
		t.Fatalf("ordinary material reclaim plan = %+v, want empty", plan)
	}
	if plan := PlanGiveUpReclaim(catalog, record, 3145, func(int64) bool { return false }); len(plan) != 0 {
		t.Fatalf("classifier-false reclaim plan = %+v, want empty", plan)
	}
}

// 任务专属（`[quest]` 家族）道具与 `[depend give item]` 事件道具要进回收名单。
func TestPlanGiveUpReclaimIncludesQuestFamilyAndActivationItems(t *testing.T) {
	catalog := buildGiveUpCatalog(t, "[type]\n`[seeking]`\n[int data]\n1001 10\n", "")
	record := dnfrepo.QuestRecord{CharacterID: "18", States: map[int64]dnfrepo.QuestState{
		3145: {Status: "active"},
	}}
	plan := PlanGiveUpReclaim(catalog, record, 3145, func(itemID int64) bool { return itemID == 1001 })
	if len(plan) != 1 || plan[0].ItemID != 1001 || plan[0].RetainCount != 0 {
		t.Fatalf("quest-family reclaim plan = %+v, want item 1001 retain 0", plan)
	}

	catalogEvent := buildGiveUpCatalog(t, "[type]\n`[clear map]`\n[depend give item]\n2002 1\n", "")
	planEvent := PlanGiveUpReclaim(catalogEvent, record, 3145, nil)
	if len(planEvent) != 1 || planEvent[0].ItemID != 2002 {
		t.Fatalf("activation-item reclaim plan = %+v, want item 2002", planEvent)
	}
}

// 其它仍在进行中的任务需要的数量必须留下（参考端的 retainCount）。
func TestPlanGiveUpReclaimRetainsOtherActiveQuestRequirements(t *testing.T) {
	catalog := buildGiveUpCatalog(t,
		"[type]\n`[seeking]`\n[int data]\n1001 10\n",
		"[type]\n`[seeking]`\n[int data]\n1001 3\n")
	record := dnfrepo.QuestRecord{
		CharacterID: "18",
		States: map[int64]dnfrepo.QuestState{
			3145: {Status: "active"},
			3146: {Status: "active"},
			3147: {Status: "completed"}, // 已完成的任务不占额度
		},
	}
	plan := PlanGiveUpReclaim(catalog, record, 3145, func(int64) bool { return true })
	if len(plan) != 1 || plan[0].RetainCount != 3 {
		t.Fatalf("retain plan = %+v, want item 1001 retain 3", plan)
	}

	record.States[3146] = dnfrepo.QuestState{Status: "completed"}
	plan = PlanGiveUpReclaim(catalog, record, 3145, func(int64) bool { return true })
	if len(plan) != 1 || plan[0].RetainCount != 0 {
		t.Fatalf("no-active-successor plan = %+v, want retain 0", plan)
	}
}

// `[cant giveup]` 依然是唯一的拒绝理由；收集类任务不再被拦（本次修复的核心）。
func TestOwnerApplyGiveUpHonorsPVFBoundaryAndAllowsCollectionQuests(t *testing.T) {
	for _, test := range []struct {
		name        string
		extra       string
		wantErr     error
		wantRemoved bool
	}{
		{name: "cant giveup", extra: "[type]\n`[disjoint item]`\n[cant giveup]\n", wantErr: ErrQuestCannotGiveUp},
		{name: "seeking", extra: "[type]\n`[seeking]`\n[int data]\n1001 10\n", wantRemoved: true},
		{name: "get item", extra: "[type]\n`[get item]`\n[int data]\n1001 10\n", wantRemoved: true},
		{name: "seek n meet npc", extra: "[type]\n`[seek n meet npc]`\n[int data]\n1001 10\n", wantRemoved: true},
		{name: "depend give item", extra: "[type]\n`[clear map]`\n[depend give item]\n1001 1\n", wantRemoved: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			repos := dnfrepomemory.NewMemoryGroup()
			if err := repos.Character.Save(ctx, dnfrepo.CharacterRecord{CharacterID: "18", AccountID: "acc-quest"}); err != nil {
				t.Fatal(err)
			}
			if err := repos.Quest.Save(ctx, dnfrepo.QuestRecord{
				CharacterID: "18",
				States:      map[int64]dnfrepo.QuestState{3145: {Status: "active", ProgressValue: 1}},
			}); err != nil {
				t.Fatal(err)
			}
			owner, err := NewOwner(repos)
			if err != nil {
				t.Fatal(err)
			}
			_, err = owner.ApplyGiveUp(
				ctx,
				testAcceptCatalog(t, test.extra),
				NewQuestIDCommand(alignedRequest("acc-quest", 18), "giveup_quest", QuestIDRequest{QuestID: 3145}),
			)
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("ApplyGiveUp() error = %v, want %v", err, test.wantErr)
			}
			stored, ok, loadErr := repos.Quest.Load(ctx, "18")
			if loadErr != nil || !ok {
				t.Fatalf("load quest ok=%t err=%v", ok, loadErr)
			}
			_, exists := stored.States[3145]
			if !test.wantRemoved {
				if !exists || stored.States[3145].Status != "active" {
					t.Fatalf("rejected give-up mutated quest: %+v", stored.States[3145])
				}
				return
			}
			if exists {
				t.Fatalf("allowed give-up left quest active: %+v", stored.States[3145])
			}
		})
	}
}

// 收集类任务 + 普通材料：放弃成功，且背包一件都不动（1111/8557 的现场）。
func TestOwnerApplyGiveUpKeepsOrdinaryMaterialsInBag(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	if err := repos.Character.Save(ctx, dnfrepo.CharacterRecord{CharacterID: "18", AccountID: "acc-quest"}); err != nil {
		t.Fatal(err)
	}
	if err := repos.Quest.Save(ctx, dnfrepo.QuestRecord{
		CharacterID: "18",
		States:      map[int64]dnfrepo.QuestState{3145: {Status: "active"}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repos.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "18",
		Slots:       map[string]dnfrepo.ItemStack{"0:121": {ItemID: 10094602, Count: 10}},
	}); err != nil {
		t.Fatal(err)
	}
	owner, err := NewOwner(repos)
	if err != nil {
		t.Fatal(err)
	}
	result, err := owner.ApplyGiveUpSettlement(ctx, buildGiveUpCatalog(t, "[type]\n`[seeking]`\n[int data]\n10094602 10\n", ""),
		GiveUpInput{
			AccountID: "acc-quest", SelectedCharacterID: 18, QuestID: 3145,
			// 物品目录判定 10094602 是 `[material]`：不是任务专属 ⇒ 不回收。
			IsQuestItem: func(int64) bool { return false },
		})
	if err != nil {
		t.Fatalf("ApplyGiveUpSettlement() error = %v", err)
	}
	if len(result.Reclaimed) != 0 || result.ReclaimSkipped || result.HasPostCommitInventory {
		t.Fatalf("ordinary-material reclaim = %+v, want none", result)
	}
	stored, found, err := repos.Quest.Load(ctx, "18")
	if err != nil || !found {
		t.Fatalf("load quest found=%t err=%v", found, err)
	}
	if _, exists := stored.States[3145]; exists {
		t.Fatalf("quest 3145 still active: %+v", stored.States[3145])
	}
	inventory, found, err := repos.Inventory.Load(ctx, "18")
	if err != nil || !found {
		t.Fatalf("load inventory found=%t err=%v", found, err)
	}
	if stack := inventory.Slots["0:121"]; stack.ItemID != 10094602 || stack.Count != 10 {
		t.Fatalf("ordinary material changed: %+v", stack)
	}
}

// 任务专属道具：放弃成功 + 同事务扣减 + 给出提交后快照；其它任务需要的量要留下。
func TestOwnerApplyGiveUpReclaimsQuestItemsInSameSettlement(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	if err := repos.Character.Save(ctx, dnfrepo.CharacterRecord{CharacterID: "18", AccountID: "acc-quest"}); err != nil {
		t.Fatal(err)
	}
	if err := repos.Quest.Save(ctx, dnfrepo.QuestRecord{
		CharacterID: "18",
		States: map[int64]dnfrepo.QuestState{
			3145: {Status: "active"},
			3146: {Status: "active"}, // 还需要 1001×3
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repos.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "18",
		Slots:       map[string]dnfrepo.ItemStack{"0:177": {ItemID: 1001, Count: 6}},
	}); err != nil {
		t.Fatal(err)
	}
	owner, err := NewOwner(repos)
	if err != nil {
		t.Fatal(err)
	}
	catalog := buildGiveUpCatalog(t,
		"[type]\n`[seeking]`\n[int data]\n1001 10\n",
		"[type]\n`[seeking]`\n[int data]\n1001 3\n")
	result, err := owner.ApplyGiveUpSettlement(ctx, catalog, GiveUpInput{
		AccountID: "acc-quest", SelectedCharacterID: 18, QuestID: 3145,
		IsQuestItem: func(itemID int64) bool { return itemID == 1001 },
	})
	if err != nil {
		t.Fatalf("ApplyGiveUpSettlement() error = %v", err)
	}
	if len(result.Reclaimed) != 1 {
		t.Fatalf("reclaimed = %+v, want one row", result.Reclaimed)
	}
	receipt := result.Reclaimed[0]
	if receipt.SlotKey != "0:177" || receipt.ItemID != 1001 || receipt.Delta != 3 || receipt.PostCount != 3 {
		t.Fatalf("reclaim receipt = %+v, want slot 0:177 item 1001 delta 3 post 3", receipt)
	}
	if !result.HasPostCommitInventory {
		t.Fatal("reclaim without post-commit inventory snapshot")
	}
	if stack := result.PostCommitInventory.Slots["0:177"]; stack.ItemID != 1001 || stack.Count != 3 {
		t.Fatalf("post-commit snapshot = %+v, want item 1001 count 3", stack)
	}
	inventory, found, err := repos.Inventory.Load(ctx, "18")
	if err != nil || !found {
		t.Fatalf("load inventory found=%t err=%v", found, err)
	}
	if stack := inventory.Slots["0:177"]; stack.ItemID != 1001 || stack.Count != 3 {
		t.Fatalf("persisted inventory = %+v, want item 1001 count 3", stack)
	}
	stored, found, err := repos.Quest.Load(ctx, "18")
	if err != nil || !found {
		t.Fatalf("load quest found=%t err=%v", found, err)
	}
	if _, exists := stored.States[3145]; exists {
		t.Fatalf("quest 3145 still active: %+v", stored.States[3145])
	}
	if stored.States[3146].Status != "active" {
		t.Fatalf("other active quest changed: %+v", stored.States[3146])
	}
}

// 整行扣完时该行必须从背包里消失。
func TestOwnerApplyGiveUpDeletesEmptiedQuestItemRow(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	if err := repos.Character.Save(ctx, dnfrepo.CharacterRecord{CharacterID: "18", AccountID: "acc-quest"}); err != nil {
		t.Fatal(err)
	}
	if err := repos.Quest.Save(ctx, dnfrepo.QuestRecord{
		CharacterID: "18",
		States:      map[int64]dnfrepo.QuestState{3145: {Status: "active"}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repos.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "18",
		Slots:       map[string]dnfrepo.ItemStack{"0:177": {ItemID: 1001, Count: 2}},
	}); err != nil {
		t.Fatal(err)
	}
	owner, err := NewOwner(repos)
	if err != nil {
		t.Fatal(err)
	}
	result, err := owner.ApplyGiveUpSettlement(ctx, buildGiveUpCatalog(t, "[type]\n`[seeking]`\n[int data]\n1001 2\n", ""),
		GiveUpInput{
			AccountID: "acc-quest", SelectedCharacterID: 18, QuestID: 3145,
			IsQuestItem: func(int64) bool { return true },
		})
	if err != nil {
		t.Fatalf("ApplyGiveUpSettlement() error = %v", err)
	}
	if len(result.Reclaimed) != 1 || result.Reclaimed[0].Delta != 2 || result.Reclaimed[0].PostCount != 0 {
		t.Fatalf("reclaim receipt = %+v, want delta 2 post 0", result.Reclaimed)
	}
	inventory, found, err := repos.Inventory.Load(ctx, "18")
	if err != nil || !found {
		t.Fatalf("load inventory found=%t err=%v", found, err)
	}
	if _, exists := inventory.Slots["0:177"]; exists {
		t.Fatalf("emptied quest-item row survived: %+v", inventory.Slots["0:177"])
	}
}

// 没有事务单元时：放弃仍然成功（绝不挡住玩家），只是不回收并如实上报。
func TestOwnerApplyGiveUpWithoutSettlementStillAbandons(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	if err := repos.Character.Save(ctx, dnfrepo.CharacterRecord{CharacterID: "18", AccountID: "acc-quest"}); err != nil {
		t.Fatal(err)
	}
	if err := repos.Quest.Save(ctx, dnfrepo.QuestRecord{
		CharacterID: "18",
		States:      map[int64]dnfrepo.QuestState{3145: {Status: "active"}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repos.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "18",
		Slots:       map[string]dnfrepo.ItemStack{"0:177": {ItemID: 1001, Count: 4}},
	}); err != nil {
		t.Fatal(err)
	}
	repos.CharacterSettlement = nil
	owner, err := NewOwner(repos)
	if err != nil {
		t.Fatal(err)
	}
	result, err := owner.ApplyGiveUpSettlement(ctx, buildGiveUpCatalog(t, "[type]\n`[seeking]`\n[int data]\n1001 10\n", ""),
		GiveUpInput{
			AccountID: "acc-quest", SelectedCharacterID: 18, QuestID: 3145,
			IsQuestItem: func(int64) bool { return true },
		})
	if err != nil {
		t.Fatalf("ApplyGiveUpSettlement() error = %v", err)
	}
	if !result.ReclaimSkipped || result.ReclaimSkipReason != "character_settlement_transaction_unavailable" {
		t.Fatalf("reclaim skip = %t/%q", result.ReclaimSkipped, result.ReclaimSkipReason)
	}
	stored, found, err := repos.Quest.Load(ctx, "18")
	if err != nil || !found {
		t.Fatalf("load quest found=%t err=%v", found, err)
	}
	if _, exists := stored.States[3145]; exists {
		t.Fatalf("degraded give-up left quest active: %+v", stored.States[3145])
	}
	inventory, _, err := repos.Inventory.Load(ctx, "18")
	if err != nil {
		t.Fatal(err)
	}
	if stack := inventory.Slots["0:177"]; stack.Count != 4 {
		t.Fatalf("degraded give-up mutated inventory: %+v", stack)
	}
}
