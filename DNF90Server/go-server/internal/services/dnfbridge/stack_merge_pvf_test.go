package dnfbridge

import (
	"testing"

	"longheng.io/server/internal/modules/dnf/dnfenum"
)

func TestAlignedStackMergeResolverUsesPVFOmittedLimitAsUnlimited(t *testing.T) {
	source := dungeonDropCatalogTestSource{
		"monster/monster.lst":                 "",
		"equipment/equipment.lst":             "",
		"stackable/stackable.lst":             "3244 `material/crystal_pure.stk`\n",
		"stackable/material/crystal_pure.stk": "[name] `concentrated purity remains`\n[stackable type] `[material]`\n",
	}
	catalog, err := newPVFDungeonDropCatalog(source)
	if err != nil {
		t.Fatal(err)
	}
	resolver, err := (&Service{pvfItemCatalog: catalog}).alignedStackMergeResolverForCommand(dnfenum.CmdPacketMoveItemspace)
	if err != nil {
		t.Fatal(err)
	}
	resolution, err := resolver(3244)
	if err != nil {
		t.Fatal(err)
	}
	if resolution.ItemID != 3244 || resolution.Kind != "stackable" ||
		resolution.PVFPath != "stackable/material/crystal_pure.stk" ||
		resolution.StackableType != "[material]" || resolution.StackLimit != 0 ||
		resolution.SlotStart != 121 || resolution.SlotEnd != 176 {
		t.Fatalf("resolution = %+v", resolution)
	}
	upgradeResolver, err := (&Service{pvfItemCatalog: catalog}).alignedStackMergeResolverForCommand(dnfenum.CmdPacketUpgradeItem)
	if err != nil || upgradeResolver == nil {
		t.Fatalf("upgrade resolver = %v err=%v", upgradeResolver, err)
	}
	upgradeResolution, err := upgradeResolver(3244)
	if err != nil || upgradeResolution.SlotStart != 121 || upgradeResolution.SlotEnd != 176 {
		t.Fatalf("upgrade resolution = %+v err=%v", upgradeResolution, err)
	}
	none, err := (&Service{pvfItemCatalog: catalog}).alignedStackMergeResolverForCommand(dnfenum.CmdPacketUseStackable)
	if err != nil || none != nil {
		t.Fatalf("non-op19 resolver = %v err=%v", none, err)
	}
}

func TestAlignedStackMergeResolverPreservesEquipmentTypeForQuickSlotPolicy(t *testing.T) {
	source := dungeonDropCatalogTestSource{
		"monster/monster.lst":                                      "",
		"stackable/stackable.lst":                                  "",
		"equipment/equipment.lst":                                  "400360005 `character/common/charm/chn_event_400360005.equ`\n",
		"equipment/character/common/charm/chn_event_400360005.equ": "[name] `神圣符咒 (飓风)`\n[equipment type] `[charm]`\n",
	}
	catalog, err := newPVFDungeonDropCatalog(source)
	if err != nil {
		t.Fatal(err)
	}
	resolver, err := (&Service{pvfItemCatalog: catalog}).alignedStackMergeResolverForCommand(dnfenum.CmdPacketMoveItemspace)
	if err != nil {
		t.Fatal(err)
	}
	resolution, err := resolver(400360005)
	if err != nil {
		t.Fatal(err)
	}
	if resolution.ItemID != 400360005 || resolution.Kind != "equipment" || resolution.EquipmentType != "[charm]" || resolution.PVFPath != "equipment/character/common/charm/chn_event_400360005.equ" {
		t.Fatalf("resolution = %+v", resolution)
	}
}
