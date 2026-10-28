package dnfbridge

import "longheng.io/server/internal/modules/dnf/dnfenum"

func skillGameplayModule() gameplayModuleDefinition {
	layoutOpcode := uint16(dnfenum.CmdPacketChangeSkillslot)
	buyOpcode := uint16(dnfenum.CmdPacketBuySkill)
	resetOpcode := uint16(dnfenum.CmdPacketSkillInit)
	characterOptionOpcode := uint16(dnfenum.CmdPacketSaveCharacterOption)
	comboOpcode := uint16(dnfenum.CmdPacketComboSkillInfo)
	comboResetOpcode := uint16(dnfenum.CmdPacketComboSkillExtensionQuickSlotReset)
	dojoOpcode := uint16(dnfenum.CmdPacketArcadePVPDataCopy)
	characterOptionHandler := func(service *Service, session *gameSession, request gameplayRequest) error {
		return service.handleCurrentSaveCharacterOption(session, request.Body)
	}
	return gameplayModuleDefinition{
		Name: "skill",
		LegacyHandlers: map[uint16]gameplayHandler{
			dojoOpcode: func(service *Service, session *gameSession, request gameplayRequest) error {
				return service.handleCurrentPVPDojoSkillMode(session, request.Body)
			},
			characterOptionOpcode: characterOptionHandler,
			comboOpcode: func(service *Service, session *gameSession, request gameplayRequest) error {
				return service.handleCurrentDarkKnightComboSkillInfo(session, request.Body)
			},
			comboResetOpcode: func(service *Service, session *gameSession, request gameplayRequest) error {
				return service.handleCurrentDarkKnightComboQuickSlotReset(session, request.Body)
			},
		},
		UpperHandlers: map[uint16]gameplayHandler{
			dojoOpcode: defaultClassGameplayHandler("game-pvp-dojo-mode-class-blocked",
				"current_exe_dojo_mode_requires_default_class", (*Service).handleCurrentPVPDojoSkillMode),
			characterOptionOpcode: defaultClassGameplayHandler(
				"game-save-character-option-ignored",
				"current_exe_op447_command_class_mismatch",
				func(service *Service, session *gameSession, body []byte) error {
					return service.handleCurrentSaveCharacterOption(session, body)
				},
			),
			comboOpcode: defaultClassGameplayHandler(
				"game-dark-knight-combo-skill-info-blocked",
				"current_exe_op508_command_class_mismatch",
				(*Service).handleCurrentDarkKnightComboSkillInfo,
			),
			comboResetOpcode: defaultClassGameplayHandler(
				"game-dark-knight-combo-quick-slot-reset-blocked",
				"current_exe_op510_command_class_mismatch",
				(*Service).handleCurrentDarkKnightComboQuickSlotReset,
			),
		},
		LegacyNormalizers: map[uint16]gameplayLegacyNormalizer{
			dojoOpcode: func(body []byte) []byte { return stripLegacyTransportTrailer(body, 1) },
			layoutOpcode: func(body []byte) []byte {
				return stripLegacyTransportTrailer(body, 8)
			},
			buyOpcode:   stripLegacyBuySkillTransportTrailer,
			resetOpcode: func(body []byte) []byte { return stripLegacyTransportTrailer(body, 2) },
			comboOpcode: normalizeLegacyDarkKnightComboSkillInfoBody,
			comboResetOpcode: func(body []byte) []byte {
				return stripLegacyTransportTrailer(body, 2)
			},
		},
	}
}
