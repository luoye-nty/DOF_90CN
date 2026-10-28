package inventory

import (
	"context"
	"testing"
	"time"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	"longheng.io/server/internal/modules/dnf/statbuff"
)

// 一次使用 = 背包 2→1 + 角色身上出现强化成功率窗口（与爆率窗口同一笔事务）。
// 这里用真实 owner 路径（含事务、存档、结果字段），钉住"套路一样"这句话。
func TestUpgradeRatePotionConsumesOneAndStartsTheReinforceWindow(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, time.October, 3, 20, 0, 0, 0, time.UTC)
	repos := newStatChangePotionFixture(t, ctx, 2, map[string]int64{"fatigue": 100})
	owner, err := NewOwner(repos)
	if err != nil {
		t.Fatal(err)
	}
	owner.now = func() time.Time { return now }
	owner.statChangeRandom = func(int) int { return 95 } // 落在最高档 1099(+30)

	cmd := Command{
		Operation:           "use_stackable",
		AccountID:           "acc",
		SelectedCharacterID: 42,
		SourceListType:      listTypeMain,
		SourceSlotIndex:     8,
		SourceInstanceValue: 0x0000BEEF,
		ItemTemplateID:      testEpicBuffPotionItemID,
	}
	resolution := alignedcmd.StackableUseResolution{
		ItemID:        testEpicBuffPotionItemID,
		PVFPath:       "stackable/490002001/chn_490002458.stk",
		StackableType: "[expert town potion]",
		StatChangePotion: &alignedcmd.StatChangePotionResolution{
			EffectDurationSeconds: 600,
			UpgradeKind:           "reinforce",
			UpgradeRolls: []alignedcmd.StatChangePotionWeightedRoll{
				{BuffID: 1097, Weight: 60, BonusPercent: 10},
				{BuffID: 1098, Weight: 30, BonusPercent: 20},
				{BuffID: 1099, Weight: 10, BonusPercent: 30},
			},
		},
	}
	result, err := owner.useStatChangePotion(ctx, cmd, resolution)
	if err != nil {
		t.Fatal(err)
	}
	if result.RemainingCount != 1 || result.UpgradeRateKind != "reinforce" ||
		result.UpgradeRatePercent != 30 || result.UpgradeRateBuffID != 1099 ||
		result.UpgradeRateExpireAt != now.Add(10*time.Minute).Unix() {
		t.Fatalf("result=%+v", result)
	}
	// 这件道具不涉及掉落率：那条路必须保持沉默。
	if result.EpicDropRatePercent != 0 || result.EpicDropRateBuffID != 0 || result.EpicDropRateExpireAt != 0 {
		t.Fatalf("drop-rate fields must stay empty: %+v", result)
	}
	character, found, err := repos.Character.Load(ctx, "42")
	if err != nil || !found {
		t.Fatalf("character load found=%t err=%v", found, err)
	}
	window, active := statbuff.ActiveRate(character, statbuff.KindUpgradeSuccess, now.Add(time.Minute))
	if !active || window.Percent != 30 || window.BuffID != 1099 || window.ItemID != testEpicBuffPotionItemID {
		t.Fatalf("active=%t window=%+v stats=%v", active, window, character.Stats)
	}
	// 增幅那一族不该被顺手写进去。
	if _, active := statbuff.ActiveRate(character, statbuff.KindAmplifySuccess, now.Add(time.Minute)); active {
		t.Fatal("a reinforce potion must not open an amplify window")
	}
	if character.Stats["fatigue"] != 100 {
		t.Fatalf("unrelated stat changed: %v", character.Stats)
	}
	// 10 分钟之后自己失效。
	if _, active := statbuff.ActiveRate(character, statbuff.KindUpgradeSuccess, now.Add(11*time.Minute)); active {
		t.Fatal("the window must expire after 10 minutes")
	}
}

// PVF 没有数字的那几件（强化秘药 / 强化的奥秘 / 永恒的增幅的秘密）：固定常量 +10。
func TestUpgradeRatePotionFallsBackToTheConstantBonus(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, time.October, 3, 20, 30, 0, 0, time.UTC)
	repos := newStatChangePotionFixture(t, ctx, 1, map[string]int64{})
	owner, err := NewOwner(repos)
	if err != nil {
		t.Fatal(err)
	}
	owner.now = func() time.Time { return now }
	cmd := Command{
		Operation:           "use_stackable",
		AccountID:           "acc",
		SelectedCharacterID: 42,
		SourceListType:      listTypeMain,
		SourceSlotIndex:     8,
		SourceInstanceValue: 0x0000BEEF,
		ItemTemplateID:      testEpicBuffPotionItemID,
	}
	resolution := alignedcmd.StackableUseResolution{
		ItemID:        testEpicBuffPotionItemID,
		PVFPath:       "stackable/490002001/chn_490002458.stk",
		StackableType: "[item upgrad buff amplify upgrade]",
		StatChangePotion: &alignedcmd.StatChangePotionResolution{
			EffectDurationSeconds: 1800,
			UpgradeKind:           "amplify",
			UpgradeBonusPercent:   10,
		},
	}
	result, err := owner.useStatChangePotion(ctx, cmd, resolution)
	if err != nil {
		t.Fatal(err)
	}
	if result.UpgradeRateKind != "amplify" || result.UpgradeRatePercent != 10 ||
		result.UpgradeRateBuffID != 0 || result.UpgradeRateExpireAt != now.Add(30*time.Minute).Unix() {
		t.Fatalf("result=%+v", result)
	}
	character, _, err := repos.Character.Load(ctx, "42")
	if err != nil {
		t.Fatal(err)
	}
	window, active := statbuff.ActiveRate(character, statbuff.KindAmplifySuccess, now.Add(time.Minute))
	if !active || window.Percent != 10 || window.BuffID != 0 {
		t.Fatalf("active=%t window=%+v", active, window)
	}
}

// 强化/增幅成功率药剂在**掷点之前**放大成功权重（服主 2026-10-03 的原话：
// "在服务端决定是否成功前增加成功率"）。
//
// 算法是乘法（weight ×(100+N)/100），与本仓库既有的"高级炉岩炭 ×110%"
// （applyUpgradeHighUpgradeTerms）一致；上限封在必成（currentUpgradeSuccessBase）。
func TestApplyCurrentUpgradeSuccessWindow(t *testing.T) {
	now := time.Date(2026, 10, 3, 14, 0, 0, 0, time.UTC)
	active := func(kind statbuff.Kind, percent int64) dnfrepo.CharacterRecord {
		character := dnfrepo.CharacterRecord{CharacterID: "11"}
		if _, err := statbuff.ActivateRate(&character, kind, 10008220, 1099, percent, time.Hour, now); err != nil {
			t.Fatalf("activate: %v", err)
		}
		return character
	}
	expired := func(kind statbuff.Kind, percent int64) dnfrepo.CharacterRecord {
		character := dnfrepo.CharacterRecord{CharacterID: "11", Stats: map[string]int64{}}
		if _, err := statbuff.ActivateRate(&character, kind, 10008220, 1099, percent, time.Hour, now.Add(-2*time.Hour)); err != nil {
			t.Fatalf("activate: %v", err)
		}
		return character
	}

	for _, tc := range []struct {
		name      string
		weight    int
		character dnfrepo.CharacterRecord
		mode      string
		want      int
	}{
		{name: "no window keeps the PVF weight", weight: 13000, mode: "reinforce",
			character: dnfrepo.CharacterRecord{CharacterID: "11"}, want: 13000},
		{name: "reinforce +10% scales the weight", weight: 13000, mode: "reinforce",
			character: active(statbuff.KindUpgradeSuccess, 10), want: 14300},
		{name: "reinforce +30% (the top Seria tier)", weight: 40000, mode: "reinforce",
			character: active(statbuff.KindUpgradeSuccess, 30), want: 52000},
		{name: "amplify uses the amplify window", weight: 13000, mode: "amplify",
			character: active(statbuff.KindAmplifySuccess, 10), want: 14300},
		{name: "a reinforce window must not help an amplify attempt", weight: 13000, mode: "amplify",
			character: active(statbuff.KindUpgradeSuccess, 30), want: 13000},
		{name: "an amplify window must not help a reinforce attempt", weight: 13000, mode: "reinforce",
			character: active(statbuff.KindAmplifySuccess, 30), want: 13000},
		{name: "expired window changes nothing", weight: 13000, mode: "reinforce",
			character: expired(statbuff.KindUpgradeSuccess, 30), want: 13000},
		{name: "clamped at guaranteed success", weight: 99000, mode: "reinforce",
			character: active(statbuff.KindUpgradeSuccess, 30), want: currentUpgradeSuccessBase},
		{name: "an unknown mode is ignored", weight: 13000, mode: "normal",
			character: active(statbuff.KindUpgradeSuccess, 30), want: 13000},
		{name: "a non-positive weight is left alone", weight: 0, mode: "reinforce",
			character: active(statbuff.KindUpgradeSuccess, 30), want: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := applyCurrentUpgradeSuccessWindow(tc.weight, tc.character, tc.mode, now.Add(time.Minute)); got != tc.want {
				t.Fatalf("weight=%d want=%d", got, tc.want)
			}
		})
	}
}

// 加成来源：buff 表掷点（赛丽亚三档 10/20/30），表里没有条目时用固定常量。
func TestCurrentUpgradeRatePotionRoll(t *testing.T) {
	rolls := []alignedcmd.StatChangePotionWeightedRoll{
		{BuffID: 1097, Weight: 60, BonusPercent: 10},
		{BuffID: 1098, Weight: 30, BonusPercent: 20},
		{BuffID: 1099, Weight: 10, BonusPercent: 30},
	}
	potion := &alignedcmd.StatChangePotionResolution{UpgradeKind: "reinforce", UpgradeRolls: rolls}
	for _, tc := range []struct {
		pick       int
		wantBonus  int64
		wantBuffID int64
	}{
		{pick: 0, wantBonus: 10, wantBuffID: 1097},
		{pick: 59, wantBonus: 10, wantBuffID: 1097},
		{pick: 60, wantBonus: 20, wantBuffID: 1098},
		{pick: 89, wantBonus: 20, wantBuffID: 1098},
		{pick: 90, wantBonus: 30, wantBuffID: 1099},
		{pick: 99, wantBonus: 30, wantBuffID: 1099},
	} {
		bonus, buffID := currentUpgradeRatePotionRoll(potion, func(int) int { return tc.pick })
		if bonus != tc.wantBonus || buffID != tc.wantBuffID {
			t.Fatalf("pick=%d got (%d,%d) want (%d,%d)", tc.pick, bonus, buffID, tc.wantBonus, tc.wantBuffID)
		}
	}
	// random 为 nil（或权重非法）时按第一条有效项取：宁可确定性，也不要随机失败。
	if bonus, buffID := currentUpgradeRatePotionRoll(potion, nil); bonus != 10 || buffID != 1097 {
		t.Fatalf("nil random got (%d,%d) want (10,1097)", bonus, buffID)
	}
	// PVF 没有候选表的那几件（强化秘药 / 强化的奥秘 / 永恒的增幅的秘密）：固定常量。
	fallback := &alignedcmd.StatChangePotionResolution{UpgradeKind: "amplify", UpgradeBonusPercent: 10}
	if bonus, buffID := currentUpgradeRatePotionRoll(fallback, nil); bonus != 10 || buffID != 0 {
		t.Fatalf("fallback got (%d,%d) want (10,0)", bonus, buffID)
	}
	// 什么都没声明 ⇒ 不加成（绝不猜）。
	if bonus, _ := currentUpgradeRatePotionRoll(&alignedcmd.StatChangePotionResolution{}, nil); bonus != 0 {
		t.Fatalf("empty resolution got bonus=%d want 0", bonus)
	}
}
