package dnfbridge

import (
	"context"
	"encoding/binary"
	"testing"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

// This file pins the class0/op2 mode3 body - the packet the client's native
// character-information command is answered with, and therefore the body behind
// the personal-information panel - against the client's own read plan, byte for
// byte.
//
// The plan was recovered instruction by instruction from NoPack.exe with
// tools\ghidra_scripts\GBModeChain.java. op2's handler sub_200BEA0 reads
// {u8 mode, u16 count, then per entry u8 route + u8 owner context} and calls
// sub_2008600 once per entry for mode 3. sub_2008600 then consumes, in order:
//
//	8 bytes   raw context (u32 adventure manage level, u16, u8, u8)
//	u16       object key
//	u32       cumulative EXP                    (sub_2004AE0)
//	u32 len + len bytes  stat blob              (sub_3457C50)
//	u8        extra-equipment-slot bitset       -> actor.vfunc_0xA98()+0x1AC
//	u8 count + rows  worn objects               (sub_1D77560)
//	u32       clone-title state                 (sub_1D77560)
//	8 bytes   equipped creature {u32, u32}       (sub_20077D0)
//	u8 + 2*(u8 count + count*(u16,u8))  skills  (sub_2003420)
//	u8        creature level                    (-> actor.vfunc_0xB54()+0x648)
//	then the fixed 39-byte trailer below.
//
// The trailer is the part that silently rots: every reader in it is gated on a
// count or flag byte that is zero for an ordinary character, so none of them
// notices a surplus or missing byte - the body simply shifts and the panel
// renders another field's value.
const (
	// Bytes sub_2008600 consumes after the creature-level byte.
	clientMode3TrailerSize = 39
	// Offset inside the trailer of the u8 that sub_2005520 stores at actor
	// +0x34C, the field the personal-information panel renders as the
	// expert/sub-profession row. It is also body[len-8].
	clientMode3TrailerExpertJobOffset = 31
	// Offsets of the pair sub_2005520 stores at actor +0x35C (u16) and +0x360
	// (u32). The S4A12 reference server labels the same pair "personal
	// power-war point" and "guild power-war point" (GuildPowerWar).
	clientMode3TrailerPowerWarU16Offset = 21
	clientMode3TrailerPowerWarU32Offset = 23
	// Offsets of the length-prefixed string sub_2005520 -> sub_3457D20 ->
	// sub_3457C80 reads between that pair and the expert-job byte: one u32
	// length, then that many bytes (bounded by 0x20).
	clientMode3TrailerStringLengthOffset = 27
)

func clientMode3FixtureCharacter(expertJobType int64, pvpGrade, pvpRankPoint int64) dnfrepo.CharacterRecord {
	return dnfrepo.CharacterRecord{
		CharacterID: "19",
		AccountID:   "dnf:1",
		Name:        "hero",
		Job:         "11",
		Level:       90,
		Stats: map[string]int64{
			"exp":             12345,
			"expert_job_type": expertJobType,
			"pvp_grade":       pvpGrade,
			"pvp_rank_point":  pvpRankPoint,
		},
	}
}

// clientMode3TrailerStart walks a mode3 body exactly the way sub_200BEA0 /
// sub_2008600 do and returns the offset where the fixed trailer begins. It
// fails the test if any field it steps over is not the shape the client
// demands, so a builder change that moves the trailer cannot pass silently.
func clientMode3TrailerStart(t *testing.T, body []byte) int {
	t.Helper()
	pos := 0
	need := func(n int, what string) {
		t.Helper()
		if pos+n > len(body) {
			t.Fatalf("mode3 body truncated reading %s at %d (len=%d)", what, pos, len(body))
		}
	}

	need(1, "mode")
	if body[pos] != 3 {
		t.Fatalf("mode3 first byte = %d, want 3", body[pos])
	}
	pos++
	need(2, "entry count")
	if got := binary.LittleEndian.Uint16(body[pos:]); got != 1 {
		t.Fatalf("mode3 entry count = %d, want 1", got)
	}
	pos += 2
	pos += 2 // per-entry route byte + owner-context byte
	pos += 8 // raw context: u32 adventure manage level, u16, u8, u8
	pos += 2 // object key
	need(4, "cumulative EXP")
	pos += 4
	need(4, "stat blob length")
	statLen := int(binary.LittleEndian.Uint32(body[pos:]))
	pos += 4
	if statLen != currentMode1StatBlobWireSize {
		t.Fatalf("mode3 stat blob length = %d, want %d", statLen, currentMode1StatBlobWireSize)
	}
	need(statLen, "stat blob")
	pos += statLen
	need(1, "extra equipment slot state")
	pos++
	need(1, "worn object create count")
	createCount := int(body[pos])
	pos++
	// This fixture has no equipment repository, so sub_1D77560 must be handed an
	// empty create list. If that ever changes, the trailer offsets below need a
	// row-size walk rather than this fixed step.
	if createCount != 0 {
		t.Fatalf("mode3 create count = %d, want 0 for the repository-less fixture", createCount)
	}
	need(4, "clone-title state")
	pos += 4
	need(8, "equipped creature")
	pos += 8
	need(1, "selected skill tree")
	pos++
	for tree := 0; tree < currentSkillInfoTreeCount; tree++ {
		need(1, "skill page count")
		rows := int(body[pos])
		pos++
		need(rows*3, "skill page rows")
		pos += rows * 3
	}
	need(1, "creature level")
	pos++
	return pos
}

func TestCurrentMode3TrailerMatchesClientReadPlan(t *testing.T) {
	const (
		expertJobType = 1
		pvpGrade      = 3
		pvpRankPoint  = 0x11223344
	)
	character := clientMode3FixtureCharacter(expertJobType, pvpGrade, pvpRankPoint)
	service := Service{characterStats: testCharacterStatTable(t)}

	body := service.buildCurrentSelectedUserInfoMode3Body(
		context.Background(), nil, nil, character, true, 19)

	trailerStart := clientMode3TrailerStart(t, body)
	if trailerStart+clientMode3TrailerSize != len(body) {
		t.Fatalf("mode3 body length %d != trailer start %d + client trailer %d "+
			"(surplus or missing bytes in the trailer); body=%x",
			len(body), trailerStart, clientMode3TrailerSize, body)
	}
	trailer := body[trailerStart:]

	// sub_2005520 reads u16, u32, u8, u8 here and stores them at actor +0x35C,
	// +0x360, +0x34C and the internal branch flag. The u32 immediately before
	// them is consumed by sub_2008600 itself as a cursor-only field.
	if got := binary.LittleEndian.Uint32(trailer[:4]); got != 0 {
		t.Fatalf("trailer cursor u32 = %#x, want 0", got)
	}
	if got := binary.LittleEndian.Uint16(trailer[clientMode3TrailerPowerWarU16Offset:]); got != pvpGrade {
		t.Fatalf("trailer actor+0x35C u16 = %d, want pvp_grade %d", got, pvpGrade)
	}
	if got := binary.LittleEndian.Uint32(trailer[clientMode3TrailerPowerWarU32Offset:]); got != pvpRankPoint {
		t.Fatalf("trailer actor+0x360 u32 = %#x, want %#x", got, pvpRankPoint)
	}
	// sub_2005520 -> sub_3457D20 -> sub_3457C80: one u32 length followed by that
	// many bytes. Dropping these four bytes is what made the peer mode-3 body
	// crash the client, so pin the length byte-for-byte and pin the fact that no
	// bytes follow it (a nonzero length would move the expert-job slot below).
	if got := binary.LittleEndian.Uint32(trailer[clientMode3TrailerStringLengthOffset:]); got != 0 {
		t.Fatalf("trailer sub_3457C80 string length = %d, want 0 (trailer=%x)", got, trailer)
	}
	// The expert/sub-profession byte. This is the field the personal-information
	// panel renders; when the builder dropped the string's length field, every
	// reader in the trailer started four bytes early and this slot held the
	// following byte instead.
	if got := trailer[clientMode3TrailerExpertJobOffset]; got != expertJobType {
		t.Fatalf("trailer actor+0x34C expert-job byte = %d, want %d (trailer=%x)",
			got, expertJobType, trailer)
	}
	if got := trailer[clientMode3TrailerExpertJobOffset+1]; got != 0 {
		t.Fatalf("trailer sub_2005520 branch flag = %d, want 0", got)
	}
	// sub_2002DC0 reads a u32 row count before its rows, sub_2006BB0 and
	// sub_20070C0 each read a u8 gate that a zero byte ends. Those three values
	// are the last 6 bytes of the body, so a reader that steps over them would
	// walk off the end of the packet.
	if got := binary.LittleEndian.Uint32(trailer[clientMode3TrailerSize-6:]); got != 0 {
		t.Fatalf("trailer sub_2002DC0 row count = %d, want 0", got)
	}
	if trailer[clientMode3TrailerSize-2] != 0 || trailer[clientMode3TrailerSize-1] != 0 {
		t.Fatalf("trailer sub_2006BB0/sub_20070C0 gates = %d/%d, want 0/0",
			trailer[clientMode3TrailerSize-2], trailer[clientMode3TrailerSize-1])
	}
	// Everything the client does not read here must stay zero: the three u32s
	// and four u8s of sub_2006940 plus its empty list count.
	for index, value := range trailer[:clientMode3TrailerPowerWarU16Offset-4] {
		if value != 0 {
			t.Fatalf("trailer byte %d = %#x, want 0 (trailer=%x)", index, value, trailer)
		}
	}
}

// The expert-job byte must be reachable as body[len-8] with nothing between it
// and the end of the body: sub_2005520's u8 pair, sub_2002DC0's u32 count and
// the two trailing gates are exactly the last seven bytes.
func TestCurrentMode3ExpertJobByteIsThePanelsLastField(t *testing.T) {
	service := Service{characterStats: testCharacterStatTable(t)}

	for _, expertJobType := range []int64{1, 2, 3, 4} {
		body := service.buildCurrentSelectedUserInfoMode3Body(
			context.Background(), nil, nil,
			clientMode3FixtureCharacter(expertJobType, 0, 0), true, 19)
		if got := body[len(body)-8]; got != byte(expertJobType) {
			t.Fatalf("expert_job_type=%d: body[len-8] = %d (body=%x)",
				expertJobType, got, body)
		}
	}
}

// Changing expert_job_type must move exactly one byte - the actor +0x34C slot -
// and must not change the body length. A reintroduced string field in front of
// it would move that byte and grow the body, which is precisely the defect this
// file guards.
func TestCurrentMode3ExpertJobByteIsTheOnlyByteItMoves(t *testing.T) {
	service := Service{characterStats: testCharacterStatTable(t)}

	withType1 := service.buildCurrentSelectedUserInfoMode3Body(
		context.Background(), nil, nil, clientMode3FixtureCharacter(1, 3, 0x11223344), true, 19)
	withType4 := service.buildCurrentSelectedUserInfoMode3Body(
		context.Background(), nil, nil, clientMode3FixtureCharacter(4, 3, 0x11223344), true, 19)

	if len(withType1) != len(withType4) {
		t.Fatalf("mode3 length changed with expert_job_type: %d vs %d",
			len(withType1), len(withType4))
	}
	differing := make([]int, 0, 2)
	for index := range withType1 {
		if withType1[index] != withType4[index] {
			differing = append(differing, index)
		}
	}
	want := len(withType1) - 8
	if len(differing) != 1 || differing[0] != want {
		t.Fatalf("expert_job_type moved bytes %v, want exactly [%d] (len=%d)",
			differing, want, len(withType1))
	}
}

// mode1 travels beside mode3 on the same opcode and carries no panel trailer.
// Its tail is reader-gated the same way, so pin its length too: the fix above
// must not leak into the actor-state body.
func TestCurrentMode1BodyIsUnaffectedByTheMode3TrailerFix(t *testing.T) {
	service := Service{characterStats: testCharacterStatTable(t)}
	character := clientMode3FixtureCharacter(1, 3, 0x11223344)

	mode1 := service.buildCurrentSelectedUserInfoMode1Body(
		context.Background(), nil, nil, character, true, 19)

	pos := 0
	pos++    // mode
	pos += 2 // entry count
	pos += 2 // route + owner context
	pos += 16
	pos += 2 // object key
	pos += 4 // cumulative EXP
	statLen := int(binary.LittleEndian.Uint32(mode1[pos:]))
	pos += 4
	if statLen != currentMode1StatBlobWireSize {
		t.Fatalf("mode1 stat blob length = %d, want %d", statLen, currentMode1StatBlobWireSize)
	}
	pos += statLen
	pos++    // extra equipment slot state
	pos++    // create count (fixture has no equipment)
	pos += 4 // clone-title state
	pos += 2 // sub_2007B00 object key
	pos++    // sub_2007B00 deferred row count
	pos += 8 // equipped creature {u32, u32}
	pos++    // selected skill tree
	for tree := 0; tree < currentSkillInfoTreeCount; tree++ {
		rows := int(mode1[pos])
		pos++
		pos += rows * 3
	}
	pos++ // creature level
	// mode1's own fixed trailer, read by sub_2002BE0 (u8 list gate + 3 u8),
	// sub_2002CC0 (u8 gate), sub_2002DC0 (u32 row count), sub_20025B0 (u32),
	// sub_20070C0 (u8 gate) and the final grief-entry byte.
	const currentMode1TrailerSize = 19
	if pos+currentMode1TrailerSize != len(mode1) {
		t.Fatalf("mode1 tail start %d + %d != body length %d (mode1=%x)",
			pos, currentMode1TrailerSize, len(mode1), mode1)
	}
	if mode1[len(mode1)-1] != 0 {
		t.Fatalf("mode1 grief-entry byte = %d, want 0 (mode1=%x)", mode1[len(mode1)-1], mode1)
	}
}
