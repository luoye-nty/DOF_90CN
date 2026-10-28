package dnfbridge

import (
	"context"
	"os"
	"testing"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	"longheng.io/server/internal/modules/dnf/worldmap"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

func TestRealScriptPVFTimePlazaIsRecommendedAtLevel90(t *testing.T) {
	pvfPath := os.Getenv("DNF_WORLDMAP_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNF_WORLDMAP_REAL_PVF_SMOKE to verify Time Plaza suitable-level rewards")
	}
	archive, err := platformpvf.Open(pvfPath)
	if err != nil {
		t.Fatal(err)
	}
	table, err := worldmap.LoadSource(context.Background(), archive, worldmap.Options{})
	if err != nil {
		t.Fatal(err)
	}
	dungeon, found := table.FindDungeon(310)
	if !found {
		t.Fatal("real PVF dungeon 310 is missing")
	}
	if len(dungeon.Metadata.RecommendedLevels) != 0 ||
		!dungeon.Metadata.MinimumRequiredLevel.Set || dungeon.Metadata.MinimumRequiredLevel.Value != 87 ||
		!dungeon.Metadata.BasisLevel.Set || dungeon.Metadata.BasisLevel.Value != 90 {
		t.Fatalf("real dungeon310 level metadata=%+v", dungeon.Metadata)
	}
	runtime := &runtimeDungeonState{
		Dungeon:                        dungeon,
		MazeIndex:                      0,
		Character:                      dnfrepo.CharacterRecord{Level: 90},
		ordinaryFinalRoomClearAccepted: true,
	}
	if !currentDungeonIsRecommendedClear(runtime) {
		t.Fatal("real PVF dungeon310 ordinary level-90 clear was not recommended")
	}
	runtime.Character.Level = 91
	if currentDungeonIsRecommendedClear(runtime) {
		t.Fatal("real PVF dungeon310 accepted level 91 above its basis level")
	}
}

func TestRealScriptPVFStoryMazeIsRecommendedAtSuitableLevel(t *testing.T) {
	pvfPath := os.Getenv("DNF_WORLDMAP_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNF_WORLDMAP_REAL_PVF_SMOKE to verify story-maze suitable-level rewards")
	}
	archive, err := platformpvf.Open(pvfPath)
	if err != nil {
		t.Fatal(err)
	}
	table, err := worldmap.LoadSource(context.Background(), archive, worldmap.Options{})
	if err != nil {
		t.Fatal(err)
	}
	dungeon, found := table.FindDungeon(3)
	if !found {
		t.Fatal("real PVF dungeon 3 is missing")
	}
	storyMazeIndex := -1
	for index, maze := range dungeon.Mazes {
		if len(maze.QuestConnection) >= 2 && maze.QuestConnection[1] > 0 {
			storyMazeIndex = index
			break
		}
	}
	if storyMazeIndex < 0 {
		t.Fatal("real PVF dungeon 3 has no quest-connected story maze")
	}
	level := int64(0)
	switch {
	case len(dungeon.Metadata.RecommendedLevels) >= 2:
		level = dungeon.Metadata.RecommendedLevels[0]
	case dungeon.Metadata.MinimumRequiredLevel.Set && dungeon.Metadata.BasisLevel.Set:
		level = dungeon.Metadata.BasisLevel.Value
	}
	if level <= 0 || level > int64(^uint(0)>>1) || dungeon.Metadata.NoFatigue {
		t.Fatalf("real story dungeon eligibility metadata=%+v", dungeon.Metadata)
	}
	runtime := &runtimeDungeonState{
		Dungeon:                        dungeon,
		MazeIndex:                      storyMazeIndex,
		Character:                      dnfrepo.CharacterRecord{Level: int(level)},
		ordinaryFinalRoomClearAccepted: true,
	}
	if !currentDungeonIsRecommendedClear(runtime) {
		t.Fatalf("real dungeon 3 story maze=%d level=%d was not recommended", storyMazeIndex, level)
	}
}
