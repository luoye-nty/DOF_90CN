package dnfbridge

import (
	"fmt"
	"strings"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
)

const currentChronicleGrowthCostPVFPath = "Etc/equipmentlevelemancipate.etc"

// alignedChronicleGrowthResolverForCommand keeps op271 policy entirely in the
// active PVF. The request only supplies item ids/slots; the owner revalidates
// those values inside its inventory transaction.
func (s *Service) alignedChronicleGrowthResolverForCommand(opcode dnfenum.CmdPacket) (alignedcmd.ChronicleGrowthResolver, error) {
	if opcode != dnfenum.CmdPacketUpgradeChronicle {
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
	return func(ticketItemID, targetItemID int64) (alignedcmd.ChronicleGrowthResolution, error) {
		return resolveCurrentChronicleGrowthMetadata(catalog, source, ticketItemID, targetItemID)
	}, nil
}

func resolveCurrentChronicleGrowthMetadata(catalog *pvfDungeonDropCatalog, source dnfpvf.Source, ticketItemID, targetItemID int64) (alignedcmd.ChronicleGrowthResolution, error) {
	if catalog == nil || source == nil {
		return alignedcmd.ChronicleGrowthResolution{}, errDungeonDropSourceRequired
	}
	ticket, err := catalog.ResolveItem(uint32(ticketItemID))
	if err != nil {
		return alignedcmd.ChronicleGrowthResolution{}, fmt.Errorf("resolve chronicle ticket %d: %w", ticketItemID, err)
	}
	if ticket.Kind != dungeonDropItemStackable {
		return alignedcmd.ChronicleGrowthResolution{}, nil
	}
	ticketDoc, err := parseDungeonCardPVFDocument(source, ticket.PVFPath)
	if err != nil {
		return alignedcmd.ChronicleGrowthResolution{}, fmt.Errorf("parse chronicle ticket %d path=%s: %w", ticketItemID, ticket.PVFPath, err)
	}
	target, err := catalog.ResolveItem(uint32(targetItemID))
	if err != nil {
		return alignedcmd.ChronicleGrowthResolution{}, fmt.Errorf("resolve chronicle target %d: %w", targetItemID, err)
	}
	if target.Kind != dungeonDropItemEquipment {
		return alignedcmd.ChronicleGrowthResolution{}, nil
	}
	targetDoc, err := parseDungeonCardPVFDocument(source, target.PVFPath)
	if err != nil {
		return alignedcmd.ChronicleGrowthResolution{}, fmt.Errorf("parse chronicle target %d path=%s: %w", targetItemID, target.PVFPath, err)
	}
	// Current PVF reuses [emancipate ticket] on both the old chronicle-growth
	// tickets and the equipment-conversion books. The former are selected by
	// their complete [equipment level emancipate] policy; only the latter use
	// the target equipment's [emancipate] recipe.
	upgrade := firstPVFInt(ticketDoc, "equipment upgrade level")
	if ticketType, found := ticketDoc.Int("emancipate ticket"); found && upgrade <= 0 {
		conversion, conversionErr := resolveCurrentEquipmentEmancipateMetadata(
			catalog,
			ticketDoc,
			targetDoc,
			ticket,
			target,
			ticketType,
		)
		if conversionErr != nil {
			return alignedcmd.ChronicleGrowthResolution{}, conversionErr
		}
		return alignedcmd.ChronicleGrowthResolution{
			TicketItemID:  ticketItemID,
			TicketPVFPath: ticket.PVFPath,
			Conversion:    conversion,
		}, nil
	}
	if upgrade <= 0 {
		return alignedcmd.ChronicleGrowthResolution{}, nil
	}
	conditionRarities, conditionMinimum, conditionMaximum := currentChronicleCondition(ticketDoc)
	probability := parseChronicleProbabilities(ticketDoc)
	if len(probability) == 0 {
		return alignedcmd.ChronicleGrowthResolution{}, fmt.Errorf("chronicle ticket %d has no complete probability table", ticketItemID)
	}
	if !chronicleAllowedRarity(conditionRarities, target.Rarity) {
		return alignedcmd.ChronicleGrowthResolution{}, nil
	}
	policies, err := currentChronicleGrowthCostPoliciesFromSource(source)
	if err != nil {
		return alignedcmd.ChronicleGrowthResolution{}, err
	}
	cost, ok := policies[target.Rarity]
	if !ok || cost.MaterialItemID <= 0 || cost.BaseCount <= 0 || cost.Multiplier <= 0 {
		return alignedcmd.ChronicleGrowthResolution{}, nil
	}
	minimum := int(target.MinimumLevel)
	if conditionMinimum > 0 && minimum < int(conditionMinimum) {
		minimum = int(conditionMinimum)
	}
	maximum := int(conditionMaximum)
	if maximum <= minimum {
		return alignedcmd.ChronicleGrowthResolution{}, nil
	}
	return alignedcmd.ChronicleGrowthResolution{
		TicketItemID:       ticketItemID,
		MaterialItemID:     cost.MaterialItemID,
		UpgradeLevel:       int(upgrade),
		TargetMinimumLevel: int(target.MinimumLevel),
		ConditionMinimum:   minimum,
		ConditionMaximum:   maximum,
		TargetRarity:       target.Rarity,
		EquipmentType:      normalizeChronicleEquipmentType(target.EquipmentType),
		AllowedRarities:    conditionRarities,
		Probability:        probability,
		CostPolicy:         cost,
		TicketPVFPath:      ticket.PVFPath,
	}, nil
}

func resolveCurrentEquipmentEmancipateMetadata(
	catalog *pvfDungeonDropCatalog,
	ticketDoc *dnfpvf.Document,
	targetDoc *dnfpvf.Document,
	ticket dungeonDropItemDefinition,
	target dungeonDropItemDefinition,
	ticketType int64,
) (*alignedcmd.ChronicleEquipmentConversionResolution, error) {
	if catalog == nil || ticketDoc == nil || targetDoc == nil || ticketType < 0 {
		return nil, fmt.Errorf("invalid equipment emancipation metadata")
	}
	targetTypes := targetDoc.Ints("emancipate type")
	if len(targetTypes) != 1 || targetTypes[0] != ticketType {
		return nil, fmt.Errorf("equipment emancipation type mismatch ticket=%d target=%v", ticketType, targetTypes)
	}
	inputValues := targetDoc.Ints("input")
	outputValues := targetDoc.Ints("output")
	if len(inputValues) == 0 || len(inputValues)%2 != 0 || len(outputValues) != 2 || outputValues[0] <= 0 || outputValues[1] != 1 {
		return nil, fmt.Errorf("invalid equipment emancipation recipe target=%d inputs=%v outputs=%v", target.ItemID, inputValues, outputValues)
	}
	inputs := make([]alignedcmd.ChronicleEquipmentConversionMaterial, 0, len(inputValues)/2)
	for index := 0; index < len(inputValues); index += 2 {
		if inputValues[index] <= 0 || inputValues[index+1] <= 0 {
			return nil, fmt.Errorf("invalid equipment emancipation input target=%d values=%v", target.ItemID, inputValues)
		}
		inputs = append(inputs, alignedcmd.ChronicleEquipmentConversionMaterial{ItemID: inputValues[index], Count: inputValues[index+1]})
	}
	output, err := catalog.ResolveItem(uint32(outputValues[0]))
	if err != nil {
		return nil, fmt.Errorf("resolve equipment emancipation output %d: %w", outputValues[0], err)
	}
	if output.Kind != dungeonDropItemEquipment || strings.TrimSpace(output.PVFPath) == "" || strings.TrimSpace(output.EquipmentType) == "" {
		return nil, fmt.Errorf("invalid equipment emancipation output %d", outputValues[0])
	}
	qualitySeed, err := newCurrentEquipmentQualitySeed()
	if err != nil {
		return nil, fmt.Errorf("generate equipment emancipation quality seed: %w", err)
	}
	upgradeMaximum := currentOptionalPVFInt(ticketDoc, "emancipate grade max")
	amplifyMaximum := currentOptionalPVFInt(ticketDoc, "emancipate amplify max")
	forgingMaximum := currentOptionalPVFInt(ticketDoc, "emancipate genuineGrade max")
	return &alignedcmd.ChronicleEquipmentConversionResolution{
		TicketType:          ticketType,
		PreserveAttributes:  currentEquipmentEmancipationPreservesAttributes(ticketType),
		Inputs:              inputs,
		OutputItemID:        int64(output.ItemID),
		OutputPVFPath:       output.PVFPath,
		OutputEquipmentType: output.EquipmentType,
		OutputRarity:        output.Rarity,
		OutputGrade:         output.Grade,
		OutputMinimumLevel:  output.MinimumLevel,
		OutputDurability:    output.Durability,
		OutputQualitySeed:   qualitySeed,
		BindOutput:          ticketType >= 8 && ticketType <= 20,
		UpgradeMaximum:      upgradeMaximum,
		AmplifyMaximum:      amplifyMaximum,
		ForgingMaximum:      forgingMaximum,
	}, nil
}

func currentEquipmentEmancipationPreservesAttributes(ticketType int64) bool {
	switch ticketType {
	case 1, 3, 6, 7, 19:
		return true
	default:
		return false
	}
}

func currentOptionalPVFInt(document *dnfpvf.Document, section string) int {
	if document == nil {
		return -1
	}
	value, found := document.Int(section)
	if !found || value < 0 {
		return -1
	}
	return int(value)
}

func currentChronicleGrowthCostPoliciesFromSource(source dnfpvf.Source) (map[int64]alignedcmd.ChronicleGrowthCostPolicy, error) {
	text, err := source.ReadText(currentChronicleGrowthCostPVFPath)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", currentChronicleGrowthCostPVFPath, err)
	}
	document, err := dnfpvf.Parse(currentChronicleGrowthCostPVFPath, text)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", currentChronicleGrowthCostPVFPath, err)
	}
	return parseCurrentChronicleGrowthCostPolicies(document)
}

func parseCurrentChronicleGrowthCostPolicies(document *dnfpvf.Document) (map[int64]alignedcmd.ChronicleGrowthCostPolicy, error) {
	if document == nil {
		return nil, fmt.Errorf("document required")
	}
	partWeights := parseChroniclePartWeights(document)
	levelWeights := parseChronicleWeightSegments(document, "weight by level")
	upgradeWeights := parseChronicleWeightSegments(document, "weight by upgrade")
	amplifyWeights := parseChronicleWeightSegments(document, "weight by amplify")
	forgingWeights := parseChronicleWeightSegments(document, "weight by seperate")
	if len(partWeights) == 0 || len(levelWeights) == 0 || len(upgradeWeights) == 0 || len(amplifyWeights) == 0 || len(forgingWeights) == 0 {
		return nil, fmt.Errorf("incomplete chronicle growth weight tables")
	}
	values := document.Numbers("consume item by rarity")
	if len(values) == 0 || len(values)%4 != 0 {
		return nil, fmt.Errorf("invalid [consume item by rarity] tuple count %d", len(values))
	}
	policies := make(map[int64]alignedcmd.ChronicleGrowthCostPolicy, len(values)/4)
	for i := 0; i < len(values); i += 4 {
		rarity, material, base := int64(values[i]), int64(values[i+1]), int64(values[i+2])
		multiplier := values[i+3]
		if rarity < 0 || material <= 0 || base <= 0 || multiplier <= 0 {
			return nil, fmt.Errorf("invalid rarity cost tuple rarity=%d material=%d count=%d multiplier=%g", rarity, material, base, multiplier)
		}
		policies[rarity] = alignedcmd.ChronicleGrowthCostPolicy{
			Rarity: rarity, MaterialItemID: material, BaseCount: base, Multiplier: multiplier,
			PartWeights: partWeights, LevelWeights: levelWeights, UpgradeWeights: upgradeWeights,
			AmplifyWeights: amplifyWeights, ForgingWeights: forgingWeights,
		}
	}
	return policies, nil
}

func parseChroniclePartWeights(document *dnfpvf.Document) map[string]float64 {
	tokens, ok := document.Section("weight by weapon")
	if !ok {
		return nil
	}
	weights := make(map[string]float64)
	for i := 0; i+1 < len(tokens); i++ {
		if tokens[i].Kind != dnfpvf.TokenString && tokens[i].Kind != dnfpvf.TokenIdent {
			continue
		}
		var weight float64
		switch tokens[i+1].Kind {
		case dnfpvf.TokenInt:
			weight = float64(tokens[i+1].Int)
		case dnfpvf.TokenFloat:
			weight = tokens[i+1].Float
		default:
			continue
		}
		part := normalizeChronicleEquipmentType(tokens[i].Value)
		if part != "" && weight > 0 {
			weights[part] = weight
		}
		i++
	}
	return weights
}

func parseChronicleWeightSegments(document *dnfpvf.Document, section string) []alignedcmd.ChronicleGrowthWeightSegment {
	values := document.Numbers(section)
	if len(values) == 0 || len(values)%4 != 0 {
		return nil
	}
	segments := make([]alignedcmd.ChronicleGrowthWeightSegment, 0, len(values)/4)
	for i := 0; i < len(values); i += 4 {
		minimum, maximum := int(values[i]), int(values[i+1])
		minimumWeight, maximumWeight := values[i+2], values[i+3]
		if maximum < minimum || minimumWeight <= 0 || maximumWeight <= 0 {
			return nil
		}
		segments = append(segments, alignedcmd.ChronicleGrowthWeightSegment{Minimum: minimum, Maximum: maximum, MinimumWeight: minimumWeight, MaximumWeight: maximumWeight})
	}
	return segments
}

func parseChronicleProbabilities(document *dnfpvf.Document) []alignedcmd.ChronicleGrowthProbability {
	values := document.Numbers("probability")
	if len(values) == 0 || len(values)%2 != 0 {
		return nil
	}
	rows := make([]alignedcmd.ChronicleGrowthProbability, 0, len(values)/2)
	for i := 0; i < len(values); i += 2 {
		maximum, weight := int(values[i]), int64(values[i+1])
		if maximum < 0 || weight < 0 || weight > 100000 {
			return nil
		}
		rows = append(rows, alignedcmd.ChronicleGrowthProbability{MaximumLevel: maximum, Weight: weight})
	}
	return rows
}

func normalizeChronicleEquipmentType(value string) string {
	value = strings.ToLower(strings.Trim(strings.TrimSpace(value), "`[] "))
	if value == "wrist" {
		return "wrisat"
	}
	return value
}

func currentChronicleCondition(document *dnfpvf.Document) (rarities []int64, minimum, maximum int64) {
	if document == nil {
		return nil, 0, 0
	}
	inCondition := false
	for _, section := range document.Sections {
		name := strings.ToLower(strings.TrimSpace(section.Name))
		switch name {
		case "equipment condition":
			inCondition = true
			continue
		case "/equipment condition":
			return rarities, minimum, maximum
		}
		if !inCondition || section.Start < 0 || section.End > len(document.Tokens) || section.Start > section.End {
			continue
		}
		values := make([]int64, 0, section.End-section.Start)
		for _, token := range document.Tokens[section.Start:section.End] {
			if token.Kind == dnfpvf.TokenInt {
				values = append(values, token.Int)
			}
		}
		if len(values) == 0 {
			continue
		}
		switch name {
		case "rarity":
			rarities = append(rarities, values...)
		case "minimum level":
			minimum = values[0]
		case "maximum level":
			maximum = values[0]
		}
	}
	return rarities, minimum, maximum
}

func firstPVFInt(document *dnfpvf.Document, section string) int64 {
	if document == nil {
		return 0
	}
	values := document.Ints(section)
	if len(values) == 0 {
		return 0
	}
	return values[0]
}

func chronicleAllowedRarity(allowed []int64, rarity int64) bool {
	if len(allowed) == 0 {
		return true
	}
	for _, value := range allowed {
		if value == rarity {
			return true
		}
	}
	return false
}
