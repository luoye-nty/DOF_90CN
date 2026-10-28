package dnfbridge

import (
	"context"
	"testing"
	"time"

	dnfraid "longheng.io/server/internal/modules/dnf/raid"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

// 「不管谁背包满了 都不能影响整体翻牌 流程要走完；装不下的走邮件」（操作员 2026-10-02 定案）。
//
// 真机 2026-10-02 16:42（raid 12058625，分片 20261002-019）：
//
//	game-raid-reward-auto-flip-commit-failed raid_key=12058625 raid_phase=1 flipped=4
//	  error=runtime raid reward grant is invalid: character=1 item=490008672 count=1:
//	        dnf dungeon pickup inventory category is full: item=490008672 range=65..120
//
// 牌**翻了**（flipped=4），坏在发奖那一步：`commitRuntimeRaidSettlementRewards` 用一个
// 事务罩住**全队**（`WithinRaidRewards(ctx, characterIDs, …)`），队里第一个背包满的人一
// `return`，整个闭包失败、事务整体回滚 —— 四个人的奖励一个都发不出去，回执也不落，
// 客户端的牌就永远停在「翻不开」（服务器自己在日志里写着
// `client_visible=card_window_stays_open_and_the_flip_never_settles`）。
//
// 本用例钉住四件事：
//
//	① 一个人背包满，**提交照样成功**（改动前这里是红）；
//	② 其余成员照常在背包里拿到奖励；
//	③ 满包的那一位改走**邮件**，附件就是他没拿到的那几件；
//	④ 重投同一份回执**不会发第二封**（completion_key 去重）。
func TestRaidRewardOverflowGoesToMailWithoutBlockingTheRaid(t *testing.T) {
	// 三个团本共用这一条发奖路径，所以这条口径对安东/卢克同样成立（操作员 2026-10-02：
	// 「安徒恩和卢克也应该适用 某些人背包满了不应该拖死全队」）。
	// 安东与卢克走的是**同一个分支**：runtimeFiendWarRewardEligible 对它们恒真、
	// 发奖循环 / 回执 / 邮件三段都没有任何 RaidMode 判断，唯一差别是奖励目录怎么取
	// （runtimeRaidBaseConfig）。所以两个模式各跑一遍就足以钉住。
	for _, scenario := range []struct {
		name  string
		apply func(fixture runtimeRaidRewardTestFixture)
	}{
		{"安东", func(fixture runtimeRaidRewardTestFixture) {
			// 夹具默认就是安东：AntonConfig 已设、RaidMode 走 runtimeRaidBaseConfig 的兜底。
			fixture.service.raids[fixture.raidKey].RaidMode = runtimeRaidOwnerAnton
		}},
		{"卢克", func(fixture runtimeRaidRewardTestFixture) {
			state := fixture.service.raids[fixture.raidKey]
			// 目录预设成夹具那一份，免得 runtimeRaidLukeRewardCatalog 去读真 PVF。
			fixture.service.raidLukeReward = fixture.service.raidAntonReward
			state.RaidMode = runtimeRaidOwnerLuke
			state.LukeConfig = &dnfraid.LukeConfig{Raid: state.AntonConfig}
		}},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			fixture := newRuntimeRaidRewardTestFixture(t, false)
			scenario.apply(fixture)
			runRaidRewardOverflowScenario(t, fixture)
		})
	}
}

// runRaidRewardOverflowScenario 是这条用例的本体，对安东与卢克各跑一次。
func runRaidRewardOverflowScenario(t *testing.T, fixture runtimeRaidRewardTestFixture) {
	t.Helper()
	service := fixture.service
	// 溢出邮件要用 currentPVFItemCatalog() 把奖励实例化成附件；夹具的目录是用
	// newAntonRewardPVFTestItems 现搭的合成 PVF（service 上没有 pvfPath），
	// 这里把**夹具解析奖励时用的同一份** source / itemRefs 挂上去。
	rewardCatalog := service.raidAntonReward
	if rewardCatalog == nil {
		t.Fatal("夹具没有奖励目录")
	}
	service.pvfItemCatalog = &pvfDungeonDropCatalog{
		source:    rewardCatalog.source,
		itemRefs:  rewardCatalog.itemRefs,
		itemCache: make(map[uint32]dungeonDropItemDefinition),
	}

	// 把 1002 的主背包整片塞满，任何分页都放不下 —— 真机那次就是 65..120 满。
	// 填的是**另一个** item id 且 bind，所以既不能并栈也没有空格。
	fillRuntimeRaidRewardTestBag(t, fixture, "1002", 1, 400)

	delivery, err := service.commitRuntimeRaidSettlementRewards(
		fixture.raidKey,
		runtimeRaidDungeonPhaseOne,
		fixture.now,
	)
	if err != nil {
		t.Fatalf("背包满的成员把整队的提交拖死了: %v\n"+
			"（真机 2026-10-02 16:42 就是这个形状：flipped=4 但 commit-failed，"+
			"客户端停在「翻不开」）", err)
	}

	// ② 没满的那位照常在背包里拿到。
	if slots := runtimeRaidRewardTestRefreshSlots(delivery, 1001); len(slots) == 0 {
		t.Fatal("背包没满的成员一个格位都没写：奖励没发出去")
	}
	// ③ 满包的那位一个格位都不该写（整份走邮件，不留半截）。
	if slots := runtimeRaidRewardTestRefreshSlots(delivery, 1002); len(slots) != 0 {
		t.Fatalf("背包满的成员竟然写进了格位 %v", slots)
	}

	wantRows := runtimeRaidRewardTestWireRows(t, delivery, 1002)
	if len(wantRows) == 0 {
		t.Fatal("投递里没有 1002 的奖励行：夹具没造出问题")
	}
	wantCount := int64(0)
	for _, row := range wantRows {
		wantCount += int64(row.CountOrAux)
	}

	// ③ 邮件真的落了，附件对得上。
	mails := runtimeRaidRewardTestSystemMails(t, fixture, "1002")
	if len(mails) != 1 {
		t.Fatalf("1002 的系统邮件 = %d 封, want 1（满包的那一份必须走邮件）", len(mails))
	}
	if got := mails[0].Metadata["source"]; got != currentRaidRewardOverflowMailSource {
		t.Fatalf("邮件 source = %q, want %q", got, currentRaidRewardOverflowMailSource)
	}
	if len(mails[0].Attachments) == 0 {
		t.Fatal("溢出邮件的附件是空的：奖励还是没到玩家手上")
	}
	attached := int64(0)
	for _, attachment := range mails[0].Attachments {
		attached += attachment.Count
		if got := attachment.Extra["overflow_mail"]; got != "true" {
			t.Fatalf("附件缺少 overflow_mail 标记：Extra=%v", attachment.Extra)
		}
	}
	if attached != wantCount {
		t.Fatalf("邮件附件总量 = %d, want %d（背包满就得把整份奖励寄出去）", attached, wantCount)
	}
	// 背包没满的那位不该收到溢出邮件。
	if free := runtimeRaidRewardTestSystemMails(t, fixture, "1001"); len(free) != 0 {
		t.Fatalf("背包没满的成员也收到了溢出邮件：%+v", free)
	}

	// ④ 重投同一份回执：不再落第二封。
	if _, err := service.commitRuntimeRaidSettlementRewards(
		fixture.raidKey,
		runtimeRaidDungeonPhaseOne,
		fixture.now.Add(time.Minute),
	); err != nil {
		t.Fatalf("重投失败: %v", err)
	}
	if again := runtimeRaidRewardTestSystemMails(t, fixture, "1002"); len(again) != 1 {
		t.Fatalf("重投之后 1002 的溢出邮件 = %d 封, want 仍为 1 封（completion_key 必须去重）", len(again))
	}
}

// fillRuntimeRaidRewardTestBag 把某个角色的主背包 [from,to] 整段填满。
func fillRuntimeRaidRewardTestBag(
	t *testing.T,
	fixture runtimeRaidRewardTestFixture,
	characterID string,
	from, to int16,
) {
	t.Helper()
	ctx := context.Background()
	inventory, found, err := fixture.repositories.Inventory.Load(ctx, characterID)
	if err != nil || !found {
		t.Fatalf("load inventory %s found=%t err=%v", characterID, found, err)
	}
	if inventory.Slots == nil {
		inventory.Slots = make(map[string]dnfrepo.ItemStack)
	}
	for slot := from; slot <= to; slot++ {
		inventory.Slots[currentCeraShopInventorySlotKey(dnfrepo.MainInventoryListType, slot)] = dnfrepo.ItemStack{
			ItemID: 990001,
			Count:  1,
			Bind:   true,
		}
	}
	if err := fixture.repositories.Inventory.Save(ctx, inventory); err != nil {
		t.Fatalf("seed full bag for %s: %v", characterID, err)
	}
}

// runtimeRaidRewardTestRefreshSlots 取某位成员这次投递刷新了哪些格位。
func runtimeRaidRewardTestRefreshSlots(
	delivery runtimeRaidSettlementRewardDelivery,
	characterID uint16,
) []int16 {
	for _, refresh := range delivery.InventoryRefreshes {
		if refresh.CharacterID == characterID {
			return refresh.Slots
		}
	}
	return nil
}

// runtimeRaidRewardTestWireRows 取某位成员这次结算的 0x0258 奖励行。
func runtimeRaidRewardTestWireRows(
	t *testing.T,
	delivery runtimeRaidSettlementRewardDelivery,
	characterID uint16,
) []raidRaidRewardTestRow {
	t.Helper()
	for _, member := range delivery.Rewards.Members {
		if member.MemberKey != characterID {
			continue
		}
		rows := make([]raidRaidRewardTestRow, 0, len(member.Rewards))
		for _, reward := range member.Rewards {
			rows = append(rows, raidRaidRewardTestRow{
				ItemIDOrValue: reward.ItemIDOrValue,
				CountOrAux:    uint32(reward.CountOrAux),
			})
		}
		return rows
	}
	t.Fatalf("投递里没有成员 %d 的奖励行", characterID)
	return nil
}

type raidRaidRewardTestRow struct {
	ItemIDOrValue uint32
	CountOrAux    uint32
}

// runtimeRaidRewardTestSystemMails 读某个角色邮箱里的系统邮件。
func runtimeRaidRewardTestSystemMails(
	t *testing.T,
	fixture runtimeRaidRewardTestFixture,
	characterID string,
) []dnfrepo.MailRecord {
	t.Helper()
	if fixture.repositories.Mailbox == nil {
		t.Fatal("夹具没有邮箱仓库")
	}
	record, found, err := fixture.repositories.Mailbox.Load(context.Background(), characterID)
	if err != nil {
		t.Fatalf("load mailbox %s: %v", characterID, err)
	}
	if !found {
		return nil
	}
	mails := make([]dnfrepo.MailRecord, 0, len(record.Mails))
	for _, mail := range record.Mails {
		if mail.Metadata["system_mail"] != "true" {
			continue
		}
		mails = append(mails, mail)
	}
	return mails
}
