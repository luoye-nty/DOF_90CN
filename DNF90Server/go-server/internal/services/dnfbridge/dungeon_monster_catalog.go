package dnfbridge

import (
	"errors"
	"fmt"
	"path"
	"strings"
	"sync"

	"longheng.io/server/internal/modules/dnf/actionobject"
	dnfmonster "longheng.io/server/internal/modules/dnf/monster"
	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
	"longheng.io/server/internal/modules/dnf/worldmap"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

var (
	errDungeonMonsterSourceRequired  = errors.New("dnf dungeon monster PVF source is required")
	errDungeonMonsterListEmpty       = errors.New("dnf dungeon monster list is empty")
	errDungeonPassiveObjectListEmpty = errors.New("dnf dungeon passive-object list is empty")
)

const currentDungeonPassiveObjectList = "passiveobject/passiveobject.lst"

type pvfDungeonMonsterCatalog struct {
	mu                  sync.Mutex
	source              dnfpvf.Source
	paths               map[int64]string
	cache               map[int64]dnfmonster.Monster
	dropCatalog         *pvfDungeonDropCatalog
	dropCatalogErr      error
	dropCatalogLoaded   bool
	passiveObjectIDs    map[int64]struct{}
	passiveObjectPaths  map[int64]string
	passiveObjectGroups map[int64]int64
	passiveObjectKnown  map[int64]bool
	passiveObjectErr    error
	passiveObjectLoaded bool
	// actionScripts caches the `[basic action]` script a placed passive object
	// runs. Only objects a room actually places are resolved, so the whole
	// passiveobject tree is never parsed.（上游 b402882）
	actionScripts      map[int64]actionobject.Script
	actionScriptLoaded map[int64]bool
	hellMonster        map[int64]bool
	hellMonsterKnown   map[int64]bool
}

func (c *pvfDungeonMonsterCatalog) ensurePassiveObjectIndexLocked() {
	if c.passiveObjectLoaded {
		return
	}
	c.passiveObjectLoaded = true
	text, err := c.source.ReadText(currentDungeonPassiveObjectList)
	if err != nil {
		c.passiveObjectErr = fmt.Errorf("read %s: %w", currentDungeonPassiveObjectList, err)
		return
	}
	document, err := dnfpvf.Parse(currentDungeonPassiveObjectList, text)
	if err != nil {
		c.passiveObjectErr = fmt.Errorf("parse %s: %w", currentDungeonPassiveObjectList, err)
		return
	}
	entries := dnfpvf.ParseList(document)
	if len(entries) == 0 {
		c.passiveObjectErr = fmt.Errorf("%w: %s", errDungeonPassiveObjectListEmpty, currentDungeonPassiveObjectList)
		return
	}
	c.passiveObjectIDs = make(map[int64]struct{}, len(entries))
	c.passiveObjectPaths = make(map[int64]string, len(entries))
	c.passiveObjectGroups = make(map[int64]int64)
	c.passiveObjectKnown = make(map[int64]bool)
	for _, entry := range entries {
		c.passiveObjectIDs[entry.ID] = struct{}{}
		if _, exists := c.passiveObjectPaths[entry.ID]; !exists {
			c.passiveObjectPaths[entry.ID] = cleanDungeonPassiveObjectPath(entry.Path)
		}
	}
}

// specialObjectStartActorCompatibility distinguishes runtime-created action
// objects from legacy map objects already instantiated by the client's map
// loader. Re-emitting a MapObject row as an op29 type-9 actor asks the current
// client to resolve it through the action-object registry, where no matching
// runtime definition exists.
func (c *pvfDungeonMonsterCatalog) specialObjectStartActorCompatibility(id int64) (bool, string, error) {
	if c == nil || c.source == nil {
		return false, "", errDungeonMonsterCatalogUnavailable
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ensurePassiveObjectIndexLocked()
	if c.passiveObjectErr != nil {
		return false, "", c.passiveObjectErr
	}
	listedPath, found := c.passiveObjectPaths[id]
	if !found {
		return false, "", nil
	}
	compatibilityPath := strings.ToLower(cleanDungeonPassiveObjectPath(listedPath))
	compatibilityPath = strings.TrimPrefix(compatibilityPath, "passiveobject/")
	if strings.HasPrefix(compatibilityPath, "mapobject/") {
		return false, listedPath, nil
	}
	return true, listedPath, nil
}

// passiveObjectRewardGroup returns only the PVF mapping proven for ordinary
// quest-item passive objects. Unknown reward types remain ungrouped.
func (c *pvfDungeonMonsterCatalog) passiveObjectRewardGroup(id int64) (int64, bool, error) {
	if c == nil || c.source == nil {
		return 0, false, errDungeonMonsterCatalogUnavailable
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ensurePassiveObjectIndexLocked()
	if c.passiveObjectErr != nil {
		return 0, false, c.passiveObjectErr
	}
	listedPath, listed := c.passiveObjectPaths[id]
	if !listed {
		return 0, false, nil
	}
	if known, loaded := c.passiveObjectKnown[id]; loaded {
		return c.passiveObjectGroups[id], known, nil
	}
	docPath, text, err := readDungeonPassiveObjectText(c.source, listedPath)
	if err != nil {
		// Some current-PVF passive-object list entries reference absent object
		// definitions. They cannot establish a quest-item reward group, so leave
		// them ungrouped rather than blocking a room transition.
		if isDungeonPassiveObjectDefinitionMissing(err) {
			c.passiveObjectKnown[id] = false
			return 0, false, nil
		}
		return 0, false, fmt.Errorf("read passive object id=%d path=%s: %w", id, listedPath, err)
	}
	document, err := dnfpvf.Parse(docPath, text)
	if err != nil {
		return 0, false, fmt.Errorf("parse passive object id=%d path=%s: %w", id, docPath, err)
	}
	rewardType, _ := document.Text("reward type")
	c.passiveObjectKnown[id] = false
	if normalizeDungeonPVFSymbol(rewardType) == "quest item" {
		c.passiveObjectGroups[id] = 1
		c.passiveObjectKnown[id] = true
	}
	return c.passiveObjectGroups[id], c.passiveObjectKnown[id], nil
}

func isDungeonPassiveObjectDefinitionMissing(err error) bool {
	return errors.Is(err, dnfpvf.ErrDocNotFound) || errors.Is(err, platformpvf.ErrFileNotFound)
}

func readDungeonPassiveObjectText(source dnfpvf.Source, listedPath string) (string, string, error) {
	candidates := []string{cleanDungeonPassiveObjectPath(listedPath)}
	if !strings.HasPrefix(strings.ToLower(candidates[0]), "passiveobject/") {
		candidates = append(candidates, path.Join("passiveobject", candidates[0]))
	}
	var lastErr error
	for _, candidate := range candidates {
		text, err := source.ReadText(candidate)
		if err == nil {
			return candidate, text, nil
		}
		lastErr = err
	}
	return "", "", lastErr
}

func cleanDungeonPassiveObjectPath(value string) string {
	value = strings.TrimSpace(strings.ReplaceAll(value, "\\", "/"))
	value = strings.TrimPrefix(value, "./")
	return strings.TrimPrefix(value, "/")
}

func newPVFDungeonMonsterCatalog(source dnfpvf.Source) (*pvfDungeonMonsterCatalog, error) {
	if source == nil {
		return nil, errDungeonMonsterSourceRequired
	}
	text, err := source.ReadText(dnfmonster.DefaultList)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", dnfmonster.DefaultList, err)
	}
	document, err := dnfpvf.Parse(dnfmonster.DefaultList, text)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", dnfmonster.DefaultList, err)
	}
	entries := dnfpvf.ParseList(document)
	if len(entries) == 0 {
		return nil, fmt.Errorf("%w: %s", errDungeonMonsterListEmpty, dnfmonster.DefaultList)
	}
	catalog := &pvfDungeonMonsterCatalog{
		source:           source,
		paths:            make(map[int64]string, len(entries)),
		cache:            make(map[int64]dnfmonster.Monster),
		hellMonster:      make(map[int64]bool),
		hellMonsterKnown: make(map[int64]bool),
	}
	for _, entry := range entries {
		if _, exists := catalog.paths[entry.ID]; !exists {
			catalog.paths[entry.ID] = cleanDungeonMonsterPath(entry.Path)
		}
	}
	return catalog, nil
}

func (c *pvfDungeonMonsterCatalog) Count() int {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.paths)
}

// randomizedObjectFamily resolves the current op29 family field.  Some PVFs
// list the same numeric id in both monster.lst and passiveobject.lst; the
// current client resolves the monster table first, which is required for the
// AT-5T ridable monster (61235).
func (c *pvfDungeonMonsterCatalog) randomizedObjectFamily(id int64) (uint32, bool, error) {
	if c == nil || c.source == nil {
		return 0, false, errDungeonMonsterCatalogUnavailable
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, found := c.paths[id]; found {
		return currentDungeonRandomizedMonsterFamily, true, nil
	}
	c.ensurePassiveObjectIndexLocked()
	if c.passiveObjectErr != nil {
		return 0, false, c.passiveObjectErr
	}
	if _, found := c.passiveObjectIDs[id]; found {
		return currentDungeonRandomizedPassiveFamily, true, nil
	}
	return 0, false, nil
}

func (c *pvfDungeonMonsterCatalog) Find(id int64) (dnfmonster.Monster, bool, error) {
	if c == nil || c.source == nil {
		return dnfmonster.Monster{}, false, errDungeonMonsterCatalogUnavailable
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if cached, ok := c.cache[id]; ok {
		return cloneDungeonMonsterDefinition(cached), true, nil
	}
	listedPath, ok := c.paths[id]
	if !ok {
		return dnfmonster.Monster{}, false, nil
	}
	docPath, text, err := readDungeonMonsterText(c.source, listedPath)
	if err != nil {
		return dnfmonster.Monster{}, false, fmt.Errorf("read monster id=%d path=%s: %w", id, listedPath, err)
	}
	document, err := worldmap.ParseDocument(docPath, text)
	if err != nil {
		return dnfmonster.Monster{}, false, fmt.Errorf("parse monster id=%d path=%s: %w", id, docPath, err)
	}
	definition := parseDungeonMonsterDefinition(id, docPath, document)
	c.cache[id] = definition
	hellMonsterValue, hellMonsterFound := document.Int("hell monster")
	c.hellMonster[id] = hellMonsterFound && hellMonsterValue == 1
	c.hellMonsterKnown[id] = true
	return cloneDungeonMonsterDefinition(definition), true, nil
}

func (c *pvfDungeonMonsterCatalog) HellMonster(id int64) (bool, error) {
	if c == nil || c.source == nil {
		return false, errDungeonMonsterCatalogUnavailable
	}
	c.mu.Lock()
	if c.hellMonsterKnown[id] {
		value := c.hellMonster[id]
		c.mu.Unlock()
		return value, nil
	}
	c.mu.Unlock()
	_, found, err := c.Find(id)
	if err != nil || !found {
		return false, err
	}
	c.mu.Lock()
	value := c.hellMonster[id]
	c.mu.Unlock()
	return value, nil
}

func readDungeonMonsterText(source dnfpvf.Source, listedPath string) (string, string, error) {
	candidates := []string{cleanDungeonMonsterPath(listedPath)}
	if !strings.HasPrefix(strings.ToLower(candidates[0]), "monster/") {
		candidates = append(candidates, path.Join("monster", candidates[0]))
	}
	var lastErr error
	for _, candidate := range candidates {
		text, err := source.ReadText(candidate)
		if err == nil {
			return candidate, text, nil
		}
		lastErr = err
	}
	return "", "", lastErr
}

func cleanDungeonMonsterPath(value string) string {
	value = strings.TrimSpace(strings.ReplaceAll(value, "\\", "/"))
	value = strings.TrimPrefix(value, "./")
	return strings.TrimPrefix(value, "/")
}

func parseDungeonMonsterDefinition(id int64, docPath string, document *dnfpvf.Document) dnfmonster.Monster {
	definition := dnfmonster.Monster{
		ID:      id,
		Path:    docPath,
		Name:    dungeonMonsterText(document, "name", "display name"),
		Kind:    dungeonMonsterText(document, "monster type", "type", "kind"),
		Rank:    dungeonMonsterText(document, "rank", "monster rank", "grade"),
		Level:   dungeonMonsterInt(document, "level", "monster level"),
		HP:      dungeonMonsterInt(document, "hp", "hit point", "max hp"),
		Attack:  dungeonMonsterInt(document, "attack", "physical attack"),
		Defense: dungeonMonsterInt(document, "defense", "physical defense"),
		Move:    dungeonMonsterNumber(document, "move speed", "movement speed"),
		Speed:   dungeonMonsterNumber(document, "attack speed"),
		Exp:     dungeonMonsterInt(document, "exp", "experience"),
		AI:      dungeonMonsterText(document, "ai", "ai pattern"),
		Icon:    dungeonMonsterText(document, "icon", "icon path"),
		Scalars: dungeonMonsterScalars(document),
	}
	if definition.Name == "" {
		definition.Name = definition.Path
	}
	if document != nil {
		definition.Sections = make([]string, 0, len(document.Sections))
		for _, section := range document.Sections {
			definition.Sections = append(definition.Sections, section.Name)
		}
	}
	return definition
}

func dungeonMonsterText(document *dnfpvf.Document, names ...string) string {
	for _, name := range names {
		if value, ok := document.Text(name); ok {
			return value
		}
	}
	return ""
}

func dungeonMonsterInt(document *dnfpvf.Document, names ...string) int64 {
	for _, name := range names {
		if value, ok := document.Int(name); ok {
			return value
		}
	}
	return 0
}

func dungeonMonsterNumber(document *dnfpvf.Document, names ...string) float64 {
	for _, name := range names {
		if value, ok := document.Number(name); ok {
			return value
		}
	}
	return 0
}

func dungeonMonsterScalars(document *dnfpvf.Document) map[string]float64 {
	names := map[string][]string{
		"hp":            {"hp", "hit point", "max hp"},
		"attack":        {"attack", "physical attack"},
		"magic_attack":  {"magical attack", "magic attack"},
		"defense":       {"defense", "physical defense"},
		"magic_defense": {"magical defense", "magic defense"},
		"move_speed":    {"move speed", "movement speed"},
		"attack_speed":  {"attack speed"},
		"hit_recovery":  {"hit recovery"},
		"exp":           {"exp", "experience"},
	}
	out := make(map[string]float64)
	for key, aliases := range names {
		for _, alias := range aliases {
			if value, ok := document.Number(alias); ok {
				out[key] = value
				break
			}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func cloneDungeonMonsterDefinition(definition dnfmonster.Monster) dnfmonster.Monster {
	definition.Scalars = cloneFloatMap(definition.Scalars)
	definition.Sections = append([]string(nil), definition.Sections...)
	return definition
}
