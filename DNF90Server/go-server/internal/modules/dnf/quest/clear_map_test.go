package quest

import (
	"context"
	"reflect"
	"testing"
	"time"

	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

func TestPlanClearMapCompletionUsesPVFTargetAndPreservesPendingReward(t *testing.T) {
	catalog := clearMapTestCatalog(t)
	completedAt := time.Date(2026, 7, 15, 20, 30, 0, 0, time.UTC)
	record := dnfrepo.QuestRecord{
		CharacterID: "19",
		States: map[int64]dnfrepo.QuestState{
			3145: {Status: "active", ProgressValue: 1, Extra: map[string]string{"kept": "yes"}},
			3146: {Status: "active", ProgressValue: 7},
		},
	}
	plan, err := catalog.PlanClearMapCompletion(record, ClearMapCompletionInput{
		DungeonID: 3, MapID: 76126, CompletionKey: "run-17/op117/430", CompletedAt: completedAt,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Completions) != 1 || plan.Completions[0].QuestID != 3145 || plan.Completions[0].PreviousTrigger != 1 || plan.Completions[0].Idempotent {
		t.Fatalf("completions = %+v", plan.Completions)
	}
	if !reflect.DeepEqual(plan.ChangedFields, []dnfrepo.QuestField{dnfrepo.QuestFieldStates}) {
		t.Fatalf("changed fields = %v", plan.ChangedFields)
	}
	state := plan.Record.States[3145]
	if state.Status != "active" || state.ProgressValue != 0 || state.Extra["reward_state"] != "pending" || state.Extra["completion_key"] != "run-17/op117/430" || state.Extra["kept"] != "yes" {
		t.Fatalf("completed state = %+v", state)
	}
	if other := plan.Record.States[3146]; other.ProgressValue != 7 {
		t.Fatalf("unrelated quest changed = %+v", other)
	}
	if original := record.States[3145]; original.ProgressValue != 1 || original.Extra["reward_state"] != "" {
		t.Fatalf("input record mutated = %+v", original)
	}
}

func TestPlanClearMapCompletionReplayIsIdempotent(t *testing.T) {
	catalog := clearMapTestCatalog(t)
	completedAt := time.Date(2026, 7, 15, 20, 30, 0, 0, time.UTC)
	input := ClearMapCompletionInput{MapID: 76126, CompletionKey: "run-17/op117/430", CompletedAt: completedAt}
	first, err := catalog.PlanClearMapCompletion(dnfrepo.QuestRecord{
		CharacterID: "19",
		States:      map[int64]dnfrepo.QuestState{3145: {Status: "active", ProgressValue: 1}},
	}, input)
	if err != nil {
		t.Fatal(err)
	}
	second, err := catalog.PlanClearMapCompletion(first.Record, input)
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Completions) != 1 || !second.Completions[0].Idempotent || len(second.ChangedFields) != 0 {
		t.Fatalf("replay plan = %+v", second)
	}
	if !reflect.DeepEqual(first.Record, second.Record) {
		t.Fatalf("replay changed record\nfirst=%+v\nsecond=%+v", first.Record, second.Record)
	}
}

func TestPlanClearMapCompletionMatchesDungeonIDAndProgressField(t *testing.T) {
	catalog := clearMapTestCatalog(t)
	plan, err := catalog.PlanClearMapCompletion(dnfrepo.QuestRecord{
		CharacterID: "19",
		Progress:    map[int64]dnfrepo.QuestState{3147: {Status: "accepted", ProgressValue: 2}},
	}, ClearMapCompletionInput{DungeonID: 3, CompletionKey: "run-dungeon-target", CompletedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Completions) != 1 || plan.Completions[0].Field != dnfrepo.QuestFieldProgress || plan.Record.Progress[3147].ProgressValue != 0 || !reflect.DeepEqual(plan.ChangedFields, []dnfrepo.QuestField{dnfrepo.QuestFieldProgress}) {
		t.Fatalf("plan = %+v", plan)
	}
}

func TestPlanConditionUnderClearSubtype6DecrementsPackedTarget(t *testing.T) {
	source := catalogTestSource{
		DefaultList: "500 `ordinary.qst`\n",
		"n_quest/ordinary.qst": questCatalogTestDefinition("[epic]", 1, 99, "[all]",
			"[type]\n`[condition under clear]`\n[sub type]\n6\n[int data]\n144 1 2\n"),
	}
	index, err := dnfpvf.Build(context.Background(), source, dnfpvf.BuildOptions{Lists: []string{DefaultList}})
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := Load(context.Background(), index)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := catalog.PlanClearMapCompletion(dnfrepo.QuestRecord{
		CharacterID: "19",
		States:      map[int64]dnfrepo.QuestState{500: {Status: "active", ProgressValue: 2}},
	}, ClearMapCompletionInput{
		DungeonID: 144, Difficulty: 1, CompletionKey: "ordinary/run-1", CompletedAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Completions) != 1 {
		t.Fatalf("ordinary completion count=%d plan=%+v", len(plan.Completions), plan)
	}
	completion := plan.Completions[0]
	if completion.NewTrigger != 1 || completion.Completed || plan.Record.States[500].ProgressValue != 1 || plan.Record.States[500].Extra["reward_state"] != "" {
		t.Fatalf("ordinary partial plan = %+v", plan)
	}
}

// 婚约任务 20737 的真实 PVF：[type] `[condition under clear]` + [sub type] 22 +
// [int data] `-1 -1 5`，即"跟心上人组 2 人队通关普通地下城 5 次"。
//
// 这个用例钉住三件事：
//  1. 初始 trigger 是 5（不是 1）—— subtype 22 没有自己的分支时会落到 stride-4 的
//     default 解析，算出 1，于是"一次通关就完成"；
//  2. 每次"夫妻二人通关"只减 1，减到 0 才算完成；
//  3. 不是这对夫妻通关时完全不匹配（-1 -1 之外还有 WeddingCouple 这道门）。
func TestPlanWeddingConditionUnderClearSubtype22CountsFiveCoupleClears(t *testing.T) {
	const questID = 20737
	source := catalogTestSource{
		DefaultList: "20737 `wedding.qst`\n",
		"n_quest/wedding.qst": questCatalogTestDefinition("[wedding]", 30, 99, "[all]",
			"[type]\n`[condition under clear]`\n[sub type]\n22\n[int data]\n-1 -1 5\n"),
	}
	index, err := dnfpvf.Build(context.Background(), source, dnfpvf.BuildOptions{Lists: []string{DefaultList}})
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := Load(context.Background(), index)
	if err != nil {
		t.Fatal(err)
	}
	definition, found := catalog.Find(questID)
	if !found {
		t.Fatal("wedding quest not in catalog")
	}
	if got := definitionInitialTrigger(definition, nil); got != 5 {
		t.Fatalf("婚约任务初始 trigger=%d，应为 5（5 次通关）", got)
	}

	// 第 1 次夫妻通关：5 -> 4，且不能算完成。
	first, err := catalog.PlanClearMapCompletion(dnfrepo.QuestRecord{
		CharacterID: "19",
		States:      map[int64]dnfrepo.QuestState{questID: {Status: "active", ProgressValue: 5}},
	}, ClearMapCompletionInput{
		WeddingCouple: true, DungeonID: 61, Difficulty: 2,
		CompletionKey: "wedding/run-1", CompletedAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Completions) != 1 {
		t.Fatalf("夫妻通关未匹配到婚约任务: %+v", first)
	}
	if got := first.Completions[0]; got.NewTrigger != 4 || got.Completed {
		t.Fatalf("第 1 次通关 NewTrigger=%d completed=%v，应为 4/false", got.NewTrigger, got.Completed)
	}
	if stored := first.Record.States[questID].ProgressValue; stored != 4 {
		t.Fatalf("持久化进度=%d，应为 4", stored)
	}

	// 第 5 次：1 -> 0，此时才算完成。
	last, err := catalog.PlanClearMapCompletion(dnfrepo.QuestRecord{
		CharacterID: "19",
		States:      map[int64]dnfrepo.QuestState{questID: {Status: "active", ProgressValue: 1}},
	}, ClearMapCompletionInput{
		WeddingCouple: true, DungeonID: 61, Difficulty: 2,
		CompletionKey: "wedding/run-5", CompletedAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(last.Completions) != 1 || !last.Completions[0].Completed || last.Completions[0].NewTrigger != 0 {
		t.Fatalf("第 5 次通关应当完成: %+v", last.Completions)
	}

	// 同样一次通关，但队伍不是这对夫妻 -> 不该匹配。
	notCouple, err := catalog.PlanClearMapCompletion(dnfrepo.QuestRecord{
		CharacterID: "19",
		States:      map[int64]dnfrepo.QuestState{questID: {Status: "active", ProgressValue: 5}},
	}, ClearMapCompletionInput{
		WeddingCouple: false, DungeonID: 61, Difficulty: 2,
		CompletionKey: "wedding/run-solo", CompletedAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(notCouple.Completions) != 0 {
		t.Fatalf("非夫妻通关不应计入婚约任务: %+v", notCouple.Completions)
	}
}

func TestConditionUnderClearPartialNoRewardSubQuestStaysActive(t *testing.T) {
	source := catalogTestSource{
		DefaultList: "503 `partial_sub.qst`\n",
		"n_quest/partial_sub.qst": questCatalogTestDefinition("[sub]", 1, 99, "[all]",
			"[type]\n`[condition under clear]`\n[sub type]\n6\n[int data]\n144 -1 2\n"),
	}
	index, err := dnfpvf.Build(context.Background(), source, dnfpvf.BuildOptions{Lists: []string{DefaultList}})
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := Load(context.Background(), index)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := catalog.PlanClearMapCompletion(dnfrepo.QuestRecord{
		CharacterID: "19",
		States:      map[int64]dnfrepo.QuestState{503: {Status: "active", ProgressValue: 2}},
	}, ClearMapCompletionInput{
		DungeonID: 144, Difficulty: 0, CompletionKey: "partial-sub/run-1", CompletedAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	state := plan.Record.States[503]
	if len(plan.Completions) != 1 || plan.Completions[0].Completed || state.Status != "active" || state.ProgressValue != 1 || state.Extra["reward_state"] != "" {
		t.Fatalf("partial no-reward subquest plan = %+v", plan)
	}
}

func TestConditionUnderClearSubtype6LegacyTwoFieldInitializesAndCompletesOnce(t *testing.T) {
	source := catalogTestSource{
		DefaultList: "501 `legacy.qst`\n",
		"n_quest/legacy.qst": questCatalogTestDefinition("[epic]", 1, 99, "[all]",
			"[type]\n`[condition under clear]`\n[sub type]\n6\n[int data]\n144 -1\n"),
	}
	index, err := dnfpvf.Build(context.Background(), source, dnfpvf.BuildOptions{Lists: []string{DefaultList}})
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := Load(context.Background(), index)
	if err != nil {
		t.Fatal(err)
	}
	definition, found := catalog.Find(501)
	if !found || definitionInitialTrigger(definition, nil) != 1 {
		t.Fatalf("legacy definition=%+v found=%t trigger=%d", definition, found, definitionInitialTrigger(definition, nil))
	}
	plan, err := catalog.PlanClearMapCompletion(dnfrepo.QuestRecord{
		CharacterID: "19", States: map[int64]dnfrepo.QuestState{501: {Status: "active", ProgressValue: 1}},
	}, ClearMapCompletionInput{DungeonID: 144, Difficulty: 0, CompletionKey: "legacy/run-1", CompletedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Completions) != 1 || !plan.Completions[0].Completed || plan.Record.States[501].ProgressValue != 0 {
		t.Fatalf("legacy plan = %+v", plan)
	}
}

func TestClearADungeonSubtype6CompletesAnyAuthoredAlternative(t *testing.T) {
	source := catalogTestSource{
		DefaultList: "1680 `blackhall/epic_70_blackhole_01.qst`\n",
		"n_quest/blackhall/epic_70_blackhole_01.qst": questCatalogTestDefinition("[side]", 70, 99, "[all]",
			"[type]\n`[clear a dungeon]`\n[sub type]\n6\n[int data]\n7165 -1 7166 -1 7179 -1\n"),
	}
	index, err := dnfpvf.Build(context.Background(), source, dnfpvf.BuildOptions{Lists: []string{DefaultList}})
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := Load(context.Background(), index)
	if err != nil {
		t.Fatal(err)
	}
	definition, found := catalog.Find(1680)
	if !found || definitionInitialTrigger(definition, nil) != 1 {
		t.Fatalf("clear-a-dungeon definition=%+v found=%t trigger=%d", definition, found, definitionInitialTrigger(definition, nil))
	}
	for _, dungeonID := range []int64{7165, 7179} {
		plan, planErr := catalog.PlanClearMapCompletion(dnfrepo.QuestRecord{
			CharacterID: "19",
			States:      map[int64]dnfrepo.QuestState{1680: {Status: "active", ProgressValue: 1}},
		}, ClearMapCompletionInput{
			DungeonID: dungeonID, Difficulty: 0,
			CompletionKey: "blackhall/run", CompletedAt: time.Now(),
		})
		if planErr != nil {
			t.Fatal(planErr)
		}
		state := plan.Record.States[1680]
		if len(plan.Completions) != 1 || !plan.Completions[0].Completed || state.ProgressValue != 0 || state.Extra["reward_state"] != "pending" {
			t.Fatalf("dungeon %d clear-a-dungeon plan = %+v", dungeonID, plan)
		}
	}

	miss, err := catalog.PlanClearMapCompletion(dnfrepo.QuestRecord{
		CharacterID: "19",
		States:      map[int64]dnfrepo.QuestState{1680: {Status: "active", ProgressValue: 1}},
	}, ClearMapCompletionInput{DungeonID: 7180, Difficulty: 0, CompletionKey: "blackhall/miss", CompletedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if len(miss.Completions) != 0 || miss.Record.States[1680].ProgressValue != 1 {
		t.Fatalf("unlisted dungeon changed clear-a-dungeon quest = %+v", miss)
	}
}

func TestConditionUnderClearSubtype11UsesOneTriggerAndRequiresAllRooms(t *testing.T) {
	source := catalogTestSource{
		DefaultList: "502 `all_rooms.qst`\n",
		"n_quest/all_rooms.qst": questCatalogTestDefinition("[epic]", 1, 99, "[all]",
			"[type]\n`[condition under clear]`\n[sub type]\n11\n[int data]\n85 -1 8\n"),
	}
	index, err := dnfpvf.Build(context.Background(), source, dnfpvf.BuildOptions{Lists: []string{DefaultList}})
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := Load(context.Background(), index)
	if err != nil {
		t.Fatal(err)
	}
	definition, found := catalog.Find(502)
	if !found || definitionInitialTrigger(definition, nil) != 1 {
		t.Fatalf("all-room definition=%+v found=%t trigger=%d", definition, found, definitionInitialTrigger(definition, nil))
	}
	base := dnfrepo.QuestRecord{CharacterID: "19", States: map[int64]dnfrepo.QuestState{502: {Status: "active", ProgressValue: 1}}}
	input := ClearMapCompletionInput{DungeonID: 85, Difficulty: 0, CompletionKey: "all-rooms/run-1", CompletedAt: time.Now()}
	miss, err := catalog.PlanClearMapCompletion(base, input)
	if err != nil {
		t.Fatal(err)
	}
	if len(miss.Completions) != 0 {
		t.Fatalf("incomplete all-room plan = %+v", miss)
	}
	input.AllRoomsCleared = true
	hit, err := catalog.PlanClearMapCompletion(base, input)
	if err != nil {
		t.Fatal(err)
	}
	if len(hit.Completions) != 1 || !hit.Completions[0].Completed || hit.Record.States[502].ProgressValue != 0 {
		t.Fatalf("complete all-room plan = %+v", hit)
	}
}

func TestPlanConditionUnderClearSubtype23RequiresHellAndRegion(t *testing.T) {
	catalog := conditionUnderClearHellPartyTestCatalog(t)
	base := dnfrepo.QuestRecord{CharacterID: "19", States: map[int64]dnfrepo.QuestState{5782: {Status: "active", ProgressValue: 1}}}
	input := ClearMapCompletionInput{
		DungeonID: 86, MapID: 20317, WorldMapID: 15, Difficulty: 0, HellPartyComplete: true,
		CompletionKey: "hell-party/train/run-1", CompletedAt: time.Now(),
	}
	plan, err := catalog.PlanClearMapCompletion(base, input)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Completions) != 1 || !plan.Completions[0].Completed || plan.Record.States[5782].ProgressValue != 0 {
		t.Fatalf("hell-party plan = %+v", plan)
	}
	for _, mutate := range []func(*ClearMapCompletionInput){
		func(value *ClearMapCompletionInput) { value.HellPartyComplete = false },
		func(value *ClearMapCompletionInput) { value.WorldMapID = 17 },
	} {
		candidate := input
		mutate(&candidate)
		rejected, rejectErr := catalog.PlanClearMapCompletion(base, candidate)
		if rejectErr != nil {
			t.Fatal(rejectErr)
		}
		if len(rejected.Completions) != 0 || rejected.Record.States[5782].ProgressValue != 1 {
			t.Fatalf("rejected hell-party plan = %+v", rejected)
		}
	}
}

func TestPlanConditionUnderClearSubtype23DecrementsRequiredCount(t *testing.T) {
	source := catalogTestSource{
		DefaultList: "5783 `train_twice.qst`\n",
		"n_quest/train_twice.qst": questCatalogTestDefinition("[epic]", 1, 99, "[all]",
			"[type]\n`[condition under clear]`\n[sub type]\n23\n[int data]\n-2 15 -1 2\n"),
	}
	index, err := dnfpvf.Build(context.Background(), source, dnfpvf.BuildOptions{Lists: []string{DefaultList}})
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := Load(context.Background(), index)
	if err != nil {
		t.Fatal(err)
	}
	definition, found := catalog.Find(5783)
	if !found || definitionInitialTrigger(definition, nil) != 2 {
		t.Fatalf("regional hell definition=%+v found=%t trigger=%d", definition, found, definitionInitialTrigger(definition, nil))
	}
	plan, err := catalog.PlanClearMapCompletion(dnfrepo.QuestRecord{
		CharacterID: "19", States: map[int64]dnfrepo.QuestState{5783: {Status: "active", ProgressValue: 2}},
	}, ClearMapCompletionInput{DungeonID: 86, WorldMapID: 15, Difficulty: 0, HellPartyComplete: true, CompletionKey: "hell/run-1", CompletedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Completions) != 1 || plan.Completions[0].Completed || plan.Completions[0].NewTrigger != 1 || plan.Record.States[5783].ProgressValue != 1 {
		t.Fatalf("regional hell partial plan = %+v", plan)
	}
	replay, err := catalog.PlanClearMapCompletion(plan.Record, ClearMapCompletionInput{
		DungeonID: 86, WorldMapID: 15, Difficulty: 0, HellPartyComplete: true,
		CompletionKey: "hell/run-1", CompletedAt: time.Now().Add(time.Second),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(replay.Completions) != 1 || !replay.Completions[0].Idempotent || replay.Completions[0].NewTrigger != 1 || len(replay.ChangedFields) != 0 || replay.Record.States[5783].ProgressValue != 1 {
		t.Fatalf("regional hell replay plan = %+v", replay)
	}
	next, err := catalog.PlanClearMapCompletion(plan.Record, ClearMapCompletionInput{
		DungeonID: 86, WorldMapID: 15, Difficulty: 0, HellPartyComplete: true,
		CompletionKey: "hell/run-2", CompletedAt: time.Now().Add(2 * time.Second),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(next.Completions) != 1 || !next.Completions[0].Completed || next.Completions[0].NewTrigger != 0 || next.Record.States[5783].ProgressValue != 0 {
		t.Fatalf("regional hell next-run plan = %+v", next)
	}
}

func conditionUnderClearHellPartyTestCatalog(t *testing.T) *Catalog {
	t.Helper()
	source := catalogTestSource{
		DefaultList: "5782 `train.qst`\n",
		"n_quest/train.qst": questCatalogTestDefinition("[epic]", 1, 99, "[all]",
			"[type]\n`[condition under clear]`\n[sub type]\n23\n[int data]\n-2 15 -1 1\n"),
	}
	index, err := dnfpvf.Build(context.Background(), source, dnfpvf.BuildOptions{Lists: []string{DefaultList}})
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := Load(context.Background(), index)
	if err != nil {
		t.Fatal(err)
	}
	return catalog
}

func TestPlanClearMapCompletionRetainsFinalLinkedChildForNativeParentFinish(t *testing.T) {
	source := catalogTestSource{
		DefaultList: "3054 `clear_sub.qst`\n3146 `parent.qst`\n",
		"n_quest/clear_sub.qst": questCatalogTestDefinition("[sub]", 1, 99, "[all]",
			"[type]\n`[clear map]`\n[main quest]\n3146\n[int data]\n76136\n"),
		"n_quest/parent.qst": questCatalogTestDefinition("[epic]", 1, 99, "[all]",
			"[type]\n`[quest clear]`\n[int data]\n3157 3054\n"),
	}
	index, err := dnfpvf.Build(context.Background(), source, dnfpvf.BuildOptions{Lists: []string{DefaultList}})
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := Load(context.Background(), index)
	if err != nil {
		t.Fatal(err)
	}
	completedAt := time.Date(2026, 7, 17, 12, 30, 0, 0, time.UTC)
	plan, err := catalog.PlanClearMapCompletion(dnfrepo.QuestRecord{
		CharacterID: "19",
		States: map[int64]dnfrepo.QuestState{
			3054: {Status: "active", ProgressValue: 1},
			3157: {Status: "completed", ProgressValue: 0},
			3146: {Status: "active", ProgressValue: 1},
		},
	}, ClearMapCompletionInput{MapID: 76136, CompletionKey: "run-3146/3054", CompletedAt: completedAt})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Completions) != 1 || plan.Completions[0].QuestID != 3054 {
		t.Fatalf("completions=%+v", plan.Completions)
	}
	child := plan.Record.States[3054]
	if child.Status != "active" || child.ProgressValue != 0 ||
		child.Extra["reward_state"] != "pending" ||
		child.Extra["completion_key"] != "run-3146/3054" ||
		child.Extra["auto_completed"] != "" {
		t.Fatalf("terminal linked child=%+v", child)
	}
	parent := plan.Record.States[3146]
	if parent.Status != "active" || parent.ProgressValue != 1 {
		t.Fatalf("quest-clear parent=%+v", parent)
	}
	if !reflect.DeepEqual(plan.ChangedFields, []dnfrepo.QuestField{dnfrepo.QuestFieldStates}) {
		t.Fatalf("changed fields=%v", plan.ChangedFields)
	}
}

func TestPlanClearMapCompletionDerivesMissingNoRewardSubFromActiveQuestClearParent(t *testing.T) {
	source := catalogTestSource{
		DefaultList: "3054 `clear_sub.qst`\n3146 `parent.qst`\n",
		"n_quest/clear_sub.qst": questCatalogTestDefinition("[sub]", 1, 99, "[all]",
			"[type]\n`[clear map]`\n[main quest]\n3146\n[int data]\n76136\n"),
		"n_quest/parent.qst": questCatalogTestDefinition("[epic]", 1, 99, "[all]",
			"[type]\n`[quest clear]`\n[int data]\n3157 3054\n"),
	}
	index, err := dnfpvf.Build(context.Background(), source, dnfpvf.BuildOptions{Lists: []string{DefaultList}})
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := Load(context.Background(), index)
	if err != nil {
		t.Fatal(err)
	}
	completedAt := time.Date(2026, 7, 17, 13, 20, 0, 0, time.UTC)
	plan, err := catalog.PlanClearMapCompletion(dnfrepo.QuestRecord{
		CharacterID: "19",
		States: map[int64]dnfrepo.QuestState{
			3157: {Status: "completed", ProgressValue: 0},
			3146: {Status: "active", ProgressValue: 2},
		},
	}, ClearMapCompletionInput{MapID: 76136, CompletionKey: "run-3146/stale-parent-only/3054", CompletedAt: completedAt})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Completions) != 1 || plan.Completions[0].QuestID != 3054 || plan.Completions[0].PreviousTrigger != 1 {
		t.Fatalf("completions=%+v", plan.Completions)
	}
	child := plan.Record.States[3054]
	if child.Status != "active" || child.ProgressValue != 0 ||
		child.Extra["reward_state"] != "pending" ||
		child.Extra["main_quest_id"] != "3146" ||
		child.Extra["auto_activated_by_main_quest"] != "true" ||
		child.Extra["completion_key"] != "run-3146/stale-parent-only/3054" ||
		child.Extra["auto_completed"] != "" {
		t.Fatalf("derived child=%+v", child)
	}
	if parent := plan.Record.States[3146]; parent.Status != "active" || parent.ProgressValue != 1 {
		t.Fatalf("quest-clear parent=%+v", parent)
	}
	if !reflect.DeepEqual(plan.ChangedFields, []dnfrepo.QuestField{dnfrepo.QuestFieldStates}) {
		t.Fatalf("changed fields=%v", plan.ChangedFields)
	}
}

func TestPlanClearMapCompletionReconcilesAllMissingNoRewardSubTasksBeforeCompletion(t *testing.T) {
	source := catalogTestSource{
		DefaultList: "3054 `clear_sub.qst`\n3157 `hunt_sub.qst`\n3146 `parent.qst`\n",
		"n_quest/clear_sub.qst": questCatalogTestDefinition("[sub]", 1, 99, "[all]",
			"[type]\n`[clear map]`\n[main quest]\n3146\n[int data]\n76136\n"),
		"n_quest/hunt_sub.qst": questCatalogTestDefinition("[sub]", 1, 99, "[all]",
			"[type]\n`[hunt enemy]`\n[main quest]\n3146\n[int data]\n3 -1 13099 3 1\n"),
		"n_quest/parent.qst": questCatalogTestDefinition("[epic]", 1, 99, "[all]",
			"[type]\n`[quest clear]`\n[int data]\n3157 3054\n"),
	}
	index, err := dnfpvf.Build(context.Background(), source, dnfpvf.BuildOptions{Lists: []string{DefaultList}})
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := Load(context.Background(), index)
	if err != nil {
		t.Fatal(err)
	}
	completedAt := time.Date(2026, 7, 17, 14, 5, 0, 0, time.UTC)
	plan, err := catalog.PlanClearMapCompletion(dnfrepo.QuestRecord{
		CharacterID: "19",
		States: map[int64]dnfrepo.QuestState{
			3146: {Status: "active", ProgressValue: 2},
		},
	}, ClearMapCompletionInput{MapID: 76136, CompletionKey: "run-3146/only-parent/3054", CompletedAt: completedAt})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Completions) != 1 || plan.Completions[0].QuestID != 3054 {
		t.Fatalf("completions=%+v", plan.Completions)
	}
	clearChild := plan.Record.States[3054]
	if clearChild.Status != "completed" || clearChild.ProgressValue != 0 ||
		clearChild.Extra["auto_activated_by_main_quest"] != "true" ||
		clearChild.Extra["completion_key"] != "run-3146/only-parent/3054" {
		t.Fatalf("clear child=%+v", clearChild)
	}
	huntChild := plan.Record.States[3157]
	if huntChild.Status != "active" || huntChild.ProgressValue != 1 ||
		huntChild.Extra["auto_activated_by_main_quest"] != "true" ||
		huntChild.Extra["auto_activation_reason"] != "active_quest_clear_parent_reconcile" {
		t.Fatalf("hunt child=%+v", huntChild)
	}
	if parent := plan.Record.States[3146]; parent.Status != "active" || parent.ProgressValue != 1 {
		t.Fatalf("quest-clear parent=%+v", parent)
	}
	if !reflect.DeepEqual(plan.ChangedFields, []dnfrepo.QuestField{dnfrepo.QuestFieldStates}) {
		t.Fatalf("changed fields=%v", plan.ChangedFields)
	}
}

func TestPlanClearMapCompletionRejectsNegativeTriggerAsCompletion(t *testing.T) {
	catalog := clearMapTestCatalog(t)
	record := dnfrepo.QuestRecord{
		CharacterID: "19",
		States:      map[int64]dnfrepo.QuestState{3145: {Status: "active", ProgressValue: -1}},
	}
	plan, err := catalog.PlanClearMapCompletion(record, ClearMapCompletionInput{
		MapID: 76126, CompletionKey: "corrupt-trigger", CompletedAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Completions) != 0 || len(plan.ChangedFields) != 0 || plan.Record.States[3145].ProgressValue != -1 {
		t.Fatalf("negative trigger plan = %+v", plan)
	}
}

func clearMapTestCatalog(t *testing.T) *Catalog {
	t.Helper()
	source := catalogTestSource{
		DefaultList:             "3145 `clear_map.qst`\n3146 `other.qst`\n3147 `dungeon.qst`\n",
		"n_quest/clear_map.qst": questCatalogTestDefinition("[epic]", 1, 99, "[gunner]", "[type]\n`[clear map]`\n[int data]\n76126\n"),
		"n_quest/other.qst":     questCatalogTestDefinition("[epic]", 1, 99, "[gunner]", "[type]\n`[clear map]`\n[int data]\n99999\n"),
		"n_quest/dungeon.qst":   questCatalogTestDefinition("[epic]", 1, 99, "[gunner]", "[type]\n`[clear map]`\n[int data]\n3\n"),
	}
	index, err := dnfpvf.Build(context.Background(), source, dnfpvf.BuildOptions{Lists: []string{DefaultList}})
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := Load(context.Background(), index)
	if err != nil {
		t.Fatal(err)
	}
	return catalog
}
