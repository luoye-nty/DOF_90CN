package dnfbridge

import (
	"encoding/binary"
	"errors"
	"fmt"
)

const currentTownSetUserPositionBodySize = 7

var errCurrentDungeonTownReturnOriginUnavailable = errors.New("current dungeon town-return origin is unavailable")

// currentTownSetUserPositionRequest is the exact plaintext written by the
// current EXE's sub_23E0660: coordinates, direction, then actor+0x748 scaled
// by 100. That final u16 is the mover's current town movement rate, not an
// opaque field: preserve it in op22 because sub_23D9DE0 applies it to peers.
type currentTownSetUserPositionRequest struct {
	PositionX       uint16
	PositionY       uint16
	MovementCode    byte
	OpaqueScaledU16 uint16
}

// currentTownPositionSnapshot is session-owned. The repository remains the
// durable area owner; high-frequency op35 reports are not written to the DB.
// A snapshot can be reused only for the exact character/town/area scene that
// established it.
type currentTownPositionSnapshot struct {
	CharacterID     uint16
	TownID          byte
	AreaID          byte
	PositionX       uint16
	PositionY       uint16
	MovementCode    byte
	OpaqueScaledU16 uint16
	PositionValid   bool
}

func parseCurrentTownSetUserPositionRequest(body []byte) (currentTownSetUserPositionRequest, error) {
	if len(body) != currentTownSetUserPositionBodySize {
		return currentTownSetUserPositionRequest{}, fmt.Errorf(
			"current town op35 body length %d, want %d",
			len(body),
			currentTownSetUserPositionBodySize,
		)
	}
	return currentTownSetUserPositionRequest{
		PositionX:       binary.LittleEndian.Uint16(body[0:2]),
		PositionY:       binary.LittleEndian.Uint16(body[2:4]),
		MovementCode:    body[4],
		OpaqueScaledU16: binary.LittleEndian.Uint16(body[5:7]),
	}, nil
}

func (s *Service) handleTownSetUserPosition(session *gameSession, body []byte) error {
	if session == nil {
		return nil
	}
	s.pvpMu.Lock()
	inAssault := session.assault != nil
	s.pvpMu.Unlock()
	if inAssault {
		return nil
	}
	request, err := parseCurrentTownSetUserPositionRequest(body)
	if err != nil {
		s.logGameEvent(session, "game-town-set-user-position-blocked",
			"body_len", len(body),
			"reason", "current_exe_op35_writer_boundary_mismatch",
			"error", err)
		return nil
	}
	characterID := session.selectedCharacterID
	if characterID == 0 {
		s.logTownSetUserPositionBlocked(session, request, "selected_character_missing")
		return nil
	}

	if err := s.commitPendingDungeonReturnBeforeTownPosition(session, request); err != nil {
		return err
	}

	session.dungeon.mu.Lock()
	runtime := session.dungeon.runtime
	if runtime != nil {
		runtime.actorPositionX = request.PositionX
		runtime.actorPositionY = request.PositionY
		runtime.actorPositionValid = true
	}
	session.dungeon.mu.Unlock()
	if runtime != nil {
		return nil
	}

	session.townMu.Lock()
	readyCharacterID := session.townSceneReadyCharacterID
	snapshot := session.townPositionSnapshot
	if readyCharacterID == characterID && snapshot.CharacterID == characterID {
		snapshot.PositionX = request.PositionX
		snapshot.PositionY = request.PositionY
		snapshot.MovementCode = request.MovementCode
		snapshot.OpaqueScaledU16 = request.OpaqueScaledU16
		snapshot.PositionValid = true
		session.townPositionSnapshot = snapshot
	}
	session.townMu.Unlock()

	if readyCharacterID != characterID {
		s.logTownSetUserPositionBlocked(session, request, "town_scene_player_state_not_finalized")
		return nil
	}
	if snapshot.CharacterID != characterID {
		s.logTownSetUserPositionBlocked(session, request, "town_scene_location_owner_missing")
		return nil
	}

	s.logGameEvent(session, "game-town-set-user-position-captured",
		"char_id", characterID,
		"town_id", snapshot.TownID,
		"area_id", snapshot.AreaID,
		"position_x", request.PositionX,
		"position_y", request.PositionY,
		"movement_code", request.MovementCode,
		"opaque_scaled_u16", request.OpaqueScaledU16,
		"persisted", false,
		"source", "current_exe_sub_23E0660_exact_plaintext")

	// A town-side op35 with a confirmed position proves this member rebuilt its
	// town actor and resumed movement, so consume the back-to-village
	// selector-return marker here. Every party member sends this arrival op35
	// (a follower never walks to the gate and so never sends op15), which is the
	// only way currentPartyTownReturnSceneReady can pass for all of them and the
	// party co-presence can be rebuilt.
	if err := s.consumeBackToVillageEnterSelectFinalizerAfterTownPosition(
		session,
		"current_exe_op35_position_after_pending_town_return",
	); err != nil {
		return err
	}

	// Broadcast position to other players in the same area.
	if s.onlinePlayers != nil {
		others := s.onlinePlayers.UpdatePosition(characterID, request.PositionX, request.PositionY)
		if len(others) > 0 {
			mover := &onlinePlayerInfo{
				CharacterID: characterID,
				ChannelID:   townPresenceChannelID(session),
				TownID:      snapshot.TownID,
				AreaID:      snapshot.AreaID,
				PositionX:   request.PositionX,
				PositionY:   request.PositionY,
				Direction:   request.MovementCode,
				Session:     session,
			}
			s.broadcastTownPlayerMove(mover, request.OpaqueScaledU16, others)
		}
	}
	if err := s.reconcileCurrentTownNPCQuestMeetings(session, snapshot); err != nil {
		return err
	}
	return nil
}

// commitPendingDungeonReturnBeforeTownPosition accepts the first town-side
// op35 position report as proof that a previously written typed op24 return
// was consumed by the client. Live Tower death logs show exactly this shape:
// op40 -> op32 -> client 10s window -> server 12s watchdog -> op24, then the client sends op35 with the frozen
// return coordinates. Blocking that op35 leaves the active dungeon runtime
// attached forever and the corpse never reaches a usable town scene.
func (s *Service) commitPendingDungeonReturnBeforeTownPosition(
	session *gameSession,
	request currentTownSetUserPositionRequest,
) error {
	if session == nil || session.selectedCharacterID == 0 {
		return nil
	}
	characterID := session.selectedCharacterID

	session.dungeon.mu.Lock()
	runtime := session.dungeon.runtime
	shouldCommit := runtime != nil && runtime.Session != nil &&
		runtime.townReturnPending && runtime.townReturnOp24Sent
	transition := currentDungeonTownTransition{}
	if shouldCommit {
		transition = cloneCurrentDungeonTownTransition(runtime.townReturnTransition)
	}
	session.dungeon.mu.Unlock()
	if !shouldCommit {
		// The runtime is detached before the staged town actor/HUD chain is
		// written. If an earlier confirmation reached that boundary but a
		// later mode0/mode1/creature/skill write failed, op35 is the next
		// current-client town proof and must resume only the unfinished suffix.
		return s.ensureCurrentConfirmedDungeonReturnPlayerState(
			session,
			"current_exe_op35_position_without_retained_runtime",
		)
	}

	if err := s.commitPendingDungeonReturnForSceneRequest(session, "current_exe_op35_position_after_pending_town_return"); err != nil {
		return err
	}

	session.dungeon.mu.Lock()
	stillHasRuntime := session.dungeon.runtime != nil
	session.dungeon.mu.Unlock()
	if stillHasRuntime || session.selectedCharacterID != characterID {
		s.logGameEvent(session, "game-town-set-user-position-pending-return-committed",
			"char_id", characterID,
			"selected_character", session.selectedCharacterID,
			"runtime_retained", stillHasRuntime,
			"reason", "pending_return_committed_but_scene_owner_changed")
		return nil
	}

	session.townMu.Lock()
	clearCurrentTownSelectorOriginLocked(session)
	session.townSceneReadyCharacterID = characterID
	session.townPositionSnapshot = currentTownPositionSnapshot{
		CharacterID:     characterID,
		TownID:          transition.TownID,
		AreaID:          transition.AreaID,
		PositionX:       request.PositionX,
		PositionY:       request.PositionY,
		MovementCode:    request.MovementCode,
		OpaqueScaledU16: request.OpaqueScaledU16,
		PositionValid:   true,
	}
	session.townMu.Unlock()

	s.logGameEvent(session, "game-town-set-user-position-pending-return-committed",
		"char_id", characterID,
		"town_id", transition.TownID,
		"area_id", transition.AreaID,
		"position_x", request.PositionX,
		"position_y", request.PositionY,
		"movement_code", request.MovementCode,
		"opaque_scaled_u16", request.OpaqueScaledU16,
		"position_source", transition.PositionSource,
		"reason", "op35_is_first_town_side_confirmation_after_typed_op24_return")
	return nil
}

// setCurrentTownPositionSceneLocked establishes the exact town scene that may
// own later op35 reports. The caller must hold session.townMu. A scene change
// invalidates the previous coordinate report even when the same character is
// moving between two areas of the same town.
func setCurrentTownPositionSceneLocked(session *gameSession, characterID uint16, townID, areaID byte) {
	if session == nil {
		return
	}
	clearCurrentTownSelectorOriginLocked(session)
	session.townMeetingNearbyNPCs = nil
	session.townPositionSnapshot = currentTownPositionSnapshot{
		CharacterID: characterID,
		TownID:      townID,
		AreaID:      areaID,
	}
}

// setCurrentTownPositionSceneFromTransitionLocked seeds the position owner
// from the exact typed op24 actor row. Character switching can open the
// dungeon selector before the first client op35 movement report; retaining an
// invalid position in that window acknowledges op15 but cannot build op27,
// leaving the selector blank until a teleport supplies a later op35. The
// transition row is already the authoritative spawn used by the current
// client, so it is safe to bind immediately when it belongs to this actor.
func setCurrentTownPositionSceneFromTransitionLocked(session *gameSession, characterID uint16, townID, areaID byte, row currentSceneTransitionRow) {
	setCurrentTownPositionSceneLocked(session, characterID, townID, areaID)
	if session == nil || characterID == 0 || row.ObjectOrResourceKey != currentSceneActorObjectKey(characterID) {
		return
	}
	session.townPositionSnapshot.PositionX = row.Value1
	session.townPositionSnapshot.PositionY = row.Value2
	session.townPositionSnapshot.MovementCode = row.Value3
	session.townPositionSnapshot.PositionValid = true
}

// bindCurrentTownSelectorOrigin freezes the town origin owned by the exact
// op15/op27 selector context. A duplicate op15 keeps the first binding. This
// prevents a later town-position report from silently changing where op132
// returns, while the same frozen snapshot remains available after op16 has
// committed an active dungeon runtime.
func bindCurrentTownSelectorOrigin(session *gameSession) (currentTownPositionSnapshot, bool) {
	if session == nil || session.selectedCharacterID == 0 {
		return currentTownPositionSnapshot{}, false
	}
	characterID := session.selectedCharacterID
	session.townMu.Lock()
	defer session.townMu.Unlock()
	if session.townSelectorOriginBound {
		if session.townSelectorOriginSnapshot.CharacterID == characterID {
			return session.townSelectorOriginSnapshot, true
		}
		clearCurrentTownSelectorOriginLocked(session)
	}
	snapshot := session.townPositionSnapshot
	if session.townSceneReadyCharacterID != characterID || snapshot.CharacterID != characterID || !snapshot.PositionValid {
		return currentTownPositionSnapshot{}, false
	}
	session.townSelectorOriginSnapshot = snapshot
	session.townSelectorOriginBound = true
	return snapshot, true
}

// freezeCurrentDungeonTownReturnOrigin copies the exact op35 origin into an
// ordinary dungeon runtime before that runtime becomes visible. Tutorial
// dungeons deliberately retain their independent persisted birth-town return
// owner because a new character can enter them before any town op35 exists.
func freezeCurrentDungeonTownReturnOrigin(session *gameSession, runtime *runtimeDungeonState) error {
	if session == nil || runtime == nil || runtime.Session == nil {
		return fmt.Errorf("%w: runtime owner missing", errCurrentDungeonTownReturnOriginUnavailable)
	}
	if isPVFTutorialDungeon(runtime) {
		runtime.townReturnOrigin = currentTownPositionSnapshot{}
		return nil
	}
	characterID, townID, areaID, err := currentDungeonTownReturnOriginOwner(session, runtime)
	if err != nil {
		return err
	}
	session.townMu.Lock()
	origin := session.townSelectorOriginSnapshot
	bound := session.townSelectorOriginBound
	session.townMu.Unlock()
	if !bound {
		// The current client's direct-entry buttons (training room 5000 and the
		// job-change trial experience 7142) can be re-entered immediately after
		// a town return: the op15 back-to-village finalizer consumed the
		// selector context (clearing the bound origin) before the next op16
		// SELECT_DUNGEON arrives. The post-return town position snapshot is
		// still valid and is exactly where the character stands, so freeze it
		// as the return origin instead of blocking the direct entry (which the
		// client experiences as a silent hang followed by a crash).
		if isCurrentDirectDungeonEntryRequest(runtime.Request) &&
			origin.CharacterID == characterID && origin.PositionValid {
			bound = true
		}
	}
	if !bound {
		return fmt.Errorf("%w: selector origin not bound", errCurrentDungeonTownReturnOriginUnavailable)
	}
	if err := validateCurrentDungeonTownReturnOrigin(origin, characterID, townID, areaID); err != nil {
		return err
	}
	runtime.townReturnOrigin = origin
	return nil
}

// currentDungeonTownReturnOriginOwner resolves the scene that owns the frozen
// selector origin. Ordinary dungeons use the durable character location. An
// Anton raid dungeon is entered from the session-scoped waiting area instead;
// that temporary 19/2 scene is deliberately never persisted over the
// character's ordinary town. Both freeze and commit must use this same owner,
// otherwise commit reinterprets 19/2 as a mismatch against (for example) 38/1
// after the request has already passed its raid-entry checks.
func currentDungeonTownReturnOriginOwner(
	session *gameSession,
	runtime *runtimeDungeonState,
) (uint16, byte, byte, error) {
	if session == nil || runtime == nil || runtime.Session == nil {
		return 0, 0, 0, fmt.Errorf("%w: runtime owner missing", errCurrentDungeonTownReturnOriginUnavailable)
	}
	characterID := session.selectedCharacterID
	if characterID == 0 || !dungeonRuntimeOwnsCharacter(runtime, characterID) {
		return 0, 0, 0, fmt.Errorf("%w: runtime character owner mismatch", errCurrentDungeonTownReturnOriginUnavailable)
	}
	if spec, ok := currentRaidTownSpecForSession(session); ok && isAntonRaidEntryDungeonID(runtime.Dungeon.ID) {
		// 超时空的返回点不是固定等待室，而是这个队伍此刻所在的作战区域。
		// 它必须与 freeze/commit 两侧校验用的 selector origin 是同一个值，否则
		// 进图会被 ordinary_dungeon_town_return_origin_unavailable 静默拒绝。
		//
		// 实测 2026-10-01: 队伍走到 area 9 后每秒重试一次开战、每次 entered=false，
		// 客户端只看到"队伍已解散"反复刷屏 —— 就是因为这里返回了等待室 98/2，而
		// 会话绑定的 selector origin 是 98/9，两边对不上。
		if isCurrentFiendWarRaidChannel(session) {
			if area, ok := currentFiendWarOperationArea(session, characterID, spec, runtime); ok {
				return characterID, byte(spec.townID), area, nil
			}
		}
		return characterID, byte(spec.townID), spec.waitingArea, nil
	}
	townID, areaID, err := currentSceneTransitionLocation(runtime.Character, true)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("%w: %v", errCurrentDungeonTownReturnOriginUnavailable, err)
	}
	return characterID, townID, areaID, nil
}

// currentFiendWarOperationArea 给出超时空这次副本尝试的返回区域。
//
// 取值顺序：
//  1. 副本运行时已记下的 origin —— 这个队伍已经从这里回过城一次；
//  2. 会话已绑定的 selector origin —— 刚走进这个区域、还没回过城；
//  3. 都没有(还在等待室) -> false，调用方退回等待室。
//
// 第 2 条是关键：遭遇是从城镇移动里发起的，那一刻 session.townPositionSnapshot
// 还没有提交到新区域(切换包还没发)，但 selector origin 已经在进入该区域时就绑好了，
// 而 freeze/commit 的校验读的正是它。
func currentFiendWarOperationArea(
	session *gameSession,
	characterID uint16,
	spec runtimeRaidTownSpec,
	runtime *runtimeDungeonState,
) (byte, bool) {
	if runtime != nil {
		origin := runtime.townReturnOrigin
		if origin.CharacterID == characterID && origin.TownID == byte(spec.townID) && origin.AreaID >= 2 {
			return origin.AreaID, true
		}
	}
	if session == nil {
		return 0, false
	}
	session.townMu.Lock()
	selector := session.townSelectorOriginSnapshot
	bound := session.townSelectorOriginBound
	session.townMu.Unlock()
	if bound && selector.CharacterID == characterID &&
		selector.TownID == byte(spec.townID) && selector.AreaID >= 2 {
		return selector.AreaID, true
	}
	return 0, false
}

func validateCurrentDungeonRuntimeTownReturnOrigin(session *gameSession, runtime *runtimeDungeonState) error {
	if isPVFTutorialDungeon(runtime) {
		return nil
	}
	characterID, townID, areaID, err := currentDungeonTownReturnOriginOwner(session, runtime)
	if err != nil {
		return err
	}
	return validateCurrentDungeonTownReturnOrigin(runtime.townReturnOrigin, characterID, townID, areaID)
}

func validateCurrentDungeonTownReturnOrigin(
	origin currentTownPositionSnapshot,
	characterID uint16,
	townID byte,
	areaID byte,
) error {
	if !origin.PositionValid {
		return fmt.Errorf("%w: op35 position was not reported", errCurrentDungeonTownReturnOriginUnavailable)
	}
	if origin.CharacterID != characterID || origin.TownID != townID || origin.AreaID != areaID {
		return fmt.Errorf(
			"%w: origin character/town/area %d/%d/%d does not match runtime %d/%d/%d",
			errCurrentDungeonTownReturnOriginUnavailable,
			origin.CharacterID,
			origin.TownID,
			origin.AreaID,
			characterID,
			townID,
			areaID,
		)
	}
	return nil
}

func clearCurrentTownSelectorOrigin(session *gameSession) {
	if session == nil {
		return
	}
	session.townMu.Lock()
	clearCurrentTownSelectorOriginLocked(session)
	session.townMu.Unlock()
}

func clearCurrentTownSelectorOriginLocked(session *gameSession) {
	if session == nil {
		return
	}
	session.townSelectorOriginSnapshot = currentTownPositionSnapshot{}
	session.townSelectorOriginBound = false
}

func currentTownPositionForTransition(
	session *gameSession,
	characterID uint16,
	townID byte,
	areaID byte,
) (currentTownPositionSnapshot, string, bool, error) {
	if session == nil || characterID == 0 {
		return currentTownPositionSnapshot{}, "", false, nil
	}
	session.townMu.Lock()
	selectorSnapshot := session.townSelectorOriginSnapshot
	selectorBound := session.townSelectorOriginBound
	session.townMu.Unlock()
	if selectorBound {
		if selectorSnapshot.CharacterID != characterID ||
			selectorSnapshot.TownID != townID ||
			selectorSnapshot.AreaID != areaID {
			return currentTownPositionSnapshot{}, "", false, fmt.Errorf(
				"selector origin character/town/area %d/%d/%d does not match transition %d/%d/%d",
				selectorSnapshot.CharacterID,
				selectorSnapshot.TownID,
				selectorSnapshot.AreaID,
				characterID,
				townID,
				areaID,
			)
		}
		if !selectorSnapshot.PositionValid {
			return currentTownPositionSnapshot{}, "", false, fmt.Errorf(
				"%w: bound selector origin has no op35 position",
				errCurrentDungeonTownReturnOriginUnavailable,
			)
		}
		return selectorSnapshot, "current_exe_op35_selector_origin_snapshot", true, nil
	}
	return currentTownPositionSnapshot{}, "", false, nil
}

func (s *Service) applyCurrentTownPositionToTransition(
	session *gameSession,
	characterID uint16,
	transition currentDungeonTownTransition,
) (currentDungeonTownTransition, bool, error) {
	snapshot, positionSource, ok, err := currentTownPositionForTransition(
		session,
		characterID,
		transition.TownID,
		transition.AreaID,
	)
	if err != nil {
		return currentDungeonTownTransition{}, false, err
	}
	if !ok {
		return transition, false, nil
	}
	transition, err = applyCurrentTownPositionSnapshotToTransition(
		transition,
		snapshot,
		characterID,
		positionSource,
	)
	if err != nil {
		return currentDungeonTownTransition{}, false, err
	}
	return transition, true, nil
}

func applyCurrentTownPositionSnapshotToTransition(
	transition currentDungeonTownTransition,
	snapshot currentTownPositionSnapshot,
	characterID uint16,
	positionSource string,
) (currentDungeonTownTransition, error) {
	if err := validateCurrentDungeonTownReturnOrigin(
		snapshot,
		characterID,
		transition.TownID,
		transition.AreaID,
	); err != nil {
		return currentDungeonTownTransition{}, err
	}
	row := currentSceneTransitionRow{
		ObjectOrResourceKey: transition.ActorObjectKey,
		Value1:              snapshot.PositionX,
		Value2:              snapshot.PositionY,
		Value3:              transition.Direction,
		Value4:              transition.AreaState,
	}
	body, err := buildCurrentSceneTransitionBody(
		transition.TownID,
		transition.AreaID,
		[]currentSceneTransitionRow{row},
	)
	if err != nil {
		return currentDungeonTownTransition{}, err
	}
	transition.PositionX = int16(snapshot.PositionX)
	transition.PositionY = int16(snapshot.PositionY)
	transition.PositionSource = positionSource
	transition.Body = body
	return transition, nil
}

func (s *Service) logTownSetUserPositionBlocked(
	session *gameSession,
	request currentTownSetUserPositionRequest,
	reason string,
) {
	s.logGameEvent(session, "game-town-set-user-position-blocked",
		"char_id", session.selectedCharacterID,
		"position_x", request.PositionX,
		"position_y", request.PositionY,
		"movement_code", request.MovementCode,
		"opaque_scaled_u16", request.OpaqueScaledU16,
		"reason", reason)
}
