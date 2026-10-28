package dnfbridge

import (
	"longheng.io/server/internal/modules/dnf/dnfenum"
	"longheng.io/server/internal/modules/dnf/dungeoncmd"
	"longheng.io/server/internal/modules/dnf/worldmap"
)

const (
	currentFruzStationDungeonID     = int64(102)
	currentFruzStationNarkeQuestID  = int64(3733)
	currentFruzStationNarkeActorID  = int64(11526)
	currentFruzStationObstacleSmall = int64(16109)
	currentFruzStationObstacleLarge = int64(16110)
)

func currentFruzStationNarkeObstacleMap(mapID int64) bool {
	switch mapID {
	case 76503, 76504, 76506:
		return true
	default:
		return false
	}
}

func currentFruzStationNarkeActorInRoom(room *runtimeDungeonRoom, objectKey uint32) bool {
	if room == nil || objectKey == 0 {
		return false
	}
	for _, actor := range room.Snapshot().ExtendedActors {
		if actor.ObjectKey != objectKey || actor.Kind != runtimeDungeonActorAICharacter ||
			actor.State != runtimeDungeonMonsterAnnounced || actor.HostileReference != nil ||
			actor.Packet.Code != uint32(currentFruzStationNarkeActorID) || actor.AICharacter == nil {
			continue
		}
		return actor.AICharacter.Code == currentFruzStationNarkeActorID &&
			normalizeDungeonPVFSymbol(actor.AICharacter.Faction) == "character" &&
			normalizeDungeonPVFSymbol(actor.AICharacter.AIType) == "normal"
	}
	return false
}

// Narke follows the player from the first room while keeping the actor key
// announced there. Check both the live room and cached visits, but accept only
// the exact friendly APC definition owned by quest 3733.
func currentFruzStationRuntimeContainsNarke(runtime *runtimeDungeonState, objectKey uint32) bool {
	if runtime == nil || objectKey == 0 {
		return false
	}
	if currentFruzStationNarkeActorInRoom(runtime.Room, objectKey) {
		return true
	}
	for _, visit := range runtime.Rooms {
		if visit == nil || visit.Room == nil || visit.Room == runtime.Room {
			continue
		}
		if currentFruzStationNarkeActorInRoom(visit.Room, objectKey) {
			return true
		}
	}
	return false
}

// handleCurrentFruzStationNarkeObstacleDeathLocked owns only the native
// variable/count-zero op39 emitted after Narke's grenade hits quest rubble.
// The obstacles are client-side passive objects and are not ordinary runtime
// actors, so they need an op38 receipt without monster, reward, quest or room
// clear mutation. Missing this receipt leaves Narke waiting forever after the
// room's monsters are dead.
func (s *Service) handleCurrentFruzStationNarkeObstacleDeathLocked(
	session *gameSession,
	runtime *runtimeDungeonState,
	scene worldmap.DungeonRoomScene,
	request dungeoncmd.DieMonsterRequest,
) (bool, error) {
	if s == nil || session == nil || runtime == nil || runtime.Session == nil || runtime.Room == nil ||
		session.dungeon.runtime != runtime || !dungeonRuntimeOwnsCharacter(runtime, session.selectedCharacterID) ||
		runtime.Dungeon.ID != currentFruzStationDungeonID ||
		currentDungeonStoryQuestID(runtime) != currentFruzStationNarkeQuestID ||
		!currentFruzStationNarkeObstacleMap(scene.Map.Map.ID) ||
		request.Layout != dungeoncmd.DieMonsterRequestLayoutVariableCombat ||
		request.CombatEntryCount != 0 {
		return false, nil
	}

	objectID := int64(request.RuntimeObjectKey)
	if objectID != currentFruzStationObstacleSmall && objectID != currentFruzStationObstacleLarge {
		return false, nil
	}
	available := currentDungeonScenePassiveObjectCount(scene, objectID)
	if available == 0 {
		return false, nil
	}
	ownerIsNarke := request.OwnerObjectKey != ^uint16(0) &&
		currentFruzStationRuntimeContainsNarke(runtime, uint32(request.OwnerObjectKey))
	if request.OwnerObjectKey != ^uint16(0) && !ownerIsNarke {
		return false, nil
	}

	visit, err := runtime.currentDungeonRoomVisit(scene)
	if err != nil {
		return false, err
	}
	responseBody, err := buildCurrentDungeonDeathNotificationBody(
		request.RuntimeObjectKey,
		currentDungeonDeathResponseAICharacter,
	)
	if err != nil {
		return false, err
	}
	if err := s.sendGameUpperRawClass(
		session,
		uint16(dnfenum.CmdPacketNotifyDieMonster),
		responseBody,
		0,
	); err != nil {
		return false, err
	}

	retired := visit.QuestPassiveObjectDeaths[objectID]
	if retired < available {
		if visit.QuestPassiveObjectDeaths == nil {
			visit.QuestPassiveObjectDeaths = make(map[int64]uint32)
		}
		retired++
		visit.QuestPassiveObjectDeaths[objectID] = retired
	}
	s.logGameEvent(session, "game-dungeon-fruz-narke-obstacle-death-accepted",
		"dungeon_id", runtime.Dungeon.ID,
		"quest_id", currentFruzStationNarkeQuestID,
		"maze_index", runtime.MazeIndex,
		"map_id", scene.Map.Map.ID,
		"object_key", request.RuntimeObjectKey,
		"related_actor_object_key", request.OwnerObjectKey,
		"owner_is_narke", ownerIsNarke,
		"retired", retired,
		"available", available,
		"response_opcode", uint16(dnfenum.CmdPacketNotifyDieMonster))
	return true, nil
}
