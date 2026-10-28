package dnfbridge

import (
	"bytes"
	"context"
	"encoding/binary"
	"testing"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	"longheng.io/server/internal/modules/dnf/channelcatalog"
	dnfguardianshield "longheng.io/server/internal/modules/dnf/guardianshield"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

func TestCurrentActorExpandedEquipmentBodyEmptyGroup0AndTwoSkippedGroups(t *testing.T) {
	got := buildCurrentActorExpandedEquipmentBody(0x1234, nil, 0)
	want := []byte{
		0x34, 0x12,
		currentActorExpandedEquipmentTownGroup,
		0,
		0, 0, 0, 0,
		currentActorExpandedEquipmentSkippedGroup,
		currentActorExpandedEquipmentSkippedGroup,
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("expanded-equipment empty body=%x want=%x", got, want)
	}
}

func TestCurrentActorExpandedEquipmentBodyCarriesCompleteVerifiedRows(t *testing.T) {
	rows := []currentMode1EquipmentObjectRow{
		{
			createEnabled:           true,
			equipmentType:           10,
			equipmentTypeKnown:      true,
			itemID:                  0x11223344,
			durability:              0x5566,
			durabilityKnown:         true,
			state112:                0,
			currentItemStateDerived: true,
		},
		{
			createEnabled:           true,
			equipmentType:           12,
			equipmentTypeKnown:      true,
			itemID:                  0x778899aa,
			durability:              0xbbcc,
			durabilityKnown:         true,
			state112:                0,
			currentItemStateDerived: true,
		},
	}
	body := buildCurrentActorExpandedEquipmentBody(19, rows, 0x44332211)

	firstRowOffset := 4
	firstRowSize := currentMode1EquipmentCreateRowWireSizeFor(rows[0])
	secondRowOffset := firstRowOffset + firstRowSize
	secondRowSize := currentMode1EquipmentCreateRowWireSizeFor(rows[1])
	finalStateOffset := secondRowOffset + secondRowSize
	if got, want := len(body), finalStateOffset+4+2; got != want {
		t.Fatalf("expanded-equipment body len=%d want=%d body=%x", got, want, body)
	}
	if binary.LittleEndian.Uint16(body[0:2]) != 19 ||
		body[2] != currentActorExpandedEquipmentTownGroup ||
		body[3] != 2 {
		t.Fatalf("expanded-equipment header=%x", body[:4])
	}
	if body[firstRowOffset] != 10 ||
		binary.LittleEndian.Uint32(body[firstRowOffset+1:firstRowOffset+5]) != 0x11223344 {
		t.Fatalf("expanded-equipment first row=%x", body[firstRowOffset:secondRowOffset])
	}
	if body[secondRowOffset] != 12 ||
		binary.LittleEndian.Uint32(body[secondRowOffset+1:secondRowOffset+5]) != 0x778899aa {
		t.Fatalf("expanded-equipment second row=%x", body[secondRowOffset:finalStateOffset])
	}
	if binary.LittleEndian.Uint32(body[finalStateOffset:finalStateOffset+4]) != 0x44332211 ||
		body[finalStateOffset+4] != currentActorExpandedEquipmentSkippedGroup ||
		body[finalStateOffset+5] != currentActorExpandedEquipmentSkippedGroup {
		t.Fatalf("expanded-equipment tail=%x", body[finalStateOffset:])
	}
}

func TestCurrentActorExpandedEquipmentProjectionRejectsPartialActorState(t *testing.T) {
	record := dnfrepo.EquipmentRecord{
		CharacterID: "19",
		Entries: map[string]dnfrepo.EquipmentEntry{
			"0": {SlotIndex: 0, ItemID: 0x11223344},
		},
	}
	if _, _, err := validateCurrentActorExpandedEquipmentProjection(record, nil); err == nil {
		t.Fatal("partial expanded-equipment projection unexpectedly passed")
	}

	rows := []currentMode1EquipmentObjectRow{{
		createEnabled:           true,
		equipmentType:           0,
		equipmentTypeKnown:      true,
		itemID:                  0x11223344,
		durabilityKnown:         true,
		currentItemStateDerived: true,
	}}
	verified, unmapped, err := validateCurrentActorExpandedEquipmentProjection(record, rows)
	if err != nil || unmapped != 0 || len(verified) != 1 {
		t.Fatalf("complete expanded-equipment projection rows=%d unmapped=%d err=%v", len(verified), unmapped, err)
	}

	legacyPet := dnfrepo.EquipmentRecord{
		CharacterID: "19",
		Entries: map[string]dnfrepo.EquipmentEntry{
			"26": {SlotIndex: 26, ItemID: 9001},
		},
	}
	if _, _, err := validateCurrentActorExpandedEquipmentProjection(legacyPet, nil); err == nil {
		t.Fatal("unmapped pet equipment unexpectedly allowed an incomplete full-group refresh")
	}
}

func TestSelectedActorExpandedEquipmentRefreshSendsClass0Op342(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "19",
		AccountID:   "dnf:1",
		Name:        "actor19",
		Level:       90,
	}); err != nil {
		t.Fatal(err)
	}
	raw := make([]byte, currentItemListEntryWireSize)
	binary.LittleEndian.PutUint16(raw[0:2], 10)
	binary.LittleEndian.PutUint32(raw[2:6], 0x11223344)
	binary.LittleEndian.PutUint16(raw[0x0b:0x0d], 30)
	petRaw := make([]byte, currentItemListEntryWireSize)
	binary.LittleEndian.PutUint16(petRaw[0:2], 26)
	binary.LittleEndian.PutUint32(petRaw[2:6], 2681471)
	if err := repositories.Equipment.Save(ctx, dnfrepo.EquipmentRecord{
		CharacterID: "19",
		Entries: map[string]dnfrepo.EquipmentEntry{
			"10": {
				SlotIndex: 10,
				ItemID:    0x11223344,
				RawEntry:  raw,
				Extra: map[string]string{
					"current_exe_equipment_type": "10",
					"current_exe_runtime_move":   "1",
				},
			},
			"26": {
				SlotIndex: 26,
				ItemID:    2681471,
				RawEntry:  petRaw,
				Extra: map[string]string{
					"equipment_slot":            "26",
					"creature_serial_or_handle": "27",
				},
			},
		},
	}); err != nil {
		t.Fatal(err)
	}

	connection := &bufferConn{}
	service := &Service{
		options: options{
			accountID:          "dnf:1",
			gameUpperHeader:    gameUpperHeaderServer16,
			gameUpperBodyCodec: gameUpperBodyCodecPlain,
		},
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
	}
	session := &gameSession{
		conn:                connection,
		connID:              "expanded-equipment",
		channel:             channelcatalog.Channel{ID: 19},
		selectedCharacterID: 19,
	}
	if err := service.sendSelectedActorExpandedEquipmentRefresh(session, "test"); err != nil {
		t.Fatal(err)
	}

	packet, trailing := splitLongHengGameServerUpperPacket(t, connection.write.Bytes())
	if packet.Header.MsgID != currentActorExpandedEquipmentMsgID ||
		packet.Header.Classification != 0 ||
		len(trailing) != 0 {
		t.Fatalf("expanded-equipment packet header=%+v trailing=%x", packet.Header, trailing)
	}
	if binary.LittleEndian.Uint16(packet.Body[0:2]) != 19 ||
		packet.Body[2] != currentActorExpandedEquipmentTownGroup ||
		packet.Body[3] != 2 {
		t.Fatalf("expanded-equipment packet body header=%x", packet.Body[:4])
	}
	if packet.Body[4] != 10 ||
		binary.LittleEndian.Uint32(packet.Body[5:9]) != 0x11223344 {
		t.Fatalf("expanded-equipment packet first row=%x", packet.Body[4:])
	}
	reader := csharpLegacyUserInfoReader{
		ctx:         ctx,
		characterID: "19",
		service:     service,
		session:     session,
	}
	createRows := currentMode1EquipmentCreateRows(reader.currentMode1EquipmentObjectRows())
	petRowOffset := 4 + currentMode1EquipmentCreateRowWireSizeFor(createRows[0])
	if packet.Body[petRowOffset] != 26 ||
		binary.LittleEndian.Uint32(packet.Body[petRowOffset+1:petRowOffset+5]) != 2681471 {
		t.Fatalf("expanded-equipment packet pet row=%x", packet.Body[petRowOffset:])
	}
	if packet.Body[len(packet.Body)-2] != currentActorExpandedEquipmentSkippedGroup ||
		packet.Body[len(packet.Body)-1] != currentActorExpandedEquipmentSkippedGroup {
		t.Fatalf("expanded-equipment packet tail=%x", packet.Body[len(packet.Body)-6:])
	}
}

func TestSelectedActorExpandedEquipmentRefreshFailsClosedForOutOfRangeActorSlot(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "19",
		AccountID:   "dnf:1",
		Name:        "actor19",
		Level:       90,
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Equipment.Save(ctx, dnfrepo.EquipmentRecord{
		CharacterID: "19",
		Entries: map[string]dnfrepo.EquipmentEntry{
			"10": {
				SlotIndex: 10,
				ItemID:    0x11223344,
				Extra: map[string]string{
					"current_exe_equipment_type": "33",
					"current_exe_runtime_move":   "1",
				},
			},
		},
	}); err != nil {
		t.Fatal(err)
	}

	connection := &bufferConn{}
	service := &Service{
		options: options{
			accountID:          "dnf:1",
			gameUpperHeader:    gameUpperHeaderServer16,
			gameUpperBodyCodec: gameUpperBodyCodecPlain,
		},
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
	}
	session := &gameSession{
		conn:                connection,
		connID:              "expanded-equipment-malformed",
		channel:             channelcatalog.Channel{ID: 19},
		selectedCharacterID: 19,
	}
	if err := service.sendSelectedActorExpandedEquipmentRefresh(session, "test"); err == nil {
		t.Fatal("out-of-range expanded-equipment row unexpectedly sent")
	}
	if connection.write.Len() != 0 {
		t.Fatalf("out-of-range expanded-equipment wrote %d bytes", connection.write.Len())
	}
}

func TestSelectedActorExpandedEquipmentRefreshProjectsGuardianMainShieldEquipAndUnequip(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "19",
		AccountID:   "dnf:1",
		Name:        "guardian19",
		Job:         "12",
		Level:       90,
		Stats:       map[string]int64{"grow_type": 1},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{CharacterID: "19", Slots: map[string]dnfrepo.ItemStack{}}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Equipment.Save(ctx, dnfrepo.EquipmentRecord{CharacterID: "19", Entries: map[string]dnfrepo.EquipmentEntry{}}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Quest.Save(ctx, dnfrepo.QuestRecord{
		CharacterID: "19",
		States:      map[int64]dnfrepo.QuestState{9001: {Status: "completed"}},
	}); err != nil {
		t.Fatal(err)
	}
	resolver := func(_ context.Context, itemID uint32, growType byte) (alignedcmd.GuardianShieldResolution, error) {
		return alignedcmd.GuardianShieldResolution{
			ItemID:        itemID,
			PVFPath:       "equipment/character/knight/weapon/shield/test.equ",
			EquipmentType: "[support weapon]",
			UnlockKind:    alignedcmd.GuardianShieldUnlockQuest,
			ClearQuestID:  9001,
		}, nil
	}
	owner, err := dnfguardianshield.NewOwner(repositories, resolver)
	if err != nil {
		t.Fatal(err)
	}
	const shieldItemID uint32 = 113370003
	moveResult, err := owner.Move(ctx, dnfguardianshield.MoveCommand{
		CharacterID:          19,
		AccountID:            "dnf:1",
		SourceListType:       dnfguardianshield.WireListCatalog,
		SourceSlotIndex:      0,
		SourceShieldItemID:   shieldItemID,
		DestinationListType:  dnfguardianshield.WireListEquipped,
		DestinationSlotIndex: dnfguardianshield.MainDeckSlot,
	})
	if err != nil || !moveResult.MainChanged {
		t.Fatalf("equip guardian shield result=%+v err=%v", moveResult, err)
	}

	service := &Service{
		options: options{
			accountID:          "dnf:1",
			gameUpperHeader:    gameUpperHeaderServer16,
			gameUpperBodyCodec: gameUpperBodyCodecPlain,
		},
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
	}
	connection := &bufferConn{}
	session := &gameSession{
		conn:                connection,
		connID:              "guardian-shield-expanded-equipment",
		channel:             channelcatalog.Channel{ID: 19},
		selectedCharacterID: 19,
	}
	refreshed, err := service.sendSelectedGuardianShieldTownAppearanceRefreshOnce(session, "guardian_shield_equip_test")
	if err != nil {
		t.Fatal(err)
	}
	if !refreshed || !session.initialTownGuardianShieldAppearanceSent {
		t.Fatalf("guardian shield town refresh refreshed=%t gate=%t", refreshed, session.initialTownGuardianShieldAppearanceSent)
	}
	equipped, rest := splitLongHengGameServerUpperPacket(t, connection.write.Bytes())
	if equipped.Header.MsgID != currentActorExpandedEquipmentMsgID ||
		equipped.Header.Classification != 0 ||
		len(equipped.Body) < 9 || equipped.Body[3] != 1 ||
		equipped.Body[4] != byte(dnfguardianshield.ActorSlot) ||
		binary.LittleEndian.Uint32(equipped.Body[5:9]) != shieldItemID {
		t.Fatalf("guardian shield equipped op342 header=%+v body=%x rest=%x", equipped.Header, equipped.Body, rest)
	}
	redraw, trailing := splitLongHengGameServerUpperPacket(t, rest)
	if redraw.Header.MsgID != currentActorUserStateMsgID || redraw.Header.Classification != 0 ||
		!bytes.Equal(redraw.Body, buildCurrentActorUserStateBody(19, currentActorDefaultUserStateBits)) || len(trailing) != 0 {
		t.Fatalf("guardian shield equipped op357 header=%+v body=%x trailing=%x", redraw.Header, redraw.Body, trailing)
	}
	beforeReplay := connection.write.Len()
	refreshed, err = service.sendSelectedGuardianShieldTownAppearanceRefreshOnce(session, "guardian_shield_equip_replay_test")
	if err != nil || refreshed || connection.write.Len() != beforeReplay {
		t.Fatalf("guardian shield generation replay refreshed=%t bytes=%d/%d err=%v", refreshed, connection.write.Len(), beforeReplay, err)
	}

	moveResult, err = owner.Move(ctx, dnfguardianshield.MoveCommand{
		CharacterID:          19,
		AccountID:            "dnf:1",
		SourceListType:       dnfguardianshield.WireListEquipped,
		SourceSlotIndex:      dnfguardianshield.MainDeckSlot,
		DestinationListType:  dnfguardianshield.WireListCatalog,
		DestinationSlotIndex: dnfguardianshield.DeckSlotCount,
	})
	if err != nil || !moveResult.MainChanged {
		t.Fatalf("unequip guardian shield result=%+v err=%v", moveResult, err)
	}
	connection = &bufferConn{}
	session.conn = connection
	if err := service.sendSelectedActorExpandedEquipmentRefresh(session, "guardian_shield_unequip_test"); err != nil {
		t.Fatal(err)
	}
	unequipped, trailing := splitLongHengGameServerUpperPacket(t, connection.write.Bytes())
	if unequipped.Header.MsgID != currentActorExpandedEquipmentMsgID ||
		unequipped.Header.Classification != 0 || len(trailing) != 0 ||
		len(unequipped.Body) < 4 || unequipped.Body[3] != 0 {
		t.Fatalf("guardian shield unequipped op342 header=%+v body=%x trailing=%x", unequipped.Header, unequipped.Body, trailing)
	}
}
