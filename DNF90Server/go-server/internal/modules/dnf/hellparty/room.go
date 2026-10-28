package hellparty

import (
	"errors"
	"fmt"

	"longheng.io/server/internal/modules/dnf/worldmap"
)

var (
	ErrRoomUnavailable = errors.New("dnf hell party room is unavailable")
	ErrRoomInvalid     = errors.New("dnf hell party room is invalid")
)

type WorldSource interface {
	FindDungeon(id int64) (worldmap.Dungeon, bool)
	FindMap(id int64) (worldmap.Map, bool)
}

type Pillar struct {
	ObjectID int64
	X        int64
	Y        int64
	Flags    int64
}

type Room struct {
	Coordinate worldmap.RoomCoordinate
	HellMap    worldmap.Map
	NormalMap  worldmap.Map
	Pillar     Pillar
	Entries    []MapEntry
}

func ResolveRoom(source WorldSource, topology *worldmap.DungeonTopology, dungeonID int64, mazeIndex int) (Room, error) {
	if source == nil || topology == nil {
		return Room{}, fmt.Errorf("%w: source or topology is nil", ErrRoomUnavailable)
	}
	dungeon, ok := source.FindDungeon(dungeonID)
	if !ok || mazeIndex < 0 || mazeIndex >= len(dungeon.Mazes) {
		return Room{}, fmt.Errorf("%w: dungeon=%d maze=%d", ErrRoomUnavailable, dungeonID, mazeIndex)
	}
	maze := dungeon.Mazes[mazeIndex]
	if !maze.SealDoorMapIndex.Set || maze.SealDoorMapIndex.Value <= 0 || len(maze.SealDoorPosition) != 2 {
		return Room{}, fmt.Errorf("%w: dungeon=%d maze=%d has no exact seal-door map/position", ErrRoomUnavailable, dungeonID, mazeIndex)
	}
	coordinate := worldmap.RoomCoordinate{X: maze.SealDoorPosition[0], Y: maze.SealDoorPosition[1]}
	topologyRoom, ok := topology.Room(coordinate)
	if !ok || topologyRoom.Map == nil {
		return Room{}, fmt.Errorf("%w: seal-door room=%s is absent or unresolved", ErrRoomInvalid, coordinate)
	}
	if topologyRoom.Boss {
		return Room{}, fmt.Errorf("%w: seal-door room=%s is a boss room", ErrRoomInvalid, coordinate)
	}
	hellMap, ok := source.FindMap(maze.SealDoorMapIndex.Value)
	if !ok {
		return Room{}, fmt.Errorf("%w: seal-door map=%d is absent", ErrRoomInvalid, maze.SealDoorMapIndex.Value)
	}
	room := Room{Coordinate: coordinate, HellMap: hellMap, NormalMap: topologyRoom.Map.Map}
	for _, object := range hellMap.SpecialPassiveObjects {
		if len(object.HellParty) == 0 {
			continue
		}
		if room.Pillar.ObjectID == 0 {
			room.Pillar = Pillar{
				ObjectID: object.ObjectID,
				X:        object.X,
				Y:        object.Y,
				Flags:    object.Flags,
			}
		}
		for _, entry := range object.HellParty {
			if entry.GroupID <= 0 || entry.GroupID > int64(^uint32(0)) ||
				entry.Rate < 0 || entry.Rate > int64(^uint32(0)) ||
				entry.Order <= 0 || entry.Order > int64(^uint32(0)) {
				return Room{}, fmt.Errorf("%w: map=%d hellparty entry=%+v", ErrRoomInvalid, hellMap.ID, entry)
			}
			room.Entries = append(room.Entries, MapEntry{
				GroupID: uint32(entry.GroupID),
				Rate:    uint32(entry.Rate),
				Order:   uint32(entry.Order),
			})
		}
	}
	if room.Pillar.ObjectID == 0 || len(room.Entries) == 0 {
		return Room{}, fmt.Errorf("%w: seal-door map=%d has no [hellparty] pillar", ErrRoomInvalid, hellMap.ID)
	}
	return room, nil
}

func ApplyRoom(topology *worldmap.DungeonTopology, room Room) error {
	if topology == nil || room.HellMap.ID <= 0 {
		return fmt.Errorf("%w: topology or Hell map is empty", ErrRoomInvalid)
	}
	return topology.ReplaceRoomMap(room.Coordinate, worldmap.ResolvedMap{
		Map:    room.HellMap,
		Source: worldmap.ResolutionHellParty,
	})
}
