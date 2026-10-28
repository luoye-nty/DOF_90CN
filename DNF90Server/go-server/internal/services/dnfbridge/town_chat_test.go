package dnfbridge

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"testing"

	"longheng.io/server/internal/modules/dnf/channelcatalog"
	"longheng.io/server/internal/modules/dnf/dnfenum"
)

func TestCurrentTownAreaChatBroadcastsOnlyToMatchingScene(t *testing.T) {
	channel := channelcatalog.Channel{ID: 16}
	newSession := func(character uint16, town, area byte) (*gameSession, *bufferConn) {
		conn := &bufferConn{}
		return &gameSession{
			conn:                      conn,
			channel:                   channel,
			selectedCharacterID:       character,
			townSceneReadyCharacterID: character,
			townPositionSnapshot: currentTownPositionSnapshot{
				CharacterID:   character,
				TownID:        town,
				AreaID:        area,
				PositionValid: true,
			},
		}, conn
	}
	sender, senderConn := newSession(19, 38, 1)
	recipient, recipientConn := newSession(20, 38, 1)
	otherArea, otherAreaConn := newSession(21, 38, 2)
	otherChannel, otherChannelConn := newSession(22, 38, 1)
	otherChannel.channel.ID = 17
	otherInstance, otherInstanceConn := newSession(23, 38, 1)
	service := &Service{gameSessions: map[uint16]*gameSession{
		19: sender, 20: recipient, 21: otherArea, 22: otherChannel, 23: otherInstance,
	}, onlinePlayers: newOnlinePlayerManager()}
	for _, presence := range []onlinePlayerInfo{
		{CharacterID: 19, TownID: 38, AreaID: 1, Session: sender},
		{CharacterID: 20, TownID: 38, AreaID: 1, Session: recipient},
		{CharacterID: 21, TownID: 38, AreaID: 2, Session: otherArea},
		// Channel 17 still shares the same visible public-town scene.
		{CharacterID: 22, TownID: 38, AreaID: 1, Session: otherChannel},
		// A private scene with the same PVF town/area must remain isolated.
		{CharacterID: 23, TownID: 38, AreaID: 1, InstanceKey: 99, Session: otherInstance},
	} {
		copy := presence
		service.onlinePlayers.EnterArea(&copy)
	}

	request := buildCurrentTownChatRequestForTest(currentTownChatAreaMode, []byte("hello-90cn"))
	if err := service.handleGameCommand(sender, byte(dnfenum.GameCmdCommand), uint16(dnfenum.CmdPacketSendMessage), request); err != nil {
		t.Fatalf("chat: %v", err)
	}
	want := buildCurrentTownChatNotification(currentTownChatAreaMode, sender.selectedCharacterID, []byte("hello-90cn"))
	for name, conn := range map[string]*bufferConn{"sender": senderConn, "recipient": recipientConn, "other_channel": otherChannelConn} {
		packet, rest := splitGameServerUpperPacket(t, conn.write.Bytes())
		if packet.Header.MsgID != currentTownChatNotificationMsgID || packet.Header.Classification != 0 || !bytes.Equal(packet.Body, want) || len(rest) != 0 {
			t.Fatalf("%s packet header=%+v body=%x rest=%x", name, packet.Header, packet.Body, rest)
		}
	}
	if otherAreaConn.write.Len() != 0 || otherInstanceConn.write.Len() != 0 {
		t.Fatalf("chat crossed scene: area=%x instance=%x", otherAreaConn.write.Bytes(), otherInstanceConn.write.Bytes())
	}
}

// A character in its private Seria room is absent from the public-town
// registry by design, which is where a freshly logged-in player stands. Area
// chat there used to answer the failure code and print nothing; the speaker is
// the whole audience of a private scene, so the line echoes back.
func TestCurrentAreaChatInPrivateSceneEchoesToTheSpeaker(t *testing.T) {
	conn := &bufferConn{}
	sender := &gameSession{
		conn:                conn,
		channel:             channelcatalog.Channel{ID: 16},
		selectedCharacterID: 19,
	}
	service := &Service{
		gameSessions:  map[uint16]*gameSession{19: sender},
		onlinePlayers: newOnlinePlayerManager(),
	}
	recipients, scope, ok := service.currentScopedChatRecipients(sender, currentTownChatAreaMode)
	if !ok || scope != "private_scene" || len(recipients) != 1 || recipients[0] != sender {
		t.Fatalf("private-scene area scope ok=%t scope=%q recipients=%d", ok, scope, len(recipients))
	}

	request := buildCurrentTownChatRequestForTest(currentTownChatAreaMode, []byte("alone"))
	parsed, parsedOK := parseCurrentTownChatRequest(request)
	if !parsedOK {
		t.Fatalf("area request rejected: %x", request)
	}
	if err := service.handleCurrentScopedTownChat(sender, parsed); err != nil {
		t.Fatal(err)
	}
	packet, rest := splitGameServerUpperPacket(t, conn.write.Bytes())
	want := buildCurrentTownChatNotification(currentTownChatAreaMode, 19, []byte("alone"))
	if packet.Header.MsgID != currentTownChatNotificationMsgID || packet.Header.Classification != 0 ||
		!bytes.Equal(packet.Body, want) || len(rest) != 0 {
		t.Fatalf("private-scene area line header=%+v body=%x want=%x rest=%x",
			packet.Header, packet.Body, want, rest)
	}
}

func TestCurrentTownChatRejectsMalformedAndUnsupportedModes(t *testing.T) {
	conn := &bufferConn{}
	session := &gameSession{
		conn:                      conn,
		channel:                   channelcatalog.Channel{ID: 16},
		selectedCharacterID:       19,
		townSceneReadyCharacterID: 19,
		townPositionSnapshot: currentTownPositionSnapshot{
			CharacterID: 19, TownID: 38, AreaID: 1, PositionValid: true,
		},
	}
	service := &Service{gameSessions: map[uint16]*gameSession{19: session}}
	malformed := buildCurrentTownChatRequestForTest(currentTownChatAreaMode, []byte("x"))
	binary.LittleEndian.PutUint32(malformed[7:11], 2)
	for name, body := range map[string][]byte{
		"bad_message_length":       malformed,
		"private_without_identity": buildCurrentTownChatRequestForTest(currentTownChatWhisperMode, []byte("private")),
		"short_body":               make([]byte, 10),
		"unsupported_mode":         buildCurrentTownChatRequestForTest(0x7f, []byte("unsupported")),
	} {
		t.Run(name, func(t *testing.T) {
			conn.write.Reset()
			if err := service.handleCurrentTownChat(session, body); err != nil {
				t.Fatalf("reject chat: %v", err)
			}
			assertCurrentTownChatFailurePacket(t, conn.write.Bytes())
		})
	}
}

func TestCurrentRobotMegaphoneUsesNonInteractiveIdentityForOtherClients(t *testing.T) {
	robotConn := &bufferConn{}
	playerConn := &bufferConn{}
	robot := &gameSession{
		conn:                robotConn,
		channel:             channelcatalog.Channel{ID: 19},
		selectedCharacterID: 348,
		robotSessionBound:   true,
	}
	player := &gameSession{
		conn:                playerConn,
		channel:             channelcatalog.Channel{ID: 19},
		selectedCharacterID: 1,
	}
	service := &Service{}
	name := []byte{0xbb, 0xb6, 0xcf, 0xeb, 0xd0, 0xc7, 0xc7, 0xf2, '3', '3', '0'}
	message := []byte{0xd0, 0xdd, 0xcf, 0xa2, 0xd2, 0xbb, 0xbb, 0xe1, 0xd4, 0xd9, 0xb3, 0xf6, 0xb7, 0xa2}
	native := buildCurrentPeerChatNoticeBody(currentTownChatMegaphoneMode, 19, robot.selectedCharacterID, name, message)
	if got := service.sendCurrentMegaphoneToRecipients(robot, []*gameSession{robot, player}, currentTownChatMegaphoneMode, native, name, message); got != 2 {
		t.Fatalf("delivered=%d want 2", got)
	}

	robotPacket, robotRest := splitGameServerUpperPacket(t, robotConn.write.Bytes())
	if robotPacket.Header.MsgID != currentPeerChatNotifyOpcode || robotPacket.Header.Classification != 0 ||
		!bytes.Equal(robotPacket.Body, native) || len(robotRest) != 0 {
		t.Fatalf("robot packet header=%+v body=%x rest=%x", robotPacket.Header, robotPacket.Body, robotRest)
	}

	playerPacket, playerRest := splitGameServerUpperPacket(t, playerConn.write.Bytes())
	wantPlayer := buildCurrentPassiveRobotMegaphoneNoticeBody(currentTownChatMegaphoneMode, 19, name, message)
	if playerPacket.Header.MsgID != currentPeerChatNotifyOpcode || playerPacket.Header.Classification != 0 ||
		!bytes.Equal(playerPacket.Body, wantPlayer) || len(playerRest) != 0 {
		t.Fatalf("player packet header=%+v body=%x want=%x rest=%x", playerPacket.Header, playerPacket.Body, wantPlayer, playerRest)
	}
	if len(playerPacket.Body) < 14 || playerPacket.Body[0] != currentTownChatMegaphoneMode ||
		binary.LittleEndian.Uint16(playerPacket.Body[4:6]) != 0 || binary.LittleEndian.Uint32(playerPacket.Body[6:10]) != 0 {
		t.Fatalf("passive identity was not cleared: %x", playerPacket.Body)
	}
	wantVisible := append(append(append([]byte(nil), name...), []byte(": ")...), message...)
	messageLength := int(binary.LittleEndian.Uint32(playerPacket.Body[10:14]))
	if 14+messageLength != len(playerPacket.Body) || !bytes.Equal(playerPacket.Body[14:], wantVisible) {
		t.Fatalf("passive visible message=%x want=%x body=%x", playerPacket.Body[14:], wantVisible, playerPacket.Body)
	}
}

func TestCurrentHumanMegaphoneKeepsNativeInteractiveIdentity(t *testing.T) {
	senderConn := &bufferConn{}
	recipientConn := &bufferConn{}
	sender := &gameSession{conn: senderConn, channel: channelcatalog.Channel{ID: 253}, selectedCharacterID: 1}
	recipient := &gameSession{conn: recipientConn, channel: channelcatalog.Channel{ID: 19}, selectedCharacterID: 2}
	service := &Service{}
	name := []byte("human")
	message := []byte("hello")
	native := buildCurrentPeerChatNoticeBody(currentTownChatMegaphoneMode, 253, sender.selectedCharacterID, name, message)
	if got := service.sendCurrentMegaphoneToRecipients(sender, []*gameSession{sender, recipient}, currentTownChatMegaphoneMode, native, name, message); got != 2 {
		t.Fatalf("delivered=%d want 2", got)
	}
	for label, connection := range map[string]*bufferConn{"sender": senderConn, "recipient": recipientConn} {
		packet, rest := splitGameServerUpperPacket(t, connection.write.Bytes())
		if packet.Header.MsgID != currentPeerChatNotifyOpcode || !bytes.Equal(packet.Body, native) || len(rest) != 0 {
			t.Fatalf("%s body=%x want=%x rest=%x", label, packet.Body, native, rest)
		}
	}
}

func TestCurrentPassiveRobotMegaphoneKeepsNativeMessageBoundary(t *testing.T) {
	message := bytes.Repeat([]byte{'x'}, currentTownChatMaxMessageBytes)
	body := buildCurrentPassiveRobotMegaphoneNoticeBody(currentTownChatMegaphoneMode, 19, []byte("robot-name"), message)
	if len(body) != 14+currentTownChatMaxMessageBytes {
		t.Fatalf("body len=%d want %d", len(body), 14+currentTownChatMaxMessageBytes)
	}
	if binary.LittleEndian.Uint16(body[4:6]) != 0 || binary.LittleEndian.Uint32(body[6:10]) != 0 {
		t.Fatalf("passive identity was not cleared: %x", body[:10])
	}
	if binary.LittleEndian.Uint32(body[10:14]) != currentTownChatMaxMessageBytes || !bytes.Equal(body[14:], message) {
		t.Fatal("maximum-length message was altered")
	}
}

func TestParseCurrentTownChatAcceptsCapturedModeShapes(t *testing.T) {
	decode := func(value string) []byte {
		t.Helper()
		body, err := hex.DecodeString(value)
		if err != nil {
			t.Fatal(err)
		}
		return body
	}
	whisper := decode("01020000000000010000003f04000000cab7caab00")
	whisperTrailingOne := decode("01020000000000010000003f04000000cab7caab01")
	peerMenu := decode("24050000000000060000006e6968616f2006000000c0b4c0b4c0b400")
	oneToOne := buildCurrentPrivateTownChatRequestForTest(currentTownChatOneToOneMode, 2, []byte("compat"), []byte("target90"))

	megaphone := buildCurrentTownChatRequestForTest(currentTownChatMegaphoneMode, []byte("123"))
	binary.LittleEndian.PutUint32(megaphone[3:7], 7)

	tests := []struct {
		name       string
		body       []byte
		mode       byte
		target     uint16
		argument   uint32
		message    []byte
		targetName []byte
	}{
		{name: "area", body: buildCurrentTownChatRequestForTest(currentTownChatAreaMode, []byte("...")), mode: currentTownChatAreaMode},
		{name: "party", body: buildCurrentTownChatRequestForTest(currentTownChatPartyMode, []byte("...")), mode: currentTownChatPartyMode},
		{name: "guild", body: buildCurrentTownChatRequestForTest(currentTownChatGuildMode, []byte("...")), mode: currentTownChatGuildMode},
		{name: "raid", body: buildCurrentTownChatRequestForTest(currentTownChatRaidMode, []byte("....")), mode: currentTownChatRaidMode},
		{name: "megaphone", body: megaphone, mode: currentTownChatMegaphoneMode, argument: 7},
		{name: "whisper-captured", body: whisper, mode: currentTownChatWhisperMode, target: 2, message: []byte("?"), targetName: []byte{0xca, 0xb7, 0xca, 0xab}},
		{name: "whisper-captured-trailing-one", body: whisperTrailingOne, mode: currentTownChatWhisperMode, target: 2, message: []byte("?"), targetName: []byte{0xca, 0xb7, 0xca, 0xab}},
		{name: "one-to-one-compat", body: oneToOne, mode: currentTownChatOneToOneMode, target: 2, message: []byte("compat"), targetName: []byte("target90")},
		{name: "peer-menu-captured", body: peerMenu, mode: currentTownChatPeerMenuMode, target: 5, message: []byte("nihao "), targetName: []byte{0xc0, 0xb4, 0xc0, 0xb4, 0xc0, 0xb4}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request, ok := parseCurrentTownChatRequest(test.body)
			if !ok {
				t.Fatalf("request rejected: %x", test.body)
			}
			if request.Mode != test.mode || request.TargetUniqueID != test.target ||
				request.Argument != test.argument || (test.message != nil && !bytes.Equal(request.Message, test.message)) ||
				!bytes.Equal(request.TargetName, test.targetName) {
				t.Fatalf("request=%+v", request)
			}
		})
	}
}

func TestCurrentPrivateTownChatModesRouteAcrossChannelsAligned(t *testing.T) {
	for _, mode := range []byte{currentTownChatWhisperMode, currentTownChatOneToOneMode, currentTownChatPeerMenuMode} {
		t.Run(hex.EncodeToString([]byte{mode}), func(t *testing.T) {
			service, source, target, sourceConn, targetConn := newCurrentSocialPeerTest(t)
			target.channel.ID = 77
			service.onlinePlayers.LeaveAreaSession(target.selectedCharacterID, target)
			request := buildCurrentPrivateTownChatRequestForTest(mode, target.selectedCharacterID, []byte("hello"), rosterNameBytes(currentSocialTestFriendName))
			if mode == currentTownChatWhisperMode {
				request[len(request)-1] = 1 // Captured whisper variant.
			}

			handled, err := service.handleAlignedGameCommand(source, byte(dnfenum.GameCmdCommand), currentPeerChatStateOpcode, request)
			if err != nil {
				t.Fatal(err)
			}
			if !handled {
				t.Fatal("private chat was not handled")
			}
			assertCurrentPrivateTownChatSuccess(t, sourceConn.write.Bytes(), targetConn.write.Bytes(), mode, []byte("hello"), source, target)
		})
	}
}

func TestCurrentPrivateTownChatPeerMenuRoutesThroughLegacyDispatcher(t *testing.T) {
	service, source, target, sourceConn, targetConn := newCurrentSocialPeerTest(t)
	request := buildCurrentPrivateTownChatRequestForTest(currentTownChatPeerMenuMode, target.selectedCharacterID, []byte("legacy"), rosterNameBytes(currentSocialTestFriendName))
	frame := buildLegacyGamePacketForBridgeTest(byte(dnfenum.GameCmdCommand), currentPeerChatStateOpcode, 1, request)
	if err := service.handleLegacyGamePacket(source, frame); err != nil {
		t.Fatal(err)
	}
	assertCurrentPrivateTownChatSuccess(t, sourceConn.write.Bytes(), targetConn.write.Bytes(), currentTownChatPeerMenuMode, []byte("legacy"), source, target)
}

func TestCurrentPrivateTownChatFailuresReleasePendingGate(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*Service, *gameSession)
		body  func(*gameSession) []byte
	}{
		{
			name: "malformed_missing_trailing_zero",
			body: func(target *gameSession) []byte {
				body := buildCurrentPrivateTownChatRequestForTest(currentTownChatWhisperMode, target.selectedCharacterID, []byte("hello"), rosterNameBytes(currentSocialTestFriendName))
				return body[:len(body)-1]
			},
		},
		{
			name: "malformed_unknown_trailing_flag",
			body: func(target *gameSession) []byte {
				body := buildCurrentPrivateTownChatRequestForTest(currentTownChatPeerMenuMode, target.selectedCharacterID, []byte("hello"), rosterNameBytes(currentSocialTestFriendName))
				body[len(body)-1] = 2
				return body
			},
		},
		{
			name: "offline",
			setup: func(service *Service, target *gameSession) {
				service.mu.Lock()
				delete(service.gameSessions, target.selectedCharacterID)
				service.mu.Unlock()
			},
			body: func(target *gameSession) []byte {
				return buildCurrentPrivateTownChatRequestForTest(currentTownChatOneToOneMode, target.selectedCharacterID, []byte("hello"), rosterNameBytes(currentSocialTestFriendName))
			},
		},
		{
			name: "target_name_mismatch",
			body: func(target *gameSession) []byte {
				return buildCurrentPrivateTownChatRequestForTest(currentTownChatPeerMenuMode, target.selectedCharacterID, []byte("hello"), []byte("wrong"))
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			service, source, target, sourceConn, targetConn := newCurrentSocialPeerTest(t)
			if test.setup != nil {
				test.setup(service, target)
			}
			handled, err := service.handleAlignedGameCommand(source, byte(dnfenum.GameCmdCommand), currentPeerChatStateOpcode, test.body(target))
			if err != nil {
				t.Fatal(err)
			}
			if !handled {
				t.Fatal("private chat failure was not handled")
			}
			assertCurrentTownChatFailurePacket(t, sourceConn.write.Bytes())
			if targetConn.write.Len() != 0 {
				t.Fatalf("failed private chat reached target: %x", targetConn.write.Bytes())
			}
		})
	}
}

// wantCurrentPrivateChatStatePacket is the state packet a private line's reader
// must receive. A typed whisper keeps the class-0 op17 peer-id state; the 1:1 and
// peer-menu windows are fed by op65 instead, which carries the line and the name of
// the slot it belongs to: flag 0 is the other party (an incoming line, named after
// the speaker) and flag 1 is the player (the sender's own copy, named after itself).
func wantCurrentPrivateChatStatePacket(mode byte, peerID uint16, message []byte, speaker, peer string, flag byte) (uint16, []byte) {
	if isCurrentOneToOneTownChatMode(mode) {
		return currentTownChatOneToOneStateMsgID,
			buildCurrentOneToOneChatStateBody(mode, flag, rosterNameBytes(speaker), rosterNameBytes(peer), message)
	}
	return currentPeerChatStateOpcode, buildCurrentPeerChatStateBody(peerID, mode)
}

func assertCurrentPrivateTownChatSuccess(t *testing.T, sourceData, targetData []byte, mode byte, message []byte, source, target *gameSession) {
	t.Helper()
	ack, sourceRest := splitGameServerUpperPacket(t, sourceData)
	if ack.Header.Classification != 1 || ack.Header.MsgID != currentPeerChatStateOpcode || !bytes.Equal(ack.Body, []byte{1}) {
		t.Fatalf("source ack header=%+v body=%x", ack.Header, ack.Body)
	}
	if isCurrentOneToOneTownChatMode(mode) {
		// One line per reader, both naming the speaker: the sender's copy comes back
		// because this client does not draw what the player typed, and the peer gets
		// the same line as an incoming one.
		wantSourceMsgID, wantSourceBody := wantCurrentPrivateChatStatePacket(
			mode, target.selectedCharacterID, message, "source1", currentSocialTestFriendName, currentChatIncomingLineFlag)
		sourceState, sourceRest := splitGameServerUpperPacket(t, sourceRest)
		if sourceState.Header.Classification != 0 || sourceState.Header.MsgID != wantSourceMsgID ||
			!bytes.Equal(sourceState.Body, wantSourceBody) || len(sourceRest) != 0 {
			t.Fatalf("1:1 sender copy header=%+v body=%x want=%x rest=%x",
				sourceState.Header, sourceState.Body, wantSourceBody, sourceRest)
		}
		wantTargetMsgID, wantTargetBody := wantCurrentPrivateChatStatePacket(
			mode, source.selectedCharacterID, message, "source1", "source1", currentChatIncomingLineFlag)
		targetState, targetRest := splitGameServerUpperPacket(t, targetData)
		if targetState.Header.Classification != 0 || targetState.Header.MsgID != wantTargetMsgID ||
			!bytes.Equal(targetState.Body, wantTargetBody) || len(targetRest) != 0 {
			t.Fatalf("1:1 peer copy header=%+v body=%x want=%x rest=%x",
				targetState.Header, targetState.Body, wantTargetBody, targetRest)
		}
		return
	}
	wantSourceStateMsgID, wantSourceStateBody := wantCurrentPrivateChatStatePacket(
		mode, target.selectedCharacterID, message, currentSocialTestFriendName, currentSocialTestFriendName, 0)
	sourceState, sourceRest := splitGameServerUpperPacket(t, sourceRest)
	if sourceState.Header.Classification != 0 || sourceState.Header.MsgID != wantSourceStateMsgID ||
		!bytes.Equal(sourceState.Body, wantSourceStateBody) {
		t.Fatalf("source state header=%+v body=%x want=%x rest=%x",
			sourceState.Header, sourceState.Body, wantSourceStateBody, sourceRest)
	}
	sourceNotice, sourceRest := splitGameServerUpperPacket(t, sourceRest)
	// Each side is named its peer, matching the state packet above: the sender's
	// echo describes the target, not itself. An echo that named the sender
	// described a conversation with itself and the client dropped it.
	// The echo also carries the outgoing twin of the mode so it reads as a line
	// this character sent rather than one it received.
	wantSourceNotice := buildCurrentGuildChatNoticeBody(currentOutgoingWhisperMode(mode), currentChatServerGroup, byte(source.channel.ID), 0, rosterNameBytes(currentSocialTestFriendName), message)
	if sourceNotice.Header.Classification != 0 || sourceNotice.Header.MsgID != currentPeerChatNotifyOpcode ||
		!bytes.Equal(sourceNotice.Body, wantSourceNotice) || len(sourceRest) != 0 {
		t.Fatalf("source notice header=%+v body=%x want=%x rest=%x", sourceNotice.Header, sourceNotice.Body, wantSourceNotice, sourceRest)
	}
	wantTargetStateMsgID, wantTargetStateBody := wantCurrentPrivateChatStatePacket(
		mode, source.selectedCharacterID, message, "source1", "source1", 0)
	targetState, targetRest := splitGameServerUpperPacket(t, targetData)
	if targetState.Header.Classification != 0 || targetState.Header.MsgID != wantTargetStateMsgID ||
		!bytes.Equal(targetState.Body, wantTargetStateBody) {
		t.Fatalf("target state header=%+v body=%x want=%x", targetState.Header, targetState.Body, wantTargetStateBody)
	}
	targetNotice, targetRest := splitGameServerUpperPacket(t, targetRest)
	wantNotice := buildCurrentGuildChatNoticeBody(mode, currentChatServerGroup, byte(target.channel.ID), 0, rosterNameBytes("source1"), message)
	if targetNotice.Header.Classification != 0 || targetNotice.Header.MsgID != currentPeerChatNotifyOpcode ||
		!bytes.Equal(targetNotice.Body, wantNotice) || len(targetRest) != 0 {
		t.Fatalf("target notice header=%+v body=%x want=%x rest=%x", targetNotice.Header, targetNotice.Body, wantNotice, targetRest)
	}
}

// TestCurrentOneToOneChatStateBodyMatchesClientReader pins op65 byte for byte.
//
// The client's reader (sub_1D691F0) is
//
//	u8 mode | u8 status | dstr name(31) | u8 flag | dstr text(256)
//	                | dstr extra(31)  -- consumed only when mode == 0x24
//
// and status must be the accept value: every other value is a rejection reason
// the client renders *instead of* the line (0x36 / 0x4D / 0x5A / default).
func TestCurrentOneToOneChatStateBodyMatchesClientReader(t *testing.T) {
	speaker := []byte("target1")
	message := []byte("hello")

	peerMenu := buildCurrentOneToOneChatStateBody(currentTownChatPeerMenuMode, currentChatIncomingLineFlag, speaker, speaker, message)
	wantPeerMenu := append([]byte{currentTownChatPeerMenuMode, currentTownChatOneToOneAcceptStatus, 7, 0, 0, 0}, speaker...)
	wantPeerMenu = append(wantPeerMenu, currentChatIncomingLineFlag, 5, 0, 0, 0)
	wantPeerMenu = append(wantPeerMenu, message...)
	wantPeerMenu = append(wantPeerMenu, 7, 0, 0, 0)
	wantPeerMenu = append(wantPeerMenu, speaker...)
	if !bytes.Equal(peerMenu, wantPeerMenu) {
		t.Fatalf("peer-menu state body=%x want=%x", peerMenu, wantPeerMenu)
	}

	// The one-to-one mode reaches the same handler but the handler only consumes
	// the trailing dstr for 0x24, so that mode must not carry it.
	oneToOne := buildCurrentOneToOneChatStateBody(currentTownChatOneToOneMode, 0, speaker, speaker, message)
	wantOneToOne := append([]byte{currentTownChatOneToOneMode, currentTownChatOneToOneAcceptStatus, 7, 0, 0, 0}, speaker...)
	wantOneToOne = append(wantOneToOne, 0, 5, 0, 0, 0)
	wantOneToOne = append(wantOneToOne, message...)
	if !bytes.Equal(oneToOne, wantOneToOne) {
		t.Fatalf("one-to-one state body=%x want=%x", oneToOne, wantOneToOne)
	}
	if len(peerMenu) != len(oneToOne)+4+len(speaker) {
		t.Fatalf("0x24 body must carry the trailing peer dstr: %d vs %d", len(peerMenu), len(oneToOne))
	}
}

func assertCurrentTownChatFailurePacket(t *testing.T, data []byte) {
	t.Helper()
	packet, rest := splitGameServerUpperPacket(t, data)
	if packet.Header.Classification != 1 || packet.Header.MsgID != currentPeerChatStateOpcode ||
		!bytes.Equal(packet.Body, []byte{0, currentTownChatGenericError}) || len(rest) != 0 {
		t.Fatalf("failure header=%+v body=%x rest=%x", packet.Header, packet.Body, rest)
	}
}

func TestCurrentTownGuildChatWithoutMembershipReturnsNativeFailure(t *testing.T) {
	conn := &bufferConn{}
	session := &gameSession{conn: conn, selectedCharacterID: 19}
	service := &Service{gameSessions: map[uint16]*gameSession{19: session}}
	if err := service.handleCurrentTownChat(session, buildCurrentTownChatRequestForTest(currentTownChatGuildMode, []byte("guild"))); err != nil {
		t.Fatal(err)
	}
	packet, rest := splitGameServerUpperPacket(t, conn.write.Bytes())
	if packet.Header.Classification != 1 || packet.Header.MsgID != currentPeerChatStateOpcode ||
		!bytes.Equal(packet.Body, []byte{0, currentTownChatGenericError}) || len(rest) != 0 {
		t.Fatalf("failure header=%+v body=%x rest=%x", packet.Header, packet.Body, rest)
	}
}

// The captured 鍏細 request the live EXE sends. It is pinned as raw bytes
// because the release snapshot assumed mode 4 and answered mode 6 from the
// unsupported-mode branch, which the chat window renders as silence.
func TestCurrentTownGuildChatCapturedRequestUsesGuildScope(t *testing.T) {
	body, err := hex.DecodeString("0601000000000008000000b9a4bbe1cffbcfa2")
	if err != nil {
		t.Fatal(err)
	}
	request, ok := parseCurrentTownChatRequest(body)
	if !ok {
		t.Fatalf("captured guild request rejected: %x", body)
	}
	if request.Mode != 6 {
		t.Fatalf("captured guild mode=%d want 6", request.Mode)
	}
	if currentTownChatGuildMode != 6 {
		t.Fatalf("currentTownChatGuildMode=%d want the captured 6", currentTownChatGuildMode)
	}
	if !bytes.Equal(request.Message, []byte{0xb9, 0xa4, 0xbb, 0xe1, 0xcf, 0xfb, 0xcf, 0xa2}) {
		t.Fatalf("captured guild message=%x", request.Message)
	}

	service := &Service{}
	session := &gameSession{selectedCharacterID: 1}
	for _, mode := range []byte{currentTownChatGuildMode, currentTownChatGuildLegacyMode} {
		if _, scope, _ := service.currentScopedChatRecipients(session, mode); scope != "guild" {
			t.Fatalf("mode %d scope=%q want guild", mode, scope)
		}
		if !isCurrentGuildTownChatMode(mode) {
			t.Fatalf("mode %d is not routed to the named-speaker notification", mode)
		}
	}
}

// Guild delivers on op12, the same carrier as area/party/raid. That carrier
// cannot name its speaker, so the line arrives unattributed 鈥?but op65 reached
// the speaker alone and op118 reached nobody, both live, so op12 is the only
// carrier observed to put a mode-6 line on screen. Pinning it here keeps the
// next attempt from silently swapping in another guessed opcode: replacing it
// needs a handler identified in the client's class-0 registry first.
// The op118 body that the client's own display gates accept, pinned field by
// field. Byte 2 must be the server group and byte 4 the reader's own channel:
// the client-side probes at 0x1D5B4E3 and 0x1D5B5B7 rejected byte(channel.ID) in
// slot 2 from readers on channels 508 and 121 (both answered 1), then accepted
// server group 1 with each reader's own channel in slot 4 (121 against 121, 251
// against 251). Putting the channel in slot 2 is what silently dropped every
// guild line.
func TestCurrentGuildChatNoticeCarriesServerGroupThenReaderChannel(t *testing.T) {
	if currentChatServerGroup != 1 {
		t.Fatalf("server group=%d want 1", currentChatServerGroup)
	}
	name := []byte{0xb9, 0xed, 0xbd, 0xa3, 0xca, 0xbf}                // 鬼剑士
	message := []byte{0xb9, 0xa4, 0xbb, 0xe1, 0xcf, 0xfb, 0xcf, 0xa2} // 工会消息
	for _, readerChannel := range []byte{121, 251} {
		body := buildCurrentGuildChatNoticeBody(currentTownChatGuildMode,
			currentChatServerGroup, readerChannel, 1, name, message)
		if body[0] != currentTownChatGuildMode {
			t.Fatalf("mode=%d", body[0])
		}
		if body[1] != currentChatServerGroup {
			t.Fatalf("byte2=%d want the server group %d", body[1], currentChatServerGroup)
		}
		if body[2] != 0 {
			t.Fatalf("byte3=%d want 0", body[2])
		}
		if body[3] != readerChannel {
			t.Fatalf("byte4=%d want the reader channel %d", body[3], readerChannel)
		}
		if binary.LittleEndian.Uint16(body[4:6]) != 1 {
			t.Fatalf("sender id=%x", body[4:6])
		}
		want := append(append([]byte(nil), name...), message...)
		if !bytes.Contains(body[6:], want[:len(name)]) || !bytes.Contains(body[6:], message) {
			t.Fatalf("notice dropped name or message: %x", body)
		}
	}
}

// A whisper typed by name arrives with 0xFFFF in the unique-id slot, not the
// recipient's character id. The captured request is
// `01 FF FF 00000000 dstr(鎮勬倓璇? dstr(楝煎墤澹? 01`; looking 0xFFFF up as a
// character id missed and answered the failure code, so neither side saw a line.
func TestCurrentWhisperResolvesPlaceholderTargetByName(t *testing.T) {
	body, err := hex.DecodeString("01ffff000000000600000063c4c7c4bbb00600000067756573740100")
	if err != nil {
		t.Fatal(err)
	}
	// Rebuild the captured shape with names this harness can compare.
	request := buildCurrentPrivateTownChatRequestForTest(currentTownChatWhisperMode,
		currentWhisperTargetUnknownID, []byte("hi"), rosterNameBytes("target1"))
	request[len(request)-1] = 1
	parsed, ok := parseCurrentTownChatRequest(request)
	if !ok {
		t.Fatalf("captured-shape whisper rejected: %x (reference capture %x)", request, body)
	}
	if parsed.TargetUniqueID != currentWhisperTargetUnknownID {
		t.Fatalf("target id=%d want the 0x%X placeholder", parsed.TargetUniqueID, currentWhisperTargetUnknownID)
	}

	service := &Service{gameSessions: map[uint16]*gameSession{}}
	source := &gameSession{selectedCharacterID: 7}
	if _, _, resolved := service.resolveCurrentWhisperTarget(source, parsed); resolved {
		t.Fatal("placeholder target resolved with nobody online")
	}
	if _, _, resolved := service.resolveCurrentWhisperTarget(source, currentTownChatRequest{
		TargetUniqueID: currentWhisperTargetUnknownID,
	}); resolved {
		t.Fatal("placeholder target resolved without a name to match")
	}
}

// Guild delivers on the named-speaker notice, never on op12: op12 identifies its
// speaker by a scene actor key no guild member can resolve, which is what left
// the line unattributed and made the filter treat it as ordinary chat.
func TestCurrentGuildChatDoesNotUseTheAreaChatCarrier(t *testing.T) {
	conn := &bufferConn{}
	sender := &gameSession{conn: conn, channel: channelcatalog.Channel{ID: 16}, selectedCharacterID: 1}
	service := &Service{gameSessions: map[uint16]*gameSession{1: sender}}
	request := buildCurrentTownChatRequestForTest(currentTownChatGuildMode, []byte("guild"))
	parsed, ok := parseCurrentTownChatRequest(request)
	if !ok {
		t.Fatalf("guild request rejected: %x", request)
	}
	if err := service.handleCurrentScopedTownChat(sender, parsed); err != nil {
		t.Fatal(err)
	}
	// No repository, so the guild scope is unavailable and the native failure is
	// the expected answer. What matters is that no op12 line was emitted.
	packet, rest := splitGameServerUpperPacket(t, conn.write.Bytes())
	if packet.Header.MsgID == currentTownChatNotificationMsgID {
		t.Fatal("guild chat fell back to the op12 scene-actor carrier")
	}
	if packet.Header.MsgID != currentPeerChatStateOpcode || len(rest) != 0 {
		t.Fatalf("guild scope failure header=%+v rest=%x", packet.Header, rest)
	}
}

// The captured 鏅€?channel 瑁呭寮曠敤 request (one linked 濞佸＋閲嶅墤). op424 was
// unhandled, so the message never left the client.
const currentCapturedItemHyperlinkChatRequestHex = "030100000000000d0000007f7f5bcec0cabfd6d8bda35d7f01" +
	"b36bffffcf6a00007fab2f1700002d0000000000000000" +
	"000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000" +
	"000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000" +
	"0000000000000000000000000000000000000000000000000000000000"

func TestCurrentCapturedItemHyperlinkChatKeepsOpaqueEntries(t *testing.T) {
	body, err := hex.DecodeString(currentCapturedItemHyperlinkChatRequestHex)
	if err != nil {
		t.Fatal(err)
	}
	if len(body) != 173 {
		t.Fatalf("captured op424 body len=%d want 173", len(body))
	}
	request, ok := parseCurrentTownChatHyperlinkRequest(body)
	if !ok {
		t.Fatalf("captured op424 request rejected: %x", body)
	}
	if request.Mode != currentTownChatAreaMode {
		t.Fatalf("captured op424 mode=%d want %d", request.Mode, currentTownChatAreaMode)
	}
	if request.ItemCount != 1 || len(request.ItemData) != 148 {
		t.Fatalf("captured op424 links=%d bytes=%d want 1 link of 148 bytes", request.ItemCount, len(request.ItemData))
	}
	if !request.hasItemHyperlinks() {
		t.Fatal("captured op424 request does not report its links")
	}
	if !bytes.Equal(request.ItemData, body[25:]) {
		t.Fatalf("entries were rewritten: %x", request.ItemData)
	}

	// The plain op17 decoder must keep rejecting it: its notification has no
	// room for the entries, so accepting it would drop the linked items.
	if _, plainOK := parseCurrentTownChatRequest(body); plainOK {
		t.Fatal("op17 decoder accepted an op424 body")
	}

	// The message keeps the client's own 0x7F-bracketed item name.
	if !bytes.Contains(request.Message, []byte{0x7f}) {
		t.Fatalf("captured op424 message lost its link markers: %x", request.Message)
	}

	// Each 瑁呭寮曠敤 notification is its plain twin's body followed by the count
	// and the entries, which is the shape sub_2717A70 reads. The ids sit one
	// slot below the 86JP reference's 371/372/373 鈥?the same shift its cmd table
	// has at ITEM_HYPERLINK_MESSAGE 鈥?and sending 86JP's numbering displayed
	// nothing live.
	if currentTownChatHyperlinkMsgID != 370 {
		t.Fatalf("op12 hyperlink twin=%d want 370", currentTownChatHyperlinkMsgID)
	}
	if currentPeerChatHyperlinkNotifyOpcode != 372 {
		t.Fatalf("op118 hyperlink twin=%d want 372", currentPeerChatHyperlinkNotifyOpcode)
	}
	for _, base := range [][]byte{
		buildCurrentTownChatNotification(request.Mode, 1, request.Message),
		buildCurrentHyperlinkChatNoticeBody(request.Mode, currentChatServerGroup, 121, 1,
			rosterNameBytes("speaker"), request.Message),
	} {
		notice := appendCurrentChatItemHyperlinks(base, request)
		if !bytes.Equal(notice[:len(base)], base) || notice[len(base)] != 1 ||
			!bytes.Equal(notice[len(base)+1:], request.ItemData) {
			t.Fatalf("hyperlink notice=%x base=%x", notice, base)
		}
		if len(notice) != len(base)+1+148 {
			t.Fatalf("hyperlink notice len=%d want base %d + 1 + 148", len(notice), len(base))
		}
	}

	// op372 puts the reader's channel in byte 3, where op118 puts it in byte 4.
	// Reusing op118's order left a zero in the slot gate 0x272936C reads and the
	// guild 瑁呭寮曠敤 line rendered nowhere.
	const readerChannel = byte(251)
	plain := buildCurrentGuildChatNoticeBody(request.Mode, currentChatServerGroup, readerChannel, 1,
		rosterNameBytes("speaker"), request.Message)
	linked := buildCurrentHyperlinkChatNoticeBody(request.Mode, currentChatServerGroup, readerChannel, 1,
		rosterNameBytes("speaker"), request.Message)
	if plain[1] != currentChatServerGroup || linked[1] != currentChatServerGroup {
		t.Fatalf("server group slot moved: plain=%x linked=%x", plain[:4], linked[:4])
	}
	// Both bodies put the channel in byte 4: sub_1D5B370 gates on [ebp-0xCCE]
	// and sub_2729060 on [ebp-0xCCF], each the fourth byte read.
	if plain[3] != readerChannel || plain[2] != 0 {
		t.Fatalf("op118 body should carry the channel in byte 4: %x", plain[:4])
	}
	if linked[3] != readerChannel || linked[2] != 0 {
		t.Fatalf("op372 body should carry the channel in byte 4: %x", linked[:4])
	}
}

func TestCurrentItemHyperlinkChatRejectsBrokenEntryPayloads(t *testing.T) {
	base := buildCurrentTownChatRequestForTest(currentTownChatAreaMode, []byte("link"))
	for name, body := range map[string][]byte{
		"missing_count":        base,
		"zero_count":           append(append([]byte(nil), base...), 0x00),
		"count_without_entry":  append(append([]byte(nil), base...), 0x01),
		"entries_not_divisble": append(append(append([]byte(nil), base...), 0x02), make([]byte, 9)...),
	} {
		t.Run(name, func(t *testing.T) {
			if _, ok := parseCurrentTownChatHyperlinkRequest(body); ok {
				t.Fatalf("accepted broken op424 body: %x", body)
			}
		})
	}
	good := append(append(append([]byte(nil), base...), 0x02), make([]byte, 8)...)
	request, ok := parseCurrentTownChatHyperlinkRequest(good)
	if !ok || request.ItemCount != 2 || len(request.ItemData) != 8 {
		t.Fatalf("two-link request ok=%t request=%+v", ok, request)
	}
}

func buildCurrentTownChatRequestForTest(mode byte, message []byte) []byte {
	body := make([]byte, currentTownChatRequestPrefixLen+len(message))
	body[0] = mode
	binary.LittleEndian.PutUint32(body[7:11], uint32(len(message)))
	copy(body[currentTownChatRequestPrefixLen:], message)
	return body
}

func buildCurrentPrivateTownChatRequestForTest(mode byte, targetID uint16, message, targetName []byte) []byte {
	body := buildCurrentTownChatRequestForTest(mode, message)
	binary.LittleEndian.PutUint16(body[1:3], targetID)
	nameLength := make([]byte, 4)
	binary.LittleEndian.PutUint32(nameLength, uint32(len(targetName)))
	body = append(body, nameLength...)
	body = append(body, targetName...)
	return append(body, 0)
}
