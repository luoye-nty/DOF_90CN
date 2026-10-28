package dnfbridge

import (
	"context"
	"encoding/binary"
	"fmt"
	"strings"
	"testing"

	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfpet "longheng.io/server/internal/modules/dnf/pet"
	dnfproto "longheng.io/server/internal/modules/dnf/protocol"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

func TestCurrentPetSealRequestUsesObservedTwentyOneByteWire(t *testing.T) {
	body := make([]byte, 21)
	binary.LittleEndian.PutUint16(body[13:15], 12)
	binary.LittleEndian.PutUint32(body[15:19], 63000)
	binary.LittleEndian.PutUint16(body[19:21], 44)
	request, err := decodeCurrentPetSealRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	if request.TargetSlot != 12 || request.TargetItemID != 63000 || request.MaterialSlot != 44 {
		t.Fatalf("request=%+v", request)
	}
	if _, err := decodeCurrentPetSealRequest(body[:20]); err == nil {
		t.Fatal("accepted truncated op1012 request")
	}
}

func TestCurrentPetSealRestoresEggAndConsumesCapsuleAtomically(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{CharacterID: "19", Slots: map[string]dnfrepo.ItemStack{
		"0:44": {ItemID: 2683240, Count: 1},
		"7:12": {ItemID: 63000, Count: 1, Bind: true, Extra: map[string]string{"hatched_from_item_id": "63006", "creature_key": "77"}},
	}}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Pet.Save(ctx, dnfrepo.PetRecord{CharacterID: "19", Entries: map[string]dnfrepo.PetEntry{
		"77": {PetKey: "77", CreatureKey: 77, ItemID: 63000, SourceListType: 7, SourceSlotIndex: 12, Extra: map[string]string{"hatched_from_item_id": "63006"}},
	}}); err != nil {
		t.Fatal(err)
	}
	source := petSealPVFSource()
	petCatalog, err := dnfpet.NewPVFCatalog(source)
	if err != nil {
		t.Fatal(err)
	}
	itemCatalog := &pvfDungeonDropCatalog{source: source, itemCache: map[uint32]dungeonDropItemDefinition{
		2683240: {ItemID: 2683240, Kind: dungeonDropItemStackable, PVFPath: "stackable/cash/pet_seal.stk"},
		63006:   {ItemID: 63006, Kind: dungeonDropItemEquipment, PVFPath: "equipment/creature/egg_faras.equ", EquipmentType: "[creature]"},
		63000:   {ItemID: 63000, Kind: dungeonDropItemEquipment, PVFPath: "equipment/creature/faras.equ", EquipmentType: "[creature]"},
	}}
	connection := &bufferConn{}
	service := &Service{
		options:            options{accountID: "account-1"},
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
		pvfItemCatalog:     itemCatalog,
		petCatalog:         petCatalog,
	}
	session := &gameSession{conn: connection, connID: "pet-seal-test", selectedCharacterID: 19}
	body := make([]byte, 21)
	binary.LittleEndian.PutUint16(body[13:15], 12)
	binary.LittleEndian.PutUint32(body[15:19], 63000)
	binary.LittleEndian.PutUint16(body[19:21], 44)
	if err := service.handleCurrentPetSeal(session, body); err != nil {
		t.Fatal(err)
	}

	ack, rest := splitGameServerUpperPacket(t, connection.write.Bytes())
	if ack.Header.MsgID != currentPetSealOpcode || ack.Header.Classification != dnfproto.DefaultChannelClassification || len(ack.Body) != 15 || ack.Body[0] != 1 {
		t.Fatalf("ack header=%+v body=%x", ack.Header, ack.Body)
	}
	if code := binary.LittleEndian.Uint32(ack.Body[1:5]); code != 0 {
		t.Fatalf("ack code=%d", code)
	}
	if value := binary.LittleEndian.Uint32(ack.Body[5:9]); value != uint32(12)<<16 {
		t.Fatalf("ack value=%08x", value)
	}
	if slot := binary.LittleEndian.Uint16(ack.Body[9:11]); slot != 44 || binary.LittleEndian.Uint32(ack.Body[11:15]) != 0 {
		t.Fatalf("ack material=%x", ack.Body[9:15])
	}
	materialUpdate, rest := splitGameServerUpperPacket(t, rest)
	eggUpdate, trailing := splitGameServerUpperPacket(t, rest)
	if len(trailing) != 0 || materialUpdate.Header.MsgID != uint16(dnfenum.CmdPacketWalkoutPartyMember) || eggUpdate.Header.MsgID != uint16(dnfenum.CmdPacketWalkoutPartyMember) {
		t.Fatalf("updates material=%+v egg=%+v trailing=%d", materialUpdate.Header, eggUpdate.Header, len(trailing))
	}
	if materialUpdate.Body[0] != dnfrepo.MainInventoryListType || eggUpdate.Body[0] != 7 {
		t.Fatalf("update lists material=%d egg=%d", materialUpdate.Body[0], eggUpdate.Body[0])
	}

	inventory, found, err := repositories.Inventory.Load(ctx, "19")
	if err != nil || !found {
		t.Fatalf("inventory found=%t err=%v", found, err)
	}
	if _, found := inventory.Slots["0:44"]; found {
		t.Fatal("capsule was not consumed")
	}
	egg := inventory.Slots["7:12"]
	if egg.ItemID != 63006 || egg.Count != 1 || !egg.Bind || egg.Extra["pet_sealed_from_item_id"] != "63000" {
		t.Fatalf("egg=%+v", egg)
	}
	pet, found, err := repositories.Pet.Load(ctx, "19")
	if err != nil || !found || len(pet.Entries) != 0 {
		t.Fatalf("pet found=%t err=%v record=%+v", found, err, pet)
	}
}

func TestCurrentPetSealRejectsEquippedCreatureWithoutConsumingCapsule(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{CharacterID: "19", Slots: map[string]dnfrepo.ItemStack{
		"0:44": {ItemID: 2683240, Count: 1},
		"7:12": {ItemID: 63000, Count: 1, Extra: map[string]string{"hatched_from_item_id": "63006"}},
	}}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Pet.Save(ctx, dnfrepo.PetRecord{CharacterID: "19", EquippedKey: "77", Entries: map[string]dnfrepo.PetEntry{
		"77": {PetKey: "77", ItemID: 63000, SourceListType: 7, SourceSlotIndex: 12, Extra: map[string]string{"hatched_from_item_id": "63006"}},
	}}); err != nil {
		t.Fatal(err)
	}
	source := petSealPVFSource()
	petCatalog, err := dnfpet.NewPVFCatalog(source)
	if err != nil {
		t.Fatal(err)
	}
	service := &Service{
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
		pvfItemCatalog: &pvfDungeonDropCatalog{source: source, itemCache: map[uint32]dungeonDropItemDefinition{
			2683240: {ItemID: 2683240, Kind: dungeonDropItemStackable},
		}},
		petCatalog: petCatalog,
	}
	connection := &bufferConn{}
	session := &gameSession{conn: connection, selectedCharacterID: 19}
	body := make([]byte, 21)
	binary.LittleEndian.PutUint16(body[13:15], 12)
	binary.LittleEndian.PutUint32(body[15:19], 63000)
	binary.LittleEndian.PutUint16(body[19:21], 44)
	if err := service.handleCurrentPetSeal(session, body); err != nil {
		t.Fatal(err)
	}
	inventory, _, _ := repositories.Inventory.Load(ctx, "19")
	if inventory.Slots["0:44"].Count != 1 || inventory.Slots["7:12"].ItemID != 63000 {
		t.Fatalf("rollback inventory=%+v", inventory.Slots)
	}
}

func petSealPVFSource() finishBridgePVFSource {
	var experience strings.Builder
	for index := 1; index <= dnfpet.MaxCreatureLevel; index++ {
		fmt.Fprintf(&experience, "%d\n", index*10)
	}
	return finishBridgePVFSource{
		"equipment/equipment.lst":          "63006 `creature/egg_faras.equ`\n63000 `creature/faras.equ`\n",
		"equipment/creature/egg_faras.equ": "[equipment type] `[creature]`\n[output index] 63000\n",
		"equipment/creature/faras.equ":     "[name] `Faras`\n[equipment type] `[creature]`\n[minimum level] 1\n",
		"creature/exptable.tbl":            experience.String(),
	}
}
