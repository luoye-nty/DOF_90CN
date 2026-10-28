package skill

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

var (
	ErrSceneOwnerUnavailable = errors.New("scene skill owner is unavailable")
	ErrCharacterRequired     = errors.New("scene skill character id is required")
	ErrSkillRecordMissing    = errors.New("scene skill record is missing")
	ErrSeedInvalid           = errors.New("scene skill seed is invalid")
	ErrLayoutBuilderRequired = errors.New("scene skill layout builder is required")
)

type BackfillCommand struct {
	CharacterID string
	Seed        dnfrepo.SkillRecord
	UpdatedAt   time.Time
}

type SyncPointsCommand struct {
	CharacterID string
	TreeIndex   int
	Target      dnfrepo.SkillPointState
	UpdatedAt   time.Time
}

type EnsureTreeCommand struct {
	CharacterID string
	TreeIndex   int
	State       dnfrepo.SkillTreeState
	Layout      dnfrepo.SkillLayout
	UpdatedAt   time.Time
}

type EnsureBaselineCommand struct {
	CharacterID string
	TreeCount   int
	Levels      map[uint16]int
	UpdatedAt   time.Time
}

type LayoutBuilder func(dnfrepo.SkillRecord) (dnfrepo.SkillLayout, error)

type EnsureLayoutCommand struct {
	CharacterID string
	TreeIndex   int
	UpdatedAt   time.Time
	Build       LayoutBuilder
}

// SceneOwner owns missing-record backfill, SP/TP ledger repair, and missing
// quick-layout persistence. Current-client protobuf projection and PVF layout
// selection remain outside this repository boundary.
type SceneOwner struct {
	skills dnfrepo.CharacterSkillUnitOfWork
}

func NewSceneOwner(repositories dnfrepo.Group) (*SceneOwner, error) {
	if repositories.CharacterSkills == nil {
		return nil, ErrSceneOwnerUnavailable
	}
	return &SceneOwner{skills: repositories.CharacterSkills}, nil
}

func (o *SceneOwner) Backfill(ctx context.Context, command BackfillCommand) (dnfrepo.SkillRecord, bool, error) {
	characterID := strings.TrimSpace(command.CharacterID)
	if characterID == "" {
		return dnfrepo.SkillRecord{}, false, ErrCharacterRequired
	}
	if strings.TrimSpace(command.Seed.CharacterID) != characterID || len(command.Seed.Skills) == 0 {
		return dnfrepo.SkillRecord{}, false, ErrSeedInvalid
	}
	if o == nil || o.skills == nil {
		return dnfrepo.SkillRecord{}, false, ErrSceneOwnerUnavailable
	}
	ctx = normalizedSceneContext(ctx)
	if err := ctx.Err(); err != nil {
		return dnfrepo.SkillRecord{}, false, err
	}
	seed := dnfrepo.CloneSkill(command.Seed)
	seed.UpdatedAt = normalizedSceneTime(command.UpdatedAt)
	selected := dnfrepo.SkillRecord{}
	persisted := false
	err := o.skills.WithinCharacterSkill(ctx, characterID, func(skills dnfrepo.SkillRepository) error {
		current, found, err := skills.Load(ctx, characterID)
		if err != nil {
			return err
		}
		if found && len(current.Skills) > 0 {
			selected = dnfrepo.CloneSkill(current)
			return nil
		}
		if found {
			seed.Cooldowns = current.Cooldowns
		}
		if err := dnfrepo.SaveSkillFields(
			ctx,
			skills,
			seed,
			dnfrepo.SkillFieldSkills,
			dnfrepo.SkillFieldPoints,
			dnfrepo.SkillFieldTrees,
			dnfrepo.SkillFieldLayouts,
		); err != nil {
			return err
		}
		selected = dnfrepo.CloneSkill(seed)
		persisted = true
		return nil
	})
	if err != nil {
		return dnfrepo.SkillRecord{}, false, err
	}
	if len(selected.Skills) == 0 {
		return dnfrepo.SkillRecord{}, false, ErrSkillRecordMissing
	}
	return selected, persisted, nil
}

func (o *SceneOwner) SyncPoints(ctx context.Context, command SyncPointsCommand) (dnfrepo.SkillRecord, bool, error) {
	characterID := strings.TrimSpace(command.CharacterID)
	if characterID == "" {
		return dnfrepo.SkillRecord{}, false, ErrCharacterRequired
	}
	if o == nil || o.skills == nil {
		return dnfrepo.SkillRecord{}, false, ErrSceneOwnerUnavailable
	}
	ctx = normalizedSceneContext(ctx)
	if err := ctx.Err(); err != nil {
		return dnfrepo.SkillRecord{}, false, err
	}
	selected := dnfrepo.SkillRecord{}
	persisted := false
	err := o.skills.WithinCharacterSkill(ctx, characterID, func(skills dnfrepo.SkillRepository) error {
		current, found, err := skills.Load(ctx, characterID)
		if err != nil {
			return err
		}
		if !found || len(current.Skills) == 0 {
			return fmt.Errorf("%w: character=%s", ErrSkillRecordMissing, characterID)
		}
		state, treeFound := dnfrepo.SkillTree(current, command.TreeIndex)
		if !treeFound {
			return fmt.Errorf("%w: character=%s tree=%d", ErrSkillRecordMissing, characterID, command.TreeIndex)
		}
		state.Points, persisted, err = SyncPointState(state.Points, command.Target)
		if err != nil {
			return err
		}
		if persisted {
			dnfrepo.SetSkillTree(&current, command.TreeIndex, state)
			current.UpdatedAt = normalizedSceneTime(command.UpdatedAt)
			field := dnfrepo.SkillFieldPoints
			if command.TreeIndex != 0 {
				field = dnfrepo.SkillFieldTrees
			}
			if err := dnfrepo.SaveSkillFields(ctx, skills, current, field); err != nil {
				return err
			}
		}
		selected = dnfrepo.CloneSkill(current)
		return nil
	})
	if err != nil {
		return dnfrepo.SkillRecord{}, false, err
	}
	return selected, persisted, nil
}

// EnsureTree creates only a missing alternate plan. Existing learned levels,
// point ledgers, and layouts are left untouched.
func (o *SceneOwner) EnsureTree(ctx context.Context, command EnsureTreeCommand) (dnfrepo.SkillRecord, bool, error) {
	characterID := strings.TrimSpace(command.CharacterID)
	if characterID == "" {
		return dnfrepo.SkillRecord{}, false, ErrCharacterRequired
	}
	if command.TreeIndex <= 0 || len(command.State.Skills) == 0 || len(command.Layout) == 0 {
		return dnfrepo.SkillRecord{}, false, ErrSeedInvalid
	}
	if o == nil || o.skills == nil {
		return dnfrepo.SkillRecord{}, false, ErrSceneOwnerUnavailable
	}
	ctx = normalizedSceneContext(ctx)
	if err := ctx.Err(); err != nil {
		return dnfrepo.SkillRecord{}, false, err
	}
	selected := dnfrepo.SkillRecord{}
	persisted := false
	err := o.skills.WithinCharacterSkill(ctx, characterID, func(skills dnfrepo.SkillRepository) error {
		current, found, err := skills.Load(ctx, characterID)
		if err != nil {
			return err
		}
		if !found || len(current.Skills) == 0 {
			return fmt.Errorf("%w: character=%s", ErrSkillRecordMissing, characterID)
		}
		if _, exists := dnfrepo.SkillTree(current, command.TreeIndex); exists {
			selected = dnfrepo.CloneSkill(current)
			return nil
		}
		current = dnfrepo.CloneSkill(current)
		dnfrepo.SetSkillTree(&current, command.TreeIndex, command.State)
		if current.Layouts == nil {
			current.Layouts = make(map[int]dnfrepo.SkillLayout)
		}
		current.Layouts[command.TreeIndex] = cloneSceneLayout(command.Layout)
		current.UpdatedAt = normalizedSceneTime(command.UpdatedAt)
		if err := dnfrepo.SaveSkillFields(ctx, skills, current, dnfrepo.SkillFieldTrees, dnfrepo.SkillFieldLayouts); err != nil {
			return err
		}
		selected = dnfrepo.CloneSkill(current)
		persisted = true
		return nil
	})
	if err != nil {
		return dnfrepo.SkillRecord{}, false, err
	}
	return selected, persisted, nil
}

// EnsureBaseline idempotently restores PVF-owned free skills in every current
// EXE skill plan. It only raises missing/lower free levels and never spends or
// refunds SP/TP, removes learned skills, or changes quick-slot layouts.
func (o *SceneOwner) EnsureBaseline(ctx context.Context, command EnsureBaselineCommand) (dnfrepo.SkillRecord, bool, error) {
	characterID := strings.TrimSpace(command.CharacterID)
	if characterID == "" {
		return dnfrepo.SkillRecord{}, false, ErrCharacterRequired
	}
	if command.TreeCount <= 0 || len(command.Levels) == 0 {
		return dnfrepo.SkillRecord{}, false, ErrSeedInvalid
	}
	for skillID, level := range command.Levels {
		if skillID == 0 || level <= 0 {
			return dnfrepo.SkillRecord{}, false, ErrSeedInvalid
		}
	}
	if o == nil || o.skills == nil {
		return dnfrepo.SkillRecord{}, false, ErrSceneOwnerUnavailable
	}
	ctx = normalizedSceneContext(ctx)
	if err := ctx.Err(); err != nil {
		return dnfrepo.SkillRecord{}, false, err
	}
	selected := dnfrepo.SkillRecord{}
	persisted := false
	err := o.skills.WithinCharacterSkill(ctx, characterID, func(skills dnfrepo.SkillRepository) error {
		current, found, err := skills.Load(ctx, characterID)
		if err != nil {
			return err
		}
		if !found || len(current.Skills) == 0 {
			return fmt.Errorf("%w: character=%s", ErrSkillRecordMissing, characterID)
		}
		current = dnfrepo.CloneSkill(current)
		for tree := 0; tree < command.TreeCount; tree++ {
			state, treeFound := dnfrepo.SkillTree(current, tree)
			if !treeFound {
				return fmt.Errorf("%w: character=%s tree=%d", ErrSkillRecordMissing, characterID, tree)
			}
			if state.Skills == nil {
				state.Skills = make(map[int64]dnfrepo.SkillState)
			}
			treeChanged := false
			for skillID, level := range command.Levels {
				stateEntry := state.Skills[int64(skillID)]
				if stateEntry.Enabled && stateEntry.Level >= level {
					continue
				}
				if stateEntry.Level < level {
					stateEntry.Level = level
				}
				stateEntry.Enabled = true
				state.Skills[int64(skillID)] = stateEntry
				treeChanged = true
			}
			if treeChanged {
				dnfrepo.SetSkillTree(&current, tree, state)
				persisted = true
			}
		}
		if persisted {
			current.UpdatedAt = normalizedSceneTime(command.UpdatedAt)
			if err := dnfrepo.SaveSkillFields(
				ctx,
				skills,
				current,
				dnfrepo.SkillFieldSkills,
				dnfrepo.SkillFieldTrees,
			); err != nil {
				return err
			}
		}
		selected = dnfrepo.CloneSkill(current)
		return nil
	})
	if err != nil {
		return dnfrepo.SkillRecord{}, false, err
	}
	return selected, persisted, nil
}

func (o *SceneOwner) EnsureLayout(ctx context.Context, command EnsureLayoutCommand) (dnfrepo.SkillRecord, dnfrepo.SkillLayout, bool, error) {
	characterID := strings.TrimSpace(command.CharacterID)
	if characterID == "" {
		return dnfrepo.SkillRecord{}, nil, false, ErrCharacterRequired
	}
	if command.Build == nil {
		return dnfrepo.SkillRecord{}, nil, false, ErrLayoutBuilderRequired
	}
	if o == nil || o.skills == nil {
		return dnfrepo.SkillRecord{}, nil, false, ErrSceneOwnerUnavailable
	}
	ctx = normalizedSceneContext(ctx)
	if err := ctx.Err(); err != nil {
		return dnfrepo.SkillRecord{}, nil, false, err
	}
	selected := dnfrepo.SkillRecord{}
	var selectedLayout dnfrepo.SkillLayout
	persisted := false
	err := o.skills.WithinCharacterSkill(ctx, characterID, func(skills dnfrepo.SkillRepository) error {
		current, found, err := skills.Load(ctx, characterID)
		if err != nil {
			return err
		}
		if !found || len(current.Skills) == 0 {
			return fmt.Errorf("%w: character=%s", ErrSkillRecordMissing, characterID)
		}
		layout, err := command.Build(dnfrepo.CloneSkill(current))
		if err != nil {
			return err
		}
		selected = dnfrepo.CloneSkill(current)
		selectedLayout = cloneSceneLayout(layout)
		if existing := current.Layouts[command.TreeIndex]; reflect.DeepEqual(existing, layout) {
			return nil
		}
		if current.Layouts == nil {
			current.Layouts = make(map[int]dnfrepo.SkillLayout)
		}
		current.Layouts[command.TreeIndex] = cloneSceneLayout(layout)
		current.UpdatedAt = normalizedSceneTime(command.UpdatedAt)
		if err := dnfrepo.SaveSkillFields(ctx, skills, current, dnfrepo.SkillFieldLayouts); err != nil {
			return err
		}
		selected = dnfrepo.CloneSkill(current)
		persisted = true
		return nil
	})
	if err != nil {
		return dnfrepo.SkillRecord{}, nil, false, err
	}
	return selected, selectedLayout, persisted, nil
}

func SyncPointState(current dnfrepo.SkillPointState, target dnfrepo.SkillPointState) (dnfrepo.SkillPointState, bool, error) {
	if current.TotalSP < 0 || current.RemainingSP < 0 || current.RemainingSP > current.TotalSP ||
		current.TotalTP < 0 || current.RemainingTP < 0 || current.RemainingTP > current.TotalTP ||
		target.TotalSP < 0 || target.TotalTP < 0 || target.SyncedLevel <= 0 {
		return dnfrepo.SkillPointState{}, false, fmt.Errorf(
			"invalid SP/TP ledger sync current=%+v target=%+v",
			current,
			target,
		)
	}
	spentSP := current.TotalSP - current.RemainingSP
	spentTP := current.TotalTP - current.RemainingTP
	next := target
	next.RemainingSP = next.TotalSP - spentSP
	if next.RemainingSP < 0 {
		next.RemainingSP = 0
	}
	next.RemainingTP = next.TotalTP - spentTP
	if next.RemainingTP < 0 {
		next.RemainingTP = 0
	}
	return next, next != current, nil
}

func normalizedSceneContext(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return ctx
}

func normalizedSceneTime(value time.Time) time.Time {
	value = value.UTC()
	if value.IsZero() {
		return time.Now().UTC()
	}
	return value
}

func cloneSceneLayout(layout dnfrepo.SkillLayout) dnfrepo.SkillLayout {
	if layout == nil {
		return nil
	}
	cloned := make(dnfrepo.SkillLayout, len(layout))
	for slot, skillID := range layout {
		cloned[slot] = skillID
	}
	return cloned
}
