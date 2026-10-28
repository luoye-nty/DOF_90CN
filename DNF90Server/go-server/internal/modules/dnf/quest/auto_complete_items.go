package quest

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

// Task-manual auto completion (op1429) deliberately grants no rewards. That is
// right for experience, gold and ordinary loot, but a handful of epic quests
// reward a story item that a later epic requires: 3226 hands over 斯卡迪女王的
// 书信 (10164786) and 3610 later asks for it; six mirror-era epics each drop one
// 毁灭纪碎片 (10164800) and 3509 asks for ten. A character who auto-completes
// the producer and then plays the consumer has no way to obtain the item and
// the chain is stuck for good.
//
// The repair below is a narrow join, not a reward replay: only items that an
// outstanding story consumer actually asks for, and that a completed no-reward
// producer would have handed over, are granted, and only up to the deficit the
// inventory shows. Parent closure (a [quest clear] parent auto-completing its
// no-reward children) does not make this redundant - it only reaches children
// whose parent itself falls under the level cutoff, whereas the stuck case is
// a producer under the cutoff and a consumer above it.

// AutoCompleteCarryForwardRepairInput repairs only story items whose ordinary
// reward was deliberately skipped by the task-manual no-reward path. It does
// not replay experience, currency, selectable rewards, or ordinary loot.
type AutoCompleteCarryForwardRepairInput struct {
	CharacterID  string
	RepairedAt   time.Time
	AllocateItem FinishItemAllocator
}

type AutoCompleteCarryForwardRepairResult struct {
	CharacterID string
	Items       []FinishCommittedItem
	Inventory   dnfrepo.InventoryRecord
	Idempotent  bool
}

type autoCompleteCarryForwardRequirement struct {
	ConsumerQuestID int64
	Count           int64
}

type autoCompleteCarryForwardGrant struct {
	ProducerQuestID int64
	ConsumerQuestID int64
	PVFPath         string
	ItemID          int64
	Count           int64
}

type autoCompleteCarryForwardContribution struct {
	anchorQuestID int64
	anchorPath    string
	rewardCount   int64
	consumeCount  int64
}

// ApplyAutoCompleteCarryForwardRepair is the reconnect compatibility path for
// characters saved before carry-forward item preservation existed. The
// completed no-reward producer, active/next consumer, and inventory deficit
// are checked again inside one character-settlement transaction.
func (o *Owner) ApplyAutoCompleteCarryForwardRepair(
	ctx context.Context,
	catalog *Catalog,
	input AutoCompleteCarryForwardRepairInput,
) (AutoCompleteCarryForwardRepairResult, error) {
	if o == nil || o.repositories.CharacterSettlement == nil || catalog == nil {
		return AutoCompleteCarryForwardRepairResult{}, ErrAutoCompleteMainUnavailable
	}
	input.CharacterID = strings.TrimSpace(input.CharacterID)
	if input.CharacterID == "" {
		return AutoCompleteCarryForwardRepairResult{}, ErrCharacterRequired
	}
	if input.RepairedAt.IsZero() {
		input.RepairedAt = time.Now().UTC()
	}
	if ctx == nil {
		ctx = context.Background()
	}

	var items []FinishCommittedItem
	var inventory dnfrepo.InventoryRecord
	err := o.repositories.CharacterSettlement.WithinCharacterSettlement(ctx, input.CharacterID, func(tx dnfrepo.Group) error {
		if tx.Character == nil || tx.Quest == nil || tx.Inventory == nil {
			return ErrAutoCompleteMainUnavailable
		}
		character, found, err := tx.Character.Load(ctx, input.CharacterID)
		if err != nil {
			return err
		}
		if !found || strings.TrimSpace(character.CharacterID) != input.CharacterID {
			return ErrCharacterNotFound
		}
		job, err := strconv.Atoi(strings.TrimSpace(character.Job))
		growType := character.Stats["grow_type"]
		if err != nil || !definitionCharacterJobValid(CharacterEligibility{Level: character.Level, Job: job}) || growType < 0 || growType > 0xff {
			return ErrAutoCompleteLevelInvalid
		}
		record, found, err := tx.Quest.Load(ctx, input.CharacterID)
		if err != nil {
			return err
		}
		if !found {
			return nil
		}
		items, inventory, err = repairAutoCompletedQuestCarryForwardItems(
			ctx,
			catalog,
			tx.Inventory,
			input.CharacterID,
			record,
			CharacterEligibility{Level: character.Level, Job: job, GrowType: int(growType)},
			input.RepairedAt,
			input.AllocateItem,
		)
		return err
	})
	if err != nil {
		return AutoCompleteCarryForwardRepairResult{}, err
	}
	return AutoCompleteCarryForwardRepairResult{
		CharacterID: input.CharacterID,
		Items:       cloneFinishItems(items),
		Inventory:   dnfrepo.CloneInventory(inventory),
		Idempotent:  len(items) == 0,
	}, nil
}

// repairAutoCompletedQuestCarryForwardItems grants the deficit of every
// carry-forward item to the main inventory. It is idempotent by construction:
// a second pass finds the inventory already holding the required count and
// grants nothing.
func repairAutoCompletedQuestCarryForwardItems(
	ctx context.Context,
	catalog *Catalog,
	inventoryRepository dnfrepo.InventoryRepository,
	characterID string,
	record dnfrepo.QuestRecord,
	character CharacterEligibility,
	repairedAt time.Time,
	allocator FinishItemAllocator,
) ([]FinishCommittedItem, dnfrepo.InventoryRecord, error) {
	grants, err := catalog.autoCompleteCarryForwardGrants(record, character)
	if err != nil {
		return nil, dnfrepo.InventoryRecord{}, err
	}
	if len(grants) == 0 {
		return nil, dnfrepo.InventoryRecord{}, nil
	}
	if inventoryRepository == nil {
		return nil, dnfrepo.InventoryRecord{}, ErrAutoCompleteMainUnavailable
	}
	inventory, found, err := inventoryRepository.Load(ctx, characterID)
	if err != nil {
		return nil, dnfrepo.InventoryRecord{}, err
	}
	if !found || strings.TrimSpace(inventory.CharacterID) != characterID {
		return nil, dnfrepo.InventoryRecord{}, ErrAutoCompleteMainUnavailable
	}
	inventory = dnfrepo.CloneInventory(inventory)
	committed := make([]FinishCommittedItem, 0, len(grants))
	for _, grant := range grants {
		held := countItemInMain(inventory, grant.ItemID)
		if held >= grant.Count {
			continue
		}
		if allocator == nil {
			return nil, dnfrepo.InventoryRecord{}, ErrFinishItemAllocatorRequired
		}
		deficit := grant.Count - held
		completionKey := fmt.Sprintf(
			"auto-complete-carry-forward/%s/%d/%d/%d",
			characterID,
			grant.ProducerQuestID,
			grant.ConsumerQuestID,
			grant.ItemID,
		)
		item, allocateErr := allocator(&inventory, FinishItemGrantRequest{
			QuestID:       grant.ProducerQuestID,
			CompletionKey: completionKey,
			Source:        "quest_pvf_auto_complete_carry_forward:" + grant.PVFPath,
			ItemID:        grant.ItemID,
			Count:         deficit,
		})
		if allocateErr != nil {
			return nil, dnfrepo.InventoryRecord{}, allocateErr
		}
		if item.ItemID != grant.ItemID || item.Delta != deficit || item.SlotKey == "" || item.PostCount < item.Delta || len(item.RawEntry) == 0 {
			return nil, dnfrepo.InventoryRecord{}, fmt.Errorf("%w: invalid carry-forward allocator receipt item=%d", ErrQuestRewardMalformed, grant.ItemID)
		}
		committed = append(committed, item)
	}
	if len(committed) == 0 {
		return nil, inventory, nil
	}
	inventory.UpdatedAt = repairedAt.UTC()
	if err := dnfrepo.SaveInventoryFields(ctx, inventoryRepository, inventory, dnfrepo.InventoryFieldSlots); err != nil {
		return nil, dnfrepo.InventoryRecord{}, err
	}
	return committed, inventory, nil
}

// autoCompleteCarryForwardGrants matches the immediate active/acceptable
// storyline's required item against a completed task-manual producer whose
// ordinary item reward was explicitly suppressed. This narrow join is what
// distinguishes a continuity item from the ordinary rewards op1429 must skip.
func (c *Catalog) autoCompleteCarryForwardGrants(
	record dnfrepo.QuestRecord,
	character CharacterEligibility,
) ([]autoCompleteCarryForwardGrant, error) {
	if c == nil {
		return nil, ErrCatalogEmpty
	}
	requirements := c.autoCompleteOutstandingItemRequirements(record, character)
	if len(requirements) == 0 {
		return nil, nil
	}

	contributions := make(map[int64]autoCompleteCarryForwardContribution, len(requirements))
	visitedDefinitions := make(map[int64]struct{})
	var collectDefinition func(Definition) error
	collectDefinition = func(definition Definition) error {
		if definition.ID <= 0 || !definitionMatchesCharacter(definition, character) {
			return nil
		}
		if _, visited := visitedDefinitions[definition.ID]; visited {
			return nil
		}
		visitedDefinitions[definition.ID] = struct{}{}

		if normalizeQuestTag(definition.RewardType) == "item" && definition.RewardParseError == "" {
			for _, rule := range definition.RewardItems {
				if _, needed := requirements[rule.ItemID]; !needed || !rewardRuleMatches(rule, character) {
					continue
				}
				contribution := contributions[rule.ItemID]
				contribution.rewardCount += rule.Count
				if contribution.anchorQuestID == 0 || definition.ID < contribution.anchorQuestID {
					contribution.anchorQuestID = definition.ID
					contribution.anchorPath = definition.Path
				}
				contributions[rule.ItemID] = contribution
			}
		}
		for _, rule := range finishDefinitionSeekingRules(definition) {
			if _, needed := requirements[rule.ItemID]; !needed {
				continue
			}
			contribution := contributions[rule.ItemID]
			contribution.consumeCount += rule.Count
			contributions[rule.ItemID] = contribution
		}

		if !questClearDefinition(definition) {
			return nil
		}
		// Task-manual completion closes a composite epic without executing its
		// rewarded internal stages. Reconstruct those stages from the parent so
		// cumulative story items (for example quest 3509's ten fragments) retain
		// the same net balance as an ordinary play-through.
		for _, childID := range definition.IntData {
			child, found := c.Find(childID)
			if !found || child.MainQuestID != definition.ID {
				continue
			}
			if err := collectDefinition(child); err != nil {
				return err
			}
		}
		return nil
	}
	collect := func(questID int64, state dnfrepo.QuestState) error {
		if !isCompletedQuestStatus(state.Status) ||
			state.Extra["auto_completed"] != "true" ||
			state.Extra["auto_complete_reason"] != "epic_below_character_level_no_reward" {
			return nil
		}
		definition, found := c.Find(questID)
		if !found || normalizeQuestTag(definition.Grade) != "epic" {
			return nil
		}
		return collectDefinition(definition)
	}
	seen := make(map[int64]struct{}, len(record.States)+len(record.Progress))
	for questID, state := range record.States {
		seen[questID] = struct{}{}
		if err := collect(questID, state); err != nil {
			return nil, err
		}
	}
	for questID, state := range record.Progress {
		if _, duplicate := seen[questID]; duplicate {
			continue
		}
		if err := collect(questID, state); err != nil {
			return nil, err
		}
	}
	itemIDs := make([]int64, 0, len(contributions))
	netCounts := make(map[int64]int64, len(contributions))
	for itemID, contribution := range contributions {
		netCount := contribution.rewardCount - contribution.consumeCount
		// What the pass owes the character is exactly what it withheld: the net
		// the completed producers would have handed over. It is deliberately
		// not the consumer's requirement. The pass stops wherever the chain
		// stops being safe to auto-complete - a [quest clear] parent with a
		// reward-bearing child, for one - and the producers inside that
		// remaining stretch are ones the player still earns by playing it. A
		// short net there is normal, not malformed PVF: quest 3473 needs three
		// fragments while its own sibling 3472 awards two of them.
		if contribution.anchorQuestID <= 0 || netCount <= 0 {
			return nil, fmt.Errorf(
				"%w: cumulative producers item=%d reward=%d consumed=%d net=%d required=%d",
				ErrQuestRewardMalformed,
				itemID,
				contribution.rewardCount,
				contribution.consumeCount,
				netCount,
				requirements[itemID].Count,
			)
		}
		if requirement := requirements[itemID].Count; netCount > requirement {
			netCount = requirement
		}
		netCounts[itemID] = netCount
		itemIDs = append(itemIDs, itemID)
	}
	sort.Slice(itemIDs, func(left, right int) bool { return itemIDs[left] < itemIDs[right] })
	grants := make([]autoCompleteCarryForwardGrant, 0, len(itemIDs))
	for _, itemID := range itemIDs {
		contribution := contributions[itemID]
		requirement := requirements[itemID]
		grants = append(grants, autoCompleteCarryForwardGrant{
			ProducerQuestID: contribution.anchorQuestID,
			ConsumerQuestID: requirement.ConsumerQuestID,
			PVFPath:         contribution.anchorPath,
			ItemID:          itemID,
			Count:           netCounts[itemID],
		})
	}
	return grants, nil
}

// autoCompleteOutstandingItemRequirements collects the story items the
// character's active or currently acceptable epic line still has to hand in.
// Consumers are epics, or children of an epic [quest clear] parent, whose type
// consumes items ([seeking] / [seek n meet npc]).
func (c *Catalog) autoCompleteOutstandingItemRequirements(
	record dnfrepo.QuestRecord,
	character CharacterEligibility,
) map[int64]autoCompleteCarryForwardRequirement {
	requirements := make(map[int64]autoCompleteCarryForwardRequirement)
	consumerIDs := make(map[int64]struct{})
	collectActive := func(states map[int64]dnfrepo.QuestState) {
		for questID, state := range states {
			if isActiveQuestStatus(state.Status) {
				consumerIDs[questID] = struct{}{}
			}
		}
	}
	collectActive(record.States)
	collectActive(record.Progress)
	for _, questID := range c.Acceptable(character, record).IDs {
		consumerIDs[int64(questID)] = struct{}{}
	}
	// A quest-clear parent owns internal child steps that are not advertised as
	// independently acceptable. Include those definitions so the item needed by
	// the next composite main quest is preserved before the parent is accepted.
	for questID := range consumerIDs {
		definition, found := c.Find(questID)
		if !found || !questClearDefinition(definition) {
			continue
		}
		for _, childID := range definition.IntData {
			if child, childFound := c.Find(childID); childFound && child.MainQuestID == definition.ID {
				consumerIDs[childID] = struct{}{}
			}
		}
	}
	for questID := range consumerIDs {
		if state, found := questStateFor(record, questID); found && isCompletedQuestStatus(state.Status) {
			continue
		}
		definition, found := c.Find(questID)
		if !found || !c.autoCompleteStoryItemConsumer(definition) || !definitionMatchesCharacter(definition, character) {
			continue
		}
		for _, rule := range finishDefinitionSeekingRules(definition) {
			if rule.ItemID <= 0 || rule.Count <= 0 {
				continue
			}
			current, exists := requirements[rule.ItemID]
			if !exists || rule.Count > current.Count || (rule.Count == current.Count && questID < current.ConsumerQuestID) {
				requirements[rule.ItemID] = autoCompleteCarryForwardRequirement{ConsumerQuestID: questID, Count: rule.Count}
			}
		}
	}
	return requirements
}

func (c *Catalog) autoCompleteStoryItemConsumer(definition Definition) bool {
	if normalizeQuestTag(definition.Grade) == "epic" {
		return true
	}
	if definition.MainQuestID <= 0 {
		return false
	}
	parent, found := c.Find(definition.MainQuestID)
	return found && normalizeQuestTag(parent.Grade) == "epic" && questClearDefinition(parent)
}
