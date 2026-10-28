package dnfbridge

import (
	"bytes"
	"encoding/binary"
	"testing"

	"longheng.io/server/internal/modules/dnf/channelcatalog"
	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfproto "longheng.io/server/internal/modules/dnf/protocol"
)

func buildCurrentCommandCapabilitiesTestBody(opcodes ...uint16) []byte {
	body := make([]byte, currentCommandCapabilityHeaderLength+int(currentCommandCapabilityBitmapLength))
	binary.LittleEndian.PutUint32(body[0:4], currentCommandCapabilityMagic)
	binary.LittleEndian.PutUint16(body[4:6], currentCommandCapabilityVersion)
	binary.LittleEndian.PutUint16(body[6:8], currentCommandCapabilityEnumEnd)
	binary.LittleEndian.PutUint16(body[8:10], currentCommandCapabilityBitmapLength)
	for _, opcode := range opcodes {
		if opcode < currentCommandCapabilityEnumEnd {
			body[currentCommandCapabilityHeaderLength+int(opcode>>3)] |= byte(1 << (opcode & 7))
		}
	}
	return body
}

func buildCurrentCommandFallbackLegacyPacket(opcode uint16, body []byte) []byte {
	packet := make([]byte, dnfproto.LegacyGameHeaderSize+len(body))
	packet[0] = byte(dnfenum.GameCmdCommand)
	binary.LittleEndian.PutUint16(packet[1:3], opcode)
	binary.LittleEndian.PutUint32(packet[3:7], uint32(len(packet)))
	copy(packet[dnfproto.LegacyGameHeaderSize:], body)
	return packet
}

func TestParseCurrentCommandCapabilitiesRequiresExactCurrentEnumEnvelope(t *testing.T) {
	body := buildCurrentCommandCapabilitiesTestBody(
		uint16(dnfenum.CmdPacketPVPChannelInfo),
		uint16(dnfenum.CmdPacketRequestMatch),
	)
	capabilities, ok := parseCurrentCommandCapabilities(body)
	if !ok {
		t.Fatal("valid current command capability envelope was rejected")
	}
	if capabilities.handlerCount != 2 ||
		capabilities.fingerprint == 0 ||
		capabilities.bitmap[uint16(dnfenum.CmdPacketPVPChannelInfo)>>3] == 0 {
		t.Fatalf("capabilities = %+v", capabilities)
	}

	for _, mutate := range []func([]byte){
		func(value []byte) { value[0] ^= 0xFF },
		func(value []byte) { binary.LittleEndian.PutUint16(value[4:6], 2) },
		func(value []byte) { binary.LittleEndian.PutUint16(value[6:8], currentCommandCapabilityEnumEnd-1) },
		func(value []byte) { binary.LittleEndian.PutUint16(value[8:10], currentCommandCapabilityBitmapLength-1) },
		func(value []byte) { value[len(value)-1] |= 0x80 },
		func(value []byte) {
			for index := currentCommandCapabilityHeaderLength; index < len(value); index++ {
				value[index] = 0
			}
		},
	} {
		candidate := append([]byte(nil), body...)
		mutate(candidate)
		if _, accepted := parseCurrentCommandCapabilities(candidate); accepted {
			t.Fatalf("invalid capability envelope accepted: % X", candidate[:10])
		}
	}
	if _, accepted := parseCurrentCommandCapabilities(body[:len(body)-1]); accepted {
		t.Fatal("short capability envelope accepted")
	}
}

func TestCurrentCommandCapabilityHandshakeMakesUnhandledUpperRequestFailSafe(t *testing.T) {
	service := &Service{options: options{
		gameUpperBodyCodec:       gameUpperBodyCodecPlain,
		gameUpperClientBodyCodec: gameUpperClientBodyCodecPlain,
	}}
	connection := &bufferConn{}
	session := &gameSession{
		conn:    connection,
		connID:  "command-fallback-upper",
		channel: channelcatalog.Channel{ID: 21, Type: 1, Name: "ch.21", Port: 10021},
	}
	body := buildCurrentCommandCapabilitiesTestBody(uint16(dnfenum.CmdPacketPVPChannelInfo))
	handshake, err := dnfproto.BuildChannelPacket(
		currentCommandCapabilityOpcode,
		body,
		1,
		dnfproto.DefaultChannelClassification,
	)
	if err != nil {
		t.Fatalf("build handshake: %v", err)
	}
	if err := service.handleGameUpper(session, handshake); err != nil {
		t.Fatalf("handle handshake: %v", err)
	}
	if connection.write.Len() != 0 {
		t.Fatalf("capability handshake received an invented ACK: % X", connection.write.Bytes())
	}
	if !session.commandFallback.hasClass1Handler(uint16(dnfenum.CmdPacketPVPChannelInfo)) {
		t.Fatal("PVP channel response handler capability was not stored")
	}

	request, err := dnfproto.BuildChannelPacket(
		uint16(dnfenum.CmdPacketPVPChannelInfo),
		nil,
		2,
		dnfproto.DefaultChannelClassification,
	)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if err := service.handleGameUpper(session, request); err != nil {
		t.Fatalf("handle request: %v", err)
	}
	response, rest := splitPVPChannelInfoFixed15Packet(t, connection.write.Bytes())
	if len(rest) != 0 ||
		response.msgID != uint16(dnfenum.CmdPacketPVPChannelInfo) ||
		!bytes.Equal(response.body, []byte{0, 0x15}) {
		t.Fatalf("safe failure = msg %d body=% X rest=% X",
			response.msgID, response.body, rest)
	}
}

func TestCurrentCommandFallbackAlsoClosesLegacyInteractiveRequest(t *testing.T) {
	service := &Service{options: options{gameUpperBodyCodec: gameUpperBodyCodecPlain}}
	connection := &bufferConn{}
	session := &gameSession{
		conn:    connection,
		connID:  "command-fallback-legacy",
		channel: channelcatalog.Channel{ID: 21, Type: 1, Name: "ch.21", Port: 10021},
	}
	capabilities, ok := parseCurrentCommandCapabilities(
		buildCurrentCommandCapabilitiesTestBody(uint16(dnfenum.CmdPacketPVPChannelInfo)),
	)
	if !ok {
		t.Fatal("test capability envelope rejected")
	}
	session.commandFallback.replace(capabilities)

	request := buildCurrentCommandFallbackLegacyPacket(
		uint16(dnfenum.CmdPacketPVPChannelInfo),
		nil,
	)
	if err := service.handleLegacyGamePacket(session, request); err != nil {
		t.Fatalf("handle legacy request: %v", err)
	}
	response, rest := splitPVPChannelInfoFixed15Packet(t, connection.write.Bytes())
	if len(rest) != 0 || response.msgID != uint16(dnfenum.CmdPacketPVPChannelInfo) ||
		!bytes.Equal(response.body, []byte{0, 0x15}) {
		t.Fatalf("legacy safe failure = msg %d body=% X rest=% X",
			response.msgID, response.body, rest)
	}
}

func TestCurrentInventoryClosesMalformedConsumablePendingRouteWithoutFallback(t *testing.T) {
	service := &Service{options: options{gameUpperBodyCodec: gameUpperBodyCodecPlain}}
	connection := &bufferConn{}
	session := &gameSession{
		conn:    connection,
		connID:  "command-fallback-malformed-consumable",
		channel: channelcatalog.Channel{ID: 21, Type: 1, Name: "ch.21", Port: 10021},
	}
	// op44 is exactly 15 bytes in the current client. A truncated request used
	// to enter inventory's blocked parse result and then depend on the optional
	// capability fallback. The inventory owner must now terminate the shared
	// native item-operation state with op44's exact error-17 shape even before a
	// capability bitmap is installed.
	request := buildCurrentCommandFallbackLegacyPacket(
		uint16(dnfenum.CmdPacketUseStackable),
		[]byte{0x01},
	)
	if err := service.handleLegacyGamePacket(session, request); err != nil {
		t.Fatalf("handle malformed consumable: %v", err)
	}
	response, rest := splitGameServerUpperPacket(t, connection.write.Bytes())
	if len(rest) != 0 || response.Header.MsgID != uint16(dnfenum.CmdPacketUseStackable) ||
		response.Header.Classification != dnfproto.DefaultChannelClassification ||
		!bytes.Equal(response.Body, []byte{0, 17, 0, 0, 0, 0, 0, 0, 0, 0, 0}) {
		t.Fatalf("malformed consumable safe failure = class %d msg %d body=% X rest=% X",
			response.Header.Classification, response.Header.MsgID, response.Body, rest)
	}
}

func TestCurrentInventoryClosesMalformedConsumableActionPendingRouteWithoutFallback(t *testing.T) {
	service := &Service{options: options{gameUpperBodyCodec: gameUpperBodyCodecPlain}}
	connection := &bufferConn{}
	session := &gameSession{
		conn:    connection,
		connID:  "command-fallback-malformed-consumable-action",
		channel: channelcatalog.Channel{ID: 21, Type: 1, Name: "ch.21", Port: 10021},
	}
	// Current sub_1E785B0 receives status/error from the class1 dispatcher and
	// does not read a feature tail on failure. Therefore every malformed op515
	// must still receive the exact two-byte error response without depending on
	// the optional capability bitmap.
	request := buildCurrentCommandFallbackLegacyPacket(
		uint16(dnfenum.CmdPacketUseStackableAction),
		[]byte{0x4C},
	)
	if err := service.handleLegacyGamePacket(session, request); err != nil {
		t.Fatalf("handle malformed consumable action: %v", err)
	}
	response, rest := splitGameServerUpperPacket(t, connection.write.Bytes())
	if len(rest) != 0 || response.Header.MsgID != uint16(dnfenum.CmdPacketUseStackableAction) ||
		response.Header.Classification != dnfproto.DefaultChannelClassification ||
		!bytes.Equal(response.Body, []byte{0, 17}) {
		t.Fatalf("malformed consumable action safe failure = class %d msg %d body=% X rest=% X",
			response.Header.Classification, response.Header.MsgID, response.Body, rest)
	}
}

func TestCurrentCommandFallbackKeepsUnregisteredPassivePacketSilent(t *testing.T) {
	service := &Service{options: options{
		gameUpperBodyCodec:       gameUpperBodyCodecPlain,
		gameUpperClientBodyCodec: gameUpperClientBodyCodecPlain,
	}}
	connection := &bufferConn{}
	session := &gameSession{
		conn:    connection,
		connID:  "command-fallback-passive",
		channel: channelcatalog.Channel{ID: 21, Type: 1, Name: "ch.21", Port: 10021},
	}
	capabilities, ok := parseCurrentCommandCapabilities(
		buildCurrentCommandCapabilitiesTestBody(uint16(dnfenum.CmdPacketPVPChannelInfo)),
	)
	if !ok {
		t.Fatal("test capability envelope rejected")
	}
	session.commandFallback.replace(capabilities)

	passive, err := dnfproto.BuildChannelPacket(
		uint16(dnfenum.CmdPacketSecurityStatus),
		make([]byte, 8),
		3,
		dnfproto.DefaultChannelClassification,
	)
	if err != nil {
		t.Fatalf("build passive packet: %v", err)
	}
	if err := service.handleGameUpper(session, passive); err != nil {
		t.Fatalf("handle passive packet: %v", err)
	}
	if connection.write.Len() != 0 {
		t.Fatalf("unregistered passive packet received a response: % X", connection.write.Bytes())
	}
}
