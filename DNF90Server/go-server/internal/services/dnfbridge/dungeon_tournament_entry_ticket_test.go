package dnfbridge

import (
	"context"
	"encoding/binary"
	"errors"
	"os"
	"strconv"
	"strings"
	"testing"

	dnfdungeonadmission "longheng.io/server/internal/modules/dnf/dungeonadmission"
	"longheng.io/server/internal/modules/dnf/dungeoncmd"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	"longheng.io/server/internal/modules/dnf/worldmap"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

func TestCurrentDungeonEntryConsumesRequiredMainInventoryItemAndCanRestore(t *testing.T) {
	service, session, repositories, _ := newPremiumServiceTestRuntime(t)
	if err := repositories.Inventory.Save(context.Background(), dnfrepo.InventoryRecord{
		CharacterID: "19",
		Slots: map[string]dnfrepo.ItemStack{
			"0:4": {ItemID: 4726, Count: 2},
			"0:8": {ItemID: 9001, Count: 1},
		},
	}); err != nil {
		t.Fatal(err)
	}
	runtime := &runtimeDungeonState{Dungeon: worldmap.Dungeon{ID: 120, Metadata: worldmap.DungeonMetadata{
		RequiredItems: []worldmap.TicketItem{{ItemID: 4726, Count: 1}},
	}}}
	updates, restore, err := service.consumeCurrentDungeonEntryItems(context.Background(), session, runtime)
	if err != nil {
		t.Fatal(err)
	}
	if len(updates) != 1 || restore == nil {
		t.Fatalf("updates=%+v restore=%v", updates, restore != nil)
	}
	after, found, err := repositories.Inventory.Load(context.Background(), "19")
	if err != nil || !found || after.Slots["0:4"].Count != 1 {
		t.Fatalf("after consume=%+v found=%t err=%v", after, found, err)
	}
	if err := restore(); err != nil {
		t.Fatal(err)
	}
	restored, found, err := repositories.Inventory.Load(context.Background(), "19")
	if err != nil || !found || restored.Slots["0:4"].Count != 2 {
		t.Fatalf("after restore=%+v found=%t err=%v", restored, found, err)
	}
}

func TestCurrentTowerOfDespairEntryAcceptsEveryDirectInvitationAlternative(t *testing.T) {
	service, session, repositories, _ := newPremiumServiceTestRuntime(t)
	catalog, err := dnfdungeonadmission.LoadCatalog(dungeonAdmissionBridgeCatalogSource())
	if err != nil {
		t.Fatal(err)
	}
	service.dungeonAdmissionCatalog = catalog
	runtime := &runtimeDungeonState{Dungeon: worldmap.Dungeon{ID: 11008, Metadata: worldmap.DungeonMetadata{
		RequiredItems: []worldmap.TicketItem{{ItemID: 4473, Count: 1}},
	}}}
	for _, definition := range currentTowerOfDespairEntryItemDefinitions {
		t.Run(strconv.FormatInt(definition.ItemID, 10), func(t *testing.T) {
			if err := repositories.Inventory.Save(context.Background(), dnfrepo.InventoryRecord{
				CharacterID: "19",
				Slots: map[string]dnfrepo.ItemStack{
					"0:140": {ItemID: definition.ItemID, Count: 2},
				},
			}); err != nil {
				t.Fatal(err)
			}
			updates, _, err := service.consumeCurrentDungeonEntryItems(context.Background(), session, runtime)
			if err != nil {
				t.Fatal(err)
			}
			if len(updates) != 1 || admissionUpdateSlot(updates[0]) != 140 || admissionUpdateItemID(updates[0]) != uint32(definition.ItemID) || admissionUpdateCount(updates[0]) != 1 {
				t.Fatalf("item=%d updates=%+v", definition.ItemID, updates)
			}
			inventory := loadAdmissionInventory(t, repositories, "19")
			if stack := inventory.Slots["0:140"]; stack.ItemID != definition.ItemID || stack.Count != 1 {
				t.Fatalf("item=%d inventory=%+v", definition.ItemID, inventory.Slots)
			}
		})
	}
}

func TestCurrentTowerEntryConsumesOnlyTheMatchedFamilyInvitation(t *testing.T) {
	service, session, repositories, _ := newPremiumServiceTestRuntime(t)
	catalog, err := dnfdungeonadmission.LoadCatalog(dungeonAdmissionBridgeCatalogSource())
	if err != nil {
		t.Fatal(err)
	}
	service.dungeonAdmissionCatalog = catalog
	if err := repositories.Inventory.Save(context.Background(), dnfrepo.InventoryRecord{
		CharacterID: "19",
		Slots: map[string]dnfrepo.ItemStack{
			"0:141": {ItemID: 10161065, Count: 2},
			"0:142": {ItemID: 490004952, Count: 2},
		},
	}); err != nil {
		t.Fatal(err)
	}
	runtime := &runtimeDungeonState{Dungeon: worldmap.Dungeon{ID: 5115, Metadata: worldmap.DungeonMetadata{
		RequiredItems: []worldmap.TicketItem{{ItemID: 10161065, Count: 1}},
	}}}
	updates, _, err := service.consumeCurrentDungeonEntryItems(context.Background(), session, runtime)
	if err != nil {
		t.Fatal(err)
	}
	if len(updates) != 1 || admissionUpdateSlot(updates[0]) != 141 || admissionUpdateItemID(updates[0]) != 10161065 || admissionUpdateCount(updates[0]) != 1 {
		t.Fatalf("grief updates=%+v", updates)
	}
	inventory := loadAdmissionInventory(t, repositories, "19")
	if inventory.Slots["0:141"].Count != 1 || inventory.Slots["0:142"].Count != 2 {
		t.Fatalf("tower families were mixed: %+v", inventory.Slots)
	}
}

func TestCurrentTowerOfDespairEntryPrefersCanonicalInvitation(t *testing.T) {
	service, session, repositories, _ := newPremiumServiceTestRuntime(t)
	catalog, err := dnfdungeonadmission.LoadCatalog(dungeonAdmissionBridgeCatalogSource())
	if err != nil {
		t.Fatal(err)
	}
	service.dungeonAdmissionCatalog = catalog
	if err := repositories.Inventory.Save(context.Background(), dnfrepo.InventoryRecord{
		CharacterID: "19",
		Slots: map[string]dnfrepo.ItemStack{
			"0:141": {ItemID: 490004952, Count: 2},
			"0:142": {ItemID: 4473, Count: 2},
		},
	}); err != nil {
		t.Fatal(err)
	}
	runtime := &runtimeDungeonState{Dungeon: worldmap.Dungeon{ID: 11008, Metadata: worldmap.DungeonMetadata{
		RequiredItems: []worldmap.TicketItem{{ItemID: 4473, Count: 1}},
	}}}
	updates, _, err := service.consumeCurrentDungeonEntryItems(context.Background(), session, runtime)
	if err != nil {
		t.Fatal(err)
	}
	if len(updates) != 1 || admissionUpdateSlot(updates[0]) != 142 || admissionUpdateItemID(updates[0]) != 4473 || admissionUpdateCount(updates[0]) != 1 {
		t.Fatalf("canonical priority updates=%+v", updates)
	}
	inventory := loadAdmissionInventory(t, repositories, "19")
	if inventory.Slots["0:141"].Count != 2 || inventory.Slots["0:142"].Count != 1 {
		t.Fatalf("canonical priority inventory=%+v", inventory.Slots)
	}
}

func TestCurrentTowerEntryDoesNotConsumeBoxesOrAllowanceItems(t *testing.T) {
	service, session, repositories, _ := newPremiumServiceTestRuntime(t)
	catalog, err := dnfdungeonadmission.LoadCatalog(dungeonAdmissionBridgeCatalogSource())
	if err != nil {
		t.Fatal(err)
	}
	service.dungeonAdmissionCatalog = catalog
	for _, test := range []struct {
		name      string
		dungeonID int64
		canonical int64
		itemID    int64
	}{
		{name: "despair booster", dungeonID: 11008, canonical: 4473, itemID: 10002584},
		{name: "despair allowance", dungeonID: 11008, canonical: 4473, itemID: 490002629},
		{name: "despair action", dungeonID: 11008, canonical: 4473, itemID: 10005999},
		{name: "grief booster", dungeonID: 5115, canonical: 10161065, itemID: 10161084},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := repositories.Inventory.Save(context.Background(), dnfrepo.InventoryRecord{
				CharacterID: "19",
				Slots: map[string]dnfrepo.ItemStack{
					"0:143": {ItemID: test.itemID, Count: 1},
				},
			}); err != nil {
				t.Fatal(err)
			}
			runtime := &runtimeDungeonState{Dungeon: worldmap.Dungeon{ID: test.dungeonID, Metadata: worldmap.DungeonMetadata{
				RequiredItems: []worldmap.TicketItem{{ItemID: test.canonical, Count: 1}},
			}}}
			if _, _, err := service.consumeCurrentDungeonEntryItems(context.Background(), session, runtime); !errors.Is(err, errDungeonEntryItemMissing) {
				t.Fatalf("item=%d err=%v", test.itemID, err)
			}
			inventory := loadAdmissionInventory(t, repositories, "19")
			if stack := inventory.Slots["0:143"]; stack.ItemID != test.itemID || stack.Count != 1 {
				t.Fatalf("item=%d was consumed: %+v", test.itemID, inventory.Slots)
			}
		})
	}
}

func TestCurrentTowerEntryInvitationDefinitionsMatchRealPVF(t *testing.T) {
	pvfPath := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE to verify tower entry invitation alternatives")
	}
	archive, err := platformpvf.Open(pvfPath)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := newPVFDungeonDropCatalog(archive)
	if err != nil {
		t.Fatal(err)
	}
	for family, definitions := range map[string][]currentTowerEntryItemDefinition{
		"绝望之邀请函": currentTowerOfDespairEntryItemDefinitions,
		"悲叹之邀请函": currentTowerOfGriefEntryItemDefinitions,
	} {
		for _, expected := range definitions {
			definition, err := catalog.ResolveItem(uint32(expected.ItemID))
			if err != nil {
				t.Fatalf("%s item=%d: %v", family, expected.ItemID, err)
			}
			if !strings.EqualFold(definition.PVFPath, expected.PVFPath) ||
				!strings.Contains(definition.Name, family) ||
				!strings.EqualFold(normalizeDungeonDropStackableType(definition.StackableType), expected.StackableType) ||
				strings.TrimSpace(definition.ActionType) != "" {
				t.Fatalf("%s expected=%+v definition=%+v", family, expected, definition)
			}
		}
	}
}

func TestCurrentDungeonEntryRestorePreservesUnrelatedConcurrentSlot(t *testing.T) {
	service, session, repositories, _ := newPremiumServiceTestRuntime(t)
	if err := repositories.Inventory.Save(context.Background(), dnfrepo.InventoryRecord{
		CharacterID: "19",
		Slots: map[string]dnfrepo.ItemStack{
			"0:4": {ItemID: 4726, Count: 2},
		},
	}); err != nil {
		t.Fatal(err)
	}
	runtime := &runtimeDungeonState{Dungeon: worldmap.Dungeon{ID: 120, Metadata: worldmap.DungeonMetadata{
		RequiredItems: []worldmap.TicketItem{{ItemID: 4726, Count: 1}},
	}}}
	_, restore, err := service.consumeCurrentDungeonEntryItems(context.Background(), session, runtime)
	if err != nil || restore == nil {
		t.Fatalf("consume restore=%v err=%v", restore != nil, err)
	}
	concurrent := loadAdmissionInventory(t, repositories, "19")
	concurrent.Slots["0:9"] = dnfrepo.ItemStack{ItemID: 9009, Count: 7}
	if err := repositories.Inventory.Save(context.Background(), concurrent); err != nil {
		t.Fatal(err)
	}
	if err := restore(); err != nil {
		t.Fatal(err)
	}
	restored := loadAdmissionInventory(t, repositories, "19")
	if stack := restored.Slots["0:4"]; stack.ItemID != 4726 || stack.Count != 2 {
		t.Fatalf("admission slot not restored: %+v", restored.Slots)
	}
	if stack := restored.Slots["0:9"]; stack.ItemID != 9009 || stack.Count != 7 {
		t.Fatalf("unrelated concurrent slot was overwritten: %+v", restored.Slots)
	}
}

func TestCurrentDungeonEntryRestoreConflictDoesNotPartiallyRestoreSlots(t *testing.T) {
	service, session, repositories, _ := newPremiumServiceTestRuntime(t)
	if err := repositories.Inventory.Save(context.Background(), dnfrepo.InventoryRecord{
		CharacterID: "19",
		Slots: map[string]dnfrepo.ItemStack{
			"0:4": {ItemID: 4726, Count: 1},
			"0:5": {ItemID: 4727, Count: 1},
		},
	}); err != nil {
		t.Fatal(err)
	}
	runtime := &runtimeDungeonState{Dungeon: worldmap.Dungeon{ID: 120, Metadata: worldmap.DungeonMetadata{
		RequiredItems: []worldmap.TicketItem{{ItemID: 4726, Count: 1}, {ItemID: 4727, Count: 1}},
	}}}
	_, restore, err := service.consumeCurrentDungeonEntryItems(context.Background(), session, runtime)
	if err != nil || restore == nil {
		t.Fatalf("consume restore=%v err=%v", restore != nil, err)
	}
	concurrent := loadAdmissionInventory(t, repositories, "19")
	if concurrent.Slots == nil {
		concurrent.Slots = make(map[string]dnfrepo.ItemStack)
	}
	concurrent.Slots["0:4"] = dnfrepo.ItemStack{ItemID: 9004, Count: 3}
	if err := repositories.Inventory.Save(context.Background(), concurrent); err != nil {
		t.Fatal(err)
	}
	if err := restore(); !errors.Is(err, errDungeonEntryItemRestoreConflict) {
		t.Fatalf("restore conflict err=%v", err)
	}
	after := loadAdmissionInventory(t, repositories, "19")
	if stack := after.Slots["0:4"]; stack.ItemID != 9004 || stack.Count != 3 {
		t.Fatalf("conflicting slot was overwritten: %+v", after.Slots)
	}
	if _, found := after.Slots["0:5"]; found {
		t.Fatalf("nonconflicting admission slot was partially restored: %+v", after.Slots)
	}
}

func TestCurrentDungeonEntryPrefersParticularHellFreePassFromDynamicSlot(t *testing.T) {
	service, session, repositories, _ := newPremiumServiceTestRuntime(t)
	service.worldMapTable = currentDungeonEntryAdmissionTestWorldMap(t, `
[dungeon]
316 -1
[/dungeon]
[particularhell freepass item]
490005479 1 490005974 1
[/particularhell freepass item]
`)
	if err := repositories.Inventory.Save(context.Background(), dnfrepo.InventoryRecord{
		CharacterID: "19",
		Slots: map[string]dnfrepo.ItemStack{
			"0:4":   {ItemID: 10158964, Count: 2},
			"0:177": {ItemID: 490005479, Count: 2},
		},
	}); err != nil {
		t.Fatal(err)
	}
	runtime := &runtimeDungeonState{Dungeon: worldmap.Dungeon{ID: 316, Metadata: worldmap.DungeonMetadata{
		DungeonType:   "[particular hell]",
		RequiredItems: []worldmap.TicketItem{{ItemID: 10158964, Count: 2}},
	}}}
	updates, _, err := service.consumeCurrentDungeonEntryItems(context.Background(), session, runtime)
	if err != nil {
		t.Fatal(err)
	}
	if len(updates) != 1 || admissionUpdateSlot(updates[0]) != 177 || admissionUpdateItemID(updates[0]) != 490005479 || admissionUpdateCount(updates[0]) != 1 {
		t.Fatalf("freepass updates=%+v", updates)
	}
	after, found, err := repositories.Inventory.Load(context.Background(), "19")
	if err != nil || !found || after.Slots["0:177"].Count != 1 || after.Slots["0:4"].Count != 2 {
		t.Fatalf("after freepass consume=%+v found=%t err=%v", after, found, err)
	}
}

func TestCurrentDungeonEntryFallsBackToParticularHellRequiredItems(t *testing.T) {
	service, session, repositories, _ := newPremiumServiceTestRuntime(t)
	service.worldMapTable = currentDungeonEntryAdmissionTestWorldMap(t, `
[dungeon]
316 -1
[/dungeon]
[particularhell freepass item]
490005479 1
[/particularhell freepass item]
`)
	if err := repositories.Inventory.Save(context.Background(), dnfrepo.InventoryRecord{
		CharacterID: "19",
		Slots: map[string]dnfrepo.ItemStack{
			"0:4": {ItemID: 10158964, Count: 3},
		},
	}); err != nil {
		t.Fatal(err)
	}
	runtime := &runtimeDungeonState{Dungeon: worldmap.Dungeon{ID: 316, Metadata: worldmap.DungeonMetadata{
		DungeonType:   "[particular hell]",
		RequiredItems: []worldmap.TicketItem{{ItemID: 10158964, Count: 2}},
	}}}
	updates, _, err := service.consumeCurrentDungeonEntryItems(context.Background(), session, runtime)
	if err != nil {
		t.Fatal(err)
	}
	if len(updates) != 1 || admissionUpdateSlot(updates[0]) != 4 || admissionUpdateCount(updates[0]) != 1 {
		t.Fatalf("required-item updates=%+v", updates)
	}
	after, found, err := repositories.Inventory.Load(context.Background(), "19")
	if err != nil || !found || after.Slots["0:4"].Count != 1 {
		t.Fatalf("after required consume=%+v found=%t err=%v", after, found, err)
	}
}

func TestCurrentDungeonEntryUsesInfernoFreePassBeforePVFItemCondition(t *testing.T) {
	service, session, repositories, _ := newPremiumServiceTestRuntime(t)
	service.worldMapTable = currentDungeonEntryAdmissionTestWorldMap(t, `
[dungeon]
7539 -1
[/dungeon]
[item condition]
3330
[/item condition]
[inferno freepass item]
490701656 1 490006449 1
[/inferno freepass item]
`)
	if err := repositories.Inventory.Save(context.Background(), dnfrepo.InventoryRecord{
		CharacterID: "19",
		Slots: map[string]dnfrepo.ItemStack{
			"0:9":  {ItemID: 3330, Count: 40},
			"0:88": {ItemID: 490006449, Count: 1},
		},
	}); err != nil {
		t.Fatal(err)
	}
	runtime := &runtimeDungeonState{Dungeon: worldmap.Dungeon{ID: 7539, Metadata: worldmap.DungeonMetadata{
		InfernoDungeon:             true,
		HellPartyRequiredItemCount: worldmap.OptionalInt{Value: 30, Set: true},
	}}}
	updates, _, err := service.consumeCurrentDungeonEntryItems(context.Background(), session, runtime)
	if err != nil {
		t.Fatal(err)
	}
	if len(updates) != 1 || admissionUpdateSlot(updates[0]) != 88 || admissionUpdateCount(updates[0]) != 0 {
		t.Fatalf("inferno freepass updates=%+v", updates)
	}
	after, found, err := repositories.Inventory.Load(context.Background(), "19")
	if err != nil || !found || after.Slots["0:9"].Count != 40 {
		t.Fatalf("after inferno pass consume=%+v found=%t err=%v", after, found, err)
	}
	if _, passFound := after.Slots["0:88"]; passFound {
		t.Fatalf("consumed inferno pass remained: %+v", after.Slots["0:88"])
	}
}

func TestCurrentDungeonEntryFallsBackToHellItemConditionCount(t *testing.T) {
	service, session, repositories, _ := newPremiumServiceTestRuntime(t)
	service.worldMapTable = currentDungeonEntryAdmissionTestWorldMap(t, `
[dungeon]
310 -1
[/dungeon]
[hell dungeon]
1
[item condition]
3330
[/item condition]
[hell freepass item]
490006398 1
[/hell freepass item]
`)
	if err := repositories.Inventory.Save(context.Background(), dnfrepo.InventoryRecord{
		CharacterID: "19",
		Slots: map[string]dnfrepo.ItemStack{
			"0:9": {ItemID: 3330, Count: 40},
		},
	}); err != nil {
		t.Fatal(err)
	}
	runtime := &runtimeDungeonState{
		Request: dungeoncmd.SelectDungeonRequest{SelectionMode: 1, RuntimeToken: 0xffff},
		Dungeon: worldmap.Dungeon{ID: 310, Metadata: worldmap.DungeonMetadata{
			HellPartyRequiredItemCount: worldmap.OptionalInt{Value: 28, Set: true},
		}},
	}
	updates, _, err := service.consumeCurrentDungeonEntryItems(context.Background(), session, runtime)
	if err != nil {
		t.Fatal(err)
	}
	if len(updates) != 1 || admissionUpdateSlot(updates[0]) != 9 || admissionUpdateCount(updates[0]) != 12 {
		t.Fatalf("hell material updates=%+v", updates)
	}
	after, found, err := repositories.Inventory.Load(context.Background(), "19")
	if err != nil || !found || after.Slots["0:9"].Count != 12 {
		t.Fatalf("after hell material consume=%+v found=%t err=%v", after, found, err)
	}
}

func TestCurrentDungeonEntryRejectsMissingRequiredMainInventoryItem(t *testing.T) {
	service, session, _, _ := newPremiumServiceTestRuntime(t)
	runtime := &runtimeDungeonState{
		Dungeon: worldmap.Dungeon{ID: 120, Metadata: worldmap.DungeonMetadata{
			RequiredItems: []worldmap.TicketItem{{ItemID: 4726, Count: 1}},
		}},
	}
	if _, _, err := service.consumeCurrentDungeonEntryItems(context.Background(), session, runtime); err == nil {
		t.Fatal("missing ticket was accepted")
	}
}

func TestCurrentDungeonEntryMissingRequirementDoesNotPartiallyConsume(t *testing.T) {
	service, session, repositories, _ := newPremiumServiceTestRuntime(t)
	if err := repositories.Inventory.Save(context.Background(), dnfrepo.InventoryRecord{
		CharacterID: "19",
		Slots: map[string]dnfrepo.ItemStack{
			"0:177": {ItemID: 4726, Count: 2},
		},
	}); err != nil {
		t.Fatal(err)
	}
	runtime := &runtimeDungeonState{Dungeon: worldmap.Dungeon{ID: 120, Metadata: worldmap.DungeonMetadata{
		RequiredItems: []worldmap.TicketItem{
			{ItemID: 4726, Count: 1},
			{ItemID: 4727, Count: 1},
		},
	}}}
	if _, _, err := service.consumeCurrentDungeonEntryItems(context.Background(), session, runtime); !errors.Is(err, errDungeonEntryItemMissing) {
		t.Fatalf("error=%v", err)
	}
	after, found, err := repositories.Inventory.Load(context.Background(), "19")
	if err != nil || !found {
		t.Fatalf("load after rejection: found=%t err=%v", found, err)
	}
	if stack := after.Slots["0:177"]; stack.ItemID != 4726 || stack.Count != 2 {
		t.Fatalf("first requirement was partially consumed: %+v", stack)
	}
}

func TestCurrentDungeonEntryPartyConsumesEachMembersOwnAdmissionChoice(t *testing.T) {
	service, leader, repositories, _ := newPremiumServiceTestRuntime(t)
	follower := &gameSession{selectedCharacterID: 20}
	service.worldMapTable = currentDungeonEntryAdmissionTestWorldMap(t, `
[dungeon]
316 -1
[/dungeon]
[particularhell freepass item]
490005479 1
[/particularhell freepass item]
`)
	for characterID, slots := range map[string]map[string]dnfrepo.ItemStack{
		"19": {
			"0:177": {ItemID: 490005479, Count: 1},
			"0:4":   {ItemID: 10158964, Count: 2},
		},
		"20": {
			"0:178": {ItemID: 10158964, Count: 2},
		},
	} {
		if err := repositories.Inventory.Save(context.Background(), dnfrepo.InventoryRecord{
			CharacterID: characterID,
			Slots:       slots,
		}); err != nil {
			t.Fatal(err)
		}
	}
	newRuntime := func() *runtimeDungeonState {
		return &runtimeDungeonState{Dungeon: worldmap.Dungeon{ID: 316, Metadata: worldmap.DungeonMetadata{
			DungeonType:   "[particular hell]",
			RequiredItems: []worldmap.TicketItem{{ItemID: 10158964, Count: 2}},
		}}}
	}
	prepared := []*runtimePartyPreparedDungeonEntry{{session: follower, runtime: newRuntime()}}
	batch, err := service.consumeCurrentDungeonEntryPartyItems(
		context.Background(), leader, newRuntime(), prepared)
	if err != nil {
		t.Fatal(err)
	}
	leaderUpdates := batch.updatesFor(leader)
	followerUpdates := batch.updatesFor(follower)
	if len(leaderUpdates) != 1 || admissionUpdateSlot(leaderUpdates[0]) != 177 ||
		admissionUpdateItemID(leaderUpdates[0]) != ^uint32(0) || admissionUpdateCount(leaderUpdates[0]) != 0 {
		t.Fatalf("leader freepass updates=%+v", leaderUpdates)
	}
	if len(followerUpdates) != 1 || admissionUpdateSlot(followerUpdates[0]) != 178 ||
		admissionUpdateItemID(followerUpdates[0]) != ^uint32(0) || admissionUpdateCount(followerUpdates[0]) != 0 {
		t.Fatalf("follower material updates=%+v", followerUpdates)
	}
	if len(prepared[0].entryItemUpdates) != 1 ||
		admissionUpdateSlot(prepared[0].entryItemUpdates[0]) != 178 {
		t.Fatalf("follower did not retain its own op14 projection: %+v", prepared[0].entryItemUpdates)
	}
	leaderInventory := loadAdmissionInventory(t, repositories, "19")
	followerInventory := loadAdmissionInventory(t, repositories, "20")
	if _, found := leaderInventory.Slots["0:177"]; found || leaderInventory.Slots["0:4"].Count != 2 {
		t.Fatalf("leader admission choice=%+v", leaderInventory.Slots)
	}
	if _, found := followerInventory.Slots["0:178"]; found {
		t.Fatalf("follower admission choice=%+v", followerInventory.Slots)
	}
}

func TestCurrentDungeonEntryPartyFollowerMissingRollsBackEveryEarlierMember(t *testing.T) {
	service, leader, repositories, _ := newPremiumServiceTestRuntime(t)
	follower := &gameSession{selectedCharacterID: 20}
	service.worldMapTable = currentDungeonEntryAdmissionTestWorldMap(t, `
[dungeon]
316 -1
[/dungeon]
[particularhell freepass item]
490005479 1
[/particularhell freepass item]
`)
	if err := repositories.Inventory.Save(context.Background(), dnfrepo.InventoryRecord{
		CharacterID: "19",
		Slots: map[string]dnfrepo.ItemStack{
			"0:177": {ItemID: 490005479, Count: 1},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Inventory.Save(context.Background(), dnfrepo.InventoryRecord{
		CharacterID: "20",
		Slots:       map[string]dnfrepo.ItemStack{},
	}); err != nil {
		t.Fatal(err)
	}
	runtime := func() *runtimeDungeonState {
		return &runtimeDungeonState{Dungeon: worldmap.Dungeon{ID: 316, Metadata: worldmap.DungeonMetadata{
			DungeonType:   "[particular hell]",
			RequiredItems: []worldmap.TicketItem{{ItemID: 10158964, Count: 2}},
		}}}
	}
	prepared := []*runtimePartyPreparedDungeonEntry{{session: follower, runtime: runtime()}}
	if _, err := service.consumeCurrentDungeonEntryPartyItems(
		context.Background(), leader, runtime(), prepared,
	); !errors.Is(err, errDungeonEntryItemMissing) {
		t.Fatalf("party missing error=%v", err)
	}
	leaderInventory := loadAdmissionInventory(t, repositories, "19")
	if stack := leaderInventory.Slots["0:177"]; stack.ItemID != 490005479 || stack.Count != 1 {
		t.Fatalf("leader freepass was not restored after follower rejection: %+v", leaderInventory.Slots)
	}
	if len(prepared[0].entryItemUpdates) != 0 {
		t.Fatalf("rejected follower retained stale op14 projection: %+v", prepared[0].entryItemUpdates)
	}
}

func TestCurrentDungeonEntryLeaderRuntimeCommitFailureRestoresEveryPartyMember(t *testing.T) {
	service, leader, repositories, _ := newPremiumServiceTestRuntime(t)
	follower := &gameSession{selectedCharacterID: 20}
	for _, characterID := range []string{"19", "20"} {
		if err := repositories.Inventory.Save(context.Background(), dnfrepo.InventoryRecord{
			CharacterID: characterID,
			Slots: map[string]dnfrepo.ItemStack{
				"0:180": {ItemID: 4726, Count: 1},
			},
		}); err != nil {
			t.Fatal(err)
		}
	}
	newRuntime := func() *runtimeDungeonState {
		return &runtimeDungeonState{Dungeon: worldmap.Dungeon{ID: 120, Metadata: worldmap.DungeonMetadata{
			RequiredItems: []worldmap.TicketItem{{ItemID: 4726, Count: 1}},
		}}}
	}
	prepared := []*runtimePartyPreparedDungeonEntry{{
		session: follower,
		runtime: newRuntime(),
	}}
	batch, err := service.consumeCurrentDungeonEntryPartyItems(
		context.Background(), leader, newRuntime(), prepared)
	if err != nil {
		t.Fatal(err)
	}
	// The follower mailbox is released after its deduction. Preserve a later
	// unrelated owner mutation when the leader runtime subsequently fails.
	followerConcurrent := loadAdmissionInventory(t, repositories, "20")
	if followerConcurrent.Slots == nil {
		followerConcurrent.Slots = make(map[string]dnfrepo.ItemStack)
	}
	followerConcurrent.Slots["0:181"] = dnfrepo.ItemStack{ItemID: 9181, Count: 4}
	if err := repositories.Inventory.Save(context.Background(), followerConcurrent); err != nil {
		t.Fatal(err)
	}
	// A runtime without a worldmap session is rejected at the exact leader
	// commit boundary, after every member's admission has been persisted.
	if err := service.commitDungeonRuntimeWithPartyAdmission(
		leader,
		newRuntime(),
		prepared,
		batch,
	); err == nil {
		t.Fatal("invalid leader runtime commit unexpectedly succeeded")
	}
	for _, characterID := range []string{"19", "20"} {
		inventory := loadAdmissionInventory(t, repositories, characterID)
		if stack := inventory.Slots["0:180"]; stack.ItemID != 4726 || stack.Count != 1 {
			t.Fatalf("character %s admission was not restored after leader commit failure: %+v", characterID, inventory.Slots)
		}
		if characterID == "20" {
			if stack := inventory.Slots["0:181"]; stack.ItemID != 9181 || stack.Count != 4 {
				t.Fatalf("follower concurrent slot was overwritten: %+v", inventory.Slots)
			}
		}
	}
	// The compensation owner is idempotent: an error-path retry cannot apply a
	// second restore over later inventory state.
	if err := service.restoreCurrentDungeonEntryInventoryBatch(batch); err != nil {
		t.Fatal(err)
	}
}

func TestDungeonEntryAdmissionDoesNotConjoinEventOrAddedItems(t *testing.T) {
	service := &Service{}
	for _, metadata := range []worldmap.DungeonMetadata{
		{
			RequiredItems:      []worldmap.TicketItem{{ItemID: 1001, Count: 2}},
			EventRequiredItems: []worldmap.TicketItem{{ItemID: 2001, Count: 1}},
		},
		{
			RequiredItems:      []worldmap.TicketItem{{ItemID: 1001, Count: 2}},
			AddedRequiredItems: []worldmap.TicketItem{{ItemID: 3001, Count: 3}},
		},
	} {
		_, err := service.resolveCurrentDungeonEntryAdmissionPlan(&runtimeDungeonState{
			Dungeon: worldmap.Dungeon{ID: 9000, Metadata: metadata},
		})
		if err == nil {
			t.Fatalf("event/schedule admission became an ordinary material conjunction: %+v", metadata)
		}
	}
}

func TestSelectDungeonEntryItemMissingFailureBody(t *testing.T) {
	got := buildSelectDungeonFailureBody(currentSelectDungeonEntryItemMissingReason)
	want := []byte{0, 17, 0}
	if string(got) != string(want) {
		t.Fatalf("failure body=%x want=%x", got, want)
	}
}

func currentDungeonEntryAdmissionTestWorldMap(t *testing.T, areaDocument string) *worldmap.Table {
	t.Helper()
	table, err := worldmap.LoadSource(context.Background(), bridgePVFSource{
		worldmap.DefaultWorldMapList: "39 `metrocenter.wdm`\n",
		"worldmap/metrocenter.wdm":   areaDocument,
	}, worldmap.Options{SkipMaps: true, SkipDungeons: true})
	if err != nil {
		t.Fatal(err)
	}
	return table
}

func admissionUpdateSlot(entry currentItemListEntry) int16 {
	return int16(binary.LittleEndian.Uint16(entry.data[0:2]))
}

func admissionUpdateItemID(entry currentItemListEntry) uint32 {
	return binary.LittleEndian.Uint32(entry.data[2:6])
}

func admissionUpdateCount(entry currentItemListEntry) uint32 {
	return binary.LittleEndian.Uint32(entry.data[6:10])
}

func loadAdmissionInventory(
	t *testing.T,
	repositories dnfrepo.Group,
	characterID string,
) dnfrepo.InventoryRecord {
	t.Helper()
	inventory, found, err := repositories.Inventory.Load(context.Background(), characterID)
	if err != nil || !found {
		t.Fatalf("load admission inventory %s: found=%t err=%v", characterID, found, err)
	}
	return inventory
}
