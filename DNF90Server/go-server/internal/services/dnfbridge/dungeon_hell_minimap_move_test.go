package dnfbridge

import (
	"testing"

	"longheng.io/server/internal/modules/dnf/worldmap"
)

func TestBuildCurrentDungeonMoveStartMapBodyKeepsHellMarkerAcrossRooms(t *testing.T) {
	coordinate := worldmap.RoomCoordinate{X: 1, Y: 0}
	scene := worldmap.DungeonRoomScene{
		Coordinate: coordinate,
		Map:        worldmap.ResolvedMap{Map: worldmap.Map{ID: 70001}},
	}
	room := &runtimeDungeonRoom{coordinate: coordinate, mapID: scene.Map.Map.ID}
	runtime := &runtimeDungeonState{
		HellParty: &currentDungeonHellPartyOwner{Mode: currentHellPartyModeA},
	}

	full, err := buildCurrentDungeonMoveStartMapBody(runtime, currentDungeonMovePlan{
		Target:      scene,
		TargetRoom:  room,
		Seed:        0x12345678,
		PayloadMode: currentDungeonStartMapPayloadBuild,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(full) < 21 || full[7] != currentHellPartyModeA || full[20] != 1 {
		t.Fatalf("full room op29 mode=%d marker=%d body=%x", full[7], full[20], full)
	}

	cached, err := buildCurrentDungeonMoveStartMapBody(runtime, currentDungeonMovePlan{
		Target:      scene,
		TargetRoom:  room,
		TargetVisit: &runtimeDungeonRoomVisit{Scene: scene, Room: room, Seed: 0x12345678},
		Seed:        0x12345678,
		PayloadMode: currentDungeonStartMapPayloadCached,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(cached) != 16 || cached[7] != currentHellPartyModeA {
		t.Fatalf("cached room op29 mode=%d len=%d body=%x", cached[7], len(cached), cached)
	}
}

func TestBuildCurrentDungeonMoveStartMapBodyDoesNotInventHellMarker(t *testing.T) {
	coordinate := worldmap.RoomCoordinate{X: 1, Y: 0}
	scene := worldmap.DungeonRoomScene{
		Coordinate: coordinate,
		Map:        worldmap.ResolvedMap{Map: worldmap.Map{ID: 70001}},
	}
	room := &runtimeDungeonRoom{coordinate: coordinate, mapID: scene.Map.Map.ID}

	full, err := buildCurrentDungeonMoveStartMapBody(&runtimeDungeonState{}, currentDungeonMovePlan{
		Target:      scene,
		TargetRoom:  room,
		Seed:        0x12345678,
		PayloadMode: currentDungeonStartMapPayloadBuild,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(full) < 21 || full[7] != 0 || full[20] != 0 {
		t.Fatalf("ordinary room unexpectedly advertised hell mode=%d marker=%d body=%x", full[7], full[20], full)
	}
}
