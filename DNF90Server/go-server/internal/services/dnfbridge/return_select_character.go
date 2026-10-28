package dnfbridge

import (
	"time"

	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfproto "longheng.io/server/internal/modules/dnf/protocol"
	"longheng.io/server/internal/modules/dnf/worldmap"
)

// handleUpperReturnSelectCharacter closes the selected-character lifecycle and
// acknowledges op7. The current client switches to the character-select module
// from the ACK handler, then issues op8 to request a fresh repository roster.
func (s *Service) handleUpperReturnSelectCharacter(session *gameSession, body []byte) error {
	if s == nil || session == nil {
		return nil
	}
	if len(body) != 0 {
		s.logGameEvent(session, "game-upper-return-select-character-blocked",
			"body_len", len(body),
			"expected_body_len", 0,
			"reason", "current_exe_op7_body_must_be_empty")
		return nil
	}

	previousCharacterID, abandonedDungeon := s.resetGameSessionForCharacterSelect(session)
	if previousCharacterID != 0 {
		// Remove the old local actor before op7 switches the client into the
		// selector. Otherwise the client keeps the previous scene root visible
		// until the next op4 scene bootstrap arrives.
		if err := s.sendCurrentSceneFixedClass0Packet(
			session,
			currentTownUserLeaveMsgID,
			buildTownUserLeaveBody(previousCharacterID),
			"return_select_character_before_op7",
		); err != nil {
			return err
		}
		session.townMu.Lock()
		session.returnSelectRetainedActorRemoved = true
		session.townMu.Unlock()
		s.logGameEvent(session, "game-return-select-retained-actor-remove-send",
			"char_id", previousCharacterID,
			"msg_id", currentTownUserLeaveMsgID,
			"reason", "same_character_reselect_removes_existing_town_actor_before_op7")
	}
	s.logGameEvent(session, "game-upper-return-select-character-send",
		"previous_char_id", previousCharacterID,
		"dungeon_runtime_abandoned", abandonedDungeon,
		"sequence", "class1_op7_success_then_wait_for_client_op8")
	// The current client consumes op7 as a bodyless class-1 transition ACK.
	// The generic success envelope would append a status byte and immediately
	// make the client reopen the selector, causing a second town load.
	return s.sendGameUpperRawClass(session, uint16(dnfenum.CmdPacketReturnSelectCharacter), nil, dnfproto.DefaultChannelClassification)
}

// returnToCharacterSelectAfterRenameCard performs the server-owned return that the
// rename-card flow needs.
//
// The 86-generation reference server does this itself: its GameProtocolHandler
// runs ReturnToCharacterSelectionAfterRenameAsync right after a successful
// USE_RENAME_CARD (and after CHANGE_CHARAC_NAME), which retires the town session
// and pushes the character list. Our client does not ask on its own - the bag-card
// flow only switches to the selector module when it receives the class-1 op7 ACK
// (see handleUpperReturnSelectCharacter) - so without this push the card was
// consumed, the pending-rename state was stored, and the player just stayed in
// town with nothing on screen, having to click 返回选择角色界面 by hand.
//
// This deliberately reuses handleUpperReturnSelectCharacter instead of open-coding
// the packets, so a server-initiated return and a client-requested one can never
// drift apart.
func (s *Service) returnToCharacterSelectAfterRenameCard(session *gameSession) error {
	if s == nil || session == nil {
		return nil
	}
	if err := s.handleUpperReturnSelectCharacter(session, nil); err != nil {
		s.logGameEvent(session, "game-character-rename-card-return-select-failed", "error", err)
		return err
	}
	s.logGameEvent(session, "game-character-rename-card-return-select",
		"sequence", "op14_item_update_then_op509_ack_then_op6_then_class1_op7",
		"reason", "server_owned_return_after_use_rename_card_86_reference_behaviour")
	return nil
}

func (s *Service) resetGameSessionForCharacterSelect(session *gameSession) (uint16, bool) {
	if s == nil || session == nil {
		return 0, false
	}
	// Clear town presence before selectedCharacterID is reset below. Once the
	// ID is zero, a later transport disconnect cannot identify this actor.
	// 超时空：回选角也在这个边界上把该角色被确认过的城镇坐标记进名册
	// (captureRuntimeFiendWarRejoinPosition)，否则重连时拿不到他最后站着的位置。
	s.captureRuntimeFiendWarRejoinPosition(session)
	s.closeCurrentPVPSession(session)
	s.cleanupOnlinePlayer(session)
	s.closeCurrentExpertJobStoreSession(session, true)
	s.closeCurrentPrivateStoreSession(session, true)
	s.detachRuntimePartySession(session, "return_select_character")
	// 回选角界面 = 该角色退队：小退和关游戏同规则，编制跟着普通队伍一起清
	// （运营规则 2026-09-27：「小退到选择角色界面 一律按退队处理」）。
	s.unassignRuntimeRaidSquadOnSessionLeave(session.selectedCharacterID, "return_select_character")
	s.stopCurrentSpendTimeClock(session, "return_select_character")
	s.stopCurrentPetGrowthClock(session, "return_select_character")
	s.stopCurrentDailyFatigueReset(session, "return_select_character")
	// 回选角界面 = 该角色下线：结清虚弱在线窗口，否则在界面上挂着也会继续掉虚弱。
	s.stopCurrentWeaknessOnlineClock(session)

	abandonedDungeon := false
	session.dungeon.mu.Lock()
	if runtime := session.dungeon.runtime; runtime != nil {
		if runtime.Session != nil && runtime.Session.Snapshot().Run.Status == worldmap.DungeonRunActive {
			if err := runtime.Session.Abandon(); err != nil {
				s.logGameEvent(session, "game-upper-return-select-character-dungeon-abandon-failed", "error", err)
			} else {
				abandonedDungeon = true
			}
		}
		if err := s.notifyRuntimeRaidDungeonAbandonedLocked(session, runtime); err != nil {
			s.logGameEvent(session, "game-upper-return-select-character-raid-abandon-publish-failed", "error", err)
		}
	}
	// Death Tower and compatibility helpers can exist without a worldmap
	// runtime, so retire the whole selected-character dungeon scope even when
	// runtime is nil.
	s.retireCurrentDungeonRuntimeLocked(session, session.dungeon.runtime, "return_select_character")
	s.mu.Lock()
	previousCharacterID := session.selectedCharacterID
	if previousCharacterID != 0 && s.gameSessions[previousCharacterID] == session {
		delete(s.gameSessions, previousCharacterID)
	}
	if previousCharacterID != 0 {
		advanceGameSessionCharacterGeneration(session)
	}
	session.selectedCharacterID = 0
	// The collection-box selection is deliberately retained: it is keyed by
	// character ID (collectboxSessionSelected), so a same-character re-entry
	// restores the box the player was filling instead of the login default,
	// which is what silently dropped its attributes until the box was clicked
	// again. Switching characters clears the pair in game_sessions.go.
	session.collectboxInitializationSent = false
	session.collectboxSceneProjectionSent = false
	session.collectboxSelectionRefreshSent = false
	session.experienceConsumableProjectionSent = false
	session.itemCooltimeProjectionSent = false
	// 城镇增益药剂（史诗Buff药剂一族）的 buff 图标也必须在这里复位：回选人时客户端
	// 已经把 buff 栏清空了，而"每个选中角色只补发一次"的标记如果留着，小退再上时
	// 恢复补发会被跳过 —— 现场 2026-10-03 服主报的"小退再上图标就没了"正是这个原因
	//（同族换角色由 game_sessions.go 清掉整个会话，不受影响）。
	session.statChangePotionBuffProjectionSent = false
	// 公会 BUFF（Guild_Status / Guild_Status1）同理：客户端回到选角界面时已经把
	// buff 栏清空了，而"这两个 buff 已经投影过"的记忆如果留着，重新进游戏就不会
	// 再补发，HUD 上永远缺这一格。characterID 归零即让下一次投影强制重发。
	session.guildBuffProjection.mu.Lock()
	session.guildBuffProjection.characterID = 0
	session.guildBuffProjection.mu.Unlock()
	// 到期摘图标的定时器同理作废：换角色/重进之后要按新会话重新武装
	//（回调里还有 generation + 角色 双重校验，作废后旧回调不会误摘图标）。
	session.statChangePotionTimerMu.Lock()
	session.statChangePotionTimerActive = false
	session.statChangePotionTimerName = ""
	session.statChangePotionTimerDueAt = time.Time{}
	session.statChangePotionTimerGeneration = nextCurrentStatChangePotionTimerGeneration(
		session.statChangePotionTimerGeneration)
	session.statChangePotionTimerCharacterID = 0
	session.statChangePotionTimerMu.Unlock()
	s.mu.Unlock()
	session.dungeon.mu.Unlock()

	session.darkKnightCombo.reset()
	session.customAbilityConverter.reset()
	resetDungeonEntrySceneGates(session)
	markReturnSelectTownReentry(session, previousCharacterID)
	clearCurrentDungeonSelectContext(session)
	clearCurrentTownSelectorOrigin(session)
	session.selectPreviewObjectStateSent = false
	session.raidDirectoryReadySent = false
	resetRuntimeRaidSessionProjection(session)
	resetCurrentSelectCharacterInit(session)
	session.rosterRequested = false
	session.channelReconnect = false
	session.channelReconnectNewCharacter = false
	session.townActorOwnerChannel = currentConnectionTownActorOwnerContext(session)
	return previousCharacterID, abandonedDungeon
}
