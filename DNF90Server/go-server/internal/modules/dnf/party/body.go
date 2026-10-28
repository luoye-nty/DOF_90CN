// 本文件按当前 EXE/C# 证据构造队伍 S2C body。
package party

import "longheng.io/server/internal/modules/dnf/alignedcmd"

const (
	fullPartyMemberPercent = 100
	defaultTownUserState   = 1
	partyPeerEndpointSize  = 22
	directoryMemberSlots   = 6
	directoryOrdinaryState = 0xff
	directoryEmptyState    = 0xff
	directoryEmptyUserID   = 0xffff

	// DirectoryTypeOrdinary selects the first current-client party directory.
	DirectoryTypeOrdinary byte = 2
)

// PeerEndpoint is one current-client PARTY_IP_INFO member row. IPv4 is kept
// as four network-order octets while Port is written in network byte order;
// the remaining numeric fields use the packet's ordinary little-endian order.
type PeerEndpoint struct {
	UserID     uint16
	InnerIPv4  [4]byte
	OuterIPv4  [4]byte
	Port       uint16
	RoutingKey uint32
	NATType    byte
	PathMTU    uint32
	UserState  byte
}

// DirectoryRecord is the server-owned public party-directory projection used
// by the current client class0/op87 reader.
type DirectoryRecord struct {
	PartyID     uint16
	SelectionID uint32
	MemberIDs   []uint16
}

// BuildDirectorySnapshot constructs the current NoPack class0/op87 body:
//
//	u16 count
//	repeat count {
//	  u16 partyID
//	  u8 reservedA
//	  u8 directoryType
//	  u8 reservedB
//	  u8 specialState
//	  u32 selectionID
//	  repeat 6 { u8 memberState; u16 characterID }
//	}
//
// Current EXE sub_1D3F040 subtracts its protected logical base value 2 from
// directoryType before indexing a five-element directory container. Therefore
// this byte is a directory discriminator, not the resident channel ID. This
// builder currently publishes only ordinary-dungeon parties and deliberately
// fixes directoryType to 2 (container index zero). sub_1D3F040 reads but does
// not retain reservedA/reservedB. Ordinary parties must use specialState FF:
// any other value enters the optional complete-display subsystem. Occupied
// slots use neutral state zero; unused slots use the exact FF/FFFF sentinels.
func BuildDirectorySnapshot(records []DirectoryRecord) []byte {
	if len(records) > int(^uint16(0)) {
		records = records[:int(^uint16(0))]
	}
	var writer packetWriter
	writer.writeUint16(uint16(len(records)))
	for _, record := range records {
		writer.writeUint16(record.PartyID)
		writer.writeByte(0)
		writer.writeByte(DirectoryTypeOrdinary)
		writer.writeByte(0)
		writer.writeByte(directoryOrdinaryState)
		writer.writeUint32(record.SelectionID)
		for slot := 0; slot < directoryMemberSlots; slot++ {
			if slot < len(record.MemberIDs) &&
				record.MemberIDs[slot] != 0 &&
				record.MemberIDs[slot] != directoryEmptyUserID {
				writer.writeByte(0)
				writer.writeUint16(record.MemberIDs[slot])
				continue
			}
			writer.writeByte(directoryEmptyState)
			writer.writeUint16(directoryEmptyUserID)
		}
	}
	return writer.bytes()
}

// BuildSingleMemberRealtimeInfo 构造 NOTI 0x0099 PARTY_MEMBER_REALTIME_INFO。
//
// 行布局与 86JP 的 PartyRealtimeInfoBuilder 相同，而该 builder 是逐字节照
// df_game_r CParty::get_party_realtime_info @0x0859CBAC 写的：
//
//	count:u8, 每有效成员 { uid:u16, hpPercent:u8, isHelpAbuseParty:u8, slotIndex:u8 }
//
// 即 +2 是该成员的 HP 百分比（0~100，满血 100；本包没有 MP 字段，客户端取到
// HP 后自行取 MP），+4 是成员在队伍表里的固定槽位。把 +2 写成 UserState、把
// +4 写死 0 会让一支多人队伍的每一行都声称自己占 0 号槽：客户端按槽位匹配时
// 血条会互相覆盖，而 HP 也永远显示满血。
//
// +4 的取值优先用 member.Slot（op9 下发的就是同一个槽位号）。只有当调用方
// 没有给出可用槽位（越界或与前面的行重复）时才按第一个空槽补齐，规则与
// dnfbridge 侧 appendRuntimePartyMember 的槽位分配完全一致，避免 op153 与
// op9 对同一支队伍给出互相矛盾的行。
func BuildSingleMemberRealtimeInfo(state alignedcmd.PartyState) []byte {
	if state.PartyID <= 0 || state.UserID == 0 {
		return []byte{0}
	}
	members := partyMembers(state)
	var writer packetWriter
	var used [4]bool
	writer.writeByte(byte(len(members)))
	for _, member := range members {
		slot := member.Slot
		if slot >= byte(len(used)) || used[slot] {
			slot = 0
			for candidate := byte(0); candidate < byte(len(used)); candidate++ {
				if !used[candidate] {
					slot = candidate
					break
				}
			}
		}
		used[slot] = true
		writer.writeUint16(member.UserID)
		writer.writeByte(memberPercentByte(member.HPPercent))
		writer.writeByte(0)
		writer.writeByte(slot)
	}
	return writer.bytes()
}

// BuildEmptyRealtimeInfo 构造退出队伍后用于清空实时队员 UI 的空列表。
func BuildEmptyRealtimeInfo() []byte {
	return []byte{0}
}

// BuildPeerEndpointInfo constructs current-client class0/op11
// PARTY_IP_INFO. The current reader consumes one count byte followed by 22
// bytes per member:
//
//	u16 uid, 4B inner IPv4, 4B outer IPv4, u16be port,
//	u32 routing key, u8 NAT type, u32 path MTU, u8 connection percentage.
//
// RoutingKey and PathMTU must be the remote connection's values previously
// published by its login/path-report flow; UserState is a connection percent.
func BuildPeerEndpointInfo(endpoints []PeerEndpoint) []byte {
	if len(endpoints) > 4 {
		endpoints = endpoints[:4]
	}
	var writer packetWriter
	writer.writeByte(byte(len(endpoints)))
	for _, endpoint := range endpoints {
		writer.writeUint16(endpoint.UserID)
		for _, octet := range endpoint.InnerIPv4 {
			writer.writeByte(octet)
		}
		for _, octet := range endpoint.OuterIPv4 {
			writer.writeByte(octet)
		}
		writer.writeByte(byte(endpoint.Port >> 8))
		writer.writeByte(byte(endpoint.Port))
		writer.writeUint32(endpoint.RoutingKey)
		writer.writeByte(endpoint.NATType)
		writer.writeUint32(endpoint.PathMTU)
		userState := endpoint.UserState
		if userState == 0 || userState == defaultTownUserState {
			userState = fullPartyMemberPercent
		}
		writer.writeByte(userState)
	}
	return writer.bytes()
}

// BuildReserveLeavePartyAck 构造 0x02B3 成功响应：success,u8 flag,u16 targetChar。
func BuildReserveLeavePartyAck(flag byte, targetChar uint16) []byte {
	var writer packetWriter
	writer.writeByte(1)
	writer.writeByte(flag)
	writer.writeUint16(targetChar)
	return writer.bytes()
}

// BuildEntryIntoPartyAck 构造 0x02C1 成功响应：success,u32 target,u32 currentChar。
func BuildEntryIntoPartyAck(targetID uint32, currentCharID uint32) []byte {
	var writer packetWriter
	writer.writeByte(1)
	writer.writeUint32(targetID)
	writer.writeUint32(currentCharID)
	return writer.bytes()
}

// BuildEntryIntoPartyFinishEmptyBody constructs class0/op706 when the bridge
// has no peer-value rows to publish. Current EXE sub_1D56F60 always consumes
// state:u8 and count:u8 before reading count pairs of u32 key/u32 value.
func BuildEntryIntoPartyFinishEmptyBody(state byte) []byte {
	return []byte{state, 0}
}

// BuildRequestPeerAck 构造 0x000A 的 class1 最小成功响应。
// 当前 EXE sub_2261E30 先消费 success:u8；success 非零时
// sub_1D11A80 立即返回，不再读取业务字段。
func BuildRequestPeerAck() []byte {
	return []byte{1}
}

// BuildRequestPeerSelectionNotice constructs the class0/op7 mode-15 reply
// that completes a town-player click. Current EXE sub_1D82CF0 reads
// target:u16, mode:u8, value0:u32 and party_marker:u16. The marker is not an
// opaque echo: 0xffff clears the selected actor's party-present flag, while
// every other value sets it, before sub_325FA70 opens the interaction menu.
// The trailing value2:u32 from the class1/op10 request is not consumed by
// this mode and therefore must not be appended to the notification.
func BuildRequestPeerSelectionNotice(targetID uint16, value0 uint32, targetHasParty bool) []byte {
	var writer packetWriter
	writer.writeUint16(targetID)
	writer.writeByte(15)
	writer.writeUint32(value0)
	if targetHasParty {
		writer.writeUint16(0)
	} else {
		writer.writeUint16(^uint16(0))
	}
	return writer.bytes()
}

// PeerNoticeStats carries the op7 prompt rows the requesting client cannot
// supply, because the mode-10 request tail and the mode-10 reply tail do not
// describe the same fields.
//
// Measured 2026-09-26 on the live server. The request tail (u32 value0,
// u16 value1, u32 value2, shared by modes 0/10/12/15) self-reports the
// requester as (复活币, 抗魔值, 0): char 22 sent value0=10 and value1=2828 while
// its stored 复活币 was 10 and its stored fatigue was 156, and char 1 sent
// value0=430 and value1=5837 while its stored 复活币 was 430 and its stored
// fatigue was 188. 复活币 matches exactly and fatigue cannot, so value1 is
// 抗魔值 -- the one row this server has no other source for, since nothing in
// this tree computes 抗魔.
//
// The reply the client actually reads was pinned by a marker probe (seven u16
// markers 0x1111..0x7777 at offsets 7/9/11/13/15/17/19 of a mode-10 op7): the
// dialog rendered 4369/8738/13107 under 疲劳值/虚弱状态/抗魔值 and ignored every
// marker past offset 11, so the tail is exactly three u16 at 7, 9, 11 in that
// order. Feeding the request's value1 into offset 7 is what put 抗魔值 on the
// 疲劳值 row.
type PeerNoticeStats struct {
	// Fatigue is the requester's remaining fatigue (character stat "fatigue").
	Fatigue uint16
	// Weakness is the requester's 虚弱状态 on the client's own 0..100 scale,
	// where 100 is healthy (see currentWeaknessCondition).
	Weakness uint16
	// Exorcism is the requester's 抗魔值, echoed from the request's value1.
	Exorcism uint16
}

// BuildRequestPeerNotice constructs current NoPack class0/op7 for an online
// peer request. sub_1D82CF0 consumes a mode-specific tail: ordinary party
// mode 0 has three u16 values and one u32 server value, trade mode 1 has two
// u32 values. Street-assault modes 4/13 use BuildAssaultInvite instead:
// the generic op7 dialog has no mode-4 content or accepting callback.
//
// Modes 0/12 share mode 10's three value rows (same prompt widget, same
// offsets); only their trailing u32 differs and stays the request's value2.
func BuildRequestPeerNotice(sourceID uint16, req RequestPeerRequest, stats PeerNoticeStats) []byte {
	var writer packetWriter
	writer.writeUint16(sourceID)
	writer.writeByte(req.Mode)
	writer.writeUint32(req.Value0)
	switch req.Mode {
	case 0, 12:
		// Same three rows as mode 10. The ordinary party invite renders the
		// same prompt widget, so feeding it the request's value1 put 抗魔值 on
		// its 疲劳值 row exactly as it did in the raid prompt (operator report
		// 2026-09-26: the party prompt showed the same wrong rows until this
		// arm was changed too). The trailing u32 stays the request's value2 --
		// it is a further field this arm has never been shown to compute.
		writer.writeUint16(stats.Fatigue)
		writer.writeUint16(stats.Weakness)
		writer.writeUint16(stats.Exorcism)
		writer.writeUint32(req.Value2)
	case 1:
		writer.writeUint32(req.Value2)
	case 10:
		writer.writeUint16(stats.Fatigue)
		writer.writeUint16(stats.Weakness)
		writer.writeUint16(stats.Exorcism)
	}
	return writer.bytes()
}

// BuildResponsePeerAckPayload is appended after the class1/op11 success byte.
// Current sub_1D12480 always consumes target:u16 and mode:u8; trade mode 1
// consumes one additional state byte before entering its local trade path.
func BuildResponsePeerAckPayload(targetID uint16, mode byte) []byte {
	var writer packetWriter
	writer.writeUint16(targetID)
	writer.writeByte(mode)
	if mode == 1 {
		writer.writeByte(0)
	}
	return writer.bytes()
}

// BuildResponsePeerNotice constructs class0/op8 for the original requester.
// Current sub_1D83250 consumes responder:u16, mode:u8 and value:u32.
func BuildResponsePeerNotice(responderID uint16, mode byte, value uint32) []byte {
	var writer packetWriter
	writer.writeUint16(responderID)
	writer.writeByte(mode)
	writer.writeUint32(value)
	return writer.bytes()
}

// BuildAssaultInvite constructs class0/op443 (invite) and op444 (waiting).
// Current sub_33C8E20/sub_33C8D00 read actor:u16 and mode:u8.
func BuildAssaultInvite(inviterID uint16, mode byte) []byte {
	var writer packetWriter
	writer.writeUint16(inviterID)
	writer.writeByte(mode)
	return writer.bytes()
}

// BuildWalkoutPartyMemberNotice 构造 0x000A 队伍踢人/返回城镇通知：u8 slot,u8 mode。
func BuildWalkoutPartyMemberNotice(slot byte, mode byte) []byte {
	var writer packetWriter
	writer.writeByte(slot)
	writer.writeByte(mode)
	return writer.bytes()
}

// BuildChangePartyMemberPositionAck constructs the exact current-EXE op335
// result body: success followed by the authoritative source and target slots.
func BuildChangePartyMemberPositionAck(fromSlot byte, toSlot byte) []byte {
	var writer packetWriter
	writer.writeByte(1)
	writer.writeByte(fromSlot)
	writer.writeByte(toSlot)
	return writer.bytes()
}

func partyMembers(state alignedcmd.PartyState) []alignedcmd.PartyMemberState {
	members := make([]alignedcmd.PartyMemberState, 0, 4)
	for _, member := range state.Members {
		if member.UserID == 0 {
			continue
		}
		members = append(members, member)
		if len(members) == 4 {
			break
		}
	}
	if len(members) == 0 && state.UserID != 0 {
		members = append(members, alignedcmd.PartyMemberState{
			UserID:    state.UserID,
			UserState: state.UserState,
			HPPercent: fullPartyMemberPercent,
			MPPercent: fullPartyMemberPercent,
		})
	}
	return members
}

func memberUserState(member alignedcmd.PartyMemberState, fallback byte) byte {
	if member.UserState != 0 {
		return member.UserState
	}
	if fallback != 0 {
		return fallback
	}
	return defaultTownUserState
}

func memberPercentByte(value byte) byte {
	if value == 0 {
		return fullPartyMemberPercent
	}
	if value > fullPartyMemberPercent {
		return fullPartyMemberPercent
	}
	return value
}
