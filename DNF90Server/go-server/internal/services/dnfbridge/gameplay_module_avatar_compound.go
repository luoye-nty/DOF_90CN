package dnfbridge

import (
	"context"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfproto "longheng.io/server/internal/modules/dnf/protocol"
)

func avatarCompoundGameplayModule() gameplayModuleDefinition {
	opcode := uint16(dnfenum.CmdPacketCompoundAvatar)
	return gameplayModuleDefinition{
		Name: "avatar-compound",
		LegacyHandlers: map[uint16]gameplayHandler{
			opcode: func(service *Service, session *gameSession, request gameplayRequest) error {
				return service.handleCurrentAvatarCompound(session, request.Body)
			},
			// 绝对/梦想装扮合成器 (e.g. 梦想装扮合成器 10007246, `[action type]
			// [absolute bind cube] 0 8 2 0 0`)：op1119 请求体 144 字节，
			// +0x10 是玩家点名的输出装扮，+0x18 是 8 件材料（槽 + 道具 ID）。
			//
			// 线上取证（2026-10-04，CubeProbe 只读探针）：客户端 class-1 注册表里
			// **有** op1119 的处理器 0x1135C10 —— 服务端从来不回这个包，才是
			// "合成无结果 + 鼠标点不动" 的根因。回包形状（55 字节，class-1 op1119）
			// 逐指令读自 0x1135C10 / 0x1135680 / 0x1F2B850，见
			// 梦想装扮合成器op1119_回包格式定案_20261004.md。
			uint16(dnfenum.CmdPacketBindPlus): func(service *Service, session *gameSession, request gameplayRequest) error {
				return service.handleCurrentAvatarBindPlus(session, request.Body)
			},
		},
		UpperHandlers: map[uint16]gameplayHandler{
			opcode: func(service *Service, session *gameSession, request gameplayRequest) error {
				if !requireDefaultGameplayClassification(service, session, request, "game-avatar-compound-blocked", "current_exe_class1_op99_required") {
					return nil
				}
				return service.handleCurrentAvatarCompound(session, request.Body)
			},
			uint16(dnfenum.CmdPacketBindPlus): func(service *Service, session *gameSession, request gameplayRequest) error {
				if !requireDefaultGameplayClassification(service, session, request, "game-avatar-bind-plus-blocked", "current_exe_class1_op1119_required") {
					return nil
				}
				return service.handleCurrentAvatarBindPlus(session, request.Body)
			},
		},
		LegacyNormalizers: map[uint16]gameplayLegacyNormalizer{
			opcode: func(body []byte) []byte {
				if len(body) == currentAvatarCompoundRequestSize+4 {
					return append([]byte(nil), body[:currentAvatarCompoundRequestSize]...)
				}
				return body
			},
		},
	}
}

func (s *Service) handleCurrentAvatarCompound(session *gameSession, body []byte) error {
	opcode := uint16(dnfenum.CmdPacketCompoundAvatar)
	request, err := parseCurrentAvatarCompoundRequest(body)
	if err != nil {
		s.logGameEvent(session, "game-avatar-compound-rejected", "body_len", len(body), "reason", err)
		return s.sendGameUpperRawClass(session, opcode, currentAvatarCompoundFailureBody(), dnfproto.DefaultChannelClassification)
	}
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	catalog, err := s.currentPVFItemCatalog()
	var result currentAvatarCompoundResult
	if err == nil {
		result, err = s.commitCurrentAvatarCompound(ctx, session, catalog, request)
	}
	if err != nil {
		s.logGameEvent(session, "game-avatar-compound-rejected",
			"consume_slot", request.ConsumeSlot,
			"slot1", request.Slot1,
			"slot2", request.Slot2,
			"requested_item", request.RequestedItemID,
			"ability_no", request.AbilityNo,
			"material_class", request.MaterialClass,
			"reason", err)
		return s.sendGameUpperRawClass(session, opcode, currentAvatarCompoundFailureBody(), dnfproto.DefaultChannelClassification)
	}
	if err := s.sendGameUpperRawClass(session, opcode, buildCurrentAvatarCompoundSuccessBody(request, result), dnfproto.DefaultChannelClassification); err != nil {
		return err
	}
	s.logGameEvent(session, "game-avatar-compound-committed",
		"consume_slot", request.ConsumeSlot,
		"material_item", result.MaterialItemID,
		"material_remaining", result.MaterialRemain,
		"slot1", request.Slot1,
		"slot2", request.Slot2,
		"source_item1", result.SourceItemIDs[0],
		"source_item2", result.SourceItemIDs[1],
		"requested_item", request.RequestedItemID,
		"ability_no", request.AbilityNo,
		"material_class", request.MaterialClass,
		"rare_rate", result.RareRate,
		"rare_roll", result.RareRoll,
		"rare_hit", result.RareHit,
		"new_slots", result.NewSlots,
		"new_items", result.NewItemIDs,
		"evidence", "NoPack.exe sub_317DE70/sub_1D1C6D0")
	return nil
}

// sendCurrentAvatarBindPlusResult 按"客户端真正的消费顺序"下发三包：
//
//  1. 合成器那一格的增量行（先发，让那一格在客户端立刻消失 / 变成剩余数量）；
//  2. class-1 op1119 成功回包（客户端据此扣 8 个材料格 + 建结果装扮对象）；
//  3. 结果装扮那一格的权威行（回包之后发，把客户端自建对象的属性校正成服务端口径）。
//
// 第 1 包是"合成器不及时消失"的修复（2026-10-04 现场回报），第 3 包是"属性显示
// 有误"的兜底修复（主修复是回包 +0x04/+0x0A 两个字段别再写反）。
func (s *Service) sendCurrentAvatarBindPlusResult(session *gameSession, outcome currentAvatarBindPlusOutcome) error {
	opcode := uint16(dnfenum.CmdPacketBindPlus)
	if err := s.sendSelectedIncrementalItemSlotRefreshes(session, "avatar_bind_plus_before_success",
		[]alignedcmd.ItemSlotRefresh{{ListType: outcome.CubeListType, SlotIndex: outcome.CubeSlot}}); err != nil {
		return err
	}
	if err := s.sendGameUpperRawClass(session, opcode, buildCurrentAvatarBindPlusSuccessBody(outcome), dnfproto.DefaultChannelClassification); err != nil {
		return err
	}
	return s.sendSelectedIncrementalItemSlotRefreshes(session, "avatar_bind_plus_after_success",
		[]alignedcmd.ItemSlotRefresh{{ListType: currentAvatarInventoryListType, SlotIndex: outcome.OutputSlot}})
}

// handleCurrentAvatarBindPlus 处理 op1119（梦想/绝对绑定装扮合成器）。
//
// 这是"客户端一直在等的那个回包"：处理器 RVA 0x1135C10 从 body[1] 起读 54 字节，
// 按 8 个材料格逐格扣减、再拿 +0x06 的道具 ID 建结果对象。回包形状与理由见
// avatar_bind_plus_protocol.go 顶部注释。
func (s *Service) handleCurrentAvatarBindPlus(session *gameSession, body []byte) error {
	opcode := uint16(dnfenum.CmdPacketBindPlus)
	request, err := parseCurrentAvatarBindPlusRequest(body)
	if err != nil {
		s.logGameEvent(session, "game-avatar-bind-plus-rejected",
			"body_len", len(body), "reason", err)
		return s.sendGameUpperRawClass(session, opcode, currentAvatarBindPlusFailureBody(), dnfproto.DefaultChannelClassification)
	}
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	catalog, err := s.currentPVFItemCatalog()
	var outcome currentAvatarBindPlusOutcome
	if err == nil {
		outcome, err = s.commitCurrentAvatarBindPlus(ctx, session, catalog, request)
	}
	if err != nil {
		s.logGameEvent(session, "game-avatar-bind-plus-rejected",
			"body_len", len(body),
			"requested_item", request.RequestedItemID,
			"ability_no", request.AbilityNo,
			"materials", len(request.Materials),
			"reason", err)
		return s.sendGameUpperRawClass(session, opcode, currentAvatarBindPlusFailureBody(), dnfproto.DefaultChannelClassification)
	}
	// 合成器那一格必须单独下发一行：客户端 op1119 的处理器只处理 8 个材料格
	// （回包 +0x0E 的 8 个槽），**没有任何字段提到合成器**。op99 之所以"合成器立刻
	// 消失"，是因为它的回包体自带 ConsumeSlot 那一行删除行；1119 没有，所以现场表现
	// 是"合成器要整理背包才消失"（2026-10-04 回报）。
	if err := s.sendCurrentAvatarBindPlusResult(session, outcome); err != nil {
		return err
	}
	slots := make([]int, 0, len(outcome.ConsumedSlots))
	for _, slot := range outcome.ConsumedSlots {
		slots = append(slots, int(slot))
	}
	s.logGameEvent(session, "game-avatar-bind-plus-committed",
		"set_index", outcome.SetIndex,
		"part", outcome.Part,
		"cube_item", outcome.CubeItemID,
		"cube_list", outcome.CubeListType,
		"cube_slot", outcome.CubeSlot,
		"cube_remaining", outcome.CubeRemain,
		"material_slots", slots,
		"output_item", outcome.OutputItemID,
		"output_slot", outcome.OutputSlot,
		"ability_no", request.AbilityNo,
		"body_len", len(body),
		"reply_len", currentAvatarBindPlusReplySize,
		"evidence", "CubeProbe class-1 op1119 handler 0x1135C10 + 0x1135680 + 0x1F2B850")
	return nil
}
