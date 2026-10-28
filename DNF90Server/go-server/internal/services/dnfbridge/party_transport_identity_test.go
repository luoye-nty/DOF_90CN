package dnfbridge

import (
	"encoding/binary"
	"testing"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	"longheng.io/server/internal/modules/dnf/channelcatalog"
)

func registerPartyPeerEndpointForTest(session *gameSession, host byte, port uint16) {
	if session == nil {
		return
	}
	session.partyPeerMu.Lock()
	session.partyPeer = currentPartyPeerEndpointRegistration{
		InnerIPv4: [4]byte{192, 168, 5, host},
		OuterIPv4: [4]byte{192, 168, 5, host},
		Port:      port,
		MTU:       currentPartyPeerDefaultPathMTU,
	}
	session.partyPeerMu.Unlock()
}

func TestPartyLoginPublishesUniqueNativeRoutingKey(t *testing.T) {
	service := &Service{options: options{serverIP: "127.0.0.1", gameOuterToken: 0x11223344}}
	channel := channelcatalog.Channel{ID: 19, Type: 1, Port: 10019}
	first := &gameSession{channel: channel}
	second := &gameSession{channel: channel}

	firstBody := service.buildLoginSuccessForSession(first)
	secondBody := service.buildLoginSuccessForSession(second)
	firstKey := binary.LittleEndian.Uint32(firstBody[4:8])
	secondKey := binary.LittleEndian.Uint32(secondBody[4:8])
	if firstKey == 0 || secondKey == 0 || firstKey == secondKey {
		t.Fatalf("login routing keys first=%#x second=%#x, want distinct nonzero keys", firstKey, secondKey)
	}
	if firstKey != first.partyRoutingKey || secondKey != second.partyRoutingKey {
		t.Fatalf("login keys first=%#x second=%#x do not match session identities first=%#x second=%#x", firstKey, secondKey, first.partyRoutingKey, second.partyRoutingKey)
	}
	if firstKey == service.options.gameOuterToken || secondKey == service.options.gameOuterToken {
		t.Fatalf("party routing key reused outer compatibility token %#x", service.options.gameOuterToken)
	}
	if first.partyPathMTU != currentPartyPeerDefaultPathMTU || second.partyPathMTU != currentPartyPeerDefaultPathMTU {
		t.Fatalf("default path MTU first=%d second=%d", first.partyPathMTU, second.partyPathMTU)
	}
}

func TestRuntimePartyPeerEndpointPublishesRemoteNativeIdentity(t *testing.T) {
	service := &Service{options: options{gameOuterToken: 0x11223344}}
	channel := channelcatalog.Channel{ID: 38, Type: 1, Port: 10038}
	first := &gameSession{conn: &bufferConn{}, channel: channel, selectedCharacterID: 1001, partyRoutingKey: 111, partyPathMTU: 512}
	second := &gameSession{conn: &bufferConn{}, channel: channel, selectedCharacterID: 1002, partyRoutingKey: 222, partyPathMTU: 1472}
	second.partyPeer = currentPartyPeerEndpointRegistration{
		InnerIPv4: [4]byte{192, 168, 5, 3}, OuterIPv4: [4]byte{192, 168, 5, 3},
		Port: 60359, MTU: 1472,
	}
	service.bindGameSessionCharacter(first, 1001)
	service.bindGameSessionCharacter(second, 1002)
	state := alignedcmd.PartyState{
		PartyID: 2, UserID: 1001, MaxMembers: 4,
		Members: []alignedcmd.PartyMemberState{
			{UserID: 1001, Slot: 0, UserState: 100},
			{UserID: 1002, Slot: 1, UserState: 100},
		},
	}

	body := service.buildRuntimePartyPeerEndpointInfoForReceiver(first, state)
	if len(body) != 23 || body[0] != 1 || binary.LittleEndian.Uint16(body[1:3]) != 1002 {
		t.Fatalf("remote-only op11 body=% X", body)
	}
	if got := binary.LittleEndian.Uint32(body[13:17]); got != second.partyRoutingKey {
		t.Fatalf("op11 routing key=%#x want remote identity=%#x body=% X", got, second.partyRoutingKey, body)
	}
	if body[17] != 1 {
		t.Fatalf("op11 NAT type=%d want direct=1 body=% X", body[17], body)
	}
	if got := binary.LittleEndian.Uint32(body[18:22]); got != 1472 {
		t.Fatalf("op11 path MTU=%d want=1472 body=% X", got, body)
	}
	if body[22] != 100 {
		t.Fatalf("op11 connection percent=%d want=100 body=% X", body[22], body)
	}
	if !service.runtimePartyNativePeerEndpointsReady(first, state) {
		t.Fatal("complete native endpoint table was not admitted")
	}

	// The legacy relay can remain constructed as an explicit compatibility
	// option, but it must not replace the same-version op2 endpoint table.
	service.partyUDPRelay = &currentPartyUDPRelay{enabled: true, advertise: [4]byte{10, 0, 0, 1}}
	withRelay := service.buildRuntimePartyPeerEndpointInfoForReceiver(first, state)
	if string(withRelay) != string(body) {
		t.Fatalf("enabled compatibility relay changed native op11 row\n got=% X\nwant=% X", withRelay, body)
	}

	second.partyPeer.Port = 0
	if service.runtimePartyNativePeerEndpointsReady(first, state) {
		t.Fatal("zero-port native endpoint table was admitted")
	}
}

func TestCurrentPartyNetworkReportUpdatesNativePathMTU(t *testing.T) {
	service := &Service{}
	session := &gameSession{}
	body := make([]byte, currentPartyNetworkReportSize)
	binary.LittleEndian.PutUint32(body[0:4], 1024)
	binary.LittleEndian.PutUint32(body[10:14], currentPartyNetworkVersion)
	if err := service.handleCurrentPartyNetworkReport(session, body); err != nil {
		t.Fatalf("handle network report: %v", err)
	}
	if session.partyRoutingKey == 0 || session.partyPathMTU != 1024 {
		t.Fatalf("transport identity key=%d mtu=%d", session.partyRoutingKey, session.partyPathMTU)
	}

	binary.LittleEndian.PutUint32(body[0:4], 1500)
	if err := service.handleCurrentPartyNetworkReport(session, body); err == nil {
		t.Fatal("accepted non-native 1500-byte path MTU")
	}
}

func TestCurrentPartyTransportTelemetryMatchesSameVersionLayouts(t *testing.T) {
	peerState := []byte{1, 0x34, 0x12}
	parsedPeer, err := parseCurrentPartyPeerStateReport(peerState)
	if err != nil || parsedPeer.InternalStateWasZero != 1 || parsedPeer.PeerActor != 0x1234 {
		t.Fatalf("peer-state parsed=%+v err=%v", parsedPeer, err)
	}

	holePunch := make([]byte, 23)
	holePunch[0] = 2
	binary.LittleEndian.PutUint32(holePunch[1:5], 3)
	binary.LittleEndian.PutUint32(holePunch[5:9], 12000)
	binary.LittleEndian.PutUint16(holePunch[21:23], 0x4321)
	parsedHolePunch, err := parseCurrentPartyHolePunchReport(holePunch)
	if err != nil || parsedHolePunch.ObservationWindowMS != 12000 || parsedHolePunch.PeerActor != 0x4321 {
		t.Fatalf("hole-punch parsed=%+v err=%v", parsedHolePunch, err)
	}

	averagePing := make([]byte, 10)
	binary.LittleEndian.PutUint16(averagePing[0:2], 3)
	binary.LittleEndian.PutUint32(averagePing[2:6], 0x11223344)
	binary.LittleEndian.PutUint32(averagePing[6:10], 27)
	parsedPing, err := parseCurrentPartyAveragePingReport(averagePing)
	if err != nil || parsedPing.Slot != 3 || parsedPing.PeerKey != 0x11223344 || parsedPing.AveragePingMS != 27 {
		t.Fatalf("average-ping parsed=%+v err=%v", parsedPing, err)
	}

	averagePing[0] = 8
	averagePing[1] = 0
	if _, err := parseCurrentPartyAveragePingReport(averagePing); err == nil {
		t.Fatal("accepted out-of-range party slot")
	}
}
