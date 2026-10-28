package dnfbridge

import (
	"context"
	"fmt"
	"time"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfquest "longheng.io/server/internal/modules/dnf/quest"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

// alignedStoryBookPlannerForCommand keeps the story quest interpretation in
// dnfbridge while inventory owns the item+quest transaction. The quest ID is
// supplied only by the current PVF item's [story book] action.
func (s *Service) alignedStoryBookPlannerForCommand(opcode dnfenum.CmdPacket) (alignedcmd.StoryBookPlanner, error) {
	if opcode != dnfenum.CmdPacketUseStackable {
		return nil, nil
	}
	catalog, err := s.loadQuestCatalog(context.Background())
	if err != nil {
		return nil, err
	}
	return func(
		character dnfrepo.CharacterRecord,
		record dnfrepo.QuestRecord,
		book alignedcmd.StoryBookResolution,
		acceptedAt time.Time,
	) (alignedcmd.StoryBookPlan, error) {
		job, valid := characterJobByte(character)
		if !valid {
			return alignedcmd.StoryBookPlan{}, fmt.Errorf("story-book character job %q invalid", character.Job)
		}
		plan, planErr := catalog.PlanAcceptMutation(
			dnfquest.CharacterEligibility{
				Level:    character.Level,
				Job:      int(job),
				GrowType: int(numericCharacterStatValue(character, "grow_type")),
			},
			record,
			book.QuestID,
			acceptedAt,
		)
		if planErr != nil {
			return alignedcmd.StoryBookPlan{}, planErr
		}
		return alignedcmd.StoryBookPlan{
			Quest:       plan.Quest,
			QuestFields: append([]dnfrepo.QuestField(nil), plan.QuestFields...),
			QuestID:     plan.QuestID,
			InitTrigger: plan.InitTrigger,
		}, nil
	}, nil
}
