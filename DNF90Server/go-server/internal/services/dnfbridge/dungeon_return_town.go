package dnfbridge

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	"longheng.io/server/internal/modules/dnf/worldmap"
)

type currentDungeonTownTransition struct {
	TownID         byte
	AreaID         byte
	ActorObjectKey uint16
	PositionX      int16
	PositionY      int16
	Direction      byte
	AreaState      byte
	PositionSource string
	Body           []byte
}

// sendCurrentDungeonTownTransitionPrefix restores the controlled town actor,
// active area and completed-quest gate state before AREA_USERS/op24 installs
// the destination scene. The dungeon-selector return uses this same prefix;
// an empty quest record is still projected as a valid zero state so it can
// never suppress the dungeon selector's map rows.
func (s *Service) sendCurrentDungeonTownTransitionPrefix(
	ctx context.Context,
	session *gameSession,
	characterID uint16,
	transition currentDungeonTownTransition,
	source string,
) error {
	row := currentSceneTransitionRow{
		ObjectOrResourceKey: transition.ActorObjectKey,
		Value1:              uint16(transition.PositionX),
		Value2:              uint16(transition.PositionY),
		Value3:              transition.Direction,
		Value4:              transition.AreaState,
	}
	if err := s.sendCurrentTownLocationNotifications(
		session,
		characterID,
		transition.TownID,
		transition.AreaID,
		row,
		source,
	); err != nil {
		return err
	}
	if err := s.sendCurrentDungeonClearQuestSeedLocked(
		ctx,
		session,
		characterID,
		source,
	); err != nil {
		return err
	}
	s.logGameEvent(session, "game-dungeon-town-transition-prefix-sent",
		"char_id", characterID,
		"town_id", transition.TownID,
		"area_id", transition.AreaID,
		"source", source,
		"sequence", "op22_user_position_then_op23_user_area_then_op356_completed_quest_state_before_op24")
	return nil
}

// sendCurrentDungeonClearQuestSeedLocked publishes the completed-quest bitmap
// (op356) alone at a caller-chosen boundary. The reference completed dungeon
// return order restores this bitmap immediately after the equipment-bearing
// mode1 and BEFORE the op21/op574 task snapshots: op574 only upserts active
// rows, so the bitmap must arrive first to drop a just-finished dungeon child
// from the HUD and to seed the map [quest condition] gates before op24
// rebuilds the destination scene.
func (s *Service) sendCurrentDungeonClearQuestSeedLocked(
	ctx context.Context,
	session *gameSession,
	characterID uint16,
	source string,
) error {
	body, err := s.buildCurrentClearQuestListTransportBodyForSession(
		ctx,
		session,
		source+"_after_mode1_before_task_snapshots_seed_completed_quest_scene_state",
	)
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
	return nil
}

func (s *Service) handleDungeonBackToVillage(session *gameSession, body []byte) error {
	if session == nil {
		return nil
	}
	if len(body) != 0 {
		s.logGameEvent(session, "game-dungeon-back-to-village-blocked",
			"body_len", len(body),
			"reason", "current_exe_op132_body_must_be_empty")
		return nil
	}
	if session.selectedCharacterID == 0 {
		s.logGameEvent(session, "game-dungeon-back-to-village-blocked",
			"body_len", len(body),
			"reason", "selected_character_missing")
		return nil
	}
	// 入场材料不足被拒之后，客户端在它自己的失败迁移里紧接着就会发一发 bodyless op132
	// （实测 6~70ms）。那不是玩家点的"返回城镇"，而是它"离开副本"的反射；照旧处理就会把
	// 玩家直接送回城（现场 2026-09-24 12:44:46 / 13:08:23 用户看到的"直接回城"）。
	// 这里吞掉恰好一发：补一遍"选择副本"界面让玩家留在界面上，之后再点返回城镇照旧回城。
	if s.suppressCurrentDungeonShortageEscape(session, "current_exe_bodyless_op132_back_2_village") {
		return nil
	}
	selectedCharacterID := session.selectedCharacterID

	if _, err := s.persistCurrentActiveTutorialExit(
		session,
		selectedCharacterID,
		"confirmed_mid_tutorial_bodyless_op132_back_to_village",
	); err != nil {
		s.logGameEvent(session, "game-dungeon-back-to-village-blocked",
			"char_id", selectedCharacterID,
			"reason", "confirmed_tutorial_abandonment_persistence_failed",
			"error", err)
		return nil
	}

	// The current client uses the same bodyless op132 from two distinct owners:
	// an active dungeon run and the town-side dungeon-selection page.  The
	// latter has no dungeon runtime by design, so require the request-driven
	// op15/op27 selector context before allowing that branch.
	session.dungeon.mu.Lock()
	hasDungeonRuntime := session.dungeon.runtime != nil && session.dungeon.runtime.Session != nil
	session.dungeon.mu.Unlock()
	selectorReady := false
	selectorReason := "active_dungeon_runtime_changed_before_return"
	// 客户端在没有服务端选择器上下文时也会发 bodyless op132：最典型的是"再次挑战被拒"
	// （疲劳不足等）之后客户端自己弹回选副本页，再点"返回城镇"。旧行为在这里静默 block、
	// 一个包都不回，客户端就一直等这一发 → 只能大退（现场 2026-09-18 深渊时空裂缝：疲劳
	// 归零后重进被正确拒绝，随后"返回城镇"完全没反应）。
	// 客户端已经明确要求回城，这里就放行同一条"选择器返回"路线，并留下专门日志便于回溯。
	clientRequestedReturnWithoutContext := false
	if !hasDungeonRuntime {
		if session.backToVillageSelectorDuplicateGuard {
			session.backToVillageSelectorDuplicateGuard = false
			s.logGameEvent(session, "game-dungeon-back-to-village-duplicate-suppressed",
				"char_id", selectedCharacterID,
				"reason", "post-return-op15-selector-reopen-already-accepted")
			return nil
		}
		selectorReady, selectorReason = currentDungeonSelectTownReturnReady(session)
		if !selectorReady {
			clientRequestedReturnWithoutContext = true
			selectorReady = true
			s.logGameEvent(session, "game-dungeon-back-to-village-context-armed",
				"char_id", selectedCharacterID,
				"body_len", len(body),
				"missing_reason", selectorReason,
				"reason", "client_requested_return_without_confirmed_selector_context")
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	transition, err := s.prepareCurrentDungeonTownTransitionForSession(ctx, session, selectedCharacterID)
	if err != nil {
		s.logGameEvent(session, "game-dungeon-back-to-village-blocked",
			"char_id", session.selectedCharacterID,
			"body_len", len(body),
			"reason", "persisted_town_transition_unavailable",
			"error", err)
		return nil
	}

	session.dungeon.mu.Lock()
	runtime := session.dungeon.runtime
	if runtime == nil || runtime.Session == nil {
		if session.selectedCharacterID != selectedCharacterID {
			selectorReady = false
			selectorReason = "selected_character_changed_before_dungeon_select_return"
		} else if !currentDungeonSelectContextConfirmed(session) && !clientRequestedReturnWithoutContext {
			selectorReady = false
			selectorReason = "confirmed_dungeon_select_context_cleared_before_return"
		}
		if !selectorReady {
			s.logGameEvent(session, "game-dungeon-back-to-village-blocked",
				"char_id", selectedCharacterID,
				"town_id", transition.TownID,
				"area_id", transition.AreaID,
				"reason", selectorReason)
			session.dungeon.mu.Unlock()
			return nil
		}
		s.logGameEvent(session, "game-dungeon-select-back-to-village-op24-send",
			"char_id", session.selectedCharacterID,
			"request_msg_id", uint16(dnfenum.CmdPacketBack2Village),
			"response_msg_id", currentSceneTransitionMsgID,
			"classification", 0,
			"town_id", transition.TownID,
			"area_id", transition.AreaID,
			"row_count", 1,
			"actor_object_key", transition.ActorObjectKey,
			"position_x", transition.PositionX,
			"position_y", transition.PositionY,
			"direction", transition.Direction,
			"area_state", transition.AreaState,
			"position_source", transition.PositionSource,
			"body_len", len(transition.Body),
			"source", "current_exe_bodyless_op132_from_confirmed_dungeon_select_context",
			"body_source", "current_exe_sub_1D901D0_typed")
		if err := s.sendCurrentDungeonTownTransitionPrefix(
			ctx,
			session,
			selectedCharacterID,
			transition,
			"current_exe_op132_selector_return",
		); err != nil {
			session.dungeon.mu.Unlock()
			return err
		}
		if err := s.sendGameUpperRawClass(session, currentSceneTransitionMsgID, transition.Body, 0); err != nil {
			session.dungeon.mu.Unlock()
			return err
		}
		clearCurrentDungeonSelectContext(session)
		// Closing the town-side selector does not destroy or recreate the town
		// actor.  Treating it as a completed dungeon return armed the op35
		// recovery chain, which replayed mode0/mode1 and unsolicited op37/op30;
		// op37 opens the native HonorExpert/熟练Lv explanation panel.  Preserve
		// the already-finalized scene generation and only guard the one client
		// op15 scene-finalizer that follows this op24.
		session.returnTownFinishLoadingAckOnly = false
		session.confirmedDungeonReturnStatePending = false
		session.confirmedDungeonReturnActionTableSent = false
		session.confirmedDungeonReturnFinalGateResetSent = false
		session.confirmedDungeonReturnRouteArmed = false
		session.confirmedDungeonReturnTransition = currentDungeonTownTransition{}
		markBackToVillageEnterSelectPending(session)
		s.logGameEvent(session, "game-dungeon-select-back-to-village-committed",
			"char_id", session.selectedCharacterID,
			"town_id", transition.TownID,
			"area_id", transition.AreaID,
			"selector_context_cleared", true,
			"finish_loading_state_rearmed", false,
			"reason", "selector_only_typed_op24_preserves_town_actor_and_suppresses_unsolicited_op37")
		session.dungeon.mu.Unlock()
		clearCurrentTownSelectorOrigin(session)
		// Opening the page retired this character's town presence, and this
		// route deliberately preserves the finalized town scene instead of
		// running a completed-return restore, so nothing else here would put the
		// roster record back. Without this the selector would be a one-way trip
		// into permanent invisibility.
		s.restoreTownPresenceAfterDungeonSelector(session, "current_exe_op132_selector_return")
		// Re-register the character in the town area table itself. The selector-close
		// route "preserves" the town actor, so nothing put this character back into the
		// presence table for its current (channel, instance, town, area). Peers kept the
		// old actor and never received this character's movement -- measured 2026-09-19:
		// A walked out of the Seria room and B still showed A standing inside it, frozen
		// at the old spot, while the party frame and the minimap marker were correct.
		// The same table is what the party co-presence rebuild and every movement relay
		// read, so re-enter the area exactly like an ordinary area enter does.
		// Only a fallback: the restore above does the full two-way exchange when the
		// selector had retired a remembered presence, and it skips when the character is
		// already registered. Re-register here only when neither happened -- otherwise this
		// would broadcast a duplicate actor.
		if _, alreadyRegistered := s.onlinePlayers.PlayerForCharacter(session.selectedCharacterID); s.onlinePlayers != nil && session.selectedCharacterID != 0 && !alreadyRegistered {
			instanceKey := uint32(0)
			if raidKey := s.runtimeRaidKeyForMember(session.selectedCharacterID); raidKey != 0 {
				instanceKey = raidKey
			}
			session.townMu.Lock()
			snapshot := session.townPositionSnapshot
			session.townMu.Unlock()
			info := &onlinePlayerInfo{
				CharacterID: session.selectedCharacterID,
				ChannelID:   townPresenceChannelID(session),
				TownID:      transition.TownID,
				AreaID:      transition.AreaID,
				InstanceKey: instanceKey,
				PositionX:   snapshot.PositionX,
				PositionY:   snapshot.PositionY,
				Direction:   snapshot.MovementCode,
				Session:     session,
			}
			others := s.onlinePlayers.EnterArea(info)
			if len(others) > 0 {
				s.broadcastTownPlayerEnter(info, others)
			}
			s.logGameEvent(session, "game-town-presence-reregistered-after-selector-return",
				"char_id", session.selectedCharacterID,
				"channel_id", info.ChannelID,
				"town_id", info.TownID,
				"area_id", info.AreaID,
				"instance_key", info.InstanceKey,
				"peer_count", len(others))
		}
		// Replay the party/town co-presence the completed-return route (返回城镇 straight from the
		// settlement) relies on. The selector-close route only sends a typed op24 and
		// deliberately preserves the town actor, so it assumed the peers' actor records were
		// still installed; after a private-room presence retirement they were not, the party
		// came back without each other's models and the next entry was refused with 队员不在附近
		// (measured 2026-09-19). Rebuild them here instead of assuming.
		s.rebuildRuntimePartyTownCoPresenceAfterReturn(session, "current_exe_op132_selector_return_party_copresence")
		if self, ok := s.onlinePlayers.PlayerForCharacter(session.selectedCharacterID); ok {
			s.sendChannelWidePeerActors(session, &self, "channel_wide_peer_actor_after_selector_return")
		}
		// The current selector is leader-owned for an ordinary party. Followers
		// enter it passively and therefore do not have a native op15 ACK, but they
		// still own a real selector context and frozen town origin. When the leader
		// closes that shared page, publish the same typed op24 to every still-bound
		// passive follower. Requiring each follower to manufacture its own op132
		// leaves exactly the reported client trapped after the leader is already
		// back in town.
		s.synchronizeRuntimePartyMembersSelectorReturn(
			session,
			"leader_op132_after_selector_return",
		)
		return nil
	}
	if runtime.townReturnPending {
		s.logGameEvent(session, "game-dungeon-back-to-village-retry",
			"char_id", session.selectedCharacterID,
			"request_msg_id", uint16(dnfenum.CmdPacketBack2Village),
			"reason", "explicit_current_exe_op132_resumes_full_town_route")
	}
	status := runtime.Session.Snapshot().Run.Status
	session.dungeon.mu.Unlock()
	if status == worldmap.DungeonRunCompleted {
		return s.sendCurrentCompletedDungeonReturnToTown(
			session,
			runtime,
			transition,
			uint16(dnfenum.CmdPacketBack2Village),
			"current_exe_bodyless_op132_completed_runtime",
			// op132 is its own handler and holds no world lock.
			false,
		)
	}
	return s.sendCurrentActiveDungeonReturnToTown(
		session,
		runtime,
		transition,
		uint16(dnfenum.CmdPacketBack2Village),
		"current_exe_bodyless_op132",
	)
}

// persistCurrentActiveTutorialExit records the player's explicit decision to
// leave an unfinished tutorial before any town transition packet is built.
// Both the in-dungeon give-up confirmation (op42) and the system-menu
// back-to-village confirmation (op132) use this boundary. Holding dungeon.mu
// keeps an immediate character switch from discarding the runtime before the
// durable character-stat update finishes.
func (s *Service) persistCurrentActiveTutorialExit(
	session *gameSession,
	characterID uint16,
	source string,
) (bool, error) {
	if session == nil || characterID == 0 {
		return false, nil
	}
	session.dungeon.mu.Lock()
	defer session.dungeon.mu.Unlock()

	runtime := session.dungeon.runtime
	if runtime == nil || runtime.Session == nil ||
		session.selectedCharacterID != characterID ||
		!dungeonRuntimeOwnsCharacter(runtime, characterID) ||
		runtime.Session.Snapshot().Run.Status != worldmap.DungeonRunActive ||
		!isPVFTutorialDungeon(runtime) {
		return false, nil
	}
	if runtime.tutorialCompletionPersisted {
		return true, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	previousFlag, err := s.persistCurrentDungeonTutorialCompletion(ctx, characterID)
	cancel()
	if err != nil {
		return false, err
	}
	runtime.tutorialCompletionPersisted = true
	applyCurrentDungeonTutorialCompletionStats(&runtime.Character)
	s.logGameEvent(session, "game-dungeon-tutorial-completion-persisted",
		"char_id", characterID,
		"dungeon_id", runtime.Dungeon.ID,
		"previous_tutorial_completed", previousFlag,
		"tutorial_completed", currentDungeonTutorialCompleteFlag,
		"town_id", newCharacterInitialTownID,
		"area_id", newCharacterInitialAreaID,
		"source", source)
	return true, nil
}

func currentDungeonSelectTownReturnReady(session *gameSession) (bool, string) {
	if session == nil || session.selectedCharacterID == 0 {
		return false, "selected_character_missing"
	}
	if !currentDungeonSelectContextConfirmed(session) {
		return false, "confirmed_dungeon_select_context_missing"
	}
	session.townMu.Lock()
	readyCharacterID := session.townSceneReadyCharacterID
	originBound := session.townSelectorOriginBound
	origin := session.townSelectorOriginSnapshot
	session.townMu.Unlock()
	if readyCharacterID == 0 || readyCharacterID != session.selectedCharacterID {
		return false, "dungeon_select_context_character_not_town_ready"
	}
	if !originBound || origin.CharacterID != session.selectedCharacterID {
		return false, "dungeon_select_context_town_origin_not_bound"
	}
	if !origin.PositionValid {
		return false, "dungeon_select_context_town_origin_position_missing"
	}
	return true, "current_town_dungeon_select_context_confirmed"
}

func currentDungeonSelectContextConfirmed(session *gameSession) bool {
	return session != nil &&
		session.enterSelectDungeonSent &&
		session.enterSelectDungeonContextSent &&
		(session.enterSelectDungeonAckSent || session.enterSelectDungeonPassiveParty)
}

func clearCurrentDungeonSelectContext(session *gameSession) {
	if session == nil {
		return
	}
	session.enterSelectDungeonSent = false
	session.enterSelectDungeonAckSent = false
	session.enterSelectDungeonPassiveParty = false
	session.enterSelectDungeonContextSent = false
	session.backToVillageEnterSelectPending = false
	session.backToVillageSelectorDuplicateGuard = false
}

// synchronizeRuntimePartyMembersSelectorReturn closes the leader-owned
// dungeon-selection page for every passive party member. Each follower is
// processed on its own session event owner and retains its independently
// frozen town position. A failed or stale follower remains untouched so its
// own bodyless op132 stays a safe recovery path.
func (s *Service) synchronizeRuntimePartyMembersSelectorReturn(
	initiator *gameSession,
	source string,
) {
	if s == nil || initiator == nil || initiator.selectedCharacterID == 0 {
		return
	}
	state := runtimePartyStateSnapshot(initiator)
	members := runtimePartyMembers(state)
	if state.PartyID <= 0 || state.UserID != initiator.selectedCharacterID || len(members) <= 1 {
		return
	}
	// 并发扇出：每个 follower 都在【它自己的】event loop 上执行，彼此独立
	// （本函数显式跳过发起者自己，且原本就是「逐个失败只记日志、不中止整轮」）。
	// 改前是在循环里给每人现开一个 createWriteTimeout 再同步等，把发起者的
	// event loop 按成员数串行累加地卡住。除「改为同时执行」外，每个成员的
	// 预算、日志字段与错误处理全部保持原样。
	var fanout sync.WaitGroup
	for _, member := range members {
		if member.UserID == 0 || member.UserID == initiator.selectedCharacterID {
			continue
		}
		identity, ok := s.onlineGameSessionCharacterSnapshot(member.UserID)
		if !ok || identity.session == initiator {
			continue
		}
		fanout.Add(1)
		go func() {
			defer fanout.Done()
			fanoutCtx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
			defer cancel()
			err := s.callGameSession(fanoutCtx, identity.session, "runtime-party-selector-return", func() error {
				if !s.boundGameSessionCharacterCurrent(identity) {
					return nil
				}
				followerState := runtimePartyStateSnapshot(identity.session)
				if followerState.PartyID != state.PartyID || followerState.UserID != state.UserID {
					return nil
				}
				return s.returnRuntimePartyPassiveSelectorFollower(
					identity.session,
					state.UserID,
					state.PartyID,
					source,
				)
			})
			if err != nil {
				s.logGameEvent(identity.session, "game-party-dungeon-select-return-follower-failed",
					"source", source,
					"leader_char_id", state.UserID,
					"char_id", member.UserID,
					"party_id", state.PartyID,
					"error", err,
					"recovery", "follower_bodyless_op132_remains_available")
			}
		}()
	}
	fanout.Wait()
}

func (s *Service) returnRuntimePartyPassiveSelectorFollower(
	follower *gameSession,
	leaderCharacterID uint16,
	partyID int,
	source string,
) error {
	if follower == nil || follower.selectedCharacterID == 0 {
		return nil
	}
	selectedCharacterID := follower.selectedCharacterID
	follower.dungeon.mu.Lock()
	hasDungeonRuntime := follower.dungeon.runtime != nil && follower.dungeon.runtime.Session != nil
	follower.dungeon.mu.Unlock()
	if hasDungeonRuntime || !follower.enterSelectDungeonPassiveParty || follower.enterSelectDungeonAckSent {
		return nil
	}
	ready, reason := currentDungeonSelectTownReturnReady(follower)
	if !ready {
		s.logGameEvent(follower, "game-party-dungeon-select-return-follower-skipped",
			"source", source,
			"leader_char_id", leaderCharacterID,
			"char_id", selectedCharacterID,
			"party_id", partyID,
			"reason", reason)
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	transition, err := s.prepareCurrentDungeonTownTransitionForSession(ctx, follower, selectedCharacterID)
	if err != nil {
		return err
	}
	followerState := runtimePartyStateSnapshot(follower)
	if followerState.PartyID != partyID || followerState.UserID != leaderCharacterID {
		return nil
	}

	follower.dungeon.mu.Lock()
	if follower.selectedCharacterID != selectedCharacterID ||
		(follower.dungeon.runtime != nil && follower.dungeon.runtime.Session != nil) ||
		!currentDungeonSelectContextConfirmed(follower) ||
		!follower.enterSelectDungeonPassiveParty || follower.enterSelectDungeonAckSent {
		follower.dungeon.mu.Unlock()
		return nil
	}

	s.logGameEvent(follower, "game-party-dungeon-select-return-follower-op24-send",
		"source", source,
		"leader_char_id", leaderCharacterID,
		"char_id", selectedCharacterID,
		"party_id", partyID,
		"response_msg_id", currentSceneTransitionMsgID,
		"classification", 0,
		"town_id", transition.TownID,
		"area_id", transition.AreaID,
		"actor_object_key", transition.ActorObjectKey,
		"position_x", transition.PositionX,
		"position_y", transition.PositionY,
		"body_len", len(transition.Body),
		"body_source", "current_exe_sub_1D901D0_typed")
	if err := s.sendCurrentDungeonTownTransitionPrefix(
		ctx,
		follower,
		selectedCharacterID,
		transition,
		source+"_passive_follower",
	); err != nil {
		follower.dungeon.mu.Unlock()
		return err
	}
	if err := s.sendGameUpperRawClass(follower, currentSceneTransitionMsgID, transition.Body, 0); err != nil {
		follower.dungeon.mu.Unlock()
		return err
	}
	clearCurrentDungeonSelectContext(follower)
	follower.returnTownFinishLoadingAckOnly = false
	follower.confirmedDungeonReturnStatePending = false
	follower.confirmedDungeonReturnActionTableSent = false
	follower.confirmedDungeonReturnFinalGateResetSent = false
	follower.confirmedDungeonReturnRouteArmed = false
	follower.confirmedDungeonReturnTransition = currentDungeonTownTransition{}
	markBackToVillageEnterSelectPending(follower)
	follower.dungeon.mu.Unlock()
	clearCurrentTownSelectorOrigin(follower)
	// The follower returns because the leader closed the shared selector, and this
	// path used to stop at the typed op24: nothing registered the follower in the
	// town area table and nothing told the peers it was back. The party then stood
	// together invisible -- the leader's client never received the follower's actor
	// record -- until each member happened to change maps once, which is exactly the
	// field rule "both have to change maps". Register and announce the follower here
	// with the same two-way exchange an area enter performs, and do not depend on the
	// remembered retired-presence record: a passive follower never opened a selector
	// of its own, so it usually has none and the restore above returns without acting.
	s.restoreTownPresenceAfterDungeonSelector(follower, source+"_passive_follower")
	if s.onlinePlayers != nil && selectedCharacterID != 0 {
		if _, registered := s.onlinePlayers.PlayerForCharacter(selectedCharacterID); !registered {
			instanceKey := uint32(0)
			if raidKey := s.runtimeRaidKeyForMember(selectedCharacterID); raidKey != 0 {
				instanceKey = raidKey
			}
			follower.townMu.Lock()
			snapshot := follower.townPositionSnapshot
			follower.townMu.Unlock()
			info := &onlinePlayerInfo{
				CharacterID: selectedCharacterID,
				ChannelID:   townPresenceChannelID(follower),
				TownID:      transition.TownID,
				AreaID:      transition.AreaID,
				InstanceKey: instanceKey,
				PositionX:   snapshot.PositionX,
				PositionY:   snapshot.PositionY,
				Direction:   snapshot.MovementCode,
				Session:     follower,
			}
			followerPeers := s.onlinePlayers.EnterArea(info)
			if len(followerPeers) > 0 {
				s.broadcastTownPlayerEnter(info, followerPeers)
			}
			s.logGameEvent(follower, "game-party-select-return-follower-presence-restored",
				"char_id", selectedCharacterID,
				"town_id", info.TownID,
				"area_id", info.AreaID,
				"instance_key", info.InstanceKey,
				"peer_count", len(followerPeers))
		}
	}
	s.logGameEvent(follower, "game-party-dungeon-select-return-follower-committed",
		"source", source,
		"leader_char_id", leaderCharacterID,
		"char_id", selectedCharacterID,
		"party_id", partyID,
		"town_id", transition.TownID,
		"area_id", transition.AreaID,
		"selector_context_cleared", true,
		"finish_loading_state_rearmed", false,
		"reason", "leader_owned_selector_closed_for_passive_party_member")
	return nil
}

// markBackToVillageEnterSelectPending records the first op15 emitted by the
// current client after the typed op24 sent for system-menu op132. The current
// client also uses that packet as the player's selector request, so the
// enter-select route commits the return and opens the selector in one pass.
func markBackToVillageEnterSelectPending(session *gameSession) {
	if session == nil {
		return
	}
	session.backToVillageEnterSelectPending = true
}

func (s *Service) prepareCurrentDungeonTownTransition(
	ctx context.Context,
	characterID uint16,
	sessions ...*gameSession,
) (currentDungeonTownTransition, error) {
	character, err := s.dungeonCharacter(ctx, characterID, sessions...)
	if err != nil {
		return currentDungeonTownTransition{}, err
	}
	townID, areaID, err := currentSceneTransitionLocation(character, true)
	if err != nil {
		return currentDungeonTownTransition{}, err
	}
	row, positionX, positionY, direction, areaState, err := currentDungeonTownTransitionRow(characterID, character.Stats)
	if err != nil {
		return currentDungeonTownTransition{}, err
	}
	body, err := buildCurrentSceneTransitionBody(townID, areaID, []currentSceneTransitionRow{row})
	if err != nil {
		return currentDungeonTownTransition{}, err
	}
	return currentDungeonTownTransition{
		TownID:         townID,
		AreaID:         areaID,
		ActorObjectKey: row.ObjectOrResourceKey,
		PositionX:      positionX,
		PositionY:      positionY,
		Direction:      direction,
		AreaState:      areaState,
		PositionSource: "character_repository_stats",
		Body:           body,
	}, nil
}

func (s *Service) prepareCurrentDungeonTownTransitionForSession(
	ctx context.Context,
	session *gameSession,
	characterID uint16,
) (currentDungeonTownTransition, error) {
	transition, err := s.prepareCurrentDungeonTownTransition(ctx, characterID, session)
	if err != nil {
		return currentDungeonTownTransition{}, err
	}
	session.dungeon.mu.Lock()
	runtime := session.dungeon.runtime
	session.dungeon.mu.Unlock()
	if runtime != nil && runtime.Session != nil {
		if !dungeonRuntimeOwnsCharacter(runtime, characterID) {
			return currentDungeonTownTransition{}, fmt.Errorf(
				"%w: active runtime character owner mismatch",
				errCurrentDungeonTownReturnOriginUnavailable,
			)
		}
		if isPVFTutorialDungeon(runtime) {
			return transition, nil
		}
		positionSource := "current_exe_op35_runtime_origin_snapshot"
		if isCurrentImplementedRaidChannel(session) && isAntonRaidEntryDungeonID(runtime.Dungeon.ID) {
			// Anton's waiting area is a session-scoped RAID scene and is never
			// persisted over the character's ordinary town.  The repository-backed
			// transition therefore still starts at (for example) 38/1 even though
			// this active run froze its real 19/2 origin.  Resolve the same strict
			// owner used by freeze/commit before validating and encoding op24.
			ownerCharacterID, townID, areaID, ownerErr := currentDungeonTownReturnOriginOwner(session, runtime)
			if ownerErr != nil {
				return currentDungeonTownTransition{}, ownerErr
			}
			if ownerCharacterID != characterID {
				return currentDungeonTownTransition{}, fmt.Errorf(
					"%w: Anton runtime origin owner %d does not match character %d",
					errCurrentDungeonTownReturnOriginUnavailable,
					ownerCharacterID,
					characterID,
				)
			}
			transition.TownID = townID
			transition.AreaID = areaID
			positionSource = "current_exe_op35_session_scoped_luke_runtime_origin_snapshot"
			if isCurrentAntonRaidChannel(session) {
				positionSource = "current_exe_op35_session_scoped_anton_runtime_origin_snapshot"
			}
		}
		// 超时空 BOSS 房（量子时空传送站）通关 = 整场攻坚的收尾，落点必须是**待机区**。
		//
		// 通用路线给的是"回到你刚才那个作战区域"的原点快照，而 BOSS 区域在快照里
		// 没有坐标 —— 真机 2026-10-01 20:35 实测发出去的就是
		//
		//	town_id=98 area_id=30 position_x=0 position_y=0
		//	position_source=current_exe_op35_session_scoped_luke_runtime_origin_snapshot
		//
		// 客户端拿不到可用落点，人直接卡在副本里（礼白666）；同一次通关里另一个人
		// 连按十几次"返回城镇"也没有落点可去。待机区坐标只有一个来源（PVF 等待室的
		// NPC），也就是撤退/掉线/通关自动回城都在用的 fiendWarWaitingAreaScene ——
		// 这里复用它，所有退出路径（手动返回城镇、阶段收尾强制回城、死亡回城）一次对齐。
		if isCurrentFiendWarRaidChannel(session) {
			waited, applies, waitErr := s.fiendWarCompletedBossReturnTransition(
				characterID,
				runtime.Dungeon.ID,
				transition,
			)
			if waitErr != nil {
				s.logGameEvent(session, "game-fiendwar-boss-return-waiting-room-failed",
					"char_id", characterID,
					"dungeon_id", runtime.Dungeon.ID,
					"error", waitErr)
			} else if applies {
				s.logGameEvent(session, "game-fiendwar-boss-return-waiting-room",
					"char_id", characterID,
					"dungeon_id", runtime.Dungeon.ID,
					"town_id", waited.TownID,
					"area_id", waited.AreaID,
					"position_source", waited.PositionSource,
					"raid_state_required", false)
				return waited, nil
			}
		}
		return applyCurrentTownPositionSnapshotToTransition(
			transition,
			runtime.townReturnOrigin,
			characterID,
			positionSource,
		)
	}
	if spec, ok := currentRaidTownSpecForSession(session); ok {
		// The Anton channel town is deliberately session-scoped and is never
		// written into the character's durable ordinary-town fields. The exact
		// selector origin is therefore authoritative for both its location and
		// position; comparing it with the repository location would reject every
		// legitimate selector-page return after moving inside town 19.
		session.townMu.Lock()
		origin := session.townSelectorOriginSnapshot
		bound := session.townSelectorOriginBound
		session.townMu.Unlock()
		if !bound || origin.CharacterID != characterID ||
			origin.TownID != byte(spec.townID) || !origin.PositionValid {
			return currentDungeonTownTransition{}, fmt.Errorf(
				"%w: Anton selector origin is not bound to character %d in town %d",
				errCurrentDungeonTownReturnOriginUnavailable,
				characterID,
				spec.townID,
			)
		}
		transition.TownID = origin.TownID
		transition.AreaID = origin.AreaID
		return applyCurrentTownPositionSnapshotToTransition(
			transition,
			origin,
			characterID,
			"current_exe_op35_session_scoped_raid_selector_origin_snapshot",
		)
	}
	// A completed settlement outlives its runtime: 选择其他地下城 retires the run
	// before the client's 返回城镇 arrives, so this branch has no runtime left to
	// read a frozen origin from. Prefer the origin stashed at that retirement --
	// falling through to the persisted position returned the party to the login
	// town (the Seria room), where the private scene hides the members from each
	// other and the client refuses the next entry with 队员不在附近.
	//
	// That stash is frozen at the retirement and deliberately never refreshed, so
	// it only describes the character while the character has not moved since.
	// Every committed town move (op36 portal, op470 personal teleport, op237
	// teleport potion, cross-town portal) rewrites the live town scene and the
	// durable location together, and the two then disagree with the stash: live
	// 2026-09-23 13:49:40Z the player teleported to 圣者之鸣号 (17/2), had two
	// entries refused for a missing ticket, pressed 返回城镇 and was sent to the
	// retired run's frozen 20/2 (989,436) -- a different town, at the coordinates
	// it stood on before teleporting. The live town scene is the only in-session
	// proof of where the character actually stands now, so the stash is kept only
	// while that scene still owns the same town/area the stash describes.
	session.townMu.Lock()
	completedOrigin := session.completedRunTownReturnOrigin
	sceneSnapshot := session.townPositionSnapshot
	sceneReady := session.townSceneReadyCharacterID == characterID
	session.townMu.Unlock()
	stashMatchesLiveTownScene := true
	if sceneReady && sceneSnapshot.CharacterID == characterID {
		stashMatchesLiveTownScene = sceneSnapshot.TownID == completedOrigin.TownID &&
			sceneSnapshot.AreaID == completedOrigin.AreaID
	}
	if completedOrigin.CharacterID == characterID && completedOrigin.PositionValid && stashMatchesLiveTownScene {
		transition.TownID = completedOrigin.TownID
		transition.AreaID = completedOrigin.AreaID
		return applyCurrentTownPositionSnapshotToTransition(
			transition,
			completedOrigin,
			characterID,
			"completed_run_frozen_town_return_origin",
		)
	}
	if completedOrigin.CharacterID == characterID && completedOrigin.PositionValid {
		// The character moved after the settlement retired its run. The durable
		// location this transition was built from is the one every town move
		// persists, so it is authoritative here; the stale stash must not drag the
		// return back to the previous town.
		transition.PositionSource = "completed_run_origin_dropped_after_town_move"
		s.logGameEvent(session, "game-dungeon-select-back-to-village-origin-dropped",
			"char_id", characterID,
			"dungeon_origin_town_id", completedOrigin.TownID,
			"dungeon_origin_area_id", completedOrigin.AreaID,
			"dungeon_origin_x", completedOrigin.PositionX,
			"dungeon_origin_y", completedOrigin.PositionY,
			"current_town_id", transition.TownID,
			"current_area_id", transition.AreaID,
			"current_x", transition.PositionX,
			"current_y", transition.PositionY,
			"reason", "completed_run_origin_is_frozen_at_the_retirement_and_the_character_has_moved_since")
		return transition, nil
	}
	transition, _, err = s.applyCurrentTownPositionToTransition(session, characterID, transition)
	if err != nil {
		return currentDungeonTownTransition{}, err
	}
	if transition.PositionSource == "character_repository_stats" {
		return currentDungeonTownTransition{}, fmt.Errorf(
			"%w: selector return has no matching frozen op35 origin",
			errCurrentDungeonTownReturnOriginUnavailable,
		)
	}
	return transition, nil
}

func currentDungeonTownTransitionRow(
	characterID uint16,
	characterStats map[string]int64,
) (currentSceneTransitionRow, int16, int16, byte, byte, error) {
	if characterID == 0 {
		return currentSceneTransitionRow{}, 0, 0, 0, 0, fmt.Errorf("selected character id is zero")
	}
	if characterStats == nil {
		return currentSceneTransitionRow{}, 0, 0, 0, 0, fmt.Errorf("selected character stats not loaded")
	}
	positionX, hasPositionX := characterStats["pos_x"]
	if !hasPositionX {
		return currentSceneTransitionRow{}, 0, 0, 0, 0, fmt.Errorf("selected character pos_x not loaded")
	}
	positionY, hasPositionY := characterStats["pos_y"]
	if !hasPositionY {
		return currentSceneTransitionRow{}, 0, 0, 0, 0, fmt.Errorf("selected character pos_y not loaded")
	}
	direction, hasDirection := characterStats["direction"]
	if !hasDirection {
		return currentSceneTransitionRow{}, 0, 0, 0, 0, fmt.Errorf("selected character direction not loaded")
	}
	areaState, hasAreaState := characterStats["area_state"]
	if !hasAreaState {
		return currentSceneTransitionRow{}, 0, 0, 0, 0, fmt.Errorf("selected character area_state not loaded")
	}
	if positionX < -1<<15 || positionX > 1<<15-1 {
		return currentSceneTransitionRow{}, 0, 0, 0, 0, fmt.Errorf("selected character pos_x %d is outside i16", positionX)
	}
	if positionY < -1<<15 || positionY > 1<<15-1 {
		return currentSceneTransitionRow{}, 0, 0, 0, 0, fmt.Errorf("selected character pos_y %d is outside i16", positionY)
	}
	if direction < 0 || direction > int64(^byte(0)) {
		return currentSceneTransitionRow{}, 0, 0, 0, 0, fmt.Errorf("selected character direction %d is outside u8", direction)
	}
	if areaState < 0 || areaState > int64(^byte(0)) {
		return currentSceneTransitionRow{}, 0, 0, 0, 0, fmt.Errorf("selected character area_state %d is outside u8", areaState)
	}
	positionX16 := int16(positionX)
	positionY16 := int16(positionY)
	return currentSceneTransitionRow{
		ObjectOrResourceKey: currentSceneActorObjectKey(characterID),
		Value1:              uint16(positionX16),
		Value2:              uint16(positionY16),
		Value3:              byte(direction),
		Value4:              byte(areaState),
	}, positionX16, positionY16, byte(direction), byte(areaState), nil
}

// sendCurrentDungeonReturnToTownLocked starts the typed current-EXE town
// transition. A successful socket write is not a scene acknowledgement, so the
// dungeon runtime remains authoritative until a later town-side client request
// proves that the page transition completed. The caller must hold
// session.dungeon.mu so final completion, death reports, and op132 cannot race.
func (s *Service) sendCurrentDungeonReturnToTownLocked(
	session *gameSession,
	runtime *runtimeDungeonState,
	transition currentDungeonTownTransition,
	requestMsgID uint16,
	source string,
) error {
	if session == nil || runtime == nil || runtime.Session == nil || session.dungeon.runtime != runtime {
		return nil
	}
	snapshot := runtime.Session.Snapshot()
	if snapshot.Run.Status != worldmap.DungeonRunActive && snapshot.Run.Status != worldmap.DungeonRunCompleted {
		s.logGameEvent(session, "game-dungeon-back-to-village-blocked",
			"char_id", session.selectedCharacterID,
			"dungeon_id", snapshot.Run.DungeonID,
			"maze_index", snapshot.Run.MazeIndex,
			"run_status", snapshot.Run.Status,
			"reason", "dungeon_run_not_active")
		return nil
	}
	// The client-side op24 write is only a proposed scene transition. Keep
	// run-owned mechanisms alive until town evidence atomically detaches the
	// generation; otherwise a rejected/cancelled transition returns to a live
	// dungeon whose Forgotten Land/altar timers were already destroyed. The
	// death watchdog is the one exception: it initiated or competes with this
	// same return and must not emit a second transition.
	s.cancelCurrentDungeonDeathReturnLocked(session, runtime, "town_return_transition_started")
	if !runtime.townReturnPending {
		runtime.townReturnPending = true
		runtime.townReturnRequestMsgID = requestMsgID
		runtime.townReturnSource = source
		runtime.townReturnTransition = cloneCurrentDungeonTownTransition(transition)
	}
	transition = runtime.townReturnTransition

	if !runtime.townReturnOp24Sent {
		s.logGameEvent(session, "game-dungeon-back-to-village-op24-send",
			"char_id", session.selectedCharacterID,
			"request_msg_id", requestMsgID,
			"response_msg_id", currentSceneTransitionMsgID,
			"classification", 0,
			"dungeon_id", snapshot.Run.DungeonID,
			"maze_index", snapshot.Run.MazeIndex,
			"room", snapshot.Run.Current.String(),
			"town_id", transition.TownID,
			"area_id", transition.AreaID,
			"row_count", 1,
			"actor_object_key", transition.ActorObjectKey,
			"position_x", transition.PositionX,
			"position_y", transition.PositionY,
			"direction", transition.Direction,
			"area_state", transition.AreaState,
			"position_source", transition.PositionSource,
			"body_len", len(transition.Body),
			"source", source,
			"body_source", "current_exe_sub_1D901D0_typed")
		if err := s.sendGameUpperRawClass(session, currentSceneTransitionMsgID, transition.Body, 0); err != nil {
			return err
		}
		runtime.townReturnOp24Sent = true
		session.confirmedDungeonReturnStatePending = true
		session.confirmedDungeonReturnActionTableSent = false
		session.confirmedDungeonReturnFinalGateResetSent = false
		session.confirmedDungeonReturnRouteArmed = false
		session.confirmedDungeonReturnTransition = cloneCurrentDungeonTownTransition(transition)
		markBackToVillageEnterSelectPending(session)
	}
	// Do not rebuild or reset the selected actor yet.  This first op24 only asks
	// the client to leave the dungeon page.  A town-side request confirms that
	// transition; only then may the bridge replay the same actor route used by a
	// normal completed return.  Sending an isolated reset here is too early and
	// is overwritten by the later town actor generation.
	s.logGameEvent(session, "game-dungeon-back-to-village-pending",
		"char_id", session.selectedCharacterID,
		"dungeon_id", snapshot.Run.DungeonID,
		"maze_index", snapshot.Run.MazeIndex,
		"room", snapshot.Run.Current.String(),
		"run_status", snapshot.Run.Status,
		"town_id", transition.TownID,
		"area_id", transition.AreaID,
		"position_source", transition.PositionSource,
		"op24_sent", runtime.townReturnOp24Sent,
		"actor_state_sent", false,
		"runtime_retained", session.dungeon.runtime == runtime,
		"reason", "socket_write_is_not_client_scene_commit_and_mode0_is_not_safe_without_town_confirmation")
	return nil
}

// sendCurrentCompletedDungeonReturnToTown reconstructs the selected town actor
// for a normally settled run. Interrupted runs use the exact same route through
// sendCurrentActiveDungeonReturnToTown after atomically becoming abandoned.
func (s *Service) sendCurrentCompletedDungeonReturnToTown(
	session *gameSession,
	runtime *runtimeDungeonState,
	transition currentDungeonTownTransition,
	requestMsgID uint16,
	source string,
	// worldLocked is true only when the caller already owns
	// runtime.partyDungeonOwner.worldMu, which is the op72 leader/peer path. It
	// is forwarded to the roster detach so that path never re-acquires a lock it
	// already holds.
	worldLocked bool,
) error {
	return s.sendCurrentFullDungeonReturnToTown(
		session,
		runtime,
		transition,
		requestMsgID,
		source,
		false,
		worldLocked,
	)
}

// sendCurrentActiveDungeonReturnToTown owns an interrupted-run exit from the
// first authoritative abandon through the complete town actor route. This is
// deliberately not split at the first typed op24: retaining the old dungeon
// owner until an incidental town request leaves the client between generations
// and is the state leak that can suppress every later consumable request.
func (s *Service) sendCurrentActiveDungeonReturnToTown(
	session *gameSession,
	runtime *runtimeDungeonState,
	transition currentDungeonTownTransition,
	requestMsgID uint16,
	source string,
) error {
	return s.sendCurrentFullDungeonReturnToTown(
		session,
		runtime,
		transition,
		requestMsgID,
		source,
		true,
		// Every interrupted-run caller (death return, raid phase close, the
		// 战况表 leader-led return) reaches this without worldMu, so the roster
		// detach has to take it itself.
		false,
	)
}

// sendCurrentActiveDungeonReturnToTownKeepingParty is the interrupted-run exit for a
// WHOLE party: a raid objective closing a gate (forceReturnRuntimeRaidGroupsMatching)
// or a squad leader leading his squad home from the 战况表 mid-run.
//
// The operator's rule (2026-09-20) is that both of those keep the squad together:
// "阶段结束正常不应该解散队伍 应该保持组队状态 全队退出副本" and "战况表回城本就是
// 设计给小队队长在副本中途带领全队回城的（回城后应当依然还是组队状态）". The
// interrupted-run route detaches a member who walks out ALONE (see
// party_forced_exit_leave.go); a whole-party exit must not, or the squad has to be
// re-formed in town -- which is exactly the live complaint "现在是只能小队成员自己退出
// 然后重新组队".
func (s *Service) sendCurrentActiveDungeonReturnToTownKeepingParty(
	session *gameSession,
	runtime *runtimeDungeonState,
	transition currentDungeonTownTransition,
	requestMsgID uint16,
	source string,
) error {
	if session != nil && runtime != nil {
		session.dungeon.mu.Lock()
		if session.dungeon.runtime == runtime {
			runtime.townReturnKeepParty = true
		}
		session.dungeon.mu.Unlock()
	}
	return s.sendCurrentActiveDungeonReturnToTown(
		session,
		runtime,
		transition,
		requestMsgID,
		source,
	)
}

// currentDungeonTownExitClass names what a town return must do to the squad.
//
// It exists because this decision used to be re-derived from three booleans --
// abandonActive, runtime.townReturnKeepParty and runtime.townReturnPartyDetached
// -- plus a soloReturn flag computed further down inside
// sendCurrentFullDungeonReturnToTown. Both live bugs on this route were that
// inference going wrong rather than a missing feature:
//
//   - the leader's 返回城镇 on a completed party settlement was classified as a
//     solo exit, so the completed route detached the member from the shared
//     roster mid-fan-out and the party dissolved as each member came home
//     (2026-09-25, "回城后队伍自动解散了 ... 有点像各自回城");
//   - the roster detach it reached re-took worldMu from a goroutine that already
//     held it, which froze the returning member's session forever and hung
//     返回城镇, item use and the next party entry until the client restarted.
//
// The operator's rule the classes encode (2026-09-25): 队长点返回城镇 所有队员
// 一起返回 并且队伍保持原样; 队员自己按 ESC 返回城镇 是自己回去并自动退队.
type currentDungeonTownExitClass int

const (
	// exitClassSquadKept: nobody leaves the party. A leader-led whole-party exit
	// (runtime.townReturnKeepParty), or a return whose detach already happened
	// (runtime.townReturnPartyDetached).
	exitClassSquadKept currentDungeonTownExitClass = iota
	// exitClassSoloLeave: this member goes home alone and leaves the party,
	// because a teammate is still inside the same run.
	exitClassSoloLeave
	// exitClassAbandonLeave: an interrupted or abandoned run leaves the party.
	exitClassAbandonLeave
)

// currentDungeonTownExitClass decides the class once, in one readable place.
//
// The order of the questions is the order the route has always asked them:
// an already-detached or whole-party return detaches nobody; an interrupted run
// always detaches; a completed run detaches only when a teammate is still inside
// the very same run, which is what completedDungeonReturnLeavesParty answers.
// peerID and peerReason are the peer that test named, and are only meaningful for
// exitClassSoloLeave.
func (s *Service) currentDungeonTownExitClass(
	session *gameSession,
	runtime *runtimeDungeonState,
	partyStateBeforeReturn alignedcmd.PartyState,
	activeReturn bool,
) (class currentDungeonTownExitClass, peerID uint16, peerReason string) {
	if runtime == nil || runtime.townReturnPartyDetached || runtime.townReturnKeepParty {
		return exitClassSquadKept, 0, ""
	}
	if activeReturn {
		return exitClassAbandonLeave, 0, ""
	}
	if inside, id, reason := s.completedDungeonReturnLeavesParty(
		session,
		runtime,
		partyStateBeforeReturn,
	); inside {
		return exitClassSoloLeave, id, reason
	}
	return exitClassSquadKept, 0, ""
}

// sendCurrentFullDungeonReturnToTown is the single current-client town rebuild
// boundary for both completed and interrupted runs. Packet bodies remain owned
// by the current 90CN builders; only the lifecycle structure follows the proven
// start/abandon/finish model from the recovered server source.
func (s *Service) sendCurrentFullDungeonReturnToTown(
	session *gameSession,
	runtime *runtimeDungeonState,
	transition currentDungeonTownTransition,
	requestMsgID uint16,
	source string,
	abandonActive bool,
	// worldLocked reports whether the caller already owns
	// runtime.partyDungeonOwner.worldMu. Only the completed-return path reached
	// from the op72 handler does, and it must say so: the roster detach below
	// used to take that lock a second time from the same goroutine and deadlock
	// the returning member's session forever.
	worldLocked bool,
) error {
	if session == nil {
		return nil
	}
	if runtime == nil || runtime.Session == nil {
		// This function used to drop the request here without a trace. Every
		// silent exit now names itself so a 返回城镇 that produced no scene
		// transition can be attributed from packet_log.txt alone; see
		// backOffCompletedSettlementReturnWhenNothingWasSent for the consumer
		// that turns the empty answer into the selector page.
		s.recordCompletedReturnOutcome(session, "not-started",
			"guard", "runtime_or_session_missing",
			"char_id", session.selectedCharacterID,
			"runtime_present", runtime != nil,
			"abandon_active", abandonActive,
			"source", source)
		return nil
	}

	session.dungeon.mu.Lock()
	if session.dungeon.runtime != runtime {
		current := session.dungeon.runtime
		session.dungeon.mu.Unlock()
		s.recordCompletedReturnOutcome(session, "not-started",
			"guard", "runtime_generation_replaced",
			"char_id", session.selectedCharacterID,
			"runtime_present", current != nil,
			"abandon_active", abandonActive,
			"source", source)
		return nil
	}
	snapshot := runtime.Session.Snapshot()
	activeReturn := false
	// A mid-tutorial give-up (op42) is an abandoned run by status, but the
	// tutorial completion is already persisted and the player expects the
	// first main quest to appear right after town re-entry. Carry that signal
	// out of the lock so the route below can seed quest snapshots even though
	// the return is classified abandoned.
	seedCompletedQuestForTutorial := false
	partyDungeonOwner := (*runtimePartyDungeonOwner)(nil)
	partyStateBeforeReturn := alignedcmd.PartyState{}
	if abandonActive {
		switch snapshot.Run.Status {
		case worldmap.DungeonRunActive:
			if err := runtime.Session.Abandon(); err != nil {
				session.dungeon.mu.Unlock()
				return err
			}
			activeReturn = true
			seedCompletedQuestForTutorial = isPVFTutorialDungeon(runtime) && runtime.tutorialCompletionPersisted
			partyDungeonOwner = runtime.partyDungeonOwner
			partyStateBeforeReturn = runtimePartyStateSnapshot(session)
			if err := s.notifyRuntimeRaidDungeonAbandonedLocked(session, runtime); err != nil {
				s.logGameEvent(session, "game-dungeon-full-return-raid-abandon-publish-failed",
					"char_id", session.selectedCharacterID,
					"dungeon_id", runtime.Dungeon.ID,
					"source", source,
					"error", err)
			}
		case worldmap.DungeonRunAbandoned:
			if !runtime.townReturnPending {
				session.dungeon.mu.Unlock()
				s.recordCompletedReturnOutcome(session, "not-started",
					"guard", "abandoned_run_without_pending_return",
					"char_id", session.selectedCharacterID,
					"run_status", string(snapshot.Run.Status),
					"dungeon_id", snapshot.Run.DungeonID,
					"abandon_active", abandonActive,
					"source", source)
				return nil
			}
			activeReturn = true
			seedCompletedQuestForTutorial = isPVFTutorialDungeon(runtime) && runtime.tutorialCompletionPersisted
			partyDungeonOwner = runtime.partyDungeonOwner
			partyStateBeforeReturn = runtimePartyStateSnapshot(session)
		default:
			session.dungeon.mu.Unlock()
			s.recordCompletedReturnOutcome(session, "not-started",
				"guard", "abandon_return_run_status_unsupported",
				"char_id", session.selectedCharacterID,
				"run_status", string(snapshot.Run.Status),
				"dungeon_id", snapshot.Run.DungeonID,
				"abandon_active", abandonActive,
				"source", source)
			return nil
		}
	} else if snapshot.Run.Status != worldmap.DungeonRunCompleted {
		session.dungeon.mu.Unlock()
		s.recordCompletedReturnOutcome(session, "not-started",
			"guard", "completed_return_run_status_not_completed",
			"char_id", session.selectedCharacterID,
			"run_status", string(snapshot.Run.Status),
			"dungeon_id", snapshot.Run.DungeonID,
			"maze_index", snapshot.Run.MazeIndex,
			"town_return_pending", runtime.townReturnPending,
			"abandon_active", abandonActive,
			"source", source)
		return nil
	} else {
		// A completed run keeps its party only while the rest of the team is
		// still on its way home. Capture the same two values the interrupted
		// branch captures so the solo-return rule can tell "everybody came
		// back" from "this member walked out alone"; see
		// completedDungeonReturnLeavesParty for the operator rule and the live
		// report behind it.
		partyDungeonOwner = runtime.partyDungeonOwner
		partyStateBeforeReturn = runtimePartyStateSnapshot(session)
	}
	// 这一次回城是不是"队内成员单独自己回城"？服主裁定 (2026-10-01)：
	// 「在战况表里撤退 应该保持编制 如果是队内成员单独自己回城 应该取消这个人的编制」。
	// 战况表带队返回与阶段结束全队返回走 exitClassSquadKept，根本不进下面那个分支；
	// 能进那个分支的超时空回城有三种并不是"自己回城"的：失败收尾把全员送回待机区
	// (townReturnKeepsSquadLabel)、区域清完的自动回城 (同上)、以及死亡回收
	// (deathReturnWaiting —— prepareRuntimeFiendWarRetreat 会把他禁锢在等待室，
	// 战场模型里他仍属于原小队)。只有玩家自己在副本里按返回城镇才算"自己回城"。
	// 判定写在 raid_squad_sync.go 的 currentDungeonTownReturnClearsRaidSquad。
	fiendWarSquadLabelKept := isCurrentFiendWarRaidChannel(session) &&
		(runtime.townReturnKeepsSquadLabel || runtime.deathReturnWaiting)
	firstAttempt := !runtime.townReturnPending
	if firstAttempt {
		// 超时空进行中的撤退：落点要从作战区域改回等待室，并且把这个人从战场区域里
		// 放出来(见 prepareRuntimeFiendWarRetreat —— 不清的话他会留在原地既挡着
		// 队友走路、又立刻触发下一场遭遇)。必须在下面冻结 townReturnTransition 之前
		// 做完，否则冻的是旧落点。非超时空频道不进这里。
		if isCurrentFiendWarRaidChannel(session) && activeReturn {
			retreat, retreatErr := s.prepareRuntimeFiendWarRetreat(session, transition, runtime.deathReturnWaiting)
			if retreatErr != nil {
				session.dungeon.mu.Unlock()
				return retreatErr
			}
			transition = retreat
		}
		if isCurrentFiendWarRaidChannel(session) {
			// 离开副本等于放弃这次"走进区域即开战"的去重记录，下次抵达要能重新开。
			session.fiendWarEncounterRaidKey, session.fiendWarEncounterArea = 0, 0
			session.fiendWarEncounterAt = time.Time{}
		}
		s.cancelCurrentDungeonDeathReturnLocked(session, runtime, "full_town_actor_route_started")
		s.cancelCurrentForgottenLandTimerLocked(session, runtime, "full_town_actor_route_started")
		s.cancelCurrentDungeonAltarTimersLocked(runtime)
		runtime.townReturnPending = true
		runtime.townReturnRequestMsgID = requestMsgID
		runtime.townReturnSource = source
		runtime.townReturnTransition = cloneCurrentDungeonTownTransition(transition)
	}
	transition = cloneCurrentDungeonTownTransition(runtime.townReturnTransition)
	session.dungeon.mu.Unlock()

	// What this return owes the squad, decided once in one named place. See
	// currentDungeonTownExitClass for the operator rule it encodes and for the
	// two live bugs the old "three booleans plus a flag computed here" inference
	// caused.
	exitClass, soloPeerID, soloPeerReason := s.currentDungeonTownExitClass(
		session,
		runtime,
		partyStateBeforeReturn,
		activeReturn,
	)
	if exitClass != exitClassSquadKept {
		detachSource := source
		soloReturn := exitClass == exitClassSoloLeave
		if soloReturn {
			detachSource = source + "_solo_completed_town_return"
		}
		if partyDungeonOwner != nil {
			// The op72 leader and each peer fan-out delivery already hold
			// worldMu (handleDungeonEplpCommand / its settlement-exit closure),
			// so reuse that hold instead of deadlocking on a second acquisition
			// from the same goroutine.
			detach := s.detachRuntimePartyDungeonMemberForTownReturn
			if worldLocked {
				detach = s.detachRuntimePartyDungeonMemberForTownReturnWhileWorldLocked
			}
			if err := detach(
				session,
				partyDungeonOwner,
				partyStateBeforeReturn,
				detachSource,
			); err != nil {
				return err
			}
		} else if soloReturn {
			// A run that owns no shared roster (a raid gate) still has to drop
			// the returning member and retire its actor for the teammates that
			// are inside the same gate.
			if err := s.leaveRuntimePartyOnSoloDungeonReturn(
				session,
				runtime,
				partyStateBeforeReturn,
				soloPeerID,
				soloPeerReason,
				detachSource,
			); err != nil {
				return err
			}
		} else if err := s.leaveRuntimePartyOnAbandonedDungeonExit(
			// A run entered alone while the town party stayed behind has no
			// shared dungeon owner, so the detach above never runs for it. The
			// operator rule is the same either way: abandoning a run leaves a
			// party that still has other members. Published before the town
			// player state so the client rebuilds its town scene already
			// knowing it is alone, rather than being handed a party window it
			// is about to lose.
			session,
			partyStateBeforeReturn,
			source+"_abandoned_run_town_return",
		); err != nil {
			return err
		}
		session.dungeon.mu.Lock()
		if session.dungeon.runtime == runtime {
			runtime.townReturnPartyDetached = true
		}
		session.dungeon.mu.Unlock()
		// The raid squad label is derived from that party, so it follows the same
		// leave -- for the leader too, which is why this is scoped to "this member
		// leaves the party" and not to "a follower leaves". Operator report
		// 2026-09-26: "在副本中直接返回城镇…却还在小队编制里", "队长自己单独返回也要清
		// 相当于队长自己退队了 按现有逻辑 队长顺延".
		//
		// Deliberately inside this branch and nowhere else: exitClassSquadKept is
		// the whole-party exit (runtime.townReturnKeepParty -- the 战况表
		// leader-led return and a raid objective closing its gates, plus the
		// leader's 返回城镇 on a completed settlement) and a return that has
		// already detached, and the operator's rule is that those must NOT touch
		// the squad ("全队一起回城绝对不要清").
		//
		// Called after the party projections and outside the dungeon lock: the
		// clear takes raidMu and broadcasts three raid packets, and every other
		// caller in this file publishes membership before it too.
		//
		// The guard is the operator's split between 战况表撤退 (keep) and
		// 单独自己回城 (clear); see currentDungeonTownReturnClearsRaidSquad for the
		// rule and for the Fiend War returns the server itself drives.
		if s.currentDungeonTownReturnClearsRaidSquad(session, exitClass, fiendWarSquadLabelKept) {
			s.unassignRuntimeRaidSquadOnSoloTownReturn(session.selectedCharacterID, detachSource)
		}
	}

	if firstAttempt {
		resetDungeonEntrySceneGates(session)
		session.townMu.Lock()
		session.townActorOwnerChannel = currentConnectionTownActorOwnerContext(session)
		session.townMu.Unlock()
		clearCurrentDungeonSelectContext(session)
		clearCurrentTownSelectorOrigin(session)
		s.armCurrentInitialTownRoute(session, session.selectedCharacterID)
		session.sceneBootstrapTailDeferred = true
		// The weakness penalty attaches to the forced exit only. The run status
		// is already committed above (a cleared run never reaches this branch),
		// and it never fails the return: the town transition is already done.
		// 修炼场, 转职体验「领悟时间」 (7142) and PVP are whitelisted rooms: leaving
		// one, however abruptly, must never weaken - see currentWeaknessExemptRoom
		// for why the rooms have to be named by dungeon id rather than by the
		// op16 practice mode.
		if activeReturn {
			if rule, exempt := currentWeaknessExemptRoom(session, runtime); exempt {
				s.logGameEvent(session, "game-dungeon-forced-return-weakness-room-exempt",
					"char_id", session.selectedCharacterID,
					"source", source+"_abandoned_run_town_return",
					"dungeon_id", runtime.Dungeon.ID,
					"rule", rule)
			} else if rule, exempt := s.currentForcedReturnWeaknessRaidExempt(session, runtime); exempt {
				// A raid exit never weakens. Leaving one costs the squad its clear
				// and eats the phase clock, which is the raid's own punishment; the
				// ordinary 虚弱 penalty is not part of it. Live report 2026-09-16:
				// stepping out of gate 210 mid-run applied weakness level 9 with
				// exemption=none.
				//
				// 2026-10-02 00:00:36 又踩了一次同一个坑，但形状不同：超时空的**失败
				// 收尾**会先把 runtime.raidDungeon 清成零值（returnRuntimeFiendWarDungeons
				// 的 ended 分支），于是只看绑定的那条判断失效、给全队挂上了虚弱 9。
				// 依据规则由 currentForcedReturnWeaknessRaidExempt 给出。
				s.logGameEvent(session, "game-dungeon-forced-return-weakness-raid-exempt",
					"char_id", session.selectedCharacterID,
					"source", source+"_abandoned_run_town_return",
					"dungeon_id", runtime.Dungeon.ID,
					"raid_key", runtime.raidDungeon.RaidKey,
					"rule", rule)
			} else {
				exemption := s.applyCurrentForcedReturnWeakness(session, source+"_abandoned_run_town_return")
				session.dungeon.mu.Lock()
				if session.dungeon.runtime == runtime {
					runtime.townReturnWeaknessExemption = exemption
				}
				session.dungeon.mu.Unlock()
			}
		}
	}

	// C2S op42/op132 sets the native dungeon actor's item-use wait byte before
	// it asks to leave.  The class0/op42 reset resolves its target through the
	// client's *current* scene object.  Sending it after the town mode0/mode1
	// rebuild therefore clears the new town actor and leaves the abandoned
	// dungeon actor armed; that stale byte is then observed by the next run's
	// op44 sender.  Retire the wait while the dungeon actor is still current,
	// before emitting any town actor packet.  A successful write is recorded so
	// a later route retry does not replay this one-shot boundary.
	if activeReturn {
		session.dungeon.mu.Lock()
		if session.dungeon.runtime != runtime {
			session.dungeon.mu.Unlock()
			s.recordCompletedReturnOutcome(session, "not-started",
				"guard", "runtime_generation_replaced_before_gate_reset",
				"char_id", session.selectedCharacterID,
				"abandon_active", abandonActive,
				"source", source)
			return nil
		}
		if !runtime.townReturnDungeonGateResetSent {
			if err := s.sendCurrentConsumableUseGateResetPacket(
				session,
				source+"_before_interrupted_town_actor_rebind",
			); err != nil {
				session.dungeon.mu.Unlock()
				return err
			}
			runtime.townReturnDungeonGateResetSent = true
		}
		session.dungeon.mu.Unlock()
	}

	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	characterID, characterName, character, hasCharacter := s.selectedCharacterForEnter(ctx, session)
	if characterID == 0 || characterID != session.selectedCharacterID || !hasCharacter {
		return fmt.Errorf(
			"completed dungeon town route character mismatch: selected=%d loaded=%d found=%t",
			session.selectedCharacterID,
			characterID,
			hasCharacter,
		)
	}
	ownerChannel := currentTownActorOwnerContext(session)
	objectBody := s.buildCurrentSceneObjectListBodyForSessionInContext(
		ctx,
		session,
		characterID,
		characterName,
		character,
		hasCharacter,
		ownerChannel,
	)
	// The dungeon actor with this stable key already exists when the result-page
	// return is requested. Reusing the ordinary generation event here makes the
	// current client replay one-shot actor initialization and opens the
	// auxiliary-profession qualification dialog. Keep the complete authoritative
	// mode0 row, but suppress only that tail event just as an existing-actor
	// appearance refresh does.
	if !suppressCurrentSceneObjectActorStateEvent(objectBody) {
		return fmt.Errorf("suppress completed-return actor-state initialization event")
	}
	adventureSummary := s.currentAccountAdventureGroupSummaryForPacket(ctx, session, character, hasCharacter)
	mode1Body := s.buildCurrentActorBindingMode1BodyForSelectedWithEquipmentInContext(
		ctx,
		session,
		character,
		hasCharacter,
		characterID,
		uint32(adventureSummary.ManageLevel),
		ownerChannel,
		true,
	)

	s.logGameEvent(session, "game-dungeon-full-town-route-send",
		"char_id", characterID,
		"request_msg_id", requestMsgID,
		"dungeon_id", runtime.Dungeon.ID,
		"town_id", transition.TownID,
		"area_id", transition.AreaID,
		"position_x", transition.PositionX,
		"position_y", transition.PositionY,
		"position_source", transition.PositionSource,
		"source", source,
		"sequence", "op376_then_event_suppressed_msg2_mode0_then_equipment_bearing_mode1_then_optional_event_suppressed_skin_mode0_then_op559_buff_registration_then_op356_completed_quest_seed_then_op21_op574_then_op22_op23_location_then_typed_op24_then_op21_op574_actor_ready_refresh",
		"body_source", "current_exe_completed_login_builders_and_persisted_town_transition")

	session.townMu.Lock()
	err := s.sendCurrentTownActorRoutePacketsLocked(
		ctx,
		session,
		characterID,
		transition.TownID,
		transition.AreaID,
		currentSceneTransitionRow{
			ObjectOrResourceKey: transition.ActorObjectKey,
			Value1:              uint16(transition.PositionX),
			Value2:              uint16(transition.PositionY),
			Value3:              transition.Direction,
			Value4:              transition.AreaState,
		},
		objectBody,
		mode1Body,
		transition.Body,
		// Both return kinds rebuild the selected actor with mode0/mode1, and the
		// current EXE takes the equipped creature's card/enchant state only from
		// the same-template list-3 op14 (plus op105/op102) that follows mode1.
		// The interrupted return used to skip that refresh, so a pet lost its
		// enchant after 强制回城 until the next full login.
		true,
		!activeReturn || seedCompletedQuestForTutorial,
	)
	routeStage := session.initialTownRouteStage
	// mode0 crossed the wire with its one-shot event suppressed. If the player
	// now uses op7, the current client clears the actor but retains an empty
	// event-source vector. Carry this fact into the selector re-entry route; an
	// ordinary cold login or a town-only character switch never sets it.
	if routeStage >= currentInitialTownRouteObjectSent {
		session.returnSelectActorStateEventSuppressionPending = true
	}
	questSnapshotsSeeded := session.initialTownQuestSnapshotsSent
	session.townMu.Unlock()
	// Record the typed op24 boundary as soon as the staged town route has
	// crossed it. If a later snapshot or native-gate reset write fails, an op35
	// produced by the already-transitioned client is valid retry evidence and
	// must resume the remaining suffix instead of being ignored.
	if routeStage >= currentInitialTownRouteTransitionSent {
		session.dungeon.mu.Lock()
		if session.dungeon.runtime == runtime {
			runtime.townReturnOp24Sent = true
		}
		session.dungeon.mu.Unlock()
	}
	if err != nil {
		// The client was ACKed before this route started, so an error here is the
		// player-visible hang. Report it on the service logger: the packet log
		// drops lines (measured ~25% for one live connection), so a
		// packet-log-only report of this failure is not evidence of anything.
		s.recordCompletedReturnOutcome(session, "town-route-failed",
			"conn_id", session.connID,
			"char_id", session.selectedCharacterID,
			"dungeon_id", runtime.Dungeon.ID,
			"route_stage", routeStage,
			"town_id", transition.TownID,
			"area_id", transition.AreaID,
			"position_source", transition.PositionSource,
			"error", err)
		return err
	}
	if routeStage < currentInitialTownRouteTransitionSent {
		s.recordCompletedReturnOutcome(session, "town-route-stopped-before-op24",
			"conn_id", session.connID,
			"char_id", session.selectedCharacterID,
			"dungeon_id", runtime.Dungeon.ID,
			"route_stage", routeStage,
			"want_stage", currentInitialTownRouteTransitionSent,
			"town_id", transition.TownID,
			"area_id", transition.AreaID)
		return nil
	}
	// A normal completed return never arms the interrupted-return wait, so keep
	// its established post-op24 reset. Interrupted returns already cleared the
	// still-current dungeon actor before the town actor rebuild above; sending
	// only here would target the wrong generation.
	if !activeReturn {
		if err := s.sendCurrentConsumableUseGateResetPacket(
			session,
			source+"_after_full_typed_op24",
		); err != nil {
			return err
		}
	}
	// Match the current 90CN actor-ready boundary: the pre-op24 pair seeds the
	// task manual, and this second repository/PVF pair refreshes visible NPC
	// task markers after the town transition has been committed by the route.
	// This is a state snapshot only; it never accepts or completes a quest.
	if questSnapshotsSeeded {
		if err := s.sendCurrentAcceptableQuestListForSession(
			session,
			source+"_post_op24_actor_ready_refresh",
		); err != nil {
			return err
		}
	}
	if err := s.sendSelectedActorUserStateRefreshIfNonDefault(
		session,
		source+"_post_op24_reapply_persisted_visibility",
	); err != nil {
		return err
	}
	// A forced exit no longer spends anything: 黑钻 exempts passively and the
	// contract is only ever charged by the player's own op9 choice, so there is
	// no premium state left to republish here.
	session.dungeon.mu.Lock()
	if session.dungeon.runtime == runtime {
		runtime.townReturnWeaknessExemption = currentWeaknessExemptionNone
	}
	session.dungeon.mu.Unlock()
	// Publish the health cap itself. subtype0 is only requested at character
	// selection, so without this the penalty (or a level that wore off while
	// the player was in the dungeon) stayed invisible until the next relog.
	// This runs after the player-state rebuild above, deliberately.
	if err := s.sendCurrentWeaknessConditionRefresh(session, source+"_forced_exit"); err != nil {
		return err
	}
	// 公会地下城净化进度，同样的道理。creditCurrentGuildDungeonClear 的注释原本
	// 假设"刷新后的表会在下一次进城路线上送达"，但 sendCurrentGuildDungeonState
	// 全项目只接在 initial_town_after_op24（选角色进城）那一处，副本返回城镇这条
	// 路从来没有重发过。于是刚打完的那次净化进度、以及因此解锁的下一个副本，都要
	// 等玩家重新选一次角色才看得到 —— 正是"攻略进度必须重进角色才刷新"。
	// 放在玩家状态重建之后，与上面的血量上限同一条理由。
	s.sendCurrentGuildDungeonState(session, source+"_town_return")
	// Dungeon entry removes the actor from OnlinePlayerManager because the
	// dungeon scene is not a town instance.  A completed Anton route used to
	// rebuild only the visible 19/2 actor and immediately detach the runtime;
	// the following op15 therefore failed the private-waiting-instance guard
	// even though the client was visibly back in the waiting map.  Restore the
	// exact server-owned RaidKey together with the frozen pre-entry position
	// before retiring the completed runtime.
	if isCurrentImplementedRaidChannel(session) && runtime.raidDungeon.RaidKey != 0 {
		restored, reason := s.restoreCompletedRaidWaitingTownPresence(
			session,
			runtime,
			transition,
			character,
		)
		if !restored && raidWaitingPresenceDeclineStrandsCompletedRun(reason) {
			// The raid this run belonged to is gone (finished, disbanded, or the
			// member left it), or this run is not returning to that raid's waiting
			// area at all. The completed owner used to stay attached here "as a
			// retry path", but the retry belongs to a raid that no longer exists:
			// the attached run then made the town-move gate refuse every later area
			// request (town_move.go:83, active_dungeon_runtime_rejects_town_area_request),
			// so the player could not leave the waiting room at all. Live
			// 2026-09-20 09:58:41Z, raid 1507329: both members kept sending op36 for
			// 19/1 and every one was refused, with the raid already gone
			// (game-dungeon-completed-raid-waiting-presence-deferred
			// reason=raid_membership_missing just before).
			//
			// Fall back to the channel town's ordinary presence -- no raid instance
			// -- and let the completed run retire, exactly as a completed ordinary
			// dungeon return does. Nothing is lost: the run is finished, and a later
			// re-entry through the portal registers the new raid's instance again.
			restored, reason = s.restoreCompletedOrdinaryTownPresence(
				session,
				runtime,
				transition,
				character,
			)
			if restored {
				s.logGameEvent(session, "game-dungeon-completed-raid-waiting-presence-fell-back",
					"char_id", characterID,
					"raid_key", runtime.raidDungeon.RaidKey,
					"town_id", transition.TownID,
					"area_id", transition.AreaID,
					"reason", reason,
					"raid_waiting_area_declined", "the_raid_no_longer_owns_this_run")
			}
		}
		if !restored {
			s.logGameEvent(session, "game-dungeon-completed-raid-waiting-presence-deferred",
				"char_id", characterID,
				"raid_key", runtime.raidDungeon.RaidKey,
				"town_id", transition.TownID,
				"area_id", transition.AreaID,
				"reason", reason,
				"runtime_retained", true)
			// Keep the completed owner attached.  Its narrow completed-stage op15
			// admission remains a safe retry path, while detaching here would leave
			// the player with neither a waiting presence nor a runtime owner.
			return nil
		}
	} else if restored, reason := s.restoreCompletedOrdinaryTownPresence(
		session,
		runtime,
		transition,
		character,
	); !restored {
		s.logGameEvent(session, "game-dungeon-completed-town-presence-deferred",
			"char_id", characterID,
			"town_id", transition.TownID,
			"area_id", transition.AreaID,
			"reason", reason)
	}

	session.dungeon.mu.Lock()
	if session.dungeon.runtime == runtime {
		runtime.townReturnOp24Sent = true
		retireReason := "completed_town_actor_route_committed"
		if activeReturn {
			retireReason = "interrupted_town_actor_route_committed"
		}
		s.retireCurrentDungeonRuntimeLocked(session, runtime, retireReason)
	}
	detached := session.dungeon.runtime != runtime
	session.dungeon.mu.Unlock()
	if detached {
		if err := s.switchCurrentPetGrowthClock(session, currentPetGrowthClockTown, s.gameplayNow(), source+"_after_full_town_route"); err != nil {
			s.logGameEvent(session, "game-pet-growth-clock-start-deferred",
				"char_id", characterID,
				"mode", currentPetGrowthClockTown.String(),
				"source", source,
				"error", err)
		}
		// Result-page op72 does not produce the historical op132/op15 town
		// finalizer on this EXE.  Rebuild party actors and the realtime roster
		// as soon as the last immutable member has restored its ordinary-town
		// presence instead of waiting forever for a callback that never arrives.
		if !activeReturn {
			if err := s.rebuildRuntimePartyTownCoPresenceAfterReturn(
				session,
				source+"_after_completed_town_presence_restore",
			); err != nil {
				return err
			}
		}
	}

	s.logGameEvent(session, "game-dungeon-full-town-route-committed",
		"char_id", characterID,
		"request_msg_id", requestMsgID,
		"dungeon_id", runtime.Dungeon.ID,
		"town_id", transition.TownID,
		"area_id", transition.AreaID,
		"route_stage", routeStage,
		"interrupted_run", activeReturn,
		"runtime_detached", detached,
		"scene_tail_deferred", session.sceneBootstrapTailDeferred,
		"next_owner", "real_client_scene_callback_then_deferred_town_player_state")
	return nil
}

// restoreCompletedOrdinaryTownPresence re-enters the process-owned public
// town index after dungeon entry removed the actor.  The typed op24 route has
// already reconstructed the local actor; this function only restores the
// authoritative server presence and a valid position gate used by party
// co-presence rebuilding and later selector requests.
func (s *Service) restoreCompletedOrdinaryTownPresence(
	session *gameSession,
	runtime *runtimeDungeonState,
	transition currentDungeonTownTransition,
	character dnfrepo.CharacterRecord,
) (bool, string) {
	if s == nil || session == nil || runtime == nil || runtime.Session == nil ||
		session.selectedCharacterID == 0 {
		return false, "runtime_owner_missing"
	}
	if s.onlinePlayers == nil {
		return false, "online_player_manager_unavailable"
	}
	position := currentTownPositionSnapshot{
		CharacterID:   session.selectedCharacterID,
		TownID:        transition.TownID,
		AreaID:        transition.AreaID,
		PositionX:     uint16(transition.PositionX),
		PositionY:     uint16(transition.PositionY),
		MovementCode:  transition.Direction,
		PositionValid: true,
	}
	if origin := runtime.townReturnOrigin; origin.CharacterID == session.selectedCharacterID &&
		origin.TownID == transition.TownID && origin.AreaID == transition.AreaID &&
		origin.PositionValid {
		position = origin
	}
	name := character.Name
	if name == "" {
		name = csharpSelectCharacterName(session.selectedCharacterID)
	}
	player := &onlinePlayerInfo{
		CharacterID: session.selectedCharacterID,
		AccountID:   character.AccountID,
		Name:        name,
		Job:         byte(numericCharacterStat(character.Job)),
		GrowType:    byte(numericCharacterStatValue(character, "grow_type")),
		Level:       byte(character.Level),
		ChannelID:   townPresenceChannelID(session),
		TownID:      transition.TownID,
		AreaID:      transition.AreaID,
		PositionX:   position.PositionX,
		PositionY:   position.PositionY,
		Direction:   position.MovementCode,
		AreaState:   transition.AreaState,
		Session:     session,
	}
	others := s.onlinePlayers.EnterArea(player)
	s.promoteResidentGameSession(session, session.selectedCharacterID)
	// The completed return owns this registration; drop any dungeon-selector
	// retirement memo so it cannot be replayed afterwards.
	clearTownSelectorRetiredPresence(session)

	// Party peers are rebuilt once, symmetrically, after all members have
	// returned.  Public-town observers are independent and can receive this
	// actor immediately without exposing a half-returned party to either peer.
	publicOthers := others[:0]
	for _, other := range others {
		if runtimePartyDungeonOwnerContains(runtime.partyDungeonOwner, other.CharacterID) {
			continue
		}
		publicOthers = append(publicOthers, other)
	}
	s.broadcastTownPlayerEnter(player, publicOthers)
	s.notifyCurrentUnitedFriendPresence(
		player.Name,
		currentFriendPresence{ChannelID: uint16(session.channel.ID)},
		"completed_dungeon_return_friend_online",
	)

	session.townMu.Lock()
	clearCurrentTownSelectorOriginLocked(session)
	session.townSceneReadyCharacterID = session.selectedCharacterID
	session.townPositionSnapshot = position
	session.townMu.Unlock()

	s.logGameEvent(session, "game-dungeon-completed-town-presence-restored",
		"char_id", session.selectedCharacterID,
		"town_id", player.TownID,
		"area_id", player.AreaID,
		"position_x", player.PositionX,
		"position_y", player.PositionY,
		"public_peer_count", len(publicOthers),
		"party_deferred", runtime.partyDungeonOwner != nil,
		"position_source", transition.PositionSource)
	return true, "ordinary_town_presence_restored"
}

func runtimePartyDungeonOwnerContains(owner *runtimePartyDungeonOwner, characterID uint16) bool {
	if owner == nil || characterID == 0 {
		return false
	}
	for _, member := range owner.roster() {
		if member.userID == characterID {
			return true
		}
	}
	return false
}

// The reasons restoreCompletedRaidWaitingTownPresence declines a raid waiting-area
// return with. They are named because the caller has to tell "the raid no longer
// owns this run" (which must fall back to the ordinary presence, or the completed
// run stays attached and strands the player in the waiting room) from a transient
// registry/owner problem (which is retried).
const (
	raidWaitingPresenceDeclineNotWaitingArea = "return_target_is_not_raid_waiting_area"
	raidWaitingPresenceDeclineRaidMissing    = "raid_membership_missing"
	raidWaitingPresenceDeclineRaidKeyChanged = "runtime_raid_key_mismatch"
	raidWaitingPresenceDeclineMemberRemoved  = "raid_member_not_in_frozen_roster"
)

// raidWaitingPresenceDeclineStrandsCompletedRun reports whether one decline means
// this completed run can never be handed back to its raid: the raid is gone, no
// longer holds the member, or the run is not returning to that raid's waiting area.
// Keeping it attached in those cases is what left players unable to leave the
// waiting room after the raid ended.
func raidWaitingPresenceDeclineStrandsCompletedRun(reason string) bool {
	switch reason {
	case raidWaitingPresenceDeclineRaidMissing,
		raidWaitingPresenceDeclineRaidKeyChanged,
		raidWaitingPresenceDeclineMemberRemoved,
		raidWaitingPresenceDeclineNotWaitingArea:
		return true
	default:
		return false
	}
}

// restoreCompletedRaidWaitingTownPresence re-enters the private raid waiting
// instance for a run that is coming back from a completed raid gate.
func (s *Service) restoreCompletedRaidWaitingTownPresence(
	session *gameSession,
	runtime *runtimeDungeonState,
	transition currentDungeonTownTransition,
	character dnfrepo.CharacterRecord,
) (bool, string) {
	if s == nil || session == nil || runtime == nil || runtime.Session == nil ||
		session.selectedCharacterID == 0 {
		return false, "runtime_owner_missing"
	}
	if s.onlinePlayers == nil {
		return false, "online_player_manager_unavailable"
	}
	spec, raidChannel := currentRaidTownSpecForSession(session)
	// 超时空：等待室不是它唯一的合法落点 —— 队伍大部分时间待在 2..31 的作战区域里，
	// 所以 >=2 的区域同样要认。安东/卢克只有等待室，判定逐字不变。
	if !raidChannel || transition.TownID != byte(spec.townID) ||
		(transition.AreaID != spec.waitingArea && !(isCurrentFiendWarRaidChannel(session) && transition.AreaID >= 2)) {
		return false, raidWaitingPresenceDeclineNotWaitingArea
	}
	raidKey := runtime.raidDungeon.RaidKey
	state, member := s.runtimeRaidForMember(session.selectedCharacterID)
	if !member {
		return false, raidWaitingPresenceDeclineRaidMissing
	}
	if state.RaidKey == 0 || state.RaidKey != raidKey {
		return false, raidWaitingPresenceDeclineRaidKeyChanged
	}
	if _, frozen := runtimeRaidFrozenMember(state, session.selectedCharacterID); !frozen {
		return false, raidWaitingPresenceDeclineMemberRemoved
	}

	position := currentTownPositionSnapshot{
		CharacterID:   session.selectedCharacterID,
		TownID:        transition.TownID,
		AreaID:        transition.AreaID,
		PositionX:     uint16(transition.PositionX),
		PositionY:     uint16(transition.PositionY),
		MovementCode:  transition.Direction,
		PositionValid: true,
	}
	if origin := runtime.townReturnOrigin; origin.CharacterID == session.selectedCharacterID &&
		origin.TownID == transition.TownID && origin.AreaID == transition.AreaID &&
		origin.PositionValid {
		position = origin
	}

	name := character.Name
	if name == "" {
		name = csharpSelectCharacterName(session.selectedCharacterID)
	}
	player := &onlinePlayerInfo{
		CharacterID: session.selectedCharacterID,
		AccountID:   character.AccountID,
		Name:        name,
		Job:         byte(numericCharacterStat(character.Job)),
		GrowType:    byte(numericCharacterStatValue(character, "grow_type")),
		Level:       byte(character.Level),
		ChannelID:   townPresenceChannelID(session),
		TownID:      transition.TownID,
		AreaID:      transition.AreaID,
		InstanceKey: raidKey,
		PositionX:   position.PositionX,
		PositionY:   position.PositionY,
		Direction:   transition.Direction,
		AreaState:   transition.AreaState,
		Session:     session,
	}
	others := s.onlinePlayers.EnterArea(player)
	s.promoteResidentGameSession(session, session.selectedCharacterID)
	// The completed return owns this registration; drop any dungeon-selector
	// retirement memo so it cannot be replayed afterwards.
	clearTownSelectorRetiredPresence(session)
	s.broadcastTownPlayerEnter(player, others)
	s.notifyCurrentUnitedFriendPresence(
		player.Name,
		currentFriendPresence{ChannelID: uint16(session.channel.ID)},
		"completed_raid_return_friend_online",
	)

	// The completed route already sent a typed op24.  Rebind its proven frozen
	// origin so a portal click does not have to wait for an incidental movement
	// op35 before op27 may create the Raid selector context.
	session.townMu.Lock()
	clearCurrentTownSelectorOriginLocked(session)
	session.townSceneReadyCharacterID = session.selectedCharacterID
	session.townPositionSnapshot = position
	session.townMu.Unlock()

	s.logGameEvent(session, "game-dungeon-completed-raid-waiting-presence-restored",
		"char_id", session.selectedCharacterID,
		"raid_key", raidKey,
		"town_id", player.TownID,
		"area_id", player.AreaID,
		"position_x", player.PositionX,
		"position_y", player.PositionY,
		"peer_count", len(others),
		"position_source", transition.PositionSource)
	// 通关后落回攻坚队待机室：官方口径是塔罗牌"入场时看见1次，之后在状况表上确认"，
	// 所以这一包只刷新数据、不弹窗（openWindowFlag 由 session 上的一次性账本决定，
	// 见 raid_luke_card_display.go）。这条路一次通关只走一次，不是每个房间都走。
	s.publishRuntimeRaidLukeCardDisplay(session, raidKey, "completed_dungeon_raid_waiting_presence_restored")
	return true, "raid_waiting_presence_restored"
}

func cloneCurrentDungeonTownTransition(transition currentDungeonTownTransition) currentDungeonTownTransition {
	transition.Body = append([]byte(nil), transition.Body...)
	return transition
}

func resetCurrentDungeonReturnAttempt(runtime *runtimeDungeonState) {
	if runtime == nil {
		return
	}
	runtime.townReturnPending = false
	runtime.townReturnOp24Sent = false
	runtime.townReturnRequestMsgID = 0
	runtime.townReturnSource = ""
	runtime.townReturnTransition = currentDungeonTownTransition{}
}

// cancelCurrentDungeonReturnAfterDungeonEvidenceLocked cancels an unconfirmed
// return when a valid dungeon-only request proves that the client never left
// the dungeon. An unrelated death or move request must not trigger another
// unsolicited op24. The caller must hold session.dungeon.mu.
func (s *Service) cancelCurrentDungeonReturnAfterDungeonEvidenceLocked(
	session *gameSession,
	runtime *runtimeDungeonState,
	evidence string,
) {
	if session == nil || runtime == nil || session.dungeon.runtime != runtime || !runtime.townReturnPending {
		return
	}
	requestMsgID := runtime.townReturnRequestMsgID
	source := runtime.townReturnSource
	transition := runtime.townReturnTransition
	resetCurrentDungeonReturnAttempt(runtime)
	session.confirmedDungeonReturnStatePending = false
	session.confirmedDungeonReturnActionTableSent = false
	session.confirmedDungeonReturnFinalGateResetSent = false
	session.confirmedDungeonReturnRouteArmed = false
	session.confirmedDungeonReturnTransition = currentDungeonTownTransition{}
	s.logGameEvent(session, "game-dungeon-back-to-village-cancelled",
		"char_id", session.selectedCharacterID,
		"request_msg_id", requestMsgID,
		"town_id", transition.TownID,
		"area_id", transition.AreaID,
		"source", source,
		"evidence", evidence,
		"runtime_retained", session.dungeon.runtime == runtime,
		"reason", "accepted_dungeon_request_proves_transition_uncommitted_no_unsolicited_retry")
}

// commitPendingDungeonReturnForSceneRequest is now only a retry edge for a
// full town route whose socket write stopped part-way through. The recovered
// server lifecycle does not use a town-side request as the authoritative
// abandon boundary: abandon and the complete town rebuild are one operation.
// Keeping this hook lets an already-arriving op15/op16/op35/op36 resume the
// staged packet suffix without detaching an incompletely rebuilt actor.
func (s *Service) commitPendingDungeonReturnForSceneRequest(session *gameSession, source string) error {
	if session == nil {
		return nil
	}
	session.dungeon.mu.Lock()
	runtime := session.dungeon.runtime
	if runtime == nil || runtime.Session == nil || !runtime.townReturnPending {
		session.dungeon.mu.Unlock()
		return s.ensureCurrentConfirmedDungeonReturnPlayerState(
			session,
			source+"_resume_confirmed_return",
		)
	}
	snapshot := runtime.Session.Snapshot()
	transition := cloneCurrentDungeonTownTransition(runtime.townReturnTransition)
	requestMsgID := runtime.townReturnRequestMsgID
	session.dungeon.mu.Unlock()
	s.logGameEvent(session, "game-dungeon-full-town-route-resume",
		"char_id", session.selectedCharacterID,
		"dungeon_id", snapshot.Run.DungeonID,
		"maze_index", snapshot.Run.MazeIndex,
		"room", snapshot.Run.Current.String(),
		"run_status", snapshot.Run.Status,
		"town_id", transition.TownID,
		"area_id", transition.AreaID,
		"source", source,
		"reason", "resume_incomplete_full_town_actor_route")
	if snapshot.Run.Status == worldmap.DungeonRunCompleted {
		return s.sendCurrentCompletedDungeonReturnToTown(
			session, runtime, transition, requestMsgID, source+"_completed_resume",
			// The resume runs off its own trigger, not the op72 handler.
			false,
		)
	}
	return s.sendCurrentActiveDungeonReturnToTown(
		session, runtime, transition, requestMsgID, source+"_interrupted_resume",
	)
}

// detachRuntimePartyDungeonMemberForTownReturn applies the current-client
// native leave projections without fabricating a class1/op13 command ACK. The
// town-returning member is removed from both the central party and the shared
// dungeon roster; surviving members keep their fixed slots and active runtime.
func (s *Service) detachRuntimePartyDungeonMemberForTownReturn(
	session *gameSession,
	owner *runtimePartyDungeonOwner,
	previousState alignedcmd.PartyState,
	source string,
) error {
	if s == nil || session == nil || owner == nil || previousState.PartyID <= 0 {
		return nil
	}
	// Different members own different session mailboxes and can confirm a town
	// return concurrently. Serialize the central-party mutation, the shared
	// dungeon roster and their native projections as one ordered world edge;
	// otherwise an older two-to-one reduction can arrive after the final member
	// has already disbanded the party and resurrect stale UI state.
	owner.worldMu.Lock()
	defer owner.worldMu.Unlock()
	return s.detachRuntimePartyDungeonMemberForTownReturnWhileWorldLocked(
		session, owner, previousState, source)
}

// detachRuntimePartyDungeonMemberForTownReturnWhileWorldLocked is the body of
// the function above. owner.worldMu must ALREADY be held by the caller.
//
// It exists because the op72 settlement path reaches this function from inside
// handleDungeonEplpCommand's own worldMu hold, and sync.Mutex is not reentrant:
// the plain wrapper there re-acquired a lock the goroutine already owned and
// deadlocked forever, leaving worldMu owned by a dead goroutine. Measured live
// 2026-09-25: a 返回城镇 click ACKed at 08:12:38.570Z and then sent nothing at
// all, and the 16:14:30 local goroutine dump showed "[sync.Mutex.Lock, 2
// minutes]" at the wrapper's Lock, on the stack
// completeCurrentSettlementReturnToTown -> sendCurrentCompletedDungeonReturnToTown
// -> sendCurrentFullDungeonReturnToTown -> here.
//
// The lock scope is otherwise unchanged: the op72 path still holds worldMu
// across its whole settlement commit, exactly as it did before, and every other
// caller still takes it in the wrapper.
func (s *Service) detachRuntimePartyDungeonMemberForTownReturnWhileWorldLocked(
	session *gameSession,
	owner *runtimePartyDungeonOwner,
	previousState alignedcmd.PartyState,
	source string,
) error {
	if s == nil || session == nil || owner == nil || previousState.PartyID <= 0 {
		return nil
	}
	identity, bound := s.boundGameSessionCharacterSnapshot(session)
	if !bound || identity.character == 0 || identity.generation == 0 {
		return errors.New("party dungeon town return has no current character identity")
	}

	nextState, result, removed := s.leaveManagedRuntimeParty(identity)
	if !removed && result.Reason != "not_in_party" {
		return fmt.Errorf("remove party dungeon town-returning member %d: %s", identity.character, result.Reason)
	}
	if !removed {
		// The party is already gone: the single-member disband rule detaches the
		// last member together with whoever left first, so a peer that returns at
		// the same moment can arrive at an empty membership. That is not an
		// error -- but this member's own client cache and its shared dungeon
		// roster row still have to be retired, which is what the rest of this
		// function does. Only a genuinely unusable session stays fatal above.
		s.clearRuntimePartyProjection(identity.character, identity.generation)
		nextState = alignedcmd.PartyState{}
	}
	removal := owner.removeMemberWhileWorldLocked(identity.character, identity.generation, nextState.UserID)
	if !removal.removed {
		return fmt.Errorf("party dungeon owner did not contain town-returning member %d", identity.character)
	}

	var projectionErr error
	if err := s.sendRuntimePartyMemberRemoved(session, uint16(previousState.PartyID), nextState); err != nil {
		projectionErr = errors.Join(projectionErr, fmt.Errorf("project party leave to returning member: %w", err))
	}
	if nextState.PartyID > 0 && len(runtimePartyMembers(nextState)) >= 2 {
		s.syncRuntimePartyUDPRelay(nextState)
	} else {
		s.closeRuntimePartyUDPRelay(previousState.PartyID)
	}
	// A concurrently confirmed final return has no survivor to notify. Its
	// caller may have captured the older two-member session snapshot before
	// waiting for worldMu, so do not feed that stale snapshot into the generic
	// disband broadcaster and accidentally republish it to the member that has
	// already left. A membership that was already gone needs no reduction
	// either: the leave that dissolved it already published that.
	if removed && len(removal.survivors) != 0 {
		if err := s.broadcastRuntimePartyMemberRemoved(session, previousState, nextState, identity.character, session); err != nil {
			projectionErr = errors.Join(projectionErr, fmt.Errorf("project party reduction to dungeon survivors: %w", err))
		}
	}

	leaderChanged := previousState.UserID == identity.character && len(removal.survivors) != 0
	newLeaderSlot := byte(0xff)
	if leaderChanged {
		// The replacement leader's slot comes from the shared roster this call
		// just reduced, not from the party snapshot: with the single-member
		// disband rule (2026-09-22) the party can be gone entirely (nextState
		// empty), while the dungeon still has to hand its host slot to the
		// member that stayed inside. removeMemberWhileWorldLocked already
		// re-elected that member -- the party manager's new leader when there is
		// one, otherwise the lowest occupied slot.
		owner.mu.Lock()
		replacementLeader := owner.leaderID
		newLeaderSlot = owner.leaderSlot
		owner.mu.Unlock()
		if newLeaderSlot == 0xff {
			for _, survivor := range removal.survivors {
				if survivor.userID == replacementLeader {
					newLeaderSlot = survivor.slot
					break
				}
			}
		}
		if newLeaderSlot == 0xff {
			projectionErr = errors.Join(projectionErr, fmt.Errorf(
				"party dungeon replacement leader %d has no stable slot",
				replacementLeader,
			))
		}
	}

	survivors := make([]runtimePartyDungeonLeaveSurvivor, 0, len(removal.survivors))
	for _, survivor := range removal.survivors {
		survivors = append(survivors, runtimePartyDungeonLeaveSurvivor{
			userID:     survivor.userID,
			generation: survivor.generation,
			slot:       survivor.slot,
		})
	}
	// op9 transfers the party-list leader, while op26 owns the live dungeon
	// controller/host slot. If the departing member was the leader, leaving
	// op26 behind keeps the survivor's combat stream addressed to an actor that
	// has already returned to town; the visible symptom is valid skills
	// resolving as one damage or no authoritative hit at all.
	projectionErr = errors.Join(projectionErr, s.projectRuntimePartyDungeonLeaveToSurvivors(
		identity.character,
		survivors,
		func(peerRuntime *runtimeDungeonState) bool {
			return peerRuntime.partyDungeonOwner == owner
		},
		leaderChanged,
		newLeaderSlot,
		"party_dungeon_member_confirmed_town_return",
	))

	s.logGameEvent(session, "game-party-dungeon-member-town-return-detached",
		"party_id", previousState.PartyID,
		"char_id", identity.character,
		"old_leader_id", previousState.UserID,
		"new_leader_id", nextState.UserID,
		"survivor_count", len(removal.survivors),
		"source", source,
		"leader_changed", leaderChanged,
		"new_leader_slot", newLeaderSlot,
		"projection", "op9_kind2_then_op153_then_optional_op26_then_scene_op6")
	return projectionErr
}

// ensureCurrentConfirmedDungeonReturnPlayerState consumes the direct
// dungeon-return op24 generation only after a real town-side request confirms
// that the client accepted it. The dungeon owner is detached before this
// function runs. The accepted completed-return actor route performs repository
// reads and transport writes, so no dungeon lock may be held while it runs.
func (s *Service) ensureCurrentConfirmedDungeonReturnPlayerState(
	session *gameSession,
	source string,
) error {
	if session == nil || session.selectedCharacterID == 0 {
		return nil
	}
	session.dungeon.mu.Lock()
	pending := session.confirmedDungeonReturnStatePending
	routeArmed := session.confirmedDungeonReturnRouteArmed
	transition := cloneCurrentDungeonTownTransition(session.confirmedDungeonReturnTransition)
	finalGateResetSent := session.confirmedDungeonReturnFinalGateResetSent
	session.dungeon.mu.Unlock()
	if !pending {
		return nil
	}
	if len(transition.Body) == 0 || transition.ActorObjectKey == 0 {
		return errors.New("confirmed dungeon return has no frozen town transition")
	}

	characterID := session.selectedCharacterID
	ownerChannel := currentConnectionTownActorOwnerContext(session)
	if !routeArmed {
		session.townMu.Lock()
		if session.selectedCharacterID != characterID {
			session.townMu.Unlock()
			return fmt.Errorf(
				"confirmed dungeon return character changed before actor rebuild: armed=%d selected=%d",
				characterID,
				session.selectedCharacterID,
			)
		}
		session.townActorOwnerChannel = ownerChannel
		session.townMu.Unlock()
		clearCurrentDungeonSelectContext(session)
		clearCurrentTownSelectorOrigin(session)
		s.armCurrentInitialTownRoute(session, characterID)
		session.sceneBootstrapTailDeferred = true
		session.dungeon.mu.Lock()
		if session.selectedCharacterID == characterID && session.confirmedDungeonReturnStatePending {
			session.confirmedDungeonReturnRouteArmed = true
		}
		session.dungeon.mu.Unlock()
	}

	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	loadedCharacterID, characterName, character, hasCharacter := s.selectedCharacterForEnter(ctx, session)
	if loadedCharacterID == 0 || loadedCharacterID != characterID || !hasCharacter {
		return fmt.Errorf(
			"confirmed dungeon town route character mismatch: selected=%d loaded=%d found=%t",
			characterID,
			loadedCharacterID,
			hasCharacter,
		)
	}
	objectBody := s.buildCurrentSceneObjectListBodyForSessionInContext(
		ctx,
		session,
		characterID,
		characterName,
		character,
		hasCharacter,
		ownerChannel,
	)
	if !suppressCurrentSceneObjectActorStateEvent(objectBody) {
		return errors.New("suppress confirmed-return actor-state initialization event")
	}
	adventureSummary := s.currentAccountAdventureGroupSummaryForPacket(ctx, session, character, hasCharacter)
	mode1Body := s.buildCurrentActorBindingMode1BodyForSelectedWithEquipmentInContext(
		ctx,
		session,
		character,
		hasCharacter,
		characterID,
		uint32(adventureSummary.ManageLevel),
		ownerChannel,
		true,
	)

	session.townMu.Lock()
	routeErr := s.sendCurrentTownActorRoutePacketsLocked(
		ctx,
		session,
		characterID,
		transition.TownID,
		transition.AreaID,
		currentSceneTransitionRow{
			ObjectOrResourceKey: transition.ActorObjectKey,
			Value1:              uint16(transition.PositionX),
			Value2:              uint16(transition.PositionY),
			Value3:              transition.Direction,
			Value4:              transition.AreaState,
		},
		objectBody,
		mode1Body,
		transition.Body,
		true,
		true,
	)
	routeStage := session.initialTownRouteStage
	questSnapshotsSeeded := session.initialTownQuestSnapshotsSent
	session.townMu.Unlock()
	session.dungeon.mu.Lock()
	if session.selectedCharacterID == characterID && session.confirmedDungeonReturnStatePending &&
		routeStage >= currentInitialTownRouteActionTableSent {
		session.confirmedDungeonReturnActionTableSent = true
	}
	session.dungeon.mu.Unlock()
	if routeErr != nil {
		return routeErr
	}
	if routeStage < currentInitialTownRouteTransitionSent {
		return nil
	}

	// Match the normal completed-return boundary exactly: actor/action-table,
	// equipment, task/BUFF state and the final typed op24 are committed first;
	// only then clear the native item-operation wait.  In particular this path
	// must not invent the post-op24 op105/op102/op37/op30/op120 generation that
	// live comparison proved exists only on the broken mid-run return.
	if !finalGateResetSent {
		if err := s.sendCurrentConsumableUseGateResetPacket(
			session,
			source+"_after_normal_completed_town_route",
		); err != nil {
			return err
		}
		session.dungeon.mu.Lock()
		if session.selectedCharacterID == characterID && session.confirmedDungeonReturnStatePending {
			session.confirmedDungeonReturnFinalGateResetSent = true
		}
		session.dungeon.mu.Unlock()
	}
	if questSnapshotsSeeded {
		if err := s.sendCurrentAcceptableQuestListForSession(
			session,
			source+"_post_op24_actor_ready_refresh",
		); err != nil {
			return err
		}
	}
	if err := s.sendSelectedActorUserStateRefreshIfNonDefault(
		session,
		source+"_post_op24_reapply_persisted_visibility",
	); err != nil {
		return err
	}

	session.townMu.Lock()
	if session.selectedCharacterID == characterID {
		session.townSceneReadyCharacterID = characterID
		// Its two sibling commit paths (commitPendingDungeonReturnBeforeTownPosition
		// and commitPendingDungeonReturnBeforeTownEnterSelect) seed the town
		// position snapshot alongside the ready character id; this fallback only
		// set the id, so currentTownEnterSelectReady failed on the position owner
		// and the first gate op15 opened no selector. Seed it from the frozen
		// return transition so a valid position is present the moment the client
		// is proven back in town.
		clearCurrentTownSelectorOriginLocked(session)
		session.townPositionSnapshot = currentTownPositionSnapshot{
			CharacterID:   characterID,
			TownID:        transition.TownID,
			AreaID:        transition.AreaID,
			PositionX:     uint16(transition.PositionX),
			PositionY:     uint16(transition.PositionY),
			MovementCode:  transition.Direction,
			PositionValid: true,
		}
	}
	session.townMu.Unlock()
	session.dungeon.mu.Lock()
	if session.selectedCharacterID == characterID {
		session.confirmedDungeonReturnStatePending = false
		session.confirmedDungeonReturnActionTableSent = false
		session.confirmedDungeonReturnFinalGateResetSent = false
		session.confirmedDungeonReturnRouteArmed = false
		session.confirmedDungeonReturnTransition = currentDungeonTownTransition{}
	}
	session.dungeon.mu.Unlock()
	s.logGameEvent(session, "game-dungeon-back-to-village-player-state-rebuilt",
		"source", source,
		"char_id", characterID,
		"owner_context", ownerChannel,
		"sequence", "normal_completed_return_op376_mode0_mode1_optional_skin_op205_op411_op14_op21_op574_op24_op1369_op42_then_op21_op574")
	return nil
}

func resetDungeonReturnSceneGates(session *gameSession) {
	if session == nil {
		return
	}
	session.runtimeAfterBlacklistSent = false
	session.runtimeFinishLoadingGateSent = false
	session.fpsFinishLoadingGateSent = false
	session.selectPreviewActorRemoved = false
	session.preDungeonContextPlayerStateSent = false
	session.dungeonSceneSkillInfoSent = false
	session.deferredDungeonUserStateObjectKey = 0
	session.currentFinishLoadingStateSent = false
	session.currentFinishLoadingCompletionSent = false
	session.postFinishLoadingPlayerStateSent = false
	session.returnTownFinishLoadingAckOnly = false
	// A confirmed non-completed return also creates a new town actor through the
	// staged post-transition mode1 path. Do not let the dungeon generation's
	// collection-box cursor suppress the town-side op1027 projection.
	session.collectboxSceneProjectionSent = false
	session.collectboxSelectionRefreshSent = false
}

func resetDungeonEntrySceneGates(session *gameSession) {
	if session == nil {
		return
	}
	session.dungeon.mu.Lock()
	session.confirmedDungeonReturnStatePending = false
	session.confirmedDungeonReturnActionTableSent = false
	session.confirmedDungeonReturnFinalGateResetSent = false
	session.confirmedDungeonReturnRouteArmed = false
	session.confirmedDungeonReturnTransition = currentDungeonTownTransition{}
	// Entering any scene retires a training-room BUFF replay that never got
	// its finish loading; the training route re-arms it right after this.
	session.trainingRoomBuffRegistrationPending = false
	session.dungeon.mu.Unlock()
	session.townMu.Lock()
	resetCurrentTownPostTransitionPlayerState(session)
	session.initialTownRouteCharacterID = 0
	session.initialTownRouteStage = currentInitialTownRouteIdle
	session.completedTownReturnBuffRegistrationPending = false
	session.initialTownLocationNotificationsSent = false
	session.initialTownQuestSnapshotsSent = false
	session.initialTownQuestHUDRefreshSent = false
	session.initialTownGuildStateProjectionSent = false
	session.initialTownSkillInfoPrepared = false
	session.initialTownSkillInfoSent = false
	session.initialTownSkillInfo = currentSceneSkillInfoProjection{}
	session.initialTownMode3RefreshPending = false
	session.initialTownLegacySceneReadyAccepted = false
	clearCurrentGameOptionRestoreSnapshot(session)
	clearCurrentCharacterOptionRestoreSnapshot(session)
	session.initialTownAdventureOverheadRefreshSent = false
	session.initialTownCombatPowerAffixesSent = false
	session.initialTownMoodProjectionSent = false
	session.townSceneReadyCharacterID = 0
	session.townActorOwnerChannel = currentSceneObjectContext
	session.townMu.Unlock()
	session.sceneBootstrapTailDeferred = false
	session.sceneBootstrapTailSent = false
	resetCurrentDeferredSelectSceneTailProgress(session)
	session.runtimeAfterBlacklistSent = false
	session.runtimeFinishLoadingGateSent = false
	session.fpsFinishLoadingGateSent = false
	session.selectedUserInfoRefreshSent = false
	session.selectedUserInfoMode3Sent = false
	session.currentSceneObjectListSent = false
	session.selectedItemListRefreshSent = false
	session.selectedItemListBootstrapCharacterID = 0
	session.selectedEquipmentUpdateSent = false
	session.selectedCreatureStateTableSent = false
	session.selectedRentalWalletStateSent = false
	session.expertJobInfoCharacterID = 0
	session.selectPreviewActorRemoved = false
	session.preDungeonContextPlayerStateSent = false
	session.postStartMapPlayerStateSent = false
	session.dungeonSceneSkillInfoSent = false
	session.deferredDungeonUserStateObjectKey = 0
	session.currentFinishLoadingStateSent = false
	session.currentFinishLoadingCompletionSent = false
	session.postFinishLoadingPlayerStateSent = false
	session.returnTownFinishLoadingAckOnly = false
	// Both dungeon entry and the first return-to-town attempt replace the native
	// selected actor. The next post-mode1 boundary must therefore replay op1027
	// even when the early select-character cache was already initialized.
	session.collectboxSceneProjectionSent = false
	session.collectboxSelectionRefreshSent = false
}
