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

const (
	testMonsterCardTarget   int64 = 3653
	testMonsterCardMaterial int64 = 3654
)

func testMonsterCardStack(itemID, count int64, upgrade byte) dnfrepo.ItemStack {
	raw := make([]byte, currentItemListEntrySize)
	binary.LittleEndian.PutUint32(raw[0x06:0x0A], uint32(count))
	raw[0x12] = upgrade
	return dnfrepo.ItemStack{
		ItemID:   itemID,
		Count:    count,
		RawEntry: raw,
		Extra: map[string]string{
			"item_kind":             "stackable",
			"pvf_path":              "stackable/monstercard/test.stk",
			"stackable_type":        "[material expert job]",
			"stack_limit":           "1000",
			"enchant_upgrade_count": string([]byte{'0' + upgrade}),
			"byte_12":               string([]byte{'0' + upgrade}),
		},
	}
}

func testMonsterCardResolver(targetRarity, materialRarity int) alignedcmd.MonsterCardUpgradeResolver {
	return func(itemID int64) (alignedcmd.MonsterCardUpgradeResolution, error) {
		rarity := materialRarity
		maximum := byte(0)
		if itemID == testMonsterCardTarget {
			rarity = targetRarity
			maximum = 3
		}
		return alignedcmd.MonsterCardUpgradeResolution{
			ItemID:         itemID,
			PVFPath:        "stackable/monstercard/test.stk",
			Rarity:         rarity,
			MaximumUpgrade: maximum,
			StackLimit:     1000,
			SlotStart:      233,
			SlotEnd:        236,
			GoldCost:       5000,
			CalculateConst: 10,
		}, nil
	}
}

func saveMonsterCardFixture(t *testing.T, repos dnfrepo.Group, gold int64, slots map[string]dnfrepo.ItemStack) {
	t.Helper()
	ctx := context.Background()
	saveTestInventory(t, ctx, repos, dnfrepo.InventoryRecord{CharacterID: "77", Slots: slots})
	saveTestCharacter(t, ctx, repos, dnfrepo.CharacterRecord{
		CharacterID: "77",
		AccountID:   "acc",
		Stats:       map[string]int64{"gold": gold},
	})
}

func monsterCardUpgradeCommand(targetSlot, materialSlot int16) Command {
	return NewMonsterCardUpgradeCommand(alignedcmd.Request{
		AccountID:           "acc",
		SelectedCharacterID: 77,
	}, MonsterCardUpgradeRequest{
		ListType:      listTypeMain,
		TargetSlot:    targetSlot,
		MaterialCount: 1,
		MaterialSlot:  materialSlot,
	})
}

func loadMonsterCardGold(t *testing.T, repos dnfrepo.Group) int64 {
	t.Helper()
	record, found, err := repos.Character.Load(context.Background(), "77")
	if err != nil || !found {
		t.Fatalf("Load character found=%t err=%v", found, err)
	}
	return record.Stats["gold"]
}

func TestDecodeMonsterCardUpgradeRequestMatchesCurrentNoPackBody(t *testing.T) {
	got, err := DecodeMonsterCardUpgradeRequest([]byte{0, 0xEA, 0, 1, 0, 0xEA, 0})
	if err != nil {
		t.Fatal(err)
	}
	if got.ListType != 0 || got.TargetSlot != 234 || got.MaterialCount != 1 || got.MaterialSlot != 234 {
		t.Fatalf("decoded request = %+v", got)
	}
	if _, err := DecodeMonsterCardUpgradeRequest([]byte{0, 0xEA, 0}); err == nil {
		t.Fatal("short op603 body unexpectedly decoded")
	}
}

func TestOwnerMonsterCardUpgradeSameSlotTwoCardsGuaranteed(t *testing.T) {
	repos := dnfrepomemory.NewMemoryGroup()
	saveMonsterCardFixture(t, repos, 10000, map[string]dnfrepo.ItemStack{
		slotKey(listTypeMain, 234): testMonsterCardStack(testMonsterCardTarget, 2, 0),
	})
	owner, err := NewOwner(repos)
	if err != nil {
		t.Fatal(err)
	}
	result, err := owner.UpgradeMonsterCard(context.Background(), monsterCardUpgradeCommand(234, 234), testMonsterCardResolver(3, 3), alignedcmd.MonsterCardUpgradePolicy{})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Success || !result.UpgradeSucceeded || result.UpgradeCount != 1 || result.Chance != monsterCardUpgradeProbabilityBase || result.UpdatedGold != 5000 {
		t.Fatalf("result = %+v", result)
	}
	if len(result.ChangedSlots) != 1 || result.ChangedSlots[0] != 234 {
		t.Fatalf("changed slots = %v", result.ChangedSlots)
	}
	stack := loadTestInventory(t, context.Background(), repos, "77").Slots[slotKey(listTypeMain, 234)]
	if stack.Count != 1 || monsterCardUpgradeCountOf(stack) != 1 || stack.RawEntry[0x12] != 1 {
		t.Fatalf("upgraded stack = %+v raw12=%d", stack, stack.RawEntry[0x12])
	}
	if got := loadMonsterCardGold(t, repos); got != 5000 {
		t.Fatalf("gold = %d, want 5000", got)
	}
}

func TestMonsterCardUpgradeChanceSupportsCurrentHighRarityCards(t *testing.T) {
	target := alignedcmd.MonsterCardUpgradeResolution{Rarity: 6, CalculateConst: 10}
	if got := monsterCardUpgradeChance(target, alignedcmd.MonsterCardUpgradeResolution{Rarity: 6}, true); got != monsterCardUpgradeProbabilityBase {
		t.Fatalf("same-card rarity-6 chance=%d", got)
	}
	if got := monsterCardUpgradeChance(target, alignedcmd.MonsterCardUpgradeResolution{Rarity: 6}, false); got != 50000 {
		t.Fatalf("rarity-6 tier-saturated chance=%d", got)
	}
}

func TestOwnerMonsterCardUpgradeAcceptsCurrentRaritySixCard(t *testing.T) {
	repos := dnfrepomemory.NewMemoryGroup()
	saveMonsterCardFixture(t, repos, 10000, map[string]dnfrepo.ItemStack{
		slotKey(listTypeMain, 234): testMonsterCardStack(10159184, 1, 0),
		slotKey(listTypeMain, 233): testMonsterCardStack(10159184, 1, 0),
	})
	owner, _ := NewOwner(repos)
	owner.cardUpgradeRandom = func(int) int { return monsterCardUpgradeProbabilityBase - 1 }
	resolver := func(itemID int64) (alignedcmd.MonsterCardUpgradeResolution, error) {
		return alignedcmd.MonsterCardUpgradeResolution{
			ItemID:         itemID,
			PVFPath:        "stackable/10159001/10159184.stk",
			Rarity:         6,
			MaximumUpgrade: 2,
			CalculateConst: 10,
			GoldCost:       5000,
			StackLimit:     1000,
			SlotStart:      0,
			SlotEnd:        240,
		}, nil
	}
	result, err := owner.UpgradeMonsterCard(context.Background(), monsterCardUpgradeCommand(234, 233), resolver, alignedcmd.MonsterCardUpgradePolicy{})
	if err != nil || !result.Success || !result.UpgradeSucceeded || result.UpgradeCount != 1 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestOwnerMonsterCardUpgradeSameSlotThreeCardsSplitsUpgradeState(t *testing.T) {
	repos := dnfrepomemory.NewMemoryGroup()
	saveMonsterCardFixture(t, repos, 10000, map[string]dnfrepo.ItemStack{
		slotKey(listTypeMain, 234): testMonsterCardStack(testMonsterCardTarget, 3, 0),
	})
	owner, _ := NewOwner(repos)
	result, err := owner.UpgradeMonsterCard(context.Background(), monsterCardUpgradeCommand(234, 234), testMonsterCardResolver(2, 2), alignedcmd.MonsterCardUpgradePolicy{})
	if err != nil || !result.Success || !result.UpgradeSucceeded || result.ResultSlot != 233 {
		t.Fatalf("result = %+v err=%v", result, err)
	}
	loaded := loadTestInventory(t, context.Background(), repos, "77")
	base := loaded.Slots[slotKey(listTypeMain, 234)]
	upgraded := loaded.Slots[slotKey(listTypeMain, 233)]
	if base.Count != 1 || monsterCardUpgradeCountOf(base) != 0 {
		t.Fatalf("base stack = %+v", base)
	}
	if upgraded.Count != 1 || upgraded.ItemID != testMonsterCardTarget || monsterCardUpgradeCountOf(upgraded) != 1 {
		t.Fatalf("upgraded stack = %+v", upgraded)
	}
}

func TestOwnerMonsterCardUpgradeFailedRollConsumesOnlyMaterialAndGold(t *testing.T) {
	repos := dnfrepomemory.NewMemoryGroup()
	saveMonsterCardFixture(t, repos, 10000, map[string]dnfrepo.ItemStack{
		slotKey(listTypeMain, 234): testMonsterCardStack(testMonsterCardTarget, 1, 0),
		slotKey(listTypeMain, 235): testMonsterCardStack(testMonsterCardMaterial, 1, 0),
	})
	owner, _ := NewOwner(repos)
	owner.cardUpgradeRandom = func(int) int { return monsterCardUpgradeProbabilityBase - 1 }
	result, err := owner.UpgradeMonsterCard(context.Background(), monsterCardUpgradeCommand(234, 235), testMonsterCardResolver(3, 0), alignedcmd.MonsterCardUpgradePolicy{})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Success || result.UpgradeSucceeded || result.Chance != 6250 || result.UpgradeCount != 0 || result.UpdatedGold != 5000 {
		t.Fatalf("result = %+v", result)
	}
	loaded := loadTestInventory(t, context.Background(), repos, "77")
	if _, found := loaded.Slots[slotKey(listTypeMain, 235)]; found {
		t.Fatal("failed-roll material was not consumed")
	}
	target := loaded.Slots[slotKey(listTypeMain, 234)]
	if target.Count != 1 || monsterCardUpgradeCountOf(target) != 0 {
		t.Fatalf("failed-roll target changed: %+v", target)
	}
}

func TestOwnerMonsterCardUpgradeStackedTargetSplitsOnSuccess(t *testing.T) {
	repos := dnfrepomemory.NewMemoryGroup()
	saveMonsterCardFixture(t, repos, 10000, map[string]dnfrepo.ItemStack{
		slotKey(listTypeMain, 234): testMonsterCardStack(testMonsterCardTarget, 2, 0),
		slotKey(listTypeMain, 235): testMonsterCardStack(testMonsterCardMaterial, 1, 0),
	})
	owner, _ := NewOwner(repos)
	result, err := owner.UpgradeMonsterCard(context.Background(), monsterCardUpgradeCommand(234, 235), testMonsterCardResolver(1, 2), alignedcmd.MonsterCardUpgradePolicy{})
	if err != nil || !result.Success || !result.UpgradeSucceeded || result.ResultSlot != 233 {
		t.Fatalf("result = %+v err=%v", result, err)
	}
	loaded := loadTestInventory(t, context.Background(), repos, "77")
	if got := loaded.Slots[slotKey(listTypeMain, 234)]; got.Count != 1 || monsterCardUpgradeCountOf(got) != 0 {
		t.Fatalf("target remainder = %+v", got)
	}
	if got := loaded.Slots[slotKey(listTypeMain, 233)]; got.Count != 1 || monsterCardUpgradeCountOf(got) != 1 {
		t.Fatalf("upgrade result = %+v", got)
	}
}

func TestOwnerMonsterCardUpgradeRejectsWithoutMutatingWalletOrCards(t *testing.T) {
	for _, test := range []struct {
		name      string
		gold      int64
		upgrade   byte
		count     int64
		fillSlots bool
		wantError byte
	}{
		{name: "insufficient gold", gold: 4999, count: 2, wantError: monsterCardUpgradeErrorGold},
		{name: "maximum upgrade", gold: 10000, upgrade: 3, count: 2, wantError: monsterCardUpgradeErrorInvalid},
		{name: "full inventory", gold: 10000, count: 3, fillSlots: true, wantError: monsterCardUpgradeErrorInventoryFull},
	} {
		t.Run(test.name, func(t *testing.T) {
			repos := dnfrepomemory.NewMemoryGroup()
			slots := map[string]dnfrepo.ItemStack{
				slotKey(listTypeMain, 234): testMonsterCardStack(testMonsterCardTarget, test.count, test.upgrade),
			}
			if test.fillSlots {
				for _, slot := range []int16{233, 235, 236} {
					slots[slotKey(listTypeMain, slot)] = testMonsterCardStack(9000+int64(slot), 1, 0)
				}
			}
			saveMonsterCardFixture(t, repos, test.gold, slots)
			owner, _ := NewOwner(repos)
			result, err := owner.UpgradeMonsterCard(context.Background(), monsterCardUpgradeCommand(234, 234), testMonsterCardResolver(3, 3), alignedcmd.MonsterCardUpgradePolicy{})
			if err != nil {
				t.Fatal(err)
			}
			if result.Success || result.Changed || result.ErrorCode != test.wantError {
				t.Fatalf("result = %+v", result)
			}
			loaded := loadTestInventory(t, context.Background(), repos, "77")
			got := loaded.Slots[slotKey(listTypeMain, 234)]
			if got.Count != test.count || monsterCardUpgradeCountOf(got) != test.upgrade {
				t.Fatalf("target mutated: %+v", got)
			}
			if gotGold := loadMonsterCardGold(t, repos); gotGold != test.gold {
				t.Fatalf("gold mutated: %d want=%d", gotGold, test.gold)
			}
		})
	}
}

func TestHandlerMonsterCardUpgradeReturnsNativeAckAndIncrementalRefresh(t *testing.T) {
	repos := dnfrepomemory.NewMemoryGroup()
	saveMonsterCardFixture(t, repos, 10000, map[string]dnfrepo.ItemStack{
		slotKey(listTypeMain, 234): testMonsterCardStack(testMonsterCardTarget, 2, 0),
	})
	result, err := NewHandler().Handle(context.Background(), alignedcmd.Request{
		Opcode:                     uint16(dnfenum.CmdPacketUpgradeCard),
		Body:                       []byte{0, 0xEA, 0, 1, 0, 0xEA, 0},
		AccountID:                  "acc",
		SelectedCharacterID:        77,
		Repositories:               repos,
		MonsterCardUpgradeResolver: testMonsterCardResolver(3, 3),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Handled || !result.ResponseAllowed || result.Operation != "upgrade_monster_card" || len(result.UpperResponses) != 1 {
		t.Fatalf("result = %+v", result)
	}
	ack := result.UpperResponses[0]
	wantAck := []byte{1, 1, 0x45, 0x0E, 0, 0, 1}
	if ack.Classification != 1 || ack.MsgID != uint16(dnfenum.CmdPacketUpgradeCard) || !bytes.Equal(ack.Body, wantAck) {
		t.Fatalf("ack = class%d msg0x%04X body=% X want=% X", ack.Classification, ack.MsgID, ack.Body, wantAck)
	}
	if len(result.ItemSlotRefreshes) != 1 || result.ItemSlotRefreshes[0].ListType != listTypeMain || result.ItemSlotRefreshes[0].SlotIndex != 234 {
		t.Fatalf("slot refreshes = %+v", result.ItemSlotRefreshes)
	}
	if result.CharacterGoldAfter == nil || *result.CharacterGoldAfter != 5000 {
		t.Fatalf("gold after = %v", result.CharacterGoldAfter)
	}
	if len(result.PostActions) != 0 {
		t.Fatalf("unexpected full-refresh post actions = %v", result.PostActions)
	}
}
