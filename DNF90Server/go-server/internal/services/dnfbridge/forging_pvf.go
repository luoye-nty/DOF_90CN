package dnfbridge

import (
	"errors"
	"fmt"
	"math"
	"strings"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
)

const currentForgingPVFPath = "etc/upgrade_separate.etc"

type currentForgingLevel struct {
	TargetLevel        byte
	IndependentAttack  int64
	SuccessWeight      int
	MaterialMultiplier float64
}

type currentForgingMaterial struct {
	ItemID    int64
	BaseCount int64
}

type currentForgingConfig struct {
	MaxLevel          byte
	Levels            []currentForgingLevel
	LevelWeight       float64
	MaterialsByGrade  map[int64]currentForgingMaterial
	ItemRarityWeights []float64
	StatRarityWeights []float64
	WeaponWeights     []float64
}

type currentForgingTicket struct {
	TargetLevel   int64
	SuccessWeight int
	Mode          string
	ApplyValue    int64
}

func (s *Service) alignedForgingResolverForCommand(opcode dnfenum.CmdPacket) (alignedcmd.ForgingResolver, error) {
	if opcode != dnfenum.CmdPacketUpgradeItemSeparate {
		return nil, nil
	}
	catalog, err := s.currentPVFItemCatalog()
	if err != nil {
		return nil, err
	}
	if catalog == nil {
		return nil, errDungeonDropSourceRequired
	}
	s.initialEquipmentMu.Lock()
	source, err := s.initialEquipmentArchiveLocked()
	s.initialEquipmentMu.Unlock()
	if err != nil {
		return nil, err
	}
	config, err := currentForgingConfigFromSource(source)
	if err != nil {
		return nil, err
	}
	return func(materialItemID int64, targetItemID int64, currentLevel byte) (alignedcmd.ForgingResolution, error) {
		return resolveCurrentForgingRequestMetadata(catalog, config, source, materialItemID, targetItemID, currentLevel)
	}, nil
}

func currentForgingConfigFromSource(source dnfpvf.Source) (*currentForgingConfig, error) {
	if source == nil {
		return nil, dnfpvf.ErrSourceRequired
	}
	text, err := source.ReadText(currentForgingPVFPath)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", currentForgingPVFPath, err)
	}
	document, err := dnfpvf.Parse(currentForgingPVFPath, text)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", currentForgingPVFPath, err)
	}
	config, err := parseCurrentForgingConfig(document)
	if err != nil {
		return nil, fmt.Errorf("parse %s config: %w", currentForgingPVFPath, err)
	}
	return config, nil
}

func parseCurrentForgingConfig(document *dnfpvf.Document) (*currentForgingConfig, error) {
	if document == nil {
		return nil, fmt.Errorf("document required")
	}
	table := document.Numbers("table")
	if len(table) == 0 || len(table)%4 != 0 {
		return nil, fmt.Errorf("[table] numeric width %d is not a positive multiple of 4", len(table))
	}
	config := &currentForgingConfig{MaterialsByGrade: make(map[int64]currentForgingMaterial)}
	for offset := 0; offset < len(table); offset += 4 {
		independentAttack, ok := exactForgingInteger(table[offset])
		if !ok || independentAttack < 0 {
			return nil, fmt.Errorf("[table] row %d independent attack %.6f invalid", offset/4+1, table[offset])
		}
		successWeight, ok := exactForgingInteger(table[offset+2])
		if !ok || successWeight < 0 || successWeight > 10000 {
			return nil, fmt.Errorf("[table] row %d success weight %.6f invalid", offset/4+1, table[offset+2])
		}
		multiplier := table[offset+3]
		if math.IsNaN(multiplier) || math.IsInf(multiplier, 0) || multiplier <= 0 {
			return nil, fmt.Errorf("[table] row %d material multiplier %.6f invalid", offset/4+1, multiplier)
		}
		config.Levels = append(config.Levels, currentForgingLevel{
			TargetLevel:        byte(len(config.Levels) + 1),
			IndependentAttack:  independentAttack,
			SuccessWeight:      int(successWeight),
			MaterialMultiplier: multiplier,
		})
	}
	maxValues := document.Numbers("separate upgrade max")
	if len(maxValues) != 1 {
		return nil, fmt.Errorf("[separate upgrade max] count %d, want 1", len(maxValues))
	}
	maxLevel, ok := exactForgingInteger(maxValues[0])
	if !ok || maxLevel <= 0 || maxLevel > int64(len(config.Levels)) || maxLevel > math.MaxUint8 {
		return nil, fmt.Errorf("[separate upgrade max] %.6f outside parsed table", maxValues[0])
	}
	config.MaxLevel = byte(maxLevel)
	levelValues := document.Numbers("level")
	if len(levelValues) != 1 || math.IsNaN(levelValues[0]) || math.IsInf(levelValues[0], 0) || levelValues[0] <= 0 {
		return nil, fmt.Errorf("[level] must contain one positive value")
	}
	config.LevelWeight = levelValues[0]

	materialValues := document.Numbers("item weights by grade")
	if len(materialValues) == 0 || len(materialValues)%3 != 0 {
		return nil, fmt.Errorf("[item weights by grade] numeric width %d is not a positive multiple of 3", len(materialValues))
	}
	for offset := 0; offset < len(materialValues); offset += 3 {
		grade, gradeOK := exactForgingInteger(materialValues[offset])
		itemID, itemOK := exactForgingInteger(materialValues[offset+1])
		baseCount, countOK := exactForgingInteger(materialValues[offset+2])
		if !gradeOK || !itemOK || !countOK || grade <= 0 || itemID <= 0 || baseCount <= 0 {
			return nil, fmt.Errorf("[item weights by grade] row %d invalid", offset/3+1)
		}
		if _, duplicate := config.MaterialsByGrade[grade]; duplicate {
			return nil, fmt.Errorf("[item weights by grade] duplicate grade %d", grade)
		}
		config.MaterialsByGrade[grade] = currentForgingMaterial{ItemID: itemID, BaseCount: baseCount}
	}
	config.ItemRarityWeights = append([]float64(nil), document.Numbers("item weights by rarity")...)
	if len(config.ItemRarityWeights) == 0 {
		return nil, fmt.Errorf("[item weights by rarity] is empty")
	}
	for rarity, weight := range config.ItemRarityWeights {
		if math.IsNaN(weight) || math.IsInf(weight, 0) || weight <= 0 {
			return nil, fmt.Errorf("[item weights by rarity] index %d value %.6f invalid", rarity, weight)
		}
	}
	config.StatRarityWeights = append([]float64(nil), document.Numbers("stat weights by rarity")...)
	if len(config.StatRarityWeights) == 0 {
		return nil, fmt.Errorf("[stat weights by rarity] is empty")
	}
	for rarity, weight := range config.StatRarityWeights {
		if math.IsNaN(weight) || math.IsInf(weight, 0) || weight <= 0 {
			return nil, fmt.Errorf("[stat weights by rarity] index %d value %.6f invalid", rarity, weight)
		}
	}
	config.WeaponWeights = append([]float64(nil), document.Numbers("weapon weights")...)
	if len(config.WeaponWeights) == 0 {
		return nil, fmt.Errorf("[weapon weights] is empty")
	}
	for index, weight := range config.WeaponWeights {
		if math.IsNaN(weight) || math.IsInf(weight, 0) || weight <= 0 {
			return nil, fmt.Errorf("[weapon weights] index %d value %.6f invalid", index, weight)
		}
	}
	return config, nil
}

func parseCurrentForgingTicket(document *dnfpvf.Document) (currentForgingTicket, bool, error) {
	const section = "equipment separate reinforcement ticket"
	if document == nil {
		return currentForgingTicket{}, false, nil
	}
	if _, found := document.Section(section); !found {
		return currentForgingTicket{}, false, nil
	}
	values := document.Ints(section)
	texts := document.Texts(section)
	if len(values) < 2 || len(texts) == 0 || values[0] <= 0 || values[0] > math.MaxUint8 || values[1] < 0 || values[1] > 100 {
		return currentForgingTicket{}, true, fmt.Errorf("invalid [%s]", section)
	}
	mode := strings.ToLower(strings.Trim(strings.TrimSpace(texts[0]), "` []"))
	if mode != "fixed" && mode != "additional" {
		return currentForgingTicket{}, true, fmt.Errorf("unsupported mode %q in [%s]", texts[0], section)
	}
	applyValue := int64(-1)
	if len(values) >= 3 {
		applyValue = values[2]
	}
	return currentForgingTicket{
		TargetLevel:   values[0],
		SuccessWeight: int(values[1] * 100),
		Mode:          mode,
		ApplyValue:    applyValue,
	}, true, nil
}

func resolveCurrentForgingMetadata(catalog *pvfDungeonDropCatalog, config *currentForgingConfig, targetItemID int64, currentLevel byte) (alignedcmd.ForgingResolution, error) {
	resolution := alignedcmd.ForgingResolution{}
	if catalog == nil || config == nil {
		return resolution, errDungeonDropSourceRequired
	}
	if targetItemID <= 0 || targetItemID > math.MaxUint32 {
		return resolution, nil
	}
	definition, err := catalog.ResolveItem(uint32(targetItemID))
	if err != nil {
		if errors.Is(err, errDungeonDropItemUnresolved) {
			return resolution, nil
		}
		return resolution, fmt.Errorf("resolve forging target item=%d: %w", targetItemID, err)
	}
	resolution.TargetKind = string(definition.Kind)
	resolution.TargetPVFPath = definition.PVFPath
	resolution.TargetEquipmentType = definition.EquipmentType
	resolution.TargetDurability = definition.Durability
	resolution.MaxLevel = config.MaxLevel
	if definition.Kind != dungeonDropItemEquipment || !currentForgingWeaponType(definition.EquipmentType) || currentLevel >= config.MaxLevel {
		return resolution, nil
	}
	targetLevel := int(currentLevel) + 1
	if targetLevel <= 0 || targetLevel > len(config.Levels) {
		return resolution, nil
	}
	level := config.Levels[targetLevel-1]
	material, ok := config.MaterialsByGrade[definition.Grade]
	if !ok || definition.Rarity < 0 || definition.Rarity >= int64(len(config.ItemRarityWeights)) || definition.Rarity >= int64(len(config.StatRarityWeights)) {
		return resolution, nil
	}
	rawCount := float64(material.BaseCount)*config.ItemRarityWeights[definition.Rarity]*level.MaterialMultiplier + 0.0000001
	if math.IsNaN(rawCount) || math.IsInf(rawCount, 0) || rawCount > math.MaxInt64 {
		return alignedcmd.ForgingResolution{}, fmt.Errorf("forging material cost item=%d grade=%d rarity=%d invalid", targetItemID, definition.Grade, definition.Rarity)
	}
	materialCount := int64(rawCount)
	if materialCount < 1 {
		materialCount = 1
	}
	resolution.TargetLevel = level.TargetLevel
	independentAttack, err := calculateCurrentForgingIndependentAttack(config, definition.MinimumLevel, definition.Rarity, level.TargetLevel)
	if err != nil {
		return alignedcmd.ForgingResolution{}, fmt.Errorf("forging independent attack item=%d level=%d: %w", targetItemID, level.TargetLevel, err)
	}
	resolution.IndependentAttack = independentAttack
	resolution.SuccessWeight = level.SuccessWeight
	resolution.MaterialItemID = material.ItemID
	resolution.MaterialCount = materialCount
	return resolution, nil
}

// resolveCurrentForgingRequestMetadata overlays the separate-upgrade ticket
// carried by the request's material slot on the ordinary target metadata.
// The current PVF declares this coupon family through
// [equipment separate reinforcement ticket]:
//
//	<level> <success-percent> `<fixed|additional>` <source-level-cap>
//
// Fixed coupons write the declared forging level even when it is lower than
// the current level. Additional coupons add the declared amount and honor the
// optional source-level cap. Both consume exactly one coupon per accepted
// attempt and use the native op438 probability base of 10000.
//
// A second family declares itself through [enchant random] plus
// [action type] `[random upgrade separate]` and is handled by
// resolveCurrentForgingRandomTicketMetadata.
func resolveCurrentForgingRequestMetadata(
	catalog *pvfDungeonDropCatalog,
	config *currentForgingConfig,
	source dnfpvf.Source,
	materialItemID int64,
	targetItemID int64,
	currentLevel byte,
) (alignedcmd.ForgingResolution, error) {
	resolution, err := resolveCurrentForgingMetadata(catalog, config, targetItemID, currentLevel)
	if err != nil {
		return alignedcmd.ForgingResolution{}, err
	}
	if source == nil || materialItemID <= 0 || materialItemID > math.MaxUint32 {
		return resolution, nil
	}
	materialDefinition, err := catalog.ResolveItem(uint32(materialItemID))
	if err != nil {
		if errors.Is(err, errDungeonDropItemUnresolved) {
			return resolution, nil
		}
		return alignedcmd.ForgingResolution{}, fmt.Errorf("resolve forging request material item=%d: %w", materialItemID, err)
	}
	if materialDefinition.Kind != dungeonDropItemStackable {
		return resolution, nil
	}
	materialDocument, err := parseDungeonCardPVFDocument(source, materialDefinition.PVFPath)
	if err != nil {
		return alignedcmd.ForgingResolution{}, fmt.Errorf("parse forging request material item=%d path=%s: %w", materialItemID, materialDefinition.PVFPath, err)
	}
	if randomTicket, found := parseEnchantRandomTicket(materialDocument); found &&
		currentEnchantRandomForgingActionType(materialDocument) {
		return resolveCurrentForgingRandomTicketMetadata(catalog, config, resolution, randomTicket, materialDefinition, targetItemID)
	}
	if action, found := materialDocument.Text("action type"); found &&
		strings.EqualFold(strings.TrimSpace(strings.ReplaceAll(action, "`", "")), "[portable genuine damage upgrade upgrede]") {
		levels := materialDocument.Ints("check usable itemlevel")
		if len(levels) != 2 || levels[0] < 0 || levels[1] < levels[0] {
			return alignedcmd.ForgingResolution{}, fmt.Errorf("portable forging item=%d has invalid [check usable itemlevel]", materialItemID)
		}
		targetDefinition, resolveErr := catalog.ResolveItem(uint32(targetItemID))
		if resolveErr != nil {
			return alignedcmd.ForgingResolution{}, fmt.Errorf("resolve portable forging target item=%d: %w", targetItemID, resolveErr)
		}
		if targetDefinition.Kind != dungeonDropItemEquipment || targetDefinition.MinimumLevel < levels[0] || targetDefinition.MinimumLevel > levels[1] {
			return alignedcmd.ForgingResolution{}, fmt.Errorf("portable forging target item=%d level=%d outside %d..%d", targetItemID, targetDefinition.MinimumLevel, levels[0], levels[1])
		}
		resolution.MaterialItemID = materialItemID
		resolution.MaterialCount = 1
		return resolution, nil
	}
	ticket, found, err := parseCurrentForgingTicket(materialDocument)
	if err != nil {
		return alignedcmd.ForgingResolution{}, fmt.Errorf("forging ticket item=%d path=%s: %w", materialItemID, materialDefinition.PVFPath, err)
	}
	if !found {
		return resolution, nil
	}

	resolution.TicketMode = ticket.Mode
	resolution.TicketApplyValue = ticket.ApplyValue
	resolution.TicketPVFPath = materialDefinition.PVFPath
	resolution.MaterialItemID = materialItemID
	resolution.MaterialCount = 1
	resolution.SuccessWeight = ticket.SuccessWeight
	resolution.TargetLevel = byte(ticket.TargetLevel)
	if ticket.Mode == "additional" {
		maximumSourceLevel := int64(config.MaxLevel) - ticket.TargetLevel
		if ticket.ApplyValue >= 0 && ticket.ApplyValue < maximumSourceLevel {
			maximumSourceLevel = ticket.ApplyValue
		}
		if int64(currentLevel) > maximumSourceLevel || int64(currentLevel)+ticket.TargetLevel > int64(config.MaxLevel) {
			resolution.TargetLevel = 0
			return resolution, nil
		}
		resolution.TargetLevel = byte(int64(currentLevel) + ticket.TargetLevel)
	}
	if resolution.TargetLevel == 0 || resolution.TargetLevel > config.MaxLevel {
		return resolution, nil
	}

	targetDefinition, err := catalog.ResolveItem(uint32(targetItemID))
	if err != nil {
		if errors.Is(err, errDungeonDropItemUnresolved) {
			return resolution, nil
		}
		return alignedcmd.ForgingResolution{}, fmt.Errorf("resolve forging ticket target item=%d: %w", targetItemID, err)
	}
	independentAttack, err := calculateCurrentForgingIndependentAttack(config, targetDefinition.MinimumLevel, targetDefinition.Rarity, resolution.TargetLevel)
	if err != nil {
		return alignedcmd.ForgingResolution{}, fmt.Errorf("forging ticket independent attack item=%d level=%d: %w", targetItemID, resolution.TargetLevel, err)
	}
	resolution.IndependentAttack = independentAttack
	return resolution, nil
}

// resolveCurrentForgingRandomTicketMetadata overlays the [enchant random] +
// [action type] `[random upgrade separate]` coupon family (e.g. +6锻造券
// 2683788) on the ordinary target metadata. Unlike the fixed/additional
// coupons this family has no single declared level: the er_enchant candidates
// and their er_slot / er_grade / er_seal restrictions travel to the inventory
// domain, which owns the weighted roll. Only the per-candidate independent
// attack is pre-computed here, because the upgrade_separate.etc table lives on
// this side of the boundary.
func resolveCurrentForgingRandomTicketMetadata(
	catalog *pvfDungeonDropCatalog,
	config *currentForgingConfig,
	resolution alignedcmd.ForgingResolution,
	ticket alignedcmd.EnchantRandomTicket,
	materialDefinition dungeonDropItemDefinition,
	targetItemID int64,
) (alignedcmd.ForgingResolution, error) {
	if len(ticket.Entries) == 0 {
		return resolution, nil
	}
	targetDefinition, err := catalog.ResolveItem(uint32(targetItemID))
	if err != nil {
		if errors.Is(err, errDungeonDropItemUnresolved) {
			return resolution, nil
		}
		return alignedcmd.ForgingResolution{}, fmt.Errorf("resolve random forging ticket target item=%d: %w", targetItemID, err)
	}
	resolution.TicketRandom = true
	resolution.RandomTicket = ticket
	resolution.TargetRarity = targetDefinition.Rarity
	resolution.TicketPVFPath = materialDefinition.PVFPath
	resolution.MaterialItemID = int64(materialDefinition.ItemID)
	resolution.MaterialCount = 1
	// The domain picks the level, so neither of these may carry the ordinary
	// next-level forging plan computed above.
	resolution.TargetLevel = 0
	resolution.SuccessWeight = 0
	resolution.IndependentAttack = 0
	if targetDefinition.Kind != dungeonDropItemEquipment {
		return resolution, nil
	}
	attacks := make(map[int64]int64, len(ticket.Entries))
	for _, entry := range ticket.Entries {
		if entry.TargetLevel <= 0 || entry.TargetLevel > int64(config.MaxLevel) {
			continue
		}
		attack, attackErr := calculateCurrentForgingIndependentAttack(config, targetDefinition.MinimumLevel, targetDefinition.Rarity, byte(entry.TargetLevel))
		if attackErr != nil {
			return alignedcmd.ForgingResolution{}, fmt.Errorf("random forging ticket independent attack item=%d level=%d: %w", targetItemID, entry.TargetLevel, attackErr)
		}
		attacks[entry.TargetLevel] = attack
	}
	resolution.RandomIndependentAttacks = attacks
	return resolution, nil
}

func calculateCurrentForgingIndependentAttack(config *currentForgingConfig, minimumLevel, rarity int64, forgingLevel byte) (int64, error) {
	if config == nil || forgingLevel == 0 || int(forgingLevel) > len(config.Levels) {
		return 0, nil
	}
	if config.LevelWeight <= 0 || rarity < 0 || rarity >= int64(len(config.StatRarityWeights)) {
		return 0, fmt.Errorf("invalid level or rarity weights")
	}
	level := config.Levels[int(forgingLevel)-1]
	raw := (1 + float64(max(minimumLevel, int64(0)))/config.LevelWeight) * float64(level.IndependentAttack) * config.StatRarityWeights[rarity]
	if math.IsNaN(raw) || math.IsInf(raw, 0) || raw < 0 || raw > math.MaxInt64 {
		return 0, fmt.Errorf("calculated value %.6f outside int64", raw)
	}
	return int64(math.Floor(raw)), nil
}

func currentForgingWeaponType(raw string) bool {
	value := strings.ToLower(strings.TrimSpace(strings.ReplaceAll(raw, "`", "")))
	return value == "[weapon]" || value == "weapon"
}

func exactForgingInteger(value float64) (int64, bool) {
	if math.IsNaN(value) || math.IsInf(value, 0) || value < math.MinInt64 || value > math.MaxInt64 {
		return 0, false
	}
	integer := int64(value)
	return integer, float64(integer) == value
}
