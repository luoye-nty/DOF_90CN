package repository

import (
	"testing"
)

// 这组用例锁定共享助手本身的语义：PVF [stack limit] N 既是单行上限，也是"这个
// 容器里该道具 id 的总量上限"；容器按行键的 listType 区分，未声明上限的道具不受限。

func TestContainerItemTotalCountsOnlyItsOwnContainer(t *testing.T) {
	items := map[string]ItemStack{
		"0:65":  {ItemID: 1001, Count: 3},
		"0:70":  {ItemID: 1001, Count: 4},
		"0:80":  {ItemID: 1002, Count: 9},
		"2:65":  {ItemID: 1001, Count: 5}, // 个人仓库是另一个容器
		"1:3":   {ItemID: 1001, Count: 6}, // 时装栏是另一个容器
		"12:65": {ItemID: 1001, Count: 7}, // 账号金库是另一个容器
		"0:90":  {ItemID: 1001, Count: 0}, // 空行不算
		"0:91":  {ItemID: 0, Count: 8},
		"bad":   {ItemID: 1001, Count: 11},
	}
	if got := ContainerItemTotal(items, 0, 1001); got != 7 {
		t.Fatalf("list-0 total = %d, want 7", got)
	}
	if got := ContainerItemTotal(items, 2, 1001); got != 5 {
		t.Fatalf("list-2 total = %d, want 5", got)
	}
	if got := ContainerItemTotal(items, 0, 1002); got != 9 {
		t.Fatalf("other item total = %d, want 9", got)
	}
	if got := ContainerItemTotal(nil, 0, 1001); got != 0 {
		t.Fatalf("nil container total = %d, want 0", got)
	}
	if got := ContainerItemTotal(items, 0, 0); got != 0 {
		t.Fatalf("zero item total = %d, want 0", got)
	}
}

func TestContainerItemSpaceCapsByContainerTotal(t *testing.T) {
	items := map[string]ItemStack{
		"0:65": {ItemID: 1001, Count: 3},
		"0:70": {ItemID: 1001, Count: 4},
	}
	if got := ContainerItemSpace(items, 0, 1001, 10); got != 3 {
		t.Fatalf("space = %d, want 3", got)
	}
	if got := ContainerItemSpace(items, 0, 1001, 7); got != 0 {
		t.Fatalf("full container space = %d, want 0", got)
	}
	if got := ContainerItemSpace(items, 0, 1001, 5); got != 0 {
		t.Fatalf("over-limit container space = %d, want 0", got)
	}
	// 未声明 [stack limit]（stackLimit <= 0）走哨兵：不受本规则约束。
	if got := ContainerItemSpace(items, 0, 1001, 0); got != ContainerUncappedSpace {
		t.Fatalf("uncapped space = %d, want %d", got, ContainerUncappedSpace)
	}
	if got := ContainerItemSpace(items, 0, 1001, -1); got != ContainerUncappedSpace {
		t.Fatalf("negative limit space = %d, want %d", got, ContainerUncappedSpace)
	}
}

func TestContainerItemRowPrefersLowestSlot(t *testing.T) {
	items := map[string]ItemStack{
		"0:70": {ItemID: 1001, Count: 4},
		"0:65": {ItemID: 1001, Count: 1},
		"2:10": {ItemID: 1001, Count: 2},
	}
	key, stack, ok := ContainerItemRow(items, 0, 1001)
	if !ok || key != "0:65" || stack.Count != 1 {
		t.Fatalf("row = %q %+v ok=%t, want the lowest list-0 slot", key, stack, ok)
	}
	if _, _, ok := ContainerItemRow(items, 0, 1002); ok {
		t.Fatal("missing item reported a row")
	}
	if key, _, ok := ContainerItemRow(items, 2, 1001); !ok || key != "2:10" {
		t.Fatalf("list-2 row = %q ok=%t", key, ok)
	}
}

func TestCanOpenContainerItemRow(t *testing.T) {
	items := map[string]ItemStack{
		"0:65": {ItemID: 1001, Count: 1},
	}
	if CanOpenContainerItemRow(items, 0, 1001, 1000) {
		t.Fatal("existing row must block a second row for a stack-limited item")
	}
	if CanOpenContainerItemRow(items, 0, 1001, 1) {
		t.Fatal("full container must block a new row")
	}
	if !CanOpenContainerItemRow(items, 0, 1002, 1) {
		t.Fatal("container without that item must allow a new row")
	}
	// 未声明 [stack limit] 与装备一律沿用旧行为。
	if !CanOpenContainerItemRow(items, 0, 1001, 0) {
		t.Fatal("uncapped item must keep the legacy new-row behavior")
	}
	if !CanOpenContainerItemRow(nil, 0, 1001, 1) {
		t.Fatal("empty container must allow a new row")
	}
}

func TestInventorySlotKeyRoundTrip(t *testing.T) {
	if got := InventorySlotKey(38, 97); got != "38:97" {
		t.Fatalf("slot key = %q", got)
	}
	listType, slot, ok := ParseInventorySlotKey("38:97")
	if !ok || listType != 38 || slot != 97 {
		t.Fatalf("parsed list=%d slot=%d ok=%t", listType, slot, ok)
	}
	for _, key := range []string{"", "38", "38:", ":97", "300:1", "38:x"} {
		if _, _, ok := ParseInventorySlotKey(key); ok {
			t.Fatalf("malformed key %q accepted", key)
		}
	}
}
