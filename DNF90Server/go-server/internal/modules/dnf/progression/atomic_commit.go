package progression

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

const characterExperienceStatKey = "exp"

var (
	ErrCommitPlanInvalid         = errors.New("dnf progression commit plan is invalid")
	ErrCommitOwnershipMismatch   = errors.New("dnf progression commit ownership mismatch")
	ErrCommitStateMissing        = errors.New("dnf progression commit state is missing")
	ErrCommitStateInvalid        = errors.New("dnf progression persisted state is invalid")
	ErrCommitVersionPrecondition = errors.New("dnf progression commit version precondition failed")
)

// CommitState is the complete optimistic version used by one progression
// commit. Expected is compared before any write; Next is the caller-computed
// state to persist. The repository layer does not calculate EXP, levels, SP,
// or TP.
type CommitState struct {
	Level       int
	Experience  uint32
	SkillPoints dnfrepo.SkillPointState
}

// CommitPlan binds a caller-computed Expected -> Next transition to one
// account-owned character. Expected is the version precondition; it avoids a
// lost update without coupling progression to unrelated character fields.
type CommitPlan struct {
	CharacterID string
	AccountID   string
	Expected    CommitState
	Next        CommitState
	// Allows a saturated same-level legacy overflow to converge back to the
	// current PVF max-level entry threshold. Ordinary EXP regressions remain
	// invalid.
	AllowExperienceCapNormalization bool
	// ExpectedCharacterStats protects state that shares the character stats
	// record with EXP but has its own domain progression, such as HonorExpert.
	ExpectedCharacterStats map[string]int64
	NextCharacterStats     map[string]int64
}

// CommitPlanFromExperienceSkillPoints maps the existing pure planner result
// into the atomic repository plan without recalculating any rule.
func CommitPlanFromExperienceSkillPoints(
	characterID string,
	accountID string,
	planned ExperienceSkillPointPlan,
) CommitPlan {
	return CommitPlan{
		CharacterID: characterID,
		AccountID:   accountID,
		Expected: CommitState{
			Level:       planned.Experience.PreviousLevel,
			Experience:  planned.Experience.PreviousExperience,
			SkillPoints: planned.SkillPoints.Previous,
		},
		Next: CommitState{
			Level:       planned.Experience.NewLevel,
			Experience:  planned.Experience.NewExperience,
			SkillPoints: planned.SkillPoints.New,
		},
		AllowExperienceCapNormalization: planned.Experience.Saturated &&
			planned.Experience.NewLevel == planned.Experience.PreviousLevel &&
			planned.Experience.NewExperience < planned.Experience.PreviousExperience,
	}
}

// CommitResult reports the exact state observed in the transaction. A retry
// that observes Next is successful and Idempotent without issuing a write. If
// a later progression state is present, the Expected precondition fails.
type CommitResult struct {
	Previous   CommitState
	Current    CommitState
	Applied    bool
	Idempotent bool
}

// Commit 在角色-技能事务里应用一个 CommitPlan（规则见 ApplyCommitPlan）。
func Commit(
	ctx context.Context,
	uow dnfrepo.CharacterProgressionUnitOfWork,
	plan CommitPlan,
) (CommitResult, error) {
	plan.CharacterID = strings.TrimSpace(plan.CharacterID)
	plan.AccountID = strings.TrimSpace(plan.AccountID)
	if uow == nil {
		return CommitResult{}, dnfrepo.ErrCharacterProgressionTransactionUnavailable
	}
	// 调用方的计划与上下文必须在**开事务之前**判定：无效计划不得打开事务。
	// 这条契约由 atomic_commit_test.go 钉着；ApplyCommitPlan 内部会再校验一次。
	if err := validateCommitPlan(plan); err != nil {
		return CommitResult{}, err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return CommitResult{}, err
	}
	var result CommitResult
	err := uow.WithinCharacterProgression(
		ctx,
		plan.CharacterID,
		func(characters dnfrepo.CharacterRepository, skills dnfrepo.SkillRepository) error {
			applied, applyErr := ApplyCommitPlan(ctx, characters, skills, plan)
			result = applied
			return applyErr
		},
	)
	if err != nil {
		return CommitResult{}, err
	}
	return result, nil
}

// ApplyCommitPlan 在**调用方已经打开的事务**里应用同一个 CommitPlan：前置校验
// （expected → next）、幂等判定与字段写入全部照旧，只是不再由本函数自己开
// 角色-技能事务。
//
// 需要把「角色经验」和「账号级冒险团状态（骁勇结晶 / 成长胶囊槽 / 荣誉经验）」
// 放进同一个提交的调用方用它 —— 例如怪物击杀经验：同一笔经验同时喂熟练等级和
// 结晶，拆成两次写就会出现"经验涨了、结晶没涨"的静默状态。
func ApplyCommitPlan(
	ctx context.Context,
	characters dnfrepo.CharacterRepository,
	skills dnfrepo.SkillRepository,
	plan CommitPlan,
) (CommitResult, error) {
	plan.CharacterID = strings.TrimSpace(plan.CharacterID)
	plan.AccountID = strings.TrimSpace(plan.AccountID)
	if characters == nil || skills == nil {
		return CommitResult{}, dnfrepo.ErrCharacterProgressionTransactionUnavailable
	}
	if err := validateCommitPlan(plan); err != nil {
		return CommitResult{}, err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return CommitResult{}, err
	}

	character, characterExists, err := characters.Load(ctx, plan.CharacterID)
	if err != nil {
		return CommitResult{}, err
	}
	if !characterExists {
		return CommitResult{}, fmt.Errorf("%w: character=%s", ErrCommitStateMissing, plan.CharacterID)
	}
	if strings.TrimSpace(character.CharacterID) != plan.CharacterID || strings.TrimSpace(character.AccountID) != plan.AccountID {
		return CommitResult{}, fmt.Errorf(
			"%w: character=%s expected_account=%s actual_character=%s actual_account=%s",
			ErrCommitOwnershipMismatch,
			plan.CharacterID,
			plan.AccountID,
			character.CharacterID,
			character.AccountID,
		)
	}

	skill, skillExists, err := skills.Load(ctx, plan.CharacterID)
	if err != nil {
		return CommitResult{}, err
	}
	if !skillExists {
		return CommitResult{}, fmt.Errorf("%w: character=%s", ErrCommitStateMissing, plan.CharacterID)
	}
	if strings.TrimSpace(skill.CharacterID) != plan.CharacterID {
		return CommitResult{}, fmt.Errorf(
			"%w: character=%s skill_character=%s",
			ErrCommitOwnershipMismatch,
			plan.CharacterID,
			skill.CharacterID,
		)
	}

	current, err := persistedCommitState(character, skill)
	if err != nil {
		return CommitResult{}, err
	}
	result := CommitResult{Previous: current, Current: current}
	if commitStatesEqual(current, plan.Next) && characterStatsMatch(character.Stats, plan.NextCharacterStats) {
		result.Idempotent = true
		return result, nil
	}
	if commitStatesEqual(current, plan.Next) && !commitStatesEqual(current, plan.Expected) {
		return CommitResult{}, fmt.Errorf(
			"%w: character=%s progression reached next state without matching protected stats",
			ErrCommitStateInvalid,
			plan.CharacterID,
		)
	}
	if !commitStatesEqual(current, plan.Expected) {
		return CommitResult{}, fmt.Errorf(
			"%w: character=%s expected=%+v actual=%+v next=%+v",
			ErrCommitVersionPrecondition,
			plan.CharacterID,
			plan.Expected,
			current,
			plan.Next,
		)
	}
	if !characterStatsMatch(character.Stats, plan.ExpectedCharacterStats) {
		return CommitResult{}, fmt.Errorf(
			"%w: character=%s expected character stats do not match",
			ErrCommitVersionPrecondition,
			plan.CharacterID,
		)
	}

	character.Level = plan.Next.Level
	character.Stats[characterExperienceStatKey] = int64(plan.Next.Experience)
	for key, value := range plan.NextCharacterStats {
		character.Stats[key] = value
	}
	skill.Points = plan.Next.SkillPoints
	if err := dnfrepo.SaveCharacterFields(
		ctx,
		characters,
		character,
		dnfrepo.CharacterFieldBase,
		dnfrepo.CharacterFieldStats,
	); err != nil {
		return CommitResult{}, err
	}
	if err := dnfrepo.SaveSkillFields(ctx, skills, skill, dnfrepo.SkillFieldPoints); err != nil {
		return CommitResult{}, err
	}

	result.Current = plan.Next
	result.Applied = true
	return result, nil
}

func validateCommitPlan(plan CommitPlan) error {
	if plan.CharacterID == "" || plan.AccountID == "" {
		return fmt.Errorf("%w: character/account ownership is required", ErrCommitPlanInvalid)
	}
	if err := validateCommitState(plan.Expected); err != nil {
		return fmt.Errorf("%w: expected: %v", ErrCommitPlanInvalid, err)
	}
	if err := validateCommitState(plan.Next); err != nil {
		return fmt.Errorf("%w: next: %v", ErrCommitPlanInvalid, err)
	}
	if plan.Next.Level < plan.Expected.Level {
		return fmt.Errorf("%w: level regressed %d -> %d", ErrCommitPlanInvalid, plan.Expected.Level, plan.Next.Level)
	}
	if plan.Next.Experience < plan.Expected.Experience &&
		(!plan.AllowExperienceCapNormalization || plan.Next.Level != plan.Expected.Level) {
		return fmt.Errorf("%w: cumulative experience regressed %d -> %d", ErrCommitPlanInvalid, plan.Expected.Experience, plan.Next.Experience)
	}
	if plan.Next.SkillPoints.TotalSP < plan.Expected.SkillPoints.TotalSP ||
		plan.Next.SkillPoints.TotalTP < plan.Expected.SkillPoints.TotalTP {
		return fmt.Errorf("%w: total SP/TP regressed expected=%+v next=%+v", ErrCommitPlanInvalid, plan.Expected.SkillPoints, plan.Next.SkillPoints)
	}
	if err := validateCharacterStatMap(plan.ExpectedCharacterStats); err != nil {
		return err
	}
	if err := validateCharacterStatMap(plan.NextCharacterStats); err != nil {
		return err
	}
	for key := range plan.ExpectedCharacterStats {
		if _, found := plan.NextCharacterStats[key]; !found {
			return fmt.Errorf("%w: expected character stat %q has no next value", ErrCommitPlanInvalid, key)
		}
	}
	expectedSpentSP := plan.Expected.SkillPoints.TotalSP - plan.Expected.SkillPoints.RemainingSP
	nextSpentSP := plan.Next.SkillPoints.TotalSP - plan.Next.SkillPoints.RemainingSP
	expectedSpentTP := plan.Expected.SkillPoints.TotalTP - plan.Expected.SkillPoints.RemainingTP
	nextSpentTP := plan.Next.SkillPoints.TotalTP - plan.Next.SkillPoints.RemainingTP
	if expectedSpentSP != nextSpentSP || expectedSpentTP != nextSpentTP {
		return fmt.Errorf(
			"%w: progression changed spent points SP=%d->%d TP=%d->%d",
			ErrCommitPlanInvalid,
			expectedSpentSP,
			nextSpentSP,
			expectedSpentTP,
			nextSpentTP,
		)
	}
	return nil
}

func validateCharacterStatMap(values map[string]int64) error {
	for key := range values {
		if strings.TrimSpace(key) == "" || key == characterExperienceStatKey {
			return fmt.Errorf("%w: invalid character stat key %q", ErrCommitPlanInvalid, key)
		}
	}
	return nil
}

func characterStatsMatch(current map[string]int64, expected map[string]int64) bool {
	for key, value := range expected {
		if current[key] != value {
			return false
		}
	}
	return true
}

func validateCommitState(state CommitState) error {
	if state.Level <= 0 || state.Level > MaximumCharacterLevel {
		return fmt.Errorf("level=%d outside current DNF90 range 1..%d", state.Level, MaximumCharacterLevel)
	}
	points := state.SkillPoints
	if points.SyncedLevel != state.Level {
		return fmt.Errorf("level=%d synced_level=%d", state.Level, points.SyncedLevel)
	}
	if points.TotalSP < 0 || points.TotalSP > math.MaxUint16 ||
		points.RemainingSP < 0 || points.RemainingSP > points.TotalSP ||
		points.TotalTP < 0 || points.TotalTP > math.MaxUint16 ||
		points.RemainingTP < 0 || points.RemainingTP > points.TotalTP {
		return fmt.Errorf("SP/TP ledger outside current EXE u16 range: %+v", points)
	}
	return nil
}

func persistedCommitState(character dnfrepo.CharacterRecord, skill dnfrepo.SkillRecord) (CommitState, error) {
	experience, ok := character.Stats[characterExperienceStatKey]
	if !ok {
		return CommitState{}, fmt.Errorf("%w: character=%s stat=%s", ErrCommitStateMissing, character.CharacterID, characterExperienceStatKey)
	}
	if experience < 0 || uint64(experience) > math.MaxUint32 {
		return CommitState{}, fmt.Errorf(
			"%w: character=%s experience=%d outside current EXE u32 range",
			ErrCommitStateInvalid,
			character.CharacterID,
			experience,
		)
	}
	state := CommitState{
		Level:       character.Level,
		Experience:  uint32(experience),
		SkillPoints: skill.Points,
	}
	if err := validateCommitState(state); err != nil {
		return CommitState{}, fmt.Errorf("%w: character=%s: %v", ErrCommitStateInvalid, character.CharacterID, err)
	}
	return state, nil
}

func commitStatesEqual(left, right CommitState) bool {
	return left.Level == right.Level &&
		left.Experience == right.Experience &&
		left.SkillPoints == right.SkillPoints
}
