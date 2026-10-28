package dnfbridge

import (
	"context"
	"encoding/binary"
	"testing"

	dnfcerashop "longheng.io/server/internal/modules/dnf/cerashop"
	dnfenum "longheng.io/server/internal/modules/dnf/dnfenum"
	dnfproto "longheng.io/server/internal/modules/dnf/protocol"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

// 现场 2026-09-19 的抱怨：「代币券和欢乐代币券会作为材料放进背包，这很不合理」。
//
// 根因：这两件道具在道具 PVF 里就是普通 `[stackable type] [material]`，
// **没有 `[action type]`，客户端根本没有任何途径把它们变成钱包余额**。
// 所以只要它们被当普通道具发下去，玩家拿到的就是一件永远用不掉的「材料」。
//
// 礼盒（`[booster]`，PVF 里有 40 个盒子会开出欢乐代币券）是主要来源之一。
// 本组测试守住：礼盒开出的券**直接进账号钱包、不占背包格**，并且当场推给客户端。

// voucherTestSource 在共用礼盒夹具上挂一个会开出两种券的盒子（道具 512）。
func voucherTestSource() dungeonDropCatalogTestSource {
	source := currentBoosterTestSource()
	source["stackable/stackable.lst"] += "512 `cash/voucher_box.stk`\n" +
		"2681762 `cash/chn_Trade_Gold/trade_point.stk`\n" +
		"2681917 `cash/chn_Trade_Gold/trade_point_event.stk`\n"
	// 与真机 PVF 一致：两件券都是普通 [material]，且**没有** [action type]。
	// 每个奖励行单独一个 section —— 真机 booster PVF（contracts.stk 里连续的多个
	// [cera]）就是这么写的；多行塞进同一个 [etc] 只会解析出一行。
	source["stackable/cash/voucher_box.stk"] = "[stackable type]\n`[booster]` 0\n[stack limit]\n1\n" +
		"[booster info]\n[etc]\n1 2681762 10000 100\n[/etc]\n[etc]\n1 2681917 10000 100\n[/etc]\n[/booster info]\n"
	source["stackable/cash/chn_Trade_Gold/trade_point.stk"] =
		"[name]\n`代币券`\n[stackable type]\n`[material]`\n[stack limit]\n1000000000\n[attach type]\n`[free]`\n"
	source["stackable/cash/chn_Trade_Gold/trade_point_event.stk"] =
		"[name]\n`欢乐代币券`\n[stackable type]\n`[material]`\n[attach type]\n`[free]`\n"
	return source
}

// currentVoucherStreamPackets 把礼盒开启写出的整条流拆成包，供"这一包在不在"这类断言用。
func currentVoucherStreamPackets(t *testing.T, data []byte) []dnfproto.ChannelPacket {
	t.Helper()
	packets := make([]dnfproto.ChannelPacket, 0, 4)
	for len(data) > 0 {
		var packet dnfproto.ChannelPacket
		packet, data = splitGameServerUpperPacket(t, data)
		packets = append(packets, packet)
	}
	return packets
}

func TestCurrentBoosterVoucherRewardCreditsWalletInsteadOfBackpack(t *testing.T) {
	catalog, err := newPVFDungeonDropCatalog(voucherTestSource())
	if err != nil {
		t.Fatal(err)
	}
	repositories := dnfrepomemory.NewMemoryGroup()
	ctx := context.Background()
	if err := repositories.Account.Save(ctx, dnfrepo.AccountRecord{
		AccountID: "account-1",
		Metadata:  map[string]string{"account_cera": "700", "account_token_cera": "5"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{CharacterID: "19", AccountID: "account-1"}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "19",
		Slots:       map[string]dnfrepo.ItemStack{"0:76": {ItemID: 512, Count: 1}},
	}); err != nil {
		t.Fatal(err)
	}

	connection := &bufferConn{}
	service := currentBoosterTestService(catalog, repositories)
	session := &gameSession{conn: connection, connID: "booster-voucher-test", selectedCharacterID: 19}
	if err := service.handleGameCommand(
		session,
		byte(dnfenum.GameCmdCommand),
		uint16(dnfenum.CmdPacketUseBoosterItem),
		[]byte{76, 0},
	); err != nil {
		t.Fatal(err)
	}

	// 1) 券进了账号钱包：代币券 5+100、欢乐代币券 0+100，点券不受影响。
	account, found, err := repositories.Account.Load(ctx, "account-1")
	if err != nil || !found {
		t.Fatalf("load account found=%t err=%v", found, err)
	}
	wallet := dnfcerashop.WalletBalance(account)
	if wallet.Cera != 700 || wallet.TokenCera != 105 || wallet.HappyTokenCera != 100 {
		t.Fatalf("钱包=%+v want cera=700 token=105 happy=100", wallet)
	}

	// 2) 券**没有**落进背包（这正是现场的抱怨）。
	inventory, found, err := repositories.Inventory.Load(ctx, "19")
	if err != nil || !found {
		t.Fatalf("load inventory found=%t err=%v", found, err)
	}
	for key, stack := range inventory.Slots {
		for _, voucherID := range dnfcerashop.VoucherItemIDs() {
			if stack.ItemID == voucherID {
				t.Fatalf("券漏进背包 key=%s stack=%+v", key, stack)
			}
		}
	}

	// 3) 源盒子照常被消耗（不能因为改了发奖路径就白送）。
	if stack, stillThere := inventory.Slots["0:76"]; stillThere {
		t.Fatalf("源盒子没被消耗: %+v", stack)
	}

	// 4) 新钱包值当场推给客户端（不推的话玩家看不到到账）。
	//    礼盒开启本来还会发道具列表等若干包，所以扫描整条流找钱包通知，
	//    而不是假设它是第一个包。
	var walletBody []byte
	for _, packet := range currentVoucherStreamPackets(t, connection.write.Bytes()) {
		if packet.Header.Classification != 0 || packet.Header.MsgID != currentCeraShopWalletMsgID {
			continue
		}
		walletBody = packet.Body
		if len(walletBody) > 3 && walletBody[0] == 0 && walletBody[1] == 0 && walletBody[2] == 0 {
			walletBody = walletBody[3:]
		}
		break
	}
	if walletBody == nil {
		t.Fatal("礼盒开出券之后没有推送钱包，玩家看不到到账")
	}
	if len(walletBody) != 13 || int32(binary.LittleEndian.Uint32(walletBody[1:5])) != 700 ||
		int32(binary.LittleEndian.Uint32(walletBody[5:9])) != 105 ||
		int32(binary.LittleEndian.Uint32(walletBody[9:13])) != 100 {
		t.Fatalf("钱包通知体=%x want 700/105/100", walletBody)
	}
}

// 钱包已经到上限时，整笔开启必须失败（盒子不能被消耗掉白扣），
// 而不是静默截断——客户端把这三个字段按 i32 读，溢出会显示成负数。
func TestCurrentBoosterVoucherRewardRefusesWalletOverflow(t *testing.T) {
	catalog, err := newPVFDungeonDropCatalog(voucherTestSource())
	if err != nil {
		t.Fatal(err)
	}
	repositories := dnfrepomemory.NewMemoryGroup()
	ctx := context.Background()
	if err := repositories.Account.Save(ctx, dnfrepo.AccountRecord{
		AccountID: "account-1",
		Metadata:  map[string]string{"account_token_cera": "2147483647"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{CharacterID: "19", AccountID: "account-1"}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "19",
		Slots:       map[string]dnfrepo.ItemStack{"0:76": {ItemID: 512, Count: 1}},
	}); err != nil {
		t.Fatal(err)
	}
	service := currentBoosterTestService(catalog, repositories)
	connection := &bufferConn{}
	session := &gameSession{conn: connection, connID: "booster-voucher-overflow", selectedCharacterID: 19}
	// handler 会把事务错误转成"失败 ACK"发回客户端并返回 nil（它既有的契约），
	// 所以这里断言的是**效果**而不是返回值。
	if err := service.handleGameCommand(
		session,
		byte(dnfenum.GameCmdCommand),
		uint16(dnfenum.CmdPacketUseBoosterItem),
		[]byte{76, 0},
	); err != nil {
		t.Fatalf("handler 不该把事务错误往上抛: %v", err)
	}
	// 客户端绝不能收到"钱包已到账"的通知（那会让它显示一个溢出的值）。
	for _, packet := range currentVoucherStreamPackets(t, connection.write.Bytes()) {
		if packet.Header.Classification == 0 && packet.Header.MsgID == currentCeraShopWalletMsgID {
			t.Fatalf("溢出失败时不该推送钱包: %x", packet.Body)
		}
	}
	// 盒子还在（没有白扣）。
	inventory, _, err := repositories.Inventory.Load(ctx, "19")
	if err != nil {
		t.Fatal(err)
	}
	if stack, ok := inventory.Slots["0:76"]; !ok || stack.Count != 1 {
		t.Fatalf("溢出失败后盒子应原样保留: %+v ok=%t", stack, ok)
	}
	// 全有或全无：代币券溢出时，欢乐代币券也不能被写进去。
	account, _, err := repositories.Account.Load(ctx, "account-1")
	if err != nil {
		t.Fatal(err)
	}
	if wallet := dnfcerashop.WalletBalance(account); wallet.HappyTokenCera != 0 || wallet.TokenCera != 2147483647 {
		t.Fatalf("溢出失败后钱包不该被改动: %+v", wallet)
	}
}
