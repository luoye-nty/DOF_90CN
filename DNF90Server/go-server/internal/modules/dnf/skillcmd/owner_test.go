package skillcmd

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
	dnfskill "longheng.io/server/internal/modules/dnf/skill"
)

func TestOwnerPlanReadsSkillState(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	if err := repos.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "31",
		AccountID:   "acc-skill",
	}); err != nil {
		t.Fatalf("save character: %v", err)
	}
	if err := repos.Skill.Save(ctx, dnfrepo.SkillRecord{
		CharacterID: "31",
		Skills: map[int64]dnfrepo.SkillState{
			7: {Level: 3, Enabled: true},
			8: {Level: 1, Enabled: true},
		},
		Cooldowns: map[int64]time.Time{7: time.Unix(100, 0)},
	}); err != nil {
		t.Fatalf("save skill: %v", err)
	}

	owner, err := NewOwner(repos)
	if err != nil {
		t.Fatalf("NewOwner() error = %v", err)
	}
	got, err := owner.Plan(ctx, NewBuyCommand(alignedRequest("acc-skill", 31), BuySkillRequest{
		RawSkillTree: 0,
		Count:        2,
		Entries: []BuySkillEntry{
			{SkillID: 7, LevelDelta: 3},
			{SkillID: 8, RefundFlag: 1, LevelDelta: 1},
		},
	}))
	if err != nil {
		t.Fatalf("Plan() error = %v", err)
	}
	if got.AccountID != "acc-skill" || got.CharacterID != "31" || !got.Known || got.SkillCount != 2 || got.CooldownCount != 1 || got.RefundCount != 1 {
		t.Fatalf("result = %+v", got)
	}
	if len(got.RequestedSkillIDs) != 2 || got.RequestedSkillIDs[0] != 7 || got.RequestedSkillIDs[1] != 8 {
		t.Fatalf("requested skill IDs = %+v", got.RequestedSkillIDs)
	}
}

func TestOwnerApplyTreeSwitchPersistsOppositeTreeAndAcceptsIdempotentReplay(t *testing.T) {
	ctx := context.Background()
	repos := seededSkillOwnerRepositories(t, ctx)
	owner, err := NewOwner(repos)
	if err != nil {
		t.Fatal(err)
	}
	command := NewTreeCommand(alignedRequest("acc-skill", 31), ChangeAnotherSkillTreeRequest{SkillTree: 0})
	result, err := owner.ApplyTreeSwitch(ctx, command)
	if err != nil {
		t.Fatalf("ApplyTreeSwitch() error = %v", err)
	}
	if result.CharacterID != "31" || result.Current != 0 || result.Target != 1 {
		t.Fatalf("result = %+v", result)
	}
	character, ok, err := repos.Character.Load(ctx, "31")
	if err != nil || !ok || character.Stats[currentEXESkillTreeIndexStat] != 1 {
		t.Fatalf("persisted character ok=%t err=%v record=%+v", ok, err, character)
	}
	replayed, err := owner.ApplyTreeSwitch(ctx, command)
	if err != nil {
		t.Fatalf("replayed ApplyTreeSwitch() error = %v", err)
	}
	if replayed.Current != 0 || replayed.Target != 1 {
		t.Fatalf("replayed result = %+v", replayed)
	}
	character, _, _ = repos.Character.Load(ctx, "31")
	if character.Stats[currentEXESkillTreeIndexStat] != 1 {
		t.Fatalf("replayed switch persisted tree = %d", character.Stats[currentEXESkillTreeIndexStat])
	}
}

func TestOwnerApplyTreeSwitchRequiresOwnedTreeMarker(t *testing.T) {
	ctx := context.Background()
	repos := seededSkillOwnerRepositories(t, ctx)
	character, _, _ := repos.Character.Load(ctx, "31")
	delete(character.Stats, currentEXESkillTreeIndexStat)
	if err := repos.Character.Save(ctx, character); err != nil {
		t.Fatal(err)
	}
	owner, err := NewOwner(repos)
	if err != nil {
		t.Fatal(err)
	}
	_, err = owner.ApplyTreeSwitch(ctx, NewTreeCommand(alignedRequest("acc-skill", 31), ChangeAnotherSkillTreeRequest{SkillTree: 0}))
	if !errors.Is(err, ErrSkillTreeUnavailable) {
		t.Fatalf("ApplyTreeSwitch() error = %v, want %v", err, ErrSkillTreeUnavailable)
	}
}

func TestOwnerPlanRejectsMissingCharacter(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	owner, err := NewOwner(repos)
	if err != nil {
		t.Fatalf("NewOwner() error = %v", err)
	}
	_, err = owner.Plan(ctx, NewSlotCommand(alignedRequest("acc-skill", 31), ChangeSkillSlotRequest{SkillTree: 0, From: 1, To: 2}))
	if !errors.Is(err, ErrCharacterNotFound) {
		t.Fatalf("Plan() error = %v, want ErrCharacterNotFound", err)
	}
}

func TestOwnerApplyBuyAllowsProvenTreeZeroAndCommitsAtomically(t *testing.T) {
	ctx := context.Background()
	repos := seededSkillOwnerRepositories(t, ctx)
	owner, err := NewOwner(repos, OwnerOptions{
		Catalog:       skillRuleCatalog(t),
		InitialLevels: map[uint16]int{46: 1},
	})
	if err != nil {
		t.Fatalf("NewOwner() error = %v", err)
	}

	result, err := owner.ApplyBuy(ctx, NewBuyCommand(alignedRequest("acc-skill", 31), BuySkillRequest{
		RawSkillTree: 0,
		Count:        2,
		FinalMode:    3,
		Entries: []BuySkillEntry{
			// The dependent skill can appear first because prerequisites are
			// validated against the final state of the current EXE batch.
			{SkillID: 47, LevelDelta: 1},
			{SkillID: 46, LevelDelta: 1},
		},
	}))
	if err != nil {
		t.Fatalf("ApplyBuy() error = %v", err)
	}
	if result.Points.RemainingSP != 50 || result.SkillTree != 0 || result.FinalMode != 3 || len(result.Entries) != 2 {
		t.Fatalf("mutation result = %+v", result)
	}
	if result.Entries[0].Slot != 1 || result.Entries[1].Slot != 0 {
		t.Fatalf("tree-zero slots = %+v", result.Entries)
	}
	record, ok, err := repos.Skill.Load(ctx, "31")
	if err != nil || !ok {
		t.Fatalf("load skill ok=%t err=%v", ok, err)
	}
	if record.Skills[46].Level != 2 || record.Skills[47].Level != 1 || record.Points.RemainingSP != 50 {
		t.Fatalf("persisted mutation = %+v", record)
	}
	if record.Layouts[0][0] != 46 || record.Layouts[0][1] != 47 {
		t.Fatalf("persisted tree-zero layout = %+v", record.Layouts)
	}
}

func TestOwnerApplySlotSwapsPersistedLayoutAtomically(t *testing.T) {
	ctx := context.Background()
	repos := seededSkillOwnerRepositories(t, ctx)
	record, _, _ := repos.Skill.Load(ctx, "31")
	record.Skills[47] = dnfrepo.SkillState{Level: 1, Enabled: true}
	record.Layouts = map[int]dnfrepo.SkillLayout{0: {0: 46, 1: 47}}
	if err := repos.Skill.Save(ctx, record); err != nil {
		t.Fatalf("save skill layout: %v", err)
	}
	owner, err := NewOwner(repos, OwnerOptions{Catalog: skillRuleCatalog(t), InitialLevels: map[uint16]int{46: 1}})
	if err != nil {
		t.Fatalf("NewOwner() error = %v", err)
	}

	result, err := owner.ApplySlot(ctx, NewSlotCommand(alignedRequest("acc-skill", 31), ChangeSkillSlotRequest{
		SkillTree: 0, From: 0, To: 1, ContextIndex: -1, Mode: 2,
	}))
	if err != nil {
		t.Fatalf("ApplySlot() error = %v", err)
	}
	if result.FromSkillID != 46 || result.ToSkillID != 47 || !result.ToOccupied {
		t.Fatalf("result = %+v", result)
	}
	persisted, ok, err := repos.Skill.Load(ctx, "31")
	if err != nil || !ok {
		t.Fatalf("load persisted layout ok=%t err=%v", ok, err)
	}
	if persisted.Layouts[0][0] != 47 || persisted.Layouts[0][1] != 46 {
		t.Fatalf("persisted layout = %+v", persisted.Layouts[0])
	}
}

func TestOwnerApplyResetRestoresPVFInitialStateAndRefundsPointsAtomically(t *testing.T) {
	ctx := context.Background()
	repos := seededSkillOwnerRepositories(t, ctx)
	record, _, _ := repos.Skill.Load(ctx, "31")
	record.Skills[46] = dnfrepo.SkillState{Level: 3, Enabled: true}
	record.Skills[47] = dnfrepo.SkillState{Level: 1, Enabled: true}
	record.Points.RemainingSP = 10
	record.Points.RemainingTP = 4
	record.Layouts = map[int]dnfrepo.SkillLayout{0: {0: 47, 1: 46}, 1: {0: 47}}
	cooldown := time.Unix(1234, 0).UTC()
	record.Cooldowns = map[int64]time.Time{46: cooldown}
	if err := repos.Skill.Save(ctx, record); err != nil {
		t.Fatal(err)
	}
	owner, err := NewOwner(repos, OwnerOptions{
		Catalog:       skillRuleCatalog(t),
		InitialLevels: map[uint16]int{46: 1},
		PointBaseline: &dnfrepo.SkillPointState{TotalSP: 100, RemainingSP: 100, TotalTP: 10, RemainingTP: 10, SyncedLevel: 10},
	})
	if err != nil {
		t.Fatal(err)
	}

	result, err := owner.ApplyReset(ctx, NewResetCommand(alignedRequest("acc-skill", 31), SkillInitRequest{SkillTree: 0, Mode: 0}))
	if err != nil {
		t.Fatalf("ApplyReset() error = %v", err)
	}
	if result.SkillCount != 1 || result.Points.RemainingSP != 100 || result.Points.RemainingTP != 10 {
		t.Fatalf("result = %+v", result)
	}
	persisted, ok, err := repos.Skill.Load(ctx, "31")
	if err != nil || !ok {
		t.Fatalf("load reset record ok=%t err=%v", ok, err)
	}
	if len(persisted.Skills) != 1 || persisted.Skills[46].Level != 1 {
		t.Fatalf("reset skills = %+v", persisted.Skills)
	}
	if persisted.Points != result.Points || len(persisted.Layouts) != 2 || persisted.Layouts[0][0] != 46 || persisted.Layouts[1][0] != 47 {
		t.Fatalf("reset points/layouts = %+v", persisted)
	}
	if !persisted.Cooldowns[46].Equal(cooldown) {
		t.Fatalf("reset overwrote cooldowns = %+v", persisted.Cooldowns)
	}
	if persisted.UpdatedAt.IsZero() {
		t.Fatal("reset did not update mutation timestamp")
	}
}

func TestOwnerMutationsKeepSkillTreesIndependent(t *testing.T) {
	ctx := context.Background()
	repos := seededSkillOwnerRepositories(t, ctx)
	record, _, err := repos.Skill.Load(ctx, "31")
	if err != nil {
		t.Fatal(err)
	}
	record.Layouts = map[int]dnfrepo.SkillLayout{
		0: {0: 46},
		1: {150: 46},
	}
	record.Trees = map[int]dnfrepo.SkillTreeState{
		1: {
			Skills: map[int64]dnfrepo.SkillState{46: {Level: 1, Enabled: true}},
			Points: record.Points,
		},
	}
	if err := repos.Skill.Save(ctx, record); err != nil {
		t.Fatal(err)
	}
	treeZeroBefore, _ := dnfrepo.SkillTree(record, 0)
	layoutZeroBefore := dnfrepo.CloneSkill(record).Layouts[0]
	owner, err := NewOwner(repos, OwnerOptions{
		Catalog:       skillRuleCatalog(t),
		InitialLevels: map[uint16]int{46: 1},
		PointBaseline: &dnfrepo.SkillPointState{TotalSP: 100, RemainingSP: 100, TotalTP: 10, RemainingTP: 10, SyncedLevel: 10},
	})
	if err != nil {
		t.Fatal(err)
	}

	buy, err := owner.ApplyBuy(ctx, NewBuyCommand(alignedRequest("acc-skill", 31), BuySkillRequest{
		RawSkillTree: 1,
		Count:        1,
		Entries:      []BuySkillEntry{{SkillID: 46, LevelDelta: 1}},
	}))
	if err != nil {
		t.Fatalf("ApplyBuy(tree 1): %v", err)
	}
	if buy.Points.RemainingSP != 80 || len(buy.Entries) != 1 || buy.Entries[0].Slot != 150 {
		t.Fatalf("tree 1 buy result = %+v", buy)
	}

	if _, err := owner.ApplySlot(ctx, NewSlotCommand(alignedRequest("acc-skill", 31), ChangeSkillSlotRequest{
		SkillTree: 1, From: 150, To: 198, ContextIndex: -1, Mode: 0,
	})); err != nil {
		t.Fatalf("ApplySlot(tree 1): %v", err)
	}
	afterSlot, _, err := repos.Skill.Load(ctx, "31")
	if err != nil {
		t.Fatal(err)
	}
	treeZeroAfterSlot, _ := dnfrepo.SkillTree(afterSlot, 0)
	if !reflect.DeepEqual(treeZeroAfterSlot, treeZeroBefore) || !reflect.DeepEqual(afterSlot.Layouts[0], layoutZeroBefore) {
		t.Fatalf("tree 1 mutations changed tree 0: before=%+v/%+v after=%+v/%+v", treeZeroBefore, layoutZeroBefore, treeZeroAfterSlot, afterSlot.Layouts[0])
	}
	treeOneAfterSlot, found := dnfrepo.SkillTree(afterSlot, 1)
	if !found || treeOneAfterSlot.Skills[46].Level != 2 || treeOneAfterSlot.Points.RemainingSP != 80 || afterSlot.Layouts[1][198] != 46 {
		t.Fatalf("tree 1 mutation was not persisted independently: tree=%+v layout=%+v", treeOneAfterSlot, afterSlot.Layouts[1])
	}

	if _, err := owner.ApplyReset(ctx, NewResetCommand(alignedRequest("acc-skill", 31), SkillInitRequest{SkillTree: 1, Mode: 0})); err != nil {
		t.Fatalf("ApplyReset(tree 1): %v", err)
	}
	afterReset, _, err := repos.Skill.Load(ctx, "31")
	if err != nil {
		t.Fatal(err)
	}
	treeZeroAfterReset, _ := dnfrepo.SkillTree(afterReset, 0)
	treeOneAfterReset, found := dnfrepo.SkillTree(afterReset, 1)
	if !reflect.DeepEqual(treeZeroAfterReset, treeZeroBefore) || !reflect.DeepEqual(afterReset.Layouts[0], layoutZeroBefore) {
		t.Fatalf("tree 1 reset changed tree 0: before=%+v/%+v after=%+v/%+v", treeZeroBefore, layoutZeroBefore, treeZeroAfterReset, afterReset.Layouts[0])
	}
	if !found || treeOneAfterReset.Skills[46].Level != 1 || treeOneAfterReset.Points.RemainingSP != 100 || afterReset.Layouts[1][0] != 46 {
		t.Fatalf("tree 1 reset state = %+v layout=%+v", treeOneAfterReset, afterReset.Layouts[1])
	}
}

func TestOwnerApplyBuyAutoAssignsTreeOneActiveSkillToQuickbar(t *testing.T) {
	ctx := context.Background()
	repos := seededSkillOwnerRepositories(t, ctx)
	record, _, err := repos.Skill.Load(ctx, "31")
	if err != nil {
		t.Fatal(err)
	}
	record.Layouts = map[int]dnfrepo.SkillLayout{
		0: {0: 46},
		1: {150: 46},
	}
	record.Trees = map[int]dnfrepo.SkillTreeState{
		1: {
			Skills: map[int64]dnfrepo.SkillState{46: {Level: 1, Enabled: true}},
			Points: record.Points,
		},
	}
	if err := repos.Skill.Save(ctx, record); err != nil {
		t.Fatal(err)
	}
	treeZeroBefore, _ := dnfrepo.SkillTree(record, 0)
	layoutZeroBefore := dnfrepo.CloneSkill(record).Layouts[0]
	owner, err := NewOwner(repos, OwnerOptions{
		Catalog:       skillRuleCatalog(t),
		InitialLevels: map[uint16]int{46: 1},
		PointBaseline: &dnfrepo.SkillPointState{TotalSP: 100, RemainingSP: 100, TotalTP: 10, RemainingTP: 10, SyncedLevel: 10},
	})
	if err != nil {
		t.Fatal(err)
	}

	result, err := owner.ApplyBuy(ctx, NewBuyCommand(alignedRequest("acc-skill", 31), BuySkillRequest{
		RawSkillTree: 1,
		Count:        2,
		Entries: []BuySkillEntry{
			{SkillID: 47, LevelDelta: 1},
			{SkillID: 46, LevelDelta: 1},
		},
	}))
	if err != nil {
		t.Fatalf("ApplyBuy(tree 1): %v", err)
	}
	if len(result.Entries) != 2 || result.Entries[0].SkillID != 47 || result.Entries[0].Slot != 0 || result.Entries[1].Slot != 150 {
		t.Fatalf("tree 1 buy slots = %+v", result.Entries)
	}
	persisted, _, err := repos.Skill.Load(ctx, "31")
	if err != nil {
		t.Fatal(err)
	}
	treeZeroAfter, _ := dnfrepo.SkillTree(persisted, 0)
	if !reflect.DeepEqual(treeZeroAfter, treeZeroBefore) || !reflect.DeepEqual(persisted.Layouts[0], layoutZeroBefore) {
		t.Fatalf("tree 1 auto assignment changed tree 0: before=%+v/%+v after=%+v/%+v", treeZeroBefore, layoutZeroBefore, treeZeroAfter, persisted.Layouts[0])
	}
	if persisted.Layouts[1][0] != 47 || persisted.Layouts[1][150] != 46 {
		t.Fatalf("persisted tree 1 layout = %+v", persisted.Layouts[1])
	}
}

func TestOwnerApplySlotMovesIntoEmptyQuickSlot(t *testing.T) {
	ctx := context.Background()
	repos := seededSkillOwnerRepositories(t, ctx)
	record, _, _ := repos.Skill.Load(ctx, "31")
	record.Layouts = map[int]dnfrepo.SkillLayout{0: {0: 46}}
	if err := repos.Skill.Save(ctx, record); err != nil {
		t.Fatalf("save skill layout: %v", err)
	}
	owner, _ := NewOwner(repos, OwnerOptions{Catalog: skillRuleCatalog(t), InitialLevels: map[uint16]int{46: 1}})
	result, err := owner.ApplySlot(ctx, NewSlotCommand(alignedRequest("acc-skill", 31), ChangeSkillSlotRequest{
		SkillTree: 0, From: 0, To: 3, ContextIndex: -1, Mode: 0,
	}))
	if err != nil {
		t.Fatalf("ApplySlot() error = %v", err)
	}
	if result.ToOccupied {
		t.Fatalf("result = %+v, want empty destination", result)
	}
	persisted, _, _ := repos.Skill.Load(ctx, "31")
	if _, exists := persisted.Layouts[0][0]; exists || persisted.Layouts[0][3] != 46 {
		t.Fatalf("persisted layout = %+v", persisted.Layouts[0])
	}
}

func TestOwnerApplySlotUsesClientSourceSlotVerbatim(t *testing.T) {
	ctx := context.Background()
	repos := seededSkillOwnerRepositories(t, ctx)
	record, _, _ := repos.Skill.Load(ctx, "31")
	record.Layouts = map[int]dnfrepo.SkillLayout{0: {107: 46}}
	if err := repos.Skill.Save(ctx, record); err != nil {
		t.Fatalf("save skill layout: %v", err)
	}
	owner, _ := NewOwner(repos, OwnerOptions{Catalog: skillRuleCatalog(t), InitialLevels: map[uint16]int{46: 1}})

	result, err := owner.ApplySlot(ctx, NewSlotCommand(alignedRequest("acc-skill", 31), ChangeSkillSlotRequest{
		SkillTree: 0, From: 107, To: 198, ContextIndex: -1, Mode: 0,
	}))
	if err != nil {
		t.Fatalf("ApplySlot() source-slot error = %v", err)
	}
	if result.From != 107 || result.To != 198 || result.FromSkillID != 46 || result.ToOccupied {
		t.Fatalf("result = %+v", result)
	}
	persisted, _, _ := repos.Skill.Load(ctx, "31")
	if _, exists := persisted.Layouts[0][107]; exists || persisted.Layouts[0][198] != 46 {
		t.Fatalf("persisted layout = %+v", persisted.Layouts[0])
	}
}

func TestOwnerApplySlotUsesBothQuickbarRowsVerbatim(t *testing.T) {
	targets := []byte{0, 1, 2, 3, 4, 5, 198, 199, 200, 201, 202, 203}
	for _, target := range targets {
		t.Run(fmt.Sprintf("target_%d", target), func(t *testing.T) {
			ctx := context.Background()
			repos := seededSkillOwnerRepositories(t, ctx)
			record, _, _ := repos.Skill.Load(ctx, "31")
			record.Skills[49] = dnfrepo.SkillState{Level: 1, Enabled: true}
			record.Layouts = map[int]dnfrepo.SkillLayout{0: {7: 49}}
			if err := repos.Skill.Save(ctx, record); err != nil {
				t.Fatalf("save live-shaped layout: %v", err)
			}
			owner, err := NewOwner(repos, OwnerOptions{Catalog: skillRuleCatalog(t), InitialLevels: map[uint16]int{46: 1}})
			if err != nil {
				t.Fatal(err)
			}

			result, err := owner.ApplySlot(ctx, NewSlotCommand(alignedRequest("acc-skill", 31), ChangeSkillSlotRequest{
				SkillTree: 0, From: 7, To: target, ContextIndex: -1, Mode: 0,
			}))
			if err != nil {
				t.Fatalf("ApplySlot() live-shaped request: %v", err)
			}
			if result.From != 7 || result.To != target || result.FromSkillID != 49 {
				t.Fatalf("result = %+v", result)
			}
			persisted, _, _ := repos.Skill.Load(ctx, "31")
			if persisted.Layouts[0][int(target)] != 49 {
				t.Fatalf("target=%d layout=%+v", target, persisted.Layouts[0])
			}
			if target == 0 {
				if persisted.Layouts[0][7] != 46 {
					t.Fatalf("source slot 7 did not receive displaced skill: %+v", persisted.Layouts[0])
				}
			} else if int(target) != 7 {
				if _, exists := persisted.Layouts[0][7]; exists {
					t.Fatalf("source slot 7 remained occupied after move: %+v", persisted.Layouts[0])
				}
			}
		})
	}
}

func TestOwnerApplySlotAcceptsRepeatedCurrentEXEQuickbarDrags(t *testing.T) {
	ctx := context.Background()
	repos := seededSkillOwnerRepositories(t, ctx)
	record, _, _ := repos.Skill.Load(ctx, "31")
	record.Skills[49] = dnfrepo.SkillState{Level: 1, Enabled: true}
	record.Skills[47] = dnfrepo.SkillState{Level: 1, Enabled: true}
	record.Layouts = map[int]dnfrepo.SkillLayout{0: {1: 46, 7: 49, 8: 47}}
	if err := repos.Skill.Save(ctx, record); err != nil {
		t.Fatalf("save current EXE layout: %v", err)
	}
	owner, err := NewOwner(repos, OwnerOptions{Catalog: skillRuleCatalog(t), InitialLevels: map[uint16]int{46: 1}})
	if err != nil {
		t.Fatal(err)
	}
	for _, request := range []ChangeSkillSlotRequest{
		{SkillTree: 0, From: 7, To: 1, ContextIndex: -1, Mode: 0},
		{SkillTree: 0, From: 8, To: 199, ContextIndex: -1, Mode: 0},
	} {
		if _, err := owner.ApplySlot(ctx, NewSlotCommand(alignedRequest("acc-skill", 31), request)); err != nil {
			t.Fatalf("ApplySlot(%d -> %d): %v", request.From, request.To, err)
		}
	}
	persisted, _, err := repos.Skill.Load(ctx, "31")
	if err != nil {
		t.Fatal(err)
	}
	if persisted.Layouts[0][1] != 49 || persisted.Layouts[0][7] != 46 || persisted.Layouts[0][199] != 47 {
		t.Fatalf("repeated current EXE drags layout=%+v", persisted.Layouts[0])
	}
}

func TestOwnerApplySlotMapsObservedTreeDragSourceOnlyIntoQuickbar(t *testing.T) {
	ctx := context.Background()
	repos := seededSkillOwnerRepositories(t, ctx)
	record, _, _ := repos.Skill.Load(ctx, "31")
	record.Skills[49] = dnfrepo.SkillState{Level: 1, Enabled: true}
	record.Skills[47] = dnfrepo.SkillState{Level: 1, Enabled: true}
	record.Layouts = map[int]dnfrepo.SkillLayout{0: {3: 49, 56: 46, 58: 47}}
	if err := repos.Skill.Save(ctx, record); err != nil {
		t.Fatalf("save tree layout: %v", err)
	}
	owner, err := NewOwner(repos, OwnerOptions{Catalog: skillRuleCatalog(t), InitialLevels: map[uint16]int{46: 1}})
	if err != nil {
		t.Fatal(err)
	}

	for _, request := range []ChangeSkillSlotRequest{
		{SkillTree: 0, From: 57, To: 199, ContextIndex: -1, Mode: 0},
		{SkillTree: 0, From: 60, To: 3, ContextIndex: -1, Mode: 0},
	} {
		result, err := owner.ApplySlot(ctx, NewSlotCommand(alignedRequest("acc-skill", 31), request))
		if err != nil {
			t.Fatalf("ApplySlot(%d -> %d): %v", request.From, request.To, err)
		}
		if result.From != request.From || result.To != request.To {
			t.Fatalf("ACK coordinates changed: result=%+v request=%+v", result, request)
		}
		ack, err := BuildChangeSkillSlotSuccess(result)
		if err != nil {
			t.Fatalf("BuildChangeSkillSlotSuccess(%+v): %v", result, err)
		}
		wantACK := []byte{1, request.SkillTree, request.From, request.To}
		if !reflect.DeepEqual(ack, wantACK) {
			t.Fatalf("ACK=%x want=%x", ack, wantACK)
		}
	}
	persisted, _, err := repos.Skill.Load(ctx, "31")
	if err != nil {
		t.Fatal(err)
	}
	if persisted.Layouts[0][199] != 46 || persisted.Layouts[0][3] != 47 || persisted.Layouts[0][60] != 49 {
		t.Fatalf("tree-to-quickbar layout=%+v", persisted.Layouts[0])
	}
	if _, exists := persisted.Layouts[0][56]; exists {
		t.Fatalf("first persisted tree source remained: %+v", persisted.Layouts[0])
	}
	if _, exists := persisted.Layouts[0][58]; exists {
		t.Fatalf("second persisted tree source remained: %+v", persisted.Layouts[0])
	}
}

func TestOwnerApplySlotConvergesTwoSlotTreeSourceOffset(t *testing.T) {
	ctx := context.Background()
	repos := seededSkillOwnerRepositories(t, ctx)
	record, _, _ := repos.Skill.Load(ctx, "31")
	record.Skills[47] = dnfrepo.SkillState{Level: 1, Enabled: true}
	record.Layouts = map[int]dnfrepo.SkillLayout{0: {2: 46, 56: 47}}
	if err := repos.Skill.Save(ctx, record); err != nil {
		t.Fatalf("save drifted layout: %v", err)
	}
	owner, err := NewOwner(repos, OwnerOptions{Catalog: skillRuleCatalog(t), InitialLevels: map[uint16]int{46: 1}})
	if err != nil {
		t.Fatal(err)
	}

	result, err := owner.ApplySlot(ctx, NewSlotCommand(alignedRequest("acc-skill", 31), ChangeSkillSlotRequest{
		SkillTree: 0, From: 58, To: 2, ContextIndex: -1, Mode: 0,
	}))
	if err != nil {
		t.Fatalf("ApplySlot(58 -> 2): %v", err)
	}
	if result.From != 58 || result.FromSkillID != 47 || result.ToSkillID != 46 {
		t.Fatalf("result=%+v", result)
	}
	persisted, _, err := repos.Skill.Load(ctx, "31")
	if err != nil {
		t.Fatal(err)
	}
	if persisted.Layouts[0][2] != 47 || persisted.Layouts[0][58] != 46 {
		t.Fatalf("converged layout=%+v", persisted.Layouts[0])
	}
	if _, exists := persisted.Layouts[0][56]; exists {
		t.Fatalf("stale source remained: %+v", persisted.Layouts[0])
	}
}

func TestOwnerApplySlotConvergesClientAndPersistedSourceCoordinates(t *testing.T) {
	ctx := context.Background()
	repos := seededSkillOwnerRepositories(t, ctx)
	record, _, _ := repos.Skill.Load(ctx, "31")
	record.Skills[47] = dnfrepo.SkillState{Level: 1, Enabled: true}
	record.Skills[49] = dnfrepo.SkillState{Level: 1, Enabled: true}
	record.Layouts = map[int]dnfrepo.SkillLayout{0: {3: 49, 58: 47, 202: 46}}
	if err := repos.Skill.Save(ctx, record); err != nil {
		t.Fatalf("save drifted layout: %v", err)
	}
	owner, err := NewOwner(repos, OwnerOptions{Catalog: skillRuleCatalog(t), InitialLevels: map[uint16]int{46: 1}})
	if err != nil {
		t.Fatal(err)
	}

	result, err := owner.ApplySlot(ctx, NewSlotCommand(alignedRequest("acc-skill", 31), ChangeSkillSlotRequest{
		SkillTree: 0, From: 60, To: 3, ContextIndex: -1, Mode: 0,
	}))
	if err != nil {
		t.Fatalf("ApplySlot(60 -> 3): %v", err)
	}
	if result.From != 60 || result.FromSkillID != 47 || result.ToSkillID != 49 {
		t.Fatalf("result=%+v", result)
	}
	persisted, _, err := repos.Skill.Load(ctx, "31")
	if err != nil {
		t.Fatal(err)
	}
	if persisted.Layouts[0][3] != 47 || persisted.Layouts[0][60] != 49 {
		t.Fatalf("converged layout=%+v", persisted.Layouts[0])
	}
	if _, exists := persisted.Layouts[0][58]; exists {
		t.Fatalf("stale source remained: %+v", persisted.Layouts[0])
	}
}

func TestOwnerApplySlotKeepsNonQuickbarSourcesLiteral(t *testing.T) {
	ctx := context.Background()
	repos := seededSkillOwnerRepositories(t, ctx)
	record, _, _ := repos.Skill.Load(ctx, "31")
	record.Layouts = map[int]dnfrepo.SkillLayout{0: {56: 46}}
	if err := repos.Skill.Save(ctx, record); err != nil {
		t.Fatalf("save tree layout: %v", err)
	}
	owner, err := NewOwner(repos, OwnerOptions{Catalog: skillRuleCatalog(t), InitialLevels: map[uint16]int{46: 1}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = owner.ApplySlot(ctx, NewSlotCommand(alignedRequest("acc-skill", 31), ChangeSkillSlotRequest{
		SkillTree: 0, From: 57, To: 58, ContextIndex: -1, Mode: 0,
	}))
	if !errors.Is(err, ErrSkillSlot) {
		t.Fatalf("ApplySlot() error = %v, want empty literal source", err)
	}
}

func TestOwnerApplySlotRejectsUnprovenWriterFieldsWithoutMutation(t *testing.T) {
	tests := []struct {
		name string
		req  ChangeSkillSlotRequest
		err  error
	}{
		{name: "tree", req: ChangeSkillSlotRequest{SkillTree: 2, From: 0, To: 1, ContextIndex: -1, Mode: 0}, err: ErrSkillTree},
		{name: "retired_normal_seventh", req: ChangeSkillSlotRequest{SkillTree: 0, From: 0, To: 204, ContextIndex: -1, Mode: 0}, err: ErrSkillSlot},
		{name: "retired_extension_seventh", req: ChangeSkillSlotRequest{SkillTree: 0, From: 0, To: 205, ContextIndex: -1, Mode: 0}, err: ErrSkillSlot},
		{name: "reserved_destination", req: ChangeSkillSlotRequest{SkillTree: 0, From: 0, To: 138, ContextIndex: -1, Mode: 0}, err: ErrSkillSlot},
		{name: "context", req: ChangeSkillSlotRequest{SkillTree: 0, From: 0, To: 1, ContextIndex: 3, Mode: 0}, err: ErrSkillSlotContext},
		{name: "mode", req: ChangeSkillSlotRequest{SkillTree: 0, From: 0, To: 1, ContextIndex: -1, Mode: 1}, err: ErrSkillSlotMode},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			repos := seededSkillOwnerRepositories(t, ctx)
			before, _, _ := repos.Skill.Load(ctx, "31")
			unitOfWork := &countingCharacterSkillUnitOfWork{delegate: repos.CharacterSkills}
			repos.CharacterSkills = unitOfWork
			owner, _ := NewOwner(repos, OwnerOptions{Catalog: skillRuleCatalog(t), InitialLevels: map[uint16]int{46: 1}})
			_, err := owner.ApplySlot(ctx, NewSlotCommand(alignedRequest("acc-skill", 31), tt.req))
			if !errors.Is(err, tt.err) {
				t.Fatalf("ApplySlot() error = %v, want %v", err, tt.err)
			}
			if unitOfWork.calls != 0 {
				t.Fatalf("unit of work calls = %d, want 0", unitOfWork.calls)
			}
			after, _, _ := repos.Skill.Load(ctx, "31")
			if !reflect.DeepEqual(after, before) {
				t.Fatalf("rejected request mutated record: before=%+v after=%+v", before, after)
			}
		})
	}
}

func TestOwnerApplyBuyRejectsUnprovenTreesBeforeUnitOfWork(t *testing.T) {
	tests := []struct {
		name string
		tree byte
	}{
		{name: "unknown_tree", tree: 2},
		{name: "sentinel", tree: 0xff},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			repos := seededSkillOwnerRepositories(t, ctx)
			before, ok, err := repos.Skill.Load(ctx, "31")
			if err != nil || !ok {
				t.Fatalf("load skill before rejection ok=%t err=%v", ok, err)
			}
			unitOfWork := &countingCharacterSkillUnitOfWork{delegate: repos.CharacterSkills}
			repos.CharacterSkills = unitOfWork
			owner, err := NewOwner(repos, OwnerOptions{
				Catalog:       skillRuleCatalog(t),
				InitialLevels: map[uint16]int{46: 1},
			})
			if err != nil {
				t.Fatalf("NewOwner() error = %v", err)
			}

			_, err = owner.ApplyBuy(ctx, NewBuyCommand(alignedRequest("acc-skill", 31), BuySkillRequest{
				RawSkillTree: tt.tree,
				Count:        1,
				Entries:      []BuySkillEntry{{SkillID: 46, LevelDelta: 1}},
			}))
			if !errors.Is(err, ErrSkillTree) {
				t.Fatalf("ApplyBuy() error = %v, want ErrSkillTree", err)
			}
			if unitOfWork.calls != 0 {
				t.Fatalf("CharacterSkillUnitOfWork calls = %d, want 0", unitOfWork.calls)
			}
			after, ok, loadErr := repos.Skill.Load(ctx, "31")
			if loadErr != nil || !ok {
				t.Fatalf("load skill after rejection ok=%t err=%v", ok, loadErr)
			}
			if !reflect.DeepEqual(after, before) {
				t.Fatalf("rejected tree mutated skill state: before=%+v after=%+v", before, after)
			}
		})
	}
}

func TestOwnerApplyBuyRejectsRefundBelowPVFInitialFloor(t *testing.T) {
	ctx := context.Background()
	repos := seededSkillOwnerRepositories(t, ctx)
	owner, err := NewOwner(repos, OwnerOptions{
		Catalog:       skillRuleCatalog(t),
		InitialLevels: map[uint16]int{46: 1},
	})
	if err != nil {
		t.Fatalf("NewOwner() error = %v", err)
	}

	_, err = owner.ApplyBuy(ctx, NewBuyCommand(alignedRequest("acc-skill", 31), BuySkillRequest{
		Count:   1,
		Entries: []BuySkillEntry{{SkillID: 46, RefundFlag: 1, LevelDelta: 1}},
	}))
	if !errors.Is(err, ErrSkillLevel) {
		t.Fatalf("ApplyBuy() error = %v, want ErrSkillLevel", err)
	}
	record, _, _ := repos.Skill.Load(ctx, "31")
	if record.Skills[46].Level != 1 || record.Points.RemainingSP != 100 {
		t.Fatalf("rejected mutation changed state: %+v", record)
	}
}

func TestOwnerApplyBuyUsesSpecialPurchaseCostForTPSkill(t *testing.T) {
	ctx := context.Background()
	repos := seededSkillOwnerRepositories(t, ctx)
	owner, err := NewOwner(repos, OwnerOptions{Catalog: skillRuleCatalog(t), InitialLevels: map[uint16]int{}})
	if err != nil {
		t.Fatalf("NewOwner() error = %v", err)
	}

	result, err := owner.ApplyBuy(ctx, NewBuyCommand(alignedRequest("acc-skill", 31), BuySkillRequest{
		Count:   1,
		Entries: []BuySkillEntry{{SkillID: 48, LevelDelta: 2}},
	}))
	if err != nil {
		t.Fatalf("ApplyBuy() error = %v", err)
	}
	if result.Points.RemainingTP != 6 || result.Points.RemainingSP != 100 || len(result.Entries) != 1 || !result.Entries[0].TP {
		t.Fatalf("TP mutation result = %+v", result)
	}
	if result.Entries[0].Slot != 1 {
		t.Fatalf("tree-zero active slot = %d, want 1", result.Entries[0].Slot)
	}
}

func TestOwnerApplyBuyRejectsStalePointLedger(t *testing.T) {
	ctx := context.Background()
	repos := seededSkillOwnerRepositories(t, ctx)
	record, _, _ := repos.Skill.Load(ctx, "31")
	record.Points.SyncedLevel = 9
	if err := repos.Skill.Save(ctx, record); err != nil {
		t.Fatalf("save stale ledger: %v", err)
	}
	owner, err := NewOwner(repos, OwnerOptions{Catalog: skillRuleCatalog(t), InitialLevels: map[uint16]int{}})
	if err != nil {
		t.Fatalf("NewOwner() error = %v", err)
	}

	_, err = owner.ApplyBuy(ctx, NewBuyCommand(alignedRequest("acc-skill", 31), BuySkillRequest{
		Count:   1,
		Entries: []BuySkillEntry{{SkillID: 46, LevelDelta: 1}},
	}))
	if !errors.Is(err, ErrSkillPointLedger) {
		t.Fatalf("ApplyBuy() error = %v, want ErrSkillPointLedger", err)
	}
}

func TestOwnerApplyBuyHydratesLegacyLedgerFromPVFBaseline(t *testing.T) {
	ctx := context.Background()
	repos := seededSkillOwnerRepositories(t, ctx)
	record, _, _ := repos.Skill.Load(ctx, "31")
	record.Skills[46] = dnfrepo.SkillState{Level: 2, Enabled: true}
	record.Points = dnfrepo.SkillPointState{}
	if err := repos.Skill.Save(ctx, record); err != nil {
		t.Fatalf("save legacy ledger: %v", err)
	}
	owner, err := NewOwner(repos, OwnerOptions{
		Catalog:       skillRuleCatalog(t),
		InitialLevels: map[uint16]int{46: 1},
		PointBaseline: &dnfrepo.SkillPointState{TotalSP: 100, RemainingSP: 100, TotalTP: 10, RemainingTP: 10, SyncedLevel: 10},
	})
	if err != nil {
		t.Fatalf("NewOwner() error = %v", err)
	}

	result, err := owner.ApplyBuy(ctx, NewBuyCommand(alignedRequest("acc-skill", 31), BuySkillRequest{
		Count:   1,
		Entries: []BuySkillEntry{{SkillID: 47, LevelDelta: 1}},
	}))
	if err != nil {
		t.Fatalf("ApplyBuy() error = %v", err)
	}
	if result.Points.RemainingSP != 50 || result.Points.SyncedLevel != 10 {
		t.Fatalf("hydrated points = %+v", result.Points)
	}
	persisted, ok, err := repos.Skill.Load(ctx, "31")
	if err != nil || !ok {
		t.Fatalf("load persisted skill ok=%t err=%v", ok, err)
	}
	if persisted.Points != result.Points || persisted.Layouts[0][0] != 46 || persisted.Layouts[0][1] != 47 {
		t.Fatalf("persisted hydrated record = %+v", persisted)
	}
}

func TestOwnerApplyBuySynchronizesLevelGainFromPVFBaseline(t *testing.T) {
	ctx := context.Background()
	repos := seededSkillOwnerRepositories(t, ctx)
	record, _, _ := repos.Skill.Load(ctx, "31")
	record.Points = dnfrepo.SkillPointState{TotalSP: 80, RemainingSP: 40, TotalTP: 8, RemainingTP: 5, SyncedLevel: 9}
	if err := repos.Skill.Save(ctx, record); err != nil {
		t.Fatalf("save stale ledger: %v", err)
	}
	owner, err := NewOwner(repos, OwnerOptions{
		Catalog:       skillRuleCatalog(t),
		InitialLevels: map[uint16]int{46: 1},
		PointBaseline: &dnfrepo.SkillPointState{TotalSP: 100, RemainingSP: 100, TotalTP: 10, RemainingTP: 10, SyncedLevel: 10},
	})
	if err != nil {
		t.Fatalf("NewOwner() error = %v", err)
	}

	result, err := owner.ApplyBuy(ctx, NewBuyCommand(alignedRequest("acc-skill", 31), BuySkillRequest{
		Count:   1,
		Entries: []BuySkillEntry{{SkillID: 46, LevelDelta: 1}},
	}))
	if err != nil {
		t.Fatalf("ApplyBuy() error = %v", err)
	}
	if result.Points.RemainingSP != 40 || result.Points.RemainingTP != 7 || result.Points.TotalSP != 100 || result.Points.TotalTP != 10 || result.Points.SyncedLevel != 10 {
		t.Fatalf("synchronized points = %+v", result.Points)
	}
}

// TestSynchronizePointLedgerKeepsAndRestoresPersistedBookBonus pins the contract
// behind "+SP books stopped letting me learn skills". The baseline handed to the
// buy/reset path now carries the persisted bonus_sp/bonus_tp (see
// dnfbridge.skillOwnerRules), so synchronization has to (1) leave a book-bonused
// ledger alone instead of clawing the bonus back, and (2) hand the missing bonus
// back exactly once to a ledger an older build already stripped, then stay a
// no-op. Case 1 is the fix; case 2 is the repair for existing characters.
func TestSynchronizePointLedgerKeepsAndRestoresPersistedBookBonus(t *testing.T) {
	const level = 90
	// Etc/spTable.etc totals 1000 SP / 20 TP at level 90, plus a paid bonus of
	// 180 SP / 4 TP granted by nine +20 SP books.
	bonusBaseline := &dnfrepo.SkillPointState{
		TotalSP: 1180, RemainingSP: 1180, TotalTP: 24, RemainingTP: 24, SyncedLevel: level,
	}

	t.Run("keeps the bonus on a matching ledger", func(t *testing.T) {
		current := dnfrepo.SkillPointState{
			TotalSP: 1180, RemainingSP: 220, TotalTP: 24, RemainingTP: 2, SyncedLevel: level,
		}
		got, err := synchronizePointLedger(current, bonusBaseline, level, nil, 0, nil, nil)
		if err != nil {
			t.Fatalf("synchronizePointLedger() error = %v", err)
		}
		if got != current {
			t.Fatalf("points = %+v, want unchanged %+v", got, current)
		}
	})

	t.Run("restores a ledger stripped by the older build", func(t *testing.T) {
		// The older baseline omitted the bonus, so a purchase reset TotalSP to the
		// PVF table and deducted the whole bonus from RemainingSP: 220 spendable SP
		// became 40, which is exactly what the live server reported right before it
		// refused a 200 SP skill for remaining=-160.
		stripped := dnfrepo.SkillPointState{
			TotalSP: 1000, RemainingSP: 40, TotalTP: 20, RemainingTP: 2, SyncedLevel: level,
		}
		want := dnfrepo.SkillPointState{
			TotalSP: 1180, RemainingSP: 220, TotalTP: 24, RemainingTP: 6, SyncedLevel: level,
		}
		got, err := synchronizePointLedger(stripped, bonusBaseline, level, nil, 0, nil, nil)
		if err != nil {
			t.Fatalf("synchronizePointLedger() error = %v", err)
		}
		if got != want {
			t.Fatalf("restored points = %+v, want %+v", got, want)
		}
		again, err := synchronizePointLedger(got, bonusBaseline, level, nil, 0, nil, nil)
		if err != nil {
			t.Fatalf("second synchronizePointLedger() error = %v", err)
		}
		if again != want {
			t.Fatalf("second call points = %+v, want idempotent %+v", again, want)
		}
	})
}

// TestSkillPointTargetForCharacterAddsPersistedBookBonus pins the boundary
// between the two baseline contracts: OwnerOptions.PointBaseline stays the
// ordinary PVF target (the SP/TP book owner adds the bonus itself), and the
// skill plan path adds the persisted bonus here.
func TestSkillPointTargetForCharacterAddsPersistedBookBonus(t *testing.T) {
	pvfBaseline := &dnfrepo.SkillPointState{
		TotalSP: 1000, RemainingSP: 1000, TotalTP: 20, RemainingTP: 20, SyncedLevel: 90,
	}
	plain := dnfrepo.CharacterRecord{CharacterID: "77"}
	withBonus := dnfrepo.CharacterRecord{
		CharacterID: "77",
		Stats:       map[string]int64{"bonus_sp": 180, "bonus_tp": 4},
	}

	got, err := skillPointTargetForCharacter(pvfBaseline, withBonus)
	if err != nil {
		t.Fatalf("skillPointTargetForCharacter() error = %v", err)
	}
	want := dnfrepo.SkillPointState{
		TotalSP: 1180, RemainingSP: 1000, TotalTP: 24, RemainingTP: 20, SyncedLevel: 90,
	}
	if got == nil || *got != want {
		t.Fatalf("target = %+v, want %+v", got, want)
	}
	// The helper must not mutate the caller's baseline: the same pointer is
	// shared with the book owner for the lifetime of one request.
	if *pvfBaseline != (dnfrepo.SkillPointState{TotalSP: 1000, RemainingSP: 1000, TotalTP: 20, RemainingTP: 20, SyncedLevel: 90}) {
		t.Fatalf("baseline was mutated: %+v", *pvfBaseline)
	}

	if got, err := skillPointTargetForCharacter(nil, withBonus); err != nil || got != nil {
		t.Fatalf("nil baseline got=%+v err=%v, want nil/nil", got, err)
	}
	// A character that never used a book, and one with no stats map at all, must
	// both fall through to the raw PVF target.
	for name, character := range map[string]dnfrepo.CharacterRecord{"no bonus": plain, "no stats": {CharacterID: "77", Stats: map[string]int64{}}} {
		got, err := skillPointTargetForCharacter(pvfBaseline, character)
		if err != nil {
			t.Fatalf("%s: error = %v", name, err)
		}
		if got == nil || *got != *pvfBaseline {
			t.Fatalf("%s: target = %+v, want unchanged %+v", name, got, *pvfBaseline)
		}
	}
	for _, bad := range []map[string]int64{{"bonus_sp": -1}, {"bonus_tp": -1}} {
		if _, err := skillPointTargetForCharacter(pvfBaseline, dnfrepo.CharacterRecord{CharacterID: "77", Stats: bad}); !errors.Is(err, ErrSkillPointLedger) {
			t.Fatalf("stats=%v error = %v, want ErrSkillPointLedger", bad, err)
		}
	}
}

// TestOwnerApplyBuyKeepsPersistedBookBonus is the end-to-end guard for "+SP books
// stopped letting me learn skills": a character that used +60 SP / +4 TP books
// must keep that bonus when it buys a skill, and a ledger an older build already
// stripped down to the raw PVF total must get the bonus handed back exactly once.
func TestOwnerApplyBuyKeepsPersistedBookBonus(t *testing.T) {
	ctx := context.Background()
	repos := seededSkillOwnerRepositories(t, ctx)
	character, found, err := repos.Character.Load(ctx, "31")
	if err != nil || !found {
		t.Fatalf("load character found=%t err=%v", found, err)
	}
	character.Stats["bonus_sp"] = 60
	character.Stats["bonus_tp"] = 4
	if err := repos.Character.Save(ctx, character); err != nil {
		t.Fatalf("save character: %v", err)
	}
	// An older build reset TotalSP to the PVF total and deducted the whole bonus
	// from RemainingSP, so 100 spendable SP is displayed as 40.
	record, _, err := repos.Skill.Load(ctx, "31")
	if err != nil {
		t.Fatal(err)
	}
	record.Points = dnfrepo.SkillPointState{
		TotalSP: 100, RemainingSP: 40, TotalTP: 10, RemainingTP: 2, SyncedLevel: 10,
	}
	if err := repos.Skill.Save(ctx, record); err != nil {
		t.Fatal(err)
	}
	owner, err := NewOwner(repos, OwnerOptions{
		Catalog:       skillRuleCatalog(t),
		InitialLevels: map[uint16]int{46: 1},
		// The ordinary PVF target, WITHOUT the bonus: the book owner shares this.
		PointBaseline: &dnfrepo.SkillPointState{TotalSP: 100, RemainingSP: 100, TotalTP: 10, RemainingTP: 10, SyncedLevel: 10},
	})
	if err != nil {
		t.Fatalf("NewOwner() error = %v", err)
	}
	result, err := owner.ApplyBuy(ctx, NewBuyCommand(alignedRequest("acc-skill", 31), BuySkillRequest{
		Count:   1,
		Entries: []BuySkillEntry{{SkillID: 46, LevelDelta: 1}},
	}))
	if err != nil {
		t.Fatalf("ApplyBuy() error = %v", err)
	}
	// skill 46 costs 20 at this level. 40 + 60 (bonus restored) - 20 = 80, and the
	// totals must carry PVF 100/10 plus the bonus 60/4.
	want := dnfrepo.SkillPointState{
		TotalSP: 160, RemainingSP: 80, TotalTP: 14, RemainingTP: 6, SyncedLevel: 10,
	}
	if result.Points != want {
		t.Fatalf("points = %+v, want %+v", result.Points, want)
	}
	persisted, _, err := repos.Skill.Load(ctx, "31")
	if err != nil {
		t.Fatal(err)
	}
	if persisted.Points != want {
		t.Fatalf("persisted points = %+v, want %+v", persisted.Points, want)
	}
}

func TestBuildInitialSkillLayoutLimitsQuickbarToThreeActiveSkillsForEachTree(t *testing.T) {
	catalog := skillRuleCatalog(t)
	states := map[int64]dnfrepo.SkillState{
		46: {Level: 1, Enabled: true},
		47: {Level: 1, Enabled: true},
		48: {Level: 1, Enabled: true},
		49: {Level: 1, Enabled: true},
		50: {Level: 1, Enabled: true},
	}
	want := dnfrepo.SkillLayout{0: 46, 1: 47, 2: 48, 54: 50, 150: 49}
	for _, tree := range []int{0, 1} {
		layout, err := BuildInitialSkillLayout(catalog, 0, tree, states)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(layout, want) {
			t.Fatalf("tree %d layout=%v, want %v", tree, layout, want)
		}
		for slot := 3; slot < 6; slot++ {
			if skillID, ok := layout[slot]; ok {
				t.Fatalf("tree %d unexpected primary slot %d skill %d", tree, slot, skillID)
			}
		}
	}
}

func TestBuildInitialSkillLayoutKeepsRealPVFGrantWithoutSkillDefinition(t *testing.T) {
	catalog := skillRuleCatalog(t)
	states := map[int64]dnfrepo.SkillState{
		46:  {Level: 1, Enabled: true},
		777: {Level: 1, Enabled: true},
	}
	layout, err := BuildInitialSkillLayout(catalog, 0, 0, states)
	if err != nil {
		t.Fatal(err)
	}
	if layout[0] != 46 || layout[6] != 777 {
		t.Fatalf("missing-definition PVF grant layout=%v, want active skill 46 and fallback skill 777", layout)
	}
}

func TestBuildCurrentSkillLayoutMovesHistoricalPassiveOutOfQuickbar(t *testing.T) {
	layout, err := BuildCurrentSkillLayout(skillRuleCatalog(t), 0, 0,
		map[int64]dnfrepo.SkillState{46: {Level: 1, Enabled: true}, 50: {Level: 1, Enabled: true}},
		dnfrepo.SkillLayout{0: 50, 1: 46})
	if err != nil {
		t.Fatal(err)
	}
	if layout[1] != 46 || layout[54] != 50 {
		t.Fatalf("historical passive quickbar layout=%+v", layout)
	}
	if _, exists := layout[0]; exists {
		t.Fatalf("passive remained in quickbar: %+v", layout)
	}
}

func TestBuildCurrentSkillLayoutMigratesRetiredSeventhSlotsIntoLearnedActiveGroup(t *testing.T) {
	states := map[int64]dnfrepo.SkillState{
		46: {Level: 8, Enabled: true},
		47: {Level: 1, Enabled: true},
	}
	existing := dnfrepo.SkillLayout{204: 46, 205: 47}
	for slot := 0; slot < 6; slot++ {
		skillID := uint16(100 + slot)
		states[int64(skillID)] = dnfrepo.SkillState{Level: 1, Enabled: true}
		existing[slot] = skillID
	}
	for slot := 198; slot < 204; slot++ {
		skillID := uint16(200 + slot)
		states[int64(skillID)] = dnfrepo.SkillState{Level: 1, Enabled: true}
		existing[slot] = skillID
	}

	layout, err := BuildCurrentSkillLayout(skillRuleCatalog(t), 0, 0, states, existing)
	if err != nil {
		t.Fatal(err)
	}
	if layout[150] != 46 || layout[151] != 47 {
		t.Fatalf("retired seventh-slot skills were not restored to learned active group: %v", layout)
	}
	for _, slot := range []int{204, 205} {
		if _, exists := layout[slot]; exists {
			t.Fatalf("retired slot %d survived stock-HUD repair: %v", slot, layout)
		}
	}
}

func TestBuildInitialDarkKnightSkillLayoutReservesASDFGHForCombos(t *testing.T) {
	states := map[int64]dnfrepo.SkillState{
		5:   {Level: 1, Enabled: true},
		8:   {Level: 1, Enabled: true},
		46:  {Level: 1, Enabled: true},
		118: {Level: 1, Enabled: true},
		119: {Level: 1, Enabled: true},
	}
	layout, err := BuildInitialSkillLayout(darkKnightSkillRuleCatalog(t), 9, 0, states)
	if err != nil {
		t.Fatal(err)
	}
	if layout[198] != 5 || layout[199] != 8 || layout[200] != 46 {
		t.Fatalf("dark knight QWERTY layout=%v, want skills 5/8/46 at 198/199/200", layout)
	}
	if layout[0] != 118 || layout[1] != 119 {
		t.Fatalf("dark knight fixed combo roots=%v, want slots 0/1 = 118/119", layout)
	}
	for slot := 2; slot < currentEXEPrimaryQuickSlotEnd; slot++ {
		if _, occupied := layout[slot]; occupied {
			t.Fatalf("dark knight unlearned combo root slot %d occupied: %v", slot, layout)
		}
	}
}

func TestOwnerApplySlotRejectsDarkKnightOrdinarySkillIntoASDFGH(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	if err := repos.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "31",
		AccountID:   "acc-skill",
		Job:         "9",
		Level:       10,
	}); err != nil {
		t.Fatal(err)
	}
	if err := repos.Skill.Save(ctx, dnfrepo.SkillRecord{
		CharacterID: "31",
		Skills:      map[int64]dnfrepo.SkillState{46: {Level: 1, Enabled: true}},
		Points:      dnfrepo.SkillPointState{SyncedLevel: 10},
		Layouts:     map[int]dnfrepo.SkillLayout{0: {198: 46}},
	}); err != nil {
		t.Fatal(err)
	}
	owner, err := NewOwner(repos, OwnerOptions{Catalog: darkKnightSkillRuleCatalog(t)})
	if err != nil {
		t.Fatal(err)
	}
	_, err = owner.ApplySlot(ctx, NewSlotCommand(alignedRequest("acc-skill", 31), ChangeSkillSlotRequest{
		SkillTree: 0, From: 198, To: 0, ContextIndex: -1, Mode: 0,
	}))
	if !errors.Is(err, ErrSkillSlot) {
		t.Fatalf("ApplySlot() error=%v, want exclusive combo slot rejection", err)
	}
	persisted, found, loadErr := repos.Skill.Load(ctx, "31")
	if loadErr != nil || !found || persisted.Layouts[0][198] != 46 {
		t.Fatalf("persisted found=%t error=%v layout=%v", found, loadErr, persisted.Layouts[0])
	}
	if _, occupied := persisted.Layouts[0][0]; occupied {
		t.Fatalf("ordinary skill entered dark knight ASDFGH: %v", persisted.Layouts[0])
	}
}

func TestOwnerApplySlotRejectsPassiveSkillIntoQuickbar(t *testing.T) {
	ctx := context.Background()
	repos := seededSkillOwnerRepositories(t, ctx)
	record, _, _ := repos.Skill.Load(ctx, "31")
	record.Skills[50] = dnfrepo.SkillState{Level: 1, Enabled: true}
	record.Layouts = map[int]dnfrepo.SkillLayout{0: {54: 50}}
	if err := repos.Skill.Save(ctx, record); err != nil {
		t.Fatalf("save passive layout: %v", err)
	}
	owner, err := NewOwner(repos, OwnerOptions{Catalog: skillRuleCatalog(t), InitialLevels: map[uint16]int{46: 1}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = owner.ApplySlot(ctx, NewSlotCommand(alignedRequest("acc-skill", 31), ChangeSkillSlotRequest{
		SkillTree: 0, From: 54, To: 0, ContextIndex: -1, Mode: 0,
	}))
	if !errors.Is(err, ErrSkillSlot) {
		t.Fatalf("ApplySlot() error = %v, want passive quickbar rejection", err)
	}
	persisted, _, err := repos.Skill.Load(ctx, "31")
	if err != nil {
		t.Fatal(err)
	}
	if persisted.Layouts[0][54] != 50 {
		t.Fatalf("passive source changed after rejected move: %+v", persisted.Layouts[0])
	}
	if _, exists := persisted.Layouts[0][0]; exists {
		t.Fatalf("passive was persisted in quickbar: %+v", persisted.Layouts[0])
	}
}

func TestOwnerApplyCommandsRejectsUnknownSkillWithoutChangingStoredCommands(t *testing.T) {
	ctx := context.Background()
	repos := seededSkillOwnerRepositories(t, ctx)
	record, _, err := repos.Skill.Load(ctx, "31")
	if err != nil {
		t.Fatal(err)
	}
	record.Commands = map[int64][]byte{46: {1, 2, 3}}
	if err := repos.Skill.Save(ctx, record); err != nil {
		t.Fatal(err)
	}
	owner, err := NewOwner(repos)
	if err != nil {
		t.Fatal(err)
	}
	_, err = owner.ApplyCommands(ctx, Command{
		AccountID:           "acc-skill",
		SelectedCharacterID: 31,
		CommandRecords: []SkillCommandRecord{{
			SkillID:      99,
			CommandBytes: []byte{4},
		}},
	})
	if !errors.Is(err, ErrSkillCommandUnknown) {
		t.Fatalf("ApplyCommands() error = %v, want %v", err, ErrSkillCommandUnknown)
	}
	persisted, _, err := repos.Skill.Load(ctx, "31")
	if err != nil || string(persisted.Commands[46]) != string([]byte{1, 2, 3}) || len(persisted.Commands) != 1 {
		t.Fatalf("unknown command changed persistence: err=%v commands=%x", err, persisted.Commands)
	}
}

func TestOwnerApplyCommandsValidatesOwnedSkillAcrossAllPages(t *testing.T) {
	ctx := context.Background()
	repos := seededSkillOwnerRepositories(t, ctx)
	record, _, err := repos.Skill.Load(ctx, "31")
	if err != nil {
		t.Fatal(err)
	}
	record.Skills[248] = dnfrepo.SkillState{Level: 1, Enabled: true}
	if err := repos.Skill.Save(ctx, record); err != nil {
		t.Fatal(err)
	}
	owner, err := NewOwner(repos)
	if err != nil {
		t.Fatal(err)
	}
	_, err = owner.ApplyCommands(ctx, Command{
		AccountID:           "acc-skill",
		SelectedCharacterID: 31,
		CommandRecords: []SkillCommandRecord{{
			SkillID:      248,
			CommandBytes: []byte{3, 1, 8},
		}},
	})
	if err != nil {
		t.Fatalf("ApplyCommands() error = %v", err)
	}
	persisted, found, err := repos.Skill.Load(ctx, "31")
	if err != nil || !found || string(persisted.Commands[248]) != string([]byte{3, 1, 8}) {
		t.Fatalf("persisted found=%t err=%v command=% X", found, err, persisted.Commands[248])
	}
}

func seededSkillOwnerRepositories(t *testing.T, ctx context.Context) dnfrepo.Group {
	t.Helper()
	repos := dnfrepomemory.NewMemoryGroup()
	if err := repos.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "31",
		AccountID:   "acc-skill",
		Job:         "0",
		Level:       10,
		Stats:       map[string]int64{"grow_type": 0, currentEXESkillTreeIndexStat: 0},
	}); err != nil {
		t.Fatalf("save character: %v", err)
	}
	if err := repos.Skill.Save(ctx, dnfrepo.SkillRecord{
		CharacterID: "31",
		Skills:      map[int64]dnfrepo.SkillState{46: {Level: 1, Enabled: true}},
		Points: dnfrepo.SkillPointState{
			TotalSP: 100, RemainingSP: 100,
			TotalTP: 10, RemainingTP: 10,
			SyncedLevel: 10,
		},
	}); err != nil {
		t.Fatalf("save skill: %v", err)
	}
	return repos
}

func skillRuleCatalog(t *testing.T) *dnfskill.Table {
	t.Helper()
	source := skillRuleTextSource{
		"skill/skilllist.lst":     "0 `SwordmanSkill.lst`\n",
		"skill/SwordmanSkill.lst": "46 `Swordman/Base.skl`\n47 `Swordman/Dependent.skl`\n48 `Swordman/TP.skl`\n49 `Swordman/Fourth.skl`\n50 `Swordman/Passive.skl`\n51 `Swordman/HighLevel.skl`\n52 `Swordman/HighDep.skl`\n197 `Swordman/Conversion.skl`\n",
		"skill/Swordman/Base.skl": `
[type]
` + "`[active]`" + `
[required level]
1
[maximum level]
20
[skill fitness growtype]
0
[/skill fitness growtype]
[purchase cost]
20
[/purchase cost]
`,
		"skill/Swordman/Dependent.skl": `
[type]
` + "`[active]`" + `
[required level]
5
[maximum level]
10
[skill fitness growtype]
0
[/skill fitness growtype]
[pre required skill]
46 2
[/pre required skill]
[purchase cost]
30
[/purchase cost]
`,
		"skill/Swordman/TP.skl": `
[type]
` + "`[active]`" + `
[required level]
1
[maximum level]
3
[skill fitness growtype]
0
[/skill fitness growtype]
[pre required skill]
46 1
[/pre required skill]
[feature skill type]
1
[special purchase cost]
2
[/special purchase cost]
`,
		"skill/Swordman/Fourth.skl": `
[type]
` + "`[active]`" + `
[required level]
1
[maximum level]
20
`,
		"skill/Swordman/Passive.skl": `
[type]
` + "`[passive]`" + `
[required level]
1
[maximum level]
20
`,
		"skill/Swordman/Conversion.skl": `
[type]
` + "`[passive]`" + `
[required level]
20
[maximum level]
1
`,
		"skill/Swordman/HighLevel.skl": `
[type]
` + "`[active]`" + `
[required level]
12
[maximum level]
5
[skill fitness growtype]
0
[/skill fitness growtype]
[purchase cost]
20
[/purchase cost]
`,
		"skill/Swordman/HighDep.skl": `
[type]
` + "`[active]`" + `
[required level]
1
[maximum level]
5
[skill fitness growtype]
0
[/skill fitness growtype]
[pre required skill]
51 2
[/pre required skill]
[purchase cost]
30
[/purchase cost]
`,
	}
	index, err := dnfpvf.Build(context.Background(), source, dnfpvf.BuildOptions{Lists: []string{dnfskill.DefaultList}})
	if err != nil {
		t.Fatalf("build skill index: %v", err)
	}
	catalog, err := dnfskill.Load(context.Background(), index, dnfskill.Options{})
	if err != nil {
		t.Fatalf("load skill catalog: %v", err)
	}
	return catalog
}

func darkKnightSkillRuleCatalog(t *testing.T) *dnfskill.Table {
	t.Helper()
	source := skillRuleTextSource{
		"skill/skilllist.lst": "9 `DemonicSwordmanSkill.lst`\n",
		"skill/DemonicSwordmanSkill.lst": "5 `DemonicSwordman/Five.skl`\n" +
			"8 `DemonicSwordman/Eight.skl`\n" +
			"46 `DemonicSwordman/FortySix.skl`\n" +
			"118 `DemonicSwordman/ComboSet0.skl`\n" +
			"119 `DemonicSwordman/ComboSet1.skl`\n",
	}
	for _, name := range []string{"Five", "Eight", "FortySix"} {
		source["skill/DemonicSwordman/"+name+".skl"] = "[type]\n`[active]`\n[required level]\n1\n[maximum level]\n20\n"
	}
	source["skill/DemonicSwordman/ComboSet0.skl"] = "[combo set]\n5 8\n[/combo set]\n"
	source["skill/DemonicSwordman/ComboSet1.skl"] = "[combo set]\n46\n[/combo set]\n"
	index, err := dnfpvf.Build(context.Background(), source, dnfpvf.BuildOptions{Lists: []string{dnfskill.DefaultList}})
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := dnfskill.Load(context.Background(), index, dnfskill.Options{})
	if err != nil {
		t.Fatal(err)
	}
	return catalog
}

func secondAwakeningSkillCatalog(t *testing.T) *dnfskill.Table {
	t.Helper()
	source := skillRuleTextSource{
		"skill/skilllist.lst":     "0 `SwordmanSkill.lst`\n",
		"skill/SwordmanSkill.lst": "46 `Swordman/Second.skl`\n",
		"skill/Swordman/Second.skl": `[type]
` + "`[active]`" + `
[required level]
1
[maximum level]
20
[skill fitness growtype]
0
[/skill fitness growtype]
[skill fitness second growtype]
2
[/skill fitness second growtype]
[purchase cost]
20
[/purchase cost]
`,
	}
	index, err := dnfpvf.Build(context.Background(), source, dnfpvf.BuildOptions{Lists: []string{dnfskill.DefaultList}})
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := dnfskill.Load(context.Background(), index, dnfskill.Options{})
	if err != nil {
		t.Fatal(err)
	}
	return catalog
}

func growthMaximumSkillCatalog(t *testing.T) *dnfskill.Table {
	t.Helper()
	source := skillRuleTextSource{
		"skill/skilllist.lst":     "0 `SwordmanSkill.lst`\n",
		"skill/SwordmanSkill.lst": "46 `Swordman/Base.skl`\n",
		"skill/Swordman/Base.skl": `[type]
` + "`[active]`" + `
[required level]
1
[maximum level]
20
[growtype maximum level]
0 20
[/growtype maximum level]
[purchase cost]
20
[/purchase cost]
`,
	}
	index, err := dnfpvf.Build(context.Background(), source, dnfpvf.BuildOptions{Lists: []string{dnfskill.DefaultList}})
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := dnfskill.Load(context.Background(), index, dnfskill.Options{})
	if err != nil {
		t.Fatal(err)
	}
	return catalog
}

func learnableLevelCapCatalog(t *testing.T) *dnfskill.Table {
	t.Helper()
	source := skillRuleTextSource{
		"skill/skilllist.lst":     "0 `SwordmanSkill.lst`\n",
		"skill/SwordmanSkill.lst": "46 `Swordman/Base.skl`\n67 `Swordman/WeaponCombo.skl`\n126 `Swordman/SecondAwakening.skl`\n127 `Swordman/LegacyUnbounded.skl`\n",
		"skill/Swordman/Base.skl": `[type]
` + "`[active]`" + `
[required level]
1
[maximum level]
20
[skill fitness growtype]
1
[/skill fitness growtype]
[growtype maximum level]
0 20 0 0 0 0
[/growtype maximum level]
[purchase cost]
0
[/purchase cost]
`,
		"skill/Swordman/WeaponCombo.skl": `[type]
` + "`[active]`" + `
[required level]
15
[required level range]
15
[maximum level]
15
[skill fitness growtype]
1
[/skill fitness growtype]
[growtype maximum level]
0 15 0 0 0 0
[/growtype maximum level]
[purchase cost]
30
[/purchase cost]
`,
		"skill/Swordman/SecondAwakening.skl": `[type]
` + "`[passive]`" + `
[required level]
75
[required level range]
3
[maximum level]
40
[skill fitness growtype]
1
[/skill fitness growtype]
[growtype maximum level]
0 0 0 0 0 0
[/growtype maximum level]
[skill fitness second growtype]
2
[/skill fitness second growtype]
[second growtype maximum level]
0 0 0 2 0 0 0 0 0 0 0 0
[/second growtype maximum level]
[purchase cost]
80
[/purchase cost]
`,
		"skill/Swordman/LegacyUnbounded.skl": `[type]
` + "`[active]`" + `
[required level]
1
[skill fitness growtype]
1
[/skill fitness growtype]
[purchase cost]
20
[/purchase cost]
`,
	}
	index, err := dnfpvf.Build(context.Background(), source, dnfpvf.BuildOptions{Lists: []string{dnfskill.DefaultList}})
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := dnfskill.Load(context.Background(), index, dnfskill.Options{})
	if err != nil {
		t.Fatal(err)
	}
	return catalog
}

type skillRuleTextSource map[string]string

func (s skillRuleTextSource) ReadText(relativePath string) (string, error) {
	want := strings.ToLower(strings.ReplaceAll(relativePath, "\\", "/"))
	for key, value := range s {
		if strings.ToLower(strings.ReplaceAll(key, "\\", "/")) == want {
			return value, nil
		}
	}
	return "", dnfpvf.ErrDocNotFound
}

func alignedRequest(accountID string, selectedCharacterID uint16) alignedcmd.Request {
	return alignedcmd.Request{AccountID: accountID, SelectedCharacterID: selectedCharacterID}
}

type countingCharacterSkillUnitOfWork struct {
	delegate dnfrepo.CharacterSkillUnitOfWork
	calls    int
}

func (u *countingCharacterSkillUnitOfWork) WithinCharacterSkill(ctx context.Context, characterID string, apply func(dnfrepo.SkillRepository) error) error {
	u.calls++
	return u.delegate.WithinCharacterSkill(ctx, characterID, apply)
}

func TestOwnerApplyBuyOverSkillContractRaisesEffectiveLevel(t *testing.T) {
	ctx := context.Background()
	repos := seededSkillOwnerRepositories(t, ctx)
	owner, err := NewOwner(repos, OwnerOptions{
		Catalog:       skillRuleCatalog(t),
		InitialLevels: map[uint16]int{},
	})
	if err != nil {
		t.Fatalf("NewOwner() error = %v", err)
	}

	buy := BuySkillRequest{Count: 1, Entries: []BuySkillEntry{{SkillID: 51, LevelDelta: 1}}}
	if _, err := owner.ApplyBuy(ctx, NewBuyCommand(alignedRequest("acc-skill", 31), buy)); !errors.Is(err, ErrSkillLevel) {
		t.Fatalf("without contract ApplyBuy() error = %v, want ErrSkillLevel", err)
	}

	future := time.Now().Add(24 * time.Hour).Unix()
	if err := repos.Account.Save(ctx, dnfrepo.AccountRecord{
		AccountID: "acc-skill",
		Metadata:  map[string]string{"premium_expire_27": strconv.FormatInt(future, 10)},
	}); err != nil {
		t.Fatal(err)
	}
	result, err := owner.ApplyBuy(ctx, NewBuyCommand(alignedRequest("acc-skill", 31), buy))
	if err != nil {
		t.Fatalf("with over-skill contract ApplyBuy() error = %v", err)
	}
	if result.Points.RemainingSP != 80 {
		t.Fatalf("remaining SP = %d, want 80", result.Points.RemainingSP)
	}
	record, _, _ := repos.Skill.Load(ctx, "31")
	if record.Skills[51].Level != 1 {
		t.Fatalf("skill 51 level = %d, want 1", record.Skills[51].Level)
	}
}

func TestOwnerApplyBuyEnforcesPVFMaximumLearnableLevel(t *testing.T) {
	newOwner := func(t *testing.T, level int, growType int, overSkillContract bool) (dnfrepo.Group, *Owner) {
		t.Helper()
		ctx := context.Background()
		repos := seededSkillOwnerRepositories(t, ctx)
		character, found, err := repos.Character.Load(ctx, "31")
		if err != nil || !found {
			t.Fatalf("load character found=%t err=%v", found, err)
		}
		character.Level = level
		character.Stats["grow_type"] = int64(growType)
		if err := repos.Character.Save(ctx, character); err != nil {
			t.Fatal(err)
		}
		record, found, err := repos.Skill.Load(ctx, "31")
		if err != nil || !found {
			t.Fatalf("load skill found=%t err=%v", found, err)
		}
		record.Points = dnfrepo.SkillPointState{
			TotalSP:     10000,
			RemainingSP: 10000,
			TotalTP:     100,
			RemainingTP: 100,
			SyncedLevel: level,
		}
		if err := repos.Skill.Save(ctx, record); err != nil {
			t.Fatal(err)
		}
		if overSkillContract {
			future := time.Now().Add(24 * time.Hour).Unix()
			if err := repos.Account.Save(ctx, dnfrepo.AccountRecord{
				AccountID: "acc-skill",
				Metadata:  map[string]string{"premium_expire_27": strconv.FormatInt(future, 10)},
			}); err != nil {
				t.Fatal(err)
			}
		}
		owner, err := NewOwner(repos, OwnerOptions{
			Catalog:       learnableLevelCapCatalog(t),
			InitialLevels: map[uint16]int{46: 1},
		})
		if err != nil {
			t.Fatal(err)
		}
		return repos, owner
	}

	t.Run("weapon combo level 90 caps at six", func(t *testing.T) {
		ctx := context.Background()
		repos, owner := newOwner(t, 90, 0x01, false)
		command := func(delta byte) Command {
			return NewBuyCommand(alignedRequest("acc-skill", 31), BuySkillRequest{
				Count:   1,
				Entries: []BuySkillEntry{{SkillID: 67, LevelDelta: delta}},
			})
		}
		if _, err := owner.ApplyBuy(ctx, command(6)); err != nil {
			t.Fatalf("learn level 6: %v", err)
		}
		if _, err := owner.ApplyBuy(ctx, command(1)); !errors.Is(err, ErrSkillLevel) {
			t.Fatalf("learn level 7 error = %v, want ErrSkillLevel", err)
		}
		record, _, _ := repos.Skill.Load(ctx, "31")
		if got := record.Skills[67].Level; got != 6 {
			t.Fatalf("persisted weapon combo level = %d, want 6", got)
		}
	})

	t.Run("five over-skill levels do not cross the next fifteen-level interval", func(t *testing.T) {
		ctx := context.Background()
		repos, owner := newOwner(t, 90, 0x01, true)
		_, err := owner.ApplyBuy(ctx, NewBuyCommand(alignedRequest("acc-skill", 31), BuySkillRequest{
			Count:   1,
			Entries: []BuySkillEntry{{SkillID: 67, LevelDelta: 7}},
		}))
		if !errors.Is(err, ErrSkillLevel) {
			t.Fatalf("effective level 95 learn level 7 error = %v, want ErrSkillLevel", err)
		}
		record, _, _ := repos.Skill.Load(ctx, "31")
		if got := record.Skills[67].Level; got != 0 {
			t.Fatalf("rejected weapon combo level persisted as %d", got)
		}
	})

	t.Run("level 105 opens weapon combo level seven", func(t *testing.T) {
		ctx := context.Background()
		repos, owner := newOwner(t, 105, 0x01, false)
		_, err := owner.ApplyBuy(ctx, NewBuyCommand(alignedRequest("acc-skill", 31), BuySkillRequest{
			Count:   1,
			Entries: []BuySkillEntry{{SkillID: 67, LevelDelta: 7}},
		}))
		if err != nil {
			t.Fatalf("learn level 7: %v", err)
		}
		record, _, _ := repos.Skill.Load(ctx, "31")
		if got := record.Skills[67].Level; got != 7 {
			t.Fatalf("persisted weapon combo level = %d, want 7", got)
		}
	})

	t.Run("legacy skill without an explicit cap remains compatible", func(t *testing.T) {
		ctx := context.Background()
		repos, owner := newOwner(t, 90, 0x01, false)
		_, err := owner.ApplyBuy(ctx, NewBuyCommand(alignedRequest("acc-skill", 31), BuySkillRequest{
			Count:   1,
			Entries: []BuySkillEntry{{SkillID: 127, LevelDelta: 3}},
		}))
		if err != nil {
			t.Fatalf("learn legacy skill without PVF cap: %v", err)
		}
		record, _, _ := repos.Skill.Load(ctx, "31")
		if got := record.Skills[127].Level; got != 3 {
			t.Fatalf("persisted legacy skill level = %d, want 3", got)
		}
	})

	t.Run("second awakening vector caps the packed growth stage", func(t *testing.T) {
		ctx := context.Background()
		repos, owner := newOwner(t, 90, 0x21, false)
		command := func(delta byte) Command {
			return NewBuyCommand(alignedRequest("acc-skill", 31), BuySkillRequest{
				Count:   1,
				Entries: []BuySkillEntry{{SkillID: 126, LevelDelta: delta}},
			})
		}
		if _, err := owner.ApplyBuy(ctx, command(2)); err != nil {
			t.Fatalf("learn second-awakening level 2: %v", err)
		}
		if _, err := owner.ApplyBuy(ctx, command(1)); !errors.Is(err, ErrSkillLevel) {
			t.Fatalf("learn second-awakening level 3 error = %v, want ErrSkillLevel", err)
		}
		record, _, _ := repos.Skill.Load(ctx, "31")
		if got := record.Skills[126].Level; got != 2 {
			t.Fatalf("persisted second-awakening level = %d, want 2", got)
		}
	})
}

func TestOwnerApplyBuyRefundConsumesForgetRiverWaterAtomically(t *testing.T) {
	ctx := context.Background()
	repos := seededSkillOwnerRepositories(t, ctx)
	record, _, _ := repos.Skill.Load(ctx, "31")
	record.Skills[46] = dnfrepo.SkillState{Level: 2, Enabled: true}
	record.Points.RemainingSP = 80
	if err := repos.Skill.Save(ctx, record); err != nil {
		t.Fatal(err)
	}
	if err := repos.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "31",
		Slots:       map[string]dnfrepo.ItemStack{"0:5": {ItemID: forgetRiverWaterItemID, Count: 1}},
	}); err != nil {
		t.Fatal(err)
	}
	owner, err := NewOwner(repos, OwnerOptions{
		Catalog:       skillRuleCatalog(t),
		InitialLevels: map[uint16]int{46: 1},
	})
	if err != nil {
		t.Fatalf("NewOwner() error = %v", err)
	}

	result, err := owner.ApplyBuy(ctx, NewBuyCommand(alignedRequest("acc-skill", 31), BuySkillRequest{
		Count:     1,
		FinalMode: currentEXELetheWaterMode,
		Entries:   []BuySkillEntry{{SkillID: 46, RefundFlag: 1, LevelDelta: 1}},
	}))
	if err != nil {
		t.Fatalf("ApplyBuy() error = %v", err)
	}
	if !result.ConsumedRefundItem || result.ConsumedRefundItemSlot != 5 {
		t.Fatalf("result = %+v, want consumed water slot 5", result)
	}
	if result.Points.RemainingSP != 100 {
		t.Fatalf("remaining SP = %d, want refunded 100", result.Points.RemainingSP)
	}
	skill, _, _ := repos.Skill.Load(ctx, "31")
	if skill.Skills[46].Level != 1 {
		t.Fatalf("skill 46 level = %d, want 1", skill.Skills[46].Level)
	}
	inventory, _, _ := repos.Inventory.Load(ctx, "31")
	if _, exists := inventory.Slots["0:5"]; exists {
		t.Fatalf("water stack still exists: %+v", inventory.Slots)
	}
}

func TestOwnerApplyBuyRefundWithoutWaterFailsClosed(t *testing.T) {
	ctx := context.Background()
	repos := seededSkillOwnerRepositories(t, ctx)
	record, _, _ := repos.Skill.Load(ctx, "31")
	record.Skills[46] = dnfrepo.SkillState{Level: 2, Enabled: true}
	record.Points.RemainingSP = 80
	if err := repos.Skill.Save(ctx, record); err != nil {
		t.Fatal(err)
	}
	if err := repos.Inventory.Save(ctx, dnfrepo.InventoryRecord{CharacterID: "31", Slots: map[string]dnfrepo.ItemStack{}}); err != nil {
		t.Fatal(err)
	}
	owner, err := NewOwner(repos, OwnerOptions{
		Catalog:       skillRuleCatalog(t),
		InitialLevels: map[uint16]int{46: 1},
	})
	if err != nil {
		t.Fatalf("NewOwner() error = %v", err)
	}

	_, err = owner.ApplyBuy(ctx, NewBuyCommand(alignedRequest("acc-skill", 31), BuySkillRequest{
		Count:     1,
		FinalMode: currentEXELetheWaterMode,
		Entries:   []BuySkillEntry{{SkillID: 46, RefundFlag: 1, LevelDelta: 1}},
	}))
	if !errors.Is(err, ErrSkillRefundConsumableRequired) {
		t.Fatalf("ApplyBuy() error = %v, want ErrSkillRefundConsumableRequired", err)
	}
	skill, _, _ := repos.Skill.Load(ctx, "31")
	if skill.Skills[46].Level != 2 || skill.Points.RemainingSP != 80 {
		t.Fatalf("rejected refund changed skill state: %+v", skill)
	}
}

func TestOwnerApplyBuyRefundFreeWhileLetheContractActive(t *testing.T) {
	ctx := context.Background()
	repos := seededSkillOwnerRepositories(t, ctx)
	record, _, _ := repos.Skill.Load(ctx, "31")
	record.Skills[46] = dnfrepo.SkillState{Level: 2, Enabled: true}
	record.Points.RemainingSP = 80
	if err := repos.Skill.Save(ctx, record); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(24 * time.Hour).Unix()
	if err := repos.Account.Save(ctx, dnfrepo.AccountRecord{
		AccountID: "acc-skill",
		Metadata:  map[string]string{"premium_expire_33": strconv.FormatInt(future, 10)},
	}); err != nil {
		t.Fatal(err)
	}
	owner, err := NewOwner(repos, OwnerOptions{
		Catalog:       skillRuleCatalog(t),
		InitialLevels: map[uint16]int{46: 1},
	})
	if err != nil {
		t.Fatalf("NewOwner() error = %v", err)
	}

	result, err := owner.ApplyBuy(ctx, NewBuyCommand(alignedRequest("acc-skill", 31), BuySkillRequest{
		Count:     1,
		FinalMode: currentEXELetheWaterMode,
		Entries:   []BuySkillEntry{{SkillID: 46, RefundFlag: 1, LevelDelta: 1}},
	}))
	if err != nil {
		t.Fatalf("ApplyBuy() error = %v", err)
	}
	if result.ConsumedRefundItem {
		t.Fatalf("lethe contract refund must not consume water: %+v", result)
	}
	if result.Points.RemainingSP != 100 {
		t.Fatalf("remaining SP = %d, want refunded 100", result.Points.RemainingSP)
	}
	skill, _, _ := repos.Skill.Load(ctx, "31")
	if skill.Skills[46].Level != 1 {
		t.Fatalf("skill 46 level = %d, want 1", skill.Skills[46].Level)
	}
}

func seedOverLevelContractSkills(t *testing.T, ctx context.Context, repos dnfrepo.Group) {
	t.Helper()
	record, _, _ := repos.Skill.Load(ctx, "31")
	record.Skills[46] = dnfrepo.SkillState{Level: 1, Enabled: true}
	record.Skills[51] = dnfrepo.SkillState{Level: 2, Enabled: true}
	record.Skills[52] = dnfrepo.SkillState{Level: 1, Enabled: true}
	// 51 costs 20x2 and 52 costs 30: 100-40-30=30 remaining.
	record.Points.RemainingSP = 30
	record.Layouts = map[int]dnfrepo.SkillLayout{0: {0: 46, 1: 51, 2: 52}}
	if err := repos.Skill.Save(ctx, record); err != nil {
		t.Fatal(err)
	}
}

func TestOwnerApplyBuySweepsExpiredOverSkillContractSkillsWithCascade(t *testing.T) {
	ctx := context.Background()
	repos := seededSkillOwnerRepositories(t, ctx)
	seedOverLevelContractSkills(t, ctx, repos)
	owner, err := NewOwner(repos, OwnerOptions{
		Catalog:       skillRuleCatalog(t),
		InitialLevels: map[uint16]int{46: 1},
	})
	if err != nil {
		t.Fatalf("NewOwner() error = %v", err)
	}

	// No active 达人契约: skill 51 (requires 12 > char 10) is swept, and skill
	// 52 (depends on 51>=2) cascades out; both refund their SP.
	result, err := owner.ApplyBuy(ctx, NewBuyCommand(alignedRequest("acc-skill", 31), BuySkillRequest{
		Count:   1,
		Entries: []BuySkillEntry{{SkillID: 46, LevelDelta: 1}},
	}))
	if err != nil {
		t.Fatalf("ApplyBuy() error = %v", err)
	}
	if !result.ExpiredContractSkillsReset {
		t.Fatalf("result = %+v, want ExpiredContractSkillsReset", result)
	}
	record, _, _ := repos.Skill.Load(ctx, "31")
	if _, exists := record.Skills[51]; exists {
		t.Fatalf("over-level skill 51 still learned: %+v", record.Skills)
	}
	if _, exists := record.Skills[52]; exists {
		t.Fatalf("broken dependent skill 52 still learned: %+v", record.Skills)
	}
	if record.Skills[46].Level != 2 {
		t.Fatalf("skill 46 level = %d, want 2", record.Skills[46].Level)
	}
	// 30 + 40 (51) + 30 (52) = 100, then -20 for the buy = 80.
	if record.Points.RemainingSP != 80 {
		t.Fatalf("remaining SP = %d, want 80", record.Points.RemainingSP)
	}
	for slot, id := range record.Layouts[0] {
		if id == 51 || id == 52 {
			t.Fatalf("layout slot %d still holds swept skill %d", slot, id)
		}
	}
}

func TestOwnerApplyBuyDoesNotLoopOnOverLevelPVFInitialFloor(t *testing.T) {
	ctx := context.Background()
	repos := seededSkillOwnerRepositories(t, ctx)
	record, _, err := repos.Skill.Load(ctx, "31")
	if err != nil {
		t.Fatal(err)
	}
	// Skill 51 requires character level 12 while this fixture is level 10,
	// but level 1 is a PVF-granted profession floor rather than a level bought
	// through the expired over-skill contract. Skill 52 is another permanent
	// floor whose declared prerequisite (51>=2) is intentionally unavailable;
	// both shapes occur in the active PVF profession audit.
	record.Skills[51] = dnfrepo.SkillState{Level: 1, Enabled: true}
	record.Skills[52] = dnfrepo.SkillState{Level: 1, Enabled: true}
	record.Layouts = map[int]dnfrepo.SkillLayout{0: {0: 46, 1: 51, 2: 52}}
	if err := repos.Skill.Save(ctx, record); err != nil {
		t.Fatal(err)
	}
	owner, err := NewOwner(repos, OwnerOptions{
		Catalog:       skillRuleCatalog(t),
		InitialLevels: map[uint16]int{46: 1, 51: 1, 52: 1},
	})
	if err != nil {
		t.Fatal(err)
	}

	type outcome struct {
		result MutationResult
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		result, applyErr := owner.ApplyBuy(ctx, NewBuyCommand(alignedRequest("acc-skill", 31), BuySkillRequest{
			Count:   1,
			Entries: []BuySkillEntry{{SkillID: 46, LevelDelta: 1}},
		}))
		done <- outcome{result: result, err: applyErr}
	}()

	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("ApplyBuy() error = %v", got.err)
		}
		if got.result.ExpiredContractSkillsReset {
			t.Fatalf("PVF floor must not be reported as swept: %+v", got.result)
		}
	case <-time.After(time.Second):
		t.Fatal("ApplyBuy() did not return; expired-contract sweep made no progress")
	}

	persisted, _, err := repos.Skill.Load(ctx, "31")
	if err != nil {
		t.Fatal(err)
	}
	if persisted.Skills[51].Level != 1 || persisted.Skills[52].Level != 1 || persisted.Skills[46].Level != 2 {
		t.Fatalf("persisted skills = %+v, want floors 51/52=1 and purchased 46=2", persisted.Skills)
	}
}

func TestValidateLearnedPrerequisitesExemptsOnlyPVFPermanentFloor(t *testing.T) {
	catalog := skillRuleCatalog(t)
	initial := map[uint16]int{52: 1}
	states := map[int64]dnfrepo.SkillState{
		51: {Level: 1, Enabled: true},
		52: {Level: 1, Enabled: true},
	}
	if err := validateLearnedPrerequisites(catalog, 0, states, initial); err != nil {
		t.Fatalf("PVF permanent floor prerequisite = %v", err)
	}
	states[52] = dnfrepo.SkillState{Level: 2, Enabled: true}
	if err := validateLearnedPrerequisites(catalog, 0, states, initial); !errors.Is(err, ErrSkillPrerequisite) {
		t.Fatalf("purchased level above floor prerequisite = %v, want %v", err, ErrSkillPrerequisite)
	}
}

func TestOwnerApplyBuyKeepsOverLevelSkillsWhileOverSkillContractActive(t *testing.T) {
	ctx := context.Background()
	repos := seededSkillOwnerRepositories(t, ctx)
	seedOverLevelContractSkills(t, ctx, repos)
	future := time.Now().Add(24 * time.Hour).Unix()
	if err := repos.Account.Save(ctx, dnfrepo.AccountRecord{
		AccountID: "acc-skill",
		Metadata:  map[string]string{"premium_expire_27": strconv.FormatInt(future, 10)},
	}); err != nil {
		t.Fatal(err)
	}
	owner, err := NewOwner(repos, OwnerOptions{
		Catalog:       skillRuleCatalog(t),
		InitialLevels: map[uint16]int{46: 1},
	})
	if err != nil {
		t.Fatalf("NewOwner() error = %v", err)
	}

	result, err := owner.ApplyBuy(ctx, NewBuyCommand(alignedRequest("acc-skill", 31), BuySkillRequest{
		Count:   1,
		Entries: []BuySkillEntry{{SkillID: 51, LevelDelta: 1}},
	}))
	if err != nil {
		t.Fatalf("ApplyBuy() error = %v", err)
	}
	if result.ExpiredContractSkillsReset {
		t.Fatalf("active contract must not sweep: %+v", result)
	}
	record, _, _ := repos.Skill.Load(ctx, "31")
	if record.Skills[51].Level != 3 || record.Skills[52].Level != 1 {
		t.Fatalf("contract-learned skills changed: %+v", record.Skills)
	}
}

// TestOwnerApplyBuyStillEnforcesTheAwakeningDimension pins the split between the
// two PVF fitness dimensions: the class branch is the client skill tree's call,
// but the awakening stage stays server-enforced because the first awakening is
// granted by its own transition and the second one requires its quest.
func TestOwnerApplyBuyStillEnforcesTheAwakeningDimension(t *testing.T) {
	ctx := context.Background()
	repos := seededSkillOwnerRepositories(t, ctx)
	character, _, _ := repos.Character.Load(ctx, "31")
	// The row is branch-0 fit ([skill fitness growtype] 0) but declares
	// [skill fitness second growtype] 2, so awakening stage 1 is still too early.
	character.Stats["grow_type"] = 0x10
	if err := repos.Character.Save(ctx, character); err != nil {
		t.Fatal(err)
	}
	owner, err := NewOwner(repos, OwnerOptions{Catalog: secondAwakeningSkillCatalog(t), InitialLevels: map[uint16]int{46: 1}})
	if err != nil {
		t.Fatal(err)
	}
	command := NewBuyCommand(alignedRequest("acc-skill", 31), BuySkillRequest{Count: 1, Entries: []BuySkillEntry{{SkillID: 46, LevelDelta: 1}}})
	if _, err := owner.ApplyBuy(ctx, command); !errors.Is(err, ErrSkillGrowType) {
		t.Fatalf("first-awakening purchase error = %v, want ErrSkillGrowType", err)
	}
	character, _, _ = repos.Character.Load(ctx, "31")
	character.Stats["grow_type"] = 0x20
	if err := repos.Character.Save(ctx, character); err != nil {
		t.Fatal(err)
	}
	result, err := owner.ApplyBuy(ctx, command)
	if err != nil {
		t.Fatalf("second-awakening purchase error = %v", err)
	}
	if len(result.Entries) != 1 || result.Entries[0].SkillID != 46 || result.Points.RemainingSP != 80 {
		t.Fatalf("second-awakening purchase result = %+v", result)
	}
}

// TestOwnerApplyBuyLearnsTreeRowWithoutOwnBranchCap pins the tree-authority rule
// on the profession-branch dimension with the shape that produced the live
// report: the PVF caps only the declared branches, the character's own branch
// row is zero, and the client still offers the row. The purchase must succeed,
// and the declared cap must still bound it.
func TestOwnerApplyBuyLearnsTreeRowWithoutOwnBranchCap(t *testing.T) {
	ctx := context.Background()
	repos := seededSkillOwnerRepositories(t, ctx)
	owner, err := NewOwner(repos, OwnerOptions{Catalog: growthMaximumSkillCatalog(t), InitialLevels: map[uint16]int{46: 1}})
	if err != nil {
		t.Fatal(err)
	}
	command := NewBuyCommand(alignedRequest("acc-skill", 31), BuySkillRequest{Count: 1, Entries: []BuySkillEntry{{SkillID: 46, LevelDelta: 1}}})
	result, err := owner.ApplyBuy(ctx, command)
	if err != nil {
		t.Fatalf("base-profession purchase error = %v", err)
	}
	if len(result.Entries) != 1 || result.Entries[0].SkillID != 46 || result.Points.RemainingSP != 80 {
		t.Fatalf("base-profession purchase result = %+v", result)
	}
	// [growtype maximum level] 0 20 still caps this branch at 20: level 1 + 20
	// targets 21.
	over := NewBuyCommand(alignedRequest("acc-skill", 31), BuySkillRequest{Count: 1, Entries: []BuySkillEntry{{SkillID: 46, LevelDelta: 20}}})
	if _, err := owner.ApplyBuy(ctx, over); !errors.Is(err, ErrSkillLevel) {
		t.Fatalf("over-cap purchase error = %v, want ErrSkillLevel", err)
	}
	// A class-changed character keeps the same caps.
	character, _, _ := repos.Character.Load(ctx, "31")
	character.Stats["grow_type"] = 1
	if err := repos.Character.Save(ctx, character); err != nil {
		t.Fatal(err)
	}
	fitting := NewBuyCommand(alignedRequest("acc-skill", 31), BuySkillRequest{Count: 1, Entries: []BuySkillEntry{{SkillID: 46, LevelDelta: 1}}})
	if _, err := owner.ApplyBuy(ctx, fitting); err != nil {
		t.Fatalf("class-profession purchase error = %v", err)
	}
}

func TestOwnerApplyBuySingleRefundModeDoesNotRequireForgetRiverWater(t *testing.T) {
	ctx := context.Background()
	repos := seededSkillOwnerRepositories(t, ctx)
	record, _, _ := repos.Skill.Load(ctx, "31")
	record.Skills[46] = dnfrepo.SkillState{Level: 2, Enabled: true}
	record.Points.RemainingSP = 80
	if err := repos.Skill.Save(ctx, record); err != nil {
		t.Fatal(err)
	}
	if err := repos.Inventory.Save(ctx, dnfrepo.InventoryRecord{CharacterID: "31", Slots: map[string]dnfrepo.ItemStack{}}); err != nil {
		t.Fatal(err)
	}
	owner, err := NewOwner(repos, OwnerOptions{
		Catalog:       skillRuleCatalog(t),
		InitialLevels: map[uint16]int{46: 1},
	})
	if err != nil {
		t.Fatalf("NewOwner() error = %v", err)
	}

	result, err := owner.ApplyBuy(ctx, NewBuyCommand(alignedRequest("acc-skill", 31), BuySkillRequest{
		Count:     1,
		Entries:   []BuySkillEntry{{SkillID: 46, RefundFlag: 1, LevelDelta: 1}},
		FinalMode: currentEXESingleSkillMode,
	}))
	if err != nil {
		t.Fatalf("ApplyBuy() error = %v", err)
	}
	if result.ConsumedRefundItem || result.Points.RemainingSP != 100 {
		t.Fatalf("result = %+v, want free single-skill refund to 100 SP", result)
	}
	skill, _, _ := repos.Skill.Load(ctx, "31")
	if skill.Skills[46].Level != 1 {
		t.Fatalf("skill 46 level = %d, want 1", skill.Skills[46].Level)
	}
}

func TestOwnerApplyBuyContractRefundRequiresActiveLetheContract(t *testing.T) {
	ctx := context.Background()
	repos := seededSkillOwnerRepositories(t, ctx)
	owner, err := NewOwner(repos, OwnerOptions{
		Catalog:       skillRuleCatalog(t),
		InitialLevels: map[uint16]int{46: 1},
	})
	if err != nil {
		t.Fatalf("NewOwner() error = %v", err)
	}

	_, err = owner.ApplyBuy(ctx, NewBuyCommand(alignedRequest("acc-skill", 31), BuySkillRequest{
		Count:     1,
		Entries:   []BuySkillEntry{{SkillID: 46, RefundFlag: 1, LevelDelta: 1}},
		FinalMode: currentEXELetheContractMode,
	}))
	if !errors.Is(err, ErrSkillRefundContractRequired) {
		t.Fatalf("ApplyBuy() error = %v, want ErrSkillRefundContractRequired", err)
	}
}
