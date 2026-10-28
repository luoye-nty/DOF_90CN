package guild

import (
	"encoding/binary"
	"errors"
	"math"
	"strconv"
	"time"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

var (
	ErrCargoItem = errors.New("guild cargo item or count mismatch")
	ErrCargoFull = errors.New("guild cargo destination is full")
)

type CargoHistory struct {
	Name      string            `json:"name"`
	Action    byte              `json:"action"`
	Item      dnfrepo.ItemStack `json:"item"`
	CreatedAt time.Time         `json:"created_at"`
}

// CargoRule is resolved from the active PVF and the persisted source item.
type CargoRule struct {
	ItemID             uint32
	Equipment          bool
	Allowed            bool
	StackLimit         int64
	SlotStart, SlotEnd int16
}

func (g *Guild) TransferCargo(characterID string, inventory *dnfrepo.InventoryRecord, deposit bool, bagSlot, cargoSlot int16, itemID uint32, count uint32, rule CargoRule, now time.Time) (int16, error) {
	member, found := g.Members[characterID]
	if !found {
		return 0, ErrNotMember
	}
	if inventory == nil || inventory.CharacterID != characterID || !g.validCargoSlot(cargoSlot) || count == 0 || count > math.MaxInt32 {
		return 0, ErrCargoItem
	}
	if !rule.Allowed {
		return 0, ErrPermissionDenied
	}
	if rule.ItemID != itemID || rule.SlotStart < 3 || rule.SlotEnd < rule.SlotStart || rule.StackLimit < 0 {
		return 0, ErrCargoItem
	}
	source := g.Cargo[cargoSlot]
	if deposit {
		source = inventory.Slots[cargoBagKey(bagSlot)]
		if bagSlot < rule.SlotStart || bagSlot > rule.SlotEnd {
			// Native main-inventory consumable quick slots are3..8.
			if rule.Equipment || bagSlot < 3 || bagSlot > 8 {
				return 0, ErrCargoItem
			}
		}
	}
	amount := int64(count)
	if source.ItemID != int64(itemID) || source.Count < amount || source.Count > math.MaxInt32 || (rule.Equipment && (source.Count != 1 || count != 1)) {
		return 0, ErrCargoItem
	}
	limit := rule.StackLimit
	if limit == 0 || limit > math.MaxInt32 {
		limit = math.MaxInt32
	}
	canMerge := func(target dnfrepo.ItemStack) bool {
		return !rule.Equipment && target.ItemID == source.ItemID && target.Count > 0 &&
			target.Bind == source.Bind && target.ExpireAt.Equal(source.ExpireAt) && target.Count <= limit-amount
	}
	if amount > limit {
		return 0, ErrCargoFull
	}
	if !deposit {
		bagSlot = -1
		for slot := rule.SlotStart; slot <= rule.SlotEnd; slot++ {
			if canMerge(inventory.Slots[cargoBagKey(slot)]) {
				bagSlot = slot
				break
			}
		}
		if bagSlot == -1 {
			for slot := rule.SlotStart; slot <= rule.SlotEnd; slot++ {
				if _, occupied := inventory.Slots[cargoBagKey(slot)]; !occupied {
					bagSlot = slot
					break
				}
			}
		}
		if bagSlot == -1 {
			return 0, ErrCargoFull
		}
	}
	key := cargoBagKey(bagSlot)
	target, occupied := inventory.Slots[key]
	if deposit {
		target, occupied = g.Cargo[cargoSlot]
	}
	if occupied && !canMerge(target) {
		return 0, ErrCargoFull
	}
	if !occupied {
		target = source
		target.Count = 0
	}
	target = cargoCount(target, target.Count+amount, rule.Equipment)
	moved := cargoCount(source, amount, rule.Equipment)
	source = cargoCount(source, source.Count-amount, rule.Equipment)
	if g.Cargo == nil {
		g.Cargo = make(map[int16]dnfrepo.ItemStack)
	}
	if inventory.Slots == nil {
		inventory.Slots = make(map[string]dnfrepo.ItemStack)
	}
	action := byte(2)
	if deposit {
		action = 1
		g.Cargo[cargoSlot] = target
		if source.Count == 0 {
			delete(inventory.Slots, key)
		} else {
			inventory.Slots[key] = source
		}
	} else {
		inventory.Slots[key] = target
		if source.Count == 0 {
			delete(g.Cargo, cargoSlot)
		} else {
			g.Cargo[cargoSlot] = source
		}
	}
	g.CargoHistory = append(g.CargoHistory, CargoHistory{Name: member.Name, Action: action, Item: moved, CreatedAt: now})
	// ponytail: retain the latest200 operations in the guild aggregate; use a
	// separate paginated repository if an archival transaction log is needed.
	if len(g.CargoHistory) > 200 {
		g.CargoHistory = g.CargoHistory[len(g.CargoHistory)-200:]
	}
	g.UpdatedAt, inventory.UpdatedAt = now, now
	return bagSlot, nil
}

func (g *Guild) MoveCargo(characterID string, source, destination int16, sourceID, destinationID uint32, now time.Time) error {
	if _, found := g.Members[characterID]; !found {
		return ErrNotMember
	}
	if source == destination || !g.validCargoSlot(source) || !g.validCargoSlot(destination) || sourceID == 0 {
		return ErrCargoItem
	}
	a, aOK := g.Cargo[source]
	b, bOK := g.Cargo[destination]
	if !aOK || a.Count <= 0 || a.ItemID != int64(sourceID) || b.ItemID != int64(destinationID) || (bOK && b.Count <= 0) {
		return ErrCargoItem
	}
	g.Cargo[destination] = a
	if bOK {
		g.Cargo[source] = b
	} else {
		delete(g.Cargo, source)
	}
	g.UpdatedAt = now
	return nil
}

func (g *Guild) validCargoSlot(slot int16) bool {
	return slot >= 0 && int(slot) < g.CargoLimit && int(slot) < MaxCargoLimit
}

func cargoBagKey(slot int16) string { return "0:" + strconv.Itoa(int(slot)) }

func cargoCount(stack dnfrepo.ItemStack, count int64, equipment bool) dnfrepo.ItemStack {
	stack.Count = count
	if equipment {
		return stack
	}
	if len(stack.RawEntry) == 119 {
		stack.RawEntry = append([]byte(nil), stack.RawEntry...)
		binary.LittleEndian.PutUint32(stack.RawEntry[6:10], uint32(count))
	}
	extra := make(map[string]string, len(stack.Extra))
	for key, value := range stack.Extra {
		extra[key] = value
	}
	for _, key := range []string{"amount_or_count", "amount", "count", "stack", "quantity"} {
		if _, exists := extra[key]; exists {
			extra[key] = strconv.FormatInt(count, 10)
		}
	}
	stack.Extra = extra
	return stack
}
