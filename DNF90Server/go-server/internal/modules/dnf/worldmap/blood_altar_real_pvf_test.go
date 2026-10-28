package worldmap

import (
	"context"
	"os"
	"testing"

	platformpvf "longheng.io/server/internal/platform/pvf"
)

func TestRealScriptPVFBloodAltarTables(t *testing.T) {
	pvfPath := os.Getenv("DNF_WORLDMAP_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNF_WORLDMAP_REAL_PVF_SMOKE to verify Endless and Ultimate Altar tables")
	}
	archive, err := platformpvf.LoadArchive(platformpvf.Options{Path: pvfPath})
	if err != nil {
		t.Fatal(err)
	}
	table, err := LoadSource(context.Background(), archive, Options{SkipAreas: true, CompactRuntime: true})
	if err != nil {
		t.Fatal(err)
	}

	assertBloodAltarDungeon(t, table, 11006, BloodAltarDungeonKindEndless, 15)
	assertBloodAltarDungeon(t, table, 11007, BloodAltarDungeonKindUltimate, 7)
	assertRealEndlessAltarFrozenRoomOrder(t, table)
}

func assertRealEndlessAltarFrozenRoomOrder(t *testing.T, table *Table) {
	t.Helper()
	resolver, err := NewResolver(table)
	if err != nil {
		t.Fatal(err)
	}
	topology, err := BuildDungeonLayout(resolver, 11006, 0, func(choice DungeonMapChoice) (int64, error) {
		return choice.Candidates[0].ID, nil
	})
	if err != nil {
		t.Fatalf("build real Endless Altar topology: %v", err)
	}
	run, err := NewDungeonRun(topology)
	if err != nil {
		t.Fatalf("freeze real Endless Altar run: %v", err)
	}
	rooms := run.Rooms()
	if len(rooms) != 3 {
		t.Fatalf("real Endless Altar frozen rooms=%d want=3: %+v", len(rooms), rooms)
	}
	for index, room := range rooms {
		want := RoomCoordinate{X: int64(index), Y: 0}
		if room.Coordinate != want || room.Map == nil || room.ResolutionError != "" {
			t.Fatalf("real Endless Altar room %d=%+v want coordinate=%s with resolved map", index, room, want)
		}
	}
	bossMap := rooms[2].Map.Map
	timedTemplate := int64(-1)
	for index, monster := range bossMap.BloodMonsters {
		if monster.MonsterCode == 61171 && monster.DurationMilliseconds == 3000 &&
			monster.SpawnIntervalMilliseconds == 1000 && monster.BaseSpawnCount == 1 &&
			monster.SpawnCountIncrement == 1 && monster.BatchCount == 2 {
			timedTemplate = int64(index)
			break
		}
	}
	if timedTemplate < 0 {
		t.Fatalf("real Endless Altar boss map %d lost timed monster 61171: %+v", bossMap.ID, bossMap.BloodMonsters)
	}
	foundTimedPhase := false
	for _, phase := range bossMap.BloodPhases {
		if phase.Round == 0 && phase.MonsterTemplateIndex == timedTemplate && phase.DelayMilliseconds == 1000 {
			foundTimedPhase = true
			break
		}
	}
	if !foundTimedPhase {
		t.Fatalf("real Endless Altar boss map %d lost round-11 timed phase for template %d: %+v",
			bossMap.ID, timedTemplate, bossMap.BloodPhases)
	}

	completed := map[RoomCoordinate]struct{}{{X: 0, Y: 0}: {}}
	first, ok := uniqueUnfinishedFrozenAltarNeighbor(rooms, RoomCoordinate{X: 0, Y: 0}, completed)
	if !ok || first.Coordinate != (RoomCoordinate{X: 1, Y: 0}) {
		t.Fatalf("real Endless Altar next after room0=%+v unique=%t", first, ok)
	}
	completed[RoomCoordinate{X: 1, Y: 0}] = struct{}{}
	second, ok := uniqueUnfinishedFrozenAltarNeighbor(rooms, RoomCoordinate{X: 1, Y: 0}, completed)
	if !ok || second.Coordinate != (RoomCoordinate{X: 2, Y: 0}) {
		t.Fatalf("real Endless Altar next after rooms0/1=%+v unique=%t", second, ok)
	}
}

func uniqueUnfinishedFrozenAltarNeighbor(
	rooms []DungeonRoom,
	current RoomCoordinate,
	completed map[RoomCoordinate]struct{},
) (DungeonRoom, bool) {
	byCoordinate := make(map[RoomCoordinate]DungeonRoom, len(rooms))
	var source DungeonRoom
	foundSource := false
	for _, room := range rooms {
		byCoordinate[room.Coordinate] = room
		if room.Coordinate == current {
			source = room
			foundSource = true
		}
	}
	if !foundSource {
		return DungeonRoom{}, false
	}
	var result DungeonRoom
	count := 0
	for _, neighbor := range source.Neighbors {
		room, found := byCoordinate[neighbor.Coordinate]
		if !found || room.Map == nil || room.ResolutionError != "" {
			continue
		}
		if _, complete := completed[room.Coordinate]; complete {
			continue
		}
		result = room
		count++
	}
	return result, count == 1
}

func assertBloodAltarDungeon(
	t *testing.T,
	table *Table,
	dungeonID int64,
	kind BloodAltarDungeonKind,
	maxRounds int64,
) {
	t.Helper()
	dungeon, found := table.FindDungeon(dungeonID)
	if !found {
		t.Fatalf("blood altar dungeon %d is missing", dungeonID)
	}
	if dungeon.BloodAltarKind != kind || dungeon.BloodMaxRounds != maxRounds {
		t.Fatalf("blood altar dungeon %d metadata = kind=%d rounds=%d", dungeonID, dungeon.BloodAltarKind, dungeon.BloodMaxRounds)
	}

	ownedMaps := 0
	timedMonsters := 0
	for _, mapValue := range table.Maps() {
		if !mapOwnsDungeonForBloodAltarSmoke(mapValue, dungeonID) {
			continue
		}
		ownedMaps++
		monsters, phases := mapValue.BloodMonsters, mapValue.BloodPhases
		if kind == BloodAltarDungeonKindUltimate {
			monsters, phases = mapValue.UltimateMonsters, mapValue.UltimatePhases
		}
		if len(monsters) == 0 || len(phases) == 0 {
			t.Fatalf("blood altar dungeon %d map %d lost compact typed tables: monsters=%d phases=%d diagnostics=%+v",
				dungeonID, mapValue.ID, len(monsters), len(phases), mapValue.Diagnostics)
		}
		for _, monster := range monsters {
			if monster.DurationMilliseconds > 0 {
				timedMonsters++
			}
		}
		for index, phase := range phases {
			if phase.Round < 0 || phase.Round >= maxRounds || phase.MonsterTemplateIndex < 0 ||
				phase.MonsterTemplateIndex >= int64(len(monsters)) {
				t.Fatalf("blood altar dungeon %d map %d phase %d is out of range: %+v monsters=%d rounds=%d",
					dungeonID, mapValue.ID, index, phase, len(monsters), maxRounds)
			}
			if kind == BloodAltarDungeonKindUltimate &&
				phase.Difficulty != BloodAltarDifficultyA && phase.Difficulty != BloodAltarDifficultyB {
				t.Fatalf("ultimate altar map %d phase %d difficulty = %q", mapValue.ID, index, phase.Difficulty)
			}
		}
	}
	if ownedMaps == 0 {
		t.Fatalf("blood altar dungeon %d has no owned maps", dungeonID)
	}
	if timedMonsters == 0 {
		t.Fatalf("blood altar dungeon %d has no typed timed monster definitions", dungeonID)
	}
}

func mapOwnsDungeonForBloodAltarSmoke(mapValue Map, dungeonID int64) bool {
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
