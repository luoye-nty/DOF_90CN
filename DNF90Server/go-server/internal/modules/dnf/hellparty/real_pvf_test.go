package hellparty

import (
	"context"
	"os"
	"testing"

	"longheng.io/server/internal/modules/dnf/worldmap"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

func TestRealPVFCatalog(t *testing.T) {
	path := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if path == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE to verify the runtime Hell Party catalog")
	}
	archive, err := platformpvf.Open(path)
	if err != nil {
		t.Fatalf("Open(%q) error = %v", path, err)
	}
	catalog, err := Load(archive)
	if err != nil {
		t.Fatalf("Load(real PVF) error = %v", err)
	}
	rules, groups, actors := catalog.Snapshot()
	if rules != 4 || groups < 160 || actors < 500 {
		t.Fatalf("real catalog snapshot rules=%d groups=%d actors=%d", rules, groups, actors)
	}
	rule, ok := catalog.Difficulty("A")
	if !ok || rule.RewardRollCount != 8 {
		t.Fatalf("real difficulty A = %+v, %v", rule, ok)
	}
	group, ok := catalog.Group(155)
	if !ok || len(group.Actors["A"]) != 2 {
		t.Fatalf("real group 155 = %+v, %v", group, ok)
	}
	t.Logf("real Hell Party catalog: rules=%d groups=%d actors=%d difficultyA=%v", rules, groups, actors, rule.Values)
}

func TestRealPVFDropCatalog(t *testing.T) {
	path := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if path == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE to verify the runtime Hell Party drop catalog")
	}
	archive, err := platformpvf.Open(path)
	if err != nil {
		t.Fatalf("Open(%q) error = %v", path, err)
	}
	catalog, err := LoadDropCatalog(archive)
	if err != nil {
		t.Fatalf("LoadDropCatalog(real PVF) error = %v", err)
	}
	probabilityRows, rarityRows, gradeRows := catalog.Snapshot()
	if probabilityRows == 0 || rarityRows < 2 || gradeRows < 90 {
		t.Fatalf("real drop catalog snapshot probability=%d rarity=%d grade=%d", probabilityRows, rarityRows, gradeRows)
	}
	rate, rateFound := catalog.DropRate(90, 0)
	window, windowFound := catalog.GradeWindow(90)
	weights, weightsFound := catalog.RarityWeights(1)
	if !rateFound || !windowFound || !weightsFound {
		t.Fatalf("real level 90 fields rate=%d/%t window=%+v/%t weights=%v/%t", rate, rateFound, window, windowFound, weights, weightsFound)
	}
	t.Logf("real Hell Party drop catalog: probability=%d rarity=%d grade=%d level90_rate=%d level90_window=%+v modeA_weights=%v",
		probabilityRows, rarityRows, gradeRows, rate, window, weights)
}

func TestRealPVFDungeon86Room(t *testing.T) {
	path := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if path == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE to verify the runtime Hell Party room")
	}
	archive, err := platformpvf.Open(path)
	if err != nil {
		t.Fatalf("Open(%q) error = %v", path, err)
	}
	table, err := worldmap.LoadSource(context.Background(), archive, worldmap.Options{SkipAreas: true})
	if err != nil {
		t.Fatalf("LoadSource(real PVF) error = %v", err)
	}
	dungeon, found := table.FindDungeon(86)
	if !found || len(dungeon.Mazes) == 0 {
		t.Fatalf("real dungeon 86 missing: found=%t mazes=%d", found, len(dungeon.Mazes))
	}
	maze := dungeon.Mazes[0]
	if !maze.SealDoorMapIndex.Set || len(maze.SealDoorPosition) != 2 {
		t.Fatalf("real dungeon 86 seal-door metadata=%+v position=%v", maze.SealDoorMapIndex, maze.SealDoorPosition)
	}
	sealMap, found := table.FindMap(maze.SealDoorMapIndex.Value)
	if !found {
		t.Fatalf("real dungeon 86 seal-door map %d missing", maze.SealDoorMapIndex.Value)
	}
	resolver, err := worldmap.NewResolver(table)
	if err != nil {
		t.Fatal(err)
	}
	topology, err := worldmap.BuildDungeonTopology(resolver, 86, 0)
	if err != nil {
		t.Fatal(err)
	}
	coordinate := worldmap.RoomCoordinate{X: maze.SealDoorPosition[0], Y: maze.SealDoorPosition[1]}
	normalRoom, found := topology.Room(coordinate)
	if !found || normalRoom.Map == nil {
		t.Fatalf("real dungeon 86 normal seal room %s missing", coordinate)
	}
	room, err := ResolveRoom(table, topology, 86, 0)
	if err != nil {
		t.Fatalf("ResolveRoom(real dungeon 86) error = %v", err)
	}
	if room.Coordinate != coordinate || room.HellMap.ID != 60050 || room.NormalMap.ID != 20304 ||
		room.Pillar.ObjectID != 30519 || len(room.Entries) != 18 {
		t.Fatalf("real dungeon 86 Hell Party room = %+v", room)
	}
	t.Logf("dungeon=86 seal_map=%d path=%s coordinate=%s normal_map=%d pillar=%d entries=%d",
		sealMap.ID, sealMap.Path, coordinate, normalRoom.Map.Map.ID, room.Pillar.ObjectID, len(room.Entries))
}

func TestRealPVFDungeon310Room(t *testing.T) {
	path := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if path == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE to inspect the current dungeon 310 Hell Party room")
	}
	archive, err := platformpvf.Open(path)
	if err != nil {
		t.Fatalf("Open(%q) error = %v", path, err)
	}
	table, err := worldmap.LoadSource(context.Background(), archive, worldmap.Options{SkipAreas: true})
	if err != nil {
		t.Fatalf("LoadSource(real PVF) error = %v", err)
	}
	dungeon, found := table.FindDungeon(310)
	if !found || len(dungeon.Mazes) == 0 {
		t.Fatalf("real dungeon 310 missing: found=%t mazes=%d", found, len(dungeon.Mazes))
	}
	maze := dungeon.Mazes[0]
	if !maze.SealDoorMapIndex.Set || len(maze.SealDoorPosition) != 2 {
		t.Fatalf("real dungeon 310 seal-door metadata=%+v position=%v", maze.SealDoorMapIndex, maze.SealDoorPosition)
	}
	sealMap, found := table.FindMap(maze.SealDoorMapIndex.Value)
	if !found {
		t.Fatalf("real dungeon 310 seal-door map %d missing", maze.SealDoorMapIndex.Value)
	}
	if sealMap.ID != 60083 || sealMap.Path != "map/MetroCenter/Time_Plaza/Hell_Time_Plaza.map" || len(sealMap.Diagnostics) != 0 {
		t.Fatalf("real dungeon 310 seal-door map id=%d path=%s diagnostics=%+v", sealMap.ID, sealMap.Path, sealMap.Diagnostics)
	}
	if len(sealMap.SpecialPassiveObjects) != 1 {
		t.Fatalf("real dungeon 310 special objects=%+v", sealMap.SpecialPassiveObjects)
	}
	pillar := sealMap.SpecialPassiveObjects[0]
	if pillar.ObjectID != 30528 || pillar.X != 594 || pillar.Y != 301 || len(pillar.HellParty) != 13 {
		t.Fatalf("real dungeon 310 Hell Party pillar=%+v", pillar)
	}
	resolver, err := worldmap.NewResolver(table)
	if err != nil {
		t.Fatal(err)
	}
	topology, err := worldmap.BuildDungeonTopology(resolver, 310, 0)
	if err != nil {
		t.Fatal(err)
	}
	room, err := ResolveRoom(table, topology, 310, 0)
	if err != nil {
		t.Fatalf("ResolveRoom(real dungeon 310) error = %v", err)
	}
	if room.HellMap.ID != 60083 || room.NormalMap.ID == 60083 || room.Pillar.ObjectID != 30528 || len(room.Entries) != 13 {
		t.Fatalf("real dungeon 310 Hell Party room=%+v", room)
	}
	t.Logf("dungeon=310 seal_map=%d path=%s coordinate=%s normal_map=%d pillar=%d entries=%d",
		sealMap.ID, sealMap.Path, room.Coordinate, room.NormalMap.ID, room.Pillar.ObjectID, len(room.Entries))
}
