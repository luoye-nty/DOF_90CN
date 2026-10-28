package dnfbridge

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"math"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"longheng.io/server/internal/modules/dnf/dnfenum"
	"longheng.io/server/internal/modules/dnf/dungeoncmd"
	dnfproto "longheng.io/server/internal/modules/dnf/protocol"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

func TestBuildCurrentDungeonNormalPickupResultMatchesCurrentEXEOp39ItemBranch(t *testing.T) {
	body, err := buildCurrentDungeonNormalPickupResultBody(0x44332211, 401, 121)
	if err != nil {
		t.Fatal(err)
	}
	if len(body) != currentDungeonPickupResultSize {
		t.Fatalf("body len=%d", len(body))
	}
	if binary.LittleEndian.Uint32(body[0:4]) != 0x44332211 ||
		binary.LittleEndian.Uint16(body[4:6]) != 401 ||
		!bytes.Equal(body[6:14], make([]byte, 8)) ||
		binary.LittleEndian.Uint16(body[14:16]) != 401 ||
		binary.LittleEndian.Uint16(body[16:18]) != 121 ||
		body[18] != 0 {
		t.Fatalf("body=%x", body)
	}
	for _, test := range []struct {
		drop   uint32
		picker uint16
		slot   uint16
	}{{picker: 1, slot: 1}, {drop: 1, slot: 1}, {drop: 1, picker: 1}} {
		if _, err := buildCurrentDungeonNormalPickupResultBody(test.drop, test.picker, test.slot); !errors.Is(err, errDungeonPickupResponseInvalid) {
			t.Fatalf("invalid response values accepted: %+v err=%v", test, err)
		}
	}
}

func TestBuildCurrentDungeonGoldPickupEffectMatchesCurrentEXEOp557(t *testing.T) {
	body, err := buildCurrentDungeonGoldPickupEffectBody(979)
	if err != nil {
		t.Fatal(err)
	}
	if len(body) != currentDungeonGoldPickupEffectSize || binary.LittleEndian.Uint16(body) != 979 {
		t.Fatalf("body=%x", body)
	}
	for _, amount := range []uint32{0, math.MaxUint16 + 1} {
		if _, err := buildCurrentDungeonGoldPickupEffectBody(amount); !errors.Is(err, errDungeonPickupResponseInvalid) {
			t.Fatalf("invalid amount=%d accepted err=%v", amount, err)
		}
	}
}

func TestAddCurrentDungeonPickupKeepsMaterialsInPVFCategorySlots(t *testing.T) {
	material := dungeonDropItemDefinition{
		ItemID:        3227,
		Kind:          dungeonDropItemStackable,
		PVFPath:       "stackable/material/test_drop.stk",
		StackableType: "[material]",
		StackLimit:    999,
		SlotStart:     121,
		SlotEnd:       176,
	}

	t.Run("existing quick-slot stack is ignored in favor of material category", func(t *testing.T) {
		record := dnfrepo.InventoryRecord{Slots: map[string]dnfrepo.ItemStack{
			"0:4":   {ItemID: 3227, Count: 2},
			"0:121": {ItemID: 3227, Count: 5},
		}}
		slot, err := addCurrentDungeonPickupToInventory(&record, material, 1)
		if err != nil {
			t.Fatal(err)
		}
		if slot != 121 || record.Slots["0:4"].Count != 2 || record.Slots["0:121"].Count != 6 {
			t.Fatalf("slot=%d inventory=%+v", slot, record.Slots)
		}
	})

	t.Run("same material-category stack is merged", func(t *testing.T) {
		record := dnfrepo.InventoryRecord{Slots: map[string]dnfrepo.ItemStack{
			"0:121": {ItemID: 3227, Count: 5},
		}}
		slot, err := addCurrentDungeonPickupToInventory(&record, material, 1)
		if err != nil {
			t.Fatal(err)
		}
		if slot != 121 || record.Slots["0:121"].Count != 6 || len(record.Slots) != 1 {
			t.Fatalf("slot=%d inventory=%+v", slot, record.Slots)
		}
	})

	t.Run("material subtype four uses independent material page", func(t *testing.T) {
		materialFour := material
		materialFour.StackableType = "[material] 4"
		materialFour.SlotStart = 345
		materialFour.SlotEnd = 359
		record := dnfrepo.InventoryRecord{Slots: map[string]dnfrepo.ItemStack{}}
		slot, err := addCurrentDungeonPickupToInventory(&record, materialFour, 1)
		if err != nil {
			t.Fatal(err)
		}
		if slot != 345 || record.Slots["0:345"].ItemID != 3227 {
			t.Fatalf("slot=%d inventory=%+v", slot, record.Slots)
		}
		if _, exists := record.Slots["0:3"]; exists {
			t.Fatalf("material subtype four entered quick slot: %+v", record.Slots)
		}
	})

	t.Run("waste still prefers item quick slots", func(t *testing.T) {
		waste := material
		waste.StackableType = "[waste]"
		waste.SlotStart = 65
		waste.SlotEnd = 120
		record := dnfrepo.InventoryRecord{Slots: map[string]dnfrepo.ItemStack{}}
		slot, err := addCurrentDungeonPickupToInventory(&record, waste, 1)
		if err != nil {
			t.Fatal(err)
		}
		if slot != 3 || record.Slots["0:3"].ItemID != 3227 {
			t.Fatalf("slot=%d inventory=%+v", slot, record.Slots)
		}
	})

	t.Run("full quick slots fall back to original category stack", func(t *testing.T) {
		record := dnfrepo.InventoryRecord{Slots: map[string]dnfrepo.ItemStack{
			"0:121": {ItemID: 3227, Count: 5},
		}}
		for slot := int16(3); slot <= 8; slot++ {
			record.Slots[currentDungeonPickupMainSlotKey(slot)] = dnfrepo.ItemStack{
				ItemID: 900000 + int64(slot),
				Count:  1,
			}
		}
		slot, err := addCurrentDungeonPickupToInventory(&record, material, 1)
		if err != nil {
			t.Fatal(err)
		}
		if slot != 121 || record.Slots["0:121"].Count != 6 {
			t.Fatalf("slot=%d inventory=%+v", slot, record.Slots)
		}
	})

	t.Run("PVF stack limit zero merges as unlimited", func(t *testing.T) {
		unlimited := material
		unlimited.ItemID = 3050
		unlimited.StackLimit = 0
		record := dnfrepo.InventoryRecord{Slots: map[string]dnfrepo.ItemStack{
			"0:136": {ItemID: 3050, Count: 1},
		}}
		for slot := int16(3); slot <= 8; slot++ {
			record.Slots[currentDungeonPickupMainSlotKey(slot)] = dnfrepo.ItemStack{
				ItemID: 900000 + int64(slot),
				Count:  1,
			}
		}
		slot, err := addCurrentDungeonPickupToInventory(&record, unlimited, 1)
		if err != nil {
			t.Fatal(err)
		}
		if slot != 136 || record.Slots["0:136"].Count != 2 || len(record.Slots) != 7 {
			t.Fatalf("slot=%d inventory=%+v", slot, record.Slots)
		}
	})

	t.Run("quest type keeps original category", func(t *testing.T) {
		quest := material
		quest.StackableType = "[quest]"
		quest.SlotStart = 177
		quest.SlotEnd = 232
		record := dnfrepo.InventoryRecord{Slots: map[string]dnfrepo.ItemStack{}}
		slot, err := addCurrentDungeonPickupToInventory(&record, quest, 1)
		if err != nil {
			t.Fatal(err)
		}
		if slot != 177 || record.Slots["0:177"].ItemID != 3227 {
			t.Fatalf("slot=%d inventory=%+v", slot, record.Slots)
		}
		if _, exists := record.Slots["0:3"]; exists {
			t.Fatalf("quest item entered quick slot: %+v", record.Slots)
		}
	})
}

func TestAddCurrentDungeonPickupPersistsPVFStackableUsePeriodInRaw77(t *testing.T) {
	expirationDate := time.Date(2028, time.August, 15, 22, 0, 0, 0, time.UTC)
	definition := dungeonDropItemDefinition{
		ItemID:         490702319,
		Kind:           dungeonDropItemStackable,
		PVFPath:        "stackable/cash/chn_490702319.stk",
		StackableType:  "[booster]",
		StackLimit:     1000,
		SlotStart:      65,
		SlotEnd:        120,
		ExpirationDate: expirationDate,
	}
	record := dnfrepo.InventoryRecord{Slots: map[string]dnfrepo.ItemStack{}}
	slot, err := addCurrentDungeonPickupToInventory(&record, definition, 20)
	if err != nil {
		t.Fatal(err)
	}
	stack := record.Slots[currentDungeonPickupMainSlotKey(int16(slot))]
	if !stack.ExpireAt.Equal(expirationDate) || stack.Extra["expire_time"] != "1849989600" || stack.Extra["expire_unix"] != "1849989600" ||
		stack.Extra["item_kind"] != string(dungeonDropItemStackable) ||
		len(stack.RawEntry) != currentItemListEntryWireSize || binary.LittleEndian.Uint16(stack.RawEntry[0x0B:0x0D]) != 0xFFFF ||
		binary.LittleEndian.Uint32(stack.RawEntry[0x6E:0x72]) != 0 {
		t.Fatalf("inserted expiring stack=%+v row=%x", stack, stack.RawEntry)
	}

	stale := stack
	stale.Extra = nil
	stale.RawEntry = append([]byte(nil), stale.RawEntry...)
	binary.LittleEndian.PutUint16(stale.RawEntry[0x0B:0x0D], 0)
	record.Slots[currentDungeonPickupMainSlotKey(int16(slot))] = stale
	mergedSlot, err := addCurrentDungeonPickupToInventory(&record, definition, 5)
	if err != nil {
		t.Fatal(err)
	}
	merged := record.Slots[currentDungeonPickupMainSlotKey(int16(mergedSlot))]
	if mergedSlot != slot || merged.Count != 25 || !merged.ExpireAt.Equal(expirationDate) || merged.Extra["expire_time"] != "1849989600" ||
		binary.LittleEndian.Uint16(merged.RawEntry[0x0B:0x0D]) != 0xFFFF || binary.LittleEndian.Uint32(merged.RawEntry[0x6E:0x72]) != 0 {
		t.Fatalf("merged expiring stack slot=%d stack=%+v", mergedSlot, merged)
	}
}

func TestCurrentDungeonNormalMonsterDropRequiresRuntimePVFRules(t *testing.T) {
	service, runtime, _ := prepareCurrentDungeonDropTest(t, 2, 3227)
	monster := runtime.Room.Snapshot().Monsters[0]
	if _, err := service.planCurrentDungeonMonsterDrops(runtime, monster, currentSceneActorObjectKey(99)); !errors.Is(err, errCurrentDungeonDropRulesUnavailable) {
		t.Fatalf("normal-drop planner error=%v", err)
	}
	if runtime.NextObjectKey != 403 || runtime.DropOwner != nil {
		t.Fatalf("deferred planner mutated runtime next=%d owner=%+v", runtime.NextObjectKey, runtime.DropOwner)
	}
}

func TestCurrentDungeonGoldDropRegistersRealAmountForPickup(t *testing.T) {
	service, runtime, _ := prepareCurrentDungeonDropTest(t, 0, 3227)
	source, ok := service.dungeonMonsterTable.source.(bridgePVFSource)
	if !ok {
		t.Fatalf("test source type=%T", service.dungeonMonsterTable.source)
	}
	source[dungeonCardDropRulePath] = currentDungeonGoldDropTestRuleText()
	source[dungeonCardGoldReferencePath] = "[gold drop ref table]\n20 100 0\n"
	currentDungeonMonsterDropRulesByCatalog.Delete(service.dungeonMonsterTable)
	currentDungeonGoldReferencesByCatalog.Delete(service.dungeonMonsterTable)

	monster := runtime.Room.Snapshot().Monsters[0]
	wires, err := service.planCurrentDungeonMonsterDrops(runtime, monster, currentSceneActorObjectKey(99))
	if err != nil {
		t.Fatal(err)
	}
	var goldWire *currentDungeonDeathDropWire
	for index := range wires {
		if binary.LittleEndian.Uint32(wires[index].Item.data[0x02:0x06]) == 0 {
			goldWire = &wires[index]
			break
		}
	}
	if goldWire == nil {
		t.Fatalf("gold drop was not planned: wires=%+v", wires)
	}
	wireAmount := binary.LittleEndian.Uint32(goldWire.Item.data[0x06:0x0A])
	if wireAmount != 120 {
		t.Fatalf("gold wire amount=%d want 120", wireAmount)
	}
	registered := runtime.DropOwner.byObjectKey[goldWire.SceneObjectKey]
	if registered == nil || !registered.isGold() || registered.Amount != wireAmount {
		t.Fatalf("registered gold drop=%+v wire_amount=%d", registered, wireAmount)
	}
}

func TestCurrentDungeonDropPlannerSkipsPersistedDropObjectKeys(t *testing.T) {
	service, runtime, _ := prepareCurrentDungeonDropTest(t, 0, 3227)
	source, ok := service.dungeonMonsterTable.source.(bridgePVFSource)
	if !ok {
		t.Fatalf("test source type=%T", service.dungeonMonsterTable.source)
	}
	source[dungeonCardDropRulePath] = currentDungeonGoldDropTestRuleText()
	source[dungeonCardGoldReferencePath] = "[gold drop ref table]\n20 100 0\n"
	currentDungeonMonsterDropRulesByCatalog.Delete(service.dungeonMonsterTable)
	currentDungeonGoldReferencesByCatalog.Delete(service.dungeonMonsterTable)

	staleNextObjectKey := runtime.NextObjectKey
	owner := newRuntimeDungeonDropOwner()
	if err := owner.registerBatch([]*runtimeDungeonDrop{{
		ObjectKey:           staleNextObjectKey,
		SceneSlot:           1,
		OwnerActorObjectKey: currentSceneActorObjectKey(99),
		Room:                runtimeDungeonRoomKey{X: -1, Y: -1, MapID: -1},
		Status:              runtimeDungeonDropConsumed,
	}}, 2); err != nil {
		t.Fatal(err)
	}
	runtime.DropOwner = owner

	monster := runtime.Room.Snapshot().Monsters[0]
	wires, err := service.planCurrentDungeonMonsterDrops(runtime, monster, currentSceneActorObjectKey(99))
	if err != nil {
		t.Fatal(err)
	}
	if len(wires) == 0 || wires[0].SceneObjectKey <= staleNextObjectKey {
		t.Fatalf("drop planner reused persisted object key: wires=%+v prior=%d", wires, staleNextObjectKey)
	}
}

func TestCurrentDungeonGoldPickupSendsRemoveEffectThenAbsoluteState(t *testing.T) {
	service, runtime, repositories := prepareCurrentDungeonDropTest(t, 0, 3227)
	connection := &bufferConn{}
	session := &gameSession{
		conn:                connection,
		connID:              "dungeon-gold-pickup-effect-test",
		selectedCharacterID: 99,
		dungeon:             dungeonSessionState{runtime: runtime},
	}
	scene, ok := runtime.Session.Scene()
	if !ok {
		t.Fatal("missing dungeon scene")
	}
	objectKey := runtime.NextObjectKey
	drop := &runtimeDungeonDrop{
		ObjectKey:           objectKey,
		SceneSlot:           1,
		Amount:              979,
		OwnerActorObjectKey: currentSceneActorObjectKey(99),
		Room:                runtimeDungeonRoomKeyFromScene(scene),
		Status:              runtimeDungeonDropAvailable,
	}
	owner := newRuntimeDungeonDropOwner()
	if err := owner.registerBatch([]*runtimeDungeonDrop{drop}, 2); err != nil {
		t.Fatal(err)
	}
	runtime.DropOwner = owner
	runtime.NextObjectKey++

	pickupBody := currentDungeonPickupTestBody(objectKey)
	if err := service.handleCurrentDungeonPickup(session, pickupBody); err != nil {
		t.Fatal(err)
	}
	removePacket, rest := splitGameServerUpperPacket(t, connection.write.Bytes())
	effectPacket, rest := splitGameServerUpperPacket(t, rest)
	goldStatePacket, trailing := splitGameServerUpperPacket(t, rest)
	wantRemove, err := buildCurrentDungeonGoldPickupResultBody(objectKey, currentSceneActorObjectKey(99))
	if err != nil {
		t.Fatal(err)
	}
	if removePacket.Header.Classification != 0 || removePacket.Header.MsgID != currentDungeonPickupResultOpcode ||
		!bytes.Equal(removePacket.Body, wantRemove) {
		t.Fatalf("remove header=%+v body=%x", removePacket.Header, removePacket.Body)
	}
	if effectPacket.Header.Classification != 0 || effectPacket.Header.MsgID != currentDungeonGoldPickupEffectOpcode ||
		len(effectPacket.Body) != currentDungeonGoldPickupEffectSize || binary.LittleEndian.Uint16(effectPacket.Body) != 979 {
		t.Fatalf("effect header=%+v body=%x", effectPacket.Header, effectPacket.Body)
	}
	if goldStatePacket.Header.Classification != 0 || goldStatePacket.Header.MsgID != uint16(dnfenum.CmdPacketWalkoutPartyMember) ||
		!bytes.Equal(goldStatePacket.Body, buildCurrentGoldStateBody(979)) || len(trailing) != 0 {
		t.Fatalf("gold state header=%+v body=%x trailing=%x", goldStatePacket.Header, goldStatePacket.Body, trailing)
	}
	character, found, err := repositories.Character.Load(context.Background(), "99")
	if err != nil || !found || character.Stats["gold"] != 979 {
		t.Fatalf("character found=%t err=%v stats=%+v", found, err, character.Stats)
	}
	if drop.Status != runtimeDungeonDropConsumed || drop.GoldAfter != 979 {
		t.Fatalf("consumed gold drop=%+v", drop)
	}

	connection.write.Reset()
	if err := service.handleCurrentDungeonPickup(session, pickupBody); err != nil {
		t.Fatal(err)
	}
	replayRemove, replayRest := splitGameServerUpperPacket(t, connection.write.Bytes())
	replayGoldState, replayTrailing := splitGameServerUpperPacket(t, replayRest)
	if replayRemove.Header.MsgID != currentDungeonPickupResultOpcode ||
		replayGoldState.Header.MsgID != uint16(dnfenum.CmdPacketWalkoutPartyMember) ||
		len(replayTrailing) != 0 {
		t.Fatalf("replay remove=%+v state=%+v trailing=%x", replayRemove.Header, replayGoldState.Header, replayTrailing)
	}
	character, _, err = repositories.Character.Load(context.Background(), "99")
	if err != nil || character.Stats["gold"] != 979 {
		t.Fatalf("replay changed gold err=%v stats=%+v", err, character.Stats)
	}
}

func TestCurrentDungeonNormalPickupIsOwnedTransactionalAndIdempotent(t *testing.T) {
	service, runtime, repositories := prepareCurrentDungeonDropTest(t, 2, 3227)
	connection := &bufferConn{}
	session := &gameSession{
		conn:                connection,
		connID:              "dungeon-drop-pickup-test",
		selectedCharacterID: 99,
		dungeon:             dungeonSessionState{runtime: runtime},
	}
	monster := runtime.Room.Snapshot().Monsters[0]
	deathBody := make([]byte, dungeoncmd.DieMonsterRequestSize)
	binary.LittleEndian.PutUint32(deathBody[0:4], monster.ObjectKey)
	binary.LittleEndian.PutUint16(deathBody[4:6], currentSceneActorObjectKey(99))
	if err := service.handleDungeonMonsterDeath(session, deathBody); err != nil {
		t.Fatal(err)
	}
	deathPacket, rest := splitGameServerUpperPacket(t, connection.write.Bytes())
	if deathPacket.Header.MsgID != uint16(dnfenum.CmdPacketNotifyDieMonster) ||
		deathPacket.Header.Classification != 0 {
		t.Fatalf("death packet header=%+v rest=%x", deathPacket.Header, rest)
	}
	assertCurrentDungeonFinalClearTail(t, rest)
	if len(deathPacket.Body) != currentDungeonZeroDropDeathBodySize || deathPacket.Body[2] != 0 {
		t.Fatalf("death body len=%d count=%d body=%x", len(deathPacket.Body), deathPacket.Body[2], deathPacket.Body)
	}
	firstDropObjectKey := seedCurrentDungeonPickupDrop(t, service, runtime, 3227, currentSceneActorObjectKey(99))

	connection.write.Reset()
	pickupBody := currentDungeonPickupTestBody(firstDropObjectKey)
	pickupFrame, err := dnfproto.BuildChannelPacket(
		uint16(dnfenum.CmdPacketGetItem),
		pickupBody,
		1,
		dnfproto.DefaultChannelClassification,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.handleGameUpper(session, pickupFrame); err != nil {
		t.Fatal(err)
	}
	pickupPacket, rest := splitGameServerUpperPacket(t, connection.write.Bytes())
	itemUpdate, trailing := splitGameServerUpperPacket(t, rest)
	wantPickup, err := buildCurrentDungeonNormalPickupResultBody(firstDropObjectKey, currentSceneActorObjectKey(99), 121)
	if err != nil {
		t.Fatal(err)
	}
	if pickupPacket.Header.MsgID != currentDungeonPickupResultOpcode ||
		pickupPacket.Header.Classification != 0 ||
		!bytes.Equal(pickupPacket.Body, wantPickup) || len(trailing) != 0 {
		t.Fatalf("pickup packet header=%+v body=%x trailing=%x", pickupPacket.Header, pickupPacket.Body, trailing)
	}
	if itemUpdate.Header.MsgID != uint16(dnfenum.CmdPacketWalkoutPartyMember) ||
		itemUpdate.Header.Classification != 0 ||
		len(itemUpdate.Body) != 3+currentItemListEntryWireSize ||
		itemUpdate.Body[0] != dnfrepo.MainInventoryListType ||
		binary.LittleEndian.Uint16(itemUpdate.Body[1:3]) != 1 ||
		binary.LittleEndian.Uint16(itemUpdate.Body[3:5]) != 121 ||
		binary.LittleEndian.Uint32(itemUpdate.Body[5:9]) != 3227 ||
		binary.LittleEndian.Uint32(itemUpdate.Body[9:13]) != 1 {
		t.Fatalf("pickup item update header=%+v body=%x", itemUpdate.Header, itemUpdate.Body)
	}
	inventory, found, err := repositories.Inventory.Load(context.Background(), "99")
	if err != nil || !found {
		t.Fatalf("inventory found=%t err=%v", found, err)
	}
	stack := inventory.Slots["0:121"]
	if stack.ItemID != 3227 || stack.Count != 1 || stack.Extra["pvf_path"] != "stackable/material/test_drop.stk" ||
		stack.Extra["stack_limit"] != "999" || len(stack.RawEntry) != currentItemListEntryWireSize {
		t.Fatalf("persisted stack=%+v", stack)
	}
	reloadBody, reloadSource, reloadCount, reloadOK := service.buildCurrentItemListBodyForSession(
		context.Background(),
		session,
		dnfrepo.MainInventoryListType,
	)
	if !reloadOK || reloadSource != "inventory" || reloadCount != 1 ||
		len(reloadBody) != 5+currentItemListEntryWireSize ||
		reloadBody[0] != dnfrepo.MainInventoryListType ||
		binary.LittleEndian.Uint16(reloadBody[3:5]) != 1 {
		t.Fatalf(
			"persisted pickup reload ok=%t source=%q count=%d body=%x",
			reloadOK,
			reloadSource,
			reloadCount,
			reloadBody,
		)
	}
	reloadedEntry := reloadBody[5:]
	if binary.LittleEndian.Uint16(reloadedEntry[0:2]) != 121 ||
		binary.LittleEndian.Uint32(reloadedEntry[2:6]) != 3227 ||
		binary.LittleEndian.Uint32(reloadedEntry[6:10]) != 1 {
		t.Fatalf("persisted pickup reloaded entry=%x", reloadedEntry)
	}
	drop := runtime.DropOwner.byObjectKey[firstDropObjectKey]
	if drop.Status != runtimeDungeonDropConsumed || drop.DestinationSlot != 121 || len(drop.PickupResponseBody) != currentDungeonPickupResultSize {
		t.Fatalf("consumed drop=%+v", drop)
	}

	connection.write.Reset()
	if err := service.handleCurrentDungeonPickup(session, pickupBody); err != nil {
		t.Fatal(err)
	}
	replayPacket, replayRest := splitGameServerUpperPacket(t, connection.write.Bytes())
	replayUpdate, replayTrailing := splitGameServerUpperPacket(t, replayRest)
	if !bytes.Equal(replayPacket.Body, wantPickup) || len(replayTrailing) != 0 ||
		!bytes.Equal(replayUpdate.Body, itemUpdate.Body) {
		t.Fatalf("replay body=%x update=%x trailing=%x", replayPacket.Body, replayUpdate.Body, replayTrailing)
	}
	inventory, _, err = repositories.Inventory.Load(context.Background(), "99")
	if err != nil || inventory.Slots["0:121"].Count != 1 {
		t.Fatalf("duplicate pickup changed inventory=%+v err=%v", inventory.Slots, err)
	}
}

func TestCurrentDungeonPickupRoutesCubeFragmentsToSharedCrystalSlots(t *testing.T) {
	for _, test := range []struct {
		itemID uint32
		slot   int16
	}{
		{3033, 354},
		{3034, 355},
		{3035, 356},
		{3036, 357},
		{3037, 358},
		{3262, 359},
	} {
		t.Run(strconv.FormatUint(uint64(test.itemID), 10), func(t *testing.T) {
			service, runtime, repositories := prepareCurrentDungeonDropTest(t, 1, test.itemID)
			connection := &bufferConn{}
			session := &gameSession{
				conn:                connection,
				connID:              "dungeon-cube-pickup-test",
				selectedCharacterID: 99,
				dungeon:             dungeonSessionState{runtime: runtime},
			}
			dropObjectKey := seedCurrentDungeonPickupDrop(t, service, runtime, test.itemID, currentSceneActorObjectKey(99))

			if err := service.handleCurrentDungeonPickup(session, currentDungeonPickupTestBody(dropObjectKey)); err != nil {
				t.Fatal(err)
			}
			pickup, rest := splitGameServerUpperPacket(t, connection.write.Bytes())
			update, trailing := splitGameServerUpperPacket(t, rest)
			if pickup.Header.MsgID != currentDungeonPickupResultOpcode || len(trailing) != 0 ||
				update.Header.MsgID != uint16(dnfenum.CmdPacketWalkoutPartyMember) || update.Body[0] != dnfrepo.MainInventoryListType ||
				binary.LittleEndian.Uint16(update.Body[1:3]) != 1 || binary.LittleEndian.Uint16(update.Body[3:5]) != uint16(test.slot) ||
				binary.LittleEndian.Uint32(update.Body[5:9]) != test.itemID || binary.LittleEndian.Uint32(update.Body[9:13]) != 1 {
				t.Fatalf("cube pickup=%+v update=%x trailing=%x", pickup.Header, update.Body, trailing)
			}
			account, found, err := repositories.AccountInventory.Load(context.Background(), "account-1")
			if err != nil || !found {
				t.Fatalf("account inventory found=%t err=%v", found, err)
			}
			if stack := account.Slots[dnfrepo.AccountSharedInventorySlotKey(test.slot)]; stack.ItemID != int64(test.itemID) || stack.Count != 1 {
				t.Fatalf("shared crystal stack=%+v", stack)
			}
			inventory, found, err := repositories.Inventory.Load(context.Background(), "99")
			if err != nil || !found || len(inventory.Slots) != 0 {
				t.Fatalf("character inventory found=%t slots=%+v err=%v", found, inventory.Slots, err)
			}
		})
	}
}

func TestCurrentDungeonPickupConsumesOnlyRequestedGroundObject(t *testing.T) {
	service, runtime, repositories := prepareCurrentDungeonDropTest(t, 1, 3227)
	connection := &bufferConn{}
	session := &gameSession{
		conn:                connection,
		connID:              "dungeon-manual-pickup-test",
		selectedCharacterID: 99,
		dungeon:             dungeonSessionState{runtime: runtime},
	}
	firstObjectKey := seedCurrentDungeonPickupDrop(
		t,
		service,
		runtime,
		3227,
		currentSceneActorObjectKey(99),
	)
	first := runtime.DropOwner.byObjectKey[firstObjectKey]
	second := *first
	second.ObjectKey = runtime.NextObjectKey
	second.SceneSlot = first.SceneSlot + 1
	third := *first
	third.ObjectKey = runtime.NextObjectKey + 1
	third.SceneSlot = first.SceneSlot + 2
	if err := runtime.DropOwner.registerBatch([]*runtimeDungeonDrop{&second, &third}, third.SceneSlot+1); err != nil {
		t.Fatal(err)
	}
	runtime.NextObjectKey += 2

	if err := service.handleCurrentDungeonPickup(session, currentDungeonPickupTestBody(second.ObjectKey)); err != nil {
		t.Fatal(err)
	}
	if runtime.DropOwner.byObjectKey[second.ObjectKey].Status != runtimeDungeonDropConsumed {
		t.Fatalf("selected drop status=%d, want consumed", runtime.DropOwner.byObjectKey[second.ObjectKey].Status)
	}
	for _, objectKey := range []uint32{firstObjectKey, third.ObjectKey} {
		if status := runtime.DropOwner.byObjectKey[objectKey].Status; status != runtimeDungeonDropAvailable {
			t.Fatalf("unselected drop %d status=%d, want available", objectKey, status)
		}
	}
	inventory, found, err := repositories.Inventory.Load(context.Background(), "99")
	if err != nil || !found {
		t.Fatalf("inventory found=%t err=%v", found, err)
	}
	if stack := inventory.Slots["0:121"]; stack.ItemID != 3227 || stack.Count != 1 {
		t.Fatalf("manual pickup stack=%+v", stack)
	}

	pickupPacket, rest := splitGameServerUpperPacket(t, connection.write.Bytes())
	if pickupPacket.Header.MsgID != currentDungeonPickupResultOpcode ||
		binary.LittleEndian.Uint32(pickupPacket.Body[0:4]) != second.ObjectKey {
		t.Fatalf("pickup header=%+v body=%x", pickupPacket.Header, pickupPacket.Body)
	}
	itemUpdate, trailing := splitGameServerUpperPacket(t, rest)
	if itemUpdate.Header.MsgID != uint16(dnfenum.CmdPacketWalkoutPartyMember) || len(trailing) != 0 {
		t.Fatalf("item update header=%+v trailing=%x", itemUpdate.Header, trailing)
	}
}

func TestCurrentDungeonPickupTransactionFailureLeavesDropAvailableAndSendsNothing(t *testing.T) {
	service, runtime, repositories := prepareCurrentDungeonDropTest(t, 1, 3227)
	connection := &bufferConn{}
	session := &gameSession{
		conn:                connection,
		connID:              "dungeon-drop-rollback-test",
		selectedCharacterID: 99,
		dungeon:             dungeonSessionState{runtime: runtime},
	}
	dropObjectKey := seedCurrentDungeonPickupDrop(
		t,
		service,
		runtime,
		3227,
		currentSceneActorObjectKey(99),
	)
	wantErr := errors.New("forced pickup save failure")
	failingInventory := pickupFailingInventoryRepository{InventoryRepository: repositories.Inventory, err: wantErr}
	failingGroup := repositories
	failingGroup.CharacterItems = pickupTestCharacterItemUnitOfWork{
		base:      repositories.CharacterItems,
		inventory: failingInventory,
		equipment: repositories.Equipment,
	}
	service.repositoryProvider = func() (dnfrepo.Group, bool) { return failingGroup, true }

	connection.write.Reset()
	if err := service.handleCurrentDungeonPickup(session, currentDungeonPickupTestBody(dropObjectKey)); err != nil {
		t.Fatal(err)
	}
	if connection.write.Len() != 0 {
		t.Fatalf("failed transaction emitted packet=%x", connection.write.Bytes())
	}
	if drop := runtime.DropOwner.byObjectKey[dropObjectKey]; drop.Status != runtimeDungeonDropAvailable || len(drop.PickupResponseBody) != 0 {
		t.Fatalf("failed transaction consumed drop=%+v", drop)
	}
	inventory, _, err := repositories.Inventory.Load(context.Background(), "99")
	if err != nil || len(inventory.Slots) != 0 {
		t.Fatalf("failed transaction changed inventory=%+v err=%v", inventory.Slots, err)
	}
}

func TestCurrentDungeonPickupRepositoryWriteDoesNotBlockSessionEvents(t *testing.T) {
	service, runtime, repositories := prepareCurrentDungeonDropTest(t, 1, 3227)
	connection := &bufferConn{}
	ctx, cancel := context.WithCancel(context.Background())
	session := &gameSession{
		conn:                connection,
		connID:              "dungeon-pickup-event-loop-test",
		selectedCharacterID: 99,
		characterGeneration: 1,
		ctx:                 ctx,
		dungeon:             dungeonSessionState{runtime: runtime},
	}
	if err := service.startGameSessionEvents(ctx, session); err != nil {
		cancel()
		t.Fatal(err)
	}

	releaseWrite := make(chan struct{})
	var releaseWriteOnce sync.Once
	releaseRepositoryWrite := func() {
		releaseWriteOnce.Do(func() { close(releaseWrite) })
	}
	defer func() {
		releaseRepositoryWrite()
		cancel()
		stopCtx, stopCancel := context.WithTimeout(context.Background(), time.Second)
		defer stopCancel()
		_ = service.stopGameSessionEvents(stopCtx, session)
	}()
	writeEntered := make(chan struct{})
	writeDeadline := make(chan time.Duration, 1)
	blockingInventory := &pickupBlockingInventoryRepository{
		InventoryRepository: repositories.Inventory,
		entered:             writeEntered,
		release:             releaseWrite,
		deadline:            writeDeadline,
	}
	blockingGroup := repositories
	blockingGroup.CharacterItems = pickupTestCharacterItemUnitOfWork{
		base:      repositories.CharacterItems,
		inventory: blockingInventory,
		equipment: repositories.Equipment,
	}
	service.repositoryProvider = func() (dnfrepo.Group, bool) { return blockingGroup, true }
	dropObjectKey := seedCurrentDungeonPickupDrop(
		t,
		service,
		runtime,
		3227,
		currentSceneActorObjectKey(99),
	)

	pickupDone := make(chan error, 1)
	go func() {
		pickupDone <- service.callGameSession(ctx, session, "blocked-pickup", func() error {
			return service.handleCurrentDungeonPickup(session, currentDungeonPickupTestBody(dropObjectKey))
		})
	}()
	select {
	case <-writeEntered:
	case <-time.After(time.Second):
		t.Fatal("pickup repository write did not start")
	}
	select {
	case remaining := <-writeDeadline:
		if remaining <= createWriteTimeout || remaining > currentDungeonPickupWriteTimeout {
			t.Fatalf("pickup repository deadline remaining=%s want=(%s,%s]", remaining, createWriteTimeout, currentDungeonPickupWriteTimeout)
		}
	case <-time.After(time.Second):
		t.Fatal("pickup repository write did not observe a context deadline")
	}
	if err := service.callGameSession(ctx, session, "pending-pickup-replay", func() error {
		return service.handleCurrentDungeonPickup(session, currentDungeonPickupTestBody(dropObjectKey))
	}); err != nil {
		t.Fatal(err)
	}
	session.dungeon.mu.Lock()
	pendingDrop := runtime.DropOwner.byObjectKey[dropObjectKey]
	pendingStatus := pendingDrop.Status
	pendingAttempt := pendingDrop.PickupAttempt
	session.dungeon.mu.Unlock()
	if pendingStatus != runtimeDungeonDropPending || pendingAttempt != 1 {
		t.Fatalf("pending replay changed drop status=%d attempt=%d", pendingStatus, pendingAttempt)
	}
	if connection.write.Len() != 0 {
		t.Fatalf("pending replay emitted a premature packet=%x", connection.write.Bytes())
	}

	nextEventRan := make(chan struct{})
	go func() {
		_ = service.callGameSession(ctx, session, "next-dungeon-event", func() error {
			close(nextEventRan)
			return nil
		})
	}()
	select {
	case <-nextEventRan:
	case <-time.After(250 * time.Millisecond):
		t.Fatal("a blocked pickup repository write stalled the complete session event loop")
	}
	select {
	case err := <-pickupDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("pickup handler remained on the session event loop while persistence was pending")
	}

	releaseRepositoryWrite()
	var wire []byte
	completionDeadline := time.Now().Add(2 * time.Second)
	for {
		var status runtimeDungeonDropStatus
		if err := service.callGameSession(ctx, session, "pickup-completion-barrier", func() error {
			session.dungeon.mu.Lock()
			status = runtime.DropOwner.byObjectKey[dropObjectKey].Status
			session.dungeon.mu.Unlock()
			if status == runtimeDungeonDropConsumed {
				wire = append([]byte(nil), connection.write.Bytes()...)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if status == runtimeDungeonDropConsumed {
			break
		}
		if time.Now().After(completionDeadline) {
			t.Fatalf("pickup persistence did not complete; drop status=%d", status)
		}
		time.Sleep(time.Millisecond)
	}
	if calls := blockingInventory.calls.Load(); calls != 1 {
		t.Fatalf("pending replay performed %d repository writes, want 1", calls)
	}
	pickupPacket, rest := splitGameServerUpperPacket(t, wire)
	itemUpdate, trailing := splitGameServerUpperPacket(t, rest)
	wantPickup, err := buildCurrentDungeonNormalPickupResultBody(
		dropObjectKey,
		currentSceneActorObjectKey(99),
		121,
	)
	if err != nil {
		t.Fatal(err)
	}
	if pickupPacket.Header.MsgID != currentDungeonPickupResultOpcode ||
		!bytes.Equal(pickupPacket.Body, wantPickup) ||
		itemUpdate.Header.MsgID != uint16(dnfenum.CmdPacketWalkoutPartyMember) ||
		len(trailing) != 0 {
		t.Fatalf(
			"async pickup packet=%+v body=%x update=%+v trailing=%x",
			pickupPacket.Header,
			pickupPacket.Body,
			itemUpdate.Header,
			trailing,
		)
	}
}

func TestRuntimeDungeonDropOwnerRejectsStaleRoomAndActor(t *testing.T) {
	owner := newRuntimeDungeonDropOwner()
	drop := &runtimeDungeonDrop{
		ObjectKey:           700,
		SceneSlot:           1,
		OwnerActorObjectKey: 401,
		Room:                runtimeDungeonRoomKey{X: 1, Y: 2, MapID: 3},
		Status:              runtimeDungeonDropAvailable,
	}
	if err := owner.registerBatch([]*runtimeDungeonDrop{drop}, 2); err != nil {
		t.Fatal(err)
	}
	if _, err := owner.owned(700, runtimeDungeonRoomKey{X: 9, Y: 2, MapID: 3}, 401); !errors.Is(err, errDungeonDropRoomMismatch) {
		t.Fatalf("stale-room error=%v", err)
	}
	if _, err := owner.owned(700, drop.Room, 402); !errors.Is(err, errDungeonDropActorMismatch) {
		t.Fatalf("actor error=%v", err)
	}
}

func seedCurrentDungeonPickupDrop(
	t *testing.T,
	service *Service,
	runtime *runtimeDungeonState,
	itemID uint32,
	ownerActorObjectKey uint16,
) uint32 {
	t.Helper()
	catalog, err := service.dungeonMonsterCatalog()
	if err != nil {
		t.Fatal(err)
	}
	drops, err := catalog.DropCatalog()
	if err != nil {
		t.Fatal(err)
	}
	definition, err := drops.ResolveItem(itemID)
	if err != nil {
		t.Fatal(err)
	}
	scene, ok := runtime.Session.Scene()
	if !ok {
		t.Fatal("missing dungeon scene")
	}
	objectKey := runtime.NextObjectKey
	if objectKey == 0 {
		t.Fatal("missing next object key")
	}
	drop := &runtimeDungeonDrop{
		ObjectKey:           objectKey,
		SceneSlot:           1,
		Item:                definition,
		Amount:              1,
		OwnerActorObjectKey: ownerActorObjectKey,
		Room:                runtimeDungeonRoomKeyFromScene(scene),
		Status:              runtimeDungeonDropAvailable,
	}
	owner := newRuntimeDungeonDropOwner()
	if err := owner.registerBatch([]*runtimeDungeonDrop{drop}, 2); err != nil {
		t.Fatal(err)
	}
	runtime.DropOwner = owner
	runtime.NextObjectKey++
	return objectKey
}

func prepareCurrentDungeonDropTest(
	t *testing.T,
	fixedDropCount int,
	itemID uint32,
) (*Service, *runtimeDungeonState, dnfrepo.Group) {
	t.Helper()
	source := bridgeDungeonPVF(false)
	source["map/dungeon/test/start.map"] = "[map name]\n`start`\n" +
		"[dungeon]\n700\n" +
		"[type]\n`[start]`\n" +
		"[monster]\n3001 10 0 100 200 0 0 " + strconv.Itoa(fixedDropCount) + " `[fixed]` `[normal]`\n"
	source["monster/test.gob"] += "[item]\n" + strconv.FormatUint(uint64(itemID), 10) + " 10\n[/item]\n"
	source[dungeonDropStackableList] = strconv.FormatUint(uint64(itemID), 10) + " `material/test_drop.stk`\n"
	source[dungeonDropEquipmentList] = "9001 `weapon/test_drop.equ`\n"
	source["stackable/material/test_drop.stk"] = "[name]\n`Test Drop`\n[stackable type]\n`[material]`\n[stack limit]\n999\n[attach type]\n`[free]`\n"
	source["equipment/weapon/test_drop.equ"] = "[name]\n`Test Equipment`\n[durability]\n50\n[attach type]\n`[trade]`\n"
	table, resolver, monsters := loadBridgeDungeonStaticData(t, source)
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Character.Save(context.Background(), dnfrepo.CharacterRecord{
		CharacterID: "99",
		AccountID:   "account-1",
		Level:       20,
		Stats:       map[string]int64{"fatigue": 100},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Inventory.Save(context.Background(), dnfrepo.InventoryRecord{
		CharacterID: "99",
		Slots:       map[string]dnfrepo.ItemStack{},
	}); err != nil {
		t.Fatal(err)
	}
	service := &Service{
		options:             options{accountID: "account-1", gameUpperBodyCodec: gameUpperBodyCodecPlain},
		worldMapTable:       table,
		worldMapResolver:    resolver,
		dungeonMonsterTable: monsters,
		dungeonChoice:       func(int) (int, error) { return 0, nil },
		dungeonSeed:         func() (uint32, error) { return 0x12345678, nil },
		repositoryProvider:  func() (dnfrepo.Group, bool) { return repositories, true },
	}
	session := &gameSession{selectedCharacterID: 99}
	runtime, _, err := service.prepareDungeonRuntime(
		context.Background(),
		session,
		dungeoncmd.SelectDungeonRequest{DungeonID: 700, Difficulty: 1},
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Room.AnnounceAllActors(runtime.Session); err != nil {
		t.Fatal(err)
	}
	return service, runtime, repositories
}

func currentDungeonPickupTestBody(dropObjectKey uint32) []byte {
	body := make([]byte, dungeoncmd.GetItemRequestSize)
	binary.LittleEndian.PutUint32(body[0:4], dropObjectKey)
	body[5] = 7
	for index, value := range []uint16{100, 200, 300, 400, 500, 600, 700} {
		binary.LittleEndian.PutUint16(body[6+index*2:], value)
	}
	return body
}

func currentDungeonGoldDropTestRuleText() string {
	return `[drop prob count]
1
[drop prob]
1 200 10000 0 0 0 0
[basis of rarity dicision]
700000 964900 990000 1000000 1000001 1000002 1000003
545500 899500 999500 1000000 1000001 1000002 1000003
500000 944900 999900 1000000 1000001 1000002 1000003
700000 944900 999000 1000000 1000001 1000002 1000003
[party member drop bonusrate]
1 1 1 1 1 1.3 1.6 2 1 1.2 1.4 1.6 1 1.2 1.4 1.6 1 1.2 1.4 1.6
[dungeon difficulty drop bonusrate]
1.00 1.00 1.00 1.40 1.10 1.00 1.40 1.60 2.20 2.00 1.00 1.40 1.60 2.20 2.00 1.00 1.40 1.60 2.20 2.00 1.00 1.40 1.60 2.20 2.00
[monster type drop bonusrate]
1.00 2.00 3.00 6.60 1.00 2.00 3.00 4.00 0.20 2.00 7.00 16.80 1.00 2.00 3.00 4.00 1.00 2.00 3.00 4.00
[item drop ref table]
1 0 3
[first boss/named mob hunting]
1 1
[condition rate]
95 5 93 7 90 10 85 15
[gold quantity]
1 2 4 6
[gold volume]
100 110
[item drop rarity control]
2 13 17 0 0 3 13 17 0 0
[/item drop rarity control]
`
}

type pickupFailingInventoryRepository struct {
	dnfrepo.InventoryRepository
	err error
}

type pickupBlockingInventoryRepository struct {
	dnfrepo.InventoryRepository
	once     sync.Once
	entered  chan struct{}
	release  <-chan struct{}
	deadline chan<- time.Duration
	calls    atomic.Int32
}

func (repository *pickupBlockingInventoryRepository) Save(
	ctx context.Context,
	record dnfrepo.InventoryRecord,
) error {
	repository.calls.Add(1)
	if deadline, ok := ctx.Deadline(); ok && repository.deadline != nil {
		select {
		case repository.deadline <- time.Until(deadline):
		default:
		}
	}
	repository.once.Do(func() { close(repository.entered) })
	select {
	case <-repository.release:
	case <-ctx.Done():
		return ctx.Err()
	}
	return repository.InventoryRepository.Save(ctx, record)
}

func (repository pickupFailingInventoryRepository) Save(context.Context, dnfrepo.InventoryRecord) error {
	return repository.err
}

type pickupTestCharacterItemUnitOfWork struct {
	base      dnfrepo.CharacterItemUnitOfWork
	inventory dnfrepo.InventoryRepository
	equipment dnfrepo.EquipmentRepository
}

func (unit pickupTestCharacterItemUnitOfWork) WithinCharacterItems(
	ctx context.Context,
	characterID string,
	apply func(dnfrepo.InventoryRepository, dnfrepo.EquipmentRepository) error,
) error {
	return unit.base.WithinCharacterItems(ctx, characterID, func(
		_ dnfrepo.InventoryRepository,
		_ dnfrepo.EquipmentRepository,
	) error {
		return apply(unit.inventory, unit.equipment)
	})
}
