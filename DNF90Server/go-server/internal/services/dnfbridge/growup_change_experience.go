package dnfbridge

import (
	"context"
	"encoding/hex"
	"strconv"
	"time"

	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfprofession "longheng.io/server/internal/modules/dnf/profession"
)

// handleCurrentGrowtypeExperience 处理转职体验请求 REQUEST_ITEM_GROWTYPE_EXPERIENCE
// （legacy cmd=1 type=678）。90CN 客户端"转职体验"界面的"体验"按钮发出该请求，
// body 布局与 RE_GROWUP_CHANGE 一致（最后一个字节为目标职业分支）。体验免费：
// 不扣金币、不递增正式转职次数（growup_change_count）。
func (s *Service) handleCurrentGrowtypeExperience(session *gameSession, body []byte) error {
	if session == nil || session.selectedCharacterID == 0 {
		return nil
	}
	characterID := strconv.FormatUint(uint64(session.selectedCharacterID), 10)
	target, err := decodeGrowupChangeTargetGrowType(body)
	if err != nil {
		s.logPacketEvent("growup-experience-request-ignored",
			"reason", "body_too_short",
			"body_len", len(body),
			"body_hex", hex.EncodeToString(body),
			"character_id", characterID)
		return nil
	}
	s.logPacketEvent("growup-experience-request",
		"character_id", characterID,
		"target_grow_type", target,
		"body_len", len(body),
		"body_hex", hex.EncodeToString(body))

	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.CharacterSettlement == nil {
		s.logPacketEvent("growup-experience-rejected", "reason", "repository_unavailable")
		return nil
	}
	profiles, skillCatalog, err := s.currentProfessionResources(ctx)
	if err != nil {
		s.logPacketEvent("growup-experience-rejected", "reason", "profession_resources_unavailable")
		return nil
	}
	result, err := dnfprofession.ApplyGrowupExperience(ctx, repositories, dnfprofession.GrowupChangeInput{
		AccountID:      s.accountIDForSession(session),
		CharacterID:    characterID,
		TargetGrowType: target,
		Profiles:       profiles,
		SkillCatalog:   skillCatalog,
		CommittedAt:    time.Now().UTC(),
	})
	if err != nil {
		s.logPacketEvent("growup-experience-rejected", "reason", "owner_failed", "error", err.Error())
		return nil
	}
	ackBody := buildGrowupChangeAckBody(result.ResultCode, result.NewChangeCount)
	if err := s.sendGameUpperSuccess(session, uint16(dnfenum.CmdPacketRequestItemGrowtypeExperience), ackBody); err != nil {
		return err
	}
	s.logPacketEvent("growup-experience-ack",
		"character_id", characterID,
		"result_code", result.ResultCode,
		"success", result.Success,
		"new_grow_type", result.NewGrowType,
		"previous_change_count", result.PreviousChangeCount,
		"new_change_count", result.NewChangeCount,
		"updated_gold", result.UpdatedGold)
	if !result.Success {
		return nil
	}
	return s.refreshAfterGrowupTransition(session, ctx, result, "growup_experience")
}
