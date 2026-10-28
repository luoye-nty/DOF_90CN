package dnfbridge

import (
	"context"
	"encoding/binary"
	"testing"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

func newGMItemCustomizationTestCatalog(t *testing.T) (*pvfDungeonDropCatalog, dungeonDropCatalogTestSource) {
	t.Helper()
	source := dungeonDropCatalogTestSource{
		"monster/monster.lst":        "",
		"stackable/stackable.lst":    "2050 `emblem/red.stk`\n2052 `emblem/green.stk`\n2600294 `bead/coat.stk`\n9001 `card/coat.stk`\n",
		"equipment/equipment.lst":    "700 `armor/coat.equ`\n701 `weapon/sword.equ`\n",
		"stackable/emblem/red.stk":   "[name] `Red Emblem`\n[rarity] 2\n[stackable type] `[avatar emblem]`\n[avatar emblem target type]\n `[A socket]`\n[/avatar emblem target type]\n",
		"stackable/emblem/green.stk": "[name] `Green Emblem`\n[rarity] 3\n[stackable type] `[avatar emblem]`\n[enchant]\n[magical attack] 10\n[skill levelup] `[swordman]` 5 1\n[/skill levelup]\n[/enchant]\n[avatar emblem target type]\n `[C socket]`\n[/avatar emblem target type]\n",
		"stackable/bead/coat.stk":    "[name] `Coat Bead`\n[rarity] 4\n[stackable type] `[enchant waste]`\n[monster card id] 9001\n",
		"stackable/card/coat.stk":    "[name] `Coat Card`\n[string data] `icon.img` `[coat]`\n[enchant table]\n[enchant index] 0\n[equipment physical attack] 25\n[elemental property] `[light element]`\n[/elemental property]\n[/enchant index]\n[/enchant table]\n",
		"equipment/armor/coat.equ":   "[name] `Test Coat`\n[equipment type] `[coat]`\n[durability] 50\n",
		"equipment/weapon/sword.equ": "[name] `Test Sword`\n[equipment type] `[weapon]`\n[durability] 50\n",
	}
	catalog, err := newPVFDungeonDropCatalog(source)
	if err != nil {
		t.Fatal(err)
	}
	return catalog, source
}

func TestGMItemMailOptionsFilterByEquipmentPart(t *testing.T) {
	catalog, _ := newGMItemCustomizationTestCatalog(t)
	service := &Service{pvfItemCatalog: catalog}

	emblems, err := service.GMItemMailOptions(context.Background(), 700, "emblem", "", 3, 20, 0)
	if err != nil {
		t.Fatal(err)
	}
	if emblems.SocketColor != "绿色" || emblems.SocketCount != 2 || emblems.Total != 1 || len(emblems.Items) != 1 || emblems.Items[0].ItemID != 2052 ||
		len(emblems.Items[0].MailEffects) != 1 || len(emblems.Items[0].MailEffects[0].Attributes) != 1 || emblems.Items[0].MailEffects[0].Attributes[0].Name != "intelligence" ||
		len(emblems.Items[0].MailEffects[0].SkillLevelUps) != 1 || emblems.Items[0].MailEffects[0].SkillLevelUps[0].SkillID != 5 {
		t.Fatalf("coat emblem options = %+v", emblems)
	}
	if empty, err := service.GMItemMailOptions(context.Background(), 700, "emblem", "", 1, 20, 0); err != nil || empty.Total != 0 {
		t.Fatalf("rarity-filtered emblems = %+v err=%v", empty, err)
	}
	enchants, err := service.GMItemMailOptions(context.Background(), 700, "enchant", "coat", 4, 20, 0)
	if err != nil {
		t.Fatal(err)
	}
	if enchants.Total != 1 || len(enchants.Items) != 1 || enchants.Items[0].ItemID != 2600294 || len(enchants.Items[0].MailEffects) != 1 ||
		len(enchants.Items[0].MailEffects[0].Attributes) != 1 || enchants.Items[0].MailEffects[0].Attributes[0].Name != "physical_attack" || enchants.Items[0].MailEffects[0].Attributes[0].Value != 25 ||
		len(enchants.Items[0].MailEffects[0].ElementalProperties) != 1 || enchants.Items[0].MailEffects[0].ElementalProperties[0] != "light element" {
		t.Fatalf("coat enchant options = %+v", enchants)
	}
	if _, err := service.GMItemMailOptions(context.Background(), 701, "emblem", "", -1, 20, 0); err == nil {
		t.Fatal("weapon must not expose emblem options")
	}
}

func TestGMApplyMailEquipmentCustomizationProjectsRawFields(t *testing.T) {
	catalog, _ := newGMItemCustomizationTestCatalog(t)
	coat, err := catalog.ResolveItem(700)
	if err != nil {
		t.Fatal(err)
	}
	stack := dnfrepo.ItemStack{ItemID: 700, Count: 1, Extra: map[string]string{"item_kind": "equipment", "equipment_type": "[coat]"}}
	if err := gmApplyMailEquipmentCustomization(&stack, catalog, coat, GMMailOptions{
		EmblemItemIDs:     []uint32{2052, 2052},
		EnchantBeadItemID: 2600294,
	}); err != nil {
		t.Fatal(err)
	}
	entry := currentItemListEntryFromStack(dnfrepo.MainInventoryListType, 0, stack)
	if entry.data[currentEquipmentVectorOffset] != 2 ||
		binary.LittleEndian.Uint32(entry.data[currentEquipmentVectorOffset+1:currentEquipmentVectorOffset+5]) != 2052 ||
		binary.LittleEndian.Uint32(entry.data[currentEquipmentVectorOffset+5:currentEquipmentVectorOffset+9]) != 2052 {
		t.Fatalf("emblem vector = %x", entry.data[currentEquipmentVectorOffset:currentEquipmentVectorOffset+currentEquipmentEmblemDataBytes])
	}
	if binary.LittleEndian.Uint32(entry.data[0x0E:0x12]) != 9001 || entry.data[0x12] != 0 {
		t.Fatalf("enchant fields = %x/%d", entry.data[0x0E:0x12], entry.data[0x12])
	}

	weapon, err := catalog.ResolveItem(701)
	if err != nil {
		t.Fatal(err)
	}
	weaponStack := dnfrepo.ItemStack{ItemID: 701, Count: 1, Extra: map[string]string{"item_kind": "equipment", "equipment_type": "[weapon]"}}
	if err := gmApplyMailEquipmentCustomization(&weaponStack, catalog, weapon, GMMailOptions{Forging: 8}); err != nil {
		t.Fatal(err)
	}
	weaponEntry := currentItemListEntryFromStack(dnfrepo.MainInventoryListType, 0, weaponStack)
	if weaponEntry.data[currentItemListForgingLevelOffset] != 8 ||
		weaponStack.Extra["forging_level"] != "8" ||
		weaponEntry.data[0x12] != 0 ||
		weaponEntry.data[0x4C] != 0 {
		t.Fatalf("forging=%d enchant_upgrade=%d extra=%+v", weaponEntry.data[currentItemListForgingLevelOffset], weaponEntry.data[0x12], weaponStack.Extra)
	}
	plainWeaponStack := dnfrepo.ItemStack{ItemID: 701, Count: 1, Extra: map[string]string{"item_kind": "equipment", "equipment_type": "[weapon]"}}
	if err := gmApplyMailEquipmentCustomization(&plainWeaponStack, catalog, weapon, GMMailOptions{Forging: 0}); err != nil {
		t.Fatal(err)
	}
	plainWeaponEntry := currentItemListEntryFromStack(dnfrepo.MainInventoryListType, 0, plainWeaponStack)
	if plainWeaponEntry.data[currentItemListForgingLevelOffset] != 0 {
		t.Fatalf("zero forging projected %d", plainWeaponEntry.data[currentItemListForgingLevelOffset])
	}
	if err := gmApplyMailEquipmentCustomization(&weaponStack, catalog, weapon, GMMailOptions{EmblemItemIDs: []uint32{2052}}); err == nil {
		t.Fatal("weapon emblem must be rejected")
	}
}
