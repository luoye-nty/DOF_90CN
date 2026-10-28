package dnfbridge

import (
	"testing"
	"time"

	"longheng.io/server/internal/modules/dnf/dnfenum"
)

// Live incident 2026-09-22 20:06: two characters of one guild (1111 and 2222)
// logged in one second apart. Each session held its own townMu inside
// sendCurrentInitialTownRoute and then asked the guild presence refresh to
// project the other member's guild window, which read that peer's town state
// under the peer's townMu. Both goroutines ended up waiting for a lock the other
// one held, neither session processed another packet, and both clients stayed
// frozen in the scene they had just entered -- the Seria room door included --
// until the server was restarted. The peer refresh must therefore never wait for
// a peer's town lock.
func TestSendCurrentGuildManagementStateToPeerNeverWaitsForAPeerTownLock(t *testing.T) {
	service := &Service{options: options{gameUpperBodyCodec: gameUpperBodyCodecPlain}}
	connection := &bufferConn{}
	peer := &gameSession{conn: connection, selectedCharacterID: 16}
	peer.initialTownRouteCharacterID = 16
	peer.initialTownRouteStage = currentInitialTownRoutePlayerStateSent

	// The peer is inside its own town route: the state both members were in at
	// 20:06.
	peer.townMu.Lock()
	defer peer.townMu.Unlock()

	done := make(chan error, 1)
	go func() { done <- service.sendCurrentGuildManagementStateToPeer(peer, nil) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("busy peer refresh returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("guild presence refresh waited for a peer town lock held by that peer's own route")
	}
	if connection.write.Len() != 0 {
		t.Fatal("a peer that is mid-transition received a guild projection")
	}
}

// Only a peer that is actually mid-transition may be skipped: an idle peer still
// receives the projection the blocking form sends.
func TestSendCurrentGuildManagementStateToPeerProjectsAnIdlePeer(t *testing.T) {
	service := &Service{options: options{gameUpperBodyCodec: gameUpperBodyCodecPlain}}
	connection := &bufferConn{}
	peer := &gameSession{conn: connection, selectedCharacterID: 16}
	peer.initialTownRouteCharacterID = 16
	peer.initialTownRouteStage = currentInitialTownRoutePlayerStateSent

	if err := service.sendCurrentGuildManagementStateToPeer(peer, nil); err != nil {
		t.Fatal(err)
	}
	packet, rest := splitGameServerUpperPacket(t, connection.write.Bytes())
	if packet.Header.MsgID != 127 {
		t.Fatalf("peer projection=%+v want op127 first", packet.Header)
	}
	if len(packet.Body) != 1 || packet.Body[0] != 100 {
		t.Fatalf("peer projection body=%x want the nonmember grade sentinel 100", packet.Body)
	}
	// writeCurrentGuildManagementState now ends with the 公会徽章 aura block and the
	// DLL-private 公会经验排名, exactly where upstream puts them, so a nonmember
	// peer receives those two right after op127. This peer's guild is nil and the
	// Service has no repository provider, so there is no 公会 BUFF row and the
	// ranking still goes out with zeros.
	aura, rest := splitGameServerUpperPacket(t, rest)
	if aura.Header.MsgID != uint16(dnfenum.CmdPacketSetUDPIPPort) || !isCurrentGuildAuraBody(aura.Body) {
		t.Fatalf("peer aura=%+v body=%x want the 公会徽章 block", aura.Header, aura.Body)
	}
	ranking, rest := splitGameServerUpperPacket(t, rest)
	if ranking.Header.MsgID != 413 || len(ranking.Body) != 12 {
		t.Fatalf("peer ranking=%+v body=%x want the 12-byte 公会经验排名", ranking.Header, ranking.Body)
	}
	if len(rest) != 0 {
		t.Fatalf("peer projection trailing=%x want nothing after the ranking", rest)
	}
}
