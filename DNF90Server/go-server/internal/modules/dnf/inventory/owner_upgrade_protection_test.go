package inventory

import (
	"context"
	"encoding/binary"
	"strconv"
	"testing"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

func staticUpgradeProtectionResolver(mode string, retain int, expectedItemID int64) alignedcmd.UpgradeProtectionResolver {
	return func(itemID int64) (alignedcmd.UpgradeProtectionResolution, error) {
		if itemID != expectedItemID {
			return alignedcmd.UpgradeProtectionResolution{}, nil
		}
		return alignedcmd.UpgradeProtectionResolution{
			TicketMode:         mode,
			FailureRetainLevel: retain,
			TicketPVFPath:      "stackable/ticket/protection.stk",
		}, nil
	}
}

func protectedUpgradeFixture(t *testing.T, mode string, oldLevel byte, ticketItemID int64, ticketCount int64) (context.Context, dnfrepo.Group, *Owner, Command) {
	t.Helper()
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	saveTestCharacter(t, ctx, repos, dnfrepo.CharacterRecord{
		CharacterID: "77",
		AccountID:   "acc",
		Stats:       map[string]int64{"gold": 1000},
	})
	targetExtra := map[string]string{
		"item_kind":      "equipment",
		"durability":     "45",
		"max_durability": "45",
		"reinforce":      strconv.Itoa(int(oldLevel)),
		"upgrade_gold":   "100",
	}
	if mode == "amplify" {
		targetExtra["amplify_type"] = "3"
		targetExtra["amplify_value"] = "12"
	}
	saveTestInventory(t, ctx, repos, dnfrepo.InventoryRecord{
		CharacterID: "77",
		Slots: map[string]dnfrepo.ItemStack{
			slotKey(listTypeMain, 9):   {ItemID: 700, Count: 1, Extra: targetExtra},
			slotKey(listTypeMain, 121): {ItemID: 5000, Count: 3, Extra: map[string]string{"item_kind": "stackable"}},
			slotKey(listTypeMain, 130): {ItemID: ticketItemID, Count: ticketCount, Extra: map[string]string{"item_kind": "stackable"}},
		},
	})
	owner, err := NewOwner(repos)
	if err != nil {
		t.Fatal(err)
	}
	owner.upgradeRandom = func(int) int { return currentUpgradeSuccessBase - 1 }
	cmd := NewUpgradeCommand(alignedcmd.Request{AccountID: "acc", SelectedCharacterID: 77}, UpgradeItemRequest{
		Mode:                    mode,
		TargetSlotIndex:         9,
		TargetItemTemplateID:    700,
		MaterialSlotIndex:       121,
		OptionalTicketSlotIndex: 130,
	})
	cmd.UpgradeSuccessWeight = 1
	cmd.UpgradePenaltyType = 3
	cmd.UpgradeMaterialItemID = 5000
	cmd.UpgradeMaterialCount = 1
	return ctx, repos, owner, cmd
}

func TestOwnerUpgradeProtectionPreventsDestroyForBothModes(t *testing.T) {
	for _, tc := range []struct {
		name         string
		mode         string
		ticketItemID int64
		retain       int
		wantLevel    byte
	}{
		{name: "ordinary reinforce", mode: "reinforce", ticketItemID: 7804, retain: 0, wantLevel: 0},
		{name: "super reinforce", mode: "reinforce", ticketItemID: 10007840, retain: 10, wantLevel: 10},
		{name: "ordinary amplify", mode: "amplify", ticketItemID: 10004511, retain: 0, wantLevel: 0},
		{name: "super amplify", mode: "amplify", ticketItemID: 10007841, retain: 7, wantLevel: 7},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, repos, owner, cmd := protectedUpgradeFixture(t, tc.mode, 12, tc.ticketItemID, 2)
			owner.SetUpgradeProtectionResolver(staticUpgradeProtectionResolver(tc.mode, tc.retain, tc.ticketItemID))
			result, err := owner.Upgrade(ctx, cmd)
			if err != nil {
				t.Fatalf("Upgrade: %v", err)
			}
			if !result.Success || result.UpgradeSucceeded || !result.ProtectionTicketUsed || result.ResultCode != upgradeResultCodeDowngrade || result.NewLevel != tc.wantLevel || result.ProtectionTicketRemaining != 1 {
				t.Fatalf("result = %+v", result)
			}
			if result.DestroyBonusItemID != 0 || len(result.DestroyBonusSlots) != 0 {
				t.Fatalf("protected failure granted destroy compensation: %+v", result)
			}
			bag := loadTestInventory(t, ctx, repos, "77")
			if got := upgradeLevelOf(bag.Slots[slotKey(listTypeMain, 9)]); got != tc.wantLevel {
				t.Fatalf("target level = %d, want %d", got, tc.wantLevel)
			}
			if got := bag.Slots[slotKey(listTypeMain, 121)].Count; got != 2 {
				t.Fatalf("material count = %d, want 2", got)
			}
			if got := bag.Slots[slotKey(listTypeMain, 130)].Count; got != 1 {
				t.Fatalf("protection count = %d, want 1", got)
			}
			character, ok, loadErr := repos.Character.Load(ctx, "77")
			if loadErr != nil || !ok || character.Stats["gold"] != 900 {
				t.Fatalf("character=%+v ok=%t err=%v", character, ok, loadErr)
			}
		})
	}
}

func TestOwnerPortableAmplifierUsesOrdinaryFailureAndProtectionTransaction(t *testing.T) {
	ctx, repos, owner, cmd := protectedUpgradeFixture(t, "amplify", 12, 10004511, 2)
	bag := loadTestInventory(t, ctx, repos, "77")
	device := bag.Slots[slotKey(listTypeMain, 121)]
	device.ItemID = 10005313
	device.Count = 2
	bag.Slots[slotKey(listTypeMain, 121)] = device
	if err := repos.Inventory.Save(ctx, bag); err != nil {
		t.Fatal(err)
	}
	cmd.UpgradePortableAmplifier = true
	cmd.UpgradePortableMinimum = 0
	cmd.UpgradePortableMaximum = 50
	cmd.UpgradeTargetMinimumLevel = 85
	cmd.UpgradeTargetLevelMinimum = 1
	cmd.UpgradeTargetLevelMaximum = 85
	cmd.UpgradeMaterialItemID = 10005313
	cmd.UpgradeMaterialCount = 1
	owner.SetUpgradeProtectionResolver(staticUpgradeProtectionResolver("amplify", 0, 10004511))
	result, err := owner.Upgrade(ctx, cmd)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Success || result.UpgradeSucceeded || !result.ProtectionTicketUsed || result.ResultCode != upgradeResultCodeDowngrade || result.NewLevel != 0 || result.GoldCost != 0 {
		t.Fatalf("portable protected result=%+v", result)
	}
	bag = loadTestInventory(t, ctx, repos, "77")
	if got := bag.Slots[slotKey(listTypeMain, 121)].Count; got != 1 {
		t.Fatalf("portable amplifier count=%d", got)
	}
	if got := bag.Slots[slotKey(listTypeMain, 130)].Count; got != 1 {
		t.Fatalf("amplification protection count=%d", got)
	}
	character, found, err := repos.Character.Load(ctx, "77")
	if err != nil || !found || character.Stats["gold"] != 1000 {
		t.Fatalf("character=%+v found=%t err=%v", character, found, err)
	}
}

func TestOwnerReinforcementAndAmplificationFailureBoundaryAndProtection(t *testing.T) {
	for _, mode := range []string{"reinforce", "amplify"} {
		for _, oldLevel := range []byte{7, 8, 9} {
			t.Run(mode+" downgrade from plus "+strconv.Itoa(int(oldLevel)), func(t *testing.T) {
				ctx, repos, owner, cmd := protectedUpgradeFixture(t, mode, oldLevel, 7804, 2)
				cmd.OptionalTicketSlot = -1
				cmd.UpgradePenaltyType = 1
				result, err := owner.Upgrade(ctx, cmd)
				if err != nil || !result.Success || result.UpgradeSucceeded || result.ResultCode != upgradeResultCodeDowngrade || result.NewLevel != oldLevel-1 {
					t.Fatalf("result=%+v err=%v", result, err)
				}
				bag := loadTestInventory(t, ctx, repos, "77")
				if got := upgradeLevelOf(bag.Slots[slotKey(listTypeMain, 9)]); got != oldLevel-1 {
					t.Fatalf("target level=%d, want %d", got, oldLevel-1)
				}
				if got := bag.Slots[slotKey(listTypeMain, 130)].Count; got != 2 {
					t.Fatalf("unused protection count=%d, want 2", got)
				}
			})
		}
	}

	t.Run("plus ten destroys without protection", func(t *testing.T) {
		ctx, repos, owner, cmd := protectedUpgradeFixture(t, "reinforce", 10, 7804, 2)
		cmd.OptionalTicketSlot = -1
		cmd.UpgradePenaltyType = 3
		result, err := owner.Upgrade(ctx, cmd)
		if err != nil || !result.Success || result.UpgradeSucceeded || result.ResultCode != upgradeResultCodeDestroy {
			t.Fatalf("result=%+v err=%v", result, err)
		}
		if _, found := loadTestInventory(t, ctx, repos, "77").Slots[slotKey(listTypeMain, 9)]; found {
			t.Fatal("plus ten target survived an unprotected destructive failure")
		}
	})

	t.Run("plus ten ordinary protection resets to zero and consumes one", func(t *testing.T) {
		ctx, repos, owner, cmd := protectedUpgradeFixture(t, "reinforce", 10, 7804, 2)
		owner.SetUpgradeProtectionResolver(staticUpgradeProtectionResolver("reinforce", 0, 7804))
		result, err := owner.Upgrade(ctx, cmd)
		if err != nil || !result.Success || result.UpgradeSucceeded || !result.ProtectionTicketUsed || result.ResultCode != upgradeResultCodeDowngrade || result.NewLevel != 0 || result.ProtectionTicketRemaining != 1 {
			t.Fatalf("result=%+v err=%v", result, err)
		}
		bag := loadTestInventory(t, ctx, repos, "77")
		if got := upgradeLevelOf(bag.Slots[slotKey(listTypeMain, 9)]); got != 0 {
			t.Fatalf("protected target level=%d, want 0", got)
		}
		if got := bag.Slots[slotKey(listTypeMain, 130)].Count; got != 1 {
			t.Fatalf("protection count=%d, want 1", got)
		}
	})
}

func TestOwnerUpgradeAutoDiscoversProtectionWhenClientSendsMinusOne(t *testing.T) {
	for _, tc := range []struct {
		name         string
		mode         string
		ticketItemID int64
	}{
		{name: "reinforcement protection", mode: "reinforce", ticketItemID: 7804},
		{name: "amplification protection", mode: "amplify", ticketItemID: 10004511},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, repos, owner, cmd := protectedUpgradeFixture(t, tc.mode, 10, tc.ticketItemID, 2)
			cmd.OptionalTicketSlot = -1
			owner.SetUpgradeProtectionResolver(staticUpgradeProtectionResolver(tc.mode, 0, tc.ticketItemID))
			result, err := owner.Upgrade(ctx, cmd)
			if err != nil || !result.Success || result.UpgradeSucceeded || !result.ProtectionTicketUsed || result.ResultCode != upgradeResultCodeDowngrade || result.NewLevel != 0 {
				t.Fatalf("result=%+v err=%v", result, err)
			}
			if result.OptionalTicketSlotIndex != -1 || result.ProtectionTicketSlotIndex != 130 || result.ProtectionTicketRemaining != 1 {
				t.Fatalf("ticket projection=%+v", result)
			}
			bag := loadTestInventory(t, ctx, repos, "77")
			if got := upgradeLevelOf(bag.Slots[slotKey(listTypeMain, 9)]); got != 0 {
				t.Fatalf("protected target level=%d, want 0", got)
			}
			if got := bag.Slots[slotKey(listTypeMain, 130)].Count; got != 1 {
				t.Fatalf("protection count=%d, want 1", got)
			}
		})
	}
}

func TestOwnerUpgradeProtectionNotConsumedOnSuccessOrNonDestroyFailure(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		ctx, repos, owner, cmd := protectedUpgradeFixture(t, "reinforce", 12, 7804, 2)
		owner.SetUpgradeProtectionResolver(staticUpgradeProtectionResolver("reinforce", 0, 7804))
		owner.upgradeRandom = func(int) int { return 0 }
		cmd.UpgradeSuccessWeight = currentUpgradeSuccessBase
		result, err := owner.Upgrade(ctx, cmd)
		if err != nil || !result.UpgradeSucceeded || result.ProtectionTicketUsed {
			t.Fatalf("result=%+v err=%v", result, err)
		}
		if got := loadTestInventory(t, ctx, repos, "77").Slots[slotKey(listTypeMain, 130)].Count; got != 2 {
			t.Fatalf("protection count = %d, want 2", got)
		}
	})

	t.Run("PVF value downgrade", func(t *testing.T) {
		ctx, repos, owner, cmd := protectedUpgradeFixture(t, "reinforce", 10, 7804, 2)
		owner.SetUpgradeProtectionResolver(staticUpgradeProtectionResolver("reinforce", 0, 7804))
		cmd.UpgradePenaltyType = 2
		cmd.UpgradePenaltyValue = 3
		result, err := owner.Upgrade(ctx, cmd)
		if err != nil || result.UpgradeSucceeded || result.ProtectionTicketUsed || result.NewLevel != 7 || result.ResultCode != upgradeResultCodeDowngrade {
			t.Fatalf("result=%+v err=%v", result, err)
		}
		if got := loadTestInventory(t, ctx, repos, "77").Slots[slotKey(listTypeMain, 130)].Count; got != 2 {
			t.Fatalf("protection count = %d, want 2", got)
		}
	})
}

func TestOwnerUpgradeProtectionDeletesLastTicket(t *testing.T) {
	ctx, repos, owner, cmd := protectedUpgradeFixture(t, "reinforce", 12, 7804, 1)
	owner.SetUpgradeProtectionResolver(staticUpgradeProtectionResolver("reinforce", 0, 7804))
	result, err := owner.Upgrade(ctx, cmd)
	if err != nil || !result.ProtectionTicketUsed || result.ProtectionTicketRemaining != 0 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if _, found := loadTestInventory(t, ctx, repos, "77").Slots[slotKey(listTypeMain, 130)]; found {
		t.Fatal("last protection ticket remained in the inventory")
	}
}

func TestOwnerUpgradeProtectionRejectsWrongOrForgedTicketWithoutMutation(t *testing.T) {
	for _, tc := range []struct {
		name     string
		resolver alignedcmd.UpgradeProtectionResolver
		wantCode byte
	}{
		{name: "missing resolver", resolver: nil, wantCode: upgradeErrorUnsupportedOptionalTicket},
		{name: "not protection", resolver: staticUpgradeProtectionResolver("reinforce", 0, 9999), wantCode: upgradeErrorUnsupportedOptionalTicket},
		{name: "wrong mode", resolver: staticUpgradeProtectionResolver("amplify", 0, 7804), wantCode: upgradeErrorWrongMode},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, repos, owner, cmd := protectedUpgradeFixture(t, "reinforce", 12, 7804, 2)
			owner.SetUpgradeProtectionResolver(tc.resolver)
			result, err := owner.Upgrade(ctx, cmd)
			if err != nil || result.Success || result.ErrorCode != tc.wantCode {
				t.Fatalf("result=%+v err=%v", result, err)
			}
			bag := loadTestInventory(t, ctx, repos, "77")
			if upgradeLevelOf(bag.Slots[slotKey(listTypeMain, 9)]) != 12 || bag.Slots[slotKey(listTypeMain, 121)].Count != 3 || bag.Slots[slotKey(listTypeMain, 130)].Count != 2 {
				t.Fatalf("inventory mutated: %+v", bag.Slots)
			}
		})
	}
}

func TestUpgradeProtectionAckAndRefreshUseExactTicketSlot(t *testing.T) {
	result := UpgradeResult{
		Mode:                         "amplify",
		Success:                      true,
		TargetSlotIndex:              9,
		MaterialSlotIndex:            121,
		OptionalTicketSlotIndex:      130,
		OldLevel:                     12,
		NewLevel:                     7,
		ResultCode:                   upgradeResultCodeDowngrade,
		MaterialRemainingStackCount:  2,
		ProtectionTicketUsed:         true,
		ProtectionTicketRemaining:    0,
		TargetUpdatedStack:           dnfrepo.ItemStack{ItemID: 700, Count: 1},
		MaterialUpdated:              true,
		MaterialUpdatedStack:         dnfrepo.ItemStack{ItemID: 5000, Count: 2},
		ProtectionTicketUpdatedStack: dnfrepo.ItemStack{},
	}
	wantAck := []byte{1, 1, 121, 0, 2, 0, 0, 0, 130, 0, 0, 12, 2, 7, 0, 9, 0, 130, 0}
	if got := buildUpgradeItemSuccessAck(result); string(got) != string(wantAck) {
		t.Fatalf("ack=% X want=% X", got, wantAck)
	}
	entries := upgradeItemUpdateEntries(result, false)
	if len(entries) != 3 || entries[0].slot != 9 || entries[1].slot != 121 || entries[2].slot != 130 || entries[2].stack.ItemID != 0 {
		t.Fatalf("entries=%+v", entries)
	}
}

func TestAutoDiscoveredUpgradeProtectionKeepsMinusOneAckAndRefreshesActualSlot(t *testing.T) {
	result := UpgradeResult{
		Mode:                         "amplify",
		Success:                      true,
		TargetSlotIndex:              9,
		MaterialSlotIndex:            121,
		OptionalTicketSlotIndex:      -1,
		ProtectionTicketSlotIndex:    130,
		OldLevel:                     10,
		NewLevel:                     0,
		ResultCode:                   upgradeResultCodeDowngrade,
		MaterialRemainingStackCount:  2,
		ProtectionTicketUsed:         true,
		ProtectionTicketRemaining:    1,
		TargetUpdatedStack:           dnfrepo.ItemStack{ItemID: 700, Count: 1},
		MaterialUpdated:              true,
		MaterialUpdatedStack:         dnfrepo.ItemStack{ItemID: 5000, Count: 2},
		ProtectionTicketUpdatedStack: dnfrepo.ItemStack{ItemID: 10004511, Count: 1},
	}
	wantAck := []byte{1, 1, 121, 0, 2, 0, 0, 0, 0xFF, 0xFF, 0, 10, 2, 0, 0, 9, 0, 0xFF, 0xFF}
	if got := buildUpgradeItemSuccessAck(result); string(got) != string(wantAck) {
		t.Fatalf("ack=% X want=% X", got, wantAck)
	}
	entries := upgradeItemUpdateEntries(result, false)
	if len(entries) != 3 || entries[0].slot != 9 || entries[1].slot != 121 || entries[2].slot != 130 || entries[2].stack.ItemID != 10004511 || entries[2].stack.Count != 1 {
		t.Fatalf("entries=%+v", entries)
	}
}

func TestHandlerUpgradeProtectionResolverReachesOwner(t *testing.T) {
	ctx, repos, _, _ := protectedUpgradeFixture(t, "reinforce", 12, 7804, 2)
	body := make([]byte, 16)
	binary.LittleEndian.PutUint16(body[0:2], 0)
	binary.LittleEndian.PutUint16(body[2:4], 9)
	binary.LittleEndian.PutUint32(body[4:8], 700)
	binary.LittleEndian.PutUint16(body[8:10], 121)
	binary.LittleEndian.PutUint16(body[10:12], 130)

	got, err := NewHandler().Handle(ctx, alignedcmd.Request{
		Opcode:                    uint16(dnfenum.CmdPacketUpgradeItem),
		Body:                      body,
		AccountID:                 "acc",
		SelectedCharacterID:       77,
		Repositories:              repos,
		UpgradeProtectionResolver: staticUpgradeProtectionResolver("amplify", 0, 7804),
		UpgradePolicyResolver: func(mode string, currentLevel int, targetItemID int64, amplifyType int) (alignedcmd.UpgradePolicyResolution, error) {
			return alignedcmd.UpgradePolicyResolution{
				SuccessWeight:  currentUpgradeSuccessBase,
				PenaltyType:    3,
				MaterialItemID: 5000,
				MaterialCount:  1,
			}, nil
		},
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(got.UpperResponses) != 1 || string(got.UpperResponses[0].Body) != string([]byte{0, upgradeErrorWrongMode}) {
		t.Fatalf("result=%+v", got)
	}
	bag := loadTestInventory(t, ctx, repos, "77")
	if upgradeLevelOf(bag.Slots[slotKey(listTypeMain, 9)]) != 12 || bag.Slots[slotKey(listTypeMain, 121)].Count != 3 || bag.Slots[slotKey(listTypeMain, 130)].Count != 2 {
		t.Fatalf("inventory mutated: %+v", bag.Slots)
	}
}

func TestHandlerPortableAmplifierRoutesThroughOrdinaryUpgradeOwner(t *testing.T) {
	ctx, repos, _, _ := protectedUpgradeFixture(t, "amplify", 0, 10004511, 1)
	bag := loadTestInventory(t, ctx, repos, "77")
	portable := bag.Slots[slotKey(listTypeMain, 121)]
	portable.ItemID = 10005313
	portable.Count = 2
	bag.Slots[slotKey(listTypeMain, 121)] = portable
	if err := repos.Inventory.Save(ctx, bag); err != nil {
		t.Fatal(err)
	}

	body := make([]byte, 16)
	binary.LittleEndian.PutUint16(body[0:2], 1)
	binary.LittleEndian.PutUint16(body[2:4], 9)
	binary.LittleEndian.PutUint32(body[4:8], 700)
	binary.LittleEndian.PutUint16(body[8:10], 121)
	binary.LittleEndian.PutUint16(body[10:12], ^uint16(0))

	got, err := NewHandler().Handle(ctx, alignedcmd.Request{
		Opcode:              uint16(dnfenum.CmdPacketUpgradeItem),
		Body:                body,
		AccountID:           "acc",
		SelectedCharacterID: 77,
		Repositories:        repos,
		UpgradeTicketResolver: func(materialItemID, targetItemID int64) (alignedcmd.UpgradeTicketResolution, error) {
			return alignedcmd.UpgradeTicketResolution{
				PortableAmplifier:    materialItemID == 10005313 && targetItemID == 700,
				PortableMinimumLevel: 0,
				PortableMaximumLevel: 50,
				TargetMinimumLevel:   85,
				TargetLevelMinimum:   1,
				TargetLevelMaximum:   85,
				TargetKind:           "equipment",
				TargetEquipmentType:  "[weapon]",
			}, nil
		},
		UpgradePolicyResolver: func(mode string, currentLevel int, targetItemID int64, amplifyType int) (alignedcmd.UpgradePolicyResolution, error) {
			return alignedcmd.UpgradePolicyResolution{
				SuccessWeight:  currentUpgradeSuccessBase,
				PenaltyType:    3,
				MaterialItemID: 5000,
				MaterialCount:  99,
				GoldCost:       777,
			}, nil
		},
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(got.UpperResponses) == 0 || len(got.UpperResponses[len(got.UpperResponses)-1].Body) == 0 || got.UpperResponses[len(got.UpperResponses)-1].Body[0] != 1 {
		t.Fatalf("result=%+v", got)
	}
	ack := got.UpperResponses[0].Body
	if len(ack) < 8 || binary.LittleEndian.Uint32(ack[4:8]) != 1 {
		t.Fatalf("portable amplifier ACK material remaining=%x want=1", ack)
	}
	bag = loadTestInventory(t, ctx, repos, "77")
	if upgradeLevelOf(bag.Slots[slotKey(listTypeMain, 9)]) != 1 || bag.Slots[slotKey(listTypeMain, 121)].ItemID != 10005313 || bag.Slots[slotKey(listTypeMain, 121)].Count != 1 {
		t.Fatalf("portable amplifier mutation=%+v", bag.Slots)
	}
	character, found, loadErr := repos.Character.Load(ctx, "77")
	if loadErr != nil || !found || character.Stats["gold"] != 1000 {
		t.Fatalf("portable amplifier charged gold: character=%+v found=%t err=%v", character, found, loadErr)
	}
}

func TestHandlerPortableReinforcerConsumesDeviceInsteadOfCrystalBalance(t *testing.T) {
	ctx, repos, _, _ := protectedUpgradeFixture(t, "reinforce", 10, 7804, 1)
	bag := loadTestInventory(t, ctx, repos, "77")
	portable := bag.Slots[slotKey(listTypeMain, 121)]
	portable.ItemID = 490702163
	portable.Count = 2
	bag.Slots[slotKey(listTypeMain, 121)] = portable
	if err := repos.Inventory.Save(ctx, bag); err != nil {
		t.Fatal(err)
	}
	if err := repos.AccountInventory.Save(ctx, dnfrepo.AccountInventoryRecord{
		AccountID: "acc",
		Slots: map[string]dnfrepo.ItemStack{
			dnfrepo.AccountSharedInventorySlotKey(358): {ItemID: 3037, Count: 20000},
		},
	}); err != nil {
		t.Fatal(err)
	}

	body := make([]byte, 16)
	binary.LittleEndian.PutUint16(body[0:2], 0)
	binary.LittleEndian.PutUint16(body[2:4], 9)
	binary.LittleEndian.PutUint32(body[4:8], 700)
	binary.LittleEndian.PutUint16(body[8:10], 121)
	binary.LittleEndian.PutUint16(body[10:12], ^uint16(0))

	got, err := NewHandler().Handle(ctx, alignedcmd.Request{
		Opcode:              uint16(dnfenum.CmdPacketUpgradeItem),
		Body:                body,
		AccountID:           "acc",
		SelectedCharacterID: 77,
		Repositories:        repos,
		UpgradeTicketResolver: func(materialItemID, targetItemID int64) (alignedcmd.UpgradeTicketResolution, error) {
			return alignedcmd.UpgradeTicketResolution{
				PortableReinforcer:   materialItemID == 490702163 && targetItemID == 700,
				PortableMinimumLevel: 10,
				PortableMaximumLevel: 30,
				TargetMinimumLevel:   85,
				TargetLevelMinimum:   85,
				TargetLevelMaximum:   90,
				TargetKind:           "equipment",
				TargetEquipmentType:  "[weapon]",
			}, nil
		},
		UpgradePolicyResolver: func(mode string, currentLevel int, targetItemID int64, amplifyType int) (alignedcmd.UpgradePolicyResolution, error) {
			return alignedcmd.UpgradePolicyResolution{
				SuccessWeight:  currentUpgradeSuccessBase,
				MaterialItemID: 3037,
				MaterialCount:  9999,
				GoldCost:       777,
			}, nil
		},
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(got.UpperResponses) == 0 || len(got.UpperResponses[len(got.UpperResponses)-1].Body) == 0 || got.UpperResponses[len(got.UpperResponses)-1].Body[0] != 1 {
		t.Fatalf("result=%+v", got)
	}
	ack := got.UpperResponses[0].Body
	if len(ack) < 8 || binary.LittleEndian.Uint32(ack[4:8]) != 1 {
		t.Fatalf("portable reinforcer ACK material remaining=%x want=1", ack)
	}
	bag = loadTestInventory(t, ctx, repos, "77")
	if upgradeLevelOf(bag.Slots[slotKey(listTypeMain, 9)]) != 11 || bag.Slots[slotKey(listTypeMain, 121)].ItemID != 490702163 || bag.Slots[slotKey(listTypeMain, 121)].Count != 1 {
		t.Fatalf("portable reinforcer mutation=%+v", bag.Slots)
	}
	character, found, loadErr := repos.Character.Load(ctx, "77")
	if loadErr != nil || !found || character.Stats["gold"] != 1000 {
		t.Fatalf("portable reinforcer charged gold: character=%+v found=%t err=%v", character, found, loadErr)
	}
	account, found, loadErr := repos.AccountInventory.Load(ctx, "acc")
	if loadErr != nil || !found || account.Slots[dnfrepo.AccountSharedInventorySlotKey(358)].Count != 20000 {
		t.Fatalf("portable reinforcer consumed crystal balance: account=%+v found=%t err=%v", account, found, loadErr)
	}
}
