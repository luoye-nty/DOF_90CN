package quest

import (
	"context"
	"testing"
	"time"

	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

func TestOwnerApplyClearMapCompletionPersistsAndVerifiesIdempotently(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	if err := repos.Character.Save(ctx, dnfrepo.CharacterRecord{CharacterID: "19", AccountID: "account"}); err != nil {
		t.Fatal(err)
	}
	if err := repos.Quest.Save(ctx, dnfrepo.QuestRecord{
		CharacterID: "19",
		States: map[int64]dnfrepo.QuestState{
			3145: {Status: "active", ProgressValue: 1},
		},
	}); err != nil {
		t.Fatal(err)
	}
	owner, err := NewOwner(repos)
	if err != nil {
		t.Fatal(err)
	}
	input := ClearMapCompletionInput{
		DungeonID: 3, MapID: 76126, CompletionKey: "runtime-12/op117/430", CompletedAt: time.Date(2026, 7, 15, 20, 45, 0, 0, time.UTC),
	}
	first, err := owner.ApplyClearMapCompletion(ctx, clearMapTestCatalog(t), "19", input)
	if err != nil {
		t.Fatal(err)
	}
	if first.Idempotent || len(first.Completions) != 1 || len(first.ChangedFields) != 1 {
		t.Fatalf("first result = %+v", first)
	}
	persisted, ok, err := repos.Quest.Load(ctx, "19")
	if err != nil || !ok {
		t.Fatalf("load persisted ok=%t err=%v", ok, err)
	}
	state := persisted.States[3145]
	if state.ProgressValue != 0 || state.Extra["completion_key"] != input.CompletionKey || state.Extra["reward_state"] != "pending" {
		t.Fatalf("persisted state = %+v", state)
	}
	second, err := owner.ApplyClearMapCompletion(ctx, clearMapTestCatalog(t), "19", input)
	if err != nil {
		t.Fatal(err)
	}
	if !second.Idempotent || len(second.Completions) != 1 || len(second.ChangedFields) != 0 {
		t.Fatalf("second result = %+v", second)
	}
}

func TestOwnerApplyConditionUnderClearPersistsPartialProgressIdempotently(t *testing.T) {
	ctx := context.Background()
	source := catalogTestSource{
		DefaultList: "5783 `train_twice.qst`\n",
		"n_quest/train_twice.qst": questCatalogTestDefinition("[epic]", 1, 99, "[all]",
			"[type]\n`[condition under clear]`\n[sub type]\n23\n[int data]\n-2 15 -1 2\n"),
	}
	index, err := dnfpvf.Build(ctx, source, dnfpvf.BuildOptions{Lists: []string{DefaultList}})
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := Load(ctx, index)
	if err != nil {
		t.Fatal(err)
	}
	repos := dnfrepomemory.NewMemoryGroup()
	if err := repos.Character.Save(ctx, dnfrepo.CharacterRecord{CharacterID: "19", AccountID: "account"}); err != nil {
		t.Fatal(err)
	}
	if err := repos.Quest.Save(ctx, dnfrepo.QuestRecord{
		CharacterID: "19",
		States: map[int64]dnfrepo.QuestState{
			5783: {Status: "active", ProgressValue: 2},
		},
	}); err != nil {
		t.Fatal(err)
	}
	owner, err := NewOwner(repos)
	if err != nil {
		t.Fatal(err)
	}
	input := ClearMapCompletionInput{
		DungeonID: 86, WorldMapID: 15, Difficulty: 0, HellPartyComplete: true,
		CompletionKey: "hell/run-1", CompletedAt: time.Date(2026, 8, 13, 18, 0, 0, 0, time.UTC),
	}
	first, err := owner.ApplyClearMapCompletion(ctx, catalog, "19", input)
	if err != nil {
		t.Fatal(err)
	}
	if first.Idempotent || len(first.Completions) != 1 || first.Completions[0].Completed || first.Completions[0].NewTrigger != 1 || len(first.ChangedFields) != 1 {
		t.Fatalf("first partial result = %+v", first)
	}
	persisted, ok, err := repos.Quest.Load(ctx, "19")
	if err != nil || !ok {
		t.Fatalf("load persisted ok=%t err=%v", ok, err)
	}
	state := persisted.States[5783]
	if state.Status != "active" || state.ProgressValue != 1 || state.Extra["completion_key"] != input.CompletionKey || state.Extra["reward_state"] != "" {
		t.Fatalf("persisted partial state = %+v", state)
	}
	second, err := owner.ApplyClearMapCompletion(ctx, catalog, "19", input)
	if err != nil {
		t.Fatal(err)
	}
	if !second.Idempotent || len(second.Completions) != 1 || second.Completions[0].NewTrigger != 1 || len(second.ChangedFields) != 0 {
		t.Fatalf("replayed partial result = %+v", second)
	}
}

func TestOwnerApplyClearMapCompletionDoesNotCreateFakeQuest(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	if err := repos.Character.Save(ctx, dnfrepo.CharacterRecord{CharacterID: "20", AccountID: "account"}); err != nil {
		t.Fatal(err)
	}
	owner, err := NewOwner(repos)
	if err != nil {
		t.Fatal(err)
	}
	result, err := owner.ApplyClearMapCompletion(ctx, clearMapTestCatalog(t), "20", ClearMapCompletionInput{
		MapID: 76126, CompletionKey: "runtime-empty", CompletedAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Completions) != 0 || len(result.ChangedFields) != 0 || result.Idempotent {
		t.Fatalf("result = %+v", result)
	}
	if _, ok, err := repos.Quest.Load(ctx, "20"); err != nil || ok {
		t.Fatalf("empty completion created quest row ok=%t err=%v", ok, err)
	}
}
