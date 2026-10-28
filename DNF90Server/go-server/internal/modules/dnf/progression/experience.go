package progression

import (
	"context"
	"fmt"
	"math"

	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
)

// ExperienceTable is the immutable cumulative level-threshold projection from
// the runtime PVF. Keeping the level-entry lookup in the progression domain
// prevents administrative writes from treating cumulative EXP as per-level
// EXP.
type ExperienceTable struct {
	thresholds []uint32
}

// LoadExperienceTable reads only character/ExpTable.tbl. Callers that need no
// SP, TP, or quest data should not make those unrelated PVF files prerequisites.
func LoadExperienceTable(ctx context.Context, source dnfpvf.Source) (*ExperienceTable, error) {
	if source == nil {
		return nil, dnfpvf.ErrSourceRequired
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	doc, err := readDocument(source, ExperienceTablePath)
	if err != nil {
		return nil, err
	}
	thresholds, err := parseExperienceThresholds(doc)
	if err != nil {
		return nil, err
	}
	return &ExperienceTable{thresholds: thresholds}, nil
}

// EntryExperience returns the cumulative EXP at the beginning of level. Level
// one begins at zero; every later level begins at the previous PVF threshold.
func (t *ExperienceTable) EntryExperience(level int) (uint32, error) {
	if t == nil || level <= 0 || level-1 > len(t.thresholds) {
		return 0, fmt.Errorf("%w: entry level=%d thresholds=%d", ErrLevelOutOfRange, level, experienceThresholdCount(t))
	}
	if level == 1 {
		return 0, nil
	}
	return t.thresholds[level-2], nil
}

func experienceThresholdCount(t *ExperienceTable) int {
	if t == nil {
		return 0
	}
	return len(t.thresholds)
}

type ExperienceResult struct {
	PreviousLevel      int
	PreviousExperience uint32
	Gain               uint32
	NewLevel           int
	NewExperience      uint32
	LevelsGained       int
	Saturated          bool
}

// ThresholdToNext returns the cumulative experience required to advance from
// level to level+1. The caller supplies the server's proven level cap when
// applying experience; the threshold-file length is not guessed to be that cap.
func (t *Tables) ThresholdToNext(level int) (uint32, error) {
	if t == nil || level <= 0 || level > len(t.experienceThresholds) {
		return 0, fmt.Errorf("%w: threshold level=%d entries=%d", ErrLevelOutOfRange, level, thresholdCount(t))
	}
	return t.experienceThresholds[level-1], nil
}

// ApplyExperience saturating-adds a current-EXE u32 experience gain and applies
// cumulative PVF thresholds up to maxLevel. It does not persist or emit an op37.
func (t *Tables) ApplyExperience(level int, totalExperience, gain uint32, maxLevel int) (ExperienceResult, error) {
	result := ExperienceResult{
		PreviousLevel:      level,
		PreviousExperience: totalExperience,
		Gain:               gain,
		NewLevel:           level,
		NewExperience:      totalExperience,
	}
	if t == nil || level <= 0 || maxLevel <= 0 || maxLevel > MaximumCharacterLevel ||
		level > maxLevel || maxLevel-1 > len(t.experienceThresholds) {
		return ExperienceResult{}, fmt.Errorf("%w: level=%d max=%d thresholds=%d", ErrLevelOutOfRange, level, maxLevel, thresholdCount(t))
	}
	sum := uint64(totalExperience) + uint64(gain)
	if sum > math.MaxUint32 {
		sum = math.MaxUint32
		result.Saturated = true
	}
	result.NewExperience = uint32(sum)
	for result.NewLevel < maxLevel {
		threshold := t.experienceThresholds[result.NewLevel-1]
		if result.NewExperience < threshold {
			break
		}
		result.NewLevel++
	}
	// Ordinary EXP stops at the cumulative threshold that enters max level;
	// later accepted EXP is owned by HonorExpert.
	if result.NewLevel == maxLevel && maxLevel > 1 {
		entryThreshold := t.experienceThresholds[maxLevel-2]
		if result.NewExperience > entryThreshold {
			result.NewExperience = entryThreshold
			result.Saturated = true
		}
	}
	result.LevelsGained = result.NewLevel - result.PreviousLevel
	return result, nil
}

func thresholdCount(t *Tables) int {
	if t == nil {
		return 0
	}
	return len(t.experienceThresholds)
}
