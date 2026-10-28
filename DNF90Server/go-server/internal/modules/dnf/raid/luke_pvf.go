package raid

import (
	"errors"
	"fmt"
	"strings"

	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
)

const LukePVFPath = "etc/raid/luke/luke.etc"

var (
	ErrLukePVFSourceRequired  = errors.New("luke raid PVF source is required")
	ErrLukePVFDocumentInvalid = errors.New("luke raid PVF document is invalid")
	ErrLukePVFRouteInvalid    = errors.New("luke single raid PVF route is invalid")
)

// LukeSanctumObjectCount is one [DEFAULT OBJECT COUNT] entry: how many
// destroyed generators a phase-one sanctum's counter holds (the 「破坏目标 x/5」
// denominator), and the gate they belong to. It is not a placement count: the
// birth sanctum's maze 0 alone places ten 64001 generators. Section is the native marker name ("birth sanctum" and its two
// siblings); the gate's own [DESTROY OBJECT INFO] names the same group with an
// " object" suffix.
type LukeSanctumObjectCount struct {
	Section   string
	Count     int
	DungeonID uint32
}

// LukePhaseOneObjectDestroy is one row of the global [1PHASE OBJ DESTROY]
// table: a tier and the inclusive range of phase-one generators destroyed
// (all three sanctums together) that earns it. The current PVF reads
// `0 0 5  1 6 10  2 11 15`: 0-5 destroyed is tier 0, 6-10 tier 1, 11-15 tier 2.
//
// The tier is [ENERGY CONTROL OPEN] (symbol 133), which luke.etc only ever
// reads: its three `5063 clear` triggers pick the energy control room's
// respawn timer by it (0 → 45 s, 1 → 90 s, 2 → 120 s). Group keeps the field's
// original name; it holds the tier.
type LukePhaseOneObjectDestroy struct {
	Group   int
	Minimum int
	Maximum int
}

// EnergyControlTier maps a phase-one destroyed-generator total onto its
// [1PHASE OBJ DESTROY] tier. ok is false when no row covers the total.
func (c *LukeConfig) EnergyControlTier(destroyed int) (tier int, ok bool) {
	if c == nil {
		return 0, false
	}
	for _, row := range c.PhaseOneObjectDestroy {
		if destroyed >= row.Minimum && destroyed <= row.Maximum {
			return row.Group, true
		}
	}
	return 0, false
}

// SanctumObjectCount is the [DEFAULT OBJECT COUNT] of one phase-one sanctum:
// how many destroyed generators that sanctum's counter can hold (5).
func (c *LukeConfig) SanctumObjectCount(dungeonID uint32) (int, bool) {
	if c == nil {
		return 0, false
	}
	for _, entry := range c.SanctumObjectCounts {
		if entry.DungeonID == dungeonID {
			return entry.Count, true
		}
	}
	return 0, false
}

// LukeConfig owns the current PVF's Luke raid configuration. The phase,
// ticket and reward shapes are shared with the native raid grammar parsed by
// AntonConfig; Luke-specific route identities remain explicit here.
type LukeConfig struct {
	Raid                   *AntonConfig
	DailyAccountEnterLimit int
	// SanctumTimerCount is the global [SANCTUM TIMER COUNT]. It matches the
	// per-sanctum object count the client draws as the phase-one energy bar.
	SanctumTimerCount int
	// SanctumObjectCounts is [DEFAULT OBJECT COUNT] in source order.
	SanctumObjectCounts []LukeSanctumObjectCount
	// PhaseOneObjectDestroy is [1PHASE OBJ DESTROY] in source order.
	PhaseOneObjectDestroy []LukePhaseOneObjectDestroy
	// CardSetMapInfo is the raw [CARD SET MAP INFO] value list. The client
	// renders the phase-one card puzzle from symbols 130..132
	// ([CARD SET PATTERN A/B/C] in etc/raid/luke/luke.etc.symbol), but how this
	// list is grouped onto them is not proven by the PVF, so it stays raw.
	CardSetMapInfo []int
	LightRoute     []uint32
	DarkRoute      []uint32
}

func LoadLukeConfig(source dnfpvf.Source) (*LukeConfig, error) {
	if source == nil {
		return nil, ErrLukePVFSourceRequired
	}
	text, err := source.ReadText(LukePVFPath)
	if err != nil {
		return nil, fmt.Errorf("read luke raid PVF %q: %w", LukePVFPath, err)
	}
	document, err := dnfpvf.Parse(LukePVFPath, text)
	if err != nil {
		return nil, fmt.Errorf("parse luke raid PVF %q: %w", LukePVFPath, err)
	}
	return ParseLukeConfig(document)
}

func ParseLukeConfig(document *dnfpvf.Document) (*LukeConfig, error) {
	if document == nil || antonPathKey(document.Path) != antonPathKey(LukePVFPath) {
		path := ""
		if document != nil {
			path = document.Path
		}
		return nil, fmt.Errorf("%w: path=%q want=%q", ErrLukePVFDocumentInvalid, path, LukePVFPath)
	}
	phaseBlocks, err := antonFindBlocks(document, "phase", 0, len(document.Sections))
	if err != nil || len(phaseBlocks) == 0 {
		return nil, fmt.Errorf("%w: ordinary phase boundary: %v", ErrLukePVFDocumentInvalid, err)
	}
	globalEnd := phaseBlocks[0].open
	dailyLimit, err := antonReadPositiveIntSection(document, "daily account enter limit", 0, globalEnd)
	if err != nil {
		return nil, fmt.Errorf("%w: daily account enter limit: %v", ErrLukePVFDocumentInvalid, err)
	}
	sanctumTimerCount, err := antonReadPositiveIntSection(document, "sanctum timer count", 0, globalEnd)
	if err != nil {
		return nil, fmt.Errorf("%w: sanctum timer count: %v", ErrLukePVFDocumentInvalid, err)
	}
	sanctumObjectCounts, err := parseLukeSanctumObjectCounts(document, 0, globalEnd)
	if err != nil {
		return nil, err
	}
	phaseOneObjectDestroy, err := parseLukePhaseOneObjectDestroy(document, 0, globalEnd)
	if err != nil {
		return nil, err
	}
	cardSetMapInfo, err := parseLukeCardSetMapInfo(document, 0, globalEnd)
	if err != nil {
		return nil, err
	}

	// The two raid documents use the same native section grammar. Parse a
	// path-adjusted document through the one strict grammar owner, then restore
	// Luke identity in this wrapper instead of maintaining a divergent parser.
	// The shared grammar now reads Luke's per-gate [LOCKOUT TYPE] and
	// [DESTROY OBJECT INFO] itself, so no section is hidden from it any more;
	// only the tokens are copied, because Luke deliberately uses zero for the
	// ordinary-phase minimum clear count and Anton rejects that value. The Luke
	// values are preserved, a parser-safe sentinel is substituted in the private
	// copy, and the native values are restored on the resulting config.
	shared := *document
	shared.Path = AntonPVFPath
	shared.Tokens = append([]dnfpvf.Token(nil), document.Tokens...)
	minimumClearCounts := make([]int, 0, len(phaseBlocks))
	for phaseIndex, block := range phaseBlocks {
		minimumIndex, findErr := antonUniqueSection(document, "minimal dungeon clear count", block.open+1, block.close)
		if findErr != nil {
			return nil, fmt.Errorf("%w: phase=%d minimum clear count: %v", ErrLukePVFDocumentInvalid, phaseIndex+1, findErr)
		}
		minimum, readErr := antonNonNegativeIntAt(document, minimumIndex, "minimal dungeon clear count")
		if readErr != nil {
			return nil, fmt.Errorf("%w: phase=%d minimum clear count: %v", ErrLukePVFDocumentInvalid, phaseIndex+1, readErr)
		}
		minimumClearCounts = append(minimumClearCounts, minimum)
		if minimum == 0 {
			section := document.Sections[minimumIndex]
			if section.End != section.Start+1 {
				return nil, fmt.Errorf("%w: phase=%d minimum clear count shape", ErrLukePVFDocumentInvalid, phaseIndex+1)
			}
			shared.Tokens[section.Start].Int = 1
			shared.Tokens[section.Start].Raw = "1"
			shared.Tokens[section.Start].Value = "1"
		}
	}
	base, err := ParseAntonConfig(&shared)
	if err != nil {
		return nil, fmt.Errorf("%w: shared raid grammar: %v", ErrLukePVFDocumentInvalid, err)
	}
	for index, minimum := range minimumClearCounts {
		base.Phases[index].MinimalDungeonClearCount = minimum
	}
	if base.SingleMode == nil {
		return nil, fmt.Errorf("%w: single phase missing", ErrLukePVFRouteInvalid)
	}

	config := &LukeConfig{
		Raid:                   base,
		DailyAccountEnterLimit: dailyLimit,
		SanctumTimerCount:      sanctumTimerCount,
		SanctumObjectCounts:    sanctumObjectCounts,
		PhaseOneObjectDestroy:  phaseOneObjectDestroy,
		CardSetMapInfo:         cardSetMapInfo,
		LightRoute:             []uint32{5323, 5324, 5325, 5326, 5327, 5328},
		DarkRoute:              []uint32{5329, 5330, 5331, 5332, 5333, 5334},
	}
	if err := validateLukeSingleRoutes(base.SingleMode.Phase.Dungeons, config.LightRoute, config.DarkRoute); err != nil {
		return nil, err
	}
	if err := validateLukeSanctumObjects(base, config); err != nil {
		return nil, err
	}
	return config, nil
}

// parseLukeSanctumObjectCounts reads the global [DEFAULT OBJECT COUNT] block.
// Each entry is a marker section carrying the object count and the gate id.
func parseLukeSanctumObjectCounts(document *dnfpvf.Document, start, end int) ([]LukeSanctumObjectCount, error) {
	blocks, err := antonFindBlocks(document, "default object count", start, end)
	if err != nil {
		return nil, fmt.Errorf("%w: default object count: %v", ErrLukePVFDocumentInvalid, err)
	}
	if len(blocks) != 1 {
		return nil, fmt.Errorf("%w: default object count blocks=%d want=1", ErrLukePVFDocumentInvalid, len(blocks))
	}
	block := blocks[0]
	entries := make([]LukeSanctumObjectCount, 0, block.close-block.open-1)
	for index := block.open + 1; index < block.close; index++ {
		tokens, tokenErr := antonSectionTokens(document, index)
		if tokenErr != nil {
			return nil, fmt.Errorf("%w: default object count: %v", ErrLukePVFDocumentInvalid, tokenErr)
		}
		if len(tokens) != 2 {
			return nil, fmt.Errorf("%w: default object count section=%q values=%d want=2",
				ErrLukePVFDocumentInvalid, document.Sections[index].Name, len(tokens))
		}
		count, countErr := antonPositiveIntToken(tokens[0], "default object count")
		if countErr != nil {
			return nil, fmt.Errorf("%w: default object count: %v", ErrLukePVFDocumentInvalid, countErr)
		}
		dungeonID, dungeonErr := antonPositiveUint32Token(tokens[1], "default object count dungeon")
		if dungeonErr != nil {
			return nil, fmt.Errorf("%w: default object count: %v", ErrLukePVFDocumentInvalid, dungeonErr)
		}
		entries = append(entries, LukeSanctumObjectCount{
			Section:   antonSectionName(document.Sections[index].Name),
			Count:     count,
			DungeonID: dungeonID,
		})
	}
	if len(entries) == 0 {
		return nil, fmt.Errorf("%w: default object count has no entries", ErrLukePVFDocumentInvalid)
	}
	return entries, nil
}

// parseLukePhaseOneObjectDestroy reads the global [1PHASE OBJ DESTROY] table.
// The values sit on the opening section as consecutive group/min/max triples.
func parseLukePhaseOneObjectDestroy(document *dnfpvf.Document, start, end int) ([]LukePhaseOneObjectDestroy, error) {
	values, err := lukeBlockIntValues(document, "1phase obj destroy", start, end)
	if err != nil {
		return nil, err
	}
	if len(values)%3 != 0 {
		return nil, fmt.Errorf("%w: 1phase obj destroy values=%d want_multiple_of=3", ErrLukePVFDocumentInvalid, len(values))
	}
	rows := make([]LukePhaseOneObjectDestroy, 0, len(values)/3)
	for index := 0; index < len(values); index += 3 {
		row := LukePhaseOneObjectDestroy{Group: values[index], Minimum: values[index+1], Maximum: values[index+2]}
		if row.Maximum < row.Minimum {
			return nil, fmt.Errorf("%w: 1phase obj destroy group=%d range=%d..%d",
				ErrLukePVFDocumentInvalid, row.Group, row.Minimum, row.Maximum)
		}
		rows = append(rows, row)
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("%w: 1phase obj destroy has no rows", ErrLukePVFDocumentInvalid)
	}
	return rows, nil
}

func parseLukeCardSetMapInfo(document *dnfpvf.Document, start, end int) ([]int, error) {
	values, err := lukeBlockIntValues(document, "card set map info", start, end)
	if err != nil {
		return nil, err
	}
	if len(values) == 0 {
		return nil, fmt.Errorf("%w: card set map info has no values", ErrLukePVFDocumentInvalid)
	}
	return values, nil
}

// lukeBlockIntValues reads one single-section block whose values sit on the
// opening section, with an empty closing tag and nothing nested between them.
func lukeBlockIntValues(document *dnfpvf.Document, name string, start, end int) ([]int, error) {
	blocks, err := antonFindBlocks(document, name, start, end)
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %v", ErrLukePVFDocumentInvalid, name, err)
	}
	if len(blocks) != 1 {
		return nil, fmt.Errorf("%w: %s blocks=%d want=1", ErrLukePVFDocumentInvalid, name, len(blocks))
	}
	block := blocks[0]
	if block.close != block.open+1 {
		return nil, fmt.Errorf("%w: %s unexpected nested sections", ErrLukePVFDocumentInvalid, name)
	}
	if err := antonRequireEmptySection(document, block.close); err != nil {
		return nil, fmt.Errorf("%w: %s: %v", ErrLukePVFDocumentInvalid, name, err)
	}
	tokens, err := antonSectionTokens(document, block.open)
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %v", ErrLukePVFDocumentInvalid, name, err)
	}
	values := make([]int, 0, len(tokens))
	for _, token := range tokens {
		value, valueErr := antonIntToken(token, name)
		if valueErr != nil {
			return nil, fmt.Errorf("%w: %s: %v", ErrLukePVFDocumentInvalid, name, valueErr)
		}
		if value < 0 {
			return nil, fmt.Errorf("%w: %s value=%d want_nonnegative", ErrLukePVFDocumentInvalid, name, value)
		}
		values = append(values, value)
	}
	return values, nil
}

// validateLukeSanctumObjects proves the phase-one object wiring is internally
// consistent: every [DEFAULT OBJECT COUNT] entry must name a phase-one gate
// that declares the matching group under its own [DESTROY OBJECT INFO], and
// the count must agree with the global [SANCTUM TIMER COUNT]. The two sides
// spell the same group with and without an " object" suffix.
func validateLukeSanctumObjects(base *AntonConfig, config *LukeConfig) error {
	if base == nil || len(base.Phases) == 0 {
		return fmt.Errorf("%w: sanctum objects: no ordinary phase", ErrLukePVFDocumentInvalid)
	}
	groups := make(map[uint32]map[string]struct{}, len(base.Phases[0].Dungeons))
	for _, dungeon := range base.Phases[0].Dungeons {
		declared := make(map[string]struct{}, len(dungeon.DestroyObjects))
		for _, object := range dungeon.DestroyObjects {
			declared[object] = struct{}{}
		}
		groups[dungeon.Index] = declared
	}
	for _, entry := range config.SanctumObjectCounts {
		declared, ok := groups[entry.DungeonID]
		if !ok {
			return fmt.Errorf("%w: sanctum object dungeon=%d not in phase one", ErrLukePVFDocumentInvalid, entry.DungeonID)
		}
		want := entry.Section + " object"
		if _, found := declared[want]; !found {
			return fmt.Errorf("%w: sanctum object dungeon=%d missing group=%q",
				ErrLukePVFDocumentInvalid, entry.DungeonID, want)
		}
		if entry.Count != config.SanctumTimerCount {
			return fmt.Errorf("%w: sanctum object dungeon=%d count=%d sanctum_timer_count=%d",
				ErrLukePVFDocumentInvalid, entry.DungeonID, entry.Count, config.SanctumTimerCount)
		}
	}
	return nil
}

func validateLukeSingleRoutes(dungeons []AntonDungeonInfo, routes ...[]uint32) error {
	want := make(map[uint32]struct{})
	for _, route := range routes {
		if len(route) == 0 {
			return fmt.Errorf("%w: empty route", ErrLukePVFRouteInvalid)
		}
		for _, dungeonID := range route {
			if dungeonID == 0 {
				return fmt.Errorf("%w: zero dungeon id", ErrLukePVFRouteInvalid)
			}
			if _, duplicate := want[dungeonID]; duplicate {
				return fmt.Errorf("%w: duplicate dungeon=%d", ErrLukePVFRouteInvalid, dungeonID)
			}
			want[dungeonID] = struct{}{}
		}
	}
	if len(dungeons) != len(want) {
		return fmt.Errorf("%w: dungeons=%d want=%d", ErrLukePVFRouteInvalid, len(dungeons), len(want))
	}
	for _, dungeon := range dungeons {
		if _, ok := want[dungeon.Index]; !ok {
			return fmt.Errorf("%w: unexpected dungeon=%d", ErrLukePVFRouteInvalid, dungeon.Index)
		}
		if strings.TrimSpace(string(dungeon.InitState)) != string(AntonDungeonHide) || dungeon.HP <= 0 {
			return fmt.Errorf("%w: dungeon=%d state=%q hp=%d", ErrLukePVFRouteInvalid, dungeon.Index, dungeon.InitState, dungeon.HP)
		}
	}
	return nil
}
