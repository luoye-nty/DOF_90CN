package hellparty

import (
	"errors"
	"fmt"
	"sort"
)

var ErrPlanInvalid = errors.New("dnf hell party plan is invalid")

type MapEntry struct {
	GroupID uint32
	Rate    uint32
	Order   uint32
}

type ActorIdentity struct {
	Order uint32
	Group uint32
	Index uint16
}

type PlannedActor struct {
	Identity            ActorIdentity
	Code                uint32
	EntityType          uint8
	SuppressGroupReward bool
}

type Wave struct {
	Order           uint32
	GroupID         uint32
	Rate            uint32
	RewardRollCount uint32
	Actors          []PlannedActor
}

type Plan struct {
	GroupKey string
	Seed     uint64
	Waves    []Wave
}

type PlanOptions struct {
	GroupKey string
	Seed     uint64
	Entries  []MapEntry
	// SuppressGroupReward is resolved from the current actor script's
	// [hell monster] flag by the caller. A nil resolver means false.
	SuppressGroupReward func(code uint32, entityType uint8) bool
}

func BuildPlan(catalog *Catalog, options PlanOptions) (Plan, error) {
	if catalog == nil {
		return Plan{}, fmt.Errorf("%w: catalog is nil", ErrPlanInvalid)
	}
	key := normalizeKey(options.GroupKey)
	rule, ok := catalog.Difficulty(key)
	if !ok {
		return Plan{}, fmt.Errorf("%w: difficulty=%q", ErrPlanInvalid, key)
	}
	byOrder := make(map[uint32][]MapEntry)
	for _, entry := range options.Entries {
		if entry.GroupID == 0 || entry.Order == 0 {
			return Plan{}, fmt.Errorf("%w: entry=%+v", ErrPlanInvalid, entry)
		}
		group, exists := catalog.Group(entry.GroupID)
		if !exists || len(group.Actors[key]) == 0 {
			continue
		}
		byOrder[entry.Order] = append(byOrder[entry.Order], entry)
	}
	orders := make([]uint32, 0, len(byOrder))
	for order := range byOrder {
		orders = append(orders, order)
	}
	sort.Slice(orders, func(i, j int) bool { return orders[i] < orders[j] })
	plan := Plan{GroupKey: key, Seed: options.Seed}
	for _, order := range orders {
		entry := selectWeighted(byOrder[order], options.Seed, order)
		group, _ := catalog.Group(entry.GroupID)
		definitions := group.Actors[key]
		wave := Wave{Order: order, GroupID: entry.GroupID, Rate: entry.Rate, RewardRollCount: rule.RewardRollCount}
		for index, actor := range definitions {
			planned := PlannedActor{
				Identity:   ActorIdentity{Order: order, Group: entry.GroupID, Index: uint16(index)},
				Code:       actor.Code,
				EntityType: actor.EntityType,
			}
			if options.SuppressGroupReward != nil {
				planned.SuppressGroupReward = options.SuppressGroupReward(actor.Code, actor.EntityType)
			}
			wave.Actors = append(wave.Actors, planned)
		}
		if len(wave.Actors) > 0 {
			plan.Waves = append(plan.Waves, wave)
		}
	}
	if len(plan.Waves) == 0 {
		return Plan{}, fmt.Errorf("%w: no eligible waves for difficulty=%s", ErrPlanInvalid, key)
	}
	return plan, nil
}

func selectWeighted(entries []MapEntry, seed uint64, order uint32) MapEntry {
	if len(entries) == 1 {
		return entries[0]
	}
	var total uint64
	for _, entry := range entries {
		total += uint64(entry.Rate)
	}
	if total == 0 {
		return entries[0]
	}
	roll := mix64(seed^uint64(order)*0x9e3779b97f4a7c15) % total
	for _, entry := range entries {
		if roll < uint64(entry.Rate) {
			return entry
		}
		roll -= uint64(entry.Rate)
	}
	return entries[0]
}

func mix64(value uint64) uint64 {
	value ^= value >> 30
	value *= 0xbf58476d1ce4e5b9
	value ^= value >> 27
	value *= 0x94d049bb133111eb
	return value ^ (value >> 31)
}
