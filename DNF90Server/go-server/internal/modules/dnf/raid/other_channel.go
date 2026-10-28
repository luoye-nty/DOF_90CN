// 本文件按 NoPack 公共频道 raid S2C handler 的读取顺序构造固定包体。
package raid

import "errors"

const (
	// RaidOtherChannelUserinfoMsgID 对应 sub_1D57390: u32 count + repeat u32 key。
	RaidOtherChannelUserinfoMsgID uint16 = 819
	// RaidOtherChannelRequestJoinResultMsgID 对应 sub_1D573E0: u32,u32。
	RaidOtherChannelRequestJoinResultMsgID uint16 = 820
	// RaidOtherChannelResponseJoinMsgID 对应 sub_CC2480: u8,u8。
	RaidOtherChannelResponseJoinMsgID uint16 = 821
	// RaidOtherChannelListPageMsgID 对应 sub_1D4C060: raw[20], rows12, rows13。
	RaidOtherChannelListPageMsgID uint16 = 0x374
	// RaidPublicListMsgID 对应 sub_1D7AA10。它才是“寻找攻坚队”窗口的
	// 攻坚队对象快照，不是 0x374 的跨频道通用页容器。
	RaidPublicListMsgID uint16 = 0x02f1
	// RaidPublicListPageSize 来自 sub_1B4B520 的 33 * page 请求偏移。
	RaidPublicListPageSize = 33
)

var errRaidPublicListPageTooLarge = errors.New("raid public list page exceeds 33 objects")

// PublicRaidListPage is the exact class-0/op753 snapshot consumed by
// sub_1D7AA10: total matching rows, returned row offset, row count, then full
// sub_17B8910 raid objects followed by their one-byte member counts.
type PublicRaidListPage struct {
	TotalCount uint32
	Offset     uint32
	Objects    []RaidObjectMode0
}

// BuildPublicRaidListBody builds the native “寻找攻坚队” object page.
func BuildPublicRaidListBody(page PublicRaidListPage) ([]byte, error) {
	if len(page.Objects) > RaidPublicListPageSize {
		return nil, errRaidPublicListPageTooLarge
	}
	var writer packetWriter
	writer.writeUint32(page.TotalCount)
	writer.writeUint32(page.Offset)
	writer.writeUint32(uint32(len(page.Objects)))
	for _, object := range page.Objects {
		if len(object.Members) > MaxAttackPartyMembers {
			return nil, errRaidMemberTooMany
		}
		if err := writeRaidObjectBase(&writer, object); err != nil {
			return nil, err
		}
		writer.writeByte(byte(len(object.Members)))
	}
	return writer.bytes(), nil
}

// OtherChannelListPage 是 S2C 0x374 的保守构造输入。
//
// MCP 20260705 复核：sub_1D4C060 开头读取 raw[20]，随后读取
// u32 count + repeat raw[12]，后半段再读取 u32 count + repeat raw[13]。
// 当前只把它作为 831 公共频道列表请求之后的候选列表页回包构造器，
// 不在 handler 中自动发送。
//
// Rows12 会被完整拷贝进步长 12 的容器。
// Rows13 是 0x183/0x34C/0x8F 同族 72 字节列表项的压缩格式：
// +0 u32, +4 u16, +6 u16, +8 u8, +9 u8, +10 u8, +11 u8；
// +12 暂未见直接使用。和扩展格式相比，0x374 不携带写入 item+0x24 的额外 u16，
// 因此客户端把 item+0x24 固定为 0。
type OtherChannelListPage struct {
	Header [20]byte
	Rows12 [][12]byte
	Rows13 [][13]byte
}

// BuildOtherChannelListPageBody 构造 S2C 0x374 公共频道/攻坚列表页候选包体。
func BuildOtherChannelListPageBody(page OtherChannelListPage) []byte {
	var writer packetWriter
	writer.writeRaw(page.Header[:])
	writeCheckUserRows12(&writer, page.Rows12)
	writer.writeUint32(uint32(len(page.Rows13)))
	for _, row := range page.Rows13 {
		writer.writeRaw(row[:])
	}
	return writer.bytes()
}

// BuildOtherChannelUserinfoBody 构造 S2C 819 公共频道用户 key 列表。
func BuildOtherChannelUserinfoBody(keys []uint32) []byte {
	var writer packetWriter
	writer.writeUint32(uint32(len(keys)))
	for _, key := range keys {
		writer.writeUint32(key)
	}
	return writer.bytes()
}

// BuildOtherChannelRequestJoinResultBody 构造 S2C 820 的两个 u32 字段。
//
// MCP 20260705 复核：sub_1D573E0 读两个 u32 后调用
// sub_1C5B070(type=3, valueA, valueB)。valueA 必须匹配客户端对象 +552，
// valueB 是累加量，达到对象 +556 阈值后才置完成标志。它不是通用成功 ACK。
func BuildOtherChannelRequestJoinResultBody(valueA uint32, valueB uint32) []byte {
	var writer packetWriter
	writer.writeUint32(valueA)
	writer.writeUint32(valueB)
	return writer.bytes()
}

// BuildOtherChannelResponseJoinBody 构造 S2C 821 响应加入结果。
//
// MCP 20260705 复核：sub_CC2480 把 flag 写到 UI 对象 292 的 +4(bool)，
// 把 resultOrSlot 写到 +8，然后触发 UI 610 刷新；实际语义仍由 owner 决定。
func BuildOtherChannelResponseJoinBody(resultOrSlot byte, flag byte) []byte {
	var writer packetWriter
	writer.writeByte(resultOrSlot)
	writer.writeByte(flag)
	return writer.bytes()
}
