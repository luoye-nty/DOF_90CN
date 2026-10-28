package dnfbridge

import "longheng.io/server/internal/modules/dnf/dnfenum"

func customAbilityGameplayModule() gameplayModuleDefinition {
	modifyOpcode := uint16(dnfenum.CmdPacketCustomAbilityEquipOption)
	setOpcode := uint16(dnfenum.CmdPacketCustomAbilitySetEquipOption)
	upgradeOpcode := uint16(dnfenum.CmdPacketCustomAbilityUpgrade)
	modifyHandler := func(service *Service, session *gameSession, request gameplayRequest) error {
		return service.handleCurrentCustomAbilityModify(session, request.Body)
	}
	upgradeHandler := func(service *Service, session *gameSession, request gameplayRequest) error {
		return service.handleCurrentCustomAbilityUpgrade(session, request.Body)
	}
	setHandler := func(service *Service, session *gameSession, request gameplayRequest) error {
		return service.handleCurrentCustomAbilitySet(session, request.Body)
	}
	return gameplayModuleDefinition{
		Name: "custom-ability",
		LegacyHandlers: map[uint16]gameplayHandler{
			modifyOpcode:  modifyHandler,
			setOpcode:     setHandler,
			upgradeOpcode: upgradeHandler,
		},
		UpperHandlers: map[uint16]gameplayHandler{
			modifyOpcode: defaultClassGameplayHandler(
				"game-custom-ability-modify-blocked",
				"current_exe_op1340_command_class_mismatch",
				func(service *Service, session *gameSession, body []byte) error {
					return service.handleCurrentCustomAbilityModify(session, body)
				},
			),
			setOpcode: defaultClassGameplayHandler(
				"game-custom-ability-set-blocked",
				"current_exe_op1341_command_class_mismatch",
				func(service *Service, session *gameSession, body []byte) error {
					return service.handleCurrentCustomAbilitySet(session, body)
				},
			),
			upgradeOpcode: defaultClassGameplayHandler(
				"game-custom-ability-upgrade-blocked",
				"current_exe_op1342_command_class_mismatch",
				func(service *Service, session *gameSession, body []byte) error {
					return service.handleCurrentCustomAbilityUpgrade(session, body)
				},
			),
		},
	}
}
