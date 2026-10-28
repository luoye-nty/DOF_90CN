package dnfbridge

import (
	"context"
	"errors"
	"math"
	"strconv"
	"strings"
	"time"

	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

// gmPostCommitPushTimeout 状态**已经落库之后**那次客户端推送的超时上限。
//
// 取 30 秒（比 createWriteTimeout 宽松得多）的原因见 gmPostCommitPushContext。
const gmPostCommitPushTimeout = 30 * time.Second

// gmPostCommitPushContext 给"状态已经落库之后的客户端推送"用的独立 context。
//
// ── 为什么不能直接用 HTTP 请求的 ctx（2026-09-28 修的真实线上问题）─────────────
// GM 接口的流程是"① 先把状态写进数据库 → ② 再把新状态推给在线客户端"。第 ② 步要排队
// 进游戏会话的事件循环（会话在跑副本/网络卡顿时队列会积压，几秒到十几秒都正常）。
//
// 而 eventloop 在取出事件时会先检查事件自带的 Context：
//
//	// internal/platform/eventloop/loop.go processEvent
//	if err := ctx.Err(); err != nil { ...; return }   // ← 直接丢弃，run() 根本不执行
//
// 于是"运维工具 8 秒超时断连 → 请求 ctx 被取消 → 排在队列里的推送事件被丢"成为必然。
// 后果是最难排查的一类故障：**钱在数据库里已经加了，接口日志也写着成功，但客户端
// 界面永远不刷新**（泡点反馈的"点券不实时刷新"就是这一条）。
//
// 所以：凡是"状态已经提交、推送只是通知"的调用，一律用本函数拿独立 context ——
// 客户端断不断开，都不该影响一次已经发生的变更被送达。
//
// 注意：**只有已经落库之后的推送才能用它**。参与事务/仓储读写的调用仍必须用请求 ctx，
// 否则客户端断开后服务端还会继续干活，那是另一种资源浪费。
func gmPostCommitPushContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), gmPostCommitPushTimeout)
}

// gmRefreshOnlineCharacter 把"已经落库的新状态"推给在线角色。
//
// **故意不接收 context.Context**（2026-09-28 修真实线上问题）：本函数是 7 个 GM 操作
// （SP/TP、等级、职业、副本权限、装备栏、黑钻、道具格刷新）统一的"提交后推送"出口。
// 以前它接收请求 ctx，于是运维工具 8 秒超时断连 → 请求 ctx 取消 → 排在游戏会话事件
// 循环里的推送事件被 eventloop 直接丢弃（它取出事件时先查 ctx.Err()），
// 结果是"改是改成功了、数据库也对，但客户端界面不刷新"。
//
// 去掉参数就是把这个契约写死：**这里只做通知，不属于任何一次请求的生命周期**。
// 详见 gmPostCommitPushContext。
func (s *Service) gmRefreshOnlineCharacter(
	characterID string,
	source string,
	send func(*gameSession) error,
) GMNativeRefreshResult {
	result := GMNativeRefreshResult{}
	numericID, err := strconv.ParseUint(strings.TrimSpace(characterID), 10, 16)
	if err != nil || numericID == 0 || numericID > math.MaxUint16 {
		return result
	}
	session, online := s.onlineGameSession(uint16(numericID))
	result.Online = online
	if !online {
		return result
	}
	if send == nil {
		result.RefreshError = "native client packet projection is unavailable"
		return result
	}
	pushCtx, pushCancel := gmPostCommitPushContext()
	defer pushCancel()
	err = s.callGameSession(pushCtx, session, source, func() error {
		if session.selectedCharacterID != uint16(numericID) {
			return errors.New("selected character changed before native GM state projection")
		}
		return send(session)
	})
	if err != nil {
		result.RefreshError = err.Error()
		return result
	}
	result.Refreshed = true
	if player, found := s.onlinePlayers.PlayerForCharacter(uint16(numericID)); found {
		s.notifyCurrentUnitedFriendPresence(
			player.Name,
			currentFriendPresence{ChannelID: uint16(session.channel.ID)},
			source+"_friend_absolute_refresh",
		)
	}
	return result
}

func (s *Service) gmOnlineCharacterInDungeon(characterID string) bool {
	numericID, err := strconv.ParseUint(strings.TrimSpace(characterID), 10, 16)
	if err != nil || numericID == 0 || numericID > math.MaxUint16 {
		return false
	}
	session, online := s.onlineGameSession(uint16(numericID))
	if !online {
		return false
	}
	session.dungeon.mu.Lock()
	defer session.dungeon.mu.Unlock()
	return session.dungeon.runtime != nil
}

func (s *Service) sendGMLevelPostCommitState(session *gameSession) error {
	return s.sendSelectedCharacterProgressionPostCommitState(session, "gm_admin_level")
}

func (s *Service) sendSelectedCharacterProgressionPostCommitState(session *gameSession, source string) error {
	if err := s.sendSelectedCharacterNativeProgressionState(session, source); err != nil {
		return err
	}
	if err := s.sendSelectedActorCurrentSceneSkillInfo(session, source+"_after_op37"); err != nil {
		return err
	}
	return s.sendGMPersistedQuestPostActionState(session, source+"_after_op37_op19")
}

func (s *Service) sendSelectedCharacterNativeProgressionState(session *gameSession, source string) error {
	if session == nil || session.selectedCharacterID == 0 {
		return errors.New("selected character is unavailable for level projection")
	}
	if strings.TrimSpace(source) == "" {
		source = "character_progression"
	}
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.Character == nil || repositories.Skill == nil {
		return ErrGMRepositoryUnavailable
	}
	characterID := strconv.Itoa(int(session.selectedCharacterID))
	character, found, err := repositories.Character.Load(ctx, characterID)
	if err != nil {
		return err
	}
	if !found {
		return ErrGMCharacterNotFound
	}
	skill, found, err := repositories.Skill.Load(ctx, characterID)
	if err != nil {
		return err
	}
	if !found {
		return errors.New("committed level has no skill-point ledger")
	}
	// Current NoPack op418 (sub_1D197C0) is the native level-up owner: its
	// success callback adds the returned EXP and SP/TP deltas before this
	// absolute projection runs.  A level change must also update the already
	// bound town actor's raw47 level byte.  Mode0 is the proved in-place actor
	// refresh; do not send full mode1 here because it recreates equipment/actor
	// state and can block the next town movement request.
	if currentTownSelfActorAppearanceReady(session) {
		if err := s.sendSelectedActorMode0AppearanceRefresh(
			session,
			source,
			"native_op418_delta_then_same_owner_mode0_level_then_op37_absolute_progression",
		); err != nil {
			return err
		}
	}

	// SkillRecord.Points is the committed per-character baseline advanced by
	// the EXP owner.  Alternate-tree ledgers are synchronized immediately by
	// the following op19 builder.  Reading the active tree here would publish
	// its pre-sync value (observed live as SP=0 after op418 granted SP=30) and
	// overwrite the native callback's point update.
	points := skill.Points
	body := buildCurrentFinishLoadingCharacterStateBody(character, points)
	if len(body) != currentFinishLoadingCharacterStateBodySize {
		return errors.New("committed level produced an invalid current-client character state")
	}
	if err := s.sendGameUpperRawClass(session, currentDungeonCharacterStateMsgID, body, 0); err != nil {
		return err
	}
	s.logGameEvent(session, "game-upper-character-progression-op37-send",
		"char_id", session.selectedCharacterID,
		"level", character.Level,
		"remaining_sp", points.RemainingSP,
		"remaining_tp", points.RemainingTP,
		"body_len", len(body),
		"sequence", "native_op418_delta_then_optional_town_mode0_level_then_op37_then_op19_quests",
		"body_source", source+"_post_commit_character_state")
	return nil
}

func (s *Service) sendGMPersistedQuestPostActionState(session *gameSession, source string) error {
	if err := s.sendCurrentActiveQuestSnapshotForSession(session, source+"_active_op574"); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	body, err := s.buildCurrentClearQuestListTransportBodyForSession(ctx, session, source+"_completed_op356")
	cancel()
	if err != nil {
		return err
	}
	if err := s.sendGameUpperFixed16Transport(
		session,
		currentClearQuestListMsgID,
		body,
		0,
		1,
		true,
		currentClearQuestListTransportCodec,
	); err != nil {
		return err
	}
	return s.sendCurrentAcceptableQuestListOnlyForSession(session, source+"_acceptable_op21")
}

func (s *Service) sendGMProfessionPostCommitState(session *gameSession) error {
	if session == nil || session.selectedCharacterID == 0 {
		return errors.New("selected character is unavailable for profession projection")
	}
	if s.gmOnlineCharacterInDungeon(strconv.Itoa(int(session.selectedCharacterID))) {
		return errors.New("profession projection is only valid in town")
	}
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.Character == nil {
		return ErrGMRepositoryUnavailable
	}
	characterID := strconv.Itoa(int(session.selectedCharacterID))
	character, found, err := repositories.Character.Load(ctx, characterID)
	if err != nil {
		return err
	}
	if !found || strings.TrimSpace(character.AccountID) == "" {
		return ErrGMCharacterNotFound
	}
	var legacyRepo dnfrepo.LegacyUserInfoRepository
	legacyRepo = repositories.LegacyUserInfo
	mode0 := s.buildCSharpSelectedUserInfoBody(
		ctx,
		session,
		legacyRepo,
		0,
		character,
		true,
		session.selectedCharacterID,
		character.Name,
	)
	if err := s.sendGameUpperRawClass(session, uint16(dnfenum.CmdPacketSetUDPIPPort), mode0, 0); err != nil {
		return err
	}
	summary := s.currentAccountAdventureGroupSummaryForPacket(ctx, session, character, true)
	mode1 := s.buildCurrentSelectedUserInfoMode1BodyWithAdventureLevelInContext(
		ctx,
		session,
		legacyRepo,
		character,
		true,
		session.selectedCharacterID,
		uint32(summary.ManageLevel),
		currentTownActorOwnerContext(session),
	)
	if err := s.sendGameUpperRawClass(session, uint16(dnfenum.CmdPacketSetUDPIPPort), mode1, 0); err != nil {
		return err
	}
	s.logGameEvent(session, "game-upper-gm-profession-actor-rebind-send",
		"char_id", session.selectedCharacterID,
		"grow_type", character.Stats["grow_type"],
		"sequence", "mode0_mode1_op19_quest_snapshots",
		"mode0_body_len", len(mode0),
		"mode1_body_len", len(mode1),
		"body_source", "repository_pvf_post_profession_commit_native_actor_rebind")
	if err := s.sendSelectedActorCurrentSceneSkillInfo(session, "gm_admin_profession_after_mode0_mode1"); err != nil {
		return err
	}
	return s.sendGMPersistedQuestPostActionState(session, "gm_admin_profession_after_actor_skill_rebind")
}

func (s *Service) sendGMDungeonPermissionPostCommitState(ctx context.Context, session *gameSession) error {
	entries, err := s.currentDungeonPermissionSnapshotEntries(ctx, session)
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		return errors.New("persisted dungeon permissions have no current-PVF client projection")
	}
	body := buildCurrentDungeonResourceStateEntriesBody(entries)
	s.logGameEvent(session, "game-upper-gm-dungeon-permission-op5-snapshot-send",
		"char_id", session.selectedCharacterID,
		"msg_id", currentDungeonResourceStateMsgID,
		"classification", 0,
		"row_count", len(entries),
		"body_len", len(body),
		"body_source", "current_exe_sub_1D37AC0_u16_count_u32_key_u8_state")
	return s.sendGameUpperRawClass(session, currentDungeonResourceStateMsgID, body, 0)
}

func (s *Service) sendGMEquipSlotPostCommitState(session *gameSession) error {
	if session == nil || session.selectedCharacterID == 0 {
		return errors.New("selected character is unavailable for equipment-slot projection")
	}
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.Character == nil {
		return ErrGMRepositoryUnavailable
	}
	characterID := strconv.Itoa(int(session.selectedCharacterID))
	character, found, err := repositories.Character.Load(ctx, characterID)
	if err != nil {
		return err
	}
	if !found {
		return ErrGMCharacterNotFound
	}
	var legacyRepo dnfrepo.LegacyUserInfoRepository
	legacyRepo = repositories.LegacyUserInfo
	summary := s.currentAccountAdventureGroupSummaryForPacket(ctx, session, character, true)
	body := s.buildCurrentSelectedUserInfoMode1BodyWithAdventureLevelInContext(
		ctx,
		session,
		legacyRepo,
		character,
		true,
		session.selectedCharacterID,
		uint32(summary.ManageLevel),
		currentTownActorOwnerContext(session),
	)
	if err := s.sendGameUpperRawClass(session, uint16(dnfenum.CmdPacketSetUDPIPPort), body, 0); err != nil {
		return err
	}
	s.logGameEvent(session, "game-upper-gm-equip-slot-mode1-send",
		"char_id", session.selectedCharacterID,
		"ex_equip_slot_stat", character.Stats["ex_equip_slot_stat"],
		"body_len", len(body),
		"mode3_suppressed", true,
		"body_source", "quest_finish_slot_expansion_proved_mode1_carrier")
	return nil
}
