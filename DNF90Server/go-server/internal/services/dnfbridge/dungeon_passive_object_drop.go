package dnfbridge

import (
	"fmt"
	"math"

	"longheng.io/server/internal/modules/dnf/worldmap"
)

type currentDungeonPassiveObjectDropPlan struct {
	ExtraEntries       []currentDungeonStartMapExtraEntry
	Drops              []*runtimeDungeonDrop
	StartObjectKey     uint32
	NextObjectKey      uint32
	StartSceneSlot     uint16
	NextSceneSlot      uint16
	StartDropRNG       uint32
	NextDropRNG        uint32
	ConfiguredRowCount int
}

// planCurrentDungeonPassiveObjectDrops performs the maze-level item roll
// without mutating runtime state. The caller commits the plan only after the
// corresponding op29 write succeeds.
func (s *Service) planCurrentDungeonPassiveObjectDrops(
	session *gameSession,
	runtime *runtimeDungeonState,
	scene worldmap.DungeonRoomScene,
	startObjectKey uint32,
	startDropRNG uint32,
) (currentDungeonPassiveObjectDropPlan, error) {
	if session == nil || runtime == nil || runtime.Dungeon.ID <= 0 || runtime.MazeIndex < 0 || runtime.MazeIndex >= len(runtime.Dungeon.Mazes) {
		return currentDungeonPassiveObjectDropPlan{}, errDungeonWorldMapUnavailable
	}
	if runtime.DropOwner != nil {
		startObjectKey = runtime.DropOwner.nextObjectKeyAtOrAbove(startObjectKey)
	}
	if startObjectKey == 0 || startObjectKey > math.MaxUint16 {
		return currentDungeonPassiveObjectDropPlan{}, fmt.Errorf("%w: next=%d", errDungeonDropObjectKeyRange, startObjectKey)
	}
	ownerActorObjectKey := currentSceneActorObjectKey(session.selectedCharacterID)
	if ownerActorObjectKey == 0 {
		return currentDungeonPassiveObjectDropPlan{}, errDungeonDropActorMismatch
	}
	nextSceneSlot := uint16(1)
	if runtime.DropOwner != nil {
		nextSceneSlot = runtime.DropOwner.nextSceneSlot
	}
	if nextSceneSlot == 0 {
		return currentDungeonPassiveObjectDropPlan{}, fmt.Errorf("%w: next=%d", errDungeonDropSceneSlotRange, nextSceneSlot)
	}

	rows := runtime.Dungeon.Metadata.SpecialPassiveObjectItems
	if len(rows) == 0 {
		// Retain the old maze-level field for synthetic fixtures and historical
		// serialized tables. Current real PVF parsing stores this definition on
		// the dungeon, matching the client/reference ownership.
		rows = runtime.Dungeon.Mazes[runtime.MazeIndex].SpecialPassiveObjectItems
	}
	plan := currentDungeonPassiveObjectDropPlan{
		StartObjectKey:     startObjectKey,
		NextObjectKey:      startObjectKey,
		StartSceneSlot:     nextSceneSlot,
		NextSceneSlot:      nextSceneSlot,
		StartDropRNG:       startDropRNG,
		NextDropRNG:        startDropRNG,
		ConfiguredRowCount: len(rows),
	}
	if currentDungeonRuntimeIsPractice(runtime) {
		return plan, nil
	}
	if len(rows) == 0 {
		return plan, nil
	}
	monsterCatalog, err := s.dungeonMonsterCatalog()
	if err != nil {
		return currentDungeonPassiveObjectDropPlan{}, err
	}
	items, err := monsterCatalog.DropCatalog()
	if err != nil {
		return currentDungeonPassiveObjectDropPlan{}, err
	}
	groups := make(map[int64][]worldmap.SpecialPassiveObjectItem, len(rows))
	for _, row := range rows {
		if row.Index < 0 || row.Index > math.MaxUint8 || row.ItemID <= 0 || row.ItemID > math.MaxUint32 ||
			row.DropRate < 0 || row.DropRate > currentDungeonDropDenominator {
			return currentDungeonPassiveObjectDropPlan{}, fmt.Errorf(
				"invalid special passive object item row: index=%d item=%d rate=%d",
				row.Index, row.ItemID, row.DropRate,
			)
		}
		groups[row.Index] = append(groups[row.Index], row)
	}
	rng := newCurrentDungeonDropLCG(startDropRNG)
	roomKey := runtimeDungeonRoomKeyFromScene(scene)
	skippedRandomAttempts := 0
	// rollGroup performs one attempt against one reward group: a single
	// 0..9999 roll compared with the group's cumulative drop rates yields at
	// most one item (86JP PassiveObjectDropGenerator.SelectSpecificItem). The
	// roll is consumed even when the group is empty, matching the reference.
	rollGroup := func(groupIndex int64, passiveObjectIndex byte, attempt, attempts int, source string) error {
		group := groups[groupIndex]
		roll := rng.Next(currentDungeonDropDenominator)
		selectedItemID := currentDungeonPassiveObjectGroupSelect(group, roll)
		s.logGameEvent(session, "game-dungeon-passive-object-item-roll",
			"dungeon_id", runtime.Dungeon.ID,
			"maze_index", runtime.MazeIndex,
			"room", scene.Coordinate.String(),
			"map_id", scene.Map.Map.ID,
			"group_index", groupIndex,
			"group_item_count", len(group),
			"passive_object_index", passiveObjectIndex,
			"attempt", attempt,
			"attempts", attempts,
			"denominator", currentDungeonDropDenominator,
			"roll", roll,
			"selected_item_id", selectedItemID,
			"source", source)
		if selectedItemID <= 0 {
			return nil
		}
		if len(plan.ExtraEntries) >= currentDungeonPassiveObjectMaxEntries {
			return nil
		}
		definition, resolveErr := items.ResolveItem(uint32(selectedItemID))
		if resolveErr != nil {
			// 86JP counts an unpreparable intent and keeps the room alive; one
			// bad PVF row must not block the whole dungeon entry.
			s.logGameEvent(session, "game-dungeon-passive-object-item-unresolved",
				"dungeon_id", runtime.Dungeon.ID,
				"maze_index", runtime.MazeIndex,
				"room", scene.Coordinate.String(),
				"map_id", scene.Map.Map.ID,
				"group_index", groupIndex,
				"item_id", selectedItemID,
				"error", resolveErr)
			return nil
		}
		if plan.NextObjectKey >= math.MaxUint16 || plan.NextSceneSlot == math.MaxUint16 {
			return fmt.Errorf(
				"passive-object drop key range exhausted: object_key=%d scene_slot=%d",
				plan.NextObjectKey, plan.NextSceneSlot,
			)
		}
		amountOrQuality := uint32(1)
		qualitySeed := uint32(0)
		if definition.Kind == dungeonDropItemEquipment {
			qualitySeed = rng.NextUint32()%currentEquipmentRandomQualitySeedCount + 1
			amountOrQuality = qualitySeed
		}
		plan.ExtraEntries = append(plan.ExtraEntries, currentDungeonStartMapExtraEntry{
			PassiveObjectIndex: passiveObjectIndex,
			GlobalSequence:     plan.NextObjectKey,
			ItemID:             definition.ItemID,
			StackCount:         amountOrQuality,
			Endurance:          definition.Durability,
		})
		plan.Drops = append(plan.Drops, &runtimeDungeonDrop{
			ObjectKey:           plan.NextObjectKey,
			SceneSlot:           plan.NextSceneSlot,
			Item:                definition,
			Amount:              1,
			QualitySeed:         qualitySeed,
			OwnerActorObjectKey: ownerActorObjectKey,
			Room:                roomKey,
			Status:              runtimeDungeonDropAvailable,
		})
		plan.NextObjectKey++
		plan.NextSceneSlot++
		return nil
	}

	// Map [special passive object] children of kind [item]: `[item] <group>
	// <specific attempts> <random attempts> ...`. The group is rolled once per
	// specific attempt; a level of -1 means no specific roll at all. The
	// random-attempt branch (itemdropinfo_object.etc generic gold/equipment)
	// is not implemented and is only counted for the log line.
	for objectIndex, object := range scene.SpecialPassiveObjects {
		for _, spawn := range object.Spawns {
			if normalizeDungeonPVFSymbol(spawn.Kind) != "item" {
				continue
			}
			if objectIndex > math.MaxUint8 {
				return currentDungeonPassiveObjectDropPlan{}, fmt.Errorf("%w: special_object_index=%d", errDungeonExtendedActorIndexRange, objectIndex)
			}
			if spawn.Code < 0 || spawn.Code > math.MaxUint8 {
				continue
			}
			if randomAttempts := currentDungeonPassiveObjectSignedByte(spawn.Params[0]); randomAttempts > 0 {
				skippedRandomAttempts += randomAttempts
			}
			attempts := currentDungeonPassiveObjectSignedByte(spawn.Level)
			for attempt := 0; attempt < attempts; attempt++ {
				if err := rollGroup(spawn.Code, byte(objectIndex), attempt, attempts, "map_special_passive_object_item_spawn"); err != nil {
					return currentDungeonPassiveObjectDropPlan{}, err
				}
			}
		}
	}
	// Ordinary map passive objects whose definition declares a quest-item
	// reward group keep the legacy group selector used by the current client
	// (they are not projected as op29 type-9 actors) and roll once per
	// instance.
	for _, object := range scene.PassiveObjects {
		group, known, groupErr := monsterCatalog.passiveObjectRewardGroup(object.ObjectID)
		if groupErr != nil {
			return currentDungeonPassiveObjectDropPlan{}, groupErr
		}
		if !known {
			continue
		}
		if group < 0 || group > math.MaxUint8 {
			return currentDungeonPassiveObjectDropPlan{}, fmt.Errorf("invalid ordinary passive-object reward group: object_id=%d group=%d", object.ObjectID, group)
		}
		if err := rollGroup(group, byte(group), 0, 1, "map_passive_object_quest_item_reward_group"); err != nil {
			return currentDungeonPassiveObjectDropPlan{}, err
		}
	}
	if skippedRandomAttempts > 0 {
		s.logGameEvent(session, "game-dungeon-passive-object-item-random-attempts-skipped",
			"dungeon_id", runtime.Dungeon.ID,
			"maze_index", runtime.MazeIndex,
			"room", scene.Coordinate.String(),
			"map_id", scene.Map.Map.ID,
			"random_attempts", skippedRandomAttempts,
			"reason", "itemdropinfo_object_generic_branch_not_implemented")
	}
	plan.NextDropRNG = rng.Seed()
	return plan, nil
}

// currentDungeonPassiveObjectMaxEntries is the op29 extra-entry count byte
// ceiling shared with the 86JP reference (MaxStartMapItems).
const currentDungeonPassiveObjectMaxEntries = 0xff

// currentDungeonPassiveObjectSignedByte decodes a map [item] child count the
// way the reference does: the PVF stores -1 for "none", and the native reader
// truncates the value to a signed byte.
func currentDungeonPassiveObjectSignedByte(value int64) int {
	return int(int8(uint8(value)))
}

// currentDungeonPassiveObjectGroupSelect returns the item won by one roll
// against a group's cumulative drop rates, or 0 when the roll lands beyond
// the group's total (or the group is empty).
func currentDungeonPassiveObjectGroupSelect(group []worldmap.SpecialPassiveObjectItem, roll int) int64 {
	cumulative := int64(0)
	for _, row := range group {
		if row.DropRate > 0 {
			cumulative += row.DropRate
		}
		if int64(roll) < cumulative {
			return row.ItemID
		}
	}
	return 0
}

// currentDungeonPassiveObjectGroupOwners resolves every maze reward group to
// the byte that the current client uses to find the owning passive object.
// A special object's [item] code selects a reward group; it is not itself the
// op29 object index. Ordinary map passive objects keep the legacy group
// selector used by the current client because they are not projected as op29
// type-9 actors.
func currentDungeonPassiveObjectGroupOwners(
	scene worldmap.DungeonRoomScene,
	catalog *pvfDungeonMonsterCatalog,
) (map[int64][]byte, error) {
	owners := make(map[int64][]byte)
	for objectIndex, object := range scene.SpecialPassiveObjects {
		groups := make(map[int64]struct{})
		for _, spawn := range object.Spawns {
			if normalizeDungeonPVFSymbol(spawn.Kind) != "item" || spawn.Code < 0 || spawn.Code > math.MaxUint8 {
				continue
			}
			groups[spawn.Code] = struct{}{}
		}
		if len(groups) != 0 && objectIndex > math.MaxUint8 {
			return nil, fmt.Errorf("%w: special_object_index=%d", errDungeonExtendedActorIndexRange, objectIndex)
		}
		for group := range groups {
			owners[group] = append(owners[group], byte(objectIndex))
		}
	}
	if len(scene.PassiveObjects) == 0 {
		return owners, nil
	}
	if catalog == nil {
		return nil, errDungeonMonsterCatalogUnavailable
	}
	for _, object := range scene.PassiveObjects {
		group, known, err := catalog.passiveObjectRewardGroup(object.ObjectID)
		if err != nil {
			return nil, err
		}
		if known {
			if group < 0 || group > math.MaxUint8 {
				return nil, fmt.Errorf("invalid ordinary passive-object reward group: object_id=%d group=%d", object.ObjectID, group)
			}
			owners[group] = append(owners[group], byte(group))
		}
	}
	return owners, nil
}

func commitCurrentDungeonPassiveObjectDrops(
	runtime *runtimeDungeonState,
	scene worldmap.DungeonRoomScene,
	plan currentDungeonPassiveObjectDropPlan,
) error {
	if runtime == nil || runtime.Session == nil || runtime.Room == nil {
		return errDungeonWorldMapUnavailable
	}
	visit, err := runtime.currentDungeonRoomVisit(scene)
	if err != nil {
		return err
	}
	if visit.PassiveObjectDropsPlanned {
		return fmt.Errorf("%w: room=%s map=%d passive_object_drops_already_planned", errDungeonMoveRuntimeOwnerMismatch, scene.Coordinate, scene.Map.Map.ID)
	}
	if runtime.NextObjectKey > plan.StartObjectKey || visit.DropRNG != plan.StartDropRNG {
		return fmt.Errorf(
			"%w: passive_object_plan start_object=%d runtime_object=%d start_rng=%08x room_rng=%08x",
			errDungeonMoveRuntimeOwnerMismatch,
			plan.StartObjectKey, runtime.NextObjectKey, plan.StartDropRNG, visit.DropRNG,
		)
	}
	// Target-room actor planning reserves the object-key range before the op29
	// write, while the live runtime advances only after that write commits.
	// Catch up to the reserved drop range without allowing any backwards move.
	runtime.NextObjectKey = plan.StartObjectKey
	owner := runtime.DropOwner
	if owner == nil {
		owner = newRuntimeDungeonDropOwner()
	}
	if owner.nextSceneSlot != plan.StartSceneSlot {
		return fmt.Errorf(
			"%w: passive_object_plan start_scene_slot=%d runtime_scene_slot=%d",
			errDungeonMoveRuntimeOwnerMismatch, plan.StartSceneSlot, owner.nextSceneSlot,
		)
	}
	if len(plan.Drops) != 0 {
		if err := owner.registerBatch(plan.Drops, plan.NextSceneSlot); err != nil {
			return err
		}
		runtime.DropOwner = owner
	}
	runtime.NextObjectKey = plan.NextObjectKey
	visit.DropRNG = plan.NextDropRNG
	visit.PassiveObjectExtraEntries = append([]currentDungeonStartMapExtraEntry(nil), plan.ExtraEntries...)
	visit.PassiveObjectDropsPlanned = true
	return nil
}
