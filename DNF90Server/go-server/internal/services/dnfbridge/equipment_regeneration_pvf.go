package dnfbridge

import (
	cryptorand "crypto/rand"
	"errors"
	"fmt"
	"math"
	"math/big"
	"sort"
	"strings"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

const (
	currentEquipmentRegenerationPath        = "etc/randomoption/regenerationrandomoption.etc"
	currentEquipmentRegenerationOverallPath = "etc/randomoption/randomizedoptionoverall1.etc"
)

type currentEquipmentRegenerationMaterial struct {
	itemID int64
	count  int64
}

type currentEquipmentRegenerationLevelWeight struct {
	current  int64
	plusFive int64
}

type currentEquipmentRegenerationCandidate struct {
	itemID        int64
	pvfPath       string
	equipmentType string
	itemGroup     string
	attachType    string
	itemCategory  string
	rarity        int64
	minimumLevel  int64
	creationRate  int64
	durability    uint16
	weight        int64
	legacy        bool
}

type currentEquipmentRegenerationCatalog struct {
	parts            map[uint16]map[string]bool
	excluded         map[int64]bool
	exceptionWeights map[int64]int64
	materials        map[int64][]currentEquipmentRegenerationMaterial
	costs            map[int64]map[int64]int64
	levelWeights     map[int64]map[int64]currentEquipmentRegenerationLevelWeight
	candidates       map[[2]int64][]currentEquipmentRegenerationCandidate
	byItemID         map[int64]currentEquipmentRegenerationCandidate
}

func (s *Service) alignedEquipmentRegenerationResolverForCommand(opcode dnfenum.CmdPacket) (alignedcmd.EquipmentRegenerationResolver, error) {
	if opcode != dnfenum.CmdPacketRegenerationRandomOption {
		return nil, nil
	}
	catalog, err := s.cachedEquipmentRegenerationCatalog()
	if err != nil {
		return nil, err
	}
	return func(sourceItemID int64, mode uint16, part uint16) (alignedcmd.EquipmentRegenerationResolution, error) {
		return catalog.resolve(sourceItemID, mode, part)
	}, nil
}

func (s *Service) cachedEquipmentRegenerationCatalog() (*currentEquipmentRegenerationCatalog, error) {
	if s == nil {
		return nil, errDungeonDropSourceRequired
	}
	s.equipmentRegenerationMu.Lock()
	defer s.equipmentRegenerationMu.Unlock()
	if s.equipmentRegenerationCatalog == nil && s.equipmentRegenerationLoadErr == nil {
		catalog, err := s.currentPVFItemCatalog()
		if err == nil {
			s.equipmentRegenerationCatalog, err = loadCurrentEquipmentRegenerationCatalog(catalog)
		}
		s.equipmentRegenerationLoadErr = err
	}
	if s.equipmentRegenerationLoadErr != nil {
		return nil, s.equipmentRegenerationLoadErr
	}
	return s.equipmentRegenerationCatalog, nil
}

func loadCurrentEquipmentRegenerationCatalog(items *pvfDungeonDropCatalog) (*currentEquipmentRegenerationCatalog, error) {
	if items == nil || items.source == nil {
		return nil, errDungeonDropSourceRequired
	}
	configDocument, err := parseCurrentEquipmentRegenerationDocument(items.source, currentEquipmentRegenerationPath)
	if err != nil {
		return nil, err
	}
	overallDocument, err := parseCurrentEquipmentRegenerationDocument(items.source, currentEquipmentRegenerationOverallPath)
	if err != nil {
		return nil, err
	}
	catalog := &currentEquipmentRegenerationCatalog{
		parts:            make(map[uint16]map[string]bool),
		excluded:         make(map[int64]bool),
		exceptionWeights: make(map[int64]int64),
		materials:        make(map[int64][]currentEquipmentRegenerationMaterial),
		costs:            make(map[int64]map[int64]int64),
		levelWeights:     make(map[int64]map[int64]currentEquipmentRegenerationLevelWeight),
		candidates:       make(map[[2]int64][]currentEquipmentRegenerationCandidate),
		byItemID:         make(map[int64]currentEquipmentRegenerationCandidate),
	}
	for _, section := range configDocument.Sections {
		key := currentRandomOptionSectionKey(section.Name)
		switch key {
		case "choose part":
			values := currentRandomOptionSectionValues(configDocument, section)
			if len(values) < 3 {
				continue
			}
			partValue, ok := values[0].integer()
			if !ok || partValue <= 0 || partValue > math.MaxUint16 {
				continue
			}
			groups := make(map[string]bool)
			for _, value := range values[2:] {
				if group := normalizeEquipmentRegenerationToken(value.text); group != "" {
					groups[group] = true
				}
			}
			if len(groups) > 0 {
				catalog.parts[uint16(partValue)] = groups
			}
		case "except item index":
			for _, itemID := range currentRandomOptionSectionInts(configDocument, section) {
				if itemID > 0 {
					catalog.excluded[itemID] = true
				}
			}
		case "probability exception handling":
			values := currentRandomOptionSectionNumbers(configDocument, section)
			for index := 0; index+1 < len(values); index += 2 {
				itemID := int64(values[index].value)
				weight := int64(math.Round(values[index+1].value * 1000))
				if itemID > 0 && weight > 0 {
					catalog.exceptionWeights[itemID] = weight
				}
			}
		case "compound need material for specific":
			catalog.materials = parseCurrentEquipmentRegenerationMaterials(configDocument, section)
		case "compound specific apply cost by level":
			catalog.costs = parseCurrentEquipmentRegenerationCosts(configDocument, section)
		}
	}
	for _, section := range overallDocument.Sections {
		if currentRandomOptionSectionKey(section.Name) != "regen level limit" {
			continue
		}
		values := currentRandomOptionSectionInts(overallDocument, section)
		for index := 0; index+3 < len(values); index += 4 {
			group, level := values[index], values[index+1]
			if group <= 0 || level < 0 || values[index+2] < 0 || values[index+3] < 0 {
				continue
			}
			if catalog.levelWeights[group] == nil {
				catalog.levelWeights[group] = make(map[int64]currentEquipmentRegenerationLevelWeight)
			}
			catalog.levelWeights[group][level] = currentEquipmentRegenerationLevelWeight{current: values[index+2], plusFive: values[index+3]}
		}
	}
	if len(catalog.parts) == 0 || len(catalog.materials) == 0 || len(catalog.costs) == 0 {
		return nil, fmt.Errorf("equipment regeneration PVF config incomplete: parts=%d materials=%d costs=%d", len(catalog.parts), len(catalog.materials), len(catalog.costs))
	}

	ids := make([]int, 0, len(items.itemRefs))
	for itemID, reference := range items.itemRefs {
		if reference.kind == dungeonDropItemEquipment {
			ids = append(ids, int(itemID))
		}
	}
	sort.Ints(ids)
	for _, rawID := range ids {
		itemID := uint32(rawID)
		reference := items.itemRefs[itemID]
		docPath, text, readErr := readDungeonDropText(items.source, string(dungeonDropItemEquipment), reference.path)
		if readErr != nil {
			continue
		}
		document, parseErr := dnfpvf.Parse(docPath, text)
		if parseErr != nil {
			continue
		}
		rarity, rarityOK := document.Int("rarity")
		minimumLevel, levelOK := document.Int("minimum level")
		itemGroup, groupOK := document.Text("item group name")
		attachType, attachOK := document.Text("attach type")
		equipmentType, typeOK := document.Text("equipment type")
		if !rarityOK || !levelOK || !groupOK || !attachOK || !typeOK || rarity < 0 || minimumLevel < 0 {
			continue
		}
		creationRate, _ := document.Int("creation rate")
		itemCategory, _ := document.Text("item category")
		legacy := normalizeEquipmentRegenerationToken(itemCategory) == "legacy"
		forceRule := document.Ints("force result item rule")
		if rarity == 3 && creationRate == 0 && len(forceRule) >= 2 && forceRule[0] == 1 && forceRule[1] == 0 {
			continue
		}
		if catalog.excluded[int64(itemID)] {
			continue
		}
		if !equipmentRegenerationEligible(rarity, minimumLevel, legacy, creationRate, catalog.exceptionWeights[int64(itemID)] > 0) {
			continue
		}
		weight := catalog.exceptionWeights[int64(itemID)]
		if weight == 0 {
			weight = 1000
		}
		durabilityValue, _ := document.Int("durability")
		if durabilityValue < 0 {
			durabilityValue = 0
		}
		if durabilityValue > math.MaxUint16 {
			durabilityValue = math.MaxUint16
		}
		candidate := currentEquipmentRegenerationCandidate{
			itemID: int64(itemID), pvfPath: docPath, equipmentType: equipmentType,
			itemGroup: normalizeEquipmentRegenerationToken(itemGroup), attachType: normalizeEquipmentRegenerationToken(attachType),
			itemCategory: normalizeEquipmentRegenerationToken(itemCategory), rarity: rarity, minimumLevel: minimumLevel,
			creationRate: creationRate, durability: uint16(durabilityValue), weight: weight, legacy: legacy,
		}
		catalog.byItemID[candidate.itemID] = candidate
		catalog.candidates[[2]int64{rarity, minimumLevel}] = append(catalog.candidates[[2]int64{rarity, minimumLevel}], candidate)
	}
	if len(catalog.byItemID) == 0 {
		return nil, fmt.Errorf("equipment regeneration candidate catalog empty")
	}
	return catalog, nil
}

func parseCurrentEquipmentRegenerationDocument(source dnfpvf.Source, documentPath string) (*dnfpvf.Document, error) {
	text, err := source.ReadText(documentPath)
	if err != nil {
		return nil, fmt.Errorf("read equipment regeneration PVF %s: %w", documentPath, err)
	}
	document, err := dnfpvf.Parse(documentPath, text)
	if err != nil {
		return nil, fmt.Errorf("parse equipment regeneration PVF %s: %w", documentPath, err)
	}
	return document, nil
}

func parseCurrentEquipmentRegenerationMaterials(document *dnfpvf.Document, section dnfpvf.Section) map[int64][]currentEquipmentRegenerationMaterial {
	values := currentRandomOptionSectionInts(document, section)
	result := make(map[int64][]currentEquipmentRegenerationMaterial)
	for index := 0; index+1 < len(values); {
		group, count := values[index], values[index+1]
		index += 2
		if group <= 0 || count <= 0 || count > 16 || index+int(count)*2 > len(values) {
			break
		}
		rows := make([]currentEquipmentRegenerationMaterial, 0, count)
		for row := int64(0); row < count; row++ {
			itemID, amount := values[index], values[index+1]
			index += 2
			if itemID > 0 && amount > 0 {
				rows = append(rows, currentEquipmentRegenerationMaterial{itemID: itemID, count: amount})
			}
		}
		if len(rows) > 0 {
			result[group] = rows
		}
	}
	return result
}

func parseCurrentEquipmentRegenerationCosts(document *dnfpvf.Document, section dnfpvf.Section) map[int64]map[int64]int64 {
	values := currentRandomOptionSectionNumbers(document, section)
	result := make(map[int64]map[int64]int64)
	for index := 0; index+2 < len(values); index += 3 {
		group := int64(values[index].value)
		level := int64(values[index+1].value)
		multiplier := int64(math.Round(values[index+2].value))
		if group <= 0 || level < 0 || multiplier <= 0 {
			continue
		}
		if result[group] == nil {
			result[group] = make(map[int64]int64)
		}
		result[group][level] = multiplier
	}
	return result
}

func (c *currentEquipmentRegenerationCatalog) resolve(sourceItemID int64, mode uint16, part uint16) (alignedcmd.EquipmentRegenerationResolution, error) {
	resolution := alignedcmd.EquipmentRegenerationResolution{SourceItemID: sourceItemID}
	if c == nil || sourceItemID <= 0 || mode != 0 || part == 0 {
		return resolution, nil
	}
	source, found := c.byItemID[sourceItemID]
	if !found || c.excluded[sourceItemID] || source.attachType != "sealing" || !c.parts[part][source.itemGroup] {
		return resolution, nil
	}
	group := source.rarity
	if source.legacy {
		group = 99
	}
	baseMaterials := c.materials[group]
	multiplier := floorEquipmentRegenerationValue(c.costs[group], source.minimumLevel)
	if len(baseMaterials) == 0 || multiplier <= 0 {
		return resolution, nil
	}
	materials := make([]alignedcmd.EquipmentRegenerationMaterial, 0, len(baseMaterials))
	for _, material := range baseMaterials {
		count := material.count * multiplier
		if count <= 0 {
			return resolution, nil
		}
		materials = append(materials, alignedcmd.EquipmentRegenerationMaterial{
			ItemID: material.itemID, Count: count, AccountShared: equipmentRegenerationAccountSharedItem(material.itemID),
		})
	}

	levelRule := floorEquipmentRegenerationLevelWeight(c.levelWeights[group], source.minimumLevel)
	type weightedPool struct {
		weight     int64
		candidates []currentEquipmentRegenerationCandidate
	}
	levels := []struct{ level, weight int64 }{{source.minimumLevel, levelRule.current}}
	if source.minimumLevel < 85 {
		levels = append(levels, struct{ level, weight int64 }{source.minimumLevel + 5, levelRule.plusFive})
	} else if levelRule.plusFive > 0 {
		levels = append(levels, struct{ level, weight int64 }{source.minimumLevel + 5, levelRule.plusFive})
	}
	if levelRule.current == 0 && levelRule.plusFive == 0 {
		levels = []struct{ level, weight int64 }{{source.minimumLevel, 1}}
	}
	pools := make([]weightedPool, 0, len(levels))
	for _, level := range levels {
		if level.weight <= 0 {
			continue
		}
		pool := weightedPool{weight: level.weight}
		for _, candidate := range c.candidates[[2]int64{source.rarity, level.level}] {
			// The output pool is every group the requested `[choose part]`
			// lists, not just the source's own `[item group name]`. Part 1
			// (武器) alone spans 37 weapon families; narrowing to the source
			// family collapsed the pool to a single candidate at every tier
			// where each family has one `[sealing]` piece per level -- which
			// is the whole 90 level band -- so regeneration always handed the
			// player back the exact item they fed in.
			if candidate.attachType != "sealing" || !c.parts[part][candidate.itemGroup] ||
				(source.legacy && !candidate.legacy) || (!source.legacy && candidate.legacy) {
				continue
			}
			pool.candidates = append(pool.candidates, candidate)
		}
		// A pool whose only member is the source is a guaranteed no-op: the
		// player pays the soul cost and gets the exact piece back. Drop it so
		// the attempt is refused up front instead of charged. Against the
		// active PVF this leaves out exactly one band -- 传承 rarity-2 weapons
		// at [minimum level] 9, where item 33947 (传承 : 神秘的银湖水仙之法杖)
		// is the only [sealing] piece in the game and its +5 band is empty.
		// Every other band has at least three candidates.
		if len(pool.candidates) == 1 && pool.candidates[0].itemID == sourceItemID {
			continue
		}
		if len(pool.candidates) > 0 {
			pools = append(pools, pool)
		}
	}
	if len(pools) == 0 {
		return resolution, nil
	}
	poolLevelWeights := make([]int64, len(pools))
	for index := range pools {
		poolLevelWeights[index] = pools[index].weight
	}
	poolIndex, err := secureEquipmentRegenerationChoice(poolLevelWeights)
	if err != nil {
		return resolution, err
	}
	pool := pools[poolIndex]
	weights := make([]int64, len(pool.candidates))
	for index := range pool.candidates {
		weights[index] = pool.candidates[index].weight
	}
	candidateIndex, err := secureEquipmentRegenerationChoice(weights)
	if err != nil {
		return resolution, err
	}
	selected := pool.candidates[candidateIndex]
	qualitySeed, err := newCurrentEquipmentQualitySeed()
	if err != nil {
		return resolution, err
	}
	return alignedcmd.EquipmentRegenerationResolution{
		Eligible: true, SourceItemID: sourceItemID, SourcePVFPath: source.pvfPath,
		ResultItemID: selected.itemID, ResultPVFPath: selected.pvfPath, ResultEquipmentType: selected.equipmentType,
		ResultRarity: selected.rarity, ResultMinimumLevel: selected.minimumLevel, ResultDurability: selected.durability,
		QualitySeed: qualitySeed, Materials: materials, CandidateCount: len(pool.candidates),
	}, nil
}

func secureEquipmentRegenerationChoice(weights []int64) (int, error) {
	total := int64(0)
	for _, weight := range weights {
		if weight <= 0 || weight > math.MaxInt64-total {
			continue
		}
		total += weight
	}
	if total <= 0 {
		return 0, errors.New("equipment regeneration candidate weights empty")
	}
	rollBig, err := cryptorand.Int(cryptorand.Reader, big.NewInt(total))
	if err != nil {
		return 0, err
	}
	roll := rollBig.Int64()
	for index, weight := range weights {
		if weight <= 0 {
			continue
		}
		if roll < weight {
			return index, nil
		}
		roll -= weight
	}
	return 0, errors.New("equipment regeneration candidate selection exhausted")
}

func floorEquipmentRegenerationValue(rows map[int64]int64, level int64) int64 {
	bestLevel, value := int64(-1), int64(0)
	for candidateLevel, candidateValue := range rows {
		if candidateLevel <= level && candidateLevel >= bestLevel {
			bestLevel, value = candidateLevel, candidateValue
		}
	}
	return value
}

func floorEquipmentRegenerationLevelWeight(rows map[int64]currentEquipmentRegenerationLevelWeight, level int64) currentEquipmentRegenerationLevelWeight {
	bestLevel := int64(-1)
	var value currentEquipmentRegenerationLevelWeight
	for candidateLevel, candidateValue := range rows {
		if candidateLevel <= level && candidateLevel >= bestLevel {
			bestLevel, value = candidateLevel, candidateValue
		}
	}
	return value
}

func equipmentRegenerationEligible(rarity, minimumLevel int64, legacy bool, creationRate int64, exception bool) bool {
	return (minimumLevel >= 85 && rarity == 6) || legacy || exception || creationRate > 0
}

// equipmentRegenerationAccountSharedItem 判断该道具是否住在账号共享晶体/灵魂仓库。
// 这里过去是一份手写的 12 条 ID 副本，现统一走权威表
// (repository/account_shared_materials.go)，避免与另两处副本漂移。
func equipmentRegenerationAccountSharedItem(itemID int64) bool {
	return dnfrepo.IsAccountSharedWarehouseItem(itemID)
}

func normalizeEquipmentRegenerationToken(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	return strings.Trim(value, "`[](){}<> \t\r\n")
}
