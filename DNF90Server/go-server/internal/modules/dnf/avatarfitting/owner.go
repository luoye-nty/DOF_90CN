package avatarfitting

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

const (
	PartCount           = 8
	settingsScopeSuffix = ":avatar_fitting_room"
	settingsUsedParts   = "used_parts_total"
)

var (
	ErrOwnerUnavailable  = errors.New("dnf avatar fitting room owner is unavailable")
	ErrCharacterRequired = errors.New("dnf avatar fitting room character is required")
	ErrCharacterNotFound = errors.New("dnf avatar fitting room character was not found")
	ErrAccountMismatch   = errors.New("dnf avatar fitting room account does not match")
	ErrTownRequired      = errors.New("dnf avatar fitting room requires town state")
	ErrRequestInvalid    = errors.New("dnf avatar fitting room request is invalid")
	ErrEquipmentMismatch = errors.New("dnf avatar fitting room equipped avatar does not match")
	ErrTicketMissing     = errors.New("dnf avatar fitting room ticket is missing")
	ErrRewardSpace       = errors.New("dnf avatar fitting room reward inventory space is unavailable")
)

// Change is one of the eight current-client avatar fitting-room rows. The
// durable equipment slot is server-owned; ObjectIndex is echoed only after a
// successful commit so the client can rebind the already refreshed object.
type Change struct {
	EquipmentSlot int16
	ObjectIndex   int32
	CurrentItemID int64
	OutputItemID  int64
	PVFPath       string
	EquipmentType string
}

type Reward struct {
	ItemID           int64
	CountPerCrossing int64
	SlotStart        int16
	SlotEnd          int16
	StackLimit       int64
	BuildStack       func(slot int16, count int64) (dnfrepo.ItemStack, error)
}

type Command struct {
	AccountID      string
	CharacterID    string
	InDungeon      bool
	TicketItemID   int64
	Changes        [PartCount]Change
	GaugeThreshold int64
	Reward         *Reward
}

type Result struct {
	Changes        [PartCount]Change
	TicketSlots    []int16
	EquipmentSlots []int16
	RewardSlots    []int16
	TicketConsumed int64
	RewardGranted  int64
	GaugeBefore    int64
	GaugeAfter     int64
}

type Owner struct {
	settlements dnfrepo.CharacterSettlementUnitOfWork
}

func NewOwner(repositories dnfrepo.Group) (*Owner, error) {
	if repositories.CharacterSettlement == nil {
		return nil, ErrOwnerUnavailable
	}
	return &Owner{settlements: repositories.CharacterSettlement}, nil
}

// Commit atomically replaces all eligible worn avatar rows, consumes one
// fitting-room ticket per replaced row and advances the character-local VIP
// gauge. Equipment, inventory and gauge are never allowed to diverge.
func (o *Owner) Commit(ctx context.Context, command Command) (Result, error) {
	if o == nil || o.settlements == nil {
		return Result{}, ErrOwnerUnavailable
	}
	command.AccountID = strings.TrimSpace(command.AccountID)
	command.CharacterID = strings.TrimSpace(command.CharacterID)
	if command.AccountID == "" || command.CharacterID == "" || command.TicketItemID <= 0 {
		return Result{}, ErrCharacterRequired
	}
	if command.InDungeon {
		return Result{}, ErrTownRequired
	}
	var result Result
	err := o.settlements.WithinCharacterSettlement(ctx, command.CharacterID, func(tx dnfrepo.Group) error {
		if tx.Character == nil || tx.Inventory == nil || tx.Equipment == nil || tx.Settings == nil {
			return ErrOwnerUnavailable
		}
		character, found, err := tx.Character.Load(ctx, command.CharacterID)
		if err != nil {
			return err
		}
		if !found {
			return ErrCharacterNotFound
		}
		if strings.TrimSpace(character.AccountID) != command.AccountID {
			return ErrAccountMismatch
		}
		inventory, found, err := tx.Inventory.Load(ctx, command.CharacterID)
		if err != nil {
			return err
		}
		if !found {
			return ErrTicketMissing
		}
		equipment, found, err := tx.Equipment.Load(ctx, command.CharacterID)
		if err != nil {
			return err
		}
		if !found {
			return ErrEquipmentMismatch
		}
		inventory = dnfrepo.CloneInventory(inventory)
		equipment = dnfrepo.CloneEquipment(equipment)

		changed := make(map[int16]struct{}, PartCount)
		for index, change := range command.Changes {
			if change.EquipmentSlot < 0 {
				if change.ObjectIndex != -1 || change.CurrentItemID != -1 || change.OutputItemID != 0 {
					return fmt.Errorf("%w: absent part=%d", ErrRequestInvalid, index)
				}
				continue
			}
			if change.ObjectIndex < 0 || change.CurrentItemID <= 0 || change.OutputItemID <= 0 ||
				change.OutputItemID > math.MaxUint32 || strings.TrimSpace(change.PVFPath) == "" ||
				strings.TrimSpace(change.EquipmentType) == "" {
				return fmt.Errorf("%w: part=%d", ErrRequestInvalid, index)
			}
			if _, duplicate := changed[change.EquipmentSlot]; duplicate {
				return fmt.Errorf("%w: duplicate equipment slot=%d", ErrRequestInvalid, change.EquipmentSlot)
			}
			key, entry, ok := equipmentEntryForSlot(equipment, change.EquipmentSlot)
			if !ok || entry.ItemID != change.CurrentItemID {
				return fmt.Errorf("%w: part=%d slot=%d expected=%d", ErrEquipmentMismatch, index, change.EquipmentSlot, change.CurrentItemID)
			}
			entry.ItemID = change.OutputItemID
			if len(entry.RawEntry) < 6 {
				return fmt.Errorf("%w: part=%d slot=%d raw=%d", ErrEquipmentMismatch, index, change.EquipmentSlot, len(entry.RawEntry))
			}
			entry.RawEntry = append([]byte(nil), entry.RawEntry...)
			binary.LittleEndian.PutUint32(entry.RawEntry[2:6], uint32(change.OutputItemID))
			entry.Extra = cloneStringMap(entry.Extra)
			entry.Extra["pvf_path"] = strings.TrimSpace(change.PVFPath)
			entry.Extra["equipment_type"] = strings.TrimSpace(change.EquipmentType)
			delete(entry.Extra, dnfrepo.CloneAvatarAppearanceExtraKey)
			equipment.Entries[key] = entry
			changed[change.EquipmentSlot] = struct{}{}
			result.Changes[index] = change
			result.EquipmentSlots = append(result.EquipmentSlots, change.EquipmentSlot)
		}
		if len(changed) == 0 {
			return ErrRequestInvalid
		}

		result.TicketConsumed = int64(len(changed))
		ticketSlots, err := consumeTickets(&inventory, command.TicketItemID, result.TicketConsumed)
		if err != nil {
			return err
		}
		result.TicketSlots = ticketSlots

		scope := "character:" + command.CharacterID + settingsScopeSuffix
		settings, settingsFound, err := tx.Settings.Load(ctx, scope)
		if err != nil {
			return err
		}
		if !settingsFound {
			settings = dnfrepo.SettingsRecord{Scope: scope, Values: make(map[string]string)}
		} else {
			settings = dnfrepo.CloneSettings(settings)
			if settings.Values == nil {
				settings.Values = make(map[string]string)
			}
		}
		result.GaugeBefore, err = parseNonNegative(settings.Values[settingsUsedParts])
		if err != nil {
			return err
		}
		result.GaugeAfter = result.GaugeBefore + result.TicketConsumed
		if result.GaugeAfter < result.GaugeBefore {
			return ErrRequestInvalid
		}
		if command.GaugeThreshold > 0 && command.Reward != nil {
			beforeCrossings := result.GaugeBefore / command.GaugeThreshold
			afterCrossings := result.GaugeAfter / command.GaugeThreshold
			if command.Reward.CountPerCrossing <= 0 {
				return ErrRequestInvalid
			}
			result.RewardGranted = (afterCrossings - beforeCrossings) * command.Reward.CountPerCrossing
			if result.RewardGranted > 0 {
				result.RewardSlots, err = grantReward(&inventory, *command.Reward, result.RewardGranted)
				if err != nil {
					return err
				}
			}
		}
		settings.Values[settingsUsedParts] = strconv.FormatInt(result.GaugeAfter, 10)

		now := time.Now().UTC()
		inventory.UpdatedAt = now
		equipment.UpdatedAt = now
		settings.UpdatedAt = now
		if err := dnfrepo.SaveInventoryFields(ctx, tx.Inventory, inventory, dnfrepo.InventoryFieldSlots); err != nil {
			return err
		}
		if err := dnfrepo.SaveEquipmentFields(ctx, tx.Equipment, equipment, dnfrepo.EquipmentFieldEntries); err != nil {
			return err
		}
		return dnfrepo.SaveSettingsFields(ctx, tx.Settings, settings, dnfrepo.SettingsFieldValues)
	})
	return result, err
}

func equipmentEntryForSlot(record dnfrepo.EquipmentRecord, slot int16) (string, dnfrepo.EquipmentEntry, bool) {
	for key, entry := range record.Entries {
		if entry.SlotIndex == slot {
			return key, entry, true
		}
	}
	return "", dnfrepo.EquipmentEntry{}, false
}

func consumeTickets(record *dnfrepo.InventoryRecord, itemID, count int64) ([]int16, error) {
	if record == nil || count <= 0 {
		return nil, ErrTicketMissing
	}
	type candidate struct {
		key  string
		slot int16
	}
	var candidates []candidate
	for key, stack := range record.Slots {
		listType, slot, ok := parseSlotKey(key)
		if ok && listType == dnfrepo.MainInventoryListType && stack.ItemID == itemID && stack.Count > 0 {
			candidates = append(candidates, candidate{key: key, slot: slot})
		}
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].slot < candidates[j].slot })
	remaining := count
	changed := make([]int16, 0, len(candidates))
	for _, candidate := range candidates {
		if remaining == 0 {
			break
		}
		stack := record.Slots[candidate.key]
		used := stack.Count
		if used > remaining {
			used = remaining
		}
		stack.Count -= used
		remaining -= used
		changed = append(changed, candidate.slot)
		if stack.Count == 0 {
			delete(record.Slots, candidate.key)
			continue
		}
		if stack.Count > math.MaxUint32 || len(stack.RawEntry) < 10 {
			return nil, ErrTicketMissing
		}
		stack.RawEntry = append([]byte(nil), stack.RawEntry...)
		binary.LittleEndian.PutUint32(stack.RawEntry[6:10], uint32(stack.Count))
		stack.Extra = cloneStringMap(stack.Extra)
		stack.Extra["count"] = strconv.FormatInt(stack.Count, 10)
		record.Slots[candidate.key] = stack
	}
	if remaining != 0 {
		return nil, ErrTicketMissing
	}
	return changed, nil
}

func grantReward(record *dnfrepo.InventoryRecord, reward Reward, count int64) ([]int16, error) {
	if record == nil || reward.ItemID <= 0 || count <= 0 || reward.BuildStack == nil ||
		reward.SlotStart < 0 || reward.SlotEnd < reward.SlotStart {
		return nil, ErrRewardSpace
	}
	// declaredLimit 是 PVF 声明值：<=0 表示没写 [stack limit]（不受总量规则约束）。
	declaredLimit := reward.StackLimit
	if reward.StackLimit <= 0 {
		reward.StackLimit = math.MaxUint32
	}
	// PVF [stack limit] 是这个容器里该道具的总量上限：额度按容器里同 id 总量算，
	// 容器里已经有该道具的行时不能再开第二行。
	space := dnfrepo.ContainerItemSpace(
		record.Slots,
		dnfrepo.MainInventoryListType,
		reward.ItemID,
		declaredLimit,
	)
	remaining := count
	changed := make([]int16, 0, 1)
	for slot := reward.SlotStart; slot <= reward.SlotEnd && remaining > 0; slot++ {
		key := fmt.Sprintf("%d:%d", dnfrepo.MainInventoryListType, slot)
		stack, occupied := record.Slots[key]
		if !occupied || stack.ItemID != reward.ItemID || stack.Count <= 0 || stack.Count >= reward.StackLimit ||
			len(stack.RawEntry) < 10 || (!stack.ExpireAt.IsZero() && !time.Now().UTC().Before(stack.ExpireAt)) {
			continue
		}
		available := reward.StackLimit - stack.Count
		if available > space {
			available = space
		}
		if available <= 0 {
			continue
		}
		added := remaining
		if added > available {
			added = available
		}
		stack.Count += added
		remaining -= added
		space -= added
		stack.RawEntry = append([]byte(nil), stack.RawEntry...)
		binary.LittleEndian.PutUint32(stack.RawEntry[6:10], uint32(stack.Count))
		stack.Extra = cloneStringMap(stack.Extra)
		stack.Extra["count"] = strconv.FormatInt(stack.Count, 10)
		record.Slots[key] = stack
		changed = append(changed, slot)
	}
	for slot := reward.SlotStart; slot <= reward.SlotEnd && remaining > 0; slot++ {
		key := fmt.Sprintf("%d:%d", dnfrepo.MainInventoryListType, slot)
		if _, occupied := record.Slots[key]; occupied {
			continue
		}
		if !dnfrepo.CanOpenContainerItemRow(record.Slots, dnfrepo.MainInventoryListType, reward.ItemID, declaredLimit) {
			break
		}
		chunk := remaining
		if chunk > reward.StackLimit {
			chunk = reward.StackLimit
		}
		if chunk > space {
			chunk = space
		}
		if chunk <= 0 {
			break
		}
		stack, err := reward.BuildStack(slot, chunk)
		if err != nil || stack.ItemID != reward.ItemID || stack.Count != chunk {
			return nil, errors.Join(ErrRewardSpace, err)
		}
		record.Slots[key] = stack
		changed = append(changed, slot)
		remaining -= chunk
		space -= chunk
	}
	if remaining != 0 {
		return nil, ErrRewardSpace
	}
	return changed, nil
}

func parseSlotKey(key string) (byte, int16, bool) {
	parts := strings.Split(strings.TrimSpace(key), ":")
	if len(parts) != 2 {
		return 0, 0, false
	}
	list, err1 := strconv.ParseUint(parts[0], 10, 8)
	slot, err2 := strconv.ParseInt(parts[1], 10, 16)
	return byte(list), int16(slot), err1 == nil && err2 == nil
}

func parseNonNegative(value string) (int64, error) {
	if strings.TrimSpace(value) == "" {
		return 0, nil
	}
	parsed, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	if err != nil || parsed < 0 {
		return 0, ErrRequestInvalid
	}
	return parsed, nil
}

func cloneStringMap(source map[string]string) map[string]string {
	if len(source) == 0 {
		return make(map[string]string)
	}
	out := make(map[string]string, len(source)+2)
	for key, value := range source {
		out[key] = value
	}
	return out
}
