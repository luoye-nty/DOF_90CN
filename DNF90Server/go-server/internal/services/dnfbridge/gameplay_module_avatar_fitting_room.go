package dnfbridge

import (
	"context"
	"errors"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfproto "longheng.io/server/internal/modules/dnf/protocol"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

func avatarFittingRoomGameplayModule() gameplayModuleDefinition {
	opcode := uint16(dnfenum.CmdPacketAvatarFittingRoomChange)
	return gameplayModuleDefinition{
		Name: "avatar-fitting-room",
		LegacyHandlers: map[uint16]gameplayHandler{
			opcode: func(service *Service, session *gameSession, request gameplayRequest) error {
				return service.handleCurrentAvatarFittingRoom(session, request.Body)
			},
		},
		UpperHandlers: map[uint16]gameplayHandler{
			opcode: func(service *Service, session *gameSession, request gameplayRequest) error {
				if !requireDefaultGameplayClassification(service, session, request, "game-avatar-fitting-room-blocked", "current_exe_class1_op1488_required") {
					return nil
				}
				return service.handleCurrentAvatarFittingRoom(session, request.Body)
			},
		},
		LegacyNormalizers: map[uint16]gameplayLegacyNormalizer{
			opcode: func(body []byte) []byte {
				if len(body) == currentAvatarFittingRoomRequestSize+4 {
					return append([]byte(nil), body[:currentAvatarFittingRoomRequestSize]...)
				}
				return body
			},
		},
	}
}

func (s *Service) handleCurrentAvatarFittingRoom(session *gameSession, body []byte) error {
	opcode := uint16(dnfenum.CmdPacketAvatarFittingRoomChange)
	request, err := parseCurrentAvatarFittingRoomRequest(body)
	if err != nil {
		s.logGameEvent(session, "game-avatar-fitting-room-rejected", "body_len", len(body), "reason", err)
		return s.sendGameUpperRawClass(session, opcode, currentAvatarFittingRoomFailureBody(), dnfproto.DefaultChannelClassification)
	}
	base := context.Background()
	if session != nil && session.ctx != nil {
		base = session.ctx
	}
	ctx, cancel := context.WithTimeout(base, currentConsumableWriteTimeout)
	defer cancel()
	catalog, err := s.currentPVFItemCatalog()
	var result currentAvatarFittingRoomResult
	if err == nil {
		result, err = s.commitCurrentAvatarFittingRoom(ctx, session, catalog, request)
	}
	if err != nil {
		s.logGameEvent(session, "game-avatar-fitting-room-rejected", "body_len", len(body), "reason", err)
		return s.sendGameUpperRawClass(session, opcode, currentAvatarFittingRoomFailureBody(), dnfproto.DefaultChannelClassification)
	}

	refreshes := make([]alignedcmd.ItemSlotRefresh, 0, len(result.Domain.TicketSlots)+len(result.Domain.RewardSlots))
	for _, slot := range result.Domain.TicketSlots {
		refreshes = append(refreshes, alignedcmd.ItemSlotRefresh{ListType: dnfrepo.MainInventoryListType, SlotIndex: slot})
	}
	for _, slot := range result.Domain.RewardSlots {
		refreshes = append(refreshes, alignedcmd.ItemSlotRefresh{ListType: dnfrepo.MainInventoryListType, SlotIndex: slot})
	}
	if err := s.sendSelectedIncrementalItemSlotRefreshes(session, "avatar_fitting_room_before_success", refreshes); err != nil {
		return err
	}
	// IDA sub_1D4A7D0 applies the op1488 success rows to already-existing
	// equipment objects. Refresh list-3 first; reversing these two writes leaves
	// the client pointing at the old avatar templates until relog.
	if err := s.sendSelectedCurrentEquipmentSlotItemUpdates(session, "avatar_fitting_room_before_success"); err != nil {
		return err
	}
	if err := s.sendGameUpperRawClass(
		session,
		opcode,
		buildCurrentAvatarFittingRoomSuccessBody(request, result.Changed),
		dnfproto.DefaultChannelClassification,
	); err != nil {
		return err
	}
	if err := s.sendAlignedPostAction(session, "avatar_fitting_room", alignedcmd.PostActionRefreshSelectedActorAppearance); err != nil &&
		!errors.Is(err, errAlignedPostCommitProjectionDeferred) {
		return err
	}
	s.logGameEvent(session, "game-avatar-fitting-room-committed",
		"ticket_consumed", result.Domain.TicketConsumed,
		"ticket_slots", result.Domain.TicketSlots,
		"equipment_slots", result.Domain.EquipmentSlots,
		"reward_granted", result.Domain.RewardGranted,
		"reward_slots", result.Domain.RewardSlots,
		"gauge_before", result.Domain.GaugeBefore,
		"gauge_after", result.Domain.GaugeAfter,
		"sequence", "atomic_inventory_equipment_gauge_then_op14_ticket_then_op14_equipment_then_op1488_success_then_mode0_appearance",
		"evidence", "NoPack.exe op1488 sender 117-byte body and success reader 1+8x13")
	return nil
}
