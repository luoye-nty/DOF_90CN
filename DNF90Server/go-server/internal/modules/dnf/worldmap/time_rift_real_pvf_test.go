package worldmap

import (
	"context"
	"os"
	"testing"

	platformpvf "longheng.io/server/internal/platform/pvf"
)

// Time Rift has level-band variants. Keep every route on the real-PVF entry
// path: a regression in one of the rare special routes must never turn
// SELECT_DUNGEON into a client spinner again.
func TestRealScriptPVFTimeRiftAllMazesBuildEntryLayouts(t *testing.T) {
	pvfPath := os.Getenv("DNF_WORLDMAP_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNF_WORLDMAP_REAL_PVF_SMOKE to verify Time Rift 7527")
	}
	archive, err := platformpvf.LoadArchive(platformpvf.Options{Path: pvfPath})
	if err != nil {
		t.Fatal(err)
	}
	table, err := LoadSource(context.Background(), archive, Options{})
	if err != nil {
		t.Fatal(err)
	}
	resolver, err := NewResolver(table)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []struct {
		dungeonID int64
		questID   int64
		mazes     int
	}{
		{dungeonID: 7527, questID: 10104, mazes: 10},
		{dungeonID: 7539, questID: 10110, mazes: 7},
	} {
		dungeon, found := table.FindDungeon(want.dungeonID)
		if !found {
			t.Fatalf("Time Rift dungeon %d is missing", want.dungeonID)
		}
		if !dungeon.Metadata.InfernoDungeon || len(dungeon.Metadata.InfernoQuestIDs) != 1 ||
			dungeon.Metadata.InfernoQuestIDs[0] != want.questID {
			t.Fatalf("Time Rift dungeon=%d inferno metadata=%+v", want.dungeonID, dungeon.Metadata)
		}
		if len(dungeon.Mazes) != want.mazes {
			t.Fatalf("Time Rift dungeon=%d mazes=%d want %d", want.dungeonID, len(dungeon.Mazes), want.mazes)
		}

		for mazeIndex, maze := range dungeon.Mazes {
			if maze.Start == nil || maze.Boss == nil || !maze.ChanceRate.Set || maze.ChanceRate.Value <= 0 {
				t.Fatalf("dungeon=%d maze=%d entry metadata=%+v", want.dungeonID, mazeIndex, maze)
			}
			topology, err := BuildDungeonLayout(resolver, dungeon.ID, mazeIndex, func(choice DungeonMapChoice) (int64, error) {
				return choice.Candidates[0].ID, nil
			})
			if err != nil {
				t.Fatalf("dungeon=%d maze=%d layout: %v", want.dungeonID, mazeIndex, err)
			}
			start, ok := topology.Room(RoomCoordinate{X: maze.Start.X, Y: maze.Start.Y})
			if !ok || start.Map == nil || len(start.Map.Map.Monsters) == 0 {
				t.Fatalf("dungeon=%d maze=%d start=%+v found=%t", want.dungeonID, mazeIndex, start, ok)
			}
			boss, ok := topology.Room(RoomCoordinate{X: maze.Boss.X, Y: maze.Boss.Y})
			if !ok || boss.Map == nil || !boss.Boss {
				t.Fatalf("dungeon=%d maze=%d boss=%+v found=%t", want.dungeonID, mazeIndex, boss, ok)
			}
			if _, err := NewDungeonRun(topology); err != nil {
				t.Fatalf("dungeon=%d maze=%d create run: %v", want.dungeonID, mazeIndex, err)
			}
		}
	}
}
