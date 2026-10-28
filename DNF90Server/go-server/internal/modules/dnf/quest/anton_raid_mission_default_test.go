package quest

import (
	"reflect"
	"testing"
	"time"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

func TestPlanAntonRaidMissionDefaultCompletionRequiresAntonAccessQuest(t *testing.T) {
	record := dnfrepo.QuestRecord{
		CharacterID: "3",
		States: map[int64]dnfrepo.QuestState{
			2903: {Status: "completed"},
		},
	}
	plan := PlanAntonRaidMissionDefaultCompletion(record, time.Now())
	if len(plan.ChangedFields) != 0 || !reflect.DeepEqual(plan.Record, record) {
		t.Fatalf("unexpected default completion without prerequisite: %+v", plan)
	}
}

func TestPlanAntonRaidMissionDefaultCompletionAddsCompletedStateWithoutReward(t *testing.T) {
	completedAt := time.Date(2026, 8, 26, 4, 30, 0, 0, time.FixedZone("CST", 8*60*60))
	record := dnfrepo.QuestRecord{
		CharacterID: "3",
		States: map[int64]dnfrepo.QuestState{
			AntonRaidAccessPrerequisiteQuestID: {Status: "completed"},
		},
	}
	plan := PlanAntonRaidMissionDefaultCompletion(record, completedAt)
	if !reflect.DeepEqual(plan.ChangedFields, []dnfrepo.QuestField{dnfrepo.QuestFieldStates}) {
		t.Fatalf("changed fields=%v", plan.ChangedFields)
	}
	state, exists := plan.Record.States[AntonRaidMissionQuestID]
	if !exists || state.Status != "completed" || state.TriggerType != 0 || state.ProgressValue != 0 ||
		state.RewardSelectIndex != int64(^uint16(0)) || state.Multiplier != 1 ||
		state.Extra["reward_state"] != "skipped_default_completion" ||
		state.Extra["auto_complete_reason"] != "local_single_player_raid_progression" ||
		!state.UpdatedAt.Equal(completedAt.UTC()) {
		t.Fatalf("completed mission state=%+v", state)
	}
	if _, mutated := record.States[AntonRaidMissionQuestID]; mutated {
		t.Fatal("planning mutated the caller record")
	}
}

func TestPlanAntonRaidMissionDefaultCompletionRepairsActiveProjectionAndIsIdempotent(t *testing.T) {
	completedAt := time.Date(2026, 8, 26, 5, 0, 0, 0, time.UTC)
	record := dnfrepo.QuestRecord{
		CharacterID: "3",
		States: map[int64]dnfrepo.QuestState{
			AntonRaidAccessPrerequisiteQuestID: {Status: "completed"},
			AntonRaidMissionQuestID:            {Status: "active", ProgressValue: 7},
		},
		Progress: map[int64]dnfrepo.QuestState{
			AntonRaidMissionQuestID: {Status: "active", ProgressValue: 7},
		},
	}
	plan := PlanAntonRaidMissionDefaultCompletion(record, completedAt)
	if !reflect.DeepEqual(plan.ChangedFields, []dnfrepo.QuestField{dnfrepo.QuestFieldStates, dnfrepo.QuestFieldProgress}) {
		t.Fatalf("changed fields=%v", plan.ChangedFields)
	}
	if _, exists := plan.Record.Progress[AntonRaidMissionQuestID]; exists {
		t.Fatalf("active progress remained: %+v", plan.Record.Progress)
	}
	idempotent := PlanAntonRaidMissionDefaultCompletion(plan.Record, completedAt.Add(time.Hour))
	if len(idempotent.ChangedFields) != 0 || !reflect.DeepEqual(idempotent.Record, plan.Record) {
		t.Fatalf("second plan was not idempotent: %+v", idempotent)
	}
}
