package dnfbridge

import "longheng.io/server/internal/modules/dnf/dnfenum"

func characterRenameGameplayModule() gameplayModuleDefinition {
	useCardOpcode := uint16(dnfenum.CmdPacketUseRenameCard)
	renameOpcode := uint16(dnfenum.CmdPacketChangeCharacName)
	return gameplayModuleDefinition{
		Name: "character-rename",
		LegacyHandlers: map[uint16]gameplayHandler{
			useCardOpcode: func(service *Service, session *gameSession, request gameplayRequest) error {
				return service.handleCurrentUseCharacterRenameCard(session, request.Body)
			},
			renameOpcode: func(service *Service, session *gameSession, request gameplayRequest) error {
				return service.handleCurrentChangeCharacterName(session, request.Body)
			},
		},
		UpperHandlers: map[uint16]gameplayHandler{
			useCardOpcode: defaultClassGameplayHandler(
				"game-character-rename-card-blocked",
				"current_exe_op509_command_class_mismatch",
				(*Service).handleCurrentUseCharacterRenameCard,
			),
			renameOpcode: defaultClassGameplayHandler(
				"game-character-rename-blocked",
				"current_exe_op135_command_class_mismatch",
				(*Service).handleCurrentChangeCharacterName,
			),
		},
		LegacyNormalizers: map[uint16]gameplayLegacyNormalizer{
			useCardOpcode: func(body []byte) []byte {
				return stripLegacyTransportTrailer(body, currentUseCharacterRenameCardBodySize)
			},
			renameOpcode: normalizeLegacyCharacterRenameBody,
		},
	}
}
