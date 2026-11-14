package dnfbridge

import (
	"context"
	"strconv"
	"testing"

	dnfchat "longheng.io/server/internal/modules/dnf/chat"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfmemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

func TestPrepareRobotServerMegaphoneSupportsManuallyCreatedRobotAccount(t *testing.T) {
	const accountID = "manual-robot-004"
	const characterID = uint16(24)
	group := dnfmemory.NewMemoryGroup()
	group.CharacterAssets = &fakeCharacterAssetUnitOfWork{
		character: group.Character,
		inventory: group.Inventory,
		equipment: group.Equipment,
	}
	ctx := context.Background()
	if err := group.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "24", AccountID: accountID, Slot: 1, Name: "manual robot",
	}); err != nil {
		t.Fatal(err)
	}
	if err := group.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "24", Slots: map[string]dnfrepo.ItemStack{
			"0:121": {ItemID: 999, Count: 1},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if group.CharacterAssets == nil {
		t.Fatal("memory character asset transaction is unavailable")
	}
	if _, found, err := group.Inventory.Load(ctx, "24"); err != nil || !found {
		t.Fatalf("prepared inventory: found=%t error=%v", found, err)
	}
	catalog := &pvfDungeonDropCatalog{
		source:    finishBridgePVFSource{},
		itemCache: make(map[uint32]dungeonDropItemDefinition),
	}
	catalog.itemCache[dnfchat.ServerMegaphoneItemID] = dungeonDropItemDefinition{
		ItemID: dnfchat.ServerMegaphoneItemID, Kind: dungeonDropItemStackable,
		PVFPath: currentServerMegaphonePVFPath, StackableType: "[waste]",
		StackLimit: 100, SlotStart: 121, SlotEnd: 176,
	}
	service := &Service{
		pvfItemCatalog:     catalog,
		repositoryProvider: func() (dnfrepo.Group, bool) { return group, true },
	}
	slot, err := service.PrepareRobotServerMegaphone(ctx, accountID, characterID)
	if err != nil {
		t.Fatal(err)
	}
	inventory, found, err := group.Inventory.Load(ctx, "24")
	if err != nil || !found {
		t.Fatalf("load inventory: found=%t error=%v", found, err)
	}
	stack, found := inventory.Slots[slotKeyForRobotMegaphoneTest(slot)]
	if !found || stack.ItemID != int64(dnfchat.ServerMegaphoneItemID) || stack.Count <= 0 {
		t.Fatalf("megaphone stock slot=%d stack=%+v", slot, stack)
	}
}

func slotKeyForRobotMegaphoneTest(slot int16) string {
	return "0:" + strconv.Itoa(int(slot))
}
