package dnfbridge

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"

	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfproto "longheng.io/server/internal/modules/dnf/protocol"
	"longheng.io/server/internal/modules/dnf/purchaselimit"
	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

// 现场 2026-09-18：商城里「每个角色只能购买N次」的商品买满之后还能继续买。
// 商城道具在道具 PVF 里没有 `[purchase limit]`（实测 chn_490702507.stk 只有
// [expiration date]/[package data]），限购只写在 `etc/cerashop.etc [buy restrict item]`：
// 4 个数字一行，第 4 个是角色维度次数——商品 101081=1、104412=3，与运营截图的
// “只能购买1次/3次”完全对应。
func TestParseCurrentCeraShopBuyRestrictReadsCharacterLimitColumn(t *testing.T) {
	source := currentCeraShopTestSource()
	source["etc/cerashop.etc"] += "[buy restrict item]\n" +
		"13 100050 1 3\n" + // 第 3 列语义未证实，第 4 列才是次数
		"14 100060 0 1\n" +
		"15 100070 30 0\n" + // 次数 0 = 不限购
		"[/buy restrict item]\n"
	catalog, err := newPVFCeraShopCatalog(source)
	if err != nil {
		t.Fatal(err)
	}
	rows := catalog.BuyRestrictRows()
	if len(rows) != 3 {
		t.Fatalf("限购表行数=%d want=3 rows=%+v", len(rows), rows)
	}
	policy, found := catalog.BuyRestrictCharacterPolicy(100050)
	if !found || policy.Scope != purchaselimit.ScopeCharacter || policy.Limit != 3 ||
		policy.Period != purchaselimit.PeriodAccumulate || policy.ItemID != 100050 ||
		policy.KeyTag != currentCeraShopBuyRestrictKeyTag {
		t.Fatalf("商品 100050 限购策略=%+v found=%t", policy, found)
	}
	// 商品维度必须带命名空间：商品号与道具号同处一个数值空间，不加前缀会撞键。
	if key := purchaselimit.CountKey(policy); !strings.Contains(key, currentCeraShopBuyRestrictKeyTag) ||
		!strings.Contains(key, "100050") {
		t.Fatalf("计数键=%q", key)
	}
	if _, found := catalog.BuyRestrictCharacterPolicy(100070); found {
		t.Fatalf("次数为 0 的商品不应产生限购：%+v", rows[100070])
	}
	if _, found := catalog.BuyRestrictCharacterPolicy(100056); found {
		t.Fatal("不在限购表里的商品不应产生限购")
	}
}

func TestParseCurrentCeraShopBuyRestrictRejectsTornRows(t *testing.T) {
	source := currentCeraShopTestSource()
	source["etc/cerashop.etc"] += "[buy restrict item]\n13 100050 1\n[/buy restrict item]\n"
	if _, err := newPVFCeraShopCatalog(source); err == nil {
		t.Fatal("字段数不是 4 的倍数时必须报错，而不是猜一行")
	}
}

// 角色维度限购必须落在角色记录里（不是会话内存），所以小退重上、重启服务端都还在。
func TestCurrentCeraShopPurchaseRefusesSecondBuyPastCharacterLimit(t *testing.T) {
	ctx := context.Background()
	source := currentCeraShopTestSource()
	source["etc/cerashop.etc"] += "[buy restrict item]\n13 100050 1 1\n[/buy restrict item]\n"
	catalog, err := newPVFCeraShopCatalog(source)
	if err != nil {
		t.Fatal(err)
	}
	repositories := dnfrepomemory.NewMemoryGroup()
	seedCeraShopAccountCera(t, ctx, repositories, "account-1", 1000)
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "19", AccountID: "account-1", Level: 90,
		Stats: map[string]int64{"cera": 1000},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "19", Slots: map[string]dnfrepo.ItemStack{},
	}); err != nil {
		t.Fatal(err)
	}
	service := &Service{
		options:            options{accountID: "account-1"},
		ceraShopCatalog:    catalog,
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
	}
	session := &gameSession{selectedCharacterID: 19}
	buy := func() (currentCeraShopCommitResult, error) {
		return service.commitCurrentCeraShopPurchase(ctx, session, catalog, currentCeraShopPurchaseRequest{
			PaymentMode: currentCeraShopPaymentModeCera,
			Items:       []currentCeraShopPurchaseRequestItem{{CommodityID: 100050}},
		})
	}

	first, err := buy()
	if err != nil {
		t.Fatalf("第一次购买被拒: %v", err)
	}
	if len(first.BuyRestrictNotes) != 1 || !strings.Contains(first.BuyRestrictNotes[0], "character:buy restrict item:100050:1/1") {
		t.Fatalf("第一次购买没有上报限购计数: %+v", first.BuyRestrictNotes)
	}
	// 计数必须落库（角色 Stats），而不是只活在这次会话里。
	character, _, err := repositories.Character.Load(ctx, "19")
	if err != nil {
		t.Fatal(err)
	}
	policy, found := catalog.BuyRestrictCharacterPolicy(100050)
	if !found {
		t.Fatal("限购策略丢失")
	}
	state := purchaselimit.ReadCharacter(character.Stats, policy, service.currentPurchaseLimitTime())
	if state.Used != 1 || state.Limit != 1 {
		t.Fatalf("角色限购计数=%+v stats=%+v", state, character.Stats)
	}

	// 第二次（等于小退重上之后再买）必须被拦，且整笔回滚：点券与背包都不动。
	before := loadCeraShopAccountCera(t, ctx, repositories, "account-1")
	inventoryBefore, _, _ := repositories.Inventory.Load(ctx, "19")
	second, err := buy()
	if err == nil {
		t.Fatalf("超限购买没有被拒: %+v", second)
	}
	if !strings.Contains(err.Error(), "purchase limit") {
		t.Fatalf("拒绝原因不是限购: %v", err)
	}
	if after := loadCeraShopAccountCera(t, ctx, repositories, "account-1"); after != before {
		t.Fatalf("被拒的购买扣了点券: %d -> %d", before, after)
	}
	inventoryAfter, _, _ := repositories.Inventory.Load(ctx, "19")
	if len(inventoryAfter.Slots) != len(inventoryBefore.Slots) {
		t.Fatalf("被拒的购买改了背包: %d -> %d", len(inventoryBefore.Slots), len(inventoryAfter.Slots))
	}
	character, _, _ = repositories.Character.Load(ctx, "19")
	if state := purchaselimit.ReadCharacter(character.Stats, policy, service.currentPurchaseLimitTime()); state.Used != 1 {
		t.Fatalf("被拒的购买改了计数: %+v", state)
	}
}

// 多个商城商品同时限购时，购物车里任意一件超限都要整笔拒绝。
func TestCurrentCeraShopPurchaseRefusesCartWithOneOverLimitCommodity(t *testing.T) {
	ctx := context.Background()
	source := currentCeraShopTestSource()
	source["etc/cerashop.etc"] += "[buy restrict item]\n13 100050 1 1\n14 100056 7 2\n[/buy restrict item]\n"
	catalog, err := newPVFCeraShopCatalog(source)
	if err != nil {
		t.Fatal(err)
	}
	repositories := dnfrepomemory.NewMemoryGroup()
	seedCeraShopAccountCera(t, ctx, repositories, "account-1", 1000)
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "19", AccountID: "account-1", Level: 90,
		Stats: map[string]int64{"cera": 1000},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "19", Slots: map[string]dnfrepo.ItemStack{},
	}); err != nil {
		t.Fatal(err)
	}
	service := &Service{
		options:            options{accountID: "account-1"},
		ceraShopCatalog:    catalog,
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
	}
	session := &gameSession{selectedCharacterID: 19}
	cart := func(rows ...uint32) currentCeraShopPurchaseRequest {
		items := make([]currentCeraShopPurchaseRequestItem, 0, len(rows))
		for _, commodityID := range rows {
			items = append(items, currentCeraShopPurchaseRequestItem{CommodityID: commodityID})
		}
		return currentCeraShopPurchaseRequest{PaymentMode: currentCeraShopPaymentModeCera, Items: items}
	}

	// 100050 限购 1 次：先把它买满。
	if _, err := service.commitCurrentCeraShopPurchase(ctx, session, catalog, cart(100050)); err != nil {
		t.Fatal(err)
	}
	// 紧接着买「100050 + 100056」：100050 已满，整笔必须失败，100056 的计数也不能被吃掉。
	if _, err := service.commitCurrentCeraShopPurchase(ctx, session, catalog, cart(100050, 100056)); err == nil {
		t.Fatal("购物车里有超限商品时必须整笔拒绝")
	}
	character, _, err := repositories.Character.Load(ctx, "19")
	if err != nil {
		t.Fatal(err)
	}
	policy, _ := catalog.BuyRestrictCharacterPolicy(100056)
	if state := purchaselimit.ReadCharacter(character.Stats, policy, service.currentPurchaseLimitTime()); state.Used != 0 {
		t.Fatalf("被回滚的购物车留下了计数: %+v", state)
	}
	// 单独买 100056 仍然可以，且计数到 2 后就满。
	if _, err := service.commitCurrentCeraShopPurchase(ctx, session, catalog, cart(100056)); err != nil {
		t.Fatal(err)
	}
	if _, err := service.commitCurrentCeraShopPurchase(ctx, session, catalog, cart(100056)); err != nil {
		t.Fatalf("第 2 次购买应被允许: %v", err)
	}
	if _, err := service.commitCurrentCeraShopPurchase(ctx, session, catalog, cart(100056)); err == nil {
		t.Fatal("第 3 次购买必须被拒")
	}
}

// 账号维度限购只在道具说明文案里（例如 490702494“每个帐号限购1个。”），所以按
// 保守模板解析；解析结果要能当账号维度策略用，并且落在账号记录上。
func TestCurrentCeraShopExplainAccountLimitParsesProseAndCountsPerAccount(t *testing.T) {
	cases := []struct {
		explain string
		want    uint32
		found   bool
	}{
		{"该礼包只能由枪剑士购买及开启， 每个帐号限购1个。", 1, true},
		{"每个帐号限购1个哦！", 1, true},
		{"该礼盒每个帐号仅限购买1次， 并且获得的道具为帐号绑定。", 1, true},
		{"(购买契约道具后， 立即适用于帐号上的所有角色； 每个帐号限购买一次)", 0, false}, // 中文数词不猜
		{"每个角色只能购买1次。", 0, false},                         // 角色维度走 [buy restrict item]
		{"使用后， 可以恢复10点疲劳值。", 0, false},
		{"", 0, false},
	}
	for _, want := range cases {
		got, found := currentCeraShopExplainAccountLimit(want.explain)
		if found != want.found || got != want.want {
			t.Fatalf("explain=%q got=%d/%t want=%d/%t", want.explain, got, found, want.want, want.found)
		}
	}

	ctx := context.Background()
	source := currentCeraShopTestSource()
	source["stackable/test_potion.stk"] += "[explain]\n`测试礼包， 每个帐号限购1个。`\n"
	catalog, err := newPVFCeraShopCatalog(source)
	if err != nil {
		t.Fatal(err)
	}
	repositories := dnfrepomemory.NewMemoryGroup()
	seedCeraShopAccountCera(t, ctx, repositories, "account-1", 1000)
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "19", AccountID: "account-1", Level: 90,
		Stats: map[string]int64{"cera": 1000},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "19", Slots: map[string]dnfrepo.ItemStack{},
	}); err != nil {
		t.Fatal(err)
	}
	service := &Service{
		options:            options{accountID: "account-1"},
		ceraShopCatalog:    catalog,
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
	}
	session := &gameSession{selectedCharacterID: 19}
	buy := func() (currentCeraShopCommitResult, error) {
		return service.commitCurrentCeraShopPurchase(ctx, session, catalog, currentCeraShopPurchaseRequest{
			PaymentMode: currentCeraShopPaymentModeCera,
			Items:       []currentCeraShopPurchaseRequestItem{{CommodityID: 100050}},
		})
	}
	first, err := buy()
	if err != nil {
		t.Fatalf("第一次购买被拒: %v", err)
	}
	if len(first.BuyRestrictNotes) != 1 || !strings.Contains(first.BuyRestrictNotes[0], "account:explain[account]:100050:1/1") {
		t.Fatalf("账号维度限购没有记账: %+v", first.BuyRestrictNotes)
	}
	account, _, err := repositories.Account.Load(ctx, "account-1")
	if err != nil {
		t.Fatal(err)
	}
	definition, err := catalog.items.ResolveItem(37)
	if err != nil {
		t.Fatal(err)
	}
	policy, found := currentCeraShopExplainAccountPolicy(definition)
	if !found {
		t.Fatal("账号限购策略丢失")
	}
	if state := purchaselimit.ReadAccount(account.Metadata, policy, service.currentPurchaseLimitTime()); state.Used != 1 {
		t.Fatalf("账号限购计数=%+v metadata=%+v", state, account.Metadata)
	}
	// 同一账号的**另一个角色**也不能再买：这是账号维度，不是角色维度。
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "20", AccountID: "account-1", Level: 90,
		Stats: map[string]int64{"cera": 1000},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "20", Slots: map[string]dnfrepo.ItemStack{},
	}); err != nil {
		t.Fatal(err)
	}
	otherSession := &gameSession{selectedCharacterID: 20}
	if _, err := service.commitCurrentCeraShopPurchase(ctx, otherSession, catalog, currentCeraShopPurchaseRequest{
		PaymentMode: currentCeraShopPaymentModeCera,
		Items:       []currentCeraShopPurchaseRequestItem{{CommodityID: 100050}},
	}); err == nil {
		t.Fatal("账号维度限购必须跨角色生效")
	}
}

// IPG 商品（商品行最后一个字段非 0）在商城显示"只能购买一次"（现场：商品 102237 宠物
// 胶囊 2683241）。但契约类（`[premium]` 行 Marker=99）也带 IPG 商品号，玩家本来就要
// 反复续费，绝不能给它们压"账号 1 次"。
func TestCurrentCeraShopIPGGoodsAreAccountLimitedOnceButContractsAreNot(t *testing.T) {
	source := currentCeraShopTestSourceWithLimitRow(currentCeraShopTestSource(),
		"104901 37 1 0 0 9800 `宠物胶囊` 0 70052")
	source["etc/cerashop.etc"] = strings.Replace(source["etc/cerashop.etc"], "[/premium]",
		"100999 44 1 0 0 1080 `测试契约` 99 50099\n[/premium]", 1)
	catalog, err := newPVFCeraShopCatalog(source)
	if err != nil {
		t.Fatal(err)
	}
	goods, found := catalog.Product(104901)
	if !found || goods.IPGProductID != 70052 || goods.Marker != 0 {
		t.Fatalf("IPG 商品行解析=%+v found=%t", goods, found)
	}
	policies := catalog.BuyRestrictPolicies(104901)
	if len(policies) != 1 || policies[0].Scope != purchaselimit.ScopeAccount || policies[0].Limit != 1 ||
		policies[0].ItemID != 104901 {
		t.Fatalf("IPG 商品策略=%+v", policies)
	}
	contract, found := catalog.Product(100999)
	if !found || contract.IPGProductID != 50099 || contract.Marker != currentCeraShopContractMarker {
		t.Fatalf("契约行解析=%+v found=%t", contract, found)
	}
	if got := catalog.BuyRestrictPolicies(100999); len(got) != 0 {
		t.Fatalf("契约类不应被限购: %+v", got)
	}
}

// 走完整 op64 链路：第二次购买必须回商城失败包（形状固定），而第一次是成功包。
func TestCurrentCeraShopBuyLimitRejectionSendsFailurePacket(t *testing.T) {
	ctx := context.Background()
	source := currentCeraShopTestSource()
	source["etc/cerashop.etc"] += "[buy restrict item]\n13 100050 1 1\n[/buy restrict item]\n"
	catalog, err := newPVFCeraShopCatalog(source)
	if err != nil {
		t.Fatal(err)
	}
	repositories := dnfrepomemory.NewMemoryGroup()
	seedCeraShopAccountCera(t, ctx, repositories, "account-1", 1000)
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "19", AccountID: "account-1", Level: 90,
		Stats: map[string]int64{"cera": 1000},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "19", Slots: map[string]dnfrepo.ItemStack{},
	}); err != nil {
		t.Fatal(err)
	}
	connection := &bufferConn{}
	service := &Service{
		options: options{
			accountID:          "account-1",
			gameUpperHeader:    gameUpperHeaderChannel13,
			gameUpperBodyCodec: gameUpperBodyCodecPlain,
		},
		ceraShopCatalog:    catalog,
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
	}
	session := &gameSession{conn: connection, connID: "cera-shop-limit", selectedCharacterID: 19}
	// 买满的那一刻，服务端会在购买回包**之前**先发"限购商品列表"（类 0 通知 860），
	// 让客户端重建商品列表时就把这一格去掉。所以这里把通知与回包一起读出来。
	notifications := make([]dnfproto.ChannelPacket, 0, 1)
	buy := func() ([]dnfproto.ChannelPacket, uint16, []byte) {
		t.Helper()
		connection.write.Reset()
		if err := service.handleGameCommand(
			session,
			byte(dnfenum.GameCmdCommand),
			uint16(dnfenum.CmdPacketBuyCerashopItem),
			currentCeraShopTestRequestBody(100050),
		); err != nil {
			t.Fatal(err)
		}
		rest := connection.write.Bytes()
		notifications = notifications[:0]
		var ack dnfproto.ChannelPacket
		for len(rest) > 0 {
			packet, trailing := splitGameServerUpperPacket(t, rest)
			rest = trailing
			if packet.Header.MsgID == currentCeraShopBuyRestrictMsgID {
				notifications = append(notifications, packet)
				continue
			}
			ack = packet
			break
		}
		return notifications, ack.Header.MsgID, ack.Body
	}

	first, msgID, body := buy()
	if msgID != uint16(dnfenum.CmdPacketBuyCerashopItem) || len(body) == 0 || body[0] != 1 {
		t.Fatalf("第一次购买 ACK msg_id=%d body=%x", msgID, body)
	}
	// 限购 1 次的商品买一次就满：通知里必须有它的**限购条目号**与商品号（客户端按条目
	// 号判定商品行状态，商品号是 Marker=101 商品的兜底键），到期时间用"永久限制"。
	if len(first) != 1 {
		t.Fatalf("买满后应发一次限购列表通知，实得 %d 条", len(first))
	}
	noti := first[0].Body
	if len(noti) > 3 && noti[0] == 0 && noti[1] == 0 && noti[2] == 0 {
		noti = noti[3:]
	}
	if len(noti) != 4+12*2 {
		t.Fatalf("限购列表通知体=%x", first[0].Body)
	}
	if binary.LittleEndian.Uint32(noti[0:4]) != 2 ||
		binary.LittleEndian.Uint32(noti[4:8]) != 13 ||
		binary.LittleEndian.Uint32(noti[8:12]) != currentCeraShopBuyRestrictPermanentEndTime ||
		binary.LittleEndian.Uint32(noti[16:20]) != 100050 {
		t.Fatalf("限购列表通知体=%x", noti)
	}
	second, msgID, body := buy()
	if msgID != uint16(dnfenum.CmdPacketBuyCerashopItem) {
		t.Fatalf("限购失败包 msg_id=%d body=%x", msgID, body)
	}
	// 被拒时不重发通知（状态没有变化），但要回商城失败包。
	if len(second) != 0 {
		t.Fatalf("被拒的购买不应重发限购通知: %d 条", len(second))
	}
	if want := buildCurrentCeraShopPurchaseFailureBodyWithCode(currentCeraShopPurchaseLimitFailureCode); !bytes.Equal(body, want) {
		t.Fatalf("限购失败包=%x want=%x", body, want)
	}
}

// 真机 PVF：把 `[item]` 段的整行（9 个字段，含最后两个尚未定性的字段）打出来，
// 用来判断"账号限购1次"这类限制到底写在哪个字段里。
//
// 现场线索（2026-09-18）：商品 104828/104830（弹刃之绚宠物礼包 / 欢乐暑假行程单）
// 商城里显示"账号限购1次"，但它们既不在 `[buy restrict item]` 里，道具 PVF 也没有
// `[purchase limit]`——所以要逐个字段对照。
func TestRealPVFCeraShopItemSectionTrailingFields(t *testing.T) {
	pvfPath := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE to the runtime Script.pvf")
	}
	archive, err := platformpvf.Open(pvfPath)
	if err != nil {
		t.Fatal(err)
	}
	document, err := parseDungeonCardPVFDocument(archive, "etc/cerashop.etc")
	if err != nil {
		t.Fatal(err)
	}
	for _, section := range []string{"item", "material", "recoveryitem", "creature", "coin", "premium"} {
		tokens, found := document.Section(section)
		if !found {
			continue
		}
		values := currentCeraShopPVFValues(tokens)
		const rowWidth = 9
		if len(values)%rowWidth != 0 {
			t.Fatalf("section=%s tokens=%d not a multiple of %d", section, len(values), rowWidth)
		}
		field8 := map[string]int{}
		rows := 0
		for offset := 0; offset+rowWidth <= len(values); offset += rowWidth {
			rows++
			field8[tokenText(values[offset+7])]++
		}
		t.Logf("段=%s 行数=%d 第8字段分布=%v", section, rows, field8)
		for offset := 0; offset+rowWidth <= len(values); offset += rowWidth {
			marker := tokenText(values[offset+7])
			if marker == "0" {
				continue
			}
			fields := make([]string, 0, rowWidth)
			for i := 0; i < rowWidth; i++ {
				fields = append(fields, tokenText(values[offset+i]))
			}
			t.Logf("  段=%s 商品=%s 道具=%s 数量=%s 点券=%s 名称=%s 第8=%s 第9=%s",
				section, fields[0], fields[1], fields[2], fields[5], fields[6], fields[7], fields[8])
		}
	}
	restrictRows := "\n"
	_ = restrictRows
	tokens, found := document.Section("item")
	if !found {
		t.Fatal("etc/cerashop.etc has no [item] section")
	}
	values := currentCeraShopPVFValues(tokens)
	const rowWidth = 9
	if len(values)%rowWidth != 0 {
		t.Fatalf("item section tokens=%d not a multiple of %d", len(values), rowWidth)
	}
	wanted := map[uint32]bool{104828: true, 104829: true, 104830: true, 101081: true, 104412: true, 100902: true}
	for offset := 0; offset+rowWidth <= len(values); offset += rowWidth {
		commodityID, _ := currentCeraShopPVFUint32(values[offset])
		if !wanted[commodityID] {
			continue
		}
		fields := make([]string, 0, rowWidth)
		for i := 0; i < rowWidth; i++ {
			fields = append(fields, tokenText(values[offset+i]))
		}
		t.Logf("商品 %s: %s", fields[0], strings.Join(fields[1:], " | "))
	}
	// 第9字段非 0 的取样，看它是不是 IPG 商品号。
	samples := 0
	for offset := 0; offset+rowWidth <= len(values) && samples < 8; offset += rowWidth {
		last := tokenText(values[offset+8])
		if last == "0" {
			continue
		}
		samples++
		t.Logf("第9字段非0 商品=%s 第8字段=%s 第9字段=%s 道具=%s",
			tokenText(values[offset]), tokenText(values[offset+7]), last, tokenText(values[offset+1]))
	}
}

func tokenText(token dnfpvf.Token) string {
	switch token.Kind {
	case dnfpvf.TokenString:
		return token.Value
	case dnfpvf.TokenIdent:
		return token.Value
	default:
		return strconv.FormatInt(token.Int, 10)
	}
}

func countNonZero(counts map[string]int) int {
	total := 0
	for key, value := range counts {
		if key == "0" || key == "" {
			continue
		}
		total += value
	}
	return total
}

// currentCeraShopTestSourceWithLimitRow 把一行商品追加进 `[item]` 段：用来构造带
// Marker（倒数第二个字段）的商品，例如 101 = 限购商品。
func currentCeraShopTestSourceWithLimitRow(source bridgePVFSource, row string) bridgePVFSource {
	source["etc/cerashop.etc"] = strings.Replace(source["etc/cerashop.etc"], "[/item]", row+"\n[/item]", 1)
	return source
}

// 商品行 Marker=101（限购商品）但既不在 `[buy restrict item]`、道具文案也没写限购时，
// 按“每个账号 1 次”兜底——这正是 104830（欢乐暑假行程单）在真机 PVF 里的样子。
func TestCurrentCeraShopMarkerLimitedCommodityIsAccountLimitedOnce(t *testing.T) {
	ctx := context.Background()
	source := currentCeraShopTestSourceWithLimitRow(currentCeraShopTestSource(),
		"104900 37 1 0 0 500 `limited package` 101 70001")
	catalog, err := newPVFCeraShopCatalog(source)
	if err != nil {
		t.Fatal(err)
	}
	product, found := catalog.Product(104900)
	if !found || product.Marker != currentCeraShopProductMarkerLimited {
		t.Fatalf("限购商品行解析=%+v found=%t", product, found)
	}
	policies := catalog.BuyRestrictPolicies(104900)
	if len(policies) != 1 || policies[0].Scope != purchaselimit.ScopeAccount || policies[0].Limit != 1 ||
		policies[0].Section != "cerashop_marker_limited" || policies[0].ItemID != 104900 {
		t.Fatalf("限购商品策略=%+v", policies)
	}
	// 普通商品行（Marker=0）不该背上限购。
	if got := catalog.BuyRestrictPolicies(100050); len(got) != 0 {
		t.Fatalf("普通商品不应有限购策略: %+v", got)
	}

	repositories := dnfrepomemory.NewMemoryGroup()
	seedCeraShopAccountCera(t, ctx, repositories, "account-1", 1000)
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "19", AccountID: "account-1", Level: 90,
		Stats: map[string]int64{"cera": 1000},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "19", Slots: map[string]dnfrepo.ItemStack{},
	}); err != nil {
		t.Fatal(err)
	}
	service := &Service{
		options:            options{accountID: "account-1"},
		ceraShopCatalog:    catalog,
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
	}
	session := &gameSession{selectedCharacterID: 19}
	buy := func() error {
		_, err := service.commitCurrentCeraShopPurchase(ctx, session, catalog, currentCeraShopPurchaseRequest{
			PaymentMode: currentCeraShopPaymentModeCera,
			Items:       []currentCeraShopPurchaseRequestItem{{CommodityID: 104900}},
		})
		return err
	}
	if err := buy(); err != nil {
		t.Fatalf("第一次购买被拒: %v", err)
	}
	if err := buy(); err == nil {
		t.Fatal("账号限购 1 次的商品第二次必须被拒")
	}
	// 账号维度：换一个角色也不能再买。
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "20", AccountID: "account-1", Level: 90,
		Stats: map[string]int64{"cera": 1000},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "20", Slots: map[string]dnfrepo.ItemStack{},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.commitCurrentCeraShopPurchase(ctx, &gameSession{selectedCharacterID: 20}, catalog, currentCeraShopPurchaseRequest{
		PaymentMode: currentCeraShopPaymentModeCera,
		Items:       []currentCeraShopPurchaseRequestItem{{CommodityID: 104900}},
	}); err == nil {
		t.Fatal("账号限购必须跨角色生效")
	}
}

// 买满的商城商品要按类 0 通知 860 下发给客户端（商品号 + 永久限制的到期时间），
// 这样客户端重建商品列表时就会把那一格去掉；没买满时不发包。
func TestCurrentCeraShopBuyRestrictProjectionSendsSoldOutCommodities(t *testing.T) {
	ctx := context.Background()
	source := currentCeraShopTestSourceWithLimitRow(currentCeraShopTestSource(),
		"104900 37 1 0 0 500 `limited package` 101 70001")
	source["etc/cerashop.etc"] += "[buy restrict item]\n13 100050 1 1\n[/buy restrict item]\n"
	catalog, err := newPVFCeraShopCatalog(source)
	if err != nil {
		t.Fatal(err)
	}
	repositories := dnfrepomemory.NewMemoryGroup()
	seedCeraShopAccountCera(t, ctx, repositories, "account-1", 1000)
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "19", AccountID: "account-1", Level: 90,
		Stats: map[string]int64{"cera": 1000},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "19", Slots: map[string]dnfrepo.ItemStack{},
	}); err != nil {
		t.Fatal(err)
	}
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
	session := &gameSession{conn: connection, connID: "cera-shop-soldout", selectedCharacterID: 19}

	// 什么都没买：不发包（空包会把客户端刚建立起的限制表清空）。
	if err := service.sendCurrentCeraShopBuyRestrictProjection(ctx, session, "unit_test_empty"); err != nil {
		t.Fatal(err)
	}
	if connection.write.Len() != 0 {
		t.Fatalf("没有买满的商品时不应发包: %x", connection.write.Bytes())
	}

	// 买满两件（账号维度 104900 / 角色维度 100050）后再投影：一次通知带两行。
	for _, commodityID := range []uint32{104900, 100050} {
		if _, err := service.commitCurrentCeraShopPurchase(ctx, session, catalog, currentCeraShopPurchaseRequest{
			PaymentMode: currentCeraShopPaymentModeCera,
			Items:       []currentCeraShopPurchaseRequestItem{{CommodityID: commodityID}},
		}); err != nil {
			t.Fatalf("购买商品 %d 失败: %v", commodityID, err)
		}
	}
	connection.write.Reset()
	if err := service.sendCurrentCeraShopBuyRestrictProjection(ctx, session, "unit_test_soldout"); err != nil {
		t.Fatal(err)
	}
	packet, trailing := splitGameServerUpperPacket(t, connection.write.Bytes())
	if len(trailing) != 0 || packet.Header.Classification != 0 ||
		packet.Header.MsgID != currentCeraShopBuyRestrictMsgID {
		t.Fatalf("限购列表通知 header=%+v trailing=%x", packet.Header, trailing)
	}
	// server16 头模式会在包体前带固定的 3 个 0 字节；通知体本身是最后的 4+12n 字节。
	body := packet.Body
	if len(body) > 3 && body[0] == 0 && body[1] == 0 && body[2] == 0 {
		body = body[3:]
	}
	// 两件买满：100050 有限购表行（序号 13）→ 序号+商品号两条；104900 只有 Marker →
	// 只有商品号一条。合计 3 条。
	if len(body) != 4+12*3 {
		t.Fatalf("限购列表通知体=%x", packet.Body)
	}
	if count := binary.LittleEndian.Uint32(body[0:4]); count != 3 {
		t.Fatalf("限购列表条数=%d", count)
	}
	if got := []uint32{
		binary.LittleEndian.Uint32(body[4:8]),
		binary.LittleEndian.Uint32(body[16:20]),
		binary.LittleEndian.Uint32(body[28:32]),
	}; got[0] != 13 || got[1] != 100050 || got[2] != 104900 {
		t.Fatalf("限购列表条目号=%v", got)
	}
	for _, offset := range []int{8, 20, 32} {
		if end := binary.LittleEndian.Uint32(body[offset : offset+4]); end != currentCeraShopBuyRestrictPermanentEndTime {
			t.Fatalf("到期时间=%d", end)
		}
	}
}

// 真机 PVF：`[buy restrict item]` 的次数列必须与运营截图一致——商品 101081
// （深渊派对通行证）1 次、104412（防虚弱药剂）3 次。这两件商品的道具 PVF 里都
// 没有 `[purchase limit]`，所以这条断言同时守住了“商城限购只能从这张表来”。
func TestRealPVFCeraShopBuyRestrictTable(t *testing.T) {
	pvfPath := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE to the runtime Script.pvf")
	}
	archive, err := platformpvf.Open(pvfPath)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := newPVFCeraShopCatalog(archive)
	if err != nil {
		t.Fatal(err)
	}
	for commodityID, wantLimit := range map[uint32]uint32{101081: 1, 104412: 3} {
		policy, found := catalog.BuyRestrictCharacterPolicy(commodityID)
		if !found || policy.Limit != wantLimit {
			t.Fatalf("商品 %d 限购=%+v found=%t want=%d", commodityID, policy, found, wantLimit)
		}
		product, found := catalog.Product(commodityID)
		if !found {
			t.Fatalf("商品 %d 不在商品表里", commodityID)
		}
		definition, err := catalog.items.ResolveItem(product.ItemID)
		if err != nil {
			t.Fatalf("商品 %d 的道具 %d 解析失败: %v", commodityID, product.ItemID, err)
		}
		if definition.PurchaseLimitFound {
			t.Fatalf("商品 %d 的道具 %d 竟然有 [purchase limit]：%+v",
				commodityID, product.ItemID, definition.PurchaseLimit)
		}
	}
	// 商城道具里凡是有机器可读 `[purchase limit]` 的，仍然以字段为准；这条只统计，
	// 便于日后确认两条来源没有打架。
	conflicts := 0
	for commodityID, row := range catalog.BuyRestrictRows() {
		product, found := catalog.Product(commodityID)
		if !found {
			continue
		}
		definition, err := catalog.items.ResolveItem(product.ItemID)
		if err != nil || !definition.PurchaseLimitFound {
			continue
		}
		conflicts++
		t.Logf("商品 %d 同时有限购表(%d 次)与 [purchase limit](%+v)", commodityID, row.CharacterLimit, definition.PurchaseLimit)
	}
	t.Logf("限购表行数=%d 同时有 PVF 字段的商品=%d", len(catalog.BuyRestrictRows()), conflicts)
	// 第 3 列（UnknownField）语义未证实，因此**不参与限购**；把非 0/1 的行打出来，
	// 便于日后拿到证据再决定要不要用。
	oddRows := 0
	for _, commodityID := range sortedCeraShopCommodityIDs(catalog) {
		row, found := catalog.BuyRestrictRows()[commodityID]
		if !found || row.UnknownField == 0 || row.UnknownField == 1 {
			continue
		}
		oddRows++
		if oddRows <= 10 {
			t.Logf("限购表未知列 商品=%d 未知列=%d 次数=%d", commodityID, row.UnknownField, row.CharacterLimit)
		}
	}
	t.Logf("限购表里未知列不是 0/1 的行=%d（这些行只按第 4 列限购）", oddRows)

	// 说明文案解析出的账号限购：逐条打出来供人工复核（文案解析天生比字段解析脆，
	// 所以上线前必须能一条条看到它到底认了哪些商品）。
	explainHits := 0
	for _, commodityID := range sortedCeraShopCommodityIDs(catalog) {
		product, found := catalog.Product(commodityID)
		if !found {
			continue
		}
		definition, err := catalog.items.ResolveItem(product.ItemID)
		if err != nil {
			continue
		}
		policy, found := currentCeraShopExplainAccountPolicy(definition)
		if !found {
			continue
		}
		explainHits++
		if explainHits <= 20 {
			t.Logf("账号限购 商品=%d 道具=%d 次数=%d pvf=%s", commodityID, product.ItemID, policy.Limit, definition.PVFPath)
		}
	}
	t.Logf("说明文案解析出账号限购的商品数=%d / 商品总数=%d", explainHits, len(catalog.products))

	// Marker==101（限购商品）与 IPG 商品（IPGProductID != 0）的总账：这些商品各拿到
	// 什么策略，一条条打出来。
	markerHits := 0
	ipgHits := 0
	for _, commodityID := range sortedCeraShopCommodityIDs(catalog) {
		product, found := catalog.Product(commodityID)
		if !found {
			continue
		}
		if product.IPGProductID != 0 {
			ipgHits++
		}
		if product.Marker != currentCeraShopProductMarkerLimited && product.IPGProductID == 0 {
			continue
		}
		markerHits++
		notes := make([]string, 0, 2)
		for _, policy := range catalog.BuyRestrictPolicies(commodityID) {
			notes = append(notes, fmt.Sprintf("%s/%s=%d", policy.Scope, policy.Section, policy.Limit))
		}
		t.Logf("限购商品 商品=%d 道具=%d 分段=%s Marker=%d IPG=%d 策略=%v",
			commodityID, product.ItemID, product.Section, product.Marker, product.IPGProductID, notes)
	}
	t.Logf("Marker=101 或 IPG 非0 的商品数=%d（其中 IPG 非0 的 %d 件）", markerHits, ipgHits)
}
