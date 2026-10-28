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

func (s *Service) alignedQuestCompletionTicketPlannerForCommand(opcode dnfenum.CmdPacket) (alignedcmd.QuestCompletionTicketPlanner, error) {
	if opcode != dnfenum.CmdPacketUseStackable && opcode != dnfenum.CmdPacketUseStackableAction &&
		opcode != dnfenum.CmdPacketRequestEventServerLevelUp {
		return nil, nil
	}
	catalog, err := s.loadQuestCatalog(context.Background())
	if err != nil {
		return nil, err
	}
	return func(
		character dnfrepo.CharacterRecord,
		record dnfrepo.QuestRecord,
		ticket alignedcmd.QuestCompletionTicketResolution,
		completedAt time.Time,
	) (alignedcmd.QuestCompletionTicketPlan, error) {
		job, valid := characterJobByte(character)
		if !valid {
			return alignedcmd.QuestCompletionTicketPlan{}, fmt.Errorf("quest completion ticket character job %q invalid", character.Job)
		}
		plan, planErr := catalog.PlanCompletionTicket(
			record,
			character,
			dnfquest.CharacterEligibility{
				Level:    character.Level,
				Job:      int(job),
				GrowType: int(numericCharacterStatValue(character, "grow_type")),
			},
			dnfquest.CompletionTicketSpec{
				ActionType:               ticket.ActionType,
				ActionValues:             append([]int64(nil), ticket.ActionValues...),
				MinimumLevel:             ticket.MinimumLevel,
				MaximumLevel:             ticket.MaximumLevel,
				PreserveSideQuests:       ticket.PreserveSideQuests,
				PreserveHellPartyUnlocks: ticket.PreserveHellPartyUnlocks,
			},
			completedAt,
		)
		if planErr != nil {
			return alignedcmd.QuestCompletionTicketPlan{}, planErr
		}
		return alignedcmd.QuestCompletionTicketPlan{
			Quest:             plan.Quest,
			Character:         plan.Character,
			QuestFields:       append([]dnfrepo.QuestField(nil), plan.QuestFields...),
			CharacterFields:   append([]dnfrepo.CharacterField(nil), plan.CharacterFields...),
			CompletedQuestIDs: append([]int64(nil), plan.CompletedQuestIDs...),
			ProfessionChanged: plan.ProfessionChanged,
		}, nil
	}, nil
}
