package dnfbridge

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// op1119（ENUM_CMDPACKET_BIND_PLUS，梦想/绝对绑定装扮合成器）的线格式。
//
// 请求体（实测 144 字节，两次现场取证逐字段一致）：
//
//	+0x00 u32 800             客户端结构体直发（窗口/分辨率域），服务端不看
//	+0x04 u32 600             同上
//	+0x08 u32                 随机/哈希，逐次变化
//	+0x0C u32 23864           固定
//	+0x10 u32 输出装扮 ID      ★ 玩家点名的输出，= chn_absolute_bind_cube.etc 的
//	                          [absolute bind plus rare][职业][部位]（现场 101560739 = 剑士头发）
//	+0x14 u16 ability         结果装扮的 ability 号（现场 4 / 上一次 5）
//	+0x16 u16 材料件数         = 8
//	+0x18 8 × (u16 槽, u32 道具) ★ 8 件材料装扮（装扮栏槽位 + 道具 ID）
//	+0x48 u32 / +0x4C u32     随选择变化，未解
//	+0x50 64 字节             客户端结构体直发（内含栈指针），服务端必须忽略
//
// 回包（class-1 op1119，共 55 字节）的形状是逐指令读出来的，不是猜的：
// 客户端处理器 RVA 0x1135C10 先 `cmp byte ptr [ebp+0xC], 0 / je return`（传输层把
// body[0] 当成功标志 a1），然后 `ReadBytes(&buf, 0x36)` 读 54 字节，再调 0x1135680。
// 见 梦想装扮合成器op1119_回包格式定案_20261004.md 第二/三/四节。
const (
	currentAvatarBindPlusMaterialOffset   = 0x18
	currentAvatarBindPlusMaterialSize     = 6
	currentAvatarBindPlusRequestedOffset  = 0x10
	currentAvatarBindPlusAbilityOffset    = 0x14
	currentAvatarBindPlusCountOffset      = 0x16
	currentAvatarBindPlusMaterialCount    = 8
	currentAvatarBindPlusResultSize       = 0x36
	currentAvatarBindPlusReplySize        = 1 + currentAvatarBindPlusResultSize
	currentAvatarBindPlusResultItemOffset = 0x06
	currentAvatarBindPlusResultSlotOffset = 0x0A
	currentAvatarBindPlusResultCountOff   = 0x0C
	currentAvatarBindPlusResultSlotsOff   = 0x0E
	currentAvatarBindPlusErrorCode        = 0x16
)

var (
	errCurrentAvatarBindPlusRequestInvalid = errors.New("dnf current avatar bind plus request is invalid")
	errCurrentAvatarBindPlusUnavailable    = errors.New("dnf current avatar bind plus source or PVF mapping is unavailable")
	errCurrentAvatarBindPlusOutputInvalid  = errors.New("dnf current avatar bind plus requested output is invalid")
	errCurrentAvatarBindPlusMaterial       = errors.New("dnf current avatar bind plus material is invalid")
	errCurrentAvatarBindPlusCube           = errors.New("dnf current avatar bind plus synthesizer is invalid")
)

type currentAvatarBindPlusMaterial struct {
	Slot   int16
	ItemID uint32
}

type currentAvatarBindPlusRequest struct {
	RequestedItemID uint32
	AbilityNo       uint16
	Count           uint16
	Materials       []currentAvatarBindPlusMaterial
	BodyLength      int
}

func parseCurrentAvatarBindPlusRequest(body []byte) (currentAvatarBindPlusRequest, error) {
	if len(body) < currentAvatarBindPlusMaterialOffset {
		return currentAvatarBindPlusRequest{}, fmt.Errorf("%w: body=%d", errCurrentAvatarBindPlusRequestInvalid, len(body))
	}
	request := currentAvatarBindPlusRequest{
		RequestedItemID: binary.LittleEndian.Uint32(body[currentAvatarBindPlusRequestedOffset : currentAvatarBindPlusRequestedOffset+4]),
		AbilityNo:       binary.LittleEndian.Uint16(body[currentAvatarBindPlusAbilityOffset : currentAvatarBindPlusAbilityOffset+2]),
		Count:           binary.LittleEndian.Uint16(body[currentAvatarBindPlusCountOffset : currentAvatarBindPlusCountOffset+2]),
		BodyLength:      len(body),
	}
	if request.RequestedItemID == 0 || request.Count == 0 || request.Count > currentAvatarBindPlusMaterialCount {
		return currentAvatarBindPlusRequest{}, fmt.Errorf("%w: item=%d count=%d body=%d",
			errCurrentAvatarBindPlusRequestInvalid, request.RequestedItemID, request.Count, len(body))
	}
	need := currentAvatarBindPlusMaterialOffset + int(request.Count)*currentAvatarBindPlusMaterialSize
	if len(body) < need {
		return currentAvatarBindPlusRequest{}, fmt.Errorf("%w: count=%d need=%d body=%d",
			errCurrentAvatarBindPlusRequestInvalid, request.Count, need, len(body))
	}
	request.Materials = make([]currentAvatarBindPlusMaterial, 0, request.Count)
	seen := make(map[int16]struct{}, request.Count)
	for index := 0; index < int(request.Count); index++ {
		offset := currentAvatarBindPlusMaterialOffset + index*currentAvatarBindPlusMaterialSize
		material := currentAvatarBindPlusMaterial{
			Slot:   int16(binary.LittleEndian.Uint16(body[offset : offset+2])),
			ItemID: binary.LittleEndian.Uint32(body[offset+2 : offset+6]),
		}
		if material.ItemID == 0 || material.Slot < 0 {
			return currentAvatarBindPlusRequest{}, fmt.Errorf("%w: index=%d slot=%d item=%d",
				errCurrentAvatarBindPlusRequestInvalid, index, material.Slot, material.ItemID)
		}
		if _, duplicate := seen[material.Slot]; duplicate {
			return currentAvatarBindPlusRequest{}, fmt.Errorf("%w: duplicate slot=%d", errCurrentAvatarBindPlusRequestInvalid, material.Slot)
		}
		seen[material.Slot] = struct{}{}
		request.Materials = append(request.Materials, material)
	}
	return request, nil
}

type currentAvatarBindPlusOutcome struct {
	OutputItemID uint32
	OutputSlot   int16
	// AbilityNo 是玩家在窗口里选的属性（请求 +0x14）。它同时进两处：服务端那一行
	// 的 avatar_ability_no，以及回包 +0x0A —— 后者是客户端结果描述符的构造参数，
	// 现场 2026-10-04 正是因为这里塞的是槽号，客户端把"属性"显示成了那个数字。
	AbilityNo uint16
	// ConsumedSlots 按客户端 8 个材料格的顺序回显，长度固定 8；不足处用 0 占位
	// （客户端只为它自己填过东西的格子取用这个值，空格的 0 不会被读）。
	ConsumedSlots []int16
	SetIndex      int64
	Part          string
	CubeItemID    uint32
	CubeListType  byte
	CubeSlot      int16
	CubeRemain    int64
}

// buildCurrentAvatarBindPlusSuccessBody 编 op1119 的成功回包（55 字节）。
//
//	body[0]     u8 1              成功标志（处理器 0x1135C10 的 cmp [ebp+0xC],0）
//	body[1]     ── 54 字节结果结构 ──
//	 +0x00 u32 0                  客户端不读
//	 +0x04 u16 新道具落在哪个槽     ★ 0x1F2B470(container, X, itemObj, 1, 1) 里
//	                              X 是**越界检查过的容器下标**（0x1F2B55D
//	                              `mov [eax + ecx*4], esi`），即"把结果对象写进
//	                              container[槽]"。
//	 +0x06 u32 输出装扮道具 ID      → 0x2359A70(道具ID, 结果描述符, 0, 0)
//	 +0x0A u16 玩家选的属性         → 结果描述符构造器 0x1D76DB0 第 4 参。
//	                              现场教训：这一格塞槽号会让客户端把属性显示成
//	                              "35"（服务端数据是对的，所以整理背包后恢复正常）。
//	 +0x0C u16 1                  每个材料格扣减数量 → 0x1F2B850(container, 槽, 扣减数)
//	 +0x0E i16×8 8 个材料槽         → 客户端逐格按槽扣
//	 +0x1E 24 字节 0              客户端不读
func buildCurrentAvatarBindPlusSuccessBody(outcome currentAvatarBindPlusOutcome) []byte {
	var body packetWriter
	body.writeByte(1)
	body.writeUint32(0)
	body.writeUint16(uint16(outcome.OutputSlot))
	body.writeUint32(outcome.OutputItemID)
	body.writeUint16(outcome.AbilityNo)
	body.writeUint16(1)
	for index := 0; index < currentAvatarBindPlusMaterialCount; index++ {
		slot := int16(0)
		if index < len(outcome.ConsumedSlots) {
			slot = outcome.ConsumedSlots[index]
		}
		body.writeUint16(uint16(slot))
	}
	body.writeZero(currentAvatarBindPlusResultSize - currentAvatarBindPlusResultSlotsOff - currentAvatarBindPlusMaterialCount*2)
	return body.bytes()
}

func currentAvatarBindPlusFailureBody() []byte {
	// 与 op99 同款：首字节 0 = 客户端处理器直接返回；第二字节是通用错误码。
	return []byte{0, currentAvatarBindPlusErrorCode, 0}
}
