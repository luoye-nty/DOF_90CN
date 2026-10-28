package inventory

import (
	"context"
	"errors"
	"math"
	"testing"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
	dnfskill "longheng.io/server/internal/modules/dnf/skill"
)

func TestOwnerTPBookCommitsSourceBonusAndEverySkillTreeAtomically(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "77", AccountID: "acc", Level: 50,
		Stats: map[string]int64{"bonus_tp": 2},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Skill.Save(ctx, dnfrepo.SkillRecord{
		CharacterID: "77",
		Points:      dnfrepo.SkillPointState{TotalSP: 100, RemainingSP: 70, TotalTP: 12, RemainingTP: 7, SyncedLevel: 50},
		Trees: map[int]dnfrepo.SkillTreeState{
			1: {Points: dnfrepo.SkillPointState{TotalSP: 100, RemainingSP: 60, TotalTP: 12, RemainingTP: 4, SyncedLevel: 50}},
		},
	}); err != nil {
		t.Fatal(err)
	}
	const pvfPath = "stackable/book_fskill1.stk"
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "77",
		Slots: map[string]dnfrepo.ItemStack{
			slotKey(listTypeMain, 10): {
				ItemID: 1204, Count: 2,
				Extra: map[string]string{"item_kind": "stackable", "pvf_path": pvfPath, "stackable_type": "[etc]"},
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
		SlotIndex: 10, ListType: listTypeMain, InstanceValue: 0x11223344, ItemCode: 1204,
	})
	result, err := owner.UseStackableWithAllResolvers(ctx, cmd, nil, nil, tpBookTestResolver(1204, pvfPath, 1), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !result.TPBookApplied || result.TPBookDelta != 1 || result.TPBookPreviousRemaining != 7 ||
		result.TPBookNewRemaining != 8 || result.RemainingCount != 1 {
		t.Fatalf("result=%+v", result)
	}
	character, _, _ := repositories.Character.Load(ctx, "77")
	skill, _, _ := repositories.Skill.Load(ctx, "77")
	inventory, _, _ := repositories.Inventory.Load(ctx, "77")
	if character.Stats["bonus_tp"] != 3 || skill.Points.TotalTP != 13 || skill.Points.RemainingTP != 8 ||
		skill.Trees[1].Points.TotalTP != 13 || skill.Trees[1].Points.RemainingTP != 5 ||
		inventory.Slots[slotKey(listTypeMain, 10)].Count != 1 {
		t.Fatalf("character=%+v skill=%+v inventory=%+v", character, skill, inventory)
	}
}

func TestOwnerTPBookRejectsLevelAndOverflowWithoutConsuming(t *testing.T) {
	for _, test := range []struct {
		name    string
		level   int
		points  dnfrepo.SkillPointState
		bonusTP int64
	}{
		{name: "below PVF minimum", level: 49, points: dnfrepo.SkillPointState{TotalTP: 10, RemainingTP: 10, SyncedLevel: 49}},
		{name: "current EXE u16 overflow", level: 50, points: dnfrepo.SkillPointState{TotalTP: math.MaxUint16, RemainingTP: math.MaxUint16, SyncedLevel: 50}},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			repositories := dnfrepomemory.NewMemoryGroup()
			_ = repositories.Character.Save(ctx, dnfrepo.CharacterRecord{CharacterID: "77", AccountID: "acc", Level: test.level, Stats: map[string]int64{"bonus_tp": test.bonusTP}})
			_ = repositories.Skill.Save(ctx, dnfrepo.SkillRecord{CharacterID: "77", Points: test.points})
			const pvfPath = "stackable/book_fskill1.stk"
			_ = repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{CharacterID: "77", Slots: map[string]dnfrepo.ItemStack{
				slotKey(listTypeMain, 10): {ItemID: 1204, Count: 1, Extra: map[string]string{"item_kind": "stackable", "pvf_path": pvfPath, "stackable_type": "[etc]"}},
			}})
			owner, _ := NewOwner(repositories)
			cmd := NewUseStackableCommand(alignedcmd.Request{AccountID: "acc", SelectedCharacterID: 77}, UseStackableRequest{SlotIndex: 10, ListType: listTypeMain, InstanceValue: 1, ItemCode: 1204})
			_, err := owner.UseStackableWithAllResolvers(ctx, cmd, nil, nil, tpBookTestResolver(1204, pvfPath, 1), nil, nil)
			if !errors.Is(err, ErrTPBookContract) {
				t.Fatalf("error=%v want ErrTPBookContract", err)
			}
			inventory, _, _ := repositories.Inventory.Load(ctx, "77")
			character, _, _ := repositories.Character.Load(ctx, "77")
			if inventory.Slots[slotKey(listTypeMain, 10)].Count != 1 || character.Stats["bonus_tp"] != test.bonusTP {
				t.Fatalf("failed use mutated inventory=%+v character=%+v", inventory, character)
			}
		})
	}
}

func TestTPBookResultRequestsNativeSkillSnapshot(t *testing.T) {
	result := ownerAppliedUseStackableResult(44, Command{Operation: "use_stackable"}, UseStackableResult{
		ListType: listTypeMain, SlotIndex: 10, ItemID: 1204, TPBookApplied: true, TPBookDelta: 1,
	})
	if len(result.PostActions) != 1 || result.PostActions[0] != alignedcmd.PostActionRefreshSelectedActorSkills {
		t.Fatalf("post actions=%+v", result.PostActions)
	}
}

func TestOwnerSPBookRepairsEveryTreeAndCommitsBonusAtomically(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "77", AccountID: "acc", Job: "0", Level: 50,
		Stats: map[string]int64{"bonus_sp": 2, "bonus_tp": 1},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Skill.Save(ctx, dnfrepo.SkillRecord{
		CharacterID: "77",
		Points:      dnfrepo.SkillPointState{TotalSP: 102, RemainingSP: 72, TotalTP: 11, RemainingTP: 7, SyncedLevel: 50},
		Trees: map[int]dnfrepo.SkillTreeState{
			1: {Points: dnfrepo.SkillPointState{TotalSP: 100, RemainingSP: 60, TotalTP: 10, RemainingTP: 5, SyncedLevel: 49}},
		},
	}); err != nil {
		t.Fatal(err)
	}
	const pvfPath = "stackable/book_skill1.stk"
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{CharacterID: "77", Slots: map[string]dnfrepo.ItemStack{
		slotKey(listTypeMain, 10): {ItemID: 1031, Count: 2, Extra: map[string]string{
			"item_kind": "stackable", "pvf_path": pvfPath, "stackable_type": "[etc]", "amount_or_count": "2",
		}},
	}}); err != nil {
		t.Fatal(err)
	}
	owner, err := NewOwner(repositories)
	if err != nil {
		t.Fatal(err)
	}
	cmd := NewUseStackableCommand(alignedcmd.Request{AccountID: "acc", SelectedCharacterID: 77}, UseStackableRequest{
		SlotIndex: 10, ListType: listTypeMain, InstanceValue: 0x11223344, ItemCode: 1031,
	})
	baseline := &dnfrepo.SkillPointState{TotalSP: 100, RemainingSP: 100, TotalTP: 10, RemainingTP: 10, SyncedLevel: 50}
	result, err := owner.UseStackableWithAllResolversAndSkillCatalog(
		ctx, cmd, nil, nil,
		skillPointBookTestResolver(1031, pvfPath, alignedcmd.TPBookResolution{Kind: alignedcmd.SkillPointBookAddSP, Delta: 5, MinimumLevel: 1}),
		nil, nil, nil, nil, map[uint16]int{}, baseline,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !result.TPBookApplied || result.SkillPointBookKind != alignedcmd.SkillPointBookAddSP || result.SkillPointBookSPDelta != 5 ||
		result.SkillPointBookPreviousSP != 72 || result.SkillPointBookNewSP != 77 || result.RemainingCount != 1 {
		t.Fatalf("result=%+v", result)
	}
	character, _, _ := repositories.Character.Load(ctx, "77")
	skill, _, _ := repositories.Skill.Load(ctx, "77")
	inventory, _, _ := repositories.Inventory.Load(ctx, "77")
	if character.Stats["bonus_sp"] != 7 || character.Stats["bonus_tp"] != 1 ||
		skill.Points != (dnfrepo.SkillPointState{TotalSP: 107, RemainingSP: 77, TotalTP: 11, RemainingTP: 7, SyncedLevel: 50}) ||
		skill.Trees[1].Points != (dnfrepo.SkillPointState{TotalSP: 107, RemainingSP: 67, TotalTP: 11, RemainingTP: 6, SyncedLevel: 50}) ||
		inventory.Slots[slotKey(listTypeMain, 10)].Count != 1 || inventory.Slots[slotKey(listTypeMain, 10)].Extra["amount_or_count"] != "1" {
		t.Fatalf("character=%+v skill=%+v inventory=%+v", character, skill, inventory)
	}
}

func TestOwnerTPResetBookClearsOnlyPurchasedTPSkillsAcrossTrees(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	_ = repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "77", AccountID: "acc", Job: "0", Level: 50,
		Stats: map[string]int64{"bonus_sp": 2, "bonus_tp": 1},
	})
	_ = repositories.Skill.Save(ctx, dnfrepo.SkillRecord{
		CharacterID: "77",
		Skills: map[int64]dnfrepo.SkillState{
			5: {Level: 2, Enabled: true}, 46: {Level: 3, Enabled: true}, 47: {Level: 2, Enabled: true},
		},
		Points: dnfrepo.SkillPointState{TotalSP: 102, RemainingSP: 82, TotalTP: 11, RemainingTP: 4, SyncedLevel: 50},
		Trees: map[int]dnfrepo.SkillTreeState{
			1: {
				Skills: map[int64]dnfrepo.SkillState{5: {Level: 2, Enabled: true}, 47: {Level: 1, Enabled: true}},
				Points: dnfrepo.SkillPointState{TotalSP: 102, RemainingSP: 82, TotalTP: 11, RemainingTP: 8, SyncedLevel: 50},
			},
		},
		Layouts: map[int]dnfrepo.SkillLayout{0: {0: 46, 1: 47, 2: 5}, 1: {0: 47, 1: 5}},
	})
	const pvfPath = "stackable/book_fskillreset.stk"
	_ = repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{CharacterID: "77", Slots: map[string]dnfrepo.ItemStack{
		slotKey(listTypeMain, 10): {ItemID: 1206, Count: 1, Extra: map[string]string{
			"item_kind": "stackable", "pvf_path": pvfPath, "stackable_type": "[etc]",
		}},
	}})
	owner, _ := NewOwner(repositories)
	cmd := NewUseStackableCommand(alignedcmd.Request{AccountID: "acc", SelectedCharacterID: 77}, UseStackableRequest{
		SlotIndex: 10, ListType: listTypeMain, InstanceValue: 1, ItemCode: 1206,
	})
	baseline := &dnfrepo.SkillPointState{TotalSP: 100, RemainingSP: 100, TotalTP: 10, RemainingTP: 10, SyncedLevel: 50}
	result, err := owner.UseStackableWithAllResolversAndSkillCatalog(
		ctx, cmd, nil, nil,
		skillPointBookTestResolver(1206, pvfPath, alignedcmd.TPBookResolution{Kind: alignedcmd.SkillPointBookResetTP, MinimumLevel: 50}),
		nil, nil, nil, skillPointBookTestCatalog(t), map[uint16]int{46: 1}, baseline,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !result.SkillPointBookTPReset || result.SkillPointBookPreviousTP != 4 || result.SkillPointBookNewTP != 11 || result.RemainingCount != 0 {
		t.Fatalf("result=%+v", result)
	}
	skill, _, _ := repositories.Skill.Load(ctx, "77")
	inventory, _, _ := repositories.Inventory.Load(ctx, "77")
	if skill.Skills[46].Level != 1 || skill.Skills[5].Level != 2 || skill.Skills[47].Level != 0 ||
		skill.Points.RemainingTP != 11 || skill.Points.RemainingSP != 82 ||
		skill.Trees[1].Skills[5].Level != 2 || skill.Trees[1].Skills[47].Level != 0 || skill.Trees[1].Points.RemainingTP != 11 ||
		skill.Layouts[0][0] != 46 || skill.Layouts[0][2] != 5 || skill.Layouts[0][1] != 0 ||
		skill.Layouts[1][1] != 5 || skill.Layouts[1][0] != 0 || len(inventory.Slots) != 0 {
		t.Fatalf("skill=%+v inventory=%+v", skill, inventory)
	}
}

func tpBookTestResolver(itemID int64, pvfPath string, delta int64) alignedcmd.StackableUseResolver {
	return func(got int64) (alignedcmd.StackableUseResolution, error) {
		if got != itemID {
			return alignedcmd.StackableUseResolution{}, nil
		}
		return alignedcmd.StackableUseResolution{
			ItemID: itemID, PVFPath: pvfPath, StackableType: "[etc]",
			TPBook: &alignedcmd.TPBookResolution{Delta: delta, MinimumLevel: 50},
		}, nil
	}
}

func skillPointBookTestResolver(itemID int64, pvfPath string, effect alignedcmd.TPBookResolution) alignedcmd.StackableUseResolver {
	return func(got int64) (alignedcmd.StackableUseResolution, error) {
		if got != itemID {
			return alignedcmd.StackableUseResolution{}, nil
		}
		return alignedcmd.StackableUseResolution{ItemID: itemID, PVFPath: pvfPath, StackableType: "[etc]", TPBook: &effect}, nil
	}
}

func skillPointBookTestCatalog(t *testing.T) *dnfskill.Table {
	t.Helper()
	index, err := dnfpvf.Build(context.Background(), skillPointBookPVFSource{
		"skill/skilllist.lst":             "0 `SwordmanSkill.lst`\n",
		"skill/SwordmanSkill.lst":         "5 `Swordman/ordinary.skl`\n46 `Swordman/tp_floor.skl`\n47 `Swordman/tp_purchased.skl`\n",
		"skill/Swordman/ordinary.skl":     "[name] `ordinary`\n[type] `[active]`\n[purchase cost] 20\n[/purchase cost]\n",
		"skill/Swordman/tp_floor.skl":     "[name] `tp floor`\n[type] `[active]`\n[feature skill type] 1\n[special purchase cost] 2\n[/special purchase cost]\n",
		"skill/Swordman/tp_purchased.skl": "[name] `tp purchased`\n[type] `[active]`\n[feature skill type] 1\n[special purchase cost] 1\n[/special purchase cost]\n",
	}, dnfpvf.BuildOptions{Lists: []string{dnfskill.DefaultList}})
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := dnfskill.Load(context.Background(), index, dnfskill.Options{})
	if err != nil {
		t.Fatal(err)
	}
	return catalog
}

type skillPointBookPVFSource map[string]string

func (source skillPointBookPVFSource) ReadText(relativePath string) (string, error) {
	text, ok := source[relativePath]
	if !ok {
		return "", dnfpvf.ErrDocNotFound
	}
	return text, nil
}
