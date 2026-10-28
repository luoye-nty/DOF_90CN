package dnfbridge

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	"longheng.io/server/internal/modules/dnf/channelcatalog"
	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfproto "longheng.io/server/internal/modules/dnf/protocol"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnftown "longheng.io/server/internal/modules/dnf/town"
)

type townMoveTestSource map[string]string

func (source townMoveTestSource) ReadText(name string) (string, error) {
	value, ok := source[name]
	if !ok {
		return "", fmt.Errorf("missing %s", name)
	}
	return value, nil
}

func splitTownAreaTransitionSequence(
	t *testing.T,
	data []byte,
	actorKey uint16,
	townID byte,
	areaID byte,
	x uint16,
	y uint16,
	direction byte,
	areaState byte,
) (dnfproto.ChannelPacket, []byte) {
	t.Helper()
	position, rest := splitCurrentGameServerUpperPacketAuto(t, data)
	wantPosition := buildCurrentTownUserPositionNotificationBody(actorKey, x, y, direction, currentTownInitialMovementRate)
	if position.Header.MsgID != currentTownUserPositionNotificationMsgID ||
		position.Header.Classification != 0 ||
		!bytes.Equal(position.Body, wantPosition) {
		t.Fatalf("town op22 position header=%+v body=%x want=%x", position.Header, position.Body, wantPosition)
	}
	area, rest := splitCurrentGameServerUpperPacketAuto(t, rest)
	wantArea := buildCurrentTownUserAreaNotificationBody(
		actorKey,
		townID,
		areaID,
		x,
		y,
		direction,
		areaState,
	)
	if area.Header.MsgID != currentTownUserAreaNotificationMsgID ||
		area.Header.Classification != 0 ||
		!bytes.Equal(area.Body, wantArea) {
		t.Fatalf("town op23 area header=%+v body=%x want=%x", area.Header, area.Body, wantArea)
	}
	transition, rest := splitCurrentGameServerUpperPacketAuto(t, rest)
	if transition.Header.MsgID != currentSceneTransitionMsgID || transition.Header.Classification != 0 {
		t.Fatalf("town op24 transition header=%+v body=%x", transition.Header, transition.Body)
	}
	return transition, rest
}

func splitTownAreaTransitionSequenceWithClearQuest(
	t *testing.T,
	data []byte,
	actorKey uint16,
	townID byte,
	areaID byte,
	x uint16,
	y uint16,
	direction byte,
	areaState byte,
) (dnfproto.ChannelPacket, dnfproto.ChannelPacket, []byte) {
	t.Helper()
	position, rest := splitCurrentGameServerUpperPacketAuto(t, data)
	wantPosition := buildCurrentTownUserPositionNotificationBody(actorKey, x, y, direction, currentTownInitialMovementRate)
	if position.Header.MsgID != currentTownUserPositionNotificationMsgID ||
		position.Header.Classification != 0 ||
		!bytes.Equal(position.Body, wantPosition) {
		t.Fatalf("town op22 position header=%+v body=%x want=%x", position.Header, position.Body, wantPosition)
	}
	area, rest := splitCurrentGameServerUpperPacketAuto(t, rest)
	wantArea := buildCurrentTownUserAreaNotificationBody(actorKey, townID, areaID, x, y, direction, areaState)
	if area.Header.MsgID != currentTownUserAreaNotificationMsgID ||
		area.Header.Classification != 0 ||
		!bytes.Equal(area.Body, wantArea) {
		t.Fatalf("town op23 area header=%+v body=%x want=%x", area.Header, area.Body, wantArea)
	}
	clearQuest, rest := splitLongHengGameServerUpperPacket(t, rest)
	if clearQuest.Header.MsgID != currentClearQuestListMsgID || clearQuest.Header.Classification != 0 {
		t.Fatalf("town pre-op24 op356 header=%+v body=%x", clearQuest.Header, clearQuest.Body)
	}
	transition, rest := splitCurrentGameServerUpperPacketAuto(t, rest)
	if transition.Header.MsgID != currentSceneTransitionMsgID || transition.Header.Classification != 0 {
		t.Fatalf("town op24 transition header=%+v body=%x", transition.Header, transition.Body)
	}
	return clearQuest, transition, rest
}

func splitTownPostTransitionPlayerState(
	t *testing.T,
	data []byte,
	session *gameSession,
	actorKey uint16,
	wantSkillBody []byte,
	wantCreatureGrowth bool,
) []byte {
	t.Helper()
	wantOwner := currentConnectionTownActorOwnerContext(session)
	if currentTownActorOwnerContext(session) != wantOwner {
		t.Fatalf(
			"town owner mismatch connection=%d actor=%d",
			wantOwner,
			currentTownActorOwnerContext(session),
		)
	}

	mode0, rest := splitCurrentGameServerUpperPacketAuto(t, data)
	if mode0.Header.Classification == 0 &&
		mode0.Header.MsgID == uint16(dnfenum.CmdPacketPVPMissionHpPercent) {
		if !bytes.Equal(mode0.Body, buildCurrentActionTableStateBody()) {
			t.Fatalf("post-op24 op376 action table body=%x", mode0.Body)
		}
		mode0, rest = splitCurrentGameServerUpperPacketAuto(t, rest)
	}
	if mode0.Header.Classification != 0 ||
		mode0.Header.MsgID != uint16(dnfenum.CmdPacketSetUDPIPPort) ||
		len(mode0.Body) < 0x4e ||
		mode0.Body[0] != 0 ||
		mode0.Body[3] != currentSceneObjectRoute ||
		mode0.Body[4] != wantOwner ||
		binary.LittleEndian.Uint16(mode0.Body[0x4c:0x4e]) != actorKey {
		t.Fatalf(
			"post-op24 native-owner mode0 header=%+v prefix=%x owner=%d key=%d",
			mode0.Header,
			mode0.Body[:minInt(len(mode0.Body), 0x4e)],
			wantOwner,
			actorKey,
		)
	}

	mode1, rest := splitCurrentGameServerUpperPacketAuto(t, rest)
	if mode1.Header.Classification != 0 ||
		mode1.Header.MsgID != uint16(dnfenum.CmdPacketSetUDPIPPort) ||
		len(mode1.Body) < currentMode1BaseWireSize ||
		mode1.Body[0] != 1 ||
		mode1.Body[3] != currentSceneObjectRoute ||
		mode1.Body[4] != wantOwner ||
		binary.LittleEndian.Uint16(mode1.Body[0x15:0x17]) != actorKey {
		t.Fatalf(
			"post-op24 native-owner mode1 header=%+v prefix=%x owner=%d key=%d",
			mode1.Header,
			mode1.Body[:minInt(len(mode1.Body), 0x17)],
			wantOwner,
			actorKey,
		)
	}
	creature, rest := splitCurrentGameServerUpperPacketAuto(t, rest)
	if creature.Header.Classification != 0 ||
		creature.Header.MsgID != currentCreatureStateTableMsgID {
		t.Fatalf("post-op24 creature table header=%+v body=%x", creature.Header, creature.Body)
	}
	if wantCreatureGrowth {
		growth, next := splitCurrentGameServerUpperPacketAuto(t, rest)
		if growth.Header.Classification != 0 ||
			growth.Header.MsgID != currentCreatureGrowthMsgID {
			t.Fatalf("post-op24 creature growth header=%+v body=%x", growth.Header, growth.Body)
		}
		rest = next
	}
	finishState, rest := splitCurrentGameServerUpperPacketAuto(t, rest)
	if finishState.Header.Classification != 0 ||
		finishState.Header.MsgID != uint16(dnfenum.CmdPacketFinishLoading) ||
		len(finishState.Body) != currentFinishLoadingCharacterStateBodySize {
		t.Fatalf("post-op24 op37 header=%+v body=%x", finishState.Header, finishState.Body)
	}
	completion, rest := splitCurrentGameServerUpperPacketAuto(t, rest)
	if completion.Header.Classification != 0 ||
		completion.Header.MsgID != currentIncreaseStatusResultMsgID ||
		!bytes.Equal(completion.Body, buildCurrentIncreaseStatusResultBody()) {
		t.Fatalf("post-op24 op30 header=%+v body=%x", completion.Header, completion.Body)
	}
	buff, rest := splitCurrentGameServerUpperPacketAuto(t, rest)
	if buff.Header.Classification != 0 || buff.Header.MsgID != skillSwitchInventoryAllDataWire || len(buff.Body) < 3 {
		t.Fatalf("post-op24 BUFF registration header=%+v body=%x", buff.Header, buff.Body)
	}
	placement, rest := splitCurrentGameServerUpperPacketAuto(t, rest)
	if placement.Header.Classification != 0 ||
		placement.Header.MsgID != uint16(dnfenum.CmdPacketRequestBlacklist) ||
		!bytes.Equal(placement.Body, buildCurrentSceneActorPlacementBody()) {
		t.Fatalf("post-op24 op120 placement header=%+v body=%x", placement.Header, placement.Body)
	}
	if len(rest) != 0 {
		packet, next := splitCurrentGameServerUpperPacketAuto(t, rest)
		if packet.Header.Classification == 0 &&
			packet.Header.MsgID == currentDungeonConsumableUseGateResetWireOpcode {
			if len(packet.Body) != 0 {
				t.Fatalf("post-op24 final op42 body=%x", packet.Body)
			}
			rest = next
		}
	}
	_ = wantSkillBody
	return rest
}

func splitTownTransitionAndPostState(
	t *testing.T,
	data []byte,
	session *gameSession,
	actorKey uint16,
	townID byte,
	areaID byte,
	x uint16,
	y uint16,
	direction byte,
	areaState byte,
	wantSkillBody []byte,
	wantCreatureGrowth bool,
) (dnfproto.ChannelPacket, []byte) {
	t.Helper()
	first, firstRest := splitCurrentGameServerUpperPacketAuto(t, data)
	if first.Header.Classification == 0 && first.Header.MsgID == currentSceneTransitionMsgID {
		return first, firstRest
	}
	transition, rest := splitTownAreaTransitionSequence(
		t,
		data,
		actorKey,
		townID,
		areaID,
		x,
		y,
		direction,
		areaState,
	)
	// Ordinary town, personal-teleport, and potion routes end their scene
	// transition at op24. Feature-specific packets (for example op898 crystal
	// state or the potion inventory/ACK suffix) remain for the caller to assert;
	// the actor-generation rebuild belongs only to the explicit dungeon-return
	// owner and must not be invented by this shared parser.
	_ = session
	_ = wantSkillBody
	_ = wantCreatureGrowth
	return transition, rest
}

func townPostTransitionPacketSignatures(t *testing.T, data []byte) []string {
	t.Helper()
	signatures := make([]string, 0, 9)
	for len(data) != 0 {
		packet, rest := splitCurrentGameServerUpperPacketAuto(t, data)
		switch packet.Header.MsgID {
		case uint16(dnfenum.CmdPacketPVPMissionHpPercent):
			if packet.Header.Classification != 0 ||
				!bytes.Equal(packet.Body, buildCurrentActionTableStateBody()) {
				t.Fatalf("unexpected post-op24 op376 body=%x", packet.Body)
			}
			signatures = append(signatures, "op376")
		case uint16(dnfenum.CmdPacketSetUDPIPPort):
			if len(packet.Body) == 0 || (packet.Body[0] != 0 && packet.Body[0] != 1) {
				t.Fatalf("unexpected post-op24 op2 body=%x", packet.Body)
			}
			signatures = append(signatures, fmt.Sprintf("mode%d", packet.Body[0]))
		case currentChatEmoticonConfigMsgID:
			signatures = append(signatures, "op411")
		case uint16(dnfenum.CmdPacketBuyItem):
			signatures = append(signatures, "op21")
		case uint16(dnfenum.CmdPacketCollectPrivateSerialData):
			signatures = append(signatures, "op574")
		case currentSceneTransitionMsgID:
			signatures = append(signatures, "op24")
		case currentWeaknessConditionMsg:
			// The health cap published after the rebuild. One byte, 0..100.
			if packet.Header.Classification != 0 || len(packet.Body) != 1 || packet.Body[0] > 100 {
				t.Fatalf("unexpected post-op24 op4 weakness body=%x", packet.Body)
			}
			signatures = append(signatures, "op33weakness")
		case currentTownUserPositionNotificationMsgID,
			currentTownUserAreaNotificationMsgID,
			currentClearQuestListMsgID:
			// The unified dungeon-return transition prefix is asserted directly
			// by its dedicated ordering regressions. Keep this legacy lifecycle
			// signature helper focused on actor/gate packets.
		case currentCreatureStateTableMsgID:
			signatures = append(signatures, "op105")
		case currentCreatureGrowthMsgID:
			signatures = append(signatures, "op102")
		case uint16(dnfenum.CmdPacketWalkoutPartyMember):
			if packet.Header.Classification != 0 ||
				len(packet.Body) != 3+currentItemListEntryWireSize ||
				packet.Body[0] != currentSocketListEquipment ||
				binary.LittleEndian.Uint16(packet.Body[1:3]) != 1 {
				t.Fatalf("unexpected post-op24 op14 body=%x", packet.Body)
			}
			slot := binary.LittleEndian.Uint16(packet.Body[3:5])
			signatures = append(signatures, fmt.Sprintf("slot%d_op14", slot))
		case uint16(dnfenum.CmdPacketFinishLoading):
			signatures = append(signatures, "op37")
		case currentHellPartyMonsterInfoWireOpcode:
			if packet.Header.Classification != 0 ||
				!bytes.Equal(packet.Body, buildCurrentHellPartyMonsterInfoBody(nil)) {
				t.Fatalf("unexpected post-op24 op679 body=%x", packet.Body)
			}
			signatures = append(signatures, "op679_empty")
		case currentIncreaseStatusResultMsgID:
			signatures = append(signatures, "op30")
		case currentSkillInfoMsgID:
			signatures = append(signatures, "op19")
		case currentExpertJobInfoNotification:
			signatures = append(signatures, "op205")
		case skillSwitchInventoryAllDataWire:
			signatures = append(signatures, "op559")
		case uint16(dnfenum.CmdPacketRequestBlacklist):
			signatures = append(signatures, "op120")
		case currentDungeonConsumableUseGateResetWireOpcode:
			if packet.Header.Classification != 0 || len(packet.Body) != 0 {
				t.Fatalf("unexpected post-op24 op42 body=%x", packet.Body)
			}
			signatures = append(signatures, "op42")
		default:
			t.Fatalf("unexpected post-op24 packet header=%+v body=%x", packet.Header, packet.Body)
		}
		data = rest
	}
	return signatures
}

func splitConfirmedDungeonReturnRoute(t *testing.T, data []byte) ([]string, []byte) {
	t.Helper()
	signatures := make([]string, 0, 8)
	for len(data) != 0 {
		_, rest := splitCurrentGameServerUpperPacketAuto(t, data)
		packetLen := len(data) - len(rest)
		one := townPostTransitionPacketSignatures(t, data[:packetLen])
		if len(one) != 1 {
			t.Fatalf("confirmed-return packet signatures=%v", one)
		}
		signatures = append(signatures, one[0])
		data = rest
		if one[0] == "op42" {
			return signatures, data
		}
	}
	t.Fatalf("confirmed-return route ended without final op42: %v", signatures)
	return nil, nil
}

func TestHandleTownSetUserAreaPersistsPVFTargetAndSendsOneAuthoritativeTransition(t *testing.T) {
	service, session, repositories := newTownMoveTest(t)
	service.onlinePlayers = newOnlinePlayerManager()
	session.currentFinishLoadingStateSent = true
	session.currentFinishLoadingCompletionSent = true
	session.postFinishLoadingPlayerStateSent = true
	session.initialTownLegacySceneReadyAccepted = true
	body := buildTownMoveRequest(38, 0, 900, 250, 5)
	binary.LittleEndian.PutUint16(body[7:9], 0x1234)
	binary.LittleEndian.PutUint16(body[9:11], 0x5678)
	binary.LittleEndian.PutUint32(body[11:15], 0x90abcdef)
	body[15] = 0x42

	if err := service.handleTownSetUserArea(session, body); err != nil {
		t.Fatal(err)
	}
	packet, rest := splitTownAreaTransitionSequence(
		t,
		session.conn.(*bufferConn).write.Bytes(),
		29,
		38,
		0,
		900,
		250,
		5,
		3,
	)
	wantBody, err := buildCurrentSceneTransitionBody(38, 0, []currentSceneTransitionRow{{
		ObjectOrResourceKey: 29,
		Value1:              uint16(900),
		Value2:              uint16(250),
		Value3:              5,
		Value4:              3,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if packet.Header.MsgID != currentSceneTransitionMsgID || packet.Header.Classification != 0 ||
		!bytes.Equal(packet.Body, wantBody) {
		t.Fatalf("town response header=%+v body=%x rest=%x want=%x", packet.Header, packet.Body, rest, wantBody)
	}
	if len(rest) != 0 {
		t.Fatalf("town response replayed a second self transition: %x", rest)
	}
	stored, found, err := repositories.Character.Load(context.Background(), "29")
	if err != nil || !found {
		t.Fatalf("load stored character found=%t err=%v", found, err)
	}
	if stored.Stats["town_id"] != 38 || stored.Stats["area_id"] != 0 ||
		stored.Stats["pos_x"] != 900 || stored.Stats["pos_y"] != 250 || stored.Stats["direction"] != 5 {
		t.Fatalf("stored town location=%+v", stored.Stats)
	}
	if session.currentFinishLoadingStateSent ||
		session.currentFinishLoadingCompletionSent ||
		session.postFinishLoadingPlayerStateSent ||
		session.initialTownLegacySceneReadyAccepted {
		t.Fatalf("town transition state was not rearmed: current=%t completion=%t post=%t legacy=%t",
			session.currentFinishLoadingStateSent,
			session.currentFinishLoadingCompletionSent,
			session.postFinishLoadingPlayerStateSent,
			session.initialTownLegacySceneReadyAccepted)
	}
}

func TestHandleTownSetUserAreaDoesNotReplaceMainInventoryWithAccountSharedRows(t *testing.T) {
	service, session, repositories := newTownMoveTest(t)
	if err := repositories.Inventory.Save(context.Background(), dnfrepo.InventoryRecord{
		CharacterID: "29",
		Slots:       map[string]dnfrepo.ItemStack{},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.AccountInventory.Save(context.Background(), dnfrepo.AccountInventoryRecord{
		AccountID: "dnf:1",
		Slots: map[string]dnfrepo.ItemStack{
			dnfrepo.AccountSharedInventorySlotKey(dnfrepo.CrystalWarehouseFirstSlot): {
				ItemID: 3033,
				Count:  2,
			},
			dnfrepo.AccountSharedInventorySlotKey(dnfrepo.SoulWarehouseFirstSlot + 3): {
				ItemID: 10099774,
				Count:  5,
			},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := service.handleTownSetUserArea(session, buildTownMoveRequest(38, 0, 900, 250, 5)); err != nil {
		t.Fatal(err)
	}
	transition, rest := splitTownAreaTransitionSequence(
		t,
		session.conn.(*bufferConn).write.Bytes(),
		29,
		38,
		0,
		900,
		250,
		5,
		3,
	)
	if transition.Header.Classification != 0 || transition.Header.MsgID != currentSceneTransitionMsgID {
		t.Fatalf("town transition header=%+v body=%x", transition.Header, transition.Body)
	}
	if len(rest) != 0 {
		t.Fatalf("town transition replaced the main inventory with an account-only snapshot: %x", rest)
	}
}

func TestHandleTownSetUserAreaSendsCompleteAreaRosterWithSelectedActorLast(t *testing.T) {
	service, session, _ := newTownMoveTest(t)
	service.onlinePlayers = newOnlinePlayerManager()
	service.onlinePlayers.EnterArea(&onlinePlayerInfo{
		CharacterID: 17,
		ChannelID:   townPresenceChannelID(session),
		TownID:      38,
		AreaID:      0,
		PositionX:   320,
		PositionY:   210,
		Direction:   1,
		AreaState:   4,
	})

	if err := service.handleTownSetUserArea(session, buildTownMoveRequest(38, 0, 900, 250, 5)); err != nil {
		t.Fatal(err)
	}
	packet, rest := splitTownAreaTransitionSequence(
		t,
		session.conn.(*bufferConn).write.Bytes(),
		29,
		38,
		0,
		900,
		250,
		5,
		3,
	)
	wantBody, err := buildCurrentSceneTransitionBody(38, 0, []currentSceneTransitionRow{
		{
			ObjectOrResourceKey: currentSceneActorObjectKey(17),
			Value1:              320,
			Value2:              210,
			Value3:              1,
			Value4:              4,
		},
		{
			ObjectOrResourceKey: currentSceneActorObjectKey(29),
			Value1:              900,
			Value2:              250,
			Value3:              5,
			Value4:              3,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if packet.Header.MsgID != currentSceneTransitionMsgID ||
		packet.Header.Classification != 0 ||
		!bytes.Equal(packet.Body, wantBody) {
		t.Fatalf(
			"complete area roster header=%+v body=%x rest=%x want=%x",
			packet.Header,
			packet.Body,
			rest,
			wantBody,
		)
	}
	if len(rest) != 0 {
		t.Fatalf("complete area roster replayed peer packets without a live peer session: %x", rest)
	}
}

func TestHandleTownSetUserAreaPreservesStationaryPartyRosterWithAreaUpdate(t *testing.T) {
	service, session, repositories := newTownMoveTest(t)
	service.options.gameUpperBodyCodec = gameUpperBodyCodecPlain
	service.onlinePlayers = newOnlinePlayerManager()
	service.bindGameSessionCharacter(session, session.selectedCharacterID)
	service.onlinePlayers.EnterArea(&onlinePlayerInfo{
		CharacterID: 29,
		AccountID:   "dnf:1",
		Name:        "mover",
		Level:       1,
		TownID:      38,
		AreaID:      1,
		PositionX:   450,
		PositionY:   234,
		Direction:   5,
		AreaState:   3,
		Session:     session,
	})

	peerCharacter := dnfrepo.CharacterRecord{
		CharacterID: "17",
		AccountID:   "dnf:2",
		Name:        "peer",
		Job:         "1",
		Level:       2,
		Stats: map[string]int64{
			"town_id": 38, "area_id": 1, "pos_x": 400, "pos_y": 200,
			"direction": 5, "area_state": 3,
		},
	}
	if err := repositories.Character.Save(context.Background(), peerCharacter); err != nil {
		t.Fatal(err)
	}
	peerConn := &bufferConn{}
	peer := &gameSession{
		conn:                      peerConn,
		channel:                   session.channel,
		residentChannel:           session.residentChannel,
		selectedCharacterID:       17,
		townSceneReadyCharacterID: 17,
		townActorOwnerChannel:     byte(session.channel.ID),
	}
	service.bindGameSessionCharacter(peer, 17)
	service.onlinePlayers.EnterArea(&onlinePlayerInfo{
		CharacterID: 17,
		AccountID:   "dnf:2",
		Name:        "peer",
		Job:         1,
		Level:       2,
		TownID:      38,
		AreaID:      1,
		PositionX:   400,
		PositionY:   200,
		Direction:   5,
		AreaState:   3,
		Session:     peer,
	})
	state := alignedcmd.PartyState{
		PartyID: 1, UserID: 29, UserState: 1, IsLeader: true, MaxMembers: 4,
		Members: []alignedcmd.PartyMemberState{
			{UserID: 29, UserState: 1},
			{UserID: 17, UserState: 1},
		},
	}
	storeRuntimePartyState(session, state)
	storeRuntimePartyState(peer, state)

	if err := service.handleTownSetUserArea(session, buildTownMoveRequest(38, 0, 900, 250, 5)); err != nil {
		t.Fatal(err)
	}
	transition, rest := splitTownAreaTransitionSequence(
		t,
		session.conn.(*bufferConn).write.Bytes(),
		29,
		38,
		0,
		900,
		250,
		5,
		3,
	)
	if transition.Header.MsgID != currentSceneTransitionMsgID {
		t.Fatalf("first packet msg=%d want town transition", transition.Header.MsgID)
	}
	remoteMode0, rest := splitGameServerUpperPacket(t, rest)
	remoteMode1, rest := splitGameServerUpperPacket(t, rest)
	remoteParty, rest := splitGameServerUpperPacket(t, rest)
	// The remote actor's op9 record carries this party's model, so the observing
	// member's own frame is closed immediately with its op153 health rows
	// (broadcastCurrentPartyActorProjection): a frame left on the bare op9 shows
	// empty HP/MP bars for every teammate.
	rest = assertRuntimePartyRealtimeRows(t, rest, 2)
	remoteArea, rest := splitCurrentGameServerUpperPacketAuto(t, rest)
	if remoteMode0.Header.MsgID != uint16(dnfenum.CmdPacketSetUDPIPPort) || len(remoteMode0.Body) == 0 || remoteMode0.Body[0] != 0 ||
		remoteMode1.Header.MsgID != uint16(dnfenum.CmdPacketSetUDPIPPort) || len(remoteMode1.Body) == 0 || remoteMode1.Body[0] != 1 ||
		remoteParty.Header.MsgID != uint16(dnfenum.CmdPacketRecoverStamina) ||
		remoteArea.Header.MsgID != currentTownUserAreaNotificationMsgID {
		t.Fatalf("remote party actor sequence mode0=%+v mode1=%+v op9=%+v area=%+v",
			remoteMode0.Header, remoteMode1.Header, remoteParty.Header, remoteArea.Header)
	}
	if len(remoteArea.Body) < 10 || remoteArea.Body[2] != 38 || remoteArea.Body[3] != 1 {
		t.Fatalf("remote party actor area body=%x want town=38 area=1", remoteArea.Body)
	}
	assertCurrentPartyFrameSelectedSlot(t, rest, 0)
	rest = assertRuntimePartySceneRefresh(t, rest, 2)
	if len(rest) != 0 {
		t.Fatalf("post-town party roster has unexpected trailing packets=%x", rest)
	}
	peerArea, peerRest := splitCurrentGameServerUpperPacketAuto(t, peerConn.write.Bytes())
	if peerArea.Header.MsgID != currentTownUserAreaNotificationMsgID ||
		len(peerArea.Body) < 10 || peerArea.Body[2] != 38 || peerArea.Body[3] != 0 {
		t.Fatalf("stationary peer area update header=%+v body=%x", peerArea.Header, peerArea.Body)
	}
	if len(peerRest) != 0 {
		t.Fatalf("stationary peer received actor or party-table replay after area-only update: %x", peerRest)
	}

	// When the second member follows into the same area, both clients now own
	// the newcomer's actor. The stationary client must finish with one op9
	// roster rebind, without replaying op153/op11 and restarting P2P.
	session.conn.(*bufferConn).write.Reset()
	peerConn.write.Reset()
	if err := service.handleTownSetUserArea(peer, buildTownMoveRequest(38, 0, 870, 248, 6)); err != nil {
		t.Fatal(err)
	}
	peerTransition, peerRest := splitTownAreaTransitionSequence(
		t,
		peerConn.write.Bytes(),
		17,
		38,
		0,
		870,
		248,
		6,
		3,
	)
	wantPeerTransition, err := buildCurrentSceneTransitionBody(38, 0, []currentSceneTransitionRow{
		{
			ObjectOrResourceKey: currentSceneActorObjectKey(29),
			Value1:              900,
			Value2:              250,
			Value3:              5,
			Value4:              3,
		},
		{
			ObjectOrResourceKey: currentSceneActorObjectKey(17),
			Value1:              870,
			Value2:              248,
			Value3:              6,
			Value4:              3,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(peerTransition.Body, wantPeerTransition) {
		t.Fatalf("party follower op24 body=%x want=%x", peerTransition.Body, wantPeerTransition)
	}
	// 城镇共现只发 mode0（2026-09-26）：mode1 是接收者自己的属性/装备快照，客户端已停用
	// 战斗力、没有任何 mode1 消费者，服务端不再给 peer 发。
	peerRemoteMode0, peerRest := splitGameServerUpperPacket(t, peerRest)
	peerRemoteParty, peerRest := splitGameServerUpperPacket(t, peerRest)
	peerRest = assertRuntimePartyRealtimeRows(t, peerRest, 2)
	peerRemoteArea, peerRest := splitCurrentGameServerUpperPacketAuto(t, peerRest)
	if peerRemoteMode0.Header.MsgID != uint16(dnfenum.CmdPacketSetUDPIPPort) || len(peerRemoteMode0.Body) == 0 || peerRemoteMode0.Body[0] != 0 ||
		peerRemoteParty.Header.MsgID != uint16(dnfenum.CmdPacketRecoverStamina) ||
		peerRemoteArea.Header.MsgID != currentTownUserAreaNotificationMsgID || len(peerRemoteArea.Body) < 10 || peerRemoteArea.Body[3] != 0 {
		t.Fatalf("party follower remote sequence mode0=%+v op9=%+v area=%+v body=%x",
			peerRemoteMode0.Header, peerRemoteParty.Header, peerRemoteArea.Header, peerRemoteArea.Body)
	}
	assertCurrentPartyFrameSelectedSlot(t, peerRest, 0)
	peerRest = assertRuntimePartySceneRefresh(t, peerRest, 2)
	if len(peerRest) != 0 {
		t.Fatalf("party follower transition left unparsed packets=%x", peerRest)
	}
	stationary := session.conn.(*bufferConn).write.Bytes()
	// 同上：城镇共现不发 mode1。
	newcomerMode0, stationary := splitGameServerUpperPacket(t, stationary)
	newcomerParty, stationary := splitGameServerUpperPacket(t, stationary)
	stationary = assertRuntimePartyRealtimeRows(t, stationary, 2)
	newcomerArea, stationary := splitCurrentGameServerUpperPacketAuto(t, stationary)
	if newcomerMode0.Header.MsgID != uint16(dnfenum.CmdPacketSetUDPIPPort) || len(newcomerMode0.Body) == 0 || newcomerMode0.Body[0] != 0 ||
		newcomerParty.Header.MsgID != uint16(dnfenum.CmdPacketRecoverStamina) ||
		newcomerArea.Header.MsgID != currentTownUserAreaNotificationMsgID || len(newcomerArea.Body) < 10 || newcomerArea.Body[3] != 0 {
		t.Fatalf("same-area newcomer sequence mode0=%+v op9=%+v area=%+v body=%x",
			newcomerMode0.Header, newcomerParty.Header, newcomerArea.Header, newcomerArea.Body)
	}
	stationary = assertRuntimePartySceneRefresh(t, stationary, 2)
	if len(stationary) != 0 {
		t.Fatalf("same-area stationary peer received unexpected trailing packets: %x", stationary)
	}
}

func TestHandleTownSetUserAreaRejectsColdLoginBeforeTownActorReadyWithoutMutation(t *testing.T) {
	service, session, repositories := newTownMoveTest(t)
	session.townSceneReadyCharacterID = 0
	session.initialTownRouteCharacterID = session.selectedCharacterID
	session.initialTownRouteStage = currentInitialTownRouteActorBound

	if err := service.handleTownSetUserArea(session, buildTownMoveRequest(38, 0, 900, 250, 5)); err != nil {
		t.Fatal(err)
	}
	if got := session.conn.(*bufferConn).write.Bytes(); len(got) != 0 {
		t.Fatalf("cold-login op36 wrote=%x", got)
	}
	stored, found, err := repositories.Character.Load(context.Background(), "29")
	if err != nil || !found || stored.Stats["town_id"] != 38 || stored.Stats["area_id"] != 1 ||
		stored.Stats["pos_x"] != 450 || stored.Stats["pos_y"] != 234 || stored.Stats["direction"] != 5 {
		t.Fatalf("cold-login op36 mutated character found=%t err=%v stats=%+v", found, err, stored.Stats)
	}
}

func TestHandleTownSetUserAreaRefreshesPVFDBQuestListAfterFullDungeonReturn(t *testing.T) {
	service, session, runtime := newBackToVillageRuntime(t)
	service.questCatalog = buildQuestListTestCatalog(t)
	townTable, err := dnftown.Load(context.Background(), townMoveTestSource{
		"town/town.lst":        "7 `return_town.twn`",
		"town/return_town.twn": "[area]\n3 `return_town.map` `[normal]`\n[/area]\n[name]\n`ReturnTown`",
	}, dnftown.Options{})
	if err != nil {
		t.Fatal(err)
	}
	service.townCatalog = townTable
	repositories, ok := service.repositoryGroup()
	if !ok || repositories.Character == nil {
		t.Fatal("return fixture character repository unavailable")
	}
	character, found, err := repositories.Character.Load(context.Background(), "99")
	if err != nil || !found {
		t.Fatalf("load return character found=%t err=%v", found, err)
	}
	character.Job = "2"
	character.Level = 2
	if err := repositories.Character.Save(context.Background(), character); err != nil {
		t.Fatal(err)
	}

	if err := service.handleDungeonBackToVillage(session, nil); err != nil {
		t.Fatalf("start pending return: %v", err)
	}
	if !runtime.townReturnPending || session.dungeon.runtime != nil {
		t.Fatalf("full return state pending=%t owner=%p", runtime.townReturnPending, session.dungeon.runtime)
	}
	connection := session.conn.(*bufferConn)
	// An interrupted (op42/op132) return never replays op356/op21/op574 quest
	// state: no dungeon quest finished, so the client still owns the pre-entry
	// quest state and the map [quest condition] gates it was left with. The
	// reference active-giveup route publishes only the actor restore and the
	// typed op24 below; quest markers refresh on the follow-up town move.
	if got := townPostTransitionPacketSignatures(t, connection.write.Bytes()); !reflect.DeepEqual(
		got,
		[]string{"op42", "op376", "mode0", "mode1", "op205", "op24", "op559", "op33weakness"},
	) {
		t.Fatalf("interrupted return route=%v", got)
	}
	session.connectionTownActorOwnerChannel = byte(session.channel.ID)
	session.townActorOwnerChannel = byte(session.channel.ID)
	wantSkillBody := enableTownMoveSkillProjection(t, service, repositories, "99")
	connection.write.Reset()

	if err := service.handleTownSetUserArea(session, buildTownMoveRequest(7, 3, 474, 234, 5)); err != nil {
		t.Fatal(err)
	}
	if session.dungeon.runtime != nil {
		t.Fatalf("town request restored dungeon runtime=%+v", session.dungeon.runtime)
	}
	_ = wantSkillBody
	townPacket, rest := splitTownAreaTransitionSequence(t, connection.write.Bytes(), 99, 7, 3, 474, 234, 5, 3)
	if townPacket.Header.Classification != 0 || townPacket.Header.MsgID != currentSceneTransitionMsgID {
		t.Fatalf("first packet after confirmed return header=%+v body=%x", townPacket.Header, townPacket.Body)
	}
	crystalPacket, rest := splitGameServerUpperPacket(t, rest)
	if crystalPacket.Header.Classification != 0 ||
		crystalPacket.Header.MsgID != currentCrystalContractStateMsgID ||
		!bytes.Equal(crystalPacket.Body, []byte{0, 0xff}) || len(rest) != 0 {
		t.Fatalf(
			"crystal state after confirmed return header=%+v body=%x trailing=%x",
			crystalPacket.Header,
			crystalPacket.Body,
			rest,
		)
	}
}

func TestHandleTownSetUserAreaDoesNotRefreshQuestsForOrdinaryTownRoomMove(t *testing.T) {
	service, session, _ := newTownMoveTest(t)
	service.questCatalog = buildQuestListTestCatalog(t)
	if err := service.handleTownSetUserArea(session, buildTownMoveRequest(38, 0, 900, 250, 5)); err != nil {
		t.Fatal(err)
	}
	packet, rest := splitTownAreaTransitionSequence(
		t,
		session.conn.(*bufferConn).write.Bytes(),
		29,
		38,
		0,
		900,
		250,
		5,
		3,
	)
	if packet.Header.MsgID != currentSceneTransitionMsgID || packet.Header.Classification != 0 || len(rest) != 0 {
		t.Fatalf("ordinary town move header=%+v body=%x trailing=%x", packet.Header, packet.Body, rest)
	}
}

func TestHandleTownSetUserAreaSeedsCompletedQuestSceneProjectionBeforeOp24(t *testing.T) {
	service, session, repositories := newTownMoveTest(t)
	completedAt := time.Date(2026, 8, 4, 1, 30, 0, 0, time.UTC)
	if err := repositories.Quest.Save(context.Background(), dnfrepo.QuestRecord{
		CharacterID: "29",
		States: map[int64]dnfrepo.QuestState{
			3162: {Status: "completed", ProgressValue: 1, UpdatedAt: completedAt},
		},
	}); err != nil {
		t.Fatal(err)
	}

	if err := service.handleTownSetUserArea(session, buildTownMoveRequest(38, 0, 900, 250, 5)); err != nil {
		t.Fatal(err)
	}
	clearQuest, transition, trailing := splitTownAreaTransitionSequenceWithClearQuest(
		t,
		session.conn.(*bufferConn).write.Bytes(),
		29,
		38,
		0,
		900,
		250,
		5,
		3,
	)
	if transition.Header.Classification != 0 || transition.Header.MsgID != currentSceneTransitionMsgID || len(trailing) != 0 {
		t.Fatalf("completed quest restore transition=%+v trailing=%x", transition.Header, trailing)
	}
	plain, err := zlibDecompress(clearQuest.Body)
	if err != nil {
		t.Fatal(err)
	}
	if len(plain) != 4+currentClearQuestListPayloadSize || plain[4+3162] != 1 {
		t.Fatalf("completed quest restore len=%d flag=%d", len(plain), plain[4+3162])
	}
	persisted, found, err := repositories.Quest.Load(context.Background(), "29")
	if err != nil || !found || persisted.States[3162].Status != "completed" || !persisted.States[3162].UpdatedAt.Equal(completedAt) {
		t.Fatalf("completed quest projection mutated persistence found=%t err=%v state=%+v", found, err, persisted.States[3162])
	}
}

func TestHandleTownSetUserAreaRejectsMissingAreaAndUnprovedQuestOrCrossTownRoutes(t *testing.T) {
	tests := []struct {
		name string
		body []byte
	}{
		{name: "missing area", body: buildTownMoveRequest(38, 99, 1, 2, 5)},
		{name: "ordinary quest gated", body: buildTownMoveRequest(38, 3, 1, 2, 5)},
		{name: "ordinary cross town", body: buildTownMoveRequest(39, 0, 1, 2, 5)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			service, session, repositories := newTownMoveTest(t)
			if err := service.handleTownSetUserArea(session, test.body); err != nil {
				t.Fatal(err)
			}
			if got := session.conn.(*bufferConn).write.Bytes(); len(got) != 0 {
				t.Fatalf("blocked move wrote=%x", got)
			}
			stored, found, err := repositories.Character.Load(context.Background(), "29")
			if err != nil || !found || stored.Stats["town_id"] != 38 || stored.Stats["area_id"] != 1 ||
				stored.Stats["pos_x"] != 450 || stored.Stats["pos_y"] != 234 {
				t.Fatalf("blocked move changed character found=%t err=%v stats=%+v", found, err, stored.Stats)
			}
		})
	}
}

func TestHandleTownSetUserAreaAllowsOnlyAuthenticatedRobotPublicPlacement(t *testing.T) {
	service, session, repositories := newTownMoveTest(t)
	session.robotSessionBound = true
	if err := service.handleTownSetUserArea(session, buildTownMoveRequest(39, 0, 640, 240, 5)); err != nil {
		t.Fatal(err)
	}
	stored, found, err := repositories.Character.Load(context.Background(), "29")
	if err != nil || !found || stored.Stats["town_id"] != 39 || stored.Stats["area_id"] != 0 ||
		stored.Stats["pos_x"] != 640 || stored.Stats["pos_y"] != 240 {
		t.Fatalf("robot public placement not committed found=%t err=%v stats=%+v", found, err, stored.Stats)
	}
	if got := session.conn.(*bufferConn).write.Bytes(); len(got) == 0 {
		t.Fatal("robot public placement emitted no town transition")
	}

	service, session, repositories = newTownMoveTest(t)
	session.robotSessionBound = true
	if err := service.handleTownSetUserArea(session, buildTownMoveRequest(38, 3, 571, 315, 5)); err != nil {
		t.Fatal(err)
	}
	stored, found, err = repositories.Character.Load(context.Background(), "29")
	if err != nil || !found || stored.Stats["town_id"] != 38 || stored.Stats["area_id"] != 3 {
		t.Fatalf("robot assigned quest-gated placement not committed found=%t err=%v stats=%+v", found, err, stored.Stats)
	}

	service, session, repositories = newTownMoveTest(t)
	session.robotSessionBound = true
	if err := service.handleTownSetUserArea(session, buildTownMoveRequest(107, 0, 640, 240, 5)); err != nil {
		t.Fatal(err)
	}
	if got := session.conn.(*bufferConn).write.Bytes(); len(got) != 0 {
		t.Fatalf("robot non-public placement emitted packets: %x", got)
	}
	stored, found, err = repositories.Character.Load(context.Background(), "29")
	if err != nil || !found || stored.Stats["town_id"] != 38 || stored.Stats["area_id"] != 1 {
		t.Fatalf("robot non-public placement changed location found=%t err=%v stats=%+v", found, err, stored.Stats)
	}
}

func TestHandleTownSetUserAreaRejectsTeleportArrayTargetsWithUnmetPVFNeedQuests(t *testing.T) {
	tests := []struct {
		name string
		body []byte
	}{
		{name: "quest gated same town", body: buildTownTeleportArrayRequest(38, 3, 571, 315, 5)},
		{name: "cross town quest gated", body: buildTownTeleportArrayRequest(39, 2, 418, 209, 5)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			service, session, repositories := newTownMoveTest(t)
			if err := service.handleTownSetUserArea(session, test.body); err != nil {
				t.Fatal(err)
			}
			if got := session.conn.(*bufferConn).write.Bytes(); len(got) != 0 {
				t.Fatalf("unmet teleport prerequisite emitted packets: %x", got)
			}
			stored, found, err := repositories.Character.Load(context.Background(), "29")
			if err != nil || !found || stored.Stats["town_id"] != 38 || stored.Stats["area_id"] != 1 ||
				stored.Stats["pos_x"] != 450 || stored.Stats["pos_y"] != 234 {
				t.Fatalf("unmet teleport changed character found=%t err=%v stats=%+v", found, err, stored.Stats)
			}
			if _, found, err := repositories.Quest.Load(context.Background(), "29"); err != nil || found {
				t.Fatalf("unmet teleport persisted quest completion found=%t err=%v", found, err)
			}
		})
	}
}

func TestHandleTownSetUserAreaRejectsWorldMapStationTailZeroTargetWithUnmetNeedQuest(t *testing.T) {
	service, session, repositories := newTownMoveTest(t)
	character, found, err := repositories.Character.Load(context.Background(), "29")
	if err != nil || !found {
		t.Fatalf("load character found=%t err=%v", found, err)
	}
	character.Stats["town_id"] = 40
	character.Stats["area_id"] = 1
	if err := repositories.Character.Save(context.Background(), character); err != nil {
		t.Fatal(err)
	}

	if err := service.handleTownSetUserArea(session, buildTownMapStationRequest(40, 0, 1519, 489, 5)); err != nil {
		t.Fatal(err)
	}
	if got := session.conn.(*bufferConn).write.Bytes(); len(got) != 0 {
		t.Fatalf("unmet map-station prerequisite emitted packets: %x", got)
	}
	stored, found, err := repositories.Character.Load(context.Background(), "29")
	if err != nil || !found || stored.Stats["town_id"] != 40 || stored.Stats["area_id"] != 1 ||
		stored.Stats["pos_x"] != 450 || stored.Stats["pos_y"] != 234 {
		t.Fatalf("unmet map-station changed character found=%t err=%v stats=%+v", found, err, stored.Stats)
	}
	if _, found, err := repositories.Quest.Load(context.Background(), "29"); err != nil || found {
		t.Fatalf("unmet map-station persisted quest completion found=%t err=%v", found, err)
	}
}

func TestHandleTownSetUserAreaDoesNotPersistTransportNeedQuest(t *testing.T) {
	service, session, repositories := newTownMoveTest(t)
	character, found, err := repositories.Character.Load(context.Background(), "29")
	if err != nil || !found {
		t.Fatalf("load character found=%t err=%v", found, err)
	}
	character.Stats["town_id"] = 40
	character.Stats["area_id"] = 1
	character.Stats["pos_x"] = 777
	character.Stats["pos_y"] = 333
	if err := repositories.Character.Save(context.Background(), character); err != nil {
		t.Fatal(err)
	}
	if err := service.handleTownSetUserArea(session, buildTownMapStationRequest(40, 0, 1519, 489, 5)); err != nil {
		t.Fatal(err)
	}
	if got := session.conn.(*bufferConn).write.Bytes(); len(got) != 0 {
		t.Fatalf("unmet transport prerequisite emitted town packets: %x", got)
	}
	stored, found, err := repositories.Character.Load(context.Background(), "29")
	if err != nil || !found || stored.Stats["town_id"] != 40 || stored.Stats["area_id"] != 1 ||
		stored.Stats["pos_x"] != 777 || stored.Stats["pos_y"] != 333 {
		t.Fatalf("unmet transport prerequisite moved character found=%t err=%v stats=%+v", found, err, stored.Stats)
	}
	if _, found, err := repositories.Quest.Load(context.Background(), "29"); err != nil || found {
		t.Fatalf("unmet transport prerequisite persisted completion found=%t err=%v", found, err)
	}
}

func TestHandleTownSetUserAreaAcceptsCrossTownSeriaRoomPortalWithOneOp24(t *testing.T) {
	service, session, repositories := newTownMoveTest(t)
	character, found, err := repositories.Character.Load(context.Background(), "29")
	if err != nil || !found {
		t.Fatalf("load character found=%t err=%v", found, err)
	}
	character.Stats["town_id"] = 40
	character.Stats["area_id"] = 0
	character.Stats["pos_x"] = 1519
	character.Stats["pos_y"] = 489
	if err := repositories.Character.Save(context.Background(), character); err != nil {
		t.Fatal(err)
	}

	if err := service.handleTownSetUserArea(session, buildTownSeriaRoomPortalRequest(38, 1, 433, 311, 5, 40)); err != nil {
		t.Fatal(err)
	}
	packet, rest := splitTownAreaTransitionSequence(
		t,
		session.conn.(*bufferConn).write.Bytes(),
		29,
		38,
		1,
		433,
		311,
		5,
		3,
	)
	if packet.Header.MsgID != currentSceneTransitionMsgID || packet.Header.Classification != 0 || len(rest) != 0 {
		t.Fatalf("Seria portal response header=%+v body=%x trailing=%x", packet.Header, packet.Body, rest)
	}
	stored, found, err := repositories.Character.Load(context.Background(), "29")
	if err != nil || !found || stored.Stats["town_id"] != 38 || stored.Stats["area_id"] != 1 ||
		stored.Stats["pos_x"] != 433 || stored.Stats["pos_y"] != 311 {
		t.Fatalf("Seria portal location found=%t err=%v stats=%+v", found, err, stored.Stats)
	}
}

func TestHandleTownSetUserAreaAcceptsCrossTownPortalTailZero(t *testing.T) {
	t.Run("west coast to HendonMyre", func(t *testing.T) {
		service, session, repositories := newTownMoveTest(t)
		character, found, err := repositories.Character.Load(context.Background(), "29")
		if err != nil || !found {
			t.Fatalf("load character found=%t err=%v", found, err)
		}
		character.Stats["town_id"] = 40
		character.Stats["area_id"] = 0
		if err := repositories.Character.Save(context.Background(), character); err != nil {
			t.Fatal(err)
		}

		if err := service.handleTownSetUserArea(session, buildTownCrossTownPortalRequest(39, 0, 3547, 270, 5, 40)); err != nil {
			t.Fatal(err)
		}
		packet, rest := splitTownAreaTransitionSequence(t, session.conn.(*bufferConn).write.Bytes(), 29, 39, 0, 3547, 270, 5, 3)
		if packet.Header.MsgID != currentSceneTransitionMsgID || packet.Header.Classification != 0 || len(rest) != 0 {
			t.Fatalf("cross-town portal response header=%+v body=%x trailing=%x", packet.Header, packet.Body, rest)
		}
		stored, found, err := repositories.Character.Load(context.Background(), "29")
		if err != nil || !found || stored.Stats["town_id"] != 39 || stored.Stats["area_id"] != 0 ||
			stored.Stats["pos_x"] != 3547 || stored.Stats["pos_y"] != 270 {
			t.Fatalf("cross-town portal location found=%t err=%v stats=%+v", found, err, stored.Stats)
		}
		if area, found := service.townArea(39, 0); !found || area.MapPath != "new_HendonMyre.map" {
			t.Fatalf("target map found=%t area=%+v", found, area)
		}
	})

	t.Run("HendonMyre to west coast need quest", func(t *testing.T) {
		service, session, repositories := newTownMoveTest(t)
		character, found, err := repositories.Character.Load(context.Background(), "29")
		if err != nil || !found {
			t.Fatalf("load character found=%t err=%v", found, err)
		}
		character.Stats["town_id"] = 39
		character.Stats["area_id"] = 0
		character.Stats["pos_x"] = 701
		character.Stats["pos_y"] = 802
		if err := repositories.Character.Save(context.Background(), character); err != nil {
			t.Fatal(err)
		}

		if err := service.handleTownSetUserArea(session, buildTownCrossTownPortalRequest(40, 0, 1519, 489, 5, 39)); err != nil {
			t.Fatal(err)
		}
		if got := session.conn.(*bufferConn).write.Bytes(); len(got) != 0 {
			t.Fatalf("unmet cross-town prerequisite emitted packets: %x", got)
		}
		stored, found, err := repositories.Character.Load(context.Background(), "29")
		if err != nil || !found || stored.Stats["town_id"] != 39 || stored.Stats["area_id"] != 0 ||
			stored.Stats["pos_x"] != 701 || stored.Stats["pos_y"] != 802 {
			t.Fatalf("unmet cross-town prerequisite changed character found=%t err=%v stats=%+v", found, err, stored.Stats)
		}
		if _, found, err := repositories.Quest.Load(context.Background(), "29"); err != nil || found {
			t.Fatalf("unmet cross-town prerequisite persisted quest completion found=%t err=%v", found, err)
		}
	})
}

func TestHandleTownSetUserAreaAcceptsPVFProvenNPCEnterTownRoute(t *testing.T) {
	service, session, repositories := newTownMoveTest(t)
	character, found, err := repositories.Character.Load(context.Background(), "29")
	if err != nil || !found {
		t.Fatalf("load character found=%t err=%v", found, err)
	}
	character.Level = 50
	character.Stats["town_id"] = 38
	character.Stats["area_id"] = 0
	if err := repositories.Character.Save(context.Background(), character); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Quest.Save(context.Background(), dnfrepo.QuestRecord{
		CharacterID: "29",
		States: map[int64]dnfrepo.QuestState{
			8618: {Status: "completed"},
		},
	}); err != nil {
		t.Fatal(err)
	}

	if err := service.handleTownSetUserArea(session, buildTownNPCEnterRequest(107, 0, 916, 224, 5, 38, 20276)); err != nil {
		t.Fatal(err)
	}
	_, packet, _ := splitTownAreaTransitionSequenceWithClearQuest(t, session.conn.(*bufferConn).write.Bytes(), 29, 107, 0, 916, 224, 5, 3)
	if packet.Header.MsgID != currentSceneTransitionMsgID {
		t.Fatalf("NPC enter-town response header=%+v", packet.Header)
	}
	stored, found, err := repositories.Character.Load(context.Background(), "29")
	if err != nil || !found || stored.Stats["town_id"] != 107 || stored.Stats["area_id"] != 0 ||
		stored.Stats["pos_x"] != 916 || stored.Stats["pos_y"] != 224 {
		t.Fatalf("NPC enter-town location found=%t err=%v stats=%+v", found, err, stored.Stats)
	}
	if !session.townTransportEnterSelectPending {
		t.Fatal("NPC enter-town route did not arm the destination dungeon selector")
	}
}

func TestHandleTownSetUserAreaRejectsUnprovedNPCEnterTownRoute(t *testing.T) {
	service, session, repositories := newTownMoveTest(t)
	character, found, err := repositories.Character.Load(context.Background(), "29")
	if err != nil || !found {
		t.Fatalf("load character found=%t err=%v", found, err)
	}
	character.Level = 50
	character.Stats["town_id"] = 38
	character.Stats["area_id"] = 0
	if err := repositories.Character.Save(context.Background(), character); err != nil {
		t.Fatal(err)
	}

	if err := service.handleTownSetUserArea(session, buildTownNPCEnterRequest(107, 0, 916, 224, 5, 38, 20277)); err != nil {
		t.Fatal(err)
	}
	if got := session.conn.(*bufferConn).write.Bytes(); len(got) != 0 {
		t.Fatalf("unproved NPC route emitted packets: %x", got)
	}
	stored, found, err := repositories.Character.Load(context.Background(), "29")
	if err != nil || !found || stored.Stats["town_id"] != 38 || stored.Stats["area_id"] != 0 {
		t.Fatalf("unproved NPC route changed location found=%t err=%v stats=%+v", found, err, stored.Stats)
	}
}

func TestHandlePrevVillageReturnsFromSeriaRoomToBoundOrigin(t *testing.T) {
	service, session, repositories := newTownMoveTest(t)
	character, found, err := repositories.Character.Load(context.Background(), "29")
	if err != nil || !found {
		t.Fatalf("load character found=%t err=%v", found, err)
	}
	character.Stats["town_id"] = 40
	character.Stats["area_id"] = 0
	character.Stats["pos_x"] = 1519
	character.Stats["pos_y"] = 489
	if err := repositories.Character.Save(context.Background(), character); err != nil {
		t.Fatal(err)
	}
	session.townPositionSnapshot = currentTownPositionSnapshot{
		CharacterID:   29,
		TownID:        40,
		AreaID:        0,
		PositionX:     1500,
		PositionY:     500,
		MovementCode:  5,
		PositionValid: true,
	}

	if err := service.handleTownSetUserArea(session, buildTownSeriaRoomPortalRequest(38, 1, 433, 311, 5, 40)); err != nil {
		t.Fatal(err)
	}
	_, enterSeriaTrailing := splitTownAreaTransitionSequence(
		t,
		session.conn.(*bufferConn).write.Bytes(),
		29,
		38,
		1,
		433,
		311,
		5,
		3,
	)
	if len(enterSeriaTrailing) != 0 {
		t.Fatalf("entering Seria left unparsed transition packets=%x", enterSeriaTrailing)
	}
	if session.townPrevVillageSnapshot.CharacterID != 29 ||
		session.townPrevVillageSnapshot.TownID != 40 ||
		session.townPrevVillageSnapshot.AreaID != 0 ||
		session.townPrevVillageSnapshot.PositionX != 1500 ||
		session.townPrevVillageSnapshot.PositionY != 500 {
		t.Fatalf("prev-village origin after entering Seria=%+v", session.townPrevVillageSnapshot)
	}
	storedSeria, found, err := repositories.Character.Load(context.Background(), "29")
	if err != nil || !found ||
		storedSeria.Stats[currentTownPrevVillageTownStat] != 40 ||
		storedSeria.Stats[currentTownPrevVillageAreaStat] != 0 ||
		storedSeria.Stats[currentTownPrevVillagePosXStat] != 1500 ||
		storedSeria.Stats[currentTownPrevVillagePosYStat] != 500 ||
		storedSeria.Stats[currentTownPrevVillageDirectionStat] != 5 {
		t.Fatalf("entering Seria did not persist prev-village origin found=%t err=%v stats=%+v", found, err, storedSeria.Stats)
	}
	connection := session.conn.(*bufferConn)
	connection.write.Reset()
	completedAt := time.Date(2026, 9, 1, 8, 50, 0, 0, time.UTC)
	if err := repositories.Quest.Save(context.Background(), dnfrepo.QuestRecord{
		CharacterID: "29",
		States: map[int64]dnfrepo.QuestState{
			4840: {Status: "completed", ProgressValue: 1, UpdatedAt: completedAt},
		},
	}); err != nil {
		t.Fatal(err)
	}

	if err := service.handleGameCommand(session, byte(dnfenum.GameCmdCommand), uint16(dnfenum.CmdPacketPrevVillage), nil); err != nil {
		t.Fatal(err)
	}
	clearQuest, packet, rest := splitTownAreaTransitionSequenceWithClearQuest(
		t,
		connection.write.Bytes(),
		29,
		40,
		0,
		1500,
		500,
		5,
		3,
	)
	if len(rest) != 0 {
		t.Fatalf("prev-village response header=%+v body=%x trailing=%x", packet.Header, packet.Body, rest)
	}
	plain, err := zlibDecompress(clearQuest.Body)
	if err != nil {
		t.Fatal(err)
	}
	if len(plain) != 4+currentClearQuestListPayloadSize || plain[4+4840] != 1 {
		t.Fatalf("prev-village completed quest projection len=%d flag=%d", len(plain), plain[4+4840])
	}
	wantBody, err := buildCurrentSceneTransitionBody(40, 0, []currentSceneTransitionRow{{
		ObjectOrResourceKey: 29,
		Value1:              1500,
		Value2:              500,
		Value3:              5,
		Value4:              3,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(packet.Body, wantBody) {
		t.Fatalf("prev-village body=%x want=%x", packet.Body, wantBody)
	}
	stored, found, err := repositories.Character.Load(context.Background(), "29")
	if err != nil || !found || stored.Stats["town_id"] != 40 || stored.Stats["area_id"] != 0 ||
		stored.Stats["pos_x"] != 1500 || stored.Stats["pos_y"] != 500 {
		t.Fatalf("prev-village location found=%t err=%v stats=%+v", found, err, stored.Stats)
	}
	if session.townPrevVillageSnapshot.PositionValid {
		t.Fatalf("prev-village origin was not cleared after return: %+v", session.townPrevVillageSnapshot)
	}
}

func TestHandlePrevVillageRestoresPersistedOriginAfterRelog(t *testing.T) {
	service, _, repositories := newTownMoveTest(t)
	character, found, err := repositories.Character.Load(context.Background(), "29")
	if err != nil || !found {
		t.Fatalf("load character found=%t err=%v", found, err)
	}
	character.Stats["town_id"] = newCharacterInitialTownID
	character.Stats["area_id"] = newCharacterInitialAreaID
	character.Stats["pos_x"] = newCharacterInitialPosX
	character.Stats["pos_y"] = newCharacterInitialPosY
	character.Stats["direction"] = newCharacterInitialDirection
	character.Stats[currentTownPrevVillageTownStat] = 40
	character.Stats[currentTownPrevVillageAreaStat] = 0
	character.Stats[currentTownPrevVillagePosXStat] = 1500
	character.Stats[currentTownPrevVillagePosYStat] = 500
	character.Stats[currentTownPrevVillageDirectionStat] = 5
	if err := repositories.Character.Save(context.Background(), character); err != nil {
		t.Fatal(err)
	}

	channel := channelcatalog.Channel{ServerID: 1, ID: 19, Type: 1, Name: "ch.19", Port: 10019}
	reloginSession := &gameSession{
		conn:                            &bufferConn{},
		channel:                         channel,
		residentChannel:                 channel,
		connectionTownActorOwnerChannel: byte(channel.ID),
		townActorOwnerChannel:           byte(channel.ID),
		selectedCharacterID:             29,
		townSceneReadyCharacterID:       29,
	}
	if err := service.handleGameCommand(reloginSession, byte(dnfenum.GameCmdCommand), uint16(dnfenum.CmdPacketPrevVillage), nil); err != nil {
		t.Fatal(err)
	}
	packet, rest := splitTownAreaTransitionSequence(
		t,
		reloginSession.conn.(*bufferConn).write.Bytes(),
		29,
		40,
		0,
		1500,
		500,
		5,
		3,
	)
	if len(rest) != 0 {
		t.Fatalf("persisted prev-village response header=%+v body=%x trailing=%x", packet.Header, packet.Body, rest)
	}
	wantBody, err := buildCurrentSceneTransitionBody(40, 0, []currentSceneTransitionRow{{
		ObjectOrResourceKey: 29,
		Value1:              1500,
		Value2:              500,
		Value3:              5,
		Value4:              3,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(packet.Body, wantBody) {
		t.Fatalf("persisted prev-village body=%x want=%x", packet.Body, wantBody)
	}
	stored, found, err := repositories.Character.Load(context.Background(), "29")
	if err != nil || !found || stored.Stats["town_id"] != 40 || stored.Stats["area_id"] != 0 ||
		stored.Stats["pos_x"] != 1500 || stored.Stats["pos_y"] != 500 || stored.Stats["direction"] != 5 {
		t.Fatalf("persisted prev-village location found=%t err=%v stats=%+v", found, err, stored.Stats)
	}
}

func TestHandlePrevVillageTransitionWriteFailureRollsBackLocation(t *testing.T) {
	service, session, repositories := newTownMoveTest(t)
	character, found, err := repositories.Character.Load(context.Background(), "29")
	if err != nil || !found {
		t.Fatalf("load character found=%t err=%v", found, err)
	}
	character.Stats["town_id"] = newCharacterInitialTownID
	character.Stats["area_id"] = newCharacterInitialAreaID
	character.Stats["pos_x"] = newCharacterInitialPosX
	character.Stats["pos_y"] = newCharacterInitialPosY
	character.Stats["direction"] = newCharacterInitialDirection
	character.Stats[currentTownPrevVillageTownStat] = 40
	character.Stats[currentTownPrevVillageAreaStat] = 0
	character.Stats[currentTownPrevVillagePosXStat] = 1500
	character.Stats[currentTownPrevVillagePosYStat] = 500
	character.Stats[currentTownPrevVillageDirectionStat] = 5
	if err := repositories.Character.Save(context.Background(), character); err != nil {
		t.Fatal(err)
	}

	wantErr := errors.New("previous-village op24 write failed")
	session.conn = &failNthDungeonWriteConn{failAt: 3, err: wantErr}
	if err := service.handleGameCommand(
		session,
		byte(dnfenum.GameCmdCommand),
		uint16(dnfenum.CmdPacketPrevVillage),
		nil,
	); !errors.Is(err, wantErr) {
		t.Fatalf("previous-village op24 failure=%v want=%v", err, wantErr)
	}
	stored, found, loadErr := repositories.Character.Load(context.Background(), "29")
	if loadErr != nil || !found ||
		stored.Stats["town_id"] != newCharacterInitialTownID ||
		stored.Stats["area_id"] != newCharacterInitialAreaID ||
		stored.Stats["pos_x"] != newCharacterInitialPosX ||
		stored.Stats["pos_y"] != newCharacterInitialPosY {
		t.Fatalf("previous-village op24 rollback found=%t err=%v stats=%+v", found, loadErr, stored.Stats)
	}
}

func TestHandleTownSetUserAreaWriteFailureRollsBackLocation(t *testing.T) {
	for _, test := range []struct {
		name   string
		failAt int
	}{
		{name: "op22", failAt: 1},
		{name: "op23", failAt: 2},
		{name: "op24", failAt: 3},
	} {
		t.Run(test.name, func(t *testing.T) {
			service, session, repositories := newTownMoveTest(t)
			wantErr := fmt.Errorf("town %s write failed", test.name)
			session.conn = &failNthDungeonWriteConn{failAt: test.failAt, err: wantErr}
			err := service.handleTownSetUserArea(session, buildTownMoveRequest(38, 0, 900, 250, 5))
			if !errors.Is(err, wantErr) {
				t.Fatalf("write error=%v want=%v", err, wantErr)
			}
			stored, found, loadErr := repositories.Character.Load(context.Background(), "29")
			if loadErr != nil || !found || stored.Stats["town_id"] != 38 || stored.Stats["area_id"] != 1 ||
				stored.Stats["pos_x"] != 450 || stored.Stats["pos_y"] != 234 {
				t.Fatalf("rollback found=%t err=%v stats=%+v", found, loadErr, stored.Stats)
			}
		})
	}
}

func TestTownSetUserAreaCurrentWriterBoundaryAndClass(t *testing.T) {
	service, session, repositories := newTownMoveTest(t)
	for _, body := range [][]byte{make([]byte, currentTownSetUserAreaBodySize-1), make([]byte, currentTownSetUserAreaBodySize+1)} {
		if err := service.handleTownSetUserArea(session, body); err != nil {
			t.Fatal(err)
		}
	}
	packet, err := dnfproto.BuildChannelPacket(36, buildTownMoveRequest(38, 0, 900, 250, 5), 1, 2)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.handleGameUpper(session, packet); err != nil {
		t.Fatal(err)
	}
	if got := session.conn.(*bufferConn).write.Bytes(); len(got) != 0 {
		t.Fatalf("invalid boundary/class wrote=%x", got)
	}
	stored, found, err := repositories.Character.Load(context.Background(), "29")
	if err != nil || !found || stored.Stats["area_id"] != 1 {
		t.Fatalf("invalid boundary/class changed character found=%t err=%v stats=%+v", found, err, stored.Stats)
	}
}

func TestInitialTownTransitionUsesPersistedNormalAreaWithoutMutation(t *testing.T) {
	service, session, repositories := newTownMoveTest(t)
	character, found, err := repositories.Character.Load(context.Background(), "29")
	if err != nil || !found {
		t.Fatalf("load character found=%t err=%v", found, err)
	}
	character.Stats["town_id"] = 39
	character.Stats["area_id"] = 0
	character.Stats["pos_x"] = 70
	character.Stats["pos_y"] = 80
	if err := repositories.Character.Save(context.Background(), character); err != nil {
		t.Fatal(err)
	}

	updated, townID, areaID, row, mapPath, err := service.currentInitialTownTransition(
		context.Background(),
		session,
		29,
		character,
	)
	if err != nil {
		t.Fatal(err)
	}
	if townID != 39 || areaID != 0 || row.Value1 != 70 || row.Value2 != 80 ||
		row.Value3 != 5 || row.Value4 != 3 || mapPath != "new_HendonMyre.map" ||
		updated.Stats["town_id"] != 39 || updated.Stats["area_id"] != 0 {
		t.Fatalf("persisted transition town=%d area=%d row=%+v map=%q stats=%+v", townID, areaID, row, mapPath, updated.Stats)
	}
	stored, found, err := repositories.Character.Load(context.Background(), "29")
	if err != nil || !found || stored.Stats["town_id"] != 39 || stored.Stats["area_id"] != 0 ||
		stored.Stats["pos_x"] != 70 || stored.Stats["pos_y"] != 80 ||
		stored.Stats["direction"] != 5 || stored.Stats["area_state"] != 3 {
		t.Fatalf("login mutated persisted location found=%t err=%v stats=%+v", found, err, stored.Stats)
	}
}

func TestInitialTownTransitionLeavesPersistedPVFNeedQuestAreaReadOnly(t *testing.T) {
	service, session, repositories := newTownMoveTest(t)
	character, found, err := repositories.Character.Load(context.Background(), "29")
	if err != nil || !found {
		t.Fatalf("load character found=%t err=%v", found, err)
	}
	character.Stats["town_id"] = 38
	character.Stats["area_id"] = 3
	character.Stats["pos_x"] = 571
	character.Stats["pos_y"] = 315
	if err := repositories.Character.Save(context.Background(), character); err != nil {
		t.Fatal(err)
	}

	_, townID, areaID, _, mapPath, err := service.currentInitialTownTransition(
		context.Background(),
		session,
		29,
		character,
	)
	if err != nil {
		t.Fatal(err)
	}
	if townID != 38 || areaID != 3 || mapPath != "Elvengard_hendon.map" {
		t.Fatalf("persisted need-quest transition town=%d area=%d map=%q", townID, areaID, mapPath)
	}
	if _, found, err := repositories.Quest.Load(context.Background(), "29"); err != nil || found {
		t.Fatalf("persisted transition wrote quest completion found=%t err=%v", found, err)
	}
	plain := buildCurrentClearQuestListBody(dnfrepo.QuestRecord{}, false)
	if got := plain[4+3155]; got != 0 {
		t.Fatalf("persisted need quest op356 flag=%d, want 0", got)
	}
}

func TestCharacterListLoginTransitionPersistsRuntimePVFSeriaGate(t *testing.T) {
	service, session, repositories := newTownMoveTest(t)
	character, found, err := repositories.Character.Load(context.Background(), "29")
	if err != nil || !found {
		t.Fatalf("load character found=%t err=%v", found, err)
	}
	character.Stats["town_id"] = 39
	character.Stats["area_id"] = 0
	character.Stats["pos_x"] = 70
	character.Stats["pos_y"] = 80
	if err := repositories.Character.Save(context.Background(), character); err != nil {
		t.Fatal(err)
	}

	updated, townID, areaID, row, mapPath, err := service.currentCharacterListLoginTransition(
		context.Background(),
		session,
		29,
		character,
	)
	if err != nil {
		t.Fatal(err)
	}
	if townID != newCharacterInitialTownID || areaID != newCharacterInitialAreaID ||
		row.Value1 != newCharacterInitialPosX || row.Value2 != newCharacterInitialPosY ||
		row.Value3 != newCharacterInitialDirection || row.Value4 != newCharacterInitialAreaState ||
		mapPath != "new_seria_room.map" {
		t.Fatalf("Seria login transition town=%d area=%d row=%+v map=%q", townID, areaID, row, mapPath)
	}
	if updated.Stats["town_id"] != newCharacterInitialTownID || updated.Stats["area_id"] != newCharacterInitialAreaID ||
		updated.Stats["pos_x"] != newCharacterInitialPosX || updated.Stats["pos_y"] != newCharacterInitialPosY {
		t.Fatalf("returned Seria login state=%+v", updated.Stats)
	}
	if updated.Stats[currentTownPrevVillageTownStat] != 39 ||
		updated.Stats[currentTownPrevVillageAreaStat] != 0 ||
		updated.Stats[currentTownPrevVillagePosXStat] != 70 ||
		updated.Stats[currentTownPrevVillagePosYStat] != 80 ||
		updated.Stats[currentTownPrevVillageDirectionStat] != 5 ||
		session.townPrevVillageSnapshot.CharacterID != 29 ||
		session.townPrevVillageSnapshot.TownID != 39 ||
		session.townPrevVillageSnapshot.AreaID != 0 ||
		session.townPrevVillageSnapshot.PositionX != 70 ||
		session.townPrevVillageSnapshot.PositionY != 80 {
		t.Fatalf("character-list login did not preserve prev-village origin stats=%+v snapshot=%+v", updated.Stats, session.townPrevVillageSnapshot)
	}
	stored, found, err := repositories.Character.Load(context.Background(), "29")
	if err != nil || !found || stored.Stats["town_id"] != newCharacterInitialTownID || stored.Stats["area_id"] != newCharacterInitialAreaID ||
		stored.Stats["pos_x"] != newCharacterInitialPosX || stored.Stats["pos_y"] != newCharacterInitialPosY ||
		stored.Stats["direction"] != newCharacterInitialDirection || stored.Stats["area_state"] != newCharacterInitialAreaState {
		t.Fatalf("persisted Seria login state found=%t err=%v stats=%+v", found, err, stored.Stats)
	}
	if stored.Stats[currentTownPrevVillageTownStat] != 39 ||
		stored.Stats[currentTownPrevVillageAreaStat] != 0 ||
		stored.Stats[currentTownPrevVillagePosXStat] != 70 ||
		stored.Stats[currentTownPrevVillagePosYStat] != 80 ||
		stored.Stats[currentTownPrevVillageDirectionStat] != 5 {
		t.Fatalf("persisted Seria login lost prev-village origin stats=%+v", stored.Stats)
	}
}

func TestAntonRaidChannelLoginUsesTemporaryPVFGateWithoutMutatingOrdinaryLocation(t *testing.T) {
	service, session, repositories := newTownMoveTest(t)
	session.channel.Type = dnfenum.RaidChannelType
	character, found, err := repositories.Character.Load(context.Background(), "29")
	if err != nil || !found {
		t.Fatalf("load character found=%t err=%v", found, err)
	}
	character.Level = 90
	character.Stats["town_id"] = 39
	character.Stats["area_id"] = 0
	character.Stats["pos_x"] = 70
	character.Stats["pos_y"] = 80
	if err := repositories.Character.Save(context.Background(), character); err != nil {
		t.Fatal(err)
	}

	updated, townID, areaID, row, mapPath, err := service.currentCharacterListLoginTransition(
		context.Background(),
		session,
		29,
		character,
	)
	if err != nil {
		t.Fatal(err)
	}
	if townID != byte(currentAntonRaidTownID) || areaID != byte(currentAntonRaidAreaID) ||
		row.Value1 != 474 || row.Value2 != 234 || row.Value3 != currentAntonRaidDirection ||
		row.Value4 != currentAntonRaidAreaState || mapPath != "Town/BattleShip/Gate_BattleShip.map" {
		t.Fatalf("Anton raid login transition town=%d area=%d row=%+v map=%q", townID, areaID, row, mapPath)
	}
	if updated.Stats["town_id"] != 39 || updated.Stats["area_id"] != 0 ||
		updated.Stats["pos_x"] != 70 || updated.Stats["pos_y"] != 80 {
		t.Fatalf("Anton raid transition rewrote returned ordinary location stats=%+v", updated.Stats)
	}
	stored, found, err := repositories.Character.Load(context.Background(), "29")
	if err != nil || !found || stored.Stats["town_id"] != 39 || stored.Stats["area_id"] != 0 ||
		stored.Stats["pos_x"] != 70 || stored.Stats["pos_y"] != 80 {
		t.Fatalf("Anton raid login mutated ordinary location found=%t err=%v stats=%+v", found, err, stored.Stats)
	}

	_, reconnectTownID, reconnectAreaID, reconnectRow, reconnectMapPath, err := service.currentChannelReconnectTownTransition(
		context.Background(),
		session,
		29,
		character,
	)
	if err != nil {
		t.Fatal(err)
	}
	if reconnectTownID != townID || reconnectAreaID != areaID || reconnectRow != row || reconnectMapPath != mapPath {
		t.Fatalf("Anton raid reconnect transition town=%d area=%d row=%+v map=%q", reconnectTownID, reconnectAreaID, reconnectRow, reconnectMapPath)
	}
}

func TestAntonRaidTownSceneSnapshotValidatesOp36AgainstTemporaryTown(t *testing.T) {
	service, session, repositories := newTownMoveTest(t)
	session.channel.Type = dnfenum.RaidChannelType
	session.townPositionSnapshot = currentTownPositionSnapshot{
		CharacterID:   29,
		TownID:        byte(currentAntonRaidTownID),
		AreaID:        byte(currentAntonRaidAreaID),
		PositionX:     474,
		PositionY:     234,
		PositionValid: true,
	}
	character, found, err := repositories.Character.Load(context.Background(), "29")
	if err != nil || !found {
		t.Fatalf("load character found=%t err=%v", found, err)
	}
	character.Level = 90
	character.Stats["town_id"] = newCharacterInitialTownID
	character.Stats["area_id"] = newCharacterInitialAreaID
	if err := repositories.Character.Save(context.Background(), character); err != nil {
		t.Fatal(err)
	}

	if err := service.handleTownSetUserArea(session, buildTownMoveRequest(19, 1, 500, 234, 5)); err != nil {
		t.Fatal(err)
	}
	if got := session.conn.(*bufferConn).write.Len(); got == 0 {
		t.Fatal("Anton town op36 was rejected despite the session town snapshot")
	}
	stored, found, err := repositories.Character.Load(context.Background(), "29")
	if err != nil || !found ||
		stored.Stats["town_id"] != newCharacterInitialTownID ||
		stored.Stats["area_id"] != newCharacterInitialAreaID ||
		stored.Stats["pos_x"] != newCharacterInitialPosX ||
		stored.Stats["pos_y"] != newCharacterInitialPosY {
		t.Fatalf("Anton town op36 leaked temporary location into durable character state found=%t err=%v stats=%+v", found, err, stored.Stats)
	}
	if snapshot := session.townPositionSnapshot; snapshot.TownID != byte(currentAntonRaidTownID) ||
		snapshot.AreaID != 1 || snapshot.PositionValid {
		t.Fatalf("Anton town op36 did not advance temporary scene snapshot=%+v", snapshot)
	}
}

func TestAntonRaidWaitingAreaRequiresMembershipAndUsesRaidInstance(t *testing.T) {
	newWaitingMove := func(t *testing.T) (*Service, *gameSession) {
		t.Helper()
		service, session, _ := newTownMoveTest(t)
		session.channel.Type = dnfenum.RaidChannelType
		session.townPositionSnapshot = currentTownPositionSnapshot{
			CharacterID:   29,
			TownID:        byte(currentAntonRaidTownID),
			AreaID:        1,
			PositionX:     500,
			PositionY:     234,
			PositionValid: true,
		}
		service.onlinePlayers = newOnlinePlayerManager()
		service.onlinePlayers.EnterArea(&onlinePlayerInfo{
			CharacterID: 29,
			TownID:      byte(currentAntonRaidTownID),
			AreaID:      1,
			Session:     session,
		})
		return service, session
	}

	t.Run("no_membership", func(t *testing.T) {
		service, session := newWaitingMove(t)
		if err := service.handleTownSetUserArea(session, buildTownMoveRequest(19, 2, 600, 234, 5)); err != nil {
			t.Fatal(err)
		}
		if got := session.conn.(*bufferConn).write.Len(); got != 0 {
			t.Fatalf("membership-less waiting move wrote %d bytes", got)
		}
		presence, ok := service.onlinePlayers.PlayerForCharacter(29)
		if !ok || presence.AreaID != 1 || presence.InstanceKey != 0 {
			t.Fatalf("membership-less waiting move changed presence: %+v found=%t", presence, ok)
		}
	})

	t.Run("raid_instance", func(t *testing.T) {
		service, session := newWaitingMove(t)
		const raidKey = uint32(0x0017001d)
		service.raids = map[uint32]*runtimeRaidState{
			raidKey: {
				RaidKey:  raidKey,
				LeaderID: 29,
				Members:  []runtimeRaidMemberState{{CharID: 29, GroupIndex: 1, SlotOrder: 0}},
			},
		}
		if err := service.handleTownSetUserArea(session, buildTownMoveRequest(19, 2, 600, 234, 5)); err != nil {
			t.Fatal(err)
		}
		if got := session.conn.(*bufferConn).write.Len(); got == 0 {
			t.Fatal("raid member waiting move emitted no transition")
		}
		presence, ok := service.onlinePlayers.PlayerForCharacter(29)
		if !ok || presence.TownID != byte(currentAntonRaidTownID) ||
			presence.AreaID != currentAntonRaidWaitingAreaID || presence.InstanceKey != raidKey {
			t.Fatalf("raid waiting presence=%+v found=%t", presence, ok)
		}
	})
}

func TestNormalChannelReconnectRepairsLeakedAntonTownLocation(t *testing.T) {
	service, session, repositories := newTownMoveTest(t)
	character, found, err := repositories.Character.Load(context.Background(), "29")
	if err != nil || !found {
		t.Fatalf("load character found=%t err=%v", found, err)
	}
	character.Stats["town_id"] = currentAntonRaidTownID
	character.Stats["area_id"] = 1
	character.Stats["pos_x"] = 500
	character.Stats["pos_y"] = 234
	character.Stats[currentTownPrevVillageTownStat] = currentAntonRaidTownID
	character.Stats[currentTownPrevVillageAreaStat] = 0
	character.Stats[currentTownPrevVillagePosXStat] = 438
	character.Stats[currentTownPrevVillagePosYStat] = 347
	character.Stats[currentTownPrevVillageDirectionStat] = 5
	if err := repositories.Character.Save(context.Background(), character); err != nil {
		t.Fatal(err)
	}

	updated, townID, areaID, row, mapPath, err := service.currentChannelReconnectTownTransition(
		context.Background(),
		session,
		29,
		character,
	)
	if err != nil {
		t.Fatal(err)
	}
	if townID != newCharacterInitialTownID || areaID != newCharacterInitialAreaID ||
		row.Value1 != newCharacterInitialPosX || row.Value2 != newCharacterInitialPosY ||
		mapPath != "new_seria_room.map" {
		t.Fatalf("normal reconnect transition town=%d area=%d row=%+v map=%q", townID, areaID, row, mapPath)
	}
	if updated.Stats["town_id"] != newCharacterInitialTownID || updated.Stats["area_id"] != newCharacterInitialAreaID {
		t.Fatalf("normal reconnect returned leaked state stats=%+v", updated.Stats)
	}
	stored, found, err := repositories.Character.Load(context.Background(), "29")
	if err != nil || !found ||
		stored.Stats["town_id"] != newCharacterInitialTownID ||
		stored.Stats["area_id"] != newCharacterInitialAreaID ||
		stored.Stats["pos_x"] != newCharacterInitialPosX ||
		stored.Stats["pos_y"] != newCharacterInitialPosY ||
		stored.Stats[currentTownPrevVillageTownStat] == currentAntonRaidTownID ||
		stored.Stats[currentTownPrevVillageAreaStat] != 0 ||
		stored.Stats[currentTownPrevVillagePosXStat] != 0 ||
		stored.Stats[currentTownPrevVillagePosYStat] != 0 ||
		stored.Stats[currentTownPrevVillageDirectionStat] != 0 {
		t.Fatalf("normal reconnect did not repair leaked Anton location found=%t err=%v stats=%+v", found, err, stored.Stats)
	}
}

func TestInitialTownTransitionAcceptsPersistedAreaWithoutPVFGate(t *testing.T) {
	repositories := testRepositoryGroup()
	character := dnfrepo.CharacterRecord{
		CharacterID: "29",
		AccountID:   "dnf:1",
		Stats: map[string]int64{
			"town_id":    40,
			"area_id":    0,
			"pos_x":      70,
			"pos_y":      80,
			"direction":  5,
			"area_state": 3,
		},
	}
	if err := repositories.Character.Save(context.Background(), character); err != nil {
		t.Fatal(err)
	}
	table, err := dnftown.Load(context.Background(), townMoveTestSource{
		"town/town.lst":    "40 `no_gate.twn`",
		"town/no_gate.twn": "[area]\n0 `only_normal.map` `[normal]`\n[/area]\n[name]\n`NoGate`",
	}, dnftown.Options{})
	if err != nil {
		t.Fatal(err)
	}
	service := &Service{
		townCatalog:        table,
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
	}
	_, townID, areaID, row, mapPath, err := service.currentInitialTownTransition(
		context.Background(),
		&gameSession{selectedCharacterID: 29},
		29,
		character,
	)
	if err != nil || townID != 40 || areaID != 0 || row.Value1 != 70 || row.Value2 != 80 ||
		row.Value3 != 5 || row.Value4 != 3 || mapPath != "only_normal.map" {
		t.Fatalf("normal-area transition town=%d area=%d row=%+v map=%q err=%v", townID, areaID, row, mapPath, err)
	}
	stored, found, loadErr := repositories.Character.Load(context.Background(), "29")
	if loadErr != nil || !found || stored.Stats["town_id"] != 40 || stored.Stats["area_id"] != 0 ||
		stored.Stats["pos_x"] != 70 || stored.Stats["pos_y"] != 80 {
		t.Fatalf("login rewrote normal-area character found=%t err=%v stats=%+v", found, loadErr, stored.Stats)
	}
}

func TestInitialTownTransitionRejectsUnknownPersistedAreaWithoutMutation(t *testing.T) {
	service, session, repositories := newTownMoveTest(t)
	character, found, err := repositories.Character.Load(context.Background(), "29")
	if err != nil || !found {
		t.Fatalf("load character found=%t err=%v", found, err)
	}
	character.Stats["area_id"] = 99
	character.Stats["pos_x"] = 70
	character.Stats["pos_y"] = 80
	if err := repositories.Character.Save(context.Background(), character); err != nil {
		t.Fatal(err)
	}
	_, _, _, _, _, err = service.currentInitialTownTransition(context.Background(), session, 29, character)
	if err == nil {
		t.Fatal("unknown persisted area unexpectedly fell back to the town gate")
	}
	stored, found, err := repositories.Character.Load(context.Background(), "29")
	if err != nil || !found || stored.Stats["area_id"] != 99 || stored.Stats["pos_x"] != 70 || stored.Stats["pos_y"] != 80 {
		t.Fatalf("failed login mutated persisted location found=%t err=%v stats=%+v", found, err, stored.Stats)
	}
}

func newTownMoveTest(t *testing.T) (*Service, *gameSession, dnfrepo.Group) {
	t.Helper()
	repositories := testRepositoryGroup()
	if err := repositories.Character.Save(context.Background(), dnfrepo.CharacterRecord{
		CharacterID: "29",
		AccountID:   "dnf:1",
		Level:       1,
		Stats: map[string]int64{
			"town_id":    38,
			"area_id":    1,
			"pos_x":      450,
			"pos_y":      234,
			"direction":  5,
			"area_state": 3,
		},
	}); err != nil {
		t.Fatal(err)
	}
	list := "10 `Fair_PVP.twn` 19 `BattleShip_Raid.twn` 38 `new_Elvengard.twn` 39 `new_HendonMyre.twn` 40 `new_WestCoast.twn` 107 `event.twn`"
	pvp := "[area]\n0 `Fair_PVP/Gate_Hendon.map` `[gate]` 123 234\n[/area]\n[name]\n`PVP`"
	battleship := "[area]\n0 `Town/BattleShip/Gate_BattleShip.map` `[gate]` 474 234\n[/area]\n" +
		"[area]\n1 `Town/BattleShip/BattleShip_Main.map` `[normal]`\n[/area]\n" +
		"[area]\n2 `Town/BattleShip/BattleShip_Anton.map` `[normal]`\n[/area]\n" +
		"[name]\n`BattleShip`"
	elvengard := "[area]\n0 `new_Elvengard.map` `[normal]`\n[/area]\n" +
		"[area]\n1 `new_seria_room.map` `[gate]` 450 234\n[/area]\n" +
		"[area]\n3 `Elvengard_hendon.map`\n[need quest]\n3155\n[/need quest]\n`[dungeon gate]` 114\n[/area]\n[name]\n`Elvengard`"
	hendon := "[area]\n0 `new_HendonMyre.map` `[normal]`\n[/area]\n" +
		"[area]\n2 `new_HendonMyre_seria.map`\n[need quest]\n3156\n[/need quest]\n`[gate]` 612 244\n[/area]\n" +
		"[name]\n`HendonMyre`"
	westCoast := "[area]\n0 `WestCoast.map`\n[need quest]\n3156\n[/need quest]\n[/area]\n" +
		"[area]\n1 `WestCoast_post.map` `[gate]` 1016 189\n[/area]\n" +
		"[name]\n`WestCoast`"
	eventTown := "[area]\n0 `Town/Event/event.map`\n[need level]\n50\n[need quest]\n8618\n[/need quest]\n`[dungeon gate]` 114\n[/area]\n[name]\n`EventTown`"
	table, err := dnftown.Load(context.Background(), townMoveTestSource{
		"town/Fair_PVP.twn":        pvp,
		"town/BattleShip_Raid.twn": battleship,
		"town/town.lst":            list,
		"town/new_Elvengard.twn":   elvengard,
		"town/new_HendonMyre.twn":  hendon,
		"town/new_WestCoast.twn":   westCoast,
		"town/event.twn":           eventTown,
		"npc/npc.lst":              "20275 `entry.npc` 20276 `exit.npc` 20277 `unrelated.npc`",
		"npc/entry.npc":            "[role]\n`[enter town]` 20276\n[/role]",
		"npc/exit.npc":             "[role]\n`[exit town]` 0\n[/role]",
		"npc/unrelated.npc":        "[role]\n`[exit town]` 0\n[/role]",
		"map/Elvengard_hendon.map": "[NPC]\n20276 `[left]` 571 315 0\n[/NPC]",
		"map/new_HendonMyre.map":   "[NPC]\n20276 `[left]` 640 240 0\n[/NPC]",
		"map/Town/Event/event.map": "[NPC]\n20276 `[left]` 100 200 0 20277 `[left]` 300 400 0\n[/NPC]",
	}, dnftown.Options{})
	if err != nil {
		t.Fatal(err)
	}
	service := &Service{
		townCatalog: table,
		premiumCatalog: &currentPremiumCatalog{
			contractsByItem: make(map[int64]currentPremiumContractInfo),
			devilSlots:      make(map[uint32]currentPremiumDevilSlotInfo),
			crystalCubeIDs:  [6]int64{3033, 3034, 3035, 3036, 3037, 3262},
		},
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
	}
	channel := channelcatalog.Channel{ServerID: 1, ID: 19, Type: 1, Name: "ch.19", Port: 10019}
	session := &gameSession{
		conn:                            &bufferConn{},
		channel:                         channel,
		residentChannel:                 channel,
		connectionTownActorOwnerChannel: byte(channel.ID),
		townActorOwnerChannel:           byte(channel.ID),
		selectedCharacterID:             29,
		characterGeneration:             1,
		townSceneReadyCharacterID:       29,
	}
	enableTownMoveSkillProjection(t, service, repositories, "29")
	return service, session, repositories
}

func enableTownMoveSkillProjection(
	t *testing.T,
	service *Service,
	repositories dnfrepo.Group,
	characterID string,
) []byte {
	t.Helper()
	character, found, err := repositories.Character.Load(context.Background(), characterID)
	if err != nil || !found {
		t.Fatalf("load town skill character found=%t err=%v", found, err)
	}
	job, validJob := characterJobByte(character)
	if !validJob {
		job = 0
		character.Job = "0"
		if err := repositories.Character.Save(context.Background(), character); err != nil {
			t.Fatal(err)
		}
	}
	jobListPath := fmt.Sprintf("job%d.lst", job)
	skillPath := fmt.Sprintf("job%d/active.skl", job)
	catalog, err := buildSkillCatalogFromSource(context.Background(), initialEquipmentMemSource{
		"skill/skilllist.lst":  fmt.Sprintf("%d `%s`\n", job, jobListPath),
		"skill/" + jobListPath: "46 `" + skillPath + "`\n",
		"skill/" + skillPath:   "[skill type]\n`active`\n",
	})
	if err != nil {
		t.Fatal(err)
	}
	record := dnfrepo.SkillRecord{
		CharacterID: characterID,
		Skills: map[int64]dnfrepo.SkillState{
			46: {Level: 1, Enabled: true},
		},
		Layouts: map[int]dnfrepo.SkillLayout{
			currentSkillInfoTreeIndex: {0: 46},
			1:                         {0: 46},
		},
		Points: dnfrepo.SkillPointState{
			TotalSP:     20,
			RemainingSP: 20,
			SyncedLevel: 1,
		},
	}
	dnfrepo.SetSkillTree(&record, 1, dnfrepo.SkillTreeState{
		Skills: record.Skills,
		Points: record.Points,
	})
	if err := repositories.Skill.Save(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	service.initialSkillsByJob = map[byte][]initialSkillEntry{
		job: {{SkillID: 46, Level: 1}},
	}
	service.initialSPTable = map[int]int{1: 20}
	service.skillCatalog = catalog
	body, _, err := buildCurrentSceneSkillInfoBody(record, record.Layouts, currentSkillInfoTreeIndex)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func townMoveSkillProjectionBody(
	t *testing.T,
	repositories dnfrepo.Group,
	characterID string,
) []byte {
	t.Helper()
	record, found, err := repositories.Skill.Load(context.Background(), characterID)
	if err != nil || !found {
		t.Fatalf("load town skill projection found=%t err=%v", found, err)
	}
	body, _, err := buildCurrentSceneSkillInfoBody(record, record.Layouts, currentSkillInfoTreeIndex)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func buildTownMoveRequest(townID, areaID byte, x, y int16, direction byte) []byte {
	body := make([]byte, currentTownSetUserAreaBodySize)
	body[0] = townID
	body[1] = areaID
	binary.LittleEndian.PutUint16(body[2:4], uint16(x))
	binary.LittleEndian.PutUint16(body[4:6], uint16(y))
	body[6] = direction
	return body
}

func buildTownTeleportArrayRequest(townID, areaID byte, x, y int16, direction byte) []byte {
	body := buildTownMoveRequest(townID, areaID, x, y, direction)
	binary.LittleEndian.PutUint16(body[7:9], 38)
	binary.LittleEndian.PutUint16(body[9:11], 1)
	body[15] = 5
	return body
}

func buildTownMapStationRequest(townID, areaID byte, x, y int16, direction byte) []byte {
	body := buildTownMoveRequest(townID, areaID, x, y, direction)
	binary.LittleEndian.PutUint16(body[7:9], uint16(townID))
	binary.LittleEndian.PutUint16(body[9:11], 1)
	return body
}

func buildTownNPCEnterRequest(townID, areaID byte, x, y int16, direction byte, sourceTownID uint16, targetNPCID uint32) []byte {
	body := buildTownMoveRequest(townID, areaID, x, y, direction)
	binary.LittleEndian.PutUint16(body[7:9], sourceTownID)
	binary.LittleEndian.PutUint16(body[9:11], 1)
	binary.LittleEndian.PutUint32(body[11:15], targetNPCID)
	return body
}

func buildTownSeriaRoomPortalRequest(townID, areaID byte, x, y int16, direction byte, sourceTownID uint16) []byte {
	body := buildTownMoveRequest(townID, areaID, x, y, direction)
	binary.LittleEndian.PutUint16(body[7:9], sourceTownID)
	return body
}

func buildTownCrossTownPortalRequest(townID, areaID byte, x, y int16, direction byte, sourceTownID uint16) []byte {
	return buildTownSeriaRoomPortalRequest(townID, areaID, x, y, direction, sourceTownID)
}

func TestCharacterListLoginSeriaOp36ExitReturnsToPreservedTown(t *testing.T) {
	service, session, repositories := newTownMoveTest(t)
	character, found, err := repositories.Character.Load(context.Background(), "29")
	if err != nil || !found {
		t.Fatalf("load character found=%t err=%v", found, err)
	}
	character.Stats["town_id"] = 39
	character.Stats["area_id"] = 0
	character.Stats["pos_x"] = 856
	character.Stats["pos_y"] = 341
	character.Stats["direction"] = 8
	if err := repositories.Character.Save(context.Background(), character); err != nil {
		t.Fatal(err)
	}
	if _, _, _, _, _, err := service.currentCharacterListLoginTransition(
		context.Background(), session, 29, character,
	); err != nil {
		t.Fatal(err)
	}
	session.townMu.Lock()
	setCurrentTownPositionSceneLocked(session, 29, newCharacterInitialTownID, newCharacterInitialAreaID)
	session.townSceneReadyCharacterID = 29
	session.townMu.Unlock()

	connection := session.conn.(*bufferConn)
	connection.write.Reset()
	// Some bootstrap paths leave the Seria room with a literal op36 for the
	// shared Elvengard map (town 38 area 0) instead of op1425.
	if err := service.handleTownSetUserArea(
		session,
		buildTownMoveRequest(newCharacterInitialTownID, 0, 706, 182, newCharacterInitialDirection),
	); err != nil {
		t.Fatal(err)
	}
	stored, found, err := repositories.Character.Load(context.Background(), "29")
	if err != nil || !found || stored.Stats["town_id"] != 39 || stored.Stats["area_id"] != 0 ||
		stored.Stats["pos_x"] != 856 || stored.Stats["pos_y"] != 341 || stored.Stats["direction"] != 8 {
		t.Fatalf("op36 Seria exit did not restore preserved town found=%t err=%v stats=%+v", found, err, stored.Stats)
	}
	if session.townPositionSnapshot.TownID != 39 || session.townPositionSnapshot.AreaID != 0 ||
		session.townPrevVillageSnapshot.PositionValid || session.townTransportEnterSelectPending {
		t.Fatalf("op36 Seria exit session state=%+v prev=%+v transport_pending=%t",
			session.townPositionSnapshot,
			session.townPrevVillageSnapshot,
			session.townTransportEnterSelectPending,
		)
	}
	wantScene, err := buildCurrentSceneTransitionBody(39, 0, []currentSceneTransitionRow{{
		ObjectOrResourceKey: currentSceneActorObjectKey(29),
		Value1:              856,
		Value2:              341,
		Value3:              8,
		Value4:              newCharacterInitialAreaState,
	}})
	if err != nil {
		t.Fatal(err)
	}
	var gotScene []byte
	for _, packet := range splitAllCurrentGameServerUpperPackets(t, connection.write.Bytes()) {
		if packet.Header.MsgID == currentSceneTransitionMsgID {
			gotScene = packet.Body
		}
	}
	if !bytes.Equal(gotScene, wantScene) {
		t.Fatalf("op36 Seria exit scene=%x, want=%x", gotScene, wantScene)
	}
}

func TestHandleTownSetUserAreaSeriaOp36ExitWithoutPrevVillageStaysLiteral(t *testing.T) {
	service, session, repositories := newTownMoveTest(t)
	// The default test character sits inside the Seria room with no preserved
	// prev-village origin; the literal Elvengard exit must keep working.
	connection := session.conn.(*bufferConn)
	connection.write.Reset()
	if err := service.handleTownSetUserArea(
		session,
		buildTownMoveRequest(newCharacterInitialTownID, 0, 706, 182, newCharacterInitialDirection),
	); err != nil {
		t.Fatal(err)
	}
	stored, found, err := repositories.Character.Load(context.Background(), "29")
	if err != nil || !found || stored.Stats["town_id"] != 38 || stored.Stats["area_id"] != 0 ||
		stored.Stats["pos_x"] != 706 || stored.Stats["pos_y"] != 182 {
		t.Fatalf("literal op36 Seria exit was redirected found=%t err=%v stats=%+v", found, err, stored.Stats)
	}
	if session.townPositionSnapshot.TownID != 38 || session.townPositionSnapshot.AreaID != 0 {
		t.Fatalf("literal op36 Seria exit session snapshot=%+v", session.townPositionSnapshot)
	}
}

// Migrated from the 最新inal line together with the PVP channel work: the PVP
// channel login must route through the temporary PVF gate (town
// currentPVPTownID / area currentPVPAreaID, Fair_PVP/Gate_Hendon.map) and must
// leave the character's ordinary town location untouched both in the returned
// record and in the repository, on the first login and on channel reconnect.
func TestPVPChannelLoginUsesTemporaryPVFGateWithoutMutatingOrdinaryLocation(t *testing.T) {
	service, session, repositories := newTownMoveTest(t)
	session.channel = channelcatalog.Channel{ServerID: 1, ID: 501, Type: 24, Group: "pvp", Name: "ch.501", Port: 10501}
	session.residentChannel = session.channel
	character, found, err := repositories.Character.Load(context.Background(), "29")
	if err != nil || !found {
		t.Fatalf("load character found=%t err=%v", found, err)
	}
	updated, townID, areaID, row, mapPath, err := service.currentCharacterListLoginTransition(
		context.Background(), session, 29, character,
	)
	if err != nil {
		t.Fatal(err)
	}
	if townID != byte(currentPVPTownID) || areaID != byte(currentPVPAreaID) ||
		row.Value1 == 0 || row.Value2 == 0 || mapPath != "Fair_PVP/Gate_Hendon.map" {
		t.Fatalf("PVP login transition town=%d area=%d row=%+v map=%q", townID, areaID, row, mapPath)
	}
	if updated.Stats["town_id"] != 38 || updated.Stats["area_id"] != 1 ||
		updated.Stats["pos_x"] != 450 || updated.Stats["pos_y"] != 234 {
		t.Fatalf("PVP transition rewrote returned ordinary location stats=%+v", updated.Stats)
	}
	stored, found, err := repositories.Character.Load(context.Background(), "29")
	if err != nil || !found || stored.Stats["town_id"] != 38 || stored.Stats["area_id"] != 1 ||
		stored.Stats["pos_x"] != 450 || stored.Stats["pos_y"] != 234 {
		t.Fatalf("PVP transition mutated ordinary location found=%t err=%v stats=%+v", found, err, stored.Stats)
	}
	_, reconnectTownID, reconnectAreaID, reconnectRow, reconnectMapPath, err := service.currentChannelReconnectTownTransition(
		context.Background(), session, 29, character,
	)
	if err != nil || reconnectTownID != byte(currentPVPTownID) || reconnectAreaID != byte(currentPVPAreaID) ||
		reconnectRow != row || reconnectMapPath != mapPath {
		t.Fatalf("PVP reconnect transition town=%d area=%d row=%+v map=%q err=%v", reconnectTownID, reconnectAreaID, reconnectRow, reconnectMapPath, err)
	}
}

// TestCurrentTownSetUserAreaSeriaRoomTeleportArrayIsNotTheExitDoor separates
// the two op36 bodies a player can send from inside a Seria room. The exit
// door always names town 38 no matter which town the room was entered from,
// so the server rewrites it to the remembered origin; the teleport array names
// a destination the player picked, and 埃尔文防线 is town 38 - classifying that
// one as the door sent the player back to their previous town instead.
//
// Every body below is a live capture from runtime/logs/packet_log.txt
// (2026-09-10). The array bodies all carry opaqueA = the town the array stands
// in, opaqueB = 1 and a nonzero tail; everything else carries tail 0.
func TestCurrentTownSetUserAreaSeriaRoomTeleportArrayIsNotTheExitDoor(t *testing.T) {
	for name, probe := range map[string]struct {
		body  string
		array bool
	}{
		"array_to_elvengard_town38": {"2600d402d10005260001000000000006", true},
		"array_to_town40":           {"28012904f80005260001000000000005", true},
		"array_to_town39":           {"2702a801060105260001000000000005", true},
		"array_to_town39_again":     {"27021702e10005260001000000000005", true},
		"seria_room_exit_door":      {"2600f2059f0005080000000000000000", false},
		"cross_town_portal":         {"080049003b0105260000000000000000", false},
		"seria_room_entry_portal":   {"2601b101370105160001000000000000", false},
		"ordinary_route_click":      {"16034d00000105160003000000000000", false},
	} {
		raw, err := hex.DecodeString(probe.body)
		if err != nil {
			t.Fatal(err)
		}
		request, err := parseCurrentTownSetUserAreaRequest(raw)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got := request.LooksLikeTeleportArraySelection(); got != probe.array {
			t.Fatalf("%s: teleport array=%t, want %t (request=%+v)", name, got, probe.array, request)
		}
		// The redirect only ever fires on the door shape, and only that shape
		// may keep reaching it.
		redirectCandidate := request.TownID == byte(newCharacterInitialTownID) &&
			request.AreaID != byte(newCharacterInitialAreaID) &&
			!request.LooksLikeTeleportArraySelection()
		wantCandidate := name == "seria_room_exit_door"
		if redirectCandidate != wantCandidate {
			t.Fatalf("%s: seria-exit redirect candidate=%t, want %t", name, redirectCandidate, wantCandidate)
		}
	}
}
