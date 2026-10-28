package dnfbridge

import (
	"bytes"
	"encoding/binary"
	"testing"

	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

func TestBuildCurrentMoodProjectionBody(t *testing.T) {
	body := buildCurrentMoodProjectionBody(7)
	if len(body) != currentMoodProjectionBodyLength || body[0] != currentMoodProjectionType || body[1] != currentMoodProjectionVersion || binary.LittleEndian.Uint16(body[2:4]) != 7 {
		t.Fatalf("mood projection body=%x", body)
	}
}

func TestSendCurrentMoodProjectionUsesPrivateClass0Envelope(t *testing.T) {
	service, session, connection := newGameplayDomainBridgeFixture(dnfrepomemory.NewMemoryGroup())
	if err := service.sendCurrentMoodProjection(session, 7, "test"); err != nil {
		t.Fatalf("send mood projection: %v", err)
	}
	packet, trailing := splitGameServerUpperPacket(t, connection.write.Bytes())
	if packet.Header.Classification != 0 || packet.Header.MsgID != currentMoodProjectionMsgID || !bytes.Equal(packet.Body, buildCurrentMoodProjectionBody(7)) {
		t.Fatalf("mood projection packet header=%+v body=%x", packet.Header, packet.Body)
	}
	if len(trailing) != 0 {
		t.Fatalf("unexpected mood projection trailing packet=%x", trailing)
	}
}
