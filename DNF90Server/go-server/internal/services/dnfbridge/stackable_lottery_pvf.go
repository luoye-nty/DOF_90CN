package dnfbridge

import (
	cryptorand "crypto/rand"
	"fmt"
	"math/big"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
)

// 抽奖类消耗品（`[monster tarot card]`）的奖池解析。
//
// PVF 形状（真档 `stackable/10159001/10159697.stk`，卢克塔罗牌「占星师罗赛洛
// 塔罗牌」）：
//
//	[stackable type]
//	`[monster tarot card]` 19
//	[lottery use cost]
//	100000
//	[int data]
//	100250574 116666 1 102040539 116666 1 106030594 116666 1 10159145 350000 1 10159142 300000 250
//	[/int data]
//
// `[int data]` 是一条**变长的 (item, weight, count) 三元组**列表 —— 与 [booster info]
// 的奖励项同一个形状（dungeon_drop_catalog.go 里解析 [booster info] 的注释就这么写）。
// 尾部的残留值（凑不满一个三元组）直接丢弃，不猜、不补零。
//
// 这一层只做**解析**：没有任何使用路径消费 LotteryUseCost / LotteryEntries 之前，
// 这些字段是纯数据，行为与改动前逐字相同。

// dungeonDropLotteryEntry is one weighted candidate of a `[int data]` lottery row.
type dungeonDropLotteryEntry struct {
	ItemID int64
	Weight int64
	Count  int64
}

// parseDungeonDropLotteryEntries chunks a flat `[int data]` value list into
// (item, weight, count) triples. A trailing partial triple is dropped: a table
// that does not describe whole rows is not a table this server will draw from.
func parseDungeonDropLotteryEntries(values []int64) []dungeonDropLotteryEntry {
	if len(values) < 3 {
		return nil
	}
	entries := make([]dungeonDropLotteryEntry, 0, len(values)/3)
	for index := 0; index+2 < len(values); index += 3 {
		entries = append(entries, dungeonDropLotteryEntry{
			ItemID: values[index],
			Weight: values[index+1],
			Count:  values[index+2],
		})
	}
	return entries
}

// dungeonDropLotteryTotalWeight is the pool's total weight. Zero means "not
// drawable": an empty table or one whose weights are all non-positive.
func dungeonDropLotteryTotalWeight(entries []dungeonDropLotteryEntry) int64 {
	total := int64(0)
	for _, entry := range entries {
		if entry.ItemID <= 0 || entry.Weight <= 0 || entry.Count <= 0 {
			continue
		}
		total += entry.Weight
	}
	return total
}

// dungeonDropLotteryPick is the deterministic half of the draw: given a roll in
// [0, dungeonDropLotteryTotalWeight), it returns the entry that roll lands on.
// The caller owns randomness; this keeps the selection testable.
func dungeonDropLotteryPick(entries []dungeonDropLotteryEntry, roll int64) (dungeonDropLotteryEntry, bool) {
	if roll < 0 {
		return dungeonDropLotteryEntry{}, false
	}
	remaining := roll
	for _, entry := range entries {
		if entry.ItemID <= 0 || entry.Weight <= 0 || entry.Count <= 0 {
			continue
		}
		if remaining < entry.Weight {
			return entry, true
		}
		remaining -= entry.Weight
	}
	return dungeonDropLotteryEntry{}, false
}

// resolveCurrentTarotLotteryCard 把一张塔罗牌的 PVF 数据变成使用契约：金币花费 +
// **已经抽中的那一项**。
//
// 抽取放在这里（服务端解析阶段）而不是 Owner 里：Owner 侧因此是纯确定性的（同一份
// 契约必然产生同一笔提交，事务里没有随机源），而随机源用服务自身可注入的那一个
// （`dungeonChoice`，与迷宫/图案抽取同一个口子），测试可以直接注入确定值。
//
// 落空区（roll 落在 [total_weight, ...) 之外，真档 10159697/10159698 合计
// 999998/1000000 就有 2/100 万的空隙）在契约里标成 Missed，由 Owner 中止整次使用
// —— 见 inventory.ErrTarotLotteryMissed 的注释（我们与官服可能不同的保守选择）。
func (s *Service) resolveCurrentTarotLotteryCard(definition dungeonDropItemDefinition) (*alignedcmd.TarotLotteryResolution, error) {
	entries := definition.LotteryEntries
	total := dungeonDropLotteryTotalWeight(entries)
	if definition.LotteryUseCost <= 0 || total <= 0 {
		// fail closed：没有金币花费或没有可抽的奖池，就不是一张可用的卡。
		return nil, fmt.Errorf("%w: item=%d path=%s cost=%d total_weight=%d",
			errDungeonDropItemUnresolved, definition.ItemID, definition.PVFPath,
			definition.LotteryUseCost, total)
	}
	catalog, err := s.currentPVFItemCatalog()
	if err != nil {
		return nil, err
	}
	// 中奖道具的 PVF 文档必须从**解析这批 item id 的同一份 PVF** 里读，否则会出现
	// "用 PVF A 解析出的 id 去 PVF B 里找 [attach type]" 的错配。罐子那条抽奖路就
	// 是这么取源的（booster_pvf.go:67 `resolveCurrentMagicBox(catalog, catalog.source, ...)`），
	// 这里对齐同一口径。顺带去掉对 initialEquipmentArchive 的依赖：那张卡的使用
	// 不该再触发一次整档加载，测试也能用与罐子同一套内存 PVF 夹具跑通。
	source := catalog.source
	if source == nil {
		return nil, errDungeonDropSourceRequired
	}
	roll := int64(0)
	if s != nil && s.dungeonChoice != nil {
		chosen, choiceErr := s.dungeonChoice(int(total))
		if choiceErr != nil {
			return nil, choiceErr
		}
		if chosen < 0 || int64(chosen) >= total {
			return nil, fmt.Errorf("tarot lottery card=%d roll=%d outside [0,%d)", definition.ItemID, chosen, total)
		}
		roll = int64(chosen)
	} else {
		// **没有注入随机源时必须自己掷，不能默认 0。**
		//
		// 现场 2026-10-03（服主报「塔罗牌用不了」，角色 1 礼白666 连点 6 次）：
		// 6 次全部命中奖池的**第一项** 108040415（血红战斧，装备行），而装备行会被
		// Owner 的发奖契约拒掉 —— 于是每次都「点了没反应」。
		//
		// 6 次同一个 item 不可能是运气：dungeonChoice 只在**测试**里被赋值
		// （全仓非测试代码里没有任何一处给它赋值），线上它永远是 nil，而这里原本
		// 把 roll 留在 0，`dungeonDropLotteryPick(entries, 0)` 就永远返回第一项。
		// 换句话说这张卡**从来没有真正抽过**，只是每次都被第一条记录挡下。
		//
		// 兜底与 chooseDungeonIndex 同口径（dungeon_runtime_state.go:1213）：
		// 注入源优先，没有就用 crypto/rand。随机性必须在服务端，不能靠客户端。
		chosen, choiceErr := cryptorand.Int(cryptorand.Reader, big.NewInt(total))
		if choiceErr != nil {
			return nil, fmt.Errorf("tarot lottery card=%d roll: %w", definition.ItemID, choiceErr)
		}
		roll = chosen.Int64()
	}
	resolution := &alignedcmd.TarotLotteryResolution{
		SourceItemID:  int64(definition.ItemID),
		SourcePVFPath: definition.PVFPath,
		StackableType: definition.StackableType,
		GoldCost:      definition.LotteryUseCost,
		Roll:          roll,
		TotalWeight:   total,
	}
	if roll < 0 || roll >= total {
		// 掷到区间外（含落空区）：不抽、不扣、不发放，整次中止。
		resolution.Missed = true
		return resolution, nil
	}
	entry, ok := dungeonDropLotteryPick(entries, roll)
	if !ok {
		resolution.Missed = true
		return resolution, nil
	}
	reward, err := resolveCurrentMagicBoxRewardItem(catalog, source, entry.ItemID)
	if err != nil {
		return nil, fmt.Errorf("resolve tarot card=%d reward=%d: %w", definition.ItemID, entry.ItemID, err)
	}
	if reward.ItemID != entry.ItemID || reward.Kind == "" {
		return nil, fmt.Errorf("resolve tarot card=%d reward=%d is unresolved", definition.ItemID, entry.ItemID)
	}
	resolution.Drawn = &alignedcmd.TarotLotteryOutcome{Reward: reward, Count: entry.Count}
	return resolution, nil
}
