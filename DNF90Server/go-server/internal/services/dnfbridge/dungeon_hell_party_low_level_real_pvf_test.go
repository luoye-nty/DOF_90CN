package dnfbridge

import (
	"context"
	"os"
	"testing"

	"longheng.io/server/internal/modules/dnf/dungeoncmd"
	dnfhellparty "longheng.io/server/internal/modules/dnf/hellparty"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	"longheng.io/server/internal/modules/dnf/worldmap"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

func TestRealPVFSeaTrainHellPartyHasLowLevelEquipmentCandidates(t *testing.T) {
	pvfPath := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE to verify Sea Train Hell Party drops")
	}
	archive, err := platformpvf.Open(pvfPath)
	if err != nil {
		t.Fatal(err)
	}
	table, err := worldmap.LoadSource(context.Background(), archive, worldmap.Options{SkipAreas: true})
	if err != nil {
		t.Fatal(err)
	}
	dungeon, found := table.FindDungeon(86)
	if !found || !dungeon.Metadata.BasisLevel.Set || dungeon.Metadata.BasisLevel.Value <= 0 {
		t.Fatalf("Sea Train dungeon=%+v found=%t", dungeon, found)
	}
	dropRules, err := dnfhellparty.LoadDropCatalog(archive)
	if err != nil {
		t.Fatal(err)
	}
	basisLevel := int(dungeon.Metadata.BasisLevel.Value)
	window, found := dropRules.GradeWindow(basisLevel)
	if !found {
		t.Fatalf("Sea Train basis=%d has no Hell Party grade window", basisLevel)
	}
	for difficulty := 0; difficulty < 4; difficulty++ {
		rate, rateFound := dropRules.DropRate(basisLevel, difficulty)
		t.Logf("Sea Train basis=%d difficulty=%d drop_rate=%d found=%t", basisLevel, difficulty, rate, rateFound)
	}
	items, err := newPVFDungeonDropCatalog(archive)
	if err != nil {
		t.Fatal(err)
	}
	if err := items.ensureGenericPools(); err != nil {
		t.Fatal(err)
	}
	counts := make([]int, 7)
	for rarity := range counts {
		counts[rarity] = len(items.genericEquipmentCandidates(basisLevel, window.Down, window.Up, int64(rarity)))
	}
	if counts[1]+counts[2]+counts[4] == 0 {
		t.Fatalf("Sea Train basis=%d window=%+v candidates=%v", basisLevel, window, counts)
	}
	t.Logf("Sea Train basis=%d window=%+v equipment candidates by rarity=%v", basisLevel, window, counts)
}

func TestRealPVFSeaTrainHellPartyPlansVisibleDrops(t *testing.T) {
	pvfPath := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE to verify Sea Train Hell Party drop planning")
	}
	service := &Service{options: options{pvfPath: pvfPath}}
	if err := service.preloadDungeonWorldMap(context.Background()); err != nil {
		t.Fatal(err)
	}
	table, resolver, err := service.dungeonWorldMap()
	if err != nil {
		t.Fatal(err)
	}
	dungeon, found := table.FindDungeon(86)
	if !found || len(dungeon.Mazes) == 0 {
		t.Fatalf("Sea Train dungeon=%+v found=%t", dungeon, found)
	}
	topology, err := worldmap.BuildDungeonTopology(resolver, dungeon.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	hellOwner, err := service.prepareCurrentDungeonHellParty(table, topology, dungeon, 0, 1)
	if err != nil {
		t.Fatal(err)
	}
	run, err := worldmap.NewDungeonRun(topology)
	if err != nil {
		t.Fatal(err)
	}
	path := seaTrainHellPartyTestPath(t, topology, *topology.Start, hellOwner.Room.Coordinate)
	for _, coordinate := range path[1:] {
		if err := run.MarkCurrentRoomCleared(); err != nil {
			t.Fatal(err)
		}
		if _, err := run.MoveTo(coordinate); err != nil {
			t.Fatal(err)
		}
	}
	dungeonSession, err := worldmap.NewDungeonSession(run)
	if err != nil {
		t.Fatal(err)
	}
	scene, found := dungeonSession.Scene()
	if !found || scene.Coordinate != hellOwner.Room.Coordinate || scene.Map.Map.ID != hellOwner.Room.HellMap.ID {
		t.Fatalf("Sea Train Hell Party scene=%+v found=%t", scene, found)
	}
	monsterCatalog, err := service.dungeonMonsterCatalog()
	if err != nil {
		t.Fatal(err)
	}
	room, nextObjectKey, err := newRuntimeDungeonRoom(scene, monsterCatalog, firstDungeonMonsterObjectKey)
	if err != nil {
		t.Fatal(err)
	}
	runtime := &runtimeDungeonState{
		Request: dungeoncmd.SelectDungeonRequest{
			DungeonID:  uint32(dungeon.ID),
			Difficulty: 4,
		},
		Dungeon:       dungeon,
		MazeIndex:     0,
		Character:     dnfrepo.CharacterRecord{CharacterID: "99", AccountID: "account-1", Level: 74},
		Session:       dungeonSession,
		Room:          room,
		HellParty:     hellOwner,
		NextObjectKey: nextObjectKey,
		Seed:          1,
		Rooms:         make(map[runtimeDungeonRoomKey]*runtimeDungeonRoomVisit),
	}
	runtime.Rooms[runtimeDungeonRoomKeyFromScene(scene)] = &runtimeDungeonRoomVisit{
		Scene: scene, Room: room, Seed: runtime.Seed, DropRNG: 1,
	}
	staleNextObjectKey := runtime.NextObjectKey
	runtime.DropOwner = newRuntimeDungeonDropOwner()
	if err := runtime.DropOwner.registerBatch([]*runtimeDungeonDrop{{
		ObjectKey:           staleNextObjectKey,
		SceneSlot:           1,
		OwnerActorObjectKey: currentSceneActorObjectKey(99),
		Room:                runtimeDungeonRoomKey{X: -1, Y: -1, MapID: -1},
		Status:              runtimeDungeonDropConsumed,
	}}, 2); err != nil {
		t.Fatal(err)
	}
	wires, selected, err := service.planCurrentDungeonHellPartyDrops(
		runtime,
		currentDungeonHellPartyActor{Code: 10626, EntityType: 1, Level: 70},
		currentSceneActorObjectKey(99),
		8,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(wires) == 0 || len(selected) == 0 || len(wires) != len(selected) {
		t.Fatalf("Sea Train Hell Party wires=%d selected=%v", len(wires), selected)
	}
	if wires[0].SceneObjectKey <= staleNextObjectKey {
		t.Fatalf("Sea Train Hell Party reused stale object key: got=%d prior=%d", wires[0].SceneObjectKey, staleNextObjectKey)
	}
}

func seaTrainHellPartyTestPath(
	t *testing.T,
	topology *worldmap.DungeonTopology,
	start worldmap.RoomCoordinate,
	target worldmap.RoomCoordinate,
) []worldmap.RoomCoordinate {
	t.Helper()
	if start == target {
		return []worldmap.RoomCoordinate{start}
	}
	rooms := make(map[worldmap.RoomCoordinate]struct{})
	for _, room := range topology.Rooms() {
		rooms[room.Coordinate] = struct{}{}
	}
	queue := []worldmap.RoomCoordinate{start}
	parents := map[worldmap.RoomCoordinate]worldmap.RoomCoordinate{}
	seen := map[worldmap.RoomCoordinate]struct{}{start: {}}
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		for _, next := range []worldmap.RoomCoordinate{
			{X: current.X - 1, Y: current.Y},
			{X: current.X + 1, Y: current.Y},
			{X: current.X, Y: current.Y - 1},
			{X: current.X, Y: current.Y + 1},
		} {
			if _, exists := rooms[next]; !exists {
				continue
			}
			if _, visited := seen[next]; visited {
				continue
			}
			seen[next] = struct{}{}
			parents[next] = current
			if next == target {
				path := []worldmap.RoomCoordinate{target}
				for path[len(path)-1] != start {
					path = append(path, parents[path[len(path)-1]])
				}
				for left, right := 0, len(path)-1; left < right; left, right = left+1, right-1 {
					path[left], path[right] = path[right], path[left]
				}
				return path
			}
			queue = append(queue, next)
		}
	}
	t.Fatalf("no Sea Train topology path from %s to %s", start, target)
	return nil
}
