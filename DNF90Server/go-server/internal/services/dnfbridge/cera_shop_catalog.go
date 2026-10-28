package dnfbridge

import (
	"fmt"
	"math"

	"longheng.io/server/internal/modules/dnf/purchaselimit"
	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
)

var currentCeraShopAvatarCouponItemIDs = map[int]map[uint32]int64{
	1: {1: 2681588, 2: 2681589, 3: 2681590},
	2: {1: 2681591, 2: 2681592, 3: 2681593},
	3: {3: 2681594},
}

// currentCeraShopProduct is a real runtime etc/cerashop.etc row.  The item
// ID, item count, and Cera price are never supplied by the client request.
type currentCeraShopProduct struct {
	CommodityID uint32
	ItemID      uint32
	Count       uint32
	CeraPrice   uint32
	// GoldPrice 是**九字段商品行**第 4 个 token 的金币标价（角色金币，不是点券）。
	//
	// 现场 2026-10-01：本服 `etc/cerashop.etc` 共 708 行普通商品，其中 **5 行是金币
	// 标价且 Cera 列写 0** —— 104824 热舞一夏称号随机变更券(50万)、104820 热舞一夏
	// 称号宝珠(50万)、104822 迷你染色剂礼盒(20万)、103155 幸运魔锤(80万)、
	// 103061 晶之契约(7天)(150万)。改造前这里的解析器**只读 Cera 列**，于是这 5 件
	// 商品在服务端眼里标价一律是 0：买得到、任何钱包都不扣 —— 就是"商城购买金币标价
	// 道具不扣金币"的现场（角色 9999 / commodity 104824 / 2026-10-01 16:44:15）。
	GoldPrice uint32
	Section   string
	// DurationDays is the runtime Cera-shop row's relative lifetime. The
	// active `visual` grammar stores it at token four (for example 30 in a
	// `(30天)` name-decoration product); it is independent of item-PVF
	// [usable period] and must be materialized once at checkout.
	DurationDays           uint32
	AvatarDurationIndex    uint32
	AvatarDurationDays     uint32
	AvatarPriceFromItemPVF bool
	// PremiumPackageDays is the [charac premium package] row's day count
	// (token 7). Zero for every non-package section.
	PremiumPackageDays uint32
	// Marker 是九字段商品行的倒数第二个字段（`[item]`/`[material]`/`[premium]`…）。
	// 真机实测取值：0/2 = 普通商品，99 = 契约类，101 = 限购商品。
	Marker uint32
	// IPGProductID 是九字段商品行的最后一个字段：非 0 = 走 IPG（官方商城后台）的商品号。
	// 真机实测只有 3 个商品非 0：102237（宠物胶囊 70052）、104828（弹刃之绚宠物礼包
	// 70206）、104830（欢乐暑假行程单 70207）——这三件在商城里都显示"限购 1 次"，
	// 因此本服按"每个账号 1 次"处理（见 cera_shop_buy_restrict.go 的说明）。
	IPGProductID uint32
}

// currentCeraShopProductMarkerIndex 是九字段商品行里 Marker 的下标（0 基）。
const currentCeraShopProductMarkerIndex = 7

// currentCeraShopProductGoldIndex 是**九字段商品行**里金币标价的下标（0 基）。
const currentCeraShopProductGoldIndex = 3

// currentCeraShopProductNineTokenRowWidth 是带金币列的普通商品行的固定宽度。
//
// ⚠ 读金币列**必须**卡死 `rowWidth == 9`，不能写成 `rowWidth >= 4` 之类的宽松判断：
//
//   - `[visual]` 是 **8** 字段行，它的 token3 是**时长天数**（DurationDays，现场值 30），
//     Cera 在 token5。放宽判断会把"买 30 天时装扣 60 点券"变成"扣 30 金币"；
//   - `[package]` 系列是 **11** 字段行、Cera 在 token4，token3 是别的含义；
//   - `[avatar]` 是 **6** 字段行，价格来自道具 PVF 的 `[avatar type select]`。
//
// 这正是"修 A 坏 B"最容易发生的一处，所以用它做显式门槛。
const currentCeraShopProductNineTokenRowWidth = 9

// currentCeraShopProductIPGIndex 是九字段商品行里 IPG 商品号的下标（0 基）。
const currentCeraShopProductIPGIndex = 8

// currentCeraShopProductMarkerLimited 是"限购商品"的标记值（真机实测）。
const currentCeraShopProductMarkerLimited uint32 = 101

// pvfCeraShopCatalog owns only static runtime PVF state.  Inventory placement
// and Cera deduction are handled within the character settlement transaction.
type pvfCeraShopCatalog struct {
	products         map[uint32]currentCeraShopProduct
	bonusByCommodity map[uint32][]currentCeraShopBonusItem
	items            *pvfDungeonDropCatalog
	source           dnfpvf.Source
	// buyRestrict 是 `[buy restrict item]` 的角色维度限购表（按商品号）。
	// 商城道具在道具 PVF 里没有 `[purchase limit]`，限购只写在这张表里。
	buyRestrict map[uint32]currentCeraShopBuyRestrictRow
	// buyCountGift 是"多买多送"（第二代）的事件计划。它只被赋值一次、之后只读，
	// 所以在登录/购买路径上并发读取是安全的。从不为 nil（未配置时是 disabled 计划）。
	buyCountGift *currentCeraShopBuyCountGiftPlan
	// buyCountGifts contains the primary campaign first, followed by optional
	// independent campaigns discovered from chn_buycountgiftsecond_*.evt.
	buyCountGifts []*currentCeraShopBuyCountGiftPlan
}

func newPVFCeraShopCatalog(source dnfpvf.Source) (*pvfCeraShopCatalog, error) {
	if source == nil {
		return nil, errCurrentCeraShopCatalogUnavailable
	}
	document, err := parseDungeonCardPVFDocument(source, "etc/cerashop.etc")
	if err != nil {
		return nil, fmt.Errorf("read cera shop catalog: %w", err)
	}
	items, err := newPVFDungeonDropCatalog(source)
	if err != nil {
		return nil, fmt.Errorf("load cera shop item resolver: %w", err)
	}
	products := make(map[uint32]currentCeraShopProduct)
	// The normal purchase categories share a 9-token row: commodity, item,
	// amount, gold, point, Cera, name, 0, 0.  The catalog deliberately owns
	// every runtime category whose row grammar is known; item placement is
	// still decided later from the real item PVF type, not the client request.
	for _, section := range []string{"item", "premium", "creature", "coin", "material", "recoveryitem"} {
		if err := parseCurrentCeraShopProductSection(document, section, 9, 5, true, products); err != nil {
			return nil, err
		}
	}
	// These category names occur in different runtime PVFs.  They have the
	// ordinary nine-token product grammar and are optional by PVF version.
	for _, section := range []string{"community item", "communityitem", "regular item", "regularitem"} {
		if err := parseCurrentCeraShopProductSection(document, section, 9, 5, false, products); err != nil {
			return nil, err
		}
	}
	// visual rows are eight tokens (the fourth token is duration metadata),
	// while the three package catalogs use the Cera price at token four.
	if err := parseCurrentCeraShopProductSection(document, "visual", 8, 5, false, products); err != nil {
		return nil, err
	}
	for _, section := range []string{"package", "regular package", "regularpackage", "community package", "communitypackage"} {
		if err := parseCurrentCeraShopProductSection(document, section, 11, 4, false, products); err != nil {
			return nil, err
		}
	}
	// Contract products are catalogued too, including their distinct row
	// grammars.  They are never treated as ordinary bag items at settlement:
	// checkout activates their account-level premium contract state instead.
	if err := parseCurrentCeraShopSelectableCharacterPremiumSection(document, products); err != nil {
		return nil, err
	}
	if err := parseCurrentCeraShopCharacterPremiumPackageSection(document, products); err != nil {
		return nil, err
	}
	// Avatar rows are six tokens.  A negative Cera field means the price and
	// duration are selected from the purchased equipment's [avatar type
	// select] row; defer that real-PVF lookup to the actual purchase instead
	// of preloading every avatar definition at login.
	if err := parseCurrentCeraShopAvatarSection(document, products); err != nil {
		return nil, err
	}
	// One-plus-one IPG gifts (etc/chn_cerashop_multibonus.etc) ride along
	// with the catalog so checkout can grant them in the same transaction.
	bonusByCommodity, err := parseCurrentCeraShopMultiBonus(source)
	if err != nil {
		return nil, err
	}
	// 商城自己的限购表（角色维度）。商城道具在道具 PVF 里没有 `[purchase limit]`，
	// 所以没有这张表就完全拦不住“每个角色限购N次”。
	buyRestrict, err := parseCurrentCeraShopBuyRestrict(document)
	if err != nil {
		return nil, err
	}
	// 多买多送（第二代）事件表。解析失败**不阻断**目录构造：这张表是可选活动配置，
	// 缺文件/缺段落只应让该活动安静地不启用（原因记在计划里，由日志暴露）。
	buyCountGifts := parseCurrentCeraShopBuyCountGiftPlans(source)
	buyCountGift := &currentCeraShopBuyCountGiftPlan{disabledReason: "catalog unavailable"}
	if len(buyCountGifts) > 0 {
		buyCountGift = buyCountGifts[0]
	}
	return &pvfCeraShopCatalog{
		products:         products,
		bonusByCommodity: bonusByCommodity,
		items:            items,
		source:           source,
		buyRestrict:      buyRestrict,
		buyCountGift:     buyCountGift,
		buyCountGifts:    buyCountGifts,
	}, nil
}

// BuyCountGiftPlan 返回多买多送事件计划（永不为 nil）。
func (c *pvfCeraShopCatalog) BuyCountGiftPlan() *currentCeraShopBuyCountGiftPlan {
	if c == nil || c.buyCountGift == nil {
		return &currentCeraShopBuyCountGiftPlan{disabledReason: "catalog unavailable"}
	}
	return c.buyCountGift
}

// BuyCountGiftPlans returns every parsed campaign with the primary first.
// Disabled extra campaigns remain in this slice for diagnostics but are never
// counted or rewarded.
func (c *pvfCeraShopCatalog) BuyCountGiftPlans() []*currentCeraShopBuyCountGiftPlan {
	if c == nil || len(c.buyCountGifts) == 0 {
		return []*currentCeraShopBuyCountGiftPlan{
			{disabledReason: "catalog unavailable"},
		}
	}
	return append([]*currentCeraShopBuyCountGiftPlan(nil), c.buyCountGifts...)
}

// BuyCountGiftEligible 判断某个商品号是否计入多买多送。
//
// 除了 `[bonus event ipg]` 直列的商品号，还要接受**折扣券镜像号**：
// 玩家选用商城折扣券时，客户端会上报 `基础商品号 + 1000`（例 104278 -> 105278）——
// 这是 86JP 参考实现 `CeraShopPurchaseBonusCatalog.IsEligibleProduct` 明确写下的行为；
// 不认镜像就会出现"用折扣券买同一件礼包却不计数"的静默 bug。
//
// 接受镜像的**充分条件**是两行商品发的是同一件道具，否则恰好偏移 1000 的无关商品
// 也能混进活动。参考实现也是这么卡的。
func (c *pvfCeraShopCatalog) BuyCountGiftEligible(commodityID uint32) bool {
	if c == nil || commodityID == 0 {
		return false
	}
	for _, plan := range c.BuyCountGiftPlans() {
		if c.buyCountGiftEligibleForPlan(plan, commodityID) {
			return true
		}
	}
	return false
}

func (c *pvfCeraShopCatalog) buyCountGiftEligibleForPlan(
	plan *currentCeraShopBuyCountGiftPlan,
	commodityID uint32,
) bool {
	if c == nil || commodityID == 0 || plan == nil {
		return false
	}
	if !plan.Enabled() {
		return false
	}
	if plan.CountsTowardGift(commodityID) {
		return true
	}
	if commodityID <= currentCeraShopBuyCountGiftDiscountProductIDOffset {
		return false
	}
	baseCommodityID := commodityID - currentCeraShopBuyCountGiftDiscountProductIDOffset
	if !plan.CountsTowardGift(baseCommodityID) {
		return false
	}
	baseProduct, baseFound := c.products[baseCommodityID]
	discountProduct, discountFound := c.products[commodityID]
	if !baseFound || !discountFound || baseProduct.ItemID == 0 {
		return false
	}
	return baseProduct.ItemID == discountProduct.ItemID
}

// BuyRestrictCharacterPolicy 返回某个商品在 `[buy restrict item]` 里的角色维度
// 限购策略；该商品不在表里（或次数为 0）时返回 false。
func (c *pvfCeraShopCatalog) BuyRestrictCharacterPolicy(commodityID uint32) (purchaselimit.Policy, bool) {
	if c == nil || commodityID == 0 {
		return purchaselimit.Policy{}, false
	}
	row, found := c.buyRestrict[commodityID]
	if !found {
		return purchaselimit.Policy{}, false
	}
	return currentCeraShopBuyRestrictCharacterPolicy(row)
}

// BuyRestrictRows 返回整张限购表（审核日志用）。
func (c *pvfCeraShopCatalog) BuyRestrictRows() map[uint32]currentCeraShopBuyRestrictRow {
	if c == nil {
		return nil
	}
	return c.buyRestrict
}

// BuyRestrictPolicies 返回某商品**生效**的限购策略，按优先级取第一条命中的来源：
//
//  1. `[buy restrict item]` 的角色维度次数（商城自己的机器可读表；客户端工具提示
//     “每个角色只能购买N次”就是这一列）；
//  2. 道具 `[explain]` 文案里的账号限购（例如 490702494“每个帐号限购1个。”）；
//  3. 商品行 Marker==101（限购商品）但前两条都没有时的兜底：每个账号 1 次。
//
// 三条**互斥**而不是叠加：6 件既有角色限购又在表外的限购商品（101081 等）在真机上
// 只显示“每个角色只能购买1次”，如果同时再压一条“账号 1 次”，就会变成整号只能买一次，
// 与客户端显示和玩家预期都不符。
func (c *pvfCeraShopCatalog) BuyRestrictPolicies(commodityID uint32) []purchaselimit.Policy {
	if c == nil || commodityID == 0 {
		return nil
	}
	product, found := c.products[commodityID]
	if !found {
		return nil
	}
	if policy, ok := c.BuyRestrictCharacterPolicy(commodityID); ok {
		return []purchaselimit.Policy{policy}
	}
	if c.items != nil && product.ItemID != 0 {
		if definition, err := c.items.ResolveItem(product.ItemID); err == nil {
			if policy, ok := currentCeraShopExplainAccountPolicy(definition); ok {
				return []purchaselimit.Policy{policy}
			}
		}
	}
	if policy, ok := currentCeraShopMarkerAccountPolicy(product); ok {
		return []purchaselimit.Policy{policy}
	}
	return nil
}

func parseCurrentCeraShopProductSection(
	document *dnfpvf.Document,
	section string,
	rowWidth int,
	ceraPriceOffset int,
	required bool,
	products map[uint32]currentCeraShopProduct,
) error {
	if document == nil || products == nil || rowWidth < 3 || ceraPriceOffset < 0 || ceraPriceOffset >= rowWidth {
		return errCurrentCeraShopCatalogUnavailable
	}
	tokens, found := document.Section(section)
	if !found {
		if required {
			return fmt.Errorf("%w: missing section=%s", errCurrentCeraShopCatalogUnavailable, section)
		}
		return nil
	}
	values := currentCeraShopPVFValues(tokens)
	if len(values)%rowWidth != 0 {
		return fmt.Errorf("%w: section=%s tokens=%d row_width=%d", errCurrentCeraShopCatalogUnavailable, section, len(values), rowWidth)
	}
	for offset := 0; offset < len(values); offset += rowWidth {
		commodityID, ok := currentCeraShopPVFUint32(values[offset])
		if !ok || commodityID == 0 {
			return fmt.Errorf("%w: section=%s row=%d invalid_commodity", errCurrentCeraShopCatalogUnavailable, section, offset/rowWidth)
		}
		itemID, ok := currentCeraShopPVFUint32(values[offset+1])
		if !ok || itemID == 0 {
			return fmt.Errorf("%w: section=%s commodity=%d invalid_item", errCurrentCeraShopCatalogUnavailable, section, commodityID)
		}
		count, ok := currentCeraShopPVFUint32(values[offset+2])
		// Some visual/package rows use a negative third token as a client
		// display sentinel.  It is never a requested quantity, so normalize it
		// to one real delivery item just as zero-count package rows are.
		if !ok {
			count = 1
		}
		if count == 0 {
			count = 1
		}
		ceraPrice, ok := currentCeraShopPVFUint32(values[offset+ceraPriceOffset])
		if !ok {
			return fmt.Errorf("%w: section=%s commodity=%d invalid_cera_price", errCurrentCeraShopCatalogUnavailable, section, commodityID)
		}
		product := currentCeraShopProduct{
			CommodityID: commodityID,
			ItemID:      itemID,
			Count:       count,
			CeraPrice:   ceraPrice,
			Section:     section,
		}
		// 九字段行的第 4 个字段是**金币标价**。只有 rowWidth 正好是 9 的行才是这套
		// 语法 —— 见 currentCeraShopProductNineTokenRowWidth 的说明，`[visual]` 的
		// token3 是时长天数，放宽判断会把它读成金币。
		if rowWidth == currentCeraShopProductNineTokenRowWidth {
			if goldPrice, goldOK := currentCeraShopPVFUint32(values[offset+currentCeraShopProductGoldIndex]); goldOK {
				product.GoldPrice = goldPrice
			}
		}
		// 九字段行（item/material/premium 等）的倒数第二个字段是**限购标记**：
		// 0/2 = 普通商品，99 = 契约类，101 = 限购商品。实证（真机 Script.pvf）：
		// 有 101 的商品集合 = `[buy restrict item]` 的角色限购商品，外加
		// 104828/104830 这两件（它们的限购只写在商城 UI 上，见 cera_shop_buy_restrict.go）。
		if rowWidth >= currentCeraShopProductMarkerIndex+1 {
			if marker, markerOK := currentCeraShopPVFUint32(values[offset+currentCeraShopProductMarkerIndex]); markerOK {
				product.Marker = marker
			}
		}
		// 九字段行的**最后一个**字段 = IPG 商品号（非 0 表示这是接官方商城后台的商品）。
		// 现场实证：102237（宠物胶囊 70052）、104828（70206）、104830（70207）三件在
		// 商城里都显示"限购 1 次"，所以本服按每个账号 1 次处理。
		if rowWidth >= currentCeraShopProductIPGIndex+1 {
			if ipg, ipgOK := currentCeraShopPVFUint32(values[offset+currentCeraShopProductIPGIndex]); ipgOK {
				product.IPGProductID = ipg
			}
		}
		if normalizeMagicBoxSectionName(section) == "visual" {
			durationDays, durationOK := currentCeraShopPVFUint32(values[offset+3])
			if !durationOK {
				return fmt.Errorf("%w: section=%s commodity=%d invalid_duration", errCurrentCeraShopCatalogUnavailable, section, commodityID)
			}
			product.DurationDays = durationDays
		}
		products[commodityID] = product
	}
	return nil
}

func parseCurrentCeraShopAvatarSection(document *dnfpvf.Document, products map[uint32]currentCeraShopProduct) error {
	if document == nil || products == nil {
		return errCurrentCeraShopCatalogUnavailable
	}
	tokens, found := document.Section("avatar")
	if !found {
		return nil
	}
	values := currentCeraShopPVFValues(tokens)
	const rowWidth = 6
	if len(values)%rowWidth != 0 {
		return fmt.Errorf("%w: section=avatar tokens=%d row_width=%d", errCurrentCeraShopCatalogUnavailable, len(values), rowWidth)
	}
	for offset := 0; offset < len(values); offset += rowWidth {
		commodityID, ok := currentCeraShopPVFUint32(values[offset])
		if !ok || commodityID == 0 {
			return fmt.Errorf("%w: section=avatar row=%d invalid_commodity", errCurrentCeraShopCatalogUnavailable, offset/rowWidth)
		}
		itemID, ok := currentCeraShopPVFUint32(values[offset+1])
		if !ok || itemID == 0 {
			return fmt.Errorf("%w: section=avatar commodity=%d invalid_item", errCurrentCeraShopCatalogUnavailable, commodityID)
		}
		durationIndex, ok := currentCeraShopPVFUint32(values[offset+2])
		if !ok || durationIndex == 0 {
			durationIndex = 1
		}
		product := currentCeraShopProduct{
			CommodityID:         commodityID,
			ItemID:              itemID,
			Count:               1,
			Section:             "avatar",
			AvatarDurationIndex: durationIndex,
		}
		if ceraPrice, ok := currentCeraShopPVFUint32(values[offset+5]); ok {
			product.CeraPrice = ceraPrice
		} else {
			product.AvatarPriceFromItemPVF = true
		}
		products[commodityID] = product
	}
	return nil
}

// [charac premium package] does not use the normal package row: its third
// value is Cera, followed by its localized duration label and activation
// metadata (token 7 = days). Buying it is consumed at checkout and activates
// every 魔王契约 devil slot for that duration; no container item is granted.
func parseCurrentCeraShopCharacterPremiumPackageSection(document *dnfpvf.Document, products map[uint32]currentCeraShopProduct) error {
	if document == nil || products == nil {
		return errCurrentCeraShopCatalogUnavailable
	}
	tokens, found := document.Section("charac premium package")
	if !found {
		return nil
	}
	values := currentCeraShopPVFValues(tokens)
	const rowWidth = 9
	if len(values)%rowWidth != 0 {
		return fmt.Errorf("%w: section=charac premium package tokens=%d row_width=%d", errCurrentCeraShopCatalogUnavailable, len(values), rowWidth)
	}
	for offset := 0; offset < len(values); offset += rowWidth {
		commodityID, ok := currentCeraShopPVFUint32(values[offset])
		if !ok || commodityID == 0 {
			return fmt.Errorf("%w: section=charac premium package row=%d invalid_commodity", errCurrentCeraShopCatalogUnavailable, offset/rowWidth)
		}
		itemID, ok := currentCeraShopPVFUint32(values[offset+1])
		if !ok || itemID == 0 {
			return fmt.Errorf("%w: section=charac premium package commodity=%d invalid_item", errCurrentCeraShopCatalogUnavailable, commodityID)
		}
		ceraPrice, ok := currentCeraShopPVFUint32(values[offset+2])
		if !ok {
			return fmt.Errorf("%w: section=charac premium package commodity=%d invalid_cera_price", errCurrentCeraShopCatalogUnavailable, commodityID)
		}
		// Token 7 is the contract day count (the label at token 3 repeats it,
		// e.g. `60天` <-> 60). A package without a positive day count is not
		// purchasable.
		packageDays, _ := currentCeraShopPVFUint32(values[offset+7])
		products[commodityID] = currentCeraShopProduct{
			CommodityID:        commodityID,
			ItemID:             itemID,
			Count:              1,
			CeraPrice:          ceraPrice,
			Section:            "charac premium package",
			PremiumPackageDays: packageDays,
		}
	}
	return nil
}

// [selectable character premium] is nominally a nine-token product section,
// but the first row in the runtime PVF is a -1 default selector rather than a
// purchasable commodity.  Preserve the structural check and ignore only such
// explicitly non-purchasable rows.
func parseCurrentCeraShopSelectableCharacterPremiumSection(document *dnfpvf.Document, products map[uint32]currentCeraShopProduct) error {
	if document == nil || products == nil {
		return errCurrentCeraShopCatalogUnavailable
	}
	tokens, found := document.Section("selectable character premium")
	if !found {
		return nil
	}
	values := currentCeraShopPVFValues(tokens)
	const rowWidth = 9
	if len(values)%rowWidth != 0 {
		return fmt.Errorf("%w: section=selectable character premium tokens=%d row_width=%d", errCurrentCeraShopCatalogUnavailable, len(values), rowWidth)
	}
	for offset := 0; offset < len(values); offset += rowWidth {
		commodityID, commodityOK := currentCeraShopPVFUint32(values[offset])
		itemID, itemOK := currentCeraShopPVFUint32(values[offset+1])
		if !commodityOK || commodityID == 0 || !itemOK || itemID == 0 {
			continue
		}
		count, ok := currentCeraShopPVFUint32(values[offset+2])
		if !ok || count == 0 {
			count = 1
		}
		ceraPrice, ok := currentCeraShopPVFUint32(values[offset+5])
		if !ok {
			continue
		}
		products[commodityID] = currentCeraShopProduct{
			CommodityID: commodityID,
			ItemID:      itemID,
			Count:       count,
			CeraPrice:   ceraPrice,
			Section:     "selectable character premium",
		}
	}
	return nil
}

func currentCeraShopPVFValues(tokens []dnfpvf.Token) []dnfpvf.Token {
	values := make([]dnfpvf.Token, 0, len(tokens))
	for _, token := range tokens {
		switch token.Kind {
		case dnfpvf.TokenInt, dnfpvf.TokenString, dnfpvf.TokenIdent:
			values = append(values, token)
		}
	}
	return values
}

func currentCeraShopPVFUint32(token dnfpvf.Token) (uint32, bool) {
	if token.Kind != dnfpvf.TokenInt || token.Int < 0 || token.Int > math.MaxUint32 {
		return 0, false
	}
	return uint32(token.Int), true
}

func (c *pvfCeraShopCatalog) Product(commodityID uint32) (currentCeraShopProduct, bool) {
	if c == nil {
		return currentCeraShopProduct{}, false
	}
	product, found := c.products[commodityID]
	return product, found
}

func (c *pvfCeraShopCatalog) resolvePurchase(product currentCeraShopProduct) (currentCeraShopProduct, error) {
	if c == nil || c.items == nil || c.source == nil || product.ItemID == 0 {
		return currentCeraShopProduct{}, errCurrentCeraShopCatalogUnavailable
	}
	if product.Section != "avatar" {
		// Contract sections ([selectable character premium], [charac premium
		// package]) and premiumlist contract items now activate their real
		// account-level state at settlement; they pass through here and are
		// classified by commitCurrentCeraShopPurchase.
		return product, nil
	}
	definition, err := c.items.ResolveItem(product.ItemID)
	if err != nil {
		return currentCeraShopProduct{}, fmt.Errorf("%w: avatar item=%d: %v", errCurrentCeraShopProductUnavailable, product.ItemID, err)
	}
	if definition.Kind != dungeonDropItemEquipment {
		return currentCeraShopProduct{}, fmt.Errorf("%w: avatar item=%d is not equipment", errCurrentCeraShopProductUnavailable, product.ItemID)
	}
	document, err := parseDungeonCardPVFDocument(c.source, definition.PVFPath)
	if err != nil {
		return currentCeraShopProduct{}, fmt.Errorf("%w: avatar item=%d PVF=%s: %v", errCurrentCeraShopProductUnavailable, product.ItemID, definition.PVFPath, err)
	}
	equipmentType, found := document.Text("equipment type")
	if !found {
		return currentCeraShopProduct{}, fmt.Errorf("%w: avatar item=%d missing equipment type", errCurrentCeraShopProductUnavailable, product.ItemID)
	}
	rule, ok := currentEquipmentPlacementRuleForPVFType(equipmentType)
	if !ok || rule.class != currentEquipmentPlacementClassAvatar {
		return currentCeraShopProduct{}, fmt.Errorf("%w: item=%d type=%q is not avatar equipment", errCurrentCeraShopProductUnavailable, product.ItemID, equipmentType)
	}
	if durationDays, ceraPrice, found := currentCeraShopAvatarTypeOption(document, product.AvatarDurationIndex); found {
		product.AvatarPriceFromItemPVF = false
		product.CeraPrice = ceraPrice
		product.Count = 1
		product.AvatarDurationDays = durationDays
		return product, nil
	}
	if product.AvatarPriceFromItemPVF {
		return currentCeraShopProduct{}, fmt.Errorf("%w: avatar item=%d duration_index=%d has no runtime price", errCurrentCeraShopProductUnavailable, product.ItemID, product.AvatarDurationIndex)
	}
	return product, nil
}

func (c *pvfCeraShopCatalog) resolveAvatarCouponItemID(product currentCeraShopProduct) (int64, error) {
	if c == nil || c.items == nil || c.source == nil || product.ItemID == 0 {
		return 0, errCurrentCeraShopCatalogUnavailable
	}
	if product.Section != "avatar" {
		return 0, fmt.Errorf("%w: commodity=%d payment_mode=avatar_coupon section=%s", errCurrentCeraShopPaymentUnsupported, product.CommodityID, product.Section)
	}
	definition, err := c.items.ResolveItem(product.ItemID)
	if err != nil {
		return 0, fmt.Errorf("%w: avatar coupon item=%d: %v", errCurrentCeraShopProductUnavailable, product.ItemID, err)
	}
	if definition.Kind != dungeonDropItemEquipment {
		return 0, fmt.Errorf("%w: avatar coupon item=%d is not equipment", errCurrentCeraShopProductUnavailable, product.ItemID)
	}
	document, err := parseDungeonCardPVFDocument(c.source, definition.PVFPath)
	if err != nil {
		return 0, fmt.Errorf("%w: avatar coupon item=%d PVF=%s: %v", errCurrentCeraShopProductUnavailable, product.ItemID, definition.PVFPath, err)
	}
	grade, found := document.Int("grade")
	if !found || grade < 1 || grade > math.MaxInt32 {
		return 0, fmt.Errorf("%w: avatar item=%d missing supported grade", errCurrentCeraShopProductUnavailable, product.ItemID)
	}
	durationIndex := product.AvatarDurationIndex
	if durationIndex == 0 {
		durationIndex = product.Count
	}
	couponID, ok := currentCeraShopAvatarCouponItemIDs[int(grade)][durationIndex]
	if !ok {
		return 0, fmt.Errorf("%w: avatar item=%d grade=%d duration_index=%d has no coupon", errCurrentCeraShopPaymentUnsupported, product.ItemID, grade, durationIndex)
	}
	return couponID, nil
}

// currentCeraShopAvatarTypeOption reads the real equipment [avatar type
// select] grammar: duration, ignored, ignored, Cera, ignored, ignored,
// ignored.  The shop's third avatar field is a one-based duration selector.
func currentCeraShopAvatarTypeOption(document *dnfpvf.Document, index uint32) (uint32, uint32, bool) {
	if document == nil || index == 0 {
		return 0, 0, false
	}
	values := document.Ints("avatar type select")
	const rowWidth = 7
	offset := int(index-1) * rowWidth
	if offset < 0 || offset+3 >= len(values) || values[offset] < 0 || values[offset] > math.MaxUint32 || values[offset+3] < 0 || values[offset+3] > math.MaxUint32 {
		return 0, 0, false
	}
	return uint32(values[offset]), uint32(values[offset+3]), true
}

func (s *Service) currentCeraShopCatalog() (*pvfCeraShopCatalog, error) {
	if s == nil {
		return nil, errCurrentCeraShopCatalogUnavailable
	}
	s.ceraShopMu.Lock()
	defer s.ceraShopMu.Unlock()
	if s.ceraShopCatalog != nil {
		return s.ceraShopCatalog, nil
	}
	if s.ceraShopCatalogLoadErr != nil {
		return nil, s.ceraShopCatalogLoadErr
	}
	s.initialEquipmentMu.Lock()
	archive, err := s.initialEquipmentArchiveLocked()
	s.initialEquipmentMu.Unlock()
	if err == nil {
		s.ceraShopCatalog, err = newPVFCeraShopCatalog(archive)
	}
	if err != nil {
		s.ceraShopCatalogLoadErr = err
		return nil, err
	}
	s.logPacketEvent("dnf-cera-shop-pvf-catalog-loaded",
		"products", len(s.ceraShopCatalog.products),
		"buy_count_gift_groups", len(s.ceraShopCatalog.BuyCountGiftPlans()),
		// 商城限购表：0 行意味着商城完全拦不住“每个角色限购N次”的商品。
		"buy_restrict_rows", len(s.ceraShopCatalog.buyRestrict))
	return s.ceraShopCatalog, nil
}
