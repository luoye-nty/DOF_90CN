package raid

import (
	"errors"
	"fmt"
	"math"
)

// LukeCardDisplayMsgID is the tarot display notification the client's
// CardSetWindow is driven by: opcode 1181 (0x49D), LUKE_RAID_CARD_DISPLAY.
//
// It is a server push. The name table lists no C2S counterpart -- every
// LUKE_RAID_* packet is an ENUM_NOTIPACKET_* -- and the handler at 0x010CDB90
// is registered by 0x010CDF50, so nothing the client sends can produce one.
const LukeCardDisplayMsgID uint16 = 1181

// The body is a single record with no outer count:
//
//	u8  cardSetIndex     index of the day's [CARD SET MAP INFO] record; the
//	                     client only checks it against the record count and
//	                     drops the WHOLE packet when it is out of range
//	u8  N                number of card records (one per phase-one sanctum = 3)
//	N x {
//	      u16 A          sanctum dungeon id (5054 / 5055 / 5056)
//	      u8  B          that sanctum's [PATTERN] index in LukeCardSet.etc
//	      u8  K          card slots of that pattern = its [CARD POS] pair count
//	      K x {
//	            u32 P    the tarot card of this slot: a [CARD] monster id
//	            u8  Q    read but never consumed by this client build; must be
//	                     sent, or every following field shifts
//	          }
//	    }
//	u8  openWindowFlag   non-zero pops the CardSetWindow on its own
//
// Field widths are pinned by the handler's read helpers: 1 byte 0x34E3970,
// 2 bytes 0x34E39E0, 4 bytes 0x34E3A50 (tooling: tools\_rev_卢克塔罗牌_1181规格_20260929.md
// §1.3). There are no coordinates in the body -- the client takes those from
// `[CARD POS]` in LukeCardSet.etc itself.
const (
	// lukeCardDisplayMaxRecords bounds N: it is a u8 on the wire.
	lukeCardDisplayMaxRecords = 255
	// lukeCardDisplaySlotPadding is the Q byte of every slot. This client build
	// reads it into [ebp-0x12] and never looks at it again; it is emitted as a
	// fixed zero rather than guessed at.
	lukeCardDisplaySlotPadding byte = 0
)

var ErrLukeCardDisplayShapeInvalid = errors.New("luke card display shape is invalid")

// LukeCardDisplaySanctum is one card record: which sanctum, which of its
// patterns, and the tarot card sitting in each of that pattern's slots.
type LukeCardDisplaySanctum struct {
	// DungeonID is field A. It must be one of the sanctum ids the 1181 handler
	// can resolve in LukeCardSet.etc, or the window draws nothing for it.
	DungeonID uint32
	// Pattern is field B: the index half of that sanctum's `[PATTERN]` key.
	Pattern int
	// Cards is the P field of every slot, in slot order. Its length is K, and it
	// must equal the pattern's `[CARD POS]` pair count: the client walks the slot
	// coordinates and the P list together, so a short list reads past the end.
	Cards []uint32
}

// LukeCardDisplay is one 1181 body before encoding.
type LukeCardDisplay struct {
	// CardSetIndex is the day's `[CARD SET MAP INFO]` record index.
	CardSetIndex byte
	// CardSetCount is how many such records the client holds, i.e. the exclusive
	// upper bound the client validates CardSetIndex against. It comes from
	// luke.etc's `[CARD SET MAP INFO]` (5 records = 15 values / 3) and the
	// builder refuses to emit a body the client would drop whole.
	CardSetCount int
	// Sanctums is N records, one per phase-one sanctum, in wire order.
	Sanctums []LukeCardDisplaySanctum
	// OpenWindow asks the client to open the CardSetWindow itself.
	OpenWindow bool
}

// LukeCardDisplayBodySize is the exact body length of that shape, so a caller
// can size a buffer without encoding it first.
func LukeCardDisplayBodySize(sanctums []LukeCardDisplaySanctum) int {
	size := 2 + 1 // cardSetIndex, N, openWindowFlag
	for _, sanctum := range sanctums {
		size += 4 + 5*len(sanctum.Cards) // A, B, K, then K x (u32 P, u8 Q)
	}
	return size
}

// BuildLukeCardDisplayBody encodes one 1181 body.
//
// It refuses anything the client would silently mis-draw instead of sending a
// half-described packet: an out-of-range cardSetIndex (the client drops the
// whole packet), a non-positive N or K, a dungeon id that does not fit its u16
// field, a zero card id (the client reads a missing key as an empty slot, so a
// zero is always a caller bug), or a card count above the u8 field width.
func BuildLukeCardDisplayBody(display LukeCardDisplay) ([]byte, error) {
	if display.CardSetCount <= 0 {
		return nil, fmt.Errorf("%w: card_set_count=%d want_positive", ErrLukeCardDisplayShapeInvalid, display.CardSetCount)
	}
	if int(display.CardSetIndex) >= display.CardSetCount {
		return nil, fmt.Errorf("%w: card_set_index=%d card_set_count=%d",
			ErrLukeCardDisplayShapeInvalid, display.CardSetIndex, display.CardSetCount)
	}
	if len(display.Sanctums) == 0 || len(display.Sanctums) > lukeCardDisplayMaxRecords {
		return nil, fmt.Errorf("%w: records=%d want=1..%d",
			ErrLukeCardDisplayShapeInvalid, len(display.Sanctums), lukeCardDisplayMaxRecords)
	}
	for index, sanctum := range display.Sanctums {
		if sanctum.DungeonID == 0 || sanctum.DungeonID > math.MaxUint16 {
			return nil, fmt.Errorf("%w: record=%d dungeon=%d want=1..%d",
				ErrLukeCardDisplayShapeInvalid, index, sanctum.DungeonID, math.MaxUint16)
		}
		if sanctum.Pattern < 0 || sanctum.Pattern > math.MaxUint8 {
			return nil, fmt.Errorf("%w: record=%d dungeon=%d pattern=%d want=0..%d",
				ErrLukeCardDisplayShapeInvalid, index, sanctum.DungeonID, sanctum.Pattern, math.MaxUint8)
		}
		if len(sanctum.Cards) == 0 || len(sanctum.Cards) > lukeCardDisplayMaxRecords {
			return nil, fmt.Errorf("%w: record=%d dungeon=%d slots=%d want=1..%d",
				ErrLukeCardDisplayShapeInvalid, index, sanctum.DungeonID, len(sanctum.Cards), lukeCardDisplayMaxRecords)
		}
		for slot, cardID := range sanctum.Cards {
			if cardID == 0 {
				return nil, fmt.Errorf("%w: record=%d dungeon=%d slot=%d card=0",
					ErrLukeCardDisplayShapeInvalid, index, sanctum.DungeonID, slot)
			}
		}
	}

	writer := packetWriter{buf: make([]byte, 0, LukeCardDisplayBodySize(display.Sanctums))}
	writer.writeByte(display.CardSetIndex)
	writer.writeByte(byte(len(display.Sanctums)))
	for _, sanctum := range display.Sanctums {
		writer.writeUint16(uint16(sanctum.DungeonID))
		writer.writeByte(byte(sanctum.Pattern))
		writer.writeByte(byte(len(sanctum.Cards)))
		for _, cardID := range sanctum.Cards {
			writer.writeUint32(cardID)
			writer.writeByte(lukeCardDisplaySlotPadding)
		}
	}
	var openWindow byte
	if display.OpenWindow {
		openWindow = 1
	}
	writer.writeByte(openWindow)
	return writer.bytes(), nil
}
