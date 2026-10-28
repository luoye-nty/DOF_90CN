package raid

import (
	"errors"
	"fmt"
	"sort"

	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
)

// LukeCardSetPVFPath is the tarot table the client's CardSetWindow draws from.
//
// It is a document of its own: luke.etc owns the phase graph and the
// [CARD SET MAP INFO] record list, while this file owns the 8 pattern slots and
// the 22 major arcana. The client loads it into LukeCardSet (window+0x1a8) and
// looks up the notification's (dungeonID, patternIndex) pair in the pattern map
// at config+0x14 (tools\_rev_卢克塔罗牌_1181规格_20260929.md §7.2), and each
// card slot's P in the card map at config+0x00 (§7.3, §8.2).
const LukeCardSetPVFPath = "etc/raid/luke/LukeCardSet.etc"

var (
	ErrLukeCardSetSourceRequired  = errors.New("luke card set PVF source is required")
	ErrLukeCardSetDocumentInvalid = errors.New("luke card set PVF document is invalid")
)

// LukeCardPosition is one `[CARD POS]` (x, y) pair.
//
// Coordinates sit on a 0..4 grid in half steps (0, 0.50, 1, 1.50 ...), which is
// why they are floats and not ints. The client reads them from this table
// itself -- the notification body never carries coordinates -- so they are
// parsed here for counting (the slot count K) and for diagnostics only.
type LukeCardPosition struct {
	X float64
	Y float64
}

// LukeCardPattern is one `[PATTERN] <dungeonID> <patternIndex>` block, the tarot
// display of one phase-one sanctum: which buff-system icon it shows, where its
// seven card slots sit, and the tooltip the window prints.
//
// patternIndex is the notification's field B and dungeonID its field A; together
// they are the 8-byte key the client's pattern map is built from (the two dwords
// pushed at 0x03926BD6).
type LukeCardPattern struct {
	DungeonID     uint32
	Index         int
	ImagePath     string
	ImageIndices  []int
	CardPositions []LukeCardPosition
	Tooltip       string
}

// LukeCard is one `[CARD] <monsterID>` block: a major arcanum. MonsterID is the
// u32 the notification puts in every card slot (field P).
type LukeCard struct {
	MonsterID  uint32
	ImagePath  string
	ImageIndex int
	Tooltip    string
}

// LukeCardSetConfig owns etc/raid/luke/LukeCardSet.etc. Both tables are global:
// the 22 cards are not per-pattern (the `[PATTERN]` blocks carry no `[CARD]`
// child at all), and the `[CARD POS]` pairs are the seven slots *inside* one
// pattern, not seven cards.
type LukeCardSetConfig struct {
	// Patterns is every `[PATTERN]` block in source order.
	Patterns []LukeCardPattern
	// Cards is every `[CARD]` block in source order (the 22 major arcana).
	Cards []LukeCard
	// cardsByMonsterID indexes Cards by the notification's P field. The client
	// silently draws nothing for a P that misses, so the caller must send only
	// keys that are present here.
	cardsByMonsterID map[uint32]int
}

func LoadLukeCardSet(source dnfpvf.Source) (*LukeCardSetConfig, error) {
	if source == nil {
		return nil, ErrLukeCardSetSourceRequired
	}
	text, err := source.ReadText(LukeCardSetPVFPath)
	if err != nil {
		return nil, fmt.Errorf("read luke card set PVF %q: %w", LukeCardSetPVFPath, err)
	}
	document, err := dnfpvf.Parse(LukeCardSetPVFPath, text)
	if err != nil {
		return nil, fmt.Errorf("parse luke card set PVF %q: %w", LukeCardSetPVFPath, err)
	}
	return ParseLukeCardSet(document)
}

func ParseLukeCardSet(document *dnfpvf.Document) (*LukeCardSetConfig, error) {
	if document == nil || antonPathKey(document.Path) != antonPathKey(LukeCardSetPVFPath) {
		path := ""
		if document != nil {
			path = document.Path
		}
		return nil, fmt.Errorf("%w: path=%q want=%q", ErrLukeCardSetDocumentInvalid, path, LukeCardSetPVFPath)
	}
	patternBlocks, err := antonFindBlocks(document, "pattern", 0, len(document.Sections))
	if err != nil {
		return nil, fmt.Errorf("%w: pattern blocks: %v", ErrLukeCardSetDocumentInvalid, err)
	}
	if len(patternBlocks) == 0 {
		return nil, fmt.Errorf("%w: no [PATTERN] block", ErrLukeCardSetDocumentInvalid)
	}
	config := &LukeCardSetConfig{
		Patterns:         make([]LukeCardPattern, 0, len(patternBlocks)),
		cardsByMonsterID: make(map[uint32]int, 32),
	}
	// The client's pattern map holds one entry per `[PATTERN]` block and a
	// duplicate key would overwrite rather than extend it, so a repeated
	// (dungeonID, index) pair is a table this server cannot describe faithfully.
	patternKeys := make(map[[2]int64]struct{}, len(patternBlocks))
	for _, block := range patternBlocks {
		pattern, parseErr := parseLukeCardPattern(document, block)
		if parseErr != nil {
			return nil, parseErr
		}
		key := [2]int64{int64(pattern.DungeonID), int64(pattern.Index)}
		if _, duplicate := patternKeys[key]; duplicate {
			return nil, fmt.Errorf("%w: duplicate [PATTERN] dungeon=%d index=%d",
				ErrLukeCardSetDocumentInvalid, pattern.DungeonID, pattern.Index)
		}
		patternKeys[key] = struct{}{}
		config.Patterns = append(config.Patterns, pattern)
	}

	cardBlocks, err := antonFindBlocks(document, "card", 0, len(document.Sections))
	if err != nil {
		return nil, fmt.Errorf("%w: card blocks: %v", ErrLukeCardSetDocumentInvalid, err)
	}
	if len(cardBlocks) == 0 {
		return nil, fmt.Errorf("%w: no [CARD] block", ErrLukeCardSetDocumentInvalid)
	}
	for _, block := range cardBlocks {
		card, parseErr := parseLukeCard(document, block)
		if parseErr != nil {
			return nil, parseErr
		}
		if _, duplicate := config.cardsByMonsterID[card.MonsterID]; duplicate {
			return nil, fmt.Errorf("%w: duplicate [CARD] monster=%d", ErrLukeCardSetDocumentInvalid, card.MonsterID)
		}
		config.cardsByMonsterID[card.MonsterID] = len(config.Cards)
		config.Cards = append(config.Cards, card)
	}
	return config, nil
}

// Pattern returns the pattern of one sanctum and pattern index.
func (c *LukeCardSetConfig) Pattern(dungeonID uint32, index int) (LukeCardPattern, bool) {
	if c == nil {
		return LukeCardPattern{}, false
	}
	for _, pattern := range c.Patterns {
		if pattern.DungeonID == dungeonID && pattern.Index == index {
			return pattern, true
		}
	}
	return LukeCardPattern{}, false
}

// PatternIndices lists one sanctum's pattern indices in ascending order: the
// index positions a caller may pick a pattern from. 5054 -> [0 1],
// 5055 -> [0 1 2 3], 5056 -> [0 1] in the shipped PVF.
func (c *LukeCardSetConfig) PatternIndices(dungeonID uint32) []int {
	if c == nil {
		return nil
	}
	indices := make([]int, 0, 4)
	for _, pattern := range c.Patterns {
		if pattern.DungeonID == dungeonID {
			indices = append(indices, pattern.Index)
		}
	}
	sort.Ints(indices)
	return indices
}

// CardMonsterIDs returns every `[CARD]` key in source order: the only values the
// notification's P field may carry.
func (c *LukeCardSetConfig) CardMonsterIDs() []uint32 {
	if c == nil {
		return nil
	}
	ids := make([]uint32, 0, len(c.Cards))
	for _, card := range c.Cards {
		ids = append(ids, card.MonsterID)
	}
	return ids
}

// HasCard reports whether a monster id is one of the `[CARD]` keys.
func (c *LukeCardSetConfig) HasCard(monsterID uint32) bool {
	if c == nil {
		return false
	}
	if _, ok := c.cardsByMonsterID[monsterID]; ok {
		return true
	}
	// A config assembled without the loader (tests, or a caller mirroring another
	// document) has no index built; fall back to the blocks themselves, the same
	// way Pattern and PatternIndices read their slices directly.
	for _, card := range c.Cards {
		if card.MonsterID == monsterID {
			return true
		}
	}
	return false
}

// parseLukeCardPattern reads one `[PATTERN] <dungeonID> <index>` block: its two
// key values sit on the opening section, and only [IMAGE] / [CARD POS] /
// [TOOLTIP TEXT] may appear inside.
func parseLukeCardPattern(document *dnfpvf.Document, block antonSectionBlock) (LukeCardPattern, error) {
	tokens, err := antonSectionTokens(document, block.open)
	if err != nil {
		return LukeCardPattern{}, fmt.Errorf("%w: pattern key: %v", ErrLukeCardSetDocumentInvalid, err)
	}
	if len(tokens) != 2 {
		return LukeCardPattern{}, fmt.Errorf("%w: pattern key values=%d want=2",
			ErrLukeCardSetDocumentInvalid, len(tokens))
	}
	dungeonID, err := antonPositiveUint32Token(tokens[0], "pattern dungeon")
	if err != nil {
		return LukeCardPattern{}, fmt.Errorf("%w: pattern dungeon: %v", ErrLukeCardSetDocumentInvalid, err)
	}
	index, err := antonIntToken(tokens[1], "pattern index")
	if err != nil {
		return LukeCardPattern{}, fmt.Errorf("%w: pattern index: %v", ErrLukeCardSetDocumentInvalid, err)
	}
	if index < 0 {
		return LukeCardPattern{}, fmt.Errorf("%w: pattern index=%d want_nonnegative", ErrLukeCardSetDocumentInvalid, index)
	}
	pattern := LukeCardPattern{DungeonID: dungeonID, Index: index}
	seen := map[string]bool{}
	for sectionIndex := block.open + 1; sectionIndex < block.close; sectionIndex++ {
		name := antonSectionName(document.Sections[sectionIndex].Name)
		switch name {
		case "image", "card pos", "tooltip text":
		case "/card pos":
			// The closing tag of [CARD POS]; it carries no value.
			continue
		default:
			return LukeCardPattern{}, fmt.Errorf("%w: pattern dungeon=%d index=%d unexpected section=%q",
				ErrLukeCardSetDocumentInvalid, dungeonID, index, document.Sections[sectionIndex].Name)
		}
		if seen[name] {
			return LukeCardPattern{}, fmt.Errorf("%w: pattern dungeon=%d index=%d duplicate section=%q",
				ErrLukeCardSetDocumentInvalid, dungeonID, index, name)
		}
		seen[name] = true
		sectionTokens, tokenErr := antonSectionTokens(document, sectionIndex)
		if tokenErr != nil {
			return LukeCardPattern{}, fmt.Errorf("%w: pattern %s: %v", ErrLukeCardSetDocumentInvalid, name, tokenErr)
		}
		switch name {
		case "image":
			path, indices, imageErr := lukeCardSetImage(sectionTokens, "pattern image")
			if imageErr != nil {
				return LukeCardPattern{}, imageErr
			}
			pattern.ImagePath = path
			pattern.ImageIndices = indices
		case "card pos":
			positions, positionErr := lukeCardSetPositions(sectionTokens)
			if positionErr != nil {
				return LukeCardPattern{}, positionErr
			}
			pattern.CardPositions = positions
		case "tooltip text":
			text, textErr := lukeCardSetTooltip(sectionTokens)
			if textErr != nil {
				return LukeCardPattern{}, textErr
			}
			pattern.Tooltip = text
		}
	}
	if pattern.ImagePath == "" {
		return LukeCardPattern{}, fmt.Errorf("%w: pattern dungeon=%d index=%d has no [IMAGE]",
			ErrLukeCardSetDocumentInvalid, dungeonID, index)
	}
	if len(pattern.CardPositions) == 0 {
		return LukeCardPattern{}, fmt.Errorf("%w: pattern dungeon=%d index=%d has no [CARD POS]",
			ErrLukeCardSetDocumentInvalid, dungeonID, index)
	}
	return pattern, nil
}

// parseLukeCard reads one `[CARD] <monsterID>` block. Its key is a single
// number, which is why the card table cannot be keyed like the 8-byte pattern
// table (see the 1181 spec, §7.3).
func parseLukeCard(document *dnfpvf.Document, block antonSectionBlock) (LukeCard, error) {
	tokens, err := antonSectionTokens(document, block.open)
	if err != nil {
		return LukeCard{}, fmt.Errorf("%w: card key: %v", ErrLukeCardSetDocumentInvalid, err)
	}
	if len(tokens) != 1 {
		return LukeCard{}, fmt.Errorf("%w: card key values=%d want=1", ErrLukeCardSetDocumentInvalid, len(tokens))
	}
	monsterID, err := antonPositiveUint32Token(tokens[0], "card monster")
	if err != nil {
		return LukeCard{}, fmt.Errorf("%w: card monster: %v", ErrLukeCardSetDocumentInvalid, err)
	}
	card := LukeCard{MonsterID: monsterID}
	for sectionIndex := block.open + 1; sectionIndex < block.close; sectionIndex++ {
		name := antonSectionName(document.Sections[sectionIndex].Name)
		if name != "image" && name != "tooltip text" {
			return LukeCard{}, fmt.Errorf("%w: card monster=%d unexpected section=%q",
				ErrLukeCardSetDocumentInvalid, monsterID, document.Sections[sectionIndex].Name)
		}
		sectionTokens, tokenErr := antonSectionTokens(document, sectionIndex)
		if tokenErr != nil {
			return LukeCard{}, fmt.Errorf("%w: card %s: %v", ErrLukeCardSetDocumentInvalid, name, tokenErr)
		}
		if name == "image" {
			if card.ImagePath != "" {
				return LukeCard{}, fmt.Errorf("%w: card monster=%d duplicate [IMAGE]", ErrLukeCardSetDocumentInvalid, monsterID)
			}
			path, indices, imageErr := lukeCardSetImage(sectionTokens, "card image")
			if imageErr != nil {
				return LukeCard{}, imageErr
			}
			// A card names exactly one frame; the two-number form belongs to the
			// patterns, whose [IMAGE] holds the buff icon pair.
			if len(indices) != 1 {
				return LukeCard{}, fmt.Errorf("%w: card monster=%d image indices=%d want=1",
					ErrLukeCardSetDocumentInvalid, monsterID, len(indices))
			}
			card.ImagePath = path
			card.ImageIndex = indices[0]
			continue
		}
		text, textErr := lukeCardSetTooltip(sectionTokens)
		if textErr != nil {
			return LukeCard{}, textErr
		}
		card.Tooltip = text
	}
	if card.ImagePath == "" {
		return LukeCard{}, fmt.Errorf("%w: card monster=%d has no [IMAGE]", ErrLukeCardSetDocumentInvalid, monsterID)
	}
	return card, nil
}

// lukeCardSetImage reads `[IMAGE]`: one path token followed by a VARIABLE number
// of image indices. `5054 *` names one index, `5055 *` / `5056 *` name two
// (`2 3`, `4 5`, ...), so the indices must not be read as a fixed pair.
func lukeCardSetImage(tokens []dnfpvf.Token, field string) (string, []int, error) {
	if len(tokens) < 2 {
		return "", nil, fmt.Errorf("%w: %s values=%d want>=2", ErrLukeCardSetDocumentInvalid, field, len(tokens))
	}
	if tokens[0].Kind != dnfpvf.TokenString {
		return "", nil, fmt.Errorf("%w: %s path kind=%q want=string",
			ErrLukeCardSetDocumentInvalid, field, tokens[0].Kind)
	}
	path := tokens[0].Value
	if path == "" {
		return "", nil, fmt.Errorf("%w: %s empty path", ErrLukeCardSetDocumentInvalid, field)
	}
	indices := make([]int, 0, len(tokens)-1)
	for _, token := range tokens[1:] {
		index, err := antonIntToken(token, field)
		if err != nil {
			return "", nil, fmt.Errorf("%w: %s: %v", ErrLukeCardSetDocumentInvalid, field, err)
		}
		if index < 0 {
			return "", nil, fmt.Errorf("%w: %s index=%d want_nonnegative", ErrLukeCardSetDocumentInvalid, field, index)
		}
		indices = append(indices, index)
	}
	return path, indices, nil
}

// lukeCardSetPositions reads `[CARD POS]`: 2n values, n (x, y) pairs. Half-step
// coordinates arrive as float tokens, whole ones as int tokens, so both kinds
// are accepted and both are required to be non-negative.
func lukeCardSetPositions(tokens []dnfpvf.Token) ([]LukeCardPosition, error) {
	if len(tokens) == 0 {
		return nil, fmt.Errorf("%w: empty [CARD POS]", ErrLukeCardSetDocumentInvalid)
	}
	if len(tokens)%2 != 0 {
		return nil, fmt.Errorf("%w: [CARD POS] values=%d want_even", ErrLukeCardSetDocumentInvalid, len(tokens))
	}
	positions := make([]LukeCardPosition, 0, len(tokens)/2)
	for index := 0; index < len(tokens); index += 2 {
		x, err := lukeCardSetCoordinate(tokens[index])
		if err != nil {
			return nil, err
		}
		y, err := lukeCardSetCoordinate(tokens[index+1])
		if err != nil {
			return nil, err
		}
		positions = append(positions, LukeCardPosition{X: x, Y: y})
	}
	return positions, nil
}

func lukeCardSetCoordinate(token dnfpvf.Token) (float64, error) {
	var value float64
	switch token.Kind {
	case dnfpvf.TokenInt:
		value = float64(token.Int)
	case dnfpvf.TokenFloat:
		value = token.Float
	default:
		return 0, fmt.Errorf("%w: [CARD POS] value=%q kind=%q want=number",
			ErrLukeCardSetDocumentInvalid, token.Raw, token.Kind)
	}
	if value < 0 {
		return 0, fmt.Errorf("%w: [CARD POS] value=%v want_nonnegative", ErrLukeCardSetDocumentInvalid, value)
	}
	return value, nil
}

// lukeCardSetTooltip reads `[TOOLTIP TEXT]`, which is exactly one quoted string.
func lukeCardSetTooltip(tokens []dnfpvf.Token) (string, error) {
	if len(tokens) != 1 {
		return "", fmt.Errorf("%w: [TOOLTIP TEXT] values=%d want=1", ErrLukeCardSetDocumentInvalid, len(tokens))
	}
	if tokens[0].Kind != dnfpvf.TokenString {
		return "", fmt.Errorf("%w: [TOOLTIP TEXT] kind=%q want=string", ErrLukeCardSetDocumentInvalid, tokens[0].Kind)
	}
	return tokens[0].Value, nil
}
