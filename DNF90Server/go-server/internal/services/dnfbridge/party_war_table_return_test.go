package dnfbridge

import (
	"reflect"
	"testing"

	"longheng.io/server/internal/modules/dnf/dnfenum"
	"longheng.io/server/internal/modules/dnf/raid"
)

// The operator's whole-party return rules (2026-09-20):
//
//	"阶段结束正常不应该解散队伍 应该保持组队状态 全队退出副本"
//	"战况表回城本就是设计给小队队长在副本中途带领全队回城的（回城后应当依然还是
//	 组队状态）现在是只能小队成员自己退出 然后重新组队"
//
// Both go through the interrupted-run town route, which used to detach the returning
// member from his ordinary party (dungeon_return_town.go:1063 with
// leaveRuntimePartyOnAbandonedDungeonExit / detachRuntimePartyDungeonMemberForTownReturn).
// Live 2026-09-20T11:05:44-47Z, raid 1507329: the phase-one force return logged
//
//	game-party-dungeon-member-town-return-detached party_id=15 char_id=10 survivor_count=1
//	game-party-dungeon-member-town-return-detached party_id=15 char_id=9  survivor_count=0
//	game-raid-phase-groups-force-returned raid_phase=1 returned_sessions=3
//
// i.e. the squad party was torn apart by the return the operator expects to keep it.

// squadReturnFixture puts a real two-member ordinary party inside two raid gates of a
// live phase-one raid, with both members standing in the raid's waiting instance.
func squadReturnFixture(t *testing.T) (runtimeRaidPhaseForceReturnFixture, *gameSession, *gameSession) {
	t.Helper()
	fixture := newRuntimeRaidPhaseForceReturnFixture(t)
	leader, ok := fixture.service.onlineGameSession(99)
	if !ok || leader == nil {
		t.Fatal("leader has no session")
	}
	member, ok := fixture.service.onlineGameSession(100)
	if !ok || member == nil {
		t.Fatal("member has no session")
	}
	if _, result, joined := fixture.service.createOrJoinManagedRuntimeParty(member, leader); !joined || !result.OK {
		t.Fatalf("squad party join=%t result=%+v", joined, result)
	}
	if state := runtimePartyStateSnapshot(leader); len(runtimePartyMembers(state)) != 2 {
		t.Fatalf("fixture party has %d members, want 2: %+v", len(runtimePartyMembers(state)), state)
	}
	fixture.enterGateRun(t, 99, 21, antonBlackFogSourceMapID)
	fixture.enterGateRun(t, 100, 22, antonBlackFogSourceMapID)
	return fixture, leader, member
}

// TestRaidPhaseForceReturnKeepsTheSquadParty is the phase-end half of the rule: every
// group still inside a gate goes home, and the squad stays grouped.
func TestRaidPhaseForceReturnKeepsTheSquadParty(t *testing.T) {
	fixture, leader, member := squadReturnFixture(t)

	if returned := fixture.service.forceReturnRuntimeRaidPhaseGroups(
		fixture.raidKey,
		runtimeRaidDungeonPhaseOne,
		"test_phase_complete_keeps_party",
	); returned != 2 {
		t.Fatalf("force return returned %d sessions, want 2", returned)
	}
	for id, session := range map[uint16]*gameSession{99: leader, 100: member} {
		if kept := fixture.sessionRuntime(t, id); kept != nil {
			t.Fatalf("char %d kept its run attached after the forced return (dungeon %d)", id, kept.Dungeon.ID)
		}
		state := runtimePartyStateSnapshot(session)
		if state.PartyID <= 0 || len(runtimePartyMembers(state)) != 2 {
			t.Fatalf("char %d came home out of its party: %+v", id, state)
		}
	}
	identity, bound := fixture.service.boundGameSessionCharacterSnapshot(leader)
	if !bound {
		t.Fatal("the leader lost its session binding")
	}
	if snapshot, found := fixture.service.runtimePartyManagerForService().
		SnapshotByUser(identity.character, identity.generation); !found || len(snapshot.Members) != 2 {
		t.Fatalf("the central party lost the squad: found=%t snapshot=%+v", found, snapshot)
	}
}

// TestWarTableReturnTakesTheWholeSquadHome is the 战况表 half: the squad leader's own
// 回城 click pulls the whole squad out of the run, and the squad survives it.
func TestWarTableReturnTakesTheWholeSquadHome(t *testing.T) {
	fixture, leader, member := squadReturnFixture(t)
	leader.conn.(*bufferConn).write.Reset()
	member.conn.(*bufferConn).write.Reset()

	// {1, 2} is the current EXE's 返回城镇 click (dungeon_card_flow.go:663-679); the
	// leader's own client is the only one the handler answers for a party run.
	if err := fixture.service.handleDungeonEplpCommand(leader, []byte{1, 2}); err != nil {
		t.Fatalf("war table return: %v", err)
	}
	ack, _ := splitGameServerUpperPacket(t, leader.conn.(*bufferConn).write.Bytes())
	if ack.Header.MsgID != uint16(dnfenum.CmdPacketEplpCommand) ||
		len(ack.Body) != 3 || ack.Body[0] != 1 || ack.Body[1] != 1 || ack.Body[2] != 2 {
		t.Fatalf("war table return ack msg=%d body=% X, want the {1,2} echo", ack.Header.MsgID, ack.Body)
	}
	// The exact frames this route sends, so the single-mode sibling can prove it
	// sends the same necessary ones without a squad (see
	// TestSingleModeWarTableReturnWithoutAnySquad).
	squadPackets := make([]uint16, 0, 4)
	for data := leader.conn.(*bufferConn).write.Bytes(); len(data) > 0; {
		packet, rest := splitGameServerUpperPacket(t, data)
		squadPackets = append(squadPackets, packet.Header.MsgID)
		data = rest
	}
	t.Logf("squad op72 {1,2} -> packets=%v", squadPackets)
	for _, id := range []uint16{99, 100} {
		if kept := fixture.sessionRuntime(t, id); kept != nil {
			t.Fatalf("char %d stayed inside the gate (dungeon %d) after the leader's 回城",
				id, kept.Dungeon.ID)
		}
	}
	leaderParty := runtimePartyStateSnapshot(leader)
	memberParty := runtimePartyStateSnapshot(member)
	if leaderParty.PartyID <= 0 || len(runtimePartyMembers(leaderParty)) != 2 {
		t.Fatalf("the leader's party did not survive the whole-squad return: %+v", leaderParty)
	}
	if memberParty.PartyID != leaderParty.PartyID || len(runtimePartyMembers(memberParty)) != 2 {
		t.Fatalf("the member's party did not survive the whole-squad return: %+v", memberParty)
	}
}

// TestWarTableReturnWorksWithoutAnySquad is the single-mode half of the 战况表 rule.
//
// 服主 2026-09-29（单人卢克实测）：「9999是单人 没队伍编制 刷暗祭坛入口 战况表的返回城镇
// 没反应 应该是没队伍编制不给用 正常应该给用的」。
//
// 根因：`leadRuntimePartyHomeFromActiveRun` 要求有一支小队
// （`runtimePartyMembers(party)` 为空就返回 false），而单人攻坚**没有**小队编制
// （`TailFlag == 1`），于是调用方落到 `settlement_result_not_ready` 的静默拒绝里 ——
// 客户端一帧都收不到。修法是把"没有小队"当成"一支只有自己的小队"。
func TestWarTableReturnWorksWithoutAnySquad(t *testing.T) {
	fixture := newRuntimeRaidPhaseForceReturnFixture(t)
	leader, ok := fixture.service.onlineGameSession(99)
	if !ok || leader == nil {
		t.Fatal("leader has no session")
	}
	if state := runtimePartyStateSnapshot(leader); state.PartyID != 0 || len(runtimePartyMembers(state)) != 0 {
		t.Fatalf("this test needs a member with no party at all: %+v", state)
	}
	fixture.enterGateRun(t, 99, 21, antonBlackFogSourceMapID)
	leader.conn.(*bufferConn).write.Reset()

	if err := fixture.service.handleDungeonEplpCommand(leader, []byte{1, 2}); err != nil {
		t.Fatalf("war table return without a squad: %v", err)
	}
	ack, _ := splitGameServerUpperPacket(t, leader.conn.(*bufferConn).write.Bytes())
	if ack.Header.MsgID != uint16(dnfenum.CmdPacketEplpCommand) ||
		len(ack.Body) != 3 || ack.Body[0] != 1 || ack.Body[1] != 1 || ack.Body[2] != 2 {
		t.Fatalf("war table return ack msg=%d body=% X, want the {1,2} echo", ack.Header.MsgID, ack.Body)
	}
	if kept := fixture.sessionRuntime(t, 99); kept != nil {
		t.Fatalf("char 99 stayed inside the gate (dungeon %d) after its 回城", kept.Dungeon.ID)
	}
	if key := fixture.service.runtimeRaidKeyForMember(99); key != fixture.raidKey {
		t.Fatalf("char 99 lost its raid membership: %d want %d", key, fixture.raidKey)
	}
}

// TestSingleModeWarTableReturnWithoutAnySquad drives the operator's case in its
// exact shape: **单人攻坚（TailFlag == 1）**, a character with no party at all,
// already standing inside a raid gate, and the client's own op72 `{1,2}`
// (战况表 返回城镇) arriving on the production dispatcher.
//
// 「如果没队伍 单人 进图 战况表也要能回城」. The sibling above
// (TestWarTableReturnWorksWithoutAnySquad) covers the same no-party shape on a
// team raid; this one turns the tail flag on so the scenario is literally 单人.
//
// It also counts the packets: the ack and the town-return packet both have to go
// out, i.e. "no squad" must not silently drop either of them.
func TestSingleModeWarTableReturnWithoutAnySquad(t *testing.T) {
	fixture := newRuntimeRaidPhaseForceReturnFixture(t)
	fixture.service.raidMu.Lock()
	state := fixture.service.raids[fixture.raidKey]
	if state == nil {
		fixture.service.raidMu.Unlock()
		t.Fatal("the fixture raid disappeared")
	}
	// TailFlag == 1 is what "单人" means everywhere else in this package
	// (raid_anton_phase_one.go:175, raid_anton_phase_two.go:192, ...).
	state.TailFlag = 1
	fixture.service.raidMu.Unlock()

	leader, ok := fixture.service.onlineGameSession(99)
	if !ok || leader == nil {
		t.Fatal("leader has no session")
	}
	if state := runtimePartyStateSnapshot(leader); state.PartyID != 0 || len(runtimePartyMembers(state)) != 0 {
		t.Fatalf("this test needs a member with no party at all: %+v", state)
	}
	run := fixture.enterGateRun(t, 99, 21, antonBlackFogSourceMapID)
	if !fixture.service.currentDungeonRuntimeIsRaidGate(run) {
		t.Fatalf("char 99 is not standing in a raid gate: %+v", run.raidDungeon)
	}
	fixture.service.raidMu.Lock()
	tailFlag := fixture.service.raids[fixture.raidKey].TailFlag
	fixture.service.raidMu.Unlock()
	if tailFlag != 1 {
		t.Fatalf("the fixture raid is not single mode: TailFlag=%d", tailFlag)
	}
	leader.conn.(*bufferConn).write.Reset()

	if err := fixture.service.handleDungeonEplpCommand(leader, []byte{1, 2}); err != nil {
		t.Fatalf("single-mode war table return: %v", err)
	}

	// Every packet the leader got, in order, so "nothing went missing" is checked
	// against the whole wire rather than against one guessed frame.
	data := leader.conn.(*bufferConn).write.Bytes()
	sent := make([]uint16, 0, 4)
	for len(data) > 0 {
		packet, rest := splitGameServerUpperPacket(t, data)
		sent = append(sent, packet.Header.MsgID)
		data = rest
	}
	t.Logf("single-mode op72 {1,2} -> packets=%v", sent)
	if len(sent) == 0 {
		t.Fatal("the single-mode 回城 was answered with no packet at all")
	}
	// 1) the 回执 the client waits for before it stops driving the exit itself.
	ack, _ := splitGameServerUpperPacket(t, leader.conn.(*bufferConn).write.Bytes())
	if ack.Header.MsgID != uint16(dnfenum.CmdPacketEplpCommand) ||
		len(ack.Body) != 3 || ack.Body[0] != 1 || ack.Body[1] != 1 || ack.Body[2] != 2 {
		t.Fatalf("war table return ack msg=%d body=% X, want the {1,2} echo", ack.Header.MsgID, ack.Body)
	}
	// 2) the 回城 frames themselves. The route is shared with the squad case, so
	// the single-mode run has to emit the same leader-local frames -- everything
	// the squad route sends **minus** 153 (0x99, the party-state projection: with
	// no squad there is no roster to project, which is correct, not a missing
	// packet). Measured, squad case first:
	//   squad       72 584 2 9 [153] 42 376 2 2 205 22 23 24 1369 2 9 [153] 23 2 9 153 23
	//   single mode 72 584 2 9       42 376 2 2 205 22 23 24 1369 2 9       23
	// The trailing 2 9 153 23 of the squad run is the second member's own return,
	// which has no counterpart with one member.
	wantFrames := []uint16{
		uint16(dnfenum.CmdPacketEplpCommand), // 回执 (the {1,2} echo the client waits for)
		raid.RaidParticipationMsgID,          // 584: the gate's participation row is dropped
		2, 9, 42, 376, 2, 2, 205, 22, 23, 24, 1369, 2, 9,
		23, // the town scene the client needs to be standing in again
	}
	if !reflect.DeepEqual(sent, wantFrames) {
		t.Fatalf("single-mode 回城 frames=%v want %v (any missing frame is a packet the "+
			"no-squad route failed to send)", sent, wantFrames)
	}
	// 3) 该次运行被退役 + 团籍保留.
	if kept := fixture.sessionRuntime(t, 99); kept != nil {
		t.Fatalf("char 99 stayed inside the gate (dungeon %d) after its 回城", kept.Dungeon.ID)
	}
	if key := fixture.service.runtimeRaidKeyForMember(99); key != fixture.raidKey {
		t.Fatalf("char 99 lost its raid membership: %d want %d", key, fixture.raidKey)
	}
}

// member's own click is answered by the party-dungeon owner path, never by leading the
// whole squad out, so the frame's return must stay the leader's decision.
func TestWarTableReturnRefusesANonLeader(t *testing.T) {
	fixture, leader, member := squadReturnFixture(t)
	member.conn.(*bufferConn).write.Reset()
	leader.conn.(*bufferConn).write.Reset()

	if err := fixture.service.handleDungeonEplpCommand(member, []byte{1, 2}); err != nil {
		t.Fatalf("member war table return: %v", err)
	}
	if kept := fixture.sessionRuntime(t, 99); kept == nil {
		t.Fatal("a squad member's click pulled the leader out of the gate")
	}
	if member.conn.(*bufferConn).write.Len() != 0 {
		t.Fatal("a squad member's war-table 回城 was answered with a whole-squad exit")
	}
}
