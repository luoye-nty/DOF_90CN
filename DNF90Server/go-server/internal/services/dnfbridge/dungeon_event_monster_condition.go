package dnfbridge

import (
	"fmt"

	"longheng.io/server/internal/modules/dnf/worldmap"
)

// currentDungeonTrapEventMonsterScope identifies the PVF-owned terrain-event
// family used by Second Spine. The DGN owns at least one event-monster row and
// a random-map selector, while the selected MAP owns event positions and a
// special passive object with retained [trap] children. The trap children are
// script metadata, not op29 actors; class0/op312 activates the client-side
// event controller only after op29 has created the room scene.
func currentDungeonTrapEventMonsterScope(
	runtime *runtimeDungeonState,
	scene worldmap.DungeonRoomScene,
	room *runtimeDungeonRoom,
) bool {
	if runtime == nil || room == nil || runtime.MazeIndex < 0 ||
		runtime.MazeIndex >= len(runtime.Dungeon.Mazes) ||
		len(runtime.Dungeon.Metadata.EventMonsters) == 0 ||
		!runtime.Dungeon.Metadata.EventMonsterRandomMap.Set ||
		len(scene.Map.Map.EventMonsterPositions) == 0 {
		return false
	}
	// Boss-room entrance conditions own the same op312 number but publish it
	// only after their hunt counters complete. Never let room entry bypass that
	// separate state machine.
	if runtime.Dungeon.Mazes[runtime.MazeIndex].BossRoomEntranceCondition != nil {
		return false
	}
	snapshot := room.Snapshot()
	if snapshot.Coordinate != scene.Coordinate || snapshot.MapID != scene.Map.Map.ID {
		return false
	}
	for _, object := range scene.SpecialPassiveObjects {
		for _, spawn := range object.Spawns {
			if normalizeDungeonPVFSymbol(spawn.Kind) == "trap" && spawn.Code > 0 {
				return true
			}
		}
	}
	return false
}

func (s *Service) publishCurrentDungeonTrapEventMonsterAfterStartMap(
	session *gameSession,
	runtime *runtimeDungeonState,
	scene worldmap.DungeonRoomScene,
	room *runtimeDungeonRoom,
	source string,
) error {
	if s == nil || session == nil || !currentDungeonTrapEventMonsterScope(runtime, scene, room) ||
		room.eventMonsterConditionWasPublished() {
		return nil
	}
	body := buildCurrentDungeonCompleteConditionPassGateBody()
	if len(body) != len(currentDungeonCompleteConditionPassGateBody) {
		return fmt.Errorf("current event-monster condition body length %d, want %d", len(body), len(currentDungeonCompleteConditionPassGateBody))
	}
	if err := s.sendGameUpperRawClass(
		session,
		currentDungeonCompleteConditionPassGateMsgID,
		body,
		0,
	); err != nil {
		return err
	}
	room.markEventMonsterConditionPublished()
	s.logGameEvent(session, "game-dungeon-event-monster-condition-published",
		"source", source,
		"dungeon_id", runtime.Dungeon.ID,
		"maze_index", runtime.MazeIndex,
		"room", scene.Coordinate.String(),
		"map_id", scene.Map.Map.ID,
		"event_monster_definitions", len(runtime.Dungeon.Metadata.EventMonsters),
		"event_monster_random_map", runtime.Dungeon.Metadata.EventMonsterRandomMap.Value,
		"event_position_count", len(scene.Map.Map.EventMonsterPositions),
		"response_opcode", currentDungeonCompleteConditionPassGateMsgID,
		"response_body_len", len(body),
		"body_source", "current_exe_complete_condition_pass_gate_after_op29_trap_event_scene")
	return nil
}
