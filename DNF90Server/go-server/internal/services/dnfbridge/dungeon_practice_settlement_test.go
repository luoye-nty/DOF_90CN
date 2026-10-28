package dnfbridge

import (
	"bytes"
	"context"
	"encoding/binary"
	"testing"

	"longheng.io/server/internal/modules/dnf/channelcatalog"
	"longheng.io/server/internal/modules/dnf/dnfenum"
	"longheng.io/server/internal/modules/dnf/dungeoncmd"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
	"longheng.io/server/internal/modules/dnf/worldmap"
)

func TestPracticeDungeonSettlementSendsOp34Op35Op255WithoutGrowth(t *testing.T) {
	service, runtime, session, connection, _ := prepareCompletedSettlementRuntime(t)
	runtime.ProtocolMode = currentDungeonProtocolModePractice
	runtime.Request.RuntimeState = byte(currentDungeonProtocolModePractice)
	runtime.clearMapCompletionKey = ""
	connection.write.Reset()

	characterBefore := runtime.Character
	request := make([]byte, currentDungeonPlayResultBaseSize)
	request[currentDungeonPlayResultClientRankPointOffset] = 73
	op34, err := buildCurrentDungeonPlayResultNoticeBody(currentDungeonPlayResultNotice{
		RankGrade: 60, ClearTimeMS: 1000, RankPoint: 73,
		Participants: []currentDungeonPlayResultParticipant{{ObjectKey: session.selectedCharacterID, ClearTimeMS: 1000}},
	})
	if err != nil {
		t.Fatal(err)
	}
	op35, err := buildCurrentPracticeDungeonClearRewardBody(session.selectedCharacterID)
	if err != nil {
		t.Fatal(err)
	}
	runtime.settlementResultPlan = &currentDungeonSettlementPacketPlan{
		CharacterID: session.selectedCharacterID, CompletionKey: "practice:test", Source: "practice_test",
		ClientRankPoint: 73, PlayResultBody: op34, ClearRewardBody: op35,
	}
	if err := service.handleDungeonSetPlayResult(session, request); err != nil {
		t.Fatal(err)
	}
	op34Packet, rest := splitGameServerUpperPacket(t, connection.write.Bytes())
	op35Packet, rest := splitGameServerUpperPacket(t, rest)
	op255, rest := splitGameServerUpperPacket(t, rest)
	if len(rest) != 0 || op34Packet.Header.MsgID != currentDungeonPlayResultNoticeMsgID ||
		op35Packet.Header.MsgID != currentDungeonClearRewardMsgID ||
		op255.Header.MsgID != currentDungeonPracticeSummaryMsgID {
		t.Fatalf("practice result order=%d,%d,%d rest=%x", op34Packet.Header.MsgID, op35Packet.Header.MsgID, op255.Header.MsgID, rest)
	}
	if len(op35Packet.Body) != currentDungeonClearRewardMinimumBodySize {
		t.Fatalf("practice op35 len=%d", len(op35Packet.Body))
	}
	if !bytes.Equal(op255.Body, []byte{0, 0, 0, 0, 0}) {
		t.Fatalf("practice op255 body=%x", op255.Body)
	}
	if runtime.settlementCharacterStateSent || runtime.settlementCardScrollStateSent ||
		runtime.settlementCardLayoutSent || runtime.settlementCardRewardCommitted {
		t.Fatalf("practice opened ordinary growth/card state=%+v", runtime)
	}
	if runtime.Character.Level != characterBefore.Level || runtime.Character.Stats["exp"] != characterBefore.Stats["exp"] {
		t.Fatalf("practice mutated runtime character before=%+v after=%+v", characterBefore, runtime.Character)
	}
	if runtime.settlementStatisticReceived || !completedDungeonSettlementResultReady(runtime, session.selectedCharacterID) ||
		!service.completedDungeonSettlementExitReady(runtime, session.selectedCharacterID) {
		t.Fatalf("practice op255 did not close its result barrier: %+v", runtime)
	}

	connection.write.Reset()
	if err := service.handleDungeonSetPlayResult(session, append([]byte(nil), request...)); err != nil {
		t.Fatal(err)
	}
	for index, want := range []uint16{currentDungeonPlayResultNoticeMsgID, currentDungeonClearRewardMsgID, currentDungeonPracticeSummaryMsgID} {
		packet, trailing := splitGameServerUpperPacket(t, connection.write.Bytes())
		if packet.Header.MsgID != want {
			t.Fatalf("replay[%d] msg=%d want=%d", index, packet.Header.MsgID, want)
		}
		connection.write.Reset()
		connection.write.Write(trailing)
	}
	if connection.write.Len() != 0 {
		t.Fatalf("practice replay trailing=%x", connection.write.Bytes())
	}
}

// The practice policy is reachable through both proven practice shapes: the
// explicit state=1/token=0 pair, and this client's 练习模式 switch, which writes
// state=1/token=UINT16_MAX. Neither shape may require fatigue or consume the
// entry item.
//
// Live 2026-09-17 16:08:52 (packet_log.txt): the operator ran 练习模式 on
// 倒悬的瞭望台 (190) with fatigue deliberately set to 0, and the selector pair was
// read as an ordinary entry, so the run was refused with
// `error=dnf dungeon character fatigue is exhausted: fatigue=0` plus the
// 「疲劳值不足」 prompt.
func TestPracticeDungeonEntryBypassesFatigueAndTicketWithoutMutation(t *testing.T) {
	for _, testCase := range []struct {
		name         string
		runtimeToken uint16
	}{
		{name: "explicit practice pair", runtimeToken: 0},
		{name: "练习模式 selector pair", runtimeToken: 0xffff},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			source := bridgeDungeonPVF(false)
			source["dungeon/test.dgn"] += "[required item]\n4726 1 0\n[/required item]\n"
			table, resolver, monsters := loadBridgeDungeonStaticData(t, source)
			repositories := dnfrepomemory.NewMemoryGroup()
			if err := repositories.Character.Save(context.Background(), dnfrepo.CharacterRecord{
				CharacterID: "99", AccountID: "account-1", Level: 20,
				Stats: map[string]int64{
					"fatigue": 0, "town_id": 38, "area_id": 1,
					"pos_x": 450, "pos_y": 234, "direction": 5, "area_state": 3,
				},
			}); err != nil {
				t.Fatal(err)
			}
			if err := repositories.Inventory.Save(context.Background(), dnfrepo.InventoryRecord{
				CharacterID: "99",
				Slots: map[string]dnfrepo.ItemStack{
					"0:4": {ItemID: 4726, Count: 3},
				},
			}); err != nil {
				t.Fatal(err)
			}
			service := &Service{
				options:             options{accountID: "account-1", gameUpperBodyCodec: gameUpperBodyCodecPlain},
				worldMapTable:       table,
				worldMapResolver:    resolver,
				dungeonMonsterTable: monsters,
				dungeonChoice:       func(int) (int, error) { return 0, nil },
				dungeonSeed:         func() (uint32, error) { return 1, nil },
				repositoryProvider:  func() (dnfrepo.Group, bool) { return repositories, true },
			}
			connection := &bufferConn{}
			channel := channelcatalog.Channel{ServerID: 1, ID: 19, Type: 1, Name: "ch.19", Port: 10019}
			session := &gameSession{
				conn: connection, connID: "practice-entry-test-" + testCase.name, channel: channel, residentChannel: channel,
				selectedCharacterID: 99, selectPreviewActorRemoved: true,
			}
			bindDungeonSelectorOriginForTestAt(t, service, session, 38, 1, 450, 234)
			body := make([]byte, dungeoncmd.SelectDungeonRequestSize)
			binary.LittleEndian.PutUint32(body[0:4], 700)
			body[4] = 1
			body[8] = byte(currentDungeonProtocolModePractice)
			binary.LittleEndian.PutUint16(body[9:11], testCase.runtimeToken)
			if err := service.handleDungeonSelectUpper(session, body); err != nil {
				t.Fatal(err)
			}

			foundOp28 := false
			remaining := connection.write.Bytes()
			for len(remaining) != 0 {
				packet, tail := splitGameServerUpperPacket(t, remaining)
				if packet.Header.MsgID == currentDungeonInfoNotification {
					foundOp28 = true
					if len(packet.Body) <= 12 || packet.Body[12] != byte(currentDungeonProtocolModePractice) {
						t.Fatalf("practice op28 body=%x", packet.Body)
					}
				}
				remaining = tail
			}
			if !foundOp28 {
				t.Fatalf("practice entry emitted no op28 stream=%x", connection.write.Bytes())
			}
			runtime := session.dungeon.runtime
			if runtime == nil || !currentDungeonRuntimeIsPractice(runtime) || runtime.Request.RuntimeState != 1 ||
				runtime.Request.RuntimeToken != testCase.runtimeToken ||
				len(runtime.Dungeon.Metadata.RequiredItems) != 1 {
				t.Fatalf("practice runtime=%+v", runtime)
			}
			inventory, found, err := repositories.Inventory.Load(context.Background(), "99")
			if err != nil || !found || inventory.Slots["0:4"].Count != 3 {
				t.Fatalf("practice ticket mutated inventory=%+v found=%t err=%v", inventory, found, err)
			}
			character, found, err := repositories.Character.Load(context.Background(), "99")
			if err != nil || !found || character.Stats["fatigue"] != 0 {
				t.Fatalf("practice fatigue mutated character=%+v found=%t err=%v", character, found, err)
			}
		})
	}
}

func TestCurrentClientDefaultDungeonEntryUsesNormalRuntimePolicy(t *testing.T) {
	source := bridgeDungeonPVF(false)
	source["dungeon/test.dgn"] += "[required item]\n4726 1 0\n[/required item]\n"
	table, resolver, monsters := loadBridgeDungeonStaticData(t, source)
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Character.Save(context.Background(), dnfrepo.CharacterRecord{
		CharacterID: "99", AccountID: "account-1", Level: 20,
		Stats: map[string]int64{
			"fatigue": 1, "town_id": 38, "area_id": 1,
			"pos_x": 450, "pos_y": 234, "direction": 5, "area_state": 3,
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Inventory.Save(context.Background(), dnfrepo.InventoryRecord{
		CharacterID: "99",
		Slots: map[string]dnfrepo.ItemStack{
			"0:4": {ItemID: 4726, Count: 3},
		},
	}); err != nil {
		t.Fatal(err)
	}
	service := &Service{
		options:             options{accountID: "account-1", gameUpperBodyCodec: gameUpperBodyCodecPlain},
		worldMapTable:       table,
		worldMapResolver:    resolver,
		dungeonMonsterTable: monsters,
		dungeonChoice:       func(int) (int, error) { return 0, nil },
		dungeonSeed:         func() (uint32, error) { return 1, nil },
		repositoryProvider:  func() (dnfrepo.Group, bool) { return repositories, true },
	}
	connection := &bufferConn{}
	channel := channelcatalog.Channel{ServerID: 1, ID: 19, Type: 1, Name: "ch.19", Port: 10019}
	session := &gameSession{
		conn: connection, connID: "normal-entry-default-state-test", channel: channel, residentChannel: channel,
		selectedCharacterID: 99, selectPreviewActorRemoved: true,
	}
	bindDungeonSelectorOriginForTestAt(t, service, session, 38, 1, 450, 234)
	body := make([]byte, dungeoncmd.SelectDungeonRequestSize)
	binary.LittleEndian.PutUint32(body[0:4], 700)
	body[4] = 1
	// The default ordinary 进入 this client actually writes: runtime state 0 with
	// the UINT16_MAX token (481 captured select requests, packet_log.txt
	// 2026-09-15..09-17). State 1 with that token is the 练习模式 switch and is
	// pinned by TestCurrentClientPracticeModeEntryUsesPracticeRuntimePolicy.
	binary.LittleEndian.PutUint16(body[9:11], 0xffff)
	if err := service.handleDungeonSelectUpper(session, body); err != nil {
		t.Fatal(err)
	}

	foundOp28 := false
	remaining := connection.write.Bytes()
	for len(remaining) != 0 {
		packet, tail := splitGameServerUpperPacket(t, remaining)
		if packet.Header.MsgID == currentDungeonInfoNotification {
			foundOp28 = true
			if len(packet.Body) <= 12 || packet.Body[12] != byte(currentDungeonProtocolModeNormal) {
				t.Fatalf("normal op28 body=%x", packet.Body)
			}
		}
		remaining = tail
	}
	if !foundOp28 {
		t.Fatalf("normal entry emitted no op28 stream=%x", connection.write.Bytes())
	}
	runtime := session.dungeon.runtime
	if runtime == nil || currentDungeonRuntimeIsPractice(runtime) || runtime.Request.RuntimeState != 0 || runtime.Request.RuntimeToken != 0xffff {
		t.Fatalf("normal runtime=%+v", runtime)
	}
	inventory, found, err := repositories.Inventory.Load(context.Background(), "99")
	if err != nil || !found || inventory.Slots["0:4"].Count != 2 {
		t.Fatalf("normal ticket was not consumed inventory=%+v found=%t err=%v", inventory, found, err)
	}
}

func TestPracticeDungeonRetryRequiresFreshManualPracticeChoice(t *testing.T) {
	service, runtime, session, connection := prepareOrdinaryCompletedSettlementRuntime(t)
	runtime.ProtocolMode = currentDungeonProtocolModePractice
	runtime.Request.RuntimeState = byte(currentDungeonProtocolModePractice)
	runtime.settlementPlayResultReceived = true
	runtime.settlementStatisticReceived = false
	runtime.settlementClearRewardSent = true
	runtime.advanceSettlementPhase(currentDungeonSettlementPhaseResultShown)
	repositories, _ := service.repositoryGroup()
	character, found, err := repositories.Character.Load(context.Background(), "99")
	if err != nil || !found {
		t.Fatalf("load character found=%t err=%v", found, err)
	}
	character.Stats["fatigue"] = 0
	if err := repositories.Character.Save(context.Background(), character); err != nil {
		t.Fatal(err)
	}
	connection.write.Reset()

	if err := service.handleDungeonEplpCommand(session, []byte{1, 0}); err != nil {
		t.Fatal(err)
	}
	first, _ := splitGameServerUpperPacket(t, connection.write.Bytes())
	if first.Header.MsgID != uint16(dnfenum.CmdPacketEplpCommand) ||
		!bytes.Equal(first.Body, buildCurrentDungeonOp72SuccessBody(1, 0)) {
		t.Fatalf("practice retry first packet=%+v body=%x", first.Header, first.Body)
	}
	// The old practice choice is not inherited. This fixture has zero fatigue,
	// so its normalized ordinary retry is rejected and returns to selector state
	// instead of silently creating another reward-free practice runtime.
	if session.dungeon.runtime != nil {
		t.Fatalf("practice retry leaked into fresh runtime old=%p new=%+v", runtime, session.dungeon.runtime)
	}
}

func TestPracticeDungeonOp42ReturnsWithoutOrdinaryCardsOrStatistic(t *testing.T) {
	service, runtime, session, connection := prepareOrdinaryCompletedSettlementRuntime(t)
	runtime.ProtocolMode = currentDungeonProtocolModePractice
	runtime.Request.RuntimeState = byte(currentDungeonProtocolModePractice)
	runtime.settlementPlayResultReceived = true
	runtime.settlementStatisticReceived = false
	runtime.settlementClearRewardSent = true
	runtime.advanceSettlementPhase(currentDungeonSettlementPhaseResultShown)
	connection.write.Reset()

	if err := service.handleDungeonGiveupGame(session, nil); err != nil {
		t.Fatal(err)
	}
	foundTownTransition := false
	remaining := connection.write.Bytes()
	for len(remaining) != 0 {
		packet, tail := splitGameServerUpperPacket(t, remaining)
		if packet.Header.MsgID == currentSceneTransitionMsgID {
			foundTownTransition = true
		}
		if packet.Header.MsgID == uint16(dnfenum.CmdPacketScoreScrollState) ||
			packet.Header.MsgID == uint16(dnfenum.CmdPacketCardSelectRightState) ||
			packet.Header.MsgID == uint16(dnfenum.CmdPacketSelectCard) {
			t.Fatalf("practice op42 emitted ordinary card packet=%d", packet.Header.MsgID)
		}
		remaining = tail
	}
	if !foundTownTransition || !runtime.townReturnOp24Sent || session.dungeon.runtime != nil {
		t.Fatalf("practice op42 transition=%t op24=%t runtime=%p stream=%x",
			foundTownTransition, runtime.townReturnOp24Sent, session.dungeon.runtime, connection.write.Bytes())
	}
}

func TestPracticeDungeonRouteUsesOnlyNonQuestMazeAndHonorsPVFProhibition(t *testing.T) {
	mazes := []worldmap.Maze{
		{QuestConnection: []int64{0, 3344}},
		{},
	}
	selection, err := selectPracticeDungeonMaze(mazes, func(int) (int, error) { return 0, nil })
	if err != nil || selection.Index != 1 {
		t.Fatalf("practice maze=%+v err=%v", selection, err)
	}
	dungeon := worldmap.Dungeon{Metadata: worldmap.DungeonMetadata{IntegerValues: map[string]int64{"prohibit practice": 1}}}
	if err := validatePracticeDungeonRoute(dungeon); err == nil {
		t.Fatal("PVF prohibit practice was accepted")
	}
}
