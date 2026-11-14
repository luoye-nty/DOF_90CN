package dnfbridge

import (
	"bytes"
	"encoding/binary"
	"testing"

	"longheng.io/server/internal/modules/dnf/channelcatalog"
	dnfproto "longheng.io/server/internal/modules/dnf/protocol"
)

func TestCurrentLocationHotkeyReturnsSelectedCharactersTownPositionByWhisper(t *testing.T) {
	conn := &bufferConn{}
	session := &gameSession{
		conn:                      conn,
		channel:                   channelcatalog.Channel{ID: 41},
		selectedCharacterID:       19,
		townSceneReadyCharacterID: 19,
		townPositionSnapshot: currentTownPositionSnapshot{
			CharacterID: 19, TownID: 40, AreaID: 0,
			PositionX: 1480, PositionY: 480, PositionValid: true,
		},
	}
	service := &Service{options: options{gameUpperBodyCodec: gameUpperBodyCodecPlain}}
	body := make([]byte, 8)
	binary.LittleEndian.PutUint32(body[0:4], currentLocationHotkeyMagic)
	body[4] = currentLocationHotkeyVersion
	binary.LittleEndian.PutUint16(body[6:8], 7)

	if err := service.handleCurrentLocationHotkey(session, dnfproto.DefaultChannelClassification, body); err != nil {
		t.Fatalf("handle location hotkey: %v", err)
	}

	state, rest := splitGameServerUpperPacket(t, conn.write.Bytes())
	if state.Header.Classification != 0 || state.Header.MsgID != currentPeerChatStateOpcode ||
		!bytes.Equal(state.Body, buildCurrentPeerChatStateBody(19, currentTownChatWhisperMode)) {
		t.Fatalf("private chat state header=%+v body=%x", state.Header, state.Body)
	}
	notice, trailing := splitGameServerUpperPacket(t, rest)
	message := encodeCurrentLocationHotkeyText(service.currentLocationHotkeyText(currentLocationHotkeyReply{
		ChannelID: 41, TownID: 40, AreaID: 0, PositionX: 1480, PositionY: 480, HasPosition: true,
	}))
	want := buildCurrentGuildChatNoticeBody(
		currentTownChatWhisperMode,
		currentChatServerGroup,
		41,
		0,
		rosterNameBytes("位置助手"),
		message,
	)
	if notice.Header.Classification != 0 || notice.Header.MsgID != currentPeerChatNotifyOpcode ||
		!bytes.Equal(notice.Body, want) || len(trailing) != 0 {
		t.Fatalf("private chat notice header=%+v body=%x want=%x trailing=%x",
			notice.Header, notice.Body, want, trailing)
	}
}

func TestCurrentLocationHotkeyRejectsInvalidEnvelope(t *testing.T) {
	if _, ok := parseCurrentLocationHotkeyRequest([]byte("LOC1\x02\x00\x01\x00")); ok {
		t.Fatal("unsupported LOC1 version was accepted")
	}
	if _, ok := parseCurrentLocationHotkeyRequest([]byte("LOC1\x01\x00\x00\x00")); ok {
		t.Fatal("zero LOC1 token was accepted")
	}
}
