package dnfbridge

import "longheng.io/server/internal/modules/dnf/dnfenum"

func privateStoreGameplayModule() gameplayModuleDefinition {
	handler := func(service *Service, session *gameSession, request gameplayRequest) error {
		return service.handleCurrentPrivateStoreCommand(session, request.Opcode, request.Body)
	}
	opcodes := []uint16{
		uint16(dnfenum.CmdPacketBuyPrivateStoreItem),
		uint16(dnfenum.CmdPacketEnterPrivateStore),
		uint16(dnfenum.CmdPacketExitPrivateStore),
		uint16(dnfenum.CmdPacketCreatePrivateStore),
		uint16(dnfenum.CmdPacketRemovePrivateStore),
		87, // COMPLETE_PRIVATE_STORE_DISPLAY in the current executable.
		uint16(dnfenum.CmdPacketVerifyPrivateStoreItem),
	}
	legacy := make(map[uint16]gameplayHandler, len(opcodes))
	upper := make(map[uint16]gameplayHandler, len(opcodes))
	for _, opcode := range opcodes {
		legacy[opcode] = handler
		upper[opcode] = handler
	}
	return gameplayModuleDefinition{Name: "private-store", LegacyHandlers: legacy, UpperHandlers: upper}
}
