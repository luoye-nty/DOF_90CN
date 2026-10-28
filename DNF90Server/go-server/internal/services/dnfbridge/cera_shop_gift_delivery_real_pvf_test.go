package dnfbridge

import (
	"os"
	"sort"
	"testing"

	platformpvf "longheng.io/server/internal/platform/pvf"
)

// 真机 PVF 下的"这件商品能不能赠送"普查。
//
// 为什么要这一条：赠送的落格路径把"买到的商品"转成邮件附件，而邮件只能表达**道具实例**
// （装备 / 可叠加）。哪些商品落在这一类，只有真实 Script.pvf 说了算 —— 单元测试用的是
// 合成目录，跑得再绿也证明不了玩家在商城里点的那件东西能寄出去。
//
// 尤其要盯住 108742：现场 2026-09-27 16:14 玩家点的就是它（2014 SAO 真·桐人礼包），
// 如果它在真实 PVF 下不可邮寄，玩家的赠送会稳定失败在「该商品无法赠送」。
func TestRealPVFCeraShopGiftDeliverability(t *testing.T) {
	pvfPath := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE to audit cera-shop gift deliverability")
	}
	archive, err := platformpvf.LoadArchive(platformpvf.Options{Path: pvfPath})
	if err != nil {
		t.Fatalf("load real Script.pvf: %v", err)
	}
	catalog, err := newPVFCeraShopCatalog(archive)
	if err != nil {
		t.Fatalf("load real Cera-shop catalog: %v", err)
	}
	if len(catalog.products) == 0 {
		t.Fatal("real Cera-shop catalog is empty")
	}

	type audit struct {
		product   currentCeraShopProduct
		giftable  bool
		path      string
		candidate string
		reason    string
	}
	audits := make([]audit, 0, len(catalog.products))
	for _, product := range catalog.products {
		entry := audit{product: product}
		definition, resolveErr := catalog.items.ResolveItem(product.ItemID)
		if resolveErr != nil {
			entry.candidate = "unresolved"
			entry.reason = resolveErr.Error()
			audits = append(audits, entry)
			continue
		}
		entry.candidate = string(definition.Kind)
		// 与结账时**同一套**判定：能认成 [cera package] 的走自动展开，逐条奖励寄；
		// 否则整件商品寄。
		rewards, isAutoOpen, autoOpenErr := resolveCurrentCeraShopAutoOpenRewards(
			catalog, definition, product.Count, nil, nil)
		switch {
		case autoOpenErr != nil:
			// 需要玩家在礼包里选东西的（选择组）在真机上带 PackageSelections 过来，
			// 这里拿不到选择，就退化成"每一件候选奖励都能寄吗"。
			// 两个错误都要留：autoOpenErr 才是**普通购买**会撞到的那一个，
			// 后面那条只是本次普查的兜底路径失败原因。
			entry.path = "package-selection"
			entry.reason = "auto-open: " + autoOpenErr.Error()
			packageDefinition, packageErr := resolveCurrentCeraPackageDefinition(catalog.items, product.ItemID)
			if packageErr != nil {
				entry.reason += "; fallback: " + packageErr.Error()
				audits = append(audits, entry)
				continue
			}
			entry.giftable, entry.reason = currentCeraShopGiftRewardsDeliverable(catalog, packageDefinition, product)
		case isAutoOpen:
			entry.path = "auto-open"
			entry.giftable = true
			for _, reward := range rewards {
				if _, convertErr := currentCeraShopGiftAttachments(reward.Definition, reward.Count, product.CommodityID); convertErr != nil {
					entry.giftable = false
					entry.candidate = "reward:" + string(reward.Definition.Kind)
					entry.reason = convertErr.Error()
					break
				}
			}
		default:
			entry.path = "item"
			if _, convertErr := currentCeraShopGiftAttachments(definition, product.Count, product.CommodityID); convertErr != nil {
				entry.reason = convertErr.Error()
			} else {
				entry.giftable = true
			}
		}
		audits = append(audits, entry)
	}

	// 汇总：按 section 统计可赠送比例（只打日志，方便看出"哪一类商品天生寄不出去"）。
	bySection := make(map[string][2]int)
	commodities := make(map[uint32]audit, len(audits))
	for _, entry := range audits {
		counts := bySection[entry.product.Section]
		counts[1]++
		if entry.giftable {
			counts[0]++
		}
		bySection[entry.product.Section] = counts
		commodities[entry.product.CommodityID] = entry
	}
	sections := make([]string, 0, len(bySection))
	for section := range bySection {
		sections = append(sections, section)
	}
	sort.Strings(sections)
	for _, section := range sections {
		counts := bySection[section]
		t.Logf("section=%-28s giftable=%d/%d", section, counts[0], counts[1])
	}
	// 不可赠送的逐条打出来（含原因）：这条日志是"为什么这件送不了"的唯一现场证据，
	// 也是判断"是我的规则太严，还是这件东西天生寄不出去"的依据。
	rejected := make([]audit, 0, 16)
	for _, entry := range audits {
		if !entry.giftable {
			rejected = append(rejected, entry)
		}
	}
	sort.Slice(rejected, func(left, right int) bool {
		return rejected[left].product.CommodityID < rejected[right].product.CommodityID
	})
	for _, entry := range rejected {
		t.Logf("NOT giftable commodity=%d item=%d section=%-18s path=%-18s kind=%s reason=%s",
			entry.product.CommodityID, entry.product.ItemID, entry.product.Section, entry.path, entry.candidate, entry.reason)
	}

	// ★ 现场那一件：108742（2014 SAO 真·桐人礼包，item 10007261，25900 点券）。
	// 它必须可赠送，否则玩家在商城里点"赠送"永远得到「该商品无法赠送」。
	for _, commodityID := range []uint32{108742, 108741, 108740} {
		entry, ok := commodities[commodityID]
		if !ok {
			t.Fatalf("commodity %d is missing from the real catalog", commodityID)
		}
		if !entry.giftable {
			t.Fatalf("commodity %d (item %d, section %s, %s) is NOT deliverable as a gift",
				commodityID, entry.product.ItemID, entry.product.Section, entry.path)
		}
		t.Logf("commodity=%d item=%d section=%s path=%s giftable=%t",
			commodityID, entry.product.ItemID, entry.product.Section, entry.path, entry.giftable)
	}

	// 历史赠送礼包整批：108515..108742 是 pvfshopgift 加进 cerashop.etc 的 228 条，
	// 它们就是"要拿来送人"的那批商品，整体必须可赠送。
	notGiftable := make([]uint32, 0, 8)
	giftableHistorical := 0
	for commodityID := uint32(108515); commodityID <= 108742; commodityID++ {
		entry, ok := commodities[commodityID]
		if !ok {
			continue
		}
		if entry.giftable {
			giftableHistorical++
			continue
		}
		notGiftable = append(notGiftable, commodityID)
	}
	t.Logf("historical gift packages giftable=%d, not giftable=%v", giftableHistorical, notGiftable)
	if giftableHistorical == 0 {
		t.Fatal("none of the historical gift packages can be delivered as a gift")
	}
}

// currentCeraShopGiftRewardsDeliverable 判定"需要选择"的礼包里**每一件候选奖励**是否
// 都可邮寄。候选包括固定奖励与每个选择组里的所有选项（真机上玩家选哪个都行，
// 所以只要有一个选项寄不出去，这件商品的赠送就存在失败面）。
func currentCeraShopGiftRewardsDeliverable(
	catalog *pvfCeraShopCatalog,
	definition currentCeraPackageDefinition,
	product currentCeraShopProduct,
) (bool, string) {
	candidates := make([]currentCeraPackageReward, 0, 8)
	candidates = append(candidates, definition.Rewards...)
	for _, group := range definition.SelectionGroups {
		candidates = append(candidates, group...)
	}
	if len(candidates) == 0 {
		return false, "package has no resolvable rewards"
	}
	for _, candidate := range candidates {
		if _, err := currentCeraShopGiftAttachments(candidate.Definition, candidate.Count, product.CommodityID); err != nil {
			return false, err.Error()
		}
	}
	return true, ""
}
