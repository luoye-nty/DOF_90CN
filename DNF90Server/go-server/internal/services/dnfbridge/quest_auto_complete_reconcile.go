package dnfbridge

import (
	"context"
	"strconv"
	"time"

	dnfquest "longheng.io/server/internal/modules/dnf/quest"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

// repairAutoCompletedQuestionQuests reopens PVF [question] quests that an
// older task-manual bulk completion marked completed without a player answer.
// Such a row makes every answer-gated successor unreachable; removing it lets
// the character accept the question and choose a branch normally.
func (s *Service) repairAutoCompletedQuestionQuests(
	ctx context.Context,
	session *gameSession,
	source string,
) (bool, error) {
	if s == nil || session == nil || session.selectedCharacterID == 0 {
		return false, nil
	}
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.Character == nil || repositories.Quest == nil {
		return false, nil
	}
	characterID := strconv.FormatUint(uint64(session.selectedCharacterID), 10)
	if _, found, err := repositories.Quest.Load(ctx, characterID); err != nil {
		return false, err
	} else if !found {
		return false, nil
	}
	catalog, err := s.loadQuestCatalog(ctx)
	if err != nil {
		return false, err
	}
	owner, err := dnfquest.NewOwner(repositories)
	if err != nil {
		return false, err
	}
	result, err := owner.ApplyAutoCompleteQuestionRepair(ctx, catalog, characterID, time.Now().UTC())
	if err != nil {
		return false, err
	}
	if result.Idempotent {
		return false, nil
	}
	s.logGameEvent(session, "game-auto-completed-question-quests-repaired",
		"char_id", session.selectedCharacterID,
		"source", source,
		"quest_ids", result.RepairedQuestIDs,
		"repair_source", "task_manual_no_reward_completion_without_player_answer")
	return true, nil
}

func (s *Service) reconcileAutoCompletedQuestClearLinkedSubQuests(
	ctx context.Context,
	session *gameSession,
	source string,
) (bool, error) {
	if s == nil || session == nil || session.selectedCharacterID == 0 {
		return false, nil
	}
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.Character == nil || repositories.Quest == nil {
		return false, nil
	}
	characterID := strconv.FormatUint(uint64(session.selectedCharacterID), 10)
	if _, found, err := repositories.Quest.Load(ctx, characterID); err != nil {
		return false, err
	} else if !found {
		return false, nil
	}
	catalog, err := s.loadQuestCatalog(ctx)
	if err != nil {
		return false, err
	}
	owner, err := dnfquest.NewOwner(repositories)
	if err != nil {
		return false, err
	}
	result, err := owner.ApplyAutoCompletedLinkedSubQuestReconciliation(ctx, catalog, characterID, time.Now().UTC())
	if err != nil {
		return false, err
	}
	questIDs := make([]int64, 0, len(result.Completions))
	parentIDs := make([]int64, 0, len(result.Completions))
	synthesized := make([]int64, 0, len(result.Completions))
	for _, completion := range result.Completions {
		questIDs = append(questIDs, completion.QuestID)
		parentIDs = append(parentIDs, completion.ParentID)
		if completion.Synthesized {
			synthesized = append(synthesized, completion.QuestID)
		}
	}
	s.logGameEvent(session, "game-auto-completed-quest-clear-linked-subquests-reconciled",
		"char_id", session.selectedCharacterID,
		"source", source,
		"completion_count", len(result.Completions),
		"quest_ids", questIDs,
		"parent_ids", parentIDs,
		"synthesized_quest_ids", synthesized,
		"changed_fields", append([]dnfrepo.QuestField(nil), result.ChangedFields...),
		"idempotent", result.Idempotent,
		"quest_mutation", "op1429_completed_parent_exact_pvf_no_reward_child_closure")
	return len(result.Completions) > 0, nil
}
