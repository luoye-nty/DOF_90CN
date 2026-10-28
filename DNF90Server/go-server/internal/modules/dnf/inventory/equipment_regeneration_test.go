package inventory

import (
	"bytes"
	"context"
	"encoding/binary"
	"testing"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

func testEquipmentRegenerationResolver() alignedcmd.EquipmentRegenerationResolver {
	return func(sourceItemID int64, mode uint16, part uint16) (alignedcmd.EquipmentRegenerationResolution, error) {
		return alignedcmd.EquipmentRegenerationResolution{
			Eligible: true, SourceItemID: sourceItemID, SourcePVFPath: "equipment/source.equ",
			ResultItemID: 2002, ResultPVFPath: "equipment/result.equ", ResultEquipmentType: "[shoes]",
			ResultRarity: 6, ResultMinimumLevel: 85, ResultDurability: 18, QualitySeed: 123456,
			Materials: []alignedcmd.EquipmentRegenerationMaterial{
				{ItemID: 10099775, Count: 6, AccountShared: true},
				{ItemID: 3167, Count: 300},
			},
			CandidateCount: 7,
		}, nil
	}
}

func saveEquipmentRegenerationFixture(t *testing.T, repos dnfrepo.Group, crystalCount int64) {
	t.Helper()
	ctx := context.Background()
	source := dnfrepo.ItemStack{ItemID: 1001, Count: 1, Bind: true, Extra: map[string]string{
		"item_kind": "equipment", "pvf_path": "equipment/source.equ", "seal_flag": "1", "durability": "18", "max_durability": "18",
	}}
	source.RawEntry = currentRawEntryForStack(11, source)
	if err := repos.Inventory.Save(ctx, dnfrepo.InventoryRecord{CharacterID: "77", Slots: map[string]dnfrepo.ItemStack{
		slotKey(listTypeMain, 11): source,
		slotKey(listTypeMain, 65): {ItemID: 3167, Count: crystalCount, Extra: map[string]string{"item_kind": "stackable"}},
	}}); err != nil {
		t.Fatal(err)
	}
	if err := repos.AccountInventory.Save(ctx, dnfrepo.AccountInventoryRecord{AccountID: "dnf:test", Slots: map[string]dnfrepo.ItemStack{
		slotKey(listTypeMain, 364): {ItemID: 10099775, Count: 10, Extra: map[string]string{"item_kind": "stackable"}},
	}}); err != nil {
		t.Fatal(err)
	}
	if err := repos.Character.Save(ctx, dnfrepo.CharacterRecord{CharacterID: "77"}); err != nil {
		t.Fatal(err)
	}
}

func TestDecodeEquipmentRegenerationRequestExactCurrentNoPackBody(t *testing.T) {
	request, err := DecodeEquipmentRegenerationRequest([]byte{0x0b, 0, 0, 0, 2, 0})
	if err != nil || request.SourceSlotIndex != 11 || request.Mode != 0 || request.Part != 2 {
		t.Fatalf("request=%+v err=%v", request, err)
	}
	if _, err := DecodeEquipmentRegenerationRequest([]byte{0x0b, 0, 0, 0, 2}); err == nil {
		t.Fatal("short request accepted")
	}
}

func TestHandlerEquipmentRegenerationConsumesMixedMaterialsAndOrdersTemplateReplacement(t *testing.T) {
	repos := dnfrepomemory.NewMemoryGroup()
	saveEquipmentRegenerationFixture(t, repos, 300)
	result, err := NewHandler().Handle(context.Background(), alignedcmd.Request{
		Opcode: uint16(dnfenum.CmdPacketRegenerationRandomOption), Body: []byte{0x0b, 0, 0, 0, 2, 0},
		AccountID: "dnf:test", SelectedCharacterID: 77, Repositories: repos,
		EquipmentRegenerationResolver: testEquipmentRegenerationResolver(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Handled || !result.ResponseAllowed || len(result.UpperResponses) != 3 || len(result.PostActions) != 0 {
		t.Fatalf("result=%+v", result)
	}
	empty, final, ack := result.UpperResponses[0], result.UpperResponses[1], result.UpperResponses[2]
	// The clear row has to carry id -1. sub_1D73120 removes a slot only on -1
	// (0x1D73912); id 0 falls into the in-place update path, which refuses to
	// change an equipment object's item id (0x1D73AB2 bails unless the object
	// type is 2), so the client kept painting the source piece until relog.
	if empty.Classification != 0 || empty.MsgID != msgItemListUpdate || empty.Body[0] != 0 ||
		binary.LittleEndian.Uint16(empty.Body[1:3]) != 1 ||
		binary.LittleEndian.Uint32(empty.Body[5:9]) != ^uint32(0) {
		t.Fatalf("empty update=%+v body=% X", empty, empty.Body)
	}
	if final.Classification != 0 || final.MsgID != msgItemListUpdate || final.Body[0] != 0 || binary.LittleEndian.Uint16(final.Body[1:3]) != 3 {
		t.Fatalf("final update=%+v body=% X", final, final.Body)
	}
	if ack.Classification != 1 || ack.MsgID != uint16(dnfenum.CmdPacketRegenerationRandomOption) || !bytes.Equal(ack.Body, []byte{1, 11, 0}) {
		t.Fatalf("ack=%+v", ack)
	}
	inventory, found, loadErr := repos.Inventory.Load(context.Background(), "77")
	if loadErr != nil || !found {
		t.Fatalf("inventory found=%t err=%v", found, loadErr)
	}
	if got := inventory.Slots[slotKey(0, 11)]; got.ItemID != 2002 || got.Extra["quality_seed"] != "123456" || got.Extra["seal_flag"] != "1" {
		t.Fatalf("result stack=%+v", got)
	}
	if _, exists := inventory.Slots[slotKey(0, 65)]; exists {
		t.Fatalf("crystal material not removed: %+v", inventory.Slots)
	}
	account, found, loadErr := repos.AccountInventory.Load(context.Background(), "dnf:test")
	if loadErr != nil || !found || account.Slots[slotKey(0, 364)].Count != 4 {
		t.Fatalf("account materials found=%t err=%v slots=%+v", found, loadErr, account.Slots)
	}
}

func TestEquipmentRegenerationMaterialFailureRollsBackBothStores(t *testing.T) {
	repos := dnfrepomemory.NewMemoryGroup()
	saveEquipmentRegenerationFixture(t, repos, 299)
	owner, err := NewOwner(repos)
	if err != nil {
		t.Fatal(err)
	}
	result, err := owner.RegenerateEquipment(context.Background(), "dnf:test", 77, EquipmentRegenerationRequest{SourceSlotIndex: 11, Part: 2}, testEquipmentRegenerationResolver())
	if err != nil {
		t.Fatal(err)
	}
	if result.Success || result.Changed {
		t.Fatalf("unexpected result=%+v", result)
	}
	inventory, _, _ := repos.Inventory.Load(context.Background(), "77")
	account, _, _ := repos.AccountInventory.Load(context.Background(), "dnf:test")
	if inventory.Slots[slotKey(0, 11)].ItemID != 1001 || inventory.Slots[slotKey(0, 65)].Count != 299 || account.Slots[slotKey(0, 364)].Count != 10 {
		t.Fatalf("rollback inventory=%+v account=%+v", inventory.Slots, account.Slots)
	}
}
