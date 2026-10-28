package dnfbridge

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"

	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfinventory "longheng.io/server/internal/modules/dnf/inventory"
	dnfproto "longheng.io/server/internal/modules/dnf/protocol"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

// tarot_lottery_item.go —— 塔罗牌类消耗品（PVF `[stackable type]` =
// `[monster tarot card]`，例：stackable/10159001/10159697.stk「占星师罗赛洛 塔罗牌」）
// 在**现网客户端的 op27 通道**上的使用。
//
// 为什么必须挂 op27，而不是 op44：
//
//	现场 2026-09-30T11:52:47Z（本地 19:52:47）的实机包日志
//	（runtime/logs/packet/20260930/packet_log-20260930-002.log）里，玩家右键
//	stackable/10159001/10159698.stk 发出的请求是
//
//	    RECV kind=game-legacy cmd=1 type=27 ENUM_CMDPACKET_USE_LOTTERY_ITEM
//	         header_seq=43 body_len=4 hex=011b0011000000a07499d82b0000006e00
//	         ⇒ body = (u16 mode=0, u16 slot=110)
//
//	而服务端在 booster 侧把它当"非随机罐"拒了：
//
//	    EVENT kind=game-lottery-item-rejected slot=110 stackable_type=
//	         reason=dnf booster runtime PVF data is invalid:
//	                source item=10159698 type="[monster tarot card]" is not random
//
//	拒绝点是 booster_pvf.go:64（op27 → lottery_item.go:51 prepareCurrentBooster）。
//	上一轮实现把塔罗牌分支挂在 stackable_use_pvf.go:104 的 **op44** 解析器里，
//	而 alignedStackableUseResolverForCommand（stackable_use_pvf.go:47-53）只对
//	44/515/418/30 生效 —— op27 永远到不了那里。这就是"修了但是没修好"的根因：
//	解析器、Owner、事务边界都写对了，只是**挂错了 opcode**。
//
// 本文件只做**加法**：它是 handleCurrentLotteryItem 最前面的一个显式早退分支，
// 只有源道具确实是 `[monster tarot card]` 时才接管这次 op27；任何其它道具都原样
// 落回既有的 booster/罐子路径（那条路径一个字节都没有改动）。
//
// 回包形状沿用既有的 op27 契约（lottery_item.go:107-114 记录的 NoPack.exe
// sub_1D31260）：u8 成功位 + u16 源槽 + 一条 0x77 字节道具行 + u32。
// 敢复用的理由是客户端只有**一个** op27 读取器，塔罗牌与抽奖罐共用它。
//
// 刻意**不做**的两件事（都有证据边界，不该猜）：
//
//   - 不消费"魔王契约双倍"（op27 body 的 mode=1）。塔罗牌的 PVF 里没有任何
//     双倍/契约声明，凭空扣一次契约日次数比不生效更糟；这里只把 mode 记进日志。
//   - 不补发 op14/op13 道具表。lottery_item.go:107-114 的既有教训：op27 结果包
//     之后插入道具表包，会替换客户端动画队列仍握着的道具对象，下一次自动开罐
//     可以解引用陈旧状态并把进程打崩。发奖落点已经由结果包那一行给出。

// currentTarotLotteryStackableType 是 PVF 里塔罗牌类消耗品的 `[stackable type]`
// 归一化文本（normalizeDungeonDropStackableType 之后）。
const currentTarotLotteryStackableType = "[monster tarot card]"

// currentTarotLotterySourceStack 只读地取出 op27 源槽里的道具。
//
// 任何读不到的情况都返回 ok=false：那种情况下必须落回既有路径，让既有的错误与
// 日志原样发生，本文件不能改变"非塔罗牌 op27"的任何行为。
func currentTarotLotterySourceStack(
	ctx context.Context,
	repositories dnfrepo.Group,
	characterID string,
	slot int16,
) (dnfrepo.ItemStack, uint32, bool) {
	if repositories.Inventory == nil || characterID == "" {
		return dnfrepo.ItemStack{}, 0, false
	}
	record, found, err := repositories.Inventory.Load(ctx, characterID)
	if err != nil || !found {
		return dnfrepo.ItemStack{}, 0, false
	}
	// 槽键必须与 booster 路径同源（prepareCurrentBooster 用的是同一个函数），
	// 否则两条路会对"slot"给出不同的理解。
	stack, found := record.Slots[currentCeraShopInventorySlotKey(dnfrepo.MainInventoryListType, slot)]
	if !found || stack.ItemID <= 0 || stack.ItemID > int64(^uint32(0)) || stack.Count <= 0 {
		return dnfrepo.ItemStack{}, 0, false
	}
	return stack, uint32(stack.ItemID), true
}

// currentTarotLotteryDefinition 报告 op27 源槽里的道具是不是塔罗牌。它是本文件
// 与既有罐子路径之间**唯一**的分岔条件：返回 false 时调用方必须原样继续。
func (s *Service) currentTarotLotteryDefinition(
	ctx context.Context,
	session *gameSession,
	catalog *pvfDungeonDropCatalog,
	slot int16,
) (dungeonDropItemDefinition, uint32, string, bool) {
	if s == nil || session == nil || catalog == nil || slot < 0 || session.selectedCharacterID == 0 {
		return dungeonDropItemDefinition{}, 0, "", false
	}
	repositories, ok := s.repositoryGroup()
	if !ok {
		return dungeonDropItemDefinition{}, 0, "", false
	}
	characterID := fmt.Sprint(session.selectedCharacterID)
	_, itemID, ok := currentTarotLotterySourceStack(ctx, repositories, characterID, slot)
	if !ok {
		return dungeonDropItemDefinition{}, 0, "", false
	}
	definition, err := catalog.ResolveItem(itemID)
	if err != nil {
		// 解析不了的源道具交回既有路径：由它给出与改动前逐字相同的错误。
		return dungeonDropItemDefinition{}, 0, "", false
	}
	if normalizeDungeonDropStackableType(definition.StackableType) != currentTarotLotteryStackableType {
		return dungeonDropItemDefinition{}, 0, "", false
	}
	return definition, itemID, characterID, true
}

// sendCurrentTarotLotteryFailure 与既有 op27 拒绝回包完全同形（lottery_item.go:30-36：
// 首字节成功位 0，次字节原生失败原因 4）。走同一个构造函数，避免两条路给出不同的
// 失败字节。
func (s *Service) sendCurrentTarotLotteryFailure(session *gameSession) error {
	return s.sendGameUpperRawClass(
		session,
		uint16(dnfenum.CmdPacketUseLotteryItem),
		buildCurrentLotteryErrorBody(),
		dnfproto.DefaultChannelClassification,
	)
}

// tryHandleCurrentTarotLotteryItem 是 handleCurrentLotteryItem 的塔罗牌分派。
//
// 返回 handled=true 表示这次 op27 已由本函数处理完（成功或已回拒绝包），调用方必须
// 直接返回；handled=false 表示源道具不是塔罗牌，调用方必须继续走原来那条一字未改的
// 罐子路径。所有"读不到 / 不是塔罗牌"的路径都只读、不产生任何包与事务。
func (s *Service) tryHandleCurrentTarotLotteryItem(
	ctx context.Context,
	session *gameSession,
	catalog *pvfDungeonDropCatalog,
	wantsDouble bool,
	slot int16,
) (bool, error) {
	definition, itemID, characterID, ok := s.currentTarotLotteryDefinition(ctx, session, catalog, slot)
	if !ok {
		return false, nil
	}

	// —— 到这里为止全是只读；下面才真正接管这次 op27。 ——
	//
	// 失败一律回 op27 拒绝包并返回 handled=true：既不落回罐子路径（它必然再拒一次
	// 并多打一条 is not random 的误导日志），也不把错误抛给上层。
	reject := func(stage string, reason error) (bool, error) {
		// 沿用既有的 game-lottery-item-rejected 名字（reason 里带 route 前缀），
		// 这样任何盯着 op27 拒绝的既有查询不会因为这次分派而失明。
		s.logGameEvent(session, "game-lottery-item-rejected",
			"slot", slot,
			"stackable_type", definition.StackableType,
			"route", "tarot_lottery",
			"stage", stage,
			"reason", fmt.Sprintf("tarot lottery route: %v", reason))
		s.logGameEvent(session, "game-tarot-lottery-item-rejected",
			"slot", slot,
			"item", itemID,
			"pvf_path", definition.PVFPath,
			"stackable_type", definition.StackableType,
			"stage", stage,
			"reason", reason)
		return true, s.sendCurrentTarotLotteryFailure(session)
	}

	// int32 溢出守卫：Command.ItemTemplateID 是 int32，而 Owner 会拿它与契约里的
	// uint32 源道具比。截断过的 id 会让契约校验永远不成立，这里显式挡住。
	if itemID > math.MaxInt32 {
		return reject("item_id", fmt.Errorf("item=%d exceeds the command's int32 item field", itemID))
	}

	// PVF 契约：`[lottery use cost]`（金币）+ `[int data]` 加权 (item, weight, count)
	// 三元组。抽中哪一项在**解析阶段**就定下来（随机源是服务自身可注入的
	// s.dungeonChoice，与迷宫/图案抽取同一个口子），Owner 侧因此是纯确定性的。
	// 解析失败/落空区都在这里 fail closed。
	resolution, resolveErr := s.resolveCurrentTarotLotteryCard(definition)
	if resolveErr != nil {
		return reject("contract", resolveErr)
	}
	if resolution == nil || resolution.Drawn == nil {
		return reject("contract", fmt.Errorf("item=%d produced no drawn reward (missed=%t)", itemID, resolution != nil && resolution.Missed))
	}
	drawnItemID := resolution.Drawn.Reward.ItemID
	drawnCount := resolution.Drawn.Count
	if drawnItemID <= 0 || drawnItemID > int64(^uint32(0)) || drawnCount <= 0 || drawnCount > int64(^uint32(0)) {
		return reject("reward", fmt.Errorf("item=%d drawn reward id=%d count=%d is out of range", itemID, drawnItemID, drawnCount))
	}

	repositories, ok := s.repositoryGroup()
	if !ok {
		return reject("owner", dnfinventory.ErrOwnerUnavailable)
	}
	owner, ownerErr := dnfinventory.NewOwner(repositories)
	if ownerErr != nil {
		return reject("owner", ownerErr)
	}

	// 金币与道具必须同一笔事务：UseTarotLotteryCard 走
	// withinCharacterAssetTransaction（characters + inventory + equipment 三个仓库
	// 同事务），任一失败整笔回滚。见 inventory/tarot_lottery_card.go 的文件头。
	command := dnfinventory.Command{
		Operation:           "use_tarot_lottery_card",
		AccountID:           s.accountIDForSession(session),
		SelectedCharacterID: session.selectedCharacterID,
		SourceListType:      dnfrepo.MainInventoryListType,
		SourceSlotIndex:     slot,
		ItemTemplateID:      int32(itemID),
	}
	result, useErr := owner.UseTarotLotteryCard(ctx, command, *resolution)
	if useErr != nil {
		if errors.Is(useErr, dnfinventory.ErrTarotLotteryMissed) {
			// 真档 10159697/10159698 的权重合计 999998/1000000，存在 2/100 万的
			// 空隙；现有实现选择"整次中止、不扣钱不扣道具"（见
			// inventory/tarot_lottery_card.go:34-37）。把 roll 打进日志，空隙频率
			// 可被真实流量证实或否证。
			s.logGameEvent(session, "game-tarot-lottery-item-gap",
				"slot", slot, "item", itemID,
				"roll", resolution.Roll, "total_weight", resolution.TotalWeight,
				"gold_cost", resolution.GoldCost)
		}
		return reject("commit", useErr)
	}

	// 结果行：单条道具行，数量是**本次抽中的数量**（不是合并后的堆叠总数）——
	// 与既有罐子路径的注释同口径（gameplay_module_booster.go:541-555）。
	commit := currentBoosterCommitResult{
		SourceItemID:    itemID,
		SourceSlot:      slot,
		SourceRemaining: uint32(result.RemainingCount),
		Rewards: []currentBoosterGrantedReward{{
			ItemID: uint32(drawnItemID),
			Count:  uint32(drawnCount),
		}},
	}
	if len(result.RandomRewardSlots) > 0 {
		// 落点用 Owner 真正写进去的那一格，而不是源槽：源槽在还剩多张牌时仍然
		// 被源道具占着，把奖励画在源槽上会让客户端把两张牌混在一格。
		//
		// 这一行的**形状必须跟着奖励的 kind 走**：客户端拿这一行直接建对象，
		// 装备行（真档 4 条里有 3 条是装备）要带 item_kind/equipment_type/耐久，
		// 否则它会按可叠加物去建，画出来是一件 0 耐久的错东西。
		// 数据全部来自契约里的 reward（resolveCurrentMagicBoxRewardItem 已经
		// 从同一份 PVF 读好），这里不另做一次 PVF 查询。
		displayStack := dnfrepo.ItemStack{ItemID: drawnItemID, Count: drawnCount}
		if strings.EqualFold(strings.TrimSpace(resolution.Drawn.Reward.Kind), "equipment") {
			displayStack.Count = 1
			displayStack.Extra = map[string]string{
				"item_kind":       "equipment",
				"pvf_path":        resolution.Drawn.Reward.PVFPath,
				"equipment_type":  resolution.Drawn.Reward.EquipmentType,
				"amount_or_count": "0",
			}
			if resolution.Drawn.Reward.Seal {
				displayStack.Extra["seal_flag"] = "1"
			}
			if durability := resolution.Drawn.Reward.Durability; durability > 0 {
				value := strconv.FormatUint(uint64(durability), 10)
				displayStack.Extra["durability"] = value
				displayStack.Extra["max_durability"] = value
			}
		}
		commit.LotteryDisplayEntry = currentItemListEntryFromStack(
			dnfrepo.MainInventoryListType,
			result.RandomRewardSlots[0],
			displayStack,
		)
		commit.LotteryDisplayEntrySet = true
	}
	resultBody, bodyErr := buildCurrentLotteryResultBody(commit)
	if bodyErr != nil {
		return reject("result_body", bodyErr)
	}
	if sendErr := s.sendGameUpperRawClass(
		session,
		uint16(dnfenum.CmdPacketUseLotteryItem),
		resultBody,
		dnfproto.DefaultChannelClassification,
	); sendErr != nil {
		return true, sendErr
	}

	// `[lottery use cost]` 是**服务端**扣的金币（inventory/tarot_lottery_card.go:155
	// 在同一个事务里 `character.Stats["gold"] -= GoldCost`）。客户端不知道这笔花费，
	// 所以必须把权威余额推给它。
	//
	// 形状与既有路径完全一致：罐子开出金币时 lottery_item.go:115-124 就是在 op27
	// 结果包之后发这**一个**钱包包。buildCurrentGoldStateBody 是**绝对值**
	// （current_item_update_protocol.go:5），与客户端是否本地扣过无关，重复下发也
	// 不会算错。仍然不发 op14/op13 道具表（见文件头）。
	goldAfter := int64(0)
	if resolution.GoldCost > 0 {
		if character, found, loadErr := repositories.Character.Load(ctx, characterID); loadErr == nil && found {
			goldAfter = character.Stats["gold"]
			if sendErr := s.sendGameUpperRawClass(
				session,
				uint16(dnfenum.CmdPacketWalkoutPartyMember),
				buildCurrentGoldStateBody(goldAfter),
				0,
			); sendErr != nil {
				return true, sendErr
			}
		}
	}

	s.logGameEvent(session, "game-tarot-lottery-item-open-committed",
		"slot", slot,
		"source_item", itemID,
		"source_remaining", result.RemainingCount,
		"source_pvf_path", definition.PVFPath,
		"stackable_type", definition.StackableType,
		"gold_cost", resolution.GoldCost,
		"gold_after", goldAfter,
		"roll", resolution.Roll,
		"total_weight", resolution.TotalWeight,
		"reward_item", drawnItemID,
		"reward_count", drawnCount,
		"reward_slots", fmt.Sprint(result.RandomRewardSlots),
		"requested_double", wantsDouble,
		"double_honored", false,
		"op27_body_len", len(resultBody),
		"state_source", "client_op27_use_lottery_item_same_reader_as_the_jars",
		"inventory_sync", "native_op27_only_then_one_absolute_gold_wallet_packet")
	return true, nil
}
