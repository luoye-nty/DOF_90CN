package dnfbridge

import (
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"

	"longheng.io/server/internal/modules/dnf/purchaselimit"
	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

// reportCeraShopDuplicateCommodities 扫描商品段找出**重复商品号**。
//
// 为什么要单独扫：目录是 `map[uint32]product`，撞号时后一行会**静默覆盖**前一行 ——
// 手加商品行时最常犯的错，而且症状很迷惑（商城少一件、或买到的东西不对）。
func reportCeraShopDuplicateCommodities(t *testing.T, source dnfpvf.Source) {
	t.Helper()
	document, err := parseDungeonCardPVFDocument(source, "etc/cerashop.etc")
	if err != nil {
		t.Logf("重复商品号扫描：读取文档失败 %v", err)
		return
	}
	// (section, rowWidth) 与 newPVFCeraShopCatalog 里的注册保持一致。
	layouts := []struct {
		section string
		width   int
	}{
		{"item", 9}, {"premium", 9}, {"creature", 9}, {"coin", 9}, {"material", 9}, {"recoveryitem", 9},
		{"community item", 9}, {"communityitem", 9}, {"regular item", 9}, {"regularitem", 9},
		{"visual", 8},
		{"package", 11}, {"regular package", 11}, {"regularpackage", 11},
		{"community package", 11}, {"communitypackage", 11},
	}
	seen := map[uint32]string{}
	duplicates := 0
	for _, layout := range layouts {
		tokens, found := document.Section(layout.section)
		if !found {
			continue
		}
		values := currentCeraShopPVFValues(tokens)
		if len(values)%layout.width != 0 {
			t.Logf("⚠ [%s] 令牌数=%d 不是行宽 %d 的整数倍 —— **整个商城目录会构造失败**",
				layout.section, len(values), layout.width)
			continue
		}
		for offset := 0; offset < len(values); offset += layout.width {
			commodityID, ok := currentCeraShopPVFUint32(values[offset])
			if !ok || commodityID == 0 {
				continue
			}
			where := "[" + layout.section + "]"
			if previous, exists := seen[commodityID]; exists {
				t.Logf("⚠ 重复商品号 %d：%s 与 %s 撞号（后者覆盖前者，商城里会少一件）",
					commodityID, previous, where)
				duplicates++
				continue
			}
			seen[commodityID] = where
		}
	}
	if duplicates == 0 {
		t.Logf("重复商品号检查：无（共 %d 个商品号）", len(seen))
	}
}

// dumpCeraShopCommodityTable 把**服务端解析出来的**全部商品写成一个可 diff 的表。
//
// 用途：比较两份 PVF 到底差哪些商品。用正则去猜商城表很不靠谱（同一行里好几个数字、
// 名字里有反引号、各段行宽还不同），直接用解析结果才作数。
//
//	DNFBRIDGE_CERASHOP_DIAG_DUMP=<输出文件>
func dumpCeraShopCommodityTable(t *testing.T, catalog *pvfCeraShopCatalog) {
	t.Helper()
	out := strings.TrimSpace(os.Getenv("DNFBRIDGE_CERASHOP_DIAG_DUMP"))
	if out == "" {
		return
	}
	ids := make([]uint32, 0, len(catalog.products))
	for id := range catalog.products {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	lines := make([]string, 0, len(ids))
	for _, id := range ids {
		product := catalog.products[id]
		lines = append(lines, fmt.Sprintf("%d\t%s\t%d\t%d\t%d\t%d\t%d",
			id, product.Section, product.ItemID, product.Count,
			product.CeraPrice, product.Marker, product.IPGProductID))
	}
	if err := os.WriteFile(out, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatalf("写入商品表 dump 失败: %v", err)
	}
	t.Logf("已 dump %d 个商品到 %s", len(lines), out)
}

// TestDiagnoseCeraShopCommodityLimits 是排查工具：用**服务端自己的策略推导函数**，
// 对给定 PVF 逐商品打印"服务端会把它判成什么限购、来源是哪一段"。
//
// 用法：
//
//	$env:DNFBRIDGE_CERASHOP_DIAG_PVF = "runtime\data\dnf\Script.pvf"
//	$env:DNFBRIDGE_CERASHOP_DIAG_COMMODITIES = "292740,292741,108743,108744"
//	go test ./internal/services/dnfbridge/ -run TestDiagnoseCeraShopCommodityLimits -v
//
// 为什么不写成一个独立命令：限购策略推导（`[buy restrict item]` / 道具 `[purchase limit]` /
// 说明文案 / Marker==101 兜底）全是包内未导出函数，只有包内测试能原样调用，
// 手抄一份逻辑去诊断等于诊断了个假的。
func TestDiagnoseCeraShopCommodityLimits(t *testing.T) {
	pvfPath := strings.TrimSpace(os.Getenv("DNFBRIDGE_CERASHOP_DIAG_PVF"))
	if pvfPath == "" {
		t.Skip("set DNFBRIDGE_CERASHOP_DIAG_PVF to a Script.pvf to inspect")
	}
	rawIDs := strings.TrimSpace(os.Getenv("DNFBRIDGE_CERASHOP_DIAG_COMMODITIES"))
	if rawIDs == "" {
		t.Skip("set DNFBRIDGE_CERASHOP_DIAG_COMMODITIES to a comma-separated commodity list")
	}

	archive, err := platformpvf.Open(pvfPath)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := newPVFCeraShopCatalog(archive)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("PVF=%s", pvfPath)
	t.Logf("商品总数=%d  多买多送白名单=%d 个商品号  最高档=%d",
		len(catalog.products), len(catalog.buyCountGift.commodities), catalog.buyCountGift.MaxTier())
	t.Logf("`[buy restrict item]` 行数=%d", len(catalog.buyRestrict))
	reportCeraShopDuplicateCommodities(t, archive)
	dumpCeraShopCommodityTable(t, catalog)

	for _, raw := range strings.Split(rawIDs, ",") {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		id, err := strconv.ParseUint(raw, 10, 32)
		if err != nil {
			t.Errorf("商品号 %q 解析失败: %v", raw, err)
			continue
		}
		commodityID := uint32(id)

		product, found := catalog.Product(commodityID)
		if !found {
			t.Logf("── 商品 %d：**不在商品表里**（客户端点了会买不到/看不到）", commodityID)
			continue
		}
		t.Logf("── 商品 %d：段=[%s] 道具=%d 数量=%d 点券价=%d Marker=%d IPG=%d",
			commodityID, product.Section, product.ItemID, product.Count,
			product.CeraPrice, product.Marker, product.IPGProductID)
		t.Logf("    多买多送是否计数=%t", catalog.BuyCountGiftEligible(commodityID))

		// 1) 商城限购三来源（互斥，按优先级）
		policies := catalog.BuyRestrictPolicies(commodityID)
		if len(policies) == 0 {
			t.Logf("    商城限购：无")
		}
		for _, policy := range policies {
			t.Logf("    商城限购：来源=[%s] 维度=%s 周期=%s 上限=%d 键=%s",
				policy.Section, policy.Scope, policy.Period, policy.Limit, purchaselimit.CountKey(policy))
		}

		// 2) 道具自己的 `[purchase limit]`
		definition, resolveErr := catalog.items.ResolveItem(product.ItemID)
		if resolveErr != nil {
			t.Logf("    道具解析失败：%v（商品会买不了）", resolveErr)
			continue
		}
		t.Logf("    道具：%s  PVF=%s", definition.Name, definition.PVFPath)
		if definition.PurchaseLimitFound {
			if policy, ok := currentPurchaseLimitPolicy(definition); ok {
				t.Logf("    道具限购（[purchase limit]）：维度=%s 周期=%s 上限=%d",
					policy.Scope, policy.Period, policy.Limit)
			} else {
				t.Logf("    道具限购：**有一行 [purchase limit] 但推导不出合法策略**（会按不限购处理）")
			}
		} else {
			t.Logf("    道具限购：无 `[purchase limit]` 字段")
		}
		if explain := strings.TrimSpace(definition.Explain); explain != "" && len(explain) < 200 {
			if limit, ok := currentCeraShopExplainAccountLimit(explain); ok {
				t.Logf("    说明文案里的账号限购：命中，上限=%d（文案：%s）", limit, explain)
			}
		}
	}
}
