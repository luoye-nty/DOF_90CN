// game_sessions.go 维护 DNF game 连接到在线角色的短生命周期索引。
// 这里的状态只用于旧客户端实时回包投影，不是队伍、角色或玩法的持久 owner。
package dnfbridge

import (
	"time"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	"longheng.io/server/internal/modules/dnf/worldmap"
)

type boundGameSessionCharacter struct {
	session    *gameSession
	character  uint16
	generation uint64
}

func (s *Service) bindGameSessionCharacter(session *gameSession, charID uint16) {
	if s == nil || session == nil || charID == 0 {
		return
	}
	characterChanged := session.selectedCharacterID != charID
	if characterChanged {
		// 换角色等于换战场上下文：先把超时空的坐标与遇敌去重记下来/清掉，
		// 再把面板增量基线归零 —— 否则新角色会拿着旧角色的基线算差异，
		// 发出一批不存在的"变化行"。
		s.captureRuntimeFiendWarRejoinPosition(session)
		session.fiendWarEncounterRaidKey, session.fiendWarEncounterArea = 0, 0
		session.fiendWarEncounterAt = time.Time{}
		session.fiendWarBoardMu.Lock()
		session.fiendWarBoard = runtimeRaidState{}
		session.fiendWarBoardMu.Unlock()
		session.darkKnightCombo.reset()
		session.customAbilityConverter.reset()
	}
	if oldID := session.selectedCharacterID; oldID != 0 && oldID != charID {
		s.stopCurrentDailyFatigueReset(session, "selected_character_changed")
		// 换角色时撤掉挂在该会话上的婚姻邀请等待（见 clearWeddingInvitations）。
		// 必须在拿 session.dungeon.mu / s.mu 之前调用：它自己要拿 s.weddingMu，
		// 而全局锁序是 weddingMu 先于 mu。
		s.clearWeddingInvitations(session)
		s.closeCurrentGuildCreateSession(session)
		s.closeCurrentPVPSession(session)
		s.cleanupOnlinePlayer(session)
		s.closeCurrentExpertJobStoreSession(session, true)
		s.closeCurrentPrivateStoreSession(session, true)
		s.detachOnlineItemTrade(session, "selected_character_changed")
		s.detachRuntimePartySession(session, "selected_character_changed")
		// Switching characters leaves whatever party the previous character was
		// in, so its attack-squad label goes with it (operator rule 2026-09-27:
		// every path that detaches the ordinary party drops the 编制 too).
		s.unassignRuntimeRaidSquadOnSessionLeave(oldID, "selected_character_changed")
		s.stopCurrentSpendTimeClock(session, "selected_character_changed")
		s.stopCurrentPetGrowthClock(session, "selected_character_changed")
	}
	session.dungeon.mu.Lock()
	if oldID := session.selectedCharacterID; oldID != 0 && oldID != charID {
		runtime := session.dungeon.runtime
		if runtime != nil && runtime.Session != nil && runtime.Session.Snapshot().Run.Status == worldmap.DungeonRunActive {
			if err := runtime.Session.Abandon(); err != nil {
				s.logGameEvent(session, "game-selected-character-change-dungeon-abandon-failed", "error", err)
			}
		}
		if runtime != nil {
			if err := s.notifyRuntimeRaidDungeonAbandonedLocked(session, runtime); err != nil {
				s.logGameEvent(session, "game-selected-character-change-raid-abandon-publish-failed", "error", err)
			}
		}
		s.retireCurrentDungeonRuntimeLocked(session, runtime, "selected_character_changed")
	}
	resident := (*gameSession)(nil)
	previousChannelID, previousChannelType, previousChannelKnown :=
		s.observeCurrentClientSelectedChannel(session, charID)
	if s.onlinePlayers != nil {
		resident = s.onlinePlayers.SessionForCharacter(charID)
		if resident != nil && resident != session {
			previousChannelID = resident.channel.ID
			previousChannelType = resident.channel.Type
			previousChannelKnown = true
		}
		if resident != nil && resident != session && session.channelReconnect {
			// A channel-reconnect session binds before its op13 inventory
			// bootstrap. Settle and stop the resident clock synchronously here,
			// so any threshold reward is already durable and therefore included
			// in that new bootstrap. Waiting for the old socket's later unbind
			// would allow the reward commit to race after op13 and remain visually
			// absent until another login. The party itself is not carried over:
			// see the replacement-session leave below.
			s.stopCurrentSpendTimeClock(resident, "replacement_game_session_bound_before_inventory_bootstrap")
		}
	}
	if session.channelReconnect && previousChannelKnown &&
		isCurrentTeamChannelType(previousChannelType) &&
		!isCurrentTeamChannelType(session.channel.Type) {
		session.spendTime.mu.Lock()
		session.spendTime.eventInfoReplayAfterTeamReturn = true
		session.spendTime.mu.Unlock()
		s.logGameEvent(session, "game-team-channel-return-op108-replay-armed",
			"char_id", charID,
			"previous_channel_id", previousChannelID,
			"previous_channel_type", previousChannelType,
			"target_channel_id", session.channel.ID,
			"target_channel_type", session.channel.Type)
	}
	// 全局锁序：weddingMu 必须在 mu 之前拿、在 mu 之后放。婚姻事务
	// （wedding_runtime.go 等）只拿 weddingMu，而会话绑定会同时碰两者；
	// 顺序写反就会和婚姻手势互相等待。
	s.weddingMu.Lock()
	s.mu.Lock()
	if s.gameSessions == nil {
		s.gameSessions = make(map[uint16]*gameSession)
	}
	if oldID := session.selectedCharacterID; oldID != 0 && oldID != charID {
		if s.gameSessions[oldID] == session {
			delete(s.gameSessions, oldID)
		}
	}
	previous := s.gameSessions[charID]
	if characterChanged || session.characterGeneration == 0 ||
		(previous != nil && previous != session && session.characterGeneration == previous.characterGeneration) {
		s.allocateGameSessionCharacterGenerationLocked(session, previous)
	} else if session.characterGeneration > s.nextGameSessionGeneration {
		s.nextGameSessionGeneration = session.characterGeneration
	}
	if characterChanged {
		session.blackDiamondMu.Lock()
		session.blackDiamondExpiresAt = time.Time{}
		session.blackDiamondMu.Unlock()
		clearCurrentGameOptionRestoreSnapshot(session)
		clearCurrentCharacterOptionRestoreSnapshot(session)
		session.expertJobInfoCharacterID = 0
		session.raidAvailabilitySent = false
		session.raidDirectoryReadySent = false
		session.raidDirectory = runtimeRaidDirectorySubscription{}
		session.raidBrowseTarget = 0
		session.raidBrowseAt = time.Time{}
		session.raidBrowseMembers = 0
		resetRuntimeRaidSessionProjection(session)
		session.selectedCollectboxIndex = 0
		session.selectedCollectboxCharacterID = 0
		session.collectboxInitializationSent = false
		session.collectboxSceneProjectionSent = false
		session.collectboxSelectionRefreshSent = false
	}
	session.selectedCharacterID = charID
	if resident != nil && resident != session {
		if current := s.gameSessions[charID]; current == resident {
			s.mu.Unlock()
			s.weddingMu.Unlock()
			session.dungeon.mu.Unlock()
			// 超时空：被顶号/换频道也先把坐标记进名册再走退队路径。
			s.captureRuntimeFiendWarRejoinPosition(resident)
			// A replacement socket for a character the resident session still
			// holds is a party LEAVE, exactly like the one the code below
			// performs for a replacement that is no longer the map's holder: a
			// channel move and a takeover both end the connection that held the
			// party, so the member's cache is no longer handed over here
			// (operator rule 2026-09-27: 「还有更换频道 也算退队」,
			// 「被顶掉以后…此时也按退队处理」).
			s.leaveManagedRuntimePartyOnReplacementSession(resident, session, "resident_session_replaced")
			s.restoreCurrentPartyUDPEndpoint(session)
			return
		}
	}
	s.gameSessions[charID] = session
	s.mu.Unlock()
	s.weddingMu.Unlock()
	session.dungeon.mu.Unlock()
	s.restoreCurrentPartyUDPEndpoint(session)
	// 角色上线：开启"在线窗口"，虚弱恢复的计时只在这个窗口里前进
	// （离线期间窗口被 stop 清掉，计时因此锁定 —— 运营要求 2026-09-15）。
	s.startCurrentWeaknessOnlineClock(session)
	if previous != nil && previous != session {
		s.clearWeddingInvitations(previous)
		s.closeCurrentGuildCreateSession(previous)
		s.closeCurrentPVPSession(previous)
		s.releaseDisconnectedDungeonAttempt(previous, "replacement_game_session_bound")
		s.leaveManagedRuntimePartyOnReplacementSession(previous, session, "replacement_game_session_bound")
		s.restoreRuntimeRaidPresence(session, "replacement_game_session_bound")
		// 接管同一个角色的，可能是本客户端换频道，也可能是同账号的第二个客户端选了
		// 同一个角色。后者要把旧客户端请出去；靠进程号/登录身份区分，判不出来就放过。
		s.displaceReplacedAccountSession(previous, session)
	}
	// 一个账号只留一个在玩的客户端。必须放在绑定**之后**：换频道时新会话接管的是同一
	// 个角色，上面那段已经把注册持有者换成自己，旧会话查不到，天然不会误踢。
	//
	// 这里刻意用**原始的** session.accountID，不用 accountIDForSession：后者在账号未
	// 绑定时会回退到实例账号，而本机的实例账号就是一个真实账号，拿它当判据会让未鉴权
	// 的客户端踢掉真正的持有者。
	s.enforceCurrentAccountSingleSession(session, session.accountID)
}

// allocateGameSessionCharacterGenerationLocked assigns a nonce that is unique
// across the bridge process. RuntimePartyManager stores this generation without
// a session pointer, so a reconnect must never reuse the retired socket's
// initial value (both fresh sockets otherwise begin at generation one).
// s.mu must be held.
func (s *Service) allocateGameSessionCharacterGenerationLocked(session *gameSession, competing *gameSession) uint64 {
	if s == nil || session == nil {
		return 0
	}
	next := s.nextGameSessionGeneration
	if session.characterGeneration > next {
		next = session.characterGeneration
	}
	if competing != nil && competing.characterGeneration > next {
		next = competing.characterGeneration
	}
	next++
	if next == 0 {
		next++
	}
	s.nextGameSessionGeneration = next
	session.characterGeneration = next
	return next
}

func (s *Service) promoteResidentGameSession(session *gameSession, charID uint16) {
	if s == nil || session == nil || charID == 0 || session.selectedCharacterID != charID {
		return
	}
	// 同 bindGameSessionCharacter：weddingMu 先于 mu，后于 mu 释放。
	s.weddingMu.Lock()
	s.mu.Lock()
	if s.gameSessions == nil {
		s.gameSessions = make(map[uint16]*gameSession)
	}
	previous := s.gameSessions[charID]
	s.gameSessions[charID] = session
	s.mu.Unlock()
	s.weddingMu.Unlock()
	s.restoreCurrentPartyUDPEndpoint(session)
	if previous != nil && previous != session {
		s.clearWeddingInvitations(previous)
		s.closeCurrentGuildCreateSession(previous)
		s.closeCurrentPVPSession(previous)
		s.releaseDisconnectedDungeonAttempt(previous, "replacement_game_session_promoted")
		s.leaveManagedRuntimePartyOnReplacementSession(previous, session, "replacement_game_session_promoted")
		s.restoreRuntimeRaidPresence(session, "replacement_game_session_promoted")
	}
}

// leaveManagedRuntimePartyOnReplacementSession treats a replacement socket for
// the same character as a LEAVE of the party the old socket was holding.
//
// Operator rule (2026-09-27): 「被顶掉以后 客户端的连接就断开了 此时也按退队处理」 and
// 「还有更换频道 也算退队」. A replacement session is exactly one of those two
// events -- the same client moving to another channel, or a second client taking
// the character over (顶号) -- and both of them end the connection that held the
// party. The party therefore does not travel with the character: the member is
// removed under the OLD session's generation, the survivors are told their
// roster shrank, and the replacement comes up partyless.
//
// This replaces the earlier hand-over (RuntimePartyManager.RebindSession), which
// moved the membership onto the new generation. Two things must survive the
// change:
//
//   - The old socket's own teardown cannot perform this leave. By the time it
//     runs, s.gameSessions[char] is already the replacement, so
//     boundGameSessionCharacterCurrent answers false for the old identity and
//     detachRuntimePartySession returns without touching the roster. Dropping the
//     hand-over without doing the leave here would leave a member nobody can
//     remove -- teammates still see him, his own client has no party, and he has
//     no way to leave (the 2026-09-20 ghost-party class).
//   - Both session caches are cleared, so the replacement cannot inherit the
//     departed party through the legacy client-projection fallback
//     (bootstrapManagedRuntimePartyForSession) this function used to end on.
func (s *Service) leaveManagedRuntimePartyOnReplacementSession(previous *gameSession, replacement *gameSession, source string) {
	if s == nil || previous == nil || replacement == nil || previous == replacement {
		return
	}
	characterID := previous.selectedCharacterID
	if characterID == 0 || characterID != replacement.selectedCharacterID {
		return
	}
	generation := previous.characterGeneration
	previousState := runtimePartyStateSnapshot(previous)
	replacementState := runtimePartyStateSnapshot(replacement)
	storeRuntimePartyState(previous, alignedcmd.PartyState{})
	// The replacement is a fresh selection of this character; whatever its cache
	// holds predates the leave and must not outlive it.
	storeRuntimePartyState(replacement, alignedcmd.PartyState{})
	// 编制 is derived from the ordinary party, so it follows this leave too
	// (operator rule: 攻坚队被顶号/掉线 本来就要掉编制).
	s.unassignRuntimeRaidSquadOnSessionLeave(characterID, source)
	if replacementState.PartyID > 0 {
		// The replacement was already showing a party -- the resident takeover
		// used to hand the cache over -- and it is not a member of it. Give its
		// client the same empty projection a leave sends, so the window cannot
		// outlive the membership it was drawn from.
		if err := s.sendRuntimePartyMemberRemoved(replacement, uint16(replacementState.PartyID), alignedcmd.PartyState{}); err != nil {
			s.logWarn("dnfbridge deferred replacement session party clear",
				"char_id", characterID,
				"party_id", replacementState.PartyID,
				"source", source,
				"error", err)
		}
	}
	if previousState.PartyID <= 0 || generation == 0 {
		return
	}
	manager := s.runtimePartyManagerForService()
	if manager == nil {
		return
	}
	result := manager.Leave(characterID, generation)
	if !result.OK {
		// Nothing to wind down: the manager no longer holds this member (an
		// explicit op13 leave, a disband, a kick). The cache clears above are all
		// the replacement owed, and the survivors' own party -- relay included --
		// must not be touched from here.
		s.logGameEvent(replacement, "game-party-replacement-session-leave-skipped",
			"char_id", characterID,
			"party_id", previousState.PartyID,
			"prior_generation", generation,
			"source", source,
			"reason", result.Reason)
		return
	}
	if result.Disbanded || result.Party.ID == 0 {
		if err := s.publishRuntimePartyDisbandToSurvivors(
			nil,
			previousState,
			characterID,
			previous,
			source+"_disband",
		); err != nil {
			s.logWarn("dnfbridge deferred party disband during replacement session leave",
				"char_id", characterID,
				"party_id", previousState.PartyID,
				"error", err)
		}
		s.closeRuntimePartyUDPRelay(previousState.PartyID)
		s.logGameEvent(replacement, "game-party-replacement-session-left",
			"char_id", characterID,
			"party_id", previousState.PartyID,
			"remaining_party_id", 0,
			"source", source,
			"reason", "a_party_that_drops_to_one_member_is_not_a_party")
		return
	}
	nextState := s.publishRuntimePartyMembershipSnapshot(result.Party)
	if nextState.PartyID > 0 {
		s.syncRuntimePartyUDPRelay(nextState)
	} else {
		s.closeRuntimePartyUDPRelay(previousState.PartyID)
	}
	// A replacement socket's leave must not fail the survivor's write: see
	// swallowPeerFanoutWrite for the fan-out boundary.
	if err := s.broadcastRuntimePartyMemberRemoved(nil, previousState, nextState, characterID, previous); err != nil {
		s.logWarn("dnfbridge deferred party member removal during replacement session leave",
			"char_id", characterID,
			"party_id", nextState.PartyID,
			"error", err)
	}
	s.logGameEvent(replacement, "game-party-replacement-session-left",
		"char_id", characterID,
		"party_id", previousState.PartyID,
		"remaining_party_id", nextState.PartyID,
		"member_count", len(runtimePartyMembers(nextState)),
		"source", source,
		"reason", "a_replacement_socket_for_the_same_character_is_a_party_leave")
}

func (s *Service) unbindGameSession(session *gameSession) {
	if s == nil || session == nil {
		return
	}
	// 掉线/关游戏/小退都由这条路径收口，先把该会话上挂着的婚姻邀请等待撤掉。
	// 必须在拿 s.mu 之前调用：clearWeddingInvitations 自己要拿 s.weddingMu，
	// 而全局锁序是 weddingMu 先于 mu（见下方 unbind 尾部的加锁顺序）。
	s.clearWeddingInvitations(session)
	s.closeCurrentGuildCreateSession(session)
	s.closeCurrentPVPSession(session)
	s.mu.Lock()
	canonical := session.selectedCharacterID != 0 && s.gameSessions[session.selectedCharacterID] == session
	s.mu.Unlock()
	if canonical {
		// The party detach runs BEFORE the dungeon attempt is released.
		// releaseDisconnectedDungeonAttempt retires the runtime and releases its
		// payload, and releaseDungeonRuntimePayload is what clears
		// runtime.partyDungeonOwner -- the only handle on the shared
		// party-dungeon roster that the teammates still need updated. Asking
		// after that release always answers "no binding", which is why a
		// mid-dungeon disconnect used to leave the roster row, the op26 host
		// slot and the actor behind. See detachRuntimePartySession.
		s.detachRuntimePartySession(session, "game_session_unbound_or_disconnected")
		// Closing the game, returning to the character select screen and a
		// dropped transport all detach the ordinary party; the attack-squad label
		// is derived from that party, so it is cleared here rather than left on
		// the roster for a reconnect to inherit. Operator rule 2026-09-27:
		// 「无论是否主动退队 只要游戏关闭了或者小退到选择角色界面了 一律按退队处理」,
		// 「攻坚队…掉线 本来就要掉编制」. Before the gate run is released on the
		// next line, which is why the clear above does not carry the
		// inside-a-dungeon guard its party-leave sibling has.
		s.unassignRuntimeRaidSquadOnSessionLeave(session.selectedCharacterID, "game_session_unbound_or_disconnected")
		s.releaseDisconnectedDungeonAttempt(session, "game_session_unbound_or_disconnected")
		s.noteRuntimeRaidSessionDisconnected(session, "game_session_unbound_or_disconnected")
		s.detachOnlineItemTrade(session, "game_session_unbound_or_disconnected")
		s.closeCurrentPrivateStoreSession(session, false)
	}
	s.cancelCurrentDungeonDeathReturn(session, "game_session_unbound_or_disconnected")
	session.dungeon.mu.Lock()
	s.cancelCurrentDungeonCardAutoFlipLocked(session, session.dungeon.runtime, "game_session_unbound_or_disconnected")
	session.dungeon.mu.Unlock()
	s.stopCurrentPetGrowthClock(session, "game_session_unbound_or_disconnected")
	s.stopCurrentSpendTimeClock(session, "game_session_unbound_or_disconnected")
	s.stopCurrentDailyFatigueReset(session, "game_session_unbound_or_disconnected")
	// 虚弱恢复的时钟是"在线秒"：下线时把本次在线窗口结清并清掉窗口标记，
	// 这样离线期间任何读路径再来结算都不会让计时前进（运营要求 2026-09-15）。
	s.stopCurrentWeaknessOnlineClock(session)
	s.endCurrentPartyTeleportForSession(session)
	// 同 bindGameSessionCharacter：weddingMu 先于 mu 拿、后于 mu 放。
	s.weddingMu.Lock()
	defer s.weddingMu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	if session.selectedCharacterID != 0 && s.gameSessions[session.selectedCharacterID] == session {
		delete(s.gameSessions, session.selectedCharacterID)
	}
	if session.selectedCharacterID != 0 {
		advanceGameSessionCharacterGeneration(session)
	}
}

// releaseDisconnectedDungeonAttempt removes only the current dungeon attempt.
// The raid itself and its completed objectives remain in the process-wide raid
// state, so a reconnect can rejoin the same single raid and continue from the
// next open objective. This also releases the group occupancy that would
// otherwise leave the character unable to enter after a dropped connection.
func (s *Service) releaseDisconnectedDungeonAttempt(session *gameSession, reason string) {
	if s == nil || session == nil {
		return
	}
	// 超时空：掉线前先把这个人被确认过的城镇坐标记进名册。按 D2 的决定，超时空
	// 掉线不退队 —— 座位由 raid_disconnect.go 的宽限窗口守着，这里存的坐标只作
	// 诊断与重连参考(重连实际一律回 PVF 等待室起点，见 runtimeFiendWarRejoinLocation)。
	s.captureRuntimeFiendWarRejoinPosition(session)
	session.dungeon.mu.Lock()
	runtime := session.dungeon.runtime
	if runtime == nil {
		// Death Tower and compatibility owners are session-scoped and may exist
		// without a worldmap runtime. A transport teardown is still a complete
		// dungeon-generation boundary, so retire that detached scope as well.
		s.retireCurrentDungeonRuntimeLocked(session, nil, reason)
		session.dungeon.mu.Unlock()
		s.logGameEvent(session, "game-dungeon-disconnect-detached-scope-released",
			"char_id", session.selectedCharacterID,
			"reason", reason)
		return
	}

	status := worldmap.DungeonRunAbandoned
	if runtime.Session != nil {
		status = runtime.Session.Snapshot().Run.Status
		if status == worldmap.DungeonRunActive {
			if err := runtime.Session.Abandon(); err != nil {
				s.logGameEvent(session, "game-dungeon-disconnect-abandon-failed",
					"char_id", session.selectedCharacterID,
					"dungeon_id", runtime.Dungeon.ID,
					"reason", reason,
					"error", err)
			} else {
				status = worldmap.DungeonRunAbandoned
			}
		}
	}
	var lifecycleErr error
	if status == worldmap.DungeonRunCompleted {
		lifecycleErr = s.notifyRuntimeRaidDungeonCompletedLocked(session, runtime)
	} else {
		lifecycleErr = s.notifyRuntimeRaidDungeonAbandonedLocked(session, runtime)
	}
	if lifecycleErr != nil {
		s.logGameEvent(session, "game-dungeon-disconnect-raid-lifecycle-publish-failed",
			"char_id", session.selectedCharacterID,
			"dungeon_id", runtime.Dungeon.ID,
			"run_status", status,
			"reason", reason,
			"error", lifecycleErr)
	}
	s.retireCurrentDungeonRuntimeLocked(session, runtime, reason)
	session.dungeon.mu.Unlock()

	s.logGameEvent(session, "game-dungeon-disconnect-attempt-released",
		"char_id", session.selectedCharacterID,
		"dungeon_id", runtime.Dungeon.ID,
		"run_status", status,
		"raid_retained", true,
		"reason", reason)
}

func (s *Service) onlineGameSession(charID uint16) (*gameSession, bool) {
	if s == nil || charID == 0 {
		return nil, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	session, ok := s.gameSessions[charID]
	return session, ok && session != nil
}

func (s *Service) boundGameSessionCharacterSnapshot(session *gameSession) (boundGameSessionCharacter, bool) {
	if s == nil || session == nil {
		return boundGameSessionCharacter{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	characterID := session.selectedCharacterID
	if characterID == 0 || s.gameSessions[characterID] != session {
		return boundGameSessionCharacter{}, false
	}
	return boundGameSessionCharacter{
		session:    session,
		character:  characterID,
		generation: session.characterGeneration,
	}, true
}

func (s *Service) onlineGameSessionCharacterSnapshot(characterID uint16) (boundGameSessionCharacter, bool) {
	if s == nil || characterID == 0 {
		return boundGameSessionCharacter{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	session := s.gameSessions[characterID]
	if session == nil || session.selectedCharacterID != characterID {
		return boundGameSessionCharacter{}, false
	}
	return boundGameSessionCharacter{
		session:    session,
		character:  characterID,
		generation: session.characterGeneration,
	}, true
}

func (s *Service) boundGameSessionCharacterCurrent(identity boundGameSessionCharacter) bool {
	if s == nil || identity.session == nil || identity.character == 0 {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.gameSessions[identity.character] == identity.session &&
		identity.session.selectedCharacterID == identity.character &&
		identity.session.characterGeneration == identity.generation
}
