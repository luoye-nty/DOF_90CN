package dnfbridge

import (
	"context"
	"encoding/binary"
	"os"
	"testing"

	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfproto "longheng.io/server/internal/modules/dnf/protocol"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

func TestCurrentResetItemAttrConsumesMaterialBeforeRefreshingRandomGradeTarget(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "19",
		Slots: map[string]dnfrepo.ItemStack{
			"0:42": {ItemID: 1001, Count: 1},
			"0:75": {ItemID: 2001, Count: 2, Extra: map[string]string{"item_kind": "stackable"}},
		},
	}); err != nil {
		t.Fatal(err)
	}

	connection := &bufferConn{}
	service := &Service{
		options:            options{accountID: "account-1"},
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
	}
	session := &gameSession{conn: connection, connID: "grade-adjust-test", selectedCharacterID: 19}
	body := make([]byte, 8)
	binary.LittleEndian.PutUint16(body[0:2], 42)
	binary.LittleEndian.PutUint32(body[2:6], 1001)
	binary.LittleEndian.PutUint16(body[6:8], 75)
	if err := service.handleCurrentResetItemAttr(session, body); err != nil {
		t.Fatal(err)
	}

	ack, rest := splitGameServerUpperPacket(t, connection.write.Bytes())
	if ack.Header.Classification != dnfproto.DefaultChannelClassification ||
		ack.Header.MsgID != uint16(dnfenum.CmdPacketResetItemAttr) ||
		len(ack.Body) != 9 || ack.Body[0] != 1 {
		t.Fatalf("ack header=%+v body=%x", ack.Header, ack.Body)
	}
	if slot := binary.LittleEndian.Uint16(ack.Body[1:3]); slot != 75 {
		t.Fatalf("ack slot=%d, want consumed material slot 75", slot)
	}
	if amount := binary.LittleEndian.Uint32(ack.Body[3:7]); amount != 1 {
		t.Fatalf("ack amount=%d, want one material remaining", amount)
	}
	if resultType := binary.LittleEndian.Uint16(ack.Body[7:9]); resultType != currentResetItemAttrGradeResult {
		t.Fatalf("ack result type=%d, want native grade-adjust result", resultType)
	}

	refresh, trailing := splitGameServerUpperPacket(t, rest)
	if len(trailing) != 0 {
		t.Fatalf("trailing bytes=%d", len(trailing))
	}
	if refresh.Header.Classification != 0 ||
		refresh.Header.MsgID != uint16(dnfenum.CmdPacketWalkoutPartyMember) {
		t.Fatalf("refresh header=%+v", refresh.Header)
	}
	if len(refresh.Body) != 3+currentItemListEntryWireSize ||
		refresh.Body[0] != dnfrepo.MainInventoryListType ||
		binary.LittleEndian.Uint16(refresh.Body[1:3]) != 1 {
		t.Fatalf("refresh body=%x", refresh.Body)
	}
	targetRow := refresh.Body[3:]
	if slot := int16(binary.LittleEndian.Uint16(targetRow[0:2])); slot != 42 {
		t.Fatalf("refreshed slot=%d, want target slot 42", slot)
	}

	inventory, found, err := repositories.Inventory.Load(ctx, "19")
	if err != nil || !found {
		t.Fatalf("load inventory found=%t err=%v", found, err)
	}
	target := inventory.Slots["0:42"]
	seed := currentEquipmentQualitySeedFromStack(target)
	if seed == 0 || seed > currentEquipmentRandomQualitySeedCount {
		t.Fatalf("persisted quality seed=%d, want full ordinary seed range", seed)
	}
	if got := binary.LittleEndian.Uint32(targetRow[6:10]); got != seed {
		t.Fatalf("refreshed quality seed=%d, persisted=%d", got, seed)
	}
	if target.Extra["grade_adjust_type"] != "standard_kaleido" {
		t.Fatalf("target extra=%v", target.Extra)
	}
	if target.Extra["item_kind"] != "equipment" {
		t.Fatalf("legacy target identity was not normalized: %v", target.Extra)
	}
	if material := inventory.Slots["0:75"]; material.Count != 1 {
		t.Fatalf("material after=%+v, want one remaining", material)
	}
}

func TestCurrentResetItemAttrGoldUsesGradeResultAndRemovedMaterialSlot(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "19",
		Slots: map[string]dnfrepo.ItemStack{
			"0:42": {
				ItemID: 1001,
				Count:  1,
				Extra: map[string]string{
					"item_kind":      "equipment",
					"equipment_type": "[earring]",
					"pvf_path":       "equipment/character/common/earring/1001.equ",
				},
			},
			"0:75": {ItemID: 2001, Count: 1, Extra: map[string]string{"item_kind": "stackable"}},
		},
	}); err != nil {
		t.Fatal(err)
	}

	connection := &bufferConn{}
	catalog := &pvfDungeonDropCatalog{
		source: finishBridgePVFSource{},
		itemCache: map[uint32]dungeonDropItemDefinition{
			2001: {
				ItemID:        2001,
				Kind:          dungeonDropItemStackable,
				StackableType: "`gold kaleido`",
			},
		},
	}
	service := &Service{
		options:            options{accountID: "account-1"},
		pvfItemCatalog:     catalog,
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
	}
	session := &gameSession{conn: connection, connID: "gold-grade-adjust-test", selectedCharacterID: 19}
	body := make([]byte, 8)
	binary.LittleEndian.PutUint16(body[0:2], 42)
	binary.LittleEndian.PutUint32(body[2:6], 1001)
	binary.LittleEndian.PutUint16(body[6:8], 75)
	if err := service.handleCurrentResetItemAttr(session, body); err != nil {
		t.Fatal(err)
	}

	ack, rest := splitGameServerUpperPacket(t, connection.write.Bytes())
	if len(ack.Body) != 9 || ack.Body[0] != 1 ||
		binary.LittleEndian.Uint16(ack.Body[1:3]) != 75 ||
		binary.LittleEndian.Uint32(ack.Body[3:7]) != 0 ||
		binary.LittleEndian.Uint16(ack.Body[7:9]) != currentResetItemAttrGradeResult {
		t.Fatalf("gold ack=%x", ack.Body)
	}
	refresh, trailing := splitGameServerUpperPacket(t, rest)
	if len(trailing) != 0 ||
		refresh.Header.MsgID != uint16(dnfenum.CmdPacketWalkoutPartyMember) ||
		len(refresh.Body) != 3+currentItemListEntryWireSize {
		t.Fatalf("gold refresh header=%+v body_len=%d trailing=%d", refresh.Header, len(refresh.Body), len(trailing))
	}
	if seed := binary.LittleEndian.Uint32(refresh.Body[3+6 : 3+10]); seed != currentEquipmentTopQualitySeed {
		t.Fatalf("gold refreshed quality seed=%d, want top seed=%d", seed, currentEquipmentTopQualitySeed)
	}
	inventory, found, err := repositories.Inventory.Load(ctx, "19")
	if err != nil || !found {
		t.Fatalf("load inventory found=%t err=%v", found, err)
	}
	if target := inventory.Slots["0:42"]; target.Extra["equipment_type"] != "[earring]" || target.Extra["quality_seed"] != "999999998" {
		t.Fatalf("gold earring target=%+v", target)
	}
}

func TestCurrentResetItemAttrTenByteKaleidoRequestsDoNotEnterReseal(t *testing.T) {
	tests := []struct {
		name          string
		materialID    int64
		stackableType string
		wantKind      string
		wantTop       bool
	}{
		{name: "ordinary legacy etc item 15", materialID: 15, stackableType: "[etc]", wantKind: "standard_kaleido"},
		{name: "gold", materialID: 2001, stackableType: "[gold kaleido]", wantKind: "gold_kaleido", wantTop: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			repositories := dnfrepomemory.NewMemoryGroup()
			if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{
				CharacterID: "19",
				Slots: map[string]dnfrepo.ItemStack{
					"0:38": {ItemID: 1001, Count: 1},
					"0:67": {ItemID: test.materialID, Count: 1},
				},
			}); err != nil {
				t.Fatal(err)
			}
			service := currentResealTestService(repositories, map[uint32]dungeonDropItemDefinition{
				1001:                    {ItemID: 1001, Kind: dungeonDropItemEquipment},
				uint32(test.materialID): {ItemID: uint32(test.materialID), Kind: dungeonDropItemStackable, StackableType: test.stackableType},
			})
			connection := &bufferConn{}
			session := &gameSession{conn: connection, selectedCharacterID: 19}
			if err := service.handleCurrentResetItemAttr(session, currentResealTestBody(38, 1001, 67, 0)); err != nil {
				t.Fatal(err)
			}
			ack, _ := splitGameServerUpperPacket(t, connection.write.Bytes())
			if len(ack.Body) != 9 || ack.Body[0] != 1 || binary.LittleEndian.Uint16(ack.Body[7:9]) != currentResetItemAttrGradeResult {
				t.Fatalf("ack=%x", ack.Body)
			}
			inventory, found, err := repositories.Inventory.Load(ctx, "19")
			if err != nil || !found {
				t.Fatalf("load inventory found=%t err=%v", found, err)
			}
			target := inventory.Slots["0:38"]
			if target.Extra["grade_adjust_type"] != test.wantKind {
				t.Fatalf("target=%+v", target)
			}
			if test.wantTop && currentEquipmentQualitySeedFromStack(target) != currentEquipmentTopQualitySeed {
				t.Fatalf("gold quality seed=%d", currentEquipmentQualitySeedFromStack(target))
			}
			if _, exists := inventory.Slots["0:67"]; exists {
				t.Fatalf("material was not consumed: %+v", inventory.Slots["0:67"])
			}
		})
	}
}

func TestRealPVFItem15TenByteRequestUsesGradeAdjustmentPath(t *testing.T) {
	pvfPath := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE to verify the active equipment grade box")
	}
	archive, err := platformpvf.LoadArchive(platformpvf.Options{Path: pvfPath})
	if err != nil {
		t.Fatalf("load active Script.pvf: %v", err)
	}
	catalog, err := newPVFDungeonDropCatalog(archive)
	if err != nil {
		t.Fatalf("load active item catalog: %v", err)
	}
	definition, err := catalog.ResolveItem(15)
	if err != nil {
		t.Fatalf("resolve active item 15: %v", err)
	}
	if got := normalizeDungeonDropStackableType(definition.StackableType); got != "[etc]" {
		t.Fatalf("active item 15 stackable type=%q, want legacy [etc] evidence", got)
	}

	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "19",
		Slots: map[string]dnfrepo.ItemStack{
			"0:41": {ItemID: 1001, Count: 1},
			"0:67": {ItemID: 15, Count: 1},
		},
	}); err != nil {
		t.Fatal(err)
	}
	service := &Service{
		options:            options{accountID: "account-1"},
		pvfItemCatalog:     catalog,
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
	}
	connection := &bufferConn{}
	session := &gameSession{conn: connection, connID: "active-grade-adjust", selectedCharacterID: 19}
	if err := service.handleCurrentResetItemAttr(session, currentResealTestBody(41, 1001, 67, 0)); err != nil {
		t.Fatal(err)
	}
	ack, rest := splitGameServerUpperPacket(t, connection.write.Bytes())
	if len(ack.Body) != 9 || ack.Body[0] != 1 ||
		binary.LittleEndian.Uint16(ack.Body[1:3]) != 67 ||
		binary.LittleEndian.Uint16(ack.Body[7:9]) != currentResetItemAttrGradeResult {
		t.Fatalf("active item 15 op81 ack=%x", ack.Body)
	}
	refresh, trailing := splitGameServerUpperPacket(t, rest)
	if refresh.Header.MsgID != uint16(dnfenum.CmdPacketWalkoutPartyMember) || len(trailing) != 0 {
		t.Fatalf("active item 15 refresh=%+v trailing=%d", refresh.Header, len(trailing))
	}
	inventory, found, err := repositories.Inventory.Load(ctx, "19")
	if err != nil || !found {
		t.Fatalf("load adjusted inventory found=%t err=%v", found, err)
	}
	if _, found := inventory.Slots["0:67"]; found {
		t.Fatalf("active item 15 was not consumed: %+v", inventory.Slots["0:67"])
	}
	if inventory.Slots["0:41"].Extra["grade_adjust_type"] != "standard_kaleido" {
		t.Fatalf("active target=%+v", inventory.Slots["0:41"])
	}
}

func TestCurrentGoldenWaxResealConsumesPVFCalculatedCount(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	raw := make([]byte, currentItemListEntryWireSize)
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{CharacterID: "19", Slots: map[string]dnfrepo.ItemStack{
		"0:42": {ItemID: 1001, Count: 1, RawEntry: raw},
		"0:75": {ItemID: 14, Count: 10},
	}}); err != nil {
		t.Fatal(err)
	}
	service := currentResealTestService(repositories, map[uint32]dungeonDropItemDefinition{
		1001: {ItemID: 1001, Kind: dungeonDropItemEquipment, MinimumLevel: 85, Rarity: 3, EquipmentType: "[coat]"},
		14:   {ItemID: 14, Kind: dungeonDropItemStackable, PVFPath: "stackable/cash/candle_goldwax.stk"},
	})
	connection := &bufferConn{}
	session := &gameSession{conn: connection, selectedCharacterID: 19}
	body := currentResealTestBody(42, 1001, 75, 0)
	if err := service.handleCurrentResetItemAttr(session, body); err != nil {
		t.Fatal(err)
	}
	inventory, _, _ := repositories.Inventory.Load(ctx, "19")
	target := inventory.Slots["0:42"]
	if currentItemSealFlag(target) != 1 || currentResealCount(target) != 1 || inventory.Slots["0:75"].Count != 2 {
		t.Fatalf("target=%+v wax=%+v", target, inventory.Slots["0:75"])
	}
	ack, _ := splitGameServerUpperPacket(t, connection.write.Bytes())
	if len(ack.Body) != 9 || ack.Body[0] != 1 || binary.LittleEndian.Uint16(ack.Body[7:9]) != 1 {
		t.Fatalf("reseal ack=%x", ack.Body)
	}
	// The ACK's slot/amount must name the consumed WAX row (10 -> 2), never the
	// equipment: the client applies them as a row mutation and deletes the row
	// when amount is 0 (2026-10-04 crash, see the reseal ACK comment).
	if slot := binary.LittleEndian.Uint16(ack.Body[1:3]); slot != 75 {
		t.Fatalf("reseal ack slot=%d, want the wax slot 75; ack=%x", slot, ack.Body)
	}
	if amount := binary.LittleEndian.Uint32(ack.Body[3:7]); amount != 2 {
		t.Fatalf("reseal ack amount=%d, want the wax remaining 2; ack=%x", amount, ack.Body)
	}
}

func TestCurrentOneShotGoldenWaxResealsAtLimitWithoutIncrementingCount(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	raw := make([]byte, currentItemListEntryWireSize)
	raw[0x0a] = 7 << 5
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{CharacterID: "19", Slots: map[string]dnfrepo.ItemStack{
		"0:42": {ItemID: 1001, Count: 1, RawEntry: raw, Extra: map[string]string{"reseal_count": "7"}},
		"0:75": {ItemID: 2660238, Count: 1},
	}}); err != nil {
		t.Fatal(err)
	}
	service := currentResealTestService(repositories, map[uint32]dungeonDropItemDefinition{
		1001:    {ItemID: 1001, Kind: dungeonDropItemEquipment, MinimumLevel: 85, Rarity: 3, EquipmentType: "[coat]"},
		2660238: {ItemID: 2660238, Kind: dungeonDropItemStackable, PVFPath: "stackable/cash/candle_goldwax_oneshot.stk", ResealLevelMin: 1, ResealLevelMax: 86, ResealOneShot: true},
	})
	connection := &bufferConn{}
	session := &gameSession{conn: connection, selectedCharacterID: 19}
	if err := service.handleCurrentResetItemAttr(session, currentResealTestBody(42, 1001, 75, 0)); err != nil {
		t.Fatal(err)
	}
	inventory, _, _ := repositories.Inventory.Load(ctx, "19")
	target := inventory.Slots["0:42"]
	if currentItemSealFlag(target) != 1 || currentResealCount(target) != 7 {
		t.Fatalf("one-shot target=%+v", target)
	}
	if _, found := inventory.Slots["0:75"]; found {
		t.Fatal("one-shot wax was not consumed")
	}
}

func TestCurrentGrandiResealConsumesAccountSoulWarehouse(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{CharacterID: "19", Slots: map[string]dnfrepo.ItemStack{
		"0:42": {ItemID: 1001, Count: 1, RawEntry: make([]byte, currentItemListEntryWireSize)},
	}}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.AccountInventory.Save(ctx, dnfrepo.AccountInventoryRecord{AccountID: "account-1", Slots: map[string]dnfrepo.ItemStack{
		"0:363": {ItemID: 10099774, Count: 100},
	}}); err != nil {
		t.Fatal(err)
	}
	service := currentResealTestService(repositories, map[uint32]dungeonDropItemDefinition{
		1001: {ItemID: 1001, Kind: dungeonDropItemEquipment, MinimumLevel: 85, Rarity: 3, EquipmentType: "[coat]"},
	})
	connection := &bufferConn{}
	session := &gameSession{conn: connection, selectedCharacterID: 19}
	if err := service.handleCurrentResetItemAttr(session, currentResealTestBody(42, 1001, 0, 1)); err != nil {
		t.Fatal(err)
	}
	inventory, _, _ := repositories.Inventory.Load(ctx, "19")
	account, _, _ := repositories.AccountInventory.Load(ctx, "account-1")
	if currentItemSealFlag(inventory.Slots["0:42"]) != 1 || account.Slots["0:363"].Count != 20 {
		t.Fatalf("target=%+v soul=%+v", inventory.Slots["0:42"], account.Slots["0:363"])
	}
	// Account-soul reseal (mode 1) consumes the soul in the account-shared
	// warehouse, which the client addresses as main-list slot 363 -- so the ACK
	// must carry that slot with its remaining count (100 -> 20).
	ack, _ := splitGameServerUpperPacket(t, connection.write.Bytes())
	if len(ack.Body) != 9 || ack.Body[0] != 1 || binary.LittleEndian.Uint16(ack.Body[7:9]) != 1 {
		t.Fatalf("reseal ack=%x", ack.Body)
	}
	if slot := binary.LittleEndian.Uint16(ack.Body[1:3]); slot != 363 {
		t.Fatalf("account-soul reseal ack slot=%d, want the soul slot 363; ack=%x", slot, ack.Body)
	}
	if amount := binary.LittleEndian.Uint32(ack.Body[3:7]); amount != 20 {
		t.Fatalf("account-soul reseal ack amount=%d, want the soul remaining 20; ack=%x", amount, ack.Body)
	}
}

func currentResealTestService(repositories dnfrepo.Group, definitions map[uint32]dungeonDropItemDefinition) *Service {
	return &Service{
		options: options{accountID: "account-1"},
		pvfItemCatalog: &pvfDungeonDropCatalog{
			source: finishBridgePVFSource{
				currentResealConfigPath: `[grade]
1 80 3 81 85 4 86 90 5
[/grade]
[rarity weight]
1 2 1 2 8 8 6
[/rarity weight]
[extended rarity weight]
0 0 1 1 1 1 1
[/extended rarity weight]
[reseal soul index]
0 10100115 1 10100116 2 10099773 3 10099774 4 10158124 5 3311 6 10099775
[/reseal soul index]
[soul cost weight]
0 0 100 10 0 1 2
[/soul cost weight]
[soul reseal constant]
0 0 1.10 1.10 0 0 1.11
[/soul reseal constant]
[reseal count limit]
1 85 7 86 90 4
[/reseal count limit]`,
			},
			itemCache: definitions,
		},
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
	}
}

func currentResealTestBody(targetSlot int16, targetItemID uint32, materialSlot int16, mode uint16) []byte {
	body := make([]byte, 10)
	binary.LittleEndian.PutUint16(body[0:2], uint16(targetSlot))
	binary.LittleEndian.PutUint32(body[2:6], targetItemID)
	binary.LittleEndian.PutUint16(body[6:8], uint16(materialSlot))
	binary.LittleEndian.PutUint16(body[8:10], mode)
	return body
}
