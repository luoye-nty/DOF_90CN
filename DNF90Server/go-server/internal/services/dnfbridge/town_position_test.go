package dnfbridge

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"reflect"
	"testing"

	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfproto "longheng.io/server/internal/modules/dnf/protocol"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnftown "longheng.io/server/internal/modules/dnf/town"
	"longheng.io/server/internal/modules/dnf/worldmap"
)

func TestParseCurrentTownSetUserPositionExactCurrentWriterBody(t *testing.T) {
	// Exact pre-TerSafe live sample captured from the current EXE:
	// x=828, y=211, movement=8, scaled opaque value=100.
	body := []byte{0x3c, 0x03, 0xd3, 0x00, 0x08, 0x64, 0x00}
	request, err := parseCurrentTownSetUserPositionRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	if request.PositionX != 828 || request.PositionY != 211 ||
		request.MovementCode != 8 || request.OpaqueScaledU16 != 100 {
		t.Fatalf("request=%+v", request)
	}
	for _, malformed := range [][]byte{
		make([]byte, currentTownSetUserPositionBodySize-1),
		make([]byte, currentTownSetUserPositionBodySize+1),
	} {
		if _, err := parseCurrentTownSetUserPositionRequest(malformed); err == nil {
			t.Fatalf("malformed body length %d unexpectedly accepted", len(malformed))
		}
	}
}

// TestTownMovementBroadcastPreservesReportedRateAndSceneIsolation pins the
// reason a boosted town move speed is visible to the mover but not to peers:
// op35's final u16 is the mover's own town movement rate (actor+0x748 scaled
// by 100) and the op16 broadcast has to carry it verbatim. Sending the fixed
// 100 initialization value instead animates the remote actor at ordinary
// speed on every other screen while the mover still moves correctly locally.
func TestTownMovementBroadcastPreservesReportedRateAndSceneIsolation(t *testing.T) {
	service, session, _ := newTownMoveTest(t)
	service.options.gameUpperBodyCodec = gameUpperBodyCodecPlain
	service.onlinePlayers = newOnlinePlayerManager()
	session.townSceneReadyCharacterID = session.selectedCharacterID
	setCurrentTownPositionSceneLocked(session, session.selectedCharacterID, 38, 0)
	peerConn, privateConn := &bufferConn{}, &bufferConn{}
	channelID := townPresenceChannelID(session)
	for _, player := range []*onlinePlayerInfo{
		{CharacterID: session.selectedCharacterID, ChannelID: channelID, TownID: 38, AreaID: 0, Session: session},
		{CharacterID: 30, ChannelID: channelID, TownID: 38, AreaID: 0, Session: &gameSession{conn: peerConn, channel: session.channel}},
		{CharacterID: 31, ChannelID: channelID, TownID: 38, AreaID: 0, InstanceKey: 99, Session: &gameSession{conn: privateConn, channel: session.channel}},
	} {
		service.onlinePlayers.EnterArea(player)
	}
	for _, rate := range []uint16{100, 170, 185, 987, 0, 100} {
		peerConn.write.Reset()
		if err := service.handleTownSetUserPosition(session, buildTownPositionRequest(640, 244, 6, rate)); err != nil {
			t.Fatal(err)
		}
		packet, rest := splitCurrentGameServerUpperPacketAuto(t, peerConn.write.Bytes())
		if packet.Header.MsgID != 22 || packet.Header.Classification != 0 || len(packet.Body) != 9 || len(rest) != 0 {
			t.Fatalf("movement notification header=%+v body=%x rest=%x", packet.Header, packet.Body, rest)
		}
		if got := binary.LittleEndian.Uint16(packet.Body[7:9]); got != rate {
			t.Fatalf("peer rate=%d, reported=%d", got, rate)
		}
		if binary.LittleEndian.Uint16(packet.Body[0:2]) != currentSceneActorObjectKey(session.selectedCharacterID) ||
			binary.LittleEndian.Uint16(packet.Body[2:4]) != 640 || packet.Body[6] != 6 {
			t.Fatalf("movement actor/position changed: %x", packet.Body)
		}
	}
	if privateConn.write.Len() != 0 {
		t.Fatal("movement leaked into a private town instance")
	}
	peerConn.write.Reset()
	session.townSceneReadyCharacterID = 0
	if err := service.handleTownSetUserPosition(session, buildTownPositionRequest(640, 244, 6, 185)); err != nil {
		t.Fatal(err)
	}
	if peerConn.write.Len() != 0 {
		t.Fatal("unready town scene broadcast movement")
	}
}

func TestHandleTownSetUserPositionCapturesSessionSnapshotWithoutPersistenceOrAck(t *testing.T) {
	service, session, repositories := newTownMoveTest(t)
	if err := service.handleTownSetUserArea(session, buildTownMoveRequest(38, 0, 900, 250, 5)); err != nil {
		t.Fatal(err)
	}
	connection := session.conn.(*bufferConn)
	connection.write.Reset()

	body := buildTownPositionRequest(640, 244, 6, 987)
	if err := service.handleGameCommand(
		session,
		byte(dnfenum.GameCmdCommand),
		uint16(dnfenum.CmdPacketSetUserPosition),
		body,
	); err != nil {
		t.Fatal(err)
	}
	if connection.write.Len() != 0 {
		t.Fatalf("passive op35 emitted an ACK=%x", connection.write.Bytes())
	}
	session.townMu.Lock()
	snapshot := session.townPositionSnapshot
	session.townMu.Unlock()
	if !snapshot.PositionValid || snapshot.CharacterID != 29 ||
		snapshot.TownID != 38 || snapshot.AreaID != 0 ||
		snapshot.PositionX != 640 || snapshot.PositionY != 244 ||
		snapshot.MovementCode != 6 || snapshot.OpaqueScaledU16 != 987 {
		t.Fatalf("snapshot=%+v", snapshot)
	}
	stored, found, err := repositories.Character.Load(context.Background(), "29")
	if err != nil || !found {
		t.Fatalf("load character found=%t err=%v", found, err)
	}
	if stored.Stats["pos_x"] != 900 || stored.Stats["pos_y"] != 250 {
		t.Fatalf("op35 unexpectedly persisted high-frequency position: stats=%+v", stored.Stats)
	}
}

func TestTownSetUserPositionCompletesPVFMeetingNPCChannelWithoutClientOp33(t *testing.T) {
	service, session, repositories := newTownMoveTest(t)
	service.options.gameUpperBodyCodec = gameUpperBodyCodecPlain
	service.questCatalog = questTriggerBridgeCatalog(t)
	if err := repositories.Quest.Save(context.Background(), dnfrepo.QuestRecord{
		CharacterID: "29",
		States: map[int64]dnfrepo.QuestState{
			3622: {Status: "active", ProgressValue: 1012},
		},
	}); err != nil {
		t.Fatal(err)
	}
	townTable, err := dnftown.Load(context.Background(), townMoveTestSource{
		"town/town.lst":                 "22 `Jelva.twn`",
		"town/Jelva.twn":                "[area]\n3 `Town/Jelva/3_Jelva_AG.map` `[normal]`\n[/area]\n[name]\n`Jelva`",
		"map/Town/Jelva/3_Jelva_AG.map": "[map name]\n`Jelva`\n[NPC]\n126 `[left]` 494 223 0\n[/NPC]\n",
	}, dnftown.Options{})
	if err != nil {
		t.Fatal(err)
	}
	service.townCatalog = townTable
	session.townMu.Lock()
	session.townSceneReadyCharacterID = session.selectedCharacterID
	setCurrentTownPositionSceneLocked(session, session.selectedCharacterID, 22, 3)
	session.townMu.Unlock()

	connection := session.conn.(*bufferConn)
	connection.write.Reset()
	if err := service.handleTownSetUserPosition(session, buildTownPositionRequest(494, 223, 5, 100)); err != nil {
		t.Fatal(err)
	}
	stored, found, err := repositories.Quest.Load(context.Background(), "29")
	if err != nil || !found {
		t.Fatalf("load quest found=%t err=%v", found, err)
	}
	if got := stored.States[3622].ProgressValue; got != 500 {
		t.Fatalf("meeting progress=%d want item-channel remainder 500", got)
	}
	ack, rest := splitGameServerUpperPacket(t, connection.write.Bytes())
	if ack.Header.MsgID != uint16(dnfenum.CmdPacketSetQuestTrigger) ||
		binary.LittleEndian.Uint16(ack.Body[1:3]) != 3622 ||
		binary.LittleEndian.Uint32(ack.Body[3:7]) != 500 {
		t.Fatalf("meeting ACK header=%+v body=%x", ack.Header, ack.Body)
	}
	active, trailing := splitGameServerUpperPacket(t, rest)
	if active.Header.MsgID != currentActiveQuestSnapshotMsgID || len(trailing) != 0 {
		t.Fatalf("meeting active snapshot header=%+v trailing=%x", active.Header, trailing)
	}

	connection.write.Reset()
	if err := service.handleCurrentSetQuestTrigger(session, []byte{0x21, 0x00, 0x26, 0x0e, 0x20, 0x00}); err != nil {
		t.Fatal(err)
	}
	if connection.write.Len() != 0 {
		t.Fatalf("client meeting replay wrote packets=%x", connection.write.Bytes())
	}

	if err := service.handleTownSetUserPosition(session, buildTownPositionRequest(800, 223, 5, 100)); err != nil {
		t.Fatal(err)
	}
	if err := repositories.AccountInventory.Save(context.Background(), dnfrepo.AccountInventoryRecord{
		AccountID: "dnf:1",
		Slots: map[string]dnfrepo.ItemStack{
			"0:358": {ItemID: 3037, Count: 500},
		},
	}); err != nil {
		t.Fatal(err)
	}
	connection.write.Reset()
	if err := service.handleTownSetUserPosition(session, buildTownPositionRequest(494, 223, 5, 100)); err != nil {
		t.Fatal(err)
	}
	stored, found, err = repositories.Quest.Load(context.Background(), "29")
	if err != nil || !found || stored.States[3622].ProgressValue != 0 {
		t.Fatalf("re-enter terminal quest found=%t err=%v state=%+v", found, err, stored.States[3622])
	}
	terminal, terminalTrailing := splitGameServerUpperPacket(t, connection.write.Bytes())
	if terminal.Header.MsgID != uint16(dnfenum.CmdPacketSetQuestTrigger) ||
		binary.LittleEndian.Uint32(terminal.Body[3:7]) != 0 || len(terminalTrailing) != 0 {
		t.Fatalf("terminal meeting ACK header=%+v body=%x trailing=%x", terminal.Header, terminal.Body, terminalTrailing)
	}
}

func TestTownSetUserPositionRequiresExactTownSceneOwnerAndClass(t *testing.T) {
	body := buildTownPositionRequest(640, 244, 6, 987)

	t.Run("town scene not ready", func(t *testing.T) {
		service, session, _ := newTownMoveTest(t)
		session.townMu.Lock()
		session.townSceneReadyCharacterID = 0
		setCurrentTownPositionSceneLocked(session, 29, 38, 1)
		session.townMu.Unlock()
		if err := service.handleTownSetUserPosition(session, body); err != nil {
			t.Fatal(err)
		}
		if session.townPositionSnapshot.PositionValid {
			t.Fatalf("unready scene captured snapshot=%+v", session.townPositionSnapshot)
		}
	})

	t.Run("wrong command class", func(t *testing.T) {
		service, session, _ := newTownMoveTest(t)
		service.markCurrentTownSceneReady(session)
		session.townMu.Lock()
		setCurrentTownPositionSceneLocked(session, 29, 38, 1)
		session.townMu.Unlock()
		packet, err := dnfproto.BuildChannelPacket(
			uint16(dnfenum.CmdPacketSetUserPosition),
			body,
			1,
			2,
		)
		if err != nil {
			t.Fatal(err)
		}
		if err := service.handleGameUpper(session, packet); err != nil {
			t.Fatal(err)
		}
		if session.townPositionSnapshot.PositionValid {
			t.Fatalf("wrong-class op35 captured snapshot=%+v", session.townPositionSnapshot)
		}
	})
}

func TestDungeonTownTransitionUsesMatchingCurrentOp35Snapshot(t *testing.T) {
	service, session, _ := newBackToVillageRuntimeAtOrigin(t, 640, 240)

	transition, err := service.prepareCurrentDungeonTownTransitionForSession(
		context.Background(),
		session,
		99,
	)
	if err != nil {
		t.Fatal(err)
	}
	want := []byte{7, 3, 1, 0, 99, 0, 0x80, 0x02, 0xf0, 0x00, 5, 3}
	if transition.PositionX != 640 || transition.PositionY != 240 ||
		transition.PositionSource != "current_exe_op35_runtime_origin_snapshot" ||
		!bytes.Equal(transition.Body, want) {
		t.Fatalf("transition=%+v body=%x want=%x", transition, transition.Body, want)
	}
}

func TestDungeonTownTransitionRejectsStaleSceneSnapshot(t *testing.T) {
	service, session, _ := newBackToVillageRuntime(t)
	session.townMu.Lock()
	setCurrentTownPositionSceneLocked(session, 99, 7, 2)
	session.townPositionSnapshot.PositionX = 640
	session.townPositionSnapshot.PositionY = 240
	session.townPositionSnapshot.PositionValid = true
	session.townMu.Unlock()

	transition, err := service.prepareCurrentDungeonTownTransitionForSession(
		context.Background(),
		session,
		99,
	)
	if err != nil {
		t.Fatal(err)
	}
	if transition.PositionX != 474 || transition.PositionY != 234 ||
		transition.PositionSource != "current_exe_op35_runtime_origin_snapshot" ||
		!bytes.Equal(transition.Body, wantDungeonTownTransitionBody()) {
		t.Fatalf("stale snapshot changed transition=%+v body=%x", transition, transition.Body)
	}
}

func TestDungeonTownTransitionRejectsMismatchedBoundSelectorOrigin(t *testing.T) {
	service, session, _ := newBackToVillageRuntime(t)
	session.dungeon.runtime = nil
	session.townMu.Lock()
	setCurrentTownPositionSceneLocked(session, 99, 7, 2)
	session.townPositionSnapshot.PositionX = 640
	session.townPositionSnapshot.PositionY = 240
	session.townPositionSnapshot.PositionValid = true
	session.townMu.Unlock()
	service.markCurrentTownSceneReady(session)
	if _, ok := bindCurrentTownSelectorOrigin(session); !ok {
		t.Fatal("failed to bind mismatched selector origin")
	}
	if _, err := service.prepareCurrentDungeonTownTransitionForSession(context.Background(), session, 99); err == nil {
		t.Fatal("mismatched bound selector origin unexpectedly fell back to repository position")
	}
}

func TestSelectorPageBackToVillageUsesLatestCurrentOp35Snapshot(t *testing.T) {
	service, session, _ := newBackToVillageRuntime(t)
	session.dungeon.runtime = nil
	session.townMu.Lock()
	setCurrentTownPositionSceneLocked(session, 99, 7, 3)
	session.townMu.Unlock()
	service.markCurrentTownSceneReady(session)
	if err := service.handleTownSetUserPosition(session, buildTownPositionRequest(640, 240, 6, 987)); err != nil {
		t.Fatal(err)
	}
	connection := session.conn.(*bufferConn)
	connection.write.Reset()
	if err := service.sendEnterSelectDungeonState(session, "test_selector_origin", false, true); err != nil {
		t.Fatal(err)
	}
	if !session.townSelectorOriginBound ||
		session.townSelectorOriginSnapshot.PositionX != 640 ||
		session.townSelectorOriginSnapshot.PositionY != 240 {
		t.Fatalf("selector origin=%+v bound=%t", session.townSelectorOriginSnapshot, session.townSelectorOriginBound)
	}
	connection.write.Reset()

	// Even if a later report arrives while the selector is visible, op132 must
	// return to the origin frozen by the successful op15/op27 context.
	if err := service.handleTownSetUserPosition(session, buildTownPositionRequest(700, 260, 7, 100)); err != nil {
		t.Fatal(err)
	}

	if err := service.handleDungeonBackToVillage(session, nil); err != nil {
		t.Fatal(err)
	}
	// The selector return writes the same documented prefix as every other dungeon
	// return before op24 installs the destination scene: op22 (controlled actor
	// position), op23 (active area) and the completed-quest bitmap. Those packets
	// use the 16-byte header, so step over them by message id instead of assuming
	// the transition is the first packet of the stream.
	frozenOrigin := []byte{0x80, 0x02, 0xf0, 0x00}
	laterReport := []byte{0xbc, 0x02, 0x04, 0x01}
	data := connection.write.Bytes()
	for _, wantMsgID := range []uint16{
		currentTownUserPositionNotificationMsgID,
		currentTownUserAreaNotificationMsgID,
		currentClearQuestListMsgID,
	} {
		prefix, prefixRest := splitCurrentGameServerUpperPacketAuto(t, data)
		if prefix.Header.Classification != 0 || prefix.Header.MsgID != wantMsgID {
			t.Fatalf("selector prefix header=%+v want msg=%d", prefix.Header, wantMsgID)
		}
		// Every prefixed packet is built from the same frozen origin, so the later
		// op35 report must not have leaked into any of them either.
		if wantMsgID != currentClearQuestListMsgID && !bytes.Contains(prefix.Body, frozenOrigin) {
			t.Fatalf("selector prefix msg=%d body=%x lacks the frozen origin", wantMsgID, prefix.Body)
		}
		if bytes.Contains(prefix.Body, laterReport) {
			t.Fatalf("selector prefix msg=%d body=%x leaked the later op35 report", wantMsgID, prefix.Body)
		}
		data = prefixRest
	}
	packet, rest := splitCurrentGameServerUpperPacketAuto(t, data)
	want := []byte{7, 3, 1, 0, 99, 0, 0x80, 0x02, 0xf0, 0x00, 5, 3}
	if packet.Header.MsgID != currentSceneTransitionMsgID ||
		packet.Header.Classification != 0 || !bytes.Equal(packet.Body, want) || len(rest) != 0 {
		t.Fatalf("selector op132 header=%+v body=%x rest=%x want=%x", packet.Header, packet.Body, rest, want)
	}
	if session.townSelectorOriginBound || session.townSelectorOriginSnapshot.CharacterID != 0 {
		t.Fatalf("successful selector return retained origin=%+v bound=%t", session.townSelectorOriginSnapshot, session.townSelectorOriginBound)
	}
}

func TestActiveDungeonPreservesFrozenSelectorOriginAndCapturesDungeonOp35(t *testing.T) {
	service, session, runtime := newBackToVillageRuntimeAtOrigin(t, 640, 240)
	if err := service.handleTownSetUserPosition(session, buildTownPositionRequest(700, 260, 7, 100)); err != nil {
		t.Fatal(err)
	}
	if !runtime.actorPositionValid || runtime.actorPositionX != 700 || runtime.actorPositionY != 260 {
		t.Fatalf("dungeon actor position valid=%t x=%d y=%d", runtime.actorPositionValid, runtime.actorPositionX, runtime.actorPositionY)
	}
	transition, err := service.prepareCurrentDungeonTownTransitionForSession(context.Background(), session, 99)
	if err != nil {
		t.Fatal(err)
	}
	if transition.PositionX != 640 || transition.PositionY != 240 ||
		transition.PositionSource != "current_exe_op35_runtime_origin_snapshot" {
		t.Fatalf("active return transition=%+v", transition)
	}
	session.townMu.Lock()
	live := session.townPositionSnapshot
	origin := session.townSelectorOriginSnapshot
	originBound := session.townSelectorOriginBound
	session.townMu.Unlock()
	if live.PositionX != 640 || live.PositionY != 240 ||
		!originBound || origin.PositionX != 640 || origin.PositionY != 240 {
		t.Fatalf("dungeon op35 changed snapshots live=%+v origin=%+v bound=%t", live, origin, originBound)
	}
}

func TestTownSetUserPositionAfterFullDungeonReturnDoesNotRebuildAgain(t *testing.T) {
	service, session, runtime := newBackToVillageRuntime(t)
	if err := service.handleDungeonBackToVillage(session, nil); err != nil {
		t.Fatalf("full return: %v", err)
	}
	if session.dungeon.runtime != nil || !runtime.townReturnPending || !runtime.townReturnOp24Sent {
		t.Fatalf("return did not retire old state owner=%p runtime=%+v", session.dungeon.runtime, runtime)
	}
	connection := session.conn.(*bufferConn)
	connection.write.Reset()

	if err := service.handleTownSetUserPosition(session, buildTownPositionRequest(85, 331, 5, 100)); err != nil {
		t.Fatal(err)
	}
	if connection.write.Len() != 0 {
		t.Fatalf("post-return op35 rebuilt town route=%x", connection.write.Bytes())
	}
	if session.dungeon.runtime != nil {
		t.Fatalf("town op35 retained pending dungeon runtime=%+v", session.dungeon.runtime)
	}
	if status := runtime.Session.Snapshot().Run.Status; status != worldmap.DungeonRunAbandoned {
		t.Fatalf("town op35 committed status=%s want abandoned", status)
	}
	session.townMu.Lock()
	snapshot := session.townPositionSnapshot
	readyCharacterID := session.townSceneReadyCharacterID
	session.townMu.Unlock()
	if readyCharacterID != session.selectedCharacterID ||
		!snapshot.PositionValid ||
		snapshot.CharacterID != session.selectedCharacterID ||
		snapshot.TownID != runtime.townReturnTransition.TownID ||
		snapshot.AreaID != runtime.townReturnTransition.AreaID ||
		snapshot.PositionX != 85 ||
		snapshot.PositionY != 331 ||
		snapshot.MovementCode != 5 ||
		snapshot.OpaqueScaledU16 != 100 {
		t.Fatalf("ready=%d snapshot=%+v transition=%+v", readyCharacterID, snapshot, runtime.townReturnTransition)
	}
}

func TestTownSetUserPositionResumesInterruptedFullReturnSuffix(t *testing.T) {
	service, session, runtime := newBackToVillageRuntime(t)
	wantErr := errors.New("full return op559 write failed")
	// The interrupted return writes op42, op376, mode0, mode1, op205, then the op22
	// and op23 location pair, then op24, and only then op559. Fail the ninth write
	// so the injected error still lands on the buff registration, which is the one
	// packet the town-side op35 is expected to resume.
	failing := &failNthDungeonWriteConn{failAt: 9, err: wantErr}
	session.conn = failing
	if err := service.handleDungeonBackToVillage(session, nil); !errors.Is(err, wantErr) {
		t.Fatalf("full return prefix failure=%v want=%v", err, wantErr)
	}
	if got := townPostTransitionPacketSignatures(t, failing.write.Bytes()); !reflect.DeepEqual(
		got,
		[]string{"op42", "op376", "mode0", "mode1", "op205", "op24"},
	) {
		t.Fatalf("full return prefix=%v", got)
	}
	if session.dungeon.runtime != runtime ||
		runtime.Session.Snapshot().Run.Status != worldmap.DungeonRunAbandoned ||
		!runtime.townReturnPending ||
		!runtime.townReturnOp24Sent ||
		session.initialTownRouteStage != currentInitialTownRouteTransitionSent {
		t.Fatalf(
			"detached return owner=%p run=%s pending=%t stage=%d",
			session.dungeon.runtime,
			runtime.Session.Snapshot().Run.Status,
			runtime.townReturnPending,
			session.initialTownRouteStage,
		)
	}

	resume := &bufferConn{}
	session.conn = resume
	if err := service.handleTownSetUserPosition(
		session,
		buildTownPositionRequest(85, 331, 5, 100),
	); err != nil {
		t.Fatalf("op35 resume confirmed return suffix: %v", err)
	}
	// The resume replays only what the interrupted return still owed: the BUFF
	// registration (op559) and, right behind it, the forced-exit weakness cap. The
	// give-up applied currentWeaknessForcedReturnLevel to the character, so the
	// health cap has to be republished before the client keeps the town actor it
	// just rebuilt -- and the aborted return never got to publish it.
	if got := townPostTransitionPacketSignatures(t, resume.write.Bytes()); !reflect.DeepEqual(
		got,
		[]string{"op559", "op33weakness"},
	) {
		t.Fatalf("op35 resumed suffix=%v want=[op559 op33weakness]", got)
	}
	if session.dungeon.runtime != nil || session.initialTownRouteStage != currentInitialTownRoutePlayerStateSent {
		t.Fatalf(
			"op35 resume owner=%p stage=%d",
			session.dungeon.runtime,
			session.initialTownRouteStage,
		)
	}
}

func TestDungeonSelectReturnWithoutFrozenOp35OriginFailsClosed(t *testing.T) {
	service, session, _ := newBackToVillageRuntime(t)
	session.dungeon.mu.Lock()
	session.dungeon.runtime = nil
	session.dungeon.mu.Unlock()
	clearCurrentTownSelectorOrigin(session)

	if _, err := service.prepareCurrentDungeonTownTransitionForSession(
		context.Background(),
		session,
		99,
	); !errors.Is(err, errCurrentDungeonTownReturnOriginUnavailable) {
		t.Fatalf("selector return error=%v, want unavailable frozen origin", err)
	}
}

func TestFreezeAntonRaidDungeonReturnOriginUsesSessionScopedWaitingArea(t *testing.T) {
	service, session, runtime := newBackToVillageRuntime(t)
	session.channel.Type = dnfenum.RaidChannelType
	runtime.Dungeon.ID = 5314
	bindDungeonSelectorOriginForTestAt(
		t,
		service,
		session,
		byte(currentAntonRaidTownID),
		currentAntonRaidWaitingAreaID,
		987,
		380,
	)

	if err := freezeCurrentDungeonTownReturnOrigin(session, runtime); err != nil {
		t.Fatalf("freeze session-scoped Anton origin: %v", err)
	}
	if origin := runtime.townReturnOrigin; !origin.PositionValid ||
		origin.CharacterID != session.selectedCharacterID ||
		origin.TownID != byte(currentAntonRaidTownID) ||
		origin.AreaID != currentAntonRaidWaitingAreaID ||
		origin.PositionX != 987 || origin.PositionY != 380 {
		t.Fatalf("Anton raid return origin=%+v", origin)
	}
	if err := validateCurrentDungeonRuntimeTownReturnOrigin(session, runtime); err != nil {
		t.Fatalf("commit validation rejected session-scoped Anton origin: %v", err)
	}
}

func TestActiveAntonRaidDungeonTownTransitionUsesFrozenSessionScopedOrigin(t *testing.T) {
	service, session, runtime := newBackToVillageRuntime(t)
	session.channel.Type = dnfenum.RaidChannelType
	runtime.Dungeon.ID = 5314
	runtime.Request.DungeonID = 5314
	runtime.townReturnOrigin = currentTownPositionSnapshot{
		CharacterID:   session.selectedCharacterID,
		TownID:        byte(currentAntonRaidTownID),
		AreaID:        currentAntonRaidWaitingAreaID,
		PositionX:     987,
		PositionY:     380,
		PositionValid: true,
	}

	transition, err := service.prepareCurrentDungeonTownTransitionForSession(
		context.Background(),
		session,
		session.selectedCharacterID,
	)
	if err != nil {
		t.Fatalf("prepare active Anton return: %v", err)
	}
	want := []byte{
		byte(currentAntonRaidTownID), currentAntonRaidWaitingAreaID, 1, 0,
		99, 0,
		0xdb, 0x03,
		0x7c, 0x01,
		5, 3,
	}
	if transition.TownID != byte(currentAntonRaidTownID) ||
		transition.AreaID != currentAntonRaidWaitingAreaID ||
		transition.PositionX != 987 || transition.PositionY != 380 ||
		transition.PositionSource != "current_exe_op35_session_scoped_anton_runtime_origin_snapshot" ||
		!bytes.Equal(transition.Body, want) {
		t.Fatalf("active Anton transition=%+v body=%x want=%x", transition, transition.Body, want)
	}
}

func TestDungeonRuntimeTownReturnOriginStillRejectsOrdinaryLocationMismatch(t *testing.T) {
	_, session, runtime := newBackToVillageRuntime(t)
	runtime.townReturnOrigin.TownID++

	if err := validateCurrentDungeonRuntimeTownReturnOrigin(session, runtime); !errors.Is(err, errCurrentDungeonTownReturnOriginUnavailable) {
		t.Fatalf("ordinary mismatched origin error=%v, want town-return origin unavailable", err)
	}
}

func buildTownPositionRequest(x, y uint16, movementCode byte, opaqueScaledU16 uint16) []byte {
	body := make([]byte, currentTownSetUserPositionBodySize)
	binary.LittleEndian.PutUint16(body[0:2], x)
	binary.LittleEndian.PutUint16(body[2:4], y)
	body[4] = movementCode
	binary.LittleEndian.PutUint16(body[5:7], opaqueScaledU16)
	return body
}
