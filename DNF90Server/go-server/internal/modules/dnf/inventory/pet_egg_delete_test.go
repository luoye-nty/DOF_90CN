package inventory

import (
	"context"
	"strings"
	"testing"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	dnfcreaturestate "longheng.io/server/internal/modules/dnf/creaturestate"
	dnfenum "longheng.io/server/internal/modules/dnf/dnfenum"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

// 现场：「宠物蛋无法丢弃」。蛋被 creaturestate 误当成"直接发放的宠物"，行上带了
// creature_serial_or_handle，于是这里按"已绑定宠物"拒绝。
//
// 2801466 -> 线上 egg_SDCharacterCreature_ft.equ（宠物蛋，带 [output index]）
// 400990167 -> 直接发放的真宠物（无 [output index]）
const (
	testEggItemID    int64 = 2681466
	testPetItemID    int64 = 400990167
	testEggPetSerial       = "2"
)

func newPetEggDeleteFixture(t *testing.T, ctx context.Context, repos dnfrepo.Group, characterID string) {
	t.Helper()
	saveTestInventory(t, ctx, repos, dnfrepo.InventoryRecord{
		CharacterID: characterID,
		Slots: map[string]dnfrepo.ItemStack{
			slotKey(listTypePet, 0): {ItemID: testEggItemID, Count: 1, Extra: map[string]string{
				"item_kind":                 "equipment",
				"equipment_type":            "[creature]",
				"pvf_path":                  "equipment/creature/egg_SDCharacterCreature_ft.equ",
				"creature_serial_or_handle": testEggPetSerial,
				"creature_key":              testEggPetSerial,
			}},
			slotKey(listTypePet, 1): {ItemID: testPetItemID, Count: 1, Extra: map[string]string{
				"item_kind":                 "equipment",
				"equipment_type":            "[creature]",
				"pvf_path":                  "equipment/creature/chn_2018_SpringFestival/chn_400990167.equ",
				"creature_serial_or_handle": "1",
			}},
		},
	})
	if err := repos.Pet.Save(ctx, dnfrepo.PetRecord{CharacterID: characterID, Entries: map[string]dnfrepo.PetEntry{
		testEggPetSerial: {PetKey: testEggPetSerial, CreatureKey: 2, ItemID: testEggItemID, SourceListType: listTypePet, SourceSlotIndex: 0},
		"1":              {PetKey: "1", CreatureKey: 1, ItemID: testPetItemID, SourceListType: listTypePet, SourceSlotIndex: 1},
	}}); err != nil {
		t.Fatal(err)
	}
}

func deletePetSlot(t *testing.T, ctx context.Context, repos dnfrepo.Group, characterID string, slot byte) alignedcmd.Result {
	t.Helper()
	got, err := NewHandler().Handle(ctx, alignedcmd.Request{
		Opcode:              uint16(dnfenum.CmdPacketDeleteItem),
		Body:                []byte{listTypePet, slot, 0, 1, 0},
		AccountID:           "acc-pet-egg",
		SelectedCharacterID: 93,
		Repositories:        repos,
	})
	if err != nil {
		t.Fatalf("Handle error = %v", err)
	}
	return got
}

// TestDeleteAllowsPetEggCarryingLegacySerial 证明：带历史脏 serial 的蛋可以丢，
// 而且它被误建的宠物记录会一并清掉。
func TestDeleteAllowsPetEggCarryingLegacySerial(t *testing.T) {
	ctx := context.Background()
	const characterID = "93"

	dnfcreaturestate.SetPetEggProbe(func(itemID int64) bool { return itemID == testEggItemID })
	defer dnfcreaturestate.SetPetEggProbe(nil)

	repos := dnfrepomemory.NewMemoryGroup()
	newPetEggDeleteFixture(t, ctx, repos, characterID)

	got := deletePetSlot(t, ctx, repos, characterID, 0)
	if !got.Handled || !got.ResponseAllowed {
		t.Fatalf("蛋（带 serial）仍被拒: %+v", got)
	}
	if strings.Contains(got.Reason, ErrPetInventoryOwnerRequired.Error()) {
		t.Fatalf("蛋仍被 typed pet owner 拦下: %q", got.Reason)
	}

	inventory := loadTestInventory(t, ctx, repos, characterID)
	if stack, found := inventory.Slots[slotKey(listTypePet, 0)]; found {
		t.Fatalf("蛋没有被删掉: %+v", stack)
	}
	petRecord, found, err := repos.Pet.Load(ctx, characterID)
	if err != nil || !found {
		t.Fatalf("load pets found=%t err=%v", found, err)
	}
	if _, still := petRecord.Entries[testEggPetSerial]; still {
		t.Fatalf("蛋的孤儿宠物记录没有被清理: %+v", petRecord.Entries)
	}
	if _, ok := petRecord.Entries["1"]; !ok {
		t.Fatalf("真宠物的记录被误删: %+v", petRecord.Entries)
	}
}

// TestDeleteStillRejectsBoundPet 钉住"宠物不允许丢弃"：判据在、但物品不是蛋时，
// 带 serial 的行必须仍然被拒绝。
func TestDeleteStillRejectsBoundPet(t *testing.T) {
	ctx := context.Background()
	const characterID = "93"

	dnfcreaturestate.SetPetEggProbe(func(itemID int64) bool { return itemID == testEggItemID })
	defer dnfcreaturestate.SetPetEggProbe(nil)

	repos := dnfrepomemory.NewMemoryGroup()
	newPetEggDeleteFixture(t, ctx, repos, characterID)

	got := deletePetSlot(t, ctx, repos, characterID, 1)
	if !strings.Contains(got.Reason, ErrPetInventoryOwnerRequired.Error()) {
		t.Fatalf("真宠物必须仍被拒绝, reason=%q", got.Reason)
	}
	inventory := loadTestInventory(t, ctx, repos, characterID)
	if _, found := inventory.Slots[slotKey(listTypePet, 1)]; !found {
		t.Fatal("真宠物不应被删除")
	}
}
