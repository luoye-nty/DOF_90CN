package dnfbridge

import (
	"encoding/binary"
	"errors"
	"fmt"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

const (
	currentDisjointItemRequestSize       = 5
	currentDisjointBatchHeaderSize       = 14
	currentDisjointBatchMaxItems         = 127
	currentAvatarDisjointRequestMinSize  = 2
	currentAvatarDisjointRequestFullSize = 6
	currentAvatarInventoryListType       = 1
	currentEmblemCompoundMinInputs       = 2
	currentEmblemCompoundMaxInputs       = 5
	currentEmblemCompoundInputSize       = 6
	currentDisjointMaxRewards            = 32
)

var currentDisjointBatchMarker = [4]byte{'B', '2', '6', 1}

var (
	errCurrentDisjointRequestInvalid = errors.New("dnf current disjoint request is invalid")
	errCurrentDisjointUnavailable    = errors.New("dnf current disjoint source or PVF mapping is unavailable")
	errCurrentDisjointSourceInvalid  = errors.New("dnf current disjoint source item is invalid")
	// errCurrentDisjointSourceLocked 是"源物品处于锁定状态"这一条独立的拒绝原因：
	// 它必须能和 errCurrentDisjointSourceInvalid（槽位空/数量不对/PVF 对不上）区分开，
	// 否则客户端只能收到一个笼统的失败码。装备锁的定义与出售/丢弃/交易/邮寄共用
	// equipment_lock_state 口径（见 currentNPCShopItemLocked）。
	errCurrentDisjointSourceLocked  = errors.New("dnf current disjoint source item is equipment locked")
	errCurrentDisjointRewardInvalid = errors.New("dnf current disjoint reward mapping is invalid")
)

// currentDisjointFailureCode 把一次被拒绝的分解/合成映射成客户端那侧的失败码。
//
// 原本所有失败一律回 4；对 op26 来说 4 是"物品栏空间不足，无法获得分解物品。"，
// 锁定装备被拒时玩家会看到一句和原因完全无关的背包提示。锁定单独拿一个码。
//
// currentDisjointLockedFailureCode = 214 是**反查确认**过的：
//
//	客户端通用映射器 0x032DDC70 里 `cmp eax, 0xd6 / jne` → `push 0; push 0xa4f8`
//	→ GetText(0x0367BE00) → show(0x02FDD1A0)，即 214 → dstr 42232
//	「在物品已锁定状态下，您无法进行当前操作。」
//	而 op26 的失败分支 0x01D293F1（jt 0x1D29560 / bt 0x1D29588，idx = code-4，
//	bound 0xAF → 码 4..175）对范围外的码落到 0x01D294F5 → `call 0x32DDC70`，
//	所以 214 在 op26 上会渲染 42232。op18/op47/op256 走同一条 fallback；
//	op202/op80/op271/op370 有显式的 214 分支。
const (
	currentDisjointFailureCode       = byte(4)
	currentDisjointLockedFailureCode = byte(214)
)

func currentDisjointFailureResult(err error) byte {
	if errors.Is(err, errCurrentDisjointSourceLocked) {
		return currentDisjointLockedFailureCode
	}
	return currentDisjointFailureCode
}

// currentDisjointItemRequest is the exact semantic body written by the
// current client's op26 sender: target slot, item-space, portable-disjoint
// slot, and (on some UI paths) a u32 context.  The reply reader consumes the
// first three fields again before it reads result rows.
type currentDisjointItemRequest struct {
	SourceSlot int16
	ListType   byte
	ToolSlot   int16
	Context    uint32
}

// currentDisjointBatchRequest is emitted only by the pinned current-client
// compatibility hook after its native "disjoint all" loop has selected and
// validated the exact equipment slots. This envelope carries the complete
// selected set as one command.
type currentDisjointBatchRequest struct {
	ListType byte
	ToolSlot int16
	Context  uint32
	Slots    []int16
}

type currentAvatarDisjointRequest struct {
	SourceSlot     int16
	ExpectedItemID uint32
}

type currentEmblemCompoundInput struct {
	ItemID uint32
	Slot   int16
}

type currentEmblemCompoundRequest struct {
	Inputs []currentEmblemCompoundInput
}

type currentDisjointReward struct {
	ItemID uint32
	Count  uint32
}

type currentDisjointRewardSlot struct {
	Slot   int16
	ItemID uint32
	// Count 是**写入后的整堆终值**（老存量 + 本次），它只用于随后的 0x14 增量行，
	// 由发那行的仓库刷新重新读出来，不直接进任何弹窗体。
	Count uint32
	// Granted 是**本次真正新增的量**，也是 op26/op202/op235/op256 弹窗体里
	// 那个数量字段的口径（当前客户端把它当"获得 N 个"直接渲染）。
	// 同族的四条路径都发 Granted：buildCurrentDisjointItemSuccessBody（op26）、
	// mergeCurrentDisjointBatchRewards（op26 批量）、expert_job_store.go（op235）、
	// buildCurrentEmblemCompoundSuccessBody（op256）。op202 早先发的是 Count，
	// 于是背包里已有同款徽章时弹窗会把老存量一起算进去（显示数量虚高、实际不多给）。
	Granted uint32
}

type currentDisjointResult struct {
	Rewards []currentDisjointRewardSlot
	Updates []currentItemListEntry
}

type currentDisjointAdditionalConst struct {
	GreatDivisor  float64
	NormalDivisor float64
	GreatChance   float64
	NormalChance  float64
}

type currentDisjointExpand struct {
	Enabled      bool
	ItemID       uint32
	LevelDivisor float64
	GreatChance  float64
	NormalChance float64
}

type currentDisjointPVFConfig struct {
	CubeItemID         uint32
	CubeBase           float64
	CubeMultipliers    []float64
	EquipmentSellRate  int64
	IndependentByItem  map[uint32][]currentDisjointReward
	Additional         [][]uint32
	AdditionalConsts   []currentDisjointAdditionalConst
	Expands            []currentDisjointExpand
	AvatarByJob        map[string][]uint32             // legacy disjoint.etc [avatar disjoint info], kept for config validation
	AvatarEmblemTables []currentAvatarEmblemGradeTable // etc/avatardisjoint/emblemlistinfo_<grade>.etc
	EmblemBoosters     map[[2]int]uint32               // (emblem grade,input count) -> booster item
}

// currentAvatarEmblemGradeTable is one grade's avatar disjoint reward table
// (86JP AvatarDisjointConfigProvider grammar): each [result info] section is
// one pool whose first value is the pick count, followed by
// item/weight/count/special 4-tuples.
type currentAvatarEmblemGradeTable struct {
	Pools []currentAvatarEmblemPool
}

type currentAvatarEmblemPool struct {
	PickCount int
	Rewards   []currentAvatarEmblemReward
}

type currentAvatarEmblemReward struct {
	ItemID  uint32
	Weight  uint32
	Count   uint32
	Special bool
}

func parseCurrentDisjointItemRequest(body []byte) (currentDisjointItemRequest, error) {
	if len(body) != currentDisjointItemRequestSize && len(body) != currentDisjointItemRequestSize+4 {
		return currentDisjointItemRequest{}, fmt.Errorf("%w: op26 body=%d", errCurrentDisjointRequestInvalid, len(body))
	}
	request := currentDisjointItemRequest{
		SourceSlot: int16(binary.LittleEndian.Uint16(body[0:2])),
		ListType:   body[2],
		ToolSlot:   int16(binary.LittleEndian.Uint16(body[3:5])),
	}
	if len(body) == currentDisjointItemRequestSize+4 {
		request.Context = binary.LittleEndian.Uint32(body[5:9])
	}
	if request.ListType != dnfrepo.MainInventoryListType || request.SourceSlot <= 0 ||
		dnfrepo.IsAccountSharedInventorySlot(request.ListType, request.SourceSlot) {
		return currentDisjointItemRequest{}, fmt.Errorf("%w: op26 list=%d slot=%d", errCurrentDisjointRequestInvalid, request.ListType, request.SourceSlot)
	}
	return request, nil
}

func parseCurrentDisjointBatchRequest(body []byte) (currentDisjointBatchRequest, bool, error) {
	if len(body) < currentDisjointBatchHeaderSize ||
		int16(binary.LittleEndian.Uint16(body[0:2])) != -1 ||
		body[9] != currentDisjointBatchMarker[0] ||
		body[10] != currentDisjointBatchMarker[1] ||
		body[11] != currentDisjointBatchMarker[2] ||
		body[12] != currentDisjointBatchMarker[3] {
		return currentDisjointBatchRequest{}, false, nil
	}
	count := int(body[13])
	if count < 1 || count > currentDisjointBatchMaxItems || len(body) != currentDisjointBatchHeaderSize+count*2 {
		return currentDisjointBatchRequest{}, true, fmt.Errorf("%w: op26 batch count=%d body=%d", errCurrentDisjointRequestInvalid, count, len(body))
	}
	request := currentDisjointBatchRequest{
		ListType: body[2],
		ToolSlot: int16(binary.LittleEndian.Uint16(body[3:5])),
		Context:  binary.LittleEndian.Uint32(body[5:9]),
		Slots:    make([]int16, 0, count),
	}
	if request.ListType != dnfrepo.MainInventoryListType {
		return currentDisjointBatchRequest{}, true, fmt.Errorf("%w: op26 batch list=%d", errCurrentDisjointRequestInvalid, request.ListType)
	}
	seen := make(map[int16]struct{}, count)
	for index := 0; index < count; index++ {
		offset := currentDisjointBatchHeaderSize + index*2
		slot := int16(binary.LittleEndian.Uint16(body[offset : offset+2]))
		if slot <= 0 || dnfrepo.IsAccountSharedInventorySlot(request.ListType, slot) {
			return currentDisjointBatchRequest{}, true, fmt.Errorf("%w: op26 batch index=%d slot=%d", errCurrentDisjointRequestInvalid, index, slot)
		}
		if _, duplicate := seen[slot]; duplicate {
			return currentDisjointBatchRequest{}, true, fmt.Errorf("%w: op26 batch duplicate slot=%d", errCurrentDisjointRequestInvalid, slot)
		}
		seen[slot] = struct{}{}
		request.Slots = append(request.Slots, slot)
	}
	return request, true, nil
}

func parseCurrentAvatarDisjointRequest(body []byte) (currentAvatarDisjointRequest, error) {
	if len(body) != currentAvatarDisjointRequestMinSize && len(body) != currentAvatarDisjointRequestFullSize {
		return currentAvatarDisjointRequest{}, fmt.Errorf("%w: op202 body=%d", errCurrentDisjointRequestInvalid, len(body))
	}
	request := currentAvatarDisjointRequest{SourceSlot: int16(binary.LittleEndian.Uint16(body[0:2]))}
	if len(body) == currentAvatarDisjointRequestFullSize {
		request.ExpectedItemID = binary.LittleEndian.Uint32(body[2:6])
	}
	if request.SourceSlot < 0 {
		return currentAvatarDisjointRequest{}, fmt.Errorf("%w: op202 slot=%d", errCurrentDisjointRequestInvalid, request.SourceSlot)
	}
	return request, nil
}

func parseCurrentEmblemCompoundRequest(body []byte) (currentEmblemCompoundRequest, error) {
	if len(body) < 1 {
		return currentEmblemCompoundRequest{}, errCurrentDisjointRequestInvalid
	}
	count := int(body[0])
	if count < currentEmblemCompoundMinInputs || count > currentEmblemCompoundMaxInputs || len(body) != 1+count*currentEmblemCompoundInputSize {
		return currentEmblemCompoundRequest{}, fmt.Errorf("%w: op256 count=%d body=%d", errCurrentDisjointRequestInvalid, count, len(body))
	}
	request := currentEmblemCompoundRequest{Inputs: make([]currentEmblemCompoundInput, 0, count)}
	for index := 0; index < count; index++ {
		offset := 1 + index*currentEmblemCompoundInputSize
		input := currentEmblemCompoundInput{ItemID: binary.LittleEndian.Uint32(body[offset : offset+4]), Slot: int16(binary.LittleEndian.Uint16(body[offset+4 : offset+6]))}
		if input.ItemID == 0 || input.Slot <= 0 || dnfrepo.IsAccountSharedInventorySlot(dnfrepo.MainInventoryListType, input.Slot) {
			return currentEmblemCompoundRequest{}, fmt.Errorf("%w: op256 input=%d item=%d slot=%d", errCurrentDisjointRequestInvalid, index, input.ItemID, input.Slot)
		}
		request.Inputs = append(request.Inputs, input)
	}
	return request, nil
}

func buildCurrentDisjointItemSuccessBody(request currentDisjointItemRequest, rewards []currentDisjointRewardSlot) []byte {
	var body packetWriter
	body.writeByte(1)
	body.writeUint16(uint16(request.SourceSlot))
	body.writeByte(request.ListType)
	body.writeUint16(uint16(request.ToolSlot))
	body.writeByte(byte(len(rewards)))
	for _, reward := range rewards {
		body.writeUint16(uint16(reward.Slot))
		body.writeUint32(reward.ItemID)
		body.writeUint32(reward.Granted)
	}
	return body.bytes()
}

// buildCurrentAvatarDisjointSuccessBody 编 op202 的"重铸成功"弹窗体。
//
// 每条奖励行最后那个 u32 是**本次新增量**（Granted），不是合并后的整堆终值：
// 当前客户端把它当"获得 N 个"渲染，而整堆终值由紧随其后的 0x14 增量行负责。
// 这里曾经发的是 reward.Count，于是"背包里已有同款徽章"时弹窗数量会等于
// 老存量 + 本次产出（玩家看到数字虚高、背包却不多给），与同族的 op26/op235/op256
// 口径也不一致。回归见 TestCurrentAvatarDisjointPopupReportsGrantedNotFinalStack。
func buildCurrentAvatarDisjointSuccessBody(request currentAvatarDisjointRequest, rewards []currentDisjointRewardSlot) []byte {
	var body packetWriter
	body.writeByte(1)
	body.writeUint16(uint16(request.SourceSlot))
	body.writeUint16(uint16(len(rewards)))
	for _, reward := range rewards {
		body.writeUint16(uint16(reward.Slot))
		body.writeUint32(reward.ItemID)
		body.writeUint32(reward.Granted)
	}
	return body.bytes()
}

func buildCurrentEmblemCompoundSuccessBody(reward currentDisjointRewardSlot) []byte {
	var body packetWriter
	body.writeByte(1)
	body.writeByte(1)
	body.writeUint32(reward.ItemID)
	body.writeUint32(reward.Granted)
	return body.bytes()
}
