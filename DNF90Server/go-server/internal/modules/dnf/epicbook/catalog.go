package epicbook

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"

	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
)

const PieceInfoPath = "etc/epicpieceinfo.etc"

var (
	ErrSourceRequired = errors.New("dnf epic book PVF source is required")
	ErrCatalogInvalid = errors.New("dnf epic book catalog is invalid")
)

type TextSource interface {
	ReadText(string) (string, error)
}

type Material struct {
	ItemID uint32
	Count  uint32
}

type Recipe struct {
	OutputItemID  uint32
	FragmentID    uint32
	FragmentCount uint32
	Materials     []Material
}

type Entry struct {
	EquipmentID uint32
	FragmentID  uint32
	Recipe      Recipe
}

type Catalog struct {
	byEquipment map[uint32]Entry
	byFragment  map[uint32]Entry
}

func Parse(text string) (*Catalog, error) {
	document, err := dnfpvf.Parse(PieceInfoPath, text)
	if err != nil {
		return nil, fmt.Errorf("%w: parse %s: %v", ErrCatalogInvalid, PieceInfoPath, err)
	}
	return ParseDocument(document)
}

func ParseDocument(document *dnfpvf.Document) (*Catalog, error) {
	if document == nil {
		return nil, fmt.Errorf("%w: document is nil", ErrCatalogInvalid)
	}
	mapping, err := parsePieceMapping(document)
	if err != nil {
		return nil, err
	}
	recipes, err := parseRecipes(document)
	if err != nil {
		return nil, err
	}
	if len(mapping) == 0 || len(recipes) == 0 || len(mapping) != len(recipes) {
		return nil, fmt.Errorf("%w: mappings=%d recipes=%d", ErrCatalogInvalid, len(mapping), len(recipes))
	}

	catalog := &Catalog{
		byEquipment: make(map[uint32]Entry, len(mapping)),
		byFragment:  make(map[uint32]Entry, len(mapping)),
	}
	for equipmentID, fragmentID := range mapping {
		recipe, ok := recipes[equipmentID]
		if !ok {
			return nil, fmt.Errorf("%w: equipment=%d has no recipe", ErrCatalogInvalid, equipmentID)
		}
		if recipe.FragmentID != fragmentID {
			return nil, fmt.Errorf("%w: equipment=%d fragment=%d recipe_fragment=%d", ErrCatalogInvalid, equipmentID, fragmentID, recipe.FragmentID)
		}
		entry := Entry{EquipmentID: equipmentID, FragmentID: fragmentID, Recipe: recipe}
		catalog.byEquipment[equipmentID] = entry
		if previous, exists := catalog.byFragment[fragmentID]; exists {
			return nil, fmt.Errorf("%w: fragment=%d maps to equipment=%d and equipment=%d", ErrCatalogInvalid, fragmentID, previous.EquipmentID, equipmentID)
		}
		catalog.byFragment[fragmentID] = entry
	}
	for equipmentID := range recipes {
		if _, ok := mapping[equipmentID]; !ok {
			return nil, fmt.Errorf("%w: recipe equipment=%d has no piece mapping", ErrCatalogInvalid, equipmentID)
		}
	}
	return catalog, nil
}

func (c *Catalog) EntryByEquipment(equipmentID uint32) (Entry, bool) {
	if c == nil {
		return Entry{}, false
	}
	entry, ok := c.byEquipment[equipmentID]
	return cloneEntry(entry), ok
}

func (c *Catalog) EntryByFragment(fragmentID uint32) (Entry, bool) {
	if c == nil {
		return Entry{}, false
	}
	entry, ok := c.byFragment[fragmentID]
	return cloneEntry(entry), ok
}

func (c *Catalog) Entries() []Entry {
	if c == nil {
		return nil
	}
	ids := make([]uint32, 0, len(c.byEquipment))
	for equipmentID := range c.byEquipment {
		ids = append(ids, equipmentID)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	entries := make([]Entry, 0, len(ids))
	for _, equipmentID := range ids {
		entries = append(entries, cloneEntry(c.byEquipment[equipmentID]))
	}
	return entries
}

func (c *Catalog) Len() int {
	if c == nil {
		return 0
	}
	return len(c.byEquipment)
}

func parsePieceMapping(document *dnfpvf.Document) (map[uint32]uint32, error) {
	tokens, ok := uniqueSectionTokens(document, "piece list")
	if !ok {
		return nil, fmt.Errorf("%w: missing [piece list]", ErrCatalogInvalid)
	}
	values, err := positiveUintTokens(tokens, "piece list")
	if err != nil {
		return nil, err
	}
	if len(values) == 0 || len(values)%2 != 0 {
		return nil, fmt.Errorf("%w: [piece list] value_count=%d", ErrCatalogInvalid, len(values))
	}
	mapping := make(map[uint32]uint32, len(values)/2)
	fragments := make(map[uint32]uint32, len(values)/2)
	for index := 0; index < len(values); index += 2 {
		equipmentID, fragmentID := values[index], values[index+1]
		if previous, exists := mapping[equipmentID]; exists {
			return nil, fmt.Errorf("%w: duplicate equipment=%d fragments=%d,%d", ErrCatalogInvalid, equipmentID, previous, fragmentID)
		}
		if previous, exists := fragments[fragmentID]; exists {
			return nil, fmt.Errorf("%w: duplicate fragment=%d equipment=%d,%d", ErrCatalogInvalid, fragmentID, previous, equipmentID)
		}
		mapping[equipmentID] = fragmentID
		fragments[fragmentID] = equipmentID
	}
	return mapping, nil
}

func parseRecipes(document *dnfpvf.Document) (map[uint32]Recipe, error) {
	recipes := make(map[uint32]Recipe)
	inRecipeInfo := false
	inInfo := false
	var outputID uint32
	var recipeValues []uint32

	commit := func() error {
		if outputID == 0 || len(recipeValues) < 2 || len(recipeValues)%2 != 0 {
			return fmt.Errorf("%w: recipe output=%d value_count=%d", ErrCatalogInvalid, outputID, len(recipeValues))
		}
		if _, exists := recipes[outputID]; exists {
			return fmt.Errorf("%w: duplicate recipe output=%d", ErrCatalogInvalid, outputID)
		}
		recipe := Recipe{
			OutputItemID:  outputID,
			FragmentID:    recipeValues[0],
			FragmentCount: recipeValues[1],
		}
		for index := 2; index < len(recipeValues); index += 2 {
			recipe.Materials = append(recipe.Materials, Material{ItemID: recipeValues[index], Count: recipeValues[index+1]})
		}
		recipes[outputID] = recipe
		return nil
	}

	for index, token := range document.Tokens {
		if token.Kind != dnfpvf.TokenSection {
			continue
		}
		name := normalizeSection(token.Value)
		switch name {
		case "equipment recipe info":
			inRecipeInfo = true
		case "/equipment recipe info":
			if inInfo {
				return nil, fmt.Errorf("%w: unterminated [info]", ErrCatalogInvalid)
			}
			inRecipeInfo = false
		case "info":
			if !inRecipeInfo || inInfo {
				return nil, fmt.Errorf("%w: unexpected [info]", ErrCatalogInvalid)
			}
			inInfo = true
			outputID = 0
			recipeValues = nil
		case "/info":
			if !inRecipeInfo || !inInfo {
				return nil, fmt.Errorf("%w: unexpected [/info]", ErrCatalogInvalid)
			}
			if err := commit(); err != nil {
				return nil, err
			}
			inInfo = false
		case "output":
			if !inRecipeInfo || !inInfo {
				continue
			}
			values, err := positiveUintTokens(sectionTokensAt(document, index), "output")
			if err != nil || len(values) != 1 {
				return nil, fmt.Errorf("%w: [output] value_count=%d", ErrCatalogInvalid, len(values))
			}
			outputID = values[0]
		case "recipe":
			if !inRecipeInfo || !inInfo {
				continue
			}
			values, err := positiveUintTokens(sectionTokensAt(document, index), "recipe")
			if err != nil {
				return nil, err
			}
			recipeValues = values
		}
	}
	if inInfo || inRecipeInfo {
		return nil, fmt.Errorf("%w: unterminated recipe section", ErrCatalogInvalid)
	}
	return recipes, nil
}

func uniqueSectionTokens(document *dnfpvf.Document, name string) ([]dnfpvf.Token, bool) {
	key := normalizeSection(name)
	var result []dnfpvf.Token
	count := 0
	for _, section := range document.Sections {
		if normalizeSection(section.Name) != key {
			continue
		}
		count++
		if section.Start < 0 || section.End > len(document.Tokens) || section.Start > section.End {
			return nil, false
		}
		result = document.Tokens[section.Start:section.End]
	}
	if count != 1 {
		return nil, false
	}
	return result, true
}

func sectionTokensAt(document *dnfpvf.Document, tokenIndex int) []dnfpvf.Token {
	start := tokenIndex + 1
	end := len(document.Tokens)
	for index := start; index < len(document.Tokens); index++ {
		if document.Tokens[index].Kind == dnfpvf.TokenSection {
			end = index
			break
		}
	}
	return document.Tokens[start:end]
}

func positiveUintTokens(tokens []dnfpvf.Token, section string) ([]uint32, error) {
	values := make([]uint32, 0, len(tokens))
	for _, token := range tokens {
		if token.Kind == dnfpvf.TokenSection {
			break
		}
		if token.Kind != dnfpvf.TokenInt || token.Int <= 0 || token.Int > math.MaxUint32 {
			return nil, fmt.Errorf("%w: [%s] token=%q is not a positive u32", ErrCatalogInvalid, section, token.Raw)
		}
		values = append(values, uint32(token.Int))
	}
	return values, nil
}

func normalizeSection(value string) string {
	return strings.ToLower(strings.Join(strings.Fields(strings.TrimSpace(value)), " "))
}

func cloneEntry(entry Entry) Entry {
	entry.Recipe.Materials = append([]Material(nil), entry.Recipe.Materials...)
	return entry
}
