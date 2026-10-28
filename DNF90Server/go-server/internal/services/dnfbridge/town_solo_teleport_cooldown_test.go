package dnfbridge

import (
	"bytes"
	"context"
	"encoding/hex"
	"testing"
	"time"

	dnfenum "longheng.io/server/internal/modules/dnf/dnfenum"
)

// TestBuildCurrentSoloTeleportCooldownBodyPinsTheSoloEntriesAndSeconds pins the
// S2C 378 body the personal teleport publishes.
//
// The client's reader (VA 0x01D55AA0) is `u8 count`, then per entry `u8 index` +
// `u32 seconds`, and it applies StartTimer only for index 0 and for index 2/3.
// The personal teleport owns 2 and 3 (the town-menu label branch turns exactly
// that pair into 13250 "个人传送"); index 0 must never be armed here because it is
// the party chain's 600 s entry.
func TestBuildCurrentSoloTeleportCooldownBodyPinsTheSoloEntriesAndSeconds(t *testing.T) {
	if currentSoloTeleportCooldownOpcode != 378 {
		t.Fatalf("personal teleport cooldown opcode = %d, want 378",
			currentSoloTeleportCooldownOpcode)
	}
	if currentSoloTeleportCooldownSeconds != 300 {
		t.Fatalf("personal teleport cooldown seconds = %d, want 300",
			currentSoloTeleportCooldownSeconds)
	}
	indices := currentSoloTeleportCooldownDefaultIndices
	if !bytes.Equal(indices, []byte{2, 3}) {
		t.Fatalf("personal teleport cooldown indices = %v, want [2 3]", indices)
	}
	for _, index := range indices {
		if index == 0 {
			t.Fatalf("personal teleport must not arm the party teleport entry 0")
		}
	}
	want := []byte{2, 2, 0x2c, 0x01, 0x00, 0x00, 3, 0x2c, 0x01, 0x00, 0x00}
	if got := buildCurrentSoloTeleportCooldownBody(indices, currentSoloTeleportCooldownSeconds); !bytes.Equal(got, want) {
		t.Fatalf("solo teleport cooldown body = %x, want %x", got, want)
	}
	if got := buildCurrentSoloTeleportCooldownBody(nil, currentSoloTeleportCooldownSeconds); !bytes.Equal(got, []byte{0}) {
		t.Fatalf("empty solo teleport cooldown body = %x, want 00", got)
	}
}

// TestCurrentSoloTeleportCooldownIndicesRejectsTheEntryTheClientDrops keeps the
// override honest: the client's reader silently drops index 1, so honouring it
// here would publish an entry that can never arm anything.
func TestCurrentSoloTeleportCooldownIndicesRejectsTheEntryTheClientDrops(t *testing.T) {
	tests := []struct {
		raw  string
		want []byte
	}{
		{raw: "", want: []byte{2, 3}},
		{raw: "off", want: nil},
		{raw: "NONE", want: nil},
		{raw: "2", want: []byte{2}},
		{raw: "3", want: []byte{3}},
		{raw: "2,3", want: []byte{2, 3}},
		{raw: "3,2,3", want: []byte{3, 2}},
		{raw: "1", want: []byte{}},
		{raw: "0,1,2,3", want: []byte{0, 2, 3}},
		{raw: "9,abc", want: []byte{}},
	}
	for _, test := range tests {
		t.Setenv(currentSoloTeleportCooldownIndicesEnv, test.raw)
		got := currentSoloTeleportCooldownIndices()
		if len(got) != len(test.want) {
			t.Fatalf("indices for %q = %v, want %v", test.raw, got, test.want)
		}
		for i := range got {
			if got[i] != test.want[i] {
				t.Fatalf("indices for %q = %v, want %v", test.raw, got, test.want)
			}
		}
	}
}

// TestHandleCurrentSoloTeleportArmsTheFiveMinuteCooldown covers the whole fixed
// chain: a committed personal teleport arms the server-side lock and publishes
// the client's countdown, and the next request is refused through op470 instead
// of moving the character again.
func TestHandleCurrentSoloTeleportArmsTheFiveMinuteCooldown(t *testing.T) {
	service, session, repositories := newTownMoveTest(t)
	first, err := hex.DecodeString("ffffffffffffffff26008403fa0005")
	if err != nil {
		t.Fatal(err)
	}
	if err := service.handleCurrentSoloTeleport(session, first); err != nil {
		t.Fatal(err)
	}
	remaining, cooling := service.currentSoloTeleportCooldownRemaining(29)
	if !cooling {
		t.Fatalf("committed personal teleport did not arm the cooldown")
	}
	if remaining > 300*time.Second || remaining < 299*time.Second {
		t.Fatalf("cooldown remaining = %s, want ~300s", remaining)
	}

	// The transition itself is the op24 owner's, so the countdown is the packet
	// that follows it.
	conn := session.conn.(*bufferConn)
	transition, rest := splitTownTransitionAndPostState(
		t,
		conn.write.Bytes(),
		session,
		29,
		38,
		0,
		900,
		250,
		5,
		3,
		townMoveSkillProjectionBody(t, repositories, "29"),
		false,
	)
	if transition.Header.MsgID != currentSceneTransitionMsgID {
		t.Fatalf("solo teleport transition msg = %d, want %d",
			transition.Header.MsgID, currentSceneTransitionMsgID)
	}
	cooldown, trailing := splitCurrentGameServerUpperPacketAuto(t, rest)
	if cooldown.Header.MsgID != currentSoloTeleportCooldownOpcode ||
		cooldown.Header.Classification != 0 {
		t.Fatalf("solo teleport cooldown header=%+v body=%x",
			cooldown.Header, cooldown.Body)
	}
	wantBody := buildCurrentSoloTeleportCooldownBody(
		currentSoloTeleportCooldownDefaultIndices, currentSoloTeleportCooldownSeconds)
	if !bytes.Equal(cooldown.Body, wantBody) {
		t.Fatalf("solo teleport cooldown body = %x, want %x", cooldown.Body, wantBody)
	}
	if len(trailing) != 0 {
		t.Fatalf("solo teleport cooldown trailing=%x", trailing)
	}

	// A second request inside the window must not move the character and must
	// answer through the opcode's own failure callback.
	conn.write.Reset()
	second, err := hex.DecodeString("ffffffffffffffff27008403fa0005")
	if err != nil {
		t.Fatal(err)
	}
	if err := service.handleCurrentSoloTeleport(session, second); err != nil {
		t.Fatal(err)
	}
	refusal, refusalRest := splitCurrentGameServerUpperPacketAuto(t, conn.write.Bytes())
	if refusal.Header.MsgID != uint16(dnfenum.CmdPacketSoloTelepoart) ||
		refusal.Header.Classification != 0 {
		t.Fatalf("cooldown refusal header=%+v body=%x", refusal.Header, refusal.Body)
	}
	if !bytes.Equal(refusal.Body, []byte{0}) {
		t.Fatalf("cooldown refusal body = %x, want 00", refusal.Body)
	}
	if len(refusalRest) != 0 {
		t.Fatalf("cooldown refusal emitted extra packet %x", refusalRest)
	}
	stored, found, loadErr := repositories.Character.Load(context.Background(), "29")
	if loadErr != nil || !found {
		t.Fatalf("cooldown refusal character found=%t err=%v", found, loadErr)
	}
	if stored.Stats["town_id"] != 38 || stored.Stats["area_id"] != 0 {
		t.Fatalf("cooldown refusal still moved the character to %+v", stored.Stats)
	}
}

// TestCurrentSoloTeleportCooldownIsPerCharacterAndExpires pins the two
// properties the client's own timer has: it belongs to the character (not the
// account) and it is a real 300-second window.
func TestCurrentSoloTeleportCooldownIsPerCharacterAndExpires(t *testing.T) {
	service, _, _ := newTownMoveTest(t)
	base := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	service.soloTeleportClock = func() time.Time { return base }
	until := service.armCurrentSoloTeleportCooldown(29)
	if want := base.Add(300 * time.Second); !until.Equal(want) {
		t.Fatalf("cooldown deadline = %s, want %s", until, want)
	}
	if remaining, cooling := service.currentSoloTeleportCooldownRemaining(29); !cooling || remaining != 300*time.Second {
		t.Fatalf("character 29 cooldown remaining=%s cooling=%t", remaining, cooling)
	}
	if _, cooling := service.currentSoloTeleportCooldownRemaining(30); cooling {
		t.Fatalf("character 30 inherited character 29's cooldown")
	}

	service.soloTeleportClock = func() time.Time { return base.Add(299 * time.Second) }
	if _, cooling := service.currentSoloTeleportCooldownRemaining(29); !cooling {
		t.Fatalf("cooldown expired one second early")
	}
	service.soloTeleportClock = func() time.Time { return base.Add(300 * time.Second) }
	if _, cooling := service.currentSoloTeleportCooldownRemaining(29); cooling {
		t.Fatalf("cooldown still running at its own deadline")
	}
	service.soloTeleportClock = func() time.Time { return base }
	if until := service.armCurrentSoloTeleportCooldown(29); !until.Equal(base.Add(300 * time.Second)) {
		t.Fatalf("re-armed cooldown deadline = %s", until)
	}
	service.clearCurrentSoloTeleportCooldown(29)
	if _, cooling := service.currentSoloTeleportCooldownRemaining(29); cooling {
		t.Fatalf("cleared cooldown still running")
	}
}

// TestHandleCurrentSoloTeleportDoesNotArmCooldownForRefusedDestination keeps the
// lock off a teleport that never happened: the shared op36 owner reports its
// refusals as a nil error, so the arm must be driven by the committed location.
func TestHandleCurrentSoloTeleportDoesNotArmCooldownForRefusedDestination(t *testing.T) {
	service, session, repositories := newTownMoveTest(t)
	// Town 40 / area 0 carries a `[need quest] 3156` requirement in the fixture
	// PVF and the character has completed no quests, so the shared owner refuses
	// the destination.
	body, err := hex.DecodeString("ffffffffffffffff28008403fa0005")
	if err != nil {
		t.Fatal(err)
	}
	if err := service.handleCurrentSoloTeleport(session, body); err != nil {
		t.Fatal(err)
	}
	if _, cooling := service.currentSoloTeleportCooldownRemaining(29); cooling {
		t.Fatalf("refused destination armed the cooldown")
	}
	if got := session.conn.(*bufferConn).write.Bytes(); bytes.Contains(got, []byte{0x7a, 0x01}) {
		t.Fatalf("refused destination published a cooldown packet: %x", got)
	}
	stored, found, loadErr := repositories.Character.Load(context.Background(), "29")
	if loadErr != nil || !found {
		t.Fatalf("refused destination character found=%t err=%v", found, loadErr)
	}
	if stored.Stats["town_id"] != 38 || stored.Stats["area_id"] != 1 {
		t.Fatalf("refused destination moved the character to %+v", stored.Stats)
	}
}
