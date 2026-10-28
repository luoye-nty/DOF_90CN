package dnfbridge

import (
	"context"
	"os"
	"testing"

	"longheng.io/server/internal/modules/dnf/worldmap"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

func TestRealScriptPVFSpecialDungeonDropsAreDungeonWide(t *testing.T) {
	pvfPath := os.Getenv("DNF_WORLDMAP_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNF_WORLDMAP_REAL_PVF_SMOKE to audit coloured special-dungeon drops")
	}
	archive, err := platformpvf.Open(pvfPath)
	if err != nil {
		t.Fatal(err)
	}
	table, err := worldmap.LoadSource(context.Background(), archive, worldmap.Options{})
	if err != nil {
		t.Fatal(err)
	}
	resolver, err := worldmap.NewResolver(table)
	if err != nil {
		t.Fatal(err)
	}
	monsters, err := newPVFDungeonMonsterCatalog(archive)
	if err != nil {
		t.Fatal(err)
	}

	// 2005 has the coloured special-dungeon flag but no passive-object reward
	// table in the active PVF. Every other listed dungeon carries one trailing
	// table which must apply to every selectable maze.
	for _, dungeonID := range []int64{2006, 2007, 2008, 2009, 2010, 2011, 2012, 2013, 2014, 2016, 315} {
		dungeon, found := table.FindDungeon(dungeonID)
		if !found {
			t.Fatalf("special dungeon=%d missing", dungeonID)
		}
		if _, special := dungeon.Metadata.Flags["special dungeon"]; !special {
			t.Fatalf("dungeon=%d missing active-PVF [special dungeon] flag", dungeonID)
		}
		if len(dungeon.Metadata.SpecialPassiveObjectItems) == 0 {
			t.Fatalf("dungeon=%d lost its dungeon-wide passive-object reward table", dungeonID)
		}
		for mazeIndex, maze := range dungeon.Mazes {
			if len(maze.SpecialPassiveObjectItems) != 0 {
				t.Fatalf("dungeon=%d maze=%d retained route-local rewards=%+v", dungeonID, mazeIndex, maze.SpecialPassiveObjectItems)
			}
			layout, layoutErr := worldmap.BuildDungeonLayout(resolver, dungeon.ID, mazeIndex, func(choice worldmap.DungeonMapChoice) (int64, error) {
				return choice.Candidates[0].ID, nil
			})
			if layoutErr != nil {
				t.Fatalf("dungeon=%d maze=%d layout: %v", dungeonID, mazeIndex, layoutErr)
			}
			configuredGroups := make(map[int64]struct{})
			for _, row := range dungeon.Metadata.SpecialPassiveObjectItems {
				if row.ItemID > 0 && row.DropRate > 0 {
					configuredGroups[row.Index] = struct{}{}
				}
			}
			foundOwner := false
			for _, room := range layout.Rooms() {
				if room.Map == nil {
					continue
				}
				owners, ownersErr := currentDungeonPassiveObjectGroupOwners(worldmap.DungeonRoomScene{
					Coordinate:            room.Coordinate,
					Map:                   *room.Map,
					PassiveObjects:        room.Map.Map.PassiveObjects,
					SpecialPassiveObjects: room.Map.Map.SpecialPassiveObjects,
				}, monsters)
				if ownersErr != nil {
					t.Fatalf("dungeon=%d maze=%d map=%d owners: %v", dungeonID, mazeIndex, room.Map.Map.ID, ownersErr)
				}
				for group := range configuredGroups {
					if len(owners[group]) > 0 {
						foundOwner = true
					}
				}
			}
			if !foundOwner {
				t.Fatalf("dungeon=%d maze=%d has configured groups=%v but no route object owns one", dungeonID, mazeIndex, configuredGroups)
			}
		}
	}
}
