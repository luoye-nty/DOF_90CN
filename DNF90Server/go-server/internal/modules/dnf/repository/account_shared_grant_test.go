package repository

import "testing"

func accountSharedGrantTestMeta() AccountSharedGrantMeta {
	return AccountSharedGrantMeta{
		Source:        "unit_test",
		ItemKind:      "stackable",
		PVFPath:       "stackable/material/cubepiece_clear.stk",
		StackableType: "[material]",
	}
}

// TestGrantAccountSharedWarehouseStackMergesIntoFixedCell 基本行为：并进固定格，
// 并在新建行时写上溯源 Extra。本 PVF 的 12 个道具都没有 [stack limit]（stackLimit=0）。
func TestGrantAccountSharedWarehouseStackMergesIntoFixedCell(t *testing.T) {
	account := &AccountInventoryRecord{AccountID: "dnf:1", Slots: map[string]ItemStack{
		"0:358": {ItemID: 3037, Count: 43890},
	}}
	grant := GrantAccountSharedWarehouseStack(account, 3037, 0, 20, accountSharedGrantTestMeta())
	if !grant.Changed || grant.Slot != 358 || grant.Placed != 20 || grant.Remainder != 0 || grant.CountAfter != 43910 {
		t.Fatalf("grant=%+v want slot358 placed20 remainder0 count43910", grant)
	}
	if got := account.Slots["0:358"]; got.ItemID != 3037 || got.Count != 43910 {
		t.Fatalf("cell=%+v want 3037x43910", got)
	}
}

func TestGrantAccountSharedWarehouseStackCreatesRowWithProvenance(t *testing.T) {
	account := &AccountInventoryRecord{AccountID: "dnf:1"}
	meta := accountSharedGrantTestMeta()
	grant := GrantAccountSharedWarehouseStack(account, 10158124, 0, 3, meta)
	if !grant.Changed || grant.Slot != 365 || grant.CountAfter != 3 {
		t.Fatalf("grant=%+v want slot365 count3", grant)
	}
	stack := account.Slots["0:365"]
	if stack.ItemID != 10158124 || stack.Count != 3 {
		t.Fatalf("soul row=%+v", stack)
	}
	// source / item_kind / pvf_path 三个键始终写入；stackable_type 非空才写。
	if stack.Extra["source"] != meta.Source || stack.Extra["item_kind"] != meta.ItemKind ||
		stack.Extra["pvf_path"] != meta.PVFPath || stack.Extra["stackable_type"] != meta.StackableType {
		t.Fatalf("row extra=%+v", stack.Extra)
	}
	if _, exists := stack.Extra["stack_limit"]; exists {
		t.Fatalf("stackLimit=0 must not write a stack_limit key: %+v", stack.Extra)
	}
	// raw entry 由调用方盖章：原语不碰它。
	if len(stack.RawEntry) != 0 {
		t.Fatalf("primitive must not stamp raw entry, got %d bytes", len(stack.RawEntry))
	}
}

// 固定格被**别的**道具占着时绝不驱赶、绝不合并，整笔退回。
func TestGrantAccountSharedWarehouseStackNeverEvictsForeignOccupant(t *testing.T) {
	account := &AccountInventoryRecord{AccountID: "dnf:1", Slots: map[string]ItemStack{
		"0:358": {ItemID: 9999, Count: 4},
	}}
	grant := GrantAccountSharedWarehouseStack(account, 3037, 0, 20, accountSharedGrantTestMeta())
	if grant.Changed || grant.Placed != 0 || grant.Remainder != 20 || grant.Slot != 358 {
		t.Fatalf("grant=%+v want unchanged with remainder 20", grant)
	}
	if got := account.Slots["0:358"]; got.ItemID != 9999 || got.Count != 4 {
		t.Fatalf("foreign occupant was disturbed: %+v", got)
	}
}

// [stack limit] 声明时额度按"这一个容器里该 id 的总量"算，放不下的原样退回。
func TestGrantAccountSharedWarehouseStackHonoursContainerStackLimit(t *testing.T) {
	account := &AccountInventoryRecord{AccountID: "dnf:1", Slots: map[string]ItemStack{
		"0:358": {ItemID: 3037, Count: 4},
		"0:100": {ItemID: 3037, Count: 1}, // 容器里的另一行同 id 也要计入额度
	}}
	grant := GrantAccountSharedWarehouseStack(account, 3037, 6, 10, accountSharedGrantTestMeta())
	if !grant.Changed || grant.Placed != 1 || grant.Remainder != 9 || grant.CountAfter != 5 {
		t.Fatalf("grant=%+v want placed1 remainder9 count5 (limit 6, container total 5)", grant)
	}
	if got := account.Slots["0:358"]; got.Count != 5 {
		t.Fatalf("cell=%+v want 3037x5", got)
	}
}

// 非账号共享道具 / 非法入参一律 fail-closed，不猜槽位、不 panic。
func TestGrantAccountSharedWarehouseStackFailsClosed(t *testing.T) {
	for _, itemID := range []int64{0, -1, 1, 42, 3038, 3166, 3167, 10158125} {
		account := &AccountInventoryRecord{AccountID: "dnf:1"}
		grant := GrantAccountSharedWarehouseStack(account, itemID, 0, 5, accountSharedGrantTestMeta())
		if grant.Changed || grant.Remainder != 5 || grant.Slot != 0 {
			t.Fatalf("item %d: grant=%+v want not-a-member remainder 5", itemID, grant)
		}
		if len(account.Slots) != 0 {
			t.Fatalf("item %d: nothing may be written: %+v", itemID, account.Slots)
		}
	}
	// nil account / 非正数量同样不能 panic。
	if grant := GrantAccountSharedWarehouseStack(nil, 3037, 0, 5, accountSharedGrantTestMeta()); grant.Changed || grant.Remainder != 5 {
		t.Fatalf("nil account grant=%+v", grant)
	}
	account := &AccountInventoryRecord{AccountID: "dnf:1"}
	if grant := GrantAccountSharedWarehouseStack(account, 3037, 0, 0, accountSharedGrantTestMeta()); grant.Changed || grant.Remainder != 0 {
		t.Fatalf("zero count grant=%+v", grant)
	}
	if len(account.Slots) != 0 {
		t.Fatalf("zero count must not create a row: %+v", account.Slots)
	}
}

// 原语必须覆盖全部 12 个道具 —— 防止"只服务晶块"的半截实现再长回来。
func TestGrantAccountSharedWarehouseStackCoversCubesAndSouls(t *testing.T) {
	canonical := AccountSharedWarehouseFixedSlots()
	if len(canonical) != 12 {
		t.Fatalf("canonical table size=%d want 12", len(canonical))
	}
	for itemID, wantSlot := range canonical {
		account := &AccountInventoryRecord{AccountID: "dnf:1"}
		grant := GrantAccountSharedWarehouseStack(account, itemID, 0, 2, accountSharedGrantTestMeta())
		if !grant.Changed || grant.Slot != wantSlot || grant.CountAfter != 2 {
			t.Fatalf("item %d: grant=%+v want slot %d count 2", itemID, grant, wantSlot)
		}
		if stack := account.Slots[AccountSharedInventorySlotKey(wantSlot)]; stack.ItemID != itemID || stack.Count != 2 {
			t.Fatalf("item %d: row=%+v", itemID, stack)
		}
	}
}
