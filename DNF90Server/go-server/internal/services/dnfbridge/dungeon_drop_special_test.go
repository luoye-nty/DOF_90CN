package dnfbridge

import (
	"os"
	"testing"

	platformpvf "longheng.io/server/internal/platform/pvf"
)

func TestCurrentDungeonIndependentDropParsesFixedInlineAndSkipsListReference(t *testing.T) {
	text := `[independent drop]
0 100 5001 1000000 0 0 0 0 2 0 0 0 0 1 20 -1 0
0 100 0 1000000 0 0 0 0 2 0 0 0 0 1 20 -1 1
[list]
5002 1 5003 3
[/list]
0 100 0 1000000 0 0 0 0 2 0 0 0 0 1 20 -1 2
[list]
7
[/list]
[/independent drop]`
	tables, skipped, err := parseCurrentDungeonIndependentDrops(text)
	if err != nil {
		t.Fatal(err)
	}
	entries := tables.ByMonster
	if skipped != 1 || len(entries[100]) != 2 {
		t.Fatalf("entries=%+v skipped=%d", entries[100], skipped)
	}
	catalog := &currentDungeonSpecialDropCatalog{IndependentByMonster: entries}
	items := currentDungeonIndependentDropItems(catalog, 100, 0, 10, 0, newCurrentDungeonDropLCG(1))
	if len(items) != 4 || items[0] != 5001 || items[1] != 5001 {
		t.Fatalf("selected items=%v", items)
	}
	for _, itemID := range items[2:] {
		if itemID != 5002 && itemID != 5003 {
			t.Fatalf("inline selection item=%d", itemID)
		}
	}
}

func TestCurrentDungeonIndependentDropAppliesLevelDifficultyAndAttempts(t *testing.T) {
	text := `[independent drop]
0 200 6001 1000000 1000000 0 0 0 1 2 0 0 0 10 20 1 0
[/independent drop]`
	tables, _, err := parseCurrentDungeonIndependentDrops(text)
	if err != nil {
		t.Fatal(err)
	}
	catalog := &currentDungeonSpecialDropCatalog{IndependentByMonster: tables.ByMonster}
	if got := currentDungeonIndependentDropItems(catalog, 200, 0, 15, 0, newCurrentDungeonDropLCG(1)); len(got) != 0 {
		t.Fatalf("difficulty mismatch selected %v", got)
	}
	if got := currentDungeonIndependentDropItems(catalog, 200, 1, 9, 0, newCurrentDungeonDropLCG(1)); len(got) != 0 {
		t.Fatalf("level mismatch selected %v", got)
	}
	if got := currentDungeonIndependentDropItems(catalog, 200, 1, 15, 0, newCurrentDungeonDropLCG(1)); len(got) != 2 || got[0] != 6001 || got[1] != 6001 {
		t.Fatalf("matching selection=%v", got)
	}
}

// Type-1 records mirror usdof independent_drop.generate: the probability is
// not capped at the denominator, `divmod` grants whole multiples per attempt
// as guaranteed drops, and only the residual is rolled.
func TestCurrentDungeonIndependentDropType1GrantsWholeMultiplesAndRollsResidual(t *testing.T) {
	text := `[independent drop]
1 300 9001 3000000 0 0 0 0 2 0 0 0 0 0 0 -1 0
1 300 9002 1000000 0 0 0 0 3 0 0 0 0 0 0 -1 0
[/independent drop]`
	tables, skipped, err := parseCurrentDungeonIndependentDrops(text)
	if err != nil {
		t.Fatal(err)
	}
	entries := tables.ByAPC
	if skipped != 0 || len(entries[300]) != 2 {
		t.Fatalf("entries=%+v skipped=%d", entries[300], skipped)
	}
	if entries[300][0].EntryType != 1 || entries[300][1].EntryType != 1 {
		t.Fatalf("entry types=%+v", entries[300])
	}
	if len(tables.ByMonster) != 0 {
		t.Fatalf("type-1 rows leaked into the monster table: %+v", tables.ByMonster)
	}
	catalog := &currentDungeonSpecialDropCatalog{IndependentByAPC: entries}
	// 3000000/1000000 = whole 3 per attempt, residual 0; 2 attempts = 6 x 9001.
	// 1000000 = whole 1, residual 0; 3 attempts = 3 x 9002. No residual means
	// no LCG rolls, so the outcome is seed-independent.
	items := currentDungeonIndependentAPCDropItems(catalog, 300, 0, 10, 0, newCurrentDungeonDropLCG(99))
	if len(items) != 9 {
		t.Fatalf("selected items=%v", items)
	}
	for index, itemID := range items {
		want := uint32(9001)
		if index >= 6 {
			want = 9002
		}
		if itemID != want {
			t.Fatalf("item[%d]=%d want=%d (%v)", index, itemID, want, items)
		}
	}
}

func TestCurrentDungeonIndependentDropType1ResidualRollsPerAttempt(t *testing.T) {
	text := `[independent drop]
1 400 9100 300000 0 0 0 0 1 0 0 0 0 0 0 -1 0
[/independent drop]`
	tables, _, err := parseCurrentDungeonIndependentDrops(text)
	if err != nil {
		t.Fatal(err)
	}
	catalog := &currentDungeonSpecialDropCatalog{IndependentByAPC: tables.ByAPC}
	// Residual 300000/1000000: across seeds the single attempt must sometimes
	// hit and sometimes miss, and every hit yields exactly one item.
	hits := 0
	for seed := uint32(1); seed <= 64; seed++ {
		items := currentDungeonIndependentAPCDropItems(catalog, 400, 0, 10, 0, newCurrentDungeonDropLCG(seed))
		if len(items) > 1 {
			t.Fatalf("seed=%d items=%v", seed, items)
		}
		if len(items) == 1 {
			if items[0] != 9100 {
				t.Fatalf("seed=%d item=%d", seed, items[0])
			}
			hits++
		}
	}
	if hits == 0 || hits == 64 {
		t.Fatalf("residual roll hits=%d/64, want a mixed outcome", hits)
	}
}

func TestCurrentDungeonIndependentDropType1BonusIsUncappedFloor(t *testing.T) {
	text := `[independent drop]
1 500 9200 1000000 0 0 0 0 1 0 0 0 0 0 0 -1 0
[/independent drop]`
	tables, _, err := parseCurrentDungeonIndependentDrops(text)
	if err != nil {
		t.Fatal(err)
	}
	catalog := &currentDungeonSpecialDropCatalog{IndependentByAPC: tables.ByAPC}
	// 1000000 * (100+150)/100 = 2500000: whole 2 guaranteed plus a 50%
	// residual roll. Every seed must yield two or three items, and both
	// outcomes must appear.
	sawTwo, sawThree := false, false
	for seed := uint32(1); seed <= 64; seed++ {
		items := currentDungeonIndependentAPCDropItems(catalog, 500, 0, 10, 150, newCurrentDungeonDropLCG(seed))
		switch len(items) {
		case 2:
			sawTwo = true
		case 3:
			sawThree = true
		default:
			t.Fatalf("seed=%d items=%v", seed, items)
		}
	}
	if !sawTwo || !sawThree {
		t.Fatalf("uncapped bonus outcomes two=%t three=%t, want both", sawTwo, sawThree)
	}
}

// The Independent_Drop.etc actor code shares one number space between
// monsters (type 0) and APCs (type 1). The real table has APC 420/421/430 rows
// (Hell-party invitation 3330, challenge letter 3331, 史诗灵魂碎片 490003937,
// item 8057 at 100%) while monsters 420/421/430 are the 蜘蛛洞穴 spiders.
// Player report: "剧情地下城暗黑城蜘蛛会爆史诗灵魂碎片和挑战书". A monster kill must
// only see type-0 rows for its own code; APC rows stay in their own table.
func TestCurrentDungeonIndependentDropAPCRowsNeverApplyToSameCodeMonster(t *testing.T) {
	text := `[independent drop]
1 420 3331 1000000 1000000 1000000 1000000 1000000 1 1 1 1 1 0 0 -1 0
1 420 490003937 1000000 1000000 1000000 1000000 1000000 1 1 1 1 1 0 0 -1 0
1 430 8057 1000000 1000000 1000000 1000000 1000000 1 1 1 1 1 0 0 -1 0
0 420 3027 1000000 1000000 1000000 1000000 1000000 1 1 1 1 1 0 0 -1 0
[/independent drop]`
	tables, skipped, err := parseCurrentDungeonIndependentDrops(text)
	if err != nil {
		t.Fatal(err)
	}
	if skipped != 0 {
		t.Fatalf("skipped=%d", skipped)
	}
	if len(tables.ByMonster[420]) != 1 || tables.ByMonster[420][0].ItemID != 3027 {
		t.Fatalf("monster 420 rows=%+v want only the type-0 3027 row", tables.ByMonster[420])
	}
	if len(tables.ByMonster[430]) != 0 {
		t.Fatalf("monster 430 inherited APC rows: %+v", tables.ByMonster[430])
	}
	if len(tables.ByAPC[420]) != 2 || len(tables.ByAPC[430]) != 1 {
		t.Fatalf("apc rows 420=%+v 430=%+v", tables.ByAPC[420], tables.ByAPC[430])
	}
	catalog := &currentDungeonSpecialDropCatalog{IndependentByMonster: tables.ByMonster, IndependentByAPC: tables.ByAPC}
	// Every row is 100%, so the outcome is seed-independent: the spider drops
	// exactly its own 3027 and nothing from the APC namespace.
	for seed := uint32(1); seed <= 8; seed++ {
		items := currentDungeonIndependentDropItems(catalog, 420, 0, 33, 0, newCurrentDungeonDropLCG(seed))
		if len(items) != 1 || items[0] != 3027 {
			t.Fatalf("seed=%d spider 420 independent items=%v want [3027]", seed, items)
		}
		if items := currentDungeonIndependentDropItems(catalog, 430, 0, 33, 0, newCurrentDungeonDropLCG(seed)); len(items) != 0 {
			t.Fatalf("seed=%d young spider 430 independent items=%v want none", seed, items)
		}
	}
	// The APC namespace still owns its rows.
	apcItems := currentDungeonIndependentAPCDropItems(catalog, 420, 0, 33, 0, newCurrentDungeonDropLCG(1))
	if len(apcItems) != 2 || apcItems[0] != 3331 || apcItems[1] != 490003937 {
		t.Fatalf("apc 420 items=%v", apcItems)
	}
}

func TestCurrentDungeonIndependentDropRejectsUnknownEntryType(t *testing.T) {
	text := `[independent drop]
2 600 9300 1000000 0 0 0 0 1 0 0 0 0 0 0 -1 0
[/independent drop]`
	if _, _, err := parseCurrentDungeonIndependentDrops(text); err == nil {
		t.Fatal("entry type 2 must fail closed")
	}
}

func TestCurrentDungeonIndependentDropProbabilityAppliesGrowthContractAndCaps(t *testing.T) {
	tests := []struct {
		base    int
		percent int64
		want    int
	}{
		{base: 500000, percent: 0, want: 500000},
		{base: 500000, percent: 20, want: 600000},
		// Floor, not ceil: 833333 * 120 / 100 = 999999.6 -> 999999 (usdof).
		{base: 833333, percent: 20, want: 999999},
		{base: 833334, percent: 20, want: 1000000},
		{base: 1000000, percent: 30, want: 1000000},
		{base: 0, percent: 20, want: 0},
	}
	for _, test := range tests {
		if got := currentDungeonIndependentDropProbability(test.base, test.percent); got != test.want {
			t.Fatalf("base=%d percent=%d got=%d want=%d", test.base, test.percent, got, test.want)
		}
	}
}

func TestCurrentDungeonWorldDropParsesWeightedLevelAndRolls(t *testing.T) {
	text := `[world drop]
10 0 7001 25000 7002 75000 7999 0 -1
200 0 8001 100000 -1
[/world drop]`
	levels, err := parseCurrentDungeonWorldDrops(text)
	if err != nil {
		t.Fatal(err)
	}
	if len(levels) != 1 || levels[10].TotalWeight != 100000 || len(levels[10].Items) != 2 {
		t.Fatalf("levels=%+v", levels)
	}
	catalog := &currentDungeonSpecialDropCatalog{WorldByLevel: levels}
	itemID, selected := currentDungeonWorldDropItem(catalog, 10, newCurrentDungeonDropLCG(2))
	if !selected || (itemID != 7001 && itemID != 7002) {
		t.Fatalf("selected=%t item=%d", selected, itemID)
	}
	if _, selected := currentDungeonWorldDropItem(catalog, 200, newCurrentDungeonDropLCG(2)); selected {
		t.Fatal("level 200 must not generate a world drop")
	}
}

func TestRealPVFSpecialDropCatalog(t *testing.T) {
	pvfPath := os.Getenv("DNF_WORLDMAP_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNF_WORLDMAP_REAL_PVF_SMOKE to verify special drop tables")
	}
	archive, err := platformpvf.Open(pvfPath)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := loadCurrentDungeonSpecialDrops(archive)
	if err != nil {
		t.Fatal(err)
	}
	items, err := newPVFDungeonDropCatalog(archive)
	if err != nil {
		t.Fatal(err)
	}
	if len(catalog.IndependentByMonster) == 0 || len(catalog.WorldByLevel) == 0 {
		t.Fatalf("independent monsters=%d world levels=%d", len(catalog.IndependentByMonster), len(catalog.WorldByLevel))
	}
	independentEntries := 0
	itemIDs := make(map[uint32]struct{})
	for _, entries := range catalog.IndependentByMonster {
		independentEntries += len(entries)
		for _, entry := range entries {
			if entry.ItemID > 0 {
				itemIDs[entry.ItemID] = struct{}{}
			}
			for _, item := range entry.Items {
				itemIDs[item.ItemID] = struct{}{}
			}
		}
	}
	worldItems := 0
	for _, level := range catalog.WorldByLevel {
		worldItems += len(level.Items)
		for _, item := range level.Items {
			itemIDs[item.ItemID] = struct{}{}
		}
	}
	for itemID := range itemIDs {
		if _, resolveErr := items.ResolveItem(itemID); resolveErr != nil {
			t.Fatalf("special drop item %d is not resolvable: %v", itemID, resolveErr)
		}
	}
	for _, levelNumber := range []int{26, 27} {
		level := catalog.WorldByLevel[levelNumber]
		t.Logf("world level %d: total_weight=%d candidates=%d", levelNumber, level.TotalWeight, len(level.Items))
	}
	for _, monsterID := range []int64{200, 250, 260, 270, 500, 501, 62993} {
		t.Logf("independent monster %d: entries=%d", monsterID, len(catalog.IndependentByMonster[monsterID]))
	}
	// 蜘蛛洞穴 spiders share their codes with APC rows (3330/3331/490003937,
	// 8057). The real table has no type-0 rows for them, so the monster table
	// must be empty for these codes while the APC table keeps the rows.
	for _, spider := range []int64{420, 421, 430} {
		if rows := catalog.IndependentByMonster[spider]; len(rows) != 0 {
			t.Fatalf("spider %d inherited %d APC independent-drop rows: %+v", spider, len(rows), rows)
		}
		if len(catalog.IndependentByAPC[spider]) == 0 {
			t.Fatalf("apc %d rows missing from the APC table", spider)
		}
	}
	if len(catalog.IndependentByAPC) == 0 {
		t.Fatal("real table has 269 type-1 rows; the APC table must not be empty")
	}
	t.Logf("real special drops: apc codes=%d", len(catalog.IndependentByAPC))
	t.Logf("real special drops: independent monsters=%d entries=%d skipped_list_flag2=%d world_levels=%d weighted_world_items=%d unique_items=%d",
		len(catalog.IndependentByMonster), independentEntries, catalog.SkippedListFlag2, len(catalog.WorldByLevel), worldItems, len(itemIDs))
}
