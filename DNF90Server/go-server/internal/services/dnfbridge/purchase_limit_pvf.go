// 本文件解析 PVF 的限购字段。两套字段都在这里认：
//
//	[purchase limit]        `account` `weekly` 10      → Scope + Period + Limit
//	[daily purchase limit]  `character` 1              → Scope + Limit，周期=每天
//
// 新字段存在时优先（同一件道具同时写两套时以新字段为准），旧字段只作为回退。
package dnfbridge

import (
	"fmt"
	"strings"

	"longheng.io/server/internal/modules/dnf/purchaselimit"
	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
)

const (
	currentPurchaseLimitSection      = "purchase limit"
	currentDailyPurchaseLimitSection = "daily purchase limit"
	// currentPurchaseLimitUnknownTradeScope 是旧字段里实测到的第三种取值
	// （`trade`，见 chn_thief_chnagejob_10006348.stk 等 4 个文件）。语义不明，
	// 按“不认识的字段”处理：不限购，也不猜它属于哪个作用域。
	currentPurchaseLimitUnknownTradeScope = "trade"
)

// parseCurrentPurchaseLimitSection 返回道具的限购策略。第二个返回值报告是否
// 存在可用的限购字段。字段存在但形状不认识时返回错误，调用方保留“无策略”
// 以免把整件道具判为不可解析。
func parseCurrentPurchaseLimitSection(itemID uint32, document *dnfpvf.Document) (purchaselimit.Policy, bool, error) {
	if document == nil {
		return purchaselimit.Policy{}, false, nil
	}
	if tokens, ok := document.Section(currentPurchaseLimitSection); ok {
		policy, err := currentPurchaseLimitFromTokens(itemID, currentPurchaseLimitSection, tokens)
		if err != nil {
			return purchaselimit.Policy{}, false, err
		}
		return policy, true, nil
	}
	if tokens, ok := document.Section(currentDailyPurchaseLimitSection); ok {
		policy, err := currentDailyPurchaseLimitFromTokens(itemID, tokens)
		if err != nil {
			return purchaselimit.Policy{}, false, err
		}
		return policy, true, nil
	}
	return purchaselimit.Policy{}, false, nil
}

// currentPurchaseLimitFromTokens 读新格式三元组：作用域、周期、次数。
func currentPurchaseLimitFromTokens(itemID uint32, section string, tokens []dnfpvf.Token) (purchaselimit.Policy, error) {
	var (
		scopeText  string
		periodText string
		limit      int64
		haveLimit  bool
	)
	for _, token := range tokens {
		switch token.Kind {
		case dnfpvf.TokenString, dnfpvf.TokenIdent:
			text := currentPurchaseLimitTokenText(token)
			if text == "" {
				continue
			}
			if scopeText == "" {
				scopeText = text
				continue
			}
			if periodText == "" {
				periodText = text
				continue
			}
		case dnfpvf.TokenInt:
			if !haveLimit {
				limit = token.Int
				haveLimit = true
			}
		}
	}
	if scopeText == "" || periodText == "" || !haveLimit {
		return purchaselimit.Policy{}, fmt.Errorf(
			"%w: item=%d section=%q scope=%q period=%q limit=%d",
			purchaselimit.ErrPolicyInvalid, itemID, section, scopeText, periodText, limit)
	}
	scope, ok := purchaselimit.ParseScope(scopeText)
	if !ok {
		return purchaselimit.Policy{}, fmt.Errorf("%w: item=%d section=%q scope=%q", purchaselimit.ErrPolicyInvalid, itemID, section, scopeText)
	}
	period, ok := purchaselimit.ParsePeriod(periodText)
	if !ok {
		return purchaselimit.Policy{}, fmt.Errorf("%w: item=%d section=%q period=%q", purchaselimit.ErrPolicyInvalid, itemID, section, periodText)
	}
	if limit <= 0 {
		return purchaselimit.Policy{}, fmt.Errorf("%w: item=%d section=%q limit=%d", purchaselimit.ErrPolicyInvalid, itemID, section, limit)
	}
	if limit > int64(^uint32(0)) {
		limit = int64(^uint32(0))
	}
	policy := purchaselimit.Policy{
		ItemID:  itemID,
		Scope:   scope,
		Period:  period,
		Limit:   uint32(limit),
		Section: section,
	}
	return policy, policy.Valid()
}

// currentDailyPurchaseLimitFromTokens 读旧格式二元组：作用域、次数；周期固定
// 为每天。
func currentDailyPurchaseLimitFromTokens(itemID uint32, tokens []dnfpvf.Token) (purchaselimit.Policy, error) {
	var (
		scopeText string
		limit     int64
		haveLimit bool
	)
	for _, token := range tokens {
		switch token.Kind {
		case dnfpvf.TokenString, dnfpvf.TokenIdent:
			if text := currentPurchaseLimitTokenText(token); text != "" && scopeText == "" {
				scopeText = text
			}
		case dnfpvf.TokenInt:
			if !haveLimit {
				limit = token.Int
				haveLimit = true
			}
		}
	}
	if scopeText == "" || !haveLimit || limit <= 0 {
		return purchaselimit.Policy{}, fmt.Errorf(
			"%w: item=%d section=%q scope=%q limit=%d",
			purchaselimit.ErrPolicyInvalid, itemID, currentDailyPurchaseLimitSection, scopeText, limit)
	}
	scope, ok := purchaselimit.ParseScope(scopeText)
	if !ok {
		return purchaselimit.Policy{}, fmt.Errorf("%w: item=%d section=%q scope=%q", purchaselimit.ErrPolicyInvalid, itemID, currentDailyPurchaseLimitSection, scopeText)
	}
	if limit > int64(^uint32(0)) {
		limit = int64(^uint32(0))
	}
	policy := purchaselimit.Policy{
		ItemID:  itemID,
		Scope:   scope,
		Period:  purchaselimit.PeriodDaily,
		Limit:   uint32(limit),
		Section: currentDailyPurchaseLimitSection,
	}
	return policy, policy.Valid()
}

// currentPurchaseLimitTokenText 取 token 的文本值：反引号字符串用 Value，裸标
// 识符用 Raw。
func currentPurchaseLimitTokenText(token dnfpvf.Token) string {
	if value := strings.TrimSpace(token.Value); value != "" {
		return value
	}
	return strings.TrimSpace(token.Raw)
}
