package dnfbridge

import (
	"fmt"

	"longheng.io/server/internal/modules/dnf/worldmap"
)

// currentDungeonDestroyObjectClearConditionSource binds a client-owned
// passive object to the active maze's explicit one-object clear condition.
// The object is proven from the current map, the current maze and the room's
// runtime actor table instead of from op38, whose target field is u16.
//
// The object code is deliberately NOT filtered by width. An earlier version
// rejected every code that fits u16 on the assumption that such a code is
// always an installed actor. Luke phase two disproves that assumption: the
// energy-control boss room declares its clear target as a plain map object.
// dungeon/Luke/Phase2/Energy_control.dgn carries
//
//	[clear condition]
//	[destroy object]
//	64062 1
//	[/clear condition]
//
// and the boss map map/Luke_Raid/Phase2/Energy_control/74232.map declares that
// same 64062 only under [passive object], with no [monster] section at all. The
// client therefore kills it locally and reports an op39 that no runtime actor
// owns, while the maze still waits for it. Rejecting the u16 code stranded the
// room: the live log shows
// game-dungeon-monster-death-blocked object_key=64062 reason=non_hostile_ai_retirement_rejected
// error="dnf dungeon monster runtime object is not found: object_key=64062"
// and the run never settled.
//
// The width rule is replaced by the property it was standing in for: a code is
// only accepted here when the room holds no runtime actor for it, so a real
// announced monster/AI keeps its own path. Everything else stays fail-closed -
// exactly one matching passive placement in the current map AND an exact
// [destroy object] target/count row in the current maze are still required.
func currentDungeonDestroyObjectClearConditionSource(
	runtime *runtimeDungeonState,
	scene worldmap.DungeonRoomScene,
	objectKey uint32,
) string {
	if runtime == nil || runtime.Room == nil || runtime.Session == nil ||
		!scene.Cleared || !scene.Boss ||
		!runtime.BossSet || scene.Coordinate != runtime.BossCoordinate ||
		runtime.MazeIndex < 0 || runtime.MazeIndex >= len(runtime.Dungeon.Mazes) {
		return ""
	}
	room := runtime.Room.Snapshot()
	if room.Coordinate != scene.Coordinate || room.MapID != scene.Map.Map.ID {
		return ""
	}
	if runtime.Room.HostsRuntimeActor(objectKey) {
		return ""
	}

	placementCount := int64(0)
	for _, object := range scene.PassiveObjects {
		if object.ObjectID == int64(objectKey) {
			placementCount++
		}
	}
	if placementCount != 1 {
		return ""
	}

	for _, condition := range runtime.Dungeon.Mazes[runtime.MazeIndex].ClearConditions {
		if normalizeDungeonPVFSymbol(condition.Type) == "destroy object" &&
			condition.TargetID == int64(objectKey) && condition.Count == placementCount {
			return fmt.Sprintf(
				"current_pvf_clear_condition_destroy_object_target_%d_count_%d",
				condition.TargetID,
				condition.Count,
			)
		}
	}
	return ""
}

// handleCurrentDungeonDestroyObjectClearConditionDeathLocked accepts the
// variable op39 emitted after the current client has already removed a PVF
// passive object locally. No op38 is sent because its target field is u16.
// The caller owns session.dungeon.mu.
func (s *Service) handleCurrentDungeonDestroyObjectClearConditionDeathLocked(
	session *gameSession,
	runtime *runtimeDungeonState,
	scene worldmap.DungeonRoomScene,
	objectKey uint32,
) (bool, error) {
	source := currentDungeonDestroyObjectClearConditionSource(runtime, scene, objectKey)
	if source == "" {
		return false, nil
	}
	s.logGameEvent(session, "game-dungeon-destroy-object-clear-condition-accepted",
		"char_id", session.selectedCharacterID,
		"dungeon_id", runtime.Dungeon.ID,
		"maze_index", runtime.MazeIndex,
		"room", scene.Coordinate.String(),
		"map_id", scene.Map.Map.ID,
		"object_key", objectKey,
		"death_ack_sent", false,
		"reason", "current_exe_op38_target_is_u16_and_client_owned_passive_object_is_already_destroyed",
		"source", source)
	if err := s.completeCurrentDungeonOrdinaryFinalRoomAfterDeathLocked(
		session,
		runtime,
		scene,
		objectKey,
	); err != nil {
		return true, err
	}
	// 通关目标在客户端那边是**队伍共享**的：boss 房一只怪都没有，全队只有**一个**
	// 客户端会报这个 op39（线上 2026-09-29 的 5066 能源储藏室，三次 run 都是两人
	// 同时进 boss 房、只有一人被结算、另一人 11~28 秒后被迫 op42 弃权）。所以接受
	// 之后必须替**同队还在同一个房间**的成员各做一遍收尾，否则他们没有任何触发点，
	// 只能按 ESC。
	s.completeCurrentDungeonDestroyObjectClearForPartyLocked(session, runtime, scene, objectKey)
	return true, nil
}

// completeCurrentDungeonDestroyObjectClearForPartyLocked 把一次已接受的
// destroy-object 通关收尾**补发给同队的其他成员**。
//
// 为什么需要它：`[clear condition][destroy object]` 的房间（卢克二阶段 5063
// 能源控制室 64062 / 5066 能源储藏室 64060，见
// TestRealScriptPVFNoAntonDungeonDeclaresDestroyObjectClearCondition 的全表扫描）
// 的 boss 房只有**一个客户端自有被动对象**、没有怪物。那个对象是全队共享的，所以
// 只有一名成员的客户端会发 op39；其他人既没怪可打、房间又已经是 clear，服务端不
// 替他们收尾就永远等不到结算。
//
// 边界（一条都不能松，逐条对应测试）：
//
//  1. 只对**同一个团 + 同一个副本迷宫 + 同一个房间坐标 + 同一张地图**的成员fan-out；
//     跨团（raidDungeon.RaidKey 不同）一律不动。
//  2. 对方的房间必须是 `Cleared && Boss`（由 completeCurrentDungeonOrdinaryFinalRoomAfterDeathLocked
//     内部的 currentDungeonOrdinaryFinalRoomReady 再确认一次）。
//  3. 对方**尚未** ordinaryFinalRoomClearAccepted —— 这是幂等的第一道闸；第二道是
//     completeCurrentDungeonOrdinaryFinalRoomAfterDeathLocked 开头的同名字段检查，
//     所以两个人**都**报了 op39 也不会有人拿到两次 op115/op31。
//  4. 还在别的房间的人**不结算**（坐标或地图不同直接跳过）。
//  5. 锁纪律同战况表回城：onlineGameSession + dungeon.mu.TryLock()，**忙则跳过、
//     绝不阻塞对端**（等待会把两个会话互锁），跳过计数写进汇总日志而不是静默。
//
// 调用方持有 session.dungeon.mu（本函数的调用点就是这样），只对**其他**会话 TryLock，
// 不会与自己的锁重入。
func (s *Service) completeCurrentDungeonDestroyObjectClearForPartyLocked(
	session *gameSession,
	runtime *runtimeDungeonState,
	scene worldmap.DungeonRoomScene,
	targetObjectKey uint32,
) {
	if s == nil || session == nil || runtime == nil || runtime.Session == nil {
		return
	}
	party := runtimePartyStateSnapshot(session)
	if party.PartyID <= 0 {
		// 单人 run：没有别人要补，行为与改动前逐字相同。
		return
	}
	members := runtimePartyMembers(party)
	if len(members) <= 1 {
		return
	}
	callerID := session.selectedCharacterID
	callerRaidKey := runtime.raidDungeon.RaidKey
	completed, skippedBusy, skippedOtherRoom, offline := 0, 0, 0, 0
	for _, member := range members {
		if member.UserID == 0 || member.UserID == callerID {
			continue
		}
		outcome := s.completeCurrentDungeonDestroyObjectClearForMember(
			member.UserID, callerID, callerRaidKey, runtime, scene, targetObjectKey)
		switch outcome {
		case destroyObjectPartyCompleted:
			completed++
		case destroyObjectPartySkippedBusy:
			skippedBusy++
		case destroyObjectPartySkippedOtherRoom:
			skippedOtherRoom++
		case destroyObjectPartyOffline:
			offline++
		}
	}
	if completed == 0 && skippedBusy == 0 && skippedOtherRoom == 0 && offline == 0 {
		return
	}
	s.logGameEvent(session, "game-dungeon-destroy-object-clear-party-fanout",
		"char_id", callerID,
		"dungeon_id", runtime.Dungeon.ID,
		"maze_index", runtime.MazeIndex,
		"room", scene.Coordinate.String(),
		"map_id", scene.Map.Map.ID,
		"raid_key", callerRaidKey,
		"object_key", targetObjectKey,
		"members", len(members),
		"completed", completed,
		"skipped_busy", skippedBusy,
		"skipped_other_room", skippedOtherRoom,
		"offline", offline,
		"reason", "the_shared_clear_object_is_reported_by_one_client_only")
}

// destroyObjectPartyOutcome is what one party member's fan-out attempt did.
type destroyObjectPartyOutcome int

const (
	destroyObjectPartyNotEligible destroyObjectPartyOutcome = iota
	destroyObjectPartyCompleted
	destroyObjectPartyOffline
	destroyObjectPartySkippedBusy
	destroyObjectPartySkippedOtherRoom
)

// completeCurrentDungeonDestroyObjectClearForMember runs the same final-room
// completion for one teammate. Every guard that makes the fan-out safe lives here.
func (s *Service) completeCurrentDungeonDestroyObjectClearForMember(
	memberID uint16,
	callerID uint16,
	callerRaidKey uint32,
	callerRuntime *runtimeDungeonState,
	callerScene worldmap.DungeonRoomScene,
	targetObjectKey uint32,
) destroyObjectPartyOutcome {
	target, online := s.onlineGameSession(memberID)
	if !online || target == nil {
		return destroyObjectPartyOffline
	}
	if !target.dungeon.mu.TryLock() {
		// Documented discipline (see scheduleRuntimeRaidForcedGroupReturn and the
		// war-table return): that member is inside its own packet path, and
		// waiting here would deadlock two sessions against each other.
		s.logPacketEvent("game-dungeon-destroy-object-clear-party-member-skipped",
			"char_id", callerID,
			"member_char_id", memberID,
			"dungeon_id", callerRuntime.Dungeon.ID,
			"room", callerScene.Coordinate.String(),
			"map_id", callerScene.Map.Map.ID,
			"reason", "peer_session_busy_never_block_a_packet_path")
		return destroyObjectPartySkippedBusy
	}
	defer target.dungeon.mu.Unlock()

	targetRuntime := target.dungeon.runtime
	if targetRuntime == nil || targetRuntime == callerRuntime ||
		targetRuntime.Session == nil || !dungeonRuntimeOwnsCharacter(targetRuntime, memberID) {
		return destroyObjectPartyNotEligible
	}
	// 1) same raid (a raid member never completes a non-raid run and vice versa)
	// and 2) the same dungeon + maze.
	if targetRuntime.raidDungeon.RaidKey != callerRaidKey ||
		targetRuntime.Dungeon.ID != callerRuntime.Dungeon.ID ||
		targetRuntime.MazeIndex != callerRuntime.MazeIndex {
		return destroyObjectPartyNotEligible
	}
	targetScene, ok := targetRuntime.Session.Scene()
	if !ok {
		return destroyObjectPartyNotEligible
	}
	// 3) the same room coordinate on the same map, and 4) it is a cleared boss
	// room -- i.e. exactly the room whose shared object was just reported.
	if targetScene.Coordinate != callerScene.Coordinate ||
		targetScene.Map.Map.ID != callerScene.Map.Map.ID {
		return destroyObjectPartySkippedOtherRoom
	}
	if !targetScene.Cleared || !targetScene.Boss {
		return destroyObjectPartyNotEligible
	}
	// 5) idempotence: a member that already settled never settles again, whichever
	// of the two op39 reports arrives first. (The completion function re-checks the
	// same field, so this is the outer half of the same guarantee.)
	if targetRuntime.ordinaryFinalRoomClearAccepted {
		return destroyObjectPartyNotEligible
	}
	if err := s.completeCurrentDungeonOrdinaryFinalRoomAfterDeathLocked(
		target,
		targetRuntime,
		targetScene,
		targetObjectKey,
	); err != nil {
		s.logGameEvent(target, "game-dungeon-destroy-object-clear-party-member-failed",
			"char_id", memberID,
			"caller_char_id", callerID,
			"dungeon_id", targetRuntime.Dungeon.ID,
			"room", targetScene.Coordinate.String(),
			"map_id", targetScene.Map.Map.ID,
			"object_key", targetObjectKey,
			"error", err)
		return destroyObjectPartyNotEligible
	}
	return destroyObjectPartyCompleted
}
