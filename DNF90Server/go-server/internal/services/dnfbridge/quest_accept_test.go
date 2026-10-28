package dnfbridge

import (
	"bytes"
	"context"
	"encoding/binary"
	"os"
	"testing"

	"longheng.io/server/internal/modules/dnf/channelcatalog"
	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfproto "longheng.io/server/internal/modules/dnf/protocol"
	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
	dnfquest "longheng.io/server/internal/modules/dnf/quest"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

func TestRealScriptPVFQuest1680AcceptanceItemsResolveInCurrentCatalog(t *testing.T) {
	pvfPath := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE to verify quest 1680 item definitions")
	}
	archive, err := platformpvf.LoadArchive(platformpvf.Options{Path: pvfPath})
	if err != nil {
		t.Fatal(err)
	}
	index, err := dnfpvf.Build(context.Background(), archive, dnfpvf.BuildOptions{Lists: []string{dnfquest.DefaultList}})
	if err != nil {
		t.Fatal(err)
	}
	quests, err := dnfquest.Load(context.Background(), index)
	if err != nil {
		t.Fatal(err)
	}
	definition, found := quests.Find(1680)
	if !found || !definition.HasDependGiveItem || len(definition.DependGiveItemData) == 0 || len(definition.DependGiveItemData)%2 != 0 {
		t.Fatalf("quest 1680 definition=%+v found=%t", definition, found)
	}
	items, err := newPVFDungeonDropCatalog(archive)
	if err != nil {
		t.Fatal(err)
	}
	for index := 0; index < len(definition.DependGiveItemData); index += 2 {
		itemID, count := definition.DependGiveItemData[index], definition.DependGiveItemData[index+1]
		resolved, err := items.ResolveItem(uint32(itemID))
		if err != nil || resolved.ItemID != uint32(itemID) || count <= 0 {
			t.Fatalf("quest 1680 event item=(%d,%d) resolved=%+v err=%v", itemID, count, resolved, err)
		}
	}
}

func TestCurrentAcceptQuestPersistsBeforeExactCurrentEXEAck(t *testing.T) {
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Character.Save(context.Background(), dnfrepo.CharacterRecord{
		CharacterID: "17",
		AccountID:   defaultAccountPrefix + "1",
		Job:         "2",
		Level:       1,
		Stats:       map[string]int64{"grow_type": 0},
	}); err != nil {
		t.Fatal(err)
	}
	connection := &bufferConn{}
	service := &Service{
		options:            options{gameUpperBodyCodec: gameUpperBodyCodecPlain},
		questCatalog:       buildAcceptQuestTestCatalog(t, false),
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
	}
	session := &gameSession{
		conn:                        connection,
		connID:                      "accept-quest-test",
		channel:                     channelcatalog.Channel{ID: 38},
		selectedCharacterID:         17,
		initialTownRouteCharacterID: 17,
		initialTownRouteStage:       currentInitialTownRoutePlayerStateSent,
	}
	staleTriggerKey := newCurrentSetQuestTriggerReplayKey(17, dnfquest.SetTriggerRequest{
		QuestID:     3145,
		TriggerType: 0x10,
	})
	session.suppressCurrentSetQuestTriggerReplay(staleTriggerKey)
	request, err := dnfproto.BuildChannelPacket(uint16(dnfenum.CmdPacketAcceptQuest), []byte{0x1f, 0x00, 0x49, 0x0c}, 9, dnfproto.DefaultChannelClassification)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.handleGameUpper(session, request); err != nil {
		t.Fatal(err)
	}
	stored, ok, err := repositories.Quest.Load(context.Background(), "17")
	if err != nil || !ok {
		t.Fatalf("load persisted quest ok=%t err=%v", ok, err)
	}
	if state := stored.States[3145]; state.Status != "active" || state.ProgressValue != 1 {
		t.Fatalf("persisted quest state = %+v", state)
	}
	if session.currentSetQuestTriggerReplaySuppressed(staleTriggerKey) {
		t.Fatal("newly activated quest retained the preceding generation's terminal op33 replay key")
	}
	packet, rest := splitGameServerUpperPacket(t, connection.write.Bytes())
	if packet.Header.Classification != dnfproto.DefaultChannelClassification || packet.Header.MsgID != uint16(dnfenum.CmdPacketAcceptQuest) {
		t.Fatalf("ack header = %+v", packet.Header)
	}
	wantBody := []byte{0x01, 0x49, 0x0c, 0x01, 0x00, 0x00, 0x00, 0x00}
	if !bytes.Equal(packet.Body, wantBody) {
		t.Fatalf("ack body = % x, want % x", packet.Body, wantBody)
	}
	// A newly active quest needs the all-task op21 before op574 so the current
	// client rebuilds its route-guide owner; no completed quest exists here, so
	// the optional op356 completed bitmap is not appended.
	acceptable, rest := splitGameServerUpperPacket(t, rest)
	if acceptable.Header.Classification != 0 || acceptable.Header.MsgID != currentAcceptableQuestListMsgID {
		t.Fatalf("acceptable list header=%+v", acceptable.Header)
	}
	active, trailing := splitGameServerUpperPacket(t, rest)
	if len(trailing) != 0 || active.Header.Classification != 0 || active.Header.MsgID != currentActiveQuestSnapshotMsgID {
		t.Fatalf("active snapshot header=%+v trailing=%x", active.Header, trailing)
	}
	wantActive := []byte{0x01, 0x00, 0x00, 0x00, 0x49, 0x0c, 0x01, 0x00, 0x00, 0x00}
	if !bytes.Equal(active.Body, wantActive) {
		t.Fatalf("active snapshot body=%x want=%x", active.Body, wantActive)
	}
}

func TestCurrentAcceptQuestAfterBulkCompletionPublishesAuthoritativeGuideOrder(t *testing.T) {
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Character.Save(context.Background(), dnfrepo.CharacterRecord{
		CharacterID: "17",
		AccountID:   defaultAccountPrefix + "1",
		Job:         "2",
		Level:       2,
		Stats:       map[string]int64{"grow_type": 0},
	}); err != nil {
		t.Fatal(err)
	}
	// Task-manual bulk completion leaves the predecessor completed and the
	// recovered successor acceptable, but no active op574 generation exists
	// until the player accepts it.
	if err := repositories.Quest.Save(context.Background(), dnfrepo.QuestRecord{
		CharacterID: "17",
		States: map[int64]dnfrepo.QuestState{
			3145: {Status: "completed"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	connection := &bufferConn{}
	service := &Service{
		options:            options{gameUpperBodyCodec: gameUpperBodyCodecPlain},
		questCatalog:       finishBridgeCatalog(t),
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
	}
	session := &gameSession{
		conn:                        connection,
		connID:                      "accept-after-bulk-completion-test",
		channel:                     channelcatalog.Channel{ID: 38},
		selectedCharacterID:         17,
		initialTownRouteCharacterID: 17,
		initialTownRouteStage:       currentInitialTownRoutePlayerStateSent,
	}
	request, err := dnfproto.BuildChannelPacket(
		uint16(dnfenum.CmdPacketAcceptQuest),
		[]byte{0x1f, 0x00, 0x4a, 0x0c},
		9,
		dnfproto.DefaultChannelClassification,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.handleGameUpper(session, request); err != nil {
		t.Fatal(err)
	}
	stored, ok, err := repositories.Quest.Load(context.Background(), "17")
	if err != nil || !ok || stored.States[3146].Status != "active" {
		t.Fatalf("persisted successor state=%+v ok=%t err=%v", stored.States[3146], ok, err)
	}

	ack, rest := splitGameServerUpperPacket(t, connection.write.Bytes())
	acceptable, rest := splitGameServerUpperPacket(t, rest)
	active, rest := splitGameServerUpperPacket(t, rest)
	completed, trailing := splitGameServerUpperPacket(t, rest)
	if len(trailing) != 0 ||
		ack.Header.MsgID != uint16(dnfenum.CmdPacketAcceptQuest) ||
		ack.Header.Classification != dnfproto.DefaultChannelClassification ||
		acceptable.Header.MsgID != currentAcceptableQuestListMsgID ||
		acceptable.Header.Classification != 0 ||
		active.Header.MsgID != currentActiveQuestSnapshotMsgID ||
		active.Header.Classification != 0 ||
		completed.Header.MsgID != currentClearQuestListMsgID ||
		completed.Header.Classification != 0 {
		t.Fatalf("recovered successor accept order ack=%+v acceptable=%+v active=%+v completed=%+v trailing=%x",
			ack.Header, acceptable.Header, active.Header, completed.Header, trailing)
	}
	if len(active.Body) != 10 || binary.LittleEndian.Uint32(active.Body) != 1 ||
		binary.LittleEndian.Uint16(active.Body[4:]) != 3146 {
		t.Fatalf("recovered successor active snapshot=%x", active.Body)
	}
}

func TestSuppressSceneInitializationAcceptQuestAllowsFinalizedTownScene(t *testing.T) {
	session := &gameSession{
		selectedCharacterID:       19,
		townSceneReadyCharacterID: 19,
		initialTownRouteStage:     currentInitialTownRouteIdle,
	}
	suppressed, readiness, stage := (&Service{}).suppressSceneInitializationAcceptQuestRequest(session)
	if suppressed || readiness != "town_scene_player_state_finalized" || stage != currentInitialTownRouteIdle {
		t.Fatalf("town-ready accept suppression=%t readiness=%q stage=%d", suppressed, readiness, stage)
	}
}

func TestCurrentAcceptQuestAlreadyActiveDoesNotReply(t *testing.T) {
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Character.Save(context.Background(), dnfrepo.CharacterRecord{
		CharacterID: "20",
		AccountID:   defaultAccountPrefix + "1",
		Job:         "2",
		Level:       1,
		Stats:       map[string]int64{"grow_type": 0},
	}); err != nil {
		t.Fatal(err)
	}
	connection := &bufferConn{}
	service := &Service{
		options:            options{gameUpperBodyCodec: gameUpperBodyCodecPlain},
		questCatalog:       buildAcceptQuestTestCatalog(t, false),
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
	}
	session := &gameSession{
		conn:                        connection,
		connID:                      "accept-quest-idempotent-test",
		channel:                     channelcatalog.Channel{ID: 38},
		selectedCharacterID:         20,
		initialTownRouteCharacterID: 20,
		initialTownRouteStage:       currentInitialTownRoutePlayerStateSent,
	}
	body := []byte{0x1f, 0x00, 0x49, 0x0c}
	if err := service.handleCurrentAcceptQuest(session, body); err != nil {
		t.Fatal(err)
	}
	connection.write.Reset()
	if err := service.handleCurrentAcceptQuest(session, body); err != nil {
		t.Fatal(err)
	}
	if connection.write.Len() != 0 {
		t.Fatalf("idempotent accept emitted response=%x", connection.write.Bytes())
	}
}

func TestCurrentAcceptQuestEventItemAtomicallyPersistsAndUsesNativeAckRows(t *testing.T) {
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Character.Save(context.Background(), dnfrepo.CharacterRecord{
		CharacterID: "18", AccountID: defaultAccountPrefix + "1", Job: "2", Level: 1,
		Stats: map[string]int64{"grow_type": 0},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Inventory.Save(context.Background(), dnfrepo.InventoryRecord{
		CharacterID: "18", Slots: map[string]dnfrepo.ItemStack{},
	}); err != nil {
		t.Fatal(err)
	}
	connection := &bufferConn{}
	service := &Service{
		options:            options{gameUpperBodyCodec: gameUpperBodyCodecPlain},
		questCatalog:       buildAcceptQuestTestCatalog(t, true),
		pvfItemCatalog:     acceptQuestTestItemCatalog(),
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
	}
	session := &gameSession{
		conn:                        connection,
		selectedCharacterID:         18,
		initialTownRouteCharacterID: 18,
		initialTownRouteStage:       currentInitialTownRoutePlayerStateSent,
	}
	if err := service.handleCurrentAcceptQuest(session, []byte{0x1f, 0x00, 0x49, 0x0c}); err != nil {
		t.Fatal(err)
	}
	questRecord, ok, err := repositories.Quest.Load(context.Background(), "18")
	if err != nil || !ok || questRecord.States[3145].Status != "active" {
		t.Fatalf("event-item quest record=%+v ok=%t err=%v", questRecord, ok, err)
	}
	inventory, ok, err := repositories.Inventory.Load(context.Background(), "18")
	if err != nil || !ok || inventory.Slots["0:177"].ItemID != 1001 || inventory.Slots["0:177"].Count != 1 {
		t.Fatalf("event-item inventory=%+v ok=%t err=%v", inventory, ok, err)
	}
	ack, rest := splitGameServerUpperPacket(t, connection.write.Bytes())
	wantACK := []byte{
		0x01, 0x49, 0x0c, 0x01, 0x00, 0x00, 0x00, 0x01,
		0xb1, 0x00, 0xe9, 0x03, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00,
	}
	if ack.Header.MsgID != uint16(dnfenum.CmdPacketAcceptQuest) ||
		ack.Header.Classification != dnfproto.DefaultChannelClassification || !bytes.Equal(ack.Body, wantACK) {
		t.Fatalf("event-item ACK header=%+v body=%x want=%x", ack.Header, ack.Body, wantACK)
	}
	acceptable, rest := splitGameServerUpperPacket(t, rest)
	if acceptable.Header.MsgID != currentAcceptableQuestListMsgID {
		t.Fatalf("event-item acceptable list header=%+v", acceptable.Header)
	}
	active, trailing := splitGameServerUpperPacket(t, rest)
	if len(trailing) != 0 || active.Header.MsgID != currentActiveQuestSnapshotMsgID {
		t.Fatalf("event-item active snapshot header=%+v trailing=%x", active.Header, trailing)
	}
}

func acceptQuestTestItemCatalog() *pvfDungeonDropCatalog {
	return &pvfDungeonDropCatalog{
		source: dungeonDropCatalogTestSource{
			"stackable/quest/accept_event.stk": "[stackable type]\n`[quest]`\n[stack limit]\n999\n",
		},
		itemRefs: map[uint32]dungeonDropItemReference{
			1001: {kind: dungeonDropItemStackable, path: "stackable/quest/accept_event.stk"},
		},
		itemCache: map[uint32]dungeonDropItemDefinition{
			1001: {
				ItemID: 1001, Kind: dungeonDropItemStackable, PVFPath: "stackable/quest/accept_event.stk",
				StackableType: "[quest]", StackLimit: 999, SlotStart: 177, SlotEnd: 232,
			},
		},
	}
}

func TestCurrentAcceptQuestPersistedActiveAcknowledgesOnceWithoutSnapshot(t *testing.T) {
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Character.Save(context.Background(), dnfrepo.CharacterRecord{
		CharacterID: "20",
		AccountID:   defaultAccountPrefix + "1",
		Job:         "2",
		Level:       1,
		Stats:       map[string]int64{"grow_type": 0},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Quest.Save(context.Background(), dnfrepo.QuestRecord{
		CharacterID: "20",
		States: map[int64]dnfrepo.QuestState{
			3145: {Status: "active", ProgressValue: 1},
		},
	}); err != nil {
		t.Fatal(err)
	}
	connection := &bufferConn{}
	service := &Service{
		options:            options{gameUpperBodyCodec: gameUpperBodyCodecPlain},
		questCatalog:       buildAcceptQuestTestCatalog(t, false),
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
	}
	session := &gameSession{
		conn:                        connection,
		selectedCharacterID:         20,
		initialTownRouteCharacterID: 20,
		initialTownRouteStage:       currentInitialTownRoutePlayerStateSent,
	}
	if err := service.handleCurrentAcceptQuest(session, []byte{0x1f, 0x00, 0x49, 0x0c}); err != nil {
		t.Fatal(err)
	}
	ack, rest := splitGameServerUpperPacket(t, connection.write.Bytes())
	if len(rest) != 0 || ack.Header.MsgID != uint16(dnfenum.CmdPacketAcceptQuest) ||
		ack.Header.Classification != dnfproto.DefaultChannelClassification ||
		!bytes.Equal(ack.Body, []byte{1, 0x49, 0x0c, 0x01, 0, 0, 0, 0}) {
		t.Fatalf("persisted-active accept ACK header=%+v body=%x trailing=%x", ack.Header, ack.Body, rest)
	}
	connection.write.Reset()
	if err := service.handleCurrentAcceptQuest(session, []byte{0x1f, 0x00, 0x49, 0x0c}); err != nil {
		t.Fatal(err)
	}
	if got := connection.write.Bytes(); len(got) != 0 {
		t.Fatalf("same-session persisted-active replay emitted response=%x", got)
	}
}

func TestKnownTerminalQuestReplaySkipsPreDispatchLoggingOnlyForExactRequest(t *testing.T) {
	session := &gameSession{selectedCharacterID: 20}
	acceptRequest := dnfquest.QuestIDRequest{QuestID: 3145}
	session.suppressCurrentAcceptQuestReplay(newCurrentAcceptQuestReplayKey(session.selectedCharacterID, acceptRequest))
	acceptBody := []byte{0x1f, 0x00, 0x49, 0x0c}
	if !shouldSuppressKnownQuestReplayBeforeGameUpperLog(session, uint16(dnfenum.CmdPacketAcceptQuest), dnfproto.DefaultChannelClassification, acceptBody) {
		t.Fatal("known active op31 replay was not suppressed before logging")
	}
	if shouldSuppressKnownQuestReplayBeforeGameUpperLog(session, uint16(dnfenum.CmdPacketAcceptQuest), dnfproto.DefaultChannelClassification, []byte{0x1f, 0x00, 0x4a, 0x0c}) {
		t.Fatal("different op31 quest was incorrectly suppressed")
	}
	if shouldSuppressKnownQuestReplayBeforeGameUpperLog(session, uint16(dnfenum.CmdPacketAcceptQuest), dnfproto.DefaultChannelClassification, []byte{0x20, 0x00, 0x49, 0x0c}) {
		t.Fatal("malformed op31 echo was incorrectly suppressed")
	}

	triggerRequest := dnfquest.SetTriggerRequest{QuestID: 4728, TriggerType: 16}
	session.suppressCurrentSetQuestTriggerReplay(newCurrentSetQuestTriggerReplayKey(session.selectedCharacterID, triggerRequest))
	triggerBody := []byte{0x21, 0x00, 0x78, 0x12, 0x10, 0x00}
	if !shouldSuppressKnownQuestReplayBeforeGameUpperLog(session, uint16(dnfenum.CmdPacketSetQuestTrigger), dnfproto.DefaultChannelClassification, triggerBody) {
		t.Fatal("known zero op33 replay was not suppressed before logging")
	}
	if shouldSuppressKnownQuestReplayBeforeGameUpperLog(session, uint16(dnfenum.CmdPacketSetQuestTrigger), dnfproto.DefaultChannelClassification, []byte{0x21, 0x00, 0x78, 0x12, 0x11, 0x00}) {
		t.Fatal("different op33 trigger channel was incorrectly suppressed")
	}
}

func TestCurrentAcceptQuestKnownIneligiblePassiveRepeatIsSilent(t *testing.T) {
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Character.Save(context.Background(), dnfrepo.CharacterRecord{
		CharacterID: "19", AccountID: defaultAccountPrefix + "1", Job: "2", Level: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Quest.Save(context.Background(), dnfrepo.QuestRecord{
		CharacterID: "19",
		States: map[int64]dnfrepo.QuestState{
			3145: {Status: "completed"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	connection := &bufferConn{}
	service := &Service{
		options:            options{gameUpperBodyCodec: gameUpperBodyCodecPlain},
		questCatalog:       buildAcceptQuestTestCatalog(t, false),
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
	}
	session := &gameSession{
		conn:                        connection,
		selectedCharacterID:         19,
		initialTownRouteCharacterID: 19,
		initialTownRouteStage:       currentInitialTownRoutePlayerStateSent,
	}
	if err := service.handleCurrentAcceptQuest(session, []byte{0x1f, 0x00, 0x49, 0x0c}); err != nil {
		t.Fatal(err)
	}
	if connection.write.Len() != 0 {
		t.Fatalf("known ineligible passive repeat emitted failure popup packet=%x", connection.write.Bytes())
	}
	stored, ok, err := repositories.Quest.Load(context.Background(), "19")
	if err != nil || !ok || stored.States[3145].Status != "completed" {
		t.Fatalf("ineligible quest state changed ok=%t err=%v state=%+v", ok, err, stored.States[3145])
	}
}

func TestCurrentAcceptQuestUnknownDefinitionKeepsCurrentFailure23(t *testing.T) {
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Character.Save(context.Background(), dnfrepo.CharacterRecord{
		CharacterID: "19", AccountID: defaultAccountPrefix + "1", Job: "2", Level: 1,
	}); err != nil {
		t.Fatal(err)
	}
	connection := &bufferConn{}
	service := &Service{
		options:            options{gameUpperBodyCodec: gameUpperBodyCodecPlain},
		questCatalog:       buildAcceptQuestTestCatalog(t, false),
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
	}
	session := &gameSession{
		conn:                        connection,
		selectedCharacterID:         19,
		initialTownRouteCharacterID: 19,
		initialTownRouteStage:       currentInitialTownRoutePlayerStateSent,
	}
	if err := service.handleCurrentAcceptQuest(session, []byte{0x1f, 0x00, 0x0f, 0x27}); err != nil {
		t.Fatal(err)
	}
	packet, rest := splitGameServerUpperPacket(t, connection.write.Bytes())
	if len(rest) != 0 || packet.Header.MsgID != uint16(dnfenum.CmdPacketAcceptQuest) ||
		!bytes.Equal(packet.Body, []byte{0x00, 0x17}) {
		t.Fatalf("unknown-definition failure header=%+v body=%x rest=%x", packet.Header, packet.Body, rest)
	}
	if _, ok, err := repositories.Quest.Load(context.Background(), "19"); err != nil || ok {
		t.Fatalf("unknown definition mutated quest record ok=%t err=%v", ok, err)
	}
}

func TestCurrentAcceptQuestKnownIneligibleBeforeInitialTownTransitionSendsNoFailure(t *testing.T) {
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Character.Save(context.Background(), dnfrepo.CharacterRecord{
		CharacterID: "19", AccountID: defaultAccountPrefix + "1", Job: "2", Level: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Quest.Save(context.Background(), dnfrepo.QuestRecord{
		CharacterID: "19",
		States: map[int64]dnfrepo.QuestState{
			3145: {Status: "completed"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	connection := &bufferConn{}
	service := &Service{
		options:            options{gameUpperBodyCodec: gameUpperBodyCodecPlain},
		questCatalog:       buildAcceptQuestTestCatalog(t, false),
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
	}
	session := &gameSession{
		conn:                        connection,
		selectedCharacterID:         19,
		initialTownRouteCharacterID: 19,
		initialTownRouteStage:       currentInitialTownRouteArmed,
	}
	if err := service.handleCurrentAcceptQuest(session, []byte{0x1f, 0x00, 0x49, 0x0c}); err != nil {
		t.Fatal(err)
	}
	if connection.write.Len() != 0 {
		t.Fatalf("initial-town automatic ineligible accept emitted packet=%x", connection.write.Bytes())
	}
	stored, ok, err := repositories.Quest.Load(context.Background(), "19")
	if err != nil || !ok || stored.States[3145].Status != "completed" {
		t.Fatalf("initial-town ineligible quest state changed ok=%t err=%v state=%+v", ok, err, stored.States[3145])
	}
}

func TestCurrentAcceptQuestEligibleBeforeInitialTownTransitionDoesNotMutateOrReply(t *testing.T) {
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Character.Save(context.Background(), dnfrepo.CharacterRecord{
		CharacterID: "19",
		AccountID:   defaultAccountPrefix + "1",
		Job:         "2",
		Level:       1,
		Stats:       map[string]int64{"grow_type": 0},
	}); err != nil {
		t.Fatal(err)
	}
	connection := &bufferConn{}
	service := &Service{
		options:            options{gameUpperBodyCodec: gameUpperBodyCodecPlain},
		questCatalog:       buildAcceptQuestTestCatalog(t, false),
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
	}
	session := &gameSession{
		conn:                        connection,
		selectedCharacterID:         19,
		initialTownRouteCharacterID: 19,
		initialTownRouteStage:       currentInitialTownRouteArmed,
	}
	if err := service.handleCurrentAcceptQuest(session, []byte{0x1f, 0x00, 0x49, 0x0c}); err != nil {
		t.Fatal(err)
	}
	if connection.write.Len() != 0 {
		t.Fatalf("initial-town automatic eligible accept emitted packet=%x", connection.write.Bytes())
	}
	if _, ok, err := repositories.Quest.Load(context.Background(), "19"); err != nil || ok {
		t.Fatalf("initial-town automatic eligible accept mutated quest record ok=%t err=%v", ok, err)
	}
}

func TestCurrentAcceptQuestEligibleOnColdFirstLoginBeforeSceneRouteDoesNotMutateOrReply(t *testing.T) {
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Character.Save(context.Background(), dnfrepo.CharacterRecord{
		CharacterID: "21",
		AccountID:   defaultAccountPrefix + "1",
		Job:         "2",
		Level:       1,
		Stats:       map[string]int64{"grow_type": 0},
	}); err != nil {
		t.Fatal(err)
	}
	connection := &bufferConn{}
	service := &Service{
		options:            options{gameUpperBodyCodec: gameUpperBodyCodecPlain},
		questCatalog:       buildAcceptQuestTestCatalog(t, false),
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
	}
	// A cold first-login can emit the passive op31 before either a town or a
	// tutorial/dungeon scene route has reached actor-ready state.
	session := &gameSession{conn: connection, selectedCharacterID: 21}
	if err := service.handleCurrentAcceptQuest(session, []byte{0x1f, 0x00, 0x49, 0x0c}); err != nil {
		t.Fatal(err)
	}
	if connection.write.Len() != 0 {
		t.Fatalf("cold first-login passive accept emitted packet=%x", connection.write.Bytes())
	}
	if _, ok, err := repositories.Quest.Load(context.Background(), "21"); err != nil || ok {
		t.Fatalf("cold first-login passive accept mutated quest record ok=%t err=%v", ok, err)
	}
}

func buildAcceptQuestTestCatalog(t *testing.T, eventItem bool) *dnfquest.Catalog {
	t.Helper()
	extra := "[type]\n`[clear map]`\n[int data]\n76126\n"
	if eventItem {
		extra += "[depend give item]\n1001 1\n"
	}
	source := questListTestSource{
		dnfquest.DefaultList: "3145 `accept.qst`\n",
		"n_quest/accept.qst": "[grade]\n`[epic]`\n[level]\n1 99\n[job]\n`[gunner]`\n[exposed by npc]\n1\n" + extra,
	}
	index, err := dnfpvf.Build(context.Background(), source, dnfpvf.BuildOptions{Lists: []string{dnfquest.DefaultList}})
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := dnfquest.Load(context.Background(), index)
	if err != nil {
		t.Fatal(err)
	}
	return catalog
}
