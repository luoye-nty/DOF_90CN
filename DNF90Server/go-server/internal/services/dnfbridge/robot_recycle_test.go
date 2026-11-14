package dnfbridge

import (
	"context"
	"testing"

	dnfchat "longheng.io/server/internal/modules/dnf/chat"
	"longheng.io/server/internal/modules/dnf/itemtrade"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfmemory "longheng.io/server/internal/modules/dnf/repository/memory"
	dnfrobotdomain "longheng.io/server/internal/modules/dnf/robot"
)

func TestPlanRobotRecycleGoldPoolCollectsUnlistedItems(t *testing.T) {
	service, catalog := newRobotRecyclePlanTestService()
	catalog.itemCache[100] = robotRecycleTestEquipment(100, 80, 2, "[free]")
	catalog.itemCache[200] = dungeonDropItemDefinition{
		ItemID: 200, Kind: dungeonDropItemStackable, AttachType: "[free]", StackLimit: 999,
	}
	offers := []onlineItemTradeOffer{
		robotRecycleTestOffer(3, 100, 2),
		robotRecycleTestOffer(4, 200, 20),
	}
	plan, err := service.planRobotRecycle(offers, dnfrobotdomain.RecyclePolicy{
		Enabled:  true,
		BuyItems: []dnfrobotdomain.RecycleBuyItem{{ItemID: 100, Price: 500}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Accepted || !plan.GoldHit || plan.Gold != 1000 || len(plan.Equipments) != 0 {
		t.Fatalf("gold plan = %+v", plan)
	}
	if len(plan.AcceptedOffers) != 2 || len(plan.Messages) != 2 ||
		!robotRecycleMessagesContain(plan.Messages, robotRecycleMessageFavorite) ||
		!robotRecycleMessagesContain(plan.Messages, robotRecycleMessageCheap) {
		t.Fatalf("gold accepted/messages = %+v", plan)
	}
}

func TestPlanRobotRecycleExchangeKeepsMaterialsOnlyWithEquipment(t *testing.T) {
	service, catalog := newRobotRecyclePlanTestService()
	catalog.itemCache[100] = robotRecycleTestEquipment(100, 80, 2, "[free]")
	catalog.itemCache[101] = robotRecycleTestEquipment(101, 75, 2, "[free]")
	catalog.itemCache[200] = dungeonDropItemDefinition{
		ItemID: 200, Kind: dungeonDropItemStackable, AttachType: "[free]", StackLimit: 999,
	}
	catalog.genericEquipment[dungeonDropGradeRarityKey{Grade: 75, Rarity: 2}] = []dungeonDropWeightedItem{{ItemID: 101, Weight: 1}}
	service.robotRecycleRollFn = sequenceRobotRecycleRolls(t, 0, 20, 0)

	plan, err := service.planRobotRecycle([]onlineItemTradeOffer{
		robotRecycleTestOffer(3, 100, 1),
		robotRecycleTestOffer(4, 200, 20),
	}, dnfrobotdomain.RecyclePolicy{Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Accepted || plan.GoldHit || len(plan.Equipments) != 1 || plan.Equipments[0].ItemID != 101 {
		t.Fatalf("exchange plan = %+v", plan)
	}
	if len(plan.AcceptedOffers) != 2 || !robotRecycleMessagesContain(plan.Messages, robotRecycleMessageMaterialTaken) {
		t.Fatalf("exchange offers/messages = %+v", plan)
	}
}

func TestPlanRobotRecycleRejectsPureMaterials(t *testing.T) {
	service, catalog := newRobotRecyclePlanTestService()
	catalog.itemCache[200] = dungeonDropItemDefinition{
		ItemID: 200, Kind: dungeonDropItemStackable, AttachType: "[free]", StackLimit: 999,
	}
	plan, err := service.planRobotRecycle(
		[]onlineItemTradeOffer{robotRecycleTestOffer(3, 200, 20)},
		dnfrobotdomain.RecyclePolicy{Enabled: true},
	)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Accepted || plan.Gold != 0 || !robotRecycleMessagesContain(plan.Messages, robotRecycleMessageRejected) {
		t.Fatalf("pure material plan = %+v", plan)
	}
}

func TestPlanRobotRecycleLowLevelGoblinGateUsesSeventyPercentReject(t *testing.T) {
	service, catalog := newRobotRecyclePlanTestService()
	catalog.itemCache[100] = robotRecycleTestEquipment(100, 10, 0, "[free]")
	service.robotRecycleRollFn = sequenceRobotRecycleRolls(t, 69)
	rejected, err := service.planRobotRecycle(
		[]onlineItemTradeOffer{robotRecycleTestOffer(3, 100, 1)},
		dnfrobotdomain.RecyclePolicy{Enabled: true},
	)
	if err != nil {
		t.Fatal(err)
	}
	if rejected.Accepted || !robotRecycleMessagesContain(rejected.Messages, robotRecycleMessageGoblin) {
		t.Fatalf("goblin rejected plan = %+v", rejected)
	}
	if robotRecycleMessagesContain(rejected.Messages, robotRecycleMessageRejected) {
		t.Fatalf("goblin rejection must not be replaced by the generic rejection: %+v", rejected.Messages)
	}

	catalog.itemCache[101] = robotRecycleTestEquipment(101, 10, 0, "[free]")
	catalog.genericEquipment[dungeonDropGradeRarityKey{Grade: 10, Rarity: 0}] = []dungeonDropWeightedItem{{ItemID: 101, Weight: 1}}
	service.robotRecycleRollFn = sequenceRobotRecycleRolls(t, 70, 50, 20, 0)
	accepted, err := service.planRobotRecycle(
		[]onlineItemTradeOffer{robotRecycleTestOffer(3, 100, 1)},
		dnfrobotdomain.RecyclePolicy{Enabled: true},
	)
	if err != nil {
		t.Fatal(err)
	}
	if !accepted.Accepted || len(accepted.Equipments) != 1 || accepted.Equipments[0].ItemID != 101 {
		t.Fatalf("goblin accepted plan = %+v", accepted)
	}
}

func TestPlanRobotRecycleFallsBackToTradeableNonFreeOutput(t *testing.T) {
	service, catalog := newRobotRecyclePlanTestService()
	catalog.itemCache[100] = robotRecycleTestEquipment(100, 80, 2, "[free]")
	catalog.itemCache[101] = robotRecycleTestEquipment(101, 80, 2, "[sealing]")
	catalog.genericEquipment[dungeonDropGradeRarityKey{Grade: 80, Rarity: 2}] = []dungeonDropWeightedItem{{ItemID: 101, Weight: 1}}
	service.robotRecycleRollFn = sequenceRobotRecycleRolls(t, 50, 20, 0)
	plan, err := service.planRobotRecycle(
		[]onlineItemTradeOffer{robotRecycleTestOffer(3, 100, 1)},
		dnfrobotdomain.RecyclePolicy{Enabled: true},
	)
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Accepted || len(plan.Equipments) != 1 || plan.Equipments[0].ItemID != 101 {
		t.Fatalf("sealing fallback candidate was not returned: %+v", plan)
	}
}

func TestPlanRobotRecycleAcceptsNonFreeInputEquipment(t *testing.T) {
	service, catalog := newRobotRecyclePlanTestService()
	catalog.itemCache[100] = robotRecycleTestEquipment(100, 45, 1, "[trade]")
	catalog.itemCache[101] = robotRecycleTestEquipment(101, 45, 1, "[free]")
	catalog.genericEquipment[dungeonDropGradeRarityKey{Grade: 45, Rarity: 1}] = []dungeonDropWeightedItem{{ItemID: 101, Weight: 1}}
	service.robotRecycleRollFn = sequenceRobotRecycleRolls(t, 70, 50, 20, 0)
	plan, err := service.planRobotRecycle(
		[]onlineItemTradeOffer{robotRecycleTestOffer(3, 100, 1)},
		dnfrobotdomain.RecyclePolicy{Enabled: true},
	)
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Accepted || len(plan.Equipments) != 1 || plan.Equipments[0].ItemID != 101 {
		t.Fatalf("non-free input equipment was rejected: %+v", plan)
	}
}

func TestRobotRecyclePolicyNormalizeRejectsDuplicateIDs(t *testing.T) {
	_, err := dnfrobotdomain.Normalize(dnfrobotdomain.Config{Recycle: dnfrobotdomain.RecyclePolicy{
		Enabled: true,
		BuyItems: []dnfrobotdomain.RecycleBuyItem{
			{ItemID: 1, Price: 10},
			{ItemID: 1, Price: 20},
		},
	}})
	if err == nil {
		t.Fatal("duplicate recycle item IDs must be rejected")
	}
}

func TestRobotRecycleAutoConfirmsAfterPlayerFinalConsent(t *testing.T) {
	const playerID, robotID = uint16(7), uint16(23)
	trade := &onlineItemTrade{
		participants: [2]uint16{playerID, robotID},
		states: map[uint16]byte{
			playerID: currentItemTradeReady,
			robotID:  currentItemTradeAddingDone,
		},
		recycle: &robotRecycleTrade{
			PlayerID: playerID,
			Robot:    dnfrobotdomain.Profile{CharacterID: robotID},
		},
	}
	if !applyRobotRecycleFinalConsentLocked(trade, playerID, currentItemTradeReady) {
		t.Fatal("robot final consent was not applied")
	}
	if trade.states[robotID] != currentItemTradeReady {
		t.Fatalf("robot state=%d want ready", trade.states[robotID])
	}
}

func TestRobotRecycleCleanupPreservesMegaphoneStock(t *testing.T) {
	runtime := dnfmemory.NewRuntime()
	group := runtime.Group()
	ctx := context.Background()
	const characterID = uint16(31)
	if err := group.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "31", AccountID: "manual-robot-009", Slot: 1, Name: "speaker",
	}); err != nil {
		t.Fatal(err)
	}
	if err := group.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "31",
		Slots: map[string]dnfrepo.ItemStack{
			"0:121": {ItemID: int64(dnfchat.ServerMegaphoneItemID), Count: 20},
			"0:122": {ItemID: 400, Count: 1},
			"0:123": {ItemID: 500, Count: 1, Extra: map[string]string{
				robotRecycleAssetMarker: "trade-token",
			}},
		},
	}); err != nil {
		t.Fatal(err)
	}
	service := &Service{repositoryProvider: func() (dnfrepo.Group, bool) { return group, true }}
	recycle := &robotRecycleTrade{
		Robot: dnfrobotdomain.Profile{ID: "manual-robot-009", AccountID: "manual-robot-009", CharacterID: characterID},
		Token: "trade-token",
		Generated: []robotRecycleGeneratedAsset{
			{Slot: 123, ItemID: 500},
		},
	}
	received := []itemtrade.Transfer{{
		DestinationList: 0,
		DestinationSlot: 122,
		Stack:           dnfrepo.ItemStack{ItemID: 400, Count: 1},
	}}
	if err := service.cleanupRobotRecycleAssets(ctx, recycle, 0, received, true); err != nil {
		t.Fatal(err)
	}
	inventory, found, err := group.Inventory.Load(ctx, "31")
	if err != nil || !found {
		t.Fatalf("load inventory: found=%t error=%v", found, err)
	}
	megaphone, found := inventory.Slots["0:121"]
	if !found || megaphone.ItemID != int64(dnfchat.ServerMegaphoneItemID) || megaphone.Count != 20 {
		t.Fatalf("megaphone stock was removed: %+v", megaphone)
	}
	if _, found := inventory.Slots["0:122"]; found {
		t.Fatal("received recycle item was not removed")
	}
	if _, found := inventory.Slots["0:123"]; found {
		t.Fatal("generated exchange item was not removed")
	}
}

func newRobotRecyclePlanTestService() (*Service, *pvfDungeonDropCatalog) {
	catalog := &pvfDungeonDropCatalog{
		source:            finishBridgePVFSource{},
		itemRefs:          make(map[uint32]dungeonDropItemReference),
		itemCache:         make(map[uint32]dungeonDropItemDefinition),
		genericLoaded:     true,
		genericEquipment:  make(map[dungeonDropGradeRarityKey][]dungeonDropWeightedItem),
		genericStackables: make(map[dungeonDropGradeRarityKey][]uint32),
		genericFallbacks:  make(map[dungeonDropGradeRarityKey]uint64),
	}
	return &Service{pvfItemCatalog: catalog}, catalog
}

func robotRecycleTestEquipment(itemID uint32, level, rarity int64, attach string) dungeonDropItemDefinition {
	return dungeonDropItemDefinition{
		ItemID: itemID, Kind: dungeonDropItemEquipment, AttachType: attach,
		MinimumLevel: level, Rarity: rarity, EquipmentType: "[weapon]", SlotStart: 9, SlotEnd: 64,
	}
}

func robotRecycleTestOffer(slot uint16, itemID uint32, count int32) onlineItemTradeOffer {
	return onlineItemTradeOffer{
		tradeSlot:  slot,
		sourceList: dnfrepo.MainInventoryListType,
		sourceSlot: int16(slot),
		count:      count,
		itemID:     int64(itemID),
		stack: dnfrepo.ItemStack{
			ItemID: int64(itemID),
			Count:  int64(count),
			Extra: map[string]string{
				"item_kind": "equipment",
			},
		},
	}
}

func sequenceRobotRecycleRolls(t *testing.T, values ...int) func(int) (int, error) {
	t.Helper()
	index := 0
	return func(limit int) (int, error) {
		if index >= len(values) {
			t.Fatalf("unexpected robot recycle roll #%d limit=%d", index, limit)
		}
		value := values[index]
		index++
		if value < 0 || value >= limit {
			t.Fatalf("robot recycle roll value=%d limit=%d", value, limit)
		}
		return value, nil
	}
}
