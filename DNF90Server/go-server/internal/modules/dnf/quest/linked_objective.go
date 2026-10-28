package quest

import (
	"math"
	"sort"
	"time"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

type LinkedObjectiveProgressPlan struct {
	Record            dnfrepo.QuestRecord
	ParentQuestID     int64
	ParentProgress    int64
	CompletedQuestIDs []int64
	ChangedFields     []dnfrepo.QuestField
}

func (c *Catalog) IsLinkedObjective(questID int64) bool {
	definition, ok := c.Find(questID)
	return ok && linkedObjectiveQuest(definition)
}

// PlanTownLinkedProgress handles the scalar parent callback used by NPC,
// inventory-presence and cinematic objectives. Combat and clear-map children
// remain owned by their dungeon path until the client closes them with op34.
func (c *Catalog) PlanTownLinkedProgress(record dnfrepo.QuestRecord, questID int64, completedAt time.Time) (LinkedObjectiveProgressPlan, error) {
	return c.planLinkedObjectiveProgress(record, questID, completedAt, true, false)
}

// PlanLinkedObjectiveFinish closes the requested no-reward child after its
// trigger has reached zero and recomputes its quest-clear parent atomically.
func (c *Catalog) PlanLinkedObjectiveFinish(record dnfrepo.QuestRecord, questID int64, completedAt time.Time) (LinkedObjectiveProgressPlan, error) {
	return c.planLinkedObjectiveProgress(record, questID, completedAt, false, true)
}

func (c *Catalog) planLinkedObjectiveProgress(
	record dnfrepo.QuestRecord,
	questID int64,
	completedAt time.Time,
	townOnly bool,
	requireRequestedChild bool,
) (LinkedObjectiveProgressPlan, error) {
	if c == nil {
		return LinkedObjectiveProgressPlan{}, ErrCatalogEmpty
	}
	if completedAt.IsZero() {
		completedAt = time.Now().UTC()
	}
	plan := LinkedObjectiveProgressPlan{Record: dnfrepo.CloneQuest(record)}
	requested, ok := c.Find(questID)
	if !ok {
		return plan, nil
	}

	parentID := requested.ID
	requestedIsChild := linkedObjectiveQuest(requested)
	if requestedIsChild {
		state, _, active := mutableQuestState(&plan.Record, requested.ID)
		if !active || !isActiveQuestStatus(state.Status) || state.ProgressValue != 0 {
			return plan, nil
		}
		parentID = requested.MainQuestID
	} else {
		if requireRequestedChild {
			return plan, nil
		}
		tag := normalizeQuestTag(requested.Type)
		if tag != "quest clear" && tag != "clear quest" {
			return plan, nil
		}
	}

	parent, ok := c.Find(parentID)
	if !ok {
		return plan, nil
	}
	parentTag := normalizeQuestTag(parent.Type)
	if parentTag != "quest clear" && parentTag != "clear quest" {
		return plan, nil
	}
	parentState, parentField, parentActive := mutableQuestState(&plan.Record, parentID)
	if !parentActive || !isActiveQuestStatus(parentState.Status) {
		return plan, nil
	}

	completed, _ := questStateSets(plan.Record)
	requestedCompleted := !requestedIsChild
	seen := make(map[int64]struct{}, len(parent.IntData))
	for _, childID := range parent.IntData {
		if childID <= 0 {
			continue
		}
		if _, duplicate := seen[childID]; duplicate {
			continue
		}
		seen[childID] = struct{}{}
		child, known := c.Find(childID)
		if !known || child.MainQuestID != parentID || !linkedObjectiveQuest(child) || (townOnly && !townLinkedObjectiveQuest(child)) {
			continue
		}
		if _, done := completed[childID]; done {
			if childID == questID {
				requestedCompleted = true
			}
			continue
		}
		state, field, active := mutableQuestState(&plan.Record, childID)
		if !active || !isActiveQuestStatus(state.Status) || state.ProgressValue != 0 {
			continue
		}
		state.Status = "completed"
		state.ProgressValue = 0
		state.UpdatedAt = completedAt
		if state.Extra == nil {
			state.Extra = make(map[string]string, 4)
		}
		state.Extra["reward_state"] = finishRewardGranted
		state.Extra["linked_objective_completed"] = "true"
		state.Extra["linked_objective_completion_source"] = "client_callback_without_reward"
		switch field {
		case dnfrepo.QuestFieldStates:
			plan.Record.States[childID] = state
		case dnfrepo.QuestFieldProgress:
			plan.Record.Progress[childID] = state
		}
		plan.ChangedFields = mergeQuestChangedField(plan.ChangedFields, field)
		plan.CompletedQuestIDs = append(plan.CompletedQuestIDs, childID)
		completed[childID] = struct{}{}
		if childID == questID {
			requestedCompleted = true
		}
	}
	if requireRequestedChild && !requestedCompleted {
		return LinkedObjectiveProgressPlan{Record: dnfrepo.CloneQuest(record)}, nil
	}
	if len(plan.CompletedQuestIDs) == 0 {
		return LinkedObjectiveProgressPlan{Record: dnfrepo.CloneQuest(record)}, nil
	}

	remaining := questClearParentTrigger(parent.IntData, completed)
	if parentState.ProgressValue != remaining {
		parentState.ProgressValue = remaining
		parentState.UpdatedAt = completedAt
		switch parentField {
		case dnfrepo.QuestFieldStates:
			plan.Record.States[parentID] = parentState
		case dnfrepo.QuestFieldProgress:
			plan.Record.Progress[parentID] = parentState
		}
		plan.ChangedFields = mergeQuestChangedField(plan.ChangedFields, parentField)
	}
	plan.ParentQuestID = parentID
	plan.ParentProgress = remaining
	plan.Record.UpdatedAt = completedAt
	sort.Slice(plan.CompletedQuestIDs, func(i, j int) bool { return plan.CompletedQuestIDs[i] < plan.CompletedQuestIDs[j] })
	return plan, nil
}

func linkedObjectiveQuest(definition Definition) bool {
	return definition.NoExperience && shouldAutoCompleteNoRewardSubQuest(definition)
}

func zeroItemRewardPlaceholder(definition Definition) bool {
	return normalizeQuestTag(definition.RewardType) == "item" &&
		len(definition.RewardIntData) == 2 && definition.RewardIntData[0] == 0 && definition.RewardIntData[1] == 0 &&
		definition.RewardParseError == "" && !definition.HasGoldReward &&
		len(definition.RewardItems) == 0 && len(definition.RewardSelectionItems) == 0
}

func rewardBearingParentChild(definition Definition) bool {
	if normalizeQuestTag(definition.Grade) != "sub" || !definition.NoExperience ||
		normalizeQuestTag(definition.RewardType) != "item" || definition.RewardParseError != "" || definition.HasGoldReward {
		return false
	}
	return !emptyRewardDataValid(definition.RewardIntData) || len(definition.RewardItems) != 0 || len(definition.RewardSelectionItems) != 0
}

func (c *Catalog) parentBoundChildren(parent Definition) ([]Definition, bool) {
	if c == nil || (normalizeQuestTag(parent.Type) != "quest clear" && normalizeQuestTag(parent.Type) != "clear quest") || parent.SubType != 2 {
		return nil, false
	}
	children := make([]Definition, 0, len(parent.IntData))
	seen := make(map[int64]struct{}, len(parent.IntData))
	for _, childID := range parent.IntData {
		if childID <= 0 {
			continue
		}
		if _, duplicate := seen[childID]; duplicate {
			continue
		}
		seen[childID] = struct{}{}
		child, found := c.byID[childID]
		if !found || child.ID > math.MaxUint16 || child.MainQuestID != parent.ID || normalizeQuestTag(child.Grade) != "sub" || !child.NoExperience {
			return nil, false
		}
		children = append(children, cloneDefinition(child))
	}
	return children, len(children) != 0
}

func (c *Catalog) completedLinkedChildrenForParentFinish(parent Definition, record dnfrepo.QuestRecord) []int64 {
	if c == nil || !zeroItemRewardPlaceholder(parent) {
		return nil
	}
	parentState, active := questStateFor(record, parent.ID)
	if !active || !isActiveQuestStatus(parentState.Status) || parentState.ProgressValue != 0 {
		return nil
	}
	children, ok := c.parentBoundChildren(parent)
	if !ok {
		return nil
	}
	completed, _ := questStateSets(record)
	result := make([]int64, 0, len(children))
	for _, child := range children {
		if !linkedObjectiveQuest(child) {
			continue
		}
		if _, done := completed[child.ID]; done {
			continue
		}
		state, known := questStateFor(record, child.ID)
		if known && isActiveQuestStatus(state.Status) && state.ProgressValue == 0 {
			result = append(result, child.ID)
		}
	}
	return result
}

func townLinkedObjectiveQuest(definition Definition) bool {
	if !linkedObjectiveQuest(definition) {
		return false
	}
	switch normalizeQuestTag(definition.Type) {
	case "meet npc", "seek n meet npc", "seeking", "look cinematic":
		return true
	default:
		return false
	}
}
