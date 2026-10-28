package dnfbridge

import (
	"context"
	"fmt"

	"longheng.io/server/internal/modules/dnf/dnfenum"
	"longheng.io/server/internal/modules/dnf/dungeoncmd"
	"longheng.io/server/internal/modules/dnf/worldmap"
)

const currentPungjinTrainingDungeonID uint32 = 5000

// currentJobChangeTrialDungeonID is 转职体验「领悟时间」
// (dungeon/Cataclysm/ChangeClass/7142_ChangeClass.dgn), the room the 90CN
// client's 转职体验 "体验" button drops the character into after
// REQUEST_ITEM_GROWTYPE_EXPERIENCE switched its grow type. It shares the
// training room's direct-entry button shape, which is why it is named here
// beside 5000 rather than inlined in the entry table, and its exit is a
// weakness-exempt room too - see currentWeaknessExemptDungeonIDs.
const currentJobChangeTrialDungeonID uint32 = 7142

// currentDirectDungeonEntryDungeonIDs lists the dungeons whose current-client
// buttons emit the "direct entry" request shape (runtime state 1 + UINT16_MAX
// token + every owned mode field zero): the training room (5000) and the
// job-change trial experience "领悟时间" (7142). The selector's 练习模式 switch
// sends that pair too, so the shape alone does not identify these rooms - the
// dungeon id does, and only these two take the guided route below.
var currentDirectDungeonEntryDungeonIDs = map[uint32]struct{}{
	currentPungjinTrainingDungeonID: {},
	currentJobChangeTrialDungeonID:  {},
}

// currentDungeonGuidedDirectEntryRoom reports whether the room owns the guided
// direct-entry route above. Those two buttons send the same C2S op16 pair that
// the selector's 练习模式 switch sends (state=1 + token=UINT16_MAX), but they are
// not practice runs: 修炼场 is a free room whose own fatigue exemption is
// deliberate, and 领悟时间 is a real job-change trial. They therefore keep the
// ordinary runtime policy when that pair is resolved - see
// currentDungeonProtocolModeForRoom.
func currentDungeonGuidedDirectEntryRoom(dungeonID int64) bool {
	if dungeonID <= 0 || dungeonID > int64(^uint32(0)) {
		return false
	}
	_, guided := currentDirectDungeonEntryDungeonIDs[uint32(dungeonID)]
	return guided
}

// isCurrentDirectDungeonEntryRequest identifies the "direct entry" request
// shape emitted by the current client's direct-button transitions (enter
// training room, job-change trial experience). It is not an ordinary selector
// grant: runtime state 1 and the UINT16_MAX token tell the client to carry the
// actor/context already prepared by op15 directly into op28/op29.
// Every other owned field must be zero; a nonzero opaque field or party state
// is an ordinary selection that keeps the ordinary route.
func isCurrentDirectDungeonEntryRequest(request dungeoncmd.SelectDungeonRequest) bool {
	if _, ok := currentDirectDungeonEntryDungeonIDs[request.DungeonID]; !ok {
		return false
	}
	return request.Difficulty == 0 &&
		request.EntryOption == 0 &&
		request.SelectionMode == 0 &&
		request.RuntimeState == 1 &&
		request.RuntimeToken == 0xffff &&
		request.Reserved == 0 &&
		request.PartyState == 0 &&
		request.LeaderObjectKey == 0 &&
		request.SpecialMode == 0 &&
		len(request.OpaqueTail) == 0
}

// isCurrentPungjinTrainingDirectEntry narrows the direct-entry shape to the
// training-room dungeon itself, for diagnostics that must stay room-specific.
func isCurrentPungjinTrainingDirectEntry(request dungeoncmd.SelectDungeonRequest) bool {
	return request.DungeonID == currentPungjinTrainingDungeonID &&
		isCurrentDirectDungeonEntryRequest(request)
}

// sendCurrentPungjinTrainingDirectEntry preserves the special current-client
// entry ABI: op28 followed immediately by op29. Live 90CN traffic proves op15
// has already installed the local actor, and the nearby 90US implementation
// independently pins the direct-button transition itself to 0x001c/0x001d.
// Replaying the ordinary op16 ACK/resource/player/placement chain constructs a
// second scene owner and crashes while the training map is loading. Our op15
// intentionally carries only an actor binding, however, so after op29 owns the
// room this route republishes the selected character's containers, complete
// equipment-bearing mode1, dungeon skill manager and creature state without
// op9/op120/op3.
func (s *Service) sendCurrentPungjinTrainingDirectEntry(
	session *gameSession,
	runtime *runtimeDungeonState,
	scene worldmap.DungeonRoomScene,
	passiveDropPlan currentDungeonPassiveObjectDropPlan,
) error {
	if s == nil || session == nil || runtime == nil || runtime.Session == nil ||
		!isCurrentDirectDungeonEntryRequest(runtime.Request) {
		return fmt.Errorf("current direct dungeon entry owner is unavailable")
	}
	// This special route bypasses the ordinary entry branch, so explicitly
	// retire town snapshot gates and make the carried actor's character state
	// eligible for a fresh post-op29 projection.
	resetDungeonEntrySceneGates(session)
	entryPackets, err := buildCurrentDungeonEntryPacketsWithPassiveDrops(runtime, scene, passiveDropPlan.ExtraEntries)
	if err != nil {
		return err
	}
	s.logGameEvent(session, "game-dungeon-training-room-direct-entry-send",
		"char_id", session.selectedCharacterID,
		"dungeon_id", runtime.Dungeon.ID,
		"maze_index", runtime.MazeIndex,
		"room", scene.Coordinate.String(),
		"map_id", scene.Map.Map.ID,
		"map_path", scene.Map.Map.Path,
		"request_runtime_state", runtime.Request.RuntimeState,
		"request_runtime_token", runtime.Request.RuntimeToken,
		"response_msg_ids", []uint16{currentDungeonInfoNotification, currentDungeonStartNotification},
		"sequence", "direct_button_op28_then_op29_then_character_projection",
		"suppressed", "ordinary_op16_ack_op5_actor_rebind_op27_op358_op359_op356_op124_op9_op120_op3",
		"reason", "current_training_room_runtime_state_1_reuses_op15_actor_context")
	if err := s.sendGameUpperRawClass(session, currentDungeonInfoNotification, entryPackets.DungeonInfo, 0); err != nil {
		return err
	}
	if err := s.sendGameUpperRawClass(session, currentDungeonStartNotification, entryPackets.StartMap, 0); err != nil {
		return err
	}
	announcedActors, err := runtime.Room.AnnounceAllActors(runtime.Session)
	if err != nil {
		return fmt.Errorf("commit current training-room actors after op29 send: %w", err)
	}
	if err := runtime.cacheCurrentDungeonRoom(); err != nil {
		return fmt.Errorf("cache current training-room after actor announce: %w", err)
	}
	if err := commitCurrentDungeonPassiveObjectDrops(runtime, scene, passiveDropPlan); err != nil {
		return fmt.Errorf("commit current training-room passive-object drops after op29 send: %w", err)
	}
	if err := s.consumeCurrentDungeonRoomFatigueAfterCommit(session, runtime, "training_room_direct_entry_after_op29_commit"); err != nil {
		return fmt.Errorf("consume current training-room fatigue: %w", err)
	}
	if err := s.sendCurrentPungjinTrainingPlayerProjection(session); err != nil {
		return fmt.Errorf("send current training-room player projection: %w", err)
	}

	// op15 supplied the actor/context used by this direct route; the dedicated
	// projection above has now hydrated that same owner with durable character
	// state. Do not schedule the ordinary placement tail afterward.
	session.preDungeonContextPlayerStateSent = true
	session.postStartMapPlayerStateSent = true
	session.sceneBootstrapTailDeferred = false
	session.sceneBootstrapTailSent = true
	session.currentFinishLoadingStateSent = false
	session.currentFinishLoadingCompletionSent = false
	session.postFinishLoadingPlayerStateSent = false
	session.returnTownFinishLoadingAckOnly = false
	// The ordinary route replays op559 from the placement tail, right after the
	// class-0/op124 layout commit that arms the client's skill-modifier rebuild.
	// This route sends neither, so the registration has to wait for the client
	// to finish loading the training map and own its own scene - sending it any
	// earlier repopulates the panel while the rebuild is silently skipped,
	// which is indistinguishable from not sending it at all.
	session.trainingRoomBuffRegistrationPending = true

	s.logGameEvent(session, "game-dungeon-training-room-direct-entry-committed",
		"char_id", session.selectedCharacterID,
		"dungeon_id", runtime.Dungeon.ID,
		"room", scene.Coordinate.String(),
		"map_id", scene.Map.Map.ID,
		"announced_actor_count", announcedActors,
		"actor_state", "carried_from_current_op15_context_then_full_character_projection",
		"scene_tail", "client_owned_training_room_direct_entry")
	s.leaveTownPresenceForDungeon(session)
	return nil
}

// sendCurrentPungjinTrainingPlayerProjection hydrates the actor shell carried
// by op15 without creating or placing another room actor. The ordinary dungeon
// route performs these state writes as part of a larger placement tail;
// training-room direct entry must keep only the state-bearing subset or the
// current client crashes during scene ownership transition. The authoritative
// dungeon op19 is installed right after the complete mode1 and before the
// client reports finish loading, exactly like the ordinary route: passives
// such as 唤魔 : 蛇腹剑 replace the weapon form while the actor action table
// is constructed.
func (s *Service) sendCurrentPungjinTrainingPlayerProjection(session *gameSession) error {
	if session == nil || session.selectedCharacterID == 0 {
		return fmt.Errorf("current training-room player projection requires selected character")
	}
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	charID, _, character, hasCharacter := s.selectedCharacterForEnter(ctx, session)
	if charID == 0 {
		charID = session.selectedCharacterID
	}
	if err := s.sendCurrentSelectInventoryBootstrap(
		session,
		"training_room_after_op29_before_full_mode1",
	); err != nil {
		return err
	}
	adventureSummary := s.currentAccountAdventureGroupSummaryForPacket(ctx, session, character, hasCharacter)
	mode1Body := s.buildCurrentActorBindingMode1BodyForSelectedWithEquipmentInContext(
		ctx,
		session,
		character,
		hasCharacter,
		charID,
		uint32(adventureSummary.ManageLevel),
		byte(currentSceneObjectContext),
		true,
	)
	s.logPacketEvent("game-upper-training-room-full-character-mode1-send",
		"conn_id", session.connID,
		"channel_id", session.channel.ID,
		"char_id", charID,
		"object_key", currentSceneActorObjectKey(charID),
		"owner_channel", currentSceneObjectContext,
		"adventure_manage_level", adventureSummary.ManageLevel,
		"body_len", len(mode1Body),
		"body_source", "carried_op15_actor_post_op29_complete_equipment_state")
	if err := s.sendGameUpperRawClass(
		session,
		uint16(dnfenum.CmdPacketSetUDPIPPort),
		mode1Body,
		0,
	); err != nil {
		return err
	}
	session.selectedUserInfoRefreshSent = true
	if hasCharacter && (s.skillCatalog != nil || len(s.initialSkillsByJob) != 0) {
		projection, ready, err := s.buildCurrentSceneSkillInfoProjection(
			session,
			ctx,
			character,
			"training_room_after_full_mode1_before_finish_loading",
		)
		if err != nil {
			return fmt.Errorf("build training-room skill initialization: %w", err)
		}
		if ready {
			if err := s.sendCurrentSceneSkillInfoProjectionWithTreeRestore(
				session,
				projection,
				"training_room_after_full_mode1_before_finish_loading",
			); err != nil {
				return fmt.Errorf("send training-room skill initialization: %w", err)
			}
			session.dungeonSceneSkillInfoSent = true
		}
	}
	if err := s.sendSelectedCreatureInitialStateAfterMode1(
		session,
		"training_room_after_full_mode1",
	); err != nil {
		return err
	}
	// 修炼场走的是**这条专用直入路径**（客户端 op15 → 服务端 op36/27/28/29 → 这里），
	// 它自建 mode1 与装备投影，完全不经过 sendCurrentPostStartMapPlayerPlacement。
	// 而 mode1/mode3 的创建行**不带**勋章守护珠的 raw socket 字与称号的 raw 随机附魔，
	// 所以这条路上此前从没补过 —— 现像就是带宝珠的勋章进修炼场后"没有已装备的守护珠"，
	// 图标与属性一起消失，出图才恢复（2026-10-08 现场，13:55:48 日志已证实走这条）。
	s.restoreCurrentSceneRawEquipmentRows(session, "training_room_after_full_mode1")
	s.logPacketEvent("game-upper-training-room-full-character-projection-finished",
		"conn_id", session.connID,
		"channel_id", session.channel.ID,
		"char_id", charID,
		"skill_manager_sent", session.dungeonSceneSkillInfoSent,
		"sequence", "op13_containers_item_locks_full_mode1_op19_op14_optional_op105_op102_optional",
		"suppressed", "op358_action_table_mode0_op359_op356_op124_op9_op120_op3")
	return nil
}
