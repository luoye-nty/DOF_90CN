package worldmap

import (
	"context"
	"os"
	"testing"

	platformpvf "longheng.io/server/internal/platform/pvf"
)

// TestZZDiagTimeRiftMaze3 dumps the real-PVF topology for dungeon 7539
// (Inferno_MetroCenter1 / 时空裂缝) maze 3 so the live op45 hang can be
// explained from the actual room graph instead of guessed.
func TestZZDiagTimeRiftMaze3(t *testing.T) {
	pvfPath := os.Getenv("DNF_WORLDMAP_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("DNF_WORLDMAP_REAL_PVF_SMOKE is not set")
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
	dungeon, found := table.FindDungeon(7539)
	if !found {
		t.Fatal("dungeon 7539 missing")
	}
	t.Logf("dungeon=%d path=%s mazes=%d", dungeon.ID, dungeon.Path, len(dungeon.Mazes))

	for mazeIndex, maze := range dungeon.Mazes {
		if mazeIndex != 3 {
			continue
		}
		t.Logf("MAZE %d index=%d start=%v boss=%v w=%v h=%v greed=%q questConnection=%v",
			mazeIndex, maze.Index, maze.Start, maze.Boss, maze.Width, maze.Height, maze.Greed, maze.QuestConnection)
		for _, spec := range maze.MapSpecifications {
			t.Logf("  MAPSPEC type=%q coord=%d:%d mapIDs=%v", spec.Type, spec.Coordinate.X, spec.Coordinate.Y, spec.MapIDs)
		}
		for _, spec := range maze.LayeredSpecifications {
			t.Logf("  LAYEREDSPEC coord=%d:%d mapIDs=%v", spec.Coordinate.X, spec.Coordinate.Y, spec.MapIDs)
		}
		t.Logf("  layeredRawSections=%d", len(maze.LayeredMapSpecifications))
		for _, cond := range maze.WarpMapConditions {
			t.Logf("  WARPCOND %+v", cond)
		}
		t.Logf("  clearConditions=%+v", maze.ClearConditions)
	}

	topology, err := BuildDungeonTopology(resolver, dungeon.ID, 3)
	if err != nil {
		t.Fatalf("build topology: %v", err)
	}
	t.Logf("TOPOLOGY start=%v bosses=%v width=%v height=%v allowMoveBeforeClear=%t",
		topology.Start, topology.Bosses, topology.Width, topology.Height, topology.AllowMoveBeforeClear)
	rooms := topology.Rooms()
	for _, room := range rooms {
		mapID := int64(0)
		monsters := 0
		if room.Map != nil {
			mapID = room.Map.Map.ID
			monsters = len(room.Map.Map.Monsters)
		}
		t.Logf("ROOM %s start=%t boss=%t map=%d monsters=%d neighbors=%v err=%q",
			room.Coordinate, room.Start, room.Boss, mapID, monsters, room.Neighbors, room.ResolutionError)
	}
	for _, d := range topology.Diagnostics {
		t.Logf("DIAG code=%s coord=%v msg=%s", d.Code, d.Coordinate, d.Message)
	}
}
