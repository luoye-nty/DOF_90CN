package dnfbridge

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

func newMagicBoxTestCatalog(t *testing.T) (*pvfDungeonDropCatalog, dungeonDropCatalogTestSource) {
	t.Helper()
	source := dungeonDropCatalogTestSource{
		"monster/monster.lst":                         "",
		"equipment/equipment.lst":                     "101600024 `avatar/weapon.equ`\n",
		"stackable/stackable.lst":                     "24 `cash/creature/creature_food.stk`\n10007368 `ect/chn_random/amazingbox.stk`\n10007367 `ect/chn_random/hammer.stk`\n2001 `booster/boosterbox.stk`\n2002 `other/plain.stk`\n2003 `booster/legacy_avatar_box.stk`\n2004 `booster/upgradable_legacy_box.stk`\n2005 `booster/lottery_key_jar.stk`\n2600014 `potion/ptn.stk`\n2682272 `other/reward2682272.stk`\n3000 `other/reward3000.stk`\n3001 `other/reward3001.stk`\n3002 `other/reward3002.stk`\n",
		"stackable/ect/chn_random/amazingbox.stk":     "[name] `Amazing Box`\n[stackable type] `[random upgradable legacy]`\n[RANDOMBOX]\n[int data]\n2600014 2 0 2600014 18000 2 0 2600014 5000 2 0 2600014 500 2 0 2600014 2 2 0\n[/int data]\n[int data]\n2682272 1 0 2682272 50000 1 0\n[/int data]\n[sealing removal item]\n2 10007367 1 10007368 1\n[/sealing removal item]\n[/RANDOMBOX]\n",
		"stackable/ect/chn_random/hammer.stk":         "[name] `Hammer`\n[stackable type] `[material]`\n",
		"stackable/booster/boosterbox.stk":            "[name] `Booster Box`\n[stackable type] `[booster]`\n[booster info]\n[etc]\n2 3001 100 1 3002 50 2\n[/etc]\n[/booster info]\n[need material] `10007367 1`\n",
		"stackable/booster/legacy_avatar_box.stk":     "[name] `Legacy Avatar Box`\n[stackable type] `[booster]`\n[booster info]\n[avatar]\n1 101600024 100000 1 3 2\n[/avatar]\n[/booster info]\n",
		"stackable/booster/upgradable_legacy_box.stk": "[name] `Old Event Box`\n[stackable type] `[upgradable legacy]`\n[int data]\n3000 0 1 3001 217 1 3002 1430 2\n[/int data]\n[need material]\n10007367 1\n[/need material]\n",
		"stackable/booster/lottery_key_jar.stk":       "[name] `Lottery Key Jar`\n[stackable type] `[upgradable legacy]`\n[int data]\n3000 1 1\n[/int data]\n[lottery use need item]\n10007367 1\n[/lottery use need item]\n",
		"stackable/other/plain.stk":                   "[name] `Plain`\n[stackable type] `[material]`\n",
		"stackable/other/reward2682272.stk":           "[name] `Reward 2682272`\n[stackable type] `[material]`\n",
		"stackable/other/reward3000.stk":              "[name] `Reward 3000`\n[stackable type] `[material]`\n",
		"stackable/other/reward3001.stk":              "[name] `Reward 3001`\n[stackable type] `[material]`\n",
		"stackable/other/reward3002.stk":              "[name] `Reward 3002`\n[stackable type] `[material]`\n",
		"stackable/potion/ptn.stk":                    "[name] `Potion`\n[stackable type] `[consumption]`\n[stack limit] 100\n[usable period] 30\n[attach type] `[sealing]`\n",
		"stackable/cash/creature/creature_food.stk":   "[name] `Creature Food`\n[stackable type] `[feed]`\n[stack limit] 1000\n",
		"equipment/avatar/weapon.equ":                 "[name] `Legacy Weapon Avatar`\n[equipment type] `[weapon avatar]` 0\n[attach type] `[trade]`\n",
	}
	catalog, err := newPVFDungeonDropCatalog(source)
	if err != nil {
		t.Fatal(err)
	}
	return catalog, source
}

// [booster info] 里的 `[charactor]` 是按职业分组的（首 token 是职业标签，
// 后面是 minLevel/maxLevel/drawCount，再接普通的 (item, weight, count) 三元组）。
// 解析器必须把职业门读出来，否则开箱时无从过滤 —— 现场就是鬼剑士开豪华婚礼
// 化妆箱把全职业的时装一起领走（2026-10-04）。
func TestParseMagicBoxBoosterCharactorGroupsCarryJobGate(t *testing.T) {
	// 直接解析一段 PVF 文本，避免改动共享 fixture 影响其它用例。
	doc, err := dnfpvf.Parse("box.stk", "[booster info]\n"+
		"[charactor]\n`[swordman]` 1 99 1 3001 100000 1\n[/charactor]\n"+
		"[charactor]\n`[at swordman]` 30 99 2 3002 100000 1\n[/charactor]\n"+
		"[etc]\n1 3000 100000 2\n[/etc]\n"+
		"[/booster info]")
	if err != nil {
		t.Fatal(err)
	}
	groups := parseMagicBoxBoosterGroups(doc)
	if len(groups) != 3 {
		t.Fatalf("分组数=%d，应为 3: %+v", len(groups), groups)
	}
	if groups[0].Job != "[swordman]" || groups[0].MinLevel != 1 || groups[0].MaxLevel != 99 || groups[0].DrawCount != 1 {
		t.Fatalf("第一组职业门=%+v", groups[0])
	}
	if groups[1].Job != "[at swordman]" || groups[1].MinLevel != 30 || groups[1].DrawCount != 2 {
		t.Fatalf("第二组职业门=%+v", groups[1])
	}
	// 普通 [etc] 组不带职业门。
	if groups[2].Job != "" || groups[2].MinLevel != 0 || groups[2].MaxLevel != 0 {
		t.Fatalf("etc 组不应带职业门: %+v", groups[2])
	}
}

func TestResolveCurrentMagicBoxHistoricalAvatarFiveFieldRow(t *testing.T) {
	catalog, source := newMagicBoxTestCatalog(t)
	definition, err := resolveCurrentBoosterDefinition(catalog, 2003, currentBoosterRequestRandom)
	if err != nil {
		t.Fatalf("resolve historical avatar box: %v", err)
	}
	amounts, options, gold, err := resolveCurrentBoosterRewardAmounts(
		definition,
		currentBoosterOpenRequest{Kind: currentBoosterRequestRandom},
		func(limit int64) (int64, error) { return 0, nil },
	)
	if err != nil {
		t.Fatalf("roll historical avatar box: %v", err)
	}
	if len(amounts) != 1 || amounts[101600024] != 1 || amounts[1] != 0 || options[101600024] != 2 || gold != 0 {
		t.Fatalf("historical avatar amounts=%v options=%v gold=%d", amounts, options, gold)
	}
	rewards, err := resolveCurrentBoosterRewards(
		catalog,
		amounts,
		options,
		dnfrepo.ItemStack{ItemID: 2003, Count: 1},
		time.Now().UTC(),
	)
	if err != nil {
		t.Fatalf("resolve historical avatar reward: %v", err)
	}
	if len(rewards) != 1 || rewards[0].Definition.ItemID != 101600024 || rewards[0].Count != 1 || rewards[0].Option != 2 || rewards[0].Placement != currentBoosterRewardAvatar {
		t.Fatalf("historical avatar rewards=%+v", rewards)
	}
	inventory := dnfrepo.InventoryRecord{Slots: make(map[string]dnfrepo.ItemStack)}
	// 24 = 最大扩展，使落点范围不被夹取，等价于本用例原本的落点语义。
	keys, err := grantCurrentBoosterReward(&inventory, source, rewards[0], 24)
	if err != nil {
		t.Fatalf("grant historical avatar reward: %v", err)
	}
	if len(keys) != 1 || keys[0] != "1:0" {
		t.Fatalf("historical avatar keys=%v", keys)
	}
	avatar := inventory.Slots["1:0"]
	if avatar.ItemID != 101600024 || avatar.Count != 1 {
		t.Fatalf("historical avatar stack=%+v", avatar)
	}
}

func TestRealPVFHistoricalAvatarBoosterUsesFiveFieldRow(t *testing.T) {
	pvfPath := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE to verify the historical avatar booster")
	}
	archive, err := platformpvf.LoadArchive(platformpvf.Options{Path: pvfPath})
	if err != nil {
		t.Fatalf("load real Script.pvf: %v", err)
	}
	catalog, err := newPVFDungeonDropCatalog(archive)
	if err != nil {
		t.Fatalf("load real item catalog: %v", err)
	}
	definition, err := resolveCurrentBoosterDefinition(catalog, 10007317, currentBoosterRequestRandom)
	if err != nil {
		t.Fatalf("resolve historical avatar booster 10007317: %v", err)
	}
	if len(definition.Random.Groups) != 1 || definition.Random.Groups[0].DrawCount != 1 || len(definition.Random.Groups[0].Entries) != 1 {
		t.Fatalf("historical avatar booster groups=%+v", definition.Random.Groups)
	}
	entry := definition.Random.Groups[0].Entries[0]
	if entry.ItemID != 101600024 || entry.Weight != 100000 || entry.Count != 1 || entry.Option != 0 {
		t.Fatalf("historical avatar booster entry=%+v", entry)
	}
}

func TestRealPVFUpgradableLegacyGiftBoxUsesNativeMagicBoxPool(t *testing.T) {
	pvfPath := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE to verify the historical upgradable-legacy gift box")
	}
	archive, err := platformpvf.LoadArchive(platformpvf.Options{Path: pvfPath})
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := newPVFDungeonDropCatalog(archive)
	if err != nil {
		t.Fatal(err)
	}
	resolution, err := resolveCurrentMagicBox(catalog, archive, 10002952)
	if err != nil {
		t.Fatal(err)
	}
	if resolution.Kind != "random" || len(resolution.Groups) != 1 || len(resolution.Groups[0].Entries) < 100 {
		t.Fatalf("rainbow gift box resolution=%+v", resolution)
	}
	if resolution.MaterialItemID != 10002941 || resolution.MaterialCountPerUse != 1 {
		t.Fatalf("rainbow gift box material=%d x%d", resolution.MaterialItemID, resolution.MaterialCountPerUse)
	}
}

func TestRealPVFAllNativeLotteryJarPoolsResolve(t *testing.T) {
	pvfPath := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE to audit every native lottery jar")
	}
	archive, err := platformpvf.LoadArchive(platformpvf.Options{Path: pvfPath})
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := newPVFDungeonDropCatalog(archive)
	if err != nil {
		t.Fatal(err)
	}
	jarCount := 0
	rewardCount := 0
	for _, itemID := range catalog.stackableIDs {
		source, resolveErr := catalog.ResolveItem(itemID)
		if resolveErr != nil {
			t.Fatalf("resolve source item=%d: %v", itemID, resolveErr)
		}
		if !isCurrentLotteryJarType(normalizeDungeonDropStackableType(source.StackableType)) {
			continue
		}
		jarCount++
		definition, resolveErr := resolveCurrentBoosterDefinition(catalog, itemID, currentBoosterRequestLottery)
		if resolveErr != nil {
			t.Fatalf("resolve lottery jar item=%d name=%q path=%s: %v", itemID, source.Name, source.PVFPath, resolveErr)
		}
		document, documentErr := parseDungeonCardPVFDocument(catalog.source, source.PVFPath)
		if documentErr != nil {
			t.Fatalf("read lottery jar item=%d path=%s: %v", itemID, source.PVFPath, documentErr)
		}
		openingItemID, openingCount := resolveMagicBoxLotteryOpeningMaterial(document, int64(itemID))
		if definition.MaterialItemID != 0 || definition.MaterialCount != 0 ||
			definition.Random.MaterialItemID != openingItemID || definition.Random.MaterialCountPerUse != openingCount {
			t.Fatalf(
				"lottery jar item=%d name=%q mixed shop/opening material: definition=%d x%d random=%d x%d want sealing=%d x%d",
				itemID,
				source.Name,
				definition.MaterialItemID,
				definition.MaterialCount,
				definition.Random.MaterialItemID,
				definition.Random.MaterialCountPerUse,
				openingItemID,
				openingCount,
			)
		}
		for groupIndex, group := range definition.Random.Groups {
			if len(group.Entries) == 0 {
				t.Fatalf("lottery jar item=%d group=%d is empty", itemID, groupIndex)
			}
			for _, entry := range group.Entries {
				if entry.Gold {
					if entry.ItemID != 0 || entry.Count <= 0 {
						t.Fatalf("lottery jar item=%d has invalid gold reward=%+v", itemID, entry)
					}
					rewardCount++
					continue
				}
				if entry.ItemID <= 0 || entry.ItemID > int64(^uint32(0)) {
					t.Fatalf("lottery jar item=%d has invalid reward=%d", itemID, entry.ItemID)
				}
				if _, rewardErr := catalog.ResolveItem(uint32(entry.ItemID)); rewardErr != nil {
					t.Fatalf("lottery jar item=%d reward=%d is not mapped: %v", itemID, entry.ItemID, rewardErr)
				}
				rewardCount++
			}
		}
	}
	if jarCount == 0 || rewardCount == 0 {
		t.Fatalf("native lottery jar audit jars=%d rewards=%d", jarCount, rewardCount)
	}
	t.Logf("native lottery jar audit jars=%d mapped_reward_rows=%d", jarCount, rewardCount)
}

func TestRealPVFLotteryUseNeedItemJarsResolveOpeningMaterials(t *testing.T) {
	pvfPath := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE to verify lottery opening materials")
	}
	archive, err := platformpvf.LoadArchive(platformpvf.Options{Path: pvfPath})
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := newPVFDungeonDropCatalog(archive)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name     string
		jarID    uint32
		material int64
		count    int64
	}{
		{name: "ancient orc", jarID: 490701821, material: 490701820, count: 1},
		{name: "three kingdoms", jarID: 10008521, material: 10008550, count: 1},
		{name: "proga", jarID: 490700713, material: 490700749, count: 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			definition, err := resolveCurrentBoosterDefinition(catalog, test.jarID, currentBoosterRequestLottery)
			if err != nil {
				t.Fatal(err)
			}
			if definition.Random.MaterialItemID != test.material ||
				definition.Random.MaterialCountPerUse != test.count {
				t.Fatalf(
					"jar=%d material=%d x%d, want %d x%d",
					test.jarID,
					definition.Random.MaterialItemID,
					definition.Random.MaterialCountPerUse,
					test.material,
					test.count,
				)
			}
		})
	}
}

func TestRealPVFAllNamedPocketJarRoutesResolve(t *testing.T) {
	pvfPath := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE to audit every named pocket jar route")
	}
	archive, err := platformpvf.LoadArchive(platformpvf.Options{Path: pvfPath})
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := newPVFDungeonDropCatalog(archive)
	if err != nil {
		t.Fatal(err)
	}
	typeCounts := make(map[string]int)
	unsupported := make([]string, 0)
	routeErrors := make([]string, 0)
	jarCount := 0
	for _, itemID := range catalog.stackableIDs {
		source, resolveErr := catalog.ResolveItem(itemID)
		if resolveErr != nil {
			t.Fatalf("resolve source item=%d: %v", itemID, resolveErr)
		}
		if !strings.Contains(source.Name, "袖珍罐") {
			continue
		}
		jarCount++
		stackableType := normalizeDungeonDropStackableType(source.StackableType)
		typeCounts[stackableType]++
		switch stackableType {
		case "[upgradable legacy]", "[random upgradable legacy]":
			_, resolveErr = resolveCurrentBoosterDefinition(catalog, itemID, currentBoosterRequestLottery)
		case "[booster]", "[cera booster]", "[booster random]":
			_, resolveErr = resolveCurrentBoosterDefinition(catalog, itemID, currentBoosterRequestRandom)
		case "[booster selection]":
			_, resolveErr = resolveCurrentBoosterDefinition(catalog, itemID, currentBoosterRequestSelection)
		default:
			unsupported = append(unsupported, fmt.Sprintf("%d %q type=%q path=%s", itemID, source.Name, stackableType, source.PVFPath))
			continue
		}
		if resolveErr != nil {
			routeErrors = append(routeErrors, fmt.Sprintf("%d %q type=%q path=%s: %v", itemID, source.Name, stackableType, source.PVFPath, resolveErr))
		}
	}
	sort.Strings(unsupported)
	sort.Strings(routeErrors)
	if len(routeErrors) != 0 {
		t.Fatalf("named pocket jars with a broken current route (%d):\n%s", len(routeErrors), strings.Join(routeErrors, "\n"))
	}
	if len(unsupported) != 0 {
		// Name matching also finds fragments, keys, services and twelve
		// retired 2010-2014 [etc]/[legacy] definitions. They have no current
		// opener type and must not be promoted into op27 based on their name.
		t.Logf("named pocket-jar terms without a current opener (%d):\n%s", len(unsupported), strings.Join(unsupported, "\n"))
	}
	if jarCount == 0 {
		t.Fatal("named pocket jar audit found no items")
	}
	t.Logf("named pocket jar audit jars=%d routes=%v", jarCount, typeCounts)
}

func TestResolveCurrentBoosterLotteryIgnoresLegacyShopNeedMaterial(t *testing.T) {
	catalog, _ := newMagicBoxTestCatalog(t)

	lottery, err := resolveCurrentBoosterDefinition(catalog, 2004, currentBoosterRequestLottery)
	if err != nil {
		t.Fatal(err)
	}
	if lottery.MaterialItemID != 0 || lottery.MaterialCount != 0 ||
		lottery.Random.MaterialItemID != 0 || lottery.Random.MaterialCountPerUse != 0 {
		t.Fatalf("lottery shop material leaked into opening cost: %+v", lottery)
	}

	magicBox, err := resolveCurrentMagicBox(catalog, catalog.source, 2004)
	if err != nil {
		t.Fatal(err)
	}
	if magicBox.MaterialItemID != 10007367 || magicBox.MaterialCountPerUse != 1 {
		t.Fatalf("non-lottery magic-box material=%d x%d", magicBox.MaterialItemID, magicBox.MaterialCountPerUse)
	}

	randomLegacy, err := resolveCurrentBoosterDefinition(catalog, 10007368, currentBoosterRequestLottery)
	if err != nil {
		t.Fatal(err)
	}
	if randomLegacy.MaterialItemID != 0 || randomLegacy.MaterialCount != 0 ||
		randomLegacy.Random.MaterialItemID != 10007367 || randomLegacy.Random.MaterialCountPerUse != 1 {
		t.Fatalf("lottery opening key was not preserved: %+v", randomLegacy)
	}
}

func TestResolveCurrentBoosterLotteryUsesLotteryUseNeedItem(t *testing.T) {
	catalog, _ := newMagicBoxTestCatalog(t)

	definition, err := resolveCurrentBoosterDefinition(catalog, 2005, currentBoosterRequestLottery)
	if err != nil {
		t.Fatal(err)
	}
	if definition.Random.MaterialItemID != 10007367 ||
		definition.Random.MaterialCountPerUse != 1 {
		t.Fatalf(
			"lottery opening material=%d x%d, want 10007367 x1",
			definition.Random.MaterialItemID,
			definition.Random.MaterialCountPerUse,
		)
	}
}

func TestResolveCurrentBoosterRandomIgnoresShopNeedMaterial(t *testing.T) {
	catalog, _ := newMagicBoxTestCatalog(t)

	booster, err := resolveCurrentBoosterDefinition(catalog, 2001, currentBoosterRequestRandom)
	if err != nil {
		t.Fatal(err)
	}
	if booster.MaterialItemID != 0 || booster.MaterialCount != 0 ||
		booster.Random.MaterialItemID != 0 || booster.Random.MaterialCountPerUse != 0 {
		t.Fatalf("booster shop material leaked into opening cost: %+v", booster)
	}
}

func TestResolveCurrentMagicBoxPrunesUnlistedLegacyRewardRows(t *testing.T) {
	source := dungeonDropCatalogTestSource{
		"monster/monster.lst":         "",
		"equipment/equipment.lst":     "",
		"stackable/stackable.lst":     "7000 `legacy/jar.stk`\n7001 `legacy/reward.stk`\n",
		"stackable/legacy/jar.stk":    "[name] `Legacy Jar`\n[stackable type] `[upgradable legacy]`\n[int data]\n7001 10 1 7999 1000 1\n[/int data]\n",
		"stackable/legacy/reward.stk": "[name] `Mapped Reward`\n[stackable type] `[material]`\n",
	}
	catalog, err := newPVFDungeonDropCatalog(source)
	if err != nil {
		t.Fatal(err)
	}
	resolution, err := resolveCurrentMagicBox(catalog, source, 7000)
	if err != nil {
		t.Fatal(err)
	}
	if len(resolution.Groups) != 1 || len(resolution.Groups[0].Entries) != 1 || resolution.Groups[0].Entries[0].ItemID != 7001 {
		t.Fatalf("filtered legacy jar=%+v", resolution.Groups)
	}
}

func TestResolveCurrentMagicBoxUpgradableLegacyGoldPseudoReward(t *testing.T) {
	source := dungeonDropCatalogTestSource{
		"monster/monster.lst":          "",
		"equipment/equipment.lst":      "",
		"stackable/stackable.lst":      "7614 `event/gold_jar.stk`\n",
		"stackable/event/gold_jar.stk": "[name] `Gold Jar`\n[stackable type] `[upgradable legacy]`\n[int data]\n0 1 1 0 2 10000\n[/int data]\n",
	}
	catalog, err := newPVFDungeonDropCatalog(source)
	if err != nil {
		t.Fatal(err)
	}
	definition, err := resolveCurrentBoosterDefinition(catalog, 7614, currentBoosterRequestLottery)
	if err != nil {
		t.Fatal(err)
	}
	if len(definition.Random.Groups) != 1 || len(definition.Random.Groups[0].Entries) != 2 || !definition.Random.Groups[0].Entries[0].Gold {
		t.Fatalf("gold jar groups=%+v", definition.Random.Groups)
	}
	amounts, options, gold, err := resolveCurrentBoosterRewardAmounts(
		definition,
		currentBoosterOpenRequest{Kind: currentBoosterRequestLottery},
		func(limit int64) (int64, error) {
			if limit != 3 {
				t.Fatalf("roll limit=%d", limit)
			}
			return 2, nil
		},
	)
	if err != nil || len(amounts) != 0 || len(options) != 0 || gold != 10000 {
		t.Fatalf("gold jar amounts=%v options=%v gold=%d err=%v", amounts, options, gold, err)
	}
}

func TestResolveCurrentMagicBoxPetFeedTargetsPetConsumableSegment(t *testing.T) {
	catalog, source := newMagicBoxTestCatalog(t)
	item, err := resolveCurrentMagicBoxRewardItem(catalog, source, 24)
	if err != nil {
		t.Fatal(err)
	}
	if item.ItemID != 24 || item.TargetListType != currentPetInventoryListType ||
		item.SlotStart != currentCeraShopPetConsumableSlotStart ||
		item.SlotEnd != currentCeraShopPetConsumableSlotEnd ||
		item.StackLimit != 1000 {
		t.Fatalf("pet feed reward=%+v", item)
	}
}

func TestResolveCurrentMagicBoxRandomLegacy(t *testing.T) {
	catalog, source := newMagicBoxTestCatalog(t)
	resolution, err := resolveCurrentMagicBox(catalog, source, 10007368)
	if err != nil {
		t.Fatalf("resolve error = %v", err)
	}
	if resolution.Kind != "random" {
		t.Fatalf("kind = %q, want random", resolution.Kind)
	}
	if len(resolution.Groups) != 2 {
		t.Fatalf("groups = %d, want 2", len(resolution.Groups))
	}
	first := resolution.Groups[0]
	if first.DrawCount != 1 || len(first.Entries) != 4 {
		t.Fatalf("group[0] = %+v, want 4 entries DrawCount 1", first)
	}
	if first.Entries[0].ItemID != 2600014 || first.Entries[0].Weight != 18000 || first.Entries[0].Count != 2 {
		t.Fatalf("group[0] entry[0] = %+v", first.Entries[0])
	}
	if first.Entries[3].Weight != 2 {
		t.Fatalf("group[0] entry[3] = %+v, want weight 2 preserved", first.Entries[3])
	}
	second := resolution.Groups[1]
	// Seven ints: exactly one quad entry after the skipped leading triple.
	if len(second.Entries) != 1 || second.Entries[0].ItemID != 2682272 || second.Entries[0].Weight != 50000 || second.Entries[0].Count != 1 {
		t.Fatalf("group[1] = %+v", second)
	}
	if resolution.MaterialItemID != 10007367 || resolution.MaterialCountPerUse != 1 {
		t.Fatalf("material = %d x%d, want 10007367 x1", resolution.MaterialItemID, resolution.MaterialCountPerUse)
	}
	if resolution.BoxPVFPath != "stackable/ect/chn_random/amazingbox.stk" {
		t.Fatalf("path = %q", resolution.BoxPVFPath)
	}
}

func TestResolveCurrentMagicBoxUpgradableLegacyTopLevelTriples(t *testing.T) {
	catalog, source := newMagicBoxTestCatalog(t)
	resolution, err := resolveCurrentMagicBox(catalog, source, 2004)
	if err != nil {
		t.Fatal(err)
	}
	if resolution.Kind != "random" || len(resolution.Groups) != 1 || resolution.Groups[0].DrawCount != 1 {
		t.Fatalf("resolution=%+v", resolution)
	}
	entries := resolution.Groups[0].Entries
	if len(entries) != 2 || entries[0].ItemID != 3001 || entries[0].Weight != 217 || entries[0].Count != 1 ||
		entries[1].ItemID != 3002 || entries[1].Weight != 1430 || entries[1].Count != 2 {
		t.Fatalf("entries=%+v", entries)
	}
	if resolution.MaterialItemID != 10007367 || resolution.MaterialCountPerUse != 1 {
		t.Fatalf("material=%d x%d", resolution.MaterialItemID, resolution.MaterialCountPerUse)
	}
}

func TestResolveCurrentMagicBoxBoosterFamily(t *testing.T) {
	catalog, source := newMagicBoxTestCatalog(t)
	resolution, err := resolveCurrentMagicBox(catalog, source, 2001)
	if err != nil {
		t.Fatalf("resolve error = %v", err)
	}
	if resolution.Kind != "random" || len(resolution.Groups) != 1 {
		t.Fatalf("resolution = %+v", resolution)
	}
	group := resolution.Groups[0]
	if group.DrawCount != 2 {
		t.Fatalf("draw count = %d, want leading DrawCount 2", group.DrawCount)
	}
	if len(group.Entries) != 2 || group.Entries[0].ItemID != 3001 || group.Entries[0].Count != 1 || group.Entries[1].ItemID != 3002 || group.Entries[1].Count != 2 {
		t.Fatalf("entries = %+v", group.Entries)
	}
	if resolution.MaterialItemID != 10007367 || resolution.MaterialCountPerUse != 1 {
		t.Fatalf("material = %d x%d, want [need material] fallback", resolution.MaterialItemID, resolution.MaterialCountPerUse)
	}
}

func TestResolveCurrentMagicBoxUnsupportedAndUnknown(t *testing.T) {
	catalog, source := newMagicBoxTestCatalog(t)
	resolution, err := resolveCurrentMagicBox(catalog, source, 2002)
	if err != nil {
		t.Fatalf("resolve error = %v", err)
	}
	if resolution.Kind != "" {
		t.Fatalf("plain stackable kind = %q, want empty", resolution.Kind)
	}
	resolution, err = resolveCurrentMagicBox(catalog, source, 9999)
	if err != nil {
		t.Fatalf("resolve error = %v", err)
	}
	if resolution.Kind != "" {
		t.Fatalf("unknown item kind = %q, want empty", resolution.Kind)
	}
}

func TestResolveCurrentMagicBoxRewardItemMetadata(t *testing.T) {
	catalog, source := newMagicBoxTestCatalog(t)
	before := time.Now().UTC()
	item, err := resolveCurrentMagicBoxRewardItem(catalog, source, 2600014)
	if err != nil {
		t.Fatalf("resolve error = %v", err)
	}
	if item.ItemID != 2600014 || item.Kind != "stackable" || item.StackLimit != 100 || !item.Seal || item.UsablePeriodDays != 30 {
		t.Fatalf("reward item = %+v", item)
	}
	minExpire := time.Unix(before.Unix()+30*86400, 0).UTC()
	maxExpire := time.Unix(time.Now().UTC().Unix()+30*86400, 0).UTC()
	if item.ExpireAt.Before(minExpire) || item.ExpireAt.After(maxExpire) {
		t.Fatalf("reward expiration=%s outside %s..%s", item.ExpireAt, minExpire, maxExpire)
	}
	if item.SlotStart != 65 || item.SlotEnd != 120 {
		t.Fatalf("slot range = %d-%d, want 65-120", item.SlotStart, item.SlotEnd)
	}
	if item.PVFPath != "stackable/potion/ptn.stk" {
		t.Fatalf("path = %q", item.PVFPath)
	}
}
