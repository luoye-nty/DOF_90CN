package worldmap

import (
	"context"
	"os"
	"testing"

	platformpvf "longheng.io/server/internal/platform/pvf"
)

// Luke's phase-one sanctums (dungeon/luke/phase1/sanctum_of_*.dgn) declare
// [coin limit] 8 after their last [maze info]; it must land on the dungeon.
func TestRealScriptPVFLukeSanctumCoinLimit(t *testing.T) {
	pvfPath := os.Getenv("DNF_WORLDMAP_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNF_WORLDMAP_REAL_PVF_SMOKE to verify dungeon [coin limit]")
	}
	archive, err := platformpvf.LoadArchive(platformpvf.Options{Path: pvfPath})
	if err != nil {
		t.Fatal(err)
	}
	table, err := LoadSource(context.Background(), archive, Options{SkipMaps: true, SkipAreas: true, CompactRuntime: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, dungeonID := range []int64{5054, 5055, 5056} {
		dungeon, found := table.FindDungeon(dungeonID)
		if !found {
			t.Fatalf("dungeon %d is missing", dungeonID)
		}
		if !dungeon.Metadata.CoinLimit.Set || dungeon.Metadata.CoinLimit.Value != 8 {
			t.Fatalf("dungeon %d coin limit=%+v want 8", dungeonID, dungeon.Metadata.CoinLimit)
		}
	}
}
