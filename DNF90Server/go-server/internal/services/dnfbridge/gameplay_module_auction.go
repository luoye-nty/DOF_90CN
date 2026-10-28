package dnfbridge

import "longheng.io/server/internal/modules/dnf/dnfenum"

func auctionGameplayModule() gameplayModuleDefinition {
	handler := func(service *Service, session *gameSession, request gameplayRequest) error {
		return service.handleCurrentAuctionCommand(session, request.Opcode, request.Body)
	}
	opcodes := []uint16{
		uint16(dnfenum.CmdPacketAuctionAskAveragePrice),
		uint16(dnfenum.CmdPacketAuctionRegistItem),
		uint16(dnfenum.CmdPacketAuctionRegistCancel),
		uint16(dnfenum.CmdPacketAuctionBidding),
		uint16(dnfenum.CmdPacketAuctionSearchByItemkey),
		uint16(dnfenum.CmdPacketAuctionSearchByNoitemkey),
		uint16(dnfenum.CmdPacketAuctionMyRegistedItemInfo),
		uint16(dnfenum.CmdPacketAuctionMyBiddingInfo),
		uint16(dnfenum.CmdPacketAuctionBuyItemApiece),
	}
	legacy := make(map[uint16]gameplayHandler, len(opcodes))
	upper := make(map[uint16]gameplayHandler, len(opcodes))
	for _, opcode := range opcodes {
		legacy[opcode] = handler
		upper[opcode] = handler
	}
	return gameplayModuleDefinition{Name: "auction", LegacyHandlers: legacy, UpperHandlers: upper}
}
