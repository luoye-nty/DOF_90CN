package dnfbridge

import (
	"context"
	"os"
	"sort"
	"testing"

	"longheng.io/server/internal/modules/dnf/worldmap"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

// TestRealScriptPVFImpossibleDungeonDropSources audits the actual current PVF
// instead of baking regional item ids into the server. It is opt-in because
// opening the 100+ MB archive is intentionally outside the ordinary unit suite.
func TestRealScriptPVFImpossibleDungeonDropSources(t *testing.T) {
	pvfPath := os.Getenv("DNF_WORLDMAP_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNF_WORLDMAP_REAL_PVF_SMOKE to audit real impossible-dungeon drops")
	}
	archive, err := platformpvf.Open(pvfPath)
	if err != nil {
		t.Fatal(err)
	}
	table, err := worldmap.LoadSource(context.Background(), archive, worldmap.Options{})
	if err != nil {
		t.Fatal(err)
	}
	monsterCatalog, err := newPVFDungeonMonsterCatalog(archive)
	if err != nil {
		t.Fatal(err)
	}
	dropCatalog, err := monsterCatalog.DropCatalog()
	if err != nil {
		t.Fatal(err)
	}
	specialDrops, err := currentDungeonSpecialDrops(monsterCatalog)
	if err != nil {
		t.Fatal(err)
	}
	dimensionDrops, err := currentDungeonDimensionDrops(monsterCatalog)
	if err != nil {
		t.Fatal(err)
	}

	allMaps := table.Maps()
	for _, dungeonID := range []int64{62, 63, 64, 65, 66, 67} {
		dimensionTier, dimensionPool, dimensionMatched, dimensionErr := currentDungeonDimensionDropPool(dimensionDrops, dungeonID, 90)
		if dimensionErr != nil {
			t.Fatalf("dungeon=%d dimension drops: %v", dungeonID, dimensionErr)
		}
		if dungeonID == 66 {
			if dimensionMatched {
				t.Fatalf("dungeon=66 is absent from the current PVF dimension membership but matched tier=%d pool=%v", dimensionTier, dimensionPool)
			}
		} else if !dimensionMatched || len(dimensionPool) == 0 {
			t.Fatalf("dungeon=%d has no current-PVF dimension material mapping: tier=%d pool=%v", dungeonID, dimensionTier, dimensionPool)
		}
		for _, entry := range dimensionPool {
			if _, resolveErr := dropCatalog.ResolveItem(entry.ItemID); resolveErr != nil {
				t.Fatalf("dungeon=%d dimension item=%d: %v", dungeonID, entry.ItemID, resolveErr)
			}
		}
		monsterIDs := make(map[int64]struct{})
		spawnRows, randomRolls, specifiedRolls := 0, int64(0), int64(0)
		for _, mapValue := range allMaps {
			if !realPVFMapOwnsDungeon(mapValue, dungeonID) {
				continue
			}
			for _, spawn := range mapValue.Monsters {
				spawnRows++
				randomRolls += spawn.RandomDropCount
				specifiedRolls += spawn.FixedDropCount
				if spawn.MonsterID > 0 {
					monsterIDs[spawn.MonsterID] = struct{}{}
				}
			}
		}
		ids := make([]int64, 0, len(monsterIDs))
		for monsterID := range monsterIDs {
			ids = append(ids, monsterID)
		}
		sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
		pools, entries, unresolved := 0, 0, 0
		independentEntries, independentCandidates := 0, 0
		independentItemNames := make(map[string]struct{})
		for _, monsterID := range ids {
			pool, poolErr := dropCatalog.MonsterPool(monsterID)
			if poolErr != nil {
				t.Fatalf("dungeon=%d monster=%d pool: %v", dungeonID, monsterID, poolErr)
			}
			if len(pool) > 0 {
				pools++
				entries += len(pool)
				for _, entry := range pool {
					if _, resolveErr := dropCatalog.ResolveItem(entry.ItemID); resolveErr != nil {
						unresolved++
						t.Logf("dungeon=%d monster=%d item=%d unresolved=%v", dungeonID, monsterID, entry.ItemID, resolveErr)
					}
				}
			}
			for _, entry := range specialDrops.IndependentByMonster[monsterID] {
				independentEntries++
				candidateIDs := make([]uint32, 0, len(entry.Items)+1)
				if entry.ItemID > 0 {
					candidateIDs = append(candidateIDs, entry.ItemID)
				}
				for _, item := range entry.Items {
					candidateIDs = append(candidateIDs, item.ItemID)
				}
				for _, itemID := range candidateIDs {
					definition, resolveErr := dropCatalog.ResolveItem(itemID)
					if resolveErr != nil {
						unresolved++
						t.Logf("dungeon=%d monster=%d independent_item=%d unresolved=%v", dungeonID, monsterID, itemID, resolveErr)
						continue
					}
					independentCandidates++
					independentItemNames[definition.Name] = struct{}{}
				}
			}
		}
		if spawnRows == 0 || randomRolls == 0 || specifiedRolls == 0 {
			t.Fatalf("dungeon=%d has no dynamic map drop source: rows=%d random=%d specified=%d", dungeonID, spawnRows, randomRolls, specifiedRolls)
		}
		itemNames := make([]string, 0, len(independentItemNames))
		for name := range independentItemNames {
			itemNames = append(itemNames, name)
		}
		sort.Strings(itemNames)
		t.Logf("dungeon=%d spawn_rows=%d random_rolls=%d specified_rolls=%d dimension_matched=%t dimension_tier=%d dimension_pool=%v unique_monsters=%d explicit_pools=%d explicit_entries=%d independent_entries=%d independent_candidates=%d independent_items=%q unresolved_items=%d",
			dungeonID, spawnRows, randomRolls, specifiedRolls,
			dimensionMatched, dimensionTier, dimensionPool, len(ids), pools, entries,
			independentEntries, independentCandidates, itemNames, unresolved)
	}
}

func realPVFMapOwnsDungeon(mapValue worldmap.Map, dungeonID int64) bool {
	if mapValue.DungeonID.Set && mapValue.DungeonID.Value == dungeonID {
		return true
	}
	for _, candidate := range mapValue.DungeonIDs {
		if candidate == dungeonID {
			return true
		}
	}
	return false
}
