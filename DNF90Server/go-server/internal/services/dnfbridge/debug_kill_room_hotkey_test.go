package dnfbridge

import (
	"encoding/binary"
	"testing"

	"longheng.io/server/internal/modules/dnf/channelcatalog"
)

func killRoomRequestBody(magic uint32, version, reserved byte, token uint16) []byte {
	body := make([]byte, currentDebugKillRoomRequestLength)
	binary.LittleEndian.PutUint32(body[0:4], magic)
	body[4] = version
	body[5] = reserved
	binary.LittleEndian.PutUint16(body[6:8], token)
	return body
}

func TestParseCurrentDebugKillRoomRequest(t *testing.T) {
	token, ok := parseCurrentDebugKillRoomRequest(
		killRoomRequestBody(currentDebugKillRoomMagic, currentDebugKillRoomVersion, 0, 41))
	if !ok || token != 41 {
		t.Fatalf("valid KRM1 body rejected: token=%d ok=%v", token, ok)
	}

	cases := []struct {
		name string
		body []byte
	}{
		{"short", killRoomRequestBody(currentDebugKillRoomMagic, currentDebugKillRoomVersion, 0, 1)[:7]},
		{"foreign magic", killRoomRequestBody(0x31554D42, currentDebugKillRoomVersion, 0, 1)},
		{"wrong version", killRoomRequestBody(currentDebugKillRoomMagic, 2, 0, 1)},
		{"reserved set", killRoomRequestBody(currentDebugKillRoomMagic, currentDebugKillRoomVersion, 1, 1)},
		{"zero token", killRoomRequestBody(currentDebugKillRoomMagic, currentDebugKillRoomVersion, 0, 0)},
	}
	for _, testCase := range cases {
		if _, ok := parseCurrentDebugKillRoomRequest(testCase.body); ok {
			t.Errorf("%s: malformed body accepted", testCase.name)
		}
	}
}

func TestIsCurrentDebugKillRoomEnvelopeIgnoresBodyLength(t *testing.T) {
	// The dispatcher matches on the magic first so a truncated KRM1 body is
	// rejected by the parser (and logged) instead of falling through to the
	// capability-bitmap parser.
	body := killRoomRequestBody(currentDebugKillRoomMagic, currentDebugKillRoomVersion, 0, 3)
	if !isCurrentDebugKillRoomEnvelope(body[:6]) {
		t.Fatal("truncated KRM1 envelope was not routed to the KRM1 parser")
	}
	if isCurrentDebugKillRoomEnvelope(killRoomRequestBody(0x314B4341, 1, 0, 3)) {
		t.Fatal("ACK1 capability body was mistaken for KRM1")
	}
}

func TestHandleCurrentDebugKillRoomRejectsWhenDisabled(t *testing.T) {
	service := &Service{}
	session := &gameSession{
		connID:              "test-conn",
		channel:             channelcatalog.Channel{ID: 1},
		selectedCharacterID: 7,
	}
	body := killRoomRequestBody(currentDebugKillRoomMagic, currentDebugKillRoomVersion, 0, 9)
	if err := service.handleCurrentDebugKillRoom(session, 0, body); err != nil {
		t.Fatalf("disabled handler returned an error: %v", err)
	}
	if service.debugKillRoomInFlight != 0 {
		t.Fatal("disabled handler claimed the single-flight slot")
	}
}
