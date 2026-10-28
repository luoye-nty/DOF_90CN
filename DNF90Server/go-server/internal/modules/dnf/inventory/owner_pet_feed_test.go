package inventory

import (
	"context"
	"encoding/binary"
	"errors"
	"strings"
	"testing"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

const testPetFeedPVFPath = "stackable/cash/creature/creature_food.stk"

func testPetFeedResolver(itemID int64) (alignedcmd.StackableUseResolution, error) {
	if itemID != 24 {
		return alignedcmd.StackableUseResolution{}, nil
	}
	return alignedcmd.StackableUseResolution{
		ItemID:                       24,
		PVFPath:                      testPetFeedPVFPath,
		StackableType:                "[feed]",
		PetFeedEffectDurationSeconds: 300,
		PetFeedSkillChargePercent:    -20,
		PetFeedSatietyDelta:          30,
	}, nil
}

func savePetFeedFixture(t *testing.T, repos dnfrepo.Group, equipped bool) {
	t.Helper()
	ctx := context.Background()
	raw := make([]byte, currentItemListEntrySize)
	binary.LittleEndian.PutUint16(raw[0:2], 204)
	binary.LittleEndian.PutUint32(raw[2:6], 24)
	binary.LittleEndian.PutUint32(raw[6:10], 2)
	saveTestInventory(t, ctx, repos, dnfrepo.InventoryRecord{
		CharacterID: "77",
		Slots: map[string]dnfrepo.ItemStack{
			slotKey(listTypePet, 204): {
				ItemID:   24,
				Count:    2,
				RawEntry: raw,
				Extra: map[string]string{
					"item_kind": "stackable",
					"pvf_path":  testPetFeedPVFPath,
				},
			},
		},
	})
	equippedKey := ""
	if equipped {
		equippedKey = "44"
	}
	if err := repos.Pet.Save(ctx, dnfrepo.PetRecord{
		CharacterID: "77",
		EquippedKey: equippedKey,
		Entries: map[string]dnfrepo.PetEntry{
			"44": {
				PetKey:          "44",
				CreatureKey:     44,
				ItemID:          400990168,
				SourceListType:  listTypeEquipment,
				SourceSlotIndex: 26,
				Level:           1,
				Satiety:         2,
				SatietyMicros:   2_567_020,
			},
		},
	}); err != nil {
		t.Fatal(err)
	}
}

func TestHandlerUsePetFeedCommitsStackAndSatietyAtomically(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	savePetFeedFixture(t, repos, true)
	body := make([]byte, 15)
	binary.LittleEndian.PutUint16(body[0:2], 204)
	body[2] = listTypePet
	binary.LittleEndian.PutUint32(body[3:7], 2)
	binary.LittleEndian.PutUint32(body[7:11], 24)

	got, err := NewHandler().Handle(ctx, alignedcmd.Request{
		Opcode:               uint16(dnfenum.CmdPacketUseStackable),
		Body:                 body,
		AccountID:            "acc",
		SelectedCharacterID:  77,
		Repositories:         repos,
		StackableUseResolver: testPetFeedResolver,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !got.Handled || !got.ResponseAllowed || len(got.UpperResponses) != 1 || len(got.ItemSlotRefreshes) != 0 || len(got.PostActions) != 1 {
		t.Fatalf("handler result = %+v", got)
	}
	wantACK := []byte{1, 204, 0, listTypePet, 2, 0, 0, 0, 24, 0, 0, 0}
	if ack := got.UpperResponses[0]; ack.MsgID != uint16(dnfenum.CmdPacketUseStackable) || string(ack.Body) != string(wantACK) {
		t.Fatalf("op44 ack = %+v, want %x", ack, wantACK)
	}
	if got.PostActions[0] != alignedcmd.PostActionRefreshSelectedCreatureGrowthState {
		t.Fatalf("post action = %v, want %v", got.PostActions, alignedcmd.PostActionRefreshSelectedCreatureGrowthState)
	}
	if !strings.Contains(got.Reason, "satiety=2->32") ||
		!strings.Contains(got.Reason, "op105/op102 creature refresh") {
		t.Fatalf("post actions/reason = %v %q", got.PostActions, got.Reason)
	}

	inventory := loadTestInventory(t, ctx, repos, "77")
	feed := inventory.Slots[slotKey(listTypePet, 204)]
	if feed.ItemID != 24 || feed.Count != 1 || binary.LittleEndian.Uint32(feed.RawEntry[6:10]) != 1 {
		t.Fatalf("feed stack after use = %+v raw=%x", feed, feed.RawEntry)
	}
	petRecord, found, err := repos.Pet.Load(ctx, "77")
	if err != nil || !found {
		t.Fatalf("load pet record found=%t err=%v", found, err)
	}
	entry := petRecord.Entries["44"]
	if entry.Satiety != 32 || entry.SatietyMicros != 32_567_020 {
		t.Fatalf("equipped pet after feed = %+v", entry)
	}
}

func TestOwnerUsePetFeedWithoutEquippedCreatureRollsBackSource(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	savePetFeedFixture(t, repos, false)
	owner, err := NewOwner(repos)
	if err != nil {
		t.Fatal(err)
	}
	_, err = owner.UseStackableWithResolvers(ctx, NewUseStackableCommand(alignedcmd.Request{
		AccountID:           "acc",
		SelectedCharacterID: 77,
	}, UseStackableRequest{
		SlotIndex:     204,
		ListType:      listTypePet,
		InstanceValue: 2,
		ItemCode:      24,
	}), nil, nil, testPetFeedResolver)
	if !errors.Is(err, ErrPetFeedEquippedCreatureRequired) {
		t.Fatalf("use pet feed error = %v", err)
	}
	if feed := loadTestInventory(t, ctx, repos, "77").Slots[slotKey(listTypePet, 204)]; feed.Count != 2 {
		t.Fatalf("feed stack changed after rollback = %+v", feed)
	}
	petRecord, _, err := repos.Pet.Load(ctx, "77")
	if err != nil {
		t.Fatal(err)
	}
	if entry := petRecord.Entries["44"]; entry.Satiety != 2 || entry.SatietyMicros != 2_567_020 {
		t.Fatalf("pet changed after rollback = %+v", entry)
	}
}

func TestOwnerUsePetFeedRejectsNonConsumablePetSlot(t *testing.T) {
	repos := dnfrepomemory.NewMemoryGroup()
	savePetFeedFixture(t, repos, true)
	owner, err := NewOwner(repos)
	if err != nil {
		t.Fatal(err)
	}
	_, err = owner.UseStackableWithResolvers(context.Background(), NewUseStackableCommand(alignedcmd.Request{
		SelectedCharacterID: 77,
	}, UseStackableRequest{
		SlotIndex:     188,
		ListType:      listTypePet,
		InstanceValue: 2,
		ItemCode:      24,
	}), nil, nil, testPetFeedResolver)
	if !errors.Is(err, ErrPetFeedContractRequired) {
		t.Fatalf("non-consumable pet slot error = %v", err)
	}
}
