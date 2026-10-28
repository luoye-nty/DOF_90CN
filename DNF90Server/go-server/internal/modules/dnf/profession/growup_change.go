package profession

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfskill "longheng.io/server/internal/modules/dnf/skill"
)

// Growup Change（转职体验）实现。
//
// 规则与 86JP GrowupChangeApplicationService 保持一致：
//   - 等级必须落在 [GrowupChangeMinLevel, GrowupChangeMaxLevel]（86 growup.etc
//     "grow up change lv" 为 15~49，即"限制 49 级以下角色开启"）。
//   - 角色必须已转职（grow_type 低 4 位 > 0）且未觉醒（高 4 位 == 0）。
//   - 目标职业分支不能等于当前分支，且必须被当前 job 的 PVF profile 支持。
//   - 金币成本按历史转职次数（changeCount）从金币表取值，达到表尾后封顶。
//
// 成功后在同一事务内扣金币、改写 grow_type、递增 changeCount 并重建目标
// 职业的技能基线（复用 ApplySkillTransition 的 KindClassChange 分支）。

var (
	ErrGrowupChangeUnavailable     = errors.New("dnf growup change is unavailable")
	ErrGrowupChangeOwnerRequired   = errors.New("dnf growup change owner is required")
	ErrGrowupChangeTimeRequired    = errors.New("dnf growup change commit time is required")
	ErrGrowupChangeTargetRange     = errors.New("dnf growup change target grow type is out of range")
	ErrGrowupChangeSkillTransition = errors.New("dnf growup change skill transition failed")
)

const (
	GrowupChangeMinLevel = 15
	GrowupChangeMaxLevel = 49
)

// GrowupChangeAckResultCode 与 86 GrowupChangeAckBuilder/ResultCode 对齐：
// 客户端只识别 0（成功）、19（状态不合法）、22（金币不足）三个值。
const (
	GrowupChangeAckResultCodeSuccess          = 0
	GrowupChangeAckResultCodeInvalidState     = 19
	GrowupChangeAckResultCodeInsufficientGold = 22
)

// growupChangeGoldCosts 是 86 真实 PVF character/growup.etc 的
// "grow up change gold" 表（含 86 SelfTest 已验证的 0 / 100113 / 133484 /
// index 9 == 1000000 约束）。index 0 免费，>= 表尾后取最后一项封顶。
var growupChangeGoldCosts = []int64{
	0, 100113, 133484, 166856, 200227, 233599, 266970, 300342, 333713, 1000000,
}

// ResolveGrowupChangeGoldCost 按 changeCount 解析转职体验金币成本。
func ResolveGrowupChangeGoldCost(changeCount int) int64 {
	if changeCount < 0 {
		changeCount = 0
	}
	if changeCount >= len(growupChangeGoldCosts) {
		changeCount = len(growupChangeGoldCosts) - 1
	}
	return growupChangeGoldCosts[changeCount]
}

// GrowupChangeInput 是一次转职体验请求的入参。
type GrowupChangeInput struct {
	AccountID      string
	CharacterID    string
	TargetGrowType byte
	Profiles       *Profiles
	SkillCatalog   *dnfskill.Table
	CommittedAt    time.Time
}

// GrowupChangeResult 是转职体验的结果。Success 为 true 时角色与技能记录为
// 提交后的快照，可直接用于客户端刷新。
type GrowupChangeResult struct {
	Success             bool
	ResultCode          int
	TargetGrowType      byte
	NewGrowType         byte
	PreviousChangeCount int
	NewChangeCount      int
	GoldCost            int64
	UpdatedGold         int64
	Character           dnfrepo.CharacterRecord
	Skill               dnfrepo.SkillRecord
}

// ApplyGrowupChange 在一个 character-scoped 事务内执行转职体验。
func ApplyGrowupChange(
	ctx context.Context,
	repositories dnfrepo.Group,
	input GrowupChangeInput,
) (GrowupChangeResult, error) {
	rejected := func(code int) GrowupChangeResult {
		return GrowupChangeResult{
			Success:        false,
			ResultCode:     code,
			TargetGrowType: input.TargetGrowType,
			NewGrowType:    input.TargetGrowType,
		}
	}
	if repositories.CharacterSettlement == nil || input.Profiles == nil || input.SkillCatalog == nil {
		return rejected(GrowupChangeAckResultCodeInvalidState), ErrGrowupChangeUnavailable
	}
	if ctx == nil {
		ctx = context.Background()
	}
	input.AccountID = strings.TrimSpace(input.AccountID)
	input.CharacterID = strings.TrimSpace(input.CharacterID)
	if input.AccountID == "" || input.CharacterID == "" {
		return rejected(GrowupChangeAckResultCodeInvalidState), ErrGrowupChangeOwnerRequired
	}
	if input.TargetGrowType > 0x0f {
		return rejected(GrowupChangeAckResultCodeInvalidState), ErrGrowupChangeTargetRange
	}
	if input.CommittedAt.IsZero() {
		return rejected(GrowupChangeAckResultCodeInvalidState), ErrGrowupChangeTimeRequired
	}

	var candidate GrowupChangeResult
	err := repositories.CharacterSettlement.WithinCharacterSettlement(ctx, input.CharacterID, func(tx dnfrepo.Group) error {
		if tx.Character == nil || tx.Skill == nil {
			return ErrGrowupChangeUnavailable
		}
		character, found, err := tx.Character.Load(ctx, input.CharacterID)
		if err != nil {
			return err
		}
		if !found || strings.TrimSpace(character.CharacterID) != input.CharacterID ||
			strings.TrimSpace(character.AccountID) != input.AccountID {
			candidate = rejected(GrowupChangeAckResultCodeInvalidState)
			return nil
		}
		character = dnfrepo.CloneCharacter(character)
		skill, found, err := tx.Skill.Load(ctx, input.CharacterID)
		if err != nil {
			return err
		}
		if !found || strings.TrimSpace(skill.CharacterID) != input.CharacterID {
			candidate = rejected(GrowupChangeAckResultCodeInvalidState)
			return nil
		}
		skill = dnfrepo.CloneSkill(skill)

		jobValue, err := strconv.ParseUint(strings.TrimSpace(character.Job), 10, 8)
		if err != nil {
			candidate = rejected(GrowupChangeAckResultCodeInvalidState)
			return nil
		}
		firstGrow, awakening := Decode(byte(character.Stats["grow_type"]))

		// 等级范围（限制 49 级以下角色开启，86 "grow up change lv" 15~49）。
		if character.Level < GrowupChangeMinLevel || character.Level > GrowupChangeMaxLevel {
			candidate = rejected(GrowupChangeAckResultCodeInvalidState)
			return nil
		}
		// 必须已转职且未觉醒。
		if firstGrow <= 0 || awakening != 0 {
			candidate = rejected(GrowupChangeAckResultCodeInvalidState)
			return nil
		}
		// 目标不能与当前职业相同。
		if input.TargetGrowType == firstGrow {
			candidate = rejected(GrowupChangeAckResultCodeInvalidState)
			return nil
		}
		// 目标职业必须被当前 job 的 PVF profile 支持。
		if !input.Profiles.SupportsSelection(byte(jobValue), input.TargetGrowType, 0) {
			candidate = rejected(GrowupChangeAckResultCodeInvalidState)
			return nil
		}

		previousChangeCount := 0
		if raw := character.Stats["growup_change_count"]; raw > 0 && raw <= math.MaxInt32 {
			previousChangeCount = int(raw)
		}
		goldCost := ResolveGrowupChangeGoldCost(previousChangeCount)
		currentGold := character.Stats["gold"]
		if currentGold < 0 {
			candidate = rejected(GrowupChangeAckResultCodeInvalidState)
			return nil
		}
		if currentGold < goldCost {
			candidate = GrowupChangeResult{
				Success:             false,
				ResultCode:          GrowupChangeAckResultCodeInsufficientGold,
				TargetGrowType:      input.TargetGrowType,
				NewGrowType:         input.TargetGrowType,
				PreviousChangeCount: previousChangeCount,
				NewChangeCount:      previousChangeCount,
				GoldCost:            goldCost,
				UpdatedGold:         currentGold,
			}
			return nil
		}

		now := input.CommittedAt.UTC()
		newChangeCount := previousChangeCount
		if previousChangeCount != math.MaxInt32 {
			newChangeCount = previousChangeCount + 1
		}
		previousGrowType := byte(character.Stats["grow_type"])
		if character.Stats == nil {
			character.Stats = make(map[string]int64, 3)
		}
		character.Stats["gold"] = currentGold - goldCost
		character.Stats["grow_type"] = int64(input.TargetGrowType)
		character.Stats["growup_change_count"] = int64(newChangeCount)
		character.UpdatedAt = now

		transition := Transition{
			Kind:             KindClassChange,
			ChainType:        1,
			GrowNumber:       input.TargetGrowType,
			PreviousGrowType: previousGrowType,
			NewGrowType:      input.TargetGrowType,
			FirstGrowType:    input.TargetGrowType,
			AwakeningStage:   0,
		}
		skill, err = input.Profiles.ApplySkillTransition(
			input.SkillCatalog,
			byte(jobValue),
			character.Level,
			transition,
			skill,
			skill.Points,
		)
		if err != nil {
			return fmt.Errorf("%w: job=%d target=%d", ErrGrowupChangeUnavailable, jobValue, input.TargetGrowType)
		}
		skill.UpdatedAt = now

		if err := dnfrepo.SaveCharacterFields(ctx, tx.Character, character, dnfrepo.CharacterFieldBase, dnfrepo.CharacterFieldStats); err != nil {
			return err
		}
		if err := dnfrepo.SaveSkillFields(ctx, tx.Skill, skill,
			dnfrepo.SkillFieldSkills, dnfrepo.SkillFieldLayouts, dnfrepo.SkillFieldCooldowns, dnfrepo.SkillFieldPoints,
		); err != nil {
			return err
		}

		candidate = GrowupChangeResult{
			Success:             true,
			ResultCode:          GrowupChangeAckResultCodeSuccess,
			TargetGrowType:      input.TargetGrowType,
			NewGrowType:         input.TargetGrowType,
			PreviousChangeCount: previousChangeCount,
			NewChangeCount:      newChangeCount,
			GoldCost:            goldCost,
			UpdatedGold:         currentGold - goldCost,
			Character:           character,
			Skill:               skill,
		}
		return nil
	})
	if err != nil {
		return GrowupChangeResult{}, err
	}
	return candidate, nil
}
