package dnfbridge

import (
	"context"
	"fmt"
	"strconv"

	"google.golang.org/protobuf/encoding/protowire"
	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
	dnfquest "longheng.io/server/internal/modules/dnf/quest"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

const (
	currentAcceptableQuestListMsgID = uint16(0x15)
	currentAcceptableQuestListKind  = "current_acceptable_quest_list"
	currentAcceptableQuestListEnum  = uint64(21)
)

func (s *Service) preloadQuestCatalog(ctx context.Context) error {
	catalog, err := s.loadQuestCatalog(ctx)
	if err != nil {
		return fmt.Errorf("preload quest catalog: %w", err)
	}
	snapshot := catalog.Snapshot()
	s.logPacketEvent("dnf-quest-catalog-loaded",
		"definitions", snapshot.Definitions,
		"scope_hints", snapshot.ScopeHints,
		"epic", snapshot.Epic,
		"normal", snapshot.Normal,
		"list", dnfquest.DefaultList)
	return nil
}

func (s *Service) loadQuestCatalog(ctx context.Context) (*dnfquest.Catalog, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.questCatalogMu.Lock()
	defer s.questCatalogMu.Unlock()
	if s.questCatalog != nil {
		return s.questCatalog, nil
	}
	if s.questCatalogLoadErr != nil {
		return nil, s.questCatalogLoadErr
	}

	s.initialEquipmentMu.Lock()
	archive, err := s.initialEquipmentArchiveLocked()
	s.initialEquipmentMu.Unlock()
	if err != nil {
		s.questCatalogLoadErr = err
		return nil, err
	}
	index, err := dnfpvf.Build(ctx, archive, dnfpvf.BuildOptions{
		Lists:    []string{dnfquest.DefaultList},
		Prefixes: []string{"n_quest/"},
	})
	if err == nil {
		s.questCatalog, err = dnfquest.Load(ctx, index)
	}
	if err != nil {
		s.questCatalogLoadErr = err
		return nil, err
	}
	return s.questCatalog, nil
}

func buildCurrentAcceptableQuestListBody(level int, questIDs []int32) []byte {
	protobuf := make([]byte, 0, 8+len(questIDs)*3)
	protobuf = appendProtoVarint(protobuf, 1, currentAcceptableQuestListEnum)
	protobuf = appendProtoVarint(protobuf, 2, uint64(uint32(level)))
	if len(questIDs) > 0 {
		packed := make([]byte, 0, len(questIDs)*3)
		for _, questID := range questIDs {
			if questID <= 0 {
				continue
			}
			packed = protowire.AppendVarint(packed, uint64(uint32(questID)))
		}
		if len(packed) > 0 {
			protobuf = appendProtoBytes(protobuf, 4, packed)
		}
	}
	var writer packetWriter
	writer.writeUint32(uint32(len(protobuf)))
	writer.writeBytes(protobuf)
	return writer.bytes()
}

func (s *Service) buildCurrentAcceptableQuestListBodyForSession(ctx context.Context, session *gameSession) ([]byte, bool) {
	if s == nil || session == nil || session.selectedCharacterID == 0 {
		return nil, false
	}
	catalog, err := s.loadQuestCatalog(ctx)
	if err != nil {
		s.logGameEvent(session, "game-upper-current-acceptable-quest-list-skipped",
			"char_id", session.selectedCharacterID,
			"reason", "quest_catalog_unavailable",
			"error", err)
		return nil, false
	}
	repos, ok := s.repositoryGroup()
	if !ok || repos.Character == nil || repos.Quest == nil {
		s.logGameEvent(session, "game-upper-current-acceptable-quest-list-skipped",
			"char_id", session.selectedCharacterID,
			"reason", "character_or_quest_repository_unavailable")
		return nil, false
	}
	_, _, character, found := s.selectedCharacterForEnter(ctx, session)
	if !found {
		s.logGameEvent(session, "game-upper-current-acceptable-quest-list-skipped",
			"char_id", session.selectedCharacterID,
			"reason", "selected_character_not_found")
		return nil, false
	}
	job, validJob := characterJobByte(character)
	if !validJob || character.Level <= 0 {
		s.logGameEvent(session, "game-upper-current-acceptable-quest-list-skipped",
			"char_id", session.selectedCharacterID,
			"job", character.Job,
			"level", character.Level,
			"reason", "selected_character_job_or_level_invalid")
		return nil, false
	}
	characterID := character.CharacterID
	if characterID == "" {
		characterID = strconv.Itoa(int(session.selectedCharacterID))
	}
	record, hasRecord, err := repos.Quest.Load(ctx, characterID)
	if err != nil {
		s.logGameEvent(session, "game-upper-current-acceptable-quest-list-skipped",
			"char_id", session.selectedCharacterID,
			"reason", "quest_record_load_failed",
			"error", err)
		return nil, false
	}
	if !hasRecord {
		record = dnfrepo.QuestRecord{CharacterID: characterID}
	}
	// The all-task op21 is the first quest projection of every scene entry,
	// including the Luke raid channel. Retire the legacy Anton team mission
	// here so the Luke quest chain is acceptable in that same list.
	record, antonMissionDefaultCompleted, antonMissionErr := s.reconcileAntonRaidMissionDefaultCompletion(
		ctx,
		session,
		repos,
		characterID,
		record,
	)
	if antonMissionErr != nil {
		s.logGameEvent(session, "game-upper-current-acceptable-quest-list-skipped",
			"char_id", session.selectedCharacterID,
			"quest_id", dnfquest.AntonRaidMissionQuestID,
			"reason", "anton_raid_mission_default_completion_failed",
			"error", antonMissionErr)
		return nil, false
	}
	if antonMissionDefaultCompleted {
		hasRecord = true
	}
	result := catalog.QuestList(dnfquest.CharacterEligibility{
		// 婚约任务只在订婚状态（wedding_state==1）下发，见 quest.WeddingEligible。
		Wedding:  dnfquest.WeddingEligible(character),
		Level:    character.Level,
		Job:      int(job),
		GrowType: int(numericCharacterStatValue(character, "grow_type")),
	}, record)
	body := buildCurrentAcceptableQuestListBody(character.Level, result.IDs)
	s.logGameEvent(session, "game-upper-current-acceptable-quest-list-built",
		"char_id", session.selectedCharacterID,
		"character_id", characterID,
		"job", job,
		"level", character.Level,
		"grow_type", numericCharacterStatValue(character, "grow_type"),
		"quest_record_found", hasRecord,
		"anton_raid_mission_default_completed", antonMissionDefaultCompleted,
		"quest_list_count", len(result.IDs),
		"acceptable_count", len(result.IDs)-result.ActiveCount,
		"active_included_count", result.ActiveCount,
		"epic_count", result.EpicCount,
		"quest_ids", boundedQuestLogIDs(result.IDs, 32),
		"msg_id", currentAcceptableQuestListMsgID,
		"protobuf_enum", currentAcceptableQuestListEnum,
		"body_len", len(body),
		"body_source", "current_exe_sub_1D6FB50_pb_level_and_pvf_db_quest_ids_including_active_for_all_view")
	return body, true
}

func (s *Service) sendCurrentAcceptableQuestListForSession(session *gameSession, source string) error {
	_, err := s.sendCurrentAcceptableQuestListForSessionTracked(session, source)
	return err
}

func (s *Service) sendCurrentAcceptableQuestListForSessionTracked(session *gameSession, source string) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	body, ok := s.buildCurrentAcceptableQuestListBodyForSession(ctx, session)
	cancel()
	if !ok {
		return false, nil
	}
	s.logGameEvent(session, "game-upper-current-acceptable-quest-list-send",
		"char_id", session.selectedCharacterID,
		"msg_id", currentAcceptableQuestListMsgID,
		"classification", 0,
		"body_len", len(body),
		"source", source,
		"body_source", "current_exe_sub_1D6FB50_pb_level_and_pvf_db_quest_ids_including_active_for_all_view")
	if err := s.sendGameUpperRawClass(session, currentAcceptableQuestListMsgID, body, 0); err != nil {
		return false, err
	}
	if err := s.sendCurrentActiveQuestSnapshotForSession(session, source+"_after_acceptable_op21"); err != nil {
		return false, err
	}
	return true, nil
}

// sendCurrentQuestHUDReadyRefresh republishes the authoritative task pair
// after type1345 confirms that the selected actor and task HUD exist. Earlier
// pre-op24 snapshots correctly initialize native state but can be ignored by a
// widget created later in the loading route.
func (s *Service) sendCurrentQuestHUDReadyRefresh(session *gameSession, source string) error {
	if s == nil || session == nil {
		return nil
	}
	session.townMu.Lock()
	characterID := session.selectedCharacterID
	generation := session.characterGeneration
	eligible := characterID != 0 &&
		session.initialTownRouteCharacterID == characterID &&
		session.initialTownRouteStage >= currentInitialTownRoutePlayerStateSent &&
		!session.initialTownQuestHUDRefreshSent
	session.townMu.Unlock()
	if !eligible {
		return nil
	}
	sent, err := s.sendCurrentAcceptableQuestListForSessionTracked(
		session, source+"_authoritative_task_hud_ready",
	)
	if err != nil || !sent {
		return err
	}
	session.townMu.Lock()
	if session.selectedCharacterID == characterID &&
		session.characterGeneration == generation &&
		session.initialTownRouteCharacterID == characterID &&
		session.initialTownRouteStage >= currentInitialTownRoutePlayerStateSent {
		session.initialTownQuestHUDRefreshSent = true
	}
	session.townMu.Unlock()
	s.logGameEvent(session, "game-current-quest-hud-ready-refresh-sent",
		"char_id", characterID,
		"character_generation", generation,
		"source", source,
		"sequence", "confirmed_hud_ready_then_op21_then_op574")
	// The current client rebuilds its quest state once more after this refresh
	// and that rebuild drops the completed-quest rows op356 wrote during the
	// loading route, which leaves the team-mode channel row built from an empty
	// table until the player changes town. The bitmap alone does not fix that -
	// the row is only recomputed when the client re-requests the channel
	// directory, which it does on a task-state change - so replay the same
	// authoritative trio (op574 -> op356 -> op21) the accept path publishes.
	s.scheduleCurrentClearQuestListReplayAfterHUDReady(session, characterID, generation, source)
	return nil
}

// sendCurrentInitialTownQuestSnapshotsLocked publishes the task-manual
// definition list at the earliest selected-actor-safe login boundary. The
// caller holds session.townMu as part of the initial-town route; this helper
// marks only that route's one-shot snapshot so later quest accept/finish
// refreshes can still send op21/op574 normally.
func (s *Service) sendCurrentInitialTownQuestSnapshotsLocked(session *gameSession, source string) error {
	if s == nil || session == nil || session.selectedCharacterID == 0 {
		return nil
	}
	if session.initialTownQuestSnapshotsSent {
		s.logGameEvent(session, "game-initial-town-quest-snapshots-duplicate-skipped",
			"char_id", session.selectedCharacterID,
			"source", source,
			"reason", "initial_town_op21_op574_already_sent_before_typed_op24")
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	if _, err := s.repairAutoCompletedQuestionQuests(
		ctx,
		session,
		source+"_before_initial_town_quest_snapshots",
	); err != nil {
		return err
	}
	if _, err := s.reconcileAutoCompletedQuestClearLinkedSubQuests(
		ctx,
		session,
		source+"_before_initial_town_quest_snapshots",
	); err != nil {
		return err
	}
	body, ok := s.buildCurrentAcceptableQuestListBodyForSession(ctx, session)
	if !ok {
		return nil
	}
	s.logGameEvent(session, "game-initial-town-current-acceptable-quest-list-send",
		"char_id", session.selectedCharacterID,
		"msg_id", currentAcceptableQuestListMsgID,
		"classification", 0,
		"body_len", len(body),
		"source", source,
		"body_source", "current_exe_sub_1D6FB50_pb_level_and_pvf_db_quest_ids_including_active_for_all_view",
		"reason", "f1_all_task_manual_needs_definition_list_before_visible_scene_interaction")
	if err := s.sendGameUpperRawClass(session, currentAcceptableQuestListMsgID, body, 0); err != nil {
		return err
	}
	if err := s.sendCurrentActiveQuestSnapshotForSession(session, source+"_after_initial_town_acceptable_op21"); err != nil {
		return err
	}
	session.initialTownQuestSnapshotsSent = true
	return nil
}

// sendCurrentPostTownTransitionQuestSnapshotsLocked reapplies the complete
// task state after op24 has committed the destination map. The current EXE can
// only refresh quest-dependent doors and town objects that already belong to
// the active scene, and op574 must precede op356 because it rebuilds the role
// quest collection. This projection is read-only and never completes a PVF
// area requirement on the character's behalf.
func (s *Service) sendCurrentPostTownTransitionQuestSnapshotsLocked(session *gameSession, source string) error {
	if s == nil || session == nil || session.selectedCharacterID == 0 {
		return nil
	}
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.Character == nil || repositories.Quest == nil {
		s.logGameEvent(session, "game-post-town-transition-quest-snapshots-skipped",
			"char_id", session.selectedCharacterID,
			"source", source,
			"reason", "character_or_quest_repository_unavailable")
		return nil
	}
	if err := s.sendCurrentAcceptableQuestListOnlyForSession(session, source+"_acceptable_op21"); err != nil {
		return err
	}
	if err := s.sendCurrentActiveQuestSnapshotForSession(session, source+"_active_op574"); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	clearQuestListBody, err := s.buildCurrentClearQuestListTransportBodyForSession(
		ctx,
		session,
		source+"_completed_op356",
	)
	cancel()
	if err != nil {
		return err
	}
	if err := s.sendGameUpperFixed16Transport(
		session,
		currentClearQuestListMsgID,
		clearQuestListBody,
		0,
		1,
		true,
		currentClearQuestListTransportCodec,
	); err != nil {
		return err
	}
	s.logGameEvent(session, "game-post-town-transition-quest-snapshots-sent",
		"char_id", session.selectedCharacterID,
		"source", source,
		"sequence", "op21_acceptable_then_op574_active_then_op356_completed",
		"projection_only", true)
	return nil
}

func (s *Service) sendCurrentAcceptableQuestListOnlyForSession(session *gameSession, source string) error {
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	body, ok := s.buildCurrentAcceptableQuestListBodyForSession(ctx, session)
	cancel()
	if !ok {
		return nil
	}
	s.logGameEvent(session, "game-upper-current-acceptable-quest-list-send",
		"char_id", session.selectedCharacterID,
		"msg_id", currentAcceptableQuestListMsgID,
		"classification", 0,
		"body_len", len(body),
		"source", source,
		"body_source", "current_exe_sub_1D6FB50_pb_level_and_pvf_db_quest_ids_including_active_for_all_view")
	if err := s.sendGameUpperRawClass(session, currentAcceptableQuestListMsgID, body, 0); err != nil {
		return err
	}
	return nil
}

func boundedQuestLogIDs(ids []int32, limit int) []int32 {
	if limit <= 0 || len(ids) == 0 {
		return nil
	}
	if len(ids) < limit {
		limit = len(ids)
	}
	return append([]int32(nil), ids[:limit]...)
}
