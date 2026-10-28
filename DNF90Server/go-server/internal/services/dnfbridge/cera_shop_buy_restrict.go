// 本文件解析 `etc/cerashop.etc` 的 `[buy restrict item]`：商城（点券商城）自己的
// 限购表，以及道具说明文案里的账号维度限购。
//
// 现场（2026-09-18 运营反馈）：
//
//	图一 每个账号限购1次 —— 买完从商城消失，大退重上又出现，还能再买
//	图二 每个角色限购1次 —— 买完不消失；限购3个的就该买满3个
//
// 为什么不能只看道具 PVF：这些商城道具**没有** `[purchase limit]` 字段（实测
// chn_490702507.stk 只有 [expiration date] 与 [package data]），限购只写在两处：
//
//  1. `etc/cerashop.etc [buy restrict item]`：4 个数字一行
//     `[序号] [商品号] [?] [次数]`
//     第 4 个数字就是客户端工具提示里的“每个角色只能购买N次”（角色维度）。
//     实证：商品 101081（深渊派对通行证）= 1，商品 104412（防虚弱药剂）= 3，
//     与运营截图里的 1 次 / 3 次完全一致。
//     第 3 个数字取值 0/1/3/7/30/90，**不是**次数（否则 104412 会显示 1 次），
//     语义未证实，因此只记日志、不据此限购（沿用本项目对语义不明字段的做法）。
//  2. 道具自己的 `[explain]` 散文，例如 490702494“每个帐号限购1个。”。这一条
//     只有文字、没有机器可读字段，所以用一条**保守**的模板解析器读出来：必须
//     同时出现“每个帐号/账号”“限购/仅限购买/只能购买”“阿拉伯数字”和“个/次”。
//     读不懂的一律不限购，并记进审核日志，绝不猜。
//
// 计数为什么按商品号：商城是按**商品**卖的（同一件道具可能有多个商品：1个装/5个装
// 各一个商品号），所以角色维度用商品号做键，并加命名空间前缀避免与道具号撞键
// （见 purchaselimit.Policy.KeyTag）。
package dnfbridge

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"longheng.io/server/internal/modules/dnf/purchaselimit"
	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
)

// currentCeraShopBuyRestrictSection 是商城限购表在 etc/cerashop.etc 里的段名。
const currentCeraShopBuyRestrictSection = "buy restrict item"

// currentCeraShopBuyRestrictRowWidth 是限购表一行的字段数：序号、商品号、未知、次数。
const currentCeraShopBuyRestrictRowWidth = 4

// currentCeraShopBuyRestrictKeyTag 是商城商品维度计数的命名空间。
const currentCeraShopBuyRestrictKeyTag = "cerashop_"

// currentCeraShopExplainLimitKeyTag 是“说明文案里的账号限购”计数的命名空间。
const currentCeraShopExplainLimitKeyTag = "cerashop_explain_"

// currentCeraShopExplainAccountLimitEnabled 允许一键关掉“说明文案里的账号限购”。
// 文案解析天生比字段解析脆，所以它单独可关，关掉后商城只剩角色维度限购。
const currentCeraShopExplainAccountLimitEnabled = true

// currentCeraShopBuyRestrictRow 是限购表的一行。
type currentCeraShopBuyRestrictRow struct {
	Index       uint32
	CommodityID uint32
	// UnknownField 是第 3 个数字：取值 0/1/3/7/30/90，不是次数（实证见文件头），
	// 语义未证实，只随日志打印。
	UnknownField uint32
	// CharacterLimit 是第 4 个数字：角色维度限购次数。
	CharacterLimit uint32
}

// parseCurrentCeraShopBuyRestrict 读 `[buy restrict item]`，返回按商品号索引的
// 角色维度限购次数。段缺失时返回空表（不报错：不同版本的 cerashop.etc 不一定有
// 这一段，缺了就是“没有商品限购”）。
func parseCurrentCeraShopBuyRestrict(
	document *dnfpvf.Document,
) (map[uint32]currentCeraShopBuyRestrictRow, error) {
	rows := make(map[uint32]currentCeraShopBuyRestrictRow)
	if document == nil {
		return rows, nil
	}
	tokens, found := document.Section(currentCeraShopBuyRestrictSection)
	if !found {
		return rows, nil
	}
	values := currentCeraShopPVFValues(tokens)
	if len(values)%currentCeraShopBuyRestrictRowWidth != 0 {
		return nil, fmt.Errorf(
			"%w: section=%s tokens=%d row_width=%d",
			errCurrentCeraShopCatalogUnavailable, currentCeraShopBuyRestrictSection,
			len(values), currentCeraShopBuyRestrictRowWidth,
		)
	}
	for offset := 0; offset+currentCeraShopBuyRestrictRowWidth <= len(values); offset += currentCeraShopBuyRestrictRowWidth {
		index, indexOK := currentCeraShopPVFUint32(values[offset])
		commodityID, commodityOK := currentCeraShopPVFUint32(values[offset+1])
		unknown, unknownOK := currentCeraShopPVFUint32(values[offset+2])
		limit, limitOK := currentCeraShopPVFUint32(values[offset+3])
		if !indexOK || !commodityOK || !unknownOK || !limitOK {
			return nil, fmt.Errorf(
				"%w: section=%s row=%d has non-numeric fields",
				errCurrentCeraShopCatalogUnavailable, currentCeraShopBuyRestrictSection,
				offset/currentCeraShopBuyRestrictRowWidth,
			)
		}
		if commodityID == 0 {
			continue
		}
		if previous, exists := rows[commodityID]; exists && previous.CharacterLimit >= limit {
			continue
		}
		rows[commodityID] = currentCeraShopBuyRestrictRow{
			Index:          index,
			CommodityID:    commodityID,
			UnknownField:   unknown,
			CharacterLimit: limit,
		}
	}
	return rows, nil
}

// currentCeraShopBuyRestrictCharacterPolicy 把一行限购表变成角色维度的限购策略。
// 次数为 0（或缺失）表示该商品不限购。
func currentCeraShopBuyRestrictCharacterPolicy(row currentCeraShopBuyRestrictRow) (purchaselimit.Policy, bool) {
	if row.CommodityID == 0 || row.CharacterLimit == 0 {
		return purchaselimit.Policy{}, false
	}
	policy := purchaselimit.Policy{
		ItemID:  row.CommodityID,
		Scope:   purchaselimit.ScopeCharacter,
		Period:  purchaselimit.PeriodAccumulate,
		Limit:   row.CharacterLimit,
		Section: currentCeraShopBuyRestrictSection,
		KeyTag:  currentCeraShopBuyRestrictKeyTag,
	}
	if err := policy.Valid(); err != nil {
		return purchaselimit.Policy{}, false
	}
	return policy, true
}

// currentCeraShopExplainAccountLimitPattern 是账号维度限购文案的**保守**模板。
//
// 只认“每个帐号/账号”+“限购/仅限购买/只能购买/限购买”+阿拉伯数字+“个/次”。
// 中文数词（“限购买一次”）、没有数字、或主语是角色/每天的一律不认。
var currentCeraShopExplainAccountLimitPattern = regexp.MustCompile(
	`每个(?:帐号|账号)\s*(?:仅限购买|限购买|只能购买|限购|仅限)\s*([0-9]{1,4})\s*(?:个|次)`,
)

// currentCeraShopExplainAccountLimit 从道具说明文案里读出账号维度限购次数。
// 读不到返回 false：调用方按“不限购”处理。
func currentCeraShopExplainAccountLimit(explain string) (uint32, bool) {
	match := currentCeraShopExplainAccountLimitPattern.FindStringSubmatch(explain)
	if len(match) != 2 {
		return 0, false
	}
	value, err := strconv.ParseUint(strings.TrimSpace(match[1]), 10, 32)
	if err != nil || value == 0 {
		return 0, false
	}
	return uint32(value), true
}

// currentCeraShopExplainAccountPolicy 把道具说明文案里的账号限购变成策略。
func currentCeraShopExplainAccountPolicy(definition dungeonDropItemDefinition) (purchaselimit.Policy, bool) {
	if !currentCeraShopExplainAccountLimitEnabled {
		return purchaselimit.Policy{}, false
	}
	// 有机器可读的 `[purchase limit]` 时以字段为准，不去解析散文。
	if definition.PurchaseLimitFound {
		return purchaselimit.Policy{}, false
	}
	if definition.ItemID == 0 {
		return purchaselimit.Policy{}, false
	}
	limit, found := currentCeraShopExplainAccountLimit(definition.Explain)
	if !found {
		return purchaselimit.Policy{}, false
	}
	policy := purchaselimit.Policy{
		ItemID:  definition.ItemID,
		Scope:   purchaselimit.ScopeAccount,
		Period:  purchaselimit.PeriodAccumulate,
		Limit:   limit,
		Section: "explain[account]",
		KeyTag:  currentCeraShopExplainLimitKeyTag,
	}
	if err := policy.Valid(); err != nil {
		return purchaselimit.Policy{}, false
	}
	return policy, true
}

// currentCeraShopLimitedMarkerAccountLimit 是"限购商品"在没有 `[buy restrict item]`
// 行、也没有道具文案时的默认限购：**每个账号 1 次**。
//
// 判定条件 = `Marker == 101` 或 `IPGProductID != 0 && Marker != 99`，依据（2026-09-18）：
//   - 商品 104828（弹刃之绚宠物礼包）/104830（欢乐暑假行程单）：Marker=101 且带 IPG
//     商品号 70206/70207，商城里显示"账号限购1次"；104828 的 `[explain]` 写了"每个帐号
//     限购1个"，104830 的文案一个字都没提 —— 规则来自商品行数据而不是文案。
//   - 商品 102237（宠物胶囊 2683241）：Marker=0 但带 IPG 商品号 70052，商城显示"只能
//     购买一次"、大退之后又能买（运营反馈）—— 说明这一类"限购"跟 IPG 商品号有关。
//   - **契约类必须排除**：`[premium]` 段的 6 件契约（Marker=99）也带 IPG 商品号
//     （50009..50019），但玩家本来就要反复续费，给它们压"账号 1 次"会直接锁死续费。
//     真机上 IPG 非 0 的 9 件里，6 件是契约，另外 3 件正是上面那三件限购商品。
const currentCeraShopLimitedMarkerAccountLimit uint32 = 1

// currentCeraShopContractMarker 是契约类商品行的 Marker 值（真机实测）。
const currentCeraShopContractMarker uint32 = 99

// currentCeraShopMarkerAccountPolicy 把"限购商品"属性变成账号维度策略。
func currentCeraShopMarkerAccountPolicy(product currentCeraShopProduct) (purchaselimit.Policy, bool) {
	if product.CommodityID == 0 {
		return purchaselimit.Policy{}, false
	}
	limited := product.Marker == currentCeraShopProductMarkerLimited ||
		(product.IPGProductID != 0 && product.Marker != currentCeraShopContractMarker)
	if !limited {
		return purchaselimit.Policy{}, false
	}
	// 账号维度按**商品号**计数：同账号下不同商品各算一条，符合"每个帐号限购1个"的
	// 商品粒度（同一件道具可能挂多个商品：1个装/5个装）。
	policy := purchaselimit.Policy{
		ItemID:  product.CommodityID,
		Scope:   purchaselimit.ScopeAccount,
		Period:  purchaselimit.PeriodAccumulate,
		Limit:   currentCeraShopLimitedMarkerAccountLimit,
		Section: "cerashop_marker_limited",
		KeyTag:  currentCeraShopBuyRestrictKeyTag,
	}
	if err := policy.Valid(); err != nil {
		return purchaselimit.Policy{}, false
	}
	return policy, true
}
