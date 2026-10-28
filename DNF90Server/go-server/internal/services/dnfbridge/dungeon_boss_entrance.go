package dnfbridge

import (
	"strings"

	"longheng.io/server/internal/modules/dnf/worldmap"
)

// The current 90CN client registers class-0/op0x0138 independently from the
// class-1 command enum that happens to use the same numeric value.  Its exact
// five-byte body is u32(0),u8(0); the client consumes it as
// COMPLETE_CONDITION_PASS_GATE and sets its current-dungeon gate flag.
const currentDungeonCompleteConditionPassGateMsgID uint16 = 0x0138

var currentDungeonCompleteConditionPassGateBody = [...]byte{0, 0, 0, 0, 0}

type currentDungeonBossEntranceHuntProgress struct {
	TargetID int64
	Required int64
	Defeated int64
}

type currentDungeonBossEntranceProgress struct {
	Action  string
	Targets []currentDungeonBossEntranceHuntProgress
	Ready   bool
}

// currentDungeonBossEntranceConditionProgress evaluates one maze-owned PVF
// gate against every visited room in the current run.  Room actors are the
// authority: an op39 replay cannot increment a counter twice, while revisiting
// a cached room preserves the already-defeated state.
func currentDungeonBossEntranceConditionProgress(
	runtime *runtimeDungeonState,
) (currentDungeonBossEntranceProgress, bool) {
	if runtime == nil || runtime.MazeIndex < 0 || runtime.MazeIndex >= len(runtime.Dungeon.Mazes) {
		return currentDungeonBossEntranceProgress{}, false
	}
	condition := runtime.Dungeon.Mazes[runtime.MazeIndex].BossRoomEntranceCondition
	if condition == nil || strings.TrimSpace(strings.ToLower(condition.Action)) != "clear action" ||
		len(condition.HuntMonsters) == 0 {
		return currentDungeonBossEntranceProgress{}, false
	}

	progress := currentDungeonBossEntranceProgress{
		Action:  "clear action",
		Targets: make([]currentDungeonBossEntranceHuntProgress, 0, len(condition.HuntMonsters)),
		Ready:   true,
	}
	requirements := make(map[int64]int64, len(condition.HuntMonsters))
	for _, target := range condition.HuntMonsters {
		if target.TargetID <= 0 || target.Count <= 0 {
			return currentDungeonBossEntranceProgress{}, false
		}
		requirements[target.TargetID] += target.Count
	}

	defeated := make(map[int64]int64, len(requirements))
	seenRooms := make(map[*runtimeDungeonRoom]struct{}, len(runtime.Rooms)+1)
	countRoom := func(room *runtimeDungeonRoom) {
		if room == nil {
			return
		}
		if _, duplicate := seenRooms[room]; duplicate {
			return
		}
		seenRooms[room] = struct{}{}
		for _, monster := range room.Snapshot().Monsters {
			if monster.State != runtimeDungeonMonsterDefeated {
				continue
			}
			if _, tracked := requirements[monster.Spawn.MonsterID]; tracked {
				defeated[monster.Spawn.MonsterID]++
			}
		}
	}
	// Include the active room explicitly.  It normally shares the pointer held
	// in Rooms, but doing so keeps the fourth-tower transition correct during
	// the small commit-to-cache interval after op39.
	countRoom(runtime.Room)
	for _, visit := range runtime.Rooms {
		if visit == nil {
			continue
		}
		countRoom(visit.Room)
	}

	for _, target := range condition.HuntMonsters {
		required := requirements[target.TargetID]
		// Duplicate target rows are folded into one requirement and one
		// projection.  The parser rejects them, but this keeps compact/runtime
		// callers fail-safe if a future source supplies one.
		if required == 0 {
			continue
		}
		progress.Targets = append(progress.Targets, currentDungeonBossEntranceHuntProgress{
			TargetID: target.TargetID,
			Required: required,
			Defeated: defeated[target.TargetID],
		})
		delete(requirements, target.TargetID)
		if defeated[target.TargetID] < required {
			progress.Ready = false
		}
	}
	return progress, true
}

func buildCurrentDungeonCompleteConditionPassGateBody() []byte {
	return append([]byte(nil), currentDungeonCompleteConditionPassGateBody[:]...)
}

// publishCurrentDungeonBossEntranceGateAfterDeathLocked is called only after
// the native op38 death notification has been written.  The room snapshots are
// the idempotent authority, so replayed op39 traffic cannot advance the gate.
// Completion is retained even if the socket write fails, while the sent flag
// advances only after op312 reaches the connection.
func (s *Service) publishCurrentDungeonBossEntranceGateAfterDeathLocked(
	session *gameSession,
	runtime *runtimeDungeonState,
) error {
	progress, found := currentDungeonBossEntranceConditionProgress(runtime)
	if !found || !progress.Ready {
		return nil
	}
	runtime.bossEntranceConditionComplete = true
	if runtime.bossEntranceGateNotificationSent {
		return nil
	}
	if err := s.sendGameUpperRawClass(
		session,
		currentDungeonCompleteConditionPassGateMsgID,
		buildCurrentDungeonCompleteConditionPassGateBody(),
		0,
	); err != nil {
		return err
	}
	runtime.bossEntranceGateNotificationSent = true
	coordinate := worldmap.RoomCoordinate{}
	mapID := int64(0)
	if runtime != nil && runtime.Session != nil {
		if scene, ok := runtime.Session.Scene(); ok {
			coordinate = scene.Coordinate
			mapID = scene.Map.Map.ID
		}
	}
	s.logGameEvent(session, "game-dungeon-boss-entrance-condition-complete",
		"dungeon_id", runtime.Dungeon.ID,
		"maze_index", runtime.MazeIndex,
		"room", coordinate.String(),
		"map_id", mapID,
		"targets", progress.Targets,
		"response_opcode", currentDungeonCompleteConditionPassGateMsgID,
		"response_body_len", len(currentDungeonCompleteConditionPassGateBody),
		"source", "current_exe_complete_condition_pass_gate_after_fourth_target_op38")
	return nil
}
