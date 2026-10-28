package alignedcmd

// 抽奖类消耗品（PVF `[stackable type]` = `[monster tarot card]`）的使用契约。
//
// 与 MagicBoxResolution 同族，但有两处必须分开表达的东西：
//   - **金币花费**：PVF `[lottery use cost]` 扣的是**金币**（服主 2026-09-29 确认：
//     塔罗牌这次就是 100000）。它的提交必须和道具同笔，所以交给
//     Owner.withinCharacterAssetTransaction（金币 + 道具 + 装备三个仓库同一事务），
//     不能走只有背包的 withinInventoryTransaction。
//   - **每项数量**：`[int data]` 的三元组是 (item, weight, count)，count 可能不是 1
//     （真档 10159697 的最后一行是 `10159142 300000 250`），所以中奖项自带 Count。
//
// 抽取在**服务端解析阶段**完成（用服务自身的可注入选择函数，测试可注入确定值），
// 这里只携带**已经抽中的那一项**：Owner 侧因此是纯确定性的，不含随机源。
type TarotLotteryOutcome struct {
	Reward MagicBoxRewardItem
	Count  int64
}

// TarotLotteryResolution is the active-PVF contract for one tarot-card use.
type TarotLotteryResolution struct {
	SourceItemID  int64
	SourcePVFPath string
	StackableType string
	// GoldCost is PVF `[lottery use cost]` in gold. It is read from the document,
	// never hard-coded per item.
	GoldCost int64
	// Drawn is the winning entry, or nil when the roll landed in the table's gap.
	Drawn *TarotLotteryOutcome
	// Missed marks a roll that landed in `[total_weight, scale)` -- a gap the
	// shipped tables can have (10159697 sums to 999998 out of 1000000). This tree
	// treats it as a failure and aborts the whole use (see useTarotLotteryCard):
	// 宁可这次不生效，也不能让玩家花了钱拿到空气。这是我们与官服可能不同的一处
	// 保守选择 —— 官方"照收钱不给东西"或"整次中止"两种做法都说得通。
	Missed      bool
	Roll        int64
	TotalWeight int64
}
