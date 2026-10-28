package dnfbridge

import (
	"encoding/binary"
	"sort"
	"strings"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

const currentMode1EquipmentCreateRowBaseWireSize = 56
const currentMode1EquipmentRawBlockHeaderSize = 4
const currentMode1EquipmentRawBlockMaxLen = 1024
const currentMode1EquipmentVectorRecordSize = 8
const currentMode1EquipmentVectorRecordMaxCount = 2
const currentMode1EquipmentDwordVectorMaxCount = 2

const currentMode1EquipmentIndexedStateMaxRecords = 3

type currentMode1EquipmentIndexedStateRecord struct {
	value0 byte
	value1 byte
	value2 byte
}

type currentMode1EquipmentIndexedState struct {
	records             []currentMode1EquipmentIndexedStateRecord
	activeValue         byte
	selectedIndex       byte
	selectedValue       byte
	selectedRecord      currentMode1EquipmentIndexedStateRecord
	hasSelectedRecord   bool
	currentItemMismatch bool
}

type currentMode1EquipmentObjectRow struct {
	createEnabled              bool
	currentItemStateDerived    bool
	slot                       byte
	itemID                     uint32
	equipmentType              uint64
	equipmentTypeKnown         bool
	instance                   uint32
	extData                    byte
	durability                 uint16
	durabilityKnown            bool
	linkedItemID               uint32
	auxValue                   uint32
	auxByte                    byte
	bindFlag                   byte
	marker16                   uint16
	linkedRaw                  []byte
	readsRawBlocks             bool
	rawA                       []byte
	rawB                       []byte
	readsDurabilityOverrideU32 bool
	durabilityOverride         uint32
	vector1D6E020              []byte
	state112                   uint32
	vector225CCA0              []byte
	state1E636E0               uint16
	indexedState1C61F40        currentMode1EquipmentIndexedState
	state120                   byte
	state160                   byte
	state128                   byte
	state140                   uint16
	state144                   byte
	resourceByte               byte
	state168                   byte
	state169                   byte
	state170                   byte
	lateRawC                   []byte
	lateRawD                   []byte
	state183                   byte
	lateRawE                   []byte
	update                     currentItemListEntry
}

func (r *csharpLegacyUserInfoReader) currentMode1EquipmentObjectRows() []currentMode1EquipmentObjectRow {
	if r == nil || r.service == nil || strings.TrimSpace(r.characterID) == "" {
		return nil
	}
	repos, ok := r.service.projectionRepositories(r.ctx)
	if !ok || repos.Equipment == nil {
		return nil
	}
	record, found, err := repos.Equipment.Load(r.ctx, r.characterID)
	if err != nil {
		if r.session != nil {
			r.service.logGameEvent(r.session, "game-upper-selected-userinfo-mode1-equipment-load-failed", "character_id", r.characterID, "error", err)
		}
		return nil
	}
	if !found || len(record.Entries) == 0 {
		return nil
	}
	var petRecord dnfrepo.PetRecord
	if repos.Pet != nil {
		if loaded, petFound, petErr := repos.Pet.Load(r.ctx, r.characterID); petErr == nil && petFound {
			petRecord = loaded
		}
	}
	// 转职光环的 USERINFO 关联块要带**被覆盖光环实例**的镶嵌字节，所以这里必须
	// 读一次背包：穿戴时快照会在"穿好之后才打徽章"的场景下过期。缺记录/读失败都
	// 只意味着"没有可用的关联实例"，投影照常发出（不猜、不阻塞）。
	var inventorySlots map[string]dnfrepo.ItemStack
	if repos.Inventory != nil {
		if inv, invFound, invErr := repos.Inventory.Load(r.ctx, r.characterID); invErr == nil && invFound {
			inventorySlots = inv.Slots
		}
	}
	rows := make([]currentMode1EquipmentObjectRow, 0, len(record.Entries))
	for _, equipped := range record.Entries {
		if equipped.SlotIndex < 0 || equipped.SlotIndex > 0xff || equipped.ItemID <= 0 {
			continue
		}
		mergeCurrentPetEnchantFromRecord(&equipped, petRecord)
		entry, entryOK := currentItemListEntryFromEquipment(equipped)
		if !entryOK {
			continue
		}
		equipmentType, equipmentTypeKnown := r.currentMode1EquipmentType(equipped)
		durability, durabilityKnown := currentMode1EquipmentDurabilityWord(equipped, entry)
		indexedState := currentMode1EquipmentIndexedStateFromCurrentItem(entry)
		// Slot 8 is the sole skin-avatar slot.  Historical rows often lack the
		// optional PVF type label, but their mode1 reader still requires the
		// avatar detail blocks to materialize the full-body render.
		readsRawBlocks := currentMode1EquipmentReadsRawBlocks(equipped.Extra, equipmentType, equipmentTypeKnown) ||
			currentMode1EquipmentIsSkinAvatar(equipped)
		rawA, rawB := currentMode1EquipmentAvatarRawBlocks(equipped, entry, readsRawBlocks)
		rows = append(rows, currentMode1EquipmentObjectRow{
			createEnabled:           currentMode1EquipmentCreateEnabled(equipped),
			currentItemStateDerived: true,
			slot:                    byte(equipped.SlotIndex),
			itemID:                  sceneInventoryUint32FromInt64(equipped.ItemID),
			equipmentType:           equipmentType,
			equipmentTypeKnown:      equipmentTypeKnown,
			// sub_1D77560's create-row v74 field is the equipped object's
			// grade/instance value. A zero default happens to render as top
			// quality, so rebuilding an already-owned actor after op19 must
			// carry the same equipment quality seed as the authoritative
			// 0x77 item row. Keep the historical PVF starter projection at
			// zero because its create scalar is an initialization marker,
			// not a durable quality seed.
			instance:        currentMode1EquipmentGradeOrInstance(equipped),
			extData:         entry.data[0x0a],
			durability:      durability,
			durabilityKnown: durabilityKnown,
			// The link field (+12) carries whichever template this row must be
			// read against: a 转职光环 records the aura it covered, a 透明克隆
			// 时装 records the template it clones. The two keys never coexist on
			// one row, so the order only decides which is consulted first.
			linkedItemID:               sceneInventoryExtraUint32(equipped.Extra, dnfrepo.AuraLinkedTemplateExtraKey, dnfrepo.CloneAvatarAppearanceExtraKey, "mode1_linked_item_id", "mode1_clone_or_linked_id", "clone_or_linked_id"),
			auxValue:                   currentMode1EquipmentExtraUint32Or(equipped.Extra, binary.LittleEndian.Uint32(entry.data[0x0e:0x12]), "mode1_aux_value", "mode1_state_24", "state_24"),
			auxByte:                    currentMode1EquipmentExtraByteOr(equipped.Extra, entry.data[0x12], "mode1_aux_byte", "mode1_state_32", "state_32"),
			bindFlag:                   currentMode1EquipmentExtraByteOr(equipped.Extra, entry.data[0x13], "mode1_state_56", "state_56", "value_c"),
			marker16:                   currentMode1EquipmentExtraUint16Or(equipped.Extra, binary.LittleEndian.Uint16(entry.data[0x14:0x16]), "marker_16", "marker16", "value_d"),
			linkedRaw:                  currentMode1EquipmentLinkedRaw(equipped.Extra, inventorySlots),
			readsRawBlocks:             readsRawBlocks,
			rawA:                       rawA,
			rawB:                       rawB,
			readsDurabilityOverrideU32: currentMode1EquipmentReadsDurabilityOverrideU32(equipped.Extra, equipmentType, equipmentTypeKnown),
			durabilityOverride:         currentMode1EquipmentDurabilityOverride(equipped),
			vector1D6E020:              currentMode1EquipmentVectorRecords(equipped.Extra, "mode1_sub_1d6e020_records_hex", "mode1_vector_records_hex"),
			state112:                   currentMode1EquipmentState112(equipped, entry),
			// sub_1D77560 reads this through sub_225CCA0 and applies it to
			// ordinary equipment through the same vtable +280 method used
			// after op14. Derive it from the durable raw+0x3C vector so a
			// cold-login create row restores sockets without a wear cycle.
			vector225CCA0: currentMode1EquipmentDwordVectorFromCurrentItem(
				entry,
				equipped.Extra,
				"mode1_sub_225cca0_dwords_hex",
				"mode1_dword_vector_hex",
			),
			state1E636E0:        binary.LittleEndian.Uint16(entry.data[0x45:0x47]),
			indexedState1C61F40: indexedState,
			state120:            currentMode1EquipmentForgingLevel(equipped, entry),
			state160:            sceneInventoryExtraByte(equipped.Extra, "mode1_state_160", "state_160"),
			state128:            sceneInventoryExtraByte(equipped.Extra, "mode1_state_128", "state_128"),
			state140:            sceneInventoryExtraUint16(equipped.Extra, "mode1_state_140", "state_140"),
			state144:            sceneInventoryExtraByte(equipped.Extra, "mode1_state_144", "state_144"),
			resourceByte:        sceneInventoryExtraByte(equipped.Extra, "mode1_resource_byte", "resource_byte"),
			state168:            sceneInventoryExtraByte(equipped.Extra, "mode1_state_168", "state_168"),
			state169:            sceneInventoryExtraByte(equipped.Extra, "mode1_state_169", "state_169"),
			state170:            sceneInventoryExtraByte(equipped.Extra, "mode1_state_170", "state_170"),
			lateRawC:            currentMode1EquipmentExtraHex(equipped.Extra, currentMode1EquipmentRawBlockMaxLen, "mode1_raw_c_hex", "mode1_state_171_raw_hex"),
			lateRawD:            currentMode1EquipmentExtraHex(equipped.Extra, currentMode1EquipmentRawBlockMaxLen, "mode1_raw_d_hex", "mode1_state_175_179_raw_hex"),
			state183:            sceneInventoryExtraByte(equipped.Extra, "mode1_state_183", "state_183"),
			// The current EXE copies this final five-byte block into equipment
			// state +188..+192. Restore authoritative raw+0x72..0x76 on cold
			// login when no explicit mode1 alias exists.
			lateRawE: currentMode1EquipmentRawBlockOrCurrentItem(
				equipped.Extra,
				entry,
				currentEpicProductionAbilityTailOffset,
				currentEpicProductionAbilityTailOffset+currentEpicProductionAbilityTailSize,
				"mode1_raw_e_hex",
				"mode1_state_188_192_raw_hex",
			),
			update: entry,
		})
	}
	sort.Slice(rows, func(i, j int) bool {
		return rows[i].slot < rows[j].slot
	})
	if len(rows) > 0xff {
		rows = rows[:0xff]
	}
	if len(rows) > 0 && r.session != nil {
		r.service.logGameEvent(r.session, "game-upper-selected-userinfo-mode1-equipment-create-rows",
			"character_id", r.characterID,
			"entry_count", len(rows),
			"unknown_type_count", currentMode1EquipmentUnknownTypeCount(rows),
			"row_summary", currentMode1EquipmentRowSummary(rows),
			"mcp_handler", "sub_1D77560")
	}
	return rows
}

// currentMode1EquipmentLinkedRaw returns the payload written after the link
// field (+12) in a create row, i.e. the block the client reads via sub_3457C50.
//
// An explicit captured alias always wins. Otherwise a worn 转职光环 resolves the
// covered aura's socket bytes from the live inventory row the equip recorded, so
// emblems socketed AFTER the 转职光环 was worn are still carried. Everything else
// (clone avatars included) keeps the previous behaviour of emitting no block.
func currentMode1EquipmentLinkedRaw(extra map[string]string, slots map[string]dnfrepo.ItemStack) []byte {
	if raw := currentMode1EquipmentExtraHex(extra, currentMode1EquipmentRawBlockMaxLen, "mode1_linked_raw_hex", "mode1_clone_raw_hex"); len(raw) != 0 {
		return raw
	}
	return currentAuraLinkedSocketBlock(extra, slots)
}

// currentAuraLinkedSocketBlock resolves the covered aura INSTANCE's socket block
// for a worn 转职光环. The link field only carries the covered aura's template id,
// so the instance has to be looked up; a row that is gone, moved, or now holds a
// different template means the binding is dead and nothing is emitted. Never
// substitute another aura from the bag or a template guess.
func currentAuraLinkedSocketBlock(extra map[string]string, slots map[string]dnfrepo.ItemStack) []byte {
	templateID := sceneInventoryExtraUint32(extra, dnfrepo.AuraLinkedTemplateExtraKey)
	if templateID == 0 {
		return nil
	}
	sourceKey := strings.TrimSpace(extra[dnfrepo.AuraLinkedSourceExtraKey])
	if sourceKey == "" || len(slots) == 0 {
		return nil
	}
	stack, found := slots[sourceKey]
	if !found || stack.ItemID != int64(templateID) {
		return nil
	}
	return currentItemListAvatarSocketData(stack.Extra)
}

func currentMode1EquipmentRawBlockOrCurrentItem(
	extra map[string]string,
	entry currentItemListEntry,
	start int,
	end int,
	keys ...string,
) []byte {
	if data := currentMode1EquipmentExtraHex(extra, currentMode1EquipmentRawBlockMaxLen, keys...); len(data) != 0 {
		return data
	}
	if start < 0 || end <= start || end > len(entry.data) {
		return nil
	}
	for _, value := range entry.data[start:end] {
		if value != 0 {
			return append([]byte(nil), entry.data[start:end]...)
		}
	}
	return nil
}

func currentMode1EquipmentForgingLevel(equipped dnfrepo.EquipmentEntry, entry currentItemListEntry) byte {
	// Current NoPack sub_1D77560 passes this byte as the forgingLevel argument
	// to ConstructEquipmentState_FromProjectionRow. That constructor stores it
	// in the encoded equipment-property field at +0x78, which is exactly what
	// AppendWeaponForgingTooltipLines reads after a character reselect.
	return sceneInventoryExtraByteDefault(
		equipped.Extra,
		entry.data[currentItemListForgingLevelOffset],
		"forging_level", "byte_57", "value_57", "mode1_state_120", "state_120",
	)
}

const currentMode1SkinAvatarColorStateBytes = 4

func currentMode1EquipmentAvatarRawBlocks(equipped dnfrepo.EquipmentEntry, entry currentItemListEntry, readsRawBlocks bool) ([]byte, []byte) {
	extra := equipped.Extra
	rawA := currentMode1EquipmentExtraHex(extra, currentMode1EquipmentRawBlockMaxLen, "mode1_raw_a_hex", "mode1_sub_225c960_raw_hex", "mode1_raw_block_a_hex")
	rawB := currentMode1EquipmentExtraHex(extra, currentMode1EquipmentRawBlockMaxLen, "mode1_raw_b_hex", "mode1_sub_225c9b0_raw_hex", "mode1_raw_block_b_hex")
	if !readsRawBlocks {
		return rawA, rawB
	}
	// Current EXE sub_1D77560 uses the same two type<=11 extension readers as
	// list-1/op13 and the avatar branch of op14: socket state first, color state
	// second. Socket mutations persist those canonical blobs on the item row,
	// so use them as the mode1 create-state fallback instead of requiring a
	// second set of mode1-only aliases. Explicit captured mode1 blocks retain
	// precedence when present.
	if len(rawA) == 0 {
		rawA = append([]byte(nil), entry.avatarSocketData...)
	}
	if len(rawB) == 0 {
		rawB = append([]byte(nil), entry.avatarColorData...)
	}
	if currentMode1EquipmentIsSkinAvatar(equipped) {
		// A skin avatar is a complete character render rather than an
		// independent paper-doll part.  The current actor's mode-1 reader
		// distinguishes a materialized avatar detail from an absent one by the
		// two length-prefixed detail blocks.  A zero-length pair leaves slot 8
		// registered as an ordinary item and the renderer falls back to the
		// visible 0..7 avatar parts.  The roster/card path does not consume
		// these blocks, which is why it can still show the skin correctly.
		//
		// A newly granted skin has no emblem or dye data, but still needs its
		// canonical empty detail shape: five empty six-byte sockets and two
		// zero colour words.  Explicit captured/persisted mode-1 data above
		// always takes precedence.
		if len(rawA) == 0 {
			rawA = make([]byte, currentAvatarSocketBytes)
		}
		if len(rawB) == 0 {
			rawB = make([]byte, currentMode1SkinAvatarColorStateBytes)
		}
	}
	return rawA, rawB
}

func currentMode1EquipmentIsSkinAvatar(equipped dnfrepo.EquipmentEntry) bool {
	// The current actor's equipment table reserves slot 8 exclusively for the
	// skin-avatar render.  The PVF type string is helpful on newly granted
	// items but is absent on old/equipped rows imported before that metadata was
	// persisted, so it must not gate the wire shape.
	return equipped.SlotIndex == 8
}
