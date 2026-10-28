package dnfbridge

import (
	"encoding/binary"
	"fmt"
	"sort"

	"google.golang.org/protobuf/encoding/protowire"
	darkcombo "longheng.io/server/internal/modules/dnf/darkknightcombo"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

const (
	currentSkillInfoMsgID       = 19
	currentSkillInfoMessageType = 19
	currentSkillInfoTreeIndex   = 0
	currentSkillInfoTreeCount   = 2
	currentSkillPrimaryQuickEnd = 6
	currentSkillExtensionStart  = 198
	currentSkillSlotCount       = 204
)

func buildCurrentSceneSkillInfoBody(record dnfrepo.SkillRecord, layouts map[int]dnfrepo.SkillLayout, activeTree byte) ([]byte, []int, error) {
	return buildCurrentSceneSkillInfoBodyWithDarkKnightCombo(record, layouts, activeTree, nil)
}

func buildCurrentSceneSkillInfoBodyWithDarkKnightCombo(
	record dnfrepo.SkillRecord,
	layouts map[int]dnfrepo.SkillLayout,
	activeTree byte,
	pages []darkcombo.Page,
) ([]byte, []int, error) {
	if activeTree >= currentSkillInfoTreeCount {
		return nil, nil, fmt.Errorf("current active skill tree out of range: tree=%d", activeTree)
	}
	comboChildren := make(map[int]map[uint16][]uint16, len(pages))
	for _, page := range pages {
		treeIndex := int(page.Index)
		if treeIndex < 0 || treeIndex >= currentSkillInfoTreeCount {
			return nil, nil, fmt.Errorf("dark knight combo page outside skill tree range: page=%d", page.Index)
		}
		if comboChildren[treeIndex] == nil {
			comboChildren[treeIndex] = make(map[uint16][]uint16, len(page.Roots))
		}
		for _, root := range page.Roots {
			comboChildren[treeIndex][root.SkillID] = append([]uint16(nil), root.Children...)
		}
	}

	var message []byte
	message = appendProtoVarint(message, 1, currentSkillInfoMessageType)
	activeSlots := make([]int, 0, 14)
	for treeIndex := 0; treeIndex < currentSkillInfoTreeCount; treeIndex++ {
		state, ok := dnfrepo.SkillTree(record, treeIndex)
		if !ok {
			// Legacy/synthetic commit snapshots may reach this formatter before the
			// login backfill has created both durable pages. Keep the projection
			// backward-compatible with an empty page; the scene owner still creates
			// and persists the PVF-backed alternate tree before interactive use.
			if treeIndex == currentSkillInfoTreeIndex {
				state = dnfrepo.SkillTreeState{Skills: record.Skills, Points: record.Points}
			}
		}
		layout := layouts[treeIndex]
		slots := make([]int, 0, len(layout))
		for slot, skillID := range layout {
			if slot < 0 || slot >= currentSkillSlotCount {
				return nil, nil, fmt.Errorf("current skill slot outside stock client range: slot=%d skill=%d count=%d", slot, skillID, currentSkillSlotCount)
			}
			skillState, ok := state.Skills[int64(skillID)]
			if !ok || skillState.Level <= 0 {
				continue
			}
			if slot < 0 || slot > 0x7fffffff {
				return nil, nil, fmt.Errorf("current skill slot out of int32 range: slot=%d skill=%d", slot, skillID)
			}
			slots = append(slots, slot)
		}
		sort.Ints(slots)

		var tree []byte
		tree = appendProtoVarint(tree, 1, uint64(nonNegativeInt(state.Points.RemainingSP)))
		tree = appendProtoVarint(tree, 2, uint64(nonNegativeInt(state.Points.RemainingTP)))
		for _, slot := range slots {
			skillID := layout[slot]
			skillState := state.Skills[int64(skillID)]
			if skillState.Level > 0x7fffffff {
				return nil, nil, fmt.Errorf("current skill level out of client rank range: skill=%d level=%d", skillID, skillState.Level)
			}
			var skill []byte
			skill = appendProtoVarint(skill, 1, uint64(slot))
			skill = appendProtoVarint(skill, 2, uint64(skillID))
			// sub_2666AE0 preserves the wire rank unless a current actor mapping
			// explicitly replaces it. Persisted learned levels therefore stay direct.
			skill = appendProtoVarint(skill, 3, uint64(skillState.Level))
			if children, comboRoot := comboChildren[treeIndex][skillID]; comboRoot {
				for _, childSkillID := range children {
					skill = appendProtoVarint(skill, 4, uint64(childSkillID))
				}
			} else {
				for _, commandByte := range record.Commands[int64(skillID)] {
					skill = appendProtoVarint(skill, 4, uint64(commandByte))
				}
			}
			tree = appendProtoBytes(tree, 3, skill)
			if treeIndex == int(activeTree) && isCurrentSceneQuickSkillSlot(slot) {
				activeSlots = append(activeSlots, slot)
			}
		}
		message = appendProtoBytes(message, 2, tree)
	}

	body := make([]byte, 4, 4+len(message))
	binary.LittleEndian.PutUint32(body, uint32(len(message)))
	body = append(body, message...)
	return body, activeSlots, nil
}

func isCurrentSceneQuickSkillSlot(slot int) bool {
	return (slot >= 0 && slot < currentSkillPrimaryQuickEnd) ||
		(slot >= currentSkillExtensionStart && slot < currentSkillSlotCount)
}

func appendProtoVarint(dst []byte, field protowire.Number, value uint64) []byte {
	dst = protowire.AppendTag(dst, field, protowire.VarintType)
	return protowire.AppendVarint(dst, value)
}

func appendProtoBytes(dst []byte, field protowire.Number, value []byte) []byte {
	dst = protowire.AppendTag(dst, field, protowire.BytesType)
	return protowire.AppendBytes(dst, value)
}

func nonNegativeInt(value int) int {
	if value < 0 {
		return 0
	}
	return value
}
