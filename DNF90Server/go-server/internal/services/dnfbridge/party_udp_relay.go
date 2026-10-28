package dnfbridge

import (
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"sort"
	"sync"
	"time"
)

const (
	currentPartyUDPRelayMaxMembers = 4
	// Reliable party actor snapshots are UDP datagrams, not the compact game
	// TCP command bodies.  Riding/transform and dense combat state can exceed
	// the former 2 KiB scratch buffer.  On Windows ReadFromUDP reports an
	// oversized datagram as WSAEMSGSIZE; the old loop treated that as a closed
	// leg, permanently making the party transport one-way.  Preserve one whole
	// IPv4 UDP payload so the relay remains byte-transparent.
	currentPartyUDPRelayMaxDatagram = 65507
	// Per-directed-leg forwarding budget.
	//
	// The relay used to forward every accepted datagram unconditionally, with no
	// rate limit, no backpressure and no inspection of the payload, so ONE broken
	// client could spend the whole server's uplink and fill the packet log. Live
	// 2026-09-20: a member whose client stopped dispatching 0.24 s after the raid
	// phase activated fell into a pure retransmit storm -- 17,320,521 datagrams
	// in 273 s (~63,400/s), one frozen 9-byte payload, no TCP at all -- and the
	// relay mirrored every one of them; clearing the packet log and waiting three
	// minutes produced a 2.4 MB file that was nothing else.
	//
	// The budget is deliberately far above real traffic. Dense two-client party
	// play measures in the low thousands of datagrams per second (the dump
	// sampler in service.go was written against that figure), so 8,000/s with a
	// 4,000 datagram burst passes normal play untouched while cutting the
	// measured storm to about a tenth of its volume. UDP party traffic is already
	// lossy and its reliable layer retransmits, so a dropped datagram degrades
	// exactly like one lost on the internet path -- which is why dropping is the
	// right answer here and blocking the read loop is not.
	currentPartyUDPRelayForwardRatePerSecond = 8000.0
	currentPartyUDPRelayForwardBurstPackets  = 4000
	// At most one throttle report per leg per interval: the storm itself must not
	// be replaced by a log storm.
	currentPartyUDPRelayThrottleLogInterval = 5 * time.Second
)

// currentPartyUDPRelayBinding is one authenticated half of a party transport.
// The TCP session's observed IPv4 is deliberately authoritative. A client may
// report an arbitrary inner/outer endpoint in CS op2, whereas the relay must
// never learn an endpoint from an unrelated Internet host.
type currentPartyUDPRelayBinding struct {
	characterID uint16
	generation  uint64
	address     [4]byte
	port        uint16
}

type currentPartyUDPRelayLegKey struct {
	partyID uint16
	ownerID uint16
	peerID  uint16
}

type currentPartyUDPRelayLeg struct {
	key     currentPartyUDPRelayLegKey
	binding currentPartyUDPRelayBinding
	conn    *net.UDPConn
	port    uint16

	mu               sync.Mutex
	learnedEndpoint  *net.UDPAddr
	ingressPackets   uint64
	forwardedPackets uint64
	// forwardTokens / forwardRefilledAt are this leg's forwarding budget (see
	// currentPartyUDPRelayForwardRatePerSecond). throttledPackets counts the
	// accepted datagrams the budget refused, and throttleLoggedAt paces the
	// report so a storm cannot turn into a log storm.
	forwardTokens     float64
	forwardRefilledAt time.Time
	throttledPackets  uint64
	throttleLoggedAt  time.Time
	// droppedPackets counts accepted datagrams that had nowhere to go because
	// the opposite leg had not learned its endpoint yet. Without it the relay
	// is silent in exactly the failure case that matters: a peer that never
	// initiates makes every payload sent towards it vanish without a trace, so
	// an absent log cannot distinguish "P2P works" from "nothing was ever sent".
	droppedPackets uint64
	droppedLogged  bool
}

// currentPartyUDPRelay is a per-directed-pair UDP relay. For party members A
// and B it owns two ports: A sends to A->B; received payload is emitted from
// B->A to B's learned endpoint. This keeps the existing client P2P handshake
// shape while exposing only the server address to every participant.
//
// It is intentionally IPv4-only because PARTY_IP_INFO carries exactly four
// octets. The listener binds 0.0.0.0 so cloud NAT / forwarder can deliver
// inbound datagrams whose destination has been rewritten to a private
// interface; advertise keeps the public IPv4 that clients dial.
type currentPartyUDPRelay struct {
	mu sync.Mutex

	enabled   bool
	bindIP    net.IP
	advertise [4]byte
	portStart int
	portCount int
	usedPorts map[int]struct{}
	legs      map[currentPartyUDPRelayLegKey]*currentPartyUDPRelayLeg
	closed    bool
	wg        sync.WaitGroup

	logWarn func(string, ...any)
	// logEvent, when set, emits a packet-log EVENT rather than a prose warning.
	// The forwarding budget reports through it because the operator reads those
	// events by name (game-party-udp-relay-throttled) when triaging a client that
	// went into a storm.
	logEvent func(string, ...any)
	// payloadDump, when set, receives every accepted ingress datagram. It is
	// wired only while the packet log is enabled: the client-to-client party
	// traffic (skill state, buffs, hits) is otherwise invisible to the server,
	// and a teammate-side symptom such as "buff arrives at level 0" can only be
	// attributed to the caster or the receiver by reading what actually crossed.
	payloadDump func(leg currentPartyUDPRelayLegKey, source *net.UDPAddr, payload []byte)
}

func newCurrentPartyUDPRelay(enabled bool, advertiseIP string, portStart, portCount int, logWarn func(string, ...any)) (*currentPartyUDPRelay, error) {
	relay := &currentPartyUDPRelay{
		enabled: enabled,
		logWarn: logWarn,
	}
	if !enabled {
		return relay, nil
	}
	ip := net.ParseIP(advertiseIP)
	if ip == nil || ip.To4() == nil || ip.IsUnspecified() {
		return nil, fmt.Errorf("party UDP relay requires a concrete IPv4 advertise address, got %q", advertiseIP)
	}
	if portStart < 1 || portStart > 65535 || portCount < 12 || portCount > 1024 || portStart+portCount-1 > 65535 {
		return nil, fmt.Errorf("party UDP relay port range %d+%d is invalid", portStart, portCount)
	}
	ipv4 := ip.To4()
	// 云 NAT / 转发器会把发往公网 IP 的入站 UDP 数据报目标地址改写为私网
	// 网卡地址，因此监听必须绑 0.0.0.0（所有接口）；advertise 仍保持公网
	// IP，客户端仍把 UDP 发到公网地址，经 NAT 改写后由通配监听接收。
	relay.bindIP = append(net.IP(nil), net.IPv4zero...)
	copy(relay.advertise[:], ipv4)
	relay.portStart = portStart
	relay.portCount = portCount
	relay.usedPorts = make(map[int]struct{})
	relay.legs = make(map[currentPartyUDPRelayLegKey]*currentPartyUDPRelayLeg)
	return relay, nil
}

func (r *currentPartyUDPRelay) Enabled() bool {
	return r != nil && r.enabled
}

// Usage reports the directed-leg picture for the raid cost summary: how many
// ports are bound right now, how many the configured range can hold, and how many
// legs exist.
//
// A raid of more than eight players is exactly where this matters: one
// four-member squad is 4x3 = 12 directed legs, so 20 members need 60 of them, and
// a pool that runs out falls back to the direct table -- which for players behind
// NAT is the "红电脑 / 连接中" the operator reports.
func (r *currentPartyUDPRelay) Usage() (usedPorts int, capacity int, legs int) {
	if r == nil || !r.enabled {
		return 0, 0, 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.usedPorts), r.portCount, len(r.legs)
}

// Sync installs a complete, transactional directed-pair set for one party.
// It returns false without publishing a partial relay topology when a member
// is stale/offline or the configured UDP range cannot satisfy the room.
func (r *currentPartyUDPRelay) Sync(partyID uint16, bindings []currentPartyUDPRelayBinding) bool {
	if r == nil || !r.enabled {
		return false
	}
	bindings = currentPartyUDPRelayBindings(bindings)
	if partyID == 0 || len(bindings) < 2 || len(bindings) > currentPartyUDPRelayMaxMembers {
		return false
	}

	wanted := make(map[currentPartyUDPRelayLegKey]currentPartyUDPRelayBinding, len(bindings)*(len(bindings)-1))
	for _, owner := range bindings {
		for _, peer := range bindings {
			if owner.characterID == peer.characterID {
				continue
			}
			wanted[currentPartyUDPRelayLegKey{partyID: partyID, ownerID: owner.characterID, peerID: peer.characterID}] = owner
		}
	}

	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return false
	}
	newLegs := make(map[currentPartyUDPRelayLegKey]*currentPartyUDPRelayLeg)
	for key, binding := range wanted {
		// A leg is identified by its directed key alone. Its binding is only the
		// ingress filter plus a seed return endpoint, so a binding that merely
		// changes -- a client whose native probe answer arrives late, or one that
		// re-probes on a new local port -- must NOT move the leg to another port.
		// Moving it would invalidate the op11 the client already holds, because the
		// peer endpoint that was published IS this leg's port: the client would keep
		// dialling a socket that no longer exists. The seeding is applied below,
		// after every socket this sync needs exists.
		if r.legs[key] != nil {
			continue
		}
		leg, err := r.newLegLocked(key, binding)
		if err != nil {
			for _, created := range newLegs {
				r.releaseLegLocked(created)
			}
			usedPorts := len(r.usedPorts)
			r.mu.Unlock()
			// 端口池耗尽必须**显式可见**。
			//
			// 以前这里只有一条 slog warn（packet_log 与团本记录里都看不到），而它的
			// 唯一后果正好是玩家报的那个词：这一队拿不到中继 → 回退成"直连打洞" →
			// 两边都在 NAT 后面时连不上 → 客户端画红电脑。
			//
			// 数量级对照：参照实现的池子是 1000 个端口（队伍侧约 500 ≈ 40 个并发四人
			// 队）；我们默认 64 ≈ 5 个四人队（一个四人队 = 4×3 = 12 条有向腿）。
			if r.logEvent != nil {
				r.logEvent("game-party-udp-relay-port-exhausted",
					"party_id", partyID,
					"used_ports", usedPorts,
					"port_start", r.portStart,
					"port_count", r.portCount,
					"legs_needed", len(wanted),
					"reason", "no_free_ports_for_directed_legs",
					"error", err)
			}
			r.log("party UDP relay synchronization deferred", "party_id", partyID, "error", err)
			return false
		}
		newLegs[key] = leg
	}

	toClose := make([]*currentPartyUDPRelayLeg, 0)
	reseed := make([]*currentPartyUDPRelayLeg, 0, len(wanted))
	for key, existing := range r.legs {
		if key.partyID != partyID {
			continue
		}
		binding, stillWanted := wanted[key]
		if !stillWanted {
			toClose = append(toClose, existing)
			delete(r.legs, key)
			delete(r.usedPorts, int(existing.port))
			continue
		}
		if existing.binding != binding {
			reseed = append(reseed, existing)
		}
	}
	for key, leg := range newLegs {
		r.legs[key] = leg
	}
	r.mu.Unlock()

	for _, leg := range toClose {
		_ = leg.conn.Close()
	}
	for _, leg := range reseed {
		leg.rebind(wanted[leg.key])
	}
	for _, leg := range newLegs {
		r.wg.Add(1)
		go r.readLoop(leg)
	}
	return true
}

// CloseParty releases all directed ports for a party immediately. It is used
// when the final member leaves or an aggregate generation is retired.
func (r *currentPartyUDPRelay) CloseParty(partyID uint16) {
	if r == nil || partyID == 0 {
		return
	}
	r.mu.Lock()
	legs := r.removePartyLocked(partyID)
	r.mu.Unlock()
	for _, leg := range legs {
		_ = leg.conn.Close()
	}
}

func (r *currentPartyUDPRelay) Close() error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil
	}
	r.closed = true
	legs := make([]*currentPartyUDPRelayLeg, 0, len(r.legs))
	for _, leg := range r.legs {
		legs = append(legs, leg)
	}
	r.legs = make(map[currentPartyUDPRelayLegKey]*currentPartyUDPRelayLeg)
	r.usedPorts = make(map[int]struct{})
	r.mu.Unlock()
	var closeErr error
	for _, leg := range legs {
		closeErr = errors.Join(closeErr, leg.conn.Close())
	}
	r.wg.Wait()
	return closeErr
}

func (r *currentPartyUDPRelay) Endpoint(partyID, ownerID, peerID uint16) ([4]byte, uint16, bool) {
	if r == nil || !r.enabled || partyID == 0 || ownerID == 0 || peerID == 0 || ownerID == peerID {
		return [4]byte{}, 0, false
	}
	r.mu.Lock()
	leg := r.legs[currentPartyUDPRelayLegKey{partyID: partyID, ownerID: ownerID, peerID: peerID}]
	address := r.advertise
	r.mu.Unlock()
	if leg == nil {
		return [4]byte{}, 0, false
	}
	return address, leg.port, true
}

func (r *currentPartyUDPRelay) newLegLocked(key currentPartyUDPRelayLegKey, binding currentPartyUDPRelayBinding) (*currentPartyUDPRelayLeg, error) {
	for offset := 0; offset < r.portCount; offset++ {
		port := r.portStart + offset
		if _, used := r.usedPorts[port]; used {
			continue
		}
		conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: append(net.IP(nil), r.bindIP...), Port: port})
		if err != nil {
			continue
		}
		r.usedPorts[port] = struct{}{}
		leg := &currentPartyUDPRelayLeg{key: key, binding: binding, conn: conn, port: uint16(port)}
		if binding.port != 0 {
			leg.learnedEndpoint = &net.UDPAddr{
				IP:   net.IPv4(binding.address[0], binding.address[1], binding.address[2], binding.address[3]),
				Port: int(binding.port),
			}
		}
		return leg, nil
	}
	return nil, fmt.Errorf("no free ports in configured range %d-%d", r.portStart, r.portStart+r.portCount-1)
}

func (r *currentPartyUDPRelay) releaseLegLocked(leg *currentPartyUDPRelayLeg) {
	if leg == nil {
		return
	}
	delete(r.usedPorts, int(leg.port))
	_ = leg.conn.Close()
}

// rebind applies a changed binding to an existing leg without touching its
// socket. The ingress filter follows the member's current authenticated TCP
// identity, and a newly learned return port seeds the leg only while it has no
// live endpoint yet: once traffic has been seen, the observed source tuple is
// stronger evidence than the registered probe port (NoPack may send reliable
// party traffic from a second socket owned by the same process).
func (leg *currentPartyUDPRelayLeg) rebind(binding currentPartyUDPRelayBinding) {
	if leg == nil {
		return
	}
	leg.mu.Lock()
	defer leg.mu.Unlock()
	leg.binding = binding
	if leg.learnedEndpoint == nil && binding.port != 0 {
		leg.learnedEndpoint = &net.UDPAddr{
			IP:   net.IPv4(binding.address[0], binding.address[1], binding.address[2], binding.address[3]),
			Port: int(binding.port),
		}
	}
}

// expectedSourceAddress is the leg's ingress filter as the read loop sees it.
func (leg *currentPartyUDPRelayLeg) expectedSourceAddress() [4]byte {
	if leg == nil {
		return [4]byte{}
	}
	leg.mu.Lock()
	defer leg.mu.Unlock()
	return leg.binding.address
}

func (r *currentPartyUDPRelay) removePartyLocked(partyID uint16) []*currentPartyUDPRelayLeg {
	legs := make([]*currentPartyUDPRelayLeg, 0)
	for key, leg := range r.legs {
		if key.partyID != partyID {
			continue
		}
		delete(r.legs, key)
		delete(r.usedPorts, int(leg.port))
		legs = append(legs, leg)
	}
	return legs
}

func (r *currentPartyUDPRelay) readLoop(leg *currentPartyUDPRelayLeg) {
	defer r.wg.Done()
	// Every close path ends this loop through net.ErrClosed, so this is the one
	// choke point where a leg's totals can be reported exactly once.
	defer r.logLegTotals(leg)
	buffer := make([]byte, currentPartyUDPRelayMaxDatagram)
	for {
		n, source, err := leg.conn.ReadFromUDP(buffer)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			// A transient UDP read error (notably an ICMP reset on Windows) must
			// not retire this directed leg.  The opposite direction may still be
			// healthy and native reliable traffic will retransmit on the same
			// socket.
			r.log("party UDP relay read deferred",
				"party_id", leg.key.partyID,
				"owner_id", leg.key.ownerID,
				"peer_id", leg.key.peerID,
				"relay_port", leg.port,
				"error", err)
			continue
		}
		if n <= 0 || !currentPartyUDPRelaySourceMatches(source, leg.expectedSourceAddress()) {
			continue
		}
		// The native reachability probe seeds the first return destination, so
		// the initial peer handshake can cross before both sides have spoken.
		// NoPack may then send reliable party traffic from a second UDP socket
		// owned by the same authenticated process/IP. That live source tuple is
		// stronger return-path evidence than the probe socket: retaining the seed
		// permanently makes one direction work while replies are delivered to the
		// inactive probe port. Learn every accepted live source port after the
		// concrete TCP-owned IPv4 check above.
		leg.mu.Lock()
		previousEndpoint := cloneCurrentPartyUDPRelayAddr(leg.learnedEndpoint)
		leg.learnedEndpoint = cloneCurrentPartyUDPRelayAddr(source)
		leg.ingressPackets++
		ingressPackets := leg.ingressPackets
		leg.mu.Unlock()
		// The forwarding budget is spent before anything the relay does per
		// datagram -- including the packet-log dump below. A datagram this leg is
		// not allowed to forward must not be able to cost the server disk, log
		// volume or the sampler's rate either; that was half of what the measured
		// storm took.
		if admitted, reportThrottle, throttledTotal := leg.admitForward(time.Now()); !admitted {
			if reportThrottle {
				r.logThrottled(leg, ingressPackets, throttledTotal)
			}
			continue
		}
		if r.payloadDump != nil {
			r.payloadDump(leg.key, source, buffer[:n])
		}
		if currentPartyUDPRelayTraceDatagram(ingressPackets) {
			args := []any{
				"party_id", leg.key.partyID,
				"owner_id", leg.key.ownerID,
				"peer_id", leg.key.peerID,
				"relay_port", leg.port,
				"source", source.String(),
				"bytes", n,
				"packet_count", ingressPackets,
			}
			if ingressPackets <= 8 {
				args = append(args, "payload_hex", hex.EncodeToString(buffer[:n]))
			}
			r.log("party UDP relay received datagram trace", args...)
		}
		if previousEndpoint != nil && previousEndpoint.Port != source.Port {
			r.log("party UDP relay learned active source port",
				"party_id", leg.key.partyID,
				"owner_id", leg.key.ownerID,
				"peer_id", leg.key.peerID,
				"registered_port", previousEndpoint.Port,
				"active_port", source.Port)
		}

		r.mu.Lock()
		opposite := r.legs[currentPartyUDPRelayLegKey{
			partyID: leg.key.partyID,
			ownerID: leg.key.peerID,
			peerID:  leg.key.ownerID,
		}]
		r.mu.Unlock()
		var destination *net.UDPAddr
		if opposite != nil {
			opposite.mu.Lock()
			destination = cloneCurrentPartyUDPRelayAddr(opposite.learnedEndpoint)
			opposite.mu.Unlock()
		}
		if destination == nil {
			leg.mu.Lock()
			leg.droppedPackets++
			firstDrop := !leg.droppedLogged
			leg.droppedLogged = true
			leg.mu.Unlock()
			if firstDrop {
				// The opposite leg only learns its endpoint from a datagram the
				// peer itself sends, so a peer that never initiates silently
				// discards everything sent towards it.
				r.log("party UDP relay has no learned peer endpoint yet; payload dropped",
					"party_id", leg.key.partyID,
					"owner_id", leg.key.ownerID,
					"peer_id", leg.key.peerID,
					"relay_port", leg.port,
					"opposite_leg_present", opposite != nil)
			}
			continue
		}
		if _, err := opposite.conn.WriteToUDP(buffer[:n], destination); err != nil {
			r.log("party UDP relay forward failed", "party_id", leg.key.partyID, "owner_id", leg.key.ownerID, "peer_id", leg.key.peerID, "error", err)
			continue
		}
		leg.mu.Lock()
		leg.forwardedPackets++
		forwardedPackets := leg.forwardedPackets
		leg.mu.Unlock()
		if currentPartyUDPRelayTraceDatagram(forwardedPackets) {
			r.log("party UDP relay forwarded datagram trace",
				"party_id", leg.key.partyID,
				"owner_id", leg.key.ownerID,
				"peer_id", leg.key.peerID,
				"ingress_port", leg.port,
				"egress_port", opposite.port,
				"destination", destination.String(),
				"bytes", n,
				"packet_count", forwardedPackets)
		}
	}
}

// logLegTotals reports one directed leg's lifetime traffic. A leg that closes
// with received=0 proves the owning client never sent P2P payload towards that
// peer at all, which no other server-side signal can distinguish from a
// working relay.
func (r *currentPartyUDPRelay) logLegTotals(leg *currentPartyUDPRelayLeg) {
	if r == nil || leg == nil {
		return
	}
	leg.mu.Lock()
	received := leg.ingressPackets
	forwarded := leg.forwardedPackets
	dropped := leg.droppedPackets
	throttled := leg.throttledPackets
	leg.mu.Unlock()
	r.log("party UDP relay leg closed",
		"party_id", leg.key.partyID,
		"owner_id", leg.key.ownerID,
		"peer_id", leg.key.peerID,
		"relay_port", leg.port,
		"received", received,
		"forwarded", forwarded,
		"dropped", dropped,
		"throttled", throttled)
}

// admitForward spends one datagram of this leg's forwarding budget and reports
// whether it may be forwarded, whether the refusal is worth a log line, and the
// leg's running throttle total.
//
// It is a token bucket, refilled at currentPartyUDPRelayForwardRatePerSecond up
// to currentPartyUDPRelayForwardBurstPackets. A leg that has never been throttled
// starts with a full bucket, so a normal party never notices it exists; only a
// sustained rate above the budget ever empties it. A clock that stands still or
// jumps backwards refills nothing, which is the safe direction.
func (leg *currentPartyUDPRelayLeg) admitForward(now time.Time) (bool, bool, uint64) {
	if leg == nil {
		return false, false, 0
	}
	leg.mu.Lock()
	defer leg.mu.Unlock()
	if leg.forwardRefilledAt.IsZero() {
		leg.forwardRefilledAt = now
		leg.forwardTokens = currentPartyUDPRelayForwardBurstPackets
	}
	if elapsed := now.Sub(leg.forwardRefilledAt); elapsed > 0 {
		leg.forwardTokens += elapsed.Seconds() * currentPartyUDPRelayForwardRatePerSecond
		if leg.forwardTokens > currentPartyUDPRelayForwardBurstPackets {
			leg.forwardTokens = currentPartyUDPRelayForwardBurstPackets
		}
		leg.forwardRefilledAt = now
	}
	if leg.forwardTokens >= 1 {
		leg.forwardTokens--
		return true, false, leg.throttledPackets
	}
	leg.throttledPackets++
	total := leg.throttledPackets
	if leg.throttleLoggedAt.IsZero() || now.Sub(leg.throttleLoggedAt) >= currentPartyUDPRelayThrottleLogInterval {
		leg.throttleLoggedAt = now
		return false, true, total
	}
	return false, false, total
}

// logThrottled reports one budget refusal. It is an event, not a warning: a
// throttled leg is the server defending itself, and the operator needs the
// numbers (offered, refused so far) next to the leg identity to tell a storm
// from an under-sized budget.
func (r *currentPartyUDPRelay) logThrottled(leg *currentPartyUDPRelayLeg, ingressPackets uint64, throttledTotal uint64) {
	if r == nil || leg == nil {
		return
	}
	if r.logEvent != nil {
		r.logEvent("game-party-udp-relay-throttled",
			"party_id", leg.key.partyID,
			"owner_id", leg.key.ownerID,
			"peer_id", leg.key.peerID,
			"relay_port", leg.port,
			"ingress_packets", ingressPackets,
			"throttled_packets", throttledTotal,
			"rate_per_second", currentPartyUDPRelayForwardRatePerSecond,
			"burst_packets", currentPartyUDPRelayForwardBurstPackets,
			"policy", "drop")
		return
	}
	r.log("party UDP relay throttled a datagram storm",
		"party_id", leg.key.partyID,
		"owner_id", leg.key.ownerID,
		"peer_id", leg.key.peerID,
		"relay_port", leg.port,
		"ingress_packets", ingressPackets,
		"throttled_packets", throttledTotal)
}

func currentPartyUDPRelayTraceDatagram(count uint64) bool {
	return count > 0 && (count <= 8 || count&(count-1) == 0)
}

// currentPartyUDPRelayDumpHeadPackets is how many datagrams of a leg are dumped in
// full before the dump switches to sampling.
const currentPartyUDPRelayDumpHeadPackets = 16

// currentPartyUDPRelayDumpDatagram decides whether one accepted datagram is worth
// dumping to the packet log. Two clients in one town emit thousands of datagrams
// per second, and a full hex transcript of that is not a diagnostic -- it is a
// disk firehose that starves the packet path it is meant to explain (228 MB in
// five minutes on 2026-09-16). The head of each leg is kept, then one datagram in
// a thousand, which still shows what the peers exchange without the flood.
func currentPartyUDPRelayDumpDatagram(count int) bool {
	if count <= currentPartyUDPRelayDumpHeadPackets {
		return true
	}
	return count%1000 == 0
}

func (r *currentPartyUDPRelay) log(message string, args ...any) {
	if r != nil && r.logWarn != nil {
		r.logWarn(message, args...)
	}
}

func currentPartyUDPRelayBindings(bindings []currentPartyUDPRelayBinding) []currentPartyUDPRelayBinding {
	unique := make(map[uint16]currentPartyUDPRelayBinding, len(bindings))
	for _, binding := range bindings {
		if binding.characterID == 0 || binding.generation == 0 {
			return nil
		}
		if _, exists := unique[binding.characterID]; exists {
			return nil
		}
		unique[binding.characterID] = binding
	}
	result := make([]currentPartyUDPRelayBinding, 0, len(unique))
	for _, binding := range unique {
		result = append(result, binding)
	}
	sort.Slice(result, func(left, right int) bool { return result[left].characterID < result[right].characterID })
	return result
}

func currentPartyUDPRelaySourceMatches(source *net.UDPAddr, expected [4]byte) bool {
	if source == nil || source.IP == nil {
		return false
	}
	ip := source.IP.To4()
	return len(ip) == net.IPv4len && ip[0] == expected[0] && ip[1] == expected[1] && ip[2] == expected[2] && ip[3] == expected[3]
}

func cloneCurrentPartyUDPRelayAddr(source *net.UDPAddr) *net.UDPAddr {
	if source == nil {
		return nil
	}
	return &net.UDPAddr{IP: append(net.IP(nil), source.IP...), Port: source.Port, Zone: source.Zone}
}
