package dnfbridge

import (
	"context"
	"os"
	"slices"
	"testing"

	"longheng.io/server/internal/modules/dnf/worldmap"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

func TestRealScriptPVFSecondSpineEventMonsterCondition(t *testing.T) {
	pvfPath := os.Getenv("DNF_WORLDMAP_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNF_WORLDMAP_REAL_PVF_SMOKE to verify Second Spine trap ownership")
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
	dungeon, ok := table.FindDungeon(26)
	if !ok {
		t.Fatal("real PVF Second Spine dungeon 26 is missing")
	}
	if !dungeon.Metadata.EventMonsterRandomMap.Set || dungeon.Metadata.EventMonsterRandomMap.Value != 0 ||
		len(dungeon.Metadata.EventMonsters) != 1 ||
		dungeon.Metadata.EventMonsters[0].Kind != "event monster" ||
		!slices.Equal(dungeon.Metadata.EventMonsters[0].Values, []int64{1, 1000, 21100, 1, 1000, 1, 1, 5}) {
		t.Fatalf("real dungeon 26 event-monster metadata=%+v", dungeon.Metadata.EventMonsters)
	}
	mapValue, ok := table.FindMap(58708)
	if !ok {
		t.Fatal("real PVF Second Spine map 58708 is missing")
	}
	if len(mapValue.SpecialPassiveObjects) != 1 {
		t.Fatalf("real map 58708 special objects=%+v, want one", mapValue.SpecialPassiveObjects)
	}
	if len(mapValue.EventMonsterPositions) != 4 {
		t.Fatalf("real map 58708 event positions=%+v, want four", mapValue.EventMonsterPositions)
	}
	object := mapValue.SpecialPassiveObjects[0]
	if object.ObjectID != 781 || len(object.Spawns) != 2 {
		t.Fatalf("real map 58708 special object=%+v", object)
	}
	wantCodes := []int64{48021, 48003}
	for index, spawn := range object.Spawns {
		if normalizeDungeonPVFSymbol(spawn.Kind) != "trap" || spawn.Code != wantCodes[index] {
			t.Fatalf("real map 58708 spawn[%d]=%+v", index, spawn)
		}
	}
	scene := worldmap.DungeonRoomScene{
		Coordinate:            worldmap.RoomCoordinate{X: 1, Y: 2},
		Map:                   worldmap.ResolvedMap{Map: mapValue},
		SpecialPassiveObjects: append([]worldmap.SpecialPassiveObject(nil), mapValue.SpecialPassiveObjects...),
	}
	room := &runtimeDungeonRoom{coordinate: scene.Coordinate, mapID: mapValue.ID}
	runtime := &runtimeDungeonState{Dungeon: dungeon, MazeIndex: 0, Room: room}
	if !currentDungeonTrapEventMonsterScope(runtime, scene, room) {
		t.Fatalf("real Second Spine room did not satisfy the PVF event-monster condition: dungeon=%+v map=%+v",
			dungeon.Metadata.EventMonsters, mapValue.EventMonsterPositions)
	}
}
