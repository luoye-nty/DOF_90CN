package quest

import dnfrepo "longheng.io/server/internal/modules/dnf/repository"

// MainContinuityLevelFloor returns the minimum level required by the next
// currently reachable PVF epic quest after the supplied quests are completed.
// It applies the same job, grow-type, prerequisite, answer, collision, and
// visibility predicates as op21 while evaluating the successor at its own
// minimum level. Different eligible target levels are treated as an ambiguous
// branch and fail closed.
func (c *Catalog) MainContinuityLevelFloor(
	character CharacterEligibility,
	record dnfrepo.QuestRecord,
	completedQuestIDs ...int64,
) (int, bool) {
	if c == nil || len(completedQuestIDs) == 0 {
		return 0, false
	}
	primary, found := c.Find(completedQuestIDs[0])
	if !found || normalizeQuestTag(primary.Grade) != "epic" ||
		!definitionMatchesCharacter(primary, character) {
		return 0, false
	}
	completed, active := questStateSets(record)
	for _, questID := range completedQuestIDs {
		if questID <= 0 {
			return 0, false
		}
		completed[questID] = struct{}{}
		delete(active, questID)
	}
	answers := questAnswerSelections(record)
	targetLevel := 0
	seen := make(map[int64]struct{})
	for _, questID := range completedQuestIDs {
		for _, successor := range c.Successors(questID) {
			if _, duplicate := seen[successor.ID]; duplicate {
				continue
			}
			seen[successor.ID] = struct{}{}
			if normalizeQuestTag(successor.Grade) != "epic" ||
				successor.LevelMin <= character.Level {
				continue
			}
			atGate := character
			atGate.Level = successor.LevelMin
			if !c.definitionAcceptable(successor, atGate, completed, active, answers, true) {
				continue
			}
			if targetLevel != 0 && targetLevel != successor.LevelMin {
				return 0, false
			}
			targetLevel = successor.LevelMin
		}
	}
	return targetLevel, targetLevel > 0
}
