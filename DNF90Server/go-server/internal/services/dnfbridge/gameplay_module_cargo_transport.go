package dnfbridge

import "longheng.io/server/internal/modules/dnf/dnfenum"

func cargoTransportGameplayModule() gameplayModuleDefinition {
	opcode := uint16(dnfenum.CmdPacketCargoTransportItem)
	handler := func(service *Service, session *gameSession, request gameplayRequest) error {
		return service.handleCurrentCargoTransport(session, request.Body)
	}
	return gameplayModuleDefinition{
		Name:           "cargo-transport",
		LegacyHandlers: map[uint16]gameplayHandler{opcode: handler},
		UpperHandlers: map[uint16]gameplayHandler{
			opcode: defaultClassGameplayHandler(
				"game-current-cargo-transport-blocked",
				"current_exe_op557_command_class_mismatch",
				func(service *Service, session *gameSession, body []byte) error {
					return service.handleCurrentCargoTransport(session, body)
				},
			),
		},
	}
}
