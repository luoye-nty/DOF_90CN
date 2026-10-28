package dnfbridge

import "longheng.io/server/internal/modules/dnf/dnfenum"

func questGameplayModule() gameplayModuleDefinition {
	acceptOpcode := uint16(dnfenum.CmdPacketAcceptQuest)
	giveUpOpcode := uint16(dnfenum.CmdPacketGiveupQuest)
	triggerOpcode := uint16(dnfenum.CmdPacketSetQuestTrigger)
	finishOpcode := uint16(dnfenum.CmdPacketFinishQuest)
	clearTicketOpcode := uint16(dnfenum.CmdPacketClearQuestTicket)
	saveNotifyOpcode := uint16(dnfenum.CmdPacketSaveQuestNotify)
	return gameplayModuleDefinition{
		Name: "quest",
		LegacyHandlers: map[uint16]gameplayHandler{
			// Live 90CN traffic carries GIVEUP_QUEST through the legacy game
			// frame (cmd=1/type=32) even though its response uses the ordinary
			// class-1 upper envelope. Route both transports to the same durable
			// owner so a legacy op32 cannot fall through to the pending aligned
			// handler after the client has already played its confirmation sound.
			giveUpOpcode: func(service *Service, session *gameSession, request gameplayRequest) error {
				return service.handleCurrentGiveUpQuest(session, request.Body)
			},
			saveNotifyOpcode: func(service *Service, session *gameSession, request gameplayRequest) error {
				return service.handleCurrentSaveQuestNotify(session, request.Body)
			},
			// The selective DPROTO route carries class-1 op33 through the
			// legacy decoder and still requires the durable owner plus ACK.
			triggerOpcode: func(service *Service, session *gameSession, request gameplayRequest) error {
				return service.handleCurrentSetQuestTrigger(session, request.Body)
			},
			// Selective plaintext op34 must share the atomic PVF/DB settlement
			// owner with its upper route.
			finishOpcode: func(service *Service, session *gameSession, request gameplayRequest) error {
				return service.handleCurrentFinishQuest(session, request.Body)
			},
			// The task-manual confirmation arrives as legacy op1429 with one
			// u32 selector and owns the no-reward epic-skip transaction.
			clearTicketOpcode: func(service *Service, session *gameSession, request gameplayRequest) error {
				return service.handleCurrentAutoCompleteMainQuest(session, request.Body)
			},
		},
		UpperHandlers: map[uint16]gameplayHandler{
			acceptOpcode: func(service *Service, session *gameSession, request gameplayRequest) error {
				if !requireDefaultGameplayClassification(service, session, request, "game-upper-accept-quest-blocked", "current_exe_op31_command_class_mismatch") {
					return nil
				}
				return service.handleCurrentAcceptQuest(session, request.Body)
			},
			giveUpOpcode: func(service *Service, session *gameSession, request gameplayRequest) error {
				if !requireDefaultGameplayClassification(service, session, request, "game-upper-give-up-quest-blocked", "current_exe_op32_command_class_mismatch") {
					return nil
				}
				return service.handleCurrentGiveUpQuest(session, request.Body)
			},
			triggerOpcode: func(service *Service, session *gameSession, request gameplayRequest) error {
				if !requireDefaultGameplayClassification(service, session, request, "game-upper-set-quest-trigger-blocked", "current_exe_op33_command_class_mismatch") {
					return nil
				}
				return service.handleCurrentSetQuestTrigger(session, request.Body)
			},
			finishOpcode: func(service *Service, session *gameSession, request gameplayRequest) error {
				if !requireDefaultGameplayClassification(service, session, request, "game-upper-finish-quest-blocked", "current_exe_op34_command_class_mismatch") {
					return nil
				}
				return service.handleCurrentFinishQuest(session, request.Body)
			},
			clearTicketOpcode: func(service *Service, session *gameSession, request gameplayRequest) error {
				if !requireDefaultGameplayClassification(service, session, request, "game-auto-complete-main-quest-blocked", "current_exe_op1429_command_class_mismatch") {
					return nil
				}
				return service.handleCurrentAutoCompleteMainQuest(session, request.Body)
			},
		},
		LegacyNormalizers: map[uint16]gameplayLegacyNormalizer{
			triggerOpcode: normalizeLegacySetQuestTriggerBody,
		},
	}
}
