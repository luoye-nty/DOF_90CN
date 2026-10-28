package dnfbridge

import "longheng.io/server/internal/modules/dnf/dnfenum"

func gameOptionGameplayModule() gameplayModuleDefinition {
	mainOpcode := uint16(dnfenum.CmdPacketSaveGameOption1)
	keymapOpcode := uint16(dnfenum.CmdPacketSaveGameOption2)
	quickChatOpcode := uint16(dnfenum.CmdPacketSaveGameOptionQuickchatting)
	return gameplayModuleDefinition{
		Name: "game-option",
		LegacyHandlers: map[uint16]gameplayHandler{
			mainOpcode: func(service *Service, session *gameSession, request gameplayRequest) error {
				return service.handleCurrentSaveGameOption(session, request.Body)
			},
			keymapOpcode: func(service *Service, session *gameSession, request gameplayRequest) error {
				return service.handleCurrentSaveGameOptionKeymap(session, request.Body)
			},
			quickChatOpcode: func(service *Service, session *gameSession, request gameplayRequest) error {
				return service.handleCurrentSaveGameOptionQuickChat(session, request.Body)
			},
		},
	}
}
