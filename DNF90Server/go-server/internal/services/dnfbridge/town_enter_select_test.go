package dnfbridge

import (
	"bytes"
	"context"
	"reflect"
	"testing"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfproto "longheng.io/server/internal/modules/dnf/protocol"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	"longheng.io/server/internal/modules/dnf/worldmap"
)

func TestTownEnterSelectOp15SendsCurrentOp27AfterAckAndFatigue(t *testing.T) {
	service, session, repositories := newTownMoveTest(t)
	if err := service.handleTownSetUserArea(session, buildTownMoveRequest(38, 0, 900, 250, 5)); err != nil {
		t.Fatal(err)
	}
	if err := service.handleTownSetUserPosition(session, buildTownPositionRequest(884, 248, 6, 100)); err != nil {
		t.Fatal(err)
	}
	connection := session.conn.(*bufferConn)
	connection.write.Reset()

	if err := service.sendEnterSelectDungeonState(session, "test_town_op15", false, true); err != nil {
		t.Fatal(err)
	}
	ack, rest := splitGameServerUpperPacket(t, connection.write.Bytes())
	if ack.Header.Classification != dnfproto.DefaultChannelClassification || ack.Header.MsgID != uint16(dnfenum.CmdPacketEnterSelectDungeon) || !bytes.Equal(ack.Body, []byte{1, 1, 0}) {
		t.Fatalf("op15 ack header=%+v body=%x rest=%x", ack.Header, ack.Body, rest)
	}
	fatigue, rest := splitGameServerUpperPacket(t, rest)
	if fatigue.Header.Classification != 0 || fatigue.Header.MsgID != currentFatigueMsgID || len(fatigue.Body) != 10 {
		t.Fatalf("fatigue header=%+v body=%x rest=%x", fatigue.Header, fatigue.Body, rest)
	}
	skills, rest := splitGameServerUpperPacket(t, rest)
	if skills.Header.Classification != 0 || skills.Header.MsgID != currentSkillInfoMsgID ||
		!bytes.Equal(skills.Body, townMoveSkillProjectionBody(t, repositories, "29")) {
		t.Fatalf("skills header=%+v body=%x rest=%x", skills.Header, skills.Body, rest)
	}
	contextPacket, trailing := splitGameServerUpperPacket(t, rest)
	if contextPacket.Header.Classification != 0 || contextPacket.Header.MsgID != currentDungeonContextMsgID || len(contextPacket.Body) != 37 || len(trailing) != 0 {
		t.Fatalf("op27 header=%+v body=%x trailing=%x", contextPacket.Header, contextPacket.Body, trailing)
	}
	if !session.enterSelectDungeonContextSent {
		t.Fatal("town enter-select op27 was not recorded")
	}
	if !session.townSelectorOriginBound ||
		session.townSelectorOriginSnapshot.CharacterID != 29 ||
		session.townSelectorOriginSnapshot.TownID != 38 ||
		session.townSelectorOriginSnapshot.AreaID != 0 ||
		session.townSelectorOriginSnapshot.PositionX != 884 ||
		session.townSelectorOriginSnapshot.PositionY != 248 ||
		!session.townSelectorOriginSnapshot.PositionValid {
		t.Fatalf("selector origin=%+v bound=%t", session.townSelectorOriginSnapshot, session.townSelectorOriginBound)
	}
}

func TestPartyDungeonSelectorIsLeaderOwnedAndSynchronizesPassiveMemberWithoutOp15Ack(t *testing.T) {
	service, leader, repositories := newTownMoveTest(t)
	service.onlinePlayers = newOnlinePlayerManager()
	if err := repositories.Character.Save(context.Background(), dnfrepo.CharacterRecord{
		CharacterID: "17",
		AccountID:   "account-1",
		Name:        "follower",
		Job:         "0",
		Level:       20,
		Stats: map[string]int64{
			"town_id": 38, "area_id": 0, "pos_x": 870, "pos_y": 248,
			"direction": 6, "area_state": 3, "fatigue": 20,
		},
	}); err != nil {
		t.Fatal(err)
	}
	enableTownMoveSkillProjection(t, service, repositories, "17")
	followerConn := &bufferConn{}
	follower := &gameSession{
		conn:                            followerConn,
		channel:                         leader.channel,
		residentChannel:                 leader.residentChannel,
		connectionTownActorOwnerChannel: leader.connectionTownActorOwnerChannel,
		townActorOwnerChannel:           leader.townActorOwnerChannel,
		selectedCharacterID:             17,
		townSceneReadyCharacterID:       17,
	}
	service.bindGameSessionCharacter(follower, 17)

	if err := service.handleTownSetUserArea(leader, buildTownMoveRequest(38, 0, 900, 250, 5)); err != nil {
		t.Fatal(err)
	}
	if err := service.handleTownSetUserPosition(leader, buildTownPositionRequest(884, 248, 6, 100)); err != nil {
		t.Fatal(err)
	}
	if err := service.handleTownSetUserArea(follower, buildTownMoveRequest(38, 0, 870, 248, 6)); err != nil {
		t.Fatal(err)
	}
	if err := service.handleTownSetUserPosition(follower, buildTownPositionRequest(870, 248, 6, 100)); err != nil {
		t.Fatal(err)
	}
	partyState := alignedcmd.PartyState{
		PartyID: 29,
		UserID:  29,
		Members: []alignedcmd.PartyMemberState{
			{UserID: 29, UserState: 1, HPPercent: 100, MPPercent: 100},
			{UserID: 17, UserState: 1, HPPercent: 100, MPPercent: 100},
		},
	}
	storeRuntimePartyState(leader, partyState)
	storeRuntimePartyState(follower, partyState)
	if !service.onlinePlayers.PeerInSameArea(29, 17) {
		t.Fatalf("party fixture members are not in the same area: leader_session=%p follower_session=%p",
			service.onlinePlayers.SessionForCharacter(29),
			service.onlinePlayers.SessionForCharacter(17))
	}
	if onlineFollower, ok := service.onlineGameSession(17); !ok || onlineFollower != follower {
		t.Fatalf("party fixture follower is not online: ok=%t session=%p want=%p", ok, onlineFollower, follower)
	}
	if ready, reason := service.currentTownEnterSelectReady(follower); !ready {
		t.Fatalf("party fixture follower selector not ready: %s snapshot=%+v", reason, follower.townPositionSnapshot)
	}
	leader.conn.(*bufferConn).write.Reset()
	followerConn.write.Reset()

	// Character 17 is an ordinary member. Same-version party selection is owned
	// by slot-zero: its accidental op15 is consumed and cannot move the party.
	if err := service.sendEnterSelectDungeonState(follower, "test_party_member_op15", false, true); err != nil {
		t.Fatal(err)
	}
	if followerConn.write.Len() != 0 || leader.conn.(*bufferConn).write.Len() != 0 {
		t.Fatalf("non-leader op15 emitted follower=%x leader=%x", followerConn.write.Bytes(), leader.conn.(*bufferConn).write.Bytes())
	}

	// The leader's op15 keeps its own ACK and fans the passive sequence to the
	// follower without a fabricated class1/op15 response or op9 roster replay.
	if err := service.sendEnterSelectDungeonState(leader, "test_party_leader_op15", false, true); err != nil {
		t.Fatal(err)
	}
	stream := followerConn.write.Bytes()
	fatigue, stream := splitGameServerUpperPacket(t, stream)
	if fatigue.Header.Classification != 0 || fatigue.Header.MsgID != currentFatigueMsgID {
		t.Fatalf("follower selector fatigue header=%+v body=%x rest=%x", fatigue.Header, fatigue.Body, stream)
	}
	contextPacket, trailing := splitGameServerUpperPacket(t, stream)
	if contextPacket.Header.Classification != 0 || contextPacket.Header.MsgID != currentDungeonContextMsgID || len(trailing) != 0 {
		t.Fatalf("follower selector context header=%+v body=%x trailing=%x", contextPacket.Header, contextPacket.Body, trailing)
	}
	for _, packet := range splitAllCurrentGameServerUpperPackets(t, followerConn.write.Bytes()) {
		if packet.Header.MsgID == uint16(dnfenum.CmdPacketEnterSelectDungeon) ||
			packet.Header.MsgID == uint16(dnfenum.CmdPacketRecoverStamina) {
			t.Fatalf("passive follower received forbidden op%d body=%x", packet.Header.MsgID, packet.Body)
		}
	}
	if !leader.enterSelectDungeonAckSent || !leader.enterSelectDungeonContextSent ||
		!follower.enterSelectDungeonContextSent || follower.enterSelectDungeonAckSent ||
		!follower.enterSelectDungeonPassiveParty {
		t.Fatalf("selector flags leader_ack=%t leader_context=%t follower_context=%t follower_ack=%t follower_passive=%t",
			leader.enterSelectDungeonAckSent, leader.enterSelectDungeonContextSent,
			follower.enterSelectDungeonContextSent, follower.enterSelectDungeonAckSent,
			follower.enterSelectDungeonPassiveParty)
	}
}

func TestTownTransportSceneFinalizerOp15IsAcknowledgedWithoutPrematureContext(t *testing.T) {
	service, session, _ := newTownMoveTest(t)
	session.townTransportEnterSelectPending = true
	connection := session.conn.(*bufferConn)
	connection.write.Reset()

	if err := service.sendEnterSelectDungeonState(session, "test_town_transport_scene_finalizer", false, true); err != nil {
		t.Fatal(err)
	}
	if connection.write.Len() == 0 || session.townTransportEnterSelectPending ||
		!session.enterSelectDungeonSent || !session.enterSelectDungeonAckSent || session.enterSelectDungeonContextSent {
		t.Fatalf("town transport finalizer wrote=%x pending=%t selector_sent=%t ack_sent=%t context_sent=%t",
			connection.write.Bytes(), session.townTransportEnterSelectPending,
			session.enterSelectDungeonSent, session.enterSelectDungeonAckSent, session.enterSelectDungeonContextSent)
	}
}

func TestTownTransportFirstOp15UsesArrivalPositionForSelectorContext(t *testing.T) {
	service, session, _ := newTownMoveTest(t)
	session.townTransportEnterSelectPending = true
	session.townPositionSnapshot = currentTownPositionSnapshot{
		CharacterID:   session.selectedCharacterID,
		TownID:        38,
		AreaID:        0,
		PositionX:     916,
		PositionY:     224,
		MovementCode:  5,
		PositionValid: true,
	}
	if err := service.sendEnterSelectDungeonState(session, "test_town_transport_first_gate_click", false, true); err != nil {
		t.Fatal(err)
	}
	if !session.enterSelectDungeonAckSent || !session.enterSelectDungeonContextSent ||
		session.townTransportEnterSelectPending {
		t.Fatalf("ack=%t context=%t pending=%t", session.enterSelectDungeonAckSent, session.enterSelectDungeonContextSent, session.townTransportEnterSelectPending)
	}
	packets := splitAllCurrentGameServerUpperPackets(t, session.conn.(*bufferConn).write.Bytes())
	seen := map[uint16]bool{}
	for _, packet := range packets {
		seen[packet.Header.MsgID] = true
	}
	for _, msgID := range []uint16{uint16(dnfenum.CmdPacketEnterSelectDungeon), currentFatigueMsgID, currentSkillInfoMsgID, currentDungeonContextMsgID} {
		if !seen[msgID] {
			t.Fatalf("first transport op15 missing msg=%d packets=%v", msgID, packets)
		}
	}
}

func TestTownEnterSelectOp15DoesNotSendOp27BeforeTownReady(t *testing.T) {
	service, session, _ := newTownMoveTest(t)
	if err := service.sendEnterSelectDungeonState(session, "test_unready_op15", false, true); err != nil {
		t.Fatal(err)
	}
	_, rest := splitGameServerUpperPacket(t, session.conn.(*bufferConn).write.Bytes())
	_, trailing := splitGameServerUpperPacket(t, rest)
	if len(trailing) != 0 || session.enterSelectDungeonContextSent {
		t.Fatalf("unready town op15 emitted trailing=%x context_sent=%t", trailing, session.enterSelectDungeonContextSent)
	}
}

func TestTownEnterSelectOp15DoesNotSendOp27WithoutTownLocationOwner(t *testing.T) {
	service, session, _ := newTownMoveTest(t)
	service.markCurrentTownSceneReady(session)
	if err := service.sendEnterSelectDungeonState(session, "test_missing_location_op15", false, true); err != nil {
		t.Fatal(err)
	}
	_, rest := splitGameServerUpperPacket(t, session.conn.(*bufferConn).write.Bytes())
	_, trailing := splitGameServerUpperPacket(t, rest)
	if len(trailing) != 0 || session.enterSelectDungeonContextSent || session.townSelectorOriginBound {
		t.Fatalf("missing-location op15 emitted trailing=%x context_sent=%t origin_bound=%t", trailing, session.enterSelectDungeonContextSent, session.townSelectorOriginBound)
	}
}

func TestTownEnterSelectOp15DoesNotSendOp27WithoutCurrentOp35Position(t *testing.T) {
	service, session, _ := newTownMoveTest(t)
	if err := service.handleTownSetUserArea(session, buildTownMoveRequest(38, 0, 900, 250, 5)); err != nil {
		t.Fatal(err)
	}
	connection := session.conn.(*bufferConn)
	connection.write.Reset()

	if err := service.sendEnterSelectDungeonState(session, "test_missing_position_op15", false, true); err != nil {
		t.Fatal(err)
	}
	_, rest := splitGameServerUpperPacket(t, connection.write.Bytes())
	_, trailing := splitGameServerUpperPacket(t, rest)
	if len(trailing) != 0 || session.enterSelectDungeonContextSent || session.townSelectorOriginBound {
		t.Fatalf(
			"missing-position op15 emitted trailing=%x context_sent=%t origin_bound=%t",
			trailing,
			session.enterSelectDungeonContextSent,
			session.townSelectorOriginBound,
		)
	}
}

func TestTownEnterSelectOp15DoesNotSendOp27WithActiveDungeonRuntime(t *testing.T) {
	service, session, _ := newTownMoveTest(t)
	service.markCurrentTownSceneReady(session)
	session.dungeon.runtime = &runtimeDungeonState{}
	if err := service.sendEnterSelectDungeonState(session, "test_active_runtime_op15", false, true); err != nil {
		t.Fatal(err)
	}
	_, rest := splitGameServerUpperPacket(t, session.conn.(*bufferConn).write.Bytes())
	_, trailing := splitGameServerUpperPacket(t, rest)
	if len(trailing) != 0 || session.enterSelectDungeonContextSent {
		t.Fatalf("active-runtime op15 emitted trailing=%x context_sent=%t", trailing, session.enterSelectDungeonContextSent)
	}
}

func TestTownEnterSelectOp15AfterBackToVillageOpensSelector(t *testing.T) {
	service, session, runtime := newBackToVillageRuntime(t)
	if err := service.handleDungeonBackToVillage(session, nil); err != nil {
		t.Fatal(err)
	}
	if !runtime.townReturnPending || !runtime.townReturnOp24Sent {
		t.Fatalf("return did not complete its full route: %+v", runtime)
	}
	connection := session.conn.(*bufferConn)
	if got := townPostTransitionPacketSignatures(t, connection.write.Bytes()); !reflect.DeepEqual(
		got,
		[]string{"op42", "op376", "mode0", "mode1", "op205", "op24", "op559", "op33weakness"},
	) {
		t.Fatalf("full return route=%v", got)
	}
	if session.dungeon.runtime != nil {
		t.Fatalf("full return retained runtime=%+v", session.dungeon.runtime)
	}
	connection.write.Reset()

	if err := service.sendEnterSelectDungeonState(session, "test_op15_after_pending_town_return", false, true); err != nil {
		t.Fatal(err)
	}

	ack, rest := splitGameServerUpperPacket(t, connection.write.Bytes())
	if ack.Header.Classification != dnfproto.DefaultChannelClassification ||
		ack.Header.MsgID != uint16(dnfenum.CmdPacketEnterSelectDungeon) ||
		!bytes.Equal(ack.Body, []byte{1, 1, 0}) {
		t.Fatalf("op15 ack header=%+v body=%x rest=%x", ack.Header, ack.Body, rest)
	}
	fatigue, rest := splitGameServerUpperPacket(t, rest)
	if fatigue.Header.Classification != 0 || fatigue.Header.MsgID != currentFatigueMsgID {
		t.Fatalf("op15 fatigue header=%+v body=%x rest=%x", fatigue.Header, fatigue.Body, rest)
	}
	skills, rest := splitGameServerUpperPacket(t, rest)
	if skills.Header.Classification != 0 || skills.Header.MsgID != currentSkillInfoMsgID {
		t.Fatalf("op15 skills header=%+v body=%x rest=%x", skills.Header, skills.Body, rest)
	}
	contextPacket, trailing := splitGameServerUpperPacket(t, rest)
	if contextPacket.Header.Classification != 0 || contextPacket.Header.MsgID != currentDungeonContextMsgID ||
		len(contextPacket.Body) != 37 || len(trailing) != 0 {
		t.Fatalf("op15 context header=%+v body=%x trailing=%x", contextPacket.Header, contextPacket.Body, trailing)
	}
	if session.dungeon.runtime != nil || !session.enterSelectDungeonContextSent {
		t.Fatalf("first post-return op15 did not open selector runtime=%p context=%t", session.dungeon.runtime, session.enterSelectDungeonContextSent)
	}
	if status := runtime.Session.Snapshot().Run.Status; status != worldmap.DungeonRunAbandoned {
		t.Fatalf("pending active return status=%s want abandoned", status)
	}
	if session.backToVillageEnterSelectPending || session.backToVillageSelectorDuplicateGuard {
		t.Fatalf("full return retained legacy two-stage flags pending=%t duplicate_guard=%t",
			session.backToVillageEnterSelectPending, session.backToVillageSelectorDuplicateGuard)
	}
	if snapshot := session.townPositionSnapshot; snapshot.CharacterID != session.selectedCharacterID ||
		snapshot.TownID != 7 || snapshot.AreaID != 3 || snapshot.PositionX != 474 ||
		snapshot.PositionY != 234 || !snapshot.PositionValid {
		t.Fatalf("post-return town snapshot=%+v", snapshot)
	}
}

func TestCompletedSettlementOp15RetiresRuntimeAndOpensOtherDungeonSelector(t *testing.T) {
	service, runtime, session, connection, _ := prepareCompletedSettlementRuntime(t)
	repositories, ok := service.repositoryGroup()
	if !ok {
		t.Fatal("completed settlement skill repository unavailable")
	}
	enableTownMoveSkillProjection(t, service, repositories, "99")
	connection.write.Reset()
	plan := currentDungeonSettlementPacketPlanForTest(t, runtime, 0)
	runtime.settlementResultPlan = &plan
	if err := service.handleDungeonSetPlayResult(session, make([]byte, currentDungeonPlayResultBaseSize)); err != nil {
		t.Fatal(err)
	}
	if err := service.handleDungeonCharacterStatistic(session, make([]byte, currentDungeonCharacterStatisticBodySize)); err != nil {
		t.Fatal(err)
	}
	connection.write.Reset()
	if err := service.sendEnterSelectDungeonState(session, "test_completed_other_dungeon", false, true); err != nil {
		t.Fatal(err)
	}

	ack, rest := splitGameServerUpperPacket(t, connection.write.Bytes())
	if ack.Header.Classification != dnfproto.DefaultChannelClassification ||
		ack.Header.MsgID != uint16(dnfenum.CmdPacketEnterSelectDungeon) ||
		!bytes.Equal(ack.Body, []byte{1, 1, 0}) {
		t.Fatalf("completed op15 ack=%+v body=%x rest=%x", ack.Header, ack.Body, rest)
	}
	fatigue, rest := splitGameServerUpperPacket(t, rest)
	if fatigue.Header.Classification != 0 || fatigue.Header.MsgID != currentFatigueMsgID {
		t.Fatalf("completed op15 fatigue=%+v body=%x rest=%x", fatigue.Header, fatigue.Body, rest)
	}
	skills, rest := splitGameServerUpperPacket(t, rest)
	if skills.Header.Classification != 0 || skills.Header.MsgID != currentSkillInfoMsgID ||
		!bytes.Equal(skills.Body, backToVillageSkillProjectionBody(t, service)) {
		t.Fatalf("completed op15 skills=%+v body=%x rest=%x", skills.Header, skills.Body, rest)
	}
	contextPacket, trailing := splitGameServerUpperPacket(t, rest)
	if contextPacket.Header.Classification != 0 || contextPacket.Header.MsgID != currentDungeonContextMsgID ||
		len(contextPacket.Body) != 37 || len(trailing) != 0 {
		t.Fatalf("completed op15 context=%+v body=%x trailing=%x", contextPacket.Header, contextPacket.Body, trailing)
	}
	if session.dungeon.runtime != nil || !session.enterSelectDungeonContextSent ||
		session.townSceneReadyCharacterID != session.selectedCharacterID ||
		session.townPositionSnapshot.CharacterID != session.selectedCharacterID ||
		!session.townPositionSnapshot.PositionValid ||
		runtime.Session.Snapshot().Run.Status != worldmap.DungeonRunCompleted {
		t.Fatalf("completed op15 runtime=%p context=%t ready=%d position=%+v run=%+v",
			session.dungeon.runtime,
			session.enterSelectDungeonContextSent,
			session.townSceneReadyCharacterID,
			session.townPositionSnapshot,
			runtime.Session.Snapshot().Run)
	}
}
