package dnfbridge

import (
	"context"
	"encoding/binary"
	"testing"
	"time"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	"longheng.io/server/internal/modules/dnf/worldmap"
)

func TestCurrentDungeonDurabilitySlotAllowlist(t *testing.T) {
	allowed := map[int16]bool{12: true, 14: true, 15: true, 16: true, 17: true, 18: true}
	for slot := int16(0); slot <= 255; slot++ {
		if got := isCurrentDungeonDurabilitySlot(slot); got != allowed[slot] {
			t.Fatalf("slot %d allowed=%t, want %t", slot, got, allowed[slot])
		}
	}
}

func TestDungeonDurabilityPersistsAndRefreshesOneEquipmentSlot(t *testing.T) {
	const (
		characterID = "77"
		itemID      = int64(10018)
		slot        = int16(12)
	)

	repos := testRepositoryGroup()
	raw := make([]byte, currentItemListEntryWireSize)
	binary.LittleEndian.PutUint32(raw[2:6], uint32(itemID))
	binary.LittleEndian.PutUint16(raw[0x0B:0x0D], 45)
	if err := repos.Equipment.Save(context.Background(), dnfrepo.EquipmentRecord{
		CharacterID: characterID,
		Entries: map[string]dnfrepo.EquipmentEntry{
			"12": {SlotIndex: slot, ItemID: itemID, RawEntry: raw},
		},
		UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("save equipment: %v", err)
	}

	service := &Service{
		repositoryProvider: func() (dnfrepo.Group, bool) { return repos, true },
		repairCostCatalog: &currentRepairCostCatalog{
			evidence: map[int64]alignedcmd.RepairCostEvidence{
				itemID: {MaxDurability: 45},
			},
		},
	}
	connection := &bufferConn{}
	session := &gameSession{
		conn:                connection,
		connID:              "dungeon-durability-regression",
		selectedCharacterID: 77,
		dungeon: dungeonSessionState{runtime: &runtimeDungeonState{
			Character: dnfrepo.CharacterRecord{CharacterID: characterID},
			Session:   &worldmap.DungeonSession{},
			Room:      &runtimeDungeonRoom{},
		}},
	}

	if err := service.handleGameCommand(
		session,
		byte(dnfenum.GameCmdCommand),
		uint16(dnfenum.CmdPacketDecreaseDurability),
		[]byte{byte(slot)},
	); err != nil {
		t.Fatalf("handle op48: %v", err)
	}
	packet, rest := splitCurrentGameServerUpperPacketAuto(t, connection.write.Bytes())
	if len(rest) != 0 {
		t.Fatalf("op48 emitted trailing packets=%x", rest)
	}
	if packet.Header.Classification != 0 || packet.Header.MsgID != uint16(dnfenum.CmdPacketWalkoutPartyMember) {
		t.Fatalf("op48 refresh header class=%d msg=%d", packet.Header.Classification, packet.Header.MsgID)
	}
	if len(packet.Body) != 3+currentItemListEntryWireSize ||
		packet.Body[0] != currentSocketListEquipment ||
		binary.LittleEndian.Uint16(packet.Body[1:3]) != 1 {
		t.Fatalf("op48 refresh body=%x", packet.Body)
	}
	if got := int16(binary.LittleEndian.Uint16(packet.Body[3:5])); got != slot {
		t.Fatalf("op48 refresh slot=%d, want %d", got, slot)
	}
	if got := binary.LittleEndian.Uint32(packet.Body[5:9]); got != uint32(itemID) {
		t.Fatalf("op48 refresh item=%d, want %d", got, itemID)
	}
	if got := binary.LittleEndian.Uint16(packet.Body[3+0x0B : 3+0x0D]); got != 44 {
		t.Fatalf("op48 refresh durability=%d, want 44", got)
	}
	loaded, found, err := repos.Equipment.Load(context.Background(), characterID)
	if err != nil || !found {
		t.Fatalf("load equipment found=%t err=%v", found, err)
	}
	entry := loaded.Entries["12"]
	if got := binary.LittleEndian.Uint16(entry.RawEntry[0x0B:0x0D]); got != 44 {
		t.Fatalf("persisted durability=%d, want 44", got)
	}
}

func TestDungeonDurabilityRejectsNonDurableEquipmentSlot(t *testing.T) {
	const (
		characterID = "78"
		itemID      = int64(10019)
		slot        = int16(13)
	)
	repos := testRepositoryGroup()
	raw := make([]byte, currentItemListEntryWireSize)
	binary.LittleEndian.PutUint32(raw[2:6], uint32(itemID))
	binary.LittleEndian.PutUint16(raw[0x0B:0x0D], 45)
	if err := repos.Equipment.Save(context.Background(), dnfrepo.EquipmentRecord{
		CharacterID: characterID,
		Entries: map[string]dnfrepo.EquipmentEntry{
			"13": {SlotIndex: slot, ItemID: itemID, RawEntry: raw},
		},
		UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("save equipment: %v", err)
	}
	service := &Service{
		repositoryProvider: func() (dnfrepo.Group, bool) { return repos, true },
		repairCostCatalog: &currentRepairCostCatalog{evidence: map[int64]alignedcmd.RepairCostEvidence{
			itemID: {MaxDurability: 45},
		}},
	}
	connection := &bufferConn{}
	session := &gameSession{
		conn:                connection,
		connID:              "dungeon-durability-rejected-slot",
		selectedCharacterID: 78,
		dungeon: dungeonSessionState{runtime: &runtimeDungeonState{
			Character: dnfrepo.CharacterRecord{CharacterID: characterID},
			Session:   &worldmap.DungeonSession{},
			Room:      &runtimeDungeonRoom{},
		}},
	}
	if err := service.handleDungeonDecreaseDurability(session, []byte{byte(slot)}); err != nil {
		t.Fatalf("handle rejected op48: %v", err)
	}
	if connection.write.Len() != 0 {
		t.Fatalf("rejected op48 emitted bytes=%d", connection.write.Len())
	}
	loaded, found, err := repos.Equipment.Load(context.Background(), characterID)
	if err != nil || !found {
		t.Fatalf("load equipment found=%t err=%v", found, err)
	}
	if got := binary.LittleEndian.Uint16(loaded.Entries["13"].RawEntry[0x0B:0x0D]); got != 45 {
		t.Fatalf("rejected slot durability=%d, want 45", got)
	}
}
