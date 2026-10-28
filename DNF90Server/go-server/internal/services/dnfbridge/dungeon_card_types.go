package dnfbridge

import (
	"errors"
	"fmt"
	"math"
	"sync"
	"time"
)

const (
	dungeonCardSideCount     = 2
	dungeonCardSlotsPerSide  = 4
	dungeonCardWireSlotCount = dungeonCardSideCount * dungeonCardSlotsPerSide
)

var (
	errDungeonCardPlanInvalid       = errors.New("dungeon card reward plan is invalid")
	errDungeonCardSelectionInvalid  = errors.New("dungeon card selection is invalid")
	errDungeonCardSelectionConflict = errors.New("dungeon card selection conflicts with the accepted selection")
)

type dungeonCardSide uint8

const (
	dungeonCardSideFree dungeonCardSide = iota
	dungeonCardSidePaid
)

type dungeonCardItemReward struct {
	ItemID     int64
	Count      int64
	Stackable  bool
	StackLimit int64
	Bind       bool
	SlotStart  int16
	SlotEnd    int16
	ExpireAt   time.Time
	RawEntry   []byte
	Extra      map[string]string
}

type dungeonCardRewardBundle struct {
	Gold                int64
	Items               []dungeonCardItemReward
	ConsumePremiumDaily bool
	PremiumDailySlot    int64
}

type dungeonCardDisplayItem struct {
	ItemID int64
	Count  int64
}

type dungeonCardRewardPlan struct {
	ID          string
	CharacterID string
	Source      string
	Sides       [dungeonCardSideCount]dungeonCardRewardBundle
	// BlackDiamond is the current client's second op35 extension group. One
	// immutable bonus bundle belongs to each free-card party seat (0..3).
	BlackDiamond [dungeonCardSlotsPerSide]dungeonCardRewardBundle
	// PaidCardCost is the gold the paid row charges when it is flipped, and the
	// same number op35 prints as the card's commission. 86JP keeps one value per
	// run (DungeonRun.PaidCardCost) for both uses; freezing it beside the plan
	// keeps the printed price and the charged price in step across a replay.
	PaidCardCost uint32
}

type dungeonCardPlanIdentity struct {
	CharacterID string
	DungeonID   int64
	MazeIndex   int
	RunSeed     uint32
}

type dungeonCardSelectionResult uint8

const (
	dungeonCardSelectionAccepted dungeonCardSelectionResult = iota
	dungeonCardSelectionReplay
)

type dungeonCardDeliveryReservation struct {
	side   dungeonCardSide
	bundle dungeonCardRewardBundle
	grant  bool
}

// dungeonCardState owns one run's immutable reward plan and the mutable
// selection/delivery gates. It deliberately does not own timers or town exit.
type dungeonCardState struct {
	mu sync.Mutex

	plan dungeonCardRewardPlan

	selectedMember [dungeonCardSideCount]int8
	deliveryBusy   [dungeonCardSideCount]bool
	delivered      [dungeonCardSideCount]bool
}

func newDungeonCardState(plan dungeonCardRewardPlan) (*dungeonCardState, error) {
	if err := validateDungeonCardPlan(plan); err != nil {
		return nil, err
	}
	return &dungeonCardState{
		plan:           cloneDungeonCardRewardPlan(plan),
		selectedMember: [dungeonCardSideCount]int8{-1, -1},
	}, nil
}

func (s *dungeonCardState) reserveSelection(side dungeonCardSide, memberIndex uint8) (dungeonCardDeliveryReservation, dungeonCardSelectionResult, error) {
	if s == nil || side >= dungeonCardSideCount || memberIndex >= dungeonCardSlotsPerSide {
		return dungeonCardDeliveryReservation{}, 0, errDungeonCardSelectionInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	accepted := s.selectedMember[side]
	if accepted >= 0 {
		if uint8(accepted) != memberIndex {
			return dungeonCardDeliveryReservation{}, 0, errDungeonCardSelectionConflict
		}
		reservation := dungeonCardDeliveryReservation{side: side}
		if !s.delivered[side] && !s.deliveryBusy[side] {
			s.deliveryBusy[side] = true
			reservation.bundle = cloneDungeonCardRewardBundle(s.plan.Sides[side])
			if side == dungeonCardSideFree {
				reservation.bundle = mergeDungeonCardRewardBundles(reservation.bundle, s.plan.BlackDiamond[memberIndex])
			}
			reservation.grant = true
		}
		return reservation, dungeonCardSelectionReplay, nil
	}
	s.selectedMember[side] = int8(memberIndex)

	reservation := dungeonCardDeliveryReservation{side: side}
	if !s.delivered[side] && !s.deliveryBusy[side] {
		s.deliveryBusy[side] = true
		reservation.bundle = cloneDungeonCardRewardBundle(s.plan.Sides[side])
		if side == dungeonCardSideFree {
			reservation.bundle = mergeDungeonCardRewardBundles(reservation.bundle, s.plan.BlackDiamond[memberIndex])
		}
		reservation.grant = true
	}
	return reservation, dungeonCardSelectionAccepted, nil
}

func (s *dungeonCardState) finishDelivery(reservation dungeonCardDeliveryReservation, committed bool) {
	if s == nil || !reservation.grant || reservation.side >= dungeonCardSideCount {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deliveryBusy[reservation.side] = false
	if committed {
		s.delivered[reservation.side] = true
	}
}

func (s *dungeonCardState) deliveryStatus(side dungeonCardSide) (selected int8, delivered bool, busy bool) {
	if s == nil || side >= dungeonCardSideCount {
		return -1, false, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.selectedMember[side], s.delivered[side], s.deliveryBusy[side]
}

func validateDungeonCardPlan(plan dungeonCardRewardPlan) error {
	if plan.ID == "" || plan.CharacterID == "" || plan.Source == "" {
		return errDungeonCardPlanInvalid
	}
	for side := range plan.Sides {
		if err := validateDungeonCardRewardBundle(plan.Sides[side], fmt.Sprintf("side=%d", side)); err != nil {
			return err
		}
	}
	for member := range plan.BlackDiamond {
		if err := validateDungeonCardRewardBundle(plan.BlackDiamond[member], fmt.Sprintf("black-diamond-member=%d", member)); err != nil {
			return err
		}
	}
	return nil
}

func validateDungeonCardRewardBundle(bundle dungeonCardRewardBundle, owner string) error {
	if bundle.Gold < 0 {
		return fmt.Errorf("%w: %s gold=%d", errDungeonCardPlanInvalid, owner, bundle.Gold)
	}
	for index, item := range bundle.Items {
		if item.ItemID <= 0 || item.Count <= 0 ||
			item.SlotStart < currentDungeonPickupQuickSlotStart ||
			item.SlotEnd < item.SlotStart ||
			len(item.RawEntry) != 0 && len(item.RawEntry) != currentItemListEntryWireSize {
			return fmt.Errorf(
				"%w: %s item=%d id=%d count=%d slots=%d..%d raw=%d",
				errDungeonCardPlanInvalid,
				owner,
				index,
				item.ItemID,
				item.Count,
				item.SlotStart,
				item.SlotEnd,
				len(item.RawEntry),
			)
		}
		if !item.Stackable && item.Count != 1 {
			return fmt.Errorf("%w: %s item=%d non-stackable-count=%d", errDungeonCardPlanInvalid, owner, index, item.Count)
		}
	}
	return nil
}

func cloneDungeonCardRewardPlan(plan dungeonCardRewardPlan) dungeonCardRewardPlan {
	for side := range plan.Sides {
		plan.Sides[side] = cloneDungeonCardRewardBundle(plan.Sides[side])
	}
	for member := range plan.BlackDiamond {
		plan.BlackDiamond[member] = cloneDungeonCardRewardBundle(plan.BlackDiamond[member])
	}
	return plan
}

func mergeDungeonCardRewardBundles(first, second dungeonCardRewardBundle) dungeonCardRewardBundle {
	merged := cloneDungeonCardRewardBundle(first)
	if second.Gold > 0 {
		if merged.Gold > math.MaxInt64-second.Gold {
			merged.Gold = math.MaxInt64
		} else {
			merged.Gold += second.Gold
		}
	}
	for _, item := range second.Items {
		merged.Items = append(merged.Items, cloneDungeonCardItemReward(item))
	}
	return merged
}

func cloneDungeonCardRewardBundle(bundle dungeonCardRewardBundle) dungeonCardRewardBundle {
	bundle.Items = append([]dungeonCardItemReward(nil), bundle.Items...)
	for index := range bundle.Items {
		bundle.Items[index] = cloneDungeonCardItemReward(bundle.Items[index])
	}
	return bundle
}

func cloneDungeonCardItemReward(item dungeonCardItemReward) dungeonCardItemReward {
	item.RawEntry = append([]byte(nil), item.RawEntry...)
	item.Extra = cloneDungeonCardStringMap(item.Extra)
	return item
}

// currentDungeonCardDisplayItems combines physical reward instances with the
// same template for op35/op71. Equipment remains separate in the durable
// bundle, while the current EXE sees the one card face and its x2 quantity.
func currentDungeonCardDisplayItems(bundle dungeonCardRewardBundle) []dungeonCardDisplayItem {
	items := make([]dungeonCardDisplayItem, 0, len(bundle.Items))
	indexByItemID := make(map[int64]int, len(bundle.Items))
	for _, item := range bundle.Items {
		if item.ItemID <= 0 || item.Count <= 0 {
			continue
		}
		if index, found := indexByItemID[item.ItemID]; found {
			if items[index].Count > math.MaxInt64-item.Count {
				items[index].Count = math.MaxInt64
			} else {
				items[index].Count += item.Count
			}
			continue
		}
		indexByItemID[item.ItemID] = len(items)
		items = append(items, dungeonCardDisplayItem{
			ItemID: item.ItemID,
			Count:  item.Count,
		})
	}
	return items
}

func cloneDungeonCardStringMap(values map[string]string) map[string]string {
	if len(values) == 0 {
		return nil
	}
	out := make(map[string]string, len(values))
	for key, value := range values {
		out[key] = value
	}
	return out
}
