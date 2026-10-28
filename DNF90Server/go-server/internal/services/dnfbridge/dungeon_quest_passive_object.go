package dnfbridge

import (
	"context"
	"fmt"
	"strconv"

	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfquest "longheng.io/server/internal/modules/dnf/quest"
	"longheng.io/server/internal/modules/dnf/worldmap"
)

const currentDungeonQuestEnemyTypePassiveObject = int64(3)

// currentDungeonQuestPassiveObjectTarget attempts a guarded binding for an
// unannounced client-side quest-object death. The current EXE packet proves
// only a variable op39 with owner=0xffff and a runtime object key; it does not
// carry or prove the PVF enemy code. The PVF code below is therefore selected
// only as a constrained inference from durable active quest state.
//
// The binding is deliberately narrow: exactly one active type-3 hunt target,
// a no-reward sub quest with an active main quest, and an object key directly
// preceding the first announced room actor. Ambiguous or ordinary unknown
// op39 requests remain rejected.
func (s *Service) currentDungeonQuestPassiveObjectTarget(
	ctx context.Context,
	session *gameSession,
	runtime *runtimeDungeonState,
	objectKey uint32,
) (dnfquest.ActiveHuntEnemyTarget, bool, error) {
	if session == nil || runtime == nil || runtime.Room == nil || session.dungeon.runtime != runtime ||
		!dungeonRuntimeOwnsCharacter(runtime, session.selectedCharacterID) ||
		!currentDungeonQuestObjectImmediatelyPrecedesAnnouncedActor(runtime.Room, objectKey) {
		return dnfquest.ActiveHuntEnemyTarget{}, false, nil
	}
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.Quest == nil {
		return dnfquest.ActiveHuntEnemyTarget{}, false, errDungeonHuntEnemyCompletionUnavailable
	}
	characterID := strconv.FormatUint(uint64(session.selectedCharacterID), 10)
	record, found, err := repositories.Quest.Load(ctx, characterID)
	if err != nil {
		return dnfquest.ActiveHuntEnemyTarget{}, false, err
	}
	if !found {
		return dnfquest.ActiveHuntEnemyTarget{}, false, nil
	}
	catalog, err := s.loadQuestCatalog(ctx)
	if err != nil {
		return dnfquest.ActiveHuntEnemyTarget{}, false, err
	}
	targets := catalog.ActiveHuntEnemyTargets(
		record,
		runtime.Dungeon.ID,
		int64(runtime.Request.Difficulty),
		currentDungeonQuestEnemyTypePassiveObject,
	)
	if len(targets) != 1 {
		return dnfquest.ActiveHuntEnemyTarget{}, false, nil
	}
	target := targets[0]
	definition, known := catalog.Find(target.QuestID)
	if !known || normalizeDungeonPVFSymbol(definition.Grade) != "sub" || definition.MainQuestID <= 0 ||
		definition.HasGoldReward || len(definition.RewardItems) != 0 || len(definition.RewardSelectionItems) != 0 {
		return dnfquest.ActiveHuntEnemyTarget{}, false, nil
	}
	parent, parentKnown := currentDungeonHuntEnemyQuestState(record, definition.MainQuestID)
	if !parentKnown || parent.Status != "active" || parent.ProgressValue <= 0 {
		return dnfquest.ActiveHuntEnemyTarget{}, false, nil
	}
	return target, true, nil
}

func currentDungeonQuestObjectImmediatelyPrecedesAnnouncedActor(room *runtimeDungeonRoom, objectKey uint32) bool {
	if room == nil || objectKey == 0 || objectKey >= uint32(^uint16(0)) {
		return false
	}
	snapshot := room.Snapshot()
	first := uint32(0)
	consider := func(key uint32, state runtimeDungeonMonsterState) {
		if state != runtimeDungeonMonsterAnnounced || key == 0 {
			return
		}
		if first == 0 || key < first {
			first = key
		}
	}
	for _, monster := range snapshot.Monsters {
		consider(monster.ObjectKey, monster.State)
	}
	for _, actor := range snapshot.ExtendedActors {
		consider(actor.ObjectKey, actor.State)
	}
	return first != 0 && objectKey+1 == first
}

func currentDungeonScenePassiveObjectCount(scene worldmap.DungeonRoomScene, objectID int64) uint32 {
	var count uint32
	for _, object := range scene.PassiveObjects {
		if object.ObjectID == objectID {
			count++
		}
	}
	for _, object := range scene.SpecialPassiveObjects {
		if object.ObjectID == objectID {
			count++
		}
	}
	return count
}

// handleCurrentDungeonQuestPassiveObjectRewardDeathLocked handles the proven
// current-client variable/count-zero/owner-ffff op39 emitted by ordinary PVF
// passive objects. The object code is matched against the current scene and
// the PVF enemy_type=3 reward table before any inventory mutation.
func (s *Service) handleCurrentDungeonQuestPassiveObjectRewardDeathLocked(
	session *gameSession,
	runtime *runtimeDungeonState,
	scene worldmap.DungeonRoomScene,
	objectKey uint32,
) (bool, error) {
	if s == nil || session == nil || runtime == nil || runtime.Room == nil || runtime.Session == nil ||
		session.dungeon.runtime != runtime || !dungeonRuntimeOwnsCharacter(runtime, session.selectedCharacterID) || objectKey == 0 {
		return false, nil
	}
	if currentDungeonRuntimeIsPractice(runtime) {
		return false, nil
	}
	enemyCode := int64(objectKey)
	available := currentDungeonScenePassiveObjectCount(scene, enemyCode)
	if available == 0 {
		return false, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.Quest == nil {
		return false, errDungeonHuntEnemyCompletionUnavailable
	}
	record, found, err := repositories.Quest.Load(ctx, strconv.FormatUint(uint64(session.selectedCharacterID), 10))
	if err != nil || !found {
		return false, err
	}
	catalog, err := s.loadQuestCatalog(ctx)
	if err != nil {
		return false, err
	}
	candidates := currentQuestDropMatchEnemy(
		catalog,
		currentQuestDropActiveIDs(record),
		int64(runtime.Dungeon.ID),
		int64(runtime.Request.Difficulty),
		enemyCode,
		currentDungeonQuestEnemyTypePassiveObject,
	)
	if len(candidates) == 0 {
		return false, nil
	}
	visit, err := runtime.currentDungeonRoomVisit(scene)
	if err != nil {
		return false, err
	}
	retired := visit.QuestPassiveObjectDeaths[enemyCode]
	deathACKSent := false
	responseOpcode := uint16(0)
	responseReason := "current_exe_op38_target_is_u16_and_client_owned_passive_object_is_already_destroyed"
	if objectKey <= uint32(^uint16(0)) {
		responseBody, buildErr := buildCurrentDungeonDeathNotificationBody(
			objectKey,
			currentDungeonDeathResponseAICharacter,
		)
		if buildErr != nil {
			return false, buildErr
		}
		if sendErr := s.sendGameUpperRawClass(session, uint16(dnfenum.CmdPacketNotifyDieMonster), responseBody, 0); sendErr != nil {
			return false, sendErr
		}
		deathACKSent = true
		responseOpcode = uint16(dnfenum.CmdPacketNotifyDieMonster)
		responseReason = "current_exe_op38_u16_target"
	}
	if retired >= available {
		s.logGameEvent(session, "game-dungeon-quest-passive-object-replayed",
			"dungeon_id", runtime.Dungeon.ID,
			"maze_index", runtime.MazeIndex,
			"map_id", scene.Map.Map.ID,
			"enemy_code", enemyCode,
			"retired", retired,
			"available", available)
		return true, nil
	}
	if visit.QuestPassiveObjectDeaths == nil {
		visit.QuestPassiveObjectDeaths = make(map[int64]uint32)
	}
	visit.QuestPassiveObjectDeaths[enemyCode] = retired + 1
	s.checkCurrentDungeonQuestEnemyDrops(session, runtime, enemyCode, currentDungeonQuestEnemyTypePassiveObject, false)
	s.logGameEvent(session, "game-dungeon-quest-passive-object-reward-accepted",
		"dungeon_id", runtime.Dungeon.ID,
		"maze_index", runtime.MazeIndex,
		"map_id", scene.Map.Map.ID,
		"enemy_code", enemyCode,
		"enemy_type", currentDungeonQuestEnemyTypePassiveObject,
		"retired", retired+1,
		"available", available,
		"candidate_count", len(candidates),
		"death_ack_sent", deathACKSent,
		"response_opcode", responseOpcode,
		"response_reason", responseReason)
	return true, nil
}

func (s *Service) handleCurrentDungeonQuestPassiveObjectDeathLocked(
	session *gameSession,
	runtime *runtimeDungeonState,
	scene worldmap.DungeonRoomScene,
	objectKey uint32,
) (bool, error) {
	if currentDungeonRuntimeIsPractice(runtime) {
		return false, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	target, matched, err := s.currentDungeonQuestPassiveObjectTarget(ctx, session, runtime, objectKey)
	if err != nil || !matched {
		return false, err
	}
	responseBody, err := buildCurrentDungeonDeathNotificationBody(
		objectKey,
		currentDungeonDeathResponseAICharacter,
	)
	if err != nil {
		return false, err
	}
	// Match the established monster owner order: acknowledge the authoritative
	// current-EXE op39 first, then persist/send the active-quest snapshot.
	if err := s.sendGameUpperRawClass(session, uint16(dnfenum.CmdPacketNotifyDieMonster), responseBody, 0); err != nil {
		return false, err
	}
	if err := s.persistCurrentDungeonHuntEnemyTargetPhaseA(
		ctx,
		session,
		runtime,
		scene,
		target.EnemyCode,
		target.EnemyType,
		objectKey,
		"guarded_inference_current_exe_op39_unique_active_pvf_passive_target",
	); err != nil {
		return true, fmt.Errorf("persist passive-object hunt target: %w", err)
	}
	s.logGameEvent(session, "game-dungeon-quest-passive-object-destroyed",
		"char_id", session.selectedCharacterID,
		"dungeon_id", runtime.Dungeon.ID,
		"maze_index", runtime.MazeIndex,
		"map_id", scene.Map.Map.ID,
		"object_key", objectKey,
		"quest_id", target.QuestID,
		"enemy_code", target.EnemyCode,
		"enemy_type", target.EnemyType,
		"response_opcode", uint16(dnfenum.CmdPacketNotifyDieMonster),
		"response_body_len", len(responseBody),
		"source", "guarded_inference_current_exe_variable_op39_owner_ffff_unique_active_pvf_passive_target")
	return true, nil
}
