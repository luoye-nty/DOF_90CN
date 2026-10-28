package dnfbridge

import (
	"testing"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

// TestBridgeAccountSharedViewsFollowCanonicalTable 防止 dnfbridge 这一侧的两个视图
// 又各自长回独立的硬编码副本。
//
// 历史：同一张 12 条映射在三个包里各写一份；dnfbridge 内部还有两份
// （currentDisjointWarehouseFixedSlots 与 equipmentRegenerationAccountSharedItem）。
// 三份互相漂移正是 2026-09-21「无色落进背包」事故的土壤。本用例把两个视图钉在
// dnfrepo 权威表上。
func TestBridgeAccountSharedViewsFollowCanonicalTable(t *testing.T) {
	canonical := dnfrepo.AccountSharedWarehouseFixedSlots()
	if len(canonical) != 12 {
		t.Fatalf("canonical table size = %d, want 12", len(canonical))
	}
	if len(currentDisjointWarehouseFixedSlots) != len(canonical) {
		t.Fatalf("bridge uint32 view size = %d, want %d",
			len(currentDisjointWarehouseFixedSlots), len(canonical))
	}
	for itemID, wantSlot := range canonical {
		if got := currentDisjointWarehouseFixedSlots[uint32(itemID)]; got != wantSlot {
			t.Fatalf("item %d: bridge uint32 view slot %d, canonical slot %d", itemID, got, wantSlot)
		}
		slot, ok := currentAccountWarehouseFixedSlot(itemID)
		if !ok || slot != wantSlot {
			t.Fatalf("item %d: currentAccountWarehouseFixedSlot = %d,%t; want %d,true",
				itemID, slot, ok, wantSlot)
		}
		if !equipmentRegenerationAccountSharedItem(itemID) {
			t.Fatalf("item %d: equipmentRegenerationAccountSharedItem = false, want true", itemID)
		}
	}
	for _, itemID := range []int64{0, -1, 1, 3038, 3166, 3167, 3261, 3263, 10100114, 10158125} {
		if _, ok := currentDisjointWarehouseFixedSlots[uint32(itemID)]; ok {
			t.Fatalf("item %d must not be in the bridge uint32 view", itemID)
		}
		if slot, ok := currentAccountWarehouseFixedSlot(itemID); ok {
			t.Fatalf("item %d: currentAccountWarehouseFixedSlot = %d, want not-a-member", itemID, slot)
		}
		if equipmentRegenerationAccountSharedItem(itemID) {
			t.Fatalf("item %d: equipmentRegenerationAccountSharedItem = true, want false", itemID)
		}
	}
}
