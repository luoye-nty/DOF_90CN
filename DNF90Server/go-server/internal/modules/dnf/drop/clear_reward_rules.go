package drop

import (
	"errors"
	"fmt"
	"math"

	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
)

const ClearRewardRulePath = "Etc/ItemDropInfo_ClearReward.etc"

const (
	// ClearRewardItemRateDenominator is the probability scale used by the
	// ordinary dungeon-clear equipment roll in the current PVF.
	ClearRewardItemRateDenominator = uint32(10_000)
	clearRewardBonusDenominator    = float64(1_000)
)

var (
	ErrClearRewardRuleSourceRequired = errors.New("dnf clear-reward rule PVF source is required")
	ErrClearRewardRuleSectionMissing = errors.New("dnf clear-reward rule PVF section is missing")
	ErrClearRewardRuleSectionShape   = errors.New("dnf clear-reward rule PVF section shape is invalid")
	ErrClearRewardRuleLookupMissing  = errors.New("dnf clear-reward rule lookup is missing")
)

// ClearRewardItemReference preserves one exact three-column
// [item drop ref table] row. The two values remain opaque until the current
// client confirms their item-level interpretation.
type ClearRewardItemReference struct {
	LookupKey int64
	ValueA    int64
	ValueB    int64
}

// ClearRewardRuleSnapshot reports only loaded PVF source coverage.
type ClearRewardRuleSnapshot struct {
	MapRateRows         int
	DifficultyBonusRows int
	RarityThresholdRows int
	PartyBonusRows      int
	GoldDifficultyRows  int
	GoldCardCostRows    int
	ItemReferenceRows   int
}

// ClearRewardRawTables is a value-copy of fixed-size clear-reward source
// tables.
type ClearRewardRawTables struct {
	ItemRateByMapCount      map[int64]int64
	DifficultyDropBonus     [5]int64
	RarityDecision          [7]int64
	PartyMemberDropBonus    [4]float64
	GoldDifficultyDropBonus [5]float64
	GoldCardCostByLevel     map[int64]int64
}

// ClearRewardSourcePlan is a pure lookup. It neither rolls a card reward nor
// charges gold; callers can use it to correlate a real-client packet trace
// with the exact rows from the runtime PVF.
type ClearRewardSourcePlan struct {
	Level               int64
	DifficultyIndex     int
	PartyMembers        int
	VisitedRoomCount    int
	ItemRate            int64
	DifficultyBonus     int64
	PartyBonus          float64
	GoldDifficultyRate  float64
	GoldCardCost        int64
	ItemReference       ClearRewardItemReference
	ItemRateFound       bool
	DifficultyFound     bool
	PartyBonusFound     bool
	GoldDifficultyFound bool
	GoldCardCostFound   bool
	ItemReferenceFound  bool
}

// ClearRewardRule is the resolved ordinary end-of-dungeon card rule. The
// equipment rate and rarity thresholds are read from the active PVF; it does
// not select or grant an item.
type ClearRewardRule struct {
	EquipmentRate      uint32
	GradeDown          int64
	GradeUp            int64
	RarityThresholds   [7]uint32
	RarityScale        uint32
	PaidCardCost       uint32
	GoldDifficultyRate float64
	MapCount           int
}

// ClearRewardRules is a read-only typed view of
// Etc/ItemDropInfo_ClearReward.etc. The data can be safely inspected by
// card-reward and diagnostics code without owning card selection or rewards.
type ClearRewardRules struct {
	itemRateByMapCount      map[int64]int64
	difficultyDropBonus     [5]int64
	rarityDecision          [7]int64
	partyMemberDropBonus    [4]float64
	goldDifficultyDropBonus [5]float64
	goldCardCostByLevel     map[int64]int64
	itemReferences          map[int64]ClearRewardItemReference
}

func LoadClearRewardRules(source dnfpvf.Source) (*ClearRewardRules, error) {
	if source == nil {
		return nil, ErrClearRewardRuleSourceRequired
	}
	text, err := source.ReadText(ClearRewardRulePath)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", ClearRewardRulePath, err)
	}
	document, err := dnfpvf.Parse(ClearRewardRulePath, text)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", ClearRewardRulePath, err)
	}

	rules := &ClearRewardRules{
		itemRateByMapCount:  make(map[int64]int64, 5),
		goldCardCostByLevel: make(map[int64]int64),
		itemReferences:      make(map[int64]ClearRewardItemReference),
	}
	if err := loadClearRewardMapRates(document, rules); err != nil {
		return nil, err
	}
	if err := fillClearRewardIntArray(document, "dungeon difficulty drop bonusrate", rules.difficultyDropBonus[:]); err != nil {
		return nil, err
	}
	for index, value := range rules.difficultyDropBonus {
		if value < 0 {
			return nil, fmt.Errorf("%w: section=dungeon difficulty drop bonusrate row=%d value=%d", ErrClearRewardRuleSectionShape, index, value)
		}
	}
	if err := fillClearRewardIntArray(document, "basis of rarity dicision", rules.rarityDecision[:]); err != nil {
		return nil, err
	}
	for index, value := range rules.rarityDecision {
		if value < 0 || index > 0 && value < rules.rarityDecision[index-1] {
			return nil, fmt.Errorf("%w: section=basis of rarity dicision row=%d value=%d", ErrClearRewardRuleSectionShape, index, value)
		}
	}
	if rules.rarityDecision[len(rules.rarityDecision)-1] == 0 {
		return nil, fmt.Errorf("%w: section=basis of rarity dicision final_threshold=0", ErrClearRewardRuleSectionShape)
	}
	if err := fillClearRewardNumberArray(document, "party member drop bonusrate", rules.partyMemberDropBonus[:]); err != nil {
		return nil, err
	}
	if err := fillClearRewardNumberArray(document, "dungeon difficulty gold drop bonusrate", rules.goldDifficultyDropBonus[:]); err != nil {
		return nil, err
	}
	if err := loadClearRewardGoldCardCosts(document, rules); err != nil {
		return nil, err
	}
	if err := loadClearRewardItemReferences(document, rules); err != nil {
		return nil, err
	}
	return rules, nil
}

func loadClearRewardMapRates(document *dnfpvf.Document, rules *ClearRewardRules) error {
	values, err := clearRewardIntegers(document, "reward item rate per map max count")
	if err != nil {
		return err
	}
	if len(values) != 10 {
		return fmt.Errorf("%w: section=reward item rate per map max count values=%d row_width=2", ErrClearRewardRuleSectionShape, len(values))
	}
	for offset := 0; offset < len(values); offset += 2 {
		mapCount, rate := values[offset], values[offset+1]
		wantMapCount := int64(offset/2 + 1)
		if mapCount != wantMapCount || rate < 0 {
			return fmt.Errorf("%w: section=reward item rate per map max count row=%d count=%d rate=%d", ErrClearRewardRuleSectionShape, offset/2, mapCount, rate)
		}
		rules.itemRateByMapCount[mapCount] = rate
	}
	return nil
}

func loadClearRewardGoldCardCosts(document *dnfpvf.Document, rules *ClearRewardRules) error {
	values, err := clearRewardIntegers(document, "gold card cost table")
	if err != nil {
		return err
	}
	if len(values) == 0 || len(values)%2 != 0 {
		return fmt.Errorf("%w: section=gold card cost table values=%d row_width=2", ErrClearRewardRuleSectionShape, len(values))
	}
	var previous int64
	for offset := 0; offset < len(values); offset += 2 {
		level, cost := values[offset], values[offset+1]
		if level <= 0 || cost <= 0 || offset != 0 && level <= previous {
			return fmt.Errorf("%w: section=gold card cost table row=%d level=%d cost=%d", ErrClearRewardRuleSectionShape, offset/2, level, cost)
		}
		rules.goldCardCostByLevel[level] = cost
		previous = level
	}
	return nil
}

func loadClearRewardItemReferences(document *dnfpvf.Document, rules *ClearRewardRules) error {
	values, err := clearRewardIntegers(document, "item drop ref table")
	if err != nil {
		return err
	}
	if len(values) == 0 || len(values)%3 != 0 {
		return fmt.Errorf("%w: section=item drop ref table values=%d row_width=3", ErrClearRewardRuleSectionShape, len(values))
	}
	var previous int64
	for offset := 0; offset < len(values); offset += 3 {
		row := ClearRewardItemReference{LookupKey: values[offset], ValueA: values[offset+1], ValueB: values[offset+2]}
		if row.LookupKey <= 0 || row.ValueA < 0 || row.ValueB < 0 || offset != 0 && row.LookupKey <= previous {
			return fmt.Errorf("%w: section=item drop ref table row=%d value=%+v", ErrClearRewardRuleSectionShape, offset/3, row)
		}
		rules.itemReferences[row.LookupKey] = row
		previous = row.LookupKey
	}
	return nil
}

func clearRewardIntegers(document *dnfpvf.Document, section string) ([]int64, error) {
	tokens, found := document.Section(section)
	if !found {
		return nil, fmt.Errorf("%w: section=%s", ErrClearRewardRuleSectionMissing, section)
	}
	if len(tokens) == 0 {
		return nil, fmt.Errorf("%w: section=%s empty", ErrClearRewardRuleSectionShape, section)
	}
	values := make([]int64, 0, len(tokens))
	for index, token := range tokens {
		if token.Kind != dnfpvf.TokenInt {
			return nil, fmt.Errorf("%w: section=%s token=%d kind=%s", ErrClearRewardRuleSectionShape, section, index, token.Kind)
		}
		values = append(values, token.Int)
	}
	return values, nil
}

func clearRewardNumbers(document *dnfpvf.Document, section string) ([]float64, error) {
	tokens, found := document.Section(section)
	if !found {
		return nil, fmt.Errorf("%w: section=%s", ErrClearRewardRuleSectionMissing, section)
	}
	if len(tokens) == 0 {
		return nil, fmt.Errorf("%w: section=%s empty", ErrClearRewardRuleSectionShape, section)
	}
	values := make([]float64, 0, len(tokens))
	for index, token := range tokens {
		var value float64
		switch token.Kind {
		case dnfpvf.TokenInt:
			value = float64(token.Int)
		case dnfpvf.TokenFloat:
			value = token.Float
		default:
			return nil, fmt.Errorf("%w: section=%s token=%d kind=%s", ErrClearRewardRuleSectionShape, section, index, token.Kind)
		}
		if math.IsNaN(value) || math.IsInf(value, 0) || value <= 0 {
			return nil, fmt.Errorf("%w: section=%s token=%d value=%v", ErrClearRewardRuleSectionShape, section, index, value)
		}
		values = append(values, value)
	}
	return values, nil
}

func fillClearRewardIntArray(document *dnfpvf.Document, section string, destination []int64) error {
	values, err := clearRewardIntegers(document, section)
	if err != nil {
		return err
	}
	if len(values) != len(destination) {
		return fmt.Errorf("%w: section=%s values=%d want=%d", ErrClearRewardRuleSectionShape, section, len(values), len(destination))
	}
	copy(destination, values)
	return nil
}

func fillClearRewardNumberArray(document *dnfpvf.Document, section string, destination []float64) error {
	values, err := clearRewardNumbers(document, section)
	if err != nil {
		return err
	}
	if len(values) != len(destination) {
		return fmt.Errorf("%w: section=%s values=%d want=%d", ErrClearRewardRuleSectionShape, section, len(values), len(destination))
	}
	copy(destination, values)
	return nil
}

func (rules *ClearRewardRules) ItemReference(level int64) (ClearRewardItemReference, bool) {
	if rules == nil {
		return ClearRewardItemReference{}, false
	}
	reference, found := rules.itemReferences[level]
	return reference, found
}

// PartyMemberRate is the PVF [party member drop bonusrate] multiplier for a
// party size, clamped to the four rows the table defines. The clear-card gold
// formula applies it alongside the difficulty rate; a size outside the table
// contributes no bonus rather than zeroing the reward.
func (rules *ClearRewardRules) PartyMemberRate(partyMembers int) float64 {
	if rules == nil {
		return 1
	}
	if partyMembers < 1 {
		partyMembers = 1
	} else if partyMembers > len(rules.partyMemberDropBonus) {
		partyMembers = len(rules.partyMemberDropBonus)
	}
	rate := rules.partyMemberDropBonus[partyMembers-1]
	if rate <= 0 {
		return 1
	}
	return rate
}

func (rules *ClearRewardRules) PlanSource(level int64, difficultyIndex, partyMembers, visitedRoomCount int) ClearRewardSourcePlan {
	plan := ClearRewardSourcePlan{
		Level:            level,
		DifficultyIndex:  difficultyIndex,
		PartyMembers:     partyMembers,
		VisitedRoomCount: visitedRoomCount,
	}
	if rules == nil {
		return plan
	}
	plan.ItemRate, plan.ItemRateFound = rules.itemRateByMapCount[int64(visitedRoomCount)]
	if difficultyIndex >= 0 && difficultyIndex < len(rules.difficultyDropBonus) {
		plan.DifficultyBonus = rules.difficultyDropBonus[difficultyIndex]
		plan.DifficultyFound = true
		plan.GoldDifficultyRate = rules.goldDifficultyDropBonus[difficultyIndex]
		plan.GoldDifficultyFound = true
	}
	if partyMembers >= 1 && partyMembers <= len(rules.partyMemberDropBonus) {
		plan.PartyBonus = rules.partyMemberDropBonus[partyMembers-1]
		plan.PartyBonusFound = true
	}
	plan.GoldCardCost, plan.GoldCardCostFound = rules.goldCardCostByLevel[level]
	plan.ItemReference, plan.ItemReferenceFound = rules.ItemReference(level)
	return plan
}

// Resolve applies the formula used by the current server/client clear-card
// flow: room rate multiplied by the additive difficulty bonus (1/1000 scale)
// and the party-size multiplier, capped to the PVF's 1/10000 probability
// denominator. Normal difficulty has a zero additive bonus, not a zero
// multiplier.
func (rules *ClearRewardRules) Resolve(level int64, difficultyIndex, partyMembers, visitedRoomCount int) (ClearRewardRule, error) {
	if rules == nil || level <= 0 {
		return ClearRewardRule{}, ErrClearRewardRuleSourceRequired
	}
	if difficultyIndex < 0 || difficultyIndex >= len(rules.difficultyDropBonus) {
		difficultyIndex = 0
	}
	if partyMembers < 1 {
		partyMembers = 1
	} else if partyMembers > len(rules.partyMemberDropBonus) {
		partyMembers = len(rules.partyMemberDropBonus)
	}
	if visitedRoomCount < 1 {
		visitedRoomCount = 1
	} else if visitedRoomCount > len(rules.itemRateByMapCount) {
		visitedRoomCount = len(rules.itemRateByMapCount)
	}
	plan := rules.PlanSource(level, difficultyIndex, partyMembers, visitedRoomCount)
	if !plan.ItemRateFound || !plan.DifficultyFound || !plan.PartyBonusFound ||
		!plan.GoldDifficultyFound || !plan.GoldCardCostFound || !plan.ItemReferenceFound {
		return ClearRewardRule{}, fmt.Errorf(
			"%w: level=%d difficulty=%d party=%d rooms=%d coverage=%+v",
			ErrClearRewardRuleLookupMissing, level, difficultyIndex, partyMembers, visitedRoomCount, plan,
		)
	}
	if plan.ItemRate < 0 || plan.DifficultyBonus < 0 || plan.PartyBonus <= 0 ||
		math.IsNaN(plan.PartyBonus) || math.IsInf(plan.PartyBonus, 0) ||
		plan.GoldCardCost <= 0 || plan.GoldCardCost > math.MaxUint32 ||
		plan.ItemReference.ValueA < 0 || plan.ItemReference.ValueB < 0 {
		return ClearRewardRule{}, fmt.Errorf("%w: resolved source=%+v", ErrClearRewardRuleSectionShape, plan)
	}
	raw := rules.RawTables()
	var rarityThresholds [7]uint32
	for index, threshold := range raw.RarityDecision {
		if threshold < 0 || threshold > math.MaxUint32 {
			return ClearRewardRule{}, fmt.Errorf(
				"%w: section=basis of rarity dicision row=%d value=%d",
				ErrClearRewardRuleSectionShape, index, threshold,
			)
		}
		rarityThresholds[index] = uint32(threshold)
	}
	rarityScale := rarityThresholds[len(rarityThresholds)-1]
	if rarityScale == 0 {
		return ClearRewardRule{}, fmt.Errorf("%w: section=basis of rarity dicision final_threshold=0", ErrClearRewardRuleSectionShape)
	}
	rate := math.Floor(float64(plan.ItemRate) *
		(clearRewardBonusDenominator + float64(plan.DifficultyBonus)) /
		clearRewardBonusDenominator * plan.PartyBonus)
	if rate < 0 {
		rate = 0
	} else if rate > float64(ClearRewardItemRateDenominator) {
		rate = float64(ClearRewardItemRateDenominator)
	}
	return ClearRewardRule{
		EquipmentRate:      uint32(rate),
		GradeDown:          plan.ItemReference.ValueA,
		GradeUp:            plan.ItemReference.ValueB,
		RarityThresholds:   rarityThresholds,
		RarityScale:        rarityScale,
		PaidCardCost:       uint32(plan.GoldCardCost),
		GoldDifficultyRate: plan.GoldDifficultyRate,
		MapCount:           visitedRoomCount,
	}, nil
}

func (rules *ClearRewardRules) Snapshot() ClearRewardRuleSnapshot {
	if rules == nil {
		return ClearRewardRuleSnapshot{}
	}
	return ClearRewardRuleSnapshot{
		MapRateRows:         len(rules.itemRateByMapCount),
		DifficultyBonusRows: len(rules.difficultyDropBonus),
		RarityThresholdRows: len(rules.rarityDecision),
		PartyBonusRows:      len(rules.partyMemberDropBonus),
		GoldDifficultyRows:  len(rules.goldDifficultyDropBonus),
		GoldCardCostRows:    len(rules.goldCardCostByLevel),
		ItemReferenceRows:   len(rules.itemReferences),
	}
}

func (rules *ClearRewardRules) RawTables() ClearRewardRawTables {
	if rules == nil {
		return ClearRewardRawTables{}
	}
	return ClearRewardRawTables{
		ItemRateByMapCount:      cloneClearRewardIntMap(rules.itemRateByMapCount),
		DifficultyDropBonus:     rules.difficultyDropBonus,
		RarityDecision:          rules.rarityDecision,
		PartyMemberDropBonus:    rules.partyMemberDropBonus,
		GoldDifficultyDropBonus: rules.goldDifficultyDropBonus,
		GoldCardCostByLevel:     cloneClearRewardIntMap(rules.goldCardCostByLevel),
	}
}

func cloneClearRewardIntMap(source map[int64]int64) map[int64]int64 {
	if len(source) == 0 {
		return nil
	}
	clone := make(map[int64]int64, len(source))
	for key, value := range source {
		clone[key] = value
	}
	return clone
}
