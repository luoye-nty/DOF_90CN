// Command monsterlevelaudit reports every PVF [monster] row whose resolved
// level falls outside 1..200, i.e. every row the clamp in
// currentDungeonMonsterLevel now rescues instead of failing the room.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"sort"

	"longheng.io/server/internal/modules/dnf/worldmap"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

const levelMax = 200

func main() {
	// The -pvf default follows this tree's other cmd/scratch PVF audits; the
	// upstream original called platform/installpath, which this tree does not
	// carry.
	pvfPath := flag.String("pvf", `D:/DNF/runtime/data/dnf/Script.pvf`, "Script.pvf path")
	sample := flag.Int("sample", 15, "rows to print per bucket")
	flag.Parse()

	archive, err := platformpvf.Open(*pvfPath)
	if err != nil {
		log.Fatal(err)
	}
	table, err := worldmap.LoadSource(context.Background(), archive, worldmap.Options{})
	if err != nil {
		log.Fatal(err)
	}

	// Map -> owning dungeon basis level. A map reachable from several dungeons
	// is audited once per dungeon, since basis is a dungeon property.
	type key struct {
		dungeon int64
		mapID   int64
	}
	basisByDungeon := map[int64]worldmap.OptionalInt{}
	for _, dungeon := range table.Dungeons() {
		basisByDungeon[dungeon.ID] = dungeon.Metadata.BasisLevel
	}

	var high, low, missingBasis []string
	rows, relative, absolute := 0, 0, 0
	seen := map[key]bool{}
	for _, dungeon := range table.Dungeons() {
		basis := basisByDungeon[dungeon.ID]
		for _, mapValue := range table.Maps() {
			if !mapOwnsDungeon(mapValue, dungeon.ID) {
				continue
			}
			k := key{dungeon: dungeon.ID, mapID: mapValue.ID}
			if seen[k] {
				continue
			}
			seen[k] = true
			for index, spawn := range mapValue.Monsters {
				rows++
				level := spawn.AutoLevel
				if spawn.Level != 0 {
					relative++
					if !basis.Set {
						missingBasis = append(missingBasis, fmt.Sprintf(
							"dungeon=%d map=%d index=%d monster=%d L2=%d L3=%d basis=unset",
							dungeon.ID, mapValue.ID, index, spawn.MonsterID, spawn.Level, spawn.AutoLevel))
						continue
					}
					level += basis.Value
				} else {
					absolute++
				}
				switch {
				case level >= levelMax:
					high = append(high, fmt.Sprintf(
						"dungeon=%d map=%d index=%d monster=%d L2=%d L3=%d basis=%d resolved=%d",
						dungeon.ID, mapValue.ID, index, spawn.MonsterID, spawn.Level, spawn.AutoLevel, basis.Value, level))
				case level <= 0:
					low = append(low, fmt.Sprintf(
						"dungeon=%d map=%d index=%d monster=%d L2=%d L3=%d basis=%d resolved=%d",
						dungeon.ID, mapValue.ID, index, spawn.MonsterID, spawn.Level, spawn.AutoLevel, basis.Value, level))
				}
			}
		}
	}

	// Maps carrying [monster] rows that no dungeon claims would reach the
	// relative branch with basis unset, so count them separately.
	orphanMaps, orphanRows := 0, 0
	claimed := map[int64]bool{}
	for k := range seen {
		claimed[k.mapID] = true
	}
	for _, mapValue := range table.Maps() {
		if len(mapValue.Monsters) == 0 || claimed[mapValue.ID] {
			continue
		}
		orphanMaps++
		orphanRows += len(mapValue.Monsters)
	}

	fmt.Printf("dungeons=%d dungeon_map_pairs=%d distinct_maps=%d monster_rows=%d relative=%d absolute=%d\n",
		len(table.Dungeons()), len(seen), len(claimed), rows, relative, absolute)
	fmt.Printf("maps with monsters but no owning dungeon: maps=%d rows=%d\n", orphanMaps, orphanRows)
	report("resolved >= 200 (clamped to 200)", high, *sample)
	report("resolved <= 0 (clamped to 1)", low, *sample)
	report("relative row without [basis level] (still an error)", missingBasis, *sample)
}

func report(title string, lines []string, sample int) {
	sort.Strings(lines)
	fmt.Printf("\n== %s: %d ==\n", title, len(lines))
	for i, line := range lines {
		if i >= sample {
			fmt.Printf("  ... %d more\n", len(lines)-sample)
			break
		}
		fmt.Printf("  %s\n", line)
	}
}

func mapOwnsDungeon(mapValue worldmap.Map, dungeonID int64) bool {
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
