package dnfbridge

import (
	"context"
	"strconv"
	"strings"
	"time"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

const (
	currentTownPrevVillageTownStat      = "prev_village_town_id"
	currentTownPrevVillageAreaStat      = "prev_village_area_id"
	currentTownPrevVillagePosXStat      = "prev_village_pos_x"
	currentTownPrevVillagePosYStat      = "prev_village_pos_y"
	currentTownPrevVillageDirectionStat = "prev_village_direction"
)

func townMapPathLooksLikeSeria(mapPath string) bool {
	normalizedPath := strings.ToLower(strings.ReplaceAll(mapPath, "\\", "/"))
	return strings.Contains(normalizedPath, "seria")
}

// currentRaidGateAreaIsSeriaRoom reports whether this session is standing in the
// Seria-room equivalent of a raid channel.
//
// townMapPathLooksLikeSeria matches on the map file name, and every raid gate
// names its own file while importing the shared Seria-room art instead:
//
//	map/town/battleship/(f)gate_battleship.map  -> [import script] Common/(F)Gate_Seria.map
//	map/town/jelva_raid/(f)gate_jelva.map       -> [import script] Common/(R)Gate_Seria.map
//	map/town/battleship2/(f)gate_battleship.map -> [import script] Common/(F)Gate_Seria.map
//
// None of those paths contains "seria", so the name check misses them and the
// room is published as an ordinary public area: every player standing in a raid
// channel's Seria room becomes mutually visible. Ordinary towns stay private
// only because their map happens to be (F)new_seria_room.map.
//
// The raid town spec is the authoritative discriminator. A raid town's areaID is
// its gate area (Anton 19/0, Luke 32/0), reached both at login and by walking
// through the town map, and the raid transition gate rejects every other town, so
// this identifies exactly the room the client renders as Seria's.
func currentRaidGateAreaIsSeriaRoom(session *gameSession, townID, areaID byte) bool {
	spec, ok := currentRaidTownSpecForSession(session)
	if !ok {
		return false
	}
	return int64(townID) == spec.townID && int64(areaID) == spec.areaID
}

func currentTownPrevVillageOriginLocked(
	session *gameSession,
	characterID uint16,
	character dnfrepo.CharacterRecord,
) (currentTownPositionSnapshot, bool) {
	if session == nil || characterID == 0 {
		return currentTownPositionSnapshot{}, false
	}
	townID, areaID, err := currentSceneTransitionLocation(character, true)
	if err != nil {
		return currentTownPositionSnapshot{}, false
	}
	if snapshot := session.townPositionSnapshot; snapshot.CharacterID == characterID &&
		snapshot.TownID == townID &&
		snapshot.AreaID == areaID &&
		snapshot.PositionValid {
		return snapshot, true
	}
	posX, okX := character.Stats["pos_x"]
	posY, okY := character.Stats["pos_y"]
	direction, okDirection := character.Stats["direction"]
	if !okX || !okY || !okDirection ||
		posX < 0 || posX > int64(^uint16(0)) ||
		posY < 0 || posY > int64(^uint16(0)) ||
		direction < 0 || direction > int64(^byte(0)) {
		return currentTownPositionSnapshot{}, false
	}
	return currentTownPositionSnapshot{
		CharacterID:   characterID,
		TownID:        townID,
		AreaID:        areaID,
		PositionX:     uint16(posX),
		PositionY:     uint16(posY),
		MovementCode:  byte(direction),
		PositionValid: true,
	}, true
}

func currentTownPrevVillageOriginFromPersistedStats(
	characterID uint16,
	character dnfrepo.CharacterRecord,
) (currentTownPositionSnapshot, bool) {
	if characterID == 0 || character.Stats == nil {
		return currentTownPositionSnapshot{}, false
	}
	townID, okTown := character.Stats[currentTownPrevVillageTownStat]
	areaID, okArea := character.Stats[currentTownPrevVillageAreaStat]
	posX, okX := character.Stats[currentTownPrevVillagePosXStat]
	posY, okY := character.Stats[currentTownPrevVillagePosYStat]
	direction, okDirection := character.Stats[currentTownPrevVillageDirectionStat]
	if !okTown || !okArea || !okX || !okY || !okDirection ||
		townID < 0 || townID > int64(^byte(0)) ||
		areaID < 0 || areaID > int64(^byte(0)) ||
		posX < 0 || posX > int64(^uint16(0)) ||
		posY < 0 || posY > int64(^uint16(0)) ||
		direction < 0 || direction > int64(^byte(0)) {
		return currentTownPositionSnapshot{}, false
	}
	return currentTownPositionSnapshot{
		CharacterID:   characterID,
		TownID:        byte(townID),
		AreaID:        byte(areaID),
		PositionX:     uint16(posX),
		PositionY:     uint16(posY),
		MovementCode:  byte(direction),
		PositionValid: true,
	}, true
}

func currentTownPrevVillageApplyOriginStats(stats map[string]int64, origin currentTownPositionSnapshot) bool {
	if stats == nil || origin.CharacterID == 0 || !origin.PositionValid {
		return false
	}
	values := map[string]int64{
		currentTownPrevVillageTownStat:      int64(origin.TownID),
		currentTownPrevVillageAreaStat:      int64(origin.AreaID),
		currentTownPrevVillagePosXStat:      int64(origin.PositionX),
		currentTownPrevVillagePosYStat:      int64(origin.PositionY),
		currentTownPrevVillageDirectionStat: int64(origin.MovementCode),
	}
	changed := false
	for key, value := range values {
		if current, ok := stats[key]; !ok || current != value {
			stats[key] = value
			changed = true
		}
	}
	return changed
}

// currentTownPrevVillageClearAntonOriginStats removes the fields written by
// an older build when it incorrectly persisted the temporary Anton town as a
// normal-channel return location. They must be cleared atomically with the
// normal Seria recovery; merely refusing to use them leaves the client
// replaying op1425 with the stale team map on every reconnect.
func currentTownPrevVillageClearAntonOriginStats(stats map[string]int64) bool {
	if stats == nil || stats[currentTownPrevVillageTownStat] != int64(currentAntonRaidTownID) {
		return false
	}
	changed := false
	for _, key := range []string{
		currentTownPrevVillageTownStat,
		currentTownPrevVillageAreaStat,
		currentTownPrevVillagePosXStat,
		currentTownPrevVillagePosYStat,
		currentTownPrevVillageDirectionStat,
	} {
		if _, ok := stats[key]; ok {
			delete(stats, key)
			changed = true
		}
	}
	return changed
}

func (s *Service) currentTownPrevVillageOriginMapPath(origin currentTownPositionSnapshot) (string, bool) {
	if s == nil || origin.CharacterID == 0 || !origin.PositionValid {
		return "", false
	}
	area, found := s.townArea(int64(origin.TownID), int64(origin.AreaID))
	if !found || area.MapPath == "" {
		return "", false
	}
	return area.MapPath, true
}

func (s *Service) currentTownPrevVillageOriginIsPersistable(origin currentTownPositionSnapshot) (string, bool) {
	mapPath, ok := s.currentTownPrevVillageOriginMapPath(origin)
	// 婚房(101)必须排除：它是一间私密房间，把它记成"上个城镇"会让下次 op1425
	// 把玩家送回婚房里（上游同一处理见 currentWeddingRoomTownID）。
	if !ok || townMapPathLooksLikeSeria(mapPath) || origin.TownID == byte(currentAntonRaidTownID) ||
		origin.TownID == currentWeddingRoomTownID {
		return mapPath, false
	}
	return mapPath, true
}

// currentWeddingPrevVillageOrigin 修复"被婚房污染过的回城点"。
//
// 触发条件（任一）：
//   - 角色当前就在婚房(101)——刚办完仪式，记忆里的回城点可能已经被覆盖成婚房；
//   - 记忆的回城点本身就指向 101——直接放行会把玩家送回婚房。
//
// 修复方式与上游一致：换成运行时 PVF 推导出的赛丽亚房间门边安全落点。
// 这样 op1425 既不会循环回婚房，也不会因为"拒绝"而变成一个什么都不做的死键。
// 返回的落点由调用方豁免 currentTownPrevVillageOriginIsPersistable（赛丽亚房间
// 本身按定义是不可持久化的，但作为一次性到达锚点是正确的）。
func (s *Service) currentWeddingPrevVillageOrigin(
	characterID uint16, origin currentTownPositionSnapshot, character dnfrepo.CharacterRecord,
) (currentTownPositionSnapshot, bool) {
	if s == nil || characterID == 0 {
		return currentTownPositionSnapshot{}, false
	}
	contaminated := character.Stats["town_id"] == currentWeddingRoomTownID ||
		character.Stats[currentTownPrevVillageTownStat] == currentWeddingRoomTownID ||
		origin.TownID == currentWeddingRoomTownID
	if !contaminated {
		return currentTownPositionSnapshot{}, false
	}
	return s.currentWeddingPublicReturnOrigin(characterID)
}

func (s *Service) handleCurrentPrevVillage(session *gameSession, requestBody []byte) error {
	if session == nil {
		return nil
	}
	if len(requestBody) != 0 {
		s.logCurrentPrevVillageBlocked(session, currentTownPositionSnapshot{}, "current_exe_op1425_body_boundary_mismatch")
		return nil
	}
	characterID := session.selectedCharacterID
	if characterID == 0 {
		s.logCurrentPrevVillageBlocked(session, currentTownPositionSnapshot{}, "selected_character_missing")
		return nil
	}
	session.dungeon.mu.Lock()
	hasDungeonRuntime := session.dungeon.runtime != nil
	session.dungeon.mu.Unlock()
	if hasDungeonRuntime {
		s.logCurrentPrevVillageBlocked(session, currentTownPositionSnapshot{}, "active_dungeon_runtime_rejects_prev_village")
		return nil
	}
	session.townMu.Lock()
	readyCharacterID := session.townSceneReadyCharacterID
	origin := session.townPrevVillageSnapshot
	session.townMu.Unlock()
	if readyCharacterID != characterID {
		s.logCurrentPrevVillageBlocked(session, origin, "town_scene_player_state_not_finalized")
		return nil
	}

	repositories, ok := s.repositoryGroup()
	if !ok || repositories.Character == nil {
		s.logCurrentPrevVillageBlocked(session, origin, "character_repository_unavailable")
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	characterKey := strconv.FormatUint(uint64(characterID), 10)
	character, found, err := repositories.Character.Load(ctx, characterKey)
	if err != nil {
		return err
	}
	if !found || character.Stats == nil {
		s.logCurrentPrevVillageBlocked(session, origin, "selected_character_location_missing")
		return nil
	}
	if origin.CharacterID != characterID || !origin.PositionValid {
		if persistedOrigin, ok := currentTownPrevVillageOriginFromPersistedStats(characterID, character); ok {
			origin = persistedOrigin
			session.townMu.Lock()
			session.townPrevVillageSnapshot = origin
			session.townMu.Unlock()
			s.logTownPrevVillageOriginBound(session, origin, "current_exe_op1425_restored_from_persisted_prev_village")
		}
	}
	if origin.CharacterID != characterID || !origin.PositionValid {
		s.logCurrentPrevVillageBlocked(session, origin, "prev_village_origin_missing")
		return nil
	}
	// 婚房污染修复。必须在 persistable 检查之前：婚房的地图不是赛丽亚房间也不是
	// 安东攻坚城镇，persistable 会**放行**它，于是玩家被送回婚房而不是上个城镇。
	// 恢复出来的锚点是一次性到达点，所以下面豁免 persistable 检查。
	weddingRecovered := false
	if recovered, ok := s.currentWeddingPrevVillageOrigin(characterID, origin, character); ok {
		origin = recovered
		weddingRecovered = true
		session.townMu.Lock()
		session.townPrevVillageSnapshot = origin
		session.townMu.Unlock()
		s.logTownPrevVillageOriginBound(session, origin,
			"current_exe_op1425_wedding_room_stale_origin_runtime_pvf_seria_arrival")
	}
	if _, persistable := s.currentTownPrevVillageOriginIsPersistable(origin); !persistable && !weddingRecovered {
		s.logCurrentPrevVillageBlocked(session, origin, "prev_village_origin_is_temporary_or_non_persistable")
		return nil
	}
	if doorOrigin, mapPath, projected := s.currentTownPrevVillageDoorOrigin(origin); projected {
		origin = doorOrigin
		currentTownPrevVillageApplyOriginStats(character.Stats, origin)
		s.logTownPrevVillageOriginBound(session, origin, "current_exe_op1425_runtime_pvf_seria_door")
		s.logGameEvent(session, "game-town-prev-village-door-projected",
			"char_id", characterID,
			"town_id", origin.TownID,
			"area_id", origin.AreaID,
			"position_x", origin.PositionX,
			"position_y", origin.PositionY,
			"map_path", mapPath,
			"source", "runtime_pvf_reverse_portal_to_town_38_area_1")
	}
	currentTownID, currentAreaID, err := currentSceneTransitionLocation(character, true)
	if err != nil {
		s.logCurrentPrevVillageBlocked(session, origin, err.Error())
		return nil
	}
	currentArea, found := s.townArea(int64(currentTownID), int64(currentAreaID))
	if !found || !townMapPathLooksLikeSeria(currentArea.MapPath) {
		s.logCurrentPrevVillageBlocked(session, origin, "current_area_is_not_seria_room")
		return nil
	}
	targetArea, found := s.townArea(int64(origin.TownID), int64(origin.AreaID))
	if !found {
		s.logCurrentPrevVillageBlocked(session, origin, "prev_village_target_area_missing_from_runtime_pvf")
		return nil
	}
	areaState, ok := character.Stats["area_state"]
	if !ok || areaState < 0 || areaState > int64(^byte(0)) {
		s.logCurrentPrevVillageBlocked(session, origin, "selected_character_area_state_missing_or_invalid")
		return nil
	}
	direction := origin.MovementCode

	previous := dnfrepo.CloneCharacter(character)
	character = dnfrepo.CloneCharacter(character)
	character.Stats["town_id"] = int64(origin.TownID)
	character.Stats["area_id"] = int64(origin.AreaID)
	character.Stats["pos_x"] = int64(origin.PositionX)
	character.Stats["pos_y"] = int64(origin.PositionY)
	character.Stats["direction"] = int64(direction)
	character.UpdatedAt = time.Now().UTC()
	if err := dnfrepo.SaveCharacterFields(ctx, repositories.Character, character, dnfrepo.CharacterFieldStats); err != nil {
		return err
	}

	row := currentSceneTransitionRow{
		ObjectOrResourceKey: currentSceneActorObjectKey(characterID),
		Value1:              origin.PositionX,
		Value2:              origin.PositionY,
		Value3:              byte(direction),
		Value4:              byte(areaState),
	}
	response, err := buildCurrentSceneTransitionBody(origin.TownID, origin.AreaID, []currentSceneTransitionRow{row})
	if err != nil {
		return err
	}
	rollbackLocation := func(writeErr error) error {
		rollbackCtx, rollbackCancel := context.WithTimeout(context.Background(), createWriteTimeout)
		rollbackErr := dnfrepo.SaveCharacterFields(rollbackCtx, repositories.Character, previous, dnfrepo.CharacterFieldStats)
		rollbackCancel()
		if rollbackErr != nil {
			s.logGameEvent(session, "game-town-prev-village-rollback-failed",
				"char_id", characterID,
				"town_id", origin.TownID,
				"area_id", origin.AreaID,
				"write_error", writeErr,
				"rollback_error", rollbackErr)
		}
		return writeErr
	}
	s.logGameEvent(session, "game-town-prev-village-transition-send",
		"char_id", characterID,
		"request_msg_id", uint16(1425),
		"response_msg_ids", []uint16{
			currentTownUserPositionNotificationMsgID,
			currentTownUserAreaNotificationMsgID,
			currentClearQuestListMsgID,
			currentSceneTransitionMsgID,
		},
		"town_id", origin.TownID,
		"area_id", origin.AreaID,
		"position_x", origin.PositionX,
		"position_y", origin.PositionY,
		"direction", int64(direction),
		"area_state", areaState,
		"map_path", targetArea.MapPath,
		"op24_body_len", len(response),
		"sequence", "op22_user_position_then_op23_user_area_then_optional_op356_completed_quest_state_then_op24_area_users",
		"body_source", "current_exe_op22_sub_1D83990_op23_sub_1D89590_op356_sub_1D58470_op24_sub_1D901D0_from_legacy_op1425_prev_village")
	// A direct Seria-room return creates the same public-town scene as an op36
	// portal move.  AREA_USERS/op24 alone only installs the roster; the current
	// client also needs op22/op23 to bind the controlled actor and active area,
	// plus the persisted completed-quest bitmap before op24 constructs map gates.
	// Omitting that prefix leaves the dungeon selector with a valid permission
	// table but no visible map rows until an unrelated town portal repairs it.
	if err := s.sendCurrentTownLocationNotifications(
		session,
		characterID,
		origin.TownID,
		origin.AreaID,
		row,
		"current_exe_op1425_prev_village_transition",
	); err != nil {
		return rollbackLocation(err)
	}
	if _, err := s.sendCurrentPersistedClearQuestListIfCompleted(
		ctx,
		session,
		repositories,
		"current_exe_op1425_after_op23_before_op24_seed_completed_quest_scene_state",
	); err != nil {
		return rollbackLocation(err)
	}
	if err := s.sendGameUpperRawClass(session, currentSceneTransitionMsgID, response, 0); err != nil {
		return rollbackLocation(err)
	}
	if err := s.sendSelectedActorUserStateRefreshIfNonDefault(
		session,
		"current_exe_op1425_after_op24_reapply_persisted_visibility",
	); err != nil {
		return err
	}

	session.townMu.Lock()
	setCurrentTownPositionSceneLocked(session, characterID, origin.TownID, origin.AreaID)
	session.townPrevVillageSnapshot = currentTownPositionSnapshot{}
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
	session.townSceneReadyCharacterID = characterID
	session.townMu.Unlock()
	s.publishCurrentTownPresence(
		session,
		character,
		origin.TownID,
		origin.AreaID,
		row,
		0,
		targetArea.MapPath,
		"prev_village_return_from_private_seria_room",
	)
	s.logGameEvent(session, "game-town-prev-village-committed",
		"char_id", characterID,
		"town_id", origin.TownID,
		"area_id", origin.AreaID,
		"position_x", origin.PositionX,
		"position_y", origin.PositionY,
		"map_path", targetArea.MapPath,
		"finish_loading_state_rearmed", true)
	return nil
}

func (s *Service) logCurrentPrevVillageBlocked(session *gameSession, origin currentTownPositionSnapshot, reason string) {
	s.logGameEvent(session, "game-town-prev-village-blocked",
		"char_id", session.selectedCharacterID,
		"origin_character_id", origin.CharacterID,
		"origin_town_id", origin.TownID,
		"origin_area_id", origin.AreaID,
		"origin_position_x", origin.PositionX,
		"origin_position_y", origin.PositionY,
		"origin_position_valid", origin.PositionValid,
		"reason", reason)
}

func (s *Service) logTownPrevVillageOriginBound(session *gameSession, origin currentTownPositionSnapshot, source string) {
	s.logGameEvent(session, "game-town-prev-village-origin-bound",
		"char_id", session.selectedCharacterID,
		"origin_town_id", origin.TownID,
		"origin_area_id", origin.AreaID,
		"origin_position_x", origin.PositionX,
		"origin_position_y", origin.PositionY,
		"origin_position_valid", origin.PositionValid,
		"source", source)
}

func (s *Service) logTownPrevVillageOriginBindFailed(session *gameSession, characterID uint16, reason string) {
	s.logGameEvent(session, "game-town-prev-village-origin-bind-failed",
		"char_id", characterID,
		"reason", reason)
}
