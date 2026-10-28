package dnfbridge

import (
	"bytes"
	"encoding/binary"
	"errors"
	"reflect"
	"testing"

	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfdungeonadmission "longheng.io/server/internal/modules/dnf/dungeonadmission"
	dnfproto "longheng.io/server/internal/modules/dnf/protocol"
	"longheng.io/server/internal/modules/dnf/worldmap"
)

func TestCompletedOrdinaryDungeonStatisticRemainsRecordOnlyWithTowerCatalog(t *testing.T) {
	service, runtime, session, conn := prepareOrdinaryCompletedSettlementRuntimeForExitTest(t)
	catalog, err := dnfdungeonadmission.LoadCatalog(dungeonAdmissionBridgeCatalogSource())
	if err != nil {
		t.Fatal(err)
	}
	service.dungeonAdmissionCatalog = catalog
	runtime.Dungeon.ID = 5006
	runtime.Request.DungeonID = 5006
	plan := currentDungeonSettlementPacketPlanForTest(t, runtime, 0)
	runtime.settlementResultPlan = &plan
	if err := service.handleDungeonSetPlayResult(
		session,
		make([]byte, currentDungeonPlayResultBaseSize),
	); err != nil {
		t.Fatal(err)
	}
	conn.write.Reset()
	if err := service.handleDungeonCharacterStatistic(
		session,
		make([]byte, currentDungeonCharacterStatisticBodySize),
	); err != nil {
		t.Fatal(err)
	}
	if conn.write.Len() != 0 || !runtime.settlementStatisticReceived ||
		runtime.townReturnPending || session.dungeon.runtime != runtime {
		t.Fatalf("ordinary statistic changed exit state=%+v owner=%p wrote=%x",
			runtime, session.dungeon.runtime, conn.write.Bytes())
	}
}

func TestCompletedSettlementCharacterStatisticCapturesSixU32WithoutResponse(t *testing.T) {
	service, runtime, session, conn, _ := prepareCompletedSettlementRuntime(t)
	conn.write.Reset()
	body := make([]byte, currentDungeonCharacterStatisticBodySize)
	for index := 0; index < 6; index++ {
		binary.LittleEndian.PutUint32(body[index*4:index*4+4], uint32(index+10))
	}

	packet, err := dnfproto.BuildChannelPacket(
		uint16(dnfenum.CmdPacketCharacterStatistic), body, 0, dnfproto.DefaultChannelClassification,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.handleGameUpper(session, packet); err != nil {
		t.Fatal(err)
	}
	if conn.write.Len() != 0 || !runtime.settlementStatisticReceived ||
		!bytes.Equal(runtime.settlementStatisticBody, body) {
		t.Fatalf("op123 capture state=%+v wrote=%x", runtime, conn.write.Bytes())
	}

	if err := service.handleDungeonCharacterStatistic(session, append([]byte(nil), body...)); err != nil {
		t.Fatal(err)
	}
	conflict := append([]byte(nil), body...)
	conflict[0]++
	if err := service.handleDungeonCharacterStatistic(session, conflict); err != nil {
		t.Fatal(err)
	}
	if conn.write.Len() != 0 || !bytes.Equal(runtime.settlementStatisticBody, body) {
		t.Fatalf("op123 replay changed capture=%x wrote=%x", runtime.settlementStatisticBody, conn.write.Bytes())
	}
}

func TestCompletedTutorialFirstOp42BuildsTownActorBeforeTypedTransition(t *testing.T) {
	service, runtime, session, conn, _ := prepareCompletedSettlementRuntime(t)
	conn.write.Reset()
	completeSettlementExitBarriersForTest(t, service, runtime, session)
	packet, err := dnfproto.BuildChannelPacket(
		uint16(dnfenum.CmdPacketGiveupGame), nil, 0, dnfproto.DefaultChannelClassification,
	)
	if err != nil {
		t.Fatal(err)
	}
	conn.write.Reset()
	if err := service.handleGameUpper(session, packet); err != nil {
		t.Fatal(err)
	}

	assertCompletedDungeonTownRouteForTest(t, conn.write.Bytes(), wantTutorialCompletionTownTransitionBody())
	if runtime.settlementCardLayoutSent || runtime.settlementCardSelectionSent {
		t.Fatalf("completed tutorial op42 incorrectly entered card flow state=%+v", runtime)
	}
	if !runtime.townReturnPending || !runtime.townReturnOp24Sent ||
		runtime.townReturnRequestMsgID != uint16(dnfenum.CmdPacketGiveupGame) ||
		runtime.Session.Snapshot().Run.Status != worldmap.DungeonRunCompleted || session.dungeon.runtime != nil ||
		session.initialTownRouteStage != currentInitialTownRoutePlayerStateSent ||
		!session.sceneBootstrapTailDeferred ||
		!session.selectedUserInfoRefreshSent ||
		session.selectedUserInfoMode3Sent {
		t.Fatalf("completed op42 state=%+v owner=%p", runtime, session.dungeon.runtime)
	}

	conn.write.Reset()
	if err := service.handleDungeonGiveupGame(session, nil); err != nil {
		t.Fatal(err)
	}
	if conn.write.Len() != 0 {
		t.Fatalf("op42 replay resent op24=%x", conn.write.Bytes())
	}
}

func TestCompletedOrdinaryDungeonOp42CannotBypassCardRequestChain(t *testing.T) {
	service, runtime, session, conn := prepareOrdinaryCompletedSettlementRuntimeForExitTest(t)
	conn.write.Reset()
	completeSettlementExitBarriersForTest(t, service, runtime, session)
	conn.write.Reset()

	if err := service.handleDungeonGiveupGame(session, nil); err != nil {
		t.Fatal(err)
	}
	if conn.write.Len() != 0 {
		t.Fatalf("ordinary pre-card op42 synthesized card/exit packets=%x", conn.write.Bytes())
	}
	if runtime.settlementCardLayoutSent || runtime.settlementCardSelectionSent ||
		runtime.townReturnPending || session.dungeon.runtime != runtime {
		t.Fatalf("ordinary completed op42 bypassed card gate state=%+v owner=%p", runtime, session.dungeon.runtime)
	}

	conn.write.Reset()
	if err := service.handleDungeonGiveupGame(session, nil); err != nil {
		t.Fatal(err)
	}
	if conn.write.Len() != 0 || runtime.settlementCardSelectionSent ||
		runtime.townReturnPending || session.dungeon.runtime != runtime {
		t.Fatalf("ordinary completed op42 did not wait for card selection state=%+v owner=%p wrote=%x",
			runtime, session.dungeon.runtime, conn.write.Bytes())
	}
}

func TestCompletedTutorialOp42WriteFailureRetainsRuntimeForRetry(t *testing.T) {
	service, runtime, session, _, _ := prepareCompletedSettlementRuntime(t)
	completeSettlementExitBarriersForTest(t, service, runtime, session)
	wantErr := errors.New("completed tutorial op24 write failed")
	connection := &failNthDungeonWriteConn{failAt: 4, err: wantErr}
	session.conn = connection

	err := service.handleDungeonGiveupGame(session, nil)
	if !errors.Is(err, wantErr) {
		t.Fatalf("completed tutorial op42 write failure=%v want=%v", err, wantErr)
	}
	if session.dungeon.runtime != runtime || !runtime.townReturnPending || runtime.townReturnOp24Sent {
		t.Fatalf("failed completed tutorial route changed ownership owner=%p state=%+v", session.dungeon.runtime, runtime)
	}
	if runtime.settlementCardLayoutSent || runtime.settlementCardSelectionSent {
		t.Fatalf("failed completed tutorial route incorrectly entered card flow state=%+v", runtime)
	}

	if err := service.handleDungeonGiveupGame(session, nil); err != nil {
		t.Fatalf("retry completed tutorial op42: %v", err)
	}
	assertCompletedDungeonTownRouteForTest(t, connection.write.Bytes(), wantTutorialCompletionTownTransitionBody())
	if session.dungeon.runtime != nil || !runtime.townReturnOp24Sent {
		t.Fatalf("successful completed tutorial retry did not detach owner=%p state=%+v", session.dungeon.runtime, runtime)
	}
}

func TestCompletedTutorialOp42WaitsForPostSettlementStatisticBarrier(t *testing.T) {
	service, runtime, session, conn, _ := prepareCompletedSettlementRuntime(t)
	conn.write.Reset()

	if err := service.handleDungeonGiveupGame(session, nil); err != nil {
		t.Fatal(err)
	}
	if conn.write.Len() != 0 || runtime.townReturnPending {
		t.Fatalf("pre-barrier op42 changed return state=%+v wrote=%x", runtime, conn.write.Bytes())
	}

	// A captured play-result request is not the final exit barrier. The current
	// EXE emits the exact six-u32 statistic after its settlement builder and
	// before the timed bodyless op42.
	plan := currentDungeonSettlementPacketPlanForTest(t, runtime, 0)
	runtime.settlementResultPlan = &plan
	if err := service.handleDungeonSetPlayResult(
		session,
		make([]byte, currentDungeonPlayResultBaseSize),
	); err != nil {
		t.Fatal(err)
	}
	conn.write.Reset()
	if err := service.handleDungeonGiveupGame(session, nil); err != nil {
		t.Fatal(err)
	}
	if conn.write.Len() != 0 || runtime.townReturnPending {
		t.Fatalf("op46-only op42 changed return state=%+v wrote=%x", runtime, conn.write.Bytes())
	}

	if err := service.handleDungeonCharacterStatistic(
		session,
		make([]byte, currentDungeonCharacterStatisticBodySize),
	); err != nil {
		t.Fatal(err)
	}
	conn.write.Reset()
	if err := service.handleDungeonGiveupGame(session, nil); err != nil {
		t.Fatal(err)
	}
	assertCompletedDungeonTownRouteForTest(t, conn.write.Bytes(), wantTutorialCompletionTownTransitionBody())
	if runtime.settlementCardLayoutSent || runtime.settlementCardSelectionSent ||
		!runtime.townReturnOp24Sent || session.dungeon.runtime != nil {
		t.Fatalf("post-barrier tutorial op42 did not commit direct town route state=%+v owner=%p",
			runtime, session.dungeon.runtime)
	}
}

func TestActiveRunOp42UsesFullTownLifecycleImmediately(t *testing.T) {
	service, session, runtime := newBackToVillageRuntime(t)
	if err := service.handleDungeonGiveupGame(session, nil); err != nil {
		t.Fatal(err)
	}
	if !runtime.townReturnPending || runtime.Session.Snapshot().Run.Status != worldmap.DungeonRunAbandoned ||
		session.dungeon.runtime != nil {
		t.Fatalf("active op42 state=%+v", runtime)
	}
	connection := session.conn.(*bufferConn)
	if got := townPostTransitionPacketSignatures(t, connection.write.Bytes()); !reflect.DeepEqual(
		got,
		[]string{"op42", "op376", "mode0", "mode1", "op205", "op24", "op559", "op33weakness"},
	) {
		t.Fatalf("active op42 full return route=%v", got)
	}
}

func TestCompletedSettlementOp16StartsNextChallengeWithFreshRuntime(t *testing.T) {
	service, completedRuntime, session, conn, _ := prepareCompletedSettlementRuntime(t)
	conn.write.Reset()
	body := make([]byte, 21)
	binary.LittleEndian.PutUint32(body[0:4], uint32(completedRuntime.Dungeon.ID))
	body[4] = 1
	if err := service.handleDungeonSelectUpper(session, body); err != nil {
		t.Fatal(err)
	}
	if conn.write.Len() == 0 || session.dungeon.runtime == nil ||
		session.dungeon.runtime == completedRuntime ||
		session.dungeon.runtime.Session.Snapshot().Run.Status != worldmap.DungeonRunActive {
		t.Fatalf("next challenge runtime old=%p current=%p wrote=%x",
			completedRuntime,
			session.dungeon.runtime,
			conn.write.Bytes())
	}
}

func TestSettlementExitRequestsRejectWrongShapeClassOrStage(t *testing.T) {
	tests := []struct {
		name           string
		msgID          uint16
		body           []byte
		classification byte
	}{
		{name: "op42 body", msgID: uint16(dnfenum.CmdPacketGiveupGame), body: []byte{0}, classification: dnfproto.DefaultChannelClassification},
		{name: "op42 class", msgID: uint16(dnfenum.CmdPacketGiveupGame), classification: 2},
		{name: "op123 body", msgID: uint16(dnfenum.CmdPacketCharacterStatistic), body: make([]byte, 23), classification: dnfproto.DefaultChannelClassification},
		{name: "op123 class", msgID: uint16(dnfenum.CmdPacketCharacterStatistic), body: make([]byte, 24), classification: 2},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			service, session, runtime := newBackToVillageRuntime(t)
			packet, err := dnfproto.BuildChannelPacket(test.msgID, test.body, 0, test.classification)
			if err != nil {
				t.Fatal(err)
			}
			if err := service.handleGameUpper(session, packet); err != nil {
				t.Fatal(err)
			}
			if session.conn.(*bufferConn).write.Len() != 0 || runtime.townReturnPending ||
				runtime.settlementStatisticReceived {
				t.Fatalf("rejected request changed state=%+v wrote=%x", runtime, session.conn.(*bufferConn).write.Bytes())
			}
		})
	}

	service, runtime := prepareTutorialScopeRuntime(t, tutorialScopeFixtureOptions{singleMonster: true})
	if err := runtime.Session.CompleteCurrentRoom(); err != nil {
		t.Fatal(err)
	}
	session := &gameSession{
		conn:                &bufferConn{},
		selectedCharacterID: 99,
		dungeon:             dungeonSessionState{runtime: runtime},
	}
	if err := service.handleDungeonGiveupGame(session, nil); err != nil {
		t.Fatal(err)
	}
	if err := service.handleDungeonCharacterStatistic(session, make([]byte, 24)); err != nil {
		t.Fatal(err)
	}
	if session.conn.(*bufferConn).write.Len() != 0 || runtime.townReturnPending || runtime.settlementStatisticReceived {
		t.Fatalf("completed pre-op31 requests changed state=%+v", runtime)
	}
}

func completeSettlementExitBarriersForTest(
	t *testing.T,
	service *Service,
	runtime *runtimeDungeonState,
	session *gameSession,
) {
	t.Helper()
	plan := currentDungeonSettlementPacketPlanForTest(t, runtime, 0)
	runtime.settlementResultPlan = &plan
	if err := service.handleDungeonSetPlayResult(
		session,
		make([]byte, currentDungeonPlayResultBaseSize),
	); err != nil {
		t.Fatal(err)
	}
	if err := service.handleDungeonCharacterStatistic(
		session,
		make([]byte, currentDungeonCharacterStatisticBodySize),
	); err != nil {
		t.Fatal(err)
	}
}

func prepareOrdinaryCompletedSettlementRuntimeForExitTest(
	t *testing.T,
) (*Service, *runtimeDungeonState, *gameSession, *bufferConn) {
	t.Helper()
	service, runtime := prepareTutorialScopeRuntime(t, tutorialScopeFixtureOptions{
		disableTutorial: true,
		singleMonster:   true,
	})
	conn := &bufferConn{}
	session := &gameSession{
		conn:                conn,
		connID:              "ordinary-completed-settlement-exit-test",
		selectedCharacterID: 99,
		dungeon:             dungeonSessionState{runtime: runtime},
		townSelectorOriginSnapshot: currentTownPositionSnapshot{
			CharacterID:   99,
			TownID:        7,
			AreaID:        3,
			PositionX:     474,
			PositionY:     234,
			PositionValid: true,
		},
		townSelectorOriginBound: true,
	}
	if err := freezeCurrentDungeonTownReturnOrigin(session, runtime); err != nil {
		t.Fatal(err)
	}
	completeSettlementRuntimeForTest(t, service, runtime, session)
	return service, runtime, session, conn
}

func assertCompletedDungeonTownRouteForTest(
	t *testing.T,
	data, wantTransition []byte,
) {
	t.Helper()
	actionTable, rest := splitGameServerUpperPacket(t, data)
	if actionTable.Header.MsgID != uint16(dnfenum.CmdPacketPVPMissionHpPercent) ||
		actionTable.Header.Classification != 0 {
		t.Fatalf("completed town route action table=%+v body=%x", actionTable.Header, actionTable.Body)
	}
	mode0, rest := splitGameServerUpperPacket(t, rest)
	if mode0.Header.MsgID != uint16(dnfenum.CmdPacketSetUDPIPPort) ||
		mode0.Header.Classification != 0 || len(mode0.Body) == 0 || mode0.Body[0] != 0 {
		t.Fatalf("completed town route mode0=%+v body=%x", mode0.Header, mode0.Body)
	}
	mode1, rest := splitGameServerUpperPacket(t, rest)
	if mode1.Header.MsgID != uint16(dnfenum.CmdPacketSetUDPIPPort) ||
		mode1.Header.Classification != 0 || len(mode1.Body) == 0 || mode1.Body[0] != 1 {
		t.Fatalf("completed town route mode1=%+v body=%x", mode1.Header, mode1.Body)
	}
	expertJob, rest := splitGameServerUpperPacket(t, rest)
	if expertJob.Header.MsgID != currentExpertJobInfoNotification || expertJob.Header.Classification != 0 ||
		!bytes.Equal(expertJob.Body, currentExpertJobClearedBody(0)) {
		t.Fatalf("completed town route state-only op205=%+v body=%x", expertJob.Header, expertJob.Body)
	}
	for {
		op24, next := splitGameServerUpperPacket(t, rest)
		if op24.Header.MsgID == currentSceneTransitionMsgID && op24.Header.Classification == 0 {
			if !bytes.Equal(op24.Body, wantTransition) {
				t.Fatalf("completed town route op24=%+v body=%x rest=%x", op24.Header, op24.Body, next)
			}
			buff, tail := splitGameServerUpperPacket(t, next)
			if buff.Header.MsgID != skillSwitchInventoryAllDataWire || buff.Header.Classification != 0 || len(buff.Body) < 3 {
				t.Fatalf("completed town route post-op24 BUFF registration=%+v body=%x tail=%x", buff.Header, buff.Body, tail)
			}
			tail = consumeCurrentDungeonConsumableGateResetForTest(t, tail)
			if len(tail) != 0 {
				t.Fatalf("completed town route trailing=%x", tail)
			}
			return
		}
		switch op24.Header.MsgID {
		case currentCreatureStateTableMsgID, currentAcceptableQuestListMsgID, currentActiveQuestSnapshotMsgID:
			// These generation-scoped projections follow the complete actor state.
		case currentTownUserPositionNotificationMsgID,
			currentTownUserAreaNotificationMsgID,
			currentClearQuestListMsgID:
			// op22/op23 现在在 mode1 与 actor 场景绑定成立后就发，不再压在库存/冒险/
			// 玩家状态引导之后（见 initial_town_route_packets.go 的注释：旧顺序会让
			// 原生 HUD 有约一秒仍停在上一个频道的位置）。这三个包的先后由专门的序列
			// 回归断言，这个 helper 只盯 actor / 场景门包 —— 与同包的
			// townPostTransitionPacketSignatures 对这三个 msgID 的处理保持一致。
		case currentSkillInfoMsgID:
			t.Fatalf("completed town route replayed op19 skill snapshot body=%x", op24.Body)
		default:
			t.Fatalf("completed town route unexpected pre-op24 packet=%+v body=%x", op24.Header, op24.Body)
		}
		rest = next
	}
}
