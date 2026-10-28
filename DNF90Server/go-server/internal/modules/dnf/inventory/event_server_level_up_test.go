package inventory

import (
	"bytes"
	"context"
	"testing"
	"time"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

func TestEventServerLevelUpHandlesCapturedOp418AndReturnsNativeDeltaACK(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "77", AccountID: "acc", Job: "2", Level: 1,
		Stats: map[string]int64{"exp": 0, "grow_type": 0, "hp": 100},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Skill.Save(ctx, dnfrepo.SkillRecord{
		CharacterID: "77", Points: dnfrepo.SkillPointState{SyncedLevel: 1},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "77",
		Slots: map[string]dnfrepo.ItemStack{
			"0:65": {
				ItemID: 10003634, Count: 1,
				Extra: map[string]string{
					"item_kind": "stackable", "pvf_path": "stackable/event/level_up_ticket.stk", "stackable_type": "[etc]",
				},
			},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Quest.Save(ctx, dnfrepo.QuestRecord{CharacterID: "77"}); err != nil {
		t.Fatal(err)
	}
	directPlanner := func(character dnfrepo.CharacterRecord, skill dnfrepo.SkillRecord, effect alignedcmd.DirectExperienceConsumableResolution, updatedAt time.Time) (alignedcmd.DirectExperienceConsumablePlan, error) {
		nextCharacter := dnfrepo.CloneCharacter(character)
		nextCharacter.Level = 2
		nextCharacter.Stats["exp"] = 100
		nextCharacter.UpdatedAt = updatedAt
		nextSkill := dnfrepo.CloneSkill(skill)
		nextSkill.Points = dnfrepo.SkillPointState{TotalSP: 25, RemainingSP: 25, TotalTP: 3, RemainingTP: 3, SyncedLevel: 2}
		nextSkill.UpdatedAt = updatedAt
		return alignedcmd.DirectExperienceConsumablePlan{
			Character: nextCharacter, Skill: nextSkill,
			CharacterFields: []dnfrepo.CharacterField{dnfrepo.CharacterFieldBase, dnfrepo.CharacterFieldStats},
			SkillFields:     []dnfrepo.SkillField{dnfrepo.SkillFieldPoints},
			Gain:            100, PreviousLevel: 1, NewLevel: 2, PreviousExperience: 0, NewExperience: 100, SPGain: 25, TPGain: 3,
		}, nil
	}
	questPlanner := func(character dnfrepo.CharacterRecord, record dnfrepo.QuestRecord, ticket alignedcmd.QuestCompletionTicketResolution, completedAt time.Time) (alignedcmd.QuestCompletionTicketPlan, error) {
		record = dnfrepo.CloneQuest(record)
		if record.States == nil {
			record.States = make(map[int64]dnfrepo.QuestState)
		}
		record.States[101] = dnfrepo.QuestState{Status: "completed", UpdatedAt: completedAt}
		record.UpdatedAt = completedAt
		return alignedcmd.QuestCompletionTicketPlan{
			Quest: record, Character: character, QuestFields: []dnfrepo.QuestField{dnfrepo.QuestFieldStates}, CompletedQuestIDs: []int64{101},
		}, nil
	}
	result, err := (handler{}).Handle(ctx, alignedcmd.Request{
		Opcode:                       uint16(dnfenum.CmdPacketRequestEventServerLevelUp),
		Body:                         []byte{0x41, 0x00, listTypeMain},
		AccountID:                    "acc",
		SelectedCharacterID:          77,
		Repositories:                 repositories,
		StackableUseResolver:         testLevelUpTicketResolver,
		DirectExperiencePlanner:      directPlanner,
		QuestCompletionTicketPlanner: questPlanner,
	})
	if err != nil {
		t.Fatal(err)
	}
	wantACK := []byte{1, 0x41, 0x00, listTypeMain, 100, 0, 0, 0, 25, 0, 3, 0}
	if !result.Handled || !result.ResponseAllowed || result.Operation != "event_server_level_up" ||
		len(result.UpperResponses) != 1 || result.UpperResponses[0].MsgID != uint16(dnfenum.CmdPacketRequestEventServerLevelUp) ||
		!bytes.Equal(result.UpperResponses[0].Body, wantACK) {
		t.Fatalf("result=%+v want_ack=%x", result, wantACK)
	}
	if len(result.ItemSlotRefreshes) != 1 || result.ItemSlotRefreshes[0].ListType != listTypeMain || result.ItemSlotRefreshes[0].SlotIndex != 65 ||
		len(result.PostActions) != 1 || result.PostActions[0] != alignedcmd.PostActionRefreshSelectedCharacterProgression {
		t.Fatalf("refresh plan=%+v post=%+v", result.ItemSlotRefreshes, result.PostActions)
	}
	character, _, _ := repositories.Character.Load(ctx, "77")
	skill, _, _ := repositories.Skill.Load(ctx, "77")
	inventory, _, _ := repositories.Inventory.Load(ctx, "77")
	if character.Level != 2 || character.Stats["exp"] != 100 || skill.Points.RemainingSP != 25 || skill.Points.RemainingTP != 3 {
		t.Fatalf("character=%+v skill=%+v", character, skill)
	}
	if _, found := inventory.Slots["0:65"]; found {
		t.Fatalf("source coupon was not consumed: %+v", inventory.Slots["0:65"])
	}
}

func TestEventServerLevelUpMalformedRequestReturnsNativeErrorWithoutMutation(t *testing.T) {
	result, err := (handler{}).Handle(context.Background(), alignedcmd.Request{
		Opcode: uint16(dnfenum.CmdPacketRequestEventServerLevelUp),
		Body:   []byte{0x41, 0x00},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Handled || !result.ResponseAllowed || len(result.UpperResponses) != 1 ||
		!bytes.Equal(result.UpperResponses[0].Body, []byte{0, eventServerLevelUpFailureCode}) {
		t.Fatalf("result=%+v", result)
	}
}
