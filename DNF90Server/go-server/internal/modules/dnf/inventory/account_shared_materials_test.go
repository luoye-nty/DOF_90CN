package inventory

import (
	"testing"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

// TestUpgradeAccountSharedMaterialSlotFollowsCanonicalTable 防止 inventory 这一侧
// 又长出第四份硬编码副本。
//
// 历史：这张 12 条映射曾在三个包里各写一份（本包的 upgradeAccountSharedMaterialSlot、
// dnfbridge 的 currentDisjointWarehouseFixedSlots 与 equipmentRegenerationAccountSharedItem），
// 而"获取时要不要查表"又是逐个功能各写一遍，最终 NPC 商店金币购买那条漏查，
// 无色小晶块落进了角色背包 0:154（2026-09-21 现场）。
// 本用例把"本包视图 == dnfrepo 权威表"钉死：谁再手写一份，这里立刻红。
func TestUpgradeAccountSharedMaterialSlotFollowsCanonicalTable(t *testing.T) {
	canonical := dnfrepo.AccountSharedWarehouseFixedSlots()
	if len(canonical) != 12 {
		t.Fatalf("canonical table size = %d, want 12", len(canonical))
	}
	for itemID, wantSlot := range canonical {
		slot, ok := upgradeAccountSharedMaterialSlot(itemID)
		if !ok {
			t.Fatalf("item %d: inventory view says not-a-member, canonical says slot %d", itemID, wantSlot)
		}
		if slot != wantSlot {
			t.Fatalf("item %d: inventory view slot %d, canonical slot %d", itemID, slot, wantSlot)
		}
		if !dnfrepo.IsAccountSharedInventorySlot(dnfrepo.MainInventoryListType, slot) {
			t.Fatalf("item %d: slot %d is outside the account-shared range", itemID, slot)
		}
	}
	for _, itemID := range []int64{0, -1, 1, 3038, 3166, 3167, 10158125} {
		if slot, ok := upgradeAccountSharedMaterialSlot(itemID); ok {
			t.Fatalf("item %d must not be a warehouse member, got slot %d", itemID, slot)
		}
	}
}
