package itemgrade

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

type ResealRangeWeight struct {
	Minimum int64
	Maximum int64
	Weight  int64
}

type ResealConfig struct {
	GradeWeights      []ResealRangeWeight
	RarityWeights     map[int64]float64
	ExtendedWeights   map[int64]float64
	SoulItemIDs       map[int64]uint32
	SoulCostWeights   map[int64]float64
	ResealConstants   map[int64]float64
	ResealCountLimits []ResealRangeWeight
}

type ResealItemDefinition struct {
	Equipment      bool
	MinimumLevel   int64
	Rarity         int64
	Wax            bool
	WaxOneShot     bool
	ResealLevelMin int64
	ResealLevelMax int64
}

type ResealCatalog interface {
	ResolveResealItem(uint32) (ResealItemDefinition, error)
}

type ResealCommand struct {
	AccountID    string
	CharacterID  string
	TargetSlot   int16
	TargetItemID uint32
	MaterialSlot int16
	Mode         uint16
	UpdatedAt    time.Time
}

type ResealResult struct {
	TargetSlot        int16
	TargetStack       dnfrepo.ItemStack
	MaterialSlot      int16
	MaterialRemaining int64
	AccountMaterial   bool
	ResealCount       byte
}

func (o *Owner) Reseal(ctx context.Context, catalog ResealCatalog, config ResealConfig, cmd ResealCommand) (ResealResult, error) {
	if o == nil || catalog == nil || o.characterItems == nil || o.accountItems == nil {
		return ResealResult{}, ErrOwnerUnavailable
	}
	accountID := strings.TrimSpace(cmd.AccountID)
	characterID := strings.TrimSpace(cmd.CharacterID)
	if characterID == "" || cmd.TargetItemID == 0 {
		return ResealResult{}, ErrCharacterRequired
	}
	definition, err := catalog.ResolveResealItem(cmd.TargetItemID)
	if err != nil || !definition.Equipment {
		return ResealResult{}, fmt.Errorf("%w: reseal target definition", ErrTargetMissing)
	}
	now := cmd.UpdatedAt
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	var committed ResealResult
	switch cmd.Mode {
	case 1:
		if accountID == "" {
			return ResealResult{}, ErrOwnerUnavailable
		}
		err = o.accountItems.WithinAccountCharacterItems(ctx, accountID, characterID, func(accounts dnfrepo.AccountInventoryRepository, inventories dnfrepo.InventoryRepository) error {
			inventory, found, loadErr := inventories.Load(ctx, characterID)
			if loadErr != nil || !found {
				return resealFallbackError(loadErr, errors.New("reseal inventory missing"))
			}
			account, found, loadErr := accounts.Load(ctx, accountID)
			if loadErr != nil || !found {
				return resealFallbackError(loadErr, errors.New("reseal account inventory missing"))
			}
			inventory = dnfrepo.CloneInventory(inventory)
			account = dnfrepo.CloneAccountInventory(account)
			target, oldCount, mutationErr := prepareResealTarget(inventory, cmd, definition, config, false)
			if mutationErr != nil {
				return mutationErr
			}
			materialID, cost, supported := config.materialAndCost(definition, oldCount)
			if !supported {
				return errors.New("reseal material rule unavailable")
			}
			materialSlot, remaining, consumeErr := consumeResealItemByID(account.Slots, materialID, cost)
			if consumeErr != nil {
				return consumeErr
			}
			inventory.Slots[resealInventoryKey(cmd.TargetSlot)] = target
			inventory.UpdatedAt = now
			account.UpdatedAt = now
			if saveErr := dnfrepo.SaveInventoryFields(ctx, inventories, inventory, dnfrepo.InventoryFieldSlots); saveErr != nil {
				return saveErr
			}
			if saveErr := accounts.Save(ctx, account); saveErr != nil {
				return saveErr
			}
			committed = ResealResult{TargetSlot: cmd.TargetSlot, TargetStack: target, MaterialSlot: materialSlot, MaterialRemaining: remaining, AccountMaterial: true, ResealCount: ResealCount(target)}
			return nil
		})
	case 0:
		err = o.characterItems.WithinCharacterItems(ctx, characterID, func(inventories dnfrepo.InventoryRepository, _ dnfrepo.EquipmentRepository) error {
			inventory, found, loadErr := inventories.Load(ctx, characterID)
			if loadErr != nil || !found {
				return resealFallbackError(loadErr, errors.New("reseal inventory missing"))
			}
			inventory = dnfrepo.CloneInventory(inventory)
			materialKey := resealInventoryKey(cmd.MaterialSlot)
			material, found := inventory.Slots[materialKey]
			if !found || material.ItemID <= 0 || material.ItemID > math.MaxUint32 || material.Count <= 0 {
				return ErrMaterialMissing
			}
			wax, resolveErr := catalog.ResolveResealItem(uint32(material.ItemID))
			if resolveErr != nil || !wax.Wax {
				return ErrMaterialMissing
			}
			target, oldCount, mutationErr := prepareResealTarget(inventory, cmd, definition, config, wax.WaxOneShot)
			if mutationErr != nil {
				return mutationErr
			}
			if wax.ResealLevelMin > 0 && definition.MinimumLevel < wax.ResealLevelMin || wax.ResealLevelMax > 0 && definition.MinimumLevel > wax.ResealLevelMax {
				return errors.New("reseal wax level mismatch")
			}
			cost := int64(1)
			if !wax.WaxOneShot {
				var supported bool
				cost, supported = config.waxCost(definition, oldCount)
				if !supported {
					return errors.New("reseal wax cost unavailable")
				}
			}
			if material.Count < cost {
				return ErrMaterialMissing
			}
			material.Count -= cost
			if material.Count == 0 {
				delete(inventory.Slots, materialKey)
			} else {
				inventory.Slots[materialKey] = material
			}
			inventory.Slots[resealInventoryKey(cmd.TargetSlot)] = target
			inventory.UpdatedAt = now
			if saveErr := dnfrepo.SaveInventoryFields(ctx, inventories, inventory, dnfrepo.InventoryFieldSlots); saveErr != nil {
				return saveErr
			}
			committed = ResealResult{TargetSlot: cmd.TargetSlot, TargetStack: target, MaterialSlot: cmd.MaterialSlot, MaterialRemaining: material.Count, ResealCount: ResealCount(target)}
			return nil
		})
	default:
		return ResealResult{}, errors.New("unsupported reseal mode")
	}
	if err != nil {
		return ResealResult{}, err
	}
	return committed, nil
}

func prepareResealTarget(inventory dnfrepo.InventoryRecord, cmd ResealCommand, definition ResealItemDefinition, config ResealConfig, oneShot bool) (dnfrepo.ItemStack, byte, error) {
	target, found := inventory.Slots[resealInventoryKey(cmd.TargetSlot)]
	if !found || target.ItemID != int64(cmd.TargetItemID) || target.ItemID <= 0 {
		return dnfrepo.ItemStack{}, 0, ErrTargetMissing
	}
	if SealFlag(target) != 0 {
		return dnfrepo.ItemStack{}, 0, errors.New("reseal target already sealed")
	}
	oldCount := ResealCount(target)
	limit := config.countLimit(definition.MinimumLevel)
	if !oneShot && (limit == 0 || oldCount >= limit) {
		return dnfrepo.ItemStack{}, oldCount, errors.New("reseal count limit reached")
	}
	newCount := oldCount
	if !oneShot {
		newCount++
	}
	return applyResealState(target, newCount), oldCount, nil
}

func applyResealState(stack dnfrepo.ItemStack, count byte) dnfrepo.ItemStack {
	if stack.Extra == nil {
		stack.Extra = make(map[string]string)
	}
	packed := packedResealByte(stack)
	packed = (packed & 0x1f) | ((count & 0x07) << 5)
	stack.Extra["ext_data0"] = strconv.Itoa(int(packed))
	stack.Extra["packed_flag_byte"] = strconv.Itoa(int(packed))
	stack.Extra["reseal_count"] = strconv.Itoa(int(count))
	stack.Extra["seal_flag"] = "1"
	stack.Extra["seal"] = "1"
	stack.Bind = true
	if len(stack.RawEntry) > 0x0d {
		stack.RawEntry = append([]byte(nil), stack.RawEntry...)
		stack.RawEntry[0x0a] = packed
		stack.RawEntry[0x0d] = 1
	}
	return stack
}

func SealFlag(stack dnfrepo.ItemStack) byte {
	if len(stack.RawEntry) > 0x0d {
		return stack.RawEntry[0x0d]
	}
	for _, key := range []string{"seal_flag", "seal", "sealed"} {
		if value, err := strconv.ParseUint(strings.TrimSpace(stack.Extra[key]), 10, 8); err == nil && value != 0 {
			return byte(value)
		}
	}
	if stack.Bind {
		return 1
	}
	return 0
}

func ResealCount(stack dnfrepo.ItemStack) byte {
	if value, err := strconv.ParseUint(strings.TrimSpace(stack.Extra["reseal_count"]), 10, 8); err == nil && value <= 7 {
		return byte(value)
	}
	return packedResealByte(stack) >> 5
}

func packedResealByte(stack dnfrepo.ItemStack) byte {
	if len(stack.RawEntry) > 0x0a {
		return stack.RawEntry[0x0a]
	}
	value, _ := strconv.ParseUint(strings.TrimSpace(stack.Extra["packed_flag_byte"]), 10, 8)
	if value == 0 {
		value, _ = strconv.ParseUint(strings.TrimSpace(stack.Extra["ext_data0"]), 10, 8)
	}
	return byte(value)
}

func (c ResealConfig) materialAndCost(definition ResealItemDefinition, count byte) (uint32, int64, bool) {
	if definition.Rarity < 0 || definition.Rarity >= 7 || definition.MinimumLevel <= 0 || count >= 7 {
		return 0, 0, false
	}
	grade := resealRangeWeight(c.GradeWeights, definition.MinimumLevel)
	weight := c.RarityWeights[definition.Rarity] * c.ExtendedWeights[definition.Rarity] * c.SoulCostWeights[definition.Rarity]
	constant := c.ResealConstants[definition.Rarity]
	cost := int64(float64(grade) * weight * math.Pow(constant, float64(count)))
	itemID := c.SoulItemIDs[definition.Rarity]
	return itemID, cost, grade > 0 && weight > 0 && constant > 0 && itemID > 0 && cost > 0
}

func (c ResealConfig) waxCost(definition ResealItemDefinition, count byte) (int64, bool) {
	if definition.Rarity < 0 || definition.Rarity >= 7 || definition.MinimumLevel <= 0 || count >= 7 {
		return 0, false
	}
	grade := resealRangeWeight(c.GradeWeights, definition.MinimumLevel)
	weight := c.RarityWeights[definition.Rarity] * c.ExtendedWeights[definition.Rarity]
	constant := c.ResealConstants[definition.Rarity]
	cost := int64(float64(grade) * weight * math.Pow(constant, float64(count)))
	return cost, grade > 0 && weight > 0 && constant > 0 && cost > 0
}

func (c ResealConfig) countLimit(level int64) byte {
	value := resealRangeWeight(c.ResealCountLimits, level)
	if value < 0 {
		return 0
	}
	if value > 7 {
		value = 7
	}
	return byte(value)
}

func resealRangeWeight(rows []ResealRangeWeight, value int64) int64 {
	for _, row := range rows {
		if value >= row.Minimum && value <= row.Maximum {
			return row.Weight
		}
	}
	return 0
}

func consumeResealItemByID(slots map[string]dnfrepo.ItemStack, itemID uint32, count int64) (int16, int64, error) {
	if count <= 0 {
		return 0, 0, errors.New("invalid reseal material count")
	}
	keys := make([]string, 0)
	available := int64(0)
	for key, stack := range slots {
		if stack.ItemID == int64(itemID) && stack.Count > 0 {
			keys = append(keys, key)
			available += stack.Count
		}
	}
	if available < count {
		return 0, 0, ErrMaterialMissing
	}
	sort.Strings(keys)
	need := count
	var lastSlot int16
	var lastRemaining int64
	for _, key := range keys {
		stack := slots[key]
		parts := strings.Split(key, ":")
		if len(parts) == 2 {
			if value, err := strconv.ParseInt(parts[1], 10, 16); err == nil {
				lastSlot = int16(value)
			}
		}
		used := stack.Count
		if used > need {
			used = need
		}
		stack.Count -= used
		need -= used
		lastRemaining = stack.Count
		if stack.Count == 0 {
			delete(slots, key)
		} else {
			slots[key] = stack
		}
		if need == 0 {
			break
		}
	}
	return lastSlot, lastRemaining, nil
}

func resealInventoryKey(slot int16) string {
	return fmt.Sprintf("%d:%d", dnfrepo.MainInventoryListType, slot)
}

func resealFallbackError(err, fallback error) error {
	if err != nil {
		return err
	}
	return fallback
}
