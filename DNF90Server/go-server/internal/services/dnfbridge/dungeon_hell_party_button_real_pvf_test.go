package dnfbridge

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"

	"longheng.io/server/internal/modules/dnf/dungeoncmd"
	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
	dnftown "longheng.io/server/internal/modules/dnf/town"
	"longheng.io/server/internal/modules/dnf/worldmap"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

// The two audits below are opt-in (they need the deployed Script.pvf) and exist
// because the op27 hell-party projection and the op16 hell admission plan both
// rest on PVF contracts that unit fixtures cannot prove:
//
//	$env:DNF_HELL_PARTY_BUTTON_REAL_PVF='runtime\data\dnf\Script.pvf'
//	go test -buildvcs=false -count=1 -run RealScriptPVFHell -v ./internal/services/dnfbridge

// TestRealScriptPVFHellPartyButtonGateAreas pins the assumption the op27
// projection rests on: a town area's [dungeon gate] value is a world map area
// id, not a dungeon id. It reports how many gates resolve to a world map area,
// how many of those are hell areas (so the projection can actually fire), and
// how many gate values would instead look like a dungeon id -- the alternative
// reading that would make the projection dead code.
func TestRealScriptPVFHellPartyButtonGateAreas(t *testing.T) {
	pvfPath := os.Getenv("DNF_HELL_PARTY_BUTTON_REAL_PVF")
	if pvfPath == "" {
		t.Skip("set DNF_HELL_PARTY_BUTTON_REAL_PVF to audit the real [dungeon gate] mapping")
	}
	archive, err := platformpvf.Open(pvfPath)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	world, err := worldmap.LoadSource(ctx, archive, worldmap.Options{SkipMaps: true, SkipDungeons: true})
	if err != nil {
		t.Fatal(err)
	}
	towns, err := dnftown.Load(ctx, archive, dnftown.Options{})
	if err != nil {
		t.Fatal(err)
	}

	var gates, resolvedAreas, hellAreas, dungeonMatches int
	for _, town := range towns.Towns() {
		for _, area := range town.Areas {
			if area.DungeonGate == nil || *area.DungeonGate <= 0 {
				continue
			}
			gates++
			if _, found := world.FindDungeon(*area.DungeonGate); found {
				dungeonMatches++
			}
			resolved, found := world.FindArea(*area.DungeonGate)
			if !found {
				continue
			}
			resolvedAreas++
			if !resolved.HellDungeon.Set || resolved.HellDungeon.Value != 1 || len(resolved.HellQuestIDs) == 0 {
				continue
			}
			hellAreas++
			if hellAreas <= 15 {
				t.Logf("hell gate: town=%d area=%d gate=%d worldmap_area=%d name=%q hell_quests=%v",
					town.ID, area.ID, *area.DungeonGate, resolved.ID, resolved.Name, resolved.HellQuestIDs)
			}
		}
	}
	if gates == 0 {
		t.Fatal("the real PVF declared no [dungeon gate] area at all")
	}
	t.Logf("dungeon gates=%d resolve_to_worldmap_area=%d of_which_hell=%d also_valid_dungeon_id=%d",
		gates, resolvedAreas, hellAreas, dungeonMatches)
	if resolvedAreas == 0 {
		t.Fatalf("no [dungeon gate] value resolved to a world map area (%d gates) - the projection would never fire", gates)
	}
	if hellAreas == 0 {
		t.Fatalf("no dungeon gate resolved to a hell area (%d gates, %d resolved) - the projection would never fire", gates, resolvedAreas)
	}
	if dungeonMatches*2 > gates {
		t.Fatalf("%d of %d gate values are also dungeon ids - the area reading is not proven", dungeonMatches, gates)
	}
}

// TestRealScriptPVFHellAdmissionCostSections dumps the raw world map area
// sections that carry the hell entry contract. The client's dungeon tooltip
// shows "消耗26个深渊派对邀请函 (0)" for dungeon 193 (能源熔炉, area 32) while the
// dungeon file itself declares no [hell party required item count], so the count
// must live in the area document -- this prints every section that could hold it.
func TestRealScriptPVFHellAdmissionCostSections(t *testing.T) {
	pvfPath := os.Getenv("DNF_HELL_PARTY_BUTTON_REAL_PVF")
	if pvfPath == "" {
		t.Skip("set DNF_HELL_PARTY_BUTTON_REAL_PVF to audit hell admission cost sections")
	}
	archive, err := platformpvf.Open(pvfPath)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	world, err := worldmap.LoadSource(ctx, archive, worldmap.Options{SkipMaps: true, SkipDungeons: true})
	if err != nil {
		t.Fatal(err)
	}
	printSections := func(area worldmap.Area) {
		t.Logf("area %d %q hell_dungeon={%d set=%t} item_conditions=%v hell_quests=%v freepass=%d",
			area.ID, area.Name, area.HellDungeon.Value, area.HellDungeon.Set,
			area.ItemConditions, area.HellQuestIDs, len(area.HellFreePassItems))
		for _, group := range [][]worldmap.RawSection{area.SourceSections, area.OpaqueSections, area.UnknownSections} {
			for _, section := range group {
				values := make([]string, 0, len(section.Tokens))
				for _, token := range section.Tokens {
					if token.Kind == dnfpvf.TokenInt {
						values = append(values, fmt.Sprintf("%d", token.Int))
						continue
					}
					values = append(values, strings.TrimSpace(token.Value))
				}
				t.Logf("   [%s] occurrence=%d tokens=%v", section.Name, section.Occurrence, values)
			}
		}
	}
	for _, areaID := range []int64{32, 39} {
		area, found := world.FindArea(areaID)
		if !found {
			t.Logf("area %d not found", areaID)
			continue
		}
		printSections(area)
	}
}

// TestRealScriptPVFHellEntryCostTable looks for the level-banded admission cost
// table the client's dungeon tooltip reads. The tooltip for dungeon 193
// (能源熔炉, minimum level 86) shows "消耗26个深渊派对邀请函 (0)", and the client picks
// the count from a row whose [low, high) band contains the level -- 26 is exactly
// (40*86-1800)/100+10, the rule the old tree's changelog recorded. That rule is
// not the per-dungeon [hell party required item count] this server requires, and
// only 11 of the 205 hell-area dungeons declare that section.
func TestRealScriptPVFHellEntryCostTable(t *testing.T) {
	pvfPath := os.Getenv("DNF_HELL_PARTY_BUTTON_REAL_PVF")
	if pvfPath == "" {
		t.Skip("set DNF_HELL_PARTY_BUTTON_REAL_PVF to inspect the hell entry cost table")
	}
	archive, err := platformpvf.Open(pvfPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		"etc/hellparty.etc",
		"etc/itemdropinfo_monster_hell.etc",
	} {
		text, err := archive.ReadText(path)
		if err != nil {
			t.Logf("%s: %v", path, err)
			continue
		}
		lines := strings.Split(text, "\n")
		t.Logf("===== %s (%d lines) =====", path, len(lines))
		inBasicValues := false
		for _, line := range lines {
			trimmed := strings.TrimSpace(line)
			if trimmed == "" {
				continue
			}
			// The admission cost rule recorded by the old tree --
			// (40*Lv-1800)/100+10 -- reads exactly like [hellparty basic values],
			// so print that block verbatim.
			if strings.HasPrefix(trimmed, "[hellparty basic values]") {
				inBasicValues = true
			}
			if inBasicValues {
				t.Logf("   %s", trimmed)
				if strings.HasPrefix(trimmed, "[/hellparty basic values]") {
					inBasicValues = false
				}
				continue
			}
			if strings.HasPrefix(trimmed, "[") || looksLikeLevelBandRow(trimmed) {
				t.Logf("   %s", trimmed)
			}
		}
	}
}

// hellPartyInvitationCountForLevel is the candidate invitation-cost rule recorded
// by the old tree's changelog: (40*Lv-1800)/100+10 with integer division.
func hellPartyInvitationCountForLevel(level int64) int64 {
	if level <= 0 {
		return 0
	}
	count := (40*level-1800)/100 + 10
	if count < 1 {
		return 1
	}
	return count
}

// looksLikeLevelBandRow reports whether a PVF line is a plausible
// "level_low level_high ... count" row of an admission cost table.
func looksLikeLevelBandRow(line string) bool {
	fields := strings.Fields(line)
	if len(fields) < 2 || len(fields) > 6 {
		return false
	}
	for _, field := range fields {
		if _, err := strconv.Atoi(field); err != nil {
			return false
		}
	}
	low, _ := strconv.Atoi(fields[0])
	high, _ := strconv.Atoi(fields[1])
	return low >= 1 && high > low && high <= 200
}

// TestRealScriptPVFHellAdmissionCosts audits the contract the op16 admission plan
// needs for a hell entry: the dungeon's own [hell party required item count] plus
// the owning area's single [item condition]. Dungeon 193 is the live failure
// (2026-09-26, character 11): the plan builder rejected it with
// "invalid PVF hell admission cost ... count=0 set=false item_conditions=[3330]",
// and the rejection was silent, so the client hung on the selector page.
func TestRealScriptPVFHellAdmissionCosts(t *testing.T) {
	pvfPath := os.Getenv("DNF_HELL_PARTY_BUTTON_REAL_PVF")
	if pvfPath == "" {
		t.Skip("set DNF_HELL_PARTY_BUTTON_REAL_PVF to audit hell admission costs")
	}
	archive, err := platformpvf.Open(pvfPath)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	// Dungeons must be loaded here (unlike the gate audit): the contract being
	// audited is the dungeon's own [hell party required item count].
	world, err := worldmap.LoadSource(ctx, archive, worldmap.Options{SkipMaps: true})
	if err != nil {
		t.Fatal(err)
	}
	// The five counts read off the live client on 2026-09-26 (hovering each
	// dungeon in the selector): the plan must price exactly what the client shows.
	// 310/316/7539 declare their own count and are pinned by
	// TestRealScriptPVFDungeonEntryAdmissionLists instead.
	measured := map[int64]int64{86: 20, 87: 20, 92: 21, 93: 21, 193: 26}
	service := &Service{worldMapTable: world}
	for _, dungeonID := range []int64{86, 87, 92, 93, 193, 310, 316, 7539} {
		dungeon, found := world.FindDungeon(dungeonID)
		if !found {
			t.Logf("dungeon %d: NOT in the world map table", dungeonID)
			continue
		}
		area, areaFound := world.FindAreaByDungeon(dungeonID)
		count := dungeon.Metadata.HellPartyRequiredItemCount
		minLevel := dungeon.Metadata.MinimumRequiredLevel
		basisLevel := dungeon.Metadata.BasisLevel
		t.Logf("dungeon %d name=%q minLevel={%d set=%t} basisLevel={%d set=%t} declaredCost={%d set=%t} formulaMin=%d formulaBasis=%d inferno=%t type=%q area=%d(%s) item_conditions=%v",
			dungeonID, dungeon.Metadata.Name,
			minLevel.Value, minLevel.Set, basisLevel.Value, basisLevel.Set,
			count.Value, count.Set,
			hellPartyInvitationCountForLevel(minLevel.Value), hellPartyInvitationCountForLevel(basisLevel.Value),
			dungeon.Metadata.InfernoDungeon, dungeon.Metadata.DungeonType,
			area.ID, area.Name, area.ItemConditions)
		if !areaFound {
			t.Logf("dungeon %d has no owning area", dungeonID)
		}
		want, isMeasured := measured[dungeonID]
		if !isMeasured {
			continue
		}
		plan, planErr := service.resolveCurrentDungeonEntryAdmissionPlan(&runtimeDungeonState{
			Request: dungeoncmd.SelectDungeonRequest{SelectionMode: 1, RuntimeToken: 0xffff},
			Dungeon: dungeon,
		})
		if planErr != nil {
			t.Errorf("dungeon %d: %v", dungeonID, planErr)
			continue
		}
		if len(plan.Requirements) != 1 ||
			plan.Requirements[0] != (worldmap.TicketItem{ItemID: 3330, Count: want}) {
			t.Errorf("dungeon %d plan requirements=%v want 3330 x%d (cost source %q)",
				dungeonID, plan.Requirements, want, plan.AdmissionCostSource)
			continue
		}
		t.Logf("  measured dungeon %d -> %v cost_source=%q", dungeonID, plan.Requirements, plan.AdmissionCostSource)
	}

	// Every dungeon that belongs to a hell area: which ones can actually price a
	// hell entry (the requirement the plan builder needs), and which cannot.
	var withCost, withoutCost, noItemCondition int
	// What the level-derived fallback needs to serve: a dungeon with no declared
	// cost must still have exactly one area [item condition] to price the entry.
	conditionHistogram := map[int]int{}
	levelMissing := 0
	fallbackReady := 0
	for _, area := range world.Areas() {
		if !area.HellDungeon.Set || area.HellDungeon.Value != 1 {
			continue
		}
		for _, entry := range area.Dungeons {
			dungeon, found := world.FindDungeon(entry.DungeonID)
			if !found {
				continue
			}
			count := dungeon.Metadata.HellPartyRequiredItemCount
			if count.Set && count.Value > 0 {
				withCost++
				continue
			}
			withoutCost++
			conditions := uniquePositiveInt64(area.ItemConditions)
			conditionHistogram[len(conditions)]++
			if dungeon.Metadata.MinimumRequiredLevel.Value <= 0 {
				levelMissing++
			} else if len(conditions) == 1 {
				fallbackReady++
			}
			if len(conditions) == 0 {
				noItemCondition++
			}
			if len(conditions) != 1 || withoutCost <= 3 {
				t.Logf("  hell area %d dungeon %d name=%q type=%q minLevel=%d basisLevel=%d has NO hell party required item count (area item conditions=%v)",
					area.ID, entry.DungeonID, dungeon.Metadata.Name, dungeon.Metadata.DungeonType,
					dungeon.Metadata.MinimumRequiredLevel.Value, dungeon.Metadata.BasisLevel.Value, area.ItemConditions)
			}
		}
	}
	keys := make([]int, 0, len(conditionHistogram))
	for k := range conditionHistogram {
		keys = append(keys, k)
	}
	sort.Ints(keys)
	t.Logf("hell-area dungeons without a declared cost: area_item_condition_count_histogram=%v", func() string {
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			parts = append(parts, fmt.Sprintf("%d->%d", k, conditionHistogram[k]))
		}
		return strings.Join(parts, " ")
	}())
	t.Logf("hell-area dungeons: with_cost=%d without_cost=%d without_cost_and_area_condition=%d level_missing=%d fallback_ready=%d",
		withCost, withoutCost, noItemCondition, levelMissing, fallbackReady)
	t.Logf("level-derived fallback cost samples: %v", func() string {
		parts := make([]string, 0, 8)
		for _, level := range []int64{45, 46, 60, 71, 72, 73, 74, 86, 90} {
			parts = append(parts, fmt.Sprintf("Lv%d=%d", level, hellPartyInvitationCountForLevel(level)))
		}
		return strings.Join(parts, " ")
	}())
}
