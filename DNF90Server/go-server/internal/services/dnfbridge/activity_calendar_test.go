package dnfbridge

import (
	"bytes"
	"encoding/binary"
	"testing"

	"longheng.io/server/internal/modules/dnf/channelcatalog"
	dnfproto "longheng.io/server/internal/modules/dnf/protocol"
)

func TestBuildCurrentActivityCalendarBodyMatchesVerifiedLayout(t *testing.T) {
	body, err := buildCurrentActivityCalendarBody()
	if err != nil {
		t.Fatal(err)
	}
	if len(body) != 184 {
		t.Fatalf("calendar body length=%d want=184", len(body))
	}
	if got := binary.LittleEndian.Uint16(body[:2]); got != 7 {
		t.Fatalf("record count=%d want=7", got)
	}
	if got := binary.LittleEndian.Uint16(body[2:4]); got != 181 {
		t.Fatalf("first event id=%d want=181", got)
	}
	if got := binary.LittleEndian.Uint32(body[4:8]); got != 0xff6e00ff {
		t.Fatalf("first field0=%#x want=%#x", got, uint32(0xff6e00ff))
	}
	if body[16] != 1 {
		t.Fatalf("first extension flag=%d want=1", body[16])
	}
	if got := body[len(body)-1]; got != 0 {
		t.Fatalf("reward tail byte=%d want=0", got)
	}
}

func TestSendCurrentActivityCalendarUsesStandaloneProtectedClass0Packet(t *testing.T) {
	connection := &bufferConn{}
	service := &Service{options: options{pvfPath: "Script.pvf"}}
	session := &gameSession{
		conn:    connection,
		connID:  "activity-calendar-test",
		channel: channelcatalog.Channel{ID: 19},
	}
	if err := service.sendCurrentActivityCalendar(session, "test_select_ack"); err != nil {
		t.Fatal(err)
	}
	body, err := buildCurrentActivityCalendarBody()
	if err != nil {
		t.Fatal(err)
	}
	packet, rest := splitGameServerUpperPacketWithHeader(t, connection.write.Bytes(), dnfproto.GameServerUpperHeaderSize16)
	if len(rest) != 0 {
		t.Fatalf("calendar packet has trailing bytes=%x", rest)
	}
	if packet.Header.Classification != 0 || packet.Header.MsgID != currentActivityCalendarMsgID ||
		packet.Header.Checksum != 1 || connection.write.Bytes()[15] != 1 {
		t.Fatalf("calendar header=%+v raw=%x", packet.Header, connection.write.Bytes()[:dnfproto.GameServerUpperHeaderSize16])
	}
	if !bytes.Equal(packet.Body, body) {
		t.Fatalf("calendar body=%x want=%x", packet.Body, body)
	}
}
