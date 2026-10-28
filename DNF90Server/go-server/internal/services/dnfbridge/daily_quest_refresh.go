package dnfbridge

import (
	"context"
	"strconv"
	"time"
)

const (
	// currentDailyQuestResetHour/currentDailyQuestResetMinute are the 06:00
	// Beijing boundary premium.ServiceDayID already rolls fatigue, daily-dungeon
	// and daily-quest state on. nextDaily resolves the moment in the queue's own
	// location, which defaults to a fixed UTC+8, so the timer and the service day
	// cannot drift apart.
	currentDailyQuestResetHour   = 6
	currentDailyQuestResetMinute = 0

	currentDailyQuestResetTimerName = "dnf-daily-quest-reset"
	currentDailyQuestResetTimerKind = "daily_quest_reset"
)

// registerDailyQuestResetTimer arms the once-a-service-day notification that
// tells online clients a daily boundary passed.
//
// It deliberately owns no state. The reset itself stays authoritative in the
// read path - ReconcileDailyReset runs from the selected-character read and
// again per character in the callback below - because this deployment is started
// and stopped by hand, so a timer that only fires while the process is alive
// silently skips every boundary the server slept through. The timer exists for
// the one thing a read cannot do: an idle client keeps the list it was last
// sent, so a player online across 06:00 would not see yesterday's dailies return
// until they relogged.
func (s *Service) registerDailyQuestResetTimer() {
	if s == nil {
		return
	}
	queue := s.ensureGameplayTimeQueue()
	if queue == nil {
		return
	}
	if err := queue.RegisterDaily(
		currentDailyQuestResetTimerName,
		currentDailyQuestResetHour,
		currentDailyQuestResetMinute,
		func(time.Time) { s.refreshOnlineQuestListsAtDailyReset() },
	); err != nil {
		s.logWarn("dnfbridge daily quest reset timer registration failed",
			"timer_name", currentDailyQuestResetTimerName,
			"hour", currentDailyQuestResetHour,
			"minute", currentDailyQuestResetMinute,
			"error", err)
		return
	}
	s.logPacketEvent("dnf-daily-quest-reset-timer-registered",
		"timer_name", currentDailyQuestResetTimerName,
		"hour", currentDailyQuestResetHour,
		"minute", currentDailyQuestResetMinute,
		"reason", "online clients keep the pre-reset task list until it is republished")
}

// refreshOnlineQuestListsAtDailyReset republishes every online character's task
// projection once the 06:00 boundary has passed.
func (s *Service) refreshOnlineQuestListsAtDailyReset() {
	if s == nil {
		return
	}
	sessions := s.onlineQuestRefreshSessions()
	if len(sessions) == 0 {
		return
	}
	s.logPacketEvent("dnf-daily-quest-reset-refresh-started",
		"sessions", len(sessions),
		"timer_name", currentDailyQuestResetTimerName)
	for _, session := range sessions {
		s.refreshSessionQuestListAtDailyReset(session)
	}
}

// onlineQuestRefreshSessions snapshots the sessions that currently have a
// selected character. The snapshot is taken under s.mu and every packet is sent
// after it is released, so a slow or stalled client can never hold the service
// lock.
func (s *Service) onlineQuestRefreshSessions() []*gameSession {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	sessions := make([]*gameSession, 0, len(s.gameSessions))
	for _, session := range s.gameSessions {
		if session == nil {
			continue
		}
		if session.selectedCharacterID == 0 {
			continue
		}
		sessions = append(sessions, session)
	}
	s.mu.Unlock()
	return sessions
}

// refreshSessionQuestListAtDailyReset submits one session's refresh to its own
// event loop. The character identity is captured first and re-checked by the
// loop, so a relog or character switch between the boundary and the submission
// turns the refresh into a no-op instead of pushing yesterday's list onto the
// wrong character.
func (s *Service) refreshSessionQuestListAtDailyReset(session *gameSession) {
	if s == nil || session == nil {
		return
	}
	session.townMu.Lock()
	characterID := session.selectedCharacterID
	generation := session.characterGeneration
	session.townMu.Unlock()
	if characterID == 0 {
		return
	}
	err := s.postGameSessionCharacterEvent(
		session,
		"daily_quest_reset_refresh",
		characterID,
		generation,
		func() error {
			return s.sendDailyResetQuestRefresh(session, characterID)
		},
	)
	if err != nil && !isClosedGameSessionEventError(err) {
		s.logGameEvent(session, "game-daily-quest-reset-refresh-failed",
			"char_id", characterID,
			"character_generation", generation,
			"error", err)
	}
}

// sendDailyResetQuestRefresh settles the new service day for one online
// character and then republishes the authoritative task trio the accept path
// publishes (op574 active -> op356 completed -> op21 acceptable), which is the
// sequence the current client actually reacts to.
//
// The settlement must run before the projections are built: op356 is generated
// from the stored row, so reading it first would re-publish yesterday's
// completed daily as still completed.
func (s *Service) sendDailyResetQuestRefresh(session *gameSession, characterID uint16) error {
	if s == nil || session == nil || characterID == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()

	// Authoritative, and idempotent: it clears the daily rows that belong to the
	// service day that just ended, and the per-character/per-day marker on the
	// session makes a repeat call a no-op.
	s.reconcileCurrentDailyQuestSelectionIfNeeded(ctx, session, characterID)

	repositories, ok := s.repositoryGroup()
	if !ok || repositories.Quest == nil {
		return nil
	}
	source := "daily_quest_reset"
	if err := s.sendCurrentActiveQuestSnapshotForSession(session, source+"_active_op574"); err != nil {
		return err
	}
	// op356 goes out even when the table becomes empty. The client still holds
	// the pre-reset row for the daily that was just cleared and the table is a
	// full replacement, so an all-zero table is exactly what removes it - which is
	// why this path sends the committed body directly instead of
	// sendCurrentPersistedClearQuestListIfCompleted, whose "no completed quests,
	// nothing to restore" shortcut is right for a town transition and wrong here.
	characterKey := strconv.Itoa(int(characterID))
	record, found, err := repositories.Quest.Load(ctx, characterKey)
	if err != nil {
		return err
	}
	if found {
		if err := s.sendCurrentClearQuestListFromCommittedQuest(session, record, source+"_completed_op356"); err != nil {
			return err
		}
	} else {
		s.logGameEvent(session, "game-daily-quest-reset-refresh-op356-skipped",
			"char_id", characterID,
			"source", source,
			"reason", "character_has_no_quest_record")
	}
	if err := s.sendCurrentAcceptableQuestListOnlyForSession(session, source+"_acceptable_op21"); err != nil {
		return err
	}
	s.logGameEvent(session, "game-daily-quest-reset-refresh-sent",
		"char_id", characterID,
		"timer_kind", currentDailyQuestResetTimerKind,
		"quest_record_found", found,
		"sequence", "active_op574_then_completed_op356_then_acceptable_op21",
		"reason", "daily_boundary_passed_while_the_client_was_online")
	return nil
}
