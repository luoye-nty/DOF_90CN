package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"sort"
	"strconv"
	"strings"

	"longheng.io/server/internal/modules/dnf/worldmap"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

func main() {
	pvfPath := flag.String("pvf", `D:/DNF/runtime/data/dnf/Script.pvf`, "Script.pvf path")
	dungeonIDs := flag.String("dungeons", "62,63,64,65,66,67,4000", "comma-separated dungeon ids")
	flag.Parse()

	ids, err := parseIDs(*dungeonIDs)
	if err != nil {
		log.Fatal(err)
	}
	archive, err := platformpvf.Open(*pvfPath)
	if err != nil {
		log.Fatal(err)
	}
	table, err := worldmap.LoadSource(context.Background(), archive, worldmap.Options{})
	if err != nil {
		log.Fatal(err)
	}

	maps := table.Maps()
	for _, dungeonID := range ids {
		dungeon, found := table.FindDungeon(dungeonID)
		if !found {
			fmt.Printf("dungeon=%d missing\n", dungeonID)
			continue
		}
		owned := make([]worldmap.Map, 0)
		for _, mapValue := range maps {
			if mapOwnsDungeon(mapValue, dungeonID) {
				owned = append(owned, mapValue)
			}
		}
		sort.Slice(owned, func(i, j int) bool { return owned[i].ID < owned[j].ID })
		monsterRows, randomRows, fixedRows := 0, 0, 0
		var randomTotal, fixedTotal int64
		for _, mapValue := range owned {
			for _, spawn := range mapValue.Monsters {
				monsterRows++
				if spawn.RandomDropCount > 0 {
					randomRows++
					randomTotal += spawn.RandomDropCount
				}
				if spawn.FixedDropCount > 0 {
					fixedRows++
					fixedTotal += spawn.FixedDropCount
				}
			}
		}
		warpRules := len(dungeon.WarpMapConditions)
		fmt.Printf("dungeon=%d name=%q path=%s mazes=%d owned_maps=%d monster_rows=%d random_rows=%d random_total=%d fixed_rows=%d fixed_total=%d warp_rules=%d\n",
			dungeon.ID, dungeon.Metadata.Name, dungeon.Path, len(dungeon.Mazes), len(owned), monsterRows,
			randomRows, randomTotal, fixedRows, fixedTotal, warpRules)
		for _, mapValue := range owned {
			for index, spawn := range mapValue.Monsters {
				if spawn.RandomDropCount == 0 && spawn.FixedDropCount == 0 {
					continue
				}
				fmt.Printf("  map=%d path=%s monster_index=%d monster=%d rank=%s random=%d fixed=%d\n",
					mapValue.ID, mapValue.Path, index, spawn.MonsterID, spawn.Rank,
					spawn.RandomDropCount, spawn.FixedDropCount)
			}
		}
	}
}

func parseIDs(value string) ([]int64, error) {
	parts := strings.Split(value, ",")
	ids := make([]int64, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		id, err := strconv.ParseInt(part, 10, 64)
		if err != nil || id <= 0 {
			return nil, fmt.Errorf("invalid dungeon id %q", part)
		}
		ids = append(ids, id)
	}
	return ids, nil
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
