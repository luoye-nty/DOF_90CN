package worldmap

import (
	"context"
	"os"
	"testing"

	platformpvf "longheng.io/server/internal/platform/pvf"
)

func TestRealScriptPVFDungeon7542EscapeHellBelongsToEachMaze(t *testing.T) {
	pvfPath := os.Getenv("DNF_WORLDMAP_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNF_WORLDMAP_REAL_PVF_SMOKE to verify dungeon 7542 escape-hell maze ownership")
	}
	archive, err := platformpvf.LoadArchive(platformpvf.Options{Path: pvfPath})
	if err != nil {
		t.Fatal(err)
	}
	table, err := LoadSource(context.Background(), archive, Options{SkipMaps: true})
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
	if _, leaked := dungeon.Metadata.IntegerValues["escape hell"]; leaked {
		t.Fatalf("maze escape-hell leaked into dungeon metadata: %+v", dungeon.Metadata.IntegerValues)
	}
	if len(dungeon.Mazes) != 6 {
		t.Fatalf("dungeon 7542 mazes=%d want 6", len(dungeon.Mazes))
	}
	for mazeIndex, maze := range dungeon.Mazes {
		if !maze.EscapeHell.Set || maze.EscapeHell.Value != 1 {
			t.Fatalf("dungeon 7542 maze=%d escape-hell=%+v want 1", mazeIndex, maze.EscapeHell)
		}
	}
}
