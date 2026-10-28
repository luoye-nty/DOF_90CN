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

func TestOwnerQuestCompletionTicketCommitsItemAndQuestAtomically(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "77", AccountID: "acc", Job: "2", Level: 90, Stats: map[string]int64{"grow_type": 1},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "77",
		Slots: map[string]dnfrepo.ItemStack{
			"0:10": {ItemID: 10164630, Count: 2, Extra: map[string]string{"pvf_path": "stackable/10164001/10164630.stk"}},
		},
	}); err != nil {
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
		SlotIndex: 10, ListType: 0, InstanceValue: 1234, ItemCode: 10164630,
	})
	resolver := func(int64) (alignedcmd.StackableUseResolution, error) {
		return alignedcmd.StackableUseResolution{
			ItemID: 10164630, PVFPath: "stackable/10164001/10164630.stk", StackableType: "[etc]",
			QuestCompletionTicket: &alignedcmd.QuestCompletionTicketResolution{ActionType: "[gm quest clear]", MinimumLevel: 90},
		}, nil
	}
	planner := func(character dnfrepo.CharacterRecord, record dnfrepo.QuestRecord, _ alignedcmd.QuestCompletionTicketResolution, completedAt time.Time) (alignedcmd.QuestCompletionTicketPlan, error) {
		record = dnfrepo.CloneQuest(record)
		if record.States == nil {
			record.States = make(map[int64]dnfrepo.QuestState)
		}
		record.States[3145] = dnfrepo.QuestState{Status: "completed", UpdatedAt: completedAt}
		record.UpdatedAt = completedAt
		return alignedcmd.QuestCompletionTicketPlan{
			Quest: record, Character: character, QuestFields: []dnfrepo.QuestField{dnfrepo.QuestFieldStates}, CompletedQuestIDs: []int64{3145},
		}, nil
	}
	result, err := owner.UseStackableWithAllResolvers(ctx, cmd, nil, nil, resolver, planner, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !result.QuestCompletionTicketApplied || result.RemainingCount != 1 || result.QuestCompletionCount != 1 {
		t.Fatalf("result=%+v", result)
	}
	inventory, found, err := repositories.Inventory.Load(ctx, "77")
	if err != nil || !found || inventory.Slots["0:10"].Count != 1 {
		t.Fatalf("inventory=%+v found=%t err=%v", inventory, found, err)
	}
	quest, found, err := repositories.Quest.Load(ctx, "77")
	if err != nil || !found || quest.States[3145].Status != "completed" {
		t.Fatalf("quest=%+v found=%t err=%v", quest, found, err)
	}
}

func TestOwnerQuestCompletionTicketPlannerFailureKeepsSourceItem(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	_ = repositories.Character.Save(ctx, dnfrepo.CharacterRecord{CharacterID: "77", Job: "2", Level: 90})
	_ = repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{CharacterID: "77", Slots: map[string]dnfrepo.ItemStack{
		"0:10": {ItemID: 10164630, Count: 1, Extra: map[string]string{"pvf_path": "stackable/ticket.stk"}},
	}})
	_ = repositories.Quest.Save(ctx, dnfrepo.QuestRecord{CharacterID: "77"})
	owner, _ := NewOwner(repositories)
	cmd := NewUseStackableCommand(alignedcmd.Request{SelectedCharacterID: 77}, UseStackableRequest{SlotIndex: 10, ListType: 0, InstanceValue: 1, ItemCode: 10164630})
	resolver := func(int64) (alignedcmd.StackableUseResolution, error) {
		return alignedcmd.StackableUseResolution{ItemID: 10164630, PVFPath: "stackable/ticket.stk", StackableType: "[etc]", QuestCompletionTicket: &alignedcmd.QuestCompletionTicketResolution{ActionType: "[gm quest clear]"}}, nil
	}
	wantErr := errors.New("no applicable quest")
	_, err := owner.UseStackableWithAllResolvers(ctx, cmd, nil, nil, resolver, func(dnfrepo.CharacterRecord, dnfrepo.QuestRecord, alignedcmd.QuestCompletionTicketResolution, time.Time) (alignedcmd.QuestCompletionTicketPlan, error) {
		return alignedcmd.QuestCompletionTicketPlan{}, wantErr
	}, nil)
	if !errors.Is(err, wantErr) {
		t.Fatalf("error=%v want=%v", err, wantErr)
	}
	inventory, _, _ := repositories.Inventory.Load(ctx, "77")
	if inventory.Slots["0:10"].Count != 1 {
		t.Fatalf("source item changed after planner failure: %+v", inventory.Slots["0:10"])
	}
}
