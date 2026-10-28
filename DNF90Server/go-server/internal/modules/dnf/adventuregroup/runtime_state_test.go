package adventuregroup

import (
	"errors"
	"testing"
	"time"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

func TestRuntimeStateMonthlyResetAccrualPurchaseAndCapsule(t *testing.T) {
	config := RuntimeConfig{
		ShopCategories: []ShopCategory{
			{
				Index:              ShopPointBrave,
				MaxPoint:           250,
				ExperiencePerPoint: 15_000_000,
				PointPerExperience: 1,
				Products: []ShopProduct{{
					ItemID: 10, Cost: 1, MonthlyPurchaseLimit: 2,
				}},
			},
			{Index: ShopPointGlory, MaxPoint: 9999, ResetPointMonthly: true},
			{Index: ShopPointPure},
		},
		Capsule: CapsuleConfig{
			MinimumExperience: 10,
			MaximumExperience: 100,
			MaximumCount:      10,
			MinimumLevel:      50,
			MaximumLevel:      86,
			GrantedExperience: 1,
		},
	}
	account := dnfrepo.AccountRecord{
		AccountID: "dnf:1",
		Metadata: map[string]string{
			RuntimeStateMetadataKey: `{"version":1,"shop_points":[7,80,0],"month":"2026-06","purchases":{"0:10":2}}`,
		},
	}
	now := time.Date(2026, time.July, 28, 12, 0, 0, 0, time.FixedZone("UTC+8", 8*60*60))
	state, err := ParseRuntimeState(account, config, now)
	if err != nil {
		t.Fatal(err)
	}
	if state.ShopPoints != [ShopPointTypeCount]uint32{7, 0, 0} || len(state.Purchases) != 0 {
		t.Fatalf("monthly normalized state=%+v", state)
	}
	// 满级角色的整笔经验：全额进结晶，同样全额（= 越过上限的部分）进胶囊槽。
	state.AddExperience(config, 30_000_005, 30_000_005)
	// 胶囊总容量 = [capsule max exp] 100 × [capsule max count] 10 = 1000
	// （PVF 真值：116,914,950 × 10；界面写"累积数上限 : 10个"）。
	// 早先这里按"单格 100 就是总上限"写，会把累计值卡死在正好 100%，
	// 客户端进度分子（累计值 mod 单格）恒 0 —— 现场就是"胶囊不累计"。
	if state.ShopPoints[ShopPointBrave] != 9 || state.BraveExperience != 5 || state.GrowthExperience != 1000 {
		t.Fatalf("maximum-level accrual=%+v", state)
	}
	category, _ := config.Shop(ShopPointBrave)
	if err := state.Spend(category, category.Products[0]); err != nil {
		t.Fatal(err)
	}
	if state.ShopPoints[ShopPointBrave] != 8 || state.PurchaseCount(ShopPointBrave, 10) != 1 {
		t.Fatalf("purchase state=%+v", state)
	}
	if !state.ConsumeCapsule(config) || state.GrowthExperience != 990 {
		t.Fatalf("capsule state=%+v", state)
	}
}

// 未满级经验只进骁勇结晶：胶囊槽与名誉经验都是满级专属（2026-09-24 口径）。
func TestRuntimeStateAddExperienceKeepsSubCapExperienceOutOfCapsule(t *testing.T) {
	config := creditTestConfig()
	state := RuntimeState{Version: 1}
	state.AddExperience(config, 30_000_005, 0)
	if state.ShopPoints[ShopPointBrave] != 2 || state.BraveExperience != 5 {
		t.Fatalf("sub-cap crystals=%+v", state)
	}
	if state.GrowthExperience != 0 {
		t.Fatalf("sub-cap experience entered the capsule slot: %+v", state)
	}
}

// 购买被拒的两种业务原因必须是可区分的错误：客户端 op1414 的结果码要按原因回
// （19 = 超过购买限制、22 = 购买材料不足），回一个笼统失败码客户端什么都不弹。
func TestRuntimeStateSpendDistinguishesLimitAndInsufficientPoints(t *testing.T) {
	product := ShopProduct{ItemID: 10, Cost: 5, MonthlyPurchaseLimit: 1}
	category := ShopCategory{Index: ShopPointBrave, MaxPoint: 250, Products: []ShopProduct{product}}
	state := RuntimeState{Version: 1}

	// 点数不足：0 点买 5 点的商品。
	if err := state.Spend(category, product); !errors.Is(err, ErrInsufficientShopPoints) {
		t.Fatalf("insufficient points error=%v", err)
	}

	// 给够点数后先买一次成功，再用完本月次数。
	state.ShopPoints[ShopPointBrave] = 10
	if err := state.Spend(category, product); err != nil {
		t.Fatalf("first purchase=%v", err)
	}
	if err := state.Spend(category, product); !errors.Is(err, ErrPurchaseLimitReached) {
		t.Fatalf("limit error=%v", err)
	}

	// 分类号越界仍然是"状态非法"，不是业务拒绝。
	bad := ShopCategory{Index: ShopPointTypeCount, Products: []ShopProduct{product}}
	if err := state.Spend(bad, product); !errors.Is(err, ErrRuntimeStateInvalid) {
		t.Fatalf("invalid category error=%v", err)
	}
}

// 成长胶囊槽按周重置：界面写明"成长胶囊于每周四凌晨6点重置"。
// 两条关键契约：① 第一次读到旧存档（没有 capsule_week 字段）**不许清零**；
// ② 跨越周四 06:00 之后才清零。
func TestRuntimeStateGrowthCapsuleResetsWeeklyOnThursdaySix(t *testing.T) {
	// 上限取真实 PVF 值（116,914,950 = 10 × [capsule need minimum exp]），
	// 免得归一化里的封顶把"是否被周重置"这件事掩盖掉。
	config := RuntimeConfig{Capsule: CapsuleConfig{MaximumExperience: 116_914_950}}
	shanghai := time.FixedZone("UTC+8", 8*60*60)

	// 2026-09-23 是周三。周四 06:00 之前仍属上一周（2026-09-17）。
	wednesday := time.Date(2026, time.September, 23, 12, 0, 0, 0, shanghai)
	state, err := ParseRuntimeState(
		dnfrepo.AccountRecord{AccountID: "dnf:1", Metadata: map[string]string{
			RuntimeStateMetadataKey: `{"version":1,"growth_experience":116914950,"month":"2026-09"}`,
		}},
		config,
		wednesday,
	)
	if err != nil {
		t.Fatal(err)
	}
	// 旧存档首次归一化：认领当前值，绝不清零。
	if state.GrowthExperience != 116_914_950 || state.CapsuleWeek != "2026-09-17" {
		t.Fatalf("first observation reset the capsule: %+v", state)
	}

	// 同一周内再读一次：不动。
	sameWeek, err := ParseRuntimeState(
		dnfrepo.AccountRecord{AccountID: "dnf:1", Metadata: map[string]string{
			RuntimeStateMetadataKey: `{"version":1,"growth_experience":500,"capsule_week":"2026-09-17"}`,
		}},
		config,
		time.Date(2026, time.September, 23, 23, 59, 0, 0, shanghai),
	)
	if err != nil {
		t.Fatal(err)
	}
	if sameWeek.GrowthExperience != 500 {
		t.Fatalf("same-week state reset: %+v", sameWeek)
	}

	// 周四 05:59 仍是上一周；06:00 起进入新的一周 → 清零。
	beforeBoundary, err := ParseRuntimeState(
		dnfrepo.AccountRecord{AccountID: "dnf:1", Metadata: map[string]string{
			RuntimeStateMetadataKey: `{"version":1,"growth_experience":500,"capsule_week":"2026-09-17"}`,
		}},
		config,
		time.Date(2026, time.September, 24, 5, 59, 0, 0, shanghai),
	)
	if err != nil {
		t.Fatal(err)
	}
	if beforeBoundary.GrowthExperience != 500 {
		t.Fatalf("reset happened before Thursday 06:00: %+v", beforeBoundary)
	}
	afterBoundary, err := ParseRuntimeState(
		dnfrepo.AccountRecord{AccountID: "dnf:1", Metadata: map[string]string{
			RuntimeStateMetadataKey: `{"version":1,"growth_experience":500,"capsule_week":"2026-09-17"}`,
		}},
		config,
		time.Date(2026, time.September, 24, 6, 0, 0, 0, shanghai),
	)
	if err != nil {
		t.Fatal(err)
	}
	if afterBoundary.GrowthExperience != 0 || afterBoundary.CapsuleWeek != "2026-09-24" {
		t.Fatalf("weekly capsule reset missing: %+v", afterBoundary)
	}
	// 骁勇结晶一辈子不重置，跨周也不许动。
	if afterBoundary.BraveExperience != 0 || afterBoundary.ShopPoints[ShopPointBrave] != 0 {
		t.Fatalf("weekly reset touched crystals: %+v", afterBoundary)
	}
}

func TestRuntimeConfigExpeditionRewardUsesRotatedPVFAttributes(t *testing.T) {
	config := RuntimeConfig{
		ExpeditionAreas: []ExpeditionArea{{
			Index: 1,
			RewardRates: map[uint32]float64{
				6 * 60 * 60: 2,
			},
			DesignatedGroups: []string{"[group A]"},
		}},
		AttributeIDs: map[string]byte{
			"[a]": 1,
		},
		AttributeGroups: map[string][]string{
			"[group a]": {"[a]"},
		},
		CharacterAttributes: map[[2]byte][]string{
			{1, 2}: {"[a]"},
		},
		AttributeRewardRates:   []float64{1, 1.5},
		PartyMemberRewardRates: []float64{1, 1, 2, 3, 4},
		ExpeditionExtraRate:    0.5,
		RotationDays:           1,
	}
	// Native formula: rate 2 x party rate 1 x attribute bonus 1.5 = 3, rounded,
	// then x extra rate 0.5 and truncated -> 1.
	reward, ok := config.ExpeditionReward(
		1,
		6*60*60,
		[]ExpeditionMemberInput{{Level: 90, Job: 1, GrowType: 2}},
		config.AreaAttributes(1, time.Unix(1_700_000_000, 0)),
	)
	if !ok || reward != 1 {
		t.Fatalf("reward=%d ok=%v", reward, ok)
	}
}
