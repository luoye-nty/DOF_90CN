package dnfbridge

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	dnfhellparty "longheng.io/server/internal/modules/dnf/hellparty"
	dnfquest "longheng.io/server/internal/modules/dnf/quest"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	"longheng.io/server/internal/modules/dnf/worldmap"
)

var errDungeonClearMapCompletionUnavailable = errors.New("dnf dungeon clear-map completion owner is unavailable")

type currentDungeonClearMapCompletionAuthority uint8

const (
	currentDungeonClearMapCompletionAuthorityBossDieCheck currentDungeonClearMapCompletionAuthority = iota + 1
	currentDungeonClearMapCompletionAuthorityScriptedAltar
)

// persistCurrentDungeonClearMapCompletionPhaseA closes only the quest-row
// Phase-A barrier. It may move real, active PVF clear-map quests to trigger
// zero with a pending reward marker, but it never grants a reward, experience,
// SP, currency, or an item.
//
// The caller holds session.dungeon.mu. A successful no-match is still retained
// on the runtime so an op115 write retry cannot repeat repository work. The
// stable key and timestamp are allocated before the first repository attempt,
// which makes a failed-persistence replay deterministic as well.
func (s *Service) persistCurrentDungeonClearMapCompletionPhaseA(
	ctx context.Context,
	session *gameSession,
	runtime *runtimeDungeonState,
	snapshot worldmap.DungeonSessionSnapshot,
) error {
	return s.persistCurrentDungeonClearMapCompletionPhaseAWithAuthority(
		ctx,
		session,
		runtime,
		snapshot,
		currentDungeonClearMapCompletionAuthorityBossDieCheck,
	)
}

// persistCurrentDungeonScriptedAltarClearMapCompletionPhaseA uses the same
// durable quest owner as an ordinary clear, but its authority is the exhausted
// server-owned altar round table rather than a fabricated boss object/op117.
func (s *Service) persistCurrentDungeonScriptedAltarClearMapCompletionPhaseA(
	ctx context.Context,
	session *gameSession,
	runtime *runtimeDungeonState,
	snapshot worldmap.DungeonSessionSnapshot,
) error {
	return s.persistCurrentDungeonClearMapCompletionPhaseAWithAuthority(
		ctx,
		session,
		runtime,
		snapshot,
		currentDungeonClearMapCompletionAuthorityScriptedAltar,
	)
}

func (s *Service) persistCurrentDungeonClearMapCompletionPhaseAWithAuthority(
	ctx context.Context,
	session *gameSession,
	runtime *runtimeDungeonState,
	snapshot worldmap.DungeonSessionSnapshot,
	authority currentDungeonClearMapCompletionAuthority,
) error {
	if session == nil || runtime == nil || runtime.Session == nil || session.dungeon.runtime != runtime {
		return errDungeonClearMapCompletionUnavailable
	}
	if runtime.clearMapCompletionPhaseAPersisted {
		return nil
	}
	authorityAccepted := false
	receiptSource := "clear_map_phase_a"
	eventSource := "authoritative_completed_op117_before_op115_op31"
	switch authority {
	case currentDungeonClearMapCompletionAuthorityBossDieCheck:
		authorityAccepted = runtime.bossDieCheckAccepted
	case currentDungeonClearMapCompletionAuthorityScriptedAltar:
		authorityAccepted = currentDungeonScriptedAltarClearMapCompletionAccepted(runtime)
		receiptSource = "blood_altar_scripted_clear_map_phase_a"
		eventSource = "authoritative_scripted_blood_altar_round_completion"
	default:
		return errDungeonClearMapCompletionUnavailable
	}
	if !authorityAccepted || snapshot.Run.Status != worldmap.DungeonRunCompleted ||
		snapshot.Run.DungeonID != runtime.Dungeon.ID || snapshot.Run.MazeIndex != runtime.MazeIndex ||
		snapshot.Scene.Map.Map.ID <= 0 || !dungeonRuntimeOwnsCharacter(runtime, session.selectedCharacterID) {
		return fmt.Errorf(
			"%w: authority=%d accepted=%t status=%s runtime_dungeon=%d snapshot_dungeon=%d runtime_maze=%d snapshot_maze=%d map=%d",
			errDungeonClearMapCompletionUnavailable,
			authority,
			authorityAccepted,
			snapshot.Run.Status,
			runtime.Dungeon.ID,
			snapshot.Run.DungeonID,
			runtime.MazeIndex,
			snapshot.Run.MazeIndex,
			snapshot.Scene.Map.Map.ID,
		)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ensureCurrentDungeonCompletionReceiptKey(
		session,
		runtime,
		snapshot,
		s.gameplayNow(),
		receiptSource,
	); err != nil {
		return err
	}

	repositories, ok := s.repositoryGroup()
	if !ok || repositories.Character == nil || repositories.Quest == nil {
		return errDungeonClearMapCompletionUnavailable
	}
	characterID := strconv.FormatUint(uint64(session.selectedCharacterID), 10)
	if _, found, err := repositories.Character.Load(ctx, characterID); err != nil {
		return err
	} else if !found {
		return dnfquest.ErrCharacterNotFound
	}
	// An absent quest row means there is no persisted active quest to mutate.
	// Avoid loading PVF or creating a synthetic row in that case.
	if _, found, err := repositories.Quest.Load(ctx, characterID); err != nil {
		return err
	} else if !found {
		runtime.clearMapCompletionQuestIDs = nil
		runtime.clearMapCompletionCompletedQuestIDs = nil
		runtime.clearMapCompletionPendingQuestIDs = nil
		runtime.clearMapCompletionPhaseAPersisted = true
		s.logGameEvent(session, "game-dungeon-clear-map-phase-a-persisted",
			"char_id", session.selectedCharacterID,
			"dungeon_id", runtime.Dungeon.ID,
			"maze_index", runtime.MazeIndex,
			"map_id", snapshot.Scene.Map.Map.ID,
			"completion_key", runtime.clearMapCompletionKey,
			"completion_count", 0,
			"changed_fields", []dnfrepo.QuestField(nil),
			"quest_record_found", false,
			"reward_granted", false,
			"source", eventSource)
		return nil
	}

	catalog, err := s.loadQuestCatalog(ctx)
	if err != nil {
		return fmt.Errorf("load clear-map quest catalog: %w", err)
	}
	owner, err := dnfquest.NewOwner(repositories)
	if err != nil {
		return fmt.Errorf("create clear-map quest owner: %w", err)
	}
	table, _, worldMapErr := s.dungeonWorldMap()
	worldMapID := int64(-1)
	if worldMapErr == nil {
		if area, found := table.FindAreaByDungeon(runtime.Dungeon.ID); found {
			worldMapID = area.ID
		}
	}
	hellPartyComplete := runtime.HellParty != nil &&
		runtime.HellParty.Runtime != nil &&
		runtime.HellParty.Runtime.Phase() == dnfhellparty.PhaseComplete
	allRoomsCleared := currentDungeonAllRoomsCleared(runtime.Session.Rooms(), snapshot.Run.Cleared)
	// 婚约任务(subtype 22)只认"正好这对夫妻、两人、普通副本"的通关。判定用副本
	// owner 的入场名册（不是城镇队伍列表），并在首次权威通关时冻结，
	// 避免先回城的一方把自己的状态改掉、让另一方的结果失效。
	weddingCouple, err := s.currentWeddingDungeonCouple(ctx, runtime)
	if err != nil {
		return err
	}
	result, err := owner.ApplyClearMapCompletion(ctx, catalog, characterID, dnfquest.ClearMapCompletionInput{
		WeddingCouple:     weddingCouple,
		DungeonID:         runtime.Dungeon.ID,
		MapID:             snapshot.Scene.Map.Map.ID,
		WorldMapID:        worldMapID,
		Difficulty:        int64(runtime.Request.Difficulty),
		HellPartyComplete: hellPartyComplete,
		AllRoomsCleared:   allRoomsCleared,
		CompletionKey:     runtime.clearMapCompletionKey,
		CompletedAt:       runtime.clearMapCompletionAt,
	})
	if err != nil {
		return fmt.Errorf("persist clear-map quest phase A: %w", err)
	}
	questIDs := make([]int64, 0, len(result.Completions))
	completedQuestIDs := make([]int64, 0, len(result.Completions))
	for _, completion := range result.Completions {
		questIDs = append(questIDs, completion.QuestID)
		if completion.Completed {
			completedQuestIDs = append(completedQuestIDs, completion.QuestID)
		}
	}
	pendingQuestIDs := make([]int64, 0, len(result.Completions))
	if len(result.Completions) != 0 {
		if persisted, found, loadErr := repositories.Quest.Load(ctx, characterID); loadErr != nil {
			return loadErr
		} else if found {
			pendingQuestIDs = currentDungeonClearMapPendingRewardQuestIDs(persisted, result.Completions)
		}
	}
	runtime.clearMapCompletionQuestIDs = append(runtime.clearMapCompletionQuestIDs[:0], questIDs...)
	runtime.clearMapCompletionCompletedQuestIDs = append(runtime.clearMapCompletionCompletedQuestIDs[:0], completedQuestIDs...)
	runtime.clearMapCompletionPendingQuestIDs = append(runtime.clearMapCompletionPendingQuestIDs[:0], pendingQuestIDs...)
	runtime.clearMapCompletionPhaseAPersisted = true
	s.logGameEvent(session, "game-dungeon-clear-map-phase-a-persisted",
		"char_id", session.selectedCharacterID,
		"dungeon_id", runtime.Dungeon.ID,
		"maze_index", runtime.MazeIndex,
		"map_id", snapshot.Scene.Map.Map.ID,
		"completion_key", runtime.clearMapCompletionKey,
		"completion_count", len(result.Completions),
		"quest_ids", questIDs,
		"completed_quest_ids", completedQuestIDs,
		"worldmap_id", worldMapID,
		"difficulty", runtime.Request.Difficulty,
		"hell_party_complete", hellPartyComplete,
		"all_rooms_cleared", allRoomsCleared,
		"changed_fields", result.ChangedFields,
		"idempotent", result.Idempotent,
		"quest_record_found", true,
		"reward_granted", false,
		"source", eventSource)
	return nil
}

func currentDungeonScriptedAltarClearMapCompletionAccepted(runtime *runtimeDungeonState) bool {
	if runtime == nil || runtime.altar == nil || runtime.altar.progress != currentDungeonAltarProgressDungeonComplete ||
		runtime.altar.completedRounds != runtime.altar.MaxRounds {
		return false
	}
	return (runtime.Dungeon.ID == 11006 && runtime.altar.Kind == worldmap.BloodAltarDungeonKindEndless && runtime.altar.MaxRounds == 15) ||
		(runtime.Dungeon.ID == 11007 && runtime.altar.Kind == worldmap.BloodAltarDungeonKindUltimate && runtime.altar.MaxRounds == 7)
}

func currentDungeonAllRoomsCleared(rooms []worldmap.DungeonRoom, cleared []worldmap.RoomCoordinate) bool {
	if len(rooms) == 0 {
		return false
	}
	clearedSet := make(map[worldmap.RoomCoordinate]struct{}, len(cleared))
	for _, coordinate := range cleared {
		clearedSet[coordinate] = struct{}{}
	}
	for _, room := range rooms {
		if _, ok := clearedSet[room.Coordinate]; !ok {
			return false
		}
	}
	return true
}

func ensureCurrentDungeonCompletionReceiptKey(
	session *gameSession,
	runtime *runtimeDungeonState,
	snapshot worldmap.DungeonSessionSnapshot,
	now time.Time,
	source string,
) error {
	if session == nil || runtime == nil || runtime.Session == nil ||
		snapshot.Run.DungeonID != runtime.Dungeon.ID ||
		snapshot.Run.MazeIndex != runtime.MazeIndex ||
		snapshot.Scene.Map.Map.ID <= 0 ||
		!dungeonRuntimeOwnsCharacter(runtime, session.selectedCharacterID) {
		return fmt.Errorf(
			"%w: source=%s runtime_dungeon=%d snapshot_dungeon=%d runtime_maze=%d snapshot_maze=%d map=%d",
			errDungeonClearMapCompletionUnavailable,
			source,
			runtime.Dungeon.ID,
			snapshot.Run.DungeonID,
			runtime.MazeIndex,
			snapshot.Run.MazeIndex,
			snapshot.Scene.Map.Map.ID,
		)
	}
	if runtime.clearMapCompletionAt.IsZero() {
		runtime.clearMapCompletionAt = now.UTC()
	}
	if runtime.clearMapCompletionKey == "" {
		runtime.clearMapCompletionKey = fmt.Sprintf(
			"dungeon-clear:char:%d:dungeon:%d:maze:%d:map:%d:seed:%d:completed_at_unix_nano:%d:target:%d:source:%s",
			session.selectedCharacterID,
			runtime.Dungeon.ID,
			runtime.MazeIndex,
			snapshot.Scene.Map.Map.ID,
			runtime.Seed,
			runtime.clearMapCompletionAt.UnixNano(),
			runtime.bossDieCheckTargetObjectKey,
			source,
		)
	}
	return nil
}

// sendCurrentDungeonClearMapCompletionNotificationLocked publishes the real
// post-persistence active-quest snapshot before settlement starts. The current
// EXE's class0/op0x023E reader rebuilds the active task group and has an
// explicit trigger-zero completion branch, so this is a state notification,
// not a fabricated FINISH_QUEST ACK or reward grant.
//
// The caller holds session.dungeon.mu. A failed repository verification or
// socket write leaves the barrier open so the exact op117 replay resumes at
// this stage without repeating Phase A.
func (s *Service) sendCurrentDungeonClearMapCompletionNotificationLocked(
	ctx context.Context,
	session *gameSession,
	runtime *runtimeDungeonState,
) error {
	if session == nil || runtime == nil || session.dungeon.runtime != runtime ||
		!runtime.clearMapCompletionPhaseAPersisted {
		return errDungeonClearMapCompletionUnavailable
	}
	if runtime.clearMapCompletionNotificationClosed {
		return nil
	}
	if len(runtime.clearMapCompletionQuestIDs) == 0 {
		runtime.clearMapCompletionNotificationClosed = true
		s.logGameEvent(session, "game-dungeon-clear-map-active-quest-snapshot-skipped",
			"char_id", session.selectedCharacterID,
			"dungeon_id", runtime.Dungeon.ID,
			"completion_key", runtime.clearMapCompletionKey,
			"reason", "no_matching_persisted_clear_map_quest",
			"reward_granted", false)
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.Quest == nil {
		return errDungeonClearMapCompletionUnavailable
	}
	characterID := strconv.FormatUint(uint64(session.selectedCharacterID), 10)
	record, found, err := repositories.Quest.Load(ctx, characterID)
	if err != nil {
		return err
	}
	if !found {
		return dnfquest.ErrQuestPersistVerify
	}

	rows := currentSelectAckQuestRows(record, true)
	for _, questID := range runtime.clearMapCompletionPendingQuestIDs {
		visible := false
		for _, row := range rows {
			if int64(row.questID) == questID && row.triggerValue == 0 {
				visible = true
				break
			}
		}
		if !visible {
			return fmt.Errorf("%w: pending-reward quest %d is absent from trigger-zero active snapshot", dnfquest.ErrQuestPersistVerify, questID)
		}
		state, ok := currentDungeonClearMapQuestState(record, questID)
		if !ok || state.ProgressValue != 0 || state.Extra["completion_key"] != runtime.clearMapCompletionKey ||
			state.Extra["reward_state"] != "pending" {
			return fmt.Errorf("%w: quest %d pending-reward marker mismatch", dnfquest.ErrQuestPersistVerify, questID)
		}
	}

	body := buildCurrentActiveQuestSnapshotBody(record, true)
	if !runtime.clearMapCompletionActiveSnapshotSent {
		s.logGameEvent(session, "game-dungeon-clear-map-active-quest-snapshot-send",
			"char_id", session.selectedCharacterID,
			"dungeon_id", runtime.Dungeon.ID,
			"maze_index", runtime.MazeIndex,
			"map_id", runtime.Session.Snapshot().Scene.Map.Map.ID,
			"completion_key", runtime.clearMapCompletionKey,
			"quest_ids", append([]int64(nil), runtime.clearMapCompletionQuestIDs...),
			"pending_reward_quest_ids", append([]int64(nil), runtime.clearMapCompletionPendingQuestIDs...),
			"active_snapshot_rows", len(rows),
			"msg_id", currentActiveQuestSnapshotMsgID,
			"classification", 0,
			"body_len", len(body),
			"ordering", "phase_a_persisted_then_op574_before_optional_completed_op21_and_op115_op31",
			"reward_granted", false,
			"body_source", "current_exe_sub_1D632D0_u32_count_u16_quest_u32_trigger")
		if err := s.sendGameUpperRawClass(session, currentActiveQuestSnapshotMsgID, body, 0); err != nil {
			return err
		}
		runtime.clearMapCompletionActiveSnapshotSent = true
	}
	if currentDungeonClearMapRequiresAcceptableQuestRefresh(
		runtime.clearMapCompletionCompletedQuestIDs,
		runtime.clearMapCompletionPendingQuestIDs,
	) {
		s.logGameEvent(session, "game-dungeon-clear-map-acceptable-quest-refresh-send",
			"char_id", session.selectedCharacterID,
			"dungeon_id", runtime.Dungeon.ID,
			"maze_index", runtime.MazeIndex,
			"map_id", runtime.Session.Snapshot().Scene.Map.Map.ID,
			"completion_key", runtime.clearMapCompletionKey,
			"quest_ids", append([]int64(nil), runtime.clearMapCompletionQuestIDs...),
			"pending_reward_quest_ids", append([]int64(nil), runtime.clearMapCompletionPendingQuestIDs...),
			"msg_id", currentAcceptableQuestListMsgID,
			"classification", 0,
			"ordering", "completed_no_reward_quest_persisted_then_op574_then_op21_before_op115_op31",
			"body_source", "s4a12_completion_projection_timing_with_current_exe_op21_builder")
		if err := s.sendCurrentAcceptableQuestListOnlyForSession(
			session,
			"dungeon_clear_map_completed_no_reward_quest_after_active_snapshot_before_settlement",
		); err != nil {
			return err
		}
	}
	runtime.clearMapCompletionNotificationClosed = true
	return nil
}

func currentDungeonClearMapRequiresAcceptableQuestRefresh(completedQuestIDs, pendingRewardQuestIDs []int64) bool {
	if len(completedQuestIDs) == 0 {
		return false
	}
	pending := make(map[int64]struct{}, len(pendingRewardQuestIDs))
	for _, questID := range pendingRewardQuestIDs {
		pending[questID] = struct{}{}
	}
	for _, questID := range completedQuestIDs {
		if _, stillPending := pending[questID]; !stillPending {
			return true
		}
	}
	return false
}

func currentDungeonClearMapPendingRewardQuestIDs(record dnfrepo.QuestRecord, completions []dnfquest.ClearMapCompletion) []int64 {
	if len(completions) == 0 {
		return nil
	}
	pending := make([]int64, 0, len(completions))
	for _, completion := range completions {
		state, ok := currentDungeonClearMapQuestState(record, completion.QuestID)
		if !ok || state.ProgressValue != 0 || state.Extra == nil {
			continue
		}
		if state.Extra["reward_state"] == "pending" && stringsEqualFoldTrim(state.Status, "active") {
			pending = append(pending, completion.QuestID)
		}
	}
	return pending
}

func currentDungeonClearMapQuestState(record dnfrepo.QuestRecord, questID int64) (dnfrepo.QuestState, bool) {
	if state, ok := record.States[questID]; ok {
		return state, true
	}
	if state, ok := record.Progress[questID]; ok {
		return state, true
	}
	return dnfrepo.QuestState{}, false
}

func stringsEqualFoldTrim(left, right string) bool {
	return strings.EqualFold(strings.TrimSpace(left), strings.TrimSpace(right))
}
