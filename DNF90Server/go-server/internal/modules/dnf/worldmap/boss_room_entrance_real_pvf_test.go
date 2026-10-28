package worldmap

import (
	"context"
	"os"
	"testing"

	platformpvf "longheng.io/server/internal/platform/pvf"
)

func TestRealScriptPVFBossRoomEntranceConditions(t *testing.T) {
	pvfPath := os.Getenv("DNF_WORLDMAP_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNF_WORLDMAP_REAL_PVF_SMOKE to verify boss-room entrance conditions")
	}
	archive, err := platformpvf.LoadArchive(platformpvf.Options{Path: pvfPath})
	if err != nil {
		t.Fatal(err)
	}
	table, err := LoadSource(context.Background(), archive, Options{SkipMaps: true, SkipAreas: true, CompactRuntime: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, dungeonID := range []int64{2005, 8001} {
		dungeon, found := table.FindDungeon(dungeonID)
		if !found {
			t.Fatalf("dungeon %d is missing", dungeonID)
		}
		conditions := 0
		for mazeIndex, maze := range dungeon.Mazes {
			condition := maze.BossRoomEntranceCondition
			if condition == nil {
				continue
			}
			conditions++
			if condition.Action != "clear action" || len(condition.TimeCondition) != 2 ||
				condition.TimeCondition[0] != 1 || condition.TimeCondition[1] != 0 || len(condition.HuntMonsters) != 2 {
				t.Fatalf("dungeon %d maze %d condition=%+v diagnostics=%+v", dungeonID, mazeIndex, condition, maze.Diagnostics)
			}
			if condition.HuntMonsters[0] != (BossRoomEntranceHuntMonster{TargetID: 69201, Opaque: 0, Count: 2}) ||
				condition.HuntMonsters[1] != (BossRoomEntranceHuntMonster{TargetID: 69210, Opaque: 0, Count: 2}) {
				t.Fatalf("dungeon %d maze %d hunt targets=%+v", dungeonID, mazeIndex, condition.HuntMonsters)
			}
		}
		if conditions == 0 {
			t.Fatalf("dungeon %d has no typed boss-room entrance condition", dungeonID)
		}
		if len(dungeon.WarpMapConditions) != 2 || len(dungeon.Mazes) != 2 {
			t.Fatalf("dungeon %d warp rules=%+v mazes=%d", dungeonID, dungeon.WarpMapConditions, len(dungeon.Mazes))
		}
		wantWarp := WarpMapCondition{
			Source: Point{X: 2, Y: 1}, Destination: Point{X: 0, Y: 0},
		}
		for mazeIndex, maze := range dungeon.Mazes {
			if len(maze.WarpMapConditions) != 1 || maze.WarpMapConditions[0] != wantWarp {
				t.Fatalf("dungeon %d maze %d warp rules=%+v", dungeonID, mazeIndex, maze.WarpMapConditions)
			}
		}
		if dungeonID == 8001 && (dungeon.Mazes[0].BossRoomEntranceCondition != nil || dungeon.Mazes[1].BossRoomEntranceCondition == nil) {
			t.Fatalf("dungeon 8001 boss gate leaked across mazes: maze0=%+v maze1=%+v",
				dungeon.Mazes[0].BossRoomEntranceCondition,
				dungeon.Mazes[1].BossRoomEntranceCondition,
			)
		}
	}
}
