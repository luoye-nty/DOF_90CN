package quest

import (
	"context"
	"sort"
	"strings"
	"time"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

// AutoCompleteQuestionRepairResult describes the narrow reconnect repair for
// question quests that an older task-manual bulk completion closed without a
// player-authored answer. Ordinary question completions and every non-question
// quest remain untouched.
type AutoCompleteQuestionRepairResult struct {
	CharacterID      string
	RepairedQuestIDs []int64
	Record           dnfrepo.QuestRecord
	Idempotent       bool
}

type autoCompleteQuestionRepairPlan struct {
	Record           dnfrepo.QuestRecord
	RepairedQuestIDs []int64
	ChangedFields    []dnfrepo.QuestField
}

// questionHasAnswerBranches reports whether any catalog quest gates itself on a
// selected answer of the given question. Only such questions carry a player
// choice worth restoring; a question without dependents has nothing to block.
func (c *Catalog) questionHasAnswerBranches(questID int64) bool {
	if c == nil || questID <= 0 {
		return false
	}
	for _, definition := range c.ordered {
		for _, requirement := range definition.PreRequiredAnswers {
			if requirement.QuestID == questID {
				return true
			}
		}
	}
	return false
}

func (c *Catalog) planAutoCompleteQuestionRepair(
	record dnfrepo.QuestRecord,
	repairedAt time.Time,
) (autoCompleteQuestionRepairPlan, error) {
	if c == nil {
		return autoCompleteQuestionRepairPlan{}, ErrCatalogEmpty
	}
	plan := autoCompleteQuestionRepairPlan{Record: dnfrepo.CloneQuest(record)}
	repair := func(field dnfrepo.QuestField, states map[int64]dnfrepo.QuestState) {
		for questID, state := range states {
			if !isCompletedQuestStatus(state.Status) ||
				strings.TrimSpace(state.Extra[questAnswerExtraKey]) != "" ||
				state.Extra["auto_completed"] != "true" ||
				state.Extra["auto_complete_reason"] != "epic_below_character_level_no_reward" {
				continue
			}
			definition, found := c.Find(questID)
			if !found || normalizeQuestTag(definition.Type) != "question" ||
				!c.questionHasAnswerBranches(questID) {
				continue
			}
			delete(states, questID)
			plan.RepairedQuestIDs = appendQuestIDOnce(plan.RepairedQuestIDs, questID)
			plan.ChangedFields = mergeQuestChangedField(plan.ChangedFields, field)
		}
	}
	repair(dnfrepo.QuestFieldStates, plan.Record.States)
	repair(dnfrepo.QuestFieldProgress, plan.Record.Progress)
	if len(plan.RepairedQuestIDs) > 0 {
		sort.Slice(plan.RepairedQuestIDs, func(left, right int) bool {
			return plan.RepairedQuestIDs[left] < plan.RepairedQuestIDs[right]
		})
		plan.Record.UpdatedAt = repairedAt.UTC()
	}
	return plan, nil
}

// ApplyAutoCompleteQuestionRepair reopens only malformed question rows created
// by the older no-reward bulk path. Removing the row makes the PVF question
// acceptable again; the ordinary accept/answer flow then records the player's
// own choice and unlocks exactly one dependent branch.
func (o *Owner) ApplyAutoCompleteQuestionRepair(
	ctx context.Context,
	catalog *Catalog,
	characterID string,
	repairedAt time.Time,
) (AutoCompleteQuestionRepairResult, error) {
	if o == nil || o.characters == nil || o.quests == nil || catalog == nil {
		return AutoCompleteQuestionRepairResult{}, ErrOwnerUnavailable
	}
	characterID = strings.TrimSpace(characterID)
	if characterID == "" {
		return AutoCompleteQuestionRepairResult{}, ErrCharacterRequired
	}
	if repairedAt.IsZero() {
		repairedAt = time.Now().UTC()
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if _, found, err := o.characters.Load(ctx, characterID); err != nil {
		return AutoCompleteQuestionRepairResult{}, err
	} else if !found {
		return AutoCompleteQuestionRepairResult{}, ErrCharacterNotFound
	}
	record, found, err := o.quests.Load(ctx, characterID)
	if err != nil {
		return AutoCompleteQuestionRepairResult{}, err
	}
	if !found {
		return AutoCompleteQuestionRepairResult{CharacterID: characterID, Idempotent: true}, nil
	}
	if strings.TrimSpace(record.CharacterID) == "" {
		record.CharacterID = characterID
	}
	plan, err := catalog.planAutoCompleteQuestionRepair(record, repairedAt)
	if err != nil {
		return AutoCompleteQuestionRepairResult{}, err
	}
	result := AutoCompleteQuestionRepairResult{
		CharacterID:      characterID,
		RepairedQuestIDs: append([]int64(nil), plan.RepairedQuestIDs...),
		Record:           dnfrepo.CloneQuest(plan.Record),
		Idempotent:       len(plan.RepairedQuestIDs) == 0,
	}
	if result.Idempotent {
		return result, nil
	}
	plan.Record.CharacterID = characterID
	if err := dnfrepo.SaveQuestFields(ctx, o.quests, plan.Record, plan.ChangedFields...); err != nil {
		return AutoCompleteQuestionRepairResult{}, err
	}
	persisted, found, err := o.quests.Load(ctx, characterID)
	if err != nil {
		return AutoCompleteQuestionRepairResult{}, err
	}
	if !found {
		return AutoCompleteQuestionRepairResult{}, ErrQuestPersistVerify
	}
	for _, questID := range result.RepairedQuestIDs {
		if _, stillPresent := questStateFor(persisted, questID); stillPresent {
			return AutoCompleteQuestionRepairResult{}, ErrQuestPersistVerify
		}
	}
	result.Record = dnfrepo.CloneQuest(persisted)
	return result, nil
}
