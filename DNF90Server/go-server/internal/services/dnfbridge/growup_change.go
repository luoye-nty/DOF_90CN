package dnfbridge

import (
	"context"
	"encoding/binary"
	"errors"
	"strconv"
	"time"

	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfprofession "longheng.io/server/internal/modules/dnf/profession"
)

// handleCurrentGrowupChange 处理转职体验请求 RE_GROWUP_CHANGE（legacy cmd=1
// type=785）。当前 90CN 客户端请求 body 为 14 字节，最后一个字节为目标职业
// 分支（body[13]）；同时兼容 86 客户端的 15 字节布局（目标分支位于 body[14]）。
func (s *Service) handleCurrentGrowupChange(session *gameSession, body []byte) error {
	if session == nil || session.selectedCharacterID == 0 {
		return nil
	}
	characterID := strconv.FormatUint(uint64(session.selectedCharacterID), 10)
	target, err := decodeGrowupChangeTargetGrowType(body)
	if err != nil {
		s.logPacketEvent("growup-change-request-ignored",
			"reason", "body_too_short",
			"body_len", len(body),
			"character_id", characterID)
		return nil
	}
	s.logPacketEvent("growup-change-request",
		"character_id", characterID,
		"target_grow_type", target,
		"body_len", len(body))

	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.CharacterSettlement == nil {
		s.logPacketEvent("growup-change-rejected", "reason", "repository_unavailable")
		return nil
	}
	profiles, skillCatalog, err := s.currentProfessionResources(ctx)
	if err != nil {
		s.logPacketEvent("growup-change-rejected", "reason", "profession_resources_unavailable")
		return nil
	}
	result, err := dnfprofession.ApplyGrowupChange(ctx, repositories, dnfprofession.GrowupChangeInput{
		AccountID:      s.accountIDForSession(session),
		CharacterID:    characterID,
		TargetGrowType: target,
		Profiles:       profiles,
		SkillCatalog:   skillCatalog,
		CommittedAt:    time.Now().UTC(),
	})
	if err != nil {
		s.logPacketEvent("growup-change-rejected", "reason", "owner_failed", "error", err.Error())
		return nil
	}
	ackBody := buildGrowupChangeAckBody(result.ResultCode, result.NewChangeCount)
	if err := s.sendGameUpperSuccess(session, uint16(dnfenum.CmdPacketReGrowupChange), ackBody); err != nil {
		return err
	}
	s.logPacketEvent("growup-change-ack",
		"character_id", characterID,
		"result_code", result.ResultCode,
		"success", result.Success,
		"new_grow_type", result.NewGrowType,
		"previous_change_count", result.PreviousChangeCount,
		"new_change_count", result.NewChangeCount,
		"gold_cost", result.GoldCost,
		"updated_gold", result.UpdatedGold)
	if !result.Success {
		return nil
	}
	return s.refreshAfterGrowupTransition(session, ctx, result, "growup_change")
}

// refreshAfterGrowupTransition 在转职/转职体验成功提交后刷新客户端外观、技能
// 与用户状态。真正的副本内（runtime 存在且尚未进入回城过渡）中途重建 actor 会
// 破坏地图运行时，因此只重发当前场景技能信息；一旦进入回城过渡
// （townReturnPending/townReturnOp24Sent）或已在城镇，则走 mode0/mode1 外观重建
// 后再补技能与用户状态的完整刷新链路。
func (s *Service) refreshAfterGrowupTransition(
	session *gameSession,
	ctx context.Context,
	result dnfprofession.GrowupChangeResult,
	eventPrefix string,
) error {
	if currentGrowupTransitionNeedsDungeonOnlySkill(session) {
		if err := s.sendCurrentSceneSkillInfo(session, ctx, result.Character, "current_exe_"+eventPrefix+"_in_dungeon"); err != nil {
			return err
		}
		return nil
	}
	if err := s.sendSelectedActorAppearanceRefresh(session, eventPrefix, "ack_then_mode0_then_mode1"); err != nil {
		return err
	}
	if err := s.sendSelectedActorCurrentSceneSkillInfo(session, "current_exe_"+eventPrefix+"_after_mode1"); err != nil {
		return err
	}
	if err := s.sendSelectedActorUserStateRefresh(session, "current_exe_"+eventPrefix+"_after_skill_rebind"); err != nil {
		return err
	}
	return nil
}

// currentGrowupTransitionNeedsDungeonOnlySkill 判断转职成功后的刷新是否只需
// 副本内技能信息。真正的副本内（runtime 存在且尚未进入回城过渡）中途重建 actor
// 会破坏地图运行时，因此只重发 op19；一旦进入回城过渡（townReturnPending /
// townReturnOp24Sent，op24 已写出但 runtime 保留等待客户端城镇场景确认）或已
// 回城，则必须走完整的 mode0/mode1 + op19 + op357 刷新链路，否则客户端职业
// 信息会停留在旧职业（技能树由 op19 单独刷新，但角色外观/职业缺失）。
func currentGrowupTransitionNeedsDungeonOnlySkill(session *gameSession) bool {
	if session == nil {
		return false
	}
	session.dungeon.mu.Lock()
	defer session.dungeon.mu.Unlock()
	runtime := session.dungeon.runtime
	if runtime == nil {
		return false
	}
	return !runtime.townReturnPending && !runtime.townReturnOp24Sent
}

// decodeGrowupChangeTargetGrowType 从转职体验请求 body 解析目标职业分支。
// 90CN 客户端 body 为 14 字节（目标分支在 body[13]）；兼容 86 客户端
// 15 字节布局（目标分支在 body[14]，GrowupChangeRequestParser offset 14）。
func decodeGrowupChangeTargetGrowType(body []byte) (byte, error) {
	switch len(body) {
	case 14:
		return body[13], nil
	case 15:
		return body[14], nil
	default:
		return 0, errGrowupChangeBodyTooShort
	}
}

var errGrowupChangeBodyTooShort = errors.New("growup change request body must be 14 or 15 bytes")

// buildGrowupChangeAckBody 构造转职体验 ACK 负载（与 86 GrowupChangeAckBuilder
// 一致）：i32 result code + u8 changeCount。sendGameUpperSuccess 会补 01 前缀。
func buildGrowupChangeAckBody(resultCode int, changeCount int) []byte {
	body := make([]byte, 5)
	binary.LittleEndian.PutUint32(body[0:4], uint32(resultCode))
	body[4] = byte(changeCount)
	return body
}
