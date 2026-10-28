package dnfbridge

import (
	"context"
	"encoding/binary"
	"testing"

	dnfcerashop "longheng.io/server/internal/modules/dnf/cerashop"
	dnfproto "longheng.io/server/internal/modules/dnf/protocol"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

// GM 补发路径上的代币券 / 欢乐代币券处理。
//
// 券在 PVF 里是普通 `[material]`、没有 `[action type]`，客户端无法自行兑换；
// 所以凡是发放路径都必须由服务端直接入账号钱包，否则玩家拿到的是用不掉的"材料"。
//
// （任务奖励路径的纯函数测试在 quest 包内：那两个 helper 是未导出的。）
func TestGMRepairMovesBothVoucherKindsIntoAccountWallet(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Account.Save(ctx, dnfrepo.AccountRecord{
		AccountID: "dnf:test",
		Metadata: map[string]string{
			"account_cera":             "700",
			"account_token_cera":       "100",
			"account_happy_token_cera": "1",
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "88", AccountID: "dnf:test", Name: "Receiver", Level: 90,
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "88",
		Slots: map[string]dnfrepo.ItemStack{
			"0:121": {ItemID: 2681917, Count: 99999}, // 欢乐代币券
			"0:123": {ItemID: 2681762, Count: 42},    // 代币券
			"0:122": {ItemID: 3227, Count: 5},        // 无关道具，必须留下
		},
	}); err != nil {
		t.Fatal(err)
	}
	connection := &bufferConn{}
	service := &Service{
		options:            options{accountID: "dnf:test", gameUpperHeader: gameUpperHeaderServer16, gameUpperBodyCodec: gameUpperBodyCodecPlain},
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
		gameSessions: map[uint16]*gameSession{88: {
			conn: connection, connID: "gm-voucher-repair", selectedCharacterID: 88,
		}},
	}

	result, err := service.GMRepairHappyTokenCeraVoucher(ctx, GMWalletRequest{CharacterID: "88"})
	if err != nil {
		t.Fatal(err)
	}
	// Granted 仍然只表示欢乐代币券（既有调用方与用例都按这个口径读）。
	if result.Granted != 99999 {
		t.Fatalf("Granted 必须仍然只表示欢乐代币券: %+v", result)
	}
	if result.TokenCeraGranted != 42 {
		t.Fatalf("TokenCeraGranted=%d want=42", result.TokenCeraGranted)
	}
	// 代币券 100+42、欢乐代币券 1+99999，点券不受影响。
	if result.Cera != 700 || result.TokenCera != 142 || result.HappyTokenCera != 100000 {
		t.Fatalf("repair result = %+v", result)
	}
	inventory, found, err := repositories.Inventory.Load(ctx, "88")
	if err != nil || !found {
		t.Fatalf("inventory found=%t err=%v", found, err)
	}
	if _, exists := inventory.Slots["0:121"]; exists {
		t.Fatalf("欢乐代币券行没有被搬走: %+v", inventory.Slots)
	}
	if _, exists := inventory.Slots["0:123"]; exists {
		t.Fatalf("代币券行没有被搬走: %+v", inventory.Slots)
	}
	if stack := inventory.Slots["0:122"]; stack.ItemID != 3227 || stack.Count != 5 {
		t.Fatalf("无关道具被动了: %+v", stack)
	}
}

// 支付瀑布的端到端证明：**点券为 0、只有代币券**也能在商城买到东西。
//
// 这正是现场抱怨的另一半 —— 代币券以前只是一组能看不能用的数字：
// 扣费只认点券，客户端又没有"用代币券支付"的选项，所以代币券永远花不出去。
func TestCurrentCeraShopPurchaseSpendsHappyThenTokenCeraBeforeCera(t *testing.T) {
	ctx := context.Background()
	catalog := mustCurrentCeraShopTestCatalog(t)
	repositories := dnfrepomemory.NewMemoryGroup()
	// 商品 100050 单价 80：欢乐代币券 50 先花掉，剩下 30 从代币券扣，点券一分不动。
	if err := repositories.Account.Save(ctx, dnfrepo.AccountRecord{
		AccountID: "account-1",
		Metadata: map[string]string{
			"account_cera":             "0",
			"account_token_cera":       "100",
			"account_happy_token_cera": "50",
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "19", AccountID: "account-1", Level: 90, Stats: map[string]int64{"cera": 0},
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

	result, err := service.commitCurrentCeraShopPurchase(ctx, session, catalog, currentCeraShopPurchaseRequest{
		PaymentMode: currentCeraShopPaymentModeCera,
		Items:       []currentCeraShopPurchaseRequestItem{{CommodityID: 100050}},
	})
	if err != nil {
		t.Fatalf("只有代币券时也必须能买: %v", err)
	}
	if result.PaidHappyTokenCera != 50 || result.PaidTokenCera != 30 || result.PaidCera != 0 {
		t.Fatalf("扣费拆分 happy=%d token=%d cera=%d want 50/30/0",
			result.PaidHappyTokenCera, result.PaidTokenCera, result.PaidCera)
	}
	if result.CeraAfter != 0 || result.TokenCera != 70 || result.HappyTokenCera != 0 {
		t.Fatalf("结账后钱包 cera=%d token=%d happy=%d want 0/70/0",
			result.CeraAfter, result.TokenCera, result.HappyTokenCera)
	}
	// 钱包真的落库了（不是只改了内存）。
	account, _, err := repositories.Account.Load(ctx, "account-1")
	if err != nil {
		t.Fatal(err)
	}
	if wallet := dnfcerashop.WalletBalance(account); wallet.Cera != 0 || wallet.TokenCera != 70 || wallet.HappyTokenCera != 0 {
		t.Fatalf("落库钱包=%+v want 0/70/0", wallet)
	}
	// 商品照常进背包。
	inventory, _, err := repositories.Inventory.Load(ctx, "19")
	if err != nil {
		t.Fatal(err)
	}
	granted := int64(0)
	for _, stack := range inventory.Slots {
		if stack.ItemID == 37 {
			granted += int64(stack.Count)
		}
	}
	if granted != 3 {
		t.Fatalf("商品应发 3 个道具 37，实际=%d", granted)
	}
}

// 三种钱包加起来都不够时，整笔失败、任何钱包都不动。
func TestCurrentCeraShopPurchaseRefusesWhenWholeWalletIsShort(t *testing.T) {
	ctx := context.Background()
	catalog := mustCurrentCeraShopTestCatalog(t)
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Account.Save(ctx, dnfrepo.AccountRecord{
		AccountID: "account-1",
		Metadata: map[string]string{
			"account_cera":             "10",
			"account_token_cera":       "20",
			"account_happy_token_cera": "30",
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "19", AccountID: "account-1", Level: 90, Stats: map[string]int64{"cera": 10},
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
	// 100050 单价 80 > 10+20+30 = 60。
	if _, err := service.commitCurrentCeraShopPurchase(ctx, session, catalog, currentCeraShopPurchaseRequest{
		PaymentMode: currentCeraShopPaymentModeCera,
		Items:       []currentCeraShopPurchaseRequestItem{{CommodityID: 100050}},
	}); err == nil {
		t.Fatal("三个钱包加起来也不够时必须失败")
	}
	account, _, err := repositories.Account.Load(ctx, "account-1")
	if err != nil {
		t.Fatal(err)
	}
	if wallet := dnfcerashop.WalletBalance(account); wallet.Cera != 10 || wallet.TokenCera != 20 || wallet.HappyTokenCera != 30 {
		t.Fatalf("失败后钱包不该被改动: %+v", wallet)
	}
}

// 溢出时整笔拒绝，两种券都不搬、库存行也不删（全有或全无）。
func TestGMRepairRefusesWhenTokenCeraWouldOverflow(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Account.Save(ctx, dnfrepo.AccountRecord{
		AccountID: "dnf:test",
		Metadata:  map[string]string{"account_token_cera": "2147483647"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "88", AccountID: "dnf:test", Name: "Receiver", Level: 90,
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "88",
		Slots: map[string]dnfrepo.ItemStack{
			"0:123": {ItemID: 2681762, Count: 1},
			"0:121": {ItemID: 2681917, Count: 10},
		},
	}); err != nil {
		t.Fatal(err)
	}
	service := &Service{
		options:            options{accountID: "dnf:test"},
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
		gameSessions:       map[uint16]*gameSession{},
	}
	if _, err := service.GMRepairHappyTokenCeraVoucher(ctx, GMWalletRequest{CharacterID: "88"}); err == nil {
		t.Fatal("代币券溢出时必须整笔拒绝")
	}
	inventory, _, err := repositories.Inventory.Load(ctx, "88")
	if err != nil {
		t.Fatal(err)
	}
	if stack := inventory.Slots["0:123"]; stack.ItemID != 2681762 || stack.Count != 1 {
		t.Fatalf("拒绝后代币券行应原样保留: %+v", stack)
	}
	if stack := inventory.Slots["0:121"]; stack.ItemID != 2681917 || stack.Count != 10 {
		t.Fatalf("拒绝后欢乐代币券行应原样保留（不能只搬一种）: %+v", stack)
	}
	account, _, err := repositories.Account.Load(ctx, "dnf:test")
	if err != nil {
		t.Fatal(err)
	}
	if wallet := dnfcerashop.WalletBalance(account); wallet.HappyTokenCera != 0 || wallet.TokenCera != 2147483647 {
		t.Fatalf("拒绝后钱包不该被改动: %+v", wallet)
	}
}

// currentVoucherWalletPacketBody 从连接缓冲里找出 class0/op53 钱包包并返回 13 字节包体。
func currentVoucherWalletPacketBody(t *testing.T, connection *bufferConn) []byte {
	t.Helper()
	stream := connection.write.Bytes()
	for len(stream) > 0 {
		var packet dnfproto.ChannelPacket
		packet, stream = splitGameServerUpperPacket(t, stream)
		if packet.Header.Classification != 0 || packet.Header.MsgID != currentCeraShopWalletMsgID {
			continue
		}
		body := packet.Body
		if len(body) > 3 && body[0] == 0 && body[1] == 0 && body[2] == 0 {
			body = body[3:]
		}
		return body
	}
	return nil
}

// GM 改**代币券 / 欢乐代币券**之后必须把新钱包推给在线客户端。
//
// 早先这里判的是 `request.Currency == "cera"`，于是这两种新面额会掉到金币分支：
// 钱包已落库、客户端三个数字不变、接口返回 refreshed=false。后果是"GM 把代币券设成
// 10 万、商城还是 0"，排查方向会被带偏到支付瀑布上 —— 而这恰恰是本批扩展 GM 读写
// 的目的（造"点券为 0、只有代币券"的验收数据）。
func TestGMUpdateWalletPushesTokenCeraAndHappyTokenCeraToOnlineClient(t *testing.T) {
	ctx := context.Background()
	for _, testCase := range []struct {
		currency string
		amount   int64
		// 期望 op53 包体里的代币券 / 欢乐代币券位置
		wantTokenCera      int32
		wantHappyTokenCera int32
	}{
		{"token_cera", 1000, 1000, 0},
		{"happy_token_cera", 2000, 0, 2000},
	} {
		t.Run(testCase.currency, func(t *testing.T) {
			repositories := dnfrepomemory.NewMemoryGroup()
			if err := repositories.Account.Save(ctx, dnfrepo.AccountRecord{
				AccountID: "dnf:test",
				Metadata:  map[string]string{"account_cera": "700"},
			}); err != nil {
				t.Fatal(err)
			}
			if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
				CharacterID: "88", AccountID: "dnf:test", Name: "Receiver", Level: 90,
			}); err != nil {
				t.Fatal(err)
			}
			connection := &bufferConn{}
			service := &Service{
				options: options{
					accountID:          "dnf:test",
					gameUpperHeader:    gameUpperHeaderServer16,
					gameUpperBodyCodec: gameUpperBodyCodecPlain,
				},
				repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
				gameSessions: map[uint16]*gameSession{88: {
					conn: connection, connID: "gm-wallet-" + testCase.currency, selectedCharacterID: 88,
				}},
			}
			result, err := service.GMUpdateWallet(ctx, GMWalletRequest{
				CharacterID: "88", Currency: testCase.currency, Mode: "set", Amount: testCase.amount,
			})
			if err != nil {
				t.Fatal(err)
			}
			if !result.Refreshed {
				t.Fatalf("在线时 %s 必须刷新客户端（refresh_error=%q）", testCase.currency, result.RefreshError)
			}
			body := currentVoucherWalletPacketBody(t, connection)
			if body == nil {
				t.Fatalf("%s 改动后没有推送 class0/op53 钱包包", testCase.currency)
			}
			if len(body) != 13 {
				t.Fatalf("钱包包体长度=%d want=13", len(body))
			}
			if got := int32(binary.LittleEndian.Uint32(body[5:9])); got != testCase.wantTokenCera {
				t.Fatalf("%s: 推送的代币券=%d want=%d", testCase.currency, got, testCase.wantTokenCera)
			}
			if got := int32(binary.LittleEndian.Uint32(body[9:13])); got != testCase.wantHappyTokenCera {
				t.Fatalf("%s: 推送的欢乐代币券=%d want=%d", testCase.currency, got, testCase.wantHappyTokenCera)
			}
		})
	}
}

// GM 券补发：背包里**只有代币券**时也必须刷新在线客户端。
//
// `Granted` 只统计欢乐代币券（既有语义），所以只补代币券时它是 0。早先的刷新门只判
// `Granted == 0` → 整段跳过：行已删、钱包已入账，客户端两样都看不到，还会去点那行
// 已经不存在的幽灵券。
func TestGMRepairRefreshesWhenOnlyTokenCeraVoucherPresent(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Account.Save(ctx, dnfrepo.AccountRecord{
		AccountID: "dnf:test", Metadata: map[string]string{"account_token_cera": "10"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "88", AccountID: "dnf:test", Name: "Receiver", Level: 90,
	}); err != nil {
		t.Fatal(err)
	}
	// 只有代币券行，没有欢乐代币券行。
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "88",
		Slots:       map[string]dnfrepo.ItemStack{"0:123": {ItemID: 2681762, Count: 5}},
	}); err != nil {
		t.Fatal(err)
	}
	connection := &bufferConn{}
	service := &Service{
		options: options{
			accountID:          "dnf:test",
			gameUpperHeader:    gameUpperHeaderServer16,
			gameUpperBodyCodec: gameUpperBodyCodecPlain,
		},
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
		gameSessions: map[uint16]*gameSession{88: {
			conn: connection, connID: "gm-token-only-repair", selectedCharacterID: 88,
		}},
	}
	result, err := service.GMRepairHappyTokenCeraVoucher(ctx, GMWalletRequest{CharacterID: "88"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Granted != 0 || result.TokenCeraGranted != 5 {
		t.Fatalf("Granted=%d TokenCeraGranted=%d want 0/5（Granted 仍只表示欢乐代币券）",
			result.Granted, result.TokenCeraGranted)
	}
	if !result.Refreshed {
		t.Fatalf("只补代币券时也必须刷新客户端（refresh_error=%q）", result.RefreshError)
	}
	body := currentVoucherWalletPacketBody(t, connection)
	if body == nil {
		t.Fatal("只补代币券时没有推送 class0/op53 钱包包")
	}
	if got := int32(binary.LittleEndian.Uint32(body[5:9])); got != 15 {
		t.Fatalf("推送的代币券=%d want=15（10+5）", got)
	}
}
