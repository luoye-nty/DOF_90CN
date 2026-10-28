package dnfbridge

import (
	"testing"

	"longheng.io/server/internal/modules/dnf/worldmap"
)

func TestRealScriptPVFFruzStationQuest3733OwnsNarkeAndGrenadeObstacles(t *testing.T) {
	_, table, _ := loadRealStoryPVF(t)
	dungeon, found := table.FindDungeon(currentFruzStationDungeonID)
	if !found {
		t.Fatalf("real dungeon %d is missing", currentFruzStationDungeonID)
	}
	questMazeFound := false
	for _, maze := range dungeon.Mazes {
		if realMazeConnectsQuest(maze, currentFruzStationNarkeQuestID) {
			questMazeFound = true
			break
		}
	}
	if !questMazeFound {
		t.Fatalf("real dungeon %d has no quest %d maze", currentFruzStationDungeonID, currentFruzStationNarkeQuestID)
	}

	startMap, found := table.FindMap(76500)
	if !found {
		t.Fatal("real Fruz quest start map 76500 is missing")
	}
	narkeFound := false
	for _, actor := range startMap.AICharacters {
		if actor.Code == currentFruzStationNarkeActorID &&
			normalizeDungeonPVFSymbol(actor.Faction) == "character" &&
			normalizeDungeonPVFSymbol(actor.AIType) == "normal" {
			narkeFound = true
			break
		}
	}
	if !narkeFound {
		t.Fatalf("real map 76500 AI characters=%+v, want friendly normal Narke %d", startMap.AICharacters, currentFruzStationNarkeActorID)
	}

	wantCounts := map[int64]map[int64]uint32{
		76503: {currentFruzStationObstacleSmall: 1, currentFruzStationObstacleLarge: 5},
		76504: {currentFruzStationObstacleSmall: 1, currentFruzStationObstacleLarge: 5},
		76506: {currentFruzStationObstacleSmall: 1, currentFruzStationObstacleLarge: 5},
	}
	for mapID, want := range wantCounts {
		mapValue, ok := table.FindMap(mapID)
		if !ok {
			t.Fatalf("real Fruz obstacle map %d is missing", mapID)
		}
		scene := worldmap.DungeonRoomScene{
			Map:                   worldmap.ResolvedMap{Map: mapValue},
			PassiveObjects:        mapValue.PassiveObjects,
			SpecialPassiveObjects: mapValue.SpecialPassiveObjects,
		}
		for objectID, wantCount := range want {
			if got := currentDungeonScenePassiveObjectCount(scene, objectID); got != wantCount {
				t.Fatalf("real map %d obstacle %d count=%d want=%d", mapID, objectID, got, wantCount)
			}
		}
	}
}
