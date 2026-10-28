package adventuregroup

import (
	"context"
	"errors"
	"testing"
	"time"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

func creditTestConfig() RuntimeConfig {
	return RuntimeConfig{
		ShopCategories: []ShopCategory{
			{
				Index:              ShopPointBrave,
				MaxPoint:           250,
				ExperiencePerPoint: 15_000_000,
				PointPerExperience: 1,
			},
			{Index: ShopPointGlory, MaxPoint: 9999, ResetPointMonthly: true},
			{Index: ShopPointPure},
		},
		Capsule: CapsuleConfig{
			MinimumExperience: 11_691_495,
			MaximumExperience: 116_914_950,
			MaximumCount:      10,
			MinimumLevel:      50,
			MaximumLevel:      86,
			GrantedExperience: 1_000_000,
		},
	}
}

func creditTestRepositories(t *testing.T, account dnfrepo.AccountRecord) dnfrepo.Group {
	t.Helper()
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Account.Save(context.Background(), account); err != nil {
		t.Fatal(err)
	}
	return repositories
}

// 一笔**满级**经验必须同时推进三个账号级累计量，余数还要逐笔累积 ——
// 这正是"怪物击杀也要给结晶"那条改动的核心口径。
func TestCreditExperienceAccruesCrystalsCapsuleAndHonorExpForMaxLevelAward(t *testing.T) {
	config := creditTestConfig()
	now := time.Date(2026, time.September, 23, 12, 0, 0, 0, time.UTC)
	repositories := creditTestRepositories(t, dnfrepo.AccountRecord{AccountID: "dnf:1"})
	// 满级角色的整笔经验都属于"越过上限"的部分。
	atCap := func(value uint64) ExperienceCredit {
		return ExperienceCredit{AwardedExperience: value, MaximumLevelExperience: value}
	}

	credit, err := CreditExperience(context.Background(), repositories.Account, config, "dnf:1", atCap(10_000_000), now)
	if err != nil {
		t.Fatal(err)
	}
	if credit.AwardedExperience != 10_000_000 || credit.BravePoints != 0 || credit.BraveExperience != 10_000_000 ||
		credit.CapsuleExperience != 10_000_000 || credit.HonorExperience != 10_000_000 {
		t.Fatalf("first credit=%+v", credit)
	}

	// 第二笔把余数推过 1500 万：结晶 +1，余数留 5,000,000。
	credit, err = CreditExperience(context.Background(), repositories.Account, config, "dnf:1", atCap(10_000_000), now)
	if err != nil {
		t.Fatal(err)
	}
	if credit.BravePoints != 1 || credit.BraveExperience != 5_000_000 || credit.HonorExperience != 20_000_000 {
		t.Fatalf("second credit=%+v", credit)
	}

	// 持久化结果必须与返回值一致（账号元数据 + honor_exp 同事务落库）。
	account, found, err := repositories.Account.Load(context.Background(), "dnf:1")
	if err != nil || !found {
		t.Fatalf("account found=%t err=%v", found, err)
	}
	state, err := ParseRuntimeState(account, config, now)
	if err != nil {
		t.Fatal(err)
	}
	if state.ShopPoints[ShopPointBrave] != 1 || state.BraveExperience != 5_000_000 ||
		state.GrowthExperience != 20_000_000 || account.HonorExp != 20_000_000 {
		t.Fatalf("persisted state=%+v honor=%d", state, account.HonorExp)
	}
}

// 2026-09-24 服主口径：**没满级的角色也要能攒骁勇结晶**。
// 未满级经验 1:1 进结晶（不影响角色自己的升级经验），但**不进**成长胶囊槽、
// **不进**名誉经验 —— 胶囊槽与名誉等级都是满级专属。
func TestCreditExperienceSubCapFeedsOnlyCrystals(t *testing.T) {
	config := creditTestConfig()
	now := time.Date(2026, time.September, 24, 1, 0, 0, 0, time.UTC)
	repositories := creditTestRepositories(t, dnfrepo.AccountRecord{AccountID: "dnf:1"})

	// 一次 40 级的打怪：全额经验，没有满级部分。
	credit, err := CreditExperience(
		context.Background(),
		repositories.Account,
		config,
		"dnf:1",
		ExperienceCredit{AwardedExperience: 20_000_000},
		now,
	)
	if err != nil {
		t.Fatal(err)
	}
	if credit.BravePoints != 1 || credit.BraveExperience != 5_000_000 {
		t.Fatalf("sub-cap crystals=%+v", credit)
	}
	if credit.CapsuleExperience != 0 {
		t.Fatalf("sub-cap experience leaked into the capsule slot: %+v", credit)
	}
	if credit.HonorExperience != 0 {
		t.Fatalf("sub-cap experience leaked into honor experience: %+v", credit)
	}

	// 再攒一笔把余数推过 1500 万：结晶 +1。
	credit, err = CreditExperience(
		context.Background(),
		repositories.Account,
		config,
		"dnf:1",
		ExperienceCredit{AwardedExperience: 10_000_000},
		now,
	)
	if err != nil {
		t.Fatal(err)
	}
	if credit.BravePoints != 2 || credit.BraveExperience != 0 {
		t.Fatalf("second sub-cap credit=%+v", credit)
	}
	account, _, err := repositories.Account.Load(context.Background(), "dnf:1")
	if err != nil {
		t.Fatal(err)
	}
	state, err := ParseRuntimeState(account, config, now)
	if err != nil {
		t.Fatal(err)
	}
	if state.GrowthExperience != 0 || account.HonorExp != 0 {
		t.Fatalf("sub-cap credits touched capsule/honor: state=%+v honor=%d", state, account.HonorExp)
	}
}

// 一笔经验里"满级部分"不可能多于总额：Normalize 必须兜住调用方填错的顺序。
func TestExperienceCreditNormalizeClampsMaximumToAwarded(t *testing.T) {
	credit := ExperienceCredit{AwardedExperience: 100, MaximumLevelExperience: 500}.Normalize()
	if credit.MaximumLevelExperience != 100 {
		t.Fatalf("normalized=%+v", credit)
	}
	if (ExperienceCredit{}).Empty() != true || (ExperienceCredit{AwardedExperience: 1}).Empty() != false {
		t.Fatalf("Empty() semantics changed")
	}
}

// 骁勇结晶封顶 [max shop point]：超过上限的经验只留在余数里，不再加结晶。
func TestCreditExperienceStopsAtShopPointCap(t *testing.T) {
	config := creditTestConfig()
	now := time.Date(2026, time.September, 23, 12, 0, 0, 0, time.UTC)
	repositories := creditTestRepositories(t, dnfrepo.AccountRecord{AccountID: "dnf:1"})

	if _, err := CreditExperience(
		context.Background(), repositories.Account, config, "dnf:1",
		ExperienceCredit{AwardedExperience: 250 * 15_000_000, MaximumLevelExperience: 250 * 15_000_000}, now,
	); err != nil {
		t.Fatal(err)
	}
	credit, err := CreditExperience(
		context.Background(), repositories.Account, config, "dnf:1",
		ExperienceCredit{AwardedExperience: 15_000_000, MaximumLevelExperience: 15_000_000}, now,
	)
	if err != nil {
		t.Fatal(err)
	}
	// 已到上限：结晶不再增加，超出的整笔经验按现有口径丢弃（余数仍是整除后的 0）。
	if credit.BravePoints != 250 || credit.BraveExperience != 0 {
		t.Fatalf("capped credit=%+v", credit)
	}
}

// 成长胶囊槽按**总容量**封顶：一格 = [capsule max exp]（100%），可以攒
// [capsule max count] 格（界面写"累积数上限 : 10个"）。所以一笔远超一格的
// 经验必须继续累加、绝不在一格处停住 —— 那正是"胶囊不累计"的根因。
func TestCreditExperienceKeepsCapsuleGrowingPastOneGauge(t *testing.T) {
	config := creditTestConfig()
	now := time.Date(2026, time.September, 23, 12, 0, 0, 0, time.UTC)
	repositories := creditTestRepositories(t, dnfrepo.AccountRecord{AccountID: "dnf:1"})

	credit, err := CreditExperience(
		context.Background(), repositories.Account, config, "dnf:1",
		ExperienceCredit{AwardedExperience: 200_000_000, MaximumLevelExperience: 200_000_000}, now,
	)
	if err != nil {
		t.Fatal(err)
	}
	if credit.BravePoints != 13 || credit.BraveExperience != 5_000_000 ||
		credit.CapsuleExperience != 200_000_000 {
		t.Fatalf("credit=%+v", credit)
	}

	// 再攒到超过总容量：胶囊停在 总容量 = 单格 × 格数，结晶继续按同一笔走。
	credit, err = CreditExperience(
		context.Background(), repositories.Account, config, "dnf:1",
		ExperienceCredit{AwardedExperience: 2_000_000_000, MaximumLevelExperience: 2_000_000_000}, now,
	)
	if err != nil {
		t.Fatal(err)
	}
	if want := CapsuleTotalCapacity(config.Capsule); credit.CapsuleExperience != want {
		t.Fatalf("capsule=%d want total capacity %d", credit.CapsuleExperience, want)
	}
	if want := config.Capsule.MaximumExperience * uint64(config.Capsule.MaximumCount); want == config.Capsule.MaximumExperience {
		t.Fatalf("test config cannot tell one gauge from the total: %d", want)
	}
}

// 成长胶囊"可获得的经验值"是**可领取次数**，不是经验值总量：
// 客户端拿到这个值会建同样多条列表项（0x13F0FA0 的 `for (i=0; i<count; i++)`），
// 所以上限必须是"格数 × 每格档数"（10 × 10 = 100），而不是 1,000 万级的总量。
func TestCapsuleClaimCountIsBoundedByTotalCapacityInChunks(t *testing.T) {
	config := creditTestConfig().Capsule
	step := config.MinimumExperience
	perGauge := config.MaximumExperience / step
	if perGauge != 10 {
		t.Fatalf("PVF ratio max/step = %d, want 10 (10%% per claim)", perGauge)
	}
	cases := []struct {
		name        string
		accumulated uint64
		want        uint32
	}{
		{name: "没到一档", accumulated: step - 1, want: 0},
		{name: "正好一档", accumulated: step, want: 1},
		{name: "一格", accumulated: config.MaximumExperience, want: uint32(perGauge)},
		{name: "一格多一档", accumulated: config.MaximumExperience + step, want: uint32(perGauge) + 1},
		{name: "满总容量", accumulated: CapsuleTotalCapacity(config), want: uint32(perGauge) * config.MaximumCount},
		{name: "超过总容量也不放大", accumulated: ^uint64(0), want: uint32(perGauge) * config.MaximumCount},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := CapsuleClaimCount(config, testCase.accumulated); got != testCase.want {
				t.Fatalf("CapsuleClaimCount(%d)=%d want=%d", testCase.accumulated, got, testCase.want)
			}
		})
	}
	if got := CapsuleClaimCount(CapsuleConfig{}, 1_000_000); got != 0 {
		t.Fatalf("missing PVF claim count = %d, want 0", got)
	}
}

// 没有可记的经验时不许写账号（否则每次零经验击杀都会刷一次账号行）。
func TestCreditExperienceIgnoresZeroValue(t *testing.T) {
	config := creditTestConfig()
	repositories := creditTestRepositories(t, dnfrepo.AccountRecord{AccountID: "dnf:1"})
	credit, err := CreditExperience(context.Background(), repositories.Account, config, "dnf:1", ExperienceCredit{}, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if credit.AwardedExperience != 0 || credit.HonorExperience != 0 {
		t.Fatalf("zero credit=%+v", credit)
	}
	account, _, err := repositories.Account.Load(context.Background(), "dnf:1")
	if err != nil {
		t.Fatal(err)
	}
	if account.Metadata[RuntimeStateMetadataKey] != "" || account.HonorExp != 0 {
		t.Fatalf("zero credit wrote account=%+v", account)
	}
}

// 账号不存在必须整笔失败：静默跳过会让"经验涨了、结晶没涨"留下来。
func TestCreditExperienceRequiresTheAccount(t *testing.T) {
	repositories := dnfrepomemory.NewMemoryGroup()
	_, err := CreditExperience(
		context.Background(), repositories.Account, creditTestConfig(), "dnf:missing",
		ExperienceCredit{AwardedExperience: 10}, time.Now().UTC(),
	)
	if !errors.Is(err, ErrCreditAccountNotFound) {
		t.Fatalf("err=%v", err)
	}
}

// ApplyExperienceCredit 是给"账号已经在调用方手里"的路径用的
// （地下城通关结算）：它只改内存记录，落库由调用方自己的事务负责。
func TestApplyExperienceCreditMutatesOnlyTheGivenAccountRecord(t *testing.T) {
	config := creditTestConfig()
	now := time.Date(2026, time.September, 23, 12, 0, 0, 0, time.UTC)
	account := dnfrepo.AccountRecord{AccountID: "dnf:1", Metadata: map[string]string{"keep": "1"}}
	credit, err := ApplyExperienceCredit(&account, config, ExperienceCredit{
		AwardedExperience:      15_000_000,
		MaximumLevelExperience: 15_000_000,
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	if credit.BravePoints != 1 || credit.HonorExperience != 15_000_000 || account.Metadata["keep"] != "1" {
		t.Fatalf("credit=%+v account=%+v", credit, account)
	}
}

// 2026-09-29 服主口径：**满级角色获得的经验不再累加进冒险团（等级）经验**，
// 满级之前照旧累加。同一笔账里的骁勇结晶 / 成长胶囊槽 / 名誉经验不受影响——
// 那三条各自有自己的口径（结晶任意等级、胶囊与名誉满级专属）。
//
// 回归背景：等级账本过去和结晶共用同一个 AwardedExperience，满级角色打怪
// 一直在推进冒险团等级（线上日志 adventure_group_credit=454 new_level=90）。
func TestCreditExperienceKeepsLevelLedgerForSubCapExperienceOnly(t *testing.T) {
	config := creditTestConfig()
	config.Growth = GrowthConfig{
		MaximumLevel:       3,
		RateParts:          growthRateScale, // 1.0，增益与冒险团经验 1:1
		RequiredExperience: []uint64{0, 0, 100, 200},
		LevelBenefits:      make([]Benefits, 4),
	}
	now := time.Date(2026, time.September, 29, 4, 0, 0, 0, time.UTC)
	repositories := creditTestRepositories(t, dnfrepo.AccountRecord{AccountID: "dnf:1"})

	// 1) 满级角色：整笔都是"越过上限"的部分 → 结晶/名誉照涨，等级账本一点不动。
	credit, err := CreditExperience(
		context.Background(), repositories.Account, config, "dnf:1",
		ExperienceCredit{AwardedExperience: 15_000_000, MaximumLevelExperience: 15_000_000},
		now,
	)
	if err != nil {
		t.Fatal(err)
	}
	if credit.BravePoints != 1 || credit.HonorExperience != 15_000_000 {
		t.Fatalf("满级笔照样要进结晶与名誉: %+v", credit)
	}
	// 等级账本必须一个字都没写（ApplyGrowthExperience 在 gain==0 时直接返回，
	// 既不生成元数据也不报告等级）。
	afterMaxLevel, found, err := repositories.Account.Load(context.Background(), "dnf:1")
	if err != nil || !found {
		t.Fatalf("account found=%t err=%v", found, err)
	}
	if value := afterMaxLevel.Metadata[GrowthMetadataKey]; value != "" {
		t.Fatalf("满级经验不该推进冒险团等级，却写了账本: %s", value)
	}

	// 2) 未满级角色：全额进等级账本（150 → 2 级 + 50 余量），等级口径照旧。
	credit, err = CreditExperience(
		context.Background(), repositories.Account, config, "dnf:1",
		ExperienceCredit{AwardedExperience: 150},
		now,
	)
	if err != nil {
		t.Fatal(err)
	}
	if credit.GrowthLevel != 2 || credit.GrowthExperience != 50 {
		t.Fatalf("满级前的经验必须照旧累加: level=%d exp=%d", credit.GrowthLevel, credit.GrowthExperience)
	}

	// 持久化的账本必须与返回值一致。
	account, found, err := repositories.Account.Load(context.Background(), "dnf:1")
	if err != nil || !found {
		t.Fatalf("account found=%t err=%v", found, err)
	}
	state, err := readGrowthState(account, config.Growth, Summary{})
	if err != nil {
		t.Fatal(err)
	}
	if state.Level != 2 || state.Experience != 50 {
		t.Fatalf("persisted growth=%+v", state)
	}
}
