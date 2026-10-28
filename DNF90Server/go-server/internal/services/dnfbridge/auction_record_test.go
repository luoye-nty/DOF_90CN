package dnfbridge

// 拍卖行「拍卖行记录」的回归测试（2026-09-30 新增）。
//
// 拍卖行此前也没有自己的记录类别：一次竞拍从挂单、出价、一口价成交到结算，
// 只能从 packet_log 里拼。这个文件锁住三件事：
//
//  1. 五个事件（上架 / 竞拍出价 / 一口价成交 / 竞拍成交 / 卖家领取收益）
//     各自的字段齐全；
//  2. **重跑不重复记**：结算扫描（settleCurrentAuctionWonListingsLocked）与
//     补发扫描（retryCurrentAuctionMailsLocked）在每个拍卖命令开头都会跑，
//     重复的那几轮必须一条都不写；
//  3. account_id / character_id 落盘是字符串（运维工具只认字符串型）。

import (
	"context"
	"encoding/binary"
	"strconv"
	"strings"
	"testing"
	"time"

	dnfauction "longheng.io/server/internal/modules/dnf/auction"
	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

// auctionRecordTestService 一个能跑通物品拍卖主路径的最小服务：
// auctionItemEligibility 是免 PVF 的测试接缝（见 currentAuctionRegistrationPolicy）。
func auctionRecordTestService(repositories dnfrepo.Group) *Service {
	return &Service{
		options:                options{gameUpperHeader: gameUpperHeaderChannel13, gameUpperBodyCodec: gameUpperBodyCodecPlain},
		repositoryProvider:     func() (dnfrepo.Group, bool) { return repositories, true },
		auctionItemEligibility: func(itemID uint32) bool { return itemID == 6001 },
	}
}

// auctionRecordEvent 取某个事件的唯一一条记录（兼做"恰好一条"的断言）。
func auctionRecordEvent(t *testing.T, entries []map[string]any, event string) map[string]any {
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
		t.Fatalf("事件 %s 的记录条数 = %d, want 1: %+v", event, count, entries)
	}
	return found
}

// auctionRecordCount 统计某个事件的记录条数。
func auctionRecordCount(entries []map[string]any, event string) int {
	count := 0
	for _, entry := range entries {
		if entry["event"] == event {
			count++
		}
	}
	return count
}

// auctionBidBody 组一个 op185 竞拍/一口价请求（30 字节 mode-0 形状）。
func auctionBidBody(listingID uint64, money int64, itemID uint32) []byte {
	body := make([]byte, 30)
	body[0] = currentAuctionPayTypeGold
	binary.LittleEndian.PutUint32(body[1:5], uint32(money))
	binary.LittleEndian.PutUint64(body[5:13], listingID)
	binary.LittleEndian.PutUint32(body[26:30], itemID)
	return body
}

// auctionAgeListing 把托管里那一行挂单的到期时间改到过去（唯一的挂单）。
func auctionAgeListing(t *testing.T, ctx context.Context, repositories dnfrepo.Group) {
	t.Helper()
	escrow, found, err := repositories.Inventory.Load(ctx, dnfauction.EscrowCharacterID)
	if err != nil || !found {
		t.Fatalf("加载托管背包失败: found=%t err=%v", found, err)
	}
	patched := 0
	for key, stack := range escrow.Slots {
		if !strings.HasPrefix(key, "auction:listing:") {
			continue
		}
		stack.Extra = cloneDungeonCardStringMap(stack.Extra)
		stack.Extra["expires_at"] = strconv.FormatInt(time.Now().UTC().Add(-time.Hour).Unix(), 10)
		escrow.Slots[key] = stack
		patched++
	}
	if patched != 1 {
		t.Fatalf("待过期的挂单条数 = %d, want 1", patched)
	}
	if err := repositories.Inventory.Save(ctx, escrow); err != nil {
		t.Fatal(err)
	}
}

// TestAuctionRegisterBidBuyoutRecordsAndPayoutNotRepeated 覆盖物品拍卖的
// 上架 / 出价 / 一口价成交 / 卖家领取收益四个事件，并证明补发扫描不重复记。
func TestAuctionRegisterBidBuyoutRecordsAndPayoutNotRepeated(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	seedCurrentAuctionBridgeCharacter(t, ctx, repositories, "10", "account-seller", "Seller", 20_000, map[string]dnfrepo.ItemStack{
		"0:9": {ItemID: 6001, Count: 1, RawEntry: make([]byte, currentItemListEntryWireSize)},
	})
	seedCurrentAuctionBridgeCharacter(t, ctx, repositories, "11", "account-bidder", "Bidder", 1_000, nil)

	recorder, dir := newTestRecorder(t)
	service := auctionRecordTestService(repositories)
	service.records.Store(recorder)
	service.nextAuctionListingID.Store(100)

	seller := &gameSession{conn: &bufferConn{}, selectedCharacterID: 10, accountID: "account-seller"}
	// 起始竞拍价 50 / 一口价 75（单价，整单 1 件）。
	if err := service.registerCurrentAuctionItemLocked(seller,
		buildCurrentAuctionRegisterRequestForTest(9, 6001, 1, 50, 75, 75, 24)); err != nil {
		t.Fatal(err)
	}
	listingID := service.nextAuctionListingID.Load()

	bidder := &gameSession{conn: &bufferConn{}, selectedCharacterID: 11, accountID: "account-bidder"}
	// 60 < 一口价 75 → 走竞拍。
	if err := service.buyCurrentAuctionItemLocked(bidder, uint16(dnfenum.CmdPacketAuctionBidding),
		auctionBidBody(listingID, 60, 6001), false); err != nil {
		t.Fatal(err)
	}
	// 75 = 一口价 → 走买断。
	if err := service.buyCurrentAuctionItemLocked(bidder, uint16(dnfenum.CmdPacketAuctionBidding),
		auctionBidBody(listingID, 75, 6001), false); err != nil {
		t.Fatal(err)
	}
	recorder.flush()

	entries := readRecordFile(t, dir, recordKindAuction)
	if len(entries) != 4 {
		t.Fatalf("拍卖行记录条数 = %d, want 4（上架/出价/一口价成交/卖家领取收益）: %+v", len(entries), entries)
	}
	list := auctionRecordEvent(t, entries, "list")
	bid := auctionRecordEvent(t, entries, "bid")
	buyout := auctionRecordEvent(t, entries, "buyout")
	payout := auctionRecordEvent(t, entries, "payout")

	if list["event_name"] != "上架" || list["result"] != "ok" || list["listing_id"] != float64(listingID) {
		t.Fatalf("上架记录 = %+v", list)
	}
	if list["item_id"] != float64(6001) || list["item_count"] != float64(1) ||
		list["unit_price"] != float64(75) || list["start_price"] != float64(50) ||
		list["buyout_price"] != float64(75) {
		t.Fatalf("上架价格字段 = %+v", list)
	}
	if list["deposit_gold"] != float64(dnfauction.ListingDepositGold) ||
		list["fee_percent"] != float64(dnfauction.SaleFeePercentFor(false)) {
		t.Fatalf("上架押金/费率字段 = %+v", list)
	}
	if list["seller_id"] != "10" || list["seller_name"] != "Seller" {
		t.Fatalf("上架卖家字段 = %+v", list)
	}
	if _, err := list["account_id"].(string); !err {
		t.Fatalf("上架 account_id 落盘类型 = %T, want string", list["account_id"])
	}
	if _, err := list["character_id"].(string); !err {
		t.Fatalf("上架 character_id 落盘类型 = %T, want string", list["character_id"])
	}

	if bid["event_name"] != "竞拍出价" || bid["bid_total"] != float64(60) ||
		bid["bid_unit_price"] != float64(60) || bid["gold_after"] != float64(940) {
		t.Fatalf("出价记录 = %+v", bid)
	}
	if bid["bidder_id"] != "11" || bid["seller_id"] != "10" || bid["item_id"] != float64(6001) {
		t.Fatalf("出价身份/物品字段 = %+v", bid)
	}
	// 这一单没有别的竞拍者被超越，就不该出现一个空的 outbid_id。
	if _, present := bid["outbid_id"]; present {
		t.Fatalf("没有对手被超越却写了 outbid_id: %+v", bid)
	}

	if buyout["event_name"] != "一口价成交" || buyout["buyout"] != true || buyout["whole_listing"] != true {
		t.Fatalf("一口价成交记录 = %+v", buyout)
	}
	if buyout["sale_price"] != float64(75) || buyout["seller_proceeds"] != float64(72+dnfauction.ListingDepositGold) ||
		buyout["fee_gold"] != float64(3) {
		t.Fatalf("一口价成交金额字段 = %+v", buyout)
	}
	// 买家自己站着的 60 金币被抵扣，实扣 15。
	if buyout["gold_after"] != float64(925) {
		t.Fatalf("买断后买家金币 = %v, want 925（1000 - 60 竞价 - 15 差额）", buyout["gold_after"])
	}
	if buyout["buyer_id"] != "11" || buyout["buyer_name"] != "Bidder" || buyout["seller_id"] != "10" {
		t.Fatalf("一口价成交双方字段 = %+v", buyout)
	}

	if payout["event_name"] != "卖家领取收益" || payout["delivered"] != true {
		t.Fatalf("卖家收益记录 = %+v", payout)
	}
	if payout["sale_price"] != float64(75) || payout["fee_gold"] != float64(3) ||
		payout["deposit_gold"] != float64(dnfauction.ListingDepositGold) ||
		payout["seller_proceeds"] != float64(75-3+dnfauction.ListingDepositGold) {
		t.Fatalf("卖家收益金额字段 = %+v", payout)
	}
	if payout["seller_id"] != "10" || payout["seller_name"] != "Seller" {
		t.Fatalf("卖家收益身份字段 = %+v", payout)
	}

	// 补发扫描跑两轮（等价于期间又来了两条拍卖命令）：一行都不许多写。
	for round := 0; round < 2; round++ {
		service.retryCurrentAuctionMailsLocked(bidder)
		service.settleCurrentAuctionWonListingsLocked(bidder)
	}
	recorder.flush()
	again := readRecordFile(t, dir, recordKindAuction)
	if len(again) != len(entries) {
		t.Fatalf("重跑结算/补发后记录条数 = %d, want %d（重复运行必须一条都不写）: %+v",
			len(again), len(entries), again)
	}
}

// TestAuctionWonSettlementRecordsOnce 覆盖"竞拍成交"这条路径：
// 到期且有竞价的挂单由任意一条拍卖命令触发结算，重复触发只记一次。
func TestAuctionWonSettlementRecordsOnce(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	seedCurrentAuctionBridgeCharacter(t, ctx, repositories, "10", "account-seller", "Seller", 20_000, map[string]dnfrepo.ItemStack{
		"0:9": {ItemID: 6001, Count: 1, RawEntry: make([]byte, currentItemListEntryWireSize)},
	})
	seedCurrentAuctionBridgeCharacter(t, ctx, repositories, "11", "account-bidder", "Bidder", 1_000, nil)

	recorder, dir := newTestRecorder(t)
	service := auctionRecordTestService(repositories)
	service.records.Store(recorder)
	service.nextAuctionListingID.Store(200)

	seller := &gameSession{conn: &bufferConn{}, selectedCharacterID: 10, accountID: "account-seller"}
	// 只开竞拍（一口价 -1、单价 0）：到点之后由结算扫描卖给最高出价者。
	if err := service.registerCurrentAuctionItemLocked(seller,
		buildCurrentAuctionRegisterRequestForTest(9, 6001, 1, 50, -1, 0, 24)); err != nil {
		t.Fatal(err)
	}
	listingID := service.nextAuctionListingID.Load()

	bidder := &gameSession{conn: &bufferConn{}, selectedCharacterID: 11, accountID: "account-bidder"}
	if err := service.buyCurrentAuctionItemLocked(bidder, uint16(dnfenum.CmdPacketAuctionBidding),
		auctionBidBody(listingID, 60, 6001), false); err != nil {
		t.Fatal(err)
	}
	auctionAgeListing(t, ctx, repositories)

	service.settleCurrentAuctionWonListingsLocked(bidder)
	// 再跑两轮：SettleWonListings 只返回本次真的结算掉的行，重复跑必须是空列表。
	service.settleCurrentAuctionWonListingsLocked(bidder)
	service.settleCurrentAuctionWonListingsLocked(bidder)
	recorder.flush()

	entries := readRecordFile(t, dir, recordKindAuction)
	won := auctionRecordEvent(t, entries, "won")
	payout := auctionRecordEvent(t, entries, "payout")
	if won["listing_id"] != float64(listingID) || won["item_id"] != float64(6001) ||
		won["item_count"] != float64(1) {
		t.Fatalf("竞拍成交记录 = %+v", won)
	}
	if won["sale_price"] != float64(60) || won["fee_gold"] != float64(3) ||
		won["seller_proceeds"] != float64(60-3+dnfauction.ListingDepositGold) {
		t.Fatalf("竞拍成交金额字段 = %+v", won)
	}
	if won["buyer_id"] != "11" || won["seller_id"] != "10" || won["seller_name"] != "Seller" {
		t.Fatalf("竞拍成交双方字段 = %+v", won)
	}
	if won["buyer_mailed"] != true || won["delivered"] != true {
		t.Fatalf("竞拍成交的投递状态 = %+v", won)
	}
	if payout["listing_id"] != float64(listingID) ||
		payout["seller_proceeds"] != float64(60-3+dnfauction.ListingDepositGold) ||
		payout["delivered"] != true {
		t.Fatalf("结算产生的卖家收益记录 = %+v", payout)
	}
	if got := auctionRecordCount(entries, "list"); got != 1 {
		t.Fatalf("上架记录条数 = %d, want 1", got)
	}
}

// TestAuctionRegisterRejectionIsRecorded 覆盖被拒绝的上架：
// 结果必须是 result=rejected + reason=<阶段名>，而不是只留一行 packet_log。
func TestAuctionRegisterRejectionIsRecorded(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	seedCurrentAuctionBridgeCharacter(t, ctx, repositories, "10", "account-seller", "Seller", 20_000, map[string]dnfrepo.ItemStack{
		"0:9": {ItemID: 6001, Count: 1, RawEntry: make([]byte, currentItemListEntryWireSize)},
	})

	recorder, dir := newTestRecorder(t)
	service := auctionRecordTestService(repositories)
	service.auctionItemEligibility = func(uint32) bool { return false }
	service.records.Store(recorder)
	service.nextAuctionListingID.Store(300)

	seller := &gameSession{conn: &bufferConn{}, selectedCharacterID: 10, accountID: "account-seller"}
	if err := service.registerCurrentAuctionItemLocked(seller,
		buildCurrentAuctionRegisterRequestForTest(9, 6001, 1, 50, 75, 75, 24)); err != nil {
		t.Fatal(err)
	}
	recorder.flush()

	entries := readRecordFile(t, dir, recordKindAuction)
	rejected := auctionRecordEvent(t, entries, "list")
	if rejected["result"] != "rejected" || rejected["reason"] != "item_policy" {
		t.Fatalf("被拒上架记录 = %+v, want result=rejected reason=item_policy", rejected)
	}
	if rejected["item_id"] != float64(6001) || rejected["item_count"] != float64(1) ||
		rejected["source_slot"] != float64(9) {
		t.Fatalf("被拒上架缺少尝试内容 = %+v", rejected)
	}
	if rejected["client_code"] != float64(currentAuctionErrorItemRejected) {
		t.Fatalf("被拒上架 client_code = %v, want %d", rejected["client_code"], currentAuctionErrorItemRejected)
	}
	if _, ok := rejected["account_id"].(string); !ok {
		t.Fatalf("被拒上架 account_id 落盘类型 = %T, want string", rejected["account_id"])
	}
}
