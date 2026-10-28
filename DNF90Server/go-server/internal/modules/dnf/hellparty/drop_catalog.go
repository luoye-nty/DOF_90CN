package hellparty

import (
	"errors"
	"fmt"
	"math"

	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
)

const (
	DropConfigPath        = "etc/itemdropinfo_monster_hell.etc"
	RarityRollDenominator = uint32(1_000_000)
)

var ErrDropCatalogInvalid = errors.New("dnf hell party drop catalog is invalid")

type DropProbabilityRow struct {
	MinimumLevel   int
	MaximumLevel   int
	DifficultyRate [5]uint32
}

type GradeWindow struct {
	Down int64
	Up   int64
}

type DropCatalog struct {
	probabilities []DropProbabilityRow
	rarity        [][7]uint32
	gradeWindows  map[int]GradeWindow
}

func LoadDropCatalog(source TextSource) (*DropCatalog, error) {
	if source == nil {
		return nil, ErrSourceRequired
	}
	text, err := source.ReadText(DropConfigPath)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", DropConfigPath, err)
	}
	return ParseDropCatalog(text)
}

func ParseDropCatalog(text string) (*DropCatalog, error) {
	document, err := dnfpvf.Parse(DropConfigPath, text)
	if err != nil {
		return nil, fmt.Errorf("%w: parse %s: %v", ErrDropCatalogInvalid, DropConfigPath, err)
	}
	countValues, ok := uniqueSection(document, "drop prob count")
	if !ok {
		return nil, fmt.Errorf("%w: missing or duplicate [drop prob count]", ErrDropCatalogInvalid)
	}
	countInts, err := strictSectionInts(countValues, "drop prob count")
	if err != nil || len(countInts) != 1 || countInts[0] <= 0 || countInts[0] > 200 {
		return nil, fmt.Errorf("%w: [drop prob count] values=%v", ErrDropCatalogInvalid, countInts)
	}
	rowCount := int(countInts[0])

	probabilityTokens, ok := uniqueSection(document, "dungeon difficulty drop prob")
	if !ok {
		return nil, fmt.Errorf("%w: missing or duplicate [dungeon difficulty drop prob]", ErrDropCatalogInvalid)
	}
	probabilityInts, err := strictSectionInts(probabilityTokens, "dungeon difficulty drop prob")
	if err != nil || len(probabilityInts) != rowCount*7 {
		return nil, fmt.Errorf("%w: [dungeon difficulty drop prob] values=%d want=%d", ErrDropCatalogInvalid, len(probabilityInts), rowCount*7)
	}
	probabilities := make([]DropProbabilityRow, 0, rowCount)
	previousMaximum := 0
	for rowIndex := 0; rowIndex < rowCount; rowIndex++ {
		base := rowIndex * 7
		minimum, maximum := probabilityInts[base], probabilityInts[base+1]
		if minimum <= 0 || maximum < minimum || maximum > 200 || int(minimum) <= previousMaximum {
			return nil, fmt.Errorf("%w: probability row=%d level=%d-%d previous_max=%d", ErrDropCatalogInvalid, rowIndex, minimum, maximum, previousMaximum)
		}
		row := DropProbabilityRow{MinimumLevel: int(minimum), MaximumLevel: int(maximum)}
		for difficulty := range row.DifficultyRate {
			value := probabilityInts[base+2+difficulty]
			if value < 0 || value > math.MaxUint32 {
				return nil, fmt.Errorf("%w: probability row=%d difficulty=%d rate=%d", ErrDropCatalogInvalid, rowIndex, difficulty, value)
			}
			row.DifficultyRate[difficulty] = uint32(value)
		}
		probabilities = append(probabilities, row)
		previousMaximum = int(maximum)
	}

	rarityTokens, ok := uniqueSection(document, "basis of rarity dicision")
	if !ok {
		return nil, fmt.Errorf("%w: missing or duplicate [basis of rarity dicision]", ErrDropCatalogInvalid)
	}
	rarityInts, err := strictSectionInts(rarityTokens, "basis of rarity dicision")
	if err != nil || len(rarityInts) == 0 || rarityInts[0] <= 0 || rarityInts[0] > 32 {
		return nil, fmt.Errorf("%w: [basis of rarity dicision] values=%v", ErrDropCatalogInvalid, rarityInts)
	}
	rarityRows := int(rarityInts[0])
	if len(rarityInts) != 1+rarityRows*7 {
		return nil, fmt.Errorf("%w: rarity values=%d want=%d", ErrDropCatalogInvalid, len(rarityInts), 1+rarityRows*7)
	}
	rarity := make([][7]uint32, rarityRows)
	for rowIndex := range rarity {
		var previousEffective uint32
		for rarityIndex := range rarity[rowIndex] {
			value := rarityInts[1+rowIndex*7+rarityIndex]
			if value < 0 || value > math.MaxUint32 {
				return nil, fmt.Errorf("%w: rarity row=%d index=%d value=%d", ErrDropCatalogInvalid, rowIndex, rarityIndex, value)
			}
			effective := uint32(value)
			if effective > RarityRollDenominator {
				effective = RarityRollDenominator
			}
			if effective < previousEffective {
				return nil, fmt.Errorf("%w: rarity row=%d index=%d value=%d effective=%d previous_effective=%d", ErrDropCatalogInvalid, rowIndex, rarityIndex, value, effective, previousEffective)
			}
			rarity[rowIndex][rarityIndex] = uint32(value)
			previousEffective = effective
		}
	}

	gradeTokens, ok := uniqueSection(document, "item drop ref table")
	if !ok {
		return nil, fmt.Errorf("%w: missing or duplicate [item drop ref table]", ErrDropCatalogInvalid)
	}
	gradeInts, err := strictSectionInts(gradeTokens, "item drop ref table")
	if err != nil || len(gradeInts) == 0 || len(gradeInts)%3 != 0 {
		return nil, fmt.Errorf("%w: [item drop ref table] values=%d", ErrDropCatalogInvalid, len(gradeInts))
	}
	gradeWindows := make(map[int]GradeWindow, len(gradeInts)/3)
	for index := 0; index < len(gradeInts); index += 3 {
		level, down, up := gradeInts[index], gradeInts[index+1], gradeInts[index+2]
		if level <= 0 || level > 200 || down < 0 || up < 0 || (down == 0 && up == 0) {
			return nil, fmt.Errorf("%w: grade row=%d level=%d down=%d up=%d", ErrDropCatalogInvalid, index/3, level, down, up)
		}
		if _, exists := gradeWindows[int(level)]; exists {
			return nil, fmt.Errorf("%w: duplicate grade level=%d", ErrDropCatalogInvalid, level)
		}
		gradeWindows[int(level)] = GradeWindow{Down: down, Up: up}
	}

	return &DropCatalog{
		probabilities: probabilities,
		rarity:        rarity,
		gradeWindows:  gradeWindows,
	}, nil
}

func strictSectionInts(tokens []dnfpvf.Token, section string) ([]int64, error) {
	values := make([]int64, 0, len(tokens))
	for _, token := range tokens {
		if token.Kind == dnfpvf.TokenSection {
			break
		}
		if token.Kind != dnfpvf.TokenInt {
			return nil, fmt.Errorf("%w: [%s] token=%q is not integer", ErrDropCatalogInvalid, section, token.Raw)
		}
		values = append(values, token.Int)
	}
	return values, nil
}

func (catalog *DropCatalog) Snapshot() (probabilityRows, rarityRows, gradeRows int) {
	if catalog == nil {
		return 0, 0, 0
	}
	return len(catalog.probabilities), len(catalog.rarity), len(catalog.gradeWindows)
}

func (catalog *DropCatalog) DropRate(itemBasisLevel, dungeonDifficulty int) (uint32, bool) {
	if catalog == nil {
		return 0, false
	}
	difficulty := dungeonDifficulty
	if difficulty < 0 {
		difficulty = 0
	}
	if difficulty > 4 {
		difficulty = 4
	}
	for _, row := range catalog.probabilities {
		if itemBasisLevel >= row.MinimumLevel && itemBasisLevel <= row.MaximumLevel {
			return row.DifficultyRate[difficulty], true
		}
	}
	return 0, false
}

func (catalog *DropCatalog) GradeWindow(itemBasisLevel int) (GradeWindow, bool) {
	if catalog == nil {
		return GradeWindow{}, false
	}
	window, ok := catalog.gradeWindows[itemBasisLevel]
	return window, ok
}

func (catalog *DropCatalog) RarityThresholds(mode int) ([7]uint32, bool) {
	if catalog == nil || len(catalog.rarity) == 0 {
		return [7]uint32{}, false
	}
	index := mode - 1
	if index < 0 {
		index = 0
	}
	if index >= len(catalog.rarity) {
		index = len(catalog.rarity) - 1
	}
	return catalog.rarity[index], true
}

func (catalog *DropCatalog) RarityWeights(mode int) ([7]uint32, bool) {
	thresholds, ok := catalog.RarityThresholds(mode)
	if !ok {
		return [7]uint32{}, false
	}
	var weights [7]uint32
	var previous uint32
	for index, threshold := range thresholds {
		if threshold > RarityRollDenominator {
			threshold = RarityRollDenominator
		}
		if threshold < previous {
			threshold = previous
		}
		weights[index] = threshold - previous
		previous = threshold
	}
	return weights, true
}

func (catalog *DropCatalog) RarityForRoll(mode int, roll uint32) (int64, bool) {
	if roll >= RarityRollDenominator {
		return 0, false
	}
	thresholds, ok := catalog.RarityThresholds(mode)
	if !ok {
		return 0, false
	}
	value := roll + 1
	for rarity, threshold := range thresholds {
		if threshold > RarityRollDenominator {
			threshold = RarityRollDenominator
		}
		if value <= threshold {
			return int64(rarity), true
		}
	}
	return 0, false
}
