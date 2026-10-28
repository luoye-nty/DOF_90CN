package quest

import (
	"context"
	"testing"
	"time"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

func TestAutoCompleteCarryForwardRepairGrantsOnlyOutstandingStoryItemOnce(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "17",
		Job:         "2",
		Level:       35,
		Stats:       map[string]int64{"grow_type": 0},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "17",
		Slots:       map[string]dnfrepo.ItemStack{"0:10": {ItemID: 9001, Count: 3}},
	}); err != nil {
		t.Fatal(err)
	}
	completedNoReward := dnfrepo.QuestState{
		Status: "completed",
		Extra: map[string]string{
			"auto_completed":       "true",
			"auto_complete_reason": "epic_below_character_level_no_reward",
			"reward_state":         finishRewardGranted,
		},
	}
	if err := repositories.Quest.Save(ctx, dnfrepo.QuestRecord{
		CharacterID: "17",
		States: map[int64]dnfrepo.QuestState{
			100: completedNoReward,
			101: completedNoReward,
			300: {Status: "active"},
		},
	}); err != nil {
		t.Fatal(err)
	}

	// 101 rewards an item that only a [side] consumer wants; a side quest is
	// not a story consumer, so 7002 must not be granted.
	definitions := []Definition{
		{ID: 100, Path: "n_quest/producer.qst", Grade: "[epic]", Type: "[meet npc]", Job: "[all]", LevelMin: 1, LevelMax: 99, RewardType: "[item]", RewardItems: []RewardItemRule{{ItemID: 7001, Count: 1}}},
		{ID: 101, Path: "n_quest/unrelated.qst", Grade: "[epic]", Type: "[meet npc]", Job: "[all]", LevelMin: 1, LevelMax: 99, RewardType: "[item]", RewardItems: []RewardItemRule{{ItemID: 7002, Count: 5}}},
		{ID: 200, Path: "n_quest/parent.qst", Grade: "[epic]", Type: "[quest clear]", Job: "[all]", LevelMin: 1, LevelMax: 99, ExposedByNPC: 1, IntData: []int64{250}},
		{ID: 250, Path: "n_quest/consumer.qst", Grade: "[sub]", Type: "[seek n meet npc]", Job: "[all]", LevelMin: 1, LevelMax: 99, MainQuestID: 200, IntData: []int64{7001, 1, 303}},
		{ID: 300, Path: "n_quest/unrelated_side_consumer.qst", Grade: "[side]", Type: "[seeking]", Job: "[all]", LevelMin: 1, LevelMax: 99, ExposedByNPC: 1, IntData: []int64{7002, 1}},
	}
	catalog := &Catalog{ordered: definitions, byID: make(map[int64]Definition, len(definitions))}
	for _, definition := range definitions {
		catalog.byID[definition.ID] = definition
	}
	owner, err := NewOwner(repositories)
	if err != nil {
		t.Fatal(err)
	}
	input := AutoCompleteCarryForwardRepairInput{
		CharacterID:  "17",
		RepairedAt:   time.Date(2026, 9, 5, 10, 30, 0, 0, time.UTC),
		AllocateItem: finishOwnerAllocator(nil),
	}
	result, err := owner.ApplyAutoCompleteCarryForwardRepair(ctx, catalog, input)
	if err != nil {
		t.Fatal(err)
	}
	if result.Idempotent || len(result.Items) != 1 || result.Items[0].ItemID != 7001 || result.Items[0].Delta != 1 || result.Items[0].PostCount != 1 {
		t.Fatalf("carry-forward result=%+v", result)
	}
	stored, found, err := repositories.Inventory.Load(ctx, "17")
	if err != nil || !found || countItemInMain(stored, 7001) != 1 || countItemInMain(stored, 7002) != 0 {
		t.Fatalf("stored inventory found=%t err=%v record=%+v", found, err, stored)
	}

	replayed, err := owner.ApplyAutoCompleteCarryForwardRepair(ctx, catalog, input)
	if err != nil {
		t.Fatal(err)
	}
	if !replayed.Idempotent || len(replayed.Items) != 0 {
		t.Fatalf("replayed carry-forward=%+v", replayed)
	}
	stored, found, err = repositories.Inventory.Load(ctx, "17")
	if err != nil || !found || countItemInMain(stored, 7001) != 1 {
		t.Fatalf("replay duplicated inventory found=%t err=%v record=%+v", found, err, stored)
	}
}

// Mirrors the 3509 shape: several producers each reward one fragment, a
// composite [quest clear] producer both rewards and consumes fragments through
// its children, and the consumer asks for the cumulative total.
func TestAutoCompleteCarryForwardRepairAggregatesCompositeStoryItemBalance(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "21",
		Job:         "6",
		Level:       62,
		Stats:       map[string]int64{"grow_type": 20},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{CharacterID: "21", Slots: map[string]dnfrepo.ItemStack{}}); err != nil {
		t.Fatal(err)
	}
	completedNoReward := dnfrepo.QuestState{
		Status: "completed",
		Extra: map[string]string{
			"auto_completed":       "true",
			"auto_complete_reason": "epic_below_character_level_no_reward",
			"reward_state":         finishRewardGranted,
		},
	}
	if err := repositories.Quest.Save(ctx, dnfrepo.QuestRecord{
		CharacterID: "21",
		States: map[int64]dnfrepo.QuestState{
			100: completedNoReward,
			110: completedNoReward,
			120: completedNoReward,
			300: {Status: "active"},
		},
	}); err != nil {
		t.Fatal(err)
	}

	definitions := []Definition{
		{ID: 100, Path: "n_quest/producer_1.qst", Grade: "[epic]", Type: "[meet npc]", Job: "[all]", LevelMin: 1, LevelMax: 99, RewardType: "[item]", RewardItems: []RewardItemRule{{ItemID: 7001, Count: 1}}},
		{ID: 110, Path: "n_quest/producer_2.qst", Grade: "[epic]", Type: "[meet npc]", Job: "[all]", LevelMin: 1, LevelMax: 99, RewardType: "[item]", RewardItems: []RewardItemRule{{ItemID: 7001, Count: 2}}},
		{ID: 120, Path: "n_quest/composite.qst", Grade: "[epic]", Type: "[quest clear]", Job: "[all]", LevelMin: 1, LevelMax: 99, IntData: []int64{121, 122, 123}},
		{ID: 121, Path: "n_quest/composite_reward_1.qst", Grade: "[sub]", Type: "[hunt monster]", Job: "[all]", LevelMin: 1, LevelMax: 99, MainQuestID: 120, RewardType: "[item]", RewardItems: []RewardItemRule{{ItemID: 7001, Count: 2}}},
		{ID: 122, Path: "n_quest/composite_exchange.qst", Grade: "[sub]", Type: "[seeking]", Job: "[all]", LevelMin: 1, LevelMax: 99, MainQuestID: 120, IntData: []int64{7001, 3}, RewardType: "[item]", RewardItems: []RewardItemRule{{ItemID: 7001, Count: 8}}},
		{ID: 123, Path: "n_quest/composite_report.qst", Grade: "[sub]", Type: "[meet npc]", Job: "[all]", LevelMin: 1, LevelMax: 99, MainQuestID: 120},
		{ID: 300, Path: "n_quest/consumer.qst", Grade: "[epic]", Type: "[seek n meet npc]", Job: "[all]", LevelMin: 1, LevelMax: 99, ExposedByNPC: 1, IntData: []int64{7001, 10, 303}},
	}
	catalog := &Catalog{ordered: definitions, byID: make(map[int64]Definition, len(definitions))}
	for _, definition := range definitions {
		catalog.byID[definition.ID] = definition
	}
	owner, err := NewOwner(repositories)
	if err != nil {
		t.Fatal(err)
	}
	input := AutoCompleteCarryForwardRepairInput{
		CharacterID:  "21",
		RepairedAt:   time.Date(2026, 9, 5, 13, 0, 0, 0, time.UTC),
		AllocateItem: finishOwnerAllocator(nil),
	}
	result, err := owner.ApplyAutoCompleteCarryForwardRepair(ctx, catalog, input)
	if err != nil {
		t.Fatal(err)
	}
	// 1 + 2 + 2 + 8 rewarded, 3 consumed by the composite's own exchange step:
	// net 10, exactly what the consumer asks for.
	if result.Idempotent || len(result.Items) != 1 || result.Items[0].ItemID != 7001 || result.Items[0].Delta != 10 || result.Items[0].PostCount != 10 {
		t.Fatalf("cumulative carry-forward result=%+v", result)
	}

	replayed, err := owner.ApplyAutoCompleteCarryForwardRepair(ctx, catalog, input)
	if err != nil {
		t.Fatal(err)
	}
	if !replayed.Idempotent || len(replayed.Items) != 0 {
		t.Fatalf("replayed cumulative carry-forward=%+v", replayed)
	}
}

// The producer is auto-completed under the cutoff while the consumer's parent
// sits above it. This is the level-35 shape of 3226 -> 3249/3610: parent
// closure never reaches 3610, so the item has to travel with the completion.
func TestAutoCompleteMainPreservesStoryItemRequiredByNewSuccessor(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "19",
		Job:         "2",
		Level:       35,
		Stats:       map[string]int64{"grow_type": 0},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{CharacterID: "19", Slots: map[string]dnfrepo.ItemStack{}}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Quest.Save(ctx, dnfrepo.QuestRecord{
		CharacterID: "19",
		States:      map[int64]dnfrepo.QuestState{100: {Status: "active"}},
	}); err != nil {
		t.Fatal(err)
	}
	definitions := []Definition{
		{ID: 100, Path: "n_quest/producer.qst", Grade: "[epic]", Type: "[meet npc]", Job: "[all]", LevelMin: 10, LevelMax: 99, ExposedByNPC: 1, RewardType: "[item]", RewardItems: []RewardItemRule{{ItemID: 7001, Count: 1}}},
		{ID: 200, Path: "n_quest/parent.qst", Grade: "[epic]", Type: "[quest clear]", Job: "[all]", LevelMin: 35, LevelMax: 99, ExposedByNPC: 1, PreRequiredGroups: [][]int64{{100}}, IntData: []int64{250}},
		{ID: 250, Path: "n_quest/consumer.qst", Grade: "[sub]", Type: "[seek n meet npc]", Job: "[all]", LevelMin: 35, LevelMax: 99, MainQuestID: 200, IntData: []int64{7001, 1, 303}},
	}
	catalog := &Catalog{ordered: definitions, byID: make(map[int64]Definition, len(definitions))}
	for _, definition := range definitions {
		catalog.byID[definition.ID] = definition
	}
	owner, err := NewOwner(repositories)
	if err != nil {
		t.Fatal(err)
	}
	result, err := owner.ApplyAutoCompleteMain(ctx, catalog, AutoCompleteMainInput{
		CharacterID:   "19",
		Eligibility:   CharacterEligibility{Level: 35, Job: 2, GrowType: 0},
		CutoffLevel:   34,
		TargetQuestID: 100,
		CompletedAt:   time.Date(2026, 9, 5, 11, 0, 0, 0, time.UTC),
		AllocateItem:  finishOwnerAllocator(nil),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.ChangedQuestIDs) != 1 || result.ChangedQuestIDs[0] != 100 || len(result.CarryForwardItems) != 1 || result.CarryForwardItems[0].ItemID != 7001 || result.Idempotent {
		t.Fatalf("auto-complete carry-forward result=%+v", result)
	}
	stored, found, err := repositories.Inventory.Load(ctx, "19")
	if err != nil || !found || countItemInMain(stored, 7001) != 1 {
		t.Fatalf("stored inventory found=%t err=%v record=%+v", found, err, stored)
	}
	// Without an allocator the quest side still commits; the item repair is
	// what needs one, and it must say so rather than silently skip.
	if _, err := owner.ApplyAutoCompleteMain(ctx, catalog, AutoCompleteMainInput{
		CharacterID:   "19",
		Eligibility:   CharacterEligibility{Level: 35, Job: 2, GrowType: 0},
		CutoffLevel:   34,
		TargetQuestID: 100,
		CompletedAt:   time.Date(2026, 9, 5, 11, 1, 0, 0, time.UTC),
	}); err != nil {
		t.Fatalf("replay without allocator must be idempotent (item already held): %v", err)
	}
}

// The bulk pass stops before a [quest clear] parent whose children carry their
// own rewards, so the producers inside that parent are ones the player still
// earns by playing it. Carrying forward only what the pass withheld is the
// whole contract; demanding that it cover the child step's full requirement
// rejected the entire operation instead. This is the live 3449/3456 -> 3458
// (3472 awards 2, 3473 needs 3) shape.
func TestAutoCompleteCarryForwardGrantsOnlyTheWithheldShareOfAnUnreachedComposite(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "23", Job: "6", Level: 62, Stats: map[string]int64{"grow_type": 20},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{CharacterID: "23", Slots: map[string]dnfrepo.ItemStack{}}); err != nil {
		t.Fatal(err)
	}
	completedNoReward := dnfrepo.QuestState{
		Status: "completed",
		Extra: map[string]string{
			"auto_completed":       "true",
			"auto_complete_reason": "epic_below_character_level_no_reward",
			"reward_state":         finishRewardGranted,
		},
	}
	if err := repositories.Quest.Save(ctx, dnfrepo.QuestRecord{
		CharacterID: "23",
		States: map[int64]dnfrepo.QuestState{
			100: completedNoReward,
			110: completedNoReward,
			120: {Status: "active"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	definitions := []Definition{
		{ID: 100, Path: "n_quest/producer_1.qst", Grade: "[epic]", Type: "[clear map]", Job: "[all]", LevelMin: 1, LevelMax: 99, RewardType: "[item]", RewardItems: []RewardItemRule{{ItemID: 7001, Count: 1}}},
		{ID: 110, Path: "n_quest/producer_2.qst", Grade: "[epic]", Type: "[clear map]", Job: "[all]", LevelMin: 1, LevelMax: 99, RewardType: "[item]", RewardItems: []RewardItemRule{{ItemID: 7001, Count: 1}}},
		{ID: 120, Path: "n_quest/composite.qst", Grade: "[epic]", Type: "[quest clear]", Job: "[all]", LevelMin: 1, LevelMax: 99, ExposedByNPC: 1, IntData: []int64{121, 122}},
		{ID: 121, Path: "n_quest/composite_hunt.qst", Grade: "[sub]", Type: "[hunt monster]", Job: "[all]", LevelMin: 1, LevelMax: 99, MainQuestID: 120, RewardType: "[item]", RewardItems: []RewardItemRule{{ItemID: 7001, Count: 2}}},
		{ID: 122, Path: "n_quest/composite_exchange.qst", Grade: "[sub]", Type: "[seeking]", Job: "[all]", LevelMin: 1, LevelMax: 99, MainQuestID: 120, IntData: []int64{7001, 3}, RewardType: "[item]", RewardItems: []RewardItemRule{{ItemID: 7001, Count: 4}}},
	}
	catalog := &Catalog{ordered: definitions, byID: make(map[int64]Definition, len(definitions))}
	for _, definition := range definitions {
		catalog.byID[definition.ID] = definition
	}
	owner, err := NewOwner(repositories)
	if err != nil {
		t.Fatal(err)
	}
	result, err := owner.ApplyAutoCompleteCarryForwardRepair(ctx, catalog, AutoCompleteCarryForwardRepairInput{
		CharacterID:  "23",
		RepairedAt:   time.Date(2026, 9, 10, 13, 0, 0, 0, time.UTC),
		AllocateItem: finishOwnerAllocator(nil),
	})
	if err != nil {
		t.Fatalf("unreached composite must not fail the pass: %v", err)
	}
	// 1 + 1 withheld by the two auto-completed producers. The remaining two of
	// the child step's three come from 121, which the player still plays.
	if result.Idempotent || len(result.Items) != 1 || result.Items[0].ItemID != 7001 ||
		result.Items[0].Delta != 2 || result.Items[0].PostCount != 2 {
		t.Fatalf("withheld-share carry-forward result=%+v", result)
	}
}
