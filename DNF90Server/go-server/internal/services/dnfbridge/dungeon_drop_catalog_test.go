package dnfbridge

import (
	"errors"
	"os"
	"reflect"
	"testing"
	"time"

	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

type dungeonDropCatalogTestSource map[string]string

func (s dungeonDropCatalogTestSource) ReadText(relativePath string) (string, error) {
	text, ok := s[cleanDungeonDropPath(relativePath)]
	if !ok {
		return "", errors.New("test PVF document missing: " + relativePath)
	}
	return text, nil
}

func TestPVFDungeonDropCatalogMapsMonsterItemPoolToRealItemDocuments(t *testing.T) {
	source := dungeonDropCatalogTestSource{
		"monster/monster.lst":                        "65005 `NewMonsters/Goblin/Madman_item.mob`\n",
		"stackable/stackable.lst":                    "3227 `material/solvent_magicpower.stk`\n4225 `quest/charm_soul.stk`\n",
		"equipment/equipment.lst":                    "9001 `weapon/test_sword.equ`\n",
		"monster/NewMonsters/Goblin/Madman_item.mob": "[item]\n3227 3\n4225 7\n9001 2\n[/item]\n",
		"stackable/material/solvent_magicpower.stk":  "[name] `Magic Solvent`\n[stackable type] `[material]`\n[stack limit] 999\n[usable period] 30\n[expiration date] `2028-08-16 06:00:00`\n",
		"stackable/quest/charm_soul.stk":             "[name] `Charm Soul`\n",
		"equipment/weapon/test_sword.equ":            "[name] `Test Sword`\n[durability] 57\n[usable period] 2\n",
	}
	catalog, err := newPVFDungeonDropCatalog(source)
	if err != nil {
		t.Fatal(err)
	}
	pool, err := catalog.MonsterPool(65005)
	if err != nil {
		t.Fatal(err)
	}
	wantPool := []dungeonMonsterDropPoolEntry{
		{ItemID: 3227, Weight: 3},
		{ItemID: 4225, Weight: 7},
		{ItemID: 9001, Weight: 2},
	}
	if !reflect.DeepEqual(pool, wantPool) {
		t.Fatalf("pool = %+v, want %+v", pool, wantPool)
	}

	stackable, err := catalog.ResolveItem(3227)
	if err != nil {
		t.Fatal(err)
	}
	if stackable.Kind != dungeonDropItemStackable || stackable.PVFPath != "stackable/material/solvent_magicpower.stk" ||
		stackable.StackableType != "[material]" || stackable.StackLimit != 999 || stackable.UsablePeriodDays != 30 ||
		stackable.SlotStart != 121 || stackable.SlotEnd != 176 || stackable.Durability != 0 ||
		!stackable.ExpirationDate.Equal(time.Date(2028, time.August, 15, 22, 0, 0, 0, time.UTC)) {
		t.Fatalf("stackable = %+v", stackable)
	}
	equipment, err := catalog.ResolveItem(9001)
	if err != nil {
		t.Fatal(err)
	}
	if equipment.Kind != dungeonDropItemEquipment || equipment.PVFPath != "equipment/weapon/test_sword.equ" ||
		equipment.SlotStart != 9 || equipment.SlotEnd != 64 || equipment.Durability != 57 || equipment.UsablePeriodDays != 2 {
		t.Fatalf("equipment = %+v", equipment)
	}
	if _, err := catalog.ResolveItem(7777); !errors.Is(err, errDungeonDropItemUnresolved) {
		t.Fatalf("unresolved item error = %v", err)
	}
}

func TestParseDungeonMonsterDropPoolRejectsIncompleteAndInvalidPairs(t *testing.T) {
	document, err := dnfpvf.Parse("monster/test.mob", "[item]\n100 5\n0 7\n200 0\n300 -1\n400\n[/item]\n")
	if err != nil {
		t.Fatal(err)
	}
	pool := parseDungeonMonsterDropPool(document)
	want := []dungeonMonsterDropPoolEntry{{ItemID: 100, Weight: 5}}
	if !reflect.DeepEqual(pool, want) {
		t.Fatalf("pool = %+v, want %+v", pool, want)
	}
}

func TestPVFDungeonDropCatalogInfersOnlyProvenGoldKaleidoMissingType(t *testing.T) {
	source := dungeonDropCatalogTestSource{
		"monster/monster.lst":                   "1 `dummy.mob`\n",
		"stackable/stackable.lst":               "490008569 `490008001/chn_490008569.stk`\n490008570 `490008001/unknown.stk`\n",
		"equipment/equipment.lst":               "",
		"stackable/490008001/chn_490008569.stk": "[name] `Golden Kaleido`\n[action type] `[gold kaleido box]` 85\n[/action type]\n",
		"stackable/490008001/unknown.stk":       "[name] `Unknown`\n[action type] `[unknown action]` 1\n[/action type]\n",
	}
	catalog, err := newPVFDungeonDropCatalog(source)
	if err != nil {
		t.Fatal(err)
	}
	gold, err := catalog.ResolveItem(490008569)
	if err != nil {
		t.Fatal(err)
	}
	if gold.StackableType != "[gold kaleido]" || gold.SlotStart != 65 || gold.SlotEnd != 120 {
		t.Fatalf("gold kaleido definition=%+v", gold)
	}
	unknown, err := catalog.ResolveItem(490008570)
	if err != nil {
		t.Fatal(err)
	}
	if unknown.StackableType != "" {
		t.Fatalf("unknown missing type was invented: %+v", unknown)
	}
}

func TestSelectDungeonMonsterDropUsesPVFWeightsWithoutInventingDropRate(t *testing.T) {
	pool := []dungeonMonsterDropPoolEntry{
		{ItemID: 100, Weight: 2},
		{ItemID: 200, Weight: 3},
	}
	tests := []struct {
		roll uint64
		want uint32
	}{
		{roll: 0, want: 100},
		{roll: 1, want: 100},
		{roll: 2, want: 200},
		{roll: 4, want: 200},
		{roll: 5, want: 100},
	}
	for _, test := range tests {
		got, ok := selectDungeonMonsterDrop(pool, test.roll)
		if !ok || got.ItemID != test.want {
			t.Fatalf("roll %d = %+v/%t, want item %d", test.roll, got, ok, test.want)
		}
	}
}

// 通用装备池必须按 .equ 自己的 ([rarity], [minimum level]) 建键、按 [creation rate]
// 取权重。这条测试同时钉住三个曾经的错误来源：
//  1. Etc/ItemDictionary*.etc 的 column1 是**别名编码**的稀有度（regional 用 9 表示
//     稀有、5/10 表示神器），把它当 [rarity] 用会让高等级稀有/神器整档没有候选；
//  2. 没有 [creation rate] 的装备不该进池（参考实现同样要求 creationRate > 0）；
//  3. 时装/宠物/称号/光环/勋章不进普通掉落池。
func TestPVFDungeonDropCatalogBuildsEquipmentPoolFromDocuments(t *testing.T) {
	source := dungeonDropCatalogTestSource{
		"monster/monster.lst":     "1 `dummy.mob`\n",
		"stackable/stackable.lst": "1 `dummy.stk`\n",
		"equipment/equipment.lst": "900 `normal.equ`\n901 `rare.equ`\n902 `no_rate.equ`\n903 `avatar.equ`\n",
		"stackable/dummy.stk":     "[creation rate] 0\n",
		"equipment/normal.equ":    "[rarity] 0\n[minimum level] 70\n[creation rate] 7\n[equipment type] `[coat]`\n",
		"equipment/rare.equ":      "[rarity] 2\n[minimum level] 85\n[creation rate] 9\n[equipment type] `[coat]`\n",
		"equipment/no_rate.equ":   "[rarity] 2\n[minimum level] 85\n[equipment type] `[coat]`\n",
		"equipment/avatar.equ":    "[rarity] 1\n[minimum level] 85\n[creation rate] 5\n[equipment type] `[coat avatar]`\n",
	}
	catalog, err := newPVFDungeonDropCatalog(source)
	if err != nil {
		t.Fatal(err)
	}
	if err := catalog.ensureGenericPools(); err != nil {
		t.Fatal(err)
	}
	normalKey := dungeonDropGradeRarityKey{Grade: 70, Rarity: 0}
	if got := catalog.genericEquipment[normalKey]; !reflect.DeepEqual(got, []dungeonDropWeightedItem{{ItemID: 900, Weight: 7}}) {
		t.Fatalf("rarity 0 bucket=%+v", got)
	}
	rareKey := dungeonDropGradeRarityKey{Grade: 85, Rarity: 2}
	if got := catalog.genericEquipment[rareKey]; !reflect.DeepEqual(got, []dungeonDropWeightedItem{{ItemID: 901, Weight: 9}}) {
		t.Fatalf("rarity 2 bucket=%+v", got)
	}
	window := catalog.genericEquipmentCandidates(84, 7, 3, 2)
	if !reflect.DeepEqual(window, []dungeonDropWeightedItem{{ItemID: 901, Weight: 9}}) {
		t.Fatalf("level 84 window rarity 2=%+v", window)
	}
	if got := catalog.genericEquipmentCandidates(84, 7, 3, 1); len(got) != 0 {
		t.Fatalf("avatar/creation-rate-less item leaked into the generic pool: %+v", got)
	}
	audit := catalog.GenericPoolAudit()
	if audit.Scanned != 4 || audit.Items != 2 || audit.CreationRateAbsent != 1 || audit.ExcludedEquipmentType != 1 {
		t.Fatalf("equipment pool audit=%+v", audit)
	}
	if audit.ByRarity[0] != 1 || audit.ByRarity[2] != 1 {
		t.Fatalf("equipment pool audit by rarity=%v", audit.ByRarity)
	}
}

// 一件读不到的 .equ 只跳过它自己，不能让整个通用池建不起来。
func TestPVFDungeonDropCatalogSkipsUnreadableEquipmentDocument(t *testing.T) {
	source := dungeonDropCatalogTestSource{
		"monster/monster.lst":     "1 `dummy.mob`\n",
		"stackable/stackable.lst": "1 `dummy.stk`\n",
		"equipment/equipment.lst": "900 `good.equ`\n901 `missing.equ`\n",
		"stackable/dummy.stk":     "[creation rate] 0\n",
		"equipment/good.equ":      "[rarity] 1\n[minimum level] 90\n[creation rate] 3\n[equipment type] `[weapon]`\n",
	}
	catalog, err := newPVFDungeonDropCatalog(source)
	if err != nil {
		t.Fatal(err)
	}
	if err := catalog.ensureGenericPools(); err != nil {
		t.Fatal(err)
	}
	key := dungeonDropGradeRarityKey{Grade: 90, Rarity: 1}
	if got := catalog.genericEquipment[key]; !reflect.DeepEqual(got, []dungeonDropWeightedItem{{ItemID: 900, Weight: 3}}) {
		t.Fatalf("rarity 1 bucket=%+v", got)
	}
	if audit := catalog.GenericPoolAudit(); audit.ReadFailed != 1 {
		t.Fatalf("equipment pool audit=%+v", audit)
	}
}

// 稀有度空池降级到 rarity 0 是既有行为（参考实现也 allowFallback），但必须留下记录。
// 修复前它是完全静默的：rarity 2 的 roll 会安静地变成一件白装，日志上看不出任何异常。
func TestSelectGenericEquipmentRecordsSilentRarityDegradation(t *testing.T) {
	source := dungeonDropCatalogTestSource{
		"monster/monster.lst":     "1 `dummy.mob`\n",
		"stackable/stackable.lst": "1 `dummy.stk`\n",
		"equipment/equipment.lst": "900 `normal.equ`\n",
		"stackable/dummy.stk":     "[creation rate] 0\n",
		"equipment/normal.equ":    "[rarity] 0\n[minimum level] 90\n[creation rate] 4\n[equipment type] `[coat]`\n",
	}
	catalog, err := newPVFDungeonDropCatalog(source)
	if err != nil {
		t.Fatal(err)
	}
	if err := catalog.ensureGenericPools(); err != nil {
		t.Fatal(err)
	}
	before := catalog.GenericFallbackMarks()
	itemID, found, err := catalog.SelectGenericEquipment(newCurrentDungeonDropLCG(1), 90, 6, 3, 2)
	if err != nil || !found || itemID != 900 {
		t.Fatalf("degraded selection item=%d found=%t err=%v", itemID, found, err)
	}
	after := catalog.GenericFallbackMarks()
	key := dungeonDropGradeRarityKey{Grade: 90, Rarity: 2}
	if before[key] != 0 || after[key] != 1 {
		t.Fatalf("fallback marks before=%v after=%v", before, after)
	}
	if audit := catalog.GenericPoolAudit(); audit.Fallbacks != 1 {
		t.Fatalf("pool audit fallbacks=%d", audit.Fallbacks)
	}
	if _, _, err := catalog.SelectGenericEquipment(newCurrentDungeonDropLCG(2), 90, 6, 3, 1); err != nil {
		t.Fatal(err)
	}
	if after = catalog.GenericFallbackMarks(); after[key] != 1 {
		t.Fatalf("non-degrading roll was recorded as a fallback: %v", after)
	}
}

func TestDungeonDropStackableQuickSlotPreferenceUsesPVFType(t *testing.T) {
	tests := []struct {
		stackableType string
		want          bool
	}{
		{stackableType: "[waste]", want: true},
		{stackableType: "`[WASTE]`", want: true},
		{stackableType: "[material]", want: false},
		{stackableType: "[material] 4", want: false},
		{stackableType: "[quest]", want: false},
		{stackableType: "[material expert job]", want: false},
		{stackableType: "[avatar emblem]", want: false},
		{stackableType: "", want: false},
	}
	for _, test := range tests {
		if got := dungeonDropStackablePrefersItemQuickSlots(test.stackableType); got != test.want {
			t.Fatalf("stackable type %q quick-slot preference=%t want=%t", test.stackableType, got, test.want)
		}
	}
}

// TestDungeonDropStackableSlotRangeMatchesCurrentClientPages 锁住每个 [stackable type] 的
// 落点页，尤其是徽章页的**末格**。
//
// 现场 2026-10-03（服主报）：「徽章栏最后一行不给用，一行 7 格、总共 9 行」。
// 徽章页是 289..351（9 行 × 7 格 = 63），这里过去写的是 289..344（只到第 8 行），
// 345..351 这一行根本没被任何发放路径用过（逐角色普查：整库该区段 0 行）。
// 损失是实的：char=1 的 56 格被 56 种徽章占满后，新徽章只能进溢出邮件，而邮件领取又
// 只能往同一页放 —— 那封邮件就永久领不出来（mail 110 的 灿烂的绿色徽章[物理暴击]
// 就是这么卡死的）。所以末格必须钉死在用例里。
func TestDungeonDropStackableSlotRangeMatchesCurrentClientPages(t *testing.T) {
	tests := []struct {
		name        string
		stackable   string
		wantStart   int16
		wantEnd     int16
		wantPageLen int16
	}{
		{name: "兜底页", stackable: "[unknown]", wantStart: 65, wantEnd: 120, wantPageLen: 56},
		{name: "材料页", stackable: "[material]", wantStart: 121, wantEnd: 176, wantPageLen: 56},
		{name: "任务页", stackable: "[quest]", wantStart: 177, wantEnd: 232, wantPageLen: 56},
		{name: "副职业材料页", stackable: "[material expert job]", wantStart: 233, wantEnd: 288, wantPageLen: 56},
		{name: "固定材料区段", stackable: "[material] 4", wantStart: 345, wantEnd: 359, wantPageLen: 15},
		// 徽章页：9 行 × 7 格 = 63，不是 56。
		{name: "徽章页九行", stackable: "[avatar emblem]", wantStart: 289, wantEnd: 351, wantPageLen: 63},
		{name: "徽章页写法不敏感", stackable: " `[Avatar Emblem]` ", wantStart: 289, wantEnd: 351, wantPageLen: 63},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			start, end := dungeonDropStackableSlotRange(test.stackable)
			if start != test.wantStart || end != test.wantEnd {
				t.Fatalf("range=%d..%d want=%d..%d", start, end, test.wantStart, test.wantEnd)
			}
			if length := end - start + 1; length != test.wantPageLen {
				t.Fatalf("page length=%d want=%d", length, test.wantPageLen)
			}
		})
	}
	if currentAvatarEmblemPageSlotStart != 289 || currentAvatarEmblemPageSlotEnd != 351 {
		t.Fatalf("徽章页常量漂了：%d..%d", currentAvatarEmblemPageSlotStart, currentAvatarEmblemPageSlotEnd)
	}
}

func TestRealPVFMaterialPickupAndMailboxPlacementStayInMaterialCategory(t *testing.T) {
	pvfPath := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE to verify real material placement")
	}
	archive, err := platformpvf.LoadArchive(platformpvf.Options{Path: pvfPath})
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := newPVFDungeonDropCatalog(archive)
	if err != nil {
		t.Fatal(err)
	}
	definition, err := catalog.ResolveItem(3340)
	if err != nil {
		t.Fatal(err)
	}
	if definition.Kind != dungeonDropItemStackable || definition.StackableType != "[material]" ||
		definition.SlotStart != 121 || definition.SlotEnd != 176 || dungeonDropStackablePrefersItemQuickSlots(definition.StackableType) {
		t.Fatalf("real material definition=%+v", definition)
	}
	inventory := dnfrepo.InventoryRecord{Slots: map[string]dnfrepo.ItemStack{
		"0:3": {ItemID: int64(definition.ItemID), Count: 4},
	}}
	slot, err := addCurrentDungeonPickupToInventory(&inventory, definition, 2)
	if err != nil {
		t.Fatal(err)
	}
	if slot != 121 || inventory.Slots["0:3"].Count != 4 || inventory.Slots["0:121"].Count != 2 {
		t.Fatalf("real material slot=%d inventory=%+v", slot, inventory.Slots)
	}
	mailbox, err := mailboxItemResolutionFromPVF(definition, false)
	if err != nil || mailbox.PreferQuickSlots || mailbox.SlotStart != 121 || mailbox.SlotEnd != 176 {
		t.Fatalf("real material mailbox=%+v err=%v", mailbox, err)
	}
}
