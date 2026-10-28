package dnfbridge

import (
	"context"
	"os"
	"testing"

	"longheng.io/server/internal/modules/dnf/worldmap"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

func TestZZProbeTowerIdentityFromRealPVF(t *testing.T) {
	path := os.Getenv("DNF_WORLDMAP_REAL_PVF_SMOKE")
	if path == "" {
		t.Skip("set DNF_WORLDMAP_REAL_PVF_SMOKE")
	}
	archive, err := platformpvf.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	table, err := worldmap.LoadSource(context.Background(), archive, worldmap.Options{CompactRuntime: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{11008, 11017, 11107, 5115, 5214} {
		d, ok := table.FindDungeon(id)
		if !ok {
			t.Logf("dungeon %d: NOT INDEXED", id)
			continue
		}
		var towers []string
		for _, s := range d.SourceSections {
			n := normalizeDungeonPVFSymbol(s.Name)
			if n == "tower of despair" || n == "tower of grief" {
				towers = append(towers, s.Name)
			}
		}
		t.Logf("dungeon %d: despairFlag=%v griefFlag=%v isDespair=%v towers=%v msgID=%v kind=%q",
			id, d.Metadata.Flags["tower of despair"], d.Metadata.Flags["tower of grief"],
			currentDungeonIsTowerOfDespair(d), towers,
			currentDungeonTowerRewardMsgID(&runtimeDungeonState{Dungeon: d}),
			currentDungeonTowerKind(&runtimeDungeonState{Dungeon: d}))
	}
}
