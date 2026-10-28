package quest

import (
	"context"
	"fmt"
	"strings"
	"time"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

type AutoCompletedLinkedSubQuestReconcileResult struct {
	CharacterID   string
	Completions   []AutoCompletedLinkedSubQuestCompletion
	ChangedFields []dnfrepo.QuestField
	PostCommit    dnfrepo.QuestRecord
	Idempotent    bool
}

func (o *Owner) ApplyAutoCompletedLinkedSubQuestReconciliation(
	ctx context.Context,
	catalog *Catalog,
	characterID string,
	reconciledAt time.Time,
) (AutoCompletedLinkedSubQuestReconcileResult, error) {
	if o == nil || o.characters == nil || o.quests == nil {
		return AutoCompletedLinkedSubQuestReconcileResult{}, ErrOwnerUnavailable
	}
	characterID = strings.TrimSpace(characterID)
	if characterID == "" {
		return AutoCompletedLinkedSubQuestReconcileResult{}, ErrCharacterRequired
	}
	if _, found, err := o.characters.Load(ctx, characterID); err != nil {
		return AutoCompletedLinkedSubQuestReconcileResult{}, err
	} else if !found {
		return AutoCompletedLinkedSubQuestReconcileResult{}, ErrCharacterNotFound
	}
	record, found, err := o.quests.Load(ctx, characterID)
	if err != nil {
		return AutoCompletedLinkedSubQuestReconcileResult{}, err
	}
	if !found {
		return AutoCompletedLinkedSubQuestReconcileResult{CharacterID: characterID, Idempotent: true}, nil
	}
	plan, err := catalog.PlanAutoCompletedLinkedSubQuestReconciliation(record, reconciledAt)
	if err != nil {
		return AutoCompletedLinkedSubQuestReconcileResult{}, err
	}
	result := AutoCompletedLinkedSubQuestReconcileResult{
		CharacterID:   characterID,
		Completions:   append([]AutoCompletedLinkedSubQuestCompletion(nil), plan.Completions...),
		ChangedFields: append([]dnfrepo.QuestField(nil), plan.ChangedFields...),
		PostCommit:    dnfrepo.CloneQuest(plan.Record),
		Idempotent:    len(plan.ChangedFields) == 0,
	}
	if len(plan.ChangedFields) == 0 {
		return result, nil
	}
	if err := dnfrepo.SaveQuestFields(ctx, o.quests, plan.Record, plan.ChangedFields...); err != nil {
		return AutoCompletedLinkedSubQuestReconcileResult{}, err
	}
	persisted, found, err := o.quests.Load(ctx, characterID)
	if err != nil {
		return AutoCompletedLinkedSubQuestReconcileResult{}, err
	}
	if !found {
		return AutoCompletedLinkedSubQuestReconcileResult{}, ErrQuestPersistVerify
	}
	for _, completion := range result.Completions {
		state, known := questStateFor(persisted, completion.QuestID)
		if !known || !isCompletedQuestStatus(state.Status) ||
			state.Extra["auto_completed_by_parent"] != fmt.Sprint(completion.ParentID) ||
			state.Extra["reward_state"] != finishRewardGranted {
			return AutoCompletedLinkedSubQuestReconcileResult{}, ErrQuestPersistVerify
		}
	}
	result.PostCommit = dnfrepo.CloneQuest(persisted)
	return result, nil
}
