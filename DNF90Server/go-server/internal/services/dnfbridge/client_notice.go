package dnfbridge

import (
	"encoding/binary"
	"fmt"
	"unicode/utf16"
)

// Class0/op413 is the DLL-private envelope this tree already shares between
// several one-way projections; the first body byte is the only discriminator.
// Types 1, 2 and 3 are taken by the combat-power sidecar, the mood projection
// and the boss-mutation decision respectively, and every consumer in 90CN.dll
// claims a packet as soon as its type and version match. Type 4 is therefore
// the first free slot and is what the generic client notice uses: any server
// module can post one short UTF-16 popup through it without a new opcode and
// without an unproved native UI call, and 90CN.dll renders it through the
// owned notice overlay that already exists for the Lua bridge. The current EXE
// never sees this packet, so no native layout is claimed here.
//
// Body layout: type, version, u16 UTF-16 unit count, u32 ARGB text color, then
// exactly count UTF-16 code units. This is the only notice surface whose color
// the server controls: the client picks the color for its own NOTI 12 surfaces
// from the message-type byte.
const (
	currentClientNoticeMsgID         = currentCombatPowerAffixMsgID
	currentClientNoticeType          = byte(4)
	currentClientNoticeVersion       = byte(1)
	currentClientNoticeHeaderBytes   = 8
	currentClientNoticeMaxCharacters = 511
	currentClientNoticePrivateABI    = "90cn_private_client_notice_v1_type4_argb"
	// The overlay keeps its own theme color when the server sends zero.
	currentClientNoticeDefaultColor = uint32(0)
)

// Version 2 of the same type-4 envelope appends a style byte plus one reserved
// byte to the header. The style is how a server module picks the client-side
// renderer without spending a new opcode:
//
//	0  the owned overlay - a layered window 90CN.dll draws itself. Byte for
//	   byte what version 1 always did, and the only style that honors the
//	   server's ARGB color.
//	1  the native selector prompt the Lua bridge already uses for the
//	   fatigue notice (ShowNativeDungeonSelectFatiguePrompt in 90CN.cpp).
//	2  the client's own native window at scene UI owner 0x298 - a real
//	   in-game window, so it survives exclusive fullscreen and honors "\n"
//	   for multiple lines. The client owns its frame, its title ("公告") and
//	   its colors, so the server controls the text and nothing else.
//
// Why owner 0x298 is the announcement window is recorded outside this tree in
// the workspace-root knowledge note; the short version is that the client
// itself has 2818 call sites of the same shape and the second argument is a
// bare const wchar_t* (proved by the inlined std::wstring::c_str() at
// 0x0094DDFD). 90CN.dll opens it through the single entry point
// OpenNativeAnnouncementWindow, which closes an already-open instance first:
// re-opening an open 0x298 is not a no-op, it takes another branch that adds a
// tutorial sprite to the window.
//
// Compatibility runs both ways on purpose. 90CN.dll still accepts version 1
// (8-byte header, no style) and treats it as style 0, and an unknown style
// falls back to the overlay instead of being dropped. So rolling back either
// side degrades the look rather than losing the message - which matters
// because the DLL hash is pinned in runtime/assets/client-compatibility.json
// and the two sides can be updated at different times.
//
// Version 2 body layout: type, version, u16 UTF-16 unit count, u32 ARGB text
// color, u8 style, u8 reserved, then exactly count UTF-16 code units.
const (
	currentClientNoticeVersionStyled     = byte(2)
	currentClientNoticeStyledHeaderBytes = 10
	currentClientNoticeStyleOverlay      = byte(0)
	currentClientNoticeStyleNativePrompt = byte(1)
	currentClientNoticeStyleNativeWindow = byte(2)
	currentClientNoticeStyledPrivateABI  = "90cn_private_client_notice_v2_type4_argb_style"
)

// buildCurrentClientNoticeBody encodes type, version, the UTF-16 unit count,
// the ARGB text color and the text itself. The count is in UTF-16 code units
// because the DLL copies wchar_t units and rejects an embedded NUL, so both
// bounds are enforced here rather than left to the client.
func buildCurrentClientNoticeBody(text string, color uint32) ([]byte, error) {
	return encodeCurrentClientNoticeBody(text, color, currentClientNoticeVersion,
		currentClientNoticeStyleOverlay)
}

// buildCurrentClientNoticeStyledBody encodes the version-2 envelope, whose only
// addition is the style byte that selects the client-side renderer.
// The style is checked against the three renderers 90CN.dll actually
// implements rather than passed through: the DLL falls back to the overlay on
// an unknown style, so a typo here would silently ship the wrong surface
// instead of failing.
func buildCurrentClientNoticeStyledBody(
	text string, color uint32, style byte,
) ([]byte, error) {
	switch style {
	case currentClientNoticeStyleOverlay,
		currentClientNoticeStyleNativePrompt,
		currentClientNoticeStyleNativeWindow:
	default:
		return nil, fmt.Errorf("client notice style %d is not a client renderer", style)
	}
	return encodeCurrentClientNoticeBody(text, color,
		currentClientNoticeVersionStyled, style)
}

// encodeCurrentClientNoticeBody is the one encoder for both versions; the
// version argument decides the header length so the two callers cannot drift
// apart. Every bound below is the DLL reader's own bound, enforced here because
// the client rejects a malformed private body silently.
func encodeCurrentClientNoticeBody(
	text string, color uint32, version byte, style byte,
) ([]byte, error) {
	header := currentClientNoticeHeaderBytes
	if version == currentClientNoticeVersionStyled {
		header = currentClientNoticeStyledHeaderBytes
	}
	units := utf16.Encode([]rune(text))
	if len(units) == 0 {
		return nil, fmt.Errorf("client notice text is empty")
	}
	if len(units) > currentClientNoticeMaxCharacters {
		return nil, fmt.Errorf(
			"client notice text is %d UTF-16 units, limit is %d",
			len(units),
			currentClientNoticeMaxCharacters,
		)
	}
	for _, unit := range units {
		if unit == 0 {
			return nil, fmt.Errorf("client notice text contains an embedded NUL")
		}
	}
	body := make([]byte, header+len(units)*2)
	body[0] = currentClientNoticeType
	body[1] = version
	binary.LittleEndian.PutUint16(body[2:4], uint16(len(units)))
	binary.LittleEndian.PutUint32(body[4:8], color)
	if header == currentClientNoticeStyledHeaderBytes {
		body[8] = style
		body[9] = 0
	}
	for index, unit := range units {
		offset := header + index*2
		binary.LittleEndian.PutUint16(body[offset:offset+2], unit)
	}
	return body, nil
}

// sendCurrentClientNotice posts one popup notice to the selected client. A
// notice is presentation only: an unencodable text is logged and dropped so a
// caller's gameplay result is never lost to a cosmetic message.
func (s *Service) sendCurrentClientNotice(
	session *gameSession,
	text string,
	color uint32,
	source string,
) error {
	if s == nil || session == nil || session.conn == nil {
		return nil
	}
	body, err := buildCurrentClientNoticeBody(text, color)
	if err != nil {
		s.logGameEvent(session, "game-client-notice-blocked",
			"source", source,
			"char_id", session.selectedCharacterID,
			"reason", err)
		return nil
	}
	return s.writeCurrentClientNotice(session, body, color,
		currentClientNoticeStyleOverlay, currentClientNoticePrivateABI, source)
}

// sendCurrentClientNativeWindowNotice posts the same envelope with style 2, so
// 90CN.dll opens the client's own native window with this text. Reached through
// sendNotice's native_window surface; see notice.go for why callers pick a
// surface rather than a backend.
func (s *Service) sendCurrentClientNativeWindowNotice(
	session *gameSession,
	text string,
	source string,
) error {
	if s == nil || session == nil || session.conn == nil {
		return nil
	}
	body, err := buildCurrentClientNoticeStyledBody(text,
		currentClientNoticeDefaultColor, currentClientNoticeStyleNativeWindow)
	if err != nil {
		s.logGameEvent(session, "game-client-notice-blocked",
			"source", source,
			"char_id", session.selectedCharacterID,
			"style", currentClientNoticeStyleNativeWindow,
			"reason", err)
		return nil
	}
	return s.writeCurrentClientNotice(session, body,
		currentClientNoticeDefaultColor,
		currentClientNoticeStyleNativeWindow,
		currentClientNoticeStyledPrivateABI, source)
}

// writeCurrentClientNotice is the single wire boundary for both notice
// styles: one log shape, one opcode, one send. The version and the header
// length are read back off the encoded body rather than passed in, so a future
// third style cannot report one thing and send another.
//
// text is not logged. A notice can carry a player-visible message that a
// caller composed from account or character state, and the packet log is kept
// free of it on purpose; character_count is enough to correlate with the DLL's
// own "client notice private envelope consumed chars=" line.
func (s *Service) writeCurrentClientNotice(
	session *gameSession,
	body []byte,
	color uint32,
	style byte,
	abi string,
	source string,
) error {
	header := currentClientNoticeHeaderBytes
	if body[1] == currentClientNoticeVersionStyled {
		header = currentClientNoticeStyledHeaderBytes
	}
	s.logGameEvent(session, "game-client-notice-send",
		"source", source,
		"char_id", session.selectedCharacterID,
		"msg_id", currentClientNoticeMsgID,
		"classification", 0,
		"notice_type", currentClientNoticeType,
		"notice_version", body[1],
		"notice_style", style,
		"text_color", color,
		"character_count", (len(body)-header)/2,
		"body_len", len(body),
		"body_source", abi,
		"wire_owner", "90CN.dll",
		"native_exe_protocol", false)
	return s.sendGameUpperRawClass(session, currentClientNoticeMsgID, body, 0)
}
