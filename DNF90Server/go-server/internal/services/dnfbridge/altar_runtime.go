package dnfbridge

import (
	"context"
	"errors"
	"fmt"
	"time"

	"longheng.io/server/internal/modules/dnf/dnfenum"
	"longheng.io/server/internal/modules/dnf/dungeoncmd"
	"longheng.io/server/internal/modules/dnf/worldmap"
)

const (
	currentDungeonAltarFinalRoundDelay      = time.Second
	currentDungeonAltarWaveRetryDelay       = 100 * time.Millisecond
	currentDungeonAltarQuestDropRetryDelay  = time.Second
	currentDungeonAltarRoomMoveRetryDelay   = time.Second
	currentDungeonAltarCompletionRetryDelay = time.Second
)

func dungeonAltarGameplayModule() gameplayModuleDefinition {
	deathOpcode := uint16(dnfenum.CmdPacketDieBloodMonster)
	readyOpcode := uint16(dnfenum.CmdPacketBloodRoundUIPrepareFinish)
	difficultyOpcode := uint16(dnfenum.CmdPacketSelectUltimateDifficulty)
	return gameplayModuleDefinition{
		Name: "dungeon-altar",
		LegacyHandlers: map[uint16]gameplayHandler{
			deathOpcode: func(service *Service, session *gameSession, request gameplayRequest) error {
				return service.handleCurrentDungeonAltarDeaths(session, request.Body)
			},
			readyOpcode: func(service *Service, session *gameSession, request gameplayRequest) error {
				return service.handleCurrentDungeonAltarReady(session, request.Body)
			},
			difficultyOpcode: func(service *Service, session *gameSession, request gameplayRequest) error {
				return service.handleCurrentDungeonAltarDifficulty(session, request.Body)
			},
		},
		UpperHandlers: map[uint16]gameplayHandler{
			deathOpcode: defaultClassGameplayHandler(
				"game-dungeon-altar-death-blocked",
				"current_exe_op255_command_class_mismatch",
				func(service *Service, session *gameSession, body []byte) error {
					return service.handleCurrentDungeonAltarDeaths(session, body)
				},
			),
			readyOpcode: defaultClassGameplayHandler(
				"game-dungeon-altar-ready-blocked",
				"current_exe_op258_command_class_mismatch",
				func(service *Service, session *gameSession, body []byte) error {
					return service.handleCurrentDungeonAltarReady(session, body)
				},
			),
			difficultyOpcode: defaultClassGameplayHandler(
				"game-dungeon-altar-difficulty-blocked",
				"current_exe_op317_command_class_mismatch",
				func(service *Service, session *gameSession, body []byte) error {
					return service.handleCurrentDungeonAltarDifficulty(session, body)
				},
			),
		},
	}
}

func (s *Service) handleCurrentDungeonAltarReady(session *gameSession, body []byte) error {
	if len(body) != 0 {
		s.logGameEvent(session, "game-dungeon-altar-ready-blocked",
			"body_len", len(body),
			"reason", "current_exe_op258_requires_empty_body")
		return nil
	}
	if session == nil {
		return nil
	}
	session.dungeon.mu.Lock()
	defer session.dungeon.mu.Unlock()
	runtime, scene, ok := currentDungeonAltarOwnerLocked(session)
	if !ok {
		s.logGameEvent(session, "game-dungeon-altar-ready-blocked", "reason", "active_altar_runtime_missing")
		return nil
	}
	schedule, accepted, err := runtime.altar.acceptReady(runtimeDungeonRoomKeyFromScene(scene), s.gameplayNow())
	if err != nil {
		s.logGameEvent(session, "game-dungeon-altar-ready-blocked", "reason", "room_generation_mismatch", "error", err)
		return nil
	}
	if !accepted {
		s.logGameEvent(session, "game-dungeon-altar-ready-idempotent",
			"dungeon_id", runtime.Dungeon.ID,
			"room", scene.Coordinate.String(),
			"room_generation", runtime.altar.Room.Generation)
		return nil
	}
	return s.startCurrentDungeonAltarScheduleLocked(session, runtime, schedule, "client_op258_ready")
}

func (s *Service) handleCurrentDungeonAltarDeaths(session *gameSession, body []byte) error {
	sequences, err := decodeCurrentDungeonAltarDeathSequences(body)
	if err != nil {
		s.logGameEvent(session, "game-dungeon-altar-death-blocked",
			"body_len", len(body), "reason", "current_exe_op255_body_malformed", "error", err)
		return nil
	}
	if session == nil {
		return nil
	}
	session.dungeon.mu.Lock()
	defer session.dungeon.mu.Unlock()
	runtime, scene, ok := currentDungeonAltarOwnerLocked(session)
	if !ok {
		s.logGameEvent(session, "game-dungeon-altar-death-blocked", "reason", "active_altar_runtime_missing")
		return nil
	}
	return s.handleCurrentDungeonAltarDeathsLocked(
		session,
		runtime,
		scene,
		sequences,
		"client_op255_death",
		false,
	)
}

// handleCurrentDungeonAltarDeathsLocked owns every accepted altar death
// transport after its caller has authenticated a current-room server sequence.
// The native altar op255 and the current client's dynamic-monster op39 writer
// must share this transaction/progression boundary: routing op39 through the
// ordinary monster path would skip the altar sequence owner and leave the
// fifth Endless round permanently active.
//
// The caller must hold session.dungeon.mu.
func (s *Service) handleCurrentDungeonAltarDeathsLocked(
	session *gameSession,
	runtime *runtimeDungeonState,
	scene worldmap.DungeonRoomScene,
	sequences []uint16,
	source string,
	dynamicDeathNotification bool,
) error {
	if session == nil || runtime == nil || runtime.altar == nil || runtime.Session == nil ||
		session.dungeon.runtime != runtime || runtimeDungeonRoomKeyFromScene(scene) != runtime.altar.Room.Key {
		s.logGameEvent(session, "game-dungeon-altar-death-blocked",
			"source", source,
			"reason", "active_altar_runtime_missing")
		return nil
	}
	roomKey := runtimeDungeonRoomKeyFromScene(scene)
	preview, err := runtime.altar.previewDeaths(roomKey, sequences)
	if err != nil {
		s.logGameEvent(session, "game-dungeon-altar-death-blocked",
			"dungeon_id", runtime.Dungeon.ID,
			"room", scene.Coordinate.String(),
			"room_generation", runtime.altar.Room.Generation,
			"sequences", sequences,
			"source", source,
			"reason", "server_owned_sequence_validation_failed",
			"error", err)
		return nil
	}
	if runtime.altar.questDropRetry != nil {
		return s.enqueueCurrentDungeonAltarDeathsBehindRetryLocked(
			session,
			runtime,
			scene,
			sequences,
			preview,
			source,
			dynamicDeathNotification,
		)
	}
	if len(preview.Active) != 0 {
		if err := s.commitCurrentDungeonAltarQuestDropsLocked(session, runtime, preview.Active); err != nil {
			pending, freezeErr := freezeCurrentDungeonAltarQuestDropRetry(
				runtime.altar,
				sequences,
				preview.Active,
				dynamicDeathNotification,
			)
			if freezeErr != nil {
				return errors.Join(err, freezeErr)
			}
			retryErr := s.armCurrentDungeonAltarQuestDropRetryLocked(
				session,
				runtime,
				pending,
				"quest_monster_drop_transaction_failed",
			)
			s.logGameEvent(session, "game-dungeon-altar-death-blocked",
				"dungeon_id", runtime.Dungeon.ID,
				"room", scene.Coordinate.String(),
				"room_generation", runtime.altar.Room.Generation,
				"sequence_count", len(preview.Active),
				"source", source,
				"reason", "quest_monster_drop_transaction_failed_server_retry_armed",
				"error", err,
				"retry_error", retryErr)
			if retryErr != nil {
				return errors.Join(err, retryErr)
			}
			return nil
		}
	}
	if dynamicDeathNotification {
		if len(sequences) != 1 {
			return errCurrentDungeonAltarRequestMalformed
		}
		if err := s.sendCurrentDungeonAltarDynamicDeathNotificationLocked(session, sequences[0]); err != nil {
			if len(preview.Active) == 0 {
				return err
			}
			pending, freezeErr := freezeCurrentDungeonAltarQuestDropRetry(
				runtime.altar,
				sequences,
				preview.Active,
				true,
			)
			if freezeErr != nil {
				return errors.Join(err, freezeErr)
			}
			retryErr := s.armCurrentDungeonAltarQuestDropRetryLocked(
				session,
				runtime,
				pending,
				"dynamic_monster_op38_write_failed",
			)
			s.logGameEvent(session, "game-dungeon-altar-death-blocked",
				"dungeon_id", runtime.Dungeon.ID,
				"room", scene.Coordinate.String(),
				"room_generation", runtime.altar.Room.Generation,
				"sequence", sequences[0],
				"source", source,
				"reason", "dynamic_monster_op38_write_failed_server_retry_armed",
				"error", err,
				"retry_error", retryErr)
			if retryErr != nil {
				return errors.Join(err, retryErr)
			}
			return nil
		}
	}
	result, err := runtime.altar.applyDeaths(roomKey, sequences)
	if err != nil {
		return err
	}
	s.logGameEvent(session, "game-dungeon-altar-death-accepted",
		"dungeon_id", runtime.Dungeon.ID,
		"room", scene.Coordinate.String(),
		"accepted", result.Accepted,
		"duplicate", result.Duplicate,
		"active_remaining", len(runtime.altar.activeSequences),
		"source", source,
		"progress", result.Progress)
	progress := result.Progress
	if progress == currentDungeonAltarProgressNone && result.Duplicate != 0 &&
		currentDungeonAltarCompletionNeedsRetry(runtime, session.selectedCharacterID) {
		progress = currentDungeonAltarProgressDungeonComplete
	}
	return s.processCurrentDungeonAltarProgressLocked(session, runtime, progress, source)
}

// currentDungeonAltarDynamicDeathSequenceLocked recognizes only a sequence
// allocated by the current altar room generation. Active sequences must also
// belong to the currently scheduled round; retired sequences are retained so
// a duplicate op39 remains idempotent instead of falling into ordinary actor
// retirement. A cross-room/stale object key is deliberately not claimed.
//
// The caller must hold session.dungeon.mu.
func currentDungeonAltarDynamicDeathSequenceLocked(
	runtime *runtimeDungeonState,
	scene worldmap.DungeonRoomScene,
	objectKey uint32,
) (uint16, bool) {
	if runtime == nil || runtime.altar == nil || objectKey == 0 || objectKey > uint32(^uint16(0)) {
		return 0, false
	}
	altar := runtime.altar
	roomKey := runtimeDungeonRoomKeyFromScene(scene)
	if altar.Room.Generation == 0 || altar.Room.Key != roomKey {
		return 0, false
	}
	sequence := uint16(objectKey)
	if owner, active := altar.activeSequences[sequence]; active {
		if altar.schedule == nil || owner.RoundGeneration != altar.schedule.Generation ||
			owner.RoomKey != roomKey || owner.RoomGeneration != altar.Room.Generation {
			return 0, false
		}
		return sequence, true
	}
	owner, retired := altar.retiredSequences[sequence]
	if !retired || owner.RoundGeneration == 0 || owner.RoomKey != roomKey ||
		owner.RoomGeneration != altar.Room.Generation {
		return 0, false
	}
	return sequence, true
}

func (s *Service) enqueueCurrentDungeonAltarDeathsBehindRetryLocked(
	session *gameSession,
	runtime *runtimeDungeonState,
	scene worldmap.DungeonRoomScene,
	sequences []uint16,
	preview currentDungeonAltarDeathPreview,
	source string,
	dynamicDeathNotification bool,
) error {
	if runtime == nil || runtime.altar == nil || runtime.altar.questDropRetry == nil {
		return errCurrentDungeonAltarQuestDropInvalid
	}
	altar := runtime.altar
	novelDeaths := make([]currentDungeonAltarDeathSequence, 0, len(preview.Active))
	for _, death := range preview.Active {
		if !currentDungeonAltarRetryOwnsSequence(altar, death.Sequence) {
			novelDeaths = append(novelDeaths, death)
		}
	}
	if len(novelDeaths) == 0 {
		if dynamicDeathNotification && preview.Duplicate != 0 && len(sequences) == 1 {
			if err := s.sendCurrentDungeonAltarDynamicDeathNotificationLocked(session, sequences[0]); err != nil {
				return err
			}
		}
		s.logGameEvent(session, "game-dungeon-altar-death-idempotent",
			"dungeon_id", runtime.Dungeon.ID,
			"room", scene.Coordinate.String(),
			"room_generation", altar.Room.Generation,
			"sequences", sequences,
			"pending_batch_count", 1+len(altar.questDropRetryQueue),
			"source", source,
			"reason", "server_owned_death_already_retired_or_queued")
		if altar.questDropRetryTimerName == "" {
			return s.scheduleCurrentDungeonAltarQuestDropRetryLocked(
				session,
				runtime,
				altar.questDropRetry,
				"idempotent_death_rearmed_missing_retry_timer",
			)
		}
		return nil
	}
	novelSequences := make([]uint16, 0, len(novelDeaths))
	for _, death := range novelDeaths {
		novelSequences = append(novelSequences, death.Sequence)
	}
	pending, err := freezeCurrentDungeonAltarQuestDropRetry(
		altar,
		novelSequences,
		novelDeaths,
		dynamicDeathNotification,
	)
	if err != nil {
		return err
	}
	if err := enqueueCurrentDungeonAltarQuestDropRetryLocked(altar, pending); err != nil {
		return err
	}
	s.logGameEvent(session, "game-dungeon-altar-death-retry-queued",
		"dungeon_id", runtime.Dungeon.ID,
		"room", scene.Coordinate.String(),
		"room_generation", altar.Room.Generation,
		"sequence_count", len(novelSequences),
		"dynamic_death_notification", dynamicDeathNotification,
		"pending_batch_count", 1+len(altar.questDropRetryQueue),
		"source", source)
	if altar.questDropRetryTimerName == "" {
		return s.scheduleCurrentDungeonAltarQuestDropRetryLocked(
			session,
			runtime,
			altar.questDropRetry,
			"queued_death_rearmed_missing_retry_timer",
		)
	}
	return nil
}

func currentDungeonAltarRetryOwnsSequence(altar *currentDungeonAltarRuntime, sequence uint16) bool {
	if altar == nil || sequence == 0 {
		return false
	}
	contains := func(pending *currentDungeonAltarQuestDropRetry) bool {
		if pending == nil || pending.Room != altar.Room {
			return false
		}
		for _, death := range pending.Deaths {
			if death.Sequence == sequence {
				return true
			}
		}
		return false
	}
	if contains(altar.questDropRetry) {
		return true
	}
	for _, pending := range altar.questDropRetryQueue {
		if contains(pending) {
			return true
		}
	}
	return false
}

func enqueueCurrentDungeonAltarQuestDropRetryLocked(
	altar *currentDungeonAltarRuntime,
	pending *currentDungeonAltarQuestDropRetry,
) error {
	if altar == nil || pending == nil || pending.Room != altar.Room || len(pending.Deaths) == 0 ||
		len(pending.Sequences) == 0 {
		return errCurrentDungeonAltarQuestDropInvalid
	}
	if altar.questDropRetry == nil {
		altar.questDropRetry = pending
		return nil
	}
	altar.questDropRetryQueue = append(altar.questDropRetryQueue, pending)
	return nil
}

func freezeCurrentDungeonAltarQuestDropRetry(
	altar *currentDungeonAltarRuntime,
	sequences []uint16,
	deaths []currentDungeonAltarDeathSequence,
	dynamicDeathNotification bool,
) (*currentDungeonAltarQuestDropRetry, error) {
	if altar == nil || altar.Room.Generation == 0 || len(sequences) == 0 ||
		len(sequences) > currentDungeonAltarMaximumDeathSequences || len(deaths) == 0 || len(deaths) > len(sequences) {
		return nil, errCurrentDungeonAltarQuestDropInvalid
	}
	if dynamicDeathNotification && len(sequences) != 1 {
		return nil, errCurrentDungeonAltarQuestDropInvalid
	}
	for _, death := range deaths {
		if death.Sequence == 0 || death.Owner.RoomKey != altar.Room.Key ||
			death.Owner.RoomGeneration != altar.Room.Generation || death.Owner.RoundGeneration == 0 ||
			death.Owner.MonsterCode <= 0 || death.Owner.QuestDropSeed == 0 {
			return nil, errCurrentDungeonAltarQuestDropInvalid
		}
	}
	return &currentDungeonAltarQuestDropRetry{
		Room:                     altar.Room,
		Sequences:                append([]uint16(nil), sequences...),
		Deaths:                   append([]currentDungeonAltarDeathSequence(nil), deaths...),
		DynamicDeathNotification: dynamicDeathNotification,
	}, nil
}

func (s *Service) sendCurrentDungeonAltarDynamicDeathNotificationLocked(
	session *gameSession,
	sequence uint16,
) error {
	body, err := buildCurrentDungeonDeathNotificationBody(uint32(sequence), currentDungeonDeathResponseMonster)
	if err != nil {
		return err
	}
	return s.sendGameUpperRawClass(
		session,
		uint16(dnfenum.CmdPacketNotifyDieMonster),
		body,
		0,
	)
}

func (s *Service) handleCurrentDungeonAltarDifficulty(session *gameSession, body []byte) error {
	difficulty, err := decodeCurrentDungeonAltarDifficulty(body)
	if err != nil {
		s.logGameEvent(session, "game-dungeon-altar-difficulty-blocked",
			"body_len", len(body), "reason", "current_exe_op317_body_malformed", "error", err)
		return nil
	}
	if session == nil {
		return nil
	}
	session.dungeon.mu.Lock()
	defer session.dungeon.mu.Unlock()
	runtime, _, ok := currentDungeonAltarOwnerLocked(session)
	if !ok {
		s.logGameEvent(session, "game-dungeon-altar-difficulty-blocked", "reason", "active_altar_runtime_missing")
		return nil
	}
	altar := runtime.altar
	roomGeneration, promptVersion := altar.Room.Generation, altar.difficultyVersion
	if !altar.resolveDifficulty(roomGeneration, promptVersion, difficulty) {
		s.logGameEvent(session, "game-dungeon-altar-difficulty-idempotent",
			"difficulty", difficulty,
			"room_generation", roomGeneration,
			"prompt_version", promptVersion,
			"reason", "selection_or_timeout_already_won")
		return nil
	}
	if altar.difficultyTimerName != "" && s.gameplayTimers != nil {
		s.gameplayTimers.Cancel(altar.difficultyTimerName)
		altar.difficultyTimerName = ""
	}
	ack, err := buildCurrentDungeonAltarDifficultyResolvedBody(difficulty)
	if err != nil {
		return err
	}
	// Current EXE op317's common success byte is owned by
	// sendGameUpperSuccess; the command-specific body is only difficulty.
	if err := s.sendGameUpperSuccess(session, uint16(dnfenum.CmdPacketSelectUltimateDifficulty), ack); err != nil {
		return err
	}
	schedule, err := altar.beginRound(s.gameplayNow())
	if err != nil {
		return err
	}
	return s.startCurrentDungeonAltarScheduleLocked(session, runtime, schedule, "client_op317_difficulty")
}

func currentDungeonAltarOwnerLocked(
	session *gameSession,
) (*runtimeDungeonState, worldmap.DungeonRoomScene, bool) {
	if session == nil || session.dungeon.runtime == nil || session.dungeon.runtime.altar == nil || session.dungeon.runtime.Session == nil {
		return nil, worldmap.DungeonRoomScene{}, false
	}
	runtime := session.dungeon.runtime
	scene, ok := runtime.Session.Scene()
	if !ok || runtimeDungeonRoomKeyFromScene(scene) != runtime.altar.Room.Key {
		return nil, worldmap.DungeonRoomScene{}, false
	}
	return runtime, scene, true
}

func (s *Service) startCurrentDungeonAltarScheduleLocked(
	session *gameSession,
	runtime *runtimeDungeonState,
	schedule *currentDungeonAltarRoundSchedule,
	source string,
) error {
	if schedule == nil {
		return nil
	}
	body := buildCurrentDungeonAltarRoundIntervalBody(schedule.GlobalRound, schedule.InitialInterval.Milliseconds())
	if err := s.sendGameUpperRawClass(session, currentDungeonAltarRoundIntervalMsgID, body, 0); err != nil {
		return err
	}
	if runtime.altar.crazyMonster != nil && schedule.RoundIndex == runtime.altar.crazyMonster.RoundIndex {
		crazyBody, err := buildCurrentDungeonAltarCrazyMonsterBody(*runtime.altar.crazyMonster)
		if err != nil {
			return err
		}
		if err := s.sendGameUpperRawClass(session, currentDungeonAltarCrazyMonsterMsgID, crazyBody, 0); err != nil {
			return err
		}
		s.logGameEvent(session, "game-dungeon-endless-altar-crazy-monster-sent",
			"dungeon_id", runtime.Dungeon.ID,
			"room", runtime.altar.Room.Key,
			"round_generation", schedule.Generation,
			"local_round", schedule.RoundIndex,
			"monster_id", runtime.altar.crazyMonster.MonsterCode,
			"variant", runtime.altar.crazyMonster.Variant,
			"body_source", "reference_server_blood_crazy_monster_current_exe_op213_u32_u8")
	}
	s.logGameEvent(session, "game-dungeon-altar-round-started",
		"source", source,
		"dungeon_id", runtime.Dungeon.ID,
		"room", runtime.altar.Room.Key,
		"room_generation", schedule.RoomGeneration,
		"round_generation", schedule.Generation,
		"local_round", schedule.RoundIndex,
		"global_round", schedule.GlobalRound,
		"difficulty", schedule.Difficulty,
		"wave_count", len(schedule.Waves))
	return s.scheduleCurrentDungeonAltarWaveLocked(session, runtime, schedule.Generation, 0)
}

func (s *Service) scheduleCurrentDungeonAltarWaveLocked(
	session *gameSession,
	runtime *runtimeDungeonState,
	scheduleGeneration uint64,
	retryDelay time.Duration,
) error {
	if s == nil || session == nil || runtime == nil || runtime.altar == nil || runtime.altar.schedule == nil ||
		runtime.altar.schedule.Generation != scheduleGeneration {
		return nil
	}
	altar := runtime.altar
	if altar.nextWaveIndex >= len(altar.schedule.Waves) {
		return nil
	}
	queue := s.ensureGameplayTimeQueue()
	if queue == nil {
		return errGameplayTimeQueueUnavailable
	}
	waveIndex := altar.nextWaveIndex
	delay := altar.schedule.StartedAt.Add(altar.schedule.Waves[waveIndex].Delay).Sub(queue.Now())
	if delay < retryDelay {
		delay = retryDelay
	}
	if delay < 0 {
		delay = 0
	}
	roomGeneration := altar.Room.Generation
	timerName := fmt.Sprintf("dnf-dungeon-altar-wave:%s:run:%d:room:%d:round:%d:wave:%d", session.connID, runtime.lifecycleToken, roomGeneration, scheduleGeneration, waveIndex)
	if altar.waveTimerName != "" {
		queue.Cancel(altar.waveTimerName)
	}
	altar.waveTimerName = timerName
	characterID, characterGeneration, err := gameSessionCharacterEventIdentity(session, session.selectedCharacterID)
	if err != nil {
		return err
	}
	return queue.ScheduleAfter(timerName, delay, func(due time.Time) {
		postErr := s.postGameSessionCharacterEvent(
			session,
			"dungeon-altar-wave-timequeue",
			characterID,
			characterGeneration,
			func() error {
				return s.fireCurrentDungeonAltarWave(session, runtime, roomGeneration, scheduleGeneration, waveIndex, due)
			},
		)
		if postErr != nil && !isClosedGameSessionEventError(postErr) {
			s.logGameEvent(session, "game-dungeon-altar-wave-post-failed", "error", postErr)
		}
	})
}

func (s *Service) fireCurrentDungeonAltarWave(
	session *gameSession,
	runtime *runtimeDungeonState,
	roomGeneration uint64,
	scheduleGeneration uint64,
	waveIndex int,
	due time.Time,
) error {
	if session == nil || runtime == nil {
		return nil
	}
	session.dungeon.mu.Lock()
	defer session.dungeon.mu.Unlock()
	if session.dungeon.runtime != runtime || runtime.altar == nil || runtime.altar.Room.Generation != roomGeneration ||
		runtime.altar.schedule == nil || runtime.altar.schedule.Generation != scheduleGeneration ||
		runtime.altar.nextWaveIndex != waveIndex {
		return nil
	}
	runtime.altar.waveTimerName = ""
	wave, err := runtime.altar.reserveWave(scheduleGeneration, waveIndex)
	if err != nil {
		return err
	}
	body, err := buildCurrentDungeonAltarMonsterSpawnBody(wave)
	if err != nil {
		return err
	}
	expiryTimerName, err := s.scheduleCurrentDungeonAltarWaveExpiryLocked(session, runtime, scheduleGeneration, waveIndex, wave)
	if err != nil {
		if scheduleErr := s.scheduleCurrentDungeonAltarWaveLocked(session, runtime, scheduleGeneration, currentDungeonAltarWaveRetryDelay); scheduleErr != nil {
			return errors.Join(err, scheduleErr)
		}
		return err
	}
	if err := s.sendGameUpperRawClass(session, currentDungeonAltarMonsterSpawnMsgID, body, 0); err != nil {
		s.cancelCurrentDungeonAltarWaveExpiryLocked(runtime, expiryTimerName)
		if scheduleErr := s.scheduleCurrentDungeonAltarWaveLocked(session, runtime, scheduleGeneration, currentDungeonAltarWaveRetryDelay); scheduleErr != nil {
			return errors.Join(err, scheduleErr)
		}
		return err
	}
	progress, err := runtime.altar.commitWave(wave)
	if err != nil {
		s.cancelCurrentDungeonAltarWaveExpiryLocked(runtime, expiryTimerName)
		return err
	}
	s.logGameEvent(session, "game-dungeon-altar-wave-spawned",
		"dungeon_id", runtime.Dungeon.ID,
		"room_generation", roomGeneration,
		"round_generation", scheduleGeneration,
		"wave_index", waveIndex,
		"spawn_count", len(wave.Spawns),
		"scheduled_due_utc", due.UTC().Format(time.RFC3339Nano))
	if runtime.altar.schedule != nil && runtime.altar.nextWaveIndex < len(runtime.altar.schedule.Waves) {
		return s.scheduleCurrentDungeonAltarWaveLocked(session, runtime, scheduleGeneration, 0)
	}
	return s.processCurrentDungeonAltarProgressLocked(session, runtime, progress, "wave_schedule_complete")
}

func (s *Service) scheduleCurrentDungeonAltarWaveExpiryLocked(
	session *gameSession,
	runtime *runtimeDungeonState,
	scheduleGeneration uint64,
	waveIndex int,
	wave currentDungeonAltarWave,
) (string, error) {
	if s == nil || session == nil || runtime == nil || runtime.altar == nil || runtime.altar.schedule == nil ||
		runtime.altar.schedule.Generation != scheduleGeneration || waveIndex < 0 ||
		waveIndex >= len(runtime.altar.schedule.Waves) || len(wave.Spawns) == 0 {
		return "", nil
	}
	altar := runtime.altar
	scheduled := altar.schedule.Waves[waveIndex]
	if scheduled.Monster.DurationMilliseconds <= 0 || scheduled.ExpireDelay <= 0 {
		return "", nil
	}
	queue := s.ensureGameplayTimeQueue()
	if queue == nil {
		return "", errGameplayTimeQueueUnavailable
	}
	deadline := altar.schedule.StartedAt.Add(scheduled.ExpireDelay)
	delay := deadline.Sub(queue.Now())
	if delay < 0 {
		delay = 0
	}
	room := altar.Room
	timerName := fmt.Sprintf("dnf-dungeon-altar-expire:%s:run:%d:room:%d:round:%d:wave:%d",
		session.connID, runtime.lifecycleToken, room.Generation, scheduleGeneration, waveIndex)
	sequences := make([]uint16, len(wave.Spawns))
	for index, spawn := range wave.Spawns {
		sequences[index] = spawn.Sequence
	}
	characterID, characterGeneration, err := gameSessionCharacterEventIdentity(session, session.selectedCharacterID)
	if err != nil {
		return "", err
	}
	if altar.expiryTimerNames == nil {
		altar.expiryTimerNames = make(map[string]struct{})
	}
	if _, exists := altar.expiryTimerNames[timerName]; exists {
		queue.Cancel(timerName)
		delete(altar.expiryTimerNames, timerName)
	}
	if err := queue.ScheduleAfter(timerName, delay, func(due time.Time) {
		postErr := s.postGameSessionCharacterEvent(
			session,
			"dungeon-altar-wave-expiry-timequeue",
			characterID,
			characterGeneration,
			func() error {
				return s.fireCurrentDungeonAltarWaveExpiry(
					session,
					runtime,
					room,
					scheduleGeneration,
					timerName,
					sequences,
					due,
				)
			},
		)
		if postErr != nil && !isClosedGameSessionEventError(postErr) {
			s.logGameEvent(session, "game-dungeon-altar-wave-expiry-post-failed", "error", postErr)
		}
	}); err != nil {
		return "", err
	}
	altar.expiryTimerNames[timerName] = struct{}{}
	return timerName, nil
}

func (s *Service) fireCurrentDungeonAltarWaveExpiry(
	session *gameSession,
	runtime *runtimeDungeonState,
	room currentDungeonAltarRoomIdentity,
	scheduleGeneration uint64,
	timerName string,
	sequences []uint16,
	due time.Time,
) error {
	if session == nil || runtime == nil || timerName == "" {
		return nil
	}
	session.dungeon.mu.Lock()
	defer session.dungeon.mu.Unlock()
	if runtime.altar == nil {
		return nil
	}
	if _, active := runtime.altar.expiryTimerNames[timerName]; !active {
		return nil
	}
	delete(runtime.altar.expiryTimerNames, timerName)
	if session.dungeon.runtime != runtime || runtime.altar.Room != room || runtime.altar.schedule == nil ||
		runtime.altar.schedule.Generation != scheduleGeneration {
		return nil
	}
	expired, progress, err := runtime.altar.expireWave(room, scheduleGeneration, sequences)
	if err != nil {
		return err
	}
	if expired != 0 {
		s.logGameEvent(session, "game-dungeon-altar-wave-expired",
			"dungeon_id", runtime.Dungeon.ID,
			"room", room.Key,
			"room_generation", room.Generation,
			"round_generation", scheduleGeneration,
			"expired", expired,
			"active_remaining", len(runtime.altar.activeSequences),
			"due_utc", due.UTC().Format(time.RFC3339Nano),
			"progress", progress)
	}
	return s.processCurrentDungeonAltarProgressLocked(session, runtime, progress, "wave_duration_expired")
}

func (s *Service) processCurrentDungeonAltarProgressLocked(
	session *gameSession,
	runtime *runtimeDungeonState,
	progress currentDungeonAltarProgress,
	source string,
) error {
	if runtime == nil || runtime.altar == nil || progress == currentDungeonAltarProgressNone {
		return nil
	}
	s.cancelCurrentDungeonAltarExpiryTimersLocked(runtime)
	altar := runtime.altar
	switch progress {
	case currentDungeonAltarProgressRoundActive:
		schedule, err := altar.beginRound(s.gameplayNow())
		if err != nil {
			return err
		}
		return s.startCurrentDungeonAltarScheduleLocked(session, runtime, schedule, source+"_next_round")

	case currentDungeonAltarProgressAwaitingDifficulty:
		body := buildCurrentDungeonAltarDifficultyPromptBody(altar.completedRounds, currentDungeonAltarDifficultyTimeoutSeconds)
		if err := s.sendGameUpperRawClass(session, currentDungeonAltarDifficultyUIMsgID, body, 0); err != nil {
			return err
		}
		return s.scheduleCurrentDungeonAltarDifficultyTimeoutLocked(session, runtime)

	case currentDungeonAltarProgressFinalRoundDelay:
		return s.scheduleCurrentDungeonAltarFinalRoundLocked(session, runtime)

	case currentDungeonAltarProgressRoomComplete:
		s.cancelCurrentDungeonAltarTimersLocked(runtime)
		if err := runtime.Session.MarkCurrentRoomScriptedCleared(); err != nil {
			return err
		}
		if err := runtime.cacheCurrentDungeonRoom(); err != nil {
			return err
		}
		s.logGameEvent(session, "game-dungeon-altar-progress-committed",
			"source", source,
			"dungeon_id", runtime.Dungeon.ID,
			"room", altar.Room.Key,
			"completed_rounds", altar.completedRounds,
			"max_rounds", altar.MaxRounds,
			"dungeon_complete", false)
		return s.advanceCurrentDungeonEndlessAltarRoomLocked(session, runtime, source)

	case currentDungeonAltarProgressDungeonComplete:
		return s.completeCurrentDungeonAltarLocked(session, runtime, source)
	}
	return nil
}

func currentDungeonEndlessAltarNextRoom(runtime *runtimeDungeonState) (worldmap.DungeonRoom, error) {
	if runtime == nil || runtime.Session == nil || runtime.altar == nil || runtime.Dungeon.ID != 11006 ||
		runtime.altar.Kind != worldmap.BloodAltarDungeonKindEndless ||
		runtime.altar.progress != currentDungeonAltarProgressRoomComplete {
		return worldmap.DungeonRoom{}, errCurrentDungeonAltarDefinitionInvalid
	}
	altar := runtime.altar
	scene, ok := runtime.Session.Scene()
	if !ok || runtimeDungeonRoomKeyFromScene(scene) != altar.Room.Key {
		return worldmap.DungeonRoom{}, fmt.Errorf(
			"%w: altar_room=%+v session_room=%s/%d",
			errCurrentDungeonAltarRoomMismatch,
			altar.Room.Key,
			scene.Coordinate,
			scene.Map.Map.ID,
		)
	}

	rooms := runtime.Session.Rooms()
	var source *worldmap.DungeonRoom
	byCoordinate := make(map[worldmap.RoomCoordinate]worldmap.DungeonRoom, len(rooms))
	for index := range rooms {
		room := rooms[index]
		if room.Map != nil && room.ResolutionError == "" {
			byCoordinate[room.Coordinate] = room
		}
		if room.Coordinate == scene.Coordinate && room.Map != nil && room.Map.Map.ID == scene.Map.Map.ID {
			source = &rooms[index]
		}
	}
	if source == nil || source.Map == nil || source.ResolutionError != "" {
		return worldmap.DungeonRoom{}, fmt.Errorf(
			"%w: source=%s/%d frozen_topology_room_missing_or_unresolved",
			errCurrentDungeonAltarRoomMismatch,
			scene.Coordinate,
			scene.Map.Map.ID,
		)
	}

	candidates := make([]worldmap.DungeonRoom, 0, len(source.Neighbors))
	seen := make(map[runtimeDungeonRoomKey]struct{}, len(source.Neighbors))
	for _, neighbor := range source.Neighbors {
		target, found := byCoordinate[neighbor.Coordinate]
		if !found || target.Map == nil || target.ResolutionError != "" {
			continue
		}
		key := runtimeDungeonRoomKey{X: target.Coordinate.X, Y: target.Coordinate.Y, MapID: target.Map.Map.ID}
		if _, complete := altar.completedRooms[key]; complete {
			continue
		}
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		candidates = append(candidates, target)
	}
	if len(candidates) != 1 {
		return worldmap.DungeonRoom{}, fmt.Errorf(
			"%w: source=%s/%d unfinished_neighbor_count=%d",
			errCurrentDungeonAltarRoomMismatch,
			scene.Coordinate,
			scene.Map.Map.ID,
			len(candidates),
		)
	}
	target := candidates[0]
	if target.Coordinate.X < 0 || target.Coordinate.X > 255 || target.Coordinate.Y < 0 || target.Coordinate.Y > 255 {
		return worldmap.DungeonRoom{}, fmt.Errorf(
			"%w: target=%s coordinate_exceeds_current_exe_op211_byte_range",
			errCurrentDungeonAltarRoomMismatch,
			target.Coordinate,
		)
	}
	return target, nil
}

// advanceCurrentDungeonEndlessAltarRoomLocked reuses the ordinary op211 move
// transaction after a five-round Endless Altar room is authoritatively clear.
// The current EXE accepts this server-owned notification without an op45 gate.
// A write or pre-commit failure leaves both owners on the source room and is
// retried; once the room generation changes, retries become stale no-ops.
func (s *Service) advanceCurrentDungeonEndlessAltarRoomLocked(
	session *gameSession,
	runtime *runtimeDungeonState,
	source string,
) error {
	if session == nil || runtime == nil || runtime.altar == nil || runtime.Session == nil ||
		session.dungeon.runtime != runtime {
		return errCurrentDungeonAltarDefinitionInvalid
	}
	altar := runtime.altar
	sourceRoom := altar.Room
	target, targetErr := currentDungeonEndlessAltarNextRoom(runtime)
	if targetErr != nil {
		s.logGameEvent(session, "game-dungeon-altar-room-transition-deferred",
			"source", source,
			"dungeon_id", runtime.Dungeon.ID,
			"room", sourceRoom.Key,
			"reason", "unique_unfinished_neighbor_unavailable",
			"error", targetErr)
		return s.scheduleCurrentDungeonAltarRoomMoveRetryLocked(session, runtime, sourceRoom, "target_unavailable")
	}
	expected := runtimeDungeonRoomKey{X: target.Coordinate.X, Y: target.Coordinate.Y, MapID: target.Map.Map.ID}
	moveErr := s.handleDungeonMoveRequestLocked(
		session,
		dungeoncmd.MoveMapRequest{NextX: byte(target.Coordinate.X), NextY: byte(target.Coordinate.Y)},
		0,
		"server_endless_altar_room_complete",
		nil,
	)

	currentScene, hasScene := runtime.Session.Scene()
	currentKey := runtimeDungeonRoomKey{}
	if hasScene {
		currentKey = runtimeDungeonRoomKeyFromScene(currentScene)
	}
	if session.dungeon.runtime == runtime && runtime.altar == altar && hasScene && currentKey == expected &&
		altar.Room.Key == expected && altar.Room.Generation != sourceRoom.Generation {
		s.logGameEvent(session, "game-dungeon-altar-room-transition-committed",
			"source", source,
			"dungeon_id", runtime.Dungeon.ID,
			"from", sourceRoom.Key,
			"to", expected,
			"room_generation", altar.Room.Generation,
			"post_commit_error", moveErr)
		return nil
	}
	if session.dungeon.runtime == runtime && runtime.altar == altar && hasScene && currentKey == sourceRoom.Key &&
		altar.Room == sourceRoom && altar.progress == currentDungeonAltarProgressRoomComplete {
		s.logGameEvent(session, "game-dungeon-altar-room-transition-deferred",
			"source", source,
			"dungeon_id", runtime.Dungeon.ID,
			"room", sourceRoom.Key,
			"target", expected,
			"reason", "op211_move_not_committed",
			"error", moveErr)
		return s.scheduleCurrentDungeonAltarRoomMoveRetryLocked(session, runtime, sourceRoom, "op211_move_not_committed")
	}
	return fmt.Errorf(
		"%w: altar op211 transition partial source=%+v expected=%+v scene=%+v altar=%+v move_error=%v",
		errCurrentDungeonAltarRoomMismatch,
		sourceRoom,
		expected,
		currentKey,
		altar.Room,
		moveErr,
	)
}

func (s *Service) scheduleCurrentDungeonAltarRoomMoveRetryLocked(
	session *gameSession,
	runtime *runtimeDungeonState,
	room currentDungeonAltarRoomIdentity,
	reason string,
) error {
	queue := s.ensureGameplayTimeQueue()
	if queue == nil || session == nil || runtime == nil || runtime.Session == nil || runtime.altar == nil ||
		session.dungeon.runtime != runtime || runtime.Dungeon.ID != 11006 ||
		runtime.altar.Kind != worldmap.BloodAltarDungeonKindEndless || runtime.altar.Room != room ||
		runtime.altar.progress != currentDungeonAltarProgressRoomComplete {
		return errGameplayTimeQueueUnavailable
	}
	altar := runtime.altar
	if altar.roomTransitionTimerName != "" {
		queue.Cancel(altar.roomTransitionTimerName)
	}
	altar.roomTransitionVersion = nextCurrentDungeonAltarGeneration(altar.roomTransitionVersion)
	retryVersion := altar.roomTransitionVersion
	lifecycleToken := runtime.lifecycleToken
	timerName := fmt.Sprintf(
		"dnf-dungeon-altar-room-move-retry:%s:run:%d:room:%d:retry:%d",
		session.connID,
		lifecycleToken,
		room.Generation,
		retryVersion,
	)
	altar.roomTransitionTimerName = timerName
	characterID, characterGeneration, err := gameSessionCharacterEventIdentity(session, session.selectedCharacterID)
	if err != nil {
		altar.roomTransitionTimerName = ""
		return err
	}
	if err := queue.ScheduleAfter(timerName, currentDungeonAltarRoomMoveRetryDelay, func(due time.Time) {
		postErr := s.postGameSessionCharacterEvent(
			session,
			"dungeon-altar-room-move-retry-timequeue",
			characterID,
			characterGeneration,
			func() error {
				return s.fireCurrentDungeonAltarRoomMoveRetry(
					session,
					runtime,
					lifecycleToken,
					room,
					retryVersion,
					timerName,
					due,
				)
			},
		)
		if postErr != nil && !isClosedGameSessionEventError(postErr) {
			s.logGameEvent(session, "game-dungeon-altar-room-move-retry-post-failed", "error", postErr)
		}
	}); err != nil {
		altar.roomTransitionTimerName = ""
		return err
	}
	s.logGameEvent(session, "game-dungeon-altar-room-move-retry-armed",
		"dungeon_id", runtime.Dungeon.ID,
		"room_generation", room.Generation,
		"retry_version", retryVersion,
		"delay", currentDungeonAltarRoomMoveRetryDelay,
		"reason", reason)
	return nil
}

func (s *Service) fireCurrentDungeonAltarRoomMoveRetry(
	session *gameSession,
	runtime *runtimeDungeonState,
	lifecycleToken uint64,
	room currentDungeonAltarRoomIdentity,
	retryVersion uint64,
	timerName string,
	_ time.Time,
) error {
	if session == nil || runtime == nil {
		return nil
	}
	session.dungeon.mu.Lock()
	defer session.dungeon.mu.Unlock()
	if session.dungeon.runtime != runtime || runtime.lifecycleToken != lifecycleToken || runtime.altar == nil ||
		runtime.altar.Room != room || runtime.altar.roomTransitionVersion != retryVersion ||
		runtime.altar.roomTransitionTimerName != timerName ||
		runtime.altar.progress != currentDungeonAltarProgressRoomComplete {
		return nil
	}
	runtime.altar.roomTransitionTimerName = ""
	return s.advanceCurrentDungeonEndlessAltarRoomLocked(session, runtime, "room_move_retry_timequeue")
}

func (s *Service) completeCurrentDungeonAltarLocked(
	session *gameSession,
	runtime *runtimeDungeonState,
	source string,
) error {
	if session == nil || runtime == nil || runtime.altar == nil || runtime.Session == nil ||
		session.dungeon.runtime != runtime || runtime.altar.progress != currentDungeonAltarProgressDungeonComplete {
		return errCurrentDungeonAltarDefinitionInvalid
	}
	s.cancelCurrentDungeonAltarTimersLocked(runtime)
	snapshot := runtime.Session.Snapshot()
	switch snapshot.Run.Status {
	case worldmap.DungeonRunActive:
		if err := runtime.Session.MarkCurrentRoomScriptedCleared(); err != nil {
			return err
		}
		if err := runtime.cacheCurrentDungeonRoom(); err != nil {
			return err
		}
		if err := runtime.Session.Complete(); err != nil {
			return err
		}
		snapshot = runtime.Session.Snapshot()
	case worldmap.DungeonRunCompleted:
		// A previous completion attempt may already have closed the run before a
		// transient PVF, repository, progression, or socket failure. Continue
		// from the altar/settlement stage flags without replaying the run.
	default:
		return errCurrentDungeonAltarDefinitionInvalid
	}

	if !currentDungeonRuntimeIsPractice(runtime) && !runtime.clearMapCompletionPhaseAPersisted {
		ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
		err := s.persistCurrentDungeonScriptedAltarClearMapCompletionPhaseA(ctx, session, runtime, snapshot)
		cancel()
		if err != nil {
			s.logGameEvent(session, "game-dungeon-altar-clear-map-phase-a-retry-scheduled",
				"dungeon_id", runtime.Dungeon.ID,
				"source", source,
				"error", err)
			return s.scheduleCurrentDungeonAltarCompletionRetryLocked(session, runtime, "clear_map_phase_a")
		}
	}
	if !currentDungeonRuntimeIsPractice(runtime) && runtime.clearMapCompletionPhaseAPersisted &&
		!runtime.clearMapCompletionNotificationClosed {
		ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
		err := s.sendCurrentDungeonClearMapCompletionNotificationLocked(ctx, session, runtime)
		cancel()
		if err != nil {
			s.logGameEvent(session, "game-dungeon-altar-clear-map-notification-retry-scheduled",
				"dungeon_id", runtime.Dungeon.ID,
				"source", source,
				"phase_a_persisted", true,
				"active_snapshot_sent", runtime.clearMapCompletionActiveSnapshotSent,
				"error", err)
			return s.scheduleCurrentDungeonAltarCompletionRetryLocked(session, runtime, "clear_map_notification")
		}
	}
	if err := s.startCurrentDungeonAltarSettlementLocked(session, runtime); err != nil {
		s.logGameEvent(session, "game-dungeon-altar-settlement-retry-scheduled",
			"dungeon_id", runtime.Dungeon.ID,
			"source", source,
			"phase_a_persisted", runtime.clearMapCompletionPhaseAPersisted,
			"error", err)
		return s.scheduleCurrentDungeonAltarCompletionRetryLocked(session, runtime, "settlement")
	}
	if runtime.altar.settlementRetryTimerName != "" && s.gameplayTimers != nil {
		s.gameplayTimers.Cancel(runtime.altar.settlementRetryTimerName)
		runtime.altar.settlementRetryTimerName = ""
	}
	s.logGameEvent(session, "game-dungeon-altar-progress-committed",
		"source", source,
		"dungeon_id", runtime.Dungeon.ID,
		"room", runtime.altar.Room.Key,
		"completed_rounds", runtime.altar.completedRounds,
		"max_rounds", runtime.altar.MaxRounds,
		"clear_map_phase_a_persisted", runtime.clearMapCompletionPhaseAPersisted,
		"dungeon_complete", true)
	return nil
}

func currentDungeonAltarCompletionNeedsRetry(runtime *runtimeDungeonState, characterID uint16) bool {
	if runtime == nil || runtime.altar == nil || runtime.altar.progress != currentDungeonAltarProgressDungeonComplete ||
		runtime.Session == nil || runtime.Session.Snapshot().Run.Status != worldmap.DungeonRunCompleted {
		return false
	}
	if !currentDungeonRuntimeIsPractice(runtime) &&
		(!runtime.clearMapCompletionPhaseAPersisted || !runtime.clearMapCompletionNotificationClosed) {
		return true
	}
	return !completedCurrentDungeonAltarSettlementExitReady(runtime, characterID)
}

func (s *Service) scheduleCurrentDungeonAltarDifficultyTimeoutLocked(
	session *gameSession,
	runtime *runtimeDungeonState,
) error {
	queue := s.ensureGameplayTimeQueue()
	if queue == nil || runtime == nil || runtime.altar == nil {
		return errGameplayTimeQueueUnavailable
	}
	altar := runtime.altar
	roomGeneration, promptVersion := altar.Room.Generation, altar.difficultyVersion
	timerName := fmt.Sprintf("dnf-dungeon-altar-difficulty:%s:run:%d:room:%d:prompt:%d", session.connID, runtime.lifecycleToken, roomGeneration, promptVersion)
	if altar.difficultyTimerName != "" {
		queue.Cancel(altar.difficultyTimerName)
	}
	altar.difficultyTimerName = timerName
	characterID, characterGeneration, err := gameSessionCharacterEventIdentity(session, session.selectedCharacterID)
	if err != nil {
		return err
	}
	return queue.ScheduleAfter(timerName, currentDungeonAltarDifficultyTimeoutSeconds*time.Second, func(due time.Time) {
		postErr := s.postGameSessionCharacterEvent(session, "dungeon-altar-difficulty-timequeue", characterID, characterGeneration, func() error {
			return s.fireCurrentDungeonAltarDifficultyTimeout(session, runtime, roomGeneration, promptVersion, due)
		})
		if postErr != nil && !isClosedGameSessionEventError(postErr) {
			s.logGameEvent(session, "game-dungeon-altar-difficulty-post-failed", "error", postErr)
		}
	})
}

func (s *Service) fireCurrentDungeonAltarDifficultyTimeout(
	session *gameSession,
	runtime *runtimeDungeonState,
	roomGeneration uint64,
	promptVersion uint64,
	_ time.Time,
) error {
	if session == nil || runtime == nil {
		return nil
	}
	session.dungeon.mu.Lock()
	defer session.dungeon.mu.Unlock()
	if session.dungeon.runtime != runtime || runtime.altar == nil {
		return nil
	}
	choice, err := s.chooseDungeonIndex(2)
	if err != nil {
		return err
	}
	difficulty := worldmap.BloodAltarDifficultyA
	if choice == 1 {
		difficulty = worldmap.BloodAltarDifficultyB
	}
	if !runtime.altar.resolveDifficulty(roomGeneration, promptVersion, difficulty) {
		return nil
	}
	runtime.altar.difficultyTimerName = ""
	body, err := buildCurrentDungeonAltarDifficultyResolvedBody(difficulty)
	if err != nil {
		return err
	}
	if err := s.sendGameUpperRawClass(session, currentDungeonAltarDifficultyUIMsgID, body, 0); err != nil {
		return err
	}
	schedule, err := runtime.altar.beginRound(s.gameplayNow())
	if err != nil {
		return err
	}
	return s.startCurrentDungeonAltarScheduleLocked(session, runtime, schedule, "difficulty_timeout")
}

func (s *Service) scheduleCurrentDungeonAltarFinalRoundLocked(
	session *gameSession,
	runtime *runtimeDungeonState,
) error {
	queue := s.ensureGameplayTimeQueue()
	if queue == nil || runtime == nil || runtime.altar == nil {
		return errGameplayTimeQueueUnavailable
	}
	altar := runtime.altar
	roomGeneration := altar.Room.Generation
	roundIndex := altar.nextRoundIndex
	timerName := fmt.Sprintf("dnf-dungeon-altar-final:%s:run:%d:room:%d:round:%d", session.connID, runtime.lifecycleToken, roomGeneration, roundIndex)
	if altar.finalTimerName != "" {
		queue.Cancel(altar.finalTimerName)
	}
	altar.finalTimerName = timerName
	characterID, characterGeneration, err := gameSessionCharacterEventIdentity(session, session.selectedCharacterID)
	if err != nil {
		return err
	}
	return queue.ScheduleAfter(timerName, currentDungeonAltarFinalRoundDelay, func(due time.Time) {
		postErr := s.postGameSessionCharacterEvent(session, "dungeon-altar-final-timequeue", characterID, characterGeneration, func() error {
			return s.fireCurrentDungeonAltarFinalRound(session, runtime, roomGeneration, roundIndex, due)
		})
		if postErr != nil && !isClosedGameSessionEventError(postErr) {
			s.logGameEvent(session, "game-dungeon-altar-final-post-failed", "error", postErr)
		}
	})
}

func (s *Service) fireCurrentDungeonAltarFinalRound(
	session *gameSession,
	runtime *runtimeDungeonState,
	roomGeneration uint64,
	roundIndex int,
	_ time.Time,
) error {
	if session == nil || runtime == nil {
		return nil
	}
	session.dungeon.mu.Lock()
	defer session.dungeon.mu.Unlock()
	if session.dungeon.runtime != runtime || runtime.altar == nil || runtime.altar.Room.Generation != roomGeneration ||
		runtime.altar.nextRoundIndex != roundIndex || runtime.altar.progress != currentDungeonAltarProgressFinalRoundDelay {
		return nil
	}
	runtime.altar.finalTimerName = ""
	runtime.altar.progress = currentDungeonAltarProgressRoundActive
	schedule, err := runtime.altar.beginRound(s.gameplayNow())
	if err != nil {
		return err
	}
	return s.startCurrentDungeonAltarScheduleLocked(session, runtime, schedule, "final_round_delay")
}

func (s *Service) armCurrentDungeonAltarQuestDropRetryLocked(
	session *gameSession,
	runtime *runtimeDungeonState,
	pending *currentDungeonAltarQuestDropRetry,
	reason string,
) error {
	if runtime == nil || runtime.altar == nil {
		return errCurrentDungeonAltarQuestDropInvalid
	}
	altar := runtime.altar
	if err := enqueueCurrentDungeonAltarQuestDropRetryLocked(altar, pending); err != nil {
		return err
	}
	if altar.questDropRetryTimerName != "" {
		return nil
	}
	return s.scheduleCurrentDungeonAltarQuestDropRetryLocked(
		session,
		runtime,
		altar.questDropRetry,
		reason,
	)
}

func (s *Service) scheduleCurrentDungeonAltarQuestDropRetryLocked(
	session *gameSession,
	runtime *runtimeDungeonState,
	pending *currentDungeonAltarQuestDropRetry,
	reason string,
) error {
	queue := s.ensureGameplayTimeQueue()
	if queue == nil || session == nil || runtime == nil || runtime.altar == nil || pending == nil ||
		session.dungeon.runtime != runtime || len(pending.Sequences) == 0 || len(pending.Deaths) == 0 ||
		pending.Room != runtime.altar.Room {
		return errGameplayTimeQueueUnavailable
	}
	altar := runtime.altar
	if altar.questDropRetry != pending {
		return errCurrentDungeonAltarQuestDropInvalid
	}
	if altar.questDropRetryTimerName != "" {
		queue.Cancel(altar.questDropRetryTimerName)
	}
	altar.questDropRetryVersion = nextCurrentDungeonAltarGeneration(altar.questDropRetryVersion)
	pending.Version = altar.questDropRetryVersion
	retryVersion := pending.Version
	room := pending.Room
	lifecycleToken := runtime.lifecycleToken
	timerName := fmt.Sprintf(
		"dnf-dungeon-altar-quest-drop-retry:%s:run:%d:room:%d:retry:%d",
		session.connID,
		lifecycleToken,
		room.Generation,
		retryVersion,
	)
	altar.questDropRetryTimerName = timerName
	characterID, characterGeneration, err := gameSessionCharacterEventIdentity(session, session.selectedCharacterID)
	if err != nil {
		altar.questDropRetryTimerName = ""
		return err
	}
	if err := queue.ScheduleAfter(timerName, currentDungeonAltarQuestDropRetryDelay, func(due time.Time) {
		postErr := s.postGameSessionCharacterEvent(
			session,
			"dungeon-altar-quest-drop-retry-timequeue",
			characterID,
			characterGeneration,
			func() error {
				return s.fireCurrentDungeonAltarQuestDropRetry(
					session,
					runtime,
					lifecycleToken,
					room,
					retryVersion,
					timerName,
					due,
				)
			},
		)
		if postErr != nil && !isClosedGameSessionEventError(postErr) {
			s.logGameEvent(session, "game-dungeon-altar-quest-drop-retry-post-failed", "error", postErr)
		}
	}); err != nil {
		altar.questDropRetryTimerName = ""
		return err
	}
	s.logGameEvent(session, "game-dungeon-altar-quest-drop-retry-armed",
		"dungeon_id", runtime.Dungeon.ID,
		"room_generation", room.Generation,
		"retry_version", retryVersion,
		"sequence_count", len(pending.Deaths),
		"delay", currentDungeonAltarQuestDropRetryDelay,
		"reason", reason)
	return nil
}

func (s *Service) fireCurrentDungeonAltarQuestDropRetry(
	session *gameSession,
	runtime *runtimeDungeonState,
	lifecycleToken uint64,
	room currentDungeonAltarRoomIdentity,
	retryVersion uint64,
	timerName string,
	_ time.Time,
) error {
	if session == nil || runtime == nil {
		return nil
	}
	session.dungeon.mu.Lock()
	defer session.dungeon.mu.Unlock()
	if session.dungeon.runtime != runtime || runtime.lifecycleToken != lifecycleToken || runtime.altar == nil {
		return nil
	}
	altar := runtime.altar
	pending := altar.questDropRetry
	if pending == nil || pending.Room != room || altar.Room != room || pending.Version != retryVersion ||
		altar.questDropRetryVersion != retryVersion || altar.questDropRetryTimerName != timerName {
		return nil
	}
	altar.questDropRetryTimerName = ""
	return s.drainCurrentDungeonAltarDeathRetriesLocked(session, runtime, room)
}

func (s *Service) drainCurrentDungeonAltarDeathRetriesLocked(
	session *gameSession,
	runtime *runtimeDungeonState,
	room currentDungeonAltarRoomIdentity,
) error {
	if session == nil || runtime == nil || runtime.altar == nil || runtime.altar.Room != room ||
		session.dungeon.runtime != runtime {
		return nil
	}
	altar := runtime.altar
	for altar.questDropRetry != nil {
		pending := altar.questDropRetry
		if pending.Room != room {
			return errCurrentDungeonAltarQuestDropInvalid
		}
		if err := s.commitCurrentDungeonAltarQuestDropsLocked(session, runtime, pending.Deaths); err != nil {
			s.logGameEvent(session, "game-dungeon-altar-quest-drop-retry-failed",
				"dungeon_id", runtime.Dungeon.ID,
				"room_generation", room.Generation,
				"retry_version", pending.Version,
				"sequence_count", len(pending.Deaths),
				"pending_batch_count", 1+len(altar.questDropRetryQueue),
				"error", err)
			return s.scheduleCurrentDungeonAltarQuestDropRetryLocked(
				session,
				runtime,
				pending,
				"quest_monster_drop_retry_failed",
			)
		}
		if pending.DynamicDeathNotification {
			if len(pending.Sequences) != 1 {
				return errCurrentDungeonAltarRequestMalformed
			}
			if err := s.sendCurrentDungeonAltarDynamicDeathNotificationLocked(session, pending.Sequences[0]); err != nil {
				s.logGameEvent(session, "game-dungeon-altar-death-notification-retry-failed",
					"dungeon_id", runtime.Dungeon.ID,
					"room_generation", room.Generation,
					"retry_version", pending.Version,
					"sequence", pending.Sequences[0],
					"pending_batch_count", 1+len(altar.questDropRetryQueue),
					"error", err)
				return s.scheduleCurrentDungeonAltarQuestDropRetryLocked(
					session,
					runtime,
					pending,
					"dynamic_monster_op38_retry_failed",
				)
			}
		}
		result, err := altar.applyDeaths(room.Key, pending.Sequences)
		if err != nil {
			return err
		}
		if result.Progress != currentDungeonAltarProgressNone {
			altar.questDropRetryProgress = result.Progress
		}
		s.logGameEvent(session, "game-dungeon-altar-death-accepted",
			"dungeon_id", runtime.Dungeon.ID,
			"room", room.Key,
			"accepted", result.Accepted,
			"duplicate", result.Duplicate,
			"active_remaining", len(altar.activeSequences),
			"progress", result.Progress,
			"pending_batch_count", 1+len(altar.questDropRetryQueue),
			"source", "quest_drop_retry_timequeue")
		if len(altar.questDropRetryQueue) == 0 {
			altar.questDropRetry = nil
			altar.questDropRetryQueue = nil
		} else {
			altar.questDropRetry = altar.questDropRetryQueue[0]
			altar.questDropRetryQueue = altar.questDropRetryQueue[1:]
		}
	}
	progress := altar.questDropRetryProgress
	altar.questDropRetryProgress = currentDungeonAltarProgressNone
	return s.processCurrentDungeonAltarProgressLocked(
		session,
		runtime,
		progress,
		"quest_drop_retry_timequeue",
	)
}

func (s *Service) scheduleCurrentDungeonAltarCompletionRetryLocked(
	session *gameSession,
	runtime *runtimeDungeonState,
	reason string,
) error {
	queue := s.ensureGameplayTimeQueue()
	if queue == nil || session == nil || runtime == nil || runtime.altar == nil || runtime.Session == nil ||
		session.dungeon.runtime != runtime || runtime.altar.progress != currentDungeonAltarProgressDungeonComplete {
		return errGameplayTimeQueueUnavailable
	}
	altar := runtime.altar
	if altar.settlementRetryTimerName != "" {
		queue.Cancel(altar.settlementRetryTimerName)
	}
	altar.settlementRetryVersion = nextCurrentDungeonAltarGeneration(altar.settlementRetryVersion)
	retryVersion := altar.settlementRetryVersion
	roomGeneration := altar.Room.Generation
	lifecycleToken := runtime.lifecycleToken
	timerName := fmt.Sprintf(
		"dnf-dungeon-altar-completion-retry:%s:run:%d:room:%d:retry:%d",
		session.connID,
		lifecycleToken,
		roomGeneration,
		retryVersion,
	)
	altar.settlementRetryTimerName = timerName
	characterID, characterGeneration, err := gameSessionCharacterEventIdentity(session, session.selectedCharacterID)
	if err != nil {
		altar.settlementRetryTimerName = ""
		return err
	}
	if err := queue.ScheduleAfter(timerName, currentDungeonAltarCompletionRetryDelay, func(due time.Time) {
		postErr := s.postGameSessionCharacterEvent(
			session,
			"dungeon-altar-completion-retry-timequeue",
			characterID,
			characterGeneration,
			func() error {
				return s.fireCurrentDungeonAltarCompletionRetry(
					session,
					runtime,
					lifecycleToken,
					roomGeneration,
					retryVersion,
					timerName,
					due,
				)
			},
		)
		if postErr != nil && !isClosedGameSessionEventError(postErr) {
			s.logGameEvent(session, "game-dungeon-altar-completion-retry-post-failed", "error", postErr)
		}
	}); err != nil {
		altar.settlementRetryTimerName = ""
		return err
	}
	s.logGameEvent(session, "game-dungeon-altar-completion-retry-armed",
		"dungeon_id", runtime.Dungeon.ID,
		"room_generation", roomGeneration,
		"retry_version", retryVersion,
		"delay", currentDungeonAltarCompletionRetryDelay,
		"reason", reason)
	return nil
}

func (s *Service) fireCurrentDungeonAltarCompletionRetry(
	session *gameSession,
	runtime *runtimeDungeonState,
	lifecycleToken uint64,
	roomGeneration uint64,
	retryVersion uint64,
	timerName string,
	_ time.Time,
) error {
	if session == nil || runtime == nil {
		return nil
	}
	session.dungeon.mu.Lock()
	defer session.dungeon.mu.Unlock()
	if session.dungeon.runtime != runtime || runtime.lifecycleToken != lifecycleToken || runtime.altar == nil ||
		runtime.altar.Room.Generation != roomGeneration || runtime.altar.settlementRetryVersion != retryVersion ||
		runtime.altar.settlementRetryTimerName != timerName || runtime.altar.progress != currentDungeonAltarProgressDungeonComplete {
		return nil
	}
	runtime.altar.settlementRetryTimerName = ""
	return s.completeCurrentDungeonAltarLocked(session, runtime, "completion_retry_timequeue")
}

func (s *Service) cancelCurrentDungeonAltarTimersLocked(runtime *runtimeDungeonState) {
	if runtime == nil || runtime.altar == nil {
		return
	}
	if s != nil && s.gameplayTimers != nil {
		for _, name := range runtime.altar.timerNames() {
			if name != "" {
				s.gameplayTimers.Cancel(name)
			}
		}
	}
	runtime.altar.waveTimerName = ""
	runtime.altar.difficultyTimerName = ""
	runtime.altar.finalTimerName = ""
	runtime.altar.questDropRetryTimerName = ""
	runtime.altar.questDropRetry = nil
	runtime.altar.questDropRetryQueue = nil
	runtime.altar.questDropRetryProgress = currentDungeonAltarProgressNone
	runtime.altar.roomTransitionTimerName = ""
	runtime.altar.settlementRetryTimerName = ""
	runtime.altar.expiryTimerNames = make(map[string]struct{})
}

func (s *Service) cancelCurrentDungeonAltarWaveExpiryLocked(runtime *runtimeDungeonState, timerName string) {
	if runtime == nil || runtime.altar == nil || timerName == "" {
		return
	}
	if s != nil && s.gameplayTimers != nil {
		s.gameplayTimers.Cancel(timerName)
	}
	delete(runtime.altar.expiryTimerNames, timerName)
}

func (s *Service) cancelCurrentDungeonAltarExpiryTimersLocked(runtime *runtimeDungeonState) {
	if runtime == nil || runtime.altar == nil {
		return
	}
	if s != nil && s.gameplayTimers != nil {
		for timerName := range runtime.altar.expiryTimerNames {
			if timerName != "" {
				s.gameplayTimers.Cancel(timerName)
			}
		}
	}
	runtime.altar.expiryTimerNames = make(map[string]struct{})
}
