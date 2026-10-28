package dnfbridge

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"

	dnfproto "longheng.io/server/internal/modules/dnf/protocol"
)

func TestBuildCurrentNativeNoticeBodyMatchesClientReader(t *testing.T) {
	body, err := buildCurrentNativeNoticeBody(currentNativeNoticeTypeBroadcast, 0, 0, "转职提示")
	if err != nil {
		t.Fatal(err)
	}
	// GBK, verified independently: 转=D7AA 职=D6B0 提=CCE1 示=CABE.
	wantText := []byte{0xD7, 0xAA, 0xD6, 0xB0, 0xCC, 0xE1, 0xCA, 0xBE}
	if body[0] != byte(currentNativeNoticeTypeBroadcast) {
		t.Fatalf("notice type=%d", body[0])
	}
	if binary.LittleEndian.Uint16(body[1:3]) != 0 || body[3] != currentNativeNoticeReservedU8 {
		t.Fatalf("notice reserved fields=%x", body[1:4])
	}
	if got := binary.LittleEndian.Uint32(body[4:8]); got != uint32(len(wantText)) {
		t.Fatalf("notice byte length=%d want %d", got, len(wantText))
	}
	if !bytes.Equal(body[8:], wantText) {
		t.Fatalf("notice text=%x want=%x", body[8:], wantText)
	}
}

func TestBuildCurrentNativeNoticeBodyRejectsOutOfBoundInput(t *testing.T) {
	if _, err := buildCurrentNativeNoticeBody(currentNativeNoticeTypeSystem, 0, 0, ""); err == nil {
		t.Fatal("empty native notice text was accepted")
	}
	// The client reader rejects a type byte at or above 0x40 before it reads
	// anything else.
	if _, err := buildCurrentNativeNoticeBody(currentNativeNoticeType(currentNativeNoticeMaxType), 0, 0, "提示"); err == nil {
		t.Fatal("out-of-bound native notice type was accepted")
	}
	if _, err := buildCurrentNativeNoticeBody(currentNativeNoticeType(currentNativeNoticeMaxType-1), 0, 0, "提示"); err != nil {
		t.Fatalf("highest valid native notice type was rejected: %v", err)
	}
	if _, err := buildCurrentNativeNoticeBody(currentNativeNoticeTypeSystem, currentNativeNoticeMaxChannel, 0, "提示"); err == nil {
		t.Fatal("out-of-bound native notice channel was accepted")
	}
	if _, err := buildCurrentNativeNoticeBody(currentNativeNoticeTypeSystem, currentNativeNoticeMaxChannel-1, 0, "提示"); err != nil {
		t.Fatalf("highest valid native notice channel was rejected: %v", err)
	}
	// The reader rejects a declared length of 0x100 or more, so 255 GBK bytes
	// is the last accepted size.
	if _, err := buildCurrentNativeNoticeBody(currentNativeNoticeTypeSystem, 0, 0,
		strings.Repeat("a", currentNativeNoticeMaxBodySize),
	); err != nil {
		t.Fatalf("native notice text at the exact bound was rejected: %v", err)
	}
	if _, err := buildCurrentNativeNoticeBody(currentNativeNoticeTypeSystem, 0, 0,
		strings.Repeat("a", currentNativeNoticeMaxBodySize+1),
	); err == nil {
		t.Fatal("native notice text above the client bound was accepted")
	}
	// Chinese costs two GBK bytes per character, so the bound is in bytes.
	if _, err := buildCurrentNativeNoticeBody(currentNativeNoticeTypeSystem, 0, 0,
		strings.Repeat("提", currentNativeNoticeMaxBodySize/2+1),
	); err == nil {
		t.Fatal("native notice text above the client bound in GBK bytes was accepted")
	}
}

func TestSendCurrentNativeNoticeWritesClass0Noti12(t *testing.T) {
	connection := &bufferConn{}
	service := &Service{options: options{gameUpperBodyCodec: gameUpperBodyCodecPlain}}
	session := &gameSession{conn: connection, selectedCharacterID: 17}
	if err := service.sendCurrentNativeNotice(
		session, currentNativeNoticeTypeSystem, 0, 0, "转职提示", "unit_test",
	); err != nil {
		t.Fatal(err)
	}
	// Class0 notices ship with the fixed 16-byte scene header.
	packet, trailing := splitGameServerUpperPacketWithHeader(
		t, connection.write.Bytes(), dnfproto.GameServerUpperHeaderSize16,
	)
	if packet.Header.MsgID != currentNativeNoticeMsgID || packet.Header.Classification != 0 ||
		len(trailing) != 0 {
		t.Fatalf("native notice header=%+v trailing=%x", packet.Header, trailing)
	}
	want, err := buildCurrentNativeNoticeBody(currentNativeNoticeTypeSystem, 0, 0, "转职提示")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(packet.Body, want) {
		t.Fatalf("native notice body=%x want=%x", packet.Body, want)
	}
}

func TestSendCurrentNativeNoticeDropsUnencodableTextWithoutWriting(t *testing.T) {
	connection := &bufferConn{}
	service := &Service{options: options{gameUpperBodyCodec: gameUpperBodyCodecPlain}}
	session := &gameSession{conn: connection, selectedCharacterID: 17}
	if err := service.sendCurrentNativeNotice(
		session, currentNativeNoticeTypeSystem, 0, 0, "", "unit_test",
	); err != nil {
		t.Fatal(err)
	}
	if got := connection.write.Bytes(); len(got) != 0 {
		t.Fatalf("unencodable native notice wrote=%x", got)
	}
}
