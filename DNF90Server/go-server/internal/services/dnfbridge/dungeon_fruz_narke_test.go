package dnfbridge

import (
	"bytes"
	"testing"

	"longheng.io/server/internal/modules/dnf/dnfenum"
	"longheng.io/server/internal/modules/dnf/dungeoncmd"
	"longheng.io/server/internal/modules/dnf/repository"
	"longheng.io/server/internal/modules/dnf/worldmap"
)

func newCurrentFruzNarkeTestRuntime(t *testing.T) (*Service, *gameSession, *runtimeDungeonState, worldmap.DungeonRoomScene, uint16) {
	t.Helper()
	scene := worldmap.DungeonRoomScene{
		Coordinate: worldmap.RoomCoordinate{X: 2, Y: 1},
		Map:        worldmap.ResolvedMap{Map: worldmap.Map{ID: 76503}},
		PassiveObjects: []worldmap.PassiveObject{
			{ObjectID: currentFruzStationObstacleSmall},
			{ObjectID: currentFruzStationObstacleLarge},
			{ObjectID: currentFruzStationObstacleLarge},
			{ObjectID: currentFruzStationObstacleLarge},
			{ObjectID: currentFruzStationObstacleLarge},
			{ObjectID: currentFruzStationObstacleLarge},
		},
	}
	room := &runtimeDungeonRoom{coordinate: scene.Coordinate, mapID: scene.Map.Map.ID}
	narkeKey := uint16(405)
	narkeRoom := &runtimeDungeonRoom{
		coordinate: worldmap.RoomCoordinate{X: 0, Y: 1},
		mapID:      76500,
		extendedActors: []runtimeDungeonExtendedActor{{
			Kind:      runtimeDungeonActorAICharacter,
			ObjectKey: uint32(narkeKey),
			Packet: currentDungeonStartMapActor{
				ObjectKey: narkeKey,
				Code:      uint32(currentFruzStationNarkeActorID),
				Type:      5,
			},
			AICharacter: &worldmap.AICharacter{
				Code:    currentFruzStationNarkeActorID,
				Faction: "[character]",
				AIType:  "[normal]",
			},
			State: runtimeDungeonMonsterAnnounced,
		}},
	}
	runtime := &runtimeDungeonState{
		Request: dungeoncmd.SelectDungeonRequest{DungeonID: uint32(currentFruzStationDungeonID)},
		Dungeon: worldmap.Dungeon{
			ID: currentFruzStationDungeonID,
			Mazes: []worldmap.Maze{{
				QuestConnection: []int64{0, currentFruzStationNarkeQuestID, -1},
			}},
		},
		MazeIndex: 0,
		Character: repository.CharacterRecord{CharacterID: "19", AccountID: "acc"},
		Session:   &worldmap.DungeonSession{},
		Room:      room,
		Seed:      7,
		Rooms: map[runtimeDungeonRoomKey]*runtimeDungeonRoomVisit{
			runtimeDungeonRoomKeyFromScene(scene): {
				Scene: scene,
				Room:  room,
				Seed:  7,
			},
			{X: 0, Y: 1, MapID: 76500}: {
				Room: narkeRoom,
				Seed: 3,
			},
		},
	}
	conn := &bufferConn{}
	session := &gameSession{
		conn: conn, connID: "fruz-narke", selectedCharacterID: 19,
		dungeon: dungeonSessionState{runtime: runtime},
	}
	return &Service{options: options{gameUpperBodyCodec: gameUpperBodyCodecPlain}}, session, runtime, scene, narkeKey
}

func TestCurrentFruzStationNarkeObstacleDeathAcknowledgesEveryMapInstance(t *testing.T) {
	service, session, runtime, scene, narkeKey := newCurrentFruzNarkeTestRuntime(t)
	request := dungeoncmd.DieMonsterRequest{
		Layout:           dungeoncmd.DieMonsterRequestLayoutVariableCombat,
		RuntimeObjectKey: uint32(currentFruzStationObstacleLarge),
		OwnerObjectKey:   narkeKey,
		CombatEntryCount: 0,
	}
	roomBefore := runtime.Room.Snapshot()

	for attempt := 0; attempt < 6; attempt++ {
		handled, err := service.handleCurrentFruzStationNarkeObstacleDeathLocked(
			session, runtime, scene, request,
		)
		if err != nil || !handled {
			t.Fatalf("attempt %d handled=%t err=%v", attempt+1, handled, err)
		}
	}

	visit := runtime.Rooms[runtimeDungeonRoomKeyFromScene(scene)]
	if got := visit.QuestPassiveObjectDeaths[currentFruzStationObstacleLarge]; got != 5 {
		t.Fatalf("retired obstacle count=%d want=5", got)
	}
	wantBody, err := buildCurrentDungeonDeathNotificationBody(
		uint32(currentFruzStationObstacleLarge), currentDungeonDeathResponseAICharacter,
	)
	if err != nil {
		t.Fatal(err)
	}
	remaining := session.conn.(*bufferConn).write.Bytes()
	for packetIndex := 0; packetIndex < 6; packetIndex++ {
		packet, rest := splitGameServerUpperPacket(t, remaining)
		if packet.Header.MsgID != uint16(dnfenum.CmdPacketNotifyDieMonster) ||
			packet.Header.Classification != 0 || !bytes.Equal(packet.Body, wantBody) {
			t.Fatalf("packet %d header=%+v body=%x want=%x", packetIndex+1, packet.Header, packet.Body, wantBody)
		}
		remaining = rest
	}
	if len(remaining) != 0 {
		t.Fatalf("trailing packets=%x", remaining)
	}
	roomAfter := runtime.Room.Snapshot()
	if len(roomAfter.Monsters) != len(roomBefore.Monsters) ||
		len(roomAfter.ExtendedActors) != len(roomBefore.ExtendedActors) {
		t.Fatalf("obstacle receipt mutated room actors: before=%+v after=%+v", roomBefore, roomAfter)
	}
}

func TestCurrentFruzStationNarkeObstacleDeathAcceptsSentinelOwner(t *testing.T) {
	service, session, runtime, scene, _ := newCurrentFruzNarkeTestRuntime(t)
	handled, err := service.handleCurrentFruzStationNarkeObstacleDeathLocked(
		session,
		runtime,
		scene,
		dungeoncmd.DieMonsterRequest{
			Layout:           dungeoncmd.DieMonsterRequestLayoutVariableCombat,
			RuntimeObjectKey: uint32(currentFruzStationObstacleSmall),
			OwnerObjectKey:   ^uint16(0),
		},
	)
	if err != nil || !handled {
		t.Fatalf("sentinel owner handled=%t err=%v", handled, err)
	}
}

func TestCurrentFruzStationNarkeObstacleDeathRejectsForeignShapes(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*runtimeDungeonState, *worldmap.DungeonRoomScene, *dungeoncmd.DieMonsterRequest)
	}{
		{name: "wrong dungeon", mutate: func(runtime *runtimeDungeonState, _ *worldmap.DungeonRoomScene, _ *dungeoncmd.DieMonsterRequest) {
			runtime.Dungeon.ID++
		}},
		{name: "wrong quest", mutate: func(runtime *runtimeDungeonState, _ *worldmap.DungeonRoomScene, _ *dungeoncmd.DieMonsterRequest) {
			runtime.Dungeon.Mazes[0].QuestConnection[1]++
		}},
		{name: "wrong map", mutate: func(_ *runtimeDungeonState, scene *worldmap.DungeonRoomScene, _ *dungeoncmd.DieMonsterRequest) {
			scene.Map.Map.ID = 76505
		}},
		{name: "foreign target", mutate: func(_ *runtimeDungeonState, _ *worldmap.DungeonRoomScene, request *dungeoncmd.DieMonsterRequest) {
			request.RuntimeObjectKey = 16111
		}},
		{name: "foreign owner", mutate: func(_ *runtimeDungeonState, _ *worldmap.DungeonRoomScene, request *dungeoncmd.DieMonsterRequest) {
			request.OwnerObjectKey = 499
		}},
		{name: "fixed layout", mutate: func(_ *runtimeDungeonState, _ *worldmap.DungeonRoomScene, request *dungeoncmd.DieMonsterRequest) {
			request.Layout = dungeoncmd.DieMonsterRequestLayoutFixed62
		}},
		{name: "combat rows", mutate: func(_ *runtimeDungeonState, _ *worldmap.DungeonRoomScene, request *dungeoncmd.DieMonsterRequest) {
			request.CombatEntryCount = 1
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			service, session, runtime, scene, narkeKey := newCurrentFruzNarkeTestRuntime(t)
			request := dungeoncmd.DieMonsterRequest{
				Layout:           dungeoncmd.DieMonsterRequestLayoutVariableCombat,
				RuntimeObjectKey: uint32(currentFruzStationObstacleSmall),
				OwnerObjectKey:   narkeKey,
			}
			test.mutate(runtime, &scene, &request)
			handled, err := service.handleCurrentFruzStationNarkeObstacleDeathLocked(session, runtime, scene, request)
			if err != nil || handled {
				t.Fatalf("handled=%t err=%v", handled, err)
			}
			if session.conn.(*bufferConn).write.Len() != 0 {
				t.Fatalf("rejected shape emitted packets=%x", session.conn.(*bufferConn).write.Bytes())
			}
		})
	}
}
