package dnfbridge

import (
	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfproto "longheng.io/server/internal/modules/dnf/protocol"
)

func epicWishGameplayModule() gameplayModuleDefinition {
	opcode := uint16(dnfenum.CmdPacketUpdateWishItem)
	handler := func(service *Service, session *gameSession, request gameplayRequest) error {
		return service.handleCurrentEpicWishUpdate(session, request.Body)
	}
	return gameplayModuleDefinition{
		Name: "epic-wish",
		LegacyHandlers: map[uint16]gameplayHandler{
			opcode: handler,
		},
		UpperHandlers: map[uint16]gameplayHandler{
			opcode: func(service *Service, session *gameSession, request gameplayRequest) error {
				if request.Classification != dnfproto.DefaultChannelClassification {
					service.logGameEvent(session, "game-upper-epic-wish-update-blocked",
						"classification", request.Classification,
						"body_len", len(request.Body),
						"reason", "current_exe_op1352_requires_class1")
					return service.sendCurrentEpicWishUpdateFailure(
						session,
						errCurrentEpicWishRequestInvalid,
					)
				}
				return handler(service, session, request)
			},
		},
	}
}
