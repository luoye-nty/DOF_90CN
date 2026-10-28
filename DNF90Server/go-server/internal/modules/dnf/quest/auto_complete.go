package quest

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"longheng.io/server/internal/modules/dnf/jobmap"
	"longheng.io/server/internal/modules/dnf/profession"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

var (
	ErrAutoCompleteMainUnavailable = errors.New("main quest auto-complete is unavailable")
	ErrAutoCompleteLevelInvalid    = errors.New("main quest auto-complete level is invalid")
	ErrAutoCompleteTargetInvalid   = errors.New("main quest auto-complete target is invalid")
	ErrAutoCompleteTargetAmbiguous = errors.New("main quest auto-complete target is ambiguous")
	ErrAutoCompleteClassRequired   = errors.New("main quest auto-complete requires class change")
	// ErrAutoCompleteAwakeningRequired reports that the story chain is standing
	// on an awakening quest. A bulk pass never completes one - that run belongs
	// to the player - so an empty result there is not "nothing left to do".
	ErrAutoCompleteAwakeningRequired = errors.New("main quest auto-complete is blocked by an awakening quest")
	// ErrAutoCompleteChoiceRequired reports that the chain is standing on a
	// [question] branch quest. The answer decides which successors unlock, so
	// it is the player's to give; the pass must not pick one.
	ErrAutoCompleteChoiceRequired = errors.New("main quest auto-complete is blocked by a branch choice quest")
)

// AwakeningRequiredError names the blocking awakening quest so the caller can
// tell the player which one to run instead of reporting an empty result.
type AwakeningRequiredError struct {
	QuestID   int64
	QuestName string
}

func (e *AwakeningRequiredError) Error() string {
	return fmt.Sprintf("%s: quest=%d name=%q", ErrAutoCompleteAwakeningRequired, e.QuestID, e.QuestName)
}

func (e *AwakeningRequiredError) Is(target error) bool {
	return target == ErrAutoCompleteAwakeningRequired
}

// ChoiceRequiredError names the branch quest waiting for the player's answer.
type ChoiceRequiredError struct {
	QuestID   int64
	QuestName string
}

func (e *ChoiceRequiredError) Error() string {
	return fmt.Sprintf("%s: quest=%d name=%q", ErrAutoCompleteChoiceRequired, e.QuestID, e.QuestName)
}

func (e *ChoiceRequiredError) Is(target error) bool {
	return target == ErrAutoCompleteChoiceRequired
}

// The story chain above this level is gated on the first awakening quest group,
// and that group is per first grow type. A character that never took the class
// change therefore has exactly one legal next step - the awakening quest the
// bulk pass must not touch - so an empty bulk result there is reported as the
// missing class change rather than as an unexplained empty candidate set.
const autoCompleteMainFirstAwakeningLevel = 50

type AutoCompleteMainInput struct {
	CharacterID   string
	Eligibility   CharacterEligibility
	CutoffLevel   int
	TargetQuestID int64
	CompletedAt   time.Time
	// AllocateItem places carry-forward story items (see auto_complete_items.go)
	// into the main inventory inside the same settlement transaction. It is only
	// consulted when an outstanding consumer actually lacks such an item.
	AllocateItem FinishItemAllocator
}

type AutoCompleteMainPlan struct {
	Record                dnfrepo.QuestRecord
	EligibleQuestIDs      []int64
	ChangedQuestIDs       []int64
	ChangedLinkedSubIDs   []int64
	ActiveCompletedCount  int
	HighestCompletedQuest int64
	ChangedFields         []dnfrepo.QuestField
}

type AutoCompleteMainResult struct {
	CharacterID           string
	CutoffLevel           int
	EligibleQuestIDs      []int64
	ChangedQuestIDs       []int64
	ChangedLinkedSubIDs   []int64
	ActiveCompletedCount  int
	HighestCompletedQuest int64
	PostCommitQuest       dnfrepo.QuestRecord
	// PostCommitInventory and CarryForwardItems describe the story items the
	// completion had to carry forward so a later epic can still be turned in.
	// They are empty for the ordinary case where nothing outstanding needs one.
	PostCommitInventory dnfrepo.InventoryRecord
	CarryForwardItems   []FinishCommittedItem
	Idempotent          bool
}

type AutoCompletedLinkedSubQuestCompletion struct {
	ParentID    int64
	QuestID     int64
	Field       dnfrepo.QuestField
	Synthesized bool
}

type AutoCompletedLinkedSubQuestReconcilePlan struct {
	Record        dnfrepo.QuestRecord
	Completions   []AutoCompletedLinkedSubQuestCompletion
	ChangedFields []dnfrepo.QuestField
}

// PlanAutoCompleteMainQuests closes past PVF main quests below the requested
// level. The current EXE's zero selector is the bulk action: it includes both
// active and not-yet-accepted eligible main quests. Quest prerequisites,
// selected-answer branches, job restrictions, and collision constraints remain
// authoritative; this planner never invents a missing branch answer or grants
// rewards. A quest-clear parent closes together with its PVF-bound children so
// the completed bitmap and its displayed counters agree; see
// autoCompleteClosableQuestClearChild for the two children it refuses.
func (c *Catalog) PlanAutoCompleteMainQuests(
	record dnfrepo.QuestRecord,
	character CharacterEligibility,
	cutoffLevel int,
	targetQuestID int64,
	completedAt time.Time,
) (AutoCompleteMainPlan, error) {
	if c == nil {
		return AutoCompleteMainPlan{}, ErrCatalogEmpty
	}
	if cutoffLevel <= 0 || cutoffLevel >= character.Level || !definitionCharacterJobValid(character) {
		return AutoCompleteMainPlan{}, ErrAutoCompleteLevelInvalid
	}
	if completedAt.IsZero() {
		return AutoCompleteMainPlan{}, ErrAutoCompleteMainUnavailable
	}

	plan := AutoCompleteMainPlan{Record: dnfrepo.CloneQuest(record)}
	completed, _ := questStateSets(plan.Record)
	answers := questAnswerSelections(plan.Record)

	eligibleMain := func(definition Definition) bool {
		return c.autoCompleteMainEligible(definition, character, cutoffLevel)
	}
	prerequisitesMet := func(definition Definition, projectedCompleted map[int64]struct{}) bool {
		return autoCompleteMainPrerequisitesMet(definition, projectedCompleted, answers)
	}
	if targetQuestID == 0 {
		candidates := make([]int64, 0, 4)
		candidateSet := make(map[int64]struct{})
		projectedCompleted := make(map[int64]struct{}, len(completed))
		for questID := range completed {
			projectedCompleted[questID] = struct{}{}
		}
		for added := true; added; {
			added = false
			for _, definition := range c.ordered {
				if _, wasCompleted := completed[definition.ID]; wasCompleted {
					continue
				}
				if _, alreadySelected := candidateSet[definition.ID]; alreadySelected ||
					!eligibleMain(definition) || !prerequisitesMet(definition, projectedCompleted) {
					continue
				}
				state, _, stateKnown := mutableQuestState(&plan.Record, definition.ID)
				if stateKnown && !isActiveQuestStatus(state.Status) {
					continue
				}
				candidates = append(candidates, definition.ID)
				candidateSet[definition.ID] = struct{}{}
				projectedCompleted[definition.ID] = struct{}{}
				added = true
			}
		}
		if len(candidates) == 0 {
			// An empty bulk result is rarely "nothing left to do": the story
			// chain above the first awakening level is gated on the awakening
			// quest group, and that group is the one thing a bulk pass must
			// never complete for the player. Name the real cause so the caller
			// can say what to run instead of reporting an empty result.
			if pending, blocked := c.pendingAwakeningMainQuest(
				plan.Record, completed, answers, character, cutoffLevel,
			); blocked {
				return AutoCompleteMainPlan{}, &AwakeningRequiredError{
					QuestID: pending.ID, QuestName: strings.TrimSpace(pending.Name),
				}
			}
			if pending, blocked := c.pendingChoiceMainQuest(
				plan.Record, completed, answers, character, cutoffLevel,
			); blocked {
				return AutoCompleteMainPlan{}, &ChoiceRequiredError{
					QuestID: pending.ID, QuestName: strings.TrimSpace(pending.Name),
				}
			}
			if autoCompleteMainRequiresClassChange(character, cutoffLevel) {
				return AutoCompleteMainPlan{}, ErrAutoCompleteClassRequired
			}
			return AutoCompleteMainPlan{}, ErrAutoCompleteTargetInvalid
		}
		for _, candidateID := range candidates {
			definition, definitionKnown := c.Find(candidateID)
			state, field, stateKnown := mutableQuestState(&plan.Record, candidateID)
			if !definitionKnown || !eligibleMain(definition) {
				return AutoCompleteMainPlan{}, ErrAutoCompleteTargetInvalid
			}
			if !stateKnown {
				state = dnfrepo.QuestState{}
				field = dnfrepo.QuestFieldStates
				if plan.Record.States == nil {
					plan.Record.States = make(map[int64]dnfrepo.QuestState)
				}
			} else if !isActiveQuestStatus(state.Status) {
				return AutoCompleteMainPlan{}, ErrAutoCompleteTargetInvalid
			}
			state = autoCompleteMainQuestState(state, definition, cutoffLevel, completedAt)
			if field == dnfrepo.QuestFieldStates {
				plan.Record.States[candidateID] = state
			} else {
				plan.Record.Progress[candidateID] = state
			}
			plan.EligibleQuestIDs = append(plan.EligibleQuestIDs, candidateID)
			plan.ChangedQuestIDs = append(plan.ChangedQuestIDs, candidateID)
			plan.ChangedFields = mergeQuestChangedField(plan.ChangedFields, field)
			plan.ActiveCompletedCount++
			if candidateID > plan.HighestCompletedQuest {
				plan.HighestCompletedQuest = candidateID
			}
		}
	} else {
		definition, definitionKnown := c.Find(targetQuestID)
		if !definitionKnown || !eligibleMain(definition) || !prerequisitesMet(definition, completed) {
			return AutoCompleteMainPlan{}, ErrAutoCompleteTargetInvalid
		}
		state, field, stateKnown := mutableQuestState(&plan.Record, targetQuestID)
		if !stateKnown {
			return AutoCompleteMainPlan{}, ErrAutoCompleteTargetInvalid
		}
		plan.EligibleQuestIDs = []int64{targetQuestID}
		plan.HighestCompletedQuest = targetQuestID
		if isCompletedQuestStatus(state.Status) {
			return plan, nil
		}
		if !isActiveQuestStatus(state.Status) {
			return AutoCompleteMainPlan{}, ErrAutoCompleteTargetInvalid
		}
		plan.ActiveCompletedCount = 1
		state = autoCompleteMainQuestState(state, definition, cutoffLevel, completedAt)
		if field == dnfrepo.QuestFieldStates {
			plan.Record.States[targetQuestID] = state
		} else {
			plan.Record.Progress[targetQuestID] = state
		}
		plan.ChangedFields = mergeQuestChangedField(plan.ChangedFields, field)
		plan.ChangedQuestIDs = []int64{targetQuestID}
	}
	// Close active no-reward linked children owned by the main quests in the
	// auto-clear closure. Unaccepted children are left untouched so the
	// operation never invents task rows that the character did not receive.
	plannedParents := make(map[int64]struct{}, len(plan.ChangedQuestIDs))
	for _, parentID := range plan.ChangedQuestIDs {
		plannedParents[parentID] = struct{}{}
		parent, parentKnown := c.Find(parentID)
		if !parentKnown {
			return AutoCompleteMainPlan{}, ErrAutoCompleteTargetInvalid
		}
		children, safe := c.autoCompletableQuestClearChildren(parent)
		if !safe {
			return AutoCompleteMainPlan{}, ErrAutoCompleteTargetInvalid
		}
		for _, child := range children {
			completion, changed := c.completeAutoCompletedLinkedSubQuest(&plan.Record, parentID, child, completedAt)
			if !changed {
				continue
			}
			plan.ChangedFields = mergeQuestChangedField(plan.ChangedFields, completion.Field)
			plan.ChangedLinkedSubIDs = appendQuestIDOnce(plan.ChangedLinkedSubIDs, completion.QuestID)
		}
	}
	for _, child := range c.ordered {
		if child.MainQuestID <= 0 || !shouldAutoCompleteNoRewardSubQuest(child) {
			continue
		}
		if _, parentPlanned := plannedParents[child.MainQuestID]; !parentPlanned {
			continue
		}
		state, field, known := mutableQuestState(&plan.Record, child.ID)
		if !known || !isActiveQuestStatus(state.Status) {
			continue
		}
		if !autoCompleteNoRewardSubQuestState(&state, child, completedAt) {
			continue
		}
		switch field {
		case dnfrepo.QuestFieldStates:
			plan.Record.States[child.ID] = state
		case dnfrepo.QuestFieldProgress:
			plan.Record.Progress[child.ID] = state
		}
		plan.ChangedFields = mergeQuestChangedField(plan.ChangedFields, field)
		plan.ChangedLinkedSubIDs = appendQuestIDOnce(plan.ChangedLinkedSubIDs, child.ID)
	}
	sort.Slice(plan.ChangedLinkedSubIDs, func(i, j int) bool { return plan.ChangedLinkedSubIDs[i] < plan.ChangedLinkedSubIDs[j] })
	if len(plan.ChangedFields) != 0 {
		plan.Record.UpdatedAt = completedAt
	}
	return plan, nil
}

// PlanAutoCompletedLinkedSubQuestReconciliation repairs rows produced by the
// older op1429 implementation, which could persist a completed quest-clear
// parent while leaving its unaccepted child objectives absent. Only the exact
// PVF-bound no-reward closure is materialized; ambiguous or reward-bearing
// graphs remain untouched.
func (c *Catalog) PlanAutoCompletedLinkedSubQuestReconciliation(
	record dnfrepo.QuestRecord,
	reconciledAt time.Time,
) (AutoCompletedLinkedSubQuestReconcilePlan, error) {
	if c == nil {
		return AutoCompletedLinkedSubQuestReconcilePlan{}, ErrCatalogEmpty
	}
	if reconciledAt.IsZero() {
		return AutoCompletedLinkedSubQuestReconcilePlan{}, ErrAutoCompleteMainUnavailable
	}
	plan := AutoCompletedLinkedSubQuestReconcilePlan{Record: dnfrepo.CloneQuest(record)}
	parentIDs := make([]int64, 0, len(record.States)+len(record.Progress))
	seen := make(map[int64]struct{}, len(record.States)+len(record.Progress))
	for questID := range record.Progress {
		seen[questID] = struct{}{}
		parentIDs = append(parentIDs, questID)
	}
	for questID := range record.States {
		if _, exists := seen[questID]; !exists {
			parentIDs = append(parentIDs, questID)
		}
	}
	sort.Slice(parentIDs, func(i, j int) bool { return parentIDs[i] < parentIDs[j] })
	for _, parentID := range parentIDs {
		state, known := questStateFor(plan.Record, parentID)
		if !known || !isCompletedQuestStatus(state.Status) || state.Extra["auto_completed"] != "true" ||
			state.Extra["auto_complete_reason"] != "epic_below_character_level_no_reward" {
			continue
		}
		parent, parentKnown := c.Find(parentID)
		if !parentKnown {
			continue
		}
		children, safe := c.autoCompletableQuestClearChildren(parent)
		if !safe {
			continue
		}
		for _, child := range children {
			completion, changed := c.completeAutoCompletedLinkedSubQuest(&plan.Record, parentID, child, reconciledAt)
			if !changed {
				continue
			}
			plan.Completions = append(plan.Completions, completion)
			plan.ChangedFields = mergeQuestChangedField(plan.ChangedFields, completion.Field)
		}
	}
	if len(plan.ChangedFields) != 0 {
		plan.Record.UpdatedAt = reconciledAt
	}
	return plan, nil
}

// isAwakeningQuest keeps the level-based no-reward shortcut away from both
// canonical story nodes and their profession-specific reward companions. A
// canonical first-awakening node has no profession reward of its own, so the
// generic epic filter must consult the strict collision index as well as the
// explicit PVF job-change/reward tags.
func (c *Catalog) isAwakeningQuest(definition Definition) bool {
	if c == nil {
		return false
	}
	if _, canonical := c.storyAwakeningByCanonical[definition.ID]; canonical {
		return true
	}
	if _, companion := c.storyAwakeningCanonicalFor[definition.ID]; companion {
		return true
	}
	return definition.JobChangeQuest > 0 || normalizeQuestTag(definition.RewardType) == "awakening type"
}

func (o *Owner) ApplyAutoCompleteMain(
	ctx context.Context,
	catalog *Catalog,
	input AutoCompleteMainInput,
) (AutoCompleteMainResult, error) {
	if o == nil || o.repositories.CharacterSettlement == nil || catalog == nil {
		return AutoCompleteMainResult{}, ErrAutoCompleteMainUnavailable
	}
	input.CharacterID = strings.TrimSpace(input.CharacterID)
	if input.CharacterID == "" || input.CompletedAt.IsZero() {
		return AutoCompleteMainResult{}, ErrCharacterRequired
	}
	if input.CutoffLevel <= 0 || input.CutoffLevel >= input.Eligibility.Level {
		return AutoCompleteMainResult{}, ErrAutoCompleteLevelInvalid
	}
	if ctx == nil {
		ctx = context.Background()
	}

	var plan AutoCompleteMainPlan
	var postInventory dnfrepo.InventoryRecord
	var carryForwardItems []FinishCommittedItem
	err := o.repositories.CharacterSettlement.WithinCharacterSettlement(ctx, input.CharacterID, func(tx dnfrepo.Group) error {
		character, found, err := tx.Character.Load(ctx, input.CharacterID)
		if err != nil {
			return err
		}
		if !found {
			return ErrCharacterNotFound
		}
		if character.Level != input.Eligibility.Level || input.CutoffLevel >= character.Level {
			return ErrAutoCompleteLevelInvalid
		}
		record, found, err := tx.Quest.Load(ctx, input.CharacterID)
		if err != nil {
			return err
		}
		if !found {
			record = dnfrepo.QuestRecord{CharacterID: input.CharacterID}
		} else if strings.TrimSpace(record.CharacterID) != input.CharacterID {
			return ErrQuestPersistVerify
		}
		plan, err = catalog.PlanAutoCompleteMainQuests(record, input.Eligibility, input.CutoffLevel, input.TargetQuestID, input.CompletedAt)
		if err != nil {
			return err
		}
		plan.Record.CharacterID = input.CharacterID
		// The item repair runs against the planned record, so a producer that is
		// being completed right now already counts, and it runs even when the
		// quest plan itself is idempotent: a replay must still be able to hand
		// over an item the previous attempt could not allocate.
		carryForwardItems, postInventory, err = repairAutoCompletedQuestCarryForwardItems(
			ctx,
			catalog,
			tx.Inventory,
			input.CharacterID,
			plan.Record,
			input.Eligibility,
			input.CompletedAt,
			input.AllocateItem,
		)
		if err != nil {
			return err
		}
		if len(plan.ChangedFields) == 0 {
			return nil
		}
		return dnfrepo.SaveQuestFields(ctx, tx.Quest, plan.Record, plan.ChangedFields...)
	})
	if err != nil {
		return AutoCompleteMainResult{}, err
	}

	persisted, found, err := o.quests.Load(ctx, input.CharacterID)
	if err != nil {
		return AutoCompleteMainResult{}, err
	}
	if !found {
		return AutoCompleteMainResult{}, ErrQuestPersistVerify
	}
	for _, questID := range append(append([]int64(nil), plan.EligibleQuestIDs...), plan.ChangedLinkedSubIDs...) {
		state, known := questStateFor(persisted, questID)
		if !known || !isCompletedQuestStatus(state.Status) {
			return AutoCompleteMainResult{}, fmt.Errorf("%w: quest=%d", ErrQuestPersistVerify, questID)
		}
	}
	return AutoCompleteMainResult{
		CharacterID:           input.CharacterID,
		CutoffLevel:           input.CutoffLevel,
		EligibleQuestIDs:      append([]int64(nil), plan.EligibleQuestIDs...),
		ChangedQuestIDs:       append([]int64(nil), plan.ChangedQuestIDs...),
		ChangedLinkedSubIDs:   append([]int64(nil), plan.ChangedLinkedSubIDs...),
		ActiveCompletedCount:  plan.ActiveCompletedCount,
		HighestCompletedQuest: plan.HighestCompletedQuest,
		PostCommitQuest:       dnfrepo.CloneQuest(persisted),
		PostCommitInventory:   dnfrepo.CloneInventory(postInventory),
		CarryForwardItems:     cloneFinishItems(carryForwardItems),
		Idempotent:            len(plan.ChangedFields) == 0 && len(carryForwardItems) == 0,
	}, nil
}

func definitionCharacterJobValid(character CharacterEligibility) bool {
	return character.Level > 0 && jobmap.Valid(character.Job)
}

// autoCompleteMainEligible is the bulk pass's own view of one definition.
// wantAwakening selects which side of the awakening split is wanted: false for
// the quests the pass may complete, true for the ones it must leave to the
// player and only reports on.
func (c *Catalog) autoCompleteMainEligible(
	definition Definition,
	character CharacterEligibility,
	cutoffLevel int,
) bool {
	return c.autoCompleteMainShapeEligible(definition, character, cutoffLevel) &&
		// A PVF [question] quest is the player's own faction/branch choice. Its
		// successors are gated by [pre required quest answer]; completing it
		// here would either record no answer or silently select branch zero,
		// which strands the rest of the chain for good.
		normalizeQuestTag(definition.Type) != "question" &&
		!c.isAwakeningQuest(definition)
}

// autoCompleteMainShapeEligible is everything the bulk pass asks of a
// definition apart from the two exclusions layered on top of it: awakening and
// [question] branch quests. Keeping the shape separate is what lets the
// "nothing to complete" answer name which of the two is actually holding the
// chain up.
func (c *Catalog) autoCompleteMainShapeEligible(
	definition Definition,
	character CharacterEligibility,
	cutoffLevel int,
) bool {
	grade := normalizeQuestTag(definition.Grade)
	// The current PVF identifies 3054 and 3157 as [sub], but both belong to
	// the verified early main-story chain and must participate in op1429.
	if grade != "epic" && definition.ID != 3054 && definition.ID != 3157 {
		return false
	}
	_, linkedChildrenSafe := c.autoCompletableQuestClearChildren(definition)
	return definition.LevelMin > 0 && definition.LevelMin <= cutoffLevel &&
		definition.ExposedByNPC != 0 && !definition.IsEvent &&
		linkedChildrenSafe &&
		definitionMatchesCharacter(definition, character)
}

func autoCompleteMainPrerequisitesMet(
	definition Definition,
	projectedCompleted map[int64]struct{},
	answers map[int64]int64,
) bool {
	if definition.MalformedPreAnswers {
		return false
	}
	if definition.MainQuestID > 0 {
		if _, parentCompleted := projectedCompleted[definition.MainQuestID]; !parentCompleted {
			return false
		}
	}
	if len(definition.PreRequiredGroups) > 0 {
		matched := false
		for _, group := range definition.PreRequiredGroups {
			if completedAll(group, projectedCompleted) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	for _, requirement := range definition.PreRequiredAnswers {
		answer, known := answers[requirement.QuestID]
		if !known || answer != requirement.Answer {
			return false
		}
	}
	for _, collision := range definition.CollisionQuests {
		if collision <= 0 {
			continue
		}
		if _, collisionCompleted := projectedCompleted[collision]; collisionCompleted {
			return false
		}
	}
	return true
}

// pendingAwakeningMainQuest returns the awakening quest the character may take
// right now: eligible, prerequisites met, not completed, and either unaccepted
// or still active. It is what stands between an empty bulk result and the rest
// of the story chain.
func (c *Catalog) pendingAwakeningMainQuest(
	record dnfrepo.QuestRecord,
	completed map[int64]struct{},
	answers map[int64]int64,
	character CharacterEligibility,
	cutoffLevel int,
) (Definition, bool) {
	return c.pendingExcludedMainQuest(record, completed, answers, character, cutoffLevel,
		func(definition Definition) bool {
			return normalizeQuestTag(definition.Type) != "question" && c.isAwakeningQuest(definition)
		})
}

// pendingChoiceMainQuest returns the [question] branch quest the character may
// answer right now. The story chain past it is gated on
// [pre required quest answer], so while it sits unanswered the bulk pass has
// nothing left it may touch - and the quest is still in the player's list.
func (c *Catalog) pendingChoiceMainQuest(
	record dnfrepo.QuestRecord,
	completed map[int64]struct{},
	answers map[int64]int64,
	character CharacterEligibility,
	cutoffLevel int,
) (Definition, bool) {
	return c.pendingExcludedMainQuest(record, completed, answers, character, cutoffLevel,
		func(definition Definition) bool {
			return normalizeQuestTag(definition.Type) == "question" && !c.isAwakeningQuest(definition)
		})
}

// pendingExcludedMainQuest finds the lowest-level quest the character may take
// right now among the ones the bulk pass deliberately leaves alone: shape
// eligible, prerequisites met, not completed, and either unaccepted or active.
func (c *Catalog) pendingExcludedMainQuest(
	record dnfrepo.QuestRecord,
	completed map[int64]struct{},
	answers map[int64]int64,
	character CharacterEligibility,
	cutoffLevel int,
	excluded func(Definition) bool,
) (Definition, bool) {
	var pending Definition
	found := false
	for _, definition := range c.ordered {
		if _, wasCompleted := completed[definition.ID]; wasCompleted {
			continue
		}
		if !excluded(definition) ||
			!c.autoCompleteMainShapeEligible(definition, character, cutoffLevel) ||
			!autoCompleteMainPrerequisitesMet(definition, completed, answers) {
			continue
		}
		if state, _, stateKnown := mutableQuestState(&record, definition.ID); stateKnown &&
			!isActiveQuestStatus(state.Status) {
			continue
		}
		if !found || definition.LevelMin < pending.LevelMin ||
			(definition.LevelMin == pending.LevelMin && definition.ID < pending.ID) {
			pending, found = definition, true
		}
	}
	return pending, found
}

// autoCompleteMainRequiresClassChange reports whether the bulk request can only
// end in an empty candidate set because the character never took its class
// change. grow_type packs the first transition in the low nibble; jobs 9 and 10
// have no class change to take.
func autoCompleteMainRequiresClassChange(character CharacterEligibility, cutoffLevel int) bool {
	if cutoffLevel < autoCompleteMainFirstAwakeningLevel {
		return false
	}
	if firstGrowType, _ := profession.Decode(byte(character.GrowType)); firstGrowType != 0 {
		return false
	}
	return character.Job != 9 && character.Job != 10
}

func sortedQuestIDSet(values map[int64]struct{}) []int64 {
	ids := make([]int64, 0, len(values))
	for questID := range values {
		ids = append(ids, questID)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

func autoCompleteMainQuestState(state dnfrepo.QuestState, definition Definition, cutoffLevel int, completedAt time.Time) dnfrepo.QuestState {
	state.Status = "completed"
	state.ProgressValue = 0
	state.RewardSelectIndex = 0
	state.UpdatedAt = completedAt
	if state.Extra == nil {
		state.Extra = make(map[string]string, 8)
	}
	if state.Extra["pvf_path"] == "" {
		state.Extra["pvf_path"] = definition.Path
	}
	if state.Extra["quest_type"] == "" {
		state.Extra["quest_type"] = definition.Type
	}
	state.Extra["reward_state"] = finishRewardGranted
	state.Extra["auto_completed"] = "true"
	state.Extra["auto_complete_reason"] = "epic_below_character_level_no_reward"
	state.Extra["auto_complete_level_cutoff"] = strconv.Itoa(cutoffLevel)
	return state
}

func shouldAutoCompleteNoRewardSubQuest(definition Definition) bool {
	rewardType := normalizeQuestTag(definition.RewardType)
	legacyEmptyReward := rewardType == ""
	zeroItemPlaceholder := definition.NoExperience && rewardType == "item" &&
		len(definition.RewardIntData) == 2 &&
		definition.RewardIntData[0] == 0 && definition.RewardIntData[1] == 0
	return normalizeQuestTag(definition.Grade) == "sub" &&
		(legacyEmptyReward || zeroItemPlaceholder) && definition.RewardParseError == "" &&
		!definition.HasGoldReward && emptyRewardDataValid(definition.RewardIntData) &&
		len(definition.RewardItems) == 0 && len(definition.RewardSelectionItems) == 0
}

func (c *Catalog) autoCompletableQuestClearChildren(parent Definition) ([]Definition, bool) {
	tag := normalizeQuestTag(parent.Type)
	if tag != "quest clear" && tag != "clear quest" {
		return nil, true
	}
	// Only subtype 2 has the proved parent/int-data child graph. Preserve the
	// existing policy for other legacy quest-clear encodings instead of
	// guessing that their integers are linked quest IDs.
	if parent.SubType != 2 {
		return nil, true
	}
	children, ok := c.parentBoundChildren(parent)
	if !ok {
		return nil, false
	}
	for _, child := range children {
		if !c.autoCompleteClosableQuestClearChild(child) {
			return nil, false
		}
	}
	return children, true
}

// autoCompleteClosableQuestClearChild reports whether one PVF-bound child of a
// [quest clear] parent may be closed together with it.
//
// The rule the live client's task manual implements is "complete every main
// quest below the character's level, except class-change and awakening
// quests". An item reward on a child is therefore not a reason to refuse the
// parent: op1429 suppresses rewards everywhere, and the story items a later
// epic still has to hand in are restored by autoCompleteCarryForwardGrants,
// which sums exactly this child closure. Requiring every child to be a
// no-reward objective instead stranded whole regions - MirrorMelt's 3458
// 阿尔伯特的请求 has a child (3472) that awards two 毁灭纪碎片, so the pass stopped
// dead in front of it and the quest sat in the player's list for good.
//
// Two refusals remain, because neither is the pass's call to make: a
// [question] branch is the player's own faction choice, and a class-change or
// awakening step has to be run by the player.
func (c *Catalog) autoCompleteClosableQuestClearChild(child Definition) bool {
	if child.ID <= 0 || normalizeQuestTag(child.Type) == "question" {
		return false
	}
	return !c.isAwakeningQuest(child)
}

func (c *Catalog) completeAutoCompletedLinkedSubQuest(
	record *dnfrepo.QuestRecord,
	parentID int64,
	child Definition,
	completedAt time.Time,
) (AutoCompletedLinkedSubQuestCompletion, bool) {
	if record == nil || parentID <= 0 || child.ID <= 0 || child.MainQuestID != parentID ||
		!c.autoCompleteClosableQuestClearChild(child) {
		return AutoCompletedLinkedSubQuestCompletion{}, false
	}
	state, field, known := mutableQuestState(record, child.ID)
	if known && isCompletedQuestStatus(state.Status) {
		return AutoCompletedLinkedSubQuestCompletion{}, false
	}
	synthesized := !known
	if synthesized {
		field = dnfrepo.QuestFieldStates
		if record.States == nil {
			record.States = make(map[int64]dnfrepo.QuestState)
		}
	}
	if !autoCompleteNoRewardSubQuestState(&state, child, completedAt) {
		// A reward-bearing child still closes with the parent, with its reward
		// suppressed like every other op1429 completion.
		autoCompleteQuestClearChildState(&state, child, completedAt)
	}
	state.Extra["auto_completed_by_parent"] = strconv.FormatInt(parentID, 10)
	state.Extra["main_quest_id"] = strconv.FormatInt(parentID, 10)
	if state.Extra["pvf_path"] == "" {
		state.Extra["pvf_path"] = child.Path
	}
	switch field {
	case dnfrepo.QuestFieldStates:
		record.States[child.ID] = state
	case dnfrepo.QuestFieldProgress:
		record.Progress[child.ID] = state
	default:
		return AutoCompletedLinkedSubQuestCompletion{}, false
	}
	return AutoCompletedLinkedSubQuestCompletion{
		ParentID:    parentID,
		QuestID:     child.ID,
		Field:       field,
		Synthesized: synthesized,
	}, true
}

func appendQuestIDOnce(values []int64, questID int64) []int64 {
	for _, existing := range values {
		if existing == questID {
			return values
		}
	}
	return append(values, questID)
}

// autoCompleteQuestClearChildState closes a PVF-bound child that carries its
// own reward. The reward is dropped, exactly like the parent's: op1429 grants
// nothing, and story items a later epic still needs come back through
// autoCompleteCarryForwardGrants instead.
func autoCompleteQuestClearChildState(state *dnfrepo.QuestState, child Definition, completedAt time.Time) {
	if state == nil {
		return
	}
	state.Status = "completed"
	state.ProgressValue = 0
	state.RewardSelectIndex = 0
	state.UpdatedAt = completedAt
	if state.Extra == nil {
		state.Extra = make(map[string]string, 8)
	}
	state.Extra["reward_state"] = finishRewardGranted
	state.Extra["auto_completed"] = "true"
	state.Extra["auto_complete_reason"] = "quest_clear_parent_closure"
	if state.Extra["quest_type"] == "" {
		state.Extra["quest_type"] = child.Type
	}
}

func autoCompleteNoRewardSubQuestState(state *dnfrepo.QuestState, definition Definition, completedAt time.Time) bool {
	if state == nil || !shouldAutoCompleteNoRewardSubQuest(definition) {
		return false
	}
	state.Status = "completed"
	state.ProgressValue = 0
	state.UpdatedAt = completedAt
	if state.Extra == nil {
		state.Extra = make(map[string]string, 8)
	}
	state.Extra["reward_state"] = finishRewardGranted
	state.Extra["auto_completed"] = "true"
	state.Extra["auto_complete_reason"] = "no_reward_sub_quest"
	return true
}

func mergeQuestChangedField(current []dnfrepo.QuestField, field dnfrepo.QuestField) []dnfrepo.QuestField {
	if field == "" {
		return current
	}
	for _, existing := range current {
		if existing == field {
			return current
		}
	}
	return append(current, field)
}

type LinkedSubQuestActivation struct {
	ParentID       int64
	QuestID        int64
	Field          dnfrepo.QuestField
	InitTrigger    uint32
	PVFPath        string
	QuestType      string
	AlreadyPresent bool
}

type LinkedSubQuestActivationPlan struct {
	Record        dnfrepo.QuestRecord
	Activations   []LinkedSubQuestActivation
	ChangedFields []dnfrepo.QuestField
}

func (c *Catalog) PlanActiveQuestClearLinkedSubQuestActivation(record dnfrepo.QuestRecord, activatedAt time.Time) (LinkedSubQuestActivationPlan, error) {
	if c == nil {
		return LinkedSubQuestActivationPlan{}, ErrCatalogEmpty
	}
	if activatedAt.IsZero() {
		activatedAt = time.Now()
	}
	plan := LinkedSubQuestActivationPlan{Record: dnfrepo.CloneQuest(record)}
	plan.Activations, plan.ChangedFields = c.activateMissingLinkedNoRewardSubQuests(&plan.Record, activatedAt, "")
	if len(plan.ChangedFields) > 0 {
		plan.Record.UpdatedAt = activatedAt
	}
	return plan, nil
}

func (c *Catalog) activateMissingLinkedNoRewardSubQuests(
	record *dnfrepo.QuestRecord,
	activatedAt time.Time,
	childType string,
) ([]LinkedSubQuestActivation, []dnfrepo.QuestField) {
	if c == nil {
		return nil, nil
	}
	childType = normalizeQuestTag(childType)
	completed, _ := questStateSets(*record)
	activations := make([]LinkedSubQuestActivation, 0)
	changedFields := make([]dnfrepo.QuestField, 0, 2)
	seenChildren := make(map[int64]struct{})
	collect := func(field dnfrepo.QuestField, states map[int64]dnfrepo.QuestState) {
		if len(states) == 0 {
			return
		}
		parentIDs := make([]int64, 0, len(states))
		for questID := range states {
			parentIDs = append(parentIDs, questID)
		}
		sort.Slice(parentIDs, func(left, right int) bool { return parentIDs[left] < parentIDs[right] })
		for _, parentID := range parentIDs {
			parentState := states[parentID]
			if !isActiveQuestStatus(parentState.Status) {
				continue
			}
			parent, known := c.Find(parentID)
			if !known {
				continue
			}
			parentType := normalizeQuestTag(parent.Type)
			if parentType != "quest clear" && parentType != "clear quest" {
				continue
			}
			for _, childID := range parent.IntData {
				if childID <= 0 {
					continue
				}
				if _, duplicate := seenChildren[childID]; duplicate {
					continue
				}
				if _, known := questStateFor(*record, childID); known {
					continue
				}
				child, known := c.Find(childID)
				if !known || child.MainQuestID != parent.ID || child.HasDependGiveItem ||
					(childType != "" && normalizeQuestTag(child.Type) != childType) ||
					!shouldAutoCompleteNoRewardSubQuest(child) {
					continue
				}
				seenChildren[childID] = struct{}{}
				initialTrigger := definitionInitialTrigger(child, completed)
				state := dnfrepo.QuestState{
					Status:        "active",
					ProgressValue: int64(initialTrigger),
					UpdatedAt:     activatedAt,
					Extra: map[string]string{
						"pvf_path":                              child.Path,
						"quest_type":                            child.Type,
						"main_quest_id":                         int64Text(parent.ID),
						"auto_activated_by_main_quest":          "true",
						"auto_activation_reason":                "active_quest_clear_parent_reconcile",
						"auto_activation_parent_progress_value": int64Text(parentState.ProgressValue),
					},
				}
				switch field {
				case dnfrepo.QuestFieldStates:
					if record.States == nil {
						record.States = make(map[int64]dnfrepo.QuestState, 1)
					}
					record.States[child.ID] = state
					changedFields = mergeQuestChangedField(changedFields, dnfrepo.QuestFieldStates)
				case dnfrepo.QuestFieldProgress:
					if record.Progress == nil {
						record.Progress = make(map[int64]dnfrepo.QuestState, 1)
					}
					record.Progress[child.ID] = state
					changedFields = mergeQuestChangedField(changedFields, dnfrepo.QuestFieldProgress)
				}
				activations = append(activations, LinkedSubQuestActivation{
					ParentID:    parent.ID,
					QuestID:     child.ID,
					Field:       field,
					InitTrigger: initialTrigger,
					PVFPath:     child.Path,
					QuestType:   child.Type,
				})
			}
		}
	}
	collect(dnfrepo.QuestFieldStates, record.States)
	collect(dnfrepo.QuestFieldProgress, record.Progress)
	return activations, changedFields
}

func int64Text(value int64) string {
	return strconv.FormatInt(value, 10)
}
