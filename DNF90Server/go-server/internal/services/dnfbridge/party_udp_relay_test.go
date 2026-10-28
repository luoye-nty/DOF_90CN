package dnfbridge

import (
	"bytes"
	"errors"
	"net"
	"os"
	"testing"
	"time"
)

func TestCurrentPartyUDPRelayDatagramTraceIsBounded(t *testing.T) {
	for count := uint64(0); count <= 1025; count++ {
		want := count >= 1 && count <= 8 || count >= 16 && count&(count-1) == 0
		if got := currentPartyUDPRelayTraceDatagram(count); got != want {
			t.Fatalf("trace count %d=%t want %t", count, got, want)
		}
	}
}

func TestCurrentPartyUDPRelayForwardsOnlyAuthenticatedDirectedLegs(t *testing.T) {
	portStart := testCurrentPartyUDPRelayPortStart(t)
	relay, err := newCurrentPartyUDPRelay(true, "127.0.0.1", portStart, testCurrentPartyUDPRelayPortSpan, nil)
	if err != nil {
		t.Fatalf("new UDP relay: %v", err)
	}
	defer func() {
		if err := relay.Close(); err != nil {
			t.Fatalf("close UDP relay: %v", err)
		}
	}()

	bindings := []currentPartyUDPRelayBinding{
		{characterID: 101, generation: 1, address: [4]byte{127, 0, 0, 1}},
		{characterID: 202, generation: 1, address: [4]byte{127, 0, 0, 1}},
	}
	if !relay.Sync(77, bindings) {
		t.Fatal("relay did not create a complete two-member room")
	}
	addressAB, portAB, ok := relay.Endpoint(77, 101, 202)
	if !ok {
		t.Fatal("missing A-to-B relay endpoint")
	}
	addressBA, portBA, ok := relay.Endpoint(77, 202, 101)
	if !ok || portAB == portBA {
		t.Fatalf("invalid directed relay endpoints: AB=%d BA=%d ok=%v", portAB, portBA, ok)
	}

	clientA := testCurrentPartyUDPClient(t, "127.0.0.1", portStart)
	defer clientA.Close()
	clientB := testCurrentPartyUDPClient(t, "127.0.0.1", portStart)
	defer clientB.Close()
	wrongSource := testCurrentPartyUDPClient(t, "127.0.0.2", portStart)
	defer wrongSource.Close()

	endpointAB := &net.UDPAddr{IP: net.IPv4(addressAB[0], addressAB[1], addressAB[2], addressAB[3]), Port: int(portAB)}
	endpointBA := &net.UDPAddr{IP: net.IPv4(addressBA[0], addressBA[1], addressBA[2], addressBA[3]), Port: int(portBA)}
	if _, err := wrongSource.WriteToUDP([]byte("forged"), endpointAB); err != nil {
		t.Fatalf("send forged endpoint registration: %v", err)
	}
	if _, err := clientB.WriteToUDP([]byte("b-ready"), endpointBA); err != nil {
		t.Fatalf("send B handshake: %v", err)
	}
	if _, err := clientA.WriteToUDP([]byte("A-payload"), endpointAB); err != nil {
		t.Fatalf("send A payload: %v", err)
	}

	// B's handshake and A's payload travel on separate sockets, and UDP gives
	// no cross-socket ordering: on a loaded machine the payload can reach the
	// relay before the handshake registers B's leg, and the relay rightly
	// drops it. Resending the payload until it arrives keeps the assertion
	// about WHAT is forwarded while tolerating WHEN registration wins.
	buffer := make([]byte, 128)
	var n int
	overall := time.Now().Add(5 * time.Second)
	for {
		if err := clientB.SetReadDeadline(time.Now().Add(200 * time.Millisecond)); err != nil {
			t.Fatalf("set client B deadline: %v", err)
		}
		var err error
		n, _, err = clientB.ReadFromUDP(buffer)
		if err == nil {
			break
		}
		if !errors.Is(err, os.ErrDeadlineExceeded) || time.Now().After(overall) {
			t.Fatalf("receive relayed payload: %v", err)
		}
		if _, err := clientA.WriteToUDP([]byte("A-payload"), endpointAB); err != nil {
			t.Fatalf("resend A payload: %v", err)
		}
	}
	if !bytes.Equal(buffer[:n], []byte("A-payload")) {
		t.Fatalf("relayed payload = %q, want A-payload", buffer[:n])
	}
}

func TestCurrentPartyUDPRelayForwardsFirstHandshakeToRegisteredPeerPort(t *testing.T) {
	portStart := testCurrentPartyUDPRelayPortStart(t)
	relay, err := newCurrentPartyUDPRelay(true, "127.0.0.1", portStart, testCurrentPartyUDPRelayPortSpan, nil)
	if err != nil {
		t.Fatalf("new UDP relay: %v", err)
	}
	defer relay.Close()

	clientA := testCurrentPartyUDPClient(t, "127.0.0.1", portStart)
	defer clientA.Close()
	clientB := testCurrentPartyUDPClient(t, "127.0.0.1", portStart)
	defer clientB.Close()
	bindings := []currentPartyUDPRelayBinding{
		{characterID: 101, generation: 1, address: [4]byte{127, 0, 0, 1}, port: uint16(clientA.LocalAddr().(*net.UDPAddr).Port)},
		{characterID: 202, generation: 1, address: [4]byte{127, 0, 0, 1}, port: uint16(clientB.LocalAddr().(*net.UDPAddr).Port)},
	}
	if !relay.Sync(78, bindings) {
		t.Fatal("relay did not create registered two-member room")
	}
	addressAB, portAB, ok := relay.Endpoint(78, 101, 202)
	if !ok {
		t.Fatal("missing A-to-B relay endpoint")
	}
	endpointAB := &net.UDPAddr{IP: net.IPv4(addressAB[0], addressAB[1], addressAB[2], addressAB[3]), Port: int(portAB)}
	if _, err := clientA.WriteToUDP([]byte("first-handshake"), endpointAB); err != nil {
		t.Fatalf("send first handshake: %v", err)
	}

	buffer := make([]byte, 128)
	if err := clientB.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatalf("set client B deadline: %v", err)
	}
	n, _, err := clientB.ReadFromUDP(buffer)
	if err != nil {
		t.Fatalf("receive first relayed handshake: %v", err)
	}
	if !bytes.Equal(buffer[:n], []byte("first-handshake")) {
		t.Fatalf("first relayed handshake = %q", buffer[:n])
	}
}

func TestCurrentPartyUDPRelayLearnsActivePartySocketAfterSeededProbePort(t *testing.T) {
	portStart := testCurrentPartyUDPRelayPortStart(t)
	relay, err := newCurrentPartyUDPRelay(true, "127.0.0.1", portStart, testCurrentPartyUDPRelayPortSpan, nil)
	if err != nil {
		t.Fatalf("new UDP relay: %v", err)
	}
	defer relay.Close()

	probeA := testCurrentPartyUDPClient(t, "127.0.0.1", portStart)
	defer probeA.Close()
	partyA := testCurrentPartyUDPClient(t, "127.0.0.1", portStart)
	defer partyA.Close()
	clientB := testCurrentPartyUDPClient(t, "127.0.0.1", portStart)
	defer clientB.Close()
	bindings := []currentPartyUDPRelayBinding{
		{characterID: 101, generation: 1, address: [4]byte{127, 0, 0, 1}, port: uint16(probeA.LocalAddr().(*net.UDPAddr).Port)},
		{characterID: 202, generation: 1, address: [4]byte{127, 0, 0, 1}, port: uint16(clientB.LocalAddr().(*net.UDPAddr).Port)},
	}
	if !relay.Sync(79, bindings) {
		t.Fatal("relay did not create seeded two-member room")
	}
	addressAB, portAB, ok := relay.Endpoint(79, 101, 202)
	if !ok {
		t.Fatal("missing A-to-B relay endpoint")
	}
	addressBA, portBA, ok := relay.Endpoint(79, 202, 101)
	if !ok {
		t.Fatal("missing B-to-A relay endpoint")
	}
	endpointAB := &net.UDPAddr{IP: net.IPv4(addressAB[0], addressAB[1], addressAB[2], addressAB[3]), Port: int(portAB)}
	endpointBA := &net.UDPAddr{IP: net.IPv4(addressBA[0], addressBA[1], addressBA[2], addressBA[3]), Port: int(portBA)}
	if _, err := partyA.WriteToUDP([]byte("A-live-source"), endpointAB); err != nil {
		t.Fatalf("send A live-source handshake: %v", err)
	}
	buffer := make([]byte, 128)
	if err := clientB.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatalf("set B read deadline: %v", err)
	}
	if _, _, err := clientB.ReadFromUDP(buffer); err != nil {
		t.Fatalf("receive A handshake at seeded B endpoint: %v", err)
	}
	if _, err := clientB.WriteToUDP([]byte("B-reply"), endpointBA); err != nil {
		t.Fatalf("send B reply: %v", err)
	}
	if err := partyA.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatalf("set A live socket deadline: %v", err)
	}
	n, _, err := partyA.ReadFromUDP(buffer)
	if err != nil {
		t.Fatalf("receive B reply at learned A party socket: %v", err)
	}
	if !bytes.Equal(buffer[:n], []byte("B-reply")) {
		t.Fatalf("learned A party socket payload=%q", buffer[:n])
	}
	if err := probeA.SetReadDeadline(time.Now().Add(50 * time.Millisecond)); err != nil {
		t.Fatalf("set A probe deadline: %v", err)
	}
	if _, _, err := probeA.ReadFromUDP(buffer); err == nil {
		t.Fatal("B reply was still delivered to the inactive seeded probe socket")
	}
}

func TestCurrentPartyUDPRelayPreservesLargeRealtimeActorDatagram(t *testing.T) {
	portStart := testCurrentPartyUDPRelayPortStart(t)
	relay, err := newCurrentPartyUDPRelay(true, "127.0.0.1", portStart, testCurrentPartyUDPRelayPortSpan, nil)
	if err != nil {
		t.Fatalf("new UDP relay: %v", err)
	}
	defer relay.Close()

	clientA := testCurrentPartyUDPClient(t, "127.0.0.1", portStart)
	defer clientA.Close()
	clientB := testCurrentPartyUDPClient(t, "127.0.0.1", portStart)
	defer clientB.Close()
	bindings := []currentPartyUDPRelayBinding{
		{characterID: 101, generation: 1, address: [4]byte{127, 0, 0, 1}, port: uint16(clientA.LocalAddr().(*net.UDPAddr).Port)},
		{characterID: 202, generation: 1, address: [4]byte{127, 0, 0, 1}, port: uint16(clientB.LocalAddr().(*net.UDPAddr).Port)},
	}
	if !relay.Sync(80, bindings) {
		t.Fatal("relay did not create large-datagram test room")
	}
	addressAB, portAB, ok := relay.Endpoint(80, 101, 202)
	if !ok {
		t.Fatal("missing A-to-B relay endpoint")
	}

	// Exceeds the former 2 KiB buffer and models one complete reliable actor
	// snapshot containing riding/transform plus combat state.
	payload := make([]byte, 32*1024)
	for index := range payload {
		payload[index] = byte(index*31 + 7)
	}
	endpointAB := &net.UDPAddr{
		IP:   net.IPv4(addressAB[0], addressAB[1], addressAB[2], addressAB[3]),
		Port: int(portAB),
	}
	if _, err := clientA.WriteToUDP(payload, endpointAB); err != nil {
		t.Fatalf("send large actor datagram: %v", err)
	}

	buffer := make([]byte, len(payload)+1)
	if err := clientB.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatalf("set client B deadline: %v", err)
	}
	n, _, err := clientB.ReadFromUDP(buffer)
	if err != nil {
		t.Fatalf("receive large actor datagram: %v", err)
	}
	if n != len(payload) || !bytes.Equal(buffer[:n], payload) {
		t.Fatalf("large actor datagram bytes=%d want=%d equal=%t", n, len(payload), bytes.Equal(buffer[:n], payload))
	}
}

func TestCurrentPartyUDPRelaySyncRemovesFormerMemberLegs(t *testing.T) {
	portStart := testCurrentPartyUDPRelayPortStart(t)
	relay, err := newCurrentPartyUDPRelay(true, "127.0.0.1", portStart, testCurrentPartyUDPRelayPortSpan, nil)
	if err != nil {
		t.Fatalf("new UDP relay: %v", err)
	}
	defer relay.Close()

	first := []currentPartyUDPRelayBinding{
		{characterID: 101, generation: 1, address: [4]byte{127, 0, 0, 1}},
		{characterID: 202, generation: 1, address: [4]byte{127, 0, 0, 1}},
		{characterID: 303, generation: 1, address: [4]byte{127, 0, 0, 1}},
	}
	if !relay.Sync(77, first) {
		t.Fatal("initial relay sync failed")
	}
	if _, _, ok := relay.Endpoint(77, 101, 303); !ok {
		t.Fatal("initial member endpoint missing")
	}
	if !relay.Sync(77, first[:2]) {
		t.Fatal("member removal relay sync failed")
	}
	if _, _, ok := relay.Endpoint(77, 101, 303); ok {
		t.Fatal("former member endpoint remained routable")
	}
	if _, _, ok := relay.Endpoint(77, 101, 202); !ok {
		t.Fatal("remaining pair endpoint was removed")
	}
}

// testCurrentPartyUDPRelayPortSpan is how many ports the relay may bind its legs
// from in these tests. It is shared with testCurrentPartyUDPClient so that a test
// client socket can never land on one of the legs' port numbers.
const testCurrentPartyUDPRelayPortSpan = 12

func testCurrentPartyUDPRelayPortStart(t *testing.T) int {
	t.Helper()
	for attempt := 0; attempt < 20; attempt++ {
		listener, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
		if err != nil {
			t.Fatalf("allocate test UDP port: %v", err)
		}
		port := listener.LocalAddr().(*net.UDPAddr).Port
		_ = listener.Close()
		if port <= 65523 {
			return port
		}
	}
	t.Fatal("could not allocate a usable twelve-port UDP test range")
	return 0
}

// testCurrentPartyUDPClient listens on an ephemeral loopback port that is kept
// outside the relay's own port window.
//
// The relay binds its legs on the wildcard address (0.0.0.0:port, deliberately:
// see newCurrentPartyUDPRelay) while this helper binds a specific address
// (127.0.0.1:port). Windows lets both exist, and the specific bind wins delivery,
// so a client socket that happened to land on a leg's port number silently
// swallowed every datagram addressed to that leg -- the relay then looked like it
// had failed to forward when in fact nothing ever reached it. The window start is
// handed in by the caller so the two allocations can never overlap.
func testCurrentPartyUDPClient(t *testing.T, ip string, windowStart int) *net.UDPConn {
	t.Helper()
	for attempt := 0; attempt < 64; attempt++ {
		conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP(ip), Port: 0})
		if err != nil {
			t.Fatalf("listen UDP client %s: %v", ip, err)
		}
		port := conn.LocalAddr().(*net.UDPAddr).Port
		if port < windowStart || port >= windowStart+testCurrentPartyUDPRelayPortSpan {
			return conn
		}
		// Give the port back: the relay is about to bind this same number on the
		// wildcard address, and holding it here would shadow that leg.
		_ = conn.Close()
	}
	t.Fatalf("could not place UDP client %s outside relay window %d..%d",
		ip, windowStart, windowStart+testCurrentPartyUDPRelayPortSpan-1)
	return nil
}
