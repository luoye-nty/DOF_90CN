package quest

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"longheng.io/server/internal/modules/dnf/adventuregroup"
	"longheng.io/server/internal/modules/dnf/progression"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

// 满级角色的任务经验以前被整笔丢掉：角色经验被钉在满级入口阈值上，
// 熟练等级没写、骁勇结晶也没写。现在越过上限的部分必须与角色经验同事务
// 记进账号级冒险团状态（骁勇结晶 / 成长胶囊槽 / 荣誉经验），重放不重复记。
func TestOwnerApplyFinishSettlementCreditsAdventureGroupAtLevelCap(t *testing.T) {
	ctx := context.Background()
	repos := finishOwnerSeed(t, ctx)
	if err := repos.Account.Save(ctx, dnfrepo.AccountRecord{AccountID: "acc"}); err != nil {
		t.Fatal(err)
	}
	tables := finishOwnerLevelCapProgression(t)
	entryExperience, err := tables.ThresholdToNext(progression.MaximumCharacterLevel - 1)
	if err != nil {
		t.Fatal(err)
	}
	character, found, err := repos.Character.Load(ctx, "19")
	if err != nil || !found {
		t.Fatalf("character found=%t err=%v", found, err)
	}
	character.Level = progression.MaximumCharacterLevel
	character.Stats = map[string]int64{"exp": int64(entryExperience), "grow_type": 0}
	if err := repos.Character.Save(ctx, character); err != nil {
		t.Fatal(err)
	}
	skill, found, err := repos.Skill.Load(ctx, "19")
	if err != nil || !found {
		t.Fatalf("skill found=%t err=%v", found, err)
	}
	skill.Points = dnfrepo.SkillPointState{SyncedLevel: progression.MaximumCharacterLevel}
	if err := repos.Skill.Save(ctx, skill); err != nil {
		t.Fatal(err)
	}

	// 30 点任务经验 = 1 个结晶（换算比例按测试配置，便于直接断言）。
	config := adventuregroup.RuntimeConfig{
		ShopCategories: []adventuregroup.ShopCategory{
			{
				Index:              adventuregroup.ShopPointBrave,
				MaxPoint:           250,
				ExperiencePerPoint: 30,
				PointPerExperience: 1,
			},
			{Index: adventuregroup.ShopPointGlory, MaxPoint: 9999, ResetPointMonthly: true},
			{Index: adventuregroup.ShopPointPure},
		},
		Capsule: adventuregroup.CapsuleConfig{
			MinimumExperience: 10,
			MaximumExperience: 500,
			MaximumCount:      10,
			MinimumLevel:      50,
			MaximumLevel:      86,
			GrantedExperience: 1_000_000,
		},
	}
	owner, err := NewOwner(repos)
	if err != nil {
		t.Fatal(err)
	}
	input := finishOwnerInput(tables, finishOwnerAllocator(nil))
	input.AdventureRuntime = &config

	first, err := owner.ApplyFinishSettlement(ctx, finishOwnerCatalog(), input)
	if err != nil {
		t.Fatal(err)
	}
	if first.ExperienceDelta != 30 {
		t.Fatalf("experience delta=%d want=30", first.ExperienceDelta)
	}
	account, found, err := repos.Account.Load(ctx, "acc")
	if err != nil || !found {
		t.Fatalf("account found=%t err=%v", found, err)
	}
	if account.HonorExp != 30 {
		t.Fatalf("honor_exp=%d want=30", account.HonorExp)
	}
	state, err := adventuregroup.ParseRuntimeState(account, config, input.CommittedAt)
	if err != nil {
		t.Fatal(err)
	}
	if state.ShopPoints[adventuregroup.ShopPointBrave] != 1 || state.GrowthExperience != 30 {
		t.Fatalf("adventure state=%+v", state)
	}

	// 重放（同一 completion key）复用收据，不能再记一次结晶。
	second, err := owner.ApplyFinishSettlement(ctx, finishOwnerCatalog(), input)
	if err != nil {
		t.Fatal(err)
	}
	if !second.Replayed {
		t.Fatalf("second result=%+v", second)
	}
	replayed, found, err := repos.Account.Load(ctx, "acc")
	if err != nil || !found {
		t.Fatalf("replayed account found=%t err=%v", found, err)
	}
	if replayed.HonorExp != 30 {
		t.Fatalf("replay double-credited honor_exp=%d", replayed.HonorExp)
	}
}

// 未满级角色做任务：任务经验照常进角色经验，**同时全额镜像进骁勇结晶**
// （2026-09-24 服主口径：没满级也要能攒结晶），但成长胶囊槽与名誉经验
// 都是满级专属，一格都不动。
//
// 这条用例原先钉的是"未满级一分不动"的旧口径，随规则变更一起改写 ——
// 断言从"没有账号状态"改成"只有结晶、没有胶囊/名誉"。
func TestOwnerApplyFinishSettlementCreditsCrystalsOnlyBelowCap(t *testing.T) {
	ctx := context.Background()
	repos := finishOwnerSeed(t, ctx)
	if err := repos.Account.Save(ctx, dnfrepo.AccountRecord{AccountID: "acc"}); err != nil {
		t.Fatal(err)
	}
	config := adventuregroup.RuntimeConfig{
		ShopCategories: []adventuregroup.ShopCategory{{
			Index:              adventuregroup.ShopPointBrave,
			MaxPoint:           250,
			ExperiencePerPoint: 30,
			PointPerExperience: 1,
		}},
		Capsule: adventuregroup.CapsuleConfig{
			MinimumExperience: 10,
			MaximumExperience: 100,
			MaximumCount:      10,
			MinimumLevel:      50,
			MaximumLevel:      86,
			GrantedExperience: 1_000_000,
		},
	}
	owner, err := NewOwner(repos)
	if err != nil {
		t.Fatal(err)
	}
	input := finishOwnerInput(finishOwnerProgression(t), finishOwnerAllocator(nil))
	input.AdventureRuntime = &config
	if _, err := owner.ApplyFinishSettlement(ctx, finishOwnerCatalog(), input); err != nil {
		t.Fatal(err)
	}
	account, found, err := repos.Account.Load(ctx, "acc")
	if err != nil || !found {
		t.Fatalf("account found=%t err=%v", found, err)
	}
	if account.HonorExp != 0 {
		t.Fatalf("below-cap quest finish wrote honor_exp=%d, want 0 (名誉等级是满级专属)", account.HonorExp)
	}
	state, err := adventuregroup.ParseRuntimeState(account, config, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if state.ShopPoints[adventuregroup.ShopPointBrave] != 3 || state.BraveExperience != 10 {
		t.Fatalf("below-cap quest crystals=%+v, want 3 points + 10 remainder", state)
	}
	if state.GrowthExperience != 0 {
		t.Fatalf("below-cap quest experience entered the capsule slot: %+v", state)
	}
}

// 满级专用表：ExpTable 每级 +1000（90 级入口 = 90000），SP 表覆盖到 90 级。
func finishOwnerLevelCapProgression(t *testing.T) *progression.Tables {
	t.Helper()
	thresholds := make([]string, progression.MaximumCharacterLevel)
	for index := range thresholds {
		thresholds[index] = fmt.Sprintf("%d", (index+1)*1000)
	}
	var sp strings.Builder
	sp.WriteString("[sp table]\n")
	for level := 1; level <= progression.MaximumCharacterLevel; level++ {
		fmt.Fprintf(&sp, "%d %d\n", level, level*3)
	}
	sp.WriteString("[/sp table]\n[tp table]\n50 1\n[/tp table]\n")
	tables, err := progression.Load(context.Background(), finishProgressionSource{
		progression.ExperienceTablePath: strings.Join(thresholds, " ") + "\n",
		progression.SkillPointTablePath: sp.String(),
		progression.QuestParameterPath:  "[difficulty]\n`N` 100\n[/difficulty]\n[exp reward table]\n100 -1\n200 -1\n300 -1\n[gold reward table]\n0 10\n[green level penalty]\n80\n[grey level penalty]\n30\n",
	})
	if err != nil {
		t.Fatal(err)
	}
	return tables
}
