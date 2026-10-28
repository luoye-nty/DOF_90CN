package dnfbridge

import (
	"encoding/binary"
	"net"
	"net/netip"
	"testing"
	"time"
)

// The current client probes once, right after login, and a player may sit on
// the character screen for minutes. The PID-keyed observation must therefore
// outlive the short address-keyed TTL so the late bind still restores it.
func TestCurrentPartyUDPProbePIDObservationSurvivesLateCharacterBind(t *testing.T) {
	service := &Service{
		options:        options{serverIP: "192.168.5.3"},
		gameSessions:   make(map[uint16]*gameSession),
		partyEndpoints: newCurrentPartyUDPEndpointRegistry(),
		partyUDPOwnerPID: func(netip.AddrPort) (uint32, bool) {
			return 404, true
		},
	}
	service.observeCurrentPartyUDPEndpoint(netip.MustParseAddrPort("192.168.5.3:62926"))
	service.partyEndpointMu.Lock()
	observation := service.partyEndpoints.byPID[404]
	observation.observedAt = time.Now().UTC().Add(-5 * time.Minute)
	service.partyEndpoints.byPID[404] = observation
	service.partyEndpointMu.Unlock()

	session := &gameSession{conn: &bufferConn{}, connID: "late-bind", clientPID: 404}
	service.bindGameSessionCharacter(session, 42)
	if got := currentPartyPeerPort(session); got != 62926 {
		t.Fatalf("endpoint observed five minutes before the bind was lost: port=%d want=62926", got)
	}

	service.partyEndpointMu.Lock()
	observation = service.partyEndpoints.byPID[404]
	observation.observedAt = time.Now().UTC().Add(-currentPartyUDPEndpointPIDObservationTTL - time.Minute)
	service.partyEndpoints.byPID[404] = observation
	service.partyEndpointMu.Unlock()
	expired := &gameSession{conn: &bufferConn{}, connID: "expired", clientPID: 404}
	service.bindGameSessionCharacter(expired, 43)
	if got := currentPartyPeerPort(expired); got != 0 {
		t.Fatalf("observation older than the PID TTL was restored: port=%d", got)
	}
}

func currentPartyOp2TestBody(port uint16) []byte {
	body := make([]byte, currentChannelReconnectProbeSize)
	body[0] = 1
	copy(body[1:5], []byte{192, 168, 5, 3})
	copy(body[5:9], []byte{192, 168, 5, 3})
	binary.BigEndian.PutUint16(body[9:11], port)
	binary.LittleEndian.PutUint32(body[11:15], 1472)
	binary.LittleEndian.PutUint32(body[15:19], 12)
	copy(body[19:], []byte("005056c00008"))
	return body
}

// Two clients behind one address cannot be told apart by remote IP, and the
// UDP socket owner lookup is unavailable on public addresses. Each probe port
// binds the first unbound session for that address (arrival order), matching
// the pre-merge behaviour; a probe never overwrites an existing registration,
// and the op2 fallback never accepts a port the probe has not seen.
func TestCurrentUDPProbeBindsSharedAddressDualLoginFirstUnbound(t *testing.T) {
	remote := &net.TCPAddr{IP: net.ParseIP("192.168.5.3"), Port: 51054}
	first := &gameSession{
		conn:                &currentRobotSessionTestConn{remote: remote},
		connID:              "first",
		selectedCharacterID: 1,
	}
	second := &gameSession{
		conn:                &currentRobotSessionTestConn{remote: remote},
		connID:              "second",
		selectedCharacterID: 2,
	}
	service := &Service{
		options:        options{serverIP: "10.1.0.16"},
		gameSessions:   map[uint16]*gameSession{1: first},
		partyEndpoints: newCurrentPartyUDPEndpointRegistry(),
		partyUDPOwnerPID: func(netip.AddrPort) (uint32, bool) {
			return 0, false
		},
	}
	// The first client probes while it is still the only session for the address.
	service.observeCurrentPartyUDPEndpoint(netip.MustParseAddrPort("192.168.5.3:5063"))
	if got := currentPartyPeerPort(first); got != 5063 {
		t.Fatalf("first probe endpoint=%d want=5063", got)
	}

	// The second client logs in later; its probe binds the still-unbound session
	// and must not disturb the first registration.
	service.gameSessions[2] = second
	service.observeCurrentPartyUDPEndpoint(netip.MustParseAddrPort("192.168.5.3:62926"))
	if got := currentPartyPeerPort(second); got != 62926 {
		t.Fatalf("second probe endpoint=%d want=62926", got)
	}
	if got := currentPartyPeerPort(first); got != 5063 {
		t.Fatalf("probe overwrote an existing registration: first=%d want=5063", got)
	}

	// A third observed port has no unbound session left to claim and stays idle.
	service.observeCurrentPartyUDPEndpoint(netip.MustParseAddrPort("192.168.5.3:54444"))
	if got := currentPartyPeerPort(first); got != 5063 {
		t.Fatalf("first changed after idle probe: port=%d", got)
	}
	if got := currentPartyPeerPort(second); got != 62926 {
		t.Fatalf("second changed after idle probe: port=%d", got)
	}

	// Cross-talk: an op2 naming the other client's probe-observed port must not
	// duplicate or overwrite a registration the probe already bound.
	service.captureCurrentPartyPeerEndpointFromOp2(second, currentPartyOp2TestBody(5063))
	if got := currentPartyPeerPort(first); got != 5063 {
		t.Fatalf("op2 cross-talk duplicated endpoint: first=%d", got)
	}
	if got := currentPartyPeerPort(second); got != 62926 {
		t.Fatalf("op2 cross-talk changed second: port=%d", got)
	}
}

// The op2 fallback remains a second net for a session whose probe observation
// arrived while the session did not exist and address restore cannot serve it
// (the shared address already holds several probe ports). It still accepts only
// a port the probe has observed, and remembers it under the client PID so a
// reconnect of the same process restores it at its own character bind.
func TestCurrentOp2FallbackBindsProbeObservedPortAndRestoresSamePID(t *testing.T) {
	remote := &net.TCPAddr{IP: net.ParseIP("192.168.5.3"), Port: 51054}
	first := &gameSession{
		conn:                &currentRobotSessionTestConn{remote: remote},
		connID:              "first",
		selectedCharacterID: 1,
	}
	second := &gameSession{
		conn:                &currentRobotSessionTestConn{remote: remote},
		connID:              "second",
		selectedCharacterID: 2,
	}
	service := &Service{
		options:        options{serverIP: "10.1.0.16"},
		gameSessions:   map[uint16]*gameSession{1: first, 2: second},
		partyEndpoints: newCurrentPartyUDPEndpointRegistry(),
		partyUDPOwnerPID: func(netip.AddrPort) (uint32, bool) {
			return 0, false
		},
	}
	service.observeCurrentPartyUDPEndpoint(netip.MustParseAddrPort("192.168.5.3:5063"))
	service.observeCurrentPartyUDPEndpoint(netip.MustParseAddrPort("192.168.5.3:62926"))
	// A third probe arrives while every session is bound: it is observed but
	// attributed to nobody, so address restore cannot serve a late session.
	service.observeCurrentPartyUDPEndpoint(netip.MustParseAddrPort("192.168.5.3:54444"))

	late := &gameSession{conn: &currentRobotSessionTestConn{remote: remote}, connID: "late", clientPID: 303}
	service.bindGameSessionCharacter(late, 7)
	if got := currentPartyPeerPort(late); got != 0 {
		t.Fatalf("ambiguous address restored an unattributed probe port: port=%d", got)
	}

	// A port the probe never observed stays rejected even for an unbound session.
	service.captureCurrentPartyPeerEndpointFromOp2(late, currentPartyOp2TestBody(39077))
	if got := currentPartyPeerPort(late); got != 0 {
		t.Fatalf("op2 port never observed by the probe was accepted: port=%d", got)
	}

	service.captureCurrentPartyPeerEndpointFromOp2(late, currentPartyOp2TestBody(54444))
	if got := currentPartyPeerPort(late); got != 54444 {
		t.Fatalf("op2 fallback did not bind the probe-observed port: port=%d want=54444", got)
	}

	// The cross-validated port is remembered under the client's PID so a later
	// connection of the same process restores it at its own character bind.
	reconnect := &gameSession{conn: &currentRobotSessionTestConn{remote: remote}, connID: "reconnect", clientPID: 303}
	service.bindGameSessionCharacter(reconnect, 8)
	if got := currentPartyPeerPort(reconnect); got != 54444 {
		t.Fatalf("op2-derived observation was not restored for the same PID: port=%d want=54444", got)
	}
}
