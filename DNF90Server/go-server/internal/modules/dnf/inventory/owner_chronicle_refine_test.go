package inventory

import (
	"context"
	"encoding/binary"
	"errors"
	"strconv"
	"testing"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

const (
	testChronicleMaterialID = 10090760
	testChronicleTargetID   = 116010135
)

func testChronicleCommand(listType byte, targetSlot int16) Command {
	return NewChronicleRefineCommand(alignedcmd.Request{AccountID: "acc", SelectedCharacterID: 77}, ChronicleRefineRequest{
		MaterialSlotIndex:      67,
		MaterialItemTemplateID: testChronicleMaterialID,
		TargetListType:         listType,
		TargetSlotIndex:        targetSlot,
		TargetItemTemplateID:   testChronicleTargetID,
		OptionNo:               11,
	})
}

func testChronicleResolution(probabilities [2]byte) alignedcmd.ChronicleRefineResolution {
	return alignedcmd.ChronicleRefineResolution{
		MaterialPVFPath:         "stackable/10090001/10090760.stk",
		ImportedScriptPVFPath:   "stackable/dimensionforce_g.stk",
		AuraType:                2,
		CanonicalAuraOptionID:   1256,
		Probabilities:           probabilities,
		FailureFragmentItemID:   3311,
		FailureCubeCount:        20,
		FailureCubeItem:         testChronicleRewardItem(3037, 999999, 54, 104),
		FailureFragmentItem:     testChronicleRewardItem(3311, 1000, 54, 104),
		TargetPVFPath:           "equipment/character/gunblader/weapon/sblade/116010135.equ",
		TargetKind:              "equipment",
		TargetEquipmentType:     "weapon",
		TargetEquipmentTypeCode: 12,
		TargetDurability:        45,
		Matched:                 true,
	}
}

func testChronicleRewardItem(itemID, stackLimit int64, start, end int16) alignedcmd.MagicBoxRewardItem {
	return alignedcmd.MagicBoxRewardItem{
		ItemID:         itemID,
		Kind:           "stackable",
		TargetListType: listTypeMain,
		StackLimit:     stackLimit,
		SlotStart:      start,
		SlotEnd:        end,
		PVFPath:        "stackable/material/test.stk",
	}
}

func testChronicleTarget(level byte, existingOption bool) dnfrepo.ItemStack {
	raw := make([]byte, currentItemListEntrySize)
	raw[0x0A] = level
	binary.LittleEndian.PutUint16(raw[0x0B:0x0D], 45)
	stack := dnfrepo.ItemStack{
		ItemID:   testChronicleTargetID,
		Count:    1,
		RawEntry: raw,
		Extra: map[string]string{
			"item_kind":      "equipment",
			"durability":     "45",
			"max_durability": "45",
			"reinforce":      strconv.Itoa(int(level)),
		},
	}
	if existingOption {
		setChronicleOptions(&stack, []chronicleOption{{
			CanonicalAuraOptionID: 1254,
			CharacterJob:          15,
			FirstGrowType:         2,
			EquipmentType:         12,
			OptionNo:              3,
		}})
	}
	return stack
}

func testChronicleMaterial(count int64) dnfrepo.ItemStack {
	raw := make([]byte, currentItemListEntrySize)
	binary.LittleEndian.PutUint32(raw[0x06:0x0A], uint32(count))
	return dnfrepo.ItemStack{
		ItemID:   testChronicleMaterialID,
		Count:    count,
		RawEntry: raw,
		Extra: map[string]string{
			"item_kind": "stackable",
			"pvf_path":  "stackable/10090001/10090760.stk",
		},
	}
}

func saveChronicleFixture(t *testing.T, ctx context.Context, repos dnfrepo.Group, targetList byte, targetSlot int16, level byte, existingOption bool) {
	t.Helper()
	saveTestCharacter(t, ctx, repos, dnfrepo.CharacterRecord{
		CharacterID: "77",
		AccountID:   "acc",
		Job:         "15",
		Stats:       map[string]int64{"grow_type": 34},
	})
	saveTestInventory(t, ctx, repos, dnfrepo.InventoryRecord{
		CharacterID: "77",
		Slots: map[string]dnfrepo.ItemStack{
			slotKey(listTypeMain, 67): testChronicleMaterial(2),
		},
	})
	stack := testChronicleTarget(level, existingOption)
	if targetList == listTypeMain {
		record := loadTestInventory(t, ctx, repos, "77")
		record.Slots[slotKey(listTypeMain, targetSlot)] = stack
		saveTestInventory(t, ctx, repos, record)
		return
	}
	if err := repos.Equipment.Save(ctx, dnfrepo.EquipmentRecord{
		CharacterID: "77",
		Entries: map[string]dnfrepo.EquipmentEntry{
			"11": {SlotIndex: 11, ItemID: stack.ItemID, RawEntry: stack.RawEntry, Extra: stack.Extra},
		},
	}); err != nil {
		t.Fatal(err)
	}
}

func testChronicleResolver(resolution alignedcmd.ChronicleRefineResolution) alignedcmd.ChronicleRefineResolver {
	return func(materialItemID, targetItemID int64, characterJob, firstGrowType, optionNo byte) (alignedcmd.ChronicleRefineResolution, error) {
		return resolution, nil
	}
}

func TestOwnerChronicleRefineGuaranteedSuccessPersistsOption(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	saveChronicleFixture(t, ctx, repos, listTypeMain, 29, 0, false)
	owner, err := NewOwner(repos)
	if err != nil {
		t.Fatal(err)
	}
	result, err := owner.RefineChronicle(ctx, testChronicleCommand(listTypeMain, 29), testChronicleResolver(testChronicleResolution([2]byte{100, 11})))
	if err != nil {
		t.Fatal(err)
	}
	if !result.Success || !result.RefineSucceeded || result.TargetDestroyed || result.MaterialRemainingCount != 1 || len(result.FailureRewards) != 0 {
		t.Fatalf("result = %+v", result)
	}
	loaded := loadTestInventory(t, ctx, repos, "77")
	options, err := parseChronicleOptions(loaded.Slots[slotKey(listTypeMain, 29)].RawEntry)
	if err != nil || len(options) != 1 || options[0].CanonicalAuraOptionID != 1256 ||
		options[0].CharacterJob != 15 || options[0].FirstGrowType != 2 ||
		options[0].EquipmentType != 12 || options[0].OptionNo != 11 {
		t.Fatalf("options=%+v err=%v", options, err)
	}
	if got := loaded.Slots[slotKey(listTypeMain, 29)].Extra["middle_data_1a"]; got != "01e8040000000000000f0002000c000b00" {
		t.Fatalf("persisted Chronicle vector=%q", got)
	}
}

func TestOwnerChronicleRefineFemaleGunnerUsesCanonicalJobAndFirstGrow(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	saveChronicleFixture(t, ctx, repos, listTypeMain, 29, 0, false)
	character, found, err := repos.Character.Load(ctx, "77")
	if err != nil || !found {
		t.Fatalf("character found=%t err=%v", found, err)
	}
	character.Job = "5"
	character.Stats["grow_type"] = 0x21
	if err := repos.Character.Save(ctx, character); err != nil {
		t.Fatal(err)
	}
	owner, err := NewOwner(repos)
	if err != nil {
		t.Fatal(err)
	}
	resolver := func(materialItemID, targetItemID int64, characterJob, firstGrowType, optionNo byte) (alignedcmd.ChronicleRefineResolution, error) {
		if characterJob != 5 || firstGrowType != 1 {
			t.Fatalf("resolver profession=(%d,%d), want female gunner (5,1)", characterJob, firstGrowType)
		}
		return testChronicleResolution([2]byte{100, 100}), nil
	}
	result, err := owner.RefineChronicle(ctx, testChronicleCommand(listTypeMain, 29), resolver)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Success || !result.RefineSucceeded {
		t.Fatalf("result = %+v", result)
	}
	loaded := loadTestInventory(t, ctx, repos, "77")
	options, err := parseChronicleOptions(loaded.Slots[slotKey(listTypeMain, 29)].RawEntry)
	if err != nil || len(options) != 1 || options[0].CharacterJob != 5 || options[0].FirstGrowType != 1 {
		t.Fatalf("female-gunner options=%+v err=%v", options, err)
	}
}

func TestValidChronicleRefineResolutionUsesClientDefinitionTypes(t *testing.T) {
	resolution := testChronicleResolution([2]byte{100, 100})
	for _, code := range []byte{12, 14, 15, 16, 17, 18, 19, 20, 21, 22, 23, 25} {
		resolution.TargetEquipmentTypeCode = code
		if !validChronicleRefineResolution(resolution) {
			t.Fatalf("current-client equipment definition type %d rejected", code)
		}
	}
	for _, code := range []byte{0, 11, 13, 24, 26, 33, 34} {
		resolution.TargetEquipmentTypeCode = code
		if validChronicleRefineResolution(resolution) {
			t.Fatalf("unsupported Chronicle equipment definition type %d accepted", code)
		}
	}
}

func TestOwnerChronicleRefineSacredSecondAuraGuaranteedSuccess(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	saveChronicleFixture(t, ctx, repos, listTypeMain, 29, 0, true)
	owner, _ := NewOwner(repos)
	owner.chronicleRandom = func(int) int {
		t.Fatal("100 percent second aura must not draw a chance roll")
		return 0
	}
	result, err := owner.RefineChronicle(ctx, testChronicleCommand(listTypeMain, 29), testChronicleResolver(testChronicleResolution([2]byte{100, 100})))
	if err != nil {
		t.Fatal(err)
	}
	if !result.Success || !result.RefineSucceeded || result.OptionCount != 2 {
		t.Fatalf("result = %+v", result)
	}
	loaded := loadTestInventory(t, ctx, repos, "77")
	options, err := parseChronicleOptions(loaded.Slots[slotKey(listTypeMain, 29)].RawEntry)
	if err != nil || len(options) != 2 || options[1].CanonicalAuraOptionID != 1256 ||
		options[1].EquipmentType != 12 || options[1].OptionNo != 11 {
		t.Fatalf("options=%+v err=%v", options, err)
	}
}

func TestOwnerChronicleRefineChanceFailureDestroysAndGrantsFixedRewards(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	saveChronicleFixture(t, ctx, repos, listTypeMain, 29, 6, true)
	owner, _ := NewOwner(repos)
	owner.chronicleRandom = func(int) int { return 50 }
	result, err := owner.RefineChronicle(ctx, testChronicleCommand(listTypeMain, 29), testChronicleResolver(testChronicleResolution([2]byte{100, 11})))
	if err != nil {
		t.Fatal(err)
	}
	if !result.Success || result.RefineSucceeded || !result.TargetDestroyed || result.Probability != 11 || result.ProbabilityRoll != 50 || !result.Changed {
		t.Fatalf("result = %+v", result)
	}
	if len(result.FailureRewards) != 2 || result.FailureRewards[0].ItemID != 3311 || result.FailureRewards[0].Count != 5 ||
		result.FailureRewards[1].ItemID != 3037 || result.FailureRewards[1].Count != 20 {
		t.Fatalf("failure rewards = %+v", result.FailureRewards)
	}
	loaded := loadTestInventory(t, ctx, repos, "77")
	if loaded.Slots[slotKey(listTypeMain, 67)].Count != 1 {
		t.Fatalf("material stack = %+v", loaded.Slots[slotKey(listTypeMain, 67)])
	}
	if _, found := loaded.Slots[slotKey(listTypeMain, 29)]; found {
		t.Fatalf("destroyed target still exists: %+v", loaded.Slots[slotKey(listTypeMain, 29)])
	}
	if stack := loaded.Slots[slotKey(listTypeMain, 54)]; stack.ItemID != 3311 || stack.Count != 5 {
		t.Fatalf("fragment stack = %+v", stack)
	}
	account, found, loadErr := repos.AccountInventory.Load(ctx, "acc")
	if loadErr != nil || !found {
		t.Fatalf("account found=%t err=%v", found, loadErr)
	}
	if stack := account.Slots[dnfrepo.AccountSharedInventorySlotKey(358)]; stack.ItemID != 3037 || stack.Count != 20 {
		t.Fatalf("account cube stack = %+v", stack)
	}
	if len(account.Slots) != 1 {
		t.Fatalf("unexpected account rewards = %+v", account.Slots)
	}
}

func TestOwnerChronicleRefineEquippedSuccessPersistsOption(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	saveChronicleFixture(t, ctx, repos, listTypeEquipment, 11, 8, false)
	owner, _ := NewOwner(repos)
	result, err := owner.RefineChronicle(ctx, testChronicleCommand(listTypeEquipment, 11), testChronicleResolver(testChronicleResolution([2]byte{100, 11})))
	if err != nil {
		t.Fatal(err)
	}
	if !result.Success || !result.RefineSucceeded || result.OptionCount != 1 {
		t.Fatalf("result = %+v", result)
	}
	equipment, found, err := repos.Equipment.Load(ctx, "77")
	if err != nil || !found {
		t.Fatalf("equipment found=%t err=%v", found, err)
	}
	options, parseErr := parseChronicleOptions(equipment.Entries["11"].RawEntry)
	if parseErr != nil || len(options) != 1 || options[0].CanonicalAuraOptionID != 1256 {
		t.Fatalf("equipped options=%+v err=%v", options, parseErr)
	}
}

func TestOwnerChronicleRefineEquippedFailureDeletesEquipment(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	saveChronicleFixture(t, ctx, repos, listTypeEquipment, 11, 8, true)
	owner, _ := NewOwner(repos)
	owner.chronicleRandom = func(int) int { return 50 }
	result, err := owner.RefineChronicle(ctx, testChronicleCommand(listTypeEquipment, 11), testChronicleResolver(testChronicleResolution([2]byte{100, 11})))
	if err != nil {
		t.Fatal(err)
	}
	if !result.Success || result.RefineSucceeded || !result.TargetDestroyed || len(result.FailureRewards) != 2 {
		t.Fatalf("result = %+v", result)
	}
	equipment, found, loadErr := repos.Equipment.Load(ctx, "77")
	if loadErr != nil || !found {
		t.Fatalf("equipment found=%t err=%v", found, loadErr)
	}
	if _, exists := equipment.Entries["11"]; exists {
		t.Fatalf("destroyed equipped target still exists: %+v", equipment.Entries["11"])
	}
}

func TestOwnerChronicleRefineFailureInventoryFullRollsBackEverything(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	saveChronicleFixture(t, ctx, repos, listTypeEquipment, 11, 6, true)
	inventory := loadTestInventory(t, ctx, repos, "77")
	for slot := int16(54); slot <= 104; slot++ {
		if slot == 67 {
			continue
		}
		inventory.Slots[slotKey(listTypeMain, slot)] = dnfrepo.ItemStack{
			ItemID: 900000 + int64(slot),
			Count:  1,
			Extra:  map[string]string{"item_kind": "stackable"},
		}
	}
	saveTestInventory(t, ctx, repos, inventory)
	owner, _ := NewOwner(repos)
	owner.chronicleRandom = func(int) int { return 50 }
	_, err := owner.RefineChronicle(ctx, testChronicleCommand(listTypeEquipment, 11), testChronicleResolver(testChronicleResolution([2]byte{100, 11})))
	if !errors.Is(err, ErrRandomRewardInventoryFull) {
		t.Fatalf("error = %v", err)
	}
	loaded := loadTestInventory(t, ctx, repos, "77")
	if loaded.Slots[slotKey(listTypeMain, 67)].Count != 2 {
		t.Fatalf("material changed despite rollback: %+v", loaded.Slots[slotKey(listTypeMain, 67)])
	}
	equipment, found, loadErr := repos.Equipment.Load(ctx, "77")
	if loadErr != nil || !found {
		t.Fatalf("equipment found=%t err=%v", found, loadErr)
	}
	if _, exists := equipment.Entries["11"]; !exists {
		t.Fatalf("equipped target removed despite rollback")
	}
	account, found, loadErr := repos.AccountInventory.Load(ctx, "acc")
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if found && len(account.Slots) != 0 {
		t.Fatalf("account reward committed despite rollback: %+v", account.Slots)
	}
}

func TestBuildChronicleRefineSuccessAckHasNoRewardRows(t *testing.T) {
	body := buildChronicleRefineResultAck(ChronicleRefineResult{
		MaterialSlotIndex:      67,
		MaterialRemainingCount: 1,
		TargetSlotIndex:        29,
		RefineSucceeded:        true,
	})
	if len(body) != 10 || body[0] != 1 || binary.LittleEndian.Uint16(body[1:3]) != 67 ||
		binary.LittleEndian.Uint16(body[3:5]) != 1 || body[5] != 1 || body[6] != 0 ||
		binary.LittleEndian.Uint16(body[7:9]) != 29 || body[9] != 0 {
		t.Fatalf("success ack = %x", body)
	}
}

func TestBuildChronicleRefineFailureAckIncludesOnlyFragmentAndCube(t *testing.T) {
	body := buildChronicleRefineResultAck(ChronicleRefineResult{
		MaterialSlotIndex:      67,
		MaterialRemainingCount: 1,
		TargetSlotIndex:        29,
		FailureRewards: []ChronicleRefineReward{
			{SlotIndex: 54, ItemID: 3311, Count: 5},
			{SlotIndex: 358, ItemID: 3037, Count: 20},
		},
	})
	if len(body) != 30 || body[0] != 1 || binary.LittleEndian.Uint16(body[1:3]) != 67 ||
		binary.LittleEndian.Uint16(body[3:5]) != 1 || body[5] != 0 || body[6] != 0 ||
		binary.LittleEndian.Uint16(body[7:9]) != 29 || body[9] != 2 ||
		binary.LittleEndian.Uint16(body[10:12]) != 54 || binary.LittleEndian.Uint32(body[12:16]) != 3311 ||
		binary.LittleEndian.Uint32(body[16:20]) != 5 || binary.LittleEndian.Uint16(body[20:22]) != 358 ||
		binary.LittleEndian.Uint32(body[22:26]) != 3037 || binary.LittleEndian.Uint32(body[26:30]) != 20 {
		t.Fatalf("failure ack = %x", body)
	}
}

func TestDecodeChronicleRefineCapturedRequest(t *testing.T) {
	body := []byte{0x43, 0x00, 0x08, 0xf9, 0x99, 0x00, 0x00, 0x1d, 0x00, 0x97, 0x2c, 0xea, 0x06, 0x0b}
	request, err := DecodeChronicleRefineRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	if request.MaterialSlotIndex != 67 || request.MaterialItemTemplateID != 10090760 ||
		request.TargetListType != 0 || request.TargetSlotIndex != 29 ||
		request.TargetItemTemplateID != 116010135 || request.OptionNo != 11 {
		t.Fatalf("request = %+v", request)
	}
	for _, malformed := range [][]byte{body[:13], append(append([]byte(nil), body...), 0)} {
		if _, err := DecodeChronicleRefineRequest(malformed); err == nil {
			t.Fatalf("malformed body accepted: %x", malformed)
		}
	}
}
