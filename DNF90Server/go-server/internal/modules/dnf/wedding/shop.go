package wedding

import (
	"context"
	"errors"
	"sort"
	"time"

	repo "longheng.io/server/internal/modules/dnf/repository"
)

// LovePointMaterial is the PVF shop currency, not a physical inventory stack.
const LovePointMaterial = 10002606

var ErrShopLoveMissing = errors.New("wedding shop love points are insufficient")

type RingPurchase struct {
	ActorID, PartnerID, AccountID string
	Item                          Item
	Gold, Love                    int64
	Now                           time.Time
	Grant                         func(*repo.InventoryRecord, Item) error
}

// BuyRingTicket debits the buyer's gold and the reciprocal couple currency
// together with the real inventory grant. It does not upgrade either ring.
func BuyRingTicket(ctx context.Context, repos repo.Group, cat *Catalog, cmd RingPurchase) (Result, error) {
	if cat == nil || repos.CharacterTrade == nil || cmd.ActorID == "" || cmd.ActorID == cmd.PartnerID || cmd.AccountID == "" || cmd.Now.IsZero() || cmd.Item.Count != 1 || cmd.Gold < 0 || cmd.Gold > 0x7fffffff || cmd.Love <= 0 || cmd.Grant == nil {
		return Result{}, Invalid
	}
	known := false
	for _, ticket := range cat.RingTickets[1:] {
		known = known || ticket.ID == cmd.Item.ID
	}
	if !known {
		return Result{}, Invalid
	}
	ids := []string{cmd.ActorID, cmd.PartnerID}
	sort.Strings(ids)
	var result Result
	err := repos.CharacterTrade.WithinCharacterTrade(ctx, ids[0], ids[1], func(chars repo.CharacterRepository, bags repo.InventoryRepository) error {
		for i, id := range []string{cmd.ActorID, cmd.PartnerID} {
			c, found, err := chars.Load(ctx, id)
			if err != nil {
				return err
			}
			if !found {
				return Missing
			}
			result.Characters[i] = repo.CloneCharacter(c)
		}
		a, b := &result.Characters[0], &result.Characters[1]
		if a.AccountID != cmd.AccountID {
			return Invalid
		}
		if err := Validate(*a, *b, cat, 9, cmd.Now); err != nil {
			return err
		}
		if a.Stats["gold"] < cmd.Gold {
			return GoldMissing
		}
		if a.Stats["gold"] > 0x7fffffff {
			return Invalid
		}
		if a.Stats["wedding_love"] != b.Stats["wedding_love"] || a.Stats["wedding_love"] < cmd.Love || a.Stats["wedding_love"] > cat.MaxLove {
			return ErrShopLoveMissing
		}
		bag, found, err := bags.Load(ctx, cmd.ActorID)
		if err != nil {
			return err
		}
		if !found || bag.CharacterID != cmd.ActorID {
			return Invalid
		}
		bag = repo.CloneInventory(bag)
		if err := cmd.Grant(&bag, cmd.Item); err != nil {
			return err
		}
		a.Stats["gold"] -= cmd.Gold
		a.Stats["wedding_love"] -= cmd.Love
		b.Stats["wedding_love"] -= cmd.Love
		for _, c := range []*repo.CharacterRecord{a, b} {
			c.UpdatedAt = cmd.Now
			if err := repo.SaveCharacterFields(ctx, chars, *c, repo.CharacterFieldStats); err != nil {
				return err
			}
		}
		bag.UpdatedAt = cmd.Now
		if err := repo.SaveInventoryFields(ctx, bags, bag, repo.InventoryFieldSlots); err != nil {
			return err
		}
		result.Inventories[0], result.AssetsChanged = bag, true
		return nil
	})
	if err != nil {
		return Result{}, err
	}
	return result, nil
}
