package raid

import (
	"encoding/binary"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
)

// lukeCardDisplayTestSanctums is the shipped phase-one shape: three sanctums,
// each with its own pattern index and seven card slots.
func lukeCardDisplayTestSanctums() []LukeCardDisplaySanctum {
	return []LukeCardDisplaySanctum{
		{
			DungeonID: 5054,
			Pattern:   1,
			Cards:     []uint32{59171, 59098, 59873, 59096, 56065, 59874, 56699},
		},
		{
			DungeonID: 5055,
			Pattern:   2,
			Cards:     []uint32{59278, 56166, 56673, 59875, 56066, 56674, 56067},
		},
		{
			DungeonID: 5056,
			Pattern:   0,
			Cards:     []uint32{56068, 56063, 56700, 57156, 59872, 59099, 59095},
		},
	}
}

// TestBuildLukeCardDisplayBodyMatchesTheReverseEngineeredWireBytes is the
// byte-exact pin for opcode 1181. Every expected byte below is written out by
// hand from the spec's field table (tools\_rev_卢克塔罗牌_1181规格_20260929.md
// §一) rather than re-encoded by the builder:
//
//	u8  cardSetIndex = 3
//	u8  N            = 3
//	per record: u16 A (little endian), u8 B, u8 K, then K x (u32 P little
//	            endian, u8 Q = 0)
//	u8  openWindowFlag = 1
//
// 5054 = 0x13BE, 5055 = 0x13BF, 5056 = 0x13C0; the card ids are the 22
// `[CARD]` monster ids, 59171 = 0x0000E723 and so on.
func TestBuildLukeCardDisplayBodyMatchesTheReverseEngineeredWireBytes(t *testing.T) {
	body, err := BuildLukeCardDisplayBody(LukeCardDisplay{
		CardSetIndex: 3,
		CardSetCount: 5,
		Sanctums:     lukeCardDisplayTestSanctums(),
		OpenWindow:   true,
	})
	if err != nil {
		t.Fatalf("BuildLukeCardDisplayBody error = %v", err)
	}
	const wantHex = "" +
		// cardSetIndex, N
		"0303" +
		// 5054, pattern 1, K=7, then the seven cards each followed by Q=0
		"be13" + "01" + "07" +
		"23e70000" + "00" +
		"dae60000" + "00" +
		"e1e90000" + "00" +
		"d8e60000" + "00" +
		"01db0000" + "00" +
		"e2e90000" + "00" +
		"7bdd0000" + "00" +
		// 5055, pattern 2, K=7
		"bf13" + "02" + "07" +
		"8ee70000" + "00" +
		"66db0000" + "00" +
		"61dd0000" + "00" +
		"e3e90000" + "00" +
		"02db0000" + "00" +
		"62dd0000" + "00" +
		"03db0000" + "00" +
		// 5056, pattern 0, K=7
		"c013" + "00" + "07" +
		"04db0000" + "00" +
		"ffda0000" + "00" +
		"7cdd0000" + "00" +
		"44df0000" + "00" +
		"e0e90000" + "00" +
		"dbe60000" + "00" +
		"d7e60000" + "00" +
		// openWindowFlag
		"01"
	want, err := hex.DecodeString(wantHex)
	if err != nil {
		t.Fatalf("decode want hex: %v", err)
	}
	if len(body) != len(want) {
		t.Fatalf("body_len=%d want=%d", len(body), len(want))
	}
	if got := hex.EncodeToString(body); got != wantHex {
		t.Fatalf("body=%s\nwant=%s", got, wantHex)
	}
	if size := LukeCardDisplayBodySize(lukeCardDisplayTestSanctums()); size != len(want) {
		t.Fatalf("LukeCardDisplayBodySize=%d want=%d", size, len(want))
	}

	// Re-read the body field by field, in the client's own order, so a change in
	// any field width fails here with the offset rather than only as a hex diff.
	if body[0] != 3 {
		t.Fatalf("cardSetIndex=%d want 3", body[0])
	}
	if body[1] != 3 {
		t.Fatalf("N=%d want 3", body[1])
	}
	offset := 2
	for index, want := range lukeCardDisplayTestSanctums() {
		if got := binary.LittleEndian.Uint16(body[offset : offset+2]); uint32(got) != want.DungeonID {
			t.Fatalf("record %d A@%d=%d want %d", index, offset, got, want.DungeonID)
		}
		offset += 2
		if int(body[offset]) != want.Pattern {
			t.Fatalf("record %d B@%d=%d want %d", index, offset, body[offset], want.Pattern)
		}
		offset++
		if int(body[offset]) != 7 {
			t.Fatalf("record %d K@%d=%d want 7", index, offset, body[offset])
		}
		offset++
		for slot, cardID := range want.Cards {
			if got := binary.LittleEndian.Uint32(body[offset : offset+4]); got != cardID {
				t.Fatalf("record %d slot %d P@%d=%d want %d", index, slot, offset, got, cardID)
			}
			offset += 4
			if body[offset] != 0 {
				t.Fatalf("record %d slot %d Q@%d=%d want 0", index, slot, offset, body[offset])
			}
			offset++
		}
	}
	if offset != len(body)-1 {
		t.Fatalf("openWindowFlag offset=%d want=%d", offset, len(body)-1)
	}
	if body[len(body)-1] != 1 {
		t.Fatalf("openWindowFlag=%d want 1", body[len(body)-1])
	}
}

// TestBuildLukeCardDisplayBodyKeepsOpenWindowOffWhenNotAsked pins the trailing
// byte's other value: only the waiting-area push sets it, later re-reads do not.
func TestBuildLukeCardDisplayBodyKeepsOpenWindowOffWhenNotAsked(t *testing.T) {
	body, err := BuildLukeCardDisplayBody(LukeCardDisplay{
		CardSetIndex: 0,
		CardSetCount: 5,
		Sanctums:     []LukeCardDisplaySanctum{{DungeonID: 5054, Pattern: 0, Cards: []uint32{59171}}},
	})
	if err != nil {
		t.Fatalf("BuildLukeCardDisplayBody error = %v", err)
	}
	want := []byte{0x00, 0x01, 0xbe, 0x13, 0x00, 0x01, 0x23, 0xe7, 0x00, 0x00, 0x00, 0x00}
	if got := hex.EncodeToString(body); got != hex.EncodeToString(want) {
		t.Fatalf("body=%s want=%s", got, hex.EncodeToString(want))
	}
}

// TestBuildLukeCardDisplayBodyRefusesBodiesTheClientWouldDrop covers the
// out-of-range card set index (the handler throws the WHOLE packet away) and the
// shapes that would make the client read past the end of a record.
func TestBuildLukeCardDisplayBodyRefusesBodiesTheClientWouldDrop(t *testing.T) {
	base := func() LukeCardDisplay {
		return LukeCardDisplay{
			CardSetIndex: 4,
			CardSetCount: 5,
			Sanctums:     lukeCardDisplayTestSanctums(),
			OpenWindow:   true,
		}
	}
	cases := []struct {
		name   string
		mutate func(*LukeCardDisplay)
	}{
		{"card set index at the bound", func(d *LukeCardDisplay) { d.CardSetIndex = 5 }},
		{"unknown card set count", func(d *LukeCardDisplay) { d.CardSetCount = 0 }},
		{"no records", func(d *LukeCardDisplay) { d.Sanctums = nil }},
		{"zero dungeon id", func(d *LukeCardDisplay) { d.Sanctums[0].DungeonID = 0 }},
		{"negative pattern", func(d *LukeCardDisplay) { d.Sanctums[0].Pattern = -1 }},
		{"pattern above the u8", func(d *LukeCardDisplay) { d.Sanctums[0].Pattern = 256 }},
		{"no slots", func(d *LukeCardDisplay) { d.Sanctums[0].Cards = nil }},
		{"zero card id", func(d *LukeCardDisplay) { d.Sanctums[0].Cards[3] = 0 }},
		{"too many records", func(d *LukeCardDisplay) {
			records := make([]LukeCardDisplaySanctum, 256)
			for index := range records {
				records[index] = LukeCardDisplaySanctum{DungeonID: 5054, Cards: []uint32{59171}}
			}
			d.Sanctums = records
		}},
	}
	for _, testCase := range cases {
		display := base()
		testCase.mutate(&display)
		if _, err := BuildLukeCardDisplayBody(display); !errors.Is(err, ErrLukeCardDisplayShapeInvalid) {
			t.Fatalf("%s: err=%v want %v", testCase.name, err, ErrLukeCardDisplayShapeInvalid)
		}
	}
	if _, err := BuildLukeCardDisplayBody(base()); err != nil {
		t.Fatalf("the unmutated shape must still encode: %v", err)
	}
	if LukeCardDisplayMsgID != 1181 {
		t.Fatalf("LukeCardDisplayMsgID=%d want 1181", LukeCardDisplayMsgID)
	}
	if strings.TrimSpace(LukeCardSetPVFPath) != "etc/raid/luke/LukeCardSet.etc" {
		t.Fatalf("LukeCardSetPVFPath=%q", LukeCardSetPVFPath)
	}
}
