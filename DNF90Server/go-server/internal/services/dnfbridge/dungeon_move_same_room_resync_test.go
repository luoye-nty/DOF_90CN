package dnfbridge

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"

	"longheng.io/server/internal/modules/dnf/dungeoncmd"
	"longheng.io/server/internal/modules/dnf/worldmap"
)

// The live client re-issues CHANGE MAP for the room it already occupies: a
// same-coordinate op45 with MoveKind=0. The plain adjacency validator rejects
// that on its own (|dx|+|dy| == 0), and the handler used to return without
// writing any packet at all, which froze the client mid-room-transition -- it
// could neither advance nor return to town. Live 2026-09-22 05:45:51Z, char 1,
// dungeon 7539 maze 3, room 2:1: one blocked request, zero replies.
//
// The room does not change, so the answer must be the cached START_MAP for the
// current room and nothing else: no scene/seed/object-key/fatigue movement.
func TestHandleDungeonMoveMapSameCoordinateAnswersCachedStartMapWithoutStateChange(t *testing.T) {
	service, runtime := prepareSyntheticMoveRuntime(t, false)
	service.options.gameUpperBodyCodec = gameUpperBodyCodecPlain
	conn := &bufferConn{}
	session := &gameSession{
		conn:                               conn,
		connID:                             "dungeon-same-room-resync-test",
		selectedCharacterID:                99,
		dungeon:                            dungeonSessionState{runtime: runtime},
		currentFinishLoadingStateSent:      true,
		currentFinishLoadingCompletionSent: true,
	}

	moveBody := func(x, y, kind byte) []byte {
		t.Helper()
		body := make([]byte, dungeoncmd.MoveMapRequestSize)
		body[0] = x
		body[1] = y
		body[10] = kind
		return body
	}

	// Walk into the room whose same-coordinate CHANGE MAP the client re-issues.
	conn.write.Reset()
	if err := service.handleDungeonMoveMap(session, moveBody(1, 0, 0)); err != nil {
		t.Fatalf("first visit move: %v", err)
	}
	firstPacket, rest := splitGameServerUpperPacket(t, conn.write.Bytes())
	rest = consumeOptionalCurrentDungeonFatigueRefreshForTest(t, rest)
	if firstPacket.Header.MsgID != currentDungeonStartNotification || len(rest) != 0 {
		t.Fatalf("first visit response header=%+v rest=%x", firstPacket.Header, rest)
	}
	scene, ok := runtime.Session.Scene()
	if !ok || scene.Coordinate != (worldmap.RoomCoordinate{X: 1, Y: 0}) || scene.Map.Map.ID != 101 {
		t.Fatalf("post-move scene=%+v ok=%t", scene, ok)
	}

	// The same-coordinate request must fail the plain planner, otherwise this
	// regression test is not exercising the resync path at all.
	sameRequest, err := dungeoncmd.DecodeMoveMapRequest(moveBody(1, 0, 0))
	if err != nil {
		t.Fatal(err)
	}
	if _, planErr := service.planCurrentDungeonMove(runtime, sameRequest); !errors.Is(planErr, worldmap.ErrRoomNotAdjacent) {
		t.Fatalf("same-coordinate preflight error=%v want ErrRoomNotAdjacent", planErr)
	}

	beforeRoom := runtime.Room
	beforeSeed := runtime.Seed
	beforeNextObjectKey := runtime.NextObjectKey
	beforeRooms := len(runtime.Rooms)
	beforeVisit := runtime.Rooms[runtimeDungeonRoomKeyFromScene(scene)]
	if beforeVisit == nil {
		t.Fatal("current room visit is missing before the resync")
	}
	beforeFatigue := beforeVisit.FatigueConsumed

	conn.write.Reset()
	if err := service.handleDungeonMoveMap(session, moveBody(1, 0, 0)); err != nil {
		t.Fatalf("same-coordinate move: %v", err)
	}
	if conn.write.Len() == 0 {
		t.Fatal("same-coordinate move wrote no packet: the client would wait forever")
	}
	packet, rest := splitGameServerUpperPacket(t, conn.write.Bytes())
	rest = consumeOptionalCurrentDungeonFatigueRefreshForTest(t, rest)
	if packet.Header.MsgID != currentDungeonStartNotification || packet.Header.Classification != 0 || len(rest) != 0 {
		t.Fatalf("resync response header=%+v rest=%x", packet.Header, rest)
	}
	// Cached payload for the CURRENT coordinate: X, Y, operation, seed, ...,
	// mode. A full rebuild here would respawn the room the client just cleared.
	if len(packet.Body) != 16 ||
		packet.Body[0] != 1 || packet.Body[1] != 0 ||
		packet.Body[2] != currentDungeonStartMapOperationCurrent ||
		binary.LittleEndian.Uint32(packet.Body[3:7]) != beforeSeed ||
		packet.Body[13] != currentDungeonStartMapPayloadCached {
		t.Fatalf("resync start-map body=%x want cached current-room(1:0) op29 seed=%d", packet.Body, beforeSeed)
	}

	// Re-armed FINISH_LOADING handshake, or the client's reload stalls.
	if session.currentFinishLoadingStateSent || session.currentFinishLoadingCompletionSent {
		t.Fatalf("resync did not re-arm the loading handshake: state=%t completion=%t",
			session.currentFinishLoadingStateSent, session.currentFinishLoadingCompletionSent)
	}

	afterScene, ok := runtime.Session.Scene()
	if !ok || afterScene.Coordinate != scene.Coordinate || afterScene.Map.Map.ID != scene.Map.Map.ID {
		t.Fatalf("resync moved the scene: before=%s/%d after=%s/%d",
			scene.Coordinate, scene.Map.Map.ID, afterScene.Coordinate, afterScene.Map.Map.ID)
	}
	if runtime.Room != beforeRoom || runtime.Seed != beforeSeed || runtime.NextObjectKey != beforeNextObjectKey {
		t.Fatalf("resync mutated room owner: room=%p/%p seed=%d/%d next_object_key=%d/%d",
			runtime.Room, beforeRoom, runtime.Seed, beforeSeed, runtime.NextObjectKey, beforeNextObjectKey)
	}
	if len(runtime.Rooms) != beforeRooms {
		t.Fatalf("resync mutated the room cache: before=%d after=%d", beforeRooms, len(runtime.Rooms))
	}
	afterVisit := runtime.Rooms[runtimeDungeonRoomKeyFromScene(afterScene)]
	if afterVisit != beforeVisit || afterVisit.FatigueConsumed != beforeFatigue {
		t.Fatalf("resync replaced or re-charged the current room visit: before=%p/%t after=%p/%t",
			beforeVisit, beforeFatigue, afterVisit, afterVisit.FatigueConsumed)
	}
	if runtime.LayeredMapActive || runtime.LayeredMapIndex != -1 || len(runtime.LayerChains) != 0 {
		t.Fatalf("resync touched layer state: active=%t index=%d chains=%d",
			runtime.LayeredMapActive, runtime.LayeredMapIndex, len(runtime.LayerChains))
	}
}

// A genuinely invalid CHANGE MAP must still be refused, not silently answered
// with a room payload: the resync is scoped to the current coordinate only.
func TestHandleDungeonMoveMapNonAdjacentTargetStaysBlocked(t *testing.T) {
	service, runtime := prepareSyntheticMoveRuntime(t, false)
	service.options.gameUpperBodyCodec = gameUpperBodyCodecPlain
	conn := &bufferConn{}
	session := &gameSession{
		conn:                               conn,
		connID:                             "dungeon-non-adjacent-blocked-test",
		selectedCharacterID:                99,
		dungeon:                            dungeonSessionState{runtime: runtime},
		currentFinishLoadingStateSent:      true,
		currentFinishLoadingCompletionSent: true,
	}
	body := make([]byte, dungeoncmd.MoveMapRequestSize)
	body[0] = 3
	body[1] = 3
	if err := service.handleDungeonMoveMap(session, body); err != nil {
		t.Fatalf("non-adjacent move: %v", err)
	}
	if conn.write.Len() != 0 {
		t.Fatalf("non-adjacent target still wrote a packet: %x", conn.write.Bytes())
	}
	scene, ok := runtime.Session.Scene()
	if !ok || scene.Coordinate != (worldmap.RoomCoordinate{X: 0, Y: 0}) {
		t.Fatalf("non-adjacent move changed the scene: %+v ok=%t", scene, ok)
	}
	if !bytes.Equal(body[:2], []byte{3, 3}) {
		t.Fatalf("test body mutated: %x", body[:2])
	}
}
