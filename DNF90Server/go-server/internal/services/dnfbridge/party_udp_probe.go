package dnfbridge

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sort"
	"sync"
)

const (
	currentPartyUDPProbeRequestSize  = 1
	currentPartyUDPProbeResponseSize = 7
	currentPartyUDPProbeResponseType = 2
)

// currentPartyUDPProbe owns the two native reachability endpoints advertised
// in CHANNELINFO. The current client sends a one-byte datagram and expects its
// observed IPv4/source-port tuple back in the seven-byte response below.
type currentPartyUDPProbe struct {
	observe func(netip.AddrPort)
	log     func(string, ...any)

	mu      sync.Mutex
	conns   []*net.UDPConn
	closed  bool
	closeWG sync.WaitGroup
}

func newCurrentPartyUDPProbe(
	ctx context.Context,
	bindIP string,
	ports []int,
	observe func(netip.AddrPort),
	log func(string, ...any),
) (*currentPartyUDPProbe, error) {
	address, err := netip.ParseAddr(bindIP)
	if err != nil || !address.Is4() {
		return nil, fmt.Errorf("party UDP probe requires an IPv4 bind address, got %q", bindIP)
	}
	unique := make(map[int]struct{}, len(ports))
	for _, port := range ports {
		if port < 0 || port > 0xffff {
			return nil, fmt.Errorf("party UDP probe port %d is invalid", port)
		}
		unique[port] = struct{}{}
	}
	ordered := make([]int, 0, len(unique))
	for port := range unique {
		ordered = append(ordered, port)
	}
	sort.Ints(ordered)
	if len(ordered) == 0 {
		return nil, errors.New("party UDP probe requires at least one port")
	}

	probe := &currentPartyUDPProbe{observe: observe, log: log}
	for _, port := range ordered {
		conn, listenErr := net.ListenUDP("udp4", &net.UDPAddr{
			IP:   net.IP(address.AsSlice()),
			Port: port,
		})
		if listenErr != nil {
			_ = probe.Close()
			return nil, fmt.Errorf("listen party UDP probe %s:%d: %w", address, port, listenErr)
		}
		probe.conns = append(probe.conns, conn)
		probe.closeWG.Add(1)
		go probe.readLoop(ctx, conn)
	}
	return probe, nil
}

func (p *currentPartyUDPProbe) Close() error {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		p.closeWG.Wait()
		return nil
	}
	p.closed = true
	conns := append([]*net.UDPConn(nil), p.conns...)
	p.conns = nil
	p.mu.Unlock()

	var closeErr error
	for _, conn := range conns {
		closeErr = errors.Join(closeErr, conn.Close())
	}
	p.closeWG.Wait()
	return closeErr
}

func (p *currentPartyUDPProbe) readLoop(ctx context.Context, conn *net.UDPConn) {
	defer p.closeWG.Done()
	buffer := make([]byte, 256)
	for {
		n, remote, err := conn.ReadFromUDP(buffer)
		if err != nil {
			if ctx.Err() == nil && !p.isClosed() {
				p.logMessage("party UDP probe read failed", "local", conn.LocalAddr(), "error", err)
			}
			return
		}
		if n != currentPartyUDPProbeRequestSize {
			p.logMessage("party UDP probe ignored", "remote", remote, "body_bytes", n, "reason", "unexpected_size")
			continue
		}
		endpoint, endpointErr := currentPartyUDPEndpoint(remote)
		if endpointErr != nil {
			p.logMessage("party UDP probe ignored", "remote", remote, "reason", endpointErr)
			continue
		}
		if p.observe != nil {
			p.observe(endpoint)
		}
		// State 5 emits a one-way zero heartbeat after reachability has already
		// been classified. The current EXE no longer calls recvfrom in that state.
		if buffer[0] == 0 {
			continue
		}
		response := buildCurrentPartyUDPProbeResponse(endpoint)
		if _, err := conn.WriteToUDP(response, remote); err != nil {
			if ctx.Err() == nil && !p.isClosed() {
				p.logMessage("party UDP probe response failed", "remote", remote, "error", err)
			}
			continue
		}
		p.logMessage("party UDP probe answered", "remote", remote, "request_type", buffer[0])
	}
}

func (p *currentPartyUDPProbe) isClosed() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.closed
}

func (p *currentPartyUDPProbe) logMessage(message string, args ...any) {
	if p != nil && p.log != nil {
		p.log(message, args...)
	}
}

func currentPartyUDPEndpoint(remote *net.UDPAddr) (netip.AddrPort, error) {
	if remote == nil || remote.Port <= 0 || remote.Port > 0xffff {
		return netip.AddrPort{}, fmt.Errorf("invalid UDP remote endpoint %v", remote)
	}
	ipv4 := remote.IP.To4()
	if ipv4 == nil {
		return netip.AddrPort{}, fmt.Errorf("UDP remote endpoint %v is not IPv4", remote)
	}
	address, ok := netip.AddrFromSlice(ipv4)
	if !ok {
		return netip.AddrPort{}, fmt.Errorf("UDP remote endpoint %v cannot be represented", remote)
	}
	return netip.AddrPortFrom(address.Unmap(), uint16(remote.Port)), nil
}

func buildCurrentPartyUDPProbeResponse(endpoint netip.AddrPort) []byte {
	response := make([]byte, currentPartyUDPProbeResponseSize)
	response[0] = currentPartyUDPProbeResponseType
	if !endpoint.IsValid() || !endpoint.Addr().Is4() {
		return response
	}
	ipv4 := endpoint.Addr().As4()
	// NoPack reads host-order integers and then calls htonl/htons, so these
	// reflected fields are intentionally little-endian on the wire.
	response[1] = ipv4[3]
	response[2] = ipv4[2]
	response[3] = ipv4[1]
	response[4] = ipv4[0]
	binary.LittleEndian.PutUint16(response[5:7], endpoint.Port())
	return response
}
