package dnfbridge

import "longheng.io/server/internal/modules/dnf/dnfenum"

func petSealGameplayModule() gameplayModuleDefinition {
	opcode := uint16(dnfenum.CmdPacketSealCreature)
	handler := func(service *Service, session *gameSession, request gameplayRequest) error {
		return service.handleCurrentPetSeal(session, request.Body)
	}
	return gameplayModuleDefinition{
		Name:           "pet-seal",
		LegacyHandlers: map[uint16]gameplayHandler{opcode: handler},
		UpperHandlers: map[uint16]gameplayHandler{
			opcode: defaultClassGameplayHandler(
				"game-current-pet-seal-blocked",
				"current_exe_op1012_command_class_mismatch",
				func(service *Service, session *gameSession, body []byte) error {
					return service.handleCurrentPetSeal(session, body)
				},
			),
		},
	}
}
