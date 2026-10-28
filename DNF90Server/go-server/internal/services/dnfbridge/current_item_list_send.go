package dnfbridge

import (
	"context"
	"fmt"
	"math"
	"strconv"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

func (s *Service) sendSelectedCurrentItemLists(session *gameSession, source string) error {
	return s.sendSelectedCurrentItemListsWithRefresh(session, source, false)
}

func (s *Service) sendSelectedCurrentItemListsRefresh(session *gameSession, source string) error {
	return s.sendSelectedCurrentItemListsWithRefresh(session, source, true)
}

func (s *Service) sendSelectedCurrentItemListsWithRefresh(session *gameSession, source string, refresh bool) error {
	s.cleanupSelectedExpiredRentalEquipment(session, source)
	// 过期道具清理必须在下面的容器快照之前：客户端拿到的列表里直接没有它们。
	s.sweepExpiredInventoryItems(session, source)
	if err := s.sendSelectedCurrentContainerListsWithRefresh(session, source, refresh); err != nil {
		return err
	}
	// Full mode1/mode3 creates the equipped objects. The working Python
	// sequence does not append a generic equipment refresh. Guardian-gem raw
	// socket words are the one proved exception: they are not contained by the
	// mode1/mode3 create rows and must be replayed through the accepted list-3
	// op14 carrier after the character containers are established.
	if err := s.sendSelectedRentalWalletStateWithRefresh(session, source, refresh); err != nil {
		return err
	}
	if err := s.sendSelectedGuardianGemWornMedalRefresh(session, source+"_after_containers"); err != nil {
		return err
	}
	// Same class as the guardian-gem socket words above: the raw title enchant
	// (+0x61..+0x65) is not contained by the mode1/mode3 create rows, so the worn
	// title row has to be replayed through the accepted list-3 op14 carrier.
	return s.sendSelectedWornTitleEnchantRowRefresh(session, source+"_after_containers")
}

func (s *Service) sendSelectedCurrentContainerListsRefresh(session *gameSession, source string) error {
	return s.sendSelectedCurrentContainerListsWithRefresh(session, source, true)
}

func (s *Service) sendSelectedCurrentContainerListsWithRefresh(session *gameSession, source string, refresh bool) error {
	if session == nil {
		return nil
	}
	if session.selectedItemListRefreshSent && !refresh {
		s.logPacketEvent("game-upper-current-item-list-refresh-skipped",
			"conn_id", session.connID,
			"channel_id", session.channel.ID,
			"source", source,
			"char_id", session.selectedCharacterID,
			"reason", "already_sent")
		return nil
	}
	if session.selectedCharacterID == 0 {
		s.logPacketEvent("game-upper-current-item-list-refresh-deferred",
			"conn_id", session.connID,
			"channel_id", session.channel.ID,
			"source", source,
			"reason", "character_not_selected")
		if refresh {
			return fmt.Errorf("current item-list refresh requires a selected character")
		}
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), currentRepositorySnapshotTimeout)
	defer cancel()

	type currentContainerListPlan struct {
		listType    byte
		body        []byte
		entrySource string
		entryCount  int
	}
	plan := make([]currentContainerListPlan, 0, len(currentSelectedItemListTypes))
	for _, listType := range currentSelectedItemListTypes {
		body, entrySource, entryCount, ok := s.buildCurrentItemListBodyForSession(ctx, session, listType)
		if !ok {
			if refresh {
				return fmt.Errorf(
					"current item-list refresh could not build required list %d for character %d",
					listType,
					session.selectedCharacterID,
				)
			}
			continue
		}
		plan = append(plan, currentContainerListPlan{
			listType:    listType,
			body:        body,
			entrySource: entrySource,
			entryCount:  entryCount,
		})
	}

	sent := 0
	for _, entry := range plan {
		s.logPacketEvent("game-upper-current-item-list-refresh-send",
			"conn_id", session.connID,
			"channel_id", session.channel.ID,
			"source", source,
			"char_id", session.selectedCharacterID,
			"msg_id", uint16(dnfenum.CmdPacketLeaveParty),
			"list_type", entry.listType,
			"entry_count", entry.entryCount,
			"body_len", len(entry.body),
			"body_source", entry.entrySource,
			"entry_size", currentItemListEntrySizeForType(entry.listType),
			"mcp_handler", "sub_1D72380")
		if err := s.sendCurrentSceneItemList(session, uint16(dnfenum.CmdPacketLeaveParty), entry.body); err != nil {
			return err
		}
		sent++
	}
	if sent > 0 {
		session.selectedItemListRefreshSent = true
		// Full op13 list refreshes replace the current client-side item objects.
		// Sorting, moving and scene handoff therefore need the persisted
		// sort-lock markers replayed only after every selected container exists.
		if err := s.sendCurrentSortLockReplayForSession(session, source+"_after_selected_container_refresh"); err != nil {
			return err
		}
		// The client's equipment-lock (op250/op251) model is keyed by
		// (listType, slot) and is never derived from an item row, so a full
		// container rebuild drops it exactly as it does at select-character
		// time. The select bootstrap pairs its op13 lists with an op251
		// snapshot itself (refresh=false path); every post-commit rebuild
		// (equip/unequip, sort, teleport potion) republishes the durable
		// snapshot here so an unequipped item keeps its lock icon.
		if refresh {
			if err := s.sendCurrentSelectedItemLockSnapshot(session, source+"_after_selected_container_refresh"); err != nil {
				return err
			}
		}
	}
	return nil
}

// sendSelectedPetInventoryListRefresh rebuilds only list 7. It is used after
// pet-panel consumables whose immediate list-7 op14 path is unstable in the
// legacy client, while still making the durable stack count visible now.
func (s *Service) sendSelectedPetInventoryListRefresh(session *gameSession, source string) error {
	if session == nil || session.selectedCharacterID == 0 {
		return fmt.Errorf("pet inventory refresh requires a selected character")
	}
	ctx, cancel := context.WithTimeout(context.Background(), currentRepositorySnapshotTimeout)
	defer cancel()
	body, entrySource, entryCount, ok := s.buildCurrentItemListBodyForSession(ctx, session, currentPetInventoryListType)
	if !ok {
		return fmt.Errorf("pet inventory refresh could not build list %d", currentPetInventoryListType)
	}
	s.logPacketEvent("game-upper-current-pet-inventory-refresh-send",
		"conn_id", session.connID,
		"channel_id", session.channel.ID,
		"source", source,
		"char_id", session.selectedCharacterID,
		"msg_id", uint16(dnfenum.CmdPacketLeaveParty),
		"classification", 0,
		"list_type", currentPetInventoryListType,
		"entry_count", entryCount,
		"body_len", len(body),
		"body_source", entrySource,
		"sequence", "durable_pet_transaction_then_class1_ack_then_current_exe_op13_list7")
	return s.sendCurrentSceneItemList(session, uint16(dnfenum.CmdPacketLeaveParty), body)
}

// sendSelectedCurrentContainerListRefresh rebuilds one already-open current
// client container without replaying unrelated list objects. Personal-cargo
// sort uses this path to preserve the committed list-2 ownership while the
// native warehouse sorter is reconciling its handles.
func (s *Service) sendSelectedCurrentContainerListRefresh(session *gameSession, source string, listType byte) error {
	if session == nil || session.selectedCharacterID == 0 {
		return fmt.Errorf("container list refresh requires a selected character")
	}
	ctx, cancel := context.WithTimeout(context.Background(), currentRepositorySnapshotTimeout)
	defer cancel()
	body, entrySource, entryCount, ok := s.buildCurrentItemListBodyForSession(ctx, session, listType)
	if !ok {
		return fmt.Errorf("container list refresh could not build list %d", listType)
	}
	s.logPacketEvent("game-upper-current-single-container-list-refresh-send",
		"conn_id", session.connID,
		"channel_id", session.channel.ID,
		"source", source,
		"char_id", session.selectedCharacterID,
		"msg_id", uint16(dnfenum.CmdPacketLeaveParty),
		"classification", 0,
		"list_type", listType,
		"entry_count", entryCount,
		"body_len", len(body),
		"body_source", entrySource,
		"sequence", "durable_sort_then_class1_ack_then_current_exe_op13_sorted_container_only")
	if err := s.sendCurrentSceneItemList(session, uint16(dnfenum.CmdPacketLeaveParty), body); err != nil {
		return err
	}
	if err := s.sendCurrentSortLockReplayForSession(session, source+"_after_single_container_refresh"); err != nil {
		return err
	}
	// Same slot-keyed lock model as the full rebuild above: the rebuilt
	// container loses its op251 state, so republish the durable snapshot.
	return s.sendCurrentSelectedItemLockSnapshot(session, source+"_after_single_container_refresh")
}

// sendCurrentAccountSharedInventoryTownUIReadyRefresh rebinds the account-owned
// crystal/soul rows after a return-to-character-select lifecycle. The current
// client keeps the account inventory singleton alive across op7, while the
// selected-character bootstrap legitimately resets its native containers with
// op391. A full list-0 snapshot then repopulates repository state but does not
// wake the surviving right-hand crystal/soul widgets. Replaying the occupied
// slots 354..365 through the native op14 update path after type1345/HUD-ready
// restores those widgets without rebuilding the character inventory or touching
// data.
//
// Only slots that currently hold an account stack are replayed. An absent slot
// would travel as a 0xFFFFFFFF delete row, and the client answers those by
// clearing the right-hand crystal/soul panel - the very widget this rebind
// exists to repaint (live report 2026-09-13: the panel looked emptied until the
// next login re-sent the bootstrap snapshot). Cells without a stack are already
// empty after the op391 container reset, so skipping them repaints the same
// widgets without the delete rows.
func (s *Service) sendCurrentAccountSharedInventoryTownUIReadyRefresh(
	session *gameSession,
	operation string,
) error {
	if session == nil {
		return nil
	}
	session.townMu.Lock()
	pending := session.accountSharedInventoryRebindPending
	characterID := session.selectedCharacterID
	session.townMu.Unlock()
	if !pending || characterID == 0 {
		return nil
	}

	occupied, err := s.currentAccountSharedWarehouseOccupiedSlots(session)
	if err != nil {
		return err
	}
	refreshes := make([]alignedcmd.ItemSlotRefresh, 0, len(occupied))
	for _, slot := range occupied {
		refreshes = append(refreshes, alignedcmd.ItemSlotRefresh{
			ListType:  dnfrepo.MainInventoryListType,
			SlotIndex: slot,
		})
	}
	if err := s.sendSelectedIncrementalItemSlotRefreshes(session, operation, refreshes); err != nil {
		return err
	}

	session.townMu.Lock()
	if session.selectedCharacterID == characterID {
		session.accountSharedInventoryRebindPending = false
	}
	session.townMu.Unlock()
	s.logGameEvent(session, "game-account-shared-inventory-town-ui-ready-refreshed",
		"source", operation,
		"char_id", characterID,
		"list_type", dnfrepo.MainInventoryListType,
		"first_slot", dnfrepo.CrystalWarehouseFirstSlot,
		"last_slot", dnfrepo.SoulWarehouseLastSlot,
		"entry_count", len(refreshes),
		"skipped_empty_slots",
		int(dnfrepo.SoulWarehouseLastSlot-dnfrepo.CrystalWarehouseFirstSlot)+1-len(refreshes),
		"reason", "return_select_preserved_account_singleton_after_container_reset")
	return nil
}

// currentAccountSharedWarehouseOccupiedSlots lists the crystal/soul warehouse
// slots that currently hold an account-owned stack. It uses the same slot key
// and occupancy rule as sendSelectedIncrementalItemSlotRefreshes so every slot it
// returns still produces the identical native 0x77-byte row.
func (s *Service) currentAccountSharedWarehouseOccupiedSlots(session *gameSession) ([]int16, error) {
	if session == nil || session.selectedCharacterID == 0 {
		return nil, nil
	}
	repos, ok := s.repositoryGroup()
	if !ok || repos.AccountInventory == nil {
		return nil, fmt.Errorf("account-shared warehouse refresh requires account inventory repository")
	}
	accountID := s.accountIDForSession(session)
	if accountID == "" {
		return nil, fmt.Errorf("account-shared warehouse refresh requires account id")
	}
	ctx, cancel := context.WithTimeout(context.Background(), currentRepositorySnapshotTimeout)
	defer cancel()
	account, _, err := repos.AccountInventory.Load(ctx, accountID)
	if err != nil {
		return nil, fmt.Errorf("account-shared warehouse refresh load account %s: %w", accountID, err)
	}
	slots := make([]int16, 0, int(dnfrepo.SoulWarehouseLastSlot-dnfrepo.CrystalWarehouseFirstSlot)+1)
	for slot := dnfrepo.CrystalWarehouseFirstSlot; slot <= dnfrepo.SoulWarehouseLastSlot; slot++ {
		stack, occupied := account.Slots[dnfrepo.AccountSharedInventorySlotKey(slot)]
		if !occupied || stack.ItemID <= 0 || stack.Count < 0 {
			continue
		}
		slots = append(slots, slot)
	}
	return slots, nil
}

// sendCurrentAccountSharedWarehouseRefresh republishes the account-owned
// crystal/soul rows that currently hold a stack. The current client rebuilds its
// right-hand warehouse panel after some inventory commands (live 2026-09-13: a
// bead enchant made the client re-query slots 354..359 through
// sub_2326860, get nothing, and blank the panel until the next login), so the
// bridge replays the occupied rows once more after such a command. Only slots
// with a stack are sent: an absent slot must never travel as a 0xFFFFFFFF delete
// row, which is what cleared the same panel earlier.
func (s *Service) sendCurrentAccountSharedWarehouseRefresh(session *gameSession, source string) error {
	if s == nil || session == nil || session.selectedCharacterID == 0 {
		return nil
	}
	occupied, err := s.currentAccountSharedWarehouseOccupiedSlots(session)
	if err != nil {
		return err
	}
	if len(occupied) == 0 {
		return nil
	}
	refreshes := make([]alignedcmd.ItemSlotRefresh, 0, len(occupied))
	for _, slot := range occupied {
		refreshes = append(refreshes, alignedcmd.ItemSlotRefresh{
			ListType:  dnfrepo.MainInventoryListType,
			SlotIndex: slot,
		})
	}
	return s.sendSelectedIncrementalItemSlotRefreshes(session, source, refreshes)
}

func (s *Service) sendSelectedIncrementalItemSlotRefreshes(
	session *gameSession,
	operation string,
	refreshes []alignedcmd.ItemSlotRefresh,
) error {
	if len(refreshes) == 0 {
		return nil
	}
	if session == nil || session.selectedCharacterID == 0 {
		return fmt.Errorf("aligned item-slot refresh requires a selected character")
	}
	repos, ok := s.repositoryGroup()
	if !ok || repos.Inventory == nil {
		return fmt.Errorf("aligned item-slot refresh requires inventory repository")
	}

	ctx, cancel := context.WithTimeout(context.Background(), currentRepositorySnapshotTimeout)
	defer cancel()
	characterID := strconv.Itoa(int(session.selectedCharacterID))
	inventory, found, err := repos.Inventory.Load(ctx, characterID)
	if err != nil {
		return fmt.Errorf("aligned item-slot refresh load character %s: %w", characterID, err)
	}
	if !found {
		return fmt.Errorf("aligned item-slot refresh inventory %s not found", characterID)
	}
	customAbilitySlots := make(map[int16]struct{})
	for _, refresh := range refreshes {
		if refresh.ListType == dnfrepo.MainInventoryListType && refresh.SlotIndex >= 0 &&
			!dnfrepo.IsAccountSharedInventorySlot(refresh.ListType, refresh.SlotIndex) {
			customAbilitySlots[refresh.SlotIndex] = struct{}{}
		}
	}
	if len(customAbilitySlots) != 0 {
		updated, initialized, initializeErr := s.initializeCurrentCustomAbilityIncrementalSlots(
			ctx,
			repos.Inventory,
			inventory,
			customAbilitySlots,
		)
		if initializeErr != nil {
			// The durable grant has already committed. Keep the ordinary item row
			// flowing even if an optional PVF compatibility projection is
			// unavailable; selected-character bootstrap will retry the repair.
			s.logPacketEvent("game-upper-custom-ability-incremental-initialize-skipped",
				"conn_id", session.connID,
				"operation", operation,
				"char_id", session.selectedCharacterID,
				"slot_count", len(customAbilitySlots),
				"reason", initializeErr)
		} else {
			inventory = updated
			if initialized != 0 {
				s.logPacketEvent("game-upper-custom-ability-incremental-initialized",
					"conn_id", session.connID,
					"operation", operation,
					"char_id", session.selectedCharacterID,
					"slot_count", len(customAbilitySlots),
					"initialized", initialized,
					"sequence", "durable_grant_then_custom_ability_tail_persist_then_same_op14_rows")
			}
		}
	}
	var account dnfrepo.AccountInventoryRecord
	accountLoaded := false
	loadAccount := func() error {
		if accountLoaded {
			return nil
		}
		if repos.AccountInventory == nil {
			return fmt.Errorf("aligned item-slot refresh requires account inventory repository")
		}
		accountID := s.accountIDForSession(session)
		if accountID == "" {
			return fmt.Errorf("aligned item-slot refresh requires account id for shared slot")
		}
		loaded, _, err := repos.AccountInventory.Load(ctx, accountID)
		if err != nil {
			return fmt.Errorf("aligned item-slot refresh load account %s: %w", accountID, err)
		}
		account = loaded
		accountLoaded = true
		return nil
	}

	entriesByList := make(map[byte][]currentItemListEntry)
	listOrder := make([]byte, 0, len(refreshes))
	seen := make(map[string]struct{}, len(refreshes))
	for _, refresh := range refreshes {
		if refresh.SlotIndex < 0 {
			return fmt.Errorf("aligned item-slot refresh has invalid slot %d", refresh.SlotIndex)
		}
		key := fmt.Sprintf("%d:%d", refresh.ListType, refresh.SlotIndex)
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		if _, knownList := entriesByList[refresh.ListType]; !knownList {
			listOrder = append(listOrder, refresh.ListType)
		}

		var stack dnfrepo.ItemStack
		var occupied bool
		if dnfrepo.IsAccountSharedInventorySlot(refresh.ListType, refresh.SlotIndex) {
			if err := loadAccount(); err != nil {
				return err
			}
			stack, occupied = account.Slots[key]
		} else {
			stack, occupied = inventory.Slots[key]
			if !occupied && refresh.ListType == 2 {
				stack, occupied = inventory.Warehouse[key]
			}
		}
		var entry currentItemListEntry
		switch {
		case occupied && stack.ItemID > 0 && stack.Count >= 0:
			entry = currentItemListEntryFromStack(refresh.ListType, refresh.SlotIndex, stack)
		case dnfrepo.IsAccountSharedInventorySlot(refresh.ListType, refresh.SlotIndex):
			// 账号共享晶体/灵魂槽清空时这一行要写 **itemID=0**（"空格"行），不是
			// 0xFFFFFFFF 删除行：拍卖行上架（sendCurrentAuctionInventorySlot）和摆摊售出
			// （private_store 的 sourceUpdate）都是用 (0,0) 清晶体仓库那一格，而现场
			// 2026-09-17 运营反馈正是"卖出/上架/交易都能正确减少，只有拖进仓库不行"——
			// 拖这条路径当时发的是 0xFFFFFFFF。右侧晶体/灵魂面板认 (0,0)，不认 0xFFFFFFFF。
			entry.patchCore(refresh.SlotIndex, 0, 0)
		default:
			entry.patchCore(refresh.SlotIndex, math.MaxUint32, 0)
		}
		entriesByList[refresh.ListType] = append(entriesByList[refresh.ListType], entry)
	}

	for _, listType := range listOrder {
		entries := entriesByList[listType]
		if len(entries) == 0 {
			continue
		}
		body := buildCurrentItemUpdateBody(listType, entries)
		s.logPacketEvent("game-aligned-command-item-slot-refresh-send",
			"conn_id", session.connID,
			"operation", operation,
			"char_id", session.selectedCharacterID,
			"msg_id", uint16(dnfenum.CmdPacketWalkoutPartyMember),
			"classification", 0,
			"list_type", listType,
			"entry_count", len(entries),
			"body_len", len(body),
			"sequence", "durable_owner_then_class1_ack_then_repository_backed_op14_changed_rows")
		if err := s.sendGameUpperRawClass(session, uint16(dnfenum.CmdPacketWalkoutPartyMember), body, 0); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) sendSelectedIncrementalEquipmentSlotRefreshes(
	session *gameSession,
	operation string,
	slots []int16,
) error {
	if len(slots) == 0 {
		return nil
	}
	if session == nil || session.selectedCharacterID == 0 {
		return fmt.Errorf("aligned equipment-slot refresh requires a selected character")
	}
	repos, ok := s.repositoryGroup()
	if !ok || repos.Equipment == nil {
		return fmt.Errorf("aligned equipment-slot refresh requires equipment repository")
	}

	ctx, cancel := context.WithTimeout(context.Background(), currentRepositorySnapshotTimeout)
	defer cancel()
	characterID := strconv.Itoa(int(session.selectedCharacterID))
	equipment, found, err := repos.Equipment.Load(ctx, characterID)
	if err != nil {
		return fmt.Errorf("aligned equipment-slot refresh load character %s: %w", characterID, err)
	}
	if !found {
		return fmt.Errorf("aligned equipment-slot refresh equipment %s not found", characterID)
	}

	seen := make(map[int16]struct{}, len(slots))
	for _, slot := range slots {
		if slot < 0 {
			return fmt.Errorf("aligned equipment-slot refresh has invalid slot %d", slot)
		}
		if _, duplicate := seen[slot]; duplicate {
			continue
		}
		seen[slot] = struct{}{}
		equipped, occupied := equipment.Entries[strconv.FormatInt(int64(slot), 10)]
		if !occupied || equipped.SlotIndex != slot || equipped.ItemID <= 0 {
			return fmt.Errorf("aligned equipment-slot refresh character %s slot %d not found", characterID, slot)
		}
		entry, projectable := currentItemListEntryFromEquipment(equipped)
		if !projectable {
			return fmt.Errorf("aligned equipment-slot refresh character %s slot %d is not projectable", characterID, slot)
		}
		// Keep the repaired worn-slot identity even for historical rows whose
		// bootstrap projection still uses legacy numbering.
		entry.setUint16(0, uint16(slot))
		body := buildCurrentItemUpdateBody(currentSocketListEquipment, []currentItemListEntry{entry})
		s.logPacketEvent("game-aligned-command-equipment-slot-refresh-send",
			"conn_id", session.connID,
			"operation", operation,
			"char_id", session.selectedCharacterID,
			"msg_id", uint16(dnfenum.CmdPacketWalkoutPartyMember),
			"classification", 0,
			"list_type", currentSocketListEquipment,
			"entry_count", 1,
			"entry_size", currentItemListEntryWireSize,
			"slot", slot,
			"body_len", len(body),
			"sequence", "durable_owner_then_class1_ack_then_op14_list3_single_changed_row")
		if err := s.sendGameUpperRawClass(session, uint16(dnfenum.CmdPacketWalkoutPartyMember), body, 0); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) sendSelectedEmptyItemSlotRefreshes(
	session *gameSession,
	operation string,
	refreshes []alignedcmd.ItemSlotRefresh,
) error {
	if len(refreshes) == 0 {
		return nil
	}
	if session == nil || session.selectedCharacterID == 0 {
		return fmt.Errorf("aligned empty item-slot refresh requires a selected character")
	}
	entriesByList := make(map[byte][]currentItemListEntry)
	listOrder := make([]byte, 0, len(refreshes))
	seen := make(map[string]struct{}, len(refreshes))
	for _, refresh := range refreshes {
		if refresh.SlotIndex < 0 {
			return fmt.Errorf("aligned empty item-slot refresh has invalid slot %d", refresh.SlotIndex)
		}
		key := fmt.Sprintf("%d:%d", refresh.ListType, refresh.SlotIndex)
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		if _, known := entriesByList[refresh.ListType]; !known {
			listOrder = append(listOrder, refresh.ListType)
		}
		var entry currentItemListEntry
		entry.patchCore(refresh.SlotIndex, math.MaxUint32, 0)
		entriesByList[refresh.ListType] = append(entriesByList[refresh.ListType], entry)
	}
	for _, listType := range listOrder {
		entries := entriesByList[listType]
		if len(entries) == 0 {
			continue
		}
		body := buildCurrentItemUpdateBody(listType, entries)
		s.logPacketEvent("game-aligned-command-item-slot-clear-send",
			"conn_id", session.connID,
			"operation", operation,
			"char_id", session.selectedCharacterID,
			"msg_id", uint16(dnfenum.CmdPacketWalkoutPartyMember),
			"classification", 0,
			"list_type", listType,
			"entry_count", len(entries),
			"body_len", len(body),
			"sequence", "class1_ack_then_empty_op14_invalidation_then_authoritative_op14_rows")
		if err := s.sendGameUpperRawClass(session, uint16(dnfenum.CmdPacketWalkoutPartyMember), body, 0); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) sendSelectedEquipmentItemUpdate(session *gameSession, source string) error {
	return s.sendSelectedEquipmentItemUpdateWithRefresh(session, source, false)
}

func (s *Service) sendSelectedEquipmentItemUpdateWithRefresh(session *gameSession, source string, refresh bool) error {
	if session == nil {
		return nil
	}
	if session.selectedEquipmentUpdateSent && !refresh {
		s.logPacketEvent("game-upper-current-equipment-item-update-skipped",
			"conn_id", session.connID,
			"channel_id", session.channel.ID,
			"source", source,
			"char_id", session.selectedCharacterID,
			"reason", "already_sent")
		return nil
	}
	if session.selectedCharacterID == 0 {
		s.logPacketEvent("game-upper-current-equipment-item-update-deferred",
			"conn_id", session.connID,
			"channel_id", session.channel.ID,
			"source", source,
			"reason", "character_not_selected")
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), currentRepositorySnapshotTimeout)
	defer cancel()

	body, entrySource, entryCount, ok := s.buildCurrentEquippedItemListBodyForSession(ctx, session)
	if !ok {
		return nil
	}
	if entryCount == 0 {
		s.logPacketEvent("game-upper-current-equipment-item-update-skipped",
			"conn_id", session.connID,
			"channel_id", session.channel.ID,
			"source", source,
			"char_id", session.selectedCharacterID,
			"msg_id", uint16(dnfenum.CmdPacketWalkoutPartyMember),
			"list_type", 3,
			"entry_count", entryCount,
			"body_len", len(body),
			"body_source", entrySource,
			"reason", "no_equipped_entries")
		session.selectedEquipmentUpdateSent = true
		return nil
	}

	s.logPacketEvent("game-upper-current-equipment-item-update-send",
		"conn_id", session.connID,
		"channel_id", session.channel.ID,
		"source", source,
		"char_id", session.selectedCharacterID,
		"msg_id", uint16(dnfenum.CmdPacketWalkoutPartyMember),
		"list_type", 3,
		"entry_count", entryCount,
		"body_len", len(body),
		"body_source", entrySource,
		"entry_size", currentItemListEntryWireSize,
		"evidence", "current_0x77_rows_restored_after_0x54_update_crashed")
	if err := s.sendGameUpperRawClass(session, uint16(dnfenum.CmdPacketWalkoutPartyMember), body, 0); err != nil {
		return err
	}
	session.selectedEquipmentUpdateSent = true
	return nil
}

func (s *Service) sendSelectedCurrentEquipmentSlotItemUpdates(session *gameSession, source string) error {
	if session == nil {
		return nil
	}
	if session.selectedCharacterID == 0 {
		s.logPacketEvent("game-upper-current-equipment-slot-items-refresh-deferred",
			"conn_id", session.connID,
			"channel_id", session.channel.ID,
			"source", source,
			"reason", "character_not_selected")
		return nil
	}
	repos, ok := s.repositoryGroup()
	if !ok || repos.Equipment == nil {
		s.logPacketEvent("game-upper-current-equipment-slot-items-refresh-skipped",
			"conn_id", session.connID,
			"channel_id", session.channel.ID,
			"source", source,
			"char_id", session.selectedCharacterID,
			"reason", "equipment_repository_unavailable")
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), currentRepositorySnapshotTimeout)
	defer cancel()

	characterID := strconv.Itoa(int(session.selectedCharacterID))
	entries, found, err := currentItemListEntriesFromEquipment(ctx, repos.Equipment, characterID, repos.Pet)
	if err != nil {
		return err
	}
	if !found || len(entries) == 0 {
		s.logPacketEvent("game-upper-current-equipment-slot-items-refresh-skipped",
			"conn_id", session.connID,
			"channel_id", session.channel.ID,
			"source", source,
			"char_id", session.selectedCharacterID,
			"reason", "no_occupied_equipment_entries")
		return nil
	}
	patchedUsePeriods, usePeriodErr := s.applyCurrentPVFUsePeriodsToEntriesWithLoadedCatalog(ctx, entries)
	if usePeriodErr != nil {
		s.logPacketEvent("game-upper-current-equipment-slot-items-use-period-wire-projection-failed",
			"conn_id", session.connID,
			"char_id", session.selectedCharacterID,
			"list_type", currentSocketListEquipment,
			"err", usePeriodErr)
	}
	if patchedUsePeriods > 0 {
		s.logPacketEvent("game-upper-current-equipment-slot-items-use-period-wire-projection-applied",
			"conn_id", session.connID,
			"char_id", session.selectedCharacterID,
			"list_type", currentSocketListEquipment,
			"patched_rows", patchedUsePeriods)
	}
	body := buildCurrentItemUpdateBody(currentSocketListEquipment, entries)
	s.logPacketEvent("game-upper-current-equipment-slot-items-refresh-send",
		"conn_id", session.connID,
		"channel_id", session.channel.ID,
		"source", source,
		"char_id", session.selectedCharacterID,
		"msg_id", uint16(dnfenum.CmdPacketWalkoutPartyMember),
		"classification", 0,
		"list_type", currentSocketListEquipment,
		"entry_count", len(entries),
		"body_len", len(body),
		"entry_size", currentItemListEntryWireSize,
		"sequence", "op14_list3_occupied_equipment_item_rows")
	return s.sendGameUpperRawClass(session, uint16(dnfenum.CmdPacketWalkoutPartyMember), body, 0)
}
