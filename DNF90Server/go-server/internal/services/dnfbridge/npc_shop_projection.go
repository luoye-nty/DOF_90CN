package dnfbridge

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"

	dnfcarrygold "longheng.io/server/internal/modules/dnf/carrygold"
	dnfnpcshop "longheng.io/server/internal/modules/dnf/npcshop"
	"longheng.io/server/internal/modules/dnf/purchaselimit"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrevivecoin "longheng.io/server/internal/modules/dnf/revivecoin"
	"longheng.io/server/internal/modules/dnf/wedding"
)

var (
	errCurrentNPCShopOwnerUnavailable     = errors.New("dnf current NPC shop owner is unavailable")
	errCurrentNPCShopGoldInsufficient     = errors.New("dnf current NPC shop gold is insufficient")
	errCurrentNPCShopMaterialInsufficient = errors.New("dnf current NPC shop material is insufficient")
	errCurrentNPCShopItemLocked           = errors.New("dnf current NPC shop item is equipment locked")
	errCurrentNPCShopGoldOverflow         = errors.New("dnf current NPC shop gold exceeds the current client wallet")
	errCurrentNPCShopGoldCarryCap         = errors.New("dnf current NPC shop payout exceeds the gold carry cap")
	// errCurrentNPCShopPurchaseLimitReached：PVF `[purchase limit]` 上限已满。
	errCurrentNPCShopPurchaseLimitReached = errors.New("dnf current NPC shop purchase limit reached")
)

func (s *Service) commitCurrentNPCShopBuy(ctx context.Context, session *gameSession, shop *currentNPCShopCatalog, items *pvfDungeonDropCatalog, request currentNPCShopBuyRequest) (currentNPCShopBuyResult, error) {
	if s == nil || session == nil || session.selectedCharacterID == 0 || shop == nil || items == nil {
		return currentNPCShopBuyResult{}, errCurrentNPCShopOwnerUnavailable
	}
	pricing, err := resolveCurrentNPCShopPricingForContext(shop, items, request.ItemID, request.ShopContext, request.ActorContext)
	if err != nil {
		return currentNPCShopBuyResult{}, err
	}
	if !pricing.Buyable {
		return currentNPCShopBuyResult{}, fmt.Errorf("%w: item=%d listed=%t buy_gold=%d material_exchange=%t", errCurrentNPCShopProductUnavailable, request.ItemID, currentNPCShopContainsItem(shop, request.ItemID), pricing.BuyGold, pricing.MaterialExchange)
	}
	count, err := normalizeCurrentNPCShopBuyCount(pricing.Definition, request.Count)
	if err != nil {
		return currentNPCShopBuyResult{}, err
	}
	request.Count = count
	// 婚房装饰（PVF stackable 类型 `[couple room item]`）不是普通背包商品：它要落进
	// 19 号婚房容器，并伴随装饰加成重算。必须在下面的 stack limit 检查之前分流，
	// 否则会被当成普通堆叠商品按背包上限拒掉。
	if normalizeDungeonDropStackableType(pricing.Definition.StackableType) == "[couple room item]" {
		return s.commitWeddingDecorationBuy(ctx, session, pricing, request)
	}
	if pricing.Definition.Kind == dungeonDropItemStackable && pricing.Definition.StackLimit > 0 && int64(request.Count) > pricing.Definition.StackLimit {
		return currentNPCShopBuyResult{}, fmt.Errorf("%w: item=%d count=%d stack_limit=%d", errCurrentNPCShopProductUnavailable, request.ItemID, request.Count, pricing.Definition.StackLimit)
	}
	if pricing.MaterialExchange {
		// 心意点（LovePointMaterial）是 PVF 商店的"夫妻货币"，不是可堆叠实体道具。
		// 它必须走婚姻事务（买卖双方一起校验、金币与心意点同事务扣减），
		// 不能落到通用材料交换路径。
		if pricing.NeedMaterialItem == wedding.LovePointMaterial {
			return s.commitWeddingRingBuy(ctx, session, pricing, request)
		}
		return s.commitCurrentNPCShopMaterialExchange(ctx, session, pricing, request)
	}
	if pricing.BuyGold > math.MaxInt64/int64(request.Count) {
		return currentNPCShopBuyResult{}, errCurrentNPCShopGoldOverflow
	}
	totalCost := pricing.BuyGold * int64(request.Count)
	accountID, characterID, repositories, owner, err := s.currentNPCShopMutationOwner(session)
	if err != nil {
		return currentNPCShopBuyResult{}, err
	}
	// 未解锁的背包扩展行不是合法落点，见 currentNPCShopPlacementMainSlotCount。
	placementMainSlotCount := s.currentInventoryPlacementMainSlotCount(ctx, nil, characterID)
	var result currentNPCShopBuyResult
	err = owner.Mutate(ctx, dnfnpcshop.Command{
		AccountID:   accountID,
		CharacterID: characterID,
		Project: func(assets *dnfnpcshop.Assets) (dnfnpcshop.Changes, error) {
			character := assets.Character
			inventory := assets.Inventory
			if character.Stats == nil {
				return dnfnpcshop.Changes{}, errCurrentNPCShopOwnerUnavailable
			}
			gold := character.Stats["gold"]
			if gold < 0 || gold > math.MaxInt32 {
				return dnfnpcshop.Changes{}, errCurrentNPCShopGoldOverflow
			}
			if gold < totalCost {
				return dnfnpcshop.Changes{}, fmt.Errorf("%w: need=%d have=%d", errCurrentNPCShopGoldInsufficient, totalCost, gold)
			}
			// PVF 限购：超限即整笔回滚（同事务里金币、道具、计数一起失败）。
			limitPolicy, limitState, limitErr := consumeCurrentPurchaseLimit(assets.Character, assets.Account, pricing.Definition, request.Count, s.currentPurchaseLimitTime())
			if limitErr != nil {
				return dnfnpcshop.Changes{}, fmt.Errorf("%w: item=%d scope=%s period=%s used=%d requested=%d limit=%d: %w",
					errCurrentPurchaseLimitReached, request.ItemID, limitPolicy.Scope, limitPolicy.Period,
					limitState.Used, request.Count, limitPolicy.Limit, limitErr)
			}
			// 无色小晶块 / 灵魂 这 12 个道具在自己账号里只有一个固定格
			// （晶体仓库 354..359 + 灵魂仓库 360..365，权威表见
			// repository/account_shared_materials.go）。**任何获取途径都必须落进
			// 那个格子**，金币购买这条路过去漏了这一步：1111 买的 20 个无色留在
			// 角色背包 0:154，而账号槽 0:358 另有 43890 个，同一条 list-0 报文里
			// 出现两行同 itemID，客户端把背包那一行显示成了仓库的数量（现场
			// 2026-09-21）。
			//
			// 落点判定不再在这里手写，改调统一入口 grantCurrentAccountSharedFirst
			// （account_shared_grant.go）—— 同一个函数材料兑换、分解、礼盒等路径
			// 也在用，新增功能照抄一行即可。装不下的余量仍然交给 NPC 商店自己的
			// 角色侧发放（含 list 38 勋章/守护宝石与复活币钱包两条特例）。
			account := assets.AccountInventory
			if account == nil {
				return dnfnpcshop.Changes{}, errCurrentNPCShopOwnerUnavailable
			}
			shared := grantCurrentAccountSharedFirst(account, pricing.Definition, request.Count, "npc_shop")
			accountDirty := shared.Changed
			grantSlots := make([]int16, 0, 2)
			itemListType := dnfrepo.MainInventoryListType
			if shared.Changed {
				grantSlots = append(grantSlots, shared.Slot)
			}
			// 两种情形才走角色侧：该道具没有固定格（Remainder == 整笔），
			// 或固定格装不下（堆叠上限已满 / 该格被别的道具占着的历史脏数据）。
			characterCount := shared.Remainder
			if !shared.Changed {
				characterCount = request.Count
			}
			if characterCount > 0 {
				listType, slots, grantErr := grantCurrentNPCShopProduct(inventory, pricing.Definition, characterCount, placementMainSlotCount)
				if grantErr != nil {
					return dnfnpcshop.Changes{}, grantErr
				}
				if !shared.Changed && len(slots) != 1 {
					// 没有固定格时保持旧的"恰好一个槽"严格性，行为逐字不变。
					return dnfnpcshop.Changes{}, fmt.Errorf("%w: item=%d changed_slots=%d", errCurrentNPCShopProductUnavailable, request.ItemID, len(slots))
				}
				itemListType = listType
				grantSlots = append(grantSlots, slots...)
			}
			if len(grantSlots) == 0 {
				return dnfnpcshop.Changes{}, fmt.Errorf("%w: item=%d changed_slots=0", errCurrentNPCShopProductUnavailable, request.ItemID)
			}
			goldAfter := gold - totalCost
			character.Stats["gold"] = goldAfter
			updates := []currentItemListEntry{currentGoldWalletItemListEntry(goldAfter)}
			var itemUpdates []currentItemListEntry
			var itemEntry currentItemListEntry
			// A revive coin lands in the fixed slot-1 wallet as item 1, so the
			// row that came back legitimately carries a different item id than
			// the product bought (item 42, cash/coin_general.stk).
			revivedCoinProduct := dnfrevivecoin.IsReward(int64(request.ItemID))
			for index, slot := range grantSlots {
				key := currentCeraShopInventorySlotKey(itemListType, slot)
				// 账号共享槽（0:354..365）由账号记录持有，角色背包格由角色记录持有；
				// 同一个 list-0 线地址，落库归属不同 —— 先查账号再查角色。
				stack, found := account.Slots[key]
				if !found {
					stack, found = inventory.Slots[key]
				}
				if !found || stack.Count <= 0 ||
					(!revivedCoinProduct && stack.ItemID != int64(request.ItemID)) ||
					(revivedCoinProduct && stack.ItemID != dnfrevivecoin.WalletItemID) {
					return dnfnpcshop.Changes{}, errCurrentNPCShopProductUnavailable
				}
				if stack.Extra == nil {
					stack.Extra = make(map[string]string, 6)
				}
				stack.Extra["last_grant_source"] = "npc_shop"
				stack.Extra["last_npc_shop_context"] = strconv.FormatUint(uint64(request.ShopContext), 10)
				stack.Extra["last_npc_shop_actor_context"] = strconv.FormatUint(uint64(request.ActorContext), 10)
				entry := currentItemListEntryFromStack(itemListType, slot, stack)
				stack.RawEntry = append([]byte(nil), entry.data[:]...)
				if _, accountOwned := account.Slots[key]; accountOwned {
					account.Slots[key] = stack
					accountDirty = true
				} else {
					inventory.Slots[key] = stack
				}
				if index == 0 {
					itemEntry = entry
				}
				if itemListType == dnfrepo.MainInventoryListType {
					updates = append(updates, entry)
				} else {
					itemUpdates = append(itemUpdates, entry)
				}
			}
			sortCurrentItemListEntries(updates)
			result = currentNPCShopBuyResult{
				GoldAfter:    goldAfter,
				SPAfter:      character.Stats["sp"],
				Item:         itemEntry,
				Updates:      updates,
				ItemListType: itemListType,
				ItemUpdates:  itemUpdates,
				Slot:         grantSlots[0],
				ItemID:       request.ItemID,
				Count:        request.Count,
			}
			changes := dnfnpcshop.Changes{Character: true, Inventory: true, AccountInventory: accountDirty}
			if limitPolicy.Scope == purchaselimit.ScopeAccount {
				changes.Account = true
			}
			return changes, nil
		},
	})
	if err != nil {
		return currentNPCShopBuyResult{}, currentNPCShopMutationError(err)
	}
	// Cera is the account-shared pool; the NPC shop ACK only displays it and
	// never debits, so a read-only post-commit load is sufficient.
	result.CoinAfter = s.loadCurrentAccountCera(ctx, repositories, session)
	return result, nil
}

// commitCurrentNPCShopMaterialExchange buys a [need material] shop item: it
// consumes the material cost (account cube-fragment cell for cube fragments,
// first matching character stack otherwise) and grants the purchase in the
// same account+inventory transaction. Cube-fragment purchases land on their
// fixed account warehouse slots.
//
// 2026-10-08: when the same PVF block ALSO carries an explicit [price], the gold
// half is charged in this very transaction (currentNPCShopPricing.RequiresGold).
// Every [need material] item used to be treated as material-only, so mixed goods
// like 3042 ([price] 2000 + [need material] 3037 x100) cost no gold at all: seven
// consecutive buys left gold_after unchanged at 49355535.
func (s *Service) commitCurrentNPCShopMaterialExchange(ctx context.Context, session *gameSession, pricing currentNPCShopPricing, request currentNPCShopBuyRequest) (currentNPCShopBuyResult, error) {
	if s == nil || session == nil || session.selectedCharacterID == 0 {
		return currentNPCShopBuyResult{}, errCurrentNPCShopOwnerUnavailable
	}
	switch pricing.Definition.Kind {
	case dungeonDropItemEquipment:
		if request.Count != 1 {
			return currentNPCShopBuyResult{}, fmt.Errorf("%w: exchange equipment=%d count=%d", errCurrentNPCShopProductUnavailable, request.ItemID, request.Count)
		}
	case dungeonDropItemStackable:
		if request.Count == 0 {
			return currentNPCShopBuyResult{}, fmt.Errorf("%w: exchange stackable=%d count=0", errCurrentNPCShopProductUnavailable, request.ItemID)
		}
	default:
		return currentNPCShopBuyResult{}, fmt.Errorf("%w: exchange item=%d kind=%s", errCurrentNPCShopProductUnavailable, request.ItemID, pricing.Definition.Kind)
	}
	if pricing.NeedMaterialItem == 0 || pricing.NeedMaterialCount <= 0 || pricing.NeedMaterialCount > math.MaxInt64/int64(request.Count) {
		return currentNPCShopBuyResult{}, fmt.Errorf("%w: exchange item=%d cost=%d x%d", errCurrentNPCShopProductUnavailable, request.ItemID, pricing.NeedMaterialItem, pricing.NeedMaterialCount)
	}
	totalMaterial := pricing.NeedMaterialCount * int64(request.Count)
	// 显式标了 [price] 的商品：金币与材料必须在同一个事务里一起扣，否则就是"只收材料"。
	goldCost := int64(0)
	if pricing.RequiresGold {
		if pricing.BuyGold <= 0 {
			return currentNPCShopBuyResult{}, fmt.Errorf("%w: exchange item=%d requires_gold with price=%d", errCurrentNPCShopProductUnavailable, request.ItemID, pricing.BuyGold)
		}
		if pricing.BuyGold > math.MaxInt64/int64(request.Count) {
			return currentNPCShopBuyResult{}, errCurrentNPCShopGoldOverflow
		}
		goldCost = pricing.BuyGold * int64(request.Count)
	}
	accountID, characterID, repositories, owner, err := s.currentNPCShopMutationOwner(session)
	if err != nil {
		return currentNPCShopBuyResult{}, err
	}
	// 未解锁的背包扩展行不是合法落点，见 currentNPCShopPlacementMainSlotCount。
	placementMainSlotCount := s.currentInventoryPlacementMainSlotCount(ctx, nil, characterID)
	var result currentNPCShopBuyResult
	err = owner.Mutate(ctx, dnfnpcshop.Command{
		AccountID:   accountID,
		CharacterID: characterID,
		Project: func(assets *dnfnpcshop.Assets) (dnfnpcshop.Changes, error) {
			character := assets.Character
			inventory := assets.Inventory
			account := assets.AccountInventory
			if character.Stats == nil {
				return dnfnpcshop.Changes{}, errCurrentNPCShopOwnerUnavailable
			}
			gold := character.Stats["gold"]
			if gold < 0 || gold > math.MaxInt32 {
				return dnfnpcshop.Changes{}, errCurrentNPCShopGoldOverflow
			}
			if gold < goldCost {
				return dnfnpcshop.Changes{}, fmt.Errorf("%w: need=%d have=%d", errCurrentNPCShopGoldInsufficient, goldCost, gold)
			}
			goldAfter := gold - goldCost
			if goldCost > 0 {
				character.Stats["gold"] = goldAfter
			}
			updates := make([]currentItemListEntry, 0, 4)

			// Consume the material cost.
			accountDirty := false
			if materialSlot, warehouseMaterial := currentDisjointWarehouseFixedSlots[pricing.NeedMaterialItem]; warehouseMaterial {
				key := dnfrepo.AccountSharedInventorySlotKey(materialSlot)
				stack, exists := account.Slots[key]
				if !exists || stack.ItemID != int64(pricing.NeedMaterialItem) || stack.Count < totalMaterial {
					return dnfnpcshop.Changes{}, fmt.Errorf("%w: item=%d need=%d have=%d", errCurrentNPCShopMaterialInsufficient, pricing.NeedMaterialItem, totalMaterial, stack.Count)
				}
				stack.Count -= totalMaterial
				entry := currentItemListEntryFromStack(dnfrepo.MainInventoryListType, materialSlot, stack)
				stack.RawEntry = append([]byte(nil), entry.data[:]...)
				account.Slots[key] = stack
				accountDirty = true
				result.CostItemID = pricing.NeedMaterialItem
				result.CostItemNewCount = stack.Count
				updates = append(updates, entry)
			} else {
				type materialCandidate struct {
					slot  int16
					key   string
					stack dnfrepo.ItemStack
				}
				candidates := make([]materialCandidate, 0, 2)
				available := int64(0)
				for slot := int16(0); slot <= dnfrepo.SoulWarehouseLastSlot; slot++ {
					if dnfrepo.IsAccountSharedInventorySlot(dnfrepo.MainInventoryListType, slot) {
						continue
					}
					key := currentCeraShopInventorySlotKey(dnfrepo.MainInventoryListType, slot)
					stack, exists := inventory.Slots[key]
					if exists && stack.ItemID == int64(pricing.NeedMaterialItem) && stack.Count > 0 {
						if available > math.MaxInt64-stack.Count {
							return dnfnpcshop.Changes{}, errCurrentNPCShopGoldOverflow
						}
						available += stack.Count
						candidates = append(candidates, materialCandidate{slot: slot, key: key, stack: stack})
					}
				}
				if available < totalMaterial {
					return dnfnpcshop.Changes{}, fmt.Errorf("%w: item=%d need=%d have=%d", errCurrentNPCShopMaterialInsufficient, pricing.NeedMaterialItem, totalMaterial, available)
				}
				remainingCost := totalMaterial
				for _, candidate := range candidates {
					if remainingCost == 0 {
						break
					}
					take := candidate.stack.Count
					if take > remainingCost {
						take = remainingCost
					}
					candidate.stack.Count -= take
					remainingCost -= take
					if candidate.stack.Count == 0 {
						delete(inventory.Slots, candidate.key)
						var removed currentItemListEntry
						removed.patchCore(candidate.slot, math.MaxUint32, 0)
						updates = append(updates, removed)
						continue
					}
					entry := currentItemListEntryFromStack(dnfrepo.MainInventoryListType, candidate.slot, candidate.stack)
					candidate.stack.RawEntry = append([]byte(nil), entry.data[:]...)
					inventory.Slots[candidate.key] = candidate.stack
					updates = append(updates, entry)
				}
				result.CostItemID = pricing.NeedMaterialItem
				result.CostItemNewCount = available - totalMaterial
			}

			// Grant the purchase.
			// PVF 限购：材料兑换与金币购买共用同一套计数。角色维度落在
			// 角色 Stats、账号维度落在账号记录 Metadata，分别需要各自的保存
			// 标志（AccountInventory 是账号仓库记录，与账号记录不是同一条）。
			limitPolicy, limitState, limitErr := consumeCurrentPurchaseLimit(assets.Character, assets.Account, pricing.Definition, request.Count, s.currentPurchaseLimitTime())
			if limitErr != nil {
				return dnfnpcshop.Changes{}, fmt.Errorf("%w: item=%d scope=%s period=%s used=%d requested=%d limit=%d: %w",
					errCurrentPurchaseLimitReached, request.ItemID, limitPolicy.Scope, limitPolicy.Period,
					limitState.Used, request.Count, limitPolicy.Limit, limitErr)
			}
			accountRecordDirty := limitPolicy.Scope == purchaselimit.ScopeAccount
			characterStatsDirty := limitPolicy.Scope == purchaselimit.ScopeCharacter
			// 与金币购买分支共用同一个落点入口（grantCurrentAccountSharedFirst）。
			// 这两条分支过去各写一遍落点判定 —— 正是那次分家让金币购买漏掉了固定槽，
			// 20 个无色落进背包（2026-09-21）。现在它们只可能一起对或一起错。
			shared := grantCurrentAccountSharedFirst(account, pricing.Definition, request.Count, "npc_shop_material_exchange")
			accountDirty = accountDirty || shared.Changed
			grantSlots := make([]int16, 0, 2)
			itemListType := dnfrepo.MainInventoryListType
			if shared.Changed {
				grantSlots = append(grantSlots, shared.Slot)
			}
			characterCount := shared.Remainder
			if !shared.Changed {
				characterCount = request.Count
			}
			if characterCount > 0 {
				listType, slots, grantErr := grantCurrentNPCShopProduct(inventory, pricing.Definition, characterCount, placementMainSlotCount)
				if grantErr != nil {
					return dnfnpcshop.Changes{}, grantErr
				}
				if !shared.Changed && len(slots) != 1 {
					return dnfnpcshop.Changes{}, fmt.Errorf("%w: item=%d changed_slots=%d", errCurrentNPCShopProductUnavailable, request.ItemID, len(slots))
				}
				itemListType = listType
				grantSlots = append(grantSlots, slots...)
			}
			if len(grantSlots) == 0 {
				return dnfnpcshop.Changes{}, fmt.Errorf("%w: item=%d changed_slots=0", errCurrentNPCShopProductUnavailable, request.ItemID)
			}
			var itemEntry currentItemListEntry
			var itemUpdates []currentItemListEntry
			for index, slot := range grantSlots {
				key := currentCeraShopInventorySlotKey(itemListType, slot)
				stack, found := account.Slots[key]
				if !found {
					stack, found = inventory.Slots[key]
				}
				if !found || stack.ItemID != int64(request.ItemID) || stack.Count <= 0 {
					return dnfnpcshop.Changes{}, errCurrentNPCShopProductUnavailable
				}
				if stack.Extra == nil {
					stack.Extra = make(map[string]string, 4)
				}
				stack.Extra["last_grant_source"] = "npc_shop"
				stack.Extra["last_npc_shop_context"] = strconv.FormatUint(uint64(request.ShopContext), 10)
				stack.Extra["last_npc_shop_actor_context"] = strconv.FormatUint(uint64(request.ActorContext), 10)
				entry := currentItemListEntryFromStack(itemListType, slot, stack)
				stack.RawEntry = append([]byte(nil), entry.data[:]...)
				if _, isAccount := account.Slots[key]; isAccount {
					account.Slots[key] = stack
					accountDirty = true
				} else {
					inventory.Slots[key] = stack
				}
				if index == 0 {
					itemEntry = entry
				}
				if itemListType == dnfrepo.MainInventoryListType {
					updates = append(updates, entry)
				} else {
					itemUpdates = append(itemUpdates, entry)
				}
			}
			updates = append(updates, currentGoldWalletItemListEntry(goldAfter))
			sortCurrentItemListEntries(updates)
			result.GoldAfter = goldAfter
			result.SPAfter = character.Stats["sp"]
			result.Item = itemEntry
			result.Updates = updates
			result.ItemListType = itemListType
			result.ItemUpdates = itemUpdates
			result.Slot = grantSlots[0]
			result.ItemID = request.ItemID
			result.Count = request.Count
			return dnfnpcshop.Changes{
				Account:          accountRecordDirty,
				AccountInventory: accountDirty,
				// 金币在角色 Stats 里：扣了就必须把角色记录一起落库。
				Character: characterStatsDirty || goldCost > 0,
				Inventory: true,
			}, nil
		},
	})
	if err != nil {
		return currentNPCShopBuyResult{}, currentNPCShopMutationError(err)
	}
	result.CoinAfter = s.loadCurrentAccountCera(ctx, repositories, session)
	return result, nil
}

func (s *Service) commitCurrentNPCShopSell(ctx context.Context, session *gameSession, shop *currentNPCShopCatalog, items *pvfDungeonDropCatalog, request currentNPCShopSellRequest) (currentNPCShopSellResult, error) {
	if s == nil || session == nil || session.selectedCharacterID == 0 || shop == nil || items == nil {
		return currentNPCShopSellResult{}, errCurrentNPCShopOwnerUnavailable
	}
	accountID, characterID, _, owner, err := s.currentNPCShopMutationOwner(session)
	if err != nil {
		return currentNPCShopSellResult{}, err
	}
	var result currentNPCShopSellResult
	err = owner.Mutate(ctx, dnfnpcshop.Command{
		AccountID:   accountID,
		CharacterID: characterID,
		Project: func(assets *dnfnpcshop.Assets) (dnfnpcshop.Changes, error) {
			character := assets.Character
			inventory := assets.Inventory
			if character.Stats == nil {
				return dnfnpcshop.Changes{}, errCurrentNPCShopOwnerUnavailable
			}
			// 晶体/灵魂仓库（list 0, 354..365）是**账号**所有：同一个 list-0 线地址
			// 由账号记录持有，客户端照旧按普通 list-0 格子寻址。卖这一格就是扣账号记录
			// 里的那一行，扣完把账号记录写回去（Changes.AccountInventory）。
			accountOwned := dnfrepo.IsAccountSharedInventorySlot(request.ListType, request.Slot)
			itemSlots := inventory.Slots
			if accountOwned {
				if assets.AccountInventory == nil {
					return dnfnpcshop.Changes{}, errCurrentNPCShopOwnerUnavailable
				}
				itemSlots = assets.AccountInventory.Slots
			}
			key := currentCeraShopInventorySlotKey(request.ListType, request.Slot)
			stack, found := itemSlots[key]
			if !found || stack.ItemID <= 0 || stack.ItemID > math.MaxUint32 || stack.Count <= 0 {
				return dnfnpcshop.Changes{}, fmt.Errorf("%w: list=%d slot=%d", errCurrentNPCShopProductUnavailable, request.ListType, request.Slot)
			}
			if currentNPCShopItemLocked(stack) {
				return dnfnpcshop.Changes{}, fmt.Errorf("%w: list=%d slot=%d", errCurrentNPCShopItemLocked, request.ListType, request.Slot)
			}
			pricing, priceErr := resolveCurrentNPCShopPricingForContext(shop, items, uint32(stack.ItemID), 0, request.ActorContext)
			if priceErr != nil {
				return dnfnpcshop.Changes{}, priceErr
			}
			// Zero-value goods are sellable. No item-dictionary field forbids NPC
			// sales (scanned 306066 stackable/equipment scripts, 1465 distinct keys:
			// "sell"/"sale" only match the event-only [sale limit]/[sale npc index],
			// and [npc gift disallowance] governs NPC gifts), so an item without
			// [price]/[value] simply pays nothing and is consumed - which is exactly
			// what the client already shows and lets the player confirm.
			//
			// Trading-system goods stay fail-closed: their price lives in the
			// matching NPC row (time stones carry no PVF price at all), so a zero
			// price there means the wrong NPC was used, not worthless goods.
			if pricing.SellGold < 0 ||
				pricing.SellGold == 0 && !pricing.ZeroGoldDisposal && currentNPCShopItemTradingListed(shop, uint32(stack.ItemID)) {
				return dnfnpcshop.Changes{}, fmt.Errorf("%w: item=%d sell_gold=%d", errCurrentNPCShopProductUnavailable, stack.ItemID, pricing.SellGold)
			}
			applied := int64(request.Count)
			if applied > stack.Count {
				applied = stack.Count
			}
			if pricing.Definition.Kind == dungeonDropItemEquipment {
				applied = 1
			}
			if applied <= 0 || applied > math.MaxUint32 || pricing.SellGold > 0 && applied > math.MaxInt64/pricing.SellGold {
				return dnfnpcshop.Changes{}, errCurrentNPCShopGoldOverflow
			}
			gold := character.Stats["gold"]
			if gold < 0 || gold > math.MaxInt32 {
				return dnfnpcshop.Changes{}, errCurrentNPCShopGoldOverflow
			}
			goldDelta := pricing.SellGold * applied
			if goldDelta > math.MaxInt32-gold {
				return dnfnpcshop.Changes{}, errCurrentNPCShopGoldOverflow
			}
			// Selling is the one gold faucet the player can retry, so the carry
			// cap refuses the sale instead of discarding the payout: the item
			// stays in the bag rather than being turned into nothing.
			if room := dnfcarrygold.Room(*character); goldDelta > room {
				return dnfnpcshop.Changes{}, fmt.Errorf(
					"%w: payout=%d room=%d limit=%d",
					errCurrentNPCShopGoldCarryCap,
					goldDelta,
					room,
					dnfcarrygold.LimitForCharacter(*character),
				)
			}
			remaining := stack.Count - applied
			var itemUpdate currentItemListEntry
			if remaining <= 0 {
				delete(itemSlots, key)
				if accountOwned {
					// 账号共享晶体/灵魂槽清空写 (itemID=0, count=0)，与拍卖行上架、摆摊售出
					// 同一写法；0xFFFFFFFF 只适用于角色背包格（GM 工具那条路径实测）。
					itemUpdate.patchCore(request.Slot, 0, 0)
				} else {
					itemUpdate.patchCore(request.Slot, math.MaxUint32, 0)
				}
			} else {
				stack.Count = remaining
				itemUpdate = currentItemListEntryFromStack(request.ListType, request.Slot, stack)
				stack.RawEntry = append([]byte(nil), itemUpdate.data[:]...)
				itemSlots[key] = stack
			}
			goldAfter := gold + goldDelta
			character.Stats["gold"] = goldAfter
			updates := []currentItemListEntry{currentGoldWalletItemListEntry(goldAfter), itemUpdate}
			sortCurrentItemListEntries(updates)
			result = currentNPCShopSellResult{
				GoldAfter: goldAfter,
				ListType:  request.ListType,
				Slot:      request.Slot,
				Applied:   uint32(applied),
				ItemID:    uint32(stack.ItemID),
				Updates:   updates,
			}
			return dnfnpcshop.Changes{
				Character:        true,
				Inventory:        !accountOwned,
				AccountInventory: accountOwned,
			}, nil
		},
	})
	if err != nil {
		return currentNPCShopSellResult{}, currentNPCShopMutationError(err)
	}
	return result, nil
}

func currentNPCShopItemLocked(stack dnfrepo.ItemStack) bool {
	if stack.Extra == nil {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(stack.Extra["equipment_lock_state"])) {
	case "1", "2", "active", "locked", "unlocking", "pending_unlock":
		return true
	default:
		return false
	}
}

// currentInventoryPlacementMainSlotCountFor 是自由函数形式，供没有 Service 接收者
// 的发放路径使用（例如 commitCurrentCompoundRecipe）。
// 读不到容器状态时返回 0 —— 对应"默认 4 行"，宁可对已扩展角色少放宽几行，也绝
// 不让物品落进未解锁行。这里用只读的 LoadCharacterContainerState，不在事务外产生
// 任何写入。
func currentInventoryPlacementMainSlotCountFor(
	ctx context.Context,
	repository dnfrepo.SettingsRepository,
	characterID string,
) uint16 {
	characterID = strings.TrimSpace(characterID)
	if characterID == "" || repository == nil {
		return 0
	}
	state, found, err := dnfrepo.LoadCharacterContainerState(ctx, repository, characterID)
	if err != nil || !found {
		return 0
	}
	return state.MainSlotCount
}

// currentInventoryPlacementMainSlotCount 取该角色当前已购买的背包扩展行数
// （CharacterContainerState.MainSlotCount，取值 0/8/16/24）。
//
// 它必须一路透传到 grantCurrentCeraShopProduct，否则
// limitCurrentMainInventoryDefinitionSlots 不会被调用，新物品的落点会越过已解锁
// 范围落进未解锁的扩展行——那些格子客户端不渲染，物品看起来就"消失"了。
// 赛丽亚商店是在事务内从 assets.Settings 取再透传的；其余模块的 Assets 都没有
// Settings 字段，所以这里在 bridge 侧用容器状态统一取。
//
// characterID 为空时退回 inventory.CharacterID（Load 会填充它）。
func (s *Service) currentInventoryPlacementMainSlotCount(
	ctx context.Context,
	inventory *dnfrepo.InventoryRecord,
	characterID string,
) uint16 {
	characterID = strings.TrimSpace(characterID)
	if characterID == "" && inventory != nil {
		characterID = strings.TrimSpace(inventory.CharacterID)
	}
	if characterID == "" {
		return 0
	}
	repositories, ok := s.repositoryGroup()
	if !ok {
		return 0
	}
	return currentInventoryPlacementMainSlotCountFor(ctx, repositories.Settings, characterID)
}

func (s *Service) currentNPCShopMutationOwner(session *gameSession) (string, string, dnfrepo.Group, *dnfnpcshop.Owner, error) {
	if s == nil || session == nil || session.selectedCharacterID == 0 {
		return "", "", dnfrepo.Group{}, nil, errCurrentNPCShopOwnerUnavailable
	}
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.AccountAssets == nil {
		return "", "", dnfrepo.Group{}, nil, errCurrentNPCShopOwnerUnavailable
	}
	accountID := strings.TrimSpace(s.accountIDForSession(session))
	if accountID == "" {
		return "", "", dnfrepo.Group{}, nil, errCurrentNPCShopOwnerUnavailable
	}
	owner, err := dnfnpcshop.NewOwner(repositories)
	if err != nil {
		return "", "", dnfrepo.Group{}, nil, errCurrentNPCShopOwnerUnavailable
	}
	characterID := strconv.FormatUint(uint64(session.selectedCharacterID), 10)
	return accountID, characterID, repositories, owner, nil
}

func currentNPCShopMutationError(err error) error {
	switch {
	case errors.Is(err, dnfnpcshop.ErrOwnerUnavailable),
		errors.Is(err, dnfnpcshop.ErrAccountRequired),
		errors.Is(err, dnfnpcshop.ErrCharacterRequired),
		errors.Is(err, dnfnpcshop.ErrCharacterNotFound),
		errors.Is(err, dnfnpcshop.ErrAccountMismatch),
		errors.Is(err, dnfnpcshop.ErrInventoryNotFound):
		return errors.Join(errCurrentNPCShopOwnerUnavailable, err)
	default:
		return err
	}
}
