package dnfbridge

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"longheng.io/server/internal/modules/dnf/adventuregroup"
	"longheng.io/server/internal/modules/dnf/alignedcmd"
	dnfenum "longheng.io/server/internal/modules/dnf/dnfenum"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	"longheng.io/server/internal/modules/dnf/repository/memory"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

type currentCustomAbilityTestSource map[string]string

func (source currentCustomAbilityTestSource) ReadText(path string) (string, error) {
	text, ok := source[path]
	if !ok {
		return "", errors.New("missing test PVF path: " + path)
	}
	return text, nil
}

func currentCustomAbilityOptionTestText() string {
	return "[parameter]\n[custom grade] `D`\n[/parameter]\n" +
		"[parameter]\n[custom grade] `C`\n[/parameter]\n" +
		"[parameter]\n[custom grade] `B`\n[/parameter]\n" +
		"[parameter]\n[custom grade] `A`\n[/parameter]\n"
}

func currentCustomAbilityModifyTestText(optionID int, itemID int) string {
	return "[info]\n[index]\n" + strconv.Itoa(optionID) + "\n" +
		"[material]\n[grade] `D`\n[list]\n" + strconv.Itoa(itemID) + " 28\n[/list]\n[/material]\n" +
		"[material]\n[grade] `C`\n[list]\n" + strconv.Itoa(itemID) + " 40\n[/list]\n[/material]\n" +
		"[material]\n[grade] `B`\n[list]\n" + strconv.Itoa(itemID) + " 60\n[/list]\n[/material]\n" +
		"[material]\n[grade] `A`\n[list]\n" + strconv.Itoa(itemID) + " 96\n[/list]\n[/material]\n" +
		"[/info]\n"
}

func TestParseCurrentCustomAbilityCatalogIncludesModifyAndUpgradeRules(t *testing.T) {
	source := currentCustomAbilityTestSource{
		currentCustomAbilityListPath:        "1 `Options/one.etc`\n2 `Options/two.etc`\n",
		"etc/customability/Options/one.etc": currentCustomAbilityOptionTestText(),
		"etc/customability/Options/two.etc": currentCustomAbilityOptionTestText(),
		currentCustomAbilityInfoPath: "[custom ability type] `warriors dungeon`\n" +
			"[group]\n[indexes]\n1 2\n[/indexes]\n[/group]\n" +
			"[grade probability]\n`D` 6000 `C` 3000 `B` 800 `A` 200\n[/grade probability]\n" +
			currentCustomAbilityModifyTestText(1, 10161533) +
			currentCustomAbilityModifyTestText(2, 10161534) +
			"[upgrade max] 4\n" +
			"[upgrade info]\n[grade] `D`\n[assign material]\n10161536 11\n[/assign material]\n[/upgrade info]\n" +
			"[upgrade info]\n[grade] `C`\n[assign material]\n10161536 11\n[/assign material]\n[/upgrade info]\n" +
			"[upgrade info]\n[grade] `B`\n[assign material]\n10161536 18\n[/assign material]\n[/upgrade info]\n" +
			"[upgrade info]\n[grade] `A`\n[assign material]\n10161536 34\n[/assign material]\n[/upgrade info]\n",
	}
	catalog, err := parseCurrentCustomAbilityCatalog(source)
	if err != nil {
		t.Fatal(err)
	}
	if catalog.upgradeMax != 4 || len(catalog.upgradeMaterial) != 4 {
		t.Fatalf("upgrade max=%d rows=%d", catalog.upgradeMax, len(catalog.upgradeMaterial))
	}
	if len(catalog.modifyMaterial) != 2 || catalog.modifyMaterial[1][3] != (currentCustomAbilityMaterialCost{itemID: 10161533, count: 96}) ||
		catalog.modifyMaterial[2][0] != (currentCustomAbilityMaterialCost{itemID: 10161534, count: 28}) {
		t.Fatalf("modify materials=%+v", catalog.modifyMaterial)
	}
	for grade, want := range map[byte]int64{0: 11, 1: 11, 2: 18, 3: 34} {
		cost := catalog.upgradeMaterial[grade]
		if cost.itemID != 10161536 || cost.count != want {
			t.Fatalf("grade=%d cost=%+v", grade, cost)
		}
	}

	broken := currentCustomAbilityTestSource{}
	for path, text := range source {
		broken[path] = text
	}
	broken["etc/customability/Options/two.etc"] = "[parameter]\n[custom grade] `D`\n[/parameter]\n"
	if _, err := parseCurrentCustomAbilityCatalog(broken); err == nil {
		t.Fatal("option missing C/B/A grades was accepted")
	}
}

func TestCommitCurrentCustomAbilityUpgradeConsumesAcrossStacksAndPersistsGrade(t *testing.T) {
	repositories := memory.NewMemoryGroup()
	catalog := &currentCustomAbilityCatalog{
		optionIDs:       map[byte]struct{}{4: {}, 12: {}},
		upgradeMax:      4,
		upgradeMaterial: map[byte]currentCustomAbilityMaterialCost{1: {itemID: 10161536, count: 11}},
	}
	raw := make([]byte, currentItemListEntryWireSize)
	binary.LittleEndian.PutUint16(raw[0:2], 9)
	binary.LittleEndian.PutUint32(raw[2:6], 100130573)
	raw[currentCustomAbilityOption0Offset] = 12
	raw[currentCustomAbilityOption1Offset] = 4
	raw[currentCustomAbilityGradeOffset] = 0
	if err := repositories.Inventory.Save(context.Background(), dnfrepo.InventoryRecord{
		CharacterID: "4",
		Slots: map[string]dnfrepo.ItemStack{
			"0:9":   {ItemID: 100130573, Count: 1, RawEntry: raw},
			"0:151": {ItemID: 10161536, Count: 6},
			"0:152": {ItemID: 10161536, Count: 10},
		},
	}); err != nil {
		t.Fatal(err)
	}

	result, err := commitCurrentCustomAbilityUpgrade(context.Background(), repositories, "4", 9, 100130573, catalog)
	if err != nil {
		t.Fatal(err)
	}
	if result.oldGrade != 0 || result.newGrade != 1 || result.materialRemaining != 5 {
		t.Fatalf("upgrade result=%+v", result)
	}
	if len(result.refreshes) != 3 || result.refreshes[0].SlotIndex != 9 || result.refreshes[1].SlotIndex != 151 || result.refreshes[2].SlotIndex != 152 {
		t.Fatalf("refreshes=%+v", result.refreshes)
	}
	stored, _, err := repositories.Inventory.Load(context.Background(), "4")
	if err != nil {
		t.Fatal(err)
	}
	if got := stored.Slots["0:9"].RawEntry[currentCustomAbilityGradeOffset]; got != 1 {
		t.Fatalf("persisted grade=%d", got)
	}
	if _, exists := stored.Slots["0:151"]; exists {
		t.Fatal("first material stack was not deleted")
	}
	if got := stored.Slots["0:152"].Count; got != 5 {
		t.Fatalf("remaining material=%d", got)
	}
}

func TestCommitCurrentCustomAbilityUpgradeMaterialFailureRollsBack(t *testing.T) {
	repositories := memory.NewMemoryGroup()
	catalog := &currentCustomAbilityCatalog{
		optionIDs:       map[byte]struct{}{4: {}, 12: {}},
		upgradeMax:      4,
		upgradeMaterial: map[byte]currentCustomAbilityMaterialCost{1: {itemID: 10161536, count: 11}},
	}
	raw := make([]byte, currentItemListEntryWireSize)
	raw[currentCustomAbilityOption0Offset] = 12
	raw[currentCustomAbilityOption1Offset] = 4
	seed := dnfrepo.InventoryRecord{CharacterID: "4", Slots: map[string]dnfrepo.ItemStack{
		"0:9":   {ItemID: 100130573, Count: 1, RawEntry: raw},
		"0:151": {ItemID: 10161536, Count: 10},
	}}
	if err := repositories.Inventory.Save(context.Background(), seed); err != nil {
		t.Fatal(err)
	}
	_, err := commitCurrentCustomAbilityUpgrade(context.Background(), repositories, "4", 9, 100130573, catalog)
	if !errors.Is(err, errCurrentCustomAbilityUpgradeMaterial) {
		t.Fatalf("upgrade error=%v", err)
	}
	stored, _, _ := repositories.Inventory.Load(context.Background(), "4")
	if stored.Slots["0:151"].Count != 10 || stored.Slots["0:9"].RawEntry[currentCustomAbilityGradeOffset] != 0 {
		t.Fatalf("failed upgrade mutated inventory=%+v", stored.Slots)
	}
}

func TestBuildCurrentCustomAbilityUpgradeSuccessBodyIncludesExpeditionState(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	state := adventuregroup.RuntimeState{Expeditions: map[string]adventuregroup.Expedition{
		"2": {
			Area:       2,
			State:      1,
			StartedAt:  now.Unix() - 10,
			EndsAt:     now.Unix() + 10,
			Attributes: []byte{4, 7},
		},
	}}
	wantState := buildCurrentAdventureExpeditionStateBody(state, now)
	body := buildCurrentCustomAbilityUpgradeSuccessBody(state, now)
	if len(body) != len(wantState)+1 || body[0] != 1 || !bytes.Equal(body[1:], wantState) {
		t.Fatalf("custom ability upgrade success body=%x expedition=%x", body, wantState)
	}
}

func TestInitializeCurrentCustomAbilityInventoryPersistsPVFDrawOnce(t *testing.T) {
	catalog := &currentCustomAbilityCatalog{
		optionIDs: map[byte]struct{}{1: {}, 2: {}, 11: {}, 21: {}},
		groups:    [][]byte{{1, 2}, {11}, {21}},
		gradeWeight: []currentCustomAbilityGradeWeight{
			{grade: 3, weight: 200},
			{grade: 2, weight: 800},
			{grade: 1, weight: 3000},
			{grade: 0, weight: 6000},
		},
		totalWeight: 10000,
	}
	raw := make([]byte, currentItemListEntryWireSize)
	binary.LittleEndian.PutUint16(raw[0:2], 9)
	binary.LittleEndian.PutUint32(raw[2:6], 100130573)
	binary.LittleEndian.PutUint32(raw[6:10], 123456)
	record := dnfrepo.InventoryRecord{
		CharacterID: "4",
		Slots: map[string]dnfrepo.ItemStack{
			"0:9":  {ItemID: 100130573, Count: 1, RawEntry: raw},
			"0:10": {ItemID: 3340, Count: 100},
		},
	}
	resolve := func(itemID uint32) (currentCustomAbilityItemProfile, error) {
		return currentCustomAbilityItemProfile{applicable: itemID == 100130573, optionCount: 2, partSetIndex: 1}, nil
	}
	draws := []int{0, 1, 2, 0, 1000}
	drawCount := 0
	draw := func(limit int) (int, error) {
		value := draws[drawCount]
		drawCount++
		if value >= limit {
			t.Fatalf("draw value %d exceeds limit %d", value, limit)
		}
		return value, nil
	}

	updated, changed, err := initializeCurrentCustomAbilityInventory(record, catalog, resolve, draw)
	if err != nil || changed != 1 {
		t.Fatalf("initialize changed=%d err=%v", changed, err)
	}
	stack := updated.Slots["0:9"]
	if got := stack.RawEntry[currentCustomAbilityOption0Offset]; got != 2 {
		t.Fatalf("option0=%d want=2", got)
	}
	if got := stack.RawEntry[currentCustomAbilityOption1Offset]; got != 21 {
		t.Fatalf("option1=%d want=21", got)
	}
	if got := stack.RawEntry[currentCustomAbilitySetRecordOffset]; got != 0 {
		t.Fatalf("set record=%d want=0", got)
	}
	if got := stack.RawEntry[currentCustomAbilityGradeOffset]; got != 1 {
		t.Fatalf("grade=%d want=1", got)
	}
	if stack.Extra["custom_ability_option_0"] != "2" || stack.Extra["custom_ability_option_1"] != "21" || stack.Extra["custom_ability_grade"] != "1" {
		t.Fatalf("custom ability extra=%+v", stack.Extra)
	}
	if bytes.Equal(record.Slots["0:9"].RawEntry, stack.RawEntry) {
		t.Fatal("initializer mutated the input repository snapshot")
	}

	drawCountBeforeReplay := drawCount
	replayed, replayChanged, err := initializeCurrentCustomAbilityInventory(updated, catalog, resolve, draw)
	if err != nil || replayChanged != 0 || drawCount != drawCountBeforeReplay {
		t.Fatalf("replay changed=%d draw_count=%d->%d err=%v", replayChanged, drawCountBeforeReplay, drawCount, err)
	}
	if !bytes.Equal(replayed.Slots["0:9"].RawEntry, stack.RawEntry) {
		t.Fatal("replay changed persisted custom ability state")
	}
}

func TestInitializeCurrentCustomAbilityInventorySlotsRepairsOnlyIncrementalGrantRows(t *testing.T) {
	catalog := &currentCustomAbilityCatalog{
		optionIDs: map[byte]struct{}{1: {}, 2: {}, 11: {}, 21: {}},
		groups:    [][]byte{{1, 2}, {11}, {21}},
		gradeWeight: []currentCustomAbilityGradeWeight{
			{grade: 0, weight: 1},
		},
		totalWeight: 1,
	}
	makeRaw := func(slot uint16, itemID uint32) []byte {
		raw := make([]byte, currentItemListEntryWireSize)
		binary.LittleEndian.PutUint16(raw[0:2], slot)
		binary.LittleEndian.PutUint32(raw[2:6], itemID)
		return raw
	}
	record := dnfrepo.InventoryRecord{
		CharacterID: "4",
		Slots: map[string]dnfrepo.ItemStack{
			"0:9":  {ItemID: 100130573, Count: 1, RawEntry: makeRaw(9, 100130573)},
			"0:10": {ItemID: 100130574, Count: 1, RawEntry: makeRaw(10, 100130574)},
			"0:11": {ItemID: 3340, Count: 100, RawEntry: makeRaw(11, 3340)},
		},
	}
	resolve := func(itemID uint32) (currentCustomAbilityItemProfile, error) {
		return currentCustomAbilityItemProfile{
			applicable:  itemID == 100130573 || itemID == 100130574,
			optionCount: 2,
		}, nil
	}
	draws := []int{0, 0, 2, 0, 0}
	drawCount := 0
	draw := func(limit int) (int, error) {
		value := draws[drawCount]
		drawCount++
		if value >= limit {
			t.Fatalf("draw value %d exceeds limit %d", value, limit)
		}
		return value, nil
	}

	updated, changed, err := initializeCurrentCustomAbilityInventorySlots(
		record,
		map[int16]struct{}{10: {}},
		catalog,
		resolve,
		draw,
	)
	if err != nil || changed != 1 {
		t.Fatalf("incremental initialize changed=%d err=%v", changed, err)
	}
	if got := updated.Slots["0:10"].RawEntry[currentCustomAbilityRawBlockStart:currentCustomAbilityRawBlockEnd]; !bytes.Equal(got, []byte{1, 21, 0, 0, 0}) {
		t.Fatalf("target custom tail=%x", got)
	}
	if got := updated.Slots["0:9"].RawEntry[currentCustomAbilityRawBlockStart:currentCustomAbilityRawBlockEnd]; !bytes.Equal(got, make([]byte, currentEpicProductionAbilityTailSize)) {
		t.Fatalf("unrefreshed custom slot was redrawn: %x", got)
	}
	if got := updated.Slots["0:11"].RawEntry[currentCustomAbilityRawBlockStart:currentCustomAbilityRawBlockEnd]; !bytes.Equal(got, make([]byte, currentEpicProductionAbilityTailSize)) {
		t.Fatalf("ordinary material row was changed: %x", got)
	}
	if bytes.Equal(record.Slots["0:10"].RawEntry, updated.Slots["0:10"].RawEntry) {
		t.Fatal("incremental initializer mutated or reused the input snapshot")
	}

	drawCountBeforeReplay := drawCount
	replayed, replayChanged, err := initializeCurrentCustomAbilityInventorySlots(
		updated,
		map[int16]struct{}{10: {}},
		catalog,
		resolve,
		draw,
	)
	if err != nil || replayChanged != 0 || drawCount != drawCountBeforeReplay {
		t.Fatalf("incremental replay changed=%d draw_count=%d->%d err=%v", replayChanged, drawCountBeforeReplay, drawCount, err)
	}
	if !bytes.Equal(replayed.Slots["0:10"].RawEntry, updated.Slots["0:10"].RawEntry) {
		t.Fatal("incremental replay changed the persisted custom ability tail")
	}
}

func TestInitializeCurrentCustomAbilityInventoryMigratesLegacyOffsetsWithoutRedraw(t *testing.T) {
	catalog := &currentCustomAbilityCatalog{
		optionIDs: map[byte]struct{}{4: {}, 12: {}},
		groups:    [][]byte{{4, 12}},
		gradeWeight: []currentCustomAbilityGradeWeight{
			{grade: 0, weight: 1},
		},
		totalWeight: 1,
	}
	raw := make([]byte, currentItemListEntryWireSize)
	raw[currentCustomAbilityLegacyOption0Offset] = 12
	raw[currentCustomAbilityLegacyOption1Offset] = 4
	raw[currentCustomAbilityLegacyGradeOffset] = 1
	record := dnfrepo.InventoryRecord{CharacterID: "4", Slots: map[string]dnfrepo.ItemStack{
		"0:9": {
			ItemID:   100130573,
			Count:    1,
			RawEntry: raw,
			Extra: map[string]string{
				"custom_ability_option_0":   "12",
				"custom_ability_option_1":   "4",
				"custom_ability_set_record": "0",
				"custom_ability_grade":      "1",
			},
		},
	}}
	resolve := func(itemID uint32) (currentCustomAbilityItemProfile, error) {
		return currentCustomAbilityItemProfile{applicable: itemID == 100130573, optionCount: 2}, nil
	}
	drawCalled := false
	updated, changed, err := initializeCurrentCustomAbilityInventory(record, catalog, resolve, func(int) (int, error) {
		drawCalled = true
		return 0, nil
	})
	if err != nil || changed != 1 || drawCalled {
		t.Fatalf("legacy migration changed=%d draw=%t err=%v", changed, drawCalled, err)
	}
	stack := updated.Slots["0:9"]
	if stack.RawEntry[currentCustomAbilityOption0Offset] != 12 ||
		stack.RawEntry[currentCustomAbilityOption1Offset] != 4 ||
		stack.RawEntry[currentCustomAbilityGradeOffset] != 1 {
		t.Fatalf("migrated tail=%x", stack.RawEntry[currentCustomAbilityRawBlockStart:currentCustomAbilityRawBlockEnd])
	}
	if stack.Extra["tail_data_72"] != "0c04000001" {
		t.Fatalf("migrated extra=%+v", stack.Extra)
	}
}

func TestCurrentCustomAbilityInventoryRefreshEntriesOnlyIncludesPVFCustomEquipment(t *testing.T) {
	catalog := &currentCustomAbilityCatalog{optionIDs: map[byte]struct{}{4: {}, 12: {}}}
	customRaw := make([]byte, currentItemListEntryWireSize)
	customRaw[currentCustomAbilityOption0Offset] = 12
	customRaw[currentCustomAbilityOption1Offset] = 4
	customRaw[currentCustomAbilityGradeOffset] = 1
	ordinaryRaw := append([]byte(nil), customRaw...)
	record := dnfrepo.InventoryRecord{
		CharacterID: "77",
		Slots: map[string]dnfrepo.ItemStack{
			"0:9":  {ItemID: 100130573, Count: 1, RawEntry: customRaw},
			"0:10": {ItemID: 100000001, Count: 1, RawEntry: ordinaryRaw},
			"0:11": {ItemID: 100130573, Count: 2, RawEntry: customRaw},
		},
	}
	resolve := func(itemID uint32) (currentCustomAbilityItemProfile, error) {
		return currentCustomAbilityItemProfile{applicable: itemID == 100130573, optionCount: 2}, nil
	}

	entries, err := currentCustomAbilityInventoryRefreshEntries(record, catalog, resolve)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("custom ability refresh entry count=%d", len(entries))
	}
	if got := binary.LittleEndian.Uint16(entries[0].data[0:2]); got != 9 {
		t.Fatalf("custom ability refresh slot=%d want=9", got)
	}
	if got := binary.LittleEndian.Uint32(entries[0].data[2:6]); got != 100130573 {
		t.Fatalf("custom ability refresh item=%d want=100130573", got)
	}
	if got := entries[0].data[currentCustomAbilityRawBlockStart:currentCustomAbilityRawBlockEnd]; !bytes.Equal(got, customRaw[currentCustomAbilityRawBlockStart:currentCustomAbilityRawBlockEnd]) {
		t.Fatalf("custom ability refresh raw=%x want=%x", got, customRaw[currentCustomAbilityRawBlockStart:currentCustomAbilityRawBlockEnd])
	}
}

func TestCurrentMode1EquipmentRawBlocksFallBackToCurrentItemState(t *testing.T) {
	var entry currentItemListEntry
	copy(entry.data[currentEpicProductionAbilityTailOffset:currentEpicProductionAbilityTailOffset+currentEpicProductionAbilityTailSize], []byte{4, 20, 0, 0, 0})

	lateRawE := currentMode1EquipmentRawBlockOrCurrentItem(nil, entry, currentEpicProductionAbilityTailOffset, currentEpicProductionAbilityTailOffset+currentEpicProductionAbilityTailSize)
	if !bytes.Equal(lateRawE, []byte{4, 20, 0, 0, 0}) {
		t.Fatalf("late rawE=%x", lateRawE)
	}

	explicit := map[string]string{"mode1_raw_d_hex": "aabb"}
	if got := currentMode1EquipmentRawBlockOrCurrentItem(explicit, entry, currentCustomAbilityRawBlockStart, currentCustomAbilityRawBlockEnd, "mode1_raw_d_hex"); !bytes.Equal(got, []byte{0xaa, 0xbb}) {
		t.Fatalf("explicit mode1 rawD did not retain precedence: %x", got)
	}
}

func TestCurrentCustomAbilityRealPVF(t *testing.T) {
	pvfPath := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE to verify current customability PVF")
	}
	archive, err := platformpvf.Open(pvfPath)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := parseCurrentCustomAbilityCatalog(archive)
	if err != nil {
		t.Fatal(err)
	}
	if len(catalog.optionIDs) != 16 || len(catalog.groups) != 3 || catalog.totalWeight != 10000 {
		t.Fatalf("customability options=%d groups=%d grade_weight=%d", len(catalog.optionIDs), len(catalog.groups), catalog.totalWeight)
	}
	if catalog.upgradeMax != 4 || len(catalog.upgradeMaterial) != 4 {
		t.Fatalf("customability upgrade max=%d rows=%d", catalog.upgradeMax, len(catalog.upgradeMaterial))
	}
	if len(catalog.modifyMaterial) != 16 {
		t.Fatalf("customability modify options=%d", len(catalog.modifyMaterial))
	}
	for optionID, grades := range catalog.modifyMaterial {
		if len(grades) != 4 {
			t.Fatalf("customability modify option=%d grades=%d", optionID, len(grades))
		}
	}
	if got := catalog.modifyMaterial[1][3]; got != (currentCustomAbilityMaterialCost{itemID: 10161533, count: 96}) {
		t.Fatalf("customability option1 gradeA material=%+v", got)
	}
	if got := catalog.modifyMaterial[14][0]; got != (currentCustomAbilityMaterialCost{itemID: 10161534, count: 23}) {
		t.Fatalf("customability option14 gradeD material=%+v", got)
	}
	if got := catalog.modifyMaterial[25][2]; got != (currentCustomAbilityMaterialCost{itemID: 10161535, count: 60}) {
		t.Fatalf("customability option25 gradeB material=%+v", got)
	}
	if len(catalog.groups[0]) != 7 || len(catalog.groups[1]) != 4 || len(catalog.groups[2]) != 5 {
		t.Fatalf("customability groups=%v", catalog.groups)
	}
	service := &Service{initialEquipmentArchive: archive}
	profile, err := service.resolveCurrentCustomAbilityItemProfile(100130573)
	if err != nil {
		t.Fatal(err)
	}
	if !profile.applicable || profile.optionCount != 2 || profile.partSetIndex != 1 {
		t.Fatalf("real PVF customability profile=%+v", profile)
	}
}

func TestCurrentDungeonPickupPersistsCustomAbilityBeforeFirstRefreshRealPVF(t *testing.T) {
	pvfPath := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE to verify fighting-spirit pickup initialization")
	}
	archive, err := platformpvf.Open(pvfPath)
	if err != nil {
		t.Fatal(err)
	}
	items, err := newPVFDungeonDropCatalog(archive)
	if err != nil {
		t.Fatal(err)
	}
	definition, err := items.ResolveItem(100130573)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.EqualFold(strings.TrimSpace(definition.CustomAbilityType), "warriors dungeon") ||
		definition.CustomAbilityCount != 2 {
		t.Fatalf("real PVF fighting-spirit definition=%+v", definition)
	}

	repositories := memory.NewMemoryGroup()
	if err := repositories.Inventory.Save(context.Background(), dnfrepo.InventoryRecord{
		CharacterID: "4",
		Slots:       map[string]dnfrepo.ItemStack{},
	}); err != nil {
		t.Fatal(err)
	}
	service := &Service{initialEquipmentArchive: archive}
	slot, update, err := service.grantCurrentDungeonPickupItem(
		context.Background(),
		repositories.CharacterItems,
		"4",
		definition,
		1,
		time.Now().UTC(),
		nil,
		123456,
	)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := parseCurrentCustomAbilityCatalog(archive)
	if err != nil {
		t.Fatal(err)
	}
	wireState, valid := currentCustomAbilityStateFromRaw(update.data[:], catalog)
	if !valid {
		t.Fatalf("first pickup update has no custom ability state: %x", update.data[currentCustomAbilityRawBlockStart:currentCustomAbilityRawBlockEnd])
	}
	stored, found, err := repositories.Inventory.Load(context.Background(), "4")
	if err != nil || !found {
		t.Fatalf("load pickup inventory found=%t err=%v", found, err)
	}
	stack := stored.Slots[currentDungeonPickupMainSlotKey(int16(slot))]
	storedRow := currentItemListEntryFromStack(dnfrepo.MainInventoryListType, int16(slot), stack)
	storedState, valid := currentCustomAbilityStateFromRaw(storedRow.data[:], catalog)
	if !valid || storedState != wireState || stack.Extra["tail_data_72"] == "" {
		t.Fatalf("persisted pickup state=%+v valid=%t wire=%+v extra=%+v", storedState, valid, wireState, stack.Extra)
	}
}

func TestCurrentCustomAbilityIncrementalRefreshInitializesGrantRealPVF(t *testing.T) {
	pvfPath := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE to verify incremental customability grant refresh")
	}
	archive, err := platformpvf.Open(pvfPath)
	if err != nil {
		t.Fatal(err)
	}
	repositories := memory.NewMemoryGroup()
	raw := make([]byte, currentItemListEntryWireSize)
	binary.LittleEndian.PutUint16(raw[0:2], 9)
	binary.LittleEndian.PutUint32(raw[2:6], 100130573)
	binary.LittleEndian.PutUint32(raw[6:10], 123456)
	if err := repositories.Inventory.Save(context.Background(), dnfrepo.InventoryRecord{
		CharacterID: "4",
		Slots: map[string]dnfrepo.ItemStack{
			"0:9": {ItemID: 100130573, Count: 1, RawEntry: raw},
		},
	}); err != nil {
		t.Fatal(err)
	}
	connection := &bufferConn{}
	service := &Service{
		initialEquipmentArchive: archive,
		repositoryProvider:      func() (dnfrepo.Group, bool) { return repositories, true },
	}
	session := &gameSession{conn: connection, connID: "custom-ability-incremental-grant", selectedCharacterID: 4}
	if err := service.sendSelectedIncrementalItemSlotRefreshes(session, "dungeon_pickup", []alignedcmd.ItemSlotRefresh{{
		ListType:  dnfrepo.MainInventoryListType,
		SlotIndex: 9,
	}}); err != nil {
		t.Fatal(err)
	}

	packet, trailing := splitGameServerUpperPacket(t, connection.write.Bytes())
	if len(trailing) != 0 || packet.Header.Classification != 0 ||
		packet.Header.MsgID != uint16(dnfenum.CmdPacketWalkoutPartyMember) ||
		len(packet.Body) != 3+currentItemListEntryWireSize ||
		packet.Body[0] != dnfrepo.MainInventoryListType ||
		binary.LittleEndian.Uint16(packet.Body[1:3]) != 1 {
		t.Fatalf("incremental packet header=%+v body_len=%d body_prefix=%x trailing=%d", packet.Header, len(packet.Body), packet.Body[:minInt(len(packet.Body), 16)], len(trailing))
	}
	catalog, err := parseCurrentCustomAbilityCatalog(archive)
	if err != nil {
		t.Fatal(err)
	}
	wireRow := packet.Body[3:]
	wireState, valid := currentCustomAbilityStateFromRaw(wireRow, catalog)
	if !valid {
		t.Fatalf("incremental row did not contain a valid custom ability tail: %x", wireRow[currentCustomAbilityRawBlockStart:currentCustomAbilityRawBlockEnd])
	}
	stored, found, err := repositories.Inventory.Load(context.Background(), "4")
	if err != nil || !found {
		t.Fatalf("load persisted incremental state found=%t err=%v", found, err)
	}
	storedRow := currentItemListEntryFromStack(dnfrepo.MainInventoryListType, 9, stored.Slots["0:9"])
	storedState, valid := currentCustomAbilityStateFromRaw(storedRow.data[:], catalog)
	if !valid || storedState != wireState {
		t.Fatalf("persisted state=%+v valid=%t wire=%+v", storedState, valid, wireState)
	}
	if stored.Slots["0:9"].Extra["tail_data_72"] == "" {
		t.Fatalf("incremental state extra was not persisted: %+v", stored.Slots["0:9"].Extra)
	}
}
