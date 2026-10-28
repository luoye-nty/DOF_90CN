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

func TestOwnerDirectExperienceConsumableCommitsProgressionAndSourceAtomically(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "77", AccountID: "acc", Job: "2", Level: 20,
		Stats: map[string]int64{"exp": 500000, "grow_type": 0, "hp": 100},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Skill.Save(ctx, dnfrepo.SkillRecord{
		CharacterID: "77", Points: dnfrepo.SkillPointState{TotalSP: 100, RemainingSP: 25, SyncedLevel: 20},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "77",
		Slots: map[string]dnfrepo.ItemStack{
			"0:10": {
				ItemID: 690001552, Count: 2,
				Extra: map[string]string{
					"item_kind": "stackable", "pvf_path": "stackable/event/tau_book.stk", "stackable_type": "[etc]",
				},
			},
		},
	}); err != nil {
		t.Fatal(err)
	}
	owner, err := NewOwner(repositories)
	if err != nil {
		t.Fatal(err)
	}
	cmd := NewUseStackableCommand(alignedcmd.Request{AccountID: "acc", SelectedCharacterID: 77}, UseStackableRequest{
		SlotIndex: 10, ListType: listTypeMain, InstanceValue: 1234, ItemCode: 690001552,
	})
	resolver := func(itemID int64) (alignedcmd.StackableUseResolution, error) {
		return alignedcmd.StackableUseResolution{
			ItemID: itemID, PVFPath: "stackable/event/tau_book.stk", StackableType: "[etc]",
			DirectExperience: &alignedcmd.DirectExperienceConsumableResolution{
				ItemID: itemID, PVFPath: "stackable/event/tau_book.stk", EffectType: "[expup]", EffectValues: []int64{1000}, MinimumLevel: 20, MaximumLevel: 84,
			},
		}, nil
	}
	planner := func(character dnfrepo.CharacterRecord, skill dnfrepo.SkillRecord, effect alignedcmd.DirectExperienceConsumableResolution, updatedAt time.Time) (alignedcmd.DirectExperienceConsumablePlan, error) {
		if character.Level != 20 || character.Stats["exp"] != 500000 || skill.Points.SyncedLevel != 20 ||
			effect.EffectType != "[expup]" || len(effect.EffectValues) != 1 || effect.EffectValues[0] != 1000 {
			t.Fatalf("character=%+v skill=%+v effect=%+v", character, skill, effect)
		}
		nextCharacter := dnfrepo.CloneCharacter(character)
		nextCharacter.Stats["exp"] = 501000
		nextCharacter.UpdatedAt = updatedAt
		nextSkill := dnfrepo.CloneSkill(skill)
		nextSkill.UpdatedAt = updatedAt
		return alignedcmd.DirectExperienceConsumablePlan{
			Character: nextCharacter, Skill: nextSkill,
			CharacterFields: []dnfrepo.CharacterField{dnfrepo.CharacterFieldStats},
			Gain:            1000, PreviousLevel: 20, NewLevel: 20, PreviousExperience: 500000, NewExperience: 501000,
		}, nil
	}
	result, err := owner.UseStackableWithAllResolvers(ctx, cmd, nil, nil, resolver, nil, planner)
	if err != nil {
		t.Fatal(err)
	}
	if !result.DirectExperienceApplied || result.DirectExperienceGain != 1000 ||
		result.DirectExperiencePreviousTotal != 500000 || result.DirectExperienceNewTotal != 501000 ||
		result.RemainingCount != 1 {
		t.Fatalf("result=%+v", result)
	}
	character, _, _ := repositories.Character.Load(ctx, "77")
	inventory, _, _ := repositories.Inventory.Load(ctx, "77")
	if character.Stats["exp"] != 501000 || inventory.Slots["0:10"].Count != 1 {
		t.Fatalf("character=%+v inventory=%+v", character, inventory)
	}

	wire := ownerAppliedUseStackableResult(44, cmd, result)
	if len(wire.ItemSlotRefreshes) != 1 || len(wire.PostActions) != 1 ||
		wire.PostActions[0] != alignedcmd.PostActionRefreshSelectedCharacterProgression {
		t.Fatalf("wire refreshes=%+v post_actions=%+v", wire.ItemSlotRefreshes, wire.PostActions)
	}
}

func TestOwnerDirectExperienceConsumablePlannerFailureConsumesNothing(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	_ = repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "77", AccountID: "acc", Job: "2", Level: 20, Stats: map[string]int64{"exp": 500000},
	})
	_ = repositories.Skill.Save(ctx, dnfrepo.SkillRecord{
		CharacterID: "77", Points: dnfrepo.SkillPointState{SyncedLevel: 20},
	})
	_ = repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{CharacterID: "77", Slots: map[string]dnfrepo.ItemStack{
		"0:10": {ItemID: 690001552, Count: 1, Extra: map[string]string{
			"item_kind": "stackable", "pvf_path": "stackable/event/tau_book.stk", "stackable_type": "[etc]",
		}},
	}})
	owner, _ := NewOwner(repositories)
	cmd := NewUseStackableCommand(alignedcmd.Request{AccountID: "acc", SelectedCharacterID: 77}, UseStackableRequest{
		SlotIndex: 10, ListType: listTypeMain, InstanceValue: 1234, ItemCode: 690001552,
	})
	resolver := func(itemID int64) (alignedcmd.StackableUseResolution, error) {
		return alignedcmd.StackableUseResolution{
			ItemID: itemID, PVFPath: "stackable/event/tau_book.stk", StackableType: "[etc]",
			DirectExperience: &alignedcmd.DirectExperienceConsumableResolution{
				ItemID: itemID, PVFPath: "stackable/event/tau_book.stk", EffectType: "[expup]", EffectValues: []int64{1000},
			},
		}, nil
	}
	wantErr := errors.New("progression table unavailable")
	_, err := owner.UseStackableWithAllResolvers(ctx, cmd, nil, nil, resolver, nil,
		func(dnfrepo.CharacterRecord, dnfrepo.SkillRecord, alignedcmd.DirectExperienceConsumableResolution, time.Time) (alignedcmd.DirectExperienceConsumablePlan, error) {
			return alignedcmd.DirectExperienceConsumablePlan{}, wantErr
		})
	if !errors.Is(err, wantErr) {
		t.Fatalf("error=%v want=%v", err, wantErr)
	}
	character, _, _ := repositories.Character.Load(ctx, "77")
	inventory, _, _ := repositories.Inventory.Load(ctx, "77")
	if character.Stats["exp"] != 500000 || inventory.Slots["0:10"].Count != 1 {
		t.Fatalf("rollback failed character=%+v inventory=%+v", character, inventory)
	}
}
