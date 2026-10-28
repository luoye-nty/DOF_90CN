package dnfbridge

import (
	"context"
	"os"
	"testing"

	"longheng.io/server/internal/modules/dnf/worldmap"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

func TestRealScriptPVFDungeon3Quest3145StartRoomHasResolvedExit(t *testing.T) {
	pvfPath := os.Getenv("DNF_WORLDMAP_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNF_WORLDMAP_REAL_PVF_SMOKE to run real dungeon topology smoke")
	}
	archive, err := platformpvf.Open(pvfPath)
	if err != nil {
		t.Fatal(err)
	}
	table, err := worldmap.LoadSource(context.Background(), archive, worldmap.Options{})
	if err != nil {
		t.Fatal(err)
	}
	resolver, err := worldmap.NewResolver(table)
	if err != nil {
		t.Fatal(err)
	}
	dungeon, ok := table.FindDungeon(3)
	if !ok {
		t.Fatal("real PVF dungeon 3 is missing")
	}
	mazeIndex := -1
	for index, maze := range dungeon.Mazes {
		connection := maze.QuestConnection
		if len(connection) >= 2 && connection[0] == 0 && connection[1] == 3145 {
			mazeIndex = index
			break
		}
	}
	if mazeIndex < 0 {
		t.Fatal("real PVF dungeon 3 has no difficulty-0 quest-3145 maze")
	}
	topology, err := worldmap.BuildDungeonLayout(
		resolver,
		dungeon.ID,
		mazeIndex,
		func(choice worldmap.DungeonMapChoice) (int64, error) {
			return choice.Candidates[0].ID, nil
		},
	)
	if err != nil {
		t.Fatalf("build real dungeon 3 maze %d layout: %v", mazeIndex, err)
	}
	if topology.Start == nil {
		t.Fatalf("real dungeon 3 maze %d has no start coordinate", mazeIndex)
	}
	start, ok := topology.Room(*topology.Start)
	if !ok || start.Map == nil {
		t.Fatalf("real dungeon 3 maze %d start room is unresolved: %+v", mazeIndex, start)
	}
	if len(start.Neighbors) == 0 {
		t.Fatalf(
			"real dungeon 3 maze %d start room %s map %d has no topology neighbor",
			mazeIndex,
			start.Coordinate,
			start.Map.Map.ID,
		)
	}
	for _, neighbor := range start.Neighbors {
		room, found := topology.Room(neighbor.Coordinate)
		if !found || room.Map == nil {
			t.Fatalf("real start neighbor is unresolved: direction=%s coordinate=%s room=%+v", neighbor.Direction, neighbor.Coordinate, room)
		}
		t.Logf(
			"real dungeon 3 quest maze exit: maze=%d start=%s map=%d direction=%s target=%s target_map=%d start_portals=%+v",
			mazeIndex,
			start.Coordinate,
			start.Map.Map.ID,
			neighbor.Direction,
			neighbor.Coordinate,
			room.Map.Map.ID,
			start.Map.Map.Portals,
		)
	}
}

func TestRealScriptPVFAnton5314StartRoomHasRightExit(t *testing.T) {
	pvfPath := os.Getenv("DNF_WORLDMAP_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNF_WORLDMAP_REAL_PVF_SMOKE to run real dungeon topology smoke")
	}
	archive, err := platformpvf.Open(pvfPath)
	if err != nil {
		t.Fatal(err)
	}
	table, err := worldmap.LoadSource(context.Background(), archive, worldmap.Options{})
	if err != nil {
		t.Fatal(err)
	}
	resolver, err := worldmap.NewResolver(table)
	if err != nil {
		t.Fatal(err)
	}
	dungeon, ok := table.FindDungeon(5314)
	if !ok || len(dungeon.Mazes) == 0 {
		t.Fatal("real PVF single-Anton dungeon 5314 maze 0 is missing")
	}
	topology, err := worldmap.BuildDungeonLayout(
		resolver,
		dungeon.ID,
		0,
		func(choice worldmap.DungeonMapChoice) (int64, error) {
			return choice.Candidates[0].ID, nil
		},
	)
	if err != nil {
		t.Fatalf("build real single-Anton dungeon 5314 maze 0 layout: %v", err)
	}
	if topology.Start == nil {
		t.Fatal("single-Anton dungeon 5314 maze 0 has no start coordinate")
	}
	start, ok := topology.Room(*topology.Start)
	if !ok || start.Map == nil {
		t.Fatalf("single-Anton dungeon 5314 start room is unresolved: %+v", start)
	}
	foundRight := false
	for _, neighbor := range start.Neighbors {
		if neighbor.Direction != worldmap.RoomDirectionRight {
			continue
		}
		target, found := topology.Room(neighbor.Coordinate)
		if !found || target.Map == nil {
			t.Fatalf("single-Anton right exit target is unresolved: coordinate=%s room=%+v", neighbor.Coordinate, target)
		}
		foundRight = true
		t.Logf(
			"single-Anton 5314 start exit: start=%s map=%d target=%s target_map=%d portals=%+v",
			start.Coordinate,
			start.Map.Map.ID,
			neighbor.Coordinate,
			target.Map.Map.ID,
			start.Map.Map.Portals,
		)
	}
	if !foundRight {
		t.Fatalf(
			"single-Anton dungeon 5314 start room %s map %d has no right exit: neighbors=%+v portals=%+v",
			start.Coordinate,
			start.Map.Map.ID,
			start.Neighbors,
			start.Map.Map.Portals,
		)
	}
}

func TestRealScriptPVFForgottenLandKeepsSparseWarpTopology(t *testing.T) {
	pvfPath := os.Getenv("DNF_WORLDMAP_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNF_WORLDMAP_REAL_PVF_SMOKE to run real Forgotten Land topology smoke")
	}
	archive, err := platformpvf.Open(pvfPath)
	if err != nil {
		t.Fatal(err)
	}
	table, err := worldmap.LoadSource(context.Background(), archive, worldmap.Options{})
	if err != nil {
		t.Fatal(err)
	}
	resolver, err := worldmap.NewResolver(table)
	if err != nil {
		t.Fatal(err)
	}
	dungeon, ok := table.FindDungeon(4000)
	if !ok || len(dungeon.Mazes) == 0 {
		t.Fatal("real PVF Forgotten Land dungeon 4000 is missing")
	}
	if len(dungeon.WarpMapConditions) != 50 {
		t.Fatalf("Forgotten Land warp rule count=%d want=50", len(dungeon.WarpMapConditions))
	}
	topology, err := worldmap.BuildDungeonLayout(resolver, dungeon.ID, 0, func(choice worldmap.DungeonMapChoice) (int64, error) {
		return choice.Candidates[0].ID, nil
	})
	if err != nil {
		t.Fatalf("build Forgotten Land sparse layout: %v", err)
	}
	start, ok := topology.Room(worldmap.RoomCoordinate{X: 9, Y: 6})
	if !ok || start.Map == nil || start.Map.Map.ID != 11200 {
		t.Fatalf("Forgotten Land start=%+v ok=%t", start, ok)
	}
	bossMarker, ok := topology.Room(worldmap.RoomCoordinate{X: 9, Y: 0})
	if !ok || bossMarker.Map != nil || bossMarker.ResolutionError == "" {
		t.Fatalf("Forgotten Land logical boss marker=%+v ok=%t", bossMarker, ok)
	}
	if _, err := worldmap.NewDungeonRun(topology); err != nil {
		t.Fatalf("Forgotten Land entry run: %v", err)
	}
	run, err := worldmap.NewDungeonRun(topology)
	if err != nil {
		t.Fatalf("Forgotten Land entry run for warp: %v", err)
	}
	for _, coordinate := range []worldmap.RoomCoordinate{{X: 8, Y: 6}, {X: 8, Y: 7}, {X: 0, Y: 9}} {
		room, moveErr := run.MoveTo(coordinate)
		if moveErr != nil || room.Map == nil {
			t.Fatalf("Forgotten Land move to %s room=%+v err=%v", coordinate, room, moveErr)
		}
	}
	if _, moveErr := run.MoveTo(worldmap.RoomCoordinate{X: 8, Y: 7}); moveErr != nil {
		t.Fatalf("Forgotten Land declared reverse warp 0:9 -> 8:7 failed: %v", moveErr)
	}
}

// TestRealScriptPVFAnton219StartRoomResolves pins the Anton second-phase gate 219
// (blackvolcano), whose resources under map/219_blackvolcano are named
// 19200(0,0)b .. 19206(6,0)s while the .dgn declares `map 1 1 19201 ... boss 4 1
// 19200` and `[start map] 0 1` -- a coordinate no specification entry covers and
// no owned map reports. Before the directory start-map pool fallback in
// buildMazeMapIndex the whole gate was unenterable ("assign dungeon room map 0:1:
// ... pool is empty: dungeon=219 maze=0 file_type=start", live
// 2026-09-16T15:01:58Z, four attempts).
func TestRealScriptPVFAnton219StartRoomResolves(t *testing.T) {
	pvfPath := os.Getenv("DNF_WORLDMAP_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNF_WORLDMAP_REAL_PVF_SMOKE to run real dungeon topology smoke")
	}
	archive, err := platformpvf.Open(pvfPath)
	if err != nil {
		t.Fatal(err)
	}
	table, err := worldmap.LoadSource(context.Background(), archive, worldmap.Options{})
	if err != nil {
		t.Fatal(err)
	}
	resolver, err := worldmap.NewResolver(table)
	if err != nil {
		t.Fatal(err)
	}
	dungeon, ok := table.FindDungeon(219)
	if !ok {
		t.Fatal("real PVF dungeon 219 is missing")
	}
	if len(dungeon.Mazes) == 0 {
		t.Fatal("real PVF dungeon 219 has no maze")
	}
	for mazeIndex := range dungeon.Mazes {
		topology, buildErr := worldmap.BuildDungeonLayout(
			resolver,
			dungeon.ID,
			mazeIndex,
			func(choice worldmap.DungeonMapChoice) (int64, error) {
				return choice.Candidates[0].ID, nil
			},
		)
		if buildErr != nil {
			t.Fatalf("build real dungeon 219 maze %d layout: %v", mazeIndex, buildErr)
		}
		if topology.Start == nil {
			t.Fatalf("real dungeon 219 maze %d has no start coordinate", mazeIndex)
		}
		start, found := topology.Room(*topology.Start)
		if !found || start.Map == nil {
			t.Fatalf("real dungeon 219 maze %d start room %s is unresolved: %+v", mazeIndex, *topology.Start, start)
		}
		boss := int64(0)
		for _, coordinate := range topology.Bosses {
			if room, bossFound := topology.Room(coordinate); bossFound && room.Map != nil {
				boss = room.Map.Map.ID
				break
			}
		}
		if _, runErr := worldmap.NewDungeonRun(topology); runErr != nil {
			t.Fatalf("real dungeon 219 maze %d entry run: %v", mazeIndex, runErr)
		}
		t.Logf("anton 219 maze %d start=%s start_map=%d bosses=%v boss_map=%d", mazeIndex, start.Coordinate, start.Map.Map.ID, topology.Bosses, boss)
	}
}
