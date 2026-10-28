package dnfbridge

import (
	"context"
	"encoding/binary"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"longheng.io/server/internal/modules/dnf/jobmap"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

func TestCurrentAvatarFittingRoomProtocolRoundTripShape(t *testing.T) {
	body := make([]byte, currentAvatarFittingRoomRequestSize)
	for index := 0; index < currentAvatarFittingRoomPartCount; index++ {
		offset := currentAvatarFittingRoomPrefixSize + index*currentAvatarFittingRoomRecordSize
		for byteIndex := 0; byteIndex < 8; byteIndex++ {
			body[offset+byteIndex] = 0xff
		}
		body[offset+8] = byte(index)
	}
	request, err := parseCurrentAvatarFittingRoomRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	changed := [currentAvatarFittingRoomPartCount]bool{true, false, true}
	response := buildCurrentAvatarFittingRoomSuccessBody(request, changed)
	if len(response) != 1+8*13 || response[0] != 1 || response[1+9] != 1 || response[1+13+9] != 0 || response[1+2*13+9] != 1 {
		t.Fatalf("response=%x", response)
	}
}

func TestRealPVFAvatarFittingRoomConfigAndTicket(t *testing.T) {
	pvfPath := filepath.Clean(filepath.Join("..", "..", "..", "..", "runtime", "data", "dnf", "Script.pvf"))
	archive, err := platformpvf.Open(pvfPath)
	if err != nil {
		t.Skipf("runtime Script.pvf unavailable: %v", err)
	}
	config, err := loadCurrentAvatarFittingRoomConfig(archive)
	if err != nil {
		t.Fatal(err)
	}
	if config.TicketItemID != 490702492 || config.GaugeThreshold != 20 || config.GaugeRewardID != 490702493 || config.GaugeRewardCount != 1 || config.NormalRate != 90 || config.RareRate != 10 || len(config.Sets) != 10 {
		t.Fatalf("config=%+v", config)
	}
	rare, normal := 0, 0
	for _, set := range config.Sets {
		if set.Rare {
			rare++
		} else {
			normal++
		}
	}
	if rare != 2 || normal != 8 {
		t.Fatalf("sets rare=%d normal=%d", rare, normal)
	}
	catalog, err := newPVFDungeonDropCatalog(archive)
	if err != nil {
		t.Fatal(err)
	}
	service := &Service{pvfItemCatalog: catalog, automaticRobotAvatarConfig: &config}
	for job := 0; job <= jobmap.MaxID; job++ {
		set, setErr := service.automaticRobotAvatarSet(byte(job), uint64(job+1))
		if setErr != nil {
			t.Fatalf("automatic robot avatar job=%d: %v", job, setErr)
		}
		supported := false
		for _, candidate := range config.Sets {
			if _, ok := candidate.Jobs[byte(job)]; ok {
				supported = true
				break
			}
		}
		if !supported {
			for index, itemID := range set {
				if itemID != 0 {
					t.Fatalf("unsupported automatic robot avatar job=%d part=%d item=%d", job, index, itemID)
				}
			}
			continue
		}
		for index, itemID := range set {
			if itemID == 0 {
				t.Fatalf("automatic robot avatar job=%d part=%d is empty", job, index)
			}
		}
	}
	definition, err := catalog.ResolveItem(config.TicketItemID)
	if err != nil {
		t.Fatal(err)
	}
	isTicket, err := currentPVFAvatarFittingRoomTicket(archive, definition)
	if err != nil || !isTicket {
		t.Fatalf("ticket definition=%+v isTicket=%t err=%v", definition, isTicket, err)
	}
}

func TestCurrentAvatarFittingRoomCommitUsesOneAtomicEightPartTransaction(t *testing.T) {
	ctx := context.Background()
	source := avatarCompoundPVFTestSource{}
	var config strings.Builder
	config.WriteString("[ticket index]\n490702492\n[vip gauge]\n20 490702493 1\n[rate]\n100 0\n[fitting room avatar]\n0 `[swordman]`\n")
	definitions := make(map[uint32]dungeonDropItemDefinition)
	equipmentEntries := make(map[string]dnfrepo.EquipmentEntry)
	request := currentAvatarFittingRoomRequest{}
	for index, part := range currentAvatarFittingRoomPartNames {
		currentID, outputID := uint32(1000+index), uint32(2000+index)
		fmt.Fprintf(&config, "`%s` %d\n", part, outputID)
		currentPath, outputPath := fmt.Sprintf("equipment/current_%d.equ", index), fmt.Sprintf("equipment/output_%d.equ", index)
		equipmentType := fmt.Sprintf("[%s avatar]", part)
		if part == "neck" {
			equipmentType = "[breast avatar]"
		}
		if part == "belt" {
			equipmentType = "[waist avatar]"
		}
		source[currentPath] = fmt.Sprintf("[equipment type] `%s`\n", equipmentType)
		source[outputPath] = fmt.Sprintf("[equipment type] `%s`\n", equipmentType)
		definitions[currentID] = dungeonDropItemDefinition{ItemID: currentID, Kind: dungeonDropItemEquipment, PVFPath: currentPath, EquipmentType: equipmentType}
		definitions[outputID] = dungeonDropItemDefinition{ItemID: outputID, Kind: dungeonDropItemEquipment, PVFPath: outputPath, EquipmentType: equipmentType}
		raw := make([]byte, 119)
		binary.LittleEndian.PutUint32(raw[2:6], currentID)
		slot := currentAvatarFittingRoomEquipmentSlots[index]
		equipmentEntries[fmt.Sprintf("%d", slot)] = dnfrepo.EquipmentEntry{SlotIndex: slot, ItemID: int64(currentID), RawEntry: raw}
		request.Records[index].ObjectIndex = int32(3000 + index)
		request.Records[index].CurrentItemID = int32(currentID)
		request.Records[index].PartCode = byte(index)
	}
	config.WriteString("[/fitting room avatar]\n")
	source[currentAvatarFittingRoomPVFPath] = config.String()
	definitions[490702493] = dungeonDropItemDefinition{ItemID: 490702493, Kind: dungeonDropItemStackable, PVFPath: "stackable/reward.stk", StackableType: "[etc]", StackLimit: 1000, SlotStart: 9, SlotEnd: 64}
	catalog := &pvfDungeonDropCatalog{source: source, itemCache: definitions, itemRefs: make(map[uint32]dungeonDropItemReference)}
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{CharacterID: "19", AccountID: "dnf:1", Job: "0"}); err != nil {
		t.Fatal(err)
	}
	ticketRaw := make([]byte, 119)
	binary.LittleEndian.PutUint32(ticketRaw[6:10], 8)
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{CharacterID: "19", Slots: map[string]dnfrepo.ItemStack{
		"0:10": {ItemID: 490702492, Count: 8, RawEntry: ticketRaw},
	}}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Equipment.Save(ctx, dnfrepo.EquipmentRecord{CharacterID: "19", Entries: equipmentEntries}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Settings.Save(ctx, dnfrepo.SettingsRecord{Scope: "character:19:avatar_fitting_room", Values: map[string]string{"used_parts_total": "19"}}); err != nil {
		t.Fatal(err)
	}
	service := &Service{options: options{accountPrefix: defaultAccountPrefix}, repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true }}
	session := &gameSession{accountID: "dnf:1", selectedCharacterID: 19}
	result, err := service.commitCurrentAvatarFittingRoom(ctx, session, catalog, request)
	if err != nil {
		t.Fatal(err)
	}
	if result.Domain.TicketConsumed != 8 || result.Domain.RewardGranted != 1 || result.Domain.GaugeAfter != 27 {
		t.Fatalf("result=%+v", result)
	}
	for index, changed := range result.Changed {
		if !changed {
			t.Fatalf("part=%d not changed", index)
		}
	}
	equipment, _, _ := repositories.Equipment.Load(ctx, "19")
	for index, slot := range currentAvatarFittingRoomEquipmentSlots {
		entry := equipment.Entries[fmt.Sprintf("%d", slot)]
		if entry.ItemID != int64(2000+index) {
			t.Fatalf("slot=%d entry=%+v", slot, entry)
		}
	}
}
