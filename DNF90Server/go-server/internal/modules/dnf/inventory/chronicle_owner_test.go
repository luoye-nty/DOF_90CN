package inventory

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"testing"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

func TestDecodeChronicleGrowthRequestCurrentEXEBody(t *testing.T) {
	body, err := hex.DecodeString("5400ee059a001900972cea0601a000ef0c0000")
	if err != nil {
		t.Fatal(err)
	}
	req, err := DecodeChronicleGrowthRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	if req.TicketSlotIndex != 84 || req.TicketItemCode != 10094062 || req.TargetSlotIndex != 25 ||
		req.TargetItemCode != 116010135 || req.TargetTaggedValue != 1 || req.MaterialSlotIndex != 160 || req.MaterialItemCode != 3311 {
		t.Fatalf("decoded request = %+v", req)
	}
	if _, err := DecodeChronicleGrowthRequest(append(body, 0)); err == nil {
		t.Fatal("20-byte body accepted")
	}
}

func TestDecodeChronicleEquipmentEmancipationAutoMaterialSentinel(t *testing.T) {
	// Live Lv85 relic-conversion request:
	// ticket=(108,10149529), target=(13,101030330), tagged=0,
	// followed by the current client's multi-material sentinel (-1,0xffff).
	body, err := hex.DecodeString("6c0099de9a000d00ba99050600ffffffff0000")
	if err != nil {
		t.Fatal(err)
	}
	req, err := DecodeChronicleGrowthRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	if req.TicketSlotIndex != 108 || req.TicketItemCode != 10149529 ||
		req.TargetSlotIndex != 13 || req.TargetItemCode != 101030330 ||
		req.TargetTaggedValue != 0 || req.MaterialSlotIndex != -1 || req.MaterialItemCode != 0xffff {
		t.Fatalf("decoded request = %+v", req)
	}

	invalid := append([]byte(nil), body...)
	invalid[15] = 0xfe
	if _, err := DecodeChronicleGrowthRequest(invalid); err == nil {
		t.Fatal("mismatched auto-material sentinel accepted")
	}
}

func TestHandlerConvertsEquipmentEmancipationAfterMoveAndSort(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	saveTestInventory(t, ctx, repos, dnfrepo.InventoryRecord{CharacterID: "84", Slots: map[string]dnfrepo.ItemStack{
		slotKey(listTypeMain, 9):   {ItemID: 700, Count: 1, Extra: map[string]string{"item_kind": "equipment"}},
		slotKey(listTypeMain, 13):  {ItemID: 101030330, Count: 1, RawEntry: make([]byte, chronicleEquipmentRawSize), Extra: map[string]string{"item_kind": "equipment"}},
		slotKey(listTypeMain, 65):  {ItemID: 900, Count: 1, Extra: map[string]string{"item_kind": "stackable"}},
		slotKey(listTypeMain, 108): {ItemID: 10149529, Count: 1, Extra: map[string]string{"item_kind": "stackable"}},
		slotKey(listTypeMain, 121): {ItemID: 901, Count: 1, Extra: map[string]string{"item_kind": "stackable"}},
		slotKey(listTypeMain, 140): {ItemID: 10149518, Count: 10, Extra: map[string]string{"item_kind": "stackable"}},
	}})
	if err := repos.AccountInventory.Save(ctx, dnfrepo.AccountInventoryRecord{AccountID: "dnf:1", Slots: map[string]dnfrepo.ItemStack{
		dnfrepo.AccountSharedInventorySlotKey(360): {ItemID: 10100115, Count: 36},
		dnfrepo.AccountSharedInventorySlotKey(361): {ItemID: 10100116, Count: 24},
		dnfrepo.AccountSharedInventorySlotKey(362): {ItemID: 10099773, Count: 12},
	}}); err != nil {
		t.Fatal(err)
	}
	owner, err := NewOwner(repos)
	if err != nil {
		t.Fatal(err)
	}
	for _, move := range []MoveItemspaceRequest{
		{SourceListType: listTypeMain, SourceSlotIndex: 13, MoveCount: 1, DestinationListType: listTypeMain, DestinationSlotIndex: 20},
		{SourceListType: listTypeMain, SourceSlotIndex: 108, MoveCount: 1, DestinationListType: listTypeMain, DestinationSlotIndex: 100},
		{SourceListType: listTypeMain, SourceSlotIndex: 140, MoveCount: 10, DestinationListType: listTypeMain, DestinationSlotIndex: 150},
	} {
		if result, moveErr := owner.Move(ctx, NewMoveCommand(alignedcmd.Request{AccountID: "dnf:1", SelectedCharacterID: 84}, move)); moveErr != nil || !result.Changed {
			t.Fatalf("move=%+v err=%v", result, moveErr)
		}
	}
	for _, category := range []byte{1, 2, 3} {
		if _, sortErr := owner.Sort(ctx, NewSortCommand(alignedcmd.Request{AccountID: "dnf:1", SelectedCharacterID: 84}, SortItemRequest{ListType: listTypeMain, Category: category})); sortErr != nil {
			t.Fatal(sortErr)
		}
	}
	bag := loadTestInventory(t, ctx, repos, "84")
	findSlot := func(itemID int64) int16 {
		for key, stack := range bag.Slots {
			listType, slot, ok := parseSlotKey(key)
			if ok && listType == listTypeMain && stack.ItemID == itemID {
				return slot
			}
		}
		return -1
	}
	ticketSlot := findSlot(10149529)
	targetSlot := findSlot(101030330)
	if ticketSlot == 108 || targetSlot == 13 || ticketSlot < 0 || targetSlot < 0 {
		t.Fatalf("sort did not produce dynamic slots ticket=%d target=%d", ticketSlot, targetSlot)
	}
	body := make([]byte, 19)
	binary.LittleEndian.PutUint16(body[0:2], uint16(ticketSlot))
	binary.LittleEndian.PutUint32(body[2:6], 10149529)
	binary.LittleEndian.PutUint16(body[6:8], uint16(targetSlot))
	binary.LittleEndian.PutUint32(body[8:12], 101030330)
	body[12] = 0
	binary.LittleEndian.PutUint16(body[13:15], 0xffff)
	binary.LittleEndian.PutUint32(body[15:19], 0xffff)
	result, err := NewHandler().Handle(ctx, alignedcmd.Request{
		Opcode: uint16(dnfenum.CmdPacketUpgradeChronicle), Body: body,
		AccountID: "dnf:1", SelectedCharacterID: 84, Repositories: repos,
		ChronicleGrowthResolver: func(ticket, target int64) (alignedcmd.ChronicleGrowthResolution, error) {
			resolution := testEquipmentEmancipationResolution(ticket)
			resolution.Conversion.Inputs[0].ItemID = 10149518
			return resolution, nil
		},
	})
	if err != nil || !result.Handled || !result.ResponseAllowed || len(result.UpperResponses) != 1 ||
		len(result.UpperResponses[0].Body) < 3 || result.UpperResponses[0].Body[0] != 1 || result.UpperResponses[0].Body[1] != 1 {
		t.Fatalf("handler result=%+v err=%v", result, err)
	}
	after := loadTestInventory(t, ctx, repos, "84")
	if got := after.Slots[slotKey(listTypeMain, targetSlot)]; got.ItemID != 104000504 || got.Count != 1 {
		t.Fatalf("converted dynamic target=%+v", got)
	}
}

func TestChronicleGrowthSuccessAckAndPreResponseProjection(t *testing.T) {
	result := ChronicleGrowthResult{Succeeded: true, TargetSlot: 25, Consumptions: []ChronicleGrowthConsumption{
		{ListType: listTypeMain, Slot: 84, Count: 1},
		{ListType: listTypeMain, Slot: 160, Count: 7},
	}}
	if got, want := hex.EncodeToString(buildChronicleGrowthSuccessAck(result)), "0101020054000100000000a00007000000"; got != want {
		t.Fatalf("op271 success ACK=%s want=%s", got, want)
	}
	plan := ownerAppliedChronicleGrowthResult(271, Command{Operation: "upgrade_chronicle"}, result)
	if len(plan.PreResponseItemSlotRefreshes) != 1 || plan.PreResponseItemSlotRefreshes[0].SlotIndex != 25 || len(plan.ItemSlotRefreshes) != 0 {
		t.Fatalf("refresh plan = %+v", plan)
	}
}

func TestOwnerConvertsEquipmentEmancipationRecipeAtomically(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	sourceRaw := make([]byte, chronicleEquipmentRawSize)
	sourceRaw[0x0A] = 12
	sourceRaw[0x13] = 2
	sourceRaw[0x14] = 12
	sourceRaw[currentForgingRawOffset] = 6
	saveTestInventory(t, ctx, repos, dnfrepo.InventoryRecord{CharacterID: "81", Slots: map[string]dnfrepo.ItemStack{
		slotKey(listTypeMain, 1): {ItemID: 10149520, Count: 2, Extra: map[string]string{"item_kind": "stackable"}},
		slotKey(listTypeMain, 2): {ItemID: 104000224, Count: 1, RawEntry: sourceRaw, Extra: map[string]string{
			"item_kind": "equipment", "upgrade_level": "12", "amplify_type": "2", "amplify_value": "12", "forging_level": "6",
		}},
		slotKey(listTypeMain, 3): {ItemID: 10149509, Count: 6},
		slotKey(listTypeMain, 4): {ItemID: 10149509, Count: 8},
	}})
	if err := repos.AccountInventory.Save(ctx, dnfrepo.AccountInventoryRecord{AccountID: "dnf:1", Slots: map[string]dnfrepo.ItemStack{
		dnfrepo.AccountSharedInventorySlotKey(360): {ItemID: 10100115, Count: 40},
		dnfrepo.AccountSharedInventorySlotKey(361): {ItemID: 10100116, Count: 24},
		dnfrepo.AccountSharedInventorySlotKey(362): {ItemID: 10099773, Count: 12},
	}}); err != nil {
		t.Fatal(err)
	}
	owner, err := NewOwner(repos)
	if err != nil {
		t.Fatal(err)
	}
	cmd := NewChronicleGrowthCommand(alignedcmd.Request{AccountID: "dnf:1", SelectedCharacterID: 81}, ChronicleGrowthRequest{
		TicketSlotIndex: 1, TicketItemCode: 10149520, TargetSlotIndex: 2, TargetItemCode: 104000224,
		MaterialSlotIndex: 4, MaterialItemCode: 10149509,
	})
	result, err := owner.UpgradeChronicle(ctx, cmd, func(ticket, target int64) (alignedcmd.ChronicleGrowthResolution, error) {
		return testEquipmentEmancipationResolution(ticket), nil
	})
	if err != nil || !result.Succeeded || !result.Conversion || result.OutputItemID != 104000504 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	record := loadTestInventory(t, ctx, repos, "81")
	if got := record.Slots[slotKey(listTypeMain, 1)].Count; got != 1 {
		t.Fatalf("ticket count=%d want=1", got)
	}
	if got := record.Slots[slotKey(listTypeMain, 3)].Count; got != 4 {
		t.Fatalf("remaining split relic-fragment stack count=%d want=4", got)
	}
	if _, found := record.Slots[slotKey(listTypeMain, 4)]; found {
		t.Fatal("client-selected relic-fragment stack was not consumed first")
	}
	account, found, loadErr := repos.AccountInventory.Load(ctx, "dnf:1")
	if loadErr != nil || !found {
		t.Fatalf("load account inventory found=%v err=%v", found, loadErr)
	}
	if _, found := account.Slots[dnfrepo.AccountSharedInventorySlotKey(362)]; found {
		t.Fatal("rare soul stack was not consumed")
	}
	if _, found := account.Slots[dnfrepo.AccountSharedInventorySlotKey(361)]; found {
		t.Fatal("advanced soul stack was not consumed")
	}
	if got := account.Slots[dnfrepo.AccountSharedInventorySlotKey(360)].Count; got != 4 {
		t.Fatalf("common soul count=%d want=4", got)
	}
	target := record.Slots[slotKey(listTypeMain, 2)]
	if target.ItemID != 104000504 || target.Count != 1 || !target.Bind || len(target.RawEntry) != chronicleEquipmentRawSize {
		t.Fatalf("converted target=%+v", target)
	}
	_, amplify := upgradeAmplifyState(target)
	if upgradeLevelOf(target) != 0 || amplify != 0 || chronicleForgingLevel(target) != 0 {
		t.Fatalf("ordinary conversion inherited source upgrade state: %+v", target.Extra)
	}
}

func TestOwnerConvertsEquipmentEmancipationWithAutoMaterialSentinel(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	saveTestInventory(t, ctx, repos, dnfrepo.InventoryRecord{CharacterID: "83", Slots: map[string]dnfrepo.ItemStack{
		slotKey(listTypeMain, 108): {ItemID: 10149529, Count: 1, Extra: map[string]string{"item_kind": "stackable"}},
		slotKey(listTypeMain, 13):  {ItemID: 101030330, Count: 1, RawEntry: make([]byte, chronicleEquipmentRawSize), Extra: map[string]string{"item_kind": "equipment"}},
		slotKey(listTypeMain, 40):  {ItemID: 10149518, Count: 100},
	}})
	if err := repos.AccountInventory.Save(ctx, dnfrepo.AccountInventoryRecord{AccountID: "dnf:1", Slots: map[string]dnfrepo.ItemStack{
		dnfrepo.AccountSharedInventorySlotKey(360): {ItemID: 10100115, Count: 216},
		dnfrepo.AccountSharedInventorySlotKey(361): {ItemID: 10100116, Count: 195},
		dnfrepo.AccountSharedInventorySlotKey(362): {ItemID: 10099773, Count: 115},
	}}); err != nil {
		t.Fatal(err)
	}
	owner, err := NewOwner(repos)
	if err != nil {
		t.Fatal(err)
	}
	cmd := NewChronicleGrowthCommand(alignedcmd.Request{AccountID: "dnf:1", SelectedCharacterID: 83}, ChronicleGrowthRequest{
		TicketSlotIndex: 108, TicketItemCode: 10149529, TargetSlotIndex: 13, TargetItemCode: 101030330,
		TargetTaggedValue: 3, MaterialSlotIndex: -1, MaterialItemCode: 0xffff,
	})
	resolution := testEquipmentEmancipationResolution(10149529)
	resolution.Conversion.Inputs = []alignedcmd.ChronicleEquipmentConversionMaterial{
		{ItemID: 10149518, Count: 10},
		{ItemID: 10099773, Count: 12},
		{ItemID: 10100116, Count: 24},
		{ItemID: 10100115, Count: 36},
	}
	result, err := owner.UpgradeChronicle(ctx, cmd, func(ticket, target int64) (alignedcmd.ChronicleGrowthResolution, error) {
		return resolution, nil
	})
	if err != nil || !result.Succeeded || !result.Conversion || len(result.Consumptions) != 5 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	record := loadTestInventory(t, ctx, repos, "83")
	if _, found := record.Slots[slotKey(listTypeMain, 108)]; found {
		t.Fatal("conversion book was not consumed")
	}
	if got := record.Slots[slotKey(listTypeMain, 40)].Count; got != 90 {
		t.Fatalf("relic fragments=%d want=90", got)
	}
	account, found, loadErr := repos.AccountInventory.Load(ctx, "dnf:1")
	if loadErr != nil || !found {
		t.Fatalf("load account inventory found=%v err=%v", found, loadErr)
	}
	if got := account.Slots[dnfrepo.AccountSharedInventorySlotKey(362)].Count; got != 103 {
		t.Fatalf("rare souls=%d want=103", got)
	}
	if got := account.Slots[dnfrepo.AccountSharedInventorySlotKey(361)].Count; got != 171 {
		t.Fatalf("advanced souls=%d want=171", got)
	}
	if got := account.Slots[dnfrepo.AccountSharedInventorySlotKey(360)].Count; got != 180 {
		t.Fatalf("common souls=%d want=180", got)
	}
	wantSlots := []int16{108, 40, 360, 361, 362}
	for index, wantSlot := range wantSlots {
		if result.Consumptions[index].ListType != listTypeMain || result.Consumptions[index].Slot != wantSlot {
			t.Fatalf("consumption[%d]=%+v want list=%d slot=%d", index, result.Consumptions[index], listTypeMain, wantSlot)
		}
	}
}

func TestOwnerEquipmentEmancipationMissingMaterialRollsBackEverything(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	before := dnfrepo.InventoryRecord{CharacterID: "82", Slots: map[string]dnfrepo.ItemStack{
		slotKey(listTypeMain, 1): {ItemID: 10149520, Count: 1},
		slotKey(listTypeMain, 2): {ItemID: 104000224, Count: 1, RawEntry: make([]byte, chronicleEquipmentRawSize), Extra: map[string]string{"item_kind": "equipment"}},
		slotKey(listTypeMain, 3): {ItemID: 10149509, Count: 10},
	}}
	saveTestInventory(t, ctx, repos, before)
	beforeAccount := dnfrepo.AccountInventoryRecord{AccountID: "dnf:1", Slots: map[string]dnfrepo.ItemStack{
		dnfrepo.AccountSharedInventorySlotKey(360): {ItemID: 10100115, Count: 36},
		dnfrepo.AccountSharedInventorySlotKey(361): {ItemID: 10100116, Count: 24},
		dnfrepo.AccountSharedInventorySlotKey(362): {ItemID: 10099773, Count: 11},
	}}
	if err := repos.AccountInventory.Save(ctx, beforeAccount); err != nil {
		t.Fatal(err)
	}
	owner, err := NewOwner(repos)
	if err != nil {
		t.Fatal(err)
	}
	cmd := NewChronicleGrowthCommand(alignedcmd.Request{AccountID: "dnf:1", SelectedCharacterID: 82}, ChronicleGrowthRequest{
		TicketSlotIndex: 1, TicketItemCode: 10149520, TargetSlotIndex: 2, TargetItemCode: 104000224,
		MaterialSlotIndex: 3, MaterialItemCode: 10149509,
	})
	if _, err := owner.UpgradeChronicle(ctx, cmd, func(ticket, target int64) (alignedcmd.ChronicleGrowthResolution, error) {
		return testEquipmentEmancipationResolution(ticket), nil
	}); err == nil {
		t.Fatal("conversion with an insufficient soul stack succeeded")
	}
	after := loadTestInventory(t, ctx, repos, "82")
	for key, want := range before.Slots {
		got, found := after.Slots[key]
		if !found || got.ItemID != want.ItemID || got.Count != want.Count {
			t.Fatalf("slot %s changed after rollback: got=%+v found=%v want=%+v", key, got, found, want)
		}
	}
	afterAccount, found, loadErr := repos.AccountInventory.Load(ctx, "dnf:1")
	if loadErr != nil || !found {
		t.Fatalf("load account inventory found=%v err=%v", found, loadErr)
	}
	for key, want := range beforeAccount.Slots {
		got, exists := afterAccount.Slots[key]
		if !exists || got.ItemID != want.ItemID || got.Count != want.Count {
			t.Fatalf("account slot %s changed after rollback: got=%+v found=%v want=%+v", key, got, exists, want)
		}
	}
}

func TestBuildEquipmentEmancipationOutputHonorsRetentionPolicy(t *testing.T) {
	source := dnfrepo.ItemStack{ItemID: 700, Count: 1, RawEntry: make([]byte, chronicleEquipmentRawSize), Extra: map[string]string{}}
	setUpgradeLevel(&source, 12)
	setAmplifyState(&source, 3, 12)
	source.Extra["forging_level"] = "6"
	source.Extra["byte_57"] = "6"
	source.RawEntry[currentForgingRawOffset] = 6
	setEnchantFields(&source, 12345, 4)
	conversion := *testEquipmentEmancipationResolution(10149519).Conversion
	conversion.UpgradeMaximum = 10
	conversion.AmplifyMaximum = 10
	conversion.ForgingMaximum = 4
	output := buildChronicleConversionOutput(2, source, conversion)
	_, amplify := upgradeAmplifyState(output)
	if upgradeLevelOf(output) != 10 || amplify != 10 || chronicleForgingLevel(output) != 4 ||
		firstExtraInt(output.Extra, 0, "enchant_card_id") != 12345 || enchantUpgradeCountOf(output) != 4 {
		t.Fatalf("capped conversion output=%+v", output.Extra)
	}

	conversion.UpgradeMaximum = -1
	conversion.AmplifyMaximum = -1
	conversion.ForgingMaximum = -1
	conversion.PreserveAttributes = true
	output = buildChronicleConversionOutput(2, source, conversion)
	_, amplify = upgradeAmplifyState(output)
	if upgradeLevelOf(output) != 12 || amplify != 12 || chronicleForgingLevel(output) != 6 || enchantUpgradeCountOf(output) != 4 {
		t.Fatalf("preserving conversion output=%+v", output.Extra)
	}
}

func testEquipmentEmancipationResolution(ticketItemID int64) alignedcmd.ChronicleGrowthResolution {
	return alignedcmd.ChronicleGrowthResolution{
		TicketItemID: ticketItemID,
		Conversion: &alignedcmd.ChronicleEquipmentConversionResolution{
			TicketType: 8,
			Inputs: []alignedcmd.ChronicleEquipmentConversionMaterial{
				{ItemID: 10149509, Count: 10},
				{ItemID: 10099773, Count: 12},
				{ItemID: 10100116, Count: 24},
				{ItemID: 10100115, Count: 36},
			},
			OutputItemID: 104000504, OutputPVFPath: "equipment/character/gunner/weapon/revolver/104000504.equ",
			OutputEquipmentType: "weapon", OutputRarity: 4, OutputGrade: 50, OutputMinimumLevel: 15,
			OutputDurability: 35, OutputQualitySeed: 123456, BindOutput: true,
			UpgradeMaximum: -1, AmplifyMaximum: -1, ForgingMaximum: -1,
		},
	}
}

func TestOwnerUpgradeChroniclePersistsRaw58AndPreservesOtherRegions(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	raw := make([]byte, chronicleEquipmentRawSize)
	raw[0x57] = 1
	raw[0x58] = 0
	raw[0x59] = 0x5A
	copy(raw[0x6E:0x72], []byte{0x61, 0x62, 0x63, 0x64})
	copy(raw[0x72:0x77], []byte{1, 2, 3, 4, 5})
	saveTestInventory(t, ctx, repos, dnfrepo.InventoryRecord{CharacterID: "77", Slots: map[string]dnfrepo.ItemStack{
		slotKey(listTypeMain, 1): {ItemID: 10094062, Count: 2, Extra: map[string]string{"item_kind": "stackable"}},
		slotKey(listTypeMain, 2): {ItemID: 700, Count: 1, RawEntry: raw, Extra: map[string]string{"item_kind": "equipment", "rarity": "5", "byte_57": "1"}},
		slotKey(listTypeMain, 3): {ItemID: 3311, Count: 100},
	}})
	owner, err := NewOwner(repos)
	if err != nil {
		t.Fatal(err)
	}
	cmd := NewChronicleGrowthCommand(alignedcmd.Request{SelectedCharacterID: 77}, ChronicleGrowthRequest{
		TicketSlotIndex: 1, TicketItemCode: 10094062, TargetSlotIndex: 2, TargetItemCode: 700,
		TargetTaggedValue: 0, MaterialSlotIndex: 3, MaterialItemCode: 3311,
	})
	result, err := owner.UpgradeChronicle(ctx, cmd, func(ticket, target int64) (alignedcmd.ChronicleGrowthResolution, error) {
		return testChronicleGrowthResolution(ticket, "coat"), nil
	})
	if err != nil || !result.Succeeded || result.OldLevel != 70 || result.NewLevel != 73 || result.MaterialCount != 6 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	record := loadTestInventory(t, ctx, repos, "77")
	if record.Slots[slotKey(listTypeMain, 1)].Count != 1 || record.Slots[slotKey(listTypeMain, 3)].Count != 94 {
		t.Fatalf("consumptions not persisted: %+v", record.Slots)
	}
	target := record.Slots[slotKey(listTypeMain, 2)]
	if target.RawEntry[0x58] != 3 || target.Extra["byte_58"] != "3" || target.Extra["value_58"] != "3" {
		t.Fatalf("target energy not synchronized: raw=%d extra=%+v", target.RawEntry[0x58], target.Extra)
	}
	if target.RawEntry[0x57] != 1 || target.RawEntry[0x59] != 0x5A ||
		hex.EncodeToString(target.RawEntry[0x6E:0x72]) != "61626364" ||
		hex.EncodeToString(target.RawEntry[0x72:0x77]) != "0102030405" {
		t.Fatalf("adjacent/special regions changed: %x", target.RawEntry[0x57:0x77])
	}
}

func TestOwnerUpgradeChronicleClampsAtConditionMaximum(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	raw := make([]byte, chronicleEquipmentRawSize)
	raw[chronicleEquipmentLevelOffset] = 19
	saveTestInventory(t, ctx, repos, dnfrepo.InventoryRecord{CharacterID: "79", Slots: map[string]dnfrepo.ItemStack{
		slotKey(listTypeMain, 1): {ItemID: 10094062, Count: 1},
		slotKey(listTypeMain, 2): {ItemID: 700, Count: 1, RawEntry: raw, Extra: map[string]string{"rarity": "5"}},
		slotKey(listTypeMain, 3): {ItemID: 3311, Count: 1000},
	}})
	owner, err := NewOwner(repos)
	if err != nil {
		t.Fatal(err)
	}
	cmd := NewChronicleGrowthCommand(alignedcmd.Request{SelectedCharacterID: 79}, ChronicleGrowthRequest{
		TicketSlotIndex: 1, TicketItemCode: 10094062, TargetSlotIndex: 2, TargetItemCode: 700,
		TargetTaggedValue: 19, MaterialSlotIndex: 3, MaterialItemCode: 3311,
	})
	result, err := owner.UpgradeChronicle(ctx, cmd, func(ticket, target int64) (alignedcmd.ChronicleGrowthResolution, error) {
		return testChronicleGrowthResolution(ticket, "coat"), nil
	})
	if err != nil || result.NewLevel != 90 || result.OldLevel != 89 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	record := loadTestInventory(t, ctx, repos, "79")
	if record.Slots[slotKey(listTypeMain, 2)].RawEntry[chronicleEquipmentLevelOffset] != 20 {
		t.Fatalf("clamped energy=%d want=20", record.Slots[slotKey(listTypeMain, 2)].RawEntry[chronicleEquipmentLevelOffset])
	}
}

func TestOwnerUpgradeChronicleAllowsConsecutiveUsesWithStableClientTag(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	raw := make([]byte, chronicleEquipmentRawSize)
	saveTestInventory(t, ctx, repos, dnfrepo.InventoryRecord{CharacterID: "78", Slots: map[string]dnfrepo.ItemStack{
		slotKey(listTypeMain, 1): {ItemID: 10094062, Count: 2},
		slotKey(listTypeMain, 2): {ItemID: 700, Count: 1, RawEntry: raw, Extra: map[string]string{"byte_58": "0", "rarity": "5"}},
		slotKey(listTypeMain, 3): {ItemID: 3311, Count: 100},
	}})
	owner, err := NewOwner(repos)
	if err != nil {
		t.Fatal(err)
	}
	cmd := NewChronicleGrowthCommand(alignedcmd.Request{SelectedCharacterID: 78}, ChronicleGrowthRequest{
		TicketSlotIndex: 1, TicketItemCode: 10094062, TargetSlotIndex: 2, TargetItemCode: 700,
		TargetTaggedValue: 3, MaterialSlotIndex: 3, MaterialItemCode: 3311,
	})
	resolve := func(ticket, target int64) (alignedcmd.ChronicleGrowthResolution, error) {
		return testChronicleGrowthResolution(ticket, "coat"), nil
	}
	first, err := owner.UpgradeChronicle(ctx, cmd, resolve)
	if err != nil || first.OldLevel != 70 || first.NewLevel != 73 || first.MaterialCount != 6 {
		t.Fatalf("first result=%+v err=%v", first, err)
	}
	second, err := owner.UpgradeChronicle(ctx, cmd, resolve)
	if err != nil || second.OldLevel != 73 || second.NewLevel != 76 || second.MaterialCount != 6 {
		t.Fatalf("second result=%+v err=%v", second, err)
	}
	record := loadTestInventory(t, ctx, repos, "78")
	if _, ticketExists := record.Slots[slotKey(listTypeMain, 1)]; ticketExists ||
		record.Slots[slotKey(listTypeMain, 3)].Count != 88 ||
		record.Slots[slotKey(listTypeMain, 2)].RawEntry[0x58] != 6 {
		t.Fatalf("consecutive growth state = %+v", record.Slots)
	}
}

func TestOwnerUpgradeChronicleLegacyRowStartsAtPVFMinimum(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	raw := make([]byte, chronicleEquipmentRawSize)
	raw[0x59] = 0x5A
	saveTestInventory(t, ctx, repos, dnfrepo.InventoryRecord{CharacterID: "80", Slots: map[string]dnfrepo.ItemStack{
		slotKey(listTypeMain, 1): {ItemID: 10094062, Count: 1},
		slotKey(listTypeMain, 2): {ItemID: 700, Count: 1, RawEntry: raw, Extra: map[string]string{"rarity": "5"}},
		slotKey(listTypeMain, 3): {ItemID: 3311, Count: 100},
	}})
	owner, err := NewOwner(repos)
	if err != nil {
		t.Fatal(err)
	}
	cmd := NewChronicleGrowthCommand(alignedcmd.Request{SelectedCharacterID: 80}, ChronicleGrowthRequest{
		TicketSlotIndex: 1, TicketItemCode: 10094062, TargetSlotIndex: 2, TargetItemCode: 700,
		TargetTaggedValue: 3, MaterialSlotIndex: 3, MaterialItemCode: 3311,
	})
	result, err := owner.UpgradeChronicle(ctx, cmd, func(ticket, target int64) (alignedcmd.ChronicleGrowthResolution, error) {
		return testChronicleGrowthResolution(ticket, "coat"), nil
	})
	if err != nil || !result.Succeeded || result.OldLevel != 70 || result.NewLevel != 73 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	record := loadTestInventory(t, ctx, repos, "80")
	target := record.Slots[slotKey(listTypeMain, 2)]
	if target.RawEntry[chronicleEquipmentLevelOffset] != 3 || target.Extra["byte_58"] != "3" || target.Extra["value_58"] != "3" {
		t.Fatalf("legacy target energy not adopted and persisted: raw=%d extra=%+v", target.RawEntry[chronicleEquipmentLevelOffset], target.Extra)
	}
	if target.RawEntry[0x59] != 0x5A {
		t.Fatalf("adjacent equipment data changed: raw59=%02x", target.RawEntry[0x59])
	}
}

func TestChronicleRequiredMaterialUsesCurrentFormula(t *testing.T) {
	ordinary := dnfrepo.ItemStack{RawEntry: make([]byte, chronicleEquipmentRawSize)}
	coat := testChronicleGrowthResolution(10094062, "coat")
	if got, err := chronicleRequiredMaterial(coat, ordinary, 70); err != nil || got != 6 {
		t.Fatalf("ordinary coat material=%d err=%v, want 6", got, err)
	}
	amplified := ordinary
	amplified.RawEntry = append([]byte(nil), ordinary.RawEntry...)
	amplified.RawEntry[0x0A] = 12
	amplified.RawEntry[0x13] = 1
	amplified.RawEntry[0x14] = 1
	if got, err := chronicleRequiredMaterial(coat, amplified, 70); err != nil || got != 172 {
		t.Fatalf("+12 amplified coat material=%d err=%v, want 172", got, err)
	}
	forged := ordinary
	forged.RawEntry = append([]byte(nil), ordinary.RawEntry...)
	forged.RawEntry[0x57] = 2
	if got, err := chronicleRequiredMaterial(coat, forged, 70); err != nil || got != 7 {
		t.Fatalf("forged coat material=%d err=%v, want 7", got, err)
	}
}

func testChronicleGrowthResolution(ticketItemID int64, equipmentType string) alignedcmd.ChronicleGrowthResolution {
	return alignedcmd.ChronicleGrowthResolution{
		TicketItemID: ticketItemID, MaterialItemID: 3311, UpgradeLevel: 3,
		TargetMinimumLevel: 70, ConditionMinimum: 70, ConditionMaximum: 90,
		TargetRarity: 5, EquipmentType: equipmentType,
		Probability: []alignedcmd.ChronicleGrowthProbability{{MaximumLevel: 75, Weight: 100000}, {MaximumLevel: 85, Weight: 100000}},
		CostPolicy: alignedcmd.ChronicleGrowthCostPolicy{
			Rarity: 5, MaterialItemID: 3311, BaseCount: 3, Multiplier: 1,
			PartWeights: map[string]float64{"weapon": 1.2, "coat": 1},
			LevelWeights: []alignedcmd.ChronicleGrowthWeightSegment{
				{Minimum: 1, Maximum: 70, MinimumWeight: 1, MaximumWeight: 2},
				{Minimum: 70, Maximum: 85, MinimumWeight: 2, MaximumWeight: 3.5},
				{Minimum: 85, Maximum: 200, MinimumWeight: 3.5, MaximumWeight: 50},
			},
			UpgradeWeights: []alignedcmd.ChronicleGrowthWeightSegment{
				{Minimum: 0, Maximum: 3, MinimumWeight: 1, MaximumWeight: 1.2},
				{Minimum: 3, Maximum: 6, MinimumWeight: 1.2, MaximumWeight: 1.5},
				{Minimum: 6, Maximum: 10, MinimumWeight: 1.5, MaximumWeight: 2},
				{Minimum: 10, Maximum: 15, MinimumWeight: 2, MaximumWeight: 10},
			},
			AmplifyWeights: []alignedcmd.ChronicleGrowthWeightSegment{
				{Minimum: 0, Maximum: 3, MinimumWeight: 1, MaximumWeight: 1.5},
				{Minimum: 3, Maximum: 6, MinimumWeight: 1.5, MaximumWeight: 2.5},
				{Minimum: 6, Maximum: 10, MinimumWeight: 2.5, MaximumWeight: 4},
				{Minimum: 10, Maximum: 15, MinimumWeight: 4, MaximumWeight: 66},
			},
			ForgingWeights: []alignedcmd.ChronicleGrowthWeightSegment{{Minimum: 0, Maximum: 2, MinimumWeight: 1, MaximumWeight: 1.2}},
		},
	}
}
