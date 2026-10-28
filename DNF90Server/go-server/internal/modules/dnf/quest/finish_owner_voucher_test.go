package quest

import (
	"testing"

	dnfcerashop "longheng.io/server/internal/modules/dnf/cerashop"
)

// 任务奖励里的代币券 / 欢乐代币券处理（纯函数部分）。
//
// 券在道具 PVF 里是普通 `[material]`、**没有 `[action type]`**，客户端根本没有任何
// 途径把它们换成钱包余额；当普通道具发下去，玩家只会拿到一件永远用不掉的"材料"。
// 所以发放前先把它们从奖励规则里摘出来，改成入账号钱包。

func finishVoucherTestRule(itemID, count int64) RewardItemRule {
	return RewardItemRule{ItemID: itemID, Count: count}
}

// 拆分必须只摘出券，并且**保持非券规则的原有顺序**——顺序变了会改变落格结果，
// 那是"修了 A 坏了 B"的典型来源。
func TestSplitFinishVoucherRulesKeepsOrdinaryRulesInOrder(t *testing.T) {
	rules := []RewardItemRule{
		finishVoucherTestRule(100050, 1),
		finishVoucherTestRule(dnfcerashop.TokenCeraVoucherItemID, 100),
		finishVoucherTestRule(100056, 3),
		finishVoucherTestRule(dnfcerashop.HappyTokenCeraVoucherItemID, 7),
		finishVoucherTestRule(100057, 2),
	}
	vouchers, items := splitFinishVoucherRules(rules)
	if len(vouchers) != 2 || len(items) != 3 {
		t.Fatalf("vouchers=%+v items=%+v", vouchers, items)
	}
	for index, want := range []int64{100050, 100056, 100057} {
		if items[index].ItemID != want {
			t.Fatalf("非券规则顺序被改变: items[%d]=%d want=%d", index, items[index].ItemID, want)
		}
	}
	if vouchers[0].ItemID != dnfcerashop.TokenCeraVoucherItemID ||
		vouchers[1].ItemID != dnfcerashop.HappyTokenCeraVoucherItemID {
		t.Fatalf("券的顺序被改变: %+v", vouchers)
	}
}

// 没有券时必须原样返回（不得产生任何副作用）。
func TestSplitFinishVoucherRulesLeavesVoucherFreeRewardsUntouched(t *testing.T) {
	if vouchers, items := splitFinishVoucherRules([]RewardItemRule{finishVoucherTestRule(1, 1)}); len(vouchers) != 0 || len(items) != 1 {
		t.Fatalf("无券时不该有变化: vouchers=%+v items=%+v", vouchers, items)
	}
	if vouchers, items := splitFinishVoucherRules(nil); len(vouchers) != 0 || len(items) != 0 {
		t.Fatalf("空输入: vouchers=%+v items=%+v", vouchers, items)
	}
}

func TestFinishVoucherCountsSumsPerKind(t *testing.T) {
	counts, err := finishVoucherCounts([]RewardItemRule{
		finishVoucherTestRule(dnfcerashop.TokenCeraVoucherItemID, 100),
		finishVoucherTestRule(dnfcerashop.HappyTokenCeraVoucherItemID, 7),
		finishVoucherTestRule(dnfcerashop.TokenCeraVoucherItemID, 5),
	})
	if err != nil {
		t.Fatal(err)
	}
	if counts.TokenCera != 105 || counts.HappyTokenCera != 7 {
		t.Fatalf("counts=%+v want token=105 happy=7", counts)
	}
}

// 坏值必须被挡住：券既然不走落格路径，就没有别的东西替我们校验这些字段了
// （allocateFinishRewardItems 的那套校验对券不再执行）。
func TestFinishVoucherCountsRejectsMalformedRules(t *testing.T) {
	cases := map[string]RewardItemRule{
		"zero_count":  finishVoucherTestRule(dnfcerashop.TokenCeraVoucherItemID, 0),
		"negative":    finishVoucherTestRule(dnfcerashop.TokenCeraVoucherItemID, -1),
		"non_voucher": finishVoucherTestRule(100050, 1),
		"zero_item":   finishVoucherTestRule(0, 1),
		"oversized":   finishVoucherTestRule(dnfcerashop.TokenCeraVoucherItemID, 1<<33),
	}
	for name, rule := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := finishVoucherCounts([]RewardItemRule{rule}); err == nil {
				t.Fatalf("坏值必须报错: %+v", rule)
			}
		})
	}
}

// 汇总后超过客户端可读范围（i32）时必须报错，而不是让它截断成负数。
func TestFinishVoucherCountsRejectsPerKindOverflow(t *testing.T) {
	if _, err := finishVoucherCounts([]RewardItemRule{
		finishVoucherTestRule(dnfcerashop.TokenCeraVoucherItemID, 1<<31-1),
		finishVoucherTestRule(dnfcerashop.TokenCeraVoucherItemID, 1),
	}); err == nil {
		t.Fatal("代币券汇总溢出必须报错")
	}
	if _, err := finishVoucherCounts([]RewardItemRule{
		finishVoucherTestRule(dnfcerashop.HappyTokenCeraVoucherItemID, 1<<31-1),
		finishVoucherTestRule(dnfcerashop.HappyTokenCeraVoucherItemID, 1),
	}); err == nil {
		t.Fatal("欢乐代币券汇总溢出必须报错")
	}
	// 刚好到上限是允许的。
	counts, err := finishVoucherCounts([]RewardItemRule{
		finishVoucherTestRule(dnfcerashop.TokenCeraVoucherItemID, 1<<31-1),
	})
	if err != nil || counts.TokenCera != 1<<31-1 {
		t.Fatalf("刚好到上限应当允许: err=%v counts=%+v", err, counts)
	}
}
