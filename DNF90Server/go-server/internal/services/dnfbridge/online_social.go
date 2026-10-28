package dnfbridge

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"os"
	"strconv"
	"strings"

	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfguild "longheng.io/server/internal/modules/dnf/guild"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnffriend "longheng.io/server/internal/modules/dnf/unitedfriend"
)

const (
	currentPeerChatStateOpcode  = uint16(17)
	currentPeerChatNotifyOpcode = uint16(118)
	currentFriendNotifyOpcode   = uint16(274)
	currentFriendListOpcode     = uint16(273)
	currentFriendAddOpcode      = uint16(290)
	currentFriendDeleteOpcode   = uint16(291)
	currentDstrMaximumLength    = 4096
	defaultCurrentRegion        = byte(1)
	currentFriendFailureMissing = byte(0x15)
	currentFriendFailureSelf    = byte(0x01)
	currentFriendFailureFull    = byte(0x04)
	currentFriendFailureDelete  = byte(0x05)
)

var errCurrentSocialBodyInvalid = errors.New("current social command body is invalid")

type currentPeerChatRequest struct {
	MessageType byte
	TargetID    uint16
	Unknown     uint32
	Message     []byte
	TargetName  []byte
}

type currentFriendAddRequest struct {
	TargetID   uint16
	Operation  byte
	TargetName []byte
}

type currentFriendDeleteRequest struct {
	Operation  byte
	TargetID   uint32
	TargetName []byte
}

type currentFriendPresence struct {
	ChannelID uint16
	Busy      bool
}

type currentFriendPresenceOverride struct {
	Name     string
	Presence currentFriendPresence
}

func (s *Service) handleOnlineSocialCommand(session *gameSession, typ uint16, body []byte) (bool, error) {
	switch typ {
	case currentPeerChatStateOpcode:
		// Live current-client op17 owns every chat mode (whisper, party,
		// area, guild, megaphone and raid) on both legacy and aligned
		// transports. Keep one decoder/owner so an aligned whisper and a
		// legacy town message cannot diverge.
		return true, s.handleCurrentTownChat(session, body)
	case uint16(dnfenum.CmdPacketItemHyperlinkMessage):
		// op424 is op17 with linked items appended. Same owner, same audience
		// rules; only the notification differs.
		return true, s.handleCurrentTownChatHyperlink(session, body)
	case currentFriendAddOpcode:
		return true, s.handleCurrentFriendAdd(session, typ, body)
	case currentFriendDeleteOpcode:
		return true, s.handleCurrentFriendDelete(session, typ, body)
	case uint16(dnfenum.CmdPacketRegisiterToBlacklist), uint16(dnfenum.CmdPacketDeleteToBlacklist):
		return true, s.handleCurrentBlacklistMutation(session, typ, body)
	default:
		return false, nil
	}
}

func (s *Service) handleCurrentPeerChat(session *gameSession, typ uint16, body []byte) error {
	request, err := decodeCurrentPeerChatRequest(body)
	if err != nil {
		s.logGameEvent(session, "game-peer-chat-request-rejected", "body_len", len(body), "error", err)
		return nil
	}
	sourceID := selectedCharacterID(session)
	if !s.currentSocialPeerAvailable(sourceID, request.TargetID) {
		s.logGameEvent(session, "game-peer-chat-request-rejected",
			"source_char_id", sourceID,
			"target_char_id", request.TargetID,
			"message_type", request.MessageType,
			"reason", "target_not_online_in_same_area")
		return nil
	}
	targetSession, online := s.onlineGameSession(request.TargetID)
	if !online {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	loadedSourceID, sourceName, _, sourceFound := s.characterForEnter(ctx, session, sourceID)
	loadedTargetID, targetName, _, targetFound := s.characterForEnter(ctx, targetSession, request.TargetID)
	if !sourceFound || loadedSourceID != sourceID || !targetFound || loadedTargetID != request.TargetID {
		s.logGameEvent(session, "game-peer-chat-request-rejected",
			"source_char_id", sourceID,
			"target_char_id", request.TargetID,
			"reason", "repository_identity_unavailable")
		return nil
	}
	if len(request.Message) == 0 {
		return nil
	}
	if err := s.sendGameUpperSuccess(session, typ, nil); err != nil {
		return err
	}
	peerState := buildCurrentPeerChatStateBody(request.TargetID, request.MessageType)
	if err := s.sendGameUpperRawClassCodec(session, currentPeerChatStateOpcode, peerState, 0, true); err != nil {
		return err
	}
	incomingState := buildCurrentPeerChatStateBody(sourceID, request.MessageType)
	if err := s.sendGameUpperRawClassCodec(targetSession, currentPeerChatStateOpcode, incomingState, 0, true); err != nil {
		return err
	}
	notice := buildCurrentPeerChatNoticeBody(
		request.MessageType,
		byte(targetSession.channel.ID),
		sourceID,
		rosterNameBytes(sourceName),
		request.Message,
	)
	if err := s.sendGameUpperRawClassCodec(targetSession, currentPeerChatNotifyOpcode, notice, 0, true); err != nil {
		return err
	}
	s.logGameEvent(session, "game-peer-chat-forwarded",
		"source_char_id", sourceID,
		"target_char_id", request.TargetID,
		"target_name", targetName,
		"message_type", request.MessageType,
		"message_len", len(request.Message),
		"body_source", "current_exe_class1_op17_ack_class0_op17_state_class0_op118_message")
	return nil
}

func (s *Service) handleCurrentFriendAdd(session *gameSession, typ uint16, body []byte) error {
	request, err := decodeCurrentFriendAddRequest(body)
	if err != nil {
		s.logGameEvent(session, "game-friend-add-request-rejected", "body_len", len(body), "error", err)
		return s.sendGameUpperFailure(session, typ, currentFriendFailureMissing)
	}
	sourceID := selectedCharacterID(session)
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	_, sourceName, _, found := s.selectedCharacterForEnter(ctx, session)
	if !found || sourceName == "" {
		return s.sendGameUpperFailure(session, typ, currentFriendFailureMissing)
	}
	targetName, err := decodeCharacterName(request.TargetName)
	if err != nil {
		return s.sendGameUpperFailure(session, typ, currentFriendFailureMissing)
	}
	repositories, ok := s.repositoryGroup()
	if !ok {
		return s.sendGameUpperFailure(session, typ, currentFriendFailureMissing)
	}
	owner, err := dnffriend.NewOwner(repositories, nil)
	if err != nil {
		return s.sendGameUpperFailure(session, typ, currentFriendFailureMissing)
	}
	s.friendMu.Lock()
	friend, err := owner.Add(ctx, sourceName, sourceID, targetName, request.TargetID)
	s.friendMu.Unlock()
	if err != nil {
		code := currentFriendFailureMissing
		switch {
		case errors.Is(err, dnffriend.ErrSelf):
			code = currentFriendFailureSelf
		case errors.Is(err, dnffriend.ErrFull):
			code = currentFriendFailureFull
		}
		return s.sendGameUpperFailure(session, typ, code)
	}
	if err := s.sendGameUpperSuccess(session, typ, buildCurrentFriendSuccessBody()); err != nil {
		return err
	}
	// Current NoPack's op273 parser marks an already-owned scene actor as a
	// friend while installing the absolute list. Do not replay mode0/mode1/op9
	// here: recreating an actor that is already represented by the party HUD can
	// overwrite its live HP/MP presentation, and cross-area creation is the old
	// source of the residual white-shadow object.
	if err := s.sendCurrentUnitedFriendList(session, sourceName, "friend_add_committed"); err != nil {
		return err
	}
	s.logGameEvent(session, "game-friend-add-response-send",
		"source_char_id", sourceID,
		"target_char_id", request.TargetID,
		"target_name", friend.Character.Name,
		"body_source", "current_exe_class1_op290_13b_ack_then_class0_op273_double_refresh")
	return nil
}

func (s *Service) handleCurrentFriendDelete(session *gameSession, typ uint16, body []byte) error {
	request, err := decodeCurrentFriendDeleteRequest(body)
	if err != nil {
		return s.sendGameUpperFailure(session, typ, currentFriendFailureMissing)
	}
	targetName, err := decodeCharacterName(request.TargetName)
	if err != nil {
		return s.sendGameUpperFailure(session, typ, currentFriendFailureMissing)
	}
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	_, sourceName, _, found := s.selectedCharacterForEnter(ctx, session)
	if !found || sourceName == "" {
		return s.sendGameUpperFailure(session, typ, currentFriendFailureMissing)
	}
	repositories, ok := s.repositoryGroup()
	if !ok {
		return s.sendGameUpperFailure(session, typ, currentFriendFailureMissing)
	}
	owner, err := dnffriend.NewOwner(repositories, nil)
	if err != nil {
		return s.sendGameUpperFailure(session, typ, currentFriendFailureMissing)
	}
	s.friendMu.Lock()
	err = owner.Remove(ctx, sourceName, targetName)
	s.friendMu.Unlock()
	if err != nil {
		code := currentFriendFailureMissing
		if errors.Is(err, dnffriend.ErrRelationNotFound) {
			code = currentFriendFailureDelete
		}
		return s.sendGameUpperFailure(session, typ, code)
	}
	if err := s.sendGameUpperSuccess(session, typ, buildCurrentFriendSuccessBody()); err != nil {
		return err
	}
	if err := s.sendGameUpperRawClassCodec(session, currentFriendListOpcode, buildCurrentUnitedFriendDeleteBody(s.currentRegionID(), targetName), 0, true); err != nil {
		return err
	}
	return s.sendCurrentUnitedFriendList(session, sourceName, "friend_delete_committed")
}

func (s *Service) currentRegionID() byte {
	if s != nil && s.options.regionID > 0 && s.options.regionID < 250 {
		return byte(s.options.regionID)
	}
	return defaultCurrentRegion
}

func (s *Service) handleCurrentBlacklistMutation(session *gameSession, typ uint16, body []byte) error {
	// This is the upper/class-1 transport's entry for 118/119 (an unlisted class-1
	// command falls through to the aligned owner). It used to answer by echoing the
	// name and storing nothing, so a block made over that transport never reached the
	// store and the list came back empty after a relog. Route both transports through
	// the ONE storing owner -- the same rule the chat path follows -- and keep the
	// historic echo only when there is nothing to persist (no character or no store),
	// so the client still receives its ack.
	if s.currentBlacklistCanStore(session) {
		switch typ {
		case uint16(dnfenum.CmdPacketRegisiterToBlacklist):
			return s.handleCurrentBlacklistRegister(session, body)
		case uint16(dnfenum.CmdPacketDeleteToBlacklist):
			return s.handleCurrentBlacklistDelete(session, body)
		}
	}
	name, next, ok := readCurrentRawDstr(body, 0)
	if !ok || next != len(body) || len(name) == 0 {
		s.logGameEvent(session, "game-blacklist-mutation-rejected",
			"type", typ,
			"body_len", len(body),
			"reason", "exact_name_dstr_required")
		return nil
	}
	var response packetWriter
	response.writeRawDstr(name)
	s.logGameEvent(session, "game-blacklist-mutation-ack",
		"type", typ,
		"selected_character_id", selectedCharacterID(session),
		"name_len", len(name),
		"body_source", "current_exe_class1_callback_reads_echoed_name_dstr")
	return s.sendGameUpperSuccess(session, typ, response.bytes())
}

func (s *Service) currentSocialPeerAvailable(sourceID, targetID uint16) bool {
	return sourceID != 0 && targetID != 0 && sourceID != targetID &&
		s != nil && s.onlinePlayers != nil && s.onlinePlayers.PeerInSameArea(sourceID, targetID)
}

func decodeCurrentPeerChatRequest(body []byte) (currentPeerChatRequest, error) {
	if len(body) < 15 {
		return currentPeerChatRequest{}, errCurrentSocialBodyInvalid
	}
	request := currentPeerChatRequest{
		MessageType: body[0],
		TargetID:    binary.LittleEndian.Uint16(body[1:3]),
		Unknown:     binary.LittleEndian.Uint32(body[3:7]),
	}
	message, next, ok := readCurrentRawDstr(body, 7)
	if !ok {
		return currentPeerChatRequest{}, errCurrentSocialBodyInvalid
	}
	targetName, next, ok := readCurrentRawDstr(body, next)
	if !ok || next != len(body) || request.TargetID == 0 || len(targetName) == 0 {
		return currentPeerChatRequest{}, errCurrentSocialBodyInvalid
	}
	request.Message = message
	request.TargetName = targetName
	return request, nil
}

func decodeCurrentFriendAddRequest(body []byte) (currentFriendAddRequest, error) {
	if len(body) < 7 {
		return currentFriendAddRequest{}, errCurrentSocialBodyInvalid
	}
	request := currentFriendAddRequest{TargetID: binary.LittleEndian.Uint16(body[:2]), Operation: body[2]}
	name, next, ok := readCurrentRawDstr(body, 3)
	if !ok || next != len(body) || request.TargetID == 0 || len(name) == 0 {
		return currentFriendAddRequest{}, errCurrentSocialBodyInvalid
	}
	request.TargetName = name
	return request, nil
}

func decodeCurrentFriendDeleteRequest(body []byte) (currentFriendDeleteRequest, error) {
	if len(body) < 6 {
		return currentFriendDeleteRequest{}, errCurrentSocialBodyInvalid
	}
	name := bytes.TrimRight(body[5:], "\x00")
	if len(name) == 0 {
		return currentFriendDeleteRequest{}, errCurrentSocialBodyInvalid
	}
	return currentFriendDeleteRequest{
		Operation:  body[0],
		TargetID:   binary.LittleEndian.Uint32(body[1:5]),
		TargetName: append([]byte(nil), name...),
	}, nil
}

func readCurrentRawDstr(body []byte, offset int) ([]byte, int, bool) {
	if offset < 0 || offset > len(body)-4 {
		return nil, offset, false
	}
	length := int32(binary.LittleEndian.Uint32(body[offset : offset+4]))
	if length < 0 || length > currentDstrMaximumLength {
		return nil, offset, false
	}
	start := offset + 4
	end := start + int(length)
	if end < start || end > len(body) {
		return nil, offset, false
	}
	return append([]byte(nil), body[start:end]...), end, true
}

func buildCurrentPeerChatStateBody(peerID uint16, messageType byte) []byte {
	var writer packetWriter
	writer.writeUint16(peerID)
	writer.writeByte(messageType)
	return writer.bytes()
}

func buildCurrentPeerChatNoticeBody(messageType, channelID byte, senderID uint16, senderName, message []byte) []byte {
	var writer packetWriter
	writer.writeByte(messageType)
	writer.writeByte(channelID)
	writer.writeByte(0)
	writer.writeByte(channelID)
	writer.writeUint16(senderID)
	writer.writeRawDstr(senderName)
	writer.writeRawDstr(message)
	return writer.bytes()
}

func buildCurrentFriendPresenceBody(region byte, presence currentFriendPresence, name string) []byte {
	var writer packetWriter
	writer.writeUint16(presence.ChannelID)
	writer.writeByte(region)
	writer.writeRawDstr(rosterNameBytes(name))
	writer.writeByte(boolByte(presence.Busy))
	return writer.bytes()
}

func buildCurrentFriendSuccessBody() []byte {
	var writer packetWriter
	writer.writeUint32(0x1bc6)
	writer.writeUint32(10)
	writer.writeUint32(2)
	return writer.bytes()
}

func buildCurrentUnitedFriendDeleteBody(region byte, name string) []byte {
	var writer packetWriter
	writer.writeUint32(2)
	writer.writeUint32(1)
	writer.writeByte(region)
	writer.writeRawDstr(rosterNameBytes(name))
	return writer.bytes()
}

func (s *Service) sendCurrentUnitedFriendList(session *gameSession, ownerName, source string) error {
	return s.sendCurrentUnitedFriendListWithOverride(session, ownerName, source, nil, nil)
}

func (s *Service) sendCurrentUnitedFriendListResumable(session *gameSession, ownerName, source string, cursor *int) error {
	return s.sendCurrentUnitedFriendListWithOverride(session, ownerName, source, cursor, nil)
}

func (s *Service) sendCurrentUnitedFriendListWithOverride(
	session *gameSession,
	ownerName string,
	source string,
	cursor *int,
	override *currentFriendPresenceOverride,
) error {
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.UnitedFriend == nil {
		return nil
	}
	owner, err := dnffriend.NewOwner(repositories, nil)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	if strings.TrimSpace(ownerName) == "" {
		_, ownerName, _, ok = s.selectedCharacterForEnter(ctx, session)
		if !ok {
			return nil
		}
	}
	list, err := owner.List(ctx, ownerName)
	if err != nil {
		return err
	}
	body := s.buildCurrentUnitedFriendListBody(session, list, override)
	next := 0
	if cursor != nil {
		next = *cursor
	}
	for next < 2 {
		if err := s.sendGameUpperRawClassCodec(session, currentFriendListOpcode, body, 0, true); err != nil {
			return err
		}
		next++
		if cursor != nil {
			*cursor = next
		}
	}
	s.logGameEvent(session, "game-united-friend-list-send", "source", source, "owner_name", ownerName, "count", len(list), "repeat", 2)
	return nil
}

func (s *Service) buildCurrentUnitedFriendListBody(
	_ *gameSession,
	list []dnffriend.Friend,
	override *currentFriendPresenceOverride,
) []byte {
	var writer packetWriter
	writer.writeUint32(0)
	writer.writeUint32(uint32(len(list)))
	for _, entry := range list {
		character := entry.Character
		id := uint16(numericCharacterStat(character.CharacterID))
		presence := s.currentUnitedFriendPresence(id)
		if override != nil && override.Name == character.Name {
			presence = override.Presence
		}
		writer.writeByte(s.currentRegionID())
		writer.writeUint16(presence.ChannelID)
		writer.writeRawDstr(rosterNameBytes(character.Name))
		writer.writeByte(byte(character.Level))
		writer.writeUint32(uint32(numericCharacterStat(character.Job)))
		writer.writeByte(byte(character.Stats["grow_type"]))
		// The state byte and the legacy A21 row's Y/Z pair are the only
		// override-able slots; without the final byte row N consumes row N+1's
		// region byte and multi-friend lists desynchronize.
		// Current NoPack sub_26E8680 consumes level,u32 job,grow,state,Y,Z,tail.
		stateByte, trailingU32, tailByte := currentUnitedFriendRowStatus(presence)
		writer.writeByte(stateByte)
		writer.writeUint32(trailingU32)
		writer.writeByte(tailByte)
	}
	return writer.bytes()
}

// currentUnitedFriendRowStateByte is the friend row's state byte.
//
// Live evidence (2026-09-11): with the historic boolByte(presence.Busy) value an
// online friend in the player's own area renders as "登录中" (logging in) instead
// of "在线" (online), and the friend window then omits every interactive item
// (邀请组队 and friends) regardless of area. The client's three states are
// 在线 / 登录中 / 不在线, so this slot is a state value rather than a boolean.
//
// A probe with the value 1 was confirmed on the wire (packet_log shows the row's
// state byte becoming 01) yet the client still rendered "登录中", so the status
// text may come from this byte, the trailing u32, or the final byte. All three
// are therefore operator-overridable, without a rebuild:
//
//	environment DNF90_FRIEND_ROW_STATE, or
//	file friend_row_state.txt, holding 1 to 3 space separated decimals:
//	    <state byte> [trailing u32] [final byte]
//
// Absent the override the historic row is sent unchanged.
func currentUnitedFriendRowStatus(presence currentFriendPresence) (byte, uint32, byte) {
	state := boolByte(presence.Busy)
	trailing := uint32(0)
	tail := byte(0)

	values := currentFriendRowStateOverrideValues()
	if len(values) > 0 {
		state = values[0]
	}
	if len(values) > 1 {
		trailing = uint32(values[1])
	}
	if len(values) > 2 {
		tail = values[2]
	}
	return state, trailing, tail
}

func currentFriendRowStateOverrideValues() []byte {
	const key = "DNF90_FRIEND_ROW_STATE"
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return nil
	}
	fields := strings.Fields(raw)
	if len(fields) > 3 {
		fields = fields[:3]
	}
	values := make([]byte, 0, len(fields))
	for _, field := range fields {
		parsed, err := strconv.Atoi(field)
		if err != nil || parsed < 0 || parsed > 255 {
			return nil
		}
		values = append(values, byte(parsed))
	}
	return values
}

func (s *Service) currentUnitedFriendPresence(characterID uint16) currentFriendPresence {
	if characterID == 0 {
		return currentFriendPresence{}
	}
	session, online := s.onlineGameSession(characterID)
	if !online || session == nil {
		return currentFriendPresence{}
	}
	_, inTown := s.onlinePlayers.PlayerForCharacter(characterID)
	return currentFriendPresence{
		ChannelID: uint16(session.channel.ID),
		Busy:      !inTown,
	}
}

// notifyCurrentUnitedFriendPresence reports which observers were told, so a
// caller that also announces to another directory can skip the ones that
// already have the line.
func (s *Service) notifyCurrentUnitedFriendPresence(friendName string, presence currentFriendPresence, source string) map[uint16]bool {
	notified := map[uint16]bool{}
	friendName = strings.TrimSpace(friendName)
	if friendName == "" {
		return notified
	}
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.UnitedFriend == nil || repositories.Character == nil {
		return notified
	}
	owner, err := dnffriend.NewOwner(repositories, nil)
	if err != nil {
		return notified
	}
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	observers, err := owner.Observers(ctx, friendName)
	if err != nil {
		s.logPacketEvent("game-united-friend-presence-observers-failed", "friend_name", friendName, "source", source, "error", err)
		return notified
	}
	for _, observerName := range observers {
		observerID, found, err := repositories.Character.FindIDByName(ctx, observerName)
		if err != nil || !found {
			continue
		}
		observerSession, online := s.onlineGameSession(uint16(numericCharacterStat(observerID)))
		if !online || observerSession == nil {
			continue
		}
		body := buildCurrentFriendPresenceBody(s.currentRegionID(), presence, friendName)
		if err := s.sendGameUpperRawClassCodec(observerSession, currentFriendNotifyOpcode, body, 0, true); err != nil {
			s.logGameEvent(observerSession, "game-united-friend-presence-send-failed", "friend_name", friendName, "source", source, "error", err)
			continue
		}
		notified[observerSession.selectedCharacterID] = true
		// Full list refresh carries changed level/job/grow/channel fields. It
		// also lets op273 update the friend bit on an actor the scene already
		// owns. Never recreate that actor from a social notification: doing so
		// corrupts party HP/MP presentation and can leave a white shadow.
		override := &currentFriendPresenceOverride{Name: friendName, Presence: presence}
		if err := s.sendCurrentUnitedFriendListWithOverride(
			observerSession,
			observerName,
			source+"_absolute_refresh",
			nil,
			override,
		); err != nil {
			s.logGameEvent(observerSession, "game-united-friend-list-refresh-failed", "friend_name", friendName, "source", source, "error", err)
		}
	}
	return notified
}

// notifyCurrentGuildMemberPresence gives guild members the same "entered
// channel N" / "left the game" line the friend list already produces, using the
// same op274 notice. Anyone already told as a friend is skipped so a guild mate
// who is also on the friend list does not read the line twice. Only the notice
// is sent: the friend-list refresh that follows it for real friends would be
// meaningless to a guild mate who has not added this character.
func (s *Service) notifyCurrentGuildMemberPresence(
	characterID uint16,
	name string,
	presence currentFriendPresence,
	source string,
	alreadyNotified map[uint16]bool,
) {
	name = strings.TrimSpace(name)
	if s == nil || characterID == 0 || name == "" {
		return
	}
	ctx, cancel := s.currentGuildContext()
	defer cancel()
	var members []uint16
	if err := s.readCurrentGuild(ctx, func(_ dnfrepo.Group, registry dnfguild.Registry) error {
		guild, _, ok := registry.GuildForCharacter(strconv.Itoa(int(characterID)))
		if !ok || guild == nil {
			return nil
		}
		for id := range guild.Members {
			if memberID := currentGuildNumericID(id); memberID != 0 {
				members = append(members, uint16(memberID))
			}
		}
		return nil
	}); err != nil {
		s.logPacketEvent("game-guild-presence-announce-load-failed", "char_id", characterID, "source", source, "error", err)
		return
	}
	body := buildCurrentFriendPresenceBody(s.currentRegionID(), presence, name)
	delivered := 0
	for _, memberID := range members {
		if memberID == characterID || alreadyNotified[memberID] {
			continue
		}
		target, online := s.onlineGameSession(memberID)
		if !online || target == nil {
			continue
		}
		if err := s.sendGameUpperRawClassCodec(target, currentFriendNotifyOpcode, body, 0, true); err != nil {
			s.logGameEvent(target, "game-guild-presence-announce-failed",
				"char_id", characterID, "source", source, "error", err)
			continue
		}
		delivered++
	}
	if delivered > 0 {
		s.logPacketEvent("game-guild-presence-announced",
			"char_id", characterID, "name", name, "channel_id", presence.ChannelID,
			"recipients", delivered, "source", source)
	}
}

// publishCurrentSocialPresence announces one character's "now online" state to
// the two directories the client keeps outside the town scene: the united
// friend list and the guild member window. It is deliberately separate from
// town co-presence, which a private Seria room legitimately suppresses.
func (s *Service) publishCurrentSocialPresence(session *gameSession, characterID uint16, name string, source string) {
	if s == nil || session == nil || characterID == 0 {
		return
	}
	presence := currentFriendPresence{ChannelID: uint16(session.channel.ID)}
	notified := s.notifyCurrentUnitedFriendPresence(name, presence, source+"_friend_online")
	s.notifyCurrentGuildMemberPresence(characterID, name, presence, source+"_guild_online", notified)
	s.publishCurrentGuildPresence(characterID, source+"_guild_online")
	// 公会积分: the client has no 签到 button and signs in automatically on the
	// first login of the day, so the grant belongs here, where a character's
	// online state is published. Registry.Attend stays idempotent per local day.
	s.grantCurrentGuildAttendance(session, source+"_guild_online_attendance")
}
