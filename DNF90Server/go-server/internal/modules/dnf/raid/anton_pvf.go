package raid

import (
	"errors"
	"fmt"
	"math"
	"strings"

	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
)

const (
	// AntonPVFPath is the only archive member read by this catalog.
	AntonPVFPath = "etc/raid/anton.etc"

	antonOrdinaryPhaseCount  = 2
	antonMovieEndValueCount  = 4
	antonSingleRaidPhaseName = "single raid phase"
)

var (
	ErrAntonPVFSourceRequired   = errors.New("anton raid PVF source is required")
	ErrAntonPVFDocumentInvalid  = errors.New("anton raid PVF document is invalid")
	ErrAntonPVFSectionMissing   = errors.New("anton raid PVF section is missing")
	ErrAntonPVFSectionShape     = errors.New("anton raid PVF section shape is invalid")
	ErrAntonPVFValueInvalid     = errors.New("anton raid PVF value is invalid")
	ErrAntonPVFDuplicateDungeon = errors.New("anton raid PVF dungeon is duplicated")
)

// AntonDungeonState is one native [INIT STATE] value.
type AntonDungeonState string

const (
	AntonDungeonOpen   AntonDungeonState = "open"
	AntonDungeonHide   AntonDungeonState = "hide"
	AntonDungeonClosed AntonDungeonState = "close"

	// AntonDungeonCleared is the runtime-only "this gate has been finished"
	// state. etc/raid/anton.etc never spells it -- the parser below still accepts
	// only the three PVF words -- but the client draws it differently from a
	// merely hidden gate, and the 86JP reference server sets it (its value 3) the
	// moment a gate's clear count reaches its requirement
	// (RaidHandler.Progression.cs:240 for the legs, ClearBlackFogSourceAsync for
	// the regenerated fog).
	AntonDungeonCleared AntonDungeonState = "cleared"
)

// AntonEntryCost is the item/count pair inside the global [ENTRY COST] block.
type AntonEntryCost struct {
	ItemID uint32
	Count  uint32
}

// AntonStateReward preserves one [STATE REWARD] row in source order. ItemID is
// the reward-definition item, not an instruction to grant that item directly.
type AntonStateReward struct {
	Kind             string
	Rank             int
	Weight           int
	DefinitionItemID uint32
	Flag             byte
}

// AntonRankCondition is one [DEAD COUNT] row inside a phase's
// [CONDITION FOR RANK]: the inclusive team death-count range that earns Rank.
// The rows are kept in source order and raw. The current catalogs leave a
// one-death gap between adjacent rows (0-1, 2-5, 6-9, 10-14), so collapsing
// them into thresholds would invent a rule for the gap.
type AntonRankCondition struct {
	MinimumDeaths int
	MaximumDeaths int
	Rank          int
}

// AntonDungeonInfo preserves one ordinary-phase [DUNGEON INFO] block.
type AntonDungeonInfo struct {
	Index     uint32
	InitState AntonDungeonState
	HP        int
	PartyMax  int
	// LockoutType is the native [LOCKOUT TYPE] pair. Only the Luke catalog
	// carries the section (its phase-two gates use 4 on the light route, 5 on
	// the dark one and 0 on the two shared rooms); etc/raid/anton.etc omits it
	// and leaves the pair at zero. What the value gates is not proven by the
	// PVF alone, so both numbers stay raw.
	LockoutType [2]int
	// DestroyObjects lists the object-group markers declared under this gate's
	// [DESTROY OBJECT INFO], in source order and with the native section
	// spelling. The section has no closing tag, so its entries run to the end
	// of the [DUNGEON INFO] block. Only the Luke phase-one sanctums declare it
	// ("birth sanctum object" and its two siblings).
	DestroyObjects []string
}

// AntonPhase preserves one phase block and its ordered rewards and dungeons.
// The single-mode phase uses the same shape even though its PVF block omits
// the ordinary minimum-clear-count section.
type AntonPhase struct {
	Name                     string
	MinimalDungeonClearCount int
	Rewards                  []AntonStateReward
	Dungeons                 []AntonDungeonInfo
	// RankConditions is the phase's [CONDITION FOR RANK] table. Every phase of
	// every current raid catalog declares one, including the single-mode phase.
	RankConditions []AntonRankCondition
	// CoinLimit is the optional [PHASE COIN LIMIT] revive-coin cap. Zero means
	// the phase declares no cap, which is every Anton phase and Luke's first.
	CoinLimit int
}

// AntonRaidModeKind identifies a mode advertised by the native Anton PVF.
type AntonRaidModeKind string

const (
	AntonRaidModeTeam   AntonRaidModeKind = "team"
	AntonRaidModeSingle AntonRaidModeKind = "single"
)

// AntonRaidModeChoice is the small, read-only projection needed by a mode
// selection UI. The complete phase data remains available on AntonConfig.
type AntonRaidModeChoice struct {
	Kind                AntonRaidModeKind
	PhaseNames          []string
	StartMinimumMembers int
	TimeLimitSeconds    []int
}

// AntonSingleModeConfig is the complete phase data declared by the native
// [SINGLE RAID PHASE]. Trigger expressions which are client/server script
// details remain opaque; the dungeon graph is advanced by the runtime owner.
type AntonSingleModeConfig struct {
	Name             string
	TimeLimitSeconds int
	Phase            AntonPhase
}

// AntonConfig is the strictly validated, read-only subset of the current
// etc/raid/anton.etc needed by both ordinary and single-mode raid runtimes.
type AntonConfig struct {
	PhaseMax                  int
	MemberMax                 int
	StartMinimumMembers       int
	SingleStartMinimumMembers int
	StartDelaySeconds         int
	// DisconnMemberWaitTime is the global [DISCONN MEMBER WAIT TIME]: how long a
	// dropped member's attack-party seat is held before the member is removed.
	// Both shipped raids declare 300 (etc/raid/anton.etc and
	// etc/raid/luke/luke.etc). Zero means the document does not declare it, and
	// the runtime then keeps its own 300-second default.
	DisconnMemberWaitTime int
	PhaseTimeoutSeconds   []int
	PhaseBreakSeconds     int
	MovieEndSeconds       []int
	EntryCost             AntonEntryCost
	// WeeklyEnterCount preserves the two global [WEEKLY ENTER COUNT] values in
	// source order (Luke 20 2, Anton 30 3). The client draws a weekly counter of
	// its own (dstr 75388 "周卢克 %d/%d"), but which of the two values is the
	// roster limit and which the per-character share is not proven here, so both
	// stay raw and no admission policy is derived from them yet.
	WeeklyEnterCount [2]int
	Phases           []AntonPhase
	SingleMode       *AntonSingleModeConfig
}

// ModeChoices returns the native team/single choices in PVF order. It returns
// fresh slices so callers can use the projection for UI rendering without
// mutating the cached configuration. No lifecycle state is created or changed.
func (config *AntonConfig) ModeChoices() []AntonRaidModeChoice {
	if config == nil {
		return nil
	}
	choices := make([]AntonRaidModeChoice, 0, 2)
	if len(config.Phases) > 0 {
		choices = append(choices, AntonRaidModeChoice{
			Kind:                AntonRaidModeTeam,
			PhaseNames:          antonPhaseNames(config.Phases),
			StartMinimumMembers: config.StartMinimumMembers,
			TimeLimitSeconds:    append([]int(nil), config.PhaseTimeoutSeconds...),
		})
	}
	if config.SingleMode != nil {
		choices = append(choices, AntonRaidModeChoice{
			Kind:                AntonRaidModeSingle,
			PhaseNames:          []string{config.SingleMode.Name},
			StartMinimumMembers: config.SingleStartMinimumMembers,
			TimeLimitSeconds:    []int{config.SingleMode.TimeLimitSeconds},
		})
	}
	return choices
}

func antonPhaseNames(phases []AntonPhase) []string {
	names := make([]string, 0, len(phases))
	for _, phase := range phases {
		names = append(names, phase.Name)
	}
	return names
}

// LoadAntonConfig reads exactly etc/raid/anton.etc and parses no other archive
// member. This makes the source boundary usable by both the runtime archive and
// small in-memory unit-test sources.
func LoadAntonConfig(source dnfpvf.Source) (*AntonConfig, error) {
	if source == nil {
		return nil, ErrAntonPVFSourceRequired
	}
	text, err := source.ReadText(AntonPVFPath)
	if err != nil {
		return nil, fmt.Errorf("read anton raid PVF %q: %w", AntonPVFPath, err)
	}
	document, err := dnfpvf.Parse(AntonPVFPath, text)
	if err != nil {
		return nil, fmt.Errorf("parse anton raid PVF %q: %w", AntonPVFPath, err)
	}
	return ParseAntonConfig(document)
}

// ParseAntonConfig converts one already-tokenized target document. It rejects
// a document for any other path so callers cannot accidentally feed a nearby
// raid file into the Anton catalog.
func ParseAntonConfig(document *dnfpvf.Document) (*AntonConfig, error) {
	if document == nil || antonPathKey(document.Path) != antonPathKey(AntonPVFPath) {
		path := ""
		if document != nil {
			path = document.Path
		}
		return nil, fmt.Errorf("%w: path=%q want=%q", ErrAntonPVFDocumentInvalid, path, AntonPVFPath)
	}

	phaseBlocks, err := antonFindBlocks(document, "phase", 0, len(document.Sections))
	if err != nil {
		return nil, err
	}
	if len(phaseBlocks) == 0 {
		return nil, antonMissing("phase", "ordinary phases")
	}
	globalEnd := phaseBlocks[0].open
	singleBlocks, err := antonFindBlocks(document, antonSingleRaidPhaseName, 0, len(document.Sections))
	if err != nil {
		return nil, err
	}
	if len(singleBlocks) > 1 {
		return nil, antonShape(antonSingleRaidPhaseName, fmt.Sprintf("blocks=%d want<=1", len(singleBlocks)))
	}

	config := &AntonConfig{}
	if config.PhaseMax, err = antonReadPositiveIntSection(document, "phase max", 0, globalEnd); err != nil {
		return nil, err
	}
	if config.PhaseMax != antonOrdinaryPhaseCount {
		return nil, antonInvalid("phase max", config.PhaseMax, fmt.Sprintf("want=%d", antonOrdinaryPhaseCount))
	}
	if len(phaseBlocks) != config.PhaseMax {
		return nil, fmt.Errorf("%w: path=%s field=phase blocks got=%d want=%d", ErrAntonPVFSectionShape, AntonPVFPath, len(phaseBlocks), config.PhaseMax)
	}
	if config.MemberMax, err = antonReadPositiveIntSection(document, "raid member max", 0, globalEnd); err != nil {
		return nil, err
	}
	if config.MemberMax > MaxAttackPartyMembers {
		return nil, antonInvalid("raid member max", config.MemberMax, fmt.Sprintf("want<=%d", MaxAttackPartyMembers))
	}
	if config.StartMinimumMembers, err = antonReadPositiveIntSection(document, "raid start minimum member", 0, globalEnd); err != nil {
		return nil, err
	}
	// MINIUM is the spelling in the current native PVF and is deliberately
	// matched exactly instead of accepting an invented alias. Team-only PVFs
	// may omit both this field and [SINGLE RAID PHASE]. Once a single block is
	// declared, however, its minimum-member field is required.
	singleMinimumIndex, hasSingleMinimum, findErr := antonOptionalUniqueSection(document, "single raid start minium member", 0, globalEnd)
	if findErr != nil {
		return nil, findErr
	}
	if hasSingleMinimum {
		if config.SingleStartMinimumMembers, err = antonPositiveIntAt(document, singleMinimumIndex, "single raid start minium member"); err != nil {
			return nil, err
		}
	} else if len(singleBlocks) == 1 {
		return nil, antonMissing("single raid start minium member", "single raid phase")
	}
	if config.StartMinimumMembers > config.MemberMax {
		return nil, antonInvalid("raid start minimum member", config.StartMinimumMembers, "want<=raid member max")
	}
	if config.SingleStartMinimumMembers > config.MemberMax {
		return nil, antonInvalid("single raid start minium member", config.SingleStartMinimumMembers, "want<=raid member max")
	}
	if config.StartDelaySeconds, err = antonReadPositiveIntSection(document, "start delay time", 0, globalEnd); err != nil {
		return nil, err
	}
	// [DISCONN MEMBER WAIT TIME] is read optionally: both shipped raids declare
	// 300, but a document that omits the section must still parse, and the
	// runtime keeps its own default for the zero.
	disconnWaitIndex, hasDisconnWait, err := antonOptionalUniqueSection(document, "disconn member wait time", 0, globalEnd)
	if err != nil {
		return nil, err
	}
	if hasDisconnWait {
		if config.DisconnMemberWaitTime, err = antonPositiveIntAt(document, disconnWaitIndex, "disconn member wait time"); err != nil {
			return nil, err
		}
	}
	if config.PhaseTimeoutSeconds, err = antonReadPositiveIntListSection(document, "phase time over", 0, globalEnd, config.PhaseMax); err != nil {
		return nil, err
	}
	if config.PhaseBreakSeconds, err = antonReadPositiveIntSection(document, "phase break time", 0, globalEnd); err != nil {
		return nil, err
	}
	if config.MovieEndSeconds, err = antonReadPositiveIntListSection(document, "movie end time", 0, globalEnd, antonMovieEndValueCount); err != nil {
		return nil, err
	}
	if config.EntryCost, err = antonParseEntryCost(document, 0, globalEnd); err != nil {
		return nil, err
	}
	if config.WeeklyEnterCount, err = antonParseWeeklyEnterCount(document, 0, globalEnd); err != nil {
		return nil, err
	}
	seenDungeons := make(map[uint32]int)
	config.Phases = make([]AntonPhase, 0, len(phaseBlocks))
	for index, block := range phaseBlocks {
		phase, parseErr := antonParsePhase(document, block, index, config.MemberMax, seenDungeons)
		if parseErr != nil {
			return nil, parseErr
		}
		config.Phases = append(config.Phases, phase)
	}

	// The native single-mode declaration follows the ordinary phases. Its
	// trigger expressions remain opaque, while the phase name, rewards and
	// dungeon definitions are consumed by the single-mode runtime owner.
	if len(singleBlocks) == 1 {
		singleBlock := singleBlocks[0]
		timeLimitSeconds, readErr := antonReadPositiveIntSection(document, "single raid time over", 0, singleBlock.open)
		if readErr != nil {
			return nil, readErr
		}
		phaseName, parseErr := antonParseSingleModeName(document, singleBlock)
		if parseErr != nil {
			return nil, parseErr
		}
		phase, phaseErr := antonParseSinglePhase(document, singleBlock, config.MemberMax, seenDungeons)
		if phaseErr != nil {
			return nil, phaseErr
		}
		config.SingleMode = &AntonSingleModeConfig{Name: phaseName, TimeLimitSeconds: timeLimitSeconds, Phase: phase}
	}
	return config, nil
}

type antonSectionBlock struct {
	open  int
	close int
}

func antonParseEntryCost(document *dnfpvf.Document, start, end int) (AntonEntryCost, error) {
	blocks, err := antonFindBlocks(document, "entry cost", start, end)
	if err != nil {
		return AntonEntryCost{}, err
	}
	if len(blocks) == 0 {
		return AntonEntryCost{}, antonMissing("entry cost", "global")
	}
	if len(blocks) != 1 {
		return AntonEntryCost{}, antonShape("entry cost", fmt.Sprintf("blocks=%d want=1", len(blocks)))
	}
	block := blocks[0]
	if block.close != block.open+1 {
		return AntonEntryCost{}, antonShape("entry cost", "unexpected nested sections")
	}
	if err := antonRequireEmptySection(document, block.close); err != nil {
		return AntonEntryCost{}, err
	}
	tokens, err := antonSectionTokens(document, block.open)
	if err != nil {
		return AntonEntryCost{}, err
	}
	if len(tokens) != 2 {
		return AntonEntryCost{}, antonShape("entry cost", fmt.Sprintf("values=%d want=2", len(tokens)))
	}
	itemID, err := antonPositiveUint32Token(tokens[0], "entry cost item id")
	if err != nil {
		return AntonEntryCost{}, err
	}
	count, err := antonPositiveUint32Token(tokens[1], "entry cost count")
	if err != nil {
		return AntonEntryCost{}, err
	}
	return AntonEntryCost{ItemID: itemID, Count: count}, nil
}

// antonParseWeeklyEnterCount reads the global [WEEKLY ENTER COUNT] pair.
// etc/raid/fiendwar/fiendwar.etc declares the same section twice at global
// scope; that catalog is not parsed by this grammar, and a duplicate stays a
// shape error here rather than a silently preferred first row.
func antonParseWeeklyEnterCount(document *dnfpvf.Document, start, end int) ([2]int, error) {
	var values [2]int
	sectionIndex, err := antonUniqueSection(document, "weekly enter count", start, end)
	if err != nil {
		return values, err
	}
	tokens, err := antonSectionTokens(document, sectionIndex)
	if err != nil {
		return values, err
	}
	if len(tokens) != len(values) {
		return values, antonShape("weekly enter count", fmt.Sprintf("values=%d want=%d", len(tokens), len(values)))
	}
	for index, token := range tokens {
		value, valueErr := antonPositiveIntToken(token, "weekly enter count")
		if valueErr != nil {
			return [2]int{}, valueErr
		}
		values[index] = value
	}
	return values, nil
}

// antonParseRankConditions reads one phase's [CONDITION FOR RANK] block into
// its ordered [DEAD COUNT] rows. The block admits no other section.
func antonParseRankConditions(document *dnfpvf.Document, start, end int, context string) ([]AntonRankCondition, error) {
	blocks, err := antonFindBlocks(document, "condition for rank", start, end)
	if err != nil {
		return nil, err
	}
	if len(blocks) == 0 {
		return nil, antonMissing("condition for rank", context)
	}
	if len(blocks) != 1 {
		return nil, antonShape("condition for rank", fmt.Sprintf("%s blocks=%d want=1", context, len(blocks)))
	}
	block := blocks[0]
	if err := antonRequireEmptySection(document, block.open); err != nil {
		return nil, err
	}
	if err := antonRequireEmptySection(document, block.close); err != nil {
		return nil, err
	}
	rows := make([]AntonRankCondition, 0, block.close-block.open-1)
	for index := block.open + 1; index < block.close; index++ {
		if antonSectionName(document.Sections[index].Name) != "dead count" {
			return nil, antonShape("condition for rank",
				fmt.Sprintf("%s unexpected_section=%q", context, document.Sections[index].Name))
		}
		tokens, tokenErr := antonSectionTokens(document, index)
		if tokenErr != nil {
			return nil, tokenErr
		}
		if len(tokens) != 3 {
			return nil, antonShape("dead count", fmt.Sprintf("%s values=%d want=3", context, len(tokens)))
		}
		var values [3]int
		for position, token := range tokens {
			value, valueErr := antonIntToken(token, "dead count")
			if valueErr != nil {
				return nil, valueErr
			}
			if value < 0 {
				return nil, antonInvalid("dead count", value, fmt.Sprintf("%s want_nonnegative", context))
			}
			values[position] = value
		}
		if values[1] < values[0] {
			return nil, antonInvalid("dead count", values[1], fmt.Sprintf("%s want>=minimum(%d)", context, values[0]))
		}
		rows = append(rows, AntonRankCondition{MinimumDeaths: values[0], MaximumDeaths: values[1], Rank: values[2]})
	}
	if len(rows) == 0 {
		return nil, antonMissing("dead count", context)
	}
	return rows, nil
}

func antonParsePhase(document *dnfpvf.Document, block antonSectionBlock, phaseIndex, memberMax int, seenDungeons map[uint32]int) (AntonPhase, error) {
	// The native Anton PVF uses PARTY MAX=0 for maps whose party size is
	// governed by the attack-party rules rather than a dungeon-local cap.
	// Preserve that value for ordinary and single maps alike.
	return antonParsePhaseWithPartyMax(document, block, phaseIndex, memberMax, true, seenDungeons)
}

func antonParseSinglePhase(document *dnfpvf.Document, block antonSectionBlock, memberMax int, seenDungeons map[uint32]int) (AntonPhase, error) {
	if err := antonRequireEmptySection(document, block.open); err != nil {
		return AntonPhase{}, err
	}
	if err := antonRequireEmptySection(document, block.close); err != nil {
		return AntonPhase{}, err
	}
	start, end := block.open+1, block.close
	nameIndex, err := antonUniqueSection(document, "phase name", start, end)
	if err != nil {
		return AntonPhase{}, err
	}
	nameTokens, err := antonSectionTokens(document, nameIndex)
	if err != nil {
		return AntonPhase{}, err
	}
	if len(nameTokens) != 1 || nameTokens[0].Kind != dnfpvf.TokenString || strings.TrimSpace(nameTokens[0].Value) == "" {
		return AntonPhase{}, antonShape("phase name", "single phase want=one nonempty string")
	}
	rewardBlocks, err := antonFindBlocks(document, "clear reward item", start, end)
	if err != nil {
		return AntonPhase{}, err
	}
	if len(rewardBlocks) != 1 {
		return AntonPhase{}, antonShape("clear reward item", fmt.Sprintf("single phase blocks=%d want=1", len(rewardBlocks)))
	}
	rewards, err := antonParseRewards(document, rewardBlocks[0], 2)
	if err != nil {
		return AntonPhase{}, err
	}
	rankConditions, err := antonParseRankConditions(document, start, end, "single phase")
	if err != nil {
		return AntonPhase{}, err
	}
	dungeonBlocks, err := antonFindBlocks(document, "dungeon info", start, end)
	if err != nil {
		return AntonPhase{}, err
	}
	if len(dungeonBlocks) == 0 {
		return AntonPhase{}, antonMissing("dungeon info", "single phase")
	}
	phase := AntonPhase{
		Name:                     nameTokens[0].Value,
		MinimalDungeonClearCount: 1,
		Rewards:                  rewards,
		Dungeons:                 make([]AntonDungeonInfo, 0, len(dungeonBlocks)),
		RankConditions:           rankConditions,
	}
	for _, dungeonBlock := range dungeonBlocks {
		dungeon, parseErr := antonParseDungeonWithPartyMax(document, dungeonBlock, 2, memberMax, true)
		if parseErr != nil {
			return AntonPhase{}, parseErr
		}
		if firstPhase, exists := seenDungeons[dungeon.Index]; exists {
			return AntonPhase{}, fmt.Errorf("%w: path=%s dungeon=%d first_phase=%d duplicate_phase=single", ErrAntonPVFDuplicateDungeon, AntonPVFPath, dungeon.Index, firstPhase+1)
		}
		seenDungeons[dungeon.Index] = 2
		phase.Dungeons = append(phase.Dungeons, dungeon)
	}
	return phase, nil
}

func antonParsePhaseWithPartyMax(document *dnfpvf.Document, block antonSectionBlock, phaseIndex, memberMax int, allowZeroPartyMax bool, seenDungeons map[uint32]int) (AntonPhase, error) {
	if err := antonRequireEmptySection(document, block.open); err != nil {
		return AntonPhase{}, err
	}
	if err := antonRequireEmptySection(document, block.close); err != nil {
		return AntonPhase{}, err
	}
	start, end := block.open+1, block.close

	nameIndex, err := antonUniqueSection(document, "phase name", start, end)
	if err != nil {
		return AntonPhase{}, err
	}
	nameTokens, err := antonSectionTokens(document, nameIndex)
	if err != nil {
		return AntonPhase{}, err
	}
	if len(nameTokens) != 1 || nameTokens[0].Kind != dnfpvf.TokenString || strings.TrimSpace(nameTokens[0].Value) == "" {
		return AntonPhase{}, antonShape("phase name", fmt.Sprintf("phase=%d want=one nonempty string", phaseIndex+1))
	}
	phase := AntonPhase{Name: nameTokens[0].Value}

	rewardBlocks, err := antonFindBlocks(document, "clear reward item", start, end)
	if err != nil {
		return AntonPhase{}, err
	}
	if len(rewardBlocks) == 0 {
		return AntonPhase{}, antonMissing("clear reward item", fmt.Sprintf("phase=%d", phaseIndex+1))
	}
	if len(rewardBlocks) != 1 {
		return AntonPhase{}, antonShape("clear reward item", fmt.Sprintf("phase=%d blocks=%d want=1", phaseIndex+1, len(rewardBlocks)))
	}
	if nameIndex >= rewardBlocks[0].open {
		return AntonPhase{}, antonShape("phase", fmt.Sprintf("phase=%d phase name must precede rewards", phaseIndex+1))
	}
	phase.Rewards, err = antonParseRewards(document, rewardBlocks[0], phaseIndex)
	if err != nil {
		return AntonPhase{}, err
	}

	phase.RankConditions, err = antonParseRankConditions(document, start, end, fmt.Sprintf("phase=%d", phaseIndex+1))
	if err != nil {
		return AntonPhase{}, err
	}

	// [PHASE COIN LIMIT] is declared only by Luke's second phase; every Anton
	// phase omits it and keeps the zero.
	coinIndex, hasCoinLimit, err := antonOptionalUniqueSection(document, "phase coin limit", start, end)
	if err != nil {
		return AntonPhase{}, err
	}
	if hasCoinLimit {
		if phase.CoinLimit, err = antonPositiveIntAt(document, coinIndex, "phase coin limit"); err != nil {
			return AntonPhase{}, err
		}
	}

	minimalIndex, err := antonUniqueSection(document, "minimal dungeon clear count", start, end)
	if err != nil {
		return AntonPhase{}, err
	}
	if minimalIndex <= rewardBlocks[0].close {
		return AntonPhase{}, antonShape("phase", fmt.Sprintf("phase=%d minimum clear count must follow rewards", phaseIndex+1))
	}
	phase.MinimalDungeonClearCount, err = antonPositiveIntAt(document, minimalIndex, "minimal dungeon clear count")
	if err != nil {
		return AntonPhase{}, err
	}

	dungeonBlocks, err := antonFindBlocks(document, "dungeon info", start, end)
	if err != nil {
		return AntonPhase{}, err
	}
	if len(dungeonBlocks) == 0 {
		return AntonPhase{}, antonMissing("dungeon info", fmt.Sprintf("phase=%d", phaseIndex+1))
	}
	if dungeonBlocks[0].open <= minimalIndex {
		return AntonPhase{}, antonShape("phase", fmt.Sprintf("phase=%d dungeons must follow minimum clear count", phaseIndex+1))
	}
	phase.Dungeons = make([]AntonDungeonInfo, 0, len(dungeonBlocks))
	for _, dungeonBlock := range dungeonBlocks {
		dungeon, parseErr := antonParseDungeonWithPartyMax(document, dungeonBlock, phaseIndex, memberMax, allowZeroPartyMax)
		if parseErr != nil {
			return AntonPhase{}, parseErr
		}
		if firstPhase, exists := seenDungeons[dungeon.Index]; exists {
			return AntonPhase{}, fmt.Errorf("%w: path=%s dungeon=%d first_phase=%d duplicate_phase=%d", ErrAntonPVFDuplicateDungeon, AntonPVFPath, dungeon.Index, firstPhase+1, phaseIndex+1)
		}
		seenDungeons[dungeon.Index] = phaseIndex
		phase.Dungeons = append(phase.Dungeons, dungeon)
	}
	if phase.MinimalDungeonClearCount > len(phase.Dungeons) {
		return AntonPhase{}, antonInvalid("minimal dungeon clear count", phase.MinimalDungeonClearCount, fmt.Sprintf("phase=%d want<=dungeons(%d)", phaseIndex+1, len(phase.Dungeons)))
	}
	return phase, nil
}

func antonParseSingleModeName(document *dnfpvf.Document, block antonSectionBlock) (string, error) {
	if err := antonRequireEmptySection(document, block.open); err != nil {
		return "", err
	}
	if err := antonRequireEmptySection(document, block.close); err != nil {
		return "", err
	}
	start, end := block.open+1, block.close
	nameIndex, err := antonUniqueSection(document, "phase name", start, end)
	if err != nil {
		return "", err
	}
	nameTokens, err := antonSectionTokens(document, nameIndex)
	if err != nil {
		return "", err
	}
	if len(nameTokens) != 1 || nameTokens[0].Kind != dnfpvf.TokenString || strings.TrimSpace(nameTokens[0].Value) == "" {
		return "", antonShape("phase name", "single phase want=one nonempty string")
	}
	return nameTokens[0].Value, nil
}

func antonParseRewards(document *dnfpvf.Document, block antonSectionBlock, phaseIndex int) ([]AntonStateReward, error) {
	if err := antonRequireEmptySection(document, block.open); err != nil {
		return nil, err
	}
	if err := antonRequireEmptySection(document, block.close); err != nil {
		return nil, err
	}
	rewards := make([]AntonStateReward, 0, block.close-block.open-1)
	for sectionIndex := block.open + 1; sectionIndex < block.close; sectionIndex++ {
		if antonSectionName(document.Sections[sectionIndex].Name) != "state reward" {
			return nil, antonShape("clear reward item", fmt.Sprintf("phase=%d unexpected_section=%q", phaseIndex+1, document.Sections[sectionIndex].Name))
		}
		tokens, err := antonSectionTokens(document, sectionIndex)
		if err != nil {
			return nil, err
		}
		if len(tokens) != 5 || tokens[0].Kind != dnfpvf.TokenString || strings.TrimSpace(tokens[0].Value) == "" {
			return nil, antonShape("state reward", fmt.Sprintf("phase=%d values=%d want=kind+4_ints", phaseIndex+1, len(tokens)))
		}
		rank, err := antonIntToken(tokens[1], "state reward rank")
		if err != nil {
			return nil, err
		}
		if rank < -1 {
			return nil, antonInvalid("state reward rank", rank, "want>=-1")
		}
		weight, err := antonPositiveIntToken(tokens[2], "state reward weight")
		if err != nil {
			return nil, err
		}
		itemID, err := antonPositiveUint32Token(tokens[3], "state reward definition item id")
		if err != nil {
			return nil, err
		}
		flag, err := antonIntToken(tokens[4], "state reward flag")
		if err != nil {
			return nil, err
		}
		if flag != 0 && flag != 1 {
			return nil, antonInvalid("state reward flag", flag, "want=0_or_1")
		}
		rewards = append(rewards, AntonStateReward{
			Kind: tokens[0].Value, Rank: rank, Weight: weight, DefinitionItemID: itemID, Flag: byte(flag),
		})
	}
	if len(rewards) == 0 {
		return nil, antonMissing("state reward", fmt.Sprintf("phase=%d", phaseIndex+1))
	}
	return rewards, nil
}

func antonParseDungeon(document *dnfpvf.Document, block antonSectionBlock, phaseIndex, memberMax int) (AntonDungeonInfo, error) {
	return antonParseDungeonWithPartyMax(document, block, phaseIndex, memberMax, false)
}

func antonParseDungeonWithPartyMax(document *dnfpvf.Document, block antonSectionBlock, phaseIndex, memberMax int, allowZeroPartyMax bool) (AntonDungeonInfo, error) {
	if err := antonRequireEmptySection(document, block.open); err != nil {
		return AntonDungeonInfo{}, err
	}
	if err := antonRequireEmptySection(document, block.close); err != nil {
		return AntonDungeonInfo{}, err
	}
	start, end := block.open+1, block.close
	indexSection, err := antonUniqueSection(document, "index", start, end)
	if err != nil {
		return AntonDungeonInfo{}, err
	}
	indexTokens, err := antonSectionTokens(document, indexSection)
	if err != nil {
		return AntonDungeonInfo{}, err
	}
	if len(indexTokens) != 1 {
		return AntonDungeonInfo{}, antonShape("index", fmt.Sprintf("phase=%d values=%d want=1", phaseIndex+1, len(indexTokens)))
	}
	dungeonID, err := antonPositiveUint32Token(indexTokens[0], "dungeon index")
	if err != nil {
		return AntonDungeonInfo{}, err
	}

	initSection, err := antonUniqueSection(document, "init state", start, end)
	if err != nil {
		return AntonDungeonInfo{}, err
	}
	initTokens, err := antonSectionTokens(document, initSection)
	if err != nil {
		return AntonDungeonInfo{}, err
	}
	if len(initTokens) != 1 || initTokens[0].Kind != dnfpvf.TokenString {
		return AntonDungeonInfo{}, antonShape("init state", fmt.Sprintf("dungeon=%d want=one string", dungeonID))
	}
	state := AntonDungeonState(strings.ToLower(strings.TrimSpace(initTokens[0].Value)))
	if state != AntonDungeonOpen && state != AntonDungeonHide && state != AntonDungeonClosed {
		return AntonDungeonInfo{}, antonInvalid("init state", initTokens[0].Value, fmt.Sprintf("dungeon=%d", dungeonID))
	}

	hpInfoSection, err := antonUniqueSection(document, "hp info", start, end)
	if err != nil {
		return AntonDungeonInfo{}, err
	}
	if err := antonRequireEmptySection(document, hpInfoSection); err != nil {
		return AntonDungeonInfo{}, err
	}
	hpSectionName := fmt.Sprintf("%d dgn hp", dungeonID)
	hpSection, err := antonUniqueSection(document, hpSectionName, start, end)
	if err != nil {
		return AntonDungeonInfo{}, err
	}
	hp, err := antonPositiveIntAt(document, hpSection, hpSectionName)
	if err != nil {
		return AntonDungeonInfo{}, err
	}

	partyMaxSection, err := antonUniqueSection(document, "party max", start, end)
	if err != nil {
		return AntonDungeonInfo{}, err
	}
	partyMax, err := antonNonNegativeIntAt(document, partyMaxSection, "party max")
	if err != nil {
		return AntonDungeonInfo{}, err
	}
	if partyMax < 0 || (!allowZeroPartyMax && partyMax == 0) || partyMax > memberMax {
		return AntonDungeonInfo{}, antonInvalid("party max", partyMax, fmt.Sprintf("dungeon=%d want<=member_max(%d)", dungeonID, memberMax))
	}

	if !(indexSection < initSection && initSection < hpInfoSection && hpInfoSection < hpSection && hpSection < partyMaxSection) {
		return AntonDungeonInfo{}, antonShape("dungeon info", fmt.Sprintf("dungeon=%d fields_out_of_order", dungeonID))
	}

	var lockoutType [2]int
	lockoutSection, hasLockout, err := antonOptionalUniqueSection(document, "lockout type", start, end)
	if err != nil {
		return AntonDungeonInfo{}, err
	}
	if hasLockout {
		lockoutTokens, tokenErr := antonSectionTokens(document, lockoutSection)
		if tokenErr != nil {
			return AntonDungeonInfo{}, tokenErr
		}
		if len(lockoutTokens) != len(lockoutType) {
			return AntonDungeonInfo{}, antonShape("lockout type",
				fmt.Sprintf("dungeon=%d values=%d want=%d", dungeonID, len(lockoutTokens), len(lockoutType)))
		}
		for position, token := range lockoutTokens {
			value, valueErr := antonIntToken(token, "lockout type")
			if valueErr != nil {
				return AntonDungeonInfo{}, valueErr
			}
			if value < 0 {
				return AntonDungeonInfo{}, antonInvalid("lockout type", value, fmt.Sprintf("dungeon=%d want_nonnegative", dungeonID))
			}
			lockoutType[position] = value
		}
	}

	// [DESTROY OBJECT INFO] has no closing tag, so its object-group markers are
	// every remaining section of this [DUNGEON INFO] block. They carry no
	// values; the name is the payload.
	var destroyObjects []string
	destroySection, hasDestroyObjects, err := antonOptionalUniqueSection(document, "destroy object info", start, end)
	if err != nil {
		return AntonDungeonInfo{}, err
	}
	strictEnd := end
	if hasDestroyObjects {
		if err := antonRequireEmptySection(document, destroySection); err != nil {
			return AntonDungeonInfo{}, err
		}
		if destroySection < partyMaxSection {
			return AntonDungeonInfo{}, antonShape("destroy object info",
				fmt.Sprintf("dungeon=%d must follow party max", dungeonID))
		}
		strictEnd = destroySection
		for sectionIndex := destroySection + 1; sectionIndex < end; sectionIndex++ {
			if err := antonRequireEmptySection(document, sectionIndex); err != nil {
				return AntonDungeonInfo{}, err
			}
			destroyObjects = append(destroyObjects, antonSectionName(document.Sections[sectionIndex].Name))
		}
		if len(destroyObjects) == 0 {
			return AntonDungeonInfo{}, antonMissing("destroy object info", fmt.Sprintf("dungeon=%d object groups", dungeonID))
		}
	}

	allowed := map[string]struct{}{
		"index": {}, "init state": {}, "hp info": {}, hpSectionName: {}, "party max": {},
		"lockout type": {},
	}
	for sectionIndex := start; sectionIndex < strictEnd; sectionIndex++ {
		if _, ok := allowed[antonSectionName(document.Sections[sectionIndex].Name)]; !ok {
			return AntonDungeonInfo{}, antonShape("dungeon info", fmt.Sprintf("dungeon=%d unexpected_section=%q", dungeonID, document.Sections[sectionIndex].Name))
		}
	}
	return AntonDungeonInfo{
		Index:          dungeonID,
		InitState:      state,
		HP:             hp,
		PartyMax:       partyMax,
		LockoutType:    lockoutType,
		DestroyObjects: destroyObjects,
	}, nil
}

func antonFindBlocks(document *dnfpvf.Document, name string, start, end int) ([]antonSectionBlock, error) {
	want := antonSectionName(name)
	closeName := "/" + want
	openIndex := -1
	blocks := make([]antonSectionBlock, 0)
	for index := start; index < end; index++ {
		sectionName := antonSectionName(document.Sections[index].Name)
		switch sectionName {
		case want:
			if openIndex >= 0 {
				return nil, antonShape(name, fmt.Sprintf("nested_open_at=%d", index))
			}
			openIndex = index
		case closeName:
			if openIndex < 0 {
				return nil, antonShape(name, fmt.Sprintf("unmatched_close_at=%d", index))
			}
			blocks = append(blocks, antonSectionBlock{open: openIndex, close: index})
			openIndex = -1
		}
	}
	if openIndex >= 0 {
		return nil, antonShape(name, fmt.Sprintf("unclosed_open_at=%d", openIndex))
	}
	return blocks, nil
}

func antonReadPositiveIntSection(document *dnfpvf.Document, name string, start, end int) (int, error) {
	sectionIndex, err := antonUniqueSection(document, name, start, end)
	if err != nil {
		return 0, err
	}
	return antonPositiveIntAt(document, sectionIndex, name)
}

func antonReadPositiveIntListSection(document *dnfpvf.Document, name string, start, end, want int) ([]int, error) {
	sectionIndex, err := antonUniqueSection(document, name, start, end)
	if err != nil {
		return nil, err
	}
	tokens, err := antonSectionTokens(document, sectionIndex)
	if err != nil {
		return nil, err
	}
	if len(tokens) != want {
		return nil, antonShape(name, fmt.Sprintf("values=%d want=%d", len(tokens), want))
	}
	values := make([]int, 0, len(tokens))
	for _, token := range tokens {
		value, valueErr := antonPositiveIntToken(token, name)
		if valueErr != nil {
			return nil, valueErr
		}
		values = append(values, value)
	}
	return values, nil
}

func antonPositiveIntAt(document *dnfpvf.Document, sectionIndex int, field string) (int, error) {
	tokens, err := antonSectionTokens(document, sectionIndex)
	if err != nil {
		return 0, err
	}
	if len(tokens) != 1 {
		return 0, antonShape(field, fmt.Sprintf("values=%d want=1", len(tokens)))
	}
	return antonPositiveIntToken(tokens[0], field)
}

func antonNonNegativeIntAt(document *dnfpvf.Document, sectionIndex int, field string) (int, error) {
	tokens, err := antonSectionTokens(document, sectionIndex)
	if err != nil {
		return 0, err
	}
	if len(tokens) != 1 {
		return 0, antonShape(field, fmt.Sprintf("values=%d want=1", len(tokens)))
	}
	value, err := antonIntToken(tokens[0], field)
	if err != nil {
		return 0, err
	}
	if value < 0 {
		return 0, antonInvalid(field, value, "want_nonnegative")
	}
	return value, nil
}

func antonUniqueSection(document *dnfpvf.Document, name string, start, end int) (int, error) {
	found, ok, err := antonOptionalUniqueSection(document, name, start, end)
	if err != nil {
		return -1, err
	}
	if !ok {
		return -1, antonMissing(name, "required")
	}
	return found, nil
}

func antonOptionalUniqueSection(document *dnfpvf.Document, name string, start, end int) (int, bool, error) {
	want := antonSectionName(name)
	found := -1
	for index := start; index < end; index++ {
		if antonSectionName(document.Sections[index].Name) != want {
			continue
		}
		if found >= 0 {
			return -1, false, antonShape(name, fmt.Sprintf("duplicate_at=%d,%d", found, index))
		}
		found = index
	}
	if found < 0 {
		return -1, false, nil
	}
	return found, true, nil
}

func antonSectionTokens(document *dnfpvf.Document, sectionIndex int) ([]dnfpvf.Token, error) {
	if sectionIndex < 0 || sectionIndex >= len(document.Sections) {
		return nil, antonShape("section", fmt.Sprintf("index=%d sections=%d", sectionIndex, len(document.Sections)))
	}
	section := document.Sections[sectionIndex]
	if section.Start < 0 || section.End < section.Start || section.End > len(document.Tokens) {
		return nil, antonShape(section.Name, fmt.Sprintf("token_range=%d:%d tokens=%d", section.Start, section.End, len(document.Tokens)))
	}
	return document.Tokens[section.Start:section.End], nil
}

func antonRequireEmptySection(document *dnfpvf.Document, sectionIndex int) error {
	tokens, err := antonSectionTokens(document, sectionIndex)
	if err != nil {
		return err
	}
	if len(tokens) != 0 {
		return antonShape(document.Sections[sectionIndex].Name, fmt.Sprintf("values=%d want=0", len(tokens)))
	}
	return nil
}

func antonIntToken(token dnfpvf.Token, field string) (int, error) {
	if token.Kind != dnfpvf.TokenInt || token.Int < math.MinInt32 || token.Int > math.MaxInt32 {
		return 0, antonInvalid(field, token.Raw, "want=int32")
	}
	return int(token.Int), nil
}

func antonPositiveIntToken(token dnfpvf.Token, field string) (int, error) {
	value, err := antonIntToken(token, field)
	if err != nil {
		return 0, err
	}
	if value <= 0 {
		return 0, antonInvalid(field, value, "want_positive")
	}
	return value, nil
}

func antonPositiveUint32Token(token dnfpvf.Token, field string) (uint32, error) {
	if token.Kind != dnfpvf.TokenInt || token.Int <= 0 || token.Int > math.MaxUint32 {
		return 0, antonInvalid(field, token.Raw, "want=positive_uint32")
	}
	return uint32(token.Int), nil
}

func antonMissing(section, context string) error {
	return fmt.Errorf("%w: path=%s section=%s context=%s", ErrAntonPVFSectionMissing, AntonPVFPath, section, context)
}

func antonShape(section, detail string) error {
	return fmt.Errorf("%w: path=%s section=%s %s", ErrAntonPVFSectionShape, AntonPVFPath, section, detail)
}

func antonInvalid(field string, value any, detail string) error {
	return fmt.Errorf("%w: path=%s field=%s value=%v %s", ErrAntonPVFValueInvalid, AntonPVFPath, field, value, detail)
}

func antonPathKey(value string) string {
	value = strings.TrimSpace(strings.ReplaceAll(value, "\\", "/"))
	value = strings.TrimLeft(value, "/")
	return strings.ToLower(value)
}

func antonSectionName(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}
