package dnfbridge

import (
	"os"
	"sort"
	"testing"

	platformpvf "longheng.io/server/internal/platform/pvf"
)

// TestCurrentNPCShopMixedGoldAndMaterialInventoryRealPVF 用真 PVF 枚举所有 NPC 商店商品，
// 按"[price] + [need material]"两个字段把它们分成三类，并打印混合类的样本。
//
// 存在意义：2026-10-08 的线上问题（3042 连买 7 次金币一分不扣，gold_after 恒定 49355535）
// 根因是"只要带 [need material] 就当成纯材料兑换"。这个扫描回答"到底影响多少商品"，
// 也顺手断言 3042 仍然同时标价与收材料 —— 如果哪天 PVF 改了这两个字段，测试会先说话。
//
// 需要真 PVF：DNFBRIDGE_REAL_PVF_SMOKE=<Script.pvf 路径>
func TestCurrentNPCShopMixedGoldAndMaterialInventoryRealPVF(t *testing.T) {
	pvfPath := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE to scan the real NPC shop pricing")
	}
	archive, err := platformpvf.LoadArchive(platformpvf.Options{Path: pvfPath})
	if err != nil {
		t.Fatal(err)
	}
	shop, err := newCurrentNPCShopCatalog(archive)
	if err != nil {
		t.Fatal(err)
	}
	items, err := newPVFDungeonDropCatalog(archive)
	if err != nil {
		t.Fatal(err)
	}

	goldOnly, materialOnly, mixed := 0, 0, 0
	type sample struct {
		itemID        uint32
		price         int64
		materialItem  uint32
		materialCount int64
		path          string
	}
	var samples []sample
	for itemID := range shop.buyableItems {
		pricing, resolveErr := resolveCurrentNPCShopPricing(shop, items, itemID)
		if resolveErr != nil {
			continue
		}
		switch {
		case pricing.MaterialExchange && pricing.RequiresGold:
			mixed++
			samples = append(samples, sample{
				itemID:        itemID,
				price:         pricing.BuyGold,
				materialItem:  pricing.NeedMaterialItem,
				materialCount: pricing.NeedMaterialCount,
				path:          pricing.Definition.PVFPath,
			})
		case pricing.MaterialExchange:
			materialOnly++
		case pricing.BuyGold > 0:
			goldOnly++
		}
	}
	sort.Slice(samples, func(i, j int) bool { return samples[i].itemID < samples[j].itemID })
	t.Logf("NPC 商店可购商品：纯金币=%d 纯材料=%d **金币+材料混合=%d**", goldOnly, materialOnly, mixed)
	limit := len(samples)
	if limit > 20 {
		limit = 20
	}
	for _, s := range samples[:limit] {
		t.Logf("  mixed item=%d price=%d + need material %d x%d  (%s)", s.itemID, s.price, s.materialItem, s.materialCount, s.path)
	}

	// 用户报的那个商品必须仍然被识别成"混合"，否则这条回归线就断了。
	found := false
	for _, s := range samples {
		if s.itemID == 3042 {
			found = true
			if s.price <= 0 || s.materialItem == 0 || s.materialCount <= 0 {
				t.Fatalf("item 3042 mixed pricing=%+v want price>0 and material>0", s)
			}
			if s.materialItem != 3037 || s.materialCount != 100 {
				t.Logf("item 3042 material changed: %d x%d (expected 3037 x100 in the 2026-10-08 report)", s.materialItem, s.materialCount)
			}
		}
	}
	if !found {
		t.Fatalf("item 3042 不再是 [price]+[need material] 混合商品 —— PVF 变了，回归线需要重新确认")
	}
}
