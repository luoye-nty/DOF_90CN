package dnfbridge

import (
	"context"
	"sort"
	"strings"

	dnfdungeonpermission "longheng.io/server/internal/modules/dnf/dungeonpermission"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	"longheng.io/server/internal/modules/dnf/worldmap"
)

const (
	currentDungeonResourceStateMsgID = 5
	// The accepted select-dungeon request owns state 1 for its selected PVF dungeon.
	currentDungeonResourceSelectedState = 1
)

type currentDungeonResourceStateEntry struct {
	DungeonID uint32
	State     byte
}

func buildCurrentDungeonResourceStateEntriesBody(entries []currentDungeonResourceStateEntry) []byte {
	var writer packetWriter
	if len(entries) > int(^uint16(0)) {
		entries = entries[:int(^uint16(0))]
	}
	writer.writeUint16(uint16(len(entries)))
	for _, entry := range entries {
		writer.writeUint32(entry.DungeonID)
		writer.writeByte(entry.State)
	}
	return writer.bytes()
}

func buildCurrentDungeonResourceStateBody(dungeonID uint32) []byte {
	return buildCurrentDungeonResourceStateEntriesBody([]currentDungeonResourceStateEntry{{
		DungeonID: dungeonID,
		State:     currentDungeonResourceSelectedState,
	}})
}

func (s *Service) sendCurrentDungeonResourceState(
	session *gameSession,
	runtime *runtimeDungeonState,
	source string,
) error {
	if session == nil || runtime == nil || runtime.Dungeon.ID <= 0 {
		return nil
	}
	clientDungeonID := currentDungeonClientID(runtime)
	if clientDungeonID <= 0 || clientDungeonID > int64(^uint32(0)) {
		return nil
	}
	body := buildCurrentDungeonResourceStateBody(uint32(clientDungeonID))
	s.logGameEvent(session, "game-upper-current-dungeon-resource-state-op5-send",
		"source", source,
		"char_id", session.selectedCharacterID,
		"msg_id", currentDungeonResourceStateMsgID,
		"classification", 0,
		"body_len", len(body),
		"row_count", 1,
		"dungeon_id", runtime.Dungeon.ID,
		"client_dungeon_id", clientDungeonID,
		"state", currentDungeonResourceSelectedState,
		"state_source", "validated_current_select_dungeon_request_and_real_pvf_runtime",
		"body_source", "current_exe_sub_1D37AC0_u16_count_u32_key_u8_state")
	return s.sendGameUpperRawClass(session, currentDungeonResourceStateMsgID, body, 0)
}

func (s *Service) sendCurrentDungeonPermissionSnapshot(
	ctx context.Context,
	session *gameSession,
	source string,
) error {
	entries, err := s.currentDungeonPermissionSnapshotEntries(ctx, session)
	if err != nil {
		s.logGameEvent(session, "game-upper-current-dungeon-permission-op5-snapshot-blocked",
			"source", source,
			"char_id", sessionSelectedCharacterID(session),
			"reason", "repository_or_worldmap_unavailable",
			"error", err)
		return nil
	}
	if len(entries) == 0 {
		s.logGameEvent(session, "game-upper-current-dungeon-permission-op5-snapshot-empty",
			"source", source,
			"char_id", sessionSelectedCharacterID(session),
			"reason", "no_current_pvf_ordinary_difficulty_definition")
		return nil
	}
	body := buildCurrentDungeonResourceStateEntriesBody(entries)
	s.logGameEvent(session, "game-upper-current-dungeon-permission-op5-snapshot-send",
		"source", source,
		"char_id", sessionSelectedCharacterID(session),
		"msg_id", currentDungeonResourceStateMsgID,
		"classification", 0,
		"body_len", len(body),
		"row_count", len(entries),
		"body_source", "current_exe_sub_1D37AC0_u16_count_u32_key_u8_state",
		"state_source", "current_pvf_complete_ordinary_difficulty_authority_with_persisted_max_overrides")
	return s.sendGameUpperRawClass(session, currentDungeonResourceStateMsgID, body, 0)
}

func (s *Service) currentDungeonPermissionSnapshotEntries(
	ctx context.Context,
	session *gameSession,
) ([]currentDungeonResourceStateEntry, error) {
	if session == nil || session.selectedCharacterID == 0 {
		return nil, nil
	}
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.Character == nil || repositories.DungeonPermission == nil {
		return nil, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	snapshot, err := s.currentAccountDungeonPermissionSnapshot(ctx, session, repositories)
	if err != nil {
		return nil, err
	}
	table, _, err := s.dungeonWorldMap()
	if err != nil {
		return nil, err
	}
	entries := currentDungeonResourceStateEntriesFromPermissionRecord(table, snapshot.Record)
	return entries, nil
}

func (s *Service) currentAccountDungeonPermissionSnapshot(
	ctx context.Context,
	session *gameSession,
	repositories dnfrepo.Group,
) (dnfdungeonpermission.Snapshot, error) {
	accountID := strings.TrimSpace(s.accountIDForSession(session))
	owner, err := dnfdungeonpermission.NewOwner(repositories)
	if err != nil {
		return dnfdungeonpermission.Snapshot{}, err
	}
	snapshot, err := owner.LoadAccount(ctx, accountID)
	if err != nil {
		return dnfdungeonpermission.Snapshot{}, err
	}
	if snapshot.MigratedEntries != 0 {
		s.logGameEvent(session, "game-dungeon-permission-account-migrated",
			"account_id", snapshot.AccountID,
			"char_id", sessionSelectedCharacterID(session),
			"legacy_owner_count", snapshot.LegacyOwners,
			"migrated_entry_count", snapshot.MigratedEntries,
			"state_source", "account_max_over_all_active_legacy_character_owners")
	}
	return snapshot, nil
}

func currentDungeonResourceStateEntriesFromPermissionRecord(
	table *worldmap.Table,
	record dnfrepo.DungeonPermissionRecord,
) []currentDungeonResourceStateEntry {
	if table == nil {
		return nil
	}
	// Current EXE class0/op5 is the complete ordinary-dungeon difficulty
	// authority table, not a sparse list of dungeons that have already been
	// cleared. A zero state is meaningful: it exposes the dungeon at normal
	// difficulty. Omitting that row makes the client depend on a live quest
	// marker to discover the dungeon and hides the same unlocked map as soon as
	// the task is absent or completed.
	stateByDungeon := make(map[uint32]byte, len(record.Entries))
	for _, entry := range record.Entries {
		if entry.DungeonID == 0 {
			continue
		}
		if current, found := stateByDungeon[entry.DungeonID]; !found || entry.ClearState > current {
			stateByDungeon[entry.DungeonID] = entry.ClearState
		}
	}
	dungeons := table.Dungeons()
	out := make([]currentDungeonResourceStateEntry, 0, len(dungeons))
	for _, dungeon := range dungeons {
		if dungeon.ID <= 0 || dungeon.ID > int64(^uint32(0)) || len(dungeon.Metadata.DifficultyLevels) == 0 {
			continue
		}
		state := stateByDungeon[uint32(dungeon.ID)]
		if maxState := currentDungeonMaxClearState(dungeon); maxState == 0 {
			continue
		} else if state > maxState {
			state = maxState
		}
		out = append(out, currentDungeonResourceStateEntry{
			DungeonID: uint32(dungeon.ID),
			State:     state,
		})
	}
	sort.SliceStable(out, func(left, right int) bool {
		return out[left].DungeonID < out[right].DungeonID
	})
	return out
}

func currentDungeonMaxClearState(dungeon worldmap.Dungeon) byte {
	if len(dungeon.Metadata.DifficultyLevels) != 0 {
		count := 0
		for _, level := range dungeon.Metadata.DifficultyLevels {
			if level != 0 {
				count++
			}
		}
		if count > 1 {
			if count-1 > int(^uint8(0)) {
				return ^uint8(0)
			}
			return byte(count - 1)
		}
		// A dungeon with one declared level is a fixed-difficulty dungeon. The
		// current client still expects state 1 after its first clear so that
		// world-map prerequisites such as ordinary Luke/Anton advance to 1/1.
		if count == 1 {
			return 1
		}
	}
	if len(dungeon.Metadata.DesignatedDifficulties) != 0 {
		return 4
	}
	if dungeon.Metadata.Difficulty.Set && dungeon.Metadata.Difficulty.Value >= 0 {
		return 4
	}
	// Real fixed-difficulty PVF dungeons commonly omit every difficulty tag.
	// Treating that shape as unsupported drops the settlement clear entirely;
	// state 1 is the client's first-clear value and preserves the prerequisite.
	return 1
}

func currentDungeonNextClearState(dungeon worldmap.Dungeon, difficulty byte) byte {
	maxState := currentDungeonMaxClearState(dungeon)
	if maxState == 0 {
		return 0
	}
	next := difficulty + 1
	if next == 0 {
		next = 1
	}
	if next > maxState {
		next = maxState
	}
	return next
}

func (s *Service) commitCurrentDungeonPermission(
	ctx context.Context,
	session *gameSession,
	runtime *runtimeDungeonState,
) ([]byte, byte, bool, error) {
	if session == nil || runtime == nil || session.selectedCharacterID == 0 || runtime.Dungeon.ID <= 0 {
		return nil, 0, false, nil
	}
	state := currentDungeonNextClearState(runtime.Dungeon, runtime.Request.Difficulty)
	if state == 0 || runtime.Dungeon.ID > int64(^uint32(0)) {
		return nil, 0, false, nil
	}
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.Character == nil || repositories.DungeonPermission == nil {
		return nil, 0, false, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	accountID := strings.TrimSpace(s.accountIDForSession(session))
	owner, err := dnfdungeonpermission.NewOwner(repositories)
	if err != nil {
		return nil, 0, false, err
	}
	snapshot, entry, updated, err := owner.Advance(
		ctx,
		accountID,
		uint32(runtime.Dungeon.ID),
		state,
	)
	if err != nil {
		return nil, 0, false, err
	}
	if !updated {
		s.logGameEvent(session, "game-dungeon-permission-clear-state-unchanged",
			"account_id", snapshot.AccountID,
			"char_id", session.selectedCharacterID,
			"dungeon_id", runtime.Dungeon.ID,
			"difficulty", runtime.Request.Difficulty,
			"state", entry.ClearState,
			"state_source", "persisted_clear_state_already_at_or_above_current_clear")
		return nil, entry.ClearState, false, nil
	}
	body := buildCurrentDungeonResourceStateEntriesBody([]currentDungeonResourceStateEntry{{
		DungeonID: entry.DungeonID,
		State:     entry.ClearState,
	}})
	s.logGameEvent(session, "game-dungeon-permission-clear-state-updated",
		"account_id", snapshot.AccountID,
		"char_id", session.selectedCharacterID,
		"dungeon_id", runtime.Dungeon.ID,
		"difficulty", runtime.Request.Difficulty,
		"state", entry.ClearState,
		"body_len", len(body),
		"body_source", "current_exe_sub_1D37AC0_u16_count_u32_key_u8_state",
		"state_source", "account_shared_settlement_clear_difficulty_plus_one_capped_by_runtime_pvf_or_fixed_difficulty_default")
	return body, entry.ClearState, true, nil
}

func sessionSelectedCharacterID(session *gameSession) uint16 {
	if session == nil {
		return 0
	}
	return session.selectedCharacterID
}
