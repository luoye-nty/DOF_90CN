package worldmap

import (
	"errors"
	"reflect"
	"testing"
)

func TestDungeonTopologyBuildsStrictPVFRoomGraph(t *testing.T) {
	resolver, err := NewResolver(topologyTestTable())
	if err != nil {
		t.Fatal(err)
	}
	topology, err := BuildDungeonTopology(resolver, 800, 0)
	if err != nil {
		t.Fatalf("build topology: %v", err)
	}
	rooms := topology.Rooms()
	if len(rooms) != 5 {
		t.Fatalf("rooms=%d, want 5: %+v", len(rooms), rooms)
	}
	if topology.Start == nil || *topology.Start != (RoomCoordinate{X: 0, Y: 0}) {
		t.Fatalf("start=%+v", topology.Start)
	}
	if len(topology.Bosses) != 1 || topology.Bosses[0] != (RoomCoordinate{X: 2, Y: 1}) {
		t.Fatalf("bosses=%+v", topology.Bosses)
	}
	start, ok := topology.Room(RoomCoordinate{X: 0, Y: 0})
	if !ok || start.Map == nil || start.Map.Map.ID != 100 || !start.Start || start.Boss {
		t.Fatalf("start room=%+v ok=%v", start, ok)
	}
	if len(start.Neighbors) != 2 || start.Neighbors[0].Direction != RoomDirectionRight || start.Neighbors[1].Direction != RoomDirectionDown {
		t.Fatalf("start neighbors=%+v", start.Neighbors)
	}
	if _, ok := topology.Room(RoomCoordinate{X: 1, Y: 1}); ok {
		t.Fatal("closed [greed] cell was added to topology")
	}
	if unresolved := topology.UnresolvedRooms(); len(unresolved) != 0 {
		t.Fatalf("unresolved rooms=%+v", unresolved)
	}
}

func TestDungeonTopologyPreservesPVFAuthoredMonsterRanksAcrossRoomReads(t *testing.T) {
	start := RoomCoordinate{}
	topology := &DungeonTopology{
		DungeonID: 800,
		MazeIndex: 0,
		Start:     &start,
		rooms: map[coordinateKey]DungeonRoom{
			{}: {
				Coordinate: start,
				Start:      true,
				Map: &ResolvedMap{Map: Map{
					ID: 501,
					Monsters: []MonsterSpawn{
						{MonsterID: 1, Rank: "[normal]"},
						{MonsterID: 2, Rank: "[dummy]"},
						{MonsterID: 3, Rank: "[champion]"},
						{MonsterID: 4, Rank: "[super champion]"},
						{MonsterID: 5, Rank: "[boss]"},
					},
				}},
			},
		},
		resolutionErrors: make(map[coordinateKey]error),
	}

	first, ok := topology.Room(start)
	if !ok || first.Map == nil {
		t.Fatalf("first room=%+v ok=%t", first, ok)
	}
	wantRanks := []string{"[normal]", "[dummy]", "[champion]", "[super champion]", "[boss]"}
	if got := monsterSpawnRanks(first.Map.Map.Monsters); !reflect.DeepEqual(got, wantRanks) {
		t.Fatalf("first ranks=%v, want authored ranks %v", got, wantRanks)
	}
	first.Map.Map.Monsters[0].Rank = "[champion]"
	for read := 0; read < 3; read++ {
		room, roomOK := topology.Room(start)
		if !roomOK || room.Map == nil {
			t.Fatalf("read %d room=%+v ok=%t", read, room, roomOK)
		}
		if got := monsterSpawnRanks(room.Map.Map.Monsters); !reflect.DeepEqual(got, wantRanks) {
			t.Fatalf("read %d ranks=%v, want authored %v", read, got, wantRanks)
		}
	}
	rooms := topology.Rooms()
	if len(rooms) != 1 || rooms[0].Map == nil {
		t.Fatalf("rooms=%+v", rooms)
	}
	if got := monsterSpawnRanks(rooms[0].Map.Map.Monsters); !reflect.DeepEqual(got, wantRanks) {
		t.Fatalf("Rooms ranks=%v, want authored %v", got, wantRanks)
	}
}

func monsterSpawnRanks(spawns []MonsterSpawn) []string {
	ranks := make([]string, len(spawns))
	for index, spawn := range spawns {
		ranks[index] = spawn.Rank
	}
	return ranks
}

func TestDungeonRunValidatesCurrentEXECoordinatesWithoutFallback(t *testing.T) {
	resolver, err := NewResolver(topologyTestTable())
	if err != nil {
		t.Fatal(err)
	}
	topology, err := BuildDungeonTopology(resolver, 800, 0)
	if err != nil {
		t.Fatal(err)
	}
	run, err := NewDungeonRun(topology)
	if err != nil {
		t.Fatal(err)
	}
	if room, ok := run.CurrentRoom(); !ok || room.Coordinate.String() != "0:0" {
		t.Fatalf("current room=%+v ok=%v", room, ok)
	}
	if _, err := run.MoveByte(1, 0); !errors.Is(err, ErrRunCurrentRoomNotCleared) {
		t.Fatalf("uncleared move error=%v", err)
	}
	if err := run.MarkCurrentRoomCleared(); err != nil {
		t.Fatal(err)
	}
	room, err := run.MoveByte(1, 0)
	if err != nil || room.Map == nil || room.Map.Map.ID != 101 {
		t.Fatalf("adjacent move room=%+v err=%v", room, err)
	}
	if _, err := run.MoveTo(RoomCoordinate{X: 2, Y: 1}); !errors.Is(err, ErrRoomNotAdjacent) {
		t.Fatalf("diagonal/far move error=%v", err)
	}
	if _, err := run.MoveTo(RoomCoordinate{X: 1, Y: 1}); !errors.Is(err, ErrRoomNotFound) {
		t.Fatalf("closed cell move error=%v", err)
	}
	if err := run.MarkCurrentRoomCleared(); err != nil {
		t.Fatal(err)
	}
	if _, err := run.MoveRoomID("2:0"); err != nil {
		t.Fatalf("room id move: %v", err)
	}
	if err := run.Complete(); !errors.Is(err, ErrRunCurrentRoomNotCleared) {
		t.Fatalf("complete uncleared room error=%v", err)
	}
	if err := run.MarkCurrentRoomCleared(); err != nil {
		t.Fatal(err)
	}
	if err := run.Complete(); err != nil {
		t.Fatal(err)
	}
	snapshot := run.Snapshot()
	if snapshot.Status != DungeonRunCompleted || snapshot.Current != (RoomCoordinate{X: 2, Y: 0}) || len(snapshot.Visited) != 3 || len(snapshot.Cleared) != 3 {
		t.Fatalf("snapshot=%+v", snapshot)
	}
	if _, err := run.MoveByte(2, 1); !errors.Is(err, ErrRunNotActive) {
		t.Fatalf("move after completion error=%v", err)
	}
}

func TestDungeonRunUsesPVFMoveBeforeClearFlag(t *testing.T) {
	table := topologyTestTable()
	table.dungeons[0].Metadata.Flags = map[string]bool{"move map even enemy": true}
	resolver, err := NewResolver(table)
	if err != nil {
		t.Fatal(err)
	}
	topology, err := BuildDungeonTopology(resolver, 800, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !topology.AllowMoveBeforeClear {
		t.Fatal("PVF move-before-clear flag was not retained")
	}
	run, err := NewDungeonRun(topology)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := run.MoveByte(1, 0); err != nil {
		t.Fatalf("PVF-authorized move failed: %v", err)
	}
}

func TestDungeonRunUsesDeclaredDirectedWarpTransitions(t *testing.T) {
	table := topologyTestTable()
	table.dungeons[0].Metadata.Flags = map[string]bool{"move map even enemy": true}
	table.dungeons[0].WarpMapConditions = []WarpMapCondition{
		{Source: Point{X: 0, Y: 0}, Destination: Point{X: 2, Y: 1}},
		{Source: Point{X: -1, Y: -1}, Destination: Point{X: 2, Y: 0}},
	}
	resolver, err := NewResolver(table)
	if err != nil {
		t.Fatal(err)
	}
	topology, err := BuildDungeonTopology(resolver, 800, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(topology.warpTransitions) != 1 {
		t.Fatalf("warp transitions=%d want=1", len(topology.warpTransitions))
	}
	run, err := NewDungeonRun(topology)
	if err != nil {
		t.Fatal(err)
	}
	room, err := run.MoveTo(RoomCoordinate{X: 2, Y: 1})
	if err != nil || room.Map == nil || room.Map.Map.ID != 104 {
		t.Fatalf("declared warp room=%+v err=%v", room, err)
	}
	if _, err := run.MoveTo(RoomCoordinate{X: 0, Y: 0}); !errors.Is(err, ErrRoomNotAdjacent) {
		t.Fatalf("undeclared reverse warp error=%v", err)
	}
}

func TestDungeonRunDoesNotTreatBossEntranceWarpAsTravelEdge(t *testing.T) {
	table := topologyTestTable()
	table.dungeons[0].Metadata.Flags = map[string]bool{"move map even enemy": true}
	maze := &table.dungeons[0].Mazes[0]
	maze.BossRoomEntranceCondition = &BossRoomEntranceCondition{
		Action:       "clear action",
		HuntMonsters: []BossRoomEntranceHuntMonster{{TargetID: 69201, Count: 1}},
	}
	maze.WarpMapConditions = []WarpMapCondition{
		{Source: Point{X: 0, Y: 0}, Destination: Point{X: 2, Y: 1}},
	}
	table.dungeons[0].WarpMapConditions = append([]WarpMapCondition(nil), maze.WarpMapConditions...)
	resolver, err := NewResolver(table)
	if err != nil {
		t.Fatal(err)
	}
	topology, err := BuildDungeonTopology(resolver, 800, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(topology.warpTransitions) != 0 {
		t.Fatalf("boss-entrance warp became travel edges: %+v", topology.warpTransitions)
	}
	run, err := NewDungeonRun(topology)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := run.MoveTo(RoomCoordinate{X: 2, Y: 1}); !errors.Is(err, ErrRoomNotAdjacent) {
		t.Fatalf("boss-entrance destination accepted as direct travel: %v", err)
	}
}

func TestDungeonTopologyRetainsMalformedAndUnresolvedRooms(t *testing.T) {
	table := topologyTestTable()
	table.maps = table.maps[:1]
	table.mapByID = map[int64]int{100: 0}
	table.mapByPath = map[string]int{pathKey(table.maps[0].Path): 0}
	maze := &table.dungeons[0].Mazes[0]
	maze.Width = OptionalInt{Set: true, Value: 2}
	maze.Height = OptionalInt{Set: true, Value: 1}
	maze.Greed = "BBEE"
	maze.Boss = nil
	maze.MapSpecifications = maze.MapSpecifications[:1]
	resolver, err := NewResolver(table)
	if err != nil {
		t.Fatal(err)
	}
	topology, err := BuildDungeonTopology(resolver, 800, 0)
	if err != nil {
		t.Fatal(err)
	}
	unresolved := topology.UnresolvedRooms()
	if len(unresolved) != 1 || unresolved[0].Coordinate != (RoomCoordinate{X: 1, Y: 0}) || unresolved[0].ResolutionError == "" {
		t.Fatalf("unresolved rooms=%+v", unresolved)
	}
	run, err := NewDungeonRun(topology)
	if err != nil {
		t.Fatal(err)
	}
	if err := run.MarkCurrentRoomCleared(); err != nil {
		t.Fatal(err)
	}
	_, err = run.MoveByte(1, 0)
	if !errors.Is(err, ErrRoomMapUnresolved) || !errors.Is(err, ErrMapUnresolved) {
		t.Fatalf("unresolved move error=%v", err)
	}

	maze.Greed = "1"
	resolver, err = NewResolver(table)
	if err != nil {
		t.Fatal(err)
	}
	topology, err = BuildDungeonTopology(resolver, 800, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !hasTopologyDiagnostic(topology.Diagnostics, "maze_grid_row_short") {
		t.Fatalf("missing malformed greed diagnostic: %+v", topology.Diagnostics)
	}

	maze.Greed = "BBEF"
	resolver, err = NewResolver(table)
	if err != nil {
		t.Fatal(err)
	}
	topology, err = BuildDungeonTopology(resolver, 800, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !hasTopologyDiagnostic(topology.Diagnostics, "maze_grid_pair_mismatch") {
		t.Fatalf("missing mismatched pair diagnostic: %+v", topology.Diagnostics)
	}

	maze.Greed = "BBZZ"
	resolver, err = NewResolver(table)
	if err != nil {
		t.Fatal(err)
	}
	topology, err = BuildDungeonTopology(resolver, 800, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !hasTopologyDiagnostic(topology.Diagnostics, "maze_grid_code_unknown") {
		t.Fatalf("missing unknown code diagnostic: %+v", topology.Diagnostics)
	}
}

func TestDungeonTopologyKeepsRepeatedBossCoordinates(t *testing.T) {
	table := topologyTestTable()
	table.dungeons[0].Mazes[0].Boss = &MazePoint{X: 2, Y: 1, Params: []int64{2, 0, 99}}
	resolver, err := NewResolver(table)
	if err != nil {
		t.Fatal(err)
	}
	topology, err := BuildDungeonTopology(resolver, 800, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(topology.Bosses) != 2 || topology.Bosses[1] != (RoomCoordinate{X: 2, Y: 0}) {
		t.Fatalf("boss coordinates=%+v", topology.Bosses)
	}
	room, ok := topology.Room(RoomCoordinate{X: 2, Y: 0})
	if !ok || !room.Boss {
		t.Fatalf("second boss room=%+v ok=%v", room, ok)
	}
}

func TestRoomCoordinateParsing(t *testing.T) {
	coordinate, err := ParseRoomCoordinate(" -2:15 ")
	if err != nil || coordinate != (RoomCoordinate{X: -2, Y: 15}) || coordinate.String() != "-2:15" {
		t.Fatalf("coordinate=%+v err=%v", coordinate, err)
	}
	for _, value := range []string{"", "1", "1:2:3", "a:2"} {
		if _, err := ParseRoomCoordinate(value); !errors.Is(err, ErrRoomCoordinateMalformed) {
			t.Errorf("ParseRoomCoordinate(%q) error=%v", value, err)
		}
	}
}

func TestGreedRoomMasksFollowLocalPVFEncoding(t *testing.T) {
	tests := []struct {
		code rune
		mask uint8
	}{
		{'A', 0x00},
		{'B', 0x01},
		{'C', 0x02},
		{'E', 0x04},
		{'I', 0x08},
		{'P', 0x0f},
		{'p', 0x0f},
	}
	for _, test := range tests {
		mask, ok := greedRoomMask(test.code)
		if !ok || mask != test.mask {
			t.Errorf("greedRoomMask(%q)=(%02x,%v), want %02x", test.code, mask, ok, test.mask)
		}
	}
	if _, ok := greedRoomMask('Z'); ok {
		t.Fatal("unknown local PVF room code was accepted")
	}
}

func hasTopologyDiagnostic(diagnostics []TopologyDiagnostic, code string) bool {
	for _, diagnostic := range diagnostics {
		if diagnostic.Code == code {
			return true
		}
	}
	return false
}

func topologyTestTable() *Table {
	maps := []Map{
		{ID: 100, Path: "map/topology/s100(0,0).map"},
		{ID: 101, Path: "map/topology/101(1,0).map"},
		{ID: 102, Path: "map/topology/102(2,0).map"},
		{ID: 103, Path: "map/topology/103(0,1).map"},
		{ID: 104, Path: "map/topology/104(2,1)boss.map"},
	}
	dungeon := Dungeon{
		ID: 800, Path: "dungeon/topology.dgn",
		Mazes: []Maze{{
			Index: 0,
			Width: OptionalInt{Set: true, Value: 3}, Height: OptionalInt{Set: true, Value: 2},
			Greed: "JJFFMM\nCCAACC",
			Start: &MazePoint{X: 0, Y: 0}, Boss: &MazePoint{X: 2, Y: 1},
			MapSpecifications: []MapSpecification{
				{Type: "start", Coordinate: Point{X: 0, Y: 0}, MapIDs: []int64{100}},
				{Type: "boss", Coordinate: Point{X: 2, Y: 1}, MapIDs: []int64{104}},
			},
		}},
	}
	table := &Table{
		maps: maps, dungeons: []Dungeon{dungeon},
		mapByID: make(map[int64]int), mapByPath: make(map[string]int),
		dungeonByID: map[int64]int{800: 0}, dungeonByPath: map[string]int{pathKey(dungeon.Path): 0},
	}
	for index, mapValue := range maps {
		table.mapByID[mapValue.ID] = index
		table.mapByPath[pathKey(mapValue.Path)] = index
	}
	return table
}
