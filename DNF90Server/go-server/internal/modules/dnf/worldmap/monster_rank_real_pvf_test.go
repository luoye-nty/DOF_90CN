package worldmap

import (
	"context"
	"os"
	"reflect"
	"testing"

	platformpvf "longheng.io/server/internal/platform/pvf"
)

func TestRealScriptPVFBlackSmokeKeepsTogAuthoredNormalRank(t *testing.T) {
	pvfPath := os.Getenv("DNF_WORLDMAP_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNF_WORLDMAP_REAL_PVF_SMOKE to run the real monster-rank regression")
	}
	archive, err := platformpvf.LoadArchive(platformpvf.Options{Path: pvfPath})
	if err != nil {
		t.Fatal(err)
	}
	table, err := LoadSource(context.Background(), archive, Options{SkipAreas: true, CompactRuntime: true})
	if err != nil {
		t.Fatal(err)
	}
	resolver, err := NewResolver(table)
	if err != nil {
		t.Fatal(err)
	}
	topology, err := BuildDungeonTopology(resolver, 243, 0)
	if err != nil {
		t.Fatal(err)
	}

	coordinate := RoomCoordinate{X: 1, Y: 0}
	wantRanks := []string{"[normal]"}
	for read := 0; read < 3; read++ {
		room, ok := topology.Room(coordinate)
		if !ok || room.Map == nil || room.Map.Map.ID != 58437 {
			t.Fatalf("read %d Black Smoke room=%+v ok=%t", read, room, ok)
		}
		if len(room.Map.Map.Monsters) != 1 || room.Map.Map.Monsters[0].MonsterID != 56675 {
			t.Fatalf("read %d Black Smoke monsters=%+v", read, room.Map.Map.Monsters)
		}
		if got := monsterSpawnRanks(room.Map.Map.Monsters); !reflect.DeepEqual(got, wantRanks) {
			t.Fatalf("read %d Tog ranks=%v, want PVF-authored %v", read, got, wantRanks)
		}
	}
}
