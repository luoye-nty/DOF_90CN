package dnfbridge

import (
	"context"
	"strconv"
	"testing"
	"time"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

// 回归（2026-09-29）：翻牌（通关卡牌）发放必须用该角色**真实已购**的背包扩展行数。
//
// 曾经的 bug：deliverCurrentDungeonSelectedCardRewardLocked 把主背包可用格数写成
// 字面量 0，语义等于"只认默认 4 行"（currentMainInventoryUnlockedEnd 的
// mainSlotCount 参数）。买过背包扩展券的角色（MainSlotCount = 24）在默认 4 行占满、
// 扩展行空着的时候，卡牌奖励仍然被判成"背包已满"整份转投邮件 —— 现场就是
// "背包栏位扩展后，翻牌好像只认初始的四行"。
//
// 这条用例同时钉住两件事：奖励落在扩展行内，且没有产生溢出邮件。
func TestDungeonCardRewardUsesPersistedExpansionRows(t *testing.T) {
	service, runtime, session, _, _ := prepareCompletedSettlementRuntime(t)
	repositories, ok := service.repositoryGroup()
	if !ok {
		t.Fatal("repository group unavailable")
	}
	ctx := context.Background()

	// 角色 99 买满 3 行扩展（24 格）。
	container := newCharacterContainerStateSettings("99", time.Unix(10, 0).UTC())
	container.Values["main_list_param16"] = "24"
	if err := repositories.Settings.Save(ctx, container); err != nil {
		t.Fatal(err)
	}

	// 装备分类的默认 4 行（9..40）占满，扩展行 41..64 全空。
	slots := make(map[string]dnfrepo.ItemStack, currentMainInventoryBaseSlotCount)
	for slot := int16(9); slot <= 9+currentMainInventoryBaseSlotCount-1; slot++ {
		slots["0:"+strconv.Itoa(int(slot))] = dnfrepo.ItemStack{ItemID: 700, Count: 1}
	}
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "99",
		Slots:       slots,
	}); err != nil {
		t.Fatal(err)
	}

	// 一份装备卡牌奖励，PVF 页 9..64（三个扩展行都在页内）。
	plan, err := newDungeonCardRewardPlan(
		dungeonCardPlanIdentity{
			CharacterID: "99",
			DungeonID:   runtime.Dungeon.ID,
			MazeIndex:   runtime.MazeIndex,
			RunSeed:     runtime.Seed,
		},
		"test_expansion_rows",
		dungeonCardRewardBundle{Items: []dungeonCardItemReward{{
			ItemID: 5001, Count: 1, SlotStart: 9, SlotEnd: 64,
			Extra: map[string]string{"item_kind": "equipment"},
		}}},
		dungeonCardRewardBundle{},
	)
	if err != nil {
		t.Fatal(err)
	}
	state, err := newDungeonCardState(plan)
	if err != nil {
		t.Fatal(err)
	}
	runtime.settlementCardRewardState = state

	if err := service.deliverCurrentDungeonSelectedCardRewardLocked(
		session,
		runtime,
		dungeonCardSideFree,
		0,
		"test_expansion_rows",
	); err != nil {
		t.Fatal(err)
	}

	inventory, found, err := repositories.Inventory.Load(ctx, "99")
	if err != nil || !found {
		t.Fatalf("inventory found=%t err=%v", found, err)
	}
	landed := int16(-1)
	for slot := int16(9 + currentMainInventoryBaseSlotCount); slot <= 64; slot++ {
		if stack, occupied := inventory.Slots["0:"+strconv.Itoa(int(slot))]; occupied && stack.ItemID == 5001 {
			landed = slot
			break
		}
	}
	if landed < 0 {
		t.Fatalf("卡牌奖励没有用已解锁的扩展行（41..64）: slots=%+v", inventory.Slots)
	}

	mailbox, found, err := repositories.Mailbox.Load(ctx, "99")
	if err != nil {
		t.Fatal(err)
	}
	if found && len(mailbox.Mails) != 0 {
		t.Fatalf("扩展行空着却把卡牌奖励转投了邮件: %+v", mailbox.Mails)
	}
}
