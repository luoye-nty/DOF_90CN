package dnfbridge

import (
	"errors"
	"fmt"
	"math"
	"path"
	"strconv"
	"strings"

	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
)

var currentAvatarCompoundPartNames = []string{"hat", "hair", "face", "neck", "coat", "pants", "belt", "shoes"}

type currentAvatarCompoundWeightedItem struct {
	ItemID uint32
	Weight uint32
}

type currentAvatarCompoundPartPool struct {
	Rare  []currentAvatarCompoundWeightedItem
	Upper []currentAvatarCompoundWeightedItem
}

type currentAvatarCompoundConfig struct {
	PVFPath          string
	Grade            int64
	UpperGrade       int64
	RareRates        map[string]int64
	UpperRareRates   map[string]int64
	Parts            map[string]currentAvatarCompoundPartPool
	MaterialBonuses  map[uint32]int64
	NormalMaterialID uint32
	UpperMaterialID  uint32
	MasterMaterialID uint32
	// UpperBonus / MasterBonus keep the two raw rate bonuses from
	// etc/serverparameter.etc, so a cube that is recognised by NAME (the event
	// re-issues of 钻石/黄金装扮合成器) can take its tier bonus without being one
	// of the three canonical ids.
	UpperBonus  int64
	MasterBonus int64
	// BlockMaterials are the [common bind cube] blocks' own [material] ids --
	// the 90-level cubes (升级的远古装扮合成器 10301984/10302329) that are NOT part
	// of the three-tier [material]/[material_upper]/[material_master] tail.
	BlockMaterials map[uint32]bool
}

// withCubeBonus returns a copy of the config whose material table also accepts
// itemID at the given rare-rate bonus. The config is shared between sessions, so
// the extension is applied to a copy rather than to the cached value.
func (c currentAvatarCompoundConfig) withCubeBonus(itemID uint32, bonus int64) currentAvatarCompoundConfig {
	bonuses := make(map[uint32]int64, len(c.MaterialBonuses)+1)
	for key, value := range c.MaterialBonuses {
		bonuses[key] = value
	}
	bonuses[itemID] = bonus
	c.MaterialBonuses = bonuses
	return c
}

func loadCurrentAvatarCompoundConfig(source dnfpvf.Source, job byte) (currentAvatarCompoundConfig, error) {
	if source == nil {
		return currentAvatarCompoundConfig{}, errCurrentAvatarCompoundUnavailable
	}
	indexText, err := source.ReadText("etc/compoundavatar.etc")
	if err != nil {
		return currentAvatarCompoundConfig{}, errors.Join(errCurrentAvatarCompoundUnavailable, err)
	}
	indexDocument, err := dnfpvf.Parse("etc/compoundavatar.etc", indexText)
	if err != nil {
		return currentAvatarCompoundConfig{}, errors.Join(errCurrentAvatarCompoundUnavailable, err)
	}
	files := make(map[int64]string)
	for offset := 0; offset+1 < len(indexDocument.Tokens); offset++ {
		left, right := indexDocument.Tokens[offset], indexDocument.Tokens[offset+1]
		if left.Kind == dnfpvf.TokenInt && (right.Kind == dnfpvf.TokenString || right.Kind == dnfpvf.TokenIdent) {
			files[left.Int] = strings.TrimSpace(right.Value)
		}
	}
	fileName := files[int64(job)+1]
	if fileName == "" {
		return currentAvatarCompoundConfig{}, fmt.Errorf("%w: job=%d", errCurrentAvatarCompoundUnavailable, job)
	}
	pvfPath := path.Join("etc", fileName)
	text, err := source.ReadText(pvfPath)
	if err != nil {
		return currentAvatarCompoundConfig{}, errors.Join(errCurrentAvatarCompoundUnavailable, err)
	}
	document, err := dnfpvf.Parse(pvfPath, text)
	if err != nil {
		return currentAvatarCompoundConfig{}, errors.Join(errCurrentAvatarCompoundUnavailable, err)
	}
	config := currentAvatarCompoundConfig{
		PVFPath:         pvfPath,
		RareRates:       currentAvatarCompoundRateSection(document, "rare rate"),
		UpperRareRates:  currentAvatarCompoundRateSection(document, "upper rare rate"),
		Parts:           make(map[string]currentAvatarCompoundPartPool, len(currentAvatarCompoundPartNames)),
		MaterialBonuses: make(map[uint32]int64, 3),
	}
	config.Grade, _ = currentAvatarCompoundFirstInt(document, "grade")
	config.UpperGrade, _ = currentAvatarCompoundFirstInt(document, "upper grade")
	for _, partName := range currentAvatarCompoundPartNames {
		values := currentAvatarCompoundFirstSectionInts(document, partName+" avatar")
		pool, parseErr := parseCurrentAvatarCompoundPartPool(values)
		if parseErr != nil {
			return currentAvatarCompoundConfig{}, fmt.Errorf("%w: %s [%s avatar]: %v", errCurrentAvatarCompoundUnavailable, pvfPath, partName, parseErr)
		}
		config.Parts[partName] = pool
	}
	config.NormalMaterialID = currentAvatarCompoundLastMaterialID(document, "material")
	config.UpperMaterialID = currentAvatarCompoundLastMaterialID(document, "material_upper")
	config.MasterMaterialID = currentAvatarCompoundLastMaterialID(document, "material_master")
	config.BlockMaterials = currentAvatarCompoundBlockMaterialIDs(document)
	serverText, err := source.ReadText("etc/serverparameter.etc")
	if err != nil {
		return currentAvatarCompoundConfig{}, errors.Join(errCurrentAvatarCompoundUnavailable, err)
	}
	serverDocument, err := dnfpvf.Parse("etc/serverparameter.etc", serverText)
	if err != nil {
		return currentAvatarCompoundConfig{}, errors.Join(errCurrentAvatarCompoundUnavailable, err)
	}
	upperBonus, _ := serverDocument.Int("upper bind cube rate bonus")
	masterBonus, _ := serverDocument.Int("master bind cube rate bonus")
	config.UpperBonus, config.MasterBonus = upperBonus, masterBonus
	config.MaterialBonuses[config.NormalMaterialID] = 0
	config.MaterialBonuses[config.UpperMaterialID] = upperBonus
	config.MaterialBonuses[config.MasterMaterialID] = masterBonus
	if config.Grade < 0 || config.UpperGrade < 0 || len(config.RareRates) != len(currentAvatarCompoundPartNames) ||
		len(config.UpperRareRates) != len(currentAvatarCompoundPartNames) || config.NormalMaterialID == 0 ||
		config.UpperMaterialID == 0 || config.MasterMaterialID == 0 || upperBonus < 0 || masterBonus < 0 {
		return currentAvatarCompoundConfig{}, fmt.Errorf("%w: incomplete %s", errCurrentAvatarCompoundUnavailable, pvfPath)
	}
	return config, nil
}

func currentAvatarCompoundFirstInt(document *dnfpvf.Document, name string) (int64, bool) {
	values := currentAvatarCompoundFirstSectionInts(document, name)
	if len(values) == 0 {
		return 0, false
	}
	return values[0], true
}

func currentAvatarCompoundFirstSectionInts(document *dnfpvf.Document, name string) []int64 {
	if document == nil {
		return nil
	}
	for _, section := range document.Sections {
		if !strings.EqualFold(strings.TrimSpace(section.Name), name) {
			continue
		}
		return currentAvatarCompoundTokenInts(currentDisjointSectionTokens(document, section))
	}
	return nil
}

// currentAvatarCompoundLastMaterialID reads the LAST [material]/[material_upper]/
// [material_master] section of the job file. Those live at the very end of the
// file (after the [common bind cube] blocks) and carry the three canonical cube
// ids as a `1 <itemID> 1` triple (count, item, consume).
func currentAvatarCompoundLastMaterialID(document *dnfpvf.Document, name string) uint32 {
	var tokens []dnfpvf.Token
	if document != nil {
		for _, section := range document.Sections {
			if strings.EqualFold(strings.TrimSpace(section.Name), name) {
				tokens = currentDisjointSectionTokens(document, section)
			}
		}
	}
	itemID, ok := currentAvatarCompoundMaterialID(tokens)
	if !ok {
		return 0
	}
	return itemID
}

// currentAvatarCompoundMaterialID reads one [material] body. Two shapes exist in
// the live PVF: a bare id (`[material] 10301984 [/material]`, used by every
// [common bind cube] block) and the `1 <id> 1` triple used by the three-tier
// tail. This mirrors the reference implementation
// (USDOF server_proto/game/character/avatar_compound_index.py:_material_item_id).
func currentAvatarCompoundMaterialID(tokens []dnfpvf.Token) (uint32, bool) {
	values := currentAvatarCompoundTokenInts(tokens)
	switch {
	case len(values) >= 3 && values[0] == 1 && values[2] == 1:
		if values[1] <= 0 || values[1] > math.MaxUint32 {
			return 0, false
		}
		return uint32(values[1]), true
	case len(values) == 1:
		if values[0] <= 0 || values[0] > math.MaxUint32 {
			return 0, false
		}
		return uint32(values[0]), true
	default:
		return 0, false
	}
}

// currentAvatarCompoundBlockMaterialIDs collects the [material] of every
// [common bind cube] block.
//
// The 90-level cubes are not part of the three-tier tail: each family is its own
// block that carries a class number (the client's op99 `material_class`, 13 for
// 升级的远古装扮合成器), the cube item in [material], a [notice] and its own
// [output] rare pools. Block cubes have no rate-bonus field of their own, so the
// caller prices them at the ordinary bonus and lets the block's pool decide what
// comes out. Live 2026-10-04: accepting only the three-tier tail rejected
// 10301984 with "synthesizer is invalid", which the client shows as an unusable
// cube.
func currentAvatarCompoundBlockMaterialIDs(document *dnfpvf.Document) map[uint32]bool {
	out := make(map[uint32]bool)
	if document == nil {
		return out
	}
	inBlock := false
	for _, section := range document.Sections {
		name := strings.ToLower(strings.TrimSpace(section.Name))
		switch name {
		case "common bind cube":
			inBlock = true
			continue
		case "/common bind cube":
			inBlock = false
			continue
		}
		if !inBlock || name != "material" {
			continue
		}
		if itemID, ok := currentAvatarCompoundMaterialID(currentDisjointSectionTokens(document, section)); ok {
			out[itemID] = true
		}
	}
	return out
}

func currentAvatarCompoundRateSection(document *dnfpvf.Document, name string) map[string]int64 {
	rates := make(map[string]int64)
	if document == nil {
		return rates
	}
	for _, section := range document.Sections {
		if !strings.EqualFold(strings.TrimSpace(section.Name), name) {
			continue
		}
		tokens := currentDisjointSectionTokens(document, section)
		for offset := 0; offset+1 < len(tokens); offset++ {
			if (tokens[offset].Kind == dnfpvf.TokenString || tokens[offset].Kind == dnfpvf.TokenIdent) && tokens[offset+1].Kind == dnfpvf.TokenInt {
				partName := strings.ToLower(strings.TrimSpace(strings.Trim(tokens[offset].Value, "`[]")))
				rates[partName] = tokens[offset+1].Int
				offset++
			}
		}
		break
	}
	return rates
}

func currentAvatarCompoundTokenInts(tokens []dnfpvf.Token) []int64 {
	out := make([]int64, 0, len(tokens))
	for _, token := range tokens {
		if token.Kind == dnfpvf.TokenInt {
			out = append(out, token.Int)
		}
	}
	return out
}

func parseCurrentAvatarCompoundPartPool(values []int64) (currentAvatarCompoundPartPool, error) {
	if len(values) < 3 || values[0] <= 0 {
		return currentAvatarCompoundPartPool{}, errCurrentAvatarCompoundOutputInvalid
	}
	rareCount := int(values[0])
	if rareCount > (len(values)-1)/2 {
		return currentAvatarCompoundPartPool{}, errCurrentAvatarCompoundOutputInvalid
	}
	pool := currentAvatarCompoundPartPool{}
	for offset, pairIndex := 1, 0; offset+1 < len(values); offset, pairIndex = offset+2, pairIndex+1 {
		itemID, weight := values[offset], values[offset+1]
		if itemID <= 0 || itemID > math.MaxUint32 || weight <= 0 || weight > math.MaxUint32 {
			return currentAvatarCompoundPartPool{}, errCurrentAvatarCompoundOutputInvalid
		}
		item := currentAvatarCompoundWeightedItem{ItemID: uint32(itemID), Weight: uint32(weight)}
		if pairIndex < rareCount {
			pool.Rare = append(pool.Rare, item)
		} else {
			pool.Upper = append(pool.Upper, item)
		}
	}
	if len(pool.Rare) != rareCount || len(pool.Upper) == 0 {
		return currentAvatarCompoundPartPool{}, errCurrentAvatarCompoundOutputInvalid
	}
	return pool, nil
}

func currentAvatarCompoundRequestedPart(config currentAvatarCompoundConfig, itemID uint32) (string, bool) {
	for _, partName := range currentAvatarCompoundPartNames {
		pool := config.Parts[partName]
		for _, candidate := range append(append([]currentAvatarCompoundWeightedItem(nil), pool.Rare...), pool.Upper...) {
			if candidate.ItemID == itemID {
				return partName, true
			}
		}
	}
	return "", false
}

func currentAvatarCompoundDocumentPart(document *dnfpvf.Document) (string, bool) {
	if document == nil {
		return "", false
	}
	equipmentType, found := document.Text("equipment type")
	if !found {
		return "", false
	}
	switch strings.ToLower(strings.TrimSpace(strings.Trim(equipmentType, "`[]"))) {
	case "hat avatar":
		return "hat", true
	case "hair avatar":
		return "hair", true
	case "face avatar":
		return "face", true
	case "breast avatar", "neck avatar":
		return "neck", true
	case "coat avatar":
		return "coat", true
	case "pants avatar":
		return "pants", true
	case "waist avatar", "belt avatar":
		return "belt", true
	case "shoes avatar":
		return "shoes", true
	default:
		return "", false
	}
}

func currentAvatarCompoundRareRate(config currentAvatarCompoundConfig, part string, grade1, grade2 int64, materialID uint32) (int64, error) {
	if (grade1 != config.Grade && grade1 != config.UpperGrade) || (grade2 != config.Grade && grade2 != config.UpperGrade) {
		return 0, errCurrentAvatarCompoundSourceInvalid
	}
	rareRate, rareFound := config.RareRates[part]
	upperRate, upperFound := config.UpperRareRates[part]
	bonus, materialFound := config.MaterialBonuses[materialID]
	if !rareFound || !upperFound || !materialFound || rareRate < 0 || upperRate < 0 || bonus < 0 {
		return 0, errCurrentAvatarCompoundUnavailable
	}
	base1, base2 := grade1 == config.Grade, grade2 == config.Grade
	rate := rareRate
	if !base1 && !base2 {
		rate += upperRate
	} else if base1 != base2 {
		rate += upperRate / 2
	}
	rate += bonus
	if rate > 10000 {
		rate = 10000
	}
	return rate, nil
}

func rollCurrentAvatarCompoundRare(pool []currentAvatarCompoundWeightedItem) (uint32, error) {
	var total uint64
	for _, candidate := range pool {
		total += uint64(candidate.Weight)
	}
	if total == 0 {
		return 0, errCurrentAvatarCompoundOutputInvalid
	}
	roll, err := currentDisjointRandomUint64(total)
	if err != nil {
		return 0, err
	}
	for _, candidate := range pool {
		if roll < uint64(candidate.Weight) {
			return candidate.ItemID, nil
		}
		roll -= uint64(candidate.Weight)
	}
	return 0, errCurrentAvatarCompoundOutputInvalid
}

func (c currentAvatarCompoundConfig) String() string {
	return c.PVFPath + ":" + strconv.FormatUint(uint64(c.NormalMaterialID), 10)
}
