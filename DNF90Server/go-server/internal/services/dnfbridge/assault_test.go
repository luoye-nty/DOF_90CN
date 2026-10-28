package dnfbridge

import (
	"bytes"
	"encoding/binary"
	"os"
	"strconv"
	"testing"
	"time"

	"golang.org/x/text/encoding/simplifiedchinese"
	dnfproto "longheng.io/server/internal/modules/dnf/protocol"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

func assaultTestPeers(t *testing.T) (*Service, [2]*gameSession, *fakeCurrentDungeonDeathTimerQueue) {
	t.Helper()
	s := pvpRoomTestService(t)
	s.onlinePlayers = newOnlinePlayerManager()
	queue := newFakeCurrentDungeonDeathTimerQueue()
	s.gameplayTimers = queue
	var peers [2]*gameSession
	for i := range peers {
		id := uint16(29 + i)
		peer := &gameSession{conn: &bufferConn{}, selectedCharacterID: id, accountID: "pvp-" + strconv.Itoa(int(id)),
			channel: s.catalog.Channels()[0], sceneBootstrapTailSent: true,
			townPositionSnapshot: currentTownPositionSnapshot{CharacterID: id, TownID: 10, AreaID: 0,
				PositionX: 474, PositionY: 234, MovementCode: 5, PositionValid: true},
			partyPeer: currentPartyPeerEndpointRegistration{InnerIPv4: [4]byte{127, 0, 0, 1},
				OuterIPv4: [4]byte{127, 0, 0, 1}, Port: 12000 + id, MTU: 512}}
		s.bindGameSessionCharacter(peer, id)
		s.onlinePlayers.EnterArea(&onlinePlayerInfo{CharacterID: id, TownID: 10, AreaID: 0, Session: peer})
		peers[i] = peer
	}
	return s, peers, queue
}

func assaultTestDispatch(t *testing.T, s *Service, peer *gameSession, route string, opcode uint16, body []byte) {
	t.Helper()
	var err error
	if route == "legacy" {
		err = s.handleGameCommand(peer, 1, opcode, body)
	} else {
		wire, buildErr := dnfproto.BuildChannelPacket(opcode, body, 0, 1)
		if buildErr != nil {
			t.Fatal(buildErr)
		}
		err = s.handleGameUpper(peer, wire)
	}
	if err != nil {
		t.Fatal(err)
	}
}

func assaultTestTake(t *testing.T, peer *gameSession) []dnfproto.ChannelPacket {
	t.Helper()
	conn := peer.conn.(*bufferConn)
	stream := append([]byte(nil), conn.write.Bytes()...)
	conn.write.Reset()
	var packets []dnfproto.ChannelPacket
	for len(stream) > 0 {
		packet, rest := splitCurrentGameServerUpperPacketAuto(t, stream)
		packets = append(packets, packet)
		stream = rest
	}
	return packets
}

func TestAssaultNativeInviteAndDecline(t *testing.T) {
	for _, route := range []string{"legacy", "upper"} {
		for _, mode := range []byte{4, 13} {
			s, peers, _ := assaultTestPeers(t)
			a, b := peers[0], peers[1]
			assaultTestDispatch(t, s, a, route, 10, []byte{30, 0, mode, 0, 0, 0, 0, 0, 0, 0, 0})
			invite, wait := assaultTestTake(t, b), assaultTestTake(t, a)
			if len(invite) != 1 || invite[0].Header.Classification != 0 || invite[0].Header.MsgID != 443 || !bytes.Equal(invite[0].Body, []byte{29, 0, mode}) {
				t.Fatalf("%s mode=%d invitation=%+v", route, mode, invite)
			}
			if len(wait) != 2 || wait[0].Header.MsgID != 10 || wait[1].Header.MsgID != 444 || !bytes.Equal(wait[1].Body, []byte{30, 0, mode}) {
				t.Fatalf("requester wait=%+v", wait)
			}
			// Invalid values must not consume the real pending request.
			assaultTestDispatch(t, s, b, route, 11, []byte{29, 0, mode, 2, 0, 0, 0})
			if len(assaultTestTake(t, b)) != 0 || len(assaultTestTake(t, a)) != 0 {
				t.Fatal("invalid answer emitted a reply")
			}
			assaultTestDispatch(t, s, b, route, 11, []byte{29, 0, mode, 0, 0, 0, 0})
			ack, decline := assaultTestTake(t, b), assaultTestTake(t, a)
			if len(ack) != 1 || !bytes.Equal(ack[0].Body, []byte{1, 29, 0, mode}) ||
				len(decline) != 1 || !bytes.Equal(decline[0].Body, []byte{0, 0xeb, mode}) {
				t.Fatalf("decline ack=%+v requester=%+v", ack, decline)
			}
			assaultTestDispatch(t, s, b, route, 11, []byte{29, 0, mode, 1, 0, 0, 0})
			if len(assaultTestTake(t, b)) != 0 || a.assault != nil || b.assault != nil || a.pvpRoom != nil || hasMultiMemberRuntimeParty(a) {
				t.Fatal("consumed street invite created another activity")
			}
		}
	}
}

func TestAssaultLoadBarrierDeathAndCleanup(t *testing.T) {
	for _, route := range []string{"legacy", "upper"} {
		for _, mode := range []byte{4, 13} {
			s, peers, queue := assaultTestPeers(t)
			match := &currentAssault{mode: mode, duration: 180 * time.Second}
			for i, peer := range peers {
				match.members[i] = &currentPVPMember{session: peer, characterID: peer.selectedCharacterID, name: "fighter" + strconv.Itoa(i), position: peer.townPositionSnapshot, stamina: byte(90 + i)}
				peer.assault = match
			}
			wantMove := []byte{29, 0, 0, 0, 10, 0, mode, 218, 1, 234, 0, 2, 0, 1, 29, 0, 1, 2, 30, 0, 0}
			if body := buildCurrentAssaultMoveBody(match); !bytes.Equal(body, wantMove) {
				t.Fatalf("native op128 layout=%x", body)
			}
			if err := s.scheduleCurrentAssaultCloseLocked(match, time.Minute); err != nil {
				t.Fatal(err)
			}
			loadingTimer := queue.task(t, 0)
			// Captured native op146 at 2026-09-06 03:21:47.614. This used
			// to close both sessions immediately because 1 was read as failure.
			connected := []byte{8, 0, 1, 1, 1, 2, 1, 3, 1, 4, 1, 5, 1, 6, 1, 7, 1}
			assaultTestDispatch(t, s, peers[0], route, 146, connected)
			if match.connected[0] {
				t.Fatal("accepted P2P report before loading")
			}
			for i, peer := range peers {
				assaultTestDispatch(t, s, peer, route, 145, nil)
				for _, recipient := range peers {
					packets := assaultTestTake(t, recipient)
					if len(packets) != 1 || packets[0].Header.MsgID != 129 || binary.LittleEndian.Uint16(packets[0].Body) != peer.selectedCharacterID {
						t.Fatalf("loaded actor notice=%+v", packets)
					}
				}
				assaultTestDispatch(t, s, peer, route, 146, connected)
				if i == 0 && match.started {
					t.Fatal("battle started before both clients were ready")
				}
			}
			for _, peer := range peers {
				packets := assaultTestTake(t, peer)
				if !match.started || len(packets) != 1 || packets[0].Header.MsgID != 130 || len(packets[0].Body) != 0 {
					t.Fatalf("native start=%+v", packets)
				}
			}
			queue.fire(loadingTimer, true)
			if match.closed {
				t.Fatal("stale load timeout closed a started match")
			}
			death := make([]byte, 17)
			death[0] = 1 // a victim cannot name its own seat as killer
			assaultTestDispatch(t, s, peers[1], route, 147, death)
			if match.ended {
				t.Fatal("accepted invalid death report")
			}
			// B's native death report captured at 2026-09-06 03:34:13.422.
			death = []byte{0, 0x16, 0, 0, 0, 0xc2, 0x7b, 9, 0, 0xa2, 9, 5, 1, 0x86, 0x0a, 0xfc, 0}
			assaultTestDispatch(t, s, peers[1], route, 147, death)
			for _, peer := range peers {
				packets := assaultTestTake(t, peer)
				wantEnd := []byte{0}
				if mode == 13 {
					wantEnd = []byte{1, 0, 0, 0}
				}
				if !match.ended || len(packets) != 2 || packets[0].Header.MsgID != 132 || !bytes.Equal(packets[0].Body, wantEnd) || packets[1].Header.MsgID != 12 {
					t.Fatalf("native death/result=%+v", packets)
				}
				notice := packets[1].Body
				// 参考实现按模式给不同标题：mode 13 是公会擂台赛，其余是街头争霸。
				wantTitle := "街头争霸"
				if mode == 13 {
					wantTitle = "公会擂台赛"
				}
				wantMessage := wantTitle + "结束：fighter0 战胜了 fighter1。"
				if len(notice) < 8 || !bytes.Equal(notice[:4], []byte{0, 29, 0, 0}) || int(binary.LittleEndian.Uint32(notice[4:8])) != len(notice)-8 {
					t.Fatalf("missing native system victory notice: %x", notice)
				}
				message, err := simplifiedchinese.GB18030.NewDecoder().String(string(notice[8:]))
				if err != nil || message != wantMessage {
					t.Fatalf("victory notice=%q err=%v", message, err)
				}
			}
			assaultTestDispatch(t, s, peers[1], route, 147, death)
			if len(assaultTestTake(t, peers[0])) != 0 {
				t.Fatal("duplicate death emitted another result")
			}
			returnTimer := queue.task(t, 2)
			wantDelay := 3 * time.Second
			if mode == 13 {
				wantDelay = 11 * time.Second
			}
			if returnTimer.delay != wantDelay {
				t.Fatalf("return delay=%v want %v", returnTimer.delay, wantDelay)
			}
			queue.fire(returnTimer, false)
			for i, peer := range peers {
				packets := assaultTestTake(t, peer)
				closeBody := []byte{byte(90 + i)}
				if peer.assault != match || !match.closed || len(packets) != 5 || packets[0].Header.MsgID != 133 || binary.LittleEndian.Uint16(packets[0].Body) != peer.selectedCharacterID || packets[1].Header.MsgID != 137 || !bytes.Equal(packets[1].Body, closeBody) || packets[3].Header.MsgID != 24 {
					t.Fatalf("native street close/town restore=%+v", packets)
				}
			}
		}
	}
}

func TestAssaultP2PFailureAndMalformedReports(t *testing.T) {
	for _, test := range []struct {
		name      string
		body      []byte
		wantClose bool
		wantReady bool
	}{
		{"failed_peer", []byte{8, 0, 1, 1, 0, 2, 1, 3, 1, 4, 1, 5, 1, 6, 1, 7, 1}, true, false},
		{"unused_seats", []byte{8, 0, 1, 1, 1, 2, 0, 3, 0, 4, 0, 5, 0, 6, 0, 7, 0}, false, true},
		{"duplicate_seat", []byte{8, 0, 1, 1, 0, 2, 1, 3, 1, 4, 1, 5, 1, 6, 1, 6, 1}, false, false},
		{"invalid_status", []byte{8, 0, 1, 1, 0, 2, 1, 3, 1, 4, 1, 5, 1, 6, 1, 7, 2}, false, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			s, peers, _ := assaultTestPeers(t)
			match := &currentAssault{mode: 4, loaded: [2]bool{true, true}}
			for i, peer := range peers {
				match.members[i] = &currentPVPMember{session: peer, characterID: peer.selectedCharacterID, position: peer.townPositionSnapshot, stamina: 100}
				peer.assault = match
			}
			assaultTestDispatch(t, s, peers[0], "legacy", 146, test.body)
			if match.closed != test.wantClose || match.connected[0] != test.wantReady || match.started {
				t.Fatalf("closed=%v connected=%v started=%v", match.closed, match.connected, match.started)
			}
			for _, peer := range peers {
				packets := assaultTestTake(t, peer)
				if test.wantClose {
					if peer.assault != match || len(packets) != 5 || packets[0].Header.MsgID != 133 || packets[1].Header.MsgID != 137 || !bytes.Equal(packets[1].Body, []byte{100}) {
						t.Fatalf("failed P2P did not restore town: %+v", packets)
					}
				} else if len(packets) != 0 || peer.assault != match {
					t.Fatalf("report unexpectedly changed the street scene: %+v", packets)
				}
			}
		})
	}
}

func TestRealPVFAssaultAccept(t *testing.T) {
	path := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if path == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE to exercise persisted actor loading and native street entry")
	}
	archive, err := platformpvf.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, route := range []string{"legacy", "upper"} {
		s, peers, _ := assaultTestPeers(t)
		s.initialEquipmentArchive = archive
		assaultTestDispatch(t, s, peers[0], route, 10, []byte{30, 0, 4, 0, 0, 0, 0, 0, 0, 0, 0})
		assaultTestTake(t, peers[0])
		assaultTestTake(t, peers[1])
		assaultTestDispatch(t, s, peers[1], route, 11, []byte{29, 0, 4, 1, 0, 0, 0})
		for i, peer := range peers {
			packets := assaultTestTake(t, peer)
			want := []uint16{2, 2, 2, 11, 128}
			if i == 1 {
				want = append([]uint16{11}, want...)
			}
			if peer.assault == nil || hasMultiMemberRuntimeParty(peer) || peer.pvpRoom != nil || len(packets) != len(want) {
				t.Fatalf("%s accept did not enter street fight: packets=%+v", route, packets)
			}
			for j, opcode := range want {
				if packets[j].Header.MsgID != opcode {
					t.Fatalf("street entry[%d]=%d want %d", j, packets[j].Header.MsgID, opcode)
				}
			}
		}
		if peers[0].assault != peers[1].assault || peers[0].assault.duration <= 0 {
			t.Fatal("street fight did not share a PVF-backed match")
		}
		s.closeCurrentPVPSession(peers[0])
		packets := assaultTestTake(t, peers[1])
		if peers[0].assault != nil || peers[1].assault == nil || !peers[1].assault.closed || len(packets) != 5 || packets[0].Header.MsgID != 133 || packets[1].Header.MsgID != 137 {
			t.Fatalf("disconnect left other character in street fight: %+v", packets)
		}
	}
}

func TestAssaultTownPeersAfterLoadComplete(t *testing.T) {
	for _, route := range []string{"legacy", "upper"} {
		t.Run(route, func(t *testing.T) {
			s, peers, _ := assaultTestPeers(t)
			s.options.gameUpperHeader = gameUpperHeaderServer16
			if path := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE"); path != "" {
				archive, err := platformpvf.Open(path)
				if err != nil {
					t.Fatal(err)
				}
				s.initialEquipmentArchive = archive
			}
			match := &currentAssault{mode: 4}
			for i, peer := range peers {
				peer.townActorOwnerChannel = byte(21 + i)
				match.members[i] = &currentPVPMember{session: peer, characterID: peer.selectedCharacterID, position: peer.townPositionSnapshot, stamina: 100}
				peer.assault = match
			}
			for _, peer := range peers {
				assaultTestDispatch(t, s, peer, route, 244, nil)
				if len(assaultTestTake(t, peer)) != 0 || peer.assault != match {
					t.Fatal("early town ACK changed an active match")
				}
			}
			s.closeCurrentAssaultLocked(match, nil)
			for _, peer := range peers {
				packets := assaultTestTake(t, peer)
				if len(packets) != 5 || packets[3].Header.MsgID != 24 {
					t.Fatalf("town return sent actors before load completed: %+v", packets)
				}
			}
			for i, peer := range peers {
				assaultTestDispatch(t, s, peer, route, 244, []byte{1})
				if len(assaultTestTake(t, peer)) != 0 || peer.assault != match {
					t.Fatal("malformed town ACK consumed pending return")
				}
				assaultTestDispatch(t, s, peer, route, 244, nil)
				actor, _ := s.onlinePlayers.PlayerForCharacter(peers[1-i].selectedCharacterID)
				conn := peer.conn.(*bufferConn)
				assertTownRemoteActorSequenceWithMode1(t, conn.write.Bytes(), peer, actor, true)
				conn.write.Reset()
				if peer.assault != nil || len(assaultTestTake(t, peers[1-i])) != 0 {
					t.Fatal("town ACK did not finish only its receiving session")
				}
				assaultTestDispatch(t, s, peer, route, 244, nil)
				if len(assaultTestTake(t, peer)) != 0 {
					t.Fatal("duplicate town ACK recreated actors")
				}
			}
			if !s.currentAssaultPeersAvailable(peers[0], peers[1]) {
				t.Fatal("completed return blocked the next street challenge")
			}
		})
	}
}

func TestAssaultPendingReturnClearedOnSessionRetirement(t *testing.T) {
	for _, retire := range []string{"disconnect", "character_change"} {
		t.Run(retire, func(t *testing.T) {
			s, peers, _ := assaultTestPeers(t)
			match := &currentAssault{mode: 4}
			for i, peer := range peers {
				peer.assault = match
				match.members[i] = &currentPVPMember{session: peer, characterID: peer.selectedCharacterID, position: peer.townPositionSnapshot}
			}
			s.closeCurrentAssaultLocked(match, nil)
			for _, peer := range peers {
				assaultTestTake(t, peer)
			}
			if retire == "disconnect" {
				s.closeCurrentPVPSession(peers[0])
			} else {
				s.bindGameSessionCharacter(peers[0], 31)
			}
			if peers[0].assault != nil || peers[1].assault != match {
				t.Fatal("session retirement left pending state or canceled the peer's return")
			}
			assaultTestDispatch(t, s, peers[0], "legacy", 244, nil)
			if len(assaultTestTake(t, peers[0])) != 0 {
				t.Fatal("stale town ACK recreated the previous character's scene")
			}
		})
	}
}
