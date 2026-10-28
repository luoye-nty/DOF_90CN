package dnfbridge

import (
	"testing"
)

// 现场 2026-10-03（服主报「塔罗牌用不了」，角色 1 礼白666）：连点 6 次，6 次的拒绝
// 原因**逐字相同**，都是奖池的第一项：
//
//	reason=random reward item requires a complete runtime-PVF outcome table:
//	       item=108040415 kind="equipment" target_list=0 slots=9..64
//
// 抽 6 次全是同一项，概率上不可能是运气（1/4 的表，6 次全中同一项的几率是 1/4096）。
// 追下去发现 stackable_lottery_pvf.go 里：
//
//	roll := int64(0)
//	if s.dungeonChoice != nil { ... }      // 只在测试里被赋值
//
// —— `dungeonChoice` 在全仓**非测试代码里没有任何一处赋值**，线上它恒为 nil，于是
// roll 永远停在 0，`dungeonDropLotteryPick(entries, 0)` 永远返回第一项。这张卡从来
// 没有真正抽过。
//
// 本用例就是这条的回归锁：**不注入随机源**（线上就是这样）时也必须真的掷骰子。
func TestTarotLotteryRollsWithoutAnInjectedRandomSource(t *testing.T) {
	catalog := mustCurrentTarotLotteryTestCatalog(t)
	definition, err := catalog.ResolveItem(tarotLotteryTestCardItemID)
	if err != nil {
		t.Fatal(err)
	}
	if len(definition.LotteryEntries) != 2 {
		t.Fatalf("夹具奖池=%d 项 want 2（这条用例的前提）", len(definition.LotteryEntries))
	}
	// 刻意**不**设 service.dungeonChoice：这就是线上的形状。
	service := &Service{pvfItemCatalog: catalog}

	const draws = 200
	first, second := definition.LotteryEntries[0].ItemID, definition.LotteryEntries[1].ItemID
	seen := map[int64]int{}
	sawRollOutsideRange := false
	for i := 0; i < draws; i++ {
		resolution, resolveErr := service.resolveCurrentTarotLotteryCard(definition)
		if resolveErr != nil {
			t.Fatalf("第 %d 次抽取失败: %v", i, resolveErr)
		}
		if resolution == nil || resolution.Drawn == nil {
			t.Fatalf("第 %d 次抽取没有结果（missed=%t）", i, resolution != nil && resolution.Missed)
		}
		if resolution.Roll < 0 || resolution.Roll >= resolution.TotalWeight {
			sawRollOutsideRange = true
		}
		seen[resolution.Drawn.Reward.ItemID]++
	}
	if sawRollOutsideRange {
		t.Fatalf("%d 次抽取里有 roll 落在 [0,total) 之外", draws)
	}
	// 权重 10000:10000，200 次里两项都该出现。钉死"不止一项"就够 ——
	// 旧代码（roll 恒 0）在这里必红：它只可能产出第一项。
	if seen[second] == 0 {
		t.Fatalf("抽了 %d 次，第二项 %d 一次都没出现（命中分布=%v）——"+
			"roll 又被钉死了", draws, second, seen)
	}
	if seen[first] == 0 {
		t.Fatalf("抽了 %d 次，第一项 %d 一次都没出现（命中分布=%v）", draws, first, seen)
	}
	t.Logf("dungeonChoice=nil（线上形状）下 %d 次抽取分布=%v", draws, seen)
}

// 注入源在时必须仍然优先，这样既有测试与"可复现抽样"的能力不被这次兜底破坏。
func TestTarotLotteryPrefersTheInjectedRandomSource(t *testing.T) {
	catalog := mustCurrentTarotLotteryTestCatalog(t)
	definition, err := catalog.ResolveItem(tarotLotteryTestCardItemID)
	if err != nil {
		t.Fatal(err)
	}
	service := &Service{pvfItemCatalog: catalog}
	second := definition.LotteryEntries[1]
	boundary := definition.LotteryEntries[0].Weight // 第二项的起始 roll
	service.dungeonChoice = func(int) (int, error) { return int(boundary), nil }

	resolution, err := service.resolveCurrentTarotLotteryCard(definition)
	if err != nil {
		t.Fatal(err)
	}
	if resolution == nil || resolution.Drawn == nil {
		t.Fatal("注入源下没有抽到结果")
	}
	if got := resolution.Drawn.Reward.ItemID; got != second.ItemID {
		t.Fatalf("注入 roll=%d 抽到 %d want %d（第二项）", boundary, got, second.ItemID)
	}
	if resolution.Roll != boundary {
		t.Fatalf("resolution.Roll=%d want %d", resolution.Roll, boundary)
	}
}

// 注入源越界必须拒绝，而不是被静默夹到边界上：宁可这次不生效，也不能给出错误的奖。
func TestTarotLotteryRejectsAnOutOfRangeInjectedRoll(t *testing.T) {
	catalog := mustCurrentTarotLotteryTestCatalog(t)
	definition, err := catalog.ResolveItem(tarotLotteryTestCardItemID)
	if err != nil {
		t.Fatal(err)
	}
	service := &Service{pvfItemCatalog: catalog}
	total := dungeonDropLotteryTotalWeight(definition.LotteryEntries)
	if total <= 0 {
		t.Fatal("夹具奖池总权重必须为正")
	}
	service.dungeonChoice = func(int) (int, error) { return int(total), nil } // total 已在区间外
	if _, err := service.resolveCurrentTarotLotteryCard(definition); err == nil {
		t.Fatal("越界的注入 roll 必须报错，不能被当成合法抽取")
	}
}
