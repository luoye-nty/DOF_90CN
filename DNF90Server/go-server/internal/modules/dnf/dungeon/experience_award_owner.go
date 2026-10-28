package dungeon

import (
	"context"
	"errors"
	"strings"
	"time"

	"longheng.io/server/internal/modules/dnf/adventuregroup"
	"longheng.io/server/internal/modules/dnf/progression"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

var ErrExperienceAwardStateInvalid = errors.New("dungeon experience award persisted state is invalid")

// ExperienceAwardCommand 是"一次已证明的角色经验结算"的完整输入：计划本身由
// bridge 用运行时 PVF 算好（角色经验/等级/SP + 熟练等级），本命令只负责把它
// 和账号级冒险团状态放进同一个事务。
type ExperienceAwardCommand struct {
	AccountID   string
	CharacterID string
	// Plan 由调用方用运行时 PVF 规划（含幂等前置条件 expected → next）。
	Plan progression.CommitPlan
	// AdventureRuntime 是运行时 PVF 的冒险团配置；AdventureCredit 描述这笔经验
	// 怎么进账号账本（任意等级进骁勇结晶，满级部分才进胶囊槽与名誉经验）。
	// 两者都缺时只落角色经验。
	AdventureRuntime *adventuregroup.RuntimeConfig
	AdventureCredit  adventuregroup.ExperienceCredit
	UpdatedAt        time.Time
}

// ExperienceAwardResult 汇报事务内实际提交的状态。
type ExperienceAwardResult struct {
	Commit                 progression.CommitResult
	AdventureGroupCredit   uint64
	AdventureGroupMaximum  uint64
	AdventureGroupCrystals uint32
}

// CommitExperienceAward 在同一个事务里同时落角色经验/等级/SP 与账号级冒险团
// 状态（骁勇结晶 / 成长胶囊槽 / 名誉经验）。
//
// 为什么必须是一个事务：同一笔经验同时进角色和账号账本。分成两次写，
// 就会出现"经验涨了、结晶没涨"这种既不报错也不自愈的状态 —— 线上长期表现为
// 打怪一辈子换不到一个结晶（怪物经验占一次通关总经验的 3~8 成）。
//
// 口径见 adventuregroup.ExperienceCredit：骁勇结晶收**任意等级**的经验，
// 成长胶囊槽与名誉经验只收满级溢出部分。
//
// 重放（同一次死亡已经落过账）只返回幂等结果，不再记第二笔结晶。
func (o *Owner) CommitExperienceAward(
	ctx context.Context,
	cmd ExperienceAwardCommand,
) (ExperienceAwardResult, error) {
	if o == nil || o.settlements == nil ||
		strings.TrimSpace(cmd.CharacterID) == "" || strings.TrimSpace(cmd.AccountID) == "" {
		return ExperienceAwardResult{}, ErrOwnerUnavailable
	}
	ctx = contextOrBackground(ctx)
	now := updatedAtOrNow(cmd.UpdatedAt)
	var result ExperienceAwardResult
	err := o.settlements.WithinCharacterSettlement(ctx, cmd.CharacterID, func(tx dnfrepo.Group) error {
		if tx.Character == nil || tx.Skill == nil {
			return ErrExperienceAwardStateInvalid
		}
		applied, err := progression.ApplyCommitPlan(ctx, tx.Character, tx.Skill, cmd.Plan)
		if err != nil {
			return err
		}
		result.Commit = applied
		if applied.Idempotent || cmd.AdventureCredit.Normalize().Empty() || cmd.AdventureRuntime == nil {
			return nil
		}
		if tx.Account == nil {
			return ErrExperienceAwardStateInvalid
		}
		credit, err := adventuregroup.CreditExperience(
			ctx,
			tx.Account,
			*cmd.AdventureRuntime,
			strings.TrimSpace(cmd.AccountID),
			cmd.AdventureCredit,
			now,
		)
		if err != nil {
			// 账号行缺失时的取舍：
			//   - 这笔经验里有**满级部分**（进胶囊槽/名誉经验，属于原版规则）→
			//     整笔失败。账号行没了说明库不对，静默丢掉这两个账本比报错更糟。
			//   - 只有**未满级经验**（2026-09-24 新增的"镜像进骁勇结晶"）→
			//     跳过记账但照常发经验。这只是加分项，不该因为一行账本缺失
			//     就让玩家连杀怪经验都拿不到（"改了A把B弄坏"正是要避免的）。
			if errors.Is(err, adventuregroup.ErrCreditAccountNotFound) &&
				cmd.AdventureCredit.Normalize().MaximumLevelExperience == 0 {
				return nil
			}
			return err
		}
		result.AdventureGroupCredit = credit.AwardedExperience
		result.AdventureGroupMaximum = credit.MaximumLevelExperience
		result.AdventureGroupCrystals = credit.BravePoints
		return nil
	})
	if err != nil {
		return ExperienceAwardResult{}, err
	}
	return result, nil
}
