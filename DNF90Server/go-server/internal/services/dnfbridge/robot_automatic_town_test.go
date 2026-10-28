package dnfbridge

import (
	"context"
	"os"
	"testing"

	dnftown "longheng.io/server/internal/modules/dnf/town"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

func TestAutomaticRobotPublicTownPlacementsDeduplicateAndIncludeDungeonGates(t *testing.T) {
	towns := []dnftown.Town{
		{ID: 38, Areas: []dnftown.Area{
			{ID: 0, Kind: "normal", MapPath: "Town/Public.map", NPCs: []dnftown.NPCSpawn{{NPCID: 1, X: 500, Y: 300}}},
			{ID: 2, Kind: "normal", MapPath: "town/public.map", NPCs: []dnftown.NPCSpawn{{NPCID: 2, X: 600, Y: 300}}},
			{ID: 3, Kind: "dungeon_gate", MapPath: "Town/Dungeon_Entrance.map", NPCs: []dnftown.NPCSpawn{{NPCID: 3, X: 700, Y: 320}}},
			{ID: 4, Kind: "gate", MapPath: "Town/Seria.map", NPCs: []dnftown.NPCSpawn{{NPCID: 4, X: 1, Y: 1}}},
		}},
		{ID: 8, Areas: []dnftown.Area{{ID: 0, Kind: "normal", MapPath: "GuildAgit/private.map", NPCs: []dnftown.NPCSpawn{{NPCID: 5, X: 1, Y: 1}}}}},
	}
	placements := automaticRobotPublicTownPlacements(towns)
	if len(placements) != 2 {
		t.Fatalf("placements=%+v, want one public map and one dungeon entrance", placements)
	}
	if placements[0].MapPath != "town/public.map" || !placements[0].StoreArea ||
		placements[1].MapPath != "town/dungeon_entrance.map" || !placements[1].DungeonGate || placements[1].StoreArea {
		t.Fatalf("placements=%+v", placements)
	}
}

func TestAutomaticRobotNPCCorridorKeepsTheDominantFloor(t *testing.T) {
	minX, maxX, minY, maxY, ok := automaticRobotNPCCorridor([]dnftown.NPCSpawn{
		{NPCID: 1, X: 800, Y: 155}, {NPCID: 2, X: 1600, Y: 156}, {NPCID: 3, X: 2400, Y: 160},
		{NPCID: 4, X: 900, Y: 350}, {NPCID: 5, X: 1000, Y: 405},
	})
	if !ok {
		t.Fatal("dominant NPC floor was not resolved")
	}
	if minX != 824 || maxX != 2376 || minY < 149 || maxY > 163 {
		t.Fatalf("corridor=(%d..%d,%d..%d), want dominant y=155..160 floor", minX, maxX, minY, maxY)
	}
}

func TestAutomaticRobotTownPlacementStaysInsideItsSafeCorridor(t *testing.T) {
	placements := automaticRobotPublicTownPlacements([]dnftown.Town{{
		ID: 38, Areas: []dnftown.Area{{ID: 0, Kind: "normal", MapPath: "town/public.map",
			NPCs: []dnftown.NPCSpawn{{NPCID: 1, X: 500, Y: 200}, {NPCID: 2, X: 900, Y: 205}}}},
	}})
	if len(placements) != 1 {
		t.Fatalf("placements=%+v", placements)
	}
	base := placements[0]
	for index := 1; index <= 100; index++ {
		x := automaticRobotCoordinateInRange(base.MinX, base.MaxX, uint64(index)*53+automaticRobotSeed(index))
		y := automaticRobotCoordinateInRange(base.MinY, base.MaxY, uint64(index)*7+(automaticRobotSeed(index)>>24))
		if x < base.MinX || x > base.MaxX || y < base.MinY || y > base.MaxY {
			t.Fatalf("placement %d=(%d,%d) is outside %+v", index, x, y, base)
		}
	}
}

func TestAutomaticRobotRealPVFPublicTownDistribution(t *testing.T) {
	pvfPath := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE to verify automatic robot town distribution")
	}
	archive, err := platformpvf.LoadArchive(platformpvf.Options{Path: pvfPath})
	if err != nil {
		t.Fatalf("load Script.pvf: %v", err)
	}
	table, err := dnftown.Load(context.Background(), archive, dnftown.Options{})
	if err != nil {
		t.Fatalf("load town catalog: %v", err)
	}
	placements := automaticRobotPublicTownPlacements(table.Towns())
	entrances := 0
	storeAreas := 0
	hendonMainSafe, westCoastMainSafe := false, false
	for _, placement := range placements {
		if placement.DungeonGate {
			entrances++
		}
		if placement.StoreArea {
			storeAreas++
		}
		if placement.X < placement.MinX || placement.X > placement.MaxX || placement.Y < placement.MinY || placement.Y > placement.MaxY {
			t.Fatalf("base placement outside corridor: %+v", placement)
		}
		if placement.Village == 39 && placement.Area == 0 {
			hendonMainSafe = placement.StoreArea && placement.MinY >= 140 && placement.MaxY <= 180
		}
		if placement.Village == 40 && placement.Area == 0 {
			westCoastMainSafe = placement.StoreArea && placement.MinY >= 140 && placement.MaxY <= 170
		}
	}
	if len(placements) < 25 || len(placements) > 50 || entrances == 0 {
		t.Fatalf("public placements=%d dungeon entrances=%d", len(placements), entrances)
	}
	if storeAreas < 10 {
		t.Fatalf("store areas=%d, want populated public hubs", storeAreas)
	}
	if !hendonMainSafe || !westCoastMainSafe {
		t.Fatalf("main hub corridors hendon=%t west_coast=%t", hendonMainSafe, westCoastMainSafe)
	}
	service := &Service{townCatalog: table}
	for index := 1; index <= 500; index++ {
		placement := service.automaticRobotTownPlacement(index)
		if placement.X < placement.MinX || placement.X > placement.MaxX || placement.Y < placement.MinY || placement.Y > placement.MaxY {
			t.Fatalf("fleet placement %d outside corridor: %+v", index, placement)
		}
	}
	t.Logf("public unique maps=%d dungeon entrances=%d robots/map=%d..%d", len(placements), entrances, 500/len(placements), (500+len(placements)-1)/len(placements))
}
