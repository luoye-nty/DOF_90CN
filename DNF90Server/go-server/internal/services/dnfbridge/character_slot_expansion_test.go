package dnfbridge

import (
	"encoding/binary"
	"strconv"
	"testing"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

func expandedRosterRecords(count int) []dnfrepo.CharacterRecord {
	characters := make([]dnfrepo.CharacterRecord, 0, count)
	for slot := 0; slot < count; slot++ {
		id := strconv.Itoa(slot + 1)
		characters = append(characters, dnfrepo.CharacterRecord{
			CharacterID: id,
			AccountID:   "dnf:1",
			Slot:        slot,
			Name:        "character-" + id,
			Job:         "0",
			Level:       1,
		})
	}
	return characters
}

// 回归：账号记录缺失或元数据损坏时限额回落到基线 32，但库里的角色仍然坐在 32..59 号
// 槽位上。投影不能跟着回落，否则那批角色会整批从选角页消失。
func TestEffectiveCharacterSlotLimitCoversOccupiedSlots(t *testing.T) {
	characters := expandedRosterRecords(40)
	if got := effectiveCharacterSlotLimit(characters, defaultCharacterSlots); got != 40 {
		t.Fatalf("fallback limit = %d, want 40", got)
	}
	if got := effectiveCharacterSlotLimit(characters, maxCharacterSlots); got != maxCharacterSlots {
		t.Fatalf("persisted limit was lowered to %d", got)
	}
	if got := effectiveCharacterSlotLimit(nil, defaultCharacterSlots); got != defaultCharacterSlots {
		t.Fatalf("empty roster limit = %d, want the baseline %d", got, defaultCharacterSlots)
	}
	last := []dnfrepo.CharacterRecord{{CharacterID: "1", AccountID: "a", Slot: maxCharacterSlots - 1}}
	if got := effectiveCharacterSlotLimit(last, defaultCharacterSlots); got != maxCharacterSlots {
		t.Fatalf("last-slot limit = %d, want %d", got, maxCharacterSlots)
	}
}

// 回归：限额回落到基线时，mode-2 快照必须仍然按真实占用槽位算总数和页数，
// op679 扩展批次也必须仍然能取到 36..39 号槽位。
func TestRosterSurvivesSlotLimitFallbackWithExpandedSlots(t *testing.T) {
	characters := expandedRosterRecords(40)
	body := buildNoPackRosterBodyForLimit(characters, defaultCharacterSlots)
	if len(body) < 15 {
		t.Fatalf("roster body too short: %d", len(body))
	}
	if body[1] != byte(len(characters)) {
		t.Fatalf("roster total count = %d, want %d", body[1], len(characters))
	}
	// mode-2 只承载 0..35，剩下的走扩展批次。
	if got := binary.LittleEndian.Uint16(body[13:15]); got != currentRosterExtensionBatchSize {
		t.Fatalf("mode-2 row count = %d, want %d", got, currentRosterExtensionBatchSize)
	}
	offset := 15
	for _, character := range characters[:currentRosterExtensionBatchSize] {
		if got := int(binary.LittleEndian.Uint16(body[offset : offset+2])); got != character.Slot {
			t.Fatalf("mode-2 entry at offset %d carries slot %d, want %d", offset, got, character.Slot)
		}
		offset += noPackRosterEntryLen(character)
	}
	if got := body[offset]; got != 2 {
		t.Fatalf("roster page count = %d, want 2", got)
	}

	extension, ok := buildNoPackRosterExtensionBodyForLimit(characters, defaultCharacterSlots, currentRosterExtensionBatchSize)
	if !ok {
		t.Fatal("extension batch was rejected after the slot limit fell back to the baseline")
	}
	if extension[0] != 4 {
		t.Fatalf("extension mode = %d, want 4", extension[0])
	}
	if got := binary.LittleEndian.Uint16(extension[1:3]); got != uint16(len(characters)) {
		t.Fatalf("extension total count = %d, want %d", got, len(characters))
	}
	if got := binary.LittleEndian.Uint16(extension[3:5]); got != 4 {
		t.Fatalf("extension row count = %d, want 4", got)
	}
	// 扩展批次的行键必须是账号级真实槽位，不是批次内的零基序号。
	offset = 5
	for _, character := range characters[currentRosterExtensionBatchSize:] {
		if got := int(binary.LittleEndian.Uint16(extension[offset : offset+2])); got != character.Slot {
			t.Fatalf("extension entry at offset %d carries slot %d, want %d", offset, got, character.Slot)
		}
		offset += noPackRosterEntryLen(character)
	}
}

// 回归：nextCharacterSlot 找不到空位时曾返回 len(records)，槽位有空洞时这个下标
// 正好落在一个已占用的槽位上，写进去会撞唯一约束。
func TestNextCharacterSlotReusesHolesAndRefusesWhenFull(t *testing.T) {
	characters := expandedRosterRecords(40)
	withHole := make([]dnfrepo.CharacterRecord, 0, len(characters)-1)
	for _, character := range characters {
		if character.Slot == 35 {
			continue
		}
		withHole = append(withHole, character)
	}
	if got := nextCharacterSlot(withHole, dnfrepo.MaxCharacterSlotLimit); got != 35 {
		t.Fatalf("next slot = %d, want the free slot 35", got)
	}
	if got := nextCharacterSlot(characters, dnfrepo.MaxCharacterSlotLimit); got != 40 {
		t.Fatalf("next slot = %d, want 40", got)
	}
	full := expandedRosterRecords(dnfrepo.MaxCharacterSlotLimit)
	if got := nextCharacterSlot(full, dnfrepo.MaxCharacterSlotLimit); got != -1 {
		t.Fatalf("full roster next slot = %d, want -1", got)
	}
	// 空洞出现在扩栏区间里时同样要复用，而不是回一个重复槽位。
	fullWithHole := make([]dnfrepo.CharacterRecord, 0, len(full)-1)
	for _, character := range full {
		if character.Slot == 47 {
			continue
		}
		fullWithHole = append(fullWithHole, character)
	}
	if got := nextCharacterSlot(fullWithHole, dnfrepo.MaxCharacterSlotLimit); got != 47 {
		t.Fatalf("next slot = %d, want the free slot 47", got)
	}
}
