package quest

import (
	"reflect"
	"testing"
	"time"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

func TestPlanBaseProfessionQuestResetClearsClassAndAwakeningOnly(t *testing.T) {
	catalog := &Catalog{byID: map[int64]Definition{
		10: {ID: 10, JobChangeQuest: 1},
		11: {ID: 11, JobChangeQuest: 2},
		12: {ID: 12, JobChangeQuest: 3},
		13: {ID: 13, JobChangeQuest: 20},
		14: {ID: 14},
	}}
	resetAt := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	record := dnfrepo.QuestRecord{
		CharacterID: "8",
		States: map[int64]dnfrepo.QuestState{
			10: {Status: "completed"},
			11: {Status: "active"},
			13: {Status: "active"},
			14: {Status: "completed"},
		},
		Progress: map[int64]dnfrepo.QuestState{
			10: {Status: "active"},
			12: {Status: "completed"},
			13: {Status: "active"},
		},
	}

	plan, err := catalog.PlanBaseProfessionQuestReset(record, resetAt)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(plan.ResetQuestIDs, []int64{10, 11, 12}) {
		t.Fatalf("reset quest IDs = %v", plan.ResetQuestIDs)
	}
	if !reflect.DeepEqual(plan.ChangedFields, []dnfrepo.QuestField{dnfrepo.QuestFieldStates, dnfrepo.QuestFieldProgress}) {
		t.Fatalf("changed fields = %v", plan.ChangedFields)
	}
	if !plan.Record.UpdatedAt.Equal(resetAt) {
		t.Fatalf("updated at = %v", plan.Record.UpdatedAt)
	}
	for _, questID := range []int64{10, 11, 12} {
		if _, ok := plan.Record.States[questID]; ok {
			t.Fatalf("profession quest %d remains in states", questID)
		}
		if _, ok := plan.Record.Progress[questID]; ok {
			t.Fatalf("profession quest %d remains in progress", questID)
		}
	}
	if _, ok := plan.Record.States[13]; !ok {
		t.Fatal("expert-job state was removed")
	}
	if _, ok := plan.Record.States[14]; !ok {
		t.Fatal("ordinary quest state was removed")
	}
	if _, ok := record.States[10]; !ok {
		t.Fatal("input record was mutated")
	}
}

func TestPlanBaseProfessionQuestResetNoopPreservesTimestamp(t *testing.T) {
	catalog := &Catalog{byID: map[int64]Definition{14: {ID: 14}}}
	updatedAt := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	plan, err := catalog.PlanBaseProfessionQuestReset(dnfrepo.QuestRecord{
		CharacterID: "8",
		States:      map[int64]dnfrepo.QuestState{14: {Status: "completed"}},
		UpdatedAt:   updatedAt,
	}, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.ResetQuestIDs) != 0 || len(plan.ChangedFields) != 0 || !plan.Record.UpdatedAt.Equal(updatedAt) {
		t.Fatalf("noop plan = %+v", plan)
	}
}
