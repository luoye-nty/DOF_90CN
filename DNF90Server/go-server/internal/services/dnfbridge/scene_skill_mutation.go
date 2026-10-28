package dnfbridge

import (
	"context"
	"fmt"
	"strings"
	"time"

	dnfprofession "longheng.io/server/internal/modules/dnf/profession"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfskill "longheng.io/server/internal/modules/dnf/skill"
	"longheng.io/server/internal/modules/dnf/skillcmd"
)

func currentSceneSkillOwner(repositories dnfrepo.Group) (*dnfskill.SceneOwner, error) {
	return dnfskill.NewSceneOwner(repositories)
}

func syncCurrentSkillPointState(
	current dnfrepo.SkillPointState,
	target dnfrepo.SkillPointState,
) (dnfrepo.SkillPointState, bool, error) {
	return dnfskill.SyncPointState(current, target)
}

func (s *Service) loadOrBackfillCurrentSceneSkillRecord(ctx context.Context, repos dnfrepo.Group, character dnfrepo.CharacterRecord, job byte) (dnfrepo.SkillRecord, bool, error) {
	record, found, err := repos.Skill.Load(ctx, character.CharacterID)
	if err != nil {
		return dnfrepo.SkillRecord{}, false, err
	}
	if found && len(record.Skills) > 0 {
		record, _, err = s.ensureCurrentSceneAlternateSkillTree(ctx, repos, character, job, record)
		if err != nil {
			return dnfrepo.SkillRecord{}, false, err
		}
		record, _, err = s.ensureCurrentSceneProfessionBaseline(ctx, repos, character, job, record)
		if err != nil {
			return dnfrepo.SkillRecord{}, false, err
		}
		record, _, err = s.syncCurrentSceneSkillPointLedgers(ctx, repos, character, record)
		return record, false, err
	}
	initial, err := s.initialCharacterSkills(ctx, job)
	if err != nil {
		return dnfrepo.SkillRecord{}, false, fmt.Errorf("load job %d initial skills from PVF: %w", job, err)
	}
	if len(initial) == 0 {
		return dnfrepo.SkillRecord{}, false, fmt.Errorf("job %d has no PVF initial skills", job)
	}
	points, err := s.initialSkillPoints(ctx, character.Level)
	if err != nil {
		return dnfrepo.SkillRecord{}, false, fmt.Errorf("load level %d initial skill points from PVF: %w", character.Level, err)
	}
	s.initialSkillsMu.Lock()
	catalog := s.skillCatalog
	s.initialSkillsMu.Unlock()
	if catalog == nil {
		return dnfrepo.SkillRecord{}, false, fmt.Errorf("job %d initial skill catalog is unavailable", job)
	}

	seed := initialSkillRecord(character, characterPVFInitialization{
		Job:         job,
		Level:       character.Level,
		Skills:      initial,
		SkillPoints: points,
	}, time.Now().UTC())
	layout, err := skillcmd.BuildInitialSkillLayout(catalog, job, currentSkillInfoTreeIndex, seed.Skills)
	if err != nil {
		return dnfrepo.SkillRecord{}, false, fmt.Errorf("build job %d initial skill layout: %w", job, err)
	}
	alternateLayout, err := skillcmd.BuildInitialSkillLayout(catalog, job, 1, seed.Skills)
	if err != nil {
		return dnfrepo.SkillRecord{}, false, fmt.Errorf("build job %d alternate initial skill layout: %w", job, err)
	}
	seed.Layouts = map[int]dnfrepo.SkillLayout{currentSkillInfoTreeIndex: layout, 1: alternateLayout}
	dnfrepo.SetSkillTree(&seed, 1, dnfrepo.SkillTreeState{Skills: seed.Skills, Points: seed.Points})

	owner, err := currentSceneSkillOwner(repos)
	if err != nil {
		return dnfrepo.SkillRecord{}, false, err
	}
	return owner.Backfill(ctx, dnfskill.BackfillCommand{
		CharacterID: character.CharacterID,
		Seed:        seed,
		UpdatedAt:   time.Now().UTC(),
	})
}

func (s *Service) ensureCurrentSceneProfessionBaseline(
	ctx context.Context,
	repos dnfrepo.Group,
	character dnfrepo.CharacterRecord,
	job byte,
	record dnfrepo.SkillRecord,
) (dnfrepo.SkillRecord, bool, error) {
	// Focused bridge fixtures do not own a runtime PVF. Production services
	// always have a configured PVF path and preload these resources at startup.
	s.professionMu.Lock()
	hasInjectedProfiles := s.professionProfiles != nil || s.professionProfilesErr != nil
	s.professionMu.Unlock()
	if !hasInjectedProfiles && strings.TrimSpace(s.options.pvfPath) == "" {
		return record, false, nil
	}
	profiles, catalog, err := s.currentProfessionResources(ctx)
	if err != nil {
		return dnfrepo.SkillRecord{}, false, err
	}
	growTypeValue := numericCharacterStatValue(character, "grow_type")
	if growTypeValue < 0 || growTypeValue > 0xff {
		return dnfrepo.SkillRecord{}, false, fmt.Errorf("invalid profession grow_type=%d", growTypeValue)
	}
	levels, ok := protectedProfessionSkillLevels(profiles, catalog, job, byte(growTypeValue), character.Level)
	if !ok {
		return dnfrepo.SkillRecord{}, false, fmt.Errorf("%w: job=%d grow_type=%d", dnfprofession.ErrProfileMissing, job, growTypeValue)
	}
	owner, err := currentSceneSkillOwner(repos)
	if err != nil {
		return dnfrepo.SkillRecord{}, false, err
	}
	selected, changed, err := owner.EnsureBaseline(ctx, dnfskill.EnsureBaselineCommand{
		CharacterID: character.CharacterID,
		TreeCount:   currentSkillInfoTreeCount,
		Levels:      levels,
		UpdatedAt:   time.Now().UTC(),
	})
	if err != nil {
		return dnfrepo.SkillRecord{}, false, err
	}
	if changed {
		s.logInfo(
			"dnfbridge profession free skill baseline restored",
			"character_id", character.CharacterID,
			"job", job,
			"grow_type", growTypeValue,
			"grant_count", len(levels),
		)
	}
	return selected, changed, nil
}

func (s *Service) ensureCurrentSceneAlternateSkillTree(
	ctx context.Context,
	repos dnfrepo.Group,
	character dnfrepo.CharacterRecord,
	job byte,
	record dnfrepo.SkillRecord,
) (dnfrepo.SkillRecord, bool, error) {
	if _, found := dnfrepo.SkillTree(record, 1); found {
		return record, false, nil
	}
	initial, err := s.initialCharacterSkills(ctx, job)
	if err != nil {
		return dnfrepo.SkillRecord{}, false, fmt.Errorf("load job %d alternate initial skills from PVF: %w", job, err)
	}
	points, err := s.currentPVFSkillPointTarget(ctx, character)
	if err != nil {
		return dnfrepo.SkillRecord{}, false, fmt.Errorf("load level %d alternate initial skill points from PVF: %w", character.Level, err)
	}
	seed := initialSkillRecord(character, characterPVFInitialization{
		Job:         job,
		Level:       character.Level,
		Skills:      initial,
		SkillPoints: initialSkillPointState(points),
	}, time.Now().UTC())
	s.initialSkillsMu.Lock()
	catalog := s.skillCatalog
	s.initialSkillsMu.Unlock()
	if catalog == nil {
		return dnfrepo.SkillRecord{}, false, fmt.Errorf("job %d alternate initial skill catalog is unavailable", job)
	}
	layout, err := skillcmd.BuildInitialSkillLayout(catalog, job, 1, seed.Skills)
	if err != nil {
		return dnfrepo.SkillRecord{}, false, fmt.Errorf("build job %d alternate initial skill layout: %w", job, err)
	}
	owner, err := currentSceneSkillOwner(repos)
	if err != nil {
		return dnfrepo.SkillRecord{}, false, err
	}
	return owner.EnsureTree(ctx, dnfskill.EnsureTreeCommand{
		CharacterID: character.CharacterID,
		TreeIndex:   1,
		State:       dnfrepo.SkillTreeState{Skills: seed.Skills, Points: seed.Points},
		Layout:      layout,
		UpdatedAt:   time.Now().UTC(),
	})
}

// syncCurrentSceneSkillPointLedgers is the login/reselect repair for characters
// whose level changed outside the ordinary EXP owner or whose historical
// ledger predates TP advancement. The target totals come from the runtime PVF
// SP/TP tables plus persisted bonus points. Remaining points preserve the
// already-spent amount and clamp exactly as the 86JP LoadAndSync domain rule.
func (s *Service) syncCurrentSceneSkillPointLedgers(
	ctx context.Context,
	repos dnfrepo.Group,
	character dnfrepo.CharacterRecord,
	record dnfrepo.SkillRecord,
) (dnfrepo.SkillRecord, bool, error) {
	target, err := s.currentPVFSkillPointTarget(ctx, character)
	if err != nil {
		return dnfrepo.SkillRecord{}, false, err
	}
	owner, err := currentSceneSkillOwner(repos)
	if err != nil {
		return dnfrepo.SkillRecord{}, false, err
	}
	changedAny := false
	for tree := 0; tree < currentSkillInfoTreeCount; tree++ {
		state, found := dnfrepo.SkillTree(record, tree)
		if !found {
			return dnfrepo.SkillRecord{}, false, fmt.Errorf("character %s skill tree %d is missing", character.CharacterID, tree)
		}
		_, changed, err := syncCurrentSkillPointState(state.Points, target)
		if err != nil {
			return dnfrepo.SkillRecord{}, false, err
		}
		if !changed {
			continue
		}
		record, _, err = owner.SyncPoints(ctx, dnfskill.SyncPointsCommand{
			CharacterID: character.CharacterID,
			TreeIndex:   tree,
			Target:      target,
			UpdatedAt:   time.Now().UTC(),
		})
		if err != nil {
			return dnfrepo.SkillRecord{}, false, err
		}
		changedAny = true
	}
	return record, changedAny, nil
}

func (s *Service) ensureCurrentSceneSkillLayout(ctx context.Context, repos dnfrepo.Group, character dnfrepo.CharacterRecord, job byte, tree int, catalog *dnfskill.Table, record dnfrepo.SkillRecord) (dnfrepo.SkillRecord, dnfrepo.SkillLayout, bool, error) {
	_, found := dnfrepo.SkillTree(record, tree)
	if !found {
		return dnfrepo.SkillRecord{}, nil, false, fmt.Errorf("character %s skill tree %d is missing", character.CharacterID, tree)
	}
	var initial []initialSkillEntry
	if len(record.Layouts[tree]) == 0 {
		var err error
		initial, err = s.initialCharacterSkills(ctx, job)
		if err != nil {
			return dnfrepo.SkillRecord{}, nil, false, fmt.Errorf("load job %d initial skills for layout: %w", job, err)
		}
	}
	build := func(current dnfrepo.SkillRecord) (dnfrepo.SkillLayout, error) {
		currentState, ok := dnfrepo.SkillTree(current, tree)
		if !ok {
			return nil, fmt.Errorf("character %s skill tree %d is missing", character.CharacterID, tree)
		}
		if existing := current.Layouts[tree]; len(existing) > 0 {
			return skillcmd.BuildCurrentSkillLayout(catalog, job, tree, currentState.Skills, existing)
		}
		if skillRecordMatchesPVFInitialSkills(currentState.Skills, initial) {
			return skillcmd.BuildInitialSkillLayout(catalog, job, tree, currentState.Skills)
		}
		return skillcmd.BuildCurrentSkillLayout(catalog, job, tree, currentState.Skills, nil)
	}
	candidate, err := build(record)
	if err != nil {
		return dnfrepo.SkillRecord{}, nil, false, err
	}
	if repos.CharacterSkills == nil {
		return record, candidate, false, nil
	}
	owner, err := currentSceneSkillOwner(repos)
	if err != nil {
		return dnfrepo.SkillRecord{}, nil, false, err
	}
	return owner.EnsureLayout(ctx, dnfskill.EnsureLayoutCommand{
		CharacterID: character.CharacterID,
		TreeIndex:   tree,
		UpdatedAt:   time.Now().UTC(),
		Build:       build,
	})
}

func skillRecordMatchesPVFInitialSkills(states map[int64]dnfrepo.SkillState, initial []initialSkillEntry) bool {
	expected := make(map[int64]int, len(initial))
	for _, entry := range initial {
		if entry.SkillID > 0 && entry.Level > 0 {
			expected[entry.SkillID] = entry.Level
		}
	}
	if len(states) != len(expected) || len(expected) == 0 {
		return false
	}
	for skillID, level := range expected {
		state, ok := states[skillID]
		if !ok || !state.Enabled || state.Level != level {
			return false
		}
	}
	return true
}
