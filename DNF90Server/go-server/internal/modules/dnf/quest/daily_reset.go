package quest

import (
	"context"
	"sort"
	"time"

	"longheng.io/server/internal/modules/dnf/premium"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

// DailyResetResult describes one idempotent daily-quest reconciliation.
type DailyResetResult struct {
	CharacterID  string
	ServiceDayID int64
	Changed      bool
	// ResetQuestIDs are the repeatable quests whose stored state belonged to
	// an earlier service day and was cleared back to "not taken".
	ResetQuestIDs []int64
}

// IsDailyRepeatable reports whether a quest's PVF grade makes it a daily task
// that resets on the 06:00 service-day boundary. The whole daily family counts -
// `daily`, `special daily`, `daily mission`, `auto daily mission` and the legacy
// `daily random` - because the client's task manual groups them as one class.
// `normaly repeat` is deliberately excluded: it repeats with no day boundary, so
// its stored row is never cleared.
func (c *Catalog) IsDailyRepeatable(questID int64) bool {
	if c == nil {
		return false
	}
	definition, found := c.Find(questID)
	if !found {
		return false
	}
	return isDailyGrade(normalizeQuestTag(definition.Grade))
}

// ReconcileDailyReset clears every repeatable quest state that was last
// written in an earlier service day, so a daily finished yesterday is takeable
// again today.
//
// This is a catch-up reconciliation rather than a scheduled 06:00 sweep on
// purpose: this deployment is not running continuously, and a timer that only
// fires while the process is alive silently skips every boundary the server
// slept through - which is exactly how yesterday's dailies stayed completed.
// Running it from the login path makes the reset depend on the calendar, not
// on server uptime, and it is idempotent, so the two can coexist.
//
// The boundary is premium.ServiceDayID, the 06:00 Beijing reset the fatigue,
// daily-dungeon and delete-guard paths already share.
func (o *Owner) ReconcileDailyReset(
	ctx context.Context,
	catalog *Catalog,
	characterID string,
	now time.Time,
) (DailyResetResult, error) {
	if o == nil || o.quests == nil {
		return DailyResetResult{}, ErrOwnerUnavailable
	}
	if characterID == "" {
		return DailyResetResult{}, ErrCharacterRequired
	}
	if catalog == nil {
		return DailyResetResult{CharacterID: characterID, ServiceDayID: premium.ServiceDayID(now.UTC())}, nil
	}
	record, found, err := o.quests.Load(ctx, characterID)
	if err != nil {
		return DailyResetResult{}, err
	}
	result := DailyResetResult{CharacterID: characterID, ServiceDayID: premium.ServiceDayID(now.UTC())}
	if !found {
		return result, nil
	}
	fields := make([]dnfrepo.QuestField, 0, 2)
	if cleared := clearStaleDailyQuestStates(record.States, catalog, result.ServiceDayID, &result.ResetQuestIDs); cleared {
		fields = append(fields, dnfrepo.QuestFieldStates)
	}
	if cleared := clearStaleDailyQuestStates(record.Progress, catalog, result.ServiceDayID, &result.ResetQuestIDs); cleared {
		fields = append(fields, dnfrepo.QuestFieldProgress)
	}
	if len(fields) == 0 {
		return result, nil
	}
	sort.Slice(result.ResetQuestIDs, func(i, j int) bool { return result.ResetQuestIDs[i] < result.ResetQuestIDs[j] })
	record.CharacterID = characterID
	record.UpdatedAt = now.UTC()
	if err := dnfrepo.SaveQuestFields(ctx, o.quests, record, fields...); err != nil {
		return DailyResetResult{}, err
	}
	result.Changed = true
	return result, nil
}

// clearStaleDailyQuestStates drops daily quest rows written before the current
// service day. A row with no timestamp is left alone: without a completion time
// there is no evidence it belongs to an earlier day, and deleting it would wipe
// progress the player just made.
func clearStaleDailyQuestStates(
	states map[int64]dnfrepo.QuestState,
	catalog *Catalog,
	serviceDayID int64,
	resetQuestIDs *[]int64,
) bool {
	changed := false
	for questID, state := range states {
		if state.UpdatedAt.IsZero() ||
			premium.ServiceDayID(state.UpdatedAt.UTC()) >= serviceDayID ||
			!catalog.IsDailyRepeatable(questID) {
			continue
		}
		delete(states, questID)
		*resetQuestIDs = append(*resetQuestIDs, questID)
		changed = true
	}
	return changed
}
