package dnfbridge

import "longheng.io/server/internal/modules/dnf/dnfenum"

func collectboxGameplayModule() gameplayModuleDefinition {
	opcodes := []uint16{
		uint16(dnfenum.CmdPacketSelectCollectbox),
		uint16(dnfenum.CmdPacketAddCollectboxItem),
		uint16(dnfenum.CmdPacketRemoveCollectboxItem),
		uint16(dnfenum.CmdPacketExtendCollectboxExpiryDate),
	}
	legacy := make(map[uint16]gameplayHandler, len(opcodes))
	upper := make(map[uint16]gameplayHandler, len(opcodes))
	for _, opcode := range opcodes {
		opcode := opcode
		legacy[opcode] = collectboxGameplayHandler
		upper[opcode] = defaultClassGameplayHandler(
			"game-collectbox-command-class-blocked",
			"current_exe_collectbox_command_class_mismatch",
			func(service *Service, session *gameSession, body []byte) error {
				return collectboxGameplayHandler(service, session, gameplayRequest{Opcode: opcode, Body: body})
			},
		)
	}
	return gameplayModuleDefinition{
		Name:           "collectbox",
		LegacyHandlers: legacy,
		UpperHandlers:  upper,
	}
}

func collectboxGameplayHandler(
	service *Service,
	session *gameSession,
	request gameplayRequest,
) error {
	return service.handleCollectboxCommand(session, request)
}
