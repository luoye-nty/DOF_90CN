package dnfbridge

import "longheng.io/server/internal/modules/dnf/dnfenum"

func epicBookGameplayModule() gameplayModuleDefinition {
	opcode := uint16(dnfenum.CmdPacketEpicBookMakeItem)
	handler := func(service *Service, session *gameSession, request gameplayRequest) error {
		return service.handleCurrentEpicBookMakeItem(session, request.Body)
	}
	return gameplayModuleDefinition{
		Name: "epic-book",
		LegacyHandlers: map[uint16]gameplayHandler{
			opcode: handler,
		},
		UpperHandlers: map[uint16]gameplayHandler{
			opcode: defaultClassGameplayHandler(
				"game-upper-epic-book-make-item-blocked",
				"current_exe_op788_command_class_mismatch",
				func(service *Service, session *gameSession, body []byte) error {
					return service.handleCurrentEpicBookMakeItem(session, body)
				},
			),
		},
	}
}
