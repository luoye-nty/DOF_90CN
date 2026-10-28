package quest

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

var (
	ErrClearMapTargetRequired         = errors.New("dnf clear-map dungeon or map target is required")
	ErrClearMapCompletionKeyRequired  = errors.New("dnf clear-map completion key is required")
	ErrClearMapCompletionTimeRequired = errors.New("dnf clear-map completion time is required")
)

const (
	clearMapRewardPending  = "pending"
	clearMapCompletionKind = "clear_map"
)

// ClearMapCompletionInput contains only facts owned by an authoritative final
// dungeon completion. CompletionKey must be stable for replays of that run.
type ClearMapCompletionInput struct {
	DungeonID         int64
	MapID             int64
	WorldMapID        int64
	Difficulty        int64
	HellPartyComplete bool
	AllRoomsCleared   bool
	WeddingCouple     bool // Exactly the engaged pair completed an ordinary dungeon.
	CompletionKey     string
	CompletedAt       time.Time
}

type ClearMapCompletion struct {
	QuestID         int64
	Field           dnfrepo.QuestField
	PreviousTrigger int64
	NewTrigger      int64
	Completed       bool
	PVFPath         string
	Idempotent      bool
}

// ClearMapCompletionPlan is a repository-independent mutation plan. Record is
// a deep clone; no caller-owned map is modified.
type ClearMapCompletionPlan struct {
	Record        dnfrepo.QuestRecord
	Completions   []ClearMapCompletion
	ChangedFields []dnfrepo.QuestField
}

// PlanClearMapCompletion advances matching active clear-map and supported
// condition-under-clear triggers. Trigger zero is the quest-complete/pending-
// reward state used by the existing quest snapshot. This plan grants no
// assets or experience; those mutations require a wider transaction.
func (c *Catalog) PlanClearMapCompletion(record dnfrepo.QuestRecord, input ClearMapCompletionInput) (ClearMapCompletionPlan, error) {
	if c == nil {
		return ClearMapCompletionPlan{}, ErrCatalogEmpty
	}
	if input.DungeonID <= 0 && input.MapID <= 0 {
		return ClearMapCompletionPlan{}, ErrClearMapTargetRequired
	}
	input.CompletionKey = strings.TrimSpace(input.CompletionKey)
	if input.CompletionKey == "" {
		return ClearMapCompletionPlan{}, ErrClearMapCompletionKeyRequired
	}
	if input.CompletedAt.IsZero() {
		return ClearMapCompletionPlan{}, ErrClearMapCompletionTimeRequired
	}

	plan := ClearMapCompletionPlan{Record: dnfrepo.CloneQuest(record)}
	statesChanged := false
	progressChanged := false
	_, activationFields := c.activateMissingLinkedNoRewardSubQuests(&plan.Record, input.CompletedAt, "")
	for _, field := range activationFields {
		switch field {
		case dnfrepo.QuestFieldStates:
			statesChanged = true
		case dnfrepo.QuestFieldProgress:
			progressChanged = true
		}
	}
	seen := make(map[int64]struct{})
	visit := func(states map[int64]dnfrepo.QuestState, field dnfrepo.QuestField) {
		ids := make([]int64, 0, len(states))
		for questID := range states {
			ids = append(ids, questID)
		}
		sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
		for _, questID := range ids {
			if _, duplicate := seen[questID]; duplicate {
				continue
			}
			seen[questID] = struct{}{}
			state := states[questID]
			if !isActiveQuestStatus(state.Status) {
				continue
			}
			definition, ok := c.byID[questID]
			if !ok {
				continue
			}
			channel, matched := clearCompletionMatchChannel(definition, input)
			if !matched {
				continue
			}
			completion := ClearMapCompletion{
				QuestID:         questID,
				Field:           field,
				PreviousTrigger: state.ProgressValue,
				PVFPath:         definition.Path,
			}
			if state.ProgressValue < 0 {
				// Repository corruption is not a completed clear-map trigger and
				// must never be normalized into a rewardable state here.
				continue
			}
			if state.ProgressValue > 0 && state.Extra["completion_key"] == input.CompletionKey {
				completion.Idempotent = true
				completion.NewTrigger = state.ProgressValue
				plan.Completions = append(plan.Completions, completion)
				continue
			}
			if state.ProgressValue == 0 {
				completion.Idempotent = true
				completion.NewTrigger = 0
				completion.Completed = true
				if !c.retainTerminalLinkedObjective(plan.Record, definition) &&
					autoCompleteNoRewardSubQuestState(&state, definition, input.CompletedAt) {
					states[questID] = state
					switch field {
					case dnfrepo.QuestFieldStates:
						statesChanged = true
					case dnfrepo.QuestFieldProgress:
						progressChanged = true
					}
				}
				plan.Completions = append(plan.Completions, completion)
				continue
			}
			newTrigger := int64(0)
			if normalizeQuestTag(definition.Type) == "condition under clear" {
				newTrigger = int64(decrementClearTriggerChannel(uint32(state.ProgressValue), channel))
			}
			state.ProgressValue = newTrigger
			completion.NewTrigger = newTrigger
			completion.Completed = newTrigger == 0
			state.UpdatedAt = input.CompletedAt
			if state.Extra == nil {
				state.Extra = make(map[string]string, 8)
			}
			state.Extra["completion_kind"] = clearMapCompletionKind
			state.Extra["completion_key"] = input.CompletionKey
			state.Extra["completion_dungeon_id"] = strconv.FormatInt(input.DungeonID, 10)
			state.Extra["completion_map_id"] = strconv.FormatInt(input.MapID, 10)
			if completion.Completed {
				state.Extra["reward_state"] = clearMapRewardPending
			} else {
				delete(state.Extra, "reward_state")
			}
			state.Extra["pvf_path"] = definition.Path
			state.Extra["quest_type"] = definition.Type
			if completion.Completed && !c.retainTerminalLinkedObjective(plan.Record, definition) {
				autoCompleteNoRewardSubQuestState(&state, definition, input.CompletedAt)
			}
			states[questID] = state
			plan.Completions = append(plan.Completions, completion)
			switch field {
			case dnfrepo.QuestFieldStates:
				statesChanged = true
			case dnfrepo.QuestFieldProgress:
				progressChanged = true
			}
		}
	}
	visit(plan.Record.States, dnfrepo.QuestFieldStates)
	visit(plan.Record.Progress, dnfrepo.QuestFieldProgress)
	for _, field := range syncActiveQuestClearParentProgress(c, &plan.Record, input.CompletedAt) {
		switch field {
		case dnfrepo.QuestFieldStates:
			statesChanged = true
		case dnfrepo.QuestFieldProgress:
			progressChanged = true
		}
	}
	if statesChanged || progressChanged {
		plan.Record.UpdatedAt = input.CompletedAt
	}
	if statesChanged {
		plan.ChangedFields = append(plan.ChangedFields, dnfrepo.QuestFieldStates)
	}
	if progressChanged {
		plan.ChangedFields = append(plan.ChangedFields, dnfrepo.QuestFieldProgress)
	}
	return plan, nil
}

// retainTerminalLinkedObjective keeps the final no-reward child of an active
// [quest clear] parent visible at trigger zero. The current client must first
// observe that state, then emit its native parent op33/op34 finish sequence;
// archiving the child here collapses the parent too early and loses the finish
// dialogue.
func (c *Catalog) retainTerminalLinkedObjective(record dnfrepo.QuestRecord, definition Definition) bool {
	if c == nil || definition.MainQuestID <= 0 || !shouldAutoCompleteNoRewardSubQuest(definition) {
		return false
	}
	parent, known := c.Find(definition.MainQuestID)
	if !known {
		return false
	}
	parentType := normalizeQuestTag(parent.Type)
	if parentType != "quest clear" && parentType != "clear quest" {
		return false
	}
	parentState, _, active := mutableQuestState(&record, parent.ID)
	if !active || !isActiveQuestStatus(parentState.Status) {
		return false
	}
	hasCurrentChild := false
	for _, childID := range parent.IntData {
		if childID <= 0 {
			continue
		}
		if childID == definition.ID {
			hasCurrentChild = true
			continue
		}
		state, _, stateKnown := mutableQuestState(&record, childID)
		if !stateKnown {
			return false
		}
		if isCompletedQuestStatus(state.Status) {
			continue
		}
		child, childKnown := c.Find(childID)
		if !childKnown || child.MainQuestID != parent.ID || !shouldAutoCompleteNoRewardSubQuest(child) {
			return false
		}
		if !isActiveQuestStatus(state.Status) || state.ProgressValue != 0 {
			return false
		}
	}
	return hasCurrentChild
}

func clearCompletionMatchChannel(definition Definition, input ClearMapCompletionInput) (int, bool) {
	typeTag := normalizeQuestTag(definition.Type)
	if typeTag == "clear map" {
		return 0, clearMapTargetMatches(definition.IntData, input.DungeonID, input.MapID)
	}
	// Current PVF side quests such as quest 1680
	// encode a set of alternative dungeons as repeated
	// (dungeon_id, minimum_difficulty) pairs.  The title's "1/4" is the
	// chapter number, not four packed trigger channels: clearing any one
	// authored dungeon completes the quest's single initial trigger.
	if typeTag == "clear a dungeon" && definition.SubType == 6 {
		for offset := 0; offset+1 < len(definition.IntData); offset += 2 {
			selector := definition.IntData[offset]
			minDifficulty := definition.IntData[offset+1]
			if selector != input.DungeonID {
				continue
			}
			if minDifficulty >= 0 && input.Difficulty < minDifficulty {
				continue
			}
			return 0, true
		}
		return 0, false
	}
	if typeTag != "condition under clear" {
		return 0, false
	}
	values := definition.IntData
	if definition.SubType == 22 && !input.WeddingCouple {
		return 0, false
	}
	switch definition.SubType {
	case 6, 22:
		stride := 3
		if len(values) == 2 {
			stride = 2
		}
		for channel, offset := 0, 0; offset < len(values) && channel < 3; channel, offset = channel+1, offset+stride {
			selector := values[offset]
			minDifficulty := int64(-1)
			if offset+1 < len(values) {
				minDifficulty = values[offset+1]
			}
			if selector >= 0 && selector != input.DungeonID {
				continue
			}
			if selector < 0 && selector != -1 {
				continue
			}
			if minDifficulty >= 0 && input.Difficulty < minDifficulty {
				continue
			}
			return channel, true
		}
	case 11:
		if len(values) >= 2 &&
			(values[0] == -1 || values[0] == input.DungeonID) &&
			(values[1] < 0 || input.Difficulty >= values[1]) &&
			input.AllRoomsCleared {
			return 0, true
		}
	case 23:
		if !input.HellPartyComplete {
			return 0, false
		}
		if len(values) > 0 && values[0] == -2 {
			for channel, offset := 0, 0; offset+3 < len(values) && channel < 3; channel, offset = channel+1, offset+4 {
				if values[offset] != -2 || values[offset+1] != input.WorldMapID {
					continue
				}
				if values[offset+2] >= 0 && input.Difficulty < values[offset+2] {
					continue
				}
				return channel, true
			}
			return 0, false
		}
		for channel, offset := 0, 0; offset < len(values) && channel < 3; channel, offset = channel+1, offset+3 {
			selector := values[offset]
			minDifficulty := int64(-1)
			if offset+1 < len(values) {
				minDifficulty = values[offset+1]
			}
			if selector >= 0 && selector != input.DungeonID {
				continue
			}
			if selector < 0 && selector != -1 {
				continue
			}
			if minDifficulty >= 0 && input.Difficulty < minDifficulty {
				continue
			}
			return channel, true
		}
	}
	return 0, false
}

func decrementClearTriggerChannel(trigger uint32, channel int) uint32 {
	if channel < 0 || channel > 2 {
		return trigger
	}
	shift := uint(channel * 9)
	current := (trigger >> shift) & 0x1ff
	if current == 0 {
		return trigger
	}
	return (trigger &^ (0x1ff << shift)) | ((current - 1) << shift)
}

func clearMapTargetMatches(values []int64, dungeonID, mapID int64) bool {
	for _, target := range values {
		if target <= 0 {
			continue
		}
		if dungeonID > 0 && target == dungeonID {
			return true
		}
		if mapID > 0 && target == mapID {
			return true
		}
	}
	return false
}

func (p ClearMapCompletionPlan) ValidateCharacter(characterID string) error {
	if strings.TrimSpace(p.Record.CharacterID) != strings.TrimSpace(characterID) {
		return fmt.Errorf("dnf clear-map quest owner mismatch: record=%q selected=%q", p.Record.CharacterID, characterID)
	}
	return nil
}
