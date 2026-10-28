package dnfbridge

import (
	"context"
	"encoding/binary"
	"fmt"
	"strconv"
	"strings"

	dnfenum "longheng.io/server/internal/modules/dnf/dnfenum"
)

// currentSelectInventoryBootstrapListTypes is the complete selected-character
// container set installed after the local actor has been bound. The bodies and
// envelope are built with the current NoPack op13 reader contract (16-byte
// fixed upper header and current 0x77 item rows).
var currentSelectInventoryBootstrapListTypes = []byte{0, 1, 2, 7, 12, currentGuildMedalInventoryListType}

type currentSelectInventoryBootstrapPacket struct {
	listType    byte
	body        []byte
	entrySource string
	entryCount  int
}

// sendCurrentSelectInventoryBootstrap owns the one-shot inventory bootstrap
// for a selected character. It must be called after the scene has created and
// bound the selected actor, and before the full equipment-bearing mode1 packet.
// Runtime mutations continue to use the native ACK path, op14, or an explicit
// op13 refresh where the command contract requires one.
func (s *Service) sendCurrentSelectInventoryBootstrap(session *gameSession, source string) error {
	if session == nil || session.selectedCharacterID == 0 {
		return nil
	}
	if session.selectedItemListBootstrapCharacterID == session.selectedCharacterID {
		s.logPacketEvent("game-upper-select-inventory-bootstrap-skipped",
			"conn_id", session.connID,
			"channel_id", session.channel.ID,
			"source", source,
			"char_id", session.selectedCharacterID,
			"reason", "already_sent")
		// Entering a dungeon recreates the local actor and its item objects but
		// intentionally reuses the selected-character container snapshot.  The
		// native sort-lock icon model is separate from those rows, so reassert
		// the durable blue-lock markers at that scene boundary.
		return s.sendCurrentSortLockReplayForSession(session, source+"_reassert_after_existing_container_bootstrap")
	}

	s.cleanupSelectedExpiredRentalEquipment(session, source)
	// PVF `[daily delete item]` 的物品跨过服务日边界（北京 06:00）就删掉。必须在
	// 第一份 list-0 快照**之前**跑，否则客户端会先收到一份还带着它的容器。
	s.sweepSelectedDailyDeleteItems(session, source)
	ctx, cancel := context.WithTimeout(context.Background(), currentRepositorySnapshotTimeout)
	defer cancel()
	// 公会徽章 rows an earlier build left in list-0 have to move to the
	// PVF-declared list-38 slots before the first list-0 snapshot goes out,
	// otherwise the client renders them on the wrong page and the badge can be
	// neither equipped nor compounded. Deferred on failure: a placement repair
	// must never make selected-character login fatal.
	if err := s.migrateCurrentGuildItemPlacement(ctx, session); err != nil {
		s.logGameEvent(session, "game-guild-item-placement-migration-deferred", "error", err)
	}
	// Characters saved before task-manual completion carried story items
	// forward may be stuck on an epic whose required item was never granted.
	// Repair before the first list-0 snapshot so the row rides in it.
	carryForwardRepair, carryForwardErr := s.repairCurrentAutoCompletedQuestCarryForwardItems(ctx, session)
	if carryForwardErr != nil {
		s.logPacketEvent("game-upper-select-inventory-carry-forward-repair-deferred",
			"conn_id", session.connID,
			"channel_id", session.channel.ID,
			"source", source,
			"char_id", session.selectedCharacterID,
			"err", carryForwardErr)
	} else if len(carryForwardRepair.Items) > 0 {
		s.logPacketEvent("game-upper-select-inventory-carry-forward-repaired",
			"conn_id", session.connID,
			"channel_id", session.channel.ID,
			"source", source,
			"char_id", session.selectedCharacterID,
			"item_count", len(carryForwardRepair.Items),
			"repair_source", "completed_no_reward_producer_and_outstanding_story_consumer")
	}
	characterID := strconv.Itoa(int(session.selectedCharacterID))
	inventoryChanged, equipmentChanged, customAbilityRefreshEntries, customAbilityStateErr := s.ensureCurrentCustomAbilityState(ctx, characterID)
	if customAbilityStateErr != nil {
		// Compatibility repair for rows created before custom-ability projection
		// existed must not make selected-character login a fatal dependency.
		s.logPacketEvent("game-upper-custom-ability-bootstrap-skipped",
			"conn_id", session.connID,
			"channel_id", session.channel.ID,
			"source", source,
			"char_id", session.selectedCharacterID,
			"reason", customAbilityStateErr)
	} else if inventoryChanged != 0 || equipmentChanged != 0 {
		s.logPacketEvent("game-upper-custom-ability-bootstrap-committed",
			"conn_id", session.connID,
			"channel_id", session.channel.ID,
			"source", source,
			"char_id", session.selectedCharacterID,
			"inventory_changed", inventoryChanged,
			"equipment_changed", equipmentChanged)
	}
	var customAbilitySetSnapshot currentCustomAbilitySetSnapshot
	customAbilitySetFound := false
	if repos, repositoriesOK := s.repositoryGroup(); repositoriesOK {
		loaded, found, loadErr := loadCurrentCustomAbilitySetSnapshot(ctx, repos, characterID)
		if loadErr != nil {
			s.logPacketEvent("game-upper-custom-ability-set-login-restore-skipped",
				"conn_id", session.connID,
				"channel_id", session.channel.ID,
				"source", source,
				"char_id", session.selectedCharacterID,
				"reason", loadErr)
		} else {
			customAbilitySetSnapshot = loaded
			customAbilitySetFound = found
		}
	}

	// Build the complete character snapshot before writing its first packet. A
	// later repository failure must not leave the client with only a subset of
	// its selected-character containers installed.
	plan := make([]currentSelectInventoryBootstrapPacket, 0, len(currentSelectInventoryBootstrapListTypes))
	for _, listType := range currentSelectInventoryBootstrapListTypes {
		body, entrySource, entryCount, ok := s.buildCurrentSelectInventoryBootstrapBody(ctx, session, listType)
		if !ok {
			s.logPacketEvent("game-upper-select-inventory-bootstrap-deferred",
				"conn_id", session.connID,
				"channel_id", session.channel.ID,
				"source", source,
				"char_id", session.selectedCharacterID,
				"list_type", listType,
				"reason", "repository_snapshot_unavailable")
			return fmt.Errorf("selected character %d inventory list %d snapshot unavailable", session.selectedCharacterID, listType)
		}
		plan = append(plan, currentSelectInventoryBootstrapPacket{
			listType:    listType,
			body:        body,
			entrySource: entrySource,
			entryCount:  entryCount,
		})
	}
	// The current client keeps a separate equipment-lock model. It does not
	// derive that model from op13 item rows, so build the complete durable
	// snapshot before any container write and deliver it immediately after all
	// six containers. An explicit empty list is required to clear a stale local
	// locked state before an item-use request such as op338 can be generated.
	lockSnapshot, lockSnapshotOK := s.buildCurrentItemLockSnapshotForSession(ctx, session)
	if !lockSnapshotOK {
		s.logPacketEvent("game-upper-select-inventory-bootstrap-deferred",
			"conn_id", session.connID,
			"channel_id", session.channel.ID,
			"source", source,
			"char_id", session.selectedCharacterID,
			"reason", "item_lock_snapshot_unavailable")
		return fmt.Errorf("selected character %d item-lock snapshot unavailable", session.selectedCharacterID)
	}
	sortLockPositions, sortLockPositionsOK := s.buildCurrentSortLockPositionsForSession(ctx, session)
	if !sortLockPositionsOK {
		s.logPacketEvent("game-upper-select-inventory-bootstrap-deferred",
			"conn_id", session.connID,
			"channel_id", session.channel.ID,
			"source", source,
			"char_id", session.selectedCharacterID,
			"reason", "item_sort_lock_snapshot_unavailable")
		return fmt.Errorf("selected character %d item-sort-lock snapshot unavailable", session.selectedCharacterID)
	}
	for index, packet := range plan {
		s.logPacketEvent("game-upper-select-inventory-bootstrap-send",
			"conn_id", session.connID,
			"channel_id", session.channel.ID,
			"source", source,
			"char_id", session.selectedCharacterID,
			"packet_index", index,
			"msg_id", uint16(dnfenum.CmdPacketLeaveParty),
			"list_type", packet.listType,
			"entry_count", packet.entryCount,
			"body_len", len(packet.body),
			"body_source", packet.entrySource,
			"entry_size", currentItemListEntrySizeForType(packet.listType),
			"wire_contract", "current_exe_sub_1D72380_fixed16_0x77",
			"lifecycle", "current_exe_actor_bound_then_character_containers_before_full_mode1")
		if err := s.sendCurrentSceneItemList(session, uint16(dnfenum.CmdPacketLeaveParty), packet.body); err != nil {
			return err
		}
	}
	if err := s.sendCurrentCharacterDailyUseStates(session, source+"_daily_use_state"); err != nil {
		return err
	}
	if len(customAbilityRefreshEntries) != 0 {
		rowSummary := make([]string, 0, len(customAbilityRefreshEntries))
		for _, entry := range customAbilityRefreshEntries {
			rowSummary = append(rowSummary, fmt.Sprintf(
				"%d:%d:custom=%x",
				binary.LittleEndian.Uint16(entry.data[0x00:0x02]),
				binary.LittleEndian.Uint32(entry.data[0x02:0x06]),
				entry.data[currentCustomAbilityRawBlockStart:currentCustomAbilityRawBlockEnd],
			))
		}
		body := buildCurrentItemUpdateBody(currentExeInventoryMainListType, customAbilityRefreshEntries)
		s.logPacketEvent("game-upper-custom-ability-inventory-refresh-send",
			"conn_id", session.connID,
			"channel_id", session.channel.ID,
			"source", source,
			"char_id", session.selectedCharacterID,
			"msg_id", uint16(dnfenum.CmdPacketWalkoutPartyMember),
			"classification", 0,
			"list_type", currentExeInventoryMainListType,
			"entry_count", len(customAbilityRefreshEntries),
			"body_len", len(body),
			"row_summary", strings.Join(rowSummary, ";"),
			"sequence", "op13_selected_containers_then_targeted_op14_custom_ability_object_refresh")
		if err := s.sendGameUpperRawClass(session, uint16(dnfenum.CmdPacketWalkoutPartyMember), body, 0); err != nil {
			return err
		}
	}
	if customAbilitySetFound {
		if err := s.sendCurrentCustomAbilitySetNotification(session, customAbilitySetSnapshot, source+"_after_item_containers"); err != nil {
			return err
		}
	}
	if err := s.sendCurrentSelectSortLockReplay(
		session,
		source+"_after_selected_containers_before_item_lock_snapshot",
		sortLockPositions,
	); err != nil {
		return err
	}
	if err := s.sendCurrentSelectItemLockSnapshot(
		session,
		source+"_after_selected_containers_before_full_mode1",
		lockSnapshot,
	); err != nil {
		return err
	}
	if err := s.sendCurrentEpicBookSnapshot(session, source+"_after_selected_containers"); err != nil {
		return err
	}
	session.selectedItemListRefreshSent = true
	session.selectedItemListBootstrapCharacterID = session.selectedCharacterID
	s.logPacketEvent("game-upper-select-inventory-bootstrap-committed",
		"conn_id", session.connID,
		"channel_id", session.channel.ID,
		"source", source,
		"char_id", session.selectedCharacterID,
		"packet_count", len(plan)+len(sortLockPositions)+1,
		"item_sort_lock_replay_count", len(sortLockPositions),
		"item_lock_snapshot_body_len", len(lockSnapshot))
	return nil
}

func (s *Service) buildCurrentSelectInventoryBootstrapBody(
	ctx context.Context,
	session *gameSession,
	listType byte,
) ([]byte, string, int, bool) {
	body, source, count, ok := s.buildCurrentItemListBodyForSession(ctx, session, listType)
	if ok {
		return body, source, count, true
	}

	// An absent inventory record is a real empty inventory. 86JP still creates
	// every client container on selection, so emit an empty current-EXE body;
	// repository errors remain fail-closed and are never disguised as empty.
	repos, repositoriesOK := s.repositoryGroup()
	if !repositoriesOK || repos.Inventory == nil {
		return nil, source, count, false
	}
	characterID := strconv.Itoa(int(session.selectedCharacterID))
	_, found, err := repos.Inventory.Load(ctx, characterID)
	if err != nil || found {
		return nil, source, count, false
	}
	state := s.loadCurrentItemListContainerState(ctx, repos.Settings, characterID, listType)
	return buildCurrentItemListBody(listType, nil, state), "inventory_absent_empty", 0, true
}
