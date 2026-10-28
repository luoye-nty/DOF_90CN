package dnfbridge

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"
	"unicode/utf16"
)

func TestBuildCurrentClientNoticeBodyMatchesTheDLLReader(t *testing.T) {
	const color = uint32(0xFF33CCFF)
	body, err := buildCurrentClientNoticeBody("转职提示", color)
	if err != nil {
		t.Fatal(err)
	}
	if body[0] != currentClientNoticeType || body[1] != currentClientNoticeVersion {
		t.Fatalf("client notice discriminator=%x", body[0:2])
	}
	// Types 1, 2 and 3 on this opcode belong to the combat-power sidecar, the
	// mood projection and the boss-mutation decision. Each of those consumers
	// claims a packet on a type match alone, so the notice type must never
	// collide with one of them.
	for _, taken := range []byte{
		currentCombatPowerAffixType,
		currentMoodProjectionType,
		currentDungeonBossMutationResponseType,
	} {
		if currentClientNoticeType == taken {
			t.Fatalf("client notice type %d collides with an existing op413 consumer", taken)
		}
	}
	units := utf16.Encode([]rune("转职提示"))
	if got := binary.LittleEndian.Uint16(body[2:4]); got != uint16(len(units)) {
		t.Fatalf("client notice unit count=%d want %d", got, len(units))
	}
	if got := binary.LittleEndian.Uint32(body[4:8]); got != color {
		t.Fatalf("client notice color=%08x want %08x", got, color)
	}
	want := make([]byte, len(units)*2)
	for index, unit := range units {
		binary.LittleEndian.PutUint16(want[index*2:index*2+2], unit)
	}
	if !bytes.Equal(body[currentClientNoticeHeaderBytes:], want) {
		t.Fatalf("client notice text=%x want=%x", body[currentClientNoticeHeaderBytes:], want)
	}
	if len(body) != currentClientNoticeHeaderBytes+len(units)*2 {
		t.Fatalf("client notice body length=%d", len(body))
	}
}

func TestBuildCurrentClientNoticeBodyCountsUTF16UnitsNotRunes(t *testing.T) {
	// A supplementary-plane rune is one rune but two UTF-16 units, and the DLL
	// copies units, so the count on the wire has to be the unit count.
	body, err := buildCurrentClientNoticeBody("𠮷", currentClientNoticeDefaultColor)
	if err != nil {
		t.Fatal(err)
	}
	if got := binary.LittleEndian.Uint16(body[2:4]); got != 2 {
		t.Fatalf("surrogate pair unit count=%d want 2", got)
	}
	if len(body) != currentClientNoticeHeaderBytes+4 {
		t.Fatalf("surrogate pair body length=%d", len(body))
	}
}

func TestBuildCurrentClientNoticeBodyRejectsOutOfBoundInput(t *testing.T) {
	if _, err := buildCurrentClientNoticeBody("", currentClientNoticeDefaultColor); err == nil {
		t.Fatal("empty client notice text was accepted")
	}
	if _, err := buildCurrentClientNoticeBody("a\x00b", currentClientNoticeDefaultColor); err == nil {
		t.Fatal("client notice text with an embedded NUL was accepted")
	}
	if _, err := buildCurrentClientNoticeBody(
		strings.Repeat("提", currentClientNoticeMaxCharacters),
		currentClientNoticeDefaultColor,
	); err != nil {
		t.Fatalf("client notice text at the exact bound was rejected: %v", err)
	}
	if _, err := buildCurrentClientNoticeBody(
		strings.Repeat("提", currentClientNoticeMaxCharacters+1),
		currentClientNoticeDefaultColor,
	); err == nil {
		t.Fatal("client notice text above the DLL bound was accepted")
	}
}

func TestSendCurrentClientNoticeWritesTheOp413PrivateEnvelope(t *testing.T) {
	service, session, connection := newNoticeRoutingTest()
	const color = uint32(0xFFE6B84A)
	if err := service.sendCurrentClientNotice(session, "转职提示", color, "unit_test"); err != nil {
		t.Fatal(err)
	}
	packet, trailing := splitGameServerUpperPacket(t, connection.write.Bytes())
	if packet.Header.MsgID != currentClientNoticeMsgID ||
		packet.Header.Classification != 0 || len(trailing) != 0 {
		t.Fatalf("client notice header=%+v trailing=%x", packet.Header, trailing)
	}
	want, err := buildCurrentClientNoticeBody("转职提示", color)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(packet.Body, want) {
		t.Fatalf("client notice body=%x want=%x", packet.Body, want)
	}
}

func TestSendCurrentClientNoticeDropsUnencodableTextWithoutWriting(t *testing.T) {
	service, session, connection := newNoticeRoutingTest()
	if err := service.sendCurrentClientNotice(
		session, "", currentClientNoticeDefaultColor, "unit_test",
	); err != nil {
		t.Fatal(err)
	}
	if got := connection.write.Bytes(); len(got) != 0 {
		t.Fatalf("unencodable client notice wrote=%x", got)
	}
}

// Every op413 consumer in 90CN.dll reads the same two discriminator bytes at
// the same offset, so the four projections that share the opcode must stay
// distinguishable by that pair alone: the notice consumer accepts a variable
// length, and the other three are exact-length gated but run first.
func TestOp413PrivateDiscriminatorsStayUnique(t *testing.T) {
	seen := map[[2]byte]string{}
	for name, pair := range map[string][2]byte{
		"combat_power_affix": {currentCombatPowerAffixType, currentCombatPowerAffixVersion},
		"mood_projection":    {currentMoodProjectionType, currentMoodProjectionVersion},
		"boss_mutation": {
			currentDungeonBossMutationResponseType,
			currentDungeonBossMutationResponseVersion,
		},
		"client_notice": {currentClientNoticeType, currentClientNoticeVersion},
	} {
		if other, clash := seen[pair]; clash {
			t.Fatalf("op413 type/version %v is claimed by both %s and %s", pair, other, name)
		}
		seen[pair] = name
	}
}
