package dnfbridge

import (
	"encoding/binary"
	"math"
	"testing"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

func TestCurrentExpertJobProjectionTrailerCarriesClampedExperience(t *testing.T) {
	tests := []struct {
		name       string
		announce   bool
		wantMode   uint32
		experience int64
		want       uint32
	}{
		{name: "announce_durable", announce: true, wantMode: currentExpertJobProjectionAnnounce, experience: 302, want: 302},
		{name: "silent_durable", announce: false, wantMode: currentExpertJobProjectionSilent, experience: 302, want: 302},
		{name: "negative", announce: false, wantMode: currentExpertJobProjectionSilent, experience: -1, want: 0},
		{name: "announce_negative", announce: true, wantMode: currentExpertJobProjectionAnnounce, experience: -1, want: 0},
		{name: "overflow", announce: false, wantMode: currentExpertJobProjectionSilent, experience: int64(math.MaxUint32) + 1, want: math.MaxUint32},
		{name: "announce_overflow", announce: true, wantMode: currentExpertJobProjectionAnnounce, experience: int64(math.MaxUint32) + 1, want: math.MaxUint32},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			w := packetWriter{}
			writeCurrentExpertJobProjectionTrailer(&w, test.experience, test.announce)
			body := w.bytes()
			if len(body) != 16 {
				t.Fatalf("trailer length=%d, want 16", len(body))
			}
			// Offsets are pinned: the compatibility DLL reads the experience
			// pair from the LAST eight bytes of the op205 body and the mode
			// block from the eight bytes before it. Nothing may be appended
			// after "EJXP", and the mode block may not move after it either.
			if magic := binary.LittleEndian.Uint32(body[:4]); magic != currentExpertJobProjectionModeMagic {
				t.Fatalf("mode magic=%#x, want %#x", magic, currentExpertJobProjectionModeMagic)
			}
			if mode := binary.LittleEndian.Uint32(body[4:8]); mode != test.wantMode {
				t.Fatalf("mode=%d, want %d", mode, test.wantMode)
			}
			if magic := binary.LittleEndian.Uint32(body[8:12]); magic != currentExpertJobProjectionTrailerMagic {
				t.Fatalf("trailer magic=%#x, want %#x", magic, currentExpertJobProjectionTrailerMagic)
			}
			if experience := binary.LittleEndian.Uint32(body[12:]); experience != test.want {
				t.Fatalf("trailer experience=%d, want %d", experience, test.want)
			}
		})
	}
}

// currentExpertJobProjectionMode reads the op205 mode block exactly the way the
// compatibility DLL does: eight bytes before the end of the body, so a test can
// prove what the client will actually decide without duplicating the offsets.
func currentExpertJobProjectionMode(t *testing.T, body []byte) uint32 {
	t.Helper()
	if len(body) < 16 {
		t.Fatalf("expert job projection body too short: %x", body)
	}
	block := body[len(body)-16 : len(body)-8]
	if magic := binary.LittleEndian.Uint32(block[:4]); magic != currentExpertJobProjectionModeMagic {
		t.Fatalf("expert job projection mode magic=%08x body=%x", magic, body)
	}
	return binary.LittleEndian.Uint32(block[4:])
}

// currentExpertJobClearedBody is the exact type-zero op205 body the client
// receives: the two native bytes, the mode block, then the experience pair. The
// cleared form is always silent, because it restates an absent profession
// instead of announcing an acquisition.
func currentExpertJobClearedBody(giveUpState byte) []byte {
	return []byte{
		giveUpState, 0,
		0x45, 0x4a, 0x58, 0x4d, byte(currentExpertJobProjectionSilent), 0, 0, 0,
		0x45, 0x4a, 0x58, 0x50, 0, 0, 0, 0,
	}
}

func TestCurrentExpertJobTypeZeroClearsClientProjection(t *testing.T) {
	connection := &bufferConn{}
	service := &Service{options: options{gameUpperBodyCodec: gameUpperBodyCodecPlain}}
	session := &gameSession{conn: connection, selectedCharacterID: 9}
	character := dnfrepo.CharacterRecord{Stats: map[string]int64{"expert_job_type": 0}}

	if err := service.sendCurrentExpertJobInfoForCharacter(session, character, false); err != nil {
		t.Fatal(err)
	}
	packet, trailing := splitCurrentGameServerUpperPacketAuto(t, connection.write.Bytes())
	if len(trailing) != 0 || packet.Header.Classification != 0 ||
		packet.Header.MsgID != currentExpertJobInfoNotification {
		t.Fatalf("clear packet header=%+v trailing=%d", packet.Header, len(trailing))
	}
	if len(packet.Body) != 18 || packet.Body[0] != 0 || packet.Body[1] != 0 ||
		binary.LittleEndian.Uint32(packet.Body[2:6]) != currentExpertJobProjectionModeMagic ||
		binary.LittleEndian.Uint32(packet.Body[6:10]) != currentExpertJobProjectionSilent ||
		binary.LittleEndian.Uint32(packet.Body[10:14]) != currentExpertJobProjectionTrailerMagic ||
		binary.LittleEndian.Uint32(packet.Body[14:18]) != 0 {
		t.Fatalf("clear op205 body=%x", packet.Body)
	}
	if session.expertJobInfoCharacterID != session.selectedCharacterID {
		t.Fatalf("projected character=%d, want %d", session.expertJobInfoCharacterID, session.selectedCharacterID)
	}

	before := connection.write.Len()
	if err := service.sendCurrentExpertJobInfoForCharacter(session, character, false); err != nil {
		t.Fatal(err)
	}
	if connection.write.Len() != before {
		t.Fatalf("duplicate type-zero projection wrote %d bytes", connection.write.Len()-before)
	}
}

func TestCurrentExpertJobInvalidTypeDoesNotProject(t *testing.T) {
	connection := &bufferConn{}
	service := &Service{options: options{gameUpperBodyCodec: gameUpperBodyCodecPlain}}
	session := &gameSession{conn: connection, selectedCharacterID: 9}
	character := dnfrepo.CharacterRecord{Stats: map[string]int64{"expert_job_type": 5}}

	if err := service.sendCurrentExpertJobInfoForCharacter(session, character, false); err != nil {
		t.Fatal(err)
	}
	if connection.write.Len() != 0 || session.expertJobInfoCharacterID != 0 {
		t.Fatalf("invalid type projected bytes=%d character=%d", connection.write.Len(), session.expertJobInfoCharacterID)
	}
}

func TestCurrentExpertJobActiveProjectionUsesPersistedGiveUpState(t *testing.T) {
	tests := []struct {
		name  string
		state int64
		want  byte
	}{
		{name: "never_given_up", state: 0, want: 0},
		{name: "given_up_once", state: 1, want: 1},
		{name: "clamped", state: int64(math.MaxUint8) + 1, want: math.MaxUint8},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			stats := map[string]int64{currentExpertJobGiveUpStateStat: test.state}
			if got := currentExpertJobGiveUpState(stats); got != test.want {
				t.Fatalf("active op205 persisted state=%d, want %d", got, test.want)
			}
		})
	}
}
