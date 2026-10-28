package dnfbridge

import (
	"context"
	"encoding/binary"
	"os"
	"strings"
	"testing"

	dnfcerashop "longheng.io/server/internal/modules/dnf/cerashop"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

// 现场症状：商城"多买多送"窗口永远显示 0/N，买到档位也不给东西。
//
// 根因两条（都已由反编译证实，见验收规格附录 E）：
//  1. 客户端**没有**上行"请求次数"包，也没有"领取"包 —— 次数纯靠服务端推
//     `class0/0x040B`（1 个 u32 绝对次数），奖励必须服务端在同一事务里发；
//  2. 事件表 `event/chn_event/chn_buycountgiftsecond.evt` 从来没被解析过。
//
// 这组测试守住：解析、计数、跨档发奖、下发，以及**活动没配置时行为与改动前完全一致**。

// currentCeraShopBuyCountGiftSecondRealEVT 是真机 `runtime/data/dnf/Script.pvf` 里
// 那份 `event/chn_event/chn_buycountgiftsecond.evt` 的原文（用
// `go run ./cmd/scratch/pvfsearch -read=...` 导出）。测试直接用它，避免夹具漂移。
const currentCeraShopBuyCountGiftSecondRealEVT = "[bonus event ipg]\n" +
	"104764 104765 104766 104767 104768 104769 104770 104771 104772 104773 104774 104775 104776 104777 104778 104779 " +
	"104780 104781 104782 104783 104784 104785 104786 104787 104788 104789 104790 104791 104792 104793 104794 104795 " +
	"104796 104797 104798 104799 104800 104801 104802 104803 104804 104805 104806 104807 104808 104809 104810 104811 " +
	"104812 104813 104814 104815 104816 104817 104818 104819 108458 108459 108460 108461 108462 108463 108464 108465 " +
	"108466 108467 108468 108469 108470 108471 108472 108473 108474 108475 108476 108477 108478 108479 108480 108481 " +
	"108482 108483 108484 108485 108486 108487 108488 108489 108490 108491 108492 108493 108494 108495 108496 108497 " +
	"108498 108499 108500 108501 108502 108503 108504 108505 108506 108507 108508 108509 108510 108511 108512 108513\n" +
	"[/bonus event ipg]\n\n" +
	"[bonus item]\n" +
	"1 490702364 1 2 490702365 1 3 490702366 1 4 490702367 1 5 490702368 1 6 490702369 1\n" +
	"[/bonus item]\n\n" +
	"[buy count gift type]\n0\n[/buy count gift type]\n"

// currentCeraShopBuyCountGiftTestCommodityID 是真实事件表 `[bonus event ipg]` 里
// 的第一个商品号（104764）。e2e 用例直接买它，这样连商品号的来源都不用编。
const currentCeraShopBuyCountGiftTestCommodityID uint32 = 104764

// currentCeraShopBuyCountGiftTestSource 在共用商城夹具上追加多买多送事件表，把
// `[bonus event ipg]` 的第一个商品号挂进商品表，并把 6 件奖励道具挂进 stackable.lst。
//
// rewardIDs 允许覆盖奖励道具号，用来测"奖励道具缺失"这类边界。
func currentCeraShopBuyCountGiftTestSource(rewardIDs ...uint32) bridgePVFSource {
	if len(rewardIDs) == 0 {
		rewardIDs = []uint32{490702364, 490702365, 490702366, 490702367, 490702368, 490702369}
	}
	source := currentCeraShopTestSource()
	source[currentCeraShopBuyCountGiftSecondPVFPath] = currentCeraShopBuyCountGiftSecondRealEVT
	// 商品行是 9 个 token：商品号 道具号 数量 金币 点券 点券价 名称 0 0。
	source["etc/cerashop.etc"] = strings.Replace(source["etc/cerashop.etc"], "[/item]",
		itoaForTest(int64(currentCeraShopBuyCountGiftTestCommodityID))+" 37 3 0 0 80 `test buycount gift potion` 0 0\n[/item]", 1)
	lst := source["stackable/stackable.lst"]
	var tiers strings.Builder
	for index, itemID := range rewardIDs {
		lst += itoaForTest(int64(itemID)) + " `cash/chn_" + itoaForTest(int64(itemID)) + ".stk`\n"
		source["stackable/cash/chn_"+itoaForTest(int64(itemID))+".stk"] =
			"[name]\n`多买多送奖励 " + itoaForTest(int64(index+1)) + "`\n[stackable type]\n`[etc]`\n[stack limit]\n100\n"
		tiers.WriteString(itoaForTest(int64(index+1)) + " " + itoaForTest(int64(itemID)) + " 1 ")
	}
	source["stackable/stackable.lst"] = lst
	// 用真实档位表（6 档）替换，默认参数下与真机原文逐字节一致。
	source[currentCeraShopBuyCountGiftSecondPVFPath] = strings.Replace(
		currentCeraShopBuyCountGiftSecondRealEVT,
		"1 490702364 1 2 490702365 1 3 490702366 1 4 490702367 1 5 490702368 1 6 490702369 1",
		strings.TrimSpace(tiers.String()), 1)
	return source
}

func itoaForTest(value int64) string {
	if value == 0 {
		return "0"
	}
	var digits []byte
	for value > 0 {
		digits = append([]byte{byte('0' + value%10)}, digits...)
		value /= 10
	}
	return string(digits)
}

func mustCurrentCeraShopBuyCountGiftCatalog(t *testing.T, source bridgePVFSource) *pvfCeraShopCatalog {
	t.Helper()
	catalog, err := newPVFCeraShopCatalog(source)
	if err != nil {
		t.Fatal(err)
	}
	return catalog
}

type currentCeraShopBuyCountGiftRangeSource struct {
	bridgePVFSource
	paths []string
}

func (s *currentCeraShopBuyCountGiftRangeSource) RangePaths(yield func(string) bool) {
	for _, path := range s.paths {
		if !yield(path) {
			return
		}
	}
}

func TestParseCurrentCeraShopBuyCountGiftDiscoversExtraCampaigns(t *testing.T) {
	const extraPath = currentCeraShopBuyCountGiftExtraPVFPathPrefix + "b.evt"
	source := &currentCeraShopBuyCountGiftRangeSource{
		bridgePVFSource: bridgePVFSource{
			currentCeraShopBuyCountGiftSecondPVFPath: currentCeraShopBuyCountGiftSecondRealEVT,
			extraPath: "[bonus event ipg]\n" +
				"200001 200002\n" +
				"[/bonus event ipg]\n" +
				"[bonus item]\n" +
				"2 490702364 1\n" +
				"[/bonus item]\n" +
				"[buy count gift type]\n0\n[/buy count gift type]\n",
		},
		paths: []string{
			currentCeraShopBuyCountGiftSecondPVFPath,
			extraPath,
			currentCeraShopBuyCountGiftExtraPVFPathPrefix + "README.txt",
		},
	}
	plans := parseCurrentCeraShopBuyCountGiftPlans(source)
	if len(plans) != 2 {
		t.Fatalf("plans=%d want=2", len(plans))
	}
	if active := plans[1]; active.Primary || active.GroupID != "b" ||
		active.CountKey != currentCeraShopBuyCountGiftSecondCountKey+".b" ||
		!active.Enabled() || len(active.Tiers()) != 1 || active.Tiers()[0].Tier != 2 {
		t.Fatalf("extra plan=%+v tiers=%+v", active, active.Tiers())
	}
}

func TestParseCurrentCeraShopBuyCountGiftMalformedExtraKeepsPrimary(t *testing.T) {
	const extraPath = currentCeraShopBuyCountGiftExtraPVFPathPrefix + "broken.evt"
	source := &currentCeraShopBuyCountGiftRangeSource{
		bridgePVFSource: bridgePVFSource{
			currentCeraShopBuyCountGiftSecondPVFPath: currentCeraShopBuyCountGiftSecondRealEVT,
			extraPath: "[bonus event ipg]\n200001\n[/bonus event ipg]\n" +
				"[bonus item]\n1 490702364\n[/bonus item]\n",
		},
		paths: []string{
			currentCeraShopBuyCountGiftSecondPVFPath,
			extraPath,
		},
	}
	plans := parseCurrentCeraShopBuyCountGiftPlans(source)
	if len(plans) != 2 || !plans[0].Enabled() {
		t.Fatalf("primary must remain enabled plans=%+v", plans)
	}
	if plans[1].Enabled() {
		t.Fatal("malformed extra campaign must stay disabled")
	}
}

func TestParseCurrentCeraShopBuyCountGiftSecondReadsRealEventTable(t *testing.T) {
	source := bridgePVFSource{currentCeraShopBuyCountGiftSecondPVFPath: currentCeraShopBuyCountGiftSecondRealEVT}
	plan := parseCurrentCeraShopBuyCountGiftSecond(source)
	if !plan.Enabled() {
		t.Fatalf("真机事件表必须启用，实际 disabled=%q", plan.disabledReason)
	}
	if plan.giftType != currentCeraShopBuyCountGiftTypePerPurchase {
		t.Fatalf("giftType=%d", plan.giftType)
	}
	// 112 个商品号：104764-104819 + 108458-108513。
	if len(plan.commodities) != 112 {
		t.Fatalf("参与计数的商品数=%d want=112", len(plan.commodities))
	}
	for _, commodityID := range []uint32{104764, 104819, 108458, 108513} {
		if !plan.CountsTowardGift(commodityID) {
			t.Fatalf("商品 %d 应在 [bonus event ipg] 里", commodityID)
		}
	}
	for _, commodityID := range []uint32{104763, 104820, 108457, 108514, 0} {
		if plan.CountsTowardGift(commodityID) {
			t.Fatalf("商品 %d 不在 [bonus event ipg] 里，不该计数", commodityID)
		}
	}
	// 6 档，且**第 1 档就有奖励**（不要照抄 86JP 参考实现里"第 1 档为空"的写法）。
	tiers := plan.Tiers()
	if len(tiers) != 6 || plan.MaxTier() != 6 {
		t.Fatalf("档位=%+v max=%d", tiers, plan.MaxTier())
	}
	for index, tier := range tiers {
		wantTier := int64(index + 1)
		wantItem := uint32(490702363 + index + 1)
		if tier.Tier != wantTier || tier.ItemID != wantItem || tier.Count != 1 {
			t.Fatalf("档位[%d]=%+v want tier=%d item=%d count=1", index, tier, wantTier, wantItem)
		}
	}
}

// 事件表是可选配置：任何异常都只能让活动安静地不启用，绝不能返回错误把整个商城
// 目录构造带崩（商城里还有一堆无关商品）。
func TestParseCurrentCeraShopBuyCountGiftSecondFailsSafe(t *testing.T) {
	base := currentCeraShopBuyCountGiftSecondRealEVT
	cases := map[string]string{
		"missing_section_ipg":  strings.Replace(base, "[bonus event ipg]", "[something else]", 1),
		"missing_section_item": strings.Replace(base, "[bonus item]", "[something else]", 1),
		"empty_ipg":            strings.Replace(base, "104764 104765", "[/bonus event ipg]\n[ignored]\n1 2", 1),
		"torn_bonus_item_rows": strings.Replace(base, "1 490702364 1 2 490702365 1", "1 490702364 1 2", 1),
		"type_one":             strings.Replace(base, "[buy count gift type]\n0\n", "[buy count gift type]\n1\n", 1),
		"type_out_of_bound":    strings.Replace(base, "[buy count gift type]\n0\n", "[buy count gift type]\n9\n", 1),
	}
	for name, document := range cases {
		t.Run(name, func(t *testing.T) {
			plan := parseCurrentCeraShopBuyCountGiftSecond(bridgePVFSource{
				currentCeraShopBuyCountGiftSecondPVFPath: document,
			})
			if plan.Enabled() {
				t.Fatalf("异常表不该启用活动（document=%q）", document[:min(len(document), 60)])
			}
			if plan.disabledReason == "" {
				t.Fatal("不启用时必须留下原因，便于线上定位")
			}
		})
	}
	t.Run("no_source", func(t *testing.T) {
		plan := parseCurrentCeraShopBuyCountGiftSecond(nil)
		if plan.Enabled() || plan.disabledReason == "" {
			t.Fatalf("nil source 必须安全降级：%+v", plan)
		}
	})
	t.Run("no_document", func(t *testing.T) {
		plan := parseCurrentCeraShopBuyCountGiftSecond(bridgePVFSource{"etc/cerashop.etc": "[item]\n[/item]\n"})
		if plan.Enabled() || plan.disabledReason == "" {
			t.Fatalf("缺文件必须安全降级：%+v", plan)
		}
	})
}

// 奖励 = 本次新跨越的档位。计数单调递增，所以每个档位天然只发一次。
func TestCurrentCeraShopBuyCountGiftRewardsBetweenCrossedTiers(t *testing.T) {
	plan := parseCurrentCeraShopBuyCountGiftSecond(bridgePVFSource{
		currentCeraShopBuyCountGiftSecondPVFPath: currentCeraShopBuyCountGiftSecondRealEVT,
	})
	if !plan.Enabled() {
		t.Fatal(plan.disabledReason)
	}
	cases := []struct {
		before, after int64
		wantTiers     []int64
	}{
		{0, 0, nil},
		{0, 1, []int64{1}},                 // 第 1 档必须有奖励
		{1, 2, []int64{2}},                 // 逐次购买
		{0, 3, []int64{1, 2, 3}},           // 一次买 3 件（购物车多行）
		{5, 6, []int64{6}},                 // 顶档
		{6, 7, nil},                        // 超过顶档不再发
		{0, 99, []int64{1, 2, 3, 4, 5, 6}}, // 一次性跨越全部档位
		{3, 3, nil},                        // 没有增长
		{4, 2, nil},                        // 回退（不该发生）也不发
	}
	for _, testCase := range cases {
		rewards := plan.RewardsBetween(testCase.before, testCase.after)
		gotTiers := make([]int64, 0, len(rewards))
		for _, reward := range rewards {
			gotTiers = append(gotTiers, reward.Tier)
		}
		if len(gotTiers) != len(testCase.wantTiers) {
			t.Fatalf("(%d,%d] 档位=%v want=%v", testCase.before, testCase.after, gotTiers, testCase.wantTiers)
		}
		for index := range gotTiers {
			if gotTiers[index] != testCase.wantTiers[index] {
				t.Fatalf("(%d,%d] 档位=%v want=%v", testCase.before, testCase.after, gotTiers, testCase.wantTiers)
			}
		}
	}
}

// 包体 = 恰好 1 个 u32（小端、绝对次数）。客户端 handler 整包只读这一个 u32。
func TestCurrentCeraShopBuyCountGiftBodyIsExactlyOneUint32(t *testing.T) {
	for _, testCase := range []struct {
		count int64
		want  uint32
	}{{0, 0}, {1, 1}, {7, 7}, {100004, 100004}, {-5, 0}} {
		body := buildCurrentCeraShopBuyCountGiftBody(testCase.count)
		if len(body) != 4 {
			t.Fatalf("count=%d 包体长度=%d want=4", testCase.count, len(body))
		}
		if got := binary.LittleEndian.Uint32(body); got != testCase.want {
			t.Fatalf("count=%d 包体=%d want=%d", testCase.count, got, testCase.want)
		}
	}
}

// 端到端：连买 7 次参与活动的商品。
// 前 6 次逐档发奖（**第 1 档就发**），第 7 次只涨计数不再发。
func TestCurrentCeraShopPurchaseCountsBuyCountGiftAndGrantsTiersInOneTransaction(t *testing.T) {
	ctx := context.Background()
	catalog := mustCurrentCeraShopBuyCountGiftCatalog(t, currentCeraShopBuyCountGiftTestSource())
	if !catalog.BuyCountGiftPlan().Enabled() {
		t.Fatalf("测试目录里多买多送未启用: %q", catalog.BuyCountGiftPlan().disabledReason)
	}
	repositories := dnfrepomemory.NewMemoryGroup()
	// 点券给够：商品 100050 单价 80。
	seedCeraShopAccountCera(t, ctx, repositories, "account-1", 100000)
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "19", AccountID: "account-1", Level: 90,
		Stats: map[string]int64{"cera": 100000},
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
	session := &gameSession{conn: connection, connID: "buycount-gift", selectedCharacterID: 19}

	countRewardInInventory := func(itemID uint32) int64 {
		t.Helper()
		inventory, found, err := repositories.Inventory.Load(ctx, "19")
		if err != nil || !found {
			t.Fatalf("load inventory found=%t err=%v", found, err)
		}
		var total int64
		for _, stack := range inventory.Slots {
			if uint32(stack.ItemID) == itemID {
				total += int64(stack.Count)
			}
		}
		return total
	}

	for purchase := 1; purchase <= 7; purchase++ {
		connection.write.Reset()
		result, err := service.commitCurrentCeraShopPurchase(ctx, session, catalog, currentCeraShopPurchaseRequest{
			PaymentMode: currentCeraShopPaymentModeCera,
			Items:       []currentCeraShopPurchaseRequestItem{{CommodityID: currentCeraShopBuyCountGiftTestCommodityID}},
		})
		if err != nil {
			t.Fatalf("第 %d 次购买失败: %v", purchase, err)
		}
		if result.BuyCountGiftCount != int64(purchase) {
			t.Fatalf("第 %d 次购买后累计次数=%d", purchase, result.BuyCountGiftCount)
		}
		character, found, err := repositories.Character.Load(ctx, "19")
		if err != nil || !found {
			t.Fatalf("load character found=%t err=%v", found, err)
		}
		if got := currentCeraShopBuyCountGiftCount(character); got != int64(purchase) {
			t.Fatalf("第 %d 次购买后落库次数=%d（必须落库，小退/重启后要还在）", purchase, got)
		}
		if purchase <= 6 {
			// 第 N 次购买发第 N 档奖励（道具 490702363+N）。
			rewardItemID := uint32(490702363 + purchase)
			if got := countRewardInInventory(rewardItemID); got != 1 {
				t.Fatalf("第 %d 次购买后奖励道具 %d 数量=%d want=1", purchase, rewardItemID, got)
			}
			if len(result.BuyCountGiftNotes) != 1 ||
				!strings.Contains(result.BuyCountGiftNotes[0], "tier:"+itoaForTest(int64(purchase))) {
				t.Fatalf("第 %d 次购买的跨档记录=%v", purchase, result.BuyCountGiftNotes)
			}
			// 弹窗数据必须一起产出：handler 就是按这个列表逐条发 `class0/op852` 的。
			if len(result.BuyCountGiftRewards) != 1 || result.BuyCountGiftRewards[0].ItemID != rewardItemID {
				t.Fatalf("第 %d 次购买的弹窗数据=%+v want item=%d",
					purchase, result.BuyCountGiftRewards, rewardItemID)
			}
		} else {
			if len(result.BuyCountGiftNotes) != 0 {
				t.Fatalf("第 7 次购买已超过顶档，不该再发奖：%v", result.BuyCountGiftNotes)
			}
			if len(result.BuyCountGiftRewards) != 0 {
				t.Fatalf("第 7 次购买不该产出弹窗：%+v", result.BuyCountGiftRewards)
			}
		}
	}
	// 6 档奖励各只发过一次。
	for tier := 1; tier <= 6; tier++ {
		if got := countRewardInInventory(uint32(490702363 + tier)); got != 1 {
			t.Fatalf("第 %d 档奖励数量=%d want=1（每档只能发一次）", tier, got)
		}
	}
}

func TestCurrentCeraShopPurchaseKeepsExtraCampaignCountsIndependent(t *testing.T) {
	ctx := context.Background()
	const (
		acommodity uint32 = 104764
		bcommodity uint32 = 200001
		areward    uint32 = 490702364
		breward    uint32 = 490702365
	)
	extraPath := currentCeraShopBuyCountGiftExtraPVFPathPrefix + "b.evt"
	base := currentCeraShopBuyCountGiftTestSource()
	base["etc/cerashop.etc"] = strings.Replace(
		base["etc/cerashop.etc"],
		"[/item]",
		itoaForTest(int64(bcommodity))+" 37 3 0 0 80 `test b buycount gift potion` 0 0\n[/item]",
		1,
	)
	base[extraPath] = "[bonus event ipg]\n" +
		itoaForTest(int64(bcommodity)) + "\n" +
		"[/bonus event ipg]\n" +
		"[bonus item]\n" +
		"1 " + itoaForTest(int64(breward)) + " 1\n" +
		"[/bonus item]\n" +
		"[buy count gift type]\n0\n[/buy count gift type]\n"
	source := &currentCeraShopBuyCountGiftRangeSource{
		bridgePVFSource: base,
		paths: []string{
			currentCeraShopBuyCountGiftSecondPVFPath,
			extraPath,
		},
	}
	catalog, err := newPVFCeraShopCatalog(source)
	if err != nil {
		t.Fatal(err)
	}
	plans := catalog.BuyCountGiftPlans()
	if len(plans) != 2 || plans[0].GroupID != "primary" || plans[1].GroupID != "b" {
		t.Fatalf("campaign plans=%+v", plans)
	}

	repositories := dnfrepomemory.NewMemoryGroup()
	seedCeraShopAccountCera(t, ctx, repositories, "account-1", 100000)
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "19", AccountID: "account-1", Level: 90,
		Stats: map[string]int64{"cera": 100000},
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
	session := &gameSession{conn: connection, connID: "buycount-multi", selectedCharacterID: 19}
	purchase := func(commodityID uint32) currentCeraShopCommitResult {
		t.Helper()
		result, purchaseErr := service.commitCurrentCeraShopPurchase(ctx, session, catalog, currentCeraShopPurchaseRequest{
			PaymentMode: currentCeraShopPaymentModeCera,
			Items:       []currentCeraShopPurchaseRequestItem{{CommodityID: commodityID}},
		})
		if purchaseErr != nil {
			t.Fatalf("purchase commodity=%d: %v", commodityID, purchaseErr)
		}
		return result
	}
	count := func() dnfrepo.CharacterRecord {
		t.Helper()
		character, found, loadErr := repositories.Character.Load(ctx, "19")
		if loadErr != nil || !found {
			t.Fatalf("load character found=%t err=%v", found, loadErr)
		}
		return character
	}
	inventoryCount := func(itemID uint32) int64 {
		t.Helper()
		inventory, found, loadErr := repositories.Inventory.Load(ctx, "19")
		if loadErr != nil || !found {
			t.Fatalf("load inventory found=%t err=%v", found, loadErr)
		}
		var total int64
		for _, stack := range inventory.Slots {
			if uint32(stack.ItemID) == itemID {
				total += int64(stack.Count)
			}
		}
		return total
	}

	result := purchase(acommodity)
	if result.BuyCountGiftCount != 1 || len(result.BuyCountGiftRewards) != 1 ||
		result.BuyCountGiftRewards[0].ItemID != areward {
		t.Fatalf("A purchase result=%+v", result)
	}
	character := count()
	if got := currentCeraShopBuyCountGiftCountForPlan(character, plans[0]); got != 1 {
		t.Fatalf("A count=%d want=1", got)
	}
	if got := currentCeraShopBuyCountGiftCountForPlan(character, plans[1]); got != 0 {
		t.Fatalf("B count after A=%d want=0", got)
	}
	if inventoryCount(areward) != 1 || inventoryCount(breward) != 0 {
		t.Fatalf("rewards after A: A=%d B=%d", inventoryCount(areward), inventoryCount(breward))
	}

	result = purchase(bcommodity)
	// The primary count is the only value projected into the current client UI,
	// so buying B must not change it.
	if result.BuyCountGiftCount != 1 || len(result.BuyCountGiftRewards) != 1 ||
		result.BuyCountGiftRewards[0].ItemID != breward {
		t.Fatalf("B purchase result=%+v", result)
	}
	character = count()
	if got := currentCeraShopBuyCountGiftCountForPlan(character, plans[0]); got != 1 {
		t.Fatalf("A count after B=%d want=1", got)
	}
	if got := currentCeraShopBuyCountGiftCountForPlan(character, plans[1]); got != 1 {
		t.Fatalf("B count=%d want=1", got)
	}
	if inventoryCount(areward) != 1 || inventoryCount(breward) != 1 {
		t.Fatalf("rewards after B: A=%d B=%d", inventoryCount(areward), inventoryCount(breward))
	}
}

// 不在 `[bonus event ipg]` 里的商品不计数、不发奖。
func TestCurrentCeraShopPurchaseIgnoresCommodityOutsideBonusEventIPG(t *testing.T) {
	ctx := context.Background()
	catalog := mustCurrentCeraShopBuyCountGiftCatalog(t, currentCeraShopBuyCountGiftTestSource())
	repositories := dnfrepomemory.NewMemoryGroup()
	seedCeraShopAccountCera(t, ctx, repositories, "account-1", 10000)
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "19", AccountID: "account-1", Level: 90, Stats: map[string]int64{"cera": 10000},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{CharacterID: "19", Slots: map[string]dnfrepo.ItemStack{}}); err != nil {
		t.Fatal(err)
	}
	service := &Service{
		options:            options{accountID: "account-1"},
		ceraShopCatalog:    catalog,
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
	}
	session := &gameSession{selectedCharacterID: 19}

	// 100056 是夹具里的普通商品，不在事件表的 112 个商品号里。
	result, err := service.commitCurrentCeraShopPurchase(ctx, session, catalog, currentCeraShopPurchaseRequest{
		PaymentMode: currentCeraShopPaymentModeCera,
		Items:       []currentCeraShopPurchaseRequestItem{{CommodityID: 100056}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.BuyCountGiftCount != 0 || len(result.BuyCountGiftNotes) != 0 {
		t.Fatalf("不参与活动的商品不该计数: count=%d notes=%v", result.BuyCountGiftCount, result.BuyCountGiftNotes)
	}
	character, _, err := repositories.Character.Load(ctx, "19")
	if err != nil {
		t.Fatal(err)
	}
	if _, found := character.Stats[currentCeraShopBuyCountGiftSecondCountKey]; found {
		t.Fatalf("不该在角色 Stats 里留下计数键: %v", character.Stats)
	}
}

// 回归守卫：活动**没配置**时，购买行为必须与改动前逐字节一致
// （不计数、不发奖、不产生任何额外包、不写任何新键）。
func TestCurrentCeraShopBuyCountGiftDisabledLeavesPurchaseBehaviourUnchanged(t *testing.T) {
	ctx := context.Background()
	// 注意：这里刻意用**共用夹具**（不含事件表），就是改动前所有既有用例用的那份。
	catalog := mustCurrentCeraShopTestCatalog(t)
	if catalog.BuyCountGiftPlan().Enabled() {
		t.Fatal("共用夹具不该启用多买多送（否则既有用例的包序列期望会漂移）")
	}
	repositories := dnfrepomemory.NewMemoryGroup()
	seedCeraShopAccountCera(t, ctx, repositories, "account-1", 1000)
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "19", AccountID: "account-1", Level: 90, Stats: map[string]int64{"cera": 1000},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{CharacterID: "19", Slots: map[string]dnfrepo.ItemStack{}}); err != nil {
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
		t.Fatal(err)
	}
	if result.BuyCountGiftCount != 0 || len(result.BuyCountGiftNotes) != 0 {
		t.Fatalf("未配置活动时不该有任何多买多送副作用: count=%d notes=%v", result.BuyCountGiftCount, result.BuyCountGiftNotes)
	}
	character, _, err := repositories.Character.Load(ctx, "19")
	if err != nil {
		t.Fatal(err)
	}
	if _, found := character.Stats[currentCeraShopBuyCountGiftSecondCountKey]; found {
		t.Fatalf("未配置活动时不该写计数键: %v", character.Stats)
	}
	// 商品照常进包（确认我们只是"不做多余的事"，没有把正常购买弄坏）。
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
		t.Fatalf("商品 100050 应照常发 3 个道具 37，实际=%d", granted)
	}
}

// 下发：类 0 / op1035，包体 4 字节绝对次数。
func TestCurrentCeraShopBuyCountGiftProjectionSendsAbsoluteCount(t *testing.T) {
	ctx := context.Background()
	catalog := mustCurrentCeraShopBuyCountGiftCatalog(t, currentCeraShopBuyCountGiftTestSource())
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "19", AccountID: "account-1", Level: 90,
		Stats: map[string]int64{currentCeraShopBuyCountGiftSecondCountKey: 4},
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
	session := &gameSession{conn: connection, connID: "buycount-push", selectedCharacterID: 19}

	if err := service.sendCurrentCeraShopBuyCountGiftProjection(ctx, session, "unit_test"); err != nil {
		t.Fatal(err)
	}
	packet, trailing := splitGameServerUpperPacket(t, connection.write.Bytes())
	if len(trailing) != 0 {
		t.Fatalf("通知后有多余字节: %x", trailing)
	}
	// 必须是类 0：同一个 1035 在类 1 表里是婚礼命令回包。
	if packet.Header.Classification != 0 || packet.Header.MsgID != currentCeraShopBuyCountGiftSecondMsgID {
		t.Fatalf("header=%+v want=class0/op%#x", packet.Header, currentCeraShopBuyCountGiftSecondMsgID)
	}
	body := packet.Body
	if len(body) > 3 && body[0] == 0 && body[1] == 0 && body[2] == 0 {
		body = body[3:] // server16 头模式在包体前带 3 个固定 0 字节
	}
	if len(body) != 4 || binary.LittleEndian.Uint32(body) != 4 {
		t.Fatalf("通知体=%x want=4 字节且值 4", packet.Body)
	}
}

// 次数为 0 时**照发**：客户端 handler 会无条件把 0 写进 +0x274C0（写在可见性判断之前），
// 正好清掉上一次会话残留的旧次数。
func TestCurrentCeraShopBuyCountGiftProjectionSendsZeroToClearStaleCount(t *testing.T) {
	ctx := context.Background()
	catalog := mustCurrentCeraShopBuyCountGiftCatalog(t, currentCeraShopBuyCountGiftTestSource())
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "19", AccountID: "account-1", Level: 90, Stats: map[string]int64{},
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
	session := &gameSession{conn: connection, connID: "buycount-zero", selectedCharacterID: 19}
	if err := service.sendCurrentCeraShopBuyCountGiftProjection(ctx, session, "unit_test"); err != nil {
		t.Fatal(err)
	}
	packet, _ := splitGameServerUpperPacket(t, connection.write.Bytes())
	body := packet.Body
	if len(body) > 3 && body[0] == 0 && body[1] == 0 && body[2] == 0 {
		body = body[3:]
	}
	if len(body) != 4 || binary.LittleEndian.Uint32(body) != 0 {
		t.Fatalf("次数为 0 时也要发一个 0，实际=%x", packet.Body)
	}
}

// 活动没配置 / 取不到角色 / session 无效时都不发包、不报错。
func TestCurrentCeraShopBuyCountGiftProjectionStaysSilentWhenUnavailable(t *testing.T) {
	ctx := context.Background()
	connection := &bufferConn{}

	t.Run("event_not_configured", func(t *testing.T) {
		connection.write.Reset()
		service := &Service{options: options{accountID: "account-1"}, ceraShopCatalog: mustCurrentCeraShopTestCatalog(t)}
		session := &gameSession{conn: connection, connID: "no-event", selectedCharacterID: 19}
		if err := service.sendCurrentCeraShopBuyCountGiftProjection(ctx, session, "unit_test"); err != nil {
			t.Fatal(err)
		}
		if connection.write.Len() != 0 {
			t.Fatalf("活动未配置时不该发包: %x", connection.write.Bytes())
		}
	})

	t.Run("character_missing", func(t *testing.T) {
		connection.write.Reset()
		repositories := dnfrepomemory.NewMemoryGroup()
		service := &Service{
			options:            options{accountID: "account-1"},
			ceraShopCatalog:    mustCurrentCeraShopBuyCountGiftCatalog(t, currentCeraShopBuyCountGiftTestSource()),
			repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
		}
		session := &gameSession{conn: connection, connID: "no-char", selectedCharacterID: 19}
		if err := service.sendCurrentCeraShopBuyCountGiftProjection(ctx, session, "unit_test"); err != nil {
			t.Fatal(err)
		}
		if connection.write.Len() != 0 {
			t.Fatalf("角色不存在时不该发包: %x", connection.write.Bytes())
		}
	})

	t.Run("no_character_selected", func(t *testing.T) {
		connection.write.Reset()
		service := &Service{options: options{accountID: "account-1"}}
		if err := service.sendCurrentCeraShopBuyCountGiftProjection(ctx, &gameSession{conn: connection}, "unit_test"); err != nil {
			t.Fatal(err)
		}
		if connection.write.Len() != 0 {
			t.Fatalf("没选角色时不该发包: %x", connection.write.Bytes())
		}
	})

	t.Run("nil_inputs", func(t *testing.T) {
		service := &Service{options: options{accountID: "account-1"}}
		if err := service.sendCurrentCeraShopBuyCountGiftProjection(ctx, nil, "unit_test"); err != nil {
			t.Fatalf("nil session 不应报错: %v", err)
		}
		if err := (*Service)(nil).sendCurrentCeraShopBuyCountGiftProjection(ctx, &gameSession{selectedCharacterID: 19}, "unit_test"); err != nil {
			t.Fatalf("nil service 不应报错: %v", err)
		}
	})
}

// 奖励道具在 PVF 里解析不出来时，整笔结账失败（点券不扣）——
// "扣了钱没给奖励"是绝对不能出现的结果。
func TestCurrentCeraShopPurchaseFailsWholeCheckoutWhenGiftItemIsMissing(t *testing.T) {
	ctx := context.Background()
	// 档位表引用 490702364，但 stackable.lst 里不挂这件道具。
	source := currentCeraShopBuyCountGiftTestSource()
	source[currentCeraShopBuyCountGiftSecondPVFPath] = strings.Replace(
		currentCeraShopBuyCountGiftSecondRealEVT,
		"1 490702364 1 2 490702365 1 3 490702366 1 4 490702367 1 5 490702368 1 6 490702369 1",
		"1 490702364 1", 1)
	// 把奖励道具从 stackable.lst 里摘掉，制造"表里有、PVF 里没有"的场景。
	lines := strings.Split(source["stackable/stackable.lst"], "\n")
	var kept []string
	for _, line := range lines {
		if strings.Contains(line, "chn_490702364.stk") {
			continue
		}
		kept = append(kept, line)
	}
	source["stackable/stackable.lst"] = strings.Join(kept, "\n")
	catalog := mustCurrentCeraShopBuyCountGiftCatalog(t, source)
	repositories := dnfrepomemory.NewMemoryGroup()
	seedCeraShopAccountCera(t, ctx, repositories, "account-1", 100000)
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "19", AccountID: "account-1", Level: 90, Stats: map[string]int64{"cera": 100000},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{CharacterID: "19", Slots: map[string]dnfrepo.ItemStack{}}); err != nil {
		t.Fatal(err)
	}
	service := &Service{
		options:            options{accountID: "account-1"},
		ceraShopCatalog:    catalog,
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
	}
	session := &gameSession{selectedCharacterID: 19}

	if _, err := service.commitCurrentCeraShopPurchase(ctx, session, catalog, currentCeraShopPurchaseRequest{
		PaymentMode: currentCeraShopPaymentModeCera,
		Items:       []currentCeraShopPurchaseRequestItem{{CommodityID: currentCeraShopBuyCountGiftTestCommodityID}},
	}); err == nil {
		t.Fatal("奖励道具解析不出来时必须整笔失败")
	}
	// 点券不能被扣。
	if got := loadCeraShopAccountCera(t, ctx, repositories, "account-1"); got != 100000 {
		t.Fatalf("结账失败后点券=%d want=100000", got)
	}
	character, _, err := repositories.Character.Load(ctx, "19")
	if err != nil {
		t.Fatal(err)
	}
	if _, found := character.Stats[currentCeraShopBuyCountGiftSecondCountKey]; found {
		t.Fatalf("结账失败时不该留下计数: %v", character.Stats)
	}
}

// 真机 PVF 冒烟：用运行时的 Script.pvf 直接跑一遍解析器，确认它对**真实文件**有效，
// 而不只是对我们手写的夹具有效。
//
//	$env:DNFBRIDGE_REAL_PVF_SMOKE = "DNF_REAL_PVF"
//	go test ./internal/services/dnfbridge/ -run "RealPVF.*BuyCountGift|RealPVFVoucher" -v
func TestRealPVFCeraShopBuyCountGiftSecondPlanMatchesRuntimePVF(t *testing.T) {
	pvfPath := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE to the runtime Script.pvf")
	}
	archive, err := platformpvf.Open(pvfPath)
	if err != nil {
		t.Fatal(err)
	}
	plan := parseCurrentCeraShopBuyCountGiftSecond(archive)
	if !plan.Enabled() {
		t.Fatalf("真机事件表必须启用，实际 disabled=%q", plan.disabledReason)
	}
	// 真机 Script.pvf 的值：112 个参与商品号、6 档、type 0。
	if len(plan.commodities) != 112 {
		t.Fatalf("参与计数的商品数=%d want=112", len(plan.commodities))
	}
	if plan.MaxTier() != 6 || len(plan.Tiers()) != 6 {
		t.Fatalf("档位=%+v", plan.Tiers())
	}
	if plan.giftType != currentCeraShopBuyCountGiftTypePerPurchase {
		t.Fatalf("giftType=%d want=%d", plan.giftType, currentCeraShopBuyCountGiftTypePerPurchase)
	}
	// 第 1 档必须有奖励（不要照抄 86JP 参考实现里"第 1 档为空"的写法）。
	tiers := plan.Tiers()
	if tiers[0].Tier != 1 || tiers[0].ItemID != 490702364 || tiers[0].Count != 1 {
		t.Fatalf("第 1 档=%+v want tier=1 item=490702364 count=1", tiers[0])
	}
	// 每一档的奖励道具都必须能在真机 PVF 里解析出来，否则发奖时才发现就太晚了。
	items, err := newPVFDungeonDropCatalog(archive)
	if err != nil {
		t.Fatal(err)
	}
	for _, tier := range tiers {
		if _, err := items.ResolveItem(tier.ItemID); err != nil {
			t.Fatalf("第 %d 档奖励道具 %d 在真机 PVF 里解析失败: %v", tier.Tier, tier.ItemID, err)
		}
	}
}

// 真机 PVF 冒烟：两件券的道具号必须真的存在、且真的是普通 `[material]`
// （没有 `[action type]`）—— 这正是"客户端无法自行兑换、必须服务端入账"的证据。
func TestRealPVFVoucherItemsArePlainMaterials(t *testing.T) {
	pvfPath := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE to the runtime Script.pvf")
	}
	archive, err := platformpvf.Open(pvfPath)
	if err != nil {
		t.Fatal(err)
	}
	items, err := newPVFDungeonDropCatalog(archive)
	if err != nil {
		t.Fatal(err)
	}
	for _, voucher := range []struct {
		itemID int64
		name   string
	}{
		{dnfcerashop.TokenCeraVoucherItemID, "代币券"},
		{dnfcerashop.HappyTokenCeraVoucherItemID, "欢乐代币券"},
	} {
		definition, err := items.ResolveItem(uint32(voucher.itemID))
		if err != nil {
			t.Fatalf("%s(%d) 在真机 PVF 里解析失败: %v", voucher.name, voucher.itemID, err)
		}
		if definition.ItemID != uint32(voucher.itemID) {
			t.Fatalf("%s 解析出的道具号=%d want=%d", voucher.name, definition.ItemID, voucher.itemID)
		}
		// 没有 [action type] 就是客户端不能自行兑换的硬证据。
		if definition.ActionType != "" {
			t.Fatalf("%s 竟然带 [action type]=%q —— 那客户端可能真的能自己兑换，需要重新评估",
				voucher.name, definition.ActionType)
		}
	}
}

// 折扣券镜像号（基础商品号 + 1000）必须也计入活动 —— 否则玩家用折扣券买同一件礼包
// 就静默不计数。这个偏移来自 86JP 参考实现 CeraShopPurchaseBonusCatalog。
func TestCurrentCeraShopBuyCountGiftAcceptsDiscountCouponMirrorProduct(t *testing.T) {
	source := currentCeraShopBuyCountGiftTestSource()
	// 104764 在 [bonus event ipg] 里，发道具 37。
	// 105764 = 104764 + 1000，也发道具 37 → 是合法镜像。
	// 105765 = 104765 + 1000，但发道具 38（不同道具）→ 必须被拒。
	source["etc/cerashop.etc"] = strings.Replace(source["etc/cerashop.etc"], "[/item]",
		"105764 37 3 0 0 80 `test buycount gift potion (discount)` 0 0\n"+
			"105765 38 3 0 0 80 `unrelated discounted product` 0 0\n[/item]", 1)
	catalog := mustCurrentCeraShopBuyCountGiftCatalog(t, source)

	cases := []struct {
		commodityID uint32
		want        bool
		why         string
	}{
		{104764, true, "基础商品号，直列在 [bonus event ipg] 里"},
		{105764, true, "折扣券镜像号，且与基础商品发同一件道具"},
		{105765, false, "镜像号偏移 1000 但发的是别的道具，不能混进活动"},
		{104765, true, "基础商品号在清单里"},
		{999999, false, "完全无关的商品"},
		{999, false, "小于偏移量、不可能有基础行"},
		{0, false, "商品号 0"},
	}
	for _, testCase := range cases {
		if got := catalog.BuyCountGiftEligible(testCase.commodityID); got != testCase.want {
			t.Fatalf("BuyCountGiftEligible(%d)=%t want=%t（%s）",
				testCase.commodityID, got, testCase.want, testCase.why)
		}
	}
}

// 活动未配置时，镜像判定也必须一律为 false（不能因为"表里查不到"就放行）。
func TestCurrentCeraShopBuyCountGiftEligibleIsFalseWhenEventMissing(t *testing.T) {
	catalog := mustCurrentCeraShopTestCatalog(t) // 共用夹具：不含事件表
	for _, commodityID := range []uint32{104764, 105764, 100050} {
		if catalog.BuyCountGiftEligible(commodityID) {
			t.Fatalf("活动未配置时商品 %d 不该计入", commodityID)
		}
	}
	if (*pvfCeraShopCatalog)(nil).BuyCountGiftEligible(104764) {
		t.Fatal("nil catalog 必须安全返回 false")
	}
}
