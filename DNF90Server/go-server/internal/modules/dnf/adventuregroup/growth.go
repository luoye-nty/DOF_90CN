package adventuregroup

import (
	"encoding/json"
	"fmt"
	"math"
	"math/bits"
	"strconv"
	"strings"
	"time"

	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

// GrowthMetadataKey is the account-owned adventure-group growth ledger. It is
// separate from the runtime state (shop points / capsule) because its level is
// what the client renders as the adventure-group level.
const GrowthMetadataKey = "adventure_group_growth_v1"

const growthRateScale = uint64(1_000_000)

// GrowthConfig is the PVF `[max adventure level]` / `[adventure exp rate]` /
// `[adventure exp table]` / per-level `[stat]` benefits.
type GrowthConfig struct {
	MaximumLevel int
	RateParts    uint64
	// Entry N is the experience needed to enter level N, not a cumulative total.
	RequiredExperience []uint64
	LevelBenefits      []Benefits
}

type growthState struct {
	Level      int    `json:"level"`
	Experience uint64 `json:"experience"`
	Remainder  uint64 `json:"remainder"`
}

func parseGrowthConfig(doc *dnfpvf.Document) (GrowthConfig, error) {
	c := GrowthConfig{MaximumLevel: int(firstSectionInt(doc, "max adventure level"))}
	if c.MaximumLevel < 1 || c.MaximumLevel > 255 {
		return c, fmt.Errorf("%w: adventure maximum level", ErrTableMalformed)
	}
	tokens, _ := doc.Section("adventure exp rate")
	if len(tokens) != 1 || tokenNumber(tokens[0]) <= 0 || tokenNumber(tokens[0]) > 1 {
		return c, fmt.Errorf("%w: adventure experience rate", ErrTableMalformed)
	}
	c.RateParts = uint64(math.Round(tokenNumber(tokens[0]) * float64(growthRateScale)))
	c.RequiredExperience = make([]uint64, c.MaximumLevel+1)
	c.LevelBenefits = make([]Benefits, c.MaximumLevel+1)
	tokens, _ = doc.Section("adventure exp table")
	seen := make(map[int]bool)
	if len(tokens)%2 != 0 {
		return c, fmt.Errorf("%w: adventure experience rows", ErrTableMalformed)
	}
	for i := 0; i < len(tokens); i += 2 {
		level := int(tokens[i].Int)
		value, err := strconv.ParseUint(tokens[i+1].Value, 10, 64)
		if tokens[i].Kind != dnfpvf.TokenInt || tokens[i+1].Kind != dnfpvf.TokenString || err != nil || level < 1 || seen[level] {
			return c, fmt.Errorf("%w: adventure experience row %d", ErrTableMalformed, i/2)
		}
		seen[level] = true
		if level <= c.MaximumLevel {
			c.RequiredExperience[level] = value
		}
	}
	for level := 1; level <= c.MaximumLevel; level++ {
		if !seen[level] || (level > 1 && c.RequiredExperience[level] == 0) {
			return c, fmt.Errorf("%w: adventure experience level %d", ErrTableMalformed, level)
		}
	}
	level := 0
	for _, section := range doc.Sections {
		values := sectionTokens(doc, section)
		switch normalizedPVFName(section.Name) {
		case "attribute":
			level = int(firstInt(values))
		case "/attribute":
			level = 0
		case "stat", "grow":
			if level < 1 || level > c.MaximumLevel || len(values) < 3 {
				continue
			}
			value := tokenNumber(values[2])
			if value < 0 || value > math.MaxUint32 {
				return c, fmt.Errorf("%w: adventure benefit level %d", ErrTableMalformed, level)
			}
			switch values[1].Value {
			case "str":
				c.LevelBenefits[level].ManageOption = uint64(value)
			case "clear result exp up":
				c.LevelBenefits[level].ExpBonusPercent = uint64(value)
			}
		}
	}
	return c, nil
}

// CurrentSummary returns the account's adventure-group level. Once the first
// earned experience is committed the growth ledger owns it; before that the
// legacy roster-derived level is migrated into the ledger so a roster change
// cannot lower an established account.
func (t *Tables) CurrentSummary(account dnfrepo.AccountRecord, characters []Character) (Summary, error) {
	legacy, err := t.Calculate(characters)
	if err != nil || t.runtime.Growth.MaximumLevel == 0 {
		return legacy, err
	}
	c := t.runtime.Growth
	initial := growthState{Level: max(1, min(legacy.ManageLevel, c.MaximumLevel))}
	if legacy.ManageLevel > 0 && legacy.ManageLevel < len(t.manageThresholds) && initial.Level < c.MaximumLevel {
		lower, upper := t.manageThresholds[legacy.ManageLevel-1], t.manageThresholds[legacy.ManageLevel]
		if upper > lower {
			hi, lo := bits.Mul64(legacy.TotalPoint-lower, c.RequiredExperience[initial.Level+1])
			initial.Experience, _ = bits.Div64(hi, lo, upper-lower)
		}
	}
	state, err := readGrowthState(account, c, Summary{ManageLevel: initial.Level, TotalPoint: initial.Experience})
	if err != nil {
		return Summary{}, err
	}
	return Summary{ManageLevel: state.Level, TotalPoint: state.Experience, Benefits: c.LevelBenefits[state.Level]}, nil
}

func readGrowthState(account dnfrepo.AccountRecord, c GrowthConfig, initial Summary) (growthState, error) {
	state := growthState{Level: max(1, initial.ManageLevel), Experience: initial.TotalPoint}
	if value := strings.TrimSpace(account.Metadata[GrowthMetadataKey]); value != "" {
		if err := json.Unmarshal([]byte(value), &state); err != nil {
			return state, fmt.Errorf("%w: growth: %v", ErrRuntimeStateInvalid, err)
		}
	}
	if state.Level < 1 || state.Level > c.MaximumLevel || state.Remainder >= growthRateScale ||
		(state.Level < c.MaximumLevel && state.Experience >= c.RequiredExperience[state.Level+1]) {
		return state, fmt.Errorf("%w: growth level/progress", ErrRuntimeStateInvalid)
	}
	return state, nil
}

// SeedGrowthState migrates the roster-derived legacy level into the growth
// ledger the first time an account is read with both the account record and
// its roster in hand. Until the ledger exists, CurrentSummary can only show the
// migrated value without persisting it; seeding at login means a later
// experience grant can never start the ledger below an established level.
func SeedGrowthState(account *dnfrepo.AccountRecord, c GrowthConfig, legacy Summary, now time.Time) (bool, error) {
	if account == nil || c.MaximumLevel == 0 {
		return false, nil
	}
	if strings.TrimSpace(account.Metadata[GrowthMetadataKey]) != "" {
		return false, nil
	}
	if account.Metadata == nil {
		return false, fmt.Errorf("%w: account metadata not loaded", ErrRuntimeStateInvalid)
	}
	state := growthState{Level: max(1, min(legacy.ManageLevel, c.MaximumLevel)), Experience: legacy.TotalPoint}
	if state.Level < c.MaximumLevel && state.Experience >= c.RequiredExperience[state.Level+1] {
		state.Experience = 0
	}
	if state.Level > 1 && state.Level < len(c.RequiredExperience) && c.RequiredExperience[state.Level] > 0 {
		// A level carried over from the roster starts its bar from zero; the
		// roster's total point is a different scale and must not be reused.
		state.Experience = 0
	}
	encoded, err := json.Marshal(state)
	if err != nil {
		return false, err
	}
	account.Metadata[GrowthMetadataKey] = string(encoded)
	account.UpdatedAt = now
	return true, nil
}

// ApplyGrowthExperience records one earned-experience grant on an account the
// caller has already loaded and is about to save itself. It mirrors the
// ApplyExperienceCredit contract (no repository access), which is what keeps a
// single settlement from double-counting the same experience.
//
// gain is the character experience that was actually awarded. RateParts turns
// it into adventure-group experience; the remainder is carried so many small
// grants still add up to the same total as one large one.
func ApplyGrowthExperience(
	account *dnfrepo.AccountRecord,
	config GrowthConfig,
	initial Summary,
	gain uint32,
	now time.Time,
) (growthState, error) {
	c := config
	if account == nil {
		return growthState{}, ErrOwnerUnavailable
	}
	if c.MaximumLevel == 0 || gain == 0 {
		return growthState{}, nil
	}
	if account.Metadata == nil {
		return growthState{}, fmt.Errorf("%w: account metadata not loaded", ErrRuntimeStateInvalid)
	}
	state, err := readGrowthState(*account, c, initial)
	if err != nil {
		return growthState{}, err
	}
	scaled := uint64(gain)*c.RateParts + state.Remainder
	remaining := scaled / growthRateScale
	state.Remainder = scaled % growthRateScale
	for state.Level < c.MaximumLevel && remaining >= c.RequiredExperience[state.Level+1]-state.Experience {
		remaining -= c.RequiredExperience[state.Level+1] - state.Experience
		state.Level++
		state.Experience = 0
	}
	if state.Level == c.MaximumLevel {
		state.Experience, state.Remainder = 0, 0
	} else {
		state.Experience += remaining
	}
	encoded, err := json.Marshal(state)
	if err != nil {
		return state, err
	}
	account.Metadata[GrowthMetadataKey] = string(encoded)
	account.UpdatedAt = now
	return state, nil
}
