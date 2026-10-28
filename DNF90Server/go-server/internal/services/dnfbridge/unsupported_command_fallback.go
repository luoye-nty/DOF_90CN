package dnfbridge

import (
	"encoding/binary"
	"hash/fnv"
	"math/bits"
	"sync"

	dnfenum "longheng.io/server/internal/modules/dnf/dnfenum"
	dnfproto "longheng.io/server/internal/modules/dnf/protocol"
)

const (
	// 1456 is an intentional hole in the current CmdPacket enum. The matching
	// 90CN compatibility DLL uses it only for the C2S response-capability
	// announcement; the server never reflects this opcode to the client.
	currentCommandCapabilityOpcode       = uint16(1456)
	currentCommandCapabilityMagic        = uint32(0x314B4341) // "ACK1"
	currentCommandCapabilityVersion      = uint16(1)
	currentCommandCapabilityEnumEnd      = uint16(dnfenum.CmdPacketEnd)
	currentCommandCapabilityBitmapLength = (currentCommandCapabilityEnumEnd + 7) / 8
	currentCommandCapabilityHeaderLength = 10
	currentUnsupportedCommandFailureCode = byte(1)
)

type currentCommandFallbackSessionState struct {
	mu           sync.RWMutex
	ready        bool
	bitmap       [currentCommandCapabilityBitmapLength]byte
	handlerCount int
	fingerprint  uint32
}

type currentCommandCapabilities struct {
	bitmap       [currentCommandCapabilityBitmapLength]byte
	handlerCount int
	fingerprint  uint32
}

func parseCurrentCommandCapabilities(body []byte) (currentCommandCapabilities, bool) {
	var result currentCommandCapabilities
	if len(body) != currentCommandCapabilityHeaderLength+int(currentCommandCapabilityBitmapLength) ||
		binary.LittleEndian.Uint32(body[0:4]) != currentCommandCapabilityMagic ||
		binary.LittleEndian.Uint16(body[4:6]) != currentCommandCapabilityVersion ||
		binary.LittleEndian.Uint16(body[6:8]) != currentCommandCapabilityEnumEnd ||
		binary.LittleEndian.Uint16(body[8:10]) != currentCommandCapabilityBitmapLength {
		return currentCommandCapabilities{}, false
	}
	copy(result.bitmap[:], body[currentCommandCapabilityHeaderLength:])
	// The enum end is exclusive. The final storage bit represents the sentinel
	// itself and must remain clear.
	if result.bitmap[len(result.bitmap)-1]&0x80 != 0 {
		return currentCommandCapabilities{}, false
	}
	hash := fnv.New32a()
	_, _ = hash.Write(result.bitmap[:])
	result.fingerprint = hash.Sum32()
	for _, value := range result.bitmap {
		result.handlerCount += bits.OnesCount8(value)
	}
	if result.handlerCount == 0 {
		return currentCommandCapabilities{}, false
	}
	return result, true
}

func (state *currentCommandFallbackSessionState) replace(capabilities currentCommandCapabilities) {
	if state == nil {
		return
	}
	state.mu.Lock()
	state.bitmap = capabilities.bitmap
	state.handlerCount = capabilities.handlerCount
	state.fingerprint = capabilities.fingerprint
	state.ready = true
	state.mu.Unlock()
}

func (state *currentCommandFallbackSessionState) hasClass1Handler(opcode uint16) bool {
	if state == nil || opcode >= currentCommandCapabilityEnumEnd {
		return false
	}
	state.mu.RLock()
	ready := state.ready
	value := state.bitmap[opcode>>3] & byte(1<<(opcode&7))
	state.mu.RUnlock()
	return ready && value != 0
}

func (s *Service) handleCurrentCommandCapabilities(
	session *gameSession,
	classification byte,
	body []byte,
) error {
	if session == nil || classification != dnfproto.DefaultChannelClassification {
		if session != nil {
			s.logGameEvent(session, "game-command-capability-rejected",
				"classification", classification,
				"body_len", len(body),
				"reason", "class1_capability_announcement_required")
		}
		return nil
	}
	capabilities, ok := parseCurrentCommandCapabilities(body)
	if !ok {
		s.logGameEvent(session, "game-command-capability-rejected",
			"classification", classification,
			"body_len", len(body),
			"reason", "invalid_current_client_capability_envelope")
		return nil
	}
	session.commandFallback.replace(capabilities)
	s.logGameEvent(session, "game-command-capability-accepted",
		"version", currentCommandCapabilityVersion,
		"enum_end", currentCommandCapabilityEnumEnd,
		"bitmap_bytes", currentCommandCapabilityBitmapLength,
		"class1_handlers", capabilities.handlerCount,
		"fingerprint", capabilities.fingerprint)
	return nil
}

func (s *Service) rejectUnsupportedInteractiveCommand(
	session *gameSession,
	opcode uint16,
	bodyLength int,
	transport string,
) (bool, error) {
	if session == nil || opcode == currentCommandCapabilityOpcode ||
		!session.commandFallback.hasClass1Handler(opcode) {
		return false, nil
	}
	s.logGameEvent(session, "game-command-unsupported-safe-failure",
		"transport", transport,
		"opcode", opcode,
		"runtime_cmd_name", dnfenum.CmdPacketName(opcode),
		"body_len", bodyLength,
		"failure_code", currentUnsupportedCommandFailureCode,
		"reason", "current_exe_registered_class1_handler_and_server_owner_missing")
	return true, s.sendGameUpperFailure(session, opcode, currentUnsupportedCommandFailureCode)
}
