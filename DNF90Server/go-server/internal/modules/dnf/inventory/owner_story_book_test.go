package inventory

import (
	"context"
	"errors"
	"testing"
	"time"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

func TestOwnerStoryBookAtomicallyConsumesSourceAndAcceptsQuest(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "77", AccountID: "acc", Job: "11", Level: 90, Stats: map[string]int64{"grow_type": 33},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{CharacterID: "77", Slots: map[string]dnfrepo.ItemStack{
		"0:10": {ItemID: 10149086, Count: 2, Extra: map[string]string{
			"item_kind": "stackable", "pvf_path": "stackable/storybook/peace_wing_3.stk", "stackable_type": "[waste]",
		}},
	}}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Quest.Save(ctx, dnfrepo.QuestRecord{CharacterID: "77"}); err != nil {
		t.Fatal(err)
	}
	owner, err := NewOwner(repositories)
	if err != nil {
		t.Fatal(err)
	}
	cmd := NewUseStackableCommand(alignedcmd.Request{AccountID: "acc", SelectedCharacterID: 77}, UseStackableRequest{
		SlotIndex: 10, ListType: 0, InstanceValue: 1234, ItemCode: 10149086,
	})
	resolver := storyBookTestResolver()
	planner := func(_ dnfrepo.CharacterRecord, record dnfrepo.QuestRecord, book alignedcmd.StoryBookResolution, acceptedAt time.Time) (alignedcmd.StoryBookPlan, error) {
		record = dnfrepo.CloneQuest(record)
		if record.States == nil {
			record.States = make(map[int64]dnfrepo.QuestState)
		}
		record.States[book.QuestID] = dnfrepo.QuestState{Status: "active", ProgressValue: 7, UpdatedAt: acceptedAt}
		record.UpdatedAt = acceptedAt
		return alignedcmd.StoryBookPlan{
			Quest: record, QuestFields: []dnfrepo.QuestField{dnfrepo.QuestFieldStates}, QuestID: book.QuestID, InitTrigger: 7,
		}, nil
	}
	result, err := owner.UseStackableWithAllResolvers(ctx, cmd, nil, nil, resolver, nil, nil, planner)
	if err != nil {
		t.Fatal(err)
	}
	if !result.StoryBookApplied || result.StoryBookQuestID != 3142 || result.StoryBookInitTrigger != 7 || result.RemainingCount != 1 {
		t.Fatalf("result=%+v", result)
	}
	inventory, found, err := repositories.Inventory.Load(ctx, "77")
	if err != nil || !found || inventory.Slots["0:10"].Count != 1 {
		t.Fatalf("inventory=%+v found=%t err=%v", inventory, found, err)
	}
	quest, found, err := repositories.Quest.Load(ctx, "77")
	if err != nil || !found || quest.States[3142].Status != "active" || quest.States[3142].ProgressValue != 7 {
		t.Fatalf("quest=%+v found=%t err=%v", quest, found, err)
	}
	wire := ownerAppliedUseStackableResult(44, cmd, result)
	if len(wire.PostActions) != 1 || wire.PostActions[0] != alignedcmd.PostActionRefreshSelectedQuestState {
		t.Fatalf("post actions=%v", wire.PostActions)
	}
}

func TestOwnerStoryBookPlannerFailureRollsBackSource(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	_ = repositories.Character.Save(ctx, dnfrepo.CharacterRecord{CharacterID: "77", AccountID: "acc", Job: "11", Level: 90})
	_ = repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{CharacterID: "77", Slots: map[string]dnfrepo.ItemStack{
		"0:10": {ItemID: 10149086, Count: 1, Extra: map[string]string{"pvf_path": "stackable/storybook/peace_wing_3.stk"}},
	}})
	_ = repositories.Quest.Save(ctx, dnfrepo.QuestRecord{CharacterID: "77"})
	owner, _ := NewOwner(repositories)
	cmd := NewUseStackableCommand(alignedcmd.Request{AccountID: "acc", SelectedCharacterID: 77}, UseStackableRequest{
		SlotIndex: 10, ListType: 0, InstanceValue: 1, ItemCode: 10149086,
	})
	wantErr := errors.New("quest is not acceptable")
	_, err := owner.UseStackableWithAllResolvers(ctx, cmd, nil, nil, storyBookTestResolver(), nil, nil,
		func(dnfrepo.CharacterRecord, dnfrepo.QuestRecord, alignedcmd.StoryBookResolution, time.Time) (alignedcmd.StoryBookPlan, error) {
			return alignedcmd.StoryBookPlan{}, wantErr
		})
	if !errors.Is(err, wantErr) {
		t.Fatalf("error=%v want=%v", err, wantErr)
	}
	inventory, _, _ := repositories.Inventory.Load(ctx, "77")
	if inventory.Slots["0:10"].Count != 1 {
		t.Fatalf("source changed after planner failure: %+v", inventory.Slots["0:10"])
	}
	quest, _, _ := repositories.Quest.Load(ctx, "77")
	if len(quest.States) != 0 {
		t.Fatalf("quest changed after planner failure: %+v", quest)
	}
}

func storyBookTestResolver() alignedcmd.StackableUseResolver {
	return func(int64) (alignedcmd.StackableUseResolution, error) {
		return alignedcmd.StackableUseResolution{
			ItemID: 10149086, PVFPath: "stackable/storybook/peace_wing_3.stk", StackableType: "[waste]",
			StoryBook: &alignedcmd.StoryBookResolution{ActionType: "[story book]", QuestID: 3142, MinimumLevel: 85, MaximumLevel: 99},
		}, nil
	}
}
