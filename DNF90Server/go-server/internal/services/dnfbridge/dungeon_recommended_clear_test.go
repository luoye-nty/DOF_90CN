package dnfbridge

import (
	"testing"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	"longheng.io/server/internal/modules/dnf/worldmap"
)

func TestCurrentDungeonIsRecommendedClearUsesPVFRangeAndAcceptedOrdinaryClear(t *testing.T) {
	runtime := &runtimeDungeonState{
		Dungeon: worldmap.Dungeon{
			ID:    11,
			Mazes: []worldmap.Maze{{}},
			Metadata: worldmap.DungeonMetadata{
				RecommendedLevels: []int64{85, 90},
			},
		},
		Character:                      dnfrepo.CharacterRecord{Level: 90},
		ordinaryFinalRoomClearAccepted: true,
	}
	if !currentDungeonIsRecommendedClear(runtime) {
		t.Fatal("accepted level-90 clear in PVF range was not recommended")
	}
	runtime.Character.Level = 91
	if currentDungeonIsRecommendedClear(runtime) {
		t.Fatal("out-of-range clear was recommended")
	}
	runtime.Character.Level = 90
	runtime.ordinaryFinalRoomClearAccepted = false
	if currentDungeonIsRecommendedClear(runtime) {
		t.Fatal("unaccepted clear was recommended")
	}
}

func TestCurrentDungeonIsRecommendedClearFallsBackToPVFEntryAndBasisLevels(t *testing.T) {
	runtime := &runtimeDungeonState{
		Dungeon: worldmap.Dungeon{
			ID:    310,
			Mazes: []worldmap.Maze{{}},
			Metadata: worldmap.DungeonMetadata{
				MinimumRequiredLevel: worldmap.OptionalInt{Set: true, Value: 87},
				BasisLevel:           worldmap.OptionalInt{Set: true, Value: 90},
			},
		},
		Character:                      dnfrepo.CharacterRecord{Level: 90},
		ordinaryFinalRoomClearAccepted: true,
	}
	if !currentDungeonIsRecommendedClear(runtime) {
		t.Fatal("ordinary level-90 clear did not use the PVF 87..90 fallback")
	}
	for _, level := range []int{86, 91} {
		runtime.Character.Level = level
		if currentDungeonIsRecommendedClear(runtime) {
			t.Fatalf("out-of-range level %d was recommended", level)
		}
	}
}

func TestCurrentDungeonIsRecommendedClearAcceptsQuestConnectedStoryMaze(t *testing.T) {
	runtime := &runtimeDungeonState{
		Dungeon: worldmap.Dungeon{
			ID:    310,
			Mazes: []worldmap.Maze{{QuestConnection: []int64{0, 3946, -1}}},
			Metadata: worldmap.DungeonMetadata{
				MinimumRequiredLevel: worldmap.OptionalInt{Set: true, Value: 87},
				BasisLevel:           worldmap.OptionalInt{Set: true, Value: 90},
			},
		},
		Character:                      dnfrepo.CharacterRecord{Level: 90},
		ordinaryFinalRoomClearAccepted: true,
	}
	if !currentDungeonIsRecommendedClear(runtime) {
		t.Fatal("accepted suitable-level story-maze clear was not recommended")
	}
}

func TestCurrentDungeonIsRecommendedClearRejectsIneligibleRoutes(t *testing.T) {
	base := runtimeDungeonState{
		Dungeon: worldmap.Dungeon{
			ID:    310,
			Mazes: []worldmap.Maze{{}},
			Metadata: worldmap.DungeonMetadata{
				MinimumRequiredLevel: worldmap.OptionalInt{Set: true, Value: 87},
				BasisLevel:           worldmap.OptionalInt{Set: true, Value: 90},
			},
		},
		Character:                      dnfrepo.CharacterRecord{Level: 90},
		ordinaryFinalRoomClearAccepted: true,
	}
	for _, test := range []struct {
		name   string
		mutate func(*runtimeDungeonState)
	}{
		{name: "practice", mutate: func(runtime *runtimeDungeonState) {
			runtime.ProtocolMode = currentDungeonProtocolModePractice
		}},
		{name: "no fatigue", mutate: func(runtime *runtimeDungeonState) {
			runtime.Dungeon.Metadata.NoFatigue = true
		}},
		{name: "tournament", mutate: func(runtime *runtimeDungeonState) {
			runtime.tournament = &worldmap.TournamentRuntime{}
		}},
		{name: "unresolved maze", mutate: func(runtime *runtimeDungeonState) {
			runtime.MazeIndex = len(runtime.Dungeon.Mazes)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			runtime := base
			runtime.Dungeon.Mazes = append([]worldmap.Maze(nil), base.Dungeon.Mazes...)
			runtime.Dungeon.Mazes[0].QuestConnection = append([]int64(nil), base.Dungeon.Mazes[0].QuestConnection...)
			test.mutate(&runtime)
			if currentDungeonIsRecommendedClear(&runtime) {
				t.Fatal("non-ordinary clear was recommended")
			}
		})
	}
}

func TestCurrentDungeonMatchesRecommendedLevelRejectsMalformedPVFRange(t *testing.T) {
	runtime := &runtimeDungeonState{
		Dungeon: worldmap.Dungeon{
			ID: 11,
			Metadata: worldmap.DungeonMetadata{
				RecommendedLevels: []int64{90, 85},
			},
		},
		Character: dnfrepo.CharacterRecord{Level: 90},
	}
	if currentDungeonMatchesRecommendedLevel(runtime) {
		t.Fatal("reversed PVF range matched")
	}
	runtime.Dungeon.Metadata.RecommendedLevels = []int64{90}
	if currentDungeonMatchesRecommendedLevel(runtime) {
		t.Fatal("incomplete PVF range matched")
	}
	runtime.Dungeon.Metadata.MinimumRequiredLevel = worldmap.OptionalInt{Set: true, Value: 85}
	runtime.Dungeon.Metadata.BasisLevel = worldmap.OptionalInt{Set: true, Value: 90}
	if currentDungeonMatchesRecommendedLevel(runtime) {
		t.Fatal("malformed explicit range silently fell back to entry levels")
	}
}
