package worldmap

import (
	"errors"
	"testing"
)

func TestBuildDungeonLayoutAssignsExplicitCandidatesAndTypedPools(t *testing.T) {
	table := layoutTestTable()
	resolver, err := NewResolver(table)
	if err != nil {
		t.Fatal(err)
	}
	var choices []DungeonMapChoice
	topology, err := BuildDungeonLayout(resolver, 800, 0, func(choice DungeonMapChoice) (int64, error) {
		choices = append(choices, choice)
		switch choice.Coordinate {
		case (RoomCoordinate{X: 0, Y: 0}):
			return 501, nil
		case (RoomCoordinate{X: 1, Y: 0}):
			return 502, nil
		default:
			return 0, errors.New("unexpected room")
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(choices) != 2 {
		t.Fatalf("choices = %+v", choices)
	}
	var coordinateChoice, poolChoice *DungeonMapChoice
	for index := range choices {
		switch choices[index].Coordinate {
		case (RoomCoordinate{X: 0, Y: 0}):
			coordinateChoice = &choices[index]
		case (RoomCoordinate{X: 1, Y: 0}):
			poolChoice = &choices[index]
		}
	}
	if coordinateChoice == nil || coordinateChoice.Source != ResolutionDungeonOwnership || len(coordinateChoice.Candidates) != 2 {
		t.Fatalf("coordinate choice = %+v", coordinateChoice)
	}
	if poolChoice == nil || poolChoice.Source != ResolutionTypePool || len(poolChoice.Candidates) != 1 {
		t.Fatalf("pool choice = %+v", poolChoice)
	}
	start, ok := topology.Room(RoomCoordinate{X: 0, Y: 0})
	if !ok || start.Map == nil || start.Map.Map.ID != 501 {
		t.Fatalf("start room = %+v ok=%v", start, ok)
	}
	boss, ok := topology.Room(RoomCoordinate{X: 1, Y: 0})
	if !ok || boss.Map == nil || boss.Map.Map.ID != 502 {
		t.Fatalf("boss room = %+v ok=%v", boss, ok)
	}
	if len(topology.UnresolvedRooms()) != 0 || len(topology.resolutionErrors) != 0 {
		t.Fatalf("layout unresolved = %+v errors=%v", topology.UnresolvedRooms(), topology.resolutionErrors)
	}
}

func TestBuildDungeonLayoutRequiresAndValidatesChoice(t *testing.T) {
	resolver, err := NewResolver(layoutTestTable())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := BuildDungeonLayout(resolver, 800, 0, nil); !errors.Is(err, ErrMapChoiceRequired) {
		t.Fatalf("missing chooser = %v", err)
	}
	_, err = BuildDungeonLayout(resolver, 800, 0, func(DungeonMapChoice) (int64, error) {
		return 9999, nil
	})
	if !errors.Is(err, ErrMapChoiceRejected) {
		t.Fatalf("rejected choice = %v", err)
	}
}

func TestBuildDungeonLayoutSingleStartBossRoomFallsBackToNormalOwnedPool(t *testing.T) {
	table := &Table{
		maps: []Map{
			{
				ID:         15130,
				Path:       "map/towerofdespair_down/15130despair001.map",
				Type:       "[normal]",
				DungeonIDs: []int64{11008},
			},
		},
		dungeons: []Dungeon{{
			ID:   11008,
			Path: "dungeon/Towers/ZTowerOfDespair001.dgn",
			Mazes: []Maze{{
				Index:  0,
				Width:  OptionalInt{Value: 1, Set: true},
				Height: OptionalInt{Value: 1, Set: true},
				Greed:  "BB",
				Start:  &MazePoint{X: 0, Y: 0},
				Boss:   &MazePoint{X: 0, Y: 0},
			}},
		}},
		mapByID:       map[int64]int{15130: 0},
		mapByPath:     map[string]int{"map/towerofdespair_down/15130despair001.map": 0},
		dungeonByID:   map[int64]int{11008: 0},
		dungeonByPath: map[string]int{"dungeon/towers/ztowerofdespair001.dgn": 0},
	}
	resolver, err := NewResolver(table)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := resolver.PoolCandidates(MapPoolRequest{
		DungeonID: 11008,
		MazeIndex: 0,
		FileType:  MapFileBoss,
	}); !errors.Is(err, ErrMapPoolEmpty) {
		t.Fatalf("fixture boss pool should be empty, got %v", err)
	}

	topology, err := BuildDungeonLayout(resolver, 11008, 0, nil)
	if err != nil {
		t.Fatalf("single start/boss layout should use normal owned pool: %v", err)
	}
	room, ok := topology.Room(RoomCoordinate{X: 0, Y: 0})
	if !ok || !room.Start || !room.Boss || room.Map == nil || room.Map.Map.ID != 15130 ||
		room.Map.Source != ResolutionTypePool {
		t.Fatalf("single start/boss room = %+v ok=%v", room, ok)
	}
	if len(topology.UnresolvedRooms()) != 0 || len(topology.resolutionErrors) != 0 {
		t.Fatalf("layout unresolved = %+v errors=%v", topology.UnresolvedRooms(), topology.resolutionErrors)
	}
}

func TestBuildDungeonLayoutSingleStartBossRoomUsesCoordinateOwnedStartMap(t *testing.T) {
	table := &Table{
		maps: []Map{{
			ID:         36250,
			Path:       "map/PoongjinTrainingRoom/11100(0,0)start.map",
			DungeonIDs: []int64{5000},
		}},
		dungeons: []Dungeon{{
			ID:   5000,
			Path: "dungeon/PoongjinTrainingRoom/PoongjinTrainingRoom.dgn",
			Mazes: []Maze{{
				Index:  0,
				Width:  OptionalInt{Value: 1, Set: true},
				Height: OptionalInt{Value: 1, Set: true},
				Greed:  "BB",
				Start:  &MazePoint{X: 0, Y: 0},
				Boss:   &MazePoint{X: 0, Y: 0},
			}},
		}},
		mapByID:       map[int64]int{36250: 0},
		mapByPath:     map[string]int{"map/poongjintrainingroom/11100(0,0)start.map": 0},
		dungeonByID:   map[int64]int{5000: 0},
		dungeonByPath: map[string]int{"dungeon/poongjintrainingroom/poongjintrainingroom.dgn": 0},
	}
	resolver, err := NewResolver(table)
	if err != nil {
		t.Fatal(err)
	}

	topology, err := BuildDungeonLayout(resolver, 5000, 0, nil)
	if err != nil {
		t.Fatalf("training-room layout should use its coordinate-owned start map: %v", err)
	}
	room, ok := topology.Room(RoomCoordinate{X: 0, Y: 0})
	if !ok || !room.Start || !room.Boss || room.Map == nil ||
		room.Map.Map.ID != 36250 || room.Map.Source != ResolutionDungeonOwnership {
		t.Fatalf("training-room start/boss room = %+v ok=%v", room, ok)
	}
	if len(topology.UnresolvedRooms()) != 0 || len(topology.resolutionErrors) != 0 {
		t.Fatalf("layout unresolved = %+v errors=%v", topology.UnresolvedRooms(), topology.resolutionErrors)
	}
}

func TestBuildDungeonLayoutKeepsUnassignedSpecialRoomsLazyButRequiresStartMap(t *testing.T) {
	newTable := func(includeStart bool) *Table {
		maps := []Map(nil)
		mapByID := map[int64]int{}
		mapByPath := map[string]int{}
		if includeStart {
			maps = append(maps, Map{
				ID: 11200, Path: "map/forgotten_land/forgotten_land_1.map",
				DungeonIDs: []int64{4000},
			})
			mapByID[11200] = 0
			mapByPath["map/forgotten_land/forgotten_land_1.map"] = 0
		}
		return &Table{
			maps: maps,
			dungeons: []Dungeon{{
				ID: 4000, Path: "dungeon/fun_test/fun_test2.dgn",
				Mazes: []Maze{{
					Index: 0, Width: OptionalInt{Value: 10, Set: true}, Height: OptionalInt{Value: 10, Set: true},
					Greed: "BBBB", Start: &MazePoint{X: 9, Y: 6}, Boss: &MazePoint{X: 9, Y: 0},
					MapSpecifications: []MapSpecification{{
						Type: "map", Coordinate: Point{X: 9, Y: 6}, MapIDs: []int64{11200},
					}},
				}},
			}},
			mapByID: mapByID, mapByPath: mapByPath,
			dungeonByID:   map[int64]int{4000: 0},
			dungeonByPath: map[string]int{"dungeon/fun_test/fun_test2.dgn": 0},
		}
	}

	resolver, err := NewResolver(newTable(true))
	if err != nil {
		t.Fatal(err)
	}
	topology, err := BuildDungeonLayout(resolver, 4000, 0, nil)
	if err != nil {
		t.Fatalf("logical unassigned boss must not reject entry: %v", err)
	}
	start, ok := topology.Room(RoomCoordinate{X: 9, Y: 6})
	if !ok || start.Map == nil || start.Map.Map.ID != 11200 {
		t.Fatalf("start=%+v ok=%t", start, ok)
	}
	boss, ok := topology.Room(RoomCoordinate{X: 9, Y: 0})
	if !ok || boss.Map != nil || boss.ResolutionError == "" {
		t.Fatalf("logical boss=%+v ok=%t", boss, ok)
	}
	if _, err := NewDungeonRun(topology); err != nil {
		t.Fatalf("entry run should require only the resolved start room: %v", err)
	}

	if _, err = NewResolver(newTable(false)); !errors.Is(err, ErrMapReferenceMissing) {
		t.Fatalf("missing explicit start reference should remain fatal, got %v", err)
	}
}

func TestBuildDungeonLayoutQuestStartUsesExactOwnedNormalCoordinate(t *testing.T) {
	table := &Table{
		maps: []Map{
			{ID: 1639, Path: "map/draconiantower/b_1639(0,1)n.map", DungeonIDs: []int64{11}},
			{ID: 1619, Path: "map/draconiantower/(0,0)b.map", DungeonIDs: []int64{11}},
		},
		dungeons: []Dungeon{{
			ID: 11, Path: "dungeon/act2/draconiantower.dgn",
			Mazes: []Maze{{
				Index: 0, Width: OptionalInt{Value: 1, Set: true}, Height: OptionalInt{Value: 2, Set: true},
				Greed: "AABB", Start: &MazePoint{X: 0, Y: 1}, Boss: &MazePoint{X: 0, Y: 0},
				QuestConnection:   []int64{0, 4307, 2},
				MapSpecifications: []MapSpecification{{Type: "boss", Coordinate: Point{X: 0, Y: 0}, MapIDs: []int64{1619}}},
			}},
		}},
		mapByID:       map[int64]int{1639: 0, 1619: 1},
		mapByPath:     map[string]int{"map/draconiantower/b_1639(0,1)n.map": 0, "map/draconiantower/(0,0)b.map": 1},
		dungeonByID:   map[int64]int{11: 0},
		dungeonByPath: map[string]int{"dungeon/act2/draconiantower.dgn": 0},
	}
	resolver, err := NewResolver(table)
	if err != nil {
		t.Fatal(err)
	}
	topology, err := BuildDungeonLayout(resolver, 11, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	start, ok := topology.Room(RoomCoordinate{X: 0, Y: 1})
	if !ok || start.Map == nil || start.Map.Map.ID != 1639 || start.Map.Source != ResolutionDungeonOwnership {
		t.Fatalf("quest start=%+v ok=%t", start, ok)
	}
}

func layoutTestTable() *Table {
	maps := []Map{
		{ID: 500, Path: "map/layout/500(0,0)start.map", DungeonIDs: []int64{800}},
		{ID: 501, Path: "map/layout/501(0,0)start.map", DungeonIDs: []int64{800}},
		{ID: 502, Path: "map/layout/b502.map", DungeonIDs: []int64{800}},
	}
	dungeons := []Dungeon{{
		ID: 800, Path: "dungeon/layout.dgn",
		Mazes: []Maze{{
			Index: 0, Width: OptionalInt{Value: 2, Set: true}, Height: OptionalInt{Value: 1, Set: true},
			Greed: "BBEE", Start: &MazePoint{X: 0, Y: 0}, Boss: &MazePoint{X: 1, Y: 0},
		}},
	}}
	table := &Table{
		maps: maps, dungeons: dungeons,
		mapByID: map[int64]int{500: 0, 501: 1, 502: 2},
		mapByPath: map[string]int{
			"map/layout/500(0,0)start.map": 0,
			"map/layout/501(0,0)start.map": 1,
			"map/layout/b502.map":          2,
		},
		dungeonByID:   map[int64]int{800: 0},
		dungeonByPath: map[string]int{"dungeon/layout.dgn": 0},
	}
	return table
}
