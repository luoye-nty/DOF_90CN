package skill

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

func TestSceneOwnerBackfillPersistsSeedAndPreservesCooldowns(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Skill.Save(ctx, dnfrepo.SkillRecord{
		CharacterID: "77",
		Cooldowns:   map[int64]time.Time{46: time.Unix(1234, 0).UTC()},
	}); err != nil {
		t.Fatal(err)
	}
	owner, err := NewSceneOwner(repositories)
	if err != nil {
		t.Fatalf("NewSceneOwner error = %v", err)
	}
	seed := dnfrepo.SkillRecord{
		CharacterID: "77",
		Skills:      map[int64]dnfrepo.SkillState{46: {Level: 1, Enabled: true}},
		Points:      dnfrepo.SkillPointState{TotalSP: 100, RemainingSP: 100, SyncedLevel: 1},
		Layouts:     map[int]dnfrepo.SkillLayout{0: {0: 46}},
	}
	result, persisted, err := owner.Backfill(ctx, BackfillCommand{CharacterID: "77", Seed: seed})
	if err != nil || !persisted {
		t.Fatalf("Backfill result=%+v persisted=%t error=%v", result, persisted, err)
	}
	if result.Skills[46].Level != 1 || result.Cooldowns[46].Unix() != 1234 || result.Layouts[0][0] != 46 {
		t.Fatalf("Backfill result = %+v", result)
	}
	saved, found, err := repositories.Skill.Load(ctx, "77")
	if err != nil || !found || saved.Cooldowns[46].Unix() != 1234 || saved.Skills[46].Level != 1 {
		t.Fatalf("saved=%+v found=%t error=%v", saved, found, err)
	}
}

func TestSceneOwnerSyncPointsPreservesSpentLedger(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	seedSceneSkillRecord(t, ctx, repositories)
	owner, err := NewSceneOwner(repositories)
	if err != nil {
		t.Fatal(err)
	}
	result, persisted, err := owner.SyncPoints(ctx, SyncPointsCommand{
		CharacterID: "77",
		Target: dnfrepo.SkillPointState{
			TotalSP:     150,
			TotalTP:     30,
			SyncedLevel: 20,
		},
	})
	if err != nil || !persisted {
		t.Fatalf("SyncPoints result=%+v persisted=%t error=%v", result, persisted, err)
	}
	want := dnfrepo.SkillPointState{
		TotalSP:     150,
		RemainingSP: 110,
		TotalTP:     30,
		RemainingTP: 20,
		SyncedLevel: 20,
	}
	if result.Points != want {
		t.Fatalf("points=%+v want=%+v", result.Points, want)
	}
	saved, _, _ := repositories.Skill.Load(ctx, "77")
	if saved.Points != want {
		t.Fatalf("saved points=%+v want=%+v", saved.Points, want)
	}
}

func TestSceneOwnerEnsureAndSyncAlternateTreePreservesPrimary(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	seedSceneSkillRecord(t, ctx, repositories)
	owner, err := NewSceneOwner(repositories)
	if err != nil {
		t.Fatal(err)
	}
	before, _, _ := repositories.Skill.Load(ctx, "77")
	primaryBefore, _ := dnfrepo.SkillTree(before, 0)

	created, persisted, err := owner.EnsureTree(ctx, EnsureTreeCommand{
		CharacterID: "77",
		TreeIndex:   1,
		State: dnfrepo.SkillTreeState{
			Skills: map[int64]dnfrepo.SkillState{46: {Level: 1, Enabled: true}},
			Points: dnfrepo.SkillPointState{TotalSP: 100, RemainingSP: 90, TotalTP: 20, RemainingTP: 20, SyncedLevel: 10},
		},
		Layout: dnfrepo.SkillLayout{150: 46},
	})
	if err != nil || !persisted {
		t.Fatalf("EnsureTree result=%+v persisted=%t error=%v", created, persisted, err)
	}
	updated, synced, err := owner.SyncPoints(ctx, SyncPointsCommand{
		CharacterID: "77",
		TreeIndex:   1,
		Target:      dnfrepo.SkillPointState{TotalSP: 150, TotalTP: 30, SyncedLevel: 20},
	})
	if err != nil || !synced {
		t.Fatalf("SyncPoints(tree 1) result=%+v persisted=%t error=%v", updated, synced, err)
	}
	primaryAfter, _ := dnfrepo.SkillTree(updated, 0)
	alternate, found := dnfrepo.SkillTree(updated, 1)
	if !reflect.DeepEqual(primaryAfter, primaryBefore) {
		t.Fatalf("alternate-tree operations changed primary: before=%+v after=%+v", primaryBefore, primaryAfter)
	}
	if !found || alternate.Points.RemainingSP != 140 || alternate.Points.RemainingTP != 30 || updated.Layouts[1][150] != 46 {
		t.Fatalf("alternate tree=%+v layout=%+v", alternate, updated.Layouts[1])
	}
}

func TestSceneOwnerEnsureBaselineRepairsEveryTreeIdempotently(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	seedSceneSkillRecord(t, ctx, repositories)
	record, _, _ := repositories.Skill.Load(ctx, "77")
	record.Skills[49] = dnfrepo.SkillState{Level: 2}
	dnfrepo.SetSkillTree(&record, 1, dnfrepo.SkillTreeState{
		Skills: map[int64]dnfrepo.SkillState{
			46: {Level: 1, Enabled: true},
			49: {Level: 3},
		},
		Points: dnfrepo.SkillPointState{TotalSP: 100, RemainingSP: 25, TotalTP: 20, RemainingTP: 5, SyncedLevel: 10},
	})
	record.Layouts[1] = dnfrepo.SkillLayout{7: 49}
	if err := repositories.Skill.Save(ctx, record); err != nil {
		t.Fatal(err)
	}
	owner, err := NewSceneOwner(repositories)
	if err != nil {
		t.Fatal(err)
	}

	updated, persisted, err := owner.EnsureBaseline(ctx, EnsureBaselineCommand{
		CharacterID: "77",
		TreeCount:   2,
		Levels:      map[uint16]int{46: 1, 48: 1, 49: 1},
	})
	if err != nil || !persisted {
		t.Fatalf("EnsureBaseline result=%+v persisted=%t error=%v", updated, persisted, err)
	}
	primary, primaryFound := dnfrepo.SkillTree(updated, 0)
	alternate, alternateFound := dnfrepo.SkillTree(updated, 1)
	if !primaryFound || !alternateFound || primary.Skills[48].Level != 1 || !primary.Skills[48].Enabled ||
		primary.Skills[49].Level != 2 || !primary.Skills[49].Enabled ||
		alternate.Skills[48].Level != 1 || !alternate.Skills[48].Enabled ||
		alternate.Skills[49].Level != 3 || !alternate.Skills[49].Enabled {
		t.Fatalf("baseline trees primary=%+v alternate=%+v", primary, alternate)
	}
	if primary.Points != record.Points || alternate.Points.RemainingSP != 25 || !reflect.DeepEqual(updated.Layouts[1], dnfrepo.SkillLayout{7: 49}) {
		t.Fatalf("baseline changed points/layouts: primary=%+v alternate=%+v layouts=%+v", primary.Points, alternate.Points, updated.Layouts)
	}

	again, persisted, err := owner.EnsureBaseline(ctx, EnsureBaselineCommand{
		CharacterID: "77",
		TreeCount:   2,
		Levels:      map[uint16]int{46: 1, 48: 1, 49: 1},
	})
	if err != nil || persisted || !reflect.DeepEqual(again, updated) {
		t.Fatalf("idempotent EnsureBaseline result=%+v persisted=%t error=%v", again, persisted, err)
	}
}

func TestSceneOwnerEnsureLayoutPersistsOnceAndKeepsConcurrentExistingLayout(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	seedSceneSkillRecord(t, ctx, repositories)
	owner, err := NewSceneOwner(repositories)
	if err != nil {
		t.Fatal(err)
	}
	want := dnfrepo.SkillLayout{0: 46, 4: 47}
	result, layout, persisted, err := owner.EnsureLayout(ctx, EnsureLayoutCommand{
		CharacterID: "77",
		TreeIndex:   0,
		Build: func(dnfrepo.SkillRecord) (dnfrepo.SkillLayout, error) {
			return want, nil
		},
	})
	if err != nil || !persisted || !reflect.DeepEqual(layout, want) || !reflect.DeepEqual(result.Layouts[0], want) {
		t.Fatalf("EnsureLayout result=%+v layout=%v persisted=%t error=%v", result, layout, persisted, err)
	}

	existing := dnfrepo.SkillLayout{2: 47}
	saved, _, _ := repositories.Skill.Load(ctx, "77")
	saved.Layouts[0] = existing
	if err := repositories.Skill.Save(ctx, saved); err != nil {
		t.Fatal(err)
	}
	result, layout, persisted, err = owner.EnsureLayout(ctx, EnsureLayoutCommand{
		CharacterID: "77",
		TreeIndex:   0,
		Build: func(record dnfrepo.SkillRecord) (dnfrepo.SkillLayout, error) {
			return record.Layouts[0], nil
		},
	})
	if err != nil || persisted || !reflect.DeepEqual(layout, existing) || !reflect.DeepEqual(result.Layouts[0], existing) {
		t.Fatalf("existing result=%+v layout=%v persisted=%t error=%v", result, layout, persisted, err)
	}
}

func TestSceneOwnerEnsureLayoutBuilderFailureDoesNotPersist(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	seedSceneSkillRecord(t, ctx, repositories)
	owner, err := NewSceneOwner(repositories)
	if err != nil {
		t.Fatal(err)
	}
	wantErr := errors.New("reject scene layout")
	_, _, persisted, err := owner.EnsureLayout(ctx, EnsureLayoutCommand{
		CharacterID: "77",
		TreeIndex:   0,
		Build: func(dnfrepo.SkillRecord) (dnfrepo.SkillLayout, error) {
			return nil, wantErr
		},
	})
	if !errors.Is(err, wantErr) || persisted {
		t.Fatalf("EnsureLayout persisted=%t error=%v", persisted, err)
	}
	saved, _, _ := repositories.Skill.Load(ctx, "77")
	if len(saved.Layouts) != 0 {
		t.Fatalf("failed layout persisted: %+v", saved.Layouts)
	}
}

func seedSceneSkillRecord(t *testing.T, ctx context.Context, repositories dnfrepo.Group) {
	t.Helper()
	if err := repositories.Skill.Save(ctx, dnfrepo.SkillRecord{
		CharacterID: "77",
		Skills: map[int64]dnfrepo.SkillState{
			46: {Level: 1, Enabled: true},
			47: {Level: 1, Enabled: true},
		},
		Points: dnfrepo.SkillPointState{
			TotalSP:     100,
			RemainingSP: 60,
			TotalTP:     20,
			RemainingTP: 10,
			SyncedLevel: 10,
		},
		Layouts: map[int]dnfrepo.SkillLayout{},
	}); err != nil {
		t.Fatal(err)
	}
}
