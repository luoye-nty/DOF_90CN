package inventory

import (
	"testing"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

// A destroyed piece refunds what disassembling it would have yielded, so the
// six cube fragments and six souls must land in their fixed account-shared
// warehouse slots -- the same destination 分解 uses -- and not in the
// character backpack. Everything else still goes to the bag.
func TestOwnerUpgradeDestroyRefundsWarehouseStackablesToTheWarehouse(t *testing.T) {
	for _, mode := range []string{"reinforce", "amplify"} {
		t.Run(mode, func(t *testing.T) {
			ctx, repos, owner, cmd := protectedUpgradeFixture(t, mode, 10, 7804, 2)
			cmd.OptionalTicketSlot = -1
			cmd.UpgradePenaltyType = 3
			cmd.UpgradeDestroyBonusItemID = 3243
			cmd.UpgradeDestroyBonusCount = 6
			cmd.UpgradeDestroyExtraRewards = []alignedcmd.UpgradeDestroyReward{
				{ItemID: 3033, Count: 2, BestEffort: true},     // cube_black -> 354
				{ItemID: 10099775, Count: 1, BestEffort: true}, // legendary soul -> 364
				{ItemID: 3311, Count: 20, BestEffort: true},    // no fixed slot -> backpack
			}
			// A warehouse slot that already holds the same item is stacked
			// into, never duplicated into a second row.
			if err := repos.AccountInventory.Save(ctx, dnfrepo.AccountInventoryRecord{
				AccountID: "acc",
				Slots: map[string]dnfrepo.ItemStack{
					dnfrepo.AccountSharedInventorySlotKey(354): {
						ItemID: 3033, Count: 5,
						Extra: map[string]string{"item_kind": "stackable"},
					},
				},
			}); err != nil {
				t.Fatal(err)
			}

			result, err := owner.Upgrade(ctx, cmd)
			if err != nil || !result.Success || result.ResultCode != upgradeResultCodeDestroy {
				t.Fatalf("result=%+v err=%v", result, err)
			}

			account, found, err := repos.AccountInventory.Load(ctx, "acc")
			if err != nil || !found {
				t.Fatalf("account warehouse load: found=%t err=%v", found, err)
			}
			if stack := account.Slots[dnfrepo.AccountSharedInventorySlotKey(354)]; stack.ItemID != 3033 || stack.Count != 7 {
				t.Fatalf("cube fragment warehouse slot=%+v want 3033 x7", stack)
			}
			if stack := account.Slots[dnfrepo.AccountSharedInventorySlotKey(364)]; stack.ItemID != 10099775 || stack.Count != 1 {
				t.Fatalf("legendary soul warehouse slot=%+v want 10099775 x1", stack)
			}

			bag := loadTestInventory(t, ctx, repos, "77")
			for slot := int16(0); slot <= 200; slot++ {
				stack, ok := bag.Slots[slotKey(listTypeMain, slot)]
				if !ok {
					continue
				}
				if stack.ItemID == 3033 || stack.ItemID == 10099775 {
					t.Fatalf("warehouse stackable %d leaked into backpack slot %d", stack.ItemID, slot)
				}
			}

			granted := map[int32]UpgradeDestroyGrantedReward{}
			for _, row := range result.DestroyExtraRewards {
				granted[row.ItemID] = row
			}
			if row := granted[3033]; row.Slot != 354 || row.Count != 2 {
				t.Fatalf("cube refund row=%+v want slot 354 x2", row)
			}
			if row := granted[10099775]; row.Slot != 364 || row.Count != 1 {
				t.Fatalf("soul refund row=%+v want slot 364 x1", row)
			}
			if row, ok := granted[3311]; !ok || row.Slot >= dnfrepo.CrystalWarehouseFirstSlot {
				t.Fatalf("non-warehouse refund row=%+v must stay in the backpack", row)
			}
			if len(result.DestroyRefundOverflow) != 0 {
				t.Fatalf("nothing should have overflowed: %+v", result.DestroyRefundOverflow)
			}

			// The client paints the warehouse in the same list 0, so the
			// incremental refresh has to carry the new warehouse counts.
			refreshed := map[int16]dnfrepo.ItemStack{}
			for _, entry := range upgradeItemUpdateEntries(result, false) {
				refreshed[entry.slot] = entry.stack
			}
			if stack, ok := refreshed[354]; !ok || stack.ItemID != 3033 || stack.Count != 7 {
				t.Fatalf("refresh for warehouse slot 354=%+v ok=%t", stack, ok)
			}
			if stack, ok := refreshed[364]; !ok || stack.ItemID != 10099775 || stack.Count != 1 {
				t.Fatalf("refresh for warehouse slot 364=%+v ok=%t", stack, ok)
			}
		})
	}
}

// 2026-09-14 22:03 活体：强化材料本身就是固定账号槽里的无色小晶块（槽 358），破坏
// 返还又回同一个槽。命令开始处抓的材料副本在返还之后才写回，把整块返还抹掉了 —— op80
// 的返还行和 op14 的刷新都写着 3037 x368，落地却只扣掉本次消耗的 200 个
// (48316 -> 48116)，小退自然也看不到。这里钉住同一个槽上的两个写者必须相加。
func TestOwnerUpgradeDestroyRefundSurvivesConsumptionOfTheSameWarehouseSlot(t *testing.T) {
	ctx, repos, owner, cmd := protectedUpgradeFixture(t, "reinforce", 13, 7804, 2)
	cmd.OptionalTicketSlot = -1
	cmd.UpgradePenaltyType = 3
	cmd.MaterialSlotIndex = 358
	cmd.UpgradeMaterialItemID = 3037
	cmd.UpgradeMaterialCount = 180
	cmd.UpgradeDestroyBonusItemID = 0
	cmd.UpgradeDestroyBonusCount = 0
	cmd.UpgradeDestroyExtraRewards = []alignedcmd.UpgradeDestroyReward{
		{ItemID: 3037, Count: 368, BestEffort: true},
	}
	const seeded = int64(48316)
	if err := repos.AccountInventory.Save(ctx, dnfrepo.AccountInventoryRecord{
		AccountID: "acc",
		Slots: map[string]dnfrepo.ItemStack{
			dnfrepo.AccountSharedInventorySlotKey(358): {
				ItemID: 3037, Count: seeded,
				Extra: map[string]string{"item_kind": "stackable"},
			},
		},
	}); err != nil {
		t.Fatal(err)
	}

	result, err := owner.Upgrade(ctx, cmd)
	if err != nil || !result.Success || result.ResultCode != upgradeResultCodeDestroy {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	want := seeded - 180 + 368

	account, found, err := repos.AccountInventory.Load(ctx, "acc")
	if err != nil || !found {
		t.Fatalf("account warehouse load: found=%t err=%v", found, err)
	}
	if stack := account.Slots[dnfrepo.AccountSharedInventorySlotKey(358)]; stack.ItemID != 3037 || stack.Count != want {
		t.Fatalf("无色 warehouse slot=%+v want 3037 x%d", stack, want)
	}
	if result.MaterialRemainingStackCount != want {
		t.Fatalf("material remaining=%d want %d", result.MaterialRemainingStackCount, want)
	}
	// 客户端画的就是这一包：返还后的槽位刷新必须与库里的值一致。
	refreshed := map[int16]dnfrepo.ItemStack{}
	for _, entry := range upgradeItemUpdateEntries(result, false) {
		refreshed[entry.slot] = entry.stack
	}
	if stack, ok := refreshed[358]; !ok || stack.ItemID != 3037 || stack.Count != want {
		t.Fatalf("refresh for warehouse slot 358=%+v ok=%t want 3037 x%d", stack, ok, want)
	}
	if len(result.DestroyExtraRewards) != 1 || result.DestroyExtraRewards[0].Slot != 358 ||
		result.DestroyExtraRewards[0].Count != 368 {
		t.Fatalf("refund rows=%+v want one slot-358 row of 368", result.DestroyExtraRewards)
	}
}

// A foreign stack squatting on a fixed warehouse slot is never merged into or
// evicted: that refund falls back to the backpack instead of being lost.
func TestOwnerUpgradeDestroyWarehouseRefundFallsBackToBagOnForeignSquatter(t *testing.T) {
	ctx, repos, owner, cmd := protectedUpgradeFixture(t, "reinforce", 10, 7804, 2)
	cmd.OptionalTicketSlot = -1
	cmd.UpgradePenaltyType = 3
	cmd.UpgradeDestroyBonusItemID = 0
	cmd.UpgradeDestroyBonusCount = 0
	cmd.UpgradeDestroyExtraRewards = []alignedcmd.UpgradeDestroyReward{
		{ItemID: 3033, Count: 2, BestEffort: true},
	}
	if err := repos.AccountInventory.Save(ctx, dnfrepo.AccountInventoryRecord{
		AccountID: "acc",
		Slots: map[string]dnfrepo.ItemStack{
			dnfrepo.AccountSharedInventorySlotKey(354): {
				ItemID: 3034, Count: 1,
				Extra: map[string]string{"item_kind": "stackable"},
			},
		},
	}); err != nil {
		t.Fatal(err)
	}

	result, err := owner.Upgrade(ctx, cmd)
	if err != nil || !result.Success || result.ResultCode != upgradeResultCodeDestroy {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	account, _, err := repos.AccountInventory.Load(ctx, "acc")
	if err != nil {
		t.Fatal(err)
	}
	if stack := account.Slots[dnfrepo.AccountSharedInventorySlotKey(354)]; stack.ItemID != 3034 || stack.Count != 1 {
		t.Fatalf("squatting stack was touched: %+v", stack)
	}
	if len(result.DestroyExtraRewards) != 1 || result.DestroyExtraRewards[0].ItemID != 3033 ||
		result.DestroyExtraRewards[0].Slot >= dnfrepo.CrystalWarehouseFirstSlot {
		t.Fatalf("refund rows=%+v want one backpack row", result.DestroyExtraRewards)
	}
	bag := loadTestInventory(t, ctx, repos, "77")
	stack := bag.Slots[slotKey(listTypeMain, result.DestroyExtraRewards[0].Slot)]
	if stack.ItemID != 3033 || stack.Count != 2 {
		t.Fatalf("backpack fallback stack=%+v", stack)
	}
}
