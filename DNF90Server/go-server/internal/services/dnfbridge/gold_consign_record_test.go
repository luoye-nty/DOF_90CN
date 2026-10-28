package dnfbridge

// 金币寄售「金币寄售记录」的回归测试（2026-09-30 新增）。
//
// 这个类别之前不存在：一次寄售从挂单到成交收款的全过程只散落在 packet_log
// 与 escrow 行里，运营既看不到"谁卖给了谁"，也回答不了"为什么他挂不上"。
// 这里锁住三件最容易写坏的事：
//
//  1. **事件发生了就有一条记录**：上架 / 买家买入 / 成交收款各一条，字段齐全；
//  2. **重跑不重复记**：发货扫描（deliverCurrentGoldConsignSalesLocked）在每条
//     寄售命令开头都会跑，必须只在 MarkDelivered 真的盖了章的那一次写记录；
//  3. **account_id / character_id 落盘是字符串**：运维工具只认字符串型，
//     数字型会被静默忽略、账号名翻译随之失效。

import (
	"context"
	"encoding/binary"
	"testing"

	dnfenum "longheng.io/server/internal/modules/dnf/dnfenum"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

// goldConsignRecordFieldsOf 把某个事件的记录取回来（同时兼任"只允许一条"的断言）。
func goldConsignRecordFieldsOf(t *testing.T, entries []map[string]any, event string) map[string]any {
	t.Helper()
	var found map[string]any
	count := 0
	for _, entry := range entries {
		if entry["event"] != event {
			continue
		}
		count++
		found = entry
	}
	if count != 1 {
		t.Fatalf("事件 %s 的记录条数 = %d, want 1（记录侧不去重，多写一条就是实打实的多一行）: %+v",
			event, count, entries)
	}
	return found
}

// goldConsignRecordString 取一个必须是字符串的字段。
func goldConsignRecordString(t *testing.T, entry map[string]any, key string) string {
	t.Helper()
	value, ok := entry[key].(string)
	if !ok {
		t.Fatalf("字段 %s 落盘类型 = %T (%v), want string（数字型会被运维工具静默忽略）",
			key, entry[key], entry[key])
	}
	return value
}

// TestGoldConsignRecordsListBuyAndSaleDelivered 走一遍完整的买卖主路径，
// 要求三个事件各留下恰好一条记录，且关键字段齐全。
func TestGoldConsignRecordsListBuyAndSaleDelivered(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	seedGoldConsignProtocolCharacter(t, ctx, repositories, "11", "account-seller", "Seller", 40, 5_000_000, nil)
	seedGoldConsignProtocolCharacter(t, ctx, repositories, "22", "account-buyer", "Buyer", 20, 0,
		map[string]string{"account_cera": "5000"})

	recorder, dir := newTestRecorder(t)
	service := goldConsignTestService(repositories)
	service.records.Store(recorder)

	listingID := buyGoldConsignAsTestBuyer(t, service)
	recorder.flush()

	entries := readRecordFile(t, dir, recordKindGoldConsign)
	if len(entries) != 3 {
		t.Fatalf("金币寄售记录条数 = %d, want 3（上架/买入/成交收款各一条）: %+v", len(entries), entries)
	}
	list := goldConsignRecordFieldsOf(t, entries, "list")
	buy := goldConsignRecordFieldsOf(t, entries, "buy")
	sale := goldConsignRecordFieldsOf(t, entries, "sale_delivered")

	// 上架：单据、档位、价格、手续费、卖家身份都要在。
	if list["event_name"] != "上架" || list["result"] != "ok" {
		t.Fatalf("上架记录 = %+v", list)
	}
	if list["listing_id"] != float64(listingID) {
		t.Fatalf("上架 listing_id = %v, want %d", list["listing_id"], listingID)
	}
	if list["gold_item_id"] != float64(2681725) || list["gold_amount"] != float64(1_000_000) {
		t.Fatalf("上架档位字段 = %+v", list)
	}
	if list["price_cera"] != float64(111) || list["fee_percent"] != float64(5) ||
		list["fee_cera"] != float64(6) || list["proceeds_cera"] != float64(105) {
		t.Fatalf("上架金额字段（实收 = 标价 - 手续费） = %+v", list)
	}
	if list["seller_id"] != "11" || list["seller_name"] != "Seller" {
		t.Fatalf("上架卖家字段 = %+v", list)
	}
	if goldConsignRecordString(t, list, "account_id") != "account-seller" ||
		goldConsignRecordString(t, list, "character_id") != "11" {
		t.Fatalf("上架身份字段 = %+v", list)
	}

	// 买入：钱的那一侧（点券前后余额）与买卖双方身份。
	if buy["event_name"] != "买家买入" || buy["result"] != "ok" {
		t.Fatalf("买入记录 = %+v", buy)
	}
	if buy["listing_id"] != float64(listingID) || buy["cera_spent"] != float64(111) ||
		buy["cera_before"] != float64(5000) || buy["cera_after"] != float64(4889) {
		t.Fatalf("买入金额字段 = %+v", buy)
	}
	if buy["buyer_id"] != "22" || buy["buyer_name"] != "Buyer" ||
		buy["seller_id"] != "11" || buy["seller_name"] != "Seller" {
		t.Fatalf("买入双方身份字段 = %+v", buy)
	}
	if _, present := buy["token_cera_after"]; !present {
		t.Fatalf("买入记录缺少买家代币券余额（钱包刷新顺带回传的值）: %+v", buy)
	}
	if goldConsignRecordString(t, buy, "account_id") != "account-buyer" ||
		goldConsignRecordString(t, buy, "character_id") != "22" {
		t.Fatalf("买入身份字段 = %+v", buy)
	}

	// 成交收款：卖家真正拿到钱的这一刻（两封信都发出、行已盖章）。
	if sale["event_name"] != "成交收款" || sale["result"] != "ok" || sale["delivered"] != true {
		t.Fatalf("成交收款记录 = %+v", sale)
	}
	if sale["listing_id"] != float64(listingID) || sale["buyer_id"] != "22" ||
		sale["proceeds_cera"] != float64(105) || sale["fee_cera"] != float64(6) {
		t.Fatalf("成交收款金额/买家字段 = %+v", sale)
	}
	if sale["seller_id"] != "11" || sale["seller_name"] != "Seller" {
		t.Fatalf("成交收款卖家字段 = %+v", sale)
	}
}

// TestGoldConsignSaleDeliveredRecordedOnlyOnce 是本任务的核心风险点：
// 发货扫描在每条寄售命令开头都会重跑，重复的那几次**一条记录都不许写**。
//
// 判据不是"这一行是从 UndeliveredSales 里取出来的，所以它一定没送过"，
// 而是域层 MarkDelivered 回传的 freshlyDelivered —— 只有它知道这次是不是
// 真的发生了跃迁。
func TestGoldConsignSaleDeliveredRecordedOnlyOnce(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	seedGoldConsignProtocolCharacter(t, ctx, repositories, "11", "account-seller", "Seller", 40, 5_000_000, nil)
	seedGoldConsignProtocolCharacter(t, ctx, repositories, "22", "account-buyer", "Buyer", 20, 0,
		map[string]string{"account_cera": "5000"})

	recorder, dir := newTestRecorder(t)
	service := goldConsignTestService(repositories)
	service.records.Store(recorder)

	buyGoldConsignAsTestBuyer(t, service)

	// 直接重跑发货扫描三次（等价于期间又来了三条寄售命令）。
	buyer := &gameSession{conn: &bufferConn{}, selectedCharacterID: 22, accountID: "account-buyer"}
	for round := 0; round < 3; round++ {
		service.deliverCurrentGoldConsignSalesLocked(ctx, buyer)
	}
	// 再真的走一条寄售命令（卖家上架第二单）：命令内部的发货扫描同样不该多写。
	seller := &gameSession{conn: &bufferConn{}, selectedCharacterID: 11, accountID: "account-seller"}
	if err := service.registerCurrentGoldConsignLocked(seller,
		uint16(dnfenum.CmdPacketAuctionRegistItem), goldConsignLiveRegisterBody()); err != nil {
		t.Fatal(err)
	}
	recorder.flush()

	entries := readRecordFile(t, dir, recordKindGoldConsign)
	// 第二单上架本身会留下第二条 list 记录，这是"命令确实执行过"的证据。
	goldConsignRecordFieldsOf(t, entries, "sale_delivered")
	goldConsignRecordFieldsOf(t, entries, "buy")
	listCount := 0
	for _, entry := range entries {
		if entry["event"] == "list" {
			listCount++
		}
	}
	if listCount != 2 {
		t.Fatalf("上架记录条数 = %d, want 2（两单各一条）: %+v", listCount, entries)
	}
	if len(entries) != 4 {
		t.Fatalf("记录总条数 = %d, want 4（2 上架 + 1 买入 + 1 成交收款）: %+v", len(entries), entries)
	}
}

// TestGoldConsignBuyRejectionIsRecorded 覆盖"被拒绝的尝试也要留痕"：
// 点券不足是运营最常被问的那一类失败。
func TestGoldConsignBuyRejectionIsRecorded(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	seedGoldConsignProtocolCharacter(t, ctx, repositories, "11", "account-seller", "Seller", 40, 5_000_000, nil)
	seedGoldConsignProtocolCharacter(t, ctx, repositories, "22", "account-buyer", "Buyer", 20, 0,
		map[string]string{"account_cera": "50"})

	recorder, dir := newTestRecorder(t)
	service := goldConsignTestService(repositories)
	service.records.Store(recorder)

	// 卖家挂单（1百万 @ 111 点券）。
	sellerConn := &bufferConn{}
	seller := &gameSession{conn: sellerConn, selectedCharacterID: 11, accountID: "account-seller"}
	if err := service.registerCurrentGoldConsignLocked(seller,
		uint16(dnfenum.CmdPacketAuctionRegistItem), goldConsignLiveRegisterBody()); err != nil {
		t.Fatal(err)
	}
	listingID := service.nextAuctionListingID.Load()

	// 买家只有 50 点券，买不起。
	buyerConn := &bufferConn{}
	buyer := &gameSession{conn: buyerConn, selectedCharacterID: 22, accountID: "account-buyer"}
	buy := make([]byte, currentAuctionGoldBuyRequestSize)
	buy[0] = currentAuctionPayTypeConsignment
	binary.LittleEndian.PutUint32(buy[1:5], 111)
	binary.LittleEndian.PutUint64(buy[5:13], listingID)
	if err := service.buyCurrentGoldConsignLocked(buyer, uint16(dnfenum.CmdPacketAuctionBidding), buy); err != nil {
		t.Fatal(err)
	}
	recorder.flush()

	entries := readRecordFile(t, dir, recordKindGoldConsign)
	rejected := goldConsignRecordFieldsOf(t, entries, "buy")
	if rejected["result"] != "rejected" || rejected["reason"] != "gold_unavailable" {
		t.Fatalf("被拒买入记录 = %+v, want result=rejected reason=gold_unavailable", rejected)
	}
	if rejected["listing_id"] != float64(listingID) || rejected["price_cera"] != float64(111) {
		t.Fatalf("被拒买入缺少单据信息 = %+v", rejected)
	}
	if goldConsignRecordString(t, rejected, "account_id") != "account-buyer" {
		t.Fatalf("被拒买入身份字段 = %+v", rejected)
	}
	// 没有成交，就不该有任何成交收款记录。
	for _, entry := range entries {
		if entry["event"] == "sale_delivered" {
			t.Fatalf("被拒的买入写下了成交收款记录: %+v", entry)
		}
	}
}
