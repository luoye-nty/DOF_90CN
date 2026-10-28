package dnfbridge

import (
	"errors"
	"fmt"
	"math"
	"path"
	"strconv"
	"strings"
	"sync"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
	dnfraid "longheng.io/server/internal/modules/dnf/raid"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

const currentAntonRewardPhaseCount = 2

// currentLukeRewardPhaseCount / currentFiendWarRewardPhaseCount 是这两张奖励表
// **各自**声明的阶段数。原来是拿安东的 2 当通用常量，超时空只有 1 个 [PHASE]
// （fiendwar.etc 的 `[PHASE MAX] 1`），于是它整条发奖在提交时被拒：
//
//	真机 2026-10-02 13:52:48（raid 12058625，16 分制里的第 2 局，5 分 39 秒通关）：
//	game-raid-reward-auto-flip-commit-failed flipped=4
//	  error=anton reward PVF definition set is invalid: phases=1 want=2
//	→ 四个人的翻牌都翻开了（flipped=4）却被整批回滚，客户端只看到一个「0」，
//	  这就是操作员看到的「翻牌翻不开」。超时空的最后一次翻牌本来就是**攻坚结束**
//	  的那一次（与卢克/安徒恩二阶段结束同一条路：movie → 倒计时 → 自动翻开 → 提交）。
const (
	currentLukeRewardPhaseCount     = 2
	currentFiendWarRewardPhaseCount = 1
)

var (
	errAntonRewardPVFCatalogRequired     = errors.New("anton reward PVF item catalog is required")
	errAntonRewardPVFConfigRequired      = errors.New("anton reward PVF config is required")
	errAntonRewardPVFDefinitionSet       = errors.New("anton reward PVF definition set is invalid")
	errAntonRewardPVFDefinitionMissing   = errors.New("anton reward PVF definition item is unresolved")
	errAntonRewardPVFSectionShape        = errors.New("anton reward PVF int data shape is invalid")
	errAntonRewardPVFValueInvalid        = errors.New("anton reward PVF value is invalid")
	errAntonRewardPVFFinalItemUnresolved = errors.New("anton reward PVF final item is unresolved")
	errAntonRewardPVFNestingInvalid      = errors.New("anton reward PVF nesting is invalid")
	errAntonRewardPVFRNGRequired         = errors.New("anton reward PVF RNG is required")
	errAntonRewardPVFPoolUnresolved      = errors.New("anton reward PVF pool is unresolved")
)

// antonRewardPVFRNG is deliberately tiny so runtime code can inject a seeded
// generator and tests can pin every outer and inner weighted choice.
type antonRewardPVFRNG interface {
	Uint64() uint64
}

// antonRewardPVFEntry preserves one exact {final item, weight, count} triple
// from a definition item's [int data] section. Duplicate rows remain distinct.
type antonRewardPVFEntry struct {
	ItemID uint32
	Weight uint32
	Count  uint32
}

type antonRewardPVFDefinition struct {
	DefinitionItemID uint32
	Kind             dungeonDropItemKind
	PVFPath          string
	RewardType       string
	DirectFinal      bool
	Entries          []antonRewardPVFEntry
	TotalWeight      uint64
}

type antonRewardPVFPoolEntry struct {
	DefinitionItemID uint32
	Rank             int
	Weight           uint32
	Flag             byte
}

type antonRewardPVFPool struct {
	PhaseIndex  int
	Kind        string
	Entries     []antonRewardPVFPoolEntry
	TotalWeight uint64
}

type antonRewardPVFPoolKey struct {
	phaseIndex int
	kind       string
}

// antonRewardPVFResolved is a final grant instruction. DefinitionItemID and
// NestedDefinitionItemID are evidence only. ItemID is either a leaf from an
// [int data] table or the definition item itself when the PVF declares a
// directly grantable [booster info] reward box.
type antonRewardPVFResolved struct {
	PhaseIndex              int
	PoolKind                string
	PoolWeight              uint32
	Rank                    int
	Flag                    byte
	DefinitionItemID        uint32
	DefinitionPVFPath       string
	NestedDefinitionItemID  uint32
	NestedDefinitionPVFPath string
	ItemID                  uint32
	Count                   uint32
	ItemKind                dungeonDropItemKind
	ItemPVFPath             string
}

type antonRewardPVFCatalogSnapshot struct {
	Definitions       int
	DefinitionEntries int
	PhasePools        int
	PhasePoolEntries  int
	NestedReferences  int
}

// antonRewardPVFCatalog contains the current 90 Anton reward definitions for
// both the team pools and the native single-mode pool.
// It does not mutate inventory or own raid state.
type antonRewardPVFCatalog struct {
	definitions     map[uint32]antonRewardPVFDefinition
	definitionOrder []uint32
	pools           map[antonRewardPVFPoolKey]antonRewardPVFPool
	poolOrder       []antonRewardPVFPoolKey
	itemRefs        map[uint32]dungeonDropItemReference
	source          dnfpvf.Source
	finalMu         sync.Mutex
	finalCache      map[uint32]dungeonDropItemDefinition
	raid            string
	overrides       *raidRewardOverrides
	nameMu          sync.Mutex
	names           map[uint32]string
}

// newAntonRewardPVFCatalog reads the definition documents referenced by the
// current team and single-mode anton.etc pools. The existing item catalog has
// already paid the list-index cost; no final item document is opened here.
//
// 安徒恩的外层奖励定义与卢克、超时空一样由 PVF 决定。阶段数仍然固定为两个，
// 但不再要求固定的定义 ID 集合；这样服主可以在 anton.etc 中替换奖池定义，
// 而不需要同步修改 Go 代码。
func newAntonRewardPVFCatalog(items *pvfDungeonDropCatalog, config *dnfraid.AntonConfig) (*antonRewardPVFCatalog, error) {
	return newRaidRewardPVFCatalog(items, config, nil, false, currentAntonRewardPhaseCount)
}

func newRaidRewardPVFCatalog(items *pvfDungeonDropCatalog, config *dnfraid.AntonConfig, requiredDefinitions []uint32, requireAll bool, expectedPhases int) (*antonRewardPVFCatalog, error) {
	if items == nil {
		return nil, errAntonRewardPVFCatalogRequired
	}
	if config == nil {
		return nil, errAntonRewardPVFConfigRequired
	}

	items.mu.Lock()
	source := items.source
	itemRefs := make(map[uint32]dungeonDropItemReference, len(items.itemRefs))
	for itemID, reference := range items.itemRefs {
		itemRefs[itemID] = reference
	}
	items.mu.Unlock()
	if source == nil {
		return nil, errAntonRewardPVFCatalogRequired
	}

	pools, poolOrder, err := buildRaidRewardPVFPools(config, requiredDefinitions, requireAll, expectedPhases)
	if err != nil {
		return nil, err
	}
	definitionOrder := raidRewardDefinitionOrder(config, requiredDefinitions)
	catalog := &antonRewardPVFCatalog{
		definitions:     make(map[uint32]antonRewardPVFDefinition, len(definitionOrder)),
		definitionOrder: definitionOrder,
		pools:           pools,
		poolOrder:       poolOrder,
		itemRefs:        itemRefs,
		source:          source,
		finalCache:      make(map[uint32]dungeonDropItemDefinition),
	}
	for _, definitionItemID := range catalog.definitionOrder {
		definition, loadErr := loadAntonRewardPVFDefinition(source, itemRefs, definitionItemID)
		if loadErr != nil {
			return nil, loadErr
		}
		catalog.definitions[definitionItemID] = definition
	}
	if err := catalog.validateFinalReferences(); err != nil {
		return nil, err
	}
	return catalog, nil
}

func antonRewardDefinitionOrder(config *dnfraid.AntonConfig) []uint32 {
	return raidRewardDefinitionOrder(config, nil)
}

func raidRewardDefinitionOrder(config *dnfraid.AntonConfig, requiredDefinitions []uint32) []uint32 {
	order := append([]uint32(nil), requiredDefinitions...)
	seen := make(map[uint32]struct{}, len(order))
	for _, id := range order {
		seen[id] = struct{}{}
	}
	appendRewards := func(rewards []dnfraid.AntonStateReward) {
		for _, reward := range rewards {
			if reward.DefinitionItemID == 0 {
				continue
			}
			if _, exists := seen[reward.DefinitionItemID]; exists {
				continue
			}
			seen[reward.DefinitionItemID] = struct{}{}
			order = append(order, reward.DefinitionItemID)
		}
	}
	if config != nil {
		for _, phase := range config.Phases {
			appendRewards(phase.Rewards)
		}
		if config.SingleMode != nil {
			appendRewards(config.SingleMode.Phase.Rewards)
		}
	}
	return order
}

func buildAntonRewardPVFPools(config *dnfraid.AntonConfig) (map[antonRewardPVFPoolKey]antonRewardPVFPool, []antonRewardPVFPoolKey, error) {
	return buildRaidRewardPVFPools(config, nil, false, currentAntonRewardPhaseCount)
}

func buildRaidRewardPVFPools(config *dnfraid.AntonConfig, requiredDefinitions []uint32, requireAll bool, expectedPhases int) (map[antonRewardPVFPoolKey]antonRewardPVFPool, []antonRewardPVFPoolKey, error) {
	if len(config.Phases) != expectedPhases {
		return nil, nil, fmt.Errorf("%w: phases=%d want=%d", errAntonRewardPVFDefinitionSet, len(config.Phases), expectedPhases)
	}
	allowed := make(map[uint32]struct{}, len(requiredDefinitions))
	for _, itemID := range requiredDefinitions {
		allowed[itemID] = struct{}{}
	}
	seenDefinitions := make(map[uint32]struct{}, len(allowed))
	pools := make(map[antonRewardPVFPoolKey]antonRewardPVFPool)
	poolOrder := make([]antonRewardPVFPoolKey, 0, 6)
	poolDefinitions := make(map[antonRewardPVFPoolKey]map[uint32]struct{})
	for phaseIndex, phase := range config.Phases {
		if len(phase.Rewards) == 0 {
			return nil, nil, fmt.Errorf("%w: phase=%d has no rewards", errAntonRewardPVFDefinitionSet, phaseIndex)
		}
		for rewardIndex, reward := range phase.Rewards {
			kind := normalizeAntonRewardPVFPoolKind(reward.Kind)
			if kind == "" || reward.DefinitionItemID == 0 || reward.Weight <= 0 || uint64(reward.Weight) > math.MaxUint32 {
				return nil, nil, fmt.Errorf("%w: phase=%d reward=%d kind=%q definition=%d weight=%d", errAntonRewardPVFValueInvalid, phaseIndex, rewardIndex, reward.Kind, reward.DefinitionItemID, reward.Weight)
			}
			if requireAll {
				if _, ok := allowed[reward.DefinitionItemID]; !ok {
					return nil, nil, fmt.Errorf("%w: phase=%d reward=%d unexpected_definition=%d", errAntonRewardPVFDefinitionSet, phaseIndex, rewardIndex, reward.DefinitionItemID)
				}
			}
			key := antonRewardPVFPoolKey{phaseIndex: phaseIndex, kind: kind}
			pool, found := pools[key]
			if !found {
				pool = antonRewardPVFPool{PhaseIndex: phaseIndex, Kind: kind}
				poolOrder = append(poolOrder, key)
				poolDefinitions[key] = make(map[uint32]struct{})
			}
			if _, duplicate := poolDefinitions[key][reward.DefinitionItemID]; duplicate {
				return nil, nil, fmt.Errorf("%w: phase=%d kind=%s duplicate_definition=%d", errAntonRewardPVFDefinitionSet, phaseIndex, kind, reward.DefinitionItemID)
			}
			weight := uint32(reward.Weight)
			if math.MaxUint64-pool.TotalWeight < uint64(weight) {
				return nil, nil, fmt.Errorf("%w: phase=%d kind=%s total_weight_overflow", errAntonRewardPVFValueInvalid, phaseIndex, kind)
			}
			pool.Entries = append(pool.Entries, antonRewardPVFPoolEntry{
				DefinitionItemID: reward.DefinitionItemID,
				Rank:             reward.Rank,
				Weight:           weight,
				Flag:             reward.Flag,
			})
			pool.TotalWeight += uint64(weight)
			pools[key] = pool
			poolDefinitions[key][reward.DefinitionItemID] = struct{}{}
			seenDefinitions[reward.DefinitionItemID] = struct{}{}
		}
	}
	if requireAll {
		for _, definitionItemID := range requiredDefinitions {
			if _, found := seenDefinitions[definitionItemID]; !found {
				return nil, nil, fmt.Errorf("%w: missing_definition=%d", errAntonRewardPVFDefinitionSet, definitionItemID)
			}
		}
	}
	if config.SingleMode != nil {
		singlePhaseIndex := len(config.Phases)
		phase := config.SingleMode.Phase
		if len(phase.Rewards) == 0 {
			return nil, nil, fmt.Errorf("%w: single phase has no rewards", errAntonRewardPVFDefinitionSet)
		}
		for rewardIndex, reward := range phase.Rewards {
			kind := normalizeAntonRewardPVFPoolKind(reward.Kind)
			if kind == "" || reward.DefinitionItemID == 0 || reward.Weight <= 0 || uint64(reward.Weight) > math.MaxUint32 {
				return nil, nil, fmt.Errorf("%w: single reward=%d kind=%q definition=%d weight=%d", errAntonRewardPVFValueInvalid, rewardIndex, reward.Kind, reward.DefinitionItemID, reward.Weight)
			}
			key := antonRewardPVFPoolKey{phaseIndex: singlePhaseIndex, kind: kind}
			pool, found := pools[key]
			if !found {
				pool = antonRewardPVFPool{PhaseIndex: singlePhaseIndex, Kind: kind}
				poolOrder = append(poolOrder, key)
				poolDefinitions[key] = make(map[uint32]struct{})
			}
			if _, duplicate := poolDefinitions[key][reward.DefinitionItemID]; duplicate {
				return nil, nil, fmt.Errorf("%w: single kind=%s duplicate_definition=%d", errAntonRewardPVFDefinitionSet, kind, reward.DefinitionItemID)
			}
			weight := uint32(reward.Weight)
			if math.MaxUint64-pool.TotalWeight < uint64(weight) {
				return nil, nil, fmt.Errorf("%w: single kind=%s total_weight_overflow", errAntonRewardPVFValueInvalid, kind)
			}
			pool.Entries = append(pool.Entries, antonRewardPVFPoolEntry{
				DefinitionItemID: reward.DefinitionItemID,
				Rank:             reward.Rank,
				Weight:           weight,
				Flag:             reward.Flag,
			})
			pool.TotalWeight += uint64(weight)
			pools[key] = pool
			poolDefinitions[key][reward.DefinitionItemID] = struct{}{}
		}
	}
	return pools, poolOrder, nil
}

func loadAntonRewardPVFDefinition(source dnfpvf.Source, itemRefs map[uint32]dungeonDropItemReference, definitionItemID uint32) (antonRewardPVFDefinition, error) {
	reference, found := itemRefs[definitionItemID]
	if !found {
		return antonRewardPVFDefinition{}, fmt.Errorf("%w: definition=%d", errAntonRewardPVFDefinitionMissing, definitionItemID)
	}
	documentPath, err := antonRewardPVFReferencePath(reference)
	if err != nil {
		return antonRewardPVFDefinition{}, fmt.Errorf("%w: definition=%d: %v", errAntonRewardPVFDefinitionMissing, definitionItemID, err)
	}
	text, err := source.ReadText(documentPath)
	if err != nil {
		return antonRewardPVFDefinition{}, fmt.Errorf("read anton reward definition=%d path=%s: %w", definitionItemID, documentPath, err)
	}
	document, err := dnfpvf.Parse(documentPath, text)
	if err != nil {
		return antonRewardPVFDefinition{}, fmt.Errorf("parse anton reward definition=%d path=%s: %w", definitionItemID, documentPath, err)
	}
	rewardType := normalizeMagicBoxPVFType(magicBoxDocumentText(document, "stackable type"))
	directFinal := raidRewardPVFHasSection(document, "booster info") && !raidRewardPVFHasSection(document, "int data")
	entries := []antonRewardPVFEntry{{ItemID: definitionItemID, Weight: 1, Count: 1}}
	totalWeight := uint64(1)
	if !directFinal {
		entries, totalWeight, err = parseAntonRewardPVFEntries(document, definitionItemID)
		if err != nil {
			return antonRewardPVFDefinition{}, err
		}
	}
	return antonRewardPVFDefinition{
		DefinitionItemID: definitionItemID,
		Kind:             reference.kind,
		PVFPath:          documentPath,
		RewardType:       rewardType,
		DirectFinal:      directFinal,
		Entries:          entries,
		TotalWeight:      totalWeight,
	}, nil
}

func raidRewardPVFHasSection(document *dnfpvf.Document, name string) bool {
	if document == nil {
		return false
	}
	want := normalizeAntonRewardPVFSection(name)
	for _, section := range document.Sections {
		if normalizeAntonRewardPVFSection(section.Name) == want {
			return true
		}
	}
	return false
}

func parseAntonRewardPVFEntries(document *dnfpvf.Document, definitionItemID uint32) ([]antonRewardPVFEntry, uint64, error) {
	if document == nil {
		return nil, 0, fmt.Errorf("%w: definition=%d nil_document", errAntonRewardPVFSectionShape, definitionItemID)
	}
	openIndex, closeIndex := -1, -1
	for index, section := range document.Sections {
		switch normalizeAntonRewardPVFSection(section.Name) {
		case "int data":
			if openIndex >= 0 {
				return nil, 0, fmt.Errorf("%w: definition=%d duplicate_int_data", errAntonRewardPVFSectionShape, definitionItemID)
			}
			openIndex = index
		case "/int data":
			if closeIndex >= 0 {
				return nil, 0, fmt.Errorf("%w: definition=%d duplicate_int_data_close", errAntonRewardPVFSectionShape, definitionItemID)
			}
			closeIndex = index
		}
	}
	if openIndex < 0 || closeIndex != openIndex+1 {
		return nil, 0, fmt.Errorf("%w: definition=%d int_data=%d close=%d", errAntonRewardPVFSectionShape, definitionItemID, openIndex, closeIndex)
	}
	closingTokens, err := antonRewardPVFSectionTokens(document, closeIndex)
	if err != nil || len(closingTokens) != 0 {
		return nil, 0, fmt.Errorf("%w: definition=%d closing_values=%d", errAntonRewardPVFSectionShape, definitionItemID, len(closingTokens))
	}
	tokens, err := antonRewardPVFSectionTokens(document, openIndex)
	if err != nil {
		return nil, 0, err
	}
	return parseRaidRewardTripleTokens(tokens, definitionItemID)
}

func parseRaidRewardTripleTokens(tokens []dnfpvf.Token, definitionItemID uint32) ([]antonRewardPVFEntry, uint64, error) {
	if len(tokens) == 0 || len(tokens)%3 != 0 {
		return nil, 0, fmt.Errorf("%w: definition=%d values=%d row_width=3", errAntonRewardPVFSectionShape, definitionItemID, len(tokens))
	}
	values := make([]int64, 0, len(tokens))
	for tokenIndex, token := range tokens {
		if token.Kind != dnfpvf.TokenInt {
			return nil, 0, fmt.Errorf("%w: definition=%d token=%d kind=%s", errAntonRewardPVFSectionShape, definitionItemID, tokenIndex, token.Kind)
		}
		values = append(values, token.Int)
	}
	entries := make([]antonRewardPVFEntry, 0, len(values)/3)
	var totalWeight uint64
	for offset := 0; offset < len(values); offset += 3 {
		itemID, weight, count := values[offset], values[offset+1], values[offset+2]
		if itemID <= 0 || itemID > math.MaxUint32 || weight <= 0 || weight > math.MaxUint32 || count <= 0 || count > math.MaxUint32 {
			return nil, 0, fmt.Errorf("%w: definition=%d row=%d item=%d weight=%d count=%d", errAntonRewardPVFValueInvalid, definitionItemID, offset/3, itemID, weight, count)
		}
		if math.MaxUint64-totalWeight < uint64(weight) {
			return nil, 0, fmt.Errorf("%w: definition=%d total_weight_overflow", errAntonRewardPVFValueInvalid, definitionItemID)
		}
		entries = append(entries, antonRewardPVFEntry{ItemID: uint32(itemID), Weight: uint32(weight), Count: uint32(count)})
		totalWeight += uint64(weight)
	}
	return entries, totalWeight, nil
}

func antonRewardPVFSectionTokens(document *dnfpvf.Document, sectionIndex int) ([]dnfpvf.Token, error) {
	if document == nil || sectionIndex < 0 || sectionIndex >= len(document.Sections) {
		return nil, fmt.Errorf("%w: section_index=%d", errAntonRewardPVFSectionShape, sectionIndex)
	}
	section := document.Sections[sectionIndex]
	if section.Start < 0 || section.End < section.Start || section.End > len(document.Tokens) {
		return nil, fmt.Errorf("%w: section=%s range=%d:%d tokens=%d", errAntonRewardPVFSectionShape, section.Name, section.Start, section.End, len(document.Tokens))
	}
	return document.Tokens[section.Start:section.End], nil
}

func (c *antonRewardPVFCatalog) validateFinalReferences() error {
	for _, definitionItemID := range c.definitionOrder {
		definition := c.definitions[definitionItemID]
		if definition.DirectFinal {
			if _, err := c.finalItemReference(definitionItemID); err != nil {
				return fmt.Errorf("definition=%d direct_final: %w", definitionItemID, err)
			}
			continue
		}
		for entryIndex, entry := range definition.Entries {
			nested, isNested := c.definitions[entry.ItemID]
			if !isNested {
				if _, err := c.finalItemReference(entry.ItemID); err != nil {
					return fmt.Errorf("definition=%d entry=%d: %w", definitionItemID, entryIndex, err)
				}
				continue
			}
			if nested.DefinitionItemID == definitionItemID {
				return fmt.Errorf("%w: definition=%d self_reference", errAntonRewardPVFNestingInvalid, definitionItemID)
			}
			if nested.DirectFinal {
				if _, err := c.finalItemReference(nested.DefinitionItemID); err != nil {
					return fmt.Errorf("definition=%d entry=%d nested_direct=%d: %w", definitionItemID, entryIndex, nested.DefinitionItemID, err)
				}
				continue
			}
			for nestedIndex, nestedEntry := range nested.Entries {
				if _, deeper := c.definitions[nestedEntry.ItemID]; deeper {
					return fmt.Errorf("%w: definition=%d entry=%d nested=%d nested_entry=%d deeper=%d", errAntonRewardPVFNestingInvalid, definitionItemID, entryIndex, nested.DefinitionItemID, nestedIndex, nestedEntry.ItemID)
				}
				if uint64(entry.Count)*uint64(nestedEntry.Count) > math.MaxUint32 {
					return fmt.Errorf("%w: definition=%d nested=%d count_overflow=%d*%d", errAntonRewardPVFValueInvalid, definitionItemID, nested.DefinitionItemID, entry.Count, nestedEntry.Count)
				}
				if _, err := c.finalItemReference(nestedEntry.ItemID); err != nil {
					return fmt.Errorf("definition=%d entry=%d nested=%d nested_entry=%d: %w", definitionItemID, entryIndex, nested.DefinitionItemID, nestedIndex, err)
				}
			}
		}
	}
	return nil
}

func (c *antonRewardPVFCatalog) finalItemReference(itemID uint32) (dungeonDropItemReference, error) {
	reference, found := c.itemRefs[itemID]
	if !found {
		return dungeonDropItemReference{}, fmt.Errorf("%w: item=%d", errAntonRewardPVFFinalItemUnresolved, itemID)
	}
	if _, err := antonRewardPVFReferencePath(reference); err != nil {
		return dungeonDropItemReference{}, fmt.Errorf("%w: item=%d: %v", errAntonRewardPVFFinalItemUnresolved, itemID, err)
	}
	return reference, nil
}

func (c *antonRewardPVFCatalog) Definition(definitionItemID uint32) (antonRewardPVFDefinition, bool) {
	if c == nil {
		return antonRewardPVFDefinition{}, false
	}
	definition, found := c.definitions[definitionItemID]
	if !found {
		return antonRewardPVFDefinition{}, false
	}
	definition.Entries = append([]antonRewardPVFEntry(nil), definition.Entries...)
	return definition, true
}

func (c *antonRewardPVFCatalog) DefinitionIDs() []uint32 {
	if c == nil {
		return nil
	}
	return append([]uint32(nil), c.definitionOrder...)
}

func (c *antonRewardPVFCatalog) Pool(phaseIndex int, kind string) (antonRewardPVFPool, bool) {
	if c == nil {
		return antonRewardPVFPool{}, false
	}
	key := antonRewardPVFPoolKey{phaseIndex: phaseIndex, kind: normalizeAntonRewardPVFPoolKind(kind)}
	pool, found := c.pools[key]
	if !found {
		return antonRewardPVFPool{}, false
	}
	pool.Entries = append([]antonRewardPVFPoolEntry(nil), pool.Entries...)
	return pool, true
}

// Pools returns every phase/kind pool in source order. Entries are copied so
// callers can inspect them without mutating the catalog.
func (c *antonRewardPVFCatalog) Pools() []antonRewardPVFPool {
	if c == nil {
		return nil
	}
	out := make([]antonRewardPVFPool, 0, len(c.poolOrder))
	for _, key := range c.poolOrder {
		pool, found := c.pools[key]
		if !found {
			continue
		}
		pool.Entries = append([]antonRewardPVFPoolEntry(nil), pool.Entries...)
		out = append(out, pool)
	}
	return out
}

// ItemName resolves the display name of one reward definition/box item. The
// reward catalog already owns the exact item reference; a one-item catalog
// avoids initializing any unrelated monster or drop pool.
func (c *antonRewardPVFCatalog) ItemName(itemID uint32) string {
	if c == nil || itemID == 0 {
		return ""
	}
	c.nameMu.Lock()
	defer c.nameMu.Unlock()
	if c.names == nil {
		c.names = make(map[uint32]string)
	}
	if name, found := c.names[itemID]; found {
		return name
	}
	reference, found := c.itemRefs[itemID]
	if !found {
		c.names[itemID] = ""
		return ""
	}
	single := &pvfDungeonDropCatalog{
		source: c.source,
		itemRefs: map[uint32]dungeonDropItemReference{
			itemID: reference,
		},
		itemCache: make(map[uint32]dungeonDropItemDefinition, 1),
	}
	definition, err := single.ResolveItem(itemID)
	if err != nil {
		c.names[itemID] = ""
		return ""
	}
	c.names[itemID] = definition.Name
	return definition.Name
}

func (c *antonRewardPVFCatalog) ResolvePool(phaseIndex int, kind string, rng antonRewardPVFRNG) (antonRewardPVFResolved, error) {
	if c == nil {
		return antonRewardPVFResolved{}, errAntonRewardPVFCatalogRequired
	}
	if rng == nil {
		return antonRewardPVFResolved{}, errAntonRewardPVFRNGRequired
	}
	pool, found := c.Pool(phaseIndex, kind)
	if !found || len(pool.Entries) == 0 || pool.TotalWeight == 0 {
		return antonRewardPVFResolved{}, fmt.Errorf("%w: phase=%d kind=%q", errAntonRewardPVFPoolUnresolved, phaseIndex, kind)
	}
	selected, totalWeight := c.selectPoolEntry(pool, rng.Uint64())
	if totalWeight == 0 {
		return antonRewardPVFResolved{}, fmt.Errorf("%w: phase=%d kind=%q all weights are zero after override", errAntonRewardPVFPoolUnresolved, phaseIndex, kind)
	}
	resolved, err := c.ResolveDefinition(selected.DefinitionItemID, rng)
	if err != nil {
		return antonRewardPVFResolved{}, err
	}
	resolved.PhaseIndex = phaseIndex
	resolved.PoolKind = pool.Kind
	resolved.PoolWeight = selected.Weight
	resolved.Rank = selected.Rank
	resolved.Flag = selected.Flag
	return resolved, nil
}

// ResolvePoolRewards is the raid-settlement form of ResolvePool. It keeps the
// historical [upgradable legacy] behavior (one weighted final item), while a
// [booster] definition is opened server-side: every independent [booster info]
// group contributes its DrawCount weighted picks and no box item is granted.
func (c *antonRewardPVFCatalog) ResolvePoolRewards(
	phaseIndex int,
	kind string,
	rng antonRewardPVFRNG,
	member runtimeRaidMemberState,
) ([]antonRewardPVFResolved, error) {
	if c == nil {
		return nil, errAntonRewardPVFCatalogRequired
	}
	if rng == nil {
		return nil, errAntonRewardPVFRNGRequired
	}
	pool, found := c.Pool(phaseIndex, kind)
	if !found || len(pool.Entries) == 0 || pool.TotalWeight == 0 {
		return nil, fmt.Errorf("%w: phase=%d kind=%q", errAntonRewardPVFPoolUnresolved, phaseIndex, kind)
	}
	selected, totalWeight := c.selectPoolEntry(pool, rng.Uint64())
	if totalWeight == 0 {
		return nil, fmt.Errorf("%w: phase=%d kind=%q all weights are zero after override", errAntonRewardPVFPoolUnresolved, phaseIndex, kind)
	}
	rewards, err := c.ResolveDefinitionRewards(selected.DefinitionItemID, rng, member)
	if err != nil {
		return nil, err
	}
	for index := range rewards {
		rewards[index].PhaseIndex = phaseIndex
		rewards[index].PoolKind = pool.Kind
		rewards[index].PoolWeight = selected.Weight
		rewards[index].Rank = selected.Rank
		rewards[index].Flag = selected.Flag
	}
	return rewards, nil
}

func (c *antonRewardPVFCatalog) ResolveDefinitionRewards(
	definitionItemID uint32,
	rng antonRewardPVFRNG,
	member runtimeRaidMemberState,
) ([]antonRewardPVFResolved, error) {
	if c == nil {
		return nil, errAntonRewardPVFCatalogRequired
	}
	if rng == nil {
		return nil, errAntonRewardPVFRNGRequired
	}
	definition, found := c.definitions[definitionItemID]
	if !found || len(definition.Entries) == 0 || definition.TotalWeight == 0 {
		return nil, fmt.Errorf("%w: definition=%d", errAntonRewardPVFDefinitionMissing, definitionItemID)
	}
	isBooster := definition.RewardType == "booster" || (definition.RewardType == "" && definition.DirectFinal)
	if !isBooster {
		resolved, err := c.ResolveDefinition(definitionItemID, rng)
		if err != nil {
			return nil, err
		}
		return []antonRewardPVFResolved{resolved}, nil
	}
	return c.resolveBoosterDefinitionRewards(definition, rng, member)
}

func (c *antonRewardPVFCatalog) resolveBoosterDefinitionRewards(
	definition antonRewardPVFDefinition,
	rng antonRewardPVFRNG,
	member runtimeRaidMemberState,
) ([]antonRewardPVFResolved, error) {
	text, err := c.source.ReadText(definition.PVFPath)
	if err != nil {
		return nil, fmt.Errorf("read raid booster definition=%d path=%s: %w", definition.DefinitionItemID, definition.PVFPath, err)
	}
	document, err := dnfpvf.Parse(definition.PVFPath, text)
	if err != nil {
		return nil, fmt.Errorf("parse raid booster definition=%d path=%s: %w", definition.DefinitionItemID, definition.PVFPath, err)
	}
	groups := parseMagicBoxBoosterGroups(document)
	if len(groups) == 0 {
		return nil, fmt.Errorf("%w: definition=%d booster_info_empty", errAntonRewardPVFDefinitionMissing, definition.DefinitionItemID)
	}
	character := dnfrepo.CharacterRecord{
		Job:   strconv.Itoa(int(member.Job)),
		Level: int(member.Level),
	}
	groups = alignedcmd.MagicBoxGroupsForCharacter(groups, character)
	if len(groups) == 0 {
		return nil, fmt.Errorf("%w: definition=%d booster_has_no_matching_character_group", errAntonRewardPVFDefinitionMissing, definition.DefinitionItemID)
	}

	rewards := make([]antonRewardPVFResolved, 0, len(groups))
	for groupIndex, group := range groups {
		entries := make([]alignedcmd.MagicBoxRewardEntry, 0, len(group.Entries))
		var totalWeight uint64
		for _, entry := range group.Entries {
			if entry.ItemID <= 0 || entry.ItemID > math.MaxUint32 || entry.Weight <= 0 || entry.Count <= 0 {
				continue
			}
			if _, err := c.finalItemReference(uint32(entry.ItemID)); err != nil {
				continue
			}
			if math.MaxUint64-totalWeight < uint64(entry.Weight) {
				return nil, fmt.Errorf("%w: definition=%d group=%d total_weight_overflow", errAntonRewardPVFValueInvalid, definition.DefinitionItemID, groupIndex)
			}
			entries = append(entries, entry)
			totalWeight += uint64(entry.Weight)
		}
		if totalWeight == 0 {
			return nil, fmt.Errorf("%w: definition=%d group=%d has no resolvable weighted entries", errAntonRewardPVFDefinitionMissing, definition.DefinitionItemID, groupIndex)
		}
		drawCount := group.DrawCount
		if drawCount < 1 {
			drawCount = 1
		}
		if drawCount > currentBoosterMaxRewardRows ||
			len(rewards)+int(drawCount) > currentBoosterMaxRewardRows {
			return nil, fmt.Errorf("%w: definition=%d group=%d draw_count=%d", errAntonRewardPVFValueInvalid, definition.DefinitionItemID, groupIndex, drawCount)
		}
		for draw := int64(0); draw < drawCount; draw++ {
			roll := rng.Uint64() % totalWeight
			var cumulative uint64
			selected := alignedcmd.MagicBoxRewardEntry{}
			for _, entry := range entries {
				cumulative += uint64(entry.Weight)
				if roll < cumulative {
					selected = entry
					break
				}
			}
			if selected.ItemID <= 0 {
				return nil, fmt.Errorf("%w: definition=%d group=%d roll=%d", errAntonRewardPVFPoolUnresolved, definition.DefinitionItemID, groupIndex, roll)
			}
			rewards = append(rewards, antonRewardPVFResolved{
				DefinitionItemID:  definition.DefinitionItemID,
				DefinitionPVFPath: definition.PVFPath,
				ItemID:            uint32(selected.ItemID),
				Count:             uint32(selected.Count),
				ItemKind:          c.itemRefs[uint32(selected.ItemID)].kind,
				ItemPVFPath:       c.itemRefs[uint32(selected.ItemID)].path,
			})
		}
	}
	return rewards, nil
}

func (c *antonRewardPVFCatalog) ResolveDefinition(definitionItemID uint32, rng antonRewardPVFRNG) (antonRewardPVFResolved, error) {
	if c == nil {
		return antonRewardPVFResolved{}, errAntonRewardPVFCatalogRequired
	}
	if rng == nil {
		return antonRewardPVFResolved{}, errAntonRewardPVFRNGRequired
	}
	definition, found := c.definitions[definitionItemID]
	if !found || len(definition.Entries) == 0 || definition.TotalWeight == 0 {
		return antonRewardPVFResolved{}, fmt.Errorf("%w: definition=%d", errAntonRewardPVFDefinitionMissing, definitionItemID)
	}
	selected := antonRewardPVFEntry{ItemID: definition.DefinitionItemID, Weight: 1, Count: 1}
	if !definition.DirectFinal {
		var selectionErr error
		selected, selectionErr = c.selectDefinitionEntry(definition, rng.Uint64())
		if selectionErr != nil {
			return antonRewardPVFResolved{}, selectionErr
		}
	}
	count := selected.Count
	resolved := antonRewardPVFResolved{
		PhaseIndex:        -1,
		DefinitionItemID:  definitionItemID,
		DefinitionPVFPath: definition.PVFPath,
	}
	if definition.DirectFinal {
		count = 1
	}
	if nested, isNested := c.definitions[selected.ItemID]; isNested {
		if nested.DirectFinal {
			resolved.NestedDefinitionItemID = nested.DefinitionItemID
			resolved.NestedDefinitionPVFPath = nested.PVFPath
			selected = antonRewardPVFEntry{ItemID: nested.DefinitionItemID, Weight: 1, Count: count}
		} else {
			nestedSelected, err := c.selectDefinitionEntry(nested, rng.Uint64())
			if err != nil {
				return antonRewardPVFResolved{}, err
			}
			if _, deeper := c.definitions[nestedSelected.ItemID]; deeper {
				return antonRewardPVFResolved{}, fmt.Errorf("%w: definition=%d nested=%d deeper=%d", errAntonRewardPVFNestingInvalid, definitionItemID, nested.DefinitionItemID, nestedSelected.ItemID)
			}
			count64 := uint64(count) * uint64(nestedSelected.Count)
			if count64 == 0 || count64 > math.MaxUint32 {
				return antonRewardPVFResolved{}, fmt.Errorf("%w: definition=%d nested=%d count=%d", errAntonRewardPVFValueInvalid, definitionItemID, nested.DefinitionItemID, count64)
			}
			resolved.NestedDefinitionItemID = nested.DefinitionItemID
			resolved.NestedDefinitionPVFPath = nested.PVFPath
			selected = nestedSelected
			count = uint32(count64)
		}
	}

	if stillDefinition, exists := c.definitions[selected.ItemID]; exists && !stillDefinition.DirectFinal {
		return antonRewardPVFResolved{}, fmt.Errorf("%w: definition=%d final=%d", errAntonRewardPVFNestingInvalid, definitionItemID, selected.ItemID)
	}
	finalReference, err := c.finalItemReference(selected.ItemID)
	if err != nil {
		return antonRewardPVFResolved{}, err
	}
	finalPath, err := antonRewardPVFReferencePath(finalReference)
	if err != nil {
		return antonRewardPVFResolved{}, err
	}
	resolved.ItemID = selected.ItemID
	resolved.Count = count
	resolved.ItemKind = finalReference.kind
	resolved.ItemPVFPath = finalPath
	return resolved, nil
}

func (c *antonRewardPVFCatalog) poolEntryWeight(pool antonRewardPVFPool, index int, entry antonRewardPVFPoolEntry) uint64 {
	weight := uint64(entry.Weight)
	if c == nil || c.overrides == nil {
		return weight
	}
	if percent, found := c.overrides.poolPercent(c.raid, pool.PhaseIndex, pool.Kind, entry.DefinitionItemID, index); found {
		weight = weight * uint64(percent) / 100
	}
	return weight
}

func (c *antonRewardPVFCatalog) selectPoolEntry(pool antonRewardPVFPool, random uint64) (antonRewardPVFPoolEntry, uint64) {
	weights := make([]uint64, len(pool.Entries))
	var total uint64
	for index, entry := range pool.Entries {
		weights[index] = c.poolEntryWeight(pool, index, entry)
		total += weights[index]
	}
	if total == 0 {
		return antonRewardPVFPoolEntry{}, 0
	}
	want := random % total
	var cumulative uint64
	for index, entry := range pool.Entries {
		cumulative += weights[index]
		if want < cumulative {
			return entry, total
		}
	}
	return pool.Entries[len(pool.Entries)-1], total
}

func (c *antonRewardPVFCatalog) definitionEntryWeight(definition antonRewardPVFDefinition, index int, entry antonRewardPVFEntry) uint64 {
	weight := uint64(entry.Weight)
	if c == nil || c.overrides == nil {
		return weight
	}
	if percent, found := c.overrides.itemPercent(c.raid, definition.DefinitionItemID, entry.ItemID, index); found {
		weight = weight * uint64(percent) / 100
	}
	return weight
}

func (c *antonRewardPVFCatalog) selectDefinitionEntry(definition antonRewardPVFDefinition, random uint64) (antonRewardPVFEntry, error) {
	weights := make([]uint64, len(definition.Entries))
	var total uint64
	for index, entry := range definition.Entries {
		weights[index] = c.definitionEntryWeight(definition, index, entry)
		total += weights[index]
	}
	if total == 0 {
		return antonRewardPVFEntry{}, fmt.Errorf("%w: definition=%d all weights are zero after override", errAntonRewardPVFDefinitionMissing, definition.DefinitionItemID)
	}
	want := random % total
	var cumulative uint64
	for index, entry := range definition.Entries {
		cumulative += weights[index]
		if want < cumulative {
			return entry, nil
		}
	}
	return definition.Entries[len(definition.Entries)-1], nil
}

// ResolveFinalItem expands metadata for one already selected final item. The
// underlying resolver opens only that item's indexed document and caches it;
// it never initializes the generic dungeon-drop pools.
func (c *antonRewardPVFCatalog) ResolveFinalItem(itemID uint32) (dungeonDropItemDefinition, error) {
	if c == nil || c.source == nil {
		return dungeonDropItemDefinition{}, errAntonRewardPVFCatalogRequired
	}
	if definition, isDefinition := c.definitions[itemID]; isDefinition && !definition.DirectFinal {
		return dungeonDropItemDefinition{}, fmt.Errorf("%w: definition=%d is not final", errAntonRewardPVFNestingInvalid, itemID)
	}
	reference, err := c.finalItemReference(itemID)
	if err != nil {
		return dungeonDropItemDefinition{}, err
	}
	exactPath, err := antonRewardPVFReferencePath(reference)
	if err != nil {
		return dungeonDropItemDefinition{}, err
	}
	c.finalMu.Lock()
	defer c.finalMu.Unlock()
	if cached, found := c.finalCache[itemID]; found {
		return cached, nil
	}
	// Supplying the already rooted path makes ResolveItem's first and only
	// successful read the exact selected document. The one-item catalog cannot
	// initialize monster or generic item pools.
	single := &pvfDungeonDropCatalog{
		source: c.source,
		itemRefs: map[uint32]dungeonDropItemReference{
			itemID: {kind: reference.kind, path: exactPath},
		},
		itemCache: make(map[uint32]dungeonDropItemDefinition, 1),
	}
	definition, err := single.ResolveItem(itemID)
	if err != nil {
		return dungeonDropItemDefinition{}, fmt.Errorf("resolve Anton final item=%d: %w", itemID, err)
	}
	c.finalCache[itemID] = definition
	return definition, nil
}

func (c *antonRewardPVFCatalog) Snapshot() antonRewardPVFCatalogSnapshot {
	if c == nil {
		return antonRewardPVFCatalogSnapshot{}
	}
	snapshot := antonRewardPVFCatalogSnapshot{Definitions: len(c.definitions), PhasePools: len(c.pools)}
	for _, definition := range c.definitions {
		snapshot.DefinitionEntries += len(definition.Entries)
		for _, entry := range definition.Entries {
			if _, nested := c.definitions[entry.ItemID]; nested {
				snapshot.NestedReferences++
			}
		}
	}
	for _, pool := range c.pools {
		snapshot.PhasePoolEntries += len(pool.Entries)
	}
	return snapshot
}

func selectAntonRewardPVFPoolEntry(pool antonRewardPVFPool, random uint64) antonRewardPVFPoolEntry {
	want := random % pool.TotalWeight
	var cumulative uint64
	for _, entry := range pool.Entries {
		cumulative += uint64(entry.Weight)
		if want < cumulative {
			return entry
		}
	}
	return pool.Entries[len(pool.Entries)-1]
}

func selectAntonRewardPVFDefinitionEntry(definition antonRewardPVFDefinition, random uint64) antonRewardPVFEntry {
	want := random % definition.TotalWeight
	var cumulative uint64
	for _, entry := range definition.Entries {
		cumulative += uint64(entry.Weight)
		if want < cumulative {
			return entry
		}
	}
	return definition.Entries[len(definition.Entries)-1]
}

func antonRewardPVFReferencePath(reference dungeonDropItemReference) (string, error) {
	if reference.kind != dungeonDropItemStackable && reference.kind != dungeonDropItemEquipment {
		return "", fmt.Errorf("unsupported_kind=%q", reference.kind)
	}
	clean := cleanDungeonDropPath(reference.path)
	if clean == "" {
		return "", errors.New("empty_path")
	}
	root := string(reference.kind)
	if !strings.HasPrefix(strings.ToLower(clean), strings.ToLower(root)+"/") {
		clean = path.Join(root, clean)
	}
	clean = path.Clean(clean)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || strings.HasPrefix(clean, "/") {
		return "", fmt.Errorf("path_outside_item_root=%q", clean)
	}
	return clean, nil
}

func normalizeAntonRewardPVFPoolKind(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

func normalizeAntonRewardPVFSection(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}
