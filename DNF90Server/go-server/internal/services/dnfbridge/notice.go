package dnfbridge

import "fmt"

// sendNotice is the single server-side entry point for telling one player
// something. Two backends exist because the client splits the work:
//
//   - noticeBackendNative writes class0/NOTI 12, the client's own announcement
//     packet (native_notice.go). The surface and its color are chosen by the
//     client from the message-type byte; the server cannot override either.
//     Decoded from sub_232BC80's jump table (type <= 0x3F indexes byte table
//     0x0272BF1C into dword table 0x0272BF00).
//   - noticeBackendOverlay writes the DLL-private class0/op413 type-4 envelope
//     that 90CN.dll renders itself (client_notice.go), which is the only
//     surface where the server controls color, wrapping and size.
//   - noticeBackendNativeWindow writes the same op413 envelope with style 2,
//     which 90CN.dll turns into the client's own native window (scene UI owner
//     0x298) carrying the server text. It is the only surface that is a real
//     in-game window rather than a chat line or an owned overlay: it survives
//     exclusive fullscreen, honors multi-line text, and the client owns its
//     frame and title ("公告"), so the server controls the text only.
//
// Callers pick a surface, not a backend: "put this on the 系统 tab", "draw this
// as a popup over the game window" and "open a native window with this" are the
// three decisions a caller actually has.
type noticeBackend byte

const (
	noticeBackendNative noticeBackend = iota
	noticeBackendOverlay
	noticeBackendNativeWindow
)

// noticeSurface names one proven destination. ClientColor documents the ARGB
// the client hardcodes for a native surface; it is informational there and
// cannot be changed from the server.
type noticeSurface struct {
	Name        string
	Backend     noticeBackend
	Type        currentNativeNoticeType
	Channel     byte
	ClientColor uint32
}

// The native surfaces below are the distinct code paths in sub_232BC80, each
// confirmed on screen on 2026-08-21 by sending one labeled notice per surface
// to a live client. The chat window puts them on three different tabs and the
// client owns every color: the ARGB values are pushed by the client itself
// from the message type, so a server color is impossible on these surfaces.
var (
	// 系统 tab, light blue. The handle byte makes no visible difference here:
	// types 0/ch0, 0/ch1 and 0/ch2 all rendered identically.
	noticeSurfaceSystemTab = noticeSurface{
		Name: "native_system_tab", Backend: noticeBackendNative,
		Type: currentNativeNoticeTypeSystem, ClientColor: 0xFF9BC8E6,
	}
	// 系统 tab, magenta. A per-player hint that the default light blue proved
	// too easy to miss.
	noticeSurfaceSystemMagenta = noticeSurface{
		Name: "native_system_magenta", Backend: noticeBackendNative,
		Type: currentNativeNoticeTypeSystemMagenta, ClientColor: 0xFFFF00FF,
	}
	// 系统 tab, cyan.
	noticeSurfaceSystemCyan = noticeSurface{
		Name: "native_system_cyan", Backend: noticeBackendNative,
		Type: currentNativeNoticeTypeSystemCyan, ClientColor: 0xFF69F1FF,
	}
	// Type 44 wraps the server text in client string 0x11653, which reads
	// "恭喜[%s]成为[疯狂强化之旅]活动中的幸运获奖者。". Only use it for that event.
	noticeSurfaceSystemLuckyTemplate = noticeSurface{
		Name: "native_system_lucky_template", Backend: noticeBackendNative,
		Type: currentNativeNoticeTypeSystemLuckyTemplate, ClientColor: 0xFFFF00FF,
	}
	// 喇叭 tab, rendered green with the native D&F badge. This is what
	// df_game_r's own world broadcast uses.
	noticeSurfaceMegaphone = noticeSurface{
		Name: "native_megaphone", Backend: noticeBackendNative,
		Type: currentNativeNoticeTypeBroadcast,
	}
	// 普通 tab. df_game_r uses it for reward lines.
	noticeSurfaceNormalTab = noticeSurface{
		Name: "native_normal_tab", Backend: noticeBackendNative,
		Type: currentNativeNoticeTypeReward,
	}
	// The owned overlay: the only surface that accepts a server color.
	noticeSurfaceOverlay = noticeSurface{
		Name: "overlay", Backend: noticeBackendOverlay,
	}
	// The client's own native window (scene UI owner 0x298). Multi-line text
	// works; the client owns the frame, the title and the colors, so a server
	// color is ignored here just like on the native chat surfaces.
	noticeSurfaceNativeWindow = noticeSurface{
		Name: "native_window", Backend: noticeBackendNativeWindow,
	}
)

// noticeRequest is what a caller fills in. Color is honored only by the
// overlay backend; a native surface logs that its client-side color wins.
type noticeRequest struct {
	Surface noticeSurface
	Text    string
	Color   uint32
	// Channel overrides the surface's third byte for a native surface. Zero
	// keeps the surface default, which is what df_game_r sends.
	Channel byte
	// ReferenceID fills the native u16 the client resolves through its own
	// lookup. The client owns every color it paints, so an id it recognizes is
	// the only way a notice line can carry a colored name.
	ReferenceID uint16
	Source      string
}

// sendNotice routes one notice to its surface. A notice is presentation only:
// an unusable request is logged and dropped so a caller's gameplay result is
// never lost to a cosmetic message.
func (s *Service) sendNotice(session *gameSession, request noticeRequest) error {
	if s == nil || session == nil || session.conn == nil {
		return nil
	}
	if request.Source == "" {
		request.Source = "unspecified"
	}
	switch request.Surface.Backend {
	case noticeBackendOverlay:
		return s.sendCurrentClientNotice(
			session,
			request.Text,
			request.Color,
			request.Source+":"+request.Surface.Name,
		)
	case noticeBackendNativeWindow:
		if request.Color != currentClientNoticeDefaultColor {
			s.logGameEvent(session, "game-notice-color-ignored",
				"source", request.Source,
				"surface", request.Surface.Name,
				"requested_color", request.Color,
				"reason", "the_client_owns_the_native_window_frame_and_colors")
		}
		return s.sendCurrentClientNativeWindowNotice(
			session,
			request.Text,
			request.Source+":"+request.Surface.Name,
		)
	case noticeBackendNative:
		if request.Color != currentClientNoticeDefaultColor {
			s.logGameEvent(session, "game-notice-color-ignored",
				"source", request.Source,
				"surface", request.Surface.Name,
				"requested_color", request.Color,
				"client_color", request.Surface.ClientColor,
				"reason", "noti12_color_is_chosen_by_the_client_from_the_message_type")
		}
		channel := request.Surface.Channel
		if request.Channel != 0 {
			channel = request.Channel
		}
		return s.sendCurrentNativeNotice(
			session,
			request.Surface.Type,
			channel,
			request.ReferenceID,
			request.Text,
			request.Source+":"+request.Surface.Name,
		)
	default:
		return fmt.Errorf("unknown notice backend %d", request.Surface.Backend)
	}
}
