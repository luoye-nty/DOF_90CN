package dnfbridge

import (
	"context"
	"encoding/binary"
	"net"
	"net/netip"
	"testing"
	"time"
)

func TestCurrentPartyUDPProbeReflectsNativeSourceTuple(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	observed := make(chan netip.AddrPort, 1)
	probe, err := newCurrentPartyUDPProbe(
		ctx,
		"127.0.0.1",
		[]int{0},
		func(endpoint netip.AddrPort) { observed <- endpoint },
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer probe.Close()
	if len(probe.conns) != 1 {
		t.Fatalf("probe listeners=%d want=1", len(probe.conns))
	}
	serverAddress := probe.conns[0].LocalAddr().(*net.UDPAddr)
	client, err := net.DialUDP("udp4", nil, serverAddress)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if _, err := client.Write([]byte{1}); err != nil {
		t.Fatal(err)
	}
	if err := client.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	response := make([]byte, currentPartyUDPProbeResponseSize)
	if _, err := client.Read(response); err != nil {
		t.Fatal(err)
	}
	local := client.LocalAddr().(*net.UDPAddr)
	if response[0] != currentPartyUDPProbeResponseType ||
		binary.LittleEndian.Uint16(response[5:7]) != uint16(local.Port) {
		t.Fatalf("probe response=%x local=%v", response, local)
	}
	select {
	case endpoint := <-observed:
		if endpoint.Port() != uint16(local.Port) || !endpoint.Addr().IsLoopback() {
			t.Fatalf("observed endpoint=%v local=%v", endpoint, local)
		}
	case <-time.After(time.Second):
		t.Fatal("UDP endpoint was not observed")
	}
}

func TestCurrentPartyUDPProbeBindsTwoLocalClientsByOwningPID(t *testing.T) {
	first := &gameSession{conn: &bufferConn{}, connID: "first", clientPID: 101, selectedCharacterID: 1}
	second := &gameSession{conn: &bufferConn{}, connID: "second", clientPID: 202, selectedCharacterID: 2}
	service := &Service{
		options:        options{serverIP: "192.168.5.3"},
		gameSessions:   map[uint16]*gameSession{1: first, 2: second},
		partyEndpoints: newCurrentPartyUDPEndpointRegistry(),
		partyUDPOwnerPID: func(endpoint netip.AddrPort) (uint32, bool) {
			if endpoint.Port() == 55540 {
				return 202, true
			}
			return 101, true
		},
	}
	service.observeCurrentPartyUDPEndpoint(netip.MustParseAddrPort("192.168.5.3:5063"))
	service.observeCurrentPartyUDPEndpoint(netip.MustParseAddrPort("192.168.5.3:55540"))
	if got := currentPartyPeerPort(first); got != 5063 {
		t.Fatalf("first endpoint=%d want=5063", got)
	}
	if got := currentPartyPeerPort(second); got != 55540 {
		t.Fatalf("second endpoint=%d want=55540", got)
	}
}

func TestCurrentPartyUDPProbeRestoresEndpointObservedBeforeCharacterBind(t *testing.T) {
	service := &Service{
		options:        options{serverIP: "192.168.5.3"},
		gameSessions:   make(map[uint16]*gameSession),
		partyEndpoints: newCurrentPartyUDPEndpointRegistry(),
		partyUDPOwnerPID: func(netip.AddrPort) (uint32, bool) {
			return 303, true
		},
	}
	service.observeCurrentPartyUDPEndpoint(netip.MustParseAddrPort("192.168.5.3:63226"))
	session := &gameSession{conn: &bufferConn{}, connID: "late-bind", clientPID: 303}
	service.bindGameSessionCharacter(session, 7)
	if got := currentPartyPeerPort(session); got != 63226 {
		t.Fatalf("restored endpoint=%d want=63226", got)
	}
}
