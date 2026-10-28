package dnfbridge

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfdungeon "longheng.io/server/internal/modules/dnf/dungeon"
	dnfepicbook "longheng.io/server/internal/modules/dnf/epicbook"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

const (
	// Current 90CN NoPack.exe registers the Epic Book balance handler on wire
	// 1149. The enum label at this value is stale and must not select the wire.
	currentEpicBookInfoWireOpcode uint16 = 1149
	currentEpicBookMakeItemSize          = 4
	currentEpicBookErrorInventory byte   = 4
	currentEpicBookErrorCondition byte   = 22
)

var (
	errCurrentEpicBookUnavailable          = errors.New("dnf epic book is unavailable")
	errCurrentEpicBookRequestInvalid       = errors.New("dnf epic book request is invalid")
	errCurrentEpicBookCondition            = errors.New("dnf epic book crafting condition is not met")
	errCurrentEpicBookBalanceOverflow      = errors.New("dnf epic book balance overflow")
	errCurrentEpicBookInventoryUnavailable = errors.New("dnf epic book inventory is unavailable")
)

type currentEpicBookBalanceRow struct {
	FragmentID uint32
	Balance    uint32
}

type currentEpicBookCraftResult struct {
	FragmentID      uint32
	FragmentBalance uint32
	ItemUpdates     []currentItemListEntry
}

func buildCurrentEpicBookInfoBody(mode byte, rows []currentEpicBookBalanceRow) ([]byte, error) {
	if mode > 1 {
		return nil, fmt.Errorf("%w: sync mode=%d", errCurrentEpicBookRequestInvalid, mode)
	}
	normalized := make(map[uint32]uint32, len(rows))
	for _, row := range rows {
		if row.FragmentID == 0 {
			return nil, fmt.Errorf("%w: fragment id is zero", errCurrentEpicBookRequestInvalid)
		}
		normalized[row.FragmentID] = row.Balance
	}
	ids := make([]uint32, 0, len(normalized))
	for fragmentID := range normalized {
		ids = append(ids, fragmentID)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	if uint64(len(ids)) > uint64(math.MaxUint32) {
		return nil, errCurrentEpicBookBalanceOverflow
	}
	var writer packetWriter
	writer.writeByte(mode)
	writer.writeUint32(uint32(len(ids)))
	for _, fragmentID := range ids {
		writer.writeUint32(fragmentID)
		writer.writeUint32(normalized[fragmentID])
	}
	return writer.bytes(), nil
}

func decodeCurrentEpicBookMakeItemRequest(body []byte) (uint32, error) {
	if len(body) != currentEpicBookMakeItemSize {
		return 0, fmt.Errorf("%w: body_len=%d", errCurrentEpicBookRequestInvalid, len(body))
	}
	equipmentID := binary.LittleEndian.Uint32(body)
	if equipmentID == 0 {
		return 0, fmt.Errorf("%w: equipment id is zero", errCurrentEpicBookRequestInvalid)
	}
	return equipmentID, nil
}

func buildCurrentEpicBookMakeItemResult(success bool, errorCode byte) []byte {
	if success {
		return []byte{1}
	}
	return []byte{0, errorCode}
}

func (s *Service) currentEpicBookCatalog() (*dnfepicbook.Catalog, error) {
	if s == nil {
		return nil, errCurrentEpicBookUnavailable
	}
	s.epicBookMu.Lock()
	defer s.epicBookMu.Unlock()
	if s.epicBookCatalog != nil || s.epicBookCatalogLoadErr != nil {
		return s.epicBookCatalog, s.epicBookCatalogLoadErr
	}
	s.initialEquipmentMu.Lock()
	archive, err := s.initialEquipmentArchiveLocked()
	s.initialEquipmentMu.Unlock()
	if err == nil {
		var text string
		text, err = archive.ReadText(dnfepicbook.PieceInfoPath)
		if err == nil {
			s.epicBookCatalog, err = dnfepicbook.Parse(text)
		}
	}
	s.epicBookCatalogLoadErr = err
	return s.epicBookCatalog, s.epicBookCatalogLoadErr
}

func (s *Service) currentEpicBookCatalogIfLoaded() (*dnfepicbook.Catalog, bool, error) {
	if s == nil {
		return nil, false, errCurrentEpicBookUnavailable
	}
	s.epicBookMu.Lock()
	defer s.epicBookMu.Unlock()
	if s.epicBookCatalog != nil {
		return s.epicBookCatalog, true, nil
	}
	if s.epicBookCatalogLoadErr != nil {
		return nil, true, s.epicBookCatalogLoadErr
	}
	return nil, false, nil
}

func (s *Service) preloadEpicBookCatalog(ctx context.Context) error {
	if s == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	started := time.Now()
	catalog, err := s.currentEpicBookCatalog()
	if err != nil {
		return fmt.Errorf("preload current epic book catalog: %w", err)
	}
	s.logPacketEvent("dnf-epic-book-catalog-loaded", "entries", catalog.Len(), "elapsed_ms", time.Since(started).Milliseconds())
	return nil
}

func (s *Service) sendCurrentEpicBookSnapshot(session *gameSession, source string) error {
	if s == nil || session == nil || session.selectedCharacterID == 0 {
		return nil
	}
	if session.selectedEpicBookBootstrapCharacterID == session.selectedCharacterID {
		return nil
	}
	catalog, loaded, err := s.currentEpicBookCatalogIfLoaded()
	if err != nil {
		return err
	}
	if !loaded {
		return nil
	}
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.EpicBook == nil {
		return errCurrentEpicBookUnavailable
	}
	accountID := strings.TrimSpace(s.accountIDForSession(session))
	ctx, cancel := context.WithTimeout(context.Background(), currentRepositorySnapshotTimeout)
	defer cancel()
	record, found, err := repositories.EpicBook.Load(ctx, accountID)
	if err != nil {
		return err
	}
	rows := make([]currentEpicBookBalanceRow, 0)
	if found {
		for equipmentID, balance := range record.Balances {
			entry, mapped := catalog.EntryByEquipment(equipmentID)
			if !mapped || balance == 0 {
				continue
			}
			rows = append(rows, currentEpicBookBalanceRow{FragmentID: entry.FragmentID, Balance: balance})
		}
	}
	body, err := buildCurrentEpicBookInfoBody(0, rows)
	if err != nil {
		return err
	}
	if err := s.sendGameUpperRawClass(session, currentEpicBookInfoWireOpcode, body, 0); err != nil {
		return err
	}
	session.selectedEpicBookBootstrapCharacterID = session.selectedCharacterID
	s.logGameEvent(session, "game-epic-book-snapshot-sent", "source", source, "account_id", accountID, "entries", len(rows), "wire_opcode", currentEpicBookInfoWireOpcode)
	return nil
}

func (s *Service) handleCurrentEpicBookMakeItem(session *gameSession, body []byte) error {
	equipmentID, err := decodeCurrentEpicBookMakeItemRequest(body)
	if err != nil {
		s.logGameEvent(session, "game-epic-book-make-item-blocked", "body_len", len(body), "reason", "current_exe_op788_request_malformed", "error", err)
		return s.sendGameUpperRawClass(session, uint16(dnfenum.CmdPacketEpicBookMakeItem), buildCurrentEpicBookMakeItemResult(false, 0), 1)
	}
	result, err := s.commitCurrentEpicBookMakeItem(context.Background(), session, equipmentID, time.Now().UTC())
	if err != nil {
		errorCode := currentEpicBookErrorCondition
		if errors.Is(err, errDungeonPickupInventoryFull) || errors.Is(err, dnfdungeon.ErrPickupInventoryFull) {
			errorCode = currentEpicBookErrorInventory
		}
		s.logGameEvent(session, "game-epic-book-make-item-rejected", "equipment_id", equipmentID, "error_code", errorCode, "error", err)
		return s.sendGameUpperRawClass(session, uint16(dnfenum.CmdPacketEpicBookMakeItem), buildCurrentEpicBookMakeItemResult(false, errorCode), 1)
	}
	if err := s.sendGameUpperRawClass(session, uint16(dnfenum.CmdPacketEpicBookMakeItem), buildCurrentEpicBookMakeItemResult(true, 0), 1); err != nil {
		return err
	}
	if len(result.ItemUpdates) > 0 {
		if err := s.sendGameUpperRawClass(session, uint16(dnfenum.CmdPacketWalkoutPartyMember), buildCurrentItemUpdateBody(dnfrepo.MainInventoryListType, result.ItemUpdates), 0); err != nil {
			return err
		}
	}
	syncBody, err := buildCurrentEpicBookInfoBody(1, []currentEpicBookBalanceRow{{FragmentID: result.FragmentID, Balance: result.FragmentBalance}})
	if err != nil {
		return err
	}
	return s.sendGameUpperRawClass(session, currentEpicBookInfoWireOpcode, syncBody, 0)
}

func (s *Service) commitCurrentEpicBookMakeItem(ctx context.Context, session *gameSession, equipmentID uint32, now time.Time) (currentEpicBookCraftResult, error) {
	if s == nil || session == nil || session.selectedCharacterID == 0 {
		return currentEpicBookCraftResult{}, errCurrentEpicBookUnavailable
	}
	catalog, err := s.currentEpicBookCatalog()
	if err != nil {
		return currentEpicBookCraftResult{}, err
	}
	entry, ok := catalog.EntryByEquipment(equipmentID)
	if !ok {
		return currentEpicBookCraftResult{}, fmt.Errorf("%w: equipment=%d recipe missing", errCurrentEpicBookCondition, equipmentID)
	}
	items, err := s.currentPVFItemCatalog()
	if err != nil {
		return currentEpicBookCraftResult{}, err
	}
	outputDefinition, err := items.ResolveItem(equipmentID)
	if err != nil || outputDefinition.Kind != dungeonDropItemEquipment || outputDefinition.Rarity != 4 {
		return currentEpicBookCraftResult{}, fmt.Errorf("%w: equipment=%d is not PVF epic: %v", errCurrentEpicBookCondition, equipmentID, err)
	}
	for _, material := range entry.Recipe.Materials {
		definition, resolveErr := items.ResolveItem(material.ItemID)
		if resolveErr != nil || definition.Kind != dungeonDropItemStackable {
			return currentEpicBookCraftResult{}, fmt.Errorf("%w: material=%d unresolved: %v", errCurrentEpicBookCondition, material.ItemID, resolveErr)
		}
	}
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.Character == nil || repositories.EpicBookCraft == nil {
		return currentEpicBookCraftResult{}, errCurrentEpicBookUnavailable
	}
	accountID := strings.TrimSpace(s.accountIDForSession(session))
	characterID := strconv.Itoa(int(session.selectedCharacterID))
	character, found, err := repositories.Character.Load(ctx, characterID)
	if err != nil || !found || strings.TrimSpace(character.AccountID) != accountID {
		return currentEpicBookCraftResult{}, fmt.Errorf("%w: character ownership", errCurrentEpicBookCondition)
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	result := currentEpicBookCraftResult{FragmentID: entry.FragmentID}
	err = repositories.EpicBookCraft.WithinEpicBookCraft(ctx, accountID, characterID, func(epicBooks dnfrepo.EpicBookRepository, accountInventories dnfrepo.AccountInventoryRepository, inventories dnfrepo.InventoryRepository, _ dnfrepo.EquipmentRepository) error {
		book, bookFound, loadErr := epicBooks.Load(ctx, accountID)
		if loadErr != nil {
			return loadErr
		}
		if !bookFound {
			book = dnfrepo.EpicBookRecord{AccountID: accountID}
		}
		if book.Balances[equipmentID] < entry.Recipe.FragmentCount {
			return fmt.Errorf("%w: fragment equipment=%d have=%d need=%d", errCurrentEpicBookCondition, equipmentID, book.Balances[equipmentID], entry.Recipe.FragmentCount)
		}
		accountInventory, accountInventoryFound, loadErr := accountInventories.Load(ctx, accountID)
		if loadErr != nil {
			return loadErr
		}
		if !accountInventoryFound {
			return errCurrentEpicBookInventoryUnavailable
		}
		accountInventory = dnfrepo.CloneAccountInventory(accountInventory)
		inventory, inventoryFound, loadErr := inventories.Load(ctx, characterID)
		if loadErr != nil {
			return loadErr
		}
		if !inventoryFound {
			return errCurrentEpicBookInventoryUnavailable
		}
		inventory = dnfrepo.CloneInventory(inventory)
		updates, consumeErr := consumeCurrentEpicBookMaterials(&accountInventory, entry.Recipe.Materials)
		if consumeErr != nil {
			return consumeErr
		}
		// 史诗图鉴合成的产物同样不能落进未解锁的背包扩展行。
		outputDefinition = limitCurrentMainInventoryDefinitionSlots(
			outputDefinition,
			s.currentInventoryPlacementMainSlotCount(ctx, &inventory, characterID),
		)
		slot, stack, placeErr := dnfdungeon.PlacePickupItem(&inventory, currentDungeonPickupPlacement(outputDefinition, 1))
		if placeErr != nil {
			return mapCurrentDungeonPickupOwnerError(placeErr)
		}
		outputUpdate := currentItemListEntryFromStack(dnfrepo.MainInventoryListType, int16(slot), stack)
		stack.RawEntry = append([]byte(nil), outputUpdate.data[:]...)
		inventory.Slots[currentDungeonPickupMainSlotKey(int16(slot))] = stack
		updates = append(updates, outputUpdate)

		book = dnfrepo.CloneEpicBook(book)
		book.Balances[equipmentID] -= entry.Recipe.FragmentCount
		result.FragmentBalance = book.Balances[equipmentID]
		book.UpdatedAt = now
		accountInventory.UpdatedAt = now
		inventory.UpdatedAt = now
		if err := epicBooks.Save(ctx, book); err != nil {
			return err
		}
		if err := accountInventories.Save(ctx, accountInventory); err != nil {
			return err
		}
		if err := dnfrepo.SaveInventoryFields(ctx, inventories, inventory, dnfrepo.InventoryFieldSlots); err != nil {
			return err
		}
		result.ItemUpdates = append([]currentItemListEntry(nil), updates...)
		return nil
	})
	if err != nil {
		return currentEpicBookCraftResult{}, err
	}
	return result, nil
}

func consumeCurrentEpicBookMaterials(inventory *dnfrepo.AccountInventoryRecord, materials []dnfepicbook.Material) ([]currentItemListEntry, error) {
	if inventory == nil {
		return nil, errCurrentEpicBookInventoryUnavailable
	}
	required := make(map[uint32]uint64, len(materials))
	for _, material := range materials {
		if material.ItemID == 0 || material.Count == 0 || required[material.ItemID] > math.MaxUint64-uint64(material.Count) {
			return nil, errCurrentEpicBookCondition
		}
		required[material.ItemID] += uint64(material.Count)
	}
	type candidate struct {
		key   string
		slot  int16
		stack dnfrepo.ItemStack
	}
	candidates := make(map[uint32][]candidate, len(required))
	available := make(map[uint32]uint64, len(required))
	for key, stack := range inventory.Slots {
		listType, slot, ok := parseSceneInventorySlotKey(key)
		if !ok || !dnfrepo.IsAccountSharedInventorySlot(listType, slot) || stack.Count <= 0 || currentNPCShopItemLocked(stack) || stack.ItemID <= 0 || stack.ItemID > math.MaxUint32 {
			continue
		}
		itemID := uint32(stack.ItemID)
		if _, needed := required[itemID]; !needed {
			continue
		}
		available[itemID] += uint64(stack.Count)
		candidates[itemID] = append(candidates[itemID], candidate{key: key, slot: slot, stack: stack})
	}
	for itemID, count := range required {
		if available[itemID] < count {
			return nil, fmt.Errorf("%w: material=%d have=%d need=%d", errCurrentEpicBookCondition, itemID, available[itemID], count)
		}
	}
	itemIDs := make([]uint32, 0, len(required))
	for itemID := range required {
		itemIDs = append(itemIDs, itemID)
	}
	sort.Slice(itemIDs, func(i, j int) bool { return itemIDs[i] < itemIDs[j] })
	updates := make([]currentItemListEntry, 0)
	for _, itemID := range itemIDs {
		sort.Slice(candidates[itemID], func(i, j int) bool { return candidates[itemID][i].slot < candidates[itemID][j].slot })
		remaining := required[itemID]
		for _, candidate := range candidates[itemID] {
			stack := inventory.Slots[candidate.key]
			consumed := min(remaining, uint64(stack.Count))
			stack.Count -= int64(consumed)
			remaining -= consumed
			if stack.Count == 0 {
				delete(inventory.Slots, candidate.key)
				var empty currentItemListEntry
				empty.patchCore(candidate.slot, 0, 0)
				updates = append(updates, empty)
			} else {
				update := currentItemListEntryFromStack(dnfrepo.MainInventoryListType, candidate.slot, stack)
				stack.RawEntry = append([]byte(nil), update.data[:]...)
				inventory.Slots[candidate.key] = stack
				updates = append(updates, update)
			}
			if remaining == 0 {
				break
			}
		}
	}
	return updates, nil
}
