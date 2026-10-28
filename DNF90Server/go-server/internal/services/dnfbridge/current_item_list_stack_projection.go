package dnfbridge

import (
	"encoding/binary"
	"strings"
	"time"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

func currentItemListEntriesFromMap(items map[string]dnfrepo.ItemStack, listType byte) []currentItemListEntry {
	if len(items) == 0 {
		return nil
	}
	entries := make([]currentItemListEntry, 0, len(items))
	for key, stack := range items {
		keyListType, slot, ok := parseSceneInventorySlotKey(key)
		goldWallet := keyListType == dnfrepo.MainInventoryListType && slot == 0 && stack.ItemID == 0
		if !ok || keyListType != listType || slot < 0 || (stack.ItemID <= 0 && !goldWallet) {
			continue
		}
		entries = append(entries, currentItemListEntryFromStack(listType, slot, stack))
	}
	return entries
}

func currentItemListEntryFromStack(listType byte, slot int16, stack dnfrepo.ItemStack) currentItemListEntry {
	if listType == dnfrepo.MainInventoryListType && slot == 0 && stack.ItemID == 0 {
		return currentGoldWalletItemListEntry(stack.Count)
	}
	extra := stack.Extra
	entry := currentItemListEntryFromRaw(append([]byte(nil), stack.RawEntry...), extra)
	overlayCurrentRandomTitleEnchantEntry(&entry, stack.Extra)
	entry.patchCore(slot, sceneInventoryUint32FromInt64(stack.ItemID), currentItemListStackAmount(listType, stack))
	packed := sceneInventoryExtraByte(extra, "ext_data0", "ext0", "packed_flag_byte", "packed_flag", "packed")
	durability := sceneInventoryExtraUint16(extra, "avatar_ability_no", "ability_no", "durability", "max_durability")
	// A row still stored in the legacy constructor layout keeps the packed
	// reinforce byte at +9 and durability at +10, one byte below the current
	// row. currentItemListEquipmentExtData/Durability already read the worn
	// side that way; without the same branch here an unequipped legacy row
	// loses both fields now that currentItemListEntryFromRaw refuses to pad it.
	if legacy, ok := currentItemListStackLegacyRow(stack); ok {
		if packed == 0 {
			packed = legacy[currentLegacyRowPackedOffset]
		}
		if durability == 0 {
			durability = binary.LittleEndian.Uint16(legacy[currentLegacyRowDurabilityOffset : currentLegacyRowDurabilityOffset+2])
		}
	}
	entry.setByte(0x0A, packed)
	entry.setUint16(0x0B, durability)
	if strings.EqualFold(strings.TrimSpace(extra["item_kind"]), "equipment") {
		// 装备行的这两个字段由服务端完全拥有：+0x0A 是打包的强化/属性字节、+0x0B 是耐久。
		// setByte/setUint16 对 0 是 no-op（本意是让旧行里服务端不拥有的字节透传），但装备行
		// 的 0 是真实取值。存储行一旦被写坏（现场 2026-09-18：修理把最大耐久写进 +0x0A），
		// no-op 就会把那个毒值原样发给客户端——装备凭空显示强化等级，数值正好等于最大耐久。
		// 装备行必须强制覆盖这两个字段，这样已经被写坏的存储行也能恢复正确显示。
		entry.data[0x0A] = packed
		binary.LittleEndian.PutUint16(entry.data[0x0B:0x0D], durability)
	}
	if listType == currentAvatarInventoryListType {
		if abilityNo, legacy := currentLegacyMisplacedAvatarAbilityNo(extra); legacy {
			entry.data[0x0A] = 0
			binary.LittleEndian.PutUint16(entry.data[0x0B:0x0D], abilityNo)
		}
	}
	if usePeriod, ok := currentItemListStackableUsePeriod(stack); ok {
		binary.LittleEndian.PutUint16(entry.data[0x0B:0x0D], usePeriod)
	}
	// Fallback: when expire_time is set but the Extra-based use_period probe
	// did not fire (missing item_kind/stackable_type), derive the remaining
	// seconds directly from expire_time so the client never sees 0x0B==0
	// on an item that still has a live expiration timestamp.
	if binary.LittleEndian.Uint16(entry.data[0x0B:0x0D]) == 0 {
		if expire := currentItemListStackExpire(stack); expire != 0 {
			binary.LittleEndian.PutUint16(entry.data[0x0B:0x0D],
				currentPVFStackableUsePeriodSeconds(time.Unix(int64(expire), 0).UTC(), time.Now().UTC()))
		}
	}
	entry.setByte(0x0D, currentItemListBindFlag(stack.Bind, extra))
	if listType != currentAvatarInventoryListType {
		// 封装位（+0x0D）和上面两个字段一样，由服务端完全拥有，而且**解封就是写 0**。
		// setByte 对 0 是 no-op，于是"把一件已封装装备解封"根本写不进去：entry 是先由
		// currentItemListEntryFromRaw 从 raw_entry 拷出来的，raw 里那个陈旧的 +0x0D=1
		// 会被原样发给客户端。
		//
		// 现场 2026-10-03（服主报，char=10 / jm，圣物武器 101030779）：
		//   圣物转换书产出的圣物，用品级箱调整后客户端显示成"封装"，小退又恢复正常。
		//   取证：那三次（09:25/09:43/09:45）品级调整下发的单行 op14 里 +0x0D 全是 1，
		//   而库里这一行 raw_entry[0x0D]=0、raw_entry_hex[0x0D]=00、seal_flag=0（bind_flag=1）；
		//   小退走的是全量 list，重建后 +0x0D=0 → "封装"消失。差别就在这一行 no-op。
		//
		// 与 +0x0A/+0x0B 同一处理：装备行的 0 是真实取值，必须强制覆盖，
		// 这样已经被写坏/带陈旧 raw 的存储行也能恢复正确显示。
		//
		// 装扮行（list 1）保持原样透传：它有自己一套 legacy 行为，
		// 归 currentLegacyMisplacedAvatarAbilityNo 那条分支管。
		entry.data[0x0D] = currentItemListBindFlag(stack.Bind, extra)
	}
	valueA := currentItemListStackValueA(listType, stack)
	if listType == currentPetInventoryListType {
		// A list-7 row is authoritative even when the field is zero: clear a
		// stale serial mirrored here by an older encoder.
		binary.LittleEndian.PutUint32(entry.data[0x0E:0x12], valueA)
	} else {
		entry.setUint32(0x0E, valueA)
	}
	entry.setByte(0x12, sceneInventoryExtraByte(extra, "byte_12", "value_12"))
	entry.setByte(0x13, sceneInventoryExtraByte(extra, "byte_13", "value_13", "value_c"))
	entry.setUint16(0x14, sceneInventoryExtraUint16(extra, "marker_16", "marker16", "value_d"))
	expire := currentItemListStackExpire(stack)
	if listType == currentPetInventoryListType {
		// The current EXE copies this dword into the creature detail's
		// remaining-seconds field. Legacy 47-byte equipped-pet rows keep the
		// creature serial at byte 0x18; when raw_entry_hex is padded to a 0x77
		// list row, that serial otherwise becomes serial<<16 seconds (serial 41
		// displays as 32 days). Pet time is authoritative here: zero for a
		// permanent pet, remaining seconds for a genuinely expiring instance.
		binary.LittleEndian.PutUint32(
			entry.data[currentPetRemainSecondsOffset:currentPetRemainSecondsOffset+4],
			currentPetRemainingSecondsAt(expire, time.Now().UTC()),
		)
	}
	entry.clearLegacyWrongExpiration(expire)
	if listType == currentPetInventoryListType {
		binary.LittleEndian.PutUint32(
			entry.data[currentItemListExpireTimeOffset:currentItemListExpireTimeOffset+4],
			expire,
		)
	} else {
		entry.setUint32(currentItemListExpireTimeOffset, expire)
	}
	entry.setByte(0x57, sceneInventoryExtraByteDefault(extra, entry.data[0x57], "forging_level", "byte_57", "value_57"))
	entry.setByte(0x58, sceneInventoryExtraByte(extra, "byte_58", "value_58"))
	entry.setByte(0x59, sceneInventoryExtraByte(extra, "byte_59", "value_59"))
	entry.copyFixed(0x72, currentItemListFixedExtraBytes(extra, 5, "tail_data_72", "tail72"))
	if listType == 1 {
		entry.avatarSocketData = currentItemListAvatarSocketData(extra)
		entry.avatarColorData = currentItemListAvatarColorData(extra)
	}
	if listType == 0 || listType == 3 {
		currentApplyEquipmentSocketVectorToEntry(&entry, extra)
		currentApplyEquipmentEffectRuneToEntry(&entry, extra)
	}
	return entry
}

// currentLegacyMisplacedAvatarAbilityNo recognizes avatars created by the Go
// server before the AbilityNo wire fix. Those rows stored the client-selected
// value in Attr (raw+0x0A) and left AbilityNo (raw+0x0B) at zero. Keep the
// compatibility rule source-bound so a legitimate imported Attr value is not
// reinterpreted as an avatar skill.
func currentLegacyMisplacedAvatarAbilityNo(extra map[string]string) (uint16, bool) {
	if !strings.EqualFold(strings.TrimSpace(extra["item_kind"]), "avatar") {
		return 0, false
	}
	for _, key := range []string{"avatar_ability_no", "ability_no", "durability", "max_durability"} {
		if _, found := extra[key]; found {
			return 0, false
		}
	}
	source := strings.ToLower(strings.TrimSpace(extra["source"]))
	lastSource := strings.ToLower(strings.TrimSpace(extra["last_grant_source"]))
	if source != "cera_shop_avatar" && source != "cera_package" && source != "booster_item" &&
		lastSource != "cera_shop" && lastSource != "cera_package" && lastSource != "booster_item" {
		return 0, false
	}
	abilityNo := sceneInventoryExtraUint16(extra, "ext_data0", "ext0")
	return abilityNo, abilityNo != 0
}

// currentItemListEquipmentContainer 是"普通装备能待在里面、客户端按 +0x06 算品级"的
// 容器表：背包 0、个人仓库 2、穿戴 3、账号仓库 12。
//
// 客户端对这四张表用的是同一个 0x77 行读取器，所以它们的 +0x06 必须都是品级种子。
// 只写 0/3 时，仓库里的装备行会落到数量回退（新装备只有 Extra["quality_seed"]，
// 于是取到 Count=1），客户端就按种子 1 重算品级——现场表现是"装备放进仓库后重登
// 品级变了，取回背包再小退又恢复"。老行看不出问题是因为它们把种子同时存在
// amount_or_count 里，回退路径恰好读得到；itemquality.Apply 造的新装备只写
// quality_seed。
//
// 时装 list 1 与公会勋章 list 38 不在此列：既有证据显示它们的 +0x06 本来就是 1
// （TestGMSystemMailRareAvatarUsesNativeListOneRawRow、
// TestCurrentNPCShopBuyGuildMedalExchangeLandsInGuildMedalList 两条测试钉着）。
// 宠物 list 7 的 +0x06 是生物序列号，在调用点上方已单独返回。
func currentItemListEquipmentContainer(listType byte) bool {
	switch listType {
	case dnfrepo.MainInventoryListType, currentEquipmentPlacementListPersonalCargo, currentSocketListEquipment, 12:
		return true
	}
	return false
}

func currentItemListStackAmount(listType byte, stack dnfrepo.ItemStack) uint32 {
	if listType == 7 {
		if value := sceneInventoryExtraUint32(stack.Extra, "creature_serial_or_handle", "serial", "handle", "instance_value", "item_uid"); value != 0 {
			return value
		}
	}
	if currentItemListEquipmentContainer(listType) {
		if seed := currentEquipmentQualitySeedFromStack(stack); seed != 0 {
			return seed
		}
	}
	if stack.Count > 0 && (listType == dnfrepo.MainInventoryListType ||
		strings.EqualFold(strings.TrimSpace(stack.Extra["item_kind"]), "stackable")) {
		// ItemStack.Count is the authoritative durable quantity for ordinary
		// main-inventory rows and PVF-proven stackables in personal cargo.
		// Legacy amount aliases in Extra and RawEntry can lag behind after a
		// stack use or cross-container merge; preferring them here makes the
		// repository-backed refresh redraw the pre-mutation count.
		return sceneInventoryUint32FromInt64(stack.Count)
	}
	return sceneInventoryStackAmount(stack)
}

func currentItemListStackValueA(listType byte, stack dnfrepo.ItemStack) uint32 {
	if listType == currentPetInventoryListType {
		// Pet rows keep the creature serial/handle only at raw+0x06. Mirroring
		// that serial into raw+0x0E makes the current client interpret serial 3
		// as a three-day duration (and serial 19 as nineteen hours). This
		// matches the pet module's dedicated list-7 encoder.
		return sceneInventoryExtraUint32(stack.Extra, "value_a")
	}
	return currentItemStackValueA(stack)
}

func currentItemListEntryFromLegacyItem(item dnfrepo.LegacyInventoryItem) currentItemListEntry {
	extra := item.Extra
	entry := currentItemListEntryFromRawExtra(extra)
	entry.patchCore(item.SlotIndex, sceneInventoryUint32FromInt64(item.ItemTemplateID), sceneInventoryLegacyAmount(item))
	entry.setByte(0x0A, sceneInventoryLegacyPacked(item))
	entry.setUint16(0x0B, sceneInventoryLegacyUint16(item.Durability, extra, "durability", "max_durability"))
	entry.setByte(0x0D, sceneInventoryLegacyByte(item.SealFlag, extra, "seal_flag", "seal", "bind_flag", "bind"))
	entry.setUint32(0x0E, sceneInventoryLegacyUint32(item.InstanceValue, extra, "value_a", "instance_value", "item_uid", "serial", "count_or_instance"))
	entry.setByte(0x13, sceneInventoryLegacyByte(item.OptionValue, extra, "byte_13", "value_13", "value_c"))
	entry.setUint16(0x14, sceneInventoryLegacyUint16(item.Marker16, extra, "marker_16", "marker16", "value_d"))
	return entry
}

func currentItemListEntryFromRawExtra(extra map[string]string) currentItemListEntry {
	return currentItemListEntryFromRaw(nil, extra)
}

const (
	// Legacy constructor summary (buildInitialEquipmentRawEntry): u8 slot,
	// u32 item id, u32 create value, u8 packed reinforce, u16 durability.
	currentLegacyRowItemIDOffset     = 0x01
	currentLegacyRowPackedOffset     = 0x09
	currentLegacyRowDurabilityOffset = 0x0A
	currentLegacyRowMinSize          = currentLegacyRowDurabilityOffset + 2
)

// currentItemListStackLegacyRow returns the stored row of a stack that is
// still in the legacy constructor layout. The layout is identified rather than
// assumed: the legacy row carries the PVF item id at +1, where the current row
// carries it at +2. A row that does not agree with the stack's own item id is
// refused so no field is read out of a shape that was never proved.
func currentItemListStackLegacyRow(stack dnfrepo.ItemStack) ([]byte, bool) {
	if stack.ItemID <= 0 || stack.ItemID > int64(^uint32(0)) {
		return nil, false
	}
	raw := stack.RawEntry
	if len(raw) == 0 {
		raw = currentItemListVariableExtraBytes(stack.Extra, "raw_entry_hex", "current_raw_entry_77", "raw_entry_77")
	}
	if len(raw) < currentLegacyRowMinSize || len(raw) >= currentItemListEntryWireSize {
		return nil, false
	}
	if binary.LittleEndian.Uint32(raw[currentLegacyRowItemIDOffset:currentLegacyRowItemIDOffset+4]) != uint32(stack.ItemID) {
		return nil, false
	}
	return raw, true
}

func currentItemListEntryFromRaw(rawEntry []byte, extra map[string]string) currentItemListEntry {
	var entry currentItemListEntry
	raw := rawEntry
	if len(raw) != currentItemListEntryWireSize {
		raw = currentItemListFullRowExtraBytes(extra, "raw_entry_hex", "current_raw_entry_77", "raw_entry_77")
	}
	if len(raw) == currentItemListEntryWireSize {
		copy(entry.data[:], raw)
	}
	return entry
}
