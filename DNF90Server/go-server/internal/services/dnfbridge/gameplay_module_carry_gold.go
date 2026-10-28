package dnfbridge

import "longheng.io/server/internal/modules/dnf/dnfenum"

// carryGoldGameplayModule routes ENUM_CMDPACKET_UPGRADE_CARRY_GOLD. The live
// client sends it as legacy cmd1/type978; the upper route is registered for
// the same reason every other gameplay module registers both, so a client
// that switches transports still gets an answer instead of a dead button.
func carryGoldGameplayModule() gameplayModuleDefinition {
	opcode := uint16(dnfenum.CmdPacketUpgradeCarryGold)
	return gameplayModuleDefinition{
		Name: "carry-gold",
		LegacyHandlers: map[uint16]gameplayHandler{
			opcode: func(service *Service, session *gameSession, request gameplayRequest) error {
				return service.handleCurrentUpgradeCarryGold(session, request.Body)
			},
		},
		UpperHandlers: map[uint16]gameplayHandler{
			opcode: defaultClassGameplayHandler(
				"game-carry-gold-upgrade-blocked",
				"current_exe_op978_command_class_mismatch",
				func(service *Service, session *gameSession, body []byte) error {
					return service.handleCurrentUpgradeCarryGold(session, body)
				},
			),
		},
	}
}
