package dnfbridge

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"strconv"
	"testing"

	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

func TestCurrentCargoTransportRequestIsExactNineByteWire(t *testing.T) {
	body := make([]byte, 9)
	binary.LittleEndian.PutUint16(body[0:2], 71)
	binary.LittleEndian.PutUint16(body[2:4], 29)
	body[4] = 2
	binary.LittleEndian.PutUint32(body[5:9], 4)
	request, err := decodeCurrentCargoTransportRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	if request.StoneSlot != 71 || request.TargetSlot != 29 || request.Mode != 2 || request.RecipientSelect != 4 {
		t.Fatalf("request=%+v", request)
	}
	for _, invalid := range [][]byte{body[:8], append(append([]byte(nil), body...), 0)} {
		if _, err := decodeCurrentCargoTransportRequest(invalid); err == nil {
			t.Fatalf("accepted length=%d", len(invalid))
		}
	}
}

func TestCurrentTransportStoneDefinitionAcceptsDirectAndActionTypeLayouts(t *testing.T) {
	tests := []struct {
		name         string
		text         string
		cargo        bool
		cargoType    int64
		creature     bool
		creatureType int64
	}{
		{name: "current cargo action", text: "[action type]\n`[cargo transport stone]` 8\n[/action type]\n", cargo: true, cargoType: 8},
		{name: "current creature action", text: "[action type]\n`[creature transport stone]` 2\n[/action type]\n", creature: true, creatureType: 2},
		{name: "legacy cargo section", text: "[cargo transport stone]\n5\n", cargo: true, cargoType: 5},
		{name: "unrelated action fails closed", text: "[action type]\n`[cargo something else]` 8\n[/action type]\n"},
		{name: "missing action integer fails closed", text: "[action type]\n`[cargo transport stone]`\n[/action type]\n"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			document, err := dnfpvf.Parse("stackable/test.stk", test.text)
			if err != nil {
				t.Fatal(err)
			}
			definition := dungeonDropItemDefinition{}
			parseCurrentTransportStoneDefinition(document, &definition)
			if definition.CargoTransportStone != test.cargo || definition.CargoTransportStoneType != test.cargoType ||
				definition.CreatureTransportStone != test.creature || definition.CreatureTransportStoneType != test.creatureType {
				t.Fatalf("definition=%+v", definition)
			}
		})
	}
}

func TestCurrentCargoTransportDirectStoneMovesEquipmentToAccountCargo(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	mustSaveCargoTransportAccount(t, ctx, repositories, "account-1", 8)
	if err := repositories.AccountInventory.Save(ctx, dnfrepo.AccountInventoryRecord{AccountID: "account-1", Slots: map[string]dnfrepo.ItemStack{}}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{CharacterID: "1", Slots: map[string]dnfrepo.ItemStack{
		"0:70": {ItemID: 9001, Count: 2},
		"0:12": {ItemID: 1001, Count: 1},
	}}); err != nil {
		t.Fatal(err)
	}
	catalog := &pvfDungeonDropCatalog{source: finishBridgePVFSource{}, itemCache: map[uint32]dungeonDropItemDefinition{
		9001: {ItemID: 9001, Kind: dungeonDropItemStackable, CargoTransportStone: true, CargoTransportStoneType: 3},
		1001: {ItemID: 1001, Kind: dungeonDropItemEquipment, EquipmentType: "[weapon]", MinimumLevel: 85, Rarity: 4},
	}}
	config := currentCargoTransportConfig{Rules: map[int64]currentCargoTransportRule{
		3: {StoneType: 3, GradeWeights: []currentPVFRangeWeight{{Minimum: 1, Maximum: 90, Weight: 1}}, RarityWeights: map[int64]int64{4: 1}, EquipmentTypes: map[string]bool{"weapon": true}, ExcludeItemIDs: map[uint32]bool{}, IncludeItemIDs: map[uint32]bool{}},
	}}
	service := &Service{}
	if err := service.commitCurrentStoneToAccountCargo(ctx, repositories, catalog, config, "account-1", "1", currentCargoTransportRequest{StoneSlot: 70, TargetSlot: 12}); err != nil {
		t.Fatal(err)
	}
	inventory, _, _ := repositories.Inventory.Load(ctx, "1")
	if inventory.Slots["0:70"].Count != 1 {
		t.Fatalf("stone=%+v", inventory.Slots["0:70"])
	}
	if _, found := inventory.Slots["0:12"]; found {
		t.Fatal("target remained in character inventory")
	}
	cargo, found, err := repositories.AccountInventory.Load(ctx, "account-1")
	if err != nil || !found || cargo.Slots["12:0"].ItemID != 1001 {
		t.Fatalf("cargo found=%t err=%v slots=%+v", found, err, cargo.Slots)
	}
}

func TestCurrentGrandiTransportConsumesExactPVFCostsAtomically(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	for _, row := range []dnfrepo.CharacterRecord{
		{CharacterID: "1", AccountID: "account-1", Slot: 0},
		{CharacterID: "2", AccountID: "account-1", Slot: 4},
	} {
		if err := repositories.Character.Save(ctx, row); err != nil {
			t.Fatal(err)
		}
	}
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{CharacterID: "1", Slots: map[string]dnfrepo.ItemStack{
		"0:12": {ItemID: 100240061, Count: 1},
		"0:70": {ItemID: 3167, Count: 500},
	}}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{CharacterID: "2", Slots: map[string]dnfrepo.ItemStack{}}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.AccountInventory.Save(ctx, dnfrepo.AccountInventoryRecord{AccountID: "account-1", Slots: map[string]dnfrepo.ItemStack{
		"0:364": {ItemID: 10158124, Count: 200},
	}}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.EpicBook.Save(ctx, dnfrepo.EpicBookRecord{AccountID: "account-1", Balances: map[uint32]uint32{100240061: 200}}); err != nil {
		t.Fatal(err)
	}
	catalog := &pvfDungeonDropCatalog{source: finishBridgePVFSource{}, itemCache: map[uint32]dungeonDropItemDefinition{
		100240061: {ItemID: 100240061, Kind: dungeonDropItemEquipment, EquipmentType: "[coat]", MinimumLevel: 85, Rarity: 4},
	}}
	rule := currentCargoTransportRule{
		StoneType: 7, GradeWeights: []currentPVFRangeWeight{{Minimum: 85, Maximum: 85, Weight: 5}},
		RarityWeights: map[int64]int64{4: 9}, EquipmentTypes: map[string]bool{"coat": true}, IncludeItemIDs: map[uint32]bool{}, ExcludeItemIDs: map[uint32]bool{},
		SoulItemIDs: map[int64]uint32{4: 10158124}, SoulWeights: map[int64][]int64{4: {3, 3, 3, 3, 3}},
		ElementItemID: 3167, ElementWeights: map[int64][]int64{4: {10, 10, 10, 10}}, EpicPieceWeight: []int64{3, 3, 3, 3, 3},
	}
	service := &Service{}
	// RecipientSelect is the roster index, not the slot: character "2" sits
	// in slot 4 but is the second row of the account roster, so the client
	// sends 1. This is exactly the gap the slot reading used to get wrong.
	if err := service.commitCurrentGrandiEquipmentTransport(ctx, repositories, catalog, currentCargoTransportConfig{Rules: map[int64]currentCargoTransportRule{7: rule}}, "account-1", "1", currentCargoTransportRequest{TargetSlot: 12, Mode: 2, RecipientSelect: 1}); err != nil {
		t.Fatal(err)
	}
	source, _, _ := repositories.Inventory.Load(ctx, "1")
	destination, _, _ := repositories.Inventory.Load(ctx, "2")
	account, _, _ := repositories.AccountInventory.Load(ctx, "account-1")
	book, _, _ := repositories.EpicBook.Load(ctx, "account-1")
	if _, found := source.Slots["0:12"]; found {
		t.Fatalf("transferred equipment stayed in the sender bag: %+v", source.Slots)
	}
	// The recipient must be reached by mail, never straight into the bag.
	if len(destination.Slots) != 0 {
		t.Fatalf("equipment went into the recipient backpack: %+v", destination.Slots)
	}
	mailbox, found, err := repositories.Mailbox.Load(ctx, "2")
	if err != nil || !found || len(mailbox.Mails) != 1 {
		t.Fatalf("recipient mailbox found=%t err=%v mails=%d", found, err, len(mailbox.Mails))
	}
	for _, mail := range mailbox.Mails {
		if len(mail.Attachments) != 1 || mail.Attachments[0].ItemID != 100240061 || mail.Attachments[0].Count != 1 {
			t.Fatalf("mail attachments=%+v", mail.Attachments)
		}
		if mail.Attachments[0].Extra["source"] != "grandi_equipment_transport" {
			t.Fatalf("mail attachment extra=%+v", mail.Attachments[0].Extra)
		}
	}
	if source.Slots["0:70"].Count != 50 || account.Slots["0:364"].Count != 65 || book.Balances[100240061] != 65 {
		t.Fatalf("element=%d soul=%d fragments=%d", source.Slots["0:70"].Count, account.Slots["0:364"].Count, book.Balances[100240061])
	}
}

// A rejection must reach the client as one of the seven codes the op557
// handler sub_1E785B0 recognises. An unlisted code leaves its dstr register
// zeroed and the client pops dstr 0 -- in this build the unrelated
// "已超出机器人放置的个数上限".
func TestCurrentCargoTransportFailureCodesAreClientRecognised(t *testing.T) {
	// code -> the dstr text docs/client-dstr-zh.txt gives for it. Every code
	// this server can send must be in here; anything else renders dstr 0.
	recognised := map[byte]string{
		0x04: "帐号金库空间不足",
		0x07: "无法转移该道具",
		0x11: "该道具不存在",
		0x13: "帐号金库未激活",
		0x15: "无法使用。",
		0x16: "所需材料不足",
		0xD6: "在物品已锁定状态下",
	}
	for _, code := range []byte{
		currentCargoTransportErrorNotTransferable,
		currentCargoTransportErrorItemMissing,
		currentCargoTransportErrorCargoInactive,
		currentCargoTransportErrorCargoFull,
		currentCargoTransportErrorMaterial,
	} {
		if _, ok := recognised[code]; !ok {
			t.Fatalf("code %#x is not in the current-EXE op557 table", code)
		}
	}
	// The material message is the one the player sees most, so pin the code
	// that carries it: 0x16, not ServerS4A21's "source missing" reading.
	if currentCargoTransportErrorMaterial != 0x16 {
		t.Fatalf("material code=%#x want 0x16 (所需材料不足)", currentCargoTransportErrorMaterial)
	}
	for _, testCase := range []struct {
		name string
		err  error
		want byte
	}{
		{name: "target missing", err: currentCargoTransportReject(currentCargoTransportErrorItemMissing, "x"), want: currentCargoTransportErrorItemMissing},
		{name: "material insufficient", err: currentCargoTransportReject(currentCargoTransportErrorMaterial, "x"), want: currentCargoTransportErrorMaterial},
		{name: "wrapped", err: fmt.Errorf("commit: %w", currentCargoTransportReject(currentCargoTransportErrorCargoFull, "x")), want: currentCargoTransportErrorCargoFull},
		{name: "uncoded repository fault", err: errors.New("boom"), want: currentCargoTransportErrorNotTransferable},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if got := currentCargoTransportFailureCode(testCase.err); got != testCase.want {
				t.Fatalf("code=%#x want %#x", got, testCase.want)
			}
		})
	}
}

// The success payload is the four u16 plus mode byte sub_1E785B0 reads at
// 0x1E78621. Mode 2 makes it close the 装备跨界 window (UI 0x435) and return
// before touching any item row, which is why the count and parameter are 0.
func TestCurrentCargoTransportSuccessBodyIsFourWordsAndAModeByte(t *testing.T) {
	body := buildCurrentCargoTransportSuccessBody(0, 20, 0, 0, currentCargoTransportGrandiMode)
	if len(body) != 9 {
		t.Fatalf("body=% X", body)
	}
	if binary.LittleEndian.Uint16(body[0:2]) != 0 ||
		binary.LittleEndian.Uint16(body[2:4]) != 20 ||
		binary.LittleEndian.Uint16(body[4:6]) != 0 ||
		binary.LittleEndian.Uint16(body[6:8]) != 0 ||
		body[8] != 2 {
		t.Fatalf("body=% X", body)
	}
	if got := upperSuccessBody(body); got[0] != 1 || len(got) != 10 {
		t.Fatalf("upper body=% X", got)
	}
}

// The dropdown value is a roster position. The recipient must be picked by
// that position over the roster ordered by slot then character id, and the
// sender's own row must never resolve.
func TestCurrentCargoRecipientIsResolvedByRosterIndexNotSlot(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	for _, row := range []dnfrepo.CharacterRecord{
		{CharacterID: "1", AccountID: "account-1", Slot: 0},
		{CharacterID: "9", AccountID: "account-1", Slot: 7},
		{CharacterID: "5", AccountID: "account-1", Slot: 13},
	} {
		if err := repositories.Character.Save(ctx, row); err != nil {
			t.Fatal(err)
		}
	}
	service := &Service{}
	for _, testCase := range []struct {
		selection uint32
		want      string
	}{{selection: 1, want: "9"}, {selection: 2, want: "5"}} {
		picked, err := service.currentCargoRecipientBySelection(ctx, repositories.Character, "account-1", "1", testCase.selection)
		if err != nil || picked.CharacterID != testCase.want {
			t.Fatalf("selection=%d picked=%q err=%v want %q", testCase.selection, picked.CharacterID, err, testCase.want)
		}
	}
	if _, err := service.currentCargoRecipientBySelection(ctx, repositories.Character, "account-1", "1", 0); err == nil {
		t.Fatal("the sender's own roster position must not resolve as a recipient")
	}
	if _, err := service.currentCargoRecipientBySelection(ctx, repositories.Character, "account-1", "1", 7); err == nil {
		t.Fatal("a slot number past the roster length must not resolve")
	}
}

func TestCurrentPetTransportMovesDurableCreatureState(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	for _, row := range []dnfrepo.CharacterRecord{{CharacterID: "1", AccountID: "account-1", Slot: 0}, {CharacterID: "2", AccountID: "account-1", Slot: 1}} {
		if err := repositories.Character.Save(ctx, row); err != nil {
			t.Fatal(err)
		}
	}
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{CharacterID: "1", Slots: map[string]dnfrepo.ItemStack{
		"0:70": {ItemID: 8001, Count: 1}, "7:4": {ItemID: 63000, Count: 1, Extra: map[string]string{"creature_key": "77"}},
	}}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{CharacterID: "2", Slots: map[string]dnfrepo.ItemStack{}}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Pet.Save(ctx, dnfrepo.PetRecord{CharacterID: "1", Entries: map[string]dnfrepo.PetEntry{
		"77": {PetKey: "77", ItemID: 63000, SourceListType: 7, SourceSlotIndex: 4},
	}}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Pet.Save(ctx, dnfrepo.PetRecord{CharacterID: "2", Entries: map[string]dnfrepo.PetEntry{}}); err != nil {
		t.Fatal(err)
	}
	catalog := &pvfDungeonDropCatalog{source: finishBridgePVFSource{}, itemCache: map[uint32]dungeonDropItemDefinition{
		8001:  {ItemID: 8001, Kind: dungeonDropItemStackable, CreatureTransportStone: true, CreatureTransportStoneType: 1},
		63000: {ItemID: 63000, Kind: dungeonDropItemEquipment, EquipmentType: "[creature]"},
	}}
	service := &Service{}
	if err := service.commitCurrentPetTransport(ctx, repositories, catalog, "account-1", "1", currentCargoTransportRequest{StoneSlot: 70, TargetSlot: 4, Mode: 1, RecipientSelect: 1}); err != nil {
		t.Fatal(err)
	}
	source, _, _ := repositories.Inventory.Load(ctx, "1")
	destination, _, _ := repositories.Inventory.Load(ctx, "2")
	sourcePets, _, _ := repositories.Pet.Load(ctx, "1")
	destinationPets, _, _ := repositories.Pet.Load(ctx, "2")
	if len(source.Slots) != 0 || destination.Slots["7:0"].ItemID != 63000 || len(sourcePets.Entries) != 0 || destinationPets.Entries["77"].SourceSlotIndex != 0 {
		t.Fatalf("source=%+v destination=%+v sourcePets=%+v destinationPets=%+v", source.Slots, destination.Slots, sourcePets, destinationPets)
	}
}

func TestCurrentResealCostAndWaxKinds(t *testing.T) {
	config := currentResealConfig{
		GradeWeights:  []currentPVFRangeWeight{{Minimum: 85, Maximum: 85, Weight: 5}},
		RarityWeights: map[int64]float64{4: 9}, ExtendedWeights: map[int64]float64{4: 1}, SoulItemIDs: map[int64]uint32{4: 10158124},
		SoulCostWeights: map[int64]float64{4: 3}, ResealConstants: map[int64]float64{4: 1.5}, ResealCountLimits: []currentPVFRangeWeight{{Minimum: 1, Maximum: 90, Weight: 7}},
	}
	definition := dungeonDropItemDefinition{Kind: dungeonDropItemEquipment, MinimumLevel: 85, Rarity: 4}
	if item, cost, ok := config.materialAndCost(definition, 0); !ok || item != 10158124 || cost != 135 {
		t.Fatalf("soul item=%d cost=%d ok=%t", item, cost, ok)
	}
	if cost, ok := config.waxCost(definition, 0); !ok || cost != 45 {
		t.Fatalf("wax cost=%d ok=%t", cost, ok)
	}
	for _, test := range []struct {
		definition dungeonDropItemDefinition
		oneShot    bool
		valid      bool
	}{
		{dungeonDropItemDefinition{PVFPath: "stackable/cash/candle_goldwax.stk"}, false, true},
		{dungeonDropItemDefinition{PVFPath: "stackable/cash/candle_goldwax_oneshot.stk"}, true, true},
		{dungeonDropItemDefinition{PVFPath: "stackable/event/chn_candle_goldwax_oneshot_490000345.stk"}, true, true},
		{dungeonDropItemDefinition{PVFPath: "stackable/other.stk"}, false, false},
	} {
		oneShot, valid := currentPVFWaxKind(test.definition)
		if oneShot != test.oneShot || valid != test.valid {
			t.Fatalf("definition=%+v oneShot=%t valid=%t", test.definition, oneShot, valid)
		}
	}
}

func TestCargoTransportAndPetSealModulesAreRegistered(t *testing.T) {
	for _, opcode := range []uint16{uint16(dnfenum.CmdPacketCargoTransportItem), uint16(dnfenum.CmdPacketSealCreature)} {
		if _, found := currentGameplayModules.legacyHandlers[opcode]; !found {
			t.Fatalf("legacy opcode=%d missing", opcode)
		}
		if _, found := currentGameplayModules.upperHandlers[opcode]; !found {
			t.Fatalf("upper opcode=%d missing", opcode)
		}
	}
}

func TestRealPVFCargoTransportAndResealRules(t *testing.T) {
	pvfPath := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE to verify cargo transport and reseal rules")
	}
	archive, err := platformpvf.Open(pvfPath)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := newPVFDungeonDropCatalog(archive)
	if err != nil {
		t.Fatal(err)
	}
	transport, err := loadCurrentCargoTransportConfig(catalog)
	if err != nil {
		t.Fatal(err)
	}
	if _, found := transport.Rules[7]; !found {
		t.Fatal("runtime PVF has no Grandi stone type 7")
	}
	for _, direct := range []struct {
		stoneID   uint32
		stoneType int64
		targetID  uint32
	}{
		{stoneID: 10304349, stoneType: 8, targetID: 100140069},
		{stoneID: 490002430, stoneType: 4016, targetID: 100060476},
	} {
		stone, resolveErr := catalog.ResolveItem(direct.stoneID)
		if resolveErr != nil {
			t.Fatal(resolveErr)
		}
		if !stone.CargoTransportStone || stone.CargoTransportStoneType != direct.stoneType {
			t.Fatalf("runtime direct cargo stone=%d not parsed: %+v", direct.stoneID, stone)
		}
		directRule, ruleFound := transport.Rules[stone.CargoTransportStoneType]
		if !ruleFound {
			t.Fatalf("runtime direct cargo stone=%d type=%d has no rule", direct.stoneID, stone.CargoTransportStoneType)
		}
		directTarget, resolveErr := catalog.ResolveItem(direct.targetID)
		if resolveErr != nil {
			t.Fatal(resolveErr)
		}
		if !currentCargoRuleAllows(directRule, directTarget, dnfrepo.ItemStack{ItemID: int64(directTarget.ItemID), Count: 1}) {
			t.Fatalf("runtime screenshot cargo target rejected stone=%+v target=%+v rule=%+v", stone, directTarget, directRule)
		}
	}
	creatureStone, err := catalog.ResolveItem(490700208)
	if err != nil {
		t.Fatal(err)
	}
	if !creatureStone.CreatureTransportStone || creatureStone.CreatureTransportStoneType != 16 {
		t.Fatalf("runtime creature transport stone not parsed: %+v", creatureStone)
	}
	rule := transport.Rules[7]
	target, err := catalog.ResolveItem(100240061)
	if err != nil {
		t.Fatal(err)
	}
	category := currentCargoEquipmentCategory(target.EquipmentType)
	base := rangeWeightFor(rule.GradeWeights, target.MinimumLevel) * rule.RarityWeights[target.Rarity]
	if len(rule.SoulWeights[target.Rarity]) <= category || len(rule.ElementWeights[target.Rarity]) <= category || len(rule.EpicPieceWeight) <= category {
		t.Fatalf("runtime Grandi cost vectors missing target=%+v category=%d base=%d rule=%+v", target, category, base, rule)
	}
	if category != 1 || base != 45 || rule.SoulItemIDs[target.Rarity] != 10158124 || rule.SoulWeights[target.Rarity][category]*base != 135 || rule.ElementWeights[target.Rarity][category]*base != 450 || rule.EpicPieceWeight[category]*base != 135 {
		t.Fatalf("runtime Grandi rule target=%+v category=%d base=%d rule=%+v", target, category, base, rule)
	}
	reseal, err := loadCurrentResealConfig(catalog)
	if err != nil {
		t.Fatal(err)
	}
	resealable := dungeonDropItemDefinition{Kind: dungeonDropItemEquipment, MinimumLevel: 85, Rarity: 3}
	if item, cost, ok := reseal.materialAndCost(resealable, 0); !ok || item != 10099774 || cost != 80 {
		t.Fatalf("runtime Grandi reseal item=%d cost=%d ok=%t config=%+v", item, cost, ok, reseal)
	}
	if cost, ok := reseal.waxCost(resealable, 0); !ok || cost != 8 {
		t.Fatalf("runtime golden wax cost=%d ok=%t", cost, ok)
	}
	for _, itemID := range []uint32{14, 2660238, 10098656, 490000345, 10157922, 10164812} {
		definition, resolveErr := catalog.ResolveItem(itemID)
		if resolveErr != nil {
			t.Fatal(resolveErr)
		}
		oneShot, valid := currentPVFWaxKind(definition)
		if !valid || (itemID == 14 && oneShot) || (itemID != 14 && !oneShot) {
			t.Fatalf("runtime wax item=%d one_shot=%t valid=%t definition=%+v", itemID, oneShot, valid, definition)
		}
	}
}

func mustSaveCargoTransportAccount(t *testing.T, ctx context.Context, repositories dnfrepo.Group, accountID string, capacity int) {
	t.Helper()
	if err := repositories.Account.Save(ctx, dnfrepo.AccountRecord{AccountID: accountID, Metadata: map[string]string{
		"account_cargo_created": "true", "account_cargo_level": strconv.Itoa(capacity),
	}}); err != nil {
		t.Fatal(err)
	}
}
