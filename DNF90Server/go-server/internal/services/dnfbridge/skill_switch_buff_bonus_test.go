package dnfbridge

import (
	"encoding/binary"
	"os"
	"testing"

	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

func parseSkillSwitchDoc(t *testing.T, text string) *dnfpvf.Document {
	t.Helper()
	doc, err := dnfpvf.Parse("test.equ", text)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return doc
}

func TestSkillSwitchDocumentBuffLevelBonusExact(t *testing.T) {
	doc := parseSkillSwitchDoc(t, `
[skill levelup]
`+"`[swordman]`"+` 38 1 `+"`[at swordman]`"+` 72 2
[/skill levelup]
`)
	got := skillSwitchDocumentBuffLevelBonus(doc, "swordman", 38, 25, true, 0)
	if got != 1 {
		t.Fatalf("exact bonus=%d want=1", got)
	}
	// different skill id
	if got := skillSwitchDocumentBuffLevelBonus(doc, "swordman", 72, 25, true, 0); got != 0 {
		t.Fatalf("skill 72 for swordman bonus=%d want=0", got)
	}
	// different job
	if got := skillSwitchDocumentBuffLevelBonus(doc, "at swordman", 72, 25, true, 0); got != 2 {
		t.Fatalf("at swordman skill 72 bonus=%d want=2", got)
	}
}

func TestSkillSwitchDocumentBuffLevelBonusNegative(t *testing.T) {
	doc := parseSkillSwitchDoc(t, `
[skill levelup]
`+"`[knight]`"+` 87 -1
[/skill levelup]
`)
	if got := skillSwitchDocumentBuffLevelBonus(doc, "knight", 87, 25, true, 0); got != -1 {
		t.Fatalf("negative bonus=%d want=-1", got)
	}
}

func TestSkillSwitchDocumentBuffLevelBonusRange(t *testing.T) {
	doc := parseSkillSwitchDoc(t, `
[all skill item container]
[all skill item]
[item growtype]
`+"`[swordman]`"+` 0
[skill apply condition]
[skill group]
`+"`[all]`"+`
[lower bound level]
1
[upper bound level]
45
[value]
1
[extra condition]
`+"`[except for ex skill]`"+`
[/extra condition]
[/skill apply condition]
[skill phase]
`+"`[all]`"+`
[/all skill item]
[/all skill item container]
`)
	if got := skillSwitchDocumentBuffLevelBonus(doc, "swordman", 38, 25, true, 0); got != 1 {
		t.Fatalf("range bonus=%d want=1", got)
	}
	// required level outside the range
	if got := skillSwitchDocumentBuffLevelBonus(doc, "swordman", 38, 60, true, 0); got != 0 {
		t.Fatalf("out-of-range bonus=%d want=0", got)
	}
	// wrong job tag
	if got := skillSwitchDocumentBuffLevelBonus(doc, "at swordman", 38, 25, true, 0); got != 0 {
		t.Fatalf("wrong-job range bonus=%d want=0", got)
	}
}

func TestSkillSwitchDocumentBuffLevelBonusRangeActiveOnly(t *testing.T) {
	doc := parseSkillSwitchDoc(t, `
[all skill item]
[item growtype]
`+"`[swordman]`"+` 0
[skill apply condition]
[skill group]
`+"`[active]`"+`
[lower bound level]
1
[upper bound level]
45
[value]
1
[/skill apply condition]
[/all skill item]
`)
	if got := skillSwitchDocumentBuffLevelBonus(doc, "swordman", 38, 25, true, 0); got != 1 {
		t.Fatalf("active skill bonus=%d want=1", got)
	}
	if got := skillSwitchDocumentBuffLevelBonus(doc, "swordman", 38, 25, false, 0); got != 0 {
		t.Fatalf("passive skill active-only bonus=%d want=0", got)
	}
}

func TestSkillSwitchDocumentBuffLevelBonusSkipsEnchant(t *testing.T) {
	doc := parseSkillSwitchDoc(t, `
[enchant]
[skill levelup]
`+"`[swordman]`"+` 38 1
[/skill levelup]
[/enchant]
`)
	if got := skillSwitchDocumentBuffLevelBonus(doc, "swordman", 38, 25, true, 0); got != 0 {
		t.Fatalf("enchant-nested bonus=%d want=0", got)
	}
}

func TestSkillSwitchDocumentBuffLevelBonusStageGate(t *testing.T) {
	doc := parseSkillSwitchDoc(t, `
[all skill item]
[item growtype]
`+"`[swordman]`"+` 2
[skill apply condition]
[skill group]
`+"`[all]`"+`
[lower bound level]
1
[upper bound level]
45
[value]
1
[/skill apply condition]
[/all skill item]
`)
	if got := skillSwitchDocumentBuffLevelBonus(doc, "swordman", 38, 25, true, 2); got != 1 {
		t.Fatalf("stage 2 character bonus=%d want=1", got)
	}
	if got := skillSwitchDocumentBuffLevelBonus(doc, "swordman", 38, 25, true, 1); got != 0 {
		t.Fatalf("stage 1 character bonus=%d want=0", got)
	}
}

func TestSkillSwitchDocumentBuffLevelBonusGroupScopedLevelup(t *testing.T) {
	doc := parseSkillSwitchDoc(t, `
[all skill item]
[item growtype]
`+"`[at swordman]`"+` 0
[skill levelup]
`+"`[at swordman]`"+` 72 1
[/skill levelup]
[/all skill item]
`)
	// Group is scoped to [at swordman]; a [swordman] character must not gain it.
	if got := skillSwitchDocumentBuffLevelBonus(doc, "swordman", 72, 25, true, 0); got != 0 {
		t.Fatalf("wrong-group levelup bonus=%d want=0", got)
	}
	if got := skillSwitchDocumentBuffLevelBonus(doc, "at swordman", 72, 25, true, 0); got != 1 {
		t.Fatalf("matching-group levelup bonus=%d want=1", got)
	}
}

func skillSwitchEmbeddedTestCatalog(t *testing.T) *pvfDungeonDropCatalog {
	t.Helper()
	source := dungeonDropCatalogTestSource{
		"monster/monster.lst":     "",
		"stackable/stackable.lst": "10001 `card/test_card.stk`\n10002 `emblem/test_emblem.stk`\n10003 `card/test_range_card.stk`\n",
		"equipment/equipment.lst": "",
		"stackable/card/test_card.stk": "[name] `Test Card`\n[stackable type] `[material expert job]`\n[item category] `monster card`\n[/item category]\n" +
			"[enchant table]\n[enchant index]\n0\n[skill levelup]\n`[all]` 38 1\n[/skill levelup]\n[/enchant index]\n" +
			"[enchant index]\n1\n[skill levelup]\n`[all]` 38 2\n[/skill levelup]\n[/enchant index]\n[/enchant table]\n",
		"stackable/card/test_range_card.stk": "[name] `Test Range Card`\n[stackable type] `[material expert job]`\n[item category] `monster card`\n[/item category]\n" +
			"[enchant table]\n[enchant index]\n0\n[all skill item container]\n[all skill item]\n[item growtype]\n`[all]` 0\n" +
			"[skill apply condition]\n[skill group]\n`[active]`\n[lower bound level]\n1\n[upper bound level]\n50\n[value]\n1\n" +
			"[extra condition]\n`[except for ex skill]`\n[/extra condition]\n[/skill apply condition]\n[skill phase]\n`[all]`\n" +
			"[/all skill item]\n[/all skill item container]\n[/enchant index]\n[/enchant table]\n",
		"stackable/emblem/test_emblem.stk": "[name] `Test Emblem`\n[stackable type] `[avatar emblem]`\n[enchant]\n[skill levelup]\n`[at swordman]` 38 1\n[/skill levelup]\n[/enchant]\n",
	}
	catalog, err := newPVFDungeonDropCatalog(source)
	if err != nil {
		t.Fatal(err)
	}
	return catalog
}

func TestSkillSwitchEnchantCardBuffLevelBonus(t *testing.T) {
	catalog := skillSwitchEmbeddedTestCatalog(t)
	if got := skillSwitchEnchantCardBuffLevelBonus(catalog, 10001, 0, "swordman", 38, 25, true, 0); got != 1 {
		t.Fatalf("upgrade 0 bonus=%d want=1", got)
	}
	if got := skillSwitchEnchantCardBuffLevelBonus(catalog, 10001, 1, "swordman", 38, 25, true, 0); got != 2 {
		t.Fatalf("upgrade 1 bonus=%d want=2", got)
	}
	if got := skillSwitchEnchantCardBuffLevelBonus(catalog, 10001, 9, "swordman", 38, 25, true, 0); got != 1 {
		t.Fatalf("missing upgrade fallback bonus=%d want=1", got)
	}
	if got := skillSwitchEnchantCardBuffLevelBonus(catalog, 10001, 0, "swordman", 72, 25, true, 0); got != 0 {
		t.Fatalf("different skill bonus=%d want=0", got)
	}
	// Range form inside the enchant index (the "1..50 skills +1" card).
	if got := skillSwitchEnchantCardBuffLevelBonus(catalog, 10003, 0, "swordman", 38, 25, true, 0); got != 1 {
		t.Fatalf("range card bonus=%d want=1", got)
	}
	if got := skillSwitchEnchantCardBuffLevelBonus(catalog, 10003, 0, "swordman", 38, 60, true, 0); got != 0 {
		t.Fatalf("range card out-of-range bonus=%d want=0", got)
	}
}

func TestSkillSwitchEmblemBuffLevelBonus(t *testing.T) {
	catalog := skillSwitchEmbeddedTestCatalog(t)
	if got := skillSwitchEmblemBuffLevelBonus(catalog, 10002, "at swordman", 38, 25, true, 0); got != 1 {
		t.Fatalf("at swordman emblem bonus=%d want=1", got)
	}
	if got := skillSwitchEmblemBuffLevelBonus(catalog, 10002, "swordman", 38, 25, true, 0); got != 0 {
		t.Fatalf("wrong job emblem bonus=%d want=0", got)
	}
}

func TestSkillSwitchEnchantCardIDAndUpgrade(t *testing.T) {
	if id, ok := skillSwitchEnchantCardID(map[string]string{"enchant_card_id": "10008713"}); !ok || id != 10008713 {
		t.Fatalf("enchant card id=%d ok=%t", id, ok)
	}
	if id, ok := skillSwitchEnchantCardID(map[string]string{"value_a": "10008713"}); ok && id != 0 {
		t.Fatalf("value_a must not be treated as enchant card id: %d", id)
	}
	if got := skillSwitchEnchantUpgradeCount(map[string]string{"enchant_upgrade_count": "2"}); got != 2 {
		t.Fatalf("upgrade=%d want=2", got)
	}
}

func TestSkillSwitchEmblemItemIDs(t *testing.T) {
	raw := make([]byte, currentItemListEntryWireSize)
	raw[currentEquipmentVectorOffset] = 2
	binary.LittleEndian.PutUint32(raw[currentEquipmentVectorOffset+1:currentEquipmentVectorOffset+5], 10002)
	binary.LittleEndian.PutUint32(raw[currentEquipmentVectorOffset+5:currentEquipmentVectorOffset+9], 10003)
	ids := skillSwitchEmblemItemIDs(nil, raw)
	if len(ids) != 2 || ids[0] != 10002 || ids[1] != 10003 {
		t.Fatalf("emblem ids=%v", ids)
	}
}

func TestSkillSwitchRegisteredItemInstance(t *testing.T) {
	inventory := dnfrepo.InventoryRecord{
		Slots: map[string]dnfrepo.ItemStack{
			"0:17": {ItemID: 600, Extra: map[string]string{"item_serial": "20"}},
		},
		Warehouse: map[string]dnfrepo.ItemStack{},
	}
	equipment := dnfrepo.EquipmentRecord{Entries: map[string]dnfrepo.EquipmentEntry{}}
	extra, raw, found := skillSwitchRegisteredItemInstance(
		skillSwitchBinding{EquipmentPart: 12, ItemSerial: 20, ItemID: 600},
		inventory,
		equipment,
	)
	if !found || extra["item_serial"] != "20" {
		t.Fatalf("instance found=%t extra=%v raw=%v", found, extra, raw)
	}
	if _, _, found := skillSwitchRegisteredItemInstance(
		skillSwitchBinding{EquipmentPart: 12, ItemSerial: 21, ItemID: 600},
		inventory,
		equipment,
	); found {
		t.Fatalf("missing serial must not resolve")
	}
}

func TestSkillSwitchLegacyEnchantCardID(t *testing.T) {
	catalog := skillSwitchEmbeddedTestCatalog(t)
	raw := make([]byte, currentItemListEntryWireSize)
	binary.LittleEndian.PutUint32(raw[0x0E:0x12], 10001)
	if id, ok := skillSwitchLegacyEnchantCardID(catalog, raw); !ok || id != 10001 {
		t.Fatalf("legacy card id=%d ok=%t", id, ok)
	}
	binary.LittleEndian.PutUint32(raw[0x0E:0x12], 99999999)
	if _, ok := skillSwitchLegacyEnchantCardID(catalog, raw); ok {
		t.Fatalf("unresolvable seed must not resolve as card")
	}
}

// TestSkillSwitchRealPVFBuffBonusSmoke exercises the bonus parsers against the
// shipped Script.pvf. Set DNFBRIDGE_REAL_PVF_SMOKE to the Script.pvf path to
// enable it.
func TestSkillSwitchRealPVFBuffBonusSmoke(t *testing.T) {
	pvfPath := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE to verify against the shipped Script.pvf")
	}
	archive, err := platformpvf.Open(pvfPath)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := newPVFDungeonDropCatalog(archive)
	if err != nil {
		t.Fatal(err)
	}

	// 圣物 : 暗影精灵之哨兵侍剑 carries 1..45 skill +1 ranges plus a
	// [knight] 87 -1 exact row.
	if _, err := catalog.ResolveItem(101000607); err == nil {
		bonus, _ := skillSwitchItemBuffLevelBonus(catalog, 101000607, "swordman", 38, 25, true, 0)
		if bonus < 0 {
			t.Fatalf("real sword weapon bonus=%d want >= 0", bonus)
		}
	} else {
		t.Logf("skip real sword weapon: %v", err)
	}

	// 白金徽章 [魔镜幻影阵] embeds [at swordman] 69 +1.
	if _, err := catalog.ResolveItem(10088976); err == nil {
		bonus := skillSwitchEmblemBuffLevelBonus(catalog, 10088976, "at swordman", 69, 25, true, 0)
		if bonus != 1 {
			t.Fatalf("real platinum emblem bonus=%d want=1", bonus)
		}
	} else {
		t.Logf("skip real platinum emblem: %v", err)
	}

	// A monster card with `[all] 188 1 / [all] 189 1` at enchant index 0.
	if _, err := catalog.ResolveItem(10006481); err == nil {
		bonus := skillSwitchEnchantCardBuffLevelBonus(catalog, 10006481, 0, "swordman", 188, 25, true, 0)
		if bonus != 1 {
			t.Fatalf("real monster card bonus=%d want=1", bonus)
		}
	} else {
		t.Logf("skip real monster card: %v", err)
	}

	// A range-form monster card: [enchant index] carries a 15..15 active
	// skill +1 [all skill item] block.
	if _, err := catalog.ResolveItem(490005834); err == nil {
		bonus := skillSwitchEnchantCardBuffLevelBonus(catalog, 490005834, 0, "swordman", 38, 15, true, 0)
		if bonus != 1 {
			t.Fatalf("real range card bonus=%d want=1", bonus)
		}
		bonus = skillSwitchEnchantCardBuffLevelBonus(catalog, 490005834, 0, "swordman", 38, 25, true, 0)
		if bonus != 0 {
			t.Fatalf("real range card out-of-range bonus=%d want=0", bonus)
		}
	} else {
		t.Logf("skip real range card: %v", err)
	}

	// 上衣装扮 415500013: [ability case index] 1450, ability 0 = [at priest] 14 +1.
	if _, err := catalog.ResolveItem(415500013); err == nil {
		bonus := skillSwitchAvatarAbilitySkillBonus(catalog, 415500013, 0, "at priest", 14)
		if bonus != 1 {
			t.Fatalf("real avatar ability bonus=%d want=1", bonus)
		}
	} else {
		t.Logf("skip real avatar: %v", err)
	}
}

func TestSkillSwitchAvatarAbilitySkillBonus(t *testing.T) {
	source := dungeonDropCatalogTestSource{
		"monster/monster.lst":       "",
		"stackable/stackable.lst":   "",
		"equipment/equipment.lst":   "415500013 `avatar/coat.equ`\n",
		"equipment/avatar/coat.equ": "[name] `Test Avatar`\n[equipment type] `[coat avatar]`\n[ability case index]\n1450\n",
		"skill/abilitydatas.dat":    "[ability case]\n1450 0 `[SKILL_LEVEL]` `[at priest]` 14 1 1 `[SKILL_LEVEL]` `[at priest]` 15 1\n[/ability case]\n",
	}
	catalog, err := newPVFDungeonDropCatalog(source)
	if err != nil {
		t.Fatal(err)
	}
	if got := skillSwitchAvatarAbilitySkillBonus(catalog, 415500013, 0, "at priest", 14); got != 1 {
		t.Fatalf("ability 0 buff skill bonus=%d want=1", got)
	}
	if got := skillSwitchAvatarAbilitySkillBonus(catalog, 415500013, 1, "at priest", 14); got != 0 {
		t.Fatalf("ability 1 non-buff skill bonus=%d want=0", got)
	}
	if got := skillSwitchAvatarAbilitySkillBonus(catalog, 415500013, 0, "priest", 14); got != 0 {
		t.Fatalf("wrong job ability bonus=%d want=0", got)
	}
}

func TestSkillSwitchAvatarAbilityNo(t *testing.T) {
	if no, ok := skillSwitchAvatarAbilityNo(map[string]string{"avatar_ability_no": "7"}); !ok || no != 7 {
		t.Fatalf("ability no=%d ok=%t", no, ok)
	}
	if _, ok := skillSwitchAvatarAbilityNo(map[string]string{}); ok {
		t.Fatalf("missing ability must not resolve")
	}
}
