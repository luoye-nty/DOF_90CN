package dnfbridge

import (
	"errors"
	"fmt"
	"strings"

	"longheng.io/server/internal/modules/dnf/worldmap"
)

var (
	errDungeonStartMapCoordinateRange = errors.New("dnf start-map coordinate exceeds current EXE range")
	errDungeonStartMapMapIDRange      = errors.New("dnf start-map map id exceeds current EXE range")
	errDungeonStartMapActorCount      = errors.New("dnf start-map actor count exceeds current EXE range")
	errDungeonStartMapObjectKeyRange  = errors.New("dnf start-map actor object key exceeds current EXE range")
	errDungeonStartMapActorCodeRange  = errors.New("dnf start-map actor code exceeds current EXE range")
	errDungeonStartMapMonsterRank     = errors.New("dnf start-map monster rank is unsupported")
	errDungeonStartMapMonsterLevel    = errors.New("dnf start-map monster level is invalid")
	errDungeonStartMapOpaqueHostile   = errors.New("dnf start-map contains an unsupported hostile actor")
	errDungeonStartMapSpecialObject   = errors.New("dnf start-map contains an unsupported special passive object")
	errDungeonStartMapSceneMismatch   = errors.New("dnf start-map scene does not match its runtime owner")
	errDungeonStartMapExtraCount      = errors.New("dnf start-map extra entry count exceeds current EXE range")
	errDungeonStartMapGroupCount      = errors.New("dnf start-map ridable group count exceeds current EXE range")
)

const (
	currentDungeonNPCActorType byte = 4

	// The highest level the current client accepts for a dungeon monster.
	// A resolved [monster] level at or above this is pinned here instead of
	// being rejected: the PVF ships rows that mean "cap it", not "fail".
	currentDungeonMonsterLevelMax = 200

	currentDungeonStartMapOperationCurrent      byte = 0
	currentDungeonStartMapOperationAdvanceLayer byte = 1
	currentDungeonStartMapOperationRestoreBase  byte = 2

	currentDungeonStartMapPayloadCached  byte = 0
	currentDungeonStartMapPayloadBuild   byte = 1
	currentDungeonStartMapPayloadRefresh byte = 2
)

type currentDungeonStartMapActor struct {
	TemplateOrder uint16
	PacketIndex   uint32
	ObjectKey     uint16
	Code          uint32
	Level         byte
	Type          byte
	Flag0         byte
	Flag1         byte
	ExtraState    uint32
	// Blocking is the current EXE's reverse-polarity room-clear byte: zero
	// makes an ordinary monster participate in the alive scan; one skips it.
	Blocking byte
}

// The current EXE reads this row as 21 bytes. The older C# implementation
// wrote a 19-byte row and is not wire-compatible here.
type currentDungeonStartMapExtraEntry struct {
	PassiveObjectIndex byte
	GlobalSequence     uint32
	ItemID             uint32
	StackCount         uint32
	Endurance          uint16
	AmplifyType        byte
	AmplifyValue       uint16
	ExtendedValue      uint16
	ExtendedFlag       byte
}

// currentDungeonStartMapRidableEntry is one 0x1D479A0 (op29) association row.
//
// The row is five u32 and the current client branches on the LAST one only:
// sub_2359970 (0x023599E3 mov eax,[edi+esi+0x10]) does
// `sub eax,0 / je case0 ; dec eax / jne next row`, i.e.
// `switch(row[4]) { case 0, case 1, default: ignore the whole row }`. Writing a
// runtime object key there (this server used 402, 403, ...) made the client drop
// every row, which is why no randomized object was ever materialized -- including
// the ones on maps that were believed to work. 86JP's builder writes the same
// table as (PosX, PosY, ObjectIndex, Faction, SpawnMode) with SpawnMode in {0,1}.
//
// PosX/PosY are the MAP position of the object inside its room, i.e. the PVF
// `[pos]` pair -- NOT the room index. The row carries no room at all, so the
// server must scope the table to the room being loaded (see
// currentDungeonRidableGroupsForRoom); RoomX/RoomY below exist only for that
// server-side scoping and never reach the wire.
type currentDungeonStartMapRidableEntry struct {
	PosX        int64
	PosY        int64
	ObjectIndex uint32
	Faction     uint32
	SpawnMode   uint32
	// RoomX/RoomY are the PVF `[map]` coordinate of the candidate this row was
	// created from, and RuntimeObjectKey is the op316 bookkeeping key. Neither is
	// part of the wire row any more.
	RoomX            uint32
	RoomY            uint32
	RuntimeObjectKey uint32
}

// currentDungeonRidableGroupsForRoom scopes the op29 association table to the
// room being loaded.
//
// The client never filters these rows by room: sub_2359970 hands every row of
// every group to the slot spawner together with row[0]/row[1] as the spawn
// position, and it runs once per room entry (flag +0xC3C armed by sub_264A7F0).
// Announcing the whole table on every room therefore re-materialized every
// randomized object in every room -- live 2026-09-16T14:2xZ on EnergyCut (218):
// the Anton transmission device and the 58530 rider appeared in all 18 rooms at
// the wrong pixel position, and the boss room (0:2), which owns no candidate at
// all, inherited rows it must never receive. 86JP scopes server-side
// (DungeonMapHandler.GetRidableEntriesForRoom) and so does this.
//
// Group indexes are preserved because the client reads a u8 group count followed
// by each group's u8 row count; empty groups are legal, and a room with no
// object at all writes the same zero group count 86JP writes for an empty table.
func currentDungeonRidableGroupsForRoom(
	groups [][]currentDungeonStartMapRidableEntry,
	room worldmap.RoomCoordinate,
) [][]currentDungeonStartMapRidableEntry {
	if len(groups) == 0 {
		return nil
	}
	filtered := make([][]currentDungeonStartMapRidableEntry, len(groups))
	occupied := false
	for index, group := range groups {
		rows := make([]currentDungeonStartMapRidableEntry, 0, len(group))
		for _, entry := range group {
			if int64(entry.RoomX) != room.X || int64(entry.RoomY) != room.Y {
				continue
			}
			rows = append(rows, entry)
		}
		if len(rows) > 0 {
			occupied = true
		}
		filtered[index] = rows
	}
	if !occupied {
		return nil
	}
	return filtered
}

type currentDungeonStartMap struct {
	X                         byte
	Y                         byte
	LayeredRoomFlag           byte
	Seed                      uint32
	HellPartyMode             byte
	UnknownAfterHellPartyMode byte
	RoomStateValue            uint32
	RoomStateFlag             byte
	MapID                     uint32
	Actors                    []currentDungeonStartMapActor
	ExtraEntries              []currentDungeonStartMapExtraEntry
	HellPartyFogFlag          byte
	RidableGroups             [][]currentDungeonStartMapRidableEntry
	PartyMemberIndex          byte
}

// currentDungeonStartMapState contains protocol state that is not owned by a
// map PVF record. Callers must provide it explicitly; this layer does not
// invent a seed, mode, room state, drop, or ridable-object value.
type currentDungeonStartMapState struct {
	LayeredRoomFlag           byte
	Seed                      uint32
	HellPartyMode             byte
	UnknownAfterHellPartyMode byte
	RoomStateValue            uint32
	RoomStateFlag             byte
	ExtraEntries              []currentDungeonStartMapExtraEntry
	HellPartyFogFlag          byte
	RidableGroups             [][]currentDungeonStartMapRidableEntry
	PartyMemberIndex          byte
}

func currentDungeonStartMapFromRuntime(
	runtime *runtimeDungeonState,
	scene worldmap.DungeonRoomScene,
	state currentDungeonStartMapState,
) (currentDungeonStartMap, error) {
	if scene.Coordinate.X < 0 || scene.Coordinate.X > 0xff || scene.Coordinate.Y < 0 || scene.Coordinate.Y > 0xff {
		return currentDungeonStartMap{}, fmt.Errorf("%w: room=%s", errDungeonStartMapCoordinateRange, scene.Coordinate)
	}
	if scene.Map.Map.ID <= 0 || scene.Map.Map.ID > int64(^uint32(0)) {
		return currentDungeonStartMap{}, fmt.Errorf("%w: map=%d", errDungeonStartMapMapIDRange, scene.Map.Map.ID)
	}
	actors, err := currentDungeonStartMapActors(runtime, scene)
	if err != nil {
		return currentDungeonStartMap{}, err
	}
	return currentDungeonStartMap{
		X:                         byte(scene.Coordinate.X),
		Y:                         byte(scene.Coordinate.Y),
		LayeredRoomFlag:           state.LayeredRoomFlag,
		Seed:                      state.Seed,
		HellPartyMode:             state.HellPartyMode,
		UnknownAfterHellPartyMode: state.UnknownAfterHellPartyMode,
		RoomStateValue:            state.RoomStateValue,
		RoomStateFlag:             state.RoomStateFlag,
		MapID:                     uint32(scene.Map.Map.ID),
		Actors:                    actors,
		ExtraEntries:              append([]currentDungeonStartMapExtraEntry(nil), state.ExtraEntries...),
		HellPartyFogFlag:          state.HellPartyFogFlag,
		RidableGroups:             cloneCurrentDungeonRidableGroups(state.RidableGroups),
		PartyMemberIndex:          state.PartyMemberIndex,
	}, nil
}

func cloneCurrentDungeonRidableGroups(values [][]currentDungeonStartMapRidableEntry) [][]currentDungeonStartMapRidableEntry {
	if len(values) == 0 {
		return nil
	}
	result := make([][]currentDungeonStartMapRidableEntry, len(values))
	for index, group := range values {
		result[index] = append([]currentDungeonStartMapRidableEntry(nil), group...)
	}
	return result
}

func (packet currentDungeonStartMap) Build() ([]byte, error) {
	if len(packet.Actors) > 0xff {
		return nil, fmt.Errorf("%w: count=%d", errDungeonStartMapActorCount, len(packet.Actors))
	}
	if len(packet.ExtraEntries) > 0xff {
		return nil, fmt.Errorf("%w: count=%d", errDungeonStartMapExtraCount, len(packet.ExtraEntries))
	}
	if len(packet.RidableGroups) > 0xff {
		return nil, fmt.Errorf("%w: count=%d", errDungeonStartMapGroupCount, len(packet.RidableGroups))
	}
	for groupIndex, group := range packet.RidableGroups {
		if len(group) > 0xff {
			return nil, fmt.Errorf("%w: group=%d count=%d", errDungeonStartMapGroupCount, groupIndex, len(group))
		}
	}

	var writer packetWriter
	writer.writeByte(packet.X)
	writer.writeByte(packet.Y)
	writer.writeByte(packet.LayeredRoomFlag)
	writer.writeUint32(packet.Seed)
	writer.writeByte(packet.HellPartyMode)
	writer.writeByte(packet.UnknownAfterHellPartyMode)
	writer.writeUint32(packet.RoomStateValue)
	writer.writeByte(packet.RoomStateFlag)
	writer.writeUint32(packet.MapID)
	writer.writeByte(byte(len(packet.Actors)))
	for _, actor := range packet.Actors {
		writer.writeUint16(actor.TemplateOrder)
		writer.writeUint32(actor.PacketIndex)
		writer.writeUint16(actor.ObjectKey)
		writer.writeUint32(actor.Code)
		writer.writeByte(actor.Level)
		writer.writeByte(actor.Type)
		writer.writeByte(actor.Flag0)
		writer.writeByte(actor.Flag1)
		writer.writeUint32(actor.ExtraState)
		writer.writeByte(actor.Blocking)
	}
	writer.writeByte(byte(len(packet.ExtraEntries)))
	for _, entry := range packet.ExtraEntries {
		writer.writeByte(entry.PassiveObjectIndex)
		writer.writeUint32(entry.GlobalSequence)
		writer.writeUint32(entry.ItemID)
		writer.writeUint32(entry.StackCount)
		writer.writeUint16(entry.Endurance)
		writer.writeByte(entry.AmplifyType)
		writer.writeUint16(entry.AmplifyValue)
		writer.writeUint16(entry.ExtendedValue)
		writer.writeByte(entry.ExtendedFlag)
	}
	writer.writeByte(packet.HellPartyFogFlag)
	writer.writeByte(byte(len(packet.RidableGroups)))
	for _, group := range packet.RidableGroups {
		writer.writeByte(byte(len(group)))
		for _, entry := range group {
			// (PosX, PosY, ObjectIndex, Faction, SpawnMode): the position is the
			// PVF `[pos]` pixel pair, never the room index -- see the entry type.
			writer.writeUint32(uint32(entry.PosX))
			writer.writeUint32(uint32(entry.PosY))
			writer.writeUint32(entry.ObjectIndex)
			writer.writeUint32(entry.Faction)
			// The client's only branch on this row: 0 = monster spawn, 1 = passive
			// object spawn. Anything else discards the row.
			writer.writeUint32(entry.SpawnMode)
		}
	}
	writer.writeByte(packet.PartyMemberIndex)
	return writer.bytes(), nil
}

// currentDungeonCachedStartMap is the exact mode0 branch consumed by the
// current client. It stops before MapID/actor rows, then reads only the ridable
// group count and party-member index, for a fixed 16-byte body.
type currentDungeonCachedStartMap struct {
	X                         byte
	Y                         byte
	Operation                 byte
	Seed                      uint32
	HellPartyMode             byte
	UnknownAfterHellPartyMode byte
	RoomStateValue            uint32
	RidableGroupCount         byte
	PartyMemberIndex          byte
}

func (packet currentDungeonCachedStartMap) Build() ([]byte, error) {
	if packet.Operation > currentDungeonStartMapOperationRestoreBase {
		return nil, fmt.Errorf("unsupported cached start-map operation=%d", packet.Operation)
	}
	var writer packetWriter
	writer.writeByte(packet.X)
	writer.writeByte(packet.Y)
	writer.writeByte(packet.Operation)
	writer.writeUint32(packet.Seed)
	writer.writeByte(packet.HellPartyMode)
	writer.writeByte(packet.UnknownAfterHellPartyMode)
	writer.writeUint32(packet.RoomStateValue)
	writer.writeByte(currentDungeonStartMapPayloadCached)
	writer.writeByte(packet.RidableGroupCount)
	writer.writeByte(packet.PartyMemberIndex)
	body := writer.bytes()
	if len(body) != 16 {
		return nil, fmt.Errorf("cached start-map body length=%d want=16", len(body))
	}
	return body, nil
}

func currentDungeonStartMapActors(
	runtime *runtimeDungeonState,
	scene worldmap.DungeonRoomScene,
) ([]currentDungeonStartMapActor, error) {
	if runtime == nil || runtime.Room == nil {
		return nil, errDungeonWorldMapUnavailable
	}
	snapshot := runtime.Room.Snapshot()
	if snapshot.Coordinate != scene.Coordinate || snapshot.MapID != scene.Map.Map.ID || len(snapshot.Monsters) != len(scene.Monsters) {
		return nil, fmt.Errorf("%w: runtime_room=%s runtime_map=%d scene_room=%s scene_map=%d runtime_monsters=%d scene_monsters=%d",
			errDungeonStartMapSceneMismatch,
			snapshot.Coordinate,
			snapshot.MapID,
			scene.Coordinate,
			scene.Map.Map.ID,
			len(snapshot.Monsters),
			len(scene.Monsters),
		)
	}
	if snapshot.SpecialObjectCount != len(scene.SpecialPassiveObjects) || snapshot.AICharacterCount != len(scene.AICharacters) {
		return nil, fmt.Errorf("%w: planned_special=%d scene_special=%d planned_AI=%d scene_AI=%d",
			errDungeonStartMapSceneMismatch,
			snapshot.SpecialObjectCount,
			len(scene.SpecialPassiveObjects),
			snapshot.AICharacterCount,
			len(scene.AICharacters),
		)
	}
	if len(snapshot.OpaqueHostiles) != 0 {
		return nil, fmt.Errorf("%w: count=%d", errDungeonStartMapOpaqueHostile, len(snapshot.OpaqueHostiles))
	}
	if len(snapshot.Monsters)+len(snapshot.ExtendedActors) > 0xff {
		return nil, fmt.Errorf("%w: normal=%d extended=%d", errDungeonStartMapActorCount, len(snapshot.Monsters), len(snapshot.ExtendedActors))
	}

	actors := make([]currentDungeonStartMapActor, 0, len(snapshot.Monsters)+len(snapshot.ExtendedActors))
	for index, monster := range snapshot.Monsters {
		if monster.ObjectKey == 0 || monster.ObjectKey > uint32(^uint16(0)) {
			return nil, fmt.Errorf("%w: index=%d object_key=%d", errDungeonStartMapObjectKeyRange, index, monster.ObjectKey)
		}
		if monster.Spawn.MonsterID <= 0 || monster.Spawn.MonsterID > int64(^uint32(0)) {
			return nil, fmt.Errorf("%w: index=%d code=%d", errDungeonStartMapActorCodeRange, index, monster.Spawn.MonsterID)
		}
		actorType, err := currentDungeonMonsterActorType(monster.Spawn)
		if err != nil {
			return nil, fmt.Errorf("monster index %d code %d: %w", index, monster.Spawn.MonsterID, err)
		}
		level, err := currentDungeonMonsterLevel(monster.Spawn, runtime.Dungeon.Metadata.BasisLevel)
		if err != nil {
			return nil, fmt.Errorf("monster index %d code %d: %w", index, monster.Spawn.MonsterID, err)
		}
		if runtime.raidSingleMonsterLevel > 0 && actorType != currentDungeonNPCActorType {
			level = runtime.raidSingleMonsterLevel
		}
		code := monster.Spawn.MonsterID
		blocking := monster.RoomClearSkipByte
		if actorType == currentDungeonNPCActorType {
			blocking = 1
		}
		actors = append(actors, currentDungeonStartMapActor{
			PacketIndex: uint32(index),
			ObjectKey:   uint16(monster.ObjectKey),
			Code:        uint32(code),
			Level:       level,
			Type:        actorType,
			Blocking:    blocking,
		})
	}
	for _, actor := range snapshot.ExtendedActors {
		packet := actor.Packet
		actors = append(actors, packet)
	}
	return actors, nil
}

func currentDungeonMonsterType(rank string) (byte, error) {
	switch strings.ToLower(strings.Trim(rank, "[] \t\r\n")) {
	case "normal", "dummy":
		return 0, nil
	case "champion":
		return 1, nil
	case "super champion", "superchampion":
		return 2, nil
	case "boss":
		return 3, nil
	default:
		return 0, fmt.Errorf("%w: rank=%q", errDungeonStartMapMonsterRank, rank)
	}
}

func currentDungeonMonsterActorType(spawn worldmap.MonsterSpawn) (byte, error) {
	if strings.EqualFold(strings.Trim(spawn.SuffixMarker, "[] \t\r\n"), "boss") {
		return 3, nil
	}
	if strings.EqualFold(strings.Trim(spawn.Rank, "[] \t\r\n"), "npc") {
		return currentDungeonNPCActorType, nil
	}
	return currentDungeonMonsterType(spawn.Rank)
}

func currentDungeonIsBossActorType(actorType byte) bool {
	// 86JP mirrors the old df_game_r kill_monster domain rule:
	// ordinary boss monsters are actor type 3, hostile AI/APC bosses are
	// actor type 8. Both mean "boss death can end combat"; do not restrict
	// the rule to PVF [monster] rows only.
	return actorType == 3 || actorType == 8
}

// currentDungeonMonsterLevel resolves one PVF [monster] row's level from its
// second and third columns. The second column (Spawn.Level) is only a switch:
// any nonzero value — 1, 2, 10 alike — makes the third column (Spawn.AutoLevel)
// a level relative to the dungeon's [basis level]; zero makes the third column
// the absolute level. The relative branch has no fallback: a dungeon without
// [basis level] cannot be resolved, and inventing one would hide the missing
// dependency behind plausible monsters. Every other row resolves, clamped.
func currentDungeonMonsterLevel(spawn worldmap.MonsterSpawn, basis worldmap.OptionalInt) (byte, error) {
	level := spawn.AutoLevel
	if spawn.Level != 0 {
		if !basis.Set {
			return 0, fmt.Errorf("%w: relative=%d auto=%d basis=unset", errDungeonStartMapMonsterLevel, spawn.Level, spawn.AutoLevel)
		}
		level += basis.Value
	}
	return clampCurrentDungeonMonsterLevel(level), nil
}

// clampCurrentDungeonMonsterLevel holds a resolved level inside the range the
// current client accepts. The lower bound of 1 is not cosmetic: the packet
// field is one byte, so a zero or negative row would otherwise be truncated
// into a 240+ monster.
func clampCurrentDungeonMonsterLevel(level int64) byte {
	if level >= currentDungeonMonsterLevelMax {
		return byte(currentDungeonMonsterLevelMax)
	}
	if level <= 0 {
		return 1
	}
	return byte(level)
}
