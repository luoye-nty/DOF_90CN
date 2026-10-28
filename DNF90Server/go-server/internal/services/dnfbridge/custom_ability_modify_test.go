package dnfbridge

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"os"
	"testing"

	dnfenum "longheng.io/server/internal/modules/dnf/dnfenum"
	dnfproto "longheng.io/server/internal/modules/dnf/protocol"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	"longheng.io/server/internal/modules/dnf/repository/memory"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

func TestDecodeCurrentCustomAbilityModifyRequest(t *testing.T) {
	request, err := decodeCurrentCustomAbilityModifyRequest([]byte{9, 0, 1, 1})
	if err != nil || request.targetSlot != 9 || request.optionPosition != 1 || request.newOptionID != 1 {
		t.Fatalf("request=%+v err=%v", request, err)
	}
	for _, body := range [][]byte{
		nil,
		{9, 0, 1},
		{9, 0, 1, 1, 0},
		{9, 0, 2, 1},
		{9, 0, 1, 0},
		{0, 0x80, 1, 1},
	} {
		if _, err := decodeCurrentCustomAbilityModifyRequest(body); !errors.Is(err, errCurrentCustomAbilityModifyInvalid) {
			t.Fatalf("invalid body=%x err=%v", body, err)
		}
	}
}

func TestCommitCurrentCustomAbilityModifyConsumesAcrossStacksAndPersistsSelectedOption(t *testing.T) {
	repositories := memory.NewMemoryGroup()
	catalog := &currentCustomAbilityCatalog{
		optionIDs: map[byte]struct{}{1: {}, 4: {}, 12: {}},
		modifyMaterial: map[byte]map[byte]currentCustomAbilityMaterialCost{
			1: {3: {itemID: 10161533, count: 96}},
		},
	}
	raw := make([]byte, currentItemListEntryWireSize)
	binary.LittleEndian.PutUint16(raw[0:2], 9)
	binary.LittleEndian.PutUint32(raw[2:6], 100130573)
	raw[currentCustomAbilityOption0Offset] = 12
	raw[currentCustomAbilityOption1Offset] = 4
	raw[currentCustomAbilitySetRecordOffset] = 7
	raw[currentCustomAbilityGradeOffset] = 3
	if err := repositories.Inventory.Save(context.Background(), dnfrepo.InventoryRecord{
		CharacterID: "4",
		Slots: map[string]dnfrepo.ItemStack{
			"0:9":   {ItemID: 100130573, Count: 1, RawEntry: raw},
			"0:151": {ItemID: 10161533, Count: 50},
			"0:152": {ItemID: 10161533, Count: 60},
		},
	}); err != nil {
		t.Fatal(err)
	}

	request := currentCustomAbilityModifyRequest{targetSlot: 9, optionPosition: 1, newOptionID: 1}
	result, err := commitCurrentCustomAbilityModify(context.Background(), repositories, "4", request, 100130573, catalog)
	if err != nil {
		t.Fatal(err)
	}
	if result.oldOptionID != 4 || result.newOptionID != 1 || result.grade != 3 || result.materialRemaining != 14 || result.idempotent {
		t.Fatalf("modify result=%+v", result)
	}
	if len(result.refreshes) != 3 || result.refreshes[0].SlotIndex != 9 || result.refreshes[1].SlotIndex != 151 || result.refreshes[2].SlotIndex != 152 {
		t.Fatalf("refreshes=%+v", result.refreshes)
	}
	stored, _, err := repositories.Inventory.Load(context.Background(), "4")
	if err != nil {
		t.Fatal(err)
	}
	storedRaw := stored.Slots["0:9"].RawEntry
	if storedRaw[currentCustomAbilityOption0Offset] != 12 || storedRaw[currentCustomAbilityOption1Offset] != 1 ||
		storedRaw[currentCustomAbilitySetRecordOffset] != 7 || storedRaw[currentCustomAbilityGradeOffset] != 3 {
		t.Fatalf("persisted state=%x", storedRaw[currentCustomAbilityRawBlockStart:currentCustomAbilityRawBlockEnd])
	}
	if _, exists := stored.Slots["0:151"]; exists || stored.Slots["0:152"].Count != 14 {
		t.Fatalf("material slots=%+v", stored.Slots)
	}

	replayed, err := commitCurrentCustomAbilityModify(context.Background(), repositories, "4", request, 100130573, catalog)
	if err != nil || !replayed.idempotent || len(replayed.refreshes) != 1 || replayed.refreshes[0].SlotIndex != 9 {
		t.Fatalf("idempotent replay=%+v err=%v", replayed, err)
	}
	stored, _, _ = repositories.Inventory.Load(context.Background(), "4")
	if stored.Slots["0:152"].Count != 14 {
		t.Fatalf("idempotent replay consumed material: %+v", stored.Slots["0:152"])
	}
}

func TestCommitCurrentCustomAbilityModifyMaterialFailureRollsBack(t *testing.T) {
	repositories := memory.NewMemoryGroup()
	catalog := &currentCustomAbilityCatalog{
		optionIDs: map[byte]struct{}{1: {}, 4: {}, 12: {}},
		modifyMaterial: map[byte]map[byte]currentCustomAbilityMaterialCost{
			1: {3: {itemID: 10161533, count: 96}},
		},
	}
	raw := make([]byte, currentItemListEntryWireSize)
	raw[currentCustomAbilityOption0Offset] = 12
	raw[currentCustomAbilityOption1Offset] = 4
	raw[currentCustomAbilitySetRecordOffset] = 7
	raw[currentCustomAbilityGradeOffset] = 3
	seed := dnfrepo.InventoryRecord{CharacterID: "4", Slots: map[string]dnfrepo.ItemStack{
		"0:9":   {ItemID: 100130573, Count: 1, RawEntry: raw},
		"0:151": {ItemID: 10161533, Count: 95},
	}}
	if err := repositories.Inventory.Save(context.Background(), seed); err != nil {
		t.Fatal(err)
	}
	request := currentCustomAbilityModifyRequest{targetSlot: 9, optionPosition: 1, newOptionID: 1}
	_, err := commitCurrentCustomAbilityModify(context.Background(), repositories, "4", request, 100130573, catalog)
	if !errors.Is(err, errCurrentCustomAbilityModifyMaterial) {
		t.Fatalf("modify error=%v", err)
	}
	stored, _, _ := repositories.Inventory.Load(context.Background(), "4")
	if stored.Slots["0:151"].Count != 95 || stored.Slots["0:9"].RawEntry[currentCustomAbilityOption1Offset] != 4 {
		t.Fatalf("failed modify mutated inventory=%+v", stored.Slots)
	}
	if got := buildCurrentCustomAbilityModifyFailureBody(err); len(got) != 2 || got[0] != 0 || got[1] != 8 {
		t.Fatalf("material failure body=%x", got)
	}
}

func TestCommitCurrentCustomAbilityModifyHandlesTwoSequentialOptionRequests(t *testing.T) {
	repositories := memory.NewMemoryGroup()
	catalog := &currentCustomAbilityCatalog{
		optionIDs: map[byte]struct{}{1: {}, 4: {}, 11: {}, 12: {}},
		modifyMaterial: map[byte]map[byte]currentCustomAbilityMaterialCost{
			1:  {1: {itemID: 10161533, count: 40}},
			11: {1: {itemID: 10161534, count: 36}},
		},
	}
	raw := make([]byte, currentItemListEntryWireSize)
	raw[currentCustomAbilityOption0Offset] = 12
	raw[currentCustomAbilityOption1Offset] = 4
	raw[currentCustomAbilityGradeOffset] = 1
	if err := repositories.Inventory.Save(context.Background(), dnfrepo.InventoryRecord{
		CharacterID: "4",
		Slots: map[string]dnfrepo.ItemStack{
			"0:9":   {ItemID: 100130573, Count: 1, RawEntry: raw},
			"0:151": {ItemID: 10161533, Count: 50},
			"0:152": {ItemID: 10161534, Count: 50},
		},
	}); err != nil {
		t.Fatal(err)
	}

	requests := []currentCustomAbilityModifyRequest{
		{targetSlot: 9, optionPosition: 0, newOptionID: 1},
		{targetSlot: 9, optionPosition: 1, newOptionID: 11},
	}
	for _, request := range requests {
		if _, err := commitCurrentCustomAbilityModify(context.Background(), repositories, "4", request, 100130573, catalog); err != nil {
			t.Fatalf("request=%+v err=%v", request, err)
		}
	}
	stored, _, err := repositories.Inventory.Load(context.Background(), "4")
	if err != nil {
		t.Fatal(err)
	}
	storedRaw := stored.Slots["0:9"].RawEntry
	if storedRaw[currentCustomAbilityOption0Offset] != 1 || storedRaw[currentCustomAbilityOption1Offset] != 11 || storedRaw[currentCustomAbilityGradeOffset] != 1 {
		t.Fatalf("sequential state=%x", storedRaw[currentCustomAbilityRawBlockStart:currentCustomAbilityRawBlockEnd])
	}
	if stored.Slots["0:151"].Count != 10 || stored.Slots["0:152"].Count != 14 {
		t.Fatalf("sequential materials=%+v", stored.Slots)
	}
}

func TestCurrentCustomAbilityModifyResponseBodiesMatchClass1Envelope(t *testing.T) {
	if got := buildCurrentCustomAbilityModifySuccessBody(); len(got) != 1 || got[0] != 1 {
		t.Fatalf("success body=%x", got)
	}
	if got := buildCurrentCustomAbilityModifyFailureBody(errCurrentCustomAbilityModifyInvalid); len(got) != 2 || got[0] != 0 || got[1] != 1 {
		t.Fatalf("generic failure body=%x", got)
	}
}

func TestHandleCurrentCustomAbilityModifyRealPVFSendsRefreshBeforeSuccess(t *testing.T) {
	pvfPath := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE to verify current customability handler")
	}
	archive, err := platformpvf.Open(pvfPath)
	if err != nil {
		t.Fatal(err)
	}
	items, err := newPVFDungeonDropCatalog(archive)
	if err != nil {
		t.Fatal(err)
	}
	repositories := memory.NewMemoryGroup()
	raw := make([]byte, currentItemListEntryWireSize)
	raw[currentCustomAbilityOption0Offset] = 12
	raw[currentCustomAbilityOption1Offset] = 4
	raw[currentCustomAbilityGradeOffset] = 3
	if err := repositories.Inventory.Save(context.Background(), dnfrepo.InventoryRecord{
		CharacterID: "4",
		Slots: map[string]dnfrepo.ItemStack{
			"0:9":   {ItemID: 100130573, Count: 1, RawEntry: raw},
			"0:151": {ItemID: 10161533, Count: 96},
		},
	}); err != nil {
		t.Fatal(err)
	}
	connection := &bufferConn{}
	service := &Service{
		options:                 options{accountID: "account-1"},
		initialEquipmentArchive: archive,
		pvfItemCatalog:          items,
		repositoryProvider:      func() (dnfrepo.Group, bool) { return repositories, true },
	}
	session := &gameSession{conn: connection, connID: "custom-ability-modify-real-pvf", selectedCharacterID: 4}
	if err := service.handleCurrentCustomAbilityModify(session, []byte{9, 0, 1, 1}); err != nil {
		t.Fatal(err)
	}

	refresh, rest := splitGameServerUpperPacket(t, connection.write.Bytes())
	if refresh.Header.Classification != 0 || refresh.Header.MsgID != uint16(dnfenum.CmdPacketWalkoutPartyMember) ||
		len(refresh.Body) != 3+2*currentItemListEntryWireSize || refresh.Body[0] != dnfrepo.MainInventoryListType ||
		binary.LittleEndian.Uint16(refresh.Body[1:3]) != 2 {
		t.Fatalf("refresh header=%+v body_len=%d body_prefix=%x", refresh.Header, len(refresh.Body), refresh.Body[:minInt(len(refresh.Body), 16)])
	}
	ack, trailing := splitGameServerUpperPacket(t, rest)
	if len(trailing) != 0 || ack.Header.Classification != dnfproto.DefaultChannelClassification ||
		ack.Header.MsgID != uint16(dnfenum.CmdPacketCustomAbilityEquipOption) ||
		!bytes.Equal(ack.Body, buildCurrentCustomAbilityModifySuccessBody()) {
		t.Fatalf("ack header=%+v body=%x trailing=%d", ack.Header, ack.Body, len(trailing))
	}
	stored, _, err := repositories.Inventory.Load(context.Background(), "4")
	if err != nil {
		t.Fatal(err)
	}
	if stored.Slots["0:9"].RawEntry[currentCustomAbilityOption1Offset] != 1 {
		t.Fatalf("persisted option1=%d", stored.Slots["0:9"].RawEntry[currentCustomAbilityOption1Offset])
	}
	if _, found := stored.Slots["0:151"]; found {
		t.Fatal("consumed material slot still exists")
	}
}
