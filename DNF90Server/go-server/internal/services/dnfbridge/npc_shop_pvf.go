package dnfbridge

import (
	"errors"
	"fmt"
	"math"
	"strings"

	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

const (
	currentNPCShopItemListPath   = "itemshop/itemshop.lst"
	currentNPCShopPriceTablePath = "equipment/pricetable.tbl"
	currentNPCTradingSystemPath  = "etc/tradingsystem.etc"
)

var (
	errCurrentNPCShopCatalogUnavailable = errors.New("dnf current NPC shop PVF catalog is unavailable")
	errCurrentNPCShopProductUnavailable = errors.New("dnf current NPC shop product is unavailable")
)

type currentNPCShopCatalog struct {
	source       dnfpvf.Source
	buyableItems map[uint32]struct{}
	// shopItems 保留每个商店 [item list] 的原始顺序（槽位下标 = 序号），供 op715
	// 的限购计数表按槽位索引下发。
	shopItems map[uint32][]uint32
	// shopPaths 是 itemshop.lst 里 shop id → .shp 文件名的映射，只用于诊断输出。
	shopPaths                 map[uint32]string
	tradingShops              map[uint32]currentNPCTradingShop
	equipmentSellRatePermille int64
	listedShopCount           int
	loadedShopCount           int
	missingShopCount          int
	firstMissingShopPath      string
}

// currentNPCTradingShop is the PVF-owned special buy/sell/exchange table used
// by the time-stone NPCs. It is deliberately kept separate from itemshop.lst:
// an item listed here is legal only while the matching NPC/shop context is
// present on the current EXE request.
type currentNPCTradingShop struct {
	TradeItems map[uint32]currentNPCTradingItem
	ShopItems  map[uint32]struct{}
}

type currentNPCTradingItem struct {
	BuyGold  int64
	SellGold int64
	Buyable  bool
}

type currentNPCShopPricing struct {
	Definition dungeonDropItemDefinition
	BuyGold    int64
	SellGold   int64
	// ZeroGoldDisposal is set only for active-PVF reinforcement/amplification
	// tickets and portable devices.  Many package-issued variants deliberately
	// omit both [price] and [value]; the NPC may remove those items for zero
	// gold, while unrelated zero-value goods remain fail-closed.
	ZeroGoldDisposal bool
	Buyable          bool
	MaterialExchange bool
	// RequiresGold 表示这一行 PVF **显式**标了 [price] > 0，也就是"金币"这一半是真的。
	// 只写 [need material]、不写 [price] 的商品才是纯材料兑换 —— 它们的 [value] 只是
	// 卖出价基准（同时被当成买入价的兜底），不能据此认定要收金币。
	// 2026-10-08 现场：3042（[price] 2000 + [need material] 3037 x100）被当成纯材料兑换，
	// 连买 7 次金币一分没扣（日志 gold_after 恒定 49355535）。
	RequiresGold      bool
	NeedMaterialItem  uint32
	NeedMaterialCount int64
	TradingShopID     uint32
}

func newCurrentNPCShopCatalog(source dnfpvf.Source) (*currentNPCShopCatalog, error) {
	if source == nil {
		return nil, errCurrentNPCShopCatalogUnavailable
	}
	shopPaths, _, err := loadDungeonDropList(source, currentNPCShopItemListPath)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errCurrentNPCShopCatalogUnavailable, err)
	}
	buyable := make(map[uint32]struct{})
	var shopItems map[uint32][]uint32
	shopPathByID := make(map[uint32]string, len(shopPaths))
	for shopID, listedPath := range shopPaths {
		shopPathByID[uint32(shopID)] = listedPath
	}
	loadedShopCount := 0
	missingShopCount := 0
	firstMissingShopPath := ""
	for shopID, listedPath := range shopPaths {
		documentPath, text, readErr := readDungeonDropText(source, "itemshop", listedPath)
		if readErr != nil {
			if errors.Is(readErr, platformpvf.ErrFileNotFound) || errors.Is(readErr, dnfpvf.ErrDocNotFound) {
				missingShopCount++
				if firstMissingShopPath == "" {
					firstMissingShopPath = listedPath
				}
				continue
			}
			return nil, fmt.Errorf("%w: shop=%d path=%s: %v", errCurrentNPCShopCatalogUnavailable, shopID, listedPath, readErr)
		}
		document, parseErr := dnfpvf.Parse(documentPath, text)
		if parseErr != nil {
			return nil, fmt.Errorf("%w: shop=%d path=%s: %v", errCurrentNPCShopCatalogUnavailable, shopID, documentPath, parseErr)
		}
		loadedShopCount++
		// 每个商店的 [item list] 顺序要保留：客户端 op715 的限购计数表可能按
		// “商品在列表里的槽位下标”而不是道具 id 索引（见 npc_shop_purchase_limit_ack.go）。
		shopItemOrder := make([]uint32, 0, 32)
		for _, section := range document.Sections {
			if strings.ToLower(strings.TrimSpace(section.Name)) != "item list" || section.Start < 0 || section.End > len(document.Tokens) || section.Start > section.End {
				continue
			}
			for _, token := range document.Tokens[section.Start:section.End] {
				if token.Kind == dnfpvf.TokenInt && token.Int > 0 && token.Int <= math.MaxUint32 {
					buyable[uint32(token.Int)] = struct{}{}
					shopItemOrder = append(shopItemOrder, uint32(token.Int))
				}
			}
		}
		if len(shopItemOrder) > 0 {
			if shopItems == nil {
				shopItems = make(map[uint32][]uint32, len(shopPaths))
			}
			shopItems[uint32(shopID)] = shopItemOrder
		}
	}
	if len(buyable) == 0 {
		return nil, fmt.Errorf("%w: no [item list] entries", errCurrentNPCShopCatalogUnavailable)
	}
	tradingShops, err := loadCurrentNPCTradingShops(source)
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %v", errCurrentNPCShopCatalogUnavailable, currentNPCTradingSystemPath, err)
	}
	priceTable, err := parseDungeonCardPVFDocument(source, currentNPCShopPriceTablePath)
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %v", errCurrentNPCShopCatalogUnavailable, currentNPCShopPriceTablePath, err)
	}
	rates := priceTable.Ints("rate")
	if len(rates) < 1 || rates[0] <= 0 || rates[0] > math.MaxInt32 {
		return nil, fmt.Errorf("%w: %s [rate] is invalid", errCurrentNPCShopCatalogUnavailable, currentNPCShopPriceTablePath)
	}
	return &currentNPCShopCatalog{
		source:                    source,
		buyableItems:              buyable,
		shopItems:                 shopItems,
		shopPaths:                 shopPathByID,
		tradingShops:              tradingShops,
		equipmentSellRatePermille: rates[0],
		listedShopCount:           len(shopPaths),
		loadedShopCount:           loadedShopCount,
		missingShopCount:          missingShopCount,
		firstMissingShopPath:      firstMissingShopPath,
	}, nil
}

func (s *Service) currentNPCShopCatalog() (*currentNPCShopCatalog, error) {
	if s == nil {
		return nil, errCurrentNPCShopCatalogUnavailable
	}
	s.npcShopCatalogMu.Lock()
	defer s.npcShopCatalogMu.Unlock()
	if s.npcShopCatalog != nil || s.npcShopCatalogLoadErr != nil {
		return s.npcShopCatalog, s.npcShopCatalogLoadErr
	}
	s.initialEquipmentMu.Lock()
	archive, err := s.initialEquipmentArchiveLocked()
	s.initialEquipmentMu.Unlock()
	if err == nil {
		s.npcShopCatalog, err = newCurrentNPCShopCatalog(archive)
	}
	s.npcShopCatalogLoadErr = err
	if err == nil && s.npcShopCatalog != nil {
		s.logInfo("dnf NPC shop PVF catalog loaded",
			"listed_shops", s.npcShopCatalog.listedShopCount,
			"loaded_shops", s.npcShopCatalog.loadedShopCount,
			"missing_shops", s.npcShopCatalog.missingShopCount,
			"buyable_items", len(s.npcShopCatalog.buyableItems),
			"trading_shops", len(s.npcShopCatalog.tradingShops))
		if s.npcShopCatalog.missingShopCount > 0 {
			s.logWarn("dnf NPC shop PVF list contains missing shop files",
				"missing_shops", s.npcShopCatalog.missingShopCount,
				"first_missing_shop", s.npcShopCatalog.firstMissingShopPath)
		}
	}
	return s.npcShopCatalog, s.npcShopCatalogLoadErr
}

func loadCurrentNPCTradingShops(source dnfpvf.Source) (map[uint32]currentNPCTradingShop, error) {
	shops := make(map[uint32]currentNPCTradingShop)
	if source == nil {
		return shops, errCurrentNPCShopCatalogUnavailable
	}
	text, err := source.ReadText(currentNPCTradingSystemPath)
	if err != nil {
		if errors.Is(err, platformpvf.ErrFileNotFound) || errors.Is(err, dnfpvf.ErrDocNotFound) {
			return shops, nil
		}
		return nil, err
	}
	document, err := dnfpvf.Parse(currentNPCTradingSystemPath, text)
	if err != nil {
		return nil, err
	}
	var currentShopID uint32
	for _, section := range document.Sections {
		if section.Start < 0 || section.End > len(document.Tokens) || section.Start > section.End {
			return nil, fmt.Errorf("invalid section range %q", section.Name)
		}
		name := strings.ToLower(strings.TrimSpace(section.Name))
		tokens := document.Tokens[section.Start:section.End]
		switch name {
		case "trade item list":
			currentShopID = 0
			for _, token := range tokens {
				if token.Kind != dnfpvf.TokenInt || token.Int <= 0 || token.Int > math.MaxUint32 {
					continue
				}
				currentShopID = uint32(token.Int)
				break
			}
			if currentShopID == 0 {
				return nil, errors.New("[trade item list] has no positive shop id")
			}
			shop := shops[currentShopID]
			if shop.TradeItems == nil {
				shop.TradeItems = make(map[uint32]currentNPCTradingItem)
			}
			if shop.ShopItems == nil {
				shop.ShopItems = make(map[uint32]struct{})
			}
			shops[currentShopID] = shop
		case "/trade item list":
			currentShopID = 0
		case "item list":
			if currentShopID == 0 {
				continue
			}
			rows, parseErr := parseCurrentNPCTradingItemRows(tokens)
			if parseErr != nil {
				return nil, fmt.Errorf("shop=%d [item list]: %w", currentShopID, parseErr)
			}
			shop := shops[currentShopID]
			for itemID, row := range rows {
				shop.TradeItems[itemID] = row
			}
			shops[currentShopID] = shop
		case "shop item":
			if currentShopID == 0 {
				continue
			}
			shop := shops[currentShopID]
			for _, token := range tokens {
				if token.Kind == dnfpvf.TokenInt && token.Int > 0 && token.Int <= math.MaxUint32 {
					shop.ShopItems[uint32(token.Int)] = struct{}{}
				}
			}
			shops[currentShopID] = shop
		}
	}
	return shops, nil
}

func parseCurrentNPCTradingItemRows(tokens []dnfpvf.Token) (map[uint32]currentNPCTradingItem, error) {
	numbers := make([]float64, 0, len(tokens))
	integers := make([]bool, 0, len(tokens))
	for _, token := range tokens {
		switch token.Kind {
		case dnfpvf.TokenInt:
			numbers = append(numbers, float64(token.Int))
			integers = append(integers, true)
		case dnfpvf.TokenFloat:
			numbers = append(numbers, token.Float)
			integers = append(integers, false)
		}
	}
	if len(numbers)%4 != 0 {
		return nil, fmt.Errorf("numeric field count=%d is not a multiple of 4", len(numbers))
	}
	rows := make(map[uint32]currentNPCTradingItem, len(numbers)/4)
	for index := 0; index < len(numbers); index += 4 {
		itemID, sellGold, multiplier, buyFlag := numbers[index], numbers[index+1], numbers[index+2], numbers[index+3]
		if !integers[index] || !integers[index+1] || !integers[index+3] || itemID <= 0 || itemID > math.MaxUint32 ||
			sellGold <= 0 || sellGold > math.MaxInt32 || multiplier < 0 || math.IsNaN(multiplier) || math.IsInf(multiplier, 0) ||
			buyFlag < 0 || buyFlag > math.MaxInt32 {
			return nil, fmt.Errorf("invalid row item=%v sell=%v multiplier=%v buy_flag=%v", itemID, sellGold, multiplier, buyFlag)
		}
		buyGoldValue := sellGold * multiplier
		if buyGoldValue < 0 || buyGoldValue > math.MaxInt32 {
			return nil, fmt.Errorf("buy price overflow item=%v price=%v", itemID, buyGoldValue)
		}
		rows[uint32(itemID)] = currentNPCTradingItem{
			BuyGold:  int64(buyGoldValue),
			SellGold: int64(sellGold),
			Buyable:  buyFlag != 0,
		}
	}
	return rows, nil
}

func resolveCurrentNPCShopPricing(shop *currentNPCShopCatalog, items *pvfDungeonDropCatalog, itemID uint32) (currentNPCShopPricing, error) {
	if shop == nil || shop.source == nil || items == nil || itemID == 0 {
		return currentNPCShopPricing{}, errCurrentNPCShopCatalogUnavailable
	}
	definition, err := items.ResolveItem(itemID)
	if err != nil {
		return currentNPCShopPricing{}, fmt.Errorf("%w: item=%d: %v", errCurrentNPCShopProductUnavailable, itemID, err)
	}
	document, err := parseDungeonCardPVFDocument(shop.source, definition.PVFPath)
	if err != nil {
		return currentNPCShopPricing{}, fmt.Errorf("%w: item=%d path=%s: %v", errCurrentNPCShopProductUnavailable, itemID, definition.PVFPath, err)
	}
	price, priceFound := document.Int("price")
	value, valueFound := document.Int("value")
	buyGold := int64(0)
	if priceFound && price >= 0 {
		buyGold = price
	} else if valueFound && value >= 0 {
		buyGold = value
	}
	if buyGold > math.MaxInt32 {
		return currentNPCShopPricing{}, fmt.Errorf("%w: item=%d buy_gold=%d", errCurrentNPCShopProductUnavailable, itemID, buyGold)
	}

	sellGold := int64(0)
	switch definition.Kind {
	case dungeonDropItemEquipment:
		base := buyGold
		if valueFound && value >= 0 {
			base = value
		}
		if base > math.MaxInt64/shop.equipmentSellRatePermille {
			return currentNPCShopPricing{}, fmt.Errorf("%w: item=%d sell price overflow", errCurrentNPCShopProductUnavailable, itemID)
		}
		sellGold = base * shop.equipmentSellRatePermille / 1000
		if sellGold < 1 {
			sellGold = 1
		}
	case dungeonDropItemStackable:
		if valueFound && value >= 0 {
			sellGold = value / 5
		} else if priceFound && price > 0 {
			sellGold = price / 5
		}
	default:
		return currentNPCShopPricing{}, fmt.Errorf("%w: item=%d kind=%s", errCurrentNPCShopProductUnavailable, itemID, definition.Kind)
	}
	if sellGold < 0 || sellGold > math.MaxInt32 {
		return currentNPCShopPricing{}, fmt.Errorf("%w: item=%d sell_gold=%d", errCurrentNPCShopProductUnavailable, itemID, sellGold)
	}
	needMaterial := document.Ints("need material")
	materialExchange := len(needMaterial) >= 2 && needMaterial[0] > 0 && needMaterial[0] <= math.MaxUint32 && needMaterial[1] > 0
	_, listed := shop.buyableItems[itemID]
	pricing := currentNPCShopPricing{
		Definition:       definition,
		BuyGold:          buyGold,
		SellGold:         sellGold,
		ZeroGoldDisposal: sellGold == 0 && currentNPCShopZeroGoldUpgradeDisposal(document),
		MaterialExchange: materialExchange,
		RequiresGold:     priceFound && price > 0,
	}
	if materialExchange {
		pricing.NeedMaterialItem = uint32(needMaterial[0])
		pricing.NeedMaterialCount = needMaterial[1]
	}
	// Material-exchange goods pay with their [need material] rows; when the same
	// PVF block also carries an explicit [price], BOTH are charged (see
	// RequiresGold and commitCurrentNPCShopMaterialExchange).
	pricing.Buyable = listed && (materialExchange || buyGold > 0)
	return pricing, nil
}

func currentNPCShopZeroGoldUpgradeDisposal(document *dnfpvf.Document) bool {
	if document == nil {
		return false
	}
	if len(document.Ints("equipment reinforcement ticket")) > 0 ||
		len(document.Ints("equipment amplify reinforcement ticket")) > 0 {
		return true
	}
	action, found := document.Text("action type")
	if !found {
		return false
	}
	action = strings.ToLower(strings.Trim(strings.TrimSpace(action), "` "))
	return action == "[portable upgrade]" || action == "[portable amplify]"
}

// resolveCurrentNPCShopPricingForContext overlays the context-scoped
// etc/tradingsystem.etc rules on top of ordinary itemshop pricing. The time
// stone itself has no [price]/[value] in its .stk file, while refined time
// stones are intentionally absent from itemshop.lst; both are authorized by
// the matching trading NPC row instead.
func resolveCurrentNPCShopPricingForContext(shop *currentNPCShopCatalog, items *pvfDungeonDropCatalog, itemID uint32, shopContext uint32, actorContext uint32) (currentNPCShopPricing, error) {
	pricing, err := resolveCurrentNPCShopPricing(shop, items, itemID)
	if err != nil {
		return currentNPCShopPricing{}, err
	}
	tradingShopID, tradingShop, found := currentNPCTradingShopForContext(shop, shopContext, actorContext)
	if !found {
		return pricing, nil
	}
	if row, listed := tradingShop.TradeItems[itemID]; listed {
		pricing.BuyGold = row.BuyGold
		pricing.SellGold = row.SellGold
		pricing.Buyable = row.Buyable && row.BuyGold > 0
		pricing.MaterialExchange = false
		pricing.NeedMaterialItem = 0
		pricing.NeedMaterialCount = 0
		pricing.RequiresGold = pricing.BuyGold > 0
		pricing.TradingShopID = tradingShopID
		return pricing, nil
	}
	if _, listed := tradingShop.ShopItems[itemID]; listed {
		pricing.Buyable = pricing.MaterialExchange || pricing.BuyGold > 0
		pricing.TradingShopID = tradingShopID
	}
	return pricing, nil
}

func currentNPCTradingShopForContext(shop *currentNPCShopCatalog, shopContext uint32, actorContext uint32) (uint32, currentNPCTradingShop, bool) {
	if shop == nil || len(shop.tradingShops) == 0 {
		return 0, currentNPCTradingShop{}, false
	}
	// Current op21 special-trading requests identify the NPC in actorContext
	// and carry the 0xFFFFFFFF shop sentinel.  op22 sell requests carry the
	// same actorContext and no shop field.  Never accept a trading-system NPC
	// supplied only through shopContext: doing so lets a forged ordinary-shop
	// request bypass the actor authorization that opened the special UI.
	if actorContext == 0 || shopContext != 0 && shopContext != math.MaxUint32 {
		return 0, currentNPCTradingShop{}, false
	}
	if tradingShop, found := shop.tradingShops[actorContext]; found {
		return actorContext, tradingShop, true
	}
	return 0, currentNPCTradingShop{}, false
}

func currentNPCShopContainsItem(catalog *currentNPCShopCatalog, itemID uint32) bool {
	if catalog == nil {
		return false
	}
	_, found := catalog.buyableItems[itemID]
	return found
}

// currentNPCShopItemTradingListed reports whether a special-trading NPC row owns
// this item's sell price. Time stones and refined time stones carry no
// [price]/[value] in their own scripts and are priced by the matching
// etc/tradingsystem.etc row instead, so a zero price for one of them means the
// request came from a non-matching NPC rather than that the goods are worthless.
func currentNPCShopItemTradingListed(catalog *currentNPCShopCatalog, itemID uint32) bool {
	if catalog == nil || itemID == 0 {
		return false
	}
	for _, tradingShop := range catalog.tradingShops {
		if _, listed := tradingShop.TradeItems[itemID]; listed {
			return true
		}
	}
	return false
}
