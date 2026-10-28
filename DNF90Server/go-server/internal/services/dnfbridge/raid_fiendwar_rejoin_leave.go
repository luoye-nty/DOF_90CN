package dnfbridge

import (
	"context"
	"fmt"
	"slices"
)

func (s *Service) captureRuntimeFiendWarRejoinPosition(session *gameSession) {
	if !isCurrentFiendWarRaidChannel(session) {
		return
	}
	session.townMu.Lock()
	position := session.townPositionSnapshot
	session.townMu.Unlock()
	session.dungeon.mu.Lock()
	if runtime := session.dungeon.runtime; runtime != nil && runtime.townReturnOrigin.PositionValid {
		position = runtime.townReturnOrigin
	}
	session.dungeon.mu.Unlock()
	if position.CharacterID != session.selectedCharacterID || !position.PositionValid || position.TownID != 98 || position.AreaID < 2 {
		return
	}
	s.raidMu.Lock()
	defer s.raidMu.Unlock()
	state := s.runtimeRaidByMemberLocked(session.selectedCharacterID)
	if state == nil || state.RaidMode != runtimeRaidOwnerFiendWar || state.Lifecycle.FiendWar == nil {
		return
	}
	member, ok := runtimeRaidFrozenMember(*state, session.selectedCharacterID)
	if !ok || state.Lifecycle.FiendWar.MemberArea(member.CharID, runtimeRaidNormalizedGroup(member.GroupIndex)) != int(position.AreaID) {
		return
	}
	for i := range state.Members {
		if state.Members[i].CharID == member.CharID {
			state.Members[i].RejoinPosition = position
		}
	}
	for i := range state.FrozenMembers {
		if state.FrozenMembers[i].CharID == member.CharID {
			state.FrozenMembers[i].RejoinPosition = position
		}
	}
}

func (s *Service) runtimeFiendWarRejoinLocation(session *gameSession, characterID uint16) (currentSceneTransitionRow, byte, string, bool, error) {
	state, ok := s.runtimeRaidForMember(characterID)
	if !ok || state.RaidMode != runtimeRaidOwnerFiendWar || !state.Started || state.Lifecycle.FiendWar == nil {
		return currentSceneTransitionRow{}, 0, "", false, nil
	}
	member, ok := runtimeRaidFrozenMember(state, characterID)
	if !ok {
		return currentSceneTransitionRow{}, 0, "", false, errRuntimeRaidNotMember
	}
	b := state.Lifecycle.FiendWar
	// A process reconnect is a fresh native Fiend War scene. The client must
	// re-enter at the PVF waiting-room start, never at a stale pre-disconnect
	// battle coordinate. Keep the old coordinate only as diagnostic state.
	areaID := b.Config.WaitingArea
	waiting, _ := s.townArea(int64(b.Config.WaitingTown), int64(areaID))
	position := currentTownPositionSnapshot{}
	for _, npc := range waiting.NPCs {
		if slices.Contains(b.Config.WaitingNPCs, int(npc.NPCID)) && npc.X >= 0 && npc.X <= 32767 && npc.Y >= 0 && npc.Y <= 32767 {
			position = currentTownPositionSnapshot{CharacterID: characterID, TownID: byte(b.Config.WaitingTown), AreaID: byte(areaID), PositionX: uint16(npc.X), PositionY: uint16(npc.Y), MovementCode: currentAntonRaidDirection, PositionValid: true}
			break
		}
	}
	area, found := s.townArea(int64(position.TownID), int64(position.AreaID))
	if !position.PositionValid || !found || area.MapPath == "" {
		return currentSceneTransitionRow{}, 0, "", true, fmt.Errorf("fiendwar rejoin position unavailable")
	}
	// 掉线宽限走本服务端既有的 raid_disconnect.go，不引入超时空专属的一套。
	// 那套已经按 raid 自己的 PVF [DISCONN MEMBER WAIT TIME] 计时(超时空 etc 同样
	// 声明 300)，并在成员重连时由 restoreRuntimeRaidPresence 清掉预约，所以这里
	// 不需要再做任何"取消保留"的动作。
	s.raidMu.Lock()
	current := s.raids[state.RaidKey]
	if current == nil || current.StartAttempt != state.StartAttempt || runtimeRaidMemberIndex(*current, characterID) < 0 || current.Lifecycle.FiendWar == nil {
		s.raidMu.Unlock()
		return currentSceneTransitionRow{}, 0, "", true, errRuntimeRaidNotMember
	}
	if current.Lifecycle.FiendWar.MemberArea(characterID, runtimeRaidNormalizedGroup(member.GroupIndex)) != areaID {
		battle := current.Lifecycle.FiendWar.Clone()
		if battle.MemberAreas == nil {
			battle.MemberAreas = make(map[uint16]int)
		}
		battle.MemberAreas[characterID] = areaID
		group := runtimeRaidNormalizedGroup(member.GroupIndex)
		peerAtPrevious := slices.ContainsFunc(current.FrozenMembers, func(peer runtimeRaidMemberState) bool {
			return peer.CharID != characterID && runtimeRaidNormalizedGroup(peer.GroupIndex) == group && battle.MemberArea(peer.CharID, group) != areaID
		})
		if !peerAtPrevious {
			battle.GroupAreas[group] = areaID
		}
		current.Lifecycle.FiendWar = battle
		current.Lifecycle.Revision++
	}
	s.raidMu.Unlock()
	row := currentSceneTransitionRow{ObjectOrResourceKey: currentSceneActorObjectKey(characterID), Value1: position.PositionX, Value2: position.PositionY, Value3: position.MovementCode, Value4: currentAntonRaidAreaState}
	s.logGameEvent(session, "game-fiendwar-rejoin-location", "raid_key", state.RaidKey, "char_id", characterID, "town_id", position.TownID, "area_id", position.AreaID, "position_x", row.Value1, "position_y", row.Value2)
	return row, position.AreaID, area.MapPath, true, nil
}

func (s *Service) removeRuntimeFiendWarMember(characterID uint16, raidKey uint32) (runtimeRaidState, error) {
	s.raidMu.Lock()
	defer s.raidMu.Unlock()
	state := s.raids[raidKey]
	if state == nil || state.RaidMode != runtimeRaidOwnerFiendWar || runtimeRaidMemberIndex(*state, characterID) < 0 {
		return runtimeRaidState{}, errRuntimeRaidNotMember
	}
	// 生命周期状态不再拦这一脚：名册里的人任何时刻都该能退（开团准备中、进行中、
	// 通关/失败结算之后都一样）。原来只放 Preparing/Active，于是"打完/失败后再点退出"
	// 与"名册只剩一个人"这些情形全被静默拒掉 —— 真机 2026-10-01 20:0x 报的就是它。
	if state.Lifecycle.FiendWar == nil {
		return runtimeRaidState{}, errRuntimeRaidLifecycleState
	}
	state.Members = slices.DeleteFunc(state.Members, func(m runtimeRaidMemberState) bool { return m.CharID == characterID })
	state.FrozenMembers = slices.DeleteFunc(state.FrozenMembers, func(m runtimeRaidMemberState) bool { return m.CharID == characterID })
	battle := state.Lifecycle.FiendWar.Clone()
	delete(battle.MemberAreas, characterID)
	delete(battle.RecoveryUntil, characterID)
	delete(battle.ImprisonCounts, characterID)
	for group, area := range battle.GroupAreas {
		peerIndex := slices.IndexFunc(state.FrozenMembers, func(m runtimeRaidMemberState) bool { return runtimeRaidNormalizedGroup(m.GroupIndex) == group })
		if peerIndex < 0 {
			delete(battle.GroupAreas, group)
		} else if !slices.ContainsFunc(state.FrozenMembers, func(m runtimeRaidMemberState) bool {
			return runtimeRaidNormalizedGroup(m.GroupIndex) == group && battle.MemberArea(m.CharID, group) == area
		}) {
			battle.GroupAreas[group] = battle.MemberArea(state.FrozenMembers[peerIndex].CharID, group)
		}
	}
	state.Lifecycle.FiendWar = battle
	delete(state.Lifecycle.FiendWarClearCounts, characterID)
	if len(state.Members) == 0 {
		state.Lifecycle.Status = runtimeRaidLifecycleAbandoned
		delete(s.raids, raidKey)
	} else if state.LeaderID == characterID {
		state.LeaderID = state.Members[0].CharID
	}
	state.Lifecycle.Revision++
	return cloneRuntimeRaidState(*state), nil
}

// fiendWarLeaveKeyMatches 判断 op665 里那个 key 是不是"这个人自己"的表达。
//
// op665 一个包干两件事：自己退出带的 key 是**自己**（或攻坚队的 key，它的低 16 位就是
// 创建者的角色 id），队长踢人带的 key 是**被踢的那个人**（那条在上层就分流走了，而且
// 现在要求 key 确实是名册里的人）。
//
// 真机 2026-10-01 20:50 实测：名册只剩一个人之后，客户端发出来的 key 是 **3** ——
// 既不是自己（22）也不是攻坚队 key 的低 16 位（1）。所以这里接受 0、自己、攻坚队 key
// 三种"自己"的写法，**其余一律当作自己退出**：玩家按了退出攻坚队，就必须让他退出去，
// 不能因为一个读不懂的 key 把他晾在原地。
func fiendWarLeaveKeyMatches(state runtimeRaidState, characterID, requestKey uint16) bool {
	switch requestKey {
	case 0, characterID, uint16(state.RaidKey):
		return true
	}
	return !fiendWarRequestKeyNamesAnotherMember(state, characterID, requestKey)
}

// fiendWarRequestKeyNamesAnotherMember 报告这个 key 是不是名册里**别人**的角色 id ——
// 只有这种形状才可能是踢人（那条已经在 handleOnlineLeaveRaid 里分流，走到这里时说明
// 发送者不是队长或目标不是成员，于是按自己退出处理）。
func fiendWarRequestKeyNamesAnotherMember(state runtimeRaidState, characterID, requestKey uint16) bool {
	index := runtimeRaidMemberIndex(state, requestKey)
	return index >= 0 && state.Members[index].CharID != characterID
}

func (s *Service) handleRuntimeFiendWarLeave(session *gameSession, typ, requestKey uint16, state runtimeRaidState) error {
	// 退出攻坚队**永远要有回应**。以前这里有三个联合条件（key 必须严格等于 RaidKey 的
	// 低 16 位、不能处于 StartPending、状态必须是 Preparing 或 Active），任一不满足就
	// 只回一个失败包、连日志都不留 —— 玩家看到的就是"点了没反应"。
	//
	// 真机 2026-10-01 20:0x（操作员）：名册只剩一个人之后点退出攻坚队，什么都没发生。
	// 现在：key 放宽到上面那三种写法；**任何生命周期状态都接受**（打完、失败、放弃之后
	// 同样得能退）；拒绝时把原因写进日志。
	if !fiendWarLeaveKeyMatches(state, session.selectedCharacterID, requestKey) {
		s.logGameEvent(session, "game-fiendwar-leave-refused",
			"raid_key", state.RaidKey,
			"char_id", session.selectedCharacterID,
			"request_key", requestKey,
			"leader_id", state.LeaderID,
			"member_count", len(state.Members),
			"lifecycle_status", state.Lifecycle.Status,
			"reason", "request_key_is_not_a_self_alias",
			"source", "handle_runtime_fiendwar_leave")
		return s.sendRaidCommandFailure(session, typ)
	}
	session.dungeon.mu.Lock()
	inDungeon := session.dungeon.runtime != nil
	session.dungeon.mu.Unlock()
	if inDungeon {
		if err := s.handleDungeonBackToVillage(session, nil); err != nil {
			return err
		}
		session.dungeon.mu.Lock()
		inDungeon = session.dungeon.runtime != nil
		session.dungeon.mu.Unlock()
		if inDungeon {
			return s.sendRaidCommandFailure(session, typ)
		}
	}
	next, err := s.removeRuntimeFiendWarMember(session.selectedCharacterID, state.RaidKey)
	if err != nil {
		// 拒绝也要说清楚：日志里留一行，玩家能不能退就靠这条查。
		s.logGameEvent(session, "game-fiendwar-leave-refused",
			"raid_key", state.RaidKey,
			"char_id", session.selectedCharacterID,
			"request_key", requestKey,
			"member_count", len(state.Members),
			"lifecycle_status", state.Lifecycle.Status,
			"start_pending", state.StartPending,
			"reason", err.Error(),
			"source", "handle_runtime_fiendwar_leave")
		return s.sendRaidCommandFailure(session, typ)
	}
	storeRuntimeRaidEndedProjection(session, state.RaidKey)
	// Remaining players must receive the new leader as well as the shorter roster.
	if len(next.Members) > 0 {
		if err := s.broadcastRuntimeRaidCreate(next); err != nil {
			s.logGameEvent(session, "game-fiendwar-leave-peer-refresh-failed", "error", err)
		}
		if err := s.broadcastRuntimeRaidRefresh(next); err != nil {
			s.logGameEvent(session, "game-fiendwar-leave-peer-refresh-failed", "error", err)
		}
	}
	s.logGameEvent(session, "game-fiendwar-member-left", "raid_key", state.RaidKey, "char_id", session.selectedCharacterID, "remaining_members", len(next.Members))
	return s.finishRuntimeFiendWarLeave(session, typ)
}

func (s *Service) finishRuntimeFiendWarLeave(session *gameSession, typ uint16) error {
	// Serialize the native reset after any in-flight board publication.
	session.fiendWarBoardMu.Lock()
	err := s.sendRuntimeSingleRaidEnded(session, typ, "fiendwar_member_leave")
	if err == nil {
		session.fiendWarBoard = runtimeRaidState{}
	}
	session.fiendWarBoardMu.Unlock()
	if err != nil {
		return err
	}
	session.townMu.Lock()
	position := session.townPositionSnapshot
	session.townMu.Unlock()
	if position.TownID != 98 || position.AreaID < 2 {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	characterID, _, character, found := s.selectedCharacterForEnter(ctx, session)
	if !found || characterID != session.selectedCharacterID {
		return errRuntimeRaidNotMember
	}
	spec, _ := currentRaidTownSpecForSession(session)
	_, town, area, row, path, err := s.currentRaidTownTransition(session, characterID, character, "fiendwar_leave_public_town", spec)
	if err != nil {
		return err
	}
	body, err := buildCurrentSceneTransitionBody(town, area, []currentSceneTransitionRow{row})
	if err != nil {
		return err
	}
	if err := s.sendCurrentTownLocationNotifications(session, characterID, town, area, row, "fiendwar_leave_public_town"); err != nil {
		return err
	}
	if err := s.sendGameUpperRawClass(session, currentSceneTransitionMsgID, body, 0); err != nil {
		return err
	}
	s.cleanupOnlinePlayer(session)
	clearCurrentDungeonSelectContext(session)
	resetDungeonEntrySceneGates(session)
	session.townMu.Lock()
	setCurrentTownPositionSceneFromTransitionLocked(session, characterID, town, area, row)
	session.townSceneReadyCharacterID = characterID
	session.townMu.Unlock()
	s.publishCurrentTownPresence(session, character, town, area, row, 0, path, "fiendwar_leave_public_town")
	return nil
}
