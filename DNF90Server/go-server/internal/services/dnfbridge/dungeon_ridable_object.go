package dnfbridge

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"

	"longheng.io/server/internal/modules/dnf/worldmap"
)

var (
	errDungeonRidableObjectCoordinate = errors.New("dnf dungeon ridable-object room coordinate exceeds current EXE range")
	errDungeonRidableObjectDefinition = errors.New("dnf dungeon ridable-object definition is invalid")
	errDungeonRidableObjectKeyRange   = errors.New("dnf dungeon ridable-object key exceeds current EXE range")
	errDungeonRidableObjectPosition   = errors.New("dnf dungeon ridable-object map position exceeds current EXE range")
)

const (
	// Current NoPack resolves op29 association rows through
	// sub_235E570(category,index). Category 1 is the map passive-object table.
	currentDungeonRidablePassiveObjectCategory uint32 = 1
	// Balgur Jump is the clickable mount used by the desert Time Rift room
	// (map 80275) and the ordinary Gust Zone counterpart (map 33746).
	currentDungeonBalgurJumpObjectID int64 = 74266
)

type currentDungeonRidableObject struct {
	Room             worldmap.RoomCoordinate
	MapID            int64
	ObjectCategory   uint32
	ObjectIndex      uint32
	RuntimeObjectKey uint32
	LastOperation    byte
	OperationCount   uint64
}

type currentDungeonRidableObjectOwner struct {
	byObjectKey map[uint32]currentDungeonRidableObject
}

type currentDungeonRidableObjectRequest struct {
	Operation        byte
	RuntimeObjectKey uint32
	RoomX            byte
	RoomY            byte
}

// planCurrentDungeonStaticRidableObjects freezes every proven static rider in
// the selected layout before the first op29 is sent. The client does NOT filter
// the association table by room, so each row is emitted with the room that owns
// it (RoomX/RoomY) and with the object's own `[passive object]` position as the
// wire pair; the senders scope the table with currentDungeonRidableGroupsForRoom.
// Later first visits and cached revisits reuse exactly the same key.
func planCurrentDungeonStaticRidableObjects(
	rooms []worldmap.DungeonRoom,
	firstObjectKey uint32,
) (*currentDungeonRidableObjectOwner, []currentDungeonStartMapRidableEntry, uint32, error) {
	entries := make([]currentDungeonStartMapRidableEntry, 0)
	owner := &currentDungeonRidableObjectOwner{byObjectKey: make(map[uint32]currentDungeonRidableObject)}
	nextObjectKey := firstObjectKey
	for _, room := range rooms {
		if room.Map == nil {
			continue
		}
		seenBalgur := false
		for _, object := range room.Map.Map.PassiveObjects {
			if object.ObjectID != currentDungeonBalgurJumpObjectID {
				continue
			}
			if seenBalgur {
				return nil, nil, firstObjectKey, fmt.Errorf(
					"%w: room=%s map=%d object=%d duplicate",
					errDungeonRidableObjectDefinition, room.Coordinate, room.Map.Map.ID, object.ObjectID,
				)
			}
			seenBalgur = true
			if room.Coordinate.X < 0 || room.Coordinate.X > math.MaxUint8 ||
				room.Coordinate.Y < 0 || room.Coordinate.Y > math.MaxUint8 {
				return nil, nil, firstObjectKey, fmt.Errorf(
					"%w: room=%s map=%d", errDungeonRidableObjectCoordinate, room.Coordinate, room.Map.Map.ID,
				)
			}
			if object.ObjectID <= 0 || object.ObjectID > math.MaxUint32 {
				return nil, nil, firstObjectKey, fmt.Errorf(
					"%w: room=%s map=%d object=%d",
					errDungeonRidableObjectDefinition, room.Coordinate, room.Map.Map.ID, object.ObjectID,
				)
			}
			if nextObjectKey == 0 || nextObjectKey > math.MaxUint16 {
				return nil, nil, firstObjectKey, fmt.Errorf(
					"%w: room=%s map=%d object_key=%d",
					errDungeonRidableObjectKeyRange, room.Coordinate, room.Map.Map.ID, nextObjectKey,
				)
			}
			// The wire row carries the map position, so an unusable `[passive
			// object]` position must fail the plan rather than materialize the
			// rider at the origin of whatever room is loading.
			if object.X < math.MinInt32 || object.X > math.MaxInt32 ||
				object.Y < math.MinInt32 || object.Y > math.MaxInt32 {
				return nil, nil, firstObjectKey, fmt.Errorf(
					"%w: room=%s map=%d object=%d pos=(%d,%d)",
					errDungeonRidableObjectPosition, room.Coordinate, room.Map.Map.ID, object.ObjectID, object.X, object.Y,
				)
			}
			rider := currentDungeonRidableObject{
				Room: room.Coordinate, MapID: room.Map.Map.ID,
				ObjectCategory: currentDungeonRidablePassiveObjectCategory,
				ObjectIndex:    uint32(object.ObjectID), RuntimeObjectKey: nextObjectKey,
			}
			owner.byObjectKey[nextObjectKey] = rider
			// Balgur is a passive object, so it takes the passive spawn mode in the
			// fifth row value -- the client's only branch on this row
			// (switch(row[4]) { case 0, case 1, default: drop }). Before this it
			// carried the runtime object key there and the row was discarded.
			entries = append(entries, currentDungeonStartMapRidableEntry{
				RoomX: uint32(room.Coordinate.X), RoomY: uint32(room.Coordinate.Y),
				PosX:             object.X,
				PosY:             object.Y,
				ObjectIndex:      rider.ObjectIndex,
				Faction:          currentDungeonRidableFaction("neutral"),
				SpawnMode:        currentDungeonRandomizedPassiveFamily,
				RuntimeObjectKey: rider.RuntimeObjectKey,
			})
			nextObjectKey++
		}
	}
	if len(entries) == 0 {
		return nil, nil, firstObjectKey, nil
	}
	return owner, entries, nextObjectKey, nil
}

func decodeCurrentDungeonRidableObjectRequest(body []byte) (currentDungeonRidableObjectRequest, error) {
	if len(body) != 7 {
		return currentDungeonRidableObjectRequest{}, fmt.Errorf("current EXE op316 body length=%d want=7", len(body))
	}
	request := currentDungeonRidableObjectRequest{
		Operation:        body[0],
		RuntimeObjectKey: binary.LittleEndian.Uint32(body[1:5]),
		RoomX:            body[5],
		RoomY:            body[6],
	}
	if request.Operation < 1 || request.Operation > 3 || request.RuntimeObjectKey == 0 {
		return currentDungeonRidableObjectRequest{}, fmt.Errorf(
			"current EXE op316 operation=%d object_key=%d", request.Operation, request.RuntimeObjectKey,
		)
	}
	return request, nil
}

func (owner *currentDungeonRidableObjectOwner) apply(
	request currentDungeonRidableObjectRequest,
	scene worldmap.DungeonRoomScene,
) (currentDungeonRidableObject, bool) {
	if owner == nil || owner.byObjectKey == nil ||
		int64(request.RoomX) != scene.Coordinate.X || int64(request.RoomY) != scene.Coordinate.Y {
		return currentDungeonRidableObject{}, false
	}
	rider, ok := owner.byObjectKey[request.RuntimeObjectKey]
	if !ok || rider.Room != scene.Coordinate || rider.MapID != scene.Map.Map.ID {
		return currentDungeonRidableObject{}, false
	}
	rider.LastOperation = request.Operation
	rider.OperationCount++
	owner.byObjectKey[request.RuntimeObjectKey] = rider
	return rider, true
}

func (s *Service) handleCurrentDungeonRidableObject(session *gameSession, body []byte) error {
	request, err := decodeCurrentDungeonRidableObjectRequest(body)
	if err != nil {
		s.logGameEvent(session, "game-dungeon-ridable-object-blocked",
			"body_len", len(body), "reason", "current_exe_op316_body_malformed", "error", err)
		return nil
	}
	if session == nil {
		return nil
	}
	session.dungeon.mu.Lock()
	defer session.dungeon.mu.Unlock()
	runtime := session.dungeon.runtime
	if runtime == nil || runtime.Session == nil || runtime.RidableObjects == nil ||
		!dungeonRuntimeOwnsCharacter(runtime, session.selectedCharacterID) {
		s.logGameEvent(session, "game-dungeon-ridable-object-blocked",
			"operation", request.Operation, "object_key", request.RuntimeObjectKey,
			"room_x", request.RoomX, "room_y", request.RoomY,
			"reason", "active_ridable_object_owner_missing")
		return nil
	}
	scene, ok := runtime.Session.Scene()
	if !ok {
		s.logGameEvent(session, "game-dungeon-ridable-object-blocked",
			"operation", request.Operation, "object_key", request.RuntimeObjectKey,
			"reason", "active_dungeon_scene_missing")
		return nil
	}
	rider, accepted := runtime.RidableObjects.apply(request, scene)
	if !accepted {
		s.logGameEvent(session, "game-dungeon-ridable-object-blocked",
			"operation", request.Operation, "object_key", request.RuntimeObjectKey,
			"room_x", request.RoomX, "room_y", request.RoomY,
			"active_room", scene.Coordinate.String(), "map_id", scene.Map.Map.ID,
			"reason", "ridable_object_key_or_room_mismatch")
		return nil
	}
	// The current EXE registers class0/op316 as DoNothing. C2S op316 is a
	// one-way rider-state notification; fabricating an ACK would target the
	// wrong packet namespace.
	s.logGameEvent(session, "game-dungeon-ridable-object-operation-accepted",
		"dungeon_id", runtime.Dungeon.ID, "maze_index", runtime.MazeIndex,
		"room", scene.Coordinate.String(), "map_id", scene.Map.Map.ID,
		"operation", request.Operation, "object_key", request.RuntimeObjectKey,
		"object_category", rider.ObjectCategory, "object_index", rider.ObjectIndex,
		"operation_count", rider.OperationCount,
		"response", "none_current_exe_class0_op316_do_nothing")
	return nil
}
