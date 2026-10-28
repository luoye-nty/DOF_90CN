package dnfbridge

import (
	"context"
	"os"
	"testing"

	"longheng.io/server/internal/modules/dnf/worldmap"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

func TestRealScriptPVFBalgurJumpRidableAssociation(t *testing.T) {
	pvfPath := os.Getenv("DNF_WORLDMAP_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNF_WORLDMAP_REAL_PVF_SMOKE to verify Time Rift Balgur Jump")
	}
	archive, err := platformpvf.Open(pvfPath)
	if err != nil {
		t.Fatal(err)
	}
	table, err := worldmap.LoadSource(context.Background(), archive, worldmap.Options{CompactRuntime: true})
	if err != nil {
		t.Fatal(err)
	}
	resolver, err := worldmap.NewResolver(table)
	if err != nil {
		t.Fatal(err)
	}
	topology, err := worldmap.BuildDungeonLayout(resolver, 7539, 3, func(choice worldmap.DungeonMapChoice) (int64, error) {
		return choice.Candidates[0].ID, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	room, ok := topology.Room(worldmap.RoomCoordinate{X: 2, Y: 1})
	if !ok || room.Map == nil || room.Map.Map.ID != 80275 {
		t.Fatalf("Time Rift desert third room=%+v found=%t", room, ok)
	}
	owner, entries, next, err := planCurrentDungeonStaticRidableObjects(topology.Rooms(), 500)
	if err != nil {
		t.Fatal(err)
	}
	if owner == nil || len(entries) != 1 || next != 501 {
		t.Fatalf("Time Rift ridable owner=%+v entries=%+v next=%d", owner, entries, next)
	}
	entry := entries[0]
	if entry.RoomX != 2 || entry.RoomY != 1 || entry.SpawnMode != currentDungeonRandomizedPassiveFamily ||
		entry.ObjectIndex != 74266 || entry.RuntimeObjectKey != 500 {
		t.Fatalf("Time Rift Balgur association=%+v", entry)
	}

	// The room repeats monster templates, but C2S death ownership is keyed by
	// the seven distinct runtime object keys rather than monster ID. Prove that
	// all seven deaths clear the room and authorize the right-hand 2:1 -> 3:1
	// transition; the passive Balgur association is not an eighth hostile.
	run, err := worldmap.NewDungeonRun(topology)
	if err != nil {
		t.Fatal(err)
	}
	session, err := worldmap.NewDungeonSession(run)
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range []worldmap.RoomCoordinate{{X: 1, Y: 1}, {X: 2, Y: 1}} {
		if err := session.MarkCurrentRoomScriptedCleared(); err != nil {
			t.Fatal(err)
		}
		if _, err := session.MoveByte(byte(target.X), byte(target.Y)); err != nil {
			t.Fatalf("move to %s: %v", target, err)
		}
	}
	desertScene, ok := session.Scene()
	if !ok || desertScene.Map.Map.ID != 80275 || len(desertScene.BlockingHostiles) != 7 ||
		len(desertScene.ExpectedHostiles) != 7 {
		t.Fatalf("Time Rift desert scene=%+v found=%t", desertScene, ok)
	}
	monsterCatalog, err := newPVFDungeonMonsterCatalog(archive)
	if err != nil {
		t.Fatal(err)
	}
	roomOwner, objectKeyAfterMonsters, err := newRuntimeDungeonRoom(desertScene, monsterCatalog, firstDungeonMonsterObjectKey)
	if err != nil {
		t.Fatal(err)
	}
	if announced, err := roomOwner.AnnounceAllActors(session); err != nil || announced != 7 ||
		objectKeyAfterMonsters != firstDungeonMonsterObjectKey+7 {
		t.Fatalf("Time Rift desert announce=%d next=%d err=%v", announced, objectKeyAfterMonsters, err)
	}
	snapshot := roomOwner.Snapshot()
	seenKeys := make(map[uint32]struct{}, len(snapshot.Monsters))
	for index, monster := range snapshot.Monsters {
		if _, duplicate := seenKeys[monster.ObjectKey]; duplicate {
			t.Fatalf("Time Rift desert duplicate runtime key=%d snapshot=%+v", monster.ObjectKey, snapshot.Monsters)
		}
		seenKeys[monster.ObjectKey] = struct{}{}
		_, cleared, deathErr := roomOwner.CommitActorDeathReport(monster.ObjectKey, session)
		if deathErr != nil || cleared != (index == len(snapshot.Monsters)-1) {
			t.Fatalf("Time Rift desert death index=%d key=%d cleared=%t err=%v", index, monster.ObjectKey, cleared, deathErr)
		}
	}
	if _, err := session.ValidateMoveByteTransition(3, 1, nil); err != nil {
		t.Fatalf("Time Rift desert right-hand exit remained blocked: %v", err)
	}

	ordinary, found := table.FindMap(33746)
	if !found || !mapHasPassiveObject(ordinary, currentDungeonBalgurJumpObjectID) {
		t.Fatalf("ordinary Gust Zone map33746 Balgur passive=%+v found=%t", ordinary.PassiveObjects, found)
	}
}

func mapHasPassiveObject(value worldmap.Map, objectID int64) bool {
	for _, object := range value.PassiveObjects {
		if object.ObjectID == objectID {
			return true
		}
	}
	return false
}
