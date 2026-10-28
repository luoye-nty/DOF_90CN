package progression

import (
	"errors"
	"fmt"
	"path"
	"strings"
	"sync"

	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
)

const DungeonDefinitionListPath = "dungeon/dungeon.lst"

var (
	ErrDungeonLocalSourceRequired = errors.New("dnf local dungeon experience PVF source is required")
	ErrDungeonLocalSectionMissing = errors.New("dnf local dungeon experience PVF section is missing")
	ErrDungeonLocalSectionShape   = errors.New("dnf local dungeon experience PVF section shape is invalid")
	ErrDungeonLocalLookup         = errors.New("dnf local dungeon experience PVF lookup is absent")
)

// DungeonLocalExperienceRow preserves one exact [increase exp rate] pair from
// a dungeon definition. Base and Bonus remain source columns; no multiplier
// or award amount is derived from them here.
type DungeonLocalExperienceRow struct {
	DungeonID int64
	Path      string
	Base      int64
	Bonus     int64
	Present   bool
}

type DungeonLocalExperienceSnapshot struct {
	DungeonDefinitions int
	LoadedDefinitions  int
}

// DungeonLocalExperienceSources resolves dungeon/dungeon.lst paths and lazily
// reads each dungeon's local experience section. Lazy reads keep normal PVF
// startup bounded while still exposing the exact source row for diagnostics.
type DungeonLocalExperienceSources struct {
	source dnfpvf.Source
	paths  map[int64]string

	mu    sync.RWMutex
	cache map[int64]dungeonLocalExperienceCacheEntry
}

type dungeonLocalExperienceCacheEntry struct {
	row DungeonLocalExperienceRow
	err error
}

func LoadDungeonLocalExperienceSources(source dnfpvf.Source) (*DungeonLocalExperienceSources, error) {
	if source == nil {
		return nil, ErrDungeonLocalSourceRequired
	}
	document, err := readProgressionDocument(source, DungeonDefinitionListPath)
	if err != nil {
		return nil, err
	}
	paths, err := parseDungeonDefinitionPaths(document)
	if err != nil {
		return nil, err
	}
	return &DungeonLocalExperienceSources{
		source: source,
		paths:  paths,
		cache:  make(map[int64]dungeonLocalExperienceCacheEntry),
	}, nil
}

func readProgressionDocument(source dnfpvf.Source, relativePath string) (*dnfpvf.Document, error) {
	text, err := source.ReadText(relativePath)
	if err != nil {
		return nil, fmt.Errorf("read progression PVF %q: %w", relativePath, err)
	}
	document, err := dnfpvf.Parse(relativePath, text)
	if err != nil {
		return nil, fmt.Errorf("parse progression PVF %q: %w", relativePath, err)
	}
	return document, nil
}

func parseDungeonDefinitionPaths(document *dnfpvf.Document) (map[int64]string, error) {
	if document == nil || len(document.Tokens) == 0 {
		return nil, fmt.Errorf("%w: path=%s empty", ErrDungeonLocalSectionShape, DungeonDefinitionListPath)
	}
	if len(document.Tokens)%2 != 0 {
		return nil, fmt.Errorf("%w: path=%s values=%d row_width=2", ErrDungeonLocalSectionShape, DungeonDefinitionListPath, len(document.Tokens))
	}
	paths := make(map[int64]string, len(document.Tokens)/2)
	for offset := 0; offset < len(document.Tokens); offset += 2 {
		idToken, pathToken := document.Tokens[offset], document.Tokens[offset+1]
		if idToken.Kind != dnfpvf.TokenInt || idToken.Int <= 0 ||
			(pathToken.Kind != dnfpvf.TokenString && pathToken.Kind != dnfpvf.TokenIdent) {
			return nil, fmt.Errorf("%w: path=%s row=%d id_kind=%s path_kind=%s", ErrDungeonLocalSectionShape,
				DungeonDefinitionListPath, offset/2, idToken.Kind, pathToken.Kind)
		}
		relative, err := normalizeDungeonDefinitionPath(pathToken.Value)
		if err != nil {
			return nil, fmt.Errorf("%w: path=%s row=%d: %v", ErrDungeonLocalSectionShape, DungeonDefinitionListPath, offset/2, err)
		}
		if _, duplicate := paths[idToken.Int]; duplicate {
			return nil, fmt.Errorf("%w: path=%s duplicate_dungeon=%d", ErrDungeonLocalSectionShape, DungeonDefinitionListPath, idToken.Int)
		}
		paths[idToken.Int] = relative
	}
	return paths, nil
}

func normalizeDungeonDefinitionPath(value string) (string, error) {
	value = strings.TrimSpace(strings.Trim(value, "`"))
	value = strings.ReplaceAll(value, `\`, "/")
	if value == "" {
		return "", errors.New("dungeon definition path is empty")
	}
	if !strings.HasPrefix(strings.ToLower(value), "dungeon/") {
		value = path.Join("dungeon", value)
	}
	value = path.Clean(value)
	if value == "." || value == ".." || strings.HasPrefix(value, "../") {
		return "", fmt.Errorf("dungeon definition path %q escapes dungeon root", value)
	}
	return value, nil
}

// Rule returns the source row for dungeonID. The second result reports that
// dungeon/dungeon.lst contained the ID; Present reports whether the definition
// itself contained [increase exp rate].
func (sources *DungeonLocalExperienceSources) Rule(dungeonID int64) (DungeonLocalExperienceRow, bool, error) {
	if sources == nil {
		return DungeonLocalExperienceRow{}, false, ErrDungeonLocalSourceRequired
	}
	if dungeonID <= 0 {
		return DungeonLocalExperienceRow{}, false, fmt.Errorf("%w: dungeon_id=%d", ErrDungeonLocalLookup, dungeonID)
	}
	sources.mu.RLock()
	pathValue, pathFound := sources.paths[dungeonID]
	cached, cachedFound := sources.cache[dungeonID]
	sources.mu.RUnlock()
	if !pathFound {
		return DungeonLocalExperienceRow{}, false, nil
	}
	if cachedFound {
		return cached.row, true, cached.err
	}
	row, err := sources.loadRule(dungeonID, pathValue)
	sources.mu.Lock()
	if existing, exists := sources.cache[dungeonID]; exists {
		cached = existing
	} else {
		cached = dungeonLocalExperienceCacheEntry{row: row, err: err}
		sources.cache[dungeonID] = cached
	}
	sources.mu.Unlock()
	return cached.row, true, cached.err
}

func (sources *DungeonLocalExperienceSources) loadRule(dungeonID int64, relativePath string) (DungeonLocalExperienceRow, error) {
	document, err := readProgressionDocument(sources.source, relativePath)
	if err != nil {
		return DungeonLocalExperienceRow{}, err
	}
	row := DungeonLocalExperienceRow{DungeonID: dungeonID, Path: relativePath}
	tokens, found := document.Section("increase exp rate")
	if !found {
		return row, nil
	}
	if len(tokens) != 2 || tokens[0].Kind != dnfpvf.TokenInt || tokens[1].Kind != dnfpvf.TokenInt {
		return DungeonLocalExperienceRow{}, fmt.Errorf("%w: path=%s section=increase exp rate values=%d", ErrDungeonLocalSectionShape, relativePath, len(tokens))
	}
	base, bonus := tokens[0].Int, tokens[1].Int
	if base <= 0 || bonus < -base {
		return DungeonLocalExperienceRow{}, fmt.Errorf("%w: path=%s section=increase exp rate base=%d bonus=%d", ErrDungeonLocalSectionShape, relativePath, base, bonus)
	}
	row.Base, row.Bonus, row.Present = base, bonus, true
	return row, nil
}

func (sources *DungeonLocalExperienceSources) Snapshot() DungeonLocalExperienceSnapshot {
	if sources == nil {
		return DungeonLocalExperienceSnapshot{}
	}
	sources.mu.RLock()
	defer sources.mu.RUnlock()
	return DungeonLocalExperienceSnapshot{
		DungeonDefinitions: len(sources.paths),
		LoadedDefinitions:  len(sources.cache),
	}
}
