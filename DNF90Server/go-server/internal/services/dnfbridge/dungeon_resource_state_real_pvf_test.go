package dnfbridge

import (
	"context"
	"os"
	"testing"

	"longheng.io/server/internal/modules/dnf/worldmap"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

func TestRealScriptPVFOrdinaryLukeAndAntonFirstClearsPersist(t *testing.T) {
	pvfPath := os.Getenv("DNF_WORLDMAP_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNF_WORLDMAP_REAL_PVF_SMOKE to verify ordinary Luke and Anton clear state")
	}
	archive, err := platformpvf.Open(pvfPath)
	if err != nil {
		t.Fatal(err)
	}
	table, err := worldmap.LoadSource(context.Background(), archive, worldmap.Options{})
	if err != nil {
		t.Fatal(err)
	}

	dungeonIDs := []int64{
		5101, 5102, 5103, 5104, 5105, // ordinary Luke sequence 42 prerequisites
		227, 233, 235, 236, 238, 1005, 1006, 1007, // ordinary Anton route
	}
	for _, dungeonID := range dungeonIDs {
		dungeon, found := table.FindDungeon(dungeonID)
		if !found {
			t.Fatalf("real PVF ordinary dungeon %d is missing", dungeonID)
		}
		if got := currentDungeonNextClearState(dungeon, 0); got != 1 {
			t.Errorf("real PVF ordinary dungeon %d normal clear state=%d want 1", dungeonID, got)
		}
	}
}

func TestRealScriptPVFSequentialLukeAndAntonDefinitions(t *testing.T) {
	pvfPath := os.Getenv("DNF_WORLDMAP_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNF_WORLDMAP_REAL_PVF_SMOKE to verify sequential-dungeon definitions")
	}
	archive, err := platformpvf.Open(pvfPath)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := loadCurrentSequentialDungeonCatalog(archive)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		sequenceID uint32
		want       []uint32
	}{
		{sequenceID: 28, want: []uint32{243, 244, 245, 246, 247}},
		{sequenceID: 42, want: []uint32{5101, 5102, 5103, 5104, 5105, 5106}},
	}
	for _, test := range tests {
		definition, found := catalog.Find(test.sequenceID)
		if !found || len(definition.DungeonIDs) != len(test.want) {
			t.Fatalf("real PVF sequence=%d found=%t definition=%+v want=%v", test.sequenceID, found, definition, test.want)
		}
		for index := range test.want {
			if definition.DungeonIDs[index] != test.want[index] {
				t.Fatalf("real PVF sequence=%d dungeon[%d]=%d want=%d", test.sequenceID, index, definition.DungeonIDs[index], test.want[index])
			}
		}
	}
}
