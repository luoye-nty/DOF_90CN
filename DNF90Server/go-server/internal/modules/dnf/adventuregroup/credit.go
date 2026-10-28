package adventuregroup

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

var ErrCreditAccountNotFound = errors.New("adventure-group credit account is missing")

// ExperienceCredit 描述"一次已经发给角色的经验"该怎么进账号级冒险团账本。
//
// 口径（2026-09-24 服主确认，此前只有满级经验才记账）：
//
//   - 骁勇结晶：**任意等级**获得的经验都按 PVF `[exp to get shop point]` 累计
//     （真值 1500 万 = 1 个，封顶 `[max shop point]` = 250）。角色自己的升级
//     经验完全不受影响 —— 这里只是把同一笔经验**镜像**一份进账号账本，
//     "获得多少经验就累计多少"。
//   - 成长胶囊槽 / 名誉经验：**满级专属**。客户端自己的说明写着
//     "Lv90以上的角色获得经验值时，经验值会累积在成长胶囊槽中"，
//     名誉等级同样是满级之后的系统，所以只有越过等级上限的那部分进这两个账本。
//
// 三个账本共用一个数值来源，是为了让同一笔结算永远算出互相一致的账号状态；
// 现在两个数值来源必须一起给出，免得调用方只填一个而悄悄改了别人的口径。
type ExperienceCredit struct {
	// AwardedExperience 是这次**真正发给角色**的经验（含成长契约 / 装备 /
	// 疲劳蓄电池 / 任务连续性补足等加成），进骁勇结晶。
	AwardedExperience uint64
	// MaximumLevelExperience 是其中**越过等级上限**的那部分，进成长胶囊槽与
	// 名誉经验。它永远不该大于 AwardedExperience，Normalize 会兜住。
	MaximumLevelExperience uint64
}

// Normalize 收敛两个数值的关系：满级部分不可能多于总经验。
func (c ExperienceCredit) Normalize() ExperienceCredit {
	if c.MaximumLevelExperience > c.AwardedExperience {
		c.MaximumLevelExperience = c.AwardedExperience
	}
	return c
}

// Empty 表示这笔经验什么都不用记（零经验或重放）。
func (c ExperienceCredit) Empty() bool {
	return c.AwardedExperience == 0 && c.MaximumLevelExperience == 0
}

// ExperienceCreditResult 汇报记入之后的账号级数值，便于日志与测试核对。
type ExperienceCreditResult struct {
	AccountID              string
	AwardedExperience      uint64
	MaximumLevelExperience uint64
	BravePoints            uint32
	BraveExperience        uint64
	CapsuleExperience      uint64
	HonorExperience        uint64
	// GrowthLevel/GrowthExperience are the adventure-group level ledger after
	// this credit, for logging and tests. A credit that carries no creditable
	// level experience (a max-level character, or a zero-exp replay) is a no-op
	// and reports zero here without touching the stored ledger.
	GrowthLevel      int
	GrowthExperience uint64
}

// ApplyExperienceCredit 在**调用方已经加载、并即将在自己的事务里保存**的账号
// 记录上记入一笔经验：
//
//   - 骁勇结晶：任意等级的经验都算，按 PVF `[exp to get shop point]` 换算，
//     封顶 `[max shop point]`；
//   - 成长胶囊槽：只算满级溢出部分，封顶 `[capsule max exp] × [capsule max count]`；
//   - 账号名誉经验 `accounts.honor_exp`：只算满级溢出部分（名誉等级是满级专属）。
//
// 调用方负责保存；本函数不碰仓库，因此不会和调用方已做的账号改动互相覆盖。
func ApplyExperienceCredit(
	account *dnfrepo.AccountRecord,
	config RuntimeConfig,
	credit ExperienceCredit,
	now time.Time,
) (ExperienceCreditResult, error) {
	if account == nil {
		return ExperienceCreditResult{}, ErrOwnerUnavailable
	}
	accountID := strings.TrimSpace(account.AccountID)
	if accountID == "" {
		return ExperienceCreditResult{}, ErrAccountRequired
	}
	credit = credit.Normalize()
	if credit.Empty() {
		return ExperienceCreditResult{AccountID: accountID}, nil
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	state, err := ParseRuntimeState(*account, config, now)
	if err != nil {
		return ExperienceCreditResult{}, err
	}
	state.AddExperience(config, credit.AwardedExperience, credit.MaximumLevelExperience)
	// 冒险团**等级**账本只收"没越过角色等级上限"的那部分经验（服主口径
	// 2026-09-29：满级角色获得的经验不该再累加进冒险团经验，满级之前照旧）。
	// 满级角色的 MaximumLevelExperience 就是整笔（honor.PostCapExperienceShare
	// 在 previousLevel >= maxLevel 时返回全额），所以这里自然归零；未满级角色
	// 通常是 0，全额照记。骁勇结晶 / 成长胶囊槽 / 名誉经验三条口径不受影响，
	// 走的仍是上面的 state.AddExperience 与下面的 HonorExp。
	levelGain := credit.AwardedExperience - credit.MaximumLevelExperience
	growth, err := ApplyGrowthExperience(account, config.Growth, Summary{}, uint32(min(levelGain, math.MaxUint32)), now)
	if err != nil {
		return ExperienceCreditResult{}, err
	}
	encoded, err := state.Marshal()
	if err != nil {
		return ExperienceCreditResult{}, err
	}
	if account.Metadata == nil {
		account.Metadata = make(map[string]string)
	}
	account.Metadata[RuntimeStateMetadataKey] = encoded
	// 名誉经验只收满级溢出：未满级经验不进名誉面板，这是服主确认的口径。
	if credit.MaximumLevelExperience > 0 {
		if account.HonorExp > math.MaxUint64-credit.MaximumLevelExperience {
			account.HonorExp = math.MaxUint64
		} else {
			account.HonorExp += credit.MaximumLevelExperience
		}
	}
	account.UpdatedAt = now
	return ExperienceCreditResult{
		AccountID:              accountID,
		AwardedExperience:      credit.AwardedExperience,
		MaximumLevelExperience: credit.MaximumLevelExperience,
		BravePoints:            state.ShopPoints[ShopPointBrave],
		BraveExperience:        state.BraveExperience,
		CapsuleExperience:      state.GrowthExperience,
		HonorExperience:        account.HonorExp,
		GrowthLevel:            growth.Level,
		GrowthExperience:       growth.Experience,
	}, nil
}

// CreditExperience 是 ApplyExperienceCredit 的读-改-写版本：在**调用方已经
// 打开的事务**里加载账号、记入经验并保存。
//
// 怪物击杀、任务完成这些路径平时只碰角色/技能两张表，没有账号记录在手，
// 用这个版本就能把账号级冒险团状态并进同一个事务提交，而不是事后再补一次
// 可能失败的写（补写失败会让"经验涨了结晶没涨"这种状态静默留存）。
func CreditExperience(
	ctx context.Context,
	accounts dnfrepo.AccountRepository,
	config RuntimeConfig,
	accountID string,
	credit ExperienceCredit,
	now time.Time,
) (ExperienceCreditResult, error) {
	if accounts == nil {
		return ExperienceCreditResult{}, ErrOwnerUnavailable
	}
	accountID = strings.TrimSpace(accountID)
	if accountID == "" {
		return ExperienceCreditResult{}, ErrAccountRequired
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return ExperienceCreditResult{}, err
	}
	if credit.Normalize().Empty() {
		return ExperienceCreditResult{AccountID: accountID}, nil
	}
	account, found, err := accounts.Load(ctx, accountID)
	if err != nil {
		return ExperienceCreditResult{}, fmt.Errorf("load adventure-group credit account: %w", err)
	}
	if !found || strings.TrimSpace(account.AccountID) != accountID {
		return ExperienceCreditResult{}, fmt.Errorf("%w: account=%s", ErrCreditAccountNotFound, accountID)
	}
	account = dnfrepo.CloneAccount(account)
	result, err := ApplyExperienceCredit(&account, config, credit, now)
	if err != nil {
		return ExperienceCreditResult{}, err
	}
	if err := accounts.Save(ctx, account); err != nil {
		return ExperienceCreditResult{}, fmt.Errorf("save adventure-group credit account: %w", err)
	}
	return result, nil
}
