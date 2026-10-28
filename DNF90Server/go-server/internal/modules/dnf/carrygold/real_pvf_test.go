package carrygold

import (
	"context"
	"os"
	"testing"

	platformpvf "longheng.io/server/internal/platform/pvf"
)

// TestRealScriptPVFGoldLimitByLevel is the evidence behind the base carry cap.
// The runtime PVF really does carry `[gold limit from level]`, so the base
// limit is level-keyed data and must never be re-hardcoded as 4亿; only the
// per-stage 亿 and the flat StageCost come from the client.
func TestRealScriptPVFGoldLimitByLevel(t *testing.T) {
	pvfPath := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE to verify the carry-gold level table")
	}
	archive, err := platformpvf.LoadArchive(platformpvf.Options{Path: pvfPath})
	if err != nil {
		t.Fatal(err)
	}
	limits, err := LoadLevelLimits(context.Background(), archive)
	if err != nil {
		t.Fatal(err)
	}
	if limits.Len() < 90 {
		t.Fatalf("runtime %s has only %d level rows", LevelLimitPath, limits.Len())
	}
	// The level-60 plateau is what makes the client's confirm dialog read
	// 4亿 -> 5亿 for a level-capped character.
	for level, want := range map[int]int64{
		0: 0, 1: 100_000, 20: 30_000_000, 30: 100_000_000,
		60: 400_000_000, 90: 400_000_000, 99: 400_000_000,
	} {
		if got := limits.Limit(level); got != want {
			t.Fatalf("runtime level=%d limit=%d want %d", level, got, want)
		}
	}
	if base := limits.Limit(90); base+StageBonus(MaxStage) != int64(DisplayedLimit(MaxStage))*Unit {
		t.Fatalf("level cap base=%d + max stage bonus does not match the client display", base)
	}
}
