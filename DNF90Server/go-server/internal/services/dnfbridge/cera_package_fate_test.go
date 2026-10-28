package dnfbridge

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"

	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

func TestCurrentCeraPackageInventoryRouteSelectsEveryPVFGroupExactlyOnce(t *testing.T) {
	source := dungeonDropCatalogTestSource{
		"monster/monster.lst":                  "",
		"equipment/equipment.lst":              "",
		"stackable/stackable.lst":              "500 `cash/fate_package.stk`\n600 `material/fixed.stk`\n601 `material/title_a.stk`\n602 `material/title_b.stk`\n603 `material/bead_a.stk`\n604 `material/bead_b.stk`\n605 `cash/nested_box.stk`\n606 `material/nested_reward.stk`\n",
		"stackable/cash/fate_package.stk":      "[stackable type]\n`[cera package]`\n[stack limit]\n1\n[package data]\n600 3\n605 2\n[/package data]\n[package data selection]\n601 1\n602 1\n[/package data selection]\n[package data selection]\n603 1\n604 1\n[/package data selection]\n",
		"stackable/cash/nested_box.stk":        "[stackable type]\n`[cera package]`\n[stack limit]\n1\n[package data selection]\n606 4\n[/package data selection]\n",
		"stackable/material/fixed.stk":         "[stackable type]\n`[material]`\n[stack limit]\n999\n",
		"stackable/material/title_a.stk":       "[stackable type]\n`[material]`\n[stack limit]\n999\n",
		"stackable/material/title_b.stk":       "[stackable type]\n`[material]`\n[stack limit]\n999\n",
		"stackable/material/bead_a.stk":        "[stackable type]\n`[material]`\n[stack limit]\n999\n",
		"stackable/material/bead_b.stk":        "[stackable type]\n`[material]`\n[stack limit]\n999\n",
		"stackable/material/nested_reward.stk": "[stackable type]\n`[material]`\n[stack limit]\n999\n",
	}
	catalog, err := newPVFDungeonDropCatalog(source)
	if err != nil {
		t.Fatal(err)
	}
	definition, err := resolveCurrentCeraPackageDefinition(catalog, 500)
	if err != nil {
		t.Fatal(err)
	}
	if definition.StackableType != "[cera package]" || len(definition.SelectionGroups) != 2 {
		t.Fatalf("definition=%+v", definition)
	}
	rewards, err := resolveCurrentCeraPackageOpenRewards(definition, []currentCeraPackageChoice{{ItemID: 602}, {ItemID: 603}})
	if err != nil {
		t.Fatal(err)
	}
	got := make([]uint32, 0, len(rewards))
	for _, reward := range rewards {
		got = append(got, reward.Definition.ItemID)
	}
	want := []uint32{600, 605, 602, 603}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("selected rewards=%v want=%v", got, want)
	}
	expanded, err := expandCurrentDeterministicNestedCeraPackages(catalog, rewards)
	if err != nil {
		t.Fatal(err)
	}
	got = got[:0]
	counts := make(map[uint32]uint32)
	for _, reward := range expanded {
		got = append(got, reward.Definition.ItemID)
		counts[reward.Definition.ItemID] += reward.Count
	}
	want = []uint32{600, 606, 602, 603}
	if fmt.Sprint(got) != fmt.Sprint(want) || counts[606] != 8 {
		t.Fatalf("expanded rewards=%v counts=%v want=%v nested_count=8", got, counts, want)
	}
	for _, choices := range [][]currentCeraPackageChoice{
		nil,
		{{ItemID: 601}},
		{{ItemID: 601}, {ItemID: 602}, {ItemID: 603}},
		{{ItemID: 601}, {ItemID: 603}, {ItemID: 604}},
		{{ItemID: 601}, {ItemID: 603}, {ItemID: 600}},
	} {
		if _, err := resolveCurrentCeraPackageOpenRewards(definition, choices); err == nil {
			t.Fatalf("invalid choices accepted: %+v", choices)
		}
	}
}

func TestRealPVFFatePackageChainUsesNativePackageAndBoosterBoundaries(t *testing.T) {
	catalog := currentCeraPackageRealPVFCatalog(t)
	shopCatalog, err := newPVFCeraShopCatalog(catalog.source)
	if err != nil {
		t.Fatalf("load Fate Cera-shop catalog: %v", err)
	}

	for itemID := uint32(2682985); itemID <= 2683002; itemID++ {
		definition, err := resolveCurrentCeraPackageDefinition(catalog, itemID)
		if err != nil {
			t.Fatalf("Fate main package item=%d: %v", itemID, err)
		}
		wantGroups := 1
		if itemID >= 2682994 {
			wantGroups = 2
		}
		if definition.StackableType != "[cera package]" || len(definition.Rewards) == 0 || len(definition.SelectionGroups) != wantGroups {
			t.Fatalf("Fate main package item=%d type=%q fixed=%d groups=%d want_groups=%d", itemID, definition.StackableType, len(definition.Rewards), len(definition.SelectionGroups), wantGroups)
		}
		groupSizes := make([]int, 0, len(definition.SelectionGroups))
		for _, group := range definition.SelectionGroups {
			groupSizes = append(groupSizes, len(group))
		}
		t.Logf("Fate main package item=%d fixed=%d selection_group_sizes=%v", itemID, len(definition.Rewards), groupSizes)

		selections := make([]currentCeraShopPackageSelection, 0, len(definition.SelectionGroups))
		selectedRewards := append([]currentCeraPackageReward(nil), definition.Rewards...)
		for groupIndex, group := range definition.SelectionGroups {
			selections = append(selections, currentCeraShopPackageSelection{
				PackageItemID: itemID,
				GroupIndex:    uint16(groupIndex),
				ChoiceIndex:   0,
			})
			selectedRewards = append(selectedRewards, group[0])
		}
		avatarChoices := make([]currentCeraPackageChoice, 0)
		seenAvatars := make(map[uint32]struct{})
		for _, reward := range selectedRewards {
			if !reward.Avatar {
				continue
			}
			if _, duplicate := seenAvatars[reward.Definition.ItemID]; duplicate {
				continue
			}
			seenAvatars[reward.Definition.ItemID] = struct{}{}
			avatarChoices = append(avatarChoices, currentCeraPackageChoice{ItemID: reward.Definition.ItemID})
		}
		shopRewards, autoOpen, resolveErr := resolveCurrentCeraShopAutoOpenRewards(
			shopCatalog,
			definition.Source,
			1,
			avatarChoices,
			selections,
		)
		if resolveErr != nil || !autoOpen || len(shopRewards) == 0 {
			t.Fatalf("Fate checkout item=%d auto_open=%t rewards=%d err=%v", itemID, autoOpen, len(shopRewards), resolveErr)
		}
		for _, reward := range shopRewards {
			if reward.Definition.ItemID == itemID || reward.Definition.ItemID == 2683004 || reward.Definition.ItemID == 2683005 || reward.Definition.ItemID == 2683006 {
				t.Fatalf("Fate checkout item=%d leaked inert package reward=%+v", itemID, reward)
			}
		}
	}

	for itemID, wantGroups := range map[uint32]int{
		2683004: 0, // title collection
		2683005: 0, // card collection
		2683006: 1, // full-body avatar box (singleton nested selection)
	} {
		definition, err := resolveCurrentCeraPackageDefinition(catalog, itemID)
		if err != nil {
			t.Fatalf("Fate nested cera package item=%d: %v", itemID, err)
		}
		if definition.StackableType != "[cera package]" || len(definition.SelectionGroups) != wantGroups {
			t.Fatalf("Fate nested cera package item=%d type=%q groups=%d want=%d", itemID, definition.StackableType, len(definition.SelectionGroups), wantGroups)
		}
		opened, openErr := resolveCurrentCeraPackageOpenRewards(definition, nil)
		if openErr != nil || len(opened) == 0 {
			t.Fatalf("Fate nested inventory package item=%d rewards=%d err=%v", itemID, len(opened), openErr)
		}
	}
	nestedRewards := make([]currentCeraPackageReward, 0, 3)
	for _, itemID := range []uint32{2683004, 2683005, 2683006} {
		item, err := catalog.ResolveItem(itemID)
		if err != nil {
			t.Fatal(err)
		}
		nestedRewards = append(nestedRewards, currentCeraPackageReward{Definition: item, Count: 1})
	}
	expanded, err := expandCurrentDeterministicNestedCeraPackages(catalog, nestedRewards)
	if err != nil {
		t.Fatalf("expand deterministic Fate inner packages: %v", err)
	}
	containsFinalAvatarBox := false
	for _, reward := range expanded {
		switch reward.Definition.ItemID {
		case 2683004, 2683005, 2683006:
			t.Fatalf("inert Fate inner package leaked through deterministic expansion: %+v", reward)
		case 2683028:
			containsFinalAvatarBox = true
		}
	}
	if !containsFinalAvatarBox {
		t.Fatalf("expanded Fate inner rewards do not contain final op160 avatar selection box: %+v", expanded)
	}

	for _, itemID := range []uint32{2683003, 2683007, 2683008, 2683035} {
		definition, err := resolveCurrentBoosterDefinition(catalog, itemID, currentBoosterRequestRandom)
		if err != nil {
			t.Fatalf("Fate random booster item=%d: %v", itemID, err)
		}
		if definition.Source.ItemID != itemID || len(definition.Random.Groups) == 0 {
			t.Fatalf("Fate random booster item=%d definition=%+v", itemID, definition)
		}
	}
	selection, err := resolveCurrentBoosterDefinition(catalog, 2683028, currentBoosterRequestSelection)
	if err != nil {
		t.Fatalf("Fate final avatar selection item=2683028: %v", err)
	}
	if selection.Source.ItemID != 2683028 || len(selection.Selection) == 0 || len(selection.SelectionCategory) == 0 {
		t.Fatalf("Fate final avatar selection=%+v", selection)
	}
}

func TestRealPVFAllCeraPackageDefinitionsUseTheSharedMapper(t *testing.T) {
	catalog := currentCeraPackageRealPVFCatalog(t)
	type counts struct {
		total     int
		withFixed int
		withGroup int
	}
	byType := make(map[string]counts)
	invalidPVF := make(map[string][]string)
	unexpected := make([]string, 0)
	itemIDs := make([]int, 0, len(catalog.itemRefs))
	for itemID := range catalog.itemRefs {
		itemIDs = append(itemIDs, int(itemID))
	}
	sort.Ints(itemIDs)
	for _, rawID := range itemIDs {
		itemID := uint32(rawID)
		definition, err := catalog.ResolveItem(itemID)
		if err != nil || definition.Kind != dungeonDropItemStackable {
			continue
		}
		stackableType := normalizeDungeonDropStackableType(definition.StackableType)
		if stackableType != "[cera package]" && stackableType != "[usable cera package]" {
			continue
		}
		parsed, recognized, parseErr := resolveCurrentCeraPackageDefinitionForSource(catalog, definition)
		entry := byType[stackableType]
		entry.total++
		if parseErr != nil || !recognized {
			detail := fmt.Sprintf("item=%d path=%s err=%v", itemID, definition.PVFPath, parseErr)
			reason := "unexpected"
			switch {
			case strings.Contains(detail, " has no rewards"):
				reason = "empty_package_data"
			case strings.Contains(detail, "is not present in a supported PVF item list"):
				reason = "missing_reward_item"
			default:
				unexpected = append(unexpected, detail)
			}
			invalidPVF[reason] = append(invalidPVF[reason], detail)
			byType[stackableType] = entry
			continue
		}
		if len(parsed.Rewards) > 0 {
			entry.withFixed++
		}
		if len(parsed.SelectionGroups) > 0 {
			entry.withGroup++
		}
		byType[stackableType] = entry
	}
	for stackableType, entry := range byType {
		t.Logf("PVF package mapper type=%s total=%d fixed=%d selection=%d", stackableType, entry.total, entry.withFixed, entry.withGroup)
	}
	for reason, entries := range invalidPVF {
		t.Logf("PVF package definitions rejected reason=%s count=%d", reason, len(entries))
	}
	if len(unexpected) > 0 {
		limit := len(unexpected)
		if limit > 50 {
			limit = 50
		}
		t.Fatalf("unexpected package-mapper failures=%d first=%v", len(unexpected), unexpected[:limit])
	}
}

func TestRealPVFAllNativeGiftBoxFamiliesAreClassified(t *testing.T) {
	catalog := currentCeraPackageRealPVFCatalog(t)
	type familyCount struct {
		total         int
		mapped        int
		invalidShape  int
		missingReward int
	}
	families := make(map[string]familyCount)
	samples := make(map[string][]string)
	addSample := func(key string, itemID uint32, path string, detail ...string) {
		if len(samples[key]) < 8 {
			suffix := ""
			if len(detail) > 0 && detail[0] != "" {
				suffix = ":" + detail[0]
			}
			samples[key] = append(samples[key], fmt.Sprintf("%d:%s%s", itemID, path, suffix))
		}
	}
	itemIDs := make([]int, 0, len(catalog.itemRefs))
	for itemID := range catalog.itemRefs {
		itemIDs = append(itemIDs, int(itemID))
	}
	sort.Ints(itemIDs)
	for _, rawID := range itemIDs {
		itemID := uint32(rawID)
		item, err := catalog.ResolveItem(itemID)
		if err != nil || item.Kind != dungeonDropItemStackable {
			continue
		}
		family := normalizeMagicBoxPVFType(item.StackableType)
		switch family {
		case "booster selection":
			entry := families[family]
			entry.total++
			definition, resolveErr := resolveCurrentBoosterDefinition(catalog, itemID, currentBoosterRequestSelection)
			if resolveErr != nil || len(definition.Selection) == 0 {
				entry.invalidShape++
				detail := currentGiftBoxPVFShape(t, catalog.source, item.PVFPath)
				if resolveErr != nil {
					detail = "error=" + resolveErr.Error() + ";" + detail
				}
				addSample(family+":invalid_shape", itemID, item.PVFPath, detail)
				families[family] = entry
				continue
			}
			missing := false
			for _, reward := range definition.Selection {
				if _, rewardErr := catalog.ResolveItem(reward.ItemID); rewardErr != nil {
					missing = true
					break
				}
			}
			if missing {
				entry.missingReward++
				addSample(family+":missing_reward", itemID, item.PVFPath)
			} else {
				entry.mapped++
			}
			families[family] = entry
		case "booster", "cera booster", "booster random", "random upgradable legacy", "upgradable legacy":
			entry := families[family]
			entry.total++
			resolution, resolveErr := resolveCurrentMagicBox(catalog, catalog.source, int64(itemID))
			if resolveErr != nil || resolution.Kind != "random" || len(resolution.Groups) == 0 {
				entry.invalidShape++
				addSample(family+":invalid_shape", itemID, item.PVFPath, currentGiftBoxPVFShape(t, catalog.source, item.PVFPath))
				families[family] = entry
				continue
			}
			missing := false
			for _, group := range resolution.Groups {
				for _, reward := range group.Entries {
					if reward.ItemID <= 0 {
						continue
					}
					if _, rewardErr := catalog.ResolveItem(uint32(reward.ItemID)); rewardErr != nil {
						missing = true
						break
					}
				}
				if missing {
					break
				}
			}
			if missing {
				entry.missingReward++
				addSample(family+":missing_reward", itemID, item.PVFPath)
			} else {
				entry.mapped++
			}
			families[family] = entry
		}
	}
	for family, entry := range families {
		t.Logf("PVF native gift-box family=%s total=%d mapped=%d invalid_shape=%d missing_reward=%d", family, entry.total, entry.mapped, entry.invalidShape, entry.missingReward)
	}
	for key, entries := range samples {
		t.Logf("PVF native gift-box rejected key=%s samples=%v", key, entries)
	}
	for _, required := range []string{"booster", "booster selection", "upgradable legacy"} {
		if entry := families[required]; entry.total == 0 || entry.mapped == 0 {
			t.Fatalf("native gift-box family %q was not mapped: %+v", required, entry)
		}
	}
}

func currentGiftBoxPVFShape(t *testing.T, source dnfpvf.Source, path string) string {
	t.Helper()
	document, err := parseDungeonCardPVFDocument(source, path)
	if err != nil {
		return "parse=" + err.Error()
	}
	parts := make([]string, 0, len(document.Sections))
	for _, section := range document.Sections {
		name := normalizeMagicBoxSectionName(section.Name)
		if strings.HasPrefix(name, "/") || name == "name" || name == "stackable type" {
			continue
		}
		ints := magicBoxSectionInts(document, section)
		if len(ints) == 0 {
			parts = append(parts, name)
			continue
		}
		if len(ints) > 16 {
			ints = ints[:16]
		}
		parts = append(parts, fmt.Sprintf("%s=%v", name, ints))
	}
	return strings.Join(parts, ";")
}

func currentCeraPackageRealPVFCatalog(t *testing.T) *pvfDungeonDropCatalog {
	t.Helper()
	pvfPath := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE to verify package mapping against the active PVF")
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
