package dnfbridge

import (
	"fmt"

	"longheng.io/server/internal/modules/dnf/dnfenum"
)

const currentDungeonPracticeSummaryMsgID = uint16(dnfenum.CmdPacketDieBloodMonster)

var currentDungeonPracticeSummaryBody = []byte{0, 0, 0, 0, 0}

func (s *Service) produceCurrentPracticeDungeonSettlementPlanLocked(
	session *gameSession,
	runtime *runtimeDungeonState,
) error {
	if session == nil || runtime == nil || runtime.Session == nil ||
		session.dungeon.runtime != runtime || !currentDungeonRuntimeIsPractice(runtime) ||
		!runtime.settlementPlayResultReceived {
		return errCurrentDungeonSettlementOwnerUnavailable
	}
	if runtime.settlementResultPlan != nil {
		return nil
	}
	s.initialEquipmentMu.Lock()
	archive, err := s.initialEquipmentArchiveLocked()
	s.initialEquipmentMu.Unlock()
	if err != nil {
		return err
	}
	ranks, err := loadCurrentDungeonClearRankCatalog(archive)
	if err != nil {
		return err
	}
	clientRankPoint := runtime.settlementPlayResultBody[currentDungeonPlayResultClientRankPointOffset]
	presentation, err := currentDungeonSettlementPresentationForRuntime(runtime, ranks, clientRankPoint)
	if err != nil {
		return err
	}
	op34, err := buildCurrentDungeonPlayResultNoticeBody(currentDungeonPlayResultNotice{
		RankGrade: presentation.RankGrade, ClearTimeMS: presentation.ClearTimeMS,
		TimeBonusPoint: presentation.TimeBonusPoint, RankPoint: clientRankPoint,
		AllVisitedClear: presentation.AllVisitedClear,
		Participants: []currentDungeonPlayResultParticipant{{
			ObjectKey: session.selectedCharacterID, ClearTimeMS: presentation.ClearTimeMS,
		}},
	})
	if err != nil {
		return err
	}
	op35, err := buildCurrentPracticeDungeonClearRewardBody(session.selectedCharacterID)
	if err != nil {
		return err
	}
	runtime.settlementResultPlan = &currentDungeonSettlementPacketPlan{
		CharacterID:     session.selectedCharacterID,
		CompletionKey:   fmt.Sprintf("practice:run:%d", runtime.lifecycleToken),
		Source:          "practice_session_local_zero_reward",
		ClientRankPoint: clientRankPoint,
		ClearTimeMS:     presentation.ClearTimeMS,
		PlayResultBody:  op34,
		ClearRewardBody: op35,
	}
	return nil
}

func (s *Service) sendCurrentPracticeDungeonSettlementResultsLocked(
	session *gameSession,
	runtime *runtimeDungeonState,
	source string,
) error {
	if session == nil || runtime == nil || session.dungeon.runtime != runtime ||
		!currentDungeonRuntimeIsPractice(runtime) || runtime.settlementResultPlan == nil {
		return nil
	}
	plan := runtime.settlementResultPlan
	if len(plan.PlayResultBody) == 0 || len(plan.ClearRewardBody) != currentDungeonClearRewardMinimumBodySize {
		return errCurrentDungeonSettlementPlanShape
	}
	if err := s.sendGameUpperRawClass(session, currentDungeonPlayResultNoticeMsgID, plan.PlayResultBody, 0); err != nil {
		return err
	}
	if err := s.sendGameUpperRawClass(session, currentDungeonClearRewardMsgID, plan.ClearRewardBody, 0); err != nil {
		return err
	}
	if err := s.sendGameUpperRawClass(session, currentDungeonPracticeSummaryMsgID, currentDungeonPracticeSummaryBody, 0); err != nil {
		return err
	}
	runtime.settlementResultNoticeSent = true
	runtime.settlementClearRewardSent = true
	runtime.settlementCharacterStateSent = false
	runtime.advanceSettlementPhase(currentDungeonSettlementPhaseResultShown)
	s.logGameEvent(session, "game-dungeon-practice-settlement-results-sent",
		"char_id", session.selectedCharacterID,
		"dungeon_id", runtime.Dungeon.ID,
		"source", source,
		"packet_order", "op34_op35_op255",
		"op255_body_len", len(currentDungeonPracticeSummaryBody),
		"persistent_reward_mutation", false)
	return nil
}
