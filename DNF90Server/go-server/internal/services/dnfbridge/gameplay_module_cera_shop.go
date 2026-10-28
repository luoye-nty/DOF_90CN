package dnfbridge

import "longheng.io/server/internal/modules/dnf/dnfenum"

func ceraShopGameplayModule() gameplayModuleDefinition {
	purchaseOpcode := uint16(dnfenum.CmdPacketBuyCerashopItem)
	ticketOpcode := uint16(dnfenum.CmdPacketGenCeraticket)
	// 赠送收件人查询：客户端在「确认收件人姓名」之后发 legacy cmd=1 type=136，
	// 拿到回包才画结果对话框。现场只观测到 legacy 一条传输（2026-09-27 收包），
	// 所以这里**只登记 legacy**；将来若抓到 class-1 到达再单独补 upper，
	// 不靠推测提前登记（upper 没登记时行为与改动前逐字节一致）。
	giftQueryOpcode := currentCeraShopGiftQueryOpcode
	return gameplayModuleDefinition{
		Name: "cera-shop",
		LegacyHandlers: map[uint16]gameplayHandler{
			// Current-client evidence proves op64 can arrive on the legacy
			// decoder with the same current cart body as the upper transport.
			purchaseOpcode: func(service *Service, session *gameSession, request gameplayRequest) error {
				return service.handleCurrentCeraShopPurchase(session, request.Body)
			},
			ticketOpcode: func(service *Service, session *gameSession, request gameplayRequest) error {
				return service.handleCurrentCeraTicket(session, request.Body)
			},
			giftQueryOpcode: func(service *Service, session *gameSession, request gameplayRequest) error {
				return service.handleCurrentCeraShopGiftQuery(session, request.Body)
			},
		},
		UpperHandlers: map[uint16]gameplayHandler{
			purchaseOpcode: func(service *Service, session *gameSession, request gameplayRequest) error {
				if !requireDefaultGameplayClassification(service, session, request, "game-cera-shop-purchase-blocked", "current_exe_op64_command_class_mismatch") {
					return nil
				}
				return service.handleCurrentCeraShopPurchase(session, request.Body)
			},
			ticketOpcode: func(service *Service, session *gameSession, request gameplayRequest) error {
				if !requireDefaultGameplayClassification(service, session, request, "game-cera-ticket-blocked", "current_exe_op65_requires_class1") {
					return nil
				}
				return service.handleCurrentCeraTicket(session, request.Body)
			},
		},
	}
}
