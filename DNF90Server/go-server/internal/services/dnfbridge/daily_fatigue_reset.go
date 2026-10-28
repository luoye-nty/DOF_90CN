package dnfbridge

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"time"

	dnffatigue "longheng.io/server/internal/modules/dnf/fatigue"
	"longheng.io/server/internal/modules/dnf/premium"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

const currentDailyFatigueResetRetryDelay = time.Minute

var currentDailyFatigueResetLocation = time.FixedZone("DNF-Beijing", 8*60*60)

type currentDailyFatigueResetClock struct {
	mu                  sync.Mutex
	generation          uint64
	timerName           string
	dueAt               time.Time
	characterID         uint16
	characterGeneration uint64
}

func (s *Service) reconcileCurrentDailyFatigueSelection(
	ctx context.Context,
	session *gameSession,
) (dnffatigue.ReconcileDailyResult, error) {
	return s.reconcileCurrentDailyFatigueSelectionAt(ctx, session, s.gameplayNow().UTC())
}

func (s *Service) reconcileCurrentDailyFatigueSelectionAt(
	ctx context.Context,
	session *gameSession,
	now time.Time,
) (dnffatigue.ReconcileDailyResult, error) {
	if s == nil || session == nil || session.selectedCharacterID == 0 {
		return dnffatigue.ReconcileDailyResult{}, nil
	}
	repositories, ok := s.repositoryGroup()
	if !ok {
		return dnffatigue.ReconcileDailyResult{}, nil
	}
	owner, err := dnffatigue.NewOwner(repositories)
	if err != nil {
		return dnffatigue.ReconcileDailyResult{}, err
	}
	result, err := owner.ReconcileDaily(ctx, dnffatigue.ReconcileDailyCommand{
		AccountID:   s.accountIDForSession(session),
		CharacterID: strconv.FormatUint(uint64(session.selectedCharacterID), 10),
		UpdatedAt:   now.UTC(),
	})
	if err != nil {
		return dnffatigue.ReconcileDailyResult{}, fmt.Errorf("reconcile daily fatigue selection: %w", err)
	}
	return result, nil
}

func (s *Service) reconcileCurrentDailyFatigueSelectionIfNeeded(
	ctx context.Context,
	session *gameSession,
	character dnfrepo.CharacterRecord,
) (dnfrepo.CharacterRecord, bool, error) {
	if character.Stats == nil {
		return character, false, nil
	}
	_, recoveryInitialized := character.Stats[dnffatigue.DailyRecoveryDayStatKey]
	_, potionInitialized := character.Stats[dnffatigue.PotionUsageDayStatKey]
	if !recoveryInitialized && !potionInitialized {
		return character, false, nil
	}
	dayID := premium.ServiceDayID(s.gameplayNow())
	if character.Stats[dnffatigue.DailyRecoveryDayStatKey] == dayID &&
		character.Stats[dnffatigue.PotionUsageDayStatKey] == dayID {
		return character, false, nil
	}
	result, err := s.reconcileCurrentDailyFatigueSelection(ctx, session)
	if err != nil || result.Character.CharacterID == "" {
		return character, false, err
	}
	character.Stats = result.Character.Stats
	character.UpdatedAt = result.Character.UpdatedAt
	return character, result.Changed, nil
}

func nextCurrentDailyFatigueResetBoundary(now time.Time) time.Time {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	local := now.In(currentDailyFatigueResetLocation)
	boundary := time.Date(local.Year(), local.Month(), local.Day(), 6, 0, 0, 0, currentDailyFatigueResetLocation)
	if !local.Before(boundary) {
		boundary = boundary.AddDate(0, 0, 1)
	}
	return boundary.UTC()
}

func (s *Service) armCurrentDailyFatigueReset(session *gameSession) error {
	queue := s.ensureGameplayTimeQueue()
	if queue == nil {
		return errGameplayTimeQueueUnavailable
	}
	return s.scheduleCurrentDailyFatigueResetAt(
		session,
		nextCurrentDailyFatigueResetBoundary(queue.Now()),
		"next_beijing_0600",
	)
}

func (s *Service) retryCurrentDailyFatigueReset(session *gameSession) error {
	queue := s.ensureGameplayTimeQueue()
	if queue == nil {
		return errGameplayTimeQueueUnavailable
	}
	return s.scheduleCurrentDailyFatigueResetAt(
		session,
		queue.Now().Add(currentDailyFatigueResetRetryDelay),
		"repository_retry",
	)
}

func (s *Service) scheduleCurrentDailyFatigueResetAt(
	session *gameSession,
	dueAt time.Time,
	source string,
) error {
	if s == nil || session == nil || session.selectedCharacterID == 0 {
		return nil
	}
	queue := s.ensureGameplayTimeQueue()
	if queue == nil {
		return errGameplayTimeQueueUnavailable
	}
	characterID, characterGeneration, err := gameSessionCharacterEventIdentity(session, session.selectedCharacterID)
	if err != nil {
		return err
	}
	now := queue.Now().UTC()
	dueAt = dueAt.UTC()
	if dueAt.Before(now) {
		dueAt = now
	}
	delay := dueAt.Sub(now)

	clock := &session.dailyFatigueReset
	clock.mu.Lock()
	oldTimerName := clock.timerName
	clock.generation++
	if clock.generation == 0 {
		clock.generation++
	}
	timerGeneration := clock.generation
	timerName := fmt.Sprintf(
		"dnf-daily-fatigue:%s:char:%d:session:%d:timer:%d",
		session.connID,
		characterID,
		characterGeneration,
		timerGeneration,
	)
	clock.timerName = timerName
	clock.dueAt = dueAt
	clock.characterID = characterID
	clock.characterGeneration = characterGeneration
	clock.mu.Unlock()
	if oldTimerName != "" {
		queue.Cancel(oldTimerName)
	}

	err = queue.ScheduleAfter(timerName, delay, func(callbackTime time.Time) {
		if callbackTime.IsZero() || callbackTime.Before(dueAt) {
			callbackTime = dueAt
		}
		postErr := s.postGameSessionCharacterEvent(
			session,
			"daily-fatigue-reset-timequeue",
			characterID,
			characterGeneration,
			func() error {
				s.runCurrentDailyFatigueReset(session, timerGeneration, callbackTime.UTC())
				return nil
			},
		)
		if postErr != nil && !isClosedGameSessionEventError(postErr) {
			s.logPacketEvent("game-session-event-submit-failed",
				"conn_id", session.connID,
				"source", "daily-fatigue-reset-timequeue",
				"char_id", characterID,
				"character_generation", characterGeneration,
				"error", postErr)
		}
	})
	if err != nil {
		clock.mu.Lock()
		if clock.generation == timerGeneration {
			clock.timerName = ""
			clock.dueAt = time.Time{}
			clock.characterID = 0
			clock.characterGeneration = 0
		}
		clock.mu.Unlock()
		return err
	}
	s.logGameEvent(session, "game-daily-fatigue-reset-timer-scheduled",
		"char_id", characterID,
		"character_generation", characterGeneration,
		"timer_name", timerName,
		"due_at_utc", dueAt.Format(time.RFC3339Nano),
		"source", source)
	return nil
}

func (s *Service) runCurrentDailyFatigueReset(
	session *gameSession,
	timerGeneration uint64,
	now time.Time,
) {
	if s == nil || session == nil {
		return
	}
	clock := &session.dailyFatigueReset
	clock.mu.Lock()
	if clock.generation != timerGeneration ||
		clock.characterID == 0 ||
		clock.characterID != session.selectedCharacterID ||
		clock.characterGeneration != session.characterGeneration {
		clock.mu.Unlock()
		return
	}
	clock.timerName = ""
	clock.dueAt = time.Time{}
	clock.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	_, _, selected, found := s.characterForEnter(ctx, session, session.selectedCharacterID)
	if !found {
		cancel()
		s.deferCurrentDailyFatigueReset(session, "selected_character_not_found", nil)
		return
	}
	if selected.Stats != nil {
		limit := selected.Stats["fatigue_limit"]
		if limit == dnffatigue.DefaultLimit || limit == dnffatigue.BlackDiamondLimit {
			if _, err := s.reconcileCurrentBlackDiamondSelection(ctx, session, selected); err != nil {
				cancel()
				s.deferCurrentDailyFatigueReset(session, "black_diamond_reconcile_failed", err)
				return
			}
		}
	}
	result, err := s.reconcileCurrentDailyFatigueSelectionAt(ctx, session, now)
	if err != nil || result.Character.CharacterID == "" {
		cancel()
		s.deferCurrentDailyFatigueReset(session, "daily_reconcile_failed", err)
		return
	}
	dailyDungeonResult, dailyDungeonErr := s.reconcileCurrentDailyDungeonSelectionAt(ctx, session, now)
	cancel()
	if dailyDungeonErr != nil {
		s.deferCurrentDailyFatigueReset(session, "daily_dungeon_reconcile_failed", dailyDungeonErr)
		return
	}
	if result.Changed {
		if err := s.sendGameUpperRawClass(
			session,
			currentFatigueMsgID,
			buildCurrentFatigueBody(result.Character, true),
			0,
		); err != nil {
			s.logGameEvent(session, "game-daily-fatigue-reset-projection-deferred",
				"char_id", session.selectedCharacterID,
				"service_day_id", result.ServiceDayID,
				"error", err)
		}
	}
	if refreshes := currentDailyDungeonInvitationRefreshes(dailyDungeonResult); len(refreshes) != 0 {
		if err := s.sendSelectedIncrementalItemSlotRefreshes(session, "daily_dungeon_invitation_grant", refreshes); err != nil {
			s.logGameEvent(session, "game-daily-dungeon-invitation-projection-deferred",
				"char_id", session.selectedCharacterID,
				"service_window_start", dailyDungeonResult.Window.Start.Format(time.RFC3339),
				"refresh_count", len(refreshes),
				"error", err)
		}
	}
	if !dailyDungeonResult.Window.Start.IsZero() {
		if err := s.sendCurrentRaidDailyAvailability(session, "beijing_0600_daily_reset"); err != nil {
			s.logGameEvent(session, "game-daily-dungeon-count-projection-deferred",
				"char_id", session.selectedCharacterID,
				"service_window_start", dailyDungeonResult.Window.Start.Format(time.RFC3339),
				"error", err)
		}
		// The same service-window boundary rolls the account AND character weekly
		// admission counters back to zero (dungeonadmission resets the weekly
		// policies on this window), and the client keeps whatever the last op1206
		// record wrote. Re-push it, or a player who is already online at 06:00
		// keeps reading last week's number -- the mirror image of the bug this
		// packet fixes, and just as misleading.
		if err := s.sendCurrentRaidWeeklySchedules(session, "beijing_0600_daily_reset"); err != nil {
			s.logGameEvent(session, "game-raid-weekly-schedule-projection-deferred",
				"char_id", session.selectedCharacterID,
				"service_window_start", dailyDungeonResult.Window.Start.Format(time.RFC3339),
				"error", err)
		}
	}
	s.logGameEvent(session, "game-daily-fatigue-reset-timequeue-reconciled",
		"char_id", session.selectedCharacterID,
		"service_day_id", result.ServiceDayID,
		"changed", result.Changed,
		"fatigue_restored", result.FatigueRestored,
		"potion_usage_reset", result.PotionUsageReset,
		"fatigue", result.Character.Stats["fatigue"],
		"fatigue_limit", result.Character.Stats["fatigue_limit"],
		"daily_dungeon_counters_changed", dailyDungeonResult.CountersChanged,
		"daily_dungeon_reset_policy_count", len(dailyDungeonResult.ResetPolicyKeys),
		"daily_invitation_inventory_changed", dailyDungeonResult.InventoryChanged)
	if err := s.armCurrentDailyFatigueReset(session); err != nil {
		s.logGameEvent(session, "game-daily-fatigue-reset-timer-rearm-deferred",
			"char_id", session.selectedCharacterID,
			"error", err)
	}
}

func (s *Service) deferCurrentDailyFatigueReset(session *gameSession, reason string, reconcileErr error) {
	s.logGameEvent(session, "game-daily-fatigue-reset-timequeue-deferred",
		"char_id", session.selectedCharacterID,
		"reason", reason,
		"error", reconcileErr)
	if err := s.retryCurrentDailyFatigueReset(session); err != nil {
		s.logGameEvent(session, "game-daily-fatigue-reset-retry-schedule-failed",
			"char_id", session.selectedCharacterID,
			"reason", reason,
			"error", err)
	}
}

func (s *Service) stopCurrentDailyFatigueReset(session *gameSession, source string) {
	if s == nil || session == nil {
		return
	}
	clock := &session.dailyFatigueReset
	clock.mu.Lock()
	timerName := clock.timerName
	characterID := clock.characterID
	clock.generation++
	if clock.generation == 0 {
		clock.generation++
	}
	clock.timerName = ""
	clock.dueAt = time.Time{}
	clock.characterID = 0
	clock.characterGeneration = 0
	clock.mu.Unlock()
	if timerName != "" {
		if queue := s.ensureGameplayTimeQueue(); queue != nil {
			queue.Cancel(timerName)
		}
		s.logGameEvent(session, "game-daily-fatigue-reset-timer-stopped",
			"char_id", characterID,
			"timer_name", timerName,
			"source", source)
	}
}
