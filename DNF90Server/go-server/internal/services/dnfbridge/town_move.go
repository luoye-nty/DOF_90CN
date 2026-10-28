package dnfbridge

import (
	"context"
	"encoding/binary"
	"fmt"
	"sort"
	"strconv"
	"time"

	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

const currentTownSetUserAreaBodySize = 16

type currentTownSetUserAreaRequest struct {
	TownID       byte
	AreaID       byte
	PositionX    int16
	PositionY    int16
	Direction    byte
	OpaqueU16A   uint16
	OpaqueU16B   uint16
	OpaqueU32    uint32
	OpaqueTailU8 byte
}

func parseCurrentTownSetUserAreaRequest(body []byte) (currentTownSetUserAreaRequest, error) {
	if len(body) != currentTownSetUserAreaBodySize {
		return currentTownSetUserAreaRequest{}, fmt.Errorf("current town op36 body length %d, want %d", len(body), currentTownSetUserAreaBodySize)
	}
	return currentTownSetUserAreaRequest{
		TownID:       body[0],
		AreaID:       body[1],
		PositionX:    int16(binary.LittleEndian.Uint16(body[2:4])),
		PositionY:    int16(binary.LittleEndian.Uint16(body[4:6])),
		Direction:    body[6],
		OpaqueU16A:   binary.LittleEndian.Uint16(body[7:9]),
		OpaqueU16B:   binary.LittleEndian.Uint16(body[9:11]),
		OpaqueU32:    binary.LittleEndian.Uint32(body[11:15]),
		OpaqueTailU8: body[15],
	}, nil
}

func (s *Service) handleTownSetUserArea(session *gameSession, body []byte) error {
	return s.handleTownSetUserAreaAfterEncounter(session, body, false)
}

// handleTownSetUserAreaAfterEncounter 是 handleTownSetUserArea 加一个"遇敌是否已经
// 由调用方尝试过"的标志。
//
// 只有一处读它：下面 fiendWarMoveBefore 那段。之所以需要这个标志，是因为超时空的
// 走位有两个入口 —— 客户端的 op36 直接进来(encounterAttempted=false，需要自己尝试
// 开战)，以及队长的遇敌流程把跟随者的那次走位排队、事后重放
// (queueRuntimeFiendWarMoveEncounterLocked，encounterAttempted=true，再试一次会自
// 己套自己)。
//
// 安东/卢克/普通频道永远走 false，且两个插入点都被 isCurrentFiendWarRaidChannel
// 守卫，所以那些路径与改动前逐字等价。
func (s *Service) handleTownSetUserAreaAfterEncounter(session *gameSession, body []byte, encounterAttempted bool) error {
	if session == nil {
		return nil
	}
	request, err := parseCurrentTownSetUserAreaRequest(body)
	if err != nil {
		s.logGameEvent(session, "game-town-set-user-area-blocked",
			"body_len", len(body),
			"reason", "current_exe_op36_writer_boundary_mismatch",
			"error", err)
		return nil
	}
	if session.selectedCharacterID == 0 {
		s.logTownSetUserAreaBlocked(session, request, "selected_character_missing")
		return nil
	}
	session.dungeon.mu.Lock()
	pendingDungeonReturn := session.dungeon.runtime != nil &&
		session.dungeon.runtime.Session != nil &&
		session.dungeon.runtime.townReturnPending
	session.dungeon.mu.Unlock()
	if !pendingDungeonReturn {
		session.townMu.Lock()
		townSceneReadyCharacterID := session.townSceneReadyCharacterID
		session.townMu.Unlock()
		if townSceneReadyCharacterID != session.selectedCharacterID {
			s.logTownSetUserAreaBlocked(session, request, "town_scene_player_state_not_finalized")
			return nil
		}
	}
	refreshQuestsAfterDungeonReturn := pendingDungeonReturn
	if err := s.commitPendingDungeonReturnForSceneRequest(session, "current_exe_op36_town_area_request"); err != nil {
		return err
	}
	session.dungeon.mu.Lock()
	hasDungeonRuntime := session.dungeon.runtime != nil
	session.dungeon.mu.Unlock()
	if hasDungeonRuntime {
		s.logTownSetUserAreaBlocked(session, request, "active_dungeon_runtime_rejects_town_area_request")
		return nil
	}

	session.townMu.Lock()
	defer session.townMu.Unlock()
	clientRequestedTownID := request.TownID
	clientRequestedAreaID := request.AreaID
	raidTownSpec, raidTownSessionScoped := currentRaidTownSpecForSession(session)
	pvpTownSessionScoped := isPVPChannel(session.channel)
	// RAID towns are session-scoped scenes only. Never persist or escape them
	// through a transport-shaped op36 while the session remains in that owner.
	if raidTownSessionScoped && request.TownID != byte(raidTownSpec.townID) {
		s.logTownSetUserAreaBlocked(session, request, raidTownSpec.ownerName+"_raid_channel_rejects_non_raid_town_transition")
		return nil
	}
	if pvpTownSessionScoped && request.TownID != byte(currentPVPTownID) {
		s.logTownSetUserAreaBlocked(session, request, "pvp_channel_rejects_non_pvp_town_transition")
		return nil
	}
	raidInstanceKey := uint32(0)
	if raidTownSessionScoped &&
		request.TownID == byte(raidTownSpec.townID) &&
		request.AreaID == raidTownSpec.waitingArea {
		state, member := s.runtimeRaidForMember(session.selectedCharacterID)
		if !member || state.RaidKey == 0 {
			s.logTownSetUserAreaBlocked(session, request, raidTownSpec.ownerName+"_waiting_instance_requires_raid_membership")
			return nil
		}
		raidInstanceKey = state.RaidKey
	}
	// 超时空：走进任何一个区域(>=2)都要求自己是攻坚成员，而且除等待室之外必须
	// 处于进行中。MoveMember 把"这个人走到哪个区域"记进战场 —— 它与下面
	// moveRuntimeFiendWarGroup 的分工是：MoveMember 记个人位置并受区域人数上限与
	// 死亡禁锢约束，moveRuntimeFiendWarGroup 记小队整体占位并校验相邻关系。
	// 安东/卢克不进这里(频道类型不同)。
	if isCurrentFiendWarRaidChannel(session) && request.AreaID >= 2 {
		state, member := s.runtimeRaidForMember(session.selectedCharacterID)
		if !member {
			s.logTownSetUserAreaBlocked(session, request, "fiendwar_membership_required")
			return nil
		}
		raidInstanceKey = state.RaidKey
		if int(request.AreaID) != int(currentFiendWarWaitingAreaID) && state.Lifecycle.Status != runtimeRaidLifecyclePhaseActive {
			s.logTownSetUserAreaBlocked(session, request, "fiendwar_not_active")
			s.refuseRuntimeFiendWarMoveLocked(session, request, "fiendwar_not_active")
			return nil
		}
		if state.Lifecycle.FiendWar != nil && state.Lifecycle.Status == runtimeRaidLifecyclePhaseActive {
			// **live-first**：账本里的个人区域一旦滞后，相邻判定就会拿**旧格子**去算，
			// 把玩家挡在隔壁门口 —— 真机 2026-10-02 14:20 礼白666：人在 11 点 7，
			// 被按"16 与 7 不相邻"拒（area 11 的 next 明明含 7）。账本滞后的来源包括
			// "被队友带进副本"（他没走过 op36）与服务端搬运，所以每一脚移动前都先对齐。
			if position := session.townPositionSnapshot; position.PositionValid &&
				int(position.TownID) == 98 && int(position.AreaID) >= 2 {
				if synced := s.syncRuntimeFiendWarMemberAreaFromLivePosition(
					session.selectedCharacterID, int(position.AreaID), "fiendwar_move"); synced.RaidKey != 0 {
					state = synced
				}
			}
			frozen, _ := runtimeRaidFrozenMember(state, session.selectedCharacterID)
			if err := state.Lifecycle.FiendWar.MoveMember(frozen.CharID, runtimeRaidNormalizedGroup(frozen.GroupIndex), int(request.AreaID), s.gameplayNow()); err != nil {
				s.logTownSetUserAreaBlocked(session, request, err.Error())
				// 拒绝也要回话：不然客户端会永远停在"正在进入"上（真机卡死过）。
				s.refuseRuntimeFiendWarMoveLocked(session, request, err.Error())
				return nil
			}
		}
	}
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.Character == nil {
		s.logTownSetUserAreaBlocked(session, request, "character_repository_unavailable")
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	characterID := strconv.FormatUint(uint64(session.selectedCharacterID), 10)
	character, found, err := repositories.Character.Load(ctx, characterID)
	if err != nil {
		return err
	}
	if !found || character.Stats == nil {
		s.logTownSetUserAreaBlocked(session, request, "selected_character_location_missing")
		return nil
	}
	// 从婚房(101/0)走左门出来的原生形状是 (300,200)/方向5，落点恰好落在传送门
	// 触发矩形里；客户端重叠 latch 不解除就会立刻把人送回房间。这里把它归一化到
	// 赛丽亚房间的门边安全落点（见 wedding_room_exit.go）。
	// 必须早于下面的 prev-village 重定向与 currentTown 读取，否则下游拿到的是
	// 未修正的目标。
	if normalized, weddingRoomExit, exitErr := s.normalizeCurrentWeddingRoomExit(session, character, request); exitErr != nil {
		s.logTownSetUserAreaBlocked(session, request, exitErr.Error())
		return nil
	} else if weddingRoomExit {
		request = normalized
		s.logGameEvent(session, "game-wedding-room-exit-normalized",
			"char_id", session.selectedCharacterID,
			"requested_town_id", clientRequestedTownID, "requested_area_id", clientRequestedAreaID,
			"town_id", request.TownID, "area_id", request.AreaID,
			"position_x", request.PositionX, "position_y", request.PositionY,
			"source", "current_exe_3410D33_minus101_portal_runtime_pvf_seria_wedding_door")
	}
	currentTown, ok := character.Stats["town_id"]
	if !ok {
		s.logTownSetUserAreaBlocked(session, request, "selected_character_town_id_missing")
		return nil
	}
	seriaExitPrevVillageRedirect := false
	if origin, redirect := s.currentTownSetUserAreaPrevVillageRedirectLocked(session, character, request); redirect {
		request.TownID = origin.TownID
		request.AreaID = origin.AreaID
		request.PositionX = int16(origin.PositionX)
		request.PositionY = int16(origin.PositionY)
		request.Direction = origin.MovementCode
		session.townPrevVillageSnapshot = origin
		seriaExitPrevVillageRedirect = true
		s.logGameEvent(session, "game-town-set-user-area-seria-exit-prev-village-redirected",
			"char_id", session.selectedCharacterID,
			"requested_town_id", clientRequestedTownID,
			"requested_area_id", clientRequestedAreaID,
			"target_town_id", request.TownID,
			"target_area_id", request.AreaID,
			"target_position_x", request.PositionX,
			"target_position_y", request.PositionY,
			"target_direction", request.Direction,
			// The opaque fields are what separates the exit door from the
			// teleport array; without them a misrouted exit is unreadable.
			"opaque_u16_a", request.OpaqueU16A,
			"opaque_u16_b", request.OpaqueU16B,
			"opaque_u32", request.OpaqueU32,
			"opaque_tail_u8", request.OpaqueTailU8,
			"source", "current_exe_op36_fixed_seria_room_exit_alias")
	}
	teleportArrayRequest := request.LooksLikeTeleportArraySelection()
	townMapStationRequest := request.LooksLikeTownMapStationSelection()
	townTransportRequest := request.LooksLikeTownTransportSelection()
	townSeriaRoomPortalRequest := false
	townCrossTownPortalRequest := false
	townNPCEnterRequest := false
	area, found := s.townArea(int64(request.TownID), int64(request.AreaID))
	if !found {
		s.logTownSetUserAreaBlocked(session, request, "target_town_area_missing_from_runtime_pvf")
		return nil
	}
	// Seria maps, a raid channel's Seria room, and the PVP channel lobby (town 10
	// / area 0) are all private scenes: their visitors never join town
	// co-presence, so each client sees an empty scene like its own Seria room.
	// Other PVP channel areas stay public. The map check is authoritative for a
	// named Seria map, the raid spec for a raid gate, and the town/area check for
	// the PVP lobby because that map path is not a "seria" file either.
	seriaMap := townMapPathLooksLikeSeria(area.MapPath)
	raidGateRoom := currentRaidGateAreaIsSeriaRoom(session, request.TownID, request.AreaID)
	pvpLobbyRoom := isPVPChannel(session.channel) &&
		request.TownID == byte(currentPVPTownID) && request.AreaID == byte(currentPVPAreaID)
	privateRoomScene := seriaMap || raidGateRoom || pvpLobbyRoom
	privateRoomKind := "private_seria_room"
	if privateRoomScene && !seriaMap && !raidGateRoom {
		privateRoomKind = "private_pvp_lobby"
	}
	// A type-23 login/reconnect uses the PVF Anton town as a session-scoped
	// scene while the character record retains its ordinary town. Once the
	// scene is ready, its snapshot is the authoritative source for op36 route
	// validation; falling back to the durable town would misclassify an Anton
	// portal as an unproven cross-town request.
	sceneSnapshot := session.townPositionSnapshot
	sceneReadyCharacterID := session.townSceneReadyCharacterID
	if raidTownSessionScoped &&
		sceneReadyCharacterID == session.selectedCharacterID &&
		sceneSnapshot.CharacterID == session.selectedCharacterID &&
		sceneSnapshot.TownID == byte(raidTownSpec.townID) {
		currentTown = int64(sceneSnapshot.TownID)
	}
	if pvpTownSessionScoped &&
		sceneReadyCharacterID == session.selectedCharacterID &&
		sceneSnapshot.CharacterID == session.selectedCharacterID &&
		sceneSnapshot.TownID == byte(currentPVPTownID) {
		currentTown = int64(sceneSnapshot.TownID)
	}
	townCrossTownPortalRequest = request.LooksLikeTownCrossTownPortalSelection(currentTown)
	townSeriaRoomPortalRequest = request.LooksLikeTownSeriaRoomPortalSelection(currentTown, area.MapPath)
	townNPCEnterRequest = request.LooksLikeTownNPCEnterSelection(currentTown) &&
		s.townAllowsNPCEnter(int64(request.TownID), int64(request.AreaID), int64(request.OpaqueU32))
	robotPublicPlacementRequest := session.robotSessionBound &&
		s.currentAutomaticRobotPublicTownAreaAllowed(request.TownID, request.AreaID)
	townTransportRequest = townTransportRequest || townCrossTownPortalRequest || townSeriaRoomPortalRequest ||
		townNPCEnterRequest || robotPublicPlacementRequest || seriaExitPrevVillageRedirect
	if currentTown != int64(request.TownID) && !townTransportRequest {
		s.logTownSetUserAreaBlocked(session, request, "cross_town_route_not_proven_by_current_pvf_topology")
		return nil
	}
	if area.MinLevel > int64(character.Level) {
		s.logTownSetUserAreaBlocked(session, request, "target_area_min_level_not_met")
		return nil
	}
	areaState, ok := character.Stats["area_state"]
	if !ok || areaState < 0 || areaState > int64(^byte(0)) {
		s.logTownSetUserAreaBlocked(session, request, "selected_character_area_state_missing_or_invalid")
		return nil
	}
	if len(area.NeedQuests) > 0 && !robotPublicPlacementRequest {
		needQuestsCompleted, questErr := s.townAreaNeedQuestsCompleted(ctx, repositories, characterID, area.NeedQuests)
		if questErr != nil {
			s.logGameEvent(session, "game-town-set-user-area-need-quest-check-failed",
				"char_id", session.selectedCharacterID,
				"town_id", request.TownID,
				"area_id", request.AreaID,
				"target_need_quests", area.NeedQuests,
				"town_transport_request", townTransportRequest,
				"error", questErr)
		}
		if !needQuestsCompleted {
			s.logTownSetUserAreaBlocked(session, request, "target_area_has_pvf_quest_requirement_without_completed_quest_owner")
			return nil
		}
	}
	prevVillageOrigin := currentTownPositionSnapshot{}
	prevVillageOriginBound := false
	if townMapPathLooksLikeSeria(area.MapPath) {
		if origin, ok := currentTownPrevVillageOriginLocked(session, session.selectedCharacterID, character); ok {
			// 不要把婚房(101)绑成"上个城镇"。
			//
			// currentTownPrevVillageOriginLocked 返回的是角色**当前**位置；从婚房
			// 走左门出来时归一化的目标正好是赛丽亚房间(38/1，也是 Seria 地图)，
			// 于是这条绑定会把回城点覆盖成婚房本身，op1425 就会把玩家送回婚房。
			// 保持旧值（进婚房之前记住的那个城镇）才是原生行为。
			if origin.TownID == currentWeddingRoomTownID {
				s.logTownPrevVillageOriginBindFailed(session, session.selectedCharacterID,
					"current_location_is_wedding_room_keeps_earlier_prev_village")
			} else {
				originSource := "current_exe_op36_enter_seria_room"
				if doorOrigin, _, projected := s.currentTownPrevVillageDoorOrigin(origin); projected {
					origin = doorOrigin
					originSource = "current_exe_op36_enter_seria_room_runtime_pvf_door"
				}
				prevVillageOrigin = origin
				prevVillageOriginBound = true
				session.townPrevVillageSnapshot = origin
				s.logTownPrevVillageOriginBound(session, origin, originSource)
			}
		} else {
			s.logTownPrevVillageOriginBindFailed(session, session.selectedCharacterID, "current_location_or_position_unavailable_before_enter_seria_room")
		}
	}

	// 婚姻场景按夫妻对分实例（见 wedding_presence.go）：教堂默认房(39/8..38)与
	// 婚房(101)都是"按对分实例的公共场景"，复用 raidInstanceKey 这条实例键通道，
	// 于是下面 EnterArea 注册进去的就是夫妻实例。无权进入者必须直接拒，否则会被
	// 塞进别人的夫妻实例里。
	weddingTownSessionScoped := false
	if key, scoped, admitted := s.currentWeddingTownInstanceKey(
		character, request.TownID, request.AreaID, uint16(townPresenceChannelID(session))); !admitted {
		s.logTownSetUserAreaBlocked(session, request, "wedding_reservation_or_marriage_required")
		return nil
	} else if scoped {
		// 只有教堂默认房是会话作用域（预约只在本次登录有效）。婚房(101)相反：
		// 它和普通城镇一样要落库，玩家重登仍然应该在婚房里 —— 与上游同口径。
		weddingTownSessionScoped = request.TownID != currentWeddingRoomTownID
		raidInstanceKey = key
	}

	previous := dnfrepo.CharacterRecord{}
	// 教堂是会话作用域的：不该把预约落成持久化的 town_id/area_id/坐标。婚房与
	// 普通城镇一样持久化（见上）。
	persistedTownLocation := !raidTownSessionScoped && !pvpTownSessionScoped && !weddingTownSessionScoped
	if persistedTownLocation {
		previous = dnfrepo.CloneCharacter(character)
		character = dnfrepo.CloneCharacter(character)
		if prevVillageOriginBound {
			currentTownPrevVillageApplyOriginStats(character.Stats, prevVillageOrigin)
		}
		character.Stats["town_id"] = int64(request.TownID)
		character.Stats["area_id"] = int64(request.AreaID)
		character.Stats["pos_x"] = int64(request.PositionX)
		character.Stats["pos_y"] = int64(request.PositionY)
		character.Stats["direction"] = int64(request.Direction)
		character.UpdatedAt = time.Now().UTC()
		if err := dnfrepo.SaveCharacterFields(ctx, repositories.Character, character, dnfrepo.CharacterFieldStats); err != nil {
			return err
		}
	}

	selfRow := currentSceneTransitionRow{
		ObjectOrResourceKey: currentSceneActorObjectKey(session.selectedCharacterID),
		Value1:              uint16(request.PositionX),
		Value2:              uint16(request.PositionY),
		Value3:              request.Direction,
		Value4:              byte(areaState),
	}
	transitionRows := make([]currentSceneTransitionRow, 0, 1)
	if s.onlinePlayers != nil {
		areaPlayers := s.onlinePlayers.GetAreaInstancePlayers(
			townPresenceChannelID(session),
			request.TownID, request.AreaID, raidInstanceKey)
		sort.Slice(areaPlayers, func(i, j int) bool {
			return areaPlayers[i].CharacterID < areaPlayers[j].CharacterID
		})
		for i := range areaPlayers {
			if areaPlayers[i].CharacterID == 0 ||
				areaPlayers[i].CharacterID == session.selectedCharacterID {
				continue
			}
			transitionRows = append(transitionRows, currentSceneTransitionRow{
				ObjectOrResourceKey: currentSceneActorObjectKey(areaPlayers[i].CharacterID),
				Value1:              areaPlayers[i].PositionX,
				Value2:              areaPlayers[i].PositionY,
				Value3:              areaPlayers[i].Direction,
				Value4:              areaPlayers[i].AreaState,
			})
		}
	}
	// Keep the selected actor last. The current EXE's op24 parser binds the
	// final row as the camera/selected actor while earlier rows form the peer
	// roster, matching the full-area roster role used by 86JP after it sends
	// peer appearance and user-info records.
	transitionRows = append(transitionRows, selfRow)
	response, err := buildCurrentSceneTransitionBody(request.TownID, request.AreaID, transitionRows)
	if err != nil {
		return err
	}
	// 超时空：这一步只把"队伍走到哪个区域"记进战场。真正的开战在下面
	// encounterAttempted 的守卫里尝试。这里失败要整笔拒绝 —— 客户端即将收到的场景
	// 切换与战场里的区域归属必须一致，否则会出现"人在 B 区、战场以为在 A 区"。
	var fiendWarMoveBefore runtimeRaidState
	if isCurrentFiendWarRaidChannel(session) && request.AreaID >= 2 {
		if state, ok := s.runtimeRaidForMember(session.selectedCharacterID); ok && state.Lifecycle.Status == runtimeRaidLifecyclePhaseActive {
			if _, moveErr := s.moveRuntimeFiendWarGroup(session.selectedCharacterID, int(request.AreaID)); moveErr != nil {
				s.logTownSetUserAreaBlocked(session, request, moveErr.Error())
				// 真机 2026-10-01：这里曾经静默 return，客户端于是卡在"正在进入"。
				s.refuseRuntimeFiendWarMoveLocked(session, request, moveErr.Error())
				return nil
			}
			fiendWarMoveBefore = state
		}
	}
	rollbackLocation := func(writeErr error) error {
		if fiendWarMoveBefore.RaidKey != 0 {
			s.rollbackRuntimeFiendWarMove(fiendWarMoveBefore, session.selectedCharacterID, int(request.AreaID))
		}
		if !persistedTownLocation {
			return writeErr
		}
		rollbackCtx, rollbackCancel := context.WithTimeout(context.Background(), createWriteTimeout)
		rollbackErr := dnfrepo.SaveCharacterFields(rollbackCtx, repositories.Character, previous, dnfrepo.CharacterFieldStats)
		rollbackCancel()
		if rollbackErr != nil {
			s.logGameEvent(session, "game-town-set-user-area-rollback-failed",
				"char_id", session.selectedCharacterID,
				"town_id", request.TownID,
				"area_id", request.AreaID,
				"write_error", writeErr,
				"rollback_error", rollbackErr)
		}
		return writeErr
	}
	if !encounterAttempted && fiendWarMoveBefore.RaidKey != 0 {
		// op24 构造的是野外场景。如果这一脚正好踩进一个有敌军的区域，就走既有的
		// op27/28/29 副本加载序列直接进图，不再发场景切换 —— 否则客户端会先被画进
		// 空地再被拖进副本。
		if handled, encErr := s.queueRuntimeFiendWarMoveEncounterLocked(session, request, selfRow, body); handled || encErr != nil {
			if encErr != nil {
				session.dungeon.mu.Lock()
				entered := session.dungeon.runtime != nil
				session.dungeon.mu.Unlock()
				if entered {
					return encErr
				}
				return rollbackLocation(encErr)
			}
			return nil
		}
	}
	s.logGameEvent(session, "game-town-set-user-area-transition-send",
		"char_id", session.selectedCharacterID,
		"request_msg_id", uint16(dnfenum.CmdPacketSetUserArea),
		"response_msg_ids", []uint16{
			currentTownUserPositionNotificationMsgID,
			currentTownUserAreaNotificationMsgID,
			currentClearQuestListMsgID,
			currentSceneTransitionMsgID,
		},
		"town_id", request.TownID,
		"area_id", request.AreaID,
		"position_x", request.PositionX,
		"position_y", request.PositionY,
		"direction", request.Direction,
		"area_state", areaState,
		"map_path", area.MapPath,
		"teleport_array_request", teleportArrayRequest,
		"town_map_station_request", townMapStationRequest,
		"town_seria_room_portal_request", townSeriaRoomPortalRequest,
		"town_cross_town_portal_request", townCrossTownPortalRequest,
		"town_npc_enter_request", townNPCEnterRequest,
		"robot_public_placement_request", robotPublicPlacementRequest,
		"town_transport_request", townTransportRequest,
		"seria_exit_prev_village_redirect", seriaExitPrevVillageRedirect,
		"client_requested_town_id", clientRequestedTownID,
		"client_requested_area_id", clientRequestedAreaID,
		"durable_character_location_written", persistedTownLocation,
		"target_need_quests", area.NeedQuests,
		"opaque_u16_a", request.OpaqueU16A,
		"opaque_u16_b", request.OpaqueU16B,
		"opaque_u32", request.OpaqueU32,
		"opaque_tail_u8", request.OpaqueTailU8,
		"row_count", len(transitionRows),
		"peer_row_count", len(transitionRows)-1,
		"op24_body_len", len(response),
		"sequence", "op22_user_position_then_op23_user_area_then_optional_op356_completed_quest_state_then_op24_area_users",
		"body_source", "current_exe_op22_sub_1D83990_op23_sub_1D89590_op356_sub_1D58470_op24_sub_1D901D0")
	if err := s.sendCurrentTownLocationNotifications(
		session,
		session.selectedCharacterID,
		request.TownID,
		request.AreaID,
		selfRow,
		"current_exe_op36_town_area_transition",
	); err != nil {
		return rollbackLocation(err)
	}
	// Initial town bootstrap installs the completed-quest bitmap before op24.
	// Ordinary town transitions need the same order because op24 constructs the
	// destination scene; a later op356 cannot restore an omitted quest gate.
	if _, err := s.sendCurrentPersistedClearQuestListIfCompleted(
		ctx,
		session,
		repositories,
		"current_exe_op36_after_op23_before_op24_seed_completed_quest_scene_state",
	); err != nil {
		return rollbackLocation(err)
	}
	if err := s.sendGameUpperRawClass(session, currentSceneTransitionMsgID, response, 0); err != nil {
		return rollbackLocation(err)
	}
	// 进婚房要把房间现状（已摆放家具 + 情侣树）推给客户端；op36 这条路径不走
	// publishCurrentTownPresence，所以在这里补一次，否则进去看到空房间。
	if request.TownID == currentWeddingRoomTownID {
		if err := s.sendWeddingRoomState(session, character); err != nil {
			s.logGameEvent(session, "game-wedding-room-restore-failed", "error", err)
			return rollbackLocation(err)
		}
	}
	if err := s.sendSelectedActorUserStateRefreshIfNonDefault(
		session,
		"current_exe_op36_after_op24_reapply_persisted_visibility",
	); err != nil {
		return err
	}
	// 区域切换和回城一样会重建客户端场景：buff 栏 / 冷却表是进程内内存，op24 之后必须
	// 重放一次，否则公会属性（op487 VariousBuff 167/169）只要过一次图就消失，玩家要
	// 小退重上才恢复（2026-10-08 现场：回城 43.083 补过 buff，45.625 的区域切换又清掉）。
	// 位置已经落库、场景已经切换，所以这里只记日志不回滚：少一个图标不该让走位失败。
	if err := s.restoreCurrentSceneHudProjections(session, "town_area_transition_after_op24"); err != nil {
		s.logGameEvent(session, "game-town-area-transition-hud-restore-failed",
			"char_id", session.selectedCharacterID,
			"town_id", request.TownID,
			"area_id", request.AreaID,
			"error", err)
	}
	setCurrentTownPositionSceneLocked(session, session.selectedCharacterID, request.TownID, request.AreaID)
	if seriaExitPrevVillageRedirect {
		// The redirected exit consumed the preserved origin exactly like op1425.
		session.townPrevVillageSnapshot = currentTownPositionSnapshot{}
	}
	if townTransportRequest {
		// A world-map/teleport request already carries the authoritative arrival
		// coordinates. Freeze them now so the first gate op15 can bind op27
		// without waiting for an incidental later op35 movement report.
		session.townPositionSnapshot = currentTownPositionSnapshot{
			CharacterID:   session.selectedCharacterID,
			TownID:        request.TownID,
			AreaID:        request.AreaID,
			PositionX:     uint16(request.PositionX),
			PositionY:     uint16(request.PositionY),
			MovementCode:  request.Direction,
			PositionValid: true,
		}
	}
	session.initialTownRouteCharacterID = 0
	session.initialTownRouteStage = currentInitialTownRouteIdle
	session.completedTownReturnBuffRegistrationPending = false
	session.initialTownLocationNotificationsSent = false
	session.initialTownQuestSnapshotsSent = false
	session.initialTownQuestHUDRefreshSent = false
	session.initialTownGuildStateProjectionSent = false
	session.initialTownMode3RefreshPending = false
	session.currentFinishLoadingStateSent = false
	session.currentFinishLoadingCompletionSent = false
	session.postFinishLoadingPlayerStateSent = false
	session.initialTownLegacySceneReadyAccepted = false
	clearCurrentGameOptionRestoreSnapshot(session)
	if !currentCharacterOptionRestoreSnapshotIsInitializationProtected(session) {
		clearCurrentCharacterOptionRestoreSnapshot(session)
	}
	session.initialTownAdventureOverheadRefreshSent = false
	session.initialTownCombatPowerAffixesSent = false
	session.initialTownMoodProjectionSent = false
	session.enterSelectDungeonSent = false
	session.enterSelectDungeonAckSent = false
	session.enterSelectDungeonPassiveParty = false
	session.enterSelectDungeonContextSent = false
	session.backToVillageSelectorDuplicateGuard = false
	// The redirected Seria exit lands on a public town like op1425 does; it
	// produces no client-owned op15 transport finalizer.
	session.townTransportEnterSelectPending = townTransportRequest && !seriaExitPrevVillageRedirect
	session.townSceneReadyCharacterID = session.selectedCharacterID
	// 超时空：场景已经提交，把新的区域归属推给全团。放在这里(而不是紧接
	// moveRuntimeFiendWarGroup 之后)是因为客户端此刻才真正站在新区域里 ——
	// 早发会让别人看到你先于实际落点换了区域。
	if fiendWarMoveBefore.RaidKey != 0 {
		if moved, ok := s.runtimeRaidForMember(session.selectedCharacterID); ok {
			if err := s.broadcastRuntimeFiendWarBoard(moved, false, s.gameplayNow()); err != nil {
				s.logGameEvent(session, "game-fiendwar-move-board-publish-failed", "error", err)
			}
		}
	}

	// Register player in the new area and broadcast to others.
	var newAreaOthers []onlinePlayerInfo
	if s.onlinePlayers != nil {
		characterID := session.selectedCharacterID
		partyStateBeforeTownMove := runtimePartyStateSnapshot(session)
		playerInfo := &onlinePlayerInfo{
			CharacterID: characterID,
			AccountID:   character.AccountID,
			Name:        character.Name,
			Job:         byte(numericCharacterStat(character.Job)),
			GrowType:    byte(numericCharacterStatValue(character, "grow_type")),
			Level:       byte(character.Level),
			ChannelID:   townPresenceChannelID(session),
			TownID:      request.TownID,
			AreaID:      request.AreaID,
			InstanceKey: raidInstanceKey,
			PositionX:   uint16(request.PositionX),
			PositionY:   uint16(request.PositionY),
			Direction:   request.Direction,
			AreaState:   byte(areaState),
			Session:     session,
		}
		// A player store is scoped to one concrete town scene. Close it before
		// removing the owner's old-area presence so every old peer receives the
		// native close notification and stale visitors are detached.
		s.closeCurrentExpertJobStoreSession(session, true)
		s.closeCurrentPrivateStoreSession(session, true)
		// SET_USER_AREA re-registers the character below on its own, so a
		// dungeon-selector retirement memo is spent here rather than left for a
		// later selector return to replay against the old area.
		clearTownSelectorRetiredPresence(session)
		// Leave the old area. Ordinary peers receive op6. Party peers retain the
		// same actor identity and receive only its op23 area change, because op6
		// also removes that member from the current client's party manager.
		_, oldOthers := s.onlinePlayers.LeaveArea(characterID)
		if len(oldOthers) > 0 {
			s.broadcastTownPlayerAreaChange(playerInfo, oldOthers, partyStateBeforeTownMove)
		}
		s.promoteResidentGameSession(session, characterID)
		if privateRoomScene {
			// Every Seria room is a character-private instance; the PVP lobby is
			// likewise actor-invisible so rivals cannot see who idles there.
			// Party ownership remains session-scoped, but the town-presence
			// directory must contain no actor for this room and therefore emits
			// no co-presence creation.
			s.logGameEvent(session, "game-town-presence-suppressed",
				"source", "town_set_user_area_"+privateRoomKind,
				"char_id", characterID,
				"town_id", request.TownID,
				"area_id", request.AreaID,
				"map_path", area.MapPath,
				"retired_peer_count", len(oldOthers),
				"reason", privateRoomKind)
		} else {
			// Enter a public destination area only after the old scene actor has
			// been retired from its previous observers.
			newOthers := s.onlinePlayers.EnterArea(playerInfo)
			newAreaOthers = append(newAreaOthers, newOthers...)
			s.broadcastTownPlayerEnter(playerInfo, newOthers)
			s.replayCurrentExpertJobStores(session, request.TownID, request.AreaID)
			s.replayCurrentPrivateStores(session, request.TownID, request.AreaID)
		}
		// Same-channel peers standing in another area of this town get their actor
		// here, in BOTH branches. A private room suppresses this character own
		// co-presence, but must not suppress the record of everyone else: the client
		// resolves a peer target only through the actor record it was sent
		// (sub_1A089E0 -> sub_1C5B070 looks the key up in its own table and silently
		// does nothing when it misses). Skipping this call in a private room meant an
		// applicant standing in the raid channel Seria room could not resolve the
		// leader and never sent the join application -- measured: stepping out to the
		// deck made the same application succeed. The call only installs actors for
		// players in OTHER areas, so it cannot make a private room visible.
		s.sendChannelWidePeerActors(session, playerInfo, "channel_wide_peer_actor_after_town_area_enter")
	}
	// SET_USER_AREA rebuilds the current EXE's scene-owned party manager. Peer
	// actor projections alone are not sufficient: whichever member crosses the
	// town boundary last can otherwise retain only the selected actor row and
	// lose its own leader/member binding. Replay only the authoritative op9
	// roster after the new-area actors have been installed. The op11 endpoint
	// exchange belongs to party formation and must not be repeated here.
	partyStateAfterTownMove := runtimePartyStateSnapshot(session)
	if partyStateAfterTownMove.PartyID > 0 {
		if !privateRoomScene {
			if err := s.sendTownRemotePartyActors(session, partyStateAfterTownMove); err != nil {
				return err
			}
		}
		if err := s.sendRuntimePartyRosterLocal(session, partyStateAfterTownMove, "town_set_user_area_local_roster_rebind"); err != nil {
			return err
		}
		// Existing same-area party clients have just received the mover's new
		// mode0/mode1 actor. Refresh their selected local roster as the final
		// step so the HUD/minimap keeps the authoritative slot mapping.
		for i := range newAreaOthers {
			peer := newAreaOthers[i].Session
			if peer == nil || peer == session ||
				!containsRuntimePartyMember(runtimePartyMembers(partyStateAfterTownMove), newAreaOthers[i].CharacterID) {
				continue
			}
			peerState := runtimePartyStateSnapshot(peer)
			if peerState.PartyID != partyStateAfterTownMove.PartyID {
				continue
			}
			if err := s.sendRuntimePartyRosterLocal(peer, partyStateAfterTownMove, "town_set_user_area_same_area_peer_roster_rebind"); err != nil {
				return err
			}
		}
	}

	s.logGameEvent(session, "game-town-set-user-area-committed",
		"char_id", session.selectedCharacterID,
		"town_id", request.TownID,
		"area_id", request.AreaID,
		"raid_instance_key", raidInstanceKey,
		"position_x", request.PositionX,
		"position_y", request.PositionY,
		"map_path", area.MapPath,
		"teleport_array_request", teleportArrayRequest,
		"town_map_station_request", townMapStationRequest,
		"town_seria_room_portal_request", townSeriaRoomPortalRequest,
		"town_cross_town_portal_request", townCrossTownPortalRequest,
		"town_npc_enter_request", townNPCEnterRequest,
		"robot_public_placement_request", robotPublicPlacementRequest,
		"town_transport_request", townTransportRequest,
		"seria_exit_prev_village_redirect", seriaExitPrevVillageRedirect,
		"client_requested_town_id", clientRequestedTownID,
		"client_requested_area_id", clientRequestedAreaID,
		"target_need_quests", area.NeedQuests,
		"finish_loading_state_rearmed", true,
		"quest_refresh_after_dungeon_return", refreshQuestsAfterDungeonReturn)
	// The actor-bound inventory bootstrap owns the normal cold-login op898.
	// Keep the first client-authored op36 as a one-shot fallback for routes that
	// bypassed that bootstrap; by this point the current NoPack class0 handler
	// sub_1E7FAD0 can also see the inventory-panel owner. Keep this optional:
	// town movement must not fail merely because a minimal test runtime has no
	// premium/PVF catalog.
	if err := s.sendCurrentCrystalContractTownUIReadyState(session); err != nil {
		s.logGameEvent(session, "game-crystal-contract-town-area-state-deferred",
			"char_id", session.selectedCharacterID,
			"town_id", request.TownID,
			"area_id", request.AreaID,
			"reason", "runtime_pvf_or_repository_state_unavailable",
			"error", err)
	}
	// 卢克攻坚队待机室入场：官方文案「塔罗卡牌在团队频道入场时可以看见1次」，
	// 所以 1181 的下发点就在这里——raidInstanceKey != 0 只有当请求的
	// (town, area) 正好是本人所在攻坚队的待机室、且本人确实是该团成员时才成立
	// （见上面 waiting_instance_requires_raid_membership 那条拒绝）。
	// 安徒恩与其它场景在这一层就被 owner 检查挡掉，见 raid_luke_card_display.go。
	if raidInstanceKey != 0 {
		s.publishRuntimeRaidLukeCardDisplay(session, raidInstanceKey, "town_set_user_area_raid_waiting_room")
	}
	if refreshQuestsAfterDungeonReturn {
		return s.sendCurrentAcceptableQuestListForSession(session, "current_exe_op36_after_dungeon_return_committed")
	}
	return nil
}

// currentTownSetUserAreaPrevVillageRedirectLocked recognizes the current EXE's
// alternate exit from the shared Elvengard Seria-room map. Depending on the
// scene bootstrap path the client may send op36 for town 38 instead of op1425;
// treating that fixed map exit literally loses the character's preserved town.
// Route only that exact Seria-room exit shape to the durable prev-village
// origin. The caller holds session.townMu.
func (s *Service) currentTownSetUserAreaPrevVillageRedirectLocked(
	session *gameSession,
	character dnfrepo.CharacterRecord,
	request currentTownSetUserAreaRequest,
) (currentTownPositionSnapshot, bool) {
	if s == nil || session == nil || session.selectedCharacterID == 0 ||
		request.TownID != byte(newCharacterInitialTownID) ||
		request.AreaID == byte(newCharacterInitialAreaID) {
		return currentTownPositionSnapshot{}, false
	}
	// The redirect exists for the Seria-room exit door, whose op36 always
	// names town 38 no matter which town the room was entered from. The
	// teleport array inside the same room names a destination the player
	// picked, and 埃尔文防线 *is* town 38 - overriding that one sent the
	// player back to their previous town instead of the town they chose.
	if request.LooksLikeTeleportArraySelection() {
		return currentTownPositionSnapshot{}, false
	}
	currentTownID, currentAreaID, err := currentSceneTransitionLocation(character, true)
	if err != nil {
		return currentTownPositionSnapshot{}, false
	}
	currentArea, found := s.townArea(int64(currentTownID), int64(currentAreaID))
	if !found || !townMapPathLooksLikeSeria(currentArea.MapPath) {
		return currentTownPositionSnapshot{}, false
	}
	clientTargetArea, found := s.townArea(int64(request.TownID), int64(request.AreaID))
	if !found || townMapPathLooksLikeSeria(clientTargetArea.MapPath) {
		return currentTownPositionSnapshot{}, false
	}

	origin := session.townPrevVillageSnapshot
	if origin.CharacterID != session.selectedCharacterID || !origin.PositionValid {
		var ok bool
		origin, ok = currentTownPrevVillageOriginFromPersistedStats(session.selectedCharacterID, character)
		if !ok {
			return currentTownPositionSnapshot{}, false
		}
	}
	if origin.TownID == request.TownID && origin.AreaID == request.AreaID {
		return currentTownPositionSnapshot{}, false
	}
	if _, persistable := s.currentTownPrevVillageOriginIsPersistable(origin); !persistable {
		return currentTownPositionSnapshot{}, false
	}
	targetArea, found := s.townArea(int64(origin.TownID), int64(origin.AreaID))
	if !found || townMapPathLooksLikeSeria(targetArea.MapPath) {
		return currentTownPositionSnapshot{}, false
	}
	return origin, true
}

func (s *Service) townAreaNeedQuestsCompleted(
	ctx context.Context,
	repositories dnfrepo.Group,
	characterID string,
	needQuests []int64,
) (bool, error) {
	if len(needQuests) == 0 {
		return true, nil
	}
	if repositories.Quest == nil {
		return false, dnfrepo.ErrRepoMissing
	}
	record, found, err := repositories.Quest.Load(ctx, characterID)
	if err != nil {
		return false, err
	}
	if !found {
		return false, nil
	}
	for _, questID := range needQuests {
		if questID <= 0 {
			continue
		}
		if !townQuestStateCompleted(record, questID) {
			return false, nil
		}
	}
	return true, nil
}

func townQuestStateCompleted(record dnfrepo.QuestRecord, questID int64) bool {
	completed := false
	if state, ok := record.Progress[questID]; ok {
		completed = isTownQuestCompletedStatus(state.Status)
	}
	if state, ok := record.States[questID]; ok {
		completed = isTownQuestCompletedStatus(state.Status)
	}
	return completed
}

func isTownQuestCompletedStatus(status string) bool {
	switch normalizeDungeonQuestStatus(status) {
	case "complete", "completed", "cleared", "finished", "done":
		return true
	default:
		return false
	}
}

// LooksLikeTeleportArraySelection identifies the current client's town
// teleport-array selection shape observed from the live 23.4.15.0 EXE. The
// destination itself is still validated against the runtime PVF town catalog;
// this only prevents server-side quest/topology guards from rejecting a
// client-visible teleport destination before op24 can be emitted.
//
// Live op36 bodies captured from the Seria-room array (opaqueA = the town the
// array stands in, opaqueB = 1, opaqueU32 = 0):
//
//	town 40 area 1 -> tail 5      town 39 area 2 -> tail 5
//	town 38 area 0 -> tail 6      (the array's own town)
//
// Only the tail moves, so pinning it to 5 misread a selection of the array's
// own town as an ordinary route body. Every non-array body in the same capture
// - route clicks, the Seria-room exit door, cross-town portals, map stations -
// carries tail 0, so the two tails stay disjoint from all of them.
func (request currentTownSetUserAreaRequest) LooksLikeTeleportArraySelection() bool {
	if request.OpaqueTailU8 != 5 && request.OpaqueTailU8 != 6 {
		return false
	}
	return request.OpaqueU32 == 0 && request.OpaqueU16A != 0
}

// LooksLikeTownMapStationSelection identifies the current EXE world-map /
// town transfer-station op36 shape observed live from West Coast:
// town=40, area=0, opaqueA=40, opaqueB=1, opaqueU32=0, tail=0. It is distinct
// from ordinary route-click bodies whose opaque fields are zero.
func (request currentTownSetUserAreaRequest) LooksLikeTownMapStationSelection() bool {
	return request.OpaqueTailU8 == 0 &&
		request.OpaqueU32 == 0 &&
		request.OpaqueU16A != 0 &&
		request.OpaqueU16B != 0
}

// LooksLikeTownSeriaRoomPortalSelection identifies the current EXE town
// in-scene Seria-room portal shape observed live from West Coast:
// source town=40, target town=38/area=1, opaqueA=40, opaqueB=0, opaqueU32=0,
// tail=0. The target area is still resolved from runtime PVF and must be a
// Seria-room map; this only admits the client's explicit portal request across
// town ownership boundaries.
func (request currentTownSetUserAreaRequest) LooksLikeTownSeriaRoomPortalSelection(currentTown int64, targetMapPath string) bool {
	if currentTown <= 0 {
		return false
	}
	return request.LooksLikeTownCrossTownPortalSelection(currentTown) &&
		townMapPathLooksLikeSeria(targetMapPath)
}

// LooksLikeTownCrossTownPortalSelection identifies the current EXE in-scene
// cross-town portal shape observed from West Coast to HendonMyre:
// target town=39/area=0, opaqueA=current town 40, opaqueB=0, opaqueU32=0,
// tail=0. The destination is still resolved from the runtime PVF town catalog;
// this only admits a client-emitted portal body whose source-town ownership is
// explicit in the request.
func (request currentTownSetUserAreaRequest) LooksLikeTownCrossTownPortalSelection(currentTown int64) bool {
	if currentTown <= 0 {
		return false
	}
	return request.OpaqueTailU8 == 0 &&
		request.OpaqueU32 == 0 &&
		request.OpaqueU16A != 0 &&
		int64(request.OpaqueU16A) == currentTown &&
		request.OpaqueU16B == 0 &&
		int64(request.TownID) != currentTown
}

// LooksLikeTownNPCEnterSelection identifies the current EXE NPC [enter town]
// op36 shape. The target NPC must still be proven against the runtime PVF town
// catalog before this request is admitted as a cross-town route.
func (request currentTownSetUserAreaRequest) LooksLikeTownNPCEnterSelection(currentTown int64) bool {
	if currentTown <= 0 {
		return false
	}
	return request.OpaqueTailU8 == 0 &&
		request.OpaqueU32 != 0 &&
		request.OpaqueU16B == 1 &&
		int64(request.OpaqueU16A) == currentTown &&
		int64(request.TownID) != currentTown
}

func (request currentTownSetUserAreaRequest) LooksLikeTownTransportSelection() bool {
	return request.LooksLikeTeleportArraySelection() || request.LooksLikeTownMapStationSelection()
}

func (s *Service) logTownSetUserAreaBlocked(session *gameSession, request currentTownSetUserAreaRequest, reason string) {
	s.logGameEvent(session, "game-town-set-user-area-blocked",
		"char_id", session.selectedCharacterID,
		"town_id", request.TownID,
		"area_id", request.AreaID,
		"position_x", request.PositionX,
		"position_y", request.PositionY,
		"direction", request.Direction,
		"opaque_u16_a", request.OpaqueU16A,
		"opaque_u16_b", request.OpaqueU16B,
		"opaque_u32", request.OpaqueU32,
		"opaque_tail_u8", request.OpaqueTailU8,
		"reason", reason)
}
