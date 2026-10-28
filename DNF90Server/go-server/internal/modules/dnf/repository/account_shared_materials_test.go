package repository

import "testing"

// accountSharedWarehouseExpectation 是权威表的期望内容。槽位序是 90 客户端布局，
// 2026-07-15 实测、2026-09-21 经运营确认：六个晶块占 354..359、六个灵魂占 360..365。
// 改动这张表必须是有意的 —— 它是"无色/灵魂落点"的唯一真源。
type accountSharedWarehouseExpectation struct {
	itemID int64
	slot   int16
	name   string
}

var accountSharedWarehouseExpectations = []accountSharedWarehouseExpectation{
	{AccountSharedCubeBlackItemID, CrystalWarehouseFirstSlot + 0, "cube_black"},
	{AccountSharedCubeWhiteItemID, CrystalWarehouseFirstSlot + 1, "cube_white"},
	{AccountSharedCubeRedItemID, CrystalWarehouseFirstSlot + 2, "cube_red"},
	{AccountSharedCubeBlueItemID, CrystalWarehouseFirstSlot + 3, "cube_blue"},
	{AccountSharedCubeClearItemID, CrystalWarehouseFirstSlot + 4, "cube_clear"},
	{AccountSharedCubeGoldItemID, CrystalWarehouseFirstSlot + 5, "cube_gold"},
	{AccountSharedSoulCommonItemID, SoulWarehouseFirstSlot + 0, "soul_common"},
	{AccountSharedSoulAdvancedItemID, SoulWarehouseFirstSlot + 1, "soul_advanced"},
	{AccountSharedSoulRareItemID, SoulWarehouseFirstSlot + 2, "soul_rare"},
	{AccountSharedSoulUniqueItemID, SoulWarehouseFirstSlot + 3, "soul_unique"},
	{AccountSharedSoulLegendaryItemID, SoulWarehouseFirstSlot + 4, "soul_legendary"},
	{AccountSharedSoulEpicItemID, SoulWarehouseFirstSlot + 5, "soul_epic"},
}

// TestAccountSharedWarehouseTablePinsTheTwelveFixedCells 锁住权威表的 12 条内容，
// 并校验它与槽位区间判定（IsAccountSharedInventorySlot）自洽 ——
// 表里给出 354..365 之外的槽，或槽数少于/多于 12，都属于结构性错误。
func TestAccountSharedWarehouseTablePinsTheTwelveFixedCells(t *testing.T) {
	canonical := AccountSharedWarehouseFixedSlots()
	if len(canonical) != len(accountSharedWarehouseExpectations) {
		t.Fatalf("canonical table size = %d, want %d", len(canonical), len(accountSharedWarehouseExpectations))
	}
	if len(canonical) != 12 {
		t.Fatalf("canonical table size = %d, want 12 (six cubes + six souls)", len(canonical))
	}
	seenSlots := make(map[int16]string, len(canonical))
	for _, want := range accountSharedWarehouseExpectations {
		slot, ok := AccountSharedWarehouseSlotForItem(want.itemID)
		if !ok {
			t.Fatalf("%s: item %d is missing from the canonical table", want.name, want.itemID)
		}
		if slot != want.slot {
			t.Fatalf("%s: item %d => slot %d, want %d", want.name, want.itemID, slot, want.slot)
		}
		if !IsAccountSharedWarehouseItem(want.itemID) {
			t.Fatalf("%s: IsAccountSharedWarehouseItem(%d) = false, want true", want.name, want.itemID)
		}
		if got := canonical[want.itemID]; got != want.slot {
			t.Fatalf("%s: copy view item %d => %d, want %d", want.name, want.itemID, got, want.slot)
		}
		if !IsAccountSharedInventorySlot(MainInventoryListType, slot) {
			t.Fatalf("%s: slot %d is outside the account-shared 354..365 range", want.name, slot)
		}
		if other, duplicated := seenSlots[slot]; duplicated {
			t.Fatalf("slot %d claimed by both %s and %s", slot, other, want.name)
		}
		seenSlots[slot] = want.name
	}
}

// TestAccountSharedWarehouseSlotForItemFailsClosedOnNonMembers 非成员必须一律 fail-closed：
// 不得猜槽位、不得 panic。元素结晶 3166/3167 与其它可叠加道具按口径留在角色背包。
func TestAccountSharedWarehouseSlotForItemFailsClosedOnNonMembers(t *testing.T) {
	for _, itemID := range []int64{0, -1, -3037, 1, 42, 3166, 3167, 3038, 3261, 3263, 10100114, 10158125, 1 << 40} {
		if slot, ok := AccountSharedWarehouseSlotForItem(itemID); ok {
			t.Fatalf("item %d => slot %d, want not-a-member", itemID, slot)
		}
		if IsAccountSharedWarehouseItem(itemID) {
			t.Fatalf("item %d reported as a warehouse member", itemID)
		}
	}
}

// TestAccountSharedWarehouseFixedSlotsHandsOutCopies 权威表不能被调用方改写 ——
// 副本语义是"多份视图共用一份真源"的前提。
func TestAccountSharedWarehouseFixedSlotsHandsOutCopies(t *testing.T) {
	first := AccountSharedWarehouseFixedSlots()
	delete(first, AccountSharedCubeClearItemID)
	first[999999] = CrystalWarehouseFirstSlot

	if _, ok := AccountSharedWarehouseSlotForItem(AccountSharedCubeClearItemID); !ok {
		t.Fatal("canonical table lost a member through the returned copy")
	}
	if IsAccountSharedWarehouseItem(999999) {
		t.Fatal("canonical table accepted an injected member through the returned copy")
	}
	second := AccountSharedWarehouseFixedSlots()
	if len(second) != len(accountSharedWarehouseExpectations) {
		t.Fatalf("second copy size = %d, want %d", len(second), len(accountSharedWarehouseExpectations))
	}
}
