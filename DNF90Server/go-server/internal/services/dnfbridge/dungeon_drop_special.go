package dnfbridge

import (
	"fmt"
	"math"
	"strings"
	"sync"

	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
)

const (
	currentDungeonIndependentDropPath        = "Etc/Independent_Drop.etc"
	currentDungeonWorldDropPath              = "Etc/WorldDrop.etc"
	currentDungeonIndependentDropDenominator = 1000000
	currentDungeonWorldDropDenominator       = 100000
	currentDungeonWorldDropMaxLevel          = 199
)

type currentDungeonSpecialDropWeightedItem struct {
	ItemID uint32
	Weight uint32
}

// Independent_Drop.etc record types (first PVF column). The second column is
// an actor code whose namespace depends on this type: type 0 rows are keyed by
// monster code, type 1 rows by APC (AI character) code. 86JPPP keys its
// catalog by (DefinitionType, ActorCode) and S4A21 skips type 1 outright; both
// agree that a type-1 row never applies to a monster kill. The numeric spaces
// overlap (APC 420/421/430 vs. monsters Spider/SpiderPoison/YoungSpider), so
// merging them by code made 蜘蛛洞穴 spiders drop the Hell-party invitation,
// challenge letter and 史诗灵魂碎片 that belong to APC 420/421.
const (
	currentDungeonIndependentDropTypeMonster = 0
	currentDungeonIndependentDropTypeAPC     = 1
)

type currentDungeonIndependentDropEntry struct {
	// EntryType is the record's first PVF column. Type 0 caps the boosted
	// probability at the denominator and rolls once per attempt; type 1 keeps
	// probabilities above 100%: `divmod(prob, denominator)` grants the whole
	// multiplier per attempt as guaranteed drops and rolls only the residual
	// (usdof_v54 game/drop/independent_drop.py `generate`, proven against
	// _disasm/game/drop/independent_drop.dis.txt lines 250-269). Type 1 rows
	// live in IndependentByAPC and are never consulted for a monster kill.
	EntryType   int
	ItemID      uint32
	Probability [5]int
	Attempts    [5]int
	LevelMin    int
	LevelMax    int
	Difficulty  int
	Items       []currentDungeonSpecialDropWeightedItem
	TotalWeight uint32
}

type currentDungeonWorldDropLevel struct {
	Items       []currentDungeonSpecialDropWeightedItem
	TotalWeight uint32
}

// currentDungeonIndependentDropTables is the parsed Independent_Drop.etc split
// by record type. The two maps share the int64 code space but not its meaning.
type currentDungeonIndependentDropTables struct {
	ByMonster map[int64][]currentDungeonIndependentDropEntry
	ByAPC     map[int64][]currentDungeonIndependentDropEntry
}

type currentDungeonSpecialDropCatalog struct {
	// IndependentByMonster holds type-0 rows keyed by monster code. This is
	// the only table a monster death consults.
	IndependentByMonster map[int64][]currentDungeonIndependentDropEntry
	// IndependentByAPC holds type-1 rows keyed by APC code. No APC death
	// currently rolls independent drops; the rows are kept parsed so that
	// path can be added without re-reading the PVF semantics.
	IndependentByAPC map[int64][]currentDungeonIndependentDropEntry
	WorldByLevel     map[int]currentDungeonWorldDropLevel
	SkippedListFlag2 int
}

type currentDungeonSpecialDropCacheEntry struct {
	catalog *currentDungeonSpecialDropCatalog
	err     error
}

var currentDungeonSpecialDropsByMonsterCatalog sync.Map

func currentDungeonSpecialDrops(catalog *pvfDungeonMonsterCatalog) (*currentDungeonSpecialDropCatalog, error) {
	if catalog == nil || catalog.source == nil {
		return nil, errDungeonDropSourceRequired
	}
	if cached, found := currentDungeonSpecialDropsByMonsterCatalog.Load(catalog); found {
		entry := cached.(currentDungeonSpecialDropCacheEntry)
		return entry.catalog, entry.err
	}
	loaded, err := loadCurrentDungeonSpecialDrops(catalog.source)
	entry := currentDungeonSpecialDropCacheEntry{catalog: loaded, err: err}
	actual, _ := currentDungeonSpecialDropsByMonsterCatalog.LoadOrStore(catalog, entry)
	entry = actual.(currentDungeonSpecialDropCacheEntry)
	return entry.catalog, entry.err
}

func loadCurrentDungeonSpecialDrops(source dnfpvf.Source) (*currentDungeonSpecialDropCatalog, error) {
	if source == nil {
		return nil, errDungeonDropSourceRequired
	}
	catalog := &currentDungeonSpecialDropCatalog{
		IndependentByMonster: make(map[int64][]currentDungeonIndependentDropEntry),
		IndependentByAPC:     make(map[int64][]currentDungeonIndependentDropEntry),
		WorldByLevel:         make(map[int]currentDungeonWorldDropLevel),
	}
	if text, err := source.ReadText(currentDungeonIndependentDropPath); err == nil && strings.TrimSpace(text) != "" {
		tables, skipped, parseErr := parseCurrentDungeonIndependentDrops(text)
		if parseErr != nil {
			return nil, parseErr
		}
		catalog.IndependentByMonster = tables.ByMonster
		catalog.IndependentByAPC = tables.ByAPC
		catalog.SkippedListFlag2 = skipped
	}
	if text, err := source.ReadText(currentDungeonWorldDropPath); err == nil && strings.TrimSpace(text) != "" {
		levels, parseErr := parseCurrentDungeonWorldDrops(text)
		if parseErr != nil {
			return nil, parseErr
		}
		catalog.WorldByLevel = levels
	}
	return catalog, nil
}

func parseCurrentDungeonIndependentDrops(text string) (currentDungeonIndependentDropTables, int, error) {
	result := currentDungeonIndependentDropTables{
		ByMonster: make(map[int64][]currentDungeonIndependentDropEntry),
		ByAPC:     make(map[int64][]currentDungeonIndependentDropEntry),
	}
	document, err := dnfpvf.Parse(currentDungeonIndependentDropPath, text)
	if err != nil {
		return result, 0, err
	}
	tokens := document.Tokens
	index := currentDungeonDropSectionToken(tokens, "independent drop")
	if index < 0 {
		return result, 0, fmt.Errorf("%s: [independent drop] section missing", currentDungeonIndependentDropPath)
	}
	index++
	skippedListFlag2 := 0
	for index < len(tokens) {
		if currentDungeonDropTokenSection(tokens[index], "/independent drop") {
			break
		}
		if tokens[index].Kind == dnfpvf.TokenSection {
			return result, 0, fmt.Errorf("%s: unexpected section %q at line %d", currentDungeonIndependentDropPath, tokens[index].Value, tokens[index].Line)
		}
		if index+17 > len(tokens) {
			return result, 0, fmt.Errorf("%s: truncated independent-drop record at token %d", currentDungeonIndependentDropPath, index)
		}
		var values [17]int64
		for field := range values {
			token := tokens[index+field]
			if token.Kind != dnfpvf.TokenInt {
				return result, 0, fmt.Errorf("%s: record field %d at line %d is %s", currentDungeonIndependentDropPath, field, token.Line, token.Kind)
			}
			values[field] = token.Int
		}
		index += len(values)

		listFlag := values[16]
		var weighted []currentDungeonSpecialDropWeightedItem
		var totalWeight uint32
		if listFlag != 0 {
			if index >= len(tokens) || !currentDungeonDropTokenSection(tokens[index], "list") {
				return result, 0, fmt.Errorf("%s: list flag %d has no [list] after actor %d", currentDungeonIndependentDropPath, listFlag, values[1])
			}
			index++
			listValues := make([]int64, 0, 8)
			for index < len(tokens) && !currentDungeonDropTokenSection(tokens[index], "/list") {
				if tokens[index].Kind != dnfpvf.TokenInt {
					return result, 0, fmt.Errorf("%s: actor %d list token at line %d is %s", currentDungeonIndependentDropPath, values[1], tokens[index].Line, tokens[index].Kind)
				}
				listValues = append(listValues, tokens[index].Int)
				index++
			}
			if index >= len(tokens) {
				return result, 0, fmt.Errorf("%s: actor %d list is not closed", currentDungeonIndependentDropPath, values[1])
			}
			index++
			if listFlag == 1 {
				if len(listValues)%2 != 0 {
					return result, 0, fmt.Errorf("%s: actor %d inline list has odd value count %d", currentDungeonIndependentDropPath, values[1], len(listValues))
				}
				var listErr error
				weighted, totalWeight, listErr = currentDungeonSpecialDropWeightedItems(listValues)
				if listErr != nil {
					return result, 0, fmt.Errorf("%s: actor %d inline list: %w", currentDungeonIndependentDropPath, values[1], listErr)
				}
			} else if listFlag == 2 {
				skippedListFlag2++
				continue
			} else {
				return result, 0, fmt.Errorf("%s: actor %d unsupported list flag %d", currentDungeonIndependentDropPath, values[1], listFlag)
			}
		}
		var destination map[int64][]currentDungeonIndependentDropEntry
		switch values[0] {
		case currentDungeonIndependentDropTypeMonster:
			destination = result.ByMonster
		case currentDungeonIndependentDropTypeAPC:
			destination = result.ByAPC
		default:
			return result, 0, fmt.Errorf("%s: actor %d unsupported entry type %d", currentDungeonIndependentDropPath, values[1], values[0])
		}
		actorCode := values[1]
		if actorCode <= 0 {
			continue
		}
		entry := currentDungeonIndependentDropEntry{
			EntryType:   int(values[0]),
			LevelMin:    int(values[13]),
			LevelMax:    int(values[14]),
			Difficulty:  int(values[15]),
			Items:       weighted,
			TotalWeight: totalWeight,
		}
		if values[2] > 0 && values[2] <= math.MaxUint32 {
			entry.ItemID = uint32(values[2])
		}
		for difficulty := 0; difficulty < 5; difficulty++ {
			probability := values[3+difficulty]
			attempts := values[8+difficulty]
			if probability < 0 || probability > math.MaxInt32 || attempts < 0 || attempts > math.MaxUint16 {
				return result, 0, fmt.Errorf("%s: actor %d difficulty %d probability=%d attempts=%d", currentDungeonIndependentDropPath, actorCode, difficulty, probability, attempts)
			}
			entry.Probability[difficulty] = int(probability)
			entry.Attempts[difficulty] = int(attempts)
		}
		destination[actorCode] = append(destination[actorCode], entry)
	}
	return result, skippedListFlag2, nil
}

func parseCurrentDungeonWorldDrops(text string) (map[int]currentDungeonWorldDropLevel, error) {
	document, err := dnfpvf.Parse(currentDungeonWorldDropPath, text)
	if err != nil {
		return nil, err
	}
	tokens, found := document.Section("world drop")
	if !found {
		return nil, fmt.Errorf("%s: [world drop] section missing", currentDungeonWorldDropPath)
	}
	values := make([]int64, 0, len(tokens))
	for _, token := range tokens {
		if token.Kind != dnfpvf.TokenInt {
			return nil, fmt.Errorf("%s: token at line %d is %s", currentDungeonWorldDropPath, token.Line, token.Kind)
		}
		values = append(values, token.Int)
	}
	result := make(map[int]currentDungeonWorldDropLevel)
	for index := 0; index < len(values); {
		if index+2 > len(values) {
			return nil, fmt.Errorf("%s: truncated level header at value %d", currentDungeonWorldDropPath, index)
		}
		level := values[index]
		index += 2 // The second PVF column is preserved as opaque and not consumed by S4A12.
		pairs := make([]int64, 0, 16)
		terminated := false
		for index < len(values) {
			itemID := values[index]
			index++
			if itemID == -1 {
				terminated = true
				break
			}
			if index >= len(values) {
				return nil, fmt.Errorf("%s: level %d item %d has no weight", currentDungeonWorldDropPath, level, itemID)
			}
			pairs = append(pairs, itemID, values[index])
			index++
		}
		if !terminated {
			return nil, fmt.Errorf("%s: level %d has no -1 terminator", currentDungeonWorldDropPath, level)
		}
		if level < 1 || level > currentDungeonWorldDropMaxLevel {
			continue
		}
		items, totalWeight, listErr := currentDungeonSpecialDropWeightedItems(pairs)
		if listErr != nil {
			return nil, fmt.Errorf("%s: level %d: %w", currentDungeonWorldDropPath, level, listErr)
		}
		if totalWeight > 0 {
			result[int(level)] = currentDungeonWorldDropLevel{Items: items, TotalWeight: totalWeight}
		}
	}
	return result, nil
}

func currentDungeonSpecialDropWeightedItems(values []int64) ([]currentDungeonSpecialDropWeightedItem, uint32, error) {
	items := make([]currentDungeonSpecialDropWeightedItem, 0, len(values)/2)
	var total uint64
	for index := 0; index+1 < len(values); index += 2 {
		itemID, weight := values[index], values[index+1]
		if itemID <= 0 || itemID > math.MaxUint32 || weight <= 0 || weight > math.MaxUint32 {
			continue
		}
		total += uint64(weight)
		if total > math.MaxUint32 {
			return nil, 0, fmt.Errorf("weighted list total %d exceeds u32", total)
		}
		items = append(items, currentDungeonSpecialDropWeightedItem{ItemID: uint32(itemID), Weight: uint32(weight)})
	}
	return items, uint32(total), nil
}

// currentDungeonIndependentDropItems rolls the type-0 rows of one monster
// code. Type-1 (APC) rows sharing the same number are deliberately invisible
// here; see currentDungeonIndependentAPCDropItems.
func currentDungeonIndependentDropItems(
	catalog *currentDungeonSpecialDropCatalog,
	monsterID int64,
	difficulty int,
	dungeonLevel int,
	bonusPercent int64,
	rng *currentDungeonDropLCG,
) []uint32 {
	if catalog == nil {
		return nil
	}
	return rollCurrentDungeonIndependentDropEntries(catalog.IndependentByMonster[monsterID], difficulty, dungeonLevel, bonusPercent, rng)
}

// currentDungeonIndependentAPCDropItems rolls the type-1 rows of one APC
// code. No runtime death path calls it yet; it exists so the APC namespace has
// an owner and the type-1 probability semantics stay covered by tests.
func currentDungeonIndependentAPCDropItems(
	catalog *currentDungeonSpecialDropCatalog,
	apcCode int64,
	difficulty int,
	dungeonLevel int,
	bonusPercent int64,
	rng *currentDungeonDropLCG,
) []uint32 {
	if catalog == nil {
		return nil
	}
	return rollCurrentDungeonIndependentDropEntries(catalog.IndependentByAPC[apcCode], difficulty, dungeonLevel, bonusPercent, rng)
}

func rollCurrentDungeonIndependentDropEntries(
	entries []currentDungeonIndependentDropEntry,
	difficulty int,
	dungeonLevel int,
	bonusPercent int64,
	rng *currentDungeonDropLCG,
) []uint32 {
	if rng == nil {
		return nil
	}
	if difficulty < 0 {
		difficulty = 0
	} else if difficulty > 4 {
		difficulty = 4
	}
	result := make([]uint32, 0)
	for _, entry := range entries {
		if entry.LevelMin > 0 && entry.LevelMax > 0 && (dungeonLevel < entry.LevelMin || dungeonLevel > entry.LevelMax) {
			continue
		}
		if entry.Difficulty >= 0 && entry.Difficulty != difficulty {
			continue
		}
		attempts := entry.Attempts[difficulty]
		if entry.Probability[difficulty] <= 0 || attempts <= 0 || (entry.ItemID == 0 && entry.TotalWeight == 0) {
			continue
		}
		if entry.EntryType == 0 {
			probability := currentDungeonIndependentDropProbability(entry.Probability[difficulty], bonusPercent)
			for attempt := 0; attempt < attempts; attempt++ {
				if probability <= rng.Next(currentDungeonIndependentDropDenominator) {
					continue
				}
				if entry.TotalWeight > 0 {
					if itemID, selected := currentDungeonSpecialDropWeightedSelect(entry.Items, entry.TotalWeight, rng); selected {
						result = append(result, itemID)
					}
				} else if entry.ItemID > 0 {
					result = append(result, entry.ItemID)
				}
			}
			continue
		}
		// Type 1 keeps probabilities above the denominator: the whole
		// multiplier is granted per attempt and only the residual is rolled,
		// with the reference's floor-rounded, uncapped bonus scaling.
		probability := int64(entry.Probability[difficulty])
		if bonusPercent > 0 {
			probability = probability * (100 + bonusPercent) / 100
		}
		wholeMultiplier := probability / currentDungeonIndependentDropDenominator
		residual := probability % currentDungeonIndependentDropDenominator
		dropCount := wholeMultiplier * int64(attempts)
		if residual != 0 {
			for attempt := 0; attempt < attempts; attempt++ {
				if residual > int64(rng.Next(currentDungeonIndependentDropDenominator)) {
					dropCount++
				}
			}
		}
		for produced := int64(0); produced < dropCount; produced++ {
			if entry.TotalWeight > 0 {
				if itemID, selected := currentDungeonSpecialDropWeightedSelect(entry.Items, entry.TotalWeight, rng); selected {
					result = append(result, itemID)
				}
			} else if entry.ItemID > 0 {
				result = append(result, entry.ItemID)
			}
		}
	}
	return result
}

// currentDungeonIndependentDropProbability applies the growth-contract bonus
// to a type-0 record: `prob * (100 + bonus) // 100` with floor rounding, then
// capped at the denominator (usdof_v54 game/drop/independent_drop.py
// `generate`; type 1 applies the same floor scaling without the cap).
func currentDungeonIndependentDropProbability(base int, bonusPercent int64) int {
	if base <= 0 || bonusPercent <= 0 {
		return base
	}
	if bonusPercent >= 10000 || int64(base) > math.MaxInt64/(100+bonusPercent) {
		return currentDungeonIndependentDropDenominator
	}
	boosted := int64(base) * (100 + bonusPercent) / 100
	if boosted > currentDungeonIndependentDropDenominator {
		return currentDungeonIndependentDropDenominator
	}
	return int(boosted)
}

func currentDungeonWorldDropItem(
	catalog *currentDungeonSpecialDropCatalog,
	monsterLevel int,
	rng *currentDungeonDropLCG,
) (uint32, bool) {
	if catalog == nil || rng == nil || monsterLevel < 1 || monsterLevel > currentDungeonWorldDropMaxLevel {
		return 0, false
	}
	level, found := catalog.WorldByLevel[monsterLevel]
	if !found || level.TotalWeight == 0 || len(level.Items) == 0 {
		return 0, false
	}
	if uint64(level.TotalWeight) <= uint64(rng.Next(currentDungeonWorldDropDenominator)) {
		return 0, false
	}
	return currentDungeonSpecialDropWeightedSelect(level.Items, level.TotalWeight, rng)
}

func currentDungeonSpecialDropWeightedSelect(
	items []currentDungeonSpecialDropWeightedItem,
	totalWeight uint32,
	rng *currentDungeonDropLCG,
) (uint32, bool) {
	if rng == nil || totalWeight == 0 || len(items) == 0 {
		return 0, false
	}
	want := rng.NextUint32() % totalWeight
	var cumulative uint32
	for _, item := range items {
		if item.ItemID == 0 || item.Weight == 0 || math.MaxUint32-cumulative < item.Weight {
			continue
		}
		cumulative += item.Weight
		if want < cumulative {
			return item.ItemID, true
		}
	}
	return 0, false
}

func currentDungeonDropSectionToken(tokens []dnfpvf.Token, name string) int {
	for index, token := range tokens {
		if currentDungeonDropTokenSection(token, name) {
			return index
		}
	}
	return -1
}

func currentDungeonDropTokenSection(token dnfpvf.Token, name string) bool {
	return token.Kind == dnfpvf.TokenSection && strings.EqualFold(strings.TrimSpace(token.Value), name)
}
