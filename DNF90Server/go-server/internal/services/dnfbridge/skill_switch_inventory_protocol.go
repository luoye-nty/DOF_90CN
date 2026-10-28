package dnfbridge

import (
	"encoding/binary"
	"fmt"
	"sort"
)

const (
	skillSwitchInventoryCmdWire     uint16 = 0x0594
	skillSwitchInventoryDeltaWire   uint16 = 0x0558
	skillSwitchInventoryAllDataWire uint16 = 0x0559
	skillSwitchInventoryBodySize           = 6
	skillSwitchClearListType        byte   = 44
	skillSwitchClearSlot            uint16 = 0xffff
	skillSwitchSkillPart            byte   = 33
)

type skillSwitchInventoryRequest struct {
	SkillID       uint16
	EquipmentPart byte
	ListType      byte
	Slot          uint16
}

type skillSwitchInventoryAction byte

const (
	skillSwitchSetItem skillSwitchInventoryAction = iota + 1
	skillSwitchRemoveItem
	skillSwitchSetSkill
)

func decodeSkillSwitchInventoryRequest(body []byte) (skillSwitchInventoryRequest, skillSwitchInventoryAction, error) {
	if len(body) != skillSwitchInventoryBodySize {
		return skillSwitchInventoryRequest{}, 0, fmt.Errorf("skill-switch body length=%d want=%d", len(body), skillSwitchInventoryBodySize)
	}
	r := skillSwitchInventoryRequest{
		SkillID:       binary.LittleEndian.Uint16(body[0:2]),
		EquipmentPart: body[2],
		ListType:      body[3],
		Slot:          binary.LittleEndian.Uint16(body[4:6]),
	}
	if r.EquipmentPart == skillSwitchSkillPart {
		if r.ListType != skillSwitchClearListType || r.Slot != skillSwitchClearSlot {
			return skillSwitchInventoryRequest{}, 0, fmt.Errorf("skill-switch skill sentinel mismatch list=%d slot=%#x", r.ListType, r.Slot)
		}
		return r, skillSwitchSetSkill, nil
	}
	if r.EquipmentPart > 32 {
		return skillSwitchInventoryRequest{}, 0, fmt.Errorf("skill-switch equipment part=%d out of range", r.EquipmentPart)
	}
	if r.ListType == skillSwitchClearListType || r.Slot == skillSwitchClearSlot {
		if r.ListType != skillSwitchClearListType || r.Slot != skillSwitchClearSlot {
			return skillSwitchInventoryRequest{}, 0, fmt.Errorf("skill-switch remove sentinel mismatch list=%d slot=%#x", r.ListType, r.Slot)
		}
		return r, skillSwitchRemoveItem, nil
	}
	return r, skillSwitchSetItem, nil
}

type skillSwitchBinding struct {
	EquipmentPart byte   `json:"equipment_part"`
	ItemSerial    uint32 `json:"item_serial"`
	ItemID        uint32 `json:"item_id"`
}

type skillSwitchInventoryState struct {
	SkillID uint16               `json:"skill_id"`
	Items   []skillSwitchBinding `json:"items,omitempty"`
}

func (s skillSwitchInventoryState) sortedItems() []skillSwitchBinding {
	items := append([]skillSwitchBinding(nil), s.Items...)
	sort.Slice(items, func(i, j int) bool { return items[i].EquipmentPart < items[j].EquipmentPart })
	return items
}

func buildSkillSwitchAllDataBody(state skillSwitchInventoryState, positions map[byte]skillSwitchInventoryPosition) []byte {
	items := state.sortedItems()
	valid := make([]skillSwitchInventoryPosition, 0, len(items))
	for _, item := range items {
		if p, ok := positions[item.EquipmentPart]; ok {
			valid = append(valid, p)
		}
	}
	result := make([]byte, 3, 3+len(valid)*5)
	binary.LittleEndian.PutUint16(result[:2], state.SkillID)
	for _, p := range valid {
		result = append(result, 1, p.EquipmentPart, p.ListType, 0, 0)
		binary.LittleEndian.PutUint16(result[len(result)-2:], uint16(p.Slot))
	}
	result[2] = byte(len(valid))
	return result
}
