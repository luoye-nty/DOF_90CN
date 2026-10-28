package dnfbridge

import (
	"context"
	"errors"

	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfproto "longheng.io/server/internal/modules/dnf/protocol"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

func npcShopGameplayModule() gameplayModuleDefinition {
	buyOpcode := uint16(dnfenum.CmdPacketBuyItem)
	purchaseCountOpcode := uint16(dnfenum.CmdPacketShopPurchaseCount)
	sellOpcode := uint16(dnfenum.CmdPacketSellItem)
	return gameplayModuleDefinition{
		Name: "npc-shop",
		LegacyHandlers: map[uint16]gameplayHandler{
			buyOpcode: func(service *Service, session *gameSession, request gameplayRequest) error {
				return service.handleCurrentNPCShopBuy(session, request.Body)
			},
			purchaseCountOpcode: func(service *Service, session *gameSession, request gameplayRequest) error {
				return service.handleCurrentNPCShopPurchaseCount(session, request.Body)
			},
			sellOpcode: func(service *Service, session *gameSession, request gameplayRequest) error {
				return service.handleCurrentNPCShopSell(session, request.Body)
			},
		},
		UpperHandlers: map[uint16]gameplayHandler{
			buyOpcode: func(service *Service, session *gameSession, request gameplayRequest) error {
				if !requireDefaultGameplayClassification(service, session, request, "game-npc-shop-buy-blocked", "current_exe_op21_command_class_mismatch") {
					return nil
				}
				return service.handleCurrentNPCShopBuy(session, request.Body)
			},
			purchaseCountOpcode: func(service *Service, session *gameSession, request gameplayRequest) error {
				if !requireDefaultGameplayClassification(service, session, request, "game-npc-shop-purchase-count-blocked", "current_exe_op715_command_class_mismatch") {
					return nil
				}
				return service.handleCurrentNPCShopPurchaseCount(session, request.Body)
			},
			sellOpcode: func(service *Service, session *gameSession, request gameplayRequest) error {
				if !requireDefaultGameplayClassification(service, session, request, "game-npc-shop-sell-blocked", "current_exe_op22_command_class_mismatch") {
					return nil
				}
				return service.handleCurrentNPCShopSell(session, request.Body)
			},
		},
		LegacyNormalizers: map[uint16]gameplayLegacyNormalizer{
			buyOpcode: func(body []byte) []byte {
				return stripLegacyTransportTrailer(body, currentNPCShopBuyRequestWireSize)
			},
			purchaseCountOpcode: func(body []byte) []byte {
				return stripLegacyTransportTrailer(body, currentNPCShopPurchaseCountRequestWireSize)
			},
			sellOpcode: func(body []byte) []byte {
				return stripLegacyTransportTrailer(body, currentNPCShopSellRequestWireSize)
			},
		},
	}
}

func (s *Service) handleCurrentNPCShopBuy(session *gameSession, body []byte) error {
	request, err := parseCurrentNPCShopBuyRequest(body)
	if err != nil {
		s.logGameEvent(session, "game-npc-shop-buy-rejected", "body_len", len(body), "reason", err)
		return s.sendCurrentNPCShopFailure(session, uint16(dnfenum.CmdPacketBuyItem), currentNPCShopBuyErrorCode)
	}
	shop, err := s.currentNPCShopCatalog()
	if err != nil {
		s.logGameEvent(session, "game-npc-shop-buy-rejected", "item_id", request.ItemID, "count", request.Count, "reason", err)
		return s.sendCurrentNPCShopFailure(session, uint16(dnfenum.CmdPacketBuyItem), currentNPCShopBuyErrorCode)
	}
	items, err := s.currentPVFItemCatalog()
	if err != nil {
		s.logGameEvent(session, "game-npc-shop-buy-rejected", "item_id", request.ItemID, "count", request.Count, "reason", err)
		return s.sendCurrentNPCShopFailure(session, uint16(dnfenum.CmdPacketBuyItem), currentNPCShopBuyErrorCode)
	}
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	result, err := s.commitCurrentNPCShopBuy(ctx, session, shop, items, request)
	if err != nil {
		// PVF 限购超限单独记一条，便于与“买不起 / 商品不可买”区分开。
		if errors.Is(err, errCurrentPurchaseLimitReached) {
			s.logGameEvent(session, "game-npc-shop-buy-limit-rejected", "item_id", request.ItemID, "count", request.Count, "shop_context", request.ShopContext, "actor_context", request.ActorContext, "reason", err)
			return s.sendCurrentNPCShopFailure(session, uint16(dnfenum.CmdPacketBuyItem), currentPurchaseLimitErrorCode)
		}
		s.logGameEvent(session, "game-npc-shop-buy-rejected", "item_id", request.ItemID, "count", request.Count, "shop_context", request.ShopContext, "actor_context", request.ActorContext, "reason", err)
		// 婚姻相关商品（戒指升级券 / 婚房装饰）有自己的原生失败码语义
		// （1=条件不符、0x0A=金币不足），不能一律回通用 0x04。
		return s.sendCurrentNPCShopFailure(session, uint16(dnfenum.CmdPacketBuyItem), currentWeddingShopFailureCode(err))
	}
	if err := s.sendGameUpperRawClass(session, uint16(dnfenum.CmdPacketBuyItem), buildCurrentNPCShopBuySuccessBody(result), dnfproto.DefaultChannelClassification); err != nil {
		return err
	}
	if err := s.sendGameUpperRawClass(session, uint16(dnfenum.CmdPacketWalkoutPartyMember), buildCurrentItemUpdateBody(dnfrepo.MainInventoryListType, result.Updates), 0); err != nil {
		return err
	}
	if result.ItemListType != dnfrepo.MainInventoryListType && len(result.ItemUpdates) > 0 {
		// Guild medals / guardian gems live in list 38. Their rows must be
		// announced under that list byte or the client files them into the
		// list-0 equipment/material pages where they cannot be used.
		if err := s.sendGameUpperRawClass(session, uint16(dnfenum.CmdPacketWalkoutPartyMember), buildCurrentItemUpdateBody(result.ItemListType, result.ItemUpdates), 0); err != nil {
			return err
		}
	}
	if err := s.refreshRuntimeRaidEntryCostAfterPurchase(session, result.ItemID); err != nil {
		return err
	}
	// 婚姻商品的副作用在 ACK 与增量行之后发布：婚房装饰要靠 1011/1008 让客户端
	// 重算装饰加成，戒指升级要靠 publishWeddingCommit(9) 推心意点/戒指状态。
	// 两者都读已落库的结果，所以前面的通知失败不会造成二次扣费。
	if result.WeddingRoom != nil {
		if err := s.publishWeddingRoom(*result.WeddingRoom, 21); err != nil {
			return err
		}
	}
	if result.WeddingPurchase != nil {
		if err := s.publishWeddingCommit(*result.WeddingPurchase, 9); err != nil {
			return err
		}
	}
	s.logGameEvent(session, "game-npc-shop-buy-committed", "item_id", result.ItemID, "count", result.Count, "list_type", result.ItemListType, "slot", result.Slot, "gold_after", result.GoldAfter, "incremental_entries", len(result.Updates)+len(result.ItemUpdates), "ack_body", "current_exe_sub_1D27590", "item_update_body", "current_exe_class0_op14_raw77")
	return nil
}

func (s *Service) handleCurrentNPCShopPurchaseCount(session *gameSession, body []byte) error {
	shopID, err := parseCurrentNPCShopPurchaseCountRequest(body)
	if err != nil {
		s.logGameEvent(session, "game-npc-shop-purchase-count-rejected", "body_len", len(body), "reason", err)
		return s.sendCurrentNPCShopFailure(session, uint16(dnfenum.CmdPacketShopPurchaseCount), currentNPCShopBuyErrorCode)
	}
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	lists := s.currentNPCShopPurchaseCountLists(ctx, session, shopID)
	total := 0
	for index := range lists {
		total += len(lists[index])
	}
	shopItems := 0
	if catalog, catalogErr := s.currentNPCShopCatalog(); catalogErr == nil && catalog != nil {
		shopItems = len(catalog.shopItems[shopID])
	}
	s.logGameEvent(session, "game-npc-shop-purchase-count", "shop_id", shopID, "body_len", len(body),
		"shop_items", shopItems,
		"limited_entries", total,
		"clock_offset_hours", currentPurchaseLimitClockOffset().Hours(),
		"maps", currentPurchaseLimitListsSummary(lists),
		"account_permanent", len(lists[currentPurchaseLimitListAccountPermanent]),
		"account_periodic", len(lists[currentPurchaseLimitListAccountPeriodic]),
		"character_permanent", len(lists[currentPurchaseLimitListCharacterPermanent]),
		"character_periodic", len(lists[currentPurchaseLimitListCharacterPeriodic]))
	return s.sendGameUpperRawClass(session, uint16(dnfenum.CmdPacketShopPurchaseCount), buildCurrentNPCShopPurchaseCountMapsBody(lists), dnfproto.DefaultChannelClassification)
}

func (s *Service) handleCurrentNPCShopSell(session *gameSession, body []byte) error {
	request, err := parseCurrentNPCShopSellRequest(body)
	if err != nil {
		s.logGameEvent(session, "game-npc-shop-sell-rejected", "body_len", len(body), "reason", err)
		return s.sendCurrentNPCShopFailure(session, uint16(dnfenum.CmdPacketSellItem), currentNPCShopSellErrorCode)
	}
	shop, err := s.currentNPCShopCatalog()
	if err != nil {
		s.logGameEvent(session, "game-npc-shop-sell-rejected", "list_type", request.ListType, "slot", request.Slot, "reason", err)
		return s.sendCurrentNPCShopFailure(session, uint16(dnfenum.CmdPacketSellItem), currentNPCShopSellErrorCode)
	}
	items, err := s.currentPVFItemCatalog()
	if err != nil {
		s.logGameEvent(session, "game-npc-shop-sell-rejected", "list_type", request.ListType, "slot", request.Slot, "reason", err)
		return s.sendCurrentNPCShopFailure(session, uint16(dnfenum.CmdPacketSellItem), currentNPCShopSellErrorCode)
	}
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	result, err := s.commitCurrentNPCShopSell(ctx, session, shop, items, request)
	if err != nil {
		s.logGameEvent(session, "game-npc-shop-sell-rejected", "list_type", request.ListType, "slot", request.Slot, "count", request.Count, "actor_context", request.ActorContext, "reason", err)
		// A locked item must not look like every other sell failure: the client
		// renders error code 214 as dstr 42232 「在物品已锁定状态下， 您无法进行当前
		// 操作。」, which is the same code the upgrade, private-store and cargo paths
		// already use. Anything else keeps the generic sell code.
		// Live 2026-09-15 07:35: the rejection was correct but silent.
		if errors.Is(err, errCurrentNPCShopItemLocked) {
			return s.sendCurrentNPCShopFailure(session, uint16(dnfenum.CmdPacketSellItem), currentNPCShopSellErrorLocked)
		}
		return s.sendCurrentNPCShopFailure(session, uint16(dnfenum.CmdPacketSellItem), currentNPCShopSellErrorCode)
	}
	if err := s.sendGameUpperRawClass(session, uint16(dnfenum.CmdPacketSellItem), buildCurrentNPCShopSellSuccessBody(result), dnfproto.DefaultChannelClassification); err != nil {
		return err
	}
	if err := s.sendGameUpperRawClass(session, uint16(dnfenum.CmdPacketWalkoutPartyMember), buildCurrentItemUpdateBody(result.ListType, result.Updates), 0); err != nil {
		return err
	}
	// 卖掉晶体/灵魂仓库那一格（list 0, 354..365）之后补一次"仍占用的晶体/灵魂行 + 金币行"
	// 重绘：清空那一格走的是 (itemID=0, count=0) 的"空格"行（与拍卖行上架、摆摊售出同一
	// 写法），面板据此把那一格置 0，这一发顺手把其余行的数量也对齐。卖已经落库，这一发
	// 只是重绘，失败只记日志、不回失败包。
	if dnfrepo.IsAccountSharedInventorySlot(result.ListType, result.Slot) {
		if err := s.sendSelectedMainWalletsRefresh(session, "npc_shop_sell_account_shared_after_ack"); err != nil {
			s.logGameEvent(session, "game-npc-shop-sell-account-shared-refresh-failed",
				"list_type", result.ListType,
				"slot", result.Slot,
				"item_id", result.ItemID,
				"error", err)
		}
	}
	s.logGameEvent(session, "game-npc-shop-sell-committed", "item_id", result.ItemID, "list_type", result.ListType, "slot", result.Slot, "applied", result.Applied, "gold_after", result.GoldAfter, "incremental_entries", len(result.Updates), "account_shared", dnfrepo.IsAccountSharedInventorySlot(result.ListType, result.Slot), "ack_body", "current_exe_sub_1D130A0", "item_update_body", "current_exe_class0_op14_raw77_delete_minus_one")
	return nil
}

func (s *Service) sendCurrentNPCShopFailure(session *gameSession, opcode uint16, errorCode byte) error {
	return s.sendGameUpperRawClass(session, opcode, buildCurrentNPCShopFailureBody(errorCode), dnfproto.DefaultChannelClassification)
}
