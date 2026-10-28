package dnfbridge

import "longheng.io/server/internal/modules/dnf/dnfenum"

func skillSwitchInventoryGameplayModule() gameplayModuleDefinition {
	opcode := uint16(dnfenum.CmdPacketSkillSwitchInventory)
	handler := func(service *Service, session *gameSession, request gameplayRequest) error {
		return service.handleSkillSwitchInventory(session, request.Body)
	}
	return gameplayModuleDefinition{
		Name:           "skill-switch-inventory",
		LegacyHandlers: map[uint16]gameplayHandler{opcode: handler},
		LegacyNormalizers: map[uint16]gameplayLegacyNormalizer{
			opcode: func(body []byte) []byte { return stripLegacyTransportTrailer(body, skillSwitchInventoryBodySize) },
		},
		UpperHandlers: map[uint16]gameplayHandler{
			opcode: func(service *Service, session *gameSession, request gameplayRequest) error {
				if !requireDefaultGameplayClassification(service, session, request, "game-skill-switch-inventory-blocked", "current_exe_command_class_mismatch") {
					return nil
				}
				return handler(service, session, request)
			},
		},
	}
}
