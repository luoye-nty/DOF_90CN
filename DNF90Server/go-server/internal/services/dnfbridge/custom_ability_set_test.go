package dnfbridge

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"os"
	"reflect"
	"testing"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	"longheng.io/server/internal/modules/dnf/repository/memory"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

func TestDecodeCurrentCustomAbilitySetRequest(t *testing.T) {
	body := []byte{1, 107, 3, 11, 0, 12, 0, 9, 0}
	request, err := decodeCurrentCustomAbilitySetRequest(body)
	if err != nil || !bytes.Equal(request.options, []byte{107}) || !reflect.DeepEqual(request.slots, []int16{11, 12, 9}) {
		t.Fatalf("request=%+v err=%v", request, err)
	}
	for _, invalid := range [][]byte{
		nil,
		{0, 3, 11, 0, 12, 0, 9, 0},
		{1, 107, 2, 11, 0, 12, 0},
		{1, 107, 3, 11, 0, 12, 0},
		{1, 107, 3, 11, 0, 12, 0, 9, 0, 0},
		{2, 107, 107, 3, 11, 0, 12, 0, 9, 0},
		{1, 107, 3, 11, 0, 11, 0, 9, 0},
		{1, 107, 3, 11, 0, 12, 0, 0, 0x80},
	} {
		if _, err := decodeCurrentCustomAbilitySetRequest(invalid); !errors.Is(err, errCurrentCustomAbilitySetInvalid) {
			t.Fatalf("invalid body=%x err=%v", invalid, err)
		}
	}
}

func currentCustomAbilitySetUnitCatalog() *currentCustomAbilitySetCatalog {
	return &currentCustomAbilitySetCatalog{
		partSetIndex: 1,
		minParts:     3,
		maxParts:     6,
		maxOptions:   4,
		maxPoints: map[byte]map[byte]int{
			0: {3: 105, 4: 175, 5: 280, 6: 420},
			1: {3: 120, 4: 200, 5: 320, 6: 480},
			2: {3: 135, 4: 225, 5: 360, 6: 540},
			3: {3: 150, 4: 250, 5: 400, 6: 600},
		},
		materialID:   10161537,
		materialRate: 2.20,
		options: map[byte]currentCustomAbilitySetOptionRule{
			1:   {points: 50, group: 1, minParts: 4},
			2:   {points: 60, group: 1, minParts: 4},
			34:  {points: 50, group: 4, minParts: 3},
			56:  {points: 104, group: 3, minParts: 3},
			100: {points: 50, group: 2, minParts: 3},
			122: {points: 10, group: 5, minParts: 3},
			152: {points: 10, group: 6, minParts: 3},
			252: {points: 150, group: 7, minParts: 6},
		},
	}
}

func currentCustomAbilityIndividualUnitCatalog() *currentCustomAbilityCatalog {
	return &currentCustomAbilityCatalog{optionIDs: map[byte]struct{}{4: {}, 12: {}}}
}

func TestCurrentCustomAbilitySetEvaluateUsesAverageGradeAndRecursivePoints(t *testing.T) {
	catalog := currentCustomAbilitySetUnitCatalog()
	grade, spent, material, err := catalog.evaluate([]byte{122, 152}, []byte{0, 1, 2})
	if err != nil || grade != 1 {
		t.Fatalf("grade=%d spent=%d material=%d err=%v", grade, spent, material, err)
	}
	first := 10 * (120 + 1000) / (10 + 1000)
	second := 10 * (120 - first + 1000) / (10 + 1000)
	wantSpent := first + second
	if spent != wantSpent || material != int64(float64(wantSpent)*2.20) {
		t.Fatalf("spent=%d material=%d want_spent=%d want_material=%d", spent, material, wantSpent, int64(float64(wantSpent)*2.20))
	}
	for _, test := range []struct {
		options []byte
		grades  []byte
	}{
		{options: []byte{1}, grades: []byte{0, 0, 0}},
		{options: []byte{252}, grades: []byte{3, 3, 3, 3, 3}},
		{options: []byte{122, 122}, grades: []byte{0, 0, 0}},
		{options: []byte{1, 2}, grades: []byte{0, 0, 0, 0}},
	} {
		if _, _, _, err := catalog.evaluate(test.options, test.grades); !errors.Is(err, errCurrentCustomAbilitySetInvalid) {
			t.Fatalf("options=%v grades=%v err=%v", test.options, test.grades, err)
		}
	}
}

func TestValidateCurrentCustomAbilitySetProfilesRejectsDuplicatePartAndMixedFullArmor(t *testing.T) {
	base := []currentCustomAbilityItemProfile{
		{applicable: true, optionCount: 2, equipmentSlot: 12},
		{applicable: true, optionCount: 2, equipmentSlot: 14, hasSubType: true, subType: 1},
		{applicable: true, optionCount: 2, equipmentSlot: 15, hasSubType: true, subType: 1},
	}
	if err := validateCurrentCustomAbilitySetProfiles(base); err != nil {
		t.Fatal(err)
	}
	duplicate := append([]currentCustomAbilityItemProfile(nil), base...)
	duplicate[2].equipmentSlot = 14
	if err := validateCurrentCustomAbilitySetProfiles(duplicate); !errors.Is(err, errCurrentCustomAbilitySetInvalid) {
		t.Fatalf("duplicate part err=%v", err)
	}
	mixedArmor := []currentCustomAbilityItemProfile{
		{applicable: true, optionCount: 2, equipmentSlot: 14, hasSubType: true, subType: 1},
		{applicable: true, optionCount: 2, equipmentSlot: 15, hasSubType: true, subType: 1},
		{applicable: true, optionCount: 2, equipmentSlot: 16, hasSubType: true, subType: 1},
		{applicable: true, optionCount: 2, equipmentSlot: 17, hasSubType: true, subType: 2},
		{applicable: true, optionCount: 2, equipmentSlot: 18, hasSubType: true, subType: 1},
	}
	if err := validateCurrentCustomAbilitySetProfiles(mixedArmor); !errors.Is(err, errCurrentCustomAbilitySetInvalid) {
		t.Fatalf("mixed armor err=%v", err)
	}
}

func TestCommitCurrentCustomAbilitySetConsumesAcrossStacksClearsOldMembersAndPersistsSnapshot(t *testing.T) {
	repositories := memory.NewMemoryGroup()
	catalog := currentCustomAbilitySetUnitCatalog()
	makeRaw := func(grade, setRecord, category byte) []byte {
		raw := make([]byte, currentItemListEntryWireSize)
		raw[currentCustomAbilityOption0Offset] = 4
		raw[currentCustomAbilityOption1Offset] = 12
		raw[currentCustomAbilityRawBlockStart+2] = category
		raw[currentCustomAbilityGradeOffset] = grade
		raw[currentCustomAbilitySetRecordOffset] = setRecord
		return raw
	}
	if err := repositories.Inventory.Save(context.Background(), dnfrepo.InventoryRecord{CharacterID: "4", Slots: map[string]dnfrepo.ItemStack{
		"0:9":  {ItemID: 1001, Count: 1, RawEntry: makeRaw(0, 0, 0x31)},
		"0:11": {ItemID: 1002, Count: 1, RawEntry: makeRaw(1, 0, 0x32)},
		"0:12": {ItemID: 1003, Count: 1, RawEntry: makeRaw(2, 0, 0x33)},
		"0:20": {ItemID: 1004, Count: 1, RawEntry: makeRaw(0, 1, 0x34), Extra: map[string]string{currentCustomAbilitySetOptionsExtra: "34"}},
		"0:50": {ItemID: 10161537, Count: 10},
		"0:51": {ItemID: 10161537, Count: 20},
	}}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Equipment.Save(context.Background(), dnfrepo.EquipmentRecord{CharacterID: "4", Entries: map[string]dnfrepo.EquipmentEntry{
		"17": {SlotIndex: 17, ItemID: 1005, RawEntry: makeRaw(0, 1, 0x35), Extra: map[string]string{currentCustomAbilitySetOptionsExtra: "34"}},
	}}); err != nil {
		t.Fatal(err)
	}
	profiles := map[uint32]currentCustomAbilityItemProfile{
		1001: {applicable: true, optionCount: 2, partSetIndex: 1, equipmentSlot: 12},
		1002: {applicable: true, optionCount: 2, partSetIndex: 1, equipmentSlot: 14, hasSubType: true, subType: 1},
		1003: {applicable: true, optionCount: 2, partSetIndex: 1, equipmentSlot: 15, hasSubType: true, subType: 1},
		1004: {applicable: true, optionCount: 2, partSetIndex: 1, equipmentSlot: 16, hasSubType: true, subType: 1},
		1005: {applicable: true, optionCount: 2, partSetIndex: 1, equipmentSlot: 17, hasSubType: true, subType: 1},
	}
	resolve := func(itemID uint32) (currentCustomAbilityItemProfile, error) { return profiles[itemID], nil }
	request := currentCustomAbilitySetRequest{options: []byte{122}, slots: []int16{11, 12, 9}}
	result, err := commitCurrentCustomAbilitySet(context.Background(), repositories, "4", request, currentCustomAbilityIndividualUnitCatalog(), catalog, resolve)
	if err != nil {
		t.Fatal(err)
	}
	if result.snapshot.grade != 1 || result.snapshot.requiredParts != 3 || result.snapshot.options[0] != 122 || result.materialCost != 24 || result.materialRemaining != 6 || !result.equipmentChanged {
		t.Fatalf("result=%+v", result)
	}
	wantRefreshSlots := []int16{9, 11, 12, 20, 50, 51}
	gotRefreshSlots := make([]int16, len(result.refreshes))
	for index, refresh := range result.refreshes {
		gotRefreshSlots[index] = refresh.SlotIndex
	}
	if !reflect.DeepEqual(gotRefreshSlots, wantRefreshSlots) {
		t.Fatalf("refresh slots=%v want=%v", gotRefreshSlots, wantRefreshSlots)
	}
	inventory, _, _ := repositories.Inventory.Load(context.Background(), "4")
	for slot, category := range map[string]byte{"0:9": 0x31, "0:11": 0x32, "0:12": 0x33} {
		stack := inventory.Slots[slot]
		if stack.RawEntry[currentCustomAbilitySetRecordOffset] != 1 || stack.RawEntry[currentCustomAbilityRawBlockStart+2] != category || stack.Extra[currentCustomAbilitySetOptionsExtra] != "122" || stack.Extra[currentCustomAbilitySetRequiredPartsExtra] != "3" || stack.Extra[currentCustomAbilitySetGradeExtra] != "1" {
			t.Fatalf("selected slot=%s raw_tail=%x extra=%v", slot, stack.RawEntry[currentCustomAbilityRawBlockStart:currentCustomAbilityRawBlockEnd], stack.Extra)
		}
	}
	if inventory.Slots["0:20"].RawEntry[currentCustomAbilitySetRecordOffset] != 0 || inventory.Slots["0:20"].Extra[currentCustomAbilitySetOptionsExtra] != "" || inventory.Slots["0:51"].Count != 6 {
		t.Fatalf("inventory after commit=%+v", inventory.Slots)
	}
	equipment, _, _ := repositories.Equipment.Load(context.Background(), "4")
	if equipment.Entries["17"].RawEntry[currentCustomAbilitySetRecordOffset] != 0 || equipment.Entries["17"].Extra[currentCustomAbilitySetOptionsExtra] != "" {
		t.Fatalf("old equipped member=%+v", equipment.Entries["17"])
	}
	snapshot, found, err := loadCurrentCustomAbilitySetSnapshot(context.Background(), repositories, "4")
	if err != nil || !found || snapshot != result.snapshot {
		t.Fatalf("restored snapshot=%+v found=%t err=%v", snapshot, found, err)
	}
}

func TestCommitCurrentCustomAbilitySetMaterialFailureRollsBack(t *testing.T) {
	repositories := memory.NewMemoryGroup()
	makeRaw := func(grade byte) []byte {
		raw := make([]byte, currentItemListEntryWireSize)
		raw[currentCustomAbilityOption0Offset] = 4
		raw[currentCustomAbilityOption1Offset] = 12
		raw[currentCustomAbilityGradeOffset] = grade
		return raw
	}
	seed := dnfrepo.InventoryRecord{CharacterID: "4", Slots: map[string]dnfrepo.ItemStack{
		"0:9":  {ItemID: 1001, Count: 1, RawEntry: makeRaw(0)},
		"0:11": {ItemID: 1002, Count: 1, RawEntry: makeRaw(1)},
		"0:12": {ItemID: 1003, Count: 1, RawEntry: makeRaw(2)},
		"0:50": {ItemID: 10161537, Count: 21},
	}}
	if err := repositories.Inventory.Save(context.Background(), seed); err != nil {
		t.Fatal(err)
	}
	profiles := map[uint32]currentCustomAbilityItemProfile{
		1001: {applicable: true, optionCount: 2, partSetIndex: 1, equipmentSlot: 12},
		1002: {applicable: true, optionCount: 2, partSetIndex: 1, equipmentSlot: 14},
		1003: {applicable: true, optionCount: 2, partSetIndex: 1, equipmentSlot: 15},
	}
	_, err := commitCurrentCustomAbilitySet(context.Background(), repositories, "4", currentCustomAbilitySetRequest{options: []byte{122}, slots: []int16{9, 11, 12}}, currentCustomAbilityIndividualUnitCatalog(), currentCustomAbilitySetUnitCatalog(), func(itemID uint32) (currentCustomAbilityItemProfile, error) { return profiles[itemID], nil })
	if !errors.Is(err, errCurrentCustomAbilitySetMaterial) {
		t.Fatalf("commit error=%v", err)
	}
	stored, _, _ := repositories.Inventory.Load(context.Background(), "4")
	if stored.Slots["0:50"].Count != 21 || stored.Slots["0:9"].RawEntry[currentCustomAbilitySetRecordOffset] != 0 {
		t.Fatalf("failed commit mutated inventory=%+v", stored.Slots)
	}
	if got := buildCurrentCustomAbilitySetFailureBody(err); !bytes.Equal(got, []byte{0, 8}) {
		t.Fatalf("material failure body=%x", got)
	}
}

func TestBuildCurrentCustomAbilitySetNotificationBody(t *testing.T) {
	snapshot := currentCustomAbilitySetSnapshot{recordID: 1, requiredParts: 3, options: [5]byte{107}, grade: 2, partSetIndex: 1}
	body := buildCurrentCustomAbilitySetNotificationBody(0x12345678, snapshot)
	want := []byte{0x78, 0x56, 0x34, 0x12, 1, 3, 107, 0, 0, 0, 0, 2}
	if !bytes.Equal(body, want) {
		t.Fatalf("notification=%x want=%x", body, want)
	}
	if binary.LittleEndian.Uint32(body[:4]) != 0x12345678 {
		t.Fatalf("actor id=%x", body[:4])
	}
}

func TestCurrentCustomAbilitySetRealPVF(t *testing.T) {
	pvfPath := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE to verify current part-set PVF")
	}
	archive, err := platformpvf.Open(pvfPath)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := parseCurrentCustomAbilitySetCatalog(archive)
	if err != nil {
		t.Fatal(err)
	}
	if catalog.partSetIndex != 1 || catalog.minParts != 3 || catalog.maxParts != 6 || catalog.maxOptions != 4 || catalog.materialID != 10161537 || catalog.materialRate != 2.20 || len(catalog.options) != 252 {
		t.Fatalf("catalog index=%d parts=%d..%d options=%d material=%d rate=%v rules=%d", catalog.partSetIndex, catalog.minParts, catalog.maxParts, catalog.maxOptions, catalog.materialID, catalog.materialRate, len(catalog.options))
	}
	if catalog.options[1].group != 1 || catalog.options[1].minParts != 4 || catalog.options[107].group != 2 || catalog.options[252].group != 7 || catalog.options[252].minParts != 6 {
		t.Fatalf("rules option1=%+v option107=%+v option252=%+v", catalog.options[1], catalog.options[107], catalog.options[252])
	}
	if catalog.maxPoints[0][3] != 105 || catalog.maxPoints[3][6] != 600 {
		t.Fatalf("max points=%v", catalog.maxPoints)
	}
}
