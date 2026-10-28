package dnfbridge

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"reflect"
	"testing"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	"longheng.io/server/internal/modules/dnf/channelcatalog"
	dnfcollectbox "longheng.io/server/internal/modules/dnf/collectbox"
	"longheng.io/server/internal/modules/dnf/dnfenum"
	"longheng.io/server/internal/modules/dnf/dungeoncmd"
	dnfproto "longheng.io/server/internal/modules/dnf/protocol"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
	"longheng.io/server/internal/modules/dnf/worldmap"
)

func TestHandleDungeonBackToVillageUsesFullTownLifecycle(t *testing.T) {
	service, session, runtime := newBackToVillageRuntime(t)
	service.markCurrentTownSceneReady(session)
	session.enterSelectDungeonSent = true
	session.enterSelectDungeonAckSent = true
	session.enterSelectDungeonContextSent = true
	session.runtimeAfterBlacklistSent = true
	session.runtimeFinishLoadingGateSent = true
	session.fpsFinishLoadingGateSent = true
	session.selectPreviewActorRemoved = true
	session.preDungeonContextPlayerStateSent = true
	session.currentFinishLoadingStateSent = true
	session.currentFinishLoadingCompletionSent = true
	session.postFinishLoadingPlayerStateSent = true

	request, err := dnfproto.BuildChannelPacket(
		uint16(dnfenum.CmdPacketBack2Village), nil, 0, dnfproto.DefaultChannelClassification,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.handleGameUpper(session, request); err != nil {
		t.Fatalf("handle op132: %v", err)
	}
	// Interrupted return never replays the completed bitmap: the client still
	// owns the pre-entry quest state (reference sendCurrentDungeonReturnToTownLocked),
	// and the exact route below pins the no-quest-packet ordering.
	if got := townPostTransitionPacketSignatures(t, session.conn.(*bufferConn).write.Bytes()); !reflect.DeepEqual(
		got,
		[]string{"op42", "op376", "mode0", "mode1", "op205", "op24", "op559", "op33weakness"},
	) {
		t.Fatalf("full interrupted return route=%v", got)
	}
	if status := runtime.Session.Snapshot().Run.Status; status != worldmap.DungeonRunAbandoned {
		t.Fatalf("interrupted run status = %s, want %s", status, worldmap.DungeonRunAbandoned)
	}
	if session.dungeon.runtime != nil || !runtime.townReturnPending || !runtime.townReturnOp24Sent {
		t.Fatalf("full return retained runtime/state: owner=%p state=%+v", session.dungeon.runtime, runtime)
	}
	if !session.returnSelectActorStateEventSuppressionPending {
		t.Fatal("event-suppressed dungeon return did not arm the next op7 replacement actor policy")
	}
	if session.enterSelectDungeonSent || session.enterSelectDungeonAckSent || session.enterSelectDungeonContextSent {
		t.Fatal("full return retained the dungeon selector context")
	}
	if session.runtimeAfterBlacklistSent || session.runtimeFinishLoadingGateSent || session.fpsFinishLoadingGateSent ||
		session.selectPreviewActorRemoved || session.preDungeonContextPlayerStateSent ||
		session.currentFinishLoadingStateSent || session.currentFinishLoadingCompletionSent ||
		session.postFinishLoadingPlayerStateSent {
		t.Fatal("full return retained the old dungeon actor generation gates")
	}
}

func TestDungeonSceneGateResetRearmsCollectboxProjection(t *testing.T) {
	session := &gameSession{collectboxSceneProjectionSent: true}
	resetDungeonEntrySceneGates(session)
	if session.collectboxSceneProjectionSent {
		t.Fatal("dungeon scene reset retained the preceding actor generation's collection-box projection")
	}
	session.collectboxSceneProjectionSent = true
	resetDungeonReturnSceneGates(session)
	if session.collectboxSceneProjectionSent {
		t.Fatal("confirmed dungeon return retained the dungeon actor generation's collection-box projection")
	}
}

func TestDungeonReturnReplaysOrcGuardianCollectboxProjection(t *testing.T) {
	repositories := dnfrepomemory.NewMemoryGroup()
	state := dnfcollectbox.State{Boxes: map[string]map[string]uint32{"5": {"0": 9005}}}
	raw, err := dnfcollectbox.EncodeState(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := repositories.Account.Save(context.Background(), dnfrepo.AccountRecord{
		AccountID: "account-return-collectbox",
		Metadata:  map[string]string{dnfcollectbox.MetadataKey: raw},
	}); err != nil {
		t.Fatal(err)
	}
	service := &Service{
		collectboxCatalog: &currentCollectboxCatalog{
			byIndex: map[byte]dnfcollectbox.Definition{
				1: {Index: 1, BaseExpirationDate: "2037-01-05 06:00:00", Slots: []dnfcollectbox.Slot{{ItemID: 9001, Count: 1}}},
				5: {Index: 5, BaseExpirationDate: "2037-01-05 06:00:00", Slots: []dnfcollectbox.Slot{{ItemID: 9005, Count: 1}}},
			},
			indexes: []byte{1, 5},
		},
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
	}
	session := &gameSession{
		conn:                          &bufferConn{},
		accountID:                     "account-return-collectbox",
		selectedCharacterID:           29,
		collectboxSceneProjectionSent: true,
	}
	resetDungeonReturnSceneGates(session)
	if err := service.sendCollectboxSceneInitializationStates(context.Background(), session); err != nil {
		t.Fatal(err)
	}
	stream := session.conn.(*bufferConn).write.Bytes()
	for _, wantIndex := range []byte{1, 5} {
		packet, rest := splitGameServerUpperPacket(t, stream)
		stream = rest
		if packet.Header.Classification != 0 || packet.Header.MsgID != collectboxNotificationMsgID ||
			len(packet.Body) < 8 || packet.Body[0] != wantIndex || packet.Body[1] != 0 {
			t.Fatalf("return cache index=%d header=%+v body=%x", wantIndex, packet.Header, packet.Body)
		}
	}
	selection, stream := splitGameServerUpperPacket(t, stream)
	if selection.Header.Classification != 1 || selection.Header.MsgID != uint16(dnfenum.CmdPacketSelectCollectbox) ||
		!bytes.Equal(selection.Body, []byte{1, defaultCollectboxIndex}) {
		t.Fatalf("return selection header=%+v body=%x", selection.Header, selection.Body)
	}
	selected, stream := splitGameServerUpperPacket(t, stream)
	if selected.Header.Classification != 0 || selected.Header.MsgID != collectboxNotificationMsgID ||
		len(selected.Body) < 12 || selected.Body[0] != defaultCollectboxIndex || selected.Body[1] != 1 ||
		binary.LittleEndian.Uint32(selected.Body[8:12]) != 9005 || len(stream) != 0 {
		t.Fatalf("return selected header=%+v body=%x trailing=%x", selected.Header, selected.Body, stream)
	}
	if !session.collectboxSceneProjectionSent || session.selectedCollectboxIndex != defaultCollectboxIndex {
		t.Fatalf("return collect-box projection sent=%t selected=%d", session.collectboxSceneProjectionSent, session.selectedCollectboxIndex)
	}
}

func TestConfirmedActiveTutorialExitPersistsBeforeImmediateCharacterSwitch(t *testing.T) {
	for _, fixture := range []struct {
		name string
		exit func(*Service, *gameSession) error
	}{
		{
			name: "in-dungeon give-up confirmation op42",
			exit: func(service *Service, session *gameSession) error {
				return service.handleDungeonGiveupGame(session, nil)
			},
		},
		{
			name: "system back-to-village confirmation op132",
			exit: func(service *Service, session *gameSession) error {
				return service.handleDungeonBackToVillage(session, nil)
			},
		},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			service, runtime := prepareTutorialScopeRuntime(t, tutorialScopeFixtureOptions{})
			connection := &bufferConn{}
			session := &gameSession{
				conn:                connection,
				connID:              "tutorial-abandon-return-test",
				selectedCharacterID: 99,
				dungeon:             dungeonSessionState{runtime: runtime},
			}

			if err := fixture.exit(service, session); err != nil {
				t.Fatalf("handle active tutorial exit: %v", err)
			}

			if got := townPostTransitionPacketSignatures(t, connection.write.Bytes()); !reflect.DeepEqual(
				got,
				// No op33weakness: this fixture's character never takes a
				// penalty, and a healthy character is deliberately not sent a
				// condition refresh (a 100 byte pops the client's "recovered"
				// message).
				[]string{"op42", "op376", "mode0", "mode1", "op205", "op24", "op559"},
			) {
				t.Fatalf("tutorial abandon full route=%v", got)
			}
			if !runtime.tutorialCompletionPersisted ||
				!hasPersistedDungeonTutorialCompletion(runtime.Character) ||
				!runtime.townReturnPending ||
				!runtime.townReturnOp24Sent ||
				session.dungeon.runtime != nil {
				t.Fatalf("tutorial abandon runtime=%+v", runtime)
			}
			if !session.returnSelectActorStateEventSuppressionPending {
				t.Fatal("tutorial return did not retain the event-suppressed actor lifecycle")
			}
			repositories, ok := service.repositoryGroup()
			if !ok || repositories.Character == nil {
				t.Fatal("tutorial abandon character repository unavailable")
			}
			character, found, err := repositories.Character.Load(context.Background(), "99")
			if err != nil || !found {
				t.Fatalf("load tutorial abandon character found=%t err=%v", found, err)
			}
			if !hasPersistedDungeonTutorialCompletion(character) ||
				character.Stats["town_id"] != newCharacterInitialTownID ||
				character.Stats["area_id"] != newCharacterInitialAreaID {
				t.Fatalf("tutorial abandon character stats=%+v", character.Stats)
			}

			// The complete interrupted-run route has already retired the dungeon;
			// an immediate character switch must not find a second run to abandon.
			previousCharacterID, abandoned := service.resetGameSessionForCharacterSelect(session)
			if previousCharacterID != 99 || abandoned || session.dungeon.runtime != nil {
				t.Fatalf("immediate character switch char=%d abandoned=%t runtime=%+v",
					previousCharacterID, abandoned, session.dungeon.runtime)
			}
			if !session.returnSelectActorStateEventSuppressionPending {
				t.Fatal("op7 reset discarded the event-suppressed actor lifecycle")
			}
			character, found, err = repositories.Character.Load(context.Background(), "99")
			if err != nil || !found || !hasPersistedDungeonTutorialCompletion(character) {
				t.Fatalf("tutorial completion lost after character switch found=%t stats=%+v err=%v",
					found, character.Stats, err)
			}
		})
	}
}

func TestHandleDungeonBackToVillagePreservesCompletedFinalStatus(t *testing.T) {
	service, session, runtime := newBackToVillageRuntime(t)
	if err := runtime.Session.CompleteCurrentRoom(); err != nil {
		t.Fatalf("complete final room: %v", err)
	}
	if err := service.handleDungeonBackToVillage(session, nil); err != nil {
		t.Fatalf("handle completed op132: %v", err)
	}

	if got := townPostTransitionPacketSignatures(t, session.conn.(*bufferConn).write.Bytes()); !reflect.DeepEqual(
		got,
		// A completed run never weakens, so no condition refresh follows.
		[]string{"op376", "mode0", "mode1", "op205", "op24", "op559", "op42"},
	) {
		t.Fatalf("completed return route=%v", got)
	}
	if status := runtime.Session.Snapshot().Run.Status; status != worldmap.DungeonRunCompleted {
		t.Fatalf("completed return status=%s want=%s", status, worldmap.DungeonRunCompleted)
	}
	if session.dungeon.runtime != nil {
		t.Fatalf("completed return retained runtime=%+v", session.dungeon.runtime)
	}
	if status := runtime.Session.Snapshot().Run.Status; status != worldmap.DungeonRunCompleted {
		t.Fatalf("confirmed completed return status=%s want completed", status)
	}
}

func TestConfirmedHellPartyReturnDoesNotInventEmptyMonsterInfo(t *testing.T) {
	service, session, _ := newBackToVillageRuntime(t)
	if err := service.handleDungeonBackToVillage(session, nil); err != nil {
		t.Fatal(err)
	}
	connection := session.conn.(*bufferConn)
	if got := townPostTransitionPacketSignatures(t, connection.write.Bytes()); !reflect.DeepEqual(
		got,
		[]string{"op42", "op376", "mode0", "mode1", "op205", "op24", "op559", "op33weakness"},
	) {
		t.Fatalf("Hell Party return state order=%v", got)
	}
	completeLen := connection.write.Len()
	if err := service.commitPendingDungeonReturnForSceneRequest(session, "test_hell_party_return_duplicate"); err != nil {
		t.Fatal(err)
	}
	if connection.write.Len() != completeLen {
		t.Fatalf("duplicate return replayed Hell Party clear=%x", connection.write.Bytes()[completeLen:])
	}
}

func TestCompletedDungeonReturnDoesNotReplayInitialTownPlayerInfoAfterTransition(t *testing.T) {
	service, session, runtime := newBackToVillageRuntime(t)
	repositories, ok := service.repositoryGroup()
	if !ok {
		t.Fatal("completed return repository group unavailable")
	}
	if err := repositories.Equipment.Save(context.Background(), dnfrepo.EquipmentRecord{
		CharacterID: "99",
		Entries: map[string]dnfrepo.EquipmentEntry{
			"11": {
				SlotIndex: 11,
				ItemID:    900001,
				RawEntry:  buildInitialEquipmentRawEntry(11, 900001, 27),
			},
		},
	}); err != nil {
		t.Fatalf("save completed-return equipment: %v", err)
	}
	enableTownMoveSkillProjection(t, service, repositories, "99")
	session.channel.ID = 253
	session.channel.Name = "ch.253"
	session.channel.Port = 10253
	session.residentChannel = session.channel
	session.connectionTownActorOwnerChannel = 253
	session.townActorOwnerChannel = currentSceneObjectContext
	if err := runtime.Session.CompleteCurrentRoom(); err != nil {
		t.Fatalf("complete final room: %v", err)
	}
	// The tower identity is derived from the PVF dungeon section/flag and the
	// clear window flag now; there is no per-run tower progress record.
	if runtime.Dungeon.Metadata.Flags == nil {
		runtime.Dungeon.Metadata.Flags = make(map[string]bool)
	}
	runtime.Dungeon.Metadata.Flags["tower of despair"] = true
	runtime.settlementTowerRewardSent = true
	transition, err := service.prepareCurrentDungeonTownTransition(context.Background(), session.selectedCharacterID)
	if err != nil {
		t.Fatal(err)
	}
	connection := session.conn.(*bufferConn)
	connection.write.Reset()

	if err := service.sendCurrentCompletedDungeonReturnToTown(
		session,
		runtime,
		transition,
		uint16(dnfenum.CmdPacketEplpCommand),
		"test_card_exit_return_to_town",
		false,
	); err != nil {
		t.Fatalf("completed dungeon town route: %v", err)
	}
	assertDungeonTownTransitionSequencePresent(t, connection.write.Bytes())
	if session.initialTownRouteStage != currentInitialTownRoutePlayerStateSent ||
		!session.sceneBootstrapTailDeferred ||
		!session.selectedUserInfoRefreshSent ||
		session.selectedUserInfoMode3Sent {
		t.Fatalf("completed return flags stage=%d tail_deferred=%t mode1=%t mode3=%t",
			session.initialTownRouteStage,
			session.sceneBootstrapTailDeferred,
			session.selectedUserInfoRefreshSent,
			session.selectedUserInfoMode3Sent)
	}
	if containsClass0Op2ModeForTest(t, connection.write.Bytes(), 3) {
		t.Fatalf("completed dungeon return route replayed mode3 before town transition: %x", connection.write.Bytes())
	}
	targetOwner := byte(253)
	mode0Count := 0
	mode1Count := 0
	expertJobCount := 0
	for ownerStream := connection.write.Bytes(); len(ownerStream) > 0; {
		packet, next := splitCurrentGameServerUpperPacketAuto(t, ownerStream)
		ownerStream = next
		if packet.Header.Classification == 0 && packet.Header.MsgID == currentDespairTowerClearRewardMsgID {
			t.Fatalf("completed town route published tower selector state outside request-driven op15/op27 body=%x", packet.Body)
		}
		if packet.Header.Classification == 0 && packet.Header.MsgID == currentSkillInfoMsgID {
			t.Fatalf("completed dungeon return replayed forbidden skill acquisition state op%d body=%x", packet.Header.MsgID, packet.Body)
		}
		if packet.Header.Classification == 0 && packet.Header.MsgID == currentExpertJobInfoNotification {
			expertJobCount++
			if !bytes.Equal(packet.Body, currentExpertJobClearedBody(0)) {
				t.Fatalf("completed dungeon return state-only op205 body=%x", packet.Body)
			}
			continue
		}
		if packet.Header.Classification != 0 ||
			packet.Header.MsgID != uint16(dnfenum.CmdPacketSetUDPIPPort) ||
			len(packet.Body) < 5 {
			continue
		}
		switch packet.Body[0] {
		case 0:
			mode0Count++
			if eventArg, ok := currentSceneObjectTailActorStateEventArgForLog(packet.Body); !ok || eventArg != 0 {
				t.Fatalf("completed dungeon return mode0 actor-state event arg=%d ok=%t, want suppressed zero", eventArg, ok)
			}
		case 1:
			mode1Count++
			if len(packet.Body) <= currentMode1CreateRowsOffset+5 ||
				packet.Body[currentMode1CreateCountOffset] != 1 ||
				binary.LittleEndian.Uint32(
					packet.Body[currentMode1CreateRowsOffset+1:currentMode1CreateRowsOffset+5],
				) != 900001 {
				t.Fatalf("completed dungeon return mode1 missing equipped-item create row body=%x", packet.Body)
			}
		default:
			continue
		}
		if packet.Body[3] != 0 || packet.Body[4] != targetOwner {
			t.Fatalf(
				"completed dungeon return mode%d owner=%x want 00/%02x",
				packet.Body[0],
				packet.Body[:5],
				targetOwner,
			)
		}
	}
	if expertJobCount != 1 {
		t.Fatalf("completed dungeon return expert-job state count=%d want=1", expertJobCount)
	}
	if mode0Count != 1 || mode1Count != 1 ||
		session.townActorOwnerChannel != targetOwner {
		t.Fatalf(
			"completed dungeon return owner packets mode0=%d mode1=%d owner=%d want 1/1/%d",
			mode0Count,
			mode1Count,
			session.townActorOwnerChannel,
			targetOwner,
		)
	}

	connection.write.Reset()
	if err := service.sendDeferredSelectSceneTail(session, "test_after_town_transition"); err != nil {
		t.Fatalf("deferred scene tail after completed return: %v", err)
	}
	if session.sceneBootstrapTailSent || !session.sceneBootstrapTailDeferred ||
		connection.write.Len() != 0 {
		t.Fatalf(
			"pre-type1345 deferred tail sent=%t deferred=%t bytes=%x",
			session.sceneBootstrapTailSent,
			session.sceneBootstrapTailDeferred,
			connection.write.Bytes(),
		)
	}
	if session.townPostTransition.stage != currentTownPostTransitionIdle {
		t.Fatalf("completed return pre-type1345 stage=%d want idle", session.townPostTransition.stage)
	}

	if err := service.handleLegacyGamePacket(session, buildLegacyTownSceneReadyAckPacket()); err != nil {
		t.Fatalf("completed return type1345 boundary: %v", err)
	}
	if !session.sceneBootstrapTailSent || session.sceneBootstrapTailDeferred ||
		session.townPostTransition.stage != currentTownPostTransitionIdle {
		t.Fatalf(
			"post-type1345 tail sent=%t deferred=%t stage=%d",
			session.sceneBootstrapTailSent,
			session.sceneBootstrapTailDeferred,
			session.townPostTransition.stage,
		)
	}
	if session.selectedUserInfoMode3Sent ||
		containsClass0Op2ModeForTest(t, connection.write.Bytes(), 3) ||
		containsInitialTownTransitionForTest(t, connection.write.Bytes()) {
		t.Fatalf("deferred tail replayed initial-town player info: mode3_sent=%t stream=%x",
			session.selectedUserInfoMode3Sent,
			connection.write.Bytes())
	}
}

func TestCompletedDungeonReturnReappliesEquippedSkinAfterMode1(t *testing.T) {
	service, session, runtime := newBackToVillageRuntime(t)
	repositories, ok := service.repositoryGroup()
	if !ok {
		t.Fatal("completed return repository group unavailable")
	}
	const skinItemID = uint32(112580095)
	if err := repositories.Equipment.Save(context.Background(), dnfrepo.EquipmentRecord{
		CharacterID: "99",
		Entries: map[string]dnfrepo.EquipmentEntry{
			"8": {
				SlotIndex: 8,
				ItemID:    int64(skinItemID),
				RawEntry:  buildInitialEquipmentRawEntry(8, int64(skinItemID), 8),
			},
		},
	}); err != nil {
		t.Fatalf("save completed-return skin avatar: %v", err)
	}
	if err := runtime.Session.CompleteCurrentRoom(); err != nil {
		t.Fatalf("complete final room: %v", err)
	}
	transition, err := service.prepareCurrentDungeonTownTransition(context.Background(), session.selectedCharacterID)
	if err != nil {
		t.Fatal(err)
	}
	connection := session.conn.(*bufferConn)
	connection.write.Reset()
	if err := service.sendCurrentCompletedDungeonReturnToTown(
		session,
		runtime,
		transition,
		uint16(dnfenum.CmdPacketEplpCommand),
		"test_completed_return_skin_avatar",
		false,
	); err != nil {
		t.Fatalf("completed dungeon town route: %v", err)
	}

	baseMode0Index, mode1Index, skinRefreshMode0Index := -1, -1, -1
	buffIndex, transitionIndex := -1, -1
	for index, packet := range splitAllCurrentUpperPackets(t, connection.write.Bytes()) {
		switch {
		case packet.Header.Classification == 0 && packet.Header.MsgID == currentSkillInfoMsgID:
			t.Fatalf("completed return replayed op19 skill snapshot body=%x", packet.Body)
		case packet.Header.Classification == 0 &&
			packet.Header.MsgID == uint16(dnfenum.CmdPacketSetUDPIPPort) &&
			len(packet.Body) > 0 && packet.Body[0] == 1:
			mode1Index = index
			if len(packet.Body) <= currentMode1CreateRowsOffset+5 ||
				packet.Body[currentMode1CreateCountOffset] != 1 ||
				packet.Body[currentMode1CreateRowsOffset] != 8 ||
				binary.LittleEndian.Uint32(packet.Body[currentMode1CreateRowsOffset+1:currentMode1CreateRowsOffset+5]) != skinItemID {
				t.Fatalf("completed return mode1 missing skin-avatar create row body=%x", packet.Body)
			}
		case packet.Header.Classification == 0 &&
			packet.Header.MsgID == uint16(dnfenum.CmdPacketSetUDPIPPort) &&
			len(packet.Body) > 0 && packet.Body[0] == 0:
			eventArg, eventOK := currentSceneObjectTailActorStateEventArgForLog(packet.Body)
			projectedSkin, skinOK := currentSceneObjectAppearanceSlotItemID(packet.Body, 8)
			if !eventOK || eventArg != 0 || !skinOK || projectedSkin != skinItemID {
				t.Fatalf("completed return mode0 skin row event=%d ok=%t item=%d skin_ok=%t want=%d", eventArg, eventOK, projectedSkin, skinOK, skinItemID)
			}
			if baseMode0Index < 0 {
				baseMode0Index = index
			} else {
				skinRefreshMode0Index = index
			}
		case packet.Header.Classification == 0 && packet.Header.MsgID == skillSwitchInventoryAllDataWire:
			buffIndex = index
		case packet.Header.Classification == 0 && packet.Header.MsgID == currentSceneTransitionMsgID:
			transitionIndex = index
		}
	}
	if !(baseMode0Index >= 0 && baseMode0Index < mode1Index &&
		mode1Index < skinRefreshMode0Index && skinRefreshMode0Index < transitionIndex && transitionIndex < buffIndex) {
		t.Fatalf("completed-return skin order base_mode0=%d mode1=%d skin_mode0=%d buff=%d op24=%d", baseMode0Index, mode1Index, skinRefreshMode0Index, buffIndex, transitionIndex)
	}
}

func TestCompletedDungeonReturnRestoresDurableBuffRegistrationAfterTownTransition(t *testing.T) {
	service, session, runtime := newBackToVillageRuntime(t)
	repositories, ok := service.repositoryGroup()
	if !ok {
		t.Fatal("completed return repositories unavailable")
	}
	ctx := context.Background()
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "99",
		Slots: map[string]dnfrepo.ItemStack{
			"0:17": {ItemID: 101010864, Count: 1, Extra: map[string]string{"item_serial": "20"}},
		},
		Warehouse: map[string]dnfrepo.ItemStack{},
	}); err != nil {
		t.Fatalf("save completed-return BUFF inventory: %v", err)
	}
	if err := saveSkillSwitchInventoryState(ctx, repositories.Settings, "99", skillSwitchInventoryState{
		SkillID: 46,
		Items:   []skillSwitchBinding{{EquipmentPart: 12, ItemSerial: 20, ItemID: 101010864}},
	}); err != nil {
		t.Fatalf("save completed-return BUFF registration: %v", err)
	}
	if err := runtime.Session.CompleteCurrentRoom(); err != nil {
		t.Fatalf("complete final room: %v", err)
	}
	transition, err := service.prepareCurrentDungeonTownTransition(ctx, session.selectedCharacterID)
	if err != nil {
		t.Fatal(err)
	}
	connection := session.conn.(*bufferConn)
	connection.write.Reset()
	if err := service.sendCurrentCompletedDungeonReturnToTown(
		session,
		runtime,
		transition,
		uint16(dnfenum.CmdPacketEplpCommand),
		"test_completed_return_buff_registration",
		false,
	); err != nil {
		t.Fatal(err)
	}

	mode1Index, buffIndex, transitionIndex := -1, -1, -1
	for index, packet := range splitAllCurrentUpperPackets(t, connection.write.Bytes()) {
		switch {
		case packet.Header.Classification == 0 && packet.Header.MsgID == currentSkillInfoMsgID:
			t.Fatalf("completed return replayed op19 skill snapshot body=%x", packet.Body)
		case packet.Header.Classification == 0 &&
			packet.Header.MsgID == uint16(dnfenum.CmdPacketSetUDPIPPort) &&
			len(packet.Body) > 0 && packet.Body[0] == 1:
			mode1Index = index
		case packet.Header.Classification == 0 && packet.Header.MsgID == skillSwitchInventoryAllDataWire:
			buffIndex = index
			if want := []byte{46, 0, 1, 1, 12, 0, 17, 0}; !bytes.Equal(packet.Body, want) {
				t.Fatalf("completed-return BUFF registration=%x want=%x", packet.Body, want)
			}
		case packet.Header.Classification == 0 && packet.Header.MsgID == currentSceneTransitionMsgID:
			transitionIndex = index
		}
	}
	if !(mode1Index >= 0 && mode1Index < transitionIndex && transitionIndex < buffIndex) {
		t.Fatalf("completed-return BUFF order mode1=%d buff=%d op24=%d", mode1Index, buffIndex, transitionIndex)
	}
}

func TestCompletedDungeonReturnRefreshesQuestSnapshotsBeforeAndAfterTransition(t *testing.T) {
	service, session, runtime := newBackToVillageRuntime(t)
	service.questCatalog = buildQuestListTestCatalog(t)
	if err := runtime.Session.CompleteCurrentRoom(); err != nil {
		t.Fatalf("complete final room: %v", err)
	}
	transition, err := service.prepareCurrentDungeonTownTransition(context.Background(), session.selectedCharacterID)
	if err != nil {
		t.Fatal(err)
	}
	connection := session.conn.(*bufferConn)
	connection.write.Reset()

	if err := service.sendCurrentCompletedDungeonReturnToTown(
		session,
		runtime,
		transition,
		uint16(dnfenum.CmdPacketEplpCommand),
		"test_completed_return_quest_refresh",
		false,
	); err != nil {
		t.Fatalf("completed dungeon town route: %v", err)
	}

	var beforeTransitionAcceptable, beforeTransitionActive int
	var afterTransitionAcceptable, afterTransitionActive int
	transitionSeen := false
	stream := connection.write.Bytes()
	for len(stream) > 0 {
		packet, rest := splitCurrentGameServerUpperPacketAuto(t, stream)
		switch {
		case packetLooksLikeInitialTownTransition(packet):
			transitionSeen = true
		case packet.Header.Classification == 0 && packet.Header.MsgID == currentAcceptableQuestListMsgID:
			if transitionSeen {
				afterTransitionAcceptable++
			} else {
				beforeTransitionAcceptable++
			}
		case packet.Header.Classification == 0 && packet.Header.MsgID == currentActiveQuestSnapshotMsgID:
			if transitionSeen {
				afterTransitionActive++
			} else {
				beforeTransitionActive++
			}
		}
		stream = rest
	}
	if !transitionSeen {
		t.Fatal("completed dungeon return did not emit typed op24")
	}
	if beforeTransitionAcceptable != 1 || beforeTransitionActive != 1 ||
		afterTransitionAcceptable != 1 || afterTransitionActive != 1 {
		t.Fatalf(
			"quest refresh counts before op24=(op21:%d op574:%d) after op24=(op21:%d op574:%d)",
			beforeTransitionAcceptable,
			beforeTransitionActive,
			afterTransitionAcceptable,
			afterTransitionActive,
		)
	}
}

func TestCompletedDungeonReturnRehydratesGuardianGemBeforeTownTransition(t *testing.T) {
	service, session, runtime := newBackToVillageRuntime(t)
	repositories, ok := service.repositoryGroup()
	if !ok || repositories.Equipment == nil {
		t.Fatal("completed return fixture equipment repository unavailable")
	}
	medalRaw := currentGuardianGemTestRaw(32, 100380017, 1)
	binary.LittleEndian.PutUint16(medalRaw[currentGuardianGemRawSocketOffset:currentGuardianGemRawSocketOffset+2], 3)
	if err := repositories.Equipment.Save(context.Background(), dnfrepo.EquipmentRecord{
		CharacterID: "99",
		Entries: map[string]dnfrepo.EquipmentEntry{
			"32": {SlotIndex: 32, ItemID: 100380017, RawEntry: medalRaw},
		},
	}); err != nil {
		t.Fatalf("save completed-return guardian-gem medal: %v", err)
	}
	if err := runtime.Session.CompleteCurrentRoom(); err != nil {
		t.Fatalf("complete final room: %v", err)
	}
	transition, err := service.prepareCurrentDungeonTownTransition(context.Background(), session.selectedCharacterID)
	if err != nil {
		t.Fatal(err)
	}
	connection := session.conn.(*bufferConn)
	connection.write.Reset()
	if err := service.sendCurrentCompletedDungeonReturnToTown(
		session,
		runtime,
		transition,
		uint16(dnfenum.CmdPacketEplpCommand),
		"test_completed_return_guardian_gem",
		false,
	); err != nil {
		t.Fatal(err)
	}

	sawMode1 := false
	sawGuardianGem := false
	stream := connection.write.Bytes()
	for len(stream) > 0 {
		packet, rest := splitCurrentGameServerUpperPacketAuto(t, stream)
		if packet.Header.MsgID == uint16(dnfenum.CmdPacketSetUDPIPPort) && len(packet.Body) > 0 && packet.Body[0] == 1 {
			sawMode1 = true
		}
		if packet.Header.MsgID == uint16(dnfenum.CmdPacketWalkoutPartyMember) &&
			len(packet.Body) == 3+currentItemListEntryWireSize &&
			packet.Body[0] == currentSocketListEquipment &&
			binary.LittleEndian.Uint16(packet.Body[3:5]) == uint16(currentGuildMedalActorSlot) {
			if !sawMode1 {
				t.Fatal("completed-return guardian-gem op14 preceded mode1 actor bind")
			}
			if !currentGuardianGemRawSocketOccupied(packet.Body[3:]) {
				t.Fatalf("completed-return guardian-gem op14 omitted raw socket state: %x", packet.Body)
			}
			sawGuardianGem = true
		}
		stream = rest
	}
	if !sawMode1 || !sawGuardianGem {
		t.Fatalf("completed-return mode1/guardian refresh mode1=%t guardian=%t", sawMode1, sawGuardianGem)
	}
}

func TestFullInterruptedDungeonReturnRejectsLateMonsterDeathFromRetiredRun(t *testing.T) {
	service, session, runtime := newBackToVillageRuntime(t)
	if _, err := runtime.Room.AnnounceAllActors(runtime.Session); err != nil {
		t.Fatal(err)
	}
	room := runtime.Room.Snapshot()
	if len(room.Monsters) == 0 {
		t.Fatal("return fixture has no monster")
	}
	if err := service.handleDungeonBackToVillage(session, nil); err != nil {
		t.Fatalf("full interrupted return: %v", err)
	}
	conn := session.conn.(*bufferConn)
	conn.write.Reset()
	deathBody := make([]byte, dungeoncmd.DieMonsterRequestSize)
	binary.LittleEndian.PutUint32(deathBody[0:4], room.Monsters[0].ObjectKey)
	binary.LittleEndian.PutUint16(deathBody[4:6], session.selectedCharacterID)
	if err := service.handleDungeonMonsterDeath(session, deathBody); err != nil {
		t.Fatalf("monster death after retired return: %v", err)
	}
	if conn.write.Len() != 0 {
		t.Fatalf("late monster death wrote packets=%x", conn.write.Bytes())
	}
	if session.dungeon.runtime != nil || runtime.Session.Snapshot().Run.Status != worldmap.DungeonRunAbandoned {
		t.Fatalf("late death resurrected run owner=%p status=%s",
			session.dungeon.runtime, runtime.Session.Snapshot().Run.Status)
	}
}

func TestActiveDungeonReturnCommitsWithoutTownSideSceneRequest(t *testing.T) {
	service, session, runtime := newBackToVillageRuntime(t)
	if err := service.handleDungeonBackToVillage(session, nil); err != nil {
		t.Fatal(err)
	}
	connection := session.conn.(*bufferConn)
	if got := townPostTransitionPacketSignatures(t, connection.write.Bytes()); !reflect.DeepEqual(
		got,
		[]string{"op42", "op376", "mode0", "mode1", "op205", "op24", "op559", "op33weakness"},
	) {
		t.Fatalf("confirmed active return route=%v", got)
	}
	if session.dungeon.runtime != nil {
		t.Fatalf("confirmed return retained runtime=%+v", session.dungeon.runtime)
	}
	if status := runtime.Session.Snapshot().Run.Status; status != worldmap.DungeonRunAbandoned {
		t.Fatalf("confirmed active return status=%s want abandoned", status)
	}
}

func TestConfirmedDungeonReturnRehydratesGuardianGemAfterTownMode1(t *testing.T) {
	service, session, runtime := newBackToVillageRuntime(t)
	repositories, ok := service.repositoryGroup()
	if !ok || repositories.Equipment == nil {
		t.Fatal("dungeon return fixture equipment repository unavailable")
	}
	medalRaw := currentGuardianGemTestRaw(32, 100380017, 1)
	binary.LittleEndian.PutUint16(medalRaw[currentGuardianGemRawSocketOffset:currentGuardianGemRawSocketOffset+2], 3)
	if err := repositories.Equipment.Save(context.Background(), dnfrepo.EquipmentRecord{
		CharacterID: "99",
		Entries: map[string]dnfrepo.EquipmentEntry{
			"32": {SlotIndex: 32, ItemID: 100380017, RawEntry: medalRaw},
		},
	}); err != nil {
		t.Fatalf("save return guardian-gem medal: %v", err)
	}
	if err := service.handleDungeonBackToVillage(session, nil); err != nil {
		t.Fatalf("start dungeon return: %v", err)
	}
	connection := session.conn.(*bufferConn)

	sawMode1 := false
	sawGuardianGem := false
	stream := connection.write.Bytes()
	for len(stream) > 0 {
		packet, rest := splitCurrentGameServerUpperPacketAuto(t, stream)
		if packet.Header.MsgID == uint16(dnfenum.CmdPacketSetUDPIPPort) && len(packet.Body) > 0 && packet.Body[0] == 1 {
			sawMode1 = true
		}
		if packet.Header.MsgID == uint16(dnfenum.CmdPacketWalkoutPartyMember) &&
			len(packet.Body) == 3+currentItemListEntryWireSize &&
			packet.Body[0] == currentSocketListEquipment &&
			binary.LittleEndian.Uint16(packet.Body[3:5]) == uint16(currentGuildMedalActorSlot) {
			if !sawMode1 {
				t.Fatal("town guardian-gem op14 preceded mode1 actor rebind")
			}
			if !currentGuardianGemRawSocketOccupied(packet.Body[3:]) {
				t.Fatalf("town guardian-gem op14 omitted raw socket state: %x", packet.Body)
			}
			sawGuardianGem = true
		}
		stream = rest
	}
	if !sawMode1 || !sawGuardianGem {
		t.Fatalf("town mode1/guardian refresh mode1=%t guardian=%t", sawMode1, sawGuardianGem)
	}
	runtimeSnapshot := runtime.Session.Snapshot()
	if runtimeSnapshot.Run.Status != worldmap.DungeonRunAbandoned || session.dungeon.runtime != nil {
		t.Fatalf("confirmed return runtime status=%s owner=%p", runtimeSnapshot.Run.Status, session.dungeon.runtime)
	}
}

func TestInterruptedDungeonReturnRehydratesEquippedCreatureAfterMode1(t *testing.T) {
	service, session, _ := newBackToVillageRuntime(t)
	repositories, ok := service.repositoryGroup()
	if !ok || repositories.Equipment == nil || repositories.Pet == nil {
		t.Fatal("dungeon return fixture creature repositories unavailable")
	}
	const (
		itemID      = 400990167
		creatureKey = 37
		cardID      = 10008713
		upgrade     = 2
	)
	if err := repositories.Equipment.Save(context.Background(), dnfrepo.EquipmentRecord{
		CharacterID: "99",
		Entries: map[string]dnfrepo.EquipmentEntry{
			"26": {
				SlotIndex: 26,
				ItemID:    itemID,
				RawEntry:  testCurrentEquippedCreatureEnchantRaw(26, itemID, creatureKey, cardID, upgrade),
				Extra: map[string]string{
					"value_a":                  "10008713",
					"pet_enchant_card_item_id": "10008713",
					"byte_12":                  "2",
				},
			},
		},
	}); err != nil {
		t.Fatalf("save return equipped creature: %v", err)
	}
	if err := repositories.Pet.Save(context.Background(), dnfrepo.PetRecord{
		CharacterID: "99",
		Entries: map[string]dnfrepo.PetEntry{
			"37": {
				PetKey:          "37",
				CreatureKey:     creatureKey,
				ItemID:          itemID,
				Name:            "ReturnPet",
				NameRaw:         []byte("ReturnPet"),
				Satiety:         72,
				Level:           7,
				Exp:             0x12345678,
				SourceListType:  currentSocketListEquipment,
				SourceSlotIndex: 26,
			},
		},
		EquippedKey: "37",
	}); err != nil {
		t.Fatalf("save return creature state: %v", err)
	}

	if err := service.handleDungeonBackToVillage(session, nil); err != nil {
		t.Fatalf("start dungeon return: %v", err)
	}
	connection := session.conn.(*bufferConn)

	// Full mode1 recreates the slot-26 creature object, but the current EXE
	// takes the creature's card/enchant and dynamic state only from the
	// same-template list-3 op14 and the op105/op102 tables that follow it. An
	// interrupted (强制回城) return rebuilds the actor exactly like a completed
	// one, so it must rehydrate the creature the same way before the typed
	// op24; skipping it dropped the pet enchant until the next full login.
	if got := townPostTransitionPacketSignatures(t, connection.write.Bytes()); !reflect.DeepEqual(
		got,
		[]string{"op42", "op376", "mode0", "mode1", "op205", "slot26_op14", "op105", "op102", "op24", "op559", "op33weakness"},
	) {
		t.Fatalf("return creature route=%v", got)
	}
}

func TestInterruptedDungeonReturnWriteFailureResumesOnlyUnfinishedSuffix(t *testing.T) {
	service, session, runtime := newBackToVillageRuntime(t)
	wantErr := errors.New("full interrupted return op24 failed")
	connection := &failNthDungeonWriteConn{failAt: 6, err: wantErr}
	session.conn = connection
	if err := service.handleDungeonBackToVillage(session, nil); !errors.Is(err, wantErr) {
		t.Fatalf("full return failure=%v want=%v", err, wantErr)
	}
	if got := townPostTransitionPacketSignatures(t, connection.write.Bytes()); !reflect.DeepEqual(
		got,
		[]string{"op42", "op376", "mode0", "mode1", "op205"},
	) {
		t.Fatalf("full return prefix=%v", got)
	}
	// The creature stage now runs for interrupted returns too; without an
	// equipped creature it writes nothing but still advances the cursor, so the
	// retry resumes right before the typed op24 instead of replaying it.
	if session.dungeon.runtime != runtime ||
		runtime.Session.Snapshot().Run.Status != worldmap.DungeonRunAbandoned ||
		!runtime.townReturnPending ||
		session.initialTownRouteStage != currentInitialTownRouteCreatureStateSent {
		t.Fatalf(
			"failed full return owner=%p run=%s pending=%t stage=%d",
			session.dungeon.runtime,
			runtime.Session.Snapshot().Run.Status,
			runtime.townReturnPending,
			session.initialTownRouteStage,
		)
	}

	prefixLen := connection.write.Len()
	if err := service.handleDungeonBackToVillage(session, nil); err != nil {
		t.Fatalf("resume full return: %v", err)
	}
	if got := townPostTransitionPacketSignatures(t, connection.write.Bytes()[prefixLen:]); !reflect.DeepEqual(
		got,
		[]string{"op24", "op559", "op33weakness"},
	) {
		t.Fatalf("full return suffix=%v", got)
	}
	if session.dungeon.runtime != nil || !runtime.townReturnOp24Sent {
		t.Fatalf("resumed full return retained owner=%p runtime=%+v", session.dungeon.runtime, runtime)
	}
}

func TestInterruptedDungeonReturnEarlyGateFailureRetriesBeforeTownPackets(t *testing.T) {
	service, session, runtime := newBackToVillageRuntime(t)
	wantErr := errors.New("full interrupted return dungeon gate reset failed")
	connection := &failNthDungeonWriteConn{failAt: 1, err: wantErr}
	session.conn = connection
	if err := service.handleDungeonBackToVillage(session, nil); !errors.Is(err, wantErr) {
		t.Fatalf("early reset failure=%v want=%v", err, wantErr)
	}
	if got := townPostTransitionPacketSignatures(t, connection.write.Bytes()); len(got) != 0 {
		t.Fatalf("failed early reset emitted town packets=%v", got)
	}
	if session.dungeon.runtime != runtime || runtime.Session.Snapshot().Run.Status != worldmap.DungeonRunAbandoned {
		t.Fatalf("failed early reset owner=%p status=%s", session.dungeon.runtime, runtime.Session.Snapshot().Run.Status)
	}
	prefixLen := connection.write.Len()
	if err := service.handleDungeonBackToVillage(session, nil); err != nil {
		t.Fatalf("retry early reset: %v", err)
	}
	if got := townPostTransitionPacketSignatures(t, connection.write.Bytes()[prefixLen:]); !reflect.DeepEqual(
		got,
		[]string{"op42", "op376", "mode0", "mode1", "op205", "op24", "op559", "op33weakness"},
	) {
		t.Fatalf("early reset retry=%v", got)
	}
	if session.dungeon.runtime != nil {
		t.Fatalf("early reset retry retained runtime=%+v", session.dungeon.runtime)
	}
}

func TestHandleDungeonBackToVillageRejectsMalformedOrWrongClass(t *testing.T) {
	tests := []struct {
		name           string
		body           []byte
		classification byte
	}{
		{name: "non-empty body", body: []byte{0}, classification: dnfproto.DefaultChannelClassification},
		{name: "wrong class", classification: 2},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			service, session, runtime := newBackToVillageRuntime(t)
			request, err := dnfproto.BuildChannelPacket(
				uint16(dnfenum.CmdPacketBack2Village), test.body, 0, test.classification,
			)
			if err != nil {
				t.Fatal(err)
			}
			if err := service.handleGameUpper(session, request); err != nil {
				t.Fatalf("handle rejected op132: %v", err)
			}
			if session.conn.(*bufferConn).write.Len() != 0 {
				t.Fatalf("rejected op132 emitted response = %x", session.conn.(*bufferConn).write.Bytes())
			}
			if session.dungeon.runtime != runtime {
				t.Fatal("rejected op132 changed runtime owner")
			}
			if status := runtime.Session.Snapshot().Run.Status; status != worldmap.DungeonRunActive {
				t.Fatalf("rejected op132 changed run status = %s", status)
			}
		})
	}
}

// TestHandleDungeonBackToVillageAnswersWhenSelectorContextIsIncomplete 旧的契约是"选择器
// 上下文不完整就静默 block"。现场 2026-09-18 证明那等于把客户端锁死：疲劳归零后"再次挑战"
// 被拒 → 客户端自己弹回选副本页 → "返回城镇"发出 bodyless op132 → 服务端不回帧 → 只能大退。
// 现在的不完整上下文一律照常回城（并留下 game-dungeon-back-to-village-context-armed 日志）。
func TestHandleDungeonBackToVillageAnswersWhenSelectorContextIsIncomplete(t *testing.T) {
	service, session, _ := newBackToVillageRuntime(t)
	session.dungeon.runtime = nil
	bindDungeonSelectorOriginForTest(t, service, session)
	session.enterSelectDungeonSent = true
	session.enterSelectDungeonAckSent = true
	if err := service.handleDungeonBackToVillage(session, nil); err != nil {
		t.Fatalf("handle op132 with incomplete selector context: %v", err)
	}
	assertDungeonTownTransitionPrefixAndOp24(t, session.conn.(*bufferConn).write.Bytes())
}

func assertDungeonTownTransitionPrefixAndOp24(t *testing.T, stream []byte) {
	assertDungeonTownTransitionPrefixAndOp24Body(t, stream, wantDungeonTownTransitionBody())
}

func assertDungeonTownTransitionPrefixAndOp24Body(t *testing.T, stream, wantTransitionBody []byte) {
	t.Helper()
	wantIDs := []uint16{
		currentTownUserPositionNotificationMsgID,
		currentTownUserAreaNotificationMsgID,
		currentClearQuestListMsgID,
		currentSceneTransitionMsgID,
	}
	for index, wantID := range wantIDs {
		packet, rest := splitCurrentGameServerUpperPacketAuto(t, stream)
		if packet.Header.Classification != 0 || packet.Header.MsgID != wantID {
			t.Fatalf("town return packet %d header=%+v want class0/op%d", index, packet.Header, wantID)
		}
		if wantID == currentSceneTransitionMsgID {
			if !bytes.Equal(packet.Body, wantTransitionBody) {
				t.Fatalf("town return op24 body=%x want=%x", packet.Body, wantTransitionBody)
			}
		}
		stream = rest
	}
	// The selector return also installs the party mates' actor records (op2): it
	// preserves the finalized town scene instead of replaying the area-enter
	// co-presence pass, so without those the party came back without models and the
	// next entry was refused with 队员不在附近 (measured 2026-09-19). Accept them
	// here; any other trailing packet is still an error.
	for len(stream) > 0 {
		packet, rest := splitCurrentGameServerUpperPacketAuto(t, stream)
		if packet.Header.Classification == 0 && packet.Header.MsgID == 2 {
			stream = rest
			continue
		}
		t.Fatalf("town return emitted trailing packets=%x", stream)
	}
}

func assertDungeonTownTransitionSequencePresent(t *testing.T, stream []byte) {
	t.Helper()
	// Reference completed dungeon return order seeds the completed bitmap
	// before the op21/op574 task snapshots and the op22/op23 location pair, so
	// op356 precedes the contiguous op22/op23/op24 tail in this stream.
	wantIDs := []uint16{
		currentClearQuestListMsgID,
		currentTownUserPositionNotificationMsgID,
		currentTownUserAreaNotificationMsgID,
		currentSceneTransitionMsgID,
	}
	matched := 0
	for len(stream) > 0 {
		packet, rest := splitCurrentGameServerUpperPacketAuto(t, stream)
		if packet.Header.Classification == 0 && packet.Header.MsgID == wantIDs[matched] {
			matched++
			if matched == len(wantIDs) {
				return
			}
		} else if packet.Header.Classification == 0 && packet.Header.MsgID == wantIDs[0] {
			matched = 1
		} else {
			matched = 0
		}
		stream = rest
	}
	t.Fatalf("town return stream does not contain op356 before contiguous op22/op23/op24")
}

func TestHandleDungeonBackToVillageReturnsFromConfirmedDungeonSelectContextWithoutRuntime(t *testing.T) {
	service, session, _ := newBackToVillageRuntime(t)
	session.dungeon.runtime = nil
	bindDungeonSelectorOriginForTest(t, service, session)
	session.enterSelectDungeonSent = true
	session.enterSelectDungeonAckSent = true
	session.enterSelectDungeonContextSent = true
	session.runtimeAfterBlacklistSent = true
	session.runtimeFinishLoadingGateSent = true
	session.fpsFinishLoadingGateSent = true
	session.selectPreviewActorRemoved = true
	session.preDungeonContextPlayerStateSent = true
	session.currentFinishLoadingStateSent = true
	session.currentFinishLoadingCompletionSent = true
	session.postFinishLoadingPlayerStateSent = true

	if err := service.handleDungeonBackToVillage(session, nil); err != nil {
		t.Fatalf("handle selector op132: %v", err)
	}

	assertDungeonTownTransitionPrefixAndOp24(t, session.conn.(*bufferConn).write.Bytes())
	if session.dungeon.runtime != nil {
		t.Fatalf("selector return created dungeon runtime = %+v", session.dungeon.runtime)
	}
	if session.enterSelectDungeonSent || session.enterSelectDungeonAckSent || session.enterSelectDungeonContextSent {
		t.Fatal("successful selector return retained selector context")
	}
	if session.townSelectorOriginBound || session.townSelectorOriginSnapshot.CharacterID != 0 {
		t.Fatalf("successful selector return retained origin=%+v bound=%t", session.townSelectorOriginSnapshot, session.townSelectorOriginBound)
	}
	if !session.runtimeAfterBlacklistSent || !session.runtimeFinishLoadingGateSent || !session.fpsFinishLoadingGateSent ||
		!session.selectPreviewActorRemoved || !session.preDungeonContextPlayerStateSent ||
		!session.currentFinishLoadingStateSent || !session.currentFinishLoadingCompletionSent ||
		!session.postFinishLoadingPlayerStateSent || session.confirmedDungeonReturnStatePending ||
		session.returnTownFinishLoadingAckOnly {
		t.Fatal("successful selector-only return mutated the finalized town actor generation")
	}
}

// TestHandleDungeonBackToVillageWithoutSelectorContextStillReturnsToTown 复现现场
// 2026-09-18（深渊时空裂缝）：疲劳为 0 时"再次挑战"被正确拒绝后，客户端自己弹回选副本页，
// 此时服务端**没有**选择器上下文（op15/op27 那套没跑过），再点"返回城镇"旧行为是静默
// block——一个包都不回，客户端只能大退。客户端已经明确要求回城，必须照常发出城镇过渡。
func TestHandleDungeonBackToVillageWithoutSelectorContextStillReturnsToTown(t *testing.T) {
	service, session, _ := newBackToVillageRuntime(t)
	session.dungeon.runtime = nil
	// 关键：不设置 enterSelectDungeon* 三个上下文标记（模拟"被拒后弹回"的现场）。
	if session.enterSelectDungeonSent || session.enterSelectDungeonContextSent || session.enterSelectDungeonAckSent {
		t.Fatal("测试前置条件错误：选择器上下文应为空")
	}

	if err := service.handleDungeonBackToVillage(session, nil); err != nil {
		t.Fatalf("handle op132 without selector context: %v", err)
	}

	assertDungeonTownTransitionPrefixAndOp24(t, session.conn.(*bufferConn).write.Bytes())
}

func TestHandleDungeonBackToVillageReturnsPassivePartyFollowerFromSelector(t *testing.T) {
	service, session, _ := newBackToVillageRuntime(t)
	session.dungeon.runtime = nil
	bindDungeonSelectorOriginForTest(t, service, session)
	session.enterSelectDungeonSent = true
	session.enterSelectDungeonAckSent = false
	session.enterSelectDungeonPassiveParty = true
	session.enterSelectDungeonContextSent = true

	if err := service.handleDungeonBackToVillage(session, nil); err != nil {
		t.Fatalf("handle passive party follower selector op132: %v", err)
	}

	assertDungeonTownTransitionPrefixAndOp24(t, session.conn.(*bufferConn).write.Bytes())
	if session.enterSelectDungeonSent || session.enterSelectDungeonAckSent ||
		session.enterSelectDungeonPassiveParty || session.enterSelectDungeonContextSent {
		t.Fatal("successful passive selector return retained selector context")
	}
}

func TestHandleDungeonBackToVillageLeaderReturnsPassivePartyFollowerFromSelector(t *testing.T) {
	service, leader, _ := newBackToVillageRuntime(t)
	leader.dungeon.runtime = nil
	service.bindGameSessionCharacter(leader, 99)
	bindDungeonSelectorOriginForTestAt(t, service, leader, 7, 3, 474, 234)
	leader.enterSelectDungeonSent = true
	leader.enterSelectDungeonAckSent = true
	leader.enterSelectDungeonContextSent = true

	repositories, ok := service.repositoryGroup()
	if !ok {
		t.Fatal("repository group unavailable")
	}
	if err := repositories.Character.Save(context.Background(), dnfrepo.CharacterRecord{
		CharacterID: "100",
		AccountID:   "account-1",
		Level:       20,
		Stats: map[string]int64{
			"fatigue": 100, "town_id": 7, "area_id": 3,
			"pos_x": 500, "pos_y": 240, "direction": 6, "area_state": 3,
		},
	}); err != nil {
		t.Fatal(err)
	}
	follower := &gameSession{
		conn:                &bufferConn{},
		connID:              "passive-party-selector-follower",
		channel:             leader.channel,
		residentChannel:     leader.residentChannel,
		selectedCharacterID: 100,
	}
	service.bindGameSessionCharacter(follower, 100)
	bindDungeonSelectorOriginForTestAt(t, service, follower, 7, 3, 500, 240)
	follower.enterSelectDungeonSent = true
	follower.enterSelectDungeonPassiveParty = true
	follower.enterSelectDungeonContextSent = true

	partyState := alignedcmd.PartyState{
		PartyID: 9,
		UserID:  99,
		Members: []alignedcmd.PartyMemberState{
			{UserID: 99, Slot: 0, UserState: 1, HPPercent: 100, MPPercent: 100},
			{UserID: 100, Slot: 1, UserState: 1, HPPercent: 100, MPPercent: 100},
		},
	}
	storeRuntimePartyState(leader, partyState)
	storeRuntimePartyState(follower, partyState)

	if err := service.handleDungeonBackToVillage(leader, nil); err != nil {
		t.Fatalf("handle leader selector op132: %v", err)
	}

	assertDungeonTownTransitionPrefixAndOp24(t, leader.conn.(*bufferConn).write.Bytes())
	followerWant := []byte{
		7, 3, 1, 0,
		100, 0,
		0xf4, 0x01,
		0xf0, 0x00,
		6, 3,
	}
	assertDungeonTownTransitionPrefixAndOp24Body(t, follower.conn.(*bufferConn).write.Bytes(), followerWant)
	for name, session := range map[string]*gameSession{"leader": leader, "follower": follower} {
		if session.enterSelectDungeonSent || session.enterSelectDungeonAckSent ||
			session.enterSelectDungeonPassiveParty || session.enterSelectDungeonContextSent {
			t.Fatalf("%s retained selector context", name)
		}
		if !session.backToVillageEnterSelectPending {
			t.Fatalf("%s did not arm selector-return scene-finalizer guard", name)
		}
		if session.townSelectorOriginBound || session.townSelectorOriginSnapshot.CharacterID != 0 {
			t.Fatalf("%s retained selector origin=%+v bound=%t",
				name, session.townSelectorOriginSnapshot, session.townSelectorOriginBound)
		}
	}
	if got := runtimePartyStateSnapshot(follower); got.PartyID != 9 || got.UserID != 99 || len(runtimePartyMembers(got)) != 2 {
		t.Fatalf("leader selector return changed follower party state=%+v", got)
	}

	leaderLen := leader.conn.(*bufferConn).write.Len()
	followerLen := follower.conn.(*bufferConn).write.Len()
	if err := service.handleDungeonBackToVillage(leader, nil); err != nil {
		t.Fatalf("duplicate leader selector op132: %v", err)
	}
	if leader.conn.(*bufferConn).write.Len() != leaderLen || follower.conn.(*bufferConn).write.Len() != followerLen {
		t.Fatalf("duplicate leader return emitted packets leader=%d/%d follower=%d/%d",
			leaderLen, leader.conn.(*bufferConn).write.Len(), followerLen, follower.conn.(*bufferConn).write.Len())
	}
}

// TestHandleDungeonBackToVillageAnswersAcklessNonPartySelectorContext 同上：ackless 的
// 非队伍上下文以前也是静默 block，现在同样照常回城——客户端发了 op132 就必须有回音。
func TestHandleDungeonBackToVillageAnswersAcklessNonPartySelectorContext(t *testing.T) {
	service, session, _ := newBackToVillageRuntime(t)
	session.dungeon.runtime = nil
	bindDungeonSelectorOriginForTest(t, service, session)
	session.enterSelectDungeonSent = true
	session.enterSelectDungeonContextSent = true

	if err := service.handleDungeonBackToVillage(session, nil); err != nil {
		t.Fatalf("handle ackless selector op132: %v", err)
	}
	assertDungeonTownTransitionPrefixAndOp24(t, session.conn.(*bufferConn).write.Bytes())
}

func TestHandleDungeonBackToVillageReturnsToSessionScopedAntonSelectorOrigin(t *testing.T) {
	service, session, _ := newBackToVillageRuntime(t)
	session.dungeon.runtime = nil
	session.channel.Type = dnfenum.RaidChannelType
	bindDungeonSelectorOriginForTestAt(t, service, session, byte(currentAntonRaidTownID), 2, 474, 234)
	session.enterSelectDungeonSent = true
	session.enterSelectDungeonAckSent = true
	session.enterSelectDungeonContextSent = true

	if err := service.handleDungeonBackToVillage(session, nil); err != nil {
		t.Fatalf("handle Anton selector op132: %v", err)
	}

	want := []byte{
		byte(currentAntonRaidTownID), 2, 1, 0,
		99, 0,
		0xda, 0x01,
		0xea, 0x00,
		5, 3,
	}
	assertDungeonTownTransitionPrefixAndOp24Body(t, session.conn.(*bufferConn).write.Bytes(), want)

	repositories, ok := service.repositoryGroup()
	if !ok {
		t.Fatal("repository unavailable")
	}
	stored, found, err := repositories.Character.Load(context.Background(), "99")
	if err != nil || !found {
		t.Fatalf("load durable character: found=%t err=%v", found, err)
	}
	if stored.Stats["town_id"] != 7 || stored.Stats["area_id"] != 3 {
		t.Fatalf("session-scoped Anton return changed durable location: %+v", stored.Stats)
	}
}

func TestHandleDungeonBackToVillageSelectorReturnDuplicateIsIdempotent(t *testing.T) {
	service, session, _ := newBackToVillageRuntime(t)
	session.dungeon.runtime = nil
	bindDungeonSelectorOriginForTest(t, service, session)
	session.enterSelectDungeonSent = true
	session.enterSelectDungeonAckSent = true
	session.enterSelectDungeonContextSent = true

	if err := service.handleDungeonBackToVillage(session, nil); err != nil {
		t.Fatalf("first selector op132: %v", err)
	}
	connection := session.conn.(*bufferConn)
	firstLen := connection.write.Len()
	if firstLen == 0 {
		t.Fatal("first selector op132 emitted no typed op24")
	}
	if !session.backToVillageEnterSelectPending {
		t.Fatal("selector op132 did not arm the client scene-finalizer guard")
	}
	if err := service.sendEnterSelectDungeonState(session, "test_selector_return_scene_finalizer", false, true); err != nil {
		t.Fatalf("consume selector-return first op15: %v", err)
	}
	firstOp15Bytes := connection.write.Bytes()[firstLen:]
	if len(firstOp15Bytes) == 0 {
		t.Fatal("selector-return first op15 emitted no selector state")
	}
	if session.backToVillageEnterSelectPending || !session.enterSelectDungeonContextSent ||
		!session.backToVillageSelectorDuplicateGuard {
		t.Fatalf("first op15 state pending=%t context=%t duplicate_guard=%t",
			session.backToVillageEnterSelectPending,
			session.enterSelectDungeonContextSent,
			session.backToVillageSelectorDuplicateGuard)
	}
	finalizedLen := connection.write.Len()
	if err := service.handleDungeonBackToVillage(session, nil); err != nil {
		t.Fatalf("duplicate selector op132: %v", err)
	}
	if connection.write.Len() != finalizedLen {
		t.Fatalf("duplicate selector op132 emitted another response: finalized_len=%d now=%d", finalizedLen, connection.write.Len())
	}
	if session.backToVillageSelectorDuplicateGuard {
		t.Fatal("duplicate selector op132 did not consume duplicate guard")
	}
}

func bindDungeonSelectorOriginForTest(t *testing.T, service *Service, session *gameSession) {
	t.Helper()
	bindDungeonSelectorOriginForTestAt(t, service, session, 7, 3, 474, 234)
}

func bindDungeonSelectorOriginForTestAt(
	t *testing.T,
	service *Service,
	session *gameSession,
	townID byte,
	areaID byte,
	positionX uint16,
	positionY uint16,
) {
	t.Helper()
	session.townMu.Lock()
	setCurrentTownPositionSceneLocked(session, session.selectedCharacterID, townID, areaID)
	session.townPositionSnapshot.PositionX = positionX
	session.townPositionSnapshot.PositionY = positionY
	session.townPositionSnapshot.PositionValid = true
	session.townMu.Unlock()
	service.markCurrentTownSceneReady(session)
	if _, ok := bindCurrentTownSelectorOrigin(session); !ok {
		t.Fatal("failed to bind selector origin")
	}
}

func TestCurrentDungeonTownTransitionRowUsesPersistedSignedPosition(t *testing.T) {
	row, x, y, direction, state, err := currentDungeonTownTransitionRow(99, map[string]int64{
		"pos_x":      -2,
		"pos_y":      -32768,
		"direction":  6,
		"area_state": 0x81,
	})
	if err != nil {
		t.Fatal(err)
	}
	if row.ObjectOrResourceKey != 99 || row.Value1 != 0xfffe || row.Value2 != 0x8000 ||
		row.Value3 != 6 || row.Value4 != 0x81 || x != -2 || y != -32768 || direction != 6 || state != 0x81 {
		t.Fatalf("row=%+v x=%d y=%d direction=%d state=%d", row, x, y, direction, state)
	}
}

func TestCurrentDungeonTownTransitionRowRequiresRealPersistedFields(t *testing.T) {
	tests := []struct {
		name  string
		id    uint16
		stats map[string]int64
	}{
		{name: "missing stats", id: 99},
		{name: "missing x", id: 99, stats: map[string]int64{"pos_y": 1, "direction": 5, "area_state": 3}},
		{name: "missing y", id: 99, stats: map[string]int64{"pos_x": 1, "direction": 5, "area_state": 3}},
		{name: "missing direction", id: 99, stats: map[string]int64{"pos_x": 1, "pos_y": 2, "area_state": 3}},
		{name: "missing state", id: 99, stats: map[string]int64{"pos_x": 1, "pos_y": 2, "direction": 5}},
		{name: "x overflow", id: 99, stats: map[string]int64{"pos_x": 32768, "pos_y": 2, "direction": 5, "area_state": 3}},
		{name: "y underflow", id: 99, stats: map[string]int64{"pos_x": 1, "pos_y": -32769, "direction": 5, "area_state": 3}},
		{name: "direction overflow", id: 99, stats: map[string]int64{"pos_x": 1, "pos_y": 2, "direction": 256, "area_state": 3}},
		{name: "state negative", id: 99, stats: map[string]int64{"pos_x": 1, "pos_y": 2, "direction": 5, "area_state": -1}},
		{name: "zero character", stats: map[string]int64{"pos_x": 1, "pos_y": 2, "direction": 5, "area_state": 3}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, _, _, _, _, err := currentDungeonTownTransitionRow(test.id, test.stats); err == nil {
				t.Fatal("invalid persisted transition fields unexpectedly accepted")
			}
		})
	}
}

func TestResetDungeonEntrySceneGatesForcesPlayerStateRebuild(t *testing.T) {
	session := &gameSession{
		sceneBootstrapTailDeferred:         true,
		sceneBootstrapTailSent:             true,
		runtimeAfterBlacklistSent:          true,
		runtimeFinishLoadingGateSent:       true,
		fpsFinishLoadingGateSent:           true,
		selectedUserInfoRefreshSent:        true,
		selectedUserInfoMode3Sent:          true,
		currentSceneObjectListSent:         true,
		selectedItemListRefreshSent:        true,
		selectedEquipmentUpdateSent:        true,
		selectPreviewObjectStateSent:       true,
		selectPreviewActorRemoved:          true,
		preDungeonContextPlayerStateSent:   true,
		postStartMapPlayerStateSent:        true,
		currentFinishLoadingStateSent:      true,
		currentFinishLoadingCompletionSent: true,
		postFinishLoadingPlayerStateSent:   true,
		initialTownQuestSnapshotsSent:      true,
	}

	resetDungeonEntrySceneGates(session)

	if session.sceneBootstrapTailDeferred || session.sceneBootstrapTailSent ||
		session.runtimeAfterBlacklistSent || session.runtimeFinishLoadingGateSent || session.fpsFinishLoadingGateSent ||
		session.selectedUserInfoRefreshSent || session.selectedUserInfoMode3Sent || session.currentSceneObjectListSent ||
		session.selectedItemListRefreshSent ||
		session.selectedEquipmentUpdateSent || session.selectPreviewActorRemoved ||
		session.preDungeonContextPlayerStateSent || session.postStartMapPlayerStateSent ||
		session.currentFinishLoadingStateSent || session.currentFinishLoadingCompletionSent ||
		session.postFinishLoadingPlayerStateSent ||
		session.initialTownQuestSnapshotsSent {
		t.Fatal("dungeon entry retained a one-shot scene gate")
	}
	if !session.selectPreviewObjectStateSent {
		t.Fatal("dungeon entry cleared the preview owner needed by op9 removal")
	}
}

func newBackToVillageRuntime(t *testing.T) (*Service, *gameSession, *runtimeDungeonState) {
	t.Helper()
	return newBackToVillageRuntimeAtOrigin(t, 474, 234)
}

func TestCompletedPartyReturnRestoresTownPresenceBeforeSymmetricActorRebuild(t *testing.T) {
	service, leader, leaderRuntime := newBackToVillageRuntime(t)
	service.onlinePlayers = newOnlinePlayerManager()
	service.bindGameSessionCharacter(leader, 99)

	repositories, ok := service.repositoryGroup()
	if !ok {
		t.Fatal("repository group unavailable")
	}
	followerCharacter := dnfrepo.CharacterRecord{
		CharacterID: "100",
		AccountID:   "account-1",
		Name:        "follower",
		Job:         "0",
		Level:       20,
		Stats: map[string]int64{
			"fatigue": 100, "town_id": 7, "area_id": 3,
			"pos_x": 500, "pos_y": 234, "direction": 5, "area_state": 3,
		},
	}
	if err := repositories.Character.Save(context.Background(), followerCharacter); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Inventory.Save(context.Background(), dnfrepo.InventoryRecord{
		CharacterID: "100",
		Slots:       map[string]dnfrepo.ItemStack{},
	}); err != nil {
		t.Fatal(err)
	}
	enableTownMoveSkillProjection(t, service, repositories, "100")
	follower := &gameSession{
		conn:                &bufferConn{},
		connID:              "party-return-follower",
		channel:             leader.channel,
		residentChannel:     leader.residentChannel,
		selectedCharacterID: 100,
	}
	service.bindGameSessionCharacter(follower, 100)

	partyState := alignedcmd.PartyState{
		PartyID: 9,
		UserID:  99,
		Members: []alignedcmd.PartyMemberState{
			{UserID: 99, Slot: 0, UserState: 1, HPPercent: 100, MPPercent: 100},
			{UserID: 100, Slot: 1, UserState: 1, HPPercent: 100, MPPercent: 100},
		},
	}
	storeRuntimePartyState(leader, partyState)
	storeRuntimePartyState(follower, partyState)
	owner := &runtimePartyDungeonOwner{
		partyID: 9, leaderID: 99, leaderSlot: 0,
		members: []runtimePartyDungeonMember{
			{slot: 0, userID: 99, generation: leader.characterGeneration},
			{slot: 1, userID: 100, generation: follower.characterGeneration},
		},
	}
	leaderRuntime.partyDungeonOwner = owner
	leaderRuntime.partyMemberIndexed = true
	leaderRuntime.partyMemberIndex = 0
	followerRuntime := &runtimeDungeonState{
		Session:            leaderRuntime.Session,
		partyDungeonOwner:  owner,
		partyMemberIndexed: true,
		partyMemberIndex:   1,
		townReturnOrigin: currentTownPositionSnapshot{
			CharacterID: 100, TownID: 7, AreaID: 3,
			PositionX: 500, PositionY: 234, MovementCode: 5, PositionValid: true,
		},
	}
	follower.dungeon.runtime = followerRuntime

	leaderTransition, err := service.prepareCurrentDungeonTownTransition(context.Background(), 99, leader)
	if err != nil {
		t.Fatal(err)
	}
	followerTransition, err := service.prepareCurrentDungeonTownTransition(context.Background(), 100, follower)
	if err != nil {
		t.Fatal(err)
	}
	leaderCharacter, found, err := repositories.Character.Load(context.Background(), "99")
	if err != nil || !found {
		t.Fatalf("load leader found=%t err=%v", found, err)
	}
	if restored, reason := service.restoreCompletedOrdinaryTownPresence(
		leader, leaderRuntime, leaderTransition, leaderCharacter,
	); !restored {
		t.Fatalf("restore leader presence: %s", reason)
	}
	leader.dungeon.runtime = nil
	if err := service.rebuildRuntimePartyTownCoPresenceAfterReturn(leader, "test_first_member_returned"); err != nil {
		t.Fatal(err)
	}
	if leader.conn.(*bufferConn).write.Len() != 0 || follower.conn.(*bufferConn).write.Len() != 0 {
		t.Fatalf("party actor rebuild ran before the last member returned: leader=%x follower=%x",
			leader.conn.(*bufferConn).write.Bytes(), follower.conn.(*bufferConn).write.Bytes())
	}

	if restored, reason := service.restoreCompletedOrdinaryTownPresence(
		follower, followerRuntime, followerTransition, followerCharacter,
	); !restored {
		t.Fatalf("restore follower presence: %s", reason)
	}
	follower.dungeon.runtime = nil
	if err := service.rebuildRuntimePartyTownCoPresenceAfterReturn(follower, "test_all_members_returned"); err != nil {
		t.Fatal(err)
	}
	if !service.onlinePlayers.PeerInSameArea(99, 100) ||
		!currentPartyTownReturnSceneReady(leader) || !currentPartyTownReturnSceneReady(follower) {
		t.Fatalf("returned party is not scene-ready: leader=%+v follower=%+v",
			leader.townPositionSnapshot, follower.townPositionSnapshot)
	}
	for name, connection := range map[string]*bufferConn{
		"leader": leader.conn.(*bufferConn), "follower": follower.conn.(*bufferConn),
	} {
		packets := splitAllCurrentGameServerUpperPackets(t, connection.write.Bytes())
		var mode0, mode1, roster, area bool
		for _, packet := range packets {
			switch {
			case packet.Header.Classification == 0 && packet.Header.MsgID == uint16(dnfenum.CmdPacketSetUDPIPPort) && len(packet.Body) > 0 && packet.Body[0] == 0:
				mode0 = true
			case packet.Header.Classification == 0 && packet.Header.MsgID == uint16(dnfenum.CmdPacketSetUDPIPPort) && len(packet.Body) > 0 && packet.Body[0] == 1:
				mode1 = true
			case packet.Header.Classification == 0 && packet.Header.MsgID == uint16(dnfenum.CmdPacketRecoverStamina):
				roster = true
			case packet.Header.Classification == 0 && packet.Header.MsgID == currentTownUserAreaNotificationMsgID:
				area = true
			}
		}
		if !mode0 || !mode1 || !roster || !area {
			t.Fatalf("%s completed return packets mode0=%t mode1=%t roster=%t area=%t count=%d",
				name, mode0, mode1, roster, area, len(packets))
		}
	}
}

// A raid squad with one member is still a party of one, and the dungeon
// transition tears the party frame down on the returning client exactly the same
// way. 0x0099 is the only HP-percent source for that frame, so the single-member
// case has to rebuild the roster too. It used to return silently on
// `len(members) < 2`, which is why live 2026-09-17T09:49:43Z shows the client
// returning from hatchery 222 with no 0x0099 at all (the last one in the whole
// log is 09:45:56.680Z, for the other squad).
func TestCompletedSingleMemberPartyReturnStillRestoresTheRealtimeRoster(t *testing.T) {
	service, leader, leaderRuntime := newBackToVillageRuntime(t)
	service.onlinePlayers = newOnlinePlayerManager()
	service.bindGameSessionCharacter(leader, 99)

	storeRuntimePartyState(leader, alignedcmd.PartyState{
		PartyID: 7,
		UserID:  99,
		Members: []alignedcmd.PartyMemberState{
			{UserID: 99, Slot: 0, UserState: 1, HPPercent: 100, MPPercent: 100},
		},
	})

	repositories, ok := service.repositoryGroup()
	if !ok {
		t.Fatal("repository group unavailable")
	}
	transition, err := service.prepareCurrentDungeonTownTransition(context.Background(), 99, leader)
	if err != nil {
		t.Fatal(err)
	}
	character, found, err := repositories.Character.Load(context.Background(), "99")
	if err != nil || !found {
		t.Fatalf("load leader found=%t err=%v", found, err)
	}
	if restored, reason := service.restoreCompletedOrdinaryTownPresence(leader, leaderRuntime, transition, character); !restored {
		t.Fatalf("restore leader presence: %s", reason)
	}
	leader.dungeon.runtime = nil
	leader.conn.(*bufferConn).write.Reset()

	if err := service.rebuildRuntimePartyTownCoPresenceAfterReturn(leader, "test_single_member_return"); err != nil {
		t.Fatal(err)
	}

	packets := splitAllCurrentGameServerUpperPackets(t, leader.conn.(*bufferConn).write.Bytes())
	var rosterBody, realtimeBody []byte
	for _, packet := range packets {
		if packet.Header.Classification != 0 {
			continue
		}
		switch packet.Header.MsgID {
		case uint16(dnfenum.CmdPacketRecoverStamina):
			rosterBody = packet.Body
		case 0x0099:
			realtimeBody = packet.Body
		}
	}
	if rosterBody == nil {
		t.Fatalf("the returned single-member party got no op9 roster: %d packets", len(packets))
	}
	if realtimeBody == nil {
		t.Fatalf("the returned single-member party got no 0x0099 realtime info: %d packets", len(packets))
	}
	// count 1, then the member row: uid u16, HP percent, help/abuse byte, slot.
	if len(realtimeBody) != 6 || realtimeBody[0] != 1 ||
		binary.LittleEndian.Uint16(realtimeBody[1:3]) != 99 ||
		realtimeBody[3] == 0 || realtimeBody[5] != 0 {
		t.Fatalf("0x0099 body=% X want 01 6300 <hp!=0> .. 00", realtimeBody)
	}
}

func newBackToVillageRuntimeAtOrigin(
	t *testing.T,
	positionX uint16,
	positionY uint16,
) (*Service, *gameSession, *runtimeDungeonState) {
	t.Helper()
	table, resolver, monsters := loadBridgeDungeonStaticData(t, bridgeDungeonPVF(false))
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Character.Save(context.Background(), dnfrepo.CharacterRecord{
		CharacterID: "99",
		AccountID:   "account-1",
		Level:       20,
		Stats: map[string]int64{
			"fatigue":    100,
			"town_id":    7,
			"area_id":    3,
			"pos_x":      474,
			"pos_y":      234,
			"direction":  5,
			"area_state": 3,
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Account.Save(context.Background(), dnfrepo.AccountRecord{
		AccountID: "account-1",
		Metadata:  map[string]string{currentRentalPointMetadataKey: "0"},
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
		premiumCatalog: &currentPremiumCatalog{
			contractsByItem: make(map[int64]currentPremiumContractInfo),
			devilSlots:      make(map[uint32]currentPremiumDevilSlotInfo),
			crystalCubeIDs:  [6]int64{3033, 3034, 3035, 3036, 3037, 3262},
		},
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
	}
	enableTownMoveSkillProjection(t, service, repositories, "99")
	channel := channelcatalog.Channel{ServerID: 1, ID: 19, Type: 1, Name: "ch.19", Port: 10019}
	session := &gameSession{
		conn:                &bufferConn{},
		connID:              "back-to-village-test",
		channel:             channel,
		residentChannel:     channel,
		selectedCharacterID: 99,
		characterGeneration: 1,
	}
	bindDungeonSelectorOriginForTestAt(t, service, session, 7, 3, positionX, positionY)
	runtime, _, err := service.prepareDungeonRuntime(
		context.Background(), session, dungeoncmd.SelectDungeonRequest{DungeonID: 700, Difficulty: 1},
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := freezeCurrentDungeonTownReturnOrigin(session, runtime); err != nil {
		t.Fatal(err)
	}
	if err := service.commitDungeonRuntime(session, runtime); err != nil {
		t.Fatal(err)
	}
	return service, session, runtime
}

func backToVillageSkillProjectionBody(t *testing.T, service *Service) []byte {
	t.Helper()
	repositories, ok := service.repositoryGroup()
	if !ok {
		t.Fatal("back-to-village skill repository unavailable")
	}
	return townMoveSkillProjectionBody(t, repositories, "99")
}

func TestCommitOrdinaryDungeonRuntimeRejectsMissingFrozenTownOrigin(t *testing.T) {
	service, session, runtime := newBackToVillageRuntime(t)
	session.dungeon.mu.Lock()
	session.dungeon.runtime = nil
	session.dungeon.mu.Unlock()
	runtime.townReturnOrigin = currentTownPositionSnapshot{}

	err := service.commitDungeonRuntime(session, runtime)
	if !errors.Is(err, errCurrentDungeonTownReturnOriginUnavailable) {
		t.Fatalf("commit error=%v, want missing frozen town origin", err)
	}
	session.dungeon.mu.Lock()
	defer session.dungeon.mu.Unlock()
	if session.dungeon.runtime != nil {
		t.Fatalf("missing-origin runtime became active: %+v", session.dungeon.runtime)
	}
}

// The 返回城镇 stash is frozen when 选择其他地下城 retires a completed run and is
// never refreshed. It stays authoritative only while the live town scene still
// owns the town/area it describes: this is the unchanged case.
func TestSelectorReturnKeepsCompletedRunOriginWhileLiveTownSceneMatches(t *testing.T) {
	service, session, _ := newBackToVillageRuntime(t)
	service.markCurrentTownSceneReady(session)
	clearCurrentTownSelectorOrigin(session)
	session.dungeon.mu.Lock()
	session.dungeon.runtime = nil
	session.dungeon.mu.Unlock()
	stashCompletedRunTownReturnOriginForTest(t, session, 7, 3, 474, 234)

	transition, err := service.prepareCurrentDungeonTownTransitionForSession(
		context.Background(),
		session,
		99,
	)
	if err != nil {
		t.Fatalf("selector return in the unchanged town: %v", err)
	}
	if transition.TownID != 7 || transition.AreaID != 3 ||
		transition.PositionX != 474 || transition.PositionY != 234 ||
		transition.PositionSource != "completed_run_frozen_town_return_origin" ||
		!bytes.Equal(transition.Body, wantDungeonTownTransitionBody()) {
		t.Fatalf(
			"unchanged-town selector return lost its frozen origin: transition=%+v body=%x want=%x",
			transition, transition.Body, wantDungeonTownTransitionBody(),
		)
	}
}

// 圣者之鸣号 regression, live 2026-09-23 13:49:40Z: teleport AFTER the settlement
// retired its completed run, fail the next entry for a missing ticket, then press
// 返回城镇. The frozen origin still named the previous town, so the player was
// sent back to that town and to the coordinates it stood on before teleporting.
func TestSelectorReturnDropsCompletedRunOriginAfterTownMove(t *testing.T) {
	service, session, _ := newBackToVillageRuntime(t)
	service.markCurrentTownSceneReady(session)
	clearCurrentTownSelectorOrigin(session)
	session.dungeon.mu.Lock()
	session.dungeon.runtime = nil
	session.dungeon.mu.Unlock()
	stashCompletedRunTownReturnOriginForTest(t, session, 7, 3, 474, 234)

	// A personal teleport / teleport potion moves the durable location and the
	// live town scene together. The stash deliberately does not move with them.
	moveDurableTownLocationForTest(t, service, 17, 2, 628, 307)
	session.townMu.Lock()
	setCurrentTownPositionSceneLocked(session, 99, 17, 2)
	session.townPositionSnapshot.PositionX = 628
	session.townPositionSnapshot.PositionY = 307
	session.townPositionSnapshot.MovementCode = 5
	session.townPositionSnapshot.PositionValid = true
	session.townMu.Unlock()
	service.markCurrentTownSceneReady(session)

	transition, err := service.prepareCurrentDungeonTownTransitionForSession(
		context.Background(),
		session,
		99,
	)
	if err != nil {
		t.Fatalf("selector return after teleport: %v", err)
	}
	want := []byte{17, 2, 1, 0, 99, 0, 0x74, 0x02, 0x33, 0x01, 5, 3}
	if transition.TownID != 17 || transition.AreaID != 2 ||
		transition.PositionX != 628 || transition.PositionY != 307 ||
		transition.PositionSource != "completed_run_origin_dropped_after_town_move" ||
		!bytes.Equal(transition.Body, want) {
		t.Fatalf(
			"stale completed-run origin survived a town move: transition=%+v body=%x want=%x",
			transition, transition.Body, want,
		)
	}
}

func stashCompletedRunTownReturnOriginForTest(
	t *testing.T,
	session *gameSession,
	townID byte,
	areaID byte,
	positionX uint16,
	positionY uint16,
) {
	t.Helper()
	session.townMu.Lock()
	session.completedRunTownReturnOrigin = currentTownPositionSnapshot{
		CharacterID:   session.selectedCharacterID,
		TownID:        townID,
		AreaID:        areaID,
		PositionX:     positionX,
		PositionY:     positionY,
		MovementCode:  5,
		PositionValid: true,
	}
	session.townMu.Unlock()
}

func moveDurableTownLocationForTest(
	t *testing.T,
	service *Service,
	townID byte,
	areaID byte,
	positionX uint16,
	positionY uint16,
) {
	t.Helper()
	repositories, ok := service.repositoryGroup()
	if !ok {
		t.Fatal("repository group unavailable")
	}
	character, found, err := repositories.Character.Load(context.Background(), "99")
	if err != nil || !found {
		t.Fatalf("load character found=%t err=%v", found, err)
	}
	character.Stats["town_id"] = int64(townID)
	character.Stats["area_id"] = int64(areaID)
	character.Stats["pos_x"] = int64(positionX)
	character.Stats["pos_y"] = int64(positionY)
	if err := repositories.Character.Save(context.Background(), character); err != nil {
		t.Fatal(err)
	}
}

func wantDungeonTownTransitionBody() []byte {
	return []byte{
		7, 3, 1, 0,
		99, 0,
		0xda, 0x01,
		0xea, 0x00,
		5, 3,
	}
}

func consumeCurrentDungeonConsumableGateResetForTest(t *testing.T, data []byte) []byte {
	t.Helper()
	reset, rest := splitGameServerUpperPacket(t, data)
	if reset.Header.MsgID != currentDungeonConsumableUseGateResetWireOpcode ||
		reset.Header.Classification != 0 || len(reset.Body) != 0 {
		t.Fatalf("consumable gate reset header=%+v body=%x trailing=%x", reset.Header, reset.Body, rest)
	}
	return rest
}

func wantTutorialCompletionTownTransitionBody() []byte {
	return []byte{
		newCharacterInitialTownID, newCharacterInitialAreaID, 1, 0,
		99, 0,
		byte(newCharacterInitialPosX & 0xff), byte(newCharacterInitialPosX >> 8),
		byte(newCharacterInitialPosY & 0xff), byte(newCharacterInitialPosY >> 8),
		newCharacterInitialDirection, newCharacterInitialAreaState,
	}
}

func containsClass0Op2ModeForTest(t *testing.T, stream []byte, mode byte) bool {
	t.Helper()
	for len(stream) > 0 {
		packet, rest := splitCurrentGameServerUpperPacketAuto(t, stream)
		if packet.Header.Classification == 0 &&
			packet.Header.MsgID == uint16(dnfenum.CmdPacketSetUDPIPPort) &&
			len(packet.Body) > 0 &&
			packet.Body[0] == mode {
			return true
		}
		stream = rest
	}
	return false
}

func containsInitialTownTransitionForTest(t *testing.T, stream []byte) bool {
	t.Helper()
	for len(stream) > 0 {
		packet, rest := splitCurrentGameServerUpperPacketAuto(t, stream)
		if packetLooksLikeInitialTownTransition(packet) {
			return true
		}
		stream = rest
	}
	return false
}
