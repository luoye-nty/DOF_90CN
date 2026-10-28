package quest

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"longheng.io/server/internal/modules/dnf/premium"
	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

// dailyQuestResetTestCatalog is the shared fixture for the daily-reset specs. It
// carries one quest per grade class the 06:00 boundary must distinguish:
//
//	1 daily              reset
//	2 special daily      reset
//	3 normaly repeat     never reset (repeatable, not a daily)
//	4 epic               never reset (one-shot)
//	5 daily mission      reset (the mission tab's daily)
//	6 auto daily mission reset
func dailyQuestResetTestCatalog(t *testing.T) *Catalog {
	t.Helper()
	source := catalogTestSource{
		DefaultList: "1 `daily.qst`\n2 `special.qst`\n3 `repeat.qst`\n4 `epic.qst`\n" +
			"5 `dailymission.qst`\n6 `autodailymission.qst`\n",
		"n_quest/daily.qst":            questCatalogTestDefinition("[daily]", 1, 90, "[gunner]", ""),
		"n_quest/special.qst":          questCatalogTestDefinition("[special daily]", 1, 90, "[gunner]", ""),
		"n_quest/repeat.qst":           questCatalogTestDefinition("[normaly repeat]", 1, 90, "[gunner]", ""),
		"n_quest/epic.qst":             questCatalogTestDefinition("[epic]", 1, 90, "[gunner]", ""),
		"n_quest/dailymission.qst":     questCatalogTestDefinition("[daily mission]", 1, 90, "[gunner]", ""),
		"n_quest/autodailymission.qst": questCatalogTestDefinition("[auto daily mission]", 1, 90, "[gunner]", ""),
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

// TestDailyResetClassificationCoversTheWholeDailyFamily pins which grades the
// 06:00 boundary clears. Every grade the client's task manual classes as a daily
// counts - including `daily mission` (the mission tab's daily) and
// `auto daily mission`, which the earlier exact-match predicate missed and which
// therefore stayed one-shot forever. `normaly repeat` is repeatable rather than
// daily, so its row survives the boundary.
func TestDailyResetClassificationCoversTheWholeDailyFamily(t *testing.T) {
	catalog := dailyQuestResetTestCatalog(t)
	for _, probe := range []struct {
		name    string
		questID int64
		want    bool
	}{
		{"daily", 1, true},
		{"special daily", 2, true},
		{"normaly repeat", 3, false},
		{"epic", 4, false},
		{"daily mission", 5, true},
		{"auto daily mission", 6, true},
		{"unknown quest id", 99, false},
	} {
		if got := catalog.IsDailyRepeatable(probe.questID); got != probe.want {
			t.Fatalf("%s: resets at the daily boundary=%t, want %t", probe.name, got, probe.want)
		}
	}
}

// TestCatalogCompletedDailyLeavesAcceptableListForTheRestOfTheServiceDay pins
// the client-visible half of the rule: finishing a daily removes it from the
// acceptable list immediately, and only the 06:00 reset - which clears the row -
// brings it back. It replaces the earlier behaviour where a completed daily was
// re-acceptable at once, which made every daily an unlimited same-day repeat.
func TestCatalogCompletedDailyLeavesAcceptableListForTheRestOfTheServiceDay(t *testing.T) {
	catalog := dailyQuestResetTestCatalog(t)
	character := CharacterEligibility{Level: 90, Job: 2, GrowType: 0x12}

	completedDaily := dnfrepo.QuestRecord{States: map[int64]dnfrepo.QuestState{
		1: {Status: "completed"},
		5: {Status: "completed"},
		6: {Status: "completed"},
	}}
	listed := catalog.QuestList(character, completedDaily).IDs
	for _, questID := range []int32{1, 5, 6} {
		if slices.Contains(listed, questID) {
			t.Fatalf("completed daily quest %d is still in the acceptable list", questID)
		}
	}
	// Re-accepting is refused, not merely hidden from the window.
	if _, err := catalog.PlanAccept(character, completedDaily, 1); !errors.Is(err, ErrQuestNotAcceptable) {
		t.Fatalf("re-accepting a completed daily err=%v, want ErrQuestNotAcceptable", err)
	}

	// A repeatable quest must stay acceptable after completion.
	completedRepeat := dnfrepo.QuestRecord{States: map[int64]dnfrepo.QuestState{
		3: {Status: "completed"},
	}}
	if !slices.Contains(catalog.QuestList(character, completedRepeat).IDs, int32(3)) {
		t.Fatal("a completed normaly-repeat quest disappeared from the acceptable list")
	}

	// A one-shot quest never returns.
	completedEpic := dnfrepo.QuestRecord{States: map[int64]dnfrepo.QuestState{
		4: {Status: "completed"},
	}}
	if slices.Contains(catalog.QuestList(character, completedEpic).IDs, int32(4)) {
		t.Fatal("a completed one-shot quest came back to the acceptable list")
	}
}

// TestReconcileDailyResetClearsYesterdaysDailyQuests covers the report that a
// daily finished yesterday stays finished when the server was not running at
// 06:00 - including `daily mission`, which used to never reset at all. The
// reconciliation is driven by the stored completion timestamp rather than by a
// tick, so it catches up however long the process was down.
func TestReconcileDailyResetClearsYesterdaysDailyQuests(t *testing.T) {
	ctx := context.Background()
	catalog := dailyQuestResetTestCatalog(t)

	now := time.Date(2026, 9, 10, 8, 0, 0, 0, time.UTC)
	yesterday := now.Add(-24 * time.Hour)
	if premium.ServiceDayID(yesterday) >= premium.ServiceDayID(now) {
		t.Fatal("probe timestamps are inside the same service day")
	}
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Quest.Save(ctx, dnfrepo.QuestRecord{
		CharacterID: "1",
		States: map[int64]dnfrepo.QuestState{
			1: {Status: "completed", UpdatedAt: yesterday},
			2: {Status: "completed", UpdatedAt: now.Add(-10 * time.Minute)},
			3: {Status: "active", ProgressValue: 3, UpdatedAt: yesterday},
			4: {Status: "completed", UpdatedAt: yesterday},
			5: {Status: "completed", UpdatedAt: yesterday},
			6: {Status: "completed", UpdatedAt: yesterday},
		},
		Progress: map[int64]dnfrepo.QuestState{
			1: {Status: "completed", ProgressValue: 1, UpdatedAt: yesterday},
		},
	}); err != nil {
		t.Fatal(err)
	}
	owner, err := NewOwner(repositories)
	if err != nil {
		t.Fatal(err)
	}
	result, err := owner.ReconcileDailyReset(ctx, catalog, "1", now)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Changed {
		t.Fatal("reconcile reported no change")
	}
	// Quest 1 is cleared once from States and once from Progress, so the sorted
	// list carries its id twice.
	wantReset := []int64{1, 1, 5, 6}
	if !slices.Equal(result.ResetQuestIDs, wantReset) {
		t.Fatalf("reset ids=%v, want %v", result.ResetQuestIDs, wantReset)
	}
	record, found, err := repositories.Quest.Load(ctx, "1")
	if err != nil || !found {
		t.Fatalf("load quest record found=%t err=%v", found, err)
	}
	for _, questID := range []int64{1, 5, 6} {
		if _, exists := record.States[questID]; exists {
			t.Fatalf("yesterday's daily quest %d is still stored", questID)
		}
	}
	if _, exists := record.Progress[1]; exists {
		t.Fatal("yesterday's daily kept its progress row")
	}
	if state, exists := record.States[2]; !exists || state.Status != "completed" {
		t.Fatalf("today's daily was reset: %+v", record.States[2])
	}
	if state, exists := record.States[3]; !exists || state.Status != "active" || state.ProgressValue != 3 {
		t.Fatalf("the repeatable quest was reset: %+v", record.States[3])
	}
	if state, exists := record.States[4]; !exists || state.Status != "completed" {
		t.Fatalf("a one-shot quest was reset: %+v", record.States[4])
	}

	// Running it again on the same day changes nothing.
	second, err := owner.ReconcileDailyReset(ctx, catalog, "1", now)
	if err != nil || second.Changed {
		t.Fatalf("second reconcile changed=%t err=%v", second.Changed, err)
	}
}

// TestReconcileDailyResetReturnsYesterdaysDailyMissionToTheAcceptableList is the
// end-to-end guard for the reported defect: a `[daily mission]` behaved as a
// permanent one-shot because no boundary ever cleared its row, while `[daily]`
// behaved as an unlimited same-day repeat because a completed row never blocked
// re-acceptance. Both must now land on the same rule - one completion per
// service day, reset at 06:00.
func TestReconcileDailyResetReturnsYesterdaysDailyMissionToTheAcceptableList(t *testing.T) {
	ctx := context.Background()
	catalog := dailyQuestResetTestCatalog(t)
	character := CharacterEligibility{Level: 90, Job: 2, GrowType: 0x12}

	now := time.Date(2026, 9, 10, 8, 0, 0, 0, time.UTC)
	yesterday := now.Add(-24 * time.Hour)
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Quest.Save(ctx, dnfrepo.QuestRecord{
		CharacterID: "1",
		States: map[int64]dnfrepo.QuestState{
			1: {Status: "completed", UpdatedAt: yesterday},
			5: {Status: "completed", UpdatedAt: yesterday},
			6: {Status: "completed", UpdatedAt: yesterday},
		},
	}); err != nil {
		t.Fatal(err)
	}

	// Before the reset, yesterday's rows still hide all three quests.
	stale, found, err := repositories.Quest.Load(ctx, "1")
	if err != nil || !found {
		t.Fatalf("load quest record found=%t err=%v", found, err)
	}
	for _, questID := range []int32{1, 5, 6} {
		if slices.Contains(catalog.QuestList(character, stale).IDs, questID) {
			t.Fatalf("quest %d with a stale row was already acceptable", questID)
		}
	}

	owner, err := NewOwner(repositories)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := owner.ReconcileDailyReset(ctx, catalog, "1", now); err != nil {
		t.Fatal(err)
	}
	reconciled, found, err := repositories.Quest.Load(ctx, "1")
	if err != nil || !found {
		t.Fatalf("reload quest record found=%t err=%v", found, err)
	}
	listed := catalog.QuestList(character, reconciled).IDs
	for _, questID := range []int32{1, 5, 6} {
		if !slices.Contains(listed, questID) {
			t.Fatalf("daily quest %d did not return to the acceptable list after the reset", questID)
		}
	}
}

// TestReconcileDailyResetKeepsRowsWithoutATimestamp protects quest rows that
// predate timestamped states: with no completion time there is no evidence
// they belong to an earlier day, and deleting them would wipe real progress.
func TestReconcileDailyResetKeepsRowsWithoutATimestamp(t *testing.T) {
	ctx := context.Background()
	source := catalogTestSource{
		DefaultList:         "1 `daily.qst`\n",
		"n_quest/daily.qst": questCatalogTestDefinition("[daily]", 1, 90, "[gunner]", ""),
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
	if err := repositories.Quest.Save(ctx, dnfrepo.QuestRecord{
		CharacterID: "1",
		States:      map[int64]dnfrepo.QuestState{1: {Status: "completed"}},
	}); err != nil {
		t.Fatal(err)
	}
	owner, _ := NewOwner(repositories)
	result, err := owner.ReconcileDailyReset(ctx, catalog, "1", time.Date(2026, 9, 10, 8, 0, 0, 0, time.UTC))
	if err != nil || result.Changed {
		t.Fatalf("untimestamped row was reset: result=%+v err=%v", result, err)
	}
}
