package quest

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

var (
	ErrCompletionTicketActionInvalid     = errors.New("quest completion ticket action is invalid")
	ErrCompletionTicketLevelInvalid      = errors.New("quest completion ticket level requirement is not met")
	ErrCompletionTicketNoApplicableQuest = errors.New("quest completion ticket has no applicable quest")
	ErrCompletionTicketSelectionRequired = errors.New("quest completion ticket requires an explicit quest selection")
)

// CompletionTicketSpec is the PVF-derived policy attached to one stackable.
// ActionValues are interpreted only by the corresponding action family.
type CompletionTicketSpec struct {
	ActionType               string
	ActionValues             []int64
	MinimumLevel             int64
	MaximumLevel             int64
	PreserveSideQuests       bool
	PreserveHellPartyUnlocks bool
}

// CompletionTicketPlan contains only no-reward quest completion changes and,
// for awakening coupons, the canonical grow_type stage transition.
type CompletionTicketPlan struct {
	Quest             dnfrepo.QuestRecord
	Character         dnfrepo.CharacterRecord
	QuestFields       []dnfrepo.QuestField
	CharacterFields   []dnfrepo.CharacterField
	CompletedQuestIDs []int64
	ProfessionChanged bool
}

// PlanCompletionTicket interprets every quest-ticket [action type] present in
// the current PVF. It never grants ordinary quest rewards: the ticket itself is
// the authorization to record the selected quests as completed.
func (c *Catalog) PlanCompletionTicket(
	record dnfrepo.QuestRecord,
	characterRecord dnfrepo.CharacterRecord,
	character CharacterEligibility,
	spec CompletionTicketSpec,
	completedAt time.Time,
) (CompletionTicketPlan, error) {
	if c == nil || completedAt.IsZero() || !definitionCharacterJobValid(character) {
		return CompletionTicketPlan{}, ErrCompletionTicketActionInvalid
	}
	if characterRecord.Level != character.Level || character.Level <= 0 ||
		(spec.MinimumLevel > 0 && int64(character.Level) < spec.MinimumLevel) ||
		(spec.MaximumLevel > 0 && int64(character.Level) > spec.MaximumLevel) {
		return CompletionTicketPlan{}, ErrCompletionTicketLevelInvalid
	}
	action := normalizeTicketAction(spec.ActionType)
	if action == "[quest clear coupon]" {
		// This family opens the native selection UI; op1429 owns the selected
		// quest mutation and therefore must retain the selector from that packet.
		return CompletionTicketPlan{}, ErrCompletionTicketSelectionRequired
	}

	plan := CompletionTicketPlan{
		Quest:     dnfrepo.CloneQuest(record),
		Character: dnfrepo.CloneCharacter(characterRecord),
	}
	completed, _ := questStateSets(plan.Quest)
	answers := questAnswerSelections(plan.Quest)
	candidates := make([]int64, 0, 64)
	selected := make(map[int64]struct{})
	projected := make(map[int64]struct{}, len(completed)+64)
	for questID := range completed {
		projected[questID] = struct{}{}
	}

	add := func(definition Definition, requirePrerequisites bool) bool {
		if definition.ID <= 0 || definition.LevelMin > character.Level ||
			!definitionMatchesCharacter(definition, character) {
			return false
		}
		// Level-up tickets advance main progression only. Side/external stories
		// and equipment-slot unlocks stay player-selected, while Hell Party
		// unlocks must reach the normal finish path so each reward can write its
		// own durable permission.
		if (spec.PreserveSideQuests && ticketOptionalExternalQuest(definition)) ||
			(spec.PreserveHellPartyUnlocks && c.HasHellPartyUnlock(definition.ID)) {
			return false
		}
		if _, done := projected[definition.ID]; done {
			return false
		}
		if _, exists := selected[definition.ID]; exists {
			return false
		}
		if requirePrerequisites && !ticketPrerequisitesMet(definition, projected, answers) {
			return false
		}
		selected[definition.ID] = struct{}{}
		projected[definition.ID] = struct{}{}
		candidates = append(candidates, definition.ID)
		return true
	}

	switch action {
	case "[any quest clear]":
		if len(spec.ActionValues) == 0 {
			return CompletionTicketPlan{}, ErrCompletionTicketActionInvalid
		}
		for _, questID := range spec.ActionValues {
			definition, found := c.Find(questID)
			if !found {
				continue
			}
			add(definition, false)
		}
	case "[gm quest clear]":
		c.collectTicketBulkCandidates(character, character.Level, func(definition Definition) bool {
			return normalizeQuestTag(definition.Grade) == "epic"
		}, add)
	case "[lv clear]":
		cutoff := character.Level
		if len(spec.ActionValues) > 0 && spec.ActionValues[0] > 0 && int64(cutoff) > spec.ActionValues[0] {
			cutoff = int(spec.ActionValues[0])
		}
		c.collectTicketBulkCandidates(character, cutoff, func(definition Definition) bool {
			grade := normalizeQuestTag(definition.Grade)
			return grade == "epic" || (grade == "achievement" && !ticketExcludedAchievement(definition))
		}, add)
	case "[quest clear]":
		cutoff := character.Level
		if spec.MaximumLevel > 0 && int64(cutoff) > spec.MaximumLevel {
			cutoff = int(spec.MaximumLevel)
		}
		c.collectTicketBulkCandidates(character, cutoff, func(definition Definition) bool {
			switch normalizeQuestTag(definition.Grade) {
			case "epic", "", "normal", "side":
				return true
			default:
				return false
			}
		}, add)
	case "[achievement quest clear]", "[side quest clear]":
		c.collectTicketBulkCandidates(character, character.Level, func(definition Definition) bool {
			grade := normalizeQuestTag(definition.Grade)
			return (grade == "achievement" || (action == "[side quest clear]" && grade == "side")) &&
				!ticketExcludedAchievement(definition)
		}, add)
	case "[first awaken clear]", "[second awaken clear]":
		targetStage := 1
		jobChangeQuest := 2
		if action == "[second awaken clear]" {
			targetStage = 2
			jobChangeQuest = 3
		}
		currentStage := (character.GrowType >> 4) & 0x0f
		if currentStage >= targetStage || currentStage+1 != targetStage {
			return CompletionTicketPlan{}, ErrCompletionTicketNoApplicableQuest
		}
		c.collectTicketBulkCandidates(character, character.Level, func(definition Definition) bool {
			return definition.JobChangeQuest == jobChangeQuest
		}, add)
		if len(candidates) > 0 {
			newGrowType := int64(targetStage<<4 | (character.GrowType & 0x0f))
			if plan.Character.Stats == nil {
				plan.Character.Stats = make(map[string]int64)
			}
			if plan.Character.Stats["grow_type"] != newGrowType {
				plan.Character.Stats["grow_type"] = newGrowType
				plan.Character.UpdatedAt = completedAt
				plan.CharacterFields = append(plan.CharacterFields, dnfrepo.CharacterFieldStats)
				plan.ProfessionChanged = true
			}
		}
	default:
		return CompletionTicketPlan{}, fmt.Errorf("%w: %q", ErrCompletionTicketActionInvalid, spec.ActionType)
	}

	// Parent [quest clear] definitions own objective children. Completing the
	// parent via a coupon must also retire already-active child rows so the task
	// panel does not retain a ghost objective after its parent is cleared.
	for index := 0; index < len(candidates); index++ {
		parent, found := c.Find(candidates[index])
		if !found || normalizeQuestTag(parent.Type) != "quest clear" {
			continue
		}
		for _, childID := range parent.IntData {
			child, childFound := c.Find(childID)
			if childFound && child.MainQuestID == parent.ID && definitionMatchesCharacter(child, character) {
				add(child, false)
			}
		}
	}

	if len(candidates) == 0 {
		return CompletionTicketPlan{}, ErrCompletionTicketNoApplicableQuest
	}
	for _, questID := range candidates {
		definition, found := c.Find(questID)
		if !found {
			continue
		}
		state, field, known := mutableQuestState(&plan.Quest, questID)
		if !known {
			state = dnfrepo.QuestState{}
			field = dnfrepo.QuestFieldStates
			if plan.Quest.States == nil {
				plan.Quest.States = make(map[int64]dnfrepo.QuestState)
			}
		}
		state.Status = "completed"
		state.ProgressValue = 0
		state.RewardSelectIndex = 0
		state.UpdatedAt = completedAt
		if state.Extra == nil {
			state.Extra = make(map[string]string, 8)
		}
		state.Extra["pvf_path"] = definition.Path
		state.Extra["quest_type"] = definition.Type
		state.Extra["reward_state"] = finishRewardGranted
		state.Extra["auto_completed"] = "true"
		state.Extra["auto_complete_reason"] = "pvf_quest_completion_ticket_no_reward"
		state.Extra["auto_complete_action"] = action
		if field == dnfrepo.QuestFieldProgress {
			plan.Quest.Progress[questID] = state
		} else {
			plan.Quest.States[questID] = state
		}
		plan.QuestFields = mergeQuestChangedField(plan.QuestFields, field)
		plan.CompletedQuestIDs = append(plan.CompletedQuestIDs, questID)
	}
	if len(plan.QuestFields) == 0 {
		return CompletionTicketPlan{}, ErrCompletionTicketNoApplicableQuest
	}
	plan.Quest.UpdatedAt = completedAt
	sort.Slice(plan.CompletedQuestIDs, func(i, j int) bool { return plan.CompletedQuestIDs[i] < plan.CompletedQuestIDs[j] })
	return plan, nil
}

func (c *Catalog) collectTicketBulkCandidates(
	character CharacterEligibility,
	cutoff int,
	match func(Definition) bool,
	add func(Definition, bool) bool,
) {
	if c == nil || cutoff <= 0 || match == nil || add == nil {
		return
	}
	for progressed := true; progressed; {
		progressed = false
		for _, definition := range c.ordered {
			if definition.LevelMin <= 0 || definition.LevelMin > cutoff || definition.ExposedByNPC == 0 ||
				definition.IsEvent || ticketExcludedGrade(normalizeQuestTag(definition.Grade)) || !match(definition) ||
				!definitionMatchesCharacter(definition, character) {
				continue
			}
			if add(definition, true) {
				progressed = true
			}
		}
	}
}

func ticketPrerequisitesMet(definition Definition, completed map[int64]struct{}, answers map[int64]int64) bool {
	if definition.MalformedPreAnswers {
		return false
	}
	if definition.MainQuestID > 0 {
		if _, found := completed[definition.MainQuestID]; !found {
			return false
		}
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
	for _, requirement := range definition.PreRequiredAnswers {
		if answer, found := answers[requirement.QuestID]; !found || answer != requirement.Answer {
			return false
		}
	}
	for _, collision := range definition.CollisionQuests {
		if collision > 0 {
			if _, found := completed[collision]; found {
				return false
			}
		}
	}
	return true
}

func ticketExcludedAchievement(definition Definition) bool {
	if definition.JobChangeQuest != 0 {
		return true
	}
	switch normalizeQuestTag(definition.RewardType) {
	case "grow type", "awakening type", "hell challenge":
		return true
	}
	// These IDs are completed only by their dedicated support-equipment,
	// magic-stone, earring, or Hell Party [any quest clear] tickets.
	switch definition.ID {
	case 649, 650, 674, 675, 2002, 2635, 2636, 2900, 4391, 4539:
		return true
	default:
		return false
	}
}

func ticketOptionalExternalQuest(definition Definition) bool {
	switch normalizeQuestTag(definition.Grade) {
	case "side", "common unique":
		return true
	}
	// Support equipment, magic stone, and earring quests are not consistently
	// graded across Script.pvf revisions. Their reward contract is authoritative
	// and must never be bypassed by a level-up ticket.
	return normalizeQuestTag(definition.RewardType) == "slot expansion"
}

func normalizeTicketAction(value string) string {
	return strings.ToLower(strings.TrimSpace(strings.ReplaceAll(value, "`", "")))
}
