package dnfbridge

import (
	"context"
	"os"
	"testing"

	"longheng.io/server/internal/modules/dnf/dungeoncmd"
	"longheng.io/server/internal/modules/dnf/worldmap"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

func TestRealScriptPVFSelectedMazeEscapeHellOwnership(t *testing.T) {
	pvfPath := os.Getenv("DNF_WORLDMAP_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNF_WORLDMAP_REAL_PVF_SMOKE to verify dungeon 7542 escape-hell maze ownership")
	}
	archive, err := platformpvf.LoadArchive(platformpvf.Options{Path: pvfPath})
	if err != nil {
		t.Fatal(err)
	}
	table, err := worldmap.LoadSource(context.Background(), archive, worldmap.Options{SkipMaps: true})
	if err != nil {
		t.Fatal(err)
	}

	dungeon, found := table.FindDungeon(7542)
	if !found {
		t.Fatal("escape-hell dungeon 7542 is missing")
	}
	if got := dungeon.Metadata.IntegerValues["hell dungeon"]; got != 1 {
		t.Fatalf("dungeon 7542 hell-dungeon marker=%d want 1", got)
	}
	if len(dungeon.Mazes) != 6 {
		t.Fatalf("dungeon 7542 mazes=%d want 6", len(dungeon.Mazes))
	}

	request := dungeoncmd.SelectDungeonRequest{
		DungeonID:     7542,
		Difficulty:    2,
		SelectionMode: 1,
		RuntimeToken:  0xffff,
	}
	for mazeIndex, maze := range dungeon.Mazes {
		if !maze.EscapeHell.Set || maze.EscapeHell.Value != 1 {
			t.Fatalf("dungeon 7542 maze=%d escape-hell=%+v want 1", mazeIndex, maze.EscapeHell)
		}
		if !currentDungeonSelectedMazeEscapeHell(request, dungeon, mazeIndex) {
			t.Fatalf("dungeon 7542 maze=%d was not classified as selected escape hell", mazeIndex)
		}
		runtime := &runtimeDungeonState{Request: request, Dungeon: dungeon, MazeIndex: mazeIndex}
		if !currentDungeonStandaloneRiftRuntime(runtime) {
			t.Fatalf("dungeon 7542 maze=%d did not retain boss-owned standalone rewards", mazeIndex)
		}
		if !currentDungeonTimeRiftRuntime(runtime) {
			t.Fatalf("dungeon 7542 maze=%d was excluded from the time-rift-only route", mazeIndex)
		}
		if currentDungeonExplicitHellPartyRequest(request, dungeon, mazeIndex) {
			t.Fatalf("dungeon 7542 maze=%d was rewritten into an embedded seal-door hell party", mazeIndex)
		}
	}

	magicRift, found := table.FindDungeon(316)
	if !found {
		t.Fatal("Magic Rift dungeon 316 is missing")
	}
	magicRequest := request
	magicRequest.DungeonID = 316
	magicRuntime := &runtimeDungeonState{Request: magicRequest, Dungeon: magicRift, MazeIndex: 0}
	if !currentDungeonStandaloneRiftRuntime(magicRuntime) || currentDungeonTimeRiftRuntime(magicRuntime) ||
		currentDungeonSelectedMazeEscapeHell(magicRequest, magicRift, 0) ||
		currentDungeonExplicitHellPartyRequest(magicRequest, magicRift, 0) {
		t.Fatalf("Magic Rift isolation changed: metadata=%+v maze=%+v", magicRift.Metadata, magicRift.Mazes[0])
	}
}
