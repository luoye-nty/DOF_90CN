package dnfbridge

import (
	"bytes"
	"errors"
	"testing"

	"longheng.io/server/internal/modules/dnf/channelcatalog"
	"longheng.io/server/internal/modules/dnf/raid"
)

func requireCurrentRaidAvailabilityPacket(t *testing.T, stream []byte) []byte {
	t.Helper()
	packet, rest := splitGameServerUpperPacket(t, stream)
	if packet.Header.Classification != 0 || packet.Header.MsgID != raid.RaidAvailabilityMsgID {
		t.Fatalf("raid availability = class %d msg %#x body=%x", packet.Header.Classification, packet.Header.MsgID, packet.Body)
	}
	want := raid.BuildNativeRaidAvailabilityBody()
	if !bytes.Equal(packet.Body, want) {
		t.Fatalf("raid availability body=%x, want %x", packet.Body, want)
	}
	return rest
}

func requireCurrentRaidDirectoryReadyPacket(t *testing.T, stream []byte) []byte {
	t.Helper()
	packet, rest := splitGameServerUpperPacket(t, stream)
	if packet.Header.Classification != 0 || packet.Header.MsgID != raid.RaidDirectoryReadyMsgID ||
		len(packet.Body) != 1 || packet.Body[0] != 0 {
		t.Fatalf("raid directory ready = class %d msg %d body=%x", packet.Header.Classification, packet.Header.MsgID, packet.Body)
	}
	return rest
}

func TestCurrentRaidAvailabilitySendsFixedBodyOnce(t *testing.T) {
	connection := &bufferConn{}
	session := &gameSession{conn: connection, channel: channelcatalog.Channel{ID: 23, Type: 23, Name: "anton"}}
	service := &Service{}

	if err := service.sendCurrentRaidAvailabilityOnce(session, "test_initial"); err != nil {
		t.Fatal(err)
	}
	if !session.raidAvailabilitySent {
		t.Fatal("successful availability write did not mark the session")
	}
	if rest := requireCurrentRaidAvailabilityPacket(t, connection.write.Bytes()); len(rest) != 0 {
		t.Fatalf("availability packet left trailing bytes: %x", rest)
	}

	connection.write.Reset()
	if err := service.sendCurrentRaidAvailabilityOnce(session, "test_repeat"); err != nil {
		t.Fatal(err)
	}
	if connection.write.Len() != 0 {
		t.Fatalf("repeated availability write emitted %d bytes", connection.write.Len())
	}
}

func TestCurrentRaidAvailabilityWriteFailureRemainsRetryable(t *testing.T) {
	wantErr := errors.New("raid availability write failed")
	connection := &failNthDungeonWriteConn{failAt: 1, err: wantErr}
	session := &gameSession{conn: connection}
	service := &Service{}

	if err := service.sendCurrentRaidAvailabilityOnce(session, "test_failure"); !errors.Is(err, wantErr) {
		t.Fatalf("send error=%v, want %v", err, wantErr)
	}
	if session.raidAvailabilitySent {
		t.Fatal("failed availability write marked the session")
	}

	session.conn = &bufferConn{}
	if err := service.sendCurrentRaidAvailabilityOnce(session, "test_retry"); err != nil {
		t.Fatal(err)
	}
	if !session.raidAvailabilitySent {
		t.Fatal("successful availability retry did not mark the session")
	}
}

func TestCurrentRaidDirectoryReadySendsOnceOnlyForTeamChannels(t *testing.T) {
	for _, channelType := range []uint8{23, 32, 42} {
		connection := &bufferConn{}
		session := &gameSession{
			conn:                   connection,
			channel:                channelcatalog.Channel{Type: channelType},
			sceneBootstrapTailSent: true,
		}
		service := &Service{}

		if err := service.sendCurrentRaidDirectoryReadyOnce(session, "test_initial"); err != nil {
			t.Fatal(err)
		}
		if !session.raidDirectoryReadySent {
			t.Fatalf("channel type %d was not marked ready", channelType)
		}
		if rest := requireCurrentRaidDirectoryReadyPacket(t, connection.write.Bytes()); len(rest) != 0 {
			t.Fatalf("channel type %d left trailing bytes: %x", channelType, rest)
		}

		connection.write.Reset()
		if err := service.sendCurrentRaidDirectoryReadyOnce(session, "test_repeat"); err != nil {
			t.Fatal(err)
		}
		if connection.write.Len() != 0 {
			t.Fatalf("channel type %d repeated ready write emitted %d bytes", channelType, connection.write.Len())
		}
	}

	connection := &bufferConn{}
	session := &gameSession{
		conn:                   connection,
		channel:                channelcatalog.Channel{Type: 11},
		sceneBootstrapTailSent: true,
	}
	if err := (&Service{}).sendCurrentRaidDirectoryReadyOnce(session, "ordinary_channel"); err != nil {
		t.Fatal(err)
	}
	if session.raidDirectoryReadySent || connection.write.Len() != 0 {
		t.Fatalf("ordinary channel emitted raid directory ready: sent=%t bytes=%d", session.raidDirectoryReadySent, connection.write.Len())
	}
}

func TestCurrentRaidDirectoryReadyWaitsForFinalizedScene(t *testing.T) {
	connection := &bufferConn{}
	session := &gameSession{
		conn:                       connection,
		channel:                    channelcatalog.Channel{Type: 23},
		sceneBootstrapTailDeferred: true,
	}
	service := &Service{}

	if err := service.sendCurrentRaidDirectoryReadyOnce(session, "before_scene_tail"); err != nil {
		t.Fatal(err)
	}
	if session.raidDirectoryReadySent || connection.write.Len() != 0 {
		t.Fatalf("pre-scene ready sent=%t bytes=%d", session.raidDirectoryReadySent, connection.write.Len())
	}

	session.sceneBootstrapTailDeferred = false
	session.sceneBootstrapTailSent = true
	if err := service.sendCurrentRaidDirectoryReadyOnce(session, "after_scene_tail"); err != nil {
		t.Fatal(err)
	}
	if !session.raidDirectoryReadySent {
		t.Fatal("finalized scene did not send raid directory readiness")
	}
	if rest := requireCurrentRaidDirectoryReadyPacket(t, connection.write.Bytes()); len(rest) != 0 {
		t.Fatalf("finalized scene ready left trailing bytes: %x", rest)
	}
}

func TestCurrentRaidDirectoryReadyFailureRemainsRetryable(t *testing.T) {
	wantErr := errors.New("raid directory ready write failed")
	session := &gameSession{
		conn:                   &failNthDungeonWriteConn{failAt: 1, err: wantErr},
		channel:                channelcatalog.Channel{Type: 23},
		sceneBootstrapTailSent: true,
	}
	service := &Service{}

	if err := service.sendCurrentRaidDirectoryReadyOnce(session, "test_failure"); !errors.Is(err, wantErr) {
		t.Fatalf("send error=%v, want %v", err, wantErr)
	}
	if session.raidDirectoryReadySent {
		t.Fatal("failed ready write marked the session")
	}

	session.conn = &bufferConn{}
	if err := service.sendCurrentRaidDirectoryReadyOnce(session, "test_retry"); err != nil {
		t.Fatal(err)
	}
	if !session.raidDirectoryReadySent {
		t.Fatal("successful ready retry did not mark the session")
	}
}

func TestCurrentRaidDirectoryReadyResetsWhenCharacterChanges(t *testing.T) {
	session := &gameSession{channel: channelcatalog.Channel{Type: 23}}
	service := &Service{}
	service.bindGameSessionCharacter(session, 1001)
	session.raidDirectoryReadySent = true

	service.bindGameSessionCharacter(session, 1001)
	if !session.raidDirectoryReadySent {
		t.Fatal("rebinding the same character reset raid directory readiness")
	}
	service.bindGameSessionCharacter(session, 1002)
	if session.raidDirectoryReadySent {
		t.Fatal("changing character preserved stale raid directory readiness")
	}
}
