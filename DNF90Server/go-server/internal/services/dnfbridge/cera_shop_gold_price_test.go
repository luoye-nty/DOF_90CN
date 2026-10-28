package dnfbridge

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfproto "longheng.io/server/internal/modules/dnf/protocol"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
	mysqlrepo "longheng.io/server/internal/modules/dnf/repository/mysql"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

// 现场：角色 9999 / commodity 104824（热舞一夏称号随机变更券）/ 2026-10-01 16:44:15。
//
// `etc/cerashop.etc` 九字段商品行是 `commodity item count gold point Cera name marker ipg`，
// 改造前解析器**只读 Cera 列**，于是金币标价行（gold 非 0、Cera=0）被当成免费商品：
// 买得到、任何钱包都不扣。下面这些用例就是钉死"金币要扣、且点券那条路一个字都不许变"。

const (
	ceraShopGoldTestCommodity     uint32 = 100910 // 金币标价 500000
	ceraShopGoldTestCommodityBig  uint32 = 100911 // 金币标价 800000
	ceraShopGoldTestCommodityFree uint32 = 100912 // 三个价格列全 0
	ceraShopGoldTestPremiumCommod uint32 = 100913 // [premium] 段的金币标价行
	ceraShopGoldTestPrice                = 500000
	ceraShopGoldTestCeraCommodity uint32 = 100050 // 既有夹具里的点券商品（80 点券）
	ceraShopGoldTestVisualCommod  uint32 = 100057 // 既有夹具里的 [visual]（8 字段，token3=30 天）
)

// currentCeraShopGoldTestSource 在既有共用夹具上**追加**行，不改动任何既有行 ——
// 共用夹具的断言（assertCurrentCeraShopTestCatalogProducts）因此仍然成立。
func currentCeraShopGoldTestSource() bridgePVFSource {
	source := currentCeraShopTestSource()
	source["etc/cerashop.etc"] = strings.Replace(source["etc/cerashop.etc"], "[/item]",
		"100910 37 1 500000 0 0 `gold priced test potion` 0 0\n"+
			"100911 37 1 800000 0 0 `gold priced stack` 0 0\n"+
			"100912 37 1 0 0 0 `free test potion` 0 0\n"+
			"[/item]", 1)
	source["etc/cerashop.etc"] = strings.Replace(source["etc/cerashop.etc"], "[/premium]",
		"100913 44 1 1500000 0 0 `gold priced contract` 0 0\n[/premium]", 1)
	return source
}

func mustCurrentCeraShopGoldTestCatalog(t *testing.T) *pvfCeraShopCatalog {
	t.Helper()
	catalog, err := newPVFCeraShopCatalog(currentCeraShopGoldTestSource())
	if err != nil {
		t.Fatal(err)
	}
	return catalog
}

// TestCurrentCeraShopCatalogReadsGoldColumnOnlyForNineTokenRows 是本改动最关键的一条
// 回归钉子：**只有九字段行**的第 4 个 token 是金币。
//
// [visual] 是 8 字段行，它的 token3 是**时长天数**（夹具里是 30）。如果解析器把行宽
// 判断放宽成 `rowWidth >= 4` 之类，30 就会被读成"30 金币"—— 买一件 30 天时装变成扣
// 30 金币，这就是"修 A 坏 B"最典型的形态。这里把三种非九字段行全部钉住。
func TestCurrentCeraShopCatalogReadsGoldColumnOnlyForNineTokenRows(t *testing.T) {
	catalog := mustCurrentCeraShopGoldTestCatalog(t)

	gold, found := catalog.Product(ceraShopGoldTestCommodity)
	if !found || gold.GoldPrice != ceraShopGoldTestPrice || gold.CeraPrice != 0 || gold.ItemID != 37 || gold.Section != "item" {
		t.Fatalf("gold priced product=%+v found=%t", gold, found)
	}
	if big, found := catalog.Product(ceraShopGoldTestCommodityBig); !found || big.GoldPrice != 800000 || big.CeraPrice != 0 {
		t.Fatalf("second gold priced product=%+v found=%t", big, found)
	}
	// 九字段的 [premium] 段同样带金币列。
	if premiumProduct, found := catalog.Product(ceraShopGoldTestPremiumCommod); !found ||
		premiumProduct.GoldPrice != 1500000 || premiumProduct.CeraPrice != 0 || premiumProduct.Section != "premium" {
		t.Fatalf("premium gold product=%+v found=%t", premiumProduct, found)
	}
	// 三个价格列全 0 的行必须保持"免费"，不能被当成价格缺失而拒绝。
	if free, found := catalog.Product(ceraShopGoldTestCommodityFree); !found || free.GoldPrice != 0 || free.CeraPrice != 0 {
		t.Fatalf("free product=%+v found=%t", free, found)
	}

	// 既有 177 行点券商品：金币列必须是 0，否则会凭空多扣金币。
	if cera, found := catalog.Product(ceraShopGoldTestCeraCommodity); !found || cera.CeraPrice != 80 || cera.GoldPrice != 0 {
		t.Fatalf("cera priced product=%+v found=%t", cera, found)
	}

	// ★ [visual] 8 字段：token3 是时长天数，不是金币。
	visual, found := catalog.Product(ceraShopGoldTestVisualCommod)
	if !found {
		t.Fatal("visual fixture product is missing")
	}
	if visual.GoldPrice != 0 {
		t.Fatalf("visual row leaked its duration into the gold column: %+v", visual)
	}
	if visual.DurationDays != 30 || visual.CeraPrice != 60 || visual.Section != "visual" {
		t.Fatalf("visual row changed shape: %+v", visual)
	}

	// [package] 11 字段：Cera 在 token4，token3 不是金币。
	if pkg, found := catalog.Product(100060); !found || pkg.GoldPrice != 0 || pkg.CeraPrice != 50 || pkg.Section != "package" {
		t.Fatalf("package product=%+v found=%t", pkg, found)
	}
	// [avatar] 6 字段：价格来自道具 PVF，金币列必须为 0。
	if avatar, found := catalog.Product(100070); !found || avatar.GoldPrice != 0 || avatar.CeraPrice != 30 || avatar.Section != "avatar" {
		t.Fatalf("avatar product=%+v found=%t", avatar, found)
	}
}

// TestRealPVFCeraShopGoldPricedCommoditiesAreCharged 用**线上正在跑的那份**
// Script.pvf 验证金币列真的被读出来了。
//
// 现场（2026-10-01）本服 708 行普通商品里只有 5 行是金币标价、且 Cera 列一律写 0：
//
//	104824 → 490702318 热舞一夏称号随机变更券  500000
//	104820 → 490702319 热舞一夏称号宝珠        500000
//	104822 → 490702376 迷你染色剂礼盒          200000
//	103155 → 10007367  幸运魔锤                800000
//	103061 → 10000389  晶之契约(7天)          1500000
//
// 改造前这 5 件的 GoldPrice 全是 0（= 免费无限买）。同时这条用例还钉住**反向**约束：
// `[visual]` / `[package]` / `[avatar]` 三种行结构上就不许出现金币价 ——
// 一旦有人把行宽判断放宽，这里会立刻红。
func TestRealPVFCeraShopGoldPricedCommoditiesAreCharged(t *testing.T) {
	pvfPath := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE to verify the real gold-priced commodities")
	}
	archive, err := platformpvf.LoadArchive(platformpvf.Options{Path: pvfPath})
	if err != nil {
		t.Fatalf("load real Script.pvf: %v", err)
	}
	catalog, err := newPVFCeraShopCatalog(archive)
	if err != nil {
		t.Fatalf("load real Cera-shop catalog: %v", err)
	}

	for _, want := range []struct {
		commodity uint32
		item      uint32
		gold      uint32
	}{
		{104824, 490702318, 500000}, // 现场那一单
		{104820, 490702319, 500000},
		{104822, 490702376, 200000},
		{103155, 10007367, 800000},
		{103061, 10000389, 1500000},
	} {
		product, found := catalog.Product(want.commodity)
		if !found {
			t.Fatalf("live commodity %d is absent from etc/cerashop.etc", want.commodity)
		}
		if product.ItemID != want.item || product.GoldPrice != want.gold || product.CeraPrice != 0 {
			t.Fatalf("live commodity %d = %+v, want item=%d gold=%d cera=0",
				want.commodity, product, want.item, want.gold)
		}
	}

	// 点券商品一个都不许被读成金币（现场 102644 装扮栏扩展券(1阶段) = 1980 点券）。
	if cera, found := catalog.Product(102644); !found || cera.GoldPrice != 0 || cera.CeraPrice != 1980 {
		t.Fatalf("live cera commodity 102644 = %+v found=%t", cera, found)
	}

	// 结构性反向约束：只有九字段段（item/material/premium/creature/coin/recoveryitem）
	// 才有金币列。这三种行的 token3 另有含义，必须一个金币价都不许出现。
	goldBearing := map[string]bool{
		"item": true, "material": true, "premium": true, "creature": true,
		"coin": true, "recoveryitem": true, "community item": true,
		"communityitem": true, "regular item": true, "regularitem": true,
	}
	goldRows := 0
	for _, product := range catalog.products {
		if product.GoldPrice == 0 {
			continue
		}
		goldRows++
		if !goldBearing[product.Section] {
			t.Errorf("section=%s commodity=%d carries a gold price but is not a nine-token row: %+v",
				product.Section, product.CommodityID, product)
		}
	}
	if goldRows == 0 {
		t.Fatal("real Cera-shop catalog read no gold prices at all")
	}
	// [visual] 的 token3 是时长天数：真机上这一批的时长都是 30/7 这类小数字，
	// 一旦泄漏成金币价，上面那个循环会以 section=visual 报错。这里再显式点一次名。
	for _, product := range catalog.products {
		if product.Section == "visual" && product.DurationDays == 0 && product.GoldPrice != 0 {
			t.Errorf("visual commodity %d has a gold price without a duration: %+v", product.CommodityID, product)
		}
	}
}

// TestLiveMySQLCeraShopGoldPurchasePersistsCharacterGoldColumn 是唯一一条跑在**真
// MySQL** 上的端到端用例（默认跳过；设 DNF_MYSQL_LIVE_DSN 才跑）。
//
// 它关掉内存仓储看不出来的最后一个缺口：角色金币住在 `dnf_characters.gold` 这一列，
// 而 `Checkout` 只在 `CheckoutChanges.Character` 为真时才写角色行。少写这一列的表现
// 是"结账当时扣了、重启服务端又回来"—— 只有在真库上跑一遍才能钉死。
//
// 与既有 live 用例同一套约定：**自己建一个一次性库、跑完自己删**，绝不碰线上 dnf_local。
func TestLiveMySQLCeraShopGoldPurchasePersistsCharacterGoldColumn(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("DNF_MYSQL_LIVE_DSN"))
	if dsn == "" {
		t.Skip("DNF_MYSQL_LIVE_DSN not set")
	}
	catalog := mustCurrentCeraShopGoldTestCatalog(t)
	ctx := context.Background()
	database, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	scratch := fmt.Sprintf("dnf_goldfix_%d", time.Now().UnixNano())
	plan := dnfrepo.DatabasePlan{
		Feature: dnfrepo.FeatureRepository, ShardID: "9999", GroupID: "live",
		ReadDatabases: []string{scratch}, WriteDatabases: []string{scratch},
	}
	if err := mysqlrepo.EnsureMySQLSchema(ctx, mysqlrepo.SQLDBAdapter{DB: database}, mysqlrepo.SchemaOptions{
		AutoCreate: true, CreateDatabases: true, TablePrefix: "dnf", DatabasePlan: plan,
	}); err != nil {
		t.Fatalf("create scratch schema: %v", err)
	}
	t.Cleanup(func() {
		if _, err := database.ExecContext(context.Background(), "DROP DATABASE `"+scratch+"`"); err != nil {
			t.Errorf("drop scratch database %s: %v", scratch, err)
		}
	})
	repositories, err := mysqlrepo.NewMySQLGroupFromDB(database, mysqlrepo.MySQLGroupOptions{DatabasePlan: plan, TablePrefix: "dnf"})
	if err != nil {
		t.Fatal(err)
	}

	const startGold = int64(2_000_000)
	if err := repositories.Account.Save(ctx, dnfrepo.AccountRecord{
		AccountID: "account-1",
		Metadata:  map[string]string{"account_cera": "200"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "19",
		AccountID:   "account-1",
		Name:        "goldfix",
		Stats:       map[string]int64{"cera": 0, "gold": startGold},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{CharacterID: "19", Slots: map[string]dnfrepo.ItemStack{}}); err != nil {
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
	session := &gameSession{conn: connection, connID: "cera-shop-mysql-test", selectedCharacterID: 19}
	if err := service.handleGameCommand(
		session,
		byte(dnfenum.GameCmdCommand),
		uint16(dnfenum.CmdPacketBuyCerashopItem),
		currentCeraShopTestRequestBody(ceraShopGoldTestCommodity),
	); err != nil {
		t.Fatal(err)
	}

	// 重新从 MySQL 读一遍，绕开任何内存态。
	character, found, err := repositories.Character.Load(ctx, "19")
	if err != nil || !found {
		t.Fatalf("load character found=%t err=%v", found, err)
	}
	if want := startGold - ceraShopGoldTestPrice; character.Stats["gold"] != want {
		t.Fatalf("dnf_characters.gold=%d, want %d (changes.Character must persist the gold column)", character.Stats["gold"], want)
	}
	account, found, err := repositories.Account.Load(ctx, "account-1")
	if err != nil || !found {
		t.Fatalf("load account found=%t err=%v", found, err)
	}
	if got := currentAccountCera(account); got != 200 {
		t.Fatalf("account_cera=%d, want untouched 200", got)
	}
	inventory, found, err := repositories.Inventory.Load(ctx, "19")
	if err != nil || !found {
		t.Fatalf("load inventory found=%t err=%v", found, err)
	}
	granted := false
	for _, stack := range inventory.Slots {
		if stack.ItemID == 37 && stack.Count == 1 {
			granted = true
		}
	}
	if !granted {
		t.Fatalf("item was not persisted: %+v", inventory.Slots)
	}
}

// ceraShopGoldTestFixture 起一个与既有商城用例同形的服务与仓储。
func ceraShopGoldTestFixture(t *testing.T, catalog *pvfCeraShopCatalog, characterGold int64, accountCera int64) (context.Context, dnfrepo.Group, *Service, *gameSession, *bufferConn) {
	t.Helper()
	repositories := dnfrepomemory.NewMemoryGroup()
	ctx := context.Background()
	seedCeraShopAccountCera(t, ctx, repositories, "account-1", accountCera)
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "19",
		AccountID:   "account-1",
		Stats:       map[string]int64{"cera": 0, "gold": characterGold},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{CharacterID: "19", Slots: map[string]dnfrepo.ItemStack{}}); err != nil {
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
	session := &gameSession{conn: connection, connID: "cera-shop-gold-test", selectedCharacterID: 19}
	return ctx, repositories, service, session, connection
}

func loadCeraShopGoldTestCharacterGold(t *testing.T, ctx context.Context, repositories dnfrepo.Group) int64 {
	t.Helper()
	character, found, err := repositories.Character.Load(ctx, "19")
	if err != nil || !found {
		t.Fatalf("load character found=%t err=%v", found, err)
	}
	return character.Stats["gold"]
}

// ceraShopTestPackets 把一次命令写出的全部上行包拆开。
func ceraShopTestPackets(t *testing.T, raw []byte) []dnfproto.ChannelPacket {
	t.Helper()
	packets := make([]dnfproto.ChannelPacket, 0, 4)
	rest := raw
	for len(rest) > 0 {
		var packet dnfproto.ChannelPacket
		packet, rest = splitGameServerUpperPacket(t, rest)
		packets = append(packets, packet)
	}
	return packets
}

func ceraShopTestBodyCount(packets []dnfproto.ChannelPacket, msgID uint16, body []byte) int {
	count := 0
	for _, packet := range packets {
		if packet.Header.MsgID == msgID && string(packet.Body) == string(body) {
			count++
		}
	}
	return count
}

// TestCurrentCeraShopPurchaseChargesGoldForGoldPricedCommodity 是现场那一单的复刻：
// 金币标价商品必须**正好扣标价的金币**，点券一分不动，道具照发，并且补发 op14 金币包。
func TestCurrentCeraShopPurchaseChargesGoldForGoldPricedCommodity(t *testing.T) {
	catalog := mustCurrentCeraShopGoldTestCatalog(t)
	const startGold = int64(2_000_000)
	ctx, repositories, service, session, connection := ceraShopGoldTestFixture(t, catalog, startGold, 200)

	if err := service.handleGameCommand(
		session,
		byte(dnfenum.GameCmdCommand),
		uint16(dnfenum.CmdPacketBuyCerashopItem),
		currentCeraShopTestRequestBody(ceraShopGoldTestCommodity),
	); err != nil {
		t.Fatal(err)
	}

	ack, rest := splitGameServerUpperPacket(t, connection.write.Bytes())
	if ack.Header.Classification != dnfproto.DefaultChannelClassification || ack.Header.MsgID != uint16(dnfenum.CmdPacketBuyCerashopItem) {
		t.Fatalf("purchase ACK header=%+v", ack.Header)
	}
	if want := buildCurrentCeraShopPurchaseSuccessBodyWithCount(ceraShopGoldTestCommodity, 1); string(ack.Body) != string(want) {
		t.Fatalf("purchase ACK body=%x want=%x", ack.Body, want)
	}

	wantGoldAfter := startGold - ceraShopGoldTestPrice
	if got := loadCeraShopGoldTestCharacterGold(t, ctx, repositories); got != wantGoldAfter {
		t.Fatalf("persisted character gold=%d, want %d (the gold-priced row must be charged)", got, wantGoldAfter)
	}
	// 点券标价列是 0：账号点券一分都不许动。
	if got := loadCeraShopAccountCera(t, ctx, repositories, "account-1"); got != 200 {
		t.Fatalf("account cera=%d, want untouched 200", got)
	}

	inventory, found, err := repositories.Inventory.Load(ctx, "19")
	if err != nil || !found {
		t.Fatalf("load inventory found=%t err=%v", found, err)
	}
	stack, found := inventory.Slots["0:3"]
	if !found || stack.ItemID != 37 || stack.Count != 1 {
		t.Fatalf("granted stack=%+v found=%t", stack, found)
	}

	// 扣了金币就必须把新余额推给客户端：op14（list0 slot0 item0 count=金币）。
	// 不发这一包，玩家看到的仍是"买了东西金币没少"。
	packets := ceraShopTestPackets(t, rest)
	if count := ceraShopTestBodyCount(packets, uint16(dnfenum.CmdPacketWalkoutPartyMember), buildCurrentGoldStateBody(wantGoldAfter)); count != 1 {
		t.Fatalf("gold wallet packet count=%d, want 1 (packets=%d)", count, len(packets))
	}
}

// TestCurrentCeraShopPurchaseRejectsInsufficientGoldWithoutDebit：金币不够必须整笔回滚，
// 回码 13「金币不足」（不是"点券不足"），且不能发货。
func TestCurrentCeraShopPurchaseRejectsInsufficientGoldWithoutDebit(t *testing.T) {
	catalog := mustCurrentCeraShopGoldTestCatalog(t)
	const startGold = int64(100) // 远低于 500000
	ctx, repositories, service, session, connection := ceraShopGoldTestFixture(t, catalog, startGold, 200)

	if err := service.handleGameCommand(
		session,
		byte(dnfenum.GameCmdCommand),
		uint16(dnfenum.CmdPacketBuyCerashopItem),
		currentCeraShopTestRequestBody(ceraShopGoldTestCommodity),
	); err != nil {
		t.Fatal(err)
	}

	ack, rest := splitGameServerUpperPacket(t, connection.write.Bytes())
	if want := buildCurrentCeraShopPurchaseFailureBodyWithCode(currentCeraShopPurchaseFailureCodeInsufficientGold); string(ack.Body) != string(want) {
		t.Fatalf("failure body=%x want=%x", ack.Body, want)
	}
	if len(rest) != 0 {
		t.Fatalf("rejected purchase still emitted %d trailing bytes", len(rest))
	}
	if got := loadCeraShopGoldTestCharacterGold(t, ctx, repositories); got != startGold {
		t.Fatalf("character gold=%d, want untouched %d", got, startGold)
	}
	if got := loadCeraShopAccountCera(t, ctx, repositories, "account-1"); got != 200 {
		t.Fatalf("account cera=%d, want untouched 200", got)
	}
	inventory, found, err := repositories.Inventory.Load(ctx, "19")
	if err != nil || !found {
		t.Fatalf("load inventory found=%t err=%v", found, err)
	}
	if len(inventory.Slots) != 0 {
		t.Fatalf("rejected purchase granted items: %+v", inventory.Slots)
	}
}

// TestCurrentCeraShopPurchaseCeraPricedCommodityLeavesGoldUntouched 是既有点券链路
// 的回归钉子：扣点券、不碰金币、**不多发任何包**。
func TestCurrentCeraShopPurchaseCeraPricedCommodityLeavesGoldUntouched(t *testing.T) {
	catalog := mustCurrentCeraShopGoldTestCatalog(t)
	const startGold = int64(1_234_567)
	ctx, repositories, service, session, connection := ceraShopGoldTestFixture(t, catalog, startGold, 200)

	if err := service.handleGameCommand(
		session,
		byte(dnfenum.GameCmdCommand),
		uint16(dnfenum.CmdPacketBuyCerashopItem),
		currentCeraShopTestRequestBody(ceraShopGoldTestCeraCommodity),
	); err != nil {
		t.Fatal(err)
	}

	if got := loadCeraShopAccountCera(t, ctx, repositories, "account-1"); got != 120 {
		t.Fatalf("account cera=%d, want 120", got)
	}
	if got := loadCeraShopGoldTestCharacterGold(t, ctx, repositories); got != startGold {
		t.Fatalf("character gold=%d, want untouched %d", got, startGold)
	}
	// 包序列必须与改造前逐字节一致：ACK → 物品更新 → 点券钱包 → 多买多送次数。
	packets := ceraShopTestPackets(t, connection.write.Bytes())
	for _, packet := range packets {
		if packet.Header.MsgID == uint16(dnfenum.CmdPacketWalkoutPartyMember) &&
			len(packet.Body) == len(buildCurrentGoldStateBody(0)) &&
			string(packet.Body) == string(buildCurrentGoldStateBody(startGold)) {
			t.Fatalf("cera-priced purchase emitted a gold wallet packet: %x", packet.Body)
		}
	}
	if want := buildCurrentCeraShopBalanceBody(120); ceraShopTestBodyCount(packets, currentCeraShopUpdateMsgID, want) != 1 {
		t.Fatalf("cera wallet packet missing (packets=%d)", len(packets))
	}
}

// TestCurrentCeraShopPurchaseRejectsMixedGoldAndCeraCartWithoutDebit：一辆车里同时有
// 金币商品和点券商品 —— **整笔拒绝**，两边都不许扣，也不许发货。
func TestCurrentCeraShopPurchaseRejectsMixedGoldAndCeraCartWithoutDebit(t *testing.T) {
	catalog := mustCurrentCeraShopGoldTestCatalog(t)
	const startGold = int64(2_000_000)
	ctx, repositories, service, session, connection := ceraShopGoldTestFixture(t, catalog, startGold, 200)

	if err := service.handleGameCommand(
		session,
		byte(dnfenum.GameCmdCommand),
		uint16(dnfenum.CmdPacketBuyCerashopItem),
		currentCeraShopTestMultiRequestBody(
			currentCeraShopPaymentModeCera,
			currentCeraShopTestCartRow{CommodityID: ceraShopGoldTestCommodity, AttributeValue: 0xff},
			currentCeraShopTestCartRow{CommodityID: ceraShopGoldTestCeraCommodity, AttributeValue: 0xff},
		),
	); err != nil {
		t.Fatal(err)
	}

	ack, rest := splitGameServerUpperPacket(t, connection.write.Bytes())
	if want := buildCurrentCeraShopPurchaseFailureBodyWithCode(currentCeraShopPurchaseFailureCodeCannotBuy); string(ack.Body) != string(want) {
		t.Fatalf("mixed cart failure body=%x want=%x", ack.Body, want)
	}
	if len(rest) != 0 {
		t.Fatalf("rejected mixed cart still emitted %d trailing bytes", len(rest))
	}
	if got := loadCeraShopGoldTestCharacterGold(t, ctx, repositories); got != startGold {
		t.Fatalf("character gold=%d, want untouched %d", got, startGold)
	}
	if got := loadCeraShopAccountCera(t, ctx, repositories, "account-1"); got != 200 {
		t.Fatalf("account cera=%d, want untouched 200", got)
	}
	inventory, found, err := repositories.Inventory.Load(ctx, "19")
	if err != nil || !found {
		t.Fatalf("load inventory found=%t err=%v", found, err)
	}
	if len(inventory.Slots) != 0 {
		t.Fatalf("rejected mixed cart granted items: %+v", inventory.Slots)
	}
}

// TestCurrentCeraShopPurchaseKeepsFullyZeroPricedRowFree：三个价格列全 0 的行
// （现场 `[coin] 102532 ... 0 0 0`）必须保持免费 —— 金币列进场不能把它变成"扣 0 金币
// 却因为缺余额被拒"。
func TestCurrentCeraShopPurchaseKeepsFullyZeroPricedRowFree(t *testing.T) {
	catalog := mustCurrentCeraShopGoldTestCatalog(t)
	const startGold = int64(0)
	ctx, repositories, service, session, connection := ceraShopGoldTestFixture(t, catalog, startGold, 200)

	if err := service.handleGameCommand(
		session,
		byte(dnfenum.GameCmdCommand),
		uint16(dnfenum.CmdPacketBuyCerashopItem),
		currentCeraShopTestRequestBody(ceraShopGoldTestCommodityFree),
	); err != nil {
		t.Fatal(err)
	}

	ack, rest := splitGameServerUpperPacket(t, connection.write.Bytes())
	if want := buildCurrentCeraShopPurchaseSuccessBodyWithCount(ceraShopGoldTestCommodityFree, 1); string(ack.Body) != string(want) {
		t.Fatalf("free purchase ACK body=%x want=%x", ack.Body, want)
	}
	if got := loadCeraShopGoldTestCharacterGold(t, ctx, repositories); got != 0 {
		t.Fatalf("character gold=%d, want untouched 0", got)
	}
	if got := loadCeraShopAccountCera(t, ctx, repositories, "account-1"); got != 200 {
		t.Fatalf("account cera=%d, want untouched 200", got)
	}
	packets := ceraShopTestPackets(t, rest)
	if count := ceraShopTestBodyCount(packets, uint16(dnfenum.CmdPacketWalkoutPartyMember), buildCurrentGoldStateBody(0)); count != 0 {
		t.Fatalf("free row emitted a gold wallet packet (count=%d)", count)
	}
}

// TestChargeCurrentCeraShopGoldIsANoOpBelowOneCost 直接把"点券商品不走这条路"钉在
// 函数契约上：cost <= 0 一个字节都不改。
func TestChargeCurrentCeraShopGoldIsANoOpBelowOneCost(t *testing.T) {
	character := &dnfrepo.CharacterRecord{Stats: map[string]int64{"gold": 42}}
	for _, cost := range []int64{0, -1, -500_000} {
		after, err := chargeCurrentCeraShopGold(character, cost)
		if err != nil || after != 0 {
			t.Fatalf("cost=%d after=%d err=%v, want no-op", cost, after, err)
		}
		if character.Stats["gold"] != 42 {
			t.Fatalf("cost=%d mutated gold to %d", cost, character.Stats["gold"])
		}
	}
	after, err := chargeCurrentCeraShopGold(character, 40)
	if err != nil || after != 2 || character.Stats["gold"] != 2 {
		t.Fatalf("after=%d gold=%d err=%v, want 2", after, character.Stats["gold"], err)
	}
	if _, err := chargeCurrentCeraShopGold(character, 3); err == nil {
		t.Fatal("insufficient gold must fail")
	}
	if character.Stats["gold"] != 2 {
		t.Fatalf("failed charge mutated gold to %d", character.Stats["gold"])
	}
}

// TestRecordCurrentCeraShopPurchaseLogsGoldColumns：没有这两列就无法从日志回答
// "这单到底扣没扣金币"—— 2026-10-01 的现场正是卡在这里。
func TestRecordCurrentCeraShopPurchaseLogsGoldColumns(t *testing.T) {
	recorder, _ := newTestRecorder(t)
	service := &Service{}
	service.records.Store(recorder)
	session := &gameSession{accountID: "dnf:5"}
	session.selectedCharacterID = 22

	service.recordCurrentCeraShopPurchase(
		session,
		currentCeraShopPurchaseRequest{
			PaymentMode: currentCeraShopPaymentModeCera,
			Items:       []currentCeraShopPurchaseRequestItem{{CommodityID: 104824, AttributeValue: 255}},
		},
		currentCeraShopCommitResult{
			Products:  []currentCeraShopProduct{{CommodityID: 104824, ItemID: 490702318, Count: 1, GoldPrice: 500000, Section: "item"}},
			CeraAfter: 908272,
			PaidGold:  500000,
			GoldAfter: 94_531_770,
		},
		true, "", 19,
	)

	records, _, err := recorder.queryRecords(recordQuery{Kind: recordKindCera})
	if err != nil || len(records) != 1 {
		t.Fatalf("records=%d err=%v", len(records), err)
	}
	record := records[0]
	if record["gold_spent"] != float64(500000) || record["gold_after"] != float64(94_531_770) || record["gold_before"] != float64(95_031_770) {
		t.Fatalf("gold columns = %+v", record)
	}
	// 点券三列必须保持"没扣"：金币不许混进 cera_spent，否则点券审计线失真。
	if record["cera_spent"] != float64(0) || record["cera_after"] != float64(908272) {
		t.Fatalf("cera columns = %+v", record)
	}
}

// TestCeraShopGoldPricedPurchaseDoesNotTouchCeraAuditLine：点券商品不写金币列
// （既有 177 行商品的记录字段集不被污染）。
func TestCeraShopGoldPricedPurchaseDoesNotTouchCeraAuditLine(t *testing.T) {
	recorder, _ := newTestRecorder(t)
	service := &Service{}
	service.records.Store(recorder)
	session := &gameSession{accountID: "dnf:5"}
	session.selectedCharacterID = 22

	service.recordCurrentCeraShopPurchase(
		session,
		currentCeraShopPurchaseRequest{
			PaymentMode: currentCeraShopPaymentModeCera,
			Items:       []currentCeraShopPurchaseRequestItem{{CommodityID: 100050, AttributeValue: 255}},
		},
		currentCeraShopCommitResult{
			Products:  []currentCeraShopProduct{{CommodityID: 100050, ItemID: 37, Count: 3, CeraPrice: 80, Section: "item"}},
			CeraAfter: 120,
			PaidCera:  80,
		},
		true, "", 19,
	)

	records, _, err := recorder.queryRecords(recordQuery{Kind: recordKindCera})
	if err != nil || len(records) != 1 {
		t.Fatalf("records=%d err=%v", len(records), err)
	}
	for _, key := range []string{"gold_spent", "gold_after", "gold_before", "gold_total_price"} {
		if _, present := records[0][key]; present {
			t.Fatalf("cera-priced record gained %q: %+v", key, records[0])
		}
	}
	if records[0]["cera_spent"] != float64(80) {
		t.Fatalf("cera_spent = %+v", records[0])
	}
}
