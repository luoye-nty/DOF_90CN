package dnfbridge

import (
	"context"
	"errors"
	"fmt"
	"time"

	dnfdungeon "longheng.io/server/internal/modules/dnf/dungeon"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

var (
	errDungeonCardAssetOwnerUnavailable = dnfdungeon.ErrOwnerUnavailable
	errDungeonCardInventoryFull         = dnfdungeon.ErrInventoryFull
	errDungeonCardGoldOverflow          = dnfdungeon.ErrGoldOverflow
)

type dungeonCardAssetGrantResult struct {
	GoldBefore     int64
	GoldAfter      int64
	ItemSlots      []int16
	OverflowMailID string
	Replayed       bool
}

// grantDungeonCardRewardBundle atomically changes the real character gold
// mirror and main inventory aggregate. mainSlotCount must come from the real
// character container state; this function never invents a bag capacity.
func grantDungeonCardRewardBundle(
	ctx context.Context,
	repositories dnfrepo.Group,
	characterID string,
	mainSlotCount uint16,
	bundle dungeonCardRewardBundle,
	now time.Time,
) (dungeonCardAssetGrantResult, error) {
	return grantDungeonCardRewardBundleWithCompletionKey(
		ctx, repositories, characterID, mainSlotCount, bundle, now, "", 0,
	)
}

func grantDungeonCardRewardBundleWithCompletionKey(
	ctx context.Context,
	repositories dnfrepo.Group,
	characterID string,
	mainSlotCount uint16,
	bundle dungeonCardRewardBundle,
	now time.Time,
	completionKey string,
	// cost is the gold the flip spends before the bundle is credited. It is
	// non-zero only for the paid row: 86JP charges GetPaidGoldCost(run) there
	// and refuses the flip outright when the character cannot pay it.
	cost int64,
) (dungeonCardAssetGrantResult, error) {
	if repositories.CharacterAssets == nil || characterID == "" {
		return dungeonCardAssetGrantResult{}, errDungeonCardAssetOwnerUnavailable
	}
	owner, err := dnfdungeon.NewOwner(repositories)
	if err != nil {
		return dungeonCardAssetGrantResult{}, err
	}
	items := make([]dnfdungeon.CardItemReward, 0, len(bundle.Items))
	for _, reward := range bundle.Items {
		items = append(items, dnfdungeon.CardItemReward{
			Stack: dnfrepo.ItemStack{
				ItemID:   reward.ItemID,
				Count:    reward.Count,
				Bind:     reward.Bind,
				RawEntry: append([]byte(nil), reward.RawEntry...),
				Extra:    cloneDungeonCardStringMap(reward.Extra),
			},
			Stackable:  reward.Stackable,
			StackLimit: reward.StackLimit,
			SlotStart:  reward.SlotStart,
			SlotEnd:    currentMainInventoryClampedSlotEnd(reward.SlotStart, reward.SlotEnd, mainSlotCount),
			ExpireAt:   reward.ExpireAt,
		})
	}
	result, err := owner.GrantCardReward(ctx, dnfdungeon.CardRewardCommand{
		CharacterID:   characterID,
		CompletionKey: completionKey,
		MainSlots:     mainSlotCount,
		Cost:          cost,
		Bundle: dnfdungeon.CardRewardBundle{
			Gold:  bundle.Gold,
			Items: items,
		},
		UpdatedAt:           now,
		ConsumePremiumDaily: bundle.ConsumePremiumDaily,
		PremiumDailySlot:    bundle.PremiumDailySlot,
		Project: func(stack dnfrepo.ItemStack, expiration time.Time) (dnfrepo.ItemStack, error) {
			stack, _ = applyCurrentStackableExpirationAt(stack, expiration, now)
			return stack, nil
		},
	})
	if err != nil {
		switch {
		case errors.Is(err, dnfdungeon.ErrCharacterNotFound):
			return dungeonCardAssetGrantResult{}, errDungeonCardAssetOwnerUnavailable
		case errors.Is(err, dnfdungeon.ErrRewardInvalid),
			errors.Is(err, dnfdungeon.ErrStackProjectorRequired):
			return dungeonCardAssetGrantResult{}, fmt.Errorf("%w: %v", errDungeonCardPlanInvalid, err)
		}
		return dungeonCardAssetGrantResult{}, err
	}
	return dungeonCardAssetGrantResult{
		GoldBefore:     result.GoldBefore,
		GoldAfter:      result.GoldAfter,
		ItemSlots:      result.ItemSlots,
		OverflowMailID: result.OverflowMailID,
		Replayed:       result.Replayed,
	}, nil
}

func deliverDungeonCardReservation(
	ctx context.Context,
	state *dungeonCardState,
	reservation dungeonCardDeliveryReservation,
	repositories dnfrepo.Group,
	mainSlotCount uint16,
	now time.Time,
) (dungeonCardAssetGrantResult, error) {
	return deliverDungeonCardReservationWithCompletionKey(
		ctx, state, reservation, repositories, mainSlotCount, now, "",
	)
}

func deliverDungeonCardReservationWithCompletionKey(
	ctx context.Context,
	state *dungeonCardState,
	reservation dungeonCardDeliveryReservation,
	repositories dnfrepo.Group,
	mainSlotCount uint16,
	now time.Time,
	completionKey string,
) (dungeonCardAssetGrantResult, error) {
	if state == nil || !reservation.grant {
		return dungeonCardAssetGrantResult{}, nil
	}
	// Only the paid row is charged. The price was frozen with the plan, so the
	// amount spent here is exactly the commission op35 printed for this card.
	cost := int64(0)
	if reservation.side == dungeonCardSidePaid {
		cost = int64(state.plan.PaidCardCost)
	}
	result, err := grantDungeonCardRewardBundleWithCompletionKey(
		ctx,
		repositories,
		state.plan.CharacterID,
		mainSlotCount,
		reservation.bundle,
		now,
		completionKey,
		cost,
	)
	state.finishDelivery(reservation, err == nil)
	return result, err
}
