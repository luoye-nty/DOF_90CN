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

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	dnfenum "longheng.io/server/internal/modules/dnf/dnfenum"
	dnfproto "longheng.io/server/internal/modules/dnf/protocol"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

const (
	currentCustomAbilitySetNotifyOpcode       uint16 = 1278
	currentCustomAbilitySetErrorGeneric       byte   = 1
	currentCustomAbilitySetErrorMaterial      byte   = 8
	currentCustomAbilitySetOptionLimit               = 4
	currentCustomAbilitySetNotificationSize          = 12
	currentCustomAbilitySetOptionsExtra              = "custom_ability_set_options"
	currentCustomAbilitySetRequiredPartsExtra        = "custom_ability_set_required_parts"
	currentCustomAbilitySetGradeExtra                = "custom_ability_set_grade"
	currentCustomAbilitySetPartIndexExtra            = "custom_ability_set_part_index"
)

var (
	errCurrentCustomAbilitySetInvalid  = errors.New("current custom ability set request is invalid")
	errCurrentCustomAbilitySetMaterial = errors.New("current custom ability set material is insufficient")
)

type currentCustomAbilitySetRequest struct {
	options []byte
	slots   []int16
}

type currentCustomAbilitySetSnapshot struct {
	recordID      byte
	requiredParts byte
	options       [5]byte
	grade         byte
	partSetIndex  byte
}

type currentCustomAbilitySetResult struct {
	snapshot          currentCustomAbilitySetSnapshot
	materialID        uint32
	materialCost      int64
	materialRemaining int64
	spentPoints       int
	refreshes         []alignedcmd.ItemSlotRefresh
	equipmentChanged  bool
}

func decodeCurrentCustomAbilitySetRequest(body []byte) (currentCustomAbilitySetRequest, error) {
	if len(body) < 1 {
		return currentCustomAbilitySetRequest{}, errCurrentCustomAbilitySetInvalid
	}
	offset := 0
	optionCount := int(body[offset])
	offset++
	if optionCount < 1 || optionCount > currentCustomAbilitySetOptionLimit || len(body) < offset+optionCount+1 {
		return currentCustomAbilitySetRequest{}, errCurrentCustomAbilitySetInvalid
	}
	request := currentCustomAbilitySetRequest{options: append([]byte(nil), body[offset:offset+optionCount]...)}
	offset += optionCount
	equipmentCount := int(body[offset])
	offset++
	if equipmentCount < 3 || equipmentCount > 6 || len(body) != offset+equipmentCount*2 {
		return currentCustomAbilitySetRequest{}, errCurrentCustomAbilitySetInvalid
	}
	seenOptions := make(map[byte]struct{}, optionCount)
	for _, optionID := range request.options {
		if optionID == 0 {
			return currentCustomAbilitySetRequest{}, errCurrentCustomAbilitySetInvalid
		}
		if _, duplicate := seenOptions[optionID]; duplicate {
			return currentCustomAbilitySetRequest{}, errCurrentCustomAbilitySetInvalid
		}
		seenOptions[optionID] = struct{}{}
	}
	seenSlots := make(map[int16]struct{}, equipmentCount)
	request.slots = make([]int16, 0, equipmentCount)
	for index := 0; index < equipmentCount; index++ {
		rawSlot := binary.LittleEndian.Uint16(body[offset : offset+2])
		offset += 2
		if rawSlot > math.MaxInt16 {
			return currentCustomAbilitySetRequest{}, errCurrentCustomAbilitySetInvalid
		}
		slot := int16(rawSlot)
		if _, duplicate := seenSlots[slot]; duplicate {
			return currentCustomAbilitySetRequest{}, errCurrentCustomAbilitySetInvalid
		}
		seenSlots[slot] = struct{}{}
		request.slots = append(request.slots, slot)
	}
	return request, nil
}

func buildCurrentCustomAbilitySetFailureBody(err error) []byte {
	code := currentCustomAbilitySetErrorGeneric
	if errors.Is(err, errCurrentCustomAbilitySetMaterial) {
		code = currentCustomAbilitySetErrorMaterial
	}
	return []byte{0, code}
}

func buildCurrentCustomAbilitySetSuccessBody() []byte { return []byte{1} }

func buildCurrentCustomAbilitySetNotificationBody(actorID uint32, snapshot currentCustomAbilitySetSnapshot) []byte {
	body := make([]byte, currentCustomAbilitySetNotificationSize)
	binary.LittleEndian.PutUint32(body[0:4], actorID)
	body[4] = snapshot.recordID
	body[5] = snapshot.requiredParts
	copy(body[6:11], snapshot.options[:])
	body[11] = snapshot.grade
	return body
}

func currentCustomAbilitySetOptionsText(options []byte) string {
	values := make([]string, 0, len(options))
	for _, optionID := range options {
		if optionID == 0 {
			break
		}
		values = append(values, strconv.Itoa(int(optionID)))
	}
	return strings.Join(values, ",")
}

func cloneCurrentCustomAbilityExtra(extra map[string]string) map[string]string {
	clone := make(map[string]string, len(extra)+5)
	for key, value := range extra {
		clone[key] = value
	}
	return clone
}

func applyCurrentCustomAbilitySetMembership(raw []byte, extra map[string]string, recordID byte, snapshot *currentCustomAbilitySetSnapshot) ([]byte, map[string]string, error) {
	if len(raw) != currentItemListEntryWireSize {
		return nil, nil, errCurrentCustomAbilitySetInvalid
	}
	raw = append([]byte(nil), raw...)
	raw[currentCustomAbilitySetRecordOffset] = recordID
	extra = cloneCurrentCustomAbilityExtra(extra)
	if recordID == 0 || snapshot == nil {
		delete(extra, currentCustomAbilitySetOptionsExtra)
		delete(extra, currentCustomAbilitySetRequiredPartsExtra)
		delete(extra, currentCustomAbilitySetGradeExtra)
		delete(extra, currentCustomAbilitySetPartIndexExtra)
	} else {
		extra[currentCustomAbilitySetOptionsExtra] = currentCustomAbilitySetOptionsText(snapshot.options[:])
		extra[currentCustomAbilitySetRequiredPartsExtra] = strconv.Itoa(int(snapshot.requiredParts))
		extra[currentCustomAbilitySetGradeExtra] = strconv.Itoa(int(snapshot.grade))
		extra[currentCustomAbilitySetPartIndexExtra] = strconv.Itoa(int(snapshot.partSetIndex))
	}
	extra["custom_ability_set_record"] = strconv.Itoa(int(recordID))
	tailHex := fmt.Sprintf("%x", raw[currentCustomAbilityRawBlockStart:currentCustomAbilityRawBlockEnd])
	extra["tail_data_72"] = tailHex
	for _, key := range []string{"mode1_raw_e_hex", "mode1_state_188_192_raw_hex"} {
		if _, exists := extra[key]; exists {
			extra[key] = tailHex
		}
	}
	return raw, extra, nil
}

func validateCurrentCustomAbilitySetProfiles(profiles []currentCustomAbilityItemProfile) error {
	occupied := make(map[int16]struct{}, len(profiles))
	armorProfiles := make([]currentCustomAbilityItemProfile, 0, 5)
	for _, profile := range profiles {
		if !profile.applicable || profile.optionCount != 2 || profile.equipmentSlot < 0 {
			return errCurrentCustomAbilitySetInvalid
		}
		if _, duplicate := occupied[profile.equipmentSlot]; duplicate {
			return errCurrentCustomAbilitySetInvalid
		}
		occupied[profile.equipmentSlot] = struct{}{}
		if profile.equipmentSlot >= 14 && profile.equipmentSlot <= 18 {
			armorProfiles = append(armorProfiles, profile)
		}
	}
	if len(armorProfiles) == 5 {
		material := armorProfiles[0].subType
		if !armorProfiles[0].hasSubType {
			return errCurrentCustomAbilitySetInvalid
		}
		for _, profile := range armorProfiles[1:] {
			if !profile.hasSubType || profile.subType != material {
				return errCurrentCustomAbilitySetInvalid
			}
		}
	}
	return nil
}

func commitCurrentCustomAbilitySet(ctx context.Context, repositories dnfrepo.Group, characterID string, request currentCustomAbilitySetRequest, individualCatalog *currentCustomAbilityCatalog, catalog *currentCustomAbilitySetCatalog, resolve currentCustomAbilityProfileResolver) (currentCustomAbilitySetResult, error) {
	if repositories.CharacterItems == nil || individualCatalog == nil || catalog == nil || resolve == nil || len(request.options) < 1 || len(request.options) > int(catalog.maxOptions) || len(request.slots) < int(catalog.minParts) || len(request.slots) > int(catalog.maxParts) {
		return currentCustomAbilitySetResult{}, errCurrentCustomAbilitySetInvalid
	}

	var result currentCustomAbilitySetResult
	err := repositories.CharacterItems.WithinCharacterItems(ctx, characterID, func(inventoryRepo dnfrepo.InventoryRepository, equipmentRepo dnfrepo.EquipmentRepository) error {
		inventory, found, err := inventoryRepo.Load(ctx, characterID)
		if err != nil {
			return err
		}
		if !found {
			return errCurrentCustomAbilitySetInvalid
		}
		inventory = dnfrepo.CloneInventory(inventory)
		equipment, equipmentFound, err := equipmentRepo.Load(ctx, characterID)
		if err != nil {
			return err
		}
		if !equipmentFound {
			equipment = dnfrepo.EquipmentRecord{CharacterID: characterID}
		} else {
			equipment = dnfrepo.CloneEquipment(equipment)
		}

		profiles := make([]currentCustomAbilityItemProfile, 0, len(request.slots))
		grades := make([]byte, 0, len(request.slots))
		selectedKeys := make(map[string]struct{}, len(request.slots))
		for _, slot := range request.slots {
			key := currentCeraShopInventorySlotKey(dnfrepo.MainInventoryListType, slot)
			stack, found := inventory.Slots[key]
			if !found || stack.ItemID <= 0 || stack.ItemID > math.MaxUint32 || stack.Count != 1 || len(stack.RawEntry) != currentItemListEntryWireSize {
				return errCurrentCustomAbilitySetInvalid
			}
			profile, err := resolve(uint32(stack.ItemID))
			if err != nil {
				return err
			}
			if !profile.applicable || profile.partSetIndex != catalog.partSetIndex {
				return errCurrentCustomAbilitySetInvalid
			}
			state, valid := currentCustomAbilityStateFromRaw(stack.RawEntry, individualCatalog)
			if !valid {
				return errCurrentCustomAbilitySetInvalid
			}
			profiles = append(profiles, profile)
			grades = append(grades, state.grade)
			selectedKeys[key] = struct{}{}
		}
		if err := validateCurrentCustomAbilitySetProfiles(profiles); err != nil {
			return err
		}
		setGrade, spentPoints, materialCost, err := catalog.evaluate(request.options, grades)
		if err != nil {
			return err
		}

		materialSlots := currentCompoundSlotsForItem(inventory.Slots, int64(catalog.materialID))
		available := int64(0)
		for _, slot := range materialSlots {
			available += inventory.Slots[currentCeraShopInventorySlotKey(dnfrepo.MainInventoryListType, slot)].Count
		}
		if available < materialCost {
			return errCurrentCustomAbilitySetMaterial
		}

		var snapshot currentCustomAbilitySetSnapshot
		snapshot.recordID = catalog.partSetIndex
		snapshot.partSetIndex = catalog.partSetIndex
		snapshot.requiredParts = byte(len(request.slots))
		snapshot.grade = setGrade
		copy(snapshot.options[:], request.options)

		refreshSet := make(map[int16]struct{})
		equipmentChanged := false
		for key, stack := range inventory.Slots {
			if stack.Count != 1 || stack.ItemID <= 0 || stack.ItemID > math.MaxUint32 || len(stack.RawEntry) != currentItemListEntryWireSize || stack.RawEntry[currentCustomAbilitySetRecordOffset] != catalog.partSetIndex {
				continue
			}
			listType, slot, ok := parseSceneInventorySlotKey(key)
			if !ok || listType != dnfrepo.MainInventoryListType {
				continue
			}
			profile, profileErr := resolve(uint32(stack.ItemID))
			if profileErr != nil {
				return profileErr
			}
			if !profile.applicable || profile.partSetIndex != catalog.partSetIndex {
				continue
			}
			stack.RawEntry, stack.Extra, err = applyCurrentCustomAbilitySetMembership(stack.RawEntry, stack.Extra, 0, nil)
			if err != nil {
				return err
			}
			inventory.Slots[key] = stack
			refreshSet[slot] = struct{}{}
		}
		for key, equipped := range equipment.Entries {
			if equipped.ItemID <= 0 || equipped.ItemID > math.MaxUint32 || len(equipped.RawEntry) != currentItemListEntryWireSize || equipped.RawEntry[currentCustomAbilitySetRecordOffset] != catalog.partSetIndex {
				continue
			}
			profile, profileErr := resolve(uint32(equipped.ItemID))
			if profileErr != nil {
				return profileErr
			}
			if !profile.applicable || profile.partSetIndex != catalog.partSetIndex {
				continue
			}
			equipped.RawEntry, equipped.Extra, err = applyCurrentCustomAbilitySetMembership(equipped.RawEntry, equipped.Extra, 0, nil)
			if err != nil {
				return err
			}
			equipment.Entries[key] = equipped
			equipmentChanged = true
		}

		for key := range selectedKeys {
			stack := inventory.Slots[key]
			stack.RawEntry, stack.Extra, err = applyCurrentCustomAbilitySetMembership(stack.RawEntry, stack.Extra, catalog.partSetIndex, &snapshot)
			if err != nil {
				return err
			}
			inventory.Slots[key] = stack
			_, slot, _ := parseSceneInventorySlotKey(key)
			refreshSet[slot] = struct{}{}
		}

		left := materialCost
		for _, slot := range materialSlots {
			if left == 0 {
				break
			}
			key := currentCeraShopInventorySlotKey(dnfrepo.MainInventoryListType, slot)
			stack := inventory.Slots[key]
			take := stack.Count
			if take > left {
				take = left
			}
			stack.Count -= take
			left -= take
			if stack.Count == 0 {
				delete(inventory.Slots, key)
			} else {
				entry := currentItemListEntryFromStack(dnfrepo.MainInventoryListType, slot, stack)
				stack.RawEntry = append([]byte(nil), entry.data[:]...)
				inventory.Slots[key] = stack
			}
			refreshSet[slot] = struct{}{}
		}

		inventory.UpdatedAt = time.Now().UTC()
		if err := dnfrepo.SaveInventoryFields(ctx, inventoryRepo, inventory, dnfrepo.InventoryFieldSlots); err != nil {
			return err
		}
		if equipmentChanged {
			equipment.UpdatedAt = time.Now().UTC()
			if err := dnfrepo.SaveEquipmentFields(ctx, equipmentRepo, equipment, dnfrepo.EquipmentFieldEntries); err != nil {
				return err
			}
		}

		refreshes := make([]alignedcmd.ItemSlotRefresh, 0, len(refreshSet))
		for slot := range refreshSet {
			refreshes = append(refreshes, alignedcmd.ItemSlotRefresh{ListType: dnfrepo.MainInventoryListType, SlotIndex: slot})
		}
		sort.Slice(refreshes, func(i, j int) bool { return refreshes[i].SlotIndex < refreshes[j].SlotIndex })
		result = currentCustomAbilitySetResult{snapshot: snapshot, materialID: catalog.materialID, materialCost: materialCost, materialRemaining: available - materialCost, spentPoints: spentPoints, refreshes: refreshes, equipmentChanged: equipmentChanged}
		return nil
	})
	return result, err
}

func parseCurrentCustomAbilitySetSnapshot(extra map[string]string, recordID byte) (currentCustomAbilitySetSnapshot, bool) {
	if recordID == 0 || extra == nil {
		return currentCustomAbilitySetSnapshot{}, false
	}
	parts := strings.Split(strings.TrimSpace(extra[currentCustomAbilitySetOptionsExtra]), ",")
	if len(parts) < 1 || len(parts) > 5 {
		return currentCustomAbilitySetSnapshot{}, false
	}
	var snapshot currentCustomAbilitySetSnapshot
	snapshot.recordID = recordID
	seen := make(map[byte]struct{}, len(parts))
	for index, value := range parts {
		parsed, err := strconv.ParseUint(strings.TrimSpace(value), 10, 8)
		if err != nil || parsed == 0 {
			return currentCustomAbilitySetSnapshot{}, false
		}
		optionID := byte(parsed)
		if _, duplicate := seen[optionID]; duplicate {
			return currentCustomAbilitySetSnapshot{}, false
		}
		seen[optionID] = struct{}{}
		snapshot.options[index] = optionID
	}
	required, requiredErr := strconv.ParseUint(strings.TrimSpace(extra[currentCustomAbilitySetRequiredPartsExtra]), 10, 8)
	grade, gradeErr := strconv.ParseUint(strings.TrimSpace(extra[currentCustomAbilitySetGradeExtra]), 10, 8)
	partIndex, partErr := strconv.ParseUint(strings.TrimSpace(extra[currentCustomAbilitySetPartIndexExtra]), 10, 8)
	if requiredErr != nil || required < 3 || required > 6 || gradeErr != nil || grade > 3 || partErr != nil || partIndex == 0 || byte(partIndex) != recordID {
		return currentCustomAbilitySetSnapshot{}, false
	}
	snapshot.requiredParts = byte(required)
	snapshot.grade = byte(grade)
	snapshot.partSetIndex = byte(partIndex)
	return snapshot, true
}

func loadCurrentCustomAbilitySetSnapshot(ctx context.Context, repositories dnfrepo.Group, characterID string) (currentCustomAbilitySetSnapshot, bool, error) {
	if repositories.Inventory == nil || repositories.Equipment == nil {
		return currentCustomAbilitySetSnapshot{}, false, errors.New("custom ability set repositories unavailable")
	}
	inventory, _, err := repositories.Inventory.Load(ctx, characterID)
	if err != nil {
		return currentCustomAbilitySetSnapshot{}, false, err
	}
	equipment, _, err := repositories.Equipment.Load(ctx, characterID)
	if err != nil {
		return currentCustomAbilitySetSnapshot{}, false, err
	}
	counts := make(map[currentCustomAbilitySetSnapshot]int)
	consider := func(raw []byte, extra map[string]string) {
		if len(raw) != currentItemListEntryWireSize {
			return
		}
		snapshot, valid := parseCurrentCustomAbilitySetSnapshot(extra, raw[currentCustomAbilitySetRecordOffset])
		if !valid {
			return
		}
		counts[snapshot]++
	}
	for _, stack := range inventory.Slots {
		if stack.Count == 1 {
			consider(stack.RawEntry, stack.Extra)
		}
	}
	for _, equipped := range equipment.Entries {
		consider(equipped.RawEntry, equipped.Extra)
	}
	for snapshot, memberCount := range counts {
		if memberCount >= int(snapshot.requiredParts) {
			return snapshot, true, nil
		}
	}
	return currentCustomAbilitySetSnapshot{}, false, nil
}

func (s *Service) sendCurrentCustomAbilitySetNotification(session *gameSession, snapshot currentCustomAbilitySetSnapshot, source string) error {
	if session == nil || session.selectedCharacterID == 0 {
		return nil
	}
	body := buildCurrentCustomAbilitySetNotificationBody(uint32(session.selectedCharacterID), snapshot)
	s.logPacketEvent("game-upper-custom-ability-set-notification-send", "conn_id", session.connID, "source", source, "char_id", session.selectedCharacterID, "msg_id", currentCustomAbilitySetNotifyOpcode, "classification", 0, "record_id", snapshot.recordID, "required_parts", snapshot.requiredParts, "options", currentCustomAbilitySetOptionsText(snapshot.options[:]), "grade", snapshot.grade, "body_len", len(body), "evidence", "current_exe_sub_1D6D760_registered_at_0x1D7F958_wire_0x04FE")
	return s.sendGameUpperRawClass(session, currentCustomAbilitySetNotifyOpcode, body, 0)
}

func (s *Service) handleCurrentCustomAbilitySet(session *gameSession, body []byte) error {
	request, err := decodeCurrentCustomAbilitySetRequest(body)
	if err != nil || session == nil || session.selectedCharacterID == 0 {
		if err == nil {
			err = errCurrentCustomAbilitySetInvalid
		}
		s.logGameEvent(session, "game-custom-ability-set-rejected", "reason", err, "body_len", len(body))
		return s.sendGameUpperRawClass(session, uint16(dnfenum.CmdPacketCustomAbilitySetEquipOption), buildCurrentCustomAbilitySetFailureBody(err), dnfproto.DefaultChannelClassification)
	}
	s.initialEquipmentMu.Lock()
	archive, archiveErr := s.initialEquipmentArchiveLocked()
	s.initialEquipmentMu.Unlock()
	if archiveErr != nil {
		return archiveErr
	}
	catalog, err := parseCurrentCustomAbilitySetCatalog(archive)
	if err != nil {
		return err
	}
	individualCatalog, err := parseCurrentCustomAbilityCatalog(archive)
	if err != nil {
		return err
	}
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.Inventory == nil || repositories.Equipment == nil || repositories.CharacterItems == nil {
		return errors.New("custom ability set repositories unavailable")
	}
	profileCache := make(map[uint32]currentCustomAbilityItemProfile)
	resolve := func(itemID uint32) (currentCustomAbilityItemProfile, error) {
		if profile, found := profileCache[itemID]; found {
			return profile, nil
		}
		profile, resolveErr := s.resolveCurrentCustomAbilityItemProfile(itemID)
		if resolveErr == nil {
			profileCache[itemID] = profile
		}
		return profile, resolveErr
	}
	ctx, cancel := context.WithTimeout(context.Background(), currentRepositorySnapshotTimeout)
	defer cancel()
	characterID := strconv.Itoa(int(session.selectedCharacterID))
	result, err := commitCurrentCustomAbilitySet(ctx, repositories, characterID, request, individualCatalog, catalog, resolve)
	if err != nil {
		s.logGameEvent(session, "game-custom-ability-set-rejected", "options", currentCustomAbilitySetOptionsText(request.options), "slots", fmt.Sprint(request.slots), "reason", err, "body_len", len(body))
		return s.sendGameUpperRawClass(session, uint16(dnfenum.CmdPacketCustomAbilitySetEquipOption), buildCurrentCustomAbilitySetFailureBody(err), dnfproto.DefaultChannelClassification)
	}
	if err := s.sendSelectedIncrementalItemSlotRefreshes(session, "custom_ability_set_commit", result.refreshes); err != nil {
		return err
	}
	if result.equipmentChanged {
		if err := s.sendSelectedCurrentEquipmentSlotItemUpdates(session, "custom_ability_set_clear_previous_equipped_members"); err != nil {
			return err
		}
	}
	if err := s.sendGameUpperRawClass(session, uint16(dnfenum.CmdPacketCustomAbilitySetEquipOption), buildCurrentCustomAbilitySetSuccessBody(), dnfproto.DefaultChannelClassification); err != nil {
		return err
	}
	s.logGameEvent(session, "game-custom-ability-set-committed", "options", currentCustomAbilitySetOptionsText(request.options), "slots", fmt.Sprint(request.slots), "set_grade", result.snapshot.grade, "spent_points", result.spentPoints, "material_item_id", result.materialID, "material_cost", result.materialCost, "material_remaining", result.materialRemaining, "sequence", "op14_list0_then_optional_op14_list3_then_class1_op1341_ack_then_class0_wire1278")
	return s.sendCurrentCustomAbilitySetNotification(session, result.snapshot, "custom_ability_set_commit")
}
