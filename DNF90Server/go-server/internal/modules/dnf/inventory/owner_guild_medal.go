package inventory

import (
	"context"
	"fmt"
	"time"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

type GuildItemCompoundCommand struct {
	CharacterID   string
	GuardianGem   bool
	Slots         []int16
	Rarity        int64
	Now           time.Time
	ResolveRarity func(int64) (int64, error)
	CreateResult  func(int16) (dnfrepo.ItemStack, error)
}

func (o *Owner) CompoundGuildItems(ctx context.Context, cmd GuildItemCompoundCommand) (dnfrepo.ItemStack, error) {
	if o == nil || o.repo == nil || cmd.CharacterID == "" || cmd.ResolveRarity == nil || cmd.CreateResult == nil || len(cmd.Slots) < 2 || len(cmd.Slots) > 4 {
		return dnfrepo.ItemStack{}, fmt.Errorf("invalid guild item compound command")
	}
	var output dnfrepo.ItemStack
	err := o.withinInventoryTransaction(ctx, cmd.CharacterID, func(owner *Owner) error {
		record, found, err := owner.repo.Load(ctx, cmd.CharacterID)
		if err != nil {
			return err
		}
		if !found {
			return ErrInventoryNotFound
		}
		record = dnfrepo.CloneInventory(record)
		start, end := int16(0), int16(48)
		if cmd.GuardianGem {
			start, end = 49, 97
		}
		used := make(map[int16]int64, len(cmd.Slots))
		for _, slot := range cmd.Slots {
			if slot < start || slot > end || (!cmd.GuardianGem && used[slot] != 0) {
				return fmt.Errorf("invalid or duplicate guild item slot %d", slot)
			}
			used[slot]++
			stack, found := record.Slots[slotKey(listTypeGuildMedal, slot)]
			if !found || stack.Count < used[slot] || (!cmd.GuardianGem && stack.Count != 1) || isEquipmentLocked(stack) || (!stack.ExpireAt.IsZero() && !stack.ExpireAt.After(cmd.Now)) {
				return fmt.Errorf("guild item slot %d missing, short, locked or expired", slot)
			}
			rarity, err := cmd.ResolveRarity(stack.ItemID)
			if err != nil {
				return err
			}
			if rarity != cmd.Rarity {
				return fmt.Errorf("guild item rarity mismatch at slot %d", slot)
			}
		}
		for slot, count := range used {
			key := slotKey(listTypeGuildMedal, slot)
			stack := record.Slots[key]
			stack.Count -= count
			if stack.Count == 0 {
				delete(record.Slots, key)
			} else {
				updateStackRawAmount(&stack)
				record.Slots[key] = stack
			}
		}
		outputSlot := cmd.Slots[0]
		if cmd.GuardianGem {
			for outputSlot = start; outputSlot <= end; outputSlot++ {
				if _, occupied := record.Slots[slotKey(listTypeGuildMedal, outputSlot)]; !occupied {
					break
				}
			}
			if outputSlot > end {
				return fmt.Errorf("guardian gem page is full")
			}
		}
		output, err = cmd.CreateResult(outputSlot)
		if err != nil {
			return err
		}
		if output.ItemID <= 0 || output.Count != 1 {
			return fmt.Errorf("invalid guild compound output")
		}
		record.Slots[slotKey(listTypeGuildMedal, outputSlot)] = output
		record.UpdatedAt = cmd.Now
		return dnfrepo.SaveInventoryFields(ctx, owner.repo, record, dnfrepo.InventoryFieldSlots)
	})
	if err != nil {
		return dnfrepo.ItemStack{}, err
	}
	return output, nil
}
