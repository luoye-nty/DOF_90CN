package inventory

import (
	"context"
	"testing"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

// 塔罗牌的事务语义：扣金币 + 扣道具 + 发物品 **要么全成要么全不成**。
//
// 契约本身由 dnfbridge 的 resolveCurrentTarotLotteryCard 从真 PVF 建（金币 100000
// 与奖池在那边已用真档断言过）；这里验证提交：三个数字一起变，或者一个都不变。

const (
	tarotCardItemID = int64(10159697)
	tarotRewardID   = int64(100250574)
)

type tarotLotteryFixture struct {
	owner *Owner
	repos dnfrepo.Group
	char  string
}

func newTarotLotteryFixture(t *testing.T) *tarotLotteryFixture {
	t.Helper()
	repos := dnfrepomemory.NewMemoryGroup()
	owner, err := NewOwner(repos)
	if err != nil {
		t.Fatal(err)
	}
	fixture := &tarotLotteryFixture{owner: owner, repos: repos, char: "51"}
	fixture.setGold(t, 1000000)
	return fixture
}

func (f *tarotLotteryFixture) setGold(t *testing.T, gold int64) {
	t.Helper()
	record := dnfrepo.CharacterRecord{
		CharacterID: f.char,
		AccountID:   "account-1",
		Name:        "tarot",
		Level:       90,
		Stats:       map[string]int64{"gold": gold},
	}
	if err := f.repos.Character.Save(context.Background(), record); err != nil {
		t.Fatal(err)
	}
}

func (f *tarotLotteryFixture) gold(t *testing.T) int64 {
	t.Helper()
	record, found, err := f.repos.Character.Load(context.Background(), f.char)
	if err != nil || !found {
		t.Fatalf("load character: found=%t err=%v", found, err)
	}
	return record.Stats["gold"]
}

func (f *tarotLotteryFixture) seedMainSlot(t *testing.T, slot int16, itemID int64, count int64) {
	t.Helper()
	saveTestInventory(t, context.Background(), f.repos, dnfrepo.InventoryRecord{
		CharacterID: f.char,
		Slots: map[string]dnfrepo.ItemStack{
			slotKey(listTypeMain, slot): {ItemID: itemID, Count: count},
		},
	})
}

func (f *tarotLotteryFixture) slotsSnapshot(t *testing.T) map[string]int64 {
	t.Helper()
	record, found, err := f.repos.Inventory.Load(context.Background(), f.char)
	if err != nil || !found {
		t.Fatalf("load inventory: found=%t err=%v", found, err)
	}
	snapshot := make(map[string]int64, len(record.Slots))
	for key, stack := range record.Slots {
		snapshot[key] = stack.Count
	}
	return snapshot
}

func (f *tarotLotteryFixture) mainSlotCount(t *testing.T, slot int16) int64 {
	t.Helper()
	record, found, err := f.repos.Inventory.Load(context.Background(), f.char)
	if err != nil || !found {
		t.Fatalf("load inventory: found=%t err=%v", found, err)
	}
	return record.Slots[slotKey(listTypeMain, slot)].Count
}

func (f *tarotLotteryFixture) rewardCount(t *testing.T, itemID int64) int64 {
	t.Helper()
	record, found, err := f.repos.Inventory.Load(context.Background(), f.char)
	if err != nil || !found {
		t.Fatalf("load inventory: found=%t err=%v", found, err)
	}
	total := int64(0)
	for _, stack := range record.Slots {
		if int64(stack.ItemID) == itemID {
			total += stack.Count
		}
	}
	return total
}

func (f *tarotLotteryFixture) useCommand(slot int16, itemID int64) Command {
	return Command{
		Operation:           "use_stackable",
		AccountID:           "account-1",
		SelectedCharacterID: 51,
		SourceListType:      listTypeMain,
		SourceSlotIndex:     slot,
		SourceInstanceValue: 1,
		ItemTemplateID:      int32(itemID),
	}
}

func tarotLotteryTestResolution(goldCost int64, rewardID int64, count int64) alignedcmd.TarotLotteryResolution {
	resolution := alignedcmd.TarotLotteryResolution{
		SourceItemID:  tarotCardItemID,
		SourcePVFPath: "stackable/10159001/10159697.stk",
		StackableType: "[monster tarot card]",
		GoldCost:      goldCost,
		TotalWeight:   999998,
		Roll:          0,
	}
	if rewardID > 0 {
		resolution.Drawn = &alignedcmd.TarotLotteryOutcome{
			Reward: alignedcmd.MagicBoxRewardItem{ItemID: rewardID, Kind: "stackable", StackLimit: 999},
			Count:  count,
		}
	}
	return resolution
}

// TestTarotLotteryCardChargesGoldConsumesCardAndGrantsReward is the end-to-end
// commit: one use moves all three numbers (gold, card count, granted item).
func TestTarotLotteryCardChargesGoldConsumesCardAndGrantsReward(t *testing.T) {
	fixture := newTarotLotteryFixture(t)
	before := fixture.gold(t)
	fixture.seedMainSlot(t, 3, tarotCardItemID, 2)

	result, err := fixture.owner.UseTarotLotteryCard(
		context.Background(),
		fixture.useCommand(3, tarotCardItemID),
		tarotLotteryTestResolution(100000, tarotRewardID, 1),
	)
	if err != nil {
		t.Fatalf("use tarot card: %v", err)
	}
	if !result.Changed || result.RemainingCount != 1 {
		t.Fatalf("result=%+v want one card consumed from two", result)
	}
	if result.RandomRewardItemID != tarotRewardID {
		t.Fatalf("granted item=%d want %d", result.RandomRewardItemID, tarotRewardID)
	}
	if got := fixture.gold(t); got != before-100000 {
		t.Fatalf("gold=%d want %d (100000 charged)", got, before-100000)
	}
	if count := fixture.mainSlotCount(t, 3); count != 1 {
		t.Fatalf("card count=%d want 1", count)
	}
	if count := fixture.rewardCount(t, tarotRewardID); count != 1 {
		t.Fatalf("reward count=%d want 1", count)
	}
}

// TestTarotLotteryCardFailsClosedWithoutChangingAnything covers the three failure
// paths: 金币不足 / 奖池为空 / 掷到落空区。金币、道具、背包三者一个都不许动。
func TestTarotLotteryCardFailsClosedWithoutChangingAnything(t *testing.T) {
	cases := []struct {
		name       string
		gold       int64
		resolution alignedcmd.TarotLotteryResolution
	}{
		{name: "insufficient gold", gold: 99999, resolution: tarotLotteryTestResolution(100000, tarotRewardID, 1)},
		{name: "empty pool", gold: 1000000, resolution: tarotLotteryTestResolution(100000, 0, 0)},
		{name: "table gap", gold: 1000000, resolution: func() alignedcmd.TarotLotteryResolution {
			resolution := tarotLotteryTestResolution(100000, 0, 0)
			resolution.Missed = true
			resolution.Roll = 999998
			return resolution
		}()},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := newTarotLotteryFixture(t)
			fixture.setGold(t, testCase.gold)
			fixture.seedMainSlot(t, 3, tarotCardItemID, 2)
			goldBefore := fixture.gold(t)
			slotsBefore := fixture.slotsSnapshot(t)

			if _, err := fixture.owner.UseTarotLotteryCard(
				context.Background(),
				fixture.useCommand(3, tarotCardItemID),
				testCase.resolution,
			); err == nil {
				t.Fatal("the use must fail closed")
			}
			if got := fixture.gold(t); got != goldBefore {
				t.Fatalf("gold moved %d -> %d", goldBefore, got)
			}
			if after := fixture.slotsSnapshot(t); !sameSlotCounts(slotsBefore, after) {
				t.Fatalf("inventory moved: before=%v after=%v", slotsBefore, after)
			}
		})
	}
}

// TestTarotLotteryCardGrantFailureRollsBackEverything is the transaction counter-proof:
// the grant fails after the card was consumed and the gold was staged in the same
// commit, and both must come back exactly as they were.
func TestTarotLotteryCardGrantFailureRollsBackEverything(t *testing.T) {
	fixture := newTarotLotteryFixture(t)
	before := fixture.gold(t)
	fixture.seedMainSlot(t, 3, tarotCardItemID, 2)
	slotsBefore := fixture.slotsSnapshot(t)

	// An inverted slot range makes grantRandomRewardItem fail its own contract
	// check, i.e. **after** the card was removed from the cloned record and after
	// the gold was staged for the same commit.
	broken := tarotLotteryTestResolution(100000, tarotRewardID, 1)
	broken.Drawn.Reward.SlotStart = 5
	broken.Drawn.Reward.SlotEnd = 0

	if _, err := fixture.owner.UseTarotLotteryCard(
		context.Background(),
		fixture.useCommand(3, tarotCardItemID),
		broken,
	); err == nil {
		t.Fatal("the broken grant must fail the whole use")
	}
	if got := fixture.gold(t); got != before {
		t.Fatalf("gold was charged despite the failed grant: %d -> %d", before, got)
	}
	if after := fixture.slotsSnapshot(t); !sameSlotCounts(slotsBefore, after) {
		t.Fatalf("the card was consumed despite the failed grant: before=%v after=%v", slotsBefore, after)
	}
}

func sameSlotCounts(before, after map[string]int64) bool {
	if len(before) != len(after) {
		return false
	}
	for key, count := range before {
		if after[key] != count {
			return false
		}
	}
	return true
}
