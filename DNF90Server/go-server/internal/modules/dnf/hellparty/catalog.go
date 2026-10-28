package hellparty

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"

	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
)

const ConfigPath = "etc/hellparty.etc"

var (
	ErrSourceRequired = errors.New("dnf hell party PVF source is required")
	ErrCatalogInvalid = errors.New("dnf hell party catalog is invalid")
)

type TextSource interface {
	ReadText(string) (string, error)
}

// DifficultyRule preserves the five current-PVF values without assigning
// names to fields whose business meaning has not been confirmed in 90CN.
// Value0 is separately exposed as RewardRollCount because the current PVF and
// reference runtime agree on that use; the other values remain opaque.
type DifficultyRule struct {
	Key             string
	RewardRollCount uint32
	Values          [5]int64
}

type ActorDefinition struct {
	Code       uint32
	EntityType uint8
}

type Group struct {
	ID     uint32
	Actors map[string][]ActorDefinition
}

type Catalog struct {
	difficulties map[string]DifficultyRule
	groups       map[uint32]Group
}

func Load(source TextSource) (*Catalog, error) {
	if source == nil {
		return nil, ErrSourceRequired
	}
	text, err := source.ReadText(ConfigPath)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", ConfigPath, err)
	}
	return Parse(text)
}

func Parse(text string) (*Catalog, error) {
	document, err := dnfpvf.Parse(ConfigPath, text)
	if err != nil {
		return nil, fmt.Errorf("%w: parse %s: %v", ErrCatalogInvalid, ConfigPath, err)
	}
	return ParseDocument(document)
}

func ParseDocument(document *dnfpvf.Document) (*Catalog, error) {
	if document == nil {
		return nil, fmt.Errorf("%w: document is nil", ErrCatalogInvalid)
	}
	difficulties, err := parseDifficulties(document)
	if err != nil {
		return nil, err
	}
	groups, err := parseGroups(document)
	if err != nil {
		return nil, err
	}
	if len(difficulties) == 0 || len(groups) == 0 {
		return nil, fmt.Errorf("%w: difficulties=%d groups=%d", ErrCatalogInvalid, len(difficulties), len(groups))
	}
	return &Catalog{difficulties: difficulties, groups: groups}, nil
}

func (c *Catalog) Difficulty(key string) (DifficultyRule, bool) {
	if c == nil {
		return DifficultyRule{}, false
	}
	rule, ok := c.difficulties[normalizeKey(key)]
	return rule, ok
}

func (c *Catalog) Group(id uint32) (Group, bool) {
	if c == nil {
		return Group{}, false
	}
	group, ok := c.groups[id]
	return cloneGroup(group), ok
}

func (c *Catalog) GroupIDs() []uint32 {
	if c == nil {
		return nil
	}
	ids := make([]uint32, 0, len(c.groups))
	for id := range c.groups {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

func (c *Catalog) Snapshot() (difficultyRules, groups, actors int) {
	if c == nil {
		return 0, 0, 0
	}
	for _, group := range c.groups {
		for _, entries := range group.Actors {
			actors += len(entries)
		}
	}
	return len(c.difficulties), len(c.groups), actors
}

func parseDifficulties(document *dnfpvf.Document) (map[string]DifficultyRule, error) {
	tokens, ok := uniqueSection(document, "difficulty")
	if !ok {
		return nil, fmt.Errorf("%w: missing or duplicate [difficulty]", ErrCatalogInvalid)
	}
	out := make(map[string]DifficultyRule)
	for index := 0; index < len(tokens); {
		token := tokens[index]
		if token.Kind == dnfpvf.TokenSection {
			break
		}
		key, isKey := tokenKey(token)
		if !isKey {
			// The live 90CN table begins with one row-count integer. Preserve
			// forward compatibility by ignoring numeric metadata before a key.
			if token.Kind != dnfpvf.TokenInt {
				return nil, fmt.Errorf("%w: [difficulty] token=%q before key", ErrCatalogInvalid, token.Raw)
			}
			index++
			continue
		}
		index++
		if _, exists := out[key]; exists {
			return nil, fmt.Errorf("%w: duplicate difficulty=%s", ErrCatalogInvalid, key)
		}
		var values [5]int64
		for valueIndex := range values {
			if index >= len(tokens) || tokens[index].Kind != dnfpvf.TokenInt {
				return nil, fmt.Errorf("%w: difficulty=%s value_index=%d", ErrCatalogInvalid, key, valueIndex)
			}
			values[valueIndex] = tokens[index].Int
			index++
		}
		if values[0] < 0 || values[0] > math.MaxUint32 {
			return nil, fmt.Errorf("%w: difficulty=%s reward_roll_count=%d", ErrCatalogInvalid, key, values[0])
		}
		out[key] = DifficultyRule{Key: key, RewardRollCount: uint32(values[0]), Values: values}
	}
	return out, nil
}

func parseGroups(document *dnfpvf.Document) (map[uint32]Group, error) {
	out := make(map[uint32]Group)
	var pendingID uint32
	for _, section := range document.Sections {
		switch normalizeSection(section.Name) {
		case "group index":
			if pendingID != 0 {
				return nil, fmt.Errorf("%w: group=%d has no [group]", ErrCatalogInvalid, pendingID)
			}
			tokens := sectionTokens(document, section)
			if len(tokens) != 1 || tokens[0].Kind != dnfpvf.TokenInt || tokens[0].Int <= 0 || tokens[0].Int > math.MaxUint32 {
				return nil, fmt.Errorf("%w: invalid [group index]", ErrCatalogInvalid)
			}
			pendingID = uint32(tokens[0].Int)
		case "group":
			if pendingID == 0 {
				continue
			}
			if _, exists := out[pendingID]; exists {
				return nil, fmt.Errorf("%w: duplicate group=%d", ErrCatalogInvalid, pendingID)
			}
			actors, err := parseGroupActors(sectionTokens(document, section), pendingID)
			if err != nil {
				return nil, err
			}
			out[pendingID] = Group{ID: pendingID, Actors: actors}
			pendingID = 0
		}
	}
	if pendingID != 0 {
		return nil, fmt.Errorf("%w: group=%d has no [group]", ErrCatalogInvalid, pendingID)
	}
	return out, nil
}

func parseGroupActors(tokens []dnfpvf.Token, groupID uint32) (map[string][]ActorDefinition, error) {
	out := make(map[string][]ActorDefinition)
	currentKey := ""
	for index := 0; index < len(tokens); {
		if tokens[index].Kind == dnfpvf.TokenSection {
			break
		}
		if key, ok := tokenKey(tokens[index]); ok {
			currentKey = key
			if _, exists := out[currentKey]; exists {
				return nil, fmt.Errorf("%w: group=%d duplicate mode=%s", ErrCatalogInvalid, groupID, currentKey)
			}
			out[currentKey] = nil
			index++
			continue
		}
		if currentKey == "" || index+1 >= len(tokens) || tokens[index].Kind != dnfpvf.TokenInt || tokens[index+1].Kind != dnfpvf.TokenInt {
			return nil, fmt.Errorf("%w: group=%d actor pair at token=%d", ErrCatalogInvalid, groupID, index)
		}
		code, entityType := tokens[index].Int, tokens[index+1].Int
		if code <= 0 || code > math.MaxUint32 || (entityType != 0 && entityType != 1) {
			return nil, fmt.Errorf("%w: group=%d code=%d entity_type=%d", ErrCatalogInvalid, groupID, code, entityType)
		}
		out[currentKey] = append(out[currentKey], ActorDefinition{Code: uint32(code), EntityType: uint8(entityType)})
		index += 2
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%w: group=%d has no modes", ErrCatalogInvalid, groupID)
	}
	for key, actors := range out {
		if len(actors) == 0 {
			return nil, fmt.Errorf("%w: group=%d mode=%s has no actors", ErrCatalogInvalid, groupID, key)
		}
	}
	return out, nil
}

func uniqueSection(document *dnfpvf.Document, name string) ([]dnfpvf.Token, bool) {
	key := normalizeSection(name)
	var result []dnfpvf.Token
	count := 0
	for _, section := range document.Sections {
		if normalizeSection(section.Name) != key {
			continue
		}
		count++
		result = sectionTokens(document, section)
	}
	return result, count == 1
}

func sectionTokens(document *dnfpvf.Document, section dnfpvf.Section) []dnfpvf.Token {
	if section.Start < 0 || section.End < section.Start || section.End > len(document.Tokens) {
		return nil
	}
	return document.Tokens[section.Start:section.End]
}

func tokenKey(token dnfpvf.Token) (string, bool) {
	if token.Kind != dnfpvf.TokenString && token.Kind != dnfpvf.TokenIdent {
		return "", false
	}
	key := normalizeKey(token.Value)
	return key, len(key) == 1 && key[0] >= 'A' && key[0] <= 'Z'
}

func normalizeKey(value string) string {
	return strings.ToUpper(strings.TrimSpace(value))
}

func normalizeSection(value string) string {
	return strings.ToLower(strings.Join(strings.Fields(strings.TrimSpace(value)), " "))
}

func cloneGroup(group Group) Group {
	if group.Actors == nil {
		return group
	}
	source := group.Actors
	group.Actors = make(map[string][]ActorDefinition, len(source))
	for key, actors := range source {
		group.Actors[key] = append([]ActorDefinition(nil), actors...)
	}
	return group
}
