package dnfbridge

import (
	"context"
	"errors"
	"strconv"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	"longheng.io/server/internal/modules/dnf/wedding"
)

func (s *Service) commitWeddingRingBuy(ctx context.Context, session *gameSession, pricing currentNPCShopPricing, request currentNPCShopBuyRequest) (currentNPCShopBuyResult, error) {
	identity, current := s.boundGameSessionCharacterSnapshot(session)
	if !current {
		return currentNPCShopBuyResult{}, wedding.Invalid
	}
	s.weddingMu.Lock()
	defer s.weddingMu.Unlock()
	c, err := s.weddingCharacter(ctx, identity)
	if err != nil {
		return currentNPCShopBuyResult{}, err
	}
	state, err := wedding.Read(c)
	if err != nil {
		return currentNPCShopBuyResult{}, err
	}
	cat, err := s.currentWeddingCatalog()
	if err != nil {
		return currentNPCShopBuyResult{}, err
	}
	var entry currentItemListEntry
	var slot int16
	repos, _ := s.repositoryGroup()
	result, err := wedding.BuyRingTicket(ctx, repos, cat, wedding.RingPurchase{
		ActorID: c.CharacterID, PartnerID: strconv.Itoa(int(state.PartnerID)), AccountID: s.accountIDForSession(session),
		Item: wedding.Item{ID: int64(request.ItemID), Count: int64(request.Count)}, Gold: pricing.BuyGold, Love: pricing.NeedMaterialCount, Now: s.gameplayNow(),
		Grant: func(bag *dnfrepo.InventoryRecord, item wedding.Item) error {
			slots, grantErr := s.grantCurrentWeddingMainProduct(bag, pricing.Definition, uint32(item.Count))
			if grantErr != nil {
				return grantErr
			}
			if len(slots) != 1 {
				return errCurrentNPCShopProductUnavailable
			}
			slot = int16(slots[0])
			key := inventorySlotKey(0, slot)
			stack := bag.Slots[key]
			if stack.Extra == nil {
				stack.Extra = make(map[string]string)
			}
			stack.Extra["last_grant_source"] = "wedding_ring_shop"
			entry = currentItemListEntryFromStack(0, slot, stack)
			stack.RawEntry = append([]byte(nil), entry.data[:]...)
			bag.Slots[key] = stack
			return nil
		},
	})
	if err != nil {
		return currentNPCShopBuyResult{}, err
	}
	gold := result.Characters[0].Stats["gold"]
	updates := []currentItemListEntry{currentGoldWalletItemListEntry(gold), entry}
	sortCurrentItemListEntries(updates)
	// 工作区没有 dnfpvp.EmblemsKey（PVP 徽章钱包），商城主流程也不回该字段，
	// 故这里不设 EmblemsAfter，由通用商店路径自行刷新其余货币。
	// 上游的 ListType: 0 也不需要写：戒指券落在主背包，而工作区的 Updates 本就
	// 固定按 MainInventoryListType(0) 下发，ItemListType 零值即主背包。
	return currentNPCShopBuyResult{
		WeddingPurchase: &result, GoldAfter: gold,
		CoinAfter: s.loadCurrentAccountCera(ctx, repos, session), Item: entry, Updates: updates, Slot: slot, ItemID: request.ItemID, Count: request.Count,
	}, nil
}

func currentWeddingShopFailureCode(err error) byte {
	// sub_1D27590 and dstr.dat: 1 -> purchase conditions not met;
	// 0x0A -> insufficient gold. 0xCB means wedding cargo full, not love.
	if errors.Is(err, wedding.ErrShopLoveMissing) {
		return 1
	}
	if errors.Is(err, wedding.GoldMissing) {
		return 0x0A
	}
	var stateError wedding.Error
	if errors.As(err, &stateError) {
		return 1
	}
	return currentNPCShopBuyErrorCode
}
