package inventory

import (
	"context"
	"encoding/binary"
	"testing"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

func testForgingResolver(weight int, targetLevel byte) alignedcmd.ForgingResolver {
	return func(materialItemID int64, targetItemID int64, currentLevel byte) (alignedcmd.ForgingResolution, error) {
		return alignedcmd.ForgingResolution{
			TargetKind:          "equipment",
			TargetPVFPath:       "equipment/weapon/test.equ",
			TargetEquipmentType: "[weapon]",
			TargetDurability:    45,
			MaxLevel:            8,
			TargetLevel:         targetLevel,
			IndependentAttack:   123,
			SuccessWeight:       weight,
			MaterialItemID:      3326,
			MaterialCount:       3,
		}, nil
	}
}

func testForgingCommand(listType byte, targetSlot int16) Command {
	return NewSeparateUpgradeCommand(alignedcmd.Request{AccountID: "acc", SelectedCharacterID: 77}, SeparateUpgradeRequest{
		TargetListType:       listType,
		TargetSlotIndex:      targetSlot,
		TargetItemTemplateID: 700,
		MaterialSlotIndex:    121,
		TargetItemName:       "test weapon",
	})
}

func testForgingTargetStack(level byte) dnfrepo.ItemStack {
	raw := make([]byte, currentItemListEntrySize)
	binary.LittleEndian.PutUint16(raw[0x0B:0x0D], 45)
	raw[currentForgingRawOffset] = level
	raw[0x62] = 0xA5
	raw[0x68] = 0xB6
	raw[0x6D] = 0xC7
	return dnfrepo.ItemStack{
		ItemID:   700,
		Count:    1,
		RawEntry: raw,
		Extra: map[string]string{
			"item_kind":  "equipment",
			"durability": "45",
			"keep":       "unchanged",
		},
	}
}

func testForgingMaterialStack(count int64) dnfrepo.ItemStack {
	raw := make([]byte, currentItemListEntrySize)
	binary.LittleEndian.PutUint32(raw[0x06:0x0A], uint32(count))
	return dnfrepo.ItemStack{
		ItemID:   3326,
		Count:    count,
		RawEntry: raw,
		Extra: map[string]string{
			"item_kind":   "stackable",
			"pvf_path":    "stackable/material/strong_breath.stk",
			"stack_limit": "1000",
		},
	}
}

func saveForgingInventoryFixture(t *testing.T, ctx context.Context, repos dnfrepo.Group, targetInMain bool) {
	t.Helper()
	slots := map[string]dnfrepo.ItemStack{
		slotKey(listTypeMain, 121): testForgingMaterialStack(10),
	}
	if targetInMain {
		slots[slotKey(listTypeMain, 25)] = testForgingTargetStack(2)
	}
	saveTestInventory(t, ctx, repos, dnfrepo.InventoryRecord{CharacterID: "77", Slots: slots})
}

func saveForgingEquipmentFixture(t *testing.T, ctx context.Context, repos dnfrepo.Group) {
	t.Helper()
	stack := testForgingTargetStack(2)
	err := repos.Equipment.Save(ctx, dnfrepo.EquipmentRecord{
		CharacterID: "77",
		Entries: map[string]dnfrepo.EquipmentEntry{
			"11": {SlotIndex: 11, ItemID: stack.ItemID, RawEntry: stack.RawEntry, Extra: stack.Extra},
		},
	})
	if err != nil {
		t.Fatalf("save equipment: %v", err)
	}
}

func TestOwnerForgeMainInventorySuccessPersistsCurrentClientOffset(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	saveForgingInventoryFixture(t, ctx, repos, true)
	owner, err := NewOwner(repos)
	if err != nil {
		t.Fatal(err)
	}
	result, err := owner.Forge(ctx, testForgingCommand(listTypeMain, 25), testForgingResolver(10000, 3))
	if err != nil {
		t.Fatal(err)
	}
	if !result.Success || !result.UpgradeSucceeded || result.OldLevel != 2 || result.NewLevel != 3 || result.IndependentAttack != 123 || result.MaterialRemainingCount != 7 {
		t.Fatalf("result = %+v", result)
	}
	loaded := loadTestInventory(t, ctx, repos, "77")
	target := loaded.Slots[slotKey(listTypeMain, 25)]
	if target.RawEntry[currentForgingRawOffset] != 3 || target.Extra["forging_level"] != "3" {
		t.Fatalf("forging state = raw:%d extra:%q", target.RawEntry[currentForgingRawOffset], target.Extra["forging_level"])
	}
	if target.RawEntry[0x62] != 0xA5 || target.RawEntry[0x68] != 0xB6 || target.RawEntry[0x6D] != 0xC7 || target.Extra["keep"] != "unchanged" {
		t.Fatalf("neighboring state changed: raw62=%02x raw68=%02x raw6d=%02x extra=%v", target.RawEntry[0x62], target.RawEntry[0x68], target.RawEntry[0x6D], target.Extra)
	}
	if loaded.Slots[slotKey(listTypeMain, 121)].Count != 7 {
		t.Fatalf("material not consumed: %+v", loaded.Slots[slotKey(listTypeMain, 121)])
	}
}

func TestOwnerForgeFailedRollKeepsLevelAndConsumesMaterial(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	saveForgingInventoryFixture(t, ctx, repos, true)
	owner, _ := NewOwner(repos)
	result, err := owner.Forge(ctx, testForgingCommand(listTypeMain, 25), testForgingResolver(0, 3))
	if err != nil {
		t.Fatal(err)
	}
	if !result.Success || result.UpgradeSucceeded || result.NewLevel != 2 || result.IndependentAttack != 0 || result.MaterialRemainingCount != 7 {
		t.Fatalf("result = %+v", result)
	}
	loaded := loadTestInventory(t, ctx, repos, "77")
	if loaded.Slots[slotKey(listTypeMain, 25)].RawEntry[currentForgingRawOffset] != 2 || loaded.Slots[slotKey(listTypeMain, 121)].Count != 7 {
		t.Fatalf("failed-roll persistence = %+v", loaded.Slots)
	}
}

func TestOwnerForgePortableDeviceAcceptsLegacyMinimalStackAfterPVFResolution(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	saveForgingInventoryFixture(t, ctx, repos, true)
	record := loadTestInventory(t, ctx, repos, "77")
	device := dnfrepo.ItemStack{ItemID: 10003912, Count: 2}
	record.Slots[slotKey(listTypeMain, 121)] = device
	saveTestInventory(t, ctx, repos, record)

	owner, err := NewOwner(repos)
	if err != nil {
		t.Fatal(err)
	}
	resolver := func(materialItemID int64, targetItemID int64, currentLevel byte) (alignedcmd.ForgingResolution, error) {
		if materialItemID != device.ItemID || targetItemID != 700 || currentLevel != 2 {
			t.Fatalf("resolver request material=%d target=%d current=%d", materialItemID, targetItemID, currentLevel)
		}
		return alignedcmd.ForgingResolution{
			TargetKind: "equipment", TargetEquipmentType: "[weapon]", TargetDurability: 45,
			MaxLevel: 8, TargetLevel: 3, IndependentAttack: 123, SuccessWeight: currentForgingSuccessBase,
			MaterialItemID: device.ItemID, MaterialCount: 1,
		}, nil
	}
	result, err := owner.Forge(ctx, testForgingCommand(listTypeMain, 25), resolver)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Success || !result.UpgradeSucceeded || result.NewLevel != 3 || result.MaterialRemainingCount != 1 {
		t.Fatalf("portable result=%+v", result)
	}
	loaded := loadTestInventory(t, ctx, repos, "77")
	if got := loaded.Slots[slotKey(listTypeMain, 121)]; got.ItemID != device.ItemID || got.Count != 1 {
		t.Fatalf("portable device remaining=%+v", got)
	}
}

func TestOwnerForgeEquippedWeaponCommitsWithMaterialAtomically(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	saveForgingInventoryFixture(t, ctx, repos, false)
	saveForgingEquipmentFixture(t, ctx, repos)
	owner, _ := NewOwner(repos)
	result, err := owner.Forge(ctx, testForgingCommand(listTypeEquipment, 11), testForgingResolver(10000, 3))
	if err != nil {
		t.Fatal(err)
	}
	if !result.Success || !result.UpgradeSucceeded || result.TargetListType != listTypeEquipment {
		t.Fatalf("result = %+v", result)
	}
	equipment, found, err := repos.Equipment.Load(ctx, "77")
	if err != nil || !found {
		t.Fatalf("load equipment found=%t err=%v", found, err)
	}
	entry := equipment.Entries["11"]
	if entry.RawEntry[currentForgingRawOffset] != 3 || entry.Extra["forging_level"] != "3" || entry.RawEntry[0x62] != 0xA5 || entry.RawEntry[0x68] != 0xB6 || entry.RawEntry[0x6D] != 0xC7 {
		t.Fatalf("equipment entry = %+v raw=%x", entry, entry.RawEntry)
	}
	if loadTestInventory(t, ctx, repos, "77").Slots[slotKey(listTypeMain, 121)].Count != 7 {
		t.Fatalf("equipped forging did not consume material")
	}
}

func TestOwnerForgeFixedTicketOverwritesLevelAndConsumesOne(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	saveForgingInventoryFixture(t, ctx, repos, true)
	record := loadTestInventory(t, ctx, repos, "77")
	target := record.Slots[slotKey(listTypeMain, 25)]
	setForgingLevel(&target, 8)
	// Ticket forging follows the PVF coupon path and does not require full
	// durability, matching the native separate-upgrade ticket scene.
	target.Extra["durability"] = "44"
	record.Slots[slotKey(listTypeMain, 25)] = target
	ticket := testForgingMaterialStack(2)
	ticket.ItemID = 9900
	ticket.Extra["pvf_path"] = "stackable/ticket/fixed7.stk"
	record.Slots[slotKey(listTypeMain, 121)] = ticket
	saveTestInventory(t, ctx, repos, record)

	owner, _ := NewOwner(repos)
	resolver := func(materialItemID int64, targetItemID int64, currentLevel byte) (alignedcmd.ForgingResolution, error) {
		return alignedcmd.ForgingResolution{
			TargetKind: "equipment", TargetEquipmentType: "[weapon]", TargetDurability: 45,
			MaxLevel: 8, TargetLevel: 7, IndependentAttack: 456, SuccessWeight: 10000,
			MaterialItemID: materialItemID, MaterialCount: 1, TicketMode: "fixed", TicketApplyValue: -1,
		}, nil
	}
	result, err := owner.Forge(ctx, testForgingCommand(listTypeMain, 25), resolver)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Success || !result.UpgradeSucceeded || result.OldLevel != 8 || result.NewLevel != 7 || result.MaterialCost != 1 || result.MaterialRemainingCount != 1 {
		t.Fatalf("ticket result = %+v", result)
	}
	loaded := loadTestInventory(t, ctx, repos, "77")
	if got := forgingLevelOf(loaded.Slots[slotKey(listTypeMain, 25)]); got != 7 {
		t.Fatalf("fixed ticket forging level=%d, want 7", got)
	}
	if got := loaded.Slots[slotKey(listTypeMain, 121)].Count; got != 1 {
		t.Fatalf("fixed ticket remaining=%d, want 1", got)
	}
}

func TestOwnerForgeAdditionalTicketFailureConsumesOneAndRetainsLevel(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	saveForgingInventoryFixture(t, ctx, repos, true)
	record := loadTestInventory(t, ctx, repos, "77")
	target := record.Slots[slotKey(listTypeMain, 25)]
	setForgingLevel(&target, 6)
	record.Slots[slotKey(listTypeMain, 25)] = target
	ticket := testForgingMaterialStack(2)
	ticket.ItemID = 9901
	ticket.Extra["pvf_path"] = "stackable/ticket/add1.stk"
	record.Slots[slotKey(listTypeMain, 121)] = ticket
	saveTestInventory(t, ctx, repos, record)

	owner, _ := NewOwner(repos)
	resolver := func(materialItemID int64, targetItemID int64, currentLevel byte) (alignedcmd.ForgingResolution, error) {
		return alignedcmd.ForgingResolution{
			TargetKind: "equipment", TargetEquipmentType: "[weapon]", TargetDurability: 45,
			MaxLevel: 8, TargetLevel: 7, IndependentAttack: 456, SuccessWeight: 0,
			MaterialItemID: materialItemID, MaterialCount: 1, TicketMode: "additional", TicketApplyValue: -1,
		}, nil
	}
	result, err := owner.Forge(ctx, testForgingCommand(listTypeMain, 25), resolver)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Success || result.UpgradeSucceeded || result.OldLevel != 6 || result.NewLevel != 6 || result.MaterialRemainingCount != 1 {
		t.Fatalf("ticket failure result = %+v", result)
	}
	loaded := loadTestInventory(t, ctx, repos, "77")
	if got := forgingLevelOf(loaded.Slots[slotKey(listTypeMain, 25)]); got != 6 {
		t.Fatalf("failed ticket changed forging level=%d", got)
	}
	if got := loaded.Slots[slotKey(listTypeMain, 121)].Count; got != 1 {
		t.Fatalf("failed ticket remaining=%d, want 1", got)
	}
}

// randomForgingTicketResolver mirrors the shipped +6锻造券 2683788:
// [enchant random] with a single er_enchant candidate (level 6, weight
// 100000), er_slot 1 (weapon), er_grade 0/1/2/3/5/6, er_seal 0.
func randomForgingTicketResolver(entries []alignedcmd.UpgradeTicketRandomEntry, attacks map[int64]int64) alignedcmd.ForgingResolver {
	return func(materialItemID int64, targetItemID int64, currentLevel byte) (alignedcmd.ForgingResolution, error) {
		return alignedcmd.ForgingResolution{
			TargetKind: "equipment", TargetEquipmentType: "[weapon]", TargetDurability: 45,
			MaxLevel: 8, TargetRarity: 3,
			MaterialItemID: materialItemID, MaterialCount: 1,
			TicketRandom: true,
			RandomTicket: alignedcmd.EnchantRandomTicket{
				Entries:         entries,
				SlotRestriction: 1,
				Rarities:        []int64{0, 1, 2, 3, 5, 6},
			},
			RandomIndependentAttacks: attacks,
		}, nil
	}
}

func saveRandomForgingTicketFixture(t *testing.T, ctx context.Context, repos dnfrepo.Group, forgingLevel byte) {
	t.Helper()
	saveForgingInventoryFixture(t, ctx, repos, true)
	record := loadTestInventory(t, ctx, repos, "77")
	target := record.Slots[slotKey(listTypeMain, 25)]
	setForgingLevel(&target, forgingLevel)
	// Like the fixed/additional coupons, the random family does not require
	// full durability.
	target.Extra["durability"] = "44"
	record.Slots[slotKey(listTypeMain, 25)] = target
	ticket := testForgingMaterialStack(2)
	ticket.ItemID = 2683788
	ticket.Extra["pvf_path"] = "stackable/cash/chn_20140701_hotsummer_package/chn_6_indeed_2683788.stk"
	record.Slots[slotKey(listTypeMain, 121)] = ticket
	saveTestInventory(t, ctx, repos, record)
}

func TestOwnerForgeRandomTicketRollsCandidateLevelAndConsumesOne(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	saveRandomForgingTicketFixture(t, ctx, repos, 0)

	owner, _ := NewOwner(repos)
	resolver := randomForgingTicketResolver(
		[]alignedcmd.UpgradeTicketRandomEntry{{TargetLevel: 6, Weight: 100000}},
		map[int64]int64{6: 456},
	)
	result, err := owner.Forge(ctx, testForgingCommand(listTypeMain, 25), resolver)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Success || !result.UpgradeSucceeded || result.OldLevel != 0 || result.NewLevel != 6 ||
		result.MaterialCost != 1 || result.MaterialRemainingCount != 1 || result.IndependentAttack != 456 {
		t.Fatalf("random ticket result = %+v", result)
	}
	// weight 100000 out of the er_enchant base must rescale to the native
	// op438 base of 10000, otherwise the roll can never succeed.
	if result.SuccessWeight != currentForgingSuccessBase {
		t.Fatalf("success weight = %d, want %d", result.SuccessWeight, currentForgingSuccessBase)
	}
	loaded := loadTestInventory(t, ctx, repos, "77")
	if got := forgingLevelOf(loaded.Slots[slotKey(listTypeMain, 25)]); got != 6 {
		t.Fatalf("random ticket forging level=%d, want 6", got)
	}
	if got := loaded.Slots[slotKey(listTypeMain, 121)].Count; got != 1 {
		t.Fatalf("random ticket remaining=%d, want 1", got)
	}
}

func TestOwnerForgeRandomTicketPicksWeightedCandidate(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	saveRandomForgingTicketFixture(t, ctx, repos, 0)

	owner, _ := NewOwner(repos)
	// Land the roll inside the second candidate (30000 <= roll < 90000).
	owner.enchantRandom = func(int) int { return 30000 }
	resolver := randomForgingTicketResolver(
		[]alignedcmd.UpgradeTicketRandomEntry{
			{TargetLevel: 4, Weight: 30000},
			{TargetLevel: 5, Weight: 60000},
			{TargetLevel: 6, Weight: 10000},
		},
		map[int64]int64{4: 100, 5: 200, 6: 300},
	)
	result, err := owner.Forge(ctx, testForgingCommand(listTypeMain, 25), resolver)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Success || !result.UpgradeSucceeded || result.NewLevel != 5 || result.IndependentAttack != 200 {
		t.Fatalf("weighted random ticket result = %+v", result)
	}
}

func TestOwnerForgeRandomTicketRejectsRestrictedTarget(t *testing.T) {
	ctx := context.Background()
	entries := []alignedcmd.UpgradeTicketRandomEntry{{TargetLevel: 6, Weight: 100000}}
	tests := []struct {
		name   string
		mutate func(*alignedcmd.ForgingResolution)
	}{
		{name: "rarity outside er_grade", mutate: func(value *alignedcmd.ForgingResolution) { value.TargetRarity = 4 }},
		{name: "er_slot demands another part", mutate: func(value *alignedcmd.ForgingResolution) {
			value.RandomTicket.SlotRestriction = 2
		}},
		{name: "er_seal demands a sealed target", mutate: func(value *alignedcmd.ForgingResolution) {
			value.RandomTicket.SealRestriction = 1
		}},
		{name: "candidate above the forging table", mutate: func(value *alignedcmd.ForgingResolution) {
			value.RandomTicket.Entries = []alignedcmd.UpgradeTicketRandomEntry{{TargetLevel: 9, Weight: 100000}}
		}},
		{name: "no candidates", mutate: func(value *alignedcmd.ForgingResolution) {
			value.RandomTicket.Entries = nil
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			repos := dnfrepomemory.NewMemoryGroup()
			saveRandomForgingTicketFixture(t, ctx, repos, 3)
			owner, _ := NewOwner(repos)
			base := randomForgingTicketResolver(entries, map[int64]int64{6: 456, 9: 789})
			resolver := func(materialItemID int64, targetItemID int64, currentLevel byte) (alignedcmd.ForgingResolution, error) {
				resolution, err := base(materialItemID, targetItemID, currentLevel)
				if err != nil {
					return resolution, err
				}
				test.mutate(&resolution)
				return resolution, nil
			}
			result, err := owner.Forge(ctx, testForgingCommand(listTypeMain, 25), resolver)
			if err != nil {
				t.Fatal(err)
			}
			if result.Success || result.Changed {
				t.Fatalf("restricted random ticket accepted: %+v", result)
			}
			loaded := loadTestInventory(t, ctx, repos, "77")
			if got := forgingLevelOf(loaded.Slots[slotKey(listTypeMain, 25)]); got != 3 {
				t.Fatalf("forging level mutated to %d", got)
			}
			if got := loaded.Slots[slotKey(listTypeMain, 121)].Count; got != 2 {
				t.Fatalf("ticket consumed on rejection: %d", got)
			}
		})
	}
}

func TestOwnerForgeRejectsWithoutMutation(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name     string
		mutate   func(*alignedcmd.ForgingResolution)
		material int64
	}{
		{name: "not weapon", mutate: func(value *alignedcmd.ForgingResolution) { value.TargetEquipmentType = "[coat]" }, material: 10},
		{name: "max level", mutate: func(value *alignedcmd.ForgingResolution) { value.MaxLevel = 2 }, material: 10},
		{name: "material insufficient", mutate: func(value *alignedcmd.ForgingResolution) { value.MaterialCount = 11 }, material: 10},
		{name: "durability not full", mutate: func(value *alignedcmd.ForgingResolution) { value.TargetDurability = 46 }, material: 10},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			repos := dnfrepomemory.NewMemoryGroup()
			saveForgingInventoryFixture(t, ctx, repos, true)
			owner, _ := NewOwner(repos)
			resolver := func(materialItemID int64, targetItemID int64, currentLevel byte) (alignedcmd.ForgingResolution, error) {
				value, _ := testForgingResolver(10000, 3)(3326, targetItemID, currentLevel)
				test.mutate(&value)
				return value, nil
			}
			result, err := owner.Forge(ctx, testForgingCommand(listTypeMain, 25), resolver)
			if err != nil {
				t.Fatal(err)
			}
			if result.Success || result.Changed {
				t.Fatalf("result = %+v", result)
			}
			loaded := loadTestInventory(t, ctx, repos, "77")
			if loaded.Slots[slotKey(listTypeMain, 121)].Count != test.material || loaded.Slots[slotKey(listTypeMain, 25)].RawEntry[currentForgingRawOffset] != 2 {
				t.Fatalf("state mutated: %+v", loaded.Slots)
			}
		})
	}
}
