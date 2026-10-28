package dnfbridge

import (
	"fmt"

	"golang.org/x/text/encoding/simplifiedchinese"
)

// Class0/NOTI 12 is the client's own announcement packet. The layout is proven
// from both ends and they agree field for field.
//
// Writer, official Linux df_game_r via GameWorld_SendNotiPacketMessage:
//
//	put_header(buf, 0, 12)   // 0 selects the NOTI namespace, 12 the id
//	put_byte(msg_type)
//	put_short(0)
//	put_byte(0)
//	put_int(len) + put_binary(bytes, len)
//	finalize(buf, 1)
//
// Reader, current 90CN client sub_1D821E0 (registered for NOTI 12 at RVA
// 0x01992870 by `push 0; push 0x01D821E0; push 0x0C`):
//
//	read u8   -> message type, rejected when >= 0x40; 0x30/0x31 normalize to 3
//	read u16  -> a reference id. sub_1D821E0 hands it to the map lookup
//	            sub_2699A30 against the manager at 0x051B2728 and stores the
//	            resolved pointer in the notice struct; the type-37 path reads it
//	            back from [esi+0x0C]. df_game_r always writes zero here.
//	read u8   -> rejected when >= 0xFA
//	read string through sub_3457D20 -> read u32 length, reject when >= 0x100,
//	            read that many raw bytes, NUL terminate, then widen into a
//	            256-unit buffer
//
// The wire therefore carries single-byte text, not UTF-16: the client widens it
// itself. A Simplified Chinese client decodes those bytes as GBK.
//
// The surface and its color are chosen by the client from the message-type
// byte (sub_232BC80's jump table: type <= 0x3F indexes byte table 0x0272BF1C
// into dword table 0x0272BF00); the server cannot override either. The types
// below were each confirmed on screen on 2026-08-21 by sending one labeled
// notice per surface to a live client.
const (
	currentNativeNoticeMsgID       = uint16(12)
	currentNativeNoticeMaxBodySize = 0xFF
	currentNativeNoticeMaxType     = byte(0x40)
	currentNativeNoticeReservedU8  = byte(0)
	// The reader rejects the third field at or above 0xFA. df_game_r always
	// sends zero. sub_2327B30 loads it as a handle: a non-zero value is used
	// directly, zero makes the client resolve one itself, and either way the
	// result only gates a later branch.
	currentNativeNoticeMaxChannel = byte(0xFA)
	currentNativeNoticePrivateABI = "current_exe_sub_1D821E0_noti12_from_df_game_r_sendnotipacketmessage"
)

// currentNativeNoticeType enumerates the message-type byte. df_game_r callers
// use 0 for a plain system line, 6 for a reward line and 14 for a world
// broadcast; the byte selects which native surface renders the text.
type currentNativeNoticeType byte

const (
	// 系统 tab, light blue (client ARGB 0xFF9BC8E6). The handle byte makes no
	// visible difference here.
	currentNativeNoticeTypeSystem currentNativeNoticeType = 0
	// 普通 tab. df_game_r uses it for reward lines.
	currentNativeNoticeTypeReward currentNativeNoticeType = 6
	// 喇叭 tab, rendered green with the native D&F badge. This is what
	// df_game_r's own world broadcast uses.
	currentNativeNoticeTypeBroadcast currentNativeNoticeType = 14
	// 系统 tab, magenta (client ARGB 0xFFFF00FF). A hint for one player rather
	// than a broadcast; the default light blue proved too easy to miss.
	currentNativeNoticeTypeSystemMagenta currentNativeNoticeType = 43
	// 系统 tab, cyan (client ARGB 0xFF69F1FF).
	currentNativeNoticeTypeSystemCyan currentNativeNoticeType = 63
	// 系统 tab, magenta, but the client wraps the server text in its own
	// string 0x11653 ("恭喜[%s]成为[疯狂强化之旅]活动中的幸运获奖者。"). Named so
	// the template is not picked by accident for a plain line.
	currentNativeNoticeTypeSystemLuckyTemplate currentNativeNoticeType = 44
)

// buildCurrentNativeNoticeBody encodes the exact plaintext the client reader
// consumes. The GBK conversion is required by the reader's byte-then-widen
// path; the length bound is the reader's own limit, enforced here so a long
// message is reported instead of being silently dropped by the client.
func buildCurrentNativeNoticeBody(
	noticeType currentNativeNoticeType,
	channel byte,
	referenceID uint16,
	text string,
) ([]byte, error) {
	if channel >= currentNativeNoticeMaxChannel {
		return nil, fmt.Errorf(
			"native notice channel %d is not below the client bound %d",
			channel,
			currentNativeNoticeMaxChannel,
		)
	}
	if byte(noticeType) >= currentNativeNoticeMaxType {
		return nil, fmt.Errorf(
			"native notice type %d is not below the client bound %d",
			noticeType,
			currentNativeNoticeMaxType,
		)
	}
	if text == "" {
		return nil, fmt.Errorf("native notice text is empty")
	}
	encoded, err := simplifiedchinese.GBK.NewEncoder().Bytes([]byte(text))
	if err != nil {
		return nil, fmt.Errorf("encode native notice text as GBK: %w", err)
	}
	if len(encoded) == 0 {
		return nil, fmt.Errorf("native notice text encoded to zero bytes")
	}
	if len(encoded) > currentNativeNoticeMaxBodySize {
		return nil, fmt.Errorf(
			"native notice text is %d GBK bytes, client accepts at most %d",
			len(encoded),
			currentNativeNoticeMaxBodySize,
		)
	}
	var writer packetWriter
	writer.writeByte(byte(noticeType))
	writer.writeUint16(referenceID)
	writer.writeByte(channel)
	writer.writeUint32(uint32(len(encoded)))
	writer.writeBytes(encoded)
	return writer.bytes(), nil
}

// sendCurrentNativeNotice pushes one announcement through the client's own
// notice surface. A notice is presentation only, so an unencodable text is
// logged and dropped instead of failing the caller's gameplay result.
func (s *Service) sendCurrentNativeNotice(
	session *gameSession,
	noticeType currentNativeNoticeType,
	channel byte,
	referenceID uint16,
	text string,
	source string,
) error {
	if s == nil || session == nil || session.conn == nil {
		return nil
	}
	body, err := buildCurrentNativeNoticeBody(noticeType, channel, referenceID, text)
	if err != nil {
		s.logGameEvent(session, "game-native-notice-blocked",
			"source", source,
			"char_id", session.selectedCharacterID,
			"notice_type", byte(noticeType),
			"notice_channel", channel,
			"reference_id", referenceID,
			"reason", err)
		return nil
	}
	s.logGameEvent(session, "game-native-notice-send",
		"source", source,
		"char_id", session.selectedCharacterID,
		"msg_id", currentNativeNoticeMsgID,
		"classification", 0,
		"notice_type", byte(noticeType),
		"notice_channel", channel,
		"reference_id", referenceID,
		"text_bytes", len(body)-8,
		"body_len", len(body),
		"body_source", currentNativeNoticePrivateABI)
	return s.sendCurrentSceneFixedClass0Packet(
		session,
		currentNativeNoticeMsgID,
		body,
		source,
	)
}
