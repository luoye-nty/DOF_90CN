package dnfbridge

import (
	"context"
	"encoding/binary"
	"strconv"
	"strings"

	dnfprofession "longheng.io/server/internal/modules/dnf/profession"
	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

// skillSwitchRegisteredBuffLevelBonus returns the buff skill the character has
// registered in the BUFF-enhancement (换装) window and the total skill-level
// bonus that registration contributes to that skill.
//
// The bonus is computed from the durable registration (skill id + the bound
// equipment items), not from the client's live skill state. It is the value
// that must be folded into the mode1/mode3 skill pages so that party members,
// which simulate the caster's buff from those pages, see the same level the
// caster sees for itself.
func (s *Service) skillSwitchRegisteredBuffLevelBonus(
	ctx context.Context,
	session *gameSession,
	character dnfrepo.CharacterRecord,
) (uint16, int, error) {
	if s == nil || session == nil || session.selectedCharacterID == 0 {
		return 0, 0, nil
	}
	repos, ok := s.projectionRepositories(ctx)
	if !ok || repos.Settings == nil {
		return 0, 0, nil
	}
	characterID := strconv.FormatUint(uint64(session.selectedCharacterID), 10)
	state, err := loadSkillSwitchInventoryState(ctx, repos.Settings, characterID)
	if err != nil || state.SkillID == 0 || len(state.Items) == 0 {
		return 0, 0, err
	}
	job, ok := characterJobByte(character)
	if !ok {
		return state.SkillID, 0, nil
	}
	s.initialEquipmentMu.Lock()
	archive, archiveErr := s.initialEquipmentArchiveLocked()
	s.initialEquipmentMu.Unlock()
	if archiveErr != nil {
		return state.SkillID, 0, nil
	}
	rawJobTag, err := currentRentalCharacterJobTag(archive, job)
	if err != nil {
		return state.SkillID, 0, nil
	}
	jobTag := skillSwitchJobTag(rawJobTag)

	growType := numericCharacterStatValue(character, "grow_type")
	_, awakeningStage := dnfprofession.Decode(byte(growType & 0xff))

	catalog, err := s.currentPVFItemCatalog()
	if err != nil {
		return state.SkillID, 0, err
	}

	requiredLevel := 0
	active := false
	s.initialSkillsMu.Lock()
	if s.skillCatalog != nil {
		if skill, found := s.skillCatalog.Find(job, state.SkillID); found {
			requiredLevel = skill.RequiredLevel
			active = skill.Active
		}
	}
	s.initialSkillsMu.Unlock()

	// The registered item template contributes its own PVF bonus; the item
	// instance additionally carries enchant-card and emblem ids in its raw row.
	// Load the instance records so the embedded bonuses can be folded in too.
	var inventory dnfrepo.InventoryRecord
	var equipment dnfrepo.EquipmentRecord
	if repos.Inventory != nil {
		if record, _, loadErr := repos.Inventory.Load(ctx, characterID); loadErr == nil {
			inventory = record
		}
	}
	if repos.Equipment != nil {
		if record, _, loadErr := repos.Equipment.Load(ctx, characterID); loadErr == nil {
			equipment = record
		}
	}

	total := 0
	registeredParts := map[byte]bool{}
	for _, item := range state.Items {
		registeredParts[item.EquipmentPart] = true
		extra, raw, found := skillSwitchRegisteredItemInstance(item, inventory, equipment)
		if !found {
			bonus, itemErr := skillSwitchItemBuffLevelBonus(
				catalog, item.ItemID, jobTag, state.SkillID, requiredLevel, active, awakeningStage,
			)
			if itemErr != nil {
				continue
			}
			total += bonus
			continue
		}
		total += skillSwitchItemInstanceBuffBonus(
			catalog, item.ItemID, extra, raw, jobTag, state.SkillID, requiredLevel, active, awakeningStage,
		)
	}

	// The client also derives the caster's worn-equipment bonus from the mode1
	// equipment block. For a registered slot that worn bonus is applied in
	// addition to the skill-page total, so subtract it here to keep peers at
	// exactly the registered level instead of registered + worn.
	for _, equipped := range equipment.Entries {
		part := byte(equipped.SlotIndex)
		if !registeredParts[part] || equipped.ItemID <= 0 {
			continue
		}
		total -= skillSwitchItemInstanceBuffBonus(
			catalog, uint32(equipped.ItemID), equipped.Extra, equipped.RawEntry,
			jobTag, state.SkillID, requiredLevel, active, awakeningStage,
		)
	}
	return state.SkillID, total, nil
}

// skillSwitchItemInstanceBuffBonus sums the buff-skill level bonus carried by
// one equipment instance: its own PVF rows plus embedded enchant card, emblems
// and avatar ability.
func skillSwitchItemInstanceBuffBonus(
	catalog *pvfDungeonDropCatalog,
	itemID uint32,
	extra map[string]string,
	raw []byte,
	jobTag string,
	skillID uint16,
	requiredLevel int,
	active bool,
	awakeningStage byte,
) int {
	total := 0
	if bonus, err := skillSwitchItemBuffLevelBonus(catalog, itemID, jobTag, skillID, requiredLevel, active, awakeningStage); err == nil {
		total += bonus
	}
	cardID, hasCard := skillSwitchEnchantCardID(extra)
	if !hasCard {
		cardID, hasCard = skillSwitchLegacyEnchantCardID(catalog, raw)
	}
	if hasCard {
		total += skillSwitchEnchantCardBuffLevelBonus(
			catalog, cardID, skillSwitchEnchantUpgradeCount(extra),
			jobTag, skillID, requiredLevel, active, awakeningStage,
		)
	}
	for _, emblemID := range skillSwitchEmblemItemIDs(extra, raw) {
		if emblemID == 0 || emblemID == ^uint32(0) {
			continue
		}
		total += skillSwitchEmblemBuffLevelBonus(
			catalog, emblemID, jobTag, skillID, requiredLevel, active, awakeningStage,
		)
	}
	if abilityNo, ok := skillSwitchAvatarAbilityNo(extra); ok {
		total += skillSwitchAvatarAbilitySkillBonus(catalog, itemID, abilityNo, jobTag, skillID)
	}
	return total
}

// skillSwitchItemBuffLevelBonus resolves one registered item and returns the
// buff-skill level bonus carried by its PVF definition.
func skillSwitchItemBuffLevelBonus(
	catalog *pvfDungeonDropCatalog,
	itemID uint32,
	jobTag string,
	skillID uint16,
	requiredLevel int,
	active bool,
	awakeningStage byte,
) (int, error) {
	doc, ok := skillSwitchResolveItemDoc(catalog, itemID)
	if !ok {
		return 0, nil
	}
	return skillSwitchDocumentBuffLevelBonus(doc, jobTag, skillID, requiredLevel, active, awakeningStage), nil
}

// skillSwitchEnchantCardBuffLevelBonus sums the enchant card's `[enchant
// index]` skill-level rows for the equipment's current upgrade stage. If the
// exact stage is absent the base stage (index 0) is used.
func skillSwitchEnchantCardBuffLevelBonus(
	catalog *pvfDungeonDropCatalog,
	cardID uint32,
	upgrade byte,
	jobTag string,
	skillID uint16,
	requiredLevel int,
	active bool,
	awakeningStage byte,
) int {
	doc, ok := skillSwitchResolveItemDoc(catalog, cardID)
	if !ok {
		return 0
	}
	type indexBonus struct {
		index    int64
		sections []dnfpvf.Section
	}
	var stages []indexBonus
	inIndex := false
	curIndex := int64(-1)
	var curSections []dnfpvf.Section
	for _, section := range doc.Sections {
		name := sectionName(section.Name)
		switch name {
		case "enchant index":
			inIndex = true
			curIndex = -1
			curSections = nil
			if value, ok := firstSectionInt(doc, section); ok {
				curIndex = value
			}
			continue
		case "/enchant index":
			if inIndex && curIndex >= 0 {
				stages = append(stages, indexBonus{index: curIndex, sections: curSections})
			}
			inIndex = false
			curSections = nil
			continue
		default:
			if inIndex {
				curSections = append(curSections, section)
			}
		}
	}
	for _, stage := range stages {
		if stage.index == int64(upgrade) {
			return skillSwitchSectionsSkillBonus(doc, stage.sections, jobTag, skillID, requiredLevel, active, awakeningStage)
		}
	}
	for _, stage := range stages {
		if stage.index == 0 {
			return skillSwitchSectionsSkillBonus(doc, stage.sections, jobTag, skillID, requiredLevel, active, awakeningStage)
		}
	}
	return 0
}

// skillSwitchEmblemBuffLevelBonus sums a socketed emblem's `[enchant]`
// skill-level rows.
func skillSwitchEmblemBuffLevelBonus(
	catalog *pvfDungeonDropCatalog,
	emblemID uint32,
	jobTag string,
	skillID uint16,
	requiredLevel int,
	active bool,
	awakeningStage byte,
) int {
	doc, ok := skillSwitchResolveItemDoc(catalog, emblemID)
	if !ok {
		return 0
	}
	var sections []dnfpvf.Section
	enchantDepth := 0
	for _, section := range doc.Sections {
		name := sectionName(section.Name)
		switch name {
		case "enchant":
			enchantDepth++
			continue
		case "/enchant":
			if enchantDepth > 0 {
				enchantDepth--
			}
			continue
		default:
			if enchantDepth > 0 {
				sections = append(sections, section)
			}
		}
	}
	return skillSwitchSectionsSkillBonus(doc, sections, jobTag, skillID, requiredLevel, active, awakeningStage)
}

// skillSwitchResolveItemDoc loads and parses one item's PVF document.
func skillSwitchResolveItemDoc(catalog *pvfDungeonDropCatalog, itemID uint32) (*dnfpvf.Document, bool) {
	if catalog == nil || itemID == 0 {
		return nil, false
	}
	definition, err := catalog.ResolveItem(itemID)
	if err != nil || definition.PVFPath == "" || catalog.source == nil {
		return nil, false
	}
	text, err := catalog.source.ReadText(definition.PVFPath)
	if err != nil {
		return nil, false
	}
	doc, err := dnfpvf.Parse(definition.PVFPath, text)
	if err != nil {
		return nil, false
	}
	return doc, true
}

// skillSwitchRegisteredItemInstance locates the durable inventory/equipment
// instance bound by one registration entry and returns its extra fields and raw
// row. Registration binds by durable item serial, never by grid cell.
func skillSwitchRegisteredItemInstance(
	binding skillSwitchBinding,
	inventory dnfrepo.InventoryRecord,
	equipment dnfrepo.EquipmentRecord,
) (map[string]string, []byte, bool) {
	for _, stack := range inventory.Slots {
		if uint32(stack.ItemID) != binding.ItemID {
			continue
		}
		if skillSwitchItemSerial(stack.Extra, stack.RawEntry) != binding.ItemSerial {
			continue
		}
		return stack.Extra, stack.RawEntry, true
	}
	for _, stack := range inventory.Warehouse {
		if uint32(stack.ItemID) != binding.ItemID {
			continue
		}
		if skillSwitchItemSerial(stack.Extra, stack.RawEntry) != binding.ItemSerial {
			continue
		}
		return stack.Extra, stack.RawEntry, true
	}
	for _, entry := range equipment.Entries {
		if uint32(entry.ItemID) != binding.ItemID {
			continue
		}
		if currentSkillSwitchEquipmentSerial(entry) != binding.ItemSerial {
			continue
		}
		return entry.Extra, entry.RawEntry, true
	}
	return nil, nil, false
}

// skillSwitchEnchantCardID reads the enchanted monster-card item id from the
// instance extra fields.
func skillSwitchEnchantCardID(extra map[string]string) (uint32, bool) {
	for _, key := range []string{"enchant_card_id", "pet_enchant_card_item_id"} {
		raw := strings.TrimSpace(extra[key])
		if raw == "" || raw == "0" {
			continue
		}
		if id, err := strconv.ParseUint(raw, 10, 32); err == nil && id != 0 {
			return uint32(id), true
		}
	}
	return 0, false
}

// skillSwitchLegacyEnchantCardID recovers the enchant card id from a legacy
// instance whose extra fields predate enchant_card_id but whose raw row still
// carries the card id at raw+0x0E. The candidate is verified to resolve as a
// PVF item so an ordinary quality seed at the same offset is not mistaken for
// a card.
func skillSwitchLegacyEnchantCardID(catalog *pvfDungeonDropCatalog, raw []byte) (uint32, bool) {
	if catalog == nil || len(raw) < 0x12 {
		return 0, false
	}
	candidate := binary.LittleEndian.Uint32(raw[0x0E:0x12])
	if candidate == 0 || candidate == ^uint32(0) {
		return 0, false
	}
	if _, err := catalog.ResolveItem(candidate); err != nil {
		return 0, false
	}
	return candidate, true
}

// skillSwitchEnchantUpgradeCount reads the enchant upgrade stage.
func skillSwitchEnchantUpgradeCount(extra map[string]string) byte {
	for _, key := range []string{"enchant_upgrade_count", "byte_12"} {
		raw := strings.TrimSpace(extra[key])
		if raw == "" {
			continue
		}
		if value, err := strconv.ParseUint(raw, 10, 8); err == nil {
			return byte(value)
		}
	}
	return 0
}

// skillSwitchEmblemItemIDs reads the socketed emblem item ids from the
// instance extra vector, falling back to the current raw+0x3C vector.
func skillSwitchEmblemItemIDs(extra map[string]string, raw []byte) []uint32 {
	if data, ok := currentEquipmentEmblemDataFromExtra(extra); ok {
		return skillSwitchEmblemIDsFromData(data)
	}
	if len(raw) >= currentEquipmentVectorOffset+9 {
		count := int(raw[currentEquipmentVectorOffset])
		if count > 0 && count <= 2 {
			ids := make([]uint32, 0, count)
			ids = append(ids, binary.LittleEndian.Uint32(raw[currentEquipmentVectorOffset+1:currentEquipmentVectorOffset+5]))
			if count > 1 {
				ids = append(ids, binary.LittleEndian.Uint32(raw[currentEquipmentVectorOffset+5:currentEquipmentVectorOffset+9]))
			}
			return ids
		}
	}
	// Legacy rows stored the emblem vector at raw+0x2F before the current
	// raw+0x3C collection existed.
	if data, ok := currentEquipmentLegacyEmblemDataFromRaw(raw); ok {
		return skillSwitchEmblemIDsFromData(data)
	}
	return nil
}

func skillSwitchEmblemIDsFromData(data [currentEquipmentEmblemDataBytes]byte) []uint32 {
	count := int(data[0])
	if count <= 0 || count > 2 {
		return nil
	}
	ids := make([]uint32, 0, count)
	if count >= 1 {
		ids = append(ids, binary.LittleEndian.Uint32(data[1:5]))
	}
	if count >= 2 {
		ids = append(ids, binary.LittleEndian.Uint32(data[5:9]))
	}
	return ids
}

const skillSwitchAbilityDataPath = "skill/abilitydatas.dat"

// skillSwitchAvatarAbilityNo reads the avatar's selected ability number from
// its instance extra. The field is only present after an ability selection, so
// a missing field means no skill bonus is active.
func skillSwitchAvatarAbilityNo(extra map[string]string) (byte, bool) {
	raw := strings.TrimSpace(extra["avatar_ability_no"])
	if raw == "" {
		return 0, false
	}
	value, err := strconv.ParseUint(raw, 10, 8)
	if err != nil {
		return 0, false
	}
	return byte(value), true
}

// skillSwitchAvatarAbilitySkillBonus resolves an avatar's selected ability
// through its [ability case index] into skill/abilitydatas.dat and returns the
// buff-skill level it grants, if the selected ability targets that buff skill.
func skillSwitchAvatarAbilitySkillBonus(
	catalog *pvfDungeonDropCatalog,
	itemID uint32,
	abilityNo byte,
	jobTag string,
	buffSkillID uint16,
) int {
	doc, ok := skillSwitchResolveItemDoc(catalog, itemID)
	if !ok {
		return 0
	}
	caseIndex, found := doc.Int("ability case index")
	if !found {
		return 0
	}
	dataText, err := catalog.source.ReadText(skillSwitchAbilityDataPath)
	if err != nil {
		return 0
	}
	dataDoc, err := dnfpvf.Parse(skillSwitchAbilityDataPath, dataText)
	if err != nil {
		return 0
	}
	for _, section := range dataDoc.Sections {
		if sectionName(section.Name) != "ability case" {
			continue
		}
		tokens := sectionTokens(dataDoc, section)
		if len(tokens) < 2 || tokens[0].Kind != dnfpvf.TokenInt || tokens[0].Int != caseIndex {
			continue
		}
		for index := 1; index+4 < len(tokens); index += 5 {
			if tokens[index].Kind != dnfpvf.TokenInt || tokens[index].Int != int64(abilityNo) {
				continue
			}
			if tokens[index+2].Kind != dnfpvf.TokenString && tokens[index+2].Kind != dnfpvf.TokenIdent {
				continue
			}
			if tokens[index+3].Kind != dnfpvf.TokenInt || tokens[index+4].Kind != dnfpvf.TokenInt {
				continue
			}
			if tokens[index+3].Int <= 0 || tokens[index+3].Int > 0xffff || uint16(tokens[index+3].Int) != buffSkillID {
				continue
			}
			normJob := skillSwitchJobTag(tokens[index+2].Value)
			if normJob != "all" && normJob != "common" && normJob != jobTag {
				continue
			}
			return int(tokens[index+4].Int)
		}
	}
	return 0
}

// skillSwitchDocumentBuffLevelBonus sums the buff-skill level bonus declared by
// one item PVF document's own (non-embedded) sections for the given character
// job tag and buff skill.
//
// Two PVF forms are recognized:
//
//  1. Exact rows in [skill levelup]: `<job> <skill_id> <level>` triples. The
//     level may be negative (some weapons carry a -1 penalty).
//  2. Range rows inside [all skill item container]/[all skill item]:
//     `[item growtype] <job> <stage>` scopes the block, and
//     `[skill apply condition]` carries `[skill group]`, `[lower bound level]`,
//     `[upper bound level]` and `[value]`; a learned skill whose required level
//     falls inside the bounds gains `value` levels.
//
// [skill levelup] rows nested inside enchant/emblem containers describe the
// bonus the item grants *when embedded into another item*, not the item's own
// worn bonus, so those rows are skipped here (the embedded bonus reaches the
// worn item through its own raw option vector instead).
func skillSwitchDocumentBuffLevelBonus(
	doc *dnfpvf.Document,
	jobTag string,
	skillID uint16,
	requiredLevel int,
	active bool,
	awakeningStage byte,
) int {
	return skillSwitchSectionsSkillBonus(doc, skillSwitchTopLevelSections(doc), jobTag, skillID, requiredLevel, active, awakeningStage)
}

// skillSwitchTopLevelSections returns the document sections outside any
// enchant/enchant-table/enchant-index container.
func skillSwitchTopLevelSections(doc *dnfpvf.Document) []dnfpvf.Section {
	if doc == nil {
		return nil
	}
	var out []dnfpvf.Section
	embedDepth := 0
	for _, section := range doc.Sections {
		name := sectionName(section.Name)
		switch name {
		case "enchant", "enchant table", "enchant index":
			embedDepth++
			continue
		case "/enchant", "/enchant table", "/enchant index":
			if embedDepth > 0 {
				embedDepth--
			}
			continue
		}
		if embedDepth == 0 {
			out = append(out, section)
		}
	}
	return out
}

// skillSwitchSectionsSkillBonus sums both exact `[skill levelup]` rows and
// `[all skill item]` range rows across an arbitrary section slice. Callers
// pre-select the slice (a document's top-level sections, one enchant index, or
// one enchant block), which is what makes the embedded bonuses reusable.
func skillSwitchSectionsSkillBonus(
	doc *dnfpvf.Document,
	sections []dnfpvf.Section,
	jobTag string,
	skillID uint16,
	requiredLevel int,
	active bool,
	awakeningStage byte,
) int {
	total := 0

	groupDepth := 0
	groupMatched := false

	inApply := false
	applyGroupActiveOnly := false
	applyLower := int64(0)
	applyUpper := int64(0)
	applyValue := int64(0)
	applyHasLower := false
	applyHasUpper := false
	applyHasValue := false

	for _, section := range sections {
		name := sectionName(section.Name)
		switch name {
		case "all skill item":
			groupDepth++
			groupMatched = false
			continue
		case "/all skill item":
			if groupDepth > 0 {
				groupDepth--
				groupMatched = false
			}
			continue
		case "item growtype":
			if groupDepth > 0 {
				groupMatched = skillSwitchItemGrowTypeMatches(doc, section, jobTag, awakeningStage)
			}
			continue
		case "skill apply condition":
			if groupDepth > 0 && groupMatched {
				inApply = true
				applyGroupActiveOnly = false
				applyLower, applyUpper, applyValue = 0, 0, 0
				applyHasLower, applyHasUpper, applyHasValue = false, false, false
			}
			continue
		case "/skill apply condition":
			if inApply {
				if applyHasLower && applyHasUpper && applyHasValue &&
					(!applyGroupActiveOnly || active) &&
					requiredLevel > 0 &&
					int64(requiredLevel) >= applyLower &&
					int64(requiredLevel) <= applyUpper {
					total += int(applyValue)
				}
				inApply = false
			}
			continue
		case "skill group":
			if inApply {
				if value, ok := firstSectionText(doc, section); ok {
					applyGroupActiveOnly = skillSwitchJobTag(value) == "active"
				}
			}
			continue
		case "lower bound level":
			if inApply {
				if value, ok := firstSectionInt(doc, section); ok {
					applyLower = value
					applyHasLower = true
				}
			}
			continue
		case "upper bound level":
			if inApply {
				if value, ok := firstSectionInt(doc, section); ok {
					applyUpper = value
					applyHasUpper = true
				}
			}
			continue
		case "value":
			if inApply {
				if value, ok := firstSectionInt(doc, section); ok {
					applyValue = value
					applyHasValue = true
				}
			}
			continue
		case "skill levelup":
			total += skillSwitchSkillLevelupBonus(doc, section, jobTag, skillID)
			continue
		}
	}
	return total
}

// skillSwitchItemGrowTypeMatches reports whether an [item growtype] block
// applies to the character: the job tag must match (or be [all]) and the block
// stage must be no higher than the character's awakening stage. Stage 0 blocks
// always apply; higher stages apply cumulatively, matching the profession
// grant logic that merges every stage from 1 through the character's stage.
func skillSwitchItemGrowTypeMatches(
	doc *dnfpvf.Document,
	section dnfpvf.Section,
	jobTag string,
	awakeningStage byte,
) bool {
	tokens := sectionTokens(doc, section)
	var blockJob string
	var stage int64 = -1
	hasStage := false
	for _, token := range tokens {
		if (token.Kind == dnfpvf.TokenString || token.Kind == dnfpvf.TokenIdent) && blockJob == "" {
			blockJob = token.Value
			continue
		}
		if token.Kind == dnfpvf.TokenInt && !hasStage {
			stage = token.Int
			hasStage = true
			continue
		}
	}
	if blockJob == "" {
		return false
	}
	normJob := skillSwitchJobTag(blockJob)
	if normJob != "all" && normJob != jobTag {
		return false
	}
	if hasStage && stage > 0 && int64(awakeningStage) < stage {
		return false
	}
	return true
}

// skillSwitchSkillLevelupBonus sums exact `<job> <skill_id> <level>` rows in a
// single [skill levelup] section whose job and skill id match the character.
func skillSwitchSkillLevelupBonus(
	doc *dnfpvf.Document,
	section dnfpvf.Section,
	jobTag string,
	skillID uint16,
) int {
	tokens := sectionTokens(doc, section)
	total := 0
	for index := 0; index+2 < len(tokens); index += 3 {
		jobToken := tokens[index]
		if jobToken.Kind != dnfpvf.TokenString && jobToken.Kind != dnfpvf.TokenIdent {
			continue
		}
		skillToken := tokens[index+1]
		levelToken := tokens[index+2]
		if skillToken.Kind != dnfpvf.TokenInt || levelToken.Kind != dnfpvf.TokenInt {
			continue
		}
		if skillToken.Int <= 0 || skillToken.Int > 0xffff || uint16(skillToken.Int) != skillID {
			continue
		}
		normJob := skillSwitchJobTag(jobToken.Value)
		if normJob != "all" && normJob != jobTag {
			continue
		}
		total += int(levelToken.Int)
	}
	return total
}

func sectionName(value string) string {
	return strings.ToLower(strings.Join(strings.Fields(strings.TrimSpace(value)), " "))
}

func sectionTokens(doc *dnfpvf.Document, section dnfpvf.Section) []dnfpvf.Token {
	if doc == nil || section.Start < 0 || section.End < section.Start || section.End > len(doc.Tokens) {
		return nil
	}
	return doc.Tokens[section.Start:section.End]
}

func firstSectionInt(doc *dnfpvf.Document, section dnfpvf.Section) (int64, bool) {
	for _, token := range sectionTokens(doc, section) {
		if token.Kind == dnfpvf.TokenInt {
			return token.Int, true
		}
	}
	return 0, false
}

func firstSectionText(doc *dnfpvf.Document, section dnfpvf.Section) (string, bool) {
	for _, token := range sectionTokens(doc, section) {
		if token.Kind == dnfpvf.TokenString || token.Kind == dnfpvf.TokenIdent {
			return token.Value, true
		}
	}
	return "", false
}
