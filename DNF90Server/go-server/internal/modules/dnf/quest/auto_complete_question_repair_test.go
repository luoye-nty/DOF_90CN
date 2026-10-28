package quest

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

func TestPlanAutoCompleteMainStopsBeforePlayerQuestion(t *testing.T) {
	source := catalogTestSource{
		DefaultList: "100 `before.qst`\n101 `question.qst`\n102 `answer.qst`\n103 `independent.qst`\n",
		"n_quest/before.qst": questCatalogTestDefinition("[epic]", 20, 99, "[gunner]",
			"[type]\n`[meet npc]`\n"),
		"n_quest/question.qst": questCatalogTestDefinition("[epic]", 30, 99, "[gunner]",
			"[type]\n`[question]`\n[pre required quest]\n100\n"),
		"n_quest/answer.qst": questCatalogTestDefinition("[epic]", 30, 99, "[gunner]",
			"[type]\n`[meet npc]`\n[pre required quest answer]\n101 0\n"),
		"n_quest/independent.qst": questCatalogTestDefinition("[epic]", 25, 99, "[gunner]",
			"[type]\n`[meet npc]`\n"),
	}
	index, err := dnfpvf.Build(context.Background(), source, dnfpvf.BuildOptions{Lists: []string{DefaultList}})
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := Load(context.Background(), index)
	if err != nil {
		t.Fatal(err)
	}
	record := dnfrepo.QuestRecord{States: map[int64]dnfrepo.QuestState{
		100: {Status: "active"},
	}}
	plan, err := catalog.PlanAutoCompleteMainQuests(
		record,
		CharacterEligibility{Level: 90, Job: 2, GrowType: 2},
		89,
		0,
		time.Date(2026, 8, 22, 13, 0, 0, 0, time.UTC),
	)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(plan.ChangedQuestIDs, []int64{100, 103}) {
		t.Fatalf("changed quests=%v want [100 103]", plan.ChangedQuestIDs)
	}
	if _, completed := questStateFor(plan.Record, 101); completed {
		t.Fatalf("player question was auto-completed: %+v", plan.Record)
	}
	acceptable := catalog.Acceptable(CharacterEligibility{Level: 90, Job: 2, GrowType: 2}, plan.Record)
	if !containsCatalogQuestID(acceptable.IDs, 101) || containsCatalogQuestID(acceptable.IDs, 102) {
		t.Fatalf("acceptable after bulk=%v want question 101 only", acceptable.IDs)
	}
	if _, err := catalog.PlanAutoCompleteMainQuests(
		dnfrepo.QuestRecord{States: map[int64]dnfrepo.QuestState{
			100: {Status: "completed"},
			101: {Status: "active"},
		}},
		CharacterEligibility{Level: 90, Job: 2, GrowType: 2},
		89,
		101,
		time.Date(2026, 8, 22, 13, 0, 0, 0, time.UTC),
	); !errors.Is(err, ErrAutoCompleteTargetInvalid) {
		t.Fatalf("exact question auto-complete error=%v", err)
	}
}

func TestOwnerRepairsOnlyAutoCompletedQuestionWithoutPlayerAnswer(t *testing.T) {
	ctx := context.Background()
	source := catalogTestSource{
		DefaultList:                   "100 `before.qst`\n101 `broken_question.qst`\n102 `broken_answer.qst`\n201 `valid_question.qst`\n202 `valid_answer.qst`\n300 `ordinary.qst`\n",
		"n_quest/before.qst":          questCatalogTestDefinition("[epic]", 20, 99, "[gunner]", "[type]\n`[meet npc]`\n"),
		"n_quest/broken_question.qst": questCatalogTestDefinition("[epic]", 30, 99, "[gunner]", "[type]\n`[question]`\n[pre required quest]\n100\n"),
		"n_quest/broken_answer.qst":   questCatalogTestDefinition("[epic]", 30, 99, "[gunner]", "[type]\n`[meet npc]`\n[pre required quest answer]\n101 0\n"),
		"n_quest/valid_question.qst":  questCatalogTestDefinition("[epic]", 40, 99, "[gunner]", "[type]\n`[question]`\n"),
		"n_quest/valid_answer.qst":    questCatalogTestDefinition("[epic]", 40, 99, "[gunner]", "[type]\n`[meet npc]`\n[pre required quest answer]\n201 1\n"),
		"n_quest/ordinary.qst":        questCatalogTestDefinition("[epic]", 50, 99, "[gunner]", "[type]\n`[meet npc]`\n"),
	}
	index, err := dnfpvf.Build(ctx, source, dnfpvf.BuildOptions{Lists: []string{DefaultList}})
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := Load(ctx, index)
	if err != nil {
		t.Fatal(err)
	}
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "17", Job: "2", Level: 90, Stats: map[string]int64{"grow_type": 2},
	}); err != nil {
		t.Fatal(err)
	}
	autoCompleted := func() dnfrepo.QuestState {
		return dnfrepo.QuestState{Status: "completed", Extra: map[string]string{
			"auto_completed":       "true",
			"auto_complete_reason": "epic_below_character_level_no_reward",
		}}
	}
	record := dnfrepo.QuestRecord{CharacterID: "17", States: map[int64]dnfrepo.QuestState{
		100: autoCompleted(),
		101: autoCompleted(),
		201: {Status: "completed", Extra: map[string]string{
			"auto_completed":       "true",
			"auto_complete_reason": "epic_below_character_level_no_reward",
			questAnswerExtraKey:    "1",
		}},
		300: autoCompleted(),
	}}
	if err := repositories.Quest.Save(ctx, record); err != nil {
		t.Fatal(err)
	}
	owner, err := NewOwner(repositories)
	if err != nil {
		t.Fatal(err)
	}
	result, err := owner.ApplyAutoCompleteQuestionRepair(
		ctx, catalog, "17", time.Date(2026, 8, 22, 13, 30, 0, 0, time.UTC),
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.Idempotent || !reflect.DeepEqual(result.RepairedQuestIDs, []int64{101}) {
		t.Fatalf("repair result=%+v", result)
	}
	if _, found := questStateFor(result.Record, 101); found {
		t.Fatalf("broken question remained: %+v", result.Record)
	}
	for _, preserved := range []int64{100, 201, 300} {
		if _, found := questStateFor(result.Record, preserved); !found {
			t.Fatalf("quest %d was removed: %+v", preserved, result.Record)
		}
	}
	persisted, found, err := repositories.Quest.Load(ctx, "17")
	if err != nil || !found {
		t.Fatalf("persisted quest record found=%t err=%v", found, err)
	}
	if _, present := questStateFor(persisted, 101); present {
		t.Fatalf("broken question still persisted: %+v", persisted)
	}
	acceptable := catalog.Acceptable(CharacterEligibility{Level: 90, Job: 2, GrowType: 2}, result.Record)
	if !containsCatalogQuestID(acceptable.IDs, 101) {
		t.Fatalf("repaired question not acceptable: %v", acceptable.IDs)
	}
	replay, err := owner.ApplyAutoCompleteQuestionRepair(ctx, catalog, "17", time.Now().UTC())
	if err != nil || !replay.Idempotent || len(replay.RepairedQuestIDs) != 0 {
		t.Fatalf("repair replay=%+v err=%v", replay, err)
	}
}
