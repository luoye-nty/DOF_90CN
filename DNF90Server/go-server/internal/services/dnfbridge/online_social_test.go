package dnfbridge

import (
	"bytes"
	"context"
	"encoding/binary"
	"testing"

	"longheng.io/server/internal/modules/dnf/channelcatalog"
	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfproto "longheng.io/server/internal/modules/dnf/protocol"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

const currentSocialTestFriendName = "friend5"

func TestDecodeCurrentPeerChatRequestMatchesLiveMenuShape(t *testing.T) {
	var writer packetWriter
	writer.writeByte(0x24)
	writer.writeUint16(5)
	writer.writeUint32(0)
	writer.writeRawDstr([]byte("hello "))
	writer.writeRawDstr(rosterNameBytes(currentSocialTestFriendName))

	request, err := decodeCurrentPeerChatRequest(writer.bytes())
	if err != nil {
		t.Fatal(err)
	}
	if request.MessageType != 0x24 || request.TargetID != 5 || request.Unknown != 0 ||
		!bytes.Equal(request.Message, []byte("hello ")) ||
		!bytes.Equal(request.TargetName, rosterNameBytes(currentSocialTestFriendName)) {
		t.Fatalf("decoded request = %+v", request)
	}
}

func TestHandleCurrentFriendAddAcknowledgesAndProjectsPersistentFriendList(t *testing.T) {
	service, source, _, sourceConn, _ := newCurrentSocialPeerTest(t)
	var request packetWriter
	request.writeUint16(5)
	request.writeByte(0)
	request.writeRawDstr(rosterNameBytes(currentSocialTestFriendName))

	handled, err := service.handleAlignedGameCommand(source, byte(dnfenum.GameCmdCommand), currentFriendAddOpcode, request.bytes())
	if err != nil {
		t.Fatal(err)
	}
	if !handled {
		t.Fatal("friend add was not handled")
	}
	ack, rest := splitGameServerUpperPacket(t, sourceConn.write.Bytes())
	if ack.Header.Classification != dnfproto.DefaultChannelClassification || ack.Header.MsgID != currentFriendAddOpcode ||
		!bytes.Equal(ack.Body, append([]byte{1}, buildCurrentFriendSuccessBody()...)) {
		t.Fatalf("friend ack = class %d msg %d body=% X", ack.Header.Classification, ack.Header.MsgID, ack.Body)
	}
	first, rest := splitGameServerUpperPacket(t, rest)
	second, rest := splitGameServerUpperPacket(t, rest)
	if first.Header.Classification != 0 || first.Header.MsgID != currentFriendListOpcode ||
		second.Header.Classification != 0 || second.Header.MsgID != currentFriendListOpcode ||
		!bytes.Equal(first.Body, second.Body) || len(rest) != 0 {
		t.Fatalf("friend list first=%+v second=%+v trailing=%d", first.Header, second.Header, len(rest))
	}
	if got := binary.LittleEndian.Uint32(first.Body[4:8]); got != 1 {
		t.Fatalf("friend list count=%d body=%x", got, first.Body)
	}
	assertCurrentUnitedFriendSingleRowBody(t, first.Body, defaultCurrentRegion, 42, false)
}

func TestHandleCurrentFriendAddMalformedRequestReleasesClientPendingGate(t *testing.T) {
	service, source, _, sourceConn, _ := newCurrentSocialPeerTest(t)

	handled, err := service.handleAlignedGameCommand(source, byte(dnfenum.GameCmdCommand), currentFriendAddOpcode, []byte{1, 0})
	if err != nil {
		t.Fatal(err)
	}
	if !handled {
		t.Fatal("malformed friend add was not handled")
	}
	ack, rest := splitGameServerUpperPacket(t, sourceConn.write.Bytes())
	if ack.Header.Classification != dnfproto.DefaultChannelClassification || ack.Header.MsgID != currentFriendAddOpcode ||
		!bytes.Equal(ack.Body, []byte{0, currentFriendFailureMissing}) || len(rest) != 0 {
		t.Fatalf("malformed friend add ack = class %d msg %d body=% X trailing=%d", ack.Header.Classification, ack.Header.MsgID, ack.Body, len(rest))
	}
}

func TestHandleCurrentFriendDeleteAcknowledgesDeletesVisibleRowAndPersists(t *testing.T) {
	service, source, _, sourceConn, _ := newCurrentSocialPeerTest(t)
	var add packetWriter
	add.writeUint16(5)
	add.writeByte(0)
	add.writeRawDstr(rosterNameBytes(currentSocialTestFriendName))
	if err := service.handleCurrentFriendAdd(source, currentFriendAddOpcode, add.bytes()); err != nil {
		t.Fatal(err)
	}
	sourceConn.write.Reset()

	var remove packetWriter
	remove.writeByte(0)
	remove.writeUint32(5)
	remove.writeBytes(rosterNameBytes(currentSocialTestFriendName))
	handled, err := service.handleAlignedGameCommand(source, byte(dnfenum.GameCmdCommand), currentFriendDeleteOpcode, remove.bytes())
	if err != nil {
		t.Fatal(err)
	}
	if !handled {
		t.Fatal("friend delete was not handled")
	}
	ack, rest := splitGameServerUpperPacket(t, sourceConn.write.Bytes())
	if ack.Header.MsgID != currentFriendDeleteOpcode ||
		!bytes.Equal(ack.Body, append([]byte{1}, buildCurrentFriendSuccessBody()...)) {
		t.Fatalf("delete ack = msg %d body=% X", ack.Header.MsgID, ack.Body)
	}
	deleted, rest := splitGameServerUpperPacket(t, rest)
	if deleted.Header.MsgID != currentFriendListOpcode ||
		!bytes.Equal(deleted.Body, buildCurrentUnitedFriendDeleteBody(defaultCurrentRegion, currentSocialTestFriendName)) {
		t.Fatalf("delete row = msg %d body=% X", deleted.Header.MsgID, deleted.Body)
	}
	first, rest := splitGameServerUpperPacket(t, rest)
	second, rest := splitGameServerUpperPacket(t, rest)
	if first.Header.MsgID != currentFriendListOpcode || second.Header.MsgID != currentFriendListOpcode ||
		!bytes.Equal(first.Body, second.Body) || len(first.Body) != 8 ||
		binary.LittleEndian.Uint32(first.Body[4:8]) != 0 || len(rest) != 0 {
		t.Fatalf("absolute empty refresh first=% X second=% X trailing=%d", first.Body, second.Body, len(rest))
	}
}

func assertCurrentUnitedFriendSingleRowBody(t *testing.T, body []byte, wantRegion byte, wantChannel uint16, wantBusy bool) {
	t.Helper()
	if len(body) < 20 || binary.LittleEndian.Uint32(body[:4]) != 0 || binary.LittleEndian.Uint32(body[4:8]) != 1 {
		t.Fatalf("friend list header/body=% X", body)
	}
	if body[8] != wantRegion || binary.LittleEndian.Uint16(body[9:11]) != wantChannel {
		t.Fatalf("friend row region/channel body=% X", body)
	}
	name, next, ok := readCurrentRawDstr(body, 11)
	if !ok || !bytes.Equal(name, rosterNameBytes(currentSocialTestFriendName)) {
		t.Fatalf("friend row name=% X ok=%t body=% X", name, ok, body)
	}
	// Current NoPack sub_26E8680 consumes level,u32 job,grow,Y,Z and one
	// final boolean. Pinning the exact tail prevents a multi-row shift back to
	// the older 86JP/A21 layout that lacks this client's last byte.
	if len(body)-next != 12 || body[next] != 90 || binary.LittleEndian.Uint32(body[next+1:next+5]) != 1 ||
		body[next+5] != 0 || body[next+6] != boolByte(wantBusy) ||
		binary.LittleEndian.Uint32(body[next+7:next+11]) != 0 || body[next+11] != 0 {
		t.Fatalf("current friend row tail=% X", body[next:])
	}
}

func TestCurrentUnitedFriendListUsesOfflineOnlineBusyStates(t *testing.T) {
	service, source, target, sourceConn, _ := newCurrentSocialPeerTest(t)
	var add packetWriter
	add.writeUint16(5)
	add.writeByte(0)
	add.writeRawDstr(rosterNameBytes(currentSocialTestFriendName))
	if err := service.handleCurrentFriendAdd(source, currentFriendAddOpcode, add.bytes()); err != nil {
		t.Fatal(err)
	}

	assertState := func(wantChannel uint16, wantBusy bool) {
		t.Helper()
		sourceConn.write.Reset()
		if err := service.sendCurrentUnitedFriendList(source, "source1", "friend_presence_state_test"); err != nil {
			t.Fatal(err)
		}
		first, rest := splitGameServerUpperPacket(t, sourceConn.write.Bytes())
		_, rest = splitGameServerUpperPacket(t, rest)
		if len(rest) != 0 {
			t.Fatalf("friend state refresh trailing=%d", len(rest))
		}
		assertCurrentUnitedFriendSingleRowBody(t, first.Body, defaultCurrentRegion, wantChannel, wantBusy)
	}

	assertState(42, false)
	service.onlinePlayers.LeaveAreaSession(5, target)
	assertState(42, true)
	service.mu.Lock()
	delete(service.gameSessions, 5)
	service.mu.Unlock()
	assertState(0, false)
}

func TestCurrentUnitedFriendListUsesConfiguredRegionID(t *testing.T) {
	service, source, _, sourceConn, _ := newCurrentSocialPeerTest(t)
	service.options.regionID = 77
	var add packetWriter
	add.writeUint16(5)
	add.writeByte(0)
	add.writeRawDstr(rosterNameBytes(currentSocialTestFriendName))
	if err := service.handleCurrentFriendAdd(source, currentFriendAddOpcode, add.bytes()); err != nil {
		t.Fatal(err)
	}

	_, rest := splitGameServerUpperPacket(t, sourceConn.write.Bytes())
	packet, _ := splitGameServerUpperPacket(t, rest)
	assertCurrentUnitedFriendSingleRowBody(t, packet.Body, 77, 42, false)
	if body := buildCurrentUnitedFriendDeleteBody(service.currentRegionID(), currentSocialTestFriendName); len(body) < 9 || body[8] != 77 {
		t.Fatalf("configured region delete body=% X", body)
	}
}

func TestHandleCurrentPeerChatAcknowledgesAndForwardsCurrentNotifications(t *testing.T) {
	service, source, _, sourceConn, targetConn := newCurrentSocialPeerTest(t)
	var request packetWriter
	request.writeByte(1)
	request.writeUint16(5)
	request.writeUint32(0)
	request.writeRawDstr([]byte("hello"))
	request.writeRawDstr(rosterNameBytes(currentSocialTestFriendName))
	request.writeByte(0)

	handled, err := service.handleAlignedGameCommand(source, byte(dnfenum.GameCmdCommand), currentPeerChatStateOpcode, request.bytes())
	if err != nil {
		t.Fatal(err)
	}
	if !handled {
		t.Fatal("peer chat was not handled")
	}
	ack, sourceRest := splitGameServerUpperPacket(t, sourceConn.write.Bytes())
	if ack.Header.Classification != dnfproto.DefaultChannelClassification || ack.Header.MsgID != currentPeerChatStateOpcode ||
		!bytes.Equal(ack.Body, []byte{1}) {
		t.Fatalf("chat ack = class %d msg %d body=% X", ack.Header.Classification, ack.Header.MsgID, ack.Body)
	}
	sourceState, sourceRest := splitGameServerUpperPacket(t, sourceRest)
	if sourceState.Header.Classification != 0 || sourceState.Header.MsgID != currentPeerChatStateOpcode ||
		!bytes.Equal(sourceState.Body, buildCurrentPeerChatStateBody(5, 1)) {
		t.Fatalf("source chat state = class %d msg %d body=% X trailing=%d", sourceState.Header.Classification, sourceState.Header.MsgID, sourceState.Body, len(sourceRest))
	}
	sourceNotice, sourceRest := splitGameServerUpperPacket(t, sourceRest)
	// The sender is named its peer and told the outgoing twin of the mode.
	wantSourceNotice := buildCurrentGuildChatNoticeBody(currentTownChatWhisperSentMode, currentChatServerGroup, byte(source.channel.ID), 0, rosterNameBytes(currentSocialTestFriendName), []byte("hello"))
	if sourceNotice.Header.Classification != 0 || sourceNotice.Header.MsgID != currentPeerChatNotifyOpcode ||
		!bytes.Equal(sourceNotice.Body, wantSourceNotice) || len(sourceRest) != 0 {
		t.Fatalf("source chat notice = class %d msg %d body=% X trailing=%d", sourceNotice.Header.Classification, sourceNotice.Header.MsgID, sourceNotice.Body, len(sourceRest))
	}

	targetState, targetRest := splitGameServerUpperPacket(t, targetConn.write.Bytes())
	if targetState.Header.Classification != 0 || targetState.Header.MsgID != currentPeerChatStateOpcode ||
		!bytes.Equal(targetState.Body, buildCurrentPeerChatStateBody(1, 1)) {
		t.Fatalf("target chat state = class %d msg %d body=% X", targetState.Header.Classification, targetState.Header.MsgID, targetState.Body)
	}
	targetNotice, targetRest := splitGameServerUpperPacket(t, targetRest)
	wantNotice := buildCurrentGuildChatNoticeBody(1, currentChatServerGroup, 42, 0, rosterNameBytes("source1"), []byte("hello"))
	if targetNotice.Header.Classification != 0 || targetNotice.Header.MsgID != currentPeerChatNotifyOpcode ||
		!bytes.Equal(targetNotice.Body, wantNotice) || len(targetRest) != 0 {
		t.Fatalf("target chat notice = class %d msg %d body=% X trailing=%d", targetNotice.Header.Classification, targetNotice.Header.MsgID, targetNotice.Body, len(targetRest))
	}
}

func TestHandleCurrentBlacklistMutationEchoesNameDstrInSuccess(t *testing.T) {
	conn := &bufferConn{}
	service := &Service{options: options{gameUpperBodyCodec: gameUpperBodyCodecPlain}}
	session := &gameSession{conn: conn, selectedCharacterID: 1}
	var request packetWriter
	request.writeRawDstr(rosterNameBytes(currentSocialTestFriendName))

	handled, err := service.handleAlignedGameCommand(session, byte(dnfenum.GameCmdCommand), uint16(dnfenum.CmdPacketRegisiterToBlacklist), request.bytes())
	if err != nil {
		t.Fatal(err)
	}
	if !handled {
		t.Fatal("blacklist mutation was not handled")
	}
	packet, rest := splitGameServerUpperPacket(t, conn.write.Bytes())
	want := append([]byte{1}, request.bytes()...)
	if packet.Header.Classification != dnfproto.DefaultChannelClassification ||
		packet.Header.MsgID != uint16(dnfenum.CmdPacketRegisiterToBlacklist) ||
		!bytes.Equal(packet.Body, want) || len(rest) != 0 {
		t.Fatalf("blacklist ack = class %d msg %d body=% X trailing=%d", packet.Header.Classification, packet.Header.MsgID, packet.Body, len(rest))
	}
}

func newCurrentSocialPeerTest(t *testing.T) (*Service, *gameSession, *gameSession, *bufferConn, *bufferConn) {
	t.Helper()
	repositories := dnfrepomemory.NewMemoryGroup()
	ctx := context.Background()
	for _, character := range []dnfrepo.CharacterRecord{
		{CharacterID: "1", AccountID: "dnf:1", Name: "source1", Job: "0", Level: 90},
		{CharacterID: "5", AccountID: "dnf:2", Name: currentSocialTestFriendName, Job: "1", Level: 90},
	} {
		if err := repositories.Character.Save(ctx, character); err != nil {
			t.Fatal(err)
		}
	}
	sourceConn := &bufferConn{}
	targetConn := &bufferConn{}
	channel := channelcatalog.Channel{ID: 42, Name: "ch.42"}
	source := &gameSession{conn: sourceConn, channel: channel, accountID: "dnf:1", selectedCharacterID: 1}
	target := &gameSession{conn: targetConn, channel: channel, accountID: "dnf:2", selectedCharacterID: 5}
	service := &Service{
		options:       options{gameUpperBodyCodec: gameUpperBodyCodecPlain},
		onlinePlayers: newOnlinePlayerManager(),
		repositoryProvider: func() (dnfrepo.Group, bool) {
			return repositories, true
		},
	}
	service.bindGameSessionCharacter(source, 1)
	service.bindGameSessionCharacter(target, 5)
	service.onlinePlayers.EnterArea(&onlinePlayerInfo{CharacterID: 1, Name: "source1", TownID: 1, AreaID: 1, Session: source})
	service.onlinePlayers.EnterArea(&onlinePlayerInfo{CharacterID: 5, Name: currentSocialTestFriendName, TownID: 1, AreaID: 1, Session: target})
	return service, source, target, sourceConn, targetConn
}
