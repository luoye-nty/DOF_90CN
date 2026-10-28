package dnfbridge

import (
	"context"
	"strconv"

	"longheng.io/server/internal/modules/dnf/premium"
	dnfquest "longheng.io/server/internal/modules/dnf/quest"
)

// reconcileCurrentDailyQuestSelectionIfNeeded clears repeatable quests that
// were last written in an earlier service day, once per character per service
// day.
//
// The scheduled 06:00 sweeps only fire while the process is alive. This
// deployment is started and stopped by hand, so every boundary the server
// slept through was skipped and yesterday's dailies stayed completed until
// someone happened to be online at 06:00. Reconciling from the login read
// makes the reset depend on the calendar instead of on uptime; the domain
// owner is idempotent, so a session that also crosses a live boundary is
// unaffected.
func (s *Service) reconcileCurrentDailyQuestSelectionIfNeeded(
	ctx context.Context,
	session *gameSession,
	characterID uint16,
) {
	if s == nil || session == nil || characterID == 0 {
		return
	}
	now := s.gameplayNow().UTC()
	serviceDayID := premium.ServiceDayID(now)
	if session.dailyQuestResetCharacterID == characterID &&
		session.dailyQuestResetServiceDayID == serviceDayID {
		return
	}
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.Quest == nil {
		return
	}
	catalog, err := s.loadQuestCatalog(ctx)
	if err != nil || catalog == nil {
		s.logGameEvent(session, "game-daily-quest-reset-deferred",
			"char_id", characterID,
			"reason", "quest_catalog_unavailable",
			"error", err)
		return
	}
	owner, err := dnfquest.NewOwner(repositories)
	if err != nil {
		return
	}
	result, err := owner.ReconcileDailyReset(ctx, catalog, strconv.Itoa(int(characterID)), now)
	if err != nil {
		s.logGameEvent(session, "game-daily-quest-reset-deferred",
			"char_id", characterID,
			"service_day_id", serviceDayID,
			"error", err)
		return
	}
	// Only remember the reconciliation once it actually ran against the
	// repository, so a deferred attempt retries on the next read.
	session.dailyQuestResetCharacterID = characterID
	session.dailyQuestResetServiceDayID = serviceDayID
	if !result.Changed {
		return
	}
	s.logGameEvent(session, "game-daily-quest-reset-reconciled",
		"char_id", characterID,
		"service_day_id", result.ServiceDayID,
		"reset_quest_count", len(result.ResetQuestIDs),
		"reset_quest_ids", result.ResetQuestIDs,
		"source", "login_read_catch_up_independent_of_server_uptime")
}
