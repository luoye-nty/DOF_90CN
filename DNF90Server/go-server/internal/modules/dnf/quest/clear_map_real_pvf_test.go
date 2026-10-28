package quest

import (
	"context"
	"os"
	"reflect"
	"testing"
	"time"

	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

func TestRealScriptPVFQuest3145ClearMapCompletionPlan(t *testing.T) {
	pvfPath := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE to verify real clear-map completion")
	}
	archive, err := platformpvf.LoadArchive(platformpvf.Options{Path: pvfPath})
	if err != nil {
		t.Fatal(err)
	}
	index, err := dnfpvf.Build(context.Background(), archive, dnfpvf.BuildOptions{Lists: []string{DefaultList}})
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := Load(context.Background(), index)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := catalog.PlanClearMapCompletion(dnfrepo.QuestRecord{
		CharacterID: "19",
		States: map[int64]dnfrepo.QuestState{
			3145: {Status: "active", ProgressValue: 1},
		},
	}, ClearMapCompletionInput{
		DungeonID: 3, MapID: 76126, CompletionKey: "real-pvf-smoke/run/op117", CompletedAt: time.Date(2026, 7, 15, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Completions) != 1 || plan.Completions[0].QuestID != 3145 || plan.Record.States[3145].ProgressValue != 0 || plan.Record.States[3145].Extra["reward_state"] != "pending" {
		t.Fatalf("real quest 3145 clear-map plan = %+v", plan)
	}
	t.Logf("real quest 3145 clear-map completion path=%q map=%d", plan.Completions[0].PVFPath, 76126)
}

func TestRealScriptPVFQuest1680ClearADungeonCompletionPlan(t *testing.T) {
	pvfPath := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE to verify real clear-a-dungeon completion")
	}
	archive, err := platformpvf.LoadArchive(platformpvf.Options{Path: pvfPath})
	if err != nil {
		t.Fatal(err)
	}
	index, err := dnfpvf.Build(context.Background(), archive, dnfpvf.BuildOptions{Lists: []string{DefaultList}})
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := Load(context.Background(), index)
	if err != nil {
		t.Fatal(err)
	}
	definition, found := catalog.Find(1680)
	wantIntData := []int64{
		7165, -1, 7166, -1, 7167, -1, 7168, -1, 7169, -1,
		7170, -1, 7171, -1, 7172, -1, 7173, -1, 7174, -1,
		7175, -1, 7176, -1, 7177, -1, 7178, -1, 7179, -1,
	}
	if !found || normalizeQuestTag(definition.Type) != "clear a dungeon" ||
		definition.SubType != 6 || !reflect.DeepEqual(definition.IntData, wantIntData) {
		t.Fatalf("real quest 1680 definition = %+v found=%t", definition, found)
	}
	plan, err := catalog.PlanClearMapCompletion(dnfrepo.QuestRecord{
		CharacterID: "19",
		States:      map[int64]dnfrepo.QuestState{1680: {Status: "active", ProgressValue: 1}},
	}, ClearMapCompletionInput{
		DungeonID: 7165, Difficulty: 0,
		CompletionKey: "real-pvf-blackhall/run", CompletedAt: time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Completions) != 1 || plan.Completions[0].QuestID != 1680 ||
		!plan.Completions[0].Completed || plan.Record.States[1680].ProgressValue != 0 {
		t.Fatalf("real quest 1680 clear-a-dungeon plan = %+v", plan)
	}
}

func TestRealScriptPVFQuest5782RegionalHellPartyCompletionPlan(t *testing.T) {
	pvfPath := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE to verify real hell-party completion")
	}
	archive, err := platformpvf.LoadArchive(platformpvf.Options{Path: pvfPath})
	if err != nil {
		t.Fatal(err)
	}
	index, err := dnfpvf.Build(context.Background(), archive, dnfpvf.BuildOptions{Lists: []string{DefaultList}})
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := Load(context.Background(), index)
	if err != nil {
		t.Fatal(err)
	}
	definition, found := catalog.Find(5782)
	if !found || definition.SubType != 23 || !reflect.DeepEqual(definition.IntData, []int64{-2, 15, -1, 1}) {
		t.Fatalf("real quest 5782 definition = %+v found=%t", definition, found)
	}
	plan, err := catalog.PlanClearMapCompletion(dnfrepo.QuestRecord{
		CharacterID: "4", States: map[int64]dnfrepo.QuestState{5782: {Status: "active", ProgressValue: 1}},
	}, ClearMapCompletionInput{
		DungeonID: 86, MapID: 20317, WorldMapID: 15, Difficulty: 0, HellPartyComplete: true,
		CompletionKey: "real-pvf-smoke/hell-party/train", CompletedAt: time.Date(2026, 8, 13, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Completions) != 1 || plan.Completions[0].QuestID != 5782 || !plan.Completions[0].Completed || plan.Record.States[5782].ProgressValue != 0 {
		t.Fatalf("real quest 5782 hell-party plan = %+v", plan)
	}
}
