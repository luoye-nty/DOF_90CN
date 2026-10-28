package dnfbridge

// 金币寄售「点券消费记录」的回归测试（2026-09-26 新增）。
//
// 背景：这条链路此前**完全没有埋点** —— 买家用点券买寄售金币，钱从钱包扣掉了，
// 但「记录查询 → 点券消费记录」里查不到（商城购买查得到）。运营对不上账，
// 只能靠玩家自己的余额截图。修复后才补上这个类别，这里锁住两件最容易写坏的事：
//
//  1. **必须真的记一条**，且字段口径与商城购买对齐（result / source /
//     payment_* / cera_spent / cera_before / cera_after / cera_spent_note），
//     这样两类消费能放在同一张表里看；
//  2. **重放同一个 op 绝不能记第二条**。payCurrentGoldConsignCera 里
//     "已付过（deferred settle 之后重放）"与"本次刚扣完"两个分支返回的 err
//     都是 nil，只靠 err 判断必然重复记账 —— 而记录侧
//     （activity_record.record / 管理工具）**都不做去重**，重复就是实打实多一行。
//     这正是给它加 charged 返回值的唯一理由。

import (
	"context"
	"encoding/binary"
	"strings"
	"testing"

	dnfauction "longheng.io/server/internal/modules/dnf/auction"
	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

// buyGoldConsignAsTestBuyer 走一遍完整的"买家买寄售金币"主路径（op185）。
// 卖家先把金币挂上去，买家再用点券买走。
func buyGoldConsignAsTestBuyer(t *testing.T, service *Service) (listingID uint64) {
	t.Helper()
	sellerConn := &bufferConn{}
	seller := &gameSession{conn: sellerConn, selectedCharacterID: 11, accountID: "account-seller"}
	if err := service.registerCurrentAuctionItemLocked(seller, goldConsignLiveRegisterBody()); err != nil {
		t.Fatalf("register: %v", err)
	}
	listingID = service.nextAuctionListingID.Load()

	buyerConn := &bufferConn{}
	buyer := &gameSession{conn: buyerConn, selectedCharacterID: 22, accountID: "account-buyer"}
	buy := make([]byte, currentAuctionGoldBuyRequestSize)
	buy[0] = currentAuctionPayTypeConsignment
	binary.LittleEndian.PutUint32(buy[1:5], 111)
	binary.LittleEndian.PutUint64(buy[5:13], listingID)
	if err := service.buyCurrentAuctionItemLocked(buyer, uint16(dnfenum.CmdPacketAuctionBidding), buy, false); err != nil {
		t.Fatalf("buy: %v", err)
	}
	return listingID
}

// TestGoldConsignBuyRecordsCeraSpend 一次成功的买入必须留下**恰好一条**点券消费记录，
// 且关键字段齐全（少了任何一项，运营都没法从记录回答"谁、花了多少、买的是哪一档金币"）。
func TestGoldConsignBuyRecordsCeraSpend(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	seedGoldConsignProtocolCharacter(t, ctx, repositories, "11", "account-seller", "Seller", 40, 5_000_000, nil)
	seedGoldConsignProtocolCharacter(t, ctx, repositories, "22", "account-buyer", "Buyer", 20, 0,
		map[string]string{"account_cera": "5000"})

	recorder, _ := newTestRecorder(t)
	service := goldConsignTestService(repositories)
	service.records.Store(recorder)

	listingID := buyGoldConsignAsTestBuyer(t, service)

	records, _, err := recorder.queryRecords(recordQuery{Kind: recordKindCera})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("点券消费记录条数 = %d, want 1（金币寄售买入必须记且只记一条）: %+v", len(records), records)
	}
	entry := records[0]

	if entry["source"] != "gold_consign" {
		t.Fatalf("source = %v, want gold_consign（这是把它与商城/aligned 区分开的唯一标记）", entry["source"])
	}
	if entry["operation"] != "buy_gold_consign" {
		t.Fatalf("operation = %v, want buy_gold_consign", entry["operation"])
	}
	if entry["result"] != "ok" {
		t.Fatalf("result = %v, want ok", entry["result"])
	}
	// 金额三分量：实扣 = 标价（寄售没有券抵扣）；before/after 必须能对上。
	if entry["cera_spent"] != float64(111) {
		t.Fatalf("cera_spent = %v, want 111", entry["cera_spent"])
	}
	if entry["cera_before"] != float64(5000) || entry["cera_after"] != float64(4889) {
		t.Fatalf("点券前后余额 = %v → %v, want 5000 → 4889（运营最常问的就是「之后他还剩多少」）",
			entry["cera_before"], entry["cera_after"])
	}
	// 概要列直接渲染 cera_spent_note，必须是人能看懂的中文（不是空串、不是原始数字）。
	note, _ := entry["cera_spent_note"].(string)
	if !strings.Contains(note, "金币寄售") || !strings.Contains(note, "100万") {
		t.Fatalf("cera_spent_note = %q, want 含「金币寄售」与「100万」", note)
	}
	// 身份字段必须有人：买家走的是 accountIDForSession / selectedCharacterIDString，
	// 与 session 上的字段不一定同源，所以记录侧显式补了一次。
	if entry["account_id"] != "account-buyer" || entry["character_id"] != "22" {
		t.Fatalf("买家身份字段 = account_id:%v character_id:%v, want account-buyer / 22",
			entry["account_id"], entry["character_id"])
	}
	// 寄售单与卖家侧字段：用来回溯"这单是谁卖给他的"。
	if entry["listing_id"] != float64(listingID) {
		t.Fatalf("listing_id = %v, want %d", entry["listing_id"], listingID)
	}
	if entry["gold_amount"] != float64(1_000_000) || entry["gold_item_id"] != float64(2681725) {
		t.Fatalf("金币档位字段 = gold_amount:%v gold_item_id:%v, want 1000000 / 2681725",
			entry["gold_amount"], entry["gold_item_id"])
	}
	if entry["seller_id"] != "11" || entry["seller_name"] != "Seller" {
		t.Fatalf("卖家字段 = %v / %v, want 11 / Seller", entry["seller_id"], entry["seller_name"])
	}
	if entry["proceeds_cera"] != float64(105) {
		t.Fatalf("proceeds_cera = %v, want 105（扣掉 5%% 手续费后卖家实得）", entry["proceeds_cera"])
	}
}

// TestGoldConsignPayReportsChargedOnlyOnTheFirstCall 是"重复记账"那条防线的根：
// 同一个寄售单付两次，只有第一次 charged=true。
//
// 为什么必须是这样：deferred settle 之后重放同一个 op185 时，钱包不会再被扣一次，
// 但函数返回的 err 依然是 nil（原来的实现两者完全同形）。如果调用方只看 err，
// 就会为同一次消费写第二条记录。
func TestGoldConsignPayReportsChargedOnlyOnTheFirstCall(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	seedGoldConsignProtocolCharacter(t, ctx, repositories, "11", "account-seller", "Seller", 40, 5_000_000, nil)
	seedGoldConsignProtocolCharacter(t, ctx, repositories, "22", "account-buyer", "Buyer", 20, 0,
		map[string]string{"account_cera": "5000"})

	service := goldConsignTestService(repositories)
	listing := dnfauction.GoldConsignListing{
		ID:         900,
		SellerID:   "11",
		SellerName: "Seller",
		GoldItemID: 2681725,
		GoldAmount: 1_000_000,
		PriceCera:  111,
	}

	before, after, charged, err := service.payCurrentGoldConsignCera(ctx, "account-buyer", "22", listing)
	if err != nil {
		t.Fatalf("首次扣费失败: %v", err)
	}
	if !charged {
		t.Fatal("首次扣费 charged 必须为 true（否则记录会被漏掉）")
	}
	if before != 5000 || after != 4889 {
		t.Fatalf("首次扣费余额 = %d → %d, want 5000 → 4889", before, after)
	}

	// 重放同一个寄售单：钱包不应再动，charged 必须是 false。
	before2, after2, charged2, err := service.payCurrentGoldConsignCera(ctx, "account-buyer", "22", listing)
	if err != nil {
		t.Fatalf("重放不应报错（原实现就是这么设计的）：%v", err)
	}
	if charged2 {
		t.Fatal("重放 charged 必须为 false —— 为 true 就会写出第二条点券消费记录（记录侧不去重）")
	}
	if before2 != 0 || after2 != 0 {
		t.Fatalf("重放不应报告余额变化，实际 %d → %d", before2, after2)
	}

	account, found, err := repositories.Account.Load(ctx, "account-buyer")
	if err != nil || !found {
		t.Fatal(err)
	}
	if got := account.Metadata["account_cera"]; got != "4889" {
		t.Fatalf("重放之后买家点券 = %s, want 4889（只扣一次）", got)
	}
}

// TestGoldConsignPayNotChargedWhenRefused 付不起时 charged 必须是 false：
// 记录不能出现"扣了 0 点券"的误导行（与商城把被拒交易记成 rejected 的口径一致，
// 但寄售这里干脆不记 —— 没有扣钱就不是消费）。
func TestGoldConsignPayNotChargedWhenRefused(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	seedGoldConsignProtocolCharacter(t, ctx, repositories, "22", "account-buyer", "Buyer", 20, 0,
		map[string]string{"account_cera": "50"})

	service := goldConsignTestService(repositories)
	listing := dnfauction.GoldConsignListing{ID: 901, SellerID: "11", GoldItemID: 2681725, GoldAmount: 1_000_000, PriceCera: 111}

	_, _, charged, err := service.payCurrentGoldConsignCera(ctx, "account-buyer", "22", listing)
	if err == nil {
		t.Fatal("点券不足时应当报错")
	}
	if charged {
		t.Fatal("被拒绝的扣费 charged 必须为 false（否则记录里会出现一条没扣钱的「消费」）")
	}
}

// TestGoldConsignAmountText 概要列的金币数量渲染：档位最大到 2 亿，
// 原始数字一眼分不出量级。
func TestGoldConsignAmountText(t *testing.T) {
	cases := []struct {
		amount int64
		want   string
	}{
		{0, "0"},
		{-1, "0"},
		{1_000_000, "100万"},
		{3_000_000, "300万"},
		{30_000_000, "3000万"},
		{100_000_000, "1亿"},
		{200_000_000, "2亿"},
		{123, "123"}, // 非整万原样显示，别造一个假的量级
	}
	for _, testCase := range cases {
		if got := goldConsignAmountText(testCase.amount); got != testCase.want {
			t.Fatalf("goldConsignAmountText(%d) = %q, want %q", testCase.amount, got, testCase.want)
		}
	}
	// 每个真实档位都要渲染成人读形式，不能留原始数字。
	for _, tier := range dnfauction.GoldConsignTiers() {
		got := goldConsignAmountText(tier.Amount)
		if got == "" || strings.HasSuffix(got, "0000") {
			t.Fatalf("档位 %d 渲染为 %q，不像人读形式", tier.Amount, got)
		}
	}
}
