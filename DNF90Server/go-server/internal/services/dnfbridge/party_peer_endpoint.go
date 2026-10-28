package dnfbridge

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"

	"longheng.io/server/internal/modules/dnf/dnfenum"
)

const (
	currentPartyPeerDefaultPathMTU = uint32(1472)
	currentPartyNetworkReportSize  = 18
	currentPartyNetworkVersion     = uint32(5)
	currentPartyPeerStateOpcode    = uint16(97)
	currentPartyHolePunchOpcode    = uint16(966)
	currentPartyAveragePingOpcode  = uint16(1295)
)

// currentPartyClientInputRejection wraps one unproven client shape in the
// non-fatal sentinel and keeps every fact a capture needs in the message.
//
// The native P2P telemetry opcodes below are written by a client generation
// whose exact layouts this bridge recovers by observation. A body it does not
// recognise therefore means "shape not proved yet", not "session is broken";
// returning the bare parser error made the fatal inbound boundary close the game
// connection instead (see errGameClientInputRejected for the live incident).
func currentPartyClientInputRejection(opcode uint16, body []byte, reason error) error {
	return fmt.Errorf("%w: type=%d body_len=%d body_hex=%s: %v",
		errGameClientInputRejected, opcode, len(body), hex.EncodeToString(body), reason)
}

type currentPartyPeerStateReport struct {
	InternalStateWasZero byte
	PeerActor            uint16
}

func parseCurrentPartyPeerStateReport(body []byte) (currentPartyPeerStateReport, error) {
	if len(body) != 3 {
		return currentPartyPeerStateReport{}, fmt.Errorf("current party peer-state body length %d, want 3", len(body))
	}
	if body[0] > 1 {
		return currentPartyPeerStateReport{}, fmt.Errorf("current party peer-state flag %d, want 0 or 1", body[0])
	}
	return currentPartyPeerStateReport{
		InternalStateWasZero: body[0],
		PeerActor:            binary.LittleEndian.Uint16(body[1:3]),
	}, nil
}

type currentPartyHolePunchReport struct {
	OpaqueMode          byte
	OpaqueSequence      uint32
	ObservationWindowMS uint32
	OpaqueU32A          uint32
	OpaqueU32B          uint32
	OpaqueU32C          uint32
	PeerActor           uint16
}

func parseCurrentPartyHolePunchReport(body []byte) (currentPartyHolePunchReport, error) {
	if len(body) != 23 {
		return currentPartyHolePunchReport{}, fmt.Errorf("current party hole-punch body length %d, want 23", len(body))
	}
	return currentPartyHolePunchReport{
		OpaqueMode:          body[0],
		OpaqueSequence:      binary.LittleEndian.Uint32(body[1:5]),
		ObservationWindowMS: binary.LittleEndian.Uint32(body[5:9]),
		OpaqueU32A:          binary.LittleEndian.Uint32(body[9:13]),
		OpaqueU32B:          binary.LittleEndian.Uint32(body[13:17]),
		OpaqueU32C:          binary.LittleEndian.Uint32(body[17:21]),
		PeerActor:           binary.LittleEndian.Uint16(body[21:23]),
	}, nil
}

type currentPartyAveragePingReport struct {
	Slot          uint16
	PeerKey       uint32
	AveragePingMS uint32
}

func parseCurrentPartyAveragePingReport(body []byte) (currentPartyAveragePingReport, error) {
	if len(body) != 10 {
		return currentPartyAveragePingReport{}, fmt.Errorf("current party average-ping body length %d, want 10", len(body))
	}
	report := currentPartyAveragePingReport{
		Slot:          binary.LittleEndian.Uint16(body[0:2]),
		PeerKey:       binary.LittleEndian.Uint32(body[2:6]),
		AveragePingMS: binary.LittleEndian.Uint32(body[6:10]),
	}
	if report.Slot >= 8 {
		return currentPartyAveragePingReport{}, fmt.Errorf("current party average-ping slot %d, want 0..7", report.Slot)
	}
	return report, nil
}

func (s *Service) handleCurrentPartyTransportTelemetry(session *gameSession, opcode uint16, body []byte) error {
	switch opcode {
	case currentPartyPeerStateOpcode:
		report, err := parseCurrentPartyPeerStateReport(body)
		if err != nil {
			return currentPartyClientInputRejection(opcode, body, err)
		}
		s.logGameEvent(session, "game-party-peer-state-accepted",
			"peer_actor", report.PeerActor,
			"internal_state_was_zero", report.InternalStateWasZero)
		return nil
	case currentPartyHolePunchOpcode:
		report, err := parseCurrentPartyHolePunchReport(body)
		if err != nil {
			return currentPartyClientInputRejection(opcode, body, err)
		}
		s.logGameEvent(session, "game-party-hole-punch-accepted",
			"peer_actor", report.PeerActor,
			"observation_window_ms", report.ObservationWindowMS,
			"opaque_mode", report.OpaqueMode,
			"opaque_sequence", report.OpaqueSequence,
			"opaque_u32_a", report.OpaqueU32A,
			"opaque_u32_b", report.OpaqueU32B,
			"opaque_u32_c", report.OpaqueU32C)
		return nil
	case currentPartyAveragePingOpcode:
		report, err := parseCurrentPartyAveragePingReport(body)
		if err != nil {
			return currentPartyClientInputRejection(opcode, body, err)
		}
		s.logGameEvent(session, "game-party-average-ping-accepted",
			"slot", report.Slot,
			"peer_key", report.PeerKey,
			"average_ping_ms", report.AveragePingMS)
		return nil
	default:
		return currentPartyClientInputRejection(opcode, body,
			fmt.Errorf("unsupported current party transport telemetry opcode %d", opcode))
	}
}

func (s *Service) assignPartyTransportIdentity(session *gameSession) {
	if s == nil || session == nil {
		return
	}
	session.partyPeerMu.Lock()
	defer session.partyPeerMu.Unlock()
	s.assignPartyTransportIdentityLocked(session)
}

func (s *Service) assignPartyTransportIdentityLocked(session *gameSession) {
	if session.partyRoutingKey == 0 {
		for session.partyRoutingKey == 0 {
			session.partyRoutingKey = s.nextPartyRoutingKey.Add(1)
		}
	}
	if session.partyPathMTU == 0 {
		session.partyPathMTU = currentPartyPeerDefaultPathMTU
	}
}

func (s *Service) partyTransportIdentity(session *gameSession) (uint32, uint32) {
	if s == nil || session == nil {
		return 0, 0
	}
	session.partyPeerMu.Lock()
	defer session.partyPeerMu.Unlock()
	s.assignPartyTransportIdentityLocked(session)
	return session.partyRoutingKey, session.partyPathMTU
}

func currentPartyPathMTU(value uint32) bool {
	switch value {
	case 512, 1024, 1280, 1472:
		return true
	default:
		return false
	}
}

// currentPartyPeerEndpointRegistration is the authenticated source tuple
// observed by the native UDP reachability service. It must never be populated
// from the unrelated 31-byte class1/op2 channel-reconnect probe.
type currentPartyPeerEndpointRegistration struct {
	CharacterAttribute byte
	InnerIPv4          [4]byte
	OuterIPv4          [4]byte
	Port               uint16
	MTU                uint32
}

func currentPartyPeerPort(session *gameSession) uint16 {
	return currentPartyPeerEndpointSnapshot(session).Port
}

func currentPartyPeerEndpointSnapshot(session *gameSession) currentPartyPeerEndpointRegistration {
	if session == nil {
		return currentPartyPeerEndpointRegistration{}
	}
	session.partyPeerMu.Lock()
	defer session.partyPeerMu.Unlock()
	return session.partyPeer
}

func currentPartyPeerAdvertisedMTU(registration currentPartyPeerEndpointRegistration) uint32 {
	if !currentPartyPathMTU(registration.MTU) {
		return 0
	}
	return registration.MTU
}

// handleCurrentPartyNetworkReport consumes the exact 18-byte class1/op434
// native path report. NoPack only produces four MTU buckets and version 5;
// publishing zero or an arbitrary Ethernet MTU makes its reliable-UDP layer
// construct empty frames indefinitely.
//
// A body of the wrong length never reaches this function: legacy op434 also
// carries the 组队传送 request, and the length discriminator in
// handleCurrentPartyTeleportOrNetworkReport routes that shape elsewhere. What is
// left here is a genuine 18-byte report with an unproven MTU/version, which is
// refused as client input instead of as a session failure.
func (s *Service) handleCurrentPartyNetworkReport(session *gameSession, body []byte) error {
	opcode := uint16(dnfenum.CmdPacketPartyTeleport)
	if session == nil || len(body) != currentPartyNetworkReportSize {
		return currentPartyClientInputRejection(opcode, body,
			fmt.Errorf("current party network report body length %d, want %d", len(body), currentPartyNetworkReportSize))
	}
	pathMTU := binary.LittleEndian.Uint32(body[0:4])
	version := binary.LittleEndian.Uint32(body[10:14])
	if !currentPartyPathMTU(pathMTU) {
		return currentPartyClientInputRejection(opcode, body,
			fmt.Errorf("current party network report path MTU %d is not a native bucket", pathMTU))
	}
	if version != currentPartyNetworkVersion {
		return currentPartyClientInputRejection(opcode, body,
			fmt.Errorf("current party network report version %d, want %d", version, currentPartyNetworkVersion))
	}
	routingKey, _ := s.partyTransportIdentity(session)
	session.partyPeerMu.Lock()
	session.partyPathMTU = pathMTU
	session.partyPeerMu.Unlock()
	s.logGameEvent(session, "game-party-network-report-accepted",
		"char_id", session.selectedCharacterID,
		"routing_key", routingKey,
		"path_mtu", pathMTU,
		"version", version,
		"source", "same_version_current_exe_op434_native_path_report")
	return nil
}
