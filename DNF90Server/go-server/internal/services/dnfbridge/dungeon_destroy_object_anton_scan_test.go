package dnfbridge

import (
	"context"
	"fmt"
	"os"
	"testing"

	"longheng.io/server/internal/modules/dnf/worldmap"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

// TestRealScriptPVFNoAntonDungeonDeclaresDestroyObjectClearCondition is the
// red-line proof for the party-wide destroy-object clear
// (completeCurrentDungeonDestroyObjectClearForPartyLocked):
//
// the fan-out lives inside the `[clear condition][destroy object]` path, so it can
// only ever reach a dungeon that declares such a row. This test scans **every**
// dungeon of the shipped Script.pvf and asserts that none of Anton's raid
// objective dungeons does. 安徒恩的原生团队目标表是 210..224
// (dungeon_runtime_state.go:59-60: "the current client keeps its native Raid
// objective table under 210-224").
//
// Run it with the real archive:
//
//	DNF_WORLDMAP_REAL_PVF_SMOKE=<Script.pvf> go test ./internal/services/dnfbridge/ \
//	    -run TestRealScriptPVFNoAntonDungeonDeclaresDestroyObjectClearCondition -v
func TestRealScriptPVFNoAntonDungeonDeclaresDestroyObjectClearCondition(t *testing.T) {
	pvfPath := os.Getenv("DNF_WORLDMAP_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNF_WORLDMAP_REAL_PVF_SMOKE to scan every dungeon for a [destroy object] clear condition")
	}
	archive, err := platformpvf.Open(pvfPath)
	if err != nil {
		t.Fatal(err)
	}
	table, err := worldmap.LoadSource(context.Background(), archive, worldmap.Options{})
	if err != nil {
		t.Fatal(err)
	}

	// Anton's native raid objective table. Everything in this range is reachable
	// by an Anton raid gate, so a [destroy object] row in it would let the party
	// fan-out fire for Anton.
	const (
		antonRaidFirstDungeonID = int64(210)
		antonRaidLastDungeonID  = int64(224)
	)

	scanned := 0
	hits := make([]string, 0, 8)
	antonHits := make([]string, 0, 1)
	for _, dungeon := range table.Dungeons() {
		scanned++
		for mazeIndex, maze := range dungeon.Mazes {
			for _, condition := range maze.ClearConditions {
				if normalizeDungeonPVFSymbol(condition.Type) != "destroy object" {
					continue
				}
				hit := fmt.Sprintf("dungeon=%d maze=%d target=%d count=%d",
					dungeon.ID, mazeIndex, condition.TargetID, condition.Count)
				hits = append(hits, hit)
				if dungeon.ID >= antonRaidFirstDungeonID && dungeon.ID <= antonRaidLastDungeonID {
					antonHits = append(antonHits, hit)
					t.Errorf("Anton raid dungeon %d declares a [destroy object] clear condition (%s): "+
						"the party-wide destroy-object clear would reach Anton", dungeon.ID, hit)
				}
			}
		}
	}
	if scanned == 0 {
		t.Fatal("the real archive exposed no dungeons at all")
	}
	if len(antonHits) != 0 {
		t.Fatalf("Anton is not isolated: %v", antonHits)
	}
	// The Luke phase-two energy rooms are the reason this path exists at all; if
	// this list ever loses them the test above stops proving anything useful.
	if len(hits) == 0 {
		t.Fatal("no [destroy object] clear condition found anywhere: the scan is not exercising the path")
	}
	t.Logf("scanned %d dungeons; %d [destroy object] clear condition(s), none of them Anton: %v",
		scanned, len(hits), hits)
}
