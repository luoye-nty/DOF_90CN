package avatarfitting

import (
	"context"
	"encoding/binary"
	"errors"
	"testing"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

func TestCommitAtomicallyConsumesTicketsReplacesAvatarsAndGrantsGaugeReward(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	seedAvatarFittingOwner(t, ctx, repositories, 2)
	owner, err := NewOwner(repositories)
	if err != nil {
		t.Fatal(err)
	}
	command := testAvatarFittingCommand()
	result, err := owner.Commit(ctx, command)
	if err != nil {
		t.Fatal(err)
	}
	if result.TicketConsumed != 2 || result.RewardGranted != 1 || result.GaugeBefore != 19 || result.GaugeAfter != 21 {
		t.Fatalf("result=%+v", result)
	}
	inventory, _, _ := repositories.Inventory.Load(ctx, "7")
	if _, found := inventory.Slots["0:10"]; found {
		t.Fatal("ticket row remained after exact two-ticket consume")
	}
	if reward, found := inventory.Slots["0:20"]; !found || reward.ItemID != 9000 || reward.Count != 1 {
		t.Fatalf("reward=%+v found=%t", reward, found)
	}
	equipment, _, _ := repositories.Equipment.Load(ctx, "7")
	for key, want := range map[string]int64{"0": 2000, "1": 2001} {
		entry := equipment.Entries[key]
		if entry.ItemID != want || int64(binary.LittleEndian.Uint32(entry.RawEntry[2:6])) != want {
			t.Fatalf("equipment[%s]=%+v want=%d", key, entry, want)
		}
	}
	settings, _, _ := repositories.Settings.Load(ctx, "character:7:avatar_fitting_room")
	if settings.Values[settingsUsedParts] != "21" {
		t.Fatalf("settings=%+v", settings.Values)
	}
}

func TestCommitRollsBackEveryAggregateWhenTicketsAreInsufficient(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	seedAvatarFittingOwner(t, ctx, repositories, 1)
	owner, _ := NewOwner(repositories)
	_, err := owner.Commit(ctx, testAvatarFittingCommand())
	if !errors.Is(err, ErrTicketMissing) {
		t.Fatalf("error=%v want=%v", err, ErrTicketMissing)
	}
	inventory, _, _ := repositories.Inventory.Load(ctx, "7")
	if stack := inventory.Slots["0:10"]; stack.ItemID != 490702492 || stack.Count != 1 {
		t.Fatalf("ticket changed on rollback: %+v", stack)
	}
	equipment, _, _ := repositories.Equipment.Load(ctx, "7")
	if equipment.Entries["0"].ItemID != 1000 || equipment.Entries["1"].ItemID != 1001 {
		t.Fatalf("equipment changed on rollback: %+v", equipment.Entries)
	}
	settings, _, _ := repositories.Settings.Load(ctx, "character:7:avatar_fitting_room")
	if settings.Values[settingsUsedParts] != "19" {
		t.Fatalf("gauge changed on rollback: %+v", settings.Values)
	}
}

func TestCommitRollsBackWhenGaugeRewardHasNoInventorySpace(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	seedAvatarFittingOwner(t, ctx, repositories, 2)
	inventory, _, _ := repositories.Inventory.Load(ctx, "7")
	inventory.Slots["0:20"] = dnfrepo.ItemStack{ItemID: 1234, Count: 1, RawEntry: make([]byte, 119)}
	if err := repositories.Inventory.Save(ctx, inventory); err != nil {
		t.Fatal(err)
	}
	owner, _ := NewOwner(repositories)
	_, err := owner.Commit(ctx, testAvatarFittingCommand())
	if !errors.Is(err, ErrRewardSpace) {
		t.Fatalf("error=%v want=%v", err, ErrRewardSpace)
	}
	inventory, _, _ = repositories.Inventory.Load(ctx, "7")
	if stack := inventory.Slots["0:10"]; stack.ItemID != 490702492 || stack.Count != 2 {
		t.Fatalf("tickets changed on reward rollback: %+v", stack)
	}
	equipment, _, _ := repositories.Equipment.Load(ctx, "7")
	if equipment.Entries["0"].ItemID != 1000 || equipment.Entries["1"].ItemID != 1001 {
		t.Fatalf("equipment changed on reward rollback: %+v", equipment.Entries)
	}
	settings, _, _ := repositories.Settings.Load(ctx, "character:7:avatar_fitting_room")
	if settings.Values[settingsUsedParts] != "19" {
		t.Fatalf("gauge changed on reward rollback: %+v", settings.Values)
	}
}

func testAvatarFittingCommand() Command {
	command := Command{
		AccountID:      "dnf:1",
		CharacterID:    "7",
		TicketItemID:   490702492,
		GaugeThreshold: 20,
		Reward: &Reward{
			ItemID: 9000, CountPerCrossing: 1, SlotStart: 20, SlotEnd: 20, StackLimit: 1000,
			BuildStack: func(slot int16, count int64) (dnfrepo.ItemStack, error) {
				return dnfrepo.ItemStack{ItemID: 9000, Count: count, RawEntry: make([]byte, 119)}, nil
			},
		},
	}
	for index := range command.Changes {
		command.Changes[index] = Change{EquipmentSlot: -1, ObjectIndex: -1, CurrentItemID: -1}
	}
	command.Changes[0] = Change{EquipmentSlot: 0, ObjectIndex: 100, CurrentItemID: 1000, OutputItemID: 2000, PVFPath: "equipment/a.equ", EquipmentType: "[hat avatar]"}
	command.Changes[1] = Change{EquipmentSlot: 1, ObjectIndex: 101, CurrentItemID: 1001, OutputItemID: 2001, PVFPath: "equipment/b.equ", EquipmentType: "[hair avatar]"}
	return command
}

func seedAvatarFittingOwner(t *testing.T, ctx context.Context, repositories dnfrepo.Group, tickets int64) {
	t.Helper()
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{CharacterID: "7", AccountID: "dnf:1", Job: "0"}); err != nil {
		t.Fatal(err)
	}
	ticketRaw := make([]byte, 119)
	binary.LittleEndian.PutUint32(ticketRaw[6:10], uint32(tickets))
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{CharacterID: "7", Slots: map[string]dnfrepo.ItemStack{
		"0:10": {ItemID: 490702492, Count: tickets, RawEntry: ticketRaw},
	}}); err != nil {
		t.Fatal(err)
	}
	raw0, raw1 := make([]byte, 119), make([]byte, 119)
	binary.LittleEndian.PutUint32(raw0[2:6], 1000)
	binary.LittleEndian.PutUint32(raw1[2:6], 1001)
	if err := repositories.Equipment.Save(ctx, dnfrepo.EquipmentRecord{CharacterID: "7", Entries: map[string]dnfrepo.EquipmentEntry{
		"0": {SlotIndex: 0, ItemID: 1000, RawEntry: raw0},
		"1": {SlotIndex: 1, ItemID: 1001, RawEntry: raw1},
	}}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Settings.Save(ctx, dnfrepo.SettingsRecord{Scope: "character:7:avatar_fitting_room", Values: map[string]string{settingsUsedParts: "19"}}); err != nil {
		t.Fatal(err)
	}
}
