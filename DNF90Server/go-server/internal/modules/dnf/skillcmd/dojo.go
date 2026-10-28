package skillcmd

import (
	"context"
	"maps"
	"time"

	repo "longheng.io/server/internal/modules/dnf/repository"
)

// Native dojo has its own two-page manager. Internal pages 2/3 keep its
// persisted skills separate; the native protocol still uses page numbers 0/1.
func (cmd Command) storageSkillTree() int {
	if cmd.DojoSkills {
		return int(cmd.SkillTree) + 2
	}
	return int(cmd.SkillTree)
}

func PrepareDojoSkills(ctx context.Context, repos repo.Group, characterID string) (repo.SkillRecord, error) {
	if repos.CharacterSkills == nil {
		return repo.SkillRecord{}, ErrOwnerUnavailable
	}
	var result repo.SkillRecord
	err := repos.CharacterSkills.WithinCharacterSkill(ctx, characterID, func(store repo.SkillRepository) error {
		record, found, err := store.Load(ctx, characterID)
		if err != nil {
			return err
		}
		if !found {
			return ErrSkillTreeUnavailable
		}
		record = repo.CloneSkill(record)
		changed := false
		for tree := 0; tree < 2; tree++ {
			if _, found := repo.SkillTree(record, tree+2); found {
				continue
			}
			state, found := repo.SkillTree(record, tree)
			if !found {
				return ErrSkillTreeUnavailable
			}
			repo.SetSkillTree(&record, tree+2, state)
			if record.Layouts == nil {
				record.Layouts = make(map[int]repo.SkillLayout)
			}
			record.Layouts[tree+2] = maps.Clone(record.Layouts[tree])
			changed = true
		}
		if changed {
			record.UpdatedAt = time.Now().UTC()
			if err := repo.SaveSkillFields(ctx, store, record, repo.SkillFieldTrees, repo.SkillFieldLayouts); err != nil {
				return err
			}
		}
		result = DojoSkillProjection(record)
		return nil
	})
	return result, err
}

func DojoSkillProjection(record repo.SkillRecord) repo.SkillRecord {
	result := repo.CloneSkill(record)
	for tree := 0; tree < 2; tree++ {
		state, found := repo.SkillTree(record, tree+2)
		if !found {
			continue
		}
		repo.SetSkillTree(&result, tree, state)
		if result.Layouts == nil {
			result.Layouts = make(map[int]repo.SkillLayout)
		}
		result.Layouts[tree] = maps.Clone(record.Layouts[tree+2])
	}
	return result
}
