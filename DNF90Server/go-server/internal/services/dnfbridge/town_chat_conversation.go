package dnfbridge

import (
	"context"
	"math"
	"strconv"
)

// This file is the semantic half of the 86 reference's 1:1 chat
// (86A21代码 …/Network/Handlers/ChatHandler.cs and ChatHandler.Conversations.cs),
// rebuilt on the current client's wire. Everything the reference does with
// packets is done here as well; only the packets themselves differ, and each
// difference is called out where it bites:
//
//   - The reference opens a window with a CREATE_GROUP notification that carries a
//     server-allocated conversation id ("status=0 | u32 id | u8 2 | dstr 自己名 |
//     dstr 对方名"), addresses every message by that id, and closes the window
//     with LEAVE_FROM_GROUP / LEAVE_USER_FROM_GROUP. The 90CN client has no such
//     field in any of its chat packets (spec §2.2 / §2.8): its request is
//     `u8 mode | u16 peer | u32 0 | u32 len | msg | u32 nameLen | name | u8`, and
//     its line packet op65 is `u8 mode | u8 status | dstr name | u8 flag |
//     dstr text | dstr extra@0x24`. So the conversation id never travels: the
//     conversation entity below *is* the routing table, the same role the
//     reference gives its `_activeConversations` dictionary.
//   - Live capture 2026-09-22 20:16:02 (`packet_log.txt`): the client whose window
//     was opened by an incoming line sends `u16 peer = 0` and an empty name,
//     while the client that picked its peer from the friend menu sends
//     `u16 peer = 22` and the peer's name. Routing therefore has to fall back to
//     the conversation this server remembers for that session — exactly the
//     reason the reference keeps the mapping on the server.
//   - The reference does not echo the line back to the sender ("The sender's
//     window already displays its own message"). This server used to echo it with
//     the *peer's* name, which is why the sender's own line came back describing a
//     conversation with the peer.
//
// What follows the reference one for one: the unordered pair key, the
// server-allocated id with its own collision skipping, one window per pair,
// "publish to a member at most once", closing on either side's leave or session
// end, "a closed conversation's traffic is inert", and the guards (self, offline,
// 256-byte text, membership).

// currentChatConversationMaxTextBytes is the reference's MaximumMessageBytes. The
// client's own op65 reader caps the text dstr at 0x100 for the same reason, so a
// longer line would be truncated on arrival.
const currentChatConversationMaxTextBytes = 256

// Body byte 4 of op65 is the selector the client registers the packet's name under
// (sub_22E85A0(flag, &name)) and the only argument that differs between the
// renderer's two exits. Live evidence says it is also what decides *which window*
// the line lands in: sending one copy with 1 split a single conversation into two
// windows ("the lines that arrived" and "the lines I sent", screenshots
// 2026-09-22 20:5x), so this server keeps every line of a conversation on 0.
const currentChatIncomingLineFlag byte = 0

// currentChatTargetFromConversation marks a peer that came from this server's own
// record rather than from the request (see resolveCurrentOneToOneChatTarget).
const currentChatTargetFromConversation = "conversation_member"

// currentChatConversationMember is one side of a window. `published` is the
// reference's Published flag: a member has been sent a line in this conversation,
// which is what makes an otherwise targetless request ("peer id 0, empty name")
// trustworthy enough to route.
type currentChatConversationMember struct {
	session     *gameSession
	characterID uint16
	name        string
	published   bool
}

// currentChatConversation is the reference's Conversation: one window between two
// characters, keyed by the unordered pair so both sides address the same entity.
type currentChatConversation struct {
	id     uint32
	key    uint64
	first  *currentChatConversationMember
	second *currentChatConversationMember
}

func currentChatConversationKey(first, second uint16) uint64 {
	low, high := first, second
	if low > high {
		low, high = high, low
	}
	return uint64(low)<<16 | uint64(high)
}

func (c *currentChatConversation) member(session *gameSession) *currentChatConversationMember {
	if c == nil || session == nil {
		return nil
	}
	if c.first.session == session {
		return c.first
	}
	if c.second.session == session {
		return c.second
	}
	return nil
}

func (c *currentChatConversation) other(member *currentChatConversationMember) *currentChatConversationMember {
	if c == nil || member == nil {
		return nil
	}
	if member == c.first {
		return c.second
	}
	if member == c.second {
		return c.first
	}
	return nil
}

// openCurrentChatConversation returns the window for this pair, creating it when
// there is none. Reuse is deliberate: the reference's self-test pins
// "concurrent duplicate opens do not create duplicate native windows", and a
// second message in the same window must not disturb the first.
//
// This client has one window at a time, so both members' previous conversations
// (with anybody else) are dropped and returned for the caller to report.
func (s *Service) openCurrentChatConversation(actor, peer *gameSession) (*currentChatConversation, bool, []*currentChatConversation) {
	if s == nil || actor == nil || peer == nil {
		return nil, false, nil
	}
	actorID := actor.selectedCharacterID
	peerID := peer.selectedCharacterID
	if actorID == 0 || peerID == 0 || actorID == peerID {
		return nil, false, nil
	}
	key := currentChatConversationKey(actorID, peerID)
	s.chatMu.Lock()
	defer s.chatMu.Unlock()
	if s.chatConversations == nil {
		s.chatConversations = make(map[uint64]*currentChatConversation)
	}
	if s.chatConversationByMember == nil {
		s.chatConversationByMember = make(map[uint16]*currentChatConversation)
	}
	if existing, ok := s.chatConversations[key]; ok {
		if existing.member(actor) != nil && existing.member(peer) != nil {
			return existing, false, nil
		}
		s.removeCurrentChatConversationLocked(existing)
	}
	var dropped []*currentChatConversation
	for _, characterID := range []uint16{actorID, peerID} {
		previous, ok := s.chatConversationByMember[characterID]
		if !ok || previous == nil {
			continue
		}
		s.removeCurrentChatConversationLocked(previous)
		dropped = append(dropped, previous)
	}
	conversation := &currentChatConversation{
		id:     s.allocateCurrentChatConversationIDLocked(),
		key:    key,
		first:  &currentChatConversationMember{session: actor, characterID: actorID},
		second: &currentChatConversationMember{session: peer, characterID: peerID},
	}
	s.chatConversations[key] = conversation
	s.chatConversationByMember[actorID] = conversation
	s.chatConversationByMember[peerID] = conversation
	return conversation, true, dropped
}

// currentChatConversationForSession is this server's answer to "who is in this
// client's chat window" — the mapping the reference keeps because its own client
// request cannot carry one either (§2.8).
func (s *Service) currentChatConversationForSession(session *gameSession) (*currentChatConversation, *currentChatConversationMember, bool) {
	if s == nil || session == nil || session.selectedCharacterID == 0 {
		return nil, nil, false
	}
	s.chatMu.Lock()
	defer s.chatMu.Unlock()
	conversation, ok := s.chatConversationByMember[session.selectedCharacterID]
	if !ok || conversation == nil {
		return nil, nil, false
	}
	member := conversation.member(session)
	if member == nil {
		return nil, nil, false
	}
	return conversation, member, true
}

// markCurrentChatConversationPublished marks both members as reached: the reference
// publishes a member before any line arrives, and a member that was never published
// is what makes an otherwise targetless reply untrustworthy.
func (s *Service) markCurrentChatConversationPublished(conversation *currentChatConversation) {
	if s == nil || conversation == nil {
		return
	}
	s.chatMu.Lock()
	defer s.chatMu.Unlock()
	if live, ok := s.chatConversations[conversation.key]; !ok || live != conversation {
		return
	}
	for _, member := range []*currentChatConversationMember{conversation.first, conversation.second} {
		if member != nil {
			member.published = true
		}
	}
}

// closeCurrentChatConversationsForSession drops every window this session owns.
// The reference registers this on SessionEnding so a disconnect cannot leave the
// other side talking into a dead conversation.
func (s *Service) closeCurrentChatConversationsForSession(session *gameSession) []*currentChatConversation {
	if s == nil || session == nil || session.selectedCharacterID == 0 {
		return nil
	}
	s.chatMu.Lock()
	defer s.chatMu.Unlock()
	conversation, ok := s.chatConversationByMember[session.selectedCharacterID]
	if !ok || conversation == nil || conversation.member(session) == nil {
		return nil
	}
	s.removeCurrentChatConversationLocked(conversation)
	return []*currentChatConversation{conversation}
}

func (s *Service) removeCurrentChatConversationLocked(conversation *currentChatConversation) {
	if s == nil || conversation == nil {
		return
	}
	if live, ok := s.chatConversations[conversation.key]; ok && live == conversation {
		delete(s.chatConversations, conversation.key)
	}
	for _, member := range []*currentChatConversationMember{conversation.first, conversation.second} {
		if member == nil {
			continue
		}
		if live, ok := s.chatConversationByMember[member.characterID]; ok && live == conversation {
			delete(s.chatConversationByMember, member.characterID)
		}
	}
}

// allocateCurrentChatConversationIDLocked mirrors the reference's allocator: ids
// start at 1 and never repeat within the live set, 0 and MaxUint32 stay unused so
// neither can be mistaken for "no conversation" on the wire.
func (s *Service) allocateCurrentChatConversationIDLocked() uint32 {
	if s.chatConversationSeq == 0 {
		s.chatConversationSeq = 1
	}
	for s.chatConversationSeq == 0 || s.chatConversationSeq == math.MaxUint32 ||
		s.currentChatConversationIDInUseLocked(s.chatConversationSeq) {
		s.chatConversationSeq++
	}
	id := s.chatConversationSeq
	s.chatConversationSeq++
	return id
}

func (s *Service) currentChatConversationIDInUseLocked(id uint32) bool {
	for _, conversation := range s.chatConversations {
		if conversation != nil && conversation.id == id {
			return true
		}
	}
	return false
}

// sendCurrentChatConversationLine writes one conversation line to a reader. This
// is the port's only wire-specific piece: the reference's MESSAGE_GROUP_CHAT
// carries `u32 id | dstr sender | dstr text` and ours carries the op65 shape from
// spec §2.2, with the *speaker's* name — the reference routes "by membership and
// server sender name, not supplied name or UID".
func (s *Service) sendCurrentChatConversationLine(reader *gameSession, mode, flag byte, speakerName, peerName string, text []byte) error {
	if reader == nil {
		return nil
	}
	body := buildCurrentOneToOneChatStateBody(mode, flag, rosterNameBytes(speakerName), rosterNameBytes(peerName), text)
	return s.sendGameUpperRawClassCodec(reader, currentTownChatOneToOneStateMsgID, body, 0, true)
}

// resolveCurrentOneToOneChatTarget answers "who is this line for" in the same
// order the reference does, minus the id it cannot receive here:
//
//  1. the peer id the request carries, when the player picked the peer;
//  2. the peer name it carries, for a client that only knows the name;
//  3. the conversation this server already remembers for that session — the case
//     a client whose window was opened by an incoming line falls into.
func (s *Service) resolveCurrentOneToOneChatTarget(
	session *gameSession,
	request currentTownChatRequest,
) (uint16, *gameSession, string, bool) {
	if request.TargetUniqueID != 0 && request.TargetUniqueID != currentWhisperTargetUnknownID {
		if target, online := s.onlineGameSession(request.TargetUniqueID); online && target != nil && target != session {
			return request.TargetUniqueID, target, "request_peer_id", true
		}
		return 0, nil, "", false
	}
	if len(request.TargetName) > 0 {
		if targetID, target, found := s.currentChatSessionByName(session, request.TargetName); found {
			return targetID, target, "request_peer_name", true
		}
	}
	conversation, member, ok := s.currentChatConversationForSession(session)
	if !ok || !member.published {
		return 0, nil, "", false
	}
	peer := conversation.other(member)
	if peer == nil || peer.characterID == 0 {
		return 0, nil, "", false
	}
	if target, online := s.onlineGameSession(peer.characterID); online && target != nil && target != session {
		return peer.characterID, target, currentChatTargetFromConversation, true
	}
	return 0, nil, "", false
}

// handleCurrentOneToOneChatMessage owns the two 1:1 window modes — 0x21 (the
// one-to-one window) and 0x24 (the peer menu). Every other private mode keeps the
// historical op17/op118 path byte for byte; see handleCurrentWhisperChat.
func (s *Service) handleCurrentOneToOneChatMessage(session *gameSession, request currentTownChatRequest) error {
	sourceID := session.selectedCharacterID
	if sourceID == 0 {
		return nil
	}
	if len(request.Message) == 0 || len(request.Message) > currentChatConversationMaxTextBytes {
		return s.rejectCurrentOneToOneChatRequest(session, request, sourceID, 0, "message_length_out_of_range")
	}
	targetID, targetSession, resolvedBy, resolved := s.resolveCurrentOneToOneChatTarget(session, request)
	if !resolved || targetID == sourceID || targetSession == nil || targetSession == session {
		return s.rejectCurrentOneToOneChatRequest(session, request, sourceID, targetID, "target_unresolved_or_offline")
	}
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	loadedSourceID, sourceName, _, sourceFound := s.characterForEnter(ctx, session, sourceID)
	loadedTargetID, targetName, _, targetFound := s.characterForEnter(ctx, targetSession, targetID)
	if !sourceFound || loadedSourceID != sourceID || !targetFound || loadedTargetID != targetID {
		return s.rejectCurrentOneToOneChatRequest(session, request, sourceID, targetID, "repository_identity_mismatch")
	}
	// A peer that came from this server's own record was never named by the
	// request, so there is nothing to compare; a peer the request *did* name is
	// checked, because that name is the only signal that the window the player
	// typed into is the target we resolved.
	if resolvedBy != currentChatTargetFromConversation &&
		!currentChatNameMatches(request.TargetName, rosterNameBytes(targetName)) {
		return s.rejectCurrentOneToOneChatRequest(session, request, sourceID, targetID, "target_name_mismatch")
	}
	// Blacklist gate, both directions like the 86 reference's IsBlocked checks. The
	// client filters blocked lines silently on its own side, which is why the sender
	// used to see nothing at all; this is the missing feedback.
	blockedByTarget := s.currentBlacklistHas(ctx, targetID, sourceName)
	blockedBySource := s.currentBlacklistHas(ctx, sourceID, targetName)
	if blockedByTarget || blockedBySource {
		// Rolled back 2026-09-22 23:4x: reporting the peer's block first (to reach dstr
		// 4763 through the whisper table) is not what the operator wants, so the
		// original precedence -- the sender's own block wins -- is restored.
		status := currentBlacklistStatusTargetBlockedSender
		if blockedBySource {
			status = currentBlacklistStatusSenderBlockedTarget
		}
		s.logGameEvent(session, "game-peer-chat-blocked",
			"source_char_id", sourceID, "target_char_id", targetID,
			"blocked_by_target", blockedByTarget, "blocked_by_source", blockedBySource,
			"status", status)
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
	conversation, created, dropped := s.openCurrentChatConversation(session, targetSession)
	if conversation == nil {
		return s.rejectCurrentOneToOneChatRequest(session, request, sourceID, targetID, "conversation_unavailable")
	}
	for _, previous := range dropped {
		s.logGameEvent(session, "game-peer-chat-conversation-replaced",
			"conversation_id", previous.id,
			"source_char_id", sourceID,
			"reason", "one_chat_window_per_client")
	}
	// The window binding (class-1 op330) is deliberately not sent. Live test
	// 2026-09-22 21:0x: it opened an *extra empty* window on the receiving side and
	// never merged with the window the op65 lines use, while the lines themselves
	// already create and reuse one window per side. The packet and what it does are
	// recorded in spec 9.6 so it can come back once the client's window key is
	// understood.
	s.markCurrentChatConversationPublished(conversation)
	// ② Two copies of the same line, one per reader, with the same flag: this
	// client opens a *separate window per flag value*, which is what the
	// 2026-09-22 20:5x screenshots showed -- four lines of one conversation split
	// into "the lines that arrived" and "the lines I sent", while the two copies
	// already carried the same name. Every line of a conversation therefore travels
	// with flag 0, and the name it carries is the other party, which is what the
	// window is titled with and what the client's name registry is filled from.
	//
	// The sender's copy has to be sent at all because this client does not draw what
	// the player typed (measured: the sender's window stays empty without it).
	if err := s.sendCurrentChatConversationLine(targetSession, request.Mode, currentChatIncomingLineFlag, sourceName, sourceName, request.Message); err != nil {
		return err
	}

	// The sender's own copy names the *speaker* (the player), so the line reads as
	// theirs. Naming the peer here was the other half of the live report: "A 发消息
	// 显示的是 B 的名字，B 发消息显示的是 A 的名字".
	if err := s.sendCurrentChatConversationLine(session, request.Mode, currentChatIncomingLineFlag, sourceName, targetName, request.Message); err != nil {
		return err
	}
	s.logGameEvent(session, "game-peer-chat-forwarded", "source_char_id", sourceID, "target_char_id", targetID, "target_name", targetName, "message_len", len(request.Message), "scope", "online_cross_channel", "notification", currentTownChatOneToOneStateMsgID, "item_links", request.ItemCount)
	s.logGameEvent(session, "game-peer-chat-conversation-advanced",
		"conversation_id", conversation.id,
		"created", created,
		"resolved_by", resolvedBy,
		"source_char_id", sourceID,
		"target_char_id", targetID,
		"line_flag", currentChatIncomingLineFlag)
	s.recordChat(session, request.Mode, currentTownChatModeName(request.Mode), request.Message, 1, int(request.ItemCount),
		"ok", "", map[string]any{
			"scope":               "online_cross_channel",
			"notification":        currentTownChatOneToOneStateMsgID,
			"target_character_id": strconv.Itoa(int(targetID)),
			"target_name":         targetName,
			"target_channel_id":   targetSession.channel.ID,
			"conversation_id":     strconv.FormatUint(uint64(conversation.id), 10),
			"resolved_by":         resolvedBy,
		})
	return nil
}

// rejectCurrentOneToOneChatRequest answers a 1:1 line that could not be routed.
// The reference answers a failed CREATE_GROUP with a single status byte to the
// requester; the current client's equivalent is the failure ack on the shared
// chat opcode, which is what a typed whisper already gets for the same reasons.
func (s *Service) rejectCurrentOneToOneChatRequest(
	session *gameSession,
	request currentTownChatRequest,
	sourceID, targetID uint16,
	reason string,
) error {
	s.logGameEvent(session, "game-peer-chat-request-rejected",
		"source_char_id", sourceID,
		"target_char_id", targetID,
		"requested_target_id", request.TargetUniqueID,
		"target_name", string(request.TargetName),
		"reason", reason)
	s.recordChat(session, request.Mode, currentTownChatModeName(request.Mode), request.Message, 0, int(request.ItemCount),
		"rejected", reason, map[string]any{
			"target_character_id": strconv.Itoa(int(targetID)),
			"target_name":         chatRecordText(request.TargetName),
		})
	return s.sendGameUpperFailure(session, currentPeerChatStateOpcode, currentTownChatGenericError)
}
