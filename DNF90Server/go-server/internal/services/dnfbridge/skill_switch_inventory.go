package dnfbridge

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

const skillSwitchInventoryStateValue = "state_json"

var (
	errSkillSwitchUnavailable  = errors.New("skill switch inventory is unavailable")
	errSkillSwitchInvalidItem  = errors.New("skill switch inventory item is invalid")
	errSkillSwitchInvalidSkill = errors.New("skill switch inventory skill is invalid")
)

type skillSwitchInventoryPosition struct {
	EquipmentPart byte
	ListType      byte
	Slot          int16
	ItemID        uint32
	ItemSerial    uint32
	Raw           [currentItemListEntryWireSize]byte
}

type skillSwitchInventoryResolution struct {
	Positions          map[byte]skillSwitchInventoryPosition
	ExactMatchCount    int
	FallbackMatchCount int
}

func skillSwitchInventoryScope(characterID string) string {
	return "character:" + strings.TrimSpace(characterID) + ":skill_switch_inventory"
}

func loadSkillSwitchInventoryState(ctx context.Context, repo dnfrepo.SettingsRepository, characterID string) (skillSwitchInventoryState, error) {
	if repo == nil || strings.TrimSpace(characterID) == "" {
		return skillSwitchInventoryState{}, errSkillSwitchUnavailable
	}
	record, found, err := repo.Load(ctx, skillSwitchInventoryScope(characterID))
	if err != nil {
		return skillSwitchInventoryState{}, err
	}
	if !found {
		return skillSwitchInventoryState{}, nil
	}
	raw := strings.TrimSpace(record.Values[skillSwitchInventoryStateValue])
	if raw == "" {
		return skillSwitchInventoryState{}, nil
	}
	var state skillSwitchInventoryState
	if err := json.Unmarshal([]byte(raw), &state); err != nil {
		return skillSwitchInventoryState{}, fmt.Errorf("decode skill-switch state: %w", err)
	}
	return normalizeSkillSwitchInventoryState(state), nil
}

func saveSkillSwitchInventoryState(ctx context.Context, repo dnfrepo.SettingsRepository, characterID string, state skillSwitchInventoryState) error {
	state = normalizeSkillSwitchInventoryState(state)
	raw, err := json.Marshal(state)
	if err != nil {
		return err
	}
	return repo.Save(ctx, dnfrepo.SettingsRecord{
		Scope:     skillSwitchInventoryScope(characterID),
		Values:    map[string]string{skillSwitchInventoryStateValue: string(raw)},
		UpdatedAt: time.Now().UTC(),
	})
}

func normalizeSkillSwitchInventoryState(state skillSwitchInventoryState) skillSwitchInventoryState {
	byPart := make(map[byte]skillSwitchBinding, len(state.Items))
	usedSerial := make(map[uint32]byte, len(state.Items))
	for _, item := range state.Items {
		if item.EquipmentPart > 32 || item.ItemSerial == 0 || item.ItemID == 0 {
			continue
		}
		if _, exists := usedSerial[item.ItemSerial]; exists {
			continue
		}
		usedSerial[item.ItemSerial] = item.EquipmentPart
		byPart[item.EquipmentPart] = item
	}
	state.Items = state.Items[:0]
	for _, item := range byPart {
		state.Items = append(state.Items, item)
	}
	sort.Slice(state.Items, func(i, j int) bool { return state.Items[i].EquipmentPart < state.Items[j].EquipmentPart })
	return state
}

func replaceSkillSwitchBinding(state skillSwitchInventoryState, binding skillSwitchBinding) skillSwitchInventoryState {
	items := make([]skillSwitchBinding, 0, len(state.Items)+1)
	for _, item := range state.Items {
		if item.EquipmentPart == binding.EquipmentPart || item.ItemSerial == binding.ItemSerial {
			continue
		}
		items = append(items, item)
	}
	items = append(items, binding)
	state.Items = items
	return normalizeSkillSwitchInventoryState(state)
}

func removeSkillSwitchBinding(state skillSwitchInventoryState, part byte) skillSwitchInventoryState {
	items := state.Items[:0]
	for _, item := range state.Items {
		if item.EquipmentPart != part {
			items = append(items, item)
		}
	}
	state.Items = items
	return normalizeSkillSwitchInventoryState(state)
}

func skillSwitchItemSerial(extra map[string]string, raw []byte) uint32 {
	for _, key := range []string{"item_serial", "item_uid", "serial", "instance_serial", "creature_serial_or_handle"} {
		value := strings.TrimSpace(extra[key])
		if value == "" {
			continue
		}
		parsed, err := strconv.ParseUint(value, 10, 32)
		if err == nil && parsed != 0 {
			return uint32(parsed)
		}
	}
	return 0
}

// currentSkillSwitchEquipmentSerial preserves the creature identity used by
// the current client. Creature equipment stores its serial/handle at raw+24;
// treating that value as a generic equipment serial makes the binding vanish
// on the following inventory snapshot.
func currentSkillSwitchEquipmentSerial(entry dnfrepo.EquipmentEntry) uint32 {
	if value := currentEquippedCreatureInstance(entry); value != 0 {
		return value
	}
	return skillSwitchItemSerial(entry.Extra, entry.RawEntry)
}

func currentSkillSwitchItemListEntryFromEquipment(equipped dnfrepo.EquipmentEntry) (currentItemListEntry, bool) {
	if entry, ok := currentItemListEntryFromEquipment(equipped); ok {
		return entry, true
	}
	if equipped.SlotIndex != 26 || currentEquippedCreatureInstance(equipped) == 0 {
		return currentItemListEntry{}, false
	}
	// List-3 registration needs the canonical 0x77 item row, but slot 26 is
	// deliberately excluded from ordinary actor-equipment construction. Supply
	// only a transient actor-slot hint for this projection; durable equipment
	// and the dedicated creature lifecycle remain unchanged.
	if equipped.Extra == nil {
		equipped.Extra = make(map[string]string, 1)
	} else {
		cloned := make(map[string]string, len(equipped.Extra)+1)
		for key, value := range equipped.Extra {
			cloned[key] = value
		}
		equipped.Extra = cloned
	}
	equipped.Extra["current_exe_equipment_type"] = "26"
	return currentItemListEntryFromEquipment(equipped)
}

func assignSkillSwitchItemSerial(stack *dnfrepo.ItemStack, used map[uint32]struct{}) uint32 {
	if stack == nil {
		return 0
	}
	if serial := skillSwitchItemSerial(stack.Extra, stack.RawEntry); serial != 0 {
		return serial
	}
	for serial := uint32(1); serial != 0; serial++ {
		if _, exists := used[serial]; exists {
			continue
		}
		if stack.Extra == nil {
			stack.Extra = make(map[string]string, 2)
		}
		stack.Extra["item_serial"] = strconv.FormatUint(uint64(serial), 10)
		return serial
	}
	return 0
}

func collectSkillSwitchSerials(inventory dnfrepo.InventoryRecord, equipment dnfrepo.EquipmentRecord) map[uint32]struct{} {
	used := make(map[uint32]struct{})
	for _, stack := range inventory.Slots {
		if serial := skillSwitchItemSerial(stack.Extra, stack.RawEntry); serial != 0 {
			used[serial] = struct{}{}
		}
	}
	for _, stack := range inventory.Warehouse {
		if serial := skillSwitchItemSerial(stack.Extra, stack.RawEntry); serial != 0 {
			used[serial] = struct{}{}
		}
	}
	for _, entry := range equipment.Entries {
		if serial := currentSkillSwitchEquipmentSerial(entry); serial != 0 {
			used[serial] = struct{}{}
		}
	}
	return used
}

func (s *Service) handleSkillSwitchInventory(session *gameSession, body []byte) error {
	request, action, parseErr := decodeSkillSwitchInventoryRequest(body)
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	if parseErr != nil {
		s.logGameEvent(session, "game-skill-switch-inventory-rejected", "body_len", len(body), "reason", parseErr)
		if err := s.sendGameUpperRawClass(session, skillSwitchInventoryCmdWire, nil, 0); err != nil {
			return err
		}
		return s.sendSkillSwitchInventoryAllData(ctx, session)
	}
	mutationErr := s.mutateSkillSwitchInventory(ctx, session, request, action)
	if mutationErr != nil {
		s.logGameEvent(session, "game-skill-switch-inventory-rejected", "action", action, "part", request.EquipmentPart, "list_type", request.ListType, "slot", request.Slot, "skill_id", request.SkillID, "reason", mutationErr)
	}
	if err := s.sendGameUpperRawClass(session, skillSwitchInventoryCmdWire, nil, 0); err != nil {
		return err
	}
	if action == skillSwitchSetItem || action == skillSwitchRemoveItem {
		return s.sendSkillSwitchInventoryDelta(ctx, session, []byte{request.EquipmentPart})
	}
	return s.sendSkillSwitchInventoryAllData(ctx, session)
}

func (s *Service) mutateSkillSwitchInventory(ctx context.Context, session *gameSession, request skillSwitchInventoryRequest, action skillSwitchInventoryAction) error {
	if s == nil || session == nil || session.selectedCharacterID == 0 {
		return errSkillSwitchUnavailable
	}
	s.skillSwitchInventoryMu.Lock()
	defer s.skillSwitchInventoryMu.Unlock()
	repos, ok := s.repositoryGroup()
	if !ok || repos.Settings == nil || repos.Character == nil || repos.Skill == nil || repos.Inventory == nil || repos.Equipment == nil {
		return errSkillSwitchUnavailable
	}
	characterID := strconv.FormatUint(uint64(session.selectedCharacterID), 10)
	character, found, err := repos.Character.Load(ctx, characterID)
	if err != nil || !found || character.CharacterID != characterID || strings.TrimSpace(character.AccountID) != strings.TrimSpace(s.accountIDForSession(session)) {
		return errSkillSwitchUnavailable
	}
	state, err := loadSkillSwitchInventoryState(ctx, repos.Settings, characterID)
	if err != nil {
		return err
	}
	switch action {
	case skillSwitchSetSkill:
		skillID := request.SkillID
		if skillID == math.MaxUint16 {
			skillID = 0
		}
		if skillID != 0 {
			skills, found, loadErr := repos.Skill.Load(ctx, characterID)
			if loadErr != nil || !found {
				return errSkillSwitchInvalidSkill
			}
			learned, ok := skillSwitchActiveTreeSkills(skills, character)[int64(skillID)]
			if !ok || !learned.Enabled || learned.Level <= 0 {
				return errSkillSwitchInvalidSkill
			}
			allowed, whitelistErr := s.skillSwitchSkillAllowedByPVF(character, skillID)
			if whitelistErr != nil || !allowed {
				return errors.Join(errSkillSwitchInvalidSkill, whitelistErr)
			}
		}
		state.SkillID = skillID
	case skillSwitchRemoveItem:
		state = removeSkillSwitchBinding(state, request.EquipmentPart)
	case skillSwitchSetItem:
		binding, err := s.skillSwitchBindingForRequest(ctx, repos, characterID, request)
		if err != nil {
			return err
		}
		state = replaceSkillSwitchBinding(state, binding)
	default:
		return errSkillSwitchUnavailable
	}
	return saveSkillSwitchInventoryState(ctx, repos.Settings, characterID, state)
}

func (s *Service) skillSwitchSkillAllowedByPVF(character dnfrepo.CharacterRecord, skillID uint16) (bool, error) {
	s.initialEquipmentMu.Lock()
	archive, err := s.initialEquipmentArchiveLocked()
	s.initialEquipmentMu.Unlock()
	if err != nil {
		return false, err
	}
	return skillSwitchSkillAllowedByPVFSource(archive, character, skillID)
}

func skillSwitchSkillAllowedByPVFSource(source initialEquipmentTextSource, character dnfrepo.CharacterRecord, skillID uint16) (bool, error) {
	candidates, err := skillSwitchSupportSkillsByPVFSource(source, character)
	if err != nil {
		return false, err
	}
	for _, candidate := range candidates {
		if candidate == skillID {
			return true, nil
		}
	}
	return false, nil
}

func (s *Service) skillSwitchSupportSkillsByPVF(character dnfrepo.CharacterRecord) ([]uint16, error) {
	s.initialEquipmentMu.Lock()
	archive, err := s.initialEquipmentArchiveLocked()
	s.initialEquipmentMu.Unlock()
	if err != nil {
		return nil, err
	}
	return skillSwitchSupportSkillsByPVFSource(archive, character)
}

// skillSwitchSupportSkillsByPVFSource returns, in PVF order, the buff skills
// the BUFF-enhancement (换装) window offers this character's job/grow type:
// the `[skill]` block that follows its `[character type]` entry in
// skill/switchingsupportskill.etc (priest grow 1 lists `7 53 45 258`, i.e.
// 勇气祝福, 天使祝福, 荣誉祝福, ...).
func skillSwitchSupportSkillsByPVFSource(source initialEquipmentTextSource, character dnfrepo.CharacterRecord) ([]uint16, error) {
	if source == nil {
		return nil, errSkillSwitchInvalidSkill
	}
	job, ok := characterJobByte(character)
	if !ok {
		return nil, errSkillSwitchInvalidSkill
	}
	jobTag, err := currentRentalCharacterJobTag(source, job)
	if err != nil {
		return nil, err
	}
	text, err := source.ReadText("skill/switchingsupportskill.etc")
	if err != nil {
		return nil, err
	}
	document, err := dnfpvf.Parse("skill/switchingsupportskill.etc", text)
	if err != nil {
		return nil, err
	}
	wantJob := skillSwitchJobTag(jobTag)
	wantGrow := int(numericCharacterStatValue(character, "grow_type")) & 0x0f
	for idx, section := range document.Sections {
		if !strings.EqualFold(strings.TrimSpace(section.Name), "character type") {
			continue
		}
		profileJob, profileGrow, valid := skillSwitchCharacterTypeTokens(document.Tokens[section.Start:section.End])
		if !valid || skillSwitchJobTag(profileJob) != wantJob || profileGrow != wantGrow {
			continue
		}
		var candidates []uint16
		for next := idx + 1; next < len(document.Sections); next++ {
			nextSection := document.Sections[next]
			name := strings.ToLower(strings.TrimSpace(nextSection.Name))
			if name == "character type" {
				break
			}
			if name != "skill" {
				continue
			}
			for _, token := range document.Tokens[nextSection.Start:nextSection.End] {
				if token.Kind == dnfpvf.TokenInt && token.Int > 0 && token.Int <= math.MaxUint16 {
					candidates = append(candidates, uint16(token.Int))
				}
			}
		}
		return candidates, nil
	}
	return nil, nil
}

// defaultSkillSwitchSupportSkill picks the buff skill the BUFF-enhancement
// record should name when the player has never chosen one: the first PVF
// candidate the character has actually learned.
//
// The current client computes the buff it applies to *party members* from
// this record, while the caster's own buff uses the live skill. With an empty
// record (skill 0, no items - what every never-registered buffer received) the
// caster's client sent teammates a level-0 buff: live 2026-09-06, a crusader
// with 勇气祝福 Lv12 (skill 7, op19) whose op1369 was `00 00 00` buffed its
// teammate at level 0, whereas the working reference capture's buffer carried
// a registration (`0e 00 11 ...`, skill 14 + 17 items) and its teammates got
// the right level. The default is a projection only; the persisted state stays
// untouched so a later explicit choice from the 换装 window wins.
func defaultSkillSwitchSupportSkill(candidates []uint16, skills map[int64]dnfrepo.SkillState) uint16 {
	for _, candidate := range candidates {
		learned, ok := skills[int64(candidate)]
		if ok && learned.Enabled && learned.Level > 0 {
			return candidate
		}
	}
	return 0
}

// skillSwitchActiveTreeSkills returns the character's active skill-tree rows.
// The durable record keeps tree 0 in Skills and alternate trees in Trees; the
// buff skill may live on either plan, so registration checks must read the
// active tree instead of always reading tree 0.
func skillSwitchActiveTreeSkills(skills dnfrepo.SkillRecord, character dnfrepo.CharacterRecord) map[int64]dnfrepo.SkillState {
	tree := currentSkillInfoTreeIndex
	if active, err := currentSceneActiveSkillTree(character); err == nil {
		tree = int(active)
	}
	if state, ok := dnfrepo.SkillTree(skills, tree); ok {
		return state.Skills
	}
	return skills.Skills
}

func skillSwitchJobTag(value string) string {
	value = strings.TrimSpace(value)
	if strings.HasPrefix(value, "[") && strings.HasSuffix(value, "]") {
		value = strings.TrimSpace(value[1 : len(value)-1])
	}
	return currentRentalJobTag(value)
}

func skillSwitchCharacterTypeTokens(tokens []dnfpvf.Token) (string, int, bool) {
	job := ""
	grow := -1
	for _, token := range tokens {
		switch {
		case job == "" && (token.Kind == dnfpvf.TokenString || token.Kind == dnfpvf.TokenIdent):
			job = token.Value
		case token.Kind == dnfpvf.TokenInt && grow < 0:
			grow = int(token.Int)
		}
	}
	return job, grow, job != "" && grow >= 0 && grow <= 5
}

func (s *Service) skillSwitchBindingForRequest(ctx context.Context, repos dnfrepo.Group, characterID string, request skillSwitchInventoryRequest) (skillSwitchBinding, error) {
	if !skillSwitchListTypeSupported(request.ListType, request.EquipmentPart, request.Slot) {
		return skillSwitchBinding{}, errSkillSwitchInvalidItem
	}
	inventory, found, err := repos.Inventory.Load(ctx, characterID)
	if err != nil || !found {
		return skillSwitchBinding{}, errSkillSwitchInvalidItem
	}
	equipment, _, err := repos.Equipment.Load(ctx, characterID)
	if err != nil {
		return skillSwitchBinding{}, errSkillSwitchInvalidItem
	}
	used := collectSkillSwitchSerials(inventory, equipment)
	if request.ListType == 3 || request.ListType == 17 {
		entry, ok := equipment.Entries[strconv.Itoa(int(request.Slot))]
		if !ok || entry.ItemID <= 0 || entry.ItemID > math.MaxUint32 || entry.SlotIndex != int16(request.EquipmentPart) {
			return skillSwitchBinding{}, errSkillSwitchInvalidItem
		}
		if err := s.validateSkillSwitchItemPart(uint32(entry.ItemID), request.EquipmentPart); err != nil {
			return skillSwitchBinding{}, err
		}
		serial := currentSkillSwitchEquipmentSerial(entry)
		if serial == 0 {
			for candidate := uint32(1); candidate != 0; candidate++ {
				if _, exists := used[candidate]; exists {
					continue
				}
				serial = candidate
				if entry.Extra == nil {
					entry.Extra = make(map[string]string, 2)
				}
				entry.Extra["item_serial"] = strconv.FormatUint(uint64(serial), 10)
				equipment.Entries[strconv.Itoa(int(request.Slot))] = entry
				if err := repos.Equipment.Save(ctx, equipment); err != nil {
					return skillSwitchBinding{}, err
				}
				break
			}
		}
		if serial == 0 {
			return skillSwitchBinding{}, errSkillSwitchInvalidItem
		}
		return skillSwitchBinding{EquipmentPart: request.EquipmentPart, ItemSerial: serial, ItemID: uint32(entry.ItemID)}, nil
	}
	var items map[string]dnfrepo.ItemStack
	if request.ListType == 2 {
		items = inventory.Warehouse
	} else {
		items = inventory.Slots
	}
	key := fmt.Sprintf("%d:%d", request.ListType, request.Slot)
	stack, ok := items[key]
	if !ok || stack.ItemID <= 0 || stack.ItemID > math.MaxUint32 {
		return skillSwitchBinding{}, errSkillSwitchInvalidItem
	}
	if err := s.validateSkillSwitchItemPart(uint32(stack.ItemID), request.EquipmentPart); err != nil {
		return skillSwitchBinding{}, err
	}
	serialWasMissing := skillSwitchItemSerial(stack.Extra, stack.RawEntry) == 0
	serial := assignSkillSwitchItemSerial(&stack, used)
	if serial == 0 {
		return skillSwitchBinding{}, errSkillSwitchInvalidItem
	}
	if serialWasMissing {
		if request.ListType == 2 {
			inventory.Warehouse[key] = stack
		} else {
			inventory.Slots[key] = stack
		}
		if err := repos.Inventory.Save(ctx, inventory); err != nil {
			return skillSwitchBinding{}, err
		}
	}
	return skillSwitchBinding{EquipmentPart: request.EquipmentPart, ItemSerial: serial, ItemID: uint32(stack.ItemID)}, nil
}

func (s *Service) validateSkillSwitchItemPart(itemID uint32, part byte) error {
	catalog, err := s.currentPVFItemCatalog()
	if err != nil || catalog == nil {
		return errors.Join(errSkillSwitchInvalidItem, err)
	}
	definition, err := catalog.ResolveItem(itemID)
	if err != nil || strings.TrimSpace(definition.EquipmentType) == "" {
		return errors.Join(errSkillSwitchInvalidItem, err)
	}
	// [creature] deliberately remains outside the ordinary equipment-placement
	// map because pet movement has its own lifecycle. BUFF registration still
	// accepts the equipped creature at actor slot 26.
	if part == 26 && strings.EqualFold(strings.TrimSpace(definition.EquipmentType), "[creature]") {
		return nil
	}
	rule, ok := currentEquipmentPlacementRuleForPVFType(definition.EquipmentType)
	if !ok || rule.targetSlot < 0 || rule.targetSlot > 32 || byte(rule.targetSlot) != part {
		return errSkillSwitchInvalidItem
	}
	return nil
}

func skillSwitchListTypeSupported(listType, part byte, slot uint16) bool {
	switch listType {
	case 0, 2:
		return part <= 32
	case 1:
		return part <= 10
	case 3, 17:
		return slot <= 32 && byte(slot) == part
	case 7:
		return part >= 26 && part <= 29
	case 38:
		return part == 32
	default:
		return false
	}
}

func (s *Service) currentSkillSwitchInventorySnapshot(ctx context.Context, session *gameSession) (skillSwitchInventoryState, map[byte]skillSwitchInventoryPosition, error) {
	state, resolution, err := s.currentSkillSwitchInventorySnapshotWithResolution(ctx, session)
	return state, resolution.Positions, err
}

func (s *Service) currentSkillSwitchInventorySnapshotWithResolution(ctx context.Context, session *gameSession) (skillSwitchInventoryState, skillSwitchInventoryResolution, error) {
	if s == nil || session == nil || session.selectedCharacterID == 0 {
		return skillSwitchInventoryState{}, skillSwitchInventoryResolution{}, errSkillSwitchUnavailable
	}
	repos, ok := s.repositoryGroup()
	if !ok || repos.Settings == nil || repos.Inventory == nil || repos.Equipment == nil {
		return skillSwitchInventoryState{}, skillSwitchInventoryResolution{}, errSkillSwitchUnavailable
	}
	characterID := strconv.FormatUint(uint64(session.selectedCharacterID), 10)
	state, err := loadSkillSwitchInventoryState(ctx, repos.Settings, characterID)
	if err != nil {
		return skillSwitchInventoryState{}, skillSwitchInventoryResolution{}, err
	}
	stateDirty := false
	projectedSkillID := uint16(0)
	if repos.Skill != nil {
		if skills, found, skillErr := repos.Skill.Load(ctx, characterID); skillErr == nil && found {
			if state.SkillID != 0 {
				var character dnfrepo.CharacterRecord
				if repos.Character != nil {
					if loaded, charFound, charErr := repos.Character.Load(ctx, characterID); charErr == nil && charFound {
						character = loaded
					}
				}
				activeSkills := skillSwitchActiveTreeSkills(skills, character)
				learned, ok := activeSkills[int64(state.SkillID)]
				if !ok || !learned.Enabled || learned.Level <= 0 {
					activeTree := currentSkillInfoTreeIndex
					if active, treeErr := currentSceneActiveSkillTree(character); treeErr == nil {
						activeTree = int(active)
					}
					s.logGameEvent(session, "game-skill-switch-inventory-skill-invalidated",
						"char_id", characterID,
						"skill_id", state.SkillID,
						"active_tree", activeTree,
						"learned_found", ok,
						"learned_enabled", learned.Enabled,
						"learned_level", learned.Level,
						"tree0_level", skills.Skills[int64(state.SkillID)].Level,
						"reason", "registered_buff_skill_not_learned_on_active_tree")
					state.SkillID = 0
					stateDirty = true
				}
			}
			if state.SkillID == 0 && repos.Character != nil {
				projectedSkillID = s.defaultSkillSwitchSupportSkillFor(ctx, session, repos, characterID, skills)
			}
		}
	}
	inventory, _, err := repos.Inventory.Load(ctx, characterID)
	if err != nil {
		return state, skillSwitchInventoryResolution{}, err
	}
	equipment, _, err := repos.Equipment.Load(ctx, characterID)
	if err != nil {
		return state, skillSwitchInventoryResolution{}, err
	}
	resolution := resolveSkillSwitchBindings(state, inventory, equipment)
	if stateDirty {
		if err := saveSkillSwitchInventoryState(ctx, repos.Settings, characterID, state); err != nil {
			return state, resolution, err
		}
	}
	// Applied after the durable save on purpose: the default is what the
	// client sees, never what the player chose.
	if state.SkillID == 0 && projectedSkillID != 0 {
		state.SkillID = projectedSkillID
	}
	return state, resolution, nil
}

func (s *Service) defaultSkillSwitchSupportSkillFor(
	ctx context.Context,
	session *gameSession,
	repos dnfrepo.Group,
	characterID string,
	skills dnfrepo.SkillRecord,
) uint16 {
	character, found, err := repos.Character.Load(ctx, characterID)
	if err != nil || !found {
		return 0
	}
	candidates, err := s.skillSwitchSupportSkillsByPVF(character)
	if err != nil {
		s.logGameEvent(session, "game-skill-switch-default-support-skill-deferred",
			"char_id", characterID,
			"error", err)
		return 0
	}
	skillID := defaultSkillSwitchSupportSkill(candidates, skillSwitchActiveTreeSkills(skills, character))
	if skillID != 0 {
		s.logGameEvent(session, "game-skill-switch-default-support-skill",
			"char_id", characterID,
			"skill_id", skillID,
			"candidates", candidates,
			"reason", "no_registered_support_skill_current_client_applies_party_buff_from_this_record")
	}
	return skillID
}

func locateSkillSwitchBindings(state skillSwitchInventoryState, inventory dnfrepo.InventoryRecord, equipment dnfrepo.EquipmentRecord) map[byte]skillSwitchInventoryPosition {
	return resolveSkillSwitchBindings(state, inventory, equipment).Positions
}

func resolveSkillSwitchBindings(state skillSwitchInventoryState, inventory dnfrepo.InventoryRecord, equipment dnfrepo.EquipmentRecord) skillSwitchInventoryResolution {
	positions := make(map[byte]skillSwitchInventoryPosition)
	resolution := skillSwitchInventoryResolution{Positions: positions}
	for _, binding := range state.Items {
		exactMatches := make([]skillSwitchInventoryPosition, 0, 1)
		fallbackMatches := make([]skillSwitchInventoryPosition, 0, 1)
		for key, stack := range inventory.Slots {
			listType, slot, ok := parseSceneInventorySlotKey(key)
			if !ok || !skillSwitchListTypeSupported(listType, binding.EquipmentPart, uint16(slot)) || uint32(stack.ItemID) != binding.ItemID {
				continue
			}
			entry := currentItemListEntryFromStack(listType, slot, stack)
			position := skillSwitchInventoryPosition{EquipmentPart: binding.EquipmentPart, ListType: listType, Slot: slot, ItemID: binding.ItemID, ItemSerial: binding.ItemSerial, Raw: entry.data}
			if skillSwitchItemSerial(stack.Extra, stack.RawEntry) == binding.ItemSerial {
				exactMatches = append(exactMatches, position)
			} else if skillSwitchItemSerial(stack.Extra, stack.RawEntry) == 0 {
				fallbackMatches = append(fallbackMatches, position)
			}
		}
		for key, stack := range inventory.Warehouse {
			_, slot, ok := parseSceneInventorySlotKey(key)
			if !ok || uint32(stack.ItemID) != binding.ItemID {
				continue
			}
			entry := currentItemListEntryFromStack(2, slot, stack)
			position := skillSwitchInventoryPosition{EquipmentPart: binding.EquipmentPart, ListType: 2, Slot: slot, ItemID: binding.ItemID, ItemSerial: binding.ItemSerial, Raw: entry.data}
			if skillSwitchItemSerial(stack.Extra, stack.RawEntry) == binding.ItemSerial {
				exactMatches = append(exactMatches, position)
			} else if skillSwitchItemSerial(stack.Extra, stack.RawEntry) == 0 {
				fallbackMatches = append(fallbackMatches, position)
			}
		}
		for _, equipped := range equipment.Entries {
			if uint32(equipped.ItemID) != binding.ItemID {
				continue
			}
			entry, ok := currentSkillSwitchItemListEntryFromEquipment(equipped)
			if !ok {
				continue
			}
			position := skillSwitchInventoryPosition{EquipmentPart: binding.EquipmentPart, ListType: 3, Slot: equipped.SlotIndex, ItemID: binding.ItemID, ItemSerial: binding.ItemSerial, Raw: entry.data}
			if currentSkillSwitchEquipmentSerial(equipped) == binding.ItemSerial {
				exactMatches = append(exactMatches, position)
			} else if currentSkillSwitchEquipmentSerial(equipped) == 0 {
				fallbackMatches = append(fallbackMatches, position)
			}
		}
		if len(exactMatches) == 1 {
			positions[binding.EquipmentPart] = exactMatches[0]
			resolution.ExactMatchCount++
		} else if len(exactMatches) == 0 && len(fallbackMatches) == 1 {
			positions[binding.EquipmentPart] = fallbackMatches[0]
			resolution.FallbackMatchCount++
		}
	}
	return resolution
}

func (s *Service) sendSkillSwitchInventoryDelta(ctx context.Context, session *gameSession, parts []byte) error {
	_, positions, err := s.currentSkillSwitchInventorySnapshot(ctx, session)
	if err != nil {
		positions = nil
	}
	body := buildSkillSwitchDeltaPositionBody(positions, parts)
	return s.sendGameUpperRawClass(session, skillSwitchInventoryDeltaWire, body, 0)
}

func (s *Service) sendSkillSwitchInventoryAllData(ctx context.Context, session *gameSession) error {
	state, positions, err := s.currentSkillSwitchInventorySnapshot(ctx, session)
	if err != nil {
		state = skillSwitchInventoryState{}
		positions = nil
	}
	return s.sendGameUpperRawClass(session, skillSwitchInventoryAllDataWire, buildSkillSwitchAllDataBody(state, positions), 0)
}

// sendSkillSwitchInventoryLocationRefresh follows a registered equipment
// instance after op19 movement or an inventory sort. Durable registration owns
// the item template plus the server-assigned instance serial, never the grid
// cell; rebuilding op559 from the committed repositories prevents the client
// from retaining the old list/slot until the player registers the item again.
//
// Characters without registered items receive no extra packet. This keeps the
// ordinary and skill-only move paths byte-for-byte unchanged because their
// registration tables contain no item position that could become stale.
func (s *Service) sendSkillSwitchInventoryLocationRefresh(session *gameSession, source string) error {
	if s == nil || session == nil || session.selectedCharacterID == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	state, resolution, err := s.currentSkillSwitchInventorySnapshotWithResolution(ctx, session)
	if err != nil {
		return err
	}
	if len(state.Items) == 0 {
		return nil
	}
	body := buildSkillSwitchAllDataBody(state, resolution.Positions)
	s.logGameEvent(session, "game-skill-switch-inventory-item-location-refresh-send",
		"source", source,
		"char_id", session.selectedCharacterID,
		"msg_id", skillSwitchInventoryAllDataWire,
		"registered_item_count", len(state.Items),
		"resolved_item_count", len(resolution.Positions),
		"exact_match_count", resolution.ExactMatchCount,
		"fallback_match_count", resolution.FallbackMatchCount,
		"wire_item_count", body[2],
		"reason", "durable_item_serial_resolved_to_committed_dynamic_list_and_slot_after_item_move")
	return s.sendGameUpperRawClass(session, skillSwitchInventoryAllDataWire, body, 0)
}

func (s *Service) sendInitialSkillSwitchInventoryAllData(ctx context.Context, session *gameSession, source string) error {
	return s.sendInitialSkillSwitchInventoryAllDataWithRefresh(ctx, session, source, false)
}

// sendSkillSwitchInventoryAllDataRefresh replays the durable BUFF registration
// projection after a dungeon actor is rebuilt. The client clears its scene-
// local registration view during the transition, while the character
// generation remains unchanged, so the login-generation gate cannot be used
// here.
func (s *Service) sendSkillSwitchInventoryAllDataRefresh(ctx context.Context, session *gameSession, source string) error {
	return s.sendInitialSkillSwitchInventoryAllDataWithRefresh(ctx, session, source, true)
}

func (s *Service) sendInitialSkillSwitchInventoryAllDataWithRefresh(ctx context.Context, session *gameSession, source string, refresh bool) error {
	if s == nil || session == nil || session.selectedCharacterID == 0 {
		return nil
	}
	if !refresh && session.skillSwitchInventoryAllDataCharacterID == session.selectedCharacterID &&
		session.skillSwitchInventoryAllDataGeneration == session.characterGeneration {
		return nil
	}

	state, resolution, err := s.currentSkillSwitchInventorySnapshotWithResolution(ctx, session)
	if err != nil {
		s.logGameEvent(session, "game-skill-switch-inventory-initial-refresh-deferred",
			"source", source,
			"char_id", session.selectedCharacterID,
			"character_generation", session.characterGeneration,
			"error", err)
		return err
	}
	chronicleRefreshRows, err := s.sendSkillSwitchChronicleOptionRefreshes(session, resolution.Positions, source)
	if err != nil {
		return err
	}
	body := buildSkillSwitchAllDataBody(state, resolution.Positions)
	reason := "current_exe_mode1_does_not_reset_buff_registration_ui_after_character_change"
	if refresh {
		reason = "current_exe_dungeon_mode1_clears_scene_local_buff_registration_ui"
	}
	s.logGameEvent(session, "game-skill-switch-inventory-initial-refresh-send",
		"source", source,
		"char_id", session.selectedCharacterID,
		"character_generation", session.characterGeneration,
		"msg_id", skillSwitchInventoryAllDataWire,
		"state_item_count", len(state.Items),
		"resolved_item_count", len(resolution.Positions),
		"exact_match_count", resolution.ExactMatchCount,
		"fallback_match_count", resolution.FallbackMatchCount,
		"chronicle_item_refresh_rows", chronicleRefreshRows,
		"wire_item_count", body[2],
		"skill_id", state.SkillID,
		"body_len", len(body),
		"reason", reason)
	if err := s.sendGameUpperRawClass(session, skillSwitchInventoryAllDataWire, body, 0); err != nil {
		return err
	}
	session.skillSwitchInventoryAllDataCharacterID = session.selectedCharacterID
	session.skillSwitchInventoryAllDataGeneration = session.characterGeneration
	return nil
}

// sendSkillSwitchChronicleOptionRefreshes hydrates the item objects referenced
// by op559 before the BUFF-registration table is rebuilt. The current client
// does not carry Chronicle aura attributes in op559 itself; it resolves the
// green/red/blue skill modifier from raw+0x27 on the referenced list-0/list-2/
// list-3 item. Scene reconstruction replaces those client-side item objects,
// so replaying only the durable positions preserves the slots but loses the
// percentage modifier until a later inventory refresh or relog.
//
// Keep this narrowly scoped to repository-proven Chronicle option vectors.
// Ordinary registered equipment is left on the existing mode1/op13 path and
// no full inventory replay is introduced.
func (s *Service) sendSkillSwitchChronicleOptionRefreshes(
	session *gameSession,
	positions map[byte]skillSwitchInventoryPosition,
	source string,
) (int, error) {
	type slotEntry struct {
		slot  int16
		entry currentItemListEntry
	}
	byList := make(map[byte]map[int16]currentItemListEntry)
	for _, position := range positions {
		if position.ListType != 0 && position.ListType != 2 && position.ListType != 3 {
			continue
		}
		if !currentSkillSwitchHasChronicleOptionVector(position.Raw) {
			continue
		}
		if byList[position.ListType] == nil {
			byList[position.ListType] = make(map[int16]currentItemListEntry)
		}
		entry := currentItemListEntry{data: position.Raw}
		byList[position.ListType][position.Slot] = entry
	}

	listTypes := make([]int, 0, len(byList))
	for listType := range byList {
		listTypes = append(listTypes, int(listType))
	}
	sort.Ints(listTypes)
	totalRows := 0
	for _, listTypeValue := range listTypes {
		listType := byte(listTypeValue)
		rows := make([]slotEntry, 0, len(byList[listType]))
		for slot, entry := range byList[listType] {
			rows = append(rows, slotEntry{slot: slot, entry: entry})
		}
		sort.Slice(rows, func(i, j int) bool { return rows[i].slot < rows[j].slot })
		entries := make([]currentItemListEntry, 0, len(rows))
		for _, row := range rows {
			entries = append(entries, row.entry)
		}
		if err := s.sendGameUpperRawClass(
			session,
			uint16(dnfenum.CmdPacketWalkoutPartyMember),
			buildCurrentItemUpdateBody(listType, entries),
			0,
		); err != nil {
			return totalRows, err
		}
		totalRows += len(entries)
		s.logGameEvent(session, "game-skill-switch-chronicle-item-refresh-send",
			"source", source,
			"char_id", session.selectedCharacterID,
			"msg_id", uint16(dnfenum.CmdPacketWalkoutPartyMember),
			"list_type", listType,
			"row_count", len(entries),
			"sequence", "repository_raw77_chronicle_rows_before_op559")
	}
	return totalRows, nil
}

func currentSkillSwitchHasChronicleOptionVector(raw [currentItemListEntryWireSize]byte) bool {
	const (
		optionCountOffset = 0x27
		optionIDOffset    = 0x28
		optionCapacity    = 2
	)
	count := int(raw[optionCountOffset])
	if count < 1 || count > optionCapacity {
		return false
	}
	for index := 0; index < count; index++ {
		optionID := binary.LittleEndian.Uint32(raw[optionIDOffset+index*4 : optionIDOffset+(index+1)*4])
		if optionID < currentChronicleCanonicalAuraBase || optionID > currentChronicleCanonicalAuraBase+2 {
			return false
		}
	}
	return true
}

func buildSkillSwitchDeltaPositionBody(positions map[byte]skillSwitchInventoryPosition, parts []byte) []byte {
	seen := make(map[byte]struct{}, len(parts))
	ordered := append([]byte(nil), parts...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i] < ordered[j] })
	result := make([]byte, 1, 1+len(ordered)*5)
	for _, part := range ordered {
		if _, ok := seen[part]; ok || part > 32 {
			continue
		}
		seen[part] = struct{}{}
		if position, ok := positions[part]; ok {
			result = append(result, 1, part, position.ListType, 0, 0)
			binaryPutUint16(result[len(result)-2:], uint16(position.Slot))
		} else {
			result = append(result, 0, part, skillSwitchClearListType, 0xff, 0xff)
		}
	}
	result[0] = byte((len(result) - 1) / 5)
	return result
}

func binaryPutUint16(dst []byte, value uint16) { dst[0] = byte(value); dst[1] = byte(value >> 8) }
