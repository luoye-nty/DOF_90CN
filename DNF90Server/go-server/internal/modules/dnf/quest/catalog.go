package quest

import (
	"context"
	"errors"
	"fmt"
	"math"
	"path"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"longheng.io/server/internal/modules/dnf/jobmap"
	"longheng.io/server/internal/modules/dnf/profession"
	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

const DefaultList = "n_quest/quest.lst"

var ErrCatalogEmpty = errors.New("dnf quest catalog is empty")

var (
	ErrQuestDefinitionMissing         = errors.New("dnf quest definition is missing")
	ErrQuestNotAcceptable             = errors.New("dnf quest is not acceptable for this character state")
	ErrQuestInitialTriggerUnsupported = errors.New("dnf quest initial trigger rule is not implemented")
	ErrQuestRewardUnsupported         = errors.New("dnf quest reward type is unsupported")
	ErrQuestRewardMalformed           = errors.New("dnf quest reward data is malformed")
	ErrQuestRewardSelectionInvalid    = errors.New("dnf quest reward selection is invalid")
)

// RewardItemRule preserves one exact PVF [reward int data] item rule. Job and
// grow-type filters are evaluated only after the selected character is known.
type RewardItemRule struct {
	ItemID       int64
	Count        int64
	HasJobFilter bool
	Job          int
	GrowType     int
}

// TargetCharacterRule preserves one PVF [target character] tuple:
// job tag, first-grow branch, and awakening stage.
type TargetCharacterRule struct {
	JobTag         string
	FirstGrowType  int
	AwakeningStage int
}

// QuestAnswerRequirement is one PVF [pre required quest answer] pair:
// completed question quest ID followed by its zero-based selected answer.
type QuestAnswerRequirement struct {
	QuestID int64
	Answer  int64
}

// MonsterRewardItemEntry preserves one PVF [monster reward item] 7-tuple:
// monster_code dungeon_id difficulty item_id count drop_rate max_stack.
// dungeon_id -1 and difficulty -1 are wildcards (86JP QuestDropProvider).
type MonsterRewardItemEntry struct {
	MonsterCode int64
	DungeonID   int64
	Difficulty  int64
	ItemID      int64
	Count       int64
	DropRate    int64
	MaxStack    int64
}

// EnemyRewardItemEntry preserves one PVF [enemy reward item] 8-tuple:
// enemy_code enemy_type dungeon_id difficulty item_id count drop_rate max_stack.
// enemy_type 2 = AI character, 3 = passive object (86JP QuestDropProvider).
type EnemyRewardItemEntry struct {
	EnemyCode  int64
	EnemyType  int64
	DungeonID  int64
	Difficulty int64
	ItemID     int64
	Count      int64
	DropRate   int64
	MaxStack   int64
}

// DungeonScope preserves one PVF [dungeon info] pair. A value of -1 is the
// archive's wildcard for the corresponding field.
type DungeonScope struct {
	DungeonID  int64
	Difficulty int64
}

// SeekingItemTarget is one (item id, required count) pair from a plain
// [seeking] quest's [int data].
type SeekingItemTarget struct {
	ItemID int64
	Count  int64
}

type FinishRewardPlan struct {
	QuestID         int64
	PVFPath         string
	QuestLevel      int
	Difficulty      rune
	IgnoreLevel4Exp bool
	HasGoldReward   bool
	GoldMultiple    int
	// GoldCost 是这次交付要**扣**的金币（PVF [int data] 里 ID 0 的那一项）。
	//
	// 与 HasGoldReward/GoldMultiple 是完全不同的两件事：后者是"完成任务发多少金币"
	// （由 [reward int data] 的标记 + 等级表算），本字段是"完成任务要交多少金币"。
	// 服主现场 2026-10-03：所有消耗金币的任务都不扣钱，就是因为 (0, 金额) 这条 pair
	// 被当成非法 itemID 丢掉了。0 表示这个任务不要金币。
	GoldCost            int64
	Items               []RewardItemRule
	RewardSelectionUsed bool
	ProfessionRequest   profession.Request
	Profession          profession.Transition
	HasProfession       bool
	ExpertJobType       byte
	HasExpertJob        bool
	HasSlotExpansion    bool
	SlotExpansionIndex  uint32
	SlotExpansionBit    byte
	HasHellPartyUnlock  bool
	// InfernoChallengeDungeonID is the exact [inferno challenge] reward target.
	// Completing it is the durable permission proof for a PVF [inferno dungeon].
	InfernoChallengeDungeonID int64
}

// HasHellPartyUnlock reports whether a quest reward has the exact PVF
// [hell challenge] form that permits a region unlock. It does not plan the
// reward, so callers can load the world-map only for this reward class.
func (c *Catalog) HasHellPartyUnlock(questID int64) bool {
	definition, ok := c.Find(questID)
	return ok && normalizeQuestTag(definition.RewardType) == "hell challenge" &&
		len(definition.RewardIntData) == 1 && definition.RewardIntData[0] == 1
}

// Definition is the subset of a PVF quest definition required to decide
// whether the quest belongs in ENUM_NOTIPACKET_ACCEPTABLE_QUEST_LIST.
type Definition struct {
	ID                     int64
	Path                   string
	Name                   string
	DependMessage          string
	ConditionMessage       string
	SolveMessage           string
	Grade                  string
	Type                   string
	SubType                int
	IntData                []int64
	CheckCount             []int64
	ConditionData          []int64
	DependGiveItemData     []int64
	HasDependGiveItem      bool
	CantGiveUp             bool
	LevelMin               int
	LevelMax               int
	Job                    string
	TargetCharacter        string
	TargetCharacterRules   []TargetCharacterRule
	GrowType               int
	HasGrowType            bool
	JobChangeQuest         int
	ExposedByNPC           int64
	IsEvent                bool
	HasCreatureRequirement bool
	HasExpertRequirement   bool
	MainQuestID            int64
	PreRequiredGroups      [][]int64
	PreRequiredAnswers     []QuestAnswerRequirement
	MalformedPreAnswers    bool
	CollisionQuests        []int64
	Difficulty             string
	IgnoreQuestLevel4Exp   bool
	NoExperience           bool
	RewardType             string
	RewardIntData          []int64
	HasGoldReward          bool
	GoldMultiple           int
	RewardItems            []RewardItemRule
	RewardSelectionItems   []RewardItemRule
	RewardParseError       string
	MonsterRewardItems     []MonsterRewardItemEntry
	EnemyRewardItems       []EnemyRewardItemEntry
	DungeonScopes          []DungeonScope
}

type Catalog struct {
	ordered                    []Definition
	byID                       map[int64]Definition
	scopeHints                 []Definition
	externalProfessionPieces   []Definition
	storyAwakeningByCanonical  map[int64][]int64
	storyAwakeningCanonicalFor map[int64]int64
}

type Snapshot struct {
	Definitions int
	Epic        int
	Normal      int
	ScopeHints  int
}

type CharacterEligibility struct {
	Level    int
	Job      int
	GrowType int
	Wedding  bool
}

// WeddingEligible exposes marriage preparation only while the character is engaged.
func WeddingEligible(character dnfrepo.CharacterRecord) bool {
	return character.Stats["wedding_state"] == 1 && character.Stats["wedding_id"] > 0 &&
		character.Stats["wedding_partner_id"] > 0 && character.Stats["delete_flag"] == 0
}

type EligibilityResult struct {
	IDs         []int32
	EpicCount   int
	ActiveCount int
}

type AcceptPlan struct {
	QuestID         int64
	Path            string
	Type            string
	InitTrigger     uint32
	EventItems      []RewardItemRule
	LinkedSubQuests []AcceptLinkedSubQuestPlan
}

// AcceptMutationPlan is the repository-ready state transition for callers
// that must combine quest acceptance with another durable asset mutation.
// Ordinary op31 acceptance continues to use Owner; story-book use consumes
// its source item and applies this plan in one CharacterSettlement.
type AcceptMutationPlan struct {
	Quest       dnfrepo.QuestRecord
	QuestFields []dnfrepo.QuestField
	QuestID     int64
	InitTrigger uint32
}

type AcceptLinkedSubQuestPlan struct {
	QuestID     int64
	Path        string
	Type        string
	InitTrigger uint32
}

// MeetingNPCTrigger describes one current-client op33 transition that becomes
// eligible when the selected character reaches a PVF-proven town NPC. Compound
// [seek n meet npc] quests intentionally expose the item probe before the NPC
// channel so an already-satisfied inventory requirement reaches terminal zero
// in the same order as the native client.
type MeetingNPCTrigger struct {
	QuestID     int64
	TriggerType byte
}

func Load(ctx context.Context, index *dnfpvf.Index) (*Catalog, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	entries := index.List(DefaultList)
	if len(entries) == 0 {
		return nil, ErrCatalogEmpty
	}
	catalog := &Catalog{
		ordered:                    make([]Definition, 0, len(entries)),
		byID:                       make(map[int64]Definition, len(entries)),
		storyAwakeningByCanonical:  make(map[int64][]int64),
		storyAwakeningCanonicalFor: make(map[int64]int64),
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if entry.ID <= 0 || entry.ID > math.MaxInt32 {
			continue
		}
		if _, exists := catalog.byID[entry.ID]; exists {
			continue
		}
		doc, ok := index.Document(entry.Path)
		if !ok {
			continue
		}
		definition := parseDefinition(entry, doc)
		catalog.ordered = append(catalog.ordered, definition)
		catalog.byID[definition.ID] = definition
	}
	if len(catalog.ordered) == 0 {
		return nil, ErrCatalogEmpty
	}
	listedPaths := make(map[string]struct{}, len(catalog.ordered))
	for _, definition := range catalog.ordered {
		listedPaths[normalizeQuestPath(definition.Path)] = struct{}{}
	}
	for _, docPath := range index.Paths() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !strings.EqualFold(path.Ext(docPath), ".qst") {
			continue
		}
		if _, listed := listedPaths[normalizeQuestPath(docPath)]; listed {
			continue
		}
		doc, ok := index.Document(docPath)
		if !ok {
			continue
		}
		hint := parseDefinition(dnfpvf.ListEntry{Path: docPath}, doc)
		if isExternalProfessionQuestPiece(docPath, doc, hint) {
			catalog.externalProfessionPieces = append(catalog.externalProfessionPieces, hint)
		}
		if len(doc.Ints("dungeon info")) < 2 {
			continue
		}
		if normalizeQuestTag(hint.Type) == "seeking" && hint.Name != "" && len(hint.DungeonScopes) != 0 &&
			len(seekingItemTargets(hint)) != 0 {
			catalog.scopeHints = append(catalog.scopeHints, hint)
		}
	}
	catalog.buildStoryAwakeningAliases()
	return catalog, nil
}

func isExternalProfessionQuestPiece(docPath string, doc *dnfpvf.Document, definition Definition) bool {
	if doc == nil || !strings.HasPrefix(normalizeQuestPath(docPath), "n_quest/time_piece/") || len(doc.Ints("quest piece")) == 0 ||
		len(definition.PreRequiredGroups) == 0 {
		return false
	}
	return jobmap.MatchesQuestTag(definition.TargetCharacter, 9) ||
		jobmap.MatchesQuestTag(definition.TargetCharacter, 10)
}

// ExternalProfessionQuestPiecePrerequisites returns the legacy completion
// gates used by the current client to reveal Dark Knight and Creator time-piece
// rows. The corresponding .qst documents remain in the current PVF but were
// removed from quest.lst, so they cannot be accepted as ordinary server-owned
// quests. Projecting only their PVF-declared prerequisites preserves the
// client-owned progression UI without inventing quest IDs or completing the
// separate awakening entries.
func (c *Catalog) ExternalProfessionQuestPiecePrerequisites(character CharacterEligibility) []int64 {
	if c == nil || (character.Job != 9 && character.Job != 10) || character.Level <= 0 {
		return nil
	}
	seen := make(map[int64]struct{})
	for _, definition := range c.externalProfessionPieces {
		if character.Level < definition.LevelMin || character.Level > definition.LevelMax ||
			!definitionMatchesCharacter(definition, character) {
			continue
		}
		for _, group := range definition.PreRequiredGroups {
			for _, questID := range group {
				if questID > 0 && questID <= math.MaxInt32 {
					seen[questID] = struct{}{}
				}
			}
		}
	}
	ids := make([]int64, 0, len(seen))
	for questID := range seen {
		ids = append(ids, questID)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

func (c *Catalog) Snapshot() Snapshot {
	if c == nil {
		return Snapshot{}
	}
	snapshot := Snapshot{Definitions: len(c.ordered), ScopeHints: len(c.scopeHints)}
	for _, definition := range c.ordered {
		switch normalizeQuestTag(definition.Grade) {
		case "epic":
			snapshot.Epic++
		case "", "normal":
			snapshot.Normal++
		}
	}
	return snapshot
}

// Definitions returns the PVF quest definitions in quest.lst order. The
// returned values are deep clones, so offline analyzers cannot mutate the
// catalog used by live quest handlers.
func (c *Catalog) Definitions() []Definition {
	if c == nil || len(c.ordered) == 0 {
		return nil
	}
	out := make([]Definition, len(c.ordered))
	for index, definition := range c.ordered {
		out[index] = cloneDefinition(definition)
	}
	return out
}

// Search returns a bounded, PVF-order-stable view for the local GM browser.
func (c *Catalog) Search(query string, limit int) []Definition {
	if c == nil {
		return nil
	}
	if limit <= 0 {
		limit = 30
	}
	if limit > 200 {
		limit = 200
	}
	query = strings.ToLower(strings.TrimSpace(query))
	numericID, numericErr := strconv.ParseInt(query, 10, 64)
	out := make([]Definition, 0, limit)
	for _, definition := range c.ordered {
		if query != "" && !(numericErr == nil && definition.ID == numericID) &&
			!strings.Contains(strings.ToLower(definition.Name), query) &&
			!strings.Contains(strings.ToLower(definition.Path), query) {
			continue
		}
		out = append(out, cloneDefinition(definition))
		if len(out) >= limit {
			break
		}
	}
	return out
}

// MatchesCharacter exposes the same PVF job/grow-type predicate used by task
// acceptance. It intentionally does not evaluate level, prerequisites, or
// persisted state.
func (c *Catalog) MatchesCharacter(questID int64, character CharacterEligibility) bool {
	if c == nil {
		return false
	}
	definition, ok := c.byID[questID]
	return ok && definitionMatchesCharacter(definition, character)
}

// buildStoryAwakeningAliases identifies PVF story nodes where the client
// presents one canonical epic task while a mutually-exclusive profession
// sibling owns the first-awakening reward. The match is intentionally strict:
// ambiguous or partially-defined pairs remain independent definitions.
func (c *Catalog) buildStoryAwakeningAliases() {
	if c == nil {
		return
	}
	if c.storyAwakeningByCanonical == nil {
		c.storyAwakeningByCanonical = make(map[int64][]int64)
	}
	if c.storyAwakeningCanonicalFor == nil {
		c.storyAwakeningCanonicalFor = make(map[int64]int64)
	}
	for _, variant := range c.ordered {
		if !firstAwakeningStoryVariant(variant) {
			continue
		}
		matches := make([]int64, 0, 1)
		for _, collisionID := range variant.CollisionQuests {
			canonical, found := c.byID[collisionID]
			if found && storyAwakeningDefinitionsMatch(canonical, variant) {
				matches = append(matches, canonical.ID)
			}
		}
		if len(matches) != 1 {
			continue
		}
		canonicalID := matches[0]
		c.storyAwakeningByCanonical[canonicalID] = append(c.storyAwakeningByCanonical[canonicalID], variant.ID)
		c.storyAwakeningCanonicalFor[variant.ID] = canonicalID
	}
	for canonicalID := range c.storyAwakeningByCanonical {
		slices.Sort(c.storyAwakeningByCanonical[canonicalID])
	}
}

func firstAwakeningStoryVariant(definition Definition) bool {
	return normalizeQuestTag(definition.Grade) == "epic" && definition.JobChangeQuest == 2 &&
		definition.HasGrowType && definition.GrowType > 0 && definition.GrowType <= 0x0f &&
		normalizeQuestTag(definition.RewardType) == "awakening type" &&
		len(definition.RewardIntData) == 1 && definition.RewardIntData[0] == 1 &&
		len(definition.RewardItems) == 0 && len(definition.RewardSelectionItems) == 0 && !definition.HasGoldReward
}

func storyAwakeningDefinitionsMatch(canonical, variant Definition) bool {
	if normalizeQuestTag(canonical.Grade) != "epic" || canonical.JobChangeQuest != 0 ||
		normalizeQuestTag(canonical.RewardType) != "item" || canonical.HasGoldReward ||
		!emptyRewardDataValid(canonical.RewardIntData) || len(canonical.RewardItems) != 0 ||
		len(canonical.RewardSelectionItems) != 0 || len(canonical.TargetCharacterRules) == 0 ||
		!slices.Contains(canonical.CollisionQuests, variant.ID) || !slices.Contains(variant.CollisionQuests, canonical.ID) ||
		normalizeQuestTag(canonical.Type) != normalizeQuestTag(variant.Type) || canonical.SubType != variant.SubType ||
		canonical.LevelMin != variant.LevelMin || canonical.LevelMax != variant.LevelMax ||
		normalizeQuestTag(canonical.Difficulty) != normalizeQuestTag(variant.Difficulty) ||
		canonical.MainQuestID != variant.MainQuestID || !slices.Equal(canonical.IntData, variant.IntData) ||
		!slices.Equal(canonical.CheckCount, variant.CheckCount) ||
		!slices.Equal(canonical.ConditionData, variant.ConditionData) ||
		!equalIntGroups(canonical.PreRequiredGroups, variant.PreRequiredGroups) {
		return false
	}
	variantJob := strings.ToLower(strings.TrimSpace(variant.Job))
	for _, rule := range canonical.TargetCharacterRules {
		if rule.FirstGrowType == variant.GrowType && rule.AwakeningStage == 1 &&
			strings.Contains(variantJob, "["+normalizeQuestTag(rule.JobTag)+"]") {
			return true
		}
	}
	return false
}

func equalIntGroups(left, right [][]int64) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if !slices.Equal(left[index], right[index]) {
			return false
		}
	}
	return true
}

func (c *Catalog) storyAwakeningCompanion(canonical Definition, character CharacterEligibility) (Definition, bool) {
	if c == nil || (character.GrowType>>4)&0x0f != 0 {
		return Definition{}, false
	}
	firstGrowType := character.GrowType & 0x0f
	if firstGrowType == 0 {
		return Definition{}, false
	}
	for _, variantID := range c.storyAwakeningByCanonical[canonical.ID] {
		variant, found := c.byID[variantID]
		if found && variant.GrowType == firstGrowType && definitionMatchesCharacter(variant, character) {
			return cloneDefinition(variant), true
		}
	}
	return Definition{}, false
}

// FinishRewardDefinition resolves a parent task's final reward-bearing child
// without changing the client-facing quest ID. It is used by the settlement
// owner to select child materials/rewards and to archive the child atomically.
func (c *Catalog) FinishRewardDefinition(record dnfrepo.QuestRecord, character CharacterEligibility, questID int64) (Definition, []int64, bool) {
	parent, found := c.Find(questID)
	if !found {
		return Definition{}, nil, false
	}
	children, ok := c.parentBoundChildren(parent)
	if !ok || !zeroItemRewardPlaceholder(parent) {
		return parent, nil, false
	}
	parentState, parentActive := questStateFor(record, questID)
	if !parentActive || !isActiveQuestStatus(parentState.Status) || parentState.ProgressValue != 0 {
		return parent, nil, false
	}
	completed, _ := questStateSets(record)
	for index := 0; index < len(children)-1; index++ {
		child := children[index]
		if _, done := completed[child.ID]; done {
			continue
		}
		state, known := questStateFor(record, child.ID)
		if !known || !isActiveQuestStatus(state.Status) || state.ProgressValue != 0 || !linkedObjectiveQuest(child) {
			return parent, nil, false
		}
	}
	final := children[len(children)-1]
	if !rewardBearingParentChild(final) {
		return parent, nil, false
	}
	finalState, finalActive := questStateFor(record, final.ID)
	if !finalActive || !isActiveQuestStatus(finalState.Status) || finalState.ProgressValue != 0 ||
		!definitionMatchesCharacter(final, character) {
		return parent, nil, false
	}
	additional := c.completedLinkedChildrenForParentFinish(parent, record)
	additional = append(additional, final.ID)
	slices.Sort(additional)
	additional = slices.Compact(additional)
	return final, additional, true
}

// ExpertJobTransitionQuestIDs returns every quest explicitly assigned to the
// PVF expert-job transition chain, preserving quest-list order.
func (c *Catalog) ExpertJobTransitionQuestIDs() []int64 {
	if c == nil {
		return nil
	}
	ids := make([]int64, 0)
	seen := make(map[int64]struct{})
	for _, definition := range c.ordered {
		if definition.ID <= 0 || definition.JobChangeQuest != 20 {
			continue
		}
		if _, exists := seen[definition.ID]; exists {
			continue
		}
		seen[definition.ID] = struct{}{}
		ids = append(ids, definition.ID)
	}
	return ids
}

// ExpertJobTransitionTerminalQuestIDs returns transition quests that are not
// prerequisites of another transition quest in the same PVF chain.
func (c *Catalog) ExpertJobTransitionTerminalQuestIDs() []int64 {
	ids := c.ExpertJobTransitionQuestIDs()
	if len(ids) == 0 {
		return nil
	}
	members := make(map[int64]struct{}, len(ids))
	for _, questID := range ids {
		members[questID] = struct{}{}
	}
	nonTerminal := make(map[int64]struct{})
	for _, questID := range ids {
		for _, successor := range c.Successors(questID) {
			if _, ok := members[successor.ID]; ok {
				nonTerminal[questID] = struct{}{}
				break
			}
		}
	}
	terminal := make([]int64, 0, len(ids))
	for _, questID := range ids {
		if _, exists := nonTerminal[questID]; !exists {
			terminal = append(terminal, questID)
		}
	}
	return terminal
}

func (c *Catalog) Find(id int64) (Definition, bool) {
	if c == nil {
		return Definition{}, false
	}
	definition, ok := c.byID[id]
	return cloneDefinition(definition), ok
}

// SeekingItemTargets returns the item/count pairs carried by a plain PVF
// [seeking] quest. Other quest types can use [int data] for unrelated tuple
// layouts, so they deliberately do not enter this fallback path.
func (c *Catalog) SeekingItemTargets(questID int64) []SeekingItemTarget {
	if c == nil {
		return nil
	}
	definition, ok := c.byID[questID]
	if !ok || normalizeQuestTag(definition.Type) != "seeking" {
		return nil
	}
	return seekingItemTargets(definition)
}

// SeekingDungeonScopes returns the explicit PVF [dungeon info] for a seeking
// quest. Event copies in the current archive sometimes omit that section while
// retaining the same name and target item as a regular quest. In that exact
// case, scopes are inherited from the matching sibling definition. Counts may
// differ between the event and regular copies and therefore are not part of
// the identity.
func (c *Catalog) SeekingDungeonScopes(questID int64) []DungeonScope {
	if c == nil {
		return nil
	}
	definition, ok := c.byID[questID]
	if !ok || normalizeQuestTag(definition.Type) != "seeking" {
		return nil
	}
	if len(definition.DungeonScopes) != 0 {
		return cloneDungeonScopes(definition.DungeonScopes)
	}
	name := normalizeQuestName(definition.Name)
	targetIDs := seekingItemTargetIDs(definition)
	if name == "" || len(targetIDs) == 0 {
		return nil
	}
	var scopes []DungeonScope
	seen := make(map[DungeonScope]struct{})
	siblings := make([]Definition, 0, len(c.ordered)+len(c.scopeHints))
	siblings = append(siblings, c.ordered...)
	siblings = append(siblings, c.scopeHints...)
	for _, sibling := range siblings {
		if sibling.ID == definition.ID || normalizeQuestTag(sibling.Type) != "seeking" ||
			normalizeQuestName(sibling.Name) != name ||
			!slices.Equal(seekingItemTargetIDs(sibling), targetIDs) {
			continue
		}
		for _, scope := range sibling.DungeonScopes {
			if scope.DungeonID <= 0 || scope.Difficulty < -1 {
				continue
			}
			if _, duplicate := seen[scope]; duplicate {
				continue
			}
			seen[scope] = struct{}{}
			scopes = append(scopes, scope)
		}
	}
	return scopes
}

func seekingItemTargets(definition Definition) []SeekingItemTarget {
	if len(definition.IntData) < 2 {
		return nil
	}
	targets := make([]SeekingItemTarget, 0, len(definition.IntData)/2)
	for index := 0; index+1 < len(definition.IntData); index += 2 {
		itemID, count := definition.IntData[index], definition.IntData[index+1]
		if itemID <= 0 || count <= 0 {
			continue
		}
		targets = append(targets, SeekingItemTarget{ItemID: itemID, Count: count})
	}
	return targets
}

func seekingItemTargetIDs(definition Definition) []int64 {
	targets := seekingItemTargets(definition)
	ids := make([]int64, len(targets))
	for index, target := range targets {
		ids[index] = target.ItemID
	}
	return ids
}

func normalizeQuestName(value string) string {
	return strings.ToLower(strings.Join(strings.Fields(value), " "))
}

func normalizeQuestPath(value string) string {
	return strings.ToLower(strings.TrimSpace(strings.ReplaceAll(value, "\\", "/")))
}

func cloneDungeonScopes(scopes []DungeonScope) []DungeonScope {
	return append([]DungeonScope(nil), scopes...)
}

// IsCompleted reports whether the repository's durable quest state contains a
// terminal completion for questID.  Entry systems use this rather than a
// synthetic unlock table when the active PVF itself names the prerequisite.
func IsCompleted(record dnfrepo.QuestRecord, questID int64) bool {
	if questID <= 0 {
		return false
	}
	if state, ok := record.States[questID]; ok && isCompletedQuestStatus(state.Status) {
		return true
	}
	if state, ok := record.Progress[questID]; ok && isCompletedQuestStatus(state.Status) {
		return true
	}
	return false
}

// ActiveMeetingNPCTriggers returns deterministic trigger transitions for
// active quests whose PVF objective names npcID. The repository record remains
// authoritative for active/completed ownership; the catalog supplies only the
// objective type and target NPC.
func (c *Catalog) ActiveMeetingNPCTriggers(record dnfrepo.QuestRecord, npcID int64) []MeetingNPCTrigger {
	if c == nil || npcID <= 0 {
		return nil
	}
	active := make(map[int64]dnfrepo.QuestState)
	collect := func(states map[int64]dnfrepo.QuestState) {
		for questID, state := range states {
			if questID > 0 && isActiveQuestStatus(state.Status) {
				active[questID] = state
			}
		}
	}
	collect(record.Progress)
	collect(record.States)
	if len(active) == 0 {
		return nil
	}
	questIDs := make([]int64, 0, len(active))
	for questID := range active {
		questIDs = append(questIDs, questID)
	}
	sort.Slice(questIDs, func(i, j int) bool { return questIDs[i] < questIDs[j] })

	triggers := make([]MeetingNPCTrigger, 0, len(questIDs))
	for _, questID := range questIDs {
		definition, ok := c.byID[questID]
		if !ok {
			continue
		}
		state := active[questID]
		switch normalizeQuestTag(definition.Type) {
		case "meet npc", "meet secret npc":
			if len(definition.IntData) >= 1 && definition.IntData[0] == npcID && state.ProgressValue > 0 {
				triggers = append(triggers, MeetingNPCTrigger{QuestID: questID})
			}
		case "seek n meet npc":
			if len(definition.IntData) >= 3 && definition.IntData[2] == npcID {
				progress := uint32(state.ProgressValue)
				if state.ProgressValue >= 0 && state.ProgressValue <= int64(^uint32(0)) && progress&0x1ff != 0 {
					triggers = append(triggers, MeetingNPCTrigger{QuestID: questID, TriggerType: 0x10})
				}
				if state.ProgressValue >= 0 && state.ProgressValue <= int64(^uint32(0)) && (progress>>9)&0x1ff != 0 {
					triggers = append(triggers, MeetingNPCTrigger{QuestID: questID, TriggerType: 0x20})
				}
			}
		}
	}
	return triggers
}

// Successors returns the definitions that list questID in any prerequisite
// group, preserving catalog order. It is the static PVF chain step used to
// resolve the next quest after a completed one; it does not inspect character
// quest state.
func (c *Catalog) Successors(questID int64) []Definition {
	if c == nil {
		return nil
	}
	var successors []Definition
	for _, definition := range c.ordered {
		for _, group := range definition.PreRequiredGroups {
			for _, prerequisite := range group {
				if prerequisite == questID {
					successors = append(successors, cloneDefinition(definition))
					goto nextDefinition
				}
			}
		}
		for _, requirement := range definition.PreRequiredAnswers {
			if requirement.QuestID == questID {
				successors = append(successors, cloneDefinition(definition))
				goto nextDefinition
			}
		}
	nextDefinition:
	}
	return successors
}

// EligibleSuccessors filters the static PVF successor relation through the
// same character and persisted quest-state rules used by the op21 task list.
// It intentionally includes an already active successor because settlement
// continuation may run after the client has accepted the next task.
func (c *Catalog) EligibleSuccessors(questID int64, character CharacterEligibility, record dnfrepo.QuestRecord) []Definition {
	if c == nil {
		return nil
	}
	completed, active := questStateSets(record)
	answers := questAnswerSelections(record)
	var successors []Definition
	for _, definition := range c.Successors(questID) {
		if c.definitionAcceptable(definition, character, completed, active, answers, true) {
			successors = append(successors, definition)
		}
	}
	return successors
}

func (c *Catalog) Acceptable(character CharacterEligibility, record dnfrepo.QuestRecord) EligibilityResult {
	return c.eligibility(character, record, false)
}

// QuestList returns the PVF-backed IDs carried by the current EXE's
// ENUM_NOTIPACKET_ACCEPTABLE_QUEST_LIST notification. The task manual's All
// view combines this definition list with the active-progress snapshot, so an
// already active quest must remain present here even though PlanAccept must
// continue to reject accepting it a second time.
func (c *Catalog) QuestList(character CharacterEligibility, record dnfrepo.QuestRecord) EligibilityResult {
	return c.eligibility(character, record, true)
}

// TitleAchievementDefinitions returns the automatic title-book achievements
// that the current character can see. Title quests are not ordinary
// acceptable quests: the client advances them through op417 without op31, so
// keep them out of QuestList/PlanAccept while still applying their PVF level,
// job, grow-type, and target-character gates.
func (c *Catalog) TitleAchievementDefinitions(character CharacterEligibility) []Definition {
	if c == nil || !jobmap.Valid(character.Job) {
		return nil
	}
	if character.Level <= 0 {
		character.Level = 1
	}
	definitions := make([]Definition, 0, 64)
	for _, definition := range c.ordered {
		if normalizeQuestTag(definition.Grade) != "title" ||
			character.Level < definition.LevelMin || character.Level > definition.LevelMax ||
			!definitionMatchesCharacter(definition, character) {
			continue
		}
		definitions = append(definitions, definition)
	}
	return definitions
}

func (c *Catalog) eligibility(character CharacterEligibility, record dnfrepo.QuestRecord, includeActive bool) EligibilityResult {
	if c == nil || !jobmap.Valid(character.Job) {
		return EligibilityResult{}
	}
	if character.Level <= 0 {
		character.Level = 1
	}
	completed, active := questStateSets(record)
	answers := questAnswerSelections(record)
	result := EligibilityResult{IDs: make([]int32, 0, 64)}
	for _, definition := range c.ordered {
		if !c.definitionAcceptable(definition, character, completed, active, answers, includeActive) {
			continue
		}
		result.IDs = append(result.IDs, int32(definition.ID))
		if _, exists := active[definition.ID]; exists {
			result.ActiveCount++
		}
		if normalizeQuestTag(definition.Grade) == "epic" {
			result.EpicCount++
		}
	}
	return result
}

// PlanAccept reuses the same PVF/character/database eligibility decision used
// for the acceptable-quest snapshot and derives only initial-trigger rules
// that are represented in the typed catalog. Acceptance-time event items stay
// in the plan so the owner can commit them with the quest state in one
// character-settlement transaction.
func (c *Catalog) PlanAccept(character CharacterEligibility, record dnfrepo.QuestRecord, questID int64) (AcceptPlan, error) {
	definition, ok := c.Find(questID)
	if !ok {
		return AcceptPlan{}, ErrQuestDefinitionMissing
	}
	if !jobmap.Valid(character.Job) {
		return AcceptPlan{}, ErrQuestNotAcceptable
	}
	if character.Level <= 0 {
		character.Level = 1
	}
	completed, active := questStateSets(record)
	answers := questAnswerSelections(record)
	if !c.definitionAcceptable(definition, character, completed, active, answers, false) {
		return AcceptPlan{}, ErrQuestNotAcceptable
	}
	eventItems, err := acceptEventItemRules(definition)
	if err != nil {
		return AcceptPlan{}, err
	}
	// Keep the accepted task's trigger in the same domain state as the C#
	// QuestData.ComputeInitTrigger implementation.  This is deliberately a
	// PVF/state calculation only; it does not infer any packet fields.
	trigger := definitionInitialTrigger(definition, completed)
	return AcceptPlan{
		QuestID:         definition.ID,
		Path:            definition.Path,
		Type:            definition.Type,
		InitTrigger:     trigger,
		EventItems:      eventItems,
		LinkedSubQuests: c.linkedSubQuestAcceptPlans(definition, character, completed, active, answers),
	}, nil
}

// PlanAcceptMutation applies the same PVF eligibility and initial-trigger
// rules as native op31 without writing a repository. Event-item quests remain
// owned by the normal quest accept settlement because this narrow path has no
// reward allocator.
func (c *Catalog) PlanAcceptMutation(
	character CharacterEligibility,
	record dnfrepo.QuestRecord,
	questID int64,
	acceptedAt time.Time,
) (AcceptMutationPlan, error) {
	definition, ok := c.Find(questID)
	if !ok {
		return AcceptMutationPlan{}, ErrQuestDefinitionMissing
	}
	plan, err := c.PlanAccept(character, record, questID)
	if err != nil {
		return AcceptMutationPlan{}, err
	}
	if len(plan.EventItems) != 0 {
		return AcceptMutationPlan{}, fmt.Errorf("%w: quest=%d story-book accept cannot allocate depend items", ErrQuestRewardUnsupported, questID)
	}
	if acceptedAt.IsZero() {
		acceptedAt = time.Now().UTC()
	}
	updated := dnfrepo.CloneQuest(record)
	applyAcceptPlan(&updated, plan, definition, record.CharacterID, acceptedAt)
	return AcceptMutationPlan{
		Quest:       updated,
		QuestFields: []dnfrepo.QuestField{dnfrepo.QuestFieldStates},
		QuestID:     plan.QuestID,
		InitTrigger: plan.InitTrigger,
	}, nil
}

func acceptEventItemRules(definition Definition) ([]RewardItemRule, error) {
	if !definition.HasDependGiveItem {
		return nil, nil
	}
	values := definition.DependGiveItemData
	if len(values) == 0 || len(values)%2 != 0 {
		return nil, fmt.Errorf("%w: quest=%d depend give item data=%v", ErrQuestRewardMalformed, definition.ID, values)
	}
	rules := make([]RewardItemRule, 0, len(values)/2)
	for index := 0; index < len(values); index += 2 {
		itemID, count := values[index], values[index+1]
		if itemID <= 0 || itemID > math.MaxUint32 || count <= 0 || count > math.MaxUint32 {
			return nil, fmt.Errorf("%w: quest=%d depend give item pair=(%d,%d)", ErrQuestRewardMalformed, definition.ID, itemID, count)
		}
		rules = append(rules, RewardItemRule{ItemID: itemID, Count: count})
	}
	return rules, nil
}

// PlanFinishReward parses the detached PVF reward for the current character.
// Profession rewards remain requests until the settlement owner resolves them
// through the job profile and current grow_type inside the repository UoW. It
// does not allocate slots, mutate repositories, calculate EXP, or emit an ACK.
func (c *Catalog) PlanFinishReward(character CharacterEligibility, questID int64, rewardSelectIndex uint16, hasRewardSelect bool) (FinishRewardPlan, error) {
	definition, ok := c.Find(questID)
	if !ok {
		return FinishRewardPlan{}, ErrQuestDefinitionMissing
	}
	if !jobmap.Valid(character.Job) {
		return FinishRewardPlan{}, ErrQuestNotAcceptable
	}
	if normalizeQuestTag(definition.Grade) == "wedding" && !character.Wedding {
		return FinishRewardPlan{}, ErrQuestNotAcceptable
	}
	// The client submits the canonical story task ID. When PVF declares a
	// unique profession collision sibling, use that sibling only as the reward
	// source; the settlement and ACK continue to use questID.
	if companion, active := c.storyAwakeningCompanion(definition, character); active {
		definition = companion
	}
	plan := FinishRewardPlan{
		QuestID:         questID,
		PVFPath:         definition.Path,
		QuestLevel:      definition.LevelMin,
		Difficulty:      'G',
		IgnoreLevel4Exp: definition.IgnoreQuestLevel4Exp,
		HasGoldReward:   definition.HasGoldReward,
		GoldMultiple:    definition.GoldMultiple,
		// 交付金币（[int data] 里的 ID 0）：与金币**奖励**互不相干，两者可以同时存在。
		GoldCost: finishDefinitionGoldCost(definition),
	}
	if difficulty := []rune(strings.TrimSpace(definition.Difficulty)); len(difficulty) == 1 {
		plan.Difficulty = difficulty[0]
	}
	rewardType := normalizeQuestTag(definition.RewardType)
	if rewardType == "" {
		if !emptyRewardDataValid(definition.RewardIntData) {
			return FinishRewardPlan{}, fmt.Errorf("%w: quest=%d empty reward type has data=%v", ErrQuestRewardMalformed, questID, definition.RewardIntData)
		}
		if hasRewardSelect {
			return FinishRewardPlan{}, fmt.Errorf("%w: quest=%d has no selectable reward", ErrQuestRewardSelectionInvalid, questID)
		}
		return plan, nil
	}
	if rewardType == "grow type" || rewardType == "awakening type" {
		request, err := profession.ParseReward(definition.JobChangeQuest, definition.RewardType, definition.RewardIntData)
		if err != nil {
			if errors.Is(err, profession.ErrRewardMalformed) {
				return FinishRewardPlan{}, fmt.Errorf("%w: quest=%d: %v", ErrQuestRewardMalformed, questID, err)
			}
			return FinishRewardPlan{}, fmt.Errorf("%w: quest=%d: %v", ErrQuestRewardUnsupported, questID, err)
		}
		plan.ProfessionRequest = request
		plan.HasProfession = true
		if hasRewardSelect {
			return FinishRewardPlan{}, fmt.Errorf("%w: quest=%d profession reward has no selection", ErrQuestRewardSelectionInvalid, questID)
		}
		return plan, nil
	}
	if rewardType == "expert job" {
		// The current PVF encodes the chosen auxiliary-profession type as one
		// positive byte (for example, quest 2710 supplies 3 for disjointer).
		// Current EXE op34 consumes that value under chain type 20.
		if len(definition.RewardIntData) != 1 || definition.RewardIntData[0] <= 0 || definition.RewardIntData[0] > math.MaxUint8 {
			return FinishRewardPlan{}, fmt.Errorf("%w: quest=%d expert job values=%v", ErrQuestRewardMalformed, questID, definition.RewardIntData)
		}
		if hasRewardSelect {
			return FinishRewardPlan{}, fmt.Errorf("%w: quest=%d expert job reward has no selection", ErrQuestRewardSelectionInvalid, questID)
		}
		plan.ExpertJobType = byte(definition.RewardIntData[0])
		plan.HasExpertJob = true
		return plan, nil
	}
	if rewardType == "slot expansion" {
		if hasRewardSelect {
			return FinishRewardPlan{}, fmt.Errorf("%w: quest=%d slot expansion reward has no selection", ErrQuestRewardSelectionInvalid, questID)
		}
		// The current runtime PVF stores the slot-expansion index in
		// [reward int data]: 0=support, 1=magic stone, 2=earring. The durable
		// character column and current global actor snapshots store the
		// corresponding bit.
		if len(definition.RewardIntData) != 1 || definition.RewardIntData[0] < 0 || definition.RewardIntData[0] > 2 {
			return FinishRewardPlan{}, fmt.Errorf("%w: quest=%d slot expansion values=%v", ErrQuestRewardMalformed, questID, definition.RewardIntData)
		}
		plan.HasSlotExpansion = true
		plan.SlotExpansionIndex = uint32(definition.RewardIntData[0])
		plan.SlotExpansionBit, _ = ExEquipSlotBitForPVFIndex(plan.SlotExpansionIndex)
		return plan, nil
	}
	if rewardType == "hell challenge" {
		if hasRewardSelect || len(definition.RewardIntData) != 1 || definition.RewardIntData[0] != 1 {
			return FinishRewardPlan{}, fmt.Errorf("%w: quest=%d hell challenge values=%v", ErrQuestRewardMalformed, questID, definition.RewardIntData)
		}
		plan.HasHellPartyUnlock = true
		return plan, nil
	}
	if rewardType == "inferno challenge" {
		if hasRewardSelect || len(definition.RewardIntData) != 1 || definition.RewardIntData[0] <= 0 {
			return FinishRewardPlan{}, fmt.Errorf("%w: quest=%d inferno challenge values=%v", ErrQuestRewardMalformed, questID, definition.RewardIntData)
		}
		plan.InfernoChallengeDungeonID = definition.RewardIntData[0]
		return plan, nil
	}
	if rewardType != "item" {
		return FinishRewardPlan{}, fmt.Errorf("%w: quest=%d type=%q", ErrQuestRewardUnsupported, questID, definition.RewardType)
	}
	if definition.RewardParseError != "" {
		return FinishRewardPlan{}, fmt.Errorf("%w: quest=%d: %s", ErrQuestRewardMalformed, questID, definition.RewardParseError)
	}
	for _, rule := range definition.RewardItems {
		if rewardRuleMatches(rule, character) {
			plan.Items = append(plan.Items, rule)
		}
	}
	selectable := make([]RewardItemRule, 0, len(definition.RewardSelectionItems))
	for _, rule := range definition.RewardSelectionItems {
		if rewardRuleMatches(rule, character) {
			selectable = append(selectable, rule)
		}
	}
	if len(selectable) == 0 {
		if hasRewardSelect {
			return FinishRewardPlan{}, fmt.Errorf("%w: quest=%d has no selectable reward", ErrQuestRewardSelectionInvalid, questID)
		}
		return plan, nil
	}
	if !hasRewardSelect || int(rewardSelectIndex) >= len(selectable) {
		return FinishRewardPlan{}, fmt.Errorf("%w: quest=%d selected=%d choices=%d", ErrQuestRewardSelectionInvalid, questID, rewardSelectIndex, len(selectable))
	}
	plan.Items = append(plan.Items, selectable[int(rewardSelectIndex)])
	plan.RewardSelectionUsed = true
	return plan, nil
}

// FinishRewardNeedsProfessionResources reports whether the reward that the
// settlement owner will actually parse needs the PVF profession tables. The
// client submits the canonical quest ID, but a story awakening companion (or
// a reward-bearing linked child) can be the final reward source. Keep this
// decision in the catalog so the bridge does not duplicate hidden reward
// routing rules.
func (c *Catalog) FinishRewardNeedsProfessionResources(
	record dnfrepo.QuestRecord,
	character CharacterEligibility,
	questID int64,
	rewardSelectIndex uint16,
	hasRewardSelect bool,
) bool {
	if c == nil {
		return false
	}
	rewardQuestID := questID
	if definition, _, child := c.FinishRewardDefinition(record, character, questID); child {
		rewardQuestID = definition.ID
	}
	plan, err := c.PlanFinishReward(character, rewardQuestID, rewardSelectIndex, hasRewardSelect)
	return err == nil && plan.HasProfession
}

func emptyRewardDataValid(values []int64) bool {
	for _, value := range values {
		if value != 0 {
			return false
		}
	}
	return true
}

func parseDefinition(entry dnfpvf.ListEntry, doc *dnfpvf.Document) Definition {
	definition := Definition{
		ID:           entry.ID,
		Path:         entry.Path,
		LevelMin:     1,
		LevelMax:     99,
		GrowType:     -1,
		ExposedByNPC: -1,
	}
	definition.Name, _ = doc.Text("name")
	definition.DependMessage = questPresentationText(doc, "depend message")
	definition.ConditionMessage = questPresentationText(doc, "condition message")
	definition.SolveMessage = questPresentationText(doc, "solve message")
	definition.Grade, _ = doc.Text("grade")
	definition.Type, _ = doc.Text("type")
	definition.Difficulty, _ = doc.Text("difficulty")
	definition.RewardType, _ = doc.Text("reward type")
	definition.RewardIntData = append([]int64(nil), doc.Ints("reward int data")...)
	if value, ok := doc.Int("gold multiple"); ok {
		definition.GoldMultiple = boundedInt(value)
	}
	if tokens, ok := doc.Section("ignore quest level 4 exp"); ok {
		definition.IgnoreQuestLevel4Exp = len(tokens) == 0
		for _, token := range tokens {
			if token.Kind == dnfpvf.TokenInt {
				definition.IgnoreQuestLevel4Exp = token.Int != 0
				break
			}
		}
	}
	for _, attribute := range doc.Texts("attribute") {
		if normalizeQuestTag(attribute) == "not give exp quest" {
			definition.NoExperience = true
			break
		}
	}
	if normalizeQuestTag(definition.RewardType) == "item" {
		definition.RewardItems, definition.HasGoldReward, definition.RewardParseError = parseRewardItemRules(doc, "reward int data")
		definition.RewardSelectionItems, definition.RewardParseError = parseRewardItemRulesAfter(definition.RewardParseError, doc, "reward selection int data")
	}
	if value, ok := doc.Int("sub type"); ok {
		definition.SubType = boundedInt(value)
	}
	definition.IntData = append([]int64(nil), doc.Ints("int data")...)
	definition.CheckCount = append([]int64(nil), doc.Ints("check count")...)
	definition.ConditionData = append([]int64(nil), doc.Ints("condition data")...)
	if tokens, ok := doc.Section("depend give item"); ok {
		// 真实 PVF 里有些任务带一个**空的** [depend give item] 段（例如婚约任务的
		// 第二步 20738）：段在、但没有要发放的道具，语义上等于"无依赖"。
		// 早先这里无条件置 true，于是这类任务在接取时被 applyAcceptEventItemSettlement
		// 判成 "depend give item data=[]" 而直接拒绝 —— 婚约任务链因此断在第二步。
		// 与上游一致：空段不算依赖。
		definition.HasDependGiveItem = len(tokens) != 0
		definition.DependGiveItemData = append([]int64(nil), doc.Ints("depend give item")...)
	}
	_, definition.CantGiveUp = doc.Section("cant giveup")
	definition.Job = strings.Join(doc.Texts("job"), " ")
	definition.TargetCharacter = strings.Join(doc.Texts("target character"), " ")
	if tokens, ok := doc.Section("target character"); ok {
		definition.TargetCharacterRules = parseTargetCharacterRules(tokens)
	}
	if levels := doc.Ints("level"); len(levels) > 0 {
		definition.LevelMin = boundedInt(levels[0])
		if len(levels) > 1 {
			definition.LevelMax = boundedInt(levels[1])
		}
	}
	if value, ok := doc.Int("grow type"); ok {
		definition.GrowType = boundedInt(value)
		definition.HasGrowType = true
	}
	if value, ok := doc.Int("job change quest"); ok {
		definition.JobChangeQuest = boundedInt(value)
	}
	if value, ok := doc.Int("exposed by npc"); ok {
		definition.ExposedByNPC = value
	}
	if value, ok := doc.Int("main quest"); ok {
		definition.MainQuestID = value
	}
	if value, ok := doc.Int("event"); ok && value != 0 {
		definition.IsEvent = true
	}
	if value, ok := doc.Int("creature kind"); ok && value >= 0 {
		definition.HasCreatureRequirement = true
	}
	if values := doc.Ints("expertjob level"); len(values) >= 2 && values[0] >= 0 && values[1] >= 0 {
		definition.HasExpertRequirement = true
	}
	definition.PreRequiredGroups = sectionIntGroups(doc, "pre required quest")
	definition.PreRequiredAnswers, definition.MalformedPreAnswers = parseQuestAnswerRequirements(sectionIntGroups(doc, "pre required quest answer"))
	definition.CollisionQuests = flattenIntGroups(sectionIntGroups(doc, "collision quest"))
	definition.MonsterRewardItems = parseMonsterRewardItems(doc)
	definition.EnemyRewardItems = parseEnemyRewardItems(doc)
	definition.DungeonScopes = parseDungeonScopes(doc)
	return definition
}

func questPresentationText(doc *dnfpvf.Document, section string) string {
	if doc == nil {
		return ""
	}
	parts := make([]string, 0, 1)
	for _, value := range doc.Texts(section) {
		if value = strings.TrimSpace(value); value != "" {
			parts = append(parts, strings.ReplaceAll(value, "%%", "%"))
		}
	}
	return strings.Join(parts, "\n")
}

func parseDungeonScopes(doc *dnfpvf.Document) []DungeonScope {
	values := doc.Ints("dungeon info")
	if len(values) < 2 {
		return nil
	}
	scopes := make([]DungeonScope, 0, len(values)/2)
	for index := 0; index+1 < len(values); index += 2 {
		if values[index] <= 0 || values[index+1] < -1 {
			continue
		}
		scopes = append(scopes, DungeonScope{DungeonID: values[index], Difficulty: values[index+1]})
	}
	return scopes
}

func parseRewardItemRulesAfter(previous string, doc *dnfpvf.Document, section string) ([]RewardItemRule, string) {
	rules, _, parseError := parseRewardItemRules(doc, section)
	if previous != "" {
		return rules, previous
	}
	return rules, parseError
}

func parseRewardItemRules(doc *dnfpvf.Document, section string) ([]RewardItemRule, bool, string) {
	tokens, ok := doc.Section(section)
	if !ok || len(tokens) == 0 {
		return nil, false, ""
	}
	rules := make([]RewardItemRule, 0, len(tokens)/2)
	hasGoldReward := false
	for index := 0; index < len(tokens); {
		if tokens[index].Kind != dnfpvf.TokenInt {
			return nil, false, fmt.Sprintf("[%s] token %d must be item id, got kind=%s raw=%q", section, index, tokens[index].Kind, tokens[index].Raw)
		}
		rule := RewardItemRule{ItemID: tokens[index].Int, Job: -1, GrowType: -1}
		index++
		if index < len(tokens) && rewardJobMarker(tokens[index]) {
			if index+3 >= len(tokens) || tokens[index+1].Kind != dnfpvf.TokenInt || tokens[index+2].Kind != dnfpvf.TokenInt || tokens[index+3].Kind != dnfpvf.TokenInt {
				return nil, false, fmt.Sprintf("[%s] item %d has incomplete [job] tuple", section, rule.ItemID)
			}
			rule.HasJobFilter = true
			rule.Job = boundedInt(tokens[index+1].Int)
			rule.GrowType = boundedInt(tokens[index+2].Int)
			rule.Count = tokens[index+3].Int
			index += 4
		} else {
			if index >= len(tokens) || tokens[index].Kind != dnfpvf.TokenInt {
				return nil, false, fmt.Sprintf("[%s] item %d is missing count", section, rule.ItemID)
			}
			rule.Count = tokens[index].Int
			index++
		}
		if rule.ItemID == 0 {
			hasGoldReward = hasGoldReward || rule.Count > 0
			continue
		}
		if rule.ItemID < 0 || rule.Count <= 0 || rule.Count > math.MaxUint32 {
			return nil, false, fmt.Sprintf("[%s] invalid item/count %d/%d", section, rule.ItemID, rule.Count)
		}
		rules = append(rules, rule)
	}
	return rules, hasGoldReward, ""
}

func rewardJobMarker(token dnfpvf.Token) bool {
	if token.Kind != dnfpvf.TokenString && token.Kind != dnfpvf.TokenIdent {
		return false
	}
	return normalizeQuestTag(token.Value) == "job"
}

// parseMonsterRewardItems parses the PVF [monster reward item] section.
// Each entry is a 7-tuple: monster_code dungeon_id difficulty item_id count
// drop_rate max_stack. dungeon_id -1 and difficulty -1 are wildcards.
func parseMonsterRewardItems(doc *dnfpvf.Document) []MonsterRewardItemEntry {
	tokens, ok := doc.Section("monster reward item")
	if !ok || len(tokens) == 0 {
		return nil
	}
	ints := make([]int64, 0, len(tokens))
	for _, token := range tokens {
		if token.Kind == dnfpvf.TokenInt {
			ints = append(ints, token.Int)
		}
	}
	entries := make([]MonsterRewardItemEntry, 0, len(ints)/7)
	for i := 0; i+6 < len(ints); i += 7 {
		entries = append(entries, MonsterRewardItemEntry{
			MonsterCode: ints[i],
			DungeonID:   ints[i+1],
			Difficulty:  ints[i+2],
			ItemID:      ints[i+3],
			Count:       ints[i+4],
			DropRate:    ints[i+5],
			MaxStack:    ints[i+6],
		})
	}
	return entries
}

// parseEnemyRewardItems parses the PVF [enemy reward item] section.
// Each entry is an 8-tuple: enemy_code enemy_type dungeon_id difficulty
// item_id count drop_rate max_stack. enemy_type 2 = AI character,
// 3 = passive object.
func parseEnemyRewardItems(doc *dnfpvf.Document) []EnemyRewardItemEntry {
	tokens, ok := doc.Section("enemy reward item")
	if !ok || len(tokens) == 0 {
		return nil
	}
	ints := make([]int64, 0, len(tokens))
	for _, token := range tokens {
		if token.Kind == dnfpvf.TokenInt {
			ints = append(ints, token.Int)
		}
	}
	entries := make([]EnemyRewardItemEntry, 0, len(ints)/8)
	for i := 0; i+7 < len(ints); i += 8 {
		entries = append(entries, EnemyRewardItemEntry{
			EnemyCode:  ints[i],
			EnemyType:  ints[i+1],
			DungeonID:  ints[i+2],
			Difficulty: ints[i+3],
			ItemID:     ints[i+4],
			Count:      ints[i+5],
			DropRate:   ints[i+6],
			MaxStack:   ints[i+7],
		})
	}
	return entries
}

func parseTargetCharacterRules(tokens []dnfpvf.Token) []TargetCharacterRule {
	rules := make([]TargetCharacterRule, 0, len(tokens)/3)
	for index := 0; index+2 < len(tokens); {
		jobToken := tokens[index]
		if (jobToken.Kind != dnfpvf.TokenString && jobToken.Kind != dnfpvf.TokenIdent) ||
			tokens[index+1].Kind != dnfpvf.TokenInt || tokens[index+2].Kind != dnfpvf.TokenInt {
			index++
			continue
		}
		rules = append(rules, TargetCharacterRule{
			JobTag: jobToken.Value, FirstGrowType: boundedInt(tokens[index+1].Int), AwakeningStage: boundedInt(tokens[index+2].Int),
		})
		index += 3
	}
	return rules
}

func rewardRuleMatches(rule RewardItemRule, character CharacterEligibility) bool {
	if rule.ItemID <= 0 || rule.Count <= 0 {
		return false
	}
	if !rule.HasJobFilter {
		return true
	}
	return rule.Job == character.Job && (rule.GrowType < 0 || rule.GrowType == character.GrowType&0x0f)
}

// definitionInitialTrigger ports the C# QuestData.ComputeInitTrigger domain
// rules.  The current EXE accepts the resulting u32 through the existing
// typed accept ACK; C# packet layouts are not used here.
//
// C# intentionally uses one as the fallback for a recognised-but-not-yet
// specialised quest type.  Returning a fake "unsupported" error after the
// task was advertised in the acceptable-quest list made the second main-line
// task (3146) impossible to accept.
func definitionInitialTrigger(definition Definition, completed map[int64]struct{}) uint32 {
	tag := normalizeQuestTag(definition.Type)
	switch tag {
	case "pvp quest":
		// 决斗场胜利任务（[pvp quest] + [sub type] 1）：int data[0] 既是初始
		// 触发值，也是需要累计的胜场数。
		if isPVPWinDefinition(definition) {
			return uint32(definition.IntData[0])
		}
	case "seek n meet npc":
		return seekAndMeetNPCInitialTrigger(definition.IntData)
	case "quest clear", "clear quest":
		return questClearInitialTrigger(definition.IntData, completed)
	case "condition under clear":
		return conditionUnderClearInitialTrigger(definition)
	case "clear map":
		return 1
	case "condition under clear2":
		return triggerFromIntData(definition.IntData, 5)
	case "normal clear":
		return packTriggerChannels(1, 1, 0)
	case "raid phase clear":
		return raidPhaseClearInitialTrigger(definition.IntData)
	case "hunt monster":
		return triggerFromIntData(definition.IntData, 4)
	case "hunt enemy":
		return triggerFromIntData(definition.IntData, 5)
	}
	if csharpTypeOneQuest(tag) && definition.SubType == 6 && len(definition.IntData) >= 3 && definition.IntData[2] > 0 {
		return boundedTriggerChannel(definition.IntData[2])
	}
	return 1
}

func conditionUnderClearInitialTrigger(definition Definition) uint32 {
	values := definition.IntData
	switch definition.SubType {
	// 22 是婚约任务（[condition under clear]，要求"正好这对夫妻通关"），与 6 共用
	// 同一套 int data 主线；漏掉它会退回 default 的 stride-4 解析，得到错误的
	// 初始 trigger（实测 1，应为 5），婚约任务因此无法按原生进度推进。
	case 6, 22:
		return conditionUnderClearSubtype6InitialTrigger(values)
	case 23:
		if len(values) > 0 && values[0] == -2 {
			return triggerFromIntData(values, 4)
		}
		return conditionUnderClearSubtype23InitialTrigger(values)
	case 11:
		return 1
	default:
		return triggerFromIntData(values, 4)
	}
}

func conditionUnderClearSubtype6InitialTrigger(values []int64) uint32 {
	if len(values) == 0 || len(values) == 2 {
		return 1
	}
	return triggerFromIntData(values, 3)
}

func conditionUnderClearSubtype23InitialTrigger(values []int64) uint32 {
	if len(values) == 0 {
		return 1
	}
	return triggerFromIntData(values, 3)
}

func (c *Catalog) linkedSubQuestAcceptPlans(definition Definition, character CharacterEligibility, completed, active map[int64]struct{}, answers map[int64]int64) []AcceptLinkedSubQuestPlan {
	if c == nil {
		return nil
	}
	tag := normalizeQuestTag(definition.Type)
	if tag != "quest clear" && tag != "clear quest" {
		return nil
	}
	// The parent is being accepted by this plan. Make that pending activation
	// visible to the shared eligibility predicate so its linked children do not
	// have to be exposed as standalone op21 tasks before the parent exists.
	active[definition.ID] = struct{}{}
	seen := make(map[int64]struct{}, len(definition.IntData))
	plans := make([]AcceptLinkedSubQuestPlan, 0, len(definition.IntData))
	for _, childID := range definition.IntData {
		if childID <= 0 {
			continue
		}
		if _, duplicate := seen[childID]; duplicate {
			continue
		}
		seen[childID] = struct{}{}
		if _, exists := completed[childID]; exists {
			continue
		}
		if _, exists := active[childID]; exists {
			continue
		}
		child, ok := c.byID[childID]
		if !ok || child.MainQuestID != definition.ID || child.HasDependGiveItem || !shouldAutoCompleteNoRewardSubQuest(child) {
			continue
		}
		if !c.definitionAcceptable(child, character, completed, active, answers, false) {
			continue
		}
		active[childID] = struct{}{}
		plans = append(plans, AcceptLinkedSubQuestPlan{
			QuestID:     child.ID,
			Path:        child.Path,
			Type:        child.Type,
			InitTrigger: definitionInitialTrigger(child, completed),
		})
	}
	return plans
}

func seekAndMeetNPCInitialTrigger(values []int64) uint32 {
	if len(values) < 3 {
		return 1
	}
	itemCount := values[1]
	if itemCount <= 0 {
		itemCount = 1
	}
	meetNPCCount := int64(0)
	if values[2] > 0 {
		meetNPCCount = 1
	}
	return packTriggerChannels(itemCount, meetNPCCount, 0)
}

func questClearInitialTrigger(values []int64, completed map[int64]struct{}) uint32 {
	missing := int64(0)
	for _, questID := range values {
		if questID > 0 {
			if _, exists := completed[questID]; !exists {
				missing++
			}
		}
	}
	if missing == 0 {
		return 1
	}
	return boundedTriggerChannel(missing)
}

// raidPhaseClearInitialTrigger maps each PVF four-value phase rule to the
// matching packed trigger channel. The current Anton mission uses
// phase/count tuples 0/*/5/* and 1/*/5/*, so accepting it must publish 5/5
// rather than the generic fallback trigger 1.
func raidPhaseClearInitialTrigger(values []int64) uint32 {
	if len(values) == 0 || len(values)%4 != 0 {
		return 1
	}
	channels := [3]int64{}
	found := false
	for offset := 0; offset+3 < len(values); offset += 4 {
		phase := values[offset]
		count := values[offset+2]
		if phase < 0 || phase >= int64(len(channels)) || count <= 0 {
			continue
		}
		channels[phase] = count
		found = true
	}
	if !found {
		return 1
	}
	return packTriggerChannels(channels[0], channels[1], channels[2])
}

func csharpTypeOneQuest(tag string) bool {
	switch tag {
	case "seeking", "hunt monster", "meet npc", "hunt enemy", "use item", "get item", "get score",
		"clear quest", "quest clear", "custom quest", "send chatting", "check life", "amplify item",
		"disjoint item", "equipped item", "check time", "use fortune coin", "meet secret npc",
		"turn gold card", "ui click", "seek n meet npc", "assault count", "mobile":
		return true
	default:
		return false
	}
}

func triggerFromIntData(values []int64, stride int) uint32 {
	if len(values) == 0 || stride <= 0 {
		return 1
	}
	counts := make([]int64, 0, 3)
	for offset := stride - 1; offset < len(values) && len(counts) < 3; offset += stride {
		counts = append(counts, values[offset])
	}
	if len(counts) == 0 {
		return 1
	}
	return packTriggerChannels(counts...)
}

func packTriggerChannels(values ...int64) uint32 {
	var packed uint32
	for index, value := range values {
		if index == 3 {
			break
		}
		packed |= boundedTriggerChannel(value) << (index * 9)
	}
	return packed
}

func boundedTriggerChannel(value int64) uint32 {
	if value <= 0 {
		return 0
	}
	if value > 0x1ff {
		return 0x1ff
	}
	return uint32(value)
}

func (c *Catalog) definitionAcceptable(definition Definition, character CharacterEligibility, completed, active map[int64]struct{}, answers map[int64]int64, includeActive bool) bool {
	// A first-awakening story variant is a reward companion, not a second
	// client-facing task. Keep the canonical epic definition in op21/op31 and
	// project the matching variant only during finish settlement.
	if canonicalID, projected := c.storyAwakeningCanonicalFor[definition.ID]; projected {
		if canonical, found := c.byID[canonicalID]; found {
			if _, activeCompanion := c.storyAwakeningCompanion(canonical, character); activeCompanion {
				return false
			}
		}
	}
	return definitionAcceptable(definition, character, completed, active, answers, includeActive)
}

func definitionAcceptable(definition Definition, character CharacterEligibility, completed, active map[int64]struct{}, answers map[int64]int64, includeActive bool) bool {
	if definition.ExposedByNPC == 0 || definition.IsEvent || definition.HasCreatureRequirement || definition.HasExpertRequirement {
		return false
	}
	grade := normalizeQuestTag(definition.Grade)
	if !selectableGrade(grade) || grade == "training" {
		return false
	}
	if grade == "wedding" && !character.Wedding {
		return false
	}
	if character.Level < definition.LevelMin || character.Level > definition.LevelMax {
		return false
	}
	if !definitionMatchesCharacter(definition, character) {
		return false
	}
	if definition.MainQuestID > 0 {
		if _, parentActive := active[definition.MainQuestID]; !parentActive {
			return false
		}
	}
	if _, exists := active[definition.ID]; exists && !includeActive {
		return false
	}
	// A completed task leaves the acceptable list unless its grade is genuinely
	// repeatable. A daily is therefore gone from op21 for the rest of its service
	// day and reappears only after ReconcileDailyReset clears the row at 06:00;
	// an already-completed one-shot never comes back.
	if _, exists := completed[definition.ID]; exists && !isRepeatableGrade(grade) {
		return false
	}
	if len(definition.PreRequiredGroups) > 0 {
		matched := false
		for _, group := range definition.PreRequiredGroups {
			if completedAll(group, completed) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	if definition.MalformedPreAnswers {
		return false
	}
	for _, requirement := range definition.PreRequiredAnswers {
		answer, ok := answers[requirement.QuestID]
		if !ok || answer != requirement.Answer {
			return false
		}
	}
	for _, collision := range definition.CollisionQuests {
		if collision <= 0 {
			continue
		}
		if _, exists := completed[collision]; exists {
			return false
		}
	}
	return true
}

// definitionMatchesCharacter is the shared PVF job/grow-type predicate for
// ordinary acceptance and server-owned main-quest skip planning. It contains
// no level, prerequisite, collision, event, or quest-state policy.
func definitionMatchesCharacter(definition Definition, character CharacterEligibility) bool {
	if job := strings.TrimSpace(definition.Job); job != "" && normalizeQuestTag(job) != "all" && !jobmap.MatchesQuestTag(job, character.Job) {
		return false
	}
	if len(definition.TargetCharacterRules) != 0 && definition.JobChangeQuest >= 1 && definition.JobChangeQuest <= 3 {
		matched := false
		firstGrowType := character.GrowType & 0x0f
		awakeningStage := (character.GrowType >> 4) & 0x0f
		for _, rule := range definition.TargetCharacterRules {
			if jobmap.MatchesQuestTag(rule.JobTag, character.Job) && rule.FirstGrowType == firstGrowType && rule.AwakeningStage == awakeningStage {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	} else if target := strings.TrimSpace(definition.TargetCharacter); target != "" && !jobmap.MatchesQuestTag(target, character.Job) {
		return false
	}
	if definition.HasGrowType && definition.GrowType >= 0 {
		switch definition.JobChangeQuest {
		case 1, 10, 20:
		case 2, 3:
			if definition.GrowType != character.GrowType&0x0f {
				return false
			}
		default:
			if definition.GrowType != character.GrowType {
				return false
			}
		}
	}
	return true
}

func questStateSets(record dnfrepo.QuestRecord) (map[int64]struct{}, map[int64]struct{}) {
	completed := make(map[int64]struct{})
	active := make(map[int64]struct{})
	collect := func(states map[int64]dnfrepo.QuestState) {
		for id, state := range states {
			if id <= 0 {
				continue
			}
			if isCompletedQuestStatus(state.Status) {
				completed[id] = struct{}{}
				continue
			}
			if isActiveQuestStatus(state.Status) {
				active[id] = struct{}{}
			}
		}
	}
	collect(record.States)
	collect(record.Progress)
	return completed, active
}

const questAnswerExtraKey = "question_answer"

func questAnswerSelections(record dnfrepo.QuestRecord) map[int64]int64 {
	answers := make(map[int64]int64)
	collect := func(states map[int64]dnfrepo.QuestState) {
		for questID, state := range states {
			raw := strings.TrimSpace(state.Extra[questAnswerExtraKey])
			if raw != "" {
				answer, err := strconv.ParseInt(raw, 10, 64)
				if err == nil && answer >= 0 {
					answers[questID] = answer
					continue
				}
			}
			// Compatibility for question quests completed before answer
			// persistence: the old handler could only complete answer zero.
			if isCompletedQuestStatus(state.Status) && state.TriggerType == 0 {
				answers[questID] = 0
			}
		}
	}
	collect(record.Progress)
	collect(record.States)
	return answers
}

func isCompletedQuestStatus(status string) bool {
	switch normalizeStatus(status) {
	case "complete", "completed", "cleared", "finished", "done":
		return true
	default:
		return false
	}
}

func normalizeStatus(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	value = strings.ReplaceAll(value, "_", "")
	value = strings.ReplaceAll(value, "-", "")
	value = strings.ReplaceAll(value, " ", "")
	return value
}

// isDailyGrade reports whether a normalized PVF [grade] makes the quest a daily
// task. The client's task manual treats the whole daily family as one class, so
// the test is the shared "daily" token rather than one exact value: the live PVF
// carries `daily`, `special daily`, `daily mission`, `auto daily mission` and
// the legacy `daily random`. `normaly repeat` deliberately does not match - it is
// a genuinely repeatable task, not a daily.
func isDailyGrade(grade string) bool {
	return strings.Contains(grade, "daily")
}

// isRepeatableGrade reports whether a completed quest may be taken again
// immediately, with no service-day boundary. Only `normaly repeat` qualifies: a
// daily task is locked for the rest of its service day (the row is cleared at
// 06:00 by ReconcileDailyReset), and every other grade is one-shot.
func isRepeatableGrade(grade string) bool {
	return grade == "normaly repeat"
}

// ticketExcludedGrade reports whether a grade disqualifies a quest from bulk
// completion tickets. A ticket must not consume a daily task or a repeatable
// one, so this is deliberately wider than either predicate above.
func ticketExcludedGrade(grade string) bool {
	return isDailyGrade(grade) || isRepeatableGrade(grade)
}

func selectableGrade(grade string) bool {
	switch grade {
	case "", "normal", "side", "sub", "epic", "realization", "training", "achievement", "common unique", "system", "mission", "wedding":
		return true
	}
	// Every member of the daily family is selectable, and `normaly repeat` is
	// selectable because it is repeatable - not because it is a daily.
	return isDailyGrade(grade) || isRepeatableGrade(grade)
}

func normalizeQuestTag(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if len(value) >= 2 && value[0] == '[' && value[len(value)-1] == ']' {
		value = strings.TrimSpace(value[1 : len(value)-1])
	}
	return value
}

func sectionIntGroups(doc *dnfpvf.Document, name string) [][]int64 {
	if doc == nil {
		return nil
	}
	want := normalizeQuestTag(name)
	groups := make([][]int64, 0, 1)
	for _, section := range doc.Sections {
		if normalizeQuestTag(section.Name) != want || section.Start < 0 || section.End > len(doc.Tokens) || section.Start > section.End {
			continue
		}
		group := make([]int64, 0, section.End-section.Start)
		for _, token := range doc.Tokens[section.Start:section.End] {
			if token.Kind == dnfpvf.TokenInt {
				group = append(group, token.Int)
			}
		}
		if len(group) > 0 {
			groups = append(groups, group)
		}
	}
	return groups
}

func flattenIntGroups(groups [][]int64) []int64 {
	var values []int64
	for _, group := range groups {
		values = append(values, group...)
	}
	return values
}

func parseQuestAnswerRequirements(groups [][]int64) ([]QuestAnswerRequirement, bool) {
	var requirements []QuestAnswerRequirement
	for _, group := range groups {
		if len(group)%2 != 0 {
			return nil, true
		}
		for index := 0; index < len(group); index += 2 {
			if group[index] <= 0 || group[index+1] < 0 {
				return nil, true
			}
			requirements = append(requirements, QuestAnswerRequirement{QuestID: group[index], Answer: group[index+1]})
		}
	}
	return requirements, false
}

func completedAll(ids []int64, completed map[int64]struct{}) bool {
	for _, id := range ids {
		if id <= 0 {
			continue
		}
		if _, exists := completed[id]; !exists {
			return false
		}
	}
	return true
}

func boundedInt(value int64) int {
	if value > int64(math.MaxInt) {
		return math.MaxInt
	}
	if value < int64(math.MinInt) {
		return math.MinInt
	}
	return int(value)
}

func cloneDefinition(definition Definition) Definition {
	definition.IntData = append([]int64(nil), definition.IntData...)
	definition.CheckCount = append([]int64(nil), definition.CheckCount...)
	definition.ConditionData = append([]int64(nil), definition.ConditionData...)
	definition.DependGiveItemData = append([]int64(nil), definition.DependGiveItemData...)
	definition.PreRequiredGroups = append([][]int64(nil), definition.PreRequiredGroups...)
	for index := range definition.PreRequiredGroups {
		definition.PreRequiredGroups[index] = append([]int64(nil), definition.PreRequiredGroups[index]...)
	}
	definition.PreRequiredAnswers = append([]QuestAnswerRequirement(nil), definition.PreRequiredAnswers...)
	definition.CollisionQuests = append([]int64(nil), definition.CollisionQuests...)
	definition.RewardIntData = append([]int64(nil), definition.RewardIntData...)
	definition.RewardItems = append([]RewardItemRule(nil), definition.RewardItems...)
	definition.RewardSelectionItems = append([]RewardItemRule(nil), definition.RewardSelectionItems...)
	definition.TargetCharacterRules = append([]TargetCharacterRule(nil), definition.TargetCharacterRules...)
	definition.MonsterRewardItems = append([]MonsterRewardItemEntry(nil), definition.MonsterRewardItems...)
	definition.EnemyRewardItems = append([]EnemyRewardItemEntry(nil), definition.EnemyRewardItems...)
	definition.DungeonScopes = cloneDungeonScopes(definition.DungeonScopes)
	return definition
}

func SortedIDs(result EligibilityResult) []int32 {
	ids := append([]int32(nil), result.IDs...)
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

// ExEquipSlotStat is the durable current-client bitset selected by the runtime
// PVF [slot expansion] reward index. Current Script.pvf uses indices 0, 1, and
// 2, but the current NoPack UI bits are deliberately not contiguous: its
// support/magic-stone/earring checks use 0x01, 0x02, and 0x10 respectively.
const (
	ExEquipSlotSupport    byte = 0x01
	ExEquipSlotMagicStone byte = 0x02
	ExEquipSlotEarring    byte = 0x10
	ExEquipSlotAll        byte = ExEquipSlotSupport | ExEquipSlotMagicStone | ExEquipSlotEarring
)

func ExEquipSlotBitForPVFIndex(index uint32) (byte, bool) {
	switch index {
	case 0:
		return ExEquipSlotSupport, true
	case 1:
		return ExEquipSlotMagicStone, true
	case 2:
		return ExEquipSlotEarring, true
	default:
		return 0, false
	}
}
