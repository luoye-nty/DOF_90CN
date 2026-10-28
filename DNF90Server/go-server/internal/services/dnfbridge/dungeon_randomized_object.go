package dnfbridge

import (
	"errors"
	"fmt"
	"strings"

	"longheng.io/server/internal/modules/dnf/worldmap"
)

var (
	errDungeonRandomizedObjectGroup      = errors.New("dnf dungeon randomized object group is invalid")
	errDungeonRandomizedObjectDefinition = errors.New("dnf dungeon randomized object definition is invalid")
	errDungeonRandomizedObjectTeam       = errors.New("dnf dungeon randomized object team is unsupported")
	errDungeonRandomizedObjectPosition   = errors.New("dnf dungeon randomized object position is invalid")
	errDungeonRandomizedObjectKeyRange   = errors.New("dnf dungeon randomized object key exceeds current EXE range")
)

const (
	currentDungeonRandomizedMonsterFamily uint32 = 0
	currentDungeonRandomizedPassiveFamily uint32 = 1
	currentDungeonExplicitPositionMode    byte   = 5
)

// currentDungeonRandomizedObjectSelection is one DGN [randomized object
// creation] choice.  The current client consumes the same choice in three
// different packets: op28 publishes its maze coordinate, op479 materializes
// eligible explicitly-positioned creatures, and op29 associates the resulting
// object key with the family/definition pair.
type currentDungeonRandomizedObjectSelection struct {
	GroupIndex     int
	CandidateIndex int
	Room           worldmap.RoomCoordinate
	Position       worldmap.Point
	Family         uint32
	DefinitionID   uint32
	ObjectKey      uint32
	Team           string
	Level          byte
}

// currentDungeonRandomizedObjectSkip records one [randomized object creation]
// candidate this server does not materialize. Skipping a kind is deliberately not
// fatal to the map: EnergyCut (Anton phase two map 218) has a group of eight
// candidates that are ALL the same passive object with team "monster"
// (18865 ActionObject/Map/Anton/Phase2/Transmission/Transmission_S.obj), so
// failing the group closed made the gate permanently unenterable -- live
// 2026-09-16T13:13Z, four attempts, each
// "dnf dungeon randomized object team is unsupported: group=0 candidate=N family=1
// team=\"monster\"". The unsupported actors are simply not announced; every other
// candidate in the map still is.
type currentDungeonRandomizedObjectSkip struct {
	GroupIndex     int
	CandidateIndex int
	DefinitionID   uint32
	Family         uint32
	Team           string
}

func planCurrentDungeonRandomizedObjects(
	groups []worldmap.RandomizedObjectScript,
	seed uint32,
	objectKeyBase uint32,
	basis worldmap.OptionalInt,
	catalog *pvfDungeonMonsterCatalog,
) ([][]currentDungeonRandomizedObjectSelection, [][]currentDungeonStartMapRidableEntry, uint32, []currentDungeonRandomizedObjectSkip, error) {
	if len(groups) == 0 {
		return nil, nil, objectKeyBase, nil, nil
	}
	if catalog == nil {
		return nil, nil, objectKeyBase, nil, errDungeonMonsterCatalogUnavailable
	}
	if len(groups) > int(^uint8(0)) {
		return nil, nil, objectKeyBase, nil, fmt.Errorf("%w: group_count=%d", errDungeonRandomizedObjectGroup, len(groups))
	}
	if !basis.Set || basis.Value <= 1 || basis.Value > int64(^uint8(0))+1 {
		return nil, nil, objectKeyBase, nil, fmt.Errorf("%w: basis_level=%d set=%t", errDungeonRandomizedObjectDefinition, basis.Value, basis.Set)
	}

	selectedCount := uint32(0)
	for groupIndex, group := range groups {
		selectCount := int(group.SelectCount.Value)
		if !group.SelectCount.Set || selectCount <= 0 || selectCount > len(group.Objects) || len(group.Objects) > int(^uint8(0)) {
			return nil, nil, objectKeyBase, nil, fmt.Errorf(
				"%w: group=%d select=%d candidates=%d set=%t",
				errDungeonRandomizedObjectGroup, groupIndex, selectCount, len(group.Objects), group.SelectCount.Set,
			)
		}
		selectedCount += uint32(selectCount)
	}
	const objectKeyLimit = uint32(^uint16(0)) + 1
	if selectedCount != 0 && (objectKeyBase == 0 || objectKeyBase >= objectKeyLimit || selectedCount > objectKeyLimit-objectKeyBase) {
		return nil, nil, objectKeyBase, nil, fmt.Errorf(
			"%w: first=%d count=%d", errDungeonRandomizedObjectKeyRange, objectKeyBase, selectedCount,
		)
	}

	selections := make([][]currentDungeonRandomizedObjectSelection, len(groups))
	associations := make([][]currentDungeonStartMapRidableEntry, len(groups))
	var skipped []currentDungeonRandomizedObjectSkip
	nextObjectKey := objectKeyBase
	for groupIndex, group := range groups {
		indices := currentDungeonRandomizedObjectSelectionIndices(
			len(group.Objects), int(group.SelectCount.Value), seed, groupIndex,
		)
		selections[groupIndex] = make([]currentDungeonRandomizedObjectSelection, 0, len(indices))
		associations[groupIndex] = make([]currentDungeonStartMapRidableEntry, 0, len(indices))
		for _, candidateIndex := range indices {
			candidate := group.Objects[candidateIndex]
			if candidate.MazeCoordinate == nil || candidate.MazeCoordinate.X < 0 || candidate.MazeCoordinate.X > 0xff ||
				candidate.MazeCoordinate.Y < 0 || candidate.MazeCoordinate.Y > 0xff {
				return nil, nil, objectKeyBase, nil, fmt.Errorf(
					"%w: group=%d candidate=%d map=%v", errDungeonRandomizedObjectPosition, groupIndex, candidateIndex, candidate.MazeCoordinate,
				)
			}
			if candidate.Position == nil || candidate.Position.X < -1<<31 || candidate.Position.X > 1<<31-1 ||
				candidate.Position.Y < -1<<31 || candidate.Position.Y > 1<<31-1 {
				return nil, nil, objectKeyBase, nil, fmt.Errorf(
					"%w: group=%d candidate=%d position=%v", errDungeonRandomizedObjectPosition, groupIndex, candidateIndex, candidate.Position,
				)
			}
			if !candidate.ObjectIndex.Set || candidate.ObjectIndex.Value <= 0 || candidate.ObjectIndex.Value > int64(^uint32(0)) {
				return nil, nil, objectKeyBase, nil, fmt.Errorf(
					"%w: group=%d candidate=%d definition=%d set=%t",
					errDungeonRandomizedObjectDefinition, groupIndex, candidateIndex, candidate.ObjectIndex.Value, candidate.ObjectIndex.Set,
				)
			}
			team := normalizeDungeonPVFSymbol(candidate.Team)
			family, found, err := catalog.randomizedObjectFamily(candidate.ObjectIndex.Value)
			if err != nil {
				return nil, nil, objectKeyBase, nil, fmt.Errorf("resolve randomized object group=%d candidate=%d: %w", groupIndex, candidateIndex, err)
			}
			if !found {
				skipped = append(skipped, currentDungeonRandomizedObjectSkip{
					GroupIndex: groupIndex, CandidateIndex: candidateIndex,
					DefinitionID: uint32(candidate.ObjectIndex.Value), Team: team,
				})
				continue
			}
			// family is the spawn mode the client branches on (86JP
			// DungeonRandomizedObjectDefinition.ResolveSpawnMode: 0 = monster,
			// 1 = passive object; ResolveSpawnMode(18865) == 1, the EnergyCut
			// transmission). Neither the team nor the family filters the candidate
			// any more: the row's fifth value is what decides, so filtering here
			// would only hide objects the client can render.
			spawnMode := family
			selection := currentDungeonRandomizedObjectSelection{
				GroupIndex: groupIndex, CandidateIndex: candidateIndex,
				Room:     worldmap.RoomCoordinate{X: candidate.MazeCoordinate.X, Y: candidate.MazeCoordinate.Y},
				Position: *candidate.Position, Family: family,
				DefinitionID: uint32(candidate.ObjectIndex.Value), ObjectKey: nextObjectKey,
				Team: team,
				// The explicit-creature descriptor uses the current EXE's zero-based
				// dungeon level byte.  For Gent North Gate basis 69 this is the
				// captured 0x44 descriptor used by AT-5T.
				Level: byte(basis.Value - 1),
			}
			selections[groupIndex] = append(selections[groupIndex], selection)
			associations[groupIndex] = append(associations[groupIndex], currentDungeonStartMapRidableEntry{
				RoomX: uint32(selection.Room.X), RoomY: uint32(selection.Room.Y),
				// The wire row wants the PVF `[pos]` pair, and the room pair above
				// only scopes the row to the room that owns this candidate.
				PosX:        selection.Position.X,
				PosY:        selection.Position.Y,
				ObjectIndex: selection.DefinitionID,
				Faction:     currentDungeonRidableFaction(team),
				SpawnMode:   spawnMode,
				// Server-side bookkeeping only; the client never sees this.
				RuntimeObjectKey: selection.ObjectKey,
			})
			nextObjectKey++
		}
	}
	return selections, associations, nextObjectKey, skipped, nil
}

// currentDungeonRidableFaction maps the PVF team word onto the fourth value of an
// op29 association row. 86JP writes item.Faction there; the current client does
// not branch on it (only on SpawnMode), so the mapping stays conservative: the
// neutral/none team is 0 and everything else is the hostile value.
func currentDungeonRidableFaction(team string) uint32 {
	switch team {
	case "", "neutral", "none":
		return 0
	default:
		return 1
	}
}

func currentDungeonRandomizedObjectSelectionIndices(candidateCount, selectCount int, seed uint32, groupIndex int) []int {
	indices := make([]int, candidateCount)
	for index := range indices {
		indices[index] = index
	}
	state := seed ^ uint32(groupIndex+1)*0x9e3779b9
	if state == 0 {
		state = 0x6d2b79f5
	}
	for index := candidateCount - 1; index > 0; index-- {
		state ^= state << 13
		state ^= state >> 17
		state ^= state << 5
		swapIndex := int(state % uint32(index+1))
		indices[index], indices[swapIndex] = indices[swapIndex], indices[index]
	}
	return indices[:selectCount]
}

func currentDungeonRandomizedObjectInfoGroups(
	groups [][]currentDungeonRandomizedObjectSelection,
) [][]currentDungeonInfoPair {
	if len(groups) == 0 {
		return nil
	}
	result := make([][]currentDungeonInfoPair, len(groups))
	for groupIndex, group := range groups {
		result[groupIndex] = make([]currentDungeonInfoPair, 0, len(group))
		for _, selection := range group {
			result[groupIndex] = append(result[groupIndex], currentDungeonInfoPair{
				First: byte(selection.Room.X), Second: byte(selection.Room.Y),
			})
		}
	}
	return result
}

// buildCurrentDungeonRandomizedCreatureQueue builds the current class0/op479
// room-explicit creature queue.  Association rows alone do not instantiate an
// actor; op28 clears this queue, so callers must send it after op28 and before
// the matching full op29 room payload.
//
// This queue carries NEUTRAL creatures only, and the team filter is what keeps it
// that way: dropping it (tried live 2026-09-16T23:3xZ) spawned a *friendly* 托梅尔
// next to the hostile one the association row already materializes, because the
// op479 descriptor has no faction field at all -- the client treats everything in
// the queue as team-neutral. Hostile randomized monsters must stay on the
// association row (fifth value 0), which is where 218's group-1 58530 comes from.
func buildCurrentDungeonRandomizedCreatureQueue(
	room worldmap.RoomCoordinate,
	groups [][]currentDungeonRandomizedObjectSelection,
) ([]byte, int, error) {
	selected := make([]currentDungeonRandomizedObjectSelection, 0)
	for _, group := range groups {
		for _, selection := range group {
			if selection.Room != room || selection.Family != currentDungeonRandomizedMonsterFamily ||
				!strings.EqualFold(selection.Team, "neutral") {
				continue
			}
			if selection.ObjectKey == 0 || selection.ObjectKey > uint32(^uint16(0)) {
				return nil, 0, fmt.Errorf("%w: object_key=%d", errDungeonRandomizedObjectKeyRange, selection.ObjectKey)
			}
			if selection.DefinitionID == 0 || selection.Level == 0 {
				return nil, 0, fmt.Errorf("%w: definition=%d level=%d", errDungeonRandomizedObjectDefinition, selection.DefinitionID, selection.Level)
			}
			selected = append(selected, selection)
		}
	}
	if len(selected) == 0 {
		return nil, 0, nil
	}
	if len(selected) > int(^uint8(0)) {
		return nil, 0, fmt.Errorf("%w: creature_count=%d", errDungeonRandomizedObjectGroup, len(selected))
	}
	var writer packetWriter
	writer.writeByte(byte(len(selected)))
	for _, selection := range selected {
		writer.writeUint16(uint16(selection.ObjectKey))
		writer.writeUint32(selection.DefinitionID)
		writer.writeUint16(uint16(selection.Level))
		writer.writeUint32(uint32(int32(selection.Position.X)))
		writer.writeUint32(uint32(int32(selection.Position.Y)))
		writer.writeUint32(0)
		writer.writeByte(0)
		writer.writeByte(currentDungeonExplicitPositionMode)
	}
	return writer.bytes(), len(selected), nil
}
