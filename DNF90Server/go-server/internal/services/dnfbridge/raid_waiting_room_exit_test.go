package dnfbridge

import (
	"testing"
)

// 现场（2026-09-20 09:58:41Z，raid 1507329，两人队打完 220 心脏、攻坚结束）：
// 两名成员都站在 19/2 等候室里，客户端反复发 op36 想走到 19/1，服务端每次都回
//   game-town-set-user-area-blocked … reason=active_dungeon_runtime_rejects_town_area_request
// 紧接着这个拒绝之前是
//   game-dungeon-completed-raid-waiting-presence-deferred … reason=raid_membership_missing
// 也就是：攻坚没了，回城路线里「恢复等候区 presence」这一步拒绝执行，于是按注释
// 「保留 runtime 当重试路径」把**已完成的 run 留在 session 上**；而 town_move.go:83
// 只要看到 session 上还有 dungeon runtime 就拒绝一切 op36 ⇒ 玩家出不去等候室。
//
// 这个测试钉住：攻坚已经不存在时，完成的 run 必须被正常退休（presence 退回频道城镇的
// 普通实例），之后 op36 才可能被受理。

const (
	raidWaitingRoomExitTestReturnOp = uint16(72)
	raidWaitingRoomExitTestSource   = "raid_ended_leave_waiting_room_test"
)

func raidWaitingRoomExitTestTransition() currentDungeonTownTransition {
	return currentDungeonTownTransition{
		TownID:    byte(currentAntonRaidTownID),
		AreaID:    currentAntonRaidWaitingAreaID,
		AreaState: currentAntonRaidAreaState,
	}
}

// TestCompletedRaidRunWhoseRaidIsGoneStillLeavesTheWaitingRoom covers the report
// "攻坚结束后退出队伍了 没法离开等候室了".
func TestCompletedRaidRunWhoseRaidIsGoneStillLeavesTheWaitingRoom(t *testing.T) {
	fixture := newRuntimeRaidPhaseForceReturnFixture(t)
	// The member is inside a phase-one gate of a raid that is about to disappear.
	runtime := fixture.enterGateRun(t, 99, 11, antonBlackFogSourceMapID)
	session, ok := fixture.service.onlineGameSession(99)
	if !ok {
		t.Fatal("member has no session")
	}
	// The raid ends: finished, disbanded, or the member left it. What matters is
	// that it no longer owns this run.
	fixture.service.raidMu.Lock()
	delete(fixture.service.raids, fixture.raidKey)
	fixture.service.raidMu.Unlock()

	if err := fixture.service.sendCurrentActiveDungeonReturnToTown(
		session,
		runtime,
		raidWaitingRoomExitTestTransition(),
		raidWaitingRoomExitTestReturnOp,
		raidWaitingRoomExitTestSource,
	); err != nil {
		t.Fatalf("completed return with the raid gone: %v", err)
	}

	// The completed run must be retired: an attached run is what refused every
	// later area request.
	if kept := fixture.sessionRuntime(t, 99); kept != nil {
		t.Fatalf("the completed run stayed attached after the raid ended (dungeon %d): "+
			"town_move.go refuses every op36 in that state", kept.Dungeon.ID)
	}
	// And the member must be back in the channel town's ordinary instance, not in a
	// raid instance nobody owns any more.
	presence, present := fixture.service.onlinePlayers.PlayerForCharacter(99)
	if !present {
		t.Fatal("no town presence was restored after the raid ended")
	}
	if presence.TownID != byte(currentAntonRaidTownID) || presence.AreaID != currentAntonRaidWaitingAreaID {
		t.Fatalf("presence=%d/%d want %d/%d", presence.TownID, presence.AreaID,
			currentAntonRaidTownID, currentAntonRaidWaitingAreaID)
	}
	if presence.InstanceKey != 0 {
		t.Fatalf("presence still points at the dead raid instance %d", presence.InstanceKey)
	}
}

// TestCompletedRaidRunKeepsItsWaitingInstanceWhileTheRaidLives is the guard in the
// other direction: as long as the raid owns the run, the member must come back
// INTO the raid's waiting instance (the private scene the squad shares), and the
// run must be retired then too.
func TestCompletedRaidRunKeepsItsWaitingInstanceWhileTheRaidLives(t *testing.T) {
	fixture := newRuntimeRaidPhaseForceReturnFixture(t)
	runtime := fixture.enterGateRun(t, 99, 11, antonBlackFogSourceMapID)
	session, ok := fixture.service.onlineGameSession(99)
	if !ok {
		t.Fatal("member has no session")
	}
	if err := fixture.service.sendCurrentActiveDungeonReturnToTown(
		session,
		runtime,
		raidWaitingRoomExitTestTransition(),
		raidWaitingRoomExitTestReturnOp,
		"raid_alive_waiting_room_return_test",
	); err != nil {
		t.Fatalf("completed return while the raid lives: %v", err)
	}
	if kept := fixture.sessionRuntime(t, 99); kept != nil {
		t.Fatalf("the completed run stayed attached while the raid lives (dungeon %d)", kept.Dungeon.ID)
	}
	presence, present := fixture.service.onlinePlayers.PlayerForCharacter(99)
	if !present {
		t.Fatal("no waiting-area presence was restored")
	}
	if presence.InstanceKey != fixture.raidKey {
		t.Fatalf("presence instance=%d want the live raid instance %d",
			presence.InstanceKey, fixture.raidKey)
	}
}
