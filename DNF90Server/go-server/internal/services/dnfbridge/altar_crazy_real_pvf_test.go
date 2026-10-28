package dnfbridge

import (
	"context"
	"os"
	"strings"
	"testing"

	"longheng.io/server/internal/modules/dnf/worldmap"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

func TestRealScriptPVFEndlessAltarCrazyMonsterSelection(t *testing.T) {
	path := os.Getenv("DNF_WORLDMAP_REAL_PVF_SMOKE")
	if path == "" {
		t.Skip("set DNF_WORLDMAP_REAL_PVF_SMOKE to verify Endless Altar crazy monsters")
	}
	archive, err := platformpvf.LoadArchive(platformpvf.Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	table, err := worldmap.LoadSource(context.Background(), archive, worldmap.Options{SkipAreas: true, CompactRuntime: true})
	if err != nil {
		t.Fatal(err)
	}
	dungeon, found := table.FindDungeon(11006)
	if !found {
		t.Fatal("Endless Altar dungeon 11006 is missing")
	}
	resolver, err := worldmap.NewResolver(table)
	if err != nil {
		t.Fatal(err)
	}
	layout, err := worldmap.BuildDungeonLayout(resolver, dungeon.ID, 0, func(choice worldmap.DungeonMapChoice) (int64, error) {
		return choice.Candidates[0].ID, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := worldmap.NewDungeonRun(layout)
	if err != nil {
		t.Fatal(err)
	}
	rooms := run.Rooms()
	if len(rooms) != 3 {
		t.Fatalf("Endless Altar rooms=%d", len(rooms))
	}
	for index, room := range rooms {
		if room.Map == nil {
			t.Fatalf("room %d has no map", index)
		}
		scene := worldmap.DungeonRoomScene{Coordinate: room.Coordinate, Map: *room.Map}
		runtime, err := newCurrentDungeonAltarRuntime(dungeon, scene)
		if err != nil {
			t.Fatalf("room %d runtime: %v", index, err)
		}
		if err := runtime.configureRunSeed(uint32(0x12345678 + index)); err != nil {
			t.Fatalf("room %d crazy selection: %v", index, err)
		}
		if runtime.crazyMonster == nil || runtime.crazyMonster.MonsterCode == 0 || runtime.crazyMonster.Variant > 18 {
			t.Fatalf("room %d crazy=%+v", index, runtime.crazyMonster)
		}
		maximumRound := 5
		if strings.EqualFold(strings.TrimSpace(room.Map.Map.Type), "[boss]") {
			maximumRound = 4
		}
		if runtime.crazyMonster.RoundIndex < 0 || runtime.crazyMonster.RoundIndex >= maximumRound {
			t.Fatalf("room %d type=%q crazy round=%d max-exclusive=%d", index, room.Map.Map.Type, runtime.crazyMonster.RoundIndex, maximumRound)
		}
	}
}
