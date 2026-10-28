package dnfbridge

import (
	"errors"
	"fmt"
	"math"
	"path"
	"strings"

	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
)

const currentCargoTransportConfigPath = "etc/cargotransportstone.etc"
const currentResealConfigPath = "etc/reseal.etc"

var errCurrentCargoTransportPVFInvalid = errors.New("current cargo transport PVF config is invalid")

type currentPVFRangeWeight struct {
	Minimum int64
	Maximum int64
	Weight  int64
}

type currentCargoTransportRule struct {
	StoneType       int64
	GradeWeights    []currentPVFRangeWeight
	RarityWeights   map[int64]int64
	EquipmentTypes  map[string]bool
	IncludeItemIDs  map[uint32]bool
	ExcludeItemIDs  map[uint32]bool
	ExceptGrowth    bool
	ExceptPeriod    bool
	SoulItemIDs     map[int64]uint32
	SoulWeights     map[int64][]int64
	ElementItemID   uint32
	ElementWeights  map[int64][]int64
	EpicPieceWeight []int64
	ParticularCosts []currentCargoParticularCost
}

type currentCargoParticularCost struct {
	Rarity int64
	Level  int64
	ItemID uint32
	Count  int64
}

type currentCargoTransportConfig struct {
	Rules map[int64]currentCargoTransportRule
}

type currentResealConfig struct {
	GradeWeights      []currentPVFRangeWeight
	RarityWeights     map[int64]float64
	ExtendedWeights   map[int64]float64
	SoulItemIDs       map[int64]uint32
	SoulCostWeights   map[int64]float64
	ResealConstants   map[int64]float64
	ResealCountLimits []currentPVFRangeWeight
}

func loadCurrentCargoTransportConfig(catalog *pvfDungeonDropCatalog) (currentCargoTransportConfig, error) {
	if catalog == nil || catalog.source == nil {
		return currentCargoTransportConfig{}, errDungeonDropSourceRequired
	}
	text, err := catalog.source.ReadText(currentCargoTransportConfigPath)
	if err != nil {
		return currentCargoTransportConfig{}, err
	}
	document, err := dnfpvf.Parse(currentCargoTransportConfigPath, text)
	if err != nil {
		return currentCargoTransportConfig{}, err
	}
	config := currentCargoTransportConfig{Rules: make(map[int64]currentCargoTransportRule)}
	tokens := document.Tokens
	for index := 0; index < len(tokens); index++ {
		if !isPVFSectionToken(tokens[index], "stone type") {
			continue
		}
		end := index + 1
		for end < len(tokens) && !isPVFSectionToken(tokens[end], "/stone type") {
			end++
		}
		if end >= len(tokens) {
			return currentCargoTransportConfig{}, fmt.Errorf("%w: unclosed stone type", errCurrentCargoTransportPVFInvalid)
		}
		stoneType, found := cargoFirstPVFInt(tokens[index+1 : end])
		if !found {
			return currentCargoTransportConfig{}, fmt.Errorf("%w: missing stone type", errCurrentCargoTransportPVFInvalid)
		}
		rule := currentCargoTransportRule{
			StoneType: stoneType, RarityWeights: make(map[int64]int64), EquipmentTypes: make(map[string]bool),
			IncludeItemIDs: make(map[uint32]bool), ExcludeItemIDs: make(map[uint32]bool), SoulItemIDs: make(map[int64]uint32),
			SoulWeights: make(map[int64][]int64), ElementWeights: make(map[int64][]int64),
		}
		group := tokens[index+1 : end]
		rule.GradeWeights = parsePVFRangeWeights(sectionPVFTokens(group, "cargo transport stone grade"))
		fillPVFIntPairs(rule.RarityWeights, sectionPVFTokens(group, "cargo transport stone rarity weight"))
		for _, value := range pvfTokenTexts(sectionPVFTokens(group, "cargo transport stone enable equip type")) {
			rule.EquipmentTypes[normalizeCargoEquipmentType(value)] = true
		}
		fillPVFItemSet(rule.IncludeItemIDs, sectionPVFTokens(group, "cargo transport stone include index"))
		fillPVFItemSet(rule.ExcludeItemIDs, sectionPVFTokens(group, "cargo transport stone except index"))
		for _, value := range pvfTokenTexts(sectionPVFTokens(group, "cargo transport stone except etc")) {
			switch strings.ToLower(strings.TrimSpace(value)) {
			case "growthtype":
				rule.ExceptGrowth = true
			case "periodequipment":
				rule.ExceptPeriod = true
			}
		}
		fillPVFItemMap(rule.SoulItemIDs, sectionPVFTokens(group, "cargo transport stone soul index"))
		// The current PVF stores one rarity key followed by four equipment
		// categories (weapon, armour, accessory, special equipment). Titles are
		// not part of the Grandi transfer table.
		fillPVFCategoryWeights(rule.SoulWeights, sectionPVFTokens(group, "cargo transport stone soul weight"), 4)
		elementValues := pvfTokenInts(sectionPVFTokens(group, "cargo transport stone element crystals cost weight"))
		if len(elementValues) > 0 && elementValues[0] > 0 && elementValues[0] <= math.MaxUint32 {
			rule.ElementItemID = uint32(elementValues[0])
			fillPVFCategoryWeightValues(rule.ElementWeights, elementValues[1:], 4)
		}
		rule.EpicPieceWeight = pvfTokenInts(sectionPVFTokens(group, "cargo transport stone epic piece cost weight"))
		particular := pvfTokenInts(sectionPVFTokens(group, "cargo transport stone particular cost"))
		for offset := 0; offset+3 < len(particular); offset += 4 {
			if particular[offset+2] <= 0 || particular[offset+2] > math.MaxUint32 || particular[offset+3] <= 0 {
				continue
			}
			rule.ParticularCosts = append(rule.ParticularCosts, currentCargoParticularCost{
				Rarity: particular[offset], Level: particular[offset+1], ItemID: uint32(particular[offset+2]), Count: particular[offset+3],
			})
		}
		config.Rules[stoneType] = rule
		index = end
	}
	if len(config.Rules) == 0 {
		return currentCargoTransportConfig{}, fmt.Errorf("%w: no stone rules", errCurrentCargoTransportPVFInvalid)
	}
	return config, nil
}

func loadCurrentResealConfig(catalog *pvfDungeonDropCatalog) (currentResealConfig, error) {
	if catalog == nil || catalog.source == nil {
		return currentResealConfig{}, errDungeonDropSourceRequired
	}
	text, err := catalog.source.ReadText(currentResealConfigPath)
	if err != nil {
		return currentResealConfig{}, err
	}
	document, err := dnfpvf.Parse(currentResealConfigPath, text)
	if err != nil {
		return currentResealConfig{}, err
	}
	config := currentResealConfig{
		GradeWeights:  parsePVFRangeWeights(documentSectionTokens(document, "grade")),
		RarityWeights: make(map[int64]float64), ExtendedWeights: make(map[int64]float64), SoulItemIDs: make(map[int64]uint32),
		SoulCostWeights: make(map[int64]float64), ResealConstants: make(map[int64]float64),
		ResealCountLimits: parsePVFRangeWeights(documentSectionTokens(document, "reseal count limit")),
	}
	fillPVFFloatSequence(config.RarityWeights, document.Numbers("rarity weight"))
	fillPVFFloatSequence(config.ExtendedWeights, document.Numbers("extended rarity weight"))
	fillPVFItemMap(config.SoulItemIDs, documentSectionTokens(document, "reseal soul index"))
	fillPVFFloatSequence(config.SoulCostWeights, document.Numbers("soul cost weight"))
	fillPVFFloatSequence(config.ResealConstants, document.Numbers("soul reseal constant"))
	if len(config.GradeWeights) == 0 || len(config.SoulItemIDs) == 0 {
		return currentResealConfig{}, fmt.Errorf("%w: incomplete reseal table", errCurrentCargoTransportPVFInvalid)
	}
	return config, nil
}

func (c currentResealConfig) materialAndCost(definition dungeonDropItemDefinition, resealCount byte) (uint32, int64, bool) {
	if definition.Rarity < 0 || definition.Rarity >= 7 || definition.MinimumLevel <= 0 || resealCount >= 7 {
		return 0, 0, false
	}
	gradeWeight := rangeWeightFor(c.GradeWeights, definition.MinimumLevel)
	weight := c.RarityWeights[definition.Rarity] * c.ExtendedWeights[definition.Rarity] * c.SoulCostWeights[definition.Rarity]
	constant := c.ResealConstants[definition.Rarity]
	if gradeWeight <= 0 || weight <= 0 || constant <= 0 {
		return 0, 0, false
	}
	cost := int64(float64(gradeWeight) * weight * math.Pow(constant, float64(resealCount)))
	itemID := c.SoulItemIDs[definition.Rarity]
	return itemID, cost, itemID > 0 && cost > 0
}

func (c currentResealConfig) waxCost(definition dungeonDropItemDefinition, resealCount byte) (int64, bool) {
	if definition.Rarity < 0 || definition.Rarity >= 7 || definition.MinimumLevel <= 0 || resealCount >= 7 {
		return 0, false
	}
	gradeWeight := rangeWeightFor(c.GradeWeights, definition.MinimumLevel)
	weight := c.RarityWeights[definition.Rarity] * c.ExtendedWeights[definition.Rarity]
	constant := c.ResealConstants[definition.Rarity]
	if gradeWeight <= 0 || weight <= 0 || constant <= 0 {
		return 0, false
	}
	cost := int64(float64(gradeWeight) * weight * math.Pow(constant, float64(resealCount)))
	return cost, cost > 0
}

func (c currentResealConfig) countLimit(level int64) byte {
	value := rangeWeightFor(c.ResealCountLimits, level)
	if value < 0 {
		return 0
	}
	if value > 7 {
		value = 7
	}
	return byte(value)
}

func currentPVFWaxKind(definition dungeonDropItemDefinition) (oneShot bool, valid bool) {
	// The current PVF does not give every one-shot golden wax a dedicated
	// marker or a stable filename. These are the five current-client item IDs
	// whose documents carry the native one-shot reseal contract (including
	// "does not increase the reseal count" at the normal seven-use limit).
	switch definition.ItemID {
	case 2660238, 10098656, 490000345, 10157922, 10164812:
		return true, true
	}
	cleanPath := strings.ToLower(strings.ReplaceAll(path.Clean(strings.ReplaceAll(definition.PVFPath, "\\", "/")), "\\", "/"))
	if definition.ResealOneShot || strings.Contains(path.Base(cleanPath), "candle_goldwax_oneshot") || strings.HasSuffix(cleanPath, "/10164812.stk") {
		return true, true
	}
	return false, cleanPath == "stackable/cash/candle_goldwax.stk" || cleanPath == "cash/candle_goldwax.stk"
}

func sectionPVFTokens(tokens []dnfpvf.Token, name string) []dnfpvf.Token {
	for index := 0; index < len(tokens); index++ {
		if !isPVFSectionToken(tokens[index], name) {
			continue
		}
		for end := index + 1; end < len(tokens); end++ {
			if isPVFSectionToken(tokens[end], "/"+name) {
				return tokens[index+1 : end]
			}
			if tokens[end].Kind == dnfpvf.TokenSection && !strings.HasPrefix(strings.TrimSpace(tokens[end].Value), "/") {
				return tokens[index+1 : end]
			}
		}
		return tokens[index+1:]
	}
	return nil
}

func documentSectionTokens(document *dnfpvf.Document, name string) []dnfpvf.Token {
	if document == nil {
		return nil
	}
	tokens, _ := document.Section(name)
	return tokens
}

func cargoFirstPVFInt(tokens []dnfpvf.Token) (int64, bool) {
	for _, token := range tokens {
		if token.Kind == dnfpvf.TokenInt {
			return token.Int, true
		}
	}
	return 0, false
}

func pvfTokenInts(tokens []dnfpvf.Token) []int64 {
	values := make([]int64, 0, len(tokens))
	for _, token := range tokens {
		if token.Kind == dnfpvf.TokenInt {
			values = append(values, token.Int)
		}
	}
	return values
}

func pvfTokenTexts(tokens []dnfpvf.Token) []string {
	values := make([]string, 0, len(tokens))
	for _, token := range tokens {
		if token.Kind == dnfpvf.TokenString || token.Kind == dnfpvf.TokenIdent {
			values = append(values, token.Value)
		}
	}
	return values
}

func parsePVFRangeWeights(tokens []dnfpvf.Token) []currentPVFRangeWeight {
	values := pvfTokenInts(tokens)
	result := make([]currentPVFRangeWeight, 0, len(values)/3)
	for index := 0; index+2 < len(values); index += 3 {
		result = append(result, currentPVFRangeWeight{Minimum: values[index], Maximum: values[index+1], Weight: values[index+2]})
	}
	return result
}

func rangeWeightFor(rows []currentPVFRangeWeight, value int64) int64 {
	for _, row := range rows {
		if value >= row.Minimum && value <= row.Maximum {
			return row.Weight
		}
	}
	return 0
}

func fillPVFIntPairs(target map[int64]int64, tokens []dnfpvf.Token) {
	values := pvfTokenInts(tokens)
	for index := 0; index+1 < len(values); index += 2 {
		target[values[index]] = values[index+1]
	}
}

func fillPVFItemMap(target map[int64]uint32, tokens []dnfpvf.Token) {
	values := pvfTokenInts(tokens)
	for index := 0; index+1 < len(values); index += 2 {
		if values[index+1] > 0 && values[index+1] <= math.MaxUint32 {
			target[values[index]] = uint32(values[index+1])
		}
	}
}

func fillPVFItemSet(target map[uint32]bool, tokens []dnfpvf.Token) {
	for _, value := range pvfTokenInts(tokens) {
		if value > 0 && value <= math.MaxUint32 {
			target[uint32(value)] = true
		}
	}
}

func fillPVFCategoryWeights(target map[int64][]int64, tokens []dnfpvf.Token, width int) {
	fillPVFCategoryWeightValues(target, pvfTokenInts(tokens), width)
}

func fillPVFCategoryWeightValues(target map[int64][]int64, values []int64, width int) {
	for index := 0; index < len(values); {
		rarity := values[index]
		index++
		if index+width > len(values) {
			break
		}
		target[rarity] = append([]int64(nil), values[index:index+width]...)
		index += width
	}
}

func fillPVFFloatSequence(target map[int64]float64, values []float64) {
	for index, value := range values {
		target[int64(index)] = value
	}
}

func normalizeCargoEquipmentType(value string) string {
	return strings.ToLower(strings.Trim(strings.TrimSpace(strings.ReplaceAll(value, "`", "")), "[]"))
}
