package dnfbridge

import (
	"bytes"
	"testing"

	"longheng.io/server/internal/modules/dnf/dnfenum"
	"longheng.io/server/internal/modules/dnf/dungeoncmd"
	dnfproto "longheng.io/server/internal/modules/dnf/protocol"
)

func TestStoryDungeonFreshConsumableStateOnFirstAdmission(t *testing.T) {
	service, runtime, session, connection := prepareStoryChainRuntime(t)
	requireQuestConnectedDungeonRuntime(t, runtime, 700)
	requireFreshDungeonFinishLoadingConsumableReset(t, service, session, connection)
}

func TestStoryDungeonFreshConsumableStateAfterMidRunTownReturn(t *testing.T) {
	service, runtime, session, connection := prepareStoryChainRuntime(t)
	requireQuestConnectedDungeonRuntime(t, runtime, 700)
	requireFreshDungeonFinishLoadingConsumableReset(t, service, session, connection)

	newRuntime := returnAndEnterDungeonForConsumableResetTest(
		t, service, session, connection, runtime, 700,
	)
	requireQuestConnectedDungeonRuntime(t, newRuntime, 700)
	requireFreshDungeonFinishLoadingConsumableReset(t, service, session, connection)
}

func TestStoryDungeonFreshConsumableStateAfterCompletedNextQuest(t *testing.T) {
	service, runtime, session, connection := prepareStoryChainCompletedSettlementRuntime(t)
	requireQuestConnectedDungeonRuntime(t, runtime, 700)
	// A real run consumes its first-entry reset before reaching settlement.
	requireFreshDungeonFinishLoadingConsumableReset(t, service, session, connection)
	runtime.settlementPlayResultReceived = true
	runtime.settlementStatisticReceived = true
	markCurrentDungeonCardRewardCommittedForTest(runtime)
	connection.write.Reset()

	if err := service.handleGameCommand(
		session,
		byte(dnfenum.GameCmdCommand),
		uint16(dnfenum.CmdPacketEplpCommand),
		[]byte{1, 0},
	); err != nil {
		t.Fatal(err)
	}
	newRuntime := session.dungeon.runtime
	if newRuntime == nil || newRuntime == runtime {
		t.Fatalf("completed story entry did not create a fresh runtime old=%p new=%p", runtime, newRuntime)
	}
	if !runtime.payloadReleased || newRuntime.lifecycleToken == 0 || newRuntime.lifecycleToken == runtime.lifecycleToken {
		t.Fatalf("completed story generation old_released=%t old_token=%d new_token=%d",
			runtime.payloadReleased, runtime.lifecycleToken, newRuntime.lifecycleToken)
	}
	requireQuestConnectedDungeonRuntime(t, newRuntime, 800)
	requireFreshDungeonFinishLoadingConsumableReset(t, service, session, connection)
}

func TestStoryDungeonToOrdinaryDungeonFreshConsumableState(t *testing.T) {
	service, runtime, session, connection := prepareStoryChainRuntime(t)
	requireQuestConnectedDungeonRuntime(t, runtime, 700)
	requireFreshDungeonFinishLoadingConsumableReset(t, service, session, connection)

	newRuntime := returnAndEnterDungeonForConsumableResetTest(
		t, service, session, connection, runtime, 900,
	)
	if len(newRuntime.Dungeon.Mazes[newRuntime.MazeIndex].QuestConnection) != 0 {
		t.Fatalf("ordinary dungeon %d unexpectedly quest-connected: %v",
			newRuntime.Dungeon.ID,
			newRuntime.Dungeon.Mazes[newRuntime.MazeIndex].QuestConnection)
	}
	requireFreshDungeonFinishLoadingConsumableReset(t, service, session, connection)
}

func returnAndEnterDungeonForConsumableResetTest(
	t *testing.T,
	service *Service,
	session *gameSession,
	connection *bufferConn,
	previous *runtimeDungeonState,
	dungeonID uint32,
) *runtimeDungeonState {
	t.Helper()
	connection.write.Reset()
	if err := service.handleDungeonBackToVillage(session, nil); err != nil {
		t.Fatalf("return active story dungeon: %v", err)
	}
	if session.dungeon.runtime != nil || !previous.townReturnPending || !previous.payloadReleased {
		t.Fatalf("return did not retire previous runtime owner=%p previous=%p pending=%t released=%t",
			session.dungeon.runtime, previous, previous.townReturnPending, previous.payloadReleased)
	}

	connection.write.Reset()
	if err := service.sendEnterSelectDungeonState(
		session,
		"test_story_return_open_selector",
		false,
		true,
	); err != nil {
		t.Fatalf("open dungeon selector after story return: %v", err)
	}
	if session.dungeon.runtime != nil || !session.enterSelectDungeonContextSent {
		t.Fatalf("story return selector runtime=%p context_sent=%t",
			session.dungeon.runtime, session.enterSelectDungeonContextSent)
	}

	connection.write.Reset()
	request := dungeoncmd.SelectDungeonRequest{DungeonID: dungeonID, Difficulty: 1}
	if err := service.handleDungeonSelectUpper(session, encodeCurrentDungeonSelectRequest(request)); err != nil {
		t.Fatalf("enter dungeon %d after story return: %v", dungeonID, err)
	}
	current := session.dungeon.runtime
	if current == nil || current == previous || current.Dungeon.ID != int64(dungeonID) {
		t.Fatalf("return re-entry runtime old=%p current=%p dungeon=%d want=%d",
			previous, current, currentDungeonIDForTest(current), dungeonID)
	}
	if !previous.payloadReleased || current.lifecycleToken == 0 || current.lifecycleToken == previous.lifecycleToken {
		t.Fatalf("return re-entry generation old_released=%t old_token=%d new_token=%d",
			previous.payloadReleased, previous.lifecycleToken, current.lifecycleToken)
	}
	return current
}

func requireQuestConnectedDungeonRuntime(t *testing.T, runtime *runtimeDungeonState, dungeonID int64) {
	t.Helper()
	if runtime == nil || runtime.Dungeon.ID != dungeonID || runtime.MazeIndex < 0 || runtime.MazeIndex >= len(runtime.Dungeon.Mazes) {
		t.Fatalf("story runtime=%+v want dungeon=%d", runtime, dungeonID)
	}
	connection := runtime.Dungeon.Mazes[runtime.MazeIndex].QuestConnection
	if len(connection) < 2 || connection[1] <= 0 {
		t.Fatalf("dungeon %d maze %d is not a PVF quest-connected story maze: %v",
			runtime.Dungeon.ID, runtime.MazeIndex, connection)
	}
}

func requireFreshDungeonFinishLoadingConsumableReset(
	t *testing.T,
	service *Service,
	session *gameSession,
	connection *bufferConn,
) {
	t.Helper()
	session.postStartMapPlayerStateSent = true
	// Keep unrelated actor/HUD projections outside this protocol-order
	// assertion. The production owner is still the real sendFinishLoadingStatus
	// path and must put the one-shot native-manager reset after op37/op30.
	session.postFinishLoadingPlayerStateSent = true
	connection.write.Reset()

	if err := service.sendFinishLoadingStatus(
		session,
		make([]byte, currentFinishLoadingLegacyRequestBodySize),
	); err != nil {
		t.Fatal(err)
	}
	status, rest := splitGameServerUpperPacket(t, connection.write.Bytes())
	if status.Header.Classification != dnfproto.DefaultChannelClassification ||
		status.Header.MsgID != uint16(dnfenum.CmdPacketFinishLoading) ||
		!bytes.Equal(status.Body, []byte{1}) {
		t.Fatalf("finish-loading status=%+v body=%x", status.Header, status.Body)
	}
	state, rest := splitGameServerUpperPacket(t, rest)
	if state.Header.Classification != 0 ||
		state.Header.MsgID != uint16(dnfenum.CmdPacketFinishLoading) ||
		len(state.Body) != currentFinishLoadingCharacterStateBodySize {
		t.Fatalf("story finish-loading op37=%+v body_len=%d", state.Header, len(state.Body))
	}
	commit, rest := splitGameServerUpperPacket(t, rest)
	if commit.Header.Classification != 0 ||
		commit.Header.MsgID != currentIncreaseStatusResultMsgID ||
		!bytes.Equal(commit.Body, make([]byte, currentIncreaseStatusResultBodySize)) {
		t.Fatalf("story finish-loading op30=%+v body=%x", commit.Header, commit.Body)
	}
	reset, rest := splitGameServerUpperPacket(t, rest)
	if reset.Header.Classification != 0 ||
		reset.Header.MsgID != currentDungeonConsumableUseGateResetWireOpcode ||
		len(reset.Body) != 0 {
		t.Fatalf("story finish-loading consumable reset=%+v body=%x", reset.Header, reset.Body)
	}
	if len(rest) != 0 {
		extra, _ := splitGameServerUpperPacket(t, rest)
		t.Fatalf("story finish-loading emitted packet after reset=%+v body=%x", extra.Header, extra.Body)
	}

	connection.write.Reset()
	if err := service.sendFinishLoadingStatus(
		session,
		make([]byte, currentFinishLoadingLegacyRequestBodySize),
	); err != nil {
		t.Fatal(err)
	}
	duplicate, trailing := splitGameServerUpperPacket(t, connection.write.Bytes())
	if duplicate.Header.Classification != dnfproto.DefaultChannelClassification ||
		duplicate.Header.MsgID != uint16(dnfenum.CmdPacketFinishLoading) ||
		!bytes.Equal(duplicate.Body, []byte{1}) || len(trailing) != 0 {
		t.Fatalf("duplicate story finish-loading replayed state header=%+v body=%x trailing=%x",
			duplicate.Header, duplicate.Body, trailing)
	}
}

func currentDungeonIDForTest(runtime *runtimeDungeonState) int64 {
	if runtime == nil {
		return 0
	}
	return runtime.Dungeon.ID
}
