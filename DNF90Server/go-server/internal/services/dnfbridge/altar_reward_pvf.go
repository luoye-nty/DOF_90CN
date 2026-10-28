package dnfbridge

import (
	"errors"
	"fmt"
	"math"

	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
	"longheng.io/server/internal/modules/dnf/worldmap"
)

const (
	currentDungeonAltarRewardDefinitionPath = "etc/bloodclearreward.etc"
	currentDungeonAltarMaximumRewardCards   = 10
)

var errCurrentDungeonAltarRewardDefinitionInvalid = errors.New("current dungeon altar reward PVF definition is invalid")

type currentDungeonAltarRewardCandidate uint8

const (
	currentDungeonAltarRewardCandidateEmpty currentDungeonAltarRewardCandidate = iota
	currentDungeonAltarRewardCandidateGold
	currentDungeonAltarRewardCandidateItem
)

type currentDungeonAltarUltimateRewardProbability struct {
	Point               int64
	Item1251Probability int64
	Item1252Probability int64
}

// currentDungeonAltarRewardDefinition is a lossless typed view of
// etc/bloodclearreward.etc. The experience vectors are consumed by the altar
// settlement formula recovered from the reference server; their result is
// projected through the current EXE's verified op208 reward-EXP field.
type currentDungeonAltarRewardDefinition struct {
	GoldCandidateWeight        int64
	ItemCandidateWeight        int64
	RewardWeightPerProgress    int64
	GoldAmountWeight           float64
	RewardExperienceWeights    []float64
	EntryCountExperienceWeight []float64
	UltimateDifficultyPoints   [2]int64
	UltimateRewards            map[int64]currentDungeonAltarUltimateRewardProbability
}

func loadCurrentDungeonAltarRewardDefinition(source dnfpvf.Source) (currentDungeonAltarRewardDefinition, error) {
	document, err := parseDungeonCardPVFDocument(source, currentDungeonAltarRewardDefinitionPath)
	if err != nil {
		return currentDungeonAltarRewardDefinition{}, err
	}
	return parseCurrentDungeonAltarRewardDefinition(document)
}

func parseCurrentDungeonAltarRewardDefinition(document *dnfpvf.Document) (currentDungeonAltarRewardDefinition, error) {
	probabilities, err := currentDungeonAltarPVFInts(document, "reward item prob")
	if err != nil || len(probabilities) != 3 {
		return currentDungeonAltarRewardDefinition{}, currentDungeonAltarRewardShapeError("reward item prob", len(probabilities), 3, err)
	}
	goldWeights, err := currentDungeonAltarPVFNumbers(document, "reward gold weight")
	if err != nil || len(goldWeights) != 1 {
		return currentDungeonAltarRewardDefinition{}, currentDungeonAltarRewardShapeError("reward gold weight", len(goldWeights), 1, err)
	}
	rewardExperienceWeights, err := currentDungeonAltarPVFNumbers(document, "reward exp weight")
	if err != nil || len(rewardExperienceWeights) == 0 {
		return currentDungeonAltarRewardDefinition{}, currentDungeonAltarRewardShapeError("reward exp weight", len(rewardExperienceWeights), -1, err)
	}
	entryCountWeights, err := currentDungeonAltarPVFNumbers(document, "reward exp inout count weight")
	if err != nil || len(entryCountWeights) == 0 {
		return currentDungeonAltarRewardDefinition{}, currentDungeonAltarRewardShapeError("reward exp inout count weight", len(entryCountWeights), -1, err)
	}
	difficultyPoints, err := currentDungeonAltarPVFInts(document, "ultimate difficulty point")
	if err != nil || len(difficultyPoints) != 2 {
		return currentDungeonAltarRewardDefinition{}, currentDungeonAltarRewardShapeError("ultimate difficulty point", len(difficultyPoints), 2, err)
	}
	ultimateValues, err := currentDungeonAltarPVFInts(document, "ultimate reward prob")
	if err != nil || len(ultimateValues) == 0 || len(ultimateValues)%3 != 0 {
		return currentDungeonAltarRewardDefinition{}, currentDungeonAltarRewardShapeError("ultimate reward prob", len(ultimateValues), 3, err)
	}

	definition := currentDungeonAltarRewardDefinition{
		GoldCandidateWeight:        probabilities[0],
		ItemCandidateWeight:        probabilities[1],
		RewardWeightPerProgress:    probabilities[2],
		GoldAmountWeight:           goldWeights[0],
		RewardExperienceWeights:    append([]float64(nil), rewardExperienceWeights...),
		EntryCountExperienceWeight: append([]float64(nil), entryCountWeights...),
		UltimateDifficultyPoints:   [2]int64{difficultyPoints[0], difficultyPoints[1]},
		UltimateRewards:            make(map[int64]currentDungeonAltarUltimateRewardProbability, len(ultimateValues)/3),
	}
	if definition.GoldCandidateWeight < 0 || definition.ItemCandidateWeight < 0 ||
		definition.RewardWeightPerProgress <= 0 ||
		definition.GoldCandidateWeight+definition.ItemCandidateWeight > definition.RewardWeightPerProgress ||
		definition.GoldAmountWeight <= 0 || math.IsNaN(definition.GoldAmountWeight) || math.IsInf(definition.GoldAmountWeight, 0) ||
		definition.UltimateDifficultyPoints[0] < 0 || definition.UltimateDifficultyPoints[1] < 0 ||
		!currentDungeonAltarFiniteNonnegativeWeights(definition.RewardExperienceWeights) ||
		!currentDungeonAltarFiniteNonnegativeWeights(definition.EntryCountExperienceWeight) {
		return currentDungeonAltarRewardDefinition{}, fmt.Errorf("%w: invalid scalar or weight vector", errCurrentDungeonAltarRewardDefinitionInvalid)
	}
	if definition.RewardWeightPerProgress > math.MaxInt/currentDungeonAltarMaximumRewardCards {
		return currentDungeonAltarRewardDefinition{}, fmt.Errorf("%w: reward roll scale overflows int", errCurrentDungeonAltarRewardDefinitionInvalid)
	}
	for offset := 0; offset < len(ultimateValues); offset += 3 {
		row := currentDungeonAltarUltimateRewardProbability{
			Point:               ultimateValues[offset],
			Item1251Probability: ultimateValues[offset+1],
			Item1252Probability: ultimateValues[offset+2],
		}
		if row.Point <= 0 || row.Item1251Probability < 0 || row.Item1252Probability < 0 ||
			row.Item1251Probability+row.Item1252Probability > 100 {
			return currentDungeonAltarRewardDefinition{}, fmt.Errorf("%w: ultimate reward row=%+v", errCurrentDungeonAltarRewardDefinitionInvalid, row)
		}
		if _, duplicate := definition.UltimateRewards[row.Point]; duplicate {
			return currentDungeonAltarRewardDefinition{}, fmt.Errorf("%w: duplicate ultimate reward point=%d", errCurrentDungeonAltarRewardDefinitionInvalid, row.Point)
		}
		definition.UltimateRewards[row.Point] = row
	}
	return definition, nil
}

func (definition currentDungeonAltarRewardDefinition) rewardCardCount(completedRounds, maximumRounds int) int {
	if maximumRounds <= 0 {
		return 0
	}
	count := currentDungeonAltarMaximumRewardCards * maxInt(0, completedRounds) / maximumRounds
	return min(currentDungeonAltarMaximumRewardCards, maxInt(1, count))
}

func (definition currentDungeonAltarRewardDefinition) rewardRollScale() int {
	if definition.RewardWeightPerProgress <= 0 || definition.RewardWeightPerProgress > math.MaxInt/currentDungeonAltarMaximumRewardCards {
		return 0
	}
	return int(definition.RewardWeightPerProgress) * currentDungeonAltarMaximumRewardCards
}

func (definition currentDungeonAltarRewardDefinition) classifyReward(completedRounds, roll int) (currentDungeonAltarRewardCandidate, error) {
	scale := definition.rewardRollScale()
	if scale <= 0 || roll < 0 || roll >= scale {
		return currentDungeonAltarRewardCandidateEmpty, fmt.Errorf("%w: completed=%d roll=%d scale=%d", errCurrentDungeonAltarRewardDefinitionInvalid, completedRounds, roll, scale)
	}
	progress := min(currentDungeonAltarMaximumRewardCards, maxInt(0, completedRounds))
	if int64(roll) < int64(progress)*definition.GoldCandidateWeight {
		return currentDungeonAltarRewardCandidateGold, nil
	}
	if int64(roll) < int64(progress)*(definition.GoldCandidateWeight+definition.ItemCandidateWeight) {
		return currentDungeonAltarRewardCandidateItem, nil
	}
	return currentDungeonAltarRewardCandidateEmpty, nil
}

func (definition currentDungeonAltarRewardDefinition) scaleGold(base int64) int64 {
	if base <= 0 || definition.GoldAmountWeight <= 0 || math.IsNaN(definition.GoldAmountWeight) || math.IsInf(definition.GoldAmountWeight, 0) {
		return 0
	}
	value := math.Floor(float64(base) * definition.GoldAmountWeight)
	if value >= math.MaxInt64 {
		return math.MaxInt64
	}
	if value <= 0 {
		return 0
	}
	return int64(value)
}

func (definition currentDungeonAltarRewardDefinition) ultimatePoint(difficulties []worldmap.BloodAltarDifficulty) int64 {
	var total int64
	for _, difficulty := range difficulties {
		var value int64
		switch difficulty {
		case worldmap.BloodAltarDifficultyA:
			value = definition.UltimateDifficultyPoints[0]
		case worldmap.BloodAltarDifficultyB:
			value = definition.UltimateDifficultyPoints[1]
		default:
			continue
		}
		if total > math.MaxInt64-value {
			return math.MaxInt64
		}
		total += value
	}
	return total
}

func (definition currentDungeonAltarRewardDefinition) ultimateRewardItem(point int64, roll int) (uint32, bool) {
	if roll < 0 || roll >= 100 {
		return 0, false
	}
	row, found := definition.UltimateRewards[point]
	if !found {
		return 0, false
	}
	if row.Item1252Probability > 0 && int64(roll) < row.Item1252Probability {
		return 1252, true
	}
	threshold := row.Item1252Probability + row.Item1251Probability
	if row.Item1251Probability > 0 && int64(roll) < threshold {
		return 1251, true
	}
	return 0, false
}

func currentDungeonAltarPVFInts(document *dnfpvf.Document, section string) ([]int64, error) {
	tokens, found := document.Section(section)
	if !found || len(tokens) == 0 {
		return nil, fmt.Errorf("%w: section=%s missing", errCurrentDungeonAltarRewardDefinitionInvalid, section)
	}
	values := make([]int64, 0, len(tokens))
	for _, token := range tokens {
		if token.Kind != dnfpvf.TokenInt {
			return nil, fmt.Errorf("%w: section=%s token=%s", errCurrentDungeonAltarRewardDefinitionInvalid, section, token.Raw)
		}
		values = append(values, token.Int)
	}
	return values, nil
}

func currentDungeonAltarPVFNumbers(document *dnfpvf.Document, section string) ([]float64, error) {
	tokens, found := document.Section(section)
	if !found || len(tokens) == 0 {
		return nil, fmt.Errorf("%w: section=%s missing", errCurrentDungeonAltarRewardDefinitionInvalid, section)
	}
	values := make([]float64, 0, len(tokens))
	for _, token := range tokens {
		switch token.Kind {
		case dnfpvf.TokenInt:
			values = append(values, float64(token.Int))
		case dnfpvf.TokenFloat:
			values = append(values, token.Float)
		default:
			return nil, fmt.Errorf("%w: section=%s token=%s", errCurrentDungeonAltarRewardDefinitionInvalid, section, token.Raw)
		}
	}
	return values, nil
}

func currentDungeonAltarFiniteNonnegativeWeights(values []float64) bool {
	if len(values) == 0 {
		return false
	}
	for _, value := range values {
		if value < 0 || math.IsNaN(value) || math.IsInf(value, 0) {
			return false
		}
	}
	return true
}

func currentDungeonAltarRewardShapeError(section string, actual, expected int, cause error) error {
	if cause != nil {
		return cause
	}
	if expected < 0 {
		return fmt.Errorf("%w: section=%s is empty", errCurrentDungeonAltarRewardDefinitionInvalid, section)
	}
	return fmt.Errorf("%w: section=%s values=%d want=%d", errCurrentDungeonAltarRewardDefinitionInvalid, section, actual, expected)
}
