package dnfbridge

import (
	"bytes"
	"context"
	"encoding/binary"
	"reflect"
	"testing"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfproto "longheng.io/server/internal/modules/dnf/protocol"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

func TestDecodeSkillSwitchInventoryRequestActions(t *testing.T) {
	tests := []struct {
		name   string
		body   []byte
		action skillSwitchInventoryAction
	}{
		{"set-item", []byte{0, 0, 13, 0, 17, 0}, skillSwitchSetItem},
		{"remove-item", []byte{0, 0, 13, 44, 0xff, 0xff}, skillSwitchRemoveItem},
		{"set-skill", []byte{40, 0, 33, 44, 0xff, 0xff}, skillSwitchSetSkill},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, action, err := decodeSkillSwitchInventoryRequest(tt.body)
			if err != nil || action != tt.action {
				t.Fatalf("action=%d err=%v want=%d", action, err, tt.action)
			}
		})
	}
}

func TestSkillSwitchInventoryProtocolBodies(t *testing.T) {
	positions := map[byte]skillSwitchInventoryPosition{
		13: {EquipmentPart: 13, ListType: 0, Slot: 17},
		21: {EquipmentPart: 21, ListType: 3, Slot: 21},
	}
	got := buildSkillSwitchDeltaPositionBody(positions, []byte{21, 13})
	want := []byte{2, 1, 13, 0, 17, 0, 1, 21, 3, 21, 0}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("delta=%x want=%x", got, want)
	}
	state := skillSwitchInventoryState{SkillID: 40, Items: []skillSwitchBinding{{EquipmentPart: 21, ItemSerial: 9, ItemID: 700}, {EquipmentPart: 13, ItemSerial: 8, ItemID: 600}}}
	all := buildSkillSwitchAllDataBody(state, positions)
	if binary.LittleEndian.Uint16(all[:2]) != 40 || all[2] != 2 || len(all) != 13 {
		t.Fatalf("all=%x", all)
	}
}

func TestSkillSwitchInventoryAllowsPVFRegisteredSkillForCharacterGrowth(t *testing.T) {
	source := initialEquipmentMemSource{
		"character/character.lst":           "11 `Swordman/ATSwordman.chr`\n",
		"character/Swordman/ATSwordman.chr": "[job]\n`[at swordman]`\n",
		"skill/switchingsupportskill.etc":   "[character type]\n`at swordman` 1\n[skill]\n14\n[/skill]\n\n[character type]\n`at swordman` 2\n[skill]\n76\n[/skill]\n",
	}
	character := dnfrepo.CharacterRecord{Job: "11", Stats: map[string]int64{"grow_type": 1}}

	allowed, err := skillSwitchSkillAllowedByPVFSource(source, character, 14)
	if err != nil || !allowed {
		t.Fatalf("registered skill allowed=%t err=%v", allowed, err)
	}
	for _, rejected := range []struct {
		name     string
		growType int64
		skillID  uint16
	}{
		{name: "wrong-growth", growType: 2, skillID: 14},
		{name: "wrong-skill", growType: 1, skillID: 76},
	} {
		t.Run(rejected.name, func(t *testing.T) {
			character.Stats["grow_type"] = rejected.growType
			allowed, err := skillSwitchSkillAllowedByPVFSource(source, character, rejected.skillID)
			if err != nil {
				t.Fatal(err)
			}
			if allowed {
				t.Fatalf("grow=%d skill=%d was accepted", rejected.growType, rejected.skillID)
			}
		})
	}
}

func TestSkillSwitchInventoryBindsEquippedCreatureByDurableSerial(t *testing.T) {
	const (
		characterID = "77"
		itemID      = uint32(9001)
		serial      = uint32(43)
	)
	catalog, err := newPVFDungeonDropCatalog(skillSwitchInventoryTestPVF())
	if err != nil {
		t.Fatal(err)
	}
	repos := testRepositoryGroup()
	equipment := dnfrepo.EquipmentRecord{
		CharacterID: characterID,
		Entries: map[string]dnfrepo.EquipmentEntry{
			"26": {
				SlotIndex: 26,
				ItemID:    int64(itemID),
				RawEntry:  testCurrentEquippedCreatureRaw(26, itemID, serial),
			},
		},
	}
	if err := repos.Equipment.Save(context.Background(), equipment); err != nil {
		t.Fatal(err)
	}
	if err := repos.Inventory.Save(context.Background(), dnfrepo.InventoryRecord{
		CharacterID: characterID,
		Slots:       map[string]dnfrepo.ItemStack{},
		Warehouse:   map[string]dnfrepo.ItemStack{},
	}); err != nil {
		t.Fatal(err)
	}
	service := &Service{pvfItemCatalog: catalog}
	binding, err := service.skillSwitchBindingForRequest(context.Background(), repos, characterID, skillSwitchInventoryRequest{
		EquipmentPart: 26,
		ListType:      3,
		Slot:          26,
	})
	if err != nil {
		t.Fatalf("bind equipped creature: %v", err)
	}
	if binding.ItemID != itemID || binding.ItemSerial != serial {
		t.Fatalf("binding=%+v, want item=%d serial=%d", binding, itemID, serial)
	}
	positions := locateSkillSwitchBindings(skillSwitchInventoryState{Items: []skillSwitchBinding{binding}}, dnfrepo.InventoryRecord{}, equipment)
	position, ok := positions[26]
	if !ok || position.ListType != 3 || position.Slot != 26 || position.ItemSerial != serial {
		t.Fatalf("creature position=%+v, want equipped slot 26 with serial %d", position, serial)
	}
}

func TestSkillSwitchInventoryBindsCreatureFromPetInventory(t *testing.T) {
	catalog, err := newPVFDungeonDropCatalog(skillSwitchInventoryTestPVF())
	if err != nil {
		t.Fatal(err)
	}
	repos := testRepositoryGroup()
	inventory := dnfrepo.InventoryRecord{
		CharacterID: "77",
		Slots: map[string]dnfrepo.ItemStack{
			"7:58": {ItemID: 9001, Count: 1, Extra: map[string]string{"creature_serial_or_handle": "43"}},
		},
		Warehouse: map[string]dnfrepo.ItemStack{},
	}
	if err := repos.Inventory.Save(context.Background(), inventory); err != nil {
		t.Fatal(err)
	}
	if err := repos.Equipment.Save(context.Background(), dnfrepo.EquipmentRecord{CharacterID: "77"}); err != nil {
		t.Fatal(err)
	}
	service := &Service{pvfItemCatalog: catalog}
	binding, err := service.skillSwitchBindingForRequest(context.Background(), repos, "77", skillSwitchInventoryRequest{
		EquipmentPart: 26,
		ListType:      currentPetInventoryListType,
		Slot:          58,
	})
	if err != nil {
		t.Fatalf("bind pet-inventory creature: %v", err)
	}
	positions := locateSkillSwitchBindings(skillSwitchInventoryState{Items: []skillSwitchBinding{binding}}, inventory, dnfrepo.EquipmentRecord{})
	position, ok := positions[26]
	if !ok || position.ListType != currentPetInventoryListType || position.Slot != 58 {
		t.Fatalf("pet-inventory position=%+v, want list=7 slot=58", position)
	}
}

func TestSkillSwitchInventoryLocatesUniqueItemWhenLegacySerialWasDropped(t *testing.T) {
	const itemID = uint32(101010864)
	inventory := dnfrepo.InventoryRecord{
		Slots: map[string]dnfrepo.ItemStack{
			"0:17": {ItemID: int64(itemID), Count: 1, RawEntry: make([]byte, currentItemListEntryWireSize)},
		},
		Warehouse: map[string]dnfrepo.ItemStack{},
	}
	state := skillSwitchInventoryState{Items: []skillSwitchBinding{{EquipmentPart: 12, ItemSerial: 20, ItemID: itemID}}}
	positions := locateSkillSwitchBindings(state, inventory, dnfrepo.EquipmentRecord{})
	position, ok := positions[12]
	if !ok || position.ListType != 0 || position.Slot != 17 || position.ItemSerial != 20 {
		t.Fatalf("position=%+v, want unique serial-less item at list 0 slot 17", position)
	}
}

func TestSkillSwitchInventoryRejectsAmbiguousSerialLessTemplateFallback(t *testing.T) {
	const itemID = uint32(400330105)
	inventory := dnfrepo.InventoryRecord{
		Slots: map[string]dnfrepo.ItemStack{
			"0:64":  {ItemID: int64(itemID), Count: 1},
			"0:100": {ItemID: int64(itemID), Count: 1},
		},
		Warehouse: map[string]dnfrepo.ItemStack{},
	}
	state := skillSwitchInventoryState{Items: []skillSwitchBinding{{EquipmentPart: 13, ItemSerial: 61, ItemID: itemID}}}
	if positions := locateSkillSwitchBindings(state, inventory, dnfrepo.EquipmentRecord{}); len(positions) != 0 {
		t.Fatalf("ambiguous fallback positions=%+v, want no guessed binding", positions)
	}
}

func TestAlignedItemMoveRefreshesSkillSwitchRegistrationAtDynamicSlot(t *testing.T) {
	service, session, connection, _ := newLegacyTownSceneReadySkillTest(t)
	repositories, ok := service.repositoryGroup()
	if !ok {
		t.Fatal("repository group unavailable")
	}
	ctx := context.Background()
	const (
		itemID = int64(101010864)
		serial = uint32(20)
	)
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "29",
		Slots: map[string]dnfrepo.ItemStack{
			// The registration was made in a different slot. The committed
			// repository now owns the same item instance at slot 31.
			"0:31": {ItemID: itemID, Count: 1, Extra: map[string]string{"item_serial": "20"}},
		},
		Warehouse: map[string]dnfrepo.ItemStack{},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Equipment.Save(ctx, dnfrepo.EquipmentRecord{CharacterID: "29"}); err != nil {
		t.Fatal(err)
	}
	if err := saveSkillSwitchInventoryState(ctx, repositories.Settings, "29", skillSwitchInventoryState{
		SkillID: 40,
		Items: []skillSwitchBinding{{
			EquipmentPart: 12,
			ItemSerial:    serial,
			ItemID:        uint32(itemID),
		}},
	}); err != nil {
		t.Fatal(err)
	}

	ackBody := []byte{1, 0, 17, 0, 1, 0, 0, 0, 0, 31, 0}
	result := alignedcmd.Result{
		Operation: "move_itemspace",
		UpperResponses: []alignedcmd.UpperResponse{{
			MsgID:          uint16(dnfenum.CmdPacketMoveItemspace),
			Body:           ackBody,
			Classification: dnfproto.DefaultChannelClassification,
			AllowCodec:     true,
		}},
	}
	if err := service.sendAlignedUpperResponses(session, result); err != nil {
		t.Fatal(err)
	}
	ack, rest := splitCurrentGameServerUpperPacketAuto(t, connection.write.Bytes())
	if ack.Header.MsgID != uint16(dnfenum.CmdPacketMoveItemspace) ||
		ack.Header.Classification != dnfproto.DefaultChannelClassification ||
		!bytes.Equal(ack.Body, ackBody) {
		t.Fatalf("move ACK header=%+v body=%x", ack.Header, ack.Body)
	}
	buff, rest := splitCurrentGameServerUpperPacketAuto(t, rest)
	if buff.Header.MsgID != skillSwitchInventoryAllDataWire || buff.Header.Classification != 0 {
		t.Fatalf("dynamic BUFF refresh header=%+v body=%x", buff.Header, buff.Body)
	}
	if want := []byte{40, 0, 1, 1, 12, 0, 31, 0}; !bytes.Equal(buff.Body, want) {
		t.Fatalf("dynamic BUFF refresh body=%x want=%x", buff.Body, want)
	}
	if len(rest) != 0 {
		t.Fatalf("unexpected move refresh tail=%x", rest)
	}
}

func TestCurrentActorBindingMode1KeepsCreatureTailAdjacentToDeferredRows(t *testing.T) {
	const (
		itemID         = uint32(400990168)
		serialOrHandle = uint32(41)
	)
	ctx := context.Background()
	repos := testRepositoryGroup()
	if err := repos.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "77",
		Slots:       map[string]dnfrepo.ItemStack{},
		Warehouse:   map[string]dnfrepo.ItemStack{},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repos.Equipment.Save(ctx, dnfrepo.EquipmentRecord{
		CharacterID: "77",
		Entries: map[string]dnfrepo.EquipmentEntry{
			"26": {
				SlotIndex: 26,
				ItemID:    int64(itemID),
				RawEntry:  testCurrentEquippedCreatureRaw(26, itemID, serialOrHandle),
			},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := saveSkillSwitchInventoryState(ctx, repos.Settings, "77", skillSwitchInventoryState{SkillID: 123}); err != nil {
		t.Fatal(err)
	}
	service := &Service{repositoryProvider: func() (dnfrepo.Group, bool) { return repos, true }}
	character := dnfrepo.CharacterRecord{CharacterID: "77", Level: 90}
	selected := service.buildCurrentActorBindingMode1BodyForSelectedWithEquipmentInContext(
		ctx, &gameSession{selectedCharacterID: 77}, character, true, 77, 0, currentSceneObjectContext, false,
	)
	other := service.buildCurrentActorBindingMode1BodyForSelectedWithEquipmentInContext(
		ctx, &gameSession{selectedCharacterID: 78}, character, true, 77, 0, currentSceneObjectContext, false,
	)
	if !bytes.Equal(selected, other) {
		t.Fatalf("mode1 embedded skill-switch state: selected=%x other=%x", selected, other)
	}
	creatureOffset := currentMode1CreateRowsOffset + 4 + 2 + 1
	if got := binary.LittleEndian.Uint32(selected[creatureOffset : creatureOffset+4]); got != itemID {
		t.Fatalf("mode1 creature item=%#x want=%#x body=%x", got, itemID, selected)
	}
	if got := binary.LittleEndian.Uint32(selected[creatureOffset+4 : creatureOffset+8]); got != serialOrHandle {
		t.Fatalf("mode1 creature serial=%#x want=%#x body=%x", got, serialOrHandle, selected)
	}
	if got := selected[creatureOffset+8]; got != 0xff {
		t.Fatalf("mode1 resource begins at %d with %#x, want 0xff", creatureOffset+8, got)
	}
}

func TestLegacyType1345RefreshesSkillSwitchInventoryAllDataAfterSelectedActorIsReady(t *testing.T) {
	service, session, connection, _ := newLegacyTownSceneReadySkillTest(t)
	repositories, ok := service.repositoryGroup()
	if !ok {
		t.Fatal("repository group unavailable")
	}
	ctx := context.Background()
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "29",
		Slots: map[string]dnfrepo.ItemStack{
			"0:17": {ItemID: 101010864, Count: 1, Extra: map[string]string{"item_serial": "20"}},
		},
		Warehouse: map[string]dnfrepo.ItemStack{},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Equipment.Save(ctx, dnfrepo.EquipmentRecord{CharacterID: "29"}); err != nil {
		t.Fatal(err)
	}
	if err := saveSkillSwitchInventoryState(ctx, repositories.Settings, "29", skillSwitchInventoryState{
		SkillID: 40,
		Items:   []skillSwitchBinding{{EquipmentPart: 12, ItemSerial: 20, ItemID: 101010864}},
	}); err != nil {
		t.Fatal(err)
	}

	if err := service.handleLegacyGamePacket(session, buildLegacyTownSceneReadyAckPacket()); err != nil {
		t.Fatal(err)
	}
	packet, rest := splitCurrentGameServerUpperPacketAuto(t, connection.write.Bytes())
	if packet.Header.Classification != 0 || packet.Header.MsgID != skillSwitchInventoryAllDataWire {
		t.Fatalf("initial BUFF refresh header=%+v body=%x", packet.Header, packet.Body)
	}
	want := []byte{40, 0, 1, 1, 12, 0, 17, 0}
	if !bytes.Equal(packet.Body, want) {
		t.Fatalf("initial BUFF refresh body=%x want=%x", packet.Body, want)
	}
	if len(rest) != 0 {
		t.Fatalf("unexpected packet tail=%x", rest)
	}

	connection.write.Reset()
	if err := service.handleLegacyGamePacket(session, buildLegacyTownSceneReadyAckPacket()); err != nil {
		t.Fatal(err)
	}
	if connection.write.Len() != 0 {
		t.Fatalf("repeated scene-ready replayed BUFF refresh=%x", connection.write.Bytes())
	}
}

func TestLegacyType1345HydratesRegisteredChronicleItemBeforeBuffAllData(t *testing.T) {
	service, session, connection, _ := newLegacyTownSceneReadySkillTest(t)
	repositories, ok := service.repositoryGroup()
	if !ok {
		t.Fatal("repository group unavailable")
	}
	ctx := context.Background()
	raw := make([]byte, currentItemListEntryWireSize)
	binary.LittleEndian.PutUint16(raw[0:2], 17)
	binary.LittleEndian.PutUint32(raw[2:6], 101010864)
	raw[0x27] = 1
	binary.LittleEndian.PutUint32(raw[0x28:0x2c], currentChronicleCanonicalAuraBase+2)
	raw[0x30] = 11
	raw[0x32] = 1
	raw[0x34] = 12
	raw[0x36] = 14
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "29",
		Slots: map[string]dnfrepo.ItemStack{
			"0:17": {
				ItemID:   101010864,
				Count:    1,
				RawEntry: raw,
				Extra:    map[string]string{"item_serial": "20", "middle_data_1a": "01e8040000000000000b0001000c000e00"},
			},
		},
		Warehouse: map[string]dnfrepo.ItemStack{},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Equipment.Save(ctx, dnfrepo.EquipmentRecord{CharacterID: "29"}); err != nil {
		t.Fatal(err)
	}
	if err := saveSkillSwitchInventoryState(ctx, repositories.Settings, "29", skillSwitchInventoryState{
		SkillID: 40,
		Items:   []skillSwitchBinding{{EquipmentPart: 12, ItemSerial: 20, ItemID: 101010864}},
	}); err != nil {
		t.Fatal(err)
	}

	if err := service.handleLegacyGamePacket(session, buildLegacyTownSceneReadyAckPacket()); err != nil {
		t.Fatal(err)
	}
	itemRefresh, rest := splitCurrentGameServerUpperPacketAuto(t, connection.write.Bytes())
	if itemRefresh.Header.Classification != 0 ||
		itemRefresh.Header.MsgID != uint16(dnfenum.CmdPacketWalkoutPartyMember) ||
		len(itemRefresh.Body) != 3+currentItemListEntryWireSize || itemRefresh.Body[0] != 0 ||
		binary.LittleEndian.Uint16(itemRefresh.Body[1:3]) != 1 {
		t.Fatalf("Chronicle BUFF item refresh header=%+v body=%x", itemRefresh.Header, itemRefresh.Body)
	}
	if got := itemRefresh.Body[3+0x27 : 3+0x38]; !bytes.Equal(got, raw[0x27:0x38]) {
		t.Fatalf("Chronicle BUFF item vector=%x want=%x", got, raw[0x27:0x38])
	}
	buff, rest := splitCurrentGameServerUpperPacketAuto(t, rest)
	if buff.Header.Classification != 0 || buff.Header.MsgID != skillSwitchInventoryAllDataWire {
		t.Fatalf("BUFF all-data header=%+v body=%x", buff.Header, buff.Body)
	}
	if want := []byte{40, 0, 1, 1, 12, 0, 17, 0}; !bytes.Equal(buff.Body, want) {
		t.Fatalf("BUFF all-data body=%x want=%x", buff.Body, want)
	}
	if len(rest) != 0 {
		t.Fatalf("unexpected Chronicle BUFF tail=%x", rest)
	}
}

func TestSkillSwitchChronicleOptionRefreshGroupsBagAndEquippedRows(t *testing.T) {
	service, session, connection, _ := newLegacyTownSceneReadySkillTest(t)
	makeChronicleRaw := func(slot uint16, itemID uint32, auraID uint32) [currentItemListEntryWireSize]byte {
		var raw [currentItemListEntryWireSize]byte
		binary.LittleEndian.PutUint16(raw[0:2], slot)
		binary.LittleEndian.PutUint32(raw[2:6], itemID)
		raw[0x27] = 1
		binary.LittleEndian.PutUint32(raw[0x28:0x2c], auraID)
		raw[0x30] = 15
		raw[0x32] = 2
		raw[0x34] = 12
		raw[0x36] = 11
		return raw
	}
	bagRaw := makeChronicleRaw(29, 116010135, currentChronicleCanonicalAuraBase+2)
	equippedRaw := makeChronicleRaw(11, 116010136, currentChronicleCanonicalAuraBase+2)
	var ordinaryRaw [currentItemListEntryWireSize]byte
	binary.LittleEndian.PutUint16(ordinaryRaw[0:2], 30)
	binary.LittleEndian.PutUint32(ordinaryRaw[2:6], 116010137)

	rows, err := service.sendSkillSwitchChronicleOptionRefreshes(session, map[byte]skillSwitchInventoryPosition{
		11: {EquipmentPart: 11, ListType: 3, Slot: 11, ItemID: 116010136, Raw: equippedRaw},
		12: {EquipmentPart: 12, ListType: 0, Slot: 29, ItemID: 116010135, Raw: bagRaw},
		13: {EquipmentPart: 13, ListType: 0, Slot: 30, ItemID: 116010137, Raw: ordinaryRaw},
	}, "test_mixed_registered_chronicle_rows")
	if err != nil {
		t.Fatal(err)
	}
	if rows != 2 {
		t.Fatalf("Chronicle BUFF refresh rows=%d want=2", rows)
	}

	bagPacket, rest := splitCurrentGameServerUpperPacketAuto(t, connection.write.Bytes())
	if bagPacket.Header.Classification != 0 || bagPacket.Header.MsgID != uint16(dnfenum.CmdPacketWalkoutPartyMember) ||
		len(bagPacket.Body) != 3+currentItemListEntryWireSize || bagPacket.Body[0] != 0 ||
		binary.LittleEndian.Uint16(bagPacket.Body[1:3]) != 1 ||
		!bytes.Equal(bagPacket.Body[3:], bagRaw[:]) {
		t.Fatalf("bag Chronicle refresh header=%+v body=%x", bagPacket.Header, bagPacket.Body)
	}
	equippedPacket, rest := splitCurrentGameServerUpperPacketAuto(t, rest)
	if equippedPacket.Header.Classification != 0 || equippedPacket.Header.MsgID != uint16(dnfenum.CmdPacketWalkoutPartyMember) ||
		len(equippedPacket.Body) != 3+currentItemListEntryWireSize || equippedPacket.Body[0] != 3 ||
		binary.LittleEndian.Uint16(equippedPacket.Body[1:3]) != 1 ||
		!bytes.Equal(equippedPacket.Body[3:], equippedRaw[:]) {
		t.Fatalf("equipped Chronicle refresh header=%+v body=%x", equippedPacket.Header, equippedPacket.Body)
	}
	if len(rest) != 0 {
		t.Fatalf("unexpected mixed Chronicle refresh tail=%x", rest)
	}
}

func TestLegacyType1345ClearsPriorSkillSwitchInventoryWithEmptyAllData(t *testing.T) {
	service, session, connection, _ := newLegacyTownSceneReadySkillTest(t)

	if err := service.handleLegacyGamePacket(session, buildLegacyTownSceneReadyAckPacket()); err != nil {
		t.Fatal(err)
	}
	packet, rest := splitCurrentGameServerUpperPacketAuto(t, connection.write.Bytes())
	if packet.Header.Classification != 0 || packet.Header.MsgID != skillSwitchInventoryAllDataWire {
		t.Fatalf("empty BUFF refresh header=%+v body=%x", packet.Header, packet.Body)
	}
	if want := []byte{0, 0, 0}; !bytes.Equal(packet.Body, want) {
		t.Fatalf("empty BUFF refresh body=%x want=%x", packet.Body, want)
	}
	if len(rest) != 0 {
		t.Fatalf("unexpected packet tail=%x", rest)
	}
}

func TestDprotoFallbackRefreshesSkillSwitchInventoryWhenLegacySceneReadyIsAbsent(t *testing.T) {
	service, session, connection, _ := newLegacyTownSceneReadySkillTest(t)
	repositories, ok := service.repositoryGroup()
	if !ok {
		t.Fatal("repository group unavailable")
	}
	ctx := context.Background()
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "29",
		Slots: map[string]dnfrepo.ItemStack{
			"0:17": {ItemID: 101010864, Count: 1, Extra: map[string]string{"item_serial": "20"}},
		},
		Warehouse: map[string]dnfrepo.ItemStack{},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Equipment.Save(ctx, dnfrepo.EquipmentRecord{CharacterID: "29"}); err != nil {
		t.Fatal(err)
	}
	if err := saveSkillSwitchInventoryState(ctx, repositories.Settings, "29", skillSwitchInventoryState{
		SkillID: 40,
		Items:   []skillSwitchBinding{{EquipmentPart: 12, ItemSerial: 20, ItemID: 101010864}},
	}); err != nil {
		t.Fatal(err)
	}

	// Live current-client traffic can continue with DPROTO callbacks after op24
	// without sending legacy type1345. Keep the tail deferred, as type1345
	// remains its only verified owner, but initialize the independent BUFF UI.
	session.initialTownLegacySceneReadyAccepted = false
	session.sceneBootstrapTailDeferred = true
	session.sceneBootstrapTailSent = false
	resetCurrentDeferredSelectSceneTailProgress(session)
	if err := service.sendCurrentSelectSceneReadyBoundary(session, "upper_dproto_callback_after_ack"); err != nil {
		t.Fatal(err)
	}
	packet, rest := splitCurrentGameServerUpperPacketAuto(t, connection.write.Bytes())
	if packet.Header.Classification != 0 || packet.Header.MsgID != skillSwitchInventoryAllDataWire {
		t.Fatalf("DPROTO BUFF fallback header=%+v body=%x", packet.Header, packet.Body)
	}
	want := []byte{40, 0, 1, 1, 12, 0, 17, 0}
	if !bytes.Equal(packet.Body, want) {
		t.Fatalf("DPROTO BUFF fallback body=%x want=%x", packet.Body, want)
	}
	if len(rest) != 0 {
		t.Fatalf("DPROTO BUFF fallback tail=%x", rest)
	}
	if !session.sceneBootstrapTailDeferred || session.sceneBootstrapTailSent {
		t.Fatalf("DPROTO fallback altered legacy tail state deferred=%t sent=%t", session.sceneBootstrapTailDeferred, session.sceneBootstrapTailSent)
	}

	connection.write.Reset()
	if err := service.sendCurrentSelectSceneReadyBoundary(session, "upper_dproto_callback_after_ack"); err != nil {
		t.Fatal(err)
	}
	if connection.write.Len() != 0 {
		t.Fatalf("repeated DPROTO callback replayed BUFF refresh=%x", connection.write.Bytes())
	}
}

func skillSwitchInventoryTestPVF() dungeonDropCatalogTestSource {
	return dungeonDropCatalogTestSource{
		"monster/monster.lst":                  "",
		"stackable/stackable.lst":              "",
		"equipment/equipment.lst":              "9001 `creature/test_creature.equ`\n",
		"equipment/creature/test_creature.equ": "[equipment type]\n`[creature]`\n",
	}
}
