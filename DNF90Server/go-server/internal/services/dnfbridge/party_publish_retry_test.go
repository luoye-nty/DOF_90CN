package dnfbridge

import (
	"bytes"
	"context"
	"io"
	"net"
	"testing"
	"time"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	"longheng.io/server/internal/modules/dnf/channelcatalog"
	dnfproto "longheng.io/server/internal/modules/dnf/protocol"
	"longheng.io/server/internal/platform/timequeue"
)

// partyPublishTestConn is a game connection with a concrete IPv4 TCP tuple.
//
// bufferConn answers RemoteAddr with a synthetic name, which is not enough here:
// every relay and publish decision is keyed on the authenticated TCP IPv4, so a
// test of those decisions needs a real *net.TCPAddr.
type partyPublishTestConn struct {
	remote *net.TCPAddr
	local  *net.TCPAddr
	write  bytes.Buffer
}

func newPartyPublishTestConn(ip string, port int) *partyPublishTestConn {
	return &partyPublishTestConn{
		remote: &net.TCPAddr{IP: net.ParseIP(ip).To4(), Port: port},
		local:  &net.TCPAddr{IP: net.ParseIP("127.0.0.1").To4(), Port: 10038},
	}
}

func (c *partyPublishTestConn) Read([]byte) (int, error)         { return 0, io.EOF }
func (c *partyPublishTestConn) Write(data []byte) (int, error)   { return c.write.Write(data) }
func (c *partyPublishTestConn) Close() error                     { return nil }
func (c *partyPublishTestConn) LocalAddr() net.Addr              { return c.local }
func (c *partyPublishTestConn) RemoteAddr() net.Addr             { return c.remote }
func (c *partyPublishTestConn) SetDeadline(time.Time) error      { return nil }
func (c *partyPublishTestConn) SetReadDeadline(time.Time) error  { return nil }
func (c *partyPublishTestConn) SetWriteDeadline(time.Time) error { return nil }

// partyPublishTestQueue is a gameplay time queue whose callbacks only run when
// the test asks them to, so a retry sequence can be exercised at all of its
// delays without waiting fifteen seconds.
type partyPublishTestQueue struct {
	callbacks map[string]timequeue.Callback
	delays    map[string]time.Duration
	order     []string
}

func newPartyPublishTestQueue() *partyPublishTestQueue {
	return &partyPublishTestQueue{
		callbacks: make(map[string]timequeue.Callback),
		delays:    make(map[string]time.Duration),
	}
}

func (q *partyPublishTestQueue) Now() time.Time { return time.Now() }

func (q *partyPublishTestQueue) ScheduleAfter(name string, delay time.Duration, callback timequeue.Callback) error {
	if _, exists := q.callbacks[name]; !exists {
		q.order = append(q.order, name)
	}
	q.callbacks[name] = callback
	q.delays[name] = delay
	return nil
}

func (q *partyPublishTestQueue) RegisterDaily(string, int, int, timequeue.Callback) error { return nil }

func (q *partyPublishTestQueue) Cancel(name string) bool {
	if _, found := q.callbacks[name]; !found {
		return false
	}
	delete(q.callbacks, name)
	return true
}

func (q *partyPublishTestQueue) Start(context.Context) {}

func (q *partyPublishTestQueue) namesMatching(prefix string) []string {
	result := make([]string, 0, len(q.order))
	for _, name := range q.order {
		if len(name) >= len(prefix) && name[:len(prefix)] == prefix {
			result = append(result, name)
		}
	}
	return result
}

func (q *partyPublishTestQueue) fire(t *testing.T, name string) {
	t.Helper()
	callback, found := q.callbacks[name]
	if !found {
		t.Fatalf("no scheduled callback named %q (have %v)", name, q.order)
	}
	delete(q.callbacks, name)
	callback(time.Now())
}

// partyPublishTestFixture is one party of two online members, formed through the
// central manager exactly like an accepted invite.
type partyPublishTestFixture struct {
	service    *Service
	queue      *partyPublishTestQueue
	leader     *gameSession
	member     *gameSession
	leaderConn *partyPublishTestConn
	memberConn *partyPublishTestConn
	state      alignedcmd.PartyState
}

func newPartyPublishTestFixture(t *testing.T, relay *currentPartyUDPRelay) *partyPublishTestFixture {
	t.Helper()
	service := &Service{options: options{gameUpperBodyCodec: gameUpperBodyCodecPlain, serverIP: "127.0.0.1"}}
	queue := newPartyPublishTestQueue()
	service.gameplayTimers = queue
	if relay != nil {
		service.partyUDPRelay = relay
	}
	channel := channelcatalog.Channel{ID: 38, Type: 1, Name: "ch.38", Port: 10038}
	fixture := &partyPublishTestFixture{
		service:    service,
		queue:      queue,
		leaderConn: newPartyPublishTestConn("127.0.0.1", 41001),
		memberConn: newPartyPublishTestConn("127.0.0.1", 41002),
	}
	fixture.leader = &gameSession{conn: fixture.leaderConn, channel: channel, selectedCharacterID: 1001}
	fixture.member = &gameSession{conn: fixture.memberConn, channel: channel, selectedCharacterID: 1002}
	service.bindGameSessionCharacter(fixture.leader, 1001)
	service.bindGameSessionCharacter(fixture.member, 1002)
	state, result, ok := service.createOrJoinManagedRuntimeParty(fixture.member, fixture.leader)
	if !ok || state.PartyID <= 0 || len(runtimePartyMembers(state)) != 2 {
		t.Fatalf("form two-member managed party: ok=%t result=%+v state=%+v", ok, result, state)
	}
	fixture.state = state
	return fixture
}

func (f *partyPublishTestFixture) leaderState() alignedcmd.PartyState {
	return runtimePartyStateSnapshot(f.leader)
}

func (f *partyPublishTestFixture) written(t *testing.T, conn *partyPublishTestConn) []dnfproto.ChannelPacket {
	t.Helper()
	data := conn.write.Bytes()
	packets := make([]dnfproto.ChannelPacket, 0, 8)
	for len(data) > 0 {
		packet, rest := splitCurrentGameServerUpperPacketAuto(t, data)
		packets = append(packets, packet)
		data = rest
	}
	return packets
}

// TestRuntimePartyRelayInstallsLegsWithoutProbePort is the regression for the
// party that could never be formed at all.
//
// Live 2026-09-29 on 203.0.113.12: party 48 member 32 never produced a native
// UDP probe answer, so syncRuntimePartyUDPRelayLegs skipped that member, the
// directed relay set for the OTHER members was therefore incomplete, op11 fell
// back to the direct table, and the native admission gate refused that table as
// well. Nothing was published: the accepting player held the class1 ACK while no
// member ever received op11/op9/op153 -- "组队还组不上 显示连接中".
//
// A relay leg is authenticated by the member's TCP IPv4 and session generation,
// never by its probe port, so the leg can be installed with no seed endpoint and
// learns the return path from the member's first datagram.
func TestRuntimePartyRelayInstallsLegsWithoutProbePort(t *testing.T) {
	portStart := testCurrentPartyUDPRelayPortStart(t)
	relay, err := newCurrentPartyUDPRelay(true, "127.0.0.1", portStart, testCurrentPartyUDPRelayPortSpan, nil)
	if err != nil {
		t.Fatalf("new UDP relay: %v", err)
	}
	defer relay.Close()
	fixture := newPartyPublishTestFixture(t, relay)

	if port := currentPartyPeerPort(fixture.member); port != 0 {
		t.Fatalf("member UDP probe port = %d, want an unobserved zero", port)
	}
	leaderState := fixture.leaderState()
	if fixture.service.runtimePartyNativePeerEndpointsReady(fixture.leader, leaderState) {
		t.Fatal("unprobed member was admitted by the direct native endpoint table")
	}
	if !fixture.service.syncRuntimePartyUDPRelayLegs(leaderState, false) {
		t.Fatal("relay refused to install the directed leg set")
	}
	if _, _, ok := relay.Endpoint(uint16(leaderState.PartyID), 1001, 1002); !ok {
		t.Fatal("leader-to-member relay leg is missing")
	}
	if _, _, ok := relay.Endpoint(uint16(leaderState.PartyID), 1002, 1001); !ok {
		t.Fatal("member-to-leader relay leg is missing")
	}
	if !fixture.service.runtimePartyPublishablePeerEndpoints(fixture.leader, leaderState) {
		t.Fatal("relay-backed endpoint table was not admitted for publication")
	}
	published, err := fixture.service.publishManagedRuntimePartySnapshots(leaderState)
	if err != nil || !published {
		t.Fatalf("relay-backed formation published=%t err=%v", published, err)
	}

	packets := fixture.written(t, fixture.leaderConn)
	if len(packets) == 0 {
		t.Fatal("leader received no formation frame")
	}
	peer := packets[0]
	if peer.Header.Classification != 0 || peer.Header.MsgID != 0x000b {
		t.Fatalf("first formation packet = class %d msg %d, want op11", peer.Header.Classification, peer.Header.MsgID)
	}
	if len(peer.Body) != 23 || peer.Body[0] != 1 {
		t.Fatalf("op11 body = % X, want one remote row", peer.Body)
	}
	if peer.Body[1] != 0xea || peer.Body[2] != 0x03 {
		t.Fatalf("op11 remote member id = % X, want 1002", peer.Body[1:3])
	}
	// Inner/outer IPv4 are the relay's advertised address and the port is one of
	// its leg ports, not a made-up peer port: the client dials the relay.
	if [4]byte{peer.Body[3], peer.Body[4], peer.Body[5], peer.Body[6]} != [4]byte{127, 0, 0, 1} {
		t.Fatalf("op11 inner IPv4 = %v, want the relay address", peer.Body[3:7])
	}
	relayPort := uint16(peer.Body[11])<<8 | uint16(peer.Body[12])
	_, wantPort, ok := relay.Endpoint(uint16(leaderState.PartyID), 1001, 1002)
	if !ok || relayPort != wantPort {
		t.Fatalf("op11 relay port = %d, want %d", relayPort, wantPort)
	}
	if relayPort < uint16(portStart) || relayPort >= uint16(portStart+testCurrentPartyUDPRelayPortSpan) {
		t.Fatalf("op11 relay port %d outside the configured window %d..%d",
			relayPort, portStart, portStart+testCurrentPartyUDPRelayPortSpan-1)
	}
	if armed := fixture.queue.namesMatching("party-snapshot-publish-retry:"); len(armed) != 0 {
		t.Fatalf("published party armed %d publish retries: %v", len(armed), armed)
	}
}

// TestCurrentPartyUDPRelayKeepsPortWhenBindingGainsProbePort pins the port
// stability the relay-backed formation depends on.
//
// A leg's port is what op11 hands the client as its peer endpoint. If a leg moved
// to a different port when the binding changed -- which is what happens whenever a
// member's late native probe answer arrives, and is now the normal path for a
// member whose leg was installed without a seed -- the client would keep dialling
// the port it was given and the link would break exactly when it was supposed to
// start working.
func TestCurrentPartyUDPRelayKeepsPortWhenBindingGainsProbePort(t *testing.T) {
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

	unseeded := []currentPartyUDPRelayBinding{
		{characterID: 101, generation: 1, address: [4]byte{127, 0, 0, 1}},
		{characterID: 202, generation: 1, address: [4]byte{127, 0, 0, 1}},
	}
	if !relay.Sync(91, unseeded) {
		t.Fatal("relay refused a leg set installed without probe ports")
	}
	_, firstPort, ok := relay.Endpoint(91, 101, 202)
	if !ok {
		t.Fatal("unseeded relay endpoint missing")
	}

	// The members' native probe answers finally arrive, so both bindings gain a
	// port. The legs must stay where they were: op11 already handed these ports to
	// the clients.
	seeded := []currentPartyUDPRelayBinding{
		{characterID: 101, generation: 1, address: [4]byte{127, 0, 0, 1}, port: uint16(clientA.LocalAddr().(*net.UDPAddr).Port)},
		{characterID: 202, generation: 1, address: [4]byte{127, 0, 0, 1}, port: uint16(clientB.LocalAddr().(*net.UDPAddr).Port)},
	}
	if !relay.Sync(91, seeded) {
		t.Fatal("relay refused a leg set with probe ports")
	}
	_, secondPort, ok := relay.Endpoint(91, 101, 202)
	if !ok {
		t.Fatal("seeded relay endpoint missing")
	}
	if firstPort != secondPort {
		t.Fatalf("relay leg moved from port %d to %d when its binding gained a probe port", firstPort, secondPort)
	}

	// The reseed must become a real return path: the leg opposite A's now has A's
	// registered port as its destination, so A's first payload crosses without
	// waiting for B to speak first.
	addressAB, portAB, ok := relay.Endpoint(91, 101, 202)
	if !ok {
		t.Fatal("endpoint missing after the probe ports became known")
	}
	if _, err := clientA.WriteToUDP([]byte("A-first"), &net.UDPAddr{
		IP:   net.IPv4(addressAB[0], addressAB[1], addressAB[2], addressAB[3]),
		Port: int(portAB),
	}); err != nil {
		t.Fatalf("send first payload: %v", err)
	}
	buffer := make([]byte, 64)
	if err := clientB.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("set client B deadline: %v", err)
	}
	n, _, err := clientB.ReadFromUDP(buffer)
	if err != nil {
		t.Fatalf("receive first relayed payload: %v", err)
	}
	if string(buffer[:n]) != "A-first" {
		t.Fatalf("first relayed payload = %q", buffer[:n])
	}
}

// TestManagedRuntimePartySnapshotDeferralPublishesExactlyOnceAfterRetry covers
// the retry that completes a deferred formation -- and, just as importantly, the
// guarantee that it does not fire a second time. op11 is a formation handshake:
// replaying it over a live link makes the client tear the peer connection down
// and re-enter 连接中.
func TestManagedRuntimePartySnapshotDeferralPublishesExactlyOnceAfterRetry(t *testing.T) {
	fixture := newPartyPublishTestFixture(t, nil)
	state := fixture.state
	// The leader probed; the second member has not. That is the live shape: one
	// member's probe answer is missing and the whole formation is deferred.
	registerPartyPeerEndpointForTest(fixture.leader, 3, 5063)

	published, err := fixture.service.publishManagedRuntimePartySnapshots(state)
	if err != nil || published {
		t.Fatalf("deferred formation published=%t err=%v", published, err)
	}
	if len(fixture.leaderConn.write.Bytes()) != 0 || len(fixture.memberConn.write.Bytes()) != 0 {
		t.Fatal("a deferred formation wrote a frame anyway")
	}
	attempts := fixture.queue.namesMatching("party-snapshot-publish-retry:")
	if len(attempts) != len(currentPartySnapshotPublishRetryDelays) {
		t.Fatalf("armed %d retry attempts, want %d", len(attempts), len(currentPartySnapshotPublishRetryDelays))
	}

	// The client's native probe answer finally lands.
	registerPartyPeerEndpointForTest(fixture.member, 3, 60359)
	fixture.queue.fire(t, attempts[0])

	packets := fixture.written(t, fixture.leaderConn)
	if len(packets) == 0 {
		t.Fatal("retry published nothing after the endpoint became known")
	}
	if packets[0].Header.Classification != 0 || packets[0].Header.MsgID != 0x000b {
		t.Fatalf("retry frame starts with class %d msg %d, want op11", packets[0].Header.Classification, packets[0].Header.MsgID)
	}
	op9 := false
	for _, packet := range packets {
		if packet.Header.MsgID == 0x0009 {
			op9 = true
		}
	}
	if !op9 {
		t.Fatalf("retry frame carried no op9 aggregate: %+v", packets)
	}

	settled := len(fixture.leaderConn.write.Bytes()) + len(fixture.memberConn.write.Bytes())
	for _, attempt := range attempts[1:] {
		fixture.queue.fire(t, attempt)
	}
	if after := len(fixture.leaderConn.write.Bytes()) + len(fixture.memberConn.write.Bytes()); after != settled {
		t.Fatalf("a settled party was re-published: bytes %d -> %d", settled, after)
	}
}

// TestManagedRuntimePartySnapshotRetryStopsWhenPartyIsGone proves the retry
// cannot resurrect a party: the attempt re-reads the manager's current snapshot
// and does nothing when the party has since dissolved.
func TestManagedRuntimePartySnapshotRetryStopsWhenPartyIsGone(t *testing.T) {
	fixture := newPartyPublishTestFixture(t, nil)
	state := fixture.state
	if published, err := fixture.service.publishManagedRuntimePartySnapshots(state); published || err != nil {
		t.Fatalf("deferred formation published=%t err=%v", published, err)
	}
	attempts := fixture.queue.namesMatching("party-snapshot-publish-retry:")
	if len(attempts) == 0 {
		t.Fatal("no retry was armed")
	}

	manager := fixture.service.runtimePartyManagerForService()
	manager.Leave(1002, fixture.member.characterGeneration)
	manager.Leave(1001, fixture.leader.characterGeneration)
	if _, found := manager.SnapshotByID(uint16(state.PartyID)); found {
		t.Fatal("party survived both leaves")
	}
	registerPartyPeerEndpointForTest(fixture.member, 3, 60359)
	for _, attempt := range attempts {
		fixture.queue.fire(t, attempt)
	}
	if len(fixture.leaderConn.write.Bytes()) != 0 || len(fixture.memberConn.write.Bytes()) != 0 {
		t.Fatal("a dissolved party was republished by its retry")
	}
}

// TestManagedRuntimePartySnapshotRetryTokenIsSuperseded pins the arming rule
// that keeps two deferrals from racing each other into a double publish.
func TestManagedRuntimePartySnapshotRetryTokenIsSuperseded(t *testing.T) {
	service := &Service{}
	first := service.armManagedRuntimePartySnapshotRetry(41)
	if first == 0 || !service.managedRuntimePartySnapshotRetryCurrent(41, first) {
		t.Fatalf("first arming token=%d was not current", first)
	}
	second := service.armManagedRuntimePartySnapshotRetry(41)
	if second == first || !service.managedRuntimePartySnapshotRetryCurrent(41, second) {
		t.Fatalf("second arming token=%d did not supersede %d", second, first)
	}
	if service.managedRuntimePartySnapshotRetryCurrent(41, first) {
		t.Fatal("superseded token stayed current")
	}
	service.settleManagedRuntimePartySnapshotRetry(41)
	if service.managedRuntimePartySnapshotRetryCurrent(41, second) {
		t.Fatal("settled party kept a current token")
	}
	if other := service.armManagedRuntimePartySnapshotRetry(42); service.managedRuntimePartySnapshotRetryCurrent(41, other) {
		t.Fatal("a token armed for another party was accepted")
	}
}
