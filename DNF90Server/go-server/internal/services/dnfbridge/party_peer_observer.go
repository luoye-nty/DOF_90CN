package dnfbridge

import (
	"encoding/binary"
	"net"
	"net/netip"
	"time"
)

// currentPartyUDPEndpointObservationTTL bounds the address-keyed observations,
// whose only consumer is the fail-closed "exactly one session on this remote
// IP" correlation. Keeping them short prevents a stale port from a previous
// client on the same NAT from being attributed to the next one.
const currentPartyUDPEndpointObservationTTL = 30 * time.Second

// currentPartyUDPEndpointPIDObservationTTL bounds the PID-keyed observations.
// The current client sends its single native reachability probe right after
// the login handshake, before any character is selected, and never repeats it;
// a player who sits on the character screen (live 2026-09-06: probes at 22:08:09
// and 22:11:37, characters bound at 22:12:09-22:12:12) therefore binds several
// minutes later. With the 30-second TTL the PID entry had already been pruned
// by then, restoreCurrentPartyUDPEndpoint found nothing, both members kept a
// zero port, runtimePartyNativePeerEndpointsReady stayed false and every party
// formation was silently deferred. A PID is an exact local-process identity:
// a reused PID belongs to a new client process that probes again on login and
// overwrites the entry, so the observation may live as long as the process.
const currentPartyUDPEndpointPIDObservationTTL = 24 * time.Hour

type currentPartyUDPEndpointObservation struct {
	endpoint   netip.AddrPort
	observedAt time.Time
}

type currentPartyUDPEndpointRegistry struct {
	byPID     map[uint32]currentPartyUDPEndpointObservation
	byAddress map[netip.Addr]map[uint16]time.Time
}

func newCurrentPartyUDPEndpointRegistry() currentPartyUDPEndpointRegistry {
	return currentPartyUDPEndpointRegistry{
		byPID:     make(map[uint32]currentPartyUDPEndpointObservation),
		byAddress: make(map[netip.Addr]map[uint16]time.Time),
	}
}

func (s *Service) observeCurrentPartyUDPEndpoint(endpoint netip.AddrPort) {
	if s == nil || !endpoint.IsValid() || !endpoint.Addr().Is4() || endpoint.Port() == 0 {
		return
	}
	endpoint = netip.AddrPortFrom(endpoint.Addr().Unmap(), endpoint.Port())
	now := time.Now().UTC()

	var ownerPID uint32
	serverAddress, _ := netip.ParseAddr(s.options.serverIP)
	if endpoint.Addr().IsLoopback() || serverAddress.IsValid() && endpoint.Addr() == serverAddress.Unmap() {
		resolver := s.partyUDPOwnerPID
		if resolver == nil {
			resolver = localUDPEndpointOwnerPID
		}
		ownerPID, _ = resolver(endpoint)
	}

	s.partyEndpointMu.Lock()
	s.pruneCurrentPartyUDPEndpointsLocked(now)
	if s.partyEndpoints.byPID == nil {
		s.partyEndpoints = newCurrentPartyUDPEndpointRegistry()
	}
	ports := s.partyEndpoints.byAddress[endpoint.Addr()]
	if ports == nil {
		ports = make(map[uint16]time.Time)
		s.partyEndpoints.byAddress[endpoint.Addr()] = ports
	}
	ports[endpoint.Port()] = now
	if ownerPID != 0 {
		s.partyEndpoints.byPID[ownerPID] = currentPartyUDPEndpointObservation{
			endpoint: endpoint, observedAt: now,
		}
	}
	s.partyEndpointMu.Unlock()

	sessions := s.currentPartyUDPEndpointSessions(endpoint.Addr(), ownerPID)
	for _, session := range sessions {
		if currentPartyPeerPort(session) != 0 {
			continue
		}
		if s.applyCurrentPartyUDPEndpoint(session, endpoint, "native_udp_probe") {
			s.logGameEvent(session, "game-party-peer-endpoint-observed",
				"char_id", session.selectedCharacterID,
				"client_pid", session.clientPID,
				"ipv4", endpoint.Addr(),
				"udp_port", endpoint.Port(),
				"source", "native_udp_reachability_probe")
			break
		}
	}
}

func (s *Service) currentPartyUDPEndpointSessions(address netip.Addr, ownerPID uint32) []*gameSession {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	result := make([]*gameSession, 0, 1)
	seen := make(map[*gameSession]struct{})
	for _, session := range s.gameSessions {
		if session == nil {
			continue
		}
		if ownerPID != 0 {
			if session.clientPID != ownerPID {
				continue
			}
		} else if currentPartySessionRemoteAddress(session) != address {
			continue
		}
		if _, duplicate := seen[session]; duplicate {
			continue
		}
		seen[session] = struct{}{}
		result = append(result, session)
	}
	// A source PID is an exact local-process identity. Remote IP correlation is
	// accepted for any current session: each observed endpoint binds to the
	// first unbound session for that address, letting shared-NAT dual-login
	// clients (owner PID is not resolvable on public addresses) register their
	// probe ports instead of failing closed on ambiguity.
	return result
}

func (s *Service) restoreCurrentPartyUDPEndpoint(session *gameSession) {
	if s == nil || session == nil {
		return
	}
	now := time.Now().UTC()
	var observation currentPartyUDPEndpointObservation
	var found bool
	s.partyEndpointMu.Lock()
	s.pruneCurrentPartyUDPEndpointsLocked(now)
	if session.clientPID != 0 {
		observation, found = s.partyEndpoints.byPID[session.clientPID]
	}
	if !found {
		address := currentPartySessionRemoteAddress(session)
		ports := s.partyEndpoints.byAddress[address]
		if len(ports) == 1 && len(s.currentPartyUDPEndpointSessionsWithoutLock(address)) == 1 {
			for port, observedAt := range ports {
				observation = currentPartyUDPEndpointObservation{
					endpoint: netip.AddrPortFrom(address, port), observedAt: observedAt,
				}
				found = true
			}
		}
	}
	s.partyEndpointMu.Unlock()
	if found {
		s.applyCurrentPartyUDPEndpoint(session, observation.endpoint, "session_restore")
	}
}

// currentPartyUDPEndpointSessionsWithoutLock is used while partyEndpointMu is
// held. It takes only Service.mu, preserving the global lock order
// partyEndpointMu -> Service.mu used by endpoint restoration.
func (s *Service) currentPartyUDPEndpointSessionsWithoutLock(address netip.Addr) []*gameSession {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := make([]*gameSession, 0, 1)
	seen := make(map[*gameSession]struct{})
	for _, session := range s.gameSessions {
		if session == nil || currentPartySessionRemoteAddress(session) != address {
			continue
		}
		if _, duplicate := seen[session]; duplicate {
			continue
		}
		seen[session] = struct{}{}
		result = append(result, session)
	}
	return result
}

func (s *Service) pruneCurrentPartyUDPEndpointsLocked(now time.Time) {
	pidCutoff := now.Add(-currentPartyUDPEndpointPIDObservationTTL)
	for pid, observation := range s.partyEndpoints.byPID {
		if observation.observedAt.Before(pidCutoff) {
			delete(s.partyEndpoints.byPID, pid)
		}
	}
	cutoff := now.Add(-currentPartyUDPEndpointObservationTTL)
	for address, ports := range s.partyEndpoints.byAddress {
		for port, observedAt := range ports {
			if observedAt.Before(cutoff) {
				delete(ports, port)
			}
		}
		if len(ports) == 0 {
			delete(s.partyEndpoints.byAddress, address)
		}
	}
}

// currentPartyUDPEndpointObservedFrom reports whether the native reachability
// probe has seen a datagram from exactly this address/port tuple: either as a
// PID-owned observation or as a still-live address-keyed one.
func (s *Service) currentPartyUDPEndpointObservedFrom(address netip.Addr, port uint16) bool {
	if s == nil || !address.IsValid() || port == 0 {
		return false
	}
	now := time.Now().UTC()
	s.partyEndpointMu.Lock()
	defer s.partyEndpointMu.Unlock()
	if s.partyEndpoints.byPID == nil {
		return false
	}
	s.pruneCurrentPartyUDPEndpointsLocked(now)
	if _, live := s.partyEndpoints.byAddress[address][port]; live {
		return true
	}
	for _, observation := range s.partyEndpoints.byPID {
		if observation.endpoint.Addr() == address && observation.endpoint.Port() == port {
			return true
		}
	}
	return false
}

// captureCurrentPartyPeerEndpointFromOp2 is the cross-validated fallback for a
// session whose native probe observation could not be attributed to it.
//
// The 31-byte class1/op2 body carries the client's own inner/outer IPv4 and a
// network-order UDP port (the old tree's SET_UDP_IP_PORT reading). Live
// 2026-09-06: both clients' op2 ports (5063, 62926) were exactly the source
// ports the native probe had answered ten milliseconds earlier. The same-version
// evidence that op2 alone is unsafe (a false 39077 endpoint) still stands, so
// the port is accepted only when the probe has actually observed it from this
// connection's remote address, and never over a registration the probe already
// bound. This rescues two local clients whose UDP socket owner PID could not be
// resolved, where remote-IP correlation is deliberately fail-closed.
func (s *Service) captureCurrentPartyPeerEndpointFromOp2(session *gameSession, body []byte) {
	if s == nil || session == nil || len(body) != currentChannelReconnectProbeSize {
		return
	}
	port := uint16(body[9])<<8 | uint16(body[10])
	if port == 0 {
		return
	}
	// 31 字节 op2 前 15 字节即 SET_UDP_IP_PORT：nat(1) + inner(4) + outer(4)
	// + port(2, 大端) + mtu(4, 小端)。90CN 客户端不单独发 op434 path report，
	// 路径 MTU 只在这里携带；忽略它会让 op11 一直下发 512，把伤害/增益同步
	// 数据拆成大量小包而丢包（伤害变 1、buff 0 级）。
	if mtu := binary.LittleEndian.Uint32(body[11:15]); currentPartyPathMTU(mtu) {
		session.partyPeerMu.Lock()
		session.partyPathMTU = mtu
		session.partyPeerMu.Unlock()
	}
	if currentPartyPeerEndpointSnapshot(session).Port != 0 {
		return
	}
	address := currentPartySessionRemoteAddress(session)
	if !address.IsValid() || !s.currentPartyUDPEndpointObservedFrom(address, port) {
		s.logGameEvent(session, "game-party-peer-endpoint-op2-rejected",
			"char_id", session.selectedCharacterID,
			"client_pid", session.clientPID,
			"udp_port", port,
			"reason", "op2_port_not_observed_by_native_probe")
		return
	}
	endpoint := netip.AddrPortFrom(address, port)
	if session.clientPID != 0 {
		now := time.Now().UTC()
		s.partyEndpointMu.Lock()
		if s.partyEndpoints.byPID == nil {
			s.partyEndpoints = newCurrentPartyUDPEndpointRegistry()
		}
		if _, known := s.partyEndpoints.byPID[session.clientPID]; !known {
			s.partyEndpoints.byPID[session.clientPID] = currentPartyUDPEndpointObservation{
				endpoint: endpoint, observedAt: now,
			}
		}
		s.partyEndpointMu.Unlock()
	}
	if s.applyCurrentPartyUDPEndpoint(session, endpoint, "op2_port_cross_validated_by_native_probe") {
		// op2 的 inner IPv4(body[1:5])是客户端本机/内网地址。公网部署下两个
		// 客户端常在同一内网，用 inner 直连才不丢包；OuterIPv4 保持 probe
		// 观察到的公网出口(applyCurrentPartyUDPEndpoint 已写入)。
		if inner := [4]byte{body[1], body[2], body[3], body[4]}; isUsablePartyPeerIPv4(inner) {
			session.partyPeerMu.Lock()
			session.partyPeer.InnerIPv4 = inner
			session.partyPeerMu.Unlock()
		}
		s.logGameEvent(session, "game-party-peer-endpoint-observed",
			"char_id", session.selectedCharacterID,
			"client_pid", session.clientPID,
			"ipv4", address,
			"inner_ipv4", netip.AddrFrom4([4]byte{body[1], body[2], body[3], body[4]}).String(),
			"udp_port", port,
			"source", "op2_port_cross_validated_by_native_probe")
	}
}

// isUsablePartyPeerIPv4 rejects unspecified/class-0 and multicast/reserved
// class-E ranges, matching the legacy SET_UDP_IP_PORT wire parser. Private,
// CGNAT, link-local and loopback addresses remain valid; reachability is
// decided by the direct/relay path.
func isUsablePartyPeerIPv4(ip [4]byte) bool {
	return ip[0] != 0 && ip[0] < 224
}

func (s *Service) applyCurrentPartyUDPEndpoint(session *gameSession, endpoint netip.AddrPort, source string) bool {
	if session == nil || !endpoint.IsValid() || !endpoint.Addr().Is4() || endpoint.Port() == 0 {
		return false
	}
	ipv4 := endpoint.Addr().As4()
	registration := currentPartyPeerEndpointRegistration{
		InnerIPv4: ipv4,
		OuterIPv4: ipv4,
		Port:      endpoint.Port(),
		MTU:       currentPartyPeerDefaultPathMTU,
	}
	session.partyPeerMu.Lock()
	changed := session.partyPeer.InnerIPv4 != registration.InnerIPv4 ||
		session.partyPeer.OuterIPv4 != registration.OuterIPv4 ||
		session.partyPeer.Port != registration.Port
	session.partyPeer = registration
	session.partyPeerMu.Unlock()
	if changed {
		s.logPacketEvent("game-party-peer-endpoint-bound",
			"conn_id", session.connID,
			"char_id", session.selectedCharacterID,
			"client_pid", session.clientPID,
			"ipv4", endpoint.Addr(),
			"udp_port", endpoint.Port(),
			"source", source)
	}
	return changed
}

func currentPartySessionRemoteAddress(session *gameSession) netip.Addr {
	if session == nil || session.conn == nil {
		return netip.Addr{}
	}
	tcpAddress, ok := session.conn.RemoteAddr().(*net.TCPAddr)
	if !ok || tcpAddress == nil {
		return netip.Addr{}
	}
	ipv4 := tcpAddress.IP.To4()
	if ipv4 == nil {
		return netip.Addr{}
	}
	address, ok := netip.AddrFromSlice(ipv4)
	if !ok {
		return netip.Addr{}
	}
	return address.Unmap()
}
