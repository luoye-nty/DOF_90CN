package quest

import (
	"time"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

// LinkedQuestChainResetPlan is the administrative rollback projection for a
// quest-clear parent and the managed subtasks it owns. A linked child cannot
// be safely made incomplete on its own: the client derives its NPC/dialogue
// ownership from the parent and from every sibling row in the same generation.
type LinkedQuestChainResetPlan struct {
	Record              dnfrepo.QuestRecord
	ParentQuestID       int64
	ReactivatedQuestIDs []int64
	ChangedFields       []dnfrepo.QuestField
}

// PlanLinkedQuestChainReset removes every persisted generation of a linked
// quest-clear family and activates a fresh parent/child generation from PVF.
// Ordinary standalone quests deliberately remain outside this operation.
func (c *Catalog) PlanLinkedQuestChainReset(
	character CharacterEligibility,
	record dnfrepo.QuestRecord,
	requestedQuestID int64,
	resetAt time.Time,
) (LinkedQuestChainResetPlan, error) {
	if c == nil {
		return LinkedQuestChainResetPlan{}, ErrCatalogEmpty
	}
	requested, ok := c.Find(requestedQuestID)
	if !ok {
		return LinkedQuestChainResetPlan{}, ErrQuestDefinitionMissing
	}
	if requested.MainQuestID <= 0 {
		return LinkedQuestChainResetPlan{}, ErrQuestNotAcceptable
	}
	parent, ok := c.Find(requested.MainQuestID)
	if !ok || !questClearDefinition(parent) {
		return LinkedQuestChainResetPlan{}, ErrQuestNotAcceptable
	}
	familyIDs := c.managedLinkedQuestChainIDs(parent)
	requestedManaged := false
	for _, questID := range familyIDs[1:] {
		if questID == requestedQuestID {
			requestedManaged = true
			break
		}
	}
	if !requestedManaged {
		return LinkedQuestChainResetPlan{}, ErrQuestNotAcceptable
	}

	reset := dnfrepo.CloneQuest(record)
	for _, questID := range familyIDs {
		delete(reset.States, questID)
		delete(reset.Progress, questID)
	}
	accept, err := c.PlanAccept(character, reset, parent.ID)
	if err != nil {
		return LinkedQuestChainResetPlan{}, err
	}
	if resetAt.IsZero() {
		resetAt = time.Now().UTC()
	}
	applyAcceptPlan(&reset, accept, parent, record.CharacterID, resetAt)
	reset.UpdatedAt = resetAt
	completed := make(map[int64]struct{})
	for _, linked := range accept.LinkedSubQuests {
		if c.linkedChildWaitsForEarlierSibling(parent, c.byID[linked.QuestID], completed) {
			delete(reset.States, linked.QuestID)
		}
	}

	reactivated := []int64{accept.QuestID}
	for _, linked := range accept.LinkedSubQuests {
		if _, active := reset.States[linked.QuestID]; active {
			reactivated = append(reactivated, linked.QuestID)
		}
	}
	return LinkedQuestChainResetPlan{
		Record:              reset,
		ParentQuestID:       parent.ID,
		ReactivatedQuestIDs: reactivated,
		ChangedFields:       []dnfrepo.QuestField{dnfrepo.QuestFieldStates, dnfrepo.QuestFieldProgress},
	}, nil
}

func (c *Catalog) managedLinkedQuestChainIDs(parent Definition) []int64 {
	ids := []int64{parent.ID}
	if c == nil || !questClearDefinition(parent) {
		return ids
	}
	seen := make(map[int64]struct{}, len(parent.IntData))
	for _, childID := range parent.IntData {
		if childID <= 0 {
			continue
		}
		if _, duplicate := seen[childID]; duplicate {
			continue
		}
		seen[childID] = struct{}{}
		child, ok := c.Find(childID)
		if !ok || child.MainQuestID != parent.ID || child.HasDependGiveItem || !shouldAutoCompleteNoRewardSubQuest(child) {
			continue
		}
		ids = append(ids, childID)
	}
	return ids
}

func questClearDefinition(definition Definition) bool {
	tag := normalizeQuestTag(definition.Type)
	return tag == "quest clear" || tag == "clear quest"
}

func (c *Catalog) linkedChildWaitsForEarlierSibling(parent, child Definition, completed map[int64]struct{}) bool {
	if c == nil || child.MainQuestID != parent.ID || !orderedLinkedStoryStep(child) {
		return false
	}
	for _, siblingID := range parent.IntData {
		if siblingID == child.ID {
			return false
		}
		if siblingID <= 0 {
			continue
		}
		sibling, ok := c.byID[siblingID]
		if !ok || sibling.MainQuestID != parent.ID || sibling.HasDependGiveItem || !shouldAutoCompleteNoRewardSubQuest(sibling) {
			continue
		}
		if _, done := completed[siblingID]; !done {
			return true
		}
	}
	return false
}

func orderedLinkedStoryStep(definition Definition) bool {
	switch normalizeQuestTag(definition.Type) {
	case "meet npc", "seek n meet npc", "reach the range", "look cinematic":
		return true
	default:
		return false
	}
}
