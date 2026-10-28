package dnfbridge

import (
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	dnfcollectbox "longheng.io/server/internal/modules/dnf/collectbox"
	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

const (
	collectboxNotificationMsgID = uint16(1027)
	collectboxErrorCode         = byte(0x12)
	// The current PVF maps index 1 to SAO and index 5 to 兽人守护神. The
	// current client also sends op1021 body 05 when that tab is selected. Keep
	// the login/actor-generation default explicit so catalog ordering cannot
	// silently return the window and its native stat owner to SAO.
	defaultCollectboxIndex = byte(5)
)

func (s *Service) handleCollectboxCommand(session *gameSession, request gameplayRequest) error {
	if s == nil || session == nil {
		return nil
	}
	catalog, err := s.currentCollectboxCatalog()
	if err != nil {
		s.logGameEvent(session, "game-collectbox-rejected", "opcode", request.Opcode, "reason", err)
		return s.sendGameUpperFailure(session, request.Opcode, collectboxErrorCode)
	}
	ctx, cancel := context.WithTimeout(context.Background(), currentRepositorySnapshotTimeout)
	defer cancel()
	switch request.Opcode {
	case uint16(dnfenum.CmdPacketSelectCollectbox):
		return s.handleCollectboxSelect(ctx, session, catalog, request.Body)
	case uint16(dnfenum.CmdPacketAddCollectboxItem):
		return s.handleCollectboxInsert(ctx, session, catalog, request.Body)
	case uint16(dnfenum.CmdPacketRemoveCollectboxItem):
		return s.handleCollectboxRemove(ctx, session, catalog, request.Body)
	case uint16(dnfenum.CmdPacketExtendCollectboxExpiryDate):
		return s.handleCollectboxExtend(ctx, session, catalog, request.Body)
	default:
		return s.sendGameUpperFailure(session, request.Opcode, collectboxErrorCode)
	}
}

func (s *Service) handleCollectboxSelect(ctx context.Context, session *gameSession, catalog *currentCollectboxCatalog, body []byte) error {
	if len(body) != 1 {
		return s.sendGameUpperFailure(session, uint16(dnfenum.CmdPacketSelectCollectbox), collectboxErrorCode)
	}
	index := body[0]
	definition, ok := collectboxDefinition(catalog, index)
	if !ok || session.selectedCharacterID == 0 {
		return s.sendGameUpperFailure(session, uint16(dnfenum.CmdPacketSelectCollectbox), collectboxErrorCode)
	}
	if err := s.sendGameUpperSuccess(session, uint16(dnfenum.CmdPacketSelectCollectbox), []byte{index}); err != nil {
		return err
	}
	session.selectedCollectboxIndex = index
	session.selectedCollectboxCharacterID = session.selectedCharacterID
	// The client applies only the selected box's attributes, so the player's
	// choice has to outlive the session. A persistence failure must not break
	// the click: the live selection is already applied below.
	if err := s.persistCollectboxSelection(ctx, session, index); err != nil {
		s.logGameEvent(session, "game-collectbox-selection-persist-failed",
			"char_id", session.selectedCharacterID,
			"box_index", index,
			"reason", err)
	}
	return s.sendCollectboxState(ctx, session, definition, true)
}

func (s *Service) handleCollectboxInsert(ctx context.Context, session *gameSession, catalog *currentCollectboxCatalog, body []byte) error {
	if len(body) != 8 {
		return s.sendGameUpperFailure(session, uint16(dnfenum.CmdPacketAddCollectboxItem), collectboxErrorCode)
	}
	indexRaw := binary.LittleEndian.Uint16(body[0:2])
	if indexRaw == 0 || indexRaw > math.MaxUint8 || !s.collectboxSessionSelected(session, byte(indexRaw)) {
		return s.sendGameUpperFailure(session, uint16(dnfenum.CmdPacketAddCollectboxItem), collectboxErrorCode)
	}
	index := byte(indexRaw)
	definition, ok := collectboxDefinition(catalog, index)
	if !ok {
		return s.sendGameUpperFailure(session, uint16(dnfenum.CmdPacketAddCollectboxItem), collectboxErrorCode)
	}
	sourceSlot := binary.LittleEndian.Uint16(body[2:4])
	if sourceSlot > math.MaxInt16 {
		return s.sendGameUpperFailure(session, uint16(dnfenum.CmdPacketAddCollectboxItem), collectboxErrorCode)
	}
	itemID := binary.LittleEndian.Uint32(body[4:8])
	owner, err := s.collectboxOwner()
	if err == nil {
		definition.Slots = collectboxSingleItemSlots(definition.Slots)
		_, err = owner.Insert(ctx, dnfcollectbox.InsertCommand{
			CharacterID: strconv.FormatUint(uint64(session.selectedCharacterID), 10),
			Definition:  definition,
			SourceSlot:  int16(sourceSlot),
			ItemID:      itemID,
			UpdatedAt:   time.Now().UTC(),
		})
	}
	if err != nil {
		s.logGameEvent(session, "game-collectbox-insert-rejected", "box_index", index, "source_slot", sourceSlot, "item_id", itemID, "reason", err)
		return s.sendGameUpperFailure(session, uint16(dnfenum.CmdPacketAddCollectboxItem), collectboxErrorCode)
	}
	if err := s.sendSelectedIncrementalItemSlotRefreshes(session, "collectbox_insert", []alignedcmd.ItemSlotRefresh{{ListType: dnfrepo.MainInventoryListType, SlotIndex: int16(sourceSlot)}}); err != nil {
		return err
	}
	if err := s.sendGameUpperSuccess(session, uint16(dnfenum.CmdPacketAddCollectboxItem), nil); err != nil {
		return err
	}
	return s.sendCollectboxState(ctx, session, definition, true)
}

func (s *Service) handleCollectboxRemove(ctx context.Context, session *gameSession, catalog *currentCollectboxCatalog, body []byte) error {
	if len(body) != 5 || session.selectedCharacterID == 0 {
		return s.sendGameUpperFailure(session, uint16(dnfenum.CmdPacketRemoveCollectboxItem), collectboxErrorCode)
	}
	itemID := binary.LittleEndian.Uint32(body[1:5])
	definition, ok := collectboxDefinitionForItem(catalog, itemID)
	if !ok {
		return s.sendGameUpperFailure(session, uint16(dnfenum.CmdPacketRemoveCollectboxItem), collectboxErrorCode)
	}
	returnPlacement, err := s.currentCollectboxReturnPlacement(itemID)
	if err != nil {
		s.logGameEvent(session, "game-collectbox-remove-rejected", "box_index", definition.Index, "item_id", itemID, "reason", err)
		return s.sendGameUpperFailure(session, uint16(dnfenum.CmdPacketRemoveCollectboxItem), collectboxErrorCode)
	}
	owner, err := s.collectboxOwner()
	var result dnfcollectbox.RemoveResult
	if err == nil {
		definition.Slots = collectboxSingleItemSlots(definition.Slots)
		result, err = owner.Remove(ctx, dnfcollectbox.RemoveCommand{
			CharacterID:     strconv.FormatUint(uint64(session.selectedCharacterID), 10),
			Definition:      definition,
			ItemID:          itemID,
			ReturnPlacement: returnPlacement,
			UpdatedAt:       time.Now().UTC(),
		})
	}
	if err != nil {
		s.logGameEvent(session, "game-collectbox-remove-rejected", "box_index", definition.Index, "item_id", itemID, "reason", err)
		return s.sendGameUpperFailure(session, uint16(dnfenum.CmdPacketRemoveCollectboxItem), collectboxErrorCode)
	}
	if err := s.sendSelectedIncrementalItemSlotRefreshes(session, "collectbox_remove", []alignedcmd.ItemSlotRefresh{{ListType: dnfrepo.MainInventoryListType, SlotIndex: result.TargetSlot}}); err != nil {
		return err
	}
	if err := s.sendGameUpperSuccess(session, uint16(dnfenum.CmdPacketRemoveCollectboxItem), nil); err != nil {
		return err
	}
	return s.sendCollectboxState(ctx, session, definition, true)
}

// currentCollectboxReturnPlacement resolves the same active-PVF page that
// ordinary item grants use.  Collection-box entries are stackables; accepting
// equipment here would recreate the old bug of returning a jewel to 9..64.
func (s *Service) currentCollectboxReturnPlacement(itemID uint32) (dnfcollectbox.ReturnPlacement, error) {
	catalog, err := s.currentPVFItemCatalog()
	if err != nil {
		return dnfcollectbox.ReturnPlacement{}, err
	}
	definition, err := catalog.ResolveItem(itemID)
	if err != nil {
		return dnfcollectbox.ReturnPlacement{}, err
	}
	return collectboxReturnPlacementFromPVF(definition)
}

func collectboxReturnPlacementFromPVF(definition dungeonDropItemDefinition) (dnfcollectbox.ReturnPlacement, error) {
	if definition.ItemID == 0 || definition.Kind != dungeonDropItemStackable ||
		definition.SlotStart < 9 || definition.SlotEnd < definition.SlotStart ||
		definition.SlotEnd >= dnfrepo.CrystalWarehouseFirstSlot {
		return dnfcollectbox.ReturnPlacement{}, fmt.Errorf("collect box item=%d has unsupported active-PVF placement kind=%s page=%d..%d", definition.ItemID, definition.Kind, definition.SlotStart, definition.SlotEnd)
	}
	return dnfcollectbox.ReturnPlacement{
		SlotStart:  definition.SlotStart,
		SlotEnd:    definition.SlotEnd,
		StackLimit: definition.StackLimit,
	}, nil
}

func collectboxSingleItemSlots(slots []dnfcollectbox.Slot) []dnfcollectbox.Slot {
	copySlots := append([]dnfcollectbox.Slot(nil), slots...)
	for index := range copySlots {
		copySlots[index].Count = 1
	}
	return copySlots
}

func (s *Service) handleCollectboxExtend(ctx context.Context, session *gameSession, catalog *currentCollectboxCatalog, body []byte) error {
	if len(body) != 5 || !s.collectboxSessionSelected(session, body[0]) {
		return s.sendGameUpperFailure(session, uint16(dnfenum.CmdPacketExtendCollectboxExpiryDate), collectboxErrorCode)
	}
	definition, ok := collectboxDefinition(catalog, body[0])
	itemID := binary.LittleEndian.Uint32(body[1:5])
	if !ok || definition.ExtendItemID == 0 || itemID != definition.ExtendItemID {
		return s.sendGameUpperFailure(session, uint16(dnfenum.CmdPacketExtendCollectboxExpiryDate), collectboxErrorCode)
	}
	owner, err := s.collectboxOwner()
	var result dnfcollectbox.ExtendResult
	if err == nil {
		result, err = owner.Extend(ctx, dnfcollectbox.ExtendCommand{
			CharacterID: strconv.FormatUint(uint64(session.selectedCharacterID), 10),
			Definition:  definition,
			ItemID:      itemID,
			UpdatedAt:   time.Now().UTC(),
		})
	}
	if err != nil {
		s.logGameEvent(session, "game-collectbox-extend-rejected", "box_index", body[0], "item_id", itemID, "reason", err)
		return s.sendGameUpperFailure(session, uint16(dnfenum.CmdPacketExtendCollectboxExpiryDate), collectboxErrorCode)
	}
	if err := s.sendSelectedIncrementalItemSlotRefreshes(session, "collectbox_extend", []alignedcmd.ItemSlotRefresh{{ListType: dnfrepo.MainInventoryListType, SlotIndex: result.SourceSlot}}); err != nil {
		return err
	}
	if err := s.sendGameUpperSuccess(session, uint16(dnfenum.CmdPacketExtendCollectboxExpiryDate), nil); err != nil {
		return err
	}
	return s.sendCollectboxState(ctx, session, definition, true)
}

func (s *Service) collectboxOwner() (*dnfcollectbox.Owner, error) {
	repositories, ok := s.repositoryGroup()
	if !ok {
		return nil, fmt.Errorf("collect box repositories unavailable")
	}
	return dnfcollectbox.NewOwner(repositories)
}

func (s *Service) collectboxSessionSelected(session *gameSession, index byte) bool {
	return index != 0 && currentCollectboxSessionSelection(session) == index
}

// currentCollectboxSessionSelection returns the box this session selected for
// the character that is currently logged in. The pair is keyed by character ID
// and survives 返回选角, so a same-character re-entry restores the player's own
// box instead of the login default.
func currentCollectboxSessionSelection(session *gameSession) byte {
	if session == nil || session.selectedCharacterID == 0 ||
		session.selectedCollectboxCharacterID != session.selectedCharacterID {
		return 0
	}
	return session.selectedCollectboxIndex
}

func (s *Service) sendCollectboxState(ctx context.Context, session *gameSession, definition dnfcollectbox.Definition, refreshSelected bool) error {
	state, err := s.loadCollectboxSessionState(ctx, session)
	if err != nil {
		return err
	}
	return s.sendCollectboxStateForSnapshot(session, definition, state, refreshSelected)
}

// loadCollectboxSessionState resolves the authoritative box contents once for a
// caller that publishes several boxes in one pass.
func (s *Service) loadCollectboxSessionState(ctx context.Context, session *gameSession) (dnfcollectbox.State, error) {
	repositories, ok := s.repositoryGroup()
	if !ok || session == nil || session.selectedCharacterID == 0 {
		return dnfcollectbox.State{}, fmt.Errorf("collect box state repositories unavailable")
	}
	return loadCollectboxState(ctx, repositories, s.accountIDForSession(session), uint64(session.selectedCharacterID))
}

func (s *Service) sendCollectboxStateForSnapshot(session *gameSession, definition dnfcollectbox.Definition, state dnfcollectbox.State, refreshSelected bool) error {
	body := buildCollectboxStateBodyAt(definition, state, refreshSelected, time.Now().UTC())
	s.logPacketEvent("game-collectbox-state-send",
		"conn_id", session.connID,
		"char_id", session.selectedCharacterID,
		"box_index", definition.Index,
		"item_count", body[7],
		"refresh_selected", refreshSelected,
		"body_len", len(body))
	return s.sendGameUpperRawClass(session, collectboxNotificationMsgID, body, 0)
}

// collectboxPopulatedIndexes lists the boxes that currently hold collected
// items, in catalog order.
func collectboxPopulatedIndexes(catalog *currentCollectboxCatalog, state dnfcollectbox.State) []byte {
	if catalog == nil {
		return nil
	}
	populated := make([]byte, 0, len(catalog.indexes))
	for _, index := range catalog.indexes {
		if collectboxItemCount(catalog, state, index) > 0 {
			populated = append(populated, index)
		}
	}
	return populated
}

func collectboxItemCount(catalog *currentCollectboxCatalog, state dnfcollectbox.State, index byte) int {
	definition, ok := collectboxDefinition(catalog, index)
	if !ok {
		return 0
	}
	return len(dnfcollectbox.Snapshot(definition, state))
}

// currentCollectboxActiveIndex picks the box that the login/actor-generation
// publication selects and refreshes last. The native client applies the
// attributes of the *selected* box only, so the selection is player intent and
// must be restored, never derived: the session's own choice first, then the
// durable per-character choice, and only an account that has never picked a box
// falls back to the most populated box and finally the PVF default. Deriving
// the box from item counts is what silently moved a player who had picked
// 生肖之灵 (2 items) back onto SAO (15 items) on every re-entry
// (live 2026-09-27 15:2x).
func currentCollectboxActiveIndex(catalog *currentCollectboxCatalog, state dnfcollectbox.State, session *gameSession) byte {
	if catalog == nil || len(catalog.indexes) == 0 {
		return 0
	}
	characterID := ""
	if session != nil && session.selectedCharacterID != 0 {
		characterID = strconv.FormatUint(uint64(session.selectedCharacterID), 10)
	}
	for _, remembered := range []byte{
		currentCollectboxSessionSelection(session),
		state.SelectedBox(characterID),
	} {
		if remembered == 0 {
			continue
		}
		if _, ok := collectboxDefinition(catalog, remembered); ok {
			return remembered
		}
	}
	best, bestCount := byte(0), 0
	for _, index := range collectboxPopulatedIndexes(catalog, state) {
		if count := collectboxItemCount(catalog, state, index); count > bestCount {
			best, bestCount = index, count
		}
	}
	if bestCount > 0 {
		return best
	}
	if _, ok := collectboxDefinition(catalog, defaultCollectboxIndex); ok {
		return defaultCollectboxIndex
	}
	return catalog.indexes[0]
}

// persistCollectboxSelection records the player's box through the collection-box
// owner so the next login - including a client restart, a channel reconnect and
// a server restart - restores it instead of a default.
func (s *Service) persistCollectboxSelection(ctx context.Context, session *gameSession, index byte) error {
	if s == nil || session == nil || session.selectedCharacterID == 0 || index == 0 {
		return nil
	}
	owner, err := s.collectboxOwner()
	if err != nil {
		return err
	}
	return owner.Select(ctx, dnfcollectbox.SelectCommand{
		CharacterID: strconv.FormatUint(uint64(session.selectedCharacterID), 10),
		BoxIndex:    index,
		UpdatedAt:   time.Now().UTC(),
	})
}

func collectboxIndexList(indexes []byte) string {
	if len(indexes) == 0 {
		return ""
	}
	parts := make([]string, 0, len(indexes))
	for _, index := range indexes {
		parts = append(parts, strconv.Itoa(int(index)))
	}
	return strings.Join(parts, ",")
}

func buildCollectboxStateBody(definition dnfcollectbox.Definition, state dnfcollectbox.State) []byte {
	return buildCollectboxStateBodyAt(definition, state, true, time.Now().UTC())
}

func buildCollectboxStateBodyAt(definition dnfcollectbox.Definition, state dnfcollectbox.State, refreshSelected bool, now time.Time) []byte {
	items := dnfcollectbox.Snapshot(definition, state)
	body := make([]byte, 8+len(items)*4)
	body[0] = definition.Index
	if refreshSelected {
		body[1] = 1
	}
	expiration := state.Expiration[strconv.Itoa(int(definition.Index))]
	if expiration.IsZero() {
		expiration, _ = parseCollectboxExpiration(definition.BaseExpirationDate)
	}
	if expiration.IsZero() {
		expiration, _ = parseCollectboxExpiration(definition.MaxExpirationDate)
	}
	remaining, flags := collectboxExpiryTime(expiration, now)
	binary.LittleEndian.PutUint32(body[2:6], remaining)
	body[6] = flags
	body[7] = byte(len(items))
	for index, itemID := range items {
		binary.LittleEndian.PutUint32(body[8+index*4:12+index*4], itemID)
	}
	return body
}

func (s *Service) sendCollectboxInitializationStates(ctx context.Context, session *gameSession) error {
	return s.sendCollectboxInitializationStatesWithReplay(ctx, session, false)
}

// sendCollectboxSceneInitializationStates publishes the complete collection-
// box projection once for the current native actor generation. The early
// select-character seed can populate the expiry cache before an actor exists,
// but only this post-mode1 publication makes the native collection-box owner
// recompute its five combat-stat bonuses. Dungeon entry and town return reset
// the cursor because both routes create a replacement actor generation.
func (s *Service) sendCollectboxSceneInitializationStates(ctx context.Context, session *gameSession) error {
	if session != nil && session.collectboxSceneProjectionSent {
		return nil
	}
	if err := s.sendCollectboxInitializationStatesWithReplay(ctx, session, true); err != nil {
		return err
	}
	session.collectboxSceneProjectionSent = true
	return nil
}

func (s *Service) sendCollectboxInitializationStatesWithReplay(ctx context.Context, session *gameSession, sceneReady bool) error {
	if s == nil || session == nil || session.selectedCharacterID == 0 {
		return fmt.Errorf("collect box initialization requires a selected character")
	}
	if session.collectboxInitializationSent && !sceneReady {
		return nil
	}
	catalog, err := s.currentCollectboxCatalog()
	if err != nil {
		return err
	}
	state, err := s.loadCollectboxSessionState(ctx, session)
	if err != nil {
		return err
	}
	populated := collectboxPopulatedIndexes(catalog, state)
	selectedIndex := currentCollectboxActiveIndex(catalog, state, session)
	selectedDefinition, ok := collectboxDefinition(catalog, selectedIndex)
	if !ok {
		return fmt.Errorf("collect box definition %d unavailable", selectedIndex)
	}
	for _, index := range catalog.indexes {
		definition, ok := collectboxDefinition(catalog, index)
		if ok {
			// Initial states populate the client's cache without changing its
			// current selection. The explicit selection is sent below.
			if err := s.sendCollectboxStateForSnapshot(session, definition, state, false); err != nil {
				return err
			}
		}
	}
	// A populated box must never stay cache-only: the native owner recomputes
	// its five combat-stat bonuses from a refreshed box, and the client's own
	// op1021 select is the only other producer of one. Leaving the player's box
	// at refresh_selected=0 made its attributes vanish on every 小退/重上 until
	// the box was clicked again. These run before the selected refresh below so
	// the window and its stat owner still end on the selected box.
	for _, index := range populated {
		if index == selectedIndex {
			continue
		}
		if definition, ok := collectboxDefinition(catalog, index); ok {
			if err := s.sendCollectboxStateForSnapshot(session, definition, state, true); err != nil {
				return err
			}
		}
	}
	// The archived current-client collection-box trace requires the ordinary
	// class-1 op1021 success body [1, index] between cache population and the
	// selected op1027 refresh. op1027 alone refreshes the state table but does
	// not change the window's active tab, which leaves the client on SAO.
	if err := s.sendGameUpperSuccess(
		session,
		uint16(dnfenum.CmdPacketSelectCollectbox),
		[]byte{selectedIndex},
	); err != nil {
		return err
	}
	session.selectedCollectboxIndex = selectedIndex
	session.selectedCollectboxCharacterID = session.selectedCharacterID
	if err := s.sendCollectboxStateForSnapshot(session, selectedDefinition, state, true); err != nil {
		return err
	}
	session.collectboxInitializationSent = true
	s.logGameEvent(session, "game-collectbox-initialization-sent",
		"char_id", session.selectedCharacterID,
		"box_count", len(catalog.indexes),
		"selected_box_index", selectedIndex,
		"selected_box_item_count", collectboxItemCount(catalog, state, selectedIndex),
		"populated_box_indexes", collectboxIndexList(populated),
		"scene_ready_replay", sceneReady,
		"wire_contract", "class0_op1027_cache_then_populated_refresh_then_class1_op1021_selection_ack_then_selected_refresh")
	return nil
}

// replayCollectboxSelectedRefreshAfterMode1 republishes the selected box once
// per actor generation, immediately after the complete mode1 that binds the
// final town actor. The town routes publish the collection-box projection
// between mode0 and that mode1 because the select/return ACK boundary needs it
// there; the native collection-box stat owner only exists with the final actor,
// so without this replay the refreshed state that carries the combat-stat
// bonuses is discarded until the player clicks the box by hand.
func (s *Service) replayCollectboxSelectedRefreshAfterMode1(ctx context.Context, session *gameSession, source string) error {
	if s == nil || session == nil || session.selectedCharacterID == 0 || session.collectboxSelectionRefreshSent {
		return nil
	}
	catalog, err := s.currentCollectboxCatalog()
	if err != nil {
		return err
	}
	state, err := s.loadCollectboxSessionState(ctx, session)
	if err != nil {
		return err
	}
	selectedIndex := currentCollectboxActiveIndex(catalog, state, session)
	definition, ok := collectboxDefinition(catalog, selectedIndex)
	if !ok {
		return fmt.Errorf("collect box definition %d unavailable", selectedIndex)
	}
	if err := s.sendCollectboxStateForSnapshot(session, definition, state, true); err != nil {
		return err
	}
	session.selectedCollectboxIndex = selectedIndex
	session.selectedCollectboxCharacterID = session.selectedCharacterID
	session.collectboxSelectionRefreshSent = true
	s.logGameEvent(session, "game-collectbox-selection-refresh-after-mode1",
		"char_id", session.selectedCharacterID,
		"box_index", selectedIndex,
		"item_count", collectboxItemCount(catalog, state, selectedIndex),
		"source", source,
		"reason", "native_collection_box_stat_owner_is_created_with_the_final_actor")
	return nil
}

func collectboxExpiry(raw string, now time.Time) (uint32, byte) {
	raw = strings.Trim(strings.TrimSpace(raw), "`")
	if raw == "" {
		return 0, 1
	}
	expiry, ok := parseCollectboxExpiration(raw)
	if !ok {
		return 0, 1
	}
	return collectboxExpiryTime(expiry, now)
}

func parseCollectboxExpiration(raw string) (time.Time, bool) {
	raw = strings.Trim(strings.TrimSpace(raw), "`")
	for _, layout := range []string{"2006-01-02 15:04:05", "2006-01-02", "2006.01.02 15:04:05", "2006.01.02"} {
		parsed, err := time.ParseInLocation(layout, raw, time.Local)
		if err == nil {
			return parsed, true
		}
	}
	return time.Time{}, false
}

func collectboxExpiryTime(expiry, now time.Time) (uint32, byte) {
	if expiry.IsZero() {
		return 0, 1
	}
	seconds := expiry.Sub(now).Seconds()
	if seconds <= 0 {
		return 0, 0
	}
	if seconds >= math.MaxUint32 {
		return math.MaxUint32, 0
	}
	return uint32(seconds), 0
}
