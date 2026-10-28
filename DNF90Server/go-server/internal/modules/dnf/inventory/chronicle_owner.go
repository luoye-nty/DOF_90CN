package inventory

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

const (
	chronicleEquipmentLevelOffset = 0x58
	chronicleEquipmentRawSize     = 0x77
)

var (
	ErrChronicleResolverRequired = fmt.Errorf("chronicle growth PVF resolver required")
	ErrChronicleRestricted       = fmt.Errorf("chronicle growth target restricted")
	ErrChronicleMaterial         = fmt.Errorf("chronicle growth material insufficient")
)

type ChronicleGrowthConsumption struct {
	ListType byte
	Slot     int16
	ItemID   int64
	Count    int64
	Remain   int64
}

type ChronicleGrowthResult struct {
	TicketSlot, TargetSlot, MaterialSlot int16
	OldLevel, NewLevel                   int
	MaterialCount                        int64
	OutputItemID                         int64
	Conversion                           bool
	Succeeded                            bool
	Consumptions                         []ChronicleGrowthConsumption
}

// UpgradeChronicle commits the target byte and both consumed stacks in one
// inventory transaction. The client owns decrementing stacks after the ACK,
// so the server persists the same mutation only once and reports it in the
// result envelope.
func (o *Owner) UpgradeChronicle(ctx context.Context, cmd Command, resolve alignedcmd.ChronicleGrowthResolver) (ChronicleGrowthResult, error) {
	if o == nil || o.repo == nil || cmd.SelectedCharacterID == 0 {
		return ChronicleGrowthResult{}, ErrOwnerUnavailable
	}
	if resolve == nil {
		return ChronicleGrowthResult{}, ErrChronicleResolverRequired
	}
	policy, err := resolve(cmd.ChronicleTicketID, cmd.ChronicleTargetID)
	if err != nil {
		return ChronicleGrowthResult{}, err
	}
	if policy.Conversion != nil {
		return o.convertChronicleEquipment(ctx, cmd, policy)
	}
	if policy.TicketItemID == 0 || policy.MaterialItemID == 0 || policy.UpgradeLevel <= 0 ||
		policy.ConditionMaximum <= 0 || policy.CostPolicy.BaseCount <= 0 || policy.CostPolicy.Multiplier <= 0 {
		return ChronicleGrowthResult{}, ErrChronicleRestricted
	}
	if chronicleProbabilityWeightAt(policy.Probability, policy.TargetMinimumLevel) != 100000 {
		return ChronicleGrowthResult{}, ErrChronicleRestricted
	}
	characterID := strconv.FormatUint(uint64(cmd.SelectedCharacterID), 10)
	var result ChronicleGrowthResult
	apply := func(txOwner *Owner) error {
		record, ok, loadErr := txOwner.repo.Load(ctx, characterID)
		if loadErr != nil {
			return loadErr
		}
		if !ok {
			return ErrInventoryNotFound
		}
		record = dnfrepo.CloneInventory(record)
		ticketKey := slotKey(listTypeMain, cmd.ChronicleTicketSlot)
		targetKey := slotKey(listTypeMain, cmd.ChronicleTargetSlot)
		materialKey := slotKey(listTypeMain, cmd.ChronicleMaterialSlot)
		ticket, ticketOK := record.Slots[ticketKey]
		target, targetOK := record.Slots[targetKey]
		material, materialOK := record.Slots[materialKey]
		if !ticketOK || ticket.Count <= 0 || ticket.ItemID != cmd.ChronicleTicketID ||
			!targetOK || target.ItemID != cmd.ChronicleTargetID ||
			!materialOK || material.ItemID != policy.MaterialItemID || cmd.ChronicleMaterialID != policy.MaterialItemID {
			return ErrChronicleMaterial
		}
		if policy.TargetRarity > 0 {
			if raw, ok := parseExtraInt(target.Extra, "rarity"); ok && raw != policy.TargetRarity {
				return ErrChronicleRestricted
			}
		}
		// Legacy rows without raw+0x58 start at their PVF minimum. Request
		// field12 is an independent tagged value and is not used as energy.
		currentEnergy := chronicleCurrentEnergy(target)
		currentLevel := policy.TargetMinimumLevel + int(currentEnergy)
		if currentLevel < policy.ConditionMinimum || currentLevel >= policy.ConditionMaximum || chronicleProbabilityWeightAt(policy.Probability, currentLevel) != 100000 {
			return ErrChronicleRestricted
		}
		need, costErr := chronicleRequiredMaterial(policy, target, currentLevel)
		if costErr != nil {
			return costErr
		}
		if material.Count < need {
			return ErrChronicleMaterial
		}
		newLevel := currentLevel + policy.UpgradeLevel
		if newLevel > policy.ConditionMaximum {
			newLevel = policy.ConditionMaximum
		}
		newEnergy := newLevel - policy.TargetMinimumLevel
		if newEnergy < 0 || newEnergy > math.MaxUint8 {
			return ErrChronicleRestricted
		}
		ticketRemain := ticket.Count - 1
		if ticketRemain <= 0 {
			delete(record.Slots, ticketKey)
		} else {
			ticket = cloneStack(ticket)
			ticket.Count = ticketRemain
			updateStackRawAmount(&ticket)
			record.Slots[ticketKey] = ticket
		}
		materialRemain := material.Count - need
		if materialRemain <= 0 {
			delete(record.Slots, materialKey)
		} else {
			material = cloneStack(material)
			material.Count = materialRemain
			updateStackRawAmount(&material)
			record.Slots[materialKey] = material
		}
		target = cloneStack(target)
		if len(target.RawEntry) < chronicleEquipmentRawSize {
			raw := make([]byte, chronicleEquipmentRawSize)
			copy(raw, target.RawEntry)
			target.RawEntry = raw
		}
		target.RawEntry[chronicleEquipmentLevelOffset] = byte(newEnergy)
		if target.Extra == nil {
			target.Extra = make(map[string]string)
		}
		// Projection code prefers Extra when present, so keep it synchronized
		// with the authoritative raw row while preserving every adjacent field.
		target.Extra["byte_58"] = strconv.Itoa(int(newEnergy))
		target.Extra["value_58"] = strconv.Itoa(int(newEnergy))
		record.Slots[targetKey] = target
		if err := dnfrepo.SaveInventoryFields(ctx, txOwner.repo, record, dnfrepo.InventoryFieldSlots); err != nil {
			return err
		}
		result = ChronicleGrowthResult{
			TicketSlot: cmd.ChronicleTicketSlot, TargetSlot: cmd.ChronicleTargetSlot, MaterialSlot: cmd.ChronicleMaterialSlot,
			OldLevel: currentLevel, NewLevel: newLevel, MaterialCount: need, Succeeded: true,
			Consumptions: []ChronicleGrowthConsumption{
				{ListType: listTypeMain, Slot: cmd.ChronicleTicketSlot, ItemID: cmd.ChronicleTicketID, Count: 1, Remain: ticketRemain},
				{ListType: listTypeMain, Slot: cmd.ChronicleMaterialSlot, ItemID: cmd.ChronicleMaterialID, Count: need, Remain: materialRemain},
			},
		}
		return nil
	}
	if o.inItemTransaction {
		err = apply(o)
	} else {
		err = o.withinInventoryTransaction(ctx, characterID, apply)
	}
	return result, err
}

func chronicleCurrentEnergy(stack dnfrepo.ItemStack) byte {
	for _, key := range []string{"chronicle_energy", "emancipate_level", "byte_58", "value_58"} {
		if value, ok := parseExtraInt(stack.Extra, key); ok && value >= 0 && value <= math.MaxUint8 {
			return byte(value)
		}
	}
	if len(stack.RawEntry) > chronicleEquipmentLevelOffset {
		return stack.RawEntry[chronicleEquipmentLevelOffset]
	}
	return 0
}

func chronicleProbabilityWeightAt(rows []alignedcmd.ChronicleGrowthProbability, level int) int64 {
	for _, row := range rows {
		if level <= row.MaximumLevel {
			return row.Weight
		}
	}
	if len(rows) == 0 {
		return 0
	}
	return rows[len(rows)-1].Weight
}

func chronicleRequiredMaterial(policy alignedcmd.ChronicleGrowthResolution, target dnfrepo.ItemStack, equipmentLevel int) (int64, error) {
	cost := policy.CostPolicy
	part := strings.ToLower(strings.Trim(strings.TrimSpace(policy.EquipmentType), "`[] "))
	if part == "wrist" {
		part = "wrisat"
	}
	partWeight, partOK := cost.PartWeights[part]
	levelWeight, levelOK := chronicleInterpolatedWeight(cost.LevelWeights, equipmentLevel)
	if !partOK || !levelOK || cost.MaterialItemID != policy.MaterialItemID {
		return 0, ErrChronicleRestricted
	}
	upgradeLevel := int(upgradeLevelOf(target))
	ampType, ampValue := upgradeAmplifyState(target)
	upgradeWeight := 1.0
	ampWeight := 1.0
	if ampType != 0 && ampType&0x80 == 0 && ampValue > 0 {
		var weightOK bool
		ampWeight, weightOK = chronicleInterpolatedWeight(cost.AmplifyWeights, upgradeLevel)
		if !weightOK {
			return 0, ErrChronicleRestricted
		}
	} else {
		var weightOK bool
		upgradeWeight, weightOK = chronicleInterpolatedWeight(cost.UpgradeWeights, upgradeLevel)
		if !weightOK {
			return 0, ErrChronicleRestricted
		}
	}
	forgingLevel := chronicleForgingLevel(target)
	forgingWeight, forgingOK := chronicleInterpolatedWeight(cost.ForgingWeights, forgingLevel)
	if !forgingOK {
		return 0, ErrChronicleRestricted
	}
	combined := upgradeWeight*ampWeight + forgingWeight - 1
	required := math.Floor(float64(cost.BaseCount) * cost.Multiplier * partWeight * levelWeight * combined)
	if required < 1 || required > float64(math.MaxInt64) {
		return 0, ErrChronicleRestricted
	}
	return int64(required), nil
}

func chronicleForgingLevel(stack dnfrepo.ItemStack) int {
	if value, ok := parseExtraInt(stack.Extra, "byte_57", "value_57", "forging", "forge_level"); ok && value >= 0 {
		return int(value)
	}
	if len(stack.RawEntry) > 0x57 {
		return int(stack.RawEntry[0x57])
	}
	return 0
}

func chronicleInterpolatedWeight(segments []alignedcmd.ChronicleGrowthWeightSegment, value int) (float64, bool) {
	for _, segment := range segments {
		if value < segment.Minimum || value > segment.Maximum || segment.MinimumWeight <= 0 || segment.MaximumWeight <= 0 {
			continue
		}
		if segment.Maximum == segment.Minimum {
			return segment.MinimumWeight, true
		}
		ratio := float64(value-segment.Minimum) / float64(segment.Maximum-segment.Minimum)
		return segment.MinimumWeight + (segment.MaximumWeight-segment.MinimumWeight)*ratio, true
	}
	return 0, false
}

func parseExtraInt(extra map[string]string, keys ...string) (int64, bool) {
	for _, key := range keys {
		if raw := strings.TrimSpace(extra[key]); raw != "" {
			if value, err := strconv.ParseInt(raw, 0, 64); err == nil {
				return value, true
			}
		}
	}
	return 0, false
}
