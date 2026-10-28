package dnfbridge

import (
	"context"
	"os"
	"testing"

	"longheng.io/server/internal/modules/dnf/dungeoncmd"
	"longheng.io/server/internal/modules/dnf/worldmap"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

// TestRealScriptPVFPowerstationGrandineSpecialPassiveObjectItemDrops locks the
// 能源中心 building-drop shape: dungeon 7136 (格兰迪发电站 A) carries the dgn
// [special passive object item] group `0 -1 1 3341 1500`, and its map 53462
// places twelve [special passive object] pillars whose `[item] 0 1 -1` child
// rolls that group exactly once at 15%. Planning must pair every wire row with
// registered pickup state, never exceed one drop per pillar, and replay
// byte-identically for the same room RNG.
func TestRealScriptPVFPowerstationGrandineSpecialPassiveObjectItemDrops(t *testing.T) {
	pvfPath := os.Getenv("DNF_WORLDMAP_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNF_WORLDMAP_REAL_PVF_SMOKE to verify powerstation special-passive-object item drops")
	}
	ctx := context.Background()
	archive, err := platformpvf.Open(pvfPath)
	if err != nil {
		t.Fatal(err)
	}
	table, err := worldmap.LoadSource(ctx, archive, worldmap.Options{})
	if err != nil {
		t.Fatal(err)
	}
	resolver, err := worldmap.NewResolver(table)
	if err != nil {
		t.Fatal(err)
	}
	monsterCatalog, err := newPVFDungeonMonsterCatalog(archive)
	if err != nil {
		t.Fatal(err)
	}

	dungeon, ok := table.FindDungeon(7136)
	if !ok {
		t.Fatal("real PVF dungeon 7136 (grandine station A) is missing")
	}
	// The file writes the table under maze 0 (`0 -1 1 3341 1500 1 -1 1
	// 10099777 2500`); it must be promoted to the dungeon so the maze that
	// owns map 53462 inherits it.
	rows := dungeon.Metadata.SpecialPassiveObjectItems
	if len(rows) != 2 || rows[0].Index != 0 || rows[0].LevelOverride != -1 || rows[0].ItemID != 3341 || rows[0].DropRate != 1500 ||
		rows[1].Index != 1 || rows[1].ItemID != 10099777 || rows[1].DropRate != 2500 {
		t.Fatalf("dungeon 7136 rows=%+v, want group 0 = 3341@1500 and group 1 = 10099777@2500", rows)
	}
	for mazeIndex, maze := range dungeon.Mazes {
		if len(maze.SpecialPassiveObjectItems) != 0 {
			t.Fatalf("dungeon 7136 maze=%d retained route-local rows=%+v", mazeIndex, maze.SpecialPassiveObjectItems)
		}
	}

	mazeIndex, roomX, roomY := -1, int64(-1), int64(-1)
	for index, maze := range dungeon.Mazes {
		for _, specification := range maze.MapSpecifications {
			for _, mapID := range specification.MapIDs {
				if mapID == 53462 {
					mazeIndex, roomX, roomY = index, specification.Coordinate.X, specification.Coordinate.Y
				}
			}
		}
	}
	if mazeIndex < 0 {
		t.Fatal("real dungeon 7136 places map 53462 in no maze")
	}
	resolved, err := resolver.Resolve(worldmap.ResolveRequest{
		DungeonID: 7136,
		MazeIndex: mazeIndex,
		X:         roomX,
		Y:         roomY,
	})
	if err != nil {
		t.Fatalf("resolve real dungeon 7136 maze %d room (%d,%d): %v", mazeIndex, roomX, roomY, err)
	}
	if resolved.Map.ID != 53462 {
		t.Fatalf("real dungeon 7136 maze %d room (%d,%d) map=%d, want 53462", mazeIndex, roomX, roomY, resolved.Map.ID)
	}
	scene := worldmap.DungeonRoomScene{
		Coordinate:            worldmap.RoomCoordinate{X: roomX, Y: roomY},
		Map:                   resolved,
		PassiveObjects:        resolved.Map.PassiveObjects,
		SpecialPassiveObjects: resolved.Map.SpecialPassiveObjects,
	}
	itemSpawnCount := 0
	for _, object := range scene.SpecialPassiveObjects {
		for _, spawn := range object.Spawns {
			if normalizeDungeonPVFSymbol(spawn.Kind) == "item" {
				itemSpawnCount++
				if spawn.Code != 0 || spawn.Level != 1 || spawn.Params[0] != -1 {
					t.Fatalf("real map 53462 unexpected [item] spawn=%+v", spawn)
				}
			}
		}
	}
	if itemSpawnCount != 12 {
		t.Fatalf("real map 53462 [item] spawn count=%d, want 12", itemSpawnCount)
	}

	service := &Service{dungeonMonsterTable: monsterCatalog}
	runtime := &runtimeDungeonState{
		Request:       dungeoncmd.SelectDungeonRequest{DungeonID: 7136},
		Dungeon:       dungeon,
		MazeIndex:     mazeIndex,
		NextObjectKey: firstDungeonMonsterObjectKey,
	}
	session := &gameSession{selectedCharacterID: 2}
	const startRNG = uint32(20260826)
	plan, err := service.planCurrentDungeonPassiveObjectDrops(session, runtime, scene, runtime.NextObjectKey, startRNG)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.ExtraEntries) != len(plan.Drops) {
		t.Fatalf("entries=%d drops=%d must pair", len(plan.ExtraEntries), len(plan.Drops))
	}
	if len(plan.Drops) > 12 {
		t.Fatalf("planned %d drops for twelve single-roll pillars", len(plan.Drops))
	}
	if plan.NextObjectKey != firstDungeonMonsterObjectKey+uint32(len(plan.Drops)) {
		t.Fatalf("next object key=%d, want first+%d", plan.NextObjectKey, len(plan.Drops))
	}
	seenPillar := make(map[byte]struct{})
	for _, entry := range plan.ExtraEntries {
		if entry.ItemID != 3341 || entry.StackCount != 1 || int(entry.PassiveObjectIndex) >= len(scene.SpecialPassiveObjects) {
			t.Fatalf("planned entry=%+v", entry)
		}
		if _, duplicate := seenPillar[entry.PassiveObjectIndex]; duplicate {
			t.Fatalf("pillar %d planned twice: %+v", entry.PassiveObjectIndex, plan.ExtraEntries)
		}
		seenPillar[entry.PassiveObjectIndex] = struct{}{}
	}
	// Twelve single rolls consume exactly twelve LCG steps.
	wantRNG := newCurrentDungeonDropLCG(startRNG)
	wantDrops := 0
	for index := 0; index < 12; index++ {
		if wantRNG.Next(currentDungeonDropDenominator) < 1500 {
			wantDrops++
		}
	}
	if len(plan.Drops) != wantDrops || plan.NextDropRNG != wantRNG.Seed() {
		t.Fatalf("drops=%d rng=%08x, want drops=%d rng=%08x", len(plan.Drops), plan.NextDropRNG, wantDrops, wantRNG.Seed())
	}
	t.Logf("real map 53462 planned %d/12 魔刹石 drops at room rng %d", len(plan.ExtraEntries), startRNG)

	replay, err := service.planCurrentDungeonPassiveObjectDrops(session, runtime, scene, runtime.NextObjectKey, startRNG)
	if err != nil {
		t.Fatal(err)
	}
	if len(replay.ExtraEntries) != len(plan.ExtraEntries) {
		t.Fatalf("replay entries=%d, want %d", len(replay.ExtraEntries), len(plan.ExtraEntries))
	}
	for index := range replay.ExtraEntries {
		if replay.ExtraEntries[index] != plan.ExtraEntries[index] {
			t.Fatalf("replay entry[%d]=%+v, want %+v", index, replay.ExtraEntries[index], plan.ExtraEntries[index])
		}
	}
}
