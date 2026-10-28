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

// currentNPCShopOwners 扫描 npc/npc.lst 下所有 NPC 文档，返回 shop id → NPC 名字（含 npc id）。
// NPC 文档里的角色段写作：`[role] `[item shop]` 1277 ... [/role]`，中文名在 [name] 段。
func currentNPCShopOwners(source dnfpvf.Source) map[uint32][]string {
	owners := make(map[uint32][]string)
	entries, _, err := loadDungeonDropList(source, currentNPCShopOwnerListPath)
	if err != nil {
		return owners
	}
	for npcID, listedPath := range entries {
		documentPath, text, readErr := readDungeonDropText(source, "npc", listedPath)
		if readErr != nil {
			continue
		}
		document, parseErr := dnfpvf.Parse(documentPath, text)
		if parseErr != nil {
			continue
		}
		name := ""
		if value, found := document.Text("name"); found {
			name = strings.TrimSpace(value)
		}
		for index, token := range document.Tokens {
			if token.Kind != dnfpvf.TokenString || strings.ToLower(strings.TrimSpace(token.Value)) != "[item shop]" {
				continue
			}
			if index+1 >= len(document.Tokens) || document.Tokens[index+1].Kind != dnfpvf.TokenInt {
				continue
			}
			shopID := document.Tokens[index+1].Int
			if shopID <= 0 {
				continue
			}
			owners[uint32(shopID)] = append(owners[uint32(shopID)], fmt.Sprintf("%s(npc=%d)", name, npcID))
		}
	}
	return owners
}

const currentNPCShopOwnerListPath = "npc/npc.lst"

// 生成“哪些商店有角色限购 / 每日限购”的清单，便于真机验证时挑商店。
//
//	$env:DNFBRIDGE_REAL_PVF_SMOKE='D:\test\地下城与勇士\Script.pvf'
//	$env:DNFBRIDGE_SHOP_LIMIT_REPORT='D:\...\限购商店清单.md'
//	go test ./internal/services/dnfbridge/ -run TestRealPVFShopLimitReport -count=1 -v
func TestRealPVFShopLimitReport(t *testing.T) {
	pvfPath := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE to the runtime Script.pvf")
	}
	archive, err := platformpvf.Open(pvfPath)
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
	owners := currentNPCShopOwners(archive)
	t.Logf("npc owners: %d shops have an [item shop] owner", len(owners))

	type shopRow struct {
		shopID    uint32
		path      string
		items     int
		character int
		daily     int
		samples   []string
	}
	rows := make([]shopRow, 0, len(shop.shopItems))
	for shopID, itemIDs := range shop.shopItems {
		row := shopRow{shopID: shopID, path: shop.shopPaths[shopID], items: len(itemIDs)}
		for _, itemID := range itemIDs {
			definition, resolveErr := items.ResolveItem(itemID)
			if resolveErr != nil || !definition.PurchaseLimitFound {
				continue
			}
			policy := definition.PurchaseLimit
			if policy.Scope == purchaselimit.ScopeCharacter {
				row.character++
			}
			if policy.Period == purchaselimit.PeriodDaily {
				row.daily++
			}
			if len(row.samples) < 6 && (policy.Scope == purchaselimit.ScopeCharacter || policy.Period == purchaselimit.PeriodDaily) {
				row.samples = append(row.samples, fmt.Sprintf("%d %s (%s %s %d)", itemID, definition.Name, policy.Scope, policy.Period, policy.Limit))
			}
		}
		if row.character > 0 || row.daily > 0 {
			rows = append(rows, row)
		}
	}
	sort.Slice(rows, func(left, right int) bool { return rows[left].shopID < rows[right].shopID })

	var builder strings.Builder
	builder.WriteString("# NPC 商店限购清单（角色限购 / 每日限购）\n\n")
	builder.WriteString("来源：`TestRealPVFShopLimitReport` 扫描 itemshop.lst 全部商店 + 道具 PVF 限购字段 + npc/npc.lst 反查 NPC 名。\n")
	builder.WriteString("说明：**商店 ID = itemshop.lst 的键**，也是购买包里的 shop context；op715 请求里那个 4 字节是 NPC id（`npc/npc.lst`）。\n\n")
	builder.WriteString("| 商店ID | 商店文件 | 找哪个NPC（npc id） | 商品数 | 角色限购 | 每日限购 | 样例 |\n|---|---|---|---|---|---|---|\n")
	for _, row := range rows {
		owner := "-"
		if names := owners[row.shopID]; len(names) > 0 {
			owner = strings.Join(names, "、")
		}
		builder.WriteString(fmt.Sprintf("| %d | `%s` | %s | %d | %d | %d | %s |\n",
			row.shopID, row.path, owner, row.items, row.character, row.daily, strings.Join(row.samples, "；")))
	}
	characterShops, dailyShops := 0, 0
	for _, row := range rows {
		if row.character > 0 {
			characterShops++
		}
		if row.daily > 0 {
			dailyShops++
		}
	}
	builder.WriteString(fmt.Sprintf("\n合计：有角色限购的商店 %d 个，有每日限购的商店 %d 个，合计列出 %d 个商店（全部已加载商店 %d 个）。\n",
		characterShops, dailyShops, len(rows), len(shop.shopItems)))

	report := os.Getenv("DNFBRIDGE_SHOP_LIMIT_REPORT")
	if report == "" {
		report = "限购商店清单.md"
	}
	if err := os.WriteFile(report, []byte(builder.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("report written: %s (character_shops=%d daily_shops=%d rows=%d)", report, characterShops, dailyShops, len(rows))
	for _, row := range rows {
		if row.character == 0 {
			continue
		}
		t.Logf("  角色限购 shop=%d %s items=%d character=%d daily=%d samples=%v",
			row.shopID, row.path, row.items, row.character, row.daily, row.samples)
	}
}

// 诊断用：列出商店里带限购字段的商品与策略，方便挑“每天/每周”限购的商品做真机验证。
//
//	$env:DNF90_FORCE_CONTROL_BUILD=1
//	$env:DNFBRIDGE_REAL_PVF_SMOKE='D:\test\地下城与勇士\Script.pvf'
//	$env:DNFBRIDGE_SHOP_LIMIT_AUDIT='1277'      # 只看某个商店（缺省看前 12 个有货的商店）
//	go test ./internal/services/dnfbridge/ -run TestRealPVFShopLimitedItems -count=1 -v
func TestRealPVFShopLimitedItems(t *testing.T) {
	pvfPath := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE to the runtime Script.pvf")
	}
	archive, err := platformpvf.Open(pvfPath)
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
	wanted := strings.TrimSpace(os.Getenv("DNFBRIDGE_SHOP_LIMIT_AUDIT"))
	periodFilter := strings.TrimSpace(os.Getenv("DNFBRIDGE_SHOP_LIMIT_PERIOD"))
	shopIDs := make([]uint32, 0, len(shop.shopItems))
	for shopID := range shop.shopItems {
		shopIDs = append(shopIDs, shopID)
	}
	sort.Slice(shopIDs, func(left, right int) bool { return shopIDs[left] < shopIDs[right] })
	printed := 0
	for _, shopID := range shopIDs {
		if wanted != "" && strconv.FormatUint(uint64(shopID), 10) != wanted {
			continue
		}
		entries := make([]string, 0, 8)
		for index, itemID := range shop.shopItems[shopID] {
			definition, resolveErr := items.ResolveItem(itemID)
			if resolveErr != nil || !definition.PurchaseLimitFound {
				continue
			}
			policy := definition.PurchaseLimit
			if periodFilter != "" && policy.Period.String() != periodFilter {
				continue
			}
			entries = append(entries, fmt.Sprintf("slot=%d item=%d scope=%s period=%s limit=%d name=%q path=%s",
				index, itemID, policy.Scope, policy.Period, policy.Limit, definition.Name, definition.PVFPath))
		}
		if len(entries) == 0 {
			continue
		}
		printed++
		t.Logf("shop=%d items=%d limited=%d", shopID, len(shop.shopItems[shopID]), len(entries))
		for _, entry := range entries {
			t.Logf("   %s", entry)
		}
		if wanted == "" && printed >= 12 {
			t.Logf("... 只打印前 12 个商店；设 DNFBRIDGE_SHOP_LIMIT_AUDIT=<shopID> 只看某个商店")
			break
		}
	}
	if printed == 0 {
		t.Fatalf("no limited items found (wanted=%q shops=%d)", wanted, len(shopIDs))
	}
}

// 诊断用：打印某件道具的限购策略、计数键与落库位置（配合 SQL 改窗口起点做到期验证）。
//
//	$env:DNFBRIDGE_ITEM_LIMIT_AUDIT='490004806'
func TestRealPVFItemPurchaseLimitPolicy(t *testing.T) {
	pvfPath := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE to the runtime Script.pvf")
	}
	itemID := uint32(490004806)
	if raw := strings.TrimSpace(os.Getenv("DNFBRIDGE_ITEM_LIMIT_AUDIT")); raw != "" {
		parsed, parseErr := strconv.ParseUint(raw, 10, 32)
		if parseErr != nil {
			t.Fatal(parseErr)
		}
		itemID = uint32(parsed)
	}
	archive, err := platformpvf.Open(pvfPath)
	if err != nil {
		t.Fatal(err)
	}
	items, err := newPVFDungeonDropCatalog(archive)
	if err != nil {
		t.Fatal(err)
	}
	definition, err := items.ResolveItem(itemID)
	if err != nil {
		t.Fatal(err)
	}
	if !definition.PurchaseLimitFound {
		t.Fatalf("item=%d path=%s has no purchase limit", itemID, definition.PVFPath)
	}
	policy := definition.PurchaseLimit
	container := "dnf_character_stats.stat_value (character_id/stat_key)"
	if policy.Scope.String() == "account" {
		container = "dnf_account_metadata.entry_value (account_id/entry_key)"
	}
	t.Logf("item=%d name=%q path=%s", itemID, definition.Name, definition.PVFPath)
	t.Logf("   section=%s scope=%s period=%s limit=%d", policy.Section, policy.Scope, policy.Period, policy.Limit)
	t.Logf("   count_key=%s", purchaselimit.CountKey(policy))
	t.Logf("   window_key=%s", purchaselimit.WindowKey(policy))
	t.Logf("   container=%s", container)
}
