package dnfbridge

import (
	"context"
	"encoding/binary"
	"math"
	"sort"
	"strconv"

	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfquest "longheng.io/server/internal/modules/dnf/quest"
)

const (
	currentTownQuestMeetingRangeX int64 = 120
	currentTownQuestMeetingRangeY int64 = 80
)

type currentTownNPCQuestTrigger struct {
	npcID   int64
	trigger dnfquest.MeetingNPCTrigger
}

// reconcileCurrentTownNPCQuestMeetings closes the gap left by current clients
// that display a PVF meet-NPC guide but do not emit op33 after reaching the
// target. Both the NPC identity/position and the quest target come from the
// active runtime PVF. The existing op33 owner remains the only persistence and
// wire-ACK path.
func (s *Service) reconcileCurrentTownNPCQuestMeetings(
	session *gameSession,
	snapshot currentTownPositionSnapshot,
) error {
	if s == nil || session == nil || session.selectedCharacterID == 0 ||
		!snapshot.PositionValid || snapshot.CharacterID != session.selectedCharacterID {
		return nil
	}
	nearby := s.currentTownNearbyNPCIDs(snapshot)
	entered := updateCurrentTownNearbyNPCs(session, snapshot, nearby)
	if len(entered) == 0 {
		return nil
	}

	repositories, ok := s.repositoryGroup()
	if !ok || repositories.Quest == nil {
		s.logGameEvent(session, "game-town-npc-quest-meeting-deferred",
			"char_id", session.selectedCharacterID,
			"town_id", snapshot.TownID,
			"area_id", snapshot.AreaID,
			"npc_ids", entered,
			"reason", "quest_repository_unavailable")
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	catalog, err := s.loadQuestCatalog(ctx)
	if err != nil {
		s.logGameEvent(session, "game-town-npc-quest-meeting-deferred",
			"char_id", session.selectedCharacterID,
			"town_id", snapshot.TownID,
			"area_id", snapshot.AreaID,
			"npc_ids", entered,
			"reason", "quest_catalog_unavailable",
			"error", err)
		return nil
	}
	characterID := strconv.FormatUint(uint64(session.selectedCharacterID), 10)
	record, found, err := repositories.Quest.Load(ctx, characterID)
	if err != nil {
		s.logGameEvent(session, "game-town-npc-quest-meeting-deferred",
			"char_id", session.selectedCharacterID,
			"town_id", snapshot.TownID,
			"area_id", snapshot.AreaID,
			"npc_ids", entered,
			"reason", "quest_record_load_failed",
			"error", err)
		return nil
	}
	if !found {
		return nil
	}

	planned := make([]currentTownNPCQuestTrigger, 0, len(entered))
	seen := make(map[[2]int64]struct{})
	for _, npcID := range entered {
		for _, trigger := range catalog.ActiveMeetingNPCTriggers(record, npcID) {
			if trigger.QuestID <= 0 || trigger.QuestID > math.MaxUint16 {
				continue
			}
			key := [2]int64{trigger.QuestID, int64(trigger.TriggerType)}
			if _, exists := seen[key]; exists {
				continue
			}
			seen[key] = struct{}{}
			planned = append(planned, currentTownNPCQuestTrigger{npcID: npcID, trigger: trigger})
		}
	}
	for _, step := range planned {
		body := make([]byte, currentSetQuestTriggerRequestBodySize)
		binary.LittleEndian.PutUint16(body[0:2], uint16(dnfenum.CmdPacketSetQuestTrigger))
		binary.LittleEndian.PutUint16(body[2:4], uint16(step.trigger.QuestID))
		body[4] = step.trigger.TriggerType
		s.logGameEvent(session, "game-town-npc-quest-meeting-detected",
			"char_id", session.selectedCharacterID,
			"town_id", snapshot.TownID,
			"area_id", snapshot.AreaID,
			"position_x", snapshot.PositionX,
			"position_y", snapshot.PositionY,
			"npc_id", step.npcID,
			"quest_id", step.trigger.QuestID,
			"trigger_type", step.trigger.TriggerType,
			"source", "current_exe_op35_pvf_town_npc_proximity")
		if err := s.handleCurrentSetQuestTrigger(session, body); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) currentTownNearbyNPCIDs(snapshot currentTownPositionSnapshot) []int64 {
	if s == nil || !snapshot.PositionValid {
		return nil
	}
	area, ok := s.townArea(int64(snapshot.TownID), int64(snapshot.AreaID))
	if !ok {
		return nil
	}
	nearbySet := make(map[int64]struct{})
	x, y := int64(snapshot.PositionX), int64(snapshot.PositionY)
	for _, spawn := range area.NPCs {
		if spawn.NPCID <= 0 || absInt64(spawn.X-x) > currentTownQuestMeetingRangeX ||
			absInt64(spawn.Y-y) > currentTownQuestMeetingRangeY {
			continue
		}
		nearbySet[spawn.NPCID] = struct{}{}
	}
	nearby := make([]int64, 0, len(nearbySet))
	for npcID := range nearbySet {
		nearby = append(nearby, npcID)
	}
	sort.Slice(nearby, func(i, j int) bool { return nearby[i] < nearby[j] })
	return nearby
}

func updateCurrentTownNearbyNPCs(
	session *gameSession,
	snapshot currentTownPositionSnapshot,
	nearby []int64,
) []int64 {
	if session == nil {
		return nil
	}
	session.townMu.Lock()
	defer session.townMu.Unlock()
	live := session.townPositionSnapshot
	if session.townSceneReadyCharacterID != snapshot.CharacterID ||
		live.CharacterID != snapshot.CharacterID || live.TownID != snapshot.TownID || live.AreaID != snapshot.AreaID {
		return nil
	}
	current := make(map[int64]struct{}, len(nearby))
	entered := make([]int64, 0, len(nearby))
	for _, npcID := range nearby {
		current[npcID] = struct{}{}
		if _, existed := session.townMeetingNearbyNPCs[npcID]; !existed {
			entered = append(entered, npcID)
		}
	}
	session.townMeetingNearbyNPCs = current
	return entered
}

func absInt64(value int64) int64 {
	if value < 0 {
		return -value
	}
	return value
}
