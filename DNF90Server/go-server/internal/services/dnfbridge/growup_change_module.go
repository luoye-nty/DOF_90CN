package dnfbridge

import "longheng.io/server/internal/modules/dnf/dnfenum"

// growupChangeGameplayModule 注册转职体验（RE_GROWUP_CHANGE / REQUEST_ITEM_GROWTYPE_EXPERIENCE）
// 请求处理。当前 90CN 客户端以 legacy 帧发送（cmd=1 type=785 转职、type=678 体验），
// 走 selective DPROTO legacy 路由；响应复用 upper 明文 ACK（sendGameUpperSuccess）。
func growupChangeGameplayModule() gameplayModuleDefinition {
	changeOpcode := uint16(dnfenum.CmdPacketReGrowupChange)
	experienceOpcode := uint16(dnfenum.CmdPacketRequestItemGrowtypeExperience)
	return gameplayModuleDefinition{
		Name: "growup-change",
		LegacyHandlers: map[uint16]gameplayHandler{
			changeOpcode: func(service *Service, session *gameSession, request gameplayRequest) error {
				return service.handleCurrentGrowupChange(session, request.Body)
			},
			experienceOpcode: func(service *Service, session *gameSession, request gameplayRequest) error {
				return service.handleCurrentGrowtypeExperience(session, request.Body)
			},
		},
	}
}
