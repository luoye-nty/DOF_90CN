// fiendwarroll: diagnose whether the boss shown on the board and the boss actually
// fought come from the SAME raid-start draw.
//
//	go run ./cmd/scratch/fiendwarroll -pvf <Script.pvf>
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"

	"longheng.io/server/internal/modules/dnf/raid"
	"longheng.io/server/internal/modules/dnf/worldmap"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

func main() {
	pvfPath := flag.String("pvf", `runtime\data\dnf\Script.pvf`, "Script.pvf path")
	flag.Parse()

	archive, err := platformpvf.Open(*pvfPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "open pvf:", err)
		os.Exit(1)
	}
	config, err := raid.LoadFiendWarConfig(archive)
	if err != nil {
		fmt.Fprintln(os.Stderr, "load fiendwar config:", err)
		os.Exit(1)
	}
	table, err := worldmap.LoadSource(context.Background(), archive, worldmap.Options{})
	if err != nil {
		fmt.Fprintln(os.Stderr, "load worldmap:", err)
		os.Exit(1)
	}
	battle, err := raid.NewFiendWarBattle(config.Battle)
	if err != nil {
		fmt.Fprintln(os.Stderr, "new battle:", err)
		os.Exit(1)
	}

	ids := make([]int, 0, len(config.Battle.Areas))
	for id := range config.Battle.Areas {
		ids = append(ids, id)
	}
	sort.Ints(ids)

	fmt.Printf("%-5s %-22s %-8s %-8s %-12s %-12s %s\n", "area", "kind", "dungeon", "roll", "table-boss", "pvf-boss", "verdict")
	for _, id := range ids {
		a := config.Battle.Areas[id]
		if strings.TrimSpace(a.Kind) == "" {
			continue
		}
		roll, rolled := battle.MazeForArea(id)
		tableBoss, _ := raid.FiendWarFixtureBossID(a.Dungeon, roll)
		mazeBosses := pvfMazeBosses(table, a.Dungeon)
		var pvfBoss int64
		if roll >= 0 && roll < len(mazeBosses) {
			pvfBoss = mazeBosses[roll]
		}
		verdict := "ok"
		switch {
		case !rolled:
			verdict = "NO-ROLL (board defaults to maze 0, entry draws again)"
		case len(mazeBosses) == 0:
			verdict = "NO-PVF-DUNGEON"
		case int64(tableBoss) != pvfBoss:
			verdict = "MISMATCH"
		}
		fmt.Printf("%-5d %-22s %-8d %-8s %-12d %-12d %s\n",
			id, a.Kind, a.Dungeon, fmt.Sprintf("%d/%v", roll, rolled), tableBoss, pvfBoss, verdict)
		if len(mazeBosses) > 1 {
			for maze, pvf := range mazeBosses {
				want, _ := raid.FiendWarFixtureBossID(a.Dungeon, maze)
				mark := "ok"
				if int64(want) != pvf {
					mark = "MISMATCH"
				}
				fmt.Printf("      maze %d: pvf=%-10d table=%-10d %s\n", maze, pvf, want, mark)
			}
		}
	}
}

func pvfMazeBosses(table *worldmap.Table, dungeon int) []int64 {
	value, ok := table.FindDungeon(int64(dungeon))
	if !ok {
		return nil
	}
	out := make([]int64, 0, len(value.Mazes))
	for _, maze := range value.Mazes {
		out = append(out, mazeBoss(table, maze))
	}
	return out
}

func mazeBoss(table *worldmap.Table, maze worldmap.Maze) int64 {
	for _, spec := range maze.MapSpecifications {
		if specKind(spec.Type) != "boss" {
			continue
		}
		for _, mapID := range spec.MapIDs {
			value, ok := table.FindMap(mapID)
			if !ok {
				continue
			}
			for _, spawn := range value.Monsters {
				if specKind(spawn.Rank) == "boss" || specKind(spawn.SuffixMarker) == "boss" {
					return spawn.MonsterID
				}
			}
		}
	}
	return 0
}

func specKind(value string) string {
	return strings.ToLower(strings.Trim(value, "[]` \t\r\n"))
}
