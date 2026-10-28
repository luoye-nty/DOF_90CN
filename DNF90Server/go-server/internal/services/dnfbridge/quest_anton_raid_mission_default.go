package dnfbridge

import (
	"context"
	"strings"
	"time"

	dnfquest "longheng.io/server/internal/modules/dnf/quest"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

// reconcileAntonRaidMissionDefaultCompletion retires the legacy "[Anton Raid]
// Voracious Giant" team mission (12830) once the Anton access quest (12829) is
// complete. The 90CN client gates the Luke raid channel quest chain on that
// five-day team mission, which the local single-player Anton flow never
// drives. The mission's selectable equipment reward is not granted. The
// returned record is the persisted projection the caller should keep using.
func (s *Service) reconcileAntonRaidMissionDefaultCompletion(
	ctx context.Context,
	session *gameSession,
	repos dnfrepo.Group,
	characterID string,
	record dnfrepo.QuestRecord,
) (dnfrepo.QuestRecord, bool, error) {
	plan := dnfquest.PlanAntonRaidMissionDefaultCompletion(record, time.Now().UTC())
	if len(plan.ChangedFields) == 0 {
		return record, false, nil
	}
	if repos.Quest == nil {
		return record, false, dnfrepo.ErrRepoMissing
	}
	if strings.TrimSpace(plan.Record.CharacterID) == "" {
		plan.Record.CharacterID = characterID
	}
	if err := dnfrepo.SaveQuestFields(ctx, repos.Quest, plan.Record, plan.ChangedFields...); err != nil {
		return record, false, err
	}
	if session != nil {
		s.logGameEvent(session, "game-anton-raid-mission-default-completed",
			"char_id", session.selectedCharacterID,
			"character_id", plan.Record.CharacterID,
			"quest_id", plan.QuestID,
			"prerequisite_quest_id", dnfquest.AntonRaidAccessPrerequisiteQuestID,
			"reward_granted", false,
			"changed_fields", plan.ChangedFields)
	}
	return plan.Record, true, nil
}
