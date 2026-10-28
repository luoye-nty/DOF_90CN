package dnfbridge

import (
	"context"
	"testing"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

// 这组用例锁死一条曾经被打破的不变量：**未解锁的背包扩展行绝不能被当作落点**。
//
// 背景：main 背包标准分类每个跨 7 行 × 8 格 = 56 格，新角色只解锁前 4 行（32 格），
// 扩展券每张解锁 1 行（最多 3 行 = 24 格）。limitCurrentMainInventoryDefinitionSlots
// 负责把落点 SlotEnd 夹到已解锁的最后一格。
//
// 曾经的 bug：grantCurrentCeraShopProduct 把扩展行数做成了 `...uint16` 可选变参，
// 结果 15 个调用点里只有赛丽亚商店的 4 个传了值，其余静默不夹取 —— 玩家 4 行放满后
// 在 NPC 商店买东西，物品落进了客户端不渲染的未解锁格，看起来就"消失"了。
//
// 现在该参数是必填的：任何新调用点漏传都会编译失败，不会再退化成静默放行。
func TestLimitCurrentMainInventoryDefinitionSlotsClampsToUnlockedRows(t *testing.T) {
	// 材料分类：65..120，共 56 格（= currentMainInventoryCategorySlotCount）。
	material := dungeonDropItemDefinition{SlotStart: 65, SlotEnd: 120}

	cases := []struct {
		name          string
		mainSlotCount uint16
		wantSlotEnd   int16
		why           string
	}{
		{"no expansion", 0, 96, "默认 4 行：65 + 32 - 1 + 0"},
		{"one row bought", 8, 104, "5 行：65 + 32 - 1 + 8"},
		{"two rows bought", 16, 112, "6 行：65 + 32 - 1 + 16"},
		{"three rows bought", 24, 120, "7 行：与 PVF 原始 SlotEnd 相同，等于不夹取"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := limitCurrentMainInventoryDefinitionSlots(material, tc.mainSlotCount).SlotEnd
			if got != tc.wantSlotEnd {
				t.Fatalf("mainSlotCount=%d: SlotEnd=%d want=%d (%s)",
					tc.mainSlotCount, got, tc.wantSlotEnd, tc.why)
			}
		})
	}
}

// 非 56 格的分类（快捷栏、特殊材料等）不能被夹取，否则会误伤这些固定槽位。
func TestLimitCurrentMainInventoryDefinitionSlotsLeavesOtherRangesAlone(t *testing.T) {
	// 快捷栏 3..8：不是 56 格范围。
	quick := dungeonDropItemDefinition{SlotStart: 3, SlotEnd: 8}
	if got := limitCurrentMainInventoryDefinitionSlots(quick, 0).SlotEnd; got != 8 {
		t.Fatalf("快捷栏被误夹取: SlotEnd=%d want=8", got)
	}
	// 特殊材料 345..359：固定仓库区段，同样不是 56 格范围。
	fixed := dungeonDropItemDefinition{SlotStart: 345, SlotEnd: 359}
	if got := limitCurrentMainInventoryDefinitionSlots(fixed, 0).SlotEnd; got != 359 {
		t.Fatalf("固定材料区段被误夹取: SlotEnd=%d want=359", got)
	}
}

// 助手取不到角色/仓储时必须**安全失败**（返回 0 = 夹到默认 4 行），
// 而不是返回一个很大的值把未解锁行放出来。
func TestPlacementMainSlotCountFailsSafe(t *testing.T) {
	if got := currentInventoryPlacementMainSlotCountFor(context.Background(), nil, "77"); got != 0 {
		t.Fatalf("仓储为 nil 时应返回 0，实际 %d", got)
	}
	if got := currentInventoryPlacementMainSlotCountFor(context.Background(), nil, ""); got != 0 {
		t.Fatalf("角色为空时应返回 0，实际 %d", got)
	}
	// 安全失败的语义是"夹到默认 4 行"，不是"不夹取"。
	// 这里断言这个回退值确实产生 4 行上限，避免将来有人把回退改成 24。
	material := dungeonDropItemDefinition{SlotStart: 65, SlotEnd: 120}
	if got := limitCurrentMainInventoryDefinitionSlots(material, 0).SlotEnd; got != 96 {
		t.Fatalf("回退值 0 必须等价于 4 行上限 96，实际 %d", got)
	}
}

// 副本内拾取是第二条曾经漏夹取的路径：整条链路把 PVF 的 SlotEnd（材料 65..120 等）
// 原样交给落点算法，于是已解锁的 4 行填满后，拾取到的物品会落进未解锁的扩展行 ——
// 客户端不渲染那些格子，物品看起来就"消失"了（用户 2026-09-12 报的
// "副本内拾取 物品会跑到未解锁的槽位"）。
//
// 这里把已解锁的 4 行全部填满（每格都顶到堆叠上限，无法并入），再拾取同类材料：
// 必须拒绝这次拾取并保留地面掉落物，绝不能落到第 5~7 行。
func TestCurrentDungeonPickupNeverUsesLockedExpansionSlots(t *testing.T) {
	ctx := context.Background()
	service, runtime, repositories := prepareCurrentDungeonDropTest(t, 1, 3227)
	connection := &bufferConn{}
	session := &gameSession{
		conn:                connection,
		connID:              "dungeon-locked-expansion-slot-test",
		selectedCharacterID: 99,
		dungeon:             dungeonSessionState{runtime: runtime},
	}
	catalog, err := service.dungeonMonsterCatalog()
	if err != nil {
		t.Fatal(err)
	}
	drops, err := catalog.DropCatalog()
	if err != nil {
		t.Fatal(err)
	}
	definition, err := drops.ResolveItem(3227)
	if err != nil {
		t.Fatal(err)
	}
	unlockedEnd := currentMainInventoryUnlockedEnd(definition.SlotStart, 0)
	if unlockedEnd >= definition.SlotEnd {
		t.Fatalf("fixture item no longer spans a locked row: slots=%d..%d unlocked_end=%d",
			definition.SlotStart, definition.SlotEnd, unlockedEnd)
	}
	record, found, err := repositories.Inventory.Load(ctx, "99")
	if err != nil || !found {
		t.Fatalf("inventory found=%t err=%v", found, err)
	}
	record.Slots = map[string]dnfrepo.ItemStack{}
	for slot := definition.SlotStart; slot <= unlockedEnd; slot++ {
		record.Slots[currentDungeonPickupMainSlotKey(slot)] = dnfrepo.ItemStack{
			ItemID: int64(definition.ItemID),
			Count:  definition.StackLimit,
		}
	}
	if err := repositories.Inventory.Save(ctx, record); err != nil {
		t.Fatal(err)
	}

	dropObjectKey := seedCurrentDungeonPickupDrop(t, service, runtime, 3227, currentSceneActorObjectKey(99))
	if err := service.handleCurrentDungeonPickup(session, currentDungeonPickupTestBody(dropObjectKey)); err != nil {
		t.Fatal(err)
	}

	after, found, err := repositories.Inventory.Load(ctx, "99")
	if err != nil || !found {
		t.Fatalf("inventory after pickup found=%t err=%v", found, err)
	}
	for slot := unlockedEnd + 1; slot <= definition.SlotEnd; slot++ {
		if stack, occupied := after.Slots[currentDungeonPickupMainSlotKey(slot)]; occupied && stack.Count > 0 {
			t.Fatalf("pickup stored item %d in locked slot %d (unlocked end %d)",
				stack.ItemID, slot, unlockedEnd)
		}
	}
	if len(after.Slots) != int(unlockedEnd-definition.SlotStart+1) {
		t.Fatalf("pickup added a stack while the unlocked rows were full: slots=%d", len(after.Slots))
	}
	drop := runtime.DropOwner.byObjectKey[dropObjectKey]
	if drop == nil || drop.Status != runtimeDungeonDropAvailable {
		t.Fatalf("full unlocked rows must leave the ground drop available: drop=%+v", drop)
	}
}
