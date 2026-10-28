package dnfbridge

import "longheng.io/server/internal/modules/dnf/dnfenum"

func dungeonTournamentRewardGameplayModule() gameplayModuleDefinition {
	stateOpcode := uint16(dnfenum.CmdPacketTournamentRewardSelectState)
	selectOpcode := uint16(dnfenum.CmdPacketTournamentRewardSelect)
	return gameplayModuleDefinition{
		Name: "dungeon-tournament-reward",
		LegacyHandlers: map[uint16]gameplayHandler{
			stateOpcode: func(service *Service, session *gameSession, request gameplayRequest) error {
				return service.handleTournamentRewardSelectState(session, request.Body)
			},
			selectOpcode: func(service *Service, session *gameSession, request gameplayRequest) error {
				return service.handleTournamentRewardSelect(session, request.Body)
			},
		},
		UpperHandlers: map[uint16]gameplayHandler{
			stateOpcode: defaultClassGameplayHandler(
				"game-tournament-reward-state-blocked",
				"current_exe_op457_command_class_mismatch",
				func(service *Service, session *gameSession, body []byte) error {
					return service.handleTournamentRewardSelectState(session, body)
				},
			),
			selectOpcode: defaultClassGameplayHandler(
				"game-tournament-reward-select-blocked",
				"current_exe_op458_command_class_mismatch",
				func(service *Service, session *gameSession, body []byte) error {
					return service.handleTournamentRewardSelect(session, body)
				},
			),
		},
	}
}
