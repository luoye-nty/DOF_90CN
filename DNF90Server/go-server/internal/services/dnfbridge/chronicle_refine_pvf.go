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

const (
	currentChronicleCanonicalAuraBase     = 1254
	currentChronicleFailureFragmentItemID = 3311
)

func (s *Service) alignedChronicleRefineResolverForCommand(opcode dnfenum.CmdPacket) (alignedcmd.ChronicleRefineResolver, error) {
	if opcode != dnfenum.CmdPacketEnchant3rdChronicleItem {
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
	return func(materialItemID, targetItemID int64, characterJob, firstGrowType, optionNo byte) (alignedcmd.ChronicleRefineResolution, error) {
		return resolveCurrentChronicleRefineMetadata(catalog, source, materialItemID, targetItemID, characterJob, firstGrowType, optionNo)
	}, nil
}

func resolveCurrentChronicleRefineMetadata(
	catalog *pvfDungeonDropCatalog,
	source dnfpvf.Source,
	materialItemID int64,
	targetItemID int64,
	characterJob byte,
	firstGrowType byte,
	optionNo byte,
) (alignedcmd.ChronicleRefineResolution, error) {
	resolution := alignedcmd.ChronicleRefineResolution{}
	if catalog == nil || source == nil {
		return resolution, errDungeonDropSourceRequired
	}
	if materialItemID <= 0 || materialItemID > math.MaxUint32 || targetItemID <= 0 || targetItemID > math.MaxUint32 {
		return resolution, nil
	}

	material, err := catalog.ResolveItem(uint32(materialItemID))
	if err != nil {
		if errors.Is(err, errDungeonDropItemUnresolved) {
			return resolution, nil
		}
		return resolution, err
	}
	if material.Kind != dungeonDropItemStackable {
		return resolution, nil
	}
	materialDocument, err := parseDungeonCardPVFDocument(source, material.PVFPath)
	if err != nil {
		return resolution, fmt.Errorf("parse chronicle material item=%d path=%s: %w", materialItemID, material.PVFPath, err)
	}
	importScript, found := materialDocument.Text("import script")
	if !found || strings.TrimSpace(importScript) == "" {
		return resolution, nil
	}
	auraType, found := materialDocument.Int("type")
	if !found || auraType < 0 || auraType > 2 {
		return resolution, nil
	}
	probabilities := materialDocument.Ints("probability")
	if len(probabilities) < 2 || probabilities[0] < 0 || probabilities[0] > 100 || probabilities[1] < 0 || probabilities[1] > 100 {
		return resolution, nil
	}

	target, err := catalog.ResolveItem(uint32(targetItemID))
	if err != nil {
		if errors.Is(err, errDungeonDropItemUnresolved) {
			return resolution, nil
		}
		return resolution, err
	}
	if target.Kind != dungeonDropItemEquipment || target.Rarity != 5 {
		return resolution, nil
	}
	targetType, targetTypeCode, ok := currentChronicleEquipmentType(target.EquipmentType)
	if !ok {
		return resolution, nil
	}
	targetDocument, err := parseDungeonCardPVFDocument(source, target.PVFPath)
	if err != nil {
		return resolution, fmt.Errorf("parse chronicle target item=%d path=%s: %w", targetItemID, target.PVFPath, err)
	}
	if !currentChronicleCharacterItemCheckMatches(targetDocument, characterJob, firstGrowType, targetType) {
		return resolution, nil
	}

	importedPath, importedText, err := readDungeonDropText(source, "stackable", importScript)
	if err != nil {
		return resolution, fmt.Errorf("read chronicle import script=%s: %w", importScript, err)
	}
	importedDocument, err := dnfpvf.Parse(importedPath, importedText)
	if err != nil {
		return resolution, fmt.Errorf("parse chronicle import script=%s: %w", importedPath, err)
	}
	if !currentChronicleImportedOptionMatches(importedDocument, characterJob, firstGrowType, targetType, optionNo) {
		return resolution, nil
	}
	disjointConfig, err := loadCurrentDisjointPVFConfig(source)
	if err != nil {
		return resolution, fmt.Errorf("load chronicle failure disjoint config: %w", err)
	}
	cubeReward, _, _, err := currentEquipmentDisjointCubeReward(disjointConfig, targetDocument)
	if err != nil {
		return resolution, fmt.Errorf("resolve chronicle failure cube target=%d path=%s: %w", targetItemID, target.PVFPath, err)
	}
	cubeItem, err := resolveCurrentMagicBoxRewardItem(catalog, source, int64(cubeReward.ItemID))
	if err != nil {
		return resolution, fmt.Errorf("resolve chronicle failure cube item=%d: %w", cubeReward.ItemID, err)
	}
	needMaterial := materialDocument.Ints("need material")
	if len(needMaterial) < 1 || needMaterial[0] != currentChronicleFailureFragmentItemID {
		return resolution, nil
	}
	fragmentItem, err := resolveCurrentMagicBoxRewardItem(catalog, source, currentChronicleFailureFragmentItemID)
	if err != nil {
		return resolution, fmt.Errorf("resolve chronicle failure fragment item=%d: %w", currentChronicleFailureFragmentItemID, err)
	}
	resolution.MaterialPVFPath = material.PVFPath
	resolution.ImportedScriptPVFPath = importedPath
	resolution.AuraType = byte(auraType)
	resolution.CanonicalAuraOptionID = int32(currentChronicleCanonicalAuraBase + auraType)
	resolution.Probabilities = [2]byte{byte(probabilities[0]), byte(probabilities[1])}
	resolution.FailureFragmentItemID = currentChronicleFailureFragmentItemID
	resolution.FailureCubeCount = int64(cubeReward.Count)
	resolution.FailureCubeItem = cubeItem
	resolution.FailureFragmentItem = fragmentItem
	resolution.TargetPVFPath = target.PVFPath
	resolution.TargetKind = string(target.Kind)
	resolution.TargetEquipmentType = targetType
	resolution.TargetEquipmentTypeCode = targetTypeCode
	resolution.TargetDurability = target.Durability
	resolution.Matched = true
	return resolution, nil
}

func currentChronicleCharacterItemCheckMatches(document *dnfpvf.Document, job, grow byte, equipmentType string) bool {
	if document == nil {
		return false
	}
	for _, section := range document.Sections {
		if !strings.EqualFold(strings.TrimSpace(section.Name), "character item check") || section.Start < 0 || section.End > len(document.Tokens) {
			continue
		}
		if currentChronicleCheckTupleMatches(document.Tokens[section.Start:section.End], job, grow, equipmentType) {
			return true
		}
	}
	return false
}

func currentChronicleImportedOptionMatches(document *dnfpvf.Document, job, grow byte, equipmentType string, optionNo byte) bool {
	if document == nil {
		return false
	}
	inEnchant := false
	matchingCheck := false
	for _, section := range document.Sections {
		name := strings.ToLower(strings.TrimSpace(section.Name))
		switch name {
		case "3choro enchant":
			inEnchant = true
			matchingCheck = false
		case "/3choro enchant":
			return false
		case "check":
			matchingCheck = false
			if !inEnchant || section.Start < 0 || section.End > len(document.Tokens) {
				continue
			}
			matchingCheck = currentChronicleCheckTupleMatches(
				document.Tokens[section.Start:section.End],
				job,
				grow,
				equipmentType,
			)
		case "/check":
			matchingCheck = false
		case "skill":
			if !inEnchant || !matchingCheck || section.Start < 0 || section.End > len(document.Tokens) {
				continue
			}
			ints, _ := currentChronicleSectionValues(document.Tokens[section.Start:section.End])
			if len(ints) >= 1 && ints[0] == int64(optionNo) {
				return true
			}
		}
	}
	return false
}

// currentChronicleCheckTupleMatches preserves the PVF value order while
// walking one [check] or [character item check] section. Shared Chronicle
// equipment stores more than one compatibility tuple in the same section,
// for example male/female gunner as:
//
//	2 1 `weapon` 5 1 `weapon`
//
// Splitting all integers from all text values loses those tuple boundaries
// and only validates the first profession. The current client evaluates every
// (job, first-grow, equipment-type) tuple in source order.
func currentChronicleCheckTupleMatches(tokens []dnfpvf.Token, job, grow byte, equipmentType string) bool {
	for index := 0; index+2 < len(tokens); index++ {
		if tokens[index].Kind != dnfpvf.TokenInt || tokens[index].Int != int64(job) ||
			tokens[index+1].Kind != dnfpvf.TokenInt || tokens[index+1].Int != int64(grow) {
			continue
		}
		typeToken := tokens[index+2]
		if typeToken.Kind != dnfpvf.TokenString && typeToken.Kind != dnfpvf.TokenIdent {
			continue
		}
		if currentChronicleNormalizeType(typeToken.Value) == equipmentType {
			return true
		}
	}
	return false
}

func currentChronicleSectionValues(tokens []dnfpvf.Token) ([]int64, []string) {
	ints := make([]int64, 0, len(tokens))
	texts := make([]string, 0, len(tokens))
	for _, token := range tokens {
		switch token.Kind {
		case dnfpvf.TokenInt:
			ints = append(ints, token.Int)
		case dnfpvf.TokenString, dnfpvf.TokenIdent:
			texts = append(texts, token.Value)
		}
	}
	return ints, texts
}

func currentChronicleEquipmentType(raw string) (string, byte, bool) {
	typeName := currentChronicleNormalizeType(raw)
	switch typeName {
	case "weapon":
		return typeName, 12, true
	case "coat":
		return typeName, 14, true
	case "shoulder":
		return typeName, 15, true
	case "pants":
		return typeName, 16, true
	case "shoes":
		return typeName, 17, true
	case "waist":
		return typeName, 18, true
	case "amulet":
		return typeName, 19, true
	case "wrist":
		return typeName, 20, true
	case "ring":
		return typeName, 21, true
	case "support":
		return typeName, 22, true
	case "magic stone":
		return typeName, 23, true
	case "earring":
		return typeName, 25, true
	default:
		return typeName, 0, false
	}
}

func currentChronicleNormalizeType(raw string) string {
	return strings.ToLower(strings.TrimSpace(strings.Trim(strings.TrimSpace(strings.ReplaceAll(raw, "`", "")), "[]")))
}
