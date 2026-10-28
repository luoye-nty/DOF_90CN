package dnfbridge

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	dnfcerashop "longheng.io/server/internal/modules/dnf/cerashop"
)

const (
	currentBoosterSelectionBodySize = 10
	currentBoosterRandomBodySize    = 2
	currentBoosterMultiBodyBaseSize = 6
	currentBoosterMultiEntrySize    = 9
	// currentBoosterCompactBodyBaseSize / currentBoosterCompactEntrySize 描述当前客户端对
	// "选择型礼盒、候选行是 etc/stackable（没有逐项 option）"时用的紧凑写法：
	//   u16 槽位 + u16 类别 + N×u32 选中道具 + u8 选项 + u8 尾字节 = 4 + 4N + 2
	//
	// 现场取证（2026-09-21 20:39，角色 1111，runtime/logs/packet_log.txt）：2018夏日礼包
	// 重购惊喜礼盒 4（490702367，PVF `[booster selection num] 2`、`[booster select
	// category] 0 0`）被点了两次，客户端两次发出**完全相同的 14 字节**：
	//   72 00 | 00 00 | f7 85 3f 1d | 64 86 3f 1d | 00 00
	//   = 槽位 114、类别 0、道具 490702327 与 490702436、选项/尾字节 0
	// 这两个道具号正是该礼盒 PVF 类别 0 里的 `[etc] 490702327 1` 与
	// `[stackable] 490702436 1 490702437 1 490702438 1 490702439 1`，件数恰好等于
	// `[booster selection num] 2`。旧解析器只认 2 / 10 / 6+9n，长度一票否决，于是客户端
	// 把失败码 4 渲染成「物品栏空间不足」——与背包无关（同 2026-09-15 公会礼盒那次的坑，
	// 见 changlog.md）。
	currentBoosterCompactBodyBaseSize = 6
	currentBoosterCompactEntrySize    = 4
	currentBoosterMaxRewardRows       = 128
	currentBoosterMaxRewardUnits      = 1_000_000
	currentBoosterPetArtifactStart    = int16(140)
	currentBoosterPetArtifactEnd      = int16(188)
)

var (
	errCurrentBoosterRequestMalformed = errors.New("dnf booster request is malformed")
	errCurrentBoosterOwnerUnavailable = errors.New("dnf booster selected character is unavailable")
	errCurrentBoosterSourceMissing    = errors.New("dnf booster source item is unavailable")
	errCurrentBoosterPVFInvalid       = errors.New("dnf booster runtime PVF data is invalid")
	errCurrentBoosterSelectionInvalid = errors.New("dnf booster selection is invalid")
	errCurrentBoosterExpired          = errors.New("dnf booster source item has expired")
	// errCurrentBoosterCooltimeActive：PVF `[cooltime maintenance]` 的道具仍在冷却窗口内。
	// 客户端的冷却门只是进程内内存，小退重上就没了，所以这条判定必须由服务端兜住
	// （现场 2026-09-17：1 天冷却的礼盒重上后又能用）。
	errCurrentBoosterCooltimeActive = errors.New("dnf booster source item is still cooling down")
	// errCurrentBoosterDailyUseLimitReached：PVF `[total usable count]`（客户端悬浮
	// 说明里的「每天可用次数」）今天已经用满。上限属于道具本身、与它走哪条消费路径
	// 无关，礼盒也是消耗品，所以开箱这条路径用与 op44/op515 完全相同的那份计数
	// （internal/modules/dnf/itemdailylimit），并且判定发生在**扣道具的同一个事务**里：
	// 到上限时整笔回滚，源礼盒不会被消耗。
	errCurrentBoosterDailyUseLimitReached = errors.New("dnf booster source item reached its daily usable count")
)

type currentBoosterRequestKind byte

const (
	currentBoosterRequestRandom currentBoosterRequestKind = iota
	currentBoosterRequestSelection
	// currentBoosterRequestLottery is the native op27 jar route. It deliberately
	// stays distinct from op160's immediate random-box route because the current
	// client consumes a complete display item row before playing the jar
	// animation; any later historical op217 callback is idempotent.
	currentBoosterRequestLottery
)

// currentBoosterSelectionRequest is one PVF-backed item/attribute pair written
// by the current NoPack op160 selection UI.
type currentBoosterSelectionRequest struct {
	ItemID uint32
	Option byte
}

// currentBoosterOpenRequest follows the current NoPack op160 writers seen on
// the live connection. Plain [booster] sends only u16 sourceSlot. A one-choice
// [booster selection] sends u16 slot, u16 category context, u32 selected item,
// u8 option and one trailing UI byte. A selection_num=0 category sends every
// category item twice: first N*u32 item IDs, then u8 N and N*{u32 item,u8
// option}, followed by one trailing UI byte.
type currentBoosterOpenRequest struct {
	Kind                currentBoosterRequestKind
	SourceSlot          int16
	SelectionContext    uint16
	Selections          []currentBoosterSelectionRequest
	SelectedItemID      uint32
	SelectionOption     byte
	TrailingUIByte      byte
	RewardMultiplier    uint32
	ConsumePremiumDaily bool
	PremiumDailySlot    int64
}

type currentBoosterSelectionCandidate struct {
	ItemID       uint32
	Count        uint32
	CategoryKind int64
	Option       byte
	// UpgradeLevel 来自本分类的 [booster equipment upgrade]，只对装备行生效；
	// 0 表示"不额外强化"。升级预约（活动 222）发放的武器靠它决定 +N。
	UpgradeLevel byte
}

type currentBoosterRewardPlacement byte

const (
	currentBoosterRewardMain currentBoosterRewardPlacement = iota
	currentBoosterRewardAvatar
	currentBoosterRewardPetBody
	currentBoosterRewardPetArtifact
	currentBoosterRewardPetConsumable
)

type currentBoosterReward struct {
	Definition dungeonDropItemDefinition
	Count      uint32
	Option     byte
	Seal       bool
	Placement  currentBoosterRewardPlacement
	// UpgradeLevel 是选中分类的 [booster equipment upgrade] 带来的强化等级，
	// 由选中判定回填；仅对主背包装备生效。
	UpgradeLevel byte
}

type currentBoosterGrantedReward struct {
	ItemID uint32
	Count  uint32
}

type currentBoosterDefinition struct {
	Source            dungeonDropItemDefinition
	StackableType     string
	SelectionRequired int
	Selection         []currentBoosterSelectionCandidate
	SelectionCategory map[uint16][]currentBoosterSelectionCandidate
	MaterialItemID    int64
	MaterialCount     int64
	Random            alignedcmd.MagicBoxResolution
}

type currentBoosterCommitResult struct {
	SourceItemID    uint32
	SourceSlot      int16
	SourceRemaining uint32
	// MaterialItemID/MaterialSlot/MaterialRemaining describe the PVF opening
	// material consumed alongside the source box. MaterialItemID==0 means this
	// opening had no additional material requirement.
	MaterialItemID    int64
	MaterialSlot      int16
	MaterialRemaining uint32
	// SourceCooltimeUntil 是这次开启落下的冷却到期时间（unix 秒），0 = 该道具没有
	// [cooltime maintenance]，不参与持久冷却。
	SourceCooltimeUntil int64
	// SourceDailyUsed / SourceDailyLimit 是这次开启之后的「每天可用次数」进度
	// （PVF `[total usable count]`）。Limit<=0 表示该道具没有声明上限，本次既没读也
	// 没写计数，也不需要发 op541。
	SourceDailyUsed         int64
	SourceDailyLimit        int64
	GoldGranted             uint32
	GoldAfter               int64
	Rewards                 []currentBoosterGrantedReward
	ChangedLists            []byte
	RewardSlots             []alignedcmd.ItemSlotRefresh
	EpicBookRows            []currentEpicBookBalanceRow
	PremiumActivations      []currentCeraShopPremiumActivation
	ReviveCoinWalletChanged bool
	// VoucherCredits 是本次开启里直接入账号钱包的代币券 / 欢乐代币券数量。
	// 券在 PVF 里是普通 `[material]`，客户端没有任何把它变成钱包余额的途径，
	// 所以服务端在发放时就入账、不占背包格（见 cerashop/voucher.go）。
	VoucherCredits         dnfcerashop.VoucherCounts
	LotteryDisplayEntry    currentItemListEntry
	LotteryDisplayEntrySet bool
}

func parseCurrentBoosterOpenRequest(body []byte) (currentBoosterOpenRequest, error) {
	switch len(body) {
	case currentBoosterRandomBodySize:
		return currentBoosterOpenRequest{
			Kind:       currentBoosterRequestRandom,
			SourceSlot: int16(binary.LittleEndian.Uint16(body[0:2])),
		}, nil
	case currentBoosterSelectionBodySize:
		selectedItemID := binary.LittleEndian.Uint32(body[4:8])
		if selectedItemID == 0 {
			return currentBoosterOpenRequest{}, errCurrentBoosterRequestMalformed
		}
		return currentBoosterOpenRequest{
			Kind:             currentBoosterRequestSelection,
			SourceSlot:       int16(binary.LittleEndian.Uint16(body[0:2])),
			SelectionContext: binary.LittleEndian.Uint16(body[2:4]),
			Selections: []currentBoosterSelectionRequest{{
				ItemID: selectedItemID,
				Option: body[8],
			}},
			SelectedItemID:  selectedItemID,
			SelectionOption: body[8],
			TrailingUIByte:  body[9],
		}, nil
	}
	// 6+9n 必须排在紧凑体之前：n 是 4 的倍数时（n=4/8/…，长度 42/78/…）两条长度判据会同时
	// 成立，而那几档长度在现网确实由"逐项带 option"的多选写法发出（booster_item_test.go 里
	// 的八件套真实包体），不能改判成紧凑体。
	if currentBoosterHasMultiSelectionShape(len(body)) {
		return parseCurrentBoosterMultiSelectionRequest(body)
	}
	if count, ok := currentBoosterCompactSelectionCount(len(body)); ok {
		return parseCurrentBoosterCompactSelectionRequest(body, count)
	}
	return currentBoosterOpenRequest{}, fmt.Errorf(
		"%w: body_len=%d want=2_10_6_plus_9n_or_4_plus_4n_plus_2",
		errCurrentBoosterRequestMalformed,
		len(body),
	)
}

// currentBoosterHasMultiSelectionShape 就是"逐项带 option 的多选写法"（6+9n）的长度判据。
// 抽成函数是为了让分发器和解析器永远给出同一个答案：这两处一旦不一致，就会出现
// "分发器把它当紧凑体、解析器又把它当多选体"的错位。
func currentBoosterHasMultiSelectionShape(length int) bool {
	return length > currentBoosterSelectionBodySize &&
		length >= currentBoosterMultiBodyBaseSize+currentBoosterMultiEntrySize &&
		(length-currentBoosterMultiBodyBaseSize)%currentBoosterMultiEntrySize == 0
}

// currentBoosterCompactSelectionCount 报告紧凑选择体的选中件数 N（长度 = 6 + 4N，N>=1）。
// 长度 10（N=1）由 parseCurrentBoosterOpenRequest 的 case 分支原样处理（语义与这里等价），
// 所以走到这里的只可能是 N>=2。
func currentBoosterCompactSelectionCount(length int) (int, bool) {
	remainder := length - currentBoosterCompactBodyBaseSize
	if length <= currentBoosterSelectionBodySize ||
		remainder <= 0 ||
		remainder%currentBoosterCompactEntrySize != 0 {
		return 0, false
	}
	count := remainder / currentBoosterCompactEntrySize
	if count < 1 || count > currentBoosterMaxRewardRows || count > math.MaxUint8 {
		return 0, false
	}
	return count, true
}

// parseCurrentBoosterCompactSelectionRequest 解析 4 + 4N + 2 的紧凑选择体。
//
// 选项与尾字节整条请求共用一份：现网这一形态的礼盒，候选行都是 etc/stackable（PVF 里
// 每行只有 item/count，没有 option），需要逐项 option 的装扮类选择礼盒走 6+9n
// （八件套真实包体）。客户端给的任何数量都不作数：每件的数量由 PVF 候选行决定，件数由
// `[booster selection num]` 在 currentBoosterSelectedCandidates 里卡死，道具号必须出现在
// 该礼盒该类别里 —— 与 10 字节单选走的是同一条校验。
func parseCurrentBoosterCompactSelectionRequest(body []byte, count int) (currentBoosterOpenRequest, error) {
	option := body[len(body)-2]
	selections := make([]currentBoosterSelectionRequest, 0, count)
	seen := make(map[uint32]struct{}, count)
	for index := 0; index < count; index++ {
		offset := 4 + index*currentBoosterCompactEntrySize
		itemID := binary.LittleEndian.Uint32(body[offset : offset+4])
		if itemID == 0 {
			return currentBoosterOpenRequest{}, fmt.Errorf(
				"%w: selection_index=%d item=0",
				errCurrentBoosterRequestMalformed,
				index,
			)
		}
		if _, duplicate := seen[itemID]; duplicate {
			return currentBoosterOpenRequest{}, fmt.Errorf("%w: duplicate_item=%d", errCurrentBoosterRequestMalformed, itemID)
		}
		seen[itemID] = struct{}{}
		selections = append(selections, currentBoosterSelectionRequest{ItemID: itemID, Option: option})
	}
	return currentBoosterOpenRequest{
		Kind:             currentBoosterRequestSelection,
		SourceSlot:       int16(binary.LittleEndian.Uint16(body[0:2])),
		SelectionContext: binary.LittleEndian.Uint16(body[2:4]),
		Selections:       selections,
		SelectedItemID:   selections[0].ItemID,
		SelectionOption:  selections[0].Option,
		TrailingUIByte:   body[len(body)-1],
	}, nil
}

func parseCurrentBoosterMultiSelectionRequest(body []byte) (currentBoosterOpenRequest, error) {
	if !currentBoosterHasMultiSelectionShape(len(body)) {
		return currentBoosterOpenRequest{}, fmt.Errorf(
			"%w: body_len=%d want=2_10_6_plus_9n_or_4_plus_4n_plus_2",
			errCurrentBoosterRequestMalformed,
			len(body),
		)
	}
	selectionCount := (len(body) - currentBoosterMultiBodyBaseSize) / currentBoosterMultiEntrySize
	if selectionCount < 1 || selectionCount > currentBoosterMaxRewardRows || selectionCount > math.MaxUint8 {
		return currentBoosterOpenRequest{}, fmt.Errorf("%w: selection_count=%d", errCurrentBoosterRequestMalformed, selectionCount)
	}
	countOffset := 4 + selectionCount*4
	if int(body[countOffset]) != selectionCount {
		return currentBoosterOpenRequest{}, fmt.Errorf(
			"%w: selection_count_byte=%d want=%d",
			errCurrentBoosterRequestMalformed,
			body[countOffset],
			selectionCount,
		)
	}
	recordOffset := countOffset + 1
	selections := make([]currentBoosterSelectionRequest, 0, selectionCount)
	seen := make(map[uint32]struct{}, selectionCount)
	for index := 0; index < selectionCount; index++ {
		prefixItemID := binary.LittleEndian.Uint32(body[4+index*4 : 8+index*4])
		entryOffset := recordOffset + index*5
		recordItemID := binary.LittleEndian.Uint32(body[entryOffset : entryOffset+4])
		if prefixItemID == 0 || recordItemID != prefixItemID {
			return currentBoosterOpenRequest{}, fmt.Errorf(
				"%w: selection_index=%d prefix_item=%d record_item=%d",
				errCurrentBoosterRequestMalformed,
				index,
				prefixItemID,
				recordItemID,
			)
		}
		if _, duplicate := seen[recordItemID]; duplicate {
			return currentBoosterOpenRequest{}, fmt.Errorf("%w: duplicate_item=%d", errCurrentBoosterRequestMalformed, recordItemID)
		}
		seen[recordItemID] = struct{}{}
		selections = append(selections, currentBoosterSelectionRequest{
			ItemID: recordItemID,
			Option: body[entryOffset+4],
		})
	}
	return currentBoosterOpenRequest{
		Kind:             currentBoosterRequestSelection,
		SourceSlot:       int16(binary.LittleEndian.Uint16(body[0:2])),
		SelectionContext: binary.LittleEndian.Uint16(body[2:4]),
		Selections:       selections,
		SelectedItemID:   selections[0].ItemID,
		SelectionOption:  selections[0].Option,
		TrailingUIByte:   body[len(body)-1],
	}, nil
}

// buildCurrentBoosterSuccessBody matches current NoPack sub_1D2BFF0 after
// the shared success byte: u32 source item, u16 source slot, u32 remaining,
// u32 special-result mode (zero for ordinary boxes), u16 reward count, then
// rewardCount * {u32 item, u32 display count}.
func buildCurrentBoosterSuccessBody(result currentBoosterCommitResult) []byte {
	var writer packetWriter
	writer.writeUint32(result.SourceItemID)
	writer.writeUint16(uint16(result.SourceSlot))
	writer.writeUint32(result.SourceRemaining)
	writer.writeUint32(0)
	writer.writeUint16(uint16(len(result.Rewards)))
	for _, reward := range result.Rewards {
		writer.writeUint32(reward.ItemID)
		writer.writeUint32(reward.Count)
	}
	return writer.bytes()
}
