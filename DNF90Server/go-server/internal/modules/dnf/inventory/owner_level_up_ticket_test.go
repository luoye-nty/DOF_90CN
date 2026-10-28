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

func TestOwnerLevelUpTicketCommitsProgressionQuestAndSourceAtomically(t *testing.T) {
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
			"0:10": {
				ItemID: 10003634, Count: 2,
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
	owner, err := NewOwner(repositories)
	if err != nil {
		t.Fatal(err)
	}
	cmd := NewUseStackableCommand(alignedcmd.Request{AccountID: "acc", SelectedCharacterID: 77}, UseStackableRequest{
		SlotIndex: 10, ListType: listTypeMain, InstanceValue: 1234, ItemCode: 10003634,
	})
	resolver := testLevelUpTicketResolver
	directPlanner := func(character dnfrepo.CharacterRecord, skill dnfrepo.SkillRecord, effect alignedcmd.DirectExperienceConsumableResolution, updatedAt time.Time) (alignedcmd.DirectExperienceConsumablePlan, error) {
		if character.Level != 1 || effect.EffectType != "[level up ticket]" || len(effect.EffectValues) != 1 || effect.EffectValues[0] != 1 {
			t.Fatalf("character=%+v effect=%+v", character, effect)
		}
		nextCharacter := dnfrepo.CloneCharacter(character)
		nextCharacter.Level = 2
		nextCharacter.Stats["exp"] = 100
		nextCharacter.Stats["hp"] = 120
		nextCharacter.UpdatedAt = updatedAt
		nextSkill := dnfrepo.CloneSkill(skill)
		nextSkill.Points = dnfrepo.SkillPointState{TotalSP: 25, RemainingSP: 25, SyncedLevel: 2}
		nextSkill.UpdatedAt = updatedAt
		return alignedcmd.DirectExperienceConsumablePlan{
			Character: nextCharacter, Skill: nextSkill,
			CharacterFields: []dnfrepo.CharacterField{dnfrepo.CharacterFieldBase, dnfrepo.CharacterFieldStats},
			SkillFields:     []dnfrepo.SkillField{dnfrepo.SkillFieldPoints},
			Gain:            100, PreviousLevel: 1, NewLevel: 2, PreviousExperience: 0, NewExperience: 100, SPGain: 25,
		}, nil
	}
	questPlanner := func(character dnfrepo.CharacterRecord, record dnfrepo.QuestRecord, ticket alignedcmd.QuestCompletionTicketResolution, completedAt time.Time) (alignedcmd.QuestCompletionTicketPlan, error) {
		if character.Level != 1 || ticket.ActionType != "[quest clear]" || ticket.MinimumLevel != 0 || ticket.MaximumLevel != 1 ||
			!ticket.PreserveSideQuests || !ticket.PreserveHellPartyUnlocks {
			t.Fatalf("character=%+v ticket=%+v", character, ticket)
		}
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
	result, err := owner.UseStackableWithAllResolvers(ctx, cmd, nil, nil, resolver, questPlanner, directPlanner)
	if err != nil {
		t.Fatal(err)
	}
	if !result.LevelUpTicketApplied || !result.DirectExperienceApplied || !result.QuestCompletionTicketApplied ||
		result.LevelUpTicketRange != 1 || result.DirectExperiencePreviousLevel != 1 || result.DirectExperienceNewLevel != 2 ||
		result.RemainingCount != 1 || result.QuestCompletionCount != 1 {
		t.Fatalf("result=%+v", result)
	}
	character, _, _ := repositories.Character.Load(ctx, "77")
	skill, _, _ := repositories.Skill.Load(ctx, "77")
	inventory, _, _ := repositories.Inventory.Load(ctx, "77")
	quest, _, _ := repositories.Quest.Load(ctx, "77")
	if character.Level != 2 || character.Stats["exp"] != 100 || character.Stats["hp"] != 120 ||
		skill.Points.SyncedLevel != 2 || skill.Points.TotalSP != 25 || inventory.Slots["0:10"].Count != 1 ||
		quest.States[101].Status != "completed" {
		t.Fatalf("character=%+v skill=%+v inventory=%+v quest=%+v", character, skill, inventory, quest)
	}
}

func TestOwnerLevelUpTicketPlannerFailureRollsBackEveryAggregate(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	_ = repositories.Character.Save(ctx, dnfrepo.CharacterRecord{CharacterID: "77", AccountID: "acc", Job: "2", Level: 1, Stats: map[string]int64{"exp": 0}})
	_ = repositories.Skill.Save(ctx, dnfrepo.SkillRecord{CharacterID: "77", Points: dnfrepo.SkillPointState{SyncedLevel: 1}})
	_ = repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{CharacterID: "77", Slots: map[string]dnfrepo.ItemStack{
		"0:10": {ItemID: 10003634, Count: 1, Extra: map[string]string{"item_kind": "stackable", "pvf_path": "stackable/event/level_up_ticket.stk", "stackable_type": "[etc]"}},
	}})
	_ = repositories.Quest.Save(ctx, dnfrepo.QuestRecord{CharacterID: "77"})
	owner, _ := NewOwner(repositories)
	cmd := NewUseStackableCommand(alignedcmd.Request{AccountID: "acc", SelectedCharacterID: 77}, UseStackableRequest{SlotIndex: 10, ListType: listTypeMain, InstanceValue: 1, ItemCode: 10003634})
	directPlanner := func(character dnfrepo.CharacterRecord, skill dnfrepo.SkillRecord, _ alignedcmd.DirectExperienceConsumableResolution, updatedAt time.Time) (alignedcmd.DirectExperienceConsumablePlan, error) {
		character = dnfrepo.CloneCharacter(character)
		character.Level = 2
		character.Stats["exp"] = 100
		skill.Points.SyncedLevel = 2
		return alignedcmd.DirectExperienceConsumablePlan{
			Character: character, Skill: skill, CharacterFields: []dnfrepo.CharacterField{dnfrepo.CharacterFieldBase, dnfrepo.CharacterFieldStats},
			SkillFields: []dnfrepo.SkillField{dnfrepo.SkillFieldPoints}, Gain: 100, PreviousLevel: 1, NewLevel: 2, NewExperience: 100,
		}, nil
	}
	wantErr := errors.New("quest catalog unavailable")
	_, err := owner.UseStackableWithAllResolvers(ctx, cmd, nil, nil, testLevelUpTicketResolver, func(dnfrepo.CharacterRecord, dnfrepo.QuestRecord, alignedcmd.QuestCompletionTicketResolution, time.Time) (alignedcmd.QuestCompletionTicketPlan, error) {
		return alignedcmd.QuestCompletionTicketPlan{}, wantErr
	}, directPlanner)
	if !errors.Is(err, wantErr) {
		t.Fatalf("error=%v want=%v", err, wantErr)
	}
	character, _, _ := repositories.Character.Load(ctx, "77")
	skill, _, _ := repositories.Skill.Load(ctx, "77")
	inventory, _, _ := repositories.Inventory.Load(ctx, "77")
	if character.Level != 1 || character.Stats["exp"] != 0 || skill.Points.SyncedLevel != 1 || inventory.Slots["0:10"].Count != 1 {
		t.Fatalf("rollback failed character=%+v skill=%+v inventory=%+v", character, skill, inventory)
	}
}

func TestLevelUpTicketResultRequestsOneCompleteProgressionProjection(t *testing.T) {
	cmd := Command{Operation: "use_stackable"}
	result := ownerAppliedUseStackableResult(44, cmd, UseStackableResult{
		ListType: listTypeMain, SlotIndex: 10, ItemID: 10003634, RemainingCount: 0,
		DirectExperienceApplied: true, DirectExperiencePreviousLevel: 1, DirectExperienceNewLevel: 2,
		LevelUpTicketApplied: true, LevelUpTicketRange: 1, QuestCompletionTicketApplied: true, QuestCompletionCount: 2,
	})
	if len(result.PostActions) != 1 || result.PostActions[0] != alignedcmd.PostActionRefreshSelectedCharacterProgression {
		t.Fatalf("post actions=%+v", result.PostActions)
	}
}

func testLevelUpTicketResolver(itemID int64) (alignedcmd.StackableUseResolution, error) {
	return alignedcmd.StackableUseResolution{
		ItemID: itemID, PVFPath: "stackable/event/level_up_ticket.stk", StackableType: "[etc]",
		DirectExperience: &alignedcmd.DirectExperienceConsumableResolution{
			ItemID: itemID, PVFPath: "stackable/event/level_up_ticket.stk", EffectType: "[level up ticket]", EffectValues: []int64{1}, MinimumLevel: 1, MaximumLevel: 79,
		},
		QuestCompletionTicket: &alignedcmd.QuestCompletionTicketResolution{ActionType: "[quest clear]", MinimumLevel: 1, MaximumLevel: 79},
	}, nil
}
