package dnfbridge

import (
	"os"
	"testing"

	platformpvf "longheng.io/server/internal/platform/pvf"
)

func TestAutomaticRobotRealPVFBuildsTradeablePermanentStock(t *testing.T) {
	pvfPath := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE to verify automatic robot stock")
	}
	archive, err := platformpvf.LoadArchive(platformpvf.Options{Path: pvfPath})
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := newPVFDungeonDropCatalog(archive)
	if err != nil {
		t.Fatal(err)
	}
	service := &Service{pvfItemCatalog: catalog}
	items, err := service.automaticRobotStockItems(1, 8, make(map[uint32]struct{}))
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 8 {
		t.Fatalf("automatic stock rows = %d, want 8: %+v", len(items), items)
	}
	for _, item := range items {
		definition, resolveErr := catalog.ResolveItem(item.ItemID)
		if resolveErr != nil || item.Count == 0 || definition.Kind != dungeonDropItemStackable ||
			!currentDungeonDiscardTradeable(definition) || !definition.ExpirationDate.IsZero() || definition.UsablePeriodDays != 0 {
			t.Fatalf("automatic stock item=%+v definition=%+v err=%v", item, definition, resolveErr)
		}
	}
}
