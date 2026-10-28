package protocol

import "encoding/binary"

// LatestGameStreamKind 标记 game 端口里混合出现的最新协议帧类型。
type LatestGameStreamKind uint8

const (
	LatestGameStreamTransport LatestGameStreamKind = 1
	LatestGameStreamUpper     LatestGameStreamKind = 2
	LatestGameStreamLegacy    LatestGameStreamKind = 3
	LatestGameStreamDproto    LatestGameStreamKind = 4

	currentDungeonMoveMapPlainBodySize        = 100
	currentTownSetUserAreaPlainBodySize       = 16
	currentAcceptQuestPlainBodySize           = 4
	currentSetQuestTriggerPlainBodySize       = 6
	currentDieCharacterPlainBodySize          = 4
	currentUseCoinPlainBodySize               = 2
	currentBossDieCheckPlainBodySize          = 39
	currentTutorialFlagPlainBodySize          = 6
	currentPlayResultBaseBodySize             = 42
	currentPlayResultOptionalBodySize         = 46
	currentPlayResultDynamicRowSize           = 7
	currentPlayResultMaximumDynamicRows       = 8
	currentChannelReconnectProbeSize          = 31
	observed90CNShortEndpointBodySize         = 586
	currentChannelDisplayRebindSize           = 590
	reference90CNChannelDisplayRebindBodySize = 598
	observed90CNCrackEndpointBodySize         = 610
	observed90CNShortEndpointMaxBodySize      = 630
	current90CNEndpointShortVariantSize       = 4
	current90CNEndpointAdapterRecordSize      = 32
	current90CNEndpointMaxAdapterRecords      = 32
)

// LatestGameStreamPacket 是从 game TCP 字节流切出的完整包。
type LatestGameStreamPacket struct {
	Kind LatestGameStreamKind
	Data []byte
}

// SplitLatestGameStream 从 game TCP 流中同时切出 transport frame 和 raw upper 包。
//
// 最新客户端在 game 连接上既会发送 12+8+7 的 game transport，也会发送
// 13 字节频道包头格式的 raw upper 包；拆流时必须先识别 raw upper，再按
// legacy game 和 frame+2 的 u16 totalLen 处理 transport。
func SplitLatestGameStream(buffer []byte, maxPacketBytes int) (packets []LatestGameStreamPacket, remaining []byte, skipped int, err error) {
	if maxPacketBytes <= 0 {
		maxPacketBytes = int(^uint16(0))
	}
	offset := 0
	for offset <= len(buffer)-4 {
		if packet, wait, ok := splitDprotoClientPacket(buffer, offset, maxPacketBytes); ok {
			packets = append(packets, LatestGameStreamPacket{
				Kind: LatestGameStreamDproto,
				Data: packet,
			})
			offset += len(packet)
			continue
		} else if wait {
			break
		}
		if packet, wait, ok := splitUpperPacket(buffer, offset, maxPacketBytes); ok {
			packets = append(packets, LatestGameStreamPacket{
				Kind: LatestGameStreamUpper,
				Data: packet,
			})
			offset += len(packet)
			continue
		} else if wait {
			break
		}
		if packet, wait, ok := splitLegacyGamePacket(buffer, offset, maxPacketBytes); ok {
			packets = append(packets, LatestGameStreamPacket{
				Kind: LatestGameStreamLegacy,
				Data: packet,
			})
			offset += len(packet)
			continue
		} else if wait {
			break
		}

		frameLength := int(binary.LittleEndian.Uint16(buffer[offset+2 : offset+4]))
		if frameLength < MinTCPFrameSize {
			offset++
			skipped++
			continue
		}
		if frameLength > maxPacketBytes {
			return nil, nil, skipped, ErrPacketTooLarge
		}
		if len(buffer)-offset < frameLength {
			break
		}
		packets = append(packets, LatestGameStreamPacket{
			Kind: LatestGameStreamTransport,
			Data: cloneBytes(buffer[offset : offset+frameLength]),
		})
		offset += frameLength
	}
	return packets, cloneBytes(buffer[offset:]), skipped, nil
}

func splitDprotoClientPacket(buffer []byte, offset int, maxPacketBytes int) ([]byte, bool, bool) {
	available := len(buffer) - offset
	if available < ChannelHeaderSize {
		return nil, false, false
	}
	if buffer[offset] != DefaultChannelClassification ||
		binary.LittleEndian.Uint16(buffer[offset+1:offset+3]) != DprotoClientEnvelopeOpcode {
		return nil, false, false
	}
	length := int(binary.LittleEndian.Uint32(buffer[offset+3 : offset+7]))
	if length < ChannelHeaderSize || length > maxPacketBytes {
		return nil, false, false
	}
	if available < length {
		return nil, true, false
	}
	return cloneBytes(buffer[offset : offset+length]), false, true
}

func splitLegacyGamePacket(buffer []byte, offset int, maxPacketBytes int) ([]byte, bool, bool) {
	available := len(buffer) - offset
	if available < LegacyGameHeaderSize {
		return nil, false, false
	}
	cmd := buffer[offset]
	if cmd != 0 && cmd != 1 {
		return nil, false, false
	}
	length := int(binary.LittleEndian.Uint32(buffer[offset+3 : offset+7]))
	if length < LegacyGameHeaderSize {
		return nil, false, false
	}
	if length > maxPacketBytes {
		return nil, false, false
	}
	if available < length {
		return nil, true, false
	}
	packet := cloneBytes(buffer[offset : offset+length])
	if _, err := ParseLegacyGamePacket(packet); err != nil {
		return nil, false, false
	}
	return packet, false, true
}

func splitUpperPacket(buffer []byte, offset int, maxPacketBytes int) ([]byte, bool, bool) {
	available := len(buffer) - offset
	if available < ChannelHeaderSize {
		return nil, false, false
	}
	classification := buffer[offset]
	if classification != 0 && classification != DefaultChannelClassification {
		return nil, false, false
	}
	length := int(binary.LittleEndian.Uint32(buffer[offset+3 : offset+7]))
	if length < ChannelHeaderSize {
		return nil, false, false
	}
	if length > maxPacketBytes {
		return nil, false, false
	}
	if packet, ok := splitHookedShortCreateUpper(buffer, offset, available, length); ok {
		return packet, false, true
	}
	if packet, ok := splitHookedCurrentSelectUpper(buffer, offset, available, length); ok {
		return packet, false, true
	}
	if available < length {
		return nil, true, false
	}
	packet := cloneBytes(buffer[offset : offset+length])
	parsed, err := ParseChannelPacketUnchecked(packet)
	if err != nil {
		return nil, false, false
	}
	if !isKnownClientUpperMsg(parsed.Header.MsgID, parsed.Body) {
		return nil, false, false
	}
	return packet, false, true
}

func splitHookedCurrentSelectUpper(buffer []byte, offset int, available int, headerLength int) ([]byte, bool) {
	const (
		selectMessageID       = 16
		staleSelectBodyLength = 32
		currentSelectBodySize = 21
	)
	msgID := binary.LittleEndian.Uint16(buffer[offset+1 : offset+3])
	if msgID != selectMessageID || headerLength != ChannelHeaderSize+staleSelectBodyLength {
		return nil, false
	}
	actualLength := ChannelHeaderSize + currentSelectBodySize
	if available < actualLength {
		return nil, false
	}
	packet := cloneBytes(buffer[offset : offset+actualLength])
	// The local hook copies the current 21-byte op16 body but leaves the old
	// 32-byte body length in the upper header. Correct only that observed shape
	// so the following packet header is not consumed as op16 data.
	binary.LittleEndian.PutUint32(packet[3:7], uint32(actualLength))
	return packet, true
}

func splitHookedShortCreateUpper(buffer []byte, offset int, available int, headerLength int) ([]byte, bool) {
	msgID := binary.LittleEndian.Uint16(buffer[offset+1 : offset+3])
	if msgID != 5 || headerLength <= ChannelHeaderSize {
		return nil, false
	}
	bodyOffset := offset + ChannelHeaderSize
	actualBodyLength, ok := hookedPlainCreateBodyLength(buffer[bodyOffset:])
	if !ok {
		return nil, false
	}
	actualLength := ChannelHeaderSize + actualBodyLength
	if actualLength >= headerLength {
		return nil, false
	}
	if available < actualLength {
		return nil, false
	}
	packet := cloneBytes(buffer[offset : offset+actualLength])
	// NoPack 本地 msg5 hook 只替换 body，没有同步修正 upper 头里的 length。
	// 真实创建体长度由 nameLen+固定 8 字节外观选项决定，不能写死，否则会吞掉后续 1276 心跳头。
	binary.LittleEndian.PutUint32(packet[3:7], uint32(actualLength))
	// NoPack 本地 msg5 hook 只替换 body，未同步修正 upper header length；
	// 服务端按实际 21 字节创建体切包，避免吞掉后续 1276 心跳头。
	binary.LittleEndian.PutUint32(packet[3:7], uint32(actualLength))
	return packet, true
}

func hookedPlainCreateBodyLength(body []byte) (int, bool) {
	if len(body) < 5 {
		return 0, false
	}
	if body[0] > 0x3f {
		return 0, false
	}
	nameLen := int(binary.LittleEndian.Uint32(body[1:5]))
	if nameLen < 2 || nameLen > 30 {
		return 0, false
	}
	actualBodyLength := 5 + nameLen + 8
	if len(body) < actualBodyLength {
		return 0, false
	}
	return actualBodyLength, true
}

func isKnownClientUpperMsg(msgID uint16, body []byte) bool {
	bodyLen := len(body)
	switch msgID {
	case 1:
		return isCurrent90CNEndpointBody(body)
	case 2:
		// A channel transfer opens the target game port without requesting the
		// ordinary roster and emits this exact 31-byte native upper probe.
		// Recognize it before the overlapping legacy header parser.
		return bodyLen == currentChannelReconnectProbeSize
	case 4, 7, 0x000f, 0x0010, 0x003f, 0x0078, 0x02a7, 0x02b1, 0x02b2, 0x02b5:
		return true
	case 31:
		// Current EXE sub_1C53100 writes class-1/op31 as the exact
		// u16 opcode echo followed by a u16 quest id. Requiring both the
		// four-byte size and echo prevents a same-shaped legacy packet from
		// being cut as raw upper traffic.
		return bodyLen == currentAcceptQuestPlainBodySize && binary.LittleEndian.Uint16(body[:2]) == msgID
	case 33:
		// Current SET_QUEST_TRIGGER is the u16 opcode echo, u16 quest id,
		// u8 trigger channel, and u8 increment flag. The selective DPROTO
		// clone form carries a four-byte trailer and therefore intentionally
		// falls through to the legacy decoder for exact normalization.
		return bodyLen == currentSetQuestTriggerPlainBodySize && binary.LittleEndian.Uint16(body[:2]) == msgID
	case 39:
		if bodyLen == 62 {
			return true
		}
		if bodyLen < 66 {
			return false
		}
		count := int(body[22])
		return count <= 254 && bodyLen == 66+count*10
	case 40:
		// Current EXE normal-dungeon writers emit exactly two u16 actor
		// coordinates. They are not an actor id and are not server authority.
		return bodyLen == currentDieCharacterPlainBodySize
	case 41:
		// Current EXE normal revive requests emit one u16 target object key.
		return bodyLen == currentUseCoinPlainBodySize
	case 42:
		return bodyLen == 0
	case 45:
		return bodyLen == currentDungeonMoveMapPlainBodySize
	case 36:
		return bodyLen == currentTownSetUserAreaPlainBodySize
	case 46:
		return isCurrentPlayResultPlainBodySize(bodyLen)
	case 117:
		return bodyLen == currentBossDieCheckPlainBodySize
	case 123:
		return bodyLen == 24
	case 132:
		return bodyLen == 0
	case 143:
		return bodyLen == currentTutorialFlagPlainBodySize
	case 295:
		// Current EXE character-slot exchange writes exactly two u32 slot
		// indexes. The 13-byte upper header overlaps the legacy game header,
		// so this exact boundary must be recognized before legacy parsing.
		return bodyLen == 8
	case 560:
		// Current EXE sub_1DB82D0/sub_1DB88B0 starts class-1/op560 and
		// immediately flushes it without writing a request body. Recognize the
		// exact boundary so the 13-byte raw upper packet is not mistaken for a
		// same-shaped legacy game packet.
		return bodyLen == 0
	// 2026-06-30 hook/packet_log 对照证明这些号是 msg1 后的 latest raw upper 上行。
	// 其中 3/8/593 等号会和旧 cmd/type 重叠，必须用 body 长度避免旧 legacy 包误切。
	case 3:
		return bodyLen == 16
	case 5:
		if actualBodyLength, ok := hookedPlainCreateBodyLength(body); ok && actualBodyLength == bodyLen {
			return true
		}
		return bodyLen == 24 || bodyLen == 32
	case 8:
		return bodyLen == 8
	case 171:
		return bodyLen == 48
	case 194:
		return bodyLen == 368
	case 415:
		return bodyLen == 0
	case 250:
		return bodyLen == 64
	case 279:
		return bodyLen == 16
	case 441:
		return bodyLen == 8
	case 476:
		return bodyLen == 16
	case 477:
		return bodyLen == 8
	case 593:
		return bodyLen == 8 || bodyLen == 24
	case 645:
		return bodyLen == 0
	case 1262:
		return bodyLen == 32
	case 1276:
		return bodyLen == 0
	case 1456:
		// 90CN 的私有控制通道复用 CmdPacket 枚举里刻意留空的 1456。
		// 这里只接管 "KRM1"（测试用秒杀当前房间）：13 字节 channel 包头 +
		// 明文 body，但 opcode 不在原生白名单里，不拦截的话
		// splitLegacyGamePacket 会按 legacy cmd=1 把它切走，包永远到不了
		// handleGameUpper（实测：客户端已发包，服务端零日志）。
		//
		// "ACK1"（能力位图）和 "BMU1"（Boss 变异）刻意维持原有行为不动：
		// 它们一直走 legacy 分支，突然接通会改变线上未实现命令的失败回包。
		return isCurrentKillRoomUpperBody(body)
	case 1286:
		return bodyLen == 16
	case 1287:
		return bodyLen == 8
	case 1320:
		return bodyLen == 32
	case 1516:
		return bodyLen > 0
	case 1518:
		return bodyLen > 0
	default:
		return false
	}
}

// isCurrentKillRoomUpperBody 只放行 LYPlugins 秒杀 DLL 的 8 字节信封：
// u32 "KRM1" | u8 version | u8 reserved | u16 token。
// 严格校验 magic 与长度，避免把同形状的 legacy 帧误切成 upper。
func isCurrentKillRoomUpperBody(body []byte) bool {
	return len(body) == 8 && string(body[:4]) == "KRM1"
}

func isCurrent90CNEndpointBody(body []byte) bool {
	bodyLen := len(body)
	shortVariant := bodyLen >= observed90CNShortEndpointBodySize &&
		bodyLen <= observed90CNShortEndpointMaxBodySize &&
		(bodyLen-observed90CNShortEndpointBodySize)%current90CNEndpointShortVariantSize == 0
	extendedVariant := bodyLen >= reference90CNChannelDisplayRebindBodySize+current90CNEndpointAdapterRecordSize &&
		bodyLen <= reference90CNChannelDisplayRebindBodySize+current90CNEndpointAdapterRecordSize*current90CNEndpointMaxAdapterRecords &&
		(bodyLen-reference90CNChannelDisplayRebindBodySize)%current90CNEndpointAdapterRecordSize == 0
	if !shortVariant && !extendedVariant {
		return false
	}

	// The current 90CN endpoint request has a fixed client-spec envelope,
	// a lowercase 32-byte hexadecimal token, and a NUL terminator. These
	// invariants distinguish the admitted length family from the overlapping
	// legacy cmd=1/type=1 frame.
	if binary.LittleEndian.Uint32(body[0:4]) != 1 ||
		body[4] != 0x30 || body[5] != 0x20 ||
		body[6] != 0 || body[7] != 0 || body[8] != 0 ||
		body[41] != 0 || binary.LittleEndian.Uint32(body[42:46]) != 8 ||
		body[54] != 0 || body[55] != 2 || body[56] != 0 || body[57] != 0 ||
		body[58] != 1 || body[59] != 1 || body[60] != 0 ||
		!isObserved90CNClientSpecTail(body[61]) {
		return false
	}
	for _, value := range body[9:41] {
		if !isLowerHexASCII(value) {
			return false
		}
	}
	return true
}

func isObserved90CNClientSpecTail(value byte) bool {
	// 2026-10-14：补 0x22。线上客户端（90CN 客户端自身的 DNF.exe）在 class1/op1
	// 端点请求里 body[61] 发的是 0x22，旧白名单只有 0x16/0x1c/0x2e，于是
	// isKnownClientUpperMsg 把它当"未知报文"拒掉 —— 表现就是登录器选完频道后
	// 一直停在"正在连接服务器"，进不了游戏。
	//
	// 线上当时是先对 dnf90server 二进制做了 2 字节补丁（把最后一处 jne 改成 nop，
	// 等于这里全部放行）临时救回来的；源码这一处才是正规修法，改完重编部署即可，
	// 不必再打二进制补丁。若以后又观察到新的取值，同样往这里加。
	return value == 0x16 || value == 0x1c || value == 0x22 || value == 0x2e
}

func isLowerHexASCII(value byte) bool {
	return value >= '0' && value <= '9' || value >= 'a' && value <= 'f'
}

func isCurrentPlayResultPlainBodySize(bodyLen int) bool {
	for rows := 0; rows <= currentPlayResultMaximumDynamicRows; rows++ {
		if bodyLen == currentPlayResultBaseBodySize+rows*currentPlayResultDynamicRowSize ||
			bodyLen == currentPlayResultOptionalBodySize+rows*currentPlayResultDynamicRowSize {
			return true
		}
	}
	return false
}
