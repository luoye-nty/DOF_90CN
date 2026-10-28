package quest

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

func TestPlanAutoCompleteMainCompletesMissingPVFBoundNoRewardChildren(t *testing.T) {
	catalog := autoCompleteQuestClearClosureCatalog(false)
	completedAt := time.Date(2026, 8, 25, 15, 30, 0, 0, time.UTC)
	plan, err := catalog.PlanAutoCompleteMainQuests(
		dnfrepo.QuestRecord{CharacterID: "19"},
		CharacterEligibility{Level: 90, Job: 0, GrowType: 2},
		89,
		0,
		completedAt,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(plan.ChangedQuestIDs, []int64{3249}) ||
		!reflect.DeepEqual(plan.ChangedLinkedSubIDs, []int64{3609, 3610}) ||
		!reflect.DeepEqual(plan.ChangedFields, []dnfrepo.QuestField{dnfrepo.QuestFieldStates}) {
		t.Fatalf("auto-complete closure=%+v", plan)
	}
	if state := plan.Record.States[3249]; state.Status != "completed" ||
		state.Extra["auto_complete_reason"] != "epic_below_character_level_no_reward" {
		t.Fatalf("parent state=%+v", state)
	}
	for _, questID := range []int64{3609, 3610} {
		state := plan.Record.States[questID]
		if state.Status != "completed" || state.ProgressValue != 0 ||
			state.Extra["auto_complete_reason"] != "no_reward_sub_quest" ||
			state.Extra["auto_completed_by_parent"] != "3249" ||
			state.Extra["main_quest_id"] != "3249" || state.Extra["pvf_path"] == "" {
			t.Fatalf("child %d state=%+v", questID, state)
		}
	}
}

// The task manual completes every main quest below the character's level except
// class-change and awakening quests, so an item reward on a child is no reason
// to strand the parent. The child closes with it, reward suppressed; what a
// later epic still has to hand in comes back through the carry-forward pass.
func TestPlanAutoCompleteMainClosesRewardBearingQuestClearChild(t *testing.T) {
	catalog := autoCompleteQuestClearClosureCatalog(true)
	plan, err := catalog.PlanAutoCompleteMainQuests(
		dnfrepo.QuestRecord{CharacterID: "19", States: map[int64]dnfrepo.QuestState{
			3249: {Status: "active"},
		}},
		CharacterEligibility{Level: 90, Job: 0},
		89,
		3249,
		time.Date(2026, 8, 25, 15, 31, 0, 0, time.UTC),
	)
	if err != nil {
		t.Fatalf("reward-bearing child auto-complete error=%v", err)
	}
	if len(plan.ChangedQuestIDs) != 1 || plan.ChangedQuestIDs[0] != 3249 {
		t.Fatalf("changed parents=%v", plan.ChangedQuestIDs)
	}
	if len(plan.ChangedLinkedSubIDs) != 2 ||
		plan.ChangedLinkedSubIDs[0] != 3609 || plan.ChangedLinkedSubIDs[1] != 3610 {
		t.Fatalf("changed children=%v", plan.ChangedLinkedSubIDs)
	}
	// 3610 is the reward-bearing one in this fixture.
	state, known := questStateFor(plan.Record, 3610)
	if !known || !isCompletedQuestStatus(state.Status) ||
		state.Extra["auto_completed"] != "true" ||
		state.Extra["auto_complete_reason"] != "quest_clear_parent_closure" ||
		state.Extra["reward_state"] != finishRewardGranted ||
		state.Extra["auto_completed_by_parent"] != "3249" {
		t.Fatalf("reward-bearing child state=%+v known=%t", state, known)
	}
}

// A [question] child is the player's own branch choice and an awakening child
// has to be run, so either one still stops the parent.
func TestPlanAutoCompleteMainRefusesQuestionAndAwakeningQuestClearChildren(t *testing.T) {
	for _, testCase := range []struct {
		name  string
		child Definition
	}{
		{name: "question child", child: Definition{
			ID: 3610, Path: "n_quest/branch.qst", Grade: "[sub]", Type: "[question]",
			MainQuestID: 3249, NoExperience: true, RewardType: "[item]", RewardIntData: []int64{0, 0},
		}},
		{name: "awakening child", child: Definition{
			ID: 3610, Path: "n_quest/awakening_child.qst", Grade: "[sub]", Type: "[meet npc]",
			MainQuestID: 3249, NoExperience: true, JobChangeQuest: 2,
			RewardType: "[awakening type]", RewardIntData: []int64{0, 0},
		}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			parent := Definition{
				ID: 3249, Path: "n_quest/parent.qst", Grade: "[epic]", Type: "[quest clear]",
				SubType: 2, IntData: []int64{3610}, LevelMin: 35, LevelMax: 99,
				Job: "[all]", ExposedByNPC: 303, RewardType: "[item]", RewardIntData: []int64{0, 0},
			}
			catalog := &Catalog{
				ordered: []Definition{parent, testCase.child},
				byID:    map[int64]Definition{3249: parent, 3610: testCase.child},
			}
			_, err := catalog.PlanAutoCompleteMainQuests(
				dnfrepo.QuestRecord{CharacterID: "19", States: map[int64]dnfrepo.QuestState{
					3249: {Status: "active"},
				}},
				CharacterEligibility{Level: 90, Job: 0},
				89,
				3249,
				time.Date(2026, 8, 25, 15, 31, 0, 0, time.UTC),
			)
			if !errors.Is(err, ErrAutoCompleteTargetInvalid) {
				t.Fatalf("closure error=%v want target invalid", err)
			}
		})
	}
}

func TestOwnerReconcilesHistoricalAutoCompletedQuestClearClosure(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{CharacterID: "19", Job: "0", Level: 90}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Quest.Save(ctx, dnfrepo.QuestRecord{
		CharacterID: "19",
		States: map[int64]dnfrepo.QuestState{
			3249: {
				Status: "completed",
				Extra: map[string]string{
					"auto_completed":       "true",
					"auto_complete_reason": "epic_below_character_level_no_reward",
					"reward_state":         finishRewardGranted,
				},
			},
		},
	}); err != nil {
		t.Fatal(err)
	}
	owner, err := NewOwner(repositories)
	if err != nil {
		t.Fatal(err)
	}
	reconciledAt := time.Date(2026, 8, 25, 15, 32, 0, 0, time.UTC)
	result, err := owner.ApplyAutoCompletedLinkedSubQuestReconciliation(
		ctx,
		autoCompleteQuestClearClosureCatalog(false),
		"19",
		reconciledAt,
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.Idempotent || len(result.Completions) != 2 ||
		!result.Completions[0].Synthesized || !result.Completions[1].Synthesized {
		t.Fatalf("reconcile result=%+v", result)
	}
	for _, questID := range []int64{3609, 3610} {
		state := result.PostCommit.States[questID]
		if state.Status != "completed" || state.Extra["auto_completed_by_parent"] != "3249" {
			t.Fatalf("reconciled child %d state=%+v", questID, state)
		}
	}
	replay, err := owner.ApplyAutoCompletedLinkedSubQuestReconciliation(
		ctx,
		autoCompleteQuestClearClosureCatalog(false),
		"19",
		reconciledAt.Add(time.Second),
	)
	if err != nil || !replay.Idempotent || len(replay.Completions) != 0 {
		t.Fatalf("reconcile replay=%+v err=%v", replay, err)
	}
}

func autoCompleteQuestClearClosureCatalog(rewardBearing bool) *Catalog {
	childReward := []int64{0, 0}
	childItems := []RewardItemRule(nil)
	if rewardBearing {
		childReward = []int64{10164786, 1}
		childItems = []RewardItemRule{{ItemID: 10164786, Count: 1}}
	}
	parent := Definition{
		ID: 3249, Path: "n_quest/quest_renewal/epic_quest/alphraira/alphraira_epic_26.qst",
		Grade: "[epic]", Type: "[quest clear]", SubType: 2, IntData: []int64{3609, 3610},
		LevelMin: 35, LevelMax: 99, Job: "[all]", ExposedByNPC: 303,
		RewardType: "[item]", RewardIntData: []int64{0, 0},
	}
	first := Definition{
		ID: 3609, Path: "n_quest/quest_renewal/epic_quest/alphraira/alphraira_epic_26_01.qst",
		Grade: "[sub]", Type: "[meet npc]", MainQuestID: 3249, NoExperience: true,
		RewardType: "[item]", RewardIntData: []int64{0, 0},
	}
	second := Definition{
		ID: 3610, Path: "n_quest/quest_renewal/epic_quest/alphraira/alphraira_epic_26_02.qst",
		Grade: "[sub]", Type: "[seek n meet npc]", MainQuestID: 3249, NoExperience: true,
		RewardType: "[item]", RewardIntData: childReward, RewardItems: childItems,
	}
	return &Catalog{
		ordered: []Definition{parent, first, second},
		byID:    map[int64]Definition{3249: parent, 3609: first, 3610: second},
	}
}

// An empty bulk result on an untransferred character is reported as the missing
// class change, because the story chain past the first awakening level is gated
// on the awakening quest group and a bulk pass must never complete that for the
// player. Jobs 9 and 10 have no class change and stay exempt, and a request
// that still has work to do is never rejected.
func TestPlanAutoCompleteMainReportsMissingClassChangeOnEmptyBulkResult(t *testing.T) {
	catalog := autoCompleteQuestClearClosureCatalog(false)
	completedAt := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	exhausted := dnfrepo.QuestRecord{CharacterID: "19", States: map[int64]dnfrepo.QuestState{
		3249: {Status: "completed"},
		3609: {Status: "completed"},
		3610: {Status: "completed"},
	}}
	for _, testCase := range []struct {
		name     string
		record   dnfrepo.QuestRecord
		job      int
		growType int
		cutoff   int
		wantErr  error
	}{
		{name: "untransferred and exhausted", record: exhausted, job: 0, growType: 0, cutoff: 89, wantErr: ErrAutoCompleteClassRequired},
		{name: "transferred and exhausted", record: exhausted, job: 0, growType: 2, cutoff: 89, wantErr: ErrAutoCompleteTargetInvalid},
		{name: "awakened and exhausted", record: exhausted, job: 0, growType: 0x12, cutoff: 89, wantErr: ErrAutoCompleteTargetInvalid},
		{name: "below awakening cutoff", record: exhausted, job: 0, growType: 0, cutoff: 49, wantErr: ErrAutoCompleteTargetInvalid},
		{name: "job without class change", record: exhausted, job: 9, growType: 0, cutoff: 89, wantErr: ErrAutoCompleteTargetInvalid},
		{name: "untransferred with work left", record: dnfrepo.QuestRecord{CharacterID: "19"}, job: 0, growType: 0, cutoff: 89},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := catalog.PlanAutoCompleteMainQuests(
				testCase.record,
				CharacterEligibility{Level: 90, Job: testCase.job, GrowType: testCase.growType},
				testCase.cutoff,
				0,
				completedAt,
			)
			if testCase.wantErr == nil {
				if err != nil {
					t.Fatalf("plan err=%v, want a completed plan", err)
				}
				return
			}
			if !errors.Is(err, testCase.wantErr) {
				t.Fatalf("plan err=%v want=%v", err, testCase.wantErr)
			}
		})
	}
}

// The story chain standing on an awakening quest is the common empty-bulk case,
// and the quest is still sitting in the player's list. Report which one blocks
// the pass instead of "nothing left", and never complete it here.
func TestPlanAutoCompleteMainNamesTheBlockingAwakeningQuest(t *testing.T) {
	awakening := Definition{
		ID: 4000, Path: "n_quest/awakening.qst", Name: "依然存在的不祥之感",
		Grade: "[epic]", Type: "[clear map]", LevelMin: 50, LevelMax: 99,
		Job: "[swordman]", ExposedByNPC: 28, GrowType: 3, HasGrowType: true,
		JobChangeQuest: 2, RewardType: "[awakening type]",
	}
	successor := Definition{
		ID: 4001, Path: "n_quest/after_awakening.qst", Name: "银冠之镜",
		Grade: "[epic]", Type: "[meet npc]", LevelMin: 50, LevelMax: 99,
		Job: "[all]", ExposedByNPC: 28, PreRequiredGroups: [][]int64{{4000}},
	}
	catalog := &Catalog{
		ordered: []Definition{awakening, successor},
		byID:    map[int64]Definition{4000: awakening, 4001: successor},
	}
	_, err := catalog.PlanAutoCompleteMainQuests(
		dnfrepo.QuestRecord{CharacterID: "19"},
		CharacterEligibility{Level: 90, Job: 0, GrowType: 0x13},
		89,
		0,
		time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC),
	)
	if !errors.Is(err, ErrAutoCompleteAwakeningRequired) {
		t.Fatalf("plan err=%v want awakening required", err)
	}
	var blocked *AwakeningRequiredError
	if !errors.As(err, &blocked) || blocked.QuestID != 4000 || blocked.QuestName != "依然存在的不祥之感" {
		t.Fatalf("blocking quest=%+v err=%v", blocked, err)
	}
	// Completing the awakening quest by hand unblocks the rest of the chain.
	plan, err := catalog.PlanAutoCompleteMainQuests(
		dnfrepo.QuestRecord{CharacterID: "19", States: map[int64]dnfrepo.QuestState{
			4000: {Status: "completed"},
		}},
		CharacterEligibility{Level: 90, Job: 0, GrowType: 0x13},
		89,
		0,
		time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC),
	)
	if err != nil || len(plan.ChangedQuestIDs) != 1 || plan.ChangedQuestIDs[0] != 4001 {
		t.Fatalf("post-awakening plan changed=%v err=%v", plan.ChangedQuestIDs, err)
	}
}

// The Lv87 Zelba chain stands on 3868 冒险家联盟的劝说和选择, a [question] quest
// whose answer gates every successor through [pre required quest answer]. The
// pass must not answer it, so an empty bulk result there has to name it rather
// than claim there is nothing left.
func TestPlanAutoCompleteMainNamesTheBlockingChoiceQuest(t *testing.T) {
	choice := Definition{
		ID: 3868, Path: "n_quest/branch.qst", Name: "冒险家联盟的劝说和选择",
		Grade: "[epic]", Type: "[question]", LevelMin: 87, LevelMax: 99,
		Job: "[all]", ExposedByNPC: 331,
	}
	branchA := Definition{
		ID: 3870, Path: "n_quest/branch_a.qst", Name: "联盟路线",
		Grade: "[epic]", Type: "[meet npc]", LevelMin: 87, LevelMax: 99,
		Job: "[all]", ExposedByNPC: 331,
		PreRequiredAnswers: []QuestAnswerRequirement{{QuestID: 3868, Answer: 1}},
	}
	catalog := &Catalog{
		ordered: []Definition{choice, branchA},
		byID:    map[int64]Definition{3868: choice, 3870: branchA},
	}
	record := dnfrepo.QuestRecord{CharacterID: "19", States: map[int64]dnfrepo.QuestState{
		3868: {Status: "active"},
	}}
	_, err := catalog.PlanAutoCompleteMainQuests(
		record,
		CharacterEligibility{Level: 90, Job: 0, GrowType: 0x13},
		89,
		0,
		time.Date(2026, 9, 10, 14, 0, 0, 0, time.UTC),
	)
	if !errors.Is(err, ErrAutoCompleteChoiceRequired) {
		t.Fatalf("plan err=%v want choice required", err)
	}
	var blocked *ChoiceRequiredError
	if !errors.As(err, &blocked) || blocked.QuestID != 3868 || blocked.QuestName != "冒险家联盟的劝说和选择" {
		t.Fatalf("blocking choice=%+v err=%v", blocked, err)
	}
	// Once the player has answered, the branch the answer opens completes.
	answered := dnfrepo.CloneQuest(record)
	answered.States[3868] = dnfrepo.QuestState{
		Status: "completed",
		Extra:  map[string]string{questAnswerExtraKey: "1"},
	}
	plan, err := catalog.PlanAutoCompleteMainQuests(
		answered,
		CharacterEligibility{Level: 90, Job: 0, GrowType: 0x13},
		89,
		0,
		time.Date(2026, 9, 10, 14, 0, 0, 0, time.UTC),
	)
	if err != nil || len(plan.ChangedQuestIDs) != 1 || plan.ChangedQuestIDs[0] != 3870 {
		t.Fatalf("post-answer plan changed=%v err=%v", plan.ChangedQuestIDs, err)
	}
}
