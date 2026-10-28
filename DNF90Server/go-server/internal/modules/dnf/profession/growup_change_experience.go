package profession

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

// Growup Experience（转职体验）实现。
//
// 90CN 客户端"转职体验"界面的"体验"按钮发送 REQUEST_ITEM_GROWTYPE_EXPERIENCE
// 请求。与转职（ApplyGrowupChange）相同的等级/状态校验，但体验免费：
//   - 不扣除金币（GoldCost 恒为 0，UpdatedGold 保持当前值）。
//   - 不递增 growup_change_count（体验不消耗正式转职次数）。
//
// 成功后在同一事务内改写 grow_type 并重建目标职业的技能基线（复用
// ApplySkillTransition 的 KindClassChange 分支），结果快照与转职一致。

var (
	ErrGrowupExperienceUnavailable   = errors.New("dnf growup experience is unavailable")
	ErrGrowupExperienceOwnerRequired = errors.New("dnf growup experience owner is required")
	ErrGrowupExperienceTimeRequired  = errors.New("dnf growup experience commit time is required")
	ErrGrowupExperienceTargetRange   = errors.New("dnf growup experience target grow type is out of range")
)

// ApplyGrowupExperience 在一个 character-scoped 事务内执行免费转职体验。
func ApplyGrowupExperience(
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
		return rejected(GrowupChangeAckResultCodeInvalidState), ErrGrowupExperienceUnavailable
	}
	if ctx == nil {
		ctx = context.Background()
	}
	input.AccountID = strings.TrimSpace(input.AccountID)
	input.CharacterID = strings.TrimSpace(input.CharacterID)
	if input.AccountID == "" || input.CharacterID == "" {
		return rejected(GrowupChangeAckResultCodeInvalidState), ErrGrowupExperienceOwnerRequired
	}
	if input.TargetGrowType > 0x0f {
		return rejected(GrowupChangeAckResultCodeInvalidState), ErrGrowupExperienceTargetRange
	}
	if input.CommittedAt.IsZero() {
		return rejected(GrowupChangeAckResultCodeInvalidState), ErrGrowupExperienceTimeRequired
	}

	var candidate GrowupChangeResult
	err := repositories.CharacterSettlement.WithinCharacterSettlement(ctx, input.CharacterID, func(tx dnfrepo.Group) error {
		if tx.Character == nil || tx.Skill == nil {
			return ErrGrowupExperienceUnavailable
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

		// 等级范围（限制 49 级以下角色开启，与转职一致 15~49）。
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
		currentGold := character.Stats["gold"]
		if currentGold < 0 {
			candidate = rejected(GrowupChangeAckResultCodeInvalidState)
			return nil
		}

		now := input.CommittedAt.UTC()
		previousGrowType := byte(character.Stats["grow_type"])
		if character.Stats == nil {
			character.Stats = make(map[string]int64, 2)
		}
		character.Stats["grow_type"] = int64(input.TargetGrowType)
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
			return fmt.Errorf("%w: job=%d target=%d", ErrGrowupExperienceUnavailable, jobValue, input.TargetGrowType)
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
			NewChangeCount:      previousChangeCount,
			GoldCost:            0,
			UpdatedGold:         currentGold,
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
