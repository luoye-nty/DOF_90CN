package dnfbridge

import (
	"context"
	"encoding/binary"
	"testing"
	"time"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

// TestCurrentCharacterOptionSnapshotReplaysOncePerActor pins the replay that
// makes a saved skill-cooldown reminder survive a relog.
//
// The client's op390 handler sub_1F9E520 stores the 512-byte table at
// manager+0x288 and calls sub_1F9D8D0 to rebuild the live reminder list, but
// sub_1F9D8D0 returns immediately while [0x52B5BB0] - the local actor - is
// null. The scene-init send lands before the actor exists, so the table was
// stored and the reminder stayed off until the player set it again. One
// replay after mode0/mode1 fixes that, and it must happen exactly once per
// character so an ordinary scene change does not re-send it every time.
func TestCurrentCharacterOptionSnapshotReplaysOncePerActor(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "7", AccountID: "account-1", Name: "Fighter",
	}); err != nil {
		t.Fatal(err)
	}
	payload := make([]byte, dnfrepo.CharacterOptionSnapshotSize)
	for slot := range payload {
		payload[slot] = 0xff
	}
	// Slots 2 and 3 of the u16 table are cooldown-reminder skill ids.
	binary.LittleEndian.PutUint16(payload[4:6], 64)
	binary.LittleEndian.PutUint16(payload[6:8], 76)
	if err := dnfrepo.SaveCharacterOptionSnapshot(ctx, repositories.Settings, "7", payload, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if got := currentCharacterOptionCooldownSkills(payload); len(got) != 2 || got[0] != 64 || got[1] != 76 {
		t.Fatalf("probe payload cooldown skills=%v", got)
	}

	connection := &bufferConn{}
	service := &Service{
		options: options{
			accountID: "account-1", gameUpperHeader: gameUpperHeaderChannel13,
			gameUpperBodyCodec: gameUpperBodyCodecPlain,
		},
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
	}
	session := &gameSession{conn: connection, selectedCharacterID: 7, accountID: "account-1"}

	if err := service.sendCurrentCharacterOptionSnapshotForActor(session, "test_after_mode1_local_actor"); err != nil {
		t.Fatal(err)
	}
	packet, rest := splitGameServerUpperPacket(t, connection.write.Bytes())
	if len(rest) != 0 || packet.Header.MsgID != currentCharacterOptionRestoreMsgID {
		t.Fatalf("replay header=%+v trailing=%d", packet.Header, len(rest))
	}
	if len(packet.Body) != currentSaveCharacterOptionBodySize ||
		binary.LittleEndian.Uint32(packet.Body[:currentCharacterOptionLengthBytes]) != currentCharacterOptionPayloadBytes {
		t.Fatalf("replay body_len=%d", len(packet.Body))
	}
	if got := currentCharacterOptionCooldownSkills(packet.Body[currentCharacterOptionLengthBytes:]); len(got) != 2 || got[0] != 64 || got[1] != 76 {
		t.Fatalf("replayed cooldown skills=%v, want [64 76]", got)
	}

	// A second call for the same character is suppressed: the actor already
	// consumed the table, and every later scene change would otherwise resend
	// a 516-byte body.
	connection.write.Reset()
	if err := service.sendCurrentCharacterOptionSnapshotForActor(session, "test_second_scene"); err != nil {
		t.Fatal(err)
	}
	if connection.write.Len() != 0 {
		t.Fatalf("second replay wrote %d bytes", connection.write.Len())
	}

	// Switching characters arms it again.
	clearCurrentCharacterOptionRestoreSnapshot(session)
	if session.characterOptionActorApplyCharacterID != 0 {
		t.Fatalf("character switch left the replay marker at %d", session.characterOptionActorApplyCharacterID)
	}
}
