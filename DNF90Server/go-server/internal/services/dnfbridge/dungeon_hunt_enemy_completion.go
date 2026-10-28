package dnfbridge

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	dnfquest "longheng.io/server/internal/modules/dnf/quest"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	"longheng.io/server/internal/modules/dnf/worldmap"
)

var errDungeonHuntEnemyCompletionUnavailable = errors.New("dnf dungeon hunt-enemy completion owner is unavailable")

type currentDungeonHuntEnemyPersistencePlan struct {
	sequence            uint64
	characterID         uint16
	characterGeneration uint64
	characterKey        string
	dungeonID           int64
	difficulty          int64
	mazeIndex           int
	mapID               int64
	seed                uint32
	enemyCode           int64
	enemyType           int64
	objectKey           uint32
	completedAt         time.Time
	completionKey       string
	source              string
	repositories        dnfrepo.Group
}

type currentDungeonHuntEnemyPersistenceResult struct {
	persisted dnfquest.HuntEnemyPersistResult
	record    dnfrepo.QuestRecord
	body      []byte
	elapsed   time.Duration
	err       error
}

// persistCurrentDungeonHuntEnemyKillPhaseA mirrors 86JP's kill-trigger domain
// path for [hunt enemy]. It only updates the quest trigger/pending marker. Item
// and EXP rewards remain behind the normal FinishQuest atomic owner.
func (s *Service) persistCurrentDungeonHuntEnemyKillPhaseA(
	ctx context.Context,
	session *gameSession,
	runtime *runtimeDungeonState,
	scene worldmap.DungeonRoomScene,
	monster runtimeDungeonMonster,
	objectKey uint32,
) error {
	if session == nil || runtime == nil || runtime.Session == nil || session.dungeon.runtime != runtime ||
		monster.Spawn.MonsterID <= 0 || !dungeonRuntimeOwnsCharacter(runtime, session.selectedCharacterID) {
		return errDungeonHuntEnemyCompletionUnavailable
	}
	if currentDungeonRuntimeIsPractice(runtime) {
		return nil
	}
	return s.persistCurrentDungeonHuntEnemyTargetPhaseA(
		ctx,
		session,
		runtime,
		scene,
		monster.Spawn.MonsterID,
		currentDungeonQuestEnemyTypeForMonster(monster),
		objectKey,
		"runtime_monster_op39",
	)
}

// persistCurrentDungeonHuntMonsterKillPhaseA is the sibling path for PVF
// [hunt monster] quests. Earlier code initialized those quest triggers but
// never consumed ordinary monster deaths, which left targets such as 巨枪多里安
// stuck at 0/1 forever.
func (s *Service) persistCurrentDungeonHuntMonsterKillPhaseA(
	ctx context.Context,
	session *gameSession,
	runtime *runtimeDungeonState,
	scene worldmap.DungeonRoomScene,
	monster runtimeDungeonMonster,
	objectKey uint32,
) error {
	if session == nil || runtime == nil || runtime.Session == nil || session.dungeon.runtime != runtime ||
		monster.Spawn.MonsterID <= 0 || !dungeonRuntimeOwnsCharacter(runtime, session.selectedCharacterID) {
		return errDungeonHuntEnemyCompletionUnavailable
	}
	if currentDungeonRuntimeIsPractice(runtime) {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.Character == nil || repositories.Quest == nil {
		return errDungeonHuntEnemyCompletionUnavailable
	}
	characterID := strconv.FormatUint(uint64(session.selectedCharacterID), 10)
	if _, found, err := repositories.Character.Load(ctx, characterID); err != nil {
		return err
	} else if !found {
		return dnfquest.ErrCharacterNotFound
	}
	if _, found, err := repositories.Quest.Load(ctx, characterID); err != nil {
		return err
	} else if !found {
		return nil
	}
	catalog, err := s.loadQuestCatalog(ctx)
	if err != nil {
		return fmt.Errorf("load hunt-monster quest catalog: %w", err)
	}
	owner, err := dnfquest.NewOwner(repositories)
	if err != nil {
		return fmt.Errorf("create hunt-monster quest owner: %w", err)
	}
	completedAt := s.gameplayNow().UTC()
	completionKey := fmt.Sprintf(
		"op39:char:%d:dungeon:%d:maze:%d:map:%d:seed:%d:monster:%d:object:%d:completed_at_unix_nano:%d",
		session.selectedCharacterID,
		runtime.Dungeon.ID,
		runtime.MazeIndex,
		scene.Map.Map.ID,
		runtime.Seed,
		monster.Spawn.MonsterID,
		objectKey,
		completedAt.UnixNano(),
	)
	result, err := owner.ApplyHuntMonsterKill(ctx, catalog, characterID, dnfquest.HuntMonsterKillInput{
		DungeonID:     runtime.Dungeon.ID,
		Difficulty:    int64(runtime.Request.Difficulty),
		MonsterCode:   monster.Spawn.MonsterID,
		CompletionKey: completionKey,
		CompletedAt:   completedAt,
	})
	if err != nil {
		return fmt.Errorf("persist hunt-monster quest phase A: %w", err)
	}
	if len(result.ChangedFields) == 0 && len(result.Completions) == 0 {
		return nil
	}
	record, found, err := repositories.Quest.Load(ctx, characterID)
	if err != nil {
		return err
	}
	if !found {
		return dnfquest.ErrQuestPersistVerify
	}
	body := buildCurrentActiveQuestSnapshotBody(record, true)
	questIDs := make([]int64, 0, len(result.Completions))
	for _, completion := range result.Completions {
		questIDs = append(questIDs, completion.QuestID)
	}
	s.logGameEvent(session, "game-dungeon-hunt-monster-phase-a-persisted",
		"char_id", session.selectedCharacterID,
		"dungeon_id", runtime.Dungeon.ID,
		"maze_index", runtime.MazeIndex,
		"map_id", scene.Map.Map.ID,
		"monster_id", monster.Spawn.MonsterID,
		"monster_object_key", objectKey,
		"completion_key", completionKey,
		"completion_count", len(result.Completions),
		"quest_ids", questIDs,
		"changed_fields", result.ChangedFields,
		"idempotent", result.Idempotent,
		"reward_granted", currentDungeonHuntMonsterResultAutoGranted(record, result),
		"snapshot_msg_id", currentActiveQuestSnapshotMsgID,
		"snapshot_body_len", len(body),
		"source", "runtime_monster_op39")
	return s.sendGameUpperRawClass(session, currentActiveQuestSnapshotMsgID, body, 0)
}

func (s *Service) persistCurrentDungeonHuntEnemyTargetPhaseA(
	ctx context.Context,
	session *gameSession,
	runtime *runtimeDungeonState,
	scene worldmap.DungeonRoomScene,
	enemyCode int64,
	enemyType int64,
	objectKey uint32,
	source string,
) error {
	if session == nil || runtime == nil || runtime.Session == nil || session.dungeon.runtime != runtime ||
		enemyCode <= 0 || enemyType <= 0 || !dungeonRuntimeOwnsCharacter(runtime, session.selectedCharacterID) {
		return errDungeonHuntEnemyCompletionUnavailable
	}
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.Character == nil || repositories.Quest == nil {
		return errDungeonHuntEnemyCompletionUnavailable
	}
	completedAt := s.gameplayNow().UTC()
	plan := currentDungeonHuntEnemyPersistencePlan{
		characterID:         session.selectedCharacterID,
		characterGeneration: session.characterGeneration,
		characterKey:        strconv.FormatUint(uint64(session.selectedCharacterID), 10),
		dungeonID:           runtime.Dungeon.ID,
		difficulty:          int64(runtime.Request.Difficulty),
		mazeIndex:           runtime.MazeIndex,
		mapID:               scene.Map.Map.ID,
		seed:                runtime.Seed,
		enemyCode:           enemyCode,
		enemyType:           enemyType,
		objectKey:           objectKey,
		completedAt:         completedAt,
		source:              source,
		repositories:        repositories,
	}
	plan.completionKey = fmt.Sprintf(
		"op39:char:%d:dungeon:%d:maze:%d:map:%d:seed:%d:enemy:%d:object:%d:completed_at_unix_nano:%d",
		plan.characterID, plan.dungeonID, plan.mazeIndex, plan.mapID, plan.seed,
		plan.enemyCode, plan.objectKey,
		completedAt.UnixNano(),
	)
	plan.sequence = reserveCurrentDungeonQuestProgress(session)
	if session.events != nil && session.events.loop != nil && session.events.active.Load() {
		go s.runCurrentDungeonHuntEnemyPersistence(session, plan)
		return nil
	}
	var runErr error
	runCurrentDungeonQuestProgressOrdered(session, plan.sequence, func() {
		if ctx == nil {
			ctx = context.Background()
		}
		result := s.executeCurrentDungeonHuntEnemyPersistence(ctx, plan)
		if result.err != nil {
			runErr = result.err
			return
		}
		runErr = s.completeCurrentDungeonHuntEnemyPersistence(session, plan, result)
	})
	return runErr
}

func (s *Service) runCurrentDungeonHuntEnemyPersistence(
	session *gameSession,
	plan currentDungeonHuntEnemyPersistencePlan,
) {
	runCurrentDungeonQuestProgressOrdered(session, plan.sequence, func() {
		ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
		result := s.executeCurrentDungeonHuntEnemyPersistence(ctx, plan)
		cancel()
		if result.err != nil {
			s.logCurrentDungeonHuntEnemyPersistenceFailure(session, plan, result)
			return
		}
		if len(result.body) == 0 {
			return
		}
		err := s.postGameSessionCharacterEvent(
			session,
			"dungeon-hunt-enemy-persistence-complete",
			plan.characterID,
			plan.characterGeneration,
			func() error { return s.completeCurrentDungeonHuntEnemyPersistence(session, plan, result) },
		)
		if err != nil && !isClosedGameSessionEventError(err) {
			s.logPacketEvent("game-session-event-submit-failed",
				"conn_id", session.connID,
				"source", "dungeon-hunt-enemy-persistence-complete",
				"char_id", plan.characterID,
				"quest_progress_sequence", plan.sequence,
				"error", err)
		}
	})
}

func (s *Service) executeCurrentDungeonHuntEnemyPersistence(
	ctx context.Context,
	plan currentDungeonHuntEnemyPersistencePlan,
) (result currentDungeonHuntEnemyPersistenceResult) {
	started := time.Now()
	defer func() { result.elapsed = time.Since(started) }()
	if _, found, err := plan.repositories.Character.Load(ctx, plan.characterKey); err != nil {
		result.err = err
		return result
	} else if !found {
		result.err = dnfquest.ErrCharacterNotFound
		return result
	}
	if _, found, err := plan.repositories.Quest.Load(ctx, plan.characterKey); err != nil {
		result.err = err
		return result
	} else if !found {
		return result
	}
	catalog, err := s.loadQuestCatalog(ctx)
	if err != nil {
		result.err = fmt.Errorf("load hunt-enemy quest catalog: %w", err)
		return result
	}
	owner, err := dnfquest.NewOwner(plan.repositories)
	if err != nil {
		result.err = fmt.Errorf("create hunt-enemy quest owner: %w", err)
		return result
	}
	result.persisted, err = owner.ApplyHuntEnemyKill(ctx, catalog, plan.characterKey, dnfquest.HuntEnemyKillInput{
		DungeonID:     plan.dungeonID,
		Difficulty:    plan.difficulty,
		EnemyCode:     plan.enemyCode,
		EnemyType:     plan.enemyType,
		CompletionKey: plan.completionKey,
		CompletedAt:   plan.completedAt,
	})
	if err != nil {
		result.err = fmt.Errorf("persist hunt-enemy quest phase A: %w", err)
		return result
	}
	if len(result.persisted.ChangedFields) == 0 && len(result.persisted.Completions) == 0 {
		return result
	}
	var found bool
	result.record, found, err = plan.repositories.Quest.Load(ctx, plan.characterKey)
	if err != nil {
		result.err = err
		return result
	}
	if !found {
		result.err = dnfquest.ErrQuestPersistVerify
		return result
	}
	result.body = buildCurrentActiveQuestSnapshotBody(result.record, true)
	return result
}

func (s *Service) completeCurrentDungeonHuntEnemyPersistence(
	session *gameSession,
	plan currentDungeonHuntEnemyPersistencePlan,
	result currentDungeonHuntEnemyPersistenceResult,
) error {
	if len(result.body) == 0 {
		return nil
	}
	questIDs := make([]int64, 0, len(result.persisted.Completions))
	for _, completion := range result.persisted.Completions {
		questIDs = append(questIDs, completion.QuestID)
	}
	s.logGameEvent(session, "game-dungeon-hunt-enemy-phase-a-persisted",
		"char_id", plan.characterID,
		"dungeon_id", plan.dungeonID,
		"maze_index", plan.mazeIndex,
		"map_id", plan.mapID,
		"enemy_code", plan.enemyCode,
		"monster_object_key", plan.objectKey,
		"enemy_type", plan.enemyType,
		"completion_key", plan.completionKey,
		"completion_count", len(result.persisted.Completions),
		"quest_ids", questIDs,
		"changed_fields", result.persisted.ChangedFields,
		"idempotent", result.persisted.Idempotent,
		"reward_granted", currentDungeonHuntEnemyResultAutoGranted(result.record, result.persisted),
		"snapshot_msg_id", currentActiveQuestSnapshotMsgID,
		"snapshot_body_len", len(result.body),
		"quest_progress_sequence", plan.sequence,
		"elapsed_ms", result.elapsed.Milliseconds(),
		"source", plan.source)
	return s.sendGameUpperRawClass(session, currentActiveQuestSnapshotMsgID, result.body, 0)
}

func (s *Service) logCurrentDungeonHuntEnemyPersistenceFailure(
	session *gameSession,
	plan currentDungeonHuntEnemyPersistencePlan,
	result currentDungeonHuntEnemyPersistenceResult,
) {
	s.logGameEvent(session, "game-dungeon-hunt-enemy-progress-deferred",
		"char_id", plan.characterID,
		"dungeon_id", plan.dungeonID,
		"maze_index", plan.mazeIndex,
		"map_id", plan.mapID,
		"enemy_code", plan.enemyCode,
		"enemy_type", plan.enemyType,
		"monster_object_key", plan.objectKey,
		"quest_progress_sequence", plan.sequence,
		"elapsed_ms", result.elapsed.Milliseconds(),
		"error", result.err,
		"source", plan.source)
}

func currentDungeonHuntEnemyResultAutoGranted(record dnfrepo.QuestRecord, result dnfquest.HuntEnemyPersistResult) bool {
	for _, completion := range result.Completions {
		state, known := currentDungeonHuntEnemyQuestState(record, completion.QuestID)
		if known && state.Status == "completed" && state.Extra["reward_state"] == "granted" {
			return true
		}
	}
	return false
}

func currentDungeonHuntMonsterResultAutoGranted(record dnfrepo.QuestRecord, result dnfquest.HuntMonsterPersistResult) bool {
	for _, completion := range result.Completions {
		state, known := currentDungeonHuntEnemyQuestState(record, completion.QuestID)
		if known && state.Status == "completed" && state.Extra["reward_state"] == "granted" {
			return true
		}
	}
	return false
}

func currentDungeonHuntEnemyQuestState(record dnfrepo.QuestRecord, questID int64) (dnfrepo.QuestState, bool) {
	if state, ok := record.States[questID]; ok {
		return state, true
	}
	if state, ok := record.Progress[questID]; ok {
		return state, true
	}
	return dnfrepo.QuestState{}, false
}

func currentDungeonQuestEnemyTypeForMonster(monster runtimeDungeonMonster) int64 {
	// 86JP's hunt-enemy domain uses type 1 for monsters and type 3 for passive
	// objects. Dungeon actor wire types/ranks are a different namespace: a boss
	// actor must not be reclassified as passive-object quest target type 3.
	_ = monster
	return 1
}
