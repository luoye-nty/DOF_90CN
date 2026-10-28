package dnfbridge

import (
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"strconv"

	"longheng.io/server/internal/modules/dnf/experiencebonus"
)

// These are current-client class0 notification IDs. They must not be taken
// from the similarly numbered C2S CmdPacket enum entries.
const (
	currentLoadEffectItemInfoMsgID          uint16 = 174
	currentExperienceSourceKeyNormalElixir  uint32 = 180
	currentExperienceSourceKeyAncientElixir uint32 = 181
)

// sendCurrentExperienceConsumableBuffProjection restores the original .stk
// effect-item after login/reselect. First use is handled locally by op44; op174
// recreates the same item-owned effect with its durable remaining duration.
func (s *Service) sendCurrentExperienceConsumableBuffProjection(session *gameSession, source string) error {
	if s == nil || session == nil || session.selectedCharacterID == 0 || session.experienceConsumableProjectionSent {
		return nil
	}
	accountID := s.accountIDForSession(session)
	if accountID == "" {
		return nil
	}
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.Account == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	account, found, err := repositories.Account.Load(ctx, accountID)
	if err != nil {
		return fmt.Errorf("load experience consumable effect for HUD restore: %w", err)
	}
	if !found {
		return nil
	}
	now := s.gameplayNow().UTC()
	effect, active := experiencebonus.ActiveForCharacter(
		account,
		strconv.FormatUint(uint64(session.selectedCharacterID), 10),
		now,
	)
	if !active {
		return nil
	}
	if effect.ItemID <= 0 || effect.ItemID > math.MaxUint32 {
		s.logGameEvent(session, "game-experience-source-restore-skipped",
			"source", source,
			"char_id", session.selectedCharacterID,
			"item_id", effect.ItemID,
			"reason", "invalid_effect_item_id")
		return nil
	}
	remaining := int64(math.Ceil(effect.ExpireAt.Sub(now).Seconds()))
	if remaining < 1 {
		return nil
	}
	if remaining > math.MaxUint32 {
		remaining = math.MaxUint32
	}
	if err := s.sendGameUpperRawClass(
		session,
		currentLoadEffectItemInfoMsgID,
		buildCurrentExperienceEffectItemInfoBody(uint32(effect.ItemID), uint32(remaining)),
		0,
	); err != nil {
		return err
	}
	session.experienceConsumableProjectionSent = true
	s.logPacketEvent("game-experience-effect-item-restore-send",
		"conn_id", session.connID,
		"char_id", session.selectedCharacterID,
		"source", source,
		"item_id", effect.ItemID,
		"remaining_seconds", remaining,
		"sequence", "class0/op174-load-effect-item-info")
	return nil
}

func currentExperienceConsumableCanonicalSourceKey(bonusPercent int64) (uint32, bool) {
	switch bonusPercent {
	case 100:
		return currentExperienceSourceKeyNormalElixir, true
	case 200:
		return currentExperienceSourceKeyAncientElixir, true
	}
	return 0, false
}

func buildCurrentExperienceEffectItemInfoBody(itemID, remainingSeconds uint32) []byte {
	return buildCurrentEffectItemInfoBody(itemID, remainingSeconds)
}

// buildCurrentEffectItemInfoBody 是 class0/op174（"load effect item info"）的记录体：
// {u8 1, u32 itemID, u32 remainingSeconds}。
//
// 经验药水（经验加成）与城镇增益药剂（史诗Buff药剂一族）共用它：客户端按 itemID
// 去 etc/variousbuffstackablelist.etc 查 buff、再按 buff id 去 etc/variousbufflist.etc
// 取 [icon index] 与 [tooltip text]，所以这一发就是"图标出现"的前提。
func buildCurrentEffectItemInfoBody(itemID, remainingSeconds uint32) []byte {
	body := make([]byte, 9)
	body[0] = 1
	binary.LittleEndian.PutUint32(body[1:5], itemID)
	binary.LittleEndian.PutUint32(body[5:9], remainingSeconds)
	return body
}
