package dnfbridge

import (
	"context"
	"encoding/binary"
	"strconv"
	"strings"

	"golang.org/x/text/encoding/simplifiedchinese"
)

const (
	// currentCharacterNameChangeLogMode is the GET_USERINFO mode behind the
	// client's 角色名称变更记录 (character name-change record) window. The
	// request shares the role-selector opcode and carries the native three-byte
	// shape: u16 target character id + u8 mode.
	currentCharacterNameChangeLogMode = byte(5)

	// currentCharacterNameChangeLogMsgID is the class-0 notification that owns
	// that window.
	//
	// Evidence for the current 90CN client (sha256
	// 8A7AF614769D0B47D0647B77C33BCE725F2F20258651C01FC6C22AC1DB44AA4D): the
	// class-0 registry dump lists `opcode=733 handler=0x015E7D60`, and that
	// handler (a) resolves the window object, (b) empties its row list and
	// zeroes the row count, (c) reads one u8 row count from the packet and
	// returns immediately when it is zero, (d) otherwise reads two fields per
	// row and appends the row. This is the same contract as the 86-generation
	// reference notification (CHARAC_NAME_CHANGE_LOG = 0x02DD = 733).
	currentCharacterNameChangeLogMsgID = uint16(733)

	// currentCharacterNameChangeLogNameMaxBytes is the client's own per-row name
	// limit. The third-cell reader (0x3457C80) rejects a length of 31 or more
	// (`cmp eax, esi / jae` against maxLen 0x1F) and falls back to its built-in
	// empty string, so a row may carry at most 30 name bytes.
	currentCharacterNameChangeLogNameMaxBytes = 30
)

// buildCurrentCharacterNameChangeLogBody encodes the window's row list.
//
// Row layout, re-derived from the opcode-733 handler at 0x015E7D60 together with
// its third-cell reader 0x3457C80 (2026-09-23):
//
//	u8  rowCount
//	per row (9 + nameLen bytes):
//	  u32 nameLen    -> 0x3457C80 compares it against maxLen 0x1F: a length of 31
//	                    or more, or exactly 0, makes the client fall back to its
//	                    built-in string and consume no further bytes.
//	  u8  name[...]  -> nameLen bytes of GBK text read by 0x34E3AC0 and converted
//	                    to UTF-16 for the 角色名 column.
//	  u8  serverId   -> 0x2272670(serverId) resolves the 大区名 column.
//	  u32 unixTime   -> localtime() -> (year + 1900, month + 1, day) 日期 column.
//
// The v1 of this file wrote a four-byte "key" where nameLen now sits and shipped
// the names in a trailing block read only by the standalone RenameLogNames.dll.
// Disassembling 0x3457C80 settled that question: it is not a table lookup at all
// but a length-prefixed string reader, so the name belongs in the row itself and
// no client patch is needed.
//
// field order is fixed by the handler's instruction sequence, which reads the
// third cell first, then the two following fields:
//
//	0x015E7E25  call 0x3457D20   ; name  (u32 length + bytes, via 0x3457C80)
//	0x015E7E33  call 0x34E3970   ; serverId (u8)
//	0x015E7E3F  call 0x34E3A50   ; unixTime (u32)
//
// The row stride was proven against five rows of a captured window: with a zero
// name length the server id sits at 5+9k and the timestamp at 6+9k, reproducing
// 山东6 / 未定名62 / 未定名53 / 奇迹暖暖 with dates 1971.01.28, 1971.04.15 and
// 2026.09.23 exactly. A five-byte stride (emitted until that revision) shifts
// every field by four bytes and produced the "correct 大区名 but 1970 date"
// reports.
func buildCurrentCharacterNameChangeLogBody(entries []currentCharacterRenameLogEntry, fallbackServerID byte) []byte {
	count := len(entries)
	if count > 0xff {
		count = 0xff
	}
	var writer packetWriter
	writer.writeByte(byte(count))
	for idx := 0; idx < count; idx++ {
		serverID := entries[idx].ServerID
		if !entries[idx].HasServerID {
			serverID = fallbackServerID
		}
		name := currentCharacterNameChangeLogNameBytes(entries[idx].OldName)
		writer.writeUint32(uint32(len(name)))
		writer.writeBytes(name)
		writer.writeByte(serverID)
		writer.writeUint32(uint32(entries[idx].Unix))
	}
	return writer.bytes()
}

// currentCharacterNameChangeLogNameBytes encodes one record's previous name.
//
// The client converts these bytes from GBK, so the value is capped at
// currentCharacterNameChangeLogNameMaxBytes on a character boundary rather than a
// byte boundary: slicing mid-character would leave a dangling lead byte that the
// conversion turns into a stray glyph.
func currentCharacterNameChangeLogNameBytes(name string) []byte {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil
	}
	encoder := simplifiedchinese.GB18030.NewEncoder()
	var encoded []byte
	for _, value := range name {
		unit, err := encoder.Bytes([]byte(string(value)))
		if err != nil || len(unit) == 0 {
			unit = []byte(string(value))
		}
		if len(encoded)+len(unit) > currentCharacterNameChangeLogNameMaxBytes {
			break
		}
		encoded = append(encoded, unit...)
	}
	return encoded
}

// handleCurrentCharacterNameChangeLogRequest answers the name-change-record
// window query.
//
// This branch exists because the request reuses the role-selector opcode: the
// name-change window emits GET_USERINFO with mode=5, and the role-selector
// bootstrap answered every GET_USERINFO with the account's full character
// roster. The client dispatches an incoming roster to the role-selector reader
// (sub_200BEA0), which is only valid on the character-select screen, so opening
// that window while a character was already in a scene fed a 33-row roster into
// the selector reader and hard-crashed the client (live crash 2026-09-23
// 00:13:23, immediately after the 4200-byte roster reply).
//
// Only the exact mode-5 shape is consumed here; mode 2 (own roster) and mode 3
// (town character inspection) keep their existing handlers untouched.
func (s *Service) handleCurrentCharacterNameChangeLogRequest(session *gameSession, body []byte) (bool, error) {
	if len(body) < 3 || body[2] != currentCharacterNameChangeLogMode {
		return false, nil
	}
	targetID := binary.LittleEndian.Uint16(body[:2])
	regionID := s.currentRegionID()
	entries := s.loadCurrentCharacterRenameLog(session, targetID)
	payload := buildCurrentCharacterNameChangeLogBody(entries, regionID)
	s.logGameEvent(session, "game-character-name-change-log-send",
		"target_char_id", targetID,
		"msg_id", currentCharacterNameChangeLogMsgID,
		"entries", len(entries),
		"region_id", regionID,
		"body_len", len(body),
		"payload_len", len(payload))
	return true, s.sendGameUpperRawClass(session, currentCharacterNameChangeLogMsgID, payload, 0)
}

// loadCurrentCharacterRenameLog loads one character's committed renames.
//
// Any requester may read them. The window is opened for a *target* character -
// the mode-5 request carries that character's id - and a player inspecting
// somebody else has to see that character's record too; until 2026-09-23 this
// returned nil unless the target belonged to the requesting account, so the
// window came up empty for every other player. The target still has to exist;
// an unknown id answers with the empty list, which keeps the window well formed.
//
// The history itself stays stored on the owner's account record (see
// appendCurrentCharacterRenameLog), so serving a foreign character means loading
// that character's owner and reading the same "<serverID>|<old name>" entries.
func (s *Service) loadCurrentCharacterRenameLog(session *gameSession, targetID uint16) []currentCharacterRenameLogEntry {
	if s == nil || session == nil || targetID == 0 {
		return nil
	}
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.Account == nil || repositories.Character == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()

	characterID := strconv.FormatUint(uint64(targetID), 10)
	character, found, err := repositories.Character.Load(ctx, characterID)
	if err != nil || !found {
		return nil
	}
	ownerID := strings.TrimSpace(character.AccountID)
	if ownerID == "" {
		return nil
	}
	account, found, err := repositories.Account.Load(ctx, ownerID)
	if err != nil || !found {
		return nil
	}
	return currentCharacterRenameLogEntries(account, character.CharacterID)
}
