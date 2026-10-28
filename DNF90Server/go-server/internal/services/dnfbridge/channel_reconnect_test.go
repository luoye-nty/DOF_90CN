package dnfbridge

import (
	"bytes"
	"context"
	"encoding/binary"
	"math/bits"
	"testing"

	"longheng.io/server/internal/modules/dnf/channelcatalog"
	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfproto "longheng.io/server/internal/modules/dnf/protocol"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

func TestChannelReconnectProbeBypassesNativeDecodeAndRosterGuardsIt(t *testing.T) {
	service, session, _ := newChannelReconnectTest(t)
	service.options.gameUpperClientBodyCodec = gameUpperClientBodyCodecNative

	probe, err := dnfproto.BuildChannelPacket(
		uint16(dnfenum.UpperMsgCharacterRoster),
		make([]byte, currentChannelReconnectProbeSize),
		1,
		dnfproto.DefaultChannelClassification,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.handleGameUpper(session, probe); err != nil {
		t.Fatalf("handle native op2 reconnect probe: %v", err)
	}
	if !session.channelReconnect {
		t.Fatal("native op2/31 was decoded or dropped instead of arming channel reconnect")
	}

	rosterService, rosterSession, _ := newChannelReconnectTest(t)
	rosterFrame, err := dnfproto.BuildChannelPacket(
		uint16(dnfenum.UpperMsgGetUserInfo),
		nil,
		2,
		dnfproto.DefaultChannelClassification,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := rosterService.handleGameUpper(rosterSession, rosterFrame); err != nil {
		t.Fatalf("handle op8 roster request: %v", err)
	}
	if !rosterSession.rosterRequested {
		t.Fatal("op8 did not mark the role roster as requested")
	}
	if err := rosterService.handleGameUpper(rosterSession, probe); err != nil {
		t.Fatalf("handle roster-guarded op2 reconnect probe: %v", err)
	}
	if rosterSession.channelReconnect {
		t.Fatal("op2/31 armed reconnect after the role roster was requested")
	}
}

func TestCurrentOp2ReconnectProbeDoesNotOverwriteNativePartyPeerPort(t *testing.T) {
	service := &Service{}
	session := &gameSession{
		conn:                &bufferConn{},
		connID:              "party-peer-registration",
		selectedCharacterID: 29,
	}
	service.bindGameSessionCharacter(session, 29)
	registerPartyPeerEndpointForTest(session, 7, 5063)
	body := make([]byte, currentChannelReconnectProbeSize)
	body[0] = 5
	copy(body[1:5], []byte{192, 168, 5, 7})
	copy(body[5:9], []byte{192, 168, 5, 7})
	// These bytes produced the live false endpoint 39077 when op2 was
	// incorrectly interpreted as SET_UDP_IP_PORT.
	binary.BigEndian.PutUint16(body[9:11], 39077)
	binary.LittleEndian.PutUint32(body[11:15], 1472)
	binary.LittleEndian.PutUint32(body[15:19], 12)
	copy(body[19:], []byte("4c0f3e70667f"))
	frame, err := dnfproto.BuildChannelPacket(
		uint16(dnfenum.UpperMsgCharacterRoster),
		body,
		1,
		dnfproto.DefaultChannelClassification,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.handleGameUpper(session, frame); err != nil {
		t.Fatal(err)
	}
	if got := currentPartyPeerPort(session); got != 5063 {
		t.Fatalf("op2 reconnect probe overwrote native peer port=%d want=5063", got)
	}
	if !session.channelReconnect {
		t.Fatal("op2 reconnect probe no longer armed reconnect state")
	}
}

func TestLegacyGetUserInfoMarksRosterAndPreventsFalseChannelReconnect(t *testing.T) {
	service, session, _ := newChannelReconnectTest(t)

	// Current live op8 has a three-byte body and is classified by the shared
	// stream splitter as legacy. It still represents an ordinary roster login.
	if err := service.handleGameCommand(
		session,
		byte(dnfenum.GameCmdCommand),
		uint16(dnfenum.GameTypeGetUserInfo),
		[]byte{0xff, 0xff, 0x02},
	); err != nil {
		t.Fatalf("handle legacy op8 roster request: %v", err)
	}
	if !session.rosterRequested {
		t.Fatal("legacy op8 did not mark the role roster as requested")
	}

	session.conn.(*bufferConn).write.Reset()
	request := make([]byte, 2)
	binary.LittleEndian.PutUint16(request, 29)
	frame, err := dnfproto.BuildChannelPacket(
		uint16(dnfenum.UpperMsgSelectCharacter),
		request,
		2,
		dnfproto.DefaultChannelClassification,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.handleGameUpper(session, frame); err != nil {
		t.Fatalf("handle roster-owned op4: %v", err)
	}
	if session.channelReconnect {
		t.Fatalf(
			"ordinary roster op4 entered reconnect lifecycle route=%t",
			session.channelReconnect,
		)
	}
	assertNoChannelReconnectResidentNotice(
		t,
		splitAllCurrentGameServerUpperPackets(t, session.conn.(*bufferConn).write.Bytes()),
	)
}

func TestChannelReconnectOp4RunsImmediateTargetTownRouteWithoutChangingPersistedLocation(t *testing.T) {
	service, session, repositories := newChannelReconnectTest(t)
	session.channel.ID = 253
	session.channel.Name = "ch.253"
	session.channel.NoticeName = "ch.253"
	session.channel.Port = 10253
	session.residentChannel = session.channel
	session.connectionTownActorOwnerChannel = 253
	session.townActorOwnerChannel = 253
	before, found, err := repositories.Character.Load(context.Background(), "29")
	if err != nil || !found {
		t.Fatalf("load pre-reconnect character found=%t err=%v", found, err)
	}
	locationBefore := currentReconnectLocationSnapshot(before)

	request := make([]byte, 2)
	binary.LittleEndian.PutUint16(request, 29)
	frame, err := dnfproto.BuildChannelPacket(
		uint16(dnfenum.UpperMsgSelectCharacter),
		request,
		2,
		dnfproto.DefaultChannelClassification,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.handleGameUpper(session, frame); err != nil {
		t.Fatalf("handle channel reconnect op4: %v", err)
	}

	packets := splitAllCurrentGameServerUpperPackets(t, session.conn.(*bufferConn).write.Bytes())
	assertImmediateChannelReconnectOrder(t, packets, 253)
	if session.channelReconnect || session.sceneBootstrapTailDeferred || !session.sceneBootstrapTailSent {
		t.Fatalf(
			"completed reconnect flags route=%t deferred=%t sent=%t",
			session.channelReconnect,
			session.sceneBootstrapTailDeferred,
			session.sceneBootstrapTailSent,
		)
	}

	after, found, err := repositories.Character.Load(context.Background(), "29")
	if err != nil || !found {
		t.Fatalf("load post-reconnect character found=%t err=%v", found, err)
	}
	if got := currentReconnectLocationSnapshot(after); got != locationBefore {
		t.Fatalf("channel reconnect changed durable location: before=%+v after=%+v", locationBefore, got)
	}
}

func TestChannelReconnectSendsOneSkillSnapshotBeforeOp24(t *testing.T) {
	service, session, _ := newChannelReconnectTest(t)
	connection := &bufferConn{}
	session.conn = connection
	session.selectedCharacterID = 29
	session.channelReconnect = true

	if err := service.sendCurrentChannelReconnectTownEntry(session); err != nil {
		t.Fatalf("channel reconnect: %v", err)
	}
	packets := splitAllCurrentGameServerUpperPackets(t, connection.write.Bytes())
	skillCount := 0
	skillPreloadIndex := -1
	skillIndex := -1
	fullMode1Index := -1
	transitionIndex := -1
	var skillPreloadBody []byte
	lastActorStateIndex := -1
	for index, packet := range packets {
		if packet.Header.MsgID == currentSkillInfoMsgID {
			skillCount++
			switch skillCount {
			case 1:
				skillPreloadIndex = index
				skillPreloadBody = append([]byte(nil), packet.Body...)
			case 2:
				skillIndex = index
				// The full equipment-bearing mode1 is the actor-state packet
				// written between the preload and the authoritative op19.
				fullMode1Index = lastActorStateIndex
				if !bytes.Equal(packet.Body, skillPreloadBody) {
					t.Fatalf("reconnect skill preload=%x final=%x, want identical selected-character projections", skillPreloadBody, packet.Body)
				}
			}
		}
		if packet.Header.MsgID == uint16(dnfenum.CmdPacketSetUDPIPPort) {
			lastActorStateIndex = index
		}
		if packetLooksLikeInitialTownTransition(packet) {
			transitionIndex = index
		}
	}
	// The weapon-validation preload precedes the full equipment-bearing mode1;
	// the authoritative op19 follows it and both stay before op24.
	if skillCount != 2 || skillPreloadIndex < 0 || skillIndex < 0 || transitionIndex < 0 ||
		fullMode1Index < 0 || skillPreloadIndex >= fullMode1Index || skillIndex <= fullMode1Index ||
		skillIndex >= transitionIndex {
		t.Fatalf("reconnect skill count=%d preload=%d final=%d full_mode1=%d transition=%d",
			skillCount, skillPreloadIndex, skillIndex, fullMode1Index, transitionIndex)
	}
	if session.initialTownRouteStage != currentInitialTownRoutePlayerStateSent ||
		session.initialTownSkillInfoPrepared || !session.initialTownSkillInfoSent || session.channelReconnect {
		t.Fatalf(
			"completed reconnect retry state stage=%d prepared=%t sent=%t reconnect=%t",
			session.initialTownRouteStage,
			session.initialTownSkillInfoPrepared,
			session.initialTownSkillInfoSent,
			session.channelReconnect,
		)
	}
}

func TestChannelReconnectPersistedTownResolverDoesNotUnlockNeedQuestArea(t *testing.T) {
	service, session, repositories := newChannelReconnectTest(t)
	character, found, err := repositories.Character.Load(context.Background(), "29")
	if err != nil || !found {
		t.Fatalf("load reconnect character found=%t err=%v", found, err)
	}
	character.Stats["town_id"] = 38
	character.Stats["area_id"] = 3
	character.Stats["pos_x"] = 571
	character.Stats["pos_y"] = 315
	character.Stats["direction"] = 4
	character.Stats["area_state"] = 6

	questWrites := &countingChannelReconnectQuestRepository{QuestRepository: repositories.Quest}
	repositories.Quest = questWrites
	service.repositoryProvider = func() (dnfrepo.Group, bool) { return repositories, true }
	_, townID, areaID, row, mapPath, err := service.currentChannelReconnectTownTransition(
		context.Background(),
		session,
		29,
		character,
	)
	if err != nil {
		t.Fatal(err)
	}
	if townID != 38 || areaID != 3 || row.Value1 != 571 || row.Value2 != 315 ||
		row.Value3 != 4 || row.Value4 != 6 || mapPath != "Elvengard_hendon.map" {
		t.Fatalf("read-only reconnect location town=%d area=%d row=%+v map=%q", townID, areaID, row, mapPath)
	}
	if questWrites.saveCount != 0 {
		t.Fatalf("channel reconnect invoked persisted-area need-quest mutator %d time(s)", questWrites.saveCount)
	}
	if questRecord, found, err := repositories.Quest.Load(context.Background(), "29"); err != nil || found {
		t.Fatalf("channel reconnect created quest state for need-quest area found=%t err=%v record=%+v", found, err, questRecord)
	}
}

func TestChannelReconnectOp1276IsAckOnlyAndEndpointProbesAreIgnored(t *testing.T) {
	service, session, _ := newChannelReconnectTest(t)
	session.selectedCharacterID = 29
	session.channelReconnect = true
	session.sceneBootstrapTailDeferred = true
	connection := session.conn.(*bufferConn)

	barrier, err := dnfproto.BuildChannelPacket(
		uint16(dnfenum.UpperMsgCheckUserConnection),
		nil,
		3,
		dnfproto.DefaultChannelClassification,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.handleGameUpper(session, barrier); err != nil {
		t.Fatalf("handle reconnect op1276: %v", err)
	}
	ack, rest := splitGameServerUpperPacket(t, connection.write.Bytes())
	if ack.Header.Classification != dnfproto.DefaultChannelClassification ||
		ack.Header.MsgID != uint16(dnfenum.UpperMsgCheckUserConnection) ||
		!bytes.Equal(ack.Body, upperSuccessBody(buildCurrentCheckUserConnectionSuccessPayload())) ||
		len(rest) != 0 {
		t.Fatalf("reconnect op1276 emitted more than its 13-byte ACK: header=%+v body=%x rest=%x", ack.Header, ack.Body, rest)
	}
	if !session.sceneBootstrapTailDeferred {
		t.Fatal("reconnect op1276 ran the ordinary deferred scene tail")
	}

	service.options.gameUpperClientBodyCodec = gameUpperClientBodyCodecNative
	for index, bodyLen := range []int{
		currentChannelReconnectDisplayProbeSize,
		reference90CNChannelReconnectDisplayProbeBodySize,
	} {
		connection.write.Reset()
		probe, err := dnfproto.BuildChannelPacket(
			uint16(dnfenum.UpperMsgGameEndpoint),
			make([]byte, bodyLen),
			uint16(4+index),
			dnfproto.DefaultChannelClassification,
		)
		if err != nil {
			t.Fatal(err)
		}
		if err := service.handleGameUpper(session, probe); err != nil {
			t.Fatalf("handle stale op1/%d probe: %v", bodyLen, err)
		}
		if connection.write.Len() != 0 {
			t.Fatalf("stale op1/%d probe emitted bytes=%x", bodyLen, connection.write.Bytes())
		}
	}
}

func TestChannelReconnectExactLifecycleRunsOnOp4AndNeverWaitsForGameEndpoint(t *testing.T) {
	service, session, _ := newChannelReconnectTest(t)
	service.options.gameUpperClientBodyCodec = gameUpperClientBodyCodecNative
	connection := session.conn.(*bufferConn)

	probe, err := dnfproto.BuildChannelPacket(
		uint16(dnfenum.UpperMsgCharacterRoster),
		make([]byte, currentChannelReconnectProbeSize),
		1,
		dnfproto.DefaultChannelClassification,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.handleGameUpper(session, probe); err != nil {
		t.Fatal(err)
	}
	if !session.channelReconnect {
		t.Fatal("native op2/31 did not arm channel reconnect")
	}

	// The generated op4 fixture is plaintext. The native op2/31 and stale
	// op1/590/598 edges are exercised at their real wire lengths.
	service.options.gameUpperClientBodyCodec = gameUpperClientBodyCodecPlain
	request := make([]byte, 2)
	binary.LittleEndian.PutUint16(request, 29)
	selectFrame, err := dnfproto.BuildChannelPacket(
		uint16(dnfenum.UpperMsgSelectCharacter),
		request,
		2,
		dnfproto.DefaultChannelClassification,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.handleGameUpper(session, selectFrame); err != nil {
		t.Fatal(err)
	}
	assertImmediateChannelReconnectOrder(
		t,
		splitAllCurrentGameServerUpperPackets(t, connection.write.Bytes()),
		byte(session.channel.ID),
	)
	if session.channelReconnect || !session.sceneBootstrapTailSent {
		t.Fatalf(
			"op4 did not finish reconnect route=%t tail_sent=%t",
			session.channelReconnect,
			session.sceneBootstrapTailSent,
		)
	}
	if session.townActorOwnerChannel != byte(session.channel.ID) {
		t.Fatalf(
			"op4 reconnect owner reset to %d, want CHANNELINFO context %d",
			session.townActorOwnerChannel,
			session.channel.ID,
		)
	}

	connection.write.Reset()
	heartbeat, err := dnfproto.BuildChannelPacket(
		uint16(dnfenum.UpperMsgCheckUserConnection),
		nil,
		3,
		dnfproto.DefaultChannelClassification,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.handleGameUpper(session, heartbeat); err != nil {
		t.Fatal(err)
	}
	ack, rest := splitGameServerUpperPacket(t, connection.write.Bytes())
	if ack.Header.Classification != dnfproto.DefaultChannelClassification ||
		ack.Header.MsgID != uint16(dnfenum.UpperMsgCheckUserConnection) ||
		!bytes.Equal(ack.Body, upperSuccessBody(buildCurrentCheckUserConnectionSuccessPayload())) ||
		len(rest) != 0 {
		t.Fatalf("op1276 lifecycle ACK=%+v body=%x rest=%x", ack.Header, ack.Body, rest)
	}

	service.options.gameUpperClientBodyCodec = gameUpperClientBodyCodecNative
	for index, bodyLen := range []int{
		currentChannelReconnectDisplayProbeSize,
		reference90CNChannelReconnectDisplayProbeBodySize,
	} {
		connection.write.Reset()
		endpointProbe, err := dnfproto.BuildChannelPacket(
			uint16(dnfenum.UpperMsgGameEndpoint),
			make([]byte, bodyLen),
			uint16(4+index),
			dnfproto.DefaultChannelClassification,
		)
		if err != nil {
			t.Fatal(err)
		}
		if err := service.handleGameUpper(session, endpointProbe); err != nil {
			t.Fatal(err)
		}
		if connection.write.Len() != 0 {
			t.Fatalf("post-reconnect op1/%d emitted bytes=%x", bodyLen, connection.write.Bytes())
		}
	}
}

func TestCurrentClientChannelSwitchStreamRoutesOp4AndIgnoresEndpointProbes(t *testing.T) {
	service, session, _ := newChannelReconnectTest(t)
	connection := session.conn.(*bufferConn)
	service.options.gameUpperClientBodyCodec = gameUpperClientBodyCodecPlain

	request := make([]byte, 2)
	binary.LittleEndian.PutUint16(request, 29)
	selectFrame, err := dnfproto.BuildChannelPacket(
		uint16(dnfenum.UpperMsgSelectCharacter),
		request,
		1,
		dnfproto.DefaultChannelClassification,
	)
	if err != nil {
		t.Fatal(err)
	}
	stream := append([]byte(nil), selectFrame...)
	for index, bodyLen := range []int{
		observed90CNShortEndpointBodySize,
		currentChannelReconnectDisplayProbeSize,
		currentChannelReconnectDisplayProbeSize + current90CNEndpointShortVariantSize,
		reference90CNChannelReconnectDisplayProbeBodySize,
	} {
		probe, err := dnfproto.BuildChannelPacket(
			uint16(dnfenum.UpperMsgGameEndpoint),
			current90CNEndpointWireTestBody(bodyLen),
			uint16(2+index),
			dnfproto.DefaultChannelClassification,
		)
		if err != nil {
			t.Fatal(err)
		}
		stream = append(stream, probe...)
	}
	packets, remaining, skipped, err := dnfproto.SplitLatestGameStream(stream, 4096)
	if err != nil {
		t.Fatal(err)
	}
	if skipped != 0 || len(remaining) != 0 || len(packets) != 5 {
		t.Fatalf("current channel switch split packets=%d skipped=%d remaining=%x", len(packets), skipped, remaining)
	}
	for index, packet := range packets {
		if packet.Kind != dnfproto.LatestGameStreamUpper {
			t.Fatalf("current channel switch packet[%d] kind=%d want upper", index, packet.Kind)
		}
	}

	if err := service.handleGameStreamPacket(session, packets[0]); err != nil {
		t.Fatal(err)
	}
	assertImmediateChannelReconnectOrder(
		t,
		splitAllCurrentGameServerUpperPackets(t, connection.write.Bytes()),
		byte(session.channel.ID),
	)
	if session.channelReconnect {
		t.Fatalf(
			"op4 reconnect state route=%t",
			session.channelReconnect,
		)
	}

	service.options.gameUpperClientBodyCodec = gameUpperClientBodyCodecNative
	for index := 1; index < len(packets); index++ {
		connection.write.Reset()
		if err := service.handleGameStreamPacket(session, packets[index]); err != nil {
			t.Fatal(err)
		}
		if connection.write.Len() != 0 {
			t.Fatalf("stale endpoint stream packet[%d] emitted bytes=%x", index, connection.write.Bytes())
		}
	}
}

func current90CNEndpointWireTestBody(bodyLen int) []byte {
	body := make([]byte, bodyLen)
	binary.LittleEndian.PutUint32(body[0:4], 1)
	body[4] = 0x30
	body[5] = 0x20
	copy(body[9:41], []byte("de509f65e9ccaae621cb7278fc2b8e6c"))
	binary.LittleEndian.PutUint32(body[42:46], 8)
	copy(body[54:62], []byte{0, 2, 0, 0, 1, 1, 0, 0x1c})
	return body
}

type currentReconnectLocation struct {
	townID    int64
	areaID    int64
	positionX int64
	positionY int64
	direction int64
	areaState int64
}

type countingChannelReconnectQuestRepository struct {
	dnfrepo.QuestRepository
	saveCount int
}

func (r *countingChannelReconnectQuestRepository) Save(ctx context.Context, record dnfrepo.QuestRecord) error {
	r.saveCount++
	return r.QuestRepository.Save(ctx, record)
}

func currentReconnectLocationSnapshot(character dnfrepo.CharacterRecord) currentReconnectLocation {
	return currentReconnectLocation{
		townID:    character.Stats["town_id"],
		areaID:    character.Stats["area_id"],
		positionX: character.Stats["pos_x"],
		positionY: character.Stats["pos_y"],
		direction: character.Stats["direction"],
		areaState: character.Stats["area_state"],
	}
}

func newChannelReconnectTest(t *testing.T) (*Service, *gameSession, dnfrepo.Group) {
	t.Helper()
	service, session, repositories := newTownMoveTest(t)
	if err := repositories.Account.Save(context.Background(), dnfrepo.AccountRecord{
		AccountID: defaultAccountPrefix + "1",
		Metadata:  make(map[string]string),
	}); err != nil {
		t.Fatal(err)
	}
	skillCatalog, err := buildSkillCatalogFromSource(context.Background(), initialEquipmentMemSource{
		"skill/skilllist.lst":    "0 `job0.lst`\n",
		"skill/job0.lst":         "46 `job0/initial.skl`\n",
		"skill/job0/initial.skl": "[skill type]\n`active`\n",
	})
	if err != nil {
		t.Fatal(err)
	}
	service.initialSkillsByJob = map[byte][]initialSkillEntry{0: {{SkillID: 46, Level: 1}}}
	service.initialSPTable = map[int]int{90: 3710}
	service.initialTPTable = map[int]int{90: 41}
	service.skillCatalog = skillCatalog
	service.options.accountPrefix = defaultAccountPrefix
	service.options.channelAdvertiseID = 0
	service.options.serverIP = "127.0.0.1"
	service.adventureGroupTable = loadAdventureGroupTestTables(t)
	service.questCatalog = buildQuestListTestCatalog(t)

	character, found, err := repositories.Character.Load(context.Background(), "29")
	if err != nil || !found {
		t.Fatalf("load reconnect character found=%t err=%v", found, err)
	}
	character.Name = "channel-reconnect"
	character.Job = "0"
	character.Level = 90
	character.Stats["town_id"] = 39
	character.Stats["area_id"] = 0
	character.Stats["pos_x"] = 777
	character.Stats["pos_y"] = 333
	character.Stats["direction"] = 2
	character.Stats["area_state"] = 7
	character.Stats[currentDungeonTutorialCompletedKey] = currentDungeonTutorialCompleteFlag
	if err := repositories.Character.Save(context.Background(), character); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Equipment.Save(context.Background(), dnfrepo.EquipmentRecord{
		CharacterID: "29",
		Entries: map[string]dnfrepo.EquipmentEntry{
			"11": {
				SlotIndex: 11,
				ItemID:    900001,
				RawEntry:  buildInitialEquipmentRawEntry(11, 900001, 27),
			},
		},
	}); err != nil {
		t.Fatal(err)
	}

	session.conn = &bufferConn{}
	session.channel = channelcatalog.Channel{
		ServerID:   1,
		ID:         17,
		Type:       1,
		Name:       "ch.17",
		NoticeName: "ch.17",
		Port:       10017,
	}
	session.residentChannel = session.channel
	session.connectionTownActorOwnerChannel = byte(session.channel.ID)
	session.townActorOwnerChannel = byte(session.channel.ID)
	session.currentChannelResidentNoticeSent = true
	session.gameEndpointSuccessSent = true
	session.selectedCharacterID = 0
	session.townSceneReadyCharacterID = 0
	session.townPositionSnapshot = currentTownPositionSnapshot{}
	session.initialTownRouteCharacterID = 0
	session.initialTownRouteStage = currentInitialTownRouteIdle
	session.rosterRequested = false
	session.channelReconnect = false
	session.sceneBootstrapTailDeferred = false
	session.sceneBootstrapTailSent = false
	return service, session, repositories
}

func splitAllCurrentGameServerUpperPackets(t *testing.T, stream []byte) []dnfproto.ChannelPacket {
	t.Helper()
	packets := make([]dnfproto.ChannelPacket, 0)
	for len(stream) > 0 {
		packet, rest := splitCurrentGameServerUpperPacketAuto(t, stream)
		packets = append(packets, packet)
		stream = rest
	}
	return packets
}

func assertNoChannelReconnectResidentNotice(t *testing.T, packets []dnfproto.ChannelPacket) {
	t.Helper()
	for index, packet := range packets {
		if packet.Header.MsgID == uint16(dnfenum.UpperMsgGameEndpoint) {
			t.Fatalf(
				"channel reconnect packet[%d] emitted forbidden endpoint op1 class=%d body=%x",
				index,
				packet.Header.Classification,
				packet.Body,
			)
		}
	}
}

func assertImmediateChannelReconnectOrder(t *testing.T, packets []dnfproto.ChannelPacket, target byte) {
	t.Helper()
	sceneOwner := target
	assertNoChannelReconnectResidentNotice(t, packets)
	indexes := map[string]int{
		"story":       -1,
		"mode0":       -1,
		"light_mode1": -1,
		"op800":       -1,
		"full_mode1":  -1,
		"first_op21":  -1,
		"first_op574": -1,
		"op124":       -1,
		"op9":         -1,
		"op120":       -1,
		"op22":        -1,
		"op23":        -1,
		"op24":        -1,
		"guild_noti":  -1,
		"last_op21":   -1,
		"last_op574":  -1,
	}
	op21Count := 0
	op574Count := 0
	mode1Count := 0
	op800Count := 0
	op124Count := 0
	op9Count := 0
	op120Count := 0
	for index, packet := range packets {
		switch packet.Header.MsgID {
		case currentStoryDigestLastLevelMsgID:
			indexes["story"] = index
		case uint16(dnfenum.CmdPacketSetUDPIPPort):
			if len(packet.Body) < 5 {
				continue
			}
			switch packet.Body[0] {
			case 0:
				if packet.Body[3] != 0 || packet.Body[4] != sceneOwner {
					t.Fatalf("channel reconnect mode0 owner=%x want 00/%02x", packet.Body[:5], sceneOwner)
				}
				if indexes["mode0"] < 0 {
					indexes["mode0"] = index
				}
			case 1:
				mode1Count++
				if packet.Body[3] != 0 || packet.Body[4] != sceneOwner {
					t.Fatalf("channel reconnect mode1 owner=%x want 00/%02x", packet.Body[:5], sceneOwner)
				}
				switch mode1Count {
				case 1:
					if len(packet.Body) != currentMode1BaseWireSize {
						t.Fatalf("channel reconnect light mode1 len=%d want=%d body=%x", len(packet.Body), currentMode1BaseWireSize, packet.Body)
					}
					indexes["light_mode1"] = index
				case 2:
					if len(packet.Body) <= currentMode1CreateRowsOffset+5 ||
						packet.Body[currentMode1CreateCountOffset] != 1 ||
						binary.LittleEndian.Uint32(packet.Body[currentMode1CreateRowsOffset+1:currentMode1CreateRowsOffset+5]) != 900001 {
						t.Fatalf("channel reconnect full mode1 missing equipment create row body=%x", packet.Body)
					}
					indexes["full_mode1"] = index
				}
			}
		case currentTownActorSceneSnapshotMsgID:
			op800Count++
			if indexes["op800"] < 0 {
				indexes["op800"] = index
			}
		case currentAcceptableQuestListMsgID:
			op21Count++
			if indexes["first_op21"] < 0 {
				indexes["first_op21"] = index
			}
			indexes["last_op21"] = index
		case currentActiveQuestSnapshotMsgID:
			op574Count++
			if indexes["first_op574"] < 0 {
				indexes["first_op574"] = index
			}
			indexes["last_op574"] = index
		case uint16(dnfenum.CmdPacketReportClientSpec):
			op124Count++
			if len(packet.Body) != 0 {
				t.Fatalf("channel reconnect op124 body=%x", packet.Body)
			}
			indexes["op124"] = index
		case uint16(dnfenum.CmdPacketRecoverStamina):
			op9Count++
			assertCurrentPartylessTownOp9Body(t, packet.Body, sceneOwner)
			indexes["op9"] = index
		case uint16(dnfenum.CmdPacketRequestBlacklist):
			op120Count++
			indexes["op120"] = index
		case currentTownUserPositionNotificationMsgID:
			indexes["op22"] = index
		case currentTownUserAreaNotificationMsgID:
			indexes["op23"] = index
		case currentSceneTransitionMsgID:
			if packetLooksLikeInitialTownTransition(packet) {
				indexes["op24"] = index
			}
		case currentGuildMembershipNotificationMsgID:
			if packet.Header.Classification != 0 ||
				!bytes.Equal(packet.Body, buildCurrentGuildMembershipNotificationBody(nil)) {
				t.Fatalf("channel reconnect guild notification header=%+v body=%x", packet.Header, packet.Body)
			}
			indexes["guild_noti"] = index
		}
	}
	if mode1Count != 2 {
		t.Fatalf("channel reconnect mode1 count=%d want light binding plus full equipment state", mode1Count)
	}
	if op800Count != 1 || op124Count != 1 || op9Count != 1 || op120Count != 1 {
		t.Fatalf(
			"channel reconnect display packet counts op800=%d op124=%d op9=%d op120=%d want=1/1/1/1",
			op800Count,
			op124Count,
			op9Count,
			op120Count,
		)
	}
	if op21Count != 2 || op574Count != 2 {
		t.Fatalf("channel reconnect quest snapshot counts op21=%d op574=%d want=2/2 indexes=%+v", op21Count, op574Count, indexes)
	}
	wantOrder := []string{
		"story",
		"mode0",
		"light_mode1",
		"op800",
		"op22",
		"op23",
		"full_mode1",
		"first_op21",
		"first_op574",
		"op124",
		"op9",
		"op120",
		"op24",
		"guild_noti",
		"last_op21",
		"last_op574",
	}
	for index, name := range wantOrder {
		if indexes[name] < 0 {
			t.Fatalf("channel reconnect packet %s missing: %+v", name, indexes)
		}
		if index > 0 && indexes[wantOrder[index-1]] >= indexes[name] {
			t.Fatalf("channel reconnect order invalid: %+v", indexes)
		}
	}
}

// assertCurrentChannelNoticePacket remains shared by the cold-login ordering
// tests. Reconnect tests must instead use assertNoChannelReconnectResidentNotice.
func assertCurrentChannelNoticePacket(
	t *testing.T,
	packet dnfproto.ChannelPacket,
	wantServerID uint64,
	wantChannelID uint64,
) {
	t.Helper()
	if packet.Header.Classification != 0 ||
		packet.Header.MsgID != uint16(dnfenum.UpperMsgGameEndpoint) {
		t.Fatalf("current channel notice header=%+v", packet.Header)
	}
	plain := make([]byte, len(packet.Body))
	for index, value := range packet.Body {
		plain[index] = bits.RotateLeft8(value, 6) ^ 0xb5
	}
	if len(plain) < 4 || int(binary.LittleEndian.Uint32(plain[:4])) != len(plain)-4 {
		t.Fatalf("current channel notice protobuf length body=%x plain=%x", packet.Body, plain)
	}
	fields, _ := consumeChannelInfoProto(t, plain[4:])
	if values := fields[7]; len(values) != 1 || values[0] != wantServerID {
		t.Fatalf("current channel notice field7=%v want=[%d] plain=%x", values, wantServerID, plain)
	}
	if values := fields[8]; len(values) != 1 || values[0] != wantChannelID {
		t.Fatalf("current channel notice field8=%v want=[%d] plain=%x", values, wantChannelID, plain)
	}
}
