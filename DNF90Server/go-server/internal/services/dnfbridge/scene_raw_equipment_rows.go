package dnfbridge

// restoreCurrentSceneRawEquipmentRows 重放"mode1/mode3 创建行不携带"的装备实例原始字节。
//
// 当前的 EXE 有**两处**装备数据不在创建行里，必须靠 list-3 的 op14 行单独补：
//
//	公会勋章上的守护珠（guardian gem）raw socket 字 —— 缺了就显示"没有已装备的守护珠"，
//	  图标与属性一起消失（2026-10-08 现场：修炼场 / 组队进图 / 团本，出图即恢复）；
//	称号的 raw 随机附魔（+0x61..+0x65）—— 与守护珠同一个缺口。
//
// 任何一次本地 actor 重建都会把这些对象重建成"没有原始字节"的状态：进副本、换房间、
// 城内换区、回城、团本边界。所以凡是"我们自己提交的场景重建"都要调用本函数一次。
// 失败只记日志：少一个面板不该让走位/进图失败。
func (s *Service) restoreCurrentSceneRawEquipmentRows(session *gameSession, source string) {
	if s == nil || session == nil || session.selectedCharacterID == 0 {
		return
	}
	if err := s.sendSelectedGuardianGemWornMedalRefresh(session, source); err != nil {
		s.logGameEvent(session, "game-scene-guardian-gem-refresh-failed",
			"source", source, "char_id", session.selectedCharacterID, "error", err)
	}
	if err := s.sendSelectedWornTitleEnchantRowRefresh(session, source); err != nil {
		s.logGameEvent(session, "game-scene-title-enchant-refresh-failed",
			"source", source, "char_id", session.selectedCharacterID, "error", err)
	}
}
