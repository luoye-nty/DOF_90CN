package inventory

import (
	"bytes"
	"context"
	"encoding/binary"
	"testing"
	"time"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

func TestUseStackableAction167AppliesQuestCompletionTicketThroughOp515(t *testing.T) {
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

	body := make([]byte, 19)
	binary.LittleEndian.PutUint16(body[0:2], 10)
	body[2] = listTypeMain
	binary.LittleEndian.PutUint32(body[7:11], questCompletionTicketActionIndex)
	result, err := NewHandler().Handle(ctx, alignedcmd.Request{
		Opcode:              uint16(dnfenum.CmdPacketUseStackableAction),
		Body:                body,
		AccountID:           "acc",
		SelectedCharacterID: 77,
		Repositories:        repositories,
		StackableUseResolver: func(itemID int64) (alignedcmd.StackableUseResolution, error) {
			return alignedcmd.StackableUseResolution{
				ItemID: itemID, PVFPath: "stackable/10164001/10164630.stk", StackableType: "[etc]",
				QuestCompletionTicket: &alignedcmd.QuestCompletionTicketResolution{
					ActionType: "[gm quest clear]", MinimumLevel: 90, MaximumLevel: 90, VillageOnly: true,
				},
			}, nil
		},
		QuestCompletionTicketPlanner: func(character dnfrepo.CharacterRecord, record dnfrepo.QuestRecord, _ alignedcmd.QuestCompletionTicketResolution, completedAt time.Time) (alignedcmd.QuestCompletionTicketPlan, error) {
			record = dnfrepo.CloneQuest(record)
			if record.States == nil {
				record.States = make(map[int64]dnfrepo.QuestState)
			}
			record.States[3145] = dnfrepo.QuestState{Status: "completed", UpdatedAt: completedAt}
			record.UpdatedAt = completedAt
			return alignedcmd.QuestCompletionTicketPlan{
				Quest: record, Character: character,
				QuestFields: []dnfrepo.QuestField{dnfrepo.QuestFieldStates}, CompletedQuestIDs: []int64{3145},
			}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.ResponseAllowed || len(result.UpperResponses) != 1 {
		t.Fatalf("result=%+v", result)
	}
	wantAck := []byte{1, 10, 0, 0, 0, 1, 0, 0, 0, 1}
	if got := result.UpperResponses[0].Body; !bytes.Equal(got, wantAck) {
		t.Fatalf("op515 ACK=%x want=%x", got, wantAck)
	}
	if len(result.ItemSlotRefreshes) != 1 || result.ItemSlotRefreshes[0] != (alignedcmd.ItemSlotRefresh{ListType: listTypeMain, SlotIndex: 10}) {
		t.Fatalf("item refreshes=%+v", result.ItemSlotRefreshes)
	}
	if len(result.PostActions) != 1 || result.PostActions[0] != alignedcmd.PostActionRefreshSelectedQuestState {
		t.Fatalf("post actions=%+v", result.PostActions)
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
