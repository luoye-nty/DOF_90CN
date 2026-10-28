package dnfbridge

import (
	"encoding/binary"
	"testing"

	"longheng.io/server/internal/modules/dnf/dnfenum"
	"longheng.io/server/internal/modules/dnf/worldmap"
)

func TestCurrentDungeonBalgurJumpIsFrozenIntoStartMapAssociation(t *testing.T) {
	source := bridgeDungeonPVF(false)
	source["map/dungeon/test/start.map"] += "[passive object]\n74266 336 421 0\n99 1 2 0\n"
	runtime := prepareSyntheticDungeonRuntimeForEntryTest(t, source, nil)
	if runtime.RidableObjects == nil || len(runtime.RidableObjects.byObjectKey) != 1 {
		t.Fatalf("ridable owner=%+v", runtime.RidableObjects)
	}
	if runtime.NextObjectKey != firstDungeonMonsterObjectKey+2 {
		t.Fatalf("next object key=%d want=%d", runtime.NextObjectKey, firstDungeonMonsterObjectKey+2)
	}
	if len(runtime.RandomizedObjectGroups) != 1 || len(runtime.RandomizedObjectGroups[0]) != 1 {
		t.Fatalf("op29 association groups=%+v", runtime.RandomizedObjectGroups)
	}
	entry := runtime.RandomizedObjectGroups[0][0]
	if entry.RoomX != 0 || entry.RoomY != 0 ||
		entry.SpawnMode != currentDungeonRandomizedPassiveFamily ||
		entry.ObjectIndex != uint32(currentDungeonBalgurJumpObjectID) ||
		entry.RuntimeObjectKey != firstDungeonMonsterObjectKey+1 {
		t.Fatalf("Balgur association=%+v", entry)
	}

	scene, ok := runtime.Session.Scene()
	if !ok {
		t.Fatal("runtime scene missing")
	}
	packets, err := buildCurrentDungeonEntryPackets(runtime, scene)
	if err != nil {
		t.Fatal(err)
	}
	// Synthetic packet: 18-byte prefix + count + one 21-byte actor +
	// extra-count + fog + group-count + row-count.
	const associationOffset = 44
	if len(packets.StartMap) != associationOffset+20+1 {
		t.Fatalf("start-map len=%d body=%x", len(packets.StartMap), packets.StartMap)
	}
	row := packets.StartMap[associationOffset : associationOffset+20]
	got := [5]uint32{
		binary.LittleEndian.Uint32(row[0:4]),
		binary.LittleEndian.Uint32(row[4:8]),
		binary.LittleEndian.Uint32(row[8:12]),
		binary.LittleEndian.Uint32(row[12:16]),
		binary.LittleEndian.Uint32(row[16:20]),
	}
	// The row is (PosX, PosY, ObjectIndex, Faction, SpawnMode): Balgur is a
	// passive object, so the fifth value is the passive spawn mode the client
	// branches on, and the leading pair is the object's own `[passive object]`
	// map position (74266 336 421 0 above) rather than the room index. The
	// runtime object key is no longer on the wire -- writing it there made the
	// client discard the row entirely.
	want := [5]uint32{336, 421, 74266, 0, currentDungeonRandomizedPassiveFamily}
	if got != want {
		t.Fatalf("op29 ridable row=%v want=%v body=%x", got, want, packets.StartMap)
	}
}

func TestCurrentDungeonRidableObjectOp316UsesFrozenRoomOwner(t *testing.T) {
	room := worldmap.DungeonRoom{
		Coordinate: worldmap.RoomCoordinate{X: 2, Y: 1},
		Map: &worldmap.ResolvedMap{Map: worldmap.Map{
			ID:             80275,
			PassiveObjects: []worldmap.PassiveObject{{ObjectID: currentDungeonBalgurJumpObjectID, X: 336, Y: 421}},
		}},
	}
	owner, entries, next, err := planCurrentDungeonStaticRidableObjects([]worldmap.DungeonRoom{room}, 500)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || next != 501 {
		t.Fatalf("entries=%+v next=%d", entries, next)
	}
	scene := worldmap.DungeonRoomScene{Coordinate: room.Coordinate, Map: *room.Map}
	body := make([]byte, 7)
	body[0] = 1
	binary.LittleEndian.PutUint32(body[1:5], 500)
	body[5], body[6] = 2, 1
	request, err := decodeCurrentDungeonRidableObjectRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	accepted, ok := owner.apply(request, scene)
	if !ok || accepted.LastOperation != 1 || accepted.OperationCount != 1 {
		t.Fatalf("accepted=%+v ok=%t", accepted, ok)
	}

	wrongRoom := request
	wrongRoom.RoomX = 3
	if _, ok := owner.apply(wrongRoom, scene); ok {
		t.Fatal("cross-room op316 was accepted")
	}
	wrongKey := request
	wrongKey.RuntimeObjectKey = 501
	if _, ok := owner.apply(wrongKey, scene); ok {
		t.Fatal("unknown op316 object key was accepted")
	}
	if state := owner.byObjectKey[500]; state.OperationCount != 1 {
		t.Fatalf("rejected request mutated rider=%+v", state)
	}
	for _, malformed := range [][]byte{{}, {4, 0, 0, 0, 0, 2, 1}, {1, 0, 0, 0, 0, 2, 1}} {
		if _, err := decodeCurrentDungeonRidableObjectRequest(malformed); err == nil {
			t.Fatalf("malformed op316 accepted body=%x", malformed)
		}
	}
}

func TestDungeonNavigationOwnsCurrentRidableObjectOpcode(t *testing.T) {
	module := dungeonNavigationGameplayModule()
	opcode := uint16(dnfenum.CmdPacketOperateRidableObject)
	if module.LegacyHandlers[opcode] == nil || module.UpperHandlers[opcode] == nil {
		t.Fatalf("op316 routes legacy=%t upper=%t", module.LegacyHandlers[opcode] != nil, module.UpperHandlers[opcode] != nil)
	}
}

func TestCurrentDungeonStaticRidablePlannerIgnoresUnprovenPassiveObjects(t *testing.T) {
	room := worldmap.DungeonRoom{
		Coordinate: worldmap.RoomCoordinate{X: 1, Y: 1},
		Map: &worldmap.ResolvedMap{Map: worldmap.Map{
			ID:             999,
			PassiveObjects: []worldmap.PassiveObject{{ObjectID: 99, X: 1, Y: 2}},
		}},
	}
	owner, entries, next, err := planCurrentDungeonStaticRidableObjects([]worldmap.DungeonRoom{room}, 500)
	if err != nil || owner != nil || len(entries) != 0 || next != 500 {
		t.Fatalf("unproven passive result owner=%+v entries=%+v next=%d err=%v", owner, entries, next, err)
	}
}
