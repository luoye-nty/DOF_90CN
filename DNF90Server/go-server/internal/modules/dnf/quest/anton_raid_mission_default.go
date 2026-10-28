package quest

import (
	"time"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

const (
	// AntonRaidAccessPrerequisiteQuestID is the ordinary Anton raid channel
	// unlock quest. Once it is complete the legacy five-day mission below is
	// no longer useful on the local single-player runtime.
	AntonRaidAccessPrerequisiteQuestID int64 = 12829
	// AntonRaidMissionQuestID is the legacy "[Anton Raid] Voracious Giant"
	// team mission whose completion the 90CN client requires before it opens
	// the Luke raid channel quest chain.
	AntonRaidMissionQuestID int64 = 12830
)

// DefaultCompletionPlan records the bounded quest-state mutation used to
// retire a legacy progression requirement without granting its old selectable
// equipment reward.
type DefaultCompletionPlan struct {
	Record        dnfrepo.QuestRecord
	QuestID       int64
	ChangedFields []dnfrepo.QuestField
}

// PlanAntonRaidMissionDefaultCompletion completes the legacy
// "[Anton Raid] Voracious Giant" mission once the character has unlocked the
// Anton raid channel. The 90CN client otherwise requires this five-day team
// mission before opening the Luke raid channel, while the local runtime offers
// a single-player Anton flow and does not own the original daily team counter.
//
// The migration is deliberately idempotent and does not grant the mission's
// selectable equipment reward.
func PlanAntonRaidMissionDefaultCompletion(
	record dnfrepo.QuestRecord,
	completedAt time.Time,
) DefaultCompletionPlan {
	if !questCompletedInRecord(record, AntonRaidAccessPrerequisiteQuestID) ||
		questCompletedInStates(record.States, AntonRaidMissionQuestID) {
		return DefaultCompletionPlan{Record: record, QuestID: AntonRaidMissionQuestID}
	}

	completed := dnfrepo.CloneQuest(record)
	if completed.States == nil {
		completed.States = make(map[int64]dnfrepo.QuestState)
	}
	state := completed.States[AntonRaidMissionQuestID]
	if completedAt.IsZero() {
		completedAt = time.Now().UTC()
	} else {
		completedAt = completedAt.UTC()
	}
	if state.Extra == nil {
		state.Extra = make(map[string]string)
	}
	state.Status = "completed"
	state.TriggerType = 0
	state.ProgressValue = 0
	state.RewardSelectIndex = int64(^uint16(0))
	state.Multiplier = 1
	state.UpdatedAt = completedAt
	state.Extra["auto_completed"] = "true"
	state.Extra["auto_complete_reason"] = "local_single_player_raid_progression"
	state.Extra["auto_completed_after_quest"] = "12829"
	state.Extra["reward_state"] = "skipped_default_completion"
	completed.States[AntonRaidMissionQuestID] = state

	changedFields := []dnfrepo.QuestField{dnfrepo.QuestFieldStates}
	if _, exists := completed.Progress[AntonRaidMissionQuestID]; exists {
		delete(completed.Progress, AntonRaidMissionQuestID)
		changedFields = append(changedFields, dnfrepo.QuestFieldProgress)
	}
	completed.UpdatedAt = completedAt
	return DefaultCompletionPlan{
		Record:        completed,
		QuestID:       AntonRaidMissionQuestID,
		ChangedFields: changedFields,
	}
}

func questCompletedInRecord(record dnfrepo.QuestRecord, questID int64) bool {
	return questCompletedInStates(record.States, questID) || questCompletedInStates(record.Progress, questID)
}

func questCompletedInStates(states map[int64]dnfrepo.QuestState, questID int64) bool {
	state, exists := states[questID]
	return exists && isCompletedQuestStatus(state.Status)
}
