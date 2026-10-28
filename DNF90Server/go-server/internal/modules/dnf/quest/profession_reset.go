package quest

import (
	"sort"
	"time"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

// ProfessionQuestResetPlan is the quest-ledger side of returning a character
// to its base profession. Profession quests are persisted independently from
// the character's grow type, so changing only the character would otherwise
// leave completed or active class-change/awakening rows behind.
type ProfessionQuestResetPlan struct {
	Record        dnfrepo.QuestRecord
	ResetQuestIDs []int64
	ChangedFields []dnfrepo.QuestField
}

// PlanBaseProfessionQuestReset removes every first-class, first-awakening and
// second-awakening quest state known to the active PVF. Expert-job quests use
// job-change type 20 and deliberately remain untouched.
func (c *Catalog) PlanBaseProfessionQuestReset(record dnfrepo.QuestRecord, resetAt time.Time) (ProfessionQuestResetPlan, error) {
	if c == nil {
		return ProfessionQuestResetPlan{}, ErrCatalogEmpty
	}
	reset := dnfrepo.CloneQuest(record)
	resetIDs := make(map[int64]struct{})
	statesChanged := resetProfessionQuestProjection(c, reset.States, resetIDs)
	progressChanged := resetProfessionQuestProjection(c, reset.Progress, resetIDs)
	ids := make([]int64, 0, len(resetIDs))
	for questID := range resetIDs {
		ids = append(ids, questID)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	fields := make([]dnfrepo.QuestField, 0, 2)
	if statesChanged {
		fields = append(fields, dnfrepo.QuestFieldStates)
	}
	if progressChanged {
		fields = append(fields, dnfrepo.QuestFieldProgress)
	}
	if len(fields) != 0 {
		if resetAt.IsZero() {
			resetAt = time.Now().UTC()
		}
		reset.UpdatedAt = resetAt
	}
	return ProfessionQuestResetPlan{Record: reset, ResetQuestIDs: ids, ChangedFields: fields}, nil
}

func resetProfessionQuestProjection(c *Catalog, states map[int64]dnfrepo.QuestState, resetIDs map[int64]struct{}) bool {
	changed := false
	for questID := range states {
		definition, ok := c.Find(questID)
		if !ok || definition.JobChangeQuest < 1 || definition.JobChangeQuest > 3 {
			continue
		}
		delete(states, questID)
		resetIDs[questID] = struct{}{}
		changed = true
	}
	return changed
}
