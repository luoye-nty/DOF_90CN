package dnfbridge

import (
	"os"
	"sort"
	"strings"
	"testing"

	platformpvf "longheng.io/server/internal/platform/pvf"
)

// TestCurrentAuctionCategoryRealPVFPartitionsEveryEligibleItem is the audit that
// keeps the category table honest against a real Script.pvf:
//
//  1. every auction-eligible item belongs to exactly one top-level tab (so no item
//     is unreachable and no item is offered under two tabs),
//  2. every pinned node matches at least one item (a node that can never match is a
//     bug, not a category),
//  3. sibling nodes never share an item,
//  4. names inside a leaf really carry the leaf's label (a swapped weapon/armor
//     directory would pass 1-3 and fail here).
//
// Run with:
//
//	DNFBRIDGE_REAL_PVF_SMOKE=<Script.pvf> go test ./internal/services/dnfbridge \
//	    -run TestCurrentAuctionCategoryRealPVF -v
func TestCurrentAuctionCategoryRealPVFPartitionsEveryEligibleItem(t *testing.T) {
	catalog := currentAuctionCategoryRealPVFCatalog(t)

	eligible := make([]uint32, 0, 4096)
	definitions := map[uint32]dungeonDropItemDefinition{}
	ids := make([]uint32, 0, len(catalog.itemRefs))
	for itemID := range catalog.itemRefs {
		ids = append(ids, itemID)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	for _, itemID := range ids {
		definition, err := catalog.ResolveItem(itemID)
		if err != nil {
			continue
		}
		if (definition.Kind == dungeonDropItemStackable || definition.Kind == dungeonDropItemEquipment) &&
			currentDungeonDiscardTradeable(definition) &&
			(!definition.AuctionRuleFound || definition.AuctionAllowed) {
			eligible = append(eligible, itemID)
			definitions[itemID] = definition
		}
	}
	if len(eligible) < 1000 {
		t.Fatalf("eligible items=%d, the audit needs a real catalogue", len(eligible))
	}
	t.Logf("eligible items=%d", len(eligible))

	// 1 + 2: exactly one top-level tab per item, and no dead node.
	nodeCounts := map[uint16]int{}
	for _, itemID := range eligible {
		definition := definitions[itemID]
		matched := uint16(0)
		for _, tab := range currentAuctionCategory.topLevel {
			if currentAuctionCategoryItemMatches(tab, definition) {
				if matched != 0 {
					t.Fatalf("item %d (%s) matches both tab %d and tab %d",
						itemID, definition.PVFPath, matched, tab)
				}
				matched = tab
			}
		}
		if matched == 0 {
			t.Fatalf("item %d (%s %s) matches no top-level tab",
				itemID, definition.EquipmentType+definition.StackableType, definition.PVFPath)
		}
		nodeCounts[matched]++
	}
	for _, tab := range currentAuctionCategory.topLevel {
		if nodeCounts[tab] == 0 {
			t.Fatalf("top-level node %d (%s) holds no eligible item",
				tab, currentAuctionCategory.nodes[tab].label)
		}
		t.Logf("tab %-6d %-8s items=%d", tab, currentAuctionCategory.nodes[tab].label, nodeCounts[tab])
	}

	// 2 (continued): every pinned node must be able to match something.
	for _, code := range currentAuctionCategory.allCodes {
		node := currentAuctionCategory.nodes[code]
		if node.kind == currentAuctionCategoryUnpinned {
			continue
		}
		matches := 0
		for _, itemID := range eligible {
			if currentAuctionCategoryItemMatches(code, definitions[itemID]) {
				matches++
			}
		}
		if matches == 0 {
			t.Fatalf("pinned node %d (%s) matches no eligible item", code, node.label)
		}
	}

	// 3: siblings never share an item.
	for code, node := range currentAuctionCategory.nodes {
		if len(node.children) < 2 {
			continue
		}
		counts := map[uint16]int{}
		for _, itemID := range eligible {
			definition := definitions[itemID]
			for _, child := range node.children {
				if child == code {
					continue
				}
				if currentAuctionCategoryItemMatches(child, definition) {
					counts[child]++
					if counts[child] > 1 {
						// Only a duplicate inside the same child would be a bug; the
						// interesting case is one item in two children.
					}
				}
			}
			matchedChildren := []uint16{}
			for _, child := range node.children {
				if currentAuctionCategoryItemMatches(child, definition) {
					matchedChildren = append(matchedChildren, child)
				}
			}
			if len(matchedChildren) > 1 {
				t.Fatalf("item %d (%s) lands in siblings %v of node %d",
					itemID, definition.PVFPath, matchedChildren, code)
			}
		}
	}
}

// TestCurrentAuctionCategoryRealPVFNamesMatchTheirLabels spot-checks that the PVF
// directory behind a leaf really is the thing the client labels that leaf with. A
// swapped mapping (say katana for ssword) keeps every structural property above and
// only this test notices.
func TestCurrentAuctionCategoryRealPVFNamesMatchTheirLabels(t *testing.T) {
	catalog := currentAuctionCategoryRealPVFCatalog(t)
	type spotCheck struct {
		code    uint16
		label   string
		allowed []string
	}
	// Expectations are read off the real catalogue (tools/_auction_leaf_names.txt),
	// not guessed: several PVF directories are named differently from the Chinese
	// items inside them (knuckle holds 「手套」, boxglove holds 「拳套」, rod holds
	// 「魔杖」, pole holds 「长棍」), so the allowed substrings follow the item names the
	// player reads under that tab.
	checks := []spotCheck{
		{10102, "短剑", []string{"剑"}},
		{10103, "太刀", []string{"刀"}},
		{10104, "钝器", []string{"棒"}},
		{10105, "巨剑", []string{"剑"}},
		{10106, "光剑", []string{"光剑", "荧光", "莹光", "极光"}},
		{10202, "手套", []string{"手套"}},
		{10203, "臂铠", []string{"臂铠"}},
		{10204, "爪", []string{"爪"}},
		{10205, "拳套", []string{"拳套"}},
		{10206, "东方棍", []string{"东方棍"}},
		{10302, "左轮枪", []string{"左轮"}},
		{10303, "自动手枪", []string{"手枪"}},
		{10304, "步枪", []string{"步枪", "燧发枪", "火绳枪"}},
		{10305, "手炮", []string{"炮"}},
		{10306, "手弩", []string{"弩"}},
		{10402, "矛", []string{"矛"}},
		{10403, "棍棒", []string{"棍", "棒"}},
		{10404, "魔杖", []string{"魔杖"}},
		{10405, "法杖", []string{"法杖"}},
		{10406, "扫把", []string{"扫把"}},
		{10502, "十字架", []string{"十字架"}},
		{10503, "念珠", []string{"念珠"}},
		{10504, "图腾", []string{"图腾"}},
		{10505, "镰刀", []string{"镰"}},
		{10506, "战斧", []string{"斧"}},
		{10602, "匕首", []string{"匕首"}},
		{10603, "双剑", []string{"双剑"}},
		{10604, "手杖", []string{"手杖"}},
		{10605, "苦无", []string{"苦无"}},
		{10802, "长枪", []string{"长枪", "枪"}},
		{10803, "战戟", []string{"战戟"}},
		{10804, "光枪", []string{"光枪"}},
		{10805, "暗矛", []string{"暗矛", "矛"}},
		{10902, "长刀", []string{"刀"}},
		{10903, "小太刀", []string{"小太刀", "短刃", "环刀", "云刀", "刀"}},
		{10904, "重剑", []string{"重剑", "剑", "刀"}},
		{10905, "源力剑", []string{"剑"}},
		{12001, "项链", []string{"项链", "项圈"}},
		{12002, "戒指", []string{"戒指", "草戒"}},
		{12003, "手镯", []string{"手镯", "护腕"}},
		{32001, "辅助装备", []string{"臂章", "手套"}},
		{32200, "耳环", []string{"耳环", "耳坠"}},
		// Armor materials are keyed by their PVF directories (cloth / leather /
		// larmor / harmor / plate). Only 皮甲 has a name token that discriminates
		// (「皮」); 布甲/轻甲/重甲/板甲 reuse each other's material words (莳芽, 钢铁,
		// 陶瓷…) at higher tiers, so their mapping rests on the directory names
		// (larmor = light armor = 轻甲, harmor = heavy armor = 重甲, plate = 板甲) plus
		// the client's own material order, and the sibling-disjointness check in the
		// partition audit keeps them apart. Their *parts* are name-checked below.
		{11100, "皮甲", []string{"皮"}},
	}
	// The five armor parts repeat inside every material. Higher tiers rename them
	// (harmor tops are 链甲/锁甲, cloth shoes become 短靴), so each part gets the union
	// of the tokens its column prints across all five materials - all read off
	// tools/_auction_leaf_names.txt. The sets stay mutually exclusive across parts,
	// which is what catches a swapped slot.
	armorPartTokens := [5][]string{
		{"衬衫", "背心", "长袍", "胸甲", "链甲", "锁甲", "兜甲", "上衣"}, // 上衣
		{"护肩", "肩甲", "肩"},                    // 头肩
		{"短裤", "长裤", "绑腿", "护腿", "胫甲", "下装"}, // 下装
		{"拖鞋", "凉鞋", "短靴", "长靴", "鞋"},        // 鞋
		{"腰带", "护腰", "腰甲"},                   // 腰带
	}
	for _, material := range []uint16{11001, 11100, 11200, 11300, 11400} {
		labels := []string{"上衣", "头肩", "下装", "鞋", "腰带"}
		for offset, tokens := range armorPartTokens {
			checks = append(checks, spotCheck{material + uint16(offset) + 1, labels[offset], tokens})
		}
	}
	for _, check := range checks {
		names := currentAuctionCategoryEligibleNames(t, catalog, check.code, 40)
		if len(names) == 0 {
			t.Fatalf("node %d (%s) has no eligible item to check", check.code, check.label)
		}
		matched := 0
		for _, name := range names {
			for _, allowed := range check.allowed {
				if strings.Contains(name, allowed) {
					matched++
					break
				}
			}
		}
		// The client's own label must describe the bulk of what a leaf returns; a
		// handful of odd names (enchant variants, sealed copies) is fine.
		if matched*2 < len(names) {
			t.Fatalf("node %d (%s): only %d/%d names look like that label: %v",
				check.code, check.label, matched, len(names), names)
		}
	}
}

// TestCurrentAuctionCategoryRealPVFReportsUnpinnedNodes makes the remaining gaps
// explicit: it fails if a node that used to be unpinned silently became pinned (the
// list must be updated deliberately) and it reports how many items each gap covers
// so the operator can judge the impact.
func TestCurrentAuctionCategoryRealPVFReportsUnpinnedNodes(t *testing.T) {
	catalog := currentAuctionCategoryRealPVFCatalog(t)
	type gap struct {
		code  uint16
		label string
		count int
	}
	gaps := []gap{}
	for _, code := range currentAuctionCategory.unpinned {
		node := currentAuctionCategory.nodes[code]
		parent, _ := currentAuctionCategoryNodeFor(node.parent)
		count := 0
		for itemID := range catalog.itemRefs {
			definition, err := catalog.ResolveItem(itemID)
			if err != nil {
				continue
			}
			if !currentDungeonDiscardTradeable(definition) ||
				(definition.AuctionRuleFound && !definition.AuctionAllowed) {
				continue
			}
			if currentAuctionCategoryItemMatches(node.parent, definition) {
				count++
			}
		}
		gaps = append(gaps, gap{code: code, label: parent.label + " > " + node.label, count: count})
	}
	for _, gap := range gaps {
		t.Logf("unpinned %-6d %-24s parent scope holds %d eligible items", gap.code, gap.label, gap.count)
	}
	if len(gaps) != 72 {
		t.Fatalf("unpinned nodes=%d want=72; update this audit when the list changes", len(gaps))
	}
}

func currentAuctionCategoryRealPVFCatalog(t *testing.T) *pvfDungeonDropCatalog {
	t.Helper()
	pvfPath := strings.TrimSpace(os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE"))
	if pvfPath == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE to audit the auction category table")
	}
	archive, err := platformpvf.LoadArchive(platformpvf.Options{Path: pvfPath})
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := newPVFDungeonDropCatalog(archive)
	if err != nil {
		t.Fatal(err)
	}
	return catalog
}

func currentAuctionCategoryEligibleNames(t *testing.T, catalog *pvfDungeonDropCatalog, code uint16, limit int) []string {
	t.Helper()
	ids := make([]uint32, 0, len(catalog.itemRefs))
	for itemID := range catalog.itemRefs {
		ids = append(ids, itemID)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	names := []string{}
	for _, itemID := range ids {
		if len(names) >= limit {
			break
		}
		definition, err := catalog.ResolveItem(itemID)
		if err != nil || strings.TrimSpace(definition.Name) == "" {
			continue
		}
		if !currentDungeonDiscardTradeable(definition) ||
			(definition.AuctionRuleFound && !definition.AuctionAllowed) {
			continue
		}
		if currentAuctionCategoryItemMatches(code, definition) {
			names = append(names, definition.Name)
		}
	}
	return names
}
