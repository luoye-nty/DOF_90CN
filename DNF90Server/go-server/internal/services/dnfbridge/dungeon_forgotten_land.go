package dnfbridge

import (
	"context"
	"encoding/binary"
	"fmt"
	"strings"
	"time"

	"longheng.io/server/internal/modules/dnf/worldmap"
)

// The current EXE registers opcode 0x025b dynamically for Forgotten Land.
// sub_1D39CE0 consumes exactly two u32 values and stores them in the runtime
// fields read by sub_17EBEB0 before its countdown UI is initialized. The same
// numeric opcode has a different meaning in other client modules, so keep this
// use local to the Lost Land entry path.
const currentForgottenLandTimeInfoMsgID uint16 = 0x025b

func isCurrentForgottenLandDungeon(runtime *runtimeDungeonState) bool {
	return runtime != nil && strings.EqualFold(strings.TrimSpace(runtime.Dungeon.Metadata.DungeonType), "lost land")
}

func currentForgottenLandTimeLimits(runtime *runtimeDungeonState) (uint32, uint32, time.Duration, error) {
	if !isCurrentForgottenLandDungeon(runtime) {
		return 0, 0, 0, nil
	}
	limits := runtime.Dungeon.Metadata.LimitTime
	const maxUint32 = int64(^uint32(0))
	if !limits.Set || limits.PrimarySeconds <= 0 || limits.SecondarySeconds <= 0 ||
		limits.PrimarySeconds > maxUint32 || limits.SecondarySeconds > maxUint32 {
		return 0, 0, 0, fmt.Errorf(
			"forgotten land PVF limit time is missing or invalid: set=%t primary=%d secondary=%d",
			limits.Set,
			limits.PrimarySeconds,
			limits.SecondarySeconds,
		)
	}
	primary := uint32(limits.PrimarySeconds)
	secondary := uint32(limits.SecondarySeconds)
	return primary, secondary, time.Duration(primary) * time.Second, nil
}

func buildCurrentForgottenLandTimeInfoBody(primarySeconds, secondarySeconds uint32) []byte {
	body := make([]byte, 8)
	binary.LittleEndian.PutUint32(body[0:4], primarySeconds)
	binary.LittleEndian.PutUint32(body[4:8], secondarySeconds)
	return body
}

func (s *Service) sendCurrentForgottenLandTimeProjection(
	session *gameSession,
	runtime *runtimeDungeonState,
) error {
	primary, secondary, _, err := currentForgottenLandTimeLimits(runtime)
	if err != nil {
		return err
	}
	if !isCurrentForgottenLandDungeon(runtime) {
		return nil
	}
	body := buildCurrentForgottenLandTimeInfoBody(primary, secondary)
	s.logGameEvent(session, "game-forgotten-land-time-op603-send",
		"char_id", session.selectedCharacterID,
		"dungeon_id", runtime.Dungeon.ID,
		"msg_id", currentForgottenLandTimeInfoMsgID,
		"classification", 0,
		"primary_seconds", primary,
		"secondary_seconds", secondary,
		"body_len", len(body),
		"body_source", "current_exe_sub_1D39CE0_and_pvf_limit_time")
	return s.sendGameUpperRawClass(session, currentForgottenLandTimeInfoMsgID, body, 0)
}

// armCurrentForgottenLandTimer freezes the town return before entering the
// queue. The deadline belongs to the whole dungeon runtime and is intentionally
// idempotent, so room/warp transitions can never reset it.
func (s *Service) armCurrentForgottenLandTimer(
	session *gameSession,
	runtime *runtimeDungeonState,
) error {
	_, _, delay, err := currentForgottenLandTimeLimits(runtime)
	if err != nil || !isCurrentForgottenLandDungeon(runtime) {
		return err
	}
	if s == nil || session == nil || runtime == nil {
		return fmt.Errorf("forgotten land timer runtime owner is unavailable")
	}
	queue := s.ensureGameplayTimeQueue()
	if queue == nil {
		return errGameplayTimeQueueUnavailable
	}
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	transition, err := s.prepareCurrentDungeonTownTransitionForSession(ctx, session, session.selectedCharacterID)
	cancel()
	if err != nil {
		return fmt.Errorf("prepare forgotten land timeout return: %w", err)
	}

	session.dungeon.mu.Lock()
	defer session.dungeon.mu.Unlock()
	return s.armCurrentForgottenLandTimerLocked(session, runtime, transition, delay, queue)
}

func (s *Service) armCurrentForgottenLandTimerLocked(
	session *gameSession,
	runtime *runtimeDungeonState,
	transition currentDungeonTownTransition,
	delay time.Duration,
	queue gameplayTimeQueue,
) error {
	if session.dungeon.runtime != runtime || runtime.lifecycleToken == 0 || queue == nil {
		return errGameplayTimeQueueUnavailable
	}
	if runtime.forgottenLandTimerActive {
		return nil
	}
	runtime.forgottenLandTimerGeneration = nextCurrentForgottenLandTimerGeneration(runtime.forgottenLandTimerGeneration)
	generation := runtime.forgottenLandTimerGeneration
	timerName := fmt.Sprintf("dnf-forgotten-land:%s:run:%d", session.connID, runtime.lifecycleToken)
	runtime.forgottenLandTimerActive = true
	runtime.forgottenLandTimerName = timerName
	runtime.forgottenLandDeadline = queue.Now().Add(delay)
	runtime.forgottenLandTownTransition = cloneCurrentDungeonTownTransition(transition)
	characterID, characterGeneration, err := gameSessionCharacterEventIdentity(session, session.selectedCharacterID)
	if err != nil {
		s.cancelCurrentForgottenLandTimerLocked(session, runtime, "timer_character_identity_changed")
		return err
	}
	dungeonID := runtime.Dungeon.ID
	runToken := runtime.lifecycleToken
	if err := queue.ScheduleAfter(timerName, delay, func(due time.Time) {
		postErr := s.postGameSessionCharacterEvent(
			session,
			"forgotten-land-timequeue",
			characterID,
			characterGeneration,
			func() error {
				s.fireCurrentForgottenLandTimer(session, runtime, generation, due)
				return nil
			},
		)
		if postErr != nil && !isClosedGameSessionEventError(postErr) {
			s.logPacketEvent("game-session-event-submit-failed",
				"conn_id", session.connID,
				"source", "forgotten-land-timequeue",
				"char_id", characterID,
				"character_generation", characterGeneration,
				"dungeon_id", dungeonID,
				"run_token", runToken,
				"error", postErr)
		}
	}); err != nil {
		s.cancelCurrentForgottenLandTimerLocked(session, runtime, "timer_schedule_failed")
		return err
	}
	s.logGameEvent(session, "game-forgotten-land-timer-scheduled",
		"char_id", characterID,
		"dungeon_id", dungeonID,
		"run_token", runToken,
		"timer_generation", generation,
		"timer_name", timerName,
		"due_at_utc", runtime.forgottenLandDeadline.UTC().Format(time.RFC3339Nano),
		"delay_seconds", int64(delay/time.Second),
		"source", "gameplay_time_queue_one_shot")
	return nil
}

func (s *Service) fireCurrentForgottenLandTimer(
	session *gameSession,
	runtime *runtimeDungeonState,
	generation uint64,
	due time.Time,
) {
	if s == nil || session == nil || runtime == nil {
		return
	}
	session.dungeon.mu.Lock()
	if session.dungeon.runtime != runtime || !runtime.forgottenLandTimerActive ||
		runtime.forgottenLandTimerGeneration != generation || runtime.lifecycleToken == 0 {
		session.dungeon.mu.Unlock()
		return
	}
	if runtime.Session == nil || runtime.Session.Snapshot().Run.Status != worldmap.DungeonRunActive {
		s.cancelCurrentForgottenLandTimerLocked(session, runtime, "timer_fired_after_runtime_state_changed")
		session.dungeon.mu.Unlock()
		return
	}
	s.logGameEvent(session, "game-forgotten-land-timer-fired",
		"char_id", runtime.Character.CharacterID,
		"dungeon_id", runtime.Dungeon.ID,
		"run_token", runtime.lifecycleToken,
		"timer_generation", generation,
		"scheduled_due_utc", due.UTC().Format(time.RFC3339Nano))
	transition := cloneCurrentDungeonTownTransition(runtime.forgottenLandTownTransition)
	// This one-shot has now fired. Consume its queue ownership before asking
	// the current run to transition; an unconfirmed user-initiated return keeps
	// a not-yet-fired timer, while an expired timer must not remain active.
	s.cancelCurrentForgottenLandTimerLocked(session, runtime, "timer_fired")
	session.dungeon.mu.Unlock()
	if err := s.sendCurrentActiveDungeonReturnToTown(
		session,
		runtime,
		transition,
		currentForgottenLandTimeInfoMsgID,
		"current_exe_forgotten_land_pvf_limit_time_expired",
	); err != nil {
		s.logGameEvent(session, "game-forgotten-land-timeout-return-failed", "error", err)
	}
}

// cancelCurrentForgottenLandTimerLocked invalidates an already-dequeued
// callback before removing the named queue item. The caller holds dungeon.mu.
func (s *Service) cancelCurrentForgottenLandTimerLocked(
	session *gameSession,
	runtime *runtimeDungeonState,
	source string,
) {
	if runtime == nil {
		return
	}
	wasActive := runtime.forgottenLandTimerActive
	timerName := runtime.forgottenLandTimerName
	previousGeneration := runtime.forgottenLandTimerGeneration
	runtime.forgottenLandTimerGeneration = nextCurrentForgottenLandTimerGeneration(previousGeneration)
	runtime.forgottenLandTimerActive = false
	runtime.forgottenLandTimerName = ""
	runtime.forgottenLandDeadline = time.Time{}
	runtime.forgottenLandTownTransition = currentDungeonTownTransition{}
	cancelled := false
	if timerName != "" && s != nil && s.gameplayTimers != nil {
		cancelled = s.gameplayTimers.Cancel(timerName)
	}
	if wasActive || timerName != "" {
		s.logGameEvent(session, "game-forgotten-land-timer-cancelled",
			"char_id", runtime.Character.CharacterID,
			"dungeon_id", runtime.Dungeon.ID,
			"run_token", runtime.lifecycleToken,
			"previous_generation", previousGeneration,
			"current_generation", runtime.forgottenLandTimerGeneration,
			"timer_name", timerName,
			"queue_item_cancelled", cancelled,
			"source", source)
	}
}

func nextCurrentForgottenLandTimerGeneration(current uint64) uint64 {
	next := current + 1
	if next == 0 {
		next++
	}
	return next
}
