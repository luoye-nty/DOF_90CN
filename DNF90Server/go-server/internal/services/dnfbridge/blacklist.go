package dnfbridge

import (
	"context"
	"encoding/binary"
	"errors"
	"sort"
	"strconv"
	"strings"
	"time"

	dnfenum "longheng.io/server/internal/modules/dnf/dnfenum"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

// Per-character blacklist.
//
// The current client owns a blacklist of its own and *does* report it: legacy
// `type=118` (ENUM_CMDPACKET_REGISITER_TO_BLACKLIST) and `type=119`
// (ENUM_CMDPACKET_DELETE_TO_BLACKLIST), each with the body
// `u32 nameLength | name` -- captured 2026-09-22 14:35,
// `017600150000005e2563b92f00 04000000 74657374` ("test").
//
// This server used to answer those two with a generic ack and store nothing, and
// it answered `type=120` (REQUEST_BLACKLIST) with a fixed *empty* list. The client
// replaces its list with whatever 120 returns, so every relog wiped the player's
// blacklist -- live report 2026-09-22 22:4x, "拉黑后小退就不在黑名单列表了".
//
// ---------------------------------------------------------------------------
// The type=120 answer layout (reverse engineered 2026-09-22, client RVA 0x1910480)
// ---------------------------------------------------------------------------
// `type=120` is answered on the *same* class-1 opcode, and the body is
//
//	u8 flag | u8 count | count * ( u32 nameLength | name | u8 dateA | u8 dateB | u8 dateC )
//
// Evidence:
//   - The client's reader for opcode 120 is sub_1910480 (registered from the same
//     contiguous 118/119/120 blacklist block at RVA 0x19369D6..0x19369F6). It first
//     tests its second argument -- the packet's *first* body byte, consumed before
//     the payload -- and returns immediately when that flag is zero, then reads
//     `u8 count` and loops `count` times, each iteration reading one DSTR
//     (readDstrTLS, max 0x1f) followed by exactly three readU8 calls.
//   - The client stores those three bytes into one row record at +0x14, +0x10 and
//     +0x0C; the official server's own builder (df_game_r(1), ELF with symbols:
//     Inter_Monitor_RequestBlackListResult::dispatch_sig) reads a 0x28-byte entry
//     struct and emits them in exactly that order as three put_byte calls after
//     `put_int(nameLength)` + `put_str(name)` -- so the wire order is dateA/dateB/dateC
//     as stored at +0x14/+0x10/+0x0C.
//   - That builder also writes `put_header(class 1, cmd)` then `put_byte(1)` before
//     the count, i.e. the same leading flag byte, and aborts when the count exceeds
//     0x0a. The flag byte is consumed as the reader's second argument, which is why
//     the 118/119 ack (`u8 flag | u32 nameLength | name`) already works today: the
//     name is read from the second body byte on.
//
// The three trailing bytes are the 黑名单日期 column (the panel's other column,
// 在线状态, is resolved locally from the actor lookup, not from the packet). They are
// written as year%100 / month / day, so a 2026-09-22 block sends 0x1A, 0x09, 0x16.
//
// The list is per character (not per account), stored through the existing
// settings repository under `character:<id>:blacklist_v1`, the same mechanism the
// project already uses for per-character counters (`:dungeon_admission_counters_v1`)
// -- no new file, no new table.
var errCurrentBlacklistStoreUnavailable = errors.New("blacklist settings repository unavailable")

const (
	currentBlacklistScopeSuffix = ":blacklist_v1"
	currentBlacklistValueKey    = "names"
	currentBlacklistMaxNames    = 100

	// currentBlacklistValueSeparator splits one stored `name<sep>unix-seconds` line.
	// A roster name can never contain a tab, and lines without the separator are the
	// date-less form written before the block date was tracked, so old saves keep
	// loading unchanged.
	currentBlacklistValueSeparator = "\t"

	// currentBlacklistListAnswerOk is the leading flag the client's op120 reader
	// consumes before the payload; zero makes it return without touching its list.
	currentBlacklistListAnswerOk byte = 1

	// currentBlacklistListAnswerMaxEntries bounds the payload count, which is a u8 on
	// the wire (the store's own cap is currentBlacklistMaxNames).
	currentBlacklistListAnswerMaxEntries = 0xff
)

// currentBlacklistRejection* are op65's `status` reason codes. The client's handler
// branches on the *mode* byte before it reaches the chat-window manager, and the two
// reachable tables hold different sentences (client-patch\bin\DNF.exe):
//
//	mode 0x24 -- the 1:1 peer-menu window, which is the mode this client actually
//	sends for window traffic (live capture 2026-09-22 23:25:
//	`...0024 03 00000000 00000002 ...`): the mapper at RVA 0x2DA5950 renders
//	    status 0x36 -> dstr 4771 已将对方添加到黑名单，无法继续进行。
//	    status 0x4D -> dstr 2079 对方在黑名单中
//	    status 0x5A -> dstr 2080 无法向GM发送聊天信息
//	    otherwise   -> dstr 2078 对方目前处于无法接收信息状态
//	mode 0x01 (whisper) -- the direct branch at RVA 0x1969CF5 renders
//	    status 0x36 -> dstr 4771
//	    status 0x4D -> dstr 4763 您在对方的黑名单内，无法进行该操作。
//	    status 0x5A -> dstr 6424
//
// Operator decision 2026-09-22 23:4x: keep the peer-menu sentences. Routing the
// peer-blocked notice through the whisper table to reach dstr 4763 was tried and
// rolled back -- the sentence arrives as a detached system line instead of in the
// window, which is not what the window is for.
const (
	currentBlacklistStatusTargetBlockedSender byte = 0x4D // 「对方在黑名单中」
	currentBlacklistStatusSenderBlockedTarget byte = 0x36 // 「已将对方添加到黑名单，无法继续进行。」
)

// currentBlacklistRecord is one stored block: the blocked character name plus the
// time it was added (unix seconds, zero when the save predates date tracking).
type currentBlacklistRecord struct {
	Name string
	At   int64
}

func currentBlacklistScope(characterID uint16) string {
	return "character:" + strconv.Itoa(int(characterID)) + currentBlacklistScopeSuffix
}

// parseCurrentBlacklistValue reads the stored value into records, accepting both
// `name` and `name<tab>unix-seconds` lines so pre-existing saves keep working.
func parseCurrentBlacklistValue(raw string) []currentBlacklistRecord {
	records := make([]currentBlacklistRecord, 0, 8)
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		name, stamp := line, ""
		if index := strings.Index(line, currentBlacklistValueSeparator); index >= 0 {
			name = strings.TrimSpace(line[:index])
			stamp = strings.TrimSpace(line[index+len(currentBlacklistValueSeparator):])
		}
		if name == "" {
			continue
		}
		at, err := strconv.ParseInt(stamp, 10, 64)
		if err != nil || at < 0 {
			at = 0
		}
		records = append(records, currentBlacklistRecord{Name: name, At: at})
	}
	return records
}

// formatCurrentBlacklistValue writes records back in the same line format.
func formatCurrentBlacklistValue(records []currentBlacklistRecord) string {
	lines := make([]string, 0, len(records))
	for _, record := range records {
		if record.At > 0 {
			lines = append(lines, record.Name+currentBlacklistValueSeparator+strconv.FormatInt(record.At, 10))
			continue
		}
		lines = append(lines, record.Name)
	}
	return strings.Join(lines, "\n")
}

func currentBlacklistRecordsSorted(records []currentBlacklistRecord) []currentBlacklistRecord {
	sorted := make([]currentBlacklistRecord, len(records))
	copy(sorted, records)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
	return sorted
}

func currentBlacklistRecordNames(records []currentBlacklistRecord) []string {
	names := make([]string, 0, len(records))
	for _, record := range records {
		names = append(names, record.Name)
	}
	return names
}

func currentBlacklistRecordsContain(records []currentBlacklistRecord, name string) bool {
	for _, record := range records {
		if strings.EqualFold(record.Name, name) {
			return true
		}
	}
	return false
}

// loadCurrentBlacklistRecords returns the stored blocks of one character, in stored order.
func (s *Service) loadCurrentBlacklistRecords(ctx context.Context, characterID uint16) []currentBlacklistRecord {
	if s == nil || characterID == 0 {
		return nil
	}
	repos, ok := s.repositoryGroup()
	if !ok || repos.Settings == nil {
		return nil
	}
	record, found, err := repos.Settings.Load(ctx, currentBlacklistScope(characterID))
	if err != nil || !found {
		return nil
	}
	return parseCurrentBlacklistValue(record.Values[currentBlacklistValueKey])
}

// loadCurrentBlacklist returns the stored names of one character, in stored order.
func (s *Service) loadCurrentBlacklist(ctx context.Context, characterID uint16) []string {
	return currentBlacklistRecordNames(s.loadCurrentBlacklistRecords(ctx, characterID))
}

func (s *Service) saveCurrentBlacklistRecords(ctx context.Context, characterID uint16, records []currentBlacklistRecord) error {
	repos, ok := s.repositoryGroup()
	if !ok || repos.Settings == nil {
		return errCurrentBlacklistStoreUnavailable
	}
	sorted := currentBlacklistRecordsSorted(records)
	record := dnfrepo.SettingsRecord{
		Scope:     currentBlacklistScope(characterID),
		Values:    map[string]string{currentBlacklistValueKey: formatCurrentBlacklistValue(sorted)},
		UpdatedAt: s.gameplayNow(),
	}
	return repos.Settings.Save(ctx, record)
}

// loadCurrentBlacklistRecordsForSession is loadCurrentBlacklistRecords with its own
// short context, for callers that have none at hand (the type=120 answer path).
func (s *Service) loadCurrentBlacklistRecordsForSession(session *gameSession) []currentBlacklistRecord {
	if session == nil || session.selectedCharacterID == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	return s.loadCurrentBlacklistRecords(ctx, session.selectedCharacterID)
}

// currentBlacklistHas reports whether `owner` has blocked `name` (case-insensitive,
// like the rest of the chat name matching).
func (s *Service) currentBlacklistHas(ctx context.Context, owner uint16, name string) bool {
	name = strings.TrimSpace(name)
	if name == "" {
		return false
	}
	for _, blocked := range s.loadCurrentBlacklist(ctx, owner) {
		if strings.EqualFold(blocked, name) {
			return true
		}
	}
	return false
}

// parseCurrentBlacklistName reads `u32 nameLength | name`; the caps mirror the
// client's own roster-name limit and reject a NUL inside the name.
func parseCurrentBlacklistName(body []byte) (string, bool) {
	if len(body) < 4 {
		return "", false
	}
	length := int(binary.LittleEndian.Uint32(body[:4]))
	if length <= 0 || length > 30 || len(body) != 4+length {
		return "", false
	}
	raw := body[4:]
	if strings.IndexByte(string(raw), 0) >= 0 {
		return "", false
	}
	// The client sends the name in the wire's GB18030 form -- a live 1:1 capture
	// carries `c0 b4 c0 b4 c0 b4` for 来来来. Decoding it here keeps the stored name
	// equal to the repository's own spelling, so the 1:1 gate's name comparison
	// matches and rosterNameBytes re-encodes back to the same bytes for the ack.
	if name, err := decodeCharacterName(raw); err == nil {
		return name, true
	}
	return string(raw), true
}

// currentBlacklistCanStore reports whether the register/delete path can actually
// persist for this session: a selected character plus a settings repository. The
// upper/class-1 transport uses it to decide between the storing owner and the
// historic ack-only echo.
func (s *Service) currentBlacklistCanStore(session *gameSession) bool {
	if s == nil || session == nil || session.selectedCharacterID == 0 {
		return false
	}
	repos, ok := s.repositoryGroup()
	return ok && repos.Settings != nil
}

// handleCurrentBlacklistRegister answers legacy 118, handleCurrentBlacklistDelete
// legacy 119. Both reply with the shape the client already got from this server:
// class 1, `flag(1) | u32 nameLength | name` (9 bytes for a 4-character name,
// captured live), so the visible protocol is unchanged for a successful call.
func (s *Service) handleCurrentBlacklistRegister(session *gameSession, body []byte) error {
	return s.mutateCurrentBlacklist(session, body, true, uint16(dnfenum.CmdPacketRegisiterToBlacklist))
}

func (s *Service) handleCurrentBlacklistDelete(session *gameSession, body []byte) error {
	return s.mutateCurrentBlacklist(session, body, false, uint16(dnfenum.CmdPacketDeleteToBlacklist))
}

func (s *Service) mutateCurrentBlacklist(session *gameSession, body []byte, add bool, opcode uint16) error {
	if session == nil || session.selectedCharacterID == 0 {
		return nil
	}
	name, ok := parseCurrentBlacklistName(body)
	if !ok {
		s.logGameEvent(session, "game-blacklist-request-rejected",
			"char_id", session.selectedCharacterID, "opcode", opcode, "body_len", len(body),
			"reason", "malformed_name_argument")
		return s.sendGameUpperFailure(session, opcode, currentTownChatGenericError)
	}
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	records := s.loadCurrentBlacklistRecords(ctx, session.selectedCharacterID)
	next := make([]currentBlacklistRecord, 0, len(records)+1)
	changed := false
	alreadyBlocked := false
	for _, existing := range records {
		if strings.EqualFold(existing.Name, name) {
			alreadyBlocked = true
			if add {
				next = append(next, existing) // already blocked: keep, no duplicate
			} else {
				changed = true
			}
			continue
		}
		next = append(next, existing)
	}
	if add && !alreadyBlocked {
		if len(next) >= currentBlacklistMaxNames {
			s.logGameEvent(session, "game-blacklist-request-rejected",
				"char_id", session.selectedCharacterID, "opcode", opcode,
				"reason", "blacklist_full", "limit", currentBlacklistMaxNames)
			return s.sendGameUpperFailure(session, opcode, currentTownChatGenericError)
		}
		next = append(next, currentBlacklistRecord{Name: name, At: s.gameplayNow().Unix()})
		changed = true
	}
	if changed {
		if err := s.saveCurrentBlacklistRecords(ctx, session.selectedCharacterID, next); err != nil {
			s.logGameEvent(session, "game-blacklist-save-failed",
				"char_id", session.selectedCharacterID, "opcode", opcode, "error", err)
			return s.sendGameUpperFailure(session, opcode, currentTownChatGenericError)
		}
	}
	s.logGameEvent(session, "game-blacklist-updated",
		"char_id", session.selectedCharacterID, "opcode", opcode, "blocked", add,
		"name", name, "count", len(next), "changed", changed)
	// FIXED 2026-09-22 23:2x: this used to write `u32 len(name)` *and then* a full
	// DSTR (which writes its own u32 length), i.e. 13 bytes for a 4-character name
	// instead of the 9 the shape calls for. The client consumed the first u32 as the
	// name length and copied the second one as the name, so the row it appended
	// showed a garbled name with Lv.0. The official server's own builder
	// (df_game_r(1) 0x84D0F99: put_byte(1) ; put_int(len) ; put_str(name,len)) proves
	// the ack is `flag(1) | u32 nameLength | name` -- exactly the 9-byte shape this
	// path was documented as sending, so the redundant length is dropped here.
	var writer packetWriter
	writer.writeRawDstr(rosterNameBytes(name))
	return s.sendGameUpperSuccess(session, opcode, writer.bytes())
}

// currentBlacklistDateFields projects one stored block time onto the answer's three
// trailing bytes: year%100, month, day. A zero timestamp (a save from before the
// date was tracked) yields three zero bytes, which the panel reads as no date.
func currentBlacklistDateFields(at int64) (byte, byte, byte) {
	if at <= 0 {
		return 0, 0, 0
	}
	stamp := time.Unix(at, 0)
	return byte(stamp.Year() % 100), byte(int(stamp.Month())), byte(stamp.Day())
}

// buildCurrentBlacklistListBody is the type=120 answer:
//
//	u8 flag(1) | u8 count | count * (u32 nameLength | name | u8 year | u8 month | u8 day)
//
// An empty list answers with currentRequestBlacklistResponseBody verbatim (`00 00`),
// which is byte for byte what this server sent before the layout was known: flag 0
// makes the client's reader return before it touches its list.
func buildCurrentBlacklistListBody(records []currentBlacklistRecord) []byte {
	if len(records) == 0 {
		return currentRequestBlacklistResponseBody
	}
	count := len(records)
	if count > currentBlacklistListAnswerMaxEntries {
		count = currentBlacklistListAnswerMaxEntries
	}
	var writer packetWriter
	writer.writeByte(currentBlacklistListAnswerOk)
	writer.writeByte(byte(count))
	for _, record := range records[:count] {
		raw := rosterNameBytes(record.Name)
		writer.writeUint32(uint32(len(raw)))
		writer.writeBytes(raw)
		year, month, day := currentBlacklistDateFields(record.At)
		writer.writeByte(year)
		writer.writeByte(month)
		writer.writeByte(day)
	}
	return writer.bytes()
}

// sendCurrentChatRejectionNotice shows the "it did not go through" sentence to the
// *sender* whose line the blacklist gate refused. The client's op65 handler
// implements the rejected branch for exactly two modes -- the 1:1 peer-menu window
// (0x24, sentences 4771/2079/2080/2078) and a typed whisper (1, sentences
// 4771/4763/6424) -- so every other mode keeps the historical failure ack rather than
// a packet the client would not render.
//
// The trailing "extra" dstr is read only in the peer-menu window, so the whisper body
// stops after the text.
//
// The text dstr is read by the same validated reader as a normal line (its error
// string is dstr 642 「信息长度有误」, reached from op65 at 0x19692B9), so the real
// message text is repeated here -- the rejected branch ignores it, but an empty one
// would be rejected by the reader itself.
func (s *Service) sendCurrentChatRejectionNotice(session *gameSession, mode, status byte, peerName string, text []byte) error {
	if session == nil {
		return nil
	}
	if mode != currentTownChatPeerMenuMode && mode != currentTownChatWhisperMode {
		return s.sendGameUpperFailure(session, currentPeerChatStateOpcode, currentTownChatGenericError)
	}
	var writer packetWriter
	writer.writeByte(mode)
	writer.writeByte(status)
	writer.writeRawDstr(rosterNameBytes(peerName))
	writer.writeByte(currentChatIncomingLineFlag)
	writer.writeRawDstr(text)
	if mode == currentTownChatPeerMenuMode {
		writer.writeRawDstr(rosterNameBytes(peerName))
	}
	return s.sendGameUpperRawClassCodec(session, currentTownChatOneToOneStateMsgID, writer.bytes(), 0, true)
}
