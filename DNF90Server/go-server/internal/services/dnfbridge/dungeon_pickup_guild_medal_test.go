package dnfbridge

import (
	"testing"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

// 勋章 [flag] 与守护珠 [flag gem] 只有落在 list 38 才能佩戴/镶嵌。副本拾取原先
// 无条件写 list 0 的 PVF 页，于是捡到的勋章躺在背包里用不了，要等下次登录的
// migrateLegacyGuildMedalItemsFromMainInventory 搬过去才正常——这正是"捡到时
// 不对、重登才好"。这里钉住拾取与 NPC 公会兑换用的是同一条归位规则。
func TestCurrentDungeonPickupListPlacementSendsGuildMedalsToList38(t *testing.T) {
	for _, row := range []struct {
		name          string
		equipmentType string
		stackableType string
		kind          dungeonDropItemKind
	}{
		{name: "guild medal", equipmentType: "[flag]", kind: dungeonDropItemEquipment},
		{name: "guardian gem", stackableType: "[flag gem]", kind: dungeonDropItemStackable},
	} {
		t.Run(row.name, func(t *testing.T) {
			definition := dungeonDropItemDefinition{
				ItemID:        90000,
				Kind:          row.kind,
				EquipmentType: row.equipmentType,
				StackableType: row.stackableType,
				SlotStart:     9,
				SlotEnd:       64,
			}
			start, end, ok := currentGuildMedalListPlacement(definition)
			if !ok {
				t.Skip("PVF placement rule does not classify this fixture; nothing to pin")
			}
			listType, gotStart, gotEnd := currentDungeonPickupListPlacement(definition)
			if listType != currentGuildMedalInventoryListType {
				t.Fatalf("pickup list = %d, want %d", listType, currentGuildMedalInventoryListType)
			}
			if gotStart != start || gotEnd != end {
				t.Fatalf("pickup page = %d..%d, want the NPC-exchange page %d..%d",
					gotStart, gotEnd, start, end)
			}
			// The quick-slot preference reaches for list-0 slots 3..8 and must
			// never be carried onto list 38.
			placement := currentDungeonPickupPlacement(definition, 1)
			if placement.ListType != currentGuildMedalInventoryListType {
				t.Fatalf("placement list = %d, want %d", placement.ListType, currentGuildMedalInventoryListType)
			}
			if placement.Definition.PreferQuickSlot {
				t.Fatal("list-38 placement must not prefer the list-0 quick slots")
			}
		})
	}
}

// 普通掉落必须原样留在 list 0，否则这次改动会把整棵掉落树搬错地方。
func TestCurrentDungeonPickupListPlacementKeepsOrdinaryDropsOnListZero(t *testing.T) {
	definition := dungeonDropItemDefinition{
		ItemID:        10154439,
		Kind:          dungeonDropItemStackable,
		StackableType: "[quest]",
		SlotStart:     65,
		SlotEnd:       97,
	}
	listType, start, end := currentDungeonPickupListPlacement(definition)
	if listType != dnfrepo.MainInventoryListType {
		t.Fatalf("ordinary drop list = %d, want %d", listType, dnfrepo.MainInventoryListType)
	}
	if start != definition.SlotStart || end != definition.SlotEnd {
		t.Fatalf("ordinary drop page = %d..%d, want the PVF page %d..%d",
			start, end, definition.SlotStart, definition.SlotEnd)
	}
}
