package dnfbridge

import (
	"context"
	"os"
	"testing"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

func TestCurrentPrivateStoreRealPVFAcceptsCapturedMultiRowAndFullStack(t *testing.T) {
	pvfPath := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE to validate captured private-store rows")
	}
	archive, err := platformpvf.LoadArchive(platformpvf.Options{Path: pvfPath})
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := newPVFDungeonDropCatalog(archive)
	if err != nil {
		t.Fatal(err)
	}
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Inventory.Save(context.Background(), dnfrepo.InventoryRecord{
		CharacterID: "1",
		Slots: map[string]dnfrepo.ItemStack{
			"0:141": {ItemID: 2683432, Count: 31, Extra: map[string]string{"item_kind": "stackable"}},
			// This is the settlement-reward row that caused the captured request
			// to fail: the quality seed is durable, but raw_entry is absent.
			"0:11": {ItemID: 116010032, Count: 1, Bind: true, Extra: map[string]string{
				"item_kind": "equipment", "quality_seed": "669297134",
			}},
			"0:115": {ItemID: 10008088, Count: 1, Extra: map[string]string{"item_kind": "stackable"}},
		},
	}); err != nil {
		t.Fatal(err)
	}
	connection := &bufferConn{}
	session := &gameSession{conn: connection, selectedCharacterID: 1, robotSessionBound: true}
	service := &Service{
		options:            options{gameUpperHeader: gameUpperHeaderChannel13, gameUpperBodyCodec: gameUpperBodyCodecPlain},
		pvfItemCatalog:     catalog,
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
		privateStores: map[uint16]*currentPrivateStore{
			1: {OwnerID: 1, OwnerSession: session},
		},
		privateStoreVisitors: make(map[*gameSession]uint16),
		onlinePlayers:        newOnlinePlayerManager(),
	}
	for _, check := range []struct {
		slot     string
		listType byte
		wire     uint32
		stack    dnfrepo.ItemStack
	}{
		{slot: "0:141", wire: 31, stack: dnfrepo.ItemStack{ItemID: 2683432, Count: 31, Extra: map[string]string{"item_kind": "stackable"}}},
		{slot: "0:11", wire: 0x27E4A9EE, stack: dnfrepo.ItemStack{ItemID: 116010032, Count: 1, Bind: true, Extra: map[string]string{"item_kind": "equipment", "quality_seed": "669297134"}}},
		{slot: "0:115", wire: 1, stack: dnfrepo.ItemStack{ItemID: 10008088, Count: 1, Extra: map[string]string{"item_kind": "stackable"}}},
	} {
		definition, resolveErr := catalog.ResolveItem(uint32(check.stack.ItemID))
		if resolveErr != nil {
			t.Fatalf("resolve %s item=%d: %v", check.slot, check.stack.ItemID, resolveErr)
		}
		count, instance, expectedBind, allowSealed, eligible := service.resolveCurrentPrivateStoreRowStack(check.listType, check.stack, check.wire)
		if !eligible {
			t.Fatalf("private-store row %s item=%d kind=%s rarity=%d attach=%q bind=%t wire=%d instance=%d rejected", check.slot, check.stack.ItemID, definition.Kind, definition.Rarity, definition.AttachType, check.stack.Bind, check.wire, currentPrivateStoreEquipmentInstance(check.listType, check.stack))
		}
		t.Logf("private-store row %s item=%d kind=%s rarity=%d attach=%q bind=%t count=%d instance=%d expected_bind=%t allow_sealed=%t", check.slot, check.stack.ItemID, definition.Kind, definition.Rarity, definition.AttachType, check.stack.Bind, count, instance, expectedBind, allowSealed)
	}
	// Exact op87 body retained at packet_log 61486 on 2026-08-28.
	body := []byte{
		0x04, 0x00, 0x00, 0x00, '1', '1', '1', '1', 0x03,
		0x02, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00, 0x8d, 0x00, 0x1f, 0x00, 0x00, 0x00,
		0x04, 0x00, 0x08, 0x4c, 0x01, 0x00, 0x00, 0x0b, 0x00, 0xee, 0xa9, 0xe4, 0x27,
		0x05, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00, 0x73, 0x00, 0x01, 0x00, 0x00, 0x00,
		0xff, 0xff,
	}
	if err := service.completeCurrentPrivateStore(session, body); err != nil {
		t.Fatal(err)
	}
	store := service.privateStores[1]
	if store == nil || !store.Active || len(store.Rows) != 3 ||
		store.Rows[0].Count != 31 || store.Rows[1].InstanceValue != 0x27E4A9EE || store.Rows[2].Count != 1 {
		t.Fatalf("captured private store=%+v", store)
	}
}
