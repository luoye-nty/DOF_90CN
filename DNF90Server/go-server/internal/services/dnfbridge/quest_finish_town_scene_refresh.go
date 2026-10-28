package dnfbridge

import (
	"sort"
	"strconv"

	dnfquest "longheng.io/server/internal/modules/dnf/quest"
)

// The current NoPack channel selector checks quest 12829 (0x321d) before it
// publishes channel types 23 and 25, reading the completed-quest state owned by
// the current scene. A late op356 updates the task book, but the already-built
// channel list keeps the entry absent until the scene is reconstructed.
const currentAntonRaidChannelUnlockQuestID = 12829

// sendCurrentQuestGatedTownSceneRefresh rebuilds only a currently loaded town
// scene whose PVF map or client feature is gated by the quest that just
// completed. The current client consumes op356 while constructing op24's
// destination scene; the op356 re-seed both op34 tails already send updates
// task and NPC state after the fact, but it cannot restore an omitted
// [quest condition] passage, map animation or cached feature entry - only a
// same-area op24 does.
//
// This is deliberately narrower than handleTownSetUserArea: it does not write
// durable coordinates, close stores, change online-area membership or touch a
// dungeon runtime. The player stays in the same town/area at the latest
// session-owned op35 position.
func (s *Service) sendCurrentQuestGatedTownSceneRefresh(
	session *gameSession,
	result dnfquest.FinishCommitResult,
	source string,
) error {
	if s == nil || session == nil || session.selectedCharacterID == 0 ||
		!result.AtomicCommitted || result.QuestID <= 0 ||
		result.CharacterID != strconv.FormatUint(uint64(session.selectedCharacterID), 10) {
		return nil
	}
	return s.sendCurrentQuestGatedTownSceneRefreshForQuestIDs(session, result, []int64{result.QuestID}, source)
}

// sendCurrentQuestGatedTownSceneRefreshForQuestIDs is the bulk-completion
// counterpart of the single-quest refresh above: it rebuilds the same-area town
// scene when ANY quest id in gateQuestIDs gates the current map or a client
// feature. The op1429 bulk auto-complete closes every eligible low-level epic at
// once; a [quest condition] gate may belong to any one of those rows, so the
// gate check must scan the whole changed set, not only the highest completed
// quest. The same in-place op22 -> op23 -> op356 -> op24 leg is used, so the
// player never moves.
func (s *Service) sendCurrentQuestGatedTownSceneRefreshForQuestIDs(
	session *gameSession,
	result dnfquest.FinishCommitResult,
	gateQuestIDs []int64,
	source string,
) error {
	if s == nil || session == nil || session.selectedCharacterID == 0 ||
		!result.AtomicCommitted ||
		result.CharacterID != strconv.FormatUint(uint64(session.selectedCharacterID), 10) {
		return nil
	}

	session.dungeon.mu.Lock()
	hasDungeonRuntime := session.dungeon.runtime != nil
	session.dungeon.mu.Unlock()
	if hasDungeonRuntime {
		return nil
	}

	characterID := session.selectedCharacterID
	session.townMu.Lock()
	defer session.townMu.Unlock()
	snapshot := session.townPositionSnapshot
	if session.townSceneReadyCharacterID != characterID ||
		snapshot.CharacterID != characterID || !snapshot.PositionValid {
		return nil
	}
	townID, areaID, err := currentSceneTransitionLocation(result.PostCommitCharacter, true)
	if err != nil || snapshot.TownID != townID || snapshot.AreaID != areaID {
		return nil
	}
	area, found := s.townArea(int64(townID), int64(areaID))
	if !found {
		return nil
	}
	mapQuestGateCompleted := anyTownMapQuestCondition(area.QuestConditions, gateQuestIDs)
	clientFeatureGateCompleted := anyCurrentClientFeatureGateQuest(gateQuestIDs)
	if !mapQuestGateCompleted && !clientFeatureGateCompleted {
		s.logGameEvent(session, "game-upper-finish-quest-town-scene-refresh-skipped",
			"source", source,
			"quest_id", result.QuestID,
			"gate_quest_ids", gateQuestIDs,
			"town_id", townID,
			"area_id", areaID,
			"map_path", area.MapPath,
			"reason", "completed_quest_set_gates_neither_map_condition_nor_client_feature")
		return nil
	}
	areaState, ok := result.PostCommitCharacter.Stats["area_state"]
	if !ok || areaState < 0 || areaState > int64(^byte(0)) {
		s.logGameEvent(session, "game-upper-finish-quest-town-scene-refresh-skipped",
			"quest_id", result.QuestID,
			"town_id", townID,
			"area_id", areaID,
			"map_path", area.MapPath,
			"reason", "committed_character_area_state_missing_or_invalid")
		return nil
	}

	selfRow := currentSceneTransitionRow{
		ObjectOrResourceKey: currentSceneActorObjectKey(characterID),
		Value1:              snapshot.PositionX,
		Value2:              snapshot.PositionY,
		Value3:              snapshot.MovementCode,
		Value4:              byte(areaState),
	}
	transitionRows := make([]currentSceneTransitionRow, 0, 1)
	if s.onlinePlayers != nil {
		areaPlayers := s.onlinePlayers.GetAreaPlayers(
			townPresenceChannelID(session), townID, areaID)
		sort.Slice(areaPlayers, func(i, j int) bool {
			return areaPlayers[i].CharacterID < areaPlayers[j].CharacterID
		})
		for i := range areaPlayers {
			if areaPlayers[i].CharacterID == 0 || areaPlayers[i].CharacterID == characterID {
				continue
			}
			transitionRows = append(transitionRows, currentSceneTransitionRow{
				ObjectOrResourceKey: currentSceneActorObjectKey(areaPlayers[i].CharacterID),
				Value1:              areaPlayers[i].PositionX,
				Value2:              areaPlayers[i].PositionY,
				Value3:              areaPlayers[i].Direction,
				Value4:              areaPlayers[i].AreaState,
			})
		}
	}
	// The current client binds the final op24 row as the selected actor.
	transitionRows = append(transitionRows, selfRow)
	transitionBody, err := buildCurrentSceneTransitionBody(townID, areaID, transitionRows)
	if err != nil {
		return err
	}

	s.logGameEvent(session, "game-upper-finish-quest-town-scene-refresh-send",
		"source", source,
		"quest_id", result.QuestID,
		"gate_quest_ids", gateQuestIDs,
		"town_id", townID,
		"area_id", areaID,
		"map_path", area.MapPath,
		"map_quest_conditions", area.QuestConditions,
		"map_quest_gate_completed", mapQuestGateCompleted,
		"client_feature_gate_completed", clientFeatureGateCompleted,
		"position_x", snapshot.PositionX,
		"position_y", snapshot.PositionY,
		"row_count", len(transitionRows),
		"sequence", "op22_then_op23_then_committed_op356_then_same_area_op24",
		"reason", "completed_pvf_map_or_client_feature_gate_requires_scene_reconstruction")
	if err := s.sendCurrentTownLocationNotifications(
		session,
		characterID,
		townID,
		areaID,
		selfRow,
		source+"_same_area_scene_rebuild",
	); err != nil {
		return err
	}
	if err := s.sendCurrentClearQuestListFromCommittedQuest(
		session,
		result.PostCommitQuest,
		source+"_after_op23_before_op24_seed_completed_quest_scene_state",
	); err != nil {
		return err
	}
	return s.sendGameUpperRawClass(session, currentSceneTransitionMsgID, transitionBody, 0)
}

func anyCurrentClientFeatureGateQuest(gateQuestIDs []int64) bool {
	for _, questID := range gateQuestIDs {
		if questID == currentAntonRaidChannelUnlockQuestID {
			return true
		}
	}
	return false
}

func anyTownMapQuestCondition(questConditions, gateQuestIDs []int64) bool {
	for _, questID := range gateQuestIDs {
		if containsTownMapQuestCondition(questConditions, questID) {
			return true
		}
	}
	return false
}

func containsTownMapQuestCondition(questConditions []int64, questID int64) bool {
	for _, candidate := range questConditions {
		if candidate == questID {
			return true
		}
	}
	return false
}
