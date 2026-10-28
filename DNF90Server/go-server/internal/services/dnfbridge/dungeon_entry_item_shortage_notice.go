package dnfbridge

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// reason 17 正是客户端自己的官方提示：sub_1CFC410(flag=body[0],reason=body[1]) →
// sub_1CFA790 的 254 路 switch → case 17 = 字符串 47707
// "因[%s]缺少入场材料，无法进入『%s』。"（参数是角色名与副本名，客户端自己取）。
//
// 曾经试过改用 op443 来避开 op16 失败后那套状态迁移 —— 错：443 在客户端协议里是
// ENUM_CMDPACKET_REGISTER_QUICK_PARTY（快速组队应答），客户端 sub_1CFC4D0 会弹两下
// 快速组队的失败窗（现场 2026-09-24 13:17:15 用户看到"两个提示窗口一闪而过"，官方
// 那条提示反而没了）。所以仍然只走 op16，客户端那套"失败后回城"的反射由
// suppressCurrentDungeonShortageEscape 拦一次。
func (s *Service) sendCurrentDungeonEntryItemMissingFailure(
	session *gameSession,
	runtime *runtimeDungeonState,
	cause error,
) error {
	s.logCurrentDungeonEntryItemShortage(session, cause)
	s.markCurrentDungeonEntryItemRefused(session)
	return s.sendSelectDungeonFailure(session, currentDungeonEntryItemFailureReason(runtime))
}

// sendCurrentDungeonEntryAdmissionPlanFailure covers the other half of the
// admission-item stage: the plan itself could not be built, so there is no
// shortage to name. It must still answer -- a select request without any
// reply parks the client on the selector page forever.
func (s *Service) sendCurrentDungeonEntryAdmissionPlanFailure(
	session *gameSession,
	runtime *runtimeDungeonState,
	cause error,
) error {
	dungeonID := int64(0)
	if runtime != nil {
		dungeonID = runtime.Dungeon.ID
	}
	s.logGameEvent(session, "game-dungeon-entry-item-shortage",
		"char_id", sessionSelectedCharacterID(session),
		"dungeon_id", dungeonID,
		"reason", "entry_admission_plan_unresolved",
		"notice_text", s.currentDungeonEntryItemShortageNoticeText(cause, session),
		"error", cause)
	s.markCurrentDungeonEntryItemRefused(session)
	return s.sendSelectDungeonFailure(session, currentDungeonEntryItemFailureReason(runtime))
}

// currentDungeonEntryItemFailureReason picks the client's own text for an
// admission-item failure: hell entries get the invitation wording (reason 27)
// and everything else keeps the generic reason 17 that was verified live on
// 2026-09-24.
func currentDungeonEntryItemFailureReason(runtime *runtimeDungeonState) byte {
	if currentDungeonHellEntryRequiresInvitations(runtime) {
		return currentSelectDungeonHellInvitationMissingReason
	}
	return currentSelectDungeonEntryItemMissingReason
}

func (s *Service) logCurrentDungeonEntryItemShortage(session *gameSession, cause error) {
	s.logGameEvent(session, "game-dungeon-entry-item-shortage",
		"char_id", sessionSelectedCharacterID(session),
		"reason", "entry_item_missing",
		"notice_text", s.currentDungeonEntryItemShortageNoticeText(cause, session))
}

// currentDungeonShortageEscapeWindow 是"入场被拒之后客户端那次反射式返回城镇"的认定窗口：
// 客户端在自己的失败迁移里紧接着就发 bodyless op132（实测 6~70ms），所以这个窗口只取几秒，
// 之后玩家真的点"返回城镇"就照旧回城。
const currentDungeonShortageEscapeWindow = 3 * time.Second

func (s *Service) markCurrentDungeonEntryItemRefused(session *gameSession) {
	if session == nil {
		return
	}
	session.dungeon.mu.Lock()
	session.dungeonEntryItemRefusedAt = s.gameplayNow()
	session.dungeon.mu.Unlock()
}

// suppressCurrentDungeonShortageEscape 吞掉"入场被拒"之后客户端那一发反射式 op132：
// 不发城镇切换，改为把"选择副本"界面再补一遍，玩家留在界面上看着提示。
// 返回 true 表示这一发已经被当成反射处理掉了。
func (s *Service) suppressCurrentDungeonShortageEscape(session *gameSession, source string) bool {
	if session == nil {
		return false
	}
	session.dungeon.mu.Lock()
	refusedAt := session.dungeonEntryItemRefusedAt
	session.dungeonEntryItemRefusedAt = time.Time{}
	session.dungeon.mu.Unlock()
	if refusedAt.IsZero() {
		return false
	}
	if elapsed := s.gameplayNow().Sub(refusedAt); elapsed > currentDungeonShortageEscapeWindow {
		return false
	}
	s.logGameEvent(session, "game-dungeon-back-to-village-shortage-escape-suppressed",
		"char_id", sessionSelectedCharacterID(session),
		"source", source,
		"reason", "client_reflex_after_entry_item_refusal_is_not_a_player_return",
		"fallback", "reproject_dungeon_select_page")
	if err := s.restoreCurrentDungeonSelectorAfterEntryFailure(
		session,
		"current_exe_op132_entry_item_refusal_reflex_suppressed",
	); err != nil {
		s.logGameEvent(session, "game-dungeon-back-to-village-shortage-escape-reproject-blocked",
			"char_id", sessionSelectedCharacterID(session),
			"source", source,
			"error", err)
	}
	return true
}

// currentDungeonEntryItemShortageNoticeText names the item from the runtime
// PVF when the catalog can resolve it and falls back to a generic label; the
// admission plan is generic over hell invitations, tower invitations and
// rift passes, so the text must not hard-code any one of them.
func (s *Service) currentDungeonEntryItemShortageNoticeText(cause error, session *gameSession) string {
	var structured currentDungeonEntryItemShortage
	if !errors.As(cause, &structured) {
		return "入场道具不足。请先准备该地下城要求的邀请函或通行证。"
	}
	shortage, owner, name := s.currentDungeonEntryItemNoticeIdentity(cause, session)
	if shortage.Required > 0 {
		return fmt.Sprintf("%s%s不足：需要%d个，当前%d个。请先兑换该道具或准备对应通行证。",
			owner, name, shortage.Required, shortage.Available)
	}
	return owner + name + "不足。请先兑换该道具或准备对应通行证。"
}

func (s *Service) currentDungeonEntryItemNoticeIdentity(
	cause error,
	session *gameSession,
) (currentDungeonEntryItemShortage, string, string) {
	var shortage currentDungeonEntryItemShortage
	if !errors.As(cause, &shortage) {
		return shortage, "", "入场道具"
	}
	owner := ""
	if session != nil && shortage.CharacterID != 0 && shortage.CharacterID != session.selectedCharacterID {
		owner = "队友"
	}
	name := "入场道具"
	if shortage.ItemID > 0 && shortage.ItemID <= 0xFFFFFFFF {
		if catalog, err := s.currentPVFItemCatalog(); err == nil && catalog != nil {
			if definition, resolveErr := catalog.ResolveItem(uint32(shortage.ItemID)); resolveErr == nil {
				if resolved := strings.TrimSpace(definition.Name); resolved != "" {
					name = resolved
				}
			}
		}
	}
	return shortage, owner, name
}
