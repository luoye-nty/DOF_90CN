package dnfbridge

import (
	"os"
	"strings"
	"testing"

	platformpvf "longheng.io/server/internal/platform/pvf"
)

// TestCurrentWornUpgradeAssistRealPVFResolvesTheWornPet 是这次线上无效的**回归锁**。
//
// 现场 2026-10-03 第二轮：日志显示宠物确实被认成出战（pet_item_id=400990076），
// 两个字段却都是 0。根因不在"id 找不到"，而在**拿到路径之后怎么读文本**：
//
//	equipment/equipment.lst 原文 : 400990076 `creature/hilda/hilda.equ`
//	归档里的真名                 : equipment/creature/hilda/hilda.equ
//
// equipment.lst 里的路径是相对 equipment/ 记的，直接 ReadText 一定 not found；
// 普通装备写的本来就是绝对路径，所以只有宠物这类中招。合成夹具当时两边都手写成带
// equipment/ 前缀，因此测试全绿、线上无效 —— 只有拿真 PVF 跑才拦得住。
//
// Run with:
//
//	DNFBRIDGE_REAL_PVF_SMOKE=<Script.pvf> go test ./internal/services/dnfbridge \
//	    -run TestCurrentWornUpgradeAssistRealPVF -v
func TestCurrentWornUpgradeAssistRealPVFResolvesTheWornPet(t *testing.T) {
	pvfPath := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE to validate the worn-pet upgrade assist")
	}
	archive, err := platformpvf.LoadArchive(platformpvf.Options{Path: pvfPath})
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := newPVFDungeonDropCatalog(archive)
	if err != nil {
		t.Fatal(err)
	}
	service := &Service{pvfItemCatalog: catalog}

	// 服主那只：400990076 = equipment/creature/hilda/hilda.equ。
	const petItemID = 400990076
	item := service.currentPVFUpgradeAssist(catalog, petItemID)
	if item.Err != "" {
		t.Fatalf("hilda did not resolve: %+v", item)
	}
	if item.ListedPath != "creature/hilda/hilda.equ" {
		t.Errorf("listed path=%q; the lst spelling changed, re-check the relative-path fix", item.ListedPath)
	}
	if item.ArchivePath != "equipment/creature/hilda/hilda.equ" {
		t.Errorf("archive path=%q want equipment/creature/hilda/hilda.equ", item.ArchivePath)
	}
	// 服主截图口径：[upgrade cost discount] 4、[increase upgrade compound rate] 400。
	// 这两个值一旦读错，客户端的 263002 就对不上服务端扣的钱。
	if item.Discount != 4 || item.Compound != 400 {
		t.Fatalf("hilda fields=%d/%d want 4/400 (text=%d bytes)", item.Discount, item.Compound, item.TextBytes)
	}

	// 全库普查：[increase upgrade compound rate] 只该出现在少数几件上（宠物 + 称号）。
	// 数量级一旦暴涨成"随便一件装备都带"，说明路径解析把**别的文档**读进来了。
	withCompoundRate := 0
	for itemID := range catalog.itemRefs {
		if resolved := service.currentPVFUpgradeAssist(catalog, int64(itemID)); resolved.Compound > 0 {
			withCompoundRate++
		}
	}
	if withCompoundRate == 0 {
		t.Fatal("no worn-able item carries [increase upgrade compound rate] - parsing is broken")
	}
	if withCompoundRate > 64 {
		t.Fatalf("%d items carry [increase upgrade compound rate]; expected a handful", withCompoundRate)
	}

	// 相对路径不是宠物独有的写法：抽查一批"lst 路径不带 equipment/ 前缀"的**可穿戴
	// .equ**（强化助力只会看到穿在槽里的 .equ，宠物也是 .equ），比较**裸 ReadText**
	// 与**解析后 readDungeonDropText** 各能读到多少 —— 改回裸 ReadText 这里必红。
	//
	// 只抽 .equ：equipment.lst 里还混着少量跨表引用（`cash/chn_*.stk`、
	// `10101001/*.stk`、`recipe/nrcp_*.stk` 这类实际住在 stackable/ 下的可堆叠物），
	// 它们本来就不穿在身上，读不到属于 PVF 自身的历史遗留，不该由这条链兜住。
	relative, rawReadable, resolvedReadable := 0, 0, 0
	misses := make([]string, 0, 16)
	for itemID := range catalog.itemRefs {
		listed, found := catalog.itemRefs[itemID]
		if !found {
			continue
		}
		path := cleanDungeonDropPath(listed.path)
		if path == "" || strings.HasPrefix(strings.ToLower(path), "equipment/") {
			continue
		}
		if !strings.EqualFold(path[strings.LastIndex(path, ".")+1:], "equ") {
			continue
		}
		relative++
		if _, err := catalog.source.ReadText(path); err == nil {
			rawReadable++
		}
		resolved := service.currentPVFUpgradeAssist(catalog, int64(itemID))
		if resolved.TextBytes > 0 {
			resolvedReadable++
		} else {
			misses = append(misses, resolved.ListedPath)
		}
		if relative >= 20000 {
			break
		}
	}
	shown := misses
	if len(shown) > 8 {
		shown = shown[:8]
	}
	t.Logf("relative-spelled .equ entries=%d raw-ReadText reads=%d resolved reads=%d unresolved=%d %v",
		relative, rawReadable, resolvedReadable, len(misses), shown)
	if relative == 0 {
		t.Fatal("no relative-spelled equipment entry found - the lst layout changed")
	}
	if rawReadable != 0 {
		t.Fatalf("%d/%d relative paths already read raw; the sample no longer proves the relative-path fix",
			rawReadable, relative)
	}
	// 解析链必须吃掉绝大多数：裸读是 0，解析后应当 95%+。
	if resolvedReadable*100 < relative*95 {
		t.Fatalf("only %d/%d relative .equ paths read after resolution (first misses: %v)",
			resolvedReadable, relative, shown)
	}
	// 剩下的必须**全是配方书**（`recipe/` 那一类 .equ 实际住在 stackable/ 下，
	// 穿不到身上，读不到是 PVF 自身的历史遗留）。一旦出现别的落差，说明解析链漏了
	// 一整类真实装备，那就得再补规则而不是放宽这条断言。
	for _, miss := range misses {
		if !strings.Contains(strings.ToLower(miss), "recipe") {
			t.Fatalf("a non-recipe relative .equ did not resolve: %s", miss)
		}
	}
}
