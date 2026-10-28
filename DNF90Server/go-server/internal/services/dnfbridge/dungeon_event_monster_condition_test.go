package dnfbridge

import (
	"bytes"
	"errors"
	"testing"

	"longheng.io/server/internal/modules/dnf/worldmap"
)

func TestCurrentDungeonTrapEventMonsterScopeUsesPVFMechanismShape(t *testing.T) {
	runtime, scene, room := currentDungeonTrapEventMonsterTestFixture()
	if !currentDungeonTrapEventMonsterScope(runtime, scene, room) {
		t.Fatal("PVF event-monster/trap room was not selected")
	}

	tests := []struct {
		name   string
		mutate func(*runtimeDungeonState, *worldmap.DungeonRoomScene, *runtimeDungeonRoom)
	}{
		{name: "missing dungeon definition", mutate: func(runtime *runtimeDungeonState, _ *worldmap.DungeonRoomScene, _ *runtimeDungeonRoom) {
			runtime.Dungeon.Metadata.EventMonsters = nil
		}},
		{name: "missing random-map selector", mutate: func(runtime *runtimeDungeonState, _ *worldmap.DungeonRoomScene, _ *runtimeDungeonRoom) {
			runtime.Dungeon.Metadata.EventMonsterRandomMap = worldmap.OptionalInt{}
		}},
		{name: "missing map positions", mutate: func(_ *runtimeDungeonState, scene *worldmap.DungeonRoomScene, _ *runtimeDungeonRoom) {
			scene.Map.Map.EventMonsterPositions = nil
		}},
		{name: "non-trap special child", mutate: func(_ *runtimeDungeonState, scene *worldmap.DungeonRoomScene, _ *runtimeDungeonRoom) {
			scene.SpecialPassiveObjects[0].Spawns[0].Kind = "[monster]"
		}},
		{name: "other room owner", mutate: func(_ *runtimeDungeonState, _ *worldmap.DungeonRoomScene, room *runtimeDungeonRoom) {
			room.mapID++
		}},
		{name: "boss entrance owns op312", mutate: func(runtime *runtimeDungeonState, _ *worldmap.DungeonRoomScene, _ *runtimeDungeonRoom) {
			runtime.Dungeon.Mazes[0].BossRoomEntranceCondition = &worldmap.BossRoomEntranceCondition{Action: "clear action"}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runtime, scene, room := currentDungeonTrapEventMonsterTestFixture()
			test.mutate(runtime, &scene, room)
			if currentDungeonTrapEventMonsterScope(runtime, scene, room) {
				t.Fatalf("unsupported mechanism shape was selected: runtime=%+v scene=%+v room=%+v", runtime.Dungeon.Metadata, scene.Map.Map, room.Snapshot())
			}
		})
	}
}

func TestCurrentDungeonTrapEventMonsterPublishesNativeConditionOnce(t *testing.T) {
	runtime, scene, room := currentDungeonTrapEventMonsterTestFixture()
	connection := &bufferConn{}
	session := &gameSession{conn: connection}
	service := &Service{}

	if err := service.publishCurrentDungeonTrapEventMonsterAfterStartMap(
		session, runtime, scene, room, "test_after_op29",
	); err != nil {
		t.Fatal(err)
	}
	packet, rest := splitGameServerUpperPacket(t, connection.write.Bytes())
	if len(rest) != 0 || packet.Header.Classification != 0 ||
		packet.Header.MsgID != currentDungeonCompleteConditionPassGateMsgID ||
		!bytes.Equal(packet.Body, []byte{0, 0, 0, 0, 0}) {
		t.Fatalf("event-monster packet class=%d msg=%d body=%x rest=%x",
			packet.Header.Classification, packet.Header.MsgID, packet.Body, rest)
	}
	if !room.Snapshot().EventMonsterConditionPublished {
		t.Fatal("successful op312 did not commit the room publication marker")
	}
	written := connection.write.Len()
	if err := service.publishCurrentDungeonTrapEventMonsterAfterStartMap(
		session, runtime, scene, room, "test_duplicate",
	); err != nil {
		t.Fatal(err)
	}
	if connection.write.Len() != written {
		t.Fatalf("duplicate publication wrote=%x", connection.write.Bytes()[written:])
	}
}

func TestCurrentDungeonTrapEventMonsterWriteFailureRemainsRetryable(t *testing.T) {
	runtime, scene, room := currentDungeonTrapEventMonsterTestFixture()
	wantErr := errors.New("event-monster op312 write failed")
	session := &gameSession{conn: &failNthDungeonWriteConn{failAt: 1, err: wantErr}}
	service := &Service{}
	if err := service.publishCurrentDungeonTrapEventMonsterAfterStartMap(
		session, runtime, scene, room, "test_failed_write",
	); !errors.Is(err, wantErr) {
		t.Fatalf("write failure=%v want=%v", err, wantErr)
	}
	if room.Snapshot().EventMonsterConditionPublished {
		t.Fatal("failed op312 write consumed the room publication marker")
	}

	connection := &bufferConn{}
	session.conn = connection
	if err := service.publishCurrentDungeonTrapEventMonsterAfterStartMap(
		session, runtime, scene, room, "test_retry",
	); err != nil {
		t.Fatal(err)
	}
	packet, rest := splitGameServerUpperPacket(t, connection.write.Bytes())
	if len(rest) != 0 || packet.Header.MsgID != currentDungeonCompleteConditionPassGateMsgID ||
		!bytes.Equal(packet.Body, []byte{0, 0, 0, 0, 0}) {
		t.Fatalf("retry packet msg=%d body=%x rest=%x", packet.Header.MsgID, packet.Body, rest)
	}
}

func currentDungeonTrapEventMonsterTestFixture() (
	*runtimeDungeonState,
	worldmap.DungeonRoomScene,
	*runtimeDungeonRoom,
) {
	coordinate := worldmap.RoomCoordinate{X: 1, Y: 2}
	mapValue := worldmap.Map{
		ID: 58708,
		EventMonsterPositions: []worldmap.Point3{
			{X: 640, Y: 215}, {X: 865, Y: 262}, {X: 931, Y: 288}, {X: 753, Y: 277},
		},
		SpecialPassiveObjects: []worldmap.SpecialPassiveObject{{
			PassiveObject: worldmap.PassiveObject{ObjectID: 781},
			Spawns:        []worldmap.SpecialObjectSpawn{{Kind: "[trap]", Code: 48021}},
		}},
	}
	scene := worldmap.DungeonRoomScene{
		Coordinate:            coordinate,
		Map:                   worldmap.ResolvedMap{Map: mapValue},
		SpecialPassiveObjects: append([]worldmap.SpecialPassiveObject(nil), mapValue.SpecialPassiveObjects...),
	}
	room := &runtimeDungeonRoom{coordinate: coordinate, mapID: mapValue.ID}
	runtime := &runtimeDungeonState{
		Dungeon: worldmap.Dungeon{
			ID: 26,
			Metadata: worldmap.DungeonMetadata{
				EventMonsterRandomMap: worldmap.OptionalInt{Value: 0, Set: true},
				EventMonsters: []worldmap.DungeonEventMonsterDefinition{{
					Kind: "event monster", Values: []int64{1, 1000, 21100, 1, 1000, 1, 1, 5},
				}},
			},
			Mazes: []worldmap.Maze{{}},
		},
		MazeIndex: 0,
		Room:      room,
	}
	return runtime, scene, room
}
