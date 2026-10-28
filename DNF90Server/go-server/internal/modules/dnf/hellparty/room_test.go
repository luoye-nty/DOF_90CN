package hellparty

import (
	"context"
	"testing"

	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
	"longheng.io/server/internal/modules/dnf/worldmap"
)

type roomTextSource map[string]string

func (source roomTextSource) ReadText(path string) (string, error) {
	return source[path], nil
}

func TestResolveAndApplySealDoorRoom(t *testing.T) {
	source := roomTextSource{
		"map/map.lst":         "100 `normal.map` 200 `hell.map`",
		"map/normal.map":      "[dungeon]\n10\n[/dungeon]",
		"map/hell.map":        "[special passive object]\n30528 550 254 0 1 `[hellparty]`\n[hellparty]\n125 25 1 155 75 1\n[/hellparty]",
		"dungeon/dungeon.lst": "10 `test.dgn`",
		"dungeon/test.dgn": `[maze info]
[size]
2 1
[greed]
` + "`FF`" + `
[map specification]
` + "`map`" + ` 0 0 100 ` + "`boss`" + ` 1 0 100
[/map specification]
[start map]
0 0
[/start map]
[boss map]
1 0
[/boss map]
[seal door map index]
200
[seal door pos]
0 0`,
		"worldmap/worldmap.lst": "1 `test.map`",
		"worldmap/test.map":     "[dungeon]\n10 -1\n[/dungeon]",
	}
	index, err := dnfpvf.Build(context.Background(), source, dnfpvf.BuildOptions{
		Paths: []string{"map/normal.map", "map/hell.map", "dungeon/test.dgn", "worldmap/test.map"},
		Lists: []string{worldmap.DefaultMapList, worldmap.DefaultDungeonList, worldmap.DefaultWorldMapList},
	})
	if err != nil {
		t.Fatal(err)
	}
	table, err := worldmap.Load(context.Background(), index, worldmap.Options{})
	if err != nil {
		t.Fatal(err)
	}
	resolver, err := worldmap.NewResolver(table)
	if err != nil {
		t.Fatal(err)
	}
	topology, err := worldmap.BuildDungeonTopology(resolver, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	room, err := ResolveRoom(table, topology, 10, 0)
	if err != nil {
		t.Fatalf("ResolveRoom() error = %v", err)
	}
	if room.HellMap.ID != 200 || room.NormalMap.ID != 100 || room.Pillar.ObjectID != 30528 || len(room.Entries) != 2 {
		t.Fatalf("room = %+v", room)
	}
	if err := ApplyRoom(topology, room); err != nil {
		t.Fatalf("ApplyRoom() error = %v", err)
	}
	applied, ok := topology.Room(room.Coordinate)
	if !ok || applied.Map == nil || applied.Map.Map.ID != 200 || applied.Map.Source != worldmap.ResolutionHellParty {
		t.Fatalf("applied room = %+v, %v", applied, ok)
	}
}
