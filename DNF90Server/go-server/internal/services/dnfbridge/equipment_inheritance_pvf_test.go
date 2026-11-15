package dnfbridge

import (
	"os"
	"testing"

	"longheng.io/server/internal/modules/dnf/dnfenum"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

func TestEquipmentInheritanceCouponParsesOptionalMaterialsAndExcludedEquipment(t *testing.T) {
	source := dungeonDropCatalogTestSource{
		"monster/monster.lst":     "",
		"equipment/equipment.lst": "9001 `character/common/coat_9001.equ`\n9002 `character/common/coat_9002.equ`\n",
		"stackable/stackable.lst": "202611001 `cash/equipment_inheritance_coupon_202611001.stk`\n3001 `material/inheritance_material.stk`\n",
		"stackable/cash/equipment_inheritance_coupon_202611001.stk": "" +
			"[stackable type] `[etc]` 8\n" +
			"[move wav] `BOOK_TOUCH`\n" +
			"[use wav] `SP_UP_ITEM`\n" +
			"[equipment inheritance material]\n" +
			"3001 4\n" +
			"0 1000000\n" +
			"3001 6\n" +
			"[/equipment inheritance material]\n" +
			"[equipment inheritance excluded equipment]\n" +
			"9002\n" +
			"9001\n" +
			"9002\n" +
			"[/equipment inheritance excluded equipment]\n",
		"stackable/material/inheritance_material.stk": "[stackable type] `[material]` 0\n[stack limit] 999\n",
		"equipment/character/common/coat_9001.equ":    "[equipment type] `[coat]`\n[rarity] 3\n[minimum level] 60\n",
		"equipment/character/common/coat_9002.equ":    "[equipment type] `[coat]`\n[rarity] 5\n[minimum level] 65\n",
	}
	catalog, err := newPVFDungeonDropCatalog(source)
	if err != nil {
		t.Fatal(err)
	}
	service := &Service{pvfItemCatalog: catalog}
	for _, opcode := range []dnfenum.CmdPacket{
		dnfenum.CmdPacketUseStackable,
		dnfenum.CmdPacketUseStackableAction,
		dnfenum.CmdPacketUseSharedEffectItem,
	} {
		if _, resolveErr := service.alignedStackableUseResolverForCommand(opcode); resolveErr != nil {
			t.Fatalf("opcode %d inheritance resolver: %v", opcode, resolveErr)
		}
	}
	resolver, err := service.alignedStackableUseResolverForCommand(dnfenum.CmdPacketUseStackable)
	if err != nil {
		t.Fatal(err)
	}
	resolution, err := resolver(currentEquipmentInheritanceCouponItemID)
	if err != nil {
		t.Fatal(err)
	}
	inheritance := resolution.EquipmentInheritance
	if inheritance == nil || len(inheritance.Materials) != 2 {
		t.Fatalf("inheritance resolution=%+v", resolution)
	}
	if inheritance.Materials[0].ItemID != 0 || inheritance.Materials[0].Count != 1000000 ||
		inheritance.Materials[1].ItemID != 3001 || inheritance.Materials[1].Count != 10 {
		t.Fatalf("materials=%+v", inheritance.Materials)
	}
	if len(inheritance.ExcludedEquipmentIDs) != 2 ||
		inheritance.ExcludedEquipmentIDs[0] != 9001 ||
		inheritance.ExcludedEquipmentIDs[1] != 9002 {
		t.Fatalf("excluded=%+v", inheritance.ExcludedEquipmentIDs)
	}
	equipment, err := inheritance.ResolveEquipment(9002)
	if err != nil {
		t.Fatal(err)
	}
	if equipment.EquipmentType != "[coat]" || equipment.Rarity != 5 || equipment.MinimumLevel != 65 {
		t.Fatalf("equipment=%+v", equipment)
	}
}

func TestEquipmentInheritanceCouponMissingSectionsMeansNoCostAndNoExclusions(t *testing.T) {
	source := dungeonDropCatalogTestSource{
		"monster/monster.lst":                                       "",
		"equipment/equipment.lst":                                   "",
		"stackable/stackable.lst":                                   "202611001 `cash/equipment_inheritance_coupon_202611001.stk`\n",
		"stackable/cash/equipment_inheritance_coupon_202611001.stk": "[stackable type] `[etc]` 8\n[move wav] `BOOK_TOUCH`\n[use wav] `SP_UP_ITEM`\n",
	}
	catalog, err := newPVFDungeonDropCatalog(source)
	if err != nil {
		t.Fatal(err)
	}
	service := &Service{pvfItemCatalog: catalog}
	resolver, err := service.alignedStackableUseResolverForCommand(dnfenum.CmdPacketUseStackable)
	if err != nil {
		t.Fatal(err)
	}
	resolution, err := resolver(currentEquipmentInheritanceCouponItemID)
	if err != nil {
		t.Fatal(err)
	}
	if resolution.EquipmentInheritance == nil ||
		len(resolution.EquipmentInheritance.Materials) != 0 ||
		len(resolution.EquipmentInheritance.ExcludedEquipmentIDs) != 0 {
		t.Fatalf("empty inheritance configuration=%+v", resolution.EquipmentInheritance)
	}
}

func TestEquipmentInheritanceCouponRealPVF(t *testing.T) {
	pvfPath := os.Getenv("DNF_EQUIPMENT_INHERITANCE_REAL_PVF")
	if pvfPath == "" {
		t.Skip("DNF_EQUIPMENT_INHERITANCE_REAL_PVF not set")
	}
	archive, err := platformpvf.Open(pvfPath)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := newPVFDungeonDropCatalog(archive)
	if err != nil {
		t.Fatal(err)
	}
	service := &Service{pvfItemCatalog: catalog}
	resolver, err := service.alignedStackableUseResolverForCommand(dnfenum.CmdPacketUseStackable)
	if err != nil {
		t.Fatal(err)
	}
	resolution, err := resolver(currentEquipmentInheritanceCouponItemID)
	if err != nil {
		t.Fatal(err)
	}
	inheritance := resolution.EquipmentInheritance
	if inheritance == nil ||
		resolution.PVFPath != "stackable/ly/equipment_inheritance_coupon_202611001.stk" ||
		len(inheritance.Materials) != 2 ||
		inheritance.Materials[0].ItemID != 0 || inheritance.Materials[0].Count != 200000 ||
		inheritance.Materials[1].ItemID != 3263 || inheritance.Materials[1].Count != 5 ||
		len(inheritance.ExcludedEquipmentIDs) != 0 {
		t.Fatalf("real PVF inheritance resolution=%+v inheritance=%+v", resolution, inheritance)
	}
}
