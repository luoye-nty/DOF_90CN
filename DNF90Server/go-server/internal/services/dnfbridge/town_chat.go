package dnfbridge

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"strconv"
	"strings"

	dnfchat "longheng.io/server/internal/modules/dnf/chat"
	dnfguild "longheng.io/server/internal/modules/dnf/guild"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

const (
	// The three chat notification bodies, read straight out of DNF.exe. Class-0
	// dispatch is sub_225CA00: it takes the opcode from packet[1..2], looks the
	// handler up in the hash registry cached at 0x51B2070, and calls it as
	// handler(opcode, context) — the body is not passed, the handler pulls it
	// from the global cursor at 0x52F8500. Four primitives touch that cursor:
	// readU8 sub_34E3970, readU16 sub_34E39E0, readU32 sub_34E3A50 and readBytes
	// sub_34E3AC0; sub_3457C80 is readDstr on top of them (readU32 length, then
	// readBytes) and sub_3457D20 wraps it through a TLS scratch buffer:
	//
	//	op12  sub_1D821E0  u8 mode | u16 actorKey | u8 flag | dstr msg(max 256)
	//	op65  sub_1D691F0  u8 mode | u8 status | dstr name(31) | u8 flag |
	//	                   dstr msg(256) | dstr(31) only when mode == 0x24
	//	op118 sub_1D5B370  u8 mode | u8 ch | u8 flag | u8 ch | u16 senderID |
	//	                   dstr name(30) | dstr msg(512)
	//
	// Each has a 装备引用 twin that repeats its body and then appends the item
	// block, `u8 count` followed by count fixed 148-byte entries, read by
	// sub_2717A70. The twins live in a third registry, not the class-0 one: they
	// are registered at 0x272CBE9 as `push context; push handler; push opcode;
	// call sub_1CF2A70; call sub_225C940`, which is why a scan of the class-0
	// registrar sub_2262980 found no trace of them and this file previously
	// recorded them as unregistered.
	//
	//	op370 sub_272BF60  op12's body  + item block
	//	op371 sub_272C140  op65's body  + item block
	//	op372 sub_2729060  op118's body + item block
	//
	// Note the one-slot shift against the 86JP reference, which numbers these
	// 371/372/373 — the same shift its cmd table has at ITEM_HYPERLINK_MESSAGE
	// (425 there, 424 here). Sending 86JP's 372 with 86JP's body displayed
	// nothing live: the opcode belongs to a different twin here and the body was
	// the wrong shape for it.
	//
	// Which carrier a channel takes follows from what its readers can resolve.
	// op12 names its speaker by a scene actor key, so it only attributes a line
	// to readers who can see that actor: fine for area, party and raid, useless
	// for guild. op118 spells the name out and, past its two gates, renders
	// through sub_27282E0 with the packet's own sender id — the clickable
	// identity. op65 is not a carrier at all: its display branches take only
	// modes 1, 0x23, 0x24, 0xFE and 0xFF, and anything else falls through to
	// sub_1D69E71, which drops the line on every reader but the speaker.
	currentTownChatNotificationMsgID uint16 = 12
	// currentRaidAnnouncementMsgID is the class-0 notification whose handler
	// sub_1D88DE0 draws the "- 公告 -" banner. It is op12's twin: the body is the
	// same u8 + u16 actorKey + u8 channel + dstr text, but sub_1D88DE0 reads the
	// first byte as a flag and only when it is zero formats string 640
	// "           - 公告 - \n\n%s" and shows it through sub_2FDD1A0(0x14E, ...).
	// op12 itself ends in the chat log, which is why broadcasting raid chat alone
	// never raised a banner.
	currentRaidAnnouncementMsgID uint16 = 119
	// The 装备引用 twins of op12 and op118. op371, the op65 twin, has no sender
	// here because op65 is not used as a carrier.
	currentTownChatHyperlinkMsgID        uint16 = 370
	currentPeerChatHyperlinkNotifyOpcode uint16 = 372
	currentTownChatWhisperMode           byte   = 1
	currentTownChatOneToOneMode          byte   = 0x21
	currentTownChatPeerMenuMode          byte   = 0x24
	currentTownChatPartyMode             byte   = 2
	currentTownChatAreaMode              byte   = 3
	// currentTownChatArenaMode is the 决斗场 room channel. Live 2026-09-14 22:37
	// (conn game-000147, channel 191, an arena room): the client sent mode 5 and
	// the router answered the unsupported-mode failure, which the chat window
	// renders as silence -- "决斗场房间内打字发不出去". The same capture shows the
	// 区域 line (mode 3) reaching private_scene, i.e. the speaker alone, because
	// an arena room is a private map and never enters the public town presence
	// registry the area scope resolves through.
	currentTownChatArenaMode byte = 5
	// currentTownChatGuildMode is 6, not the 4 the release snapshot assumed.
	// Every op17 request the live EXE has ever produced for the 公会 channel
	// carries mode 6 (protocol.log 2026-09-07 17:47:45 and 22:00:09); mode 4
	// has never appeared on the wire once. The assumed value sent both guild
	// messages into the unsupported-mode branch, which answers a failure code
	// the chat window renders as silence. Keep 4 as a tolerated alias so a
	// build that does emit it is still routed to the guild scope.
	currentTownChatGuildMode       byte = 6
	currentTownChatGuildLegacyMode byte = 4
	currentTownChatMegaphoneMode   byte = 11
	// currentTownChatChannelMegaphoneMode is the 频道喇叭 twin of mode 11. The
	// current EXE builds the same request body for both and only swaps the
	// megaphone it spends; the audience is the speaker's own channel.
	currentTownChatChannelMegaphoneMode byte = 13
	currentTownChatRaidMode             byte = 55
	currentTownChatRequestPrefixLen          = 11
	currentTownChatMaxMessageBytes           = 512
	// op12's decoder (sub_1D821E0) reads its message dstr with a 256-byte cap
	// and discards anything longer, so a composed line has to stay under it.
	currentTownChatDecodedMessageLimit      = 256
	currentTownChatGenericError        byte = 4
	// currentTownChatOneToOneStateMsgID is the class-0 notification the 90CN
	// client reads as its own private-chat window state. Its handler sub_1D691F0
	// reads, in order:
	//
	//	u8 mode | u8 status | dstr name(31) | u8 flag | dstr text(256)
	//	                | dstr extra(31)  -- read only when mode == 0x24
	//
	// status is a *rejection reason*, not a window selector. The handler branches
	// on it at 0x1969CC0, before it reaches the chat window manager, and the four
	// values it can render come from dstr resources 0x12A3 "已将对方添加到黑名单" /
	// 0x81F "对方在黑名单中" / 0x820 "无法向GM发送聊天信息" / 0x81E "对方目前处于无法
	// 接收信息状态". Zero is the only value that means the line was delivered
	// normally, so a successful private line always carries it.
	//
	// The trailing extra dstr feeds the last argument of the chat-line renderer
	// sub_23282E0. What the client expects there is not established, so an empty
	// string is sent rather than invented data.
	currentTownChatOneToOneStateMsgID   uint16 = 65
	currentTownChatOneToOneAcceptStatus byte   = 0
	// Body byte 4 of op65 is not a sender/receiver selector: the client feeds it to
	// sub_22E85A0(flag, &name) as the name registry's selector, where slot 0 is the
	// other party and slot 1 is the player. See currentChatIncomingLineFlag and
	// currentChatOwnLineFlag in town_chat_conversation.go.
)

const (
	currentServerMegaphonePVFPath  = "stackable/cash/megaphone_heartbeat.stk"
	currentChannelMegaphonePVFPath = "stackable/cash/megaphone_echo.stk"
)

type currentTownChatRequest struct {
	Mode           byte
	TargetUniqueID uint16
	Argument       uint32
	Message        []byte
	TargetName     []byte
	TrailingFlag   byte
	HasTrailing    bool
	// ItemCount and ItemData carry the 装备引用 payload of an op424 request. The
	// server never decodes an entry: it echoes the captured bytes back to the
	// audience so the client's own item tooltip owns their layout.
	ItemCount byte
	ItemData  []byte
}

func (r currentTownChatRequest) hasItemHyperlinks() bool {
	return r.ItemCount > 0 && len(r.ItemData) > 0
}

// parseCurrentTownChatFields reads the fixed prefix and the message that open
// both op17 and op424, and reports where the caller must continue.
func parseCurrentTownChatFields(body []byte) (currentTownChatRequest, int, bool) {
	if len(body) < currentTownChatRequestPrefixLen {
		return currentTownChatRequest{}, 0, false
	}
	messageLength := int(int32(binary.LittleEndian.Uint32(body[7:11])))
	if messageLength <= 0 || messageLength > currentTownChatMaxMessageBytes {
		return currentTownChatRequest{}, 0, false
	}
	next := currentTownChatRequestPrefixLen + messageLength
	if next < currentTownChatRequestPrefixLen || next > len(body) {
		return currentTownChatRequest{}, 0, false
	}
	message := append([]byte(nil), body[currentTownChatRequestPrefixLen:next]...)
	if bytes.IndexByte(message, 0) >= 0 {
		return currentTownChatRequest{}, 0, false
	}
	return currentTownChatRequest{
		Mode:           body[0],
		TargetUniqueID: binary.LittleEndian.Uint16(body[1:3]),
		Argument:       binary.LittleEndian.Uint32(body[3:7]),
		Message:        message,
	}, next, true
}

// parseCurrentTownChatPrivateTail reads the target name and the trailing flag a
// private mode appends after its message.
func parseCurrentTownChatPrivateTail(request *currentTownChatRequest, body []byte, next, end int) (int, bool) {
	if next < end {
		name, afterName, ok := readCurrentRawDstr(body[:end], next)
		if !ok || len(name) >= 30 || bytes.IndexByte(name, 0) >= 0 {
			return 0, false
		}
		if len(name) == 0 && !isCurrentOneToOneTownChatMode(request.Mode) {
			return 0, false
		}
		request.TargetName = name
		next = afterName
	}
	if next < end {
		if next+1 != end {
			return 0, false
		}
		request.TrailingFlag = body[next]
		request.HasTrailing = true
		next++
	}
	return next, next == end
}

func currentTownChatRequestShapeValid(request currentTownChatRequest) bool {
	if isCurrentOneToOneTownChatMode(request.Mode) {
		// The two 1:1 window modes are the only private modes whose request may
		// name no peer at all. A client whose window was opened by an *incoming*
		// line sends peer id 0 with an empty name (live capture 2026-09-22
		// 20:16:02 in packet_log.txt), while the client that picked its peer from
		// the friend menu sends the peer's character id and name. The peer of an
		// unnamed request comes from the conversation this server remembers; see
		// town_chat_conversation.go. The trailing byte stays mandatory, and every
		// captured request sent 0 there.
		return request.HasTrailing && request.TrailingFlag <= 1
	}
	if isCurrentPrivateTownChatMode(request.Mode) {
		return request.TargetUniqueID != 0 && len(request.TargetName) > 0 &&
			request.HasTrailing && request.TrailingFlag <= 1
	}
	return len(request.TargetName) == 0 && !request.HasTrailing
}

func parseCurrentTownChatRequest(body []byte) (currentTownChatRequest, bool) {
	request, next, ok := parseCurrentTownChatFields(body)
	if !ok {
		return currentTownChatRequest{}, false
	}
	if next < len(body) {
		name, afterName, ok := readCurrentRawDstr(body, next)
		if !ok || len(name) >= 30 || bytes.IndexByte(name, 0) >= 0 {
			return currentTownChatRequest{}, false
		}
		if len(name) == 0 && !isCurrentOneToOneTownChatMode(request.Mode) {
			return currentTownChatRequest{}, false
		}
		request.TargetName = name
		next = afterName
	}
	if next < len(body) {
		if next+1 != len(body) {
			return currentTownChatRequest{}, false
		}
		request.TrailingFlag = body[next]
		request.HasTrailing = true
		next++
	}
	if next != len(body) {
		return currentTownChatRequest{}, false
	}
	return request, currentTownChatRequestShapeValid(request)
}

// parseCurrentTownChatHyperlinkRequest reads op424, the 装备引用 twin of op17:
// the same fields, then one item count and that many fixed-width entries. The
// captured 普通-channel request (protocol.log 2026-09-07 22:34:36, one linked
// 威士重剑) spends 148 bytes on its single entry, so the width is taken from the
// request itself rather than pinned — this build does not use the 124-byte
// entry of the 86JP reference (`../86JPPP`,
// Server/DfoServer/Network/Builders/ChatMessageBuilder.cs HyperlinkItemDataSize).
func parseCurrentTownChatHyperlinkRequest(body []byte) (currentTownChatRequest, bool) {
	request, next, ok := parseCurrentTownChatFields(body)
	if !ok {
		return currentTownChatRequest{}, false
	}
	if isCurrentPrivateTownChatMode(request.Mode) {
		// The private tail sits between the message and the item payload, so it
		// has to be located before the count byte can be read.
		nameEnd, found := currentTownChatPrivateTailEnd(body, next)
		if !found {
			return currentTownChatRequest{}, false
		}
		if next, ok = parseCurrentTownChatPrivateTail(&request, body, next, nameEnd); !ok {
			return currentTownChatRequest{}, false
		}
	}
	if next >= len(body) {
		return currentTownChatRequest{}, false
	}
	count := body[next]
	next++
	itemBytes := len(body) - next
	if count == 0 || itemBytes <= 0 || itemBytes%int(count) != 0 {
		return currentTownChatRequest{}, false
	}
	request.ItemCount = count
	request.ItemData = append([]byte(nil), body[next:]...)
	return request, currentTownChatRequestShapeValid(request)
}

// currentTownChatPrivateTailEnd finds the end of a private request's name/flag
// tail by reading the name's own length prefix. What follows is the item count.
func currentTownChatPrivateTailEnd(body []byte, next int) (int, bool) {
	name, afterName, ok := readCurrentRawDstr(body, next)
	if !ok || len(name) == 0 || len(name) >= 30 {
		return 0, false
	}
	if afterName >= len(body) {
		return 0, false
	}
	return afterName + 1, true
}

func isCurrentPrivateTownChatMode(mode byte) bool {
	switch mode {
	case currentTownChatWhisperMode, currentTownChatOneToOneMode, currentTownChatPeerMenuMode:
		return true
	default:
		return false
	}
}

// isCurrentOneToOneTownChatMode reports the two private modes that come from the
// client's own 1:1 / peer-menu chat window rather than from a typed whisper.
// They are the only modes the client accepts op65 for, and the only ones whose
// class-0 op17 state packet is withheld (see handleCurrentWhisperChat).
func isCurrentOneToOneTownChatMode(mode byte) bool {
	return mode == currentTownChatOneToOneMode || mode == currentTownChatPeerMenuMode
}

func (s *Service) handleCurrentTownChat(session *gameSession, body []byte) error {
	request, ok := parseCurrentTownChatRequest(body)
	return s.routeCurrentTownChat(session, request, body, ok)
}

// handleCurrentTownChatHyperlink answers op424. Every channel reaches the same
// audience as its plain op17 twin; only the notification changes, because the
// linked items ride along behind the message.
func (s *Service) handleCurrentTownChatHyperlink(session *gameSession, body []byte) error {
	request, ok := parseCurrentTownChatHyperlinkRequest(body)
	return s.routeCurrentTownChat(session, request, body, ok)
}

func (s *Service) routeCurrentTownChat(session *gameSession, request currentTownChatRequest, body []byte, ok bool) error {
	if !ok || session == nil || session.selectedCharacterID == 0 {
		if session != nil {
			s.logGameEvent(session, "game-town-chat-rejected", "body_len", len(body), "reason", "current_exe_chat_shape_required")
			s.recordChat(session, request.Mode, currentTownChatModeName(request.Mode), request.Message, 0, int(request.ItemCount),
				"rejected", "current_exe_chat_shape_required", map[string]any{"body_len": len(body)})
		}
		if !ok && session != nil && session.selectedCharacterID != 0 {
			return s.sendGameUpperFailure(session, currentPeerChatStateOpcode, currentTownChatGenericError)
		}
		return nil
	}

	switch request.Mode {
	case currentTownChatWhisperMode, currentTownChatOneToOneMode, currentTownChatPeerMenuMode:
		return s.handleCurrentWhisperChat(session, request)
	case currentTownChatMegaphoneMode:
		return s.handleCurrentServerMegaphoneChat(session, request)
	case currentTownChatChannelMegaphoneMode:
		return s.handleCurrentChannelMegaphoneChat(session, request)
	case currentTownChatAreaMode, currentTownChatArenaMode, currentTownChatPartyMode, currentTownChatGuildMode,
		currentTownChatGuildLegacyMode, currentTownChatRaidMode:
		return s.handleCurrentScopedTownChat(session, request)
	default:
		s.logGameEvent(session, "game-town-chat-rejected", "mode", request.Mode, "body_len", len(body), "reason", "unsupported_chat_mode")
		s.recordChat(session, request.Mode, currentTownChatModeName(request.Mode), request.Message, 0, int(request.ItemCount),
			"rejected", "unsupported_chat_mode", map[string]any{"body_len": len(body)})
		return s.sendGameUpperFailure(session, currentPeerChatStateOpcode, currentTownChatGenericError)
	}
}

func (s *Service) handleCurrentScopedTownChat(session *gameSession, request currentTownChatRequest) error {
	recipients, scope, ok := s.currentScopedChatRecipients(session, request.Mode)
	if !ok || len(recipients) == 0 {
		s.logGameEvent(session, "game-town-chat-rejected", "mode", request.Mode, "reason", "chat_scope_unavailable")
		s.recordChat(session, request.Mode, currentTownChatModeName(request.Mode), request.Message, 0, int(request.ItemCount),
			"rejected", "chat_scope_unavailable", map[string]any{"scope": scope})
		return s.sendGameUpperFailure(session, currentPeerChatStateOpcode, currentTownChatGenericError)
	}
	// Area, party and raid share a scene with their audience, so op12's actor key
	// resolves and the client prints the speaker itself. Guild does not, and gets
	// the notice that spells the speaker out.
	if isCurrentGuildTownChatMode(request.Mode) {
		return s.broadcastCurrentGuildChat(session, request, recipients, scope)
	}
	// The raid leader's raid-channel speech IS the announcement, so its mode is
	// REPLACED rather than accompanied: mode 56 draws the "- 公告 -" banner, and it
	// prints its own chat line and overhead bubble on the way (the sweep saw one
	// line per packet), so sending the ordinary raid line as well would print every
	// message twice. A hyperlink line travels on op370 instead, whose reader never
	// reaches the notice surface, so those keep the plain mode.
	mode := request.Mode
	var announcedKey uint32
	if !request.hasItemHyperlinks() {
		mode, announcedKey = s.currentRaidAnnouncementModeFor(session, request.Mode)
	}
	notification := buildCurrentTownChatNotification(mode, session.selectedCharacterID, request.Message)
	msgID := currentTownChatNotificationMsgID
	if request.hasItemHyperlinks() {
		notification = appendCurrentChatItemHyperlinks(notification, request)
		msgID = currentTownChatHyperlinkMsgID
	}
	delivered := s.sendCurrentChatPacketToRecipients(session, recipients, msgID, notification)
	// 公会聊天 is one of the four PVF-configured 公会贡献度 activities, paid once per
	// local day against a receipt. Only a 公会-channel line that actually reached
	// someone and carried text counts, which is upstream's gate too; a failure here
	// is deferred rather than turned into a send failure, because the line has
	// already been delivered.
	if isCurrentGuildTownChatMode(request.Mode) && delivered > 0 && len(bytes.TrimSpace(request.Message)) > 0 {
		if updates, err := s.applyCurrentGuildContribution(session, dnfguild.ContributionChat); err != nil {
			s.logGameEvent(session, "game-guild-chat-contribution-deferred", "error", err)
		} else if err = s.sendCurrentGuildContributionUpdates(session, updates); err != nil {
			return err
		}
	}
	announced := 0
	if announcedKey != 0 {
		announced = delivered
		s.logGameEvent(session, "game-raid-leader-announcement-broadcast",
			"raid_key", announcedKey,
			"char_id", session.selectedCharacterID,
			"message_len", len(request.Message),
			"mode", mode,
			"recipients", delivered,
			"source", "op17_raid_chat_from_raid_leader")
	}
	s.logGameEvent(session, "game-town-chat-broadcast",
		"mode", mode,
		"message_len", len(request.Message),
		"recipients", delivered,
		"scope", scope,
		"notification", msgID,
		"item_links", request.ItemCount,
		"announcement_recipients", announced)
	s.recordChat(session, request.Mode, currentTownChatModeName(request.Mode), request.Message, delivered, int(request.ItemCount),
		"ok", "", map[string]any{"scope": scope, "notification": msgID})
	return nil
}

// currentRaidAnnouncementModeFor reports the mode the raid leader's own raid-channel
// speech has to travel as, plus the raid it belongs to.
//
// Only the raid leader (团长) gets the banner; a squad leader or an ordinary member
// keeps the plain raid line. Returning the caller's own mode together with a zero
// key means "send it unchanged".
func (s *Service) currentRaidAnnouncementModeFor(session *gameSession, mode byte) (byte, uint32) {
	if s == nil || session == nil || mode != currentTownChatRaidMode {
		return mode, 0
	}
	sourceID := selectedCharacterID(session)
	state, inRaid := s.runtimeRaidForMember(sourceID)
	if !inRaid || state.LeaderID != sourceID {
		return mode, 0
	}
	return currentRaidAnnouncementMode, state.RaidKey
}

// currentRaidAnnouncementMode is the 90 client's raid announcement banner mode.
//
// Settled by the labelled sweep: the client drew the banner for 56 while 52, 53, 54
// and 57 stayed ordinary chat lines, and a later single-message test rendered the
// banner as "通知 | 横幅测试" from the raw GBK bytes. The 86 server pins the mechanism
// as a chat MODE rather than a separate opcode -- ServerS4A21
// ChatHandler.IsRaidMessageMode(52|53) with CanSendRaidMessage gating 53 on
// senderId == leaderId, the notification carrying the mode through unchanged -- but
// its numbers do not map onto this client (86 raid chat is 52, ours is 55).
//
// The surface itself sits behind op12's notice interception: sub_1D821E0 hands the
// entry to sub_CAB4F0/sub_CAB410 before the chat manager sub_272BC80 ever sees it,
// and sub_272BC80's 64-way switch on the mode folds 45..62 (56 included) into its
// plain-line default -- so the banner is drawn by that interception, not by the
// switch.
const currentRaidAnnouncementMode byte = 56

// buildCurrentRaidAnnouncementNotification builds the leader's banner notification.
//
// The body is the ordinary op12 notification byte for byte -- the mode is the only
// thing that selects the banner -- and the text travels exactly as the client sent
// it. Live confirmation on this client: the banner drew "横幅测试" correctly from the
// raw GBK bytes, which is the same reader every other chat line already uses
// (sub_3457D20 -> sub_34D9CA0 -> wide), so re-encoding the text can only corrupt it.
func buildCurrentRaidAnnouncementNotification(sourceID uint16, message []byte) []byte {
	return buildCurrentTownChatNotification(currentRaidAnnouncementMode, sourceID, message)
}

func isCurrentGuildTownChatMode(mode byte) bool {
	return mode == currentTownChatGuildMode || mode == currentTownChatGuildLegacyMode
}

// currentChatServerGroup is body byte 2 of the op118 notice, and the value gate
// 0x1D5B4E3 checks. Two probe rounds settled it: with byte(channel.ID) there,
// clients on channels 508 and 121 both answered local_channel=1 and rejected the
// line; with 1 there, both accepted it and gate 0x1D5B5B7 then matched the
// reader's own channel out of byte 4 (121 against 121, 251 against 251). So byte
// 2 is the server group — the field 86JP names serverGroup in the third slot of
// its own op12 body (`../86JPPP`,
// Server/DfoServer/Network/Builders/ChatMessageBuilder.cs BuildChatMessage) —
// and it equals this instance's protocol.channelServerIndex / regionId.
const currentChatServerGroup = byte(1)

// broadcastCurrentGuildChat delivers a guild line on the notice that names its
// speaker. One projection per reader: gate 0x1D5B5B7 wants that reader's own
// channel in byte 4, so a single shared body would be dropped by everyone
// standing elsewhere.
func (s *Service) broadcastCurrentGuildChat(
	session *gameSession,
	request currentTownChatRequest,
	recipients []*gameSession,
	scope string,
) error {
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	loadedID, sourceName, _, found := s.characterForEnter(ctx, session, session.selectedCharacterID)
	if !found || loadedID != session.selectedCharacterID {
		s.logGameEvent(session, "game-town-chat-rejected", "mode", request.Mode,
			"reason", "chat_speaker_name_unavailable")
		s.recordChat(session, request.Mode, currentTownChatModeName(request.Mode), request.Message, 0, int(request.ItemCount),
			"rejected", "chat_speaker_name_unavailable", map[string]any{"scope": scope})
		return s.sendGameUpperFailure(session, currentPeerChatStateOpcode, currentTownChatGenericError)
	}
	name := rosterNameBytes(sourceName)
	msgID := currentPeerChatNotifyOpcode
	if request.hasItemHyperlinks() {
		msgID = currentPeerChatHyperlinkNotifyOpcode
	}
	delivered := 0
	for _, recipient := range recipients {
		if recipient == nil {
			continue
		}
		body := buildCurrentGuildChatNoticeBody(request.Mode, currentChatServerGroup,
			byte(recipient.channel.ID), session.selectedCharacterID, name, request.Message)
		if request.hasItemHyperlinks() {
			body = appendCurrentChatItemHyperlinks(
				buildCurrentHyperlinkChatNoticeBody(request.Mode, currentChatServerGroup,
					byte(recipient.channel.ID), session.selectedCharacterID, name, request.Message),
				request)
		}
		if err := s.sendGameUpperRawClassCodec(recipient, msgID, body, 0, true); err != nil {
			s.logGameEvent(session, "game-chat-recipient-write-failed",
				"recipient_char_id", recipient.selectedCharacterID,
				"msg_id", msgID, "error", err)
			continue
		}
		delivered++
	}
	s.logGameEvent(session, "game-town-chat-broadcast",
		"mode", request.Mode,
		"message_len", len(request.Message),
		"recipients", delivered,
		"scope", scope,
		"notification", msgID,
		"item_links", request.ItemCount)
	s.recordChat(session, request.Mode, currentTownChatModeName(request.Mode), request.Message, delivered, int(request.ItemCount),
		"ok", "", map[string]any{"scope": scope, "notification": msgID, "speaker_name": sourceName})
	// 进行公会聊天，可以获得2点公会贡献 (help text 76515), once per local day.
	// Paused on the user's request (2026-09-12): only 签到 pays 公会贡献 for now.
	// Re-enable by uncommenting; the grant itself is asynchronous and never blocks
	// this session goroutine.
	// s.grantCurrentGuildChatMileage(session, "current_guild_chat")
	return nil
}

// buildCurrentGuildChatNoticeBody is buildCurrentPeerChatNoticeBody with the two
// leading bytes told apart: sub_1D5B370 reads mode, server group, a flag, the
// reader's channel, the sender id, then the name and the message.
//
// The horn builder still writes the channel into both slots. That is wrong by
// this layout, and it is why horns render on the gate-failure path instead of
// the ordinary one — but they do render, and moving them would need its own
// retest, so it is left alone. Cross-channel whisper uses the same builder and
// is very likely broken by the same byte; it has not been retested either.
func buildCurrentGuildChatNoticeBody(mode, serverGroup, channelID byte, senderID uint16, senderName, message []byte) []byte {
	var writer packetWriter
	writer.writeByte(mode)
	writer.writeByte(serverGroup)
	writer.writeByte(0)
	writer.writeByte(channelID)
	writer.writeUint16(senderID)
	writer.writeRawDstr(senderName)
	writer.writeRawDstr(message)
	return writer.bytes()
}

// buildCurrentOneToOneChatStateBody builds op65, the notification the client's
// own 1:1 and peer-menu chat windows are fed by. It carries one chat line and the
// name of the person who spoke it: the client's two renderer exits differ only in
// the flag slot, and the name is what the line is attributed to.
//
// The trailing dstr exists only because the client's reader consumes it whenever
// the mode byte is the peer-menu mode (sub_1D691F0, 0x19692DA compares it against
// 0x24). It is sent empty rather than omitted, so the body keeps its declared
// shape on the exact mode that asks for it.
func buildCurrentOneToOneChatStateBody(mode, flag byte, speakerName, peerName, message []byte) []byte {
	var writer packetWriter
	writer.writeByte(mode)
	writer.writeByte(currentTownChatOneToOneAcceptStatus)
	writer.writeRawDstr(speakerName)
	writer.writeByte(flag)
	writer.writeRawDstr(message)
	// The trailing dstr is the window's *peer* -- the person this conversation is
	// with -- and leaving it empty is what made the client open a nameless window
	// titled 除玩家[]外1人 next to the named one the player already had. Live
	// screenshot 2026-09-22 21:5x: the window that actually carried the lines was
	// titled with an EMPTY name while the window the player opened showed the peer.
	if mode == currentTownChatPeerMenuMode {
		writer.writeRawDstr(peerName)
	}
	return writer.bytes()
}

// sendCurrentHyperlinkChatToRecipients fans an op372 line out with one body per
// reader, which its channel gate requires.
func (s *Service) sendCurrentHyperlinkChatToRecipients(
	sender *gameSession,
	recipients []*gameSession,
	request currentTownChatRequest,
	senderName []byte,
) int {
	delivered := 0
	for _, recipient := range recipients {
		if recipient == nil {
			continue
		}
		body := appendCurrentChatItemHyperlinks(
			buildCurrentHyperlinkChatNoticeBody(request.Mode, currentChatServerGroup,
				byte(recipient.channel.ID), sender.selectedCharacterID, senderName, request.Message),
			request)
		if err := s.sendGameUpperRawClassCodec(recipient, currentPeerChatHyperlinkNotifyOpcode, body, 0, true); err != nil {
			s.logGameEvent(sender, "game-chat-recipient-write-failed",
				"recipient_char_id", recipient.selectedCharacterID,
				"msg_id", currentPeerChatHyperlinkNotifyOpcode, "error", err)
			continue
		}
		delivered++
	}
	return delivered
}

// buildCurrentHyperlinkChatNoticeBody is the op372 twin of the body above. Its
// four leading bytes sit in the same order: sub_2729060 stores them at
// [ebp-0xCCD], [ebp-0xCCE], [ebp-0xCD0] and [ebp-0xCCF], and gates on
// [ebp-0xCCE] at 0x2729296 and [ebp-0xCCF] at 0x272936C — byte 2 for the server
// group and byte 4 for the reader's channel, exactly like sub_1D5B370.
//
// A first reading of that prologue paired one `lea` with the wrong `call` and
// concluded bytes 3 and 4 were swapped here; sending the channel in byte 3 then
// left byte 4 zero and the line rendered nowhere, which is the same symptom the
// original order produced for a different reason. Whatever still blocks op372
// is past both gates — the probes on 0x2729296 and 0x272936C will say where.
func buildCurrentHyperlinkChatNoticeBody(mode, serverGroup, channelID byte, senderID uint16, senderName, message []byte) []byte {
	var writer packetWriter
	writer.writeByte(mode)
	writer.writeByte(serverGroup)
	writer.writeByte(0)
	writer.writeByte(channelID)
	writer.writeUint16(senderID)
	writer.writeRawDstr(senderName)
	writer.writeRawDstr(message)
	return writer.bytes()
}

// appendCurrentChatItemHyperlinks closes a 装备引用 notification with the count
// and the request's own entries, in the shape sub_2717A70 reads them: one count
// byte then that many 148-byte entries. The entries stay opaque on purpose —
// the server has no reason to reinterpret an item block the client both wrote
// and will render.
func appendCurrentChatItemHyperlinks(body []byte, request currentTownChatRequest) []byte {
	out := make([]byte, 0, len(body)+1+len(request.ItemData))
	out = append(out, body...)
	out = append(out, request.ItemCount)
	return append(out, request.ItemData...)
}

// currentPVPRoomChatRecipients resolves the 决斗场 room the speaker is seated in:
// its seats are the audience the client shows for a room channel. A speaker who
// is not seated in a live room keeps the historical private-scene audience,
// which is the speaker alone.
//
// session.pvpRoom is read under pvpMu like every other room reader, and the
// seats are resolved back through currentChatSessionsForCharacterIDs so a stale
// seat cannot deliver into a replaced session.
func (s *Service) currentPVPRoomChatRecipients(session *gameSession) ([]*gameSession, string, bool) {
	if s == nil || session == nil {
		return nil, "private_scene", false
	}
	s.pvpMu.Lock()
	room := session.pvpRoom
	var characterIDs []uint16
	if room != nil && s.pvpRooms[room.id] == room {
		characterIDs = make([]uint16, 0, len(room.members))
		for _, member := range room.members {
			if member == nil || member.characterID == 0 {
				continue
			}
			characterIDs = append(characterIDs, member.characterID)
		}
	}
	s.pvpMu.Unlock()
	if len(characterIDs) == 0 {
		return []*gameSession{session}, "private_scene", true
	}
	recipients := s.currentChatSessionsForCharacterIDs(characterIDs)
	included := false
	for _, recipient := range recipients {
		if recipient == session {
			included = true
			break
		}
	}
	if !included {
		recipients = append(recipients, session)
	}
	return recipients, "pvp_room", len(recipients) > 0
}

func (s *Service) currentScopedChatRecipients(session *gameSession, mode byte) ([]*gameSession, string, bool) {
	if s == nil || session == nil || session.selectedCharacterID == 0 {
		return nil, "", false
	}
	switch mode {
	case currentTownChatArenaMode:
		// The 决斗场 channel only exists inside an arena room, so its audience is
		// that room's seats.
		return s.currentPVPRoomChatRecipients(session)
	case currentTownChatAreaMode:
		// Town actors, movement and private stores are owned by one concrete
		// OnlinePlayerManager scene, not by the transport's channel number.  A
		// visible actor can therefore come from another channel (ordinary towns
		// use instance zero), while raid waiting rooms remain isolated by their
		// server-owned InstanceKey.  Area chat must use that same visibility
		// boundary or clients can see a robot/player without receiving its chat.
		if s.onlinePlayers == nil {
			return s.currentPVPRoomChatRecipients(session)
		}
		sender, found := s.onlinePlayers.PlayerForCharacter(session.selectedCharacterID)
		if !found || sender.Session != session {
			// A character in its own Seria room is deliberately kept out of the
			// public-town registry — publishCurrentTownPresence retires it and
			// returns early for a private map — so this lookup is empty for the
			// whole stretch between login and walking out into the town. Treating
			// that as "no audience" answered the failure code, which the chat
			// window renders as silence: a player who logged in and typed in the
			// 普通 channel saw nothing at all. There is nobody else in a private
			// scene, so the speaker is the whole audience — except in an arena
			// room, where the other seats are the audience the client shows.
			return s.currentPVPRoomChatRecipients(session)
		}
		players := s.onlinePlayers.GetAreaInstancePlayers(
			sender.ChannelID, sender.TownID, sender.AreaID, sender.InstanceKey)
		recipients := make([]*gameSession, 0, len(players))
		for _, player := range players {
			candidate := player.Session
			if candidate == nil || candidate.selectedCharacterID != player.CharacterID {
				continue
			}
			recipients = append(recipients, candidate)
		}
		return recipients, "visible_town_scene", len(recipients) > 0
	case currentTownChatPartyMode:
		identity, bound := s.boundGameSessionCharacterSnapshot(session)
		if !bound {
			return nil, "party", false
		}
		party, found := s.runtimePartyManagerForService().SnapshotByUser(identity.character, identity.generation)
		if !found {
			return nil, "party", false
		}
		ids := make([]uint16, 0, len(party.Members))
		for _, member := range party.Members {
			if online, found := s.onlineGameSessionCharacterSnapshot(member.UserID); found && online.generation == member.SessionGeneration {
				ids = append(ids, member.UserID)
			}
		}
		return s.currentChatSessionsForCharacterIDs(ids), "party", len(ids) > 0
	case currentTownChatRaidMode:
		raidState, found := s.runtimeRaidForMember(session.selectedCharacterID)
		if !found {
			return nil, "raid", false
		}
		ids := make([]uint16, 0, len(raidState.Members))
		for _, member := range raidState.Members {
			ids = append(ids, member.CharID)
		}
		return s.currentChatSessionsForCharacterIDs(ids), "raid", len(ids) > 0
	case currentTownChatGuildMode, currentTownChatGuildLegacyMode:
		ids, found := s.currentGuildChatCharacterIDs(session.selectedCharacterID)
		if !found {
			return nil, "guild", false
		}
		return s.currentChatSessionsForCharacterIDs(ids), "guild", len(ids) > 0
	default:
		return nil, "", false
	}
}

func (s *Service) currentGuildChatCharacterIDs(characterID uint16) ([]uint16, bool) {
	ctx, cancel := s.currentGuildContext()
	defer cancel()
	var ids []uint16
	err := s.readCurrentGuild(ctx, func(_ dnfrepo.Group, registry dnfguild.Registry) error {
		guild, _, found := registry.GuildForCharacter(strconv.Itoa(int(characterID)))
		if !found || guild == nil {
			return dnfguild.ErrNotMember
		}
		ids = make([]uint16, 0, len(guild.Members))
		for memberID := range guild.Members {
			value, err := strconv.ParseUint(strings.TrimSpace(memberID), 10, 16)
			if err == nil && value != 0 {
				ids = append(ids, uint16(value))
			}
		}
		return nil
	})
	return ids, err == nil
}

func (s *Service) handleCurrentWhisperChat(session *gameSession, request currentTownChatRequest) error {
	// The two 1:1 window modes have their own path: town_chat_conversation.go owns
	// the conversation registry -- the server-side answer to "who is in this
	// client's chat window", which the client's request cannot carry -- and
	// forwards one line to the peer only. Every other private mode below keeps the
	// historical op17/op118 packets byte for byte.
	if isCurrentOneToOneTownChatMode(request.Mode) {
		return s.handleCurrentOneToOneChatMessage(session, request)
	}
	sourceID := session.selectedCharacterID
	targetID, targetSession, resolved := s.resolveCurrentWhisperTarget(session, request)
	if !resolved || targetID == sourceID {
		s.logGameEvent(session, "game-peer-chat-request-rejected",
			"source_char_id", sourceID,
			"requested_target_id", request.TargetUniqueID,
			"target_name", string(request.TargetName),
			"reason", "target_unresolved_or_offline")
		s.recordChat(session, request.Mode, currentTownChatModeName(request.Mode), request.Message, 0, int(request.ItemCount),
			"rejected", "target_unresolved_or_offline", map[string]any{
				// 统一成字符串：同一类别里同一个字段出现两种 JSON 类型，会给
				// CSV 导出与按字段筛选埋坑（"7" 与 7 不是一回事）。
				"target_character_id": strconv.Itoa(int(request.TargetUniqueID)),
				"target_name":         chatRecordText(request.TargetName),
			})
		return s.sendGameUpperFailure(session, currentPeerChatStateOpcode, currentTownChatGenericError)
	}
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	loadedSourceID, sourceName, _, sourceFound := s.characterForEnter(ctx, session, sourceID)
	loadedTargetID, targetName, _, targetFound := s.characterForEnter(ctx, targetSession, targetID)
	if !sourceFound || loadedSourceID != sourceID || !targetFound || loadedTargetID != targetID ||
		!currentChatNameMatches(request.TargetName, rosterNameBytes(targetName)) {
		s.logGameEvent(session, "game-peer-chat-request-rejected", "source_char_id", sourceID, "target_char_id", targetID, "reason", "repository_identity_or_target_name_mismatch")
		s.recordChat(session, request.Mode, currentTownChatModeName(request.Mode), request.Message, 0, int(request.ItemCount),
			"rejected", "repository_identity_or_target_name_mismatch", map[string]any{
				"target_character_id": strconv.Itoa(int(targetID)),
				"target_name":         chatRecordText(request.TargetName),
			})
		return s.sendGameUpperFailure(session, currentPeerChatStateOpcode, currentTownChatGenericError)
	}
	// Blacklist gate, the same both-directions rule the 1:1 window path applies. A
	// typed whisper used to be delivered to a character who had blocked the sender
	// (operator report 2026-09-22 23:5x: "悄悄话现在并不能拦截并提示"), because only
	// town_chat_conversation.go carried the gate. The rejection rides the client's own
	// whisper rejected branch, which renders dstr 4763 您在对方的黑名单内 / 4771
	// 已将对方添加到黑名单 as a system line.
	blockedByTarget := s.currentBlacklistHas(ctx, targetID, sourceName)
	blockedBySource := s.currentBlacklistHas(ctx, sourceID, targetName)
	if blockedByTarget || blockedBySource {
		status := currentBlacklistStatusTargetBlockedSender
		if blockedBySource {
			status = currentBlacklistStatusSenderBlockedTarget
		}
		s.logGameEvent(session, "game-peer-chat-blocked",
			"source_char_id", sourceID, "target_char_id", targetID,
			"blocked_by_target", blockedByTarget, "blocked_by_source", blockedBySource,
			"status", status, "mode", request.Mode, "channel", "whisper")
		s.recordChat(session, request.Mode, currentTownChatModeName(request.Mode), request.Message, 0, int(request.ItemCount),
			"rejected", "blacklist", map[string]any{
				"target_character_id": strconv.Itoa(int(targetID)),
				"target_name":         targetName,
				"status":              strconv.Itoa(int(status)),
			})
		return s.sendCurrentChatRejectionNotice(session, request.Mode, status, targetName, request.Message)
	}
	if err := s.sendGameUpperSuccess(session, currentPeerChatStateOpcode, nil); err != nil {
		return err
	}
	if err := s.sendGameUpperRawClassCodec(session, currentPeerChatStateOpcode, buildCurrentPeerChatStateBody(targetID, request.Mode), 0, true); err != nil {
		return err
	}
	if err := s.sendGameUpperRawClassCodec(targetSession, currentPeerChatStateOpcode, buildCurrentPeerChatStateBody(sourceID, request.Mode), 0, true); err != nil {
		return err
	}
	// One projection per reader: sub_1D5B370 wants the server group in body byte
	// 2 and that reader's own channel in byte 4 before it reaches the ordinary
	// private-chat display path. This also supplies the sender echo that the
	// current 90CN client does not create locally.
	//
	// Each side is told its *peer*, not the speaker. A private line is filed
	// under the person on the other end — which is how the op17 state packets
	// address a typed whisper, the sender getting the target's id and the target
	// getting the sender's, and how the op65 state above names each side's peer.
	// Naming the speaker in both copies made
	// the sender's echo describe a conversation with itself, and the client drops
	// that: the whisper reached the target and left the sender's own window
	// empty. 86JP echoes with the peer's name for the same reason (`../86JPPP`,
	// Server/DfoServer/Network/Handlers/ChatHandler.cs SendWhisperAsync).
	//
	// The echo also uses the outgoing twin of the request's mode, so the line
	// reads "发送给 [名字]" instead of repeating the incoming "[名字]:" form.
	// sub_2727B30 switches on the mode at struct+0x18 through the table at
	// 0x27280BC, and modes 1, 7, 0x24, 0x27, 0x28 and 0x29 share one case while
	// every other mode takes the other — that set is the private-chat family,
	// and 7 is the member that pairs with the 1 the client sends for a typed
	// whisper. The prefix strings themselves are encrypted in the EXE
	// (sub_34D9B20 decodes them), so which member carries which wording is the
	// one part of this that a live line still has to confirm.
	// A private line carries no peer id. sub_2727B30 treats modes 1 and 2 alike
	// at 0x2727D4D: it resolves the notice's peer id to a scene actor and puts a
	// speech balloon over that actor's head. That is right for party chat and
	// wrong for a whisper, and it only showed up once the notice started
	// rendering at all. Zero leaves sub_2699A30 with nothing to resolve, so the
	// line stays in the chat window. The private window still knows who it is
	// talking to -- a typed whisper is told by the two op17 state packets above.
	//
	// The cost is that the name in a whisper line is text rather than a
	// clickable identity. Guild keeps its id and stays clickable.
	//
	// op118's display branches do not know mode 0x24 at all — they recognise the
	// megaphone family and the typed whisper — which is why the 1:1 modes left
	// this path for op65 (town_chat_conversation.go). Every other private mode
	// keeps the named notice byte for byte.
	noticeOpcode := currentPeerChatNotifyOpcode
	const privateChatNoBalloonPeer = uint16(0)
	sourceNotice := buildCurrentGuildChatNoticeBody(currentOutgoingWhisperMode(request.Mode), currentChatServerGroup, byte(session.channel.ID), privateChatNoBalloonPeer, rosterNameBytes(targetName), request.Message)
	targetNotice := buildCurrentGuildChatNoticeBody(request.Mode, currentChatServerGroup, byte(targetSession.channel.ID), privateChatNoBalloonPeer, rosterNameBytes(sourceName), request.Message)
	if request.hasItemHyperlinks() {
		noticeOpcode = currentPeerChatHyperlinkNotifyOpcode
		sourceNotice = appendCurrentChatItemHyperlinks(
			buildCurrentHyperlinkChatNoticeBody(currentOutgoingWhisperMode(request.Mode), currentChatServerGroup,
				byte(session.channel.ID), privateChatNoBalloonPeer, rosterNameBytes(targetName), request.Message), request)
		targetNotice = appendCurrentChatItemHyperlinks(
			buildCurrentHyperlinkChatNoticeBody(request.Mode, currentChatServerGroup,
				byte(targetSession.channel.ID), privateChatNoBalloonPeer, rosterNameBytes(sourceName), request.Message), request)
	}
	if err := s.sendGameUpperRawClassCodec(session, noticeOpcode, sourceNotice, 0, true); err != nil {
		return err
	}
	if err := s.sendGameUpperRawClassCodec(targetSession, noticeOpcode, targetNotice, 0, true); err != nil {
		return err
	}
	s.logGameEvent(session, "game-peer-chat-forwarded", "source_char_id", sourceID, "target_char_id", targetID, "target_name", targetName, "message_len", len(request.Message), "scope", "online_cross_channel", "notification", noticeOpcode, "item_links", request.ItemCount)
	s.recordChat(session, request.Mode, currentTownChatModeName(request.Mode), request.Message, 2, int(request.ItemCount),
		"ok", "", map[string]any{
			"scope":               "online_cross_channel",
			"notification":        noticeOpcode,
			"target_character_id": strconv.Itoa(int(targetID)),
			"target_name":         targetName,
			"target_channel_id":   targetSession.channel.ID,
		})
	return nil
}

// currentWhisperTargetUnknownID is what the EXE puts in the unique-id slot when
// the player typed the recipient's name instead of clicking a known peer. The
// captured request reads `01 FF FF ... dstr 悄悄话 ... dstr 鬼剑士 01`: the id is
// a placeholder and the name is the real selector. Treating 0xFFFF as a
// character id looked it up in the session map, missed, and answered the
// failure code the chat window renders as silence.
const currentWhisperTargetUnknownID = uint16(0xFFFF)

// currentTownChatWhisperSentMode is the outgoing twin of the plain 悄悄话 mode.
// The other private modes are echoed unchanged: they come from the one-to-one
// window and the peer-menu, which own their own presentation.
const currentTownChatWhisperSentMode = byte(7)

func currentOutgoingWhisperMode(mode byte) byte {
	if mode == currentTownChatWhisperMode {
		return currentTownChatWhisperSentMode
	}
	return mode
}

func (s *Service) resolveCurrentWhisperTarget(
	session *gameSession,
	request currentTownChatRequest,
) (uint16, *gameSession, bool) {
	if request.TargetUniqueID != 0 && request.TargetUniqueID != currentWhisperTargetUnknownID {
		if target, online := s.onlineGameSession(request.TargetUniqueID); online {
			return request.TargetUniqueID, target, true
		}
		return 0, nil, false
	}
	if len(request.TargetName) == 0 {
		return 0, nil, false
	}
	if targetID, target, found := s.currentChatSessionByName(session, request.TargetName); found {
		return targetID, target, true
	}
	return 0, nil, false
}

// currentChatSessionByName resolves a chat target by the name a client carried:
// the typed name of a whisper, or the peer name inside a 1:1 window. The stored
// roster name is what decides, because that is the name the other side's own
// window will display.
func (s *Service) currentChatSessionByName(session *gameSession, name []byte) (uint16, *gameSession, bool) {
	if len(name) == 0 {
		return 0, nil, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	for _, candidate := range s.currentOnlineChatSessions() {
		if candidate == nil || candidate == session {
			continue
		}
		charID := candidate.selectedCharacterID
		loadedID, stored, _, found := s.characterForEnter(ctx, candidate, charID)
		if !found || loadedID != charID {
			continue
		}
		if currentChatNameMatches(name, rosterNameBytes(stored)) {
			return charID, candidate, true
		}
	}
	return 0, nil, false
}

func currentChatNameMatches(requested, stored []byte) bool {
	return bytes.Equal(requested, stored) || strings.EqualFold(string(requested), string(stored))
}

// currentMegaphoneKind is one loudspeaker chat mode. The current EXE builds the
// same request body for every megaphone mode and picks the stackable it spends
// from the mode byte alone (see the item-id table quoted in dnfchat), so the
// server side differs only in the item it debits and in who hears the line.
type currentMegaphoneKind struct {
	itemID         uint32
	pvfPath        string
	scope          string
	rejectedEvent  string
	broadcastEvent string
	refreshNote    string
	// channelScoped narrows the audience to the speaker's own channel, which is
	// what 频道喇叭 means. The server megaphone stays server-wide.
	channelScoped bool
	consume       func(*dnfchat.Owner, context.Context, dnfchat.Command) (dnfchat.Result, error)
}

var (
	currentServerMegaphoneKind = currentMegaphoneKind{
		itemID:         dnfchat.ServerMegaphoneItemID,
		pvfPath:        currentServerMegaphonePVFPath,
		scope:          "all_online_game_sessions",
		rejectedEvent:  "game-server-megaphone-rejected",
		broadcastEvent: "game-server-megaphone-broadcast",
		refreshNote:    "current_exe_op118_type11_local_sender_debits_item36",
		consume:        (*dnfchat.Owner).ConsumeServerMegaphone,
	}
	currentChannelMegaphoneKind = currentMegaphoneKind{
		itemID:         dnfchat.ChannelMegaphoneItemID,
		pvfPath:        currentChannelMegaphonePVFPath,
		scope:          "online_game_sessions_on_speaker_channel",
		rejectedEvent:  "game-channel-megaphone-rejected",
		broadcastEvent: "game-channel-megaphone-broadcast",
		refreshNote:    "current_exe_op118_type13_local_sender_debits_item37",
		channelScoped:  true,
		consume:        (*dnfchat.Owner).ConsumeChannelMegaphone,
	}
)

func (s *Service) handleCurrentServerMegaphoneChat(session *gameSession, request currentTownChatRequest) error {
	return s.handleCurrentMegaphoneChat(session, request, currentServerMegaphoneKind)
}

// handleCurrentChannelMegaphoneChat answers 频道喇叭 (mode 13). It is the exact
// twin of the server megaphone apart from the item it spends (37 instead of 36)
// and the audience, which is limited to the sessions attached to the speaker's
// own channel.
func (s *Service) handleCurrentChannelMegaphoneChat(session *gameSession, request currentTownChatRequest) error {
	return s.handleCurrentMegaphoneChat(session, request, currentChannelMegaphoneKind)
}

func (s *Service) handleCurrentMegaphoneChat(session *gameSession, request currentTownChatRequest, kind currentMegaphoneKind) error {
	if request.Argument > uint32(^uint16(0)>>1) {
		return s.sendGameUpperFailure(session, currentPeerChatStateOpcode, currentTownChatGenericError)
	}
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	loadedID, sourceName, _, found := s.characterForEnter(ctx, session, session.selectedCharacterID)
	if !found || loadedID != session.selectedCharacterID {
		return s.sendGameUpperFailure(session, currentPeerChatStateOpcode, currentTownChatGenericError)
	}
	catalog, err := s.currentPVFItemCatalog()
	if err != nil {
		return s.rejectCurrentMegaphone(session, kind, request, fmt.Errorf("load item catalog: %w", err))
	}
	definition, err := catalog.ResolveItem(kind.itemID)
	if err != nil || definition.Kind != dungeonDropItemStackable ||
		cleanDungeonDropPath(definition.PVFPath) != kind.pvfPath ||
		normalizeDungeonDropStackableType(definition.StackableType) != "[waste]" {
		return s.rejectCurrentMegaphone(session, kind, request, fmt.Errorf("item %d PVF identity mismatch: path=%q type=%q err=%v", kind.itemID, definition.PVFPath, definition.StackableType, err))
	}
	repositories, ok := s.repositoryGroup()
	if !ok {
		return s.rejectCurrentMegaphone(session, kind, request, dnfchat.ErrOwnerUnavailable)
	}
	owner, err := dnfchat.NewOwner(repositories)
	if err != nil {
		return s.rejectCurrentMegaphone(session, kind, request, err)
	}
	result, err := kind.consume(owner, ctx, dnfchat.Command{
		AccountID:   s.accountIDForSession(session),
		CharacterID: strconv.Itoa(int(session.selectedCharacterID)),
		Slot:        int16(request.Argument),
		UpdatedAt:   s.gameplayNow(),
	})
	if err != nil {
		return s.rejectCurrentMegaphone(session, kind, request, err)
	}
	sourceNameBytes := rosterNameBytes(sourceName)
	notice := buildCurrentPeerChatNoticeBody(request.Mode, byte(session.channel.ID), session.selectedCharacterID, sourceNameBytes, request.Message)
	recipients := s.currentOnlineChatSessions()
	if kind.channelScoped {
		recipients = currentChatSessionsOnChannel(recipients, session.channel.ID)
	}
	// A horn carrying 装备引用 goes out on op372, which needs the gate bytes filled
	// properly and therefore one body per reader. Only a human speaker can raise
	// one, so this stays clear of the robot identity projection below, and the
	// plain horn keeps the body it renders with today untouched.
	var delivered int
	if request.hasItemHyperlinks() && !session.robotSessionBound {
		delivered = s.sendCurrentHyperlinkChatToRecipients(session, recipients, request, sourceNameBytes)
	} else {
		if request.hasItemHyperlinks() {
			s.logGameEvent(session, "game-chat-item-links-dropped",
				"mode", request.Mode, "msg_id", currentPeerChatNotifyOpcode, "item_links", request.ItemCount)
		}
		delivered = s.sendCurrentMegaphoneToRecipients(session, recipients, request.Mode, notice, sourceNameBytes, request.Message)
	}
	s.logGameEvent(session, kind.broadcastEvent,
		"source_char_id", session.selectedCharacterID,
		"material_slot", result.Slot,
		"material_remaining", result.Remaining,
		"material_removed", result.Removed,
		"message_len", len(request.Message),
		"recipients", delivered,
		"scope", kind.scope,
		"channel_id", session.channel.ID,
		"robot_identity_projection", session.robotSessionBound,
		"inventory_refresh", kind.refreshNote)
	s.recordChat(session, request.Mode, currentTownChatModeName(request.Mode), request.Message, delivered, int(request.ItemCount),
		"ok", "", map[string]any{
			"scope":                     kind.scope,
			"notification":              currentPeerChatNotifyOpcode,
			"material_item_id":          kind.itemID,
			"material_slot":             result.Slot,
			"material_remaining":        result.Remaining,
			"material_removed":          result.Removed,
			"robot_identity_projection": session.robotSessionBound,
		})
	return nil
}

// currentChatSessionsOnChannel keeps the sessions attached to one channel.  The
// transport channel is the right boundary here, unlike area chat: 频道喇叭 must
// still reach a listener standing in a private Seria room or reading the auction
// house, who is deliberately absent from every town-area roster. Being audible
// and being visible are different questions.
func currentChatSessionsOnChannel(sessions []*gameSession, channelID int) []*gameSession {
	filtered := make([]*gameSession, 0, len(sessions))
	for _, candidate := range sessions {
		if candidate == nil || candidate.channel.ID != channelID {
			continue
		}
		filtered = append(filtered, candidate)
	}
	return filtered
}

func (s *Service) sendCurrentMegaphoneToRecipients(sender *gameSession, recipients []*gameSession, mode byte, senderBody, senderName, message []byte) int {
	if sender == nil || !sender.robotSessionBound {
		return s.sendCurrentChatPacketToRecipients(sender, recipients, currentPeerChatNotifyOpcode, senderBody)
	}

	// The current EXE renders megaphone names as interactive player identities.
	// Automatic actors have native sessions, but their generated identity is not
	// backed by the complete peer-menu/social context expected by that click
	// path.  Clicking such a retained horn row exits the client before it emits
	// any request.  Keep the source robot's native row so its local megaphone
	// mirror is debited, while every other client receives the same horn mode
	// with no interactive identity.  The visible name is preserved in the
	// message instead of being supplied as a clickable sender field.
	passiveBody := buildCurrentPassiveRobotMegaphoneNoticeBody(mode, byte(sender.channel.ID), senderName, message)
	delivered := 0
	for _, recipient := range recipients {
		if recipient == nil {
			continue
		}
		body := passiveBody
		if recipient == sender {
			body = senderBody
		}
		if err := s.sendGameUpperRawClassCodec(recipient, currentPeerChatNotifyOpcode, body, 0, true); err != nil {
			s.logGameEvent(sender, "game-chat-recipient-write-failed", "recipient_char_id", recipient.selectedCharacterID, "msg_id", currentPeerChatNotifyOpcode, "error", err)
			continue
		}
		delivered++
	}
	return delivered
}

func buildCurrentPassiveRobotMegaphoneNoticeBody(mode byte, channelID byte, senderName, message []byte) []byte {
	visibleMessage := append([]byte(nil), message...)
	const separator = ": "
	if len(senderName)+len(separator)+len(message) <= currentTownChatMaxMessageBytes {
		visibleMessage = make([]byte, 0, len(senderName)+len(separator)+len(message))
		visibleMessage = append(visibleMessage, senderName...)
		visibleMessage = append(visibleMessage, separator...)
		visibleMessage = append(visibleMessage, message...)
	}
	return buildCurrentPeerChatNoticeBody(mode, channelID, 0, nil, visibleMessage)
}

func (s *Service) rejectCurrentMegaphone(session *gameSession, kind currentMegaphoneKind, request currentTownChatRequest, err error) error {
	s.logGameEvent(session, kind.rejectedEvent, "source_char_id", session.selectedCharacterID, "material_slot", request.Argument, "message_len", len(request.Message), "error", err)
	s.recordChat(session, request.Mode, currentTownChatModeName(request.Mode), request.Message, 0, int(request.ItemCount),
		"rejected", err.Error(), map[string]any{
			"scope":            kind.scope,
			"material_item_id": kind.itemID,
			"material_slot":    request.Argument,
		})
	return s.sendGameUpperFailure(session, currentPeerChatStateOpcode, currentTownChatGenericError)
}

func (s *Service) currentOnlineChatSessions() []*gameSession {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	result := make([]*gameSession, 0, len(s.gameSessions))
	for characterID, candidate := range s.gameSessions {
		if candidate != nil && characterID != 0 && candidate.selectedCharacterID == characterID {
			result = append(result, candidate)
		}
	}
	return result
}

func (s *Service) currentChatSessionsForCharacterIDs(characterIDs []uint16) []*gameSession {
	seen := make(map[*gameSession]struct{}, len(characterIDs))
	result := make([]*gameSession, 0, len(characterIDs))
	for _, characterID := range characterIDs {
		identity, online := s.onlineGameSessionCharacterSnapshot(characterID)
		if !online {
			continue
		}
		if _, duplicate := seen[identity.session]; duplicate {
			continue
		}
		seen[identity.session] = struct{}{}
		result = append(result, identity.session)
	}
	return result
}

func (s *Service) sendCurrentChatPacketToRecipients(sender *gameSession, recipients []*gameSession, msgID uint16, body []byte) int {
	delivered := 0
	for _, recipient := range recipients {
		if recipient == nil {
			continue
		}
		if err := s.sendGameUpperRawClassCodec(recipient, msgID, body, 0, true); err != nil {
			s.logGameEvent(sender, "game-chat-recipient-write-failed", "recipient_char_id", recipient.selectedCharacterID, "msg_id", msgID, "error", err)
			continue
		}
		delivered++
	}
	return delivered
}

func buildCurrentTownChatNotification(mode byte, senderCharacterID uint16, message []byte) []byte {
	body := make([]byte, 1+2+1+4+len(message))
	body[0] = mode
	binary.LittleEndian.PutUint16(body[1:3], currentSceneActorObjectKey(senderCharacterID))
	body[3] = 0
	binary.LittleEndian.PutUint32(body[4:8], uint32(len(message)))
	copy(body[8:], message)
	return body
}

// buildCurrentRaidAnnouncementBody builds the class-0 notification sub_1D88DE0
// renders as the "- 公告 -" banner.
//
// The handler reads u8 flag, u16 actorKey, u8 channel and then the text, so the
// layout matches op12 byte for byte; only the first byte differs in meaning, and
// zero is what selects the banner branch. The actor key is the same scene object
// key op12 carries, or the client cannot resolve the speaker.
func buildCurrentRaidAnnouncementBody(senderCharacterID uint16, channel byte, message []byte) []byte {
	body := make([]byte, 1+2+1+4+len(message))
	body[0] = 0
	binary.LittleEndian.PutUint16(body[1:3], currentSceneActorObjectKey(senderCharacterID))
	body[3] = channel
	binary.LittleEndian.PutUint32(body[4:8], uint32(len(message)))
	copy(body[8:], message)
	return body
}
