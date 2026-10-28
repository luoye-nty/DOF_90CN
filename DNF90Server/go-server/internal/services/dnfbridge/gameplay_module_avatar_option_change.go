package dnfbridge

import (
	"context"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfproto "longheng.io/server/internal/modules/dnf/protocol"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

func avatarOptionChangeGameplayModule() gameplayModuleDefinition {
	opcode := uint16(dnfenum.CmdPacketAvatarOptionChange)
	return gameplayModuleDefinition{
		Name: "avatar-option-change",
		LegacyHandlers: map[uint16]gameplayHandler{
			opcode: func(service *Service, session *gameSession, request gameplayRequest) error {
				return service.handleCurrentAvatarOptionChange(session, request.Body)
			},
		},
		UpperHandlers: map[uint16]gameplayHandler{
			opcode: func(service *Service, session *gameSession, request gameplayRequest) error {
				if !requireDefaultGameplayClassification(service, session, request, "game-avatar-option-change-blocked", "current_exe_class1_op459_required") {
					return nil
				}
				return service.handleCurrentAvatarOptionChange(session, request.Body)
			},
		},
	}
}

func (s *Service) handleCurrentAvatarOptionChange(session *gameSession, body []byte) error {
	request, err := parseCurrentAvatarOptionChangeRequest(body)
	if err == nil && (session == nil || session.selectedCharacterID == 0) {
		err = errCurrentAvatarOptionChangeUnavailable
	}
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	if err == nil {
		var catalog *pvfDungeonDropCatalog
		catalog, err = s.currentPVFItemCatalog()
		if err == nil {
			_, err = s.commitCurrentAvatarOptionChange(ctx, session, catalog, request)
		}
	}
	resultOpcode := uint16(dnfenum.CmdPacketAvatarOptionChange)
	if err != nil {
		s.logGameEvent(session, "game-avatar-option-change-rejected",
			"source_slot", request.SourceSlot,
			"source_item", request.SourceItemID,
			"target_slot", request.TargetSlot,
			"target_item", request.TargetItemID,
			"ability_no", request.AbilityNo,
			"body_len", len(body),
			"reason", err)
		return s.sendGameUpperRawClass(session, resultOpcode, currentAvatarOptionChangeFailureBody(), dnfproto.DefaultChannelClassification)
	}

	// The current reader applies the avatar ability from the ACK itself.  It
	// only needs the consumed main-bag row to be incrementally refreshed; a
	// scene rebuild or full avatar-list replay would reintroduce stale paper-
	// doll state during an ordinary inventory action.
	if err := s.sendGameUpperRawClass(session, resultOpcode, buildCurrentAvatarOptionChangeSuccessBody(request), dnfproto.DefaultChannelClassification); err != nil {
		return err
	}
	if err := s.sendSelectedIncrementalItemSlotRefreshes(session, "avatar_option_change", []alignedcmd.ItemSlotRefresh{{
		ListType:  dnfrepo.MainInventoryListType,
		SlotIndex: request.SourceSlot,
	}}); err != nil {
		return err
	}
	s.logGameEvent(session, "game-avatar-option-change-committed",
		"source_slot", request.SourceSlot,
		"source_item", request.SourceItemID,
		"target_slot", request.TargetSlot,
		"target_item", request.TargetItemID,
		"ability_no", request.AbilityNo,
		"sequence", "durable_avatar_ability_then_class1_op459_result_then_source_op14")
	return nil
}
