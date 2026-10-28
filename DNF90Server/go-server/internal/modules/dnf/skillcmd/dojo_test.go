package skillcmd

import (
	"context"
	"reflect"
	"testing"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

func TestDojoResetBuyAndSlotsKeepOrdinarySkills(t *testing.T) {
	ctx := context.Background()
	repos := seededSkillOwnerRepositories(t, ctx)
	record, _, _ := repos.Skill.Load(ctx, "31")
	record.Layouts = map[int]dnfrepo.SkillLayout{0: {0: 46}, 1: {150: 46}}
	dnfrepo.SetSkillTree(&record, 1, dnfrepo.SkillTreeState{Skills: record.Skills, Points: record.Points})
	if err := repos.Skill.Save(ctx, record); err != nil {
		t.Fatal(err)
	}
	if _, err := PrepareDojoSkills(ctx, repos, "31"); err != nil {
		t.Fatal(err)
	}
	owner, err := NewOwner(repos, OwnerOptions{Catalog: skillRuleCatalog(t), InitialLevels: map[uint16]int{46: 1},
		PointBaseline: &dnfrepo.SkillPointState{TotalSP: 100, RemainingSP: 100, TotalTP: 10, RemainingTP: 10, SyncedLevel: 10}})
	if err != nil {
		t.Fatal(err)
	}
	req := alignedRequest("acc-skill", 31)
	req.DojoSkills = true
	reset, err := owner.ApplyReset(ctx, NewResetCommand(req, SkillInitRequest{SkillTree: 0, Mode: 3}))
	if err != nil || reset.Points.RemainingSP != 100 {
		t.Fatalf("dojo reset=%+v %v", reset, err)
	}
	buy, err := owner.ApplyBuy(ctx, NewBuyCommand(req, BuySkillRequest{RawSkillTree: 0, Count: 1,
		FinalMode: 3, Entries: []BuySkillEntry{{SkillID: 46, LevelDelta: 1}}}))
	if err != nil || buy.SkillTree != 0 || buy.FinalMode != 3 {
		t.Fatalf("dojo learn=%+v %v", buy, err)
	}
	_, err = owner.ApplySlot(ctx, NewSlotCommand(req, ChangeSkillSlotRequest{SkillTree: 0, From: 0, To: 1}))
	if err != nil {
		t.Fatal(err)
	}
	_, err = owner.ApplyBuy(ctx, NewBuyCommand(req, BuySkillRequest{RawSkillTree: 0, Count: 1,
		FinalMode: 3, Entries: []BuySkillEntry{{SkillID: 46, LevelDelta: 1, RefundFlag: 1}}}))
	if err != nil {
		t.Fatalf("native dojo refund incorrectly requires paid contract: %v", err)
	}
	current, _, _ := repos.Skill.Load(ctx, "31")
	for tree := 0; tree < 2; tree++ {
		before, _ := dnfrepo.SkillTree(record, tree)
		after, _ := dnfrepo.SkillTree(current, tree)
		if !reflect.DeepEqual(before, after) || !reflect.DeepEqual(record.Layouts[tree], current.Layouts[tree]) {
			t.Fatalf("dojo overwrote ordinary tree %d", tree)
		}
	}
	projected, err := PrepareDojoSkills(ctx, repos, "31")
	if err != nil || projected.Layouts[0][1] != 46 || projected.Skills[46].Level != 1 {
		t.Fatalf("dojo relog=%+v %v", projected, err)
	}
}
