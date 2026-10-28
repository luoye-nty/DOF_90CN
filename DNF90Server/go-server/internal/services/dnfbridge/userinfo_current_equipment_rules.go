package dnfbridge

import (
	"strconv"
	"strings"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

func (r *csharpLegacyUserInfoReader) currentMode1EquipmentType(equipped dnfrepo.EquipmentEntry) (uint64, bool) {
	return currentEXEActorEquipmentSlot(equipped)
}

func currentEXEActorEquipmentSlot(equipped dnfrepo.EquipmentEntry) (uint64, bool) {
	// Builds before the dual-weapon metadata fix changed SlotIndex/map keys but
	// left current_exe_equipment_type on the opposite endpoint. Prefer the
	// authoritative location for this exact runtime 12<->24 mismatch so already
	// affected Vagabonds recover on their next character selection.
	if currentEXEDualWeaponSlotOverridesStaleMarker(equipped) {
		return uint64(equipped.SlotIndex), true
	}
	if value, ok := sceneInventoryExtraUint(equipped.Extra, "current_exe_equipment_type"); ok {
		if !isPVFInitialEquipment(equipped) || sceneInventoryExtraByte(equipped.Extra, "current_exe_runtime_move") != 0 {
			return value, true
		}
	}
	// Current runtime equipment records use the same direct base appearance
	// slots 0..13 as mode0. Only untouched PVF starter rows retain the legacy
	// worn-slot numbering translated below.
	if !isPVFInitialEquipment(equipped) &&
		equipped.SlotIndex >= 0 &&
		equipped.SlotIndex < currentActorMode0AppearanceSlotCount {
		return uint64(equipped.SlotIndex), true
	}
	// Legacy equipped-creature rows predate current_exe_runtime_move but carry
	// the same current slot plus a concrete creature identity. This is enough
	// to preserve slot 26 in a complete op342 group without treating arbitrary
	// 26..29 records as ordinary equipment.
	if equipped.SlotIndex == 26 {
		equipmentSlot, hasEquipmentSlot := sceneInventoryExtraUint(
			equipped.Extra,
			"equipment_slot",
			"equipped_slot",
		)
		creatureKey, hasCreatureKey := sceneInventoryExtraUint(
			equipped.Extra,
			"creature_key",
			"creature_serial_or_handle",
		)
		if hasEquipmentSlot && equipmentSlot == 26 &&
			hasCreatureKey && creatureKey > 0 {
			return 26, true
		}
	}
	return currentEXEActorEquipmentSlotForWornSlot(equipped.SlotIndex)
}

func currentEXEDualWeaponSlotOverridesStaleMarker(equipped dnfrepo.EquipmentEntry) bool {
	if sceneInventoryExtraByte(equipped.Extra, "current_exe_runtime_move") == 0 {
		return false
	}
	marker, ok := sceneInventoryExtraUint(equipped.Extra, "current_exe_equipment_type")
	if !ok {
		return false
	}
	return (equipped.SlotIndex == 12 && marker == 24) ||
		(equipped.SlotIndex == 24 && marker == 12)
}

func currentEXEActorEquipmentSlotForWornSlot(slot int16) (uint64, bool) {
	switch {
	case slot == 11:
		return 12, true
	case slot >= 13 && slot <= 22:
		return uint64(slot + 1), true
	case slot == 23:
		return 25, true
	case slot == 32:
		return 32, true
	default:
		return 0, false
	}
}

func currentMode1EquipmentCreateRows(rows []currentMode1EquipmentObjectRow) []currentMode1EquipmentObjectRow {
	if len(rows) == 0 {
		return nil
	}
	createRows := make([]currentMode1EquipmentObjectRow, 0, len(rows))
	for _, row := range rows {
		if row.createEnabled && currentMode1EquipmentCreateRowHasVerifiedState(row) {
			createRows = append(createRows, row)
		}
	}
	return createRows
}

func currentMode1EquipmentDeferredUpdateRows(rows []currentMode1EquipmentObjectRow) []currentMode1EquipmentObjectRow {
	if len(rows) == 0 {
		return nil
	}
	updateRows := make([]currentMode1EquipmentObjectRow, 0, len(rows))
	for _, row := range rows {
		if row.createEnabled && currentMode1EquipmentCreateRowHasVerifiedState(row) &&
			!currentMode1EquipmentRowNeedsDeferredStateRow(row) {
			continue
		}
		updateRows = append(updateRows, row)
	}
	return updateRows
}

// currentMode1EquipmentRowNeedsDeferredStateRow 报告这一行除了 create 行之外，还必须
// 额外发一条 mode1 的延时更新行（sub_2007B00，逐字节携带完整 0x77 物品行）。
//
// 为什么必须有它（2026-09-25 现场：称号附魔进图就消失）：
//
// writeCurrentMode1EquipmentCreateRow 只从 0x77 行里读三段——
// [0x0A,0x16)、[0x38,0x58)、[0x72,0x77)；随机称号附魔的 +0x61..+0x65 落在它不读的
// [0x58,0x72) 里。角色身上装备对象的建立只有 mode1/mode3 的 create 行这一条路（本部署
// 的穿戴栏 list-3 行从不单独下发，见 packet_log 全量统计），所以每次 actor 重建
// （进图、回城、选角）附魔都会被重建掉：客户端上附魔显示消失，它带来的技能等级同时失效。
//
// 邻居已经因为同一个洞补过一次：守护者宝石 socket 字在 +0x65..+0x6C（同样落在
// [0x58,0x72)），走的是"mode1 之后补一条 list-3 op14"（guardian_gem_refresh.go 与
// scene_post_start_map.go 的注释写明了 "creation rows … do not carry generic raw 0x77
// offsets"）。称号附魔以前谁都没补。
//
// 本函数走的是同一问题的另一条已验证通路：mode1 包里本来就有一个延时更新行区
// （sub_2007B00），它的每一行就是完整 0x77 行。这里**只做加法**——命中的行照旧进 create
// 列表（对象必须照原样被建立，绝不能因为改判定而让装备凭空消失），只是额外再进一次
// update 列表。因此 create 列表、其它任何行、任何其它流程都不受影响。
func currentMode1EquipmentRowNeedsDeferredStateRow(row currentMode1EquipmentObjectRow) bool {
	if !row.currentItemStateDerived {
		return false
	}
	return currentItemListEntryCarriesRandomTitleEnchant(row.update)
}

func currentMode1EquipmentCreateRowHasVerifiedState(row currentMode1EquipmentObjectRow) bool {
	if _, ok := currentMode1EquipmentActorSlot(row); !ok || !row.durabilityKnown || row.itemID == 0 {
		return false
	}
	// The current actor slot also selects the item-table conditional readers.
	if _, ok := currentMode1EquipmentRuntimeType(row); !ok {
		return false
	}
	if row.indexedState1C61F40.currentItemMismatch {
		return false
	}
	// Current op25 and sub_21792A0 prove that the plain mode1 detail fields are
	// the same values carried by the current 0x77 item record. This branch is
	// intentionally limited to a detail state that can be reproduced exactly.
	if row.currentItemStateDerived && !row.readsRawBlocks && !row.readsDurabilityOverrideU32 &&
		row.linkedItemID == 0 && row.auxValue == 0 && row.auxByte == 0 && row.bindFlag == 0 && row.marker16 == 0 &&
		len(row.vector1D6E020) == 0 && row.state112 == 0 && len(row.vector225CCA0) == 0 &&
		row.state120 == 0 && row.state160 == 0 && row.state128 == 0 && row.state140 == 0 &&
		row.state144 == 0 && row.resourceByte == 0 && row.state168 == 0 && row.state169 == 0 &&
		row.state170 == 0 && len(row.lateRawC) == 0 && len(row.lateRawD) == 0 &&
		row.state183 == 0 && len(row.lateRawE) == 0 {
		return true
	}
	if (row.readsRawBlocks && (len(row.rawA) > 0 || len(row.rawB) > 0)) ||
		(row.linkedItemID != 0 && row.slot == 9 && len(row.linkedRaw) > 0) ||
		len(row.vector1D6E020) > 0 || len(row.vector225CCA0) > 0 ||
		len(row.lateRawC) > 0 || len(row.lateRawD) > 0 || len(row.lateRawE) > 0 {
		return true
	}
	return row.linkedItemID != 0 || row.auxValue != 0 || row.auxByte != 0 ||
		row.state112 != 0xffffffff ||
		row.state120 != 0 || row.state160 != 0 || row.state128 != 0 ||
		row.state140 != 0 || row.state144 != 0 || row.resourceByte != 0 ||
		row.state168 != 0 || row.state169 != 0 || row.state170 != 0 || row.state183 != 0
}

func currentMode1EquipmentCreateEnabled(equipped dnfrepo.EquipmentEntry) bool {
	extra := equipped.Extra
	for _, key := range []string{"mode1_create_enabled", "mode1_create_row_verified", "current_exe_mode1_create_verified"} {
		raw := strings.TrimSpace(strings.ToLower(extra[key]))
		switch raw {
		case "1", "true", "yes", "on":
			return true
		case "0", "false", "no", "off":
			return false
		}
	}
	// The working Python implementation creates every equipped row in mode1.
	// Go previously defaulted this to untouched legacy starter slots only.
	// After op19 normalized those rows, or for runtime/rental equipment, mode1
	// emitted update rows without first creating actor equipment objects.
	// Preserve explicit overrides and the verified-state gate, but enable every
	// row whose current actor slot is known.
	_, ok := currentEXEActorEquipmentSlot(equipped)
	return ok
}

func currentMode1EquipmentReadsRawBlocks(extra map[string]string, equipmentType uint64, equipmentTypeKnown bool) bool {
	for _, key := range []string{"mode1_reads_raw_blocks", "current_exe_reads_equipment_raw_blocks"} {
		raw := strings.TrimSpace(strings.ToLower(extra[key]))
		switch raw {
		case "1", "true", "yes", "on":
			return true
		case "0", "false", "no", "off":
			return false
		}
	}
	if equipmentTypeKnown {
		return equipmentType <= 0x0b
	}
	return false
}

func currentMode1EquipmentUnknownTypeCount(rows []currentMode1EquipmentObjectRow) int {
	count := 0
	for _, row := range rows {
		if !row.equipmentTypeKnown {
			count++
		}
	}
	return count
}

func currentMode1EquipmentRowSummary(rows []currentMode1EquipmentObjectRow) string {
	if len(rows) == 0 {
		return ""
	}
	parts := make([]string, 0, len(rows))
	for _, row := range rows {
		typePart := "unknown"
		if row.equipmentTypeKnown {
			typePart = strconv.FormatUint(row.equipmentType, 10)
		}
		parts = append(parts, strconv.Itoa(int(row.slot))+":"+strconv.FormatUint(uint64(row.itemID), 10)+":type="+typePart+":durability="+strconv.Itoa(int(row.durability))+":derived="+boolDigit(row.currentItemStateDerived)+":raw="+boolDigit(row.readsRawBlocks)+":create="+boolDigit(row.createEnabled))
	}
	return strings.Join(parts, ";")
}

func boolDigit(value bool) string {
	if value {
		return "1"
	}
	return "0"
}

func currentMode1EquipmentReadsDurabilityOverrideU32(extra map[string]string, equipmentType uint64, equipmentTypeKnown bool) bool {
	for _, key := range []string{"mode1_reads_durability_override_u32", "current_exe_reads_type26_u32"} {
		raw := strings.TrimSpace(strings.ToLower(extra[key]))
		switch raw {
		case "1", "true", "yes", "on":
			return true
		case "0", "false", "no", "off":
			return false
		}
	}
	if equipmentTypeKnown {
		return equipmentType == 26
	}
	return false
}

func currentMode1EquipmentGradeOrInstance(equipped dnfrepo.EquipmentEntry) uint32 {
	if value, ok := sceneInventoryExtraUint(equipped.Extra, "mode1_v74", "mode1_grade_or_instance"); ok {
		return sceneInventoryClampUint32(value)
	}
	if value := currentEquippedCreatureInstance(equipped); value != 0 {
		return value
	}
	if isPVFInitialEquipment(equipped) {
		return 0
	}
	if seed := sceneInventoryExtraUint32(equipped.Extra, "quality_seed"); validCurrentEquipmentQualitySeed(seed) {
		return seed
	}
	if !currentEquipmentEntryHasCurrentRaw77(equipped) {
		return 0
	}
	value := currentItemListEquipmentInstance(equipped)
	if !validCurrentEquipmentQualitySeed(value) {
		return 0
	}
	return value
}
