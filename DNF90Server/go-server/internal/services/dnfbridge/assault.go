package dnfbridge

import (
	"encoding/binary"
	"errors"
	"fmt"
	"time"

	"golang.org/x/text/encoding/simplifiedchinese"
	"longheng.io/server/internal/modules/dnf/dnfenum"
	"longheng.io/server/internal/modules/dnf/party"
)

// The native street controller has its own roster and load barrier. It must
// never create a dungeon party or a normal PVP room (sub_1D49550).
type currentAssault struct {
	members         [2]*currentPVPMember
	loaded          [2]bool
	connected       [2]bool
	mode            byte
	started         bool
	ended           bool
	closed          bool
	duration        time.Duration
	timerGeneration uint64
}

func isCurrentAssaultMode(mode byte) bool { return mode == 4 || mode == 13 }

func (s *Service) sendCurrentAssaultFailure(session *gameSession, mode, code byte) error {
	// sub_1D12480 reads a mode byte after the failure code, including on the
	// requester's unsolicited decline notification, and closes the wait dialog.
	return s.sendGameUpperRaw(session, 11, []byte{0, code, mode})
}

func (s *Service) respondCurrentAssault(acceptor, inviter *gameSession, req party.ResponsePeerRequest) error {
	if req.Value == 0 {
		if err := s.sendGameUpperSuccess(acceptor, 11, party.BuildResponsePeerAckPayload(req.TargetID, req.Mode)); err != nil {
			return err
		}
		s.logGameEvent(acceptor, "game-assault-declined", "inviter", req.TargetID, "mode", req.Mode)
		return s.sendCurrentAssaultFailure(inviter, req.Mode, 0xeb)
	}
	match, err := s.prepareCurrentAssault(inviter, acceptor, req.Mode)
	if err != nil {
		s.logGameEvent(acceptor, "game-assault-start-rejected", "error", err)
		return errors.Join(s.sendCurrentAssaultFailure(acceptor, req.Mode, 0x5f), s.sendCurrentAssaultFailure(inviter, req.Mode, 0x5f))
	}
	s.pvpMu.Lock()
	defer s.pvpMu.Unlock()
	// Recheck after loading persisted actors: another response or reconnect may
	// have changed either session while the repository was being read.
	if !s.currentAssaultPeersAvailable(inviter, acceptor) {
		return errors.Join(s.sendCurrentAssaultFailure(acceptor, req.Mode, 0x5f), s.sendCurrentAssaultFailure(inviter, req.Mode, 0x5f))
	}
	var endpoints [2]party.PeerEndpoint
	for i, member := range match.members {
		registration := currentPartyPeerEndpointSnapshot(member.session)
		routingKey, mtu := s.partyTransportIdentity(member.session)
		if registration.Port == 0 || registration.OuterIPv4 == [4]byte{} || !currentPartyPathMTU(mtu) {
			return errors.Join(s.sendCurrentAssaultFailure(acceptor, req.Mode, 0x5f), s.sendCurrentAssaultFailure(inviter, req.Mode, 0x5f))
		}
		endpoints[i] = party.PeerEndpoint{UserID: member.characterID, InnerIPv4: registration.InnerIPv4,
			OuterIPv4: registration.OuterIPv4, Port: registration.Port, RoutingKey: routingKey,
			NATType: 1, PathMTU: mtu, UserState: 100}
	}
	for _, member := range match.members {
		member.session.assault = match
	}
	if err := s.sendGameUpperSuccess(acceptor, 11, party.BuildResponsePeerAckPayload(req.TargetID, req.Mode)); err != nil {
		s.closeCurrentAssaultLocked(match, nil)
		return err
	}
	for i, member := range match.members {
		peer := match.members[1-i]
		for _, body := range [][]byte{peer.mode0, peer.mode1, member.mode1} {
			if err := s.sendGameUpperRawClass(member.session, uint16(dnfenum.CmdPacketSetUDPIPPort), body, 0); err != nil {
				s.closeCurrentAssaultLocked(match, nil)
				return err
			}
		}
		if err := s.sendGameUpperRawClass(member.session, 11, party.BuildPeerEndpointInfo([]party.PeerEndpoint{endpoints[1-i]}), 0); err != nil {
			s.closeCurrentAssaultLocked(match, nil)
			return err
		}
	}
	if err := s.broadcastCurrentAssaultLocked(match, 128, buildCurrentAssaultMoveBody(match)); err != nil {
		s.closeCurrentAssaultLocked(match, nil)
		return err
	}
	s.logGameEvent(acceptor, "game-assault-entered", "inviter", req.TargetID, "mode", req.Mode,
		"sequence", "op2_actors_op11_endpoints_op128_street_roster")
	return s.scheduleCurrentAssaultCloseLocked(match, 60*time.Second)
}

func (s *Service) currentAssaultPeersAvailable(inviter, acceptor *gameSession) bool {
	if inviter == nil || acceptor == nil || inviter == acceptor || inviter.channel.ID != acceptor.channel.ID ||
		inviter.assault != nil || acceptor.assault != nil || inviter.pvpRoom != nil || acceptor.pvpRoom != nil ||
		s.onlinePlayers == nil || !s.onlinePlayers.PeerInSameArea(inviter.selectedCharacterID, acceptor.selectedCharacterID) {
		return false
	}
	for _, peer := range []*gameSession{inviter, acceptor} {
		if _, bound := s.boundGameSessionCharacterSnapshot(peer); !bound || hasMultiMemberRuntimeParty(peer) {
			return false
		}
		peer.dungeon.mu.Lock()
		inDungeon := peer.dungeon.runtime != nil
		peer.dungeon.mu.Unlock()
		if inDungeon {
			return false
		}
	}
	return true
}

func (s *Service) prepareCurrentAssault(inviter, acceptor *gameSession, mode byte) (*currentAssault, error) {
	s.pvpMu.Lock()
	available := s.currentAssaultPeersAvailable(inviter, acceptor)
	s.pvpMu.Unlock()
	if !available {
		return nil, fmt.Errorf("street challenge requires two available solo characters in the same area")
	}
	match := &currentAssault{mode: mode}
	for i, peer := range []*gameSession{inviter, acceptor} {
		member, err := s.loadCurrentPVPMember(peer)
		if err != nil {
			return nil, err
		}
		match.members[i] = member
	}
	a, b := match.members[0].position, match.members[1].position
	if a.TownID != b.TownID || a.AreaID != b.AreaID {
		return nil, fmt.Errorf("street challenge town positions changed")
	}
	s.initialEquipmentMu.Lock()
	archive, err := s.initialEquipmentArchiveLocked()
	s.initialEquipmentMu.Unlock()
	if err == nil {
		// sub_33C9E50 uses the ordinary PVP duration (dword_50E3998).
		match.duration, err = currentPVPMinimumMatchDuration(archive, 1, 1)
	}
	return match, err
}

func buildCurrentAssaultMoveBody(match *currentAssault) []byte {
	var w packetWriter
	p := match.members[0].position
	w.writeUint32(uint32(match.members[0].characterID))
	w.writeByte(p.TownID)
	w.writeByte(p.AreaID)
	w.writeByte(match.mode)
	w.writeUint16(p.PositionX)
	w.writeUint16(p.PositionY)
	w.writeByte(2)
	for seat, member := range match.members {
		w.writeByte(byte(seat))
		w.writeByte(byte(seat + 1)) // native teams 1 and 2, sub_26C6250
		w.writeUint16(member.characterID)
	}
	w.writeByte(0) // no spectators
	return w.bytes()
}

func (s *Service) handleCurrentAssaultCommand(session *gameSession, opcode uint16, body []byte) (bool, error) {
	switch dnfenum.CmdPacket(opcode) {
	case dnfenum.CmdPacketCompleteLoadAssault, dnfenum.CmdPacketConnectP2pAssault,
		dnfenum.CmdPacketDieAssaultPlayer, dnfenum.CmdPacketEvadeAssault,
		dnfenum.CmdPacketLoadCompleteAfterAssault, dnfenum.CmdPacketConnectP2pAfterAssault:
	default:
		return false, nil
	}
	if session == nil {
		return true, nil
	}
	if _, bound := s.boundGameSessionCharacterSnapshot(session); !bound {
		return true, nil
	}
	s.pvpMu.Lock()
	defer s.pvpMu.Unlock()
	match := session.assault
	if match == nil {
		return true, nil
	}
	seat := -1
	for i, member := range match.members {
		if member.session == session && member.characterID == session.selectedCharacterID {
			seat = i
		}
	}
	if seat < 0 {
		return true, nil
	}
	if match.closed {
		if dnfenum.CmdPacket(opcode) != dnfenum.CmdPacketLoadCompleteAfterAssault || len(body) != 0 {
			return true, nil
		}
		// op24 destroys the old town actors. Recreate peers only after this
		// receiver's native sub_1D901D0 acknowledges the new scene with op244.
		for _, peer := range s.onlinePlayers.PeersInSameArea(session.selectedCharacterID) {
			if peer.Session == nil || peer.Session.channel.ID != session.channel.ID {
				continue
			}
			if err := s.sendTownRemoteActorState(session, peer, "assault_return_peer_actor"); err != nil {
				return true, err
			}
			area := buildCurrentTownUserAreaNotificationBody(peer.CharacterID, peer.TownID, peer.AreaID,
				peer.PositionX, peer.PositionY, peer.Direction, peer.AreaState)
			if err := s.sendCurrentSceneFixedClass0Packet(session, currentTownUserAreaNotificationMsgID, area, "assault_return_peer_area"); err != nil {
				return true, err
			}
		}
		session.assault = nil
		s.logGameEvent(session, "game-assault-town-peers-restored", "character_id", session.selectedCharacterID)
		return true, nil
	}
	switch dnfenum.CmdPacket(opcode) {
	case dnfenum.CmdPacketCompleteLoadAssault:
		if len(body) != 0 || match.started || match.ended || match.loaded[seat] {
			return true, nil
		}
		match.loaded[seat] = true
		var notice [2]byte
		binary.LittleEndian.PutUint16(notice[:], session.selectedCharacterID)
		return true, s.broadcastCurrentAssaultLocked(match, 129, notice[:])
	case dnfenum.CmdPacketConnectP2pAssault:
		// sub_33C6F40 writes (sub_2FB65F0(seat) == 0): wire 1 means
		// connected. The native internal state 0 is the working P2P state.
		if len(body) != 17 || body[0] != 8 || !match.loaded[seat] || match.started || match.ended {
			return true, nil
		}
		var seen [8]bool
		peerConnected := false
		for i := 1; i < len(body); i += 2 {
			other, status := body[i], body[i+1]
			if other >= 8 || seen[other] || status > 1 {
				return true, nil
			}
			seen[other] = true
			if int(other) == 1-seat {
				peerConnected = status == 1
			}
		}
		s.logGameEvent(session, "game-assault-p2p-report", "seat", seat, "peer_connected", peerConnected, "body", fmt.Sprintf("%x", body))
		if !peerConnected {
			s.logGameEvent(session, "game-assault-aborted", "reason", "peer_connection_failed", "peer_seat", 1-seat)
			s.closeCurrentAssaultLocked(match, nil)
			return true, nil
		}
		match.connected[seat] = true
		if !match.loaded[0] || !match.loaded[1] || !match.connected[0] || !match.connected[1] {
			return true, nil
		}
		match.started = true
		if err := s.broadcastCurrentAssaultLocked(match, 130, nil); err != nil {
			s.closeCurrentAssaultLocked(match, nil)
			return true, err
		}
		s.logGameEvent(session, "game-assault-started", "inviter", match.members[0].characterID)
		return true, s.scheduleCurrentAssaultCloseLocked(match, match.duration+5*time.Second)
	case dnfenum.CmdPacketDieAssaultPlayer:
		// sub_24A7470: killer seat, two u32 counters, four u16 coordinates.
		if len(body) != 17 || !match.started || match.ended || (body[0] != 0xff && int(body[0]) != 1-seat) {
			return true, nil
		}
		match.ended = true
		// The final death ends this two-player match. op131 calls
		// sub_1FA9FA0(3) and opens the receiver's coin UI, even for the winner.
		end := []byte{0}
		returnDelay := 3 * time.Second
		if match.mode == 13 {
			end = []byte{1, byte(1 - seat), 0, 0}
			returnDelay = 11 * time.Second // native result state 5 lasts 9999ms
		}
		if err := s.broadcastCurrentAssaultLocked(match, 132, end); err != nil {
			s.closeCurrentAssaultLocked(match, nil)
			return true, err
		}
		winner, loser := match.members[1-seat], match.members[seat]
		title := "\u8857\u5934\u4e89\u9738"
		if match.mode == 13 {
			title = "\u516c\u4f1a\u64c2\u53f0\u8d5b"
		}
		message := fmt.Sprintf("%s\u7ed3\u675f\uff1a%s \u6218\u80dc\u4e86 %s\u3002", title, winner.name, loser.name)
		encoded, err := simplifiedchinese.GB18030.NewEncoder().Bytes([]byte(message))
		if err != nil {
			s.closeCurrentAssaultLocked(match, nil)
			return true, err
		}
		// Native op12 mode 0 is a system notice (sub_1D821E0/sub_272BC80).
		if err := s.broadcastCurrentAssaultLocked(match, currentTownChatNotificationMsgID, buildCurrentTownChatNotification(0, winner.characterID, encoded)); err != nil {
			s.closeCurrentAssaultLocked(match, nil)
			return true, err
		}
		s.logGameEvent(session, "game-assault-ended", "winner", winner.characterID, "loser", loser.characterID, "return_after", returnDelay)
		return true, s.scheduleCurrentAssaultCloseLocked(match, returnDelay)
	case dnfenum.CmdPacketEvadeAssault:
		if len(body) == 0 {
			s.closeCurrentAssaultLocked(match, nil)
		}
	}
	return true, nil
}

func (s *Service) broadcastCurrentAssaultLocked(match *currentAssault, opcode uint16, body []byte) error {
	var result error
	for _, member := range match.members {
		result = errors.Join(result, s.sendGameUpperRawClass(member.session, opcode, body, 0))
	}
	return result
}

func (s *Service) scheduleCurrentAssaultCloseLocked(match *currentAssault, delay time.Duration) error {
	name := fmt.Sprintf("dnf-assault:%p", match)
	queue := s.ensureGameplayTimeQueue()
	queue.Cancel(name)
	match.timerGeneration++
	generation := match.timerGeneration
	err := queue.ScheduleAfter(name, delay, func(time.Time) {
		s.pvpMu.Lock()
		defer s.pvpMu.Unlock()
		if !match.closed && match.timerGeneration == generation && match.members[0].session.assault == match && match.members[1].session.assault == match {
			s.closeCurrentAssaultLocked(match, nil)
		}
	})
	if err != nil {
		s.closeCurrentAssaultLocked(match, nil)
	}
	return err
}

func (s *Service) closeCurrentAssaultLocked(match *currentAssault, disconnecting *gameSession) {
	if match.closed {
		return
	}
	match.closed = true
	s.ensureGameplayTimeQueue().Cancel(fmt.Sprintf("dnf-assault:%p", match))
	for _, member := range match.members {
		if member.session.assault != match {
			continue
		}
		if member.session == disconnecting {
			member.session.assault = nil
			continue
		}
		// sub_1D3C5D0 clears the actor's death state and closes coin dialog 91
		// before sub_1D3A360 releases the street scene, including for the loser.
		var restored [2]byte
		binary.LittleEndian.PutUint16(restored[:], member.characterID)
		err := s.sendGameUpperRawClass(member.session, 133, restored[:], 0)
		// sub_1D3A360 reads one stamina byte, then frees the street roster.
		// op135 (sub_1D38A10) is a different, actor-id-based leave notice.
		err = errors.Join(err, s.sendGameUpperRawClass(member.session, 137, []byte{member.stamina}, 0))
		err = errors.Join(err, s.returnCurrentPVPMemberToTown(member))
		s.logGameEvent(member.session, "game-assault-closed", "error", err)
	}
}
