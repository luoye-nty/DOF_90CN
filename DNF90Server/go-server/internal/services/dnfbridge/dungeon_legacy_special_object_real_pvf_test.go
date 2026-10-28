package dnfbridge

import (
	"context"
	"encoding/binary"
	"os"
	"testing"

	"longheng.io/server/internal/modules/dnf/dungeoncmd"
	"longheng.io/server/internal/modules/dnf/worldmap"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

func TestRealScriptPVFBwangaLegacyMapObjectStaysOutsideStartMapActors(t *testing.T) {
	pvfPath := os.Getenv("DNF_WORLDMAP_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNF_WORLDMAP_REAL_PVF_SMOKE to verify Bwanga legacy map-object ownership")
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
	monsterCatalog, err := newPVFDungeonMonsterCatalog(archive)
	if err != nil {
		t.Fatal(err)
	}
	dungeon, found := table.FindDungeon(141)
	if !found || len(dungeon.Mazes) <= 1 {
		t.Fatalf("real PVF dungeon 141 or maze 1 missing: found=%t mazes=%d", found, len(dungeon.Mazes))
	}
	topology, err := worldmap.BuildDungeonLayout(resolver, dungeon.ID, 1, func(choice worldmap.DungeonMapChoice) (int64, error) {
		return choice.Candidates[0].ID, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := worldmap.NewDungeonRun(topology)
	if err != nil {
		t.Fatal(err)
	}
	session, err := worldmap.NewDungeonSession(run)
	if err != nil {
		t.Fatal(err)
	}
	scene, ok := session.Scene()
	if !ok || scene.Map.Map.ID != 92113 {
		t.Fatalf("real dungeon 141 maze 1 start scene=%+v", scene.Map.Map)
	}
	if len(scene.Monsters) != 6 || len(scene.SpecialPassiveObjects) != 1 || scene.SpecialPassiveObjects[0].ObjectID != 826 {
		t.Fatalf("real map 92113 scene monsters=%d special=%+v", len(scene.Monsters), scene.SpecialPassiveObjects)
	}
	room, nextObjectKey, err := newRuntimeDungeonRoom(scene, monsterCatalog, firstDungeonMonsterObjectKey)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := planRuntimeDungeonExtendedActors(scene, monsterCatalog, nil, dungeon.Metadata.BasisLevel, nextObjectKey)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Actors) != 0 || len(plan.RetainedSpecialSpawns) != 1 || plan.RetainedSpecialSpawns[0].Spawn.Code != 17031 {
		t.Fatalf("real map 92113 extended plan=%+v", plan)
	}
	if err := room.AttachExtendedActors(plan); err != nil {
		t.Fatal(err)
	}
	runtime := &runtimeDungeonState{
		Request:        dungeoncmd.SelectDungeonRequest{DungeonID: uint32(dungeon.ID)},
		Dungeon:        dungeon,
		MazeIndex:      1,
		Session:        session,
		Room:           room,
		NextObjectKey:  plan.NextObjectKey,
		BossCoordinate: topology.Bosses[0],
		BossSet:        true,
		Seed:           1,
	}
	packets, err := buildCurrentDungeonEntryPackets(runtime, scene)
	if err != nil {
		t.Fatal(err)
	}
	if actorCount := int(packets.StartMap[18]); actorCount != 6 {
		t.Fatalf("real map 92113 op29 actor count=%d, want six ordinary monsters", actorCount)
	}
	for index := 0; index < int(packets.StartMap[18]); index++ {
		offset := 19 + index*21
		if code := binary.LittleEndian.Uint32(packets.StartMap[offset+8 : offset+12]); code == 826 {
			t.Fatalf("real map 92113 legacy object 826 leaked into op29 actor %d", index)
		}
	}
}
