package quest

import (
	"strings"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

// ImageCommunicationTarget identifies the remote NPC object that the current
// client must materialize after the player uses Mia Richter's communication
// equipment. The NPC ID comes from the active PVF meet-NPC quest's [int data].
type ImageCommunicationTarget struct {
	QuestID int64
	NPCID   uint32
}

// ActiveImageCommunicationTarget resolves one unambiguous active remote-call
// quest. Canonical States override legacy Progress rows, matching the other
// quest projections; an ambiguous record fails closed instead of showing the
// wrong remote NPC.
func (c *Catalog) ActiveImageCommunicationTarget(record dnfrepo.QuestRecord) (ImageCommunicationTarget, bool) {
	if c == nil {
		return ImageCommunicationTarget{}, false
	}
	target := ImageCommunicationTarget{}
	foundTarget := false
	consider := func(questID int64, state dnfrepo.QuestState) bool {
		if !isActiveQuestStatus(state.Status) {
			return true
		}
		definition, found := c.Find(questID)
		if !found || !isImageCommunicationDefinition(definition) {
			return true
		}
		npcID := definition.IntData[0]
		if npcID <= 0 || npcID > int64(^uint32(0)) {
			return true
		}
		candidate := ImageCommunicationTarget{QuestID: questID, NPCID: uint32(npcID)}
		if foundTarget && candidate != target {
			return false
		}
		target = candidate
		foundTarget = true
		return true
	}

	for questID, state := range record.States {
		if !consider(questID, state) {
			return ImageCommunicationTarget{}, false
		}
	}
	for questID, state := range record.Progress {
		if _, canonical := record.States[questID]; canonical {
			continue
		}
		if !consider(questID, state) {
			return ImageCommunicationTarget{}, false
		}
	}
	return target, foundTarget
}

func isImageCommunicationDefinition(definition Definition) bool {
	if normalizeQuestTag(definition.Type) != "meet npc" || len(definition.IntData) == 0 {
		return false
	}
	message := definition.DependMessage + "\n" + definition.ConditionMessage + "\n" + definition.SolveMessage
	return strings.Contains(message, "远程通话装置") || strings.Contains(message, "视频通信装置")
}
