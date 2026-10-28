package dnfbridge

import (
	"context"
	"reflect"
	"testing"
)

func TestGMItemDetailUsesTypeSpecificPVFDescription(t *testing.T) {
	t.Parallel()
	catalog, err := newPVFDungeonDropCatalog(dungeonDropCatalogTestSource{
		"monster/monster.lst":                          "",
		"equipment/equipment.lst":                      "27000 `character/swordman/weapon/test.equ`\n",
		"stackable/stackable.lst":                      "3037 `material/cube.stk`\n",
		"skill/skilllist.lst":                          "0 `SwordmanSkill.lst`\n9 `DemonicSwordmanSkill.lst`\n11 `AtSwordmanSkill.lst`\n",
		"skill/SwordmanSkill.lst":                      "33 `Swordman/BeamMastery.skl`\n91 `Swordman/BladeSkill.skl`\n",
		"skill/DemonicSwordmanSkill.lst":               "33 `DemonicSwordman/BeamMastery.skl`\n257 `DemonicSwordman/TimeStop.skl`\n",
		"skill/AtSwordmanSkill.lst":                    "72 `AtSwordman/ThreeFlower.skl`\n95 `AtSwordman/BeamMastery.skl`\n",
		"skill/Swordman/BeamMastery.skl":               "[name] `光剑掌握`\n[skill type] `[passive]`\n",
		"skill/Swordman/BladeSkill.skl":                "[name] `极·鬼剑术（斩铁式）`\n[skill type] `[passive]`\n",
		"skill/DemonicSwordman/BeamMastery.skl":        "[name] `黑暗光剑掌握`\n[skill type] `[passive]`\n",
		"skill/DemonicSwordman/TimeStop.skl":           "[name] `时间停滞`\n[skill type] `[passive]`\n",
		"skill/AtSwordman/BeamMastery.skl":             "[name] `光剑掌握`\n[skill type] `[passive]`\n",
		"skill/AtSwordman/ThreeFlower.skl":             "[name] `三花聚顶`\n[skill type] `[passive]`\n",
		"etc/equipmentpartset.etc":                     "90 `character/partset2/set_item_90.equ` `测试荣光套装`\n",
		"equipment/character/partset2/set_item_90.equ": "[piece set ability]\n3\n[all elemental attack]\n25\n[parameter basic explain]\n`攻击时， 附加15%%的伤害。`\n[/piece set ability]\n[piece set ability]\n5\n[parameter detail explain]\n`暴击伤害 +35%%`\n[/piece set ability]\n",
		"equipment/character/swordman/weapon/test.equ": "[name] `测试光剑`\n[basic explain] `物理、 魔法和独立攻击力 +22%%`\n[detail explain] `攻击时， 额外增加35%%的伤害增加量。`\n[flavor text] `林纳斯留下的旧剑。`\n[minimum level] 12\n[rarity] 2\n[equipment type] `[weapon]`\n[usable job] `[swordman]`\n[item group name] `beamswd`\n[required job skill]\n`[swordman]` 33 `[demonic swordman]` 33 `[at swordman]` 95\n[/required job skill]\n[skill levelup]\n`[swordman]` 91 2 `[at swordman]` 72 2 `[demonic swordman]` 257 2\n[/skill levelup]\n[part set index] 90\n[durability] 45\n[weight] 3100\n[repair price] 560\n[value] 504\n[physical attack] 68\n[magical attack] 102\n[physical defense] 10\n[magical defense] 20\n[equipment physical attack] 962 830\n[equipment magical attack] 1063 918\n[separate attack] 624 404\n[attack speed] 80\n[cast speed] 40\n[stuck] 1\n[physical critical hit] 2\n[add physical critical hit] 4\n[add magical critical hit] 6\n[elemental property] `[dark element]`\n[/elemental property]\n[anti evil] 826\n",
		"stackable/material/cube.stk":                  "[name] `无色小晶块`\n[explain] `完成任务及施放高级技能所需的材料。`\n[minimum level] 1\n[rarity] 1\n[stackable type] `[material]`\n[stack limit] 999\n[usable period] 7\n",
	})
	if err != nil {
		t.Fatal(err)
	}
	service := &Service{pvfItemCatalog: catalog}

	equipment, err := service.GMItemDetail(context.Background(), 27000)
	if err != nil {
		t.Fatal(err)
	}
	if equipment.Item.Kind != "equipment" || equipment.FlavorText != "林纳斯留下的旧剑。" || equipment.Explain != "" {
		t.Fatalf("equipment detail = %+v", equipment)
	}
	if equipment.BasicExplain != "物理、 魔法和独立攻击力 +22%" || equipment.DetailExplain != "攻击时， 额外增加35%的伤害增加量。" {
		t.Fatalf("equipment explanations = %+v", equipment)
	}
	if equipment.Durability != 45 || equipment.Weight != 3100 || equipment.RepairPrice != 560 || equipment.Value != 504 {
		t.Fatalf("equipment numeric detail = %+v", equipment)
	}
	if equipment.QualityPercent != 100 || equipment.WeaponAttackSpeed != "fast" || equipment.ItemGroupName != "beamswd" ||
		!reflect.DeepEqual(equipment.ElementalProperties, []string{"dark element"}) {
		t.Fatalf("equipment presentation metadata = %+v", equipment)
	}
	if !reflect.DeepEqual(equipment.RequiredSkills, []GMItemRequiredSkill{
		{Job: "[swordman]", SkillID: 33, SkillName: "光剑掌握"},
		{Job: "[demonic swordman]", SkillID: 33, SkillName: "黑暗光剑掌握"},
		{Job: "[at swordman]", SkillID: 95, SkillName: "光剑掌握"},
	}) {
		t.Fatalf("required skills = %+v", equipment.RequiredSkills)
	}
	if !reflect.DeepEqual(equipment.SkillLevelUps, []GMItemSkillLevelUp{
		{Job: "[swordman]", SkillID: 91, SkillName: "极·鬼剑术（斩铁式）", Level: 2},
		{Job: "[at swordman]", SkillID: 72, SkillName: "三花聚顶", Level: 2},
		{Job: "[demonic swordman]", SkillID: 257, SkillName: "时间停滞", Level: 2},
	}) {
		t.Fatalf("skill level ups = %+v", equipment.SkillLevelUps)
	}
	if !reflect.DeepEqual(equipment.SkillModifiers, []GMItemSkillModifier{{SkillType: "physical", MPCostPercent: -10, CooldownPercent: -10}}) {
		t.Fatalf("skill modifiers = %+v", equipment.SkillModifiers)
	}
	if equipment.EquipmentSet == nil || equipment.EquipmentSet.SetID != 90 || equipment.EquipmentSet.Name != "测试荣光套装" ||
		len(equipment.EquipmentSet.Bonuses) != 2 || equipment.EquipmentSet.Bonuses[0].RequiredPieces != 3 ||
		equipment.EquipmentSet.Bonuses[0].BasicExplain != "攻击时， 附加15%的伤害。" ||
		len(equipment.EquipmentSet.Bonuses[0].Attributes) != 1 || equipment.EquipmentSet.Bonuses[0].Attributes[0].Name != "all_element_enhancement" ||
		equipment.EquipmentSet.Bonuses[0].Attributes[0].Value != 25 || equipment.EquipmentSet.Bonuses[1].RequiredPieces != 5 ||
		equipment.EquipmentSet.Bonuses[1].DetailExplain != "暴击伤害 +35%" {
		t.Fatalf("equipment set detail = %+v", equipment.EquipmentSet)
	}
	wantAttributes := map[string]int64{
		"physical_attack": 1058, "magical_attack": 1169, "independent_attack": 686,
		"strength": 75, "intelligence": 112, "vitality": 11, "spirit": 22,
		"cast_speed": 40, "hit_rate": -1, "physical_critical": 6, "magical_critical": 6,
		"anti_magic": 826,
	}
	for _, attribute := range equipment.Attributes {
		if want, found := wantAttributes[attribute.Name]; found {
			if attribute.Value != want {
				t.Fatalf("attribute %s = %d, want %d", attribute.Name, attribute.Value, want)
			}
			delete(wantAttributes, attribute.Name)
		}
	}
	if len(wantAttributes) != 0 {
		t.Fatalf("missing equipment attributes: %+v", wantAttributes)
	}

	stackable, err := service.GMItemDetail(context.Background(), 3037)
	if err != nil {
		t.Fatal(err)
	}
	if stackable.Item.Kind != "stackable" || stackable.Explain != "完成任务及施放高级技能所需的材料。" || stackable.FlavorText != "" {
		t.Fatalf("stackable detail = %+v", stackable)
	}
	if stackable.Item.StackLimit != 999 || stackable.Item.UsablePeriodDays != 7 || len(stackable.Attributes) != 0 {
		t.Fatalf("stackable metadata = %+v", stackable)
	}
}

func TestGMItemDetailRejectsUnknownItem(t *testing.T) {
	t.Parallel()
	catalog, err := newPVFDungeonDropCatalog(dungeonDropCatalogTestSource{
		"monster/monster.lst":     "",
		"equipment/equipment.lst": "",
		"stackable/stackable.lst": "",
	})
	if err != nil {
		t.Fatal(err)
	}
	service := &Service{pvfItemCatalog: catalog}
	if _, err := service.GMItemDetail(context.Background(), 999); err == nil {
		t.Fatal("unknown item detail succeeded")
	}
}

func TestGMItemDetailPreservesAvatarJobVariationsAndLayerOrder(t *testing.T) {
	t.Parallel()
	catalog, err := newPVFDungeonDropCatalog(dungeonDropCatalogTestSource{
		"monster/monster.lst":     "",
		"equipment/equipment.lst": "39401 `character/swordman/avatar/hair/test.equ`\n",
		"stackable/stackable.lst": "",
		"equipment/character/swordman/avatar/hair/test.equ": "[name] `测试头发`\n" +
			"[equipment type] `[hair avatar]`\n" +
			"[animation job] `[swordman]`\n" +
			"[variation] 1 2\n" +
			"[layer variation] 2000 `hair_a` 2050 `hair_c`\n" +
			"[animation job] `[demonic swordman]`\n" +
			"[variation] 114 0\n" +
			"[layer variation] 2100 `hair_a`\n",
	})
	if err != nil {
		t.Fatal(err)
	}
	service := &Service{pvfItemCatalog: catalog}
	detail, err := service.GMItemDetail(context.Background(), 39401)
	if err != nil {
		t.Fatal(err)
	}
	want := []GMAvatarVariation{
		{AnimationJob: "[swordman]", Variation: 1, Palette: 2, Layers: []GMAvatarLayer{{Order: 2000, Name: "hair_a"}, {Order: 2050, Name: "hair_c"}}},
		{AnimationJob: "[demonic swordman]", Variation: 114, Palette: 0, Layers: []GMAvatarLayer{{Order: 2100, Name: "hair_a"}}},
	}
	if !reflect.DeepEqual(detail.AvatarVariations, want) {
		t.Fatalf("avatar variations = %+v, want %+v", detail.AvatarVariations, want)
	}
}

func TestGMItemDetailInfersWeaponAvatarPreviewFromExpandAnimation(t *testing.T) {
	t.Parallel()
	catalog, err := newPVFDungeonDropCatalog(dungeonDropCatalogTestSource{
		"monster/monster.lst":     "",
		"stackable/stackable.lst": "",
		"equipment/equipment.lst": "112600001 `character/swordman/at_avatar/weapon/test.equ`\n",
		"equipment/character/swordman/at_avatar/weapon/test.equ": "[name] `测试武器装扮`\n" +
			"[equipment type] `[weapon avatar]`\n" +
			"[usable job] `[at swordman]`\n[/usable job]\n" +
			"[expand ani]\n" +
			"[variation] 26 3\n" +
			"[layer variation] 2050 `at_katanaa`\n" +
			"[layer variation] 650 `at_katanab`\n" +
			"[layer variation] 1350 `at_katanax`\n" +
			"[expand path] `equipment/character/swordman/weapon/katana/`\n" +
			"[/expand ani]\n",
	})
	if err != nil {
		t.Fatal(err)
	}
	service := &Service{pvfItemCatalog: catalog}
	detail, err := service.GMItemDetail(context.Background(), 112600001)
	if err != nil {
		t.Fatal(err)
	}
	want := []GMAvatarVariation{{
		AnimationJob: "[at swordman]",
		Variation:    26,
		Palette:      3,
		Layers: []GMAvatarLayer{
			{Order: 2050, Name: "at_katanaa"},
			{Order: 650, Name: "at_katanab"},
			{Order: 1350, Name: "at_katanax"},
		},
	}}
	if detail.AvatarWeaponGroup != "katana" || !reflect.DeepEqual(detail.AvatarVariations, want) {
		t.Fatalf("weapon avatar preview = group %q variations %+v, want group katana variations %+v", detail.AvatarWeaponGroup, detail.AvatarVariations, want)
	}
}

func TestGMItemDetailResolvesAuraForegroundAndBackgroundAnimationFrames(t *testing.T) {
	t.Parallel()
	catalog, err := newPVFDungeonDropCatalog(dungeonDropCatalogTestSource{
		"monster/monster.lst":     "",
		"stackable/stackable.lst": "",
		"equipment/equipment.lst": "101590019 `character/swordman/avatar/aura/test.equ`\n",
		"equipment/character/swordman/avatar/aura/test.equ": "[name] `测试光环`\n" +
			"[equipment type] `[aurora avatar]`\n" +
			"[aurora graphic effects]\n" +
			"1 `Character/Common/Animation/Aura/test_front_normal.ani` 0 " +
			"`Character/Common/Animation/Aura/test_back_normal.ani` 0 " +
			"`Character/Common/Animation/Aura/test_back_dodge.ani`\n" +
			"[/aurora graphic effects]\n",
		"character/common/animation/aura/test_front_normal.ani": "[LOOP] 1\n[FRAME MAX] 2\n" +
			"[FRAME000]\n[IMAGE] `Character/Common/Aura/test_front.img` 3\n[IMAGE POS] -110 -250\n[GRAPHIC EFFECT] `LINEARDODGE`\n[DELAY] 80\n" +
			"[FRAME001]\n[IMAGE] `Character/Common/Aura/test_front.img` 4\n[IMAGE POS] -109 -249\n[DELAY] 90\n",
		"character/common/animation/aura/test_back_normal.ani": "[LOOP] 1\n[FRAME MAX] 1\n" +
			"[FRAME000]\n[IMAGE] `Character/Common/Aura/test_back.img` 7\n[IMAGE POS] -120 -260\n[DELAY] 120\n",
		"character/common/animation/aura/test_back_dodge.ani": "[FRAME000]\n" +
			"[IMAGE] `Character/Common/Aura/test_back_dodge.img` 9\n[IMAGE POS] 0 0\n[DELAY] 100\n",
	})
	if err != nil {
		t.Fatal(err)
	}
	service := &Service{pvfItemCatalog: catalog}
	detail, err := service.GMItemDetail(context.Background(), 101590019)
	if err != nil {
		t.Fatal(err)
	}
	want := []GMAvatarAuraAnimation{
		{
			Layer:   "back",
			ANIPath: "Character/Common/Animation/Aura/test_back_normal.ani",
			Frames:  []GMAvatarAuraFrame{{ImagePath: "Character/Common/Aura/test_back.img", ImageIndex: 7, X: -120, Y: -260, Delay: 120}},
		},
		{
			Layer:   "front",
			ANIPath: "Character/Common/Animation/Aura/test_front_normal.ani",
			Frames: []GMAvatarAuraFrame{
				{ImagePath: "Character/Common/Aura/test_front.img", ImageIndex: 3, X: -110, Y: -250, Delay: 80, GraphicEffect: "lineardodge"},
				{ImagePath: "Character/Common/Aura/test_front.img", ImageIndex: 4, X: -109, Y: -249, Delay: 90},
			},
		},
	}
	if !reflect.DeepEqual(detail.AvatarAuraAnimations, want) {
		t.Fatalf("aura preview = %+v, want %+v", detail.AvatarAuraAnimations, want)
	}
}

func TestGMItemDetailUsesAuraLayerMarkersAndRepairsKnownDownPathTypo(t *testing.T) {
	t.Parallel()
	catalog, err := newPVFDungeonDropCatalog(dungeonDropCatalogTestSource{
		"monster/monster.lst":     "",
		"stackable/stackable.lst": "",
		"equipment/equipment.lst": "101590059 `character/swordman/avatar/aura/joyclub.equ`\n",
		"equipment/character/swordman/avatar/aura/joyclub.equ": "[name] `腾龙光环`\n" +
			"[equipment type] `[aurora avatar]`\n" +
			"[aurora graphic effects]\n" +
			"0 `Character/Common/Animation/Aura/vip3/down_dodge.ani` " +
			"0 `Character/Common/Animation/Aura/vip3/donw_normal.ani` " +
			"1 `Character/Common/Animation/Aura/vip3/up_normal.ani` " +
			"1 `Character/Common/Animation/Aura/vip3/up_dodge.ani`\n" +
			"[/aurora graphic effects]\n",
		"character/common/animation/aura/vip3/down_normal.ani": "[FRAME000]\n" +
			"[IMAGE] `Character/Common/Aura/vip3/down.img` 5\n[IMAGE POS] -300 -500\n[DELAY] 100\n",
		"character/common/animation/aura/vip3/up_normal.ani": "[FRAME000]\n" +
			"[IMAGE] `Character/Common/Aura/vip3/up.img` 7\n[IMAGE POS] -310 -510\n[DELAY] 90\n",
	})
	if err != nil {
		t.Fatal(err)
	}
	service := &Service{pvfItemCatalog: catalog}
	detail, err := service.GMItemDetail(context.Background(), 101590059)
	if err != nil {
		t.Fatal(err)
	}
	want := []GMAvatarAuraAnimation{
		{Layer: "back", ANIPath: "character/common/animation/aura/vip3/down_normal.ani", Frames: []GMAvatarAuraFrame{{ImagePath: "Character/Common/Aura/vip3/down.img", ImageIndex: 5, X: -300, Y: -500, Delay: 100}}},
		{Layer: "front", ANIPath: "Character/Common/Animation/Aura/vip3/up_normal.ani", Frames: []GMAvatarAuraFrame{{ImagePath: "Character/Common/Aura/vip3/up.img", ImageIndex: 7, X: -310, Y: -510, Delay: 90}}},
	}
	if !reflect.DeepEqual(detail.AvatarAuraAnimations, want) {
		t.Fatalf("marker aura preview = %+v, want %+v", detail.AvatarAuraAnimations, want)
	}
}
