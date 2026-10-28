package dnfbridge

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	"longheng.io/server/internal/modules/dnf/mail"
	"longheng.io/server/internal/modules/dnf/purchaselimit"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

// ★ 运营口径：「多买多送等活动奖励归**付款方**」（A 送道具给 B，奖励归 A）。
//
// 赠送形态下"买到的商品"改道去邮件，但**活动奖励必须留在付款方背包**：它们是
// "买了才有"的奖励，跟着钱走。这条用例把三件事一起钉住：
//  1. 赠送**照样计数**多买多送（否则送一次就能刷活动进度）；
//  2. 跨档奖励落进**付款方背包**；
//  3. 跨档奖励**不进收件人的邮件**（否则等于把奖励也送给对方了）。
func TestCurrentCeraShopGiftKeepsBuyCountGiftRewardsWithThePayer(t *testing.T) {
	ctx := context.Background()
	catalog := mustCurrentCeraShopBuyCountGiftCatalog(t, currentCeraShopBuyCountGiftTestSource())
	if !catalog.BuyCountGiftPlan().Enabled() {
		t.Fatalf("测试目录里多买多送未启用: %q", catalog.BuyCountGiftPlan().disabledReason)
	}
	repositories := dnfrepomemory.NewMemoryGroup()
	now := time.Now().UTC()

	// 付款方（赠送方）：有名字/职业、账号满 30 天、点券充足。
	if err := repositories.Account.Save(ctx, dnfrepo.AccountRecord{
		AccountID: "account-1",
		Metadata:  map[string]string{"account_cera": "100000"},
		CreatedAt: now.Add(-30 * 24 * time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "19", AccountID: "account-1", Name: "1111", Job: "0", Level: 90,
		Stats: map[string]int64{"grow_type": 100000},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "19", Slots: map[string]dnfrepo.ItemStack{},
	}); err != nil {
		t.Fatal(err)
	}
	// 收件人：同职业、互为好友 30 天。
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "22", AccountID: "account-2", Name: "9999", Job: "0", Level: 90,
		Stats: map[string]int64{"grow_type": 1},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "22", Slots: map[string]dnfrepo.ItemStack{},
	}); err != nil {
		t.Fatal(err)
	}
	seedCeraShopGiftQueryFriendship(t, repositories, "1111", "9999", now.Add(-30*24*time.Hour))

	connection := &bufferConn{}
	service := &Service{
		options: options{
			accountID:          "account-1",
			gameUpperHeader:    gameUpperHeaderServer16,
			gameUpperBodyCodec: gameUpperBodyCodecPlain,
		},
		ceraShopCatalog:    catalog,
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
	}
	session := &gameSession{conn: connection, connID: "gift-buycount", selectedCharacterID: 19}

	// 参与 `[bonus event ipg]` 的那件商品（104764，对应道具 37）。
	if err := service.handleCurrentCeraShopPurchase(
		session,
		currentCeraShopGiftPurchaseBody("9999", currentCeraShopBuyCountGiftTestCommodityID),
	); err != nil {
		t.Fatal(err)
	}
	raw := connection.write.Bytes()
	if len(raw) == 0 {
		t.Fatal("no reply packets")
	}

	payerInventory, found, err := repositories.Inventory.Load(ctx, "19")
	if err != nil || !found {
		t.Fatalf("load payer inventory found=%t err=%v", found, err)
	}
	countIn := func(inventory dnfrepo.InventoryRecord, itemID uint32) int64 {
		var total int64
		for _, stack := range inventory.Slots {
			if uint32(stack.ItemID) == itemID {
				total += int64(stack.Count)
			}
		}
		return total
	}
	// 1) 跨档奖励（第 1 档 = 490702364）落在**付款方**背包。
	const firstTierReward uint32 = 490702364
	if got := countIn(payerInventory, firstTierReward); got != 1 {
		t.Fatalf("payer tier-1 reward count=%d want=1 (event rewards belong to whoever paid)", got)
	}
	// 2) 买到的商品（道具 37）**不在**付款方背包。
	if got := countIn(payerInventory, 37); got != 0 {
		t.Fatalf("payer inventory holds %d of the purchased item 37: a gift must never land in the payer's bag", got)
	}

	// 3) 收件人邮箱里只有**买到的商品**，没有活动奖励。
	mailbox, found, err := repositories.Mailbox.Load(ctx, "22")
	if err != nil || !found {
		t.Fatalf("load recipient mailbox found=%t err=%v", found, err)
	}
	if len(mailbox.Mails) != 1 {
		t.Fatalf("recipient mails=%d want=1", len(mailbox.Mails))
	}
	for mailID, mail := range mailbox.Mails {
		if mail.SenderName != "1111" {
			t.Fatalf("mail %s sender=%q want=1111", mailID, mail.SenderName)
		}
		if len(mail.Attachments) != 1 {
			t.Fatalf("mail %s attachments=%d want=1: %+v", mailID, len(mail.Attachments), mail.Attachments)
		}
		if got := mail.Attachments[0].ItemID; got != 37 {
			t.Fatalf("mail %s attachment item=%d want=37", mailID, got)
		}
		for _, attachment := range mail.Attachments {
			if uint32(attachment.ItemID) == firstTierReward {
				t.Fatalf("mail %s carries the buy-count tier reward %d: event rewards must stay with the payer",
					mailID, firstTierReward)
			}
		}
	}
	// 4) 计数落库在付款方角色上（小退/重启后还在），且收件人身上没有这个计数。
	payer, found, err := repositories.Character.Load(ctx, "19")
	if err != nil || !found {
		t.Fatalf("load payer character found=%t err=%v", found, err)
	}
	if got := currentCeraShopBuyCountGiftCount(payer); got != 1 {
		t.Fatalf("payer buy-count gift count=%d want=1 (a gift must still count toward the event)", got)
	}
	recipient, found, err := repositories.Character.Load(ctx, "22")
	if err != nil || !found {
		t.Fatalf("load recipient character found=%t err=%v", found, err)
	}
	if got := currentCeraShopBuyCountGiftCount(recipient); got != 0 {
		t.Fatalf("recipient buy-count gift count=%d want=0 (the recipient did not pay)", got)
	}
}

// ★ 收件人**真的能领出来**：赠送邮件里的附件必须能被邮件领取路径物化成背包道具。
// 只断言"邮件里有附件"是不够的 —— 附件行如果表达不了这件道具，玩家点领取时会失败
// 或者领到一行垃圾。
func TestCurrentCeraShopGiftMailAttachmentIsClaimableByTheRecipient(t *testing.T) {
	ctx := context.Background()
	catalog := mustCurrentCeraShopTestCatalog(t)
	repositories := dnfrepomemory.NewMemoryGroup()
	now := time.Now().UTC()

	if err := repositories.Account.Save(ctx, dnfrepo.AccountRecord{
		AccountID: "account-1",
		Metadata:  map[string]string{"account_cera": "200"},
		CreatedAt: now.Add(-30 * 24 * time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	for _, character := range []dnfrepo.CharacterRecord{
		{CharacterID: "19", AccountID: "account-1", Name: "1111", Job: "0", Level: 90,
			Stats: map[string]int64{"grow_type": 1}},
		{CharacterID: "22", AccountID: "account-2", Name: "9999", Job: "0", Level: 90,
			Stats: map[string]int64{"grow_type": 1}},
	} {
		if err := repositories.Character.Save(ctx, character); err != nil {
			t.Fatal(err)
		}
		if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{
			CharacterID: character.CharacterID, Slots: map[string]dnfrepo.ItemStack{},
		}); err != nil {
			t.Fatal(err)
		}
	}
	seedCeraShopGiftQueryFriendship(t, repositories, "1111", "9999", now.Add(-30*24*time.Hour))

	connection := &bufferConn{}
	service := newCeraShopPremiumTestService(repositories, catalog)
	session := &gameSession{conn: connection, connID: "gift-claim", selectedCharacterID: 19}
	// 100050 = 道具 37（可叠加），单价 80，余额 200。
	if err := service.handleCurrentCeraShopPurchase(session, currentCeraShopGiftPurchaseBody("9999", 100050)); err != nil {
		t.Fatal(err)
	}

	recipientMail, found, err := repositories.Mailbox.Load(ctx, "22")
	if err != nil || !found || len(recipientMail.Mails) != 1 {
		t.Fatalf("recipient mailbox found=%t mails=%d err=%v", found, len(recipientMail.Mails), err)
	}
	var mailID string
	for id := range recipientMail.Mails {
		mailID = id
	}
	// 用邮件模块自己的领取入口（客户端 op316 走的就是它）把附件领出来。
	// 领取路径要一个"道具号 → 落格信息"的解析器，生产环境是 `alignedMailboxItemResolverForCommand`
	// 从当前 PVF 建的；这里用同一张测试目录建一个等价的。
	itemResolver := func(itemID uint32) (alignedcmd.MailboxItemResolution, error) {
		definition, resolveErr := catalog.items.ResolveItem(itemID)
		if resolveErr != nil {
			return alignedcmd.MailboxItemResolution{}, resolveErr
		}
		return mailboxItemResolutionFromPVF(definition, false)
	}
	mailNumber, err := strconv.ParseUint(mailID, 10, 32)
	if err != nil {
		t.Fatalf("gift mail id %q: %v", mailID, err)
	}
	claim, err := mail.NewOwner(repositories, itemResolver).Claim(ctx, "22", mail.ExtractRequest{MailIDs: []uint32{uint32(mailNumber)}})
	if err != nil {
		t.Fatalf("claim gift mail %s: %v", mailID, err)
	}
	// ItemCount 数的是**领出来的附件行**（这里是 1 行），不是道具个数（3 个）；
	// 道具个数在下面按背包实际内容核对。
	if claim.ItemCount != 1 {
		t.Fatalf("claim reported ItemCount=%d want=1 attachment row", claim.ItemCount)
	}

	claimed, found, err := repositories.Inventory.Load(ctx, "22")
	if err != nil || !found {
		t.Fatalf("load recipient inventory found=%t err=%v", found, err)
	}
	var total int64
	for _, stack := range claimed.Slots {
		if uint32(stack.ItemID) == 37 {
			total += int64(stack.Count)
		}
	}
	if total != 3 {
		t.Fatalf("recipient claimed %d of item 37, want 3 (the gift must be extractable): slots=%+v", total, claimed.Slots)
	}
}

// ★ 反向红线：**普通购买绝不能被改道成赠送**。
// 赠送的改道是靠 `request.GiftRecipientName` 触发的；这条用例从结果侧再钉一次：
// 普通结账的 GiftAttachments 必须是空的、商品必须落进付款方自己的背包、且不产生邮件。
func TestCurrentCeraShopNormalPurchaseIsNeverCollectedAsAGift(t *testing.T) {
	ctx := context.Background()
	catalog := mustCurrentCeraShopTestCatalog(t)
	repositories := dnfrepomemory.NewMemoryGroup()
	now := time.Now().UTC()
	if err := repositories.Account.Save(ctx, dnfrepo.AccountRecord{
		AccountID: "account-1",
		Metadata:  map[string]string{"account_cera": "200"},
		CreatedAt: now.Add(-30 * 24 * time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "19", AccountID: "account-1", Name: "1111", Job: "0", Level: 90,
		Stats: map[string]int64{"grow_type": 1},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "19", Slots: map[string]dnfrepo.ItemStack{},
	}); err != nil {
		t.Fatal(err)
	}
	service := newCeraShopPremiumTestService(repositories, catalog)
	session := &gameSession{connID: "normal-purchase", selectedCharacterID: 19}

	result, err := service.commitCurrentCeraShopPurchase(ctx, session, catalog, currentCeraShopPurchaseRequest{
		PaymentMode: currentCeraShopPaymentModeCera,
		Items:       []currentCeraShopPurchaseRequestItem{{CommodityID: 100050}},
	})
	if err != nil {
		t.Fatalf("normal purchase failed: %v", err)
	}
	if len(result.GiftAttachments) != 0 {
		t.Fatalf("normal purchase collected %d gift attachments: %+v", len(result.GiftAttachments), result.GiftAttachments)
	}
	if len(result.GiftGrantNotes) != 0 {
		t.Fatalf("normal purchase recorded gift notes: %v", result.GiftGrantNotes)
	}
	inventory, found, err := repositories.Inventory.Load(ctx, "19")
	if err != nil || !found {
		t.Fatalf("load payer inventory found=%t err=%v", found, err)
	}
	var total int64
	for _, stack := range inventory.Slots {
		if uint32(stack.ItemID) == 37 {
			total += int64(stack.Count)
		}
	}
	if total != 3 {
		t.Fatalf("normal purchase delivered %d of item 37 to the buyer, want 3: %+v", total, inventory.Slots)
	}
	mailbox, found, err := repositories.Mailbox.Load(ctx, "19")
	if err != nil {
		t.Fatal(err)
	}
	if found && len(mailbox.Mails) != 0 {
		t.Fatalf("normal purchase wrote %d mails", len(mailbox.Mails))
	}
}

func avatarDefinitionForGiftTest(t *testing.T, catalog *pvfCeraShopCatalog) dungeonDropItemDefinition {
	t.Helper()
	definition, err := catalog.items.ResolveItem(9001)
	if err != nil {
		t.Fatalf("resolve avatar item 9001: %v", err)
	}
	return definition
}

// ★★ D1 回归：**同一件商品连着送同一个收件人两次**必须真的产生两封邮件。
//
// AppendSystemMail 按 completion_key 去重：命中就把**原来那封**返回、不新建。所以
// completion_key 里只要少了"每次结账都不同"的那一段，第二单就会：点券照扣、回包照回
// 成功、收件人只有第一封邮件 —— 玩家花了钱什么都没有。这条用例就是那个场景。
func TestCurrentCeraShopGiftTwiceToTheSameRecipientCreatesTwoMails(t *testing.T) {
	ctx := context.Background()
	catalog := mustCurrentCeraShopTestCatalog(t)
	repositories := dnfrepomemory.NewMemoryGroup()
	now := time.Now().UTC()
	if err := repositories.Account.Save(ctx, dnfrepo.AccountRecord{
		AccountID: "account-1",
		Metadata:  map[string]string{"account_cera": "200"},
		CreatedAt: now.Add(-30 * 24 * time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	for _, character := range []dnfrepo.CharacterRecord{
		{CharacterID: "19", AccountID: "account-1", Name: "1111", Job: "0", Level: 90,
			Stats: map[string]int64{"grow_type": 1}},
		{CharacterID: "22", AccountID: "account-2", Name: "9999", Job: "0", Level: 90,
			Stats: map[string]int64{"grow_type": 1}},
	} {
		if err := repositories.Character.Save(ctx, character); err != nil {
			t.Fatal(err)
		}
		if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{
			CharacterID: character.CharacterID, Slots: map[string]dnfrepo.ItemStack{},
		}); err != nil {
			t.Fatal(err)
		}
	}
	seedCeraShopGiftQueryFriendship(t, repositories, "1111", "9999", now.Add(-30*24*time.Hour))

	service := newCeraShopPremiumTestService(repositories, catalog)
	// 100050 单价 80，余额 200 —— 正好够送两次，第三次会被点券不足挡住。
	for attempt := 1; attempt <= 2; attempt++ {
		session := &gameSession{conn: &bufferConn{}, connID: "gift-twice", selectedCharacterID: 19}
		if err := service.handleCurrentCeraShopPurchase(session, currentCeraShopGiftPurchaseBody("9999", 100050)); err != nil {
			t.Fatalf("第 %d 次赠送失败: %v", attempt, err)
		}
	}

	if balance := loadCeraShopAccountCera(t, ctx, repositories, "account-1"); balance != 40 {
		t.Fatalf("sender cera=%d want=40 (200 - 80 - 80)", balance)
	}
	mailbox, found, err := repositories.Mailbox.Load(ctx, "22")
	if err != nil || !found {
		t.Fatalf("load recipient mailbox found=%t err=%v", found, err)
	}
	if len(mailbox.Mails) != 2 {
		t.Fatalf("recipient mails=%d want=2 (a repeat gift was charged but never delivered): %+v",
			len(mailbox.Mails), mailbox.Mails)
	}
	keys := make(map[string]struct{}, 2)
	for mailID, mail := range mailbox.Mails {
		if len(mail.Attachments) != 1 || mail.Attachments[0].ItemID != 37 {
			t.Fatalf("mail %s attachments=%+v want one stack of item 37", mailID, mail.Attachments)
		}
		key := strings.TrimSpace(mail.Metadata["completion_key"])
		if key == "" {
			t.Fatalf("mail %s has no completion key", mailID)
		}
		if _, duplicate := keys[key]; duplicate {
			t.Fatalf("two gift mails share the completion key %q", key)
		}
		keys[key] = struct{}{}
	}
}

// ★★ D2 回归：赠送**限时时装**时，收件人拿到的必须还是限时的、且带着买的那一档属性。
//
// 时装落格不是"把 PVF 定义塞进一行"：grantCurrentCeraShopAvatar 会写
// avatar_ability_no（属性档）、durability、avatar_duration_days、item_kind=avatar。
// 用通用转换构造附件会把这些全丢掉 —— 后果是"买 30 天时装送人，对方领到永久时装"。
func TestCurrentCeraShopGiftTimedAvatarKeepsItsDurationAndOption(t *testing.T) {
	ctx := context.Background()
	catalog := mustCurrentCeraShopTestCatalog(t)
	repositories := dnfrepomemory.NewMemoryGroup()
	now := time.Now().UTC()
	if err := repositories.Account.Save(ctx, dnfrepo.AccountRecord{
		AccountID: "account-1",
		Metadata:  map[string]string{"account_cera": "200"},
		CreatedAt: now.Add(-30 * 24 * time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	for _, character := range []dnfrepo.CharacterRecord{
		{CharacterID: "19", AccountID: "account-1", Name: "1111", Job: "0", Level: 90,
			Stats: map[string]int64{"grow_type": 1}},
		{CharacterID: "22", AccountID: "account-2", Name: "9999", Job: "0", Level: 90,
			Stats: map[string]int64{"grow_type": 1}},
	} {
		if err := repositories.Character.Save(ctx, character); err != nil {
			t.Fatal(err)
		}
		if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{
			CharacterID: character.CharacterID, Slots: map[string]dnfrepo.ItemStack{},
		}); err != nil {
			t.Fatal(err)
		}
	}
	seedCeraShopGiftQueryFriendship(t, repositories, "1111", "9999", now.Add(-30*24*time.Hour))

	service := newCeraShopPremiumTestService(repositories, catalog)
	session := &gameSession{conn: &bufferConn{}, connID: "gift-avatar", selectedCharacterID: 19}
	// 100070 = 时装 9001 的**第 1 档**（测试 PVF 的 `[avatar type select]` 第 1 行 = 7 天/30 点券）。
	// 100071 是第 3 档 = 永久，用它测不出"限时被寄成永久"这件事。
	request, err := parseCurrentCeraShopPurchaseRequest(currentCeraShopGiftPurchaseBody("9999", 100070))
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.commitCurrentCeraShopPurchase(ctx, session, catalog, request)
	if err != nil {
		t.Fatalf("gift a timed avatar: %v", err)
	}
	if len(result.GiftAttachments) != 1 {
		t.Fatalf("attachments=%+v want one avatar row", result.GiftAttachments)
	}
	attachment := result.GiftAttachments[0]
	if attachment.ItemID != 9001 {
		t.Fatalf("attachment item=%d want=9001", attachment.ItemID)
	}
	if attachment.ExpireAt.IsZero() {
		t.Fatalf("gifted avatar has no expiry: a 7-day avatar must not arrive permanent (%+v)", attachment)
	}
	remaining := time.Until(attachment.ExpireAt)
	if remaining < 6*24*time.Hour || remaining > 8*24*time.Hour {
		t.Fatalf("gifted avatar expires in %v, want ~7 days", remaining)
	}
	for _, key := range []string{"avatar_duration_days", "avatar_ability_no", "item_kind"} {
		if strings.TrimSpace(attachment.Extra[key]) == "" {
			t.Fatalf("gifted avatar is missing Extra[%q]: %+v", key, attachment.Extra)
		}
	}
	if got := attachment.Extra["item_kind"]; got != "avatar" {
		t.Fatalf("gifted avatar item_kind=%q want=avatar", got)
	}
	// 买的是 7 天那一档，附件里记的时长必须也是 7 —— 用通用转换构造附件时这里是 0。
	if got := attachment.Extra["avatar_duration_days"]; got != "7" {
		t.Fatalf("gifted avatar duration=%q want=7 (the paid tier must survive the mail)", got)
	}
	if got := attachment.Extra["avatar_duration_selector_index"]; got != "1" {
		t.Fatalf("gifted avatar selector index=%q want=1", got)
	}
	if len(attachment.RawEntry) == 0 {
		t.Fatal("gifted avatar has no native row")
	}
	// 这条路为什么必须单独写：通用转换（PVF 定义 → 实例行）**不知道**"买的是哪一档"，
	// 它产出的行既没有时长也没有属性档。同一件商品用通用转换的产物对比一下，
	// 就能证明上面那些字段是"走真正落格函数"换来的，而不是碰巧带上的。
	generic, genericErr := currentCeraShopGiftAttachments(avatarDefinitionForGiftTest(t, catalog), 1, 100070)
	if genericErr != nil {
		t.Fatalf("generic conversion of the avatar failed outright: %v", genericErr)
	}
	if len(generic) != 1 {
		t.Fatalf("generic conversion produced %d rows", len(generic))
	}
	if generic[0].Extra["avatar_duration_days"] == "7" || !generic[0].ExpireAt.IsZero() {
		t.Fatalf("generic conversion unexpectedly preserved the paid tier: %+v", generic[0])
	}
	payerInventory, found, err := repositories.Inventory.Load(ctx, "19")
	if err != nil || !found {
		t.Fatalf("load payer inventory found=%t err=%v", found, err)
	}
	if len(payerInventory.Slots) != 0 {
		t.Fatalf("payer inventory changed: %+v", payerInventory.Slots)
	}
}

// ★★ 每日赠送额度：账号维度、每天 10 万点券（客户端 70107 / op136 结果码 0xA3）。
//
// 现场 2026-09-27 17:46：同一账号连送 5 个 39900 的礼包（合计 199500 点券）没有任何拦截
// —— 这条规则当时根本没实现。这条用例把边界钉死：
//   - 额度只剩 30 时，30 点券的时装**可以**送（正好用满、不算超额）；
//   - 再送 80 点券的商品必须被拦（会超到 100010），回 11「超出赠送金额上限」，
//     而且**一分钱不扣、一封邮件不发**；
//   - 额度用完后 op136 直接回 0xA3，连收件人都不用查。
func TestCurrentCeraShopGiftDailyCeraAllowanceIsEnforced(t *testing.T) {
	ctx := context.Background()
	catalog := mustCurrentCeraShopTestCatalog(t)
	repositories := dnfrepomemory.NewMemoryGroup()
	now := time.Now().UTC()
	policy := currentCeraShopGiftDailyPolicy()
	// 预置今天的额度：只剩 30 点券（100000 - 99970）。
	metadata := map[string]string{
		"account_cera":                  "200",
		purchaselimit.CountKey(policy):  "99970",
		purchaselimit.WindowKey(policy): strconv.FormatInt(policy.Window(now).Start.Unix(), 10),
	}
	if err := repositories.Account.Save(ctx, dnfrepo.AccountRecord{
		AccountID: "account-1",
		Metadata:  metadata,
		CreatedAt: now.Add(-30 * 24 * time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	for _, character := range []dnfrepo.CharacterRecord{
		{CharacterID: "19", AccountID: "account-1", Name: "1111", Job: "0", Level: 90,
			Stats: map[string]int64{"grow_type": 1}},
		{CharacterID: "22", AccountID: "account-2", Name: "9999", Job: "0", Level: 90,
			Stats: map[string]int64{"grow_type": 1}},
	} {
		if err := repositories.Character.Save(ctx, character); err != nil {
			t.Fatal(err)
		}
		if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{
			CharacterID: character.CharacterID, Slots: map[string]dnfrepo.ItemStack{},
		}); err != nil {
			t.Fatal(err)
		}
	}
	seedCeraShopGiftQueryFriendship(t, repositories, "1111", "9999", now.Add(-30*24*time.Hour))

	service := newCeraShopPremiumTestService(repositories, catalog)

	// 1) 30 点券的时装：正好用满额度，必须成功。
	conn := &bufferConn{}
	session := &gameSession{conn: conn, connID: "gift-allowance-ok", selectedCharacterID: 19}
	if err := service.handleCurrentCeraShopPurchase(session, currentCeraShopGiftPurchaseBody("9999", 100071)); err != nil {
		t.Fatal(err)
	}
	okReply, _ := splitGameServerUpperPacket(t, conn.write.Bytes())
	if len(okReply.Body) == 0 || okReply.Body[0] != 1 {
		t.Fatalf("gift inside the allowance was rejected: body=%x", okReply.Body)
	}

	// 2) 再送 80 点券的商品：会超到 100010 > 100000，必须被拦。
	conn2 := &bufferConn{}
	session2 := &gameSession{conn: conn2, connID: "gift-allowance-exceeded", selectedCharacterID: 19}
	if err := service.handleCurrentCeraShopPurchase(session2, currentCeraShopGiftPurchaseBody("9999", 100050)); err != nil {
		t.Fatal(err)
	}
	rejected, _ := splitGameServerUpperPacket(t, conn2.write.Bytes())
	if len(rejected.Body) != 22 || rejected.Body[0] != 0 ||
		rejected.Body[1] != currentCeraShopPurchaseFailureCodeGiftAmountCap {
		t.Fatalf("daily allowance body=%x want failure with code 0x%02x (超出赠送金额上限)",
			rejected.Body, currentCeraShopPurchaseFailureCodeGiftAmountCap)
	}
	// 只扣了第一单的 30：第二单整笔回滚，一分钱都不能扣。
	if balance := loadCeraShopAccountCera(t, ctx, repositories, "account-1"); balance != 170 {
		t.Fatalf("sender cera=%d want=170 (200 - 30): the over-allowance gift must not be charged", balance)
	}
	mailbox, found, err := repositories.Mailbox.Load(ctx, "22")
	if err != nil || !found {
		t.Fatalf("load recipient mailbox found=%t err=%v", found, err)
	}
	if len(mailbox.Mails) != 1 {
		t.Fatalf("recipient mails=%d want=1 (the over-allowance gift must not be delivered)", len(mailbox.Mails))
	}
	inventory, found, err := repositories.Inventory.Load(ctx, "19")
	if err != nil || !found {
		t.Fatalf("load payer inventory found=%t err=%v", found, err)
	}
	for _, stack := range inventory.Slots {
		if uint32(stack.ItemID) == 37 {
			t.Fatalf("payer inventory holds the over-allowance item: %+v", inventory.Slots)
		}
	}

	// 3) 额度用完后，op136 直接回 0xA3（不查收件人）。
	if code := giftQueryReplyCode(t, service, session2, currentCeraShopGiftQueryLiveCaptureBody()); code != currentCeraShopGiftQueryCodeDailyCap {
		t.Fatalf("exhausted allowance query code=%#x want=%#x", code, currentCeraShopGiftQueryCodeDailyCap)
	}
}
